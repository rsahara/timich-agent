package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestLocalPhase0ScanDiscoversSupportedMediaAndQueuesMetadata(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "2026"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "2026", "family.jpg"), []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "clip.MOV"), []byte("video"), 0o644); err != nil {
		t.Fatalf("WriteFile(video) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "notes.txt"), []byte("notes"), 0o644); err != nil {
		t.Fatalf("WriteFile(notes) error = %v", err)
	}

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			QuickScanInterval: "15m",
			SettlingDuration:  "1ns",
		},
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	result, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result.Status != localPhase0StatusCompleted || result.RootStatus != LocalMediaRootStatusReady {
		t.Fatalf("scan result = %+v, want completed ready", result)
	}
	if result.DiscoveredPaths != 2 || result.ChangedPaths != 2 || result.QueuedMetadata != 2 || result.MissingPaths != 0 {
		t.Fatalf("scan result counts = %+v, want 2 discovered/changed/queued and 0 missing", result)
	}
	if result.SkippedPaths == 0 || result.SkipCounts["unsupported_extension"] == 0 {
		t.Fatalf("skip counts = %#v, want unsupported extension skip", result.SkipCounts)
	}

	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'discovered'`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued'`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_runs WHERE status = 'completed'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_root_state WHERE root_status = 'ready' AND phase0_status = 'completed'`, 1)

	metadata, err := service.RunLocalMetadataBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	if metadata.ProcessedJobs != 2 || metadata.CompletedJobs != 2 || metadata.FailedJobs != 0 || metadata.RegisteredAssets != 2 {
		t.Fatalf("metadata result = %+v, want 2 completed registrations", metadata)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'active'`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM catalog_assets WHERE datasource_kind = 'local_filesystem' AND visibility_status = 'active'`, 2)
	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() error = %v", err)
	}
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("timeline page total=%d items=%d, want metadata-only assets hidden until renditions are ready", page.Total, len(page.Items))
	}
	var family Asset
	family.SourceKey = "1111111111111111"
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT asset_id FROM local_assets WHERE source_key = ? AND filename = 'family.jpg'`, family.SourceKey).Scan(&family.ID); err != nil {
		t.Fatalf("read family local asset id: %v", err)
	}
	if family.ID == "" || family.SourceKey != "1111111111111111" {
		t.Fatalf("family item = %+v, want source-keyed local asset", family)
	}
	originalRequest, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/"+family.ID+"/original", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	original, err := service.OriginalFromSource(originalRequest, family.SourceKey, family.ID)
	if err != nil {
		t.Fatalf("OriginalFromSource() error = %v", err)
	}
	defer original.Body.Close()
	body, err := io.ReadAll(original.Body)
	if err != nil {
		t.Fatalf("ReadAll(original) error = %v", err)
	}
	if original.StatusCode != http.StatusOK || string(body) != "image" {
		t.Fatalf("original status=%d body=%q, want local original bytes", original.StatusCode, string(body))
	}
	rangeRequest, err := http.NewRequest(http.MethodGet, originalRequest.URL.String(), nil)
	if err != nil {
		t.Fatalf("NewRequest(range) error = %v", err)
	}
	rangeRequest.Header.Set("Range", "bytes=1-3")
	ranged, err := service.OriginalFromSource(rangeRequest, family.SourceKey, family.ID)
	if err != nil {
		t.Fatalf("OriginalFromSource(range) error = %v", err)
	}
	rangeBody, err := io.ReadAll(ranged.Body)
	if closeErr := ranged.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("read ranged local original: %v", err)
	}
	if ranged.StatusCode != http.StatusPartialContent ||
		ranged.Header.Get("Accept-Ranges") != "bytes" ||
		ranged.Header.Get("Content-Range") != "bytes 1-3/5" ||
		ranged.Header.Get("Content-Length") != "3" ||
		string(rangeBody) != "mag" {
		t.Fatalf("ranged original status=%d headers=%v body=%q", ranged.StatusCode, ranged.Header, string(rangeBody))
	}
	staleIfRangeRequest, err := http.NewRequest(http.MethodGet, originalRequest.URL.String(), nil)
	if err != nil {
		t.Fatalf("NewRequest(stale If-Range) error = %v", err)
	}
	staleIfRangeRequest.Header.Set("Range", "bytes=1-3")
	staleIfRangeRequest.Header.Set("If-Range", "Sat, 01 Jan 2000 00:00:00 GMT")
	staleIfRange, err := service.OriginalFromSource(staleIfRangeRequest, family.SourceKey, family.ID)
	if err != nil {
		t.Fatalf("OriginalFromSource(stale If-Range) error = %v", err)
	}
	staleIfRangeBody, err := io.ReadAll(staleIfRange.Body)
	if closeErr := staleIfRange.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("read stale If-Range original: %v", err)
	}
	if staleIfRange.StatusCode != http.StatusOK || staleIfRange.Header.Get("Content-Range") != "" || string(staleIfRangeBody) != "image" {
		t.Fatalf("stale If-Range status=%d headers=%v body=%q, want full representation", staleIfRange.StatusCode, staleIfRange.Header, string(staleIfRangeBody))
	}
	matchingIfRangeRequest, err := http.NewRequest(http.MethodGet, originalRequest.URL.String(), nil)
	if err != nil {
		t.Fatalf("NewRequest(matching If-Range) error = %v", err)
	}
	matchingIfRangeRequest.Header.Set("Range", "bytes=1-3")
	matchingIfRangeRequest.Header.Set("If-Range", original.Header.Get("Last-Modified"))
	matchingIfRange, err := service.OriginalFromSource(matchingIfRangeRequest, family.SourceKey, family.ID)
	if err != nil {
		t.Fatalf("OriginalFromSource(matching If-Range) error = %v", err)
	}
	matchingIfRangeBody, err := io.ReadAll(matchingIfRange.Body)
	if closeErr := matchingIfRange.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("read matching If-Range original: %v", err)
	}
	if matchingIfRange.StatusCode != http.StatusPartialContent || string(matchingIfRangeBody) != "mag" {
		t.Fatalf("matching If-Range status=%d headers=%v body=%q", matchingIfRange.StatusCode, matchingIfRange.Header, string(matchingIfRangeBody))
	}
	invalidRangeRequest, err := http.NewRequest(http.MethodGet, originalRequest.URL.String(), nil)
	if err != nil {
		t.Fatalf("NewRequest(invalid range) error = %v", err)
	}
	invalidRangeRequest.Header.Set("Range", "bytes=99-100")
	invalidRange, err := service.OriginalFromSource(invalidRangeRequest, family.SourceKey, family.ID)
	if err != nil {
		t.Fatalf("OriginalFromSource(invalid range) error = %v", err)
	}
	defer invalidRange.Body.Close()
	if invalidRange.StatusCode != http.StatusRequestedRangeNotSatisfiable ||
		invalidRange.Header.Get("Content-Range") != "bytes */5" ||
		invalidRange.Header.Get("Content-Length") != "0" {
		t.Fatalf("invalid ranged original status=%d headers=%v", invalidRange.StatusCode, invalidRange.Header)
	}
	if _, err := service.PreviewFromSource(originalRequest, family.SourceKey, family.ID); err != ErrAssetNotFound {
		t.Fatalf("PreviewFromSource() error = %v, want ErrAssetNotFound before thumbnail generation", err)
	}

	if err := os.Remove(filepath.Join(rootPath, "clip.MOV")); err != nil {
		t.Fatalf("Remove(video) error = %v", err)
	}
	second, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("second RunLocalReconciliationScan() error = %v", err)
	}
	if second.DiscoveredPaths != 1 || second.MissingPaths != 1 || second.ChangedPaths != 0 || second.QueuedMetadata != 0 {
		t.Fatalf("second scan result = %+v, want one discovered, one missing, no duplicate queue", second)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'missing'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'active'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM catalog_assets WHERE datasource_kind = 'local_filesystem' AND visibility_status = 'active'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata'`, 0)
	page, err = service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() after delete error = %v", err)
	}
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("page after delete total=%d items=%#v, want remaining metadata-only family hidden", page.Total, page.Items)
	}

	oldFamilyID := family.ID
	if err := os.WriteFile(filepath.Join(rootPath, "2026", "family.jpg"), []byte("replacement-image"), 0o644); err != nil {
		t.Fatalf("WriteFile(replacement) error = %v", err)
	}
	third, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("third RunLocalReconciliationScan() error = %v", err)
	}
	if third.DiscoveredPaths != 1 || third.MissingPaths != 0 || third.ChangedPaths != 1 || third.QueuedMetadata != 1 {
		t.Fatalf("third scan result = %+v, want one changed replacement", third)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'active'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM catalog_assets WHERE datasource_kind = 'local_filesystem' AND visibility_status = 'active'`, 0)

	replacementMetadata, err := service.RunLocalMetadataBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch() replacement error = %v", err)
	}
	if replacementMetadata.ProcessedJobs != 1 || replacementMetadata.CompletedJobs != 1 || replacementMetadata.RegisteredAssets != 1 {
		t.Fatalf("replacement metadata = %+v, want one completed registration", replacementMetadata)
	}
	page, err = service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() after replacement error = %v", err)
	}
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("page after replacement total=%d items=%#v, want replacement hidden until renditions are ready", page.Total, page.Items)
	}
	var replacementAssetID string
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT asset_id FROM local_assets WHERE source_key = ? AND filename = 'family.jpg' AND visibility_status = 'active'`, family.SourceKey).Scan(&replacementAssetID); err != nil {
		t.Fatalf("read replacement local asset id: %v", err)
	}
	if replacementAssetID == oldFamilyID {
		t.Fatalf("replacement asset id = %q, want a new id", replacementAssetID)
	}
}

func TestLocalMediaAccessRejectsSymlinkInsertedAfterScan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink-safe local file opening is a POSIX runtime contract")
	}
	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "photo.jpg")
	if err := os.WriteFile(mediaPath, []byte("inside"), 0o600); err != nil {
		t.Fatalf("WriteFile(media) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 1); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	var assetID string
	if err := service.catalog.db.QueryRow(`SELECT asset_id FROM local_assets WHERE source_key = '1111111111111111'`).Scan(&assetID); err != nil {
		t.Fatalf("read local asset ID: %v", err)
	}
	outsidePath := filepath.Join(t.TempDir(), "outside.jpg")
	if err := os.WriteFile(outsidePath, []byte("outside-secret"), 0o600); err != nil {
		t.Fatalf("WriteFile(outside) error = %v", err)
	}
	if err := os.Remove(mediaPath); err != nil {
		t.Fatalf("Remove(media) error = %v", err)
	}
	if err := os.Symlink(outsidePath, mediaPath); err != nil {
		t.Fatalf("Symlink(outside, media) error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/assets/"+assetID+"/original", nil)
	if _, err := service.OriginalFromSource(request, "1111111111111111", assetID); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("OriginalFromSource(symlink) error = %v, want ErrAssetNotFound", err)
	}
}

func TestLocalOriginalRejectsAtomicReplacementWithPreservedStat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stable file identity is a POSIX runtime contract")
	}
	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "photo.jpg")
	fixedTime := time.Date(2026, 7, 20, 1, 2, 3, 0, time.UTC)
	if err := os.WriteFile(mediaPath, []byte("inside"), 0o600); err != nil {
		t.Fatalf("WriteFile(media) error = %v", err)
	}
	if err := os.Chtimes(mediaPath, fixedTime, fixedTime); err != nil {
		t.Fatalf("Chtimes(media) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 1); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	var assetID string
	if err := service.catalog.db.QueryRow(`SELECT asset_id FROM local_assets WHERE source_key = '1111111111111111'`).Scan(&assetID); err != nil {
		t.Fatalf("read local asset ID: %v", err)
	}
	replacementPath := filepath.Join(rootPath, "replacement.jpg")
	if err := os.WriteFile(replacementPath, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile(replacement) error = %v", err)
	}
	if err := os.Chtimes(replacementPath, fixedTime, fixedTime); err != nil {
		t.Fatalf("Chtimes(replacement) error = %v", err)
	}
	if err := os.Rename(replacementPath, mediaPath); err != nil {
		t.Fatalf("Rename(replacement) error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/assets/"+assetID+"/original", nil)
	if _, err := service.OriginalFromSource(request, "1111111111111111", assetID); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("OriginalFromSource(replaced path) error = %v, want ErrAssetNotFound", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'discovered'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued'`, 1)
}

func TestLocalOriginalResettlePreservesThumbnailWhenDuplicateLocationRemains(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stable file identity is a POSIX runtime contract")
	}
	rootPath := t.TempDir()
	fixedTime := time.Date(2026, 7, 20, 8, 9, 10, 0, time.UTC)
	for _, name := range []string{"a.jpg", "b.jpg"} {
		mediaPath := filepath.Join(rootPath, name)
		if err := os.WriteFile(mediaPath, []byte("inside"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
		if err := os.Chtimes(mediaPath, fixedTime, fixedTime); err != nil {
			t.Fatalf("Chtimes(%s) error = %v", name, err)
		}
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 2); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	var assetID string
	var primaryLocationID int64
	var relativePath string
	if err := service.catalog.db.QueryRow(`SELECT a.asset_id, a.primary_location_id, l.relative_path
		FROM local_assets a
		JOIN local_asset_locations l ON l.id = a.primary_location_id
		WHERE a.source_key = '1111111111111111'`).Scan(&assetID, &primaryLocationID, &relativePath); err != nil {
		t.Fatalf("read primary local location: %v", err)
	}
	if _, err := service.catalog.db.Exec(`UPDATE local_assets SET thumbnail_status = 'ready'
		WHERE source_key = '1111111111111111' AND asset_id = ?`, assetID); err != nil {
		t.Fatalf("mark thumbnail ready: %v", err)
	}
	replacementPath := filepath.Join(rootPath, "replacement.jpg")
	if err := os.WriteFile(replacementPath, []byte("secret!"), 0o600); err != nil {
		t.Fatalf("WriteFile(replacement) error = %v", err)
	}
	if err := os.Chtimes(replacementPath, fixedTime, fixedTime); err != nil {
		t.Fatalf("Chtimes(replacement) error = %v", err)
	}
	if err := os.Rename(replacementPath, filepath.Join(rootPath, relativePath)); err != nil {
		t.Fatalf("Rename(replacement) error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/assets/"+assetID+"/original", nil)
	if _, err := service.OriginalFromSource(request, "1111111111111111", assetID); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("OriginalFromSource(replaced path) error = %v, want ErrAssetNotFound", err)
	}
	var activeLocations int
	if err := service.catalog.db.QueryRow(`SELECT COUNT(*) FROM local_asset_locations
		WHERE source_key = '1111111111111111' AND asset_id = ? AND status = 'active'`, assetID).Scan(&activeLocations); err != nil {
		t.Fatalf("count remaining active locations: %v", err)
	}
	if activeLocations != 1 {
		t.Fatalf("remaining active locations = %d, want 1", activeLocations)
	}
	var repairedPrimaryLocationID int64
	var visibilityStatus string
	var primaryOwnerAssetID string
	var primaryStatus string
	if err := service.catalog.db.QueryRow(`SELECT a.primary_location_id, a.visibility_status, l.asset_id, l.status
		FROM local_assets a
		JOIN local_asset_locations l ON l.id = a.primary_location_id
		WHERE a.source_key = '1111111111111111' AND a.asset_id = ?`, assetID).
		Scan(&repairedPrimaryLocationID, &visibilityStatus, &primaryOwnerAssetID, &primaryStatus); err != nil {
		t.Fatalf("read repaired primary local location: %v", err)
	}
	if repairedPrimaryLocationID == primaryLocationID {
		t.Fatalf("primary location ID = %d, want replacement for resettled location", repairedPrimaryLocationID)
	}
	if visibilityStatus != "active" {
		t.Fatalf("visibility status = %q, want active", visibilityStatus)
	}
	if primaryOwnerAssetID != assetID {
		t.Fatalf("primary location owner = %q, want %q", primaryOwnerAssetID, assetID)
	}
	if primaryStatus != "active" {
		t.Fatalf("primary location status = %q, want active", primaryStatus)
	}
	var thumbnailStatus string
	if err := service.catalog.db.QueryRow(`SELECT thumbnail_status FROM local_assets
		WHERE source_key = '1111111111111111' AND asset_id = ?`, assetID).Scan(&thumbnailStatus); err != nil {
		t.Fatalf("read thumbnail status: %v", err)
	}
	if thumbnailStatus != "ready" {
		t.Fatalf("thumbnail status = %q, want ready", thumbnailStatus)
	}
}

func TestRefreshLocalAssetVisibilityRepairsForeignPrimaryLocation(t *testing.T) {
	rootPath := t.TempDir()
	for name, content := range map[string]string{
		"a.jpg": "duplicate",
		"b.jpg": "duplicate",
		"c.jpg": "different",
	} {
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 3); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}

	var duplicateAssetID string
	if err := service.catalog.db.QueryRow(`SELECT asset_id
		FROM local_asset_locations
		WHERE source_key = '1111111111111111' AND status = 'active'
		GROUP BY asset_id
		HAVING COUNT(*) = 2`).Scan(&duplicateAssetID); err != nil {
		t.Fatalf("read duplicate local asset: %v", err)
	}
	var foreignLocationID int64
	if err := service.catalog.db.QueryRow(`SELECT id
		FROM local_asset_locations
		WHERE source_key = '1111111111111111'
			AND status = 'active'
			AND asset_id != ?
		LIMIT 1`, duplicateAssetID).Scan(&foreignLocationID); err != nil {
		t.Fatalf("read foreign local location: %v", err)
	}
	if _, err := service.catalog.db.Exec(`UPDATE local_assets
		SET primary_location_id = ?
		WHERE source_key = '1111111111111111' AND asset_id = ?`, foreignLocationID, duplicateAssetID); err != nil {
		t.Fatalf("set foreign primary location: %v", err)
	}

	nowText := formatCatalogTime(time.Now().UTC())
	tx, err := service.catalog.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if _, err := refreshLocalAssetVisibilityInTx(context.Background(), tx, "1111111111111111", "nas-photos", nowText); err != nil {
		_ = tx.Rollback()
		t.Fatalf("refreshLocalAssetVisibilityInTx() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	var repairedPrimaryLocationID int64
	var primaryOwnerAssetID string
	var primaryStatus string
	if err := service.catalog.db.QueryRow(`SELECT a.primary_location_id, l.asset_id, l.status
		FROM local_assets a
		JOIN local_asset_locations l ON l.id = a.primary_location_id
		WHERE a.source_key = '1111111111111111' AND a.asset_id = ?`, duplicateAssetID).
		Scan(&repairedPrimaryLocationID, &primaryOwnerAssetID, &primaryStatus); err != nil {
		t.Fatalf("read repaired foreign primary location: %v", err)
	}
	if repairedPrimaryLocationID == foreignLocationID {
		t.Fatalf("primary location ID = %d, want replacement for foreign location", repairedPrimaryLocationID)
	}
	if primaryOwnerAssetID != duplicateAssetID {
		t.Fatalf("primary location owner = %q, want %q", primaryOwnerAssetID, duplicateAssetID)
	}
	if primaryStatus != "active" {
		t.Fatalf("primary location status = %q, want active", primaryStatus)
	}
}

func TestLocalPrimaryLocationSelectionUsesVerificationMTimeAndPathOrder(t *testing.T) {
	rootPath := t.TempDir()
	for _, name := range []string{"a.jpg", "b.jpg", "c.jpg", "d.jpg"} {
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte("duplicate"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	service := newLocalPhase0TestService(t, rootPath)
	ctx := context.Background()
	if _, err := service.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(ctx, 4); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}

	var assetID string
	if err := service.catalog.db.QueryRow(`SELECT asset_id
		FROM local_asset_locations
		WHERE source_key = '1111111111111111' AND status = 'active'
		GROUP BY asset_id
		HAVING COUNT(*) = 4`).Scan(&assetID); err != nil {
		t.Fatalf("read duplicate local asset: %v", err)
	}
	locationIDs := map[string]int64{}
	rows, err := service.catalog.db.Query(`SELECT id, relative_path
		FROM local_asset_locations
		WHERE source_key = '1111111111111111' AND asset_id = ?`, assetID)
	if err != nil {
		t.Fatalf("query duplicate local locations: %v", err)
	}
	for rows.Next() {
		var id int64
		var relativePath string
		if err := rows.Scan(&id, &relativePath); err != nil {
			_ = rows.Close()
			t.Fatalf("scan duplicate local location: %v", err)
		}
		locationIDs[relativePath] = id
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close duplicate local locations: %v", err)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate duplicate local locations: %v", err)
	}
	if len(locationIDs) != 4 {
		t.Fatalf("location IDs = %#v, want four duplicate paths", locationIDs)
	}

	updatePriority := func(relativePath string, verifiedAt time.Time, mtime time.Time) {
		t.Helper()
		if _, err := service.catalog.db.Exec(`UPDATE local_asset_locations
			SET verified_at = ?, mtime = ?
			WHERE id = ?`,
			formatCatalogTime(verifiedAt),
			formatCatalogTime(mtime),
			locationIDs[relativePath],
		); err != nil {
			t.Fatalf("update %s priority: %v", relativePath, err)
		}
	}
	clearPrimary := func() {
		t.Helper()
		if _, err := service.catalog.db.Exec(`UPDATE local_assets
			SET primary_location_id = NULL
			WHERE source_key = '1111111111111111' AND asset_id = ?`, assetID); err != nil {
			t.Fatalf("clear primary location: %v", err)
		}
	}
	refreshVisibility := func(rootKey string) {
		t.Helper()
		tx, err := service.catalog.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx() error = %v", err)
		}
		if _, err := refreshLocalAssetVisibilityInTx(ctx, tx, "1111111111111111", rootKey, formatCatalogTime(time.Now().UTC())); err != nil {
			_ = tx.Rollback()
			t.Fatalf("refreshLocalAssetVisibilityInTx() error = %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit() error = %v", err)
		}
	}
	assertPrimary := func(wantPath string) {
		t.Helper()
		var gotPath string
		if err := service.catalog.db.QueryRow(`SELECT l.relative_path
			FROM local_assets a
			JOIN local_asset_locations l ON l.id = a.primary_location_id
			WHERE a.source_key = '1111111111111111' AND a.asset_id = ?`, assetID).Scan(&gotPath); err != nil {
			t.Fatalf("read primary location path: %v", err)
		}
		if gotPath != wantPath {
			t.Fatalf("primary location path = %q, want %q", gotPath, wantPath)
		}
	}

	baseTime := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	updatePriority("a.jpg", baseTime.Add(4*time.Hour), baseTime)
	updatePriority("b.jpg", baseTime.Add(3*time.Hour), baseTime.Add(4*time.Hour))
	updatePriority("c.jpg", baseTime.Add(3*time.Hour), baseTime.Add(3*time.Hour))
	updatePriority("d.jpg", baseTime.Add(2*time.Hour), baseTime.Add(5*time.Hour))
	clearPrimary()
	refreshVisibility("nas-photos")
	assertPrimary("a.jpg")

	updatePriority("b.jpg", baseTime.Add(5*time.Hour), baseTime.Add(4*time.Hour))
	refreshVisibility("nas-photos")
	assertPrimary("a.jpg")
	location, err := service.localActiveLocation(ctx, "1111111111111111", "nas-photos", assetID)
	if err != nil {
		t.Fatalf("localActiveLocation(valid primary) error = %v", err)
	}
	if location.RelativePath != "a.jpg" {
		t.Fatalf("active location with valid primary = %q, want a.jpg", location.RelativePath)
	}

	tiedVerifiedAt := baseTime.Add(3 * time.Hour)
	updatePriority("a.jpg", baseTime, baseTime)
	updatePriority("b.jpg", tiedVerifiedAt, baseTime.Add(2*time.Hour))
	updatePriority("c.jpg", tiedVerifiedAt, baseTime.Add(4*time.Hour))
	updatePriority("d.jpg", baseTime.Add(2*time.Hour), baseTime.Add(5*time.Hour))
	clearPrimary()
	refreshVisibility("nas-photos")
	assertPrimary("c.jpg")

	tiedMTime := baseTime.Add(4 * time.Hour)
	for _, name := range []string{"b.jpg", "c.jpg", "d.jpg"} {
		updatePriority(name, tiedVerifiedAt, tiedMTime)
	}
	if _, err := service.catalog.db.Exec(`UPDATE local_asset_locations
		SET root_key = CASE relative_path
				WHEN 'b.jpg' THEN 'z-root'
				ELSE 'a-root'
			END
		WHERE id IN (?, ?, ?)`,
		locationIDs["b.jpg"],
		locationIDs["c.jpg"],
		locationIDs["d.jpg"],
	); err != nil {
		t.Fatalf("set path-order roots: %v", err)
	}
	clearPrimary()
	refreshVisibility("a-root")
	assertPrimary("c.jpg")

	clearPrimary()
	location, err = service.localActiveLocation(ctx, "1111111111111111", "a-root", assetID)
	if err != nil {
		t.Fatalf("localActiveLocation(fallback) error = %v", err)
	}
	if location.RelativePath != "c.jpg" {
		t.Fatalf("active location fallback = %q, want c.jpg", location.RelativePath)
	}
}

func TestLocalMediaSelectionRepairsPrimaryFromFormerRoot(t *testing.T) {
	dataDir := t.TempDir()
	rootAPath := t.TempDir()
	rootBPath := t.TempDir()
	for _, rootPath := range []string{rootAPath, rootBPath} {
		if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), []byte("duplicate"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", rootPath, err)
		}
	}
	ctx := context.Background()
	newService := func(rootKey string, rootPath string) *Service {
		t.Helper()
		service, err := NewServiceWithOptions([]config.DatasourceConfig{{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   rootKey,
			Scan: &config.LocalDatasourceScanConfig{
				SettlingDuration: "1ns",
			},
		}}, ServiceOptions{
			DataDir:    dataDir,
			LocalRoots: []config.LocalMediaRootConfig{{Key: rootKey, Path: rootPath}},
		})
		if err != nil {
			t.Fatalf("NewServiceWithOptions(%s) error = %v", rootKey, err)
		}
		return service
	}

	serviceA := newService("root-a", rootAPath)
	if _, err := serviceA.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(root-a) error = %v", err)
	}
	var assetID string
	if result, err := serviceA.RunLocalMetadataBatch(ctx, 1); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch(root-a) result=%+v error=%v", result, err)
	}
	if err := serviceA.catalog.db.QueryRow(`SELECT asset_id
		FROM local_asset_locations
		WHERE source_key = '1111111111111111' AND root_key = 'root-a' AND status = 'active'`).Scan(&assetID); err != nil {
		t.Fatalf("read root-a asset: %v", err)
	}
	if err := serviceA.Close(); err != nil {
		t.Fatalf("Close(root-a) error = %v", err)
	}

	service := newService("root-b", rootBPath)
	defer service.Close()
	if _, err := service.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(root-b) error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(ctx, 1); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch(root-b) result=%+v error=%v", result, err)
	}

	location, err := service.localActiveLocation(ctx, "1111111111111111", "root-b", assetID)
	if err != nil {
		t.Fatalf("localActiveLocation(current root) error = %v", err)
	}
	if location.RootKey != "root-b" || location.RelativePath != "family.jpg" {
		t.Fatalf("current location = %+v, want family.jpg in root-b", location)
	}
	var primaryRootKey string
	var primaryPath string
	if err := service.catalog.db.QueryRow(`SELECT l.root_key, l.relative_path
		FROM local_assets a
		JOIN local_asset_locations l ON l.id = a.primary_location_id
		WHERE a.source_key = '1111111111111111' AND a.asset_id = ?`, assetID).
		Scan(&primaryRootKey, &primaryPath); err != nil {
		t.Fatalf("read repaired primary location: %v", err)
	}
	if primaryRootKey != "root-b" || primaryPath != "family.jpg" {
		t.Fatalf("repaired primary = %s/%s, want root-b/family.jpg", primaryRootKey, primaryPath)
	}
	var formerRootLocations int
	if err := service.catalog.db.QueryRow(`SELECT COUNT(*) FROM local_asset_locations
		WHERE source_key = '1111111111111111' AND root_key = 'root-a' AND asset_id = ? AND status = 'active'`, assetID).
		Scan(&formerRootLocations); err != nil {
		t.Fatalf("count former-root locations: %v", err)
	}
	if formerRootLocations != 1 {
		t.Fatalf("former-root active location count = %d, want retained history", formerRootLocations)
	}

	datasource := service.datasourceStateSnapshot().datasources["1111111111111111"]
	request := httptest.NewRequest(http.MethodGet, "http://agent.local/original", nil)
	response, err := service.localOriginalMediaResponse(request, &datasource, assetID)
	if err != nil {
		t.Fatalf("localOriginalMediaResponse() error = %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read original response: %v", err)
	}
	if string(body) != "duplicate" {
		t.Fatalf("original body = %q, want duplicate", body)
	}

	trustedRoot, err := service.acquireTrustedLocalMediaRoot(ctx, "1111111111111111")
	if err != nil {
		t.Fatalf("acquireTrustedLocalMediaRoot() error = %v", err)
	}
	defer trustedRoot.Close()
	source, thumbnailLocation, _, err := service.localThumbnailSource(ctx, "1111111111111111", assetID, trustedRoot)
	if err != nil {
		t.Fatalf("localThumbnailSource() error = %v", err)
	}
	defer source.File.Close()
	if thumbnailLocation.RootKey != "root-b" || thumbnailLocation.RelativePath != "family.jpg" {
		t.Fatalf("thumbnail location = %+v, want family.jpg in root-b", thumbnailLocation)
	}
}

