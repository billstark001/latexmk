package resources

// PackageArchive describes transport availability for a resolved package. It is
// deliberately separate from Environment so cache state and download policy do
// not change existing environment identities or lock files.
type PackageArchive struct {
	Package  Package `json:"package"`
	Present  bool    `json:"present"`
	MaxBytes int64   `json:"maxBytes"`
}

type PackageArchives struct {
	EnvironmentDigest string           `json:"environmentDigest"`
	Archives          []PackageArchive `json:"archives"`
	Sources           *SourcePolicy    `json:"sources,omitempty"`
}
