package catalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestImmichExternalLibraryChecksumIsNotContentIdentity(t *testing.T) {
	t.Parallel()

	originalPath := "/mnt/photos/2026/family.jpg"
	pathChecksumHex := immichExternalPathChecksumHex(originalPath)
	pathChecksumBytes, err := hex.DecodeString(pathChecksumHex)
	if err != nil {
		t.Fatalf("decode path checksum: %v", err)
	}
	asset := immichAsset{
		Checksum:     base64.StdEncoding.EncodeToString(pathChecksumBytes),
		OriginalPath: originalPath,
		ExifInfo: &immichExif{
			FileSizeInByte: []byte(`3397077`),
		},
	}
	algorithm, checksumHex := asset.UpstreamChecksumIdentity()
	if algorithm != upstreamChecksumAlgorithmSHA1Path || checksumHex != pathChecksumHex {
		t.Fatalf("upstream identity = (%q, %q), want sha1-path %q", algorithm, checksumHex, pathChecksumHex)
	}
	if sha1Hex, sizeBytes := asset.ContentIdentity(); sha1Hex != "" || sizeBytes != 0 {
		t.Fatalf("external content identity = (%q, %d), want unavailable", sha1Hex, sizeBytes)
	}
	if sizeBytes := asset.ContentSizeBytes(); sizeBytes != 3397077 {
		t.Fatalf("nested exif size = %d, want 3397077", sizeBytes)
	}
}

