// Package process owns bounded subprocess execution and cancellation. Callers
// choose arguments, environment and deadlines; this package never invokes a shell.
package process

import (
	"context"
	"errors"
	"os/exec"
)

type Spec struct {
	Name string
	Args []string
	Dir  string
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
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...)
	cmd.Dir, cmd.Env = spec.Dir, spec.Env
	configureProcess(cmd)
	stdout, stderr := newCappedBuffer(spec.MaxOutputBytes), newCappedBuffer(spec.MaxOutputBytes)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if spec.CombinedOutput {
		cmd.Stderr = stdout
	}
	err := cmd.Start()
	if err == nil {
		// Also terminate surviving descendants when the direct child exits normally.
		defer terminateProcessTree(cmd)
		err = cmd.Wait()
	}
	if ctx.Err() != nil {
		err = ctx.Err()
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
