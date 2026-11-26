package resources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	model "github.com/billstark001/latexmk/packages/shared/resources"
)

func TestManagerPinnedIndexResolutionDoesNotReserveWritableCache(t *testing.T) {
	for _, kind := range []string{"full disk cache", "fully reserved cache"} {
		t.Run(kind, func(t *testing.T) {
			m := testManager(t)
			baseline := model.Baseline{Repository: "https://snapshot.example/2026/09/09/tlnet", TexliveYear: 2026,
				Image: m.cfg.RunnerImage, Packages: []model.Package{{Name: "float", Revision: 39}}}
			data := []byte(packageFixture("algorithms", 42, "float") + "\n" + packageFixture("float", 39))
			sum := sha256.Sum256(data)
			index := &model.PinnedPackageIndex{Version: 1, Path: filepath.Join(t.TempDir(), "remote.tlpdb"),
				SHA256: hex.EncodeToString(sum[:]), Repository: baseline.Repository, TexliveYear: baseline.TexliveYear}
			if err := os.WriteFile(index.Path, data, 0600); err != nil {
				t.Fatal(err)
			}
			m.cfg.ResourcePackageIndex = index
			m.cfg.ResourceCacheBytes = 16
			if kind == "full disk cache" {
				if err := os.WriteFile(
					filepath.Join(m.dir, "metadata", "occupied"),
					make([]byte, 16),
					0600,
				); err != nil {
					t.Fatal(err)
				}
			} else {
				m.reserved = 16
			}
			before := m.reserved
			packages, err := m.resolvePackages(context.Background(), baseline, []string{"algorithms"})
			if err != nil || len(packages) != 1 || packages[0].Name != "algorithms" || packages[0].Revision != 42 {
				t.Fatalf("valid pinned index depended on writable cache capacity: %+v %v", packages, err)
			}
			if m.reserved != before {
				t.Fatalf("read-only index reserved disk bytes: before=%d after=%d", before, m.reserved)
			}
			entries, err := os.ReadDir(filepath.Join(m.dir, "metadata"))
			if err != nil || (kind == "full disk cache" && len(entries) != 1) ||
				(kind == "fully reserved cache" && len(entries) != 0) {
				t.Fatalf("read-only index wrote cache metadata: %+v %v", entries, err)
			}
			wrongBaseline := baseline
			wrongBaseline.Repository = "https://different.example/2026/09/09/tlnet"
			_, err = m.resolvePackages(context.Background(), wrongBaseline, []string{"algorithms"})
			var failure *model.AcquisitionError
			if !errors.As(err, &failure) || failure.Code != "resource_index_invalid" || failure.Reason != "binding" {
				t.Fatalf("cache bypass discarded snapshot binding: %+v %v", failure, err)
			}
			// Pinned reads continue to use preparation admission rather than
			// allowing unbounded concurrent index parsing.
			for range cap(m.prepares) {
				m.prepares <- struct{}{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if _, err := m.resolvePackages(
				ctx,
				baseline,
				[]string{"algorithms"},
			); !errors.Is(
				err,
				context.DeadlineExceeded,
			) {
				t.Fatalf("pinned index bypassed preparation admission: %v", err)
			}
			for range cap(m.prepares) {
				<-m.prepares
			}
		})
	}
}
