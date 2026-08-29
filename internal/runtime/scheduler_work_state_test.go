package runtime

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
)

func TestSchedulerWorkStateFreshness(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	schedule := semanticIndexingSchedule{}
	state := schedulerWorkState{
		ConfigHash: "config-a",
		UpdatedAt:  now.Add(-29 * time.Minute),
	}
	if !state.fresh(now, "config-a", schedule, false) {
		t.Fatal("fresh() = false, want true before stale threshold")
	}
	if state.fresh(now, "config-b", schedule, false) {
		t.Fatal("fresh() = true after config mismatch, want false")
	}
	state.Dirty = true
	if state.fresh(now, "config-a", schedule, false) {
		t.Fatal("fresh() = true for dirty state, want false")
	}
	state.Dirty = false
	state.UpdatedAt = now.Add(-30 * time.Minute)
	if state.fresh(now, "config-a", schedule, false) {
		t.Fatal("fresh() = true at stale threshold, want false")
	}
	state.UpdatedAt = now
	deadline := now
	state.MetadataNextEligibleAt = &deadline
	if state.fresh(now, "config-a", schedule, false) {
		t.Fatal("fresh() = true at metadata eligibility deadline, want recount")
	}
	state.MetadataNextEligibleAt = nil
	state.SemanticNextEligibleAt = &deadline
	if state.fresh(now, "config-a", schedule, true) {
		t.Fatal("fresh() = true at semantic eligibility deadline, want recount")
	}
}

func TestSchedulerWorkStateRecomputeWaitsForForegroundQuiet(t *testing.T) {
	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{{
		Name: "Static demo",
		Kind: config.DatasourceKindStaticDemo,
		URL:  writeStaticDemoManifest(t),
	}})
	runtime.foregroundCatalog.quietPeriod = time.Nanosecond
	finishForeground := runtime.foregroundCatalog.begin()
	defer finishForeground()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if state, ok := runtime.schedulerWorkStateSnapshot(ctx, semanticIndexingSchedule{}, false); ok {
		t.Fatalf("schedulerWorkStateSnapshot() state=%+v ok=true during foreground work, want deferred recount", state)
	}
}

func TestSchedulerWorkStateRecomputeLogsRealErrorBeforeReleaseCancellation(t *testing.T) {
	localRoot := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "nas-photos", Path: localRoot}}
	})
	if err := runtime.catalogService().Close(); err != nil {
		t.Fatalf("Close(catalog service) error = %v", err)
	}

	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
	})

	if state, ok := runtime.schedulerWorkStateSnapshot(context.Background(), semanticIndexingSchedule{}, false); ok {
		t.Fatalf("schedulerWorkStateSnapshot() state=%+v ok=true with closed catalog, want failure", state)
	}
	if !strings.Contains(logs.String(), "scheduler work-state recompute failed") ||
		!strings.Contains(logs.String(), "count local metadata jobs") {
		t.Fatalf("scheduler recompute logs = %q, want real database error", logs.String())
	}
}

func TestSchedulerWorkStateRetriesNegativeSemanticReadinessAtScheduleInterval(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	schedule := semanticIndexingSchedule{Interval: 30 * time.Second, Workers: 1}
	state := schedulerWorkState{
		ConfigHash:           "config-a",
		UpdatedAt:            now.Add(-30 * time.Second),
		SemanticScheduled:    true,
		SemanticRuntimeRetry: true,
	}
	if state.fresh(now, "config-a", schedule, true) {
		t.Fatal("fresh() = true for negative semantic readiness at retry interval, want recount")
	}
	pausedSchedule := schedule
	pausedSchedule.Workers = 0
	if !state.fresh(now, "config-a", pausedSchedule, true) {
		t.Fatal("fresh() = false for paused semantic workers before general stale threshold")
	}
	state.SemanticRuntimeRetry = false
	state.SemanticEmbeddingReady = true
	if !state.fresh(now, "config-a", schedule, true) {
		t.Fatal("fresh() = false for positive semantic readiness before general stale threshold")
	}
}

