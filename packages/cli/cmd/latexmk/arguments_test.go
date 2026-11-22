package main

import (
	"testing"

	"github.com/billstark001/latexmk/packages/shared/argv"
)

func TestCredentialsAreValidatedAtEachOccurrence(t *testing.T) {
	for _, name := range []string{"token", "token-file", "token-mode"} {
		t.Run(name, func(t *testing.T) {
			_, _, err := loadCommand("test", t.TempDir(), []string{"--" + name + "=", "--" + name + "=valid"})
			if err == nil {
				t.Fatal("a later value concealed an earlier invalid credential option")
			}
		})
	}
}

func TestSyntaxErrorsUseOnlyAlreadyParsedOutputOptions(t *testing.T) {
	type outputOptions struct{ json bool }
	for _, test := range []struct {
		args []string
		json bool
	}{
		{[]string{"--json", "--unknown"}, true},
		{[]string{"--unknown", "--json"}, false},
		{[]string{"--json=false", "--unknown"}, false},
	} {
		changes := new(argv.Options[outputOptions])
		changes.Bool("json", func(options *outputOptions, value bool) error { options.json = value; return nil })
		parsed, err := argv.Parse("test", test.args, changes.Flags()...)
		if err == nil || parsed.Bool("json") != test.json {
			t.Fatalf("args=%#v json=%t error=%v", test.args, parsed.Bool("json"), err)
		}
	}
}
