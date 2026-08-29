package catalog

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestNormalizeAssetSearchRequestRejectsOverflowingPageOffset(t *testing.T) {
	t.Parallel()

	pageSize := 60
	maxSafeIndex := (math.MaxInt - pageSize) / pageSize
	if _, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Page: AssetSearchPageRequest{Index: maxSafeIndex, Size: pageSize},
	}); err != nil {
		t.Fatalf("normalizeAssetSearchRequest(max safe page) error = %v", err)
	}
	if _, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Page: AssetSearchPageRequest{Index: maxSafeIndex + 1, Size: pageSize},
	}); !errors.Is(err, ErrInvalidSearchRequest) {
		t.Fatalf("normalizeAssetSearchRequest(overflow page) error = %v, want ErrInvalidSearchRequest", err)
	}
	if _, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Page: AssetSearchPageRequest{Index: math.MaxInt},
	}); !errors.Is(err, ErrInvalidSearchRequest) {
		t.Fatalf("normalizeAssetSearchRequest(default-size overflow page) error = %v, want ErrInvalidSearchRequest", err)
	}
}

func TestCatalogGalleryReadinessUsesImmichOnlyFastPath(t *testing.T) {
	t.Parallel()

	const immichSource = "1111111111111111"
	readiness := catalogGalleryReadinessForDatasources([]config.DatasourceConfig{{
		SourceKey: immichSource,
		Kind:      config.DatasourceKindImmichIndexed,
	}})
	clause, args := catalogGalleryReadinessClause(func(name string) string {
		return "c." + name
	}, readiness)
	if !strings.Contains(clause, "configured_immich.canonical_asset_id = c.canonical_asset_id") ||
		!strings.Contains(clause, "configured_immich.source_key IN (?)") {
		t.Fatalf("Immich-only readiness clause = %q, want configured active-source lookup", clause)
	}
	if len(args) != 1 || args[0] != immichSource {
		t.Fatalf("Immich-only readiness args = %#v, want source key", args)
	}
	if !strings.Contains(clause, "EXISTS") || strings.Contains(clause, "local_") {
		t.Fatalf("Immich-only readiness clause unexpectedly joins Local state: %s", clause)
	}
	sourceKeyColumn, upstreamAssetIDColumn, sourceArgs := catalogGallerySourceProjection(func(name string) string {
		return "c." + name
	}, readiness)
	if !strings.Contains(sourceKeyColumn, "configured_immich.source_key") ||
		!strings.Contains(upstreamAssetIDColumn, "configured_immich.upstream_asset_id") {
		t.Fatalf("Immich-only source projection = %q / %q, want configured source identity", sourceKeyColumn, upstreamAssetIDColumn)
	}
	if !reflect.DeepEqual(sourceArgs, []any{immichSource, immichSource}) {
		t.Fatalf("Immich-only source projection args = %#v, want source key for each projected column", sourceArgs)
	}

	mixed := catalogGalleryReadinessForDatasources([]config.DatasourceConfig{
		{SourceKey: immichSource, Kind: config.DatasourceKindImmichIndexed},
		{SourceKey: "2222222222222222", Kind: config.DatasourceKindLocalFiles},
	})
	mixedClause, _ := catalogGalleryReadinessClause(func(name string) string {
		return "c." + name
	}, mixed)
	if !strings.Contains(mixedClause, "local_assets") || !strings.Contains(mixedClause, "local_renditions") {
		t.Fatalf("mixed readiness clause = %q, want Local rendition checks", mixedClause)
	}
}

func TestCatalogGalleryKeepsConfiguredImmichDuplicateWhenPrimaryIsRemoved(t *testing.T) {
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
	const removedSource = "1111111111111111"
	const configuredSource = "2222222222222222"
	const configuredAssetID = "configured-copy"
	capturedAt := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	sha1Hex := strings.Repeat("a", 40)
	for _, source := range []struct {
		key    string
		assets []ImmichMirrorAsset
	}{
		{key: removedSource, assets: []ImmichMirrorAsset{
			{
				UpstreamAssetID:  "removed-primary",
				MediaType:        "image",
				Filename:         "removed-primary.jpg",
				CapturedAt:       capturedAt,
				ContentSHA1Hex:   sha1Hex,
				ContentSizeBytes: 1234,
			},
			{
				UpstreamAssetID: "removed-only",
				MediaType:       "image",
				Filename:        "removed-only.jpg",
				CapturedAt:      capturedAt.Add(-time.Minute),
			},
		}},
		{key: configuredSource, assets: []ImmichMirrorAsset{{
			UpstreamAssetID:  configuredAssetID,
			MediaType:        "image",
			Filename:         configuredAssetID + ".jpg",
			CapturedAt:       capturedAt,
			ContentSHA1Hex:   sha1Hex,
			ContentSizeBytes: 1234,
		}}},
	} {
		if _, err := store.ReplaceFull(ctx, source.key, source.assets, 0, time.Now().UTC()); err != nil {
			t.Fatalf("ReplaceFull(%s) error = %v", source.key, err)
		}
	}

	var primarySource string
	if err := store.queryDB().QueryRowContext(ctx, `SELECT primary_source_key
		FROM catalog_canonical_assets`).Scan(&primarySource); err != nil {
		t.Fatalf("read canonical primary: %v", err)
	}
	if primarySource != removedSource {
		t.Fatalf("canonical primary = %q, want removed source %q", primarySource, removedSource)
	}
	store.setStandaloneGalleryReadiness(catalogGalleryReadiness{
		immichSourceKeys: []string{configuredSource},
		immichOnly:       true,
	})
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
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("timeline page total=%d items=%d, want configured duplicate", page.Total, len(page.Items))
	}
	if page.Items[0].SourceKey != configuredSource || page.Items[0].ID != configuredAssetID {
		t.Fatalf("timeline item = %#v, want configured duplicate identity", page.Items[0])
	}
	semanticItems, err := store.canonicalAssetsForScoredSources(ctx, []semanticScoredAsset{{
		Asset:      semanticAsset{SourceKey: configuredSource, ID: configuredAssetID},
		Similarity: 0.9,
	}}, true)
	if err != nil {
		t.Fatalf("canonicalAssetsForScoredSources() error = %v", err)
	}
	if len(semanticItems) != 1 || semanticItems[0].SourceKey != configuredSource || semanticItems[0].ID != configuredAssetID {
		t.Fatalf("semantic item = %#v, want configured duplicate identity", semanticItems)
	}
}

func TestCatalogTimelineExactTotalUsesConfiguredSourceIndex(t *testing.T) {
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

	readiness := catalogGalleryReadiness{
		immichSourceKeys: []string{"1111111111111111"},
		immichOnly:       true,
	}
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 60},
	})
	if err != nil {
		t.Fatalf("normalize timeline request: %v", err)
	}
	where, whereArgs := catalogSearchWhere(normalized, "c", readiness)
	query, args := catalogExactTotalQuery(normalized, readiness, where, whereArgs)
	rows, err := store.queryDB().QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
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
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "COVERING INDEX "+catalogGallerySourceCanonicalIndex) {
		t.Fatalf("timeline count does not use configured-source covering index:\n%s", plan)
	}
	if strings.Contains(plan, "CORRELATED") {
		t.Fatalf("timeline count still uses a correlated lookup:\n%s", plan)
	}
}

func TestCatalogTimelineReadModelPublishesWithMirrorCommit(t *testing.T) {
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
	capturedAt := time.Date(2026, 8, 3, 11, 0, 0, 0, time.UTC)
	replace := func(assets []ImmichMirrorAsset) {
		t.Helper()
		if _, err := store.ReplaceFull(context.Background(), sourceKey, assets, 0, time.Now().UTC()); err != nil {
			t.Fatalf("ReplaceFull() error = %v", err)
		}
	}
	search := func(pageSize int) AssetSearchPage {
		t.Helper()
		normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
			Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
			Page:       AssetSearchPageRequest{Index: 0, Size: pageSize},
		})
		if err != nil {
			t.Fatalf("normalize timeline request: %v", err)
		}
		page, err := store.SearchCatalogAssets(context.Background(), normalized)
		if err != nil {
			t.Fatalf("SearchCatalogAssets() error = %v", err)
		}
		return page
	}

	replace([]ImmichMirrorAsset{{
		UpstreamAssetID: "asset-1",
		MediaType:       "image",
		Filename:        "asset-1.jpg",
		CapturedAt:      capturedAt,
	}})
	if page := search(1); page.Total != 1 {
		t.Fatalf("initial timeline total = %d, want 1", page.Total)
	}
	var firstGeneration int64
	var firstTotal int
	if err := store.queryDB().QueryRow(`SELECT generation, total_count
		FROM catalog_gallery_timeline_state WHERE singleton_id = ?`, catalogGalleryTimelineStateID).Scan(&firstGeneration, &firstTotal); err != nil {
		t.Fatalf("read initial gallery timeline state: %v", err)
	}
	if firstGeneration <= 0 || firstTotal != 1 {
		t.Fatalf("initial gallery timeline generation=%d total=%d, want positive generation and total 1", firstGeneration, firstTotal)
	}
	if page := search(10); page.Total != 1 {
		t.Fatalf("reused timeline total = %d, want 1", page.Total)
	}

	replace([]ImmichMirrorAsset{
		{
			UpstreamAssetID: "asset-1",
			MediaType:       "image",
			Filename:        "asset-1.jpg",
			CapturedAt:      capturedAt,
		},
		{
			UpstreamAssetID: "asset-2",
			MediaType:       "image",
			Filename:        "asset-2.jpg",
			CapturedAt:      capturedAt.Add(-time.Minute),
		},
	})
	var secondGeneration int64
	var secondTotal int
	if err := store.queryDB().QueryRow(`SELECT generation, total_count
		FROM catalog_gallery_timeline_state WHERE singleton_id = ?`, catalogGalleryTimelineStateID).Scan(&secondGeneration, &secondTotal); err != nil {
		t.Fatalf("read updated gallery timeline state: %v", err)
	}
	if secondGeneration <= firstGeneration || secondTotal != 2 {
		t.Fatalf("updated gallery timeline generation=%d total=%d, want generation > %d and total 2", secondGeneration, secondTotal, firstGeneration)
	}
	if page := search(10); page.Total != 2 {
		t.Fatalf("timeline total after mirror commit = %d, want 2", page.Total)
	}

	for name, incrementalAssets := range map[string][]ImmichMirrorAsset{
		"empty": nil,
		"unchanged": {{
			UpstreamAssetID: "asset-1",
			MediaType:       "image",
			Filename:        "asset-1.jpg",
			CapturedAt:      capturedAt,
		}},
	} {
		if _, err := store.MergeIncremental(context.Background(), sourceKey, incrementalAssets, time.Now().UTC()); err != nil {
			t.Fatalf("MergeIncremental(%s) error = %v", name, err)
		}
		var generation int64
		if err := store.queryDB().QueryRow(`SELECT generation
			FROM catalog_gallery_timeline_state WHERE singleton_id = ?`, catalogGalleryTimelineStateID).Scan(&generation); err != nil {
			t.Fatalf("read gallery timeline state after %s incremental sync: %v", name, err)
		}
		if generation != secondGeneration {
			t.Fatalf("generation after %s incremental sync = %d, want unchanged %d", name, generation, secondGeneration)
		}
	}

	if _, err := store.MergeIncremental(context.Background(), sourceKey, []ImmichMirrorAsset{{
		UpstreamAssetID: "asset-1",
		MediaType:       "image",
		Filename:        "asset-1-renamed.jpg",
		CapturedAt:      capturedAt,
	}}, time.Now().UTC()); err != nil {
		t.Fatalf("MergeIncremental(changed) error = %v", err)
	}
	var thirdGeneration int64
	if err := store.queryDB().QueryRow(`SELECT generation
		FROM catalog_gallery_timeline_state WHERE singleton_id = ?`, catalogGalleryTimelineStateID).Scan(&thirdGeneration); err != nil {
		t.Fatalf("read gallery timeline state after changed incremental sync: %v", err)
	}
	if thirdGeneration <= secondGeneration {
		t.Fatalf("generation after changed incremental sync = %d, want > %d", thirdGeneration, secondGeneration)
	}
}

func TestCatalogTimelineSearchSkipsExactTotalAfterFirstPage(t *testing.T) {
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
	capturedAt := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	assets := []ImmichMirrorAsset{
		{UpstreamAssetID: "asset-c", MediaType: "image", Filename: "c.jpg", CapturedAt: capturedAt},
		{UpstreamAssetID: "asset-a", MediaType: "image", Filename: "a.jpg", CapturedAt: capturedAt},
		{UpstreamAssetID: "asset-b", MediaType: "image", Filename: "b.jpg", CapturedAt: capturedAt},
		{UpstreamAssetID: "asset-e", MediaType: "image", Filename: "e.jpg", CapturedAt: capturedAt},
		{UpstreamAssetID: "asset-d", MediaType: "image", Filename: "d.jpg", CapturedAt: capturedAt},
	}
	if _, err := store.ReplaceFull(context.Background(), sourceKey, assets, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}
	store.setStandaloneGalleryReadiness(catalogGalleryReadiness{
		immichSourceKeys: []string{sourceKey},
		immichOnly:       true,
	})
	exactNormalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize exact timeline request: %v", err)
	}
	exact, err := store.SearchCatalogAssets(context.Background(), exactNormalized)
	if err != nil {
		t.Fatalf("SearchCatalogAssets(exact) error = %v", err)
	}
	if len(exact.Items) != 5 || exact.Total != 5 || exact.TotalAccuracy != TotalAccuracyExact {
		t.Fatalf("exact timeline page = %#v, want five items and exact total", exact)
	}
	search := func(pageIndex int) AssetSearchPage {
		t.Helper()
		normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
			Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
			Page:       AssetSearchPageRequest{Index: pageIndex, Size: 2},
		})
		if err != nil {
			t.Fatalf("normalizeAssetSearchRequest(page %d) error = %v", pageIndex, err)
		}
		page, err := store.SearchCatalogAssets(context.Background(), normalized)
		if err != nil {
			t.Fatalf("SearchCatalogAssets(page %d) error = %v", pageIndex, err)
		}
		return page
	}

	first := search(0)
	if len(first.Items) != 2 || first.Total != 5 || first.TotalAccuracy != TotalAccuracyExact ||
		first.NextPageIndex == nil || *first.NextPageIndex != 1 {
		t.Fatalf("first page = %#v, want exact total and next page", first)
	}
	if first.Items[0].ID != exact.Items[0].ID || first.Items[1].ID != exact.Items[1].ID {
		t.Fatalf("first page IDs = %q/%q, want exact order %q/%q",
			first.Items[0].ID, first.Items[1].ID, exact.Items[0].ID, exact.Items[1].ID)
	}
	second := search(1)
	if len(second.Items) != 2 || second.Total != 5 || second.TotalAccuracy != TotalAccuracyLowerBound ||
		second.NextPageIndex == nil || *second.NextPageIndex != 2 {
		t.Fatalf("count-free second page = %#v, want two items and next-page lower bound", second)
	}
	if second.Items[0].ID != exact.Items[2].ID || second.Items[1].ID != exact.Items[3].ID {
		t.Fatalf("count-free second page IDs = %q/%q, want exact order %q/%q",
			second.Items[0].ID, second.Items[1].ID, exact.Items[2].ID, exact.Items[3].ID)
	}
	last := search(2)
	if len(last.Items) != 1 || last.Items[0].ID != exact.Items[4].ID || last.Total != 5 ||
		last.TotalAccuracy != TotalAccuracyExact || last.NextPageIndex != nil {
		t.Fatalf("count-free last page = %#v, want exact terminal page", last)
	}
	beyond := search(3)
	if len(beyond.Items) != 0 || beyond.Total != 0 || beyond.TotalAccuracy != TotalAccuracyLowerBound ||
		beyond.Boundary == nil || beyond.Boundary.Kind != BoundaryPastEnd {
		t.Fatalf("count-free beyond page = %#v, want past-end lower bound", beyond)
	}

	from := capturedAt.Add(-time.Hour)
	filteredNormalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindTimeline,
			Filters: AssetSearchFilters{CapturedAt: &AssetSearchCapturedTime{
				From: &from,
			}},
		},
		Page: AssetSearchPageRequest{Index: 1, Size: 2},
	})
	if err != nil {
		t.Fatalf("normalize filtered timeline request: %v", err)
	}
	filtered, err := store.SearchCatalogAssets(context.Background(), filteredNormalized)
	if err != nil {
		t.Fatalf("SearchCatalogAssets(filtered) error = %v", err)
	}
	if filtered.Total != 5 || filtered.TotalAccuracy != TotalAccuracyExact {
		t.Fatalf("filtered timeline page = %#v, want exact total for date-jump compatibility", filtered)
	}
}

func TestCatalogTimelineQueryUsesExistingTimelineIndex(t *testing.T) {
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

	readiness := catalogGalleryReadiness{
		immichSourceKeys: []string{"1111111111111111"},
		immichOnly:       true,
	}
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 5000, Size: 60},
	})
	if err != nil {
		t.Fatalf("normalize timeline request: %v", err)
	}
	where, whereArgs := catalogSearchWhere(normalized, "c", readiness)
	sourceKeyColumn, upstreamAssetIDColumn, sourceArgs := catalogGallerySourceProjection(func(name string) string {
		return "c." + name
	}, readiness)
	query := `EXPLAIN QUERY PLAN SELECT ` + sourceKeyColumn + `, ` + upstreamAssetIDColumn + `,
			c.media_type, c.filename, c.captured_at, c.duration
		FROM catalog_canonical_assets c ` + where + `
		ORDER BY c.captured_at DESC, c.canonical_asset_id ASC
		LIMIT ? OFFSET ?`
	queryArgs := append(append(append([]any{}, sourceArgs...), whereArgs...), 60, 300000)
	rows, err := store.queryDB().QueryContext(context.Background(), query, queryArgs...)
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
	plan := strings.Join(details, "\n")
	if strings.Contains(plan, "USE TEMP B-TREE") {
		t.Fatalf("timeline query plan uses a temporary sort:\n%s", plan)
	}
	if !strings.Contains(plan, "idx_catalog_canonical_visible_timeline") {
		t.Fatalf("timeline query plan does not use visible timeline index:\n%s", plan)
	}
}

func TestSearchAssetsItemsUnmarshalSupportsStringNextPage(t *testing.T) {
	t.Parallel()

	var items searchAssetsItems
	payload := []byte(`{
		"items": [
			{
				"id": "asset-123",
				"type": "IMAGE",
				"originalFileName": "photo.jpg",
				"fileCreatedAt": "2026-04-07T09:57:15.053Z"
			}
		],
		"total": 1,
		"nextPage": "2"
	}`)

	if err := json.Unmarshal(payload, &items); err != nil {
		t.Fatalf("unmarshal search assets items: %v", err)
	}

	if items.Total != 1 {
		t.Fatalf("expected total 1, got %d", items.Total)
	}
	if items.NextPage == nil || *items.NextPage != 2 {
		t.Fatalf("expected nextPage 2, got %#v", items.NextPage)
	}
	if len(items.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items.Items))
	}
}

func TestAssetReturnsNormalizedImmichMetadata(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet || r.URL.Path != "/api/assets/asset-123" {
				t.Fatalf("request = %s %s, want GET /api/assets/asset-123", r.Method, r.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{
					"id":"asset-123",
					"type":"VIDEO",
					"originalFileName":"memory.mov",
					"fileCreatedAt":"2026-04-07T09:57:15.053Z",
					"duration":"0:00:10.000000"
				}`)),
			}, nil
		}),
	}

	asset, err := service.Asset("asset-123")
	if err != nil {
		t.Fatalf("Asset() error = %v", err)
	}
	if asset.ID != "asset-123" || asset.Type != "video" || asset.Filename != "memory.mov" {
		t.Fatalf("Asset() = %#v", asset)
	}
	if !asset.CapturedAt.Equal(time.Date(2026, time.April, 7, 9, 57, 15, 53_000_000, time.UTC)) {
		t.Fatalf("CapturedAt = %v", asset.CapturedAt)
	}
}

func TestAssetFromSourceUsesOpaqueIDDatasource(t *testing.T) {
	t.Parallel()

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("primary datasource was called for %s", r.URL.Path)
	}))
	t.Cleanup(primary.Close)
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/assets/asset-secondary" {
			t.Fatalf("secondary request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "secondary-key" {
			t.Fatalf("secondary x-api-key = %q, want secondary-key", r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"asset-secondary",
			"type":"IMAGE",
			"originalFileName":"secondary.jpg",
			"fileCreatedAt":"2026-07-18T12:00:00Z"
		}`)
	}))
	t.Cleanup(secondary.Close)

	service := NewService([]config.DatasourceConfig{
		{
			SourceKey:   "1111111111111111",
			Name:        "Primary Immich",
			Kind:        config.DatasourceKindImmich,
			URL:         primary.URL,
			AccessToken: "primary-key",
		},
		{
			SourceKey:   "2222222222222222",
			Name:        "Secondary Immich",
			Kind:        config.DatasourceKindImmich,
			URL:         secondary.URL,
			AccessToken: "secondary-key",
		},
	})
	asset, err := service.AssetFromSource("2222222222222222", "asset-secondary")
	if err != nil {
		t.Fatalf("AssetFromSource() error = %v", err)
	}
	if asset.SourceKey != "2222222222222222" || asset.ID != "asset-secondary" || asset.Filename != "secondary.jpg" {
		t.Fatalf("AssetFromSource() = %#v", asset)
	}
}

func TestImmichMirrorSyncUsesLatestLimitAndSearchesSQLite(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	service.client = immichMirrorTestClient(t)

	result, err := service.SyncMirror(context.Background(), MirrorSyncModeFull)
	if err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}
	if result.FetchedCount != 2 || result.ActiveCount != 2 || result.OutOfScopeCount != 0 {
		t.Fatalf("sync result counts = fetched:%d active:%d outOfScope:%d", result.FetchedCount, result.ActiveCount, result.OutOfScopeCount)
	}
	capabilities := service.SearchCapabilities()
	if fmt.Sprint(capabilities.QueryModes) != fmt.Sprint([]string{QueryModeAuto, QueryModeSemantic, QueryModeFilename}) {
		t.Fatalf("mirror query modes = %#v", capabilities.QueryModes)
	}
	if capabilities.Semantic == nil || capabilities.Semantic.Status != "missing" || capabilities.Semantic.MessageCode != semanticMessageIndexMissing {
		t.Fatalf("catalog semantic capabilities = %#v, want missing without an installed model", capabilities.Semantic)
	}

	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("timeline search: %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("timeline page total=%d items=%d, want 2", page.Total, len(page.Items))
	}
	if page.Items[0].ID != "asset-new" || page.Items[1].ID != "asset-beach" {
		t.Fatalf("timeline item order = %#v", page.Items)
	}

	filenamePage, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "beach",
				Mode: QueryModeFilename,
			},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("filename search: %v", err)
	}
	if filenamePage.Total != 1 || len(filenamePage.Items) != 1 || filenamePage.Items[0].ID != "asset-beach" {
		t.Fatalf("filename page = %#v", filenamePage)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	limitedService, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 1,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() after limit change error = %v", err)
	}
	defer limitedService.Close()
	limitedService.client = immichMirrorTestClient(t)
	limitedResult, err := limitedService.SyncMirror(context.Background(), MirrorSyncModeFull)
	if err != nil {
		t.Fatalf("limited SyncMirror() error = %v", err)
	}
	if limitedResult.ActiveCount != 1 || limitedResult.OutOfScopeCount != 1 || limitedResult.MissingCount != 0 {
		t.Fatalf("limited sync counts active=%d outOfScope=%d missing=%d", limitedResult.ActiveCount, limitedResult.OutOfScopeCount, limitedResult.MissingCount)
	}
}

func TestCatalogAssetsSearchesAcrossIndexedSources(t *testing.T) {
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

	sourceA := "1111111111111111"
	sourceB := "2222222222222222"
	if _, err := store.ReplaceFull(context.Background(), sourceA, []ImmichMirrorAsset{{
		UpstreamAssetID: "source-a-old",
		MediaType:       "image",
		Filename:        "old-family.jpg",
		CapturedAt:      time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
	}}, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull(sourceA) error = %v", err)
	}
	if _, err := store.ReplaceFull(context.Background(), sourceB, []ImmichMirrorAsset{{
		UpstreamAssetID: "source-b-new",
		MediaType:       "video",
		Filename:        "new-family.mov",
		CapturedAt:      time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC),
	}}, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull(sourceB) error = %v", err)
	}

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize timeline request: %v", err)
	}
	page, err := store.SearchCatalogAssets(context.Background(), normalized)
	if err != nil {
		t.Fatalf("SearchCatalogAssets(timeline) error = %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("timeline page total=%d items=%d, want 2", page.Total, len(page.Items))
	}
	if page.Items[0].SourceKey != sourceB || page.Items[0].ID != "source-b-new" ||
		page.Items[1].SourceKey != sourceA || page.Items[1].ID != "source-a-old" {
		t.Fatalf("timeline items = %#v, want newest across sources", page.Items)
	}

	filename, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "family",
				Mode: QueryModeFilename,
			},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize filename request: %v", err)
	}
	filenamePage, err := store.SearchCatalogAssets(context.Background(), filename)
	if err != nil {
		t.Fatalf("SearchCatalogAssets(filename) error = %v", err)
	}
	if filenamePage.Total != 2 || len(filenamePage.Items) != 2 {
		t.Fatalf("filename page total=%d items=%d, want 2", filenamePage.Total, len(filenamePage.Items))
	}
}

func TestReadOnlyCatalogQueriesDoNotWaitOnMainDBHandle(t *testing.T) {
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	sourceKey := "1111111111111111"
	sourceUpdatedAt := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)
	if _, err := store.ReplaceFull(context.Background(), sourceKey, []ImmichMirrorAsset{{
		UpstreamAssetID: "source-a",
		MediaType:       "image",
		Filename:        "family.jpg",
		CapturedAt:      time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		SourceUpdatedAt: &sourceUpdatedAt,
	}}, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}

	blockingTx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin blocking tx: %v", err)
	}
	defer blockingTx.Rollback()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	status, err := store.Status(ctx, sourceKey)
	if err != nil {
		t.Fatalf("Status() while main DB handle busy error = %v", err)
	}
	if status.ActiveCount != 1 {
		t.Fatalf("Status().ActiveCount = %d, want 1", status.ActiveCount)
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
		t.Fatalf("SearchCatalogAssets() while main DB handle busy error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("SearchCatalogAssets() total=%d items=%d, want 1", page.Total, len(page.Items))
	}

	latest, err := store.LatestSourceUpdatedAt(ctx, sourceKey)
	if err != nil {
		t.Fatalf("LatestSourceUpdatedAt() while main DB handle busy error = %v", err)
	}
	if latest == nil {
		t.Fatal("LatestSourceUpdatedAt() = nil, want timestamp")
	}
}

func TestImmichFullSyncAppliesMaterialDiffAndAdvancesGenerationOnce(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()
	nowText := formatCatalogTime(time.Now().UTC())
	if _, err := store.db.Exec(`INSERT INTO semantic_state (
		source_key, model_id, vector_space_id, status, embedding_dim,
		completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
		updated_at
	) VALUES (?, 'test-model', 'test-model/d4', 'pending', 4, 0, 0, 0, -1, ?)`, sourceKey, nowText); err != nil {
		t.Fatalf("insert semantic state: %v", err)
	}
	assets := []ImmichMirrorAsset{
		{UpstreamAssetID: "asset-a", MediaType: "image", Filename: "a.jpg", CapturedAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)},
		{UpstreamAssetID: "asset-b", MediaType: "image", Filename: "b.jpg", CapturedAt: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)},
		{UpstreamAssetID: "asset-c", MediaType: "video", Filename: "c.mov", CapturedAt: time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)},
	}
	readGeneration := func() int64 {
		t.Helper()
		var generation int64
		if err := store.db.QueryRow(`SELECT asset_generation FROM semantic_state WHERE source_key = ? AND model_id = 'test-model'`, sourceKey).Scan(&generation); err != nil {
			t.Fatalf("read asset generation: %v", err)
		}
		return generation
	}
	if _, err := store.ReplaceFull(context.Background(), sourceKey, assets, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull(initial) error = %v", err)
	}
	if got := readGeneration(); got != 1 {
		t.Fatalf("initial asset generation = %d, want one transaction advance", got)
	}
	if _, err := store.ReplaceFull(context.Background(), sourceKey, assets, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull(identical) error = %v", err)
	}
	if got := readGeneration(); got != 1 {
		t.Fatalf("identical asset generation = %d, want unchanged", got)
	}
	assets[0].Filename = "a-renamed.jpg"
	assets = assets[:2]
	if _, err := store.ReplaceFull(context.Background(), sourceKey, assets, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull(changed) error = %v", err)
	}
	if got := readGeneration(); got != 2 {
		t.Fatalf("changed asset generation = %d, want one additional transaction advance", got)
	}
	status, err := store.CatalogDeduplicationStatus(context.Background())
	if err != nil {
		t.Fatalf("CatalogDeduplicationStatus() error = %v", err)
	}
	if status.SourceRows != 3 || status.CanonicalAssets != 3 || status.ActiveAssets != 2 || status.NeedsRepair {
		t.Fatalf("catalog status = %#v, want two active assets and one hidden source without repair", status)
	}
}