func TestReopenLocalRootFileDetectsAtomicPathReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("safe root-relative reopening is a POSIX runtime contract")
	}
	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "photo.jpg")
	fixedTime := time.Date(2026, 7, 20, 4, 5, 6, 0, time.UTC)
	if err := os.WriteFile(mediaPath, []byte("inside"), 0o600); err != nil {
		t.Fatalf("WriteFile(media) error = %v", err)
	}
	if err := os.Chtimes(mediaPath, fixedTime, fixedTime); err != nil {
		t.Fatalf("Chtimes(media) error = %v", err)
	}
	pinned, pinnedInfo, err := openLocalRootFile(rootPath, "photo.jpg")
	if err != nil {
		t.Fatalf("openLocalRootFile() error = %v", err)
	}
	defer pinned.Close()
	replacementPath := filepath.Join(rootPath, "replacement.jpg")
	if err := os.WriteFile(replacementPath, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile(replacement) error = %v", err)
	}
	if err := os.Chtimes(replacementPath, fixedTime, fixedTime); err != nil {
		t.Fatalf("Chtimes(replacement) error = %v", err)
	}
	if err := os.Rename(replacementPath, mediaPath); err != nil {
		t.Fatalf("Rename(replacement) error = %v", err)
	}

	current, _, matches, err := reopenLocalRootFileAndMatch(rootPath, "photo.jpg", pinnedInfo)
	if err != nil {
		t.Fatalf("reopenLocalRootFileAndMatch() error = %v", err)
	}
	defer current.Close()
	if matches {
		t.Fatal("reopened pathname matched the replaced pinned file")
	}
}

func TestSHA1OpenFileStopsWhenContextIsCanceled(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, _, err := sha1OpenFile(ctx, reader)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sha1OpenFile() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("sha1OpenFile() did not stop after cancellation")
	}
}

func TestLocalPhase0ScanNotifiesCommittedMetadataBeforeScanCompletes(t *testing.T) {
	rootPath := t.TempDir()
	for index := 0; index <= localPhase0WriteBatchSize; index++ {
		name := fmt.Sprintf("photo-%03d.jpg", index)
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte("image"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	service := newLocalPhase0TestService(t, rootPath)

	var notifications atomic.Int32
	notified := make(chan struct{})
	var notifyOnce sync.Once
	service.SetLocalWorkNotifier(func() {
		notifications.Add(1)
		notifyOnce.Do(func() { close(notified) })
	})

	originalWalkDir := localPhase0WalkDir
	t.Cleanup(func() { localPhase0WalkDir = originalWalkDir })
	walkBlocked := make(chan struct{})
	releaseWalk := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWalk) }) }
	t.Cleanup(release)
	localPhase0WalkDir = func(_ string, rootFS fs.FS, fn fs.WalkDirFunc) error {
		fileCount := 0
		return fs.WalkDir(rootFS, ".", func(path string, entry fs.DirEntry, walkErr error) error {
			if err := fn(path, entry, walkErr); err != nil {
				return err
			}
			if walkErr != nil || entry == nil || entry.IsDir() {
				return nil
			}
			fileCount++
			if fileCount != localPhase0WriteBatchSize {
				return nil
			}
			select {
			case <-notified:
			default:
				return errors.New("metadata batch committed without notifying local work")
			}
			close(walkBlocked)
			<-releaseWalk
			return nil
		})
	}

	type scanOutcome struct {
		result LocalPhase0ScanResult
		err    error
	}
	scanDone := make(chan scanOutcome, 1)
	go func() {
		result, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
		scanDone <- scanOutcome{result: result, err: err}
	}()
	select {
	case <-walkBlocked:
	case <-time.After(3 * time.Second):
		t.Fatal("Phase 0 did not reach the first committed metadata batch")
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued'`, localPhase0WriteBatchSize)
	select {
	case outcome := <-scanDone:
		t.Fatalf("scan completed before the first batch notification was observed: %+v", outcome)
	default:
	}

	release()
	select {
	case outcome := <-scanDone:
		if outcome.err != nil {
			t.Fatalf("RunLocalReconciliationScan() error = %v", outcome.err)
		}
		if outcome.result.Status != localPhase0StatusCompleted || outcome.result.QueuedMetadata != localPhase0WriteBatchSize+1 {
			t.Fatalf("scan result = %+v, want all metadata queued", outcome.result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Phase 0 did not finish after the walk was released")
	}
	if got := notifications.Load(); got != 2 {
		t.Fatalf("local work notifications = %d, want one per committed metadata batch", got)
	}
}

func TestLocalPhase0CancellationFinalizesRun(t *testing.T) {
	rootPath := t.TempDir()
	unsupportedPath := filepath.Join(rootPath, "notes.txt")
	if err := os.WriteFile(unsupportedPath, []byte("notes"), 0o600); err != nil {
		t.Fatalf("WriteFile(unsupported) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	ctx, cancel := context.WithCancel(context.Background())
	originalWalkDir := localPhase0WalkDir
	t.Cleanup(func() { localPhase0WalkDir = originalWalkDir })
	localPhase0WalkDir = func(logicalRoot string, rootFS fs.FS, fn fs.WalkDirFunc) error {
		if err := fs.WalkDir(rootFS, ".", func(path string, entry fs.DirEntry, walkErr error) error {
			if localPhase0LogicalWalkPath(logicalRoot, path) == unsupportedPath {
				cancel()
			}
			return fn(path, entry, walkErr)
		}); err != nil {
			return err
		}
		return nil
	}
	result, err := service.RunLocalReconciliationScan(ctx, "1111111111111111")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunLocalReconciliationScan() error = %v, want context.Canceled", err)
	}
	if result.Status != localPhase0StatusFailed {
		t.Fatalf("RunLocalReconciliationScan() status = %q, want failed", result.Status)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_runs WHERE status = 'running'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_runs WHERE status = 'failed' AND completed_at IS NOT NULL`, 1)
	var rootStatus string
	var lastReconciliationAt sql.NullString
	if err := service.catalog.db.QueryRow(`SELECT root_status, last_reconciliation_at
		FROM local_scan_root_state WHERE source_key = '1111111111111111'`).Scan(&rootStatus, &lastReconciliationAt); err != nil {
		t.Fatalf("read canceled scan root state: %v", err)
	}
	if rootStatus != LocalMediaRootStatusReady || lastReconciliationAt.Valid {
		t.Fatalf("canceled scan root status=%q lastReconciliation=%#v, want ready and no successful reconciliation", rootStatus, lastReconciliationAt)
	}
}

func TestLocalMetadataWaitsForSettlingAndRestartsWhenSourceChanges(t *testing.T) {
	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "copying.jpg")
	if err := os.WriteFile(mediaPath, []byte("partial"), 0o644); err != nil {
		t.Fatalf("WriteFile(partial) error = %v", err)
	}
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: "2m",
		},
	}}, ServiceOptions{
		DataDir:    t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	state, err := service.LocalMetadataQueueState(context.Background())
	if err != nil {
		t.Fatalf("LocalMetadataQueueState() error = %v", err)
	}
	if state.Queued != 0 || state.Settling != 1 || state.NextEligibleAt == nil {
		t.Fatalf("metadata queue state = %+v, want one settling job", state)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.ProcessedJobs != 0 {
		t.Fatalf("RunLocalMetadataBatch(before due) result=%+v error=%v, want no work", result, err)
	}

	if err := os.WriteFile(mediaPath, []byte("completed-copy"), 0o644); err != nil {
		t.Fatalf("WriteFile(completed) error = %v", err)
	}
	past := formatCatalogTime(time.Now().UTC().Add(-time.Second))
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_asset_locations SET metadata_not_before = ?`, past); err != nil {
		t.Fatalf("make location due: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_scan_jobs SET scheduled_at = ? WHERE job_kind = 'metadata'`, past); err != nil {
		t.Fatalf("make metadata job due: %v", err)
	}
	result, err := service.RunLocalMetadataBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch(changed) error = %v", err)
	}
	if result.ProcessedJobs != 1 || result.SettlingJobs != 1 || result.RegisteredAssets != 0 {
		t.Fatalf("changed metadata result = %+v, want resettled job", result)
	}
	state, err = service.LocalMetadataQueueState(context.Background())
	if err != nil {
		t.Fatalf("LocalMetadataQueueState(after change) error = %v", err)
	}
	if state.Queued != 0 || state.Settling != 1 {
		t.Fatalf("metadata queue state after change = %+v, want one settling job", state)
	}

	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_asset_locations SET metadata_not_before = ?`, past); err != nil {
		t.Fatalf("make stable location due: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_scan_jobs SET scheduled_at = ? WHERE job_kind = 'metadata' AND status = 'queued'`, past); err != nil {
		t.Fatalf("make stable metadata job due: %v", err)
	}
	result, err = service.RunLocalMetadataBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch(stable) error = %v", err)
	}
	if result.RegisteredAssets != 1 || result.FailedJobs != 0 {
		t.Fatalf("stable metadata result = %+v, want registration", result)
	}
}

func TestLocalMetadataRediscoveryRefreshesExistingJobDeadline(t *testing.T) {
	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "copying.jpg")
	if err := os.WriteFile(mediaPath, []byte("partial"), 0o644); err != nil {
		t.Fatalf("WriteFile(partial) error = %v", err)
	}
	service := newLocalPhase0SettlingTestService(t, rootPath, "2m")
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(initial) error = %v", err)
	}

	var (
		jobID             int64
		jobRootKey        string
		jobRootGeneration int64
	)
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT id, COALESCE(root_key, ''), root_generation
		FROM local_scan_jobs
		WHERE job_kind = 'metadata' AND status = 'queued'`).Scan(&jobID, &jobRootKey, &jobRootGeneration); err != nil {
		t.Fatalf("read initial metadata job: %v", err)
	}
	past := formatCatalogTime(time.Now().UTC().Add(-time.Minute))
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_scan_jobs SET scheduled_at = ? WHERE id = ?`, past, jobID); err != nil {
		t.Fatalf("make existing metadata job obsolete: %v", err)
	}
	if err := os.WriteFile(mediaPath, []byte("completed-copy"), 0o644); err != nil {
		t.Fatalf("WriteFile(completed) error = %v", err)
	}
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(rediscovery) error = %v", err)
	}

	var metadataNotBefore string
	var scheduledAt string
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT l.metadata_not_before, j.scheduled_at
		FROM local_asset_locations l
		JOIN local_scan_jobs j ON j.location_id = l.id
		WHERE j.id = ?`, jobID).Scan(&metadataNotBefore, &scheduledAt); err != nil {
		t.Fatalf("read refreshed metadata deadline: %v", err)
	}
	if scheduledAt != metadataNotBefore {
		t.Fatalf("metadata job scheduled_at = %q, want location deadline %q", scheduledAt, metadataNotBefore)
	}
	if jobs, err := service.nextLocalMetadataJobs(context.Background(), "1111111111111111", 10); err != nil {
		t.Fatalf("nextLocalMetadataJobs(before refreshed deadline) error = %v", err)
	} else if len(jobs) != 0 {
		t.Fatalf("nextLocalMetadataJobs(before refreshed deadline) = %+v, want no runnable job", jobs)
	}

	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_scan_jobs SET scheduled_at = ? WHERE id = ?`, past, jobID); err != nil {
		t.Fatalf("simulate selected metadata job: %v", err)
	}
	future := formatCatalogTime(time.Now().UTC().Add(2 * time.Minute))
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_asset_locations SET metadata_not_before = ? WHERE id = (SELECT location_id FROM local_scan_jobs WHERE id = ?)`, future, jobID); err != nil {
		t.Fatalf("move location deadline after selection: %v", err)
	}
	trustedRoot, err := service.acquireTrustedLocalMediaRoot(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("acquireTrustedLocalMediaRoot() error = %v", err)
	}
	claimed, err := service.claimLocalMetadataJob(context.Background(), localMetadataJob{
		ID:             jobID,
		SourceKey:      "1111111111111111",
		RootKey:        jobRootKey,
		RootGeneration: jobRootGeneration,
	}, trustedRoot, formatCatalogTime(time.Now().UTC()))
	_ = trustedRoot.Close()
	if err != nil {
		t.Fatalf("claimLocalMetadataJob() error = %v", err)
	}
	if claimed {
		t.Fatal("claimLocalMetadataJob() = true, want latest location deadline enforced")
	}
}

func TestLocalMetadataRegistrationRequeuesWhenRediscoveredAfterClaim(t *testing.T) {
	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "copying.jpg")
	if err := os.WriteFile(mediaPath, []byte("partial"), 0o644); err != nil {
		t.Fatalf("WriteFile(partial) error = %v", err)
	}
	service := newLocalPhase0SettlingTestService(t, rootPath, "2m")
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(initial) error = %v", err)
	}

	var job localMetadataJob
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT id, source_key, COALESCE(root_key, ''), root_generation, location_id
		FROM local_scan_jobs
		WHERE job_kind = 'metadata' AND status = 'queued'`).Scan(&job.ID, &job.SourceKey, &job.RootKey, &job.RootGeneration, &job.LocationID); err != nil {
		t.Fatalf("read initial metadata job: %v", err)
	}
	past := formatCatalogTime(time.Now().UTC().Add(-time.Minute))
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_asset_locations SET metadata_not_before = ? WHERE id = ?`, past, job.LocationID); err != nil {
		t.Fatalf("make initial location eligible: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_scan_jobs SET scheduled_at = ? WHERE id = ?`, past, job.ID); err != nil {
		t.Fatalf("make initial metadata job eligible: %v", err)
	}
	trustedRoot, err := service.acquireTrustedLocalMediaRoot(context.Background(), job.SourceKey)
	if err != nil {
		t.Fatalf("acquireTrustedLocalMediaRoot() error = %v", err)
	}
	claimed, err := service.claimLocalMetadataJob(context.Background(), job, trustedRoot, formatCatalogTime(time.Now().UTC()))
	_ = trustedRoot.Close()
	if err != nil || !claimed {
		t.Fatalf("claimLocalMetadataJob() claimed=%t error=%v", claimed, err)
	}
	claimedLocation, err := service.localLocationForMetadata(context.Background(), job.LocationID)
	if err != nil {
		t.Fatalf("localLocationForMetadata(before rediscovery) error = %v", err)
	}

	if err := os.WriteFile(mediaPath, []byte("completed-copy"), 0o644); err != nil {
		t.Fatalf("WriteFile(completed) error = %v", err)
	}
	rediscovered, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan(rediscovery) error = %v", err)
	}
	if rediscovered.ChangedPaths != 1 || rediscovered.QueuedMetadata != 0 {
		t.Fatalf("rediscovery result = %+v, want changed running job without duplicate queue", rediscovered)
	}
	guardTx, err := service.catalog.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin registration guard check: %v", err)
	}
	eligible, err := lockLocalMetadataRegistrationInTx(context.Background(), guardTx, claimedLocation, job.ID, formatCatalogTime(time.Now().UTC()))
	_ = guardTx.Rollback()
	if err != nil {
		t.Fatalf("lockLocalMetadataRegistrationInTx(old generation) error = %v", err)
	}
	if eligible {
		t.Fatal("old worker location generation remained registration-eligible after rediscovery")
	}

	location, err := service.localLocationForMetadata(context.Background(), job.LocationID)
	if err != nil {
		t.Fatalf("localLocationForMetadata() error = %v", err)
	}
	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatalf("Stat(completed) error = %v", err)
	}
	sha1Hex, _, err := sha1File(mediaPath)
	if err != nil {
		t.Fatalf("sha1File(completed) error = %v", err)
	}
	datasource, _, err := service.localDatasourceAndRoot(job.SourceKey)
	if err != nil {
		t.Fatalf("localDatasourceAndRoot() error = %v", err)
	}
	err = service.registerLocalMetadata(
		context.Background(),
		*datasource,
		location,
		sha1Hex,
		"image",
		filepath.Base(mediaPath),
		info.ModTime().UTC(),
		info,
		formatCatalogTime(time.Now().UTC()),
		job.ID,
	)
	if !errors.Is(err, errLocalMetadataSourceChanged) {
		t.Fatalf("registerLocalMetadata(after rediscovery) error = %v, want source changed", err)
	}

	var status string
	var scheduledAt string
	var metadataNotBefore string
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT j.status, j.scheduled_at, l.metadata_not_before
		FROM local_scan_jobs j
		JOIN local_asset_locations l ON l.id = j.location_id
		WHERE j.id = ?`, job.ID).Scan(&status, &scheduledAt, &metadataNotBefore); err != nil {
		t.Fatalf("read requeued metadata state: %v", err)
	}
	if status != "queued" || scheduledAt != metadataNotBefore {
		t.Fatalf("metadata state status=%q scheduled_at=%q not_before=%q, want queued at latest deadline", status, scheduledAt, metadataNotBefore)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE id = `+fmt.Sprint(job.LocationID)+` AND status = 'discovered'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets`, 0)

	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_asset_locations SET metadata_not_before = ? WHERE id = ?`, past, job.LocationID); err != nil {
		t.Fatalf("make rediscovered location eligible: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_scan_jobs SET scheduled_at = ? WHERE id = ?`, past, job.ID); err != nil {
		t.Fatalf("make requeued metadata job eligible: %v", err)
	}
	result, err := service.RunLocalMetadataBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch(retry) error = %v", err)
	}
	if result.RegisteredAssets != 1 || result.FailedJobs != 0 {
		t.Fatalf("metadata retry result = %+v, want one registration", result)
	}
}

func TestLocalThumbnailReturnsChangedSourceToSettling(t *testing.T) {
	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "family.jpg")
	if err := os.WriteFile(mediaPath, encodeJPEGForTest(t, 320, 240), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	helperPath, _ := writeFakeMediaHelperImageScript(t)
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: "1ns",
		},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaHelperPath: helperPath,
		LocalRoots:      []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v", result, err)
	}
	datasource := service.datasourceStateSnapshot().datasources["1111111111111111"]
	datasource.Scan.SettlingDuration = "2m"
	service.ReconfigureDatasources([]config.DatasourceConfig{datasource})
	if err := os.WriteFile(mediaPath, encodeJPEGForTest(t, 640, 480), 0o644); err != nil {
		t.Fatalf("replace source image: %v", err)
	}
	result, err := service.RunLocalThumbnailBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalThumbnailBatch() error = %v", err)
	}
	if result.ResettledAssets != 1 || result.FailedJobs != 0 || result.GeneratedAssets != 0 {
		t.Fatalf("thumbnail result = %+v, want changed source resettled", result)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions WHERE status = 'ready'`, 0)
	state, err := service.LocalMetadataQueueState(context.Background())
	if err != nil {
		t.Fatalf("LocalMetadataQueueState() error = %v", err)
	}
	if state.Settling != 1 || state.Queued != 0 {
		t.Fatalf("metadata queue state = %+v, want thumbnail source settling", state)
	}
}

func TestCommittedLocalUploadQueuesMetadataImmediately(t *testing.T) {
	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "uploaded.jpg")
	if err := os.WriteFile(mediaPath, []byte("verified-upload"), 0o644); err != nil {
		t.Fatalf("WriteFile(upload) error = %v", err)
	}
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: "2m",
		},
	}}, ServiceOptions{
		DataDir:    t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued'`, 1)
	queued, err := service.QueueCommittedLocalUpload(context.Background(), "1111111111111111", mediaPath)
	if err != nil || !queued {
		t.Fatalf("QueueCommittedLocalUpload() queued=%t error=%v", queued, err)
	}
	state, err := service.LocalMetadataQueueState(context.Background())
	if err != nil {
		t.Fatalf("LocalMetadataQueueState() error = %v", err)
	}
	if state.Queued != 1 || state.Settling != 0 {
		t.Fatalf("metadata queue state = %+v, want immediately queued upload", state)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued'`, 1)
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v", result, err)
	}
}

