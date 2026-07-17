package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
)

var (
	version   = "0.1.0-dev"
	commit    = "unknown"
	buildDate = "unknown"
)

type compileOptions struct {
	server        string
	token         string
	projectRoot   string
	rootMode      string
	gitIgnore     bool
	engine        string
	outDir        string
	timeout       time.Duration
	interaction   string
	synctex       bool
	haltOnError   bool
	fileLineError bool
	shellEscape   bool
	jobName       string
	force         bool
	quiet         bool
	jsonOutput    bool
	dryRun        bool
	insecure      bool
	entry         string
	exclude       []string
	configPath    string
}

func main() {
	os.Exit(run(os.Args))
}

func run(args []string) int {
	if len(args) == 0 {
		return 2
	}
	invokedAs := strings.ToLower(filepath.Base(args[0]))
	argv := args[1:]
	if len(argv) > 0 {
		switch argv[0] {
		case "version", "--version", "-version":
			fmt.Printf("latexmk %s\ncommit: %s\nbuilt: %s\n", version, commit, buildDate)
			return 0
		case "help", "--help", "-h":
			usage()
			return 0
		case "init":
			return runInit(argv[1:])
		case "meta":
			return runMeta(argv[1:], false)
		case "doctor":
			return runMeta(argv[1:], true)
		case "clean":
			return runClean(argv[1:])
		case "files":
			return runCompile(argv[1:], "", true)
		case "compile":
			argv = argv[1:]
		}
	}

	forcedEngine := ""
	switch invokedAs {
	case "xelatex", "xelatex.exe":
		forcedEngine = "xelatex"
	case "lualatex", "lualatex.exe":
		forcedEngine = "lualatex"
	case "pdflatex", "pdflatex.exe":
		forcedEngine = "pdflatex"
	}
	return runCompile(argv, forcedEngine, false)
}

func runCompile(args []string, forcedEngine string, listOnly bool) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		return fail(err)
	}
	opts := compileOptions{
		server:        cfg.Server,
		token:         cfg.Token,
		projectRoot:   cfg.ProjectRoot,
		rootMode:      cfg.RootMode,
		gitIgnore:     cfg.RespectGitIgnore,
		engine:        cfg.Engine,
		outDir:        "",
		timeout:       cfg.Timeout,
		interaction:   "nonstopmode",
		synctex:       true,
		haltOnError:   true,
		fileLineError: true,
		insecure:      cfg.InsecureSkipVerify,
		exclude:       cfg.Exclude,
		configPath:    cfg.ConfigPath,
		dryRun:        listOnly,
	}
	if forcedEngine != "" {
		opts.engine = forcedEngine
	}
	if err := parseCompileArgs(args, &opts); err != nil {
		fmt.Fprintln(os.Stderr, "latexmk:", err)
		fmt.Fprintln(os.Stderr, "run 'latexmk help' for usage")
		return 2
	}
	if opts.entry == "" {
		return fail(errors.New("no TeX entry file was provided"))
	}
	if err := normalizeCompilePaths(&opts, cwd); err != nil {
		return fail(err)
	}
	if opts.dryRun {
		return printManifest(opts)
	}

	c, err := client.New(opts.server, opts.token, opts.timeout, opts.insecure)
	if err != nil {
		return fail(err)
	}
	c.ProjectRoot = opts.projectRoot
	c.Exclude = opts.exclude
	c.RespectGitIgnore = opts.gitIgnore
	request := protocol.CompileRequest{
		ProtocolVersion: protocol.Version,
		Entry:           opts.entry,
		Engine:          opts.engine,
		Interaction:     opts.interaction,
		Synctex:         opts.synctex,
		HaltOnError:     opts.haltOnError,
		FileLineError:   opts.fileLineError,
		ShellEscape:     opts.shellEscape,
		JobName:         opts.jobName,
		Force:           opts.force,
		Quiet:           opts.quiet,
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	out, err := c.Compile(ctx, request, opts.outDir)
	if err != nil {
		return fail(err)
	}
	if opts.jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(out.Result)
	} else {
		if !opts.quiet && len(out.Stdout) > 0 {
			_, _ = os.Stdout.Write(out.Stdout)
		}
		if len(out.Stderr) > 0 {
			_, _ = os.Stderr.Write(out.Stderr)
		}
		fmt.Fprintf(os.Stderr, "latexmk: request=%s server=%s profile=%s engine=%s duration=%dms artifacts=%d\n", out.Result.RequestID, out.Result.ServerVersion, out.Result.ImageProfile, out.Result.Engine, out.Result.DurationMS, len(out.Result.Artifacts))
		if out.Result.StdoutTruncated || out.Result.StderrTruncated {
			fmt.Fprintln(os.Stderr, "latexmk: warning: server truncated compiler output")
		}
	}
	if !out.Result.Success {
		if out.Result.Error != "" {
			fmt.Fprintln(os.Stderr, "latexmk:", out.Result.Error)
		}
		if out.Result.TimedOut {
			return 124
		}
		if out.Result.ExitCode > 0 && out.Result.ExitCode < 126 {
			return out.Result.ExitCode
		}
		return 1
	}
	return 0
}

