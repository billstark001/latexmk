package safefs

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestPortableOutputNames(t *testing.T) {
	for _, name := range []string{"paper-2026-10-22.pdf", "output/论文 draft.pdf", "output/{date}.zip", "../release/source.zip", "/tmp/source.zip"} {
		if err := ValidateOutputPath(name); err != nil {
			t.Errorf("rejected %q: %v", name, err)
		}
	}
	for _, name := range []string{"", "folder?/paper.pdf", "a*.zip", "a|b.pdf", "a<b>.pdf", "a\"b.pdf",
		"date-12:34.pdf", "bad\nname.pdf", "bad\x7fname.pdf", "folder./paper.pdf", "folder /paper.pdf",
		"CON.pdf", "aux.zip", "NUL.tar.gz", "com1/output.zip", "lpt¹.pdf", "COM².zip", "con .pdf",
		strings.Repeat("a", 256) + "/paper.pdf", string([]byte{255}) + ".pdf"} {
		if err := ValidateOutputPath(name); err == nil {
			t.Errorf("accepted invalid output %q", name)
		}
	}
	for _, fragment := range []string{"../outside", "2026/10", `folder\file`, "12:34", "x\x00y"} {
		if err := ValidateNamePart(fragment); err == nil {
			t.Errorf("accepted fragment %q", fragment)
		}
	}
	if err := ValidateNamePart("part. "); err != nil {
		t.Fatal("fragment restrictions applied before composition", err)
	}
}

func TestCheckDestinationDoesNotCreateAndRejectsNonRegular(t *testing.T) {
	root := testRoot(t)
	if err := root.CheckDestination("new/paper.pdf"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat("new"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created destination parent", err)
	}
	if err := root.MakeDirs("directory.pdf", 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.CheckDestination("directory.pdf"); err == nil {
		t.Fatal("accepted directory destination")
	}
	if err := root.Symlink("new/paper.pdf", "link.pdf"); err != nil {
		t.Skip(err)
	}
	if err := root.CheckDestination("link.pdf"); !errors.Is(err, ErrSymlink) {
		t.Fatal("accepted symlink", err)
	}
}