func TestLocalPhase0ScanDoesNotMarkCommittedUploadAfterWalkPassedPath(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "a"), 0o755); err != nil {
		t.Fatalf("MkdirAll(a) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(rootPath, "z"), 0o755); err != nil {
		t.Fatalf("MkdirAll(z) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "z", "trigger.jpg"), []byte("trigger"), 0o644); err != nil {
		t.Fatalf("WriteFile(trigger) error = %v", err)
	}

	originalWalkDir := localPhase0WalkDir
	t.Cleanup(func() { localPhase0WalkDir = originalWalkDir })
	uploadPath := filepath.Join(rootPath, "a", "uploaded.jpg")
	var (
		uploadOnce sync.Once
		uploadErr  error
	)
	localPhase0WalkDir = func(_ string, rootFS fs.FS, fn fs.WalkDirFunc) error {
		return fs.WalkDir(rootFS, ".", func(path string, entry fs.DirEntry, walkErr error) error {
			if err := fn(path, entry, walkErr); err != nil {
				return err
			}
			if path != "z" || walkErr != nil {
				return nil
			}
			uploadOnce.Do(func() {
				if err := os.WriteFile(uploadPath, []byte("verified-upload"), 0o644); err != nil {
					uploadErr = err
					return
				}
				queued, err := service.QueueCommittedLocalUpload(context.Background(), "1111111111111111", uploadPath)
				if err != nil {
					uploadErr = err
					return
				}
				if !queued {
					uploadErr = errors.New("committed upload did not queue metadata")
				}
			})
			return uploadErr
		})
	}

	result, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if uploadErr != nil {
		t.Fatalf("QueueCommittedLocalUpload() error = %v", uploadErr)
	}
	if result.Status != localPhase0StatusCompleted || result.MissingPaths != 0 {
		t.Fatalf("scan result = %+v, want committed upload retained", result)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations
		WHERE relative_path = 'a/uploaded.jpg' AND status = 'discovered'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE job_kind = 'metadata'
			AND status = 'queued'
			AND location_id = (
				SELECT id FROM local_asset_locations WHERE relative_path = 'a/uploaded.jpg'
			)`, 1)
}

func TestLocalPhase0MissingUpdateRechecksScanStartBoundary(t *testing.T) {
	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "uploaded.jpg")
	if err := os.WriteFile(mediaPath, []byte("verified-upload"), 0o644); err != nil {
		t.Fatalf("WriteFile(upload) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}
	datasource, root, err := service.localDatasourceAndRoot("1111111111111111")
	if err != nil {
		t.Fatalf("localDatasourceAndRoot() error = %v", err)
	}
	startedAt := time.Now().UTC()
	scanner := &localPhase0Scanner{
		service:    service,
		datasource: *datasource,
		root:       *root,
		startedAt:  startedAt,
		nowText:    formatCatalogTime(startedAt),
		seen:       map[string]struct{}{},
		scanMode:   localPhase0ScanModeReconciliation,
	}
	missingIDs, _, _, err := scanner.missingLocationIDs(context.Background())
	if err != nil {
		t.Fatalf("missingLocationIDs() error = %v", err)
	}
	if len(missingIDs) != 1 {
		t.Fatalf("missingLocationIDs() = %v, want one pre-scan candidate", missingIDs)
	}
	for formatCatalogTime(time.Now().UTC()) <= scanner.nowText {
		runtime.Gosched()
	}
	if _, err := service.QueueCommittedLocalUpload(context.Background(), "1111111111111111", mediaPath); err != nil {
		t.Fatalf("QueueCommittedLocalUpload() error = %v", err)
	}
	updated, err := scanner.markLocationIDs(context.Background(), missingIDs, "missing", "phase0_absent")
	if err != nil {
		t.Fatalf("markLocationIDs() error = %v", err)
	}
	if updated != 0 {
		t.Fatalf("markLocationIDs() updated = %d, want post-scan-start upload protected", updated)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations
		WHERE relative_path = 'uploaded.jpg' AND status != 'missing'`, 1)
}

func TestLocalFastPhase0ScanDefersInPlaceFileChangesUntilFullScan(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	albumPath := filepath.Join(rootPath, "album")
	if err := os.MkdirAll(albumPath, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	mediaPath := filepath.Join(albumPath, "photo.jpg")
	if err := os.WriteFile(mediaPath, []byte("before"), 0o644); err != nil {
		t.Fatalf("WriteFile(before) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)

	full, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if full.ScanMode != localPhase0ScanModeReconciliation {
		t.Fatalf("reconciliation mode = %q, want %q", full.ScanMode, localPhase0ScanModeReconciliation)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	albumInfo, err := os.Stat(albumPath)
	if err != nil {
		t.Fatalf("Stat(album) error = %v", err)
	}
	if err := os.WriteFile(mediaPath, []byte("after-with-a-different-size"), 0o644); err != nil {
		t.Fatalf("WriteFile(after) error = %v", err)
	}
	if err := os.Chtimes(albumPath, albumInfo.ModTime(), albumInfo.ModTime()); err != nil {
		t.Fatalf("Chtimes(album) error = %v", err)
	}

	fast, err := service.RunLocalQuickDiscoveryScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalQuickDiscoveryScan() error = %v", err)
	}
	if fast.ScanMode != localPhase0ScanModeQuick || fast.DiscoveredPaths != 0 || fast.ChangedPaths != 0 || fast.QueuedMetadata != 0 {
		t.Fatalf("quick discovery result = %+v, want unchanged directory files deferred", fast)
	}

	reconcile, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("reconcile RunLocalReconciliationScan() error = %v", err)
	}
	if reconcile.ChangedPaths != 1 || reconcile.QueuedMetadata != 1 {
		t.Fatalf("reconcile scan result = %+v, want deferred file change detected", reconcile)
	}
}

func TestLocalFastPhase0ScanRetriesFileStatFailureWithoutMarkingMissing(t *testing.T) {
	rootPath := t.TempDir()
	albumPath := filepath.Join(rootPath, "album")
	if err := os.MkdirAll(albumPath, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	mediaPath := filepath.Join(albumPath, "photo.jpg")
	if err := os.WriteFile(mediaPath, []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}

	var originalDirectoryMTime string
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT mtime FROM local_scan_directories
		WHERE source_key = '1111111111111111' AND root_key = 'nas-photos' AND relative_path = 'album'`).Scan(&originalDirectoryMTime); err != nil {
		t.Fatalf("read original directory state: %v", err)
	}
	changedAt := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(albumPath, changedAt, changedAt); err != nil {
		t.Fatalf("Chtimes(album) error = %v", err)
	}

	originalWalkDir := localPhase0WalkDir
	t.Cleanup(func() { localPhase0WalkDir = originalWalkDir })
	localPhase0WalkDir = func(logicalRoot string, rootFS fs.FS, fn fs.WalkDirFunc) error {
		return fs.WalkDir(rootFS, ".", func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr == nil && localPhase0LogicalWalkPath(logicalRoot, path) == mediaPath {
				entry = localPhase0InfoErrorDirEntry{DirEntry: entry, err: errors.New("transient stat failure")}
			}
			return fn(path, entry, walkErr)
		})
	}

	failed, err := service.RunLocalQuickDiscoveryScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalQuickDiscoveryScan(stat failure) error = %v", err)
	}
	if failed.MissingPaths != 0 || failed.SkipCounts["stat_error"] != 1 {
		t.Fatalf("stat-failure quick discovery = %+v, want preserved path and one stat error", failed)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'active'`, 1)
	var retainedDirectoryMTime string
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT mtime FROM local_scan_directories
		WHERE source_key = '1111111111111111' AND root_key = 'nas-photos' AND relative_path = 'album'`).Scan(&retainedDirectoryMTime); err != nil {
		t.Fatalf("read retained directory state: %v", err)
	}
	if retainedDirectoryMTime != originalDirectoryMTime {
		t.Fatalf("directory mtime after stat failure = %q, want retained %q for retry", retainedDirectoryMTime, originalDirectoryMTime)
	}

	localPhase0WalkDir = originalWalkDir
	retried, err := service.RunLocalQuickDiscoveryScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalQuickDiscoveryScan(retry) error = %v", err)
	}
	if retried.DiscoveredPaths != 1 || retried.MissingPaths != 0 {
		t.Fatalf("retried quick discovery = %+v, want unchanged file inspected again", retried)
	}
}

func TestLocalFastPhase0ScanTraversesUnchangedAncestors(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	yearPath := filepath.Join(rootPath, "2026")
	albumPath := filepath.Join(yearPath, "trip")
	if err := os.MkdirAll(albumPath, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	rootInfo, err := os.Stat(rootPath)
	if err != nil {
		t.Fatalf("Stat(root) error = %v", err)
	}
	yearInfo, err := os.Stat(yearPath)
	if err != nil {
		t.Fatalf("Stat(year) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(albumPath, "new.jpg"), []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(new) error = %v", err)
	}
	changedAt := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(albumPath, changedAt, changedAt); err != nil {
		t.Fatalf("Chtimes(album) error = %v", err)
	}
	if err := os.Chtimes(yearPath, yearInfo.ModTime(), yearInfo.ModTime()); err != nil {
		t.Fatalf("Chtimes(year) error = %v", err)
	}
	if err := os.Chtimes(rootPath, rootInfo.ModTime(), rootInfo.ModTime()); err != nil {
		t.Fatalf("Chtimes(root) error = %v", err)
	}

	result, err := service.RunLocalQuickDiscoveryScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalQuickDiscoveryScan() error = %v", err)
	}
	if result.DiscoveredPaths != 1 || result.ChangedPaths != 1 || result.QueuedMetadata != 1 {
		t.Fatalf("quick discovery result = %+v, want nested addition discovered through unchanged ancestors", result)
	}
}

func TestLocalFastPhase0ScanMarksRemovedDirectoryMediaMissing(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	yearPath := filepath.Join(rootPath, "2026")
	albumPath := filepath.Join(yearPath, "trip")
	if err := os.MkdirAll(albumPath, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(albumPath, "photo.jpg"), []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	rootInfo, err := os.Stat(rootPath)
	if err != nil {
		t.Fatalf("Stat(root) error = %v", err)
	}
	yearInfo, err := os.Stat(yearPath)
	if err != nil {
		t.Fatalf("Stat(year) error = %v", err)
	}
	if err := os.RemoveAll(albumPath); err != nil {
		t.Fatalf("RemoveAll(album) error = %v", err)
	}
	if err := os.Chtimes(yearPath, yearInfo.ModTime(), yearInfo.ModTime()); err != nil {
		t.Fatalf("Chtimes(year) error = %v", err)
	}
	if err := os.Chtimes(rootPath, rootInfo.ModTime(), rootInfo.ModTime()); err != nil {
		t.Fatalf("Chtimes(root) error = %v", err)
	}

	result, err := service.RunLocalQuickDiscoveryScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalQuickDiscoveryScan() error = %v", err)
	}
	if result.MissingPaths != 1 {
		t.Fatalf("quick discovery result = %+v, want removed directory media marked missing", result)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'missing'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_directories WHERE relative_path = '2026/trip'`, 0)
}

func TestLocalReconciliationDueUsesDailyScheduleAndIgnoresQuickDiscovery(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	service := newLocalPhase0TestService(t, rootPath)
	due, err := service.LocalReconciliationDue(context.Background(), "1111111111111111", time.Now().UTC())
	if err != nil {
		t.Fatalf("LocalReconciliationDue(before reconciliation) error = %v", err)
	}
	if !due {
		t.Fatal("LocalReconciliationDue(before reconciliation) = false, want true")
	}
	reconciliation, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	quick, err := service.RunLocalQuickDiscoveryScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalQuickDiscoveryScan() error = %v", err)
	}
	statuses, err := service.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses() error = %v", err)
	}
	if len(statuses) != 1 || statuses[0].LastQuickScanAt == nil || !statuses[0].LastQuickScanAt.Equal(quick.CompletedAt) ||
		statuses[0].LastReconciliationAt == nil || !statuses[0].LastReconciliationAt.Equal(reconciliation.CompletedAt) {
		t.Fatalf("scan mode completion times = %+v, want quick=%v reconciliation=%v", statuses, quick.CompletedAt, reconciliation.CompletedAt)
	}
	quickDue, err := service.LocalQuickDiscoveryDue(context.Background(), "1111111111111111", quick.CompletedAt.Add(5*time.Minute-time.Second), 5*time.Minute)
	if err != nil {
		t.Fatalf("LocalQuickDiscoveryDue(before deadline) error = %v", err)
	}
	if quickDue {
		t.Fatal("LocalQuickDiscoveryDue(before deadline) = true, want false")
	}
	quickDue, err = service.LocalQuickDiscoveryDue(context.Background(), "1111111111111111", quick.CompletedAt.Add(5*time.Minute), 5*time.Minute)
	if err != nil {
		t.Fatalf("LocalQuickDiscoveryDue(at deadline) error = %v", err)
	}
	if !quickDue {
		t.Fatal("LocalQuickDiscoveryDue(at deadline) = false, want true")
	}
	due, err = service.LocalReconciliationDue(context.Background(), "1111111111111111", reconciliation.CompletedAt.Add(-time.Second))
	if err != nil {
		t.Fatalf("LocalReconciliationDue(for already completed schedule) error = %v", err)
	}
	if due {
		t.Fatal("LocalReconciliationDue(for already completed schedule) = true, want false")
	}
	due, err = service.LocalReconciliationDue(context.Background(), "1111111111111111", reconciliation.CompletedAt.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("LocalReconciliationDue(for next daily schedule) error = %v", err)
	}
	if !due {
		t.Fatal("LocalReconciliationDue(for next daily schedule) = false, want true")
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_scan_runs
		SET skip_counts_json = '{"read_error":1}'
		WHERE id = (SELECT MAX(id) FROM local_scan_runs WHERE scan_mode = 'reconciliation')`); err != nil {
		t.Fatalf("mark reconciliation scan partial error: %v", err)
	}
	due, err = service.LocalReconciliationDue(context.Background(), "1111111111111111", reconciliation.CompletedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("LocalReconciliationDue(after partial reconciliation) error = %v", err)
	}
	if !due {
		t.Fatal("LocalReconciliationDue(after partial reconciliation) = false, want true")
	}
}

func TestLocalPhase0NextRunUsesEffectiveQuickScanInterval(t *testing.T) {
	tests := []struct {
		name string
		scan *config.LocalDatasourceScanConfig
		want time.Duration
	}{
		{
			name: "configured quick scan",
			scan: &config.LocalDatasourceScanConfig{QuickScanInterval: "1m"},
			want: time.Minute,
		},
		{
			name: "default",
			want: defaultLocalQuickScanInterval,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, err := NewServiceWithOptions([]config.DatasourceConfig{{
				SourceKey: "1111111111111111",
				Name:      "NAS Photos",
				Kind:      config.DatasourceKindLocalFiles,
				RootKey:   "nas-photos",
				Scan:      test.scan,
			}}, ServiceOptions{
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

			result, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
			if err != nil {
				t.Fatalf("RunLocalReconciliationScan() error = %v", err)
			}
			statuses, err := service.LocalDatasourceScanStatuses(context.Background())
			if err != nil {
				t.Fatalf("LocalDatasourceScanStatuses() error = %v", err)
			}
			if len(statuses) != 1 || statuses[0].NextPhase0At == nil {
				t.Fatalf("scan statuses = %+v, want one next run deadline", statuses)
			}
			want := result.CompletedAt.Add(test.want)
			if !statuses[0].NextPhase0At.Equal(want) {
				t.Fatalf("NextPhase0At = %s, want %s", statuses[0].NextPhase0At, want)
			}
		})
	}
}

func newLocalPhase0TestService(t *testing.T, rootPath string) *Service {
	return newLocalPhase0SettlingTestService(t, rootPath, "1ns")
}

func TestLocalFastScanPersistsLatestTimeAndPrunesHistory(t *testing.T) {
	t.Parallel()

	service := newLocalPhase0TestService(t, t.TempDir())
	ctx := context.Background()
	var latest LocalPhase0ScanResult
	for index := 0; index < localScanRunRetentionPerMode+6; index++ {
		result, err := service.RunLocalQuickDiscoveryScan(ctx, "1111111111111111")
		if err != nil {
			t.Fatalf("RunLocalQuickDiscoveryScan(%d) error = %v", index, err)
		}
		latest = result
	}
	var retained int
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM local_scan_runs
		WHERE source_key = ? AND root_key = ? AND scan_mode = ?`,
		"1111111111111111", "nas-photos", localPhase0ScanModeQuick).Scan(&retained); err != nil {
		t.Fatalf("count retained quick scans: %v", err)
	}
	if retained != localScanRunRetentionPerMode {
		t.Fatalf("retained quick scans = %d, want %d", retained, localScanRunRetentionPerMode)
	}
	statuses, err := service.LocalDatasourceScanStatuses(ctx)
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses() error = %v", err)
	}
	if len(statuses) != 1 || statuses[0].LastQuickScanAt == nil || !statuses[0].LastQuickScanAt.Equal(latest.CompletedAt) {
		t.Fatalf("scan statuses = %#v, want latest quick completion %s", statuses, latest.CompletedAt)
	}
	if statuses[0].LastReconciliationAt != nil {
		t.Fatalf("LastReconciliationAt = %s, want nil", statuses[0].LastReconciliationAt)
	}
}

func newLocalPhase0SettlingTestService(t *testing.T, rootPath string, settlingDuration string) *Service {
	t.Helper()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: settlingDuration,
		},
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

type localPhase0InfoErrorDirEntry struct {
	fs.DirEntry
	err error
}

func (e localPhase0InfoErrorDirEntry) Info() (fs.FileInfo, error) {
	return nil, e.err
}

func TestLocalPhase0ScanDoesNotHoldWriterForWholeWalk(t *testing.T) {
	rootPath := t.TempDir()
	for index := 0; index < localPhase0WriteBatchSize; index++ {
		name := fmt.Sprintf("photo-%03d.jpg", index)
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte("image"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	originalWalkDir := localPhase0WalkDir
	flushed := make(chan struct{})
	release := make(chan struct{})
	released := false
	t.Cleanup(func() {
		localPhase0WalkDir = originalWalkDir
		if !released {
			close(release)
		}
	})
	localPhase0WalkDir = func(_ string, rootFS fs.FS, fn fs.WalkDirFunc) error {
		rootInfo, err := fs.Stat(rootFS, ".")
		if err != nil {
			return err
		}
		if err := fn(".", fs.FileInfoToDirEntry(rootInfo), nil); err != nil {
			return err
		}
		entries, err := fs.ReadDir(rootFS, ".")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := fn(entry.Name(), entry, nil); err != nil {
				return err
			}
		}
		close(flushed)
		<-release
		return nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
		done <- err
	}()

	select {
	case <-flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("phase0 scan did not reach the first write batch")
	}

	writerDB, err := sql.Open("sqlite", service.catalog.path)
	if err != nil {
		t.Fatalf("Open(second writer) error = %v", err)
	}
	defer writerDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := writerDB.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE while phase0 walk is paused error = %v, want no long-held writer transaction", err)
	}
	if _, err := writerDB.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatalf("ROLLBACK second writer error = %v", err)
	}

	released = true
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
}

