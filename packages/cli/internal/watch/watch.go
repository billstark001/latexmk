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
	Refresh  func() ([]Target, error)
	MaxWait  time.Duration
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
	reconcile := func(eventPath string) error {
		now := time.Now()
		changed := false
		if t.Refresh != nil {
			targets, err := t.Refresh()
			if err != nil {
				return err
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
		if changed {
			if firstChange.IsZero() {
				firstChange = now
			}
			delay := t.debounce
			if t.MaxWait > 0 {
				delay = min(delay, max(0, t.MaxWait-now.Sub(firstChange)))
			}
			settle.Reset(delay)
		}
		addWatches()
		return nil
	}
	if err := reconcile(""); err != nil {
		return nil, err
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
			}
			if err := reconcile(event.Name); err != nil {
				return nil, err
			}
		case _, ok := <-watcherErrors:
			if !ok {
				watcherErrors = nil
				continue
			}
			// Overflow and backend failures invalidate every event assumption.
			if err := reconcile(""); err != nil {
				return nil, err
			}
			for _, target := range t.targets {
				pending[target.Name] = struct{}{}
			}
			if len(pending) > 0 {
				settle.Reset(t.debounce)
			}
		case <-ticker.C:
			if err := reconcile(""); err != nil {
				return nil, err
			}
		case <-settle.C:
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
