package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
)

var artifactIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type resultCommandOptions struct {
	server     string
	token      string
	timeout    time.Duration
	insecure   bool
	jsonOutput bool
	jobID      string
	artifactID string
	outDir     string
	source     string
	tailLines  int
	maxBytes   int64
}

type artifactsListData struct {
	JobID     string                `json:"jobId"`
	Count     int                   `json:"count"`
	Artifacts []client.ArtifactInfo `json:"artifacts"`
}

func runArtifacts(args []string) int {
	jsonOutput := hasJSONFlag(args)
	command := "artifacts"
	if len(args) == 0 {
		return failAgentArguments(command, jsonOutput, errors.New("artifacts requires list or get"))
	}
	action := args[0]
	command += "." + action
	if action != "list" && action != "get" {
		return failAgentArguments(command, jsonOutput, fmt.Errorf("unknown artifacts action %q", action))
	}
	opts, args, err := loadResultCommandOptions(args)
	if err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	opts.jsonOutput = jsonOutput
	if err := parseResultCommandArgs("artifacts."+action, args[1:], &opts); err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	c, err := client.New(opts.server, opts.token, opts.timeout, opts.insecure)
	if err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	if action == "list" {
		artifacts, err := c.ListArtifacts(ctx, opts.jobID)
		if err != nil {
			return failAgent(command, opts.jsonOutput, err)
		}
		data := artifactsListData{JobID: opts.jobID, Count: len(artifacts), Artifacts: artifacts}
		if opts.jsonOutput {
			if err := writeAgentJSON(command, data); err != nil {
				return fail(err)
			}
			return 0
		}
		for _, artifact := range artifacts {
			fmt.Printf("%s\t%d\t%s\t%s\n", artifact.ID, artifact.Size, artifact.MIMEType, artifact.Path)
		}
		return 0
	}
	download, err := c.DownloadArtifact(ctx, opts.jobID, opts.artifactID, opts.outDir)
	if err != nil {
		return failAgent(command, opts.jsonOutput, err)
	}
	if opts.jsonOutput {
		if err := writeAgentJSON(command, download); err != nil {
			return fail(err)
		}
		return 0
	}
	fmt.Printf("downloaded: %s\nsize: %d\nSHA-256: %s\n", download.LocalPath, download.Size, download.SHA256)
	return 0
}

func runLogs(args []string) int {
	jsonOutput := hasJSONFlag(args)
	command := "logs.get"
	opts, args, err := loadResultCommandOptions(args)
	if err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	opts.jsonOutput = jsonOutput
	if err := parseResultCommandArgs("logs", args, &opts); err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	c, err := client.New(opts.server, opts.token, opts.timeout, opts.insecure)
	if err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	logs, err := c.Logs(ctx, opts.jobID, opts.source, opts.tailLines, opts.maxBytes)
	if err != nil {
		return failAgent(command, opts.jsonOutput, err)
	}
	if opts.jsonOutput {
		if err := writeAgentJSON(command, logs); err != nil {
			return fail(err)
		}
		return 0
	}
	for _, entry := range logs.Entries {
		fmt.Printf(
			"== %s: %s (%d/%d bytes) ==\n%s",
			entry.Source,
			entry.Path,
			entry.ReturnedBytes,
			entry.TotalBytes,
			entry.Content,
		)
		if entry.Content != "" && !strings.HasSuffix(entry.Content, "\n") {
			fmt.Println()
		}
	}
	return 0
}

func runDiagnostics(args []string) int {
	jsonOutput := hasJSONFlag(args)
	command := "diagnostics.get"
	opts, args, err := loadResultCommandOptions(args)
	if err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	opts.jsonOutput = jsonOutput
	if err := parseResultCommandArgs("diagnostics", args, &opts); err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	c, err := client.New(opts.server, opts.token, opts.timeout, opts.insecure)
	if err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	diagnostics, err := c.Diagnostics(ctx, opts.jobID)
	if err != nil {
		return failAgent(command, opts.jsonOutput, err)
	}
	if opts.jsonOutput {
		if err := writeAgentJSON(command, diagnostics); err != nil {
			return fail(err)
		}
		return 0
	}
	for _, diagnostic := range diagnostics.Diagnostics {
		position := diagnostic.File
		if diagnostic.Line > 0 {
			position += fmt.Sprintf(":%d", diagnostic.Line)
		}
		if position == "" {
			position = "-"
		}
		locations := make([]string, 0, len(diagnostic.LogLocations))
		for _, location := range diagnostic.LogLocations {
			locations = append(
				locations,
				fmt.Sprintf("%s:%s:%d-%d", location.Source, location.Path, location.StartLine, location.EndLine),
			)
		}
		fmt.Printf(
			"%s\t%s\t%s\t[%s]\n",
			diagnostic.Severity,
			position,
			diagnostic.Message,
			strings.Join(locations, ", "),
		)
	}
	if diagnostics.Incomplete {
		fmt.Fprintln(os.Stderr, "latexmk: diagnostic index is incomplete; inspect the raw logs")
	}
	return 0
}

