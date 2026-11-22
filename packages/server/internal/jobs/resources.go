package jobs

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"

	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/resources"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

// ValidateRequest authorizes the environment before accepting source uploads.
func (m *Manager) ValidateRequest(owner string, request protocol.CompileRequest) error {
	if request.EnvironmentDigest == "" {
		return compile.ValidateRequest(m.cfg, request)
	}
	lease, err := m.pinEnvironment(owner, request.EnvironmentDigest)
	if err != nil {
		return err
	}
	defer lease.Release()
	return compile.ValidateRequest(lease.RunnerConfig(m.cfg), request)
}

func (m *Manager) pinEnvironment(owner, digest string) (*resources.Lease, error) {
	if digest == "" {
		return nil, nil
	}
	if m.environments == nil {
		return nil, errors.New("resource environments are not configured")
	}
	return m.environments.Pin(owner, digest)
}

func (m *Manager) pinJobEnvironment(rec record) error {
	if rec.Request.EnvironmentDigest == "" {
		return nil
	}
	lease, err := m.pinEnvironment(rec.OwnerID, rec.Request.EnvironmentDigest)
	if err != nil {
		return fmt.Errorf("pin compile environment: %w", err)
	}
	m.mu.Lock()
	previous := m.environmentPins[rec.Job.ID]
	m.environmentPins[rec.Job.ID] = lease
	m.mu.Unlock()
	if previous != nil {
		previous.Release()
	}
	return nil
}

func (m *Manager) releaseEnvironment(jobID string) {
	m.mu.Lock()
	lease := m.environmentPins[jobID]
	delete(m.environmentPins, jobID)
	m.mu.Unlock()
	if lease != nil {
		lease.Release()
	}
}

func (m *Manager) releaseJobInputs(rec record) {
	m.projects.ReleaseSnapshot(rec.Snapshot.ID)
	m.releaseEnvironment(rec.Job.ID)
}

func resourceInput(lease *resources.Lease, cfg config.Config) (compile.ResourceInput, error) {
	var input compile.ResourceInput
	if lease == nil {
		return input, nil
	}
	input.FontLookup = lease.Environment().Baseline.FontLookup
	var total int64
	for _, resource := range lease.Files() {
		total += resource.Size
		if total > cfg.MaxResourceBytes || len(input.Files) >= cfg.MaxResourceFiles {
			return input, errors.New("resource inputs exceed environment limits")
		}
		input.Files = append(input.Files, compile.ResourceFile{Path: resource.Path, File: compile.File{
			Workspace: filepath.Dir(resource.Source), RelativePath: filepath.Base(resource.Source),
			Size: resource.Size, SHA256: resource.SHA256,
		}})
		if resource.Kind == "font" {
			input.FontDirs = append(input.FontDirs, path.Dir(resource.Path))
		}
	}
	slices.Sort(input.FontDirs)
	input.FontDirs = slices.Compact(input.FontDirs)
	var err error
	input.TexmfFiles, err = compile.DescribeResourceTree(
		lease.Tree(),
		cfg.MaxResourceFiles-len(input.Files),
		cfg.MaxResourceBytes-total,
	)
	return input, err
}
