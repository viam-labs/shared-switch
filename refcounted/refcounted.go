// Package refcounted implements a Viam switch whose state is driven by a
// reference-counted set of client holders. Intended to coordinate a shared
// load (e.g. a vacuum) across multiple machines: while any client holds the
// switch it stays energized; when the last one releases, it turns off.
package refcounted

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"go.viam.com/rdk/components/board"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

var Model = resource.NewModel("viam", "shared-switch", "refcounted")

const (
	positionOff       uint32 = 0
	positionOn        uint32 = 1
	numberOfPositions uint32 = 2

	// manualHolder is the reserved client_id used by SetPosition to add/remove
	// itself as a virtual holder. Reserved so DoCommand callers can't
	// accidentally release the UI toggle by sending the same id.
	manualHolder = "manual"
)

var positionLabels = []string{"off", "on"}

func init() {
	resource.RegisterComponent(toggleswitch.API, Model,
		resource.Registration[toggleswitch.Switch, *Config]{
			Constructor: newRefcounted,
		},
	)
}

type Config struct {
	Board string `json:"board"`
	Pin   string `json:"pin"`
	// ActiveHigh selects the wiring polarity. nil (unset) is treated as true —
	// active-high is the common case (pin high energizes the load via SSR/transistor).
	ActiveHigh *bool `json:"active_high,omitempty"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.Board == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "board")
	}
	if cfg.Pin == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "pin")
	}
	return []string{cfg.Board}, nil, nil
}

func (cfg *Config) activeHigh() bool {
	if cfg.ActiveHigh == nil {
		return true
	}
	return *cfg.ActiveHigh
}

type refcounted struct {
	resource.AlwaysRebuild
	resource.Named
	logger     logging.Logger
	pin        board.GPIOPin
	activeHigh bool

	mu      sync.Mutex
	holders map[string]struct{}
}

func newRefcounted(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (toggleswitch.Switch, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	b, err := board.FromProvider(deps, cfg.Board)
	if err != nil {
		return nil, fmt.Errorf("resolve board %q: %w", cfg.Board, err)
	}
	pin, err := b.GPIOPinByName(cfg.Pin)
	if err != nil {
		return nil, fmt.Errorf("resolve pin %q on board %q: %w", cfg.Pin, cfg.Board, err)
	}
	r := &refcounted{
		Named:      conf.ResourceName().AsNamed(),
		logger:     logger,
		pin:        pin,
		activeHigh: cfg.activeHigh(),
		holders:    make(map[string]struct{}),
	}
	r.mu.Lock()
	err = r.applyLocked(ctx)
	r.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("initial pin write: %w", err)
	}
	return r, nil
}

func (r *refcounted) applyLocked(ctx context.Context) error {
	on := len(r.holders) > 0
	high := on == r.activeHigh
	return r.pin.Set(ctx, high, nil)
}

func (r *refcounted) sortedHoldersLocked() []string {
	out := make([]string, 0, len(r.holders))
	for id := range r.holders {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (r *refcounted) addHolderLocked(ctx context.Context, id string) error {
	if _, held := r.holders[id]; held {
		return nil
	}
	r.holders[id] = struct{}{}
	if err := r.applyLocked(ctx); err != nil {
		return err
	}
	r.logger.Infof("acquire %q: holders=%v", id, r.sortedHoldersLocked())
	return nil
}

func (r *refcounted) removeHolderLocked(ctx context.Context, id string) error {
	if _, held := r.holders[id]; !held {
		r.logger.Warnf("release for unknown client_id %q — no-op", id)
		return nil
	}
	delete(r.holders, id)
	if err := r.applyLocked(ctx); err != nil {
		return err
	}
	r.logger.Infof("release %q: holders=%v", id, r.sortedHoldersLocked())
	return nil
}

func (r *refcounted) SetPosition(ctx context.Context, position uint32, _ map[string]interface{}) error {
	if position >= numberOfPositions {
		return fmt.Errorf("invalid position %d (must be 0 or 1)", position)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if position == positionOn {
		return r.addHolderLocked(ctx, manualHolder)
	}
	// SetPosition(0) removes only the manual holder — other clients are unaffected.
	// Use {"command":"clear"} to force all holders off.
	if _, held := r.holders[manualHolder]; !held {
		return nil
	}
	return r.removeHolderLocked(ctx, manualHolder)
}

func (r *refcounted) GetPosition(_ context.Context, _ map[string]interface{}) (uint32, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.holders) > 0 {
		return positionOn, nil
	}
	return positionOff, nil
}

func (r *refcounted) GetNumberOfPositions(_ context.Context, _ map[string]interface{}) (uint32, []string, error) {
	return numberOfPositions, positionLabels, nil
}

func (r *refcounted) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	raw, ok := cmd["command"]
	if !ok {
		return nil, errors.New(`missing "command"`)
	}
	name, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf(`"command" must be a string, got %T`, raw)
	}
	switch name {
	case "acquire":
		return nil, r.acquire(ctx, cmd)
	case "release":
		return nil, r.release(ctx, cmd)
	case "clear":
		return nil, r.clear(ctx)
	default:
		return nil, fmt.Errorf("unknown command %q", name)
	}
}

func (r *refcounted) clientID(cmd map[string]interface{}) (string, error) {
	raw, ok := cmd["client_id"]
	if !ok {
		return "", errors.New(`missing "client_id"`)
	}
	id, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf(`"client_id" must be a string, got %T`, raw)
	}
	if id == "" {
		return "", errors.New(`"client_id" must be non-empty`)
	}
	if id == manualHolder {
		return "", fmt.Errorf(`"client_id" %q is reserved for SetPosition — pick a different id`, manualHolder)
	}
	return id, nil
}

func (r *refcounted) acquire(ctx context.Context, cmd map[string]interface{}) error {
	id, err := r.clientID(cmd)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addHolderLocked(ctx, id)
}

func (r *refcounted) release(ctx context.Context, cmd map[string]interface{}) error {
	id, err := r.clientID(cmd)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.removeHolderLocked(ctx, id)
}

func (r *refcounted) clear(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.holders) == 0 {
		return nil
	}
	dropped := r.sortedHoldersLocked()
	r.holders = make(map[string]struct{})
	if err := r.applyLocked(ctx); err != nil {
		return err
	}
	r.logger.Infof("clear: dropped=%v", dropped)
	return nil
}

func (r *refcounted) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.pin.Set(ctx, !r.activeHigh, nil); err != nil {
		r.logger.Warnf("failed to de-energize pin on close: %v", err)
	}
	return nil
}
