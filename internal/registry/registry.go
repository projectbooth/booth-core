// Package registry is booth-core's module registry: what's installed, its manifest, and
// its live health status. Two sources populate it: the real BoothModule CRD watch
// (ADR 0019, controller.go) in a real cluster, or a static YAML file for local
// development without one (devregistry package, wired in by cmd/core).
package registry

import (
	"fmt"
	"sync"

	boothv1alpha1 "github.com/projectbooth/booth-core/api/v1alpha1"
)

// Module is the registry's read model of one installed module — the manifest fields
// plus the last-observed health, decoupled from the Kubernetes API types so the gateway
// and API layers don't need to import controller-runtime just to read the registry.
type Module struct {
	Spec   boothv1alpha1.BoothModuleSpec
	Status boothv1alpha1.BoothModuleStatus
}

// BaseURL is where the gateway routes requests for this module. A real, cluster-sourced
// module has ServiceRef.Namespace and .Port set and resolves to the in-cluster Service
// DNS name; a devregistry-loaded module has only ServiceRef.Name set, to a directly
// dialable "host:port" for local development, and is used as-is.
func (m Module) BaseURL() string {
	ref := m.Spec.ServiceRef
	if ref.Namespace == "" && ref.Port == 0 {
		return "http://" + ref.Name
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", ref.Name, ref.Namespace, ref.Port)
}

// Registry is a concurrency-safe, in-memory view of every installed module, keyed by
// module ID. It has no opinion about where updates come from — Put/Delete are called by
// whichever source (CRD controller or dev-mode file watcher) is active.
type Registry struct {
	mu      sync.RWMutex
	modules map[string]Module
}

func New() *Registry {
	return &Registry{modules: make(map[string]Module)}
}

// Put inserts or replaces a module's entry.
func (r *Registry) Put(m Module) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.modules[m.Spec.ID] = m
}

// Delete removes a module's entry, e.g. on BoothModule deletion (uninstall).
func (r *Registry) Delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.modules, id)
}

// Get returns a single module by ID.
func (r *Registry) Get(id string) (Module, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.modules[id]
	return m, ok
}

// List returns every registered module, in no particular order.
func (r *Registry) List() []Module {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Module, 0, len(r.modules))
	for _, m := range r.modules {
		out = append(out, m)
	}
	return out
}
