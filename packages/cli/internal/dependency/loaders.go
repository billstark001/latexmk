package dependency

import (
	"path"
	"strings"
)

func forwardOptions(d *discoverer, source string, call invocation) {
	pattern := commandRegistry[call.name]
	for _, name := range splitOptions(normalizeArgument(call.args[1])) {
		name = strings.TrimSpace(name)
		if !literalReference(name) {
			d.addDiagnostic(
				source,
				call.line,
				call.name,
				name,
				"dynamic",
				"package name must be literal; use explicit manifest selection",
			)
			continue
		}
		key := pattern.loadKind + ":" + name
		d.forwarded[key] = append(d.forwarded[key], parseOptions([]string{call.args[0]})...)
	}
}

func loadModule(d *discoverer, source string, call invocation) {
	pattern := commandRegistry[call.name]
	for _, name := range splitOptions(normalizeArgument(call.args[0])) {
		name = strings.TrimSpace(name)
		key := pattern.loadKind + ":" + name
		if d.loaded[key] {
			continue
		}
		options := append([]option(nil), d.forwarded[key]...)
		if pattern.inheritOptions {
			options = append(options, d.moduleOptions...)
		} else {
			options = append(options, parseOptions(call.options)...)
		}
		if pattern.globalOptions {
			d.globalOptions = append([]option(nil), options...)
		}
		extension := moduleExtensions[pattern.loadKind]
		d.loaded[key] = true
		previous := d.moduleOptions
		d.moduleOptions = options
		d.consumeReference(
			source,
			call.line,
			call.name,
			name,
			referenceRule{extensions: []string{extension}, recursive: true, missing: systemFile},
		)
		d.moduleOptions = previous
		if pattern.loadKind == "package" {
			if handler := packageRegistry[name]; handler != nil {
				combined := append(append([]option(nil), d.globalOptions...), options...)
				handler(d, source, call, combined)
			}
		}
	}
}

// The early data-model pass and the later style pass have different precedence.
func biblatexOptions(d *discoverer, source string, call invocation, options []option) {
	values := optionValues(options)
	model := values["datamodel"]
	if model != "" {
		d.consumeReference(source, call.line, call.name, model, referenceRule{suffix: ".dbx", recursive: true})
	} else {
		models := []string{values["style"]}
		if models[0] == "" {
			models = []string{values["citestyle"], values["bibstyle"]}
		}
		for _, name := range models {
			if name != "" {
				d.consumeReference(
					source,
					call.line,
					call.name,
					name,
					referenceRule{suffix: ".dbx", recursive: true, missing: optionalFile},
				)
			}
		}
	}
	styles := map[string]string{".bbx": "numeric", ".cbx": "numeric"}
	for _, item := range options {
		for _, rule := range biblatexStyleRegistry[item.key] {
			styles[rule] = item.value
		}
	}
	for _, extension := range []string{".bbx", ".cbx"} {
		if name := styles[extension]; name != "" {
			d.consumeReference(
				source,
				call.line,
				call.name,
				name,
				referenceRule{suffix: extension, recursive: true, missing: systemFile},
			)
		}
	}
}

func importFile(d *discoverer, source string, call invocation) {
	dir := normalizeArgument(call.args[0])
	file := normalizeArgument(call.args[1])
	if dir == "" {
		dir = "."
	}
	if !literalReference(dir) || !literalReference(file) {
		d.addDiagnostic(
			source,
			call.line,
			call.name,
			dir+file,
			"dynamic",
			"import directory and filename must be literal; use explicit manifest selection",
		)
		return
	}
	if strings.HasPrefix(dir, "/") || strings.HasPrefix(file, "/") || strings.Contains(dir, ":") ||
		strings.Contains(file, ":") {
		d.addDiagnostic(source, call.line, call.name, dir+file, "outside_root", "import path escapes the project root")
		return
	}
	previous := d.context.importDirs
	if commandRegistry[call.name].relativeImport && len(previous) > 0 {
		dir = path.Join(previous[0], dir)
	}
	dir = cleanProjectPath(dir)
	if dir == "" || cleanProjectPath(path.Join(dir, file)) == "" {
		d.addDiagnostic(
			source,
			call.line,
			call.name,
			call.args[0]+file,
			"outside_root",
			"import path escapes the project root",
		)
		return
	}
	d.context.importDirs = append([]string{dir}, previous...)
	d.consumeReference(
		source,
		call.line,
		call.name,
		path.Join(dir, file),
		referenceRule{extensions: []string{"", ".tex"}, recursive: true, rootRelative: true},
	)
	d.context.importDirs = previous
}

