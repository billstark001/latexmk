package main

import (
	"errors"

	"github.com/urfave/cli/v3"

	"github.com/billstark001/latexmk/packages/cli/internal/config"
	"github.com/billstark001/latexmk/packages/shared/argv"
)

// loadCommand parses credentials and command options together. In particular,
// credential-looking values of another option are never interpreted a second time.
func loadCommand(name, cwd string, args []string, flags ...cli.Flag) (config.Resolved, *cli.Command, error) {
	credentials := &argv.Options[config.CredentialOptions]{}
	credentials.Value("token", func(options *config.CredentialOptions, value string) error {
		if value == "" {
			return errors.New("--token requires a nonempty value")
		}
		options.Token, options.TokenSet = value, true
		return nil
	})
	credentials.Value("token-file", func(options *config.CredentialOptions, value string) error {
		if value == "" {
			return errors.New("--token-file requires a nonempty value")
		}
		options.TokenFile, options.TokenFileSet = value, true
		return nil
	})
	credentials.Value("token-mode", func(options *config.CredentialOptions, value string) error {
		if value == "" {
			return errors.New("--token-mode requires a nonempty value")
		}
		options.TokenMode, options.TokenModeSet = value, true
		return nil
	})
	credentials.Value("env-file", func(options *config.CredentialOptions, value string) error {
		options.EnvFile = &value
		return nil
	})
	credentials.Bool("no-env-file", func(options *config.CredentialOptions, value bool) error {
		if value {
			empty := ""
			options.EnvFile = &empty
		} else {
			options.EnvFile = nil
		}
		return nil
	})
	parsed, err := argv.Parse(name, args, append(flags, credentials.Flags()...)...)
	if err != nil {
		return config.Resolved{}, parsed, err
	}
	var options config.CredentialOptions
	if err := credentials.Apply(&options); err != nil {
		return config.Resolved{}, parsed, err
	}
	cfg, err := config.Load(cwd, options)
	return cfg, parsed, err
}
