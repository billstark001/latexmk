package resources

import (
	"cmp"
	"slices"
	"strings"
)

// BaselineDigest fingerprints the complete runtime inventory, rather than a
// caller-supplied label. Set ordering and exact duplicates do not change it;
// the original inventory and its claimed Digest are left untouched.
func BaselineDigest(baseline Baseline) (string, error) {
	baseline.Digest = ""
	baseline.Engines = sortedUnique(baseline.Engines)
	if len(baseline.Engines) == 0 {
		baseline.Engines = nil
	}
	baseline.Fonts = slices.Clone(baseline.Fonts)
	slices.SortFunc(baseline.Fonts, func(a, b Font) int {
		return cmp.Or(
			strings.Compare(a.File, b.File),
			cmp.Compare(a.Index, b.Index),
			strings.Compare(a.SHA256, b.SHA256),
			strings.Compare(a.Family, b.Family),
			strings.Compare(a.Style, b.Style),
			strings.Compare(a.PostScript, b.PostScript),
		)
	})
	baseline.Fonts = slices.Compact(baseline.Fonts)
	if len(baseline.Fonts) == 0 {
		baseline.Fonts = nil
	}
	baseline.Packages = sortedPackages(baseline.Packages)
	return Hash(struct {
		Schema   int
		Baseline Baseline
	}{1, baseline})
}

func sortedPackages(packages []Package) []Package {
	packages = slices.Clone(packages)
	slices.SortFunc(packages, func(a, b Package) int {
		return cmp.Or(
			strings.Compare(a.Name, b.Name),
			cmp.Compare(a.Revision, b.Revision),
			strings.Compare(a.Checksum, b.Checksum),
		)
	})
	if len(packages) == 0 {
		return nil
	}
	return slices.Compact(packages)
}
