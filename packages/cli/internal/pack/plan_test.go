package pack

import (
	"strings"
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/shared/resources"
)

func publicationFixture() (resources.Resource, []projectarchive.File) {
	hash := strings.Repeat("a", 64)
	resource := resources.Resource{ID: "body-font", Type: "files", Kind: "font", Mount: "fonts/body",
		Redistribution: "allow", LicenseFiles: []string{"LICENSE"},
		Files: []resources.File{{Path: "Body.otf", SHA256: hash, Size: 100}, {Path: "LICENSE", SHA256: hash, Size: 12}}}
	files := []projectarchive.File{{Path: "fonts/body/Body.otf", SHA256: hash, Size: 100},
		{Path: "fonts/body/LICENSE", SHA256: hash, Size: 12}}
	return resource, files
}

func TestPublicationResourceDispositionIsExplicit(t *testing.T) {
	resource, files := publicationFixture()
	for _, action := range []string{"", "omit", "provided", "bundle"} {
		t.Run(action, func(t *testing.T) {
			planner, err := NewPlanner(PlanOptions{
				Entry: "main.tex",
				Mode:  "default",
				Resources: []resources.Resource{
					resource,
				},
				ResourceFiles: map[string][]projectarchive.File{resource.ID: files},
				Actions:       map[string]string{resource.ID: action},
			})
			if action == "" {
				// Omitted map entry and an explicitly empty action are different.
				if err == nil {
					t.Fatal("accepted an invalid explicit action")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			plan, err := planner.Build([]projectarchive.File{{Path: "main.tex"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if action == "bundle" {
				want = 3
			}
			if len(plan.Files) != want || (len(planner.Provided()) == 1) != (action == "provided") {
				t.Fatalf("wrong disposition: %+v", plan)
			}
		})
	}
	planner, err := NewPlanner(PlanOptions{Mode: "default", Resources: []resources.Resource{resource}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Build(nil, nil)
	if err != nil || len(plan.Files) != 0 || plan.Resources[0].Action != "omit" {
		t.Fatalf("resources became public implicitly: %+v %v", plan, err)
	}
}

func TestBundlingRequiresPermissionAndPreservesLicense(t *testing.T) {
	for _, change := range []string{"denied", "unknown", "no-license", "excluded-license", "tampered"} {
		t.Run(change, func(t *testing.T) {
			resource, files := publicationFixture()
			options := PlanOptions{
				Mode:      "default",
				Resources: []resources.Resource{resource},
				Actions: map[string]string{
					resource.ID: "bundle",
				},
				ResourceFiles: map[string][]projectarchive.File{resource.ID: files},
			}
			switch change {
			case "denied":
				options.Resources[0].Redistribution = "deny"
			case "unknown":
				options.Resources[0].Redistribution = "unknown"
			case "no-license":
				options.Resources[0].LicenseFiles = nil
			case "excluded-license":
				options.Exclude = []string{"fonts/**/LICENSE"}
			case "tampered":
				options.ResourceFiles[resource.ID][0].SHA256 = strings.Repeat("b", 64)
			}
			if _, err := NewPlanner(options); err == nil {
				t.Fatal("accepted an invalid resource publication")
			}
		})
	}
}

func TestPublicationExclusionsCannotRemoveEntryOrHideCollisions(t *testing.T) {
	planner, err := NewPlanner(PlanOptions{Entry: "main.tex", Mode: "default", Exclude: []string{"**/*.tex"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner.Build([]projectarchive.File{{Path: "main.tex"}}, nil); err == nil {
		t.Fatal("excluded the entry")
	}
	resource, files := publicationFixture()
	planner, err = NewPlanner(PlanOptions{
		Mode:          "default",
		Resources:     []resources.Resource{resource},
		Actions:       map[string]string{resource.ID: "bundle"},
		ResourceFiles: map[string][]projectarchive.File{resource.ID: files},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner.Build(
		[]projectarchive.File{{Path: "fonts/body/Body.otf", SHA256: strings.Repeat("b", 64), Size: 100}},
		nil,
	); err == nil {
		t.Fatal("overwrote a different source member")
	}
}

func TestMembersDigestIsOrderIndependentAndContentSensitive(t *testing.T) {
	first := []projectarchive.File{{Path: "a.tex", SHA256: "a", Size: 1}, {Path: "b.tex", SHA256: "b", Size: 2}}
	second := []projectarchive.File{first[1], first[0]}
	a, _ := MembersDigest(first, nil)
	b, _ := MembersDigest(second, nil)
	if a != b {
		t.Fatal("digest depends on member order")
	}
	second[0].SHA256 = "changed"
	c, _ := MembersDigest(second, nil)
	if a == c {
		t.Fatal("digest ignored content")
	}
}

func TestPlannerKeepsOnePolicyAcrossBuildStages(t *testing.T) {
	resource, files := publicationFixture()
	options := PlanOptions{Entry: "main.tex", Mode: "default", Exclude: []string{"notes/**"},
		Resources: []resources.Resource{resource}, Actions: map[string]string{resource.ID: "bundle"},
		ResourceFiles: map[string][]projectarchive.File{resource.ID: files}}
	planner, err := NewPlanner(options)
	if err != nil {
		t.Fatal(err)
	}
	// Later option/capture mutations must not change a policy already used to
	// build the source package or accumulate generated members between plans.
	options.Exclude[0] = "**"
	options.Actions[resource.ID] = "omit"
	options.ResourceFiles[resource.ID][0].SHA256 = strings.Repeat("b", 64)
	sources := []projectarchive.File{{Path: "main.tex"}, {Path: "notes/draft.tex"}}
	withGenerated, err := planner.Build(sources, []projectarchive.File{{Path: "main.bbl"}})
	if err != nil {
		t.Fatal(err)
	}
	withoutGenerated, err := planner.Build(sources, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(withGenerated.Files) != 4 || len(withoutGenerated.Files) != 3 {
		t.Fatalf("plans shared mutable stage state: %+v %+v", withGenerated.Files, withoutGenerated.Files)
	}
	for _, file := range withoutGenerated.Files {
		if file.Path == "notes/draft.tex" || file.Path == "main.bbl" || file.SHA256 == strings.Repeat("b", 64) {
			t.Fatalf("compiled policy changed: %+v", file)
		}
	}
}

func TestPlannerProvidedRequirementsDoNotShareMutableSlices(t *testing.T) {
	options := PlanOptions{
		Mode:      "default",
		Resources: []resources.Resource{{ID: "math", Type: "texlive", Packages: []string{"algorithms"}}},
		Actions:   map[string]string{"math": "provided"},
	}
	planner, err := NewPlanner(options)
	if err != nil {
		t.Fatal(err)
	}
	options.Resources[0].Packages[0] = "changed-input"
	provided := planner.Provided()
	provided[0].Packages[0] = "changed-result"
	if got := planner.Provided()[0].Packages[0]; got != "algorithms" {
		t.Fatalf("provided requirement escaped frozen policy: %s", got)
	}
}
