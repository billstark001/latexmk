package resources

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/shared/jsonutil"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	model "github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

// Registry files are administrator-controlled. Directory and authorization
// policy never leave the service; clients receive only immutable manifests.
type catalogResource struct {
	Ref            string       `json:"ref"`
	Directory      string       `json:"directory"`
	Kind           string       `json:"kind,omitempty"`
	Redistribution string       `json:"redistribution"`
	LicenseFiles   []string     `json:"licenseFiles,omitempty"`
	Owners         []string     `json:"owners,omitempty"`
	Files          []model.File `json:"files,omitempty"`
}

type registry struct {
	Default   string            `json:"default,omitempty"`
	Baselines []model.Baseline  `json:"baselines,omitempty"`
	Resources []catalogResource `json:"resources,omitempty"`
}

func defaultBaseline(cfg config.Config, meta protocol.Metadata) (model.Baseline, error) {
	baseline := model.Baseline{
		ID:         "default",
		Image:      cfg.RunnerImage,
		Engines:    slices.Clone(cfg.Engines),
		Toolchain:  maps.Clone(meta.Toolchain),
		FontLookup: "names",
	}
	dir := cfg.ResourceInventoryDir
	if dir == "" {
		dir = "/usr/local/lib/latexmk"
	}
	if data, err := os.ReadFile(filepath.Join(dir, "texlive-repository.txt")); err == nil {
		baseline.Repository = strings.TrimSpace(string(data))
	} else if !errors.Is(err, os.ErrNotExist) {
		return baseline, err
	}
	if match := regexp.MustCompile(`/(20[0-9]{2})/`).FindStringSubmatch(baseline.Repository); len(match) > 1 {
		baseline.TexliveYear, _ = strconv.Atoi(match[1])
	}
	if file, err := os.Open(filepath.Join(dir, "texlive.tlpdb")); err == nil {
		packages, parseErr := parseTLPDB(file)
		_ = file.Close()
		if parseErr != nil {
			return baseline, parseErr
		}
		for _, pkg := range packages {
			baseline.Packages = append(baseline.Packages, model.Package{Name: pkg.Name, Revision: pkg.Revision})
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return baseline, err
	}
	slices.SortFunc(baseline.Packages, func(a, b model.Package) int { return strings.Compare(a.Name, b.Name) })
	hashes := map[string]string{}
	if file, err := os.Open(filepath.Join(dir, "fonts.sha256")); err == nil {
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if len(line) > 66 && model.ValidDigest(line[:64]) {
				hashes[strings.TrimSpace(line[64:])] = line[:64]
			}
		}
		err = scanner.Err()
		_ = file.Close()
		if err != nil {
			return baseline, err
		}
	}
	if file, err := os.Open(filepath.Join(dir, "fonts.tsv")); err == nil {
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			columns := strings.Split(scanner.Text(), "\t")
			if len(columns) < 5 || columns[0] == "file" {
				continue
			}
			index, _ := strconv.Atoi(columns[1])
			baseline.Fonts = append(
				baseline.Fonts,
				model.Font{
					File:       filepath.Base(columns[0]),
					SHA256:     hashes[columns[0]],
					Index:      index,
					Family:     columns[2],
					Style:      columns[3],
					PostScript: columns[4],
				},
			)
		}
		err = scanner.Err()
		_ = file.Close()
		if err != nil {
			return baseline, err
		}
	}
	digest, err := model.BaselineDigest(baseline)
	baseline.Digest = digest
	return baseline, err
}

func validateProvided(baseline model.Baseline, values []model.Resource) error {
	installed := make(map[string]bool, len(baseline.Packages))
	for _, pkg := range baseline.Packages {
		installed[pkg.Name] = true
	}
	for _, resource := range values {
		if resource.Type != "texlive" {
			return fmt.Errorf(
				"provided resource %q cannot be proved at its exact path in this baseline; use explicit baseline font/tool requirements or bundle the files",
				resource.ID,
			)
		}
		for _, name := range resource.Packages {
			if !installed[name] {
				return fmt.Errorf("baseline does not provide TeX Live package %q", name)
			}
		}
	}
	return nil
}