func TestLocalPhase0MissingDetectionUsesReadOnlyPool(t *testing.T) {
	rootPath := t.TempDir()
	for _, name := range []string{"keep.jpg", "gone.jpg"} {
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte("image"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	datasource := config.DatasourceConfig{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}
	root := config.LocalMediaRootConfig{
		Key:  "nas-photos",
		Path: rootPath,
	}
	service, err := NewServiceWithOptions([]config.DatasourceConfig{datasource}, ServiceOptions{
		DataDir:    t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{root},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	if _, err := service.RunLocalReconciliationScan(context.Background(), datasource.SourceKey); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}

	blockingTx, err := service.catalog.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin blocking tx: %v", err)
	}
	defer blockingTx.Rollback()

	scanner := localPhase0Scanner{
		service:    service,
		datasource: datasource,
		root:       root,
		seen:       map[string]struct{}{"keep.jpg": struct{}{}},
		nowText:    formatCatalogTime(time.Now().UTC()),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	missingIDs, blockedIDs, currentLocationCount, err := scanner.missingLocationIDs(ctx)
	if err != nil {
		t.Fatalf("missingLocationIDs() while writer handle busy error = %v", err)
	}
	if len(missingIDs) != 1 || len(blockedIDs) != 0 || currentLocationCount != 2 {
		t.Fatalf("missingLocationIDs() missing=%v blocked=%v current=%d, want one missing, no blocked, and two current", missingIDs, blockedIDs, currentLocationCount)
	}
}

func TestLocalPhase0DiagnosticRowsIncludeLocationState(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "family.jpg")
	if err := os.WriteFile(mediaPath, []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	rows, err := service.LocalPhase0DiagnosticRows(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("LocalPhase0DiagnosticRows() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("diagnostic rows length = %d, want 1: %#v", len(rows), rows)
	}
	if rows[0].RelativePath != "family.jpg" ||
		rows[0].LocationStatus != "active" ||
		rows[0].ReasonCode != "active" ||
		rows[0].AssetVisibilityStatus != "active" ||
		rows[0].ScanRunID == "" {
		t.Fatalf("active diagnostic row = %+v, want active registered row", rows[0])
	}

	if err := os.Remove(mediaPath); err != nil {
		t.Fatalf("Remove(image) error = %v", err)
	}
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(after remove) error = %v", err)
	}
	rows, err = service.LocalPhase0DiagnosticRows(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("LocalPhase0DiagnosticRows(after remove) error = %v", err)
	}
	if len(rows) != 1 ||
		rows[0].LocationStatus != "missing" ||
		rows[0].ReasonCode != "phase0_absent" ||
		rows[0].StatusReason != "phase0_absent" {
		t.Fatalf("missing diagnostic rows = %#v, want one phase0_absent row", rows)
	}
}

func TestLocalFailureDiagnosticRowsIncludeErrorDetails(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "broken.jpg"), []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	var assetID string
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT asset_id FROM local_assets WHERE source_key = ?`, "1111111111111111").Scan(&assetID); err != nil {
		t.Fatalf("query local asset id: %v", err)
	}
	now := "2026-06-20T00:00:00Z"
	if _, err := service.catalog.db.ExecContext(context.Background(), `
		UPDATE local_scan_root_state
		SET root_status = 'unreadable',
			root_last_error = 'permission denied',
			updated_at = ?
		WHERE source_key = ?`, now, "1111111111111111"); err != nil {
		t.Fatalf("mark root failed: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, asset_id, status, attempts, scheduled_at, completed_at, last_error
		) VALUES (?, 'metadata', 0, 'nas-photos', ?, 'failed', 2, ?, ?, ?)`,
		"1111111111111111", assetID, now, now, "metadata parser failed"); err != nil {
		t.Fatalf("insert metadata failure: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `
		UPDATE local_assets SET thumbnail_status = 'failed', updated_at = ? WHERE source_key = ? AND asset_id = ?`,
		now, "1111111111111111", assetID); err != nil {
		t.Fatalf("mark thumbnail failed: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_renditions (
			source_key, asset_id, kind, status, last_error
		) VALUES (?, ?, 'preview', 'failed', ?)`,
		"1111111111111111", assetID, "ffmpeg decoder missing"); err != nil {
		t.Fatalf("insert thumbnail failure: %v", err)
	}
	insertSemanticVectorForTest(t,
		service.catalog,
		context.Background(),
		"1111111111111111",
		assetID,
		"model-a",
		"space-a",
		1,
		[]float32{0},
		"preview",
		"failed",
		"embedding runtime failed",
		now,
		nil,
	)
	statuses, err := service.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses() error = %v", err)
	}
	if len(statuses) != 1 || statuses[0].FailedEmbeddingJobs != 1 {
		t.Fatalf("LocalDatasourceScanStatuses() = %+v, want one failed embedding job", statuses)
	}

	rows, err := service.LocalFailureDiagnosticRows(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("LocalFailureDiagnosticRows() error = %v", err)
	}
	failures := map[string]LocalFailureDiagnosticRow{}
	for _, row := range rows {
		failures[row.FailureKind] = row
	}
	for _, kind := range []string{"media_discovery", "metadata", "thumbnail", "embedding"} {
		if _, ok := failures[kind]; !ok {
			t.Fatalf("failure kind %q missing from rows: %#v", kind, rows)
		}
	}
	if failures["media_discovery"].LastError != "permission denied" ||
		failures["metadata"].LastError != "metadata parser failed" ||
		failures["metadata"].Attempts != 2 ||
		failures["thumbnail"].LastError != "ffmpeg decoder missing" ||
		failures["thumbnail"].RelativePath != "broken.jpg" ||
		failures["embedding"].LastError != "embedding runtime failed" ||
		failures["embedding"].Component != "model-a" {
		t.Fatalf("failure rows = %#v, want detailed errors and relative path", rows)
	}
}

func TestLocalMetadataRequeueIsAsynchronousPrioritizedAndIdempotent(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	retryPath := filepath.Join(rootPath, "retry-family.jpg")
	retryMTime := time.Date(2026, 7, 16, 9, 30, 0, 0, time.UTC)
	if err := os.WriteFile(retryPath, []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	if err := os.Chtimes(retryPath, retryMTime, retryMTime); err != nil {
		t.Fatalf("Chtimes(image) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "ordinary-family.jpg"), []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(ordinary image) error = %v", err)
	}

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued'`, 2)
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_scan_jobs
		SET status = 'failed',
			completed_at = ?,
			last_error = 'transient metadata failure'
		WHERE job_kind = 'metadata'
			AND location_id IN (
				SELECT id FROM local_asset_locations WHERE relative_path = 'retry-family.jpg'
			)`,
		formatCatalogTime(time.Now().UTC()),
	); err != nil {
		t.Fatalf("mark metadata failed: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_scan_jobs
		SET scheduled_at = ?
		WHERE job_kind = 'metadata'
			AND status = 'queued'
			AND location_id IN (
				SELECT id FROM local_asset_locations WHERE relative_path = 'ordinary-family.jpg'
			)`,
		formatCatalogTime(time.Now().UTC().Add(-time.Hour)),
	); err != nil {
		t.Fatalf("age ordinary metadata job: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, location_id, status, scheduled_at, sort_at
		)
		SELECT source_key, job_kind, ?, root_key, location_id, 'queued', ?, sort_at
		FROM local_scan_jobs
		WHERE job_kind = 'metadata'
			AND status = 'failed'
			AND location_id IN (
				SELECT id FROM local_asset_locations WHERE relative_path = 'retry-family.jpg'
			)`,
		localMetadataBackgroundPriority,
		formatCatalogTime(time.Now().UTC().Add(-time.Hour)),
	); err != nil {
		t.Fatalf("insert existing queued metadata job: %v", err)
	}
	statuses, err := service.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses() error = %v", err)
	}
	if len(statuses) != 1 || statuses[0].FailedMetadataJobs != 1 {
		t.Fatalf("status before repair = %+v, want one failed metadata job", statuses)
	}

	requeue, err := service.RequeueFailedLocalMetadata(context.Background())
	if err != nil {
		t.Fatalf("RequeueFailedLocalMetadata() error = %v", err)
	}
	if requeue.Queued != 1 {
		t.Fatalf("metadata requeue = %+v, want one queued retry", requeue)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'failed'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued'`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued' AND priority = 0`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM catalog_assets WHERE datasource_kind = 'local_filesystem'`, 0)

	requeue, err = service.RequeueFailedLocalMetadata(context.Background())
	if err != nil {
		t.Fatalf("second RequeueFailedLocalMetadata() error = %v", err)
	}
	if requeue.Queued != 0 {
		t.Fatalf("second metadata requeue = %+v, want idempotent no-op", requeue)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued'`, 2)

	retiredRetryPath := retryPath + ".retired"
	if err := os.Rename(retryPath, retiredRetryPath); err != nil {
		t.Fatalf("Rename(retry image) error = %v", err)
	}
	if err := os.Mkdir(retryPath, 0o755); err != nil {
		t.Fatalf("Mkdir(retry image path) error = %v", err)
	}
	failedBatch, err := service.RunLocalMetadataBatch(context.Background(), 1)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch(failing retry) error = %v", err)
	}
	if failedBatch.ProcessedJobs != 1 || failedBatch.FailedJobs != 1 || failedBatch.RegisteredAssets != 0 {
		t.Fatalf("failing metadata retry batch = %+v, want one failed repair-priority job", failedBatch)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'failed'`, 1)

	if err := os.Remove(retryPath); err != nil {
		t.Fatalf("Remove(retry image directory) error = %v", err)
	}
	if err := os.WriteFile(retryPath, []byte("image"), 0o644); err != nil {
		t.Fatalf("restore retry image error = %v", err)
	}
	if err := os.Chtimes(retryPath, retryMTime, retryMTime); err != nil {
		t.Fatalf("restore retry image mtime error = %v", err)
	}
	requeue, err = service.RequeueFailedLocalMetadata(context.Background())
	if err != nil {
		t.Fatalf("third RequeueFailedLocalMetadata() error = %v", err)
	}
	if requeue.Queued != 1 {
		t.Fatalf("third metadata requeue = %+v, want failed job queued again", requeue)
	}

	metadata, err := service.RunLocalMetadataBatch(context.Background(), 1)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	if metadata.ProcessedJobs != 1 || metadata.CompletedJobs != 1 || metadata.FailedJobs != 0 || metadata.SettlingJobs != 1 || metadata.RegisteredAssets != 0 {
		t.Fatalf("metadata retry batch = %+v, want replacement resettled before registration", metadata)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND priority = 0`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued'`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE filename = 'retry-family.jpg' AND visibility_status = 'active'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE filename = 'ordinary-family.jpg'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM catalog_assets WHERE datasource_kind = 'local_filesystem' AND visibility_status = 'active'`, 0)
}

func TestLocalMetadataRequeuePreservesSettlingDeadline(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "settling.jpg"), []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1h"},
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_scan_jobs
		SET status = 'failed', attempts = 3, completed_at = ?, last_error = 'transient metadata failure'
		WHERE job_kind = 'metadata'`, formatCatalogTime(time.Now().UTC())); err != nil {
		t.Fatalf("mark metadata failed: %v", err)
	}

	requeue, err := service.RequeueFailedLocalMetadata(context.Background())
	if err != nil {
		t.Fatalf("RequeueFailedLocalMetadata() error = %v", err)
	}
	if requeue.Queued != 1 {
		t.Fatalf("metadata requeue = %+v, want one settling retry", requeue)
	}
	var status, scheduledAt, metadataNotBefore string
	var priority, attempts int
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT j.status, j.priority, j.attempts, j.scheduled_at, l.metadata_not_before
		FROM local_scan_jobs j
		JOIN local_asset_locations l ON l.id = j.location_id
		WHERE j.job_kind = 'metadata'`).Scan(&status, &priority, &attempts, &scheduledAt, &metadataNotBefore); err != nil {
		t.Fatalf("read requeued metadata job: %v", err)
	}
	if status != "queued" || priority != localMetadataRepairPriority || attempts != 0 || scheduledAt != metadataNotBefore {
		t.Fatalf("requeued metadata job = status %q priority %d attempts %d scheduled %q deadline %q", status, priority, attempts, scheduledAt, metadataNotBefore)
	}
	jobs, err := service.nextLocalMetadataJobs(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("nextLocalMetadataJobs() error = %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("nextLocalMetadataJobs() = %+v, want settling job to remain ineligible", jobs)
	}
	statuses, err := service.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses() error = %v", err)
	}
	if len(statuses) != 1 || statuses[0].QueuedMetadataJobs != 0 || statuses[0].SettlingMetadataJobs != 1 || statuses[0].FailedMetadataJobs != 0 {
		t.Fatalf("status after requeue = %+v, want one settling metadata job", statuses)
	}
}

func TestLocalPhase0ScanBlocksWhenRootMissingWithoutMarkingMissing(t *testing.T) {
	t.Parallel()

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: filepath.Join(t.TempDir(), "missing"),
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	result, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result.Status != localPhase0StatusBlocked || result.RootStatus != LocalMediaRootStatusMissing || result.LastError == "" {
		t.Fatalf("scan result = %+v, want blocked missing root", result)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_runs WHERE status = 'blocked'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_root_state WHERE root_status = 'missing' AND phase0_status = 'blocked'`, 1)
}

func TestLocalPhase0ScanBlocksWhenRootIsSymlinkWithoutMarkingMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link creation requires platform-specific privileges on Windows")
	}

	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	initial, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}
	if initial.Status != localPhase0StatusCompleted || initial.DiscoveredPaths != 1 {
		t.Fatalf("initial scan result = %+v, want one discovered path", initial)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'discovered'`, 1)

	realRootPath := filepath.Join(parentPath, "photos-real")
	if err := os.Rename(rootPath, realRootPath); err != nil {
		t.Fatalf("Rename(root) error = %v", err)
	}
	if err := os.Symlink(realRootPath, rootPath); err != nil {
		t.Fatalf("Symlink(root) error = %v", err)
	}

	blocked, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("symlink RunLocalReconciliationScan() error = %v", err)
	}
	if blocked.Status != localPhase0StatusBlocked || blocked.RootStatus != LocalMediaRootStatusUnreadable || blocked.LastError == "" || blocked.MissingPaths != 0 {
		t.Fatalf("symlink scan result = %+v, want blocked unreadable root without missing paths", blocked)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'discovered'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'missing'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_runs WHERE status = 'blocked'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_root_state WHERE root_status = 'unreadable' AND phase0_status = 'blocked'`, 1)
}

func TestLocalPhase0ScanRejectsRootReplacedBySymlinkBeforeWalk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link creation requires platform-specific privileges on Windows")
	}

	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	initial, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}
	if initial.Status != localPhase0StatusCompleted || initial.DiscoveredPaths != 1 {
		t.Fatalf("initial scan result = %+v, want one discovered path", initial)
	}

	originalWalkDir := localPhase0WalkDir
	t.Cleanup(func() { localPhase0WalkDir = originalWalkDir })
	realRootPath := filepath.Join(parentPath, "photos-real")
	localPhase0WalkDir = func(_ string, rootFS fs.FS, fn fs.WalkDirFunc) error {
		if err := os.Rename(rootPath, realRootPath); err != nil {
			return err
		}
		if err := os.Symlink(realRootPath, rootPath); err != nil {
			return err
		}
		return fs.WalkDir(rootFS, ".", fn)
	}

	failed, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("replacement RunLocalReconciliationScan() error = %v", err)
	}
	if failed.Status != localPhase0StatusFailed || failed.RootStatus != LocalMediaRootStatusUnreadable || failed.LastError == "" || failed.MissingPaths != 0 {
		t.Fatalf("replacement scan result = %+v, want failed unreadable root without missing paths", failed)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'discovered'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'missing'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_runs WHERE status = 'failed'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_root_state WHERE root_status = 'unreadable' AND phase0_status = 'failed'`, 1)
}

func TestLocalPhase0ScanPinsRootAcrossTemporaryPathReplacement(t *testing.T) {
	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	initial, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}
	if initial.Status != localPhase0StatusCompleted || initial.DiscoveredPaths != 1 {
		t.Fatalf("initial scan result = %+v, want one discovered path", initial)
	}

	originalWalkDir := localPhase0WalkDir
	t.Cleanup(func() { localPhase0WalkDir = originalWalkDir })
	pinnedRootPath := filepath.Join(parentPath, "photos-pinned")
	localPhase0WalkDir = func(_ string, rootFS fs.FS, fn fs.WalkDirFunc) error {
		swapped := false
		walkErr := fs.WalkDir(rootFS, ".", func(path string, entry fs.DirEntry, walkErr error) error {
			if path != "." || walkErr != nil {
				if swapped && walkErr == nil && path == "family.jpg" {
					entry = localPhase0InfoErrorDirEntry{DirEntry: entry, err: fs.ErrNotExist}
				}
				return fn(path, entry, walkErr)
			}
			if err := fn(path, entry, nil); err != nil {
				return err
			}
			if err := os.Rename(rootPath, pinnedRootPath); err != nil {
				return err
			}
			if err := os.Mkdir(rootPath, 0o755); err != nil {
				_ = os.Rename(pinnedRootPath, rootPath)
				return err
			}
			swapped = true
			return nil
		})
		if !swapped {
			return walkErr
		}
		removeErr := os.Remove(rootPath)
		restoreErr := os.Rename(pinnedRootPath, rootPath)
		return errors.Join(walkErr, removeErr, restoreErr)
	}

	completed, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("temporary replacement RunLocalReconciliationScan() error = %v", err)
	}
	if completed.Status != localPhase0StatusCompleted || completed.RootStatus != LocalMediaRootStatusReady || completed.DiscoveredPaths != 1 || completed.MissingPaths != 0 {
		t.Fatalf("temporary replacement scan result = %+v, want completed pinned-root scan", completed)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'discovered'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'missing'`, 0)
}

func TestLocalPhase0ScanRefusesReplacementRootWithoutMutation(t *testing.T) {
	tests := []struct {
		name             string
		placeholder      bool
		matchRootModTime bool
		run              func(*Service) (LocalPhase0ScanResult, error)
	}{
		{
			name: "full_empty",
			run: func(service *Service) (LocalPhase0ScanResult, error) {
				return service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
			},
		},
		{
			name:        "full_nonempty",
			placeholder: true,
			run: func(service *Service) (LocalPhase0ScanResult, error) {
				return service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
			},
		},
		{
			name:             "fast_empty",
			matchRootModTime: true,
			run: func(service *Service) (LocalPhase0ScanResult, error) {
				return service.RunLocalQuickDiscoveryScan(context.Background(), "1111111111111111")
			},
		},
		{
			name:             "fast_nonempty",
			placeholder:      true,
			matchRootModTime: true,
			run: func(service *Service) (LocalPhase0ScanResult, error) {
				return service.RunLocalQuickDiscoveryScan(context.Background(), "1111111111111111")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parentPath := t.TempDir()
			rootPath := filepath.Join(parentPath, "photos")
			albumPath := filepath.Join(rootPath, "album")
			if err := os.MkdirAll(albumPath, 0o755); err != nil {
				t.Fatalf("MkdirAll(album) error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(albumPath, "family.jpg"), []byte("image"), 0o644); err != nil {
				t.Fatalf("WriteFile(image) error = %v", err)
			}

			service := newLocalPhase0TestService(t, rootPath)
			initial, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
			if err != nil {
				t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
			}
			if initial.Status != localPhase0StatusCompleted || initial.DiscoveredPaths != 1 || initial.MissingPaths != 0 {
				t.Fatalf("initial scan result = %+v, want one discovered path", initial)
			}
			initialRootInfo, err := os.Stat(rootPath)
			if err != nil {
				t.Fatalf("Stat(initial root) error = %v", err)
			}
			var previousFullScanAt sql.NullString
			var previousRootIdentity string
			if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT last_reconciliation_at, root_identity
				FROM local_scan_root_state
				WHERE source_key = '1111111111111111' AND root_key = 'nas-photos'`).Scan(&previousFullScanAt, &previousRootIdentity); err != nil {
				t.Fatalf("read previous full completion: %v", err)
			}
			if strings.TrimSpace(previousRootIdentity) == "" {
				t.Fatal("initial scan did not persist root identity")
			}
			assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_directories`, 2)

			mountedPath := filepath.Join(parentPath, "photos-mounted")
			if err := os.Rename(rootPath, mountedPath); err != nil {
				t.Fatalf("Rename(root) error = %v", err)
			}
			if err := os.Mkdir(rootPath, 0o755); err != nil {
				t.Fatalf("Mkdir(empty mountpoint) error = %v", err)
			}
			if test.placeholder {
				if err := os.WriteFile(filepath.Join(rootPath, "placeholder.jpg"), []byte("placeholder"), 0o644); err != nil {
					t.Fatalf("WriteFile(placeholder) error = %v", err)
				}
			}
			if test.matchRootModTime {
				if err := os.Chtimes(rootPath, initialRootInfo.ModTime(), initialRootInfo.ModTime()); err != nil {
					t.Fatalf("Chtimes(replacement root) error = %v", err)
				}
			}

			failed, err := test.run(service)
			if err != nil {
				t.Fatalf("empty-mountpoint scan error = %v", err)
			}
			if failed.Status != localPhase0StatusFailed || failed.RootStatus != LocalMediaRootStatusIdentityChanged || failed.DiscoveredPaths != 0 || failed.MissingPaths != 0 || !strings.Contains(failed.LastError, ErrLocalMediaRootIdentityChanged.Error()) {
				t.Fatalf("replacement-root scan result = %+v, want identity-changed failure without traversal or missing paths", failed)
			}
			assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'discovered'`, 1)
			assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'missing'`, 0)
			assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE relative_path = 'placeholder.jpg'`, 0)
			assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_directories`, 2)
			assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_runs WHERE status = 'failed'`, 1)
			assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_root_state WHERE root_status = 'identity_changed' AND phase0_status = 'failed'`, 1)
			var retainedFullScanAt sql.NullString
			var retainedRootIdentity string
			if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT last_reconciliation_at, root_identity
				FROM local_scan_root_state
				WHERE source_key = '1111111111111111' AND root_key = 'nas-photos'`).Scan(&retainedFullScanAt, &retainedRootIdentity); err != nil {
				t.Fatalf("read retained full completion: %v", err)
			}
			if retainedFullScanAt != previousFullScanAt {
				t.Fatalf("last full completion = %#v, want retained %#v", retainedFullScanAt, previousFullScanAt)
			}
			if retainedRootIdentity != previousRootIdentity {
				t.Fatalf("root identity = %q, want retained %q", retainedRootIdentity, previousRootIdentity)
			}

			if test.placeholder {
				if err := os.Remove(filepath.Join(rootPath, "placeholder.jpg")); err != nil {
					t.Fatalf("Remove(placeholder) error = %v", err)
				}
			}
			if err := os.Remove(rootPath); err != nil {
				t.Fatalf("Remove(empty mountpoint) error = %v", err)
			}
			if err := os.Rename(mountedPath, rootPath); err != nil {
				t.Fatalf("restore root error = %v", err)
			}
			recovered, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
			if err != nil {
				t.Fatalf("recovered RunLocalReconciliationScan() error = %v", err)
			}
			if recovered.Status != localPhase0StatusCompleted || recovered.RootStatus != LocalMediaRootStatusReady || recovered.DiscoveredPaths != 1 || recovered.MissingPaths != 0 {
				t.Fatalf("recovered scan result = %+v, want completed ready root without missing paths", recovered)
			}
			assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'discovered'`, 1)
			assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'missing'`, 0)
		})
	}
}

