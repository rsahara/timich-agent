package catalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSemanticIndexJobFailureBacksOffAndReconcilePreservesDeadline(t *testing.T) {
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	profile := testImageSemanticProfile{}
	sourceKey := "1111111111111111"
	now := time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC)
	if err := store.enqueueSemanticIndexJob(ctx, sourceKey, profile, now); err != nil {
		t.Fatalf("enqueueSemanticIndexJob() error = %v", err)
	}
	job, err := store.claimSemanticIndexJob(ctx, []string{sourceKey}, profile, now)
	if err != nil {
		t.Fatalf("claimSemanticIndexJob() error = %v", err)
	}
	if job == nil || job.Attempts != 1 {
		t.Fatalf("claimed job = %#v, want first attempt", job)
	}
	renewedAt := now.Add(5 * time.Minute)
	if err := store.renewSemanticIndexJobLease(ctx, job, renewedAt); err != nil {
		t.Fatalf("renewSemanticIndexJobLease() error = %v", err)
	}
	var leaseText string
	if err := store.db.QueryRowContext(ctx, `SELECT lease_expires_at FROM semantic_index_jobs WHERE id = ?`, job.ID).Scan(&leaseText); err != nil {
		t.Fatalf("read renewed semantic job lease: %v", err)
	}
	leaseAt, err := time.Parse(time.RFC3339Nano, leaseText)
	if err != nil {
		t.Fatalf("parse renewed semantic job lease: %v", err)
	}
	if want := renewedAt.Add(semanticIndexJobLeaseDuration); !leaseAt.Equal(want) {
		t.Fatalf("renewed semantic job lease = %s, want %s", leaseAt, want)
	}
	if err := store.failSemanticIndexJob(ctx, job.ID, job.Attempts, errors.New("disk full"), now); err != nil {
		t.Fatalf("failSemanticIndexJob() error = %v", err)
	}

	pending, failed, eligible, retryAt, err := store.semanticIndexJobState(ctx, sourceKey, profile.ModelID(), profile.VectorSpaceID(), now)
	if err != nil {
		t.Fatalf("semanticIndexJobState() error = %v", err)
	}
	wantRetryAt := now.Add(semanticRetryBaseInterval)
	if pending != 0 || failed != 1 || eligible != 0 || retryAt == nil || !retryAt.Equal(wantRetryAt) {
		t.Fatalf("failed job state = pending:%d failed:%d eligible:%d retry:%v, want retry at %s", pending, failed, eligible, retryAt, wantRetryAt)
	}

	if err := store.enqueueSemanticIndexJob(ctx, sourceKey, profile, now.Add(time.Second)); err != nil {
		t.Fatalf("reconcile enqueueSemanticIndexJob() error = %v", err)
	}
	_, _, eligible, retryAt, err = store.semanticIndexJobState(ctx, sourceKey, profile.ModelID(), profile.VectorSpaceID(), now.Add(time.Second))
	if err != nil {
		t.Fatalf("semanticIndexJobState(after reconcile) error = %v", err)
	}
	if eligible != 0 || retryAt == nil || !retryAt.Equal(wantRetryAt) {
		t.Fatalf("reconciled failed job eligible=%d retry=%v, want preserved retry %s", eligible, retryAt, wantRetryAt)
	}
	if job, err := store.claimSemanticIndexJob(ctx, []string{sourceKey}, profile, wantRetryAt.Add(-time.Nanosecond)); err != nil || job != nil {
		t.Fatalf("claim before retry = %#v, %v, want no job", job, err)
	}
	job, err = store.claimSemanticIndexJob(ctx, []string{sourceKey}, profile, wantRetryAt)
	if err != nil {
		t.Fatalf("claim at retry deadline error = %v", err)
	}
	if job == nil || job.Attempts != 2 {
		t.Fatalf("retried job = %#v, want second attempt", job)
	}
}

