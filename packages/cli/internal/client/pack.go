package client

import (
	"context"
	"errors"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

// CompileCaptured builds exactly these captured members, without rediscovery,
// missing-file retries, recorder-cache updates or legacy transport fallback.
func (c *Client) CompileCaptured(
	ctx context.Context,
	request protocol.CompileRequest,
	snapshot *projectarchive.Frozen,
	outputRoot string,
	meta protocol.Metadata,
) (CompileOutput, error) {
	if !meta.Capabilities.IncrementalUpload || !meta.Capabilities.QueuedJobs {
		return CompileOutput{}, &CapabilityError{Capability: "captured queued compilation"}
	}
	if err := validateAuxiliaryCapability(request, meta); err != nil {
		return CompileOutput{}, err
	}
	request.RecordInputs = meta.Capabilities.DependencyInputs
	request.DetectMissingFiles = false
	output, err := c.compileQueued(ctx, request, outputRoot, snapshot.Files)
	if err == nil && (output.Result.Entry != request.Entry || output.Result.Engine != request.Engine) {
		return CompileOutput{}, errors.New("captured build result does not match the requested entry and engine")
	}
	return output, err
}
