package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestRefreshAssetProcessingStatsRecountsLocalStages(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	service := newAssetProcessingStatsTestService(t)

	insertAssetProcessingStatsTestAsset(t, service, "asset-thumb-ready", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "ready")
	insertAssetProcessingStatsTestAsset(t, service, "asset-thumb-pending-a", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "pending")
	insertAssetProcessingStatsTestAsset(t, service, "asset-thumb-pending-b", "cccccccccccccccccccccccccccccccccccccccc", "pending")
	insertAssetProcessingStatsTestAsset(t, service, "asset-thumb-failed", "dddddddddddddddddddddddddddddddddddddddd", "failed")
	insertAssetProcessingStatsTestJob(t, service, localMetadataJobKind, "queued")
	insertAssetProcessingStatsTestJob(t, service, localMetadataJobKind, "running")
	insertAssetProcessingStatsTestJob(t, service, localMetadataJobKind, "failed")
	insertAssetProcessingStatsTestJob(t, service, localThumbnailJobKind, "running")

	snapshot, err := service.RefreshAssetProcessingStats(ctx, nil, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats() error = %v", err)
	}
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageMetadata, AssetProcessingStatusPending, 1, 7)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageMetadata, AssetProcessingStatusRunning, 1, 7)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageMetadata, AssetProcessingStatusReady, 4, 7)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageMetadata, AssetProcessingStatusFailed, 1, 7)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageThumbnails, AssetProcessingStatusPending, 1, 4)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageThumbnails, AssetProcessingStatusRunning, 1, 4)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageThumbnails, AssetProcessingStatusReady, 1, 4)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageThumbnails, AssetProcessingStatusFailed, 1, 4)
	assertAssetProcessingScopedStat(t, snapshot, "1111111111111111", AssetProcessingStageFoundMedias, AssetProcessingStatusReady, 4, 4)
	assertAssetProcessingScopedStat(t, snapshot, "1111111111111111", AssetProcessingStageBrowsable, AssetProcessingStatusReady, 1, 4)
	assertAssetProcessingScopedStat(t, snapshot, "1111111111111111", AssetProcessingStageSearchable, AssetProcessingStatusUnavailable, 0, 1)
	assertAssetProcessingScopedStat(t, snapshot, "1111111111111111", AssetProcessingStageIssues, AssetProcessingStatusReady, 2, 4)

	persisted, err := service.AssetProcessingStats(ctx)
	if err != nil {
		t.Fatalf("AssetProcessingStats() error = %v", err)
	}
	assertAssetProcessingStat(t, persisted, AssetProcessingStageThumbnails, AssetProcessingStatusPending, 1, 4)

	if _, err := service.catalog.db.ExecContext(ctx, `UPDATE local_assets
		SET thumbnail_status = 'ready'
		WHERE asset_id = 'asset-thumb-pending-a'`); err != nil {
		t.Fatalf("update thumbnail status: %v", err)
	}
	unchanged, err := service.RefreshAssetProcessingStats(ctx, nil, time.Hour)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats(minAge) error = %v", err)
	}
	assertAssetProcessingStat(t, unchanged, AssetProcessingStageThumbnails, AssetProcessingStatusPending, 1, 4)

	updated, err := service.RefreshAssetProcessingStats(ctx, nil, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats(force) error = %v", err)
	}
	assertAssetProcessingStat(t, updated, AssetProcessingStageThumbnails, AssetProcessingStatusPending, 0, 4)
	assertAssetProcessingStat(t, updated, AssetProcessingStageThumbnails, AssetProcessingStatusReady, 2, 4)
}

