package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/client"
	"github.com/billstark001/latexmk/packages/cli/internal/config"
	cliresources "github.com/billstark001/latexmk/packages/cli/internal/resources"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func applyPublication(packing *packOptions, opts *compileOptions, cfg config.Resolved) error {
	if packing.publication == "" {
		return nil
	}
	publication, exists := cfg.Publications[packing.publication]
	if !exists {
		return fmt.Errorf("unknown publication %q", packing.publication)
	}
	if opts.entry != "" || opts.target != "" || opts.engineExplicit {
		return errors.New("--publication cannot be combined with an entry, --target, or engine override")
	}
	opts.target = publication.Target
	if !packing.modeExplicit && publication.Mode != "" {
		packing.mode = publication.Mode
	}
	if packing.verifyMode == "" {
		packing.verifyMode = publication.Verify
		if packing.verifyMode == "" {
			packing.verifyMode = "none"
			if publication.Environment != "" {
				packing.verifyMode = "target"
			} else if packing.mode == "arxiv" {
				packing.verifyMode = "build"
			}
		}
	}
	if packing.verifyMode == "target" && packing.verifyIn == "" {
		packing.verifyIn = publication.Environment
	}
	if packing.verifyMode == "target" && packing.verifyIn == "" {
		return errors.New("target verification requires publication.environment or --verify-in")
	}
	if packing.verifyMode == "build" && publication.Environment != "" {
		return errors.New(
			"build verification cannot be combined with publication.environment; use --verify-in for target verification",
		)
	}
	if packing.verifyMode == "none" && (publication.Environment != "" || packing.mode == "arxiv") {
		return errors.New("verification none requires default mode without a target environment")
	}
	if packing.output == "" && packing.outputTemplate == "" {
		packing.output = publication.Output
		packing.outputFromPublication = publication.Output != ""
	}
	actions := make(map[string]string, len(publication.Resources)+len(packing.resourceActions))
	for id, action := range publication.Resources {
		actions[id] = action
	}
	for id, action := range packing.resourceActions {
		actions[id] = action
	}
	packing.resourceActions = actions
	packing.includeFiles = append(slices.Clone(publication.IncludeFiles), packing.includeFiles...)
	packing.exclude = append(slices.Clone(publication.Exclude), packing.exclude...)
	return nil
}

func publicationScope(packing packOptions) string {
	if packing.verifyIn != "" {
		return "target-environment"
	}
	return "server-environment"
}

func resourceAction(packing packOptions, id string) string {
	if action := packing.resourceActions[id]; action != "" {
		return action
	}
	return "omit"
}

func hasServerBundle(values []resources.Resource, packing packOptions) bool {
	for _, value := range values {
		if value.Type == "server" && resourceAction(packing, value.ID) == "bundle" {
			return true
		}
	}
	return false
}

func publicationPlanKey(packing packOptions, opts compileOptions) string {
	if packing.publication != "" {
		return "publication:" + packing.publication
	}
	return "pack:" + opts.target + ":" + opts.entry + ":" + packing.mode
}

func exportPublicationResources(ctx context.Context, connected *client.Client, opts compileOptions,
	packing packOptions, meta protocol.Metadata, capture *cliresources.Captured,
) (exports []*projectarchive.Frozen, err error) {
	var selected []resources.Resource
	for _, value := range capture.Resources {
		if value.Type == "server" && resourceAction(packing, value.ID) == "bundle" {
			selected = append(selected, value)
		}
	}
	if len(selected) == 0 {
		return nil, nil
	}
	baseline := packing.verifyIn
	if baseline == "" {
		baseline = opts.environment
	}
	planKey := publicationPlanKey(packing, opts) + ":exports"
	environment, err := resolveResourceEnvironment(
		ctx,
		connected,
		opts,
		meta,
		capture,
		planKey,
		resources.ResolveRequest{Baseline: baseline, Resources: selected},
		resourcePrepare,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			for _, exported := range exports {
				_ = exported.Close()
			}
			exports = nil
		}
	}()
	requested := make(map[string]int, len(selected))
	for i, value := range capture.Resources {
		if value.Type == "server" && resourceAction(packing, value.ID) == "bundle" {
			requested[value.ID] = i
		}
	}
	if len(environment.Resources) != len(requested) {
		return exports, errors.New("resource export environment does not match the requested resources")
	}
	for _, resolved := range environment.Resources {
		index, ok := requested[resolved.ID]
		if !ok {
			return exports, errors.New("resource export environment contains an undeclared resource")
		}
		original := capture.Resources[index]
		if resolved.Type != "server" || resolved.Ref != original.Ref || resolved.Mount != original.Mount {
			return exports, errors.New("resource export environment changed a registered resource reference or mount")
		}
	}
	for _, resolved := range environment.Resources {
		exported, err := connected.ExportEnvironmentResource(ctx, environment, resolved.ID)
		if err != nil {
			return exports, err
		}
		exports = append(exports, exported)
	}
	// Publish capture changes only after every export has succeeded. A failed
	// export leaves no descriptors pointing at spools removed by the error path.
	for i, resolved := range environment.Resources {
		capture.MountFilesByID[resolved.ID] = exports[i].Files
		capture.Resources[requested[resolved.ID]] = resolved
	}
	return exports, nil
}

// Offline server resources have no bytes or trustworthy member inventory yet.
// Literal references beneath a dedicated declared mount can be shown as pending
// without claiming that the resource exists or that its nested inputs are known.
func addPendingPublicationReferences(
	c *client.Client,
	request protocol.CompileRequest,
	values []resources.Resource,
) ([]string, error) {
	var mounts []string
	for _, value := range values {
		if value.Type == "server" && value.Mount != "" && value.Mount != "." {
			mounts = append(mounts, value.Mount+"/")
		}
	}
	if len(mounts) == 0 {
		return nil, nil
	}
	selection, err := c.Selection(request.Entry, request.Engine, nil)
	if err != nil {
		return nil, err
	}
	var warnings []string
	pending := make(map[string]bool)
	for _, diagnostic := range selection.Diagnostics {
		if diagnostic.Resolution != "" || diagnostic.Kind != "unavailable" {
			continue
		}
		clean, err := safefs.Clean(diagnostic.Reference)
		if err != nil || clean != diagnostic.Reference || pending[clean] {
			continue
		}
		for _, mount := range mounts {
			if strings.HasPrefix(clean, mount) {
				pending[clean] = true
				c.ResourceFiles = append(
					c.ResourceFiles,
					projectarchive.File{Path: clean, External: true, Reason: "server resource inventory pending"},
				)
				warnings = append(
					warnings,
					"server resource reference "+clean+" is pending; nested dependency discovery may be incomplete",
				)
				break
			}
		}
	}
	return warnings, nil
}