func TestSemanticSchedulerRuntimeRetryNeededOnlyDuringManagedWarmup(t *testing.T) {
	profile := catalog.SemanticModelProfileStatus{
		ModelID:       "model-a",
		VectorSpaceID: "model-a/d4",
		Role:          catalog.SemanticModelRoleCandidate,
		ProfileKind:   catalog.SemanticProfileKindModelPack,
		Runtime: &catalog.SemanticModelRuntimeStatus{
			Runtime:  semanticONNXRuntimeName,
			Loaded:   false,
			CanEmbed: false,
		},
	}
	status := catalog.SemanticModelRegistryStatus{Profiles: []catalog.SemanticModelProfileStatus{profile}}
	onnx := SemanticONNXRuntimeResponse{Status: semanticONNXRuntimeStatusRunning, ProcessCount: 1}
	if !semanticSchedulerRuntimeRetryNeeded(context.Background(), status, nil, onnx) {
		t.Fatal("semanticSchedulerRuntimeRetryNeeded() = false during managed runtime warmup")
	}
	status.Profiles[0].Runtime.Loaded = true
	status.Profiles[0].Runtime.CanEmbed = true
	if semanticSchedulerRuntimeRetryNeeded(context.Background(), status, nil, onnx) {
		t.Fatal("semanticSchedulerRuntimeRetryNeeded() = true after runtime is ready")
	}
	status.Profiles[0].Runtime.Loaded = false
	status.Profiles[0].Runtime.CanEmbed = false
	onnx.Status = semanticONNXRuntimeStatusFailed
	if semanticSchedulerRuntimeRetryNeeded(context.Background(), status, nil, onnx) {
		t.Fatal("semanticSchedulerRuntimeRetryNeeded() = true for failed managed runtime")
	}
}

func TestSemanticSchedulerSnapshotLoadsReadyProfileOncePerDecision(t *testing.T) {
	nextEligibleAt := time.Now().UTC().Add(time.Minute)
	profile := catalog.SemanticModelProfileStatus{
		ModelID:       "model-a",
		VectorSpaceID: "model-a/d4",
		EmbeddingDim:  4,
		Role:          catalog.SemanticModelRoleCandidate,
		ProfileKind:   catalog.SemanticProfileKindModelPack,
		Runtime: &catalog.SemanticModelRuntimeStatus{
			Loaded:   true,
			CanEmbed: true,
		},
	}
	loads := 0
	snapshot := loadSemanticSchedulerSnapshot(context.Background(), []catalog.SemanticModelProfileStatus{profile}, func(context.Context, catalog.SemanticModelProfileStatus) (*catalog.SemanticModelBackfillSnapshot, error) {
		loads++
		return &catalog.SemanticModelBackfillSnapshot{Status: catalog.SemanticModelBackfillStatus{
			ModelID:                profile.ModelID,
			VectorSpaceID:          profile.VectorSpaceID,
			EmbeddingDim:           profile.EmbeddingDim,
			EligibleAssetCount:     200,
			EligibleNowVectorCount: 100,
			CompletedVectorCount:   100,
			IndexedVectorCount:     50,
			NextEligibleAt:         &nextEligibleAt,
		}}, nil
	})

	lookup := snapshot.backfillStatus
	if got := semanticSchedulerNextEligibleAtWithLookup(snapshot.profiles, lookup); got == nil || !got.Equal(nextEligibleAt) {
		t.Fatalf("semanticSchedulerNextEligibleAtWithLookup() = %v, want %s", got, nextEligibleAt)
	}
	if got := semanticCandidateNeedingPriorityIndexPublishWithLookup(snapshot.profiles, lookup); got == nil || got.ModelID != profile.ModelID {
		t.Fatalf("semanticCandidateNeedingPriorityIndexPublishWithLookup() = %#v, want model-a", got)
	}
	if got, status := semanticCandidateNeedingMixedBackfillWithLookup(snapshot.profiles, semanticIndexingSchedule{BatchSize: 100}, lookup); got == nil || status == nil {
		t.Fatalf("semanticCandidateNeedingMixedBackfillWithLookup() = %#v, %#v, want candidate and status", got, status)
	}
	if got := semanticCandidateNeedingBackfillWithLookup(snapshot.profiles, lookup); got == nil || got.ModelID != profile.ModelID {
		t.Fatalf("semanticCandidateNeedingBackfillWithLookup() = %#v, want model-a", got)
	}
	if loads != 1 {
		t.Fatalf("semantic snapshot loads = %d, want exactly 1 for all scheduler decisions", loads)
	}
}

