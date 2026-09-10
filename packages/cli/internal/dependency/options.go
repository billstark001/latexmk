package dependency

import "strings"

// splitOptions splits only top-level commas. Braced values and escaped
// delimiters belong to their option, even when they contain commas or brackets.
func splitOptions(value string) []string {
	var options []string
	start := 0
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\':
			i++
		case '{':
			_, next, ok := balanced(value, i, '{', '}')
			if !ok {
				return append(options, value[start:])
			}
			i = next - 1
		case ',':
			options = append(options, value[start:i])
			start = i + 1
		}
	}
	return append(options, value[start:])
}

type option struct{ key, value string }

func parseOptions(groups []string) []option {
	var result []option
	for _, group := range groups {
		for _, field := range splitOptions(normalizeArgument(group)) {
			key, value, _ := strings.Cut(field, "=")
			key = strings.TrimSpace(key)
			if key != "" {
				result = append(result, option{key, unwrap(value)})
			}
		}
	}
	return result
}

func unwrap(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "{") {
		if inner, next, ok := balanced(value, 0, '{', '}'); ok && next == len(value) {
			return strings.TrimSpace(inner)
		}
	}
	return value
}

func optionValues(options []option) map[string]string {
	values := make(map[string]string, len(options))
	for _, item := range options {
		values[item.key] = item.value
	}
	return values
}
