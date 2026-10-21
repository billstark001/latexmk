package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	"github.com/billstark001/latexmk/packages/cli/internal/naming"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func namingGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is unavailable")
	}
	flags := []string{"-C", root, "-c", "user.name=Naming Test", "-c", "user.email=naming@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}
	cmd := exec.Command("git", append(flags, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestInvocationAndBuildScopeCaptureGitAndTime(t *testing.T) {
	root := t.TempDir()
	namingGit(t, root, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	namingGit(t, root, "add", "main.tex")
	namingGit(t, root, "commit", "--quiet", "-m", "one")
	first := namingGit(t, root, "rev-parse", "HEAD")
	template, err := naming.Parse("{date}-{git.commit:8}-{entry.stem}-{target}-{engine}")
	if err != nil {
		t.Fatal(err)
	}
	invocation := newNameInvocation()
	invocation.at = time.Date(2026, 10, 21, 23, 30, 0, 0, time.UTC)
	invocation.now = func() time.Time { return invocation.at.Add(48 * time.Hour) }
	opts := compileOptions{projectRoot: root, entry: "main.tex", target: "paper", engine: "xelatex",
		invocation: invocation, nameTimeZone: "Asia/Tokyo"}
	before, err := renderName(context.Background(), template, opts)
	if err != nil || before != "2026-10-22-"+first[:8]+"-main-paper-xelatex" {
		t.Fatal(before, err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	namingGit(t, root, "commit", "--quiet", "-am", "two")
	second := namingGit(t, root, "rev-parse", "HEAD")
	after, err := renderName(context.Background(), template, opts)
	if err != nil || before != after {
		t.Fatalf("invocation context changed: %q %v", after, err)
	}
	opts.nameScope = "build"
	after, err = renderName(context.Background(), template, opts)
	if err != nil || after != "2026-10-24-"+second[:8]+"-main-paper-xelatex" {
		t.Fatal(after, err)
	}
	worktree := filepath.Join(t.TempDir(), "checkout")
	namingGit(t, root, "worktree", "add", "--quiet", "--detach", worktree, "HEAD")
	opts.projectRoot = worktree
	if value, err := renderName(context.Background(), template, opts); err != nil || value != after {
		t.Fatalf("worktree/detached HEAD: %q %v", value, err)
	}
}

func TestWatchNamingScopesFreezeOrRefreshBetweenBuilds(t *testing.T) {
	for _, scope := range []string{"", "invocation", "build"} {
		t.Run("scope="+scope, func(t *testing.T) {
			invocation := newNameInvocation()
			invocation.at = time.Date(2026, 10, 22, 0, 0, 1, 0, time.UTC)
			clock := invocation.at
			invocation.now = func() time.Time { return clock }
			opts := compileOptions{projectRoot: t.TempDir(), entry: "main.tex", invocation: invocation,
				pdfTemplate: "{datetime:go:150405}.pdf", nameScope: scope, nameTimeZone: "UTC"}
			if err := preparePDFNaming(&opts); err != nil {
				t.Fatal(err)
			}
			first, err := prepareBuildNaming(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseBuildNaming(first)
			clock = clock.Add(time.Second)
			second, err := prepareBuildNaming(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseBuildNaming(second)
			want := "000001.pdf"
			if scope == "build" {
				want = "000002.pdf"
			}
			if first.pdfExport != "000001.pdf" || second.pdfExport != want {
				t.Fatalf("build names: first=%q second=%q want second=%q", first.pdfExport, second.pdfExport, want)
			}
		})
	}
}

func TestOutputTemplatesRejectIllegalNamesBeforeSideEffects(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.WriteFile("main.tex", []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{
		"{datetime:iso8601}", "bad?/file", "NUL", "bad./file",
		"{datetime:go:2006/01}", "control\nfile", strings.Repeat("x", 256),
	} {
		for _, command := range []string{"compile", "pack"} {
			suffix, flag := ".pdf", "--pdf-template"
			if command == "pack" {
				suffix, flag = ".zip", "--output-template"
			}
			code, stdout, stderr := captureCommandOutput(t, func() int {
				return run([]string{"latexmk", command, "main.tex", "--dry-run", "--json", flag, source + suffix})
			})
			if code == 0 {
				t.Fatalf("accepted invalid %s destination %q: %s %s", command, source, stdout, stderr)
			}
		}
	}
	if _, err := os.Stat(".latexmk-cache"); !os.IsNotExist(err) {
		t.Fatal("invalid output created history", err)
	}
	if _, err := os.Stat("bad?"); !os.IsNotExist(err) {
		t.Fatal("invalid pack output created directories", err)
	}
	code, stdout, stderr := captureCommandOutput(t, func() int {
		return run([]string{"latexmk", "name", "render", "main.tex", "--template", "{datetime:iso8601}", "--json"})
	})
	var response struct{ Data struct{ Resolved string } }
	if err := json.Unmarshal(
		[]byte(stdout),
		&response,
	); err != nil || code != 0 ||
		!strings.Contains(response.Data.Resolved, ":") {
		t.Fatalf("plain rendering was sanitized: %d %s %s %v", code, stdout, stderr, err)
	}
}

func TestBuildNameReservationsAreReleasedAndHistoryIsBounded(t *testing.T) {
	invocation := newNameInvocation()
	var clock atomic.Int64
	invocation.now = func() time.Time { return time.Unix(clock.Add(1), 0).UTC() }
	opts := compileOptions{projectRoot: t.TempDir(), entry: "main.tex", invocation: invocation, nameScope: "build",
		pdfTemplate: "{datetime:go:150405}.pdf", nameTimeZone: "UTC"}
	if err := preparePDFNaming(&opts); err != nil {
		t.Fatal(err)
	}
	defer opts.naming.previewLease.Close()
	for range 100 {
		build, err := prepareBuildNaming(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		releaseBuildNaming(build)
		releaseBuildNaming(build) // Option copies share an idempotent release.
	}
	if got := len(opts.generated.snapshot()); got != 1 {
		t.Fatalf("leaked build reservations: %d", got)
	}
	if len(opts.generated.paths) != 0 {
		t.Fatal("unpublished candidates entered permanent history")
	}
}

func TestInvalidOutputNeverContactsTheBuildServer(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.WriteFile("main.tex", []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "must not contact server", http.StatusInternalServerError)
	}))
	defer server.Close()
	for _, args := range [][]string{
		{"latexmk", "compile", "main.tex", "--pdf-template", "{datetime:iso8601}.pdf"},
		{"latexmk", "pack", "main.tex", "--mode=arxiv", "--output-template", "bad?/source.zip"},
	} {
		args = append(args, "--server", server.URL)
		code, _, _ := captureCommandOutput(t, func() int { return run(args) })
		if code == 0 || requests.Load() != 0 {
			t.Fatalf("invalid output reached server: code=%d requests=%d", code, requests.Load())
		}
	}
}

func TestTemplatePackPreservesPreviousZIPWhenHistoryIsUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix permission enforcement")
	}
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.WriteFile("main.tex", []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"latexmk", "pack", "main.tex", "--output-template", "bundle.zip", "--json"}
	code, stdout, stderr := captureCommandOutput(t, func() int { return run(args) })
	if code != 0 {
		t.Fatalf("initial pack: %d %s %s", code, stdout, stderr)
	}
	before, err := os.ReadFile("bundle.zip")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".latexmk-cache", "exports")
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if err := os.WriteFile("main.tex", []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = captureCommandOutput(t, func() int { return run(args) })
	if code == 0 {
		t.Fatalf("pack ignored unwritable history: %s %s", stdout, stderr)
	}
	after, err := os.ReadFile("bundle.zip")
	if err != nil || string(after) != string(before) {
		t.Fatal("failed bookkeeping replaced previous ZIP", err)
	}
}

func TestWatcherRefreshesAnotherInvocationExportHistory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := compileOptions{projectRoot: root, entry: "main.tex", uploadMode: "all"}
	if err := prepareGeneratedPaths(&opts); err != nil {
		t.Fatal(err)
	}
	watcher := selectionClient(opts)
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(output, "main.pdf"), []byte("pdf"), 0600); err != nil {
		t.Fatal(err)
	}
	other := compileOptions{projectRoot: root, entry: "main.tex", outDir: output, pdfTemplate: "other.pdf"}
	if err := preparePDFNaming(&other); err != nil {
		t.Fatal(err)
	}
	defer other.naming.previewLease.Close()
	digest := sha256.Sum256([]byte("pdf"))
	if err := exportPDF(
		other,
		client.CompileOutput{
			Result: protocol.CompileResult{
				Success:   true,
				Artifacts: []protocol.Artifact{{Path: "main.pdf", Size: 3, SHA256: hex.EncodeToString(digest[:])}},
			},
		},
	); err != nil {
		t.Fatal(err)
	}
	files, _, err := watcher.Manifest("main.tex", "xelatex")
	if err != nil || len(files) != 1 || files[0].Path != "main.tex" {
		t.Fatal("watcher selected another invocation's export", files, err)
	}
}

func TestNameRenderIsOfflineAndDoesNotCreateState(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.WriteFile("main.tex", []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		".latexmk.json",
		[]byte(`{"token":{"file":"missing"},"targets":{"paper":{"entry":"main.tex","engine":"pdflatex"}}}`),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureCommandOutput(t, func() int {
		return run(
			[]string{"latexmk", "name", "render", "--template", "{{{target}}}-{entry.stem}-{engine}.pdf", "--json"},
		)
	})
	var response struct {
		OK      bool
		Command string
		Data    struct{ Template, Resolved string }
	}
	if err := json.Unmarshal([]byte(stdout), &response); err != nil || code != 0 || !response.OK ||
		response.Command != "name.render" || response.Data.Resolved != "{paper}-main-pdflatex.pdf" {
		t.Fatalf("preview: %d %s %s %v", code, stdout, stderr, err)
	}
	if _, err := os.Stat(".latexmk-cache"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview created state")
	}
	for _, source := range []string{"{git.commit:8}", "{unknown}", "{datetime:bad}", "{"} {
		code, stdout, _ := captureCommandOutput(
			t,
			func() int { return run([]string{"latexmk", "name", "render", "--template", source, "--json"}) },
		)
		if code != 2 || !strings.Contains(stdout, `"code":"invalid_arguments"`) {
			t.Fatalf("error: %d %s", code, stdout)
		}
	}
}

func TestPDFNamingPrecedenceAndValidation(t *testing.T) {
	count := 2
	cfg := config.Resolved{
		Timeout:      time.Second,
		PDFTemplate:  "global.pdf",
		PDFRetention: &config.PDFRetention{KeepLast: &count},
		Targets: map[string]config.Target{
			"paper": {
				Entry:        "main.tex",
				PDF:          "literal-{date}.pdf",
				PDFRetention: &config.PDFRetention{KeepLast: &count},
			},
		},
	}
	opts := optionsFromConfig(cfg, false)
	opts.target = "paper"
	if err := parseCompileArgs([]string{"--pdf-template=cli-{date}.pdf", "--pdf-keep-last=all"}, &opts); err != nil {
		t.Fatal(err)
	}
	if err := applyBuildTarget(
		&opts,
		cfg,
		[]string{"--pdf-template=cli-{date}.pdf", "--pdf-keep-last=all"},
	); err != nil {
		t.Fatal(err)
	}
	if opts.pdfTemplate != "cli-{date}.pdf" || opts.pdfExport != "" || opts.pdfKeepLast != nil {
		t.Fatalf("CLI precedence lost: %+v", opts)
	}
	for _, args := range [][]string{{"--name-scope=wrong"}, {"--name-timezone=Unknown/Nowhere"}, {"--pdf-keep-last=0"}, {"--pdf-keep-last=-2"}, {"--pdf-template="}} {
		candidate := compileOptions{timeout: time.Second}
		if err := parseCompileArgs(args, &candidate); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	root := t.TempDir()
	for _, source := range []string{"../escape.pdf", "output.txt", "{missing}.pdf", "{datetime:iso8601}.pdf"} {
		candidate := compileOptions{projectRoot: root, entry: "main.tex", pdfTemplate: source}
		if err := preparePDFNaming(&candidate); err == nil {
			t.Fatalf("accepted invalid export %q", source)
		}
	}
}

func TestSuccessfulPDFExportsTrackExcludeAndRetainAcrossInvocations(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tex"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(output, "main.pdf"), []byte("pdf"), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("pdf"))
	result := client.CompileOutput{
		Result: protocol.CompileResult{
			Success:   true,
			Artifacts: []protocol.Artifact{{Path: "main.pdf", Size: 3, SHA256: hex.EncodeToString(digest[:])}},
		},
	}
	keep := 1
	for i, day := range []int{21, 22} {
		invocation := newNameInvocation()
		invocation.at = time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC)
		opts := compileOptions{
			projectRoot:  root,
			entry:        "main.tex",
			engine:       "xelatex",
			outDir:       output,
			pdfTemplate:  "{entry.stem}-{date}.pdf",
			invocation:   invocation,
			pdfKeepLast:  &keep,
			uploadMode:   "all",
			nameTimeZone: "UTC",
		}
		if err := preparePDFNaming(&opts); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			files, _, err := selectionClient(opts).Manifest("main.tex", "xelatex")
			if err != nil || len(files) != 1 || files[0].Path != "main.tex" {
				t.Fatalf("previous export selected: %v %v", files, err)
			}
			failed := result
			failed.Result.Success = false
			if err := exportPDF(opts, failed); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, "main-2026-10-21.pdf")); err != nil {
				t.Fatal("failed build removed last good PDF", err)
			}
		}
		if err := exportPDF(opts, result); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "main-2026-10-21.pdf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retention did not remove old export", err)
	}
	if _, err := os.Stat(filepath.Join(root, "main-2026-10-22.pdf")); err != nil {
		t.Fatal(err)
	}
}

func TestPackTemplatesPreserveLiteralOutputAndExcludeHistory(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.WriteFile("main.tex", []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"one-{entry.stem}.zip", "two-{entry.stem}.zip"} {
		code, stdout, stderr := captureCommandOutput(t, func() int {
			return run(
				[]string{"latexmk", "pack", "main.tex", "--upload-mode=all", "--output-template", source, "--json"},
			)
		})
		if code != 0 {
			t.Fatalf("pack: %d %s %s", code, stdout, stderr)
		}
	}
	files := readPackZIP(t, "two-main.zip")
	if len(files) != 1 || files["main.tex"] != "source" {
		t.Fatalf("included previous exports: %v", files)
	}
	code, stdout, stderr := captureCommandOutput(t, func() int {
		return run([]string{"latexmk", "pack", "main.tex", "--output", "literal-{date}.zip", "--json"})
	})
	if code != 0 {
		t.Fatalf("literal: %d %s %s", code, stdout, stderr)
	}
	if _, err := os.Stat("literal-{date}.zip"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parsePackArgs([]string{"--output=a.zip", "--output-template=b.zip"}); err == nil {
		t.Fatal("accepted conflicting outputs")
	}
}
