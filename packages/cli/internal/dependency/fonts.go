package dependency

import (
	"path"
	"strings"
)

func fontDefaults(d *discoverer, source string, call invocation) {
	options := parseOptions([]string{call.args[0]})
	if len(call.options) == 0 {
		if call.star {
			options = append(append([]option(nil), d.context.fontDefaults...), options...)
		}
		d.context.fontDefaults = options
		return
	}
	// Copy on write so defaults obey surrounding TeX groups.
	values := make(map[string][]option, len(d.context.fontNamedDefaults))
	for key, value := range d.context.fontNamedDefaults {
		values[key] = value
	}
	for _, name := range splitOptions(normalizeArgument(call.options[0])) {
		name = unwrap(name)
		if !literalReference(name) {
			d.addDiagnostic(
				source,
				call.line,
				call.name,
				name,
				"dynamic",
				"font defaults require a literal family name",
			)
			continue
		}
		merged := options
		if call.star {
			merged = append(append([]option(nil), values[name]...), options...)
		}
		values[name] = merged
	}
	d.context.fontNamedDefaults = values
}

func fontInput(d *discoverer, source string, call invocation) {
	pattern := commandRegistry[call.name]
	base := normalizeArgument(call.args[pattern.reference])
	if !literalReference(base) {
		d.addDiagnostic(
			source,
			call.line,
			call.name,
			base,
			"dynamic",
			"font name must be literal; use explicit manifest selection",
		)
		return
	}
	explicit := parseOptions(call.options)
	if _, ignore := optionValues(explicit)["IgnoreFontspecFile"]; !ignore {
		d.consumeReference(
			source,
			call.line,
			call.name,
			base,
			referenceRule{suffix: ".fontspec", recursive: true, missing: optionalFile},
		)
	}
	options := append([]option(nil), d.context.fontDefaults...)
	options = append(options, d.context.fontNamedDefaults[base]...)
	options = append(options, explicit...)
	values := optionValues(options)
	for _, face := range fontFaceRegistry {
		name, present := values[face.font]
		if !present && face.font == "UprightFont" {
			name = base
		}
		if name == "" {
			continue
		}
		name = strings.ReplaceAll(name, "*", base)
		features := optionValues(
			append(append([]option(nil), options...), parseOptions([]string{values[face.features]})...),
		)
		// Nested face features can select a different font and directory.
		if selected := features["Font"]; selected != "" {
			name = strings.ReplaceAll(selected, "*", base)
		}
		dir, hasPath := features["Path"]
		extension := features["Extension"]
		if !literalReference(name) {
			d.addDiagnostic(
				source,
				call.line,
				call.name,
				name,
				"dynamic",
				"font face must be literal; use explicit manifest selection",
			)
			continue
		}
		isFile := hasPath || extension != "" || strings.Contains(name, "/") ||
			containsString(fontExtensions, strings.ToLower(path.Ext(name)))
		if !isFile {
			continue
		} // Named system fonts have no project-file obligation.
		if !literalReference(name) || dir != "" && !literalReference(dir) ||
			extension != "" && !literalReference(extension) {
			d.addDiagnostic(
				source,
				call.line,
				call.name,
				name,
				"dynamic",
				"font filename, Path and Extension must be literal",
			)
			continue
		}
		if extension != "" && !strings.HasSuffix(name, extension) {
			name += extension
		}
		if strings.HasPrefix(dir, "/") || strings.HasPrefix(name, "/") || strings.Contains(dir, ":") ||
			strings.Contains(name, ":") {
			d.addDiagnostic(
				source,
				call.line,
				call.name,
				dir+name,
				"outside_root",
				"font path escapes the project root",
			)
			continue
		}
		d.consumeReference(
			source,
			call.line,
			call.name,
			path.Join(dir, name),
			referenceRule{extensions: fontExtensions},
		)
	}
}
