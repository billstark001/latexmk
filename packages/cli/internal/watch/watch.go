// Package watch polls a bounded set of already approved project files.
package watch

import (
	"context"
	"errors"
	"os"
	"sort"
	"time"
)

type Target struct {
	Name string
	Path string
}

type fileState struct {
	exists  bool
	mode    os.FileMode
	size    int64
	modTime int64
}

type Tracker struct {
	targets  []Target
	states   map[string]fileState
	interval time.Duration
	debounce time.Duration
}

func New(targets []Target, interval, debounce time.Duration) (*Tracker, error) {
	if interval <= 0 {
		return nil, errors.New("watch interval must be positive")
	}
	if debounce < 0 {
		return nil, errors.New("watch debounce cannot be negative")
	}
	unique := make(map[string]Target)
	for _, target := range targets {
		if target.Name == "" || target.Path == "" {
			return nil, errors.New("watch target must have a name and path")
		}
		unique[target.Path] = target
	}
	ordered := make([]Target, 0, len(unique))
	for _, target := range unique {
		ordered = append(ordered, target)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	tracker := &Tracker{targets: ordered, states: make(map[string]fileState, len(ordered)), interval: interval, debounce: debounce}
	for _, target := range ordered {
		tracker.states[target.Path] = statFile(target.Path)
	}
	return tracker, nil
}

// Wait returns after one or more target changes have remained quiet for the
// debounce duration. Missing targets are tracked so later creation is visible.
func (t *Tracker) Wait(ctx context.Context) ([]string, error) {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	pending := make(map[string]struct{})
	var quietSince time.Time
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case now := <-ticker.C:
			changed := false
			for _, target := range t.targets {
				current := statFile(target.Path)
				if current == t.states[target.Path] {
					continue
				}
				t.states[target.Path] = current
				pending[target.Name] = struct{}{}
				changed = true
			}
			if changed {
				quietSince = now
			}
			if len(pending) == 0 || now.Sub(quietSince) < t.debounce {
				continue
			}
			result := make([]string, 0, len(pending))
			for name := range pending {
				result = append(result, name)
			}
			sort.Strings(result)
			return result, nil
		}
	}
}

func statFile(path string) fileState {
	info, err := os.Lstat(path)
	if err != nil {
		return fileState{}
	}
	return fileState{exists: true, mode: info.Mode(), size: info.Size(), modTime: info.ModTime().UnixNano()}
}
