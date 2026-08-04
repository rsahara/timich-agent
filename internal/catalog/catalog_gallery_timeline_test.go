package catalog

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestGalleryTimelineDeepPageUsesGlobalPositionSeek(t *testing.T) {
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

	const sourceKey = "1111111111111111"
	store.setStandaloneGalleryReadiness(catalogGalleryReadiness{
		immichSourceKeys: []string{sourceKey},
		immichOnly:       true,
	})
	capturedAt := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	assets := make([]ImmichMirrorAsset, 6_001)
	for index := range assets {
		id := fmt.Sprintf("asset-%06d", index)
		assets[index] = ImmichMirrorAsset{
			UpstreamAssetID: id,
			MediaType:       "image",
			Filename:        id + ".jpg",
			CapturedAt:      capturedAt.Add(-time.Duration(index) * time.Second),
		}
	}
	if _, err := store.ReplaceFull(context.Background(), sourceKey, assets, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 500, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize timeline request: %v", err)
	}
	page, err := store.SearchCatalogAssets(context.Background(), normalized)
	if err != nil {
		t.Fatalf("SearchCatalogAssets() error = %v", err)
	}
	if len(page.Items) != 10 || page.Items[0].ID != "asset-005000" || page.Items[9].ID != "asset-005009" {
		t.Fatalf("deep page items = %#v, want asset-005000 through asset-005009", page.Items)
	}
	if page.Total != 5_011 || page.TotalAccuracy != TotalAccuracyLowerBound || page.NextPageIndex == nil || *page.NextPageIndex != 501 {
		t.Fatalf("deep page metadata = %#v, want bounded look-ahead lower bound", page)
	}

	var generation int64
	var total int
	if err := store.queryDB().QueryRow(`SELECT generation, total_count
		FROM catalog_gallery_timeline_state WHERE singleton_id = ?`, catalogGalleryTimelineStateID).Scan(&generation, &total); err != nil {
		t.Fatalf("read gallery timeline state: %v", err)
	}
	if total != len(assets) {
		t.Fatalf("gallery timeline total = %d, want %d", total, len(assets))
	}
	rows, err := store.queryDB().Query(`EXPLAIN QUERY PLAN
		SELECT source_key, upstream_asset_id, media_type, filename, captured_at, duration
		FROM catalog_gallery_timeline
		WHERE generation = ? AND global_position >= ? AND global_position < ?
		ORDER BY global_position ASC
		LIMIT ?`, generation, 5_000, total, 11)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN error = %v", err)
	}
	defer rows.Close()
	planDetails := []string{}
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		planDetails = append(planDetails, detail)
	}
	plan := strings.Join(planDetails, "\n")
	if !strings.Contains(plan, "SEARCH catalog_gallery_timeline") || strings.Contains(plan, "SCAN catalog_gallery_timeline") {
		t.Fatalf("deep page does not use a bounded position seek:\n%s", plan)
	}
}

func TestGalleryTimelineDateRangeUsesPositionBoundaries(t *testing.T) {
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

	const sourceKey = "1111111111111111"
	store.setStandaloneGalleryReadiness(catalogGalleryReadiness{
		immichSourceKeys: []string{sourceKey},
		immichOnly:       true,
	})
	newest := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	assets := make([]ImmichMirrorAsset, 10)
	for index := range assets {
		id := fmt.Sprintf("asset-%02d", index)
		assets[index] = ImmichMirrorAsset{
			UpstreamAssetID: id,
			MediaType:       "image",
			Filename:        id + ".jpg",
			CapturedAt:      newest.AddDate(0, 0, -index),
		}
	}
	if _, err := store.ReplaceFull(context.Background(), sourceKey, assets, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}

	from := newest.AddDate(0, 0, -6)
	to := newest.AddDate(0, 0, -3)
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindTimeline,
			Filters: AssetSearchFilters{CapturedAt: &AssetSearchCapturedTime{
				From: &from,
				To:   &to,
			}},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 1},
	})
	if err != nil {
		t.Fatalf("normalize date range request: %v", err)
	}
	page, err := store.SearchCatalogAssets(context.Background(), normalized)
	if err != nil {
		t.Fatalf("SearchCatalogAssets() error = %v", err)
	}
	if page.Total != 3 || page.TotalAccuracy != TotalAccuracyExact || len(page.Items) != 1 || page.Items[0].ID != "asset-04" {
		t.Fatalf("date range page = %#v, want three assets beginning at asset-04", page)
	}
}