func TestSemanticIndexActivationFailureBacksOffCompletedJob(t *testing.T) {
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	profile := testImageSemanticProfile{}
	const sourceKey = "1111111111111111"
	now := time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC)
	if err := store.enqueueSemanticIndexJob(ctx, sourceKey, profile, now); err != nil {
		t.Fatalf("enqueueSemanticIndexJob() error = %v", err)
	}
	job, err := store.claimSemanticIndexJob(ctx, []string{sourceKey}, profile, now)
	if err != nil || job == nil {
		t.Fatalf("claimSemanticIndexJob() = %#v, %v", job, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE semantic_index_jobs SET status = ? WHERE id = ?`, semanticIndexJobStatusCompleted, job.ID); err != nil {
		t.Fatalf("complete semantic index job fixture: %v", err)
	}
	if err := store.failSemanticIndexJob(ctx, job.ID, job.Attempts, errors.New("manifest write failed"), now); err != nil {
		t.Fatalf("failSemanticIndexJob(completed) error = %v", err)
	}

	var status string
	var scheduledAtText string
	if err := store.db.QueryRowContext(ctx, `SELECT status, scheduled_at FROM semantic_index_jobs WHERE id = ?`, job.ID).Scan(&status, &scheduledAtText); err != nil {
		t.Fatalf("read semantic index job after activation failure: %v", err)
	}
	scheduledAt, err := time.Parse(time.RFC3339Nano, scheduledAtText)
	if err != nil {
		t.Fatalf("parse semantic index retry deadline: %v", err)
	}
	if status != semanticIndexJobStatusFailed || !scheduledAt.Equal(now.Add(semanticRetryBaseInterval)) {
		t.Fatalf("activation failure job status=%q scheduledAt=%s", status, scheduledAt)
	}
}

func TestSemanticIndexJobExpiredLeaseBecomesEligibleAndIsReclaimed(t *testing.T) {
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	profile := testImageSemanticProfile{}
	sourceKey := "2222222222222222"
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	if err := store.enqueueSemanticIndexJob(ctx, sourceKey, profile, now); err != nil {
		t.Fatalf("enqueueSemanticIndexJob() error = %v", err)
	}
	job, err := store.claimSemanticIndexJob(ctx, []string{sourceKey}, profile, now)
	if err != nil || job == nil {
		t.Fatalf("claimSemanticIndexJob() = %#v, %v", job, err)
	}

	leaseDeadline := now.Add(semanticIndexJobLeaseDuration)
	pending, failed, eligible, nextEligibleAt, err := store.semanticIndexJobState(
		ctx, sourceKey, profile.ModelID(), profile.VectorSpaceID(), leaseDeadline.Add(-time.Nanosecond),
	)
	if err != nil {
		t.Fatalf("semanticIndexJobState(before lease) error = %v", err)
	}
	if pending != 1 || failed != 0 || eligible != 0 || nextEligibleAt == nil || !nextEligibleAt.Equal(leaseDeadline) {
		t.Fatalf("state before lease = pending:%d failed:%d eligible:%d next:%v, want active until %s", pending, failed, eligible, nextEligibleAt, leaseDeadline)
	}

	_, _, eligible, _, err = store.semanticIndexJobState(
		ctx, sourceKey, profile.ModelID(), profile.VectorSpaceID(), leaseDeadline,
	)
	if err != nil {
		t.Fatalf("semanticIndexJobState(at lease) error = %v", err)
	}
	if eligible != 1 {
		t.Fatalf("eligible at expired lease = %d, want 1", eligible)
	}
	reclaimed, err := store.claimSemanticIndexJob(ctx, []string{sourceKey}, profile, leaseDeadline)
	if err != nil {
		t.Fatalf("claimSemanticIndexJob(expired lease) error = %v", err)
	}
	if reclaimed == nil || reclaimed.ID != job.ID || reclaimed.Attempts != 2 {
		t.Fatalf("reclaimed job = %#v, want id=%d attempt=2", reclaimed, job.ID)
	}
}

func TestSemanticSourceRetryIsScopedPerDatasource(t *testing.T) {
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	service := &Service{semanticSourceNow: func() time.Time { return now }}

	firstRetryAt := service.deferSemanticSourceRetry("source-a", now)
	if want := now.Add(semanticRetryBaseInterval); !firstRetryAt.Equal(want) {
		t.Fatalf("first retry = %s, want %s", firstRetryAt, want)
	}
	if retryAt, deferred := service.semanticSourceRetryDeadline("source-a", now); !deferred || retryAt == nil || !retryAt.Equal(firstRetryAt) {
		t.Fatalf("source-a retry = %v, %t, want deferred", retryAt, deferred)
	}
	if retryAt, deferred := service.semanticSourceRetryDeadline("source-b", now); deferred || retryAt != nil {
		t.Fatalf("source-b retry = %v, %t, want unaffected", retryAt, deferred)
	}

	now = firstRetryAt
	if retryAt, deferred := service.semanticSourceRetryDeadline("source-a", now); deferred || retryAt != nil {
		t.Fatalf("source-a retry at deadline = %v, %t, want eligible", retryAt, deferred)
	}
	secondRetryAt := service.deferSemanticSourceRetry("source-a", now)
	if want := now.Add(2 * semanticRetryBaseInterval); !secondRetryAt.Equal(want) {
		t.Fatalf("second retry = %s, want %s", secondRetryAt, want)
	}
	service.clearSemanticSourceRetry("source-a")
	if retryAt, deferred := service.semanticSourceRetryDeadline("source-a", now); deferred || retryAt != nil {
		t.Fatalf("cleared source-a retry = %v, %t, want eligible", retryAt, deferred)
	}
}

func TestSemanticBackfillWorkerCount(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		assetCount int
		want       int
	}{
		{name: "empty", configured: 3, assetCount: 0, want: 1},
		{name: "single asset", configured: 3, assetCount: 1, want: 1},
		{name: "auto", configured: 0, assetCount: 10, want: 1},
		{name: "configured", configured: 3, assetCount: 10, want: 3},
		{name: "capped by assets", configured: 8, assetCount: 3, want: 3},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := semanticBackfillWorkerCount(test.configured, test.assetCount); got != test.want {
				t.Fatalf("semanticBackfillWorkerCount(%d, %d) = %d, want %d", test.configured, test.assetCount, got, test.want)
			}
		})
	}
}

func TestSemanticBinaryTraversalUsesProcessedBudgetAndReportsFrontier(t *testing.T) {
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()

	const sourceKey = "3333333333333333"
	profile := testImageSemanticProfile{}
	assets := make([]semanticAsset, 0, 200)
	for index := 0; index < 200; index++ {
		assetID := fmt.Sprintf("asset-%03d", index)
		assets = append(assets, semanticAsset{
			SourceKey:  sourceKey,
			ID:         assetID,
			MediaType:  "image",
			Filename:   assetID + ".jpg",
			CapturedAt: time.Unix(int64(index), 0).UTC(),
			Vector:     semanticTestVector(index, profile.EmbeddingDim()),
			MaxLevel:   semanticHNSWLevel(assetID),
		})
	}
	seedAndWriteSemanticBinaryIndexForTest(t, store, context.Background(), sourceKey, profile, assets, time.Now().UTC(), 0)
	reader, err := store.openSemanticBinaryIndexFileForGeneration(context.Background(), sourceKey, profile, 0)
	if err != nil {
		t.Fatalf("openSemanticBinaryIndexFile() error = %v", err)
	}
	defer reader.Close()

	session, err := reader.newSearchSession(context.Background(), []float32{1, 0, 0, 0})
	if err != nil {
		t.Fatalf("newSearchSession() error = %v", err)
	}
	result, exhausted, err := session.advance(context.Background(), 40)
	if err != nil {
		t.Fatalf("advance() error = %v", err)
	}
	if len(result) != 40 || session.processed != 40 {
		t.Fatalf("traversal result count=%d visited=%d, want full processed budget 40", len(result), session.processed)
	}
	for _, scored := range result {
		if len(scored.Asset.Vector) != profile.EmbeddingDim() {
			t.Fatalf("traversal vector length=%d, want %d", len(scored.Asset.Vector), profile.EmbeddingDim())
		}
	}
	if exhausted {
		t.Fatal("search Exhausted = true with an unvisited frontier")
	}
}

func TestSemanticBinaryManifestDetectsBodyCorruption(t *testing.T) {
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()

	const sourceKey = "3333333333333333"
	profile := testImageSemanticProfile{}
	builtAt := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	assets := []semanticAsset{{
		SourceKey: sourceKey, ID: "asset-a", MediaType: "image", Filename: "asset.jpg",
		CapturedAt: builtAt, Vector: []float32{1, 0, 0, 0}, MaxLevel: 0,
	}}
	seedAndWriteSemanticBinaryIndexForTest(t, store, context.Background(), sourceKey, profile, assets, builtAt, 3)
	status := SemanticModelBackfillStatus{
		ModelID: profile.ModelID(), VectorSpaceID: profile.VectorSpaceID(), EmbeddingDim: profile.EmbeddingDim(),
		CompletedVectorCount: 1, IndexedVectorCount: 1, AssetGeneration: 3, IndexedGeneration: 3, LastPublishedAt: &builtAt,
	}
	if !store.semanticBinaryIndexMatchesBackfillStatus(context.Background(), sourceKey, profile, status) {
		t.Fatal("fresh semantic binary index did not match its manifest")
	}
	path := store.semanticBinaryIndexPath(sourceKey, profile, 3)
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open semantic binary for corruption: %v", err)
	}
	if _, err := file.WriteAt([]byte{0xff}, semanticBinaryIndexHeaderBytes); err != nil {
		_ = file.Close()
		t.Fatalf("corrupt semantic binary body: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close corrupted semantic binary: %v", err)
	}
	if store.semanticBinaryIndexMatchesBackfillStatus(context.Background(), sourceKey, profile, status) {
		t.Fatal("corrupted semantic binary index still matched its manifest")
	}
}

func TestSemanticBinaryReaderRejectsImpossibleNodeCountBeforeAllocation(t *testing.T) {
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()

	const sourceKey = "3333333333333333"
	profile := testImageSemanticProfile{}
	path := store.semanticBinaryIndexPath(sourceKey, profile, 4)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create semantic binary directory: %v", err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create malformed semantic binary: %v", err)
	}
	header := semanticBinaryIndexHeader{
		Version: semanticBinaryIndexVersion, Precision: semanticSearchIndexPrecision, SourceKey: sourceKey,
		ModelID: profile.ModelID(), VectorSpaceID: profile.VectorSpaceID(), EmbeddingDim: profile.EmbeddingDim(),
		IndexedVectorCount: int(^uint(0) >> 1), AssetGeneration: 4, NodeCount: int(^uint(0) >> 1),
		NodeOffset: semanticBinaryIndexHeaderBytes, VectorOffset: semanticBinaryIndexHeaderBytes,
		EdgeOffset: semanticBinaryIndexHeaderBytes, StringOffset: semanticBinaryIndexHeaderBytes,
		VectorStride: profile.EmbeddingDim() * 4, NodeRecordSize: semanticBinaryIndexNodeRecordSize, EdgeRecordSize: semanticBinaryIndexEdgeRecordSize,
	}
	if err := writeSemanticBinaryHeader(file, header); err != nil {
		_ = file.Close()
		t.Fatalf("write malformed semantic binary header: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close malformed semantic binary: %v", err)
	}
	if _, err := store.openSemanticBinaryIndexFileForGeneration(context.Background(), sourceKey, profile, 4); !errors.Is(err, errSemanticBinaryIndexUnavailable) {
		t.Fatalf("open malformed semantic binary error = %v, want unavailable", err)
	}
}

func TestSemanticIndexPublishNeededWaitsForGenerationVectorCoverage(t *testing.T) {
	t.Parallel()

	store := &CatalogStore{}
	profile := testImageSemanticProfile{}
	tests := []struct {
		name         string
		eligible     int
		completed    int
		allowPartial bool
		want         bool
	}{
		{name: "empty snapshot clears", want: true},
		{name: "eligible assets without vectors wait", eligible: 1, want: false},
		{name: "incomplete vectors wait", eligible: 2, completed: 1, want: false},
		{name: "explicit partial publish", eligible: 2, completed: 1, allowPartial: true, want: true},
		{name: "complete replacement publishes", eligible: 2, completed: 2, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := SemanticModelBackfillStatus{
				EligibleAssetCount:   test.eligible,
				CompletedVectorCount: test.completed,
				AssetGeneration:      1,
				IndexedGeneration:    0,
			}
			if got := store.semanticIndexPublishNeeded(context.Background(), "source", profile, status, test.allowPartial); got != test.want {
				t.Fatalf("semanticIndexPublishNeeded() = %t, want %t for %#v", got, test.want, status)
			}
		})
	}
}

func TestSemanticDiversifySemanticCandidateSnapshotDelaysNearDuplicates(t *testing.T) {
	scored := []semanticScoredAsset{
		semanticTestScoredAsset("primary", 0.90, []float32{1, 0}),
		semanticTestScoredAsset("near-copy", 0.89, []float32{0.98, 0.02}),
		semanticTestScoredAsset("different", 0.88, []float32{0, 1}),
		semanticTestScoredAsset("also-different", 0.87, []float32{0.70, 0.70}),
	}

	diversified := diversifySemanticCandidateSnapshot(scored)

	if got, want := semanticTestAssetIDs(diversified), []string{"primary", "different", "also-different", "near-copy"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("diversified asset IDs = %v, want %v", got, want)
	}
}

func TestSemanticDiversifySemanticCandidateSnapshotKeepsRelevantOrderWhenDistinct(t *testing.T) {
	scored := []semanticScoredAsset{
		semanticTestScoredAsset("first", 0.90, []float32{1, 0}),
		semanticTestScoredAsset("second", 0.89, []float32{0, 1}),
		semanticTestScoredAsset("third", 0.88, []float32{0.70, 0.70}),
	}

	diversified := diversifySemanticCandidateSnapshot(scored)

	if got, want := semanticTestAssetIDs(diversified), []string{"first", "second", "third"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("diversified asset IDs = %v, want %v", got, want)
	}
}

func semanticTestScoredAsset(id string, similarity float32, vector []float32) semanticScoredAsset {
	return semanticScoredAsset{
		Asset: semanticAsset{
			ID:     id,
			Vector: vector,
		},
		Similarity: similarity,
	}
}

func semanticTestVector(seed int, dim int) []float32 {
	vector := make([]float32, dim)
	vector[seed%dim] = 1
	vector[(seed*7+3)%dim] += 0.5
	vector[(seed*11+5)%dim] += 0.25
	return normalizeSemanticVector(vector)
}

func semanticTestAssetIDs(scored []semanticScoredAsset) []string {
	ids := make([]string, 0, len(scored))
	for _, item := range scored {
		ids = append(ids, item.Asset.ID)
	}
	return ids
}
