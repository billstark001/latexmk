package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"

	"github.com/billstark001/latexmk/packages/cli/internal/client"
)

const agentJSONSchemaVersion = 1

type agentJSONEnvelope struct {
	SchemaVersion int             `json:"schemaVersion"`
	OK            bool            `json:"ok"`
	Command       string          `json:"command"`
	Data          any             `json:"data,omitempty"`
	Error         *agentJSONError `json:"error,omitempty"`
}

type agentJSONError struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	Retryable bool           `json:"retryable"`
}

func writeAgentJSON(command string, data any) error {
	return json.NewEncoder(os.Stdout).Encode(agentJSONEnvelope{
		SchemaVersion: agentJSONSchemaVersion,
		OK:            true,
		Command:       command,
		Data:          data,
	})
}

func failAgent(command string, jsonOutput bool, err error) int {
	if !jsonOutput {
		return fail(err)
	}
	code, details, retryable, exitCode := classifyAgentError(err)
	encodeErr := json.NewEncoder(os.Stdout).Encode(agentJSONEnvelope{
		SchemaVersion: agentJSONSchemaVersion,
		OK:            false,
		Command:       command,
		Error: &agentJSONError{
			Code:      code,
			Message:   err.Error(),
			Details:   details,
			Retryable: retryable,
		},
	})
	if encodeErr != nil {
		return fail(encodeErr)
	}
	return exitCode
}

func failAgentArguments(command string, jsonOutput bool, err error) int {
	if !jsonOutput {
		return fail(err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(agentJSONEnvelope{
		SchemaVersion: agentJSONSchemaVersion,
		OK:            false,
		Command:       command,
		Error: &agentJSONError{
			Code:      "invalid_arguments",
			Message:   err.Error(),
			Retryable: false,
		},
	})
	return 2
}

func classifyAgentError(err error) (string, map[string]any, bool, int) {
	var httpErr *client.HTTPError
	if errors.As(err, &httpErr) {
		return classifyHTTPError(err, httpErr)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", nil, true, 124
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled", nil, false, 1
	}
	var capabilityErr *client.CapabilityError
	if errors.As(err, &capabilityErr) {
		return "unsupported_capability", map[string]any{"capability": capabilityErr.Capability}, false, 1
	}
	var stateErr *client.ResultStateError
	if errors.As(err, &stateErr) {
		if stateErr.Status == "queued" || stateErr.Status == "running" {
			return "result_not_ready", map[string]any{"status": stateErr.Status}, true, 1
		}
		return "result_unavailable", map[string]any{"status": stateErr.Status}, false, 1
	}
	var artifactErr *client.ArtifactNotFoundError
	if errors.As(err, &artifactErr) {
		return "artifact_not_found", map[string]any{"artifactId": artifactErr.ID}, false, 1
	}
	var netErr net.Error
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) && errors.As(err, &netErr) {
		return "network_error", nil, true, 1
	}
	return "command_failed", nil, false, 1
}

func classifyHTTPError(cause error, err *client.HTTPError) (string, map[string]any, bool, int) {
	details := map[string]any{"httpStatus": err.StatusCode}
	for key, value := range map[string]string{
		"requestId": err.RequestID, "retryAfter": err.RetryAfter, "method": err.Method, "path": err.Path,
		"contentType": err.ContentType, "responseIssue": err.ResponseIssue, "serverMessage": err.Message,
	} {
		if value != "" {
			details[key] = value
		}
	}
	if err.BodyTruncated {
		details["bodyTruncated"] = true
	}
	if err.MessageTruncated {
		details["messageTruncated"] = true
	}
	if err.StatusCode >= 200 && err.StatusCode < 300 {
		if errors.Is(cause, context.DeadlineExceeded) {
			return "timeout", details, true, 124
		}
		if errors.Is(cause, context.Canceled) {
			return "cancelled", details, false, 1
		}
		var network net.Error
		var pathErr *os.PathError
		if !errors.As(cause, &pathErr) && errors.As(cause, &network) {
			return "network_error", details, true, 1
		}
		return "invalid_response", details, false, 1
	}
	switch err.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication_failed", details, false, 1
	case http.StatusNotFound:
		return "not_found", details, false, 1
	case http.StatusConflict:
		return "conflict", details, false, 1
	case http.StatusTooManyRequests:
		return "rate_limited", details, true, 1
	default:
		if err.StatusCode >= 500 {
			return "server_error", details, true, 1
		}
		return "http_error", details, err.StatusCode == http.StatusRequestTimeout, 1
	}
}