func TestCatalogCanonicalAssetsCollapseExactDuplicatesAcrossDatasources(t *testing.T) {
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
	immichSource := "1111111111111111"
	localSource := "2222222222222222"
	sha1Hex := strings.Repeat("a", 40)
	capturedAt := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	if _, err := store.ReplaceFull(ctx, immichSource, []ImmichMirrorAsset{{
		UpstreamAssetID:  "immich-family",
		MediaType:        "image",
		Filename:         "immich-family.jpg",
		CapturedAt:       capturedAt,
		ContentSHA1Hex:   sha1Hex,
		ContentSizeBytes: 1234,
	}}, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}
	nowText := formatCatalogTime(time.Now().UTC())
	if _, err := store.db.ExecContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename,
			captured_at, duration, visibility_status, source_updated_at, is_favorite,
			content_sha1_hex, content_size_bytes, place_label, description, first_seen_at,
			updated_at
		) VALUES (?, 'local_filesystem', ?, 'image', ?, ?, NULL, 'active', ?, 0, ?, ?, NULL, NULL, ?, ?)`,
		localSource,
		"local-family",
		"local-family.jpg",
		formatCatalogTime(capturedAt),
		nowText,
		sha1Hex,
		int64(1234),
		nowText,
		nowText,
	); err != nil {
		t.Fatalf("insert local catalog source row: %v", err)
	}
	if _, err := store.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}
	store.setStandaloneGalleryReadiness(catalogGalleryReadiness{
		localSourceKeys:               []string{localSource},
		localImmichFallbackSourceKeys: []string{localSource},
		immichSourceKeys:              []string{immichSource},
	})

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
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("timeline page total=%d items=%d, want one canonical asset", page.Total, len(page.Items))
	}
	if got := page.Items[0]; got.SourceKey != localSource || got.ID != "local-family" {
		t.Fatalf("canonical primary = %s/%s, want local source", got.SourceKey, got.ID)
	}

	filenameSearch, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "immich-family",
				Mode: QueryModeFilename,
			},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize filename search: %v", err)
	}
	filenamePage, err := store.SearchCatalogAssets(ctx, filenameSearch)
	if err != nil {
		t.Fatalf("SearchCatalogAssets(filename) error = %v", err)
	}
	if filenamePage.Total != 1 || len(filenamePage.Items) != 1 {
		t.Fatalf("filename page total=%d items=%d, want one canonical duplicate", filenamePage.Total, len(filenamePage.Items))
	}
	if got := filenamePage.Items[0]; got.SourceKey != localSource || got.ID != "local-family" {
		t.Fatalf("filename search canonical primary = %s/%s, want local source", got.SourceKey, got.ID)
	}

	semanticItems, err := store.canonicalAssetsForScoredSources(ctx, []semanticScoredAsset{
		{Asset: semanticAsset{SourceKey: immichSource, ID: "immich-family"}, Similarity: 0.9},
		{Asset: semanticAsset{SourceKey: localSource, ID: "local-family"}, Similarity: 0.8},
	}, true)
	if err != nil {
		t.Fatalf("canonicalAssetsForScoredSources() error = %v", err)
	}
	if len(semanticItems) != 1 {
		t.Fatalf("semantic canonical items = %#v, want one deduplicated result", semanticItems)
	}
	if got := semanticItems[0]; got.SourceKey != localSource || got.ID != "local-family" || got.SemanticScore == nil || *got.SemanticScore != float32(0.9) {
		t.Fatalf("semantic canonical primary = %#v, want local primary with best duplicate score", got)
	}

	status, err := store.CatalogDeduplicationStatus(ctx)
	if err != nil {
		t.Fatalf("CatalogDeduplicationStatus() error = %v", err)
	}
	if status.SourceRows != 2 || status.CanonicalAssets != 1 || status.DuplicateSourceRows != 1 || status.NeedsRepair {
		t.Fatalf("dedup status = %#v, want one duplicate source and healthy links", status)
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM catalog_canonical_assets`); err != nil {
		t.Fatalf("delete canonical assets: %v", err)
	}
	broken, err := store.CatalogDeduplicationStatus(ctx)
	if err != nil {
		t.Fatalf("broken CatalogDeduplicationStatus() error = %v", err)
	}
	if !broken.NeedsRepair || broken.UnlinkedSourceRows != 2 {
		t.Fatalf("broken dedup status = %#v, want repair needed for two source rows", broken)
	}
	repaired, err := store.RebuildCatalogCanonicalAssets(ctx)
	if err != nil {
		t.Fatalf("repair RebuildCatalogCanonicalAssets() error = %v", err)
	}
	if repaired.NeedsRepair || repaired.CanonicalAssets != 1 {
		t.Fatalf("repaired dedup status = %#v, want healthy one canonical asset", repaired)
	}
}

func TestCanonicalSemanticAssetsApplyLocalGalleryReadiness(t *testing.T) {
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
	const assetID = "local-family"
	sha1Hex := strings.Repeat("a", 40)
	nowText := formatCatalogTime(time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC))
	if _, err := store.db.ExecContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename,
			captured_at, duration, visibility_status, source_updated_at, is_favorite,
			content_sha1_hex, content_size_bytes, place_label, description, first_seen_at,
			updated_at
		) VALUES (?, 'local_filesystem', ?, 'image', 'family.jpg', ?, NULL, 'active', ?, 0, ?, 1234, NULL, NULL, ?, ?)`,
		sourceKey, assetID, nowText, nowText, sha1Hex, nowText, nowText); err != nil {
		t.Fatalf("insert local catalog asset: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO local_assets (
			source_key, asset_id, sha1_hex, content_size_bytes, media_type, filename,
			captured_at, captured_at_source, visibility_status, thumbnail_status,
			first_seen_at, updated_at
		) VALUES (?, ?, ?, 1234, 'image', 'family.jpg', ?, 'filesystem', 'active', 'pending', ?, ?)`,
		sourceKey, assetID, sha1Hex, nowText, nowText, nowText); err != nil {
		t.Fatalf("insert local asset: %v", err)
	}
	if _, err := store.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}

	scored := []semanticScoredAsset{{
		Asset:      semanticAsset{SourceKey: sourceKey, ID: assetID},
		Similarity: 0.9,
	}}
	items, err := store.canonicalAssetsForScoredSources(ctx, scored, true)
	if err != nil {
		t.Fatalf("canonicalAssetsForScoredSources(pending) error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("pending semantic canonical items = %#v, want hidden asset", items)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE local_assets
		SET thumbnail_status = 'ready'
		WHERE source_key = ? AND asset_id = ?`, sourceKey, assetID); err != nil {
		t.Fatalf("mark local thumbnail ready: %v", err)
	}
	for _, kind := range []string{localRenditionKindPreview, localRenditionKindDetailPreview} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO local_renditions (
				source_key, asset_id, kind, status, relative_path, source_sha1_hex
			) VALUES (?, ?, ?, 'ready', ?, ?)`,
			sourceKey, assetID, kind, kind+".jpg", sha1Hex); err != nil {
			t.Fatalf("insert %s rendition: %v", kind, err)
		}
	}
	items, err = store.canonicalAssetsForScoredSources(ctx, scored, true)
	if err != nil {
		t.Fatalf("canonicalAssetsForScoredSources(ready) error = %v", err)
	}
	if len(items) != 1 || items[0].SourceKey != sourceKey || items[0].ID != assetID {
		t.Fatalf("ready semantic canonical items = %#v, want local asset", items)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE local_renditions
		SET status = 'failed'
		WHERE source_key = ? AND asset_id = ? AND kind = ?`, sourceKey, assetID, localRenditionKindDetailPreview); err != nil {
		t.Fatalf("mark detail rendition failed: %v", err)
	}
	items, err = store.canonicalAssetsForScoredSources(ctx, scored, true)
	if err != nil {
		t.Fatalf("canonicalAssetsForScoredSources(failed) error = %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("failed semantic canonical items = %#v, want hidden asset", items)
	}
}

func TestCatalogSemanticSearchFiltersCanonicalPrimaryBeforePagination(t *testing.T) {
	t.Parallel()

	const (
		immichSource = "1111111111111111"
		localSource  = "2222222222222222"
	)
	service, err := NewServiceWithOptions([]config.DatasourceConfig{
		{
			SourceKey:   immichSource,
			Name:        "Home Immich",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://immich.test",
			AccessToken: "test-key",
		},
		{
			SourceKey: localSource,
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
		},
	}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: t.TempDir(),
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	ctx := context.Background()
	profile := testImageSemanticProfile{}
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	nowText := formatCatalogTime(now)
	insertAsset := func(sourceKey string, datasourceKind string, assetID string, capturedAt time.Time, sha1Hex string) {
		t.Helper()
		if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO catalog_assets (
				source_key, datasource_kind, upstream_asset_id, media_type, filename,
				captured_at, duration, visibility_status, source_updated_at, is_favorite,
				content_sha1_hex, content_size_bytes, place_label, description, first_seen_at,
				updated_at
			) VALUES (?, ?, ?, 'image', ?, ?, NULL, 'active', ?, 0, ?, 1234, NULL, NULL, ?, ?)`,
			sourceKey,
			datasourceKind,
			assetID,
			assetID+".jpg",
			formatCatalogTime(capturedAt),
			nowText,
			sha1Hex,
			nowText,
			nowText,
		); err != nil {
			t.Fatalf("insert catalog asset %q: %v", assetID, err)
		}
	}
	blockedCandidates := []struct {
		id         string
		canonical  string
		hash       string
		similarity float32
	}{
		{id: "duplicate-in-range-1", canonical: "canonical-out-of-range-1", hash: strings.Repeat("a", 40), similarity: 1.00},
		{id: "duplicate-in-range-2", canonical: "canonical-out-of-range-2", hash: strings.Repeat("b", 40), similarity: 0.99},
		{id: "duplicate-in-range-3", canonical: "canonical-out-of-range-3", hash: strings.Repeat("c", 40), similarity: 0.98},
		{id: "duplicate-in-range-4", canonical: "canonical-out-of-range-4", hash: strings.Repeat("d", 40), similarity: 0.97},
		{id: "duplicate-in-range-5", canonical: "canonical-out-of-range-5", hash: strings.Repeat("e", 40), similarity: 0.96},
	}
	for _, candidate := range blockedCandidates {
		insertAsset(immichSource, "immich", candidate.id, time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC), candidate.hash)
		insertAsset(localSource, "local_filesystem", candidate.canonical, time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC), candidate.hash)
	}
	insertAsset(immichSource, "immich", "keep-in-range", time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC), strings.Repeat("f", 40))
	if _, err := service.catalog.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}

	semanticCandidates := []struct {
		id     string
		vector []float32
	}{}
	for index, candidate := range blockedCandidates {
		semanticCandidates = append(semanticCandidates, struct {
			id     string
			vector []float32
		}{
			id:     candidate.id,
			vector: []float32{float32(index+1) * 0.01, candidate.similarity, 0, 0},
		})
	}
	semanticCandidates = append(semanticCandidates, struct {
		id     string
		vector []float32
	}{id: "keep-in-range", vector: []float32{0, 0.8, 0.6, 0}})
	for _, candidate := range semanticCandidates {
		insertSemanticVectorForTest(t,
			service.catalog,
			ctx,
			immichSource,
			candidate.id,
			profile.ModelID(),
			profile.VectorSpaceID(),
			profile.EmbeddingDim(),
			candidate.vector,
			"test",
			"ready",
			nil,
			nowText,
			nowText,
		)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			built_at, last_error, updated_at
		) VALUES (?, ?, ?, 'ready', ?, ?, ?, 0, 0, ?, NULL, ?)`,
		immichSource,
		profile.ModelID(),
		profile.VectorSpaceID(),
		profile.EmbeddingDim(),
		len(semanticCandidates),
		len(semanticCandidates),
		nowText,
		nowText,
	); err != nil {
		t.Fatalf("insert semantic state: %v", err)
	}
	writeSemanticBinaryIndexFromReadyVectorsForTest(t, service.catalog, ctx, immichSource, profile)

	from := time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "beach",
				Mode: QueryModeSemantic,
			},
			Filters: AssetSearchFilters{CapturedAt: &AssetSearchCapturedTime{From: &from, To: &to}},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 1},
	})
	if err != nil {
		t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
	}
	page, err := service.searchCatalogSemanticAssets(ctx, normalized, profile, AssetSearchOptions{})
	if err != nil {
		t.Fatalf("searchCatalogSemanticAssets() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != "keep-in-range" {
		t.Fatalf("semantic filtered page = %#v, want in-range canonical item after dropped first result", page)
	}
}

func TestCatalogSemanticSearchContinuesPastLargeCanonicalFilteredPrefix(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	ctx := context.Background()
	profile := testImageSemanticProfile{}
	builtAt := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	nowText := formatCatalogTime(builtAt)
	duplicateCapturedAt := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	uniqueCapturedAt := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	assets := make([]semanticAsset, 0, 301)
	insertCatalogAsset := func(assetID string, capturedAt time.Time, contentHash string) {
		t.Helper()
		if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO catalog_assets (
				source_key, datasource_kind, upstream_asset_id, media_type, filename,
				captured_at, duration, visibility_status, source_updated_at, is_favorite,
				content_sha1_hex, content_size_bytes, place_label, description, first_seen_at,
				updated_at
			) VALUES (?, 'immich', ?, 'image', ?, ?, NULL, 'active', ?, 0, ?, 1234, NULL, NULL, ?, ?)`,
			sourceKey,
			assetID,
			assetID+".jpg",
			formatCatalogTime(capturedAt),
			nowText,
			contentHash,
			nowText,
			nowText,
		); err != nil {
			t.Fatalf("insert catalog asset %q: %v", assetID, err)
		}
	}
	for index := 0; index < 300; index++ {
		assetID := fmt.Sprintf("duplicate-%03d", index)
		insertCatalogAsset(assetID, duplicateCapturedAt, strings.Repeat("a", 40))
		assets = append(assets, semanticAsset{
			SourceKey:  sourceKey,
			ID:         assetID,
			MediaType:  "image",
			Filename:   assetID + ".jpg",
			CapturedAt: duplicateCapturedAt,
			Vector:     []float32{0, 1, 0, 0},
		})
	}
	insertCatalogAsset("unique-in-range", uniqueCapturedAt, strings.Repeat("b", 40))
	assets = append(assets, semanticAsset{
		SourceKey:  sourceKey,
		ID:         "unique-in-range",
		MediaType:  "image",
		Filename:   "unique-in-range.jpg",
		CapturedAt: uniqueCapturedAt,
		Vector:     []float32{0, 0.5, 0.5, 0},
	})
	if _, err := service.catalog.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}

	seedAndWriteSemanticBinaryIndexForTest(t, service.catalog, ctx, sourceKey, profile, assets, builtAt, 0)
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			built_at, last_error, updated_at
		) VALUES (?, ?, ?, 'ready', ?, ?, ?, 0, 0, ?, NULL, ?)`,
		sourceKey,
		profile.ModelID(),
		profile.VectorSpaceID(),
		profile.EmbeddingDim(),
		len(assets),
		len(assets),
		nowText,
		nowText,
	); err != nil {
		t.Fatalf("insert semantic state: %v", err)
	}
	from := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind:  CollectionKindSearch,
			Query: &AssetSearchQuery{Text: "beach", Mode: QueryModeSemantic},
			Filters: AssetSearchFilters{CapturedAt: &AssetSearchCapturedTime{
				From: &from,
				To:   &to,
			}},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 1},
	})
	if err != nil {
		t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
	}
	page, err := service.searchCatalogSemanticAssets(ctx, normalized, profile, AssetSearchOptions{})
	if err != nil {
		t.Fatalf("searchCatalogSemanticAssets() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != "unique-in-range" || page.NextPageIndex != nil {
		t.Fatalf("semantic continued filtered page = %#v, want the unique result beyond 300 duplicate candidates", page)
	}
}

func TestCatalogSemanticSearchRanksBoundedCandidateSnapshotAcrossChunks(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	assets := make([]semanticAsset, 0, semanticHNSWEfSearch*2)
	scored := make([]semanticScoredAsset, 0, semanticHNSWEfSearch*2)
	for index := 0; index < semanticHNSWEfSearch*2; index++ {
		assetID := fmt.Sprintf("asset-%03d", index)
		asset := semanticAsset{
			SourceKey:  sourceKey,
			ID:         assetID,
			MediaType:  "image",
			Filename:   assetID + ".jpg",
			CapturedAt: builtAt.Add(time.Duration(index) * time.Second),
			Vector:     []float32{0, 0, 0, 0},
		}
		assets = append(assets, asset)
		similarity := 1 - float32(index)/1000
		if index == semanticHNSWEfSearch*2-1 {
			similarity = 2
		}
		scored = append(scored, semanticScoredAsset{Asset: asset, Similarity: similarity})
	}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, assets, builtAt)

	search := func(pageIndex int, pageSize int, filters AssetSearchFilters) (catalogSemanticResolvedPage, catalogSemanticTraversalStats, *scriptedCatalogSemanticTraversal) {
		t.Helper()
		normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
			Collection: AssetCollectionRequest{
				Kind:    CollectionKindSearch,
				Query:   &AssetSearchQuery{Text: "beach", Mode: QueryModeSemantic},
				Filters: filters,
			},
			Page: AssetSearchPageRequest{Index: pageIndex, Size: pageSize},
		})
		if err != nil {
			t.Fatalf("normalizeAssetSearchRequest(page %d size %d) error = %v", pageIndex, pageSize, err)
		}
		traversal := &scriptedCatalogSemanticTraversal{candidates: append([]semanticScoredAsset(nil), scored...)}
		page, stats, err := service.resolveCatalogSemanticTraversalPage(
			context.Background(),
			normalized,
			[]catalogSemanticSourceTraversal{{traversal: traversal}},
			nil,
			false,
		)
		if err != nil {
			t.Fatalf("resolveCatalogSemanticTraversalPage(page %d size %d) error = %v", pageIndex, pageSize, err)
		}
		return page, stats, traversal
	}

	first, firstStats, firstTraversal := search(0, 200, AssetSearchFilters{})
	if firstStats.CandidateVisits != semanticHNSWEfSearch*2 || firstStats.Rounds != 2 ||
		len(firstTraversal.calls) != 2 || firstTraversal.calls[0] != semanticHNSWEfSearch || firstTraversal.calls[1] != semanticHNSWEfSearch {
		t.Fatalf("first-page traversal stats=%#v calls=%v, want the complete bounded candidate snapshot", firstStats, firstTraversal.calls)
	}
	if len(first.Items) != 200 || first.Items[0].ID != "asset-511" || first.Items[1].ID != "asset-000" || first.Items[199].ID != "asset-198" || !first.HasMore {
		t.Fatalf("first globally ranked semantic page = %#v", first)
	}

	second, secondStats, secondTraversal := search(1, 200, AssetSearchFilters{})
	if secondStats.CandidateVisits != semanticHNSWEfSearch*2 || secondStats.Rounds != 2 ||
		len(secondTraversal.calls) != 2 || secondTraversal.calls[0] != semanticHNSWEfSearch || secondTraversal.calls[1] != semanticHNSWEfSearch {
		t.Fatalf("second-page traversal stats=%#v calls=%v, want the same complete candidate snapshot", secondStats, secondTraversal.calls)
	}
	if len(second.Items) != 200 || second.Items[0].ID != "asset-199" || second.Items[199].ID != "asset-398" {
		t.Fatalf("second globally ranked semantic page = %#v", second.Items)
	}
	firstIDs := make(map[string]struct{}, len(first.Items))
	for _, item := range first.Items {
		firstIDs[item.ID] = struct{}{}
	}
	for _, item := range second.Items {
		if _, duplicate := firstIDs[item.ID]; duplicate {
			t.Fatalf("globally ranked semantic pages overlap at %q", item.ID)
		}
	}

	from := builtAt.Add(510 * time.Second)
	to := builtAt.Add(512 * time.Second)
	filtered, filteredStats, _ := search(0, 1, AssetSearchFilters{CapturedAt: &AssetSearchCapturedTime{From: &from, To: &to}})
	if filteredStats.CandidateVisits != semanticHNSWEfSearch*2 || filteredStats.Rounds != 2 {
		t.Fatalf("filtered traversal stats = %#v, want expansion through two candidate rounds", filteredStats)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].ID != "asset-511" || !filtered.HasMore {
		t.Fatalf("filtered globally ranked semantic page = %#v", filtered)
	}
}

func TestCatalogSemanticAutoSearchPromotesMetadataBeyondInitialTraversalChunk(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	assets := make([]semanticAsset, 0, semanticHNSWEfSearch+1)
	scored := make([]semanticScoredAsset, 0, semanticHNSWEfSearch+1)
	for index := 0; index <= semanticHNSWEfSearch; index++ {
		assetID := fmt.Sprintf("asset-%03d", index)
		filename := assetID + ".jpg"
		if index == semanticHNSWEfSearch {
			assetID = "metadata-match"
			filename = "Kyoto exact match.jpg"
		}
		asset := semanticAsset{
			SourceKey:  sourceKey,
			ID:         assetID,
			MediaType:  "image",
			Filename:   filename,
			CapturedAt: builtAt.Add(time.Duration(index) * time.Second),
			Vector:     []float32{0, 1, 0, 0},
		}
		assets = append(assets, asset)
		scored = append(scored, semanticScoredAsset{
			Asset:      asset,
			Similarity: 1 - float32(index)/1000,
		})
	}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, assets, builtAt)
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind:  CollectionKindSearch,
			Query: &AssetSearchQuery{Text: "Kyoto", Mode: QueryModeAuto},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 1},
	})
	if err != nil {
		t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
	}
	metadataSession, ok, err := service.catalog.openSemanticIndexTraversal(
		context.Background(),
		sourceKey,
		testImageSemanticProfile{},
		[]float32{0, 1, 0, 0},
	)
	if err != nil || !ok || metadataSession == nil || metadataSession.reader == nil {
		t.Fatalf("open metadata semantic traversal = session:%#v ok:%t error:%v", metadataSession, ok, err)
	}
	defer metadataSession.Close()
	metadataCandidates, err := service.catalogSemanticMetadataCandidates(
		context.Background(),
		normalized,
		[]float32{0, 1, 0, 0},
		[]catalogSemanticSourceTraversal{{
			traversal: metadataSession,
			sourceKey: sourceKey,
			reader:    metadataSession.reader,
		}},
	)
	if err != nil {
		t.Fatalf("catalogSemanticMetadataCandidates() error = %v", err)
	}
	traversal := &scriptedCatalogSemanticTraversal{candidates: scored}
	page, stats, err := service.resolveCatalogSemanticTraversalPage(
		context.Background(),
		normalized,
		[]catalogSemanticSourceTraversal{{traversal: traversal}},
		metadataCandidates,
		false,
	)
	if err != nil {
		t.Fatalf("resolveCatalogSemanticTraversalPage() error = %v", err)
	}
	if len(metadataCandidates) != 1 || stats.MetadataCandidates != 1 {
		t.Fatalf("metadata candidates=%d stats=%#v, want one independent match", len(metadataCandidates), stats)
	}
	if stats.CandidateVisits != semanticHNSWEfSearch+1 || stats.Rounds != 2 ||
		len(traversal.calls) != 2 || traversal.calls[0] != semanticHNSWEfSearch || traversal.calls[1] != semanticHNSWEfSearch {
		t.Fatalf("auto traversal stats=%#v calls=%v, want the complete bounded candidate snapshot", stats, traversal.calls)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "metadata-match" || !page.HasMore {
		t.Fatalf("auto metadata page=%#v, want the match beyond the initial semantic chunk", page)
	}
}

type scriptedCatalogSemanticTraversal struct {
	candidates []semanticScoredAsset
	offset     int
	calls      []int
}

func (s *scriptedCatalogSemanticTraversal) Advance(ctx context.Context, additionalVisits int) ([]semanticScoredAsset, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.calls = append(s.calls, additionalVisits)
	end := min(s.offset+additionalVisits, len(s.candidates))
	batch := append([]semanticScoredAsset(nil), s.candidates[s.offset:end]...)
	s.offset = end
	return batch, nil
}

func (s *scriptedCatalogSemanticTraversal) Close() error {
	return nil
}

func (s *scriptedCatalogSemanticTraversal) Done() bool {
	return s == nil || s.offset >= len(s.candidates)
}

func TestCatalogSemanticResultPageUsesGalleryProjectionIncrementally(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	assets := make([]semanticAsset, 0, 250)
	scored := make([]semanticScoredAsset, 0, 250)
	for index := 0; index < 250; index++ {
		assetID := fmt.Sprintf("asset-%03d", index)
		asset := semanticAsset{
			SourceKey:  sourceKey,
			ID:         assetID,
			MediaType:  "image",
			Filename:   assetID + ".jpg",
			CapturedAt: builtAt.Add(time.Duration(index) * time.Second),
			Vector:     []float32{0, 1, 0, 0},
		}
		assets = append(assets, asset)
		scored = append(scored, semanticScoredAsset{
			Asset:      asset,
			Similarity: 1 - float32(index)/1000,
		})
	}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, assets, builtAt)

	resolve := func(pageIndex int) catalogSemanticResolvedPage {
		t.Helper()
		normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
			Collection: AssetCollectionRequest{
				Kind:  CollectionKindSearch,
				Query: &AssetSearchQuery{Text: "beach", Mode: QueryModeSemantic},
			},
			Page: AssetSearchPageRequest{Index: pageIndex, Size: 60},
		})
		if err != nil {
			t.Fatalf("normalizeAssetSearchRequest(page %d) error = %v", pageIndex, err)
		}
		page, err := service.resolveCatalogSemanticResultPage(context.Background(), normalized, scored, false)
		if err != nil {
			t.Fatalf("resolveCatalogSemanticResultPage(page %d) error = %v", pageIndex, err)
		}
		return page
	}

	first := resolve(0)
	if first.Projection != catalogSemanticResultProjectionGallery || first.CandidateCount != 64 ||
		first.Total != 61 || !first.HasMore || len(first.Items) != 60 ||
		first.Items[0].ID != "asset-000" || first.Items[59].ID != "asset-059" {
		t.Fatalf("first incremental semantic page = %#v", first)
	}
	for _, item := range first.Items {
		if item.SemanticScore != nil {
			t.Fatalf("first incremental semantic score = %v, want omitted", *item.SemanticScore)
		}
	}

	second := resolve(1)
	if second.Projection != catalogSemanticResultProjectionGallery || second.CandidateCount != 128 ||
		second.Total != 121 || !second.HasMore || len(second.Items) != 60 ||
		second.Items[0].ID != "asset-060" || second.Items[59].ID != "asset-119" {
		t.Fatalf("second incremental semantic page = %#v", second)
	}

	last := resolve(4)
	if last.Projection != catalogSemanticResultProjectionGallery || last.CandidateCount != 250 ||
		last.Total != 250 || last.HasMore || len(last.Items) != 10 ||
		last.Items[0].ID != "asset-240" || last.Items[9].ID != "asset-249" {
		t.Fatalf("last incremental semantic page = %#v", last)
	}
}

func TestCatalogSemanticResultPageFallsBackFromStaleGalleryGeneration(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	assets := make([]semanticAsset, 0, 80)
	scored := make([]semanticScoredAsset, 0, 80)
	for index := 0; index < 80; index++ {
		assetID := fmt.Sprintf("asset-%03d", index)
		asset := semanticAsset{
			SourceKey:  sourceKey,
			ID:         assetID,
			MediaType:  "image",
			Filename:   assetID + ".jpg",
			CapturedAt: builtAt.Add(time.Duration(index) * time.Second),
			Vector:     []float32{0, 1, 0, 0},
		}
		assets = append(assets, asset)
		scored = append(scored, semanticScoredAsset{Asset: asset, Similarity: 1 - float32(index)/1000})
	}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, assets, builtAt)
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE catalog_canonical_state
		SET generation = generation + 1
		WHERE singleton_id = ?`, catalogGalleryTimelineStateID); err != nil {
		t.Fatalf("make Gallery generation stale: %v", err)
	}
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind:  CollectionKindSearch,
			Query: &AssetSearchQuery{Text: "beach", Mode: QueryModeSemantic},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 60},
	})
	if err != nil {
		t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
	}
	page, err := service.resolveCatalogSemanticResultPage(context.Background(), normalized, scored, false)
	if err != nil {
		t.Fatalf("resolveCatalogSemanticResultPage() error = %v", err)
	}
	if page.Projection != catalogSemanticResultProjectionCanonical || page.CandidateCount != 80 ||
		page.Total != 61 || !page.HasMore || len(page.Items) != 60 || page.Items[0].ID != "asset-000" {
		t.Fatalf("stale-generation semantic fallback page = %#v", page)
	}
}

