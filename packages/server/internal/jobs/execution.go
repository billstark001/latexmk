package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/project"
	"github.com/billstark001/latexmk/packages/server/internal/sandbox"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

type execution struct {
	invalidateCheckpoint bool
	output               compile.Output
	cache                *protocol.CompileCache
	cacheKey             string
	sessionCheckpoint    bool
	checkpoint           string
	stamps               map[string]int64
}

func (m *Manager) execute(ctx context.Context, rec record, workspace *compile.Workspace) execution {
	// Every queued job must leave the controller's daemon credential boundary.
	// Closing a session cannot cause its job to fall back to a native runner.
	isolated := m.cfg.RunnerImage != ""
	if isolated {
		if rec.Job.SessionID == "" {
			return m.executePortableIsolated(ctx, rec, workspace)
		}
		return m.executeIsolated(ctx, rec, workspace)
	}
	e := execution{cacheKey: project.CompileCacheKey(rec.Request, m.meta, m.cfg.CompileCacheEpoch)}
	if rec.Request.Auxiliary.Server == "reuse" {
		info := protocol.CompileCache{Status: "bypass", Reason: "forced clean compile"}
		if !rec.Request.Force {
			info = m.projects.RestoreCompileCache(rec.Snapshot, e.cacheKey, workspace.Project)
		}
		e.cache = &info
	}
	e.output = m.runner.Run(ctx, workspace.Project, rec.Request, rec.Job.ID)
	if e.cache != nil && e.cache.Status == "hit" && !e.output.Result.Success && !e.output.Result.TimedOut &&
		ctx.Err() == nil {
		e.cache.ColdRetry = true
		e.cache.Reason = "warm compile failed; retried with clean sources"
		err := workspace.Reset()
		if err == nil {
			err = m.projects.Materialize(rec.Snapshot, workspace.Project)
		}
		if err != nil {
			e.output = failedOutput(rec, err)
			return e
		}
		e.output = m.runner.Run(ctx, workspace.Project, rec.Request, rec.Job.ID)
	}
	return e
}

func (m *Manager) executeIsolated(ctx context.Context, rec record, workspace *compile.Workspace) execution {
	e := execution{stamps: make(map[string]int64)}
	m.admissionMu.Lock()
	s := m.sessions[rec.Job.SessionID]
	reuse := s != nil && s.state.Workspace == "reuse"
	e.sessionCheckpoint = reuse
	cachePath := ""
	if s != nil && s.state.Workspace == "reuse" && !rec.Request.Force && time.Now().Before(s.cacheExpires) &&
		project.CompatibleCacheInputs(s.cacheInputs, rec.Snapshot.Files) {
		cachePath = s.cachePath
	}
	for _, file := range rec.Snapshot.Files {
		stamp := int64(946684800) + int64(rec.Job.Revision)
		if cachePath != "" && s.cacheHashes[file.Path] == file.SHA256 {
			stamp = s.cacheStamps[file.Path]
		}
		e.stamps[file.Path] = stamp
	}
	m.admissionMu.Unlock()
	info := protocol.CompileCache{Status: "miss", Reason: "no compatible session checkpoint"}
	if cachePath != "" {
		info.Status = "hit"
		info.Reason = "successful isolated workspace checkpoint"
	}
	if rec.Request.Force {
		info.Status = "bypass"
		info.Reason = "forced clean compile"
	}
	if reuse {
		e.cache = &info
	}
	m.runIsolated(ctx, rec, workspace, &e, cachePath)
	return e
}

func failedOutput(rec record, err error) compile.Output {
	return compile.Output{
		Result: protocol.CompileResult{
			ProtocolVersion: protocol.Version,
			RequestID:       rec.Job.ID,
			Entry:           rec.Request.Entry,
			Engine:          rec.Request.Engine,
			ExitCode:        -1,
			Error:           err.Error(),
		},
	}
}

// publishExecution prepares state outside admissionMu after terminal success.
// Cancellation cannot overwrite that conditional transition; session closure
// can still prevent checkpoint publication by removing the session.
func (m *Manager) publishExecution(ctx context.Context, rec record, workspace *compile.Workspace, e *execution) {
	if e.cache == nil || !e.output.Result.Success || ctx.Err() != nil {
		return
	}
	if e.sessionCheckpoint {
		if e.checkpoint == "" {
			return
		}
		root, err := safefs.Open(filepath.Dir(e.checkpoint))
		if err != nil {
			e.cache.Warning = err.Error()
			return
		}
		file, err := root.OpenRegular(filepath.Base(e.checkpoint))
		if err != nil {
			_ = root.Close()
			e.cache.Warning = err.Error()
			return
		}
		hash, size, err := safefs.Digest(file, m.cfg.MaxCompileCacheBytes+(1<<20))
		err = errors.Join(err, file.Close(), root.Close())
		if err != nil {
			e.cache.Warning = err.Error()
			return
		}
		publication, err := m.projects.StageLiveCache(rec.OwnerID, rec.Job.SessionID, e.checkpoint, size, hash)
		if err != nil {
			e.cache.Warning = err.Error()
			return
		}
		defer func() {
			if err := publication.Close(); err != nil {
				m.logger.Warn("discard staged checkpoint", "error", err)
			}
		}()
		m.admissionMu.Lock()
		defer m.admissionMu.Unlock()
		s := m.sessions[rec.Job.SessionID]
		if s == nil || s.state.Workspace != "reuse" || s.state.RunningJobID != rec.Job.ID || ctx.Err() != nil {
			return
		}
		if err := publication.Commit(); err != nil {
			e.cache.Warning = err.Error()
			return
		}
		ttl := m.cfg.CompileCacheRetention
		if rec.Request.Auxiliary.ServerTTL != "" {
			requested, _ := time.ParseDuration(rec.Request.Auxiliary.ServerTTL)
			ttl = min(ttl, requested)
		}
		expires := time.Now().UTC().Add(ttl)
		s.cacheExpires = expires
		e.output.Result.AuxiliaryExpiresAt = &expires
		s.cachePath = publication.Path()
		s.cacheInputs = append([]protocol.ProjectFile(nil), rec.Snapshot.Files...)
		s.cacheStamps = e.stamps
		s.cacheHashes = make(map[string]string, len(rec.Snapshot.Files))
		for _, file := range rec.Snapshot.Files {
			s.cacheHashes[file.Path] = file.SHA256
		}
		e.invalidateCheckpoint = false
		return
	}
	if rec.Request.Auxiliary.ServerTTL != "" {
		ttl, _ := time.ParseDuration(rec.Request.Auxiliary.ServerTTL)
		ttl = min(ttl, m.cfg.CompileCacheRetention)
		expires := time.Now().UTC().Add(ttl)
		e.output.Result.AuxiliaryExpiresAt = &expires
	}
	count, err := m.projects.SaveCompileCache(
		rec.Snapshot,
		e.cacheKey,
		workspace.Project,
		rec.Job.ID,
		rec.Job.CreatedAt,
		e.output,
	)
	e.cache.StoredFiles = count
	if err != nil {
		e.cache.Warning = err.Error()
	}
}

