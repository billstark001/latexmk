package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func validateResources(cfg FileConfig) error {
	if len(cfg.Resources) > 256 {
		return fmt.Errorf("resources exceeds 256 declarations")
	}
	for id, spec := range cfg.Resources {
		if !resources.ValidID(id) {
			return fmt.Errorf("invalid resource ID %q", id)
		}
		switch spec.Type {
		case "files":
			clean, err := safefs.Clean(spec.Path)
			if err != nil || clean != spec.Path {
				return fmt.Errorf("resource %q path must be a clean project-relative file or directory", id)
			}
			if spec.Ref != "" || len(spec.Packages) != 0 {
				return fmt.Errorf("file resource %q cannot specify ref or packages", id)
			}
		case "texlive", "server":
			if spec.Path != "" || len(spec.Include) != 0 || len(spec.Exclude) != 0 {
				return fmt.Errorf("resource %q local path filters require type files", id)
			}
			_, err := resources.NormalizeRequest(
				resources.ResolveRequest{
					Resources: []resources.Resource{
						{
							ID:             id,
							Type:           spec.Type,
							Kind:           spec.Kind,
							Mount:          spec.Mount,
							Packages:       spec.Packages,
							Ref:            spec.Ref,
							Redistribution: spec.Redistribution,
						},
					},
				},
			)
			if err != nil {
				return fmt.Errorf("resource %q: %w", id, err)
			}
			// The shared normalizer has validated remote resource metadata already.
			continue
		default:
			return fmt.Errorf("resource %q type must be texlive, files, or server", id)
		}
		if spec.Kind != "" && spec.Kind != "font" && spec.Kind != "tex" && spec.Kind != "texmf" && spec.Kind != "data" {
			return fmt.Errorf("resource %q has invalid kind", id)
		}
		if spec.Redistribution != "" && spec.Redistribution != "allow" && spec.Redistribution != "deny" &&
			spec.Redistribution != "unknown" {
			return fmt.Errorf("resource %q has invalid redistribution", id)
		}
		if _, err := resources.MountPath(spec.Mount); err != nil {
			return fmt.Errorf("resource %q: %w", id, err)
		}
	}
	check := func(ids []string) error {
		for _, id := range ids {
			if _, ok := cfg.Resources[id]; !ok {
				return fmt.Errorf("unknown resource %q", id)
			}
		}
		return nil
	}
	if err := check(cfg.DefaultResources); err != nil {
		return err
	}
	for name, target := range cfg.Targets {
		if err := check(target.Resources); err != nil {
			return fmt.Errorf("target %q: %w", name, err)
		}
		if _, err := resources.NormalizeRequest(
			resources.ResolveRequest{Baseline: target.Environment, Requires: target.Requires},
		); err != nil {
			return fmt.Errorf("target %q: %w", name, err)
		}
	}
	if cfg.ResourceOptions.LockMode != "" && cfg.ResourceOptions.LockMode != "auto" &&
		cfg.ResourceOptions.LockMode != "frozen" {
		return fmt.Errorf("resourceOptions.lockMode must be auto or frozen")
	}
	if name := cfg.ResourceOptions.LockFile; name != "" {
		if clean, err := safefs.Clean(name); err != nil || clean != name {
			return fmt.Errorf("resourceOptions.lockFile must be project-relative")
		}
	}
	if value := cfg.ResourceOptions.PrepareTimeout; value != "" {
		if d, err := time.ParseDuration(value); err != nil || d <= 0 {
			return fmt.Errorf("resourceOptions.prepareTimeout must be positive")
		}
	}
	for name, publication := range cfg.Publications {
		if !resources.ValidID(name) {
			return fmt.Errorf("invalid publication name %q", name)
		}
		if strings.TrimSpace(publication.Target) == "" {
			return fmt.Errorf("publication %q requires a target", name)
		}
		if _, ok := cfg.Targets[publication.Target]; !ok {
			return fmt.Errorf("publication %q references unknown target %q", name, publication.Target)
		}
		if publication.Mode != "" && publication.Mode != "default" && publication.Mode != "arxiv" {
			return fmt.Errorf("publication %q has invalid mode", name)
		}
		if publication.Environment != "" && !resources.ValidID(publication.Environment) {
			return fmt.Errorf("publication %q has invalid environment", name)
		}
		if publication.Verify != "" && publication.Verify != "target" && publication.Verify != "build" &&
			publication.Verify != "none" {
			return fmt.Errorf("publication %q verify must be target, build, or none", name)
		}
		if publication.Verify == "target" && publication.Environment == "" {
			return fmt.Errorf("publication %q target verification requires an environment", name)
		}
		if publication.Verify == "build" && publication.Environment != "" {
			return fmt.Errorf("publication %q build verification cannot specify an environment", name)
		}
		if publication.Verify == "none" && (publication.Environment != "" || publication.Mode == "arxiv") {
			return fmt.Errorf("publication %q verify none requires default mode without an environment", name)
		}
		for id, action := range publication.Resources {
			if _, ok := cfg.Resources[id]; !ok {
				return fmt.Errorf("publication %q references unknown resource %q", name, id)
			}
			if action != "bundle" && action != "provided" && action != "omit" {
				return fmt.Errorf("publication %q resource %q has invalid disposition %q", name, id, action)
			}
		}
	}
	return nil
}

// MarshalJSON preserves an explicit empty resource list, which clears inherited defaults.
func (target Target) MarshalJSON() ([]byte, error) {
	type plain Target
	var ids *[]string
	if target.Resources != nil {
		ids = &target.Resources
	}
	return json.Marshal(struct {
		*plain
		Resources *[]string `json:"resources,omitempty"`
	}{(*plain)(&target), ids})
}
