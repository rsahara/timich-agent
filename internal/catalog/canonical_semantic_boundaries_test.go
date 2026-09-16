package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func canonicalBoundarySources(t *testing.T) (*Service, *SemanticModelPackStore, SemanticModelProfileStatus, []config.DatasourceConfig, *atomic.Int32) {
	t.Helper()
	var unavailableCalls atomic.Int32
	image := encodeJPEGForTest(t, 32, 32)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(image)
	}))
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unavailableCalls.Add(1)
		http.Error(w, "source unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(good.Close)
	t.Cleanup(bad.Close)
	dataDir := t.TempDir()
	models, _ := installSemanticRuntimeStatusTestModel(t, dataDir)
	candidate, ok := models.InstalledCandidateProfile()
	if !ok {
		t.Fatal("missing installed candidate")
	}
	sources := []config.DatasourceConfig{
		{SourceKey: "1111111111111111", Kind: config.DatasourceKindImmichIndexed, Name: "Available", URL: good.URL, AccessToken: "test"},
		{SourceKey: "2222222222222222", Kind: config.DatasourceKindImmichIndexed, Name: "Unavailable", URL: bad.URL, AccessToken: "test"},
	}
	service, err := NewServiceWithOptions(sources, ServiceOptions{DataDir: dataDir, SemanticModels: models})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	now := time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC)
	for i, source := range sources {
		_, err := service.catalog.ReplaceFull(context.Background(), source.SourceKey, []ImmichMirrorAsset{{
			UpstreamAssetID: "asset", MediaType: "image", Filename: source.Name + ".jpg", CapturedAt: now.Add(-time.Duration(i) * time.Hour),
		}}, 0, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	return service, models, candidate, sources, &unavailableCalls
}

func TestCanonicalBackfillIsolatesUnavailableDatasource(t *testing.T) {
	for _, tc := range []struct {
		name             string
		workers          int
		unavailableFirst bool
	}{{"serial/healthy-first", 1, false}, {"serial/unavailable-first", 1, true}, {"parallel/healthy-first", 2, false}, {"parallel/unavailable-first", 2, true}} {
		t.Run(tc.name, func(t *testing.T) {
			workers := tc.workers
			service, models, candidate, sources, badCalls := canonicalBoundarySources(t)
			ctx := context.Background()
			if tc.unavailableFirst {
				if err := service.rememberSemanticBackfillSource(ctx, candidate, sources[1].SourceKey); err != nil {
					t.Fatal(err)
				}
			}
			result, err := service.BackfillSemanticModelCandidateWithOptions(ctx, models, candidate, SemanticModelBackfillOptions{MaxAssets: 10, Workers: workers})
			if err != nil || result.ProcessedVectorCount != 1 {
				t.Fatalf("mixed-source backfill = %+v, %v, want healthy source saved", result, err)
			}
			var ready int
			if err := service.catalog.db.QueryRow(`SELECT count(*) FROM semantic_vectors WHERE source_key = ? AND status = 'ready'`, canonicalSemanticCorpusSourceKey).Scan(&ready); err != nil || ready != 1 {
				t.Fatalf("persisted healthy vectors = %d, %v", ready, err)
			}
			if _, deferred := service.semanticSourceRetryDeadline(sources[1].SourceKey, time.Now()); !deferred {
				t.Fatal("failed datasource has no retry deadline")
			}
			if _, deferred := service.semanticSourceRetryDeadline(sources[0].SourceKey, time.Now()); deferred {
				t.Fatal("healthy datasource was deferred")
			}
			if _, deferred := service.semanticSourceRetryDeadline(canonicalSemanticCorpusSourceKey, time.Now()); deferred {
				t.Fatal("shared corpus was deferred")
			}
			if result.Status.EligibleAssetCount != 2 || result.Status.EligibleNowVectorCount != 0 || result.Status.NextEligibleAt == nil {
				t.Fatalf("source cooldown progress = %+v", result.Status)
			}
			if len(result.SourceStatuses) != 1 || result.SourceStatuses[0].SourceKey != canonicalSemanticCorpusSourceKey || result.SourceStatuses[0].Status.CompletedVectorCount != 1 {
				t.Fatalf("canonical progress notification = %+v", result.SourceStatuses)
			}
			calls := badCalls.Load()
			now := time.Now().UTC()
			_, err = service.catalog.ReplaceFull(ctx, sources[0].SourceKey, []ImmichMirrorAsset{
				{UpstreamAssetID: "asset", MediaType: "image", Filename: "Available.jpg", CapturedAt: time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC)},
				{UpstreamAssetID: "next", MediaType: "image", Filename: "next.jpg", CapturedAt: now},
			}, 0, now)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := service.SemanticModelBackfillSnapshot(ctx, candidate)
			if err != nil || snapshot == nil || snapshot.Status.EligibleNowVectorCount != 1 {
				t.Fatalf("healthy source readiness during cooldown = %+v, %v", snapshot, err)
			}
			result, err = service.BackfillSemanticModelCandidateWithOptions(ctx, models, candidate, SemanticModelBackfillOptions{MaxAssets: 10, Workers: workers})
			if err != nil || result.ProcessedVectorCount != 1 || badCalls.Load() != calls {
				t.Fatalf("backfill during source cooldown = %+v, %v; unavailable calls %d -> %d", result, err, calls, badCalls.Load())
			}
		})
	}
}

