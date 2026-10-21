package naming

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTypedFormattingAndEscapes(t *testing.T) {
	at := time.Date(2026, 10, 22, 1, 2, 3, 123000000, time.FixedZone("JST", 9*3600))
	values := Values{
		"entry.stem": String("论文&{unchanged}"),
		"git.commit": Hash("0123456789abcdef"),
		"datetime":   Time{Time: at},
	}
	for _, test := range []struct{ source, want string }{
		{"{{{entry.stem}}}-{git.commit:8}.pdf", "{论文&{unchanged}}-01234567.pdf"},
		{"{git.commit}-{git.commit:12}", "0123456789abcdef-0123456789ab"},
		{"{datetime}", "20261022T010203"},
		{"{datetime:iso8601}", "2026-10-22T01:02:03+09:00"},
		{"{datetime:rfc3339nano}", "2026-10-22T01:02:03.123+09:00"},
		{"{datetime:go:2006-01-02 15:04:05}", "2026-10-22 01:02:03"},
		{"{datetime:date}-{datetime:time}-{datetime:compact}", "2026-10-22-01-02-03-20261022T010203"},
		{"plain {{braces}}", "plain {braces}"},
	} {
		t.Run(test.source, func(t *testing.T) {
			template, err := Parse(test.source)
			if err != nil {
				t.Fatal(err)
			}
			got, err := template.Render(values)
			if err != nil || got != test.want {
				t.Fatalf("got %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestStrictGrammarAndFormats(t *testing.T) {
	for _, source := range []string{"{", "}", "{}", "{a:}", "{a..b}", "{a b}", "{x:{y}}", "{.a}", "{1a}", "\x00", string([]byte{255}), strings.Repeat("x", MaxBytes+1)} {
		if _, err := Parse(source); err == nil {
			t.Errorf("accepted invalid template %q", source[:min(len(source), 30)])
		}
	}
	values := Values{"s": String("x"), "h": Hash("abcd1234"), "t": Time{Time: time.Now()}}
	for _, source := range []string{"{missing}", "{s:8}", "{h:0}", "{h:-1}", "{h:+2}", "{h:9}", "{h:short}", "{t:iso860}", "{t:20060102}", "{t:go:}"} {
		template, err := Parse(source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := template.Render(values); err == nil {
			t.Errorf("accepted invalid format %q", source)
		}
	}
}

type customValue string

func (v customValue) Format(spec string) (string, error) { return string(v) + ":" + spec, nil }

func TestDependenciesAndCustomValuesAreIndependentOfCLI(t *testing.T) {
	template, err := Parse("{custom.part:a:b}/{custom.part}/{other}")
	if err != nil {
		t.Fatal(err)
	}
	names := template.Variables()
	if fmt.Sprint(names) != "[custom.part other]" {
		t.Fatal(names)
	}
	names[0] = "changed"
	if template.Variables()[0] != "custom.part" {
		t.Fatal("mutable dependency list")
	}
	values := Values{"custom.part": customValue("value"), "other": String("end")}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			got, err := template.Render(values)
			if err != nil || got != "value:a:b/value:/end" {
				t.Errorf("render: %q %v", got, err)
			}
		})
	}
	workers.Wait()
}

func TestOutputLimitIncludesLiteralAndExpandedBytes(t *testing.T) {
	template, err := Parse("x{s}{s}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := template.Render(Values{"s": String(strings.Repeat("x", MaxBytes/2))}); err == nil {
		t.Fatal("unbounded output")
	}
}

func FuzzTemplateGrammar(f *testing.F) {
	for _, source := range []string{"{s}", "{{{s}}}", "{s:go:15:04}", "x}", "{{}"} {
		f.Add(source)
	}
	f.Fuzz(func(t *testing.T, source string) {
		template, err := Parse(source)
		if err != nil {
			return
		}
		output, err := template.Render(Values{"s": String("{s}")})
		if err == nil && len(output) > MaxBytes {
			t.Fatal("output limit bypassed")
		}
	})
}