func (m *Manager) clearSessionCacheLocked(s *liveSession) {
	s.cacheExpires = time.Time{}
	s.cachePath = ""
	s.cacheInputs = nil
	s.cacheStamps = nil
	s.cacheHashes = nil
	if err := m.projects.DeleteLiveCache(s.ownerID, s.state.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.logger.Warn("remove session checkpoint", "error", err)
	}
}

func (m *Manager) updatePublishedCache(ctx context.Context, rec record, e execution) {
	if e.cache == nil || !e.output.Result.Success {
		return
	}
	operation, cancel := m.persistenceContext(ctx)
	defer cancel()
	result := *rec.Job.Result
	cache := *e.cache
	result.CompileCache = &cache
	result.AuxiliaryExpiresAt = e.output.Result.AuxiliaryExpiresAt
	rec.Job.Result = &result
	rec.CompletionFrom = "succeeded"
	_, err := m.transition(operation, rec, "succeeded")
	if err != nil {
		m.deferCompletion(rec)
		m.logger.Warn("update completed cache accounting", "job_id", rec.Job.ID, "error", err)
	}
}

// Ordinary jobs retain their portable auxiliary policy while moving all TeX
// execution out of a controller that has access to the Docker daemon.
func (m *Manager) executePortableIsolated(ctx context.Context, rec record, workspace *compile.Workspace) execution {
	e := execution{
		cacheKey: project.CompileCacheKey(rec.Request, m.meta, m.cfg.CompileCacheEpoch),
		stamps:   make(map[string]int64),
	}
	for _, file := range rec.Snapshot.Files {
		e.stamps[file.Path] = 946684800
	}
	checkpoint := ""
	if rec.Request.Auxiliary.Server == "reuse" {
		info := protocol.CompileCache{Status: "bypass", Reason: "forced clean compile"}
		if !rec.Request.Force {
			portable := filepath.Join(workspace.Path, "portable")
			if err := os.MkdirAll(portable, 0700); err != nil {
				e.output = failedOutput(rec, err)
				return e
			}
			info = m.projects.RestoreCompileCache(rec.Snapshot, e.cacheKey, portable)
			if info.Status == "hit" {
				checkpoint = filepath.Join(workspace.Path, "portable.tar.gz")
				if err := sandbox.ArchiveCheckpoint(
					portable,
					checkpoint,
					m.cfg.MaxFiles,
					m.cfg.MaxCompileCacheBytes,
				); err != nil {
					info.Status, info.Reason, info.Warning = "miss", "portable checkpoint packaging failed", err.Error()
					checkpoint = ""
				}
			}
		}
		e.cache = &info
	}
	m.runIsolated(ctx, rec, workspace, &e, checkpoint)
	return e
}

func (m *Manager) runIsolated(
	ctx context.Context,
	rec record,
	workspace *compile.Workspace,
	e *execution,
	checkpoint string,
) {
	var err error
	e.output, e.checkpoint, err = sandbox.Run(
		ctx,
		m.cfg,
		rec.Request,
		rec.Job.ID,
		rec.Snapshot.Files,
		e.stamps,
		checkpoint,
		workspace.Project,
		filepath.Join(workspace.Path, "isolated"),
		e.sessionCheckpoint,
	)
	// A completed compiler error cannot mutate the previous checkpoint: each
	// attempt restores a private copy. Invalid transport/state is discarded.
	e.invalidateCheckpoint = e.sessionCheckpoint && checkpoint != "" && err != nil && ctx.Err() == nil
	if checkpoint != "" && (err != nil || !e.output.Result.Success) && ctx.Err() == nil && !e.output.Result.TimedOut {
		e.cache.ColdRetry = true
		e.cache.Reason = "warm state failed; retried with clean sources"
		// The cold attempt uses a separate transport directory and a new container.
		e.output, e.checkpoint, err = sandbox.Run(
			ctx,
			m.cfg,
			rec.Request,
			rec.Job.ID+"-cold",
			rec.Snapshot.Files,
			e.stamps,
			"",
			workspace.Project,
			filepath.Join(workspace.Path, "isolated-cold"),
			e.sessionCheckpoint,
		)
	}
	if err != nil {
		e.output = failedOutput(rec, err)
		e.output.Result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	}
	// The worker's private attempt ID is replaced by the public immutable job ID.
	e.output.Result.RequestID = rec.Job.ID
}