func TestAcceptLocalMediaRootIdentityRequiresCurrentCandidateAndClearsCheckpoints(t *testing.T) {
	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	albumPath := filepath.Join(rootPath, "album")
	if err := os.MkdirAll(albumPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(album) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(albumPath, "family.jpg"), []byte("family"), 0o644); err != nil {
		t.Fatalf("WriteFile(family) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}
	verificationScheduledAt := time.Now().UTC()
	started, err := service.StartLocalContentVerificationWindow(
		context.Background(),
		"1111111111111111",
		verificationScheduledAt,
		verificationScheduledAt.Add(30*time.Minute),
	)
	if err != nil {
		t.Fatalf("StartLocalContentVerificationWindow() error = %v", err)
	}
	if !started {
		t.Fatal("StartLocalContentVerificationWindow() = false, want running window")
	}
	var trustedIdentity string
	if err := service.catalog.db.QueryRow(`SELECT root_identity FROM local_scan_root_state
		WHERE source_key = '1111111111111111' AND root_key = 'nas-photos'`).Scan(&trustedIdentity); err != nil {
		t.Fatalf("read trusted identity: %v", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_directories`, 2)

	mountedPath := filepath.Join(parentPath, "photos-mounted")
	if err := os.Rename(rootPath, mountedPath); err != nil {
		t.Fatalf("Rename(root) error = %v", err)
	}
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "placeholder.jpg"), []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("WriteFile(placeholder) error = %v", err)
	}
	continuity, err := service.LocalMediaRootContinuityStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalMediaRootContinuityStatuses() error = %v", err)
	}
	if len(continuity) != 1 || !continuity[0].AcceptanceRequired || continuity[0].RootStatus != LocalMediaRootStatusIdentityChanged || continuity[0].ObservedRootIdentity == "" || continuity[0].ObservedRootIdentity == trustedIdentity {
		t.Fatalf("continuity = %+v, want one changed candidate", continuity)
	}
	candidateIdentity := continuity[0].ObservedRootIdentity

	staleCandidatePath := filepath.Join(parentPath, "photos-stale-candidate")
	if err := os.Rename(rootPath, staleCandidatePath); err != nil {
		t.Fatalf("Rename(candidate root) error = %v", err)
	}
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(new candidate) error = %v", err)
	}
	if _, err := service.AcceptLocalMediaRootIdentity(context.Background(), "1111111111111111", "nas-photos", candidateIdentity); !errors.Is(err, ErrLocalMediaRootAcceptanceStale) {
		t.Fatalf("AcceptLocalMediaRootIdentity(stale candidate) error = %v, want ErrLocalMediaRootAcceptanceStale", err)
	}
	var retainedIdentity string
	if err := service.catalog.db.QueryRow(`SELECT root_identity FROM local_scan_root_state
		WHERE source_key = '1111111111111111' AND root_key = 'nas-photos'`).Scan(&retainedIdentity); err != nil {
		t.Fatalf("read retained identity: %v", err)
	}
	if retainedIdentity != trustedIdentity {
		t.Fatalf("identity after stale acceptance = %q, want %q", retainedIdentity, trustedIdentity)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_directories`, 2)
	if err := os.Remove(rootPath); err != nil {
		t.Fatalf("Remove(new candidate) error = %v", err)
	}
	if err := os.Rename(staleCandidatePath, rootPath); err != nil {
		t.Fatalf("Rename(stale candidate back) error = %v", err)
	}

	accepted, err := service.AcceptLocalMediaRootIdentity(context.Background(), "1111111111111111", "nas-photos", candidateIdentity)
	if err != nil {
		t.Fatalf("AcceptLocalMediaRootIdentity() error = %v", err)
	}
	if !accepted.Accepted || accepted.SourceKey != "1111111111111111" || accepted.RootKey != "nas-photos" {
		t.Fatalf("acceptance = %+v", accepted)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_directories`, 0)
	var acceptedIdentity string
	if err := service.catalog.db.QueryRow(`SELECT root_identity FROM local_scan_root_state
		WHERE source_key = '1111111111111111' AND root_key = 'nas-photos'`).Scan(&acceptedIdentity); err != nil {
		t.Fatalf("read accepted identity: %v", err)
	}
	if acceptedIdentity != candidateIdentity {
		t.Fatalf("accepted identity = %q, want %q", acceptedIdentity, candidateIdentity)
	}
	var verificationWindowStarted sql.NullString
	var verificationWindowDeadline sql.NullString
	var lastVerificationAt sql.NullString
	var verificationStatus string
	var verificationSkipReason sql.NullString
	var retainedVerificationSchedule string
	if err := service.catalog.db.QueryRow(`SELECT content_verification_scheduled_at,
			content_verification_window_started_at,
			content_verification_window_deadline_at,
			last_content_verification_at,
			content_verification_status,
			content_verification_skip_reason
		FROM local_scan_root_state
		WHERE source_key = '1111111111111111' AND root_key = 'nas-photos'`).Scan(
		&retainedVerificationSchedule,
		&verificationWindowStarted,
		&verificationWindowDeadline,
		&lastVerificationAt,
		&verificationStatus,
		&verificationSkipReason,
	); err != nil {
		t.Fatalf("read content verification after root acceptance: %v", err)
	}
	if retainedVerificationSchedule != formatCatalogTime(verificationScheduledAt) ||
		verificationWindowStarted.Valid ||
		verificationWindowDeadline.Valid ||
		!lastVerificationAt.Valid ||
		verificationStatus != LocalContentVerificationStatusSkipped ||
		!verificationSkipReason.Valid ||
		verificationSkipReason.String != LocalContentVerificationSkipRootChanged {
		t.Fatalf(
			"content verification after root acceptance = scheduled %q started %+v deadline %+v last %+v status %q reason %+v",
			retainedVerificationSchedule,
			verificationWindowStarted,
			verificationWindowDeadline,
			lastVerificationAt,
			verificationStatus,
			verificationSkipReason,
		)
	}
	if nextDeadline, err := service.LocalContentVerificationNextDeadline(context.Background()); err != nil {
		t.Fatalf("LocalContentVerificationNextDeadline() error = %v", err)
	} else if nextDeadline != nil {
		t.Fatalf("next content verification deadline = %v, want canceled root window removed", nextDeadline)
	}

	result, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan(accepted root) error = %v", err)
	}
	if result.Status != localPhase0StatusCompleted || result.DiscoveredPaths != 1 || result.MissingPaths != 1 {
		t.Fatalf("accepted-root scan = %+v, want one discovered and one missing", result)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE relative_path = 'placeholder.jpg' AND status = 'discovered'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE relative_path = 'album/family.jpg' AND status = 'missing'`, 1)
	if started, err := service.StartLocalContentVerificationWindow(
		context.Background(),
		"1111111111111111",
		verificationScheduledAt,
		verificationScheduledAt.Add(30*time.Minute),
	); err != nil || started {
		t.Fatalf("same-occurrence StartLocalContentVerificationWindow() = %t, %v, want false without error", started, err)
	}
	nextVerificationSchedule := verificationScheduledAt.Add(24 * time.Hour)
	if started, err := service.StartLocalContentVerificationWindow(
		context.Background(),
		"1111111111111111",
		nextVerificationSchedule,
		nextVerificationSchedule.Add(30*time.Minute),
	); err != nil || !started {
		t.Fatalf("next-occurrence StartLocalContentVerificationWindow() = %t, %v, want true without error", started, err)
	}
	if _, err := service.AcceptLocalMediaRootIdentity(context.Background(), "1111111111111111", "nas-photos", candidateIdentity); !errors.Is(err, ErrLocalMediaRootAcceptanceNotRequired) {
		t.Fatalf("AcceptLocalMediaRootIdentity(current) error = %v, want ErrLocalMediaRootAcceptanceNotRequired", err)
	}
}

func TestAcceptedRootReconciliationDefersOldJobsAndAdmitsCurrentGenerationMetadata(t *testing.T) {
	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	oldPath := filepath.Join(rootPath, "old.jpg")
	if err := os.WriteFile(oldPath, []byte("old-root"), 0o644); err != nil {
		t.Fatalf("WriteFile(old) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	service.mediaHelperPath = "/bin/false"
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}
	past := formatCatalogTime(time.Now().UTC().Add(-time.Minute))
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_asset_locations SET metadata_not_before = ?`, past); err != nil {
		t.Fatalf("make initial metadata eligible: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_scan_jobs SET scheduled_at = ? WHERE job_kind = ?`, past, localMetadataJobKind); err != nil {
		t.Fatalf("schedule initial metadata: %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch(initial) result=%+v error=%v, want one registered asset", result, err)
	}

	var oldLocationID int64
	if err := service.catalog.db.QueryRow(`SELECT id FROM local_asset_locations WHERE relative_path = 'old.jpg' AND status = 'active'`).Scan(&oldLocationID); err != nil {
		t.Fatalf("read old location: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, location_id, status, scheduled_at, sort_at
		)
		SELECT rs.source_key, ?, ?, rs.root_key, rs.root_generation, ?, 'queued', ?, ?
		FROM local_scan_root_state rs
		WHERE rs.source_key = ? AND rs.root_key = ?`,
		localMetadataJobKind,
		localMetadataBackgroundPriority,
		oldLocationID,
		past,
		past,
		"1111111111111111",
		"nas-photos",
	); err != nil {
		t.Fatalf("insert old-root metadata backlog: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, asset_id, status, scheduled_at, sort_at
		)
		SELECT rs.source_key, ?, ?, rs.root_key, rs.root_generation, a.asset_id, 'queued', ?, a.captured_at
		FROM local_scan_root_state rs
		JOIN local_assets a ON a.source_key = rs.source_key
		WHERE rs.source_key = ? AND rs.root_key = ?`,
		localThumbnailJobKind,
		localThumbnailBackgroundPriority,
		past,
		"1111111111111111",
		"nas-photos",
	); err != nil {
		t.Fatalf("insert old-root thumbnail backlog: %v", err)
	}

	trustedPath := filepath.Join(parentPath, "trusted")
	if err := os.Rename(rootPath, trustedPath); err != nil {
		t.Fatalf("Rename(root) error = %v", err)
	}
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement) error = %v", err)
	}
	currentPath := filepath.Join(rootPath, "current.jpg")
	if err := os.WriteFile(currentPath, []byte("current-root"), 0o644); err != nil {
		t.Fatalf("WriteFile(current) error = %v", err)
	}
	continuity, err := service.LocalMediaRootContinuityStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalMediaRootContinuityStatuses() error = %v", err)
	}
	if len(continuity) != 1 || !continuity[0].AcceptanceRequired || continuity[0].ObservedRootIdentity == "" {
		t.Fatalf("continuity = %+v, want replacement candidate", continuity)
	}
	if _, err := service.AcceptLocalMediaRootIdentity(context.Background(), "1111111111111111", "nas-photos", continuity[0].ObservedRootIdentity); err != nil {
		t.Fatalf("AcceptLocalMediaRootIdentity() error = %v", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_root_state WHERE root_generation = 2 AND reconciliation_pending = 1`, 1)
	if state, err := service.LocalMetadataQueueState(context.Background()); err != nil || state.Queued != 0 || state.Settling != 0 {
		t.Fatalf("LocalMetadataQueueState(old backlog) = %+v, %v, want deferred old generation", state, err)
	}
	if jobs, err := service.nextLocalMetadataJobs(context.Background(), "1111111111111111", 10); err != nil || len(jobs) != 0 {
		t.Fatalf("nextLocalMetadataJobs(old backlog) = %+v, %v, want no old generation jobs", jobs, err)
	}
	if pending, err := service.PendingLocalThumbnailJobs(context.Background()); err != nil || pending != 0 {
		t.Fatalf("PendingLocalThumbnailJobs(reconciling) = %d, %v, want thumbnails deferred", pending, err)
	}
	if jobs, err := service.nextLocalThumbnailJobs(context.Background(), "1111111111111111", 10); err != nil || len(jobs) != 0 {
		t.Fatalf("nextLocalThumbnailJobs(reconciling) = %+v, %v, want thumbnails deferred", jobs, err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE id = `+fmt.Sprint(oldLocationID)+` AND status = 'active'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE root_generation = 1 AND status = 'failed'`, 0)

	queued, err := service.QueueCommittedLocalUpload(context.Background(), "1111111111111111", currentPath)
	if err != nil || !queued {
		t.Fatalf("QueueCommittedLocalUpload(current) queued=%t error=%v, want current generation metadata", queued, err)
	}
	if jobs, err := service.nextLocalMetadataJobs(context.Background(), "1111111111111111", 10); err != nil || len(jobs) != 1 {
		t.Fatalf("nextLocalMetadataJobs(current) = %+v, %v, want one current generation job", jobs, err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 || result.FailedJobs != 0 {
		t.Fatalf("RunLocalMetadataBatch(current) result=%+v error=%v, want current metadata only", result, err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE root_generation = 1 AND job_kind = 'metadata' AND status = 'queued'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE root_generation = 1 AND status = 'failed'`, 0)

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.RunLocalReconciliationScan(canceledCtx, "1111111111111111"); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunLocalReconciliationScan(canceled) error = %v, want context.Canceled", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_root_state WHERE root_generation = 2 AND reconciliation_pending = 1`, 1)
	if due, err := service.LocalReconciliationDue(context.Background(), "1111111111111111", time.Now().UTC()); err != nil || !due {
		t.Fatalf("LocalReconciliationDue(pending) = %t, %v, want forced reconciliation scan", due, err)
	}

	result, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan(reconcile) error = %v", err)
	}
	if result.Status != localPhase0StatusCompleted || result.DiscoveredPaths != 1 || result.MissingPaths != 1 {
		t.Fatalf("reconciliation result = %+v, want one current and one missing path", result)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_root_state WHERE root_generation = 2 AND reconciliation_pending = 0`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE id = `+fmt.Sprint(oldLocationID)+` AND status = 'missing'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE root_generation = 1`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE status = 'failed'`, 0)
}

func TestBlockedAcceptedRootScanPreservesReconciliationGeneration(t *testing.T) {
	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "old.jpg"), []byte("old-root"), 0o644); err != nil {
		t.Fatalf("WriteFile(old) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}

	oldRootPath := filepath.Join(parentPath, "old-root")
	if err := os.Rename(rootPath, oldRootPath); err != nil {
		t.Fatalf("Rename(old root) error = %v", err)
	}
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement root) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "current.jpg"), []byte("current-root"), 0o644); err != nil {
		t.Fatalf("WriteFile(current) error = %v", err)
	}
	continuity, err := service.LocalMediaRootContinuityStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalMediaRootContinuityStatuses() error = %v", err)
	}
	if len(continuity) != 1 || !continuity[0].AcceptanceRequired {
		t.Fatalf("continuity = %+v, want replacement acceptance", continuity)
	}
	if _, err := service.AcceptLocalMediaRootIdentity(context.Background(), "1111111111111111", "nas-photos", continuity[0].ObservedRootIdentity); err != nil {
		t.Fatalf("AcceptLocalMediaRootIdentity() error = %v", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_root_state WHERE root_generation = 2 AND reconciliation_pending = 1`, 1)

	acceptedRootPath := filepath.Join(parentPath, "accepted-root")
	if err := os.Rename(rootPath, acceptedRootPath); err != nil {
		t.Fatalf("Rename(accepted root) error = %v", err)
	}
	blocked, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan(blocked) error = %v", err)
	}
	if blocked.Status != localPhase0StatusBlocked || blocked.RootStatus != LocalMediaRootStatusMissing {
		t.Fatalf("blocked scan = %+v, want blocked/missing", blocked)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_root_state WHERE root_generation = 2 AND reconciliation_pending = 1`, 1)
	if due, err := service.LocalReconciliationDue(context.Background(), "1111111111111111", time.Now().UTC()); err != nil || !due {
		t.Fatalf("LocalReconciliationDue(blocked reconciliation) = %t, %v, want forced reconciliation scan", due, err)
	}
}

func TestAcceptedRootScanQueuesCurrentMetadataBesideOldRunningJob(t *testing.T) {
	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	sharedPath := filepath.Join(rootPath, "shared.jpg")
	if err := os.WriteFile(sharedPath, []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile(old shared) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.catalog.db.Exec(`UPDATE local_scan_jobs
		SET status = 'running', locked_at = ?
		WHERE job_kind = ?`, formatCatalogTime(time.Now().UTC()), localMetadataJobKind); err != nil {
		t.Fatalf("mark old metadata running: %v", err)
	}

	oldRootPath := filepath.Join(parentPath, "old-root")
	if err := os.Rename(rootPath, oldRootPath); err != nil {
		t.Fatalf("Rename(old root) error = %v", err)
	}
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement root) error = %v", err)
	}
	if err := os.WriteFile(sharedPath, []byte("new-root-content"), 0o644); err != nil {
		t.Fatalf("WriteFile(new shared) error = %v", err)
	}
	continuity, err := service.LocalMediaRootContinuityStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalMediaRootContinuityStatuses() error = %v", err)
	}
	if len(continuity) != 1 || !continuity[0].AcceptanceRequired {
		t.Fatalf("continuity = %+v, want replacement acceptance", continuity)
	}
	if _, err := service.AcceptLocalMediaRootIdentity(context.Background(), "1111111111111111", "nas-photos", continuity[0].ObservedRootIdentity); err != nil {
		t.Fatalf("AcceptLocalMediaRootIdentity() error = %v", err)
	}
	result, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan(replacement) error = %v", err)
	}
	if result.Status != localPhase0StatusCompleted || result.QueuedMetadata != 1 {
		t.Fatalf("replacement scan = %+v, want one current-generation metadata job", result)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE root_generation = 1`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE root_generation = 2 AND job_kind = 'metadata' AND status = 'queued'`, 1)
}

func TestStaleThumbnailFailureCannotMutateAcceptedRootState(t *testing.T) {
	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	imagePath := filepath.Join(rootPath, "shared.jpg")
	if err := os.WriteFile(imagePath, []byte("old-image"), 0o644); err != nil {
		t.Fatalf("WriteFile(old image) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}
	past := formatCatalogTime(time.Now().UTC().Add(-time.Minute))
	if _, err := service.catalog.db.Exec(`UPDATE local_asset_locations SET metadata_not_before = ?`, past); err != nil {
		t.Fatalf("make metadata eligible: %v", err)
	}
	if _, err := service.catalog.db.Exec(`UPDATE local_scan_jobs SET scheduled_at = ? WHERE job_kind = ?`, past, localMetadataJobKind); err != nil {
		t.Fatalf("schedule metadata: %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v, want one registered asset", result, err)
	}
	var assetID string
	if err := service.catalog.db.QueryRow(`SELECT asset_id FROM local_assets WHERE source_key = ?`, "1111111111111111").Scan(&assetID); err != nil {
		t.Fatalf("read local asset ID: %v", err)
	}
	if _, err := service.catalog.db.Exec(`DELETE FROM local_scan_jobs WHERE job_kind = ?`, localThumbnailJobKind); err != nil {
		t.Fatalf("clear thumbnail jobs: %v", err)
	}
	result, err := service.catalog.db.Exec(`INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, asset_id,
			status, scheduled_at, sort_at, locked_at
		) VALUES (?, ?, ?, ?, 1, ?, 'running', ?, ?, ?)`,
		"1111111111111111",
		localThumbnailJobKind,
		localThumbnailBackgroundPriority,
		"nas-photos",
		assetID,
		past,
		past,
		past,
	)
	if err != nil {
		t.Fatalf("insert running old thumbnail job: %v", err)
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read old thumbnail job ID: %v", err)
	}

	oldRootPath := filepath.Join(parentPath, "old-root")
	if err := os.Rename(rootPath, oldRootPath); err != nil {
		t.Fatalf("Rename(old root) error = %v", err)
	}
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement root) error = %v", err)
	}
	if err := os.WriteFile(imagePath, []byte("new-image-content"), 0o644); err != nil {
		t.Fatalf("WriteFile(new image) error = %v", err)
	}
	continuity, err := service.LocalMediaRootContinuityStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalMediaRootContinuityStatuses() error = %v", err)
	}
	if len(continuity) != 1 || !continuity[0].AcceptanceRequired {
		t.Fatalf("continuity = %+v, want replacement acceptance", continuity)
	}
	if _, err := service.AcceptLocalMediaRootIdentity(context.Background(), "1111111111111111", "nas-photos", continuity[0].ObservedRootIdentity); err != nil {
		t.Fatalf("AcceptLocalMediaRootIdentity() error = %v", err)
	}
	if deferred, err := service.failLocalThumbnailJob(context.Background(), localThumbnailJob{
		ID:             jobID,
		SourceKey:      "1111111111111111",
		RootKey:        "nas-photos",
		RootGeneration: 1,
		AssetID:        assetID,
	}, errors.New("old-root render failed")); err != nil || !deferred {
		t.Fatalf("failLocalThumbnailJob(stale) deferred=%t error=%v", deferred, err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE id = `+fmt.Sprint(jobID)+` AND status = 'queued'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE asset_id = '`+assetID+`' AND thumbnail_status = 'failed'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions WHERE asset_id = '`+assetID+`' AND status = 'failed'`, 0)

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(reconcile) error = %v", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE id = `+fmt.Sprint(jobID), 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'failed'`, 0)
}

func TestLocalWorkersRejectFormerRootKeyAtSameGeneration(t *testing.T) {
	rootPath := t.TempDir()
	service := newLocalPhase0TestService(t, rootPath)
	service.mediaHelperPath = "/bin/false"
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	nowText := formatCatalogTime(time.Now().UTC())
	past := formatCatalogTime(time.Now().UTC().Add(-time.Minute))
	if _, err := service.catalog.db.Exec(`INSERT INTO local_scan_root_state (
			source_key, root_key, root_status, phase0_status, root_generation,
			reconciliation_pending, updated_at
		) VALUES (?, ?, 'ready', 'completed', 1, 0, ?)`,
		"1111111111111111",
		"former-root",
		nowText,
	); err != nil {
		t.Fatalf("insert former root state: %v", err)
	}
	locationResult, err := service.catalog.db.Exec(`INSERT INTO local_asset_locations (
			source_key, root_key, relative_path, size_bytes, mtime, fast_signature,
			file_identity, status, first_seen_at, last_seen_at, metadata_not_before, updated_at
		) VALUES (?, ?, ?, 4, ?, ?, 'former-file', 'discovered', ?, ?, ?, ?)`,
		"1111111111111111",
		"former-root",
		"former.jpg",
		past,
		"4:"+past,
		past,
		past,
		past,
		past,
	)
	if err != nil {
		t.Fatalf("insert former root location: %v", err)
	}
	locationID, err := locationResult.LastInsertId()
	if err != nil {
		t.Fatalf("read former root location ID: %v", err)
	}
	const assetID = "former-asset"
	if _, err := service.catalog.db.Exec(`INSERT INTO local_assets (
			source_key, asset_id, sha1_hex, content_size_bytes, media_type, filename,
			captured_at, captured_at_source, visibility_status, thumbnail_status,
			first_seen_at, updated_at
		) VALUES (?, ?, ?, 4, 'image', 'former.jpg', ?, 'filesystem', 'active', 'pending', ?, ?)`,
		"1111111111111111",
		assetID,
		strings.Repeat("a", 40),
		past,
		past,
		past,
	); err != nil {
		t.Fatalf("insert former root asset: %v", err)
	}
	metadataResult, err := service.catalog.db.Exec(`INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, location_id,
			status, scheduled_at, sort_at
		) VALUES (?, ?, ?, ?, 1, ?, 'queued', ?, ?)`,
		"1111111111111111",
		localMetadataJobKind,
		localMetadataBackgroundPriority,
		"former-root",
		locationID,
		past,
		past,
	)
	if err != nil {
		t.Fatalf("insert former root metadata job: %v", err)
	}
	metadataJobID, err := metadataResult.LastInsertId()
	if err != nil {
		t.Fatalf("read former metadata job ID: %v", err)
	}
	thumbnailResult, err := service.catalog.db.Exec(`INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, asset_id,
			status, scheduled_at, sort_at
		) VALUES (?, ?, ?, ?, 1, ?, 'queued', ?, ?)`,
		"1111111111111111",
		localThumbnailJobKind,
		localThumbnailBackgroundPriority,
		"former-root",
		assetID,
		past,
		past,
	)
	if err != nil {
		t.Fatalf("insert former root thumbnail job: %v", err)
	}
	thumbnailJobID, err := thumbnailResult.LastInsertId()
	if err != nil {
		t.Fatalf("read former thumbnail job ID: %v", err)
	}
	metadataJob := localMetadataJob{
		ID:             metadataJobID,
		SourceKey:      "1111111111111111",
		RootKey:        "former-root",
		RootGeneration: 1,
		LocationID:     locationID,
	}
	thumbnailJob := localThumbnailJob{
		ID:             thumbnailJobID,
		SourceKey:      "1111111111111111",
		RootKey:        "former-root",
		RootGeneration: 1,
		AssetID:        assetID,
	}

	if state, err := service.LocalMetadataQueueState(context.Background()); err != nil || state.Queued != 0 || state.Settling != 0 {
		t.Fatalf("LocalMetadataQueueState() = %+v, %v, want former root excluded", state, err)
	}
	if jobs, err := service.nextLocalMetadataJobs(context.Background(), "", 10); err != nil || len(jobs) != 0 {
		t.Fatalf("nextLocalMetadataJobs() = %+v, %v, want former root excluded", jobs, err)
	}
	if jobs, err := service.nextLocalThumbnailJobs(context.Background(), "", 10); err != nil || len(jobs) != 0 {
		t.Fatalf("nextLocalThumbnailJobs() = %+v, %v, want former root excluded", jobs, err)
	}
	trustedRoot, err := service.acquireTrustedLocalMediaRoot(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("acquireTrustedLocalMediaRoot() error = %v", err)
	}
	if claimed, err := service.claimLocalMetadataJob(context.Background(), metadataJob, trustedRoot, nowText); err != nil || claimed {
		_ = trustedRoot.Close()
		t.Fatalf("claimLocalMetadataJob(former root) = %t, %v, want rejected", claimed, err)
	}
	if claimed, err := service.claimLocalThumbnailJob(context.Background(), thumbnailJob, trustedRoot, nowText); err != nil || claimed {
		_ = trustedRoot.Close()
		t.Fatalf("claimLocalThumbnailJob(former root) = %t, %v, want rejected", claimed, err)
	}
	_ = trustedRoot.Close()

	if _, err := service.catalog.db.Exec(`UPDATE local_scan_jobs SET status = 'running'
		WHERE id IN (?, ?)`, metadataJobID, thumbnailJobID); err != nil {
		t.Fatalf("mark former root jobs running: %v", err)
	}
	if deferred, err := service.failLocalMetadataJob(context.Background(), metadataJob, errors.New("former metadata failed")); err != nil || !deferred {
		t.Fatalf("failLocalMetadataJob(former root) deferred=%t error=%v", deferred, err)
	}
	if deferred, err := service.failLocalThumbnailJob(context.Background(), thumbnailJob, errors.New("former thumbnail failed")); err != nil || !deferred {
		t.Fatalf("failLocalThumbnailJob(former root) deferred=%t error=%v", deferred, err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
			WHERE id IN (`+fmt.Sprint(metadataJobID)+`, `+fmt.Sprint(thumbnailJobID)+`)
				AND status = 'queued'`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets
		WHERE asset_id = 'former-asset' AND thumbnail_status = 'failed'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions
		WHERE asset_id = 'former-asset' AND status = 'failed'`, 0)
}

func TestLocalThumbnailAdmissionAndRepairIgnoreFormerRootJobs(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeJPEGForTest(t, 800, 600), 0o644); err != nil {
		t.Fatalf("WriteFile(local jpeg) error = %v", err)
	}
	helperPath, _ := writeFakeMediaHelperImageScript(t)
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaHelperPath: helperPath,
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v", result, err)
	}
	var assetID string
	if err := service.catalog.db.QueryRow(`SELECT asset_id FROM local_assets WHERE source_key = ?`, "1111111111111111").Scan(&assetID); err != nil {
		t.Fatalf("read local asset ID: %v", err)
	}
	nowText := formatCatalogTime(time.Now().UTC())
	if _, err := service.catalog.db.Exec(`INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, asset_id,
			status, scheduled_at, sort_at
		) VALUES (?, ?, ?, 'former-root', 1, ?, 'queued', ?, ?)`,
		"1111111111111111",
		localThumbnailJobKind,
		localThumbnailBackgroundPriority,
		assetID,
		nowText,
		nowText,
	); err != nil {
		t.Fatalf("insert former-root thumbnail job: %v", err)
	}
	if err := service.queuePendingLocalThumbnailJobsForSource(context.Background(), "1111111111111111", 10, 10); err != nil {
		t.Fatalf("queuePendingLocalThumbnailJobsForSource() error = %v", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE job_kind = 'thumbnail' AND asset_id = '`+assetID+`'
			AND root_key = 'former-root' AND status = 'queued'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE job_kind = 'thumbnail' AND asset_id = '`+assetID+`'
			AND root_key = 'nas-photos' AND root_generation = 1 AND status = 'queued'`, 1)

	if _, err := service.catalog.db.Exec(`DELETE FROM local_scan_jobs
		WHERE job_kind = ? AND asset_id = ? AND root_key = ?`,
		localThumbnailJobKind,
		assetID,
		"nas-photos",
	); err != nil {
		t.Fatalf("delete current-root thumbnail job: %v", err)
	}
	if _, err := service.catalog.db.Exec(`UPDATE local_scan_jobs
		SET status = 'running', locked_at = ?
		WHERE job_kind = ? AND asset_id = ? AND root_key = 'former-root'`,
		nowText,
		localThumbnailJobKind,
		assetID,
	); err != nil {
		t.Fatalf("mark former-root thumbnail job running: %v", err)
	}
	if _, err := service.catalog.db.Exec(`UPDATE local_assets SET thumbnail_status = 'failed' WHERE asset_id = ?`, assetID); err != nil {
		t.Fatalf("mark local thumbnail failed: %v", err)
	}
	requeued, err := service.requeueFailedLocalThumbnailsForSource(context.Background(), "1111111111111111")
	if err != nil || requeued != 1 {
		t.Fatalf("requeueFailedLocalThumbnailsForSource() = %d, %v, want one current-root job", requeued, err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE job_kind = 'thumbnail' AND asset_id = '`+assetID+`'
			AND root_key = 'former-root' AND status = 'running'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE job_kind = 'thumbnail' AND asset_id = '`+assetID+`'
			AND root_key = 'nas-photos' AND root_generation = 1 AND status = 'queued'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets
		WHERE asset_id = '`+assetID+`' AND thumbnail_status = 'pending'`, 1)

	if _, err := service.catalog.db.Exec(`UPDATE local_scan_root_state
		SET reconciliation_pending = 1
		WHERE source_key = ? AND root_key = ?`,
		"1111111111111111",
		"nas-photos",
	); err != nil {
		t.Fatalf("mark root reconciliation pending: %v", err)
	}
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(reconcile) error = %v", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE root_key = 'former-root'`, 0)
}

func TestAdminLocalWorkReadModelsExcludeStaleRootGeneration(t *testing.T) {
	rootPath := t.TempDir()
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}
	past := formatCatalogTime(time.Now().UTC().Add(-time.Minute))
	if _, err := service.catalog.db.Exec(`INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, status, scheduled_at, sort_at
		) VALUES
			('1111111111111111', 'metadata', 1, 'nas-photos', 1, 'queued', ?, ?),
			('1111111111111111', 'metadata', 1, 'nas-photos', 1, 'running', ?, ?),
			('1111111111111111', 'metadata', 1, 'nas-photos', 1, 'failed', ?, ?),
			('1111111111111111', 'thumbnail', 1, 'nas-photos', 1, 'queued', ?, ?),
			('1111111111111111', 'thumbnail', 1, 'nas-photos', 1, 'running', ?, ?),
			('1111111111111111', 'metadata', 1, 'nas-photos', 2, 'queued', ?, ?),
			('1111111111111111', 'metadata', 1, 'nas-photos', 2, 'failed', ?, ?)`,
		past, past,
		past, past,
		past, past,
		past, past,
		past, past,
		past, past,
		past, past,
	); err != nil {
		t.Fatalf("insert mixed-generation jobs: %v", err)
	}
	if _, err := service.catalog.db.Exec(`UPDATE local_scan_root_state
		SET root_generation = 2, reconciliation_pending = 1
		WHERE source_key = '1111111111111111' AND root_key = 'nas-photos'`); err != nil {
		t.Fatalf("advance root generation: %v", err)
	}

	statuses, err := service.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses() error = %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("LocalDatasourceScanStatuses() = %+v, want one datasource", statuses)
	}
	status := statuses[0]
	if status.QueuedMetadataJobs != 1 || status.RunningMetadataJobs != 0 || status.FailedMetadataJobs != 1 ||
		status.QueuedThumbnailJobs != 0 || status.RunningThumbnailJobs != 0 {
		t.Fatalf("current-generation status = %+v, want metadata queued/failed only", status)
	}

	stats, err := service.RefreshAssetProcessingStats(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats() error = %v", err)
	}
	assertAssetProcessingStat(t, stats, AssetProcessingStageMetadata, AssetProcessingStatusPending, 1, 2)
	assertAssetProcessingStat(t, stats, AssetProcessingStageMetadata, AssetProcessingStatusRunning, 0, 2)
	assertAssetProcessingStat(t, stats, AssetProcessingStageMetadata, AssetProcessingStatusFailed, 1, 2)
	assertAssetProcessingStat(t, stats, AssetProcessingStageThumbnails, AssetProcessingStatusRunning, 0, 0)

	diagnostics, err := service.LocalFailureDiagnosticRows(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("LocalFailureDiagnosticRows() error = %v", err)
	}
	if len(diagnostics) != 1 || diagnostics[0].Status != "failed" || diagnostics[0].FailureKind != localMetadataJobKind {
		t.Fatalf("current-generation diagnostics = %+v, want one metadata failure", diagnostics)
	}
	issues, err := service.countDatasourceIssues(context.Background(), service.catalog.db, processingStatsDatasource{
		SourceKey: "1111111111111111",
		Kind:      config.DatasourceKindLocalFiles,
	}, nil)
	if err != nil {
		t.Fatalf("countDatasourceIssues() error = %v", err)
	}
	if issues != 1 {
		t.Fatalf("countDatasourceIssues() = %d, want one current-generation failed job", issues)
	}
}

