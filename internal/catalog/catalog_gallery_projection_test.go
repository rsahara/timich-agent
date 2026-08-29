package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestMixedGalleryProjectionTracksFallbackAndLocalReadiness(t *testing.T) {
	t.Parallel()

	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	ctx := context.Background()
	const immichSource = "1111111111111111"
	const localSource = "2222222222222222"
	const immichAssetID = "immich-asset"
	const localAssetID = "local-asset"
	sha1Hex := strings.Repeat("a", 40)
	capturedAt := time.Date(2026, 8, 23, 4, 0, 0, 0, time.UTC)
	if _, err := store.ReplaceFull(ctx, immichSource, []ImmichMirrorAsset{{
		UpstreamAssetID:  immichAssetID,
		MediaType:        "image",
		Filename:         "immich.jpg",
		CapturedAt:       capturedAt,
		ContentSHA1Hex:   sha1Hex,
		ContentSizeBytes: 1234,
	}}, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}
	nowText := formatCatalogTime(time.Now().UTC())
	if _, err := store.db.ExecContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename,
			captured_at, visibility_status, content_sha1_hex, content_size_bytes,
			first_seen_at, updated_at
		) VALUES (?, 'local_filesystem', ?, 'image', 'local.jpg', ?, 'active', ?, 1234, ?, ?)`,
		localSource, localAssetID, formatCatalogTime(capturedAt), sha1Hex, nowText, nowText); err != nil {
		t.Fatalf("insert Local catalog source: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO local_assets (
			source_key, asset_id, sha1_hex, content_size_bytes, media_type, filename,
			captured_at, captured_at_source, visibility_status, thumbnail_status,
			first_seen_at, updated_at
		) VALUES (?, ?, ?, 1234, 'image', 'local.jpg', ?, 'metadata', 'active', 'pending', ?, ?)`,
		localSource, localAssetID, sha1Hex, formatCatalogTime(capturedAt), nowText, nowText); err != nil {
		t.Fatalf("insert pending Local asset: %v", err)
	}
	if _, err := store.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}

	readiness := catalogGalleryReadiness{
		localSourceKeys:               []string{localSource},
		localImmichFallbackSourceKeys: []string{localSource},
		immichSourceKeys:              []string{immichSource},
	}
	store.setStandaloneGalleryReadiness(readiness)
	search := func() AssetSearchPage {
		t.Helper()
		normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
			Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
			Page:       AssetSearchPageRequest{Index: 0, Size: 10},
		})
		if err != nil {
			t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
		}
		page, err := store.SearchCatalogAssets(ctx, normalized)
		if err != nil {
			t.Fatalf("SearchCatalogAssets() error = %v", err)
		}
		return page
	}
	if page := search(); page.Total != 1 || page.TotalAccuracy != TotalAccuracyExact ||
		len(page.Items) != 1 || page.Items[0].SourceKey != localSource || page.Items[0].ID != localAssetID {
		t.Fatalf("fallback Gallery page = %#v, want exact Local identity backed by Immich media", page)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE catalog_assets SET visibility_status = 'missing'
		WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, immichAssetID); err != nil {
		t.Fatalf("remove Immich fallback source: %v", err)
	}
	if page := search(); page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("removed-fallback Gallery page = %#v, want pending Local asset hidden", page)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE catalog_assets SET visibility_status = 'active'
		WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, immichAssetID); err != nil {
		t.Fatalf("restore Immich fallback source: %v", err)
	}
	if page := search(); page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("restored-fallback Gallery page = %#v, want pending Local asset visible", page)
	}

	readiness.localImmichFallbackSourceKeys = nil
	store.setStandaloneGalleryReadiness(readiness)
	if page := search(); page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("pending no-fallback Gallery page = %#v, want hidden asset", page)
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin Local readiness update: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_assets SET thumbnail_status = 'ready', updated_at = ?
		WHERE source_key = ? AND asset_id = ?`, nowText, localSource, localAssetID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("mark Local asset ready: %v", err)
	}
	for _, kind := range []string{"preview", "detail_preview"} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO local_renditions (
				source_key, asset_id, kind, status, relative_path, source_sha1_hex
			) VALUES (?, ?, ?, 'ready', ?, ?)`,
			localSource, localAssetID, kind, kind+".jpg", sha1Hex); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert %s rendition: %v", kind, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit Local readiness update: %v", err)
	}
	if page := search(); page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("ready Local Gallery page = %#v, want one asset", page)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE catalog_canonical_assets
		SET filename = 'renamed-local.jpg', updated_at = ?`, nowText); err != nil {
		t.Fatalf("rename canonical asset: %v", err)
	}
	if page := search(); len(page.Items) != 1 || page.Items[0].Filename != "renamed-local.jpg" {
		t.Fatalf("renamed Local Gallery page = %#v, want trigger-refreshed metadata", page)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE local_renditions SET status = 'pending'
		WHERE source_key = ? AND asset_id = ? AND kind = 'preview'`, localSource, localAssetID); err != nil {
		t.Fatalf("invalidate Local rendition: %v", err)
	}
	if page := search(); page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("invalidated Local Gallery page = %#v, want hidden asset", page)
	}

	store.setStandaloneGalleryReadiness(catalogGalleryReadiness{
		immichSourceKeys: []string{immichSource},
		immichOnly:       true,
	})
	var projectionStateCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_gallery_projection_state`).Scan(&projectionStateCount); err != nil {
		t.Fatalf("count mixed projection state: %v", err)
	}
	if projectionStateCount != 0 {
		t.Fatalf("mixed projection state count = %d after Immich-only reconfigure, want 0", projectionStateCount)
	}
}

func TestMixedGalleryProjectionDeepPageUsesDayAnchorSeek(t *testing.T) {
	t.Parallel()

	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		immichSource = "1111111111111111"
		localSource  = "2222222222222222"
		assetCount   = 6_001
	)
	readiness := catalogGalleryReadiness{
		localSourceKeys:  []string{localSource},
		immichSourceKeys: []string{immichSource},
	}
	store.setStandaloneGalleryReadiness(readiness)
	newest := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin projection seed: %v", err)
	}
	statement, err := tx.Prepare(`INSERT INTO catalog_gallery_projection (
		canonical_asset_id, source_key, upstream_asset_id, media_type,
		filename, captured_at, duration
	) VALUES (?, ?, ?, 'image', ?, ?, NULL)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare projection seed: %v", err)
	}
	for index := 0; index < assetCount; index++ {
		assetID := fmt.Sprintf("asset-%06d", index)
		capturedAt := newest.AddDate(0, 0, -index/25).Add(-time.Duration(index%25) * time.Second)
		if _, err := statement.Exec(assetID, immichSource, assetID, assetID+".jpg", formatCatalogTime(capturedAt)); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			t.Fatalf("insert projection asset %d: %v", index, err)
		}
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close projection seed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit projection seed: %v", err)
	}

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 499, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize deep mixed Gallery request: %v", err)
	}
	page, err := store.SearchCatalogAssets(context.Background(), normalized)
	if err != nil {
		t.Fatalf("SearchCatalogAssets() error = %v", err)
	}
	if len(page.Items) != 10 || page.Items[0].ID != "asset-004990" || page.Items[9].ID != "asset-004999" {
		t.Fatalf("deep mixed Gallery page = %#v, want asset-004990 through asset-004999", page.Items)
	}
	if page.Total != 5_001 || page.TotalAccuracy != TotalAccuracyLowerBound ||
		page.NextPageIndex == nil || *page.NextPageIndex != 500 {
		t.Fatalf("deep mixed Gallery page metadata = %#v, want bounded look-ahead lower bound", page)
	}

	dayPlan := explainGalleryProjectionQueryPlan(
		t,
		store.db,
		galleryProjectionDayAtOffsetSQL,
		4_990,
		4_990,
	)
	if !strings.Contains(dayPlan, "catalog_gallery_projection_days") ||
		strings.Contains(dayPlan, "SCAN catalog_gallery_projection ") {
		t.Fatalf("day lookup scans the full mixed projection:\n%s", dayPlan)
	}
	anchorPlan := explainGalleryProjectionQueryPlan(
		t,
		store.db,
		galleryProjectionAnchorWithinDaySQL,
		formatCatalogTime(newest.AddDate(0, 0, -199)),
		formatCatalogTime(newest.AddDate(0, 0, -198)),
		15,
	)
	if !strings.Contains(anchorPlan, "idx_catalog_gallery_projection_captured") ||
		strings.Contains(anchorPlan, "SCAN catalog_gallery_projection") {
		t.Fatalf("within-day anchor lookup does not use the captured index:\n%s", anchorPlan)
	}
	pagePlan := explainGalleryProjectionQueryPlan(
		t,
		store.db,
		galleryProjectionAfterAnchorSQL,
		formatCatalogTime(newest.AddDate(0, 0, -199)),
		formatCatalogTime(newest.AddDate(0, 0, -199)),
		"asset-004990",
		11,
	)
	if !strings.Contains(pagePlan, "idx_catalog_gallery_projection_captured") ||
		strings.Contains(pagePlan, "SCAN catalog_gallery_projection") {
		t.Fatalf("anchored page lookup does not use the captured index:\n%s", pagePlan)
	}

	if _, err := store.db.Exec(`UPDATE catalog_gallery_projection
		SET captured_at = ? WHERE canonical_asset_id = 'asset-005999'`, formatCatalogTime(newest.Add(time.Minute))); err != nil {
		t.Fatalf("move projection asset to another day: %v", err)
	}
	if _, err := store.db.Exec(`DELETE FROM catalog_gallery_projection
		WHERE canonical_asset_id = 'asset-000000'`); err != nil {
		t.Fatalf("delete projection asset: %v", err)
	}
	assertGalleryProjectionDayIndexMatches(t, store.db)

	if _, err := store.db.Exec(`DELETE FROM catalog_gallery_projection_day_index_state`); err != nil {
		t.Fatalf("clear day index marker: %v", err)
	}
	if _, err := store.db.Exec(`DELETE FROM catalog_gallery_projection_days`); err != nil {
		t.Fatalf("clear day index: %v", err)
	}
	if err := store.ensureGalleryProjectionDayIndex(context.Background()); err != nil {
		t.Fatalf("ensureGalleryProjectionDayIndex() error = %v", err)
	}
	assertGalleryProjectionDayIndexMatches(t, store.db)
}

