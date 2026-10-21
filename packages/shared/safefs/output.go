package safefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidateNamePart checks text inserted into a filename. Separators and
// nonportable characters are rejected, never replaced. Empty text, dots and
// spaces are allowed here; complete components are checked by ValidateOutputPath.
func ValidateNamePart(value string) error {
	if !utf8.ValidString(value) {
		return errors.New("filename text must be valid UTF-8")
	}
	for _, c := range value {
		if unicode.IsControl(c) || strings.ContainsRune(`<>:"/\|?*`, c) {
			return fmt.Errorf("filename text contains forbidden character %q", c)
		}
	}
	return nil
}

// ValidateOutputPath applies a portable filename policy to a native relative
// or absolute path. It does not resolve paths or enforce root confinement;
// Clean and Root operations continue to enforce those separate requirements.
func ValidateOutputPath(name string) error {
	if name == "" || len(name) > 4096 || !utf8.ValidString(name) {
		return errors.New("output path must be nonempty valid UTF-8, at most 4096 bytes")
	}
	local := strings.TrimPrefix(name, filepath.VolumeName(name))
	for _, component := range strings.Split(filepath.ToSlash(local), "/") {
		if component == "" || component == "." || component == ".." {
			continue
		}
		if err := ValidateNamePart(component); err != nil {
			return fmt.Errorf("invalid output component %q: %w", component, err)
		}
		if len(component) > 255 {
			return errors.New("output filename component exceeds 255 UTF-8 bytes")
		}
		if strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return fmt.Errorf("output component %q ends with a dot or space", component)
		}
		base, _, _ := strings.Cut(component, ".")
		base = strings.ToUpper(strings.TrimRight(base, " ."))
		reserved := base == "CON" || base == "PRN" || base == "AUX" || base == "NUL"
		if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
			suffix := base[3:]
			reserved = reserved || (len(suffix) == 1 && suffix[0] >= '1' && suffix[0] <= '9') ||
				suffix == "¹" || suffix == "²" || suffix == "³"
		}
		if reserved {
			return fmt.Errorf("output component %q is a reserved device name", component)
		}
	}
	return nil
}

// CheckDestination validates a prospective regular-file destination without
// creating directories. Publication must still recheck it at the actual write.
func (r *Root) CheckDestination(name string) error {
	local, err := r.check(name, true)
	if err != nil {
		return err
	}
	info, err := r.Root.Lstat(local)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("destination is not a regular file: %s", name)
	}
	return nil
}
