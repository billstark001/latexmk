package resources

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/platform/process"
	model "github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func fontFilename(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".otf", ".ttf", ".ttc":
		return true
	}
	return false
}

// ScanFont runs as a fixed, network-free container helper in isolated service
// deployments. Neither the uploaded filename nor an executable crosses the API.
func ScanFont(input io.Reader, output io.Writer, maxBytes int64) error {
	return scanFontStream(context.Background(), input, output, maxBytes)
}
func scanFontStream(parent context.Context, input io.Reader, output io.Writer, maxBytes int64) error {
	if maxBytes <= 0 {
		return errors.New("invalid font scan limit")
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "latexmk-font-scan-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file, err := os.OpenFile(filepath.Join(dir, "font"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(file, io.LimitReader(input, maxBytes+1))
	err = errors.Join(copyErr, file.Close())
	if err != nil {
		return err
	}
	if n > maxBytes {
		return safefs.ErrLimit
	}
	run := process.Run(
		ctx,
		process.Spec{
			Name:           "fc-scan",
			Args:           []string{"--format", "%{family}\t%{style}\n", filepath.Join(dir, "font")},
			Env:            []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + dir, "LC_ALL=C"},
			Stdout:         output,
			MaxStreamBytes: 1 << 20,
			MaxOutputBytes: 4096,
		},
	)
	if run.Err != nil {
		return fmt.Errorf("font scan failed: %w", run.Err)
	}
	return nil
}

func (m *Manager) scanFont(
	ctx context.Context,
	baseline model.Baseline,
	source string,
	file model.File,
) ([]model.Font, error) {
	input, err := verifiedOpen(source, file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = input.Close() }()
	var raw strings.Builder
	if m.cfg.RunnerImage != "" || baseline.Image != "" {
		image := baseline.Image
		if image == "" {
			image = m.cfg.RunnerImage
		}
		memory := m.cfg.RunnerMemoryBytes
		if memory <= 0 {
			memory = 1 << 30
		}
		var random [12]byte
		if _, err = rand.Read(random[:]); err != nil {
			return nil, err
		}
		name := "latexmk-font-" + hex.EncodeToString(random[:])
		completed := false
		defer func() {
			if completed {
				return
			}
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = process.Run(
				cleanup,
				process.Spec{Name: "docker", Args: []string{"rm", "--force", name}, MaxOutputBytes: 4096},
			)
		}()
		args := []string{
			"run",
			"--name",
			name,
			"--rm",
			"--pull=never",
			"--label",
			"latexmk.runner=true",
			"--label",
			"latexmk.runner.namespace=" + m.cfg.RunnerNamespace,
			"--network=none",
			"--read-only",
			"--cap-drop=ALL",
			"--security-opt=no-new-privileges",
			"--pids-limit",
			"32",
			"--memory",
			strconv.FormatInt(memory, 10),
			"--memory-swap",
			strconv.FormatInt(memory, 10),
			"--cpus",
			"1",
			"--user",
			"10001:10001",
			"--env",
			"HOME=/tmp",
			"--tmpfs",
			fmt.Sprintf("/tmp:rw,nosuid,nodev,noexec,size=%d,mode=1777", file.Size+(1<<20)),
			"--entrypoint",
			"/usr/local/bin/latexmk-server",
			"-i",
			image,
			"resource-font-scan",
			strconv.FormatInt(file.Size, 10),
		}
		run := process.Run(
			ctx,
			process.Spec{
				Name:           "docker",
				Args:           args,
				Stdin:          input,
				Stdout:         &raw,
				MaxStreamBytes: 1 << 20,
				MaxOutputBytes: 4096,
			},
		)
		if run.Err != nil {
			return nil, fmt.Errorf("isolated font scan failed: %w", run.Err)
		}
		completed = true // --rm already removed a normally completed helper.
	} else {
		if err = scanFontStream(ctx, input, &raw, max(file.Size, 1)); err != nil {
			return nil, err
		}
	}
	var fonts []model.Font
	for _, line := range strings.Split(raw.String(), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) == 2 && strings.TrimSpace(parts[0]) != "" {
			fonts = append(fonts, model.Font{File: file.Path, SHA256: file.SHA256, Family: parts[0], Style: parts[1]})
		}
	}
	if len(fonts) == 0 {
		return nil, errors.New("font file contains no discoverable faces")
	}
	return fonts, nil
}
