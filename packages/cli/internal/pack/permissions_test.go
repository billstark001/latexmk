package pack

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"testing"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/shared/resources"
)

func permissionPointer(mode os.FileMode) *os.FileMode { return &mode }

func TestPermissionPlanRequiresOptInAndAuthorizedLocalSource(t *testing.T) {
	resource, files := publicationFixture()
	for i := range files {
		files[i].SourcePermissions = permissionPointer(0750)
	}
	for _, preserve := range []bool{false, true} {
		for _, kind := range []string{"files", "server"} {
			t.Run(kind+"-"+map[bool]string{false: "normalized", true: "requested"}[preserve], func(t *testing.T) {
				resource.Type = kind
				planner, err := NewPlanner(PlanOptions{
					Mode:                "default",
					PreservePermissions: preserve,
					Resources: []resources.Resource{
						resource,
					},
					ResourceFiles: map[string][]projectarchive.File{resource.ID: files},
					Actions:       map[string]string{resource.ID: "bundle"},
				})
				if err != nil {
					t.Fatal(err)
				}
				plan, err := planner.Build(
					[]projectarchive.File{
						{Path: "main.tex", SourcePermissions: permissionPointer(0700)},
						{
							Path: "unknown.dat",
						},
					},
					[]projectarchive.File{{Path: "main.bbl", SourcePermissions: permissionPointer(0777)}},
				)
				if err != nil {
					t.Fatal(err)
				}
				wantProject, wantResource := os.FileMode(0644), os.FileMode(0644)
				if preserve {
					wantProject = 0700
					if kind == "files" {
						wantResource = 0750
					}
				}
				if plan.Permissions["main.tex"] != wantProject ||
					plan.Permissions["fonts/body/Body.otf"] != wantResource ||
					plan.Permissions["main.bbl"] != 0644 ||
					plan.Permissions["unknown.dat"] != 0644 {
					t.Fatalf("wrong effective permission policy: %+v", plan)
				}
				for _, member := range plan.Members {
					if member.FileMode == "" || member.PermissionReason == "" || member.PermissionPolicy == "" {
						t.Fatalf("permission decision missing from plan: %+v", member)
					}
				}
			})
		}
	}
	resource.Redistribution = "deny"
	if _, err := NewPlanner(
		PlanOptions{
			Mode:                "default",
			PreservePermissions: true,
			Resources:           []resources.Resource{resource},
			ResourceFiles: map[string][]projectarchive.File{
				resource.ID: files,
			},
			Actions: map[string]string{resource.ID: "bundle"},
		},
	); err == nil {
		t.Fatal("preserving permissions bypassed redistribution denial")
	}
}

func TestPermissionCollisionsAreExplicitAndDigestIncludesEffectiveMode(t *testing.T) {
	resource, files := publicationFixture()
	files[0].SourcePermissions = permissionPointer(0755)
	planner, err := NewPlanner(PlanOptions{Mode: "default", PreservePermissions: true,
		Resources: []resources.Resource{resource}, ResourceFiles: map[string][]projectarchive.File{resource.ID: files},
		Actions: map[string]string{resource.ID: "bundle"}})
	if err != nil {
		t.Fatal(err)
	}
	source := files[0]
	source.SourcePermissions = permissionPointer(0644)
	if _, err := planner.Build([]projectarchive.File{source}, nil); err == nil {
		t.Fatal("identical bytes with conflicting permissions were silently merged")
	}
	secondResource := resource
	secondResource.ID = "other-font"
	otherFiles := append([]projectarchive.File(nil), files...)
	otherFiles[0].SourcePermissions = permissionPointer(0640)
	for _, preserve := range []bool{false, true} {
		_, err := NewPlanner(PlanOptions{Mode: "default", PreservePermissions: preserve,
			Resources:     []resources.Resource{resource, secondResource},
			ResourceFiles: map[string][]projectarchive.File{resource.ID: files, secondResource.ID: otherFiles},
			Actions:       map[string]string{resource.ID: "bundle", secondResource.ID: "bundle"}})
		if (err != nil) != preserve {
			t.Fatalf("resource permission collision policy preserve=%t: %v", preserve, err)
		}
	}
	first, err := MembersDigest(files, map[string]os.FileMode{files[0].Path: 0755, files[1].Path: 0644})
	if err != nil {
		t.Fatal(err)
	}
	second, err := MembersDigest(files, nil)
	if err != nil || first == second {
		t.Fatalf("digest ignored archive permissions: %s %s %v", first, second, err)
	}
}

func TestZIPOnlyAcceptsOrdinaryPermissionBitsAndWritesZeroPrecisely(t *testing.T) {
	frozen := capture(t, map[string]string{"main.tex": "source"})
	for _, mode := range []os.FileMode{0, 0640, 0755, 0777} {
		var data bytes.Buffer
		if err := writeZIP(
			context.Background(),
			&data,
			frozen.Files,
			map[string]os.FileMode{"main.tex": mode},
		); err != nil {
			t.Fatal(err)
		}
		archive, err := zip.NewReader(bytes.NewReader(data.Bytes()), int64(data.Len()))
		if err != nil || archive.File[0].Mode() != mode || !archive.File[0].Mode().IsRegular() {
			t.Fatalf("wrong ZIP mode: %v %v", archive, err)
		}
	}
	for _, mode := range []os.FileMode{os.ModeSetuid | 0755, os.ModeSetgid, os.ModeSticky, os.ModeSymlink, os.ModeNamedPipe, os.ModeDir} {
		if err := writeZIP(
			context.Background(),
			&bytes.Buffer{},
			frozen.Files,
			map[string]os.FileMode{"main.tex": mode},
		); err == nil {
			t.Fatalf("accepted unsafe ZIP mode %v", mode)
		}
	}
	if err := writeZIP(
		context.Background(),
		&bytes.Buffer{},
		frozen.Files,
		map[string]os.FileMode{"missing": 0755},
	); err == nil {
		t.Fatal("accepted permissions for a member absent from the archive")
	}
}

func TestZIPAndMembersDigestRejectTheSameInvalidPermissionMaps(t *testing.T) {
	frozen := capture(t, map[string]string{"main.tex": "source", "helper.sh": "helper"})
	for _, permissions := range []map[string]os.FileMode{
		{},
		{"main.tex": 0644},
		{"main.tex": 0644, "helper.sh": 0755, "extra": 0600},
		{"main.tex": 0644, "unselected": 0755},
		{"main.tex": 0644, "helper.sh": os.ModeSetuid | 0755},
	} {
		if err := writeZIP(context.Background(), &bytes.Buffer{}, frozen.Files, permissions); err == nil {
			t.Fatalf("ZIP accepted invalid permission map: %+v", permissions)
		}
		if _, err := MembersDigest(frozen.Files, permissions); err == nil {
			t.Fatalf("digest accepted invalid permission map: %+v", permissions)
		}
	}
}
