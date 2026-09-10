package dependency

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
)

const maxDiscoveryVisits = 20_000

type scanContext struct {
	importDirs        []string
	graphicDirs       []string
	graphicExtensions []string
	fontDefaults      []option
	fontNamedDefaults map[string][]option
	svgOptions        []option
	svgDirs           []string
}

type generatedFile struct {
	content string
	source  string
	line    int
}

type discoverer struct {
	candidates     map[string]projectarchive.File
	selected       map[string]projectarchive.File
	visiting       map[string]bool
	text           map[string]string
	generated      map[string]generatedFile
	assets         map[string]bool
	diagnostics    []Diagnostic
	context        scanContext
	scopes         []scanContext
	forwarded      map[string][]option
	moduleOptions  []option
	globalOptions  []option
	loaded         map[string]bool
	visits         int
	assetDepth     int
	uncertainDepth int
}

// Discover walks registered literal dependencies exclusively through candidates.
// Text is cached, but execution is repeated in the caller's path and group context.
func Discover(entry string, candidates []projectarchive.File) (Result, error) {
	entry = cleanProjectPath(entry)
	if entry == "" {
		return Result{}, errors.New("entry path is outside the project root")
	}
	d := discoverer{
		candidates: make(map[string]projectarchive.File), selected: make(map[string]projectarchive.File),
		visiting: make(map[string]bool), text: make(map[string]string), generated: make(map[string]generatedFile),
		assets: make(map[string]bool), forwarded: make(map[string][]option), loaded: make(map[string]bool),
	}
	for _, file := range candidates {
		d.candidates[file.Path] = file
	}
	if _, ok := d.candidates[entry]; !ok {
		return Result{}, fmt.Errorf("entry %q is missing, ignored, or denied by the upload policy", entry)
	}
	if err := d.visit(entry, "entry file"); err != nil {
		return Result{}, err
	}
	result := Result{Diagnostics: d.diagnostics, Resolved: len(d.diagnostics) == 0}
	for _, file := range d.selected {
		result.Files = append(result.Files, file)
		result.Stats.Files++
		result.Stats.Bytes += file.Size
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	return result, nil
}

func (d *discoverer) visit(filePath, reason string) error {
	if !d.selectFile(filePath, reason) {
		return fmt.Errorf("dependency %q is not present in the allowed manifest", filePath)
	}
	if d.visiting[filePath] {
		return nil
	}
	if d.visits >= maxDiscoveryVisits || len(d.visiting) >= 256 {
		d.addDiagnostic(
			filePath,
			0,
			"",
			"",
			"limit",
			"static dependency traversal limit reached; use explicit manifest selection",
		)
		return nil
	}
	d.visits++
	d.visiting[filePath] = true
	defer delete(d.visiting, filePath)
	if generated, ok := d.generated[filePath]; ok {
		d.scan(generated.source, generated.content, generated.line)
		return nil
	}
	content, err := d.readText(filePath)
	if err != nil {
		return err
	}
	d.scan(filePath, content, 1)
	return nil
}

func (d *discoverer) readText(filePath string) (string, error) {
	if content, ok := d.text[filePath]; ok {
		return content, nil
	}
	file, ok := d.candidates[filePath]
	if !ok {
		return "", fmt.Errorf("dependency %q is not present in the allowed manifest", filePath)
	}
	if file.Size > maxParsedFileSize {
		d.addDiagnostic(
			filePath,
			0,
			"",
			"",
			"too_large",
			fmt.Sprintf("text dependency exceeds the %d-byte static parser limit", maxParsedFileSize),
		)
		return "", nil
	}
	content, err := projectarchive.ReadFile(file, maxParsedFileSize)
	if err != nil {
		return "", fmt.Errorf("read dependency %s: %w", filePath, err)
	}
	d.text[filePath] = string(content)
	return string(content), nil
}

func (d *discoverer) selectFile(filePath, reason string) bool {
	if _, ok := d.generated[filePath]; ok {
		return true
	}
	file, ok := d.candidates[filePath]
	if !ok {
		return false
	}
	if _, exists := d.selected[filePath]; !exists {
		file.Reason = reason
		d.selected[filePath] = file
	}
	return true
}

type condition struct{ parent, active, known, truth bool }

func (d *discoverer) scan(source, text string, firstLine int) {
	inheritedUncertainty := d.uncertainDepth
	defer func() { d.uncertainDepth = inheritedUncertainty }()
	var conditions []condition
	active := true
	for _, call := range scanInvocations(text) {
		d.uncertainDepth = inheritedUncertainty
		for _, condition := range conditions {
			if !condition.known {
				d.uncertainDepth++
			}
		}
		call.line += firstLine - 1
		call.bodyLine += firstLine - 1
		for i := range call.argLines {
			call.argLines[i] += firstLine - 1
		}
		if call.syntax == conditionalSyntax {
			action := commandRegistry[call.name].condition
			switch action {
			case conditionalEnd:
				if len(conditions) > 0 {
					active = conditions[len(conditions)-1].parent
					conditions = conditions[:len(conditions)-1]
				}
			case conditionalAlternative:
				if len(conditions) > 0 {
					c := &conditions[len(conditions)-1]
					c.active = c.parent && (!c.known || !c.truth)
					active = c.active
				}
			default:
				c := condition{
					parent: active,
					known:  action != conditionalUnknown,
					truth:  action == conditionalTrue,
				}
				c.active = c.parent && (!c.known || c.truth)
				conditions = append(conditions, c)
				active = c.active
			}
			continue
		}
		if !active {
			continue
		}
		if call.malformed {
			d.addDiagnostic(
				source,
				call.line,
				call.name,
				"",
				"unsupported",
				"expected literal arguments in the registered syntax; use includeFiles with manifest mode for computed syntax",
			)
			continue
		}
		switch call.syntax {
		case scopeOpenSyntax:
			d.pushScope()
			continue
		case scopeCloseSyntax:
			d.popScope()
			continue
		case environmentSyntax:
			if call.generated {
				if d.uncertainDepth > 0 {
					d.addDiagnostic(
						source,
						call.line,
						call.name,
						call.args[1],
						"dynamic",
						"conditional generated contents require explicit manifest selection",
					)
				}
				d.generate(source, call)
			} else if commandRegistry[call.name].opensScope {
				d.pushScope()
			} else {
				d.popScope()
			}
			continue
		}
		pattern := commandRegistry[call.name]
		if d.uncertainDepth > 0 && (pattern.changesState || pattern.loadKind != "") {
			d.addDiagnostic(
				source,
				call.line,
				call.name,
				"",
				"dynamic",
				"conditional changes to dependency settings require explicit manifest selection",
			)
		}
		if pattern.handle != nil {
			pattern.handle(d, source, call)
			continue
		}
		references := []string{normalizeArgument(call.args[pattern.reference])}
		if pattern.splitComma {
			references = splitOptions(references[0])
		}
		for _, reference := range references {
			d.consumeReference(source, call.line, call.name, strings.TrimSpace(reference), pattern.rule)
		}
	}
}

func (d *discoverer) pushScope() { d.scopes = append(d.scopes, d.context) }
func (d *discoverer) popScope() {
	if len(d.scopes) > 0 {
		d.context = d.scopes[len(d.scopes)-1]
		d.scopes = d.scopes[:len(d.scopes)-1]
	}
}

func (d *discoverer) exists(filePath string) bool {
	_, generated := d.generated[filePath]
	_, candidate := d.candidates[filePath]
	return generated || candidate
}

func (d *discoverer) consumeReference(source string, line int, command, reference string, rule referenceRule) string {
	if !literalReference(reference) {
		d.addDiagnostic(
			source,
			line,
			command,
			reference,
			"dynamic",
			"dependency uses a macro or non-literal path; declare inputs and use manifest mode",
		)
		return ""
	}
	resolved, unsafe := d.resolveReference(reference, rule)
	if unsafe {
		d.addDiagnostic(source, line, command, reference, "outside_root", "dependency path escapes the project root")
		return ""
	}
	if resolved == "" {
		if rule.missing == optionalFile || rule.missing == systemFile && !strings.Contains(reference, "/") {
			return ""
		}
		d.addDiagnostic(
			source,
			line,
			command,
			reference,
			"unavailable",
			"dependency is missing, ignored by Git, or denied by the upload policy",
		)
		return ""
	}
	reason := fmt.Sprintf("\\%s from %s:%d", command, source, line)
	if rule.recursive {
		if err := d.visit(resolved, reason); err != nil {
			d.addDiagnostic(source, line, command, reference, "unavailable", err.Error())
		}
	} else {
		d.selectFile(resolved, reason)
		if handler := assetRegistry[strings.ToLower(path.Ext(resolved))]; handler != nil && !d.assets[resolved] {
			if d.assetDepth >= 256 || len(d.assets) >= maxDiscoveryVisits {
				d.addDiagnostic(
					source,
					line,
					command,
					reference,
					"limit",
					"companion asset traversal limit reached; use explicit manifest selection",
				)
				return resolved
			}
			d.assets[resolved] = true
			d.assetDepth++
			handler(d, resolved)
			d.assetDepth--
		}
	}
	return resolved
}

// Search extensions before directories, as graphics does. Import prepends its
// active directory to the search path, while ordinary inputs remain root-relative.
func (d *discoverer) resolveReference(reference string, rule referenceRule) (string, bool) {
	if strings.HasPrefix(reference, "/") || strings.Contains(strings.Split(reference, "/")[0], ":") {
		return "", true
	}
	search := append([]string(nil), d.context.importDirs...)
	if rule.rootRelative {
		search = nil
	}
	search = append(search, "")
	if rule.graphics {
		search = append(search, d.context.graphicDirs...)
	}
	exts := rule.extensions
	if rule.graphics && len(d.context.graphicExtensions) > 0 && len(rule.extensions) > 1 {
		exts = d.context.graphicExtensions
	}
	if rule.suffix != "" {
		reference += rule.suffix
		exts = []string{""}
	} else if path.Ext(reference) != "" {
		exts = []string{""}
	}
	if len(exts) == 0 {
		exts = []string{""}
	}
	valid := false
	for _, ext := range exts {
		for _, dir := range search {
			candidate := cleanProjectPath(path.Join(dir, reference+ext))
			if candidate == "" {
				continue
			}
			valid = true
			if d.exists(candidate) {
				return candidate, false
			}
		}
	}
	return "", !valid
}

func (d *discoverer) addDiagnostic(file string, line int, command, reference, kind, message string) {
	diagnostic := Diagnostic{
		File:      file,
		Line:      line,
		Command:   command,
		Reference: reference,
		Kind:      kind,
		Message:   message,
	}
	for _, existing := range d.diagnostics {
		if existing == diagnostic {
			return
		}
	}
	d.diagnostics = append(d.diagnostics, diagnostic)
}
