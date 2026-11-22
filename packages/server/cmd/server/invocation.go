package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/urfave/cli/v3"

	"github.com/billstark001/latexmk/packages/server/internal/resources"
	"github.com/billstark001/latexmk/packages/server/internal/sandbox"
)

var (
	errServerInvocation = errors.New("invalid server invocation")
	errWorkerLimits     = errors.New("invalid worker limits")
	errFontLimit        = errors.New("invalid font scan limit")
)

type internalInvocation struct {
	command  string
	maxBytes int64
	maxFiles int
}

// The internal worker protocol has positional limits rather than flags. Let the
// library select the command, then validate its fixed decimal argument values.
func parseInternalInvocation(args []string) (internalInvocation, error) {
	var invocation internalInvocation
	command := &cli.Command{
		Name: "latexmk-server", HideHelp: true, HideHelpCommand: true, HideVersion: true,
		Writer: io.Discard, ErrWriter: io.Discard,
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		OnUsageError:   func(context.Context, *cli.Command, error, bool) error { return errServerInvocation },
		Action: func(_ context.Context, c *cli.Command) error {
			if c.Args().Present() {
				return errServerInvocation
			}
			return nil
		},
		Commands: []*cli.Command{
			{
				Name:            "compile-worker",
				HideHelp:        true,
				SkipFlagParsing: true,
				Action: func(_ context.Context, c *cli.Command) error {
					if c.Args().Len() != 2 {
						return errServerInvocation
					}
					maxBytes, err := strconv.ParseInt(c.Args().Get(0), 10, 64)
					maxFiles, filesErr := strconv.Atoi(c.Args().Get(1))
					if err != nil || filesErr != nil || maxBytes <= 0 || maxFiles <= 0 {
						return errWorkerLimits
					}
					invocation = internalInvocation{command: c.Name, maxBytes: maxBytes, maxFiles: maxFiles}
					return nil
				},
			},
			{
				Name:            "resource-font-scan",
				HideHelp:        true,
				SkipFlagParsing: true,
				Action: func(_ context.Context, c *cli.Command) error {
					if c.Args().Len() != 1 {
						return errServerInvocation
					}
					maxBytes, err := strconv.ParseInt(c.Args().First(), 10, 64)
					if err != nil || maxBytes <= 0 {
						return errFontLimit
					}
					invocation = internalInvocation{command: c.Name, maxBytes: maxBytes}
					return nil
				},
			},
		},
	}
	err := command.Run(context.Background(), append([]string{command.Name}, args...))
	return invocation, err
}

func runInternal(args []string, input io.Reader, output, stderr io.Writer) (bool, int) {
	invocation, err := parseInternalInvocation(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return true, 2
	}
	if invocation.command == "" {
		return false, 0
	}
	switch invocation.command {
	case "compile-worker":
		err = sandbox.Worker(input, output, invocation.maxBytes, invocation.maxFiles)
	case "resource-font-scan":
		err = resources.ScanFont(input, output, invocation.maxBytes)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return true, 1
	}
	return true, 0
}
