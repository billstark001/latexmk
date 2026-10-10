package metadata

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/billstark001/latexmk/packages/server/internal/config"
	"github.com/billstark001/latexmk/packages/shared/engine"
	"github.com/billstark001/latexmk/packages/shared/protocol"
)

func TestValidateToolchain(t *testing.T) {
	cfg := config.Config{Engines: []string{"xelatex"}}
	meta := protocol.Metadata{Toolchain: map[string]string{"latexmk": "v", "xelatex": "v"}}
	if err := ValidateToolchain(meta, cfg); err != nil {
		t.Fatal(err)
	}
	delete(meta.Toolchain, "xelatex")
	if err := ValidateToolchain(meta, cfg); err == nil {
		t.Fatal("expected missing engine error")
	}
}

var customEngineCounter atomic.Uint64

type customEngine struct{}

func (customEngine) LatexmkArgs() []string        { return []string{"-pdf"} }
func (customEngine) GraphicsExtensions() []string { return []string{".pdf"} }
func (customEngine) VersionProbe() engine.Command {
	return engine.Command{Name: "trusted-tex", Args: []string{"--version"}}
}

func TestToolchainProbesRegisteredDriverInsteadOfEngineName(t *testing.T) {
	name := fmt.Sprintf("lab/custom+%d", customEngineCounter.Add(1))
	if err := engine.Default.Register(name, customEngine{}); err != nil {
		t.Fatal(err)
	}
	tools := collectToolchain(func(command string, args ...string) string {
		if command == name {
			t.Fatal("used the engine key as an executable")
		}
		if command == "trusted-tex" {
			if len(args) != 1 || args[0] != "--version" {
				t.Fatal(args)
			}
			return "Custom TeX 2026"
		}
		return "test version"
	})
	if tools[name] != "Custom TeX 2026" {
		t.Fatal(tools)
	}
	if err := ValidateToolchain(
		protocol.Metadata{Toolchain: tools},
		config.Config{Engines: []string{name}},
	); err != nil {
		t.Fatal(err)
	}
}
