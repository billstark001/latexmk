// Package project implements the content-addressed source cache used by the
// incremental upload protocol. It never accepts client-supplied filesystem
// paths without validating them first.
package project

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/server/internal/resultarchive"
	"github.com/billstark001/latexmk/packages/server/internal/store"
)

const uploadLifetime = 15 * time.Minute

type Snapshot struct {
	ID        string            `json:"snapshotId"`
	OwnerID   string            `json:"ownerId"`
	ProjectID string            `json:"projectId"`
	Files     []api.ProjectFile `json:"files"`
}

type session struct {
	id         string
	ownerID    string
	projectID  string
	request    api.CompileRequest
	files      []api.ProjectFile
	expected   map[string]int64
	expires    time.Time
	committing bool
}

type storedSnapshot struct {
	Snapshot  Snapshot
	UpdatedAt time.Time
}

type pinnedSnapshot struct {
	Snapshot Snapshot
	Count    int
}

type Manager struct {
	cfg      config.Config
	db       *store.Postgres
	stateDir string

	mu           sync.Mutex
	sessions     map[string]session
	snapshots    map[string]storedSnapshot
	pins         map[string]pinnedSnapshot
	snapshotMu   sync.Mutex
	stateBytes   int64
	pendingBytes int64
}

func New(cfg config.Config, db *store.Postgres) (*Manager, error) {
	stateDir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	for _, dir := range []string{
		filepath.Join(stateDir, "blobs"),
		filepath.Join(stateDir, "results"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
		}
	}
	stateBytes, err := directorySize(stateDir)
	if err != nil {
		return nil, fmt.Errorf("measure state directory: %w", err)
	}
	if stateBytes > cfg.MaxStateBytes {
		return nil, fmt.Errorf("state directory already exceeds LATEXMK_MAX_STATE_BYTES (%d bytes)", cfg.MaxStateBytes)
	}
	return &Manager{
		cfg: cfg, db: db, stateDir: stateDir,
		sessions: make(map[string]session), snapshots: make(map[string]storedSnapshot), pins: make(map[string]pinnedSnapshot), stateBytes: stateBytes,
	}, nil
}

func (m *Manager) Plan(ownerID string, request api.UploadPlanRequest) (api.UploadPlan, error) {
	if ownerID == "" {
		return api.UploadPlan{}, errors.New("authenticated owner is required")
	}
	if !validProjectID(request.ProjectID) {
		return api.UploadPlan{}, errors.New("projectId may contain only letters, digits, dot, underscore, and hyphen")
	}
	if len(request.Files) == 0 {
		return api.UploadPlan{}, errors.New("project must contain at least one file")
	}
	if len(request.Files) > m.cfg.MaxFiles {
		return api.UploadPlan{}, fmt.Errorf("project contains more than %d files", m.cfg.MaxFiles)
	}
	paths := make(map[string]bool, len(request.Files))
	expected := make(map[string]int64, len(request.Files))
	var total int64
	for _, file := range request.Files {
		if !validProjectPath(file.Path) {
			return api.UploadPlan{}, fmt.Errorf("invalid project path %q", file.Path)
		}
		if paths[file.Path] {
			return api.UploadPlan{}, fmt.Errorf("duplicate project path %q", file.Path)
		}
		paths[file.Path] = true
		if !validSHA256(file.SHA256) || file.Size < 0 {
			return api.UploadPlan{}, fmt.Errorf("invalid manifest entry %q", file.Path)
		}
		if prior, seen := expected[file.SHA256]; seen && prior != file.Size {
			return api.UploadPlan{}, fmt.Errorf("inconsistent size for digest %s", file.SHA256)
		}
		expected[file.SHA256] = file.Size
		total += file.Size
		if total > m.cfg.MaxExpandedBytes {
			return api.UploadPlan{}, fmt.Errorf("project expands beyond %d bytes", m.cfg.MaxExpandedBytes)
		}
	}
	id, err := randomID("upl")
	if err != nil {
		return api.UploadPlan{}, err
	}
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, old := range m.sessions {
		if now.After(old.expires) {
			delete(m.sessions, key)
		}
	}
	if m.cfg.MaxUploadSessions > 0 && len(m.sessions) >= m.cfg.MaxUploadSessions {
		return api.UploadPlan{}, fmt.Errorf("upload session limit of %d has been reached", m.cfg.MaxUploadSessions)
	}
	missing := make([]string, 0, len(expected))
	for digest, size := range expected {
		if m.cfg.MaxUploadBytes > 0 && size > m.cfg.MaxUploadBytes {
			return api.UploadPlan{}, fmt.Errorf("file %q exceeds the per-blob upload limit of %d bytes", filePathForDigest(request.Files, digest), m.cfg.MaxUploadBytes)
		}
		if !m.hasBlob(ownerID, digest, size) {
			missing = append(missing, digest)
		}
	}
	sort.Strings(missing)
	m.sessions[id] = session{id: id, ownerID: ownerID, projectID: request.ProjectID, request: request.Request, files: append([]api.ProjectFile(nil), request.Files...), expected: expected, expires: now.Add(uploadLifetime)}
	return api.UploadPlan{UploadID: id, Missing: missing, ExpiresAt: now.Add(uploadLifetime)}, nil
}

