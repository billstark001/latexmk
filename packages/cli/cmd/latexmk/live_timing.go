package main

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// A nil recorder keeps the normal realtime path silent. The event schema only
// permits phase names, opaque correlation IDs and measurements, never requests,
// paths, credentials or error text.
type liveTiming struct {
	mu     sync.Mutex
	writer io.Writer
}

type liveTimingEvent struct {
	Event  string `json:"event"`
	TimeNS int64  `json:"time_ns"`
	// A measured phase may take zero clock ticks; nil denotes an instant event.
	DurationNS *int64 `json:"duration_ns,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	JobID      string `json:"job_id,omitempty"`
	Revision   uint64 `json:"revision,omitempty"`
	Attempt    uint64 `json:"attempt,omitempty"`
	Trigger    string `json:"trigger,omitempty"`
	OK         bool   `json:"ok"`
}

func newLiveTiming() *liveTiming {
	if os.Getenv("LATEXMK_REALTIME_DIAGNOSTICS") != "1" {
		return nil
	}
	return &liveTiming{writer: os.Stderr}
}

func (timing *liveTiming) start() time.Time {
	if timing == nil {
		return time.Time{}
	}
	return time.Now()
}

func (timing *liveTiming) record(started time.Time, event liveTimingEvent) {
	if timing == nil {
		return
	}

	now := time.Now()
	event.TimeNS = now.UnixNano()
	if !started.IsZero() {
		// Sub uses the monotonic readings carried by start and now. Wall clock
		// adjustments therefore cannot distort a measured phase duration.
		duration := max(0, now.Sub(started).Nanoseconds())
		event.DurationNS = &duration
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return
	}
	line := append([]byte("realtime_diagnostics "), encoded...)
	line = append(line, '\n')
	// Timestamp the phase boundary before serialization or writer contention.
	// A single write under this shared lock keeps concurrent events whole.
	timing.mu.Lock()
	defer timing.mu.Unlock()
	_, _ = timing.writer.Write(line)
}
