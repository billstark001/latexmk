package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/shared/argv"
)

func resultTestArguments(command string, args []string, opts *resultCommandOptions) error {
	changes := resultArguments()
	parsed, err := argv.Parse(command, args, changes.Flags()...)
	if err != nil {
		return err
	}
	if err := changes.Apply(opts); err != nil {
		return err
	}
	return validateResultArguments(command, parsed.Args().Slice(), parsed, opts)
}

func TestPackArgumentSchemaPreservesValuesAndOrder(t *testing.T) {
	compileChanges, packChanges := compileArguments(), packArguments()
	flags := append(packCompileFlags(compileChanges), packChanges.Flags()...)
	parsed, err := argv.Parse("pack", []string{
		"main.tex", "--output", "first.zip", "--output=final.zip",
		"--name-timezone", "--verify", "--pack-include", "assets/{a,b}.dat",
		"--pack-include=extra.dat", "--publish-resource=font=bundle", "--publish-resource=font=omit",
		"--verify-in=archive-v1", "--verify=false", "--explain=false", "--json=false",
	}, flags...)
	if err != nil {
		t.Fatal(err)
	}
	packing := packOptions{mode: "default"}
	if err := packChanges.Apply(&packing); err != nil {
		t.Fatal(err)
	}
	if err := validatePackArguments(packing, parsed); err != nil {
		t.Fatal(err)
	}
	if packing.output != "final.zip" || packing.verifyMode != "target" || packing.explain ||
		packing.resourceActions["font"] != "omit" || !reflect.DeepEqual(packing.includeFiles, []string{"assets/{a,b}.dat", "extra.dat"}) {
		t.Fatalf("pack options = %+v", packing)
	}
	// Applying records independently proves --verify was a timezone value, not a pack flag.
	opts := compileOptions{timeout: time.Minute}
	if err := compileChanges.Apply(&opts); err != nil {
		t.Fatal(err)
	}
	if opts.nameTimeZone != "--verify" || opts.jsonOutput || parsed.Bool("json") {
		t.Fatalf("compile overrides = %+v", opts)
	}
}

func TestPackFlagsRespectTerminatorAndColdBuildPolicy(t *testing.T) {
	for _, flag := range []string{"--local-cache=none", "--server-cache=none", "--force=false", "-g", "-gg", "-synctex=1", "--no-synctex=false"} {
		t.Run(flag, func(t *testing.T) {
			compileChanges, packChanges := compileArguments(), packArguments()
			flags := append(packCompileFlags(compileChanges), packChanges.Flags()...)
			parsed, err := argv.Parse("pack", []string{flag, "main.tex"}, flags...)
			if err != nil {
				t.Fatal(err)
			}
			if err := validatePackArguments(packOptions{mode: "default"}, parsed); err == nil {
				t.Fatalf("accepted %s", flag)
			}
		})
	}
	compileChanges, packChanges := compileArguments(), packArguments()
	flags := append(packCompileFlags(compileChanges), packChanges.Flags()...)
	parsed, err := argv.Parse("pack", []string{"--", "--verify"}, flags...)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.IsSet("verify") || !reflect.DeepEqual(parsed.Args().Slice(), []string{"--verify"}) {
		t.Fatalf("terminator ignored: %v", parsed.Args().Slice())
	}
}

func TestNameArgumentSchemaRetainsFlagLikeTemplates(t *testing.T) {
	for _, source := range []string{"--json", "--token-file", "--template"} {
		compileChanges, nameChanges := compileArguments(), nameArguments()
		parsed, err := argv.Parse("name.render", []string{"main.tex", "--template", source},
			append(compileChanges.Flags(), nameChanges.Flags()...)...)
		if err != nil {
			t.Fatal(err)
		}
		var options nameRenderOptions
		if err := nameChanges.Apply(&options); err != nil {
			t.Fatal(err)
		}
		if options.template != source || parsed.IsSet("json") {
			t.Fatalf("template %q parsed as %+v", source, options)
		}
	}
	for _, args := range [][]string{{"--template="}, {"--template=a", "--template=b"}} {
		changes := nameArguments()
		if _, err := argv.Parse("name.render", args, changes.Flags()...); err != nil {
			t.Fatal(err)
		}
		if err := changes.Apply(new(nameRenderOptions)); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestJobAndResultArgumentSchemas(t *testing.T) {
	changes := jobsArguments()
	parsed, err := argv.Parse("jobs", []string{"--server", "--json", "job_test", "--timeout=2s"}, changes.Flags()...)
	if err != nil {
		t.Fatal(err)
	}
	opts := jobsOptions{timeout: time.Minute}
	if err := changes.Apply(&opts); err != nil {
		t.Fatal(err)
	}
	if err := validateJobsArguments("show", parsed.Args().Slice(), parsed, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.server != "--json" || opts.jsonOutput || opts.timeout != 2*time.Second || opts.jobID != "job_test" {
		t.Fatalf("jobs options = %+v", opts)
	}
	for _, test := range []struct {
		command string
		args    []string
	}{
		{"diagnostics", []string{"job_test", "--tail=2"}},
		{"logs", []string{"job_test", "--tail=0"}},
		{"logs", []string{"job_test", "--max-bytes=4194305"}},
		{"artifacts.list", []string{"job_test", "--out-dir=output"}},
		{"artifacts.get", []string{"job_test", strings.Repeat("A", 32)}},
	} {
		options := resultCommandOptions{timeout: time.Minute, source: "all", tailLines: 200, maxBytes: 64 << 10}
		if err := resultTestArguments(test.command, test.args, &options); err == nil {
			t.Fatalf("accepted %s %v", test.command, test.args)
		}
	}
}

func TestNameRenderFlagLikeTemplateDoesNotEnableJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.WriteFile("main.tex", []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := captureCommandOutput(t, func() int {
		return run([]string{"latexmk", "name", "render", "main.tex", "--template", "--json"})
	})
	if code != 0 || stdout != "--json\n" || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
