package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/platform/safefs"
	"github.com/billstark001/latexmk/packages/server/internal/project"
	"github.com/billstark001/latexmk/packages/server/internal/sandbox"
)

type execution struct {
	output     compile.Output
	cache      *api.CompileCache
	cacheKey   string
	isolated   bool
	checkpoint string
	stamps     map[string]int64
}

func (m *Manager) execute(ctx context.Context, rec record, workspace *compile.Workspace) execution {
	m.admissionMu.Lock()
	s := m.sessions[rec.Job.SessionID]
	isolated := s != nil && s.state.Workspace == "reuse"
	m.admissionMu.Unlock()
	if isolated {
		return m.executeIsolated(ctx, rec, workspace)
	}
	e := execution{cacheKey: project.CompileCacheKey(rec.Request, m.meta, m.cfg.CompileCacheEpoch)}
	if rec.Request.Auxiliary.Server == "reuse" {
		info := api.CompileCache{Status: "bypass", Reason: "forced clean compile"}
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
	e := execution{isolated: true, stamps: make(map[string]int64)}
	m.admissionMu.Lock()
	s := m.sessions[rec.Job.SessionID]
	cachePath := ""
	if s != nil && !rec.Request.Force && project.CompatibleCacheInputs(s.cacheInputs, rec.Snapshot.Files) {
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
	info := api.CompileCache{Status: "miss", Reason: "no compatible session checkpoint"}
	if cachePath != "" {
		info.Status = "hit"
		info.Reason = "successful isolated workspace checkpoint"
	}
	if rec.Request.Force {
		info.Status = "bypass"
		info.Reason = "forced clean compile"
	}
	e.cache = &info
	var err error
	e.output, e.checkpoint, err = sandbox.Run(
		ctx,
		m.cfg,
		rec.Request,
		rec.Job.ID,
		rec.Snapshot.Files,
		e.stamps,
		cachePath,
		workspace.Project,
		filepath.Join(workspace.Path, "isolated"),
	)
	if cachePath != "" && (err != nil || !e.output.Result.Success) && ctx.Err() == nil && !e.output.Result.TimedOut {
		info.ColdRetry = true
		info.Reason = "warm workspace failed; retried with clean sources"
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
		)
	}
	if err != nil {
		e.output = failedOutput(rec, err)
		e.output.Result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	}
	// The worker's private attempt ID is replaced by the public immutable job ID.
	e.output.Result.RequestID = rec.Job.ID
	return e
}

func failedOutput(rec record, err error) compile.Output {
	return compile.Output{
		Result: api.CompileResult{
			ProtocolVersion: api.ProtocolVersion,
			RequestID:       rec.Job.ID,
			Entry:           rec.Request.Entry,
			Engine:          rec.Request.Engine,
			ExitCode:        -1,
			Error:           err.Error(),
		},
	}
}

// publishExecution runs under admissionMu after durable result publication. A
// cancellation cannot interleave with cache publication and terminal transition.
func (m *Manager) publishExecution(ctx context.Context, rec record, workspace *compile.Workspace, e *execution) {
	if e.cache == nil || !e.output.Result.Success || ctx.Err() != nil {
		return
	}
	if e.isolated {
		s := m.sessions[rec.Job.SessionID]
		if s == nil || s.state.RunningJobID != rec.Job.ID || e.checkpoint == "" {
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
		hash, size, err := checkpointDigest(file, m.cfg.MaxCompileCacheBytes+(1<<20))
		err = errors.Join(err, file.Close(), root.Close())
		if err != nil {
			e.cache.Warning = err.Error()
			return
		}
		path, err := m.projects.SaveLiveCache(rec.OwnerID, rec.Job.SessionID, e.checkpoint, size, hash)
		if err != nil {
			e.cache.Warning = err.Error()
			return
		}
		s.cachePath = path
		s.cacheInputs = append([]api.ProjectFile(nil), rec.Snapshot.Files...)
		s.cacheStamps = e.stamps
		s.cacheHashes = make(map[string]string, len(rec.Snapshot.Files))
		for _, file := range rec.Snapshot.Files {
			s.cacheHashes[file.Path] = file.SHA256
		}
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
	s.cachePath = ""
	s.cacheInputs = nil
	s.cacheStamps = nil
	s.cacheHashes = nil
	if err := m.projects.DeleteLiveCache(s.ownerID, s.state.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.logger.Warn("remove session checkpoint", "error", err)
	}
}

func checkpointDigest(r io.Reader, max int64) (string, int64, error) {
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(r, max+1))
	if err == nil && size > max {
		err = safefs.ErrLimit
	}
	return hex.EncodeToString(hash.Sum(nil)), size, err
}
