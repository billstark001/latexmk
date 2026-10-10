package metadata

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

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

func TestFailedVersionProbeIsUnavailable(t *testing.T) {
	if line := firstLine(os.Args[0], "-test.run=TestVersionProbeHelper", "--", "failure"); line != "" {
		t.Fatalf("failed probe advertised %q", line)
	}
	if line := firstLine(os.Args[0], "-test.run=TestVersionProbeHelper", "--", "success"); line != "Test tool version" {
		t.Fatalf("successful version=%q", line)
	}
}

func TestVersionProbeHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			fmt.Println("Test tool version")
			if os.Args[i+1] == "failure" {
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
}

func TestToolchainSharesIdenticalVersionProbes(t *testing.T) {
	first := fmt.Sprintf("alias-a-%d", customEngineCounter.Add(1))
	second := fmt.Sprintf("alias-b-%d", customEngineCounter.Add(1))
	for _, name := range []string{first, second} {
		if err := engine.Default.Register(name, customEngine{}); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int64
	tools := collectToolchain(func(command string, args ...string) string {
		if command == "trusted-tex" {
			calls.Add(1)
		}
		return "version"
	})
	if calls.Load() != 1 || tools[first] != "version" || tools[second] != "version" {
		t.Fatalf("calls=%d, tools=%v", calls.Load(), tools)
	}
}

func TestToolchainBoundsConcurrentProbes(t *testing.T) {
	started := make(chan struct{}, 32)
	release := make(chan struct{})
	done := make(chan struct{})
	var active, peak atomic.Int64
	go func() {
		collectToolchain(func(string, ...string) string {
			count := active.Add(1)
			for old := peak.Load(); count > old && !peak.CompareAndSwap(old, count); old = peak.Load() {
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			return "version"
		})
		close(done)
	}()
	received := 0
	for received < maxConcurrentProbes {
		select {
		case <-started:
			received++
		case <-time.After(time.Second):
			close(release)
			<-done
			t.Fatalf("only %d probes started concurrently", received)
		}
	}
	close(release)
	<-done
	if peak.Load() != maxConcurrentProbes {
		t.Fatalf("concurrent probes=%d", peak.Load())
	}
}