func TestPopulateSemanticSchedulerWorkStateLoadsEachProfileOncePerDecision(t *testing.T) {
	modelID := "scheduler-single-snapshot"
	localRoot := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "nas-photos", Path: localRoot}}
		cfg.SemanticRuntime.HelperPath = writeSchedulerSnapshotSemanticHelper(t, modelID)
	})
	installRuntimeSemanticPackForTest(t, runtime, runtimeSemanticPackForTest(modelID))
	deadline := time.Now().Add(5 * time.Second)
	var profiles []catalog.SemanticModelProfileStatus
	for time.Now().Before(deadline) {
		installed := runtime.SemanticModelRegistryInstalledStatusWithContext(context.Background())
		profiles = semanticBackfillCandidateProfiles(context.Background(), installed, runtime.semanticModels)
		if len(profiles) == 1 && profiles[0].Runtime != nil && profiles[0].Runtime.Loaded && profiles[0].Runtime.CanEmbed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(profiles) != 1 || profiles[0].Runtime == nil || !profiles[0].Runtime.Loaded || !profiles[0].Runtime.CanEmbed {
		var runtimeStatus *catalog.SemanticModelRuntimeStatus
		if len(profiles) == 1 {
			runtimeStatus = profiles[0].Runtime
		}
		t.Fatalf("installed scheduler profiles = %#v runtime=%+v, want one ready runtime profile", profiles, runtimeStatus)
	}

	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
	})

	state := schedulerWorkState{}
	runtime.populateSemanticSchedulerWorkState(
		context.Background(),
		runtime.catalogService(),
		semanticIndexingSchedule{Workers: 1, BatchSize: 100},
		&state,
	)

	started := "timich-agent semantic model backfill status started model=" + modelID + " "
	if loads := strings.Count(logs.String(), started); loads != 1 {
		t.Fatalf("semantic snapshot loads through populateSemanticSchedulerWorkState = %d, want exactly 1; logs:\n%s", loads, logs.String())
	}
}

