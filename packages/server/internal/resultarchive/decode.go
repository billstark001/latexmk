package resultarchive

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/billstark001/latexmk/packages/server/internal/api"
	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/platform/safefs"
)

type Limits struct {
	MaxFiles     int
	MaxArtifacts int64
	MaxLogs      int64
}

// Decode verifies every declared artifact before returning host filesystem
// references. The compiler container's output is always treated as untrusted.
func Decode(reader io.Reader, destination string, limits Limits) (compile.Output, error) {
	var out compile.Output
	scoped, err := safefs.Open(destination)
	if err != nil {
		return out, err
	}
	defer func() { _ = scoped.Close() }()
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return out, err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	first, err := tr.Next()
	if err != nil || first.Name != "result.json" || first.Typeflag != tar.TypeReg || first.Size > 4<<20 ||
		first.Size < 0 {
		return out, errors.New("invalid result envelope")
	}
	raw, err := safefs.ReadLimited(tr, 4<<20)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out.Result); err != nil {
		return out, err
	}
	declared := make(map[string]api.Artifact)
	var total int64
	for _, artifact := range out.Result.Artifacts {
		clean, err := safefs.Clean(artifact.Path)
		if err != nil || clean != artifact.Path || artifact.Size < 0 || len(artifact.SHA256) != 64 {
			return out, errors.New("invalid result artifact metadata")
		}
		if _, exists := declared[artifact.Path]; exists {
			return out, errors.New("duplicate result artifact")
		}
		total += artifact.Size
		if total > limits.MaxArtifacts || len(declared) >= limits.MaxFiles {
			return out, errors.New("result exceeds artifact limits")
		}
		declared[artifact.Path] = artifact
	}
	seen := map[string]bool{"result.json": true}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, err
		}
		if seen[header.Name] || header.Typeflag != tar.TypeReg || header.Size < 0 {
			return out, errors.New("invalid result archive member")
		}
		seen[header.Name] = true
		switch header.Name {
		case "stdout.log", "stderr.log":
			data, err := safefs.ReadLimited(tr, limits.MaxLogs)
			if err != nil {
				return out, err
			}
			if header.Name == "stdout.log" {
				out.Stdout = data
			} else {
				out.Stderr = data
			}
		default:
			if !strings.HasPrefix(header.Name, "artifacts/") {
				return out, errors.New("unexpected result archive member")
			}
			name := strings.TrimPrefix(header.Name, "artifacts/")
			artifact, ok := declared[name]
			if !ok || artifact.Size != header.Size {
				return out, fmt.Errorf("undeclared or inconsistent artifact %q", name)
			}
			if err := scoped.WriteExclusive(
				header.Name,
				artifact.Size,
				func(w io.Writer) error { return safefs.CopyVerified(w, tr, artifact.Size, artifact.SHA256) },
			); err != nil {
				return out, err
			}
			out.Files = append(
				out.Files,
				compile.File{
					Workspace:    filepath.Join(destination, "artifacts"),
					RelativePath: name,
					Size:         artifact.Size,
					SHA256:       artifact.SHA256,
				},
			)
		}
	}
	if !seen["stdout.log"] || !seen["stderr.log"] || len(out.Files) != len(declared) {
		return out, errors.New("incomplete result archive")
	}
	if tail, err := safefs.ReadLimited(gz, 1024); err != nil || len(tail) != 0 {
		return out, errors.New("unexpected result archive trailer")
	}
	return out, nil
}
