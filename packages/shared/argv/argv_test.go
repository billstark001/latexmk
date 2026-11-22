package argv

import (
	"reflect"
	"testing"
)

func TestLibraryParsingPreservesOrderValuesAndTerminator(t *testing.T) {
	type options struct {
		files []string
		json  bool
	}
	changes := new(Options[options])
	changes.Value("include", func(target *options, value string) error {
		target.files = append(target.files, value)
		return nil
	}, "I")
	changes.Bool("clear", func(target *options, value bool) error {
		if value {
			target.files = nil
		}
		return nil
	})
	changes.Bool("json", func(target *options, value bool) error { target.json = value; return nil })
	parsed, err := Parse("example", []string{
		"entry.tex", "--include", "{a,b}", "--clear", "-I", "--json", "--json=false", "--", "--clear",
	}, changes.Flags()...)
	if err != nil {
		t.Fatal(err)
	}
	target := options{files: []string{"inherited"}, json: true}
	if err := changes.Apply(&target); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(target.files, []string{"--json"}) || target.json || parsed.Bool("json") {
		t.Fatalf("options = %#v, json = %t", target, parsed.Bool("json"))
	}
	if !reflect.DeepEqual(parsed.Args().Slice(), []string{"entry.tex", "--clear"}) || !parsed.IsSet("include") {
		t.Fatalf("parsed args = %#v", parsed.Args().Slice())
	}
}

func TestLibraryAcceptsSingleDashLongFlagsAndRetainsParsedErrorFormat(t *testing.T) {
	type options struct{ output string }
	changes := new(Options[options])
	changes.Value(
		"out-dir",
		func(target *options, value string) error { target.output = value; return nil },
		"output-directory",
	)
	changes.Bool("json", func(*options, bool) error { return nil })
	parsed, err := Parse("example", []string{"--json", "-output-directory=a b", "--unknown"}, changes.Flags()...)
	if err == nil || !parsed.Bool("json") || !parsed.IsSet("out-dir") {
		t.Fatalf("parsed json = %t, err = %v", parsed.Bool("json"), err)
	}
	var target options
	if err := changes.Apply(&target); err != nil || target.output != "a b" {
		t.Fatalf("output = %q, err = %v", target.output, err)
	}
}
