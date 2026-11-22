package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	cliresources "github.com/billstark001/latexmk/packages/cli/internal/resources"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	shared "github.com/billstark001/latexmk/packages/shared/resources"
)

type resourceOperation uint8

const (
	resourcePrepare resourceOperation = iota
	resourceRefreshLock
)

func needsResourceEnvironment(opts compileOptions) bool {
	return len(opts.resourceIDs) > 0 || opts.environment != "" || len(opts.requires.Fonts) > 0 ||
		len(opts.requires.Tools) > 0 || opts.resourceOptions.LockMode == "frozen" || opts.resourceOptions.LockFile != ""
}
func resourcePrepareTimeout(opts compileOptions) time.Duration {
	if value, err := time.ParseDuration(opts.resourceOptions.PrepareTimeout); err == nil && value > 0 {
		return value
	}
	return 5 * time.Minute
}
func resourcePlanKey(opts compileOptions) string {
	if opts.target != "" {
		return "target:" + opts.target
	}
	return "default"
}

func prepareResourceEnvironment(
	ctx context.Context,
	c *client.Client,
	opts compileOptions,
	meta protocol.Metadata,
	captured *cliresources.Captured,
) (shared.Environment, error) {
	return resolveResourceEnvironment(
		ctx,
		c,
		opts,
		meta,
		captured,
		resourcePlanKey(opts),
		shared.ResolveRequest{Baseline: opts.environment, Resources: captured.Resources, Requires: opts.requires},
		resourcePrepare,
	)
}

// resolveResourceEnvironment freezes the resolver's result before any build.
// A matching lock pins its concrete baseline even when the server default moves.
func resolveResourceEnvironment(
	ctx context.Context,
	c *client.Client,
	opts compileOptions,
	meta protocol.Metadata,
	captured *cliresources.Captured,
	planKey string,
	request shared.ResolveRequest,
	operation resourceOperation,
) (shared.Environment, error) {
	var environment shared.Environment
	preparationClient := *c
	if c.HTTP != nil {
		httpClient := *c.HTTP
		httpClient.Timeout = resourcePrepareTimeout(opts)
		preparationClient.HTTP = &httpClient
	}
	c = &preparationClient
	if !meta.Capabilities.ResourceEnvironments {
		return environment, &client.CapabilityError{Capability: "resource environments"}
	}
	normalized, err := shared.NormalizeRequest(request)
	if err != nil {
		return environment, err
	}
	specDigest, err := shared.Hash(normalized)
	if err != nil {
		return environment, err
	}
	lock, err := cliresources.LoadLock(opts.projectRoot, opts.resourceOptions.LockFile)
	if err != nil {
		return environment, err
	}
	locked, found := lock.Plans[planKey]
	matching := found && locked.SpecDigest == specDigest && operation != resourceRefreshLock
	if opts.resourceOptions.LockMode == "frozen" && operation != resourceRefreshLock && !matching {
		return environment, fmt.Errorf("resource lock plan %q is missing or stale; run latexmk resources lock", planKey)
	}
	// Re-resolve a matching plan against its concrete baseline after eviction or
	// restart. The fingerprint above still describes the caller's original request.
	resolveRequest := normalized
	if matching {
		resolveRequest.Baseline = locked.Environment.Baseline.ID
	}
	environment, err = c.ResolveEnvironment(ctx, resolveRequest)
	if err != nil {
		return environment, err
	}
	if matching && environment.Digest != locked.Environment.Digest {
		return environment, errors.New(
			"locked resource environment no longer matches the server; update the resource lock explicitly",
		)
	}

	if !supportsEnvironmentEngine(environment, opts.engine) {
		return environment, fmt.Errorf(
			"environment %q does not support engine %q",
			environment.Baseline.ID,
			opts.engine,
		)
	}
	if operation == resourcePrepare {
		var blobs map[string]projectarchive.File
		if captured != nil {
			blobs = captured.Blobs
		}
		environment, err = c.PrepareEnvironment(ctx, environment, blobs)
		if err != nil {
			return environment, err
		}
	}
	if !matching {
		plan := shared.LockedPlan{SpecDigest: specDigest, Environment: environment}
		if err := cliresources.SavePlan(opts.projectRoot, opts.resourceOptions.LockFile, planKey, plan); err != nil {
			return environment, err
		}
	}
	return environment, nil
}

