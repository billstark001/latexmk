package pack

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"

	"github.com/bmatcuk/doublestar/v4"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/dependency"
	"github.com/billstark001/latexmk/packages/shared/resources"
)

// Member explains an included or excluded member without revealing its local spool.
type Member struct {
	Path             string `json:"path"`
	SHA256           string `json:"sha256,omitempty"`
	Size             int64  `json:"size"`
	Source           string `json:"source"`
	ResourceID       string `json:"resourceId,omitempty"`
	Action           string `json:"action"`
	Reason           string `json:"reason"`
	FileMode         string `json:"fileMode,omitempty"`
	PermissionPolicy string `json:"permissionPolicy,omitempty"`
	PermissionReason string `json:"permissionReason,omitempty"`
	permissions      os.FileMode
}

type ResourceAction struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Reason string `json:"reason"`
}

type Plan struct {
	PreservePermissions bool                   `json:"preservePermissions"`
	Permissions         map[string]os.FileMode `json:"-"`
	Files               []projectarchive.File  `json:"-"`
	Members             []Member               `json:"members"`
	Resources           []ResourceAction       `json:"resources,omitempty"`
}

type PlanOptions struct {
	PreservePermissions bool
	Entry               string
	Mode                string
	Exclude             []string
	Resources           []resources.Resource
	Actions             map[string]string
	ResourceFiles       map[string][]projectarchive.File
	Preview             bool
}

type exclusion struct {
	pattern string
	reason  string
}

// Planner freezes publication policy once, before source or generated members
// are selected. All build stages use the same exclusions and resource decisions.
type Planner struct {
	entry               string
	preservePermissions bool
	resourcePermissions map[string]os.FileMode
	mode                string
	exclusions          []exclusion
	resourceFiles       []projectarchive.File
	resourceMembers     []Member
	actions             []ResourceAction
	provided            []resources.Resource
}

func NewPlanner(options PlanOptions) (*Planner, error) {
	if err := ValidateMode(options.Mode); err != nil {
		return nil, err
	}
	planner := &Planner{entry: options.Entry, mode: options.Mode, preservePermissions: options.PreservePermissions,
		resourcePermissions: make(map[string]os.FileMode)}
	for _, expression := range options.Exclude {
		pattern, err := dependency.NormalizePattern(expression)
		if err != nil {
			return nil, fmt.Errorf("pack exclusion: %w", err)
		}
		planner.exclusions = append(
			planner.exclusions,
			exclusion{pattern: pattern, reason: "pack exclusion: " + expression},
		)
	}
	known := make(map[string]bool, len(options.Resources))
	for _, resource := range options.Resources {
		if known[resource.ID] {
			return nil, fmt.Errorf("duplicate publication resource %q", resource.ID)
		}
		known[resource.ID] = true
	}
	for id, action := range options.Actions {
		if !known[id] {
			return nil, fmt.Errorf("publication resource %q is not selected by its target", id)
		}
		if action != "bundle" && action != "provided" && action != "omit" {
			return nil, fmt.Errorf("resource %q action must be bundle, provided, or omit", id)
		}
	}
	for _, resource := range options.Resources {
		action := options.Actions[resource.ID]
		if action == "" {
			action = "omit"
		}
		decision := ResourceAction{ID: resource.ID, Action: action, Reason: "publication resource policy"}
		files := options.ResourceFiles[resource.ID]
		switch action {
		case "provided", "omit":
			if action == "provided" {
				planner.provided = append(planner.provided, resource)
				if options.Preview {
					decision.Reason = "target baseline inventory check pending"
				}
			}
			for _, file := range files {
				planner.resourceMembers = append(planner.resourceMembers, Member{
					Path:       file.Path,
					SHA256:     file.SHA256,
					Size:       file.Size,
					Source:     "resource",
					ResourceID: resource.ID,
					Action:     "exclude",
					Reason:     action + " resource",
				})
			}
		case "bundle":
			if options.Preview && resource.Type == "server" && len(files) == 0 {
				decision.Reason = "server export and permission checks pending"
				break
			}
			if err := validateExport(resource, files); err != nil {
				return nil, err
			}
			included := make(map[string]bool, len(files))
			for _, file := range files {
				member, err := planner.member(file, "resource", resource.ID, resource.Type == "files")
				if err != nil {
					return nil, err
				}
				planner.resourceMembers = append(planner.resourceMembers, member)
				if member.Action == "include" {
					included[file.Path] = true
					if previous, exists := planner.resourcePermissions[file.Path]; exists &&
						previous != member.permissions {
						return nil, fmt.Errorf("publication permission collision at %q", file.Path)
					}
					planner.resourceFiles = append(planner.resourceFiles, file)
					planner.resourcePermissions[file.Path] = member.permissions
				}
			}
			for _, name := range resource.LicenseFiles {
				destination, err := resources.Destination(resource.Mount, name)
				if err != nil {
					return nil, err
				}
				if !included[destination] {
					return nil, fmt.Errorf(
						"license file %q for resource %q is missing or excluded",
						destination,
						resource.ID,
					)
				}
			}
		}
		planner.actions = append(planner.actions, decision)
	}
	planner.provided = cloneResources(planner.provided)
	sort.Slice(planner.actions, func(i, j int) bool { return planner.actions[i].ID < planner.actions[j].ID })
	return planner, nil
}