func TestLocalRootAcceptanceDoesNotBlockAnotherRoot(t *testing.T) {
	service, rootAPath, rootBPath := newTwoLocalRootTransitionTestService(t)
	const (
		sourceA = "1111111111111111"
		sourceB = "2222222222222222"
		rootA   = "nas-a"
		rootB   = "nas-b"
	)

	heldRootB, err := service.acquireTrustedLocalMediaRoot(context.Background(), sourceB)
	if err != nil {
		t.Fatalf("acquireTrustedLocalMediaRoot(B) error = %v", err)
	}
	t.Cleanup(func() { _ = heldRootB.Close() })

	trustedRootBPath := rootBPath + "-trusted"
	if err := os.Rename(rootBPath, trustedRootBPath); err != nil {
		t.Fatalf("Rename(root B) error = %v", err)
	}
	if err := os.Mkdir(rootBPath, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement root B) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootBPath, "replacement.jpg"), []byte("replacement"), 0o644); err != nil {
		t.Fatalf("WriteFile(replacement B) error = %v", err)
	}
	inspection, pinnedRoot, err := openLocalMediaRoot(rootBPath)
	if err != nil {
		t.Fatalf("openLocalMediaRoot(replacement B) error = %v", err)
	}
	_ = pinnedRoot.Close()

	type acceptanceOutcome struct {
		result LocalMediaRootAcceptanceResult
		err    error
	}
	acceptance := make(chan acceptanceOutcome, 1)
	acceptCtx, cancelAccept := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelAccept()
	go func() {
		result, acceptErr := service.AcceptLocalMediaRootIdentity(acceptCtx, sourceB, rootB, inspection.identity)
		acceptance <- acceptanceOutcome{result: result, err: acceptErr}
	}()

	gateB := service.localRootTransitionGate(sourceB, rootB)
	waitDeadline := time.Now().Add(time.Second)
	for gateB.semaphore.TryAcquire(1) {
		gateB.semaphore.Release(1)
		if time.Now().After(waitDeadline) {
			t.Fatal("root B acceptance did not begin waiting for its in-flight reader")
		}
		time.Sleep(time.Millisecond)
	}

	rootACtx, cancelRootA := context.WithTimeout(context.Background(), 100*time.Millisecond)
	trustedRootA, err := service.acquireTrustedLocalMediaRoot(rootACtx, sourceA)
	cancelRootA()
	if err != nil {
		t.Fatalf("acquireTrustedLocalMediaRoot(A) while B acceptance waits error = %v", err)
	}
	_ = trustedRootA.Close()

	keysStartedAt := time.Now()
	trustedRoots, err := service.trustedLocalMediaRootReferences(context.Background(), "")
	if err != nil {
		t.Fatalf("trustedLocalMediaRootReferences() error = %v", err)
	}
	if time.Since(keysStartedAt) > 100*time.Millisecond {
		t.Fatalf("trusted source enumeration took %s while root B transitioned", time.Since(keysStartedAt))
	}
	if len(trustedRoots) != 1 || trustedRoots[0].sourceKey != sourceA || trustedRoots[0].rootKey != rootA {
		t.Fatalf("trusted roots = %+v, want only healthy root A", trustedRoots)
	}
	metadataState, err := service.LocalMetadataQueueState(context.Background())
	if err != nil {
		t.Fatalf("LocalMetadataQueueState() error = %v", err)
	}
	if metadataState.Queued+metadataState.Settling != 1 {
		t.Fatalf("LocalMetadataQueueState() = %+v, want only root A's job", metadataState)
	}

	scanA, err := service.RunLocalReconciliationScan(context.Background(), sourceA)
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan(A) while B acceptance waits error = %v", err)
	}
	if scanA.Status != localPhase0StatusCompleted || scanA.RootStatus != LocalMediaRootStatusReady {
		t.Fatalf("root A scan = %+v, want completed/ready", scanA)
	}
	if _, err := os.Stat(filepath.Join(rootAPath, "a.jpg")); err != nil {
		t.Fatalf("Stat(root A media) error = %v", err)
	}

	if err := heldRootB.Close(); err != nil {
		t.Fatalf("Close(held root B) error = %v", err)
	}
	select {
	case outcome := <-acceptance:
		if outcome.err != nil {
			t.Fatalf("AcceptLocalMediaRootIdentity(B) error = %v", outcome.err)
		}
		if !outcome.result.Accepted || outcome.result.SourceKey != sourceB || outcome.result.RootKey != rootB {
			t.Fatalf("acceptance outcome = %+v", outcome.result)
		}
	case <-time.After(time.Second):
		t.Fatal("root B acceptance did not finish after its reader released")
	}
}

func TestLocalRootAcceptanceWaitHonorsContext(t *testing.T) {
	service, _, rootBPath := newTwoLocalRootTransitionTestService(t)
	const (
		sourceB = "2222222222222222"
		rootB   = "nas-b"
	)
	heldRootB, err := service.acquireTrustedLocalMediaRoot(context.Background(), sourceB)
	if err != nil {
		t.Fatalf("acquireTrustedLocalMediaRoot(B) error = %v", err)
	}
	defer heldRootB.Close()

	trustedRootBPath := rootBPath + "-trusted"
	if err := os.Rename(rootBPath, trustedRootBPath); err != nil {
		t.Fatalf("Rename(root B) error = %v", err)
	}
	if err := os.Mkdir(rootBPath, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement root B) error = %v", err)
	}
	inspection, pinnedRoot, err := openLocalMediaRoot(rootBPath)
	if err != nil {
		t.Fatalf("openLocalMediaRoot(replacement B) error = %v", err)
	}
	_ = pinnedRoot.Close()

	acceptCtx, cancelAccept := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelAccept()
	if result, err := service.AcceptLocalMediaRootIdentity(acceptCtx, sourceB, rootB, inspection.identity); !errors.Is(err, context.DeadlineExceeded) || result.Accepted {
		t.Fatalf("AcceptLocalMediaRootIdentity(B) result=%+v error=%v, want context deadline without commit", result, err)
	}
	var retainedIdentity string
	if err := service.catalog.db.QueryRow(`SELECT root_identity FROM local_scan_root_state
		WHERE source_key = ? AND root_key = ?`, sourceB, rootB).Scan(&retainedIdentity); err != nil {
		t.Fatalf("read retained root B identity: %v", err)
	}
	if retainedIdentity == "" || retainedIdentity == inspection.identity {
		t.Fatalf("retained identity = %q, want previous trusted identity", retainedIdentity)
	}
}

func newTwoLocalRootTransitionTestService(t *testing.T) (*Service, string, string) {
	t.Helper()
	parentPath := t.TempDir()
	rootAPath := filepath.Join(parentPath, "root-a")
	rootBPath := filepath.Join(parentPath, "root-b")
	for path, contents := range map[string]string{
		filepath.Join(rootAPath, "a.jpg"): "root-a",
		filepath.Join(rootBPath, "b.jpg"): "root-b",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	service, err := NewServiceWithOptions([]config.DatasourceConfig{
		{SourceKey: "1111111111111111", Name: "Root A", Kind: config.DatasourceKindLocalFiles, RootKey: "nas-a"},
		{SourceKey: "2222222222222222", Name: "Root B", Kind: config.DatasourceKindLocalFiles, RootKey: "nas-b"},
	}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{
			{Key: "nas-a", Path: rootAPath},
			{Key: "nas-b", Path: rootBPath},
		},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	for _, sourceKey := range []string{"1111111111111111", "2222222222222222"} {
		result, scanErr := service.RunLocalReconciliationScan(context.Background(), sourceKey)
		if scanErr != nil {
			t.Fatalf("RunLocalReconciliationScan(%s) error = %v", sourceKey, scanErr)
		}
		if result.Status != localPhase0StatusCompleted || result.DiscoveredPaths != 1 {
			t.Fatalf("RunLocalReconciliationScan(%s) result = %+v", sourceKey, result)
		}
	}
	return service, rootAPath, rootBPath
}

func TestUnacceptedReplacementRootDefersWorkersOriginalAndCommittedUpload(t *testing.T) {
	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	mediaPath := filepath.Join(rootPath, "family.jpg")
	if err := os.WriteFile(mediaPath, encodeJPEGForTest(t, 320, 240), 0o644); err != nil {
		t.Fatalf("WriteFile(family) error = %v", err)
	}
	helperPath, _ := writeFakeMediaHelperImageScript(t)
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: "1ns",
		},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaHelperPath: helperPath,
		LocalRoots:      []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v, want one active asset", result, err)
	}
	if err := service.queuePendingLocalThumbnailJobsForSource(context.Background(), "1111111111111111", 10, 10); err != nil {
		t.Fatalf("queuePendingLocalThumbnailJobsForSource() error = %v", err)
	}

	var locationID int64
	var assetID string
	if err := service.catalog.db.QueryRow(`SELECT id, asset_id FROM local_asset_locations
		WHERE source_key = '1111111111111111' AND relative_path = 'family.jpg'`).Scan(&locationID, &assetID); err != nil {
		t.Fatalf("read active location: %v", err)
	}
	nowText := formatCatalogTime(time.Now().UTC().Add(-time.Minute))
	if _, err := service.catalog.db.Exec(`INSERT INTO local_scan_jobs (
		source_key, job_kind, priority, root_key, location_id, status, scheduled_at, sort_at
	) VALUES ('1111111111111111', 'metadata', 5, 'nas-photos', ?, 'queued', ?, ?)`, locationID, nowText, nowText); err != nil {
		t.Fatalf("insert waiting metadata job: %v", err)
	}
	if _, err := service.catalog.db.Exec(`UPDATE local_asset_locations SET metadata_not_before = ? WHERE id = ?`, nowText, locationID); err != nil {
		t.Fatalf("make metadata job eligible: %v", err)
	}
	var metadataJob localMetadataJob
	if err := service.catalog.db.QueryRow(`SELECT id, source_key, location_id FROM local_scan_jobs
		WHERE job_kind = 'metadata' AND location_id = ?`, locationID).Scan(&metadataJob.ID, &metadataJob.SourceKey, &metadataJob.LocationID); err != nil {
		t.Fatalf("read metadata job: %v", err)
	}
	var thumbnailJob localThumbnailJob
	if err := service.catalog.db.QueryRow(`SELECT id, source_key, asset_id FROM local_scan_jobs
		WHERE job_kind = 'thumbnail' AND asset_id = ?`, assetID).Scan(&thumbnailJob.ID, &thumbnailJob.SourceKey, &thumbnailJob.AssetID); err != nil {
		t.Fatalf("read thumbnail job: %v", err)
	}
	thumbnailJob.MediaType = "image"

	trustedPath := filepath.Join(parentPath, "photos-trusted")
	if err := os.Rename(rootPath, trustedPath); err != nil {
		t.Fatalf("Rename(trusted root) error = %v", err)
	}
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement root) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeJPEGForTest(t, 640, 480), 0o644); err != nil {
		t.Fatalf("WriteFile(replacement family) error = %v", err)
	}
	uploadPath := filepath.Join(rootPath, "uploaded.jpg")
	if err := os.WriteFile(uploadPath, encodeJPEGForTest(t, 160, 120), 0o644); err != nil {
		t.Fatalf("WriteFile(replacement upload) error = %v", err)
	}

	if state, err := service.LocalMetadataQueueState(context.Background()); err != nil || state.Queued != 0 || state.Settling != 0 {
		t.Fatalf("LocalMetadataQueueState(untrusted) = %+v, %v, want no schedulable work", state, err)
	}
	if pending, err := service.PendingLocalThumbnailJobs(context.Background()); err != nil || pending != 0 {
		t.Fatalf("PendingLocalThumbnailJobs(untrusted) = %d, %v, want no schedulable work", pending, err)
	}
	if processed, err := service.processLocalMetadataJob(context.Background(), metadataJob); processed || !errors.Is(err, ErrLocalMediaRootNotTrusted) {
		t.Fatalf("processLocalMetadataJob(untrusted) processed=%t error=%v", processed, err)
	}
	if generated, err := service.processLocalThumbnailJob(context.Background(), thumbnailJob); generated || !errors.Is(err, ErrLocalMediaRootNotTrusted) {
		t.Fatalf("processLocalThumbnailJob(untrusted) generated=%t error=%v", generated, err)
	}
	datasource := service.datasourceStateSnapshot().datasources["1111111111111111"]
	request := httptest.NewRequest(http.MethodGet, "http://agent.local/original", nil)
	if response, err := service.localOriginalMediaResponse(request, &datasource, assetID); response != nil || !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("localOriginalMediaResponse(untrusted) response=%v error=%v, want unavailable", response, err)
	}
	if queued, err := service.QueueCommittedLocalUpload(context.Background(), "1111111111111111", uploadPath); queued || !errors.Is(err, ErrLocalMediaRootNotTrusted) {
		t.Fatalf("QueueCommittedLocalUpload(untrusted) queued=%t error=%v", queued, err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE id = `+fmt.Sprint(locationID)+` AND status = 'active' AND asset_id IS NOT NULL`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE relative_path = 'uploaded.jpg'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE id IN (`+fmt.Sprint(metadataJob.ID)+`, `+fmt.Sprint(thumbnailJob.ID)+`) AND status = 'queued' AND attempts = 0`, 2)

	replacementPath := filepath.Join(parentPath, "photos-replacement")
	if err := os.Rename(rootPath, replacementPath); err != nil {
		t.Fatalf("Rename(replacement root) error = %v", err)
	}
	if err := os.Rename(trustedPath, rootPath); err != nil {
		t.Fatalf("Rename(trusted root back) error = %v", err)
	}
	if state, err := service.LocalMetadataQueueState(context.Background()); err != nil || state.Queued != 1 {
		t.Fatalf("LocalMetadataQueueState(restored) = %+v, %v, want queued work restored", state, err)
	}
	if pending, err := service.PendingLocalThumbnailJobs(context.Background()); err != nil || pending != 1 {
		t.Fatalf("PendingLocalThumbnailJobs(restored) = %d, %v, want pending work restored", pending, err)
	}
}

func TestLocalPhase0ScanRejectsRootIdentityChangeBeforeMissingUpdate(t *testing.T) {
	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	mediaPath := filepath.Join(rootPath, "family.jpg")
	if err := os.WriteFile(mediaPath, []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	initial, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}
	if initial.Status != localPhase0StatusCompleted || initial.DiscoveredPaths != 1 {
		t.Fatalf("initial scan result = %+v, want one discovered path", initial)
	}
	if err := os.Remove(mediaPath); err != nil {
		t.Fatalf("Remove(image) error = %v", err)
	}

	originalWalkDir := localPhase0WalkDir
	t.Cleanup(func() { localPhase0WalkDir = originalWalkDir })
	originalRootPath := filepath.Join(parentPath, "photos-original")
	localPhase0WalkDir = func(_ string, rootFS fs.FS, fn fs.WalkDirFunc) error {
		if err := fs.WalkDir(rootFS, ".", fn); err != nil {
			return err
		}
		if err := os.Rename(rootPath, originalRootPath); err != nil {
			return err
		}
		return os.Mkdir(rootPath, 0o755)
	}

	failed, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("replacement RunLocalReconciliationScan() error = %v", err)
	}
	if failed.Status != localPhase0StatusFailed || failed.RootStatus != LocalMediaRootStatusUnreadable || failed.LastError == "" || failed.MissingPaths != 0 {
		t.Fatalf("replacement scan result = %+v, want failed unreadable root without missing paths", failed)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'discovered'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'missing'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_runs WHERE status = 'failed'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_root_state WHERE root_status = 'unreadable' AND phase0_status = 'failed'`, 1)
}

func TestLocalPhase0ScanSkipsNASSystemDirectories(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	for _, dir := range []string{"@eaDir", "@Recycle"} {
		if err := os.MkdirAll(filepath.Join(rootPath, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(rootPath, dir, "system.jpg"), []byte("system"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(rootPath, "visible.jpg"), []byte("visible"), 0o644); err != nil {
		t.Fatalf("WriteFile(visible) error = %v", err)
	}

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	result, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result.DiscoveredPaths != 1 || result.ChangedPaths != 1 || result.QueuedMetadata != 1 {
		t.Fatalf("scan result = %+v, want only visible media discovered", result)
	}
	if result.SkipCounts["system_directory"] != 2 {
		t.Fatalf("skip counts = %#v, want two system directories skipped", result.SkipCounts)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations`, 1)
}

func TestLocalThumbnailBatchGeneratesAndServesPreview(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeJPEGForTest(t, 320, 240), 0o644); err != nil {
		t.Fatalf("WriteFile(local jpeg) error = %v", err)
	}
	helperPath, _ := writeFakeMediaHelperImageScript(t)

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: "1ns",
		},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaHelperPath: helperPath,
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	metadata, err := service.RunLocalMetadataBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	if metadata.RegisteredAssets != 1 {
		t.Fatalf("metadata result = %+v, want one registered asset", metadata)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'thumbnail' AND status = 'queued'`, 0)

	thumbnails, err := service.RunLocalThumbnailBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalThumbnailBatch() error = %v", err)
	}
	if thumbnails.ProcessedJobs != 1 || thumbnails.CompletedJobs != 1 || thumbnails.FailedJobs != 0 || thumbnails.GeneratedAssets != 1 || thumbnails.GeneratedImageAssets != 1 {
		t.Fatalf("thumbnail result = %+v, want one generated asset", thumbnails)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions WHERE status = 'ready'`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'ready'`, 1)

	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("timeline page total=%d items=%d, want one local asset", page.Total, len(page.Items))
	}

	request, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/"+page.Items[0].ID+"/preview", nil)
	if err != nil {
		t.Fatalf("NewRequest(preview) error = %v", err)
	}
	preview, err := service.PreviewFromSource(request, page.Items[0].SourceKey, page.Items[0].ID)
	if err != nil {
		t.Fatalf("PreviewFromSource() error = %v", err)
	}
	defer preview.Body.Close()
	previewBody, err := io.ReadAll(preview.Body)
	if err != nil {
		t.Fatalf("ReadAll(preview) error = %v", err)
	}
	if preview.StatusCode != http.StatusOK || preview.Header.Get("Content-Type") != "image/jpeg" || len(previewBody) == 0 {
		t.Fatalf("preview status=%d contentType=%q bytes=%d, want jpeg body", preview.StatusCode, preview.Header.Get("Content-Type"), len(previewBody))
	}
	previewImage, _, err := image.Decode(bytes.NewReader(previewBody))
	if err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if max(previewImage.Bounds().Dx(), previewImage.Bounds().Dy()) > previewMaxEdgePixels {
		t.Fatalf("preview bounds = %v, want max edge <= %d", previewImage.Bounds(), previewMaxEdgePixels)
	}

	detail, err := service.DetailPreviewFromSource(request, page.Items[0].SourceKey, page.Items[0].ID)
	if err != nil {
		t.Fatalf("DetailPreviewFromSource() error = %v", err)
	}
	defer detail.Body.Close()
	detailBody, err := io.ReadAll(detail.Body)
	if err != nil {
		t.Fatalf("ReadAll(detail preview) error = %v", err)
	}
	detailImage, _, err := image.Decode(bytes.NewReader(detailBody))
	if err != nil {
		t.Fatalf("decode detail preview: %v", err)
	}
	if max(detailImage.Bounds().Dx(), detailImage.Bounds().Dy()) > detailPreviewMaxEdgePixels {
		t.Fatalf("detail preview bounds = %v, want max edge <= %d", detailImage.Bounds(), detailPreviewMaxEdgePixels)
	}
}

func TestLocalVideoThumbnailBatchGeneratesPosterPreview(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "clip.mov"), []byte("video"), 0o644); err != nil {
		t.Fatalf("WriteFile(local video) error = %v", err)
	}
	posterPath := filepath.Join(t.TempDir(), "poster.jpg")
	if err := os.WriteFile(posterPath, encodeJPEGForTest(t, 640, 360), 0o644); err != nil {
		t.Fatalf("WriteFile(poster) error = %v", err)
	}
	helperPath, helperLogPath := writeFakeMediaHelperPosterScript(t, posterPath)

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Videos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-videos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: "1ns",
		},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaHelperPath: helperPath,
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-videos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	if status := service.LocalMediaRuntimeStatus(); !status.MediaHelperAvailable || !status.MediaHelperUsable ||
		status.MediaHelperStatus != "ready" || !status.MediaHelperRenderVideoPoster {
		t.Fatalf("LocalMediaRuntimeStatus() = %+v, want ready video-poster helper", status)
	}

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	metadata, err := service.RunLocalMetadataBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	if metadata.RegisteredAssets != 1 {
		t.Fatalf("metadata result = %+v, want one video asset", metadata)
	}
	thumbnails, err := service.RunLocalThumbnailBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalThumbnailBatch() error = %v", err)
	}
	if thumbnails.ProcessedJobs != 1 || thumbnails.CompletedJobs != 1 || thumbnails.FailedJobs != 0 || thumbnails.GeneratedAssets != 1 || thumbnails.GeneratedImageAssets != 0 {
		t.Fatalf("thumbnail result = %+v, want one generated video poster asset", thumbnails)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE media_type = 'video' AND thumbnail_status = 'ready'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions WHERE status = 'ready'`, 2)
	helperLog, err := os.ReadFile(helperLogPath)
	if err != nil {
		t.Fatalf("ReadFile(helper log) error = %v", err)
	}
	if got := strings.Count(string(helperLog), "\n"); got != 3 {
		t.Fatalf("fake media helper command count = %d, want poster extraction plus two image renders:\n%s", got, string(helperLog))
	}

	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Type != "video" {
		t.Fatalf("timeline page total=%d items=%#v, want one local video asset", page.Total, page.Items)
	}

	request, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/"+page.Items[0].ID+"/preview", nil)
	if err != nil {
		t.Fatalf("NewRequest(preview) error = %v", err)
	}
	preview, err := service.PreviewFromSource(request, page.Items[0].SourceKey, page.Items[0].ID)
	if err != nil {
		t.Fatalf("PreviewFromSource() error = %v", err)
	}
	defer preview.Body.Close()
	previewBody, err := io.ReadAll(preview.Body)
	if err != nil {
		t.Fatalf("ReadAll(preview) error = %v", err)
	}
	if preview.StatusCode != http.StatusOK || preview.Header.Get("Content-Type") != "image/jpeg" || len(previewBody) == 0 {
		t.Fatalf("preview status=%d contentType=%q bytes=%d, want jpeg body", preview.StatusCode, preview.Header.Get("Content-Type"), len(previewBody))
	}
	if _, _, err := image.Decode(bytes.NewReader(previewBody)); err != nil {
		t.Fatalf("decode video poster preview: %v", err)
	}
}

