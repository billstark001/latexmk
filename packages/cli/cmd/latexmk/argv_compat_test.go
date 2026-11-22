// The fixture records parser outcomes from 6d46e77, before the library migration.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/shared/argv"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	resources "github.com/billstark001/latexmk/packages/shared/resources"
)

type argvCompatibilityCase struct {
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Action    string         `json:"action,omitempty"`
	Args      []string       `json:"args"`
	WantError bool           `json:"wantError,omitempty"`
	Want      map[string]any `json:"want,omitempty"`
}

func compatibilityCompileSeed() compileOptions {
	return compileOptions{
		server:          "https://service.example",
		projectRoot:     "/project",
		rootMode:        "entry",
		uploadMode:      "auto",
		engine:          "xelatex",
		timeout:         3 * time.Minute,
		interaction:     "nonstopmode",
		synctex:         true,
		haltOnError:     true,
		fileLineError:   true,
		gitIgnore:       true,
		watchInterval:   500 * time.Millisecond,
		watchDebounce:   500 * time.Millisecond,
		watchMaxWait:    2500 * time.Millisecond,
		nameScope:       "invocation",
		nameTimeZone:    "UTC",
		ignoreFiles:     []string{"inherited.ignore"},
		includeFiles:    []string{"inherited.dat"},
		resourceIDs:     []string{"inherited"},
		resourceOptions: resources.Options{LockMode: "update", PrepareTimeout: "5m"},
		auxiliary:       protocol.AuxiliaryOptions{Local: "none", Server: "none"},
	}
}
func compatibilityJobsSeed() jobsOptions {
	return jobsOptions{server: "https://service.example", timeout: 3 * time.Minute, limit: protocol.DefaultJobListLimit}
}
func compatibilityResultSeed() resultCommandOptions {
	return resultCommandOptions{
		server:    "https://service.example",
		timeout:   3 * time.Minute,
		outDir:    "out",
		source:    "all",
		tailLines: 200,
		maxBytes:  512 << 10,
	}
}
func compatibilityRemoteSeed() remoteCleanOptions {
	return remoteCleanOptions{server: "https://service.example", timeout: 3 * time.Minute, scope: "results"}
}

const compatibilityCompileFields = "resourceOptions resourceIDs resourceAddIDs resourceRemoveIDs resourceExplicit requires environment environmentExplicit explain ignoreFiles unmatchedGlob target pdfExport pdfTemplate pdfKeepLast nameScope nameTimeZone auxiliary server projectRoot projectID rootMode uploadMode manifestFile includeFiles gitIgnore engine engineExplicit outDir timeout interaction synctex haltOnError fileLineError shellEscape jobName force quiet jsonOutput dryRun detach watch realtime watchInterval watchDebounce watchMaxWait insecure entry exclude"

func compatibilityView(value any, prefix string, fields string) map[string]any {
	result := map[string]any{}
	v := reflect.ValueOf(value)
	if fields == "" {
		names := []string{}
		for i := 0; i < v.NumField(); i++ {
			names = append(names, v.Type().Field(i).Name)
		}
		fields = strings.Join(names, " ")
	}
	for _, name := range strings.Fields(fields) {
		field := v.FieldByName(name)
		if !field.IsValid() {
			panic("missing contract field " + name)
		}
		result[prefix+name] = compatibilityValue(field)
	}
	return result
}
func compatibilityValue(v reflect.Value) any {
	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return compatibilityValue(v.Elem())
	case reflect.Slice:
		if v.IsNil() {
			return nil
		}
		result := []any{}
		for i := 0; i < v.Len(); i++ {
			result = append(result, compatibilityValue(v.Index(i)))
		}
		return result
	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		result := map[string]any{}
		it := v.MapRange()
		for it.Next() {
			result[it.Key().String()] = compatibilityValue(it.Value())
		}
		return result
	case reflect.Struct:
		result := map[string]any{}
		for i := 0; i < v.NumField(); i++ {
			result[v.Type().Field(i).Name] = compatibilityValue(v.Field(i))
		}
		return result
	default:
		panic("unsupported contract value " + v.Kind().String())
	}
}
func compatibilityChanges(value, base map[string]any) map[string]any {
	result := map[string]any{}
	for key, v := range value {
		a, _ := json.Marshal(v)
		b, _ := json.Marshal(base[key])
		if string(a) != string(b) {
			result[key] = v
		}
	}
	return result
}
func compatibilityMerge(a, b map[string]any) map[string]any {
	for key, v := range b {
		a[key] = v
	}
	return a
}

