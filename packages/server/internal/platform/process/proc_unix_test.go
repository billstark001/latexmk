//go:build !windows

package process

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigureProcessKillsChildProcessGroupOnTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & wait")
	configureProcess(cmd)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	started := time.Now()
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("process group termination took too long: %v", elapsed)
	}
}

func TestRunBoundsOutputAndPreservesExitStatus(t *testing.T) {
	result := Run(
		context.Background(),
		Spec{Name: "sh", Args: []string{"-c", "printf 'abcdefgh'; printf '12345678' >&2; exit 7"}, MaxOutputBytes: 4},
	)
	if result.ExitCode != 7 || result.Err == nil || string(result.Stdout) != "abcd" ||
		string(result.Stderr) != "1234" ||
		!result.StdoutTruncated ||
		!result.StderrTruncated {
		t.Fatalf("%+v", result)
	}
	combined := Run(
		context.Background(),
		Spec{
			Name:           "sh",
			Args:           []string{"-c", "printf 'abc'; printf 'def' >&2"},
			MaxOutputBytes: 4,
			CombinedOutput: true,
		},
	)
	if combined.Err != nil || string(combined.Stdout) != "abcd" || !combined.StdoutTruncated ||
		len(combined.Stderr) != 0 {
		t.Fatalf("%+v", combined)
	}
}

func TestRunCancellationAndStartupFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := Run(ctx, Spec{Name: "sh", Args: []string{"-c", "exit 0"}, MaxOutputBytes: 10})
	if !errors.Is(result.Err, context.Canceled) || result.ExitCode != -1 {
		t.Fatalf("%+v", result)
	}
	result = Run(context.Background(), Spec{Name: filepath.Join(t.TempDir(), "missing"), MaxOutputBytes: 10})
	if result.Err == nil || result.ExitCode != -1 {
		t.Fatalf("%+v", result)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	result = Run(ctx, Spec{Name: "sh", Args: []string{"-c", "sleep 30 & wait"}, MaxOutputBytes: 10})
	if !errors.Is(result.Err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second {
		t.Fatalf("%+v", result)
	}
}

func TestRunTerminatesDescendantsAfterSuccessfulParentExit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "orphan-finished")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := Run(
		ctx,
		Spec{
			Name:           "sh",
			Args:           []string{"-c", "(sleep 0.2; touch \"$1\") >/dev/null 2>&1 &", "sh", marker},
			MaxOutputBytes: 10,
		},
	)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descendant survived: %v", err)
	}
}
