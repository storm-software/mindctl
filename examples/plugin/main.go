// Example is a disabled-by-default executor plugin.
// Build it with `go build -buildmode=plugin -o example.so .` and install it
// through the management API. It cannot replace the classifier.
package main

import "github.com/storm-software/mindctl/internal/pluginhost"

type example struct{}

func (example) Name() string          { return "example" }
func (example) Kind() pluginhost.Kind { return pluginhost.KindExecutor }

// Plugin is the symbol the host loads.
var Plugin pluginhost.Plugin = example{}
