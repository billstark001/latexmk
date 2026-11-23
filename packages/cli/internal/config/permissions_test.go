package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublicationPermissionSettingDefaultsAndRejectsString(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, test := range []struct {
		value   string
		want    bool
		invalid bool
	}{
		{"", false, false},
		{`,"preservePermissions":false`, false, false},
		{`,"preservePermissions":true`, true, false},
		{`,"preservePermissions":"yes"`, false, true},
	} {
		t.Run(test.value, func(t *testing.T) {
			root := t.TempDir()
			data := `{"targets":{"paper":{"entry":"main.tex"}},"publications":{"release":{"target":"paper"` + test.value + `}}}`
			if err := os.WriteFile(filepath.Join(root, FileName), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(root, CredentialOptions{})
			if (err != nil) != test.invalid {
				t.Fatalf("invalid=%t err=%v", test.invalid, err)
			}
			if err == nil && cfg.Publications["release"].PreservePermissions != test.want {
				t.Fatalf("wrong publication permission default/override: %+v", cfg.Publications["release"])
			}
		})
	}
}
