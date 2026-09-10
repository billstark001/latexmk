package client

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
)

// ExportArtifact applies the same path and checksum checks as result downloads.
func ExportArtifact(outputRoot, projectRoot, destination string, artifact protocol.Artifact) error {
	if err := validateArtifactMetadata(artifact); err != nil {
		return err
	}
	if filepath.IsAbs(destination) {
		var err error
		destination, err = filepath.Rel(projectRoot, destination)
		if err != nil {
			return err
		}
	}
	if !filepath.IsLocal(destination) || !strings.EqualFold(filepath.Ext(destination), ".pdf") {
		return fmt.Errorf("PDF export must be a .pdf path inside the project root")
	}
	input, err := projectarchive.OpenFile(projectarchive.File{
		Path: artifact.Path, Source: filepath.Join(outputRoot, filepath.FromSlash(artifact.Path)),
	})
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	return writeArtifact(projectRoot, filepath.ToSlash(destination), input, artifact.Size, artifact.SHA256)
}

func validateAuxiliaryCapability(req protocol.CompileRequest, meta protocol.Metadata) error {
	// Old servers may understand reuse but silently ignore retention/TTL fields.
	// Do not claim that an explicitly requested storage policy was enforced.
	if !meta.Capabilities.AuxiliaryRetention &&
		(req.Auxiliary.Server == "none" || req.Auxiliary.Server == "retain" || req.Auxiliary.ServerTTL != "") {
		return &CapabilityError{Capability: "auxiliary retention policy"}
	}
	return nil
}

func storeReturnedArtifact(
	root, output string,
	req protocol.CompileRequest,
	artifact protocol.Artifact,
	input io.Reader,
) error {
	if err := validateArtifactMetadata(artifact); err != nil {
		return err
	}
	kind := artifact.Kind
	if kind == "" {
		switch {
		case strings.HasSuffix(artifact.Path, ".pdf"), strings.HasSuffix(artifact.Path, ".synctex.gz"):
			kind = "output"
		case strings.HasSuffix(artifact.Path, ".log"), strings.HasSuffix(artifact.Path, ".blg"):
			kind = "diagnostic"
		default:
			kind = "auxiliary"
		}
	}
	if kind != "auxiliary" {
		if err := os.MkdirAll(output, 0700); err != nil {
			return err
		}
		return writeArtifact(output, artifact.Path, input, artifact.Size, artifact.SHA256)
	}
	switch req.Auxiliary.Local {
	case "", "none":
		hash := sha256.New()
		size, err := io.CopyN(hash, input, artifact.Size)
		if err != nil {
			return err
		}
		if size != artifact.Size || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
			return fmt.Errorf("artifact %s SHA-256 mismatch", artifact.Path)
		}
		return nil
	case "cache":
		key := sha256.Sum256([]byte(req.Entry + "\x00" + req.Engine + "\x00" + req.JobName))
		// Validate/create each parent without following project-local symlinks.
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		relative := filepath.Join(".latexmk-cache", "aux", hex.EncodeToString(key[:16]), artifact.Path)
		parent, err := ensureSafeParent(resolved, filepath.Dir(relative))
		if err != nil {
			return err
		}
		output = parent
		artifact.Path = filepath.Base(artifact.Path)
	case "output":
	default:
		return fmt.Errorf("unknown auxiliary.local %q", req.Auxiliary.Local)
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return err
	}
	return writeArtifact(output, artifact.Path, input, artifact.Size, artifact.SHA256)
}
