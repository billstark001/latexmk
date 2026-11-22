package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	cliresources "github.com/billstark001/latexmk/packages/cli/internal/resources"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	resources "github.com/billstark001/latexmk/packages/shared/resources"
)

// Metadata remains embedded so existing doctor JSON consumers keep their fields.
type doctorReport struct {
	protocol.Metadata
	Doctor doctorDiagnostics `json:"doctor"`
}

type doctorDiagnostics struct {
	Status   string         `json:"status"`
	Target   string         `json:"target,omitempty"`
	Entry    string         `json:"entry,omitempty"`
	Engine   string         `json:"engine"`
	Realtime doctorRealtime `json:"realtime"`
}

type doctorRealtime struct {
	Requested               bool           `json:"requested"`
	CachePolicy             string         `json:"cachePolicy"`
	Workspace               string         `json:"workspace"`
	Supported               bool           `json:"supported"`
	Enabled                 bool           `json:"enabled"`
	Server                  doctorServer   `json:"server"`
	Baseline                doctorBaseline `json:"baseline"`
	LockState               string         `json:"lockState"`
	ResourceContentsChecked bool           `json:"resourceContentsChecked"`
	Reasons                 []doctorReason `json:"reasons"`
	Actions                 []string       `json:"actions"`
}

type doctorServer struct {
	Realtime               bool  `json:"realtime"`
	CompileCache           bool  `json:"compileCache"`
	DefaultRuntimeIsolated bool  `json:"defaultRuntimeIsolated"`
	MaxCompileCacheBytes   int64 `json:"maxCompileCacheBytes"`
}

type doctorBaseline struct {
	ID       string `json:"id,omitempty"`
	Digest   string `json:"digest,omitempty"`
	Source   string `json:"source"`
	State    string `json:"state"`
	Isolated bool   `json:"isolated"`
}

type doctorReason struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Blocking bool   `json:"blocking"`
}

func runDoctor(args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	changes := compileArguments()
	cfg, parsed, err := loadCommand("doctor", cwd, args, changes.Flags()...)
	if err != nil {
		return fail(err)
	}
	opts := optionsFromConfig(cfg, false)
	if err := applyCompileArguments(changes, &opts, parsed); err != nil {
		return fail(err)
	}
	if opts.target == "" && opts.entry == "" {
		opts.target, err = implicitTarget(cfg)
		if err != nil {
			return fail(err)
		}
	}
	if opts.target == "all" {
		return fail(errors.New("doctor selects one target; use --target NAME"))
	}
	if opts.target != "" {
		if err := applyBuildTarget(&opts, cfg, parsed); err != nil {
			return fail(err)
		}
	}
	finalizeResourceOptions(&opts)
	if err := validateResourceCLI(opts); err != nil {
		return fail(err)
	}
	if err := doctorPaths(cwd, cfg, &opts); err != nil {
		return fail(err)
	}
	reportDoctorProjectCache(cwd, opts.projectRoot, opts.jsonOutput)
	if err := cfg.Authenticate(opts.projectRoot); err != nil {
		return fail(err)
	}
	if !opts.jsonOutput {
		terminalFprintln(os.Stderr, "authentication:", cfg.TokenSource)
	}
	connected, err := client.New(opts.server, cfg.Token, opts.timeout, opts.insecure)
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	if err := connected.Health(ctx); err != nil {
		return fail(fmt.Errorf("health check failed: %w", err))
	}
	meta, err := connected.Metadata(ctx)
	if err != nil {
		return fail(err)
	}
	var inventory client.EnvironmentInventory
	var inventoryErr error
	if meta.Capabilities.ResourceEnvironments {
		inventory, inventoryErr = connected.Baselines(ctx)
	}
	diagnostics := diagnoseRealtime(opts, meta, inventory, inventoryErr)
	report := doctorReport{Metadata: meta, Doctor: diagnostics}
	if opts.jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
			return fail(err)
		}
		return 0
	}
	printMetadata(meta)
	printDoctor(diagnostics)
	return 0
}