func TestLocalVideoThumbnailBatchDoesNotUseDirectFFmpegWithoutMediaHelper(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "clip.mov"), []byte("video"), 0o644); err != nil {
		t.Fatalf("WriteFile(local video) error = %v", err)
	}
	posterPath := filepath.Join(t.TempDir(), "poster.jpg")
	if err := os.WriteFile(posterPath, encodeJPEGForTest(t, 640, 360), 0o644); err != nil {
		t.Fatalf("WriteFile(poster) error = %v", err)
	}
	ffmpegPath, ffmpegLogPath := writeFakeFFmpegPosterScript(t, posterPath)

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Videos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-videos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: "1ns",
		},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaFFmpegPath: ffmpegPath,
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-videos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	if status := service.LocalMediaRuntimeStatus(); status.MediaHelperAvailable || !status.FFmpegAvailable || !status.FFmpegUsable {
		t.Fatalf("LocalMediaRuntimeStatus() = %+v, want ffmpeg visible but media helper unavailable", status)
	}

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	metadata, err := service.RunLocalMetadataBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	if metadata.RegisteredAssets != 1 {
		t.Fatalf("metadata result = %+v, want one video asset", metadata)
	}
	thumbnails, err := service.RunLocalThumbnailBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalThumbnailBatch() error = %v", err)
	}
	if thumbnails.ProcessedJobs != 0 || thumbnails.CompletedJobs != 0 || thumbnails.FailedJobs != 0 || thumbnails.GeneratedAssets != 0 {
		t.Fatalf("thumbnail result = %+v, want no video poster jobs without media helper", thumbnails)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE media_type = 'video' AND thumbnail_status = 'pending'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'thumbnail'`, 0)

	if ffmpegLog, err := os.ReadFile(ffmpegLogPath); err == nil && strings.TrimSpace(string(ffmpegLog)) != "" {
		t.Fatalf("fake ffmpeg was called without media helper:\n%s", string(ffmpegLog))
	}
}

func TestLocalImageThumbnailBatchDoesNotQueueWithoutMediaHelper(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeJPEGForTest(t, 320, 240), 0o644); err != nil {
		t.Fatalf("WriteFile(local image) error = %v", err)
	}
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: "1ns",
		},
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	if status := service.LocalMediaRuntimeStatus(); status.MediaHelperAvailable {
		t.Fatalf("LocalMediaRuntimeStatus() = %+v, want media helper unavailable", status)
	}
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	metadata, err := service.RunLocalMetadataBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	if metadata.RegisteredAssets != 1 {
		t.Fatalf("metadata result = %+v, want one image asset", metadata)
	}
	pending, err := service.PendingLocalThumbnailJobs(context.Background())
	if err != nil {
		t.Fatalf("PendingLocalThumbnailJobs() error = %v", err)
	}
	if pending != 0 {
		t.Fatalf("PendingLocalThumbnailJobs() = %d, want no schedulable work without media helper", pending)
	}
	for iteration := 0; iteration < 2; iteration++ {
		thumbnails, err := service.RunLocalThumbnailBatch(context.Background(), 10)
		if err != nil {
			t.Fatalf("RunLocalThumbnailBatch(%d) error = %v", iteration, err)
		}
		if thumbnails.ProcessedJobs != 0 || thumbnails.CompletedJobs != 0 || thumbnails.FailedJobs != 0 || thumbnails.GeneratedAssets != 0 {
			t.Fatalf("thumbnail result %d = %+v, want no image jobs without media helper", iteration, thumbnails)
		}
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE media_type = 'image' AND thumbnail_status = 'pending'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'thumbnail'`, 0)
}

func TestLocalVideoThumbnailExplicitMediaHelperFailureDoesNotFallbackToFFmpeg(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake media helper shell script is unix-only")
	}
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "clip.mov"), []byte("video"), 0o644); err != nil {
		t.Fatalf("WriteFile(local video) error = %v", err)
	}
	posterPath := filepath.Join(t.TempDir(), "poster.jpg")
	if err := os.WriteFile(posterPath, encodeJPEGForTest(t, 640, 360), 0o644); err != nil {
		t.Fatalf("WriteFile(poster) error = %v", err)
	}
	helperPath := writeFailingMediaHelperPosterScript(t)
	ffmpegPath, ffmpegLogPath := writeFakeFFmpegPosterScript(t, posterPath)

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Videos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-videos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaHelperPath: helperPath,
		MediaFFmpegPath: ffmpegPath,
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-videos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	thumbnails, err := service.RunLocalThumbnailBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalThumbnailBatch() error = %v", err)
	}
	if thumbnails.ProcessedJobs != 1 || thumbnails.CompletedJobs != 0 || thumbnails.FailedJobs != 1 {
		t.Fatalf("thumbnail result = %+v, want explicit helper failure without fallback", thumbnails)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE media_type = 'video' AND thumbnail_status = 'failed'`, 1)

	if ffmpegLog, err := os.ReadFile(ffmpegLogPath); err == nil && strings.TrimSpace(string(ffmpegLog)) != "" {
		t.Fatalf("fake ffmpeg was called despite explicit media helper failure:\n%s", string(ffmpegLog))
	}
}

func TestLocalSemanticBackfillUsesReadyDetailPreview(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeJPEGForTest(t, 1200, 800), 0o644); err != nil {
		t.Fatalf("WriteFile(local jpeg) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "clip.mov"), []byte("video"), 0o644); err != nil {
		t.Fatalf("WriteFile(local video) error = %v", err)
	}
	posterPath := filepath.Join(t.TempDir(), "poster.jpg")
	if err := os.WriteFile(posterPath, encodeJPEGForTest(t, 640, 360), 0o644); err != nil {
		t.Fatalf("WriteFile(poster) error = %v", err)
	}
	helperPath, _ := writeFakeMediaHelperPosterScript(t, posterPath)

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: sourceKey,
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: "1ns",
		},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaHelperPath: helperPath,
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	ctx := context.Background()
	if _, err := service.RunLocalReconciliationScan(ctx, sourceKey); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	metadata, err := service.RunLocalMetadataBatch(ctx, 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	if metadata.RegisteredAssets != 2 {
		t.Fatalf("metadata result = %+v, want image and video registered", metadata)
	}
	thumbnails, err := service.RunLocalThumbnailBatch(ctx, 10)
	if err != nil {
		t.Fatalf("RunLocalThumbnailBatch() error = %v", err)
	}
	if thumbnails.GeneratedAssets != 2 || thumbnails.FailedJobs != 0 {
		t.Fatalf("thumbnail result = %+v, want image and video poster assets", thumbnails)
	}

	profile := testImageSemanticProfile{}
	candidate := SemanticModelProfileStatus{
		ModelID:       profile.ModelID(),
		VectorSpaceID: profile.VectorSpaceID(),
		EmbeddingDim:  profile.EmbeddingDim(),
		Role:          semanticModelRoleCandidate,
		ProfileKind:   profile.ProfileKind(),
		InputKind:     profile.InputKind(),
	}
	status, err := service.catalog.SemanticBackfillStatus(ctx, sourceKey, candidate)
	if err != nil {
		t.Fatalf("SemanticBackfillStatus() error = %v", err)
	}
	if status.EligibleAssetCount != 1 || status.RemainingVectorCount != 1 || status.Status != semanticBackfillStatusPending {
		t.Fatalf("local semantic status before backfill = %#v, want one pending image asset", status)
	}

	backfill, err := service.catalog.BackfillSemanticVectors(ctx, sourceKey, profile, time.Now().UTC(), SemanticBackfillOptions{
		ImageLoader: service,
		MaxAssets:   10,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors() error = %v", err)
	}
	if backfill.ProcessedVectorCount != 1 ||
		backfill.Status.Status != semanticBackfillStatusIndexing ||
		backfill.Status.CompletedVectorCount != 1 ||
		backfill.Status.IndexedVectorCount != 0 {
		t.Fatalf("backfill result = %#v, want one completed local vector awaiting index publish", backfill)
	}
	if enqueued, err := service.catalog.ReconcileSemanticIndexJobs(ctx, []string{sourceKey}, profile, false, time.Now().UTC()); err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs() error = %v", err)
	} else if enqueued != 1 {
		t.Fatalf("ReconcileSemanticIndexJobs() = %d, want one publish job", enqueued)
	}
	if publish, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{sourceKey}, profile, time.Now().UTC()); err != nil {
		t.Fatalf("PublishNextSemanticIndexJob() error = %v", err)
	} else if !publish.Published ||
		publish.Status.Status != semanticBackfillStatusReady ||
		publish.Status.IndexedVectorCount != 1 {
		t.Fatalf("publish result = %#v, want one ready local vector", publish)
	}

	var embeddingInput string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT embedding_input
		FROM semantic_vectors
		WHERE source_key = ? AND model_id = ?`, sourceKey, profile.ModelID()).Scan(&embeddingInput); err != nil {
		t.Fatalf("read local embedding input: %v", err)
	}
	if !strings.HasPrefix(embeddingInput, "local_detail_preview:image/jpeg") {
		t.Fatalf("embedding input = %q, want local detail preview jpeg", embeddingInput)
	}

	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindSearch,
			Query: &AssetSearchQuery{
				Text: "family",
				Mode: QueryModeSemantic,
			},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize local semantic search: %v", err)
	}
	page, err := service.searchCatalogSemanticAssets(ctx, normalized, profile, AssetSearchOptions{IncludeSemanticScores: true})
	if err != nil {
		t.Fatalf("searchCatalogSemanticAssets() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].SourceKey != sourceKey || page.Items[0].Filename != "family.jpg" {
		t.Fatalf("local semantic page = %#v, want family image result", page)
	}
	if page.Items[0].SemanticScore == nil ||
		page.Resolved.Semantic == nil ||
		page.Resolved.Semantic.IndexedVectorCount != 1 {
		t.Fatalf("local semantic diagnostics = item:%#v resolved:%#v", page.Items[0], page.Resolved.Semantic)
	}
}

func TestLocalThumbnailBatchUsesMediaHelperImageRenderer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake media helper shell script is unix-only")
	}

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeJPEGForTest(t, 128, 96), 0o644); err != nil {
		t.Fatalf("WriteFile(local jpeg) error = %v", err)
	}
	helperPath, helperLogPath := writeFakeMediaHelperImageScript(t)

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: "1ns",
		},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaHelperPath: helperPath,
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	if status := service.LocalMediaRuntimeStatus(); status.Renderer != "media-helper" || !status.MediaHelperAvailable ||
		!status.MediaHelperUsable || !status.MediaHelperRenderImage {
		t.Fatalf("LocalMediaRuntimeStatus() = %+v, want configured media helper image renderer", status)
	}

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	thumbnails, err := service.RunLocalThumbnailBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalThumbnailBatch() error = %v", err)
	}
	if thumbnails.GeneratedAssets != 1 || thumbnails.FailedJobs != 0 {
		t.Fatalf("thumbnail result = %+v, want one generated asset via media helper", thumbnails)
	}
	logBody, err := os.ReadFile(helperLogPath)
	if err != nil {
		t.Fatalf("ReadFile(helper log) error = %v", err)
	}
	if got := strings.Count(string(logBody), "render-image"); got != 2 {
		t.Fatalf("fake media helper render-image count = %d, want 2 calls for preview and detail preview:\n%s", got, string(logBody))
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions WHERE status = 'ready'`, 2)
}

func TestLocalMediaVipsCommandEnvIncludesBundlePaths(t *testing.T) {
	bundleRoot := filepath.Join(t.TempDir(), "media-runtime", "libvips")
	vipsPath := filepath.Join(bundleRoot, "bin", mediaVipsBinaryName())
	moduleDir := filepath.Join(bundleRoot, "lib", "vips-modules-8.16")
	if err := os.MkdirAll(filepath.Dir(vipsPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(vips dir) error = %v", err)
	}
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(module dir) error = %v", err)
	}

	env := localMediaVipsCommandEnv([]string{"PATH=/usr/bin"}, vipsPath)
	assertEnvPathPrefix(t, env, "PATH", filepath.Join(bundleRoot, "bin"))
	assertEnvPathPrefix(t, env, "LD_LIBRARY_PATH", filepath.Join(bundleRoot, "lib"))
	assertEnvPathPrefix(t, env, "DYLD_LIBRARY_PATH", filepath.Join(bundleRoot, "lib"))
	assertEnvPathPrefix(t, env, "DYLD_FALLBACK_LIBRARY_PATH", filepath.Join(bundleRoot, "lib"))
	assertEnvPathPrefix(t, env, "GI_TYPELIB_PATH", filepath.Join(bundleRoot, "lib", "girepository-1.0"))
	assertEnvPathPrefix(t, env, "XDG_DATA_DIRS", filepath.Join(bundleRoot, "share"))
	assertEnvPathPrefix(t, env, "VIPS_MODULE_PATH", moduleDir)
}

func TestLocalMediaRuntimeStatusMarksBundledVips(t *testing.T) {
	bundleRoot := filepath.Join(t.TempDir(), "media-runtime", "libvips")
	vipsPath := filepath.Join(bundleRoot, "bin", mediaVipsBinaryName())
	service, err := NewServiceWithOptions(nil, ServiceOptions{
		MediaVipsPath: vipsPath,
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	status := service.LocalMediaRuntimeStatus()
	if status.Renderer != "unavailable" || status.VipsPath != vipsPath || status.VipsAutoDetected || !status.VipsBundled {
		t.Fatalf("LocalMediaRuntimeStatus() = %+v, want bundled vips backend without helper renderer", status)
	}
}

func TestLocalMediaRuntimeStatusReportsMediaHelperHealth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake helper shell script is unix-only")
	}
	t.Parallel()

	helperPath := filepath.Join(t.TempDir(), mediaHelperBinaryName())
	script := `#!/bin/sh
printf '%s\n' '{"schemaVersion":1,"ok":true,"helper":{"version":"0.1.0-test","platform":"test-platform"},"capabilities":{"renderImage":true,"renderVideoPoster":false,"inspectImage":true,"inspectVideo":false}}'
`
	if err := os.WriteFile(helperPath, []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile(helper) error = %v", err)
	}
	service, err := NewServiceWithOptions(nil, ServiceOptions{
		MediaHelperPath: helperPath,
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	status := service.LocalMediaRuntimeStatus()
	if !status.MediaHelperAvailable || status.MediaHelperAuto || !status.MediaHelperUsable ||
		status.MediaHelperStatus != "ready" || status.MediaHelperVersion != "0.1.0-test" ||
		status.MediaHelperPlatform != "test-platform" || !status.MediaHelperRenderImage ||
		status.MediaHelperRenderVideoPoster || !status.MediaHelperInspectImage || status.MediaHelperInspectVideo {
		t.Fatalf("LocalMediaRuntimeStatus() = %+v, want ready helper health", status)
	}
}

func TestLocalMediaRuntimeStatusReportsMediaHelperFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake helper shell script is unix-only")
	}
	t.Parallel()

	helperPath := filepath.Join(t.TempDir(), mediaHelperBinaryName())
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\necho helper broken >&2\nexit 42\n"), 0o700); err != nil {
		t.Fatalf("WriteFile(helper) error = %v", err)
	}
	service, err := NewServiceWithOptions(nil, ServiceOptions{
		MediaHelperPath: helperPath,
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	status := service.LocalMediaRuntimeStatus()
	if !status.MediaHelperAvailable || status.MediaHelperUsable || status.MediaHelperStatus != "failed" ||
		!strings.Contains(status.MediaHelperLastError, "helper broken") {
		t.Fatalf("LocalMediaRuntimeStatus() = %+v, want failed helper preflight", status)
	}
}

func TestLocalMediaRuntimeStatusReportsFFmpegPreflightFailure(t *testing.T) {
	t.Parallel()

	ffmpegPath := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(ffmpegPath, []byte("#!/bin/sh\necho broken ffmpeg >&2\nexit 42\n"), 0o700); err != nil {
		t.Fatalf("WriteFile(ffmpeg) error = %v", err)
	}
	service, err := NewServiceWithOptions(nil, ServiceOptions{
		MediaFFmpegPath: ffmpegPath,
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	status := service.LocalMediaRuntimeStatus()
	if !status.FFmpegAvailable || status.FFmpegUsable || status.FFmpegStatus != "failed" ||
		!strings.Contains(status.FFmpegLastError, "broken ffmpeg") {
		t.Fatalf("LocalMediaRuntimeStatus() = %+v, want failed ffmpeg preflight", status)
	}
}

func TestLocalGalleryWaitsForBothRenditionsAndPreviewDoesNotGenerate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake media helper shell script is unix-only")
	}
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeJPEGForTest(t, 640, 480), 0o644); err != nil {
		t.Fatalf("WriteFile(local jpeg) error = %v", err)
	}
	helperPath, _ := writeFakeMediaHelperImageScript(t)
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaHelperPath: helperPath,
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	var assetID string
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT asset_id FROM local_assets WHERE filename = 'family.jpg'`).Scan(&assetID); err != nil {
		t.Fatalf("read local asset id: %v", err)
	}
	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() error = %v", err)
	}
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("metadata-only timeline page total=%d items=%d, want asset hidden", page.Total, len(page.Items))
	}

	request, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/"+assetID+"/preview", nil)
	if err != nil {
		t.Fatalf("NewRequest(preview) error = %v", err)
	}
	if _, err := service.PreviewFromSource(request, "1111111111111111", assetID); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("PreviewFromSource() error = %v, want ErrAssetNotFound without on-demand generation", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'pending'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'failed'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'thumbnail'`, 0)

	thumbnails, err := service.RunLocalThumbnailBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalThumbnailBatch() error = %v", err)
	}
	if thumbnails.ProcessedJobs != 1 || thumbnails.GeneratedAssets != 1 || thumbnails.FailedJobs != 0 {
		t.Fatalf("thumbnail result = %+v, want queued retry to generate one asset", thumbnails)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'ready'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions WHERE status = 'ready'`, 2)
	page, err = service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() after rendition generation error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != assetID {
		t.Fatalf("ready timeline page = %#v, want generated asset", page)
	}

	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_renditions SET status = 'failed' WHERE source_key = ? AND asset_id = ? AND kind = 'detail_preview'`, "1111111111111111", assetID); err != nil {
		t.Fatalf("mark detail preview failed: %v", err)
	}
	page, err = service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() with missing detail preview error = %v", err)
	}
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("preview-only timeline page = %#v, want asset hidden until detail preview is ready", page)
	}
}

func TestEffectiveFirstViewThumbnailCountDefaultsToTwoGalleryPages(t *testing.T) {
	t.Parallel()

	datasource := config.DatasourceConfig{Kind: config.DatasourceKindLocalFiles}
	if got := effectiveFirstViewThumbnailCount(datasource); got != 120 {
		t.Fatalf("effectiveFirstViewThumbnailCount() = %d, want 120", got)
	}
}

func TestLocalThumbnailQueuePrioritizesFirstViewOnly(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	files := []struct {
		name string
		size int
		when time.Time
	}{
		{name: "old.jpg", size: 320, when: time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)},
		{name: "new.jpg", size: 360, when: time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)},
		{name: "mid.jpg", size: 340, when: time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC)},
	}
	for _, file := range files {
		path := filepath.Join(rootPath, file.name)
		if err := os.WriteFile(path, encodeJPEGForTest(t, file.size, 240), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", file.name, err)
		}
		if err := os.Chtimes(path, file.when, file.when); err != nil {
			t.Fatalf("Chtimes(%s) error = %v", file.name, err)
		}
	}
	helperPath, _ := writeFakeMediaHelperImageScript(t)

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			FirstViewThumbnailCount: 1,
			SettlingDuration:        "1ns",
		},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaHelperPath: helperPath,
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	if err := service.queuePendingLocalThumbnailJobs(context.Background(), 10); err != nil {
		t.Fatalf("queuePendingLocalThumbnailJobs() error = %v", err)
	}

	priorities := localThumbnailPrioritiesByFilename(t, service)
	if priorities["new.jpg"] != localThumbnailFirstViewPriority {
		t.Fatalf("new.jpg priority = %d, want first-view priority %d", priorities["new.jpg"], localThumbnailFirstViewPriority)
	}
	if priorities["mid.jpg"] != localThumbnailBackgroundPriority || priorities["old.jpg"] != localThumbnailBackgroundPriority {
		t.Fatalf("background priorities = %#v, want only newest file in first-view priority", priorities)
	}
}

func TestLocalThumbnailRequeueIsAsynchronousPrioritizedAndIdempotent(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeJPEGForTest(t, 800, 600), 0o644); err != nil {
		t.Fatalf("WriteFile(local jpeg) error = %v", err)
	}
	helperPath, _ := writeFakeMediaHelperImageScript(t)

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaHelperPath: helperPath,
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	if _, err := service.RunLocalThumbnailBatch(context.Background(), 10); err != nil {
		t.Fatalf("RunLocalThumbnailBatch() error = %v", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'ready'`, 1)

	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_assets SET thumbnail_status = 'failed'`); err != nil {
		t.Fatalf("mark thumbnail failed: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_renditions SET status = 'failed', relative_path = NULL, source_sha1_hex = NULL`); err != nil {
		t.Fatalf("mark renditions failed: %v", err)
	}
	var assetID string
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT asset_id FROM local_assets LIMIT 1`).Scan(&assetID); err != nil {
		t.Fatalf("read local asset id: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, asset_id,
			status, scheduled_at, completed_at, last_error
		) VALUES (?, ?, ?, 'nas-photos', 1, ?, 'failed', ?, ?, ?)`,
		"1111111111111111",
		localThumbnailJobKind,
		localThumbnailRepairPriority,
		assetID,
		formatCatalogTime(time.Now().UTC()),
		formatCatalogTime(time.Now().UTC()),
		"old thumbnail failure",
	); err != nil {
		t.Fatalf("insert stale failed thumbnail job: %v", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'thumbnail' AND status = 'failed'`, 1)
	statuses, err := service.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses() error = %v", err)
	}
	if len(statuses) != 1 || statuses[0].FailedThumbnailJobs != 1 {
		t.Fatalf("status before repair = %+v, want one failed thumbnail asset", statuses)
	}

	requeue, err := service.RequeueFailedLocalThumbnails(context.Background())
	if err != nil {
		t.Fatalf("RequeueFailedLocalThumbnails() error = %v", err)
	}
	if requeue.Queued != 1 {
		t.Fatalf("requeue result = %+v, want one queued thumbnail", requeue)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'pending'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'ready'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions WHERE status = 'pending'`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'thumbnail' AND status = 'queued' AND priority = 1`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'thumbnail' AND status = 'failed'`, 0)
	statuses, err = service.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses() after requeue error = %v", err)
	}
	if len(statuses) != 1 || statuses[0].PendingThumbnailJobs != 1 || statuses[0].QueuedThumbnailJobs != 1 || statuses[0].FailedThumbnailJobs != 0 {
		t.Fatalf("status after requeue = %+v, want one queued and no failed thumbnail assets", statuses)
	}
	diagnostics, err := service.LocalFailureDiagnosticRows(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("LocalFailureDiagnosticRows() after requeue error = %v", err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("failure diagnostics after requeue = %+v, want none", diagnostics)
	}

	requeue, err = service.RequeueFailedLocalThumbnails(context.Background())
	if err != nil {
		t.Fatalf("second RequeueFailedLocalThumbnails() error = %v", err)
	}
	if requeue.Queued != 0 {
		t.Fatalf("second requeue result = %+v, want idempotent no-op", requeue)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'thumbnail' AND status = 'queued'`, 1)

	service.mediaHelperPath = writeFailingMediaHelperImageScript(t)
	failedBatch, err := service.RunLocalThumbnailBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalThumbnailBatch(failing retry) error = %v", err)
	}
	if failedBatch.ProcessedJobs != 1 || failedBatch.FailedJobs != 1 || failedBatch.GeneratedAssets != 0 {
		t.Fatalf("failing retry result = %+v, want one failed thumbnail", failedBatch)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'failed'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions WHERE status = 'failed'`, 2)

	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, asset_id, status, scheduled_at
		) VALUES (?, ?, ?, 'nas-photos', 1, ?, 'queued', ?)`,
		"1111111111111111",
		localThumbnailJobKind,
		localThumbnailBackgroundPriority,
		assetID,
		formatCatalogTime(time.Now().UTC()),
	); err != nil {
		t.Fatalf("insert ordinary queued thumbnail job: %v", err)
	}
	service.mediaHelperPath = helperPath
	requeue, err = service.RequeueFailedLocalThumbnails(context.Background())
	if err != nil {
		t.Fatalf("third RequeueFailedLocalThumbnails() error = %v", err)
	}
	if requeue.Queued != 1 {
		t.Fatalf("third requeue result = %+v, want failed thumbnail queued again", requeue)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'thumbnail' AND status = 'queued'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'thumbnail' AND status = 'queued' AND priority = 1`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'thumbnail' AND status = 'failed'`, 0)
	thumbnail, err := service.RunLocalThumbnailBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalThumbnailBatch(successful retry) error = %v", err)
	}
	if thumbnail.ProcessedJobs != 1 || thumbnail.GeneratedAssets != 1 || thumbnail.FailedJobs != 0 {
		t.Fatalf("successful retry result = %+v, want one generated thumbnail", thumbnail)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'ready'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions WHERE status = 'ready' AND relative_path IS NOT NULL`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'thumbnail' AND status = 'failed'`, 0)
}

func TestLocalPhase0ScanRevalidatesReappearedMissingLocation(t *testing.T) {
	rootPath := t.TempDir()
	photoPath := filepath.Join(rootPath, "family.jpg")
	fixedTime := time.Date(2026, 6, 14, 9, 30, 0, 0, time.UTC)
	if err := os.WriteFile(photoPath, []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(photo) error = %v", err)
	}
	if err := os.Chtimes(photoPath, fixedTime, fixedTime); err != nil {
		t.Fatalf("Chtimes(photo) error = %v", err)
	}

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: "1ns",
		},
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	var originalAssetID string
	if err := service.catalog.db.QueryRowContext(context.Background(),
		`SELECT asset_id FROM local_assets WHERE filename = 'family.jpg'`).Scan(&originalAssetID); err != nil {
		t.Fatalf("read original local asset id: %v", err)
	}

	retiredPhotoPath := filepath.Join(rootPath, "family.retired")
	if err := os.Rename(photoPath, retiredPhotoPath); err != nil {
		t.Fatalf("Rename(photo) error = %v", err)
	}
	missing, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("missing RunLocalReconciliationScan() error = %v", err)
	}
	if missing.MissingPaths != 1 || missing.QueuedMetadata != 0 {
		t.Fatalf("missing scan result = %+v, want one missing and no metadata queue", missing)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'missing'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'active'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM catalog_assets WHERE datasource_kind = 'local_filesystem' AND visibility_status = 'active'`, 0)

	if err := os.WriteFile(photoPath, []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(restored photo) error = %v", err)
	}
	if err := os.Chtimes(photoPath, fixedTime, fixedTime); err != nil {
		t.Fatalf("Chtimes(restored photo) error = %v", err)
	}
	restored, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("restored RunLocalReconciliationScan() error = %v", err)
	}
	if restored.DiscoveredPaths != 1 || restored.MissingPaths != 0 || restored.ChangedPaths != 1 || restored.QueuedMetadata != 1 {
		t.Fatalf("restored scan result = %+v, want replacement identity revalidation", restored)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'discovered'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'missing'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'active'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM catalog_assets WHERE datasource_kind = 'local_filesystem' AND visibility_status = 'active'`, 0)

	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() after restore error = %v", err)
	}
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("page after restore total=%d items=%#v, want metadata-only asset %q hidden until renditions are ready", page.Total, page.Items, originalAssetID)
	}
}

func TestLocalPhase0ScanMarksBlockedSubtreeWithoutMissing(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "blocked"), 0o755); err != nil {
		t.Fatalf("MkdirAll(blocked) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "visible.jpg"), []byte("visible"), 0o644); err != nil {
		t.Fatalf("WriteFile(visible) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "blocked", "hidden.jpg"), []byte("hidden"), 0o644); err != nil {
		t.Fatalf("WriteFile(hidden) error = %v", err)
	}

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: "1ns",
		},
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'active'`, 2)

	originalWalkDir := localPhase0WalkDir
	t.Cleanup(func() {
		localPhase0WalkDir = originalWalkDir
	})
	blockedPath := filepath.Join(rootPath, "blocked")
	localPhase0WalkDir = func(logicalRoot string, rootFS fs.FS, fn fs.WalkDirFunc) error {
		return fs.WalkDir(rootFS, ".", func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return fn(path, entry, err)
			}
			if localPhase0LogicalWalkPath(logicalRoot, path) == blockedPath {
				return fn(path, entry, fs.ErrPermission)
			}
			return fn(path, entry, nil)
		})
	}

	blocked, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("blocked RunLocalReconciliationScan() error = %v", err)
	}
	if blocked.DiscoveredPaths != 1 || blocked.MissingPaths != 0 || blocked.ChangedPaths != 0 || blocked.QueuedMetadata != 0 || blocked.SkipCounts["read_error"] != 1 {
		t.Fatalf("blocked scan result = %+v, want visible-only scan with one read error and no missing", blocked)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'missing'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'permission_blocked'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'active'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'permission_blocked'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM catalog_assets WHERE datasource_kind = 'local_filesystem' AND visibility_status = 'active'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM catalog_assets WHERE datasource_kind = 'local_filesystem' AND visibility_status = 'permission_blocked'`, 1)

	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() after blocked subtree error = %v", err)
	}
	if page.Total != 0 || len(page.Items) != 0 {
		t.Fatalf("page after blocked subtree total=%d items=%#v, want metadata-only visible photo hidden until renditions are ready", page.Total, page.Items)
	}

	localPhase0WalkDir = originalWalkDir
	restored, err := service.RunLocalQuickDiscoveryScan(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("restored RunLocalQuickDiscoveryScan() error = %v", err)
	}
	if restored.DiscoveredPaths != 1 || restored.MissingPaths != 0 || restored.ChangedPaths != 1 || restored.QueuedMetadata != 0 {
		t.Fatalf("restored scan result = %+v, want blocked location restored without metadata", restored)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'permission_blocked'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'active'`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'active'`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM catalog_assets WHERE datasource_kind = 'local_filesystem' AND visibility_status = 'active'`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata'`, 0)
}

