package client

import (
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/resources"
)

func TestRealtimePolicyUsesSelectedBaseline(t *testing.T) {
	for _, test := range []struct {
		name            string
		globalIsolation bool
		baseline        *resources.Baseline
		wantError       bool
	}{
		{"selected-image", false, &resources.Baseline{ID: "image", Image: "runner@sha256:" + strings.Repeat("a", 64)}, false},
		{"selected-native", true, &resources.Baseline{ID: "native"}, true},
		{"legacy-image", true, nil, false},
		{"legacy-native", false, nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := &Client{}
			if test.baseline != nil {
				c.SetBaseline(*test.baseline)
			}
			meta := protocol.Metadata{Capabilities: protocol.Capabilities{
				CompileCache: true, IsolatedWorkspaces: test.globalIsolation,
			}}
			req := protocol.CompileRequest{Auxiliary: protocol.AuxiliaryOptions{Server: "reuse"}}
			err := c.ValidateRealtimeRequest(req, meta)
			if (err != nil) != test.wantError {
				t.Fatalf("ValidateRealtimeRequest() = %v; wantError %t", err, test.wantError)
			}
		})
	}
}

func TestRealtimePolicyRejectsUnavailableCacheAndImageShellEscape(t *testing.T) {
	c := &Client{}
	c.SetBaseline(resources.Baseline{Image: "runner@sha256:" + strings.Repeat("a", 64)})
	meta := protocol.Metadata{Capabilities: protocol.Capabilities{AuxiliaryRetention: true}}
	req := protocol.CompileRequest{Auxiliary: protocol.AuxiliaryOptions{Server: "reuse"}}
	if err := c.ValidateRealtimeRequest(req, meta); err == nil || !strings.Contains(err.Error(), "compile cache") {
		t.Fatalf("missing cache: %v", err)
	}
	meta.Capabilities.CompileCache = true
	req.ShellEscape = true
	if err := c.ValidateRealtimeRequest(req, meta); err == nil || !strings.Contains(err.Error(), "shell escape") {
		t.Fatalf("image shell escape: %v", err)
	}
	// Fresh isolated sessions have the same shell policy as reusable ones.
	req.Auxiliary.Server = "none"
	if err := c.ValidateRealtimeRequest(req, meta); err == nil || !strings.Contains(err.Error(), "shell escape") {
		t.Fatalf("fresh image shell escape: %v", err)
	}
}