func supportsEnvironmentEngine(environment shared.Environment, engine string) bool {
	return engine == "" || slices.Contains(environment.Baseline.Engines, engine)
}

func environmentResourceFiles(
	environment shared.Environment,
	captured *cliresources.Captured,
) ([]projectarchive.File, error) {
	local := make(map[string]projectarchive.File)
	if captured != nil {
		for _, file := range captured.MountFiles() {
			local[file.Path] = file
		}
	}
	var files []projectarchive.File
	for _, resource := range environment.Resources {
		for _, file := range resource.Files {
			destination, err := shared.Destination(resource.Mount, file.Path)
			if err != nil {
				return nil, fmt.Errorf("environment resource %q: %w", resource.ID, err)
			}
			member, ok := local[destination]
			if ok && (member.SHA256 != file.SHA256 || member.Size != file.Size) {
				return nil, fmt.Errorf("captured resource does not match environment destination %q", destination)
			}
			if !ok {
				member = projectarchive.File{
					Path:     destination,
					SHA256:   file.SHA256,
					Size:     file.Size,
					External: true,
					Reason:   "server resource " + resource.ID,
				}
			}
			files = append(files, member)
		}
	}
	return files, nil
}

func resourceControlPaths(opts compileOptions) []string {
	if !needsResourceEnvironment(opts) && len(opts.resourceSpecs) == 0 {
		return nil
	}
	names := map[string]bool{
		filepath.Join(opts.projectRoot, cliresources.LockName(opts.resourceOptions.LockFile)): true,
	}
	for _, root := range cliresources.ControlPaths(opts.projectRoot, opts.resourceSpecs, opts.resourceIDs) {
		names[root] = true
		// Observe every member and directory, including nested additions. Keep the
		// root watched if traversal becomes incomplete; the next capture reports
		// missing/unreadable sources, rather than accepting a partial environment.
		_ = filepath.WalkDir(root, func(name string, _ os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			names[name] = true
			return nil
		})
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func runResources(args []string) int {
	jsonOutput := hasJSONFlag(args)
	if len(args) == 0 {
		return failAgentArguments("resources", jsonOutput, errors.New("resources requires lock, fetch, or inspect"))
	}
	action := args[0]
	command := "resources." + action
	if action != "lock" && action != "fetch" && action != "inspect" {
		return failAgentArguments(command, jsonOutput, errors.New("resources supports lock, fetch, or inspect"))
	}
	cwd, err := os.Getwd()
	if err != nil {
		return failAgent(command, jsonOutput, err)
	}
	cfg, remaining, err := config.LoadArgs(cwd, args[1:])
	if err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	opts := optionsFromConfig(cfg, false)
	if err := parseCompileArgs(remaining, &opts); err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	if opts.watch || opts.realtime || opts.detach {
		return failAgentArguments(
			command,
			jsonOutput,
			errors.New("resource commands do not support watch, realtime, or detach"),
		)
	}
	if opts.target == "" && opts.entry == "" {
		opts.target, err = implicitTarget(cfg)
		if err != nil {
			return failAgentArguments(command, jsonOutput, err)
		}
	}
	if opts.target != "" {
		if opts.target == "all" {
			return failAgentArguments(command, jsonOutput, errors.New("resource commands select one target"))
		}
		if err := applyBuildTarget(&opts, cfg, remaining); err != nil {
			return failAgentArguments(command, jsonOutput, err)
		}
	}
	if opts.entry != "" {
		if err := normalizeCompilePaths(&opts, cwd); err != nil {
			return failAgentArguments(command, jsonOutput, err)
		}
	}
	if opts.projectRoot == "" {
		opts.projectRoot = cwd
		if cfg.ConfigPath != "" {
			opts.projectRoot = filepath.Dir(cfg.ConfigPath)
		}
	}
	opts.projectRoot, err = filepath.Abs(opts.projectRoot)
	if err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	finalizeResourceOptions(&opts)
	if err := validateResourceCLI(opts); err != nil {
		return failAgentArguments(command, jsonOutput, err)
	}
	captured, err := cliresources.Capture(
		context.Background(),
		opts.projectRoot,
		opts.resourceSpecs,
		opts.resourceIDs,
		opts.denyFiles...)
	if err != nil {
		return failAgent(command, jsonOutput, err)
	}
	defer func() { _ = captured.Close() }()
	if action == "inspect" || opts.dryRun {
		lock, err := cliresources.LoadLock(opts.projectRoot, opts.resourceOptions.LockFile)
		if err != nil {
			return failAgent(command, jsonOutput, err)
		}
		value := struct {
			Resources []shared.Resource   `json:"resources"`
			Requires  shared.Requirements `json:"requires"`
			Baseline  string              `json:"baseline,omitempty"`
			Lock      shared.Lock         `json:"lock"`
			Pending   bool                `json:"preparationPending"`
		}{captured.Resources, opts.requires, opts.environment, lock, action != "inspect"}
		if jsonOutput {
			if err := writeAgentJSON(command, value); err != nil {
				return fail(err)
			}
		} else {
			raw, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				return failAgent(command, jsonOutput, err)
			}
			terminalPrintln(string(raw))
		}
		return 0
	}
	if err := cfg.Authenticate(opts.projectRoot); err != nil {
		return failAgent(command, jsonOutput, err)
	}
	c, err := client.New(opts.server, cfg.Token, resourcePrepareTimeout(opts), opts.insecure)
	if err != nil {
		return failAgent(command, jsonOutput, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), resourcePrepareTimeout(opts))
	defer cancel()
	meta, err := c.Metadata(ctx)
	if err != nil {
		return failAgent(command, jsonOutput, err)
	}
	operation := resourcePrepare
	if action == "lock" {
		operation = resourceRefreshLock
	}
	environment, err := resolveResourceEnvironment(
		ctx,
		c,
		opts,
		meta,
		captured,
		resourcePlanKey(opts),
		shared.ResolveRequest{Baseline: opts.environment, Resources: captured.Resources, Requires: opts.requires},
		operation,
	)
	if err != nil {
		return failAgent(command, jsonOutput, err)
	}
	if jsonOutput {
		if err := writeAgentJSON(command, environment); err != nil {
			return fail(err)
		}
	} else {
		terminalPrintf(
			"environment: %s\nbaseline: %s\nready: %t\n",
			environment.Digest,
			environment.Baseline.ID,
			environment.Ready,
		)
	}
	return 0
}

func finalizeResourceOptions(opts *compileOptions) {
	ids := make(map[string]bool)
	for _, id := range opts.resourceIDs {
		ids[id] = true
	}
	for _, id := range opts.resourceAddIDs {
		ids[id] = true
	}
	for _, id := range opts.resourceRemoveIDs {
		delete(ids, id)
	}
	opts.resourceIDs = make([]string, 0, len(ids))
	for id := range ids {
		opts.resourceIDs = append(opts.resourceIDs, id)
	}
	sort.Strings(opts.resourceIDs)
}

func validateResourceCLI(opts compileOptions) error {
	for _, ids := range [][]string{opts.resourceAddIDs, opts.resourceRemoveIDs} {
		for _, id := range ids {
			if _, ok := opts.resourceSpecs[id]; !ok {
				return fmt.Errorf("unknown resource %q", id)
			}
		}
	}
	return nil
}
