package safefs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
)

func removalDigest(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func TestRemoveVerifiedLeavesChangedProtectedAndNonRegularPaths(t *testing.T) {
	root := testRoot(t)
	if err := root.WriteExclusive("paper.pdf", 3, textWriter("old")); err != nil {
		t.Fatal(err)
	}
	info, err := root.Lstat("paper.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := root.RemoveVerified("paper.pdf", 3, removalDigest("old"), info); err != nil || removed {
		t.Fatal("removed protected file", removed, err)
	}
	if removed, err := root.RemoveVerified("paper.pdf", 3, removalDigest("new")); err != nil || removed {
		t.Fatal("removed changed file", removed, err)
	}
	if removed, err := root.RemoveVerified("missing.pdf", 3, removalDigest("old")); err != nil || removed {
		t.Fatal(removed, err)
	}
	if err := root.MakeDirs("directory.pdf", 0700); err != nil {
		t.Fatal(err)
	}
	if removed, err := root.RemoveVerified("directory.pdf", 3, removalDigest("old")); err != nil || removed {
		t.Fatal("removed directory", removed, err)
	}
	if removed, err := root.RemoveVerified(
		"paper.pdf",
		3,
		strings.ToUpper(removalDigest("old")),
	); err != nil ||
		!removed {
		t.Fatal("did not remove verified file", removed, err)
	}
	if _, err := root.Lstat("paper.pdf"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := root.Symlink("missing.pdf", "link.pdf"); err != nil {
		t.Skip(err)
	}
	if removed, err := root.RemoveVerified("link.pdf", 3, removalDigest("old")); err != nil || removed {
		t.Fatal("removed symlink", removed, err)
	}
}

func TestRemovalRestoresAtomicReplacementAndNeverOverwritesNewPath(t *testing.T) {
	for _, occupied := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore", true: "occupied"}[occupied], func(t *testing.T) {
			root := testRoot(t)
			if err := root.WriteExclusive("paper.pdf", 3, textWriter("old")); err != nil {
				t.Fatal(err)
			}
			expected, err := root.Lstat("paper.pdf")
			if err != nil {
				t.Fatal(err)
			}
			// Simulate the exact race: after verification, an editor atomically
			// replaces the path; cleanup then quarantines that replacement.
			if _, err := root.WriteAtomic("paper.pdf", 3, textWriter("new")); err != nil {
				t.Fatal(err)
			}
			quarantine, err := stagedSibling("paper.pdf")
			if err != nil {
				t.Fatal(err)
			}
			if err := root.Root.Rename("paper.pdf", quarantine); err != nil {
				t.Fatal(err)
			}
			if occupied {
				if _, err := root.WriteAtomic("paper.pdf", 5, textWriter("later")); err != nil {
					t.Fatal(err)
				}
			}
			removed, err := root.removeQuarantined("paper.pdf", quarantine, expected, 3, removalDigest("old"))
			if removed {
				t.Fatal("deleted replacement")
			}
			if occupied {
				if err == nil || !strings.Contains(err.Error(), quarantine) {
					t.Fatal("no recovery path", err)
				}
				data, err := root.ReadLimited(quarantine, 10)
				if err != nil || string(data) != "new" {
					t.Fatal("lost preserved replacement", string(data), err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			want := "new"
			if occupied {
				want = "later"
			}
			data, err := root.ReadLimited("paper.pdf", 10)
			if err != nil || string(data) != want {
				t.Fatal("overwrote user's file", string(data), err)
			}
		})
	}
}
