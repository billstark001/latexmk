// Package dependency selects literal LaTeX dependencies from an already
// policy-filtered project manifest. It never reads outside that manifest.
package dependency

import (
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
)

const maxParsedFileSize = 8 << 20

type Diagnostic struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Command   string `json:"command"`
	Reference string `json:"reference,omitempty"`
	Kind      string `json:"kind"`
	Message   string `json:"message"`
}

type Result struct {
	Files       []projectarchive.File
	Stats       projectarchive.Stats
	Diagnostics []Diagnostic
	Resolved    bool
}

// Select applies an upload mode to a policy-filtered candidate manifest.
func Select(entry, mode string, candidates []projectarchive.File) (Result, error) {
	switch mode {
	case "", "auto":
		return Discover(entry, candidates)
	case "all":
		files := append([]projectarchive.File(nil), candidates...)
		stats := projectarchive.Stats{}
		for i := range files {
			files[i].Reason = "upload mode all"
			stats.Files++
			stats.Bytes += files[i].Size
		}
		return Result{Files: files, Stats: stats, Resolved: true}, nil
	default:
		return Result{}, fmt.Errorf("unsupported upload mode %q", mode)
	}
}

func FormatDiagnostic(diagnostic Diagnostic) string {
	location := diagnostic.File
	if diagnostic.Line > 0 {
		location = fmt.Sprintf("%s:%d", location, diagnostic.Line)
	}
	reference := diagnostic.Reference
	if diagnostic.Command != "" {
		reference = "\\" + diagnostic.Command + "{" + diagnostic.Reference + "}"
	}
	if reference != "" {
		return fmt.Sprintf("%s: %s: %s", location, reference, diagnostic.Message)
	}
	return fmt.Sprintf("%s: %s", location, diagnostic.Message)
}

type commandSpec struct {
	argCount   int
	reference  int
	extensions []string
	recursive  bool
	optional   bool
	splitComma bool
	graphics   bool
}

var commandSpecs = map[string]commandSpec{
	"input":             {argCount: 1, reference: 0, extensions: []string{"", ".tex"}, recursive: true},
	"include":           {argCount: 1, reference: 0, extensions: []string{"", ".tex"}, recursive: true},
	"subfile":           {argCount: 1, reference: 0, extensions: []string{"", ".tex"}, recursive: true},
	"loadglsentries":    {argCount: 1, reference: 0, extensions: []string{"", ".tex"}, recursive: true},
	"includegraphics":   {argCount: 1, reference: 0, extensions: []string{".pdf", ".png", ".jpg", ".jpeg", ".eps", ".mps"}, graphics: true},
	"includepdf":        {argCount: 1, reference: 0, extensions: []string{".pdf"}},
	"includesvg":        {argCount: 1, reference: 0, extensions: []string{".svg"}},
	"bibliography":      {argCount: 1, reference: 0, extensions: []string{".bib"}, splitComma: true},
	"addbibresource":    {argCount: 1, reference: 0, extensions: []string{".bib"}},
	"documentclass":     {argCount: 1, reference: 0, extensions: []string{".cls"}, recursive: true, optional: true},
	"usepackage":        {argCount: 1, reference: 0, extensions: []string{".sty"}, recursive: true, optional: true, splitComma: true},
	"bibliographystyle": {argCount: 1, reference: 0, extensions: []string{".bst"}, optional: true},
	"lstinputlisting":   {argCount: 1, reference: 0, extensions: []string{""}},
	"verbatiminput":     {argCount: 1, reference: 0, extensions: []string{""}},
	"VerbatimInput":     {argCount: 1, reference: 0, extensions: []string{""}},
	"inputminted":       {argCount: 2, reference: 1, extensions: []string{""}},
	"DTLloaddb":         {argCount: 2, reference: 1, extensions: []string{""}},
	"pgfplotstableread": {argCount: 1, reference: 0, extensions: []string{""}},
}

