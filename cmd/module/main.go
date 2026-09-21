// Package main runs the shared-switch Viam module.
package main

import (
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"

	"github.com/viam-labs/shared-switch/refcounted"
)

func main() {
	module.ModularMain(
		resource.APIModel{API: toggleswitch.API, Model: refcounted.Model},
	)
}
