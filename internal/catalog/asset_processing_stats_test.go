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
	prepareAssetProcessingStatsCanonicalInputs(t, service)
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
	prepareAssetProcessingStatsCanonicalInputs(t, service)
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

func TestRefreshAssetProcessingStatsExcludesRemovedSemanticSources(t *testing.T) {
	for _, removedIndexed := range []bool{false, true} {
		t.Run(map[bool]string{false: "removed-unprocessed", true: "removed-indexed"}[removedIndexed], func(t *testing.T) {
			ctx := context.Background()
			service, models, profile, sources, _ := canonicalBoundarySources(t)
			if removedIndexed {
				sources[1].URL = sources[0].URL
				if err := service.ReconfigureDatasources(sources); err != nil {
					t.Fatal(err)
				}
			} else if err := service.ReconfigureDatasources(sources[:1]); err != nil {
				t.Fatal(err)
			}
			result, err := service.BackfillSemanticModelCandidateWithOptions(ctx, models, profile, SemanticModelBackfillOptions{MaxAssets: 10, DrainIndexJobs: true})
			wantStored := 1
			if removedIndexed {
				wantStored = 2
			}
			if err != nil || result.IndexedVectorCount != wantStored {
				t.Fatalf("publish = %+v, %v, want %d indexed vectors", result, err, wantStored)
			}
			if err := service.ReconfigureDatasources(sources[:1]); err != nil {
				t.Fatal(err)
			}

			snapshot, err := service.RefreshAssetProcessingStats(ctx, &profile, 0)
			if err != nil {
				t.Fatalf("RefreshAssetProcessingStats() error = %v", err)
			}
			for _, stage := range []string{AssetProcessingStageEmbeddings, AssetProcessingStageSearchIndex} {
				assertAssetProcessingStat(t, snapshot, stage, AssetProcessingStatusReady, 1, 1)
				assertAssetProcessingStat(t, snapshot, stage, AssetProcessingStatusPending, 0, 1)
			}
			assertAssetProcessingScopedStat(t, snapshot, sources[0].SourceKey, AssetProcessingStageSearchable, AssetProcessingStatusReady, 1, 1)
			if snapshot.HasStageForScope(sources[1].SourceKey, AssetProcessingStageSearchable) {
				t.Fatal("removed datasource remains in searchable coverage")
			}
			persisted, err := service.AssetProcessingStats(ctx)
			if err != nil {
				t.Fatalf("AssetProcessingStats() error = %v", err)
			}
			status := SemanticBackfillStatusFromAssetProcessingStats(persisted, profile)
			if status == nil || status.Status != SemanticBackfillStatusReady || status.EligibleAssetCount != 1 || status.CompletedVectorCount != 1 || status.IndexedVectorCount != 1 || status.RemainingVectorCount != 0 {
				t.Fatalf("persisted progress includes removed source: %+v", status)
			}
			var storedIndexed int
			if err := service.catalog.db.QueryRowContext(ctx, `SELECT indexed_vector_count FROM semantic_state WHERE source_key = ? AND model_id = ?`, canonicalSemanticCorpusSourceKey, profile.ModelID).Scan(&storedIndexed); err != nil {
				t.Fatal(err)
			}
			if storedIndexed != wantStored {
				t.Fatalf("durable corpus count = %d, want %d", storedIndexed, wantStored)
			}
		})
	}
}

