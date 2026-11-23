package source

// Registry holds the enabled providers in registration order.
type Registry struct {
	byName map[string]Provider
	order  []Provider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Provider{}}
}

// Register adds a provider. Registering the same name twice replaces it.
func (r *Registry) Register(p Provider) {
	if _, exists := r.byName[p.Name()]; exists {
		for i, old := range r.order {
			if old.Name() == p.Name() {
				r.order[i] = p
			}
		}
	} else {
		r.order = append(r.order, p)
	}
	r.byName[p.Name()] = p
}

// Get returns the provider with the given name.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// All returns providers in registration order.
func (r *Registry) All() []Provider {
	out := make([]Provider, len(r.order))
	copy(out, r.order)
	return out
}
