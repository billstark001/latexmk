package resources

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/billstark001/latexmk/packages/shared/safefs"
)

const LockVersion = 1

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var packageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+._-]{0,127}$`)

func ValidID(value string) bool { return identifier.MatchString(value) }

func ValidDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func Hash(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// MountPath validates a resource directory, allowing the project root.
func MountPath(mount string) (string, error) {
	if mount == "" {
		return ".", nil
	}
	if mount != "." {
		clean, err := safefs.Clean(mount)
		if err != nil || clean != mount {
			return "", errors.New("resource mount must be a clean project-relative path")
		}
	}
	return mount, checkResourceNamespace(mount)
}

// Destination validates logical resource paths, including the private worker namespace.
func Destination(mount, name string) (string, error) {
	mount, err := MountPath(mount)
	if err != nil {
		return "", err
	}
	clean, err := safefs.Clean(name)
	if err != nil || clean != name {
		return "", errors.New("resource file must have a clean relative path")
	}
	result := path.Join(mount, clean)
	return result, checkResourceNamespace(clean)
}

func checkResourceNamespace(name string) error {
	for _, part := range strings.Split(name, "/") {
		lower := strings.ToLower(part)
		if strings.ContainsAny(part, "\r\n") || lower == ".git" || lower == ".env" ||
			strings.HasPrefix(lower, ".env.") || strings.HasPrefix(lower, ".latexmk") {
			return errors.New("resource path uses a private configuration or worker namespace")
		}
	}
	return nil
}

func NormalizeRequest(request ResolveRequest) (ResolveRequest, error) {
	var err error
	if request.Baseline != "" && !ValidID(request.Baseline) {
		return request, errors.New("invalid baseline ID")
	}
	request.Resources, err = normalizeResources(request.Resources)
	if err != nil {
		return request, err
	}
	request.Provided, err = normalizeResources(request.Provided)
	if err != nil {
		return request, err
	}
	seen := make(map[string]bool)
	for _, resource := range request.Resources {
		seen[resource.ID] = true
	}
	for _, resource := range request.Provided {
		if seen[resource.ID] {
			return request, fmt.Errorf("resource %q cannot be both injected and provided", resource.ID)
		}
	}
	request.Requires.Tools = sortedUnique(request.Requires.Tools)
	for _, name := range request.Requires.Tools {
		if !ValidID(name) {
			return request, errors.New("invalid required tool name")
		}
	}
	request.Requires.Fonts = slices.Clone(request.Requires.Fonts)
	slices.SortFunc(request.Requires.Fonts, func(a, b FontRequirement) int {
		return cmp.Or(strings.Compare(a.Family, b.Family), strings.Compare(a.Style, b.Style))
	})
	request.Requires.Fonts = slices.Compact(request.Requires.Fonts)
	for _, font := range request.Requires.Fonts {
		if strings.TrimSpace(font.Family) == "" || len(font.Family) > 256 || len(font.Style) > 256 ||
			strings.ContainsAny(font.Family+font.Style, "\x00\r\n") {
			return request, errors.New("invalid required font")
		}
	}
	return request, nil
}

func normalizeResources(values []Resource) ([]Resource, error) {
	if len(values) > 256 {
		return nil, errors.New("too many resources")
	}
	values = slices.Clone(values)
	seen := make(map[string]bool)
	destinations := make(map[string]File)
	for i := range values {
		value := &values[i]
		if !ValidID(value.ID) || seen[value.ID] {
			return nil, errors.New("resource IDs must be valid and unique")
		}
		seen[value.ID] = true
		switch value.Type {
		case "texlive":
			if len(value.Packages) == 0 || value.Ref != "" || len(value.Files) != 0 || value.Mount != "" {
				return nil, errors.New("texlive resource requires packages only")
			}
			value.Packages = sortedUnique(value.Packages)
			for _, name := range value.Packages {
				if !packageName.MatchString(name) {
					return nil, fmt.Errorf("invalid TeX Live package %q", name)
				}
			}
		case "files":
			if value.Ref != "" || len(value.Packages) != 0 || len(value.Files) == 0 {
				return nil, errors.New("file resource requires a nonempty file manifest")
			}
		case "server":
			if value.Ref == "" || len(value.Ref) > 256 || strings.ContainsAny(value.Ref, " /\\\x00\r\n") ||
				len(value.Packages) != 0 {
				return nil, errors.New("server resource requires a registered immutable reference")
			}
		default:
			return nil, fmt.Errorf("unknown resource type %q", value.Type)
		}
		if value.Kind != "" && value.Kind != "font" && value.Kind != "tex" && value.Kind != "data" &&
			value.Kind != "texmf" {
			return nil, errors.New("resource kind must be font, tex, texmf, or data")
		}
		if value.Redistribution == "" && value.Type != "server" {
			value.Redistribution = "unknown"
		}
		if value.Redistribution != "allow" && value.Redistribution != "deny" && value.Redistribution != "unknown" &&
			!(value.Type == "server" && value.Redistribution == "") {
			return nil, errors.New("redistribution must be allow, deny, or unknown")
		}
		if value.Type != "texlive" {
			var err error
			value.Mount, err = MountPath(value.Mount)
			if err != nil {
				return nil, err
			}
		}
		value.Files = slices.Clone(value.Files)
		slices.SortFunc(value.Files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
		fileNames := make(map[string]bool)
		for _, file := range value.Files {
			if fileNames[file.Path] || file.Size < 0 || !ValidDigest(file.SHA256) {
				return nil, errors.New("invalid or duplicate resource file")
			}
			fileNames[file.Path] = true
			destination, err := Destination(value.Mount, file.Path)
			if err != nil {
				return nil, err
			}
			if old, exists := destinations[destination]; exists &&
				(old.Size != file.Size || old.SHA256 != file.SHA256) {
				return nil, fmt.Errorf("resource destination collision: %s", destination)
			}
			destinations[destination] = file
		}
		value.LicenseFiles = sortedUnique(value.LicenseFiles)
		for _, name := range value.LicenseFiles {
			if !fileNames[name] {
				return nil, fmt.Errorf("license file %q is missing from resource %q", name, value.ID)
			}
		}
	}
	slices.SortFunc(values, func(a, b Resource) int { return strings.Compare(a.ID, b.ID) })
	return values, nil
}

func sortedUnique(values []string) []string {
	values = slices.Clone(values)
	slices.Sort(values)
	return slices.Compact(values)
}

// EnvironmentDigest excludes cache state and export permissions from compiler identity.
func EnvironmentDigest(environment Environment) (string, error) {
	request, err := NormalizeRequest(ResolveRequest{
		Baseline: environment.Baseline.ID, Resources: environment.Resources,
		Provided: environment.Provided, Requires: environment.Requires,
	})
	if err != nil {
		return "", err
	}
	for i := range request.Resources {
		request.Resources[i].Redistribution = ""
		request.Resources[i].LicenseFiles = nil
	}
	for i := range request.Provided {
		request.Provided[i].Redistribution = ""
		request.Provided[i].LicenseFiles = nil
	}
	return Hash(struct {
		Version  int
		Baseline string
		Request  ResolveRequest
		Packages []Package
	}{1, environment.Baseline.Digest, request, sortedPackages(environment.Packages)})
}
