package compile

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/api"
)

// ArtifactKind classifies generated artifacts; it is never used to delete inputs.
func ArtifactKind(name string) string {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".synctex.gz") {
		return "synctex"
	}
	switch filepath.Ext(lower) {
	case ".pdf":
		return "output"
	case ".log", ".blg", ".ilg", ".glg":
		return "diagnostic"
	default:
		return "auxiliary"
	}
}

// RetainArtifacts runs after recorder/diagnostic extraction. Transfer-only auxiliary
// files have a short expiry; final outputs and logs retain the normal job lifetime.
func RetainArtifacts(output Output, req api.CompileRequest, limit time.Duration) Output {
	retained := output
	retained.Files = nil
	retained.Result.Artifacts = nil
	var expires *time.Time
	if req.Auxiliary.Server == "none" || req.Auxiliary.Server == "" {
		if req.Auxiliary.Local == "cache" || req.Auxiliary.Local == "output" {
			ttl := 15 * time.Minute
			if limit > 0 && limit < ttl {
				ttl = limit
			}
			value := time.Now().UTC().Add(ttl)
			expires = &value
		}
	} else {
		ttl := limit
		if req.Auxiliary.ServerTTL != "" {
			requested, err := time.ParseDuration(req.Auxiliary.ServerTTL)
			if err == nil && (ttl <= 0 || requested < ttl) {
				ttl = requested
			}
		}
		if ttl > 0 {
			value := time.Now().UTC().Add(ttl)
			expires = &value
		}
	}
	retained.Result.AuxiliaryExpiresAt = expires
	for _, file := range output.Files {
		kind := ArtifactKind(file.RelativePath)
		if kind == "auxiliary" && expires == nil && (req.Auxiliary.Server == "none" || req.Auxiliary.Server == "") {
			continue
		}
		retained.Files = append(retained.Files, file)
		artifact := api.Artifact{Path: file.RelativePath, Size: file.Size, SHA256: file.SHA256}
		artifact.Kind = kind
		retained.Result.Artifacts = append(retained.Result.Artifacts, artifact)
	}
	return retained
}
