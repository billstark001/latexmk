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
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

// ExportArtifact verifies a downloaded artifact and atomically exports it to a
// PDF path inside projectRoot. Absolute destinations within that root are allowed.
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
	destination = filepath.Clean(destination)
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
		kind = protocol.ClassifyArtifactPath(artifact.Path)
	}
	if kind != "auxiliary" {
		if err := os.MkdirAll(output, 0700); err != nil {
			return err
		}
		return writeArtifact(output, artifact.Path, input, artifact.Size, artifact.SHA256)
	}
	switch req.Auxiliary.Local {
	case "", "none":
		if err := safefs.CopyVerified(io.Discard, input, artifact.Size, strings.ToLower(artifact.SHA256)); err != nil {
			return fmt.Errorf("verify discarded auxiliary %q: %w", artifact.Path, err)
		}
		return nil
	case "cache":
		key := sha256.Sum256([]byte(req.Entry + "\x00" + req.Engine + "\x00" + req.JobName))
		relative := filepath.Join(".latexmk-cache", "aux", hex.EncodeToString(key[:16]), artifact.Path)
		return writeArtifact(root, filepath.ToSlash(relative), input, artifact.Size, artifact.SHA256)
	case "output":
	default:
		return fmt.Errorf("unknown auxiliary.local %q", req.Auxiliary.Local)
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return err
	}
	return writeArtifact(output, artifact.Path, input, artifact.Size, artifact.SHA256)
}
