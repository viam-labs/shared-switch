// Package refcounted implements a Viam switch whose state is driven by a
// reference-counted set of client holders. Intended to coordinate a shared
// load (e.g. a vacuum) across multiple machines: while any client holds the
// switch it stays energized; when the last one releases, it turns off.
//
// This file is a stub. The switch is registered and can be instantiated,
// but all methods return errNotImplemented until the coordinator logic
// lands in a follow-up PR.
package refcounted

import (
	"context"
	"errors"

	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

var Model = resource.NewModel("viam", "shared-switch", "refcounted")

var errNotImplemented = errors.New("not implemented")

func init() {
	resource.RegisterComponent(toggleswitch.API, Model,
		resource.Registration[toggleswitch.Switch, *Config]{
			Constructor: newRefcounted,
		},
	)
}

type Config struct{}

func (cfg *Config) Validate(_ string) ([]string, []string, error) {
	return nil, nil, nil
}

type refcounted struct {
	resource.AlwaysRebuild
	resource.Named
	logger logging.Logger
}

func newRefcounted(_ context.Context, _ resource.Dependencies, conf resource.Config, logger logging.Logger) (toggleswitch.Switch, error) {
	return &refcounted{
		Named:  conf.ResourceName().AsNamed(),
		logger: logger,
	}, nil
}

func (r *refcounted) SetPosition(_ context.Context, _ uint32, _ map[string]interface{}) error {
	return errNotImplemented
}

func (r *refcounted) GetPosition(_ context.Context, _ map[string]interface{}) (uint32, error) {
	return 0, errNotImplemented
}

func (r *refcounted) GetNumberOfPositions(_ context.Context, _ map[string]interface{}) (uint32, []string, error) {
	return 0, nil, errNotImplemented
}

func (r *refcounted) DoCommand(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	return nil, errNotImplemented
}

func (r *refcounted) Close(_ context.Context) error {
	return nil
}
