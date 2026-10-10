package main

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	"github.com/billstark001/latexmk/packages/cli/internal/dependency"
	projectwatch "github.com/billstark001/latexmk/packages/cli/internal/watch"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

type sourceTracker struct {
	*projectwatch.Tracker
	targets []projectwatch.Target
}

// newSourceTracker shares dependency and policy observation between independent
// jobs and realtime sessions. Settings invalidation stays with the session adapter.
func newSourceTracker(
	c *client.Client,
	request protocol.CompileRequest,
	opts compileOptions,
	files []projectarchive.File,
	extra []projectwatch.Target,
) (*sourceTracker, error) {
	targets := append(watchTargets(opts, files), extra...)
	tracker, err := projectwatch.New(targets, opts.watchInterval, opts.watchDebounce, opts.watchMaxWait)
	if err != nil {
		return nil, err
	}
	source := &sourceTracker{Tracker: tracker, targets: targets}
	tracker.Refresh = func() ([]projectwatch.Target, error) {
		selection, err := c.SelectionPaths(request.Entry, request.Engine)
		if err != nil {
			return nil, err
		}
		source.targets = append(watchTargets(opts, selection.Files), extra...)
		return source.targets, nil
	}
	return source, nil
}

type liveObservation struct {
	changed chan struct{}
	reload  chan struct{}
	errors  chan error
}

// Lease renewal must continue while capture, upload or result downloads block
// the revision loop. SSE is a read-only subscriber and never renews the lease.
func observeSessionLease(
	ctx context.Context,
	c *client.Client,
	id string,
	interval, timeout time.Duration,
) <-chan error {
	failures := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				operation, cancel := context.WithTimeout(ctx, min(timeout, interval))
				_, err := c.RenewSession(operation, id)
				cancel()
				if err != nil && ctx.Err() == nil {
					select {
					case failures <- err:
					default:
					}
				}
			}
		}
	}()
	return failures
}

func observeLive(
	ctx context.Context,
	c *client.Client,
	request protocol.CompileRequest,
	opts compileOptions,
	invalidate context.CancelCauseFunc,
) liveObservation {
	observation := liveObservation{
		changed: make(chan struct{}, 1),
		reload:  make(chan struct{}, 1),
		errors:  make(chan error, 1),
	}
	{
		selection, err := c.SelectionPaths(request.Entry, request.Engine)
		if err != nil {
			observation.errors <- err
			return observation
		}
		var settings []projectwatch.Target
		for _, file := range opts.controlFiles {
			if file == "" {
				continue
			}
			if !filepath.IsAbs(file) {
				file = filepath.Join(opts.projectRoot, file)
			}
			settings = append(settings, projectwatch.Target{Name: "settings: " + file, Path: file})
		}
		tracker, err := newSourceTracker(c, request, opts, selection.Files, settings)
		if err != nil {
			observation.errors <- err
			return observation
		}
		tracker.RefreshInterval = 2 * time.Second
		refresh := tracker.Refresh
		tracker.Refresh = func() ([]projectwatch.Target, error) {
			targets, err := refresh()
			if err != nil {
				return tracker.targets, nil
			} // Freeze rechecks policy before every upload.
			return targets, nil
		}
		go func() {
			for {
				names, err := tracker.Wait(ctx)
				if err != nil {
					if ctx.Err() == nil {
						observation.errors <- err
					}
					return
				}
				for _, name := range names {
					if strings.HasPrefix(name, "settings: ") || strings.HasPrefix(name, "ignore policy ") ||
						strings.HasPrefix(name, "Git policy ") || strings.HasPrefix(name, "dependency manifest ") {
						invalidate(errLiveSettingsChanged)
						select {
						case observation.reload <- struct{}{}:
						default:
						}
						return
					}
				}
				select {
				case observation.changed <- struct{}{}:
				default:
				}
			}
		}()
	}
	return observation
}

func observeSession(ctx context.Context, c *client.Client, id string) <-chan struct{} {
	changed := make(chan struct{}, 1)
	go func() {
		after := uint64(0)
		for ctx.Err() == nil {
			streamCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			_ = c.StreamSessionEvents(streamCtx, id, after, func(event protocol.SessionEvent) {
				after = event.Sequence
				if event.Type != "finished" && event.Type != "resync" {
					return
				}
				select {
				case changed <- struct{}{}:
				default:
				}
			})
			cancel()
			if !waitForContext(ctx, time.Second) {
				return
			}
		}
	}()
	return changed
}

func selectedFilesChanged(before, after []projectarchive.File) bool {
	if len(before) != len(after) {
		return true
	}
	current := make(map[string]string, len(after))
	for _, file := range after {
		current[file.Path] = file.SHA256
	}
	for _, file := range before {
		if current[file.Path] != file.SHA256 {
			return true
		}
	}
	return false
}

func watchTargets(opts compileOptions, files []projectarchive.File) []projectwatch.Target {
	targets := make([]projectwatch.Target, 0, len(files)+8)
	for _, file := range files {
		targets = append(targets, projectwatch.Target{Name: file.Path, Path: file.Source})
	}
	if opts.manifestFile != "" {
		if clean, err := dependency.NormalizeExplicitManifestPath(opts.manifestFile); err == nil {
			targets = append(
				targets,
				projectwatch.Target{
					Name: "dependency manifest " + clean,
					Path: filepath.Join(opts.projectRoot, filepath.FromSlash(clean)),
				},
			)
		}
	}
	if opts.manifestFile == "" && opts.uploadMode == "manifest" && len(opts.includeFiles) == 0 {
		for _, name := range []string{".latexmk-manifest", ".latexmk-files"} {
			targets = append(
				targets,
				projectwatch.Target{Name: "dependency manifest " + name, Path: filepath.Join(opts.projectRoot, name)},
			)
		}
	}
	names := opts.ignoreFiles
	if names == nil {
		names = []string{".latexmkignore"}
	}
	for _, name := range names {
		targets = append(
			targets,
			projectwatch.Target{Name: "ignore policy " + name, Path: filepath.Join(opts.projectRoot, name)},
		)
	}
	if !opts.gitIgnore {
		return targets
	}
	repoRoot, err := config.FindGitRoot(opts.projectRoot)
	if err != nil {
		return targets
	}
	policyPaths := make(map[string]struct{})
	for _, file := range files {
		for dir := filepath.Dir(file.Source); ; dir = filepath.Dir(dir) {
			policyPaths[filepath.Join(dir, ".gitignore")] = struct{}{}
			if dir == repoRoot || filepath.Dir(dir) == dir {
				break
			}
		}
	}
	policyPaths[filepath.Join(repoRoot, ".git", "info", "exclude")] = struct{}{}
	if globalExcludes, ok := effectiveGitExcludesFile(repoRoot); ok {
		policyPaths[globalExcludes] = struct{}{}
	}
	for policyPath := range policyPaths {
		label, relErr := filepath.Rel(opts.projectRoot, policyPath)
		if relErr != nil {
			label = policyPath
		}
		targets = append(targets, projectwatch.Target{Name: "Git policy " + filepath.ToSlash(label), Path: policyPath})
	}
	return targets
}

func waitForContext(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