func TestCatalogSemanticResultPageHandlesMaximumNormalizedPage(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	asset := semanticAsset{
		SourceKey:  sourceKey,
		ID:         "asset-001",
		MediaType:  "image",
		Filename:   "asset-001.jpg",
		CapturedAt: builtAt,
		Vector:     []float32{0, 1, 0, 0},
	}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, []semanticAsset{asset}, builtAt)
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind:  CollectionKindSearch,
			Query: &AssetSearchQuery{Text: "beach", Mode: QueryModeSemantic},
		},
		Page: AssetSearchPageRequest{Index: math.MaxInt - 1, Size: 1},
	})
	if err != nil {
		t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
	}
	page, err := service.resolveCatalogSemanticResultPage(context.Background(), normalized, []semanticScoredAsset{{
		Asset:      asset,
		Similarity: 1,
	}}, false)
	if err != nil {
		t.Fatalf("resolveCatalogSemanticResultPage() error = %v", err)
	}
	if page.Total != 1 || page.HasMore || len(page.Items) != 0 || page.CandidateCount != 1 {
		t.Fatalf("maximum normalized semantic page = %#v, want past-end result without overflow", page)
	}
}

func TestCatalogSemanticSearchPreservesVectorBackedDiversification(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	assets := []semanticAsset{
		{
			SourceKey:  sourceKey,
			ID:         "best",
			MediaType:  "image",
			Filename:   "best.jpg",
			CapturedAt: builtAt,
			Vector:     []float32{0, 1, 0, 0},
		},
		{
			SourceKey:  sourceKey,
			ID:         "near-copy",
			MediaType:  "image",
			Filename:   "near-copy.jpg",
			CapturedAt: builtAt.Add(time.Second),
			Vector:     []float32{0, 0.999, 0.01, 0},
		},
		{
			SourceKey:  sourceKey,
			ID:         "different",
			MediaType:  "image",
			Filename:   "different.jpg",
			CapturedAt: builtAt.Add(2 * time.Second),
			Vector:     []float32{0, 0.8, 0.6, 0},
		},
	}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, assets, builtAt)
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind:  CollectionKindSearch,
			Query: &AssetSearchQuery{Text: "beach", Mode: QueryModeSemantic},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 2},
	})
	if err != nil {
		t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
	}
	page, err := service.searchCatalogSemanticAssets(context.Background(), normalized, testImageSemanticProfile{}, AssetSearchOptions{})
	if err != nil {
		t.Fatalf("searchCatalogSemanticAssets() error = %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].ID != "best" || page.Items[1].ID != "different" {
		t.Fatalf("diversified semantic page = %#v, want best then different before near-copy", page.Items)
	}
}

func TestCatalogSemanticAutoSearchPromotesMetadataMatches(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	assets := []semanticAsset{
		{
			SourceKey:  sourceKey,
			ID:         "semantic-best",
			MediaType:  "image",
			Filename:   "plain.jpg",
			CapturedAt: builtAt,
			Vector:     []float32{1, 0, 0, 0},
		},
		{
			SourceKey:  sourceKey,
			ID:         "metadata-match",
			MediaType:  "image",
			Filename:   "Kyoto evening.jpg",
			CapturedAt: builtAt.Add(time.Second),
			Vector:     []float32{0.8, 0.6, 0, 0},
		},
	}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, assets, builtAt)
	for index := 0; index < 2; index++ {
		capturedAt := builtAt.Add(time.Duration(index+1) * time.Hour)
		if _, err := service.catalog.db.Exec(`INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename,
			captured_at, duration, visibility_status, source_updated_at, is_favorite,
			content_sha1_hex, content_size_bytes, place_label, description, first_seen_at, updated_at
		) VALUES (?, 'immich', ?, 'image', ?, ?, NULL, 'active', ?, 0, ?, ?, NULL, NULL, ?, ?)`,
			sourceKey,
			fmt.Sprintf("external-metadata-%d", index),
			fmt.Sprintf("Kyoto newer %d.jpg", index),
			formatCatalogTime(capturedAt),
			formatCatalogTime(capturedAt),
			fmt.Sprintf("%040x", 100+index),
			int64(20_000+index),
			formatCatalogTime(capturedAt),
			formatCatalogTime(capturedAt),
		); err != nil {
			t.Fatalf("insert external metadata match %d: %v", index, err)
		}
	}

	search := func(mode string) AssetSearchPage {
		t.Helper()
		normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
			Collection: AssetCollectionRequest{
				Kind:  CollectionKindSearch,
				Query: &AssetSearchQuery{Text: "Kyoto", Mode: mode},
			},
			Page: AssetSearchPageRequest{Index: 0, Size: 2},
		})
		if err != nil {
			t.Fatalf("normalizeAssetSearchRequest(%s) error = %v", mode, err)
		}
		page, err := service.searchCatalogSemanticAssets(context.Background(), normalized, testImageSemanticProfile{}, AssetSearchOptions{})
		if err != nil {
			t.Fatalf("searchCatalogSemanticAssets(%s) error = %v", mode, err)
		}
		return page
	}

	semantic := search(QueryModeSemantic)
	if len(semantic.Items) != 2 || semantic.Items[0].ID != "semantic-best" {
		t.Fatalf("semantic page = %#v, want vector order", semantic.Items)
	}
	auto := search(QueryModeAuto)
	if len(auto.Items) != 2 || auto.Items[0].ID != "metadata-match" {
		t.Fatalf("auto page = %#v, want filename metadata match promoted", auto.Items)
	}
}

func TestCatalogSemanticAutoSearchExcludesUnpublishedReadyMetadataMatch(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	assets := []semanticAsset{
		{
			SourceKey:  sourceKey,
			ID:         "semantic-best",
			MediaType:  "image",
			Filename:   "plain.jpg",
			CapturedAt: builtAt,
			Vector:     []float32{1, 0, 0, 0},
		},
		{
			SourceKey:  sourceKey,
			ID:         "published-metadata-match",
			MediaType:  "image",
			Filename:   "Kyoto published.jpg",
			CapturedAt: builtAt.Add(time.Second),
			Vector:     []float32{0.8, 0.6, 0, 0},
		},
	}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, assets, builtAt)
	ctx := context.Background()
	unpublished := semanticAsset{
		SourceKey:  sourceKey,
		ID:         "unpublished-metadata-match",
		MediaType:  "image",
		Filename:   "Kyoto unpublished.jpg",
		CapturedAt: builtAt.Add(2 * time.Hour),
		Vector:     []float32{1, 0, 0, 0},
	}
	nowText := formatCatalogTime(unpublished.CapturedAt)
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename,
			captured_at, duration, visibility_status, source_updated_at, is_favorite,
			content_sha1_hex, content_size_bytes, place_label, description, first_seen_at, updated_at
		) VALUES (?, 'immich', ?, 'image', ?, ?, NULL, 'active', ?, 0, ?, 25000, NULL, NULL, ?, ?)`,
		sourceKey,
		unpublished.ID,
		unpublished.Filename,
		nowText,
		nowText,
		strings.Repeat("f", 40),
		nowText,
		nowText,
	); err != nil {
		t.Fatalf("insert unpublished metadata asset: %v", err)
	}
	if _, err := service.catalog.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("rebuild canonical assets with unpublished match: %v", err)
	}
	if err := service.catalog.upsertSemanticVectors(
		ctx,
		sourceKey,
		testImageSemanticProfile{},
		[]semanticAsset{unpublished},
		unpublished.CapturedAt,
	); err != nil {
		t.Fatalf("upsert unpublished ready vector: %v", err)
	}

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind:  CollectionKindSearch,
			Query: &AssetSearchQuery{Text: "Kyoto", Mode: QueryModeAuto},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 1},
	})
	if err != nil {
		t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
	}
	page, err := service.searchCatalogSemanticAssets(ctx, normalized, testImageSemanticProfile{}, AssetSearchOptions{})
	if err != nil {
		t.Fatalf("searchCatalogSemanticAssets() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "published-metadata-match" {
		t.Fatalf("auto page = %#v, want only the metadata match in the active binary", page.Items)
	}
}

func TestCatalogSemanticMetadataCandidateDiscoveryIsBoundedBeforeVectorReads(t *testing.T) {
	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	ctx := context.Background()
	header := semanticBinaryIndexHeader{
		SourceKey:       sourceKey,
		ModelID:         "test-image-profile",
		VectorSpaceID:   "test-image-profile/d4",
		AssetGeneration: 7,
	}
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_index_membership_state (
			source_key, model_id, vector_space_id, asset_generation,
			binary_sha256, binary_size_bytes, node_count, built_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		header.SourceKey,
		header.ModelID,
		header.VectorSpaceID,
		header.AssetGeneration,
		strings.Repeat("a", 64),
		semanticBinaryIndexHeaderBytes,
		semanticSearchVisitBudget+128,
		formatCatalogTime(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)),
	); err != nil {
		t.Fatalf("insert metadata membership identity: %v", err)
	}
	tx, err := service.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin metadata candidate fixture: %v", err)
	}
	assetStatement, err := tx.PrepareContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename,
			captured_at, duration, visibility_status, source_updated_at, is_favorite,
			content_sha1_hex, content_size_bytes, place_label, description, first_seen_at, updated_at
		) VALUES (?, 'immich', ?, 'image', ?, ?, NULL, 'active', ?, 1, ?, 25000, 'Kyoto 京都', NULL, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare metadata asset insert: %v", err)
	}
	membershipStatement, err := tx.PrepareContext(ctx, `INSERT INTO semantic_index_membership (
			source_key, model_id, vector_space_id, asset_generation, upstream_asset_id, ordinal
		) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = assetStatement.Close()
		_ = tx.Rollback()
		t.Fatalf("prepare metadata membership insert: %v", err)
	}
	baseTime := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	for index := 0; index < semanticSearchVisitBudget+128; index++ {
		assetID := fmt.Sprintf("broad-match-%05d", index)
		filename := assetID + ".jpg"
		if index == semanticSearchVisitBudget {
			assetID = "boundary-match-after-limit"
			filename = "東京 exact match.jpg"
		}
		nowText := formatCatalogTime(baseTime.Add(time.Duration(index) * time.Second))
		if _, err := assetStatement.ExecContext(
			ctx,
			sourceKey,
			assetID,
			filename,
			nowText,
			nowText,
			fmt.Sprintf("%040x", index+1),
			nowText,
			nowText,
		); err != nil {
			_ = membershipStatement.Close()
			_ = assetStatement.Close()
			_ = tx.Rollback()
			t.Fatalf("insert broad metadata asset %d: %v", index, err)
		}
		if _, err := membershipStatement.ExecContext(
			ctx,
			sourceKey,
			header.ModelID,
			header.VectorSpaceID,
			header.AssetGeneration,
			assetID,
			index,
		); err != nil {
			_ = membershipStatement.Close()
			_ = assetStatement.Close()
			_ = tx.Rollback()
			t.Fatalf("insert broad metadata membership %d: %v", index, err)
		}
	}
	if err := membershipStatement.Close(); err != nil {
		_ = assetStatement.Close()
		_ = tx.Rollback()
		t.Fatalf("close metadata membership insert: %v", err)
	}
	if err := assetStatement.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close metadata asset insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit metadata candidate fixture: %v", err)
	}

	for _, query := range []string{"favorites", "Kyoto", "京都"} {
		normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
			Collection: AssetCollectionRequest{
				Kind:  CollectionKindSearch,
				Query: &AssetSearchQuery{Text: query, Mode: QueryModeAuto},
			},
			Page: AssetSearchPageRequest{Index: 0, Size: 1},
		})
		if err != nil {
			t.Fatalf("normalizeAssetSearchRequest(%q) error = %v", query, err)
		}
		refs, err := service.catalogSemanticMetadataCandidateRefs(ctx, normalized, query, header)
		if err != nil {
			t.Fatalf("catalogSemanticMetadataCandidateRefs(%q) error = %v", query, err)
		}
		if len(refs) != semanticSearchVisitBudget {
			t.Fatalf("metadata refs for %q = %d, want bounded %d before vector reads", query, len(refs), semanticSearchVisitBudget)
		}
	}

	from := baseTime.Add(semanticSearchVisitBudget * time.Second)
	to := from.Add(time.Second)
	for _, query := range []string{"Kyoto", "favorites"} {
		filtered, err := normalizeAssetSearchRequest(AssetSearchRequest{
			Collection: AssetCollectionRequest{
				Kind:  CollectionKindSearch,
				Query: &AssetSearchQuery{Text: query, Mode: QueryModeAuto},
				Filters: AssetSearchFilters{CapturedAt: &AssetSearchCapturedTime{
					From: &from,
					To:   &to,
				}},
			},
			Page: AssetSearchPageRequest{Index: 0, Size: 1},
		})
		if err != nil {
			t.Fatalf("normalize filtered metadata request %q: %v", query, err)
		}
		refs, err := service.catalogSemanticMetadataCandidateRefs(ctx, filtered, query, header)
		if err != nil {
			t.Fatalf("catalogSemanticMetadataCandidateRefs(filtered %q) error = %v", query, err)
		}
		if len(refs) != 1 || refs[0].AssetID != "boundary-match-after-limit" || refs[0].Ordinal != semanticSearchVisitBudget {
			t.Fatalf("filtered metadata refs for %q = %#v, want the only in-range match after the branch limit", query, refs)
		}
	}

	short, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind:  CollectionKindSearch,
			Query: &AssetSearchQuery{Text: "東京", Mode: QueryModeAuto},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 1},
	})
	if err != nil {
		t.Fatalf("normalize short metadata request: %v", err)
	}
	refs, err := service.catalogSemanticMetadataCandidateRefs(ctx, short, "東京", header)
	if err != nil {
		t.Fatalf("catalogSemanticMetadataCandidateRefs(short Tokyo) error = %v", err)
	}
	if len(refs) != 1 || refs[0].AssetID != "boundary-match-after-limit" || refs[0].Ordinal != semanticSearchVisitBudget {
		t.Fatalf("short metadata refs = %#v, want the exact match after the ordinal prefix", refs)
	}
}

func TestCatalogSemanticAutoSearchDoesNotWaitForWriterConnection(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	assets := []semanticAsset{
		{
			SourceKey:  sourceKey,
			ID:         "semantic-best",
			MediaType:  "image",
			Filename:   "plain.jpg",
			CapturedAt: builtAt,
			Vector:     []float32{0, 1, 0, 0},
		},
		{
			SourceKey:  sourceKey,
			ID:         "metadata-match",
			MediaType:  "image",
			Filename:   "Beach evening.jpg",
			CapturedAt: builtAt.Add(time.Second),
			Vector:     []float32{0, 0.8, 0.6, 0},
		},
	}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, assets, builtAt)
	writer, err := service.catalog.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin held catalog writer: %v", err)
	}
	defer writer.Rollback()

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind:  CollectionKindSearch,
			Query: &AssetSearchQuery{Text: "Beach", Mode: QueryModeAuto},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 2},
	})
	if err != nil {
		t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	page, err := service.searchCatalogSemanticAssets(ctx, normalized, testImageSemanticProfile{}, AssetSearchOptions{})
	if err != nil {
		t.Fatalf("searchCatalogSemanticAssets() with held writer error = %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].ID != "metadata-match" {
		t.Fatalf("semantic auto page with held writer = %#v, want metadata match promoted", page.Items)
	}
}

func TestCatalogSemanticSearchRareFilterStopsAtFixedVisitBudget(t *testing.T) {
	if catalogRaceDetectorEnabled {
		t.Skip("production 4,097-node builder coverage is deterministic and exceeds the race package timeout")
	}
	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	assets := make([]semanticAsset, 0, semanticSearchVisitBudget+1)
	for index := 0; index <= semanticSearchVisitBudget; index++ {
		assetID := fmt.Sprintf("asset-%04d", index)
		capturedAt := builtAt.Add(-24 * time.Hour)
		vector := []float32{0, 0.5, 0.5, 0}
		if index == semanticSearchVisitBudget {
			assetID = "only-filter-match"
			capturedAt = builtAt.Add(time.Hour)
			vector = []float32{0, 1, 0, 0}
		}
		assets = append(assets, semanticAsset{
			SourceKey:  sourceKey,
			ID:         assetID,
			MediaType:  "image",
			Filename:   assetID + ".jpg",
			CapturedAt: capturedAt,
			Vector:     vector,
		})
	}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, assets, builtAt)

	from := builtAt
	to := builtAt.Add(2 * time.Hour)
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind:  CollectionKindSearch,
			Query: &AssetSearchQuery{Text: "beach", Mode: QueryModeSemantic},
			Filters: AssetSearchFilters{CapturedAt: &AssetSearchCapturedTime{
				From: &from,
				To:   &to,
			}},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 1},
	})
	if err != nil {
		t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
	}
	page, err := service.searchCatalogSemanticAssets(context.Background(), normalized, testImageSemanticProfile{}, AssetSearchOptions{})
	if err != nil {
		t.Fatalf("searchCatalogSemanticAssets() error = %v", err)
	}
	if page.NextPageIndex != nil || len(page.Items) > 1 || (len(page.Items) == 1 && page.Items[0].ID != "only-filter-match") {
		t.Fatalf("rare filtered semantic page = %#v, want bounded best-available result", page)
	}
}

func newCatalogSemanticSearchBinaryFixture(
	t *testing.T,
	sourceKey string,
	assets []semanticAsset,
	builtAt time.Time,
) *Service {
	t.Helper()

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })

	ctx := context.Background()
	nowText := formatCatalogTime(builtAt)
	tx, err := service.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin catalog fixture transaction: %v", err)
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename,
			captured_at, duration, visibility_status, source_updated_at, is_favorite,
			content_sha1_hex, content_size_bytes, place_label, description, first_seen_at,
			updated_at
		) VALUES (?, 'immich', ?, 'image', ?, ?, NULL, 'active', ?, 0, ?, ?, NULL, NULL, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare catalog fixture insert: %v", err)
	}
	for index, asset := range assets {
		if _, err := statement.ExecContext(
			ctx,
			sourceKey,
			asset.ID,
			asset.Filename,
			formatCatalogTime(asset.CapturedAt),
			nowText,
			fmt.Sprintf("%040x", index+1),
			int64(10_000+index),
			nowText,
			nowText,
		); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			t.Fatalf("insert catalog fixture asset %q: %v", asset.ID, err)
		}
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close catalog fixture statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit catalog fixture transaction: %v", err)
	}
	if _, err := service.catalog.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}

	profile := testImageSemanticProfile{}
	seedAndWriteSemanticBinaryIndexForTest(t, service.catalog, ctx, sourceKey, profile, assets, builtAt, 0)
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			built_at, last_error, updated_at
		) VALUES (?, ?, ?, 'ready', ?, ?, ?, 0, 0, ?, NULL, ?)`,
		sourceKey,
		profile.ModelID(),
		profile.VectorSpaceID(),
		profile.EmbeddingDim(),
		len(assets),
		len(assets),
		nowText,
		nowText,
	); err != nil {
		t.Fatalf("insert semantic fixture state: %v", err)
	}
	return service
}

func installSemanticRuntimeStatusTestModel(t *testing.T, dataDir string) (*SemanticModelPackStore, SemanticModelPackStatus) {
	t.Helper()

	helperPath := writeEmbeddingRuntimeHelperForStatusTest(t)
	modelStore, err := LoadOrCreateSemanticModelPackStoreWithOptions(dataDir, SemanticModelPackStoreOptions{
		RuntimeHelperPath: helperPath,
	})
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStoreWithOptions() error = %v", err)
	}
	artifact := semanticRuntimeStatusTestZipArtifact(t)
	pack := semanticRuntimeStatusTestPack()
	pack.EmbeddingDim = 4
	sum := sha256.Sum256(artifact)
	pack.Artifact.SHA256 = hex.EncodeToString(sum[:])
	pack.Artifact.SizeBytes = int64(len(artifact))
	install, err := modelStore.InstallPack(context.Background(), pack, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack() error = %v", err)
	}
	if err := modelStore.FinalizePackInstall(pack.ID, pack.VectorSpaceID, install.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall() error = %v", err)
	}
	return modelStore, pack
}

func semanticRuntimeStatusTestZipArtifactForIdentity(t *testing.T, modelID string, vectorSpaceID string) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	writeEntry := func(name string, payload []byte) {
		t.Helper()
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("Create(%q) error = %v", name, err)
		}
		if _, err := entry.Write(payload); err != nil {
			t.Fatalf("Write(%q) error = %v", name, err)
		}
	}
	layout, err := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"product":       "timich-semantic-model-pack",
		"modelId":       modelID,
		"vectorSpaceId": vectorSpaceID,
		"embeddingDim":  4,
		"inputKind":     semanticInputKindImage,
		"runtime":       semanticRuntimeLoaderONNXRuntime,
		"imageModel":    "models/image.onnx",
		"textModel":     "models/text.onnx",
		"tokenizer":     "tokenizer/tokenizer.json",
	})
	if err != nil {
		t.Fatalf("Marshal(runtime layout) error = %v", err)
	}
	writeEntry("timich-model.json", layout)
	writeEntry("models/image.onnx", []byte("image model"))
	writeEntry("models/text.onnx", []byte("text model"))
	writeEntry("tokenizer/tokenizer.json", []byte(`{"model":"test"}`))
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return buffer.Bytes()
}

func TestSemanticSearchCandidateProfileUsesPublishedManifestWithoutCoverageAggregation(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	modelStore, pack := installSemanticRuntimeStatusTestModel(t, dataDir)

	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: dataDir, SemanticModels: modelStore})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	profile, ok := modelStore.CandidateEmbeddingProfile(pack.ID, pack.VectorSpaceID)
	if !ok {
		t.Fatal("CandidateEmbeddingProfile() ok = false")
	}
	if selected := service.semanticSearchCandidateProfile(ctx, AssetSearchOptions{}); selected == nil {
		t.Fatal("semanticSearchCandidateProfile() before first publish = nil, want runtime profile")
	}
	selection := service.semanticSearchProfileSelection(ctx, AssetSearchOptions{})
	if selection.profile == nil || selection.published {
		t.Fatalf("semanticSearchProfileSelection() before first publish = %#v, want identified unpublished profile", selection)
	}
	builtAt := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	seedAndWriteSemanticBinaryIndexForTest(t, service.catalog, ctx, sourceKey, profile, []semanticAsset{{
		SourceKey:  sourceKey,
		ID:         "asset-beach",
		MediaType:  "image",
		Filename:   "Summer Beach.jpg",
		CapturedAt: builtAt,
		Vector:     []float32{0, 1, 0, 0},
	}}, builtAt, 0)
	if _, err := service.catalog.db.ExecContext(ctx, `DROP TABLE semantic_vectors`); err != nil {
		t.Fatalf("drop semantic_vectors to make coverage aggregation unavailable: %v", err)
	}
	candidate, ok := modelStore.InstalledCandidateProfile()
	if !ok {
		t.Fatal("InstalledCandidateProfile() ok = false")
	}
	if _, err := service.SemanticModelBackfillStatus(ctx, candidate); err == nil {
		t.Fatal("SemanticModelBackfillStatus() error = nil after dropping semantic_vectors")
	}
	selection = service.semanticSearchProfileSelection(ctx, AssetSearchOptions{})
	if selection.profile == nil || !selection.published || selection.profile.ModelID() != pack.ID || selection.profile.VectorSpaceID() != pack.VectorSpaceID {
		t.Fatalf("semanticSearchProfileSelection() = %#v, want published candidate despite unavailable coverage aggregation", selection)
	}
	if _, err := modelStore.ActivatePack(pack.ID, pack.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack() error = %v", err)
	}
	selected := service.semanticSearchActiveProfile(ctx)
	if selected == nil || selected.ModelID() != pack.ID || selected.VectorSpaceID() != pack.VectorSpaceID {
		t.Fatalf("semanticSearchActiveProfile() = %#v, want published active profile despite unavailable coverage aggregation", selected)
	}
}

func TestSearchAssetsSkipsCandidateRuntimeInspectionForPublishedActiveIndex(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	markerPath := filepath.Join(t.TempDir(), "candidate-inspected")
	helperPath := filepath.Join(t.TempDir(), "timich-semantic-helper")
	const activeModelID = "active-search-model"
	const activeVectorSpaceID = "active-search-model/v1/d4"
	const candidateModelID = "candidate-search-model"
	const candidateVectorSpaceID = "candidate-search-model/v1/d4"
	helperScript := `#!/bin/sh