func TestNextLocalMetadataJobsUsesWorkerOrderIndexes(t *testing.T) {
	t.Parallel()

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}, {
		SourceKey: "2222222222222222",
		Name:      "Phone Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "phone-photos",
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: t.TempDir(),
		}, {
			Key:  "phone-photos",
			Path: t.TempDir(),
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()
	for _, sourceKey := range []string{"1111111111111111", "2222222222222222"} {
		if _, err := service.RunLocalReconciliationScan(context.Background(), sourceKey); err != nil {
			t.Fatalf("RunLocalReconciliationScan(%s) error = %v", sourceKey, err)
		}
	}

	_, err = service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, location_id, status, scheduled_at, sort_at
		) VALUES
			('1111111111111111', 'metadata', 10, 'nas-photos', 101, 'queued', '2026-01-01T00:00:03Z', '2026-01-05T00:00:00Z'),
			('2222222222222222', 'metadata', 1, 'phone-photos', 201, 'queued', '2026-01-01T00:00:01Z', '2026-01-03T00:00:00Z'),
			('1111111111111111', 'metadata', 2, 'nas-photos', 102, 'queued', '2026-01-01T00:00:02Z', '2026-01-02T00:00:00Z'),
			('1111111111111111', 'metadata', 2, 'nas-photos', 103, 'queued', '2026-01-01T00:00:04Z', '2026-01-04T00:00:00Z'),
			('1111111111111111', 'metadata', 0, 'nas-photos', NULL, 'queued', '2026-01-01T00:00:00Z', '2026-01-06T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert local scan jobs: %v", err)
	}

	allJobs, err := service.nextLocalMetadataJobs(context.Background(), "", 2)
	if err != nil {
		t.Fatalf("nextLocalMetadataJobs(all) error = %v", err)
	}
	if len(allJobs) != 2 || allJobs[0].SourceKey != "2222222222222222" || allJobs[1].LocationID != 103 {
		t.Fatalf("all jobs = %+v, want priority then latest jobs across sources", allJobs)
	}
	sourceJobs, err := service.nextLocalMetadataJobs(context.Background(), "1111111111111111", 2)
	if err != nil {
		t.Fatalf("nextLocalMetadataJobs(source) error = %v", err)
	}
	if len(sourceJobs) != 2 || sourceJobs[0].LocationID != 103 || sourceJobs[1].LocationID != 102 {
		t.Fatalf("source jobs = %+v, want priority then latest jobs for source", sourceJobs)
	}

	planNowText := formatCatalogTime(time.Now().UTC())
	metadataPlan := localScanQueryPlan(t, service.catalog.db, `SELECT
				j.id,
				j.source_key,
				COALESCE(j.root_key, ''),
				j.root_generation,
				j.location_id,
				j.priority,
				j.sort_at
				FROM local_scan_jobs AS j INDEXED BY idx_local_scan_jobs_source_metadata_ready
				JOIN local_scan_root_state rs
					ON rs.source_key = j.source_key
					AND rs.root_key = j.root_key
					AND rs.root_generation = j.root_generation
				WHERE j.source_key = ?
					AND j.root_key = ?
					AND j.root_generation = ?
					AND j.job_kind = ?
				AND j.status = 'queued'
				AND j.scheduled_at <= ?
				AND j.location_id IS NOT NULL
				AND NOT EXISTS (
					SELECT 1 FROM local_asset_locations l
					WHERE l.id = j.location_id AND l.metadata_not_before > ?
					)
				ORDER BY j.priority ASC, j.sort_at DESC, j.id ASC
				LIMIT ?`, "1111111111111111", "nas-photos", int64(1), localMetadataJobKind, planNowText, planNowText, 2)
	if !strings.Contains(metadataPlan, "idx_local_scan_jobs_source_metadata_ready (source_key=? AND root_key=? AND root_generation=? AND job_kind=? AND status=?)") || strings.Contains(metadataPlan, "SCAN j") || strings.Contains(metadataPlan, "USE TEMP B-TREE") {
		t.Fatalf("metadata worker query plan = %s, want ordered current-generation index lookup without a job-table scan or temp sort", metadataPlan)
	}
	queueStatePlan := localScanQueryPlan(t, service.catalog.db, `SELECT COUNT(*)
			FROM local_scan_jobs AS j INDEXED BY idx_local_scan_jobs_source_metadata_ready
			JOIN local_asset_locations l
				ON l.id = j.location_id
				AND l.root_key = j.root_key
			WHERE j.source_key = ?
				AND j.root_key = ?
				AND j.root_generation = ?
				AND j.job_kind = ?
				AND j.status = 'queued'
				AND j.location_id IS NOT NULL`,
		"1111111111111111",
		"nas-photos",
		int64(1),
		localMetadataJobKind,
	)
	if !strings.Contains(queueStatePlan, "idx_local_scan_jobs_source_metadata_ready (source_key=? AND root_key=? AND root_generation=? AND job_kind=? AND status=?)") || strings.Contains(queueStatePlan, "SCAN j") {
		t.Fatalf("metadata queue-state query plan = %s, want current-generation index lookup without a job-table scan", queueStatePlan)
	}
	thumbnailPlan := localScanQueryPlan(t, service.catalog.db, `SELECT
					j.id,
					j.source_key,
					COALESCE(j.root_key, ''),
					j.root_generation,
					j.asset_id,
				COALESCE(a.media_type, ''),
				j.priority,
				j.sort_at,
				j.scheduled_at
				FROM local_scan_jobs AS j INDEXED BY idx_local_scan_jobs_source_thumbnail_ready
				JOIN local_scan_root_state rs
					ON rs.source_key = j.source_key
					AND rs.root_key = j.root_key
					AND rs.root_generation = j.root_generation
					AND rs.reconciliation_pending = 0
				LEFT JOIN local_assets a ON a.source_key = j.source_key AND a.asset_id = j.asset_id
				WHERE j.source_key = ?
					AND j.root_key = ?
					AND j.root_generation = ?
				AND j.job_kind = ?
				AND j.status = 'queued'
					AND j.asset_id IS NOT NULL
				ORDER BY j.priority ASC, j.sort_at DESC, j.scheduled_at ASC, j.id ASC
				LIMIT ?`, "1111111111111111", "nas-photos", int64(1), localThumbnailJobKind, 2)
	if !strings.Contains(thumbnailPlan, "idx_local_scan_jobs_source_thumbnail_ready (source_key=? AND root_key=? AND root_generation=? AND job_kind=? AND status=?)") || strings.Contains(thumbnailPlan, "SCAN j") || strings.Contains(thumbnailPlan, "USE TEMP B-TREE") {
		t.Fatalf("thumbnail worker query plan = %s, want ordered current-generation index lookup without a job-table scan or temp sort", thumbnailPlan)
	}
	locationPlan := localScanQueryPlan(t, service.catalog.db, `SELECT COUNT(*)
			FROM local_scan_jobs
			WHERE source_key = ?
				AND root_generation = ?
					AND job_kind = ?
					AND location_id = ?
					AND status IN ('queued', 'running')`, "1111111111111111", int64(1), localMetadataJobKind, int64(102))
	if !strings.Contains(locationPlan, "idx_local_scan_jobs_location_pending (source_key=? AND job_kind=? AND location_id=? AND status=? AND root_generation=?)") {
		t.Fatalf("metadata location query plan = %s, want location pending index", locationPlan)
	}
	promotionPlan := localScanQueryPlan(t, service.catalog.db, `UPDATE local_scan_jobs INDEXED BY idx_local_scan_jobs_location_pending
			SET scheduled_at = ?, sort_at = ?, root_key = ?, root_generation = ?
			WHERE source_key = ?
				AND job_kind = ?
				AND location_id = ?
				AND status = 'queued'
				AND (scheduled_at != ? OR sort_at != ? OR COALESCE(root_key, '') != ? OR root_generation != ?)`,
		planNowText,
		planNowText,
		"nas-photos",
		int64(2),
		"1111111111111111",
		localMetadataJobKind,
		int64(102),
		planNowText,
		planNowText,
		"nas-photos",
		int64(2),
	)
	if !strings.Contains(promotionPlan, "idx_local_scan_jobs_location_pending (source_key=? AND job_kind=? AND location_id=? AND status=?)") || strings.Contains(promotionPlan, "SCAN local_scan_jobs") {
		t.Fatalf("metadata promotion query plan = %s, want per-location queued-job seek", promotionPlan)
	}
	generationRefreshPlan := localScanQueryPlan(t, service.catalog.db, `UPDATE local_scan_jobs INDEXED BY idx_local_scan_jobs_location_pending
			SET root_key = ?, root_generation = ?
			WHERE source_key = ?
				AND job_kind = ?
				AND location_id = ?
				AND status = 'queued'
				AND (COALESCE(root_key, '') != ? OR root_generation != ?)`,
		"nas-photos",
		int64(2),
		"1111111111111111",
		localMetadataJobKind,
		int64(102),
		"nas-photos",
		int64(2),
	)
	if !strings.Contains(generationRefreshPlan, "idx_local_scan_jobs_location_pending (source_key=? AND job_kind=? AND location_id=? AND status=?)") || strings.Contains(generationRefreshPlan, "SCAN local_scan_jobs") {
		t.Fatalf("metadata generation-refresh query plan = %s, want per-location queued-job seek", generationRefreshPlan)
	}
}

func localThumbnailPrioritiesByFilename(t *testing.T, service *Service) map[string]int {
	t.Helper()

	rows, err := service.catalog.db.QueryContext(context.Background(), `SELECT a.filename, j.priority
		FROM local_scan_jobs j
		JOIN local_assets a
			ON a.source_key = j.source_key AND a.asset_id = j.asset_id
		WHERE j.job_kind = 'thumbnail'
			AND j.status = 'queued'`)
	if err != nil {
		t.Fatalf("query thumbnail priorities: %v", err)
	}
	defer rows.Close()
	priorities := map[string]int{}
	for rows.Next() {
		var filename string
		var priority int
		if err := rows.Scan(&filename, &priority); err != nil {
			t.Fatalf("scan thumbnail priority: %v", err)
		}
		priorities[filename] = priority
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate thumbnail priorities: %v", err)
	}
	return priorities
}

func writeFakeVipsThumbnailScript(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "vips")
	script := `#!/bin/sh
if [ "$1" != "thumbnail" ]; then
  exit 9
fi
input="$2"
output="$3"
output="${output%%[*}"
printf '%s\n' "$*" >> "$TIMICH_FAKE_VIPS_LOG"
cp "$input" "$output"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake vips script: %v", err)
	}
	return path
}

func writeFakeFFmpegPosterScript(t *testing.T, posterPath string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "ffmpeg")
	logPath := filepath.Join(dir, "ffmpeg.log")
	script := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "lavfi" ] || [ "$arg" = "color=c=black:s=16x16:d=0.1" ]; then
    echo "lavfi smoke is not supported by this fake runtime" >&2
    exit 12
  fi
  if [ "$arg" = "-version" ]; then
    echo "ffmpeg version test-ffmpeg"
    exit 0
  fi
  if [ "$arg" = "-decoders" ]; then
    echo " VFS..D h264 H.264"
    echo " VFS..D hevc HEVC"
    exit 0
  fi
done
output=""
smoke=0
for arg in "$@"; do
  output="$arg"
  case "$arg" in
  *timich-ffmpeg-check-*)
    smoke=1
    ;;
  esac
done
if [ -z "$output" ]; then
  exit 8
fi
if [ "$smoke" != "1" ]; then
  printf '%s\n' "$*" >> ` + shellQuoteForTest(logPath) + `
fi
cp ` + shellQuoteForTest(posterPath) + ` "$output"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake ffmpeg script: %v", err)
	}
	return path, logPath
}

func writeFakeMediaHelperPosterScript(t *testing.T, posterPath string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, mediaHelperBinaryName())
	logPath := filepath.Join(dir, "media-helper.log")
	script := `#!/bin/sh
case "$1" in
health)
  printf '%s\n' '{"schemaVersion":1,"ok":true,"helper":{"version":"0.1.0-test","platform":"test-platform"},"capabilities":{"renderImage":true,"renderVideoPoster":true,"inspectImage":false,"inspectVideo":false}}'
  exit 0
  ;;
render-image)
  printf '%s\n' "$*" >> ` + shellQuoteForTest(logPath) + `
  shift
  input=""
  output=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
    --input)
      input="$2"
      shift 2
      ;;
    --output)
      output="$2"
      shift 2
      ;;
    *)
      shift
      ;;
    esac
  done
  if [ -z "$input" ] || [ -z "$output" ]; then
    echo "missing image input or output" >&2
    exit 8
  fi
  cp "$input" "$output"
  printf '%s\n' '{"schemaVersion":1,"ok":true,"operation":"render-image","backend":"libvips-cli","outputPath":"rendition.jpg"}'
  exit 0
  ;;
render-video-poster)
  printf '%s\n' "$*" >> ` + shellQuoteForTest(logPath) + `
  shift
  output=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
    --output)
      output="$2"
      shift 2
      ;;
    *)
      shift
      ;;
    esac
  done
  if [ -z "$output" ]; then
    echo "missing output" >&2
    exit 8
  fi
  cp ` + shellQuoteForTest(posterPath) + ` "$output"
  printf '%s\n' '{"schemaVersion":1,"ok":true,"operation":"render-video-poster","backend":"ffmpeg-cli","outputPath":"poster.jpg"}'
  exit 0
  ;;
*)
  echo "unexpected command: $1" >&2
  exit 2
  ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake media helper script: %v", err)
	}
	return path, logPath
}

func writeFakeMediaHelperImageScript(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, mediaHelperBinaryName())
	logPath := filepath.Join(dir, "media-helper.log")
	script := `#!/bin/sh
case "$1" in
health)
  printf '%s\n' '{"schemaVersion":1,"ok":true,"helper":{"version":"0.1.0-test","platform":"test-platform"},"capabilities":{"renderImage":true,"renderVideoPoster":false,"inspectImage":false,"inspectVideo":false}}'
  exit 0
  ;;
render-image)
  printf '%s\n' "$*" >> ` + shellQuoteForTest(logPath) + `
  shift
  input=""
  output=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
    --input)
      input="$2"
      shift 2
      ;;
    --output)
      output="$2"
      shift 2
      ;;
    *)
      shift
      ;;
    esac
  done
  if [ -z "$input" ] || [ -z "$output" ]; then
    echo "missing image input or output" >&2
    exit 8
  fi
  cp "$input" "$output"
  printf '%s\n' '{"schemaVersion":1,"ok":true,"operation":"render-image","backend":"libvips-cli","outputPath":"rendition.jpg"}'
  exit 0
  ;;
*)
  echo "unexpected command: $1" >&2
  exit 2
  ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake media helper image script: %v", err)
	}
	return path, logPath
}

func writeFailingMediaHelperPosterScript(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), mediaHelperBinaryName())
	script := `#!/bin/sh
echo "media helper poster failure" >&2
exit 42
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write failing media helper script: %v", err)
	}
	return path
}

func writeFailingMediaHelperImageScript(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), mediaHelperBinaryName())
	script := `#!/bin/sh
echo "media helper image failure" >&2
exit 42
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write failing media helper image script: %v", err)
	}
	return path
}

func writeFailingVipsThumbnailScript(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "vips")
	script := `#!/bin/sh
printf 'temporary renderer failure\n' >&2
exit 42
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write failing vips script: %v", err)
	}
	return path
}

func shellQuoteForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func assertEnvPathPrefix(t *testing.T, env []string, key string, want string) {
	t.Helper()

	prefix := key + "="
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			continue
		}
		value := strings.TrimPrefix(item, prefix)
		if value == want || strings.HasPrefix(value, want+string(os.PathListSeparator)) {
			return
		}
		t.Fatalf("%s = %q, want prefix %q", key, value, want)
	}
	t.Fatalf("%s not found in environment: %v", key, env)
}

func assertLocalScanCount(t *testing.T, service *Service, query string, want int) {
	t.Helper()

	var got int
	if err := service.catalog.db.QueryRowContext(context.Background(), query).Scan(&got); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("query %q count = %d, want %d", query, got, want)
	}
}

func localScanQueryPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
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