func TestRefreshAssetProcessingStatsReusesSemanticCountsForShortInterval(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	service := newAssetProcessingStatsTestService(t)
	profile := SemanticModelProfileStatus{
		ModelID:       "model-a",
		VectorSpaceID: "model-a/d4",
		EmbeddingDim:  4,
		ProfileKind:   semanticProfileKindModelPack,
		InputKind:     semanticInputKindImage,
	}

	insertAssetProcessingStatsTestAsset(t, service, "asset-semantic-a", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "ready")
	insertAssetProcessingStatsTestRendition(t, service, "asset-semantic-a")
	insertAssetProcessingStatsTestVector(t, service, "asset-semantic-a", profile, true)

	first, err := service.RefreshAssetProcessingStats(ctx, &profile, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats() error = %v", err)
	}
	assertAssetProcessingStat(t, first, AssetProcessingStageEmbeddings, AssetProcessingStatusReady, 1, 1)
	assertAssetProcessingStat(t, first, AssetProcessingStageSearchIndex, AssetProcessingStatusReady, 1, 1)
	assertAssetProcessingScopedStat(t, first, "1111111111111111", AssetProcessingStageSearchable, AssetProcessingStatusReady, 1, 1)

	insertAssetProcessingStatsTestAsset(t, service, "asset-semantic-b", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "ready")
	insertAssetProcessingStatsTestRendition(t, service, "asset-semantic-b")
	insertAssetProcessingStatsTestVector(t, service, "asset-semantic-b", profile, true)

	reused, err := service.RefreshAssetProcessingStats(ctx, &profile, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats(reuse) error = %v", err)
	}
	assertAssetProcessingScopedStat(t, reused, "1111111111111111", AssetProcessingStageBrowsable, AssetProcessingStatusReady, 2, 2)
	assertAssetProcessingScopedStat(t, reused, "1111111111111111", AssetProcessingStageSearchable, AssetProcessingStatusReady, 1, 2)
	assertAssetProcessingStat(t, reused, AssetProcessingStageSearchIndex, AssetProcessingStatusReady, 1, 1)

	statsDB, err := service.catalog.openStatsWriteDB(ctx)
	if err != nil {
		t.Fatalf("open stats db: %v", err)
	}
	defer statsDB.Close()
	stale := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if _, err := statsDB.ExecContext(ctx, `UPDATE asset_processing_stats
		SET refreshed_at = ?
		WHERE stage IN (?, ?, ?)`,
		stale,
		AssetProcessingStageEmbeddings,
		AssetProcessingStageSearchIndex,
		AssetProcessingStageSearchable,
	); err != nil {
		t.Fatalf("stale semantic stats: %v", err)
	}
	recounted, err := service.RefreshAssetProcessingStats(ctx, &profile, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats(recount) error = %v", err)
	}
	assertAssetProcessingStat(t, recounted, AssetProcessingStageEmbeddings, AssetProcessingStatusReady, 2, 2)
	assertAssetProcessingStat(t, recounted, AssetProcessingStageSearchIndex, AssetProcessingStatusReady, 2, 2)
	assertAssetProcessingScopedStat(t, recounted, "1111111111111111", AssetProcessingStageSearchable, AssetProcessingStatusReady, 2, 2)
}

func TestSemanticBackfillStatusFromAssetProcessingStatsKeepsPendingIndexJobsSeparate(t *testing.T) {
	t.Parallel()

	snapshot := AssetProcessingStatsSnapshot{
		RefreshedAt: time.Now().UTC(),
		Stats: []AssetProcessingStat{
			{Stage: AssetProcessingStageEmbeddings, Status: AssetProcessingStatusReady, Count: 1050, TotalCount: 1500},
			{Stage: AssetProcessingStageEmbeddings, Status: AssetProcessingStatusPending, Count: 450, TotalCount: 1500},
			{Stage: AssetProcessingStageSearchIndex, Status: AssetProcessingStatusReady, Count: 1000, TotalCount: 1050},
			{Stage: AssetProcessingStageSearchIndex, Status: AssetProcessingStatusPending, Count: 50, TotalCount: 1050},
		},
	}
	profile := SemanticModelProfileStatus{
		ModelID:       "model-a",
		VectorSpaceID: "model-a/d4",
		EmbeddingDim:  4,
		ProfileKind:   semanticProfileKindModelPack,
		InputKind:     semanticInputKindImage,
	}

	status := SemanticBackfillStatusFromAssetProcessingStats(snapshot, profile)
	if status == nil {
		t.Fatal("SemanticBackfillStatusFromAssetProcessingStats() = nil")
	}
	if status.PendingIndexJobCount != 0 {
		t.Fatalf("PendingIndexJobCount = %d, want 0 because search_index pending is unindexed vectors, not queued publish jobs", status.PendingIndexJobCount)
	}
	if status.CompletedVectorCount != 1050 || status.IndexedVectorCount != 1000 {
		t.Fatalf("semantic counts = completed %d indexed %d, want 1050/1000", status.CompletedVectorCount, status.IndexedVectorCount)
	}
}

