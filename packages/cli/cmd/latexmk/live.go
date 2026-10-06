package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/dependency"
	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
	projectwatch "github.com/billstark001/latexmk/packages/cli/internal/watch"
)

const reloadLiveSettings = -20

type liveObservation struct {
	changed chan struct{}
	reload  chan struct{}
	errors  chan error
}

func observeLive(
	ctx context.Context,
	c *client.Client,
	request protocol.CompileRequest,
	opts compileOptions,
) liveObservation {
	observation := liveObservation{
		changed: make(chan struct{}, 1),
		reload:  make(chan struct{}, 1),
		errors:  make(chan error, 1),
	}
	go func() {
		selection, err := c.SelectionPaths(request.Entry, request.Engine)
		if err != nil {
			observation.errors <- err
			return
		}
		targets := watchTargets(opts, selection.Files)
		for _, file := range opts.controlFiles {
			if file == "" {
				continue
			}
			if !filepath.IsAbs(file) {
				file = filepath.Join(opts.projectRoot, file)
			}
			targets = append(targets, projectwatch.Target{Name: "settings: " + file, Path: file})
		}
		tracker, err := projectwatch.New(targets, opts.watchInterval, opts.watchDebounce)
		if err != nil {
			observation.errors <- err
			return
		}
		tracker.Refresh = func() ([]projectwatch.Target, error) {
			next, err := c.SelectionPaths(request.Entry, request.Engine)
			if err != nil {
				return targets, nil
			} // Never upload from this fallback; Freeze rechecks policy.
			refreshed := watchTargets(opts, next.Files)
			for _, target := range targets {
				if strings.HasPrefix(target.Name, "settings: ") {
					refreshed = append(refreshed, target)
				}
			}
			targets = refreshed
			return targets, nil
		}
		for {
			names, err := tracker.Wait(ctx)
			if err != nil {
				if ctx.Err() == nil {
					observation.errors <- err
				}
				return
			}
			for _, name := range names {
				if strings.HasPrefix(name, "settings: ") {
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

func runLive(c *client.Client, request protocol.CompileRequest, opts compileOptions) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	operation, finish := context.WithTimeout(ctx, opts.timeout)
	meta, err := c.Metadata(operation)
	finish()
	if err != nil {
		return fail(err)
	}
	if !meta.Capabilities.RealtimeSessions {
		return fail(&client.CapabilityError{Capability: "realtime sessions"})
	}
	if err := client.ValidateRealtimeRequest(request, meta); err != nil {
		return fail(err)
	}
	request.RecordInputs = meta.Capabilities.DependencyInputs
	request.DetectMissingFiles = meta.Capabilities.NeedsFiles && (c.UploadMode == "" || c.UploadMode == "auto")
	mode := "fresh"
	if request.Auxiliary.Server == "reuse" {
		mode = "reuse"
	}
	observation := observeLive(ctx, c, request, opts)
	for ctx.Err() == nil {
		operation, finish := context.WithTimeout(ctx, opts.timeout)
		session, err := c.CreateSession(
			operation,
			protocol.SessionRequest{ProjectID: c.ProjectID, Request: request, Workspace: mode},
		)
		finish()
		if err != nil {
			if permanentLiveError(err) {
				return fail(err)
			}
			fmt.Fprintln(os.Stderr, "latexmk: session connection failed; retrying:", err)
			if !waitForContext(ctx, 2*time.Second) {
				return 0
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "latexmk: realtime session %s (%s workspace)\n", session.ID, mode)
		code := runLiveSession(ctx, c, request, opts, meta, session, observation)
		cleanup, finish := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.CloseSession(cleanup, session.ID)
		finish()
		if code != -1 {
			return code
		}
		fmt.Fprintln(os.Stderr, "latexmk: session ended; reconnecting with a fresh source snapshot")
	}
	return 0
}

func runLiveSession(
	parent context.Context,
	c *client.Client,
	request protocol.CompileRequest,
	opts compileOptions,
	meta protocol.Metadata,
	session protocol.Session,
	observation liveObservation,
) int {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	events := observeSession(ctx, c, session.ID)
	poll := time.NewTicker(5 * time.Second)
	defer poll.Stop()
	verify := time.NewTicker(30 * time.Second)
	defer verify.Stop()
	// A buffered signal coalesces changes while snapshot capture/upload is active.
	trigger := make(chan struct{}, 1)
	trigger <- struct{}{}
	var lastFiles []projectarchive.File
	var pending *client.PreparedRevision
	var pendingFiles []projectarchive.File
	dirty := true
	var additional []string
	missingRounds := 0
	lastReported := ""
	displayed := uint64(0)
	submit := func() error {
		operation, finish := context.WithTimeout(ctx, opts.timeout)
		defer finish()
		acceptPending := func() error {
			job, err := c.CommitRevision(operation, *pending)
			if err != nil {
				var failure *client.HTTPError
				if errors.As(err, &failure) && failure.StatusCode == http.StatusConflict {
					pending = nil
				}
				return err
			}
			session.Revision, session.LatestJobID = job.Revision, job.ID
			lastFiles = pendingFiles
			pending, pendingFiles = nil, nil
			fmt.Fprintf(os.Stderr, "latexmk: submitted revision %d (%s)\n", job.Revision, job.ID)
			return nil
		}
		if pending != nil {
			if err := acceptPending(); err != nil {
				return err
			}
		}
		dirty = true
		frozen, err := c.FreezeSnapshot(operation, request, additional, meta)
		if err != nil {
			return err
		}
		defer func() { _ = frozen.Close() }()
		if lastFiles != nil && !selectedFilesChanged(lastFiles, frozen.Files) {
			dirty = false
			return nil
		}
		state, err := c.GetSession(operation, session.ID)
		if err != nil {
			return err
		}
		session = state
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		prepared, err := c.PrepareRevision(operation, session, request, frozen, hex.EncodeToString(nonce[:]))
		if err != nil {
			return err
		}
		pending = &prepared
		pendingFiles = append([]projectarchive.File(nil), frozen.Files...)
		if err := acceptPending(); err != nil {
			return err
		}
		dirty = false
		return nil
	}
	report := func() error {
		operation, finish := context.WithTimeout(ctx, opts.timeout)
		defer finish()
		state, err := c.GetSession(operation, session.ID)
		if err != nil {
			return err
		}
		session = state
		// Deliver a completed success even if newer input is pending, but never move
		// backwards. Consumers can compare its revision with the session's wanted one.
		if state.LastSuccessfulJobID != "" && state.LastSuccessfulJobID != lastReported {
			job, err := c.GetJob(operation, state.LastSuccessfulJobID)
			if err != nil {
				return err
			}
			if job.Revision > displayed {
				out, root, err := c.DownloadLiveResult(operation, job, request, opts.outDir)
				if err != nil {
					return err
				}
				if out.Result.Success {
					publishedOptions := opts
					publishedOptions.outDir = root
					if err := exportPDF(publishedOptions, out); err != nil {
						return err
					}
					if len(out.Result.InputFiles) > 0 {
						if err := dependency.SaveCachedInputs(
							c.ProjectRoot,
							request.Entry,
							request.Engine,
							out.Result.InputFiles,
						); err != nil {
							fmt.Fprintln(os.Stderr, "latexmk: dependency cache:", err)
						}
					}
					fmt.Fprintf(
						os.Stderr,
						"latexmk: published revision %d (wanted %d), bundle: %s\n",
						job.Revision,
						state.Revision,
						root,
					)
					reportCompile(out, nil, opts)
					displayed = job.Revision
					lastReported = job.ID
				}
			}
		}
		if state.LatestJobID == "" {
			return nil
		}
		job, err := c.GetJob(operation, state.LatestJobID)
		if err != nil {
			return err
		}
		if job.ID == lastReported || job.Status != "failed" {
			return nil
		}
		lastReported = job.ID
		fmt.Fprintf(os.Stderr, "latexmk: revision %d failed; retaining the last successful PDF\n", job.Revision)
		if job.Result == nil {
			fmt.Fprintln(os.Stderr, "latexmk:", job.Error)
			return nil
		}
		out, _, err := c.DownloadLiveResult(operation, job, request, opts.outDir)
		if err != nil {
			return err
		}
		reportCompile(out, nil, opts)
		if request.DetectMissingFiles && missingRounds < 3 && len(out.Result.NeedsFiles) > 0 {
			allowed, err := c.ResolveMissingFiles(out.Result.NeedsFiles, lastFiles, additional)
			if err != nil {
				fmt.Fprintln(os.Stderr, "latexmk: missing-file recovery refused:", err)
				return nil
			}
			if len(allowed) > 0 {
				missingRounds++
				additional = append(additional, allowed...)
				lastFiles = nil
				select {
				case trigger <- struct{}{}:
				default:
				}
			}
		}
		return nil
	}
	handle := func(err error) int {
		if err == nil {
			return -2
		}
		var failure *client.HTTPError
		if errors.As(err, &failure) && failure.StatusCode == http.StatusNotFound {
			return -1
		}
		if permanentLiveError(err) {
			return fail(err)
		}
		fmt.Fprintln(os.Stderr, "latexmk: realtime operation deferred:", err)
		return -2
	}
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-observation.reload:
			fmt.Fprintln(os.Stderr, "latexmk: settings changed; rebuilding the session")
			return reloadLiveSettings
		case err := <-observation.errors:
			return fail(err)
		case <-observation.changed:
			missingRounds = 0
			if code := handle(submit()); code != -2 {
				return code
			}
		case <-trigger:
			if code := handle(submit()); code != -2 {
				return code
			}
		case <-verify.C:
			if code := handle(submit()); code != -2 {
				return code
			}
		case <-poll.C:
			if code := handle(report()); code != -2 {
				return code
			}
			// Retry disconnected uploads even when no further editor event occurs.
			if dirty {
				if code := handle(submit()); code != -2 {
					return code
				}
			}
		case <-events:
			if code := handle(report()); code != -2 {
				return code
			}
		}
	}
}

func permanentLiveError(err error) bool {
	var failure *client.HTTPError
	return errors.As(err, &failure) &&
		(failure.StatusCode == http.StatusUnauthorized || failure.StatusCode == http.StatusForbidden || failure.StatusCode == http.StatusBadRequest)
}