func TestCanonicalBackfillExcludesRemovedDatasource(t *testing.T) {
	service, models, candidate, sources, badCalls := canonicalBoundarySources(t)
	service.ReconfigureDatasources(sources[:1])
	result, err := service.BackfillSemanticModelCandidateWithOptions(context.Background(), models, candidate, SemanticModelBackfillOptions{MaxAssets: 10, DrainIndexJobs: true})
	if err != nil || result.ProcessedVectorCount != 1 || result.IndexedVectorCount != 1 || badCalls.Load() != 0 {
		t.Fatalf("backfill after source removal = %+v, %v, removed requests=%d", result, err, badCalls.Load())
	}
	snapshot, err := service.SemanticModelBackfillSnapshot(context.Background(), candidate)
	if err != nil || snapshot == nil || snapshot.Status.EligibleAssetCount != 1 || snapshot.Status.RemainingVectorCount != 0 {
		t.Fatalf("configured-source progress = %+v, %v", snapshot, err)
	}
}

func canonicalFailedInputFixture(t *testing.T) (*Service, semanticEmbeddingProfile, SemanticModelProfileStatus) {
	t.Helper()
	source := config.DatasourceConfig{SourceKey: "1111111111111111", Kind: config.DatasourceKindImmichIndexed, URL: "http://immich.test", AccessToken: "test"}
	service, err := NewServiceWithOptions([]config.DatasourceConfig{source}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	profile := testImageSemanticProfile{}
	status := SemanticModelProfileStatus{ModelID: profile.ModelID(), VectorSpaceID: profile.VectorSpaceID(), EmbeddingDim: profile.EmbeddingDim(), InputKind: profile.InputKind()}
	now := time.Now().UTC()
	_, err = service.catalog.ReplaceFull(context.Background(), source.SourceKey, []ImmichMirrorAsset{{UpstreamAssetID: "broken", Filename: "broken.jpg", MediaType: "image", CapturedAt: now}}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.catalog.BackfillSemanticVectors(context.Background(), canonicalSemanticCorpusSourceKey, profile, now, SemanticBackfillOptions{
		ImageLoader: failingSemanticImageLoader{err: errors.Join(ErrSemanticAssetInput, errors.New("invalid image"))}, MaxAssets: 1,
	})
	if err != nil || result.ProcessedVectorCount != 1 {
		t.Fatalf("first failure = %+v, %v", result, err)
	}
	return service, profile, status
}

func TestCanonicalFailedInputHonorsRetryDeadline(t *testing.T) {
	service, profile, status := canonicalFailedInputFixture(t)
	ctx := context.Background()
	var recorded int
	if err := service.catalog.db.QueryRow(`SELECT count(*) FROM canonical_semantic_vector_inputs WHERE refresh_required = 0 AND input_fingerprint <> ''`).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("recorded failed input = %d, %v", recorded, err)
	}
	for _, legacy := range []bool{false, true} {
		if legacy {
			if _, err := service.catalog.db.Exec(`DELETE FROM canonical_semantic_vector_inputs`); err != nil {
				t.Fatal(err)
			}
		}
		assets, err := service.catalog.loadCanonicalSemanticBackfillAssets(ctx, profile, 10, nil, "")
		if err != nil || len(assets) != 0 {
			t.Fatalf("failed input selected before deadline (missing ledger=%t): %+v, %v", legacy, assets, err)
		}
		counts, err := service.catalog.canonicalSemanticCounts(ctx, status, time.Now().UTC(), nil)
		if err != nil || counts.failed != 1 || counts.eligibleNow != 0 || counts.nextEligibleAt == nil {
			t.Fatalf("failure counts = %+v, %v", counts, err)
		}
	}
	expired := time.Now().UTC().Add(-31 * time.Minute)
	if _, err := service.catalog.db.Exec(`UPDATE semantic_vectors SET generated_at = ?`, formatCatalogTime(expired)); err != nil {
		t.Fatal(err)
	}
	assets, err := service.catalog.loadCanonicalSemanticBackfillAssets(ctx, profile, 10, nil, "")
	if err != nil || len(assets) != 1 {
		t.Fatalf("expired failure selection = %+v, %v", assets, err)
	}
}

func TestCanonicalFailureDiagnosticsAndManualRetry(t *testing.T) {
	service, profile, status := canonicalFailedInputFixture(t)
	ctx := context.Background()
	// Include already-stored failures from before an input ledger existed.
	if _, err := service.catalog.db.Exec(`DELETE FROM canonical_semantic_vector_inputs`); err != nil {
		t.Fatal(err)
	}
	rows, err := collectSemanticEmbeddingFailureDiagnostics(ctx, service, status)
	if err != nil || len(rows) != 1 {
		t.Fatalf("canonical diagnostics = %+v, %v", rows, err)
	}
	if rows[0].SourceKey != "1111111111111111" || rows[0].AssetID != "broken" || rows[0].Filename != "broken.jpg" || rows[0].RetryState != "scheduled" {
		t.Fatalf("canonical failure source identity = %+v", rows[0])
	}
	requested, err := service.RequestSemanticEmbeddingFailureRetry(ctx, status, nil, time.Now().UTC())
	if err != nil || requested.RequestedCount != 1 {
		t.Fatalf("canonical manual retry = %+v, %v", requested, err)
	}
	assets, err := service.catalog.loadCanonicalSemanticBackfillAssets(ctx, profile, 10, nil, "")
	if err != nil || len(assets) != 1 || assets[0].SelectedRetryRequestedAt == "" {
		t.Fatalf("requested canonical failure = %+v, %v", assets, err)
	}
	result, err := service.catalog.BackfillSemanticVectors(ctx, canonicalSemanticCorpusSourceKey, profile, time.Now().UTC(), SemanticBackfillOptions{ImageLoader: staticSemanticImageLoader{}, MaxAssets: 1})
	if err != nil || result.Status.CompletedVectorCount != 1 || result.Status.FailedVectorCount != 0 {
		t.Fatalf("manual retry recovery = %+v, %v", result, err)
	}
	var pending int
	if err := service.catalog.db.QueryRow(`SELECT count(*) FROM semantic_vector_retry_requests`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("consumed retry requests = %d, %v", pending, err)
	}
}

func TestCanonicalBackfillUsesConfiguredDuplicateAfterPrimaryRemoval(t *testing.T) {
	service, models, candidate, sources, badCalls := canonicalBoundarySources(t)
	ctx := context.Background()
	// Make the unavailable copy the catalog primary, while retaining a healthy
	// duplicate. Removing only its configuration must change input resolution.
	now := time.Now().UTC()
	for i, source := range sources {
		_, err := service.catalog.ReplaceFull(ctx, source.SourceKey, []ImmichMirrorAsset{{
			UpstreamAssetID: "asset", MediaType: "image", Filename: source.Name + ".jpg", CapturedAt: now.Add(time.Duration(i) * time.Hour),
			ContentSHA1Hex: "0123456789abcdef0123456789abcdef01234567", ContentSizeBytes: 1024,
		}}, 0, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	var primary string
	if err := service.catalog.db.QueryRow(`SELECT primary_source_key FROM catalog_canonical_assets WHERE visibility_status = 'active'`).Scan(&primary); err != nil || primary != sources[1].SourceKey {
		t.Fatalf("duplicate primary = %q, %v", primary, err)
	}
	service.ReconfigureDatasources(sources[:1])
	result, err := service.BackfillSemanticModelCandidateWithOptions(ctx, models, candidate, SemanticModelBackfillOptions{MaxAssets: 10})
	if err != nil || result.ProcessedVectorCount != 1 || result.Status.EligibleAssetCount != 1 || badCalls.Load() != 0 {
		t.Fatalf("configured duplicate backfill = %+v, %v, removed requests=%d", result, err, badCalls.Load())
	}
}

func TestCanonicalFailedInputRefreshResetsRetryDeadlineAfterAttempt(t *testing.T) {
	service, profile, _ := canonicalFailedInputFixture(t)
	ctx := context.Background()
	// A real source metadata change invalidates the previously attempted input.
	now := time.Now().UTC().Add(time.Minute)
	_, err := service.catalog.ReplaceFull(ctx, "1111111111111111", []ImmichMirrorAsset{{UpstreamAssetID: "broken", Filename: "broken.jpg", MediaType: "image", CapturedAt: now}}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.catalog.BackfillSemanticVectors(ctx, canonicalSemanticCorpusSourceKey, profile, now, SemanticBackfillOptions{
		ImageLoader: failingSemanticImageLoader{err: errors.Join(ErrSemanticAssetInput, errors.New("still invalid"))}, MaxAssets: 1,
	})
	if err != nil || result.ProcessedVectorCount != 1 {
		t.Fatalf("changed input retry = %+v, %v", result, err)
	}
	assets, err := service.catalog.loadCanonicalSemanticBackfillAssets(ctx, profile, 10, nil, "")
	if err != nil || len(assets) != 0 {
		t.Fatalf("repeated failed refresh skipped cooldown: %+v, %v", assets, err)
	}
}

func TestCanonicalFailureTargetsDeduplicateAndRespectSourceAndProfile(t *testing.T) {
	service, _, _, sources, _ := canonicalBoundarySources(t)
	ctx := context.Background()
	profile := testImageSemanticProfile{}
	status := SemanticModelProfileStatus{ModelID: profile.ModelID(), VectorSpaceID: profile.VectorSpaceID(), EmbeddingDim: profile.EmbeddingDim(), InputKind: profile.InputKind()}
	now := time.Now().UTC()
	for _, source := range sources {
		_, err := service.catalog.ReplaceFull(ctx, source.SourceKey, []ImmichMirrorAsset{{
			UpstreamAssetID: "asset", MediaType: "video", Filename: source.Name + ".mp4", CapturedAt: now,
			ContentSHA1Hex: "0123456789abcdef0123456789abcdef01234567", ContentSizeBytes: 1024,
		}}, 0, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := service.catalog.BackfillSemanticVectors(ctx, canonicalSemanticCorpusSourceKey, profile, now, SemanticBackfillOptions{
		ImageLoader: failingSemanticImageLoader{err: errors.Join(ErrSemanticAssetInput, errors.New("invalid poster"))}, MaxAssets: 10,
	})
	if err != nil || result.ProcessedVectorCount != 1 {
		t.Fatalf("duplicate poster failure = %+v, %v", result, err)
	}
	// A migrated source row must not duplicate the current canonical failure.
	if err := service.catalog.upsertSemanticVectorFailures(ctx, sources[0].SourceKey, profile, []semanticBackfillAssetFailure{{Asset: semanticAsset{ID: "asset"}, Err: errors.New("legacy failure")}}, now); err != nil {
		t.Fatal(err)
	}
	rows, err := collectSemanticEmbeddingFailureDiagnostics(ctx, service, status)
	if err != nil || len(rows) != 1 || rows[0].LastError == "legacy failure" || rows[0].MediaType != "video" {
		t.Fatalf("deduplicated failure diagnostics = %+v, %v", rows, err)
	}
	for _, scope := range [][]string{nil, {sources[1].SourceKey}} {
		requested, err := service.RequestSemanticEmbeddingFailureRetry(ctx, status, scope, now)
		if err != nil || requested.RequestedCount != 1 {
			t.Fatalf("scoped retry = %+v, %v", requested, err)
		}
	}
	wrongProfile := status
	wrongProfile.EmbeddingDim++
	rows, err = collectSemanticEmbeddingFailureDiagnostics(ctx, service, wrongProfile)
	if err != nil || len(rows) != 0 {
		t.Fatalf("other-profile failures = %+v, %v", rows, err)
	}
	requested, err := service.RequestSemanticEmbeddingFailureRetry(ctx, wrongProfile, nil, now)
	if err != nil || requested.RequestedCount != 0 {
		t.Fatalf("other-profile retry = %+v, %v", requested, err)
	}
	if _, err := service.catalog.db.Exec(`UPDATE catalog_assets SET visibility_status = 'missing'`); err != nil {
		t.Fatal(err)
	}
	rows, err = collectSemanticEmbeddingFailureDiagnostics(ctx, service, status)
	if err != nil || len(rows) != 0 {
		t.Fatalf("hidden failures = %+v, %v", rows, err)
	}
	requested, err = service.RequestSemanticEmbeddingFailureRetry(ctx, status, nil, now)
	if err != nil || requested.RequestedCount != 0 {
		t.Fatalf("hidden retry = %+v, %v", requested, err)
	}
}

func TestCanonicalPublishedCorpusRemainsValidAfterSourceRemoval(t *testing.T) {
	service, models, candidate, sources, _ := canonicalBoundarySources(t)
	ctx := context.Background()
	// Publish both sources while they are reachable, then remove one without
	// deleting its immutable vector or its catalog rows.
	sources[1].URL = sources[0].URL
	if err := service.ReconfigureDatasources(sources); err != nil {
		t.Fatal(err)
	}
	result, err := service.BackfillSemanticModelCandidateWithOptions(ctx, models, candidate, SemanticModelBackfillOptions{MaxAssets: 10, DrainIndexJobs: true})
	if err != nil || result.IndexedVectorCount != 2 {
		t.Fatalf("published corpus = %+v, %v", result, err)
	}
	if err := service.ReconfigureDatasources(sources[:1]); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.SemanticModelBackfillSnapshot(ctx, candidate)
	if err != nil || snapshot == nil || snapshot.Status.EligibleAssetCount != 1 || snapshot.Status.IndexedVectorCount != 1 {
		t.Fatalf("configured published coverage = %+v, %v", snapshot, err)
	}
	needed, count, err := service.SemanticModelIndexPublishNeededFromSnapshot(ctx, models, candidate, snapshot, false)
	if err != nil || needed || count != 0 {
		t.Fatalf("unnecessary snapshot republish = %t/%d, %v", needed, count, err)
	}
	needed, count, err = service.SemanticModelIndexPublishNeeded(ctx, models, candidate, nil, false)
	if err != nil || needed || count != 0 {
		t.Fatalf("unnecessary republish = %t/%d, %v", needed, count, err)
	}
	queued, err := service.ReconcileSemanticIndexJobs(ctx, models, candidate, nil, false)
	if err != nil || queued != 0 {
		t.Fatalf("unchanged corpus publication jobs = %d, %v", queued, err)
	}
}

func TestCanonicalPublishReportsConfiguredSourceProgress(t *testing.T) {
	for _, removedReady := range []bool{false, true} {
		t.Run(map[bool]string{false: "removed-unprocessed", true: "removed-ready"}[removedReady], func(t *testing.T) {
			service, models, candidate, sources, _ := canonicalBoundarySources(t)
			ctx := context.Background()
			if removedReady {
				sources[1].URL = sources[0].URL
				if err := service.ReconfigureDatasources(sources); err != nil {
					t.Fatal(err)
				}
			} else if err := service.ReconfigureDatasources(sources[:1]); err != nil {
				t.Fatal(err)
			}
			backfill, err := service.BackfillSemanticModelCandidateWithOptions(ctx, models, candidate, SemanticModelBackfillOptions{MaxAssets: 10})
			if err != nil || backfill.Status.PendingIndexJobCount != 1 {
				t.Fatalf("backfill = %+v, %v", backfill, err)
			}
			if err := service.ReconfigureDatasources(sources[:1]); err != nil {
				t.Fatal(err)
			}
			published, err := service.PublishNextSemanticIndexJob(ctx, models, candidate, nil)
			if err != nil || !published.Published {
				t.Fatalf("publish = %+v, %v", published, err)
			}
			progress := published.Status
			if progress.EligibleAssetCount != 1 || progress.CompletedVectorCount != 1 || progress.RemainingVectorCount != 0 || progress.IndexedVectorCount != 1 || progress.Status != SemanticBackfillStatusReady {
				t.Fatalf("post-publication progress includes removed source: %+v", progress)
			}
			if published.IndexedVectorCount != 1 {
				t.Fatalf("reported indexed count = %d, want configured coverage 1", published.IndexedVectorCount)
			}
			var storedIndexed int
			if err := service.catalog.db.QueryRow(`SELECT indexed_vector_count FROM semantic_state WHERE source_key = ? AND model_id = ?`, canonicalSemanticCorpusSourceKey, candidate.ModelID).Scan(&storedIndexed); err != nil {
				t.Fatal(err)
			}
			wantStored := 1
			if removedReady {
				wantStored = 2
			}
			if storedIndexed != wantStored {
				t.Fatalf("durable corpus count = %d, want %d", storedIndexed, wantStored)
			}
			needed, _, err := service.SemanticModelIndexPublishNeeded(ctx, models, candidate, nil, false)
			if err != nil || needed {
				t.Fatalf("unchanged corpus requires another publish: %t, %v", needed, err)
			}
			idle, err := service.PublishNextSemanticIndexJob(ctx, models, candidate, nil)
			if err != nil || idle.Published || idle.Status.ModelID != "" {
				t.Fatalf("no-job result = %+v, %v", idle, err)
			}
		})
	}
}
