package resources

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/billstark001/latexmk/packages/shared/jsonutil"
	shared "github.com/billstark001/latexmk/packages/shared/resources"
	"github.com/billstark001/latexmk/packages/shared/safefs"
)

const DefaultLockFile = ".latexmk-resources.lock.json"
const maxLockBytes = 16 << 20

func LockName(name string) string {
	if name == "" {
		return DefaultLockFile
	}
	return name
}

func LoadLock(root, name string) (shared.Lock, error) {
	lock := shared.Lock{Version: shared.LockVersion, Plans: make(map[string]shared.LockedPlan)}
	fs, err := safefs.Open(root)
	if err != nil {
		return lock, err
	}
	defer func() { _ = fs.Close() }()
	f, err := fs.OpenRegular(LockName(name))
	if errors.Is(err, os.ErrNotExist) {
		return lock, nil
	}
	if err != nil {
		return lock, err
	}
	defer func() { _ = f.Close() }()
	if err := jsonutil.DecodeStrict(f, maxLockBytes, &lock); err != nil {
		return lock, fmt.Errorf("read resource lock: %w", err)
	}
	if lock.Version != shared.LockVersion || lock.Plans == nil {
		return lock, errors.New("unsupported resource lock format")
	}
	for key, plan := range lock.Plans {
		if err := validateLockedPlan(key, plan); err != nil {
			return lock, err
		}
	}

	return lock, nil
}

// SavePlan replaces exactly one plan after re-reading the lock under its
// publication guard. A resolver cannot replay stale plans belonging to another target.
func SavePlan(root, name, key string, plan shared.LockedPlan) error {
	if err := validateLockedPlan(key, plan); err != nil {
		return err
	}
	fs, err := safefs.Open(root)
	if err != nil {
		return err
	}
	defer func() { _ = fs.Close() }()
	name = LockName(name)
	if clean, err := safefs.Clean(name); err != nil || clean != name {
		return errors.New("resource lock path must be project-relative")
	}
	if err := fs.MakeDirs(filepath.ToSlash(filepath.Dir(name)), 0700); err != nil {
		return err
	}
	guard, err := fs.TryLock(name + ".guard")
	if errors.Is(err, safefs.ErrLocked) {
		return fmt.Errorf("resource lock is being updated; retry: %w", err)
	}
	if err != nil {
		return err
	}
	defer func() { _ = guard.Close() }()
	// Read every other plan at publication time, after network resolution has completed.
	current, err := LoadLock(root, name)
	if err != nil {
		return err
	}
	current.Plans[key] = plan
	current.Version = shared.LockVersion
	raw, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	_, err = fs.WriteAtomic(name, maxLockBytes, func(w io.Writer) error { _, err := w.Write(raw); return err })
	return err
}

func validateLockedPlan(key string, plan shared.LockedPlan) error {
	if key == "" || !shared.ValidDigest(plan.SpecDigest) {
		return errors.New("invalid locked plan")
	}
	digest, err := shared.EnvironmentDigest(plan.Environment)
	if err != nil {
		return fmt.Errorf("invalid environment in resource lock plan %q: %w", key, err)
	}
	if digest != plan.Environment.Digest {
		return fmt.Errorf("environment digest does not match resource lock plan %q", key)
	}
	return nil
}
