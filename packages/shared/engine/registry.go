// Package engine registers compiler-specific behavior without constraining
// engine names in configuration or the wire format to a fixed enumeration.
package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Command describes a trusted executable probe, never a shell command.
type Command struct {
	Name string
	Args []string
}

// Driver supplies the engine-specific parts of a latexmk build. Implementations
// must be immutable, safe for concurrent use and return caller-owned slices.
// Register implementations in trusted application code before serving requests;
// names received from configuration or clients are only registry lookup keys.
type Driver interface {
	LatexmkArgs() []string
	GraphicsExtensions() []string
	VersionProbe() Command
}

// Registry maps opaque names to trusted drivers. Its zero value is ready to use.
type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
}

func NewRegistry() *Registry {
	return &Registry{drivers: make(map[string]Driver)}
}

// Register adds an opaque string key. It never replaces an existing driver.
func (r *Registry) Register(name string, driver Driver) error {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\x00\r\n") {
		return errors.New("engine name must be nonempty and contain no NUL or newline")
	}
	if driver == nil {
		return errors.New("engine driver is required")
	}
	if len(driver.LatexmkArgs()) == 0 || driver.VersionProbe().Name == "" {
		return errors.New("engine driver must provide latexmk arguments and a version probe")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.drivers[name]; exists {
		return fmt.Errorf("engine %q is already registered", name)
	}
	if r.drivers == nil {
		r.drivers = make(map[string]Driver)
	}
	r.drivers[name] = driver
	return nil
}

func (r *Registry) Lookup(name string) (Driver, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	driver, ok := r.drivers[name]
	if !ok {
		return nil, fmt.Errorf("unregistered engine %q", name)
	}
	return driver, nil
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.drivers))
	for name := range r.drivers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