case "$1" in
  inspect)
    runtime_layout="$3"
    if grep -q '"modelId":"` + candidateModelID + `"' "$runtime_layout/timich-model.json"; then
      : > ` + shellQuoteForTest(markerPath) + `
    fi
    printf '%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"` + activeModelID + `","vectorSpaceId":"` + activeVectorSpaceID + `","embeddingDim":4,"inputKind":"image","loaded":true,"canEmbed":true}'
    ;;
  embed-text)
    printf '%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"` + activeModelID + `","vectorSpaceId":"` + activeVectorSpaceID + `","embeddingDim":4,"inputKind":"image","vector":[0,1,0,0],"input":"text-helper"}'
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o700); err != nil {
		t.Fatalf("WriteFile(runtime helper) error = %v", err)
	}
	modelStore, err := LoadOrCreateSemanticModelPackStoreWithOptions(dataDir, SemanticModelPackStoreOptions{
		RuntimeHelperPath: helperPath,
	})
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStoreWithOptions() error = %v", err)
	}
	install := func(modelID string, vectorSpaceID string, role string) SemanticModelPackStatus {
		t.Helper()
		artifact := semanticRuntimeStatusTestZipArtifactForIdentity(t, modelID, vectorSpaceID)
		sum := sha256.Sum256(artifact)
		pack := semanticRuntimeStatusTestPack()
		pack.ID = modelID
		pack.VectorSpaceID = vectorSpaceID
		pack.EmbeddingDim = 4
		pack.Artifact.Filename = modelID + ".zip"
		pack.Artifact.SHA256 = hex.EncodeToString(sum[:])
		pack.Artifact.SizeBytes = int64(len(artifact))
		result, err := modelStore.InstallPack(ctx, pack, bytes.NewReader(artifact))
		if err != nil {
			t.Fatalf("InstallPack(%s) error = %v", role, err)
		}
		if err := modelStore.FinalizePackInstall(pack.ID, pack.VectorSpaceID, result.InstallID); err != nil {
			t.Fatalf("FinalizePackInstall(%s) error = %v", role, err)
		}
		return pack
	}
	activePack := install(activeModelID, activeVectorSpaceID, "active")
	if _, err := modelStore.ActivatePack(activePack.ID, activePack.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack() error = %v", err)
	}
	install(candidateModelID, candidateVectorSpaceID, "candidate")

	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: dataDir, SemanticModels: modelStore})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	activeProfile, ok := modelStore.ActiveEmbeddingProfileWithContext(ctx)
	if !ok {
		t.Fatal("ActiveEmbeddingProfileWithContext() ok = false")
	}
	builtAt := time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC)
	seedAndWriteSemanticBinaryIndexForTest(t, service.catalog, ctx, sourceKey, activeProfile, []semanticAsset{{
		SourceKey:  sourceKey,
		ID:         "asset-beach",
		MediaType:  "image",
		Filename:   "Summer Beach.jpg",
		CapturedAt: builtAt,
		Vector:     []float32{0, 1, 0, 0},
	}}, builtAt, 0)
	builtAtText := formatCatalogTime(builtAt)
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			built_at, last_error, updated_at
		) VALUES (?, ?, ?, 'ready', ?, 1, 1, 0, 0, ?, NULL, ?)`,
		sourceKey,
		activeModelID,
		activeVectorSpaceID,
		activeProfile.EmbeddingDim(),
		builtAtText,
		builtAtText,
	); err != nil {
		t.Fatalf("insert semantic state: %v", err)
	}
	if _, err := service.catalog.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}
	if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove(candidate marker) error = %v", err)
	}

	page, err := service.SearchAssetsWithContext(ctx, AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "Beach",
				Mode: QueryModeSemantic,
			},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssetsWithContext() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "asset-beach" || page.Resolved.Semantic == nil || page.Resolved.Semantic.ModelID != activeModelID {
		t.Fatalf("SearchAssetsWithContext() page = %#v, want active semantic result", page)
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate runtime inspection occurred despite a published active index: %v", err)
	}
}

func TestSearchAssetsPreservesActiveSemanticProfileBeforeFirstPublish(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	modelStore, pack := installSemanticRuntimeStatusTestModel(t, dataDir)
	if _, err := modelStore.ActivatePack(pack.ID, pack.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack() error = %v", err)
	}

	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: dataDir, SemanticModels: modelStore})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	startedAt := time.Date(2026, 8, 22, 3, 0, 0, 0, time.UTC)
	if _, err := service.catalog.ReplaceFull(ctx, sourceKey, []ImmichMirrorAsset{{
		UpstreamAssetID: "asset-beach",
		MediaType:       "image",
		Filename:        "Summer Beach.jpg",
		CapturedAt:      startedAt,
	}}, 0, startedAt); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}
	profile, ok := modelStore.ActiveEmbeddingProfileWithContext(ctx)
	if !ok {
		t.Fatal("ActiveEmbeddingProfileWithContext() ok = false")
	}
	backfill, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, profile, startedAt, SemanticBackfillOptions{
		ImageLoader: staticSemanticImageLoader{},
		MaxAssets:   1,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors() error = %v", err)
	}
	if backfill.Status.CompletedVectorCount != 1 || backfill.Status.IndexedVectorCount != 0 {
		t.Fatalf("backfill status = %#v, want one unpublished vector", backfill.Status)
	}

	page, err := service.SearchAssetsWithContext(ctx, AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "Beach",
				Mode: QueryModeAuto,
			},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssetsWithContext() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "asset-beach" || page.Resolved.QueryMode != QueryModeFilename {
		t.Fatalf("SearchAssetsWithContext() page = %#v, want filename fallback", page)
	}
	semantic := page.Resolved.Semantic
	if semantic == nil ||
		semantic.Status != semanticBackfillStatusBackfilling ||
		semantic.ModelID != pack.ID ||
		semantic.VectorSpaceID != pack.VectorSpaceID ||
		semantic.CompletedVectorCount != 1 ||
		semantic.IndexedVectorCount != 0 ||
		semantic.MessageCode != semanticMessageIndexUnavailableFallback ||
		semantic.FallbackQueryMode != QueryModeFilename {
		t.Fatalf("SearchAssetsWithContext() semantic = %#v", semantic)
	}

	semanticOnly, err := service.SearchAssetsWithContext(ctx, AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "Beach",
				Mode: QueryModeSemantic,
			},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssetsWithContext(semantic) error = %v", err)
	}
	semantic = semanticOnly.Resolved.Semantic
	if len(semanticOnly.Items) != 0 ||
		semanticOnly.Resolved.QueryMode != QueryModeSemantic ||
		semantic == nil ||
		semantic.Status != semanticBackfillStatusBackfilling ||
		semantic.ModelID != pack.ID ||
		semantic.VectorSpaceID != pack.VectorSpaceID ||
		semantic.MessageCode != semanticMessageIndexBackfilling ||
		semantic.FallbackQueryMode != "" {
		t.Fatalf("SearchAssetsWithContext(semantic) page = %#v", semanticOnly)
	}
}

func TestSemanticSearchPublishedIndexAvailabilityUsesManifestAcrossPartialSources(t *testing.T) {
	const publishedSourceKey = "1111111111111111"
	const missingSourceKey = "2222222222222222"
	builtAt := time.Date(2026, 8, 22, 1, 0, 0, 0, time.UTC)
	service := newCatalogSemanticSearchBinaryFixture(t, publishedSourceKey, []semanticAsset{{
		SourceKey:  publishedSourceKey,
		ID:         "asset-001",
		MediaType:  "image",
		Filename:   "asset-001.jpg",
		CapturedAt: builtAt,
		Vector:     []float32{1, 0, 0, 0},
	}}, builtAt)
	service.ReconfigureDatasources([]config.DatasourceConfig{
		{
			SourceKey:   missingSourceKey,
			Name:        "Missing index",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://missing.immich.test",
			AccessToken: "missing-key",
		},
		{
			SourceKey:   publishedSourceKey,
			Name:        "Published index",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://published.immich.test",
			AccessToken: "published-key",
		},
	})
	profile := testImageSemanticProfile{}
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE semantic_state
		SET status = 'backfilling', asset_generation = indexed_generation + 1
		WHERE source_key = ? AND model_id = ?`, publishedSourceKey, profile.ModelID()); err != nil {
		t.Fatalf("make live semantic state newer than published generation: %v", err)
	}
	if !service.semanticSearchHasPublishedIndex(context.Background(), profile) {
		t.Fatal("semanticSearchHasPublishedIndex() = false, want last published partial-source index to remain searchable during a newer generation")
	}

	manifestPath := service.catalog.semanticBinaryActiveManifestPath(publishedSourceKey, profile)
	manifest, err := readSemanticBinaryActiveManifest(manifestPath)
	if err != nil {
		t.Fatalf("readSemanticBinaryActiveManifest() error = %v", err)
	}
	manifest.Header.VectorSpaceID = "other/vector-space"
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatalf("write mismatched manifest: %v", err)
	}
	if service.semanticSearchHasPublishedIndex(context.Background(), profile) {
		t.Fatal("semanticSearchHasPublishedIndex() = true for mismatched published manifest")
	}
}

func TestCatalogSemanticAutoSearchFallsBackWhenPublishedIndexDisappears(t *testing.T) {
	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, []semanticAsset{{
		SourceKey:  sourceKey,
		ID:         "asset-beach",
		MediaType:  "image",
		Filename:   "Summer Beach.jpg",
		CapturedAt: builtAt,
		Vector:     []float32{0, 1, 0, 0},
	}}, builtAt)
	profile := testImageSemanticProfile{}
	if err := os.Remove(service.catalog.semanticBinaryActiveManifestPath(sourceKey, profile)); err != nil {
		t.Fatalf("remove active semantic manifest: %v", err)
	}
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind:  CollectionKindSearch,
			Query: &AssetSearchQuery{Text: "beach", Mode: QueryModeAuto},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
	}
	page, err := service.searchCatalogSemanticAssets(context.Background(), normalized, profile, AssetSearchOptions{})
	if err != nil {
		t.Fatalf("searchCatalogSemanticAssets() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "asset-beach" || page.Resolved.QueryMode != QueryModeFilename {
		t.Fatalf("fallback page = %#v, want filename result after published index disappears", page)
	}
	if page.Resolved.Semantic == nil || page.Resolved.Semantic.Eligible || page.Resolved.Semantic.FallbackQueryMode != QueryModeFilename {
		t.Fatalf("fallback semantic resolution = %#v", page.Resolved.Semantic)
	}
}

func TestCanonicalAssetsForScoredSourcesChunksLargeCandidateSets(t *testing.T) {
	t.Parallel()

	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sourceKey := "1111111111111111"
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	nowText := formatCatalogTime(now)
	scored := make([]semanticScoredAsset, 0, 250)
	for index := 0; index < 250; index++ {
		assetID := fmt.Sprintf("asset-%03d", index)
		if _, err := store.db.ExecContext(ctx, `INSERT INTO catalog_assets (
				source_key, datasource_kind, upstream_asset_id, media_type, filename,
				captured_at, duration, visibility_status, source_updated_at, is_favorite,
				content_sha1_hex, content_size_bytes, place_label, description, first_seen_at,
				updated_at
			) VALUES (?, 'immich', ?, 'image', ?, ?, NULL, 'active', ?, 0, ?, 1234, NULL, NULL, ?, ?)`,
			sourceKey,
			assetID,
			assetID+".jpg",
			nowText,
			nowText,
			fmt.Sprintf("%040x", index+1),
			nowText,
			nowText,
		); err != nil {
			t.Fatalf("insert catalog asset %q: %v", assetID, err)
		}
		scored = append(scored, semanticScoredAsset{
			Asset:      semanticAsset{SourceKey: sourceKey, ID: assetID},
			Similarity: float32(250-index) / 250,
		})
	}
	if _, err := store.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}

	items, err := store.canonicalAssetsForScoredSources(ctx, scored, true)
	if err != nil {
		t.Fatalf("canonicalAssetsForScoredSources() error = %v", err)
	}
	if len(items) != len(scored) {
		t.Fatalf("canonical items count = %d, want %d", len(items), len(scored))
	}
	if items[0].ID != "asset-000" || items[len(items)-1].ID != "asset-249" {
		t.Fatalf("canonical item order = %s/%s, want asset-000/asset-249", items[0].ID, items[len(items)-1].ID)
	}
}

func TestLocalDuplicatePreviewFallsBackToImmichThumbnail(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	immichSource := "1111111111111111"
	localSource := "2222222222222222"
	secondImmichSource := "3333333333333333"
	thirdImmichSource := "4444444444444444"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{
		{
			SourceKey: localSource,
			Name:      "Local",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "photos",
		},
		{
			SourceKey:   immichSource,
			Name:        "Immich",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://immich.test",
			AccessToken: "test-key",
			Indexing:    &config.DatasourceIndexingConfig{},
		},
		{
			SourceKey:   secondImmichSource,
			Name:        "Immich 2",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://immich-2.test",
			AccessToken: "test-key",
			Indexing:    &config.DatasourceIndexingConfig{},
		},
		{
			SourceKey:   thirdImmichSource,
			Name:        "Immich 3",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://immich-3.test",
			AccessToken: "test-key",
			Indexing:    &config.DatasourceIndexingConfig{},
		},
	}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	t.Cleanup(func() {
		if service.catalog != nil {
			if err := service.catalog.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
		}
	})

	sha1Hex := strings.Repeat("b", 40)
	capturedAt := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	if _, err := service.catalog.ReplaceFull(ctx, immichSource, []ImmichMirrorAsset{
		{
			UpstreamAssetID:  "immich-family",
			MediaType:        "image",
			Filename:         "immich-family.jpg",
			CapturedAt:       capturedAt,
			ContentSHA1Hex:   sha1Hex,
			ContentSizeBytes: 1234,
		},
		{
			UpstreamAssetID:  "immich-family-copy",
			MediaType:        "image",
			Filename:         "immich-family-copy.jpg",
			CapturedAt:       capturedAt,
			ContentSHA1Hex:   sha1Hex,
			ContentSizeBytes: 1234,
		},
	}, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}
	for _, duplicate := range []struct {
		sourceKey string
		assetID   string
	}{
		{sourceKey: secondImmichSource, assetID: "immich-family-copy-2"},
		{sourceKey: thirdImmichSource, assetID: "immich-family-copy-3"},
	} {
		if _, err := service.catalog.ReplaceFull(ctx, duplicate.sourceKey, []ImmichMirrorAsset{{
			UpstreamAssetID:  duplicate.assetID,
			MediaType:        "image",
			Filename:         duplicate.assetID + ".jpg",
			CapturedAt:       capturedAt,
			ContentSHA1Hex:   sha1Hex,
			ContentSizeBytes: 1234,
		}}, 0, time.Now().UTC()); err != nil {
			t.Fatalf("ReplaceFull(%s) error = %v", duplicate.sourceKey, err)
		}
	}
	nowText := formatCatalogTime(time.Now().UTC())
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename,
			captured_at, duration, visibility_status, source_updated_at, is_favorite,
			content_sha1_hex, content_size_bytes, place_label, description, first_seen_at,
			updated_at
		) VALUES (?, 'local_filesystem', ?, 'image', ?, ?, NULL, 'active', ?, 0, ?, ?, NULL, NULL, ?, ?)`,
		localSource,
		"local-family",
		"local-family.jpg",
		formatCatalogTime(capturedAt),
		nowText,
		sha1Hex,
		int64(1234),
		nowText,
		nowText,
	); err != nil {
		t.Fatalf("insert local catalog source row: %v", err)
	}
	if _, err := service.catalog.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}

	var requestedPaths []string
	immichThumbnail := encodeJPEGForTest(t, 320, 240)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requestedPaths = append(requestedPaths, r.URL.RequestURI())
			switch r.URL.Host {
			case "immich.test":
				return nil, errors.New("first duplicate is offline")
			case "immich-2.test":
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("unavailable")),
				}, nil
			case "immich-3.test":
				if r.URL.RequestURI() != "/api/assets/immich-family-copy-3/thumbnail?size=thumbnail" {
					t.Fatalf("unexpected healthy fallback request path %q", r.URL.RequestURI())
				}
			default:
				t.Fatalf("unexpected fallback host %q", r.URL.Host)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type":   []string{"image/jpeg"},
					"Content-Length": []string{strconv.Itoa(len(immichThumbnail))},
				},
				Body: io.NopCloser(bytes.NewReader(immichThumbnail)),
			}, nil
		}),
	}
	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() with fallback enabled error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].SourceKey != localSource {
		t.Fatalf("fallback-enabled page = %#v, want visible local-primary duplicate", page)
	}

	request, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/local-family/preview", nil)
	if err != nil {
		t.Fatalf("NewRequest(preview) error = %v", err)
	}
	response, err := service.PreviewFromSource(request, localSource, "local-family")
	if err != nil {
		t.Fatalf("PreviewFromSource() error = %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll(preview) error = %v", err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "image/jpeg" || len(body) == 0 {
		t.Fatalf("fallback response status=%d contentType=%q bytes=%d, want jpeg", response.StatusCode, response.Header.Get("Content-Type"), len(body))
	}
	if len(requestedPaths) != 3 {
		t.Fatalf("requested paths = %#v, want all duplicates through healthy source", requestedPaths)
	}

	var secondRequestedHosts []string
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			secondRequestedHosts = append(secondRequestedHosts, r.URL.Host)
			switch r.URL.Host {
			case "immich.test":
				return nil, errors.New("first duplicate is offline")
			case "immich-2.test":
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: http.NoBody}, nil
			case "immich-3.test":
				return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: http.NoBody}, nil
			default:
				t.Fatalf("unexpected fallback host %q", r.URL.Host)
				return nil, nil
			}
		}),
	}
	if _, err := service.PreviewFromSource(request, localSource, "local-family"); !errors.Is(err, ErrDatasourceUnavailable) {
		t.Fatalf("PreviewFromSource() with all duplicate sources unavailable error = %v, want ErrDatasourceUnavailable", err)
	}
	if len(secondRequestedHosts) != 1 || secondRequestedHosts[0] != "immich-3.test" {
		t.Fatalf("second fallback sources = %#v, want only the eligible last healthy source", secondRequestedHosts)
	}
	thirdRequests := 0
	service.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		thirdRequests++
		return nil, errors.New("deferred source must not be called")
	})}
	if _, err := service.PreviewFromSource(request, localSource, "local-family"); !errors.Is(err, ErrDatasourceUnavailable) {
		t.Fatalf("PreviewFromSource() while all sources are deferred error = %v, want ErrDatasourceUnavailable", err)
	}
	if thirdRequests != 0 {
		t.Fatalf("fallback calls during backoff = %d, want 0", thirdRequests)
	}

	fallbackDisabled := false
	state := service.datasourceStateSnapshot()
	localDatasource := state.datasources[localSource]
	localDatasource.Scan = &config.LocalDatasourceScanConfig{ImmichFallbackEnabled: &fallbackDisabled}
	service.ReconfigureDatasources([]config.DatasourceConfig{
		localDatasource,
		state.datasources[immichSource],
		state.datasources[secondImmichSource],
		state.datasources[thirdImmichSource],
	})
	page, err = service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() with fallback disabled error = %v", err)
	}
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("fallback-disabled page = %#v, want pending local duplicate hidden", page)
	}
	requestedPaths = nil
	if _, err := service.PreviewFromSource(request, localSource, "local-family"); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("PreviewFromSource() with fallback disabled error = %v, want ErrAssetNotFound", err)
	}
	if len(requestedPaths) != 0 {
		t.Fatalf("fallback-disabled requested paths = %#v, want no Immich request", requestedPaths)
	}
}

func TestImmichDatasourceUsesCatalogWithoutIndexingConfiguration(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	service.client = immichMirrorTestClient(t)

	if _, err := service.SyncDatasourceMirror(context.Background(), sourceKey, MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncDatasourceMirror() error = %v", err)
	}
	capabilities := service.SearchCapabilities()
	if fmt.Sprint(capabilities.QueryModes) != fmt.Sprint([]string{QueryModeAuto, QueryModeSemantic, QueryModeFilename}) ||
		capabilities.Semantic == nil {
		t.Fatalf("always-indexed Immich capabilities = %#v, want catalog semantic capabilities", capabilities)
	}
	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() error = %v", err)
	}
	if page.Total != 3 || len(page.Items) != 3 {
		t.Fatalf("timeline page total=%d items=%d, want 3", page.Total, len(page.Items))
	}
	if page.Items[0].SourceKey != sourceKey || page.Items[0].ID != "asset-new" {
		t.Fatalf("timeline item = %#v, want indexed Immich catalog item", page.Items[0])
	}
}

func TestSemanticBackfillSourceOrderUsesDurableCursor(t *testing.T) {
	t.Parallel()

	const firstSource = "1111111111111111"
	const secondSource = "2222222222222222"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{
		{SourceKey: firstSource, Name: "First", Kind: config.DatasourceKindImmichIndexed, URL: "http://first.test", AccessToken: "test-key"},
		{SourceKey: secondSource, Name: "Second", Kind: config.DatasourceKindImmichIndexed, URL: "http://second.test", AccessToken: "test-key"},
	}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	candidate := SemanticModelProfileStatus{ModelID: "test-model", VectorSpaceID: "test-model/d4"}
	if err := service.rememberSemanticBackfillSource(context.Background(), candidate, secondSource); err != nil {
		t.Fatalf("rememberSemanticBackfillSource() error = %v", err)
	}
	ordered := service.semanticBackfillSourceOrder(context.Background(), candidate, []string{firstSource, secondSource})
	if len(ordered) != 2 || ordered[0] != secondSource || ordered[1] != firstSource {
		t.Fatalf("semantic source order = %#v, want durable cursor source first", ordered)
	}

	var stored string
	if err := service.catalog.db.QueryRow(`SELECT next_source_key FROM semantic_backfill_scheduler_state
		WHERE model_id = ? AND vector_space_id = ?`, candidate.ModelID, candidate.VectorSpaceID).Scan(&stored); err != nil {
		t.Fatalf("read semantic backfill cursor: %v", err)
	}
	if stored != secondSource {
		t.Fatalf("stored semantic source cursor = %q, want %q", stored, secondSource)
	}
}

func TestNextSemanticBackfillSourceRotatesAfterFullVisit(t *testing.T) {
	t.Parallel()

	sources := []string{"source-a", "source-b", "source-c"}
	if got := nextSemanticBackfillSource(sources, len(sources)-1); got != "source-b" {
		t.Fatalf("next source after full visit = %q, want source-b", got)
	}
	if got := nextSemanticBackfillSource(sources, 0); got != "source-b" {
		t.Fatalf("next source after partial visit = %q, want source-b", got)
	}
	if got := nextSemanticBackfillSource([]string{"source-a"}, 0); got != "source-a" {
		t.Fatalf("next singleton source = %q, want source-a", got)
	}
}

func TestSemanticBackfillRoundQuotasRedistributeFairly(t *testing.T) {
	t.Parallel()

	first := semanticBackfillRoundQuotas(100, 3)
	if !reflect.DeepEqual(first, []int{34, 33, 33}) {
		t.Fatalf("first semantic source quotas = %#v, want [34 33 33]", first)
	}
	second := semanticBackfillRoundQuotas(33, 2)
	if !reflect.DeepEqual(second, []int{17, 16}) {
		t.Fatalf("redistributed semantic source quotas = %#v, want [17 16]", second)
	}
}

func TestImmichMediaFallbackSourceFailureExcludesAssetErrors(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		err  error
		want bool
	}{
		{err: fmt.Errorf("wrapped: %w", ErrDatasourceUnavailable), want: true},
		{err: ErrMediaTooLarge, want: false},
		{err: ErrMediaInvalid, want: false},
		{err: ErrAssetNotFound, want: false},
	} {
		if got := immichMediaFallbackSourceFailure(testCase.err); got != testCase.want {
			t.Fatalf("immichMediaFallbackSourceFailure(%v) = %t, want %t", testCase.err, got, testCase.want)
		}
	}
}

func TestCatalogSemanticCandidateBackfillStatusCountsActiveAssets(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	service.client = immichMirrorTestClientWithImages(t)

	ctx := context.Background()
	if _, err := service.SyncMirror(ctx, MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}
	candidate := SemanticModelProfileStatus{
		ModelID:       "candidate-image-profile",
		VectorSpaceID: "candidate-image-profile/d4",
		EmbeddingDim:  4,
		Role:          semanticModelRoleCandidate,
		ProfileKind:   semanticProfileKindModelPack,
		InputKind:     semanticInputKindImage,
	}
	status, err := service.SemanticModelBackfillStatus(ctx, candidate)
	if err != nil {
		t.Fatalf("SemanticModelBackfillStatus() error = %v", err)
	}
	if status == nil {
		t.Fatal("SemanticModelBackfillStatus() = nil, want status")
	}
	if status.Status != semanticBackfillStatusPending || status.EligibleAssetCount != 2 || status.CompletedVectorCount != 0 || status.IndexedVectorCount != 0 || status.RemainingVectorCount != 2 {
		t.Fatalf("pending candidate status = %#v", status)
	}

	nowText := formatCatalogTime(time.Now().UTC())
	insertVector := func(assetID string, vector []float32) {
		t.Helper()
		insertSemanticVectorForTest(t,
			service.catalog,
			ctx,
			sourceKey,
			assetID,
			candidate.ModelID,
			candidate.VectorSpaceID,
			candidate.EmbeddingDim,
			vector,
			"test",
			"ready",
			nil,
			nowText,
			nil,
		)
	}
	insertVector("asset-new", []float32{1, 0, 0, 0})
	insertVector("asset-old-out-of-scope", []float32{0, 0, 1, 0})

	status, err = service.SemanticModelBackfillStatus(ctx, candidate)
	if err != nil {
		t.Fatalf("SemanticModelBackfillStatus() after stale vector error = %v", err)
	}
	if status == nil || status.Status != semanticBackfillStatusBackfilling || status.EligibleAssetCount != 2 || status.CompletedVectorCount != 1 || status.IndexedVectorCount != 0 || status.RemainingVectorCount != 1 {
		t.Fatalf("stale vector candidate status = %#v", status)
	}

	insertVector("asset-beach", []float32{0, 1, 0, 0})

	status, err = service.SemanticModelBackfillStatus(ctx, candidate)
	if err != nil {
		t.Fatalf("SemanticModelBackfillStatus() after vectors error = %v", err)
	}
	if status == nil || status.Status != semanticBackfillStatusIndexing || status.EligibleAssetCount != 2 || status.CompletedVectorCount != 2 || status.IndexedVectorCount != 0 || status.RemainingVectorCount != 0 {
		t.Fatalf("indexing candidate status = %#v", status)
	}

	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_state (
		source_key, model_id, vector_space_id, status, embedding_dim,
		completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
		built_at, last_error, updated_at
	) VALUES (?, ?, ?, 'ready', ?, 2, 2, 0, 0, ?, NULL, ?)`,
		sourceKey, candidate.ModelID, candidate.VectorSpaceID, candidate.EmbeddingDim, nowText, nowText); err != nil {
		t.Fatalf("insert published candidate semantic state: %v", err)
	}
	status, err = service.SemanticModelBackfillStatus(ctx, candidate)
	if err != nil {
		t.Fatalf("SemanticModelBackfillStatus() after hnsw error = %v", err)
	}
	if status == nil || status.Status != semanticBackfillStatusReady || status.EligibleAssetCount != 2 || status.CompletedVectorCount != 2 || status.IndexedVectorCount != 2 || status.RemainingVectorCount != 0 {
		t.Fatalf("ready candidate status = %#v", status)
	}
}

