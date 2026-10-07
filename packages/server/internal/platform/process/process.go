// Package process owns bounded subprocess execution and cancellation. Callers
// choose arguments, environment and deadlines; this package never invokes a shell.
package process

import (
	"context"
	"errors"
	"io"
	"os/exec"
)

type Spec struct {
	Stdin          io.Reader
	Stdout         io.Writer
	MaxStreamBytes int64
	Name           string
	Args           []string
	Dir            string
	// A nil Env inherits the host environment. Compilers must supply their whitelist.
	Env            []string
	MaxOutputBytes int64
	CombinedOutput bool
}

type Result struct {
	Stdout, Stderr                   []byte
	StdoutTruncated, StderrTruncated bool
	ExitCode                         int
	Err                              error
}

func Run(ctx context.Context, spec Spec) Result {
	if spec.MaxOutputBytes < 0 {
		return Result{ExitCode: -1, Err: errors.New("negative process output limit")}
	}
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Dir, cmd.Env = spec.Dir, spec.Env
	configureProcess(cmd)
	stdout, stderr := newCappedBuffer(spec.MaxOutputBytes), newCappedBuffer(spec.MaxOutputBytes)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = stdout, stderr, spec.Stdin
	var stream *streamWriter
	if spec.Stdout != nil {
		if spec.MaxStreamBytes <= 0 {
			return Result{ExitCode: -1, Err: errors.New("stream output requires a positive byte limit")}
		}
		stream = &streamWriter{dst: spec.Stdout, remaining: spec.MaxStreamBytes, cancel: cancel}
		cmd.Stdout = stream
	}
	if spec.CombinedOutput {
		cmd.Stderr = stdout
	}
	err := cmd.Start()
	if err == nil {
		// Also terminate surviving descendants when the direct child exits normally.
		defer terminateProcessTree(cmd)
		err = cmd.Wait()
	}
	if parent.Err() != nil {
		err = parent.Err()
	}
	if stream != nil && stream.err != nil {
		err = errors.Join(err, stream.err)
	}
	code := -1
	if err == nil {
		code = 0
	} else if e, ok := errors.AsType[*exec.ExitError](err); ok {
		code = e.ExitCode()
	}
	return Result{
		Stdout:          stdout.Bytes(),
		Stderr:          stderr.Bytes(),
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
		ExitCode:        code,
		Err:             err,
	}
}
