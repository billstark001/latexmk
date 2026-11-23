// Package resources defines content-addressed compilation resources and environments.
package resources

// Spec declares a resource in project configuration. Local paths never cross the wire.
type Spec struct {
	Type           string   `json:"type"`
	Kind           string   `json:"kind,omitempty"`
	Path           string   `json:"path,omitempty"`
	Mount          string   `json:"mount,omitempty"`
	Packages       []string `json:"packages,omitempty"`
	Ref            string   `json:"ref,omitempty"`
	Include        []string `json:"include,omitempty"`
	Exclude        []string `json:"exclude,omitempty"`
	LicenseFiles   []string `json:"licenseFiles,omitempty"`
	Redistribution string   `json:"redistribution,omitempty"`
}

type Options struct {
	LockFile       string `json:"lockFile,omitempty"`
	LockMode       string `json:"lockMode,omitempty"`
	PrepareTimeout string `json:"prepareTimeout,omitempty"`
}

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Resource is an explicit, transportable requirement with resource-relative files.
type Resource struct {
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	Kind           string   `json:"kind,omitempty"`
	Mount          string   `json:"mount,omitempty"`
	Packages       []string `json:"packages,omitempty"`
	Ref            string   `json:"ref,omitempty"`
	Files          []File   `json:"files,omitempty"`
	LicenseFiles   []string `json:"licenseFiles,omitempty"`
	Redistribution string   `json:"redistribution,omitempty"`
}

type FontRequirement struct {
	Family string `json:"family"`
	Style  string `json:"style,omitempty"`
}

type Requirements struct {
	Fonts []FontRequirement `json:"fonts,omitempty"`
	Tools []string          `json:"tools,omitempty"`
}

type Font struct {
	File       string `json:"file"`
	SHA256     string `json:"sha256,omitempty"`
	Index      int    `json:"index,omitempty"`
	Family     string `json:"family,omitempty"`
	Style      string `json:"style,omitempty"`
	PostScript string `json:"postscript,omitempty"`
}

type Package struct {
	Name     string `json:"name"`
	Revision int    `json:"revision"`
	Checksum string `json:"checksum,omitempty"`
}

// Baseline describes a registered runtime; paths and credentials stay server-side.
type Baseline struct {
	ID          string            `json:"id"`
	Digest      string            `json:"digest"`
	Image       string            `json:"image,omitempty"`
	TexliveYear int               `json:"texliveYear,omitempty"`
	Repository  string            `json:"repository,omitempty"`
	Engines     []string          `json:"engines"`
	Toolchain   map[string]string `json:"toolchain,omitempty"`
	FontLookup  string            `json:"fontLookup,omitempty"`
	Fonts       []Font            `json:"fonts,omitempty"`
	Packages    []Package         `json:"packages,omitempty"`
}

type ResolveRequest struct {
	Baseline  string       `json:"baseline,omitempty"`
	Resources []Resource   `json:"resources,omitempty"`
	Provided  []Resource   `json:"provided,omitempty"`
	Requires  Requirements `json:"requires,omitempty"`
}

// Environment binds a job to exact bytes. Ready/Missing describe cache availability.
type Environment struct {
	Digest    string       `json:"digest"`
	Baseline  Baseline     `json:"baseline"`
	Resources []Resource   `json:"resources,omitempty"`
	Provided  []Resource   `json:"provided,omitempty"`
	Requires  Requirements `json:"requires,omitempty"`
	Packages  []Package    `json:"packages,omitempty"`
	Ready     bool         `json:"ready"`
	Missing   []string     `json:"missing,omitempty"`
}

type LockedPlan struct {
	SpecDigest  string      `json:"specDigest"`
	Environment Environment `json:"environment"`
}

type Lock struct {
	Version int                   `json:"version"`
	Plans   map[string]LockedPlan `json:"plans"`
}

type Publication struct {
	Target              string            `json:"target"`
	PreservePermissions bool              `json:"preservePermissions,omitempty"`
	Mode                string            `json:"mode,omitempty"`
	Environment         string            `json:"environment,omitempty"`
	Resources           map[string]string `json:"resources,omitempty"`
	IncludeFiles        []string          `json:"includeFiles,omitempty"`
	Exclude             []string          `json:"exclude,omitempty"`
	Verify              string            `json:"verify,omitempty"`
	Output              string            `json:"output,omitempty"`
}

type Verification struct {
	Status            string `json:"status"`
	Scope             string `json:"scope"`
	EnvironmentID     string `json:"environmentId,omitempty"`
	EnvironmentDigest string `json:"environmentDigest,omitempty"`
	MembersDigest     string `json:"membersDigest,omitempty"`
	JobID             string `json:"jobId,omitempty"`
}