func TestMixedGalleryProjectionDayAnchorPreservesCanonicalTieBreak(t *testing.T) {
	t.Parallel()

	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	readiness := catalogGalleryReadiness{
		localSourceKeys:  []string{"2222222222222222"},
		immichSourceKeys: []string{"1111111111111111"},
	}
	store.setStandaloneGalleryReadiness(readiness)
	capturedAt := formatCatalogTime(time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	for index := 0; index < 30; index++ {
		assetID := fmt.Sprintf("asset-%03d", index)
		if _, err := store.db.Exec(`INSERT INTO catalog_gallery_projection (
			canonical_asset_id, source_key, upstream_asset_id, media_type,
			filename, captured_at, duration
		) VALUES (?, '1111111111111111', ?, 'image', ?, ?, NULL)`,
			assetID, assetID, assetID+".jpg", capturedAt); err != nil {
			t.Fatalf("insert tied projection asset %d: %v", index, err)
		}
	}

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 2, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize tied deep mixed Gallery request: %v", err)
	}
	page, err := store.SearchCatalogAssets(context.Background(), normalized)
	if err != nil {
		t.Fatalf("SearchCatalogAssets() error = %v", err)
	}
	if len(page.Items) != 10 || page.Items[0].ID != "asset-020" || page.Items[9].ID != "asset-029" {
		t.Fatalf("tied deep mixed Gallery page = %#v, want canonical IDs asset-020 through asset-029", page.Items)
	}
}

func explainGalleryProjectionQueryPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN error = %v", err)
	}
	defer rows.Close()
	details := []string{}
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate query plan: %v", err)
	}
	return strings.Join(details, "\n")
}

func assertGalleryProjectionDayIndexMatches(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT captured_day, item_count
		FROM catalog_gallery_projection_days ORDER BY captured_day`)
	if err != nil {
		t.Fatalf("read mixed Gallery day index: %v", err)
	}
	indexed := map[string]int{}
	for rows.Next() {
		var day string
		var count int
		if err := rows.Scan(&day, &count); err != nil {
			_ = rows.Close()
			t.Fatalf("scan mixed Gallery day index: %v", err)
		}
		indexed[day] = count
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close mixed Gallery day index: %v", err)
	}

	rows, err = db.Query(`SELECT substr(captured_at, 1, 10), COUNT(*)
		FROM catalog_gallery_projection
		GROUP BY substr(captured_at, 1, 10)
		ORDER BY substr(captured_at, 1, 10)`)
	if err != nil {
		t.Fatalf("derive mixed Gallery day counts: %v", err)
	}
	derived := map[string]int{}
	for rows.Next() {
		var day string
		var count int
		if err := rows.Scan(&day, &count); err != nil {
			_ = rows.Close()
			t.Fatalf("scan derived mixed Gallery day counts: %v", err)
		}
		derived[day] = count
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close derived mixed Gallery day counts: %v", err)
	}
	if fmt.Sprint(indexed) != fmt.Sprint(derived) {
		t.Fatalf("mixed Gallery day index = %v, want %v", indexed, derived)
	}
}

func TestMixedGalleryProjectionReconfigurePublishesConfiguredSourceScope(t *testing.T) {
	t.Parallel()

	const (
		firstImmich  = "1111111111111111"
		firstLocal   = "2222222222222222"
		secondImmich = "3333333333333333"
		secondLocal  = "4444444444444444"
	)
	mixedDatasources := func(localSource string, immichSource string) []config.DatasourceConfig {
		return []config.DatasourceConfig{
			{
				SourceKey: localSource,
				Name:      "Local",
				Kind:      config.DatasourceKindLocalFiles,
				RootKey:   "photos",
			},
			{
				SourceKey: immichSource,
				Name:      "Immich",
				Kind:      config.DatasourceKindImmichIndexed,
				URL:       "http://immich.test",
			},
		}
	}
	service, err := NewServiceWithOptions(
		mixedDatasources(firstLocal, firstImmich),
		ServiceOptions{DataDir: t.TempDir()},
	)
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })

	capturedAt := time.Date(2026, 8, 23, 4, 0, 0, 0, time.UTC)
	sharedSHA1 := strings.Repeat("b", 40)
	for _, source := range []struct {
		sourceKey string
		assets    []ImmichMirrorAsset
	}{
		{
			sourceKey: firstImmich,
			assets: []ImmichMirrorAsset{
				{
					UpstreamAssetID:  "removed-primary",
					MediaType:        "image",
					Filename:         "removed-primary.jpg",
					CapturedAt:       capturedAt,
					ContentSHA1Hex:   sharedSHA1,
					ContentSizeBytes: 1234,
				},
				{
					UpstreamAssetID: "removed-only",
					MediaType:       "image",
					Filename:        "removed-only.jpg",
					CapturedAt:      capturedAt.Add(-time.Minute),
				},
			},
		},
		{
			sourceKey: secondImmich,
			assets: []ImmichMirrorAsset{{
				UpstreamAssetID:  "configured-duplicate",
				MediaType:        "image",
				Filename:         "configured-duplicate.jpg",
				CapturedAt:       capturedAt,
				ContentSHA1Hex:   sharedSHA1,
				ContentSizeBytes: 1234,
			}},
		},
	} {
		if _, err := service.catalog.ReplaceFull(context.Background(), source.sourceKey, source.assets, 0, time.Now().UTC()); err != nil {
			t.Fatalf("ReplaceFull(%s) error = %v", source.sourceKey, err)
		}
	}

	service.ReconfigureDatasources(mixedDatasources(secondLocal, secondImmich))
	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 ||
		page.Items[0].SourceKey != secondImmich || page.Items[0].ID != "configured-duplicate" {
		t.Fatalf("reconfigured mixed Gallery page = %#v, want only second configured source", page)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE catalog_assets
		SET updated_at = ?
		WHERE source_key = ? AND upstream_asset_id = 'removed-only'`,
		formatCatalogTime(time.Now().UTC()), firstImmich); err != nil {
		t.Fatalf("update removed source after reconfigure: %v", err)
	}
	page, err = service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() after removed-source update error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 ||
		page.Items[0].SourceKey != secondImmich || page.Items[0].ID != "configured-duplicate" {
		t.Fatalf("post-update mixed Gallery page = %#v, want removed source to stay excluded", page)
	}
}
