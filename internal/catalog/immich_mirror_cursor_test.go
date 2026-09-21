package catalog

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func requireMirrorCursor(t *testing.T, store *CatalogStore, source string, want time.Time) {
	t.Helper()
	got, err := store.immichMirrorUpdatedAfter(context.Background(), source)
	if err != nil || got == nil || !got.Equal(want) {
		t.Fatalf("mirror cursor = %v, %v; want %v", got, err, want)
	}
}

func TestImmichMirrorCursorUnseenExclusionsAndRestart(t *testing.T) {
	for _, visibility := range []string{"hidden", "archive"} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restart_%t", visibility, restart), func(t *testing.T) {
				dataDir := t.TempDir()
				s := visibilityTestServiceIn(t, dataDir)
				ctx := context.Background()
				assets := []map[string]any{visibilityTestAsset("stable", "timeline", 1)}
				var requests []map[string]any
				s.client = visibilityTestClient(t, &assets, &requests)
				full, err := s.SyncMirror(ctx, MirrorSyncModeFull)
				if err != nil {
					t.Fatal(err)
				}
				requireMirrorCursor(t, s.catalog, visibilityTestSource, full.SyncedThrough)
				for i := 0; i < 3; i++ {
					asset := visibilityTestAsset(fmt.Sprintf("unseen-%d", i), visibility, 2)
					asset["updatedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
					assets = append(assets, asset)
				}
				// Progress-only commits must not rebuild the published gallery or
				// invalidate semantic work. These triggers also survive a restart.
				for _, source := range []string{visibilityTestSource, canonicalSemanticCorpusSourceKey} {
					if _, err := s.catalog.db.Exec(`INSERT INTO semantic_state (source_key, model_id, vector_space_id, status, embedding_dim, updated_at) VALUES (?, 'test', 'test', 'ready', 1, ?)`, source, formatCatalogTime(full.SyncedThrough)); err != nil {
						t.Fatal(err)
					}
				}
				for _, statement := range []string{
					`CREATE TRIGGER reject_cursor_gallery_rebuild BEFORE INSERT ON catalog_gallery_timeline BEGIN SELECT RAISE(ABORT, 'unexpected gallery rebuild'); END`,
					`CREATE TRIGGER reject_cursor_generation BEFORE UPDATE OF generation ON catalog_canonical_state BEGIN SELECT RAISE(ABORT, 'unexpected canonical generation'); END`,
					`CREATE TRIGGER reject_cursor_semantics BEFORE UPDATE OF asset_generation ON semantic_state BEGIN SELECT RAISE(ABORT, 'unexpected semantic invalidation'); END`,
				} {
					if _, err := s.catalog.db.Exec(statement); err != nil {
						t.Fatal(err)
					}
				}
				requests = nil
				first, err := s.SyncMirror(ctx, MirrorSyncModeIncremental)
				if err != nil || first.FetchedCount != 3 || len(requests) != 3 {
					t.Fatalf("first delta = %+v, %v, requests=%d", first, err, len(requests))
				}
				requireMirrorCursor(t, s.catalog, visibilityTestSource, first.SyncedThrough)
				if restart {
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
					s = visibilityTestServiceIn(t, dataDir)
					s.client = visibilityTestClient(t, &assets, &requests)
				}
				requests = nil
				second, err := s.SyncMirror(ctx, MirrorSyncModeIncremental)
				if err != nil || second.Mode != MirrorSyncModeIncremental || second.FetchedCount != 0 || len(requests) != 3 {
					t.Fatalf("unchanged delta = %+v, %v, requests=%d", second, err, len(requests))
				}
				for _, request := range requests {
					if request["updatedAfter"] != first.SyncedThrough.Format(time.RFC3339Nano) || request["updatedBefore"] != second.SyncedThrough.Format(time.RFC3339Nano) {
						t.Fatalf("wrong completed window: %#v", request)
					}
				}
				requireMirrorCursor(t, s.catalog, visibilityTestSource, second.SyncedThrough)
				var stored int
				if err := s.catalog.db.QueryRow(`SELECT COUNT(*) FROM catalog_assets`).Scan(&stored); err != nil || stored != 1 {
					t.Fatalf("unseen exclusions inserted: %d, %v", stored, err)
				}
				requireVisibilityGallery(t, s, "stable")
			})
		}
	}
}

