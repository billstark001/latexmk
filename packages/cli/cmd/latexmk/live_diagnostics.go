package main

import (
	"encoding/json"
	"os"
	"time"
)

// Live diagnostics describe phase boundaries without including paths, request
// contents, credentials or remote error text.
type liveDiagnostics bool

type liveDiagnostic struct {
	Event      string `json:"event"`
	TimeNS     int64  `json:"time_ns"`
	DurationNS int64  `json:"duration_ns,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	JobID      string `json:"job_id,omitempty"`
	Revision   uint64 `json:"revision,omitempty"`
	Attempt    uint64 `json:"attempt,omitempty"`
	Trigger    string `json:"trigger,omitempty"`
	OK         bool   `json:"ok"`
}

func newLiveDiagnostics() liveDiagnostics {
	return os.Getenv("LATEXMK_REALTIME_DIAGNOSTICS") == "1"
}

func (enabled liveDiagnostics) start() time.Time {
	if enabled {
		return time.Now()
	}
	return time.Time{}
}

func (enabled liveDiagnostics) record(started time.Time, event liveDiagnostic) {
	if !enabled {
		return
	}
	now := time.Now()
	event.TimeNS = now.UnixNano()
	if !started.IsZero() {
		event.DurationNS = now.Sub(started).Nanoseconds()
	}
	line, err := json.Marshal(event)
	if err == nil {
		terminalFprintf(os.Stderr, "realtime_diagnostics %s\n", line)
	}
}
