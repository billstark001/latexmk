package project

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
)

// Only portable TeX state is restored. In particular, .fls, .fdb_latexmk,
// .run.xml and final outputs may contain old absolute workspace paths or
// suppress necessary tool runs. Every job still runs latexmk from fresh sources.
func reusableAuxiliary(path string) bool {
	switch filepath.Ext(path) {
	case ".aux", ".toc", ".lof", ".lot", ".out", ".bbl", ".nav", ".snm":
		return true
	}
	return false
}

type auxiliaryFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}

type compileCacheRecord struct {
	Version   int               `json:"version"`
	Submitted time.Time         `json:"submitted"`
	JobID     string            `json:"jobId"`
	Inputs    []api.ProjectFile `json:"inputs"`
	Files     []auxiliaryFile   `json:"files"`
}

// CompileCacheKey deliberately excludes input hashes: ordinary edits to TeX
// should benefit from the previous auxiliary state. Input compatibility is
// checked separately before restoring anything.
func CompileCacheKey(req api.CompileRequest, meta api.Metadata, epoch string) string {
	req.Auxiliary = api.AuxiliaryOptions{}
	req.Force, req.Quiet, req.RecordInputs, req.DetectMissingFiles = false, false, false, false
	payload, _ := json.Marshal(struct {
		Format    int
		Request   api.CompileRequest
		Version   string
		Commit    string
		BuildDate string
		Profile   string
		Toolchain map[string]string
		Epoch     string
	}{1, req, meta.Version, meta.Commit, meta.BuildDate, meta.ImageProfile, meta.Toolchain, epoch})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func (m *Manager) compileCacheDir(ownerID, projectID string) string {
	return filepath.Join(m.stateDir, "compile-cache", ownerKey(ownerID), ownerKey(projectID))
}

func (m *Manager) compileCachePath(snapshot Snapshot, key string) (string, error) {
	if snapshot.OwnerID == "" || !validProjectID(snapshot.ProjectID) || !validSHA256(key) {
		return "", errors.New("invalid compile cache identity")
	}
	return filepath.Join(m.compileCacheDir(snapshot.OwnerID, snapshot.ProjectID), key+".json.gz"), nil
}

func (m *Manager) readCompileCache(path string) (compileCacheRecord, error) {
	var record compileCacheRecord
	info, err := os.Lstat(path)
	if err != nil {
		return record, err
	}
	if !info.Mode().IsRegular() {
		return record, errors.New("cache is not a regular file")
	}
	if m.cfg.CompileCacheRetention <= 0 || time.Since(info.ModTime()) > m.cfg.CompileCacheRetention {
		return record, errors.New("cache expired")
	}
	// Base64 content and the bounded source manifest are included in this cap.
	limit := 2*m.cfg.MaxCompileCacheBytes + (16 << 20)
	if info.Size() > limit {
		return record, errors.New("cache exceeds size limit")
	}
	f, err := os.Open(path)
	if err != nil {
		return record, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return record, err
	}
	defer gz.Close()
	payload, err := io.ReadAll(io.LimitReader(gz, limit+1))
	if err != nil {
		return record, err
	}
	if int64(len(payload)) > limit {
		return record, errors.New("cache exceeds expanded size limit")
	}
	if err := json.Unmarshal(payload, &record); err != nil {
		return record, err
	}
	if record.Version != 1 || len(record.Inputs) > m.cfg.MaxFiles || len(record.Files) > m.cfg.MaxFiles {
		return record, errors.New("invalid cache format or file count")
	}
	var total int64
	seen := make(map[string]bool)
	for _, file := range record.Files {
		if !validProjectPath(file.Path) || filepath.ToSlash(filepath.Clean(file.Path)) != file.Path || !reusableAuxiliary(file.Path) || seen[file.Path] {
			return record, errors.New("invalid cached auxiliary path")
		}
		seen[file.Path] = true
		total += int64(len(file.Data))
		if total > m.cfg.MaxCompileCacheBytes {
			return record, errors.New("auxiliary cache exceeds size limit")
		}
		digest := sha256.Sum256(file.Data)
		if hex.EncodeToString(digest[:]) != file.SHA256 {
			return record, errors.New("auxiliary cache checksum mismatch")
		}
	}
	return record, nil
}

func compatibleCacheInputs(previous, current []api.ProjectFile) bool {
	if len(previous) != len(current) {
		return false
	}
	old := make(map[string]api.ProjectFile, len(previous))
	for _, file := range previous {
		old[file.Path] = file
	}
	if len(old) != len(current) {
		return false
	}
	for _, file := range current {
		prior, exists := old[file.Path]
		if !exists {
			return false
		}
		// Bibliographies, classes, styles, graphics and supplied auxiliary sources
		// invalidate the warm start. Additions/removals also start completely clean.
		if filepath.Ext(file.Path) != ".tex" && (prior.SHA256 != file.SHA256 || prior.Size != file.Size) {
			return false
		}
	}
	return true
}

// RestoreCompileCache is best effort: corrupt or incompatible state produces
// a cold build, never a partial warm start or overwritten source file.
func (m *Manager) RestoreCompileCache(snapshot Snapshot, key, workspace string) api.CompileCache {
	info := api.CompileCache{Status: "miss", Reason: "no compatible cache"}
	path, err := m.compileCachePath(snapshot, key)
	if err != nil {
		info.Warning = err.Error()
		return info
	}
	m.mu.Lock()
	record, err := m.readCompileCache(path)
	m.mu.Unlock()
	if err != nil {
		if !os.IsNotExist(err) {
			info.Reason = "cache unavailable"
			info.Warning = err.Error()
		}
		return info
	}
	if !compatibleCacheInputs(record.Inputs, snapshot.Files) {
		info.Reason = "input set or non-TeX input changed"
		return info
	}
	sources := make(map[string]bool)
	for _, file := range snapshot.Files {
		sources[filepath.ToSlash(filepath.Clean(file.Path))] = true
	}
	created := []string{}
	for _, file := range record.Files {
		if sources[file.Path] {
			err = errors.New("cached auxiliary would overwrite a source")
			break
		}
		var destination string
		destination, err = safeCacheFile(workspace, file.Path, true)
		if err != nil {
			break
		}
		var out *os.File
		out, err = os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			break
		}
		created = append(created, destination)
		_, err = out.Write(file.Data)
		closeErr := out.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			break
		}
	}
	if err != nil {
		for _, path := range created {
			_ = os.Remove(path)
		}
		info.Reason, info.Warning = "cache restore failed; cold build", err.Error()
		return info
	}
	info.Status, info.Reason, info.RestoredFiles = "hit", "portable auxiliary state restored", len(created)
	return info
}