func TestImmichMirrorCursorEmptyFullAndInclusiveBoundary(t *testing.T) {
	s := visibilityTestService(t)
	ctx := context.Background()
	assets := []map[string]any{visibilityTestAsset("excluded", "hidden", 1)}
	var requests []map[string]any
	s.client = visibilityTestClient(t, &assets, &requests)
	full, err := s.SyncMirror(ctx, MirrorSyncModeFull)
	if err != nil || full.FetchedCount != 0 {
		t.Fatalf("empty full = %+v, %v", full, err)
	}
	requireMirrorCursor(t, s.catalog, visibilityTestSource, full.SyncedThrough)
	requests = nil
	empty, err := s.SyncMirror(ctx, MirrorSyncModeIncremental)
	if err != nil || empty.Mode != MirrorSyncModeIncremental || empty.FetchedCount != 0 || len(requests) != 3 {
		t.Fatalf("empty delta = %+v, %v, requests=%d", empty, err, len(requests))
	}
	requireMirrorCursor(t, s.catalog, visibilityTestSource, empty.SyncedThrough)
	// Restoration at the exact previous upper bound must remain eligible.
	assets[0]["visibility"] = "timeline"
	assets[0]["updatedAt"] = empty.SyncedThrough.Format(time.RFC3339Nano)
	if _, err := s.SyncMirror(ctx, MirrorSyncModeIncremental); err != nil {
		t.Fatal(err)
	}
	requireVisibilityGallery(t, s, "excluded")
}

func TestImmichMirrorCursorMigratesV5WithoutRebuildingCatalog(t *testing.T) {
	dataDir := t.TempDir()
	store, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	updated := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	started := updated.Add(time.Hour)
	asset := ImmichMirrorAsset{UpstreamAssetID: "preserved", MediaType: "image", Filename: "preserved.jpg", CapturedAt: updated, SourceUpdatedAt: &updated}
	if _, err := store.ReplaceFull(ctx, visibilityTestSource, []ImmichMirrorAsset{asset}, 0, started); err != nil {
		t.Fatal(err)
	}
	var originalID string
	if err := store.db.QueryRow(`SELECT canonical_asset_id FROM catalog_assets`).Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	// Recreate the deployed V5 state: no checkpoint column, completion times
	// newer than stored source timestamps. Migration must not use those times.
	if _, err := store.db.Exec(`ALTER TABLE immich_mirror_state DROP COLUMN synced_through`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.immichMirrorUpdatedAfter(ctx, visibilityTestSource); err == nil {
		t.Fatal("legacy cursor must require explicit full reconciliation")
	}
	completed := started.Add(time.Hour)
	if _, err := store.ReplaceFull(ctx, visibilityTestSource, []ImmichMirrorAsset{asset}, 0, completed, completed); err != nil {
		t.Fatal(err)
	}
	requireMirrorCursor(t, store, visibilityTestSource, completed)
	var id string
	var count int
	if err := store.db.QueryRow(`SELECT canonical_asset_id, COUNT(*) FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = 'preserved'`, visibilityTestSource).Scan(&id, &count); err != nil || count != 1 || id != originalID {
		t.Fatalf("catalog changed during migration: id=%s count=%d, %v", id, count, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	requireMirrorCursor(t, store, visibilityTestSource, completed)
}

func TestImmichMirrorCursorSourceIsolationAndFullReset(t *testing.T) {
	s := visibilityTestService(t)
	ctx := context.Background()
	first := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	const other = "2222222222222222"
	for _, source := range []string{visibilityTestSource, other} {
		if _, err := s.catalog.ReplaceFull(ctx, source, nil, 0, first); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.catalog.MergeIncremental(ctx, visibilityTestSource, nil, first.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	requireMirrorCursor(t, s.catalog, visibilityTestSource, first.Add(time.Hour))
	requireMirrorCursor(t, s.catalog, other, first)
	// A full sync establishes its start, never its later completion timestamp.
	fullStart := first.Add(2 * time.Hour)
	if _, err := s.catalog.ReplaceFull(ctx, visibilityTestSource, nil, 10, fullStart); err != nil {
		t.Fatal(err)
	}
	requireMirrorCursor(t, s.catalog, visibilityTestSource, fullStart)
	requireMirrorCursor(t, s.catalog, other, first)
}

func TestImmichMirrorCursorRejectsBackwardClock(t *testing.T) {
	s := visibilityTestService(t)
	future := time.Now().UTC().Add(time.Hour)
	if _, err := s.catalog.ReplaceFull(context.Background(), visibilityTestSource, nil, 0, future); err != nil {
		t.Fatal(err)
	}
	var assets []map[string]any
	var requests []map[string]any
	s.client = visibilityTestClient(t, &assets, &requests)
	if _, err := s.SyncMirror(context.Background(), MirrorSyncModeIncremental); err == nil || len(requests) != 0 {
		t.Fatalf("backward clock: error=%v, requests=%d", err, len(requests))
	}
	requireMirrorCursor(t, s.catalog, visibilityTestSource, future)
}