// PutBlob stores a single manifest digest. The content length is verified and
// only a digest listed in this caller's still-valid upload session is accepted.
func (m *Manager) PutBlob(ownerID, uploadID, digest string, body io.Reader) error {
	m.mu.Lock()
	s, ok := m.sessions[uploadID]
	m.mu.Unlock()
	if !ok || s.ownerID != ownerID {
		return errors.New("upload session not found")
	}
	if time.Now().After(s.expires) {
		m.mu.Lock()
		delete(m.sessions, uploadID)
		m.mu.Unlock()
		return errors.New("upload session expired")
	}
	size, ok := s.expected[digest]
	if !ok || !validSHA256(digest) {
		return errors.New("digest is not required by this upload")
	}
	if m.hasBlob(ownerID, digest, size) {
		return nil
	}
	m.mu.Lock()
	if m.hasBlob(ownerID, digest, size) {
		m.mu.Unlock()
		return nil
	}
	if m.stateBytes+m.pendingBytes+size > m.cfg.MaxStateBytes {
		m.mu.Unlock()
		return fmt.Errorf("state storage limit of %d bytes would be exceeded", m.cfg.MaxStateBytes)
	}
	m.pendingBytes += size
	m.mu.Unlock()
	reserved := true
	defer func() {
		if reserved {
			m.mu.Lock()
			m.pendingBytes -= size
			m.mu.Unlock()
		}
	}()
	dir := filepath.Dir(m.blobPath(ownerID, digest))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(body, size+1))
	closeErr := tmp.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != size {
		return fmt.Errorf("digest %s has %d bytes; expected %d", digest, n, size)
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("uploaded file does not match its SHA-256 digest")
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	target := m.blobPath(ownerID, digest)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hasBlob(ownerID, digest, size) {
		return nil
	}
	if m.stateBytes+m.pendingBytes > m.cfg.MaxStateBytes {
		return fmt.Errorf("state storage limit of %d bytes would be exceeded", m.cfg.MaxStateBytes)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	m.stateBytes += size
	m.pendingBytes -= size
	reserved = false
	return nil
}

func (m *Manager) Commit(ctx context.Context, ownerID, uploadID string) (Snapshot, api.CompileRequest, error) {
	m.mu.Lock()
	s, ok := m.sessions[uploadID]
	if ok && time.Now().After(s.expires) {
		delete(m.sessions, uploadID)
		ok = false
	}
	if !ok || s.ownerID != ownerID {
		m.mu.Unlock()
		return Snapshot{}, api.CompileRequest{}, errors.New("upload session not found or expired")
	}
	if ok && s.committing {
		m.mu.Unlock()
		return Snapshot{}, api.CompileRequest{}, errors.New("upload session is already being committed")
	}
	if ok {
		s.committing = true
		m.sessions[uploadID] = s
	}
	m.mu.Unlock()
	release := func() {
		m.mu.Lock()
		if current, exists := m.sessions[uploadID]; exists && current.ownerID == ownerID {
			current.committing = false
			m.sessions[uploadID] = current
		}
		m.mu.Unlock()
	}
	for digest, size := range s.expected {
		if !m.hasBlob(ownerID, digest, size) {
			release()
			return Snapshot{}, api.CompileRequest{}, fmt.Errorf("missing required digest %s", digest)
		}
	}
	snapshot, err := NewSnapshot(ownerID, s.projectID, s.files)
	if err != nil {
		release()
		return Snapshot{}, api.CompileRequest{}, err
	}
	m.snapshotMu.Lock()
	defer m.snapshotMu.Unlock()
	if m.db != nil {
		manifest, err := json.Marshal(snapshot)
		if err != nil {
			release()
			return Snapshot{}, api.CompileRequest{}, err
		}
		if err := m.db.SaveSnapshot(ctx, store.ProjectSnapshot{OwnerID: ownerID, ProjectID: s.projectID, Manifest: manifest}); err != nil {
			release()
			return Snapshot{}, api.CompileRequest{}, fmt.Errorf("save project snapshot: %w", err)
		}
	} else {
		m.mu.Lock()
		m.snapshots[snapshotKey(ownerID, s.projectID)] = storedSnapshot{Snapshot: snapshot, UpdatedAt: time.Now().UTC()}
		m.mu.Unlock()
	}
	m.mu.Lock()
	delete(m.sessions, uploadID)
	m.mu.Unlock()
	return snapshot, s.request, nil
}

func (m *Manager) Snapshot(ctx context.Context, ownerID, projectID string) (Snapshot, error) {
	if m.db != nil {
		record, err := m.db.LoadSnapshot(ctx, ownerID, projectID)
		if err != nil {
			return Snapshot{}, err
		}
		var snapshot Snapshot
		if err := json.Unmarshal(record.Manifest, &snapshot); err != nil {
			return Snapshot{}, fmt.Errorf("decode project snapshot: %w", err)
		}
		if snapshot.ID == "" {
			snapshot, err = NewSnapshot(snapshot.OwnerID, snapshot.ProjectID, snapshot.Files)
			if err != nil {
				return Snapshot{}, fmt.Errorf("normalize project snapshot: %w", err)
			}
		}
		return snapshot, nil
	}
	m.snapshotMu.Lock()
	m.mu.Lock()
	stored, ok := m.snapshots[snapshotKey(ownerID, projectID)]
	m.mu.Unlock()
	m.snapshotMu.Unlock()
	if !ok {
		return Snapshot{}, errors.New("project snapshot not found")
	}
	return stored.Snapshot, nil
}

// NewSnapshot validates and canonicalizes a project manifest, then assigns a
// content-derived identifier. The owner and project are included so IDs are
// scoped to the same authorization boundary as the source blobs.
func NewSnapshot(ownerID, projectID string, files []api.ProjectFile) (Snapshot, error) {
	if ownerID == "" {
		return Snapshot{}, errors.New("snapshot owner is required")
	}
	if !validProjectID(projectID) {
		return Snapshot{}, errors.New("snapshot project ID is invalid")
	}
	if len(files) == 0 {
		return Snapshot{}, errors.New("snapshot must contain at least one file")
	}
	canonical := append([]api.ProjectFile(nil), files...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Path < canonical[j].Path })
	for i, file := range canonical {
		if !validProjectPath(file.Path) || !validSHA256(file.SHA256) || file.Size < 0 {
			return Snapshot{}, fmt.Errorf("snapshot contains invalid file %q", file.Path)
		}
		if i > 0 && canonical[i-1].Path == file.Path {
			return Snapshot{}, fmt.Errorf("snapshot contains duplicate path %q", file.Path)
		}
	}
	identity := struct {
		OwnerID   string            `json:"ownerId"`
		ProjectID string            `json:"projectId"`
		Files     []api.ProjectFile `json:"files"`
	}{OwnerID: ownerID, ProjectID: projectID, Files: canonical}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return Snapshot{}, err
	}
	digest := sha256.Sum256(encoded)
	return Snapshot{ID: "src_" + hex.EncodeToString(digest[:16]), OwnerID: ownerID, ProjectID: projectID, Files: canonical}, nil
}

