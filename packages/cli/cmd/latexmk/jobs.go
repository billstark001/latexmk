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
	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
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
	jsonOutput := hasJSONFlag(args)
	command := "jobs"
	if len(args) == 0 {
		return failAgentArguments(command, jsonOutput, errors.New("jobs requires list, show, or cancel"))
	}
	action := args[0]
	command += "." + action
	if action != "list" && action != "show" && action != "cancel" {
		return failAgentArguments(command, jsonOutput, fmt.Errorf("unknown jobs action %q", action))
	}

	cwd, err := os.Getwd()
	if err != nil {
		return failAgent(command, jsonOutput, err)
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		return failAgent(command, jsonOutput, err)
	}
	opts := jobsOptions{
		server: cfg.Server, token: cfg.Token, timeout: cfg.Timeout,
		insecure: cfg.InsecureSkipVerify, jsonOutput: jsonOutput, limit: 50,
	}
	if err := parseJobsArgs(action, args[1:], &opts); err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
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
			fmt.Printf("%s\t%s\t%s\t%s\n", job.ID, job.Status, job.CreatedAt.Format(time.RFC3339), job.ProjectID)
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

func parseJobsArgs(action string, args []string, opts *jobsOptions) error {
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
		var err error
		switch {
		case a == "--server" || strings.HasPrefix(a, "--server="):
			opts.server, err = value("--server")
		case a == "--token" || strings.HasPrefix(a, "--token="):
			opts.token, err = value("--token")
		case a == "--token-file" || strings.HasPrefix(a, "--token-file="):
			var path string
			path, err = value("--token-file")
			if err == nil {
				opts.token, err = config.ReadTokenFile(path)
			}
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
		case a == "--limit" || strings.HasPrefix(a, "--limit="):
			if action != "list" {
				return fmt.Errorf("--limit is only valid for jobs list")
			}
			var raw string
			raw, err = value("--limit")
			if err == nil {
				opts.limit, err = strconv.Atoi(raw)
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
	if action == "list" {
		if len(positionals) != 0 {
			return errors.New("jobs list does not accept a job ID")
		}
		if opts.limit < 1 || opts.limit > 200 {
			return errors.New("--limit must be between 1 and 200")
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
	fmt.Printf("job ID: %s\nproject ID: %s\nstatus: %s\ncreated: %s\n", job.ID, job.ProjectID, job.Status, job.CreatedAt.Format(time.RFC3339))
	if job.SnapshotID != "" {
		fmt.Printf("snapshot ID: %s\n", job.SnapshotID)
	}
	if job.StartedAt != nil {
		fmt.Printf("started: %s\n", job.StartedAt.Format(time.RFC3339))
	}
	if job.FinishedAt != nil {
		fmt.Printf("finished: %s\n", job.FinishedAt.Format(time.RFC3339))
	}
	if job.Error != "" {
		fmt.Printf("error: %s\n", job.Error)
	}
	return 0
}

func hasJSONFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--json" {
			return true
		}
	}
	return false
}