func validateExport(resource resources.Resource, files []projectarchive.File) error {
	if resource.Type == "texlive" {
		return fmt.Errorf(
			"resource %q: bundling TeX Live packages requires an explicit files resource with reviewed runtime files",
			resource.ID,
		)
	}
	if resource.Redistribution != "allow" {
		return fmt.Errorf("resource %q requires redistribution: allow before bundling", resource.ID)
	}
	if len(files) == 0 {
		return fmt.Errorf("resource %q has no captured export files", resource.ID)
	}
	if (resource.Kind == "font" || resource.Kind == "tex" || resource.Kind == "texmf") &&
		len(resource.LicenseFiles) == 0 {
		return fmt.Errorf("resource %q must declare licenseFiles before bundling", resource.ID)
	}
	expected := make(map[string]resources.File, len(resource.Files))
	for _, file := range resource.Files {
		destination, err := resources.Destination(resource.Mount, file.Path)
		if err != nil {
			return fmt.Errorf("resource %q: %w", resource.ID, err)
		}
		if _, exists := expected[destination]; exists {
			return fmt.Errorf("resource %q has a duplicate export member %q", resource.ID, destination)
		}
		expected[destination] = file
	}
	if len(files) != len(expected) {
		return fmt.Errorf("resource %q export does not match its manifest", resource.ID)
	}
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		manifest, ok := expected[file.Path]
		if !ok || seen[file.Path] || manifest.Size != file.Size || manifest.SHA256 != file.SHA256 {
			return fmt.Errorf("resource %q export member %q does not match its manifest", resource.ID, file.Path)
		}
		seen[file.Path] = true
	}
	return nil
}

// Provided returns the explicit baseline requirements, without resource injection.
func (p *Planner) Provided() []resources.Resource { return cloneResources(p.provided) }

func cloneResources(values []resources.Resource) []resources.Resource {
	result := slices.Clone(values)
	for i := range result {
		result[i].Files = slices.Clone(result[i].Files)
		result[i].Packages = slices.Clone(result[i].Packages)
		result[i].LicenseFiles = slices.Clone(result[i].LicenseFiles)
	}
	return result
}