func TestSemanticVectorPayloadIsWrittenBeforeWriterTransaction(t *testing.T) {
	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing:    &config.DatasourceIndexingConfig{},
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	profile := testImageSemanticProfile{}
	payloadDir := filepath.Join(service.catalog.root, semanticVectorPayloadDirName)
	blockingTx, err := service.catalog.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin blocking tx: %v", err)
	}
	blockingReleased := false
	defer func() {
		if !blockingReleased {
			_ = blockingTx.Rollback()
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- service.catalog.upsertSemanticVectors(context.Background(), sourceKey, profile, []semanticAsset{{
			SourceKey:   sourceKey,
			ID:          "asset-a",
			MediaType:   "image",
			Filename:    "asset-a.jpg",
			CapturedAt:  time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
			Vector:      []float32{1, 0, 0, 0},
			VectorInput: "image",
		}}, time.Now().UTC())
	}()

	expectedPayloadSize := int64(profile.EmbeddingDim() * 4)
	payloadPath, payloadWritten := waitForPayloadFileAtLeast(payloadDir, expectedPayloadSize, 2*time.Second)
	if !payloadWritten {
		blockingReleased = true
		_ = blockingTx.Rollback()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Fatalf("semantic vector payload %q was not written while writer tx was occupied", payloadPath)
	}
	select {
	case err := <-done:
		blockingReleased = true
		_ = blockingTx.Rollback()
		t.Fatalf("upsertSemanticVectors completed before blocking writer tx was released: %v", err)
	default:
	}
	cleanupStarted := make(chan struct{})
	cleanupDone := make(chan error, 1)
	go func() {
		close(cleanupStarted)
		cleanupDone <- service.catalog.cleanupUnreferencedSemanticVectorPayloads(context.Background())
	}()
	<-cleanupStarted
	select {
	case err := <-cleanupDone:
		blockingReleased = true
		_ = blockingTx.Rollback()
		t.Fatalf("payload cleanup completed while publication was in flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	blockingReleased = true
	if err := blockingTx.Rollback(); err != nil {
		t.Fatalf("rollback blocking tx: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("upsertSemanticVectors() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upsertSemanticVectors() did not finish after writer tx was released")
	}
	select {
	case err := <-cleanupDone:
		if err != nil {
			t.Fatalf("cleanupUnreferencedSemanticVectorPayloads() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("payload cleanup did not finish after publication committed")
	}
	if _, err := os.Stat(payloadPath); err != nil {
		t.Fatalf("published payload was removed by concurrent cleanup: %v", err)
	}

	var vectorOffset int64
	var vectorLength int
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT vector_offset, vector_length
		FROM semantic_vectors
		WHERE source_key = ? AND upstream_asset_id = ? AND model_id = ?`,
		sourceKey,
		"asset-a",
		profile.ModelID(),
	).Scan(&vectorOffset, &vectorLength); err != nil {
		t.Fatalf("read inserted semantic vector ref: %v", err)
	}
	if vectorOffset != 0 || int64(vectorLength) != expectedPayloadSize {
		t.Fatalf("semantic vector payload ref = offset %d length %d, want 0/%d", vectorOffset, vectorLength, expectedPayloadSize)
	}
}

func TestSemanticContentRevisionRequeuesVectorAndCollectsOnlyCandidatePayloads(t *testing.T) {
	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	ctx := context.Background()
	nowText := formatCatalogTime(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC))
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO catalog_assets (
		source_key, datasource_kind, upstream_asset_id, media_type, filename, captured_at,
		visibility_status, content_sha1_hex, content_size_bytes, first_seen_at, updated_at
	) VALUES (?, 'immich_indexed', 'asset-a', 'image', 'asset-a.jpg', ?, 'active', ?, 100, ?, ?)`,
		sourceKey, nowText, strings.Repeat("a", 40), nowText, nowText); err != nil {
		t.Fatalf("insert catalog asset: %v", err)
	}
	profile := testImageSemanticProfile{}
	if err := service.catalog.upsertSemanticVectors(ctx, sourceKey, profile, []semanticAsset{{
		SourceKey:   sourceKey,
		ID:          "asset-a",
		MediaType:   "image",
		Filename:    "asset-a.jpg",
		CapturedAt:  time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
		Vector:      []float32{1, 0, 0, 0},
		VectorInput: "image",
	}}, time.Now().UTC()); err != nil {
		t.Fatalf("upsertSemanticVectors() error = %v", err)
	}
	var replacedBatchID string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT payload_batch_id FROM semantic_vectors
		WHERE source_key = ? AND upstream_asset_id = 'asset-a' AND model_id = ?`, sourceKey, profile.ModelID()).Scan(&replacedBatchID); err != nil {
		t.Fatalf("read payload batch ID: %v", err)
	}
	replacedPath := filepath.Join(service.catalog.root, semanticVectorPayloadDirName, replacedBatchID+semanticVectorPayloadExt)
	orphanRef, err := service.catalog.appendSemanticVectorPayloadBlob(ctx, sourceKey, profile.ModelID(), profile.VectorSpaceID(), profile.EmbeddingDim(), encodeSemanticVector([]float32{0, 1, 0, 0}))
	if err != nil {
		t.Fatalf("append unrelated orphan payload: %v", err)
	}
	orphanPath := filepath.Join(service.catalog.root, semanticVectorPayloadDirName, orphanRef.BatchID+semanticVectorPayloadExt)

	if _, err := service.catalog.db.ExecContext(ctx, `UPDATE catalog_assets
		SET content_sha1_hex = ?, content_size_bytes = 200, updated_at = ?
		WHERE source_key = ? AND upstream_asset_id = 'asset-a'`, strings.Repeat("b", 40), nowText, sourceKey); err != nil {
		t.Fatalf("update catalog content revision: %v", err)
	}
	var status string
	var payloadBatchID sql.NullString
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT status, payload_batch_id FROM semantic_vectors
		WHERE source_key = ? AND upstream_asset_id = 'asset-a' AND model_id = ?`, sourceKey, profile.ModelID()).Scan(&status, &payloadBatchID); err != nil {
		t.Fatalf("read invalidated semantic vector: %v", err)
	}
	if status != "pending" || payloadBatchID.Valid {
		t.Fatalf("invalidated vector status=%q batch=%q, want pending without payload", status, payloadBatchID.String)
	}
	if err := service.catalog.cleanupSemanticVectorPayloadCandidates(ctx); err != nil {
		t.Fatalf("cleanupSemanticVectorPayloadCandidates() error = %v", err)
	}
	if _, err := os.Stat(replacedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replaced payload remains after candidate cleanup: %v", err)
	}
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("unrelated payload was scanned during candidate cleanup: %v", err)
	}
	var candidates int
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM semantic_vector_payload_gc_candidates`).Scan(&candidates); err != nil {
		t.Fatalf("count payload GC candidates: %v", err)
	}
	if candidates != 0 {
		t.Fatalf("payload GC candidates = %d, want 0", candidates)
	}
}

func TestSemanticVectorPayloadRecoveryRequeuesTruncatedBatch(t *testing.T) {
	const sourceKey = "1111111111111111"
	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	profile := testImageSemanticProfile{}
	ctx := context.Background()
	if err := service.catalog.upsertSemanticVectors(ctx, sourceKey, profile, []semanticAsset{{
		SourceKey:   sourceKey,
		ID:          "asset-a",
		MediaType:   "image",
		Filename:    "asset-a.jpg",
		CapturedAt:  time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
		Vector:      []float32{1, 0, 0, 0},
		VectorInput: "image",
	}}, time.Now().UTC()); err != nil {
		service.Close()
		t.Fatalf("upsertSemanticVectors() error = %v", err)
	}
	var batchID string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT payload_batch_id
		FROM semantic_vectors
		WHERE source_key = ? AND upstream_asset_id = ? AND model_id = ?`,
		sourceKey, "asset-a", profile.ModelID()).Scan(&batchID); err != nil {
		service.Close()
		t.Fatalf("read semantic payload batch ID: %v", err)
	}
	payloadPath := filepath.Join(service.catalog.root, semanticVectorPayloadDirName, batchID+semanticVectorPayloadExt)
	if err := service.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := os.Truncate(payloadPath, 1); err != nil {
		t.Fatalf("Truncate(%s) error = %v", payloadPath, err)
	}

	reopened, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer reopened.Close()
	var status string
	var recoveredBatchID sql.NullString
	if err := reopened.db.QueryRowContext(ctx, `SELECT status, payload_batch_id
		FROM semantic_vectors
		WHERE source_key = ? AND upstream_asset_id = ? AND model_id = ?`,
		sourceKey, "asset-a", profile.ModelID()).Scan(&status, &recoveredBatchID); err != nil {
		t.Fatalf("read recovered semantic vector: %v", err)
	}
	if status != "pending" || recoveredBatchID.Valid {
		t.Fatalf("recovered semantic vector status=%q batch=%q, want pending without payload", status, recoveredBatchID.String)
	}
	if _, err := os.Stat(payloadPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("truncated payload batch still exists: %v", err)
	}
}

func TestSemanticVectorPayloadRecoveryRequeuesSameSizeCorruption(t *testing.T) {
	const sourceKey = "1111111111111111"
	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	profile := testImageSemanticProfile{}
	ctx := context.Background()
	if err := service.catalog.upsertSemanticVectors(ctx, sourceKey, profile, []semanticAsset{{
		SourceKey:   sourceKey,
		ID:          "asset-a",
		MediaType:   "image",
		Filename:    "asset-a.jpg",
		CapturedAt:  time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
		Vector:      []float32{1, 0, 0, 0},
		VectorInput: "image",
	}}, time.Now().UTC()); err != nil {
		service.Close()
		t.Fatalf("upsertSemanticVectors() error = %v", err)
	}
	var batchID string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT payload_batch_id
		FROM semantic_vectors
		WHERE source_key = ? AND upstream_asset_id = ? AND model_id = ?`,
		sourceKey, "asset-a", profile.ModelID()).Scan(&batchID); err != nil {
		service.Close()
		t.Fatalf("read semantic payload batch ID: %v", err)
	}
	payloadPath := filepath.Join(service.catalog.root, semanticVectorPayloadDirName, batchID+semanticVectorPayloadExt)
	if err := service.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	raw, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", payloadPath, err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(payloadPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", payloadPath, err)
	}

	reopened, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.readSemanticVectorPayload(ctx, batchID, profile.EmbeddingDim(), 0, profile.EmbeddingDim()*4); err == nil {
		t.Fatal("readSemanticVectorPayload(corrupt batch) error = nil")
	}
	var status string
	var recoveredBatchID sql.NullString
	if err := reopened.db.QueryRowContext(ctx, `SELECT status, payload_batch_id
		FROM semantic_vectors
		WHERE source_key = ? AND upstream_asset_id = ? AND model_id = ?`,
		sourceKey, "asset-a", profile.ModelID()).Scan(&status, &recoveredBatchID); err != nil {
		t.Fatalf("read recovered semantic vector: %v", err)
	}
	if status != "pending" || recoveredBatchID.Valid {
		t.Fatalf("recovered semantic vector status=%q batch=%q, want pending without payload", status, recoveredBatchID.String)
	}
	if _, err := os.Stat(payloadPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupted payload batch still exists: %v", err)
	}
}

func waitForPayloadFileAtLeast(directory string, size int64, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(directory, "*"+semanticVectorPayloadExt))
		for _, path := range matches {
			if info, err := os.Stat(path); err == nil && info.Size() >= size {
				return path, true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", false
}

func waitForFileAtLeast(path string, size int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		info, err := os.Stat(path)
		if err == nil && info.Size() >= size {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestCatalogSemanticBackfillsCandidateSemanticVectorsInBatches(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	service.client = immichMirrorTestClientWithImages(t)

	ctx := context.Background()
	if _, err := service.SyncMirror(ctx, MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}

	first, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, testImageSemanticProfile{}, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: service,
		MaxAssets:   1,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors(first) error = %v", err)
	}
	if first.ProcessedVectorCount != 1 ||
		first.IndexedVectorCount != 0 ||
		first.Status.Status != semanticBackfillStatusBackfilling ||
		first.Status.CompletedVectorCount != 1 ||
		first.Status.IndexedVectorCount != 0 ||
		first.Status.RemainingVectorCount != 1 {
		t.Fatalf("first backfill result = %#v", first)
	}

	second, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, testImageSemanticProfile{}, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: service,
		MaxAssets:   10,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors(second) error = %v", err)
	}
	if second.ProcessedVectorCount != 1 ||
		second.IndexedVectorCount != 0 ||
		second.Status.Status != semanticBackfillStatusIndexing ||
		second.Status.CompletedVectorCount != 2 ||
		second.Status.IndexedVectorCount != 0 ||
		second.Status.RemainingVectorCount != 0 {
		t.Fatalf("second backfill result = %#v", second)
	}
	third, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, testImageSemanticProfile{}, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: service,
		MaxAssets:   10,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors(third) error = %v", err)
	}
	if third.ProcessedVectorCount != 0 || third.IndexedVectorCount != 0 || third.Status.Status != semanticBackfillStatusIndexing {
		t.Fatalf("third backfill result = %#v", third)
	}
	var vectorCount int
	if err := service.catalog.db.QueryRow(`SELECT COUNT(*) FROM semantic_vectors WHERE source_key = ? AND model_id = ?`, sourceKey, testImageSemanticProfile{}.ModelID()).Scan(&vectorCount); err != nil {
		t.Fatalf("count vectors after no-op backfill: %v", err)
	}
	if vectorCount != 2 {
		t.Fatalf("vector count after no-op backfill = %d, want 2", vectorCount)
	}
}

func TestCatalogSemanticBackfillFailureDoesNotStarveLaterAssets(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	service.client = immichMirrorTestClientWithImages(t)

	ctx := context.Background()
	if _, err := service.SyncMirror(ctx, MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}
	loader := selectiveFailSemanticImageLoader{failAssetID: "asset-new"}
	first, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, testImageSemanticProfile{}, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: loader,
		MaxAssets:   1,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors(first) error = %v", err)
	}
	if first.ProcessedVectorCount != 1 || first.Status.CompletedVectorCount != 0 {
		t.Fatalf("first backfill result = %#v, want one failed attempt and no ready vectors", first)
	}
	if first.Status.EligibleNowVectorCount != 1 || first.Status.NextEligibleAt == nil {
		t.Fatalf("first backfill eligibility = %#v, want later asset ready now and failed asset deferred", first.Status)
	}
	var firstStatus string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT status FROM semantic_vectors
		WHERE source_key = ? AND upstream_asset_id = ? AND model_id = ?`, sourceKey, "asset-new", testImageSemanticProfile{}.ModelID()).Scan(&firstStatus); err != nil {
		t.Fatalf("read failed semantic vector row: %v", err)
	}
	if firstStatus != "failed" {
		t.Fatalf("failed semantic vector status = %q, want failed", firstStatus)
	}

	second, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, testImageSemanticProfile{}, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: loader,
		MaxAssets:   1,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors(second) error = %v", err)
	}
	if second.ProcessedVectorCount != 1 || second.Status.CompletedVectorCount != 1 {
		t.Fatalf("second backfill result = %#v, want later asset ready", second)
	}
	if second.Status.EligibleNowVectorCount != 0 || second.Status.NextEligibleAt == nil || !second.Status.NextEligibleAt.After(time.Now().UTC()) {
		t.Fatalf("second backfill eligibility = %#v, want only a future failed-asset retry", second.Status)
	}
	var readyStatus string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT status FROM semantic_vectors
		WHERE source_key = ? AND upstream_asset_id = ? AND model_id = ?`, sourceKey, "asset-beach", testImageSemanticProfile{}.ModelID()).Scan(&readyStatus); err != nil {
		t.Fatalf("read ready semantic vector row: %v", err)
	}
	if readyStatus != "ready" {
		t.Fatalf("later semantic vector status = %q, want ready", readyStatus)
	}

	third, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, testImageSemanticProfile{}, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: loader,
		MaxAssets:   1,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors(third) error = %v", err)
	}
	if third.ProcessedVectorCount != 0 {
		t.Fatalf("third backfill result = %#v, want failed asset retry backoff", third)
	}

	profile := testImageSemanticProfile{}
	enqueued, err := service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs(settled failure) error = %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs(settled failure) = %d, want one publish job", enqueued)
	}
	publish, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, time.Now().UTC())
	if err != nil {
		t.Fatalf("PublishNextSemanticIndexJob(settled failure) error = %v", err)
	}
	if !publish.Published ||
		publish.Status.CompletedVectorCount != 1 ||
		publish.Status.FailedVectorCount != 1 ||
		publish.Status.IndexedVectorCount != 1 ||
		publish.Status.Status != semanticBackfillStatusBackfilling {
		t.Fatalf("settled-failure publish = %#v, want the ready vector published while the failure remains retryable", publish)
	}

	makeFailedAssetRetryEligible := func() {
		t.Helper()
		tx, err := service.catalog.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin failed semantic asset retry eligibility update: %v", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `INSERT INTO semantic_generation_suppression(source_key)
			VALUES (?) ON CONFLICT(source_key) DO NOTHING`, sourceKey); err != nil {
			t.Fatalf("suppress generation for retry eligibility update: %v", err)
		}
		retryBefore := formatCatalogTime(time.Now().UTC().Add(-semanticBackfillFailureRetryInterval - time.Minute))
		if _, err := tx.ExecContext(ctx, `UPDATE semantic_vectors
			SET generated_at = ?
			WHERE source_key = ? AND upstream_asset_id = ? AND model_id = ?`,
			retryBefore, sourceKey, "asset-new", profile.ModelID()); err != nil {
			t.Fatalf("make failed semantic asset retry eligible: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM semantic_generation_suppression WHERE source_key = ?`, sourceKey); err != nil {
			t.Fatalf("restore generation after retry eligibility update: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit failed semantic asset retry eligibility update: %v", err)
		}
	}

	makeFailedAssetRetryEligible()
	retriedFailure, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, profile, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: loader,
		MaxAssets:   1,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors(retried failure) error = %v", err)
	}
	if retriedFailure.ProcessedVectorCount != 1 ||
		retriedFailure.Status.CompletedVectorCount != 1 ||
		retriedFailure.Status.FailedVectorCount != 1 ||
		retriedFailure.Status.IndexedVectorCount != 1 ||
		retriedFailure.Status.AssetGeneration != publish.Status.IndexedGeneration ||
		retriedFailure.Status.IndexedGeneration != publish.Status.IndexedGeneration {
		t.Fatalf("retried failure = %#v, want unchanged published generation", retriedFailure)
	}
	enqueued, err = service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs(retried failure) error = %v", err)
	}
	if enqueued != 0 {
		t.Fatalf("ReconcileSemanticIndexJobs(retried failure) = %d, want no unchanged-index publish job", enqueued)
	}

	makeFailedAssetRetryEligible()
	recovered, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, profile, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: staticSemanticImageLoader{},
		MaxAssets:   1,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors(recovered) error = %v", err)
	}
	if recovered.ProcessedVectorCount != 1 ||
		recovered.Status.CompletedVectorCount != 2 ||
		recovered.Status.FailedVectorCount != 0 ||
		recovered.Status.IndexedVectorCount != 1 {
		t.Fatalf("recovered backfill = %#v, want the formerly failed vector ready and unpublished", recovered)
	}
	enqueued, err = service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs(recovered) error = %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs(recovered) = %d, want one follow-up publish job", enqueued)
	}
	republish, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, time.Now().UTC())
	if err != nil {
		t.Fatalf("PublishNextSemanticIndexJob(recovered) error = %v", err)
	}
	if !republish.Published ||
		republish.Status.Status != semanticBackfillStatusReady ||
		republish.Status.CompletedVectorCount != 2 ||
		republish.Status.IndexedVectorCount != 2 {
		t.Fatalf("recovered publish = %#v, want both vectors published and ready", republish)
	}
}

func TestCatalogSemanticBackfillSharedFailureDoesNotPersistAssetFailures(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	service.client = immichMirrorTestClientWithImages(t)

	ctx := context.Background()
	if _, err := service.SyncMirror(ctx, MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}
	tests := []struct {
		name       string
		profile    semanticEmbeddingProfile
		loader     SemanticImageLoader
		errorClass error
	}{
		{
			name:       "source loader",
			profile:    testImageSemanticProfile{},
			loader:     failingSemanticImageLoader{err: errors.New("immich unavailable")},
			errorClass: ErrSemanticSourceUnavailable,
		},
		{
			name:       "runtime",
			profile:    runtimeFailSemanticProfile{},
			loader:     staticSemanticImageLoader{},
			errorClass: ErrSemanticRuntimeUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, test.profile, time.Now().UTC(), SemanticBackfillOptions{
				ImageLoader: test.loader,
				MaxAssets:   2,
				Workers:     2,
			})
			if err == nil {
				t.Fatalf("BackfillSemanticVectors() result = %#v, want shared failure", result)
			}
			if !errors.Is(err, test.errorClass) {
				t.Fatalf("BackfillSemanticVectors() error = %v, want %v", err, test.errorClass)
			}
			var persisted int
			if err := service.catalog.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM semantic_vectors WHERE source_key = ? AND model_id = ?`, sourceKey, test.profile.ModelID()).Scan(&persisted); err != nil {
				t.Fatalf("count semantic failure rows: %v", err)
			}
			if persisted != 0 {
				t.Fatalf("persisted semantic rows = %d, want no asset failures for shared error", persisted)
			}
		})
	}
}

func TestCatalogSemanticAutoSearchFallsBackWhenModelPackHasNoPublishedIndex(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	service.client = immichMirrorTestClient(t)

	ctx := context.Background()
	if _, err := service.SyncMirror(ctx, MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}
	backfill, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, testImageSemanticProfile{}, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: staticSemanticImageLoader{},
		MaxAssets:   1,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors() error = %v", err)
	}
	if backfill.Status.CompletedVectorCount != 1 || backfill.Status.IndexedVectorCount != 0 {
		t.Fatalf("backfill status = %#v, want one unpublished vector", backfill.Status)
	}

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "Beach",
				Mode: QueryModeAuto,
			},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize search request: %v", err)
	}
	page, err := service.searchCatalogSemanticAssets(ctx, normalized, testImageSemanticProfile{}, AssetSearchOptions{})
	if err != nil {
		t.Fatalf("searchCatalogSemanticAssets() error = %v", err)
	}
	if len(page.Items) != 1 || page.Total != 1 || page.Items[0].ID != "asset-beach" {
		t.Fatalf("search page = %#v, want filename fallback while the first index is unpublished", page)
	}
	if page.Resolved.QueryMode != QueryModeFilename ||
		page.Resolved.Semantic == nil ||
		page.Resolved.Semantic.ModelID != "test-image-profile" ||
		page.Resolved.Semantic.ProfileKind != semanticProfileKindModelPack ||
		page.Resolved.Semantic.Status != semanticBackfillStatusBackfilling ||
		page.Resolved.Semantic.MessageCode != semanticMessageIndexUnavailableFallback ||
		page.Resolved.Semantic.Eligible ||
		page.Resolved.Semantic.FallbackQueryMode != QueryModeFilename {
		t.Fatalf("semantic resolution = %#v", page.Resolved.Semantic)
	}
}

func TestSemanticBinarySearchDoesNotWaitOnStatusOverlayWhenDBHandleBusy(t *testing.T) {
	const (
		indexedSourceKey = "1111111111111111"
		missingSourceKey = "2222222222222222"
	)
	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{
		{
			SourceKey:   indexedSourceKey,
			Name:        "Indexed Immich",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://immich-indexed.test",
			AccessToken: "test-key",
			Indexing: &config.DatasourceIndexingConfig{
				LatestAssetLimit: 1,
			},
		},
		{
			SourceKey:   missingSourceKey,
			Name:        "Missing Binary Immich",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://immich-missing.test",
			AccessToken: "test-key",
			Indexing: &config.DatasourceIndexingConfig{
				LatestAssetLimit: 1,
			},
		},
	}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	ctx := context.Background()
	profile := testImageSemanticProfile{}
	now := time.Date(2026, 7, 8, 7, 55, 0, 0, time.UTC)
	nowText := formatCatalogTime(now)
	assetID := "indexed-001"
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename, captured_at,
			duration, visibility_status, source_updated_at, is_favorite, content_sha1_hex,
			content_size_bytes, place_label, description, first_seen_at, updated_at
		) VALUES (?, 'immich', ?, 'image', ?, ?, NULL, 'active', ?, 0, ?, 1000, NULL, NULL, ?, ?)`,
		indexedSourceKey,
		assetID,
		assetID+".jpg",
		nowText,
		nowText,
		fmt.Sprintf("%040x", 1),
		nowText,
		nowText,
	); err != nil {
		t.Fatalf("insert catalog asset: %v", err)
	}
	insertSemanticVectorForTest(t,
		service.catalog,
		ctx,
		indexedSourceKey,
		assetID,
		profile.ModelID(),
		profile.VectorSpaceID(),
		profile.EmbeddingDim(),
		[]float32{1, 0, 0, 0},
		"test",
		"ready",
		nil,
		nowText,
		nowText,
	)
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			built_at, last_error, updated_at
		) VALUES (?, ?, ?, 'ready', ?, 1, 1, 0, 0, ?, NULL, ?)`,
		indexedSourceKey,
		profile.ModelID(),
		profile.VectorSpaceID(),
		profile.EmbeddingDim(),
		nowText,
		nowText,
	); err != nil {
		t.Fatalf("insert semantic state: %v", err)
	}
	writeSemanticBinaryIndexFromReadyVectorsForTest(t, service.catalog, ctx, indexedSourceKey, profile)

	tx, err := service.catalog.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin blocking tx: %v", err)
	}
	defer tx.Rollback()

	missingCtx, cancelMissing := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelMissing()
	missingStarted := time.Now()
	missingTraversal, ok, err := service.catalog.openSemanticIndexTraversal(
		missingCtx,
		missingSourceKey,
		profile,
		[]float32{1, 0, 0, 0},
	)
	if err != nil {
		t.Fatalf("missing binary search error = %v", err)
	}
	if ok {
		if missingTraversal != nil {
			_ = missingTraversal.Close()
		}
		t.Fatal("missing binary search reported usable index")
	}
	if elapsed := time.Since(missingStarted); elapsed > time.Second {
		t.Fatalf("missing binary search elapsed=%s, want short best-effort status overlay", elapsed)
	}

	indexedCtx, cancelIndexed := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelIndexed()
	indexedStarted := time.Now()
	indexedTraversal, ok, err := service.catalog.openSemanticIndexTraversal(
		indexedCtx,
		indexedSourceKey,
		profile,
		[]float32{1, 0, 0, 0},
	)
	if err != nil {
		t.Fatalf("indexed binary search error = %v", err)
	}
	if !ok || indexedTraversal == nil {
		t.Fatalf("indexed binary traversal ok=%v traversal=%#v", ok, indexedTraversal)
	}
	defer indexedTraversal.Close()
	scored, err := indexedTraversal.Advance(indexedCtx, 10)
	if err != nil {
		t.Fatalf("indexed binary traversal advance error = %v", err)
	}
	if len(scored) != 1 || indexedTraversal.IndexedCount != 1 {
		t.Fatalf("indexed binary search scored=%d indexed=%d", len(scored), indexedTraversal.IndexedCount)
	}
	if indexedTraversal.Semantic.Status != semanticBackfillStatusReady || indexedTraversal.Semantic.IndexedVectorCount != 1 {
		t.Fatalf("indexed binary semantic status = %#v", indexedTraversal.Semantic)
	}
	if elapsed := time.Since(indexedStarted); elapsed > time.Second {
		t.Fatalf("indexed binary search elapsed=%s, want file-backed search without DB wait", elapsed)
	}
}

func TestCatalogSemanticSearchDiversifiesWithinRequestedPage(t *testing.T) {
	t.Parallel()

	const (
		soccerSourceKey = "1111111111111111"
		otherSourceKey  = "2222222222222222"
	)
	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{
		{
			SourceKey:   soccerSourceKey,
			Name:        "Soccer Immich",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://immich-a.test",
			AccessToken: "test-key",
			Indexing: &config.DatasourceIndexingConfig{
				LatestAssetLimit: 80,
			},
		},
		{
			SourceKey:   otherSourceKey,
			Name:        "Other Immich",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://immich-b.test",
			AccessToken: "test-key",
			Indexing: &config.DatasourceIndexingConfig{
				LatestAssetLimit: 80,
			},
		},
	}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	ctx := context.Background()
	profile := testImageSemanticProfile{}
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	nowText := formatCatalogTime(now)
	insertAsset := func(sourceKey string, index int, prefix string, vector []float32) {
		t.Helper()
		assetID := fmt.Sprintf("%s-%03d", prefix, index)
		contentIndex := index + 1
		if sourceKey == otherSourceKey {
			contentIndex += 1000
		}
		if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO catalog_assets (
				source_key, datasource_kind, upstream_asset_id, media_type, filename, captured_at,
				duration, visibility_status, source_updated_at, is_favorite, content_sha1_hex,
				content_size_bytes, place_label, description, first_seen_at, updated_at
			) VALUES (?, 'immich', ?, 'image', ?, ?, NULL, 'active', ?, 0, ?, ?, NULL, NULL, ?, ?)`,
			sourceKey,
			assetID,
			assetID+".jpg",
			formatCatalogTime(now.Add(time.Duration(index)*time.Second)),
			nowText,
			fmt.Sprintf("%040x", contentIndex),
			int64(10_000+contentIndex),
			nowText,
			nowText,
		); err != nil {
			t.Fatalf("insert catalog asset %q: %v", assetID, err)
		}
		insertSemanticVectorForTest(t,
			service.catalog,
			ctx,
			sourceKey,
			assetID,
			profile.ModelID(),
			profile.VectorSpaceID(),
			profile.EmbeddingDim(),
			vector,
			"test",
			"ready",
			nil,
			nowText,
			nowText,
		)
	}
	for index := 0; index < 80; index++ {
		insertAsset(soccerSourceKey, index, "soccer", []float32{1, 0, 0, 0})
		insertAsset(otherSourceKey, index, "other", []float32{0.5, 0.5, 0, 0})
	}
	for _, sourceKey := range []string{soccerSourceKey, otherSourceKey} {
		if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_state (
				source_key, model_id, vector_space_id, status, embedding_dim,
				completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
				built_at, last_error, updated_at
			) VALUES (?, ?, ?, 'ready', ?, 80, 80, 0, 0, ?, NULL, ?)`,
			sourceKey,
			profile.ModelID(),
			profile.VectorSpaceID(),
			profile.EmbeddingDim(),
			nowText,
			nowText,
		); err != nil {
			t.Fatalf("insert semantic state for %q: %v", sourceKey, err)
		}
	}
	if _, err := service.catalog.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}
	for _, sourceKey := range []string{soccerSourceKey, otherSourceKey} {
		writeSemanticBinaryIndexFromReadyVectorsForTest(t, service.catalog, ctx, sourceKey, profile)
	}

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "soccer",
				Mode: QueryModeSemantic,
			},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 20},
	})
	if err != nil {
		t.Fatalf("normalize search request: %v", err)
	}
	page, err := service.searchCatalogSemanticAssets(ctx, normalized, profile, AssetSearchOptions{IncludeSemanticScores: true})
	if err != nil {
		t.Fatalf("searchCatalogSemanticAssets() error = %v", err)
	}
	if len(page.Items) != 20 {
		t.Fatalf("search items = %d, want 20", len(page.Items))
	}
	for index, item := range page.Items {
		if item.SourceKey != soccerSourceKey {
			t.Fatalf("item %d source key = %q (%q), want %q", index, item.SourceKey, item.Filename, soccerSourceKey)
		}
		if item.SemanticScore == nil || *item.SemanticScore < 0.9 {
			t.Fatalf("item %d semantic score = %v, want high soccer cluster score", index, item.SemanticScore)
		}
	}
}

func TestCatalogSemanticBackfillPublishesCandidateIndexWhenComplete(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	service.client = immichMirrorTestClientWithImages(t)

	ctx := context.Background()
	if _, err := service.SyncMirror(ctx, MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}

	first, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, testImageSemanticProfile{}, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: service,
		MaxAssets:   1,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors(first) error = %v", err)
	}
	if first.ProcessedVectorCount != 1 ||
		first.IndexedVectorCount != 0 ||
		first.Status.Status != semanticBackfillStatusBackfilling ||
		first.Status.CompletedVectorCount != 1 ||
		first.Status.IndexedVectorCount != 0 ||
		first.Status.RemainingVectorCount != 1 {
		t.Fatalf("first backfill result = %#v", first)
	}

	second, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, testImageSemanticProfile{}, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: service,
		MaxAssets:   10,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors(second) error = %v", err)
	}
	if second.ProcessedVectorCount != 1 ||
		second.IndexedVectorCount != 0 ||
		second.Status.Status != semanticBackfillStatusIndexing ||
		second.Status.CompletedVectorCount != 2 ||
		second.Status.IndexedVectorCount != 0 ||
		second.Status.RemainingVectorCount != 0 {
		t.Fatalf("second backfill result = %#v", second)
	}
	profile := testImageSemanticProfile{}
	enqueued, err := service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs() error = %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs() = %d, want one publish job", enqueued)
	}
	publish, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, time.Now().UTC())
	if err != nil {
		t.Fatalf("PublishNextSemanticIndexJob() error = %v", err)
	}
	if !publish.Published ||
		publish.IndexedVectorCount != 2 ||
		publish.Status.Status != semanticBackfillStatusReady ||
		publish.Status.PendingIndexJobCount != 0 {
		t.Fatalf("publish result = %#v", publish)
	}
}

func TestCatalogSemanticBackfillCanDeferCandidateIndexPublish(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	service.client = immichMirrorTestClientWithImages(t)

	ctx := context.Background()
	if _, err := service.SyncMirror(ctx, MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}

	profile := testImageSemanticProfile{}
	backfill, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, profile, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: service,
		MaxAssets:   10,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors() error = %v", err)
	}
	if backfill.ProcessedVectorCount != 2 ||
		backfill.IndexedVectorCount != 0 ||
		backfill.Status.Status != semanticBackfillStatusIndexing ||
		backfill.Status.CompletedVectorCount != 2 ||
		backfill.Status.IndexedVectorCount != 0 ||
		backfill.Status.PendingIndexJobCount != 0 {
		t.Fatalf("deferred backfill result = %#v", backfill)
	}
	enqueued, err := service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs() error = %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs() = %d, want one publish job", enqueued)
	}

	publish, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, time.Now().UTC())
	if err != nil {
		t.Fatalf("PublishNextSemanticIndexJob() error = %v", err)
	}
	if !publish.Published ||
		publish.IndexedVectorCount != 2 ||
		publish.Status.Status != semanticBackfillStatusReady ||
		publish.Status.PendingIndexJobCount != 0 {
		t.Fatalf("publish result = %#v", publish)
	}
	persisted, err := service.catalog.semanticStatus(ctx, sourceKey, profile)
	if err != nil {
		t.Fatalf("semanticStatus() error = %v", err)
	}
	if persisted.Status != semanticBackfillStatusReady ||
		persisted.CompletedVectorCount != 2 ||
		persisted.IndexedVectorCount != 2 {
		t.Fatalf("persisted semantic status = %#v, want ready with two indexed vectors", persisted)
	}
}

func TestSemanticIndexPublishFinalizesStateAndJobAtomically(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	service.client = immichMirrorTestClientWithImages(t)

	ctx := context.Background()
	profile := testImageSemanticProfile{}
	if _, err := service.SyncMirror(ctx, MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}
	if _, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, profile, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: service,
		MaxAssets:   10,
	}); err != nil {
		t.Fatalf("BackfillSemanticVectors() error = %v", err)
	}
	if enqueued, err := service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, false, time.Now().UTC()); err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs() error = %v", err)
	} else if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs() = %d, want one publish job", enqueued)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `CREATE TRIGGER fail_semantic_publish_state
		BEFORE UPDATE ON semantic_state
		WHEN NEW.indexed_vector_count > OLD.indexed_vector_count
		BEGIN
			SELECT RAISE(FAIL, 'injected semantic state failure');
		END`); err != nil {
		t.Fatalf("create semantic state failure trigger: %v", err)
	}

	if _, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, time.Now().UTC()); err == nil {
		t.Fatal("PublishNextSemanticIndexJob() error = nil, want injected finalization failure")
	}
	var jobStatus string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT status FROM semantic_index_jobs
		WHERE source_key = ? AND model_id = ? AND vector_space_id = ?`,
		sourceKey, profile.ModelID(), profile.VectorSpaceID()).Scan(&jobStatus); err != nil {
		t.Fatalf("read failed semantic publish job: %v", err)
	}
	if jobStatus == semanticIndexJobStatusCompleted {
		t.Fatalf("semantic publish job status = %q, must not complete without semantic state", jobStatus)
	}
	persisted, err := service.catalog.semanticStatus(ctx, sourceKey, profile)
	if err != nil {
		t.Fatalf("semanticStatus(after failed finalization) error = %v", err)
	}
	if persisted.IndexedVectorCount != 0 || persisted.Status == semanticBackfillStatusReady {
		t.Fatalf("semantic state after failed finalization = %#v, want previous non-ready state", persisted)
	}
	buildFiles, err := filepath.Glob(filepath.Join(service.catalog.root, semanticBinaryIndexDirName, "*.build.db"))
	if err != nil || len(buildFiles) != 1 {
		t.Fatalf("semantic index resumable build files after failure = %#v, err=%v, want one", buildFiles, err)
	}

	if _, err := service.catalog.db.ExecContext(ctx, `DROP TRIGGER fail_semantic_publish_state`); err != nil {
		t.Fatalf("drop semantic state failure trigger: %v", err)
	}
	nowText := formatCatalogTime(time.Now().UTC())
	if _, err := service.catalog.db.ExecContext(ctx, `UPDATE semantic_index_jobs SET scheduled_at = ? WHERE source_key = ? AND model_id = ?`,
		nowText, sourceKey, profile.ModelID()); err != nil {
		t.Fatalf("make failed semantic publish job eligible: %v", err)
	}
	retry, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, time.Now().UTC())
	if err != nil {
		t.Fatalf("PublishNextSemanticIndexJob(retry) error = %v", err)
	}
	if !retry.Published || retry.Status.Status != semanticBackfillStatusReady || retry.Status.PendingIndexJobCount != 0 || retry.Status.NextEligibleAt != nil {
		t.Fatalf("semantic publish retry = %#v, want atomically ready completion", retry)
	}
	persisted, err = service.catalog.semanticStatus(ctx, sourceKey, profile)
	if err != nil {
		t.Fatalf("semanticStatus(after retry) error = %v", err)
	}
	if persisted.Status != semanticBackfillStatusReady || persisted.IndexedVectorCount != retry.IndexedVectorCount {
		t.Fatalf("semantic state after retry = %#v, want ready state matching publish", persisted)
	}
	buildFiles, err = filepath.Glob(filepath.Join(service.catalog.root, semanticBinaryIndexDirName, "*.build.db"))
	if err != nil || len(buildFiles) != 0 {
		t.Fatalf("semantic index build files after successful retry = %#v, err=%v, want none", buildFiles, err)
	}
}