// ValidateSnapshot rejects a modified or incomplete snapshot manifest.
func ValidateSnapshot(snapshot Snapshot) error {
	expected, err := NewSnapshot(snapshot.OwnerID, snapshot.ProjectID, snapshot.Files)
	if err != nil {
		return err
	}
	if snapshot.ID == "" || snapshot.ID != expected.ID {
		return errors.New("snapshot ID does not match its manifest")
	}
	return nil
}

// PinSnapshot keeps source blobs alive while a queued or running job refers to
// an older project version. Pins are process-local; database-backed recovery is
// also covered by active job manifests during pruning.
func (m *Manager) PinSnapshot(snapshot Snapshot) error {
	if err := ValidateSnapshot(snapshot); err != nil {
		return err
	}
	snapshot.Files = append([]api.ProjectFile(nil), snapshot.Files...)
	m.mu.Lock()
	defer m.mu.Unlock()
	pin := m.pins[snapshot.ID]
	if pin.Count == 0 {
		pin.Snapshot = snapshot
	}
	pin.Count++
	m.pins[snapshot.ID] = pin
	return nil
}

func (m *Manager) ReleaseSnapshot(snapshotID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pin, ok := m.pins[snapshotID]
	if !ok {
		return
	}
	if pin.Count <= 1 {
		delete(m.pins, snapshotID)
		return
	}
	pin.Count--
	m.pins[snapshotID] = pin
}

