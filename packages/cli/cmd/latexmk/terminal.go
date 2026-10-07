package main

import "strings"

// Compiler output is untrusted text. Preserve ordinary UTF-8, tabs and newlines,
// but never pass terminal commands, carriage-return rewrites or C1 controls.
func terminalText(value string) string {
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f) {
			return '\ufffd'
		}
		return r
	}, value)
}
