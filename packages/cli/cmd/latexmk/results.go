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

	"github.com/urfave/cli/v3"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/shared/argv"
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
	command := "artifacts"
	opts, parsed, err := loadResultCommandOptions(command, args)
	positionals := []string(nil)
	if parsed != nil {
		positionals = parsed.Args().Slice()
	}
	if len(positionals) > 0 {
		command += "." + positionals[0]
	}
	if err != nil {
		return failAgentArguments(command, opts.jsonOutput, err)
	}
	action := positionals[0]
	c, err := client.New(opts.server, opts.token, opts.timeout, opts.insecure)
	if err != nil {
		return failAgentArguments(command, opts.jsonOutput, err)
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
			terminalPrintf("%s\t%d\t%s\t%s\n", artifact.ID, artifact.Size, artifact.MIMEType, artifact.Path)
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
	terminalPrintf("downloaded: %s\nsize: %d\nSHA-256: %s\n", download.LocalPath, download.Size, download.SHA256)
	return 0
}

func runLogs(args []string) int {
	command := "logs.get"
	opts, _, err := loadResultCommandOptions("logs", args)
	if err != nil {
		return failAgentArguments(command, opts.jsonOutput, err)
	}
	c, err := client.New(opts.server, opts.token, opts.timeout, opts.insecure)
	if err != nil {
		return failAgentArguments(command, opts.jsonOutput, err)
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
		terminalPrintf(
			"== %s: %s (%d/%d bytes) ==\n%s",
			entry.Source,
			entry.Path,
			entry.ReturnedBytes,
			entry.TotalBytes,
			entry.Content,
		)
		if entry.Content != "" && !strings.HasSuffix(entry.Content, "\n") {
			terminalPrintln()
		}
	}
	return 0
}

func runDiagnostics(args []string) int {
	command := "diagnostics.get"
	opts, _, err := loadResultCommandOptions("diagnostics", args)
	if err != nil {
		return failAgentArguments(command, opts.jsonOutput, err)
	}
	c, err := client.New(opts.server, opts.token, opts.timeout, opts.insecure)
	if err != nil {
		return failAgentArguments(command, opts.jsonOutput, err)
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
		terminalPrintf(
			"%s\t%s\t%s\t[%s]\n",
			diagnostic.Severity,
			position,
			diagnostic.Message,
			strings.Join(locations, ", "),
		)
	}
	if diagnostics.Incomplete {
		terminalFprintln(os.Stderr, "latexmk: diagnostic index is incomplete; inspect the raw logs")
	}
	return 0
}

func loadResultCommandOptions(command string, args []string) (resultCommandOptions, *cli.Command, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return resultCommandOptions{}, nil, err
	}
	changes := resultArguments()
	cfg, parsed, err := loadCommand(command, cwd, args, changes.Flags()...)
	opts := resultCommandOptions{jsonOutput: parsed != nil && parsed.Bool("json")}
	if err != nil {
		return opts, parsed, err
	}
	opts.server, opts.token, opts.timeout = cfg.Server, cfg.Token, cfg.Timeout
	opts.insecure = cfg.InsecureSkipVerify
	opts.outDir, opts.source, opts.tailLines, opts.maxBytes = cwd, "all", 200, 64<<10
	if err := changes.Apply(&opts); err != nil {
		return opts, parsed, err
	}
	positionals := parsed.Args().Slice()
	if command == "artifacts" {
		if len(positionals) == 0 {
			return opts, parsed, errors.New("artifacts requires list or get")
		}
		action := positionals[0]
		if action != "list" && action != "get" {
			return opts, parsed, fmt.Errorf("unknown artifacts action %q", action)
		}
		command += "." + action
		positionals = positionals[1:]
	}
	if err := validateResultArguments(command, positionals, parsed, &opts); err != nil {
		return opts, parsed, err
	}
	if err := cfg.Authenticate(cfg.ProjectRoot); err != nil {
		return opts, parsed, err
	}
	opts.token = cfg.Token
	return opts, parsed, nil
}

func resultArguments() *argv.Options[resultCommandOptions] {
	changes := new(argv.Options[resultCommandOptions])
	changes.Value("server", func(opts *resultCommandOptions, value string) error { opts.server = value; return nil })
	changes.Value("timeout", func(opts *resultCommandOptions, value string) (err error) {
		opts.timeout, err = time.ParseDuration(value)
		return err
	})
	changes.Bool(
		"insecure-skip-verify",
		func(opts *resultCommandOptions, value bool) error { opts.insecure = value; return nil },
	)
	changes.Bool("json", func(opts *resultCommandOptions, value bool) error { opts.jsonOutput = value; return nil })
	changes.Value("out-dir", func(opts *resultCommandOptions, value string) error { opts.outDir = value; return nil })
	changes.Value("source", func(opts *resultCommandOptions, value string) error { opts.source = value; return nil })
	changes.Value("tail", func(opts *resultCommandOptions, value string) (err error) {
		opts.tailLines, err = strconv.Atoi(value)
		return err
	})
	changes.Value("max-bytes", func(opts *resultCommandOptions, value string) (err error) {
		opts.maxBytes, err = strconv.ParseInt(value, 10, 64)
		return err
	})
	return changes
}

func validateResultArguments(
	command string,
	positionals []string,
	parsed *cli.Command,
	opts *resultCommandOptions,
) error {
	if parsed.IsSet("out-dir") && command != "artifacts.get" {
		return errors.New("--out-dir is only valid for artifacts get")
	}
	for _, name := range []string{"source", "tail", "max-bytes"} {
		if parsed.IsSet(name) && command != "logs" {
			return fmt.Errorf("--%s is only valid for logs", name)
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
