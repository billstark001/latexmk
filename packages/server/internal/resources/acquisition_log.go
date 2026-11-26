package resources

import (
	"context"
	"log/slog"

	model "github.com/billstark001/latexmk/packages/shared/resources"
)

type acquisitionLogKey struct{}

type acquisitionLog struct {
	logger    *slog.Logger
	requestID string
}

// WithAcquisitionLogger associates transport/cache provenance with its API
// request. Resource operations remain usable without an HTTP request or logger.
func WithAcquisitionLogger(ctx context.Context, logger *slog.Logger, requestID string) context.Context {
	if logger == nil {
		return ctx
	}
	return context.WithValue(ctx, acquisitionLogKey{}, acquisitionLog{logger: logger, requestID: requestID})
}

func logArchiveAcquisition(
	ctx context.Context,
	pkg model.Package,
	cacheHit bool,
	result model.ArchiveFetchResult,
	err error,
) {
	log, ok := ctx.Value(acquisitionLogKey{}).(acquisitionLog)
	if !ok {
		return
	}
	level, code := slog.LevelInfo, "ok"
	if err != nil {
		level = slog.LevelWarn
		failure := model.AsAcquisitionError(err, "resource_storage_failed", "archive-cache", "cache", pkg)
		code = failure.Code
		if len(result.Attempts) == 0 {
			result.Attempts = failure.Attempts
		}
		if result.SourceID == "" {
			result.SourceID = failure.SourceID
		}
	}
	if cacheHit {
		result.SourceID = "cache"
	}
	// All fields are public identities or transport-produced enums. There are
	// no URLs, local paths, raw causes, response bodies or process output.
	log.logger.Log(ctx, level, "resource archive acquisition",
		"request_id", log.requestID, "package", pkg.Name, "revision", pkg.Revision,
		"sha512", pkg.Checksum, "cache_hit", cacheHit, "source_id", result.SourceID,
		"bytes", result.Bytes, "code", code, "attempts", result.Attempts)
}

func logIndexAcquisition(ctx context.Context, baseline model.Baseline, sourceID string, err error) {
	log, ok := ctx.Value(acquisitionLogKey{}).(acquisitionLog)
	if !ok {
		return
	}
	level, code := slog.LevelInfo, "ok"
	if err != nil {
		level = slog.LevelWarn
		code = model.AsAcquisitionError(
			err,
			"resource_index_invalid",
			"metadata-resolve",
			"index",
			model.Package{},
		).Code
	}
	log.logger.Log(ctx, level, "resource index acquisition", "request_id", log.requestID,
		"baseline_digest", baseline.Digest, "texlive_year", baseline.TexliveYear,
		"source_id", sourceID, "code", code)
}
