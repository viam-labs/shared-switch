// Package refcounted implements a Viam switch whose state is driven by a
// reference-counted set of client holders. Intended to coordinate a shared
// load (e.g. a vacuum) across multiple machines: while any client holds the
// switch it stays energized; when the last one releases, it turns off.
package refcounted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

	moduleDataEnvVar = "VIAM_MODULE_DATA"
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
	boardName  string
	pinName    string
	pin        board.GPIOPin
	activeHigh bool

	// holdersPath persists holders so a rebuild or restart of this resource does not
	// silently drop a client that is still mid-operation. Empty disables persistence.
	holdersPath string

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
	var holdersPath string
	if dir := os.Getenv(moduleDataEnvVar); dir != "" {
		holdersPath = filepath.Join(dir, conf.ResourceName().Name+".holders.json")
	} else {
		logger.Warnf("%s unset: holders are kept in memory only and are lost on rebuild", moduleDataEnvVar)
	}
	holders, err := loadHolders(holdersPath)
	if err != nil {
		return nil, err
	}
	r := &refcounted{
		Named:       conf.ResourceName().AsNamed(),
		logger:      logger,
		boardName:   cfg.Board,
		pinName:     cfg.Pin,
		pin:         pin,
		activeHigh:  cfg.activeHigh(),
		holdersPath: holdersPath,
		holders:     holders,
	}
	logger.Infof("configured: board=%q pin=%q active_high=%v holders_file=%q restored_holders=%v",
		r.boardName, r.pinName, r.activeHigh, r.holdersPath, r.sortedHoldersLocked())
	r.mu.Lock()
	err = r.applyLocked(ctx)
	r.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("initial pin write: %w", err)
	}
	return r, nil
}

func loadHolders(path string) (map[string]struct{}, error) {
	holders := make(map[string]struct{})
	if path == "" {
		return holders, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return holders, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read holders file %q: %w", path, err)
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, fmt.Errorf("parse holders file %q: %w", path, err)
	}
	for _, id := range ids {
		holders[id] = struct{}{}
	}
	return holders, nil
}

// persistLocked writes via temp file + rename so a crash mid-write never leaves a
// truncated file that would fail the next construction.
func (r *refcounted) persistLocked() error {
	if r.holdersPath == "" {
		return nil
	}
	data, err := json.Marshal(r.sortedHoldersLocked())
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.holdersPath), filepath.Base(r.holdersPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("persist holders: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("persist holders: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("persist holders: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("persist holders: %w", err)
	}
	if err := os.Rename(tmp.Name(), r.holdersPath); err != nil {
		return fmt.Errorf("persist holders: %w", err)
	}
	return nil
}

func (r *refcounted) applyLocked(ctx context.Context) error {
	on := len(r.holders) > 0
	high := on == r.activeHigh
	if err := r.pin.Set(ctx, high, nil); err != nil {
		return err
	}
	actual, err := r.pin.Get(ctx, nil)
	if err != nil {
		return fmt.Errorf("verify pin %q after Set(high=%v): %w", r.pinName, high, err)
	}
	if actual != high {
		return fmt.Errorf("pin %q state mismatch after Set(high=%v): read back high=%v", r.pinName, high, actual)
	}
	return nil
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
	_, alreadyHeld := r.holders[id]
	r.holders[id] = struct{}{}
	if !alreadyHeld {
		if err := r.persistLocked(); err != nil {
			delete(r.holders, id)
			r.logger.Errorf("acquire %q: %v", id, err)
			return err
		}
	}
	high := r.activeHigh
	if err := r.applyLocked(ctx); err != nil {
		r.logger.Errorf("acquire %q (already_held=%v): pin %q set high=%v failed: %v", id, alreadyHeld, r.pinName, high, err)
		return err
	}
	r.logger.Infof("acquire %q (already_held=%v): pin %q set high=%v holders=%v", id, alreadyHeld, r.pinName, high, r.sortedHoldersLocked())
	return nil
}

func (r *refcounted) removeHolderLocked(ctx context.Context, id string) error {
	if _, held := r.holders[id]; !held {
		r.logger.Warnf("release for unknown client_id %q — no-op", id)
		return nil
	}
	delete(r.holders, id)
	if err := r.persistLocked(); err != nil {
		r.holders[id] = struct{}{}
		r.logger.Errorf("release %q: %v", id, err)
		return err
	}
	high := len(r.holders) > 0 == r.activeHigh
	if err := r.applyLocked(ctx); err != nil {
		r.logger.Errorf("release %q: pin %q set high=%v failed: %v", id, r.pinName, high, err)
		return err
	}
	r.logger.Infof("release %q: pin %q set high=%v holders=%v", id, r.pinName, high, r.sortedHoldersLocked())
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
	prev := r.holders
	r.holders = make(map[string]struct{})
	if err := r.persistLocked(); err != nil {
		r.holders = prev
		r.logger.Errorf("clear: %v", err)
		return err
	}
	high := !r.activeHigh
	if err := r.applyLocked(ctx); err != nil {
		r.logger.Errorf("clear: pin %q set high=%v failed (dropped=%v): %v", r.pinName, high, dropped, err)
		return err
	}
	r.logger.Infof("clear: pin %q set high=%v dropped=%v", r.pinName, high, dropped)
	return nil
}

func (r *refcounted) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.pin.Set(ctx, !r.activeHigh, nil); err != nil {
		r.logger.Warnf("failed to de-energize pin %q on close: %v", r.pinName, err)
	}
	return nil
}
