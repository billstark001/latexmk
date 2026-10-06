package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/dependency"
	"github.com/billstark001/latexmk/packages/cli/internal/protocol"
)

func (c *Client) CreateSession(ctx context.Context, req protocol.SessionRequest) (protocol.Session, error) {
	var session protocol.Session
	err := c.jsonRequest(ctx, http.MethodPost, "/v1/sessions", req, &session)
	return session, err
}
func (c *Client) GetSession(ctx context.Context, id string) (protocol.Session, error) {
	var session protocol.Session
	err := c.jsonRequest(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(id), nil, &session)
	return session, err
}
func (c *Client) CloseSession(ctx context.Context, id string) error {
	return c.jsonRequest(ctx, http.MethodDelete, "/v1/sessions/"+url.PathEscape(id), nil, nil)
}

// FreezeSnapshot reevaluates the complete upload policy before and after reading
// sources. Membership changes discard the spool rather than uploading stale policy.
func (c *Client) FreezeSnapshot(
	ctx context.Context,
	request protocol.CompileRequest,
	additional []string,
	meta protocol.Metadata,
) (*projectarchive.Frozen, error) {
	selection, err := c.selectFiles(request.Entry, request.Engine, additional, false)
	if err != nil {
		return nil, err
	}
	frozen, err := projectarchive.Freeze(
		ctx,
		selection.Files,
		meta.Capabilities.MaxFiles,
		meta.Capabilities.MaxExpandedBytes,
	)
	if err != nil {
		return nil, err
	}
	current, err := c.selectFiles(request.Entry, request.Engine, additional, false)
	if err != nil {
		_ = frozen.Close()
		return nil, err
	}
	paths := make(map[string]bool, len(current.Files))
	for _, file := range current.Files {
		paths[file.Path] = true
	}
	if len(paths) != len(frozen.Files) {
		_ = frozen.Close()
		return nil, errors.New("file selection changed while capturing the snapshot")
	}
	for _, file := range frozen.Files {
		if !paths[file.Path] {
			_ = frozen.Close()
			return nil, errors.New("upload policy changed while capturing the snapshot")
		}
	}
	return frozen, nil
}

// PreparedRevision survives an ambiguous HTTP response. Replaying the exact
// upload ID, base revision and key resolves acceptance without submitting twice.
type PreparedRevision struct {
	SessionID string
	Payload   protocol.RevisionRequest
}

func (c *Client) PrepareRevision(
	ctx context.Context,
	session protocol.Session,
	request protocol.CompileRequest,
	frozen *projectarchive.Frozen,
	key string,
) (PreparedRevision, error) {
	plan, err := c.uploadManifest(ctx, request, frozen.Files)
	if err != nil {
		return PreparedRevision{}, err
	}
	return PreparedRevision{
		SessionID: session.ID,
		Payload: protocol.RevisionRequest{
			UploadID:       plan.UploadID,
			BaseRevision:   session.Revision,
			IdempotencyKey: key,
		},
	}, nil
}

func (c *Client) CommitRevision(ctx context.Context, prepared PreparedRevision) (protocol.Job, error) {
	var job protocol.Job
	err := c.jsonRequest(
		ctx,
		http.MethodPost,
		"/v1/sessions/"+url.PathEscape(prepared.SessionID)+"/revisions",
		prepared.Payload,
		&job,
	)
	return job, err
}

// StreamSessionEvents is bounded by its context, and uses a separate HTTP client
// lifetime so status subscriptions cannot exhaust an ordinary compile timeout.
func (c *Client) StreamSessionEvents(
	ctx context.Context,
	id string,
	after uint64,
	receive func(protocol.SessionEvent),
) error {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		c.BaseURL+"/v1/sessions/"+url.PathEscape(id)+"/events",
		nil,
	)
	if err != nil {
		return err
	}
	c.decorate(req)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", strconv.FormatUint(after, 10))
	stream := *c.HTTP
	stream.Timeout = 0
	resp, err := stream.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return readHTTPError(resp)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return errors.New("unexpected session event content type")
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	var data string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
		if line == "" && data != "" {
			var event protocol.SessionEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				return fmt.Errorf("invalid session event: %w", err)
			}
			if event.Type == "resync" || event.Sequence > after {
				after = event.Sequence
				receive(event)
			}
			data = ""
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

func (c *Client) ResolveMissingFiles(
	needs []string,
	selected []projectarchive.File,
	already []string,
) ([]string, error) {
	if len(already) >= maxNeedsFiles {
		return nil, errors.New("missing-file recovery reached its file limit")
	}
	candidates, _, err := c.policyManifest()
	if err != nil {
		return nil, err
	}
	requested, err := dependency.ResolveRequestedFiles(needs, candidates)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, file := range selected {
		seen[file.Path] = true
	}
	for _, name := range already {
		seen[name] = true
	}
	var additional []string
	var bytes int64
	for _, file := range candidates {
		for _, name := range already {
			if file.Path == name {
				bytes += file.Size
			}
		}
	}
	for _, file := range requested {
		if seen[file.Path] {
			continue
		}
		bytes += file.Size
		additional = append(additional, file.Path)
	}
	if len(already)+len(additional) > maxNeedsFiles || bytes > maxNeedsFileBytes {
		return nil, errors.New("missing-file recovery exceeds file or byte limits")
	}
	return additional, nil
}

func ValidateRealtimeRequest(request protocol.CompileRequest, meta protocol.Metadata) error {
	if err := validateAuxiliaryCapability(request, meta); err != nil {
		return err
	}
	if request.Auxiliary.Server == "reuse" && !meta.Capabilities.IsolatedWorkspaces {
		return &CapabilityError{Capability: "isolated reusable workspaces"}
	}
	return nil
}
