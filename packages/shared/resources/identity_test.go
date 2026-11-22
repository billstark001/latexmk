package resources

import (
	"strings"
	"testing"
)

func TestEnvironmentIdentityCanonicalAndSensitiveToContent(t *testing.T) {
	env := Environment{Baseline: Baseline{ID: "default", Digest: strings.Repeat("a", 64)}, Resources: []Resource{
		{ID: "tex", Type: "texlive", Packages: []string{"algorithms", "algorithmicx"}},
		{ID: "fonts", Type: "files", Kind: "font", Mount: "fonts", Redistribution: "allow",
			Files: []File{{Path: "main.otf", SHA256: strings.Repeat("b", 64), Size: 10}}},
	}}
	first, err := EnvironmentDigest(env)
	if err != nil {
		t.Fatal(err)
	}
	env.Ready, env.Missing = true, []string{"ignored cache status"}
	env.Resources[0].Packages = []string{"algorithmicx", "algorithms", "algorithms"}
	env.Resources[1].Redistribution = "deny"
	second, err := EnvironmentDigest(env)
	if err != nil || first != second {
		t.Fatalf("canonical identity changed: %s %s %v", first, second, err)
	}
	env.Resources[1].Files[0].SHA256 = strings.Repeat("c", 64)
	third, err := EnvironmentDigest(env)
	if err != nil || first == third {
		t.Fatalf("font content did not change identity: %v", err)
	}
}

func TestResourcePathsAndCollisions(t *testing.T) {
	for _, name := range []string{"../secret", ".latexmk-home/config", ".git/config", "fonts/../../secret", "fonts/x\n.otf"} {
		if _, err := Destination(".", name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	_, err := NormalizeRequest(ResolveRequest{Resources: []Resource{
		{ID: "one", Type: "files", Files: []File{{Path: "a.sty", SHA256: strings.Repeat("a", 64)}}},
		{ID: "two", Type: "files", Files: []File{{Path: "a.sty", SHA256: strings.Repeat("b", 64)}}},
	}})
	if err == nil {
		t.Fatal("accepted conflicting resources")
	}
}

func TestUnresolvedServerResourcesStillValidateTheirMount(t *testing.T) {
	for _, mount := range []string{"../fonts", "fonts/../private", ".latexmk-home/fonts", "fonts/.env"} {
		_, err := NormalizeRequest(ResolveRequest{Resources: []Resource{
			{ID: "registered", Type: "server", Ref: "fonts-v1", Mount: mount},
		}})
		if err == nil {
			t.Errorf("accepted unresolved resource mount %q", mount)
		}
	}
	for _, mount := range []string{"", ".", "fonts/body"} {
		got, err := MountPath(mount)
		if err != nil || got == "" {
			t.Fatalf("rejected resource directory %q: %q %v", mount, got, err)
		}
	}
}

func TestEnvironmentPackagesAreCanonicalSets(t *testing.T) {
	env := Environment{Baseline: Baseline{ID: "default", Digest: strings.Repeat("a", 64)}, Packages: []Package{
		{Name: "algorithms", Revision: 1}, {Name: "algorithmicx", Revision: 2},
	}}
	first, err := EnvironmentDigest(env)
	if err != nil {
		t.Fatal(err)
	}
	env.Packages = []Package{env.Packages[1], env.Packages[0], env.Packages[0]}
	second, err := EnvironmentDigest(env)
	if err != nil || first != second {
		t.Fatalf("equivalent package closure changed identity: %s %s %v", first, second, err)
	}
	env.Packages = nil
	empty, err := EnvironmentDigest(env)
	if err != nil {
		t.Fatal(err)
	}
	env.Packages = []Package{}
	second, err = EnvironmentDigest(env)
	if err != nil || empty != second {
		t.Fatal("nil and empty package closures have different identities", err)
	}
}