// Diagnosis does not require an entry to exist, capture resource bytes, or write
// a project identity. Root selection still follows the chosen target/CLI policy.
func doctorPaths(cwd string, cfg config.Resolved, opts *compileOptions) error {
	root := opts.projectRoot
	entry := opts.entry
	if entry != "" && !filepath.IsAbs(entry) {
		entry = filepath.Join(cwd, entry)
	}
	if root == "" {
		root = cwd
		if entry != "" {
			root = filepath.Dir(entry)
		} else if cfg.ConfigPath != "" {
			root = filepath.Dir(cfg.ConfigPath)
		}
		if opts.rootMode == "git" {
			var err error
			root, err = config.FindGitRoot(root)
			if err != nil {
				return err
			}
		}
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	opts.projectRoot = absolute
	if physical, err := filepath.EvalSymlinks(absolute); err == nil {
		opts.projectRoot = physical
	}
	if entry != "" {
		rel, err := filepath.Rel(absolute, entry)
		if err != nil {
			return err
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("doctor entry is outside the project root")
		}
		opts.entry = filepath.ToSlash(rel)
	}
	return nil
}

func diagnoseRealtime(
	opts compileOptions,
	meta protocol.Metadata,
	inventory client.EnvironmentInventory,
	inventoryErr error,
) doctorDiagnostics {
	result := doctorDiagnostics{Status: "healthy", Target: opts.target, Entry: opts.entry, Engine: opts.engine}
	realtime := doctorRealtime{
		Requested:   opts.realtime,
		CachePolicy: opts.auxiliary.Server,
		Workspace:   "fresh",
		LockState:   "not-used",
		Server: doctorServer{
			Realtime:               meta.Capabilities.RealtimeSessions,
			CompileCache:           meta.Capabilities.CompileCache,
			DefaultRuntimeIsolated: meta.Capabilities.IsolatedWorkspaces,
			MaxCompileCacheBytes:   meta.Capabilities.MaxCompileCacheBytes,
		},
		Reasons: []doctorReason{},
		Actions: []string{},
	}
	if realtime.CachePolicy == "" {
		realtime.CachePolicy = "none"
	}
	if realtime.CachePolicy == "reuse" {
		realtime.Workspace = "reuse"
	}
	add := func(code, message string, blocking bool) {
		realtime.Reasons = append(realtime.Reasons, doctorReason{Code: code, Message: message, Blocking: blocking})
	}
	action := func(message string) {
		if !slices.Contains(realtime.Actions, message) {
			realtime.Actions = append(realtime.Actions, message)
		}
	}
	baseline, baselineView, lockState, selectionReasons := doctorSelectedBaseline(opts, meta, inventory, inventoryErr)
	realtime.Baseline, realtime.LockState = baselineView, lockState
	realtime.Reasons = append(realtime.Reasons, selectionReasons...)
	if !meta.Capabilities.RealtimeSessions {
		add("sessions-disabled", "The server does not enable realtime sessions.", true)
		action("Enable LATEXMK_MAX_REALTIME_SESSIONS with a positive value on the server.")
	}
	if !meta.Capabilities.CompileCache {
		add("compile-cache-disabled", "The server does not enable reusable compile state.", true)
		action("Enable positive LATEXMK_COMPILE_CACHE_RETENTION and LATEXMK_MAX_COMPILE_CACHE_BYTES on the server.")
	}
	baselineKnown := baselineView.State == "available" || baselineView.State == "server-metadata"
	realtime.Supported = meta.Capabilities.RealtimeSessions && meta.Capabilities.CompileCache && baselineKnown &&
		baselineView.Isolated
	if meta.Capabilities.ResourceEnvironments &&
		(!meta.Capabilities.QueuedJobs || !meta.Capabilities.IncrementalUpload) {
		realtime.Supported = false
		add(
			"resource-transport-unsupported",
			"Resource-bound realtime sessions require queued jobs and incremental upload support.",
			true,
		)
		action("Upgrade the server to support queued jobs and incremental uploads for resource environments.")
	}
	if baselineKnown && !baselineView.Isolated {
		add(
			"baseline-not-isolated",
			"The selected runtime has no isolated runner image; it supports only fresh realtime workspaces.",
			true,
		)
		action(
			"Configure a digest-pinned LATEXMK_RUNNER_IMAGE and a server with Docker access, or select a registered image baseline with --environment ID.",
		)
	}
	if !opts.realtime {
		add("realtime-not-selected", "This configuration does not select --realtime.", false)
	}
	if realtime.CachePolicy != "reuse" {
		add(
			"reuse-not-selected",
			"auxiliary.server is "+realtime.CachePolicy+"; realtime workspace mode is fresh.",
			false,
		)
	}
	if !opts.realtime || realtime.CachePolicy != "reuse" {
		action(doctorReuseCommand(opts))
	}
	if len(opts.resourceIDs) > 0 {
		add(
			"resources-not-inspected",
			"Resource bytes and package availability were not inspected; doctor does not prepare environments.",
			false,
		)
	}
	if baseline != nil && !slices.Contains(baseline.Engines, opts.engine) {
		add("engine-unavailable", "The selected baseline does not enable engine "+opts.engine+".", true)
		action("Select an engine listed by the chosen runtime, or another target/environment.")
	}
	probe := &client.Client{}
	if baseline != nil {
		probe.SetBaseline(*baseline)
	}
	request := requestFromOptions(opts)
	request.Auxiliary.Server = "reuse"
	if err := probe.ValidateRealtimeRequest(request, meta); err != nil {
		// Infrastructure blockers above carry specific setup guidance. Remaining
		// request policy failures include shell escape and unsupported retention TTL.
		if baselineKnown && baselineView.Isolated && meta.Capabilities.CompileCache {
			add("request-policy", err.Error(), true)
			action("Use --no-shell-escape and remove unsupported auxiliary retention options.")
		}
	}
	if needsResourceEnvironment(opts) && !meta.Capabilities.ResourceEnvironments {
		realtime.Supported = false
		add("resources-unsupported", "The server cannot resolve the selected resource/environment configuration.", true)
	}
	blocked := false
	for _, reason := range realtime.Reasons {
		blocked = blocked || reason.Blocking
	}
	realtime.Enabled = opts.realtime && realtime.CachePolicy == "reuse" && realtime.Supported && !blocked
	if blocked {
		result.Status = "attention"
	}
	if !baselineKnown {
		action(
			"Check the selected baseline and resource lock; register the runtime on this server or refresh the lock explicitly.",
		)
	}
	if realtime.LockState == "invalid" || realtime.LockState == "stale" || realtime.LockState == "missing" {
		action(
			"Review resource requirements, then run latexmk resources lock to update the selected target's lock plan.",
		)
	}
	result.Realtime = realtime
	return result
}

func doctorReuseCommand(opts compileOptions) string {
	command := "Run: latexmk --realtime --server-cache reuse"
	if opts.target != "" {
		command += " --target " + strconv.Quote(opts.target)
	} else if opts.entry != "" {
		command += " " + strconv.Quote(opts.entry)
	}
	return command + ", or set auxiliary.server to reuse in .latexmk.json and invoke --realtime."
}

func doctorSelectedBaseline(
	opts compileOptions,
	meta protocol.Metadata,
	inventory client.EnvironmentInventory,
	inventoryErr error,
) (*resources.Baseline, doctorBaseline, string, []doctorReason) {
	view := doctorBaseline{Source: "server-default", State: "unavailable"}
	lockState := "not-used"
	var reasons []doctorReason
	blocked := func(code, message string) {
		reasons = append(reasons, doctorReason{Code: code, Message: message, Blocking: true})
	}
	if !meta.Capabilities.ResourceEnvironments {
		if needsResourceEnvironment(opts) {
			blocked("baseline-unavailable", "The server does not publish resource baseline inventory.")
			return nil, view, lockState, reasons
		}
		view.Source, view.State, view.Isolated = "server-metadata", "server-metadata", meta.Capabilities.IsolatedWorkspaces
		return nil, view, lockState, reasons
	}
	if inventoryErr != nil {
		blocked("baseline-inventory-unavailable", "Could not inspect runtime baselines: "+inventoryErr.Error())
		return nil, view, lockState, reasons
	}
	selectedID := opts.environment
	if selectedID != "" {
		view.Source = "target"
		if opts.environmentExplicit {
			view.Source = "cli"
		}
	}
	var locked *resources.LockedPlan
	if needsResourceEnvironment(opts) {
		lock, err := cliresources.LoadLock(opts.projectRoot, opts.resourceOptions.LockFile)
		if err != nil {
			lockState = "invalid"
			blocked("lock-invalid", err.Error())
		} else if plan, ok := lock.Plans[resourcePlanKey(opts)]; ok {
			matches, err := doctorLockMatches(opts, plan)
			if err != nil {
				lockState = "invalid"
				blocked("lock-invalid", err.Error())
			} else if matches {
				locked = &plan
				lockState = "matching-recorded-requirements"
				selectedID = plan.Environment.Baseline.ID
				view.Source = "lock"
			} else {
				lockState = "stale"
				if opts.resourceOptions.LockMode == "frozen" {
					blocked("frozen-lock-stale", "Frozen resource requirements differ from the existing lock.")
				}
			}
		} else {
			lockState = "missing"
			if opts.resourceOptions.LockMode == "frozen" {
				blocked("frozen-lock-missing", "Frozen resource mode requires an existing matching lock plan.")
			}
		}
	}
	if selectedID == "" {
		selectedID = inventory.Default
	}
	view.ID = selectedID
	for _, baseline := range inventory.Baselines {
		if baseline.ID != selectedID {
			continue
		}
		view.Digest, view.Isolated, view.State = baseline.Digest, baseline.Image != "", "available"
		if locked != nil && locked.Environment.Baseline.Digest != baseline.Digest {
			view.State = "locked-runtime-changed"
			blocked(
				"locked-runtime-changed",
				"The selected runtime inventory differs from the locked baseline; compilation will reject the drift.",
			)
		}
		return &baseline, view, lockState, reasons
	}
	blocked("baseline-unregistered", "Runtime baseline "+strconv.Quote(selectedID)+" is not registered on this server.")
	return nil, view, lockState, reasons
}

// Reuse the locked local manifests only to compare declarative requirements.
// File contents/filter matches are deliberately not read or claimed as verified.
func doctorLockMatches(opts compileOptions, plan resources.LockedPlan) (bool, error) {
	locked := make(map[string]resources.Resource)
	for _, resource := range plan.Environment.Resources {
		locked[resource.ID] = resource
	}
	request := resources.ResolveRequest{Baseline: opts.environment, Requires: opts.requires}
	for _, id := range opts.resourceIDs {
		spec, ok := opts.resourceSpecs[id]
		if !ok {
			return false, fmt.Errorf("unknown resource %q", id)
		}
		resource := resources.Resource{
			ID:             id,
			Type:           spec.Type,
			Kind:           spec.Kind,
			Mount:          spec.Mount,
			Packages:       spec.Packages,
			Ref:            spec.Ref,
			LicenseFiles:   spec.LicenseFiles,
			Redistribution: spec.Redistribution,
		}
		if spec.Type == "files" {
			saved, ok := locked[id]
			if !ok || saved.Type != "files" {
				return false, nil
			}
			resource.Files = saved.Files
		}
		request.Resources = append(request.Resources, resource)
	}
	normalized, err := resources.NormalizeRequest(request)
	if err != nil {
		return false, err
	}
	digest, err := resources.Hash(normalized)
	return digest == plan.SpecDigest, err
}

func printDoctor(result doctorDiagnostics) {
	realtime := result.Realtime
	terminalPrintf("realtime target: %s (engine=%s)\n", result.Target, result.Engine)
	terminalPrintf(
		"realtime requested: %t\nserver cache policy: %s\nselected workspace: %s\n",
		realtime.Requested,
		realtime.CachePolicy,
		realtime.Workspace,
	)
	terminalPrintf("reuse supported: %t\nreuse enabled: %t\n", realtime.Supported, realtime.Enabled)
	terminalPrintf(
		"selected baseline: %s (%s, %s)\nresource lock: %s\n",
		realtime.Baseline.ID,
		realtime.Baseline.Source,
		realtime.Baseline.State,
		realtime.LockState,
	)
	for _, reason := range realtime.Reasons {
		terminalPrintln(reason.Code + ": " + reason.Message)
	}
	for _, action := range realtime.Actions {
		terminalPrintln(action)
	}
	terminalPrintln("status: " + result.Status)
}
