package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/shared/argv"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

var jobIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type jobsOptions struct {
	server     string
	token      string
	timeout    time.Duration
	insecure   bool
	jsonOutput bool
	limit      int
	jobID      string
}

type jobsListData struct {
	Jobs  []protocol.Job `json:"jobs"`
	Count int            `json:"count"`
	Limit int            `json:"limit"`
}

func runJobs(args []string) int {
	command := "jobs"
	cwd, err := os.Getwd()
	if err != nil {
		return failAgent(command, false, err)
	}
	changes := jobsArguments()
	cfg, parsed, err := loadCommand(command, cwd, args, changes.Flags()...)
	jsonOutput := parsed != nil && parsed.Bool("json")
	positionals := []string(nil)
	if parsed != nil {
		positionals = parsed.Args().Slice()
	}
	if len(positionals) > 0 {
		command += "." + positionals[0]
	}
	if err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	if len(positionals) == 0 {
		return failAgentArguments(command, jsonOutput, errors.New("jobs requires list, show, or cancel"))
	}
	action := positionals[0]
	if action != "list" && action != "show" && action != "cancel" {
		return failAgentArguments(command, jsonOutput, fmt.Errorf("unknown jobs action %q", action))
	}
	opts := jobsOptions{
		server: cfg.Server, token: cfg.Token, timeout: cfg.Timeout,
		insecure: cfg.InsecureSkipVerify, limit: protocol.DefaultJobListLimit,
	}
	if err := changes.Apply(&opts); err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	if err := validateJobsArguments(action, positionals[1:], parsed, &opts); err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	if err := cfg.Authenticate(cfg.ProjectRoot); err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	opts.token = cfg.Token
	c, err := client.New(opts.server, opts.token, opts.timeout, opts.insecure)
	if err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	switch action {
	case "list":
		jobs, err := c.ListJobs(ctx, opts.limit)
		if err != nil {
			return failAgent(command, opts.jsonOutput, err)
		}
		data := jobsListData{Jobs: jobs, Count: len(jobs), Limit: opts.limit}
		if opts.jsonOutput {
			if err := writeAgentJSON(command, data); err != nil {
				return fail(err)
			}
			return 0
		}
		for _, job := range jobs {
			terminalPrintf("%s\t%s\t%s\t%s\n", job.ID, job.Status, job.CreatedAt.Format(time.RFC3339), job.ProjectID)
		}
		return 0
	case "show":
		job, err := c.GetJob(ctx, opts.jobID)
		if err != nil {
			return failAgent(command, opts.jsonOutput, err)
		}
		return reportJob(command, job, opts.jsonOutput)
	case "cancel":
		job, err := c.CancelJob(ctx, opts.jobID)
		if err != nil {
			return failAgent(command, opts.jsonOutput, err)
		}
		return reportJob(command, job, opts.jsonOutput)
	}
	return failAgentArguments(command, opts.jsonOutput, errors.New("unsupported jobs action"))
}

func jobsArguments() *argv.Options[jobsOptions] {
	changes := new(argv.Options[jobsOptions])
	changes.Value("server", func(opts *jobsOptions, value string) error { opts.server = value; return nil })
	changes.Value("timeout", func(opts *jobsOptions, value string) (err error) {
		opts.timeout, err = time.ParseDuration(value)
		return err
	})
	changes.Bool(
		"insecure-skip-verify",
		func(opts *jobsOptions, value bool) error { opts.insecure = value; return nil },
	)
	changes.Bool("json", func(opts *jobsOptions, value bool) error { opts.jsonOutput = value; return nil })
	changes.Value("limit", func(opts *jobsOptions, value string) (err error) {
		opts.limit, err = strconv.Atoi(value)
		return err
	})
	return changes
}

func validateJobsArguments(action string, positionals []string, parsed *cli.Command, opts *jobsOptions) error {
	if action != "list" && parsed.IsSet("limit") {
		return errors.New("--limit is only valid for jobs list")
	}
	if opts.timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if action == "list" {
		if len(positionals) != 0 {
			return errors.New("jobs list does not accept a job ID")
		}
		if opts.limit < 1 || opts.limit > protocol.MaxJobListLimit {
			return fmt.Errorf("--limit must be between 1 and %d", protocol.MaxJobListLimit)
		}
		return nil
	}
	if len(positionals) != 1 || !jobIDPattern.MatchString(positionals[0]) {
		return fmt.Errorf("jobs %s requires one valid job ID", action)
	}
	opts.jobID = positionals[0]
	return nil
}

func reportJob(command string, job protocol.Job, jsonOutput bool) int {
	if jsonOutput {
		if err := writeAgentJSON(command, job); err != nil {
			return fail(err)
		}
		return 0
	}
	terminalPrintf(
		"job ID: %s\nproject ID: %s\nstatus: %s\ncreated: %s\n",
		job.ID,
		job.ProjectID,
		job.Status,
		job.CreatedAt.Format(time.RFC3339),
	)
	if job.SnapshotID != "" {
		terminalPrintf("snapshot ID: %s\n", job.SnapshotID)
	}
	if job.StartedAt != nil {
		terminalPrintf("started: %s\n", job.StartedAt.Format(time.RFC3339))
	}
	if job.FinishedAt != nil {
		terminalPrintf("finished: %s\n", job.FinishedAt.Format(time.RFC3339))
	}
	if job.Error != "" {
		terminalPrintf("error: %s\n", job.Error)
	}
	return 0
}
