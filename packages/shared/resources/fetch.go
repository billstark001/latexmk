package resources

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var archivePackageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+._-]{0,127}$`)

type ArchiveFetchOptions struct {
	Client     *http.Client
	Repository string
	Package    Package
	Policy     *SourcePolicy
	MaxBytes   int64
}

type ArchiveFetchResult struct {
	Bytes    int64
	SourceID string
	Attempts []AcquisitionAttempt
}

func ValidateArchivePackage(pkg Package) error {
	if !archivePackageName.MatchString(pkg.Name) || pkg.Revision <= 0 || len(pkg.Checksum) != sha512.Size*2 ||
		pkg.Checksum != strings.ToLower(pkg.Checksum) {
		return policyError("invalid package archive identity")
	}
	if _, err := hex.DecodeString(pkg.Checksum); err != nil {
		return policyError("invalid package archive checksum")
	}
	return nil
}

// NewArchiveHTTPClient does not inherit ambient proxy credentials. Redirects
// must remain HTTPS and cannot introduce URL credentials.
func NewArchiveHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || req.URL.User != nil || len(via) >= 10 {
				return &AcquisitionError{
					Code:   "resource_upstream_tls",
					Stage:  "archive-fetch",
					Reason: "redirect-policy",
				}
			}
			return nil
		},
	}
}

// FetchArchive tries each configured source once within the caller's overall
// context. publish must create a fresh staging destination for each invocation,
// invoke write once and publish only after write succeeds. Failed staged bytes
// must be discarded. No package bytes are buffered in memory.
func FetchArchive(
	ctx context.Context,
	options ArchiveFetchOptions,
	publish func(write func(io.Writer) error) error,
) (ArchiveFetchResult, error) {
	var result ArchiveFetchResult
	if err := ValidateArchivePackage(options.Package); err != nil || publish == nil {
		return result, acquisitionFailure(
			"resource_package_unsupported",
			"archive-fetch",
			"identity",
			options.Package,
			err,
		)
	}
	if options.MaxBytes <= 0 || options.MaxBytes == math.MaxInt64 {
		return result, acquisitionFailure(
			"resource_limit_exceeded",
			"archive-fetch",
			"download-limit",
			options.Package,
			nil,
		)
	}
	var sources []string
	timeout := 30 * time.Second
	if options.Policy == nil {
		if err := validateSourceURL(options.Repository); err != nil {
			return result, acquisitionFailure(
				"resource_runtime_unavailable",
				"archive-fetch",
				"repository",
				options.Package,
				err,
			)
		}
		sources = []string{options.Repository}
	} else {
		if err := options.Policy.Validate(); err != nil {
			return result, acquisitionFailure(
				"resource_runtime_unavailable",
				"archive-fetch",
				"source-policy",
				options.Package,
				err,
			)
		}
		sources = options.Policy.ArchiveMirrors
		timeout, _ = options.Policy.sourceTimeout()
	}
	if len(sources) == 0 {
		return result, acquisitionFailure(
			"resource_runtime_unavailable",
			"archive-fetch",
			"no-sources",
			options.Package,
			nil,
		)
	}
	client := options.Client
	if client == nil {
		client = NewArchiveHTTPClient()
		defer client.CloseIdleConnections()
	}
	// Supplied clients may configure certificates or timeouts, but cannot relax
	// the transport policy's HTTPS/credential constraints on redirects.
	copyClient := *client
	previousRedirect := client.CheckRedirect
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || req.URL.User != nil || len(via) >= 10 {
			return &AcquisitionError{Code: "resource_upstream_tls", Stage: "archive-fetch", Reason: "redirect-policy"}
		}
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		return nil
	}
	client = &copyClient
	var last *AcquisitionError
	for i, source := range sources {
		if err := ctx.Err(); err != nil {
			last = contextAcquisitionError(err, options.Package)
			break
		}
		sourceID := fmt.Sprintf("mirror-%d", i+1)
		name := fmt.Sprintf("%s.r%d.tar.xz", options.Package.Name, options.Package.Revision)
		if options.Policy == nil {
			sourceID, name = "snapshot", options.Package.Name+".tar.xz"
		}
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		var count int64
		var transferErr error
		called := false
		err := publish(func(destination io.Writer) error {
			if called {
				return acquisitionFailure(
					"resource_storage_failed",
					"archive-fetch",
					"destination-reused",
					options.Package,
					nil,
				)
			}
			called = true
			count, transferErr = fetchArchiveSource(
				attemptCtx,
				client,
				strings.TrimRight(source, "/")+"/archive/"+name,
				destination,
				options,
			)
			return transferErr
		})
		cancel()
		if transferErr != nil {
			err = transferErr
		}
		if err == nil && !called {
			err = acquisitionFailure(
				"resource_storage_failed",
				"archive-fetch",
				"destination-unused",
				options.Package,
				nil,
			)
		}
		if err == nil {
			result.Bytes, result.SourceID = count, sourceID
			result.Attempts = append(result.Attempts, AcquisitionAttempt{SourceID: sourceID, Code: "ok", Bytes: count})
			return result, nil
		}
		if ctx.Err() != nil {
			last = contextAcquisitionError(ctx.Err(), options.Package)
		} else {
			last = AsAcquisitionError(err, "resource_storage_failed", "archive-fetch", "publish", options.Package)
		}
		last.SourceID = sourceID
		result.Attempts = append(result.Attempts, AcquisitionAttempt{
			SourceID:       sourceID,
			Code:           last.Code,
			Reason:         last.Reason,
			UpstreamStatus: last.UpstreamStatus,
			ContentType:    last.ContentType,
			RetryAfter:     last.RetryAfter,
			Bytes:          last.Bytes,
		})
		if !last.FallbackAllowed {
			break
		}
	}
	last.Attempts = result.Attempts
	return result, last
}

func contextAcquisitionError(err error, pkg Package) *AcquisitionError {
	code, reason := "resource_cancelled", "cancelled"
	if errors.Is(err, context.DeadlineExceeded) {
		code, reason = "resource_prepare_timeout", "timeout"
	}
	failure := acquisitionFailure(code, "archive-fetch", reason, pkg, err)
	failure.Retryable = errors.Is(err, context.DeadlineExceeded)
	return failure
}

func transportAcquisitionError(err error, pkg Package) *AcquisitionError {
	var existing *AcquisitionError
	if errors.As(err, &existing) {
		copyFailure := *existing
		copyFailure.Package, copyFailure.Revision = pkg.Name, pkg.Revision
		return &copyFailure
	}
	failure := acquisitionFailure("resource_upstream_network", "archive-fetch", "network", pkg, err)
	failure.Retryable, failure.FallbackAllowed = true, true
	var certificate *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var record tls.RecordHeaderError
	var dns *net.DNSError
	var network net.Error
	switch {
	case errors.As(err, &certificate), errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &hostname):
		failure.Code, failure.Reason, failure.Retryable = "resource_upstream_tls", "verification", false
	case errors.As(err, &record):
		failure.Code, failure.Reason, failure.Retryable = "resource_upstream_tls", "protocol", false
	case errors.As(err, &dns):
		failure.Reason = "dns"
		failure.Retryable = !dns.IsNotFound
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &network) && network.Timeout():
		failure.Reason = "timeout"
	}
	return failure
}

type archiveWriter struct {
	destination io.Writer
	err         error
}

func (w *archiveWriter) Write(p []byte) (int, error) {
	n, err := w.destination.Write(p)
	if err != nil {
		w.err = err
	} else if n != len(p) {
		w.err, err = io.ErrShortWrite, io.ErrShortWrite
	}
	return n, err
}

func fetchArchiveSource(
	ctx context.Context,
	client *http.Client,
	target string,
	destination io.Writer,
	options ArchiveFetchOptions,
) (int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, acquisitionFailure("resource_runtime_unavailable", "archive-fetch", "request", options.Package, err)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, transportAcquisitionError(err, options.Package)
	}
	defer func() { _ = response.Body.Close() }()
	failure := acquisitionFailure("resource_upstream_http", "archive-fetch", "http-status", options.Package, nil)
	failure.UpstreamStatus = response.StatusCode
	if mediaType, _, err := mime.ParseMediaType(
		response.Header.Get("Content-Type"),
	); err == nil &&
		len(mediaType) <= 128 {
		failure.ContentType = mediaType
	}
	if response.StatusCode != http.StatusOK {
		failure.Retryable = response.StatusCode == 408 || response.StatusCode == 429 || response.StatusCode >= 500
		failure.FallbackAllowed = true
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable {
			failure.RetryAfter = CanonicalRetryAfter(response.Header.Get("Retry-After"))
		}
		return 0, failure
	}
	if response.ContentLength > options.MaxBytes {
		failure.Code, failure.Reason, failure.Bytes, failure.Limit = "resource_limit_exceeded", "download-bytes", response.ContentLength, options.MaxBytes
		return 0, failure
	}
	reader := bufio.NewReader(response.Body)
	prefix, prefixErr := reader.Peek(6)
	if prefixErr != nil && !errors.Is(prefixErr, io.EOF) {
		return 0, transportAcquisitionError(prefixErr, options.Package)
	}
	if !bytes.Equal(prefix, []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}) {
		failure.Code, failure.Reason, failure.FallbackAllowed = "resource_upstream_format", "xz-magic", true
		trimmed := strings.ToLower(strings.TrimSpace(string(prefix)))
		if strings.HasPrefix(trimmed, "<!doc") || strings.HasPrefix(trimmed, "<html") ||
			failure.ContentType == "text/html" {
			failure.Reason = "html"
		}
		return 0, failure
	}
	hash := sha512.New()
	writer := &archiveWriter{destination: destination}
	size, err := io.Copy(io.MultiWriter(writer, hash), io.LimitReader(reader, options.MaxBytes))
	if err != nil {
		if writer.err != nil {
			return 0, acquisitionFailure(
				"resource_storage_failed",
				"archive-fetch",
				"write",
				options.Package,
				writer.err,
			)
		}
		return 0, transportAcquisitionError(err, options.Package)
	}
	var extra [1]byte
	n, err := io.ReadFull(reader, extra[:])
	if n > 0 {
		failure.Code, failure.Reason, failure.Bytes, failure.Limit = "resource_limit_exceeded", "download-bytes", size+int64(
			n,
		), options.MaxBytes
		return 0, failure
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, transportAcquisitionError(err, options.Package)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != options.Package.Checksum {
		failure.Code, failure.Reason = "resource_archive_checksum", "sha512"
		failure.Expected, failure.Actual, failure.Bytes = options.Package.Checksum, actual, size
		failure.FallbackAllowed = true
		return 0, failure
	}
	return size, nil
}
