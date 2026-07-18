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
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", nil, true, 124
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled", nil, false, 1
	}
	var httpErr *client.HTTPError
	if errors.As(err, &httpErr) {
		details := map[string]any{"httpStatus": httpErr.StatusCode}
		switch httpErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "authentication_failed", details, false, 1
		case http.StatusNotFound:
			return "not_found", details, false, 1
		case http.StatusConflict:
			return "conflict", details, false, 1
		case http.StatusTooManyRequests:
			return "rate_limited", details, true, 1
		default:
			if httpErr.StatusCode >= 500 {
				return "server_error", details, true, 1
			}
			return "http_error", details, false, 1
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return "network_error", nil, true, 1
	}
	return "command_failed", nil, false, 1
}