func TestRefreshAssetProcessingStatsUsesCanonicalCorpusBeforeCanonicalStateExists(t *testing.T) {
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

	insertAssetProcessingStatsTestAsset(t, service, "asset-migrated", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "ready")
	insertAssetProcessingStatsTestRendition(t, service, "asset-migrated")
	if _, err := service.catalog.db.ExecContext(ctx, `UPDATE local_renditions
		SET source_sha1_hex = (
			SELECT local_asset.sha1_hex
			FROM local_assets local_asset
			WHERE local_asset.source_key = local_renditions.source_key
				AND local_asset.asset_id = local_renditions.asset_id
		)
		WHERE source_key = ?`, "1111111111111111"); err != nil {
		t.Fatalf("make Local rendition canonical-semantic eligible: %v", err)
	}
	if _, err := service.catalog.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}

	// An existing source-scoped vector and published state remain a search
	// fallback until canonical publication. Neither belongs to the canonical corpus.
	insertAssetProcessingStatsTestLegacyVector(t, service, "asset-migrated", profile, true)
	if _, err := service.catalog.db.ExecContext(ctx, `DELETE FROM semantic_state WHERE source_key = ?`, canonicalSemanticCorpusSourceKey); err != nil {
		t.Fatalf("remove canonical semantic state: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `DELETE FROM semantic_vectors WHERE source_key = ?`, canonicalSemanticCorpusSourceKey); err != nil {
		t.Fatalf("remove canonical semantic vectors: %v", err)
	}

	snapshot, err := service.RefreshAssetProcessingStats(ctx, &profile, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats() error = %v", err)
	}
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusPending, 1, 1)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusReady, 0, 1)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageSearchIndex, AssetProcessingStatusPending, 0, 0)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageSearchIndex, AssetProcessingStatusReady, 0, 0)
	assertAssetProcessingScopedStat(t, snapshot, "1111111111111111", AssetProcessingStageSearchable, AssetProcessingStatusReady, 1, 1)

	now := formatCatalogTime(time.Date(2026, 7, 4, 12, 5, 0, 0, time.UTC))
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			built_at, last_error, updated_at
		) VALUES (?, ?, ?, 'backfilling', ?, 0, 0, 0, -1, NULL, NULL, ?)`,
		canonicalSemanticCorpusSourceKey,
		profile.ModelID,
		profile.VectorSpaceID,
		profile.EmbeddingDim,
		now,
	); err != nil {
		t.Fatalf("insert unpublished canonical semantic state: %v", err)
	}
	statsDB, err := service.catalog.openStatsWriteDB(ctx)
	if err != nil {
		t.Fatalf("open stats db: %v", err)
	}
	if _, err := statsDB.ExecContext(ctx, `UPDATE asset_processing_stats SET refreshed_at = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
		_ = statsDB.Close()
		t.Fatalf("stale semantic stats: %v", err)
	}
	if err := statsDB.Close(); err != nil {
		t.Fatalf("close stats db: %v", err)
	}

	unpublished, err := service.RefreshAssetProcessingStats(ctx, &profile, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats(unpublished canonical state) error = %v", err)
	}
	assertAssetProcessingStat(t, unpublished, AssetProcessingStageEmbeddings, AssetProcessingStatusPending, 1, 1)
	assertAssetProcessingStat(t, unpublished, AssetProcessingStageSearchIndex, AssetProcessingStatusReady, 0, 0)
	assertAssetProcessingScopedStat(t, unpublished, "1111111111111111", AssetProcessingStageSearchable, AssetProcessingStatusReady, 1, 1)
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
	prepareAssetProcessingStatsCanonicalInputs(t, service)
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
	prepareAssetProcessingStatsCanonicalInputs(t, service)
	insertAssetProcessingStatsTestVector(t, service, "asset-ready", profile, false)
	insertAssetProcessingStatsTestFailedVector(t, service, "asset-failed", profile)

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
	if _, err := service.catalog.db.ExecContext(ctx, `UPDATE catalog_assets
		SET visibility_status = 'missing'
		WHERE source_key = ? AND upstream_asset_id = ?`,
		"1111111111111111", "asset-out-of-scope-failed"); err != nil {
		t.Fatalf("mark failed asset out of scope: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `UPDATE local_renditions
		SET source_sha1_hex = (
			SELECT local_asset.sha1_hex
			FROM local_assets local_asset
			WHERE local_asset.source_key = local_renditions.source_key
				AND local_asset.asset_id = local_renditions.asset_id
		)
		WHERE source_key = ?`, "1111111111111111"); err != nil {
		t.Fatalf("make Local renditions canonical-semantic eligible: %v", err)
	}
	if _, err := service.catalog.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}
	canonicalAssetID := func(assetID string) string {
		t.Helper()
		var canonicalAssetID string
		if err := service.catalog.db.QueryRowContext(ctx, `SELECT canonical_asset_id
			FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`,
			"1111111111111111", assetID).Scan(&canonicalAssetID); err != nil {
			t.Fatalf("read canonical asset ID for %q: %v", assetID, err)
		}
		return canonicalAssetID
	}
	now := formatCatalogTime(time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	insertCanonicalSemanticVectorForTest(
		t,
		service.catalog,
		ctx,
		canonicalAssetID("asset-ready"),
		"1111111111111111",
		"asset-ready",
		profile.ModelID,
		profile.VectorSpaceID,
		profile.EmbeddingDim,
		[]float32{1, 0, 0, 0},
		"local_preview",
		now,
	)
	insertFailedVector := func(assetID string, failedProfile SemanticModelProfileStatus) {
		t.Helper()
		insertSemanticVectorForTest(
			t,
			service.catalog,
			ctx,
			canonicalSemanticCorpusSourceKey,
			canonicalAssetID(assetID),
			failedProfile.ModelID,
			failedProfile.VectorSpaceID,
			failedProfile.EmbeddingDim,
			[]float32{0, 1, 0, 0},
			"local_preview",
			"failed",
			"embedding failed",
			now,
			nil,
		)
	}
	insertFailedVector("asset-failed", profile)
	insertFailedVector("asset-old-model-failed", oldProfile)
	insertFailedVector("asset-out-of-scope-failed", profile)
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			built_at, last_error, updated_at
		) VALUES (?, ?, ?, 'backfilling', ?, 1, 0, 0, -1, NULL, NULL, ?)`,
		canonicalSemanticCorpusSourceKey,
		profile.ModelID,
		profile.VectorSpaceID,
		profile.EmbeddingDim,
		now,
	); err != nil {
		t.Fatalf("insert canonical semantic state: %v", err)
	}

	snapshot, err := service.RefreshAssetProcessingStats(ctx, &profile, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats() error = %v", err)
	}
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusReady, 1, 4)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusPending, 2, 4)
	assertAssetProcessingStat(t, snapshot, AssetProcessingStageEmbeddings, AssetProcessingStatusFailed, 1, 4)
	// Issues includes the missing source asset plus the active current-profile canonical failure.
	assertAssetProcessingScopedStat(t, snapshot, "1111111111111111", AssetProcessingStageIssues, AssetProcessingStatusReady, 2, 5)

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