// SaveCompileCache publishes a complete successful generation atomically. An
// older submitted job cannot replace a cache from a newer successful job.
func (m *Manager) SaveCompileCache(snapshot Snapshot, key, workspace, jobID string, submitted time.Time, output compile.Output) (int, error) {
	if !output.Result.Success || output.Result.TimedOut {
		return 0, nil
	}
	path, err := m.compileCachePath(snapshot, key)
	if err != nil {
		return 0, err
	}
	record := compileCacheRecord{Version: 1, Submitted: submitted, JobID: jobID, Inputs: snapshot.Files}
	sources := make(map[string]bool)
	for _, file := range snapshot.Files {
		sources[filepath.ToSlash(filepath.Clean(file.Path))] = true
	}
	var total int64
	for _, file := range output.Files {
		if !reusableAuxiliary(file.RelativePath) || sources[file.RelativePath] {
			continue
		}
		source, err := safeCacheFile(workspace, file.RelativePath, false)
		if err != nil {
			return 0, err
		}
		stat, err := os.Lstat(source)
		if err != nil || !stat.Mode().IsRegular() {
			return 0, errors.New("auxiliary is not a regular file")
		}
		if stat.Size() != file.Size || total+stat.Size() > m.cfg.MaxCompileCacheBytes {
			return 0, errors.New("auxiliary cache exceeds size limit or file changed")
		}
		input, err := os.Open(source)
		if err != nil {
			return 0, err
		}
		data, readErr := io.ReadAll(io.LimitReader(input, file.Size+1))
		closeErr := input.Close()
		if readErr != nil {
			return 0, readErr
		}
		if closeErr != nil {
			return 0, closeErr
		}
		if int64(len(data)) != file.Size {
			return 0, errors.New("auxiliary changed after collection")
		}
		if bytes.Contains(data, []byte(workspace)) {
			return 0, errors.New("auxiliary contains a nonportable workspace path")
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != file.SHA256 {
			return 0, errors.New("auxiliary changed after collection")
		}
		total += int64(len(data))
		record.Files = append(record.Files, auxiliaryFile{file.RelativePath, file.SHA256, data})
		if len(record.Files) > m.cfg.MaxFiles {
			return 0, errors.New("too many auxiliary files")
		}
	}
	if len(record.Files) == 0 {
		return 0, nil
	}
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	if err := json.NewEncoder(gz).Encode(record); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if prior, err := m.readCompileCache(path); err == nil && (prior.Submitted.After(submitted) || (prior.Submitted.Equal(submitted) && prior.JobID > jobID)) {
		return 0, nil
	}
	var replaced int64
	if stat, err := os.Lstat(path); err == nil {
		if !stat.Mode().IsRegular() {
			return 0, errors.New("cache destination is not regular")
		}
		replaced = stat.Size()
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	// Include both the old and temporary new generation in the hard quota.
	if m.stateBytes+m.pendingBytes+int64(buffer.Len()) > m.cfg.MaxStateBytes {
		return 0, errors.New("state storage limit prevents caching auxiliary files")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".compile-cache-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(buffer.Bytes())
	closeErr := tmp.Close()
	if err != nil {
		return 0, err
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return 0, err
	}
	m.stateBytes += int64(buffer.Len()) - replaced
	return len(record.Files), nil
}

// safeCacheFile rejects symlinks in every component, including the leaf.
func safeCacheFile(root, rel string, createParents bool) (string, error) {
	if !validProjectPath(rel) || filepath.ToSlash(filepath.Clean(rel)) != rel {
		return "", errors.New("invalid auxiliary path")
	}
	parts := strings.Split(rel, "/")
	current := root
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) && createParents {
			if i == len(parts)-1 {
				return current, nil
			}
			if err := os.Mkdir(current, 0o700); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !info.IsDir()) {
			return "", errors.New("unsafe auxiliary path component")
		}
	}
	return current, nil
}

// CompileCacheStats participates in the existing exact-preview cleanup digest.
func (m *Manager) CompileCacheStats(ownerID, projectID string) (int, int64, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	paths, err := filepath.Glob(filepath.Join(m.compileCacheDir(ownerID, projectID), "*.json.gz"))
	if err != nil {
		return 0, 0, "", err
	}
	sort.Strings(paths)
	hash := sha256.New()
	var total int64
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return 0, 0, "", err
		}
		if !info.Mode().IsRegular() {
			return 0, 0, "", errors.New("invalid cache file")
		}
		total += info.Size()
		fmt.Fprintf(hash, "%s\x00%d\x00%d\n", filepath.Base(path), info.Size(), info.ModTime().UnixNano())
	}
	return len(paths), total, hex.EncodeToString(hash.Sum(nil)), nil
}

func (m *Manager) DeleteCompileCaches(ownerID, projectID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dir := m.compileCacheDir(ownerID, projectID)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return 0, nil
	}
	size, err := directorySize(dir)
	if err != nil {
		return 0, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return 0, err
	}
	m.stateBytes -= size
	return size, nil
}