func TestConfiguredImmichExternalLibraryMappingCollapsesExactLocalAssetWithoutInvalidatingVector(t *testing.T) {
	t.Parallel()

	const (
		immichSource = "1111111111111111"
		localSource  = "2222222222222222"
		assetID      = "immich-family"
		localAssetID = "local-family"
		otherSource  = "3333333333333333"
		otherAssetID = "other-photo"
		contentSHA1  = "0123456789abcdef0123456789abcdef01234567"
		otherSHA1    = "abcdef0123456789abcdef0123456789abcdef01"
		contentSize  = int64(3397077)
	)
	originalPath := "/mnt/photos/2026/family.jpg"
	relativePath := "2026/family.jpg"
	withoutMapping := []config.DatasourceConfig{
		{
			SourceKey:   immichSource,
			Name:        "Immich",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://immich.test",
			AccessToken: "test-key",
		},
		{
			SourceKey: localSource,
			Name:      "Local",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "photos",
		},
	}
	service, err := NewServiceWithOptions(withoutMapping, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "photos",
			Path: t.TempDir(),
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	ctx := context.Background()
	now := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	nowText := formatCatalogTime(now)
	if _, err := service.catalog.ReplaceFull(ctx, immichSource, []ImmichMirrorAsset{{
		UpstreamAssetID:           assetID,
		MediaType:                 "image",
		Filename:                  "family.jpg",
		CapturedAt:                now,
		UpstreamChecksumAlgorithm: upstreamChecksumAlgorithmSHA1Path,
		ContentSHA1Hex:            immichExternalPathChecksumHex(originalPath),
		ContentSizeBytes:          contentSize,
	}}, 0, now); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO local_assets (
			source_key, asset_id, sha1_hex, content_size_bytes, media_type, filename,
			captured_at, captured_at_source, visibility_status, thumbnail_status,
			first_seen_at, updated_at
		) VALUES (?, ?, ?, ?, 'image', 'family.jpg', ?, 'exif', 'active', 'ready', ?, ?)`,
		localSource, localAssetID, contentSHA1, contentSize, nowText, nowText, nowText); err != nil {
		t.Fatalf("insert Local asset: %v", err)
	}
	locationResult, err := service.catalog.db.ExecContext(ctx, `INSERT INTO local_asset_locations (
			source_key, asset_id, root_key, relative_path, size_bytes, mtime,
			fast_signature, sha1_hex, status, first_seen_at, last_seen_at, updated_at
		) VALUES (?, ?, 'photos', ?, ?, ?, 'stable', ?, 'active', ?, ?, ?)`,
		localSource, localAssetID, relativePath, contentSize, nowText, contentSHA1, nowText, nowText, nowText)
	if err != nil {
		t.Fatalf("insert Local location: %v", err)
	}
	locationID, err := locationResult.LastInsertId()
	if err != nil {
		t.Fatalf("read Local location id: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename,
			captured_at, visibility_status, upstream_checksum_algorithm,
			content_sha1_hex, content_size_bytes, canonical_content_sha1_hex,
			canonical_content_size_bytes, first_seen_at, updated_at
		) VALUES (?, 'local_filesystem', ?, 'image', 'family.jpg', ?, 'active', ?, ?, ?, ?, ?, ?, ?)`,
		localSource, localAssetID, nowText, upstreamChecksumAlgorithmSHA1,
		contentSHA1, contentSize, contentSHA1, contentSize, nowText, nowText); err != nil {
		t.Fatalf("insert Local catalog source: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename,
			captured_at, visibility_status, upstream_checksum_algorithm,
			content_sha1_hex, content_size_bytes, first_seen_at, updated_at
		) VALUES (?, 'local_filesystem', ?, 'image', 'other.jpg', ?, 'active', ?, ?, 42, ?, ?)`,
		otherSource, otherAssetID, nowText, upstreamChecksumAlgorithmSHA1,
		otherSHA1, nowText, nowText); err != nil {
		t.Fatalf("insert unrelated catalog source: %v", err)
	}
	if _, err := service.catalog.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() before mapping error = %v", err)
	}
	before, err := service.catalog.CatalogDeduplicationStatus(ctx)
	if err != nil {
		t.Fatalf("CatalogDeduplicationStatus() before mapping error = %v", err)
	}
	if before.CanonicalAssets != 3 || before.DuplicateSourceRows != 0 {
		t.Fatalf("before mapping status = %+v, want three independent assets", before)
	}
	var unrelatedCanonicalUpdatedAt string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical.updated_at
		FROM catalog_assets asset
		JOIN catalog_canonical_assets canonical
		  ON canonical.canonical_asset_id = asset.canonical_asset_id
		WHERE asset.source_key = ? AND asset.upstream_asset_id = ?`, otherSource, otherAssetID).
		Scan(&unrelatedCanonicalUpdatedAt); err != nil {
		t.Fatalf("read unrelated canonical timestamp before mapping: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			updated_at
		) VALUES (?, 'test-model', 'test-space', 'ready', 2, 1, 1, 0, 0, ?)`,
		immichSource, nowText); err != nil {
		t.Fatalf("insert semantic state: %v", err)
	}
	insertSemanticVectorForTest(t, service.catalog, ctx, immichSource, assetID,
		"test-model", "test-space", 2, []float32{1, 0}, "preview", "ready", nil, nowText, nil)
	var semanticGenerationBefore int64
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT asset_generation
		FROM semantic_state WHERE source_key = ? AND model_id = 'test-model'`, immichSource).
		Scan(&semanticGenerationBefore); err != nil {
		t.Fatalf("read semantic generation before mapping: %v", err)
	}

	withMapping := append([]config.DatasourceConfig(nil), withoutMapping...)
	withMapping[1].Scan = &config.LocalDatasourceScanConfig{
		ImmichExternalLibraryMappings: []config.LocalDatasourceImmichExternalLibraryMapping{{
			SourceKey:          immichSource,
			OriginalPathPrefix: "/mnt/photos",
		}},
	}
	service.ReconfigureDatasources(withMapping)

	after, err := service.catalog.CatalogDeduplicationStatus(ctx)
	if err != nil {
		t.Fatalf("CatalogDeduplicationStatus() after mapping error = %v", err)
	}
	if after.CanonicalAssets != 2 || after.DuplicateSourceRows != 1 {
		t.Fatalf("after mapping status = %+v, want one exact canonical duplicate plus unrelated asset", after)
	}
	var primarySource string
	var sourceCount int
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT primary_source_key, source_count
		FROM catalog_canonical_assets WHERE duplicate_source_count = 1`).Scan(&primarySource, &sourceCount); err != nil {
		t.Fatalf("read mapped canonical asset: %v", err)
	}
	if primarySource != localSource || sourceCount != 2 {
		t.Fatalf("mapped canonical primary/count = (%q, %d), want Local and 2", primarySource, sourceCount)
	}
	var vectorStatus string
	var payloadBatchID string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT status, payload_batch_id
		FROM semantic_vectors WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, assetID).
		Scan(&vectorStatus, &payloadBatchID); err != nil {
		t.Fatalf("read preserved Immich vector: %v", err)
	}
	if vectorStatus != "ready" || strings.TrimSpace(payloadBatchID) == "" {
		t.Fatalf("mapped Immich vector = (%q, %q), want retained ready payload", vectorStatus, payloadBatchID)
	}
	var semanticGenerationAfter int64
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT asset_generation
		FROM semantic_state WHERE source_key = ? AND model_id = 'test-model'`, immichSource).
		Scan(&semanticGenerationAfter); err != nil {
		t.Fatalf("read semantic generation after mapping: %v", err)
	}
	if semanticGenerationAfter != semanticGenerationBefore {
		t.Fatalf("semantic generation after mapping = %d, want unchanged %d", semanticGenerationAfter, semanticGenerationBefore)
	}
	var unrelatedCanonicalUpdatedAfter string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical.updated_at
		FROM catalog_assets asset
		JOIN catalog_canonical_assets canonical
		  ON canonical.canonical_asset_id = asset.canonical_asset_id
		WHERE asset.source_key = ? AND asset.upstream_asset_id = ?`, otherSource, otherAssetID).
		Scan(&unrelatedCanonicalUpdatedAfter); err != nil {
		t.Fatalf("read unrelated canonical timestamp after mapping: %v", err)
	}
	if unrelatedCanonicalUpdatedAfter != unrelatedCanonicalUpdatedAt {
		t.Fatalf("unrelated canonical timestamp after mapping = %q, want unchanged %q", unrelatedCanonicalUpdatedAfter, unrelatedCanonicalUpdatedAt)
	}

	missingAt := formatCatalogTime(now.Add(time.Minute))
	stateWithMapping := service.datasourceStateSnapshot()
	if _, err := service.catalog.db.ExecContext(ctx, `CREATE TRIGGER fail_missing_external_identity_update
		BEFORE UPDATE OF canonical_content_sha1_hex ON catalog_assets
		WHEN OLD.source_key = '`+immichSource+`'
		BEGIN
			SELECT RAISE(FAIL, 'injected missing identity failure');
		END`); err != nil {
		t.Fatalf("create missing identity failure trigger: %v", err)
	}
	if err := service.markLocalLocationMissing(
		ctx,
		stateWithMapping.externalContentIdentityMappings,
		stateWithMapping.externalContentIdentityScopeKey,
		locationID,
		missingAt,
		"test_absent",
	); err == nil || !strings.Contains(err.Error(), "injected missing identity failure") {
		t.Fatalf("markLocalLocationMissing(injected failure) error = %v, want injected failure", err)
	}
	var locationStatus string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT status FROM local_asset_locations WHERE id = ?`, locationID).
		Scan(&locationStatus); err != nil {
		t.Fatalf("read Local location after failed missing update: %v", err)
	}
	if locationStatus != "active" {
		t.Fatalf("Local location after failed missing update = %q, want transaction rollback to active", locationStatus)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `DROP TRIGGER fail_missing_external_identity_update`); err != nil {
		t.Fatalf("drop missing identity failure trigger: %v", err)
	}
	if err := service.markLocalLocationMissing(
		ctx,
		stateWithMapping.externalContentIdentityMappings,
		stateWithMapping.externalContentIdentityScopeKey,
		locationID,
		missingAt,
		"test_absent",
	); err != nil {
		t.Fatalf("markLocalLocationMissing() error = %v", err)
	}
	afterMissing, err := service.catalog.CatalogDeduplicationStatus(ctx)
	if err != nil {
		t.Fatalf("CatalogDeduplicationStatus() after Local missing error = %v", err)
	}
	if afterMissing.CanonicalAssets != 3 || afterMissing.DuplicateSourceRows != 0 {
		t.Fatalf("after Local missing status = %+v, want mapped identity separated and unrelated asset retained", afterMissing)
	}
	var canonicalContentSHA1 sql.NullString
	var canonicalContentSize sql.NullInt64
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical_content_sha1_hex, canonical_content_size_bytes
		FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, assetID).
		Scan(&canonicalContentSHA1, &canonicalContentSize); err != nil {
		t.Fatalf("read Immich identity after Local missing: %v", err)
	}
	if canonicalContentSHA1.Valid || canonicalContentSize.Valid {
		t.Fatalf("Immich identity after Local missing = (%+v, %+v), want cleared", canonicalContentSHA1, canonicalContentSize)
	}
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT asset_generation
		FROM semantic_state WHERE source_key = ? AND model_id = 'test-model'`, immichSource).
		Scan(&semanticGenerationAfter); err != nil {
		t.Fatalf("read semantic generation after Local missing: %v", err)
	}
	if semanticGenerationAfter != semanticGenerationBefore {
		t.Fatalf("semantic generation after Local missing = %d, want unchanged %d", semanticGenerationAfter, semanticGenerationBefore)
	}

	service.ReconfigureDatasources(withoutMapping)
	removed, err := service.catalog.CatalogDeduplicationStatus(ctx)
	if err != nil {
		t.Fatalf("CatalogDeduplicationStatus() after removing mapping error = %v", err)
	}
	if removed.CanonicalAssets != 3 || removed.DuplicateSourceRows != 0 {
		t.Fatalf("after removing mapping status = %+v, want safe separation", removed)
	}
	if err := service.markLocalLocationMissing(
		ctx,
		stateWithMapping.externalContentIdentityMappings,
		stateWithMapping.externalContentIdentityScopeKey,
		locationID,
		formatCatalogTime(now.Add(90*time.Second)),
		"stale_worker_absent",
	); !errors.Is(err, errExternalContentIdentityScopeChanged) {
		t.Fatalf("stale Local missing worker error = %v, want datasource scope changed", err)
	}

	staleWorkerTx, err := service.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin stale Local worker transaction: %v", err)
	}
	staleWorkerErr := service.catalog.reconcileImmichExternalIdentityForLocalInTx(
		ctx,
		staleWorkerTx,
		withMapping[1],
		immichExternalIdentityScopeKey(withMapping),
		relativePath,
		contentSHA1,
		contentSize,
		"image",
		formatCatalogTime(now.Add(2*time.Minute)),
	)
	_ = staleWorkerTx.Rollback()
	if !errors.Is(staleWorkerErr, errExternalContentIdentityScopeChanged) {
		t.Fatalf("stale Local worker reconciliation error = %v, want datasource scope changed", staleWorkerErr)
	}
	if changed, err := service.catalog.reconcileConfiguredImmichExternalIdentities(ctx, withoutMapping); err != nil || changed != 0 {
		t.Fatalf("cached no-mapping reconciliation = (%d, %v), want no-op", changed, err)
	}
	afterStaleWorker, err := service.catalog.CatalogDeduplicationStatus(ctx)
	if err != nil {
		t.Fatalf("CatalogDeduplicationStatus() after stale Local worker error = %v", err)
	}
	if afterStaleWorker.CanonicalAssets != 3 || afterStaleWorker.DuplicateSourceRows != 0 {
		t.Fatalf("after stale Local worker status = %+v, want removed mapping to stay separated", afterStaleWorker)
	}

	staleMirrorAsset := ImmichMirrorAsset{
		UpstreamAssetID:                 assetID,
		MediaType:                       "image",
		Filename:                        "family.jpg",
		CapturedAt:                      now,
		UpstreamChecksumAlgorithm:       upstreamChecksumAlgorithmSHA1Path,
		ContentSHA1Hex:                  immichExternalPathChecksumHex(originalPath),
		CanonicalContentSHA1Hex:         contentSHA1,
		CanonicalContentSizeBytes:       contentSize,
		MappedLocalSourceKey:            localSource,
		MappedLocalRootKey:              "photos",
		MappedLocalRelativePath:         relativePath,
		ExternalContentIdentityScopeKey: immichExternalIdentityScopeKey(withMapping),
	}
	if _, err := service.catalog.ReplaceFull(ctx, immichSource, []ImmichMirrorAsset{staleMirrorAsset}, 0, now.Add(3*time.Minute)); !errors.Is(err, errExternalContentIdentityScopeChanged) {
		t.Fatalf("stale Immich mirror sync error = %v, want datasource scope changed", err)
	}
	afterStaleMirror, err := service.catalog.CatalogDeduplicationStatus(ctx)
	if err != nil {
		t.Fatalf("CatalogDeduplicationStatus() after stale mirror error = %v", err)
	}
	if afterStaleMirror.CanonicalAssets != 3 || afterStaleMirror.DuplicateSourceRows != 0 {
		t.Fatalf("after stale mirror status = %+v, want removed mapping to stay separated", afterStaleMirror)
	}
}

func TestMissingLocalPathFallsBackToAnotherActiveExternalIdentityMapping(t *testing.T) {
	t.Parallel()

	const (
		immichSource = "1111111111111111"
		localSourceA = "3333333333333333"
		localSourceB = "2222222222222222"
		rootA        = "photos-a"
		rootB        = "photos-b"
		assetID      = "immich-shared"
		localAssetA  = "local-a"
		localAssetB  = "local-b"
		sha1A        = "0123456789abcdef0123456789abcdef01234567"
		sha1B        = "abcdef0123456789abcdef0123456789abcdef01"
		sizeA        = int64(101)
		sizeB        = int64(202)
	)
	const relativePath = "2026/shared.jpg"
	const originalPath = "/mnt/photos/2026/shared.jpg"
	localDatasource := func(sourceKey string, rootKey string) config.DatasourceConfig {
		return config.DatasourceConfig{
			SourceKey: sourceKey,
			Name:      sourceKey,
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   rootKey,
			Scan: &config.LocalDatasourceScanConfig{
				ImmichExternalLibraryMappings: []config.LocalDatasourceImmichExternalLibraryMapping{{
					SourceKey:          immichSource,
					OriginalPathPrefix: "/mnt/photos",
				}},
			},
		}
	}
	datasources := []config.DatasourceConfig{
		{
			SourceKey:   immichSource,
			Name:        "Immich",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://immich.test",
			AccessToken: "test-key",
		},
		localDatasource(localSourceB, rootB),
		localDatasource(localSourceA, rootA),
	}
	service, err := NewServiceWithOptions(datasources, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{
			{Key: rootA, Path: t.TempDir()},
			{Key: rootB, Path: t.TempDir()},
		},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	ctx := context.Background()
	now := time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC)
	nowText := formatCatalogTime(now)
	if _, err := service.catalog.ReplaceFull(ctx, immichSource, []ImmichMirrorAsset{{
		UpstreamAssetID:           assetID,
		MediaType:                 "image",
		Filename:                  "shared.jpg",
		CapturedAt:                now,
		UpstreamChecksumAlgorithm: upstreamChecksumAlgorithmSHA1Path,
		ContentSHA1Hex:            immichExternalPathChecksumHex(originalPath),
		ContentSizeBytes:          sizeA,
	}}, 0, now); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}
	insertLocalIdentity := func(sourceKey string, rootKey string, localAssetID string, sha1Hex string, sizeBytes int64) int64 {
		t.Helper()
		if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO local_assets (
				source_key, asset_id, sha1_hex, content_size_bytes, media_type, filename,
				captured_at, captured_at_source, visibility_status, thumbnail_status,
				first_seen_at, updated_at
			) VALUES (?, ?, ?, ?, 'image', 'shared.jpg', ?, 'exif', 'active', 'ready', ?, ?)`,
			sourceKey, localAssetID, sha1Hex, sizeBytes, nowText, nowText, nowText); err != nil {
			t.Fatalf("insert Local asset %s: %v", sourceKey, err)
		}
		result, err := service.catalog.db.ExecContext(ctx, `INSERT INTO local_asset_locations (
				source_key, asset_id, root_key, relative_path, size_bytes, mtime,
				fast_signature, sha1_hex, status, first_seen_at, last_seen_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, 'stable', ?, 'active', ?, ?, ?)`,
			sourceKey, localAssetID, rootKey, relativePath, sizeBytes, nowText, sha1Hex, nowText, nowText, nowText)
		if err != nil {
			t.Fatalf("insert Local location %s: %v", sourceKey, err)
		}
		locationID, err := result.LastInsertId()
		if err != nil {
			t.Fatalf("read Local location id %s: %v", sourceKey, err)
		}
		return locationID
	}
	locationA := insertLocalIdentity(localSourceA, rootA, localAssetA, sha1A, sizeA)
	_ = insertLocalIdentity(localSourceB, rootB, localAssetB, sha1B, sizeB)

	state := service.datasourceStateSnapshot()
	tx, err := service.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin initial external identity transaction: %v", err)
	}
	if err := service.catalog.reconcileImmichExternalIdentityForLocalInTx(
		ctx,
		tx,
		datasources[2],
		state.externalContentIdentityScopeKey,
		relativePath,
		sha1A,
		sizeA,
		"image",
		nowText,
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("apply initial external identity: %v", err)
	}
	if err := service.catalog.commitCatalogAssetChanges(ctx, tx, true); err != nil {
		t.Fatalf("commit initial external identity: %v", err)
	}

	if err := service.markLocalLocationMissing(
		ctx,
		state.externalContentIdentityMappings,
		state.externalContentIdentityScopeKey,
		locationA,
		formatCatalogTime(now.Add(time.Minute)),
		"test_absent",
	); err != nil {
		t.Fatalf("markLocalLocationMissing() error = %v", err)
	}
	var mappedSHA1 string
	var mappedSize int64
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical_content_sha1_hex, canonical_content_size_bytes
		FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, assetID).
		Scan(&mappedSHA1, &mappedSize); err != nil {
		t.Fatalf("read replacement external identity: %v", err)
	}
	if mappedSHA1 != sha1B || mappedSize != sizeB {
		t.Fatalf("replacement external identity = (%q, %d), want active Local B (%q, %d)", mappedSHA1, mappedSize, sha1B, sizeB)
	}
}