func loadResultCommandOptions(args []string) (resultCommandOptions, []string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return resultCommandOptions{}, nil, err
	}
	cfg, args, err := config.LoadArgs(cwd, args)
	if err != nil {
		return resultCommandOptions{}, nil, err
	}
	if err := cfg.Authenticate(cfg.ProjectRoot); err != nil {
		return resultCommandOptions{}, nil, err
	}
	return resultCommandOptions{
		server: cfg.Server, token: cfg.Token, timeout: cfg.Timeout,
		insecure: cfg.InsecureSkipVerify,
		outDir:   cwd, source: "all", tailLines: 200, maxBytes: 64 << 10,
	}, args, nil
}

func parseResultCommandArgs(command string, args []string, opts *resultCommandOptions) error {
	positionals := make([]string, 0, 2)
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
		var err error
		switch {
		case a == "--server" || strings.HasPrefix(a, "--server="):
			opts.server, err = value("--server")
		case a == "--timeout" || strings.HasPrefix(a, "--timeout="):
			var raw string
			raw, err = value("--timeout")
			if err == nil {
				opts.timeout, err = time.ParseDuration(raw)
			}
		case a == "--insecure-skip-verify":
			opts.insecure = true
		case a == "--json":
			opts.jsonOutput = true
		case a == "--out-dir" || strings.HasPrefix(a, "--out-dir="):
			if command != "artifacts.get" {
				return errors.New("--out-dir is only valid for artifacts get")
			}
			opts.outDir, err = value("--out-dir")
		case a == "--source" || strings.HasPrefix(a, "--source="):
			if command != "logs" {
				return errors.New("--source is only valid for logs")
			}
			opts.source, err = value("--source")
		case a == "--tail" || strings.HasPrefix(a, "--tail="):
			if command != "logs" {
				return errors.New("--tail is only valid for logs")
			}
			var raw string
			raw, err = value("--tail")
			if err == nil {
				opts.tailLines, err = strconv.Atoi(raw)
			}
		case a == "--max-bytes" || strings.HasPrefix(a, "--max-bytes="):
			if command != "logs" {
				return errors.New("--max-bytes is only valid for logs")
			}
			var raw string
			raw, err = value("--max-bytes")
			if err == nil {
				opts.maxBytes, err = strconv.ParseInt(raw, 10, 64)
			}
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown option %q", a)
		default:
			positionals = append(positionals, a)
		}
		if err != nil {
			return err
		}
	}
	if opts.timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if command == "logs" || command == "diagnostics" {
		if len(positionals) != 1 || !jobIDPattern.MatchString(positionals[0]) {
			return fmt.Errorf("%s requires one valid job ID", command)
		}
		if command == "logs" {
			if opts.source != "all" && opts.source != "stdout" && opts.source != "stderr" && opts.source != "compiler" {
				return errors.New("--source must be all, stdout, stderr, or compiler")
			}
			if opts.tailLines < 1 || opts.tailLines > 10_000 {
				return errors.New("--tail must be between 1 and 10000")
			}
			if opts.maxBytes < 1 || opts.maxBytes > 4<<20 {
				return errors.New("--max-bytes must be between 1 and 4194304")
			}
		}
		opts.jobID = positionals[0]
		return nil
	}
	want := 1
	if command == "artifacts.get" {
		want = 2
	}
	if len(positionals) != want || !jobIDPattern.MatchString(positionals[0]) {
		return fmt.Errorf("%s requires a valid job ID", command)
	}
	opts.jobID = positionals[0]
	if command == "artifacts.get" {
		if !artifactIDPattern.MatchString(positionals[1]) {
			return errors.New("artifacts get requires the 32-character artifact ID returned by artifacts list")
		}
		opts.artifactID = positionals[1]
	}
	return nil
}
