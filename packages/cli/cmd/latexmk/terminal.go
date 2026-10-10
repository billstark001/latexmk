package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

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

// Human-readable output passes through one filter, including filenames, server
// metadata and errors. JSON encoders retain their original machine-readable data.
func terminalFprintf(writer io.Writer, format string, args ...any) {
	_, _ = fmt.Fprint(writer, terminalText(fmt.Sprintf(format, args...)))
}

func terminalPrintf(format string, args ...any) { terminalFprintf(os.Stdout, format, args...) }
func terminalFprintln(writer io.Writer, args ...any) {
	terminalFprintf(writer, "%s", fmt.Sprintln(args...))
}
func terminalPrintln(args ...any) { terminalFprintln(os.Stdout, args...) }
func terminalPrint(args ...any)   { terminalFprintf(os.Stdout, "%s", fmt.Sprint(args...)) }