func TestSemanticIndexPublishTracksExactActiveAssetGeneration(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	ctx := context.Background()
	profile := testImageSemanticProfile{}
	nowText := formatCatalogTime(time.Now().UTC())
	for _, asset := range []struct {
		id         string
		visibility string
		vector     []float32
	}{
		{id: "asset-a", visibility: "active", vector: []float32{1, 0, 0, 0}},
		{id: "asset-b", visibility: "missing", vector: []float32{0, 1, 0, 0}},
	} {
		if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO catalog_assets (
				source_key, datasource_kind, upstream_asset_id, media_type, filename,
				captured_at, duration, visibility_status, source_updated_at, is_favorite,
				content_sha1_hex, content_size_bytes, place_label, description, first_seen_at, updated_at
			) VALUES (?, 'immich', ?, 'image', ?, ?, NULL, ?, ?, 0, NULL, NULL, NULL, NULL, ?, ?)`,
			sourceKey, asset.id, asset.id+".jpg", nowText, asset.visibility, nowText, nowText, nowText); err != nil {
			t.Fatalf("insert catalog asset %s: %v", asset.id, err)
		}
	}
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			built_at, last_error, updated_at
		) VALUES (?, ?, ?, 'indexing', ?, 1, 0, 0, -1, NULL, NULL, ?)`,
		sourceKey, profile.ModelID(), profile.VectorSpaceID(), profile.EmbeddingDim(), nowText); err != nil {
		t.Fatalf("insert semantic state: %v", err)
	}
	insertSemanticVectorForTest(t, service.catalog, ctx, sourceKey, "asset-a", profile.ModelID(), profile.VectorSpaceID(), profile.EmbeddingDim(), []float32{1, 0, 0, 0}, "image", "ready", nil, nowText, nil)
	insertSemanticVectorForTest(t, service.catalog, ctx, sourceKey, "asset-b", profile.ModelID(), profile.VectorSpaceID(), profile.EmbeddingDim(), []float32{0, 1, 0, 0}, "image", "ready", nil, nowText, nil)

	if enqueued, err := service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, false, time.Now().UTC()); err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs(initial) error = %v", err)
	} else if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs(initial) = %d, want 1", enqueued)
	}
	if _, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, time.Now().UTC()); err != nil {
		t.Fatalf("PublishNextSemanticIndexJob(initial) error = %v", err)
	}

	if _, err := service.catalog.db.ExecContext(ctx, `UPDATE catalog_assets
		SET visibility_status = CASE upstream_asset_id
			WHEN 'asset-a' THEN 'missing'
			WHEN 'asset-b' THEN 'active'
		END
		WHERE source_key = ? AND upstream_asset_id IN ('asset-a', 'asset-b')`, sourceKey); err != nil {
		t.Fatalf("swap catalog visibility: %v", err)
	}
	status, err := service.catalog.SemanticBackfillStatus(ctx, sourceKey, SemanticModelProfileStatus{
		ModelID: profile.ModelID(), VectorSpaceID: profile.VectorSpaceID(), EmbeddingDim: profile.EmbeddingDim(), InputKind: profile.InputKind(),
	})
	if err != nil {
		t.Fatalf("SemanticBackfillStatus(after swap) error = %v", err)
	}
	if status.AssetGeneration == status.IndexedGeneration || status.IndexedVectorCount != 1 {
		t.Fatalf("semantic status after active swap = %#v, want stale generation with previous snapshot count", status)
	}
	if enqueued, err := service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, false, time.Now().UTC()); err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs(after swap) error = %v", err)
	} else if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs(after swap) = %d, want 1", enqueued)
	}
	if _, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, time.Now().UTC()); err != nil {
		t.Fatalf("PublishNextSemanticIndexJob(after swap) error = %v", err)
	}

	reader, semantic, err := service.catalog.openSemanticBinaryIndexFile(ctx, sourceKey, profile)
	if err != nil {
		t.Fatalf("openSemanticBinaryIndexFile() error = %v", err)
	}
	defer reader.Close()
	asset, err := reader.assetForOrdinal(ctx, 0)
	if err != nil {
		t.Fatalf("assetForOrdinal() error = %v", err)
	}
	if asset.ID != "asset-b" || semantic.AssetGeneration != status.AssetGeneration {
		t.Fatalf("published binary asset=%q generation=%d, want asset-b generation=%d", asset.ID, semantic.AssetGeneration, status.AssetGeneration)
	}
}

func TestSemanticGenerationDriftKeepsPublishedSnapshotSearchableAndAggregatesIndexing(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	assets := []semanticAsset{{
		SourceKey:  sourceKey,
		ID:         "asset-a",
		MediaType:  "image",
		Filename:   "before.jpg",
		CapturedAt: builtAt,
		Vector:     []float32{1, 0, 0, 0},
	}}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, assets, builtAt)
	profile := testImageSemanticProfile{}
	if _, err := service.catalog.db.Exec(`UPDATE semantic_state SET asset_generation = 0, indexed_generation = 0
		WHERE source_key = ? AND model_id = ?`, sourceKey, profile.ModelID()); err != nil {
		t.Fatalf("reset semantic fixture generations: %v", err)
	}
	if _, err := service.catalog.db.Exec(`UPDATE catalog_assets SET filename = 'after.jpg'
		WHERE source_key = ? AND upstream_asset_id = 'asset-a'`, sourceKey); err != nil {
		t.Fatalf("update catalog metadata: %v", err)
	}
	if _, err := service.catalog.RebuildCatalogCanonicalAssets(context.Background()); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}

	status, err := service.semanticModelBackfillStatusForSourceKeys(context.Background(), []string{sourceKey}, SemanticModelProfileStatus{
		ModelID: profile.ModelID(), VectorSpaceID: profile.VectorSpaceID(), EmbeddingDim: profile.EmbeddingDim(), InputKind: profile.InputKind(),
	})
	if err != nil {
		t.Fatalf("semanticModelBackfillStatusForSourceKeys() error = %v", err)
	}
	if status.Status != semanticBackfillStatusIndexing || status.GenerationMismatchSourceCount != 1 || status.CompletedVectorCount != 1 || status.IndexedVectorCount != 1 {
		t.Fatalf("aggregate semantic status = %#v, want generation-aware indexing with 1/1 vectors", status)
	}

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindSearch, Query: &AssetSearchQuery{Text: "asset", Mode: QueryModeSemantic}},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize semantic search request: %v", err)
	}
	page, err := service.searchCatalogSemanticAssets(context.Background(), normalized, profile, AssetSearchOptions{})
	if err != nil {
		t.Fatalf("searchCatalogSemanticAssets() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "asset-a" || page.Items[0].Filename != "after.jpg" || page.Resolved.Semantic == nil || page.Resolved.Semantic.Status != "backfilling" {
		t.Fatalf("search during generation drift = %#v, want current catalog metadata from previous snapshot", page)
	}
}

func TestSemanticGenerationDriftPreservesPublishedCountAfterAssetRemoval(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 7, 19, 8, 15, 0, 0, time.UTC)
	assets := []semanticAsset{
		{SourceKey: sourceKey, ID: "asset-a", MediaType: "image", Filename: "asset-a.jpg", CapturedAt: builtAt, Vector: []float32{1, 0, 0, 0}},
		{SourceKey: sourceKey, ID: "asset-b", MediaType: "image", Filename: "asset-b.jpg", CapturedAt: builtAt.Add(time.Second), Vector: []float32{0.8, 0.6, 0, 0}},
	}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, assets, builtAt)
	profile := testImageSemanticProfile{}
	if _, err := service.catalog.db.Exec(`UPDATE catalog_assets SET visibility_status = 'missing'
		WHERE source_key = ? AND upstream_asset_id = 'asset-b'`, sourceKey); err != nil {
		t.Fatalf("hide semantic asset: %v", err)
	}
	if _, err := service.catalog.RebuildCatalogCanonicalAssets(context.Background()); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}
	current := SemanticModelBackfillStatus{
		Status: semanticBackfillStatusIndexing, ModelID: profile.ModelID(), VectorSpaceID: profile.VectorSpaceID(), EmbeddingDim: profile.EmbeddingDim(),
		EligibleAssetCount: 1, CompletedVectorCount: 1, IndexedVectorCount: 1, AssetGeneration: 1, IndexedGeneration: 0,
	}
	if err := service.catalog.upsertSemanticBackfillState(context.Background(), sourceKey, profile, current, builtAt.Add(time.Minute)); err != nil {
		t.Fatalf("upsertSemanticBackfillState(after removal) error = %v", err)
	}

	semantic, err := service.catalog.semanticStatus(context.Background(), sourceKey, profile)
	if err != nil {
		t.Fatalf("semanticStatus() error = %v", err)
	}
	if semantic.IndexedVectorCount != 2 || semantic.AssetGeneration != 1 || semantic.IndexedGeneration != 0 {
		t.Fatalf("published semantic state after removal = %#v, want old 2-vector snapshot at generation 0", semantic)
	}
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindSearch, Query: &AssetSearchQuery{Text: "asset", Mode: QueryModeSemantic}},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize semantic search request: %v", err)
	}
	page, err := service.searchCatalogSemanticAssets(context.Background(), normalized, profile, AssetSearchOptions{})
	if err != nil {
		t.Fatalf("searchCatalogSemanticAssets() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "asset-a" {
		t.Fatalf("search after semantic asset removal = %#v, want only current visible asset-a", page.Items)
	}
}

func TestSemanticStateUpdatesDoNotRewindAssetGeneration(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 7, 19, 8, 30, 0, 0, time.UTC)
	assets := []semanticAsset{{SourceKey: sourceKey, ID: "asset-a", MediaType: "image", Filename: "before.jpg", CapturedAt: builtAt, Vector: []float32{1, 0, 0, 0}}}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, assets, builtAt)
	profile := testImageSemanticProfile{}
	if _, err := service.catalog.db.Exec(`UPDATE catalog_assets SET filename = 'after.jpg'
		WHERE source_key = ? AND upstream_asset_id = 'asset-a'`, sourceKey); err != nil {
		t.Fatalf("advance semantic asset generation: %v", err)
	}
	stale := SemanticModelBackfillStatus{
		Status: semanticBackfillStatusReady, ModelID: profile.ModelID(), VectorSpaceID: profile.VectorSpaceID(), EmbeddingDim: profile.EmbeddingDim(),
		EligibleAssetCount: 1, CompletedVectorCount: 1, IndexedVectorCount: 99, AssetGeneration: 0, IndexedGeneration: 0,
	}
	if err := service.catalog.upsertSemanticBackfillState(context.Background(), sourceKey, profile, stale, builtAt.Add(time.Minute)); err != nil {
		t.Fatalf("upsertSemanticBackfillState(stale) error = %v", err)
	}
	assetGeneration, indexedGeneration, err := service.catalog.semanticIndexGenerations(context.Background(), sourceKey, profile.ModelID())
	if err != nil {
		t.Fatalf("semanticIndexGenerations() error = %v", err)
	}
	if assetGeneration != 1 || indexedGeneration != 0 {
		t.Fatalf("semantic generations after stale upsert = %d/%d, want 1/0", assetGeneration, indexedGeneration)
	}
	var indexedVectorCount int
	var publishedAt string
	if err := service.catalog.db.QueryRow(`SELECT indexed_vector_count, built_at FROM semantic_state
		WHERE source_key = ? AND model_id = ?`, sourceKey, profile.ModelID()).Scan(&indexedVectorCount, &publishedAt); err != nil {
		t.Fatalf("read published semantic state after stale upsert: %v", err)
	}
	if indexedVectorCount != 1 || publishedAt != formatCatalogTime(builtAt) {
		t.Fatalf("published semantic state after stale upsert = count %d built %q, want 1 and %q", indexedVectorCount, publishedAt, formatCatalogTime(builtAt))
	}

	nowText := formatCatalogTime(builtAt.Add(2 * time.Minute))
	result, err := service.catalog.db.Exec(`INSERT INTO semantic_index_jobs (
		source_key, model_id, vector_space_id, embedding_dim, status, priority, attempts,
		scheduled_at, started_at, lease_expires_at, created_at, updated_at
	) VALUES (?, ?, ?, ?, 'running', 100, 1, ?, ?, ?, ?, ?)`,
		sourceKey, profile.ModelID(), profile.VectorSpaceID(), profile.EmbeddingDim(), nowText, nowText, nowText, nowText, nowText)
	if err != nil {
		t.Fatalf("insert running semantic index job: %v", err)
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("semantic job LastInsertId() error = %v", err)
	}
	job := &semanticIndexJob{ID: jobID, SourceKey: sourceKey, ModelID: profile.ModelID(), VectorSpaceID: profile.VectorSpaceID(), EmbeddingDim: profile.EmbeddingDim(), Attempts: 1, AssetGeneration: 0}
	if err := service.catalog.finalizeSemanticIndexJob(context.Background(), job, profile, stale, builtAt.Add(3*time.Minute)); err == nil {
		t.Fatal("finalizeSemanticIndexJob(stale generation) error = nil")
	}
	assetGeneration, indexedGeneration, err = service.catalog.semanticIndexGenerations(context.Background(), sourceKey, profile.ModelID())
	if err != nil {
		t.Fatalf("semanticIndexGenerations(after finalization) error = %v", err)
	}
	var jobStatus string
	if err := service.catalog.db.QueryRow(`SELECT status FROM semantic_index_jobs WHERE id = ?`, jobID).Scan(&jobStatus); err != nil {
		t.Fatalf("read semantic job status: %v", err)
	}
	if assetGeneration != 1 || indexedGeneration != 0 || jobStatus != semanticIndexJobStatusRunning {
		t.Fatalf("stale finalization state = generations %d/%d job=%s, want 1/0 running", assetGeneration, indexedGeneration, jobStatus)
	}
}

func TestSemanticIndexPublishConvergesToEmptySnapshot(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	builtAt := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	assets := []semanticAsset{{SourceKey: sourceKey, ID: "asset-a", MediaType: "image", Filename: "asset.jpg", CapturedAt: builtAt, Vector: []float32{1, 0, 0, 0}}}
	service := newCatalogSemanticSearchBinaryFixture(t, sourceKey, assets, builtAt)
	profile := testImageSemanticProfile{}
	if _, err := service.catalog.db.Exec(`UPDATE semantic_state SET asset_generation = 0, indexed_generation = 0
		WHERE source_key = ? AND model_id = ?`, sourceKey, profile.ModelID()); err != nil {
		t.Fatalf("reset semantic fixture generations: %v", err)
	}
	oldPath := service.catalog.semanticBinaryIndexPath(sourceKey, profile, 0)
	if _, err := service.catalog.db.Exec(`UPDATE catalog_assets SET visibility_status = 'missing'
		WHERE source_key = ? AND upstream_asset_id = 'asset-a'`, sourceKey); err != nil {
		t.Fatalf("hide final semantic asset: %v", err)
	}
	if enqueued, err := service.catalog.ReconcileSemanticIndexJobs(context.Background(), []string{sourceKey}, profile, false, time.Now().UTC()); err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs(empty) error = %v", err)
	} else if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs(empty) = %d, want 1", enqueued)
	}
	publish, err := service.catalog.PublishNextSemanticIndexJob(context.Background(), []string{sourceKey}, profile, time.Now().UTC())
	if err != nil {
		t.Fatalf("PublishNextSemanticIndexJob(empty) error = %v", err)
	}
	if !publish.Published || publish.IndexedVectorCount != 0 || publish.Status.Status != semanticBackfillStatusReady || publish.Status.AssetGeneration != publish.Status.IndexedGeneration {
		t.Fatalf("empty semantic publish = %#v, want current ready empty snapshot", publish)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old semantic binary stat error = %v, want removed", err)
	}
	emptyPath := service.catalog.semanticBinaryIndexPath(sourceKey, profile, publish.Status.IndexedGeneration)
	if info, err := os.Stat(emptyPath); err != nil || info.Size() != semanticBinaryIndexHeaderBytes {
		t.Fatalf("empty semantic generation stat = info:%v err:%v, want durable header-only generation", info, err)
	}
	manifest, err := readSemanticBinaryActiveManifest(service.catalog.semanticBinaryActiveManifestPath(sourceKey, profile))
	if err != nil || manifest.Header.AssetGeneration != publish.Status.IndexedGeneration || manifest.Header.NodeCount != 0 {
		t.Fatalf("empty semantic active manifest = %#v err:%v", manifest, err)
	}
	var indexedVectorCount int
	var assetGeneration, indexedGeneration int64
	if err := service.catalog.db.QueryRow(`SELECT indexed_vector_count, asset_generation, indexed_generation FROM semantic_state
		WHERE source_key = ? AND model_id = ?`, sourceKey, profile.ModelID()).Scan(&indexedVectorCount, &assetGeneration, &indexedGeneration); err != nil {
		t.Fatalf("read empty semantic state: %v", err)
	}
	if indexedVectorCount != 0 || assetGeneration != indexedGeneration {
		t.Fatalf("empty semantic state = count %d generations %d/%d, want 0 and current", indexedVectorCount, assetGeneration, indexedGeneration)
	}
}

func TestCatalogSemanticReconcileRepublishesMissingSemanticBinaryIndex(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	service.client = immichMirrorTestClientWithImages(t)

	ctx := context.Background()
	if _, err := service.SyncMirror(ctx, MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}
	profile := testImageSemanticProfile{}
	if _, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, profile, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: service,
		MaxAssets:   10,
	}); err != nil {
		t.Fatalf("BackfillSemanticVectors() error = %v", err)
	}
	if enqueued, err := service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, false, time.Now().UTC()); err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs(initial) error = %v", err)
	} else if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs(initial) = %d, want one publish job", enqueued)
	}
	if publish, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, time.Now().UTC()); err != nil {
		t.Fatalf("PublishNextSemanticIndexJob(initial) error = %v", err)
	} else if !publish.Published || publish.Status.Status != semanticBackfillStatusReady || publish.IndexedVectorCount != 2 {
		t.Fatalf("initial publish result = %#v", publish)
	}
	assertSemanticBinaryIndexesExist(t, service.catalog, sourceKey, profile)
	if err := os.Remove(currentSemanticBinaryIndexPath(t, service.catalog, sourceKey, profile)); err != nil {
		t.Fatalf("remove semantic binary index: %v", err)
	}
	assertSemanticBinaryIndexMissing(t, service.catalog, sourceKey, profile)
	if traversal, ok, err := service.catalog.openSemanticIndexTraversal(
		ctx,
		sourceKey,
		profile,
		[]float32{1, 0, 0, 0},
	); err != nil {
		t.Fatalf("openSemanticIndexTraversal(missing binary) error = %v", err)
	} else if !ok || traversal == nil || traversal.Semantic.Status != semanticBackfillStatusIndexing || !traversal.Exhausted {
		t.Fatalf("openSemanticIndexTraversal(missing binary) ok=%v traversal=%#v, want stale ready state to become exhausted indexing", ok, traversal)
	}

	status, err := service.catalog.SemanticBackfillStatus(ctx, sourceKey, SemanticModelProfileStatus{
		ModelID:       profile.ModelID(),
		VectorSpaceID: profile.VectorSpaceID(),
		EmbeddingDim:  profile.EmbeddingDim(),
		Role:          semanticModelRoleCandidate,
		ProfileKind:   profile.ProfileKind(),
		InputKind:     profile.InputKind(),
		ModelPack:     profile.ModelPackStatus(),
	})
	if err != nil {
		t.Fatalf("SemanticBackfillStatus() error = %v", err)
	}
	if status.IndexedVectorCount != status.CompletedVectorCount || status.CompletedVectorCount != 2 {
		t.Fatalf("SemanticBackfillStatus() = %#v, want DB state to look fully indexed", status)
	}
	needed, workCount, err := service.catalog.SemanticIndexPublishNeeded(ctx, []string{sourceKey}, profile, false)
	if err != nil {
		t.Fatalf("SemanticIndexPublishNeeded(missing binary) error = %v", err)
	}
	if !needed || workCount <= 0 {
		t.Fatalf("SemanticIndexPublishNeeded(missing binary) needed=%t work=%d, want repair publish work", needed, workCount)
	}

	enqueued, err := service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs(missing binary) error = %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs(missing binary) = %d, want one publish job", enqueued)
	}
	publish, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, time.Now().UTC())
	if err != nil {
		t.Fatalf("PublishNextSemanticIndexJob(republish) error = %v", err)
	}
	if !publish.Published || publish.Status.Status != semanticBackfillStatusReady || publish.IndexedVectorCount != 2 {
		t.Fatalf("republish result = %#v", publish)
	}
	assertSemanticBinaryIndexesExist(t, service.catalog, sourceKey, profile)
}

func TestCatalogSemanticBackfillCanPublishReadyVectorsWithoutGeneratingMore(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	service.client = immichMirrorTestClientWithImages(t)

	ctx := context.Background()
	if _, err := service.SyncMirror(ctx, MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}
	first, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, testImageSemanticProfile{}, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: service,
		MaxAssets:   1,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors(first) error = %v", err)
	}
	if first.ProcessedVectorCount != 1 || first.Status.CompletedVectorCount != 1 || first.Status.IndexedVectorCount != 0 {
		t.Fatalf("first backfill result = %#v", first)
	}

	profile := testImageSemanticProfile{}
	enqueued, err := service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, true, time.Now().UTC())
	if err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs() error = %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs() = %d, want one partial publish job", enqueued)
	}
	publishStartedAt := time.Now().UTC()
	publish, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, publishStartedAt)
	if err != nil {
		t.Fatalf("PublishNextSemanticIndexJob(partial) error = %v", err)
	}
	if !publish.Published ||
		publish.Status.CompletedVectorCount != 1 ||
		publish.Status.IndexedVectorCount != 1 ||
		publish.IndexedVectorCount != 1 {
		t.Fatalf("partial publish result = %#v", publish)
	}
	if publish.Status.LastPublishedAt == nil {
		t.Fatalf("partial publish LastPublishedAt = nil, want publish timestamp")
	}
	if publish.CompletedAt.Before(publishStartedAt) {
		t.Fatalf("partial publish CompletedAt = %s, want not before publish start %s", publish.CompletedAt, publishStartedAt)
	}
	if publish.Status.LastPublishedAt.Before(publishStartedAt) {
		t.Fatalf("partial publish LastPublishedAt = %s, want completion-based timestamp after %s", *publish.Status.LastPublishedAt, publishStartedAt)
	}
	if !publish.Status.LastPublishedAt.Equal(publish.CompletedAt) {
		t.Fatalf("partial publish LastPublishedAt = %s, want CompletedAt %s", *publish.Status.LastPublishedAt, publish.CompletedAt)
	}

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "beach",
				Mode: QueryModeSemantic,
			},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize semantic search request: %v", err)
	}
	page, err := service.searchCatalogSemanticAssets(ctx, normalized, profile, AssetSearchOptions{})
	if err != nil {
		t.Fatalf("partial semantic SearchAssets() error = %v", err)
	}
	if len(page.Items) != 1 || page.Resolved.Semantic == nil ||
		page.Resolved.Semantic.IndexedVectorCount != 1 ||
		page.Resolved.Semantic.MessageCode != semanticMessageIndexBackfilling {
		t.Fatalf("partial semantic search page = %#v", page)
	}
	second, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, testImageSemanticProfile{}, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: service,
		MaxAssets:   10,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors(second) error = %v", err)
	}
	if second.ProcessedVectorCount != 1 ||
		second.Status.CompletedVectorCount != 2 ||
		second.Status.IndexedVectorCount != 1 {
		t.Fatalf("second backfill result = %#v", second)
	}
	enqueued, err = service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs(complete) error = %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs(complete) = %d, want one publish job", enqueued)
	}
	completePublish, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, time.Now().UTC())
	if err != nil {
		t.Fatalf("PublishNextSemanticIndexJob(complete) error = %v", err)
	}
	if !completePublish.Published ||
		completePublish.Status.CompletedVectorCount != 2 ||
		completePublish.Status.IndexedVectorCount != 2 {
		t.Fatalf("complete publish result = %#v", completePublish)
	}
	assertSemanticBinaryIndexesExist(t, service.catalog, sourceKey, profile)
	page, err = service.searchCatalogSemanticAssets(ctx, normalized, profile, AssetSearchOptions{})
	if err != nil {
		t.Fatalf("complete semantic SearchAssets(fp32 file) error = %v", err)
	}
	if len(page.Items) != 2 || page.Resolved.Semantic == nil ||
		page.Resolved.Semantic.IndexedVectorCount != 2 {
		t.Fatalf("complete semantic search page (fp32 file) = %#v", page)
	}
	if err := os.RemoveAll(filepath.Join(service.catalog.root, semanticBinaryIndexDirName)); err != nil {
		t.Fatalf("remove semantic binary index dir: %v", err)
	}
	check, err := service.catalog.CheckSemanticIndex(ctx, SemanticIndexCheckOptions{
		SourceKey: sourceKey,
		ModelID:   profile.ModelID(),
	})
	if err != nil {
		t.Fatalf("CheckSemanticIndex(missing binary) error = %v", err)
	}
	if check.ErrorCount == 0 {
		t.Fatalf("CheckSemanticIndex(missing binary) = %#v, want an error", check)
	}
	enqueued, err = service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, false, time.Now().UTC())
	if err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs(repair) error = %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs(repair) = %d, want 1", enqueued)
	}
	if _, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, time.Now().UTC()); err != nil {
		t.Fatalf("PublishNextSemanticIndexJob(repair) error = %v", err)
	}
	assertSemanticBinaryIndexesExist(t, service.catalog, sourceKey, profile)
	check, err = service.catalog.CheckSemanticIndex(ctx, SemanticIndexCheckOptions{
		SourceKey: sourceKey,
		ModelID:   profile.ModelID(),
		Deep:      true,
	})
	if err != nil {
		t.Fatalf("CheckSemanticIndex() error = %v", err)
	}
	if check.ErrorCount != 0 || check.SourceCount != 1 || check.CheckedVectorCount != 2 {
		t.Fatalf("CheckSemanticIndex() = %#v, want one clean source with two vectors", check)
	}

	page, err = service.searchCatalogSemanticAssets(ctx, normalized, profile, AssetSearchOptions{})
	if err != nil {
		t.Fatalf("complete semantic SearchAssets() error = %v", err)
	}
	if len(page.Items) != 2 || page.Resolved.Semantic == nil ||
		page.Resolved.Semantic.IndexedVectorCount != 2 {
		t.Fatalf("complete semantic search page = %#v", page)
	}
}

