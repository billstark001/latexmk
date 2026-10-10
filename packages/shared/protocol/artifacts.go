package protocol

import (
	"path"
	"strings"
)

// ClassifyArtifactPath returns the kind used for retention and legacy downloads
// when explicit artifact metadata is unavailable. Extension comparison is case
// insensitive. It does not validate paths or authorize deleting source files.
func ClassifyArtifactPath(name string) string {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".synctex.gz") {
		return "synctex"
	}
	switch path.Ext(lower) {
	case ".pdf":
		return "output"
	case ".log", ".blg", ".ilg", ".glg":
		return "diagnostic"
	default:
		return "auxiliary"
	}
}
