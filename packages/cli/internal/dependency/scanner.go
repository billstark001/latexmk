package dependency

import "strings"

type invocation struct {
	name      string
	args      []string
	argLines  []int
	options   []string
	line      int
	malformed bool
	star      bool
	syntax    syntaxKind
	body      string
	bodyLine  int
	generated bool
	plotMode  string
}

// scanInvocations preserves source lines and group events. It recognizes only
// registered syntax; unknown formatting commands do not generate diagnostics.
func scanInvocations(text string) []invocation {
	raw := text
	text = sanitize(text)
	var result []invocation
	line := 1
	for i := 0; i < len(text); {
		start := i
		if text[i] == '\n' {
			line++
			i++
			continue
		}
		if text[i] == '{' || text[i] == '}' {
			kind := scopeOpenSyntax
			if text[i] == '}' {
				kind = scopeCloseSyntax
			}
			result = append(result, invocation{line: line, syntax: kind})
			i++
			continue
		}
		if text[i] != '\\' {
			i++
			continue
		}
		name, next := controlSequence(text, i)
		i = next
		pattern, known := commandRegistry[name]
		if !known {
			continue
		}
		call := invocation{name: name, line: line, syntax: pattern.syntax}
		cursor := i
		if cursor < len(text) && (text[cursor] == '*' || (pattern.syntax == plotSyntax && text[cursor] == '+')) {
			call.star = text[cursor] == '*'
			cursor++
		}
		cursor = skipSpace(text, cursor)
		if pattern.syntax == scopeOpenSyntax || pattern.syntax == scopeCloseSyntax ||
			pattern.syntax == conditionalSyntax {
			result = append(result, call)
			line += strings.Count(text[start:cursor], "\n")
			i = cursor
			continue
		}
		cursor = scanOptions(text, cursor, &call)
		if pattern.syntax == plotSyntax && !call.malformed {
			wordStart := cursor
			for cursor < len(text) && asciiLetter(text[cursor]) {
				cursor++
			}
			call.plotMode = text[wordStart:cursor]
			if call.plotMode != "table" && call.plotMode != "file" {
				// Consume the whole plot so inline coordinates/functions are never
				// interpreted as TeX dependency loaders or groups.
				cursor = plotEnd(text, cursor)
				line += strings.Count(text[start:cursor], "\n")
				i = cursor
				continue
			}
			cursor = scanOptions(text, skipSpace(text, cursor), &call)
			arg, end, ok := scanArgument(text, cursor, inputArgument)
			call.args = []string{arg}
			call.argLines = []int{line + strings.Count(text[start:cursor], "\n")}
			call.malformed = call.malformed || !ok
			cursor = end
		} else {
			for _, kind := range pattern.arguments {
				if call.malformed {
					break
				}
				call.argLines = append(call.argLines, line+strings.Count(text[start:cursor], "\n"))
				arg, end, ok := scanArgument(text, cursor, kind)
				call.args = append(call.args, arg)
				call.malformed = !ok
				cursor = skipSpace(text, end)
				// Font family declarations accept options after the control sequence.
				if kind == controlArgument {
					cursor = scanOptions(text, cursor, &call)
				}
			}
		}
		if pattern.trailingOptions && !call.malformed {
			cursor = scanOptions(text, cursor, &call)
		}
		if pattern.syntax == environmentSyntax && !call.malformed {
			env := normalizeArgument(call.args[0])
			if pattern.opensScope {
				if spec, ok := environmentRegistry[env]; ok {
					call.generated = spec.generated
					if spec.generated {
						cursor = scanOptions(text, cursor, &call)
						arg, end, ok := scanArgument(text, cursor, bracedArgument)
						call.args = append(call.args, arg)
						call.malformed = call.malformed || !ok
						cursor = end
					}
					token := "\\end{" + env + "}"
					end := strings.Index(raw[cursor:], token)
					call.bodyLine = line + strings.Count(text[start:cursor], "\n")
					if end < 0 {
						call.malformed = true
						call.body = raw[cursor:]
						cursor = len(text)
					} else {
						call.body = raw[cursor : cursor+end]
						cursor += end + len(token)
					}
					if !spec.generated {
						line += strings.Count(text[start:cursor], "\n")
						i = cursor
						continue
					}
				}
			}
		}
		result = append(result, call)
		line += strings.Count(text[start:cursor], "\n")
		i = cursor
	}
	return result
}

func scanOptions(text string, cursor int, call *invocation) int {
	for cursor < len(text) && text[cursor] == '[' && !call.malformed {
		value, next, ok := balanced(text, cursor, '[', ']')
		if !ok {
			call.malformed = true
			return len(text)
		}
		call.options = append(call.options, value)
		cursor = skipSpace(text, next)
	}
	return cursor
}

func scanArgument(text string, cursor int, kind argumentKind) (string, int, bool) {
	cursor = skipSpace(text, cursor)
	if cursor >= len(text) {
		return "", cursor, false
	}
	if text[cursor] == '{' {
		value, next, ok := balanced(text, cursor, '{', '}')
		if !ok {
			return "", len(text), false
		}
		return value, next, true
	}
	if kind == controlArgument && text[cursor] == '\\' {
		_, next := controlSequence(text, cursor)
		return text[cursor:next], next, true
	}
	if kind != inputArgument {
		return "", cursor, false
	}
	// Literal unbraced input ends at TeX whitespace or a control sequence. A
	// leading control sequence is retained for an actionable dynamic diagnostic.
	if text[cursor] == '\\' {
		_, next := controlSequence(text, cursor)
		return text[cursor:next], next, true
	}
	end := cursor
	for end < len(text) && !strings.ContainsRune(" \t\r\n\\{};", rune(text[end])) {
		if text[end] == 0 {
			// TeX removes comment continuations, including the following newline.
			for end < len(text) && text[end] == 0 {
				end++
			}
			if end < len(text) && text[end] == '\n' {
				end++
			}
			continue
		}
		end++
	}
	return text[cursor:end], end, end > cursor
}

func controlSequence(text string, start int) (string, int) {
	end := start + 1
	if end >= len(text) {
		return "", end
	}
	if !asciiLetter(text[end]) && text[end] != '@' {
		return text[end : end+1], end + 1
	}
	for end < len(text) && (asciiLetter(text[end]) || text[end] == '@') {
		end++
	}
	// Registered numeric syntax suffixes are consumed with their control word.
	if end < len(text) && text[end] >= '0' && text[end] <= '9' {
		if _, registered := commandRegistry[text[start+1:end+1]]; registered {
			end++
		}
	}
	return text[start+1 : end], end
}

func asciiLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func plotEnd(text string, cursor int) int {
	for cursor < len(text) {
		if text[cursor] == ';' {
			return cursor + 1
		}
		if text[cursor] == '\\' {
			cursor += 2
			continue
		}
		if text[cursor] == '{' {
			_, next, ok := balanced(text, cursor, '{', '}')
			if !ok {
				return len(text)
			}
			cursor = next
			continue
		}
		cursor++
	}
	return len(text)
}