func TestGalleryTimelineStartupReusesPublishedGeneration(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	const sourceKey = "1111111111111111"
	datasources := []config.DatasourceConfig{{
		SourceKey: sourceKey,
		Name:      "Home Immich",
		Kind:      config.DatasourceKindImmichIndexed,
		URL:       "http://immich.test",
	}}
	service, err := NewServiceWithOptions(datasources, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	capturedAt := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	if _, err := service.catalog.ReplaceFull(context.Background(), sourceKey, []ImmichMirrorAsset{{
		UpstreamAssetID: "asset-1",
		MediaType:       "image",
		Filename:        "asset-1.jpg",
		CapturedAt:      capturedAt,
	}}, 0, time.Now().UTC()); err != nil {
		_ = service.Close()
		t.Fatalf("ReplaceFull() error = %v", err)
	}
	var firstGeneration int64
	if err := service.catalog.queryDB().QueryRow(`SELECT generation
		FROM catalog_gallery_timeline_state WHERE singleton_id = ?`, catalogGalleryTimelineStateID).Scan(&firstGeneration); err != nil {
		_ = service.Close()
		t.Fatalf("read first generation: %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("close first service: %v", err)
	}

	reopened, err := NewServiceWithOptions(datasources, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("reopen service: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Fatalf("close reopened service: %v", err)
		}
	})
	var reopenedGeneration int64
	if err := reopened.catalog.queryDB().QueryRow(`SELECT generation
		FROM catalog_gallery_timeline_state WHERE singleton_id = ?`, catalogGalleryTimelineStateID).Scan(&reopenedGeneration); err != nil {
		t.Fatalf("read reopened generation: %v", err)
	}
	if reopenedGeneration != firstGeneration {
		t.Fatalf("reopened generation = %d, want existing generation %d", reopenedGeneration, firstGeneration)
	}
}

func TestGalleryTimelineReconfigurePublishesConfiguredSourceScope(t *testing.T) {
	t.Parallel()

	const firstSource = "1111111111111111"
	const secondSource = "2222222222222222"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: firstSource,
		Name:      "First Immich",
		Kind:      config.DatasourceKindImmichIndexed,
		URL:       "http://first.test",
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	capturedAt := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	for sourceKey, assetID := range map[string]string{
		firstSource:  "first-asset",
		secondSource: "second-asset",
	} {
		if _, err := service.catalog.ReplaceFull(context.Background(), sourceKey, []ImmichMirrorAsset{{
			UpstreamAssetID: assetID,
			MediaType:       "image",
			Filename:        assetID + ".jpg",
			CapturedAt:      capturedAt,
		}}, 0, time.Now().UTC()); err != nil {
			t.Fatalf("ReplaceFull(%s) error = %v", sourceKey, err)
		}
	}
	service.ReconfigureDatasources([]config.DatasourceConfig{{
		SourceKey: secondSource,
		Name:      "Second Immich",
		Kind:      config.DatasourceKindImmichIndexed,
		URL:       "http://second.test",
	}})

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize timeline request: %v", err)
	}
	page, err := service.catalog.SearchCatalogAssets(context.Background(), normalized)
	if err != nil {
		t.Fatalf("SearchCatalogAssets() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].SourceKey != secondSource || page.Items[0].ID != "second-asset" {
		t.Fatalf("reconfigured timeline = %#v, want only second configured source", page)
	}
}

func TestGalleryTimelineTracksCanonicalCommitAfterImmichOnlyReconfigure(t *testing.T) {
	t.Parallel()

	const immichSource = "1111111111111111"
	const localSource = "2222222222222222"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{
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
	}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	store := service.catalog

	ctx := context.Background()
	const immichAssetID = "immich-family"
	const localAssetID = "local-family"
	sha1Hex := strings.Repeat("a", 40)
	capturedAt := time.Date(2026, 8, 4, 1, 0, 0, 0, time.UTC)
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

	// The Local worker was admitted while the configuration was mixed, but its
	// file processing finishes and opens the write transaction only after the
	// configured datasource has become Immich-only.
	service.ReconfigureDatasources([]config.DatasourceConfig{{
		SourceKey: immichSource,
		Name:      "Immich",
		Kind:      config.DatasourceKindImmichIndexed,
		URL:       "http://immich.test",
	}})
	localTx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin in-flight Local transaction: %v", err)
	}
	defer localTx.Rollback()

	nowText := formatCatalogTime(time.Now().UTC())
	if _, err := localTx.ExecContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename,
			captured_at, duration, visibility_status, source_updated_at, is_favorite,
			content_sha1_hex, content_size_bytes, place_label, description, first_seen_at,
			updated_at
		) VALUES (?, 'local_filesystem', ?, 'image', 'local.jpg', ?, NULL, 'active', ?, 0, ?, 1234, NULL, NULL, ?, ?)`,
		localSource,
		localAssetID,
		formatCatalogTime(capturedAt),
		nowText,
		sha1Hex,
		nowText,
		nowText,
	); err != nil {
		t.Fatalf("insert in-flight Local source row: %v", err)
	}
	if err := store.refreshCatalogCanonicalAssetInTx(ctx, localTx, localSource, localAssetID, nowText); err != nil {
		t.Fatalf("refresh canonical asset from Local transaction: %v", err)
	}
	if err := store.commitCatalogAssetChanges(ctx, localTx, true); err != nil {
		t.Fatalf("commit in-flight Local canonical change: %v", err)
	}

	var currentCanonicalGeneration int64
	var timelineCanonicalGeneration int64
	var timelineFilename string
	if err := store.queryDB().QueryRowContext(ctx, `SELECT canonical.generation,
			timeline.canonical_generation, item.filename
		FROM catalog_canonical_state canonical
		JOIN catalog_gallery_timeline_state timeline ON timeline.singleton_id = canonical.singleton_id
		JOIN catalog_gallery_timeline item ON item.generation = timeline.generation
		WHERE canonical.singleton_id = ?`, catalogGalleryTimelineStateID).Scan(
		&currentCanonicalGeneration,
		&timelineCanonicalGeneration,
		&timelineFilename,
	); err != nil {
		t.Fatalf("read published canonical and Gallery generations: %v", err)
	}
	if timelineCanonicalGeneration != currentCanonicalGeneration || timelineFilename != "local.jpg" {
		t.Fatalf("published Gallery canonical generation=%d current=%d filename=%q, want matching generations and local.jpg", timelineCanonicalGeneration, currentCanonicalGeneration, timelineFilename)
	}

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize timeline request: %v", err)
	}
	page, err := store.SearchCatalogAssets(ctx, normalized)
	if err != nil {
		t.Fatalf("SearchCatalogAssets() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Filename != "local.jpg" || page.Items[0].SourceKey != immichSource || page.Items[0].ID != immichAssetID {
		t.Fatalf("Gallery page = %#v, want current canonical metadata through configured Immich identity", page)
	}

	if err := store.ensureGalleryTimeline(ctx, catalogGalleryReadiness{
		immichSourceKeys: []string{immichSource},
		immichOnly:       true,
	}); err != nil {
		t.Fatalf("ensureGalleryTimeline() error = %v", err)
	}
	if _, err := store.MergeIncremental(ctx, immichSource, nil, time.Now().UTC()); err != nil {
		t.Fatalf("MergeIncremental(no changes) error = %v", err)
	}
	var finalCanonicalGeneration int64
	var finalTimelineCanonicalGeneration int64
	if err := store.queryDB().QueryRowContext(ctx, `SELECT canonical.generation, timeline.canonical_generation
		FROM catalog_canonical_state canonical
		JOIN catalog_gallery_timeline_state timeline ON timeline.singleton_id = canonical.singleton_id
		WHERE canonical.singleton_id = ?`, catalogGalleryTimelineStateID).Scan(
		&finalCanonicalGeneration,
		&finalTimelineCanonicalGeneration,
	); err != nil {
		t.Fatalf("read final Gallery generations: %v", err)
	}
	if finalCanonicalGeneration != currentCanonicalGeneration || finalTimelineCanonicalGeneration != currentCanonicalGeneration {
		t.Fatalf("final generations canonical=%d timeline=%d, want unchanged %d", finalCanonicalGeneration, finalTimelineCanonicalGeneration, currentCanonicalGeneration)
	}
}

func TestGalleryTimelineRejectsAndRepairsStaleCanonicalGeneration(t *testing.T) {
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
	const sourceKey = "1111111111111111"
	readiness := catalogGalleryReadiness{
		immichSourceKeys: []string{sourceKey},
		immichOnly:       true,
	}
	store.setStandaloneGalleryReadiness(readiness)
	if _, err := store.ReplaceFull(ctx, sourceKey, []ImmichMirrorAsset{{
		UpstreamAssetID: "asset-1",
		MediaType:       "image",
		Filename:        "before.jpg",
		CapturedAt:      time.Date(2026, 8, 4, 2, 0, 0, 0, time.UTC),
	}}, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin stale-generation simulation: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE catalog_canonical_assets
		SET filename = 'after.jpg', updated_at = ?`, formatCatalogTime(time.Now().UTC())); err != nil {
		_ = tx.Rollback()
		t.Fatalf("update canonical asset without Gallery publication: %v", err)
	}
	if err := advanceCatalogCanonicalGenerationInTx(ctx, tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("advance canonical generation: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit stale-generation simulation: %v", err)
	}

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize timeline request: %v", err)
	}
	page, err := store.SearchCatalogAssets(ctx, normalized)
	if err != nil {
		t.Fatalf("SearchCatalogAssets() with stale projection error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Filename != "after.jpg" {
		t.Fatalf("stale-generation Gallery page = %#v, want canonical fallback after.jpg", page)
	}

	if err := store.ensureGalleryTimeline(ctx, readiness); err != nil {
		t.Fatalf("ensureGalleryTimeline() stale repair error = %v", err)
	}
	var canonicalGeneration int64
	var timelineCanonicalGeneration int64
	var timelineFilename string
	if err := store.queryDB().QueryRowContext(ctx, `SELECT canonical.generation,
			timeline.canonical_generation, item.filename
		FROM catalog_canonical_state canonical
		JOIN catalog_gallery_timeline_state timeline ON timeline.singleton_id = canonical.singleton_id
		JOIN catalog_gallery_timeline item ON item.generation = timeline.generation
		WHERE canonical.singleton_id = ?`, catalogGalleryTimelineStateID).Scan(
		&canonicalGeneration,
		&timelineCanonicalGeneration,
		&timelineFilename,
	); err != nil {
		t.Fatalf("read repaired Gallery generation: %v", err)
	}
	if timelineCanonicalGeneration != canonicalGeneration || timelineFilename != "after.jpg" {
		t.Fatalf("repaired Gallery canonical generation=%d current=%d filename=%q, want matching generations and after.jpg", timelineCanonicalGeneration, canonicalGeneration, timelineFilename)
	}
}