func parseCompileArgs(args []string, opts *compileOptions) error {
	positionals := make([]string, 0, 1)
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func(name string) (string, error) {
			if strings.Contains(a, "=") {
				return strings.SplitN(a, "=", 2)[1], nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", name)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--":
			positionals = append(positionals, args[i+1:]...)
			i = len(args)
		case a == "--server" || strings.HasPrefix(a, "--server="):
			v, err := value("--server")
			if err != nil {
				return err
			}
			opts.server = v
		case a == "--token" || strings.HasPrefix(a, "--token="):
			v, err := value("--token")
			if err != nil {
				return err
			}
			opts.token = v
		case a == "--project-root" || strings.HasPrefix(a, "--project-root="):
			v, err := value("--project-root")
			if err != nil {
				return err
			}
			opts.projectRoot = v
		case a == "--root-mode" || strings.HasPrefix(a, "--root-mode="):
			v, err := value("--root-mode")
			if err != nil {
				return err
			}
			if v != "entry" && v != "git" {
				return fmt.Errorf("--root-mode must be entry or git, got %q", v)
			}
			opts.rootMode = v
		case a == "--gitignore":
			opts.gitIgnore = true
		case a == "--no-gitignore":
			opts.gitIgnore = false
		case a == "--out-dir" || strings.HasPrefix(a, "--out-dir=") || a == "-output-directory" || strings.HasPrefix(a, "-output-directory="):
			v, err := value("--out-dir")
			if err != nil {
				return err
			}
			opts.outDir = v
		case a == "--engine" || strings.HasPrefix(a, "--engine="):
			v, err := value("--engine")
			if err != nil {
				return err
			}
			opts.engine = v
		case a == "--timeout" || strings.HasPrefix(a, "--timeout="):
			v, err := value("--timeout")
			if err != nil {
				return err
			}
			d, err := time.ParseDuration(v)
			if err != nil {
				return err
			}
			opts.timeout = d
		case a == "--interaction" || strings.HasPrefix(a, "--interaction=") || strings.HasPrefix(a, "-interaction="):
			v, err := value("interaction")
			if err != nil {
				return err
			}
			opts.interaction = v
		case a == "--jobname" || strings.HasPrefix(a, "--jobname=") || strings.HasPrefix(a, "-jobname="):
			v, err := value("jobname")
			if err != nil {
				return err
			}
			opts.jobName = v
		case a == "--synctex" || a == "-synctex=1":
			opts.synctex = true
		case a == "--no-synctex" || a == "-synctex=0":
			opts.synctex = false
		case a == "--shell-escape" || a == "-shell-escape":
			opts.shellEscape = true
		case a == "--no-shell-escape" || a == "-no-shell-escape":
			opts.shellEscape = false
		case a == "--halt-on-error" || a == "-halt-on-error":
			opts.haltOnError = true
		case a == "--no-halt-on-error":
			opts.haltOnError = false
		case a == "--file-line-error" || a == "-file-line-error":
			opts.fileLineError = true
		case a == "--no-file-line-error":
			opts.fileLineError = false
		case a == "-xelatex" || a == "-pdfxe":
			opts.engine = "xelatex"
		case a == "-lualatex" || a == "-pdflua":
			opts.engine = "lualatex"
		case a == "-pdf" || a == "-pdflatex":
			opts.engine = "pdflatex"
		case a == "-g" || a == "-gg" || a == "--force":
			opts.force = true
		case a == "-quiet" || a == "-silent" || a == "--quiet":
			opts.quiet = true
		case a == "--json":
			opts.jsonOutput = true
		case a == "--dry-run":
			opts.dryRun = true
		case a == "--insecure-skip-verify":
			opts.insecure = true
		case a == "--version":
			return errors.New("--version must be used without a compile target")
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unsupported option %q; use structured options instead of arbitrary TeX flags", a)
		default:
			positionals = append(positionals, a)
		}
	}
	if len(positionals) > 1 {
		return fmt.Errorf("only one entry file is supported, got %d", len(positionals))
	}
	if len(positionals) == 1 {
		opts.entry = positionals[0]
	}
	if opts.timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	return nil
}