func TestLocalIdentityLossReconcilesExternalIdentityAtomically(t *testing.T) {
	t.Parallel()

	const (
		immichSource = "1111111111111111"
		localSource  = "2222222222222222"
		assetID      = "immich-family"
		relativePath = "2026/family.jpg"
		originalPath = "/mnt/photos/2026/family.jpg"
	)
	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(media parent) error = %v", err)
	}
	if err := os.WriteFile(mediaPath, []byte("phase0-external-identity"), 0o644); err != nil {
		t.Fatalf("WriteFile(media) error = %v", err)
	}
	datasources := []config.DatasourceConfig{
		{
			SourceKey:   immichSource,
			Name:        "Immich",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://immich.test",
			AccessToken: "test-key",
		},
		{
			SourceKey: localSource,
			Name:      "Local",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "photos",
			Scan: &config.LocalDatasourceScanConfig{
				SettlingDuration: "1ns",
				ImmichExternalLibraryMappings: []config.LocalDatasourceImmichExternalLibraryMapping{{
					SourceKey:          immichSource,
					OriginalPathPrefix: "/mnt/photos",
				}},
			},
		},
	}
	service, err := NewServiceWithOptions(datasources, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	contentSHA1, contentSize, err := sha1File(mediaPath)
	if err != nil {
		t.Fatalf("sha1File(media) error = %v", err)
	}
	if _, err := service.catalog.ReplaceFull(ctx, immichSource, []ImmichMirrorAsset{{
		UpstreamAssetID:           assetID,
		MediaType:                 "image",
		Filename:                  "family.jpg",
		CapturedAt:                now,
		UpstreamChecksumAlgorithm: upstreamChecksumAlgorithmSHA1Path,
		ContentSHA1Hex:            immichExternalPathChecksumHex(originalPath),
		ContentSizeBytes:          contentSize,
	}}, 0, now); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}
	if _, err := service.RunLocalReconciliationScan(ctx, localSource); err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.RunLocalMetadataBatch(ctx, 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	var mappedSHA1 string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical_content_sha1_hex
		FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, assetID).
		Scan(&mappedSHA1); err != nil {
		t.Fatalf("read initial mapped Immich identity: %v", err)
	}
	if mappedSHA1 != contentSHA1 {
		t.Fatalf("initial mapped Immich identity = %q, want %q", mappedSHA1, contentSHA1)
	}

	initialInfo, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatalf("Stat(media before content verification) error = %v", err)
	}
	if err := os.WriteFile(mediaPath, []byte(strings.Repeat("x", int(contentSize))), 0o644); err != nil {
		t.Fatalf("WriteFile(content replacement) error = %v", err)
	}
	if err := os.Chtimes(mediaPath, initialInfo.ModTime(), initialInfo.ModTime()); err != nil {
		t.Fatalf("Chtimes(content replacement) error = %v", err)
	}
	verificationOutcome, err := service.verifyNextLocalContent(
		ctx,
		localSource,
		formatCatalogTime(time.Now().UTC().Add(time.Hour)),
	)
	if err != nil {
		t.Fatalf("verifyNextLocalContent(hash mismatch) error = %v", err)
	}
	if !verificationOutcome.found || !verificationOutcome.changed {
		t.Fatalf("verifyNextLocalContent(hash mismatch) outcome = %+v, want changed candidate", verificationOutcome)
	}
	var canonicalContentSHA1 sql.NullString
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical_content_sha1_hex
		FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, assetID).
		Scan(&canonicalContentSHA1); err != nil {
		t.Fatalf("read Immich identity after content hash mismatch: %v", err)
	}
	if canonicalContentSHA1.Valid {
		t.Fatalf("Immich identity after content hash mismatch = %+v, want cleared", canonicalContentSHA1)
	}
	if _, err := service.RunLocalMetadataBatch(ctx, 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch(after content hash mismatch) error = %v", err)
	}
	contentSHA1, contentSize, err = sha1File(mediaPath)
	if err != nil {
		t.Fatalf("sha1File(after content hash mismatch) error = %v", err)
	}
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical_content_sha1_hex
		FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, assetID).
		Scan(&mappedSHA1); err != nil {
		t.Fatalf("read remapped Immich identity after content verification: %v", err)
	}
	if mappedSHA1 != contentSHA1 {
		t.Fatalf("remapped Immich identity after content verification = %q, want %q", mappedSHA1, contentSHA1)
	}

	if err := os.WriteFile(mediaPath, []byte("phase0-signature-change-with-new-size"), 0o644); err != nil {
		t.Fatalf("WriteFile(Phase 0 signature change) error = %v", err)
	}
	phase0ChangedAt := time.Now().UTC().Add(time.Second)
	if err := os.Chtimes(mediaPath, phase0ChangedAt, phase0ChangedAt); err != nil {
		t.Fatalf("Chtimes(Phase 0 signature change) error = %v", err)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `CREATE TRIGGER fail_phase0_signature_local_visibility_update
		BEFORE UPDATE OF visibility_status ON local_assets
		WHEN OLD.source_key = '`+localSource+`' AND NEW.visibility_status = 'missing'
		BEGIN
			SELECT RAISE(FAIL, 'injected Phase 0 signature visibility failure');
		END`); err != nil {
		t.Fatalf("create Phase 0 signature visibility failure trigger: %v", err)
	}
	if _, err := service.RunLocalReconciliationScan(ctx, localSource); err == nil || !strings.Contains(err.Error(), "injected Phase 0 signature visibility failure") {
		t.Fatalf("RunLocalReconciliationScan(signature injected failure) error = %v, want injected failure", err)
	}
	var phase0LocationStatus string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT status FROM local_asset_locations
		WHERE source_key = ? AND root_key = 'photos' AND relative_path = ?`, localSource, relativePath).
		Scan(&phase0LocationStatus); err != nil {
		t.Fatalf("read location after failed Phase 0 signature change: %v", err)
	}
	if phase0LocationStatus != "active" {
		t.Fatalf("location after failed Phase 0 signature change = %q, want transaction rollback to active", phase0LocationStatus)
	}
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical_content_sha1_hex
		FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, assetID).
		Scan(&mappedSHA1); err != nil {
		t.Fatalf("read mapped identity after failed Phase 0 signature change: %v", err)
	}
	if mappedSHA1 != contentSHA1 {
		t.Fatalf("mapped identity after failed Phase 0 signature change = %q, want unchanged %q", mappedSHA1, contentSHA1)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `DROP TRIGGER fail_phase0_signature_local_visibility_update`); err != nil {
		t.Fatalf("drop Phase 0 signature visibility failure trigger: %v", err)
	}
	if _, err := service.RunLocalReconciliationScan(ctx, localSource); err != nil {
		t.Fatalf("RunLocalReconciliationScan(signature change) error = %v", err)
	}
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical_content_sha1_hex
		FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, assetID).
		Scan(&canonicalContentSHA1); err != nil {
		t.Fatalf("read Immich identity after Phase 0 signature change: %v", err)
	}
	if canonicalContentSHA1.Valid {
		t.Fatalf("Immich identity after Phase 0 signature change = %+v, want cleared", canonicalContentSHA1)
	}
	if _, err := service.RunLocalMetadataBatch(ctx, 10); err != nil {
		t.Fatalf("RunLocalMetadataBatch(after Phase 0 signature change) error = %v", err)
	}
	contentSHA1, contentSize, err = sha1File(mediaPath)
	if err != nil {
		t.Fatalf("sha1File(after Phase 0 signature change) error = %v", err)
	}
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical_content_sha1_hex
		FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, assetID).
		Scan(&mappedSHA1); err != nil {
		t.Fatalf("read remapped Immich identity after Phase 0: %v", err)
	}
	if mappedSHA1 != contentSHA1 {
		t.Fatalf("remapped Immich identity after Phase 0 = %q, want %q", mappedSHA1, contentSHA1)
	}

	var locationID int64
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT id FROM local_asset_locations
		WHERE source_key = ? AND root_key = 'photos' AND relative_path = ?`, localSource, relativePath).
		Scan(&locationID); err != nil {
		t.Fatalf("read location before permission-blocked transition: %v", err)
	}
	permissionScanner := localPhase0Scanner{
		service:                         service,
		datasource:                      datasources[1],
		root:                            config.LocalMediaRootConfig{Key: "photos", Path: rootPath},
		nowText:                         formatCatalogTime(time.Now().UTC()),
		externalContentIdentityMappings: configuredImmichExternalLibraryMappings(datasources),
		externalContentIdentityScopeKey: immichExternalIdentityScopeKey(datasources),
	}
	blockedCount, err := permissionScanner.markLocationIDs(ctx, []int64{locationID}, "permission_blocked", "test_read_error")
	if err != nil {
		t.Fatalf("markLocationIDs(permission_blocked) error = %v", err)
	}
	if blockedCount != 1 {
		t.Fatalf("markLocationIDs(permission_blocked) count = %d, want 1", blockedCount)
	}
	var locationStatus string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT status FROM local_asset_locations
		WHERE source_key = ? AND root_key = 'photos' AND relative_path = ?`, localSource, relativePath).
		Scan(&locationStatus); err != nil {
		t.Fatalf("read permission-blocked location: %v", err)
	}
	if locationStatus != "permission_blocked" {
		t.Fatalf("permission-blocked location status = %q, want permission_blocked", locationStatus)
	}
	var localVisibility string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT asset.visibility_status
		FROM local_asset_locations location
		JOIN local_assets asset ON asset.source_key = location.source_key AND asset.asset_id = location.asset_id
		WHERE location.id = ?`, locationID).Scan(&localVisibility); err != nil {
		t.Fatalf("read Local visibility after permission block: %v", err)
	}
	if localVisibility != "permission_blocked" {
		t.Fatalf("Local visibility after permission block = %q, want permission_blocked", localVisibility)
	}
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical_content_sha1_hex
		FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, assetID).
		Scan(&canonicalContentSHA1); err != nil {
		t.Fatalf("read Immich identity after permission block: %v", err)
	}
	if canonicalContentSHA1.Valid {
		t.Fatalf("Immich identity after permission block = %+v, want cleared", canonicalContentSHA1)
	}
	if _, err := service.RunLocalReconciliationScan(ctx, localSource); err != nil {
		t.Fatalf("RunLocalReconciliationScan(after permission block) error = %v", err)
	}
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical_content_sha1_hex
		FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, assetID).
		Scan(&mappedSHA1); err != nil {
		t.Fatalf("read restored Immich identity after permission block: %v", err)
	}
	if mappedSHA1 != contentSHA1 {
		t.Fatalf("restored Immich identity after permission block = %q, want %q", mappedSHA1, contentSHA1)
	}

	if err := os.Remove(mediaPath); err != nil {
		t.Fatalf("Remove(media) error = %v", err)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `CREATE TRIGGER fail_phase0_external_identity_update
		BEFORE UPDATE OF canonical_content_sha1_hex ON catalog_assets
		WHEN OLD.source_key = '`+immichSource+`'
		BEGIN
			SELECT RAISE(FAIL, 'injected phase0 identity failure');
		END`); err != nil {
		t.Fatalf("create Phase 0 identity failure trigger: %v", err)
	}
	if _, err := service.RunLocalReconciliationScan(ctx, localSource); err == nil || !strings.Contains(err.Error(), "injected phase0 identity failure") {
		t.Fatalf("RunLocalReconciliationScan(injected failure) error = %v, want injected failure", err)
	}
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT status FROM local_asset_locations
		WHERE source_key = ? AND root_key = 'photos' AND relative_path = ?`, localSource, relativePath).
		Scan(&locationStatus); err != nil {
		t.Fatalf("read location after failed Phase 0 scan: %v", err)
	}
	if locationStatus != "active" {
		t.Fatalf("location after failed Phase 0 scan = %q, want transaction rollback to active", locationStatus)
	}
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical_content_sha1_hex
		FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, assetID).
		Scan(&mappedSHA1); err != nil {
		t.Fatalf("read mapped identity after failed Phase 0 scan: %v", err)
	}
	if mappedSHA1 != contentSHA1 {
		t.Fatalf("mapped identity after failed Phase 0 scan = %q, want unchanged %q", mappedSHA1, contentSHA1)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `DROP TRIGGER fail_phase0_external_identity_update`); err != nil {
		t.Fatalf("drop Phase 0 identity failure trigger: %v", err)
	}
	result, err := service.RunLocalReconciliationScan(ctx, localSource)
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan(retry) error = %v", err)
	}
	if result.MissingPaths != 1 {
		t.Fatalf("RunLocalReconciliationScan(retry) missing paths = %d, want 1", result.MissingPaths)
	}
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT status FROM local_asset_locations
		WHERE source_key = ? AND root_key = 'photos' AND relative_path = ?`, localSource, relativePath).
		Scan(&locationStatus); err != nil {
		t.Fatalf("read location after successful Phase 0 scan: %v", err)
	}
	if locationStatus != "missing" {
		t.Fatalf("location after successful Phase 0 scan = %q, want missing", locationStatus)
	}
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical_content_sha1_hex
		FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`, immichSource, assetID).
		Scan(&canonicalContentSHA1); err != nil {
		t.Fatalf("read Immich identity after successful Phase 0 scan: %v", err)
	}
	if canonicalContentSHA1.Valid {
		t.Fatalf("Immich identity after successful Phase 0 scan = %+v, want cleared", canonicalContentSHA1)
	}
	deduplication, err := service.catalog.CatalogDeduplicationStatus(ctx)
	if err != nil {
		t.Fatalf("CatalogDeduplicationStatus() error = %v", err)
	}
	if deduplication.DuplicateSourceRows != 0 {
		t.Fatalf("deduplication after successful Phase 0 scan = %+v, want no stale duplicate", deduplication)
	}
}

func TestDatasourceReconfigureKeepsPublishedScopeWhenExternalIdentityReconciliationFails(t *testing.T) {
	t.Parallel()

	const (
		immichSource = "1111111111111111"
		localSource  = "2222222222222222"
	)
	withoutMapping := []config.DatasourceConfig{
		{
			SourceKey:   immichSource,
			Name:        "Immich",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://immich.test",
			AccessToken: "test-key",
		},
		{
			SourceKey: localSource,
			Name:      "Local",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "photos",
		},
	}
	withMapping := append([]config.DatasourceConfig(nil), withoutMapping...)
	withMapping[1].Scan = &config.LocalDatasourceScanConfig{
		ImmichExternalLibraryMappings: []config.LocalDatasourceImmichExternalLibraryMapping{{
			SourceKey:          immichSource,
			OriginalPathPrefix: "/mnt/photos",
		}},
	}
	service, err := NewServiceWithOptions(withoutMapping, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "photos",
			Path: t.TempDir(),
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	ctx := context.Background()
	oldScopeKey := immichExternalIdentityScopeKey(withoutMapping)
	newScopeKey := immichExternalIdentityScopeKey(withMapping)
	if _, err := service.catalog.db.ExecContext(ctx, `CREATE TRIGGER fail_external_identity_scope_change
		BEFORE UPDATE ON catalog_external_identity_state
		WHEN NEW.scope_key <> OLD.scope_key
		BEGIN
			SELECT RAISE(FAIL, 'injected external identity scope failure');
		END`); err != nil {
		t.Fatalf("create reconciliation failure trigger: %v", err)
	}

	if err := service.ReconfigureDatasources(withMapping); err == nil || !strings.Contains(err.Error(), "injected external identity scope failure") {
		t.Fatalf("ReconfigureDatasources(failing) error = %v, want injected failure", err)
	}
	if state := service.datasourceStateSnapshot(); state == nil || state.externalContentIdentityScopeKey != oldScopeKey {
		t.Fatalf("published scope after failure = %+v, want old scope %q", state, oldScopeKey)
	}
	var durableScopeKey string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT scope_key FROM catalog_external_identity_state WHERE singleton_id = 1`).Scan(&durableScopeKey); err != nil {
		t.Fatalf("read durable scope after failure: %v", err)
	}
	if durableScopeKey != oldScopeKey {
		t.Fatalf("durable scope after failure = %q, want old scope %q", durableScopeKey, oldScopeKey)
	}
	for attempt := 0; attempt < 2; attempt++ {
		tx, err := service.catalog.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin old-scope worker attempt %d: %v", attempt+1, err)
		}
		matches, matchErr := catalogExternalIdentityScopeMatchesInTx(ctx, tx, oldScopeKey)
		_ = tx.Rollback()
		if matchErr != nil || !matches {
			t.Fatalf("old-scope worker attempt %d matches=%t error=%v, want current", attempt+1, matches, matchErr)
		}
	}

	if _, err := service.catalog.db.ExecContext(ctx, `DROP TRIGGER fail_external_identity_scope_change`); err != nil {
		t.Fatalf("drop reconciliation failure trigger: %v", err)
	}
	if err := service.ReconfigureDatasources(withMapping); err != nil {
		t.Fatalf("ReconfigureDatasources(retry) error = %v", err)
	}
	if state := service.datasourceStateSnapshot(); state == nil || state.externalContentIdentityScopeKey != newScopeKey {
		t.Fatalf("published scope after retry = %+v, want new scope %q", state, newScopeKey)
	}
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT scope_key FROM catalog_external_identity_state WHERE singleton_id = 1`).Scan(&durableScopeKey); err != nil {
		t.Fatalf("read durable scope after retry: %v", err)
	}
	if durableScopeKey != newScopeKey {
		t.Fatalf("durable scope after retry = %q, want new scope %q", durableScopeKey, newScopeKey)
	}
}

func TestDatasourceReconfigureFailureKeepsPublishedGalleryProjectionScope(t *testing.T) {
	t.Parallel()

	const (
		firstImmich  = "1111111111111111"
		firstLocal   = "2222222222222222"
		secondImmich = "3333333333333333"
		secondLocal  = "4444444444444444"
	)
	mixedDatasources := func(localSource string, localRoot string, immichSource string, originalPrefix string) []config.DatasourceConfig {
		return []config.DatasourceConfig{
			{
				SourceKey:   immichSource,
				Name:        "Immich",
				Kind:        config.DatasourceKindImmichIndexed,
				URL:         "http://immich.test",
				AccessToken: "test-key",
			},
			{
				SourceKey: localSource,
				Name:      "Local",
				Kind:      config.DatasourceKindLocalFiles,
				RootKey:   localRoot,
				Scan: &config.LocalDatasourceScanConfig{
					ImmichExternalLibraryMappings: []config.LocalDatasourceImmichExternalLibraryMapping{{
						SourceKey:          immichSource,
						OriginalPathPrefix: originalPrefix,
					}},
				},
			},
		}
	}
	initialDatasources := mixedDatasources(firstLocal, "photos-a", firstImmich, "/mnt/photos-a")
	nextDatasources := mixedDatasources(secondLocal, "photos-b", secondImmich, "/mnt/photos-b")
	service, err := NewServiceWithOptions(initialDatasources, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{
			{Key: "photos-a", Path: t.TempDir()},
			{Key: "photos-b", Path: t.TempDir()},
		},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	ctx := context.Background()
	var initialProjectionScope string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT scope_key
		FROM catalog_gallery_projection_state WHERE singleton_id = 1`).Scan(&initialProjectionScope); err != nil {
		t.Fatalf("read initial Gallery projection scope: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `CREATE TRIGGER fail_external_identity_scope_change
		BEFORE UPDATE ON catalog_external_identity_state
		WHEN NEW.scope_key <> OLD.scope_key
		BEGIN
			SELECT RAISE(FAIL, 'injected external identity scope failure');
		END`); err != nil {
		t.Fatalf("create reconciliation failure trigger: %v", err)
	}

	if err := service.ReconfigureDatasources(nextDatasources); err == nil || !strings.Contains(err.Error(), "injected external identity scope failure") {
		t.Fatalf("ReconfigureDatasources(failing) error = %v, want injected failure", err)
	}
	var projectionScopeAfterFailure string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT scope_key
		FROM catalog_gallery_projection_state WHERE singleton_id = 1`).Scan(&projectionScopeAfterFailure); err != nil {
		t.Fatalf("read Gallery projection scope after failure: %v", err)
	}
	if projectionScopeAfterFailure != initialProjectionScope {
		t.Fatalf("Gallery projection scope after failure = %q, want %q", projectionScopeAfterFailure, initialProjectionScope)
	}
	publishedState := service.datasourceStateSnapshot()
	if publishedState == nil || publishedState.externalContentIdentityScopeKey != immichExternalIdentityScopeKey(initialDatasources) {
		t.Fatalf("published datasource state after failure = %+v, want initial scope", publishedState)
	}
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
	}
	if _, handled, err := service.catalog.searchGalleryProjection(ctx, normalized, publishedState.galleryReadiness); err != nil || !handled {
		t.Fatalf("searchGalleryProjection() handled=%t error=%v, want prior projection handled", handled, err)
	}
}

