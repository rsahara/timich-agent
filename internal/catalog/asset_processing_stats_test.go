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

func TestRefreshAssetProcessingStatsDoesNotReuseSemanticCountsAcrossProfiles(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	service := newAssetProcessingStatsTestService(t)
	oldProfile := SemanticModelProfileStatus{
		ModelID:       "model-shared",
		VectorSpaceID: "model-shared/v1",
		EmbeddingDim:  4,
		ProfileKind:   semanticProfileKindModelPack,
		InputKind:     semanticInputKindImage,
	}
	newVectorSpace := oldProfile
	newVectorSpace.VectorSpaceID = "model-shared/v2"
	newModel := oldProfile
	newModel.ModelID = "model-new"
	newModel.VectorSpaceID = "model-new/v1"

	insertAssetProcessingStatsTestAsset(t, service, "asset-profile-switch", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "ready")
	insertAssetProcessingStatsTestRendition(t, service, "asset-profile-switch")
	insertAssetProcessingStatsTestFailedVector(t, service, "asset-profile-switch", oldProfile)

	oldSnapshot, err := service.RefreshAssetProcessingStats(ctx, &oldProfile, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats(old profile) error = %v", err)
	}
	assertAssetProcessingStat(t, oldSnapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusPending, 0, 1)
	assertAssetProcessingStat(t, oldSnapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusFailed, 1, 1)
	if status := SemanticBackfillStatusFromAssetProcessingStats(oldSnapshot, newVectorSpace); status != nil {
		t.Fatalf("SemanticBackfillStatusFromAssetProcessingStats(old snapshot, new vector space) = %+v, want nil", status)
	}

	vectorSpaceSnapshot, err := service.RefreshAssetProcessingStats(ctx, &newVectorSpace, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats(new vector space) error = %v", err)
	}
	assertAssetProcessingStat(t, vectorSpaceSnapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusPending, 1, 1)
	assertAssetProcessingStat(t, vectorSpaceSnapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusFailed, 0, 1)
	assertAssetProcessingSemanticVariant(t, vectorSpaceSnapshot, newVectorSpace)

	modelSnapshot, err := service.RefreshAssetProcessingStats(ctx, &newModel, time.Hour)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats(new model within min age) error = %v", err)
	}
	assertAssetProcessingStat(t, modelSnapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusPending, 1, 1)
	assertAssetProcessingStat(t, modelSnapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusFailed, 0, 1)
	assertAssetProcessingSemanticVariant(t, modelSnapshot, newModel)

	persisted, err := service.AssetProcessingStats(ctx)
	if err != nil {
		t.Fatalf("AssetProcessingStats() error = %v", err)
	}
	assertAssetProcessingSemanticVariant(t, persisted, newModel)
	if !persisted.MatchesSemanticProfile(&newModel) {
		t.Fatalf("persisted snapshot does not match new model: %+v", persisted)
	}
}

func TestRefreshAssetProcessingStatsKeepsFailedEmbeddingsOutOfPending(t *testing.T) {
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

	assets := []struct {
		id   string
		sha1 string
	}{
		{id: "asset-ready", sha1: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{id: "asset-failed", sha1: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{id: "asset-pending", sha1: "cccccccccccccccccccccccccccccccccccccccc"},
	}
	for _, asset := range assets {
		insertAssetProcessingStatsTestAsset(t, service, asset.id, asset.sha1, "ready")
		insertAssetProcessingStatsTestRendition(t, service, asset.id)
	}
	insertAssetProcessingStatsTestVector(t, service, "asset-ready", profile, false)
	now := formatCatalogTime(time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	insertSemanticVectorForTest(t,
		service.catalog,
		ctx,
		"1111111111111111",
		"asset-failed",
		profile.ModelID,
		profile.VectorSpaceID,
		profile.EmbeddingDim,
		[]float32{0, 1, 0, 0},
		"test",
		"failed",
		"embedding failed",
		now,
		nil,
	)

	snapshot, err := service.RefreshAssetProcessingStats(ctx, &profile, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats() error = %v", err)
	}
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusReady, 1, 3)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusPending, 1, 3)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusFailed, 1, 3)
}

