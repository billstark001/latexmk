package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNamingMergeAndEnvironment(t *testing.T) {
	user := isolateUserConfig(t)
	if err := os.MkdirAll(filepath.Join(user, "latexmk"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(user, "latexmk", UserFileName),
		[]byte(`{"naming":{"scope":"build","timeZone":"UTC"},"pdfRetention":{"keepLast":5}}`),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, FileName),
		[]byte(
			`{"naming":{"timeZone":"Asia/Tokyo"},"targets":{"paper":{"entry":"main.tex","pdfTemplate":"{date}.pdf","pdfRetention":{"keepLast":2}}}}`,
		),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, EnvFileName),
		[]byte("LATEXMK_NAME_SCOPE=invocation\nLATEXMK_NAME_TIMEZONE=UTC\n"),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LATEXMK_NAME_TIMEZONE", "Asia/Tokyo")
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Naming.Scope != "invocation" || cfg.Naming.TimeZone != "Asia/Tokyo" || *cfg.PDFRetention.KeepLast != 5 ||
		*cfg.Targets["paper"].PDFRetention.KeepLast != 2 {
		t.Fatalf("settings: %+v", cfg)
	}
}

func TestNamingConfigValidation(t *testing.T) {
	for _, data := range []string{
		`{"naming":null}`, `{"naming":{"scope":"session"}}`, `{"naming":{"timeZone":"Unknown/Nowhere"}}`,
		`{"pdfRetention":{"keepLast":0}}`, `{"pdfRetention":{"keepLast":-1}}`, `{"pdfRetention":{"keepLast":1.5}}`,
		`{"targets":{"x":{"entry":"main.tex","pdf":"x.pdf","pdfTemplate":"{date}.pdf"}}}`,
		`{"targets":{"x":{"entry":"main.tex","pdfRetention":{"keepLast":0}}}}`,
	} {
		t.Run(data, func(t *testing.T) {
			isolateUserConfig(t)
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, FileName), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(root); err == nil {
				t.Fatal("accepted invalid naming configuration")
			}
		})
	}
	isolateUserConfig(t)
	cfg, err := Load(t.TempDir())
	if err != nil || cfg.Naming.Scope != "invocation" || cfg.Naming.TimeZone != "local" || cfg.PDFRetention != nil {
		t.Fatalf("defaults: %+v %v", cfg.Naming, err)
	}
}
