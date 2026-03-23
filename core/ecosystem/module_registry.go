// core/ecosystem/module_registry.go — Plugin module registry.
//
// Maintains a manifest of optional HisnOS extension modules (future capability).
// Modules are registered by their installer scripts and tracked with SHA-256
// checksums for integrity verification.
//
// The registry is intentionally append-only in the MVP: modules can be
// registered and queried but automatic loading is deferred to future phases.
//
// Persisted to: /var/lib/hisnos/module-registry.json
package ecosystem

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ModuleManifest describes a registered extension module.
type ModuleManifest struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	SHA256      string    `json:"sha256"`
	InstallPath string    `json:"install_path"`
	Enabled     bool      `json:"enabled"`
	RegisteredAt time.Time `json:"registered_at"`
	Tags        []string  `json:"tags,omitempty"`
}

// Registry holds all registered module manifests.
type Registry struct {
	Modules   []ModuleManifest `json:"modules"`
	UpdatedAt time.Time        `json:"updated_at"`

	path string
}

// NewModuleRegistry loads the registry from stateDir or returns an empty one.
func NewModuleRegistry(stateDir string) *Registry {
	r := &Registry{path: filepath.Join(stateDir, "module-registry.json")}
	if err := r.load(); err != nil {
		// New installation — start empty.
		r.UpdatedAt = time.Now().UTC()
	}
	return r
}

// Register adds or updates a module manifest.
// If a module with the same ID exists, it is replaced.
func (r *Registry) Register(m ModuleManifest) error {
	if m.ID == "" {
		return fmt.Errorf("module ID is required")
	}
	m.RegisteredAt = time.Now().UTC()

	// Replace if exists.
	for i, existing := range r.Modules {
		if existing.ID == m.ID {
			r.Modules[i] = m
			r.UpdatedAt = time.Now().UTC()
			return r.save()
		}
	}
	r.Modules = append(r.Modules, m)
	r.UpdatedAt = time.Now().UTC()
	return r.save()
}

// Enable toggles a module's enabled state.
func (r *Registry) Enable(id string, enabled bool) error {
	for i := range r.Modules {
		if r.Modules[i].ID == id {
			r.Modules[i].Enabled = enabled
			r.UpdatedAt = time.Now().UTC()
			return r.save()
		}
	}
	return fmt.Errorf("module %q not found", id)
}

// Get returns a module by ID.
func (r *Registry) Get(id string) (ModuleManifest, bool) {
	for _, m := range r.Modules {
		if m.ID == id {
			return m, true
		}
	}
	return ModuleManifest{}, false
}

// All returns all registered modules.
func (r *Registry) All() []ModuleManifest {
	out := make([]ModuleManifest, len(r.Modules))
	copy(out, r.Modules)
	return out
}

// StatusMap returns a summary suitable for IPC responses.
func (r *Registry) StatusMap() map[string]any {
	enabled := 0
	for _, m := range r.Modules {
		if m.Enabled {
			enabled++
		}
	}
	return map[string]any{
		"total_modules":   len(r.Modules),
		"enabled_modules": enabled,
		"modules":         r.Modules,
		"updated_at":      r.UpdatedAt.Format(time.RFC3339),
	}
}

func (r *Registry) load() error {
	b, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, r)
}

func (r *Registry) save() error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeAtomic(r.path, string(b))
}