func normalizeCompilePaths(opts *compileOptions, cwd string) error {
	entryAbs := opts.entry
	if !filepath.IsAbs(entryAbs) {
		entryAbs = filepath.Join(cwd, entryAbs)
	}
	entryAbs, err := filepath.Abs(entryAbs)
	if err != nil {
		return err
	}
	entryAbs, err = filepath.EvalSymlinks(entryAbs)
	if err != nil {
		return fmt.Errorf("entry file: %w", err)
	}
	st, err := os.Stat(entryAbs)
	if err != nil {
		return fmt.Errorf("entry file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return errors.New("entry is not a regular file")
	}

	root := opts.projectRoot
	if root == "" {
		switch opts.rootMode {
		case "", "entry":
			root = filepath.Dir(entryAbs)
		case "git":
			root, err = config.FindGitRoot(filepath.Dir(entryAbs))
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("invalid root mode %q", opts.rootMode)
		}
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("project root: %w", err)
	}
	rel, err := filepath.Rel(root, entryAbs)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("entry %s is outside project root %s", entryAbs, root)
	}
	opts.projectRoot = root
	opts.entry = filepath.ToSlash(rel)
	if opts.outDir == "" {
		opts.outDir = root
	}
	if !filepath.IsAbs(opts.outDir) {
		opts.outDir = filepath.Join(cwd, opts.outDir)
	}
	opts.outDir, err = filepath.Abs(opts.outDir)
	return err
}

type manifestView struct {
	ProjectRoot string                `json:"projectRoot"`
	Entry       string                `json:"entry"`
	Files       []projectarchive.File `json:"files"`
	Stats       projectarchive.Stats  `json:"stats"`
}

func printManifest(opts compileOptions) int {
	files, stats, err := projectarchive.Manifest(projectarchive.Options{
		Root: opts.projectRoot, Exclude: opts.exclude, RespectGitIgnore: opts.gitIgnore, MaxFiles: 20_000, MaxBytes: 2 << 30,
	})
	if err != nil {
		return fail(fmt.Errorf("build project manifest: %w", err))
	}
	if opts.jsonOutput {
		view := manifestView{ProjectRoot: opts.projectRoot, Entry: opts.entry, Files: files, Stats: stats}
		if err := json.NewEncoder(os.Stdout).Encode(view); err != nil {
			return fail(err)
		}
		return 0
	}
	fmt.Printf("project root: %s\nentry: %s\nfiles: %d\nbytes: %d\n", opts.projectRoot, opts.entry, stats.Files, stats.Bytes)
	for _, file := range files {
		fmt.Printf("%10d  %s  %s\n", file.Size, file.SHA256, file.Path)
	}
	return 0
}