func (m *Manager) loadRegistry(file string) error {
	f, err := safefs.OpenRegularFile(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var input registry
	if err := jsonutil.DecodeStrict(f, 16<<20, &input); err != nil {
		return fmt.Errorf("resource registry: %w", err)
	}
	if len(input.Baselines) > 64 || len(input.Resources) > 256 {
		return errors.New("resource registry has too many entries")
	}
	if len(input.Baselines) > 0 && m.cfg.RunnerImage == "" {
		return errors.New(
			"registered image baselines require an isolated controller deployment with LATEXMK_RUNNER_IMAGE",
		)
	}
	for _, baseline := range input.Baselines {
		for _, font := range baseline.Fonts {
			if filepath.IsAbs(font.File) || strings.ContainsAny(font.File, "\\\x00\r\n") {
				return errors.New("baseline font filenames must be logical relative names")
			}
		}
		if !model.ValidID(baseline.ID) || len(baseline.Engines) == 0 {
			return errors.New("invalid registered baseline")
		}
		if _, exists := m.baselines[baseline.ID]; exists {
			return errors.New("duplicate baseline ID")
		}
		if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]*@sha256:[a-f0-9]{64}$`).MatchString(baseline.Image) {
			return errors.New("registered baseline requires a digest-pinned runner image")
		}
		if baseline.FontLookup == "" {
			baseline.FontLookup = "names"
		}
		if baseline.FontLookup != "names" && baseline.FontLookup != "files" {
			return errors.New("fontLookup must be names or files")
		}
		actual, err := model.BaselineDigest(baseline)
		if err != nil {
			return err
		}
		if baseline.Digest != "" && baseline.Digest != actual {
			return fmt.Errorf("registered baseline %q digest does not match its runtime inventory", baseline.ID)
		}
		baseline.Digest = actual
		m.baselines[baseline.ID] = baseline
	}
	if input.Default != "" {
		if _, ok := m.baselines[input.Default]; !ok {
			return errors.New("unknown default baseline")
		}
		m.defaultID = input.Default
	}
	for _, value := range input.Resources {
		if value.Ref == "" || strings.ContainsAny(value.Ref, " /\\\x00\r\n") || !filepath.IsAbs(value.Directory) {
			return errors.New("registered resource requires a ref and absolute directory")
		}
		if _, ok := m.catalog[value.Ref]; ok {
			return errors.New("duplicate registered resource")
		}
		if value.Redistribution == "" {
			value.Redistribution = "unknown"
		}
		files, err := inventoryFiles(value.Directory, m.cfg.MaxResourceBytes, m.cfg.MaxResourceFiles)
		if err != nil {
			return fmt.Errorf("registered resource %s: %w", value.Ref, err)
		}
		if len(value.Files) > 0 {
			if !slices.Equal(value.Files, files) {
				return errors.New("registered resource manifest does not match directory")
			}
		}
		value.Files = files
		probe := model.Resource{
			ID:             "registry",
			Type:           "server",
			Ref:            value.Ref,
			Kind:           value.Kind,
			Files:          files,
			LicenseFiles:   value.LicenseFiles,
			Redistribution: value.Redistribution,
		}
		if _, err = model.NormalizeRequest(model.ResolveRequest{Resources: []model.Resource{probe}}); err != nil {
			return err
		}
		m.catalog[value.Ref] = value
	}
	return nil
}

func inventoryFiles(dir string, maxBytes int64, maxFiles int) ([]model.File, error) {
	root, err := safefs.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var files []model.File
	var total int64
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if len(files) >= maxFiles {
			return ErrQuota
		}
		f, err := root.OpenRegular(name)
		if err != nil {
			return err
		}
		sha, size, err := safefs.Digest(f, maxBytes-total)
		_ = f.Close()
		if err != nil {
			return err
		}
		total += size
		files = append(files, model.File{Path: filepath.ToSlash(name), SHA256: sha, Size: size})
		return nil
	})
	return files, err
}

func (m *Manager) expand(owner string, values []model.Resource) ([]model.Resource, error) {
	values = copyResources(values)
	for i := range values {
		value := &values[i]
		if value.Type != "server" {
			continue
		}
		registered, ok := m.catalog[value.Ref]
		if !ok {
			return nil, errors.New("unknown server resource")
		}
		if len(registered.Owners) > 0 && !slices.Contains(registered.Owners, owner) {
			return nil, ErrNotFound
		}
		if len(value.Files) > 0 {
			if !slices.Equal(value.Files, registered.Files) {
				return nil, errors.New("server resource manifest changed")
			}
		}
		if value.Kind != "" && value.Kind != registered.Kind {
			return nil, errors.New("server resource kind cannot be overridden")
		}
		value.Kind = registered.Kind
		value.Files = slices.Clone(registered.Files)
		value.LicenseFiles = slices.Clone(registered.LicenseFiles)
		value.Redistribution = restrictRedistribution(value.Redistribution, registered.Redistribution)
	}
	return values, nil
}

func restrictRedistribution(project, registered string) string {
	if project == "" {
		return registered
	}
	if project == "deny" || registered == "deny" {
		return "deny"
	}
	if project == "unknown" || registered == "unknown" {
		return "unknown"
	}
	return "allow"
}

func (m *Manager) importCatalogLocked(registered catalogResource, file model.File) error {
	if m.hasBlob(file) {
		return nil
	}
	if err := m.reserveLocked(file.Size); err != nil {
		return err
	}
	defer func() { m.reserved -= file.Size }()
	root, err := safefs.Open(registered.Directory)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	source, err := root.OpenRegular(file.Path)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	temp, err := os.CreateTemp(filepath.Join(m.dir, "staging"), "catalog-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	err = safefs.CopyVerified(temp, source, file.Size, file.SHA256)
	if err == nil {
		err = temp.Sync()
	}
	err = errors.Join(err, temp.Close())
	if err != nil {
		return err
	}
	if err = os.Chmod(name, 0444); err != nil {
		return err
	}
	return os.Rename(name, m.blobPath(file.SHA256))
}