func TestDefinedArgvBehaviorMatchesBeforeLibraryMigration(t *testing.T) {
	raw, err := os.ReadFile("testdata/argv_compat.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []argvCompatibilityCase
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, item := range cases {
		t.Run(item.Name, func(t *testing.T) {
			current, base, err := compatibilityParse(item)
			if (err != nil) != item.WantError {
				t.Fatalf("args %#v: error=%v, expected failure=%t", item.Args, err, item.WantError)
			}
			if err != nil {
				return
			}
			got := compatibilityChanges(current, base)
			want := item.Want
			if want == nil {
				want = map[string]any{}
			}
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			wantJSON, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Errorf("args %#v\n got %s\nwant %s", item.Args, gotJSON, wantJSON)
			}
		})
	}
}
func compatibilityParse(item argvCompatibilityCase) (current, base map[string]any, err error) {
	switch item.Kind {
	case "compile":
		opts := compatibilityCompileSeed()
		base = compatibilityView(opts, "", compatibilityCompileFields)
		changes := compileArguments()
		parsed, parseErr := argv.Parse("compile", item.Args, changes.Flags()...)
		err = parseErr
		if err == nil {
			err = applyCompileArguments(changes, &opts, parsed)
		}
		current = compatibilityView(opts, "", compatibilityCompileFields)
	case "pack":
		opts := compatibilityCompileSeed()
		packing := packOptions{mode: "default", resourceActions: map[string]string{}}
		base = compatibilityMerge(
			compatibilityView(opts, "compile.", compatibilityCompileFields),
			compatibilityView(packing, "pack.", ""),
		)
		compileChanges, packChanges := compileArguments(), packArguments()
		parsed, parseErr := argv.Parse(
			"pack",
			item.Args,
			append(packCompileFlags(compileChanges), packChanges.Flags()...)...)
		err = parseErr
		if err == nil {
			err = packChanges.Apply(&packing)
		}
		if err == nil {
			err = validatePackArguments(packing, parsed)
		}
		if err == nil {
			err = applyCompileArguments(compileChanges, &opts, parsed)
		}
		current = compatibilityMerge(
			compatibilityView(opts, "compile.", compatibilityCompileFields),
			compatibilityView(packing, "pack.", ""),
		)
	case "name":
		opts := compatibilityCompileSeed()
		base = compatibilityView(opts, "", compatibilityCompileFields)
		base["template"] = ""
		compileChanges, nameChanges := compileArguments(), nameArguments()
		parsed, parseErr := argv.Parse("name", item.Args, append(compileChanges.Flags(), nameChanges.Flags()...)...)
		err = parseErr
		var source nameRenderOptions
		if err == nil {
			err = nameChanges.Apply(&source)
		}
		if err == nil && source.count == 0 {
			err = fmt.Errorf("missing template")
		}
		if err == nil {
			err = applyCompileArguments(compileChanges, &opts, parsed)
		}
		current = compatibilityView(opts, "", compatibilityCompileFields)
		current["template"] = source.template
	case "jobs":
		opts := compatibilityJobsSeed()
		base = compatibilityView(opts, "", "")
		changes := jobsArguments()
		parsed, parseErr := argv.Parse("jobs", item.Args, changes.Flags()...)
		err = parseErr
		if err == nil {
			err = changes.Apply(&opts)
		}
		if err == nil {
			err = validateJobsArguments(item.Action, parsed.Args().Slice(), parsed, &opts)
		}
		current = compatibilityView(opts, "", "")
	case "result":
		opts := compatibilityResultSeed()
		base = compatibilityView(opts, "", "")
		changes := resultArguments()
		parsed, parseErr := argv.Parse("result", item.Args, changes.Flags()...)
		err = parseErr
		if err == nil {
			err = changes.Apply(&opts)
		}
		if err == nil {
			err = validateResultArguments(item.Action, parsed.Args().Slice(), parsed, &opts)
		}
		current = compatibilityView(opts, "", "")
	case "remote":
		opts := compatibilityRemoteSeed()
		base = compatibilityView(opts, "", "")
		changes := remoteCleanArguments()
		parsed, parseErr := argv.Parse("remote", item.Args, changes.Flags()...)
		err = parseErr
		if err == nil {
			err = changes.Apply(&opts)
		}
		if err == nil && parsed.Args().Len() != 0 {
			err = fmt.Errorf("unexpected positionals")
		}
		if err == nil {
			err = validateRemoteCleanOptions(opts)
		}
		current = compatibilityView(opts, "", "")
	default:
		err = fmt.Errorf("unknown fixture kind %q", item.Kind)
	}
	return
}
