package httpapi

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/billstark001/latexmk/packages/server/internal/auth"
	"github.com/billstark001/latexmk/packages/server/internal/resources"
	model "github.com/billstark001/latexmk/packages/shared/resources"
)

func (s *Server) resourceService(c *gin.Context) bool {
	if s.environments == nil {
		writeError(c, http.StatusServiceUnavailable, "resource environments are unavailable")
		return false
	}
	return true
}
func (s *Server) listEnvironments(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"baselines": s.environments.List(), "default": s.environments.DefaultID()})
}
func (s *Server) resolveEnvironment(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	var request model.ResolveRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<20)
	if err := decodeStrictJSON(c.Request.Body, 4<<20, &request); err != nil {
		writeError(c, http.StatusBadRequest, "invalid resource environment: "+err.Error())
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	ctx := resources.WithAcquisitionLogger(c.Request.Context(), s.logger, c.Writer.Header().Get("X-Request-ID"))
	env, err := s.environments.Resolve(ctx, principal.ID, request)
	if err != nil {
		s.resourceError(c, err)
		return
	}
	c.JSON(http.StatusOK, env)
}
func (s *Server) inspectEnvironment(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	env, err := s.environments.Inspect(principal.ID, c.Param("digest"))
	if err != nil {
		s.resourceError(c, err)
		return
	}
	c.JSON(http.StatusOK, env)
}
func (s *Server) listEnvironmentPackageArchives(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	manifest, err := s.environments.PackageArchives(c.Request.Context(), principal.ID, c.Param("digest"))
	if err != nil {
		s.resourceError(c, err)
		return
	}
	c.JSON(http.StatusOK, manifest)
}
func (s *Server) putEnvironmentPackageArchive(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	limit := s.cfg.MaxResourceBytes
	if limit == 0 {
		limit = 512 << 20
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	timeout := s.cfg.ResourcePrepareTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	// Context-aware readers cannot interrupt a blocked network Read themselves.
	// Bound the socket read as well as queue/lock waits and reset it for keepalive.
	controller := http.NewResponseController(c.Writer)
	uploadComplete := false
	if deadline, ok := ctx.Deadline(); ok {
		if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			s.resourceError(c, err)
			return
		}
		defer func() {
			// Leave a failed body's deadline in place: net/http may otherwise
			// block draining unread bytes before flushing the error response.
			if uploadComplete {
				_ = controller.SetReadDeadline(time.Time{})
			}
		}()
	}
	if err := s.environments.PutPackageArchive(ctx, principal.ID, c.Param("digest"),
		c.Param("sha512"), c.Request.ContentLength, c.Request.Body); err != nil {
		s.resourceError(c, err)
		return
	}
	uploadComplete = true
	c.Status(http.StatusNoContent)
}
func (s *Server) putEnvironmentBlob(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	limit := s.cfg.MaxResourceBytes
	if limit == 0 {
		limit = 512 << 20
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	if err := s.environments.PutBlob(
		c.Request.Context(),
		principal.ID,
		c.Param("digest"),
		c.Param("sha"),
		c.Request.Body,
	); err != nil {
		s.resourceError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
func (s *Server) prepareEnvironment(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	ctx := resources.WithAcquisitionLogger(c.Request.Context(), s.logger, c.Writer.Header().Get("X-Request-ID"))
	env, err := s.environments.Prepare(ctx, principal.ID, c.Param("digest"))
	if err != nil {
		s.resourceError(c, err)
		return
	}
	c.JSON(http.StatusOK, env)
}
func (s *Server) exportEnvironmentBlob(c *gin.Context) {
	if !s.resourceService(c) {
		return
	}
	principal, _ := auth.FromContext(c.Request.Context())
	file, metadata, err := s.environments.OpenExport(
		principal.ID,
		c.Param("digest"),
		c.Param("resourceID"),
		c.Param("sha"),
	)
	if err != nil {
		s.resourceError(c, err)
		return
	}
	defer func() { _ = file.Close() }()
	c.Header("X-Content-SHA256", metadata.SHA256)
	c.Header("Content-Length", strconv.FormatInt(metadata.Size, 10))
	c.DataFromReader(http.StatusOK, metadata.Size, "application/octet-stream", file, nil)
}
func (s *Server) resourceError(c *gin.Context, err error) {
	status := http.StatusBadRequest
	message := boundedResourceText(err.Error(), 512)
	var failure *model.AcquisitionError
	var maxBytes *http.MaxBytesError
	var pathError *fs.PathError
	switch {
	case errors.As(err, &failure):
		status = failure.HTTPStatus()
	case errors.Is(err, resources.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, resources.ErrExport):
		status = http.StatusForbidden
	case errors.Is(err, resources.ErrQuota):
		status = http.StatusRequestEntityTooLarge
		failure = model.AsAcquisitionError(
			err,
			"resource_quota_exceeded",
			"resource-prepare",
			"configured_quota",
			model.Package{},
		)
	case errors.Is(err, resources.ErrArchiveSize), errors.As(err, &maxBytes):
		status = http.StatusRequestEntityTooLarge
		failure = model.AsAcquisitionError(
			err,
			"resource_limit_exceeded",
			"archive-upload",
			"archive_bytes",
			model.Package{},
		)
	case errors.Is(err, resources.ErrArchiveChecksum):
		failure = model.AsAcquisitionError(
			err,
			"resource_archive_checksum",
			"archive-upload",
			"uploaded_checksum",
			model.Package{},
		)
	case errors.Is(err, resources.ErrNotReady):
		status = http.StatusConflict
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
		failure = model.AsAcquisitionError(
			err,
			"resource_prepare_timeout",
			"resource-prepare",
			"deadline",
			model.Package{},
		)
	case errors.Is(err, context.Canceled):
		status = http.StatusRequestTimeout
		failure = model.AsAcquisitionError(err, "resource_cancelled", "resource-prepare", "cancelled", model.Package{})
	case errors.As(err, &pathError):
		status = http.StatusInternalServerError
		failure = model.AsAcquisitionError(
			err,
			"resource_storage_failed",
			"resource-cache",
			"filesystem",
			model.Package{},
		)
	}
	if failure != nil {
		details := boundedResourceFailure(*failure)
		message = details.Error()
		if (details.UpstreamStatus == http.StatusTooManyRequests || details.UpstreamStatus == http.StatusServiceUnavailable) &&
			details.RetryAfter != "" {
			c.Header("Retry-After", details.RetryAfter)
		}
		if s.logger != nil {
			s.logger.Warn("resource operation failed", "request_id", c.Writer.Header().Get("X-Request-ID"),
				"code", details.Code, "stage", details.Stage, "reason", details.Reason,
				"package", details.Package, "source_id", details.SourceID, "upstream_status", details.UpstreamStatus)
		}
		c.AbortWithStatusJSON(
			status,
			gin.H{"error": message, "code": details.Code, "retryable": details.Retryable, "details": details},
		)
		return
	}
	writeError(c, status, message)
}

func boundedResourceText(value string, max int) string {
	runes := []rune(value)
	if len(runes) > max {
		runes = runes[:max]
	}
	for index, r := range runes {
		if r < 32 || r == 127 {
			runes[index] = ' '
		}
	}
	return string(runes)
}

func boundedResourceFailure(value model.AcquisitionError) model.AcquisitionError {
	value.Code = boundedResourceText(value.Code, 64)
	value.Stage = boundedResourceText(value.Stage, 64)
	value.Reason = boundedResourceText(value.Reason, 128)
	value.Package = boundedResourceText(value.Package, 128)
	value.SourceID = boundedResourceText(value.SourceID, 64)
	value.ContentType = boundedResourceText(value.ContentType, 128)
	value.RetryAfter = model.CanonicalRetryAfter(value.RetryAfter)
	value.Expected = boundedResourceText(value.Expected, 128)
	value.Actual = boundedResourceText(value.Actual, 128)
	value.Cause = nil
	if len(value.Attempts) > 8 {
		value.Attempts = value.Attempts[:8]
	}
	attempts := make([]model.AcquisitionAttempt, 0, len(value.Attempts))
	for _, attempt := range value.Attempts {
		attempt.SourceID = boundedResourceText(attempt.SourceID, 64)
		attempt.Code = boundedResourceText(attempt.Code, 64)
		attempt.Reason = boundedResourceText(attempt.Reason, 128)
		attempt.ContentType = boundedResourceText(attempt.ContentType, 128)
		attempt.RetryAfter = model.CanonicalRetryAfter(attempt.RetryAfter)
		attempts = append(attempts, attempt)
	}
	value.Attempts = attempts
	return value
}
