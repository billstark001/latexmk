package resources

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	shared "github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

func lockedPlan(t *testing.T, version string) shared.LockedPlan {
	t.Helper()
	baselineDigest, err := shared.Hash(version)
	if err != nil {
		t.Fatal(err)
	}
	env := shared.Environment{
		Baseline: shared.Baseline{ID: version, Digest: baselineDigest, Engines: []string{"xelatex"}},
	}
	env.Digest, err = shared.EnvironmentDigest(env)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := shared.Hash(shared.ResolveRequest{Baseline: version})
	if err != nil {
		t.Fatal(err)
	}
	return shared.LockedPlan{SpecDigest: spec, Environment: env}
}

func TestSavePlanDoesNotReplayOtherTargetsFromStaleReaders(t *testing.T) {
	root := t.TempDir()
	a0, b0 := lockedPlan(t, "a-v0"), lockedPlan(t, "b-v0")
	if err := SavePlan(root, "", "target:a", a0); err != nil {
		t.Fatal(err)
	}
	if err := SavePlan(root, "", "target:b", b0); err != nil {
		t.Fatal(err)
	}
	// Both resolvers read before either goes online. Their opposite-target views
	// remain deliberately stale when the independent resolutions are published.
	left, err := LoadLock(root, "")
	if err != nil {
		t.Fatal(err)
	}
	right, err := LoadLock(root, "")
	if err != nil {
		t.Fatal(err)
	}
	a1, b1 := lockedPlan(t, "a-v1"), lockedPlan(t, "b-v1")
	left.Plans["target:a"] = a1
	right.Plans["target:b"] = b1
	if err := SavePlan(root, "", "target:a", left.Plans["target:a"]); err != nil {
		t.Fatal(err)
	}
	if err := SavePlan(root, "", "target:b", right.Plans["target:b"]); err != nil {
		t.Fatal(err)
	}
	final, err := LoadLock(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if final.Plans["target:a"].Environment.Digest != a1.Environment.Digest ||
		final.Plans["target:b"].Environment.Digest != b1.Environment.Digest {
		t.Fatalf("stale reader rolled back another target: %#v", final.Plans)
	}
}

func TestSavePlanFailuresPreserveExistingLock(t *testing.T) {
	root := t.TempDir()
	original := lockedPlan(t, "slim-v1")
	if err := SavePlan(root, "", "target:paper", original); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(root, DefaultLockFile)
	before, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		after, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatal("failed plan publication changed existing lock bytes")
		}
	}
	invalid := lockedPlan(t, "slim-v2")
	invalid.Environment.Digest = "bad"
	if err := SavePlan(root, "", "target:paper", invalid); err == nil {
		t.Fatal("invalid plan accepted")
	}
	check()
	fs, err := safefs.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fs.Close() }()
	guard, err := fs.TryLock(DefaultLockFile + ".guard")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = guard.Close() }()
	if err := SavePlan(root, "", "target:paper", lockedPlan(t, "slim-v2")); !errors.Is(err, safefs.ErrLocked) {
		t.Fatalf("busy guard did not report retryable locking error: %v", err)
	}
	check()
}