func writeSemanticBinaryIndexFromReadyVectorsForTest(t *testing.T, catalogStore *CatalogStore, ctx context.Context, sourceKey string, profile semanticEmbeddingProfile) {
	t.Helper()
	var generation int64
	if err := catalogStore.db.QueryRowContext(ctx, `SELECT indexed_generation
		FROM semantic_state WHERE source_key = ? AND model_id = ?`, sourceKey, profile.ModelID()).Scan(&generation); err != nil {
		t.Fatalf("read semantic test generation: %v", err)
	}
	writeSemanticBinaryIndexWithBuilderForTest(t, catalogStore, ctx, sourceKey, profile, time.Now().UTC(), generation)
}

func assertSemanticBinaryIndexesExist(t *testing.T, catalogStore *CatalogStore, sourceKey string, profile semanticEmbeddingProfile) {
	t.Helper()
	path := currentSemanticBinaryIndexPath(t, catalogStore, sourceKey, profile)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("semantic binary index fp32 stat error = %v", err)
	}
}

func assertSemanticBinaryIndexMissing(t *testing.T, catalogStore *CatalogStore, sourceKey string, profile semanticEmbeddingProfile) {
	t.Helper()
	path := currentSemanticBinaryIndexPath(t, catalogStore, sourceKey, profile)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("semantic binary index fp32 stat error = %v, want not exist", err)
	}
}

func TestGarbageCollectSemanticModelCorporaRemovesRetiredDerivedState(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	ctx := context.Background()
	now := formatCatalogTime(time.Now().UTC())
	profile := semanticIndexFileProfile{modelID: "retired-model", vectorSpaceID: "retired-space", embeddingDim: 1}
	payloadRoot := filepath.Join(service.catalog.root, semanticVectorPayloadDirName)
	if err := os.MkdirAll(payloadRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll(payload root) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(payloadRoot, "retired.bin"), []byte{0, 0, 0, 0}, 0o600); err != nil {
		t.Fatalf("WriteFile(payload) error = %v", err)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO semantic_vector_payload_batches (batch_id, relative_path, size_bytes, sha256, created_at) VALUES (?, ?, ?, ?, ?)`, []any{"retired-batch", "retired.bin", 4, strings.Repeat("0", 64), now}},
		{`INSERT INTO semantic_state (source_key, model_id, vector_space_id, status, embedding_dim, completed_vector_count, indexed_vector_count, asset_generation, indexed_generation, updated_at) VALUES (?, ?, ?, 'ready', ?, 1, 1, 1, 1, ?)`, []any{sourceKey, profile.ModelID(), profile.VectorSpaceID(), profile.EmbeddingDim(), now}},
		{`INSERT INTO semantic_vectors (source_key, upstream_asset_id, model_id, vector_space_id, embedding_dim, payload_batch_id, vector_offset, vector_length, embedding_input, status, generated_at) VALUES (?, 'asset-1', ?, ?, ?, 'retired-batch', 0, 4, 'preview', 'ready', ?)`, []any{sourceKey, profile.ModelID(), profile.VectorSpaceID(), profile.EmbeddingDim(), now}},
		{`INSERT INTO semantic_index_jobs (source_key, model_id, vector_space_id, embedding_dim, status, scheduled_at, created_at, updated_at) VALUES (?, ?, ?, ?, 'completed', ?, ?, ?)`, []any{sourceKey, profile.ModelID(), profile.VectorSpaceID(), profile.EmbeddingDim(), now, now, now}},
	}
	for _, statement := range statements {
		if _, err := service.catalog.db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed retired semantic corpus: %v", err)
		}
	}
	indexRoot := filepath.Join(service.catalog.root, semanticBinaryIndexDirName)
	if err := os.MkdirAll(indexRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll(index root) error = %v", err)
	}
	base := service.catalog.semanticBinaryIndexBaseName(sourceKey, profile)
	for _, name := range []string{base + ".active.json", base + ".g1.tidx", base + ".g2.build.db"} {
		if err := os.WriteFile(filepath.Join(indexRoot, name), []byte("retired"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}

	removed, err := service.GarbageCollectSemanticModelCorpora(ctx, nil)
	if err != nil {
		t.Fatalf("GarbageCollectSemanticModelCorpora() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("GarbageCollectSemanticModelCorpora() removed = %d, want 1", removed)
	}
	for _, table := range []string{"semantic_vectors", "semantic_state", "semantic_index_jobs", "semantic_vector_payload_batches", "semantic_vector_payload_gc_candidates"} {
		var count int
		if err := service.catalog.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d, want 0", table, count)
		}
	}
	if _, err := os.Stat(filepath.Join(payloadRoot, "retired.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("payload stat error = %v, want not exist", err)
	}
	for _, name := range []string{base + ".active.json", base + ".g1.tidx", base + ".g2.build.db"} {
		if _, err := os.Stat(filepath.Join(indexRoot, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("semantic index artifact %s stat error = %v, want not exist", name, err)
		}
	}
}

func TestCheckSemanticIndexIncludesStateWithZeroReadyVectors(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	ctx := context.Background()
	profile := semanticIndexFileProfile{modelID: "repair-model", vectorSpaceID: "repair-space", embeddingDim: 4}
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation, updated_at
		) VALUES (?, ?, ?, 'indexing', ?, 2, 0, 3, 2, ?)`,
		sourceKey, profile.ModelID(), profile.VectorSpaceID(), profile.EmbeddingDim(), formatCatalogTime(time.Now().UTC())); err != nil {
		t.Fatalf("insert semantic state: %v", err)
	}
	manifestPath := service.catalog.semanticBinaryActiveManifestPath(sourceKey, profile)
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(binary root) error = %v", err)
	}
	if err := os.WriteFile(manifestPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("WriteFile(active manifest) error = %v", err)
	}

	result, err := service.catalog.CheckSemanticIndex(ctx, SemanticIndexCheckOptions{SourceKey: sourceKey, ModelID: profile.ModelID()})
	if err != nil {
		t.Fatalf("CheckSemanticIndex() error = %v", err)
	}
	if result.SourceCount != 1 || result.CheckedVectorCount != 0 || result.ErrorCount < 2 {
		t.Fatalf("CheckSemanticIndex() = %#v, want zero-ready state and stale binary errors", result)
	}

	if _, err := service.catalog.db.ExecContext(ctx, `DELETE FROM semantic_state WHERE source_key = ? AND model_id = ?`, sourceKey, profile.ModelID()); err != nil {
		t.Fatalf("delete semantic state: %v", err)
	}
	manifestRaw, err := json.Marshal(semanticBinaryActiveManifest{
		Header: semanticBinaryIndexHeader{
			SourceKey:     sourceKey,
			ModelID:       profile.ModelID(),
			VectorSpaceID: profile.VectorSpaceID(),
			EmbeddingDim:  profile.EmbeddingDim(),
		},
		FileSize:   semanticBinaryIndexHeaderBytes,
		FileSHA256: strings.Repeat("0", 64),
	})
	if err != nil {
		t.Fatalf("marshal orphan manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, manifestRaw, 0o600); err != nil {
		t.Fatalf("WriteFile(orphan manifest) error = %v", err)
	}
	result, err = service.catalog.CheckSemanticIndex(ctx, SemanticIndexCheckOptions{SourceKey: sourceKey, ModelID: profile.ModelID()})
	if err != nil {
		t.Fatalf("CheckSemanticIndex(orphan manifest) error = %v", err)
	}
	if result.SourceCount != 1 || result.ErrorCount == 0 {
		t.Fatalf("CheckSemanticIndex(orphan manifest) = %#v, want orphan binary error", result)
	}
}

func currentSemanticBinaryIndexPath(t *testing.T, catalogStore *CatalogStore, sourceKey string, profile semanticEmbeddingProfile) string {
	t.Helper()
	_, indexedGeneration, err := catalogStore.semanticIndexGenerations(context.Background(), sourceKey, profile.ModelID())
	if err != nil {
		t.Fatalf("semanticIndexGenerations() error = %v", err)
	}
	return catalogStore.semanticBinaryIndexPath(sourceKey, profile, indexedGeneration)
}

func TestImmichMirrorSyncSkipsHiddenAssets(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	service.client = immichMirrorVisibilityMetadataClient(t)

	result, err := service.SyncMirror(context.Background(), MirrorSyncModeFull)
	if err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}
	if result.FetchedCount != 2 || result.ActiveCount != 2 || result.MissingCount != 0 {
		t.Fatalf("sync result counts = fetched:%d active:%d missing:%d", result.FetchedCount, result.ActiveCount, result.MissingCount)
	}
	timeline, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("timeline search: %v", err)
	}
	if len(timeline.Items) != 2 || timeline.Items[0].ID != "asset-new" || timeline.Items[1].ID != "asset-beach" {
		t.Fatalf("timeline items = %#v", timeline.Items)
	}

}

func TestImmichMirrorSyncEnrichesMetadataFromAssetDetail(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit:    1,
			MetadataDetailLimit: 1,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	detailRequests := 0
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("x-api-key") != "test-key" {
				t.Fatalf("x-api-key = %q, want test-key", r.Header.Get("x-api-key"))
			}
			switch r.URL.Path {
			case "/api/search/metadata":
				return jsonResponse(`{
					"assets": {
						"total": 1,
						"items": [
							{
								"id": "asset-detail",
								"type": "IMAGE",
								"originalFileName": "plain.jpg",
								"fileCreatedAt": "2026-06-01T10:00:00Z",
								"updatedAt": "2026-06-01T10:05:00Z"
							}
						],
						"nextPage": null
					}
				}`), nil
			case "/api/assets/asset-detail":
				detailRequests++
				return jsonResponse(`{
					"id": "asset-detail",
					"type": "IMAGE",
					"originalFileName": "Kyoto Detail.jpg",
					"fileCreatedAt": "2026-06-01T10:00:00Z",
					"updatedAt": "2026-06-01T10:06:00Z",
					"isFavorite": true,
					"exifInfo": {
						"city": "Kyoto",
						"state": "Kyoto",
						"country": "Japan",
						"description": "Kamo River"
					}
				}`), nil
			default:
				t.Fatalf("unexpected request path %s", r.URL.Path)
				return nil, nil
			}
		}),
	}

	result, err := service.SyncMirror(context.Background(), MirrorSyncModeFull)
	if err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}
	if result.FetchedCount != 1 || result.ActiveCount != 1 || detailRequests != 1 {
		t.Fatalf("sync result fetched=%d active=%d detailRequests=%d", result.FetchedCount, result.ActiveCount, detailRequests)
	}
	hybrid, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "Kyoto",
				Mode: QueryModeAuto,
			},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("hybrid search: %v", err)
	}
	if len(hybrid.Items) != 1 || hybrid.Items[0].ID != "asset-detail" || hybrid.Items[0].Filename != "Kyoto Detail.jpg" {
		t.Fatalf("hybrid page = %#v", hybrid)
	}
}

func TestImmichMirrorIncrementalSyncUsesUpdatedAfterAndMergesAssets(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing:    &config.DatasourceIndexingConfig{},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	requestBodies := []map[string]any{}
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/api/search/metadata" {
				t.Fatalf("unexpected request path %s", r.URL.Path)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			requestBodies = append(requestBodies, body)
			if len(requestBodies) == 1 {
				return jsonResponse(`{
					"assets": {
						"total": 1,
						"items": [
							{
								"id": "asset-existing",
								"type": "IMAGE",
								"originalFileName": "existing.jpg",
								"fileCreatedAt": "2026-06-01T10:00:00Z",
								"updatedAt": "2026-06-01T10:05:00Z"
							}
						],
						"nextPage": null
					}
				}`), nil
			}
			return jsonResponse(`{
				"assets": {
					"total": 1,
					"items": [
						{
							"id": "asset-new",
							"type": "IMAGE",
							"originalFileName": "new.jpg",
							"fileCreatedAt": "2026-06-02T10:00:00Z",
							"updatedAt": "2026-06-02T10:05:00Z"
						}
					],
					"nextPage": null
				}
			}`), nil
		}),
	}

	full, err := service.SyncMirror(context.Background(), MirrorSyncModeFull)
	if err != nil {
		t.Fatalf("full SyncMirror() error = %v", err)
	}
	if full.ActiveCount != 1 {
		t.Fatalf("full ActiveCount = %d, want 1", full.ActiveCount)
	}
	incremental, err := service.SyncMirror(context.Background(), MirrorSyncModeIncremental)
	if err != nil {
		t.Fatalf("incremental SyncMirror() error = %v", err)
	}
	if incremental.Mode != MirrorSyncModeIncremental || incremental.FetchedCount != 1 || incremental.ActiveCount != 2 {
		t.Fatalf("incremental result = %#v", incremental)
	}
	if len(requestBodies) != 2 {
		t.Fatalf("request count = %d, want 2", len(requestBodies))
	}
	if _, ok := requestBodies[0]["updatedAfter"]; ok {
		t.Fatalf("full request unexpectedly had updatedAfter: %#v", requestBodies[0])
	}
	if got := requestBodies[1]["updatedAfter"]; got != "2026-06-01T10:05:00Z" {
		t.Fatalf("incremental updatedAfter = %#v, want previous max updatedAt", got)
	}
	timeline, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("timeline search: %v", err)
	}
	if len(timeline.Items) != 2 || timeline.Items[0].ID != "asset-new" || timeline.Items[1].ID != "asset-existing" {
		t.Fatalf("timeline after incremental = %#v", timeline)
	}
}

func TestImmichMirrorIncrementalSyncRunsFullWhenLatestLimitChanges(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	dataDir := t.TempDir()
	requestBodies := []map[string]any{}
	client := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			requestBodies = append(requestBodies, body)
			return jsonResponse(`{
				"assets": {
					"total": 2,
					"items": [
						{
							"id": "asset-newest",
							"type": "IMAGE",
							"originalFileName": "newest.jpg",
							"fileCreatedAt": "2026-06-02T10:00:00Z",
							"updatedAt": "2026-06-02T10:05:00Z"
						},
						{
							"id": "asset-expanded",
							"type": "IMAGE",
							"originalFileName": "expanded.jpg",
							"fileCreatedAt": "2026-06-01T10:00:00Z",
							"updatedAt": "2026-06-01T10:05:00Z"
						}
					],
					"nextPage": null
				}
			}`), nil
		}),
	}

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 1,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	service.client = client
	initial, err := service.SyncDatasourceMirror(context.Background(), sourceKey, MirrorSyncModeFull)
	if err != nil {
		t.Fatalf("initial SyncDatasourceMirror() error = %v", err)
	}
	if initial.FetchedCount != 1 || initial.ActiveCount != 1 || initial.LatestAssetLimit != 1 {
		t.Fatalf("initial sync = %#v, want one active asset at limit 1", initial)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	expandedService, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() after limit change error = %v", err)
	}
	defer expandedService.Close()
	expandedService.client = client
	expanded, err := expandedService.SyncDatasourceMirror(context.Background(), sourceKey, MirrorSyncModeIncremental)
	if err != nil {
		t.Fatalf("expanded SyncDatasourceMirror() error = %v", err)
	}
	if expanded.Mode != MirrorSyncModeFull || expanded.FetchedCount != 2 || expanded.ActiveCount != 2 || expanded.LatestAssetLimit != 2 {
		t.Fatalf("expanded sync = %#v, want full sync with two active assets at limit 2", expanded)
	}
	if len(requestBodies) != 2 {
		t.Fatalf("request count = %d, want 2", len(requestBodies))
	}
	if _, ok := requestBodies[1]["updatedAfter"]; ok {
		t.Fatalf("expanded sync unexpectedly used incremental cursor: %#v", requestBodies[1])
	}
}

func TestCatalogSemanticAutoSearchFallsBackToFilenameWhenSemanticMissing(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing:    &config.DatasourceIndexingConfig{},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	startedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	_, err = service.catalog.ReplaceFull(context.Background(), "1111111111111111", []ImmichMirrorAsset{
		{
			UpstreamAssetID: "asset-new",
			MediaType:       "image",
			Filename:        "new.jpg",
			CapturedAt:      startedAt,
		},
		{
			UpstreamAssetID: "asset-beach",
			MediaType:       "image",
			Filename:        "Summer Beach.JPG",
			CapturedAt:      startedAt.Add(-time.Hour),
		},
	}, 0, startedAt)
	if err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}

	capabilities := service.SearchCapabilities()
	if capabilities.Semantic == nil || capabilities.Semantic.Status != "missing" || capabilities.Semantic.MessageCode != semanticMessageIndexMissing {
		t.Fatalf("missing semantic capabilities = %#v", capabilities.Semantic)
	}
	if capabilities.Semantic.ModelPack != nil {
		t.Fatalf("missing semantic model pack = %#v, want no synthetic built-in pack", capabilities.Semantic.ModelPack)
	}

	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "beach",
				Mode: QueryModeAuto,
			},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("auto search fallback: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != "asset-beach" {
		t.Fatalf("auto fallback page = %#v", page)
	}
	if page.Resolved.QueryMode != QueryModeFilename || page.Resolved.Semantic == nil || page.Resolved.Semantic.Eligible || page.Resolved.Semantic.FallbackQueryMode != QueryModeFilename {
		t.Fatalf("auto fallback resolved = %#v", page.Resolved)
	}
	if page.Resolved.Semantic.MessageCode != semanticMessageIndexMissingFallback {
		t.Fatalf("auto fallback semantic message = %q", page.Resolved.Semantic.MessageCode)
	}

	semanticOnly, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "beach",
				Mode: QueryModeSemantic,
			},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("semantic search without model: %v", err)
	}
	if semanticOnly.Total != 0 || len(semanticOnly.Items) != 0 ||
		semanticOnly.Resolved.QueryMode != QueryModeSemantic ||
		semanticOnly.Resolved.Semantic == nil || semanticOnly.Resolved.Semantic.Eligible ||
		semanticOnly.Resolved.Semantic.FallbackQueryMode != "" {
		t.Fatalf("semantic-only missing-model page = %#v", semanticOnly)
	}
}

func TestImmichSemanticImageUsesImmichPreviewForVideoAssets(t *testing.T) {
	t.Parallel()

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing:    &config.DatasourceIndexingConfig{},
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/api/assets/video-asset/thumbnail" {
				t.Fatalf("semantic image path = %s, want video thumbnail path", r.URL.RequestURI())
			}
			if r.URL.Query().Get("size") != detailPreviewSize {
				t.Fatalf("thumbnail size = %q, want %q", r.URL.Query().Get("size"), detailPreviewSize)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/jpeg"},
				},
				Body: io.NopCloser(bytes.NewReader(encodeJPEGForTest(t, 64, 64))),
			}, nil
		}),
	}

	image, err := service.LoadSemanticImage(context.Background(), "1111111111111111", "video-asset")
	if err != nil {
		t.Fatalf("LoadSemanticImage() error = %v", err)
	}
	if image == nil || image.Source != "immich_preview" || image.ContentType != "image/jpeg" || len(image.Bytes) == 0 {
		t.Fatalf("semantic image = %#v", image)
	}
}

func TestImmichSemanticProfileLoadsDetailPreviewInput(t *testing.T) {
	dataDir := t.TempDir()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 2,
		},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	imageRequests := []string{}
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("x-api-key") != "test-key" {
				t.Fatalf("x-api-key = %q, want test-key", r.Header.Get("x-api-key"))
			}
			switch r.URL.Path {
			case "/api/search/metadata":
				return immichMirrorMetadataResponse(), nil
			case "/api/assets/asset-new/thumbnail", "/api/assets/asset-beach/thumbnail":
				if r.URL.Query().Get("size") != detailPreviewSize {
					t.Fatalf("thumbnail size = %q, want %q", r.URL.Query().Get("size"), detailPreviewSize)
				}
				imageRequests = append(imageRequests, strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/thumbnail"), "/api/assets/"))
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": []string{"image/jpeg"},
					},
					Body: io.NopCloser(bytes.NewReader(encodeJPEGForTest(t, 64, 64))),
				}, nil
			default:
				t.Fatalf("unexpected mirror image sync path %s", r.URL.RequestURI())
				return nil, nil
			}
		}),
	}

	_, err = service.SyncMirror(context.Background(), MirrorSyncModeFull)
	if err != nil {
		t.Fatalf("SyncMirror() error = %v", err)
	}
	semanticResult, err := service.catalog.BackfillSemanticVectors(
		context.Background(),
		"1111111111111111",
		testImageSemanticProfile{},
		time.Now().UTC(),
		SemanticBackfillOptions{ImageLoader: service, MaxAssets: 10},
	)
	if err != nil {
		t.Fatalf("BackfillSemanticVectors() error = %v", err)
	}
	if fmt.Sprint(imageRequests) != fmt.Sprint([]string{"asset-new", "asset-beach"}) {
		t.Fatalf("image requests = %#v", imageRequests)
	}
	if semanticResult.Status.ModelID != "test-image-profile" || semanticResult.Status.CompletedVectorCount != 2 {
		t.Fatalf("image semantic status = %#v", semanticResult.Status)
	}
}

func immichMirrorTestClient(t *testing.T) *http.Client {
	t.Helper()

	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/api/search/metadata" {
				t.Fatalf("unexpected mirror sync path %s", r.URL.Path)
			}
			if r.Header.Get("x-api-key") != "test-key" {
				t.Fatalf("x-api-key = %q, want test-key", r.Header.Get("x-api-key"))
			}
			var request struct {
				Page  int    `json:"page"`
				Size  int    `json:"size"`
				Order string `json:"order"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode mirror request: %v", err)
			}
			if request.Page != 1 || request.Size != maxPageSize || request.Order != SortDirectionDesc {
				t.Fatalf("mirror request = %#v", request)
			}
			return immichMirrorMetadataResponse(), nil
		}),
	}
}

func immichMirrorLargeTestClient(t *testing.T, totalAssets int) *http.Client {
	t.Helper()

	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/api/search/metadata" {
				t.Fatalf("unexpected mirror sync path %s", r.URL.Path)
			}
			if r.Header.Get("x-api-key") != "test-key" {
				t.Fatalf("x-api-key = %q, want test-key", r.Header.Get("x-api-key"))
			}
			var request struct {
				Page  int    `json:"page"`
				Size  int    `json:"size"`
				Order string `json:"order"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode mirror request: %v", err)
			}
			if request.Page <= 0 || request.Size != maxPageSize || request.Order != SortDirectionDesc {
				t.Fatalf("mirror request = %#v", request)
			}
			offset := (request.Page - 1) * request.Size
			end := min(offset+request.Size, totalAssets)
			items := []map[string]any{}
			baseTime := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
			for index := offset; index < end; index++ {
				capturedAt := baseTime.Add(-time.Duration(index) * time.Minute).Format(time.RFC3339Nano)
				items = append(items, map[string]any{
					"id":               fmt.Sprintf("asset-large-%05d", index),
					"type":             "IMAGE",
					"originalFileName": fmt.Sprintf("large-%05d.jpg", index),
					"fileCreatedAt":    capturedAt,
					"updatedAt":        capturedAt,
				})
			}
			var nextPage any
			if end < totalAssets {
				nextPage = request.Page + 1
			}
			payload := map[string]any{
				"assets": map[string]any{
					"total":    len(items),
					"items":    items,
					"nextPage": nextPage,
				},
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal large mirror response: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader(body)),
			}, nil
		}),
	}
}

