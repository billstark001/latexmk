package resources

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/billstark001/latexmk/packages/server/internal/platform/process"
	sharedresources "github.com/billstark001/latexmk/packages/shared/resources"
)

func packageFixture(name string, revision int, dependencies ...string) string {
	text := fmt.Sprintf(
		"name %s\ncategory Package\nrevision %d\nrelocated 1\ncontainerchecksum %s\n",
		name,
		revision,
		strings.Repeat("a", 128),
	)
	for _, dependency := range dependencies {
		text += "depend " + dependency + "\n"
	}
	return text + "runfiles size=1\n RELOC/tex/latex/" + name + "/" + name + ".sty\n"
}

func TestTLPDBParsesRemotePreambleCRLFAndDependencies(t *testing.T) {
	fixture := "location-url\thttps://example.test/2026/09/09/tlnet\n" + packageFixture(
		"algorithms",
		42,
		"float",
		"latex",
		"float",
	) + "\n" + packageFixture(
		"float",
		39,
	)
	packages, err := parseTLPDB(strings.NewReader(strings.ReplaceAll(fixture, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	algorithm, ok := packages["algorithms"]
	if len(packages) != 2 || !ok || algorithm.Revision != 42 || !algorithm.Relocatable ||
		!reflect.DeepEqual(algorithm.Depends, []string{"float", "latex"}) ||
		algorithm.Checksum != strings.Repeat("a", 128) {
		t.Fatalf("unexpected parsed package: %#v", packages)
	}
}

func TestTLPDBRejectsMalformedIdentityAndBinaryRelocation(t *testing.T) {
	for name, fixture := range map[string]string{
		"empty":                   "# no database\n",
		"duplicate":               packageFixture("algorithms", 42) + "\n" + packageFixture("algorithms", 42),
		"negative":                strings.ReplaceAll(packageFixture("algorithms", 42), "revision 42", "revision -1"),
		"duplicate zero revision": strings.ReplaceAll(packageFixture("algorithms", 42), "revision 42", "revision 0\nrevision 42"),
		"conflicting relocation":  strings.ReplaceAll(packageFixture("algorithms", 42), "relocated 1", "relocated 0\nrelocated 1"),
		"bad checksum":            strings.ReplaceAll(packageFixture("algorithms", 42), strings.Repeat("a", 128), "not-a-checksum"),
		"unsafe name":             packageFixture("../algorithms", 42),
		"missing newline":         packageFixture("algorithms", 42) + packageFixture("float", 39),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseTLPDB(strings.NewReader(fixture)); err == nil {
				t.Fatal("accepted malformed database")
			}
		})
	}
	packages, err := parseTLPDB(
		strings.NewReader(
			packageFixture("script-package", 42) + "binfiles arch=x86_64-linux size=1\n bin/x86_64-linux/script\n",
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	if packages["script-package"].Relocatable {
		t.Fatal("a binary section must override the relocation flag")
	}
}

func TestPackageClosurePinsTransitivePackagesAndUsesBaseline(t *testing.T) {
	remote, err := parseTLPDB(
		strings.NewReader(
			packageFixture("algorithms", 42, "float", "latex-bin") + "\n" + packageFixture("float", 39, "algorithms"),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	packages, err := resolvePackageClosure(
		[]sharedresources.Package{{Name: "latex-bin", Revision: 1}},
		[]string{"algorithms", "algorithms"},
		remote,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []sharedresources.Package{
		{Name: "algorithms", Revision: 42, Checksum: strings.Repeat("a", 128)},
		{Name: "float", Revision: 39, Checksum: strings.Repeat("a", 128)},
	}
	if !reflect.DeepEqual(packages, want) {
		t.Fatalf("closure = %#v, want %#v", packages, want)
	}
	remote["float"] = packageMetadata{Name: "float", Revision: 39, Checksum: strings.Repeat("b", 128)}
	if _, err := resolvePackageClosure(
		[]sharedresources.Package{{Name: "latex-bin"}},
		[]string{"algorithms"},
		remote,
	); err == nil ||
		!strings.Contains(err.Error(), "dependency of algorithms") {
		t.Fatalf("nonrelocatable transitive dependency accepted: %v", err)
	}
}

func TestPackageClosureRejectsUnknownArchAndUnpinnedPackages(t *testing.T) {
	for _, dependency := range []string{"missing", "binary.ARCH", "setting_available_architectures:x86_64-linux"} {
		remote, err := parseTLPDB(strings.NewReader(packageFixture("algorithms", 42, dependency)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolvePackageClosure(nil, []string{"algorithms"}, remote); err == nil {
			t.Errorf("accepted unresolved dependency %q", dependency)
		}
	}
	for _, pkg := range []packageMetadata{
		{Name: "algorithms", Revision: 42, Relocatable: true},
		{Name: "algorithms", Relocatable: true, Checksum: strings.Repeat("a", 128)},
		{Name: "collection-science", Revision: 42, Relocatable: true, Checksum: strings.Repeat("a", 128)},
	} {
		if _, err := resolvePackageClosure(
			nil,
			[]string{pkg.Name},
			map[string]packageMetadata{pkg.Name: pkg},
		); err == nil {
			t.Errorf("accepted unsupported package %#v", pkg)
		}
	}
}

func TestResolveInstalledPackagesDoesNotNeedTLMGR(t *testing.T) {
	baseline := sharedresources.Baseline{Packages: []sharedresources.Package{{Name: "algorithms", Revision: 42}}}
	packages, err := resolvePackages(context.Background(), baseline, []string{"algorithms"}, t.TempDir(), 4096)
	if err != nil || len(packages) != 0 {
		t.Fatalf("baseline-provided package was not a no-op: %#v %v", packages, err)
	}
}

func TestResolveUsesRegisteredBaselineMetadataAndCancellation(t *testing.T) {
	baseline := sharedresources.Baseline{
		Digest:      strings.Repeat("b", 64),
		Repository:  "https://snapshot.example/2026/09/09/tlnet",
		TexliveYear: 2026,
	}
	directory := t.TempDir()
	key := sha256.Sum256([]byte(baseline.Digest + "\x00" + baseline.Repository))
	if err := os.WriteFile(
		filepath.Join(directory, hex.EncodeToString(key[:])+".tlpdb"),
		[]byte(packageFixture("algorithms", 42)),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	packages, err := resolvePackages(context.Background(), baseline, []string{"algorithms"}, directory, 4096)
	if err != nil || len(packages) != 1 || packages[0].Revision != 42 {
		t.Fatalf("cached snapshot was not resolved: %#v %v", packages, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolvePackages(
		ctx,
		baseline,
		[]string{"algorithms"},
		directory,
		4096,
	); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestMetadataWaitHonorsCancellationBeforeTheCurrentFetchFinishes(t *testing.T) {
	packageMetadataSlot <- struct{}{}
	defer func() { <-packageMetadataSlot }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	baseline := sharedresources.Baseline{Repository: "https://snapshot.example/2026/09/09/tlnet", TexliveYear: 2026}
	directory := t.TempDir()
	finished := make(chan error, 1)
	go func() {
		_, err := resolvePackages(ctx, baseline, []string{"algorithms"}, directory, 4096)
		finished <- err
	}()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("metadata wait discarded cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled request remained blocked behind another metadata fetch")
	}
}

func TestTeXLiveVersionProbePreservesExecutionErrors(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := validateTeXLiveYear(ctx, 2026, t.TempDir(), ""); !errors.Is(err, context.Canceled) {
			t.Fatalf("version probe discarded cancellation: %v", err)
		}
	})
	t.Run("missing executable", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if err := validateTeXLiveYear(context.Background(), 2026, t.TempDir(), ""); !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("version probe discarded the executable lookup failure: %v", err)
		}
	})
}

func TestCompactPackageIndexPreservesIdentityAndSafetyWithoutFileInventories(t *testing.T) {
	fixture := packageFixture("algorithms", 42, "float")
	for range 1000 {
		fixture += " RELOC/tex/latex/algorithms/a-long-runtime-file-name.sty\n"
	}
	fixture += "\n" + packageFixture("unsafe", 10) + "runfiles size=1\n RELOC/../outside\n\n"
	fixture += packageFixture("binary", 11) + "binfiles arch=x86_64-linux size=1\n bin/x86_64-linux/tool\n"
	packages, err := parseTLPDB(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	index := packageIndex(packages)
	if len(index) >= len(fixture)/10 || bytes.Contains(index, []byte("a-long-runtime-file-name")) {
		t.Fatalf("index retained full file inventories: %d versus %d bytes", len(index), len(fixture))
	}
	actual, err := parseTLPDB(bytes.NewReader(index))
	if err != nil || !reflect.DeepEqual(actual, packages) {
		t.Fatalf("compaction changed identity or safety: %#v versus %#v, %v", actual, packages, err)
	}
	if actual["unsafe"].Relocatable || !actual["unsafe"].unsafeFiles || actual["binary"].Relocatable ||
		!actual["binary"].hasBinary {
		t.Fatal("compaction lost a package exclusion flag")
	}
	if !bytes.Equal(index, packageIndex(actual)) {
		t.Fatal("package index serialization is not deterministic")
	}
}

func TestArchiveCachePublishesOnlyVerifiedBytesWithinQuota(t *testing.T) {
	contents := []byte("an exact compressed archive")
	hash := sha512.Sum512(contents)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/snapshot/archive/algorithms.tar.xz" {
			t.Errorf("wrong registered archive path: %s", r.URL.Path)
		}
		_, _ = w.Write(contents)
	}))
	defer server.Close()
	pkg := sharedresources.Package{Name: "algorithms", Revision: 42, Checksum: hex.EncodeToString(hash[:])}
	for _, test := range []struct {
		name     string
		checksum string
		limit    int64
		wantErr  bool
	}{
		{"exact", pkg.Checksum, int64(len(contents)), false},
		{"changed bytes", strings.Repeat("a", 128), int64(len(contents)), true},
		{"oversized", pkg.Checksum, int64(len(contents) - 1), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected := pkg
			selected.Checksum = test.checksum
			directory := t.TempDir()
			name, size, err := cachedPackageArchive(
				context.Background(),
				directory,
				server.URL+"/snapshot",
				selected,
				test.limit,
				server.Client(),
			)
			if (err != nil) != test.wantErr {
				t.Fatalf("download returned size %d, error %v", size, err)
			}
			entries, readErr := os.ReadDir(directory)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if test.wantErr {
				if len(entries) != 0 {
					t.Fatalf("failed download left partial cache bytes: %#v", entries)
				}
				return
			}
			data, err := os.ReadFile(name)
			if err != nil || !bytes.Equal(data, contents) || len(entries) != 1 {
				t.Fatalf("successful archive differs: %q %v", data, err)
			}
		})
	}
}

func TestArchiveCacheSingleFlightRepairAndLeaseCancellation(t *testing.T) {
	contents := []byte("one immutable archive shared by several resource sets")
	hash := sha512.Sum512(contents)
	pkg := sharedresources.Package{Name: "algorithms", Revision: 42, Checksum: hex.EncodeToString(hash[:])}
	var hits atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			close(entered)
			<-release
		}
		_, _ = w.Write(contents)
	}))
	defer server.Close()
	directory := t.TempDir()
	var group sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		group.Go(func() {
			name, size, err := cachedPackageArchive(
				context.Background(),
				directory,
				server.URL,
				pkg,
				4096,
				server.Client(),
			)
			if err == nil &&
				(name != filepath.Join(directory, pkg.Checksum+".tar.xz") || size != int64(len(contents))) {
				err = fmt.Errorf("wrong cache identity: %q %d", name, size)
			}
			results <- err
		})
	}
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := cachedPackageArchive(
		ctx,
		directory,
		server.URL,
		pkg,
		4096,
		server.Client(),
	); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Fatalf("waiting download ignored cancellation: %v", err)
	}
	close(release)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Error(err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("concurrent environments fetched the same archive %d times", hits.Load())
	}
	name := filepath.Join(directory, pkg.Checksum+".tar.xz")
	if err := os.Chmod(name, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cachedPackageArchive(
		context.Background(),
		directory,
		server.URL,
		pkg,
		4096,
		server.Client(),
	); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(data, contents) || hits.Load() != 2 {
		t.Fatalf("bad cache was not refetched and atomically repaired: %q %d %v", data, hits.Load(), err)
	}
	if _, _, err := cachedPackageArchive(
		context.Background(),
		directory,
		server.URL,
		pkg,
		1,
		server.Client(),
	); err == nil {
		t.Fatal("cache hit bypassed the environment byte budget")
	}
	data, err = os.ReadFile(name)
	if err != nil || !bytes.Equal(data, contents) || hits.Load() != 2 {
		t.Fatal("quota rejection damaged an existing shared archive")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != pkg.Checksum+".tar.xz" {
		t.Fatalf("archive publication leaked staging files: %#v %v", entries, err)
	}
}

type packageTarMember struct {
	name, body, link string
	kind             byte
}

func packageTar(t *testing.T, members []packageTarMember) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	for _, member := range members {
		kind := member.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{
			Name:     member.name,
			Typeflag: kind,
			Size:     int64(len(member.body)),
			Linkname: member.link,
			Mode:     0600,
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, member.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestPackageTarRejectsTraversalLinksAndIdentityChanges(t *testing.T) {
	pkg := sharedresources.Package{Name: "algorithms", Revision: 42}
	metadata := packageTarMember{name: "tlpkg/tlpobj/algorithms.tlpobj", body: packageFixture("algorithms", 42)}
	valid := []packageTarMember{metadata, {name: "tex/latex/algorithms/algorithm.sty", body: "safe macro"}}
	if err := inspectPackageTar(pkg, tar.NewReader(bytes.NewReader(packageTar(t, valid))), 4096, 10); err != nil {
		t.Fatal(err)
	}
	for name, members := range map[string][]packageTarMember{
		"traversal": {metadata, {name: "../escaped.sty", body: "bad"}},
		"absolute":  {metadata, {name: "/escaped.sty", body: "bad"}},
		"symlink":   {metadata, {name: "escaped.sty", kind: tar.TypeSymlink, link: "/etc/passwd"}},
		"duplicate": {metadata, metadata},
		"revision":  {{name: metadata.name, body: packageFixture("algorithms", 43)}},
		"binary":    {{name: metadata.name, body: packageFixture("algorithms", 42) + "binfiles arch=x86_64-linux size=1\n bin/algorithm\n"}},
		"no object": {{name: "tex/latex/algorithms/algorithm.sty", body: "macro"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := inspectPackageTar(
				pkg,
				tar.NewReader(bytes.NewReader(packageTar(t, members))),
				4096,
				10,
			); err == nil {
				t.Fatal("accepted invalid package tar")
			}
		})
	}
	if err := inspectPackageTar(pkg, tar.NewReader(bytes.NewReader(packageTar(t, valid))), 10, 10); err == nil {
		t.Fatal("accepted oversized package tar")
	}
	if err := inspectPackageTar(pkg, tar.NewReader(bytes.NewReader(packageTar(t, valid))), 4096, 1); err == nil {
		t.Fatal("accepted excessive package entries")
	}
}

func TestCompressedPackageValidationStreamsAndStopsOnConsumerErrors(t *testing.T) {
	xz, err := exec.LookPath("xz")
	if err != nil {
		t.Skip("xz is not installed")
	}
	work := t.TempDir()
	compress := func(name string, members []packageTarMember) []byte {
		t.Helper()
		raw := packageTar(t, members)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		run := process.Run(
			ctx,
			process.Spec{
				Name:           xz,
				Args:           []string{"--compress", "--stdout", "-0"},
				Stdin:          bytes.NewReader(raw),
				Env:            packageEnvironment(work, ""),
				MaxOutputBytes: 4 << 20,
			},
		)
		if run.Err != nil || run.StdoutTruncated {
			t.Fatalf("compress %s fixture: %v %s", name, run.Err, run.Stderr)
		}
		return run.Stdout
	}
	pkg := sharedresources.Package{Name: "algorithms", Revision: 42}
	metadata := packageTarMember{name: "tlpkg/tlpobj/algorithms.tlpobj", body: packageFixture("algorithms", 42)}
	valid := compress(
		"valid",
		[]packageTarMember{metadata, {name: "tex/latex/algorithms/algorithm.sty", body: "macro"}},
	)
	invalid := compress(
		"bad-link",
		[]packageTarMember{
			{name: "unsafe", kind: tar.TypeSymlink, link: "../outside"},
			{name: "unread.bin", body: strings.Repeat("a", 2<<20)},
		},
	)
	for _, test := range []struct {
		name    string
		data    []byte
		wantErr bool
	}{
		{"valid", valid, false},
		{"bad-consumer", invalid, true},
		{"bad-compressor-trailer", valid[:len(valid)-5], true},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := filepath.Join(work, test.name+".tar.xz")
			if err := os.WriteFile(archive, test.data, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stats, err := validatePackageArchive(ctx, pkg, archive, work, 3<<20, 10)
			if (err != nil) != test.wantErr {
				t.Fatalf("stats %#v, error %v", stats, err)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("consumer error left the decompressor blocked: %v", err)
			}
			if !test.wantErr && (stats.Files != 2 || stats.Bytes != int64(len(metadata.body)+len("macro"))) {
				t.Fatalf("wrong expanded quota accounting: %#v", stats)
			}
		})
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tar") {
			t.Fatal("validation retained an uncompressed tar on disk")
		}
	}
}

func TestPackageTreeMaterializesOnlyContainedRegularAliases(t *testing.T) {
	for _, test := range []struct {
		name, target string
		wantErr      bool
	}{
		{"relative", "actual.map", false},
		{"escape", "../outside.map", true},
		{"directory", ".", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "tree")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "actual.map"), []byte("font-map"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(parent, "outside.map"), []byte("private"), 0600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "pdftex.map")
			if err := os.Symlink(test.target, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			err := regularizePackageTree(root, 100, 10)
			if (err != nil) != test.wantErr {
				t.Fatalf("link conversion error: %v", err)
			}
			if !test.wantErr {
				info, err := os.Lstat(link)
				if err != nil || !info.Mode().IsRegular() {
					t.Fatalf("alias was not made a regular file: %v", err)
				}
				if err := checkPackageTree(root, 15, 10, false); err == nil {
					t.Fatal("materialized alias did not count against bytes")
				}
			}
		})
	}
}

func TestPackageEnvironmentDoesNotInheritSecretsOrTeXOverrides(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "private")
	t.Setenv("TEXINPUTS", "outside")
	environment := strings.Join(packageEnvironment("/private/home", "/private/tree"), "\n")
	if strings.Contains(environment, "AWS_SECRET_ACCESS_KEY") || strings.Contains(environment, "private\n") ||
		strings.Contains(environment, "TEXINPUTS") {
		t.Fatalf("package preparation inherited process environment: %s", environment)
	}
	for _, expected := range []string{"HOME=/private/home", "TEXMFHOME=/private/tree", "TEXMFVAR=/private/tree", "TEXMFCONFIG=/private/tree"} {
		if !strings.Contains(environment, expected) {
			t.Errorf("missing isolated setting %s", expected)
		}
	}
}
