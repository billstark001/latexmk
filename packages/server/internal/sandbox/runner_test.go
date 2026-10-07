package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/billstark001/latexmk/packages/server/internal/compile"
	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestTransportRejectsSourceDescriptorMismatchBeforeStartingWorker(t *testing.T) {
	source := protocol.ProjectFile{Path: "main.tex", SHA256: strings.Repeat("a", 64), Size: 1}
	for _, descriptors := range []map[string]compile.File{
		nil,
		{"other.tex": {Size: 1, SHA256: source.SHA256}},
		{"main.tex": {Size: 2, SHA256: source.SHA256}},
		{"main.tex": {Size: 1, SHA256: strings.Repeat("b", 64)}},
	} {
		_, _, err := Run(
			context.Background(),
			config.Config{RunnerImage: "unused-image"},
			protocol.CompileRequest{},
			"attempt",
			[]protocol.ProjectFile{source},
			nil,
			"",
			descriptors,
			t.TempDir(),
			false,
		)
		if err == nil || !strings.Contains(err.Error(), "source descriptor") {
			t.Fatalf("descriptor mismatch was not rejected before worker startup: %v", err)
		}
	}
}
