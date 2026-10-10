package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplainUploadPolicyBoundaries(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{
		"main.tex": "main", ".env.latexmk": "private",
		".latexmkignore": "blocked/\n", "blocked/child.tex": "excluded",
		"sub/.gitignore": "*.tmp\n", "sub/drop.tmp": "excluded",
	} {
		mustWrite(t, filepath.Join(root, name), body)
	}
	opts := Options{Root: root, RespectGitIgnore: true}
	for _, tc := range []struct {
		name, reason string
		allowed      bool
	}{
		{"main.tex", "allowed by upload policy", true},
		{".env.latexmk", "local configuration or credential", false},
		{"blocked/child.tex", "blocked/", false},
		{"blocked/absent.tex", "blocked/", false},
		{"sub/drop.tmp", "*.tmp", false},
		{"absent.tex", "missing", false},
		{"sub", "unsupported file type", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Explain(opts, tc.name)
			if err != nil || result.Allowed != tc.allowed || !strings.Contains(result.Reason, tc.reason) {
				t.Fatalf("explanation=%+v err=%v, want allowed=%v reason=%q", result, err, tc.allowed, tc.reason)
			}
		})
	}
	for _, name := range []string{"../main.tex", filepath.Join(root, "main.tex")} {
		if _, err := Explain(opts, name); err == nil {
			t.Errorf("accepted unconfined path %q", name)
		}
	}
}

func TestExplainRespectsTrackedFilesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	mustRun(t, root, "git", "init", "--quiet")
	mustWrite(t, filepath.Join(root, ".gitignore"), "*.tmp\n")
	mustWrite(t, filepath.Join(root, "tracked.tmp"), "tracked")
	mustWrite(t, filepath.Join(root, "untracked.tmp"), "ignored")
	mustRun(t, root, "git", "add", "--force", "tracked.tmp")
	opts := Options{Root: root, RespectGitIgnore: true}
	for name, want := range map[string]bool{"tracked.tmp": true, "untracked.tmp": false} {
		result, err := Explain(opts, name)
		if err != nil || result.Allowed != want {
			t.Fatalf("%s: explanation=%+v err=%v", name, result, err)
		}
	}
	if err := os.Symlink("tracked.tmp", filepath.Join(root, "linked.tex")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result, err := Explain(opts, "linked.tex")
	if err != nil || result.Allowed || result.Reason != "symbolic link" {
		t.Fatalf("symlink explanation=%+v err=%v", result, err)
	}
}