func prepareAssetProcessingStatsCanonicalInputs(t *testing.T, service *Service) {
	t.Helper()

	ctx := context.Background()
	if _, err := service.catalog.db.ExecContext(ctx, `UPDATE local_renditions
		SET source_sha1_hex = (
			SELECT local_asset.sha1_hex
			FROM local_assets local_asset
			WHERE local_asset.source_key = local_renditions.source_key
				AND local_asset.asset_id = local_renditions.asset_id
		)
		WHERE source_key = ?`, "1111111111111111"); err != nil {
		t.Fatalf("make Local renditions canonical-semantic eligible: %v", err)
	}
	if _, err := service.catalog.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `UPDATE canonical_semantic_vector_inputs SET refresh_required = 0`); err != nil {
		t.Fatalf("keep unchanged canonical test inputs current: %v", err)
	}
}

func assetProcessingStatsTestCanonicalAssetID(t *testing.T, service *Service, assetID string) string {
	t.Helper()

	var canonicalAssetID string
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT canonical_asset_id
		FROM catalog_assets WHERE source_key = ? AND upstream_asset_id = ?`,
		"1111111111111111", assetID).Scan(&canonicalAssetID); err != nil {
		t.Fatalf("read canonical asset ID for %q: %v", assetID, err)
	}
	return canonicalAssetID
}

func insertAssetProcessingStatsTestLegacyVector(t *testing.T, service *Service, assetID string, profile SemanticModelProfileStatus, indexed bool) {
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

func insertAssetProcessingStatsTestVector(t *testing.T, service *Service, assetID string, profile SemanticModelProfileStatus, indexed bool) {
	t.Helper()

	ctx := context.Background()
	now := formatCatalogTime(time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	canonicalAssetID := assetProcessingStatsTestCanonicalAssetID(t, service, assetID)
	insertCanonicalSemanticVectorForTest(
		t,
		service.catalog,
		ctx,
		canonicalAssetID,
		"1111111111111111",
		assetID,
		profile.ModelID,
		profile.VectorSpaceID,
		profile.EmbeddingDim,
		[]float32{1, 0, 0, 0},
		"local_preview",
		now,
	)
	if !indexed {
		return
	}
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_index_membership_state (
			source_key, model_id, vector_space_id, asset_generation,
			binary_sha256, binary_size_bytes, node_count, built_at
		) VALUES (?, ?, ?, 0, ?, 1, 1, ?)
		ON CONFLICT(source_key, model_id, vector_space_id, asset_generation) DO UPDATE SET
			node_count = node_count + 1,
			built_at = excluded.built_at`,
		canonicalSemanticCorpusSourceKey,
		profile.ModelID,
		profile.VectorSpaceID,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		now,
	); err != nil {
		t.Fatalf("insert canonical semantic membership state: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_index_membership (
			source_key, model_id, vector_space_id, asset_generation, upstream_asset_id, ordinal
		) VALUES (?, ?, ?, 0, ?, (
			SELECT COUNT(*) FROM semantic_index_membership
			WHERE source_key = ? AND model_id = ? AND vector_space_id = ? AND asset_generation = 0
		))`,
		canonicalSemanticCorpusSourceKey,
		profile.ModelID,
		profile.VectorSpaceID,
		canonicalAssetID,
		canonicalSemanticCorpusSourceKey,
		profile.ModelID,
		profile.VectorSpaceID,
	); err != nil {
		t.Fatalf("insert canonical semantic membership: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			built_at, last_error, updated_at
		) VALUES (?, ?, ?, 'ready', ?, 1, 1, 0, 0, ?, NULL, ?)
		ON CONFLICT(source_key, model_id) DO UPDATE SET
			status = 'ready',
			completed_vector_count = completed_vector_count + 1,
			indexed_vector_count = indexed_vector_count + 1,
			asset_generation = 0,
			indexed_generation = 0,
			built_at = excluded.built_at,
			updated_at = excluded.updated_at`,
		canonicalSemanticCorpusSourceKey,
		profile.ModelID,
		profile.VectorSpaceID,
		profile.EmbeddingDim,
		now,
		now,
	); err != nil {
		t.Fatalf("mark canonical semantic snapshot published: %v", err)
	}
}

func insertAssetProcessingStatsTestFailedVector(t *testing.T, service *Service, assetID string, profile SemanticModelProfileStatus) {
	t.Helper()

	now := formatCatalogTime(time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	insertSemanticVectorForTest(t,
		service.catalog,
		context.Background(),
		canonicalSemanticCorpusSourceKey,
		assetProcessingStatsTestCanonicalAssetID(t, service, assetID),
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
