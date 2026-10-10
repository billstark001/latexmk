package protocol

import (
	"strings"
	"unicode"
)

// ValidBearerToken reports whether a nonempty opaque credential can be carried
// as a single bearer field. Whitespace and control characters are rejected;
// punctuation and non-ASCII characters do not change the credential's identity.
// Callers deciding whether authentication is optional must handle empty values.
func ValidBearerToken(token string) bool {
	return token != "" &&
		!strings.ContainsFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}
