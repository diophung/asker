package sdk

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Registry is the hub's catalog of available in-process connectors, keyed
// by Spec().ID. It is safe for concurrent use.
type Registry struct {
	mu   sync.RWMutex
	byID map[string]Connector
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byID: make(map[string]Connector)}
}

// Register adds c under c.Spec().ID. It returns an error when c is nil, when
// the spec ID is empty, or when the ID is already registered — registration
// happens once at startup, so any of these is a programming error worth
// failing loudly on.
func (r *Registry) Register(c Connector) error {
	if c == nil {
		return errors.New("sdk: Register called with nil connector")
	}
	id := c.Spec().ID
	if id == "" {
		return errors.New("sdk: Register called with empty spec ID")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byID[id]; exists {
		return fmt.Errorf("sdk: connector %q already registered", id)
	}
	r.byID[id] = c
	return nil
}

// Get returns the connector registered under id, and whether one exists.
func (r *Registry) Get(id string) (Connector, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.byID[id]
	return c, ok
}

// List returns the specs of all registered connectors, sorted by ID.
func (r *Registry) List() []Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	specs := make([]Spec, 0, len(r.byID))
	for _, c := range r.byID {
		specs = append(specs, c.Spec())
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].ID < specs[j].ID })
	return specs
}
