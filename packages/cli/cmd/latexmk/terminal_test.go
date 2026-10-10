package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestJobReportAndErrorsDoNotExecuteTerminalControls(t *testing.T) {
	injected := "compiler\x1b]52;c;clipboard\x07\rrewrite\u009bcontrol"
	_, stdout, stderr := captureCommandOutput(t, func() int {
		reportJob(
			"jobs.show",
			protocol.Job{ID: injected, ProjectID: injected, SnapshotID: injected, Status: injected, Error: injected},
			false,
		)
		return fail(errors.New(injected))
	})
	for _, output := range []string{stdout, stderr} {
		if strings.ContainsAny(output, "\x1b\x07\r\u009b") {
			t.Fatalf("terminal controls passed through: %q", output)
		}
	}
}
