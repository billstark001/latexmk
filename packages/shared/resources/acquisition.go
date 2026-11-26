package resources

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// AcquisitionAttempt contains only public identities and bounded diagnostics.
// Transport URLs, local paths, response bodies and process output are excluded.
type AcquisitionAttempt struct {
	SourceID       string `json:"sourceId"`
	Code           string `json:"code"`
	Reason         string `json:"reason,omitempty"`
	UpstreamStatus int    `json:"upstreamStatus,omitempty"`
	ContentType    string `json:"contentType,omitempty"`
	RetryAfter     string `json:"retryAfter,omitempty"`
	Bytes          int64  `json:"bytes,omitempty"`
}

// AcquisitionError describes a resource failure without exposing its raw cause.
// Cause remains available to local errors.Is/As checks through Unwrap.
type AcquisitionError struct {
	Code            string               `json:"code"`
	Stage           string               `json:"stage"`
	Reason          string               `json:"reason,omitempty"`
	Package         string               `json:"package,omitempty"`
	Revision        int                  `json:"revision,omitempty"`
	SourceID        string               `json:"sourceId,omitempty"`
	UpstreamStatus  int                  `json:"upstreamStatus,omitempty"`
	ContentType     string               `json:"contentType,omitempty"`
	RetryAfter      string               `json:"retryAfter,omitempty"`
	Bytes           int64                `json:"bytes,omitempty"`
	Limit           int64                `json:"limit,omitempty"`
	Expected        string               `json:"expected,omitempty"`
	Actual          string               `json:"actual,omitempty"`
	Attempts        []AcquisitionAttempt `json:"attempts,omitempty"`
	Retryable       bool                 `json:"retryable"`
	FallbackAllowed bool                 `json:"fallbackAllowed"`
	Cause           error                `json:"-"`
}

func (e *AcquisitionError) Error() string {
	message := "resource acquisition failed: " + e.Code
	if archivePackageName.MatchString(e.Package) {
		message += " (package " + e.Package + ")"
	}
	return message
}

func (e *AcquisitionError) Unwrap() error { return e.Cause }

func (e *AcquisitionError) HTTPStatus() int {
	switch e.Code {
	case "resource_upstream_network":
		if e.Reason == "timeout" {
			return http.StatusGatewayTimeout
		}
		return http.StatusBadGateway
	case "resource_upstream_http":
		if e.UpstreamStatus == http.StatusTooManyRequests {
			return http.StatusServiceUnavailable
		}
		if e.UpstreamStatus == http.StatusRequestTimeout || e.UpstreamStatus == http.StatusGatewayTimeout {
			return http.StatusGatewayTimeout
		}
		return http.StatusBadGateway
	case "resource_limit_exceeded", "resource_quota_exceeded":
		return http.StatusRequestEntityTooLarge
	case "resource_prepare_timeout":
		return http.StatusGatewayTimeout
	case "resource_runtime_unavailable", "resource_cache_unavailable":
		return http.StatusServiceUnavailable
	case "resource_package_unsupported", "resource_package_unavailable":
		return http.StatusUnprocessableEntity
	case "resource_storage_failed", "resource_install_failed", "resource_index_invalid":
		return http.StatusInternalServerError
	case "resource_cancelled":
		return http.StatusRequestTimeout
	default:
		return http.StatusBadGateway
	}
}

func acquisitionFailure(code, stage, reason string, pkg Package, cause error) *AcquisitionError {
	return &AcquisitionError{
		Code:     code,
		Stage:    stage,
		Reason:   reason,
		Package:  pkg.Name,
		Revision: pkg.Revision,
		Cause:    cause,
	}
}

// AsAcquisitionError preserves typed failures while assigning local operation
// failures a safe classification. It never incorporates the cause's text.
func AsAcquisitionError(err error, code, stage, reason string, pkg Package) *AcquisitionError {
	var failure *AcquisitionError
	if errors.As(err, &failure) {
		return failure
	}
	if errors.Is(err, context.Canceled) {
		code, reason = "resource_cancelled", "cancelled"
	} else if errors.Is(err, context.DeadlineExceeded) {
		return &AcquisitionError{
			Code:      "resource_prepare_timeout",
			Stage:     stage,
			Reason:    "timeout",
			Package:   pkg.Name,
			Revision:  pkg.Revision,
			Retryable: true,
			Cause:     err,
		}
	}
	return acquisitionFailure(code, stage, reason, pkg, err)
}

func policyError(reason string) error {
	return fmt.Errorf("invalid TeX Live source policy: %s", reason)
}

// CanonicalRetryAfter accepts only bounded HTTP dates or nonnegative integer
// seconds. It never forwards arbitrary upstream header text.
func CanonicalRetryAfter(value string) string {
	if value == "" || len(value) > 128 {
		return ""
	}
	if date, err := http.ParseTime(value); err == nil {
		return date.UTC().Format(http.TimeFormat)
	}
	if strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return ""
	}
	seconds, err := strconv.ParseUint(value, 10, 63)
	if err != nil {
		return ""
	}
	return strconv.FormatUint(seconds, 10)
}