func newAssetProcessingStatsTestService(t *testing.T) *Service {
	t.Helper()

	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
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
	now := formatCatalogTime(time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_scan_root_state (
			source_key, root_key, root_status, phase0_status, root_generation, reconciliation_pending, updated_at
		) VALUES (?, ?, 'ready', 'completed', 1, 0, ?)`,
		"1111111111111111",
		"nas-photos",
		now,
	); err != nil {
		t.Fatalf("insert local root state: %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return service
}

func insertAssetProcessingStatsTestAsset(t *testing.T, service *Service, assetID string, sha1Hex string, thumbnailStatus string) {
	t.Helper()

	now := formatCatalogTime(time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_assets (
			source_key, asset_id, sha1_hex, content_size_bytes, media_type, filename, captured_at,
			captured_at_source, visibility_status, thumbnail_status, first_seen_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"1111111111111111",
		assetID,
		sha1Hex,
		12345,
		"image",
		assetID+".jpg",
		now,
		"exif",
		"active",
		thumbnailStatus,
		now,
		now,
	); err != nil {
		t.Fatalf("insert local asset %s: %v", assetID, err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename, captured_at,
			visibility_status, content_sha1_hex, content_size_bytes, first_seen_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"1111111111111111",
		config.DatasourceKindLocalFiles,
		assetID,
		"image",
		assetID+".jpg",
		now,
		"active",
		sha1Hex,
		12345,
		now,
		now,
	); err != nil {
		t.Fatalf("insert catalog asset %s: %v", assetID, err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_asset_locations (
			source_key, asset_id, root_key, relative_path, size_bytes, mtime, fast_signature,
			sha1_hex, status, first_seen_at, last_seen_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"1111111111111111",
		assetID,
		"nas-photos",
		assetID+".jpg",
		12345,
		now,
		assetID+"-sig",
		sha1Hex,
		"active",
		now,
		now,
		now,
	); err != nil {
		t.Fatalf("insert local asset location %s: %v", assetID, err)
	}
}

func insertAssetProcessingStatsTestRendition(t *testing.T, service *Service, assetID string) {
	t.Helper()

	now := formatCatalogTime(time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_renditions (
			source_key, asset_id, kind, status, relative_path, generated_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		"1111111111111111",
		assetID,
		"preview",
		"ready",
		assetID+".jpg",
		now,
	); err != nil {
		t.Fatalf("insert local rendition %s: %v", assetID, err)
	}
}

func insertAssetProcessingStatsTestVector(t *testing.T, service *Service, assetID string, profile SemanticModelProfileStatus, indexed bool) {
	t.Helper()

	now := formatCatalogTime(time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	var indexedAt any
	if indexed {
		indexedAt = now
	}
	insertSemanticVectorForTest(t,
		service.catalog,
		context.Background(),
		"1111111111111111",
		assetID,
		profile.ModelID,
		profile.VectorSpaceID,
		profile.EmbeddingDim,
		[]float32{1, 0, 0, 0},
		"test",
		"ready",
		nil,
		now,
		indexedAt,
	)
	if indexed {
		if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO semantic_state (
				source_key, model_id, vector_space_id, status, embedding_dim,
				completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
				built_at, last_error, updated_at
			) VALUES (?, ?, ?, 'ready', ?, 1, 1, 0, 0, ?, NULL, ?)
			ON CONFLICT(source_key, model_id) DO UPDATE SET
				status = 'ready',
				completed_vector_count = (
				SELECT COUNT(*) FROM semantic_vectors
				WHERE source_key = semantic_state.source_key
					AND model_id = semantic_state.model_id
					AND vector_space_id = semantic_state.vector_space_id
					AND embedding_dim = semantic_state.embedding_dim
					AND status = 'ready'
				),
				indexed_vector_count = (
				SELECT COUNT(*) FROM semantic_vectors
				WHERE source_key = semantic_state.source_key
					AND model_id = semantic_state.model_id
					AND vector_space_id = semantic_state.vector_space_id
					AND embedding_dim = semantic_state.embedding_dim
					AND status = 'ready'
				),
				indexed_generation = semantic_state.asset_generation,
				built_at = excluded.built_at,
				updated_at = excluded.updated_at`,
			"1111111111111111", profile.ModelID, profile.VectorSpaceID, profile.EmbeddingDim, now, now); err != nil {
			t.Fatalf("mark semantic snapshot published: %v", err)
		}
	}
}

func insertAssetProcessingStatsTestJob(t *testing.T, service *Service, kind string, status string) {
	t.Helper()

	now := formatCatalogTime(time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, status, scheduled_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"1111111111111111",
		kind,
		0,
		"nas-photos",
		1,
		status,
		now,
	); err != nil {
		t.Fatalf("insert local scan job %s/%s: %v", kind, status, err)
	}
}

func assertAssetProcessingStat(t *testing.T, snapshot AssetProcessingStatsSnapshot, stage string, status string, count int, total int) {
	t.Helper()

	if got := snapshot.Count(stage, status); got != count {
		t.Fatalf("%s/%s count = %d, want %d; snapshot=%+v", stage, status, got, count, snapshot)
	}
	if got := snapshot.Total(stage); got != total {
		t.Fatalf("%s total = %d, want %d; snapshot=%+v", stage, got, total, snapshot)
	}
}

func assertAssetProcessingScopedStat(t *testing.T, snapshot AssetProcessingStatsSnapshot, scopeKey string, stage string, status string, count int, total int) {
	t.Helper()

	if got := snapshot.CountForScope(scopeKey, stage, status); got != count {
		t.Fatalf("%s/%s/%s count = %d, want %d; snapshot=%+v", scopeKey, stage, status, got, count, snapshot)
	}
	if got := snapshot.TotalForScope(scopeKey, stage); got != total {
		t.Fatalf("%s/%s total = %d, want %d; snapshot=%+v", scopeKey, stage, got, total, snapshot)
	}
}