// Start keeps the state volume bounded over time. Capacity is still enforced
// synchronously on each upload/result write; this background pass reclaims
// expired results, snapshots, and orphaned content-addressed blobs.
func (m *Manager) Start(ctx context.Context, logger *slog.Logger) {
	if m.cfg.StateSweepInterval <= 0 {
		return
	}
	m.sweep(ctx, logger)
	go func() {
		ticker := time.NewTicker(m.cfg.StateSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.sweep(ctx, logger)
			}
		}
	}()
}

func (m *Manager) sweep(ctx context.Context, logger *slog.Logger) {
	reclaimed, err := m.Prune(ctx)
	if err != nil {
		logger.Error("state cache sweep failed", "error", err)
		return
	}
	if reclaimed > 0 {
		logger.Info("state cache swept", "reclaimed_bytes", reclaimed)
	}
}

// Prune is exported for deterministic maintenance and tests. It never removes
// content referenced by a non-expired upload session or current project
// snapshot, even if the blob itself is older than the retention period.
func (m *Manager) Prune(ctx context.Context) (int64, error) {
	now := time.Now().UTC()
	references := make(map[string]struct{})
	m.mu.Lock()
	for id, s := range m.sessions {
		if now.After(s.expires) {
			delete(m.sessions, id)
			continue
		}
		addExpectedReferences(references, ownerKey(s.ownerID), s.expected)
	}
	for _, pin := range m.pins {
		addSnapshotReferences(references, pin.Snapshot)
	}
	m.mu.Unlock()

	m.snapshotMu.Lock()
	defer m.snapshotMu.Unlock()
	if m.db != nil {
		if err := m.db.DeleteSnapshotsBefore(ctx, now.Add(-m.cfg.SnapshotRetention)); err != nil {
			return 0, fmt.Errorf("delete expired project snapshots: %w", err)
		}
		if err := m.db.VisitSnapshots(ctx, 100, func(record store.ProjectSnapshot) error {
			var snapshot Snapshot
			if err := json.Unmarshal(record.Manifest, &snapshot); err != nil {
				return fmt.Errorf("decode project snapshot %q: %w", record.ID, err)
			}
			addSnapshotReferences(references, snapshot)
			return nil
		}); err != nil {
			return 0, fmt.Errorf("visit project snapshots: %w", err)
		}
		if err := m.db.VisitActiveJobSnapshots(ctx, 100, func(manifest []byte) error {
			var snapshot Snapshot
			if err := json.Unmarshal(manifest, &snapshot); err != nil {
				return fmt.Errorf("decode active job snapshot: %w", err)
			}
			if err := ValidateSnapshot(snapshot); err != nil {
				return fmt.Errorf("validate active job snapshot: %w", err)
			}
			addSnapshotReferences(references, snapshot)
			return nil
		}); err != nil {
			return 0, fmt.Errorf("visit active job snapshots: %w", err)
		}
	} else {
		m.mu.Lock()
		for key, stored := range m.snapshots {
			if stored.UpdatedAt.Before(now.Add(-m.cfg.SnapshotRetention)) {
				delete(m.snapshots, key)
				continue
			}
			addSnapshotReferences(references, stored.Snapshot)
		}
		m.mu.Unlock()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if now.After(s.expires) {
			delete(m.sessions, id)
			continue
		}
		addExpectedReferences(references, ownerKey(s.ownerID), s.expected)
	}
	for _, pin := range m.pins {
		addSnapshotReferences(references, pin.Snapshot)
	}
	var reclaimed int64
	var err error
	if m.cfg.ResultRetention > 0 {
		reclaimed, err = removeExpiredRegularFiles(filepath.Join(m.stateDir, "results"), now.Add(-m.cfg.ResultRetention), nil)
		if err != nil {
			return 0, err
		}
	}
	if m.cfg.BlobRetention > 0 {
		removed, removeErr := removeExpiredRegularFiles(filepath.Join(m.stateDir, "blobs"), now.Add(-m.cfg.BlobRetention), references)
		if removeErr != nil {
			return 0, removeErr
		}
		reclaimed += removed
	}
	stateBytes, err := directorySize(m.stateDir)
	if err != nil {
		return 0, fmt.Errorf("measure state directory after sweep: %w", err)
	}
	m.stateBytes = stateBytes
	return reclaimed, nil
}

