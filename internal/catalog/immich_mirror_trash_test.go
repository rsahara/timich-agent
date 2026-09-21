package catalog

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestImmichMirrorTrashTransitions(t *testing.T) {
	for _, visibility := range []string{"timeline", "hidden", "archive"} {
		t.Run(visibility, func(t *testing.T) {
			s := visibilityTestService(t)
			ctx := context.Background()
			assets := []map[string]any{visibilityTestAsset("changing", "timeline", 1), visibilityTestAsset("stable", "timeline", 0)}
			var requests []map[string]any
			s.client = visibilityTestClient(t, &assets, &requests)
			full, err := s.SyncMirror(ctx, MirrorSyncModeFull)
			if err != nil {
				t.Fatal(err)
			}
			var canonicalBefore string
			if err := s.catalog.db.QueryRow(`SELECT canonical_asset_id FROM catalog_assets WHERE upstream_asset_id = 'changing'`).Scan(&canonicalBefore); err != nil {
				t.Fatal(err)
			}
			updated := full.SyncedThrough.Add(time.Second).Format(time.RFC3339Nano)
			assets[0]["visibility"], assets[0]["isTrashed"], assets[0]["updatedAt"] = visibility, true, updated
			// Deleted rows still paginate; unseen trash must not be inserted.
			for i := 0; i < maxPageSize; i++ {
				asset := visibilityTestAsset(fmt.Sprintf("unseen-trash-%d", i), visibility, 2)
				asset["isTrashed"], asset["updatedAt"] = true, updated
				asset["fileCreatedAt"] = time.Date(2026, 8, 1, 0, i, 0, 0, time.UTC).Format(time.RFC3339Nano)
				assets = append(assets, asset)
			}
			requests = nil
			result, err := s.SyncMirror(ctx, MirrorSyncModeIncremental)
			if err != nil {
				t.Fatal(err)
			}
			requireVisibilityGallery(t, s, "stable")
			if result.ActiveCount != 1 || result.OutOfScopeCount != 1 {
				t.Fatalf("unexpected trash result: %+v", result)
			}
			var count int
			if err := s.catalog.db.QueryRow(`SELECT COUNT(*) FROM catalog_assets`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("catalog count=%d error=%v; unseen trash must not be inserted", count, err)
			}
			for _, request := range requests {
				if request["withDeleted"] != true || request["updatedAfter"] != full.SyncedThrough.Format(time.RFC3339Nano) || request["updatedBefore"] != result.SyncedThrough.Format(time.RFC3339Nano) {
					t.Fatalf("trash query escaped its window: %#v", request)
				}
			}
			if _, err := s.SyncMirror(ctx, MirrorSyncModeIncremental); err != nil {
				t.Fatal(err)
			}
			requireVisibilityGallery(t, s, "stable")
			assets[0]["visibility"], assets[0]["isTrashed"] = "timeline", false
			assets[0]["updatedAt"] = result.SyncedThrough.Add(time.Second).Format(time.RFC3339Nano)
			if _, err := s.SyncMirror(ctx, MirrorSyncModeIncremental); err != nil {
				t.Fatal(err)
			}
			requireVisibilityGallery(t, s, "changing", "stable")
			var canonicalAfter string
			if err := s.catalog.db.QueryRow(`SELECT canonical_asset_id FROM catalog_assets WHERE upstream_asset_id = 'changing'`).Scan(&canonicalAfter); err != nil || canonicalBefore != canonicalAfter {
				t.Fatalf("restore changed canonical identity: %q -> %q, %v", canonicalBefore, canonicalAfter, err)
			}
		})
	}
}

func TestImmichMirrorFullLimitDoesNotRequestTrash(t *testing.T) {
	s := visibilityTestService(t)
	assets := []map[string]any{visibilityTestAsset("trashed", "timeline", 2), visibilityTestAsset("visible", "timeline", 1)}
	assets[0]["isTrashed"] = true
	var requests []map[string]any
	s.client = visibilityTestClient(t, &assets, &requests)
	datasource, err := s.mirrorDatasource("")
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.fetchImmichMirrorAssets(context.Background(), datasource, immichMirrorFetchOptions{LatestAssetLimit: 1})
	if err != nil || len(result) != 1 || result[0].UpstreamAssetID != "visible" {
		t.Fatalf("limited full result=%+v error=%v", result, err)
	}
	if len(requests) != 1 || requests[0]["withDeleted"] != nil {
		t.Fatalf("full sync fetched retained trash: %#v", requests)
	}
}
