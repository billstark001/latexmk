package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/client"
	cliresources "github.com/billstark001/latexmk/packages/cli/internal/resources"
	"github.com/billstark001/latexmk/packages/shared/protocol"
	"github.com/billstark001/latexmk/packages/shared/resources"
)

func writePublicationFiles(t *testing.T, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPublicationBundlesOnlyExplicitResourcesOffline(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writePublicationFiles(t, map[string]string{
		".latexmk.json": `{"uploadMode":"all","token":{"file":"missing-token"},"resources":{
 "body":{"type":"files","kind":"font","path":"private/body","mount":"fonts/body","redistribution":"allow","licenseFiles":["LICENSE"]},
 "unused":{"type":"files","kind":"data","path":"private/unused","mount":"unused"}},
 "targets":{"paper":{"entry":"main.tex","engine":"xelatex","resources":["body"]}},
 "publications":{"release":{"target":"paper","verify":"none","resources":{"body":"bundle"},"output":"paper.zip"}}}`,
		"main.tex":                  `\documentclass{article}\usepackage{fontspec}\setmainfont{Body.otf}[Path=fonts/body/]\begin{document}ok\end{document}`,
		"private/body/Body.otf":     "font bytes",
		"private/body/LICENSE":      "font license",
		"private/unused/secret.dat": "private data",
	})
	code, stdout, stderr := captureCommandOutput(t, func() int {
		return run([]string{"latexmk", "pack", "--publication", "release", "--json"})
	})
	if code != 0 {
		t.Fatalf("offline resource publication: %d %s %s", code, stdout, stderr)
	}
	files := readPackZIP(t, "paper.zip")
	if len(files) != 3 || files["fonts/body/Body.otf"] != "font bytes" ||
		files["fonts/body/LICENSE"] != "font license" {
		t.Fatal(files)
	}
	for name := range files {
		if strings.HasPrefix(name, "private/") || strings.HasPrefix(name, "unused/") {
			t.Fatalf("private resource leaked into publication: %s", name)
		}
	}
	var response struct{ Data packView }
	if err := json.Unmarshal(
		[]byte(stdout),
		&response,
	); err != nil || response.Data.Verified ||
		response.Data.Verification.Status != "not-requested" {
		t.Fatalf("false verification: %s %v", stdout, err)
	}
}

func TestPublicationRejectsEntryTargetAndEngineOverrides(t *testing.T) {
	for _, override := range [][]string{{"main.tex"}, {"--target", "paper"}, {"--engine", "xelatex"}, {"-pdf"}} {
		t.Run(strings.Join(override, " "), func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			writePublicationFiles(
				t,
				map[string]string{
					"main.tex":      "source",
					".latexmk.json": `{"targets":{"paper":{"entry":"main.tex"}},"publications":{"release":{"target":"paper","verify":"none"}}}`,
				},
			)
			args := append([]string{"latexmk", "pack", "--publication", "release", "--json"}, override...)
			code, stdout, stderr := captureCommandOutput(t, func() int { return run(args) })
			if code != 2 || !strings.Contains(stdout, "--publication cannot be combined") {
				t.Fatalf("accepted override: %d %s %s", code, stdout, stderr)
			}
		})
	}
}

