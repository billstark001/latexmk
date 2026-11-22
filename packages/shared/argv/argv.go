// Package argv declares command options and delegates command-line syntax to urfave/cli.
package argv

import (
	"context"
	"io"
	"strconv"

	"github.com/urfave/cli/v3"
)

// Options records explicit overrides in input order. Apply runs them after the
// command's configuration has supplied defaults, without reparsing its arguments.
type Options[T any] struct {
	flags []cli.Flag
	steps []func(*T) error
}

func (o *Options[T]) Value(name string, apply func(*T, string) error, aliases ...string) {
	o.flags = append(o.flags, &cli.GenericFlag{
		Name: name, Aliases: aliases,
		Value: &optionValue{current: "", set: func(raw string) (any, error) {
			o.steps = append(o.steps, func(target *T) error { return apply(target, raw) })
			return raw, nil
		}},
	})
}

func (o *Options[T]) Bool(name string, apply func(*T, bool) error, aliases ...string) {
	o.flags = append(o.flags, &cli.GenericFlag{
		Name: name, Aliases: aliases,
		Value: &optionValue{current: false, boolean: true, set: func(raw string) (any, error) {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				return nil, err
			}
			o.steps = append(o.steps, func(target *T) error { return apply(target, value) })
			return value, nil
		}},
	})
}

func (o *Options[T]) Flags() []cli.Flag { return o.flags }

func (o *Options[T]) Apply(target *T) error {
	for _, apply := range o.steps {
		if err := apply(target); err != nil {
			return err
		}
	}
	return nil
}

type optionValue struct {
	current any
	boolean bool
	set     func(string) (any, error)
}

func (v *optionValue) Set(raw string) error {
	value, err := v.set(raw)
	if err != nil {
		return err
	}
	v.current = value
	return nil
}

func (v *optionValue) Get() any         { return v.current }
func (v *optionValue) String() string   { return "" }
func (v *optionValue) IsBoolFlag() bool { return v.boolean }

// Parse returns the library's parsed command even on failure, so callers can
// format errors using options already parsed. It never prints help or exits.
func Parse(name string, args []string, flags ...cli.Flag) (*cli.Command, error) {
	command := &cli.Command{
		Name: name, Flags: flags,
		HideHelp: true, HideHelpCommand: true, HideVersion: true,
		Writer: io.Discard, ErrWriter: io.Discard,
		OnUsageError:   func(_ context.Context, _ *cli.Command, err error, _ bool) error { return err },
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Action:         func(context.Context, *cli.Command) error { return nil },
	}
	err := command.Run(context.Background(), append([]string{name}, args...))
	return command, err
}