func TestImmichFullSyncCanonicalIdentityEnrichmentDoesNotAdvanceSemanticGeneration(t *testing.T) {
	t.Parallel()

	const (
		sourceKey   = "1111111111111111"
		assetID     = "external-family"
		contentSHA1 = "0123456789abcdef0123456789abcdef01234567"
		contentSize = int64(3397077)
	)
	ctx := context.Background()
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	nowText := formatCatalogTime(now)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			updated_at
		) VALUES (?, 'test-model', 'test-space', 'ready', 2, 0, 0, 0, 0, ?)`,
		sourceKey, nowText); err != nil {
		t.Fatalf("insert semantic state: %v", err)
	}
	originalPath := "/mnt/photos/2026/family.jpg"
	pathChecksum := immichExternalPathChecksumHex(originalPath)
	base := ImmichMirrorAsset{
		UpstreamAssetID:           assetID,
		MediaType:                 "image",
		Filename:                  "family.jpg",
		CapturedAt:                now,
		UpstreamChecksumAlgorithm: upstreamChecksumAlgorithmSHA1Path,
		ContentSHA1Hex:            pathChecksum,
	}
	if _, err := store.ReplaceFull(ctx, sourceKey, []ImmichMirrorAsset{base}, 0, now); err != nil {
		t.Fatalf("ReplaceFull(base) error = %v", err)
	}
	var generationBefore int64
	if err := store.db.QueryRowContext(ctx, `SELECT asset_generation FROM semantic_state
		WHERE source_key = ? AND model_id = 'test-model'`, sourceKey).Scan(&generationBefore); err != nil {
		t.Fatalf("read generation before enrichment: %v", err)
	}
	enriched := base
	enriched.CanonicalContentSHA1Hex = contentSHA1
	enriched.CanonicalContentSizeBytes = contentSize
	if _, err := store.ReplaceFull(ctx, sourceKey, []ImmichMirrorAsset{enriched}, 0, now.Add(time.Minute)); err != nil {
		t.Fatalf("ReplaceFull(enriched) error = %v", err)
	}
	var generationAfter int64
	if err := store.db.QueryRowContext(ctx, `SELECT asset_generation FROM semantic_state
		WHERE source_key = ? AND model_id = 'test-model'`, sourceKey).Scan(&generationAfter); err != nil {
		t.Fatalf("read generation after enrichment: %v", err)
	}
	if generationAfter != generationBefore {
		t.Fatalf("semantic generation after canonical enrichment = %d, want unchanged %d", generationAfter, generationBefore)
	}
}
