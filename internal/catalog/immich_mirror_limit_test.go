package catalog

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestImmichMirrorLimitedFullCountsUniqueAssetsDuringUpdate(t *testing.T) {
	for _, sameCapture := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_capture_%t", sameCapture), func(t *testing.T) {
			const limit = maxPageSize + 1
			s, err := NewServiceWithOptions([]config.DatasourceConfig{{
				SourceKey: visibilityTestSource, Kind: config.DatasourceKindImmichIndexed,
				URL: "http://immich.test", AccessToken: "test-key",
				Indexing: &config.DatasourceIndexingConfig{LatestAssetLimit: limit},
			}}, ServiceOptions{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			f := &mirrorWindowFixture{now: now}
			for i := 0; i < limit+1; i++ {
				captured := now.Add(-time.Duration(i) * time.Second)
				if sameCapture {
					captured = now
				}
				// ID order matches capture order, including the tied-time case.
				f.assets = append(f.assets, mirrorWindowAsset(fmt.Sprintf("asset-%04d", limit+1-i), captured, now.Add(-time.Hour)))
			}
			s.client = &http.Client{Transport: f}
			if initial, err := s.SyncMirror(ctx, MirrorSyncModeFull); err != nil || initial.ActiveCount != limit {
				t.Fatalf("initial full: %+v, %v", initial, err)
			}
			lastID := f.assets[limit-1].ID
			var originalCanonicalID string
			if err := s.catalog.db.QueryRow(`SELECT canonical_asset_id FROM catalog_assets WHERE upstream_asset_id = ?`, lastID).Scan(&originalCanonicalID); err != nil {
				t.Fatal(err)
			}
			f.now = now.Add(2 * time.Second)
			f.requests, f.pings = nil, 0
			f.afterPage = func(_ map[string]any) {
				if len(f.requests) == 1 {
					// The overlapping 200th asset is returned again with newer data.
					f.assets[maxPageSize-1].UpdatedAt = &flexibleTime{Time: f.now.Add(time.Second)}
					f.assets[maxPageSize-1].OriginalFileName = "updated-boundary.jpg"
				}
			}
			reconciled, err := s.SyncMirror(ctx, MirrorSyncModeFull)
			if err != nil {
				t.Fatal(err)
			}
			if reconciled.FetchedCount != limit || reconciled.ActiveCount != limit || reconciled.OutOfScopeCount != 0 {
				t.Errorf("reconciliation must retain %d unique assets: %+v", limit, reconciled)
			}
			wantRequests := 2
			if sameCapture {
				wantRequests = 3
			}
			if len(f.requests) != wantRequests || f.pings != 1 {
				t.Errorf("unexpected request cost: metadata=%d (want %d), pings=%d", len(f.requests), wantRequests, f.pings)
			}
			var filename string
			if err := s.catalog.db.QueryRow(`SELECT filename FROM catalog_assets WHERE upstream_asset_id = ?`, f.assets[maxPageSize-1].ID).Scan(&filename); err != nil || filename != "updated-boundary.jpg" {
				t.Errorf("newer boundary metadata was lost: %q, %v", filename, err)
			}
			// The actual 201st asset has no new update. A delta cannot recover it
			// if a duplicate made the full reconciliation mark it out of scope.
			f.afterPage = nil
			f.now = f.now.Add(2 * time.Second)
			if _, err := s.SyncMirror(ctx, MirrorSyncModeIncremental); err != nil {
				t.Fatal(err)
			}
			var visibility, canonicalID string
			if err := s.catalog.db.QueryRow(`SELECT visibility_status, canonical_asset_id FROM catalog_assets WHERE upstream_asset_id = ?`, lastID).Scan(&visibility, &canonicalID); err != nil || visibility != MirrorVisibilityActive || canonicalID != originalCanonicalID {
				t.Errorf("last unique asset lost: visibility=%q identity=%q, %v", visibility, canonicalID, err)
			}
			page, err := s.CatalogPage(0, maxPageSize)
			if err != nil || page.Total != limit {
				t.Errorf("gallery has %d assets, want %d: %v", page.Total, limit, err)
			}
			var outside int
			if err := s.catalog.db.QueryRow(`SELECT COUNT(*) FROM catalog_assets WHERE upstream_asset_id = ?`, f.assets[limit].ID).Scan(&outside); err != nil || outside != 0 {
				t.Errorf("imported an asset beyond the unique limit: count=%d, %v", outside, err)
			}
		})
	}
}
