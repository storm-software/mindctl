package provider

// Registry resolves configured provider IDs to their native adapters.
type Registry struct{ providers map[string]Provider }

// NewRegistry copies entries so caller mutations cannot change live routing.
func NewRegistry(entries map[string]Provider) *Registry {
	providers := make(map[string]Provider, len(entries))
	for id, adapter := range entries {
		providers[id] = adapter
	}
	return &Registry{providers: providers}
}

// Get returns the adapter registered for id.
func (r *Registry) Get(id string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	provider, ok := r.providers[id]
	return provider, ok
}