func (m *Manager) Materialize(snapshot Snapshot, destination string) error {
	for _, file := range snapshot.Files {
		if !validProjectPath(file.Path) || !m.hasBlob(snapshot.OwnerID, file.SHA256, file.Size) {
			return fmt.Errorf("project snapshot is missing valid content for %q", file.Path)
		}
		output, err := safeDestination(destination, file.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
			return err
		}
		in, err := os.Open(m.blobPath(snapshot.OwnerID, file.SHA256))
		if err != nil {
			return err
		}
		out, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = in.Close()
			return err
		}
		_, copyErr := io.CopyN(out, in, file.Size)
		closeIn := in.Close()
		closeOut := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeIn != nil {
			return closeIn
		}
		if closeOut != nil {
			return closeOut
		}
	}
	return nil
}

func (m *Manager) ResultPath(ownerID, jobID string) (string, error) {
	if ownerID == "" || !validProjectID(jobID) {
		return "", errors.New("valid owner and job identifier are required")
	}
	dir := filepath.Join(m.stateDir, "results", ownerKey(ownerID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, jobID+".tar.gz"), nil
}

// WriteResult reserves state-volume capacity before adding a result archive.
// The reservation uses the uncompressed inputs as a safe upper bound; the
// stored gzip archive normally consumes substantially less.
func (m *Manager) WriteResult(ownerID, jobID string, output compile.Output) (string, error) {
	path, err := m.ResultPath(ownerID, jobID)
	if err != nil {
		return "", err
	}
	var reserve int64 = int64(len(output.Stdout) + len(output.Stderr) + 4096)
	for _, file := range output.Files {
		reserve += file.Size + 1024
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var replaced int64
	if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
		replaced = info.Size()
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return "", statErr
	}
	if m.stateBytes+m.pendingBytes-replaced+reserve > m.cfg.MaxStateBytes {
		return "", fmt.Errorf("state storage limit of %d bytes would be exceeded", m.cfg.MaxStateBytes)
	}
	if replaced > 0 {
		if err := os.Remove(path); err != nil {
			return "", err
		}
	}
	if err := resultarchive.Write(path, output); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if m.stateBytes-replaced+info.Size() > m.cfg.MaxStateBytes {
		_ = os.Remove(path)
		return "", fmt.Errorf("state storage limit of %d bytes would be exceeded", m.cfg.MaxStateBytes)
	}
	m.stateBytes = m.stateBytes - replaced + info.Size()
	return path, nil
}

func (m *Manager) blobPath(ownerID, digest string) string {
	return filepath.Join(m.stateDir, "blobs", ownerKey(ownerID), digest[:2], digest)
}

func (m *Manager) hasBlob(ownerID, digest string, size int64) bool {
	if !validSHA256(digest) {
		return false
	}
	info, err := os.Stat(m.blobPath(ownerID, digest))
	return err == nil && info.Mode().IsRegular() && info.Size() == size
}

func safeDestination(root, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("project path escapes workspace")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(rootAbs, clean)
	if !strings.HasPrefix(path, rootAbs+string(filepath.Separator)) {
		return "", errors.New("project path escapes workspace")
	}
	return path, nil
}

func validProjectPath(value string) bool {
	if value == "" || len(value) > 4096 || strings.Contains(value, "\\") || strings.ContainsRune(value, 0) {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	return clean != "." && clean != ".." && !filepath.IsAbs(clean) && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func validProjectID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return value != "." && value != ".."
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ownerKey(ownerID string) string {
	hash := sha256.Sum256([]byte(ownerID))
	return hex.EncodeToString(hash[:16])
}

func snapshotKey(ownerID, projectID string) string { return ownerID + "\x00" + projectID }

func filePathForDigest(files []api.ProjectFile, digest string) string {
	for _, file := range files {
		if file.SHA256 == digest {
			return file.Path
		}
	}
	return digest
}

func addExpectedReferences(references map[string]struct{}, owner string, expected map[string]int64) {
	for digest := range expected {
		references[owner+"\x00"+digest] = struct{}{}
	}
}

func addSnapshotReferences(references map[string]struct{}, snapshot Snapshot) {
	owner := ownerKey(snapshot.OwnerID)
	for _, file := range snapshot.Files {
		if validSHA256(file.SHA256) {
			references[owner+"\x00"+file.SHA256] = struct{}{}
		}
	}
}

func removeExpiredRegularFiles(root string, cutoff time.Time, references map[string]struct{}) (int64, error) {
	var reclaimed int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("state directory contains unsupported file %q", path)
		}
		if !info.ModTime().Before(cutoff) {
			return nil
		}
		if references != nil {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if len(parts) == 3 && validSHA256(parts[2]) && parts[1] == parts[2][:2] {
				if _, referenced := references[parts[0]+"\x00"+parts[2]]; referenced {
					return nil
				}
			}
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		reclaimed += info.Size()
		return nil
	})
	return reclaimed, err
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("state directory contains unsupported file %q", path)
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func randomID(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(b), nil
}