func writeSchedulerSnapshotSemanticHelper(t *testing.T, modelID string) string {
	t.Helper()

	helperPath := filepath.Join(t.TempDir(), "timich-semantic-helper")
	script := fmt.Sprintf(`#!/bin/sh
command="$1"
case "$command" in
  inspect)
    printf '%%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"%s","vectorSpaceId":"%s/d4","embeddingDim":4,"inputKind":"image","loaded":true,"canEmbed":true}'
    ;;
  *)
    exit 2
    ;;
esac
`, modelID, modelID)
	if err := os.WriteFile(helperPath, []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return helperPath
}

func TestSchedulerWorkStateLocalMetadataDeltaAfterAssignment(t *testing.T) {
	runtime := &AgentRuntime{}
	runtime.schedulerWorkState = schedulerWorkState{
		ConfigHash:        "config-a",
		UpdatedAt:         time.Now().UTC(),
		MetadataQueued:    50,
		ThumbnailQueued:   0,
		SemanticScheduled: true,
	}

	runtime.schedulerWorkStateAssign("metadata", 48)
	runtime.schedulerWorkStateCompleteLocalMetadata(catalog.LocalMetadataBatchResult{
		ProcessedJobs:    48,
		CompletedJobs:    48,
		RegisteredAssets: 48,
	}, 48)

	state := runtime.schedulerWorkState
	if state.MetadataQueued != 2 {
		t.Fatalf("MetadataQueued = %d, want 2", state.MetadataQueued)
	}
	if state.ThumbnailQueued != 48 {
		t.Fatalf("ThumbnailQueued = %d, want 48", state.ThumbnailQueued)
	}
	if state.Dirty {
		t.Fatal("Dirty = true after registering media, want no semantic recount")
	}

	runtime.schedulerWorkStateAssign("metadata", 2)
	runtime.schedulerWorkStateCompleteLocalMetadata(catalog.LocalMetadataBatchResult{
		ProcessedJobs:    2,
		CompletedJobs:    2,
		RegisteredAssets: 2,
	}, 2)

	state = runtime.schedulerWorkState
	if state.MetadataQueued != 0 {
		t.Fatalf("MetadataQueued after drain = %d, want 0", state.MetadataQueued)
	}
	if state.ThumbnailQueued != 50 {
		t.Fatalf("ThumbnailQueued after drain = %d, want 50", state.ThumbnailQueued)
	}
	if state.Dirty {
		t.Fatal("Dirty = true after registering more media, want no semantic recount")
	}
}

func TestSchedulerWorkStateResettledMetadataForcesDeadlineRecount(t *testing.T) {
	runtime := &AgentRuntime{}
	runtime.schedulerWorkState = schedulerWorkState{
		ConfigHash:     "config-a",
		UpdatedAt:      time.Now().UTC(),
		MetadataQueued: 1,
	}
	runtime.schedulerWorkStateAssign("metadata", 1)
	runtime.schedulerWorkStateCompleteLocalMetadata(catalog.LocalMetadataBatchResult{
		ProcessedJobs: 1,
		CompletedJobs: 1,
		SettlingJobs:  1,
	}, 1)
	if !runtime.schedulerWorkState.Dirty {
		t.Fatal("Dirty = false after metadata resettled, want deadline recount")
	}
}

func TestSchedulerWorkStateDeferredLocalWorkForcesTrustedRootRecount(t *testing.T) {
	runtime := &AgentRuntime{}
	runtime.schedulerWorkState = schedulerWorkState{MetadataQueued: 1}
	runtime.schedulerWorkStateAssign("metadata", 1)
	runtime.schedulerWorkStateCompleteLocalMetadata(catalog.LocalMetadataBatchResult{
		ProcessedJobs: 1,
		DeferredJobs:  1,
	}, 1)
	if !runtime.schedulerWorkState.Dirty {
		t.Fatal("Dirty = false after deferred metadata, want trusted-root recount")
	}

	runtime.schedulerWorkState = schedulerWorkState{ThumbnailQueued: 1}
	runtime.schedulerWorkStateAssign("thumbnails", 1)
	runtime.schedulerWorkStateCompleteLocalThumbnail(catalog.LocalThumbnailBatchResult{
		ProcessedJobs: 1,
		DeferredJobs:  1,
	}, 1)
	if !runtime.schedulerWorkState.Dirty {
		t.Fatal("Dirty = false after deferred thumbnail, want trusted-root recount")
	}
}

func TestSchedulerWorkStateLocalThumbnailDeltaReleasesUnprocessedAssignment(t *testing.T) {
	runtime := &AgentRuntime{}
	runtime.schedulerWorkState = schedulerWorkState{
		ConfigHash:                  "config-a",
		UpdatedAt:                   time.Now().UTC(),
		ThumbnailQueued:             50,
		SemanticMixedEmbeddingBatch: 100,
	}

	runtime.schedulerWorkStateAssign("thumbnails", 16)
	runtime.schedulerWorkStateCompleteLocalThumbnail(catalog.LocalThumbnailBatchResult{
		ProcessedJobs:        10,
		CompletedJobs:        10,
		GeneratedAssets:      10,
		GeneratedImageAssets: 8,
	}, 16)

	if got := runtime.schedulerWorkState.ThumbnailQueued; got != 40 {
		t.Fatalf("ThumbnailQueued = %d, want 40 after partial assigned batch", got)
	}
	if runtime.schedulerWorkState.Dirty {
		t.Fatal("Dirty = true after generating thumbnails, want in-memory semantic delta")
	}
	if got := runtime.schedulerWorkState.SemanticMixedEmbeddingHint; got != 8 {
		t.Fatalf("SemanticMixedEmbeddingHint = %d, want 8 generated images", got)
	}

	runtime.schedulerWorkStateCompleteLocalThumbnail(catalog.LocalThumbnailBatchResult{
		ProcessedJobs:   16,
		CompletedJobs:   16,
		GeneratedAssets: 16,
	}, 0)

	if got := runtime.schedulerWorkState.ThumbnailQueued; got != 24 {
		t.Fatalf("ThumbnailQueued = %d, want 24 after direct unassigned batch", got)
	}
}

func TestSchedulerWorkStateSemanticMixedEmbeddingScheduledQueuedIncludesHint(t *testing.T) {
	state := schedulerWorkState{
		SemanticMixedEmbeddingQueued: 80,
		SemanticMixedEmbeddingHint:   20,
	}
	if got := state.semanticMixedEmbeddingScheduledQueued(); got != 100 {
		t.Fatalf("semanticMixedEmbeddingScheduledQueued() = %d, want 100", got)
	}
}

func TestSchedulerWorkStateHintOnlyNoWorkDiscardsPhantomEmbeddingRetry(t *testing.T) {
	runtime := &AgentRuntime{}
	runtime.schedulerWorkState = schedulerWorkState{
		ConfigHash:                  "config-a",
		UpdatedAt:                   time.Now().UTC(),
		SemanticMixedEmbeddingHint:  100,
		SemanticMixedEmbeddingBatch: 100,
	}

	schedule := semanticIndexingSchedule{Workers: 1, BatchSize: 100}
	assignment, ok := runtime.mixedBackgroundWorkerAssignment(1, true, schedule, runtime.schedulerWorkState)
	if !ok || assignment.phase != "embeddings" {
		t.Fatalf("mixedBackgroundWorkerAssignment() = %#v, %t, want hint-only embeddings", assignment, ok)
	}
	runtime.schedulerWorkStateAssign(assignment.phase, assignment.planned)
	if assignment.run(context.Background()) {
		t.Fatal("hint-only mixed embedding run = true, want no-work")
	}

	state := runtime.schedulerWorkState
	if state.SemanticMixedEmbeddingQueued != 0 || state.SemanticMixedEmbeddingHint != 0 {
		t.Fatalf("mixed embedding work = queued %d hint %d, want no phantom retry", state.SemanticMixedEmbeddingQueued, state.SemanticMixedEmbeddingHint)
	}
	if !state.Dirty {
		t.Fatal("Dirty = false after hint-only no-work, want catalog reconciliation")
	}
}

func TestSchedulerWorkStateSemanticStatusPreservesHintsAddedDuringEmbedding(t *testing.T) {
	runtime := &AgentRuntime{}
	runtime.schedulerWorkState = schedulerWorkState{
		ConfigHash:                  "config-a",
		UpdatedAt:                   time.Now().UTC(),
		SemanticMixedEmbeddingHint:  8,
		SemanticMixedEmbeddingBatch: 100,
	}

	consumedHint := runtime.schedulerWorkStateEmbeddingHintSnapshot()
	runtime.schedulerWorkStateCompleteLocalThumbnail(catalog.LocalThumbnailBatchResult{
		ProcessedJobs:        4,
		CompletedJobs:        4,
		GeneratedAssets:      4,
		GeneratedImageAssets: 4,
	}, 4)
	runtime.schedulerWorkStateApplySemanticStatusAfterEmbedding(catalog.SemanticModelBackfillStatus{
		EligibleAssetCount:   100,
		CompletedVectorCount: 100,
		IndexedVectorCount:   100,
	}, consumedHint)

	if got := runtime.schedulerWorkState.SemanticMixedEmbeddingHint; got != 4 {
		t.Fatalf("SemanticMixedEmbeddingHint = %d, want 4 hints added during embedding", got)
	}
}

func TestSchedulerWorkStateSemanticStatusKeepsWaitTargetWithPendingPublishJob(t *testing.T) {
	runtime := &AgentRuntime{}
	runtime.schedulerWorkState = schedulerWorkState{
		ConfigHash:        "config-a",
		UpdatedAt:         time.Now().UTC(),
		SemanticScheduled: true,
	}

	runtime.schedulerWorkStateApplySemanticStatus(catalog.SemanticModelBackfillStatus{
		EligibleAssetCount:     1500,
		EligibleNowVectorCount: 450,
		CompletedVectorCount:   1050,
		IndexedVectorCount:     1000,
		RemainingVectorCount:   450,
		PendingIndexJobCount:   1,
		FailedIndexJobCount:    0,
		EligibleIndexJobCount:  1,
	})

	state := runtime.schedulerWorkState
	if state.SemanticWaitingQueuedTarget != 200 {
		t.Fatalf("SemanticWaitingQueuedTarget = %d, want 200 even with a pending publish job", state.SemanticWaitingQueuedTarget)
	}
	if !state.SemanticPublishReady {
		t.Fatal("SemanticPublishReady = false, want true for pending publish job")
	}
}

func TestSchedulerWorkStateSemanticBackoffClearsAssignmentUntilDeadline(t *testing.T) {
	runtime := &AgentRuntime{}
	nextEligibleAt := time.Now().UTC().Add(30 * time.Minute)
	runtime.schedulerWorkState = schedulerWorkState{
		ConfigHash:                   "config-a",
		UpdatedAt:                    time.Now().UTC(),
		SemanticScheduled:            true,
		SemanticEmbeddingReady:       true,
		SemanticEmbeddingBatchSize:   10,
		SemanticMixedEmbeddingQueued: 10,
	}

	runtime.schedulerWorkStateApplySemanticStatus(catalog.SemanticModelBackfillStatus{
		EligibleAssetCount:     1,
		EligibleNowVectorCount: 0,
		RemainingVectorCount:   1,
		NextEligibleAt:         &nextEligibleAt,
	})

	state := runtime.schedulerWorkState
	if state.SemanticEmbeddingReady || state.SemanticEmbeddingBatchSize != 0 || state.SemanticMixedEmbeddingQueued != 0 {
		t.Fatalf("semantic assignment state = %+v, want no eligible work during backoff", state)
	}
	if state.SemanticNextEligibleAt == nil || !state.SemanticNextEligibleAt.Equal(nextEligibleAt) {
		t.Fatalf("SemanticNextEligibleAt = %v, want %s", state.SemanticNextEligibleAt, nextEligibleAt)
	}
	if state.Dirty {
		t.Fatal("Dirty = true during explicit semantic backoff, want cached deadline")
	}
}

func TestSchedulerWorkStateDefersStaleScheduledEmbeddingAssignment(t *testing.T) {
	runtime := &AgentRuntime{}
	runtime.schedulerWorkState = schedulerWorkState{
		SemanticEmbeddingReady:     true,
		SemanticEmbeddingBatchSize: 8,
	}
	nextEligibleAt := time.Now().UTC().Add(time.Minute)

	runtime.schedulerWorkStateDeferScheduledEmbeddingWork(nextEligibleAt)

	state := runtime.schedulerWorkState
	if state.SemanticEmbeddingReady || state.SemanticEmbeddingBatchSize != 0 || state.Dirty {
		t.Fatalf("deferred scheduled embedding state = %+v, want no cached assignment or immediate dirty refresh", state)
	}
	if state.SemanticNextEligibleAt == nil || !state.SemanticNextEligibleAt.Equal(nextEligibleAt) {
		t.Fatalf("SemanticNextEligibleAt = %v, want retry at %s", state.SemanticNextEligibleAt, nextEligibleAt)
	}
}

func TestNextBackgroundWorkerAssignmentDefersOnlySemanticEmbeddingAfterSharedFailure(t *testing.T) {
	runtime := &AgentRuntime{}
	nextRetry := time.Now().UTC().Add(10 * time.Minute)
	runtime.setSemanticIndexingRetryNotBefore(&nextRetry)
	schedule := semanticIndexingSchedule{Interval: time.Minute, BatchSize: 8, Workers: 1}
	state := schedulerWorkState{
		SemanticScheduled:          true,
		SemanticEmbeddingReady:     true,
		SemanticEmbeddingBatchSize: 8,
	}

	if assignment, ok := runtime.nextBackgroundWorkerAssignment(context.Background(), 1, false, schedule, true, state, true); ok {
		t.Fatalf("deferred assignment = %+v, want no embedding retry before deadline", assignment)
	}

	state.SemanticPublishReady = true
	assignment, ok := runtime.nextBackgroundWorkerAssignment(context.Background(), 1, false, schedule, true, state, true)
	if !ok || assignment.phase != "search_index" {
		t.Fatalf("publish assignment = %+v, %t, want search_index while embedding is deferred", assignment, ok)
	}

	pastRetry := time.Now().UTC().Add(-time.Second)
	runtime.setSemanticIndexingRetryNotBefore(&pastRetry)
	state.SemanticPublishReady = false
	assignment, ok = runtime.nextBackgroundWorkerAssignment(context.Background(), 1, false, schedule, true, state, true)
	if !ok || assignment.phase != "embeddings" {
		t.Fatalf("expired retry assignment = %+v, %t, want embeddings", assignment, ok)
	}
}

func TestNextBackgroundWorkerAssignmentDefersOnlySemanticPublishAfterFailure(t *testing.T) {
	runtime := &AgentRuntime{}
	nextRetry := time.Now().UTC().Add(10 * time.Minute)
	runtime.setSemanticPublishRetryNotBefore(&nextRetry)
	schedule := semanticIndexingSchedule{Interval: time.Minute, BatchSize: 8, Workers: 1}
	state := schedulerWorkState{
		SemanticScheduled:          true,
		SemanticPublishReady:       true,
		SemanticEmbeddingReady:     true,
		SemanticEmbeddingBatchSize: 8,
	}

	assignment, ok := runtime.nextBackgroundWorkerAssignment(context.Background(), 1, false, schedule, true, state, true)
	if !ok || assignment.phase != "embeddings" {
		t.Fatalf("assignment = %+v, %t, want embeddings while publish is deferred", assignment, ok)
	}

	state.SemanticEmbeddingReady = false
	if assignment, ok := runtime.nextBackgroundWorkerAssignment(context.Background(), 1, false, schedule, true, state, true); ok {
		t.Fatalf("deferred publish assignment = %+v, want no search-index retry before deadline", assignment)
	}
}

func TestNextBackgroundWorkerAssignmentYieldsContentVerificationToMetadataWork(t *testing.T) {
	runtime := &AgentRuntime{
		backgroundWorkerRandom: func() float64 { return 0 },
	}
	state := schedulerWorkState{
		ContentVerificationReady: true,
	}

	assignment, ok := runtime.nextBackgroundWorkerAssignment(
		context.Background(),
		1,
		false,
		semanticIndexingSchedule{},
		false,
		state,
		true,
	)
	if !ok || assignment.phase != "content_verification" || assignment.workers != 1 {
		t.Fatalf("assignment = %+v, %t, want one-worker content verification slice", assignment, ok)
	}

	// A newly discovered file must finish settling before verification starts.
	// The scheduler's existing MetadataNextEligibleAt timer will wake it when
	// the metadata job becomes runnable.
	state.MetadataSettling = 1
	if assignment, ok := runtime.nextBackgroundWorkerAssignment(
		context.Background(),
		1,
		false,
		semanticIndexingSchedule{},
		false,
		state,
		true,
	); ok {
		t.Fatalf("assignment = %+v, want settling metadata to suppress content verification", assignment)
	}

	// A verification slice returns to the scheduler. Work that became runnable
	// while that file was being hashed must win the next priority decision.
	state.MetadataSettling = 0
	state.MetadataQueued = 1
	assignment, ok = runtime.nextBackgroundWorkerAssignment(
		context.Background(),
		1,
		false,
		semanticIndexingSchedule{},
		false,
		state,
		true,
	)
	if !ok || assignment.phase != "metadata" {
		t.Fatalf("assignment = %+v, %t, want new metadata before another verification slice", assignment, ok)
	}

	state.MetadataQueued = 0
	assignment, ok = runtime.nextBackgroundWorkerAssignment(
		context.Background(),
		1,
		false,
		semanticIndexingSchedule{},
		false,
		state,
		true,
	)
	if !ok || assignment.phase != "content_verification" {
		t.Fatalf("assignment = %+v, %t, want content verification after metadata drains", assignment, ok)
	}
}

func TestNotifyDatasourceMirrorSyncCompletedRefreshesAndWakesScheduler(t *testing.T) {
	runtime := &AgentRuntime{backgroundWorkerWake: make(chan struct{}, 1)}
	runtime.notifyDatasourceMirrorSyncCompleted()

	if !runtime.schedulerWorkState.Dirty {
		t.Fatal("scheduler work state Dirty = false, want mirror sync to invalidate cache")
	}
	select {
	case <-runtime.backgroundWorkerWake:
	default:
		t.Fatal("background worker scheduler was not woken after mirror sync")
	}
}

func TestApplySchedulerWorkStatePrefersSearchIndexWaitOverPendingPublishQueue(t *testing.T) {
	tasks := []DatasourceTaskStatus{{
		Phase:          "search_index",
		Label:          "Search index",
		QueuedTasks:    50,
		CompletedTasks: 1000,
		TotalTasks:     1050,
		Status:         "queued",
	}}

	applySchedulerWorkStateToDatasourceTasks(tasks, schedulerWorkState{
		SemanticScheduled:            true,
		SemanticCompletedVectors:     1050,
		SemanticIndexedVectors:       1000,
		SemanticMixedEmbeddingQueued: 450,
		SemanticPendingIndexJobs:     1,
		SemanticWaitingQueuedTarget:  200,
	})
	tasks = normalizeDatasourceTaskDependencies(tasks)

	got := tasks[0]
	if got.Status != "waiting" ||
		got.WaitingReason != datasourceTaskWaitingQueuedTarget ||
		got.WaitingQueuedTarget != 200 ||
		got.QueuedTasks != 50 {
		t.Fatalf("search index task = %+v, want waiting queued target over pending publish queue", got)
	}
}

func TestApplySchedulerWorkStateReplacesStaleEmbeddingFailureCount(t *testing.T) {
	tasks := []DatasourceTaskStatus{{
		Phase:       "embeddings",
		Label:       "Embeddings",
		FailedTasks: 2,
		Status:      "attention",
	}}

	applySchedulerWorkStateToDatasourceTasks(tasks, schedulerWorkState{
		SemanticScheduled:        true,
		SemanticEligibleVectors:  10,
		SemanticFailedVectors:    1,
		SemanticCompletedVectors: 0,
	})
	tasks = normalizeDatasourceTaskDependencies(tasks)

	got := tasks[0]
	if got.QueuedTasks != 9 || got.FailedTasks != 1 || got.TotalTasks != 10 {
		t.Fatalf("embeddings task = %+v, want current model queued/failed/total 9/1/10", got)
	}
}