func conditionalInput(d *discoverer, source string, call invocation) {
	pattern := commandRegistry[call.name]
	reference := normalizeArgument(call.args[0])
	rule := pattern.rule
	rule.recursive = false
	resolved := d.consumeReference(source, call.line, call.name, reference, rule)
	branch := 2
	if resolved != "" {
		branch = 1
	}
	if !literalReference(reference) {
		for _, index := range []int{1, 2} {
			d.pushScope()
			d.scan(source, call.args[index], call.argLines[index])
			d.popScope()
		}
		return
	}
	d.scan(source, call.args[branch], call.argLines[branch])
	if resolved != "" && pattern.rule.recursive {
		if err := d.visit(resolved, "optional input from "+source); err != nil {
			d.addDiagnostic(source, call.line, call.name, reference, "unavailable", err.Error())
		}
	}
}

func graphicsPath(d *discoverer, source string, call invocation) {
	value := normalizeArgument(call.args[0])
	if value == "" {
		d.context.graphicDirs = nil
		return
	}
	dirs, ok := bracedList(value)
	if !ok {
		d.addDiagnostic(
			source,
			call.line,
			call.name,
			value,
			"dynamic",
			"graphic paths must be literal braced directories",
		)
		return
	}
	var paths []string
	for _, dir := range dirs {
		clean := cleanProjectPath(dir)
		if clean == "" {
			d.addDiagnostic(source, call.line, call.name, dir, "outside_root", "graphic path escapes the project root")
			continue
		}
		paths = append(paths, clean)
	}
	d.context.graphicDirs = paths
}

func graphicsExtensions(d *discoverer, source string, call invocation) {
	var extensions []string
	for _, value := range splitOptions(normalizeArgument(call.args[0])) {
		value = strings.TrimSpace(value)
		if !literalReference(value) || !strings.HasPrefix(value, ".") || strings.Contains(value, "/") {
			d.addDiagnostic(
				source,
				call.line,
				call.name,
				value,
				"dynamic",
				"graphics extensions must be literal dot-prefixed suffixes",
			)
			return
		}
		extensions = append(extensions, value)
	}
	d.context.graphicExtensions = extensions
}

func tableInput(d *discoverer, source string, call invocation) {
	reference := normalizeArgument(call.args[0])
	if call.plotMode != "file" {
		if strings.HasPrefix(reference, "\\") {
			_, next := controlSequence(reference, 0)
			if next == len(reference) {
				return
			}
		}
		if strings.ContainsAny(reference, "\n\r") || strings.Contains(reference, `\\`) {
			return
		}
		if strings.ContainsAny(reference, " \t") && path.Ext(reference) == "" && !strings.Contains(reference, "/") {
			return
		}
	}
	d.consumeReference(source, call.line, call.name, reference, referenceRule{extensions: []string{""}})
}

func dynamicCode(d *discoverer, source string, call invocation) {
	d.addDiagnostic(
		source,
		call.line,
		call.name,
		"",
		"dynamic",
		"Lua file access cannot be evaluated statically; declare inputs and use manifest mode",
	)
}

func (d *discoverer) generate(source string, call invocation) {
	name := normalizeArgument(call.args[1])
	if !literalReference(name) {
		d.addDiagnostic(source, call.line, call.name, name, "dynamic", "generated filename must be literal")
		return
	}
	name = cleanProjectPath(name)
	if name == "" {
		d.addDiagnostic(
			source,
			call.line,
			call.name,
			call.args[1],
			"outside_root",
			"generated path escapes the project root",
		)
		return
	}
	values := optionValues(parseOptions(call.options))
	_, overwrite := values["overwrite"]
	if _, exists := d.candidates[name]; exists && !overwrite {
		return
	}
	d.generated[name] = generatedFile{content: call.body, source: source, line: call.bodyLine}
}
