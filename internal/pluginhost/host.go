// Package pluginhost loads optional executors, translators, auth providers,
// scheduler hooks, and management routes. It cannot replace the classifier.
package pluginhost

import (
	"errors"
	"plugin"
	"sync"
)

// Kind is the extension point a plugin fills.
type Kind string

const (
	KindExecutor   Kind = "executor"
	KindTranslator Kind = "translator"
	KindAuth       Kind = "auth"
	KindScheduler  Kind = "scheduler"
	KindManagement Kind = "management"
	KindClassifier Kind = "classifier"
)

// Plugin is the symbol a plugin file exports as Plugin.
type Plugin interface {
	Name() string
	Kind() Kind
}

// Info is the public description of a loaded plugin.
type Info struct {
	Name, Path string
	Kind       Kind
}

type registered struct {
	info   Info
	plugin Plugin
}

// Host is disabled until constructed with enabled set.
type Host struct {
	mu      sync.Mutex
	enabled bool
	items   []registered
}

// New returns a host. A disabled host rejects every load and registration.
func New(enabled bool) *Host {
	return &Host{enabled: enabled, items: []registered{}}
}

// Enabled reports whether plugins may be added.
func (h *Host) Enabled() bool {
	return h != nil && h.enabled
}

// Register adds an in-process plugin. A classifier plugin is rejected even
// when the host is enabled.
func (h *Host) Register(item Plugin) error {
	if h == nil || !h.enabled {
		return errors.New("plugins are disabled")
	}
	if item == nil || item.Name() == "" {
		return errors.New("plugin is incomplete")
	}
	if item.Kind() == KindClassifier || item.Kind() == "" {
		return errors.New("plugins cannot replace the classifier")
	}
	switch item.Kind() {
	case KindExecutor, KindTranslator, KindAuth, KindScheduler, KindManagement:
	default:
		return errors.New("plugins cannot replace the classifier")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.items = append(h.items, registered{info: Info{Name: item.Name(), Kind: item.Kind()}, plugin: item})
	return nil
}

// Load opens a Go plugin file and registers its Plugin symbol.
func (h *Host) Load(path string) error {
	if h == nil || !h.enabled {
		return errors.New("plugins are disabled")
	}
	if path == "" {
		return errors.New("plugin path is required")
	}
	opened, err := plugin.Open(path)
	if err != nil {
		return err
	}
	symbol, err := opened.Lookup("Plugin")
	if err != nil {
		return err
	}
	var item Plugin
	switch typed := symbol.(type) {
	case Plugin:
		item = typed
	case *Plugin:
		if typed == nil || *typed == nil {
			return errors.New("plugin symbol is empty")
		}
		item = *typed
	default:
		return errors.New("plugin symbol must implement pluginhost.Plugin")
	}
	if err := h.Register(item); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	last := len(h.items) - 1
	if last >= 0 {
		h.items[last].info.Path = path
	}
	return nil
}

// List returns the loaded plugins without their private state.
func (h *Host) List() []Info {
	if h == nil {
		return []Info{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Info, len(h.items))
	for index, item := range h.items {
		out[index] = item.info
	}
	return out
}