func TestPublicationBothBuildsUseTargetBaselineWithoutWorkResources(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var requests []protocol.UploadPlanRequest
	var resolved resources.ResolveRequest
	var environment resources.Environment
	artifacts := map[string]string{"main.pdf": "pdf", "main.bbl": "target bibliography"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/meta":
			_ = json.NewEncoder(w).Encode(protocol.Metadata{
				ProtocolVersion: protocol.Version,
				Capabilities: protocol.Capabilities{
					ResourceEnvironments: true,
					IncrementalUpload:    true,
					QueuedJobs:           true,
					AuxiliaryRetention:   true,
				},
			})
		case r.URL.Path == "/v1/environments/resolve":
			if err := json.NewDecoder(r.Body).Decode(&resolved); err != nil {
				t.Error(err)
			}
			if len(resolved.Resources) != 0 || resolved.Baseline != "arxiv-test" || len(resolved.Provided) != 1 {
				t.Errorf("publication injected work resources or lost target: %+v", resolved)
			}
			environment = resources.Environment{
				Baseline: resources.Baseline{
					ID:      "arxiv-test",
					Digest:  strings.Repeat("a", 64),
					Engines: []string{"pdflatex"},
				},
				Provided: resolved.Provided,
			}
			environment.Digest, _ = resources.EnvironmentDigest(environment)
			_ = json.NewEncoder(w).Encode(environment)
		case strings.HasSuffix(r.URL.Path, "/prepare"):
			environment.Ready = true
			_ = json.NewEncoder(w).Encode(environment)
		case r.URL.Path == "/v1/uploads/plans":
			var request protocol.UploadPlanRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			requests = append(requests, request)
			var missing []string
			for _, file := range request.Files {
				missing = append(missing, file.SHA256)
				if strings.Contains(file.Path, "private") || strings.Contains(file.Path, "Body.otf") {
					t.Errorf("omitted font leaked into target build: %s", file.Path)
				}
			}
			_ = json.NewEncoder(
				w,
			).Encode(
				protocol.UploadPlan{UploadID: fmt.Sprintf("upl_%d", len(requests)), Missing: missing},
			)
		case strings.Contains(r.URL.Path, "/blobs/"):
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/commit"):
			_ = json.NewEncoder(w).Encode(protocol.Job{ID: fmt.Sprintf("job_%d", len(requests)), Status: "succeeded"})
		case strings.HasSuffix(r.URL.Path, "/result"):
			result := protocol.CompileResult{
				ProtocolVersion:   protocol.Version,
				RequestID:         fmt.Sprintf("job_%d", len(requests)),
				Entry:             "main.tex",
				Engine:            "pdflatex",
				Success:           true,
				EnvironmentDigest: environment.Digest,
			}
			for name, contents := range artifacts {
				digest := sha256.Sum256([]byte(contents))
				result.Artifacts = append(
					result.Artifacts,
					protocol.Artifact{Path: name, Size: int64(len(contents)), SHA256: hex.EncodeToString(digest[:])},
				)
			}
			_, _ = w.Write(packArchive(t, result, artifacts))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	writePublicationFiles(t, map[string]string{
		".latexmk.json":          `{"uploadMode":"all","resources":{"extras":{"type":"texlive","packages":["algorithms"]},"private":{"type":"files","kind":"font","path":"private/fonts","mount":"fonts","redistribution":"deny"}},"targets":{"paper":{"entry":"main.tex","engine":"pdflatex","resources":["extras","private"]}},"publications":{"arxiv":{"target":"paper","mode":"arxiv","environment":"arxiv-test","resources":{"extras":"provided","private":"omit"},"output":"paper.zip"}}}`,
		"main.tex":               `\documentclass{article}\begin{document}ok\end{document}`,
		"private/fonts/Body.otf": "private font",
	})
	code, stdout, stderr := captureCommandOutput(t, func() int {
		return run([]string{"latexmk", "pack", "--publication", "arxiv", "--server", server.URL, "--json"})
	})
	if code != 0 || len(requests) != 2 {
		t.Fatalf("publication builds: %d requests=%d %s %s", code, len(requests), stdout, stderr)
	}
	for _, request := range requests {
		if request.Request.EnvironmentDigest != environment.Digest {
			t.Fatal("a build escaped the target environment")
		}
	}
	var response struct{ Data packView }
	if err := json.Unmarshal(
		[]byte(stdout),
		&response,
	); err != nil || response.Data.Verification.Scope != "target-environment" || response.Data.Verification.JobID != "job_2" ||
		!response.Data.Verified {
		t.Fatalf("missing target verification evidence: %s %v", stdout, err)
	}
	if files := readPackZIP(t, "paper.zip"); len(files) != 2 || files["main.bbl"] != "target bibliography" {
		t.Fatal(files)
	}
}

func TestPublicationExportsAuthorizedServerFontWithoutImplicitBuild(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	contents := map[string]string{"Body.otf": "registered font", "LICENSE": "font license"}
	resource := resources.Resource{ID: "body", Type: "server", Kind: "font", Ref: "body-v1", Mount: "fonts/body",
		Redistribution: "allow", LicenseFiles: []string{"LICENSE"}}
	blobs := make(map[string]string)
	for name, value := range contents {
		digest := sha256.Sum256([]byte(value))
		sha := hex.EncodeToString(digest[:])
		resource.Files = append(resource.Files, resources.File{Path: name, SHA256: sha, Size: int64(len(value))})
		blobs[sha] = value
	}
	environments := make(map[string]resources.Environment)
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestCount++
		switch {
		case request.URL.Path == "/v1/meta":
			_ = json.NewEncoder(
				w,
			).Encode(
				protocol.Metadata{Capabilities: protocol.Capabilities{ResourceEnvironments: true}},
			)
		case request.URL.Path == "/v1/environments/resolve":
			var input resources.ResolveRequest
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			environment := resources.Environment{
				Baseline: resources.Baseline{
					ID:      "default",
					Digest:  strings.Repeat("c", 64),
					Engines: []string{"xelatex"},
				},
			}
			if len(input.Resources) != 0 {
				environment.Resources = []resources.Resource{resource}
			}
			environment.Digest, _ = resources.EnvironmentDigest(environment)
			environments[environment.Digest] = environment
			_ = json.NewEncoder(w).Encode(environment)
		case strings.HasSuffix(request.URL.Path, "/prepare"):
			parts := strings.Split(request.URL.Path, "/")
			environment := environments[parts[3]]
			environment.Ready = true
			_ = json.NewEncoder(w).Encode(environment)
		case strings.Contains(request.URL.Path, "/resources/body/blobs/"):
			value, ok := blobs[filepath.Base(request.URL.Path)]
			if !ok {
				http.NotFound(w, request)
				return
			}
			_, _ = w.Write([]byte(value))
		default:
			t.Errorf("unexpected publication request (must not build): %s", request.URL.Path)
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	writePublicationFiles(t, map[string]string{
		".latexmk.json": `{"resources":{"body":{"type":"server","kind":"font","ref":"body-v1","mount":"fonts/body"}},"targets":{"paper":{"entry":"main.tex","engine":"xelatex","resources":["body"]}},"publications":{"release":{"target":"paper","verify":"none","resources":{"body":"bundle"},"output":"paper.zip"}}}`,
		"main.tex":      `\documentclass{article}\usepackage{fontspec}\setmainfont{Body.otf}[Path=fonts/body/]\begin{document}ok\end{document}`,
	})
	code, stdout, stderr := captureCommandOutput(t, func() int {
		return run(
			[]string{"latexmk", "pack", "--publication", "release", "--server", server.URL, "--explain", "--json"},
		)
	})
	if code != 0 || requestCount != 0 {
		t.Fatalf(
			"server resource preview must stay offline and pending: %d calls=%d %s %s",
			code,
			requestCount,
			stdout,
			stderr,
		)
	}
	code, stdout, stderr = captureCommandOutput(t, func() int {
		return run([]string{"latexmk", "pack", "--publication", "release", "--server", server.URL, "--json"})
	})
	if code != 0 {
		t.Fatalf("server resource export: %d %s %s", code, stdout, stderr)
	}
	files := readPackZIP(t, "paper.zip")
	if len(files) != 3 || files["fonts/body/Body.otf"] != contents["Body.otf"] ||
		files["fonts/body/LICENSE"] != contents["LICENSE"] {
		t.Fatal(files)
	}
}

func TestResourceFreePackUsesAdvertisedDefaultWithoutWritingLock(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	environment := resources.Environment{
		Baseline: resources.Baseline{ID: "registered-default", Digest: strings.Repeat("d", 64),
			Engines: []string{"xelatex"}, Fonts: []resources.Font{{File: "SystemBody.otf", Family: "System Body"}}},
	}
	environment.Digest, _ = resources.EnvironmentDigest(environment)
	var uploaded protocol.UploadPlanRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/v1/meta":
			_ = json.NewEncoder(
				w,
			).Encode(
				protocol.Metadata{Capabilities: protocol.Capabilities{ResourceEnvironments: true,
					IncrementalUpload: true, QueuedJobs: true, AuxiliaryRetention: true}},
			)
		case request.URL.Path == "/v1/environments/resolve":
			var input resources.ResolveRequest
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input.Baseline != "" || len(input.Resources) != 0 || len(input.Provided) != 0 {
				t.Errorf("resource-free verification did not respect server default: %+v", input)
			}
			_ = json.NewEncoder(w).Encode(environment)
		case strings.HasSuffix(request.URL.Path, "/prepare"):
			environment.Ready = true
			_ = json.NewEncoder(w).Encode(environment)
		case request.URL.Path == "/v1/uploads/plans":
			if err := json.NewDecoder(request.Body).Decode(&uploaded); err != nil {
				t.Error(err)
			}
			var missing []string
			for _, file := range uploaded.Files {
				missing = append(missing, file.SHA256)
				if file.Path == "SystemBody.otf" {
					t.Error("baseline font became a source upload")
				}
			}
			_ = json.NewEncoder(w).Encode(protocol.UploadPlan{UploadID: "upl_1", Missing: missing})
		case strings.Contains(request.URL.Path, "/blobs/"):
			_, _ = io.Copy(io.Discard, request.Body)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(request.URL.Path, "/commit"):
			_ = json.NewEncoder(w).Encode(protocol.Job{ID: "job_1", Status: "succeeded"})
		case strings.HasSuffix(request.URL.Path, "/result"):
			_, _ = w.Write(packArchive(t, protocol.CompileResult{ProtocolVersion: protocol.Version, RequestID: "job_1",
				Entry: "main.tex", Engine: "xelatex", Success: true, EnvironmentDigest: environment.Digest}, nil))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	writePublicationFiles(
		t,
		map[string]string{
			"main.tex": `\documentclass{article}\usepackage{fontspec}\setmainfont{SystemBody.otf}\begin{document}ok\end{document}`,
		},
	)
	code, stdout, stderr := captureCommandOutput(t, func() int {
		return run(
			[]string{
				"latexmk",
				"pack",
				"--verify",
				"--server",
				server.URL,
				"--output",
				"paper.zip",
				"--json",
				"main.tex",
			},
		)
	})
	if code != 0 || uploaded.Request.EnvironmentDigest != environment.Digest {
		t.Fatalf("resource-free exact verification: %d %s %s", code, stdout, stderr)
	}
	if _, err := os.Stat(".latexmk-resources.lock.json"); !os.IsNotExist(err) {
		t.Fatalf("legacy resource-free verification wrote a resource lock: %v", err)
	}
	var response struct{ Data packView }
	if err := json.Unmarshal(
		[]byte(stdout),
		&response,
	); err != nil || response.Data.Verification.EnvironmentID != "registered-default" ||
		response.Data.Verification.Scope != "server-environment" {
		t.Fatalf("incorrect baseline evidence: %s %v", stdout, err)
	}
}

func TestFailedServerExportsLeaveResourceCaptureUnchanged(t *testing.T) {
	sha := sha256.Sum256([]byte("resource"))
	hash := hex.EncodeToString(sha[:])
	capture := &cliresources.Captured{
		Resources: []resources.Resource{{ID: "first", Type: "server", Ref: "first-v1", Mount: "first"},
			{ID: "second", Type: "server", Ref: "second-v1", Mount: "second"}},
		MountFilesByID: make(map[string][]projectarchive.File),
	}
	environment := resources.Environment{
		Baseline: resources.Baseline{ID: "default", Digest: strings.Repeat("a", 64), Engines: []string{"xelatex"}},
	}
	for _, value := range capture.Resources {
		value.Redistribution = "allow"
		value.Files = []resources.File{{Path: "data.bin", SHA256: hash, Size: int64(len("resource"))}}
		environment.Resources = append(environment.Resources, value)
	}
	environment.Digest, _ = resources.EnvironmentDigest(environment)
	downloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/v1/environments/resolve":
			_ = json.NewEncoder(w).Encode(environment)
		case strings.HasSuffix(request.URL.Path, "/prepare"):
			environment.Ready = true
			_ = json.NewEncoder(w).Encode(environment)
		case strings.Contains(request.URL.Path, "/resources/first/"):
			downloads++
			_, _ = w.Write([]byte("resource"))
		case strings.Contains(request.URL.Path, "/resources/second/"):
			downloads++
			http.Error(w, "export unavailable", http.StatusForbidden)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	connected, err := client.New(server.URL, "", time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	meta := protocol.Metadata{Capabilities: protocol.Capabilities{ResourceEnvironments: true}}
	packing := packOptions{
		publication:     "release",
		resourceActions: map[string]string{"first": "bundle", "second": "bundle"},
	}
	exports, err := exportPublicationResources(context.Background(), connected,
		compileOptions{projectRoot: t.TempDir(), engine: "xelatex"}, packing, meta, capture)
	if err == nil || len(exports) != 0 || downloads != 2 {
		t.Fatalf("export failure was not preserved: exports=%d downloads=%d error=%v", len(exports), downloads, err)
	}
	if len(capture.MountFilesByID) != 0 || len(capture.Resources[0].Files) != 0 ||
		len(capture.Resources[1].Files) != 0 {
		t.Fatal("failed export published a partial resource capture")
	}
}
