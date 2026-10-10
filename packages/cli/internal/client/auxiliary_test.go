package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestExplicitRetentionRequiresServerCapability(t *testing.T) {
	for _, options := range []protocol.AuxiliaryOptions{
		{Server: "none"}, {Server: "retain"}, {Server: "reuse", ServerTTL: "1h"},
	} {
		req := protocol.CompileRequest{Auxiliary: options}
		meta := protocol.Metadata{}
		if err := validateAuxiliaryCapability(req, meta); err == nil {
			t.Fatal("old server silently accepted retention policy")
		}
		meta.Capabilities.AuxiliaryRetention = true
		if err := validateAuxiliaryCapability(req, meta); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocalAuxiliaryPlacementAndVerification(t *testing.T) {
	data := []byte("auxiliary state")
	hash := sha256.Sum256(data)
	artifact := protocol.Artifact{
		Path:   "main.aux",
		Size:   int64(len(data)),
		SHA256: hex.EncodeToString(hash[:]),
		Kind:   "auxiliary",
	}
	for _, mode := range []string{"none", "cache", "output"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			output := filepath.Join(root, "output")
			req := protocol.CompileRequest{
				Entry:     "main.tex",
				Engine:    "xelatex",
				Auxiliary: protocol.AuxiliaryOptions{Local: mode},
			}
			if err := storeReturnedArtifact(root, output, req, artifact, bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			var names []string
			if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					names = append(names, path)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if mode == "none" && len(names) != 0 {
				t.Fatalf("none saved files: %v", names)
			}
			if mode != "none" && len(names) != 1 {
				t.Fatalf("missing auxiliary: %v", names)
			}
			if mode == "output" && names[0] != filepath.Join(output, "main.aux") {
				t.Fatalf("wrong output: %v", names)
			}
			if err := storeReturnedArtifact(
				root,
				output,
				req,
				artifact,
				bytes.NewReader(bytes.Repeat([]byte("x"), len(data))),
			); err == nil {
				t.Fatal("checksum failure ignored")
			}
		})
	}
}

func TestLegacyArtifactClassificationPreservesOutputsAndDiagnostics(t *testing.T) {
	for _, name := range []string{"main.PDF", "main.SYNCTEX.GZ", "main.ILG", "main.GLG", "main.log", "main.aux"} {
		t.Run(name, func(t *testing.T) {
			root, output := t.TempDir(), t.TempDir()
			data := "verified artifact"
			digest := sha256.Sum256([]byte(data))
			artifact := protocol.Artifact{Path: name, Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}
			if err := storeReturnedArtifact(
				root,
				output,
				protocol.CompileRequest{},
				artifact,
				strings.NewReader(data),
			); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(output, name))
			if name == "main.aux" {
				if !os.IsNotExist(err) {
					t.Fatalf("discarded auxiliary was written: %q err=%v", got, err)
				}
			} else if err != nil || string(got) != data {
				t.Fatalf("output or diagnostic was discarded: %q err=%v", got, err)
			}
		})
	}
}