func immichMirrorTestClientWithImages(t *testing.T) *http.Client {
	t.Helper()

	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("x-api-key") != "test-key" {
				t.Fatalf("x-api-key = %q, want test-key", r.Header.Get("x-api-key"))
			}
			switch r.URL.Path {
			case "/api/search/metadata":
				var request struct {
					Page  int    `json:"page"`
					Size  int    `json:"size"`
					Order string `json:"order"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatalf("decode mirror request: %v", err)
				}
				if request.Page != 1 || request.Size != maxPageSize || request.Order != SortDirectionDesc {
					t.Fatalf("mirror request = %#v", request)
				}
				return immichMirrorMetadataResponse(), nil
			case "/api/assets/asset-new/thumbnail", "/api/assets/asset-beach/thumbnail":
				if r.URL.Query().Get("size") != detailPreviewSize {
					t.Fatalf("thumbnail size = %q, want %q", r.URL.Query().Get("size"), detailPreviewSize)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": []string{"image/jpeg"},
					},
					Body: io.NopCloser(bytes.NewReader(encodeJPEGForTest(t, 64, 64))),
				}, nil
			default:
				t.Fatalf("unexpected mirror sync path %s", r.URL.RequestURI())
				return nil, nil
			}
		}),
	}
}

func immichMirrorVisibilityMetadataClient(t *testing.T) *http.Client {
	t.Helper()

	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/api/search/metadata" {
				t.Fatalf("unexpected mirror sync path %s", r.URL.Path)
			}
			if r.Header.Get("x-api-key") != "test-key" {
				t.Fatalf("x-api-key = %q, want test-key", r.Header.Get("x-api-key"))
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(strings.NewReader(`{
					"assets": {
						"total": 6,
						"items": [
							{
								"id": "asset-archived",
								"type": "IMAGE",
								"originalFileName": "archived.jpg",
								"fileCreatedAt": "2026-06-03T10:00:00Z",
								"updatedAt": "2026-06-03T10:05:00Z",
								"isArchived": true
							},
							{
								"id": "asset-hidden",
								"type": "IMAGE",
								"originalFileName": "hidden.jpg",
								"fileCreatedAt": "2026-06-03T09:00:00Z",
								"updatedAt": "2026-06-03T09:05:00Z",
								"visibility": "hidden"
							},
							{
								"id": "asset-new",
								"type": "IMAGE",
								"originalFileName": "new.jpg",
								"fileCreatedAt": "2026-06-02T10:00:00Z",
								"updatedAt": "2026-06-02T10:05:00Z",
								"isFavorite": true
							},
							{
								"id": "asset-trashed",
								"type": "VIDEO",
								"originalFileName": "trashed.mov",
								"fileCreatedAt": "2026-06-01T10:00:00Z",
								"updatedAt": "2026-06-01T10:05:00Z",
								"isTrashed": true
							},
							{
								"id": "asset-trash-date",
								"type": "IMAGE",
								"originalFileName": "trash-date.jpg",
								"fileCreatedAt": "2026-06-01T09:00:00Z",
								"updatedAt": "2026-06-01T09:05:00Z",
								"trashedAt": "2026-06-01T09:10:00Z"
							},
							{
								"id": "asset-beach",
								"type": "IMAGE",
								"originalFileName": "Summer Beach.JPG",
								"fileCreatedAt": "2026-05-01T10:00:00Z",
								"updatedAt": "2026-05-01T10:05:00Z",
								"exifInfo": {
									"city": "Kyoto",
									"state": "Kyoto",
									"country": "Japan",
									"description": "Kamo River"
								}
							}
						],
						"nextPage": null
					}
				}`)),
			}, nil
		}),
	}
}

func immichMirrorMetadataResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(`{
			"assets": {
				"total": 3,
				"items": [
					{
						"id": "asset-new",
						"type": "IMAGE",
						"originalFileName": "new.jpg",
						"fileCreatedAt": "2026-06-01T10:00:00Z",
						"updatedAt": "2026-06-01T10:05:00Z"
					},
					{
						"id": "asset-beach",
						"type": "IMAGE",
						"originalFileName": "Summer Beach.JPG",
						"fileCreatedAt": "2026-05-01T10:00:00Z",
						"updatedAt": "2026-05-01T10:05:00Z"
					},
					{
						"id": "asset-old",
						"type": "VIDEO",
						"originalFileName": "old.mov",
						"fileCreatedAt": "2025-05-01T10:00:00Z",
						"updatedAt": "2025-05-01T10:05:00Z",
						"duration": "00:00:03.000"
					}
				],
				"nextPage": null
			}
		}`)),
	}
}

type testImageSemanticProfile struct{}

func (testImageSemanticProfile) ModelID() string { return "test-image-profile" }

func (testImageSemanticProfile) VectorSpaceID() string { return "test-image-profile/d4" }

func (testImageSemanticProfile) EmbeddingDim() int { return 4 }

func (testImageSemanticProfile) ProfileKind() string { return semanticProfileKindModelPack }

func (testImageSemanticProfile) InputKind() string { return semanticInputKindImage }

func (testImageSemanticProfile) ModelPackStatus() *SemanticModelPackStatus {
	return &SemanticModelPackStatus{
		ID:        "test-image-profile",
		Name:      "Test image profile",
		Status:    "installed",
		Source:    "test",
		InputKind: semanticInputKindImage,
		Installed: true,
	}
}

func (testImageSemanticProfile) EmbedSemanticAsset(_ context.Context, input semanticAssetEmbeddingInput) (semanticEmbeddingResult, error) {
	if input.Image == nil || len(input.Image.Bytes) == 0 {
		return semanticEmbeddingResult{}, fmt.Errorf("image input is required")
	}
	vector := []float32{1, 0, 0, 0}
	if strings.Contains(strings.ToLower(input.Asset.Filename), "beach") {
		vector = []float32{0, 1, 0, 0}
	}
	return semanticEmbeddingResult{
		Vector: vector,
		Input:  input.Image.Source + ":" + input.Image.ContentType,
	}, nil
}

func (testImageSemanticProfile) EmbedText(_ context.Context, text string) ([]float32, error) {
	if strings.Contains(strings.ToLower(text), "beach") {
		return []float32{0, 1, 0, 0}, nil
	}
	return []float32{1, 0, 0, 0}, nil
}

type staticSemanticImageLoader struct{}

func (staticSemanticImageLoader) LoadSemanticImage(context.Context, string, string) (*semanticImageEmbeddingInput, error) {
	return &semanticImageEmbeddingInput{
		Bytes:       []byte("test-image"),
		ContentType: "image/jpeg",
		Source:      "test",
	}, nil
}

type selectiveFailSemanticImageLoader struct {
	failAssetID string
}

func (l selectiveFailSemanticImageLoader) LoadSemanticImage(_ context.Context, _ string, assetID string) (*semanticImageEmbeddingInput, error) {
	if assetID == l.failAssetID {
		return nil, fmt.Errorf("%w: broken semantic image", ErrSemanticAssetInput)
	}
	return staticSemanticImageLoader{}.LoadSemanticImage(context.Background(), "", assetID)
}

type failingSemanticImageLoader struct {
	err error
}

func (l failingSemanticImageLoader) LoadSemanticImage(context.Context, string, string) (*semanticImageEmbeddingInput, error) {
	return nil, l.err
}

type runtimeFailSemanticProfile struct {
	testImageSemanticProfile
}

func (runtimeFailSemanticProfile) ModelID() string {
	return "test-runtime-failure"
}

func (runtimeFailSemanticProfile) VectorSpaceID() string {
	return "test-runtime-failure/d4"
}

func (runtimeFailSemanticProfile) EmbedSemanticAsset(context.Context, semanticAssetEmbeddingInput) (semanticEmbeddingResult, error) {
	return semanticEmbeddingResult{}, errors.New("semantic runtime unavailable")
}

func TestRenderHostedImageRejectsInvalidForcedJPEGInput(t *testing.T) {
	_, _, _, err := renderHostedImage([]byte("not-an-image"), hostedImageProfile{
		MaxEdgePixels: 512,
		MaxBytes:      1024,
		JPEGQualities: []int{80},
		ForceJPEG:     true,
	})
	if err == nil {
		t.Fatal("renderHostedImage() error = nil, want invalid image rejection")
	}
}

func TestStaticDemoCatalogAndMedia(t *testing.T) {
	t.Parallel()

	bundleDir := t.TempDir()
	assetDir := filepath.Join(bundleDir, "assets", "demo-0001")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	imageBody := encodeJPEGForTest(t, 320, 240)
	for _, name := range []string{"preview.jpg", "detail_preview.jpg", "original.jpg"} {
		if err := os.WriteFile(filepath.Join(assetDir, name), imageBody, 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	manifest := `{
		"version": 1,
		"assets": [
			{
				"id": "demo-0001",
				"type": "IMAGE",
				"originalFileName": "demo-0001.jpg",
				"fileCreatedAt": "2026-01-01T12:00:00Z",
				"previewPath": "assets/demo-0001/preview.jpg",
				"detailPreviewPath": "assets/demo-0001/detail_preview.jpg",
				"originalPath": "assets/demo-0001/original.jpg"
			}
		]
	}`
	manifestPath := filepath.Join(bundleDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("WriteFile(manifest) error = %v", err)
	}
	service := NewService([]config.DatasourceConfig{{
		Name: "Review Demo",
		Kind: config.DatasourceKindStaticDemo,
		URL:  bundleDir,
	}})
	if !service.Ready() {
		t.Fatal("Ready() = false, want directory-backed static demo ready")
	}

	page, err := service.CatalogPage(0, 60)
	if err != nil {
		t.Fatalf("CatalogPage() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("page = %#v, want one static asset", page)
	}
	if !page.Items[0].CapturedAt.Equal(time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("CapturedAt = %v", page.Items[0].CapturedAt)
	}
	asset, err := service.Asset("demo-0001")
	if err != nil {
		t.Fatalf("Asset() error = %v", err)
	}
	if asset.ID != "demo-0001" || asset.Type != "image" || asset.Filename != "demo-0001.jpg" {
		t.Fatalf("Asset() = %#v", asset)
	}

	response, err := service.Preview(nil, "demo-0001")
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", response.Header.Get("Content-Type"))
	}
}

func TestMediaRoutesUseRequestedDatasourceSourceKey(t *testing.T) {
	t.Parallel()

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("primary datasource was called for %s", r.URL.Path)
	}))
	t.Cleanup(primary.Close)
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/assets/asset-secondary/original" {
			t.Fatalf("secondary path = %q, want original asset path", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "secondary-key" {
			t.Fatalf("secondary x-api-key = %q, want secondary-key", r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("secondary-original"))
	}))
	t.Cleanup(secondary.Close)

	service := NewService([]config.DatasourceConfig{
		{
			SourceKey:   "1111111111111111",
			Name:        "Primary Immich",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         primary.URL,
			AccessToken: "primary-key",
		},
		{
			SourceKey:   "2222222222222222",
			Name:        "Secondary Immich",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         secondary.URL,
			AccessToken: "secondary-key",
		},
	})
	response, err := service.OriginalFromSource(httptest.NewRequest(http.MethodGet, "http://timich.test/original", nil), "2222222222222222", "asset-secondary")
	if err != nil {
		t.Fatalf("OriginalFromSource() error = %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(body) != "secondary-original" {
		t.Fatalf("body=%q, want secondary datasource response", string(body))
	}
}

func TestStaticDemoBrokenManifestIsNotReady(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name: "Review Demo",
		Kind: config.DatasourceKindStaticDemo,
		URL:  filepath.Join(t.TempDir(), "missing-bundle"),
	}})
	if service.Ready() {
		t.Fatal("Ready() = true, want broken static demo datasource to be not ready")
	}
	if _, err := service.CatalogPage(0, 60); !errors.Is(err, ErrNoDatasourceConfigured) {
		t.Fatalf("CatalogPage() error = %v, want ErrNoDatasourceConfigured", err)
	}
}

func TestStaticDemoOriginalSupportsByteRange(t *testing.T) {
	t.Parallel()

	bundleDir := t.TempDir()
	assetDir := filepath.Join(bundleDir, "assets", "demo-0001")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	for name, body := range map[string][]byte{
		"preview.jpg":        encodeJPEGForTest(t, 32, 32),
		"detail_preview.jpg": encodeJPEGForTest(t, 64, 64),
		"original.mp4":       []byte("0123456789"),
	} {
		if err := os.WriteFile(filepath.Join(assetDir, name), body, 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	manifest := `{
		"version": 1,
		"assets": [
			{
				"id": "demo-0001",
				"type": "VIDEO",
				"originalFileName": "demo-0001.mp4",
				"fileCreatedAt": "2026-01-01T12:00:00Z",
				"duration": "0:00:10.000000",
				"previewPath": "assets/demo-0001/preview.jpg",
				"detailPreviewPath": "assets/demo-0001/detail_preview.jpg",
				"originalPath": "assets/demo-0001/original.mp4"
			}
		]
	}`
	manifestPath := filepath.Join(bundleDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("WriteFile(manifest) error = %v", err)
	}
	service := NewService([]config.DatasourceConfig{{
		Name: "Review Demo",
		Kind: config.DatasourceKindStaticDemo,
		URL:  manifestPath,
	}})
	request, err := http.NewRequest(http.MethodGet, "http://agent.test/v1/assets/demo-0001/original", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Range", "bytes=2-5")

	response, err := service.Original(request, "demo-0001")
	if err != nil {
		t.Fatalf("Original() error = %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if response.StatusCode != http.StatusPartialContent {
		t.Fatalf("StatusCode = %d, want 206", response.StatusCode)
	}
	if string(body) != "2345" {
		t.Fatalf("body = %q, want 2345", string(body))
	}
	if response.Header.Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q", response.Header.Get("Content-Range"))
	}
}

func TestOriginalForwardsRangeHeaders(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	var receivedMethod string
	var receivedRange string
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			receivedMethod = r.Method
			receivedRange = r.Header.Get("Range")
			return &http.Response{
				StatusCode: http.StatusPartialContent,
				Header: http.Header{
					"Content-Type":   []string{"video/mp4"},
					"Accept-Ranges":  []string{"bytes"},
					"Content-Range":  []string{"bytes 0-1/100"},
					"Content-Length": []string{"2"},
				},
				Body: io.NopCloser(strings.NewReader("ok")),
			}, nil
		}),
	}

	request, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/original", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Range", "bytes=0-1")

	response, err := service.Original(request, "asset-123")
	if err != nil {
		t.Fatalf("original proxy: %v", err)
	}
	defer response.Body.Close()

	if receivedMethod != http.MethodGet {
		t.Fatalf("expected method GET, got %s", receivedMethod)
	}
	if receivedRange != "bytes=0-1" {
		t.Fatalf("expected Range header to be forwarded, got %q", receivedRange)
	}
	if response.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected status 206, got %d", response.StatusCode)
	}
	if response.Header.Get("Content-Range") != "bytes 0-1/100" {
		t.Fatalf("expected Content-Range header to survive, got %q", response.Header.Get("Content-Range"))
	}
}

func TestImmichMediaNormalizesUpstreamFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		load       func(*Service, *http.Request, string) (*UpstreamMediaResponse, error)
		wantErr    error
		wantStatus int
	}{
		{name: "original upstream unauthorized", statusCode: http.StatusUnauthorized, load: (*Service).Original, wantErr: ErrDatasourceUnavailable},
		{name: "original upstream forbidden", statusCode: http.StatusForbidden, load: (*Service).Original, wantErr: ErrDatasourceUnavailable},
		{name: "original upstream unavailable", statusCode: http.StatusServiceUnavailable, load: (*Service).Original, wantErr: ErrDatasourceUnavailable},
		{name: "profile upstream unauthorized", statusCode: http.StatusUnauthorized, load: (*Service).Preview, wantErr: ErrDatasourceUnavailable},
		{name: "profile upstream not found", statusCode: http.StatusNotFound, load: (*Service).Preview, wantErr: ErrAssetNotFound},
		{name: "original upstream not found", statusCode: http.StatusNotFound, load: (*Service).Original, wantErr: ErrAssetNotFound},
		{name: "original unsatisfied range", statusCode: http.StatusRequestedRangeNotSatisfiable, load: (*Service).Original, wantStatus: http.StatusRequestedRangeNotSatisfiable},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service := NewService([]config.DatasourceConfig{{
				Name:        "test",
				Kind:        config.DatasourceKindImmichIndexed,
				URL:         "http://immich.test",
				AccessToken: "test-key",
			}})
			service.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tt.statusCode, Header: make(http.Header), Body: http.NoBody}, nil
			})}
			request := httptest.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/original", nil)
			response, err := tt.load(service, request, "asset-123")
			if tt.wantErr != nil {
				if response != nil {
					response.Body.Close()
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("load error = %v", err)
			}
			defer response.Body.Close()
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, tt.wantStatus)
			}
		})
	}
}

func TestPreviewClassifiesUpstreamBodyReadFailureAsDatasourceUnavailable(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})
	service.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/jpeg"}},
			Body:       io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF)),
		}, nil
	})}

	response, err := service.Preview(httptest.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/preview", nil), "asset-123")
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, ErrDatasourceUnavailable) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Preview() body read error = %v, want datasource unavailable wrapping body failure", err)
	}
	if !immichMediaFallbackSourceFailure(err) {
		t.Fatalf("Preview() body read error = %v, want source failure classification", err)
	}
}

func TestPreviewUsesDeterministicThumbnailProfile(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	var requestedPaths []string
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requestedPaths = append(requestedPaths, r.URL.RequestURI())
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/jpeg"},
				},
				Body: io.NopCloser(strings.NewReader("ok")),
			}, nil
		}),
	}

	localRequest, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/preview", nil)
	if err != nil {
		t.Fatalf("new local request: %v", err)
	}
	if _, err := service.Preview(localRequest, "asset-123"); err != nil {
		t.Fatalf("local preview proxy: %v", err)
	}

	hostedRequest, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/preview", nil)
	if err != nil {
		t.Fatalf("new hosted request: %v", err)
	}
	hostedRequest.Header.Set("X-Timich-Hosted-Base-URL", "https://timich.runo.jp")
	if _, err := service.Preview(hostedRequest, "asset-123"); err != nil {
		t.Fatalf("hosted asset preview proxy: %v", err)
	}

	if len(requestedPaths) != 2 {
		t.Fatalf("expected 2 preview requests, got %d", len(requestedPaths))
	}
	if requestedPaths[0] != "/api/assets/asset-123/thumbnail?size=thumbnail" {
		t.Fatalf("expected local preview path, got %q", requestedPaths[0])
	}
	if requestedPaths[1] != "/api/assets/asset-123/thumbnail?size=thumbnail" {
		t.Fatalf("expected hosted asset preview path, got %q", requestedPaths[1])
	}
}

func TestPreviewReturnsSmallPreviewUnchanged(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeJPEGForTest(t, 320, 240)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.RequestURI() != "/api/assets/asset-123/thumbnail?size=thumbnail" {
				t.Fatalf("unexpected request path %q", r.URL.RequestURI())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/jpeg"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.Preview(newHostedAssetPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("hosted asset preview proxy: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read hosted asset preview body: %v", err)
	}
	if !bytes.Equal(body, originalBody) {
		t.Fatalf("expected small hosted asset preview passthrough")
	}
}

func TestPreviewResizesLargePreviewTo512(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeJPEGForTest(t, 1200, 800)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/jpeg"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.Preview(newHostedAssetPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("hosted asset preview proxy: %v", err)
	}
	defer response.Body.Close()

	encodedBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read hosted asset preview body: %v", err)
	}
	if len(encodedBody) > previewMaxBytes {
		t.Fatalf("hosted asset preview bytes = %d, want <= %d", len(encodedBody), previewMaxBytes)
	}
	previewImage, _, err := image.Decode(bytes.NewReader(encodedBody))
	if err != nil {
		t.Fatalf("decode hosted asset preview response: %v", err)
	}
	if previewImage.Bounds().Dx() != 512 || previewImage.Bounds().Dy() != 341 {
		t.Fatalf("expected 512x341 hosted asset preview, got %dx%d", previewImage.Bounds().Dx(), previewImage.Bounds().Dy())
	}
}

func TestDetailPreviewLocalUsesPreviewProfile(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeJPEGForTest(t, 1440, 960)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.RequestURI() != "/api/assets/asset-123/thumbnail?size=preview" {
				t.Fatalf("unexpected request path %q", r.URL.RequestURI())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type":        []string{"image/jpeg"},
					"Content-Disposition": []string{"inline; filename*=UTF-8''asset-123.jpg"},
					"Cache-Control":       []string{"private, max-age=86400"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	request, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/detail_preview", nil)
	if err != nil {
		t.Fatalf("new detail preview request: %v", err)
	}
	response, err := service.DetailPreview(request, "asset-123")
	if err != nil {
		t.Fatalf("detail preview proxy: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read detail preview response body: %v", err)
	}
	if !bytes.Equal(body, originalBody) {
		t.Fatalf("expected small detail preview passthrough")
	}
	if response.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("expected original content-type, got %q", response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Content-Disposition") != "inline; filename*=UTF-8''detail-preview.jpg" {
		t.Fatalf("expected normalized content-disposition, got %q", response.Header.Get("Content-Disposition"))
	}
}

func TestDetailPreviewResizesLargeImagesToJPEG(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeJPEGForTest(t, 4000, 3000)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.RequestURI() != "/api/assets/asset-123/thumbnail?size=preview" {
				t.Fatalf("unexpected request path %q", r.URL.RequestURI())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type":        []string{"image/jpeg"},
					"Content-Disposition": []string{"inline; filename*=UTF-8''asset-123.jpg"},
					"Cache-Control":       []string{"private, max-age=86400"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.DetailPreview(newHostedDetailPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("detail preview proxy: %v", err)
	}
	defer response.Body.Close()

	encodedBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read detail preview response body: %v", err)
	}
	if response.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("expected jpeg content-type, got %q", response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Content-Disposition") != "inline; filename*=UTF-8''detail-preview.jpg" {
		t.Fatalf("unexpected content-disposition %q", response.Header.Get("Content-Disposition"))
	}
	if serverTiming := response.Header.Get("Server-Timing"); !strings.Contains(serverTiming, "read_original;dur=") ||
		!strings.Contains(serverTiming, "transform;dur=") ||
		!strings.Contains(serverTiming, "total;dur=") {
		t.Fatalf("expected detail preview server timing stages, got %q", serverTiming)
	}
	detailImage, _, err := image.Decode(bytes.NewReader(encodedBody))
	if err != nil {
		t.Fatalf("decode detail preview response: %v", err)
	}
	if detailImage.Bounds().Dx() != 2560 || detailImage.Bounds().Dy() != 1920 {
		t.Fatalf("expected 2560x1920 detail preview, got %dx%d", detailImage.Bounds().Dx(), detailImage.Bounds().Dy())
	}
}

func TestDetailPreviewReturnsSmallPreviewUnchanged(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeJPEGForTest(t, 1440, 960)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.RequestURI() != "/api/assets/asset-123/thumbnail?size=preview" {
				t.Fatalf("unexpected request path %q", r.URL.RequestURI())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/jpeg"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.DetailPreview(newHostedDetailPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("detail preview proxy: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read detail preview response body: %v", err)
	}
	if !bytes.Equal(body, originalBody) {
		t.Fatalf("expected small detail preview passthrough")
	}
}

func TestDetailPreviewAppliesJPEGOrientationBeforeResize(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeJPEGWithOrientationForTest(t, 3000, 4000, 6)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/jpeg"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.DetailPreview(newHostedDetailPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("detail preview proxy: %v", err)
	}
	defer response.Body.Close()

	encodedBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read detail preview response body: %v", err)
	}

	detailImage, _, err := image.Decode(bytes.NewReader(encodedBody))
	if err != nil {
		t.Fatalf("decode oriented detail preview response: %v", err)
	}
	if detailImage.Bounds().Dx() != 2560 || detailImage.Bounds().Dy() != 1920 {
		t.Fatalf("expected oriented 2560x1920 detail preview, got %dx%d", detailImage.Bounds().Dx(), detailImage.Bounds().Dy())
	}
}

func TestDetailPreviewReturnsSmallOrientedPreviewUnchanged(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeQuadrantJPEGWithOrientationForTest(t, 600, 400, 6)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/jpeg"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.DetailPreview(newHostedDetailPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("detail preview proxy: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read detail preview response body: %v", err)
	}
	if !bytes.Equal(body, originalBody) {
		t.Fatalf("expected small oriented detail preview passthrough")
	}
}

func TestDetailPreviewFallsBackToOriginalForUnsupportedFormats(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := []byte("not-a-decodable-image")
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/heic"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.DetailPreview(newHostedDetailPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("detail preview proxy: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read fallback body: %v", err)
	}
	if string(body) != string(originalBody) {
		t.Fatalf("expected original body fallback, got %q", string(body))
	}
	if response.Header.Get("Content-Type") != "image/heic" {
		t.Fatalf("expected original content-type, got %q", response.Header.Get("Content-Type"))
	}
}

func TestDetailPreviewRejectsOversizedOriginal(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type":   []string{"image/jpeg"},
					"Content-Length": []string{fmt.Sprintf("%d", detailPreviewMaxSource+1)},
				},
				Body: io.NopCloser(strings.NewReader("too-large")),
			}, nil
		}),
	}

	response, err := service.DetailPreview(newHostedDetailPreviewRequest(t), "asset-123")
	if err == nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatalf("expected oversized detail preview error")
	}
	if !errors.Is(err, ErrMediaTooLarge) {
		t.Fatalf("error = %v, want %v", err, ErrMediaTooLarge)
	}
}

func TestProfileHeadRequestsUseGetUpstreamAndReportRenderedLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		route        string
		upstreamPath string
		load         func(*Service, *http.Request, string) (*UpstreamMediaResponse, error)
	}{
		{
			name:         "preview",
			route:        "preview",
			upstreamPath: "/api/assets/asset-123/thumbnail?size=thumbnail",
			load:         (*Service).Preview,
		},
		{
			name:         "detail preview",
			route:        "detail_preview",
			upstreamPath: "/api/assets/asset-123/thumbnail?size=preview",
			load:         (*Service).DetailPreview,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			service := NewService([]config.DatasourceConfig{{
				Name:        "test",
				Kind:        config.DatasourceKindImmichIndexed,
				URL:         "http://immich.test",
				AccessToken: "test-key",
			}})

			originalBody := encodeJPEGForTest(t, 3200, 1800)
			var upstreamMethod string
			var upstreamPath string
			var upstreamRange string
			var upstreamIfRange string
			var upstreamETag string
			var upstreamEncoding string
			service.client = &http.Client{
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					upstreamMethod = r.Method
					upstreamPath = r.URL.RequestURI()
					upstreamRange = r.Header.Get("Range")
					upstreamIfRange = r.Header.Get("If-Range")
					upstreamETag = r.Header.Get("If-None-Match")
					upstreamEncoding = r.Header.Get("Accept-Encoding")
					return &http.Response{
						StatusCode: http.StatusOK,
						Header: http.Header{
							"Content-Type": []string{"image/jpeg"},
						},
						Body: io.NopCloser(bytes.NewReader(originalBody)),
					}, nil
				}),
			}

			request, err := http.NewRequest(http.MethodHead, "http://timich-agent.test/v1/assets/asset-123/"+tt.route, nil)
			if err != nil {
				t.Fatalf("new head request: %v", err)
			}
			request.Header.Set("Range", "bytes=0-10")
			request.Header.Set("If-Range", "source-etag")
			request.Header.Set("If-None-Match", "profile-etag")
			request.Header.Set("Accept-Encoding", "gzip")
			response, err := tt.load(service, request, "asset-123")
			if err != nil {
				t.Fatalf("profile head request: %v", err)
			}
			defer response.Body.Close()

			if upstreamMethod != http.MethodGet {
				t.Fatalf("upstream method = %q, want GET", upstreamMethod)
			}
			if upstreamPath != tt.upstreamPath {
				t.Fatalf("upstream path = %q, want %q", upstreamPath, tt.upstreamPath)
			}
			if upstreamRange != "" || upstreamIfRange != "" || upstreamETag != "" || upstreamEncoding != "" {
				t.Fatalf("profile request forwarded representation headers range=%q if-range=%q etag=%q encoding=%q", upstreamRange, upstreamIfRange, upstreamETag, upstreamEncoding)
			}

			encodedBody, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read profile body: %v", err)
			}
			if len(encodedBody) == 0 {
				t.Fatalf("expected rendered body for header calculation")
			}
			if response.Header.Get("Content-Length") != fmt.Sprint(len(encodedBody)) {
				t.Fatalf("content-length = %q, want %d", response.Header.Get("Content-Length"), len(encodedBody))
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func newHostedDetailPreviewRequest(t *testing.T) *http.Request {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/detail_preview", nil)
	if err != nil {
		t.Fatalf("new hosted detail preview request: %v", err)
	}
	request.Header.Set("X-Timich-Hosted-Base-URL", "https://timich.runo.jp")
	return request
}

func newHostedAssetPreviewRequest(t *testing.T) *http.Request {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/preview", nil)
	if err != nil {
		t.Fatalf("new hosted asset preview request: %v", err)
	}
	request.Header.Set("X-Timich-Hosted-Base-URL", "https://timich.runo.jp")
	return request
}

func encodeJPEGForTest(t *testing.T, width int, height int) []byte {
	t.Helper()

	imageBuffer := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			imageBuffer.Set(x, y, color.RGBA{
				R: uint8(x % 255),
				G: uint8(y % 255),
				B: uint8((x + y) % 255),
				A: 255,
			})
		}
	}

	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, imageBuffer, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpeg for test: %v", err)
	}
	return encoded.Bytes()
}

func encodeJPEGWithOrientationForTest(t *testing.T, width int, height int, orientation uint16) []byte {
	t.Helper()

	body := encodeJPEGForTest(t, width, height)
	return injectJPEGOrientationForTest(t, body, orientation)
}

func encodeQuadrantJPEGWithOrientationForTest(t *testing.T, width int, height int, orientation uint16) []byte {
	t.Helper()

	imageBuffer := image.NewRGBA(image.Rect(0, 0, width, height))
	midX := width / 2
	midY := height / 2
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var pixel color.RGBA
			switch {
			case x < midX && y < midY:
				pixel = color.RGBA{R: 255, A: 255}
			case x >= midX && y < midY:
				pixel = color.RGBA{G: 255, A: 255}
			case x < midX && y >= midY:
				pixel = color.RGBA{B: 255, A: 255}
			default:
				pixel = color.RGBA{R: 255, G: 255, A: 255}
			}
			imageBuffer.Set(x, y, pixel)
		}
	}

	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, imageBuffer, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode quadrant jpeg for test: %v", err)
	}
	return injectJPEGOrientationForTest(t, encoded.Bytes(), orientation)
}

func injectJPEGOrientationForTest(t *testing.T, body []byte, orientation uint16) []byte {
	t.Helper()

	if len(body) < 2 || body[0] != 0xFF || body[1] != 0xD8 {
		t.Fatal("expected jpeg body with SOI marker")
	}

	var exif bytes.Buffer
	exif.WriteString("Exif")
	exif.Write([]byte{0x00, 0x00})
	exif.Write([]byte{'I', 'I'})
	_ = binary.Write(&exif, binary.LittleEndian, uint16(42))
	_ = binary.Write(&exif, binary.LittleEndian, uint32(8))
	_ = binary.Write(&exif, binary.LittleEndian, uint16(1))
	_ = binary.Write(&exif, binary.LittleEndian, uint16(0x0112))
	_ = binary.Write(&exif, binary.LittleEndian, uint16(3))
	_ = binary.Write(&exif, binary.LittleEndian, uint32(1))
	_ = binary.Write(&exif, binary.LittleEndian, orientation)
	_ = binary.Write(&exif, binary.LittleEndian, uint16(0))
	_ = binary.Write(&exif, binary.LittleEndian, uint32(0))

	segmentBody := exif.Bytes()
	segmentLength := uint16(len(segmentBody) + 2)

	var encoded bytes.Buffer
	encoded.Write(body[:2])
	encoded.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&encoded, binary.BigEndian, segmentLength)
	encoded.Write(segmentBody)
	encoded.Write(body[2:])
	return encoded.Bytes()
}

func assertApproxColorAt(t *testing.T, img image.Image, x int, y int, want color.RGBA) {
	t.Helper()

	got := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
	const tolerance = 48
	if absInt(int(got.R)-int(want.R)) > tolerance ||
		absInt(int(got.G)-int(want.G)) > tolerance ||
		absInt(int(got.B)-int(want.B)) > tolerance {
		t.Fatalf(
			"unexpected color at (%d,%d): got rgba(%d,%d,%d,%d), want approx rgba(%d,%d,%d,%d)",
			x,
			y,
			got.R,
			got.G,
			got.B,
			got.A,
			want.R,
			want.G,
			want.B,
			want.A,
		)
	}
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
