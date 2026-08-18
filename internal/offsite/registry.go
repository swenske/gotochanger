package offsite

import "sort"

// Registry holds every registered OffsiteMode by name.
type Registry struct {
	modes map[string]OffsiteMode
}

func NewRegistry() *Registry {
	return &Registry{modes: make(map[string]OffsiteMode)}
}

func (r *Registry) Register(m OffsiteMode) {
	r.modes[m.Name()] = m
}

func (r *Registry) Get(name string) (OffsiteMode, bool) {
	m, ok := r.modes[name]
	return m, ok
}

// All returns every registered mode, sorted by Name for stable API/UI
// ordering.
func (r *Registry) All() []OffsiteMode {
	out := make([]OffsiteMode, 0, len(r.modes))
	for _, m := range r.modes {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
