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
	// RefreshInterval batches native event hints before rediscovering membership.
	// Zero refreshes selection for every event and poll.
	RefreshInterval time.Duration
	// AdaptiveDebounce extends the quiet window when distinct changes arrive close together.
	AdaptiveDebounce   bool
	targets            []Target
	states             map[string]fileState
	refreshed          time.Time
	interval           time.Duration
	debounce           time.Duration
	maxWait            time.Duration
	lastDistinctChange time.Time
}

const adaptiveDebounceLimit = 500 * time.Millisecond

type settleWindow struct {
	firstChange time.Time
	quiet       time.Duration
}

func (t *Tracker) observeChange(now time.Time, quiet time.Duration, distinct bool) time.Duration {
	if !t.AdaptiveDebounce || t.debounce == 0 || !distinct {
		return quiet
	}
	previous := t.lastDistinctChange
	t.lastDistinctChange = now
	if gap := now.Sub(previous); !previous.IsZero() && gap > 0 && gap < adaptiveDebounceLimit {
		// Keep the window open for one more change at the observed cadence,
		// plus a cushion for differences in save and notification timing.
		quiet = max(quiet, min(adaptiveDebounceLimit, gap+t.debounce/2))
	}
	return quiet
}

func (t *Tracker) settleDelay(now time.Time, window *settleWindow, distinct bool) time.Duration {
	if window.firstChange.IsZero() {
		window.firstChange = now
		window.quiet = t.debounce
	}
	window.quiet = t.observeChange(now, window.quiet, distinct)
	return min(window.quiet, max(0, t.maxWait-now.Sub(window.firstChange)))
}

func New(targets []Target, interval, debounce, maxWait time.Duration) (*Tracker, error) {
	if interval <= 0 {
		return nil, errors.New("watch interval must be positive")
	}
	if debounce < 0 {
		return nil, errors.New("watch debounce cannot be negative")
	}
	if maxWait <= 0 {
		return nil, errors.New("watch max wait must be positive")
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
		maxWait:  maxWait,
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
	var window settleWindow
	byPath := make(map[string]Target, len(t.targets))
	indexTargets := func() {
		clear(byPath)
		for _, target := range t.targets {
			byPath[filepath.Clean(target.Path)] = target
		}
	}
	indexTargets()
	arm := func(distinct bool) {
		settle.Reset(t.settleDelay(time.Now(), &window, distinct))
	}
	reconcile := func(eventPath string, refresh bool) (changed, distinct bool, err error) {
		if t.Refresh != nil && refresh {
			targets, err := t.Refresh()
			if err != nil {
				return false, false, err
			}
			targets, err = normalizeTargets(targets)
			if err != nil {
				return false, false, err
			}
			{
				next := make(map[string]bool, len(targets))
				for _, target := range targets {
					next[target.Path] = true
					if _, exists := t.states[target.Path]; !exists {
						pending[target.Name] = struct{}{}
						changed, distinct = true, true
					}
				}
				for _, target := range t.targets {
					if !next[target.Path] {
						pending[target.Name] = struct{}{}
						delete(t.states, target.Path)
						changed, distinct = true, true
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
			stateChanged := current != t.states[target.Path]
			if !stateChanged && filepath.Clean(eventPath) != filepath.Clean(target.Path) {
				continue
			}
			t.states[target.Path] = current
			pending[target.Name] = struct{}{}
			changed = true
			distinct = distinct || stateChanged
		}
		return changed, distinct, nil
	}
	refreshDue := func() bool {
		return t.RefreshInterval <= 0 || time.Since(t.refreshed) >= t.RefreshInterval || events == nil ||
			len(watched) == 0
	}
	if changed, distinct, err := reconcile("", refreshDue()); err != nil {
		return nil, err
	} else if changed {
		arm(distinct)
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
					current := statFile(target.Path)
					// An unchanged stat still triggers capture. It may be a
					// same-metadata save or another notification for one save.
					distinct := current != t.states[target.Path]
					t.states[target.Path] = current
					pending[target.Name] = struct{}{}
					arm(distinct)
				} else if len(pending) == 0 {
					// New members are discovered once after the burst. Unrelated
					// output writes never delay an already pending source change.
					arm(false)
				}
			} else if changed, distinct, err := reconcile(event.Name, true); err != nil {
				return nil, err
			} else if changed {
				arm(distinct)
			}
		case _, ok := <-watcherErrors:
			if !ok {
				watcherErrors = nil
				continue
			}
			// Overflow and backend failures invalidate every event assumption.
			_, distinct, err := reconcile("", true)
			if err != nil {
				return nil, err
			}
			for _, target := range t.targets {
				pending[target.Name] = struct{}{}
			}
			if len(pending) > 0 {
				arm(distinct)
			}
		case <-ticker.C:
			if changed, distinct, err := reconcile("", refreshDue()); err != nil {
				return nil, err
			} else if changed {
				arm(distinct)
			}
		case <-settle.C:
			if t.RefreshInterval > 0 {
				_, distinct, err := reconcile("", true)
				if err != nil {
					return nil, err
				}
				if distinct {
					// Newly discovered changes still contribute to the next burst's history.
					t.observeChange(time.Now(), window.quiet, true)
				}
			}
			if len(pending) == 0 {
				window = settleWindow{}
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
