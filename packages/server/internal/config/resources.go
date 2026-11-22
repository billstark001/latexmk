package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func loadResourceConfig(cfg *Config) error {
	cfg.ResourceCacheDir = os.Getenv("LATEXMK_RESOURCE_CACHE_DIR")
	cfg.ResourceRegistryFile = os.Getenv("LATEXMK_RESOURCE_REGISTRY_FILE")
	cfg.ResourceInventoryDir = env("LATEXMK_RUNTIME_INVENTORY_DIR", "/usr/local/lib/latexmk")
	var err error
	for _, value := range []struct {
		name        string
		fallback    int64
		destination *int64
	}{
		{"LATEXMK_MAX_RESOURCE_BYTES", 512 << 20, &cfg.MaxResourceBytes},
		{"LATEXMK_RESOURCE_CACHE_BYTES", 4 << 30, &cfg.ResourceCacheBytes},
		{"LATEXMK_RESOURCE_OWNER_BYTES", 1 << 30, &cfg.ResourceOwnerBytes},
	} {
		if *value.destination, err = envBytes(value.name, value.fallback); err != nil {
			return err
		}
	}
	if cfg.MaxResourceFiles, err = envInt("LATEXMK_MAX_RESOURCE_FILES", 20000); err != nil {
		return err
	}
	if cfg.MaxResourceEnvironments, err = envInt("LATEXMK_MAX_RESOURCE_ENVIRONMENTS", 64); err != nil {
		return err
	}
	if cfg.MaxConcurrentResourcePrepares, err = envInt("LATEXMK_MAX_CONCURRENT_RESOURCE_PREPARES", 2); err != nil {
		return err
	}
	if cfg.ResourcePrepareTimeout, err = envDuration("LATEXMK_RESOURCE_PREPARE_TIMEOUT", 5*time.Minute); err != nil {
		return err
	}
	if cfg.ResourceRetention, err = envDuration("LATEXMK_RESOURCE_RETENTION", 7*24*time.Hour); err != nil {
		return err
	}
	if cfg.MaxResourceFiles <= 0 || cfg.MaxResourceEnvironments <= 0 || cfg.MaxConcurrentResourcePrepares <= 0 ||
		cfg.ResourcePrepareTimeout <= 0 ||
		cfg.ResourceRetention <= 0 {
		return errors.New("resource environment counts and durations must be positive")
	}
	return nil
}

// ResourceDirectory canonicalizes existing ancestors before checking overlap.
// Source-state accounting may exclude exactly this independently quota-bound
// directory; aliases cannot move it over project data or its parent.
func (c Config) ResourceDirectory() (string, error) {
	state, err := canonicalDirectory(c.StateDir)
	if err != nil {
		return "", err
	}
	selected := c.ResourceCacheDir
	if selected == "" {
		selected = filepath.Join(state, "environments")
	}
	dir, err := canonicalDirectory(selected)
	if err != nil {
		return "", err
	}
	if withinDirectory(dir, state) {
		return "", errors.New("resource cache cannot equal or contain the project state directory")
	}
	if withinDirectory(state, dir) {
		rel, _ := filepath.Rel(state, dir)
		first := strings.Split(rel, string(filepath.Separator))[0]
		for _, reserved := range []string{"blobs", "results", "compile-cache", "live-cache", "staging", ".staging", "jobs", "sessions", "uploads", "snapshots"} {
			if first == reserved {
				return "", fmt.Errorf("resource cache overlaps reserved project state %s", reserved)
			}
		}
	}
	return dir, nil
}

func withinDirectory(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
func canonicalDirectory(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	probe := absolute
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}
