package client

import (
	"errors"
	"fmt"

	projectarchive "github.com/billstark001/latexmk/packages/cli/internal/archive"
	"github.com/billstark001/latexmk/packages/cli/internal/dependency"
)

// MissingFileRecovery bounds server-assisted additions across all attempts for
// one source edit. Every candidate is selected again through local upload policy.
type MissingFileRecovery struct {
	Additional []string
	Rounds     int
	addedBytes int64
}

// ResolveMissingFiles selects new allowed paths and advances recovery only on
// success. Previously admitted files count at their larger historical/current
// byte size, so growth cannot reset the budget. recovery must be non-nil.
func (c *Client) ResolveMissingFiles(
	needs []string,
	selected []projectarchive.File,
	recovery *MissingFileRecovery,
) ([]string, error) {
	if recovery == nil {
		return nil, errors.New("missing-file recovery state is required")
	}
	if recovery.addedBytes < 0 || recovery.addedBytes > maxNeedsFileBytes || len(recovery.Additional) > maxNeedsFiles {
		return nil, missingFileLimitError()
	}
	if recovery.Rounds >= maxNeedsFileRounds {
		return nil, fmt.Errorf("missing-file retry stopped after %d rounds", maxNeedsFileRounds)
	}
	candidates, _, err := c.policyManifest()
	if err != nil {
		return nil, err
	}
	requested, err := dependency.ResolveRequestedFiles(needs, candidates)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(selected)+len(recovery.Additional))
	for _, file := range selected {
		seen[file.Path] = true
	}
	already := make(map[string]bool, len(recovery.Additional))
	for _, name := range recovery.Additional {
		seen[name], already[name] = true, true
	}
	var currentBytes int64
	for _, file := range candidates {
		if already[file.Path] {
			if file.Size < 0 || file.Size > maxNeedsFileBytes-currentBytes {
				return nil, missingFileLimitError()
			}
			currentBytes += file.Size
		}
	}
	var additional []string
	var newBytes int64
	remainingBytes := maxNeedsFileBytes - max(recovery.addedBytes, currentBytes)
	for _, file := range requested {
		if seen[file.Path] {
			continue
		}
		if file.Size < 0 || file.Size > remainingBytes-newBytes {
			return nil, missingFileLimitError()
		}
		seen[file.Path] = true
		additional = append(additional, file.Path)
		newBytes += file.Size
	}
	if len(additional) == 0 {
		return nil, errors.New("server requested no new allowed files")
	}
	if len(additional) > maxNeedsFiles-len(recovery.Additional) {
		return nil, missingFileLimitError()
	}
	recovery.Additional = append(recovery.Additional, additional...)
	recovery.addedBytes = max(recovery.addedBytes, currentBytes) + newBytes
	recovery.Rounds++
	return additional, nil
}

// ValidateCaptured enforces byte limits against the bytes actually frozen,
// rather than trusting earlier stat sizes that can change during an editor save.
// Successful validation advances the historical byte watermark.
func (r *MissingFileRecovery) ValidateCaptured(files []projectarchive.File) error {
	allowed := make(map[string]bool, len(r.Additional))
	for _, name := range r.Additional {
		allowed[name] = true
	}
	var total int64
	for _, file := range files {
		if allowed[file.Path] {
			if file.Size < 0 || file.Size > maxNeedsFileBytes-total {
				return errors.New("missing-file recovery exceeds byte limit after capture")
			}
			total += file.Size
		}
	}
	r.addedBytes = max(r.addedBytes, total)
	return nil
}

func missingFileLimitError() error {
	return fmt.Errorf("additions exceed %d files or %d bytes", maxNeedsFiles, maxNeedsFileBytes)
}