func runMeta(args []string, doctor bool) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		return fail(err)
	}
	server, token, timeout, insecure, jsonOutput := cfg.Server, cfg.Token, cfg.Timeout, cfg.InsecureSkipVerify, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if strings.Contains(a, "=") {
				return strings.SplitN(a, "=", 2)[1], nil
			}
			if i+1 >= len(args) {
				return "", errors.New("missing option value")
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--server" || strings.HasPrefix(a, "--server="):
			v, e := next()
			if e != nil {
				return fail(e)
			}
			server = v
		case a == "--token" || strings.HasPrefix(a, "--token="):
			v, e := next()
			if e != nil {
				return fail(e)
			}
			token = v
		case a == "--timeout" || strings.HasPrefix(a, "--timeout="):
			v, e := next()
			if e != nil {
				return fail(e)
			}
			timeout, e = time.ParseDuration(v)
			if e != nil {
				return fail(e)
			}
		case a == "--insecure-skip-verify":
			insecure = true
		case a == "--json":
			jsonOutput = true
		default:
			return fail(fmt.Errorf("unknown option %q", a))
		}
	}
	c, err := client.New(server, token, timeout, insecure)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if doctor {
		if err := c.Health(ctx); err != nil {
			return fail(fmt.Errorf("health check failed: %w", err))
		}
	}
	meta, err := c.Metadata(ctx)
	if err != nil {
		return fail(err)
	}
	if jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(meta)
		return 0
	}
	fmt.Printf("service: %s %s\nprotocol: %d\nprofile: %s\nauth: %s\ndatabase: %s\nengines: %s\n", meta.Service, meta.Version, meta.ProtocolVersion, meta.ImageProfile, meta.AuthMode, meta.Database, strings.Join(meta.Capabilities.Engines, ", "))
	for _, name := range []string{"latexmk", "xelatex", "lualatex", "pdflatex", "biber"} {
		if v := meta.Toolchain[name]; v != "" {
			fmt.Printf("%s: %s\n", name, v)
		}
	}
	if doctor {
		fmt.Println("status: healthy")
	}
	return 0
}

func runInit(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	path := filepath.Join(cwd, config.FileName)
	server := "http://127.0.0.1:8080"
	for i := 0; i < len(args); i++ {
		if args[i] == "--server" && i+1 < len(args) {
			i++
			server = args[i]
		} else if strings.HasPrefix(args[i], "--server=") {
			server = strings.SplitN(args[i], "=", 2)[1]
		} else {
			return fail(fmt.Errorf("unknown option %q", args[i]))
		}
	}
	if _, err := os.Stat(path); err == nil {
		return fail(fmt.Errorf("%s already exists", path))
	}
	if err := config.Write(path, config.FileConfig{Server: server, Engine: "xelatex", Timeout: "3m"}); err != nil {
		return fail(err)
	}
	fmt.Println(path)
	return 0
}

func runClean(args []string) int {
	entry := "main.tex"
	if len(args) > 1 {
		return fail(errors.New("clean accepts at most one entry file"))
	}
	if len(args) == 1 {
		entry = args[0]
	}
	stem := strings.TrimSuffix(filepath.Base(entry), filepath.Ext(entry))
	dir := filepath.Dir(entry)
	if dir == "." {
		dir = "."
	}
	extensions := []string{".aux", ".bbl", ".bcf", ".blg", ".fdb_latexmk", ".fls", ".log", ".out", ".run.xml", ".synctex.gz", ".toc", ".xdv"}
	removed := 0
	for _, ext := range extensions {
		p := filepath.Join(dir, stem+ext)
		if err := os.Remove(p); err == nil {
			removed++
		} else if !os.IsNotExist(err) {
			return fail(err)
		}
	}
	fmt.Printf("removed %d generated files\n", removed)
	return 0
}

func usage() {
	fmt.Print(`latexmk - remote, PaaS-hosted LaTeX compiler

Usage:
  latexmk compile [options] <main.tex>
  latexmk [latex-compatible-options] <main.tex>
  latexmk meta [--json]
  latexmk doctor
  latexmk init [--server URL]
  latexmk clean [main.tex]
  latexmk files [options] <main.tex>
  latexmk version

Compile options:
  --server URL                 Remote server URL
  --token TOKEN                Bearer token (prefer LATEXMK_TOKEN)
  --project-root DIR           Root directory uploaded to the server
  --root-mode entry|git        Default root when --project-root is absent
  --gitignore                  Respect Git ignore rules (default)
  --no-gitignore               Include Git-ignored files unless otherwise excluded
  --out-dir DIR                Local root for returned artifacts
  --engine xelatex|lualatex|pdflatex
  --timeout 3m                 End-to-end request timeout
  --shell-escape               Request shell escape; server policy may reject it
  --jobname NAME               TeX job name
  --no-synctex                 Disable SyncTeX
  --json                       Print machine-readable result
  --dry-run                    Print the upload manifest without contacting the server

The executable may be symlinked as xelatex, lualatex, or pdflatex.
Configuration is read from .latexmk.json and environment variables.
`)
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "latexmk:", err)
	return 2
}