type invocation struct {
	name      string
	args      []string
	line      int
	malformed bool
}

type discoverer struct {
	candidates  map[string]projectarchive.File
	selected    map[string]projectarchive.File
	visiting    map[string]bool
	parsed      map[string]bool
	diagnostics []Diagnostic
	graphicDirs []string
}

// Discover returns the dependency closure rooted at entry. Candidates must
// already have passed Git-ignore, denylist, symlink, size, and path checks.
func Discover(entry string, candidates []projectarchive.File) (Result, error) {
	entry = cleanProjectPath(entry)
	if entry == "" {
		return Result{}, errors.New("entry path is outside the project root")
	}
	d := discoverer{
		candidates: make(map[string]projectarchive.File, len(candidates)),
		selected:   make(map[string]projectarchive.File),
		visiting:   make(map[string]bool),
		parsed:     make(map[string]bool),
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

	files := make([]projectarchive.File, 0, len(d.selected))
	stats := projectarchive.Stats{}
	for _, file := range d.selected {
		files = append(files, file)
		stats.Files++
		stats.Bytes += file.Size
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return Result{Files: files, Stats: stats, Diagnostics: d.diagnostics, Resolved: len(d.diagnostics) == 0}, nil
}

func (d *discoverer) visit(filePath, reason string) error {
	file, ok := d.candidates[filePath]
	if !ok {
		return fmt.Errorf("dependency %q is not present in the allowed manifest", filePath)
	}
	if existing, selected := d.selected[filePath]; !selected {
		file.Reason = reason
		d.selected[filePath] = file
	} else if existing.Reason == "" {
		existing.Reason = reason
		d.selected[filePath] = existing
	}
	if d.parsed[filePath] || d.visiting[filePath] {
		return nil
	}
	d.visiting[filePath] = true
	defer delete(d.visiting, filePath)

	if file.Size > maxParsedFileSize {
		d.addDiagnostic(filePath, 0, "", "", "too_large", fmt.Sprintf("text dependency exceeds the %d-byte static parser limit", maxParsedFileSize))
		return nil
	}
	content, err := os.ReadFile(file.Source)
	if err != nil {
		return fmt.Errorf("read dependency %s: %w", filePath, err)
	}
	text := sanitize(string(content))
	for _, call := range scanInvocations(text) {
		if call.name == "graphicspath" {
			d.consumeGraphicPath(filePath, call)
			continue
		}
		spec, ok := commandSpecs[call.name]
		if !ok {
			continue
		}
		if call.malformed || len(call.args) < spec.argCount {
			d.addDiagnostic(filePath, call.line, call.name, "", "unsupported", "expected a braced literal argument")
			continue
		}
		references := []string{strings.TrimSpace(call.args[spec.reference])}
		if spec.splitComma {
			references = strings.Split(call.args[spec.reference], ",")
		}
		for _, reference := range references {
			d.consumeReference(filePath, call.line, call.name, strings.TrimSpace(reference), spec)
		}
	}
	d.parsed[filePath] = true
	return nil
}

func (d *discoverer) consumeReference(source string, line int, command, reference string, spec commandSpec) {
	if !literalReference(reference) {
		d.addDiagnostic(source, line, command, reference, "dynamic", "dependency uses a macro or non-literal path")
		return
	}
	if cleanProjectPath(reference) == "" {
		d.addDiagnostic(source, line, command, reference, "outside_root", "dependency path escapes the project root")
		return
	}
	search := []string{reference}
	if spec.graphics {
		search = make([]string, 0, len(d.graphicDirs)+1)
		search = append(search, reference)
		for _, dir := range d.graphicDirs {
			search = append(search, path.Join(dir, reference))
		}
	}
	resolved := ""
	for _, base := range search {
		if candidate := d.resolve(base, spec.extensions); candidate != "" {
			resolved = candidate
			break
		}
	}
	if resolved == "" {
		if spec.optional && !strings.Contains(reference, "/") {
			return
		}
		d.addDiagnostic(source, line, command, reference, "unavailable", "dependency is missing, ignored by Git, or denied by the upload policy")
		return
	}
	reason := fmt.Sprintf("\\%s from %s:%d", command, source, line)
	if spec.recursive {
		if err := d.visit(resolved, reason); err != nil {
			d.addDiagnostic(source, line, command, reference, "unavailable", err.Error())
		}
		return
	}
	file := d.candidates[resolved]
	file.Reason = reason
	if _, exists := d.selected[resolved]; !exists {
		d.selected[resolved] = file
	}
}

func (d *discoverer) resolve(reference string, extensions []string) string {
	base := cleanProjectPath(reference)
	if base == "" {
		return ""
	}
	if path.Ext(base) != "" {
		if _, ok := d.candidates[base]; ok {
			return base
		}
		return ""
	}
	for _, extension := range extensions {
		candidate := base + extension
		if _, ok := d.candidates[candidate]; ok {
			return candidate
		}
	}
	return ""
}

func (d *discoverer) consumeGraphicPath(source string, call invocation) {
	if call.malformed || len(call.args) != 1 {
		d.addDiagnostic(source, call.line, call.name, "", "unsupported", "expected \\graphicspath{{dir/}{dir/}}")
		return
	}
	dirs, ok := bracedList(call.args[0])
	if !ok {
		d.addDiagnostic(source, call.line, call.name, call.args[0], "dynamic", "graphic paths must be literal braced directories")
		return
	}
	for _, dir := range dirs {
		clean := cleanProjectPath(dir)
		if clean == "" {
			d.addDiagnostic(source, call.line, call.name, dir, "outside_root", "graphic path escapes the project root")
			continue
		}
		if clean == "." {
			clean = ""
		}
		if !containsString(d.graphicDirs, clean) {
			d.graphicDirs = append(d.graphicDirs, clean)
		}
	}
}

func (d *discoverer) addDiagnostic(file string, line int, command, reference, kind, message string) {
	d.diagnostics = append(d.diagnostics, Diagnostic{File: file, Line: line, Command: command, Reference: reference, Kind: kind, Message: message})
}

func scanInvocations(text string) []invocation {
	result := make([]invocation, 0)
	line := 1
scanLoop:
	for i := 0; i < len(text); {
		if text[i] == '\n' {
			line++
			i++
			continue
		}
		if text[i] != '\\' {
			i++
			continue
		}
		start, startLine := i, line
		i++
		nameStart := i
		for i < len(text) && ((text[i] >= 'A' && text[i] <= 'Z') || (text[i] >= 'a' && text[i] <= 'z') || text[i] == '@') {
			i++
		}
		if nameStart == i {
			continue
		}
		name := text[nameStart:i]
		if name != "graphicspath" {
			if _, ok := commandSpecs[name]; !ok {
				continue
			}
		}
		cursor := i
		if cursor < len(text) && text[cursor] == '*' {
			cursor++
		}
		cursor = skipSpace(text, cursor)
		for cursor < len(text) && text[cursor] == '[' {
			_, next, ok := balanced(text, cursor, '[', ']')
			if !ok {
				result = append(result, invocation{name: name, line: startLine, malformed: true})
				line += strings.Count(text[start:cursor], "\n")
				i = cursor + 1
				continue scanLoop
			}
			cursor = skipSpace(text, next)
		}
		argCount := 1
		if spec, ok := commandSpecs[name]; ok {
			argCount = spec.argCount
		}
		call := invocation{name: name, line: startLine}
		for len(call.args) < argCount {
			if cursor >= len(text) || text[cursor] != '{' {
				call.malformed = true
				break
			}
			arg, next, ok := balanced(text, cursor, '{', '}')
			if !ok {
				call.malformed = true
				cursor = len(text)
				break
			}
			call.args = append(call.args, arg)
			cursor = skipSpace(text, next)
		}
		result = append(result, call)
		line += strings.Count(text[start:cursor], "\n")
		i = cursor
	}
	return result
}

func balanced(text string, start int, open, close byte) (string, int, bool) {
	depth := 0
	for i := start; i < len(text); i++ {
		switch text[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return text[start+1 : i], i + 1, true
			}
		}
	}
	return "", start, false
}

func skipSpace(text string, at int) int {
	for at < len(text) && (text[at] == ' ' || text[at] == '\t' || text[at] == '\r' || text[at] == '\n') {
		at++
	}
	return at
}

func sanitize(text string) string {
	bytes := []byte(text)
	for i := 0; i < len(bytes); i++ {
		if bytes[i] != '%' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && bytes[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			continue
		}
		for i < len(bytes) && bytes[i] != '\n' {
			bytes[i] = ' '
			i++
		}
	}
	text = string(bytes)
	for _, environment := range []string{"verbatim", "verbatim*", "Verbatim", "lstlisting", "minted"} {
		text = maskEnvironment(text, environment)
	}
	text = maskInlineVerb(text, "\\lstinline")
	text = maskInlineVerb(text, "\\verb")
	return text
}

func maskInlineVerb(text, token string) string {
	for search := 0; ; {
		relative := strings.Index(text[search:], token)
		if relative < 0 {
			return text
		}
		start := search + relative
		cursor := start + len(token)
		if cursor < len(text) && ((text[cursor] >= 'A' && text[cursor] <= 'Z') || (text[cursor] >= 'a' && text[cursor] <= 'z')) {
			search = cursor
			continue
		}
		if cursor < len(text) && text[cursor] == '*' {
			cursor++
		}
		if token == "\\lstinline" && cursor < len(text) && text[cursor] == '[' {
			_, next, ok := balanced(text, cursor, '[', ']')
			if !ok {
				return text
			}
			cursor = next
		}
		if cursor >= len(text) || text[cursor] == '\n' || text[cursor] == '\r' {
			search = cursor
			continue
		}
		delimiter := text[cursor]
		endRelative := strings.IndexByte(text[cursor+1:], delimiter)
		if endRelative < 0 {
			return text
		}
		end := cursor + 1 + endRelative + 1
		masked := []byte(text)
		for i := start; i < end; i++ {
			if masked[i] != '\n' {
				masked[i] = ' '
			}
		}
		text = string(masked)
		search = end
	}
}

func maskEnvironment(text, environment string) string {
	startToken := "\\begin{" + environment + "}"
	endToken := "\\end{" + environment + "}"
	for search := 0; ; {
		startRel := strings.Index(text[search:], startToken)
		if startRel < 0 {
			return text
		}
		start := search + startRel
		endRel := strings.Index(text[start+len(startToken):], endToken)
		end := len(text)
		if endRel >= 0 {
			end = start + len(startToken) + endRel + len(endToken)
		}
		masked := []byte(text)
		for i := start; i < end; i++ {
			if masked[i] != '\n' {
				masked[i] = ' '
			}
		}
		text = string(masked)
		if endRel < 0 {
			return text
		}
		search = end
	}
}

func bracedList(value string) ([]string, bool) {
	result := make([]string, 0)
	for cursor := skipSpace(value, 0); cursor < len(value); cursor = skipSpace(value, cursor) {
		if value[cursor] != '{' {
			return nil, false
		}
		item, next, ok := balanced(value, cursor, '{', '}')
		if !ok || !literalReference(item) {
			return nil, false
		}
		result = append(result, strings.TrimSpace(item))
		cursor = next
	}
	return result, len(result) > 0
}

func literalReference(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && !strings.ContainsAny(value, "\\#$%{}~")
}

func cleanProjectPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(strings.Split(value, "/")[0], ":") {
		return ""
	}
	clean := path.Clean(strings.TrimPrefix(value, "./"))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return ""
	}
	return clean
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
