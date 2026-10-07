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

func (c *Client) ResolveMissingFiles(
	needs []string,
	selected []projectarchive.File,
	recovery *MissingFileRecovery,
) ([]string, error) {
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
			currentBytes += file.Size
		}
	}
	var additional []string
	var newBytes int64
	for _, file := range requested {
		if seen[file.Path] {
			continue
		}
		seen[file.Path] = true
		additional = append(additional, file.Path)
		newBytes += file.Size
	}
	if len(additional) == 0 {
		return nil, errors.New("server requested no new allowed files")
	}
	if len(recovery.Additional)+len(additional) > maxNeedsFiles ||
		max(recovery.addedBytes, currentBytes)+newBytes > maxNeedsFileBytes {
		return nil, fmt.Errorf("additions exceed %d files or %d bytes", maxNeedsFiles, maxNeedsFileBytes)
	}
	recovery.Additional = append(recovery.Additional, additional...)
	recovery.addedBytes += newBytes
	recovery.Rounds++
	return additional, nil
}

// ValidateCaptured enforces byte limits against the bytes actually frozen,
// rather than trusting earlier stat sizes that can change during an editor save.
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
	return nil
}