func (p *Planner) member(file projectarchive.File, source, id string, permissionsAllowed bool) (Member, error) {
	reason := file.Reason
	if reason == "" {
		reason = source
	}
	member := Member{Path: file.Path, Size: file.Size, SHA256: file.SHA256,
		Source: source, ResourceID: id, Action: "include", Reason: reason}
	for _, rule := range p.exclusions {
		if doublestar.MatchUnvalidated(rule.pattern, file.Path) {
			if file.Path == p.entry {
				return Member{}, errors.New("pack exclusions cannot remove the entry")
			}
			member.Action, member.Reason = "exclude", rule.reason
			return member, nil
		}
	}
	keep, err := keepSource(file.Path, p.entry, p.mode)
	if err != nil {
		return Member{}, err
	}
	if !keep {
		member.Action, member.Reason = "exclude", p.mode+" packaging policy"
		return member, nil
	}
	member.permissions, member.PermissionPolicy = 0644, "normalized"
	switch {
	case !p.preservePermissions:
		member.PermissionReason = "permission preservation is disabled"
	case !permissionsAllowed:
		member.PermissionReason = source + " has no authorized original permission metadata"
	case file.SourcePermissions == nil:
		member.PermissionReason = "source does not expose portable POSIX permissions"
	default:
		member.permissions = file.SourcePermissions.Perm()
		member.PermissionPolicy, member.PermissionReason = "preserved", "captured POSIX rwx bits only"
	}
	member.FileMode = fmt.Sprintf("%04o", member.permissions)
	return member, nil
}

// Build applies the frozen policy to one complete set of source and generated
// members. It never edits an earlier plan, even when a later build fails.
func (p *Planner) Build(sources, generated []projectarchive.File) (Plan, error) {
	plan := Plan{PreservePermissions: p.preservePermissions, Permissions: make(map[string]os.FileMode),
		Members: slices.Clone(p.resourceMembers), Resources: slices.Clone(p.actions)}
	byPath := make(map[string]projectarchive.File, len(sources)+len(generated)+len(p.resourceFiles))
	add := func(file projectarchive.File, permissions os.FileMode) error {
		if old, exists := byPath[file.Path]; exists && (old.SHA256 != file.SHA256 || old.Size != file.Size ||
			plan.Permissions[file.Path] != permissions) {
			return fmt.Errorf("publication member collision at %q", file.Path)
		}
		byPath[file.Path] = file
		plan.Permissions[file.Path] = permissions
		return nil
	}
	for _, group := range []struct {
		source string
		files  []projectarchive.File
	}{{"project", sources}, {"generated", generated}} {
		for _, file := range group.files {
			member, err := p.member(file, group.source, "", group.source == "project")
			if err != nil {
				return Plan{}, err
			}
			plan.Members = append(plan.Members, member)
			if member.Action == "include" {
				if err := add(file, member.permissions); err != nil {
					return Plan{}, err
				}
			}
		}
	}
	for _, file := range p.resourceFiles {
		if err := add(file, p.resourcePermissions[file.Path]); err != nil {
			return Plan{}, err
		}
	}
	for _, file := range byPath {
		plan.Files = append(plan.Files, file)
	}
	sort.Slice(plan.Files, func(i, j int) bool { return plan.Files[i].Path < plan.Files[j].Path })
	sort.Slice(plan.Members, func(i, j int) bool {
		a, b := plan.Members[i], plan.Members[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.ResourceID != b.ResourceID {
			return a.ResourceID < b.ResourceID
		}
		return a.Source < b.Source
	})
	return plan, nil
}

// MembersDigest identifies only the final portable archive contents.
func MembersDigest(files []projectarchive.File, permissions map[string]os.FileMode) (string, error) {
	if err := validatePermissions(files, permissions); err != nil {
		return "", err
	}
	type member struct {
		Path        string `json:"path"`
		SHA256      string `json:"sha256"`
		Size        int64  `json:"size"`
		Permissions uint32 `json:"permissions"`
	}
	manifest := make([]member, 0, len(files))
	for _, file := range files {
		mode := memberPermissions(file.Path, permissions)
		manifest = append(
			manifest,
			member{Path: file.Path, SHA256: file.SHA256, Size: file.Size, Permissions: uint32(mode)},
		)
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Path < manifest[j].Path })
	return resources.Hash(manifest)
}
