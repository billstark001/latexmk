// Package watch reconciles approved files using native notifications and polling.
package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/fsnotify/fsnotify"
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
	Refresh func() ([]Target, error)
	MaxWait time.Duration
	// RefreshInterval batches native event hints before rediscovering membership.
	// Zero refreshes selection for every event and poll.
	RefreshInterval time.Duration
	targets         []Target
	states          map[string]fileState
	refreshed       time.Time
	interval        time.Duration
	debounce        time.Duration
}

func New(targets []Target, interval, debounce time.Duration) (*Tracker, error) {
	if interval <= 0 {
		return nil, errors.New("watch interval must be positive")
	}
	if debounce < 0 {
		return nil, errors.New("watch debounce cannot be negative")
	}
	ordered, err := normalizeTargets(targets)
	if err != nil {
		return nil, err
	}
	tracker := &Tracker{
		targets:  ordered,
		states:   make(map[string]fileState, len(ordered)),
		interval: interval,
		debounce: debounce,
		MaxWait:  max(2*time.Second, 5*debounce),
	}
	for _, target := range ordered {
		tracker.states[target.Path] = statFile(target.Path)
	}
	return tracker, nil
}

func normalizeTargets(targets []Target) ([]Target, error) {
	unique := make(map[string]Target, len(targets))
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
	return ordered, nil
}

// Wait returns after one or more target changes have remained quiet for the
// debounce duration. Missing targets are tracked so later creation is visible.
func (t *Tracker) Wait(ctx context.Context) ([]string, error) {
	// Watch directories: editor saves commonly replace the original inode.
	// Notifications are hints only; selection and policy still come from Refresh.
	w, _ := fsnotify.NewWatcher()
	var events <-chan fsnotify.Event
	var watcherErrors <-chan error
	watched := make(map[string]bool)
	if w != nil {
		defer func() { _ = w.Close() }()
		events, watcherErrors = w.Events, w.Errors
	}
	addWatches := func() {
		if w == nil {
			return
		}
		for _, target := range t.targets {
			dir := filepath.Dir(target.Path)
			for {
				info, err := os.Lstat(dir)
				if err == nil && info.IsDir() {
					break
				}
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
				dir = parent
			}
			// A large tree remains correct through reconciliation without exhausting FDs.
			if !watched[dir] && len(watched) < 4096 && w.Add(dir) == nil {
				watched[dir] = true
			}
		}
	}
	addWatches()
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	settle := time.NewTimer(time.Hour)
	defer settle.Stop()
	settle.Stop()
	pending := make(map[string]struct{})
	var firstChange time.Time
	byPath := make(map[string]Target, len(t.targets))
	indexTargets := func() {
		clear(byPath)
		for _, target := range t.targets {
			byPath[filepath.Clean(target.Path)] = target
		}
	}
	indexTargets()
	arm := func() {
		now := time.Now()
		if firstChange.IsZero() {
			firstChange = now
		}
		delay := t.debounce
		if t.MaxWait > 0 {
			delay = min(delay, max(0, t.MaxWait-now.Sub(firstChange)))
		}
		settle.Reset(delay)
	}
	reconcile := func(eventPath string, refresh bool) (bool, error) {
		changed := false
		if t.Refresh != nil && refresh {
			targets, err := t.Refresh()
			if err != nil {
				return false, err
			}
			targets, err = normalizeTargets(targets)
			if err != nil {
				return false, err
			}
			{
				next := make(map[string]bool, len(targets))
				for _, target := range targets {
					next[target.Path] = true
					if _, exists := t.states[target.Path]; !exists {
						pending[target.Name] = struct{}{}
						changed = true
					}
				}
				for _, target := range t.targets {
					if !next[target.Path] {
						pending[target.Name] = struct{}{}
						delete(t.states, target.Path)
						changed = true
					}
				}
				t.targets = targets
			}
			t.refreshed = time.Now()
			indexTargets()
			addWatches()
		}
		for _, target := range t.targets {
			current := statFile(target.Path)
			if current == t.states[target.Path] && filepath.Clean(eventPath) != filepath.Clean(target.Path) {
				continue
			}
			t.states[target.Path] = current
			pending[target.Name] = struct{}{}
			changed = true
		}
		return changed, nil
	}
	refreshDue := func() bool {
		return t.RefreshInterval <= 0 || time.Since(t.refreshed) >= t.RefreshInterval || events == nil ||
			len(watched) == 0
	}
	if changed, err := reconcile("", refreshDue()); err != nil {
		return nil, err
	} else if changed {
		arm()
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				delete(watched, event.Name)
				addWatches()
			}
			if t.RefreshInterval > 0 {
				if target, known := byPath[filepath.Clean(event.Name)]; known {
					t.states[target.Path] = statFile(target.Path)
					pending[target.Name] = struct{}{}
					arm()
				} else if len(pending) == 0 {
					// New members are discovered once after the burst. Unrelated
					// output writes never delay an already pending source change.
					arm()
				}
			} else if changed, err := reconcile(event.Name, true); err != nil {
				return nil, err
			} else if changed {
				arm()
			}
		case _, ok := <-watcherErrors:
			if !ok {
				watcherErrors = nil
				continue
			}
			// Overflow and backend failures invalidate every event assumption.
			if _, err := reconcile("", true); err != nil {
				return nil, err
			}
			for _, target := range t.targets {
				pending[target.Name] = struct{}{}
			}
			if len(pending) > 0 {
				arm()
			}
		case <-ticker.C:
			if changed, err := reconcile("", refreshDue()); err != nil {
				return nil, err
			} else if changed {
				arm()
			}
		case <-settle.C:
			if t.RefreshInterval > 0 {
				if _, err := reconcile("", true); err != nil {
					return nil, err
				}
			}
			if len(pending) == 0 {
				firstChange = time.Time{}
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