func TestRefreshAssetProcessingStatsScopesFailedEmbeddingsToCurrentEligibleCorpus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	service := newAssetProcessingStatsTestService(t)
	profile := SemanticModelProfileStatus{
		ModelID:       "model-current",
		VectorSpaceID: "model-current/d4",
		EmbeddingDim:  4,
		ProfileKind:   semanticProfileKindModelPack,
		InputKind:     semanticInputKindImage,
	}
	oldProfile := profile
	oldProfile.ModelID = "model-old"
	oldProfile.VectorSpaceID = "model-old/d4"

	assets := []struct {
		id   string
		sha1 string
	}{
		{id: "asset-ready", sha1: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{id: "asset-failed", sha1: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{id: "asset-old-model-failed", sha1: "cccccccccccccccccccccccccccccccccccccccc"},
		{id: "asset-pending", sha1: "dddddddddddddddddddddddddddddddddddddddd"},
		{id: "asset-out-of-scope-failed", sha1: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},
	}
	for _, asset := range assets {
		insertAssetProcessingStatsTestAsset(t, service, asset.id, asset.sha1, "ready")
		insertAssetProcessingStatsTestRendition(t, service, asset.id)
	}
	insertAssetProcessingStatsTestVector(t, service, "asset-ready", profile, false)
	insertAssetProcessingStatsTestFailedVector(t, service, "asset-failed", profile)
	insertAssetProcessingStatsTestFailedVector(t, service, "asset-old-model-failed", oldProfile)
	insertAssetProcessingStatsTestFailedVector(t, service, "asset-out-of-scope-failed", profile)
	if _, err := service.catalog.db.ExecContext(ctx, `UPDATE catalog_assets
		SET visibility_status = 'missing'
		WHERE source_key = ? AND upstream_asset_id = ?`,
		"1111111111111111", "asset-out-of-scope-failed"); err != nil {
		t.Fatalf("mark failed asset out of scope: %v", err)
	}

	snapshot, err := service.RefreshAssetProcessingStats(ctx, &profile, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats() error = %v", err)
	}
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusReady, 1, 4)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusPending, 2, 4)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusFailed, 1, 4)

	status, err := service.SemanticModelBackfillStatus(ctx, profile)
	if err != nil {
		t.Fatalf("SemanticModelBackfillStatus() error = %v", err)
	}
	if status == nil || status.EligibleAssetCount != 4 || status.CompletedVectorCount != 1 || status.FailedVectorCount != 1 {
		t.Fatalf("SemanticModelBackfillStatus() = %+v, want current-profile eligible counts 4/1/1", status)
	}
}

func TestSemanticBackfillStatusFromAssetProcessingStatsKeepsPendingIndexJobsSeparate(t *testing.T) {
	t.Parallel()

	profile := SemanticModelProfileStatus{
		ModelID:       "model-a",
		VectorSpaceID: "model-a/d4",
		EmbeddingDim:  4,
		ProfileKind:   semanticProfileKindModelPack,
		InputKind:     semanticInputKindImage,
	}
	variant := assetProcessingSemanticVariant(&profile)
	snapshot := AssetProcessingStatsSnapshot{
		RefreshedAt: time.Now().UTC(),
		Stats: []AssetProcessingStat{
			{Stage: AssetProcessingStageEmbeddings, Variant: variant, Status: AssetProcessingStatusReady, Count: 1050, TotalCount: 1500},
			{Stage: AssetProcessingStageEmbeddings, Variant: variant, Status: AssetProcessingStatusPending, Count: 449, TotalCount: 1500},
			{Stage: AssetProcessingStageEmbeddings, Variant: variant, Status: AssetProcessingStatusFailed, Count: 1, TotalCount: 1500},
			{Stage: AssetProcessingStageSearchIndex, Variant: variant, Status: AssetProcessingStatusReady, Count: 1000, TotalCount: 1050},
			{Stage: AssetProcessingStageSearchIndex, Variant: variant, Status: AssetProcessingStatusPending, Count: 50, TotalCount: 1050},
		},
	}

	status := SemanticBackfillStatusFromAssetProcessingStats(snapshot, profile)
	if status == nil {
		t.Fatal("SemanticBackfillStatusFromAssetProcessingStats() = nil")
	}
	if status.PendingIndexJobCount != 0 {
		t.Fatalf("PendingIndexJobCount = %d, want 0 because search_index pending is unindexed vectors, not queued publish jobs", status.PendingIndexJobCount)
	}
	if status.CompletedVectorCount != 1050 || status.FailedVectorCount != 1 || status.IndexedVectorCount != 1000 {
		t.Fatalf("semantic counts = completed %d failed %d indexed %d, want 1050/1/1000", status.CompletedVectorCount, status.FailedVectorCount, status.IndexedVectorCount)
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

func insertAssetProcessingStatsTestFailedVector(t *testing.T, service *Service, assetID string, profile SemanticModelProfileStatus) {
	t.Helper()

	now := formatCatalogTime(time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	insertSemanticVectorForTest(t,
		service.catalog,
		context.Background(),
		"1111111111111111",
		assetID,
		profile.ModelID,
		profile.VectorSpaceID,
		profile.EmbeddingDim,
		[]float32{0, 1, 0, 0},
		"test",
		"failed",
		"embedding failed",
		now,
		nil,
	)
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

func assertAssetProcessingSemanticVariant(t *testing.T, snapshot AssetProcessingStatsSnapshot, profile SemanticModelProfileStatus) {
	t.Helper()

	want := assetProcessingSemanticVariant(&profile)
	for _, stat := range snapshot.Stats {
		switch stat.Stage {
		case AssetProcessingStageEmbeddings, AssetProcessingStageSearchIndex, AssetProcessingStageSearchable:
			if stat.Variant != want {
				t.Fatalf("%s/%s variant = %q, want %q; snapshot=%+v", stat.ScopeKey, stat.Stage, stat.Variant, want, snapshot)
			}
		}
	}
}
