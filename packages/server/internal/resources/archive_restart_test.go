package resources

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func TestRestartRemovesOnlyUncommittedArchiveAndIndexSiblings(t *testing.T) {
	m := testManager(t)
	env := archiveEnvironment(t, m, []byte("verified archive"))
	checksum := env.Packages[0].Checksum
	if err := m.PutPackageArchive(
		context.Background(),
		"alice",
		env.Digest,
		checksum,
		16,
		strings.NewReader("verified archive"),
	); err != nil {
		t.Fatal(err)
	}
	const incomplete = "interrupted publication bytes"
	kept := map[string]string{
		"archives/" + checksum + ".tar.xz":               "verified archive",
		"archives/keep.guard":                            "archive guard",
		"metadata/" + strings.Repeat("a", 64) + ".tlpdb": "cached index",
		"metadata/.latexmk-keep.guard":                   "metadata guard",
	}
	for name, content := range kept {
		if strings.HasPrefix(name, "archives/"+checksum) {
			continue
		}
		if err := os.WriteFile(filepath.Join(m.dir, filepath.FromSlash(name)), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{"archives", "metadata"} {
		root, err := safefs.Open(filepath.Join(m.dir, directory))
		if err != nil {
			t.Fatal(err)
		}
		// Stage closes its data descriptor. Abandoning the pending publication
		// and closing the root models a crash before Commit/Close without an
		// open-file leak in this process.
		_, err = root.Stage("unfinished-object", 128, func(writer io.Writer) error {
			_, err := io.WriteString(writer, incomplete)
			return err
		})
		closeErr := root.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("stage crash fixture: %v %v", err, closeErr)
		}
	}
	before, err := diskBytes(m.dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(m.cfg, protocol.Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	after, err := diskBytes(m.dir)
	if err != nil || before-after != int64(2*len(incomplete)) {
		t.Fatalf("uncommitted bytes retained after restart: before=%d after=%d error=%v", before, after, err)
	}
	for name, content := range kept {
		got, err := os.ReadFile(filepath.Join(m.dir, filepath.FromSlash(name)))
		if err != nil || string(got) != content {
			t.Fatalf("startup removed or modified published/guard object %s: %q %v", name, got, err)
		}
	}
	for _, directory := range []string{"archives", "metadata"} {
		entries, err := os.ReadDir(filepath.Join(m.dir, directory))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if safefs.IsStagedName(entry.Name()) {
				t.Fatalf("startup retained private sibling %s", entry.Name())
			}
		}
	}
	manifest, err := restarted.PackageArchives(context.Background(), "alice", env.Digest)
	if err != nil || len(manifest.Archives) != 1 || !manifest.Archives[0].Present {
		t.Fatalf("restart lost authorized verified archives: %+v %v", manifest, err)
	}
}
