package safefs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RemoveVerified removes a regular file only if its contents match the expected
// size and SHA-256. Missing, edited and non-regular paths are left alone. Before
// unlinking, it moves the candidate to a private sibling and verifies that exact
// object again, so an editor's atomic replacement cannot be deleted by mistake.
func (r *Root) RemoveVerified(name string, size int64, digest string, preserve ...os.FileInfo) (bool, error) {
	hash, err := hex.DecodeString(digest)
	if err != nil || len(hash) != sha256.Size || size < 0 || size == 1<<63-1 {
		return false, errors.New("invalid removal size or SHA-256")
	}
	digest = strings.ToLower(digest)
	input, err := r.OpenRegular(name)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrSymlink) || errors.Is(err, ErrNotRegular) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	info, err := input.Stat()
	if err != nil {
		return false, errors.Join(err, input.Close())
	}
	if err == nil {
		for _, protected := range preserve {
			if os.SameFile(info, protected) {
				return false, input.Close()
			}
		}
	}
	if err == nil {
		err = CopyVerified(io.Discard, input, size, digest)
	}
	closeErr := input.Close()
	if closeErr != nil {
		return false, closeErr
	}
	if err != nil {
		return false, nil
	}
	local, err := r.check(name, false)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrSymlink) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	quarantine, err := stagedSibling(local)
	if err != nil {
		return false, err
	}
	if err := r.Root.Rename(local, quarantine); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return r.removeQuarantined(local, quarantine, info, size, digest)
}

func (r *Root) removeQuarantined(
	original, quarantine string,
	expected os.FileInfo,
	size int64,
	digest string,
) (bool, error) {
	input, err := r.OpenRegular(filepath.ToSlash(quarantine))
	if err != nil {
		return false, fmt.Errorf(
			"cleanup preserved replacement at %s; manual restoration required: %w",
			quarantine,
			err,
		)
	}
	if err == nil {
		var actual os.FileInfo
		actual, err = input.Stat()
		if err == nil && !os.SameFile(actual, expected) {
			err = errors.New("file was replaced before cleanup")
		}
		if err == nil {
			err = CopyVerified(io.Discard, input, size, digest)
		}
		err = errors.Join(err, input.Close())
	}
	if err == nil {
		err = r.Root.Remove(quarantine)
		return err == nil, err
	}
	// Hard-link creation restores without overwriting a newer path. If another
	// writer already recreated it, keep both objects and report the recovery path.
	if restoreErr := r.Root.Link(quarantine, original); restoreErr != nil {
		return false, fmt.Errorf(
			"cleanup preserved changed file at %s; could not restore %s: %w",
			quarantine,
			original,
			restoreErr,
		)
	}
	return false, r.Root.Remove(quarantine)
}
