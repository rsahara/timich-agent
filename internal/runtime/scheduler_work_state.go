package runtime

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
)

const schedulerWorkStateStaleAfter = 30 * time.Minute

type schedulerWorkState struct {
	ConfigHash string
	UpdatedAt  time.Time
	Dirty      bool

	MetadataQueued           int
	MetadataSettling         int
	MetadataNextEligibleAt   *time.Time
	ThumbnailQueued          int
	ContentVerificationReady bool

	SemanticScheduled             bool
	SemanticRuntimeRetry          bool
	SemanticPriorityPublishReady  bool
	SemanticPublishReady          bool
	SemanticEmbeddingReady        bool
	SemanticEmbeddingBatchSize    int
	SemanticMixedEmbeddingQueued  int
	SemanticMixedEmbeddingHint    int
	SemanticMixedEmbeddingBatch   int
	SemanticEligibleVectors       int
	SemanticCompletedVectors      int
	SemanticFailedVectors         int
	SemanticIndexedVectors        int
	SemanticPendingIndexJobs      int
	SemanticFailedIndexJobs       int
	SemanticEligibleIndexJobs     int
	SemanticWaitingQueuedTarget   int
	SemanticNextEligibleAt        *time.Time
	SemanticSelectedModelID       string
	SemanticSelectedVectorSpaceID string
}

func (s schedulerWorkState) empty() bool {
	return s.UpdatedAt.IsZero() && s.ConfigHash == ""
}

func (s schedulerWorkState) fresh(now time.Time, configHash string, schedule semanticIndexingSchedule, semanticScheduled bool) bool {
	if s.empty() || s.Dirty || s.ConfigHash != configHash {
		return false
	}
	staleAfter := schedulerWorkStateStaleAfter
	if semanticScheduled &&
		schedule.Workers > 0 &&
		s.SemanticRuntimeRetry &&
		schedule.Interval > 0 {
		staleAfter = min(staleAfter, schedule.Interval)
	}
	if now.Sub(s.UpdatedAt) >= staleAfter {
		return false
	}
	if s.MetadataNextEligibleAt != nil && !s.MetadataNextEligibleAt.After(now) {
		return false
	}
	if s.SemanticNextEligibleAt != nil && !s.SemanticNextEligibleAt.After(now) {
		return false
	}
	return true
}

func (a *AgentRuntime) schedulerWorkStateSnapshot(ctx context.Context, schedule semanticIndexingSchedule, semanticScheduled bool) (schedulerWorkState, bool) {
	if a == nil {
		return schedulerWorkState{}, false
	}
	now := time.Now().UTC()
	configHash := a.schedulerWorkStateConfigHash(schedule, semanticScheduled)
	a.schedulerWorkStateMu.Lock()
	if a.schedulerWorkState.fresh(now, configHash, schedule, semanticScheduled) {
		snapshot := a.schedulerWorkState
		a.schedulerWorkStateMu.Unlock()
		return snapshot, true
	}
	startSeq := a.schedulerWorkStateSeq
	a.schedulerWorkStateMu.Unlock()

	recomputeCtx, release, err := a.foregroundCatalog.beginCancelableBackground(ctx)
	if err != nil {
		return schedulerWorkState{}, false
	}
	recomputed, err := a.recomputeSchedulerWorkState(recomputeCtx, schedule, semanticScheduled, configHash, now)
	recomputeCanceled := recomputeCtx.Err() != nil
	release()
	if err != nil {
		if ctx.Err() == nil && !recomputeCanceled {
			log.Printf("timich-agent scheduler work-state recompute failed error=%v", err)
		}
		return schedulerWorkState{}, false
	}

	a.schedulerWorkStateMu.Lock()
	defer a.schedulerWorkStateMu.Unlock()
	if a.schedulerWorkStateSeq != startSeq {
		a.schedulerWorkState.Dirty = true
		if !a.schedulerWorkState.empty() {
			return a.schedulerWorkState, true
		}
		return schedulerWorkState{}, false
	}
	a.schedulerWorkState = recomputed
	return recomputed, true
}

func (a *AgentRuntime) cachedSchedulerWorkStateForDisplay(schedule semanticIndexingSchedule, semanticScheduled bool) (schedulerWorkState, bool) {
	if a == nil {
		return schedulerWorkState{}, false
	}
	now := time.Now().UTC()
	configHash := a.schedulerWorkStateConfigHash(schedule, semanticScheduled)
	a.schedulerWorkStateMu.Lock()
	defer a.schedulerWorkStateMu.Unlock()
	if !a.schedulerWorkState.fresh(now, configHash, schedule, semanticScheduled) {
		return schedulerWorkState{}, false
	}
	return a.schedulerWorkState, true
}

func (a *AgentRuntime) schedulerWorkStateConfigHash(schedule semanticIndexingSchedule, semanticScheduled bool) string {
	if a == nil {
		return ""
	}
	return strings.Join([]string{
		a.datasourceIndexingConfigHash(),
		fmt.Sprintf("semantic=%t", semanticScheduled),
		fmt.Sprintf("interval=%s", schedule.Interval),
		fmt.Sprintf("batch=%d", schedule.BatchSize),
		fmt.Sprintf("workers=%d", schedule.Workers),
		fmt.Sprintf("target=%d", schedule.TargetCompletedVectors),
	}, "\x1f")
}

func (a *AgentRuntime) recomputeSchedulerWorkState(ctx context.Context, schedule semanticIndexingSchedule, semanticScheduled bool, configHash string, now time.Time) (schedulerWorkState, error) {
	catalogService := a.catalogService()
	if catalogService == nil {
		return schedulerWorkState{}, catalog.ErrNoDatasourceConfigured
	}
	state := schedulerWorkState{
		ConfigHash:        configHash,
		UpdatedAt:         now,
		SemanticScheduled: semanticScheduled,
	}
	if _, ok := a.localDatasourceScanSchedule(); ok {
		countCtx, cancel := context.WithTimeout(ctx, localWorkerCountTimeout)
		metadataState, err := catalogService.LocalMetadataQueueState(countCtx)
		cancel()
		if err != nil {
			return schedulerWorkState{}, fmt.Errorf("count local metadata jobs: %w", err)
		}
		state.MetadataQueued = max(metadataState.Queued, 0)
		state.MetadataSettling = max(metadataState.Settling, 0)
		state.MetadataNextEligibleAt = metadataState.NextEligibleAt

		countCtx, cancel = context.WithTimeout(ctx, localWorkerCountTimeout)
		thumbnailQueued, err := catalogService.PendingLocalThumbnailJobs(countCtx)
		cancel()
		if err != nil {
			return schedulerWorkState{}, fmt.Errorf("count local thumbnail jobs: %w", err)
		}
		state.ThumbnailQueued = max(thumbnailQueued, 0)

		countCtx, cancel = context.WithTimeout(ctx, localWorkerCountTimeout)
		runnableSourceKeys, err := catalogService.LocalContentVerificationRunnableSourceKeys(countCtx, now)
		cancel()
		if err != nil {
			return schedulerWorkState{}, fmt.Errorf("check local content verification: %w", err)
		}
		state.ContentVerificationReady = len(runnableSourceKeys) > 0
	}
	if (semanticScheduled && schedule.Workers > 0) || a.hasPendingSemanticIndexJobs(ctx, catalogService) {
		a.populateSemanticSchedulerWorkState(ctx, catalogService, schedule, &state)
	}
	return state, nil
}

func (a *AgentRuntime) hasPendingSemanticIndexJobs(ctx context.Context, catalogService *catalog.Service) bool {
	if a == nil || catalogService == nil || a.effectiveHeavyTaskWorkers() <= 0 {
		return false
	}
	pending, err := catalogService.PendingSemanticIndexJobs(ctx)
	return err == nil && pending
}

func (a *AgentRuntime) populateSemanticSchedulerWorkState(ctx context.Context, catalogService *catalog.Service, schedule semanticIndexingSchedule, state *schedulerWorkState) {
	if a == nil || a.semanticModels == nil || catalogService == nil || state == nil {
		return
	}
	// The scheduler derives coverage and publication decisions from the single
	// snapshot below. Starting from the installed-only registry avoids running
	// the same large-catalog counts while selecting a candidate first.
	status := a.SemanticModelRegistryInstalledStatusWithContext(ctx)
	profiles := semanticBackfillCandidateProfiles(ctx, status, a.semanticModels)
	snapshot := loadSemanticSchedulerSnapshot(ctx, profiles, catalogService.SemanticModelBackfillSnapshot)
	statusLookup := snapshot.backfillStatus
	state.SemanticRuntimeRetry = semanticSchedulerRuntimeRetryNeeded(ctx, status, a.semanticModels, a.semanticONNXRuntime.Status())
	state.SemanticNextEligibleAt = semanticSchedulerNextEligibleAtWithLookup(profiles, statusLookup)
	state.SemanticMixedEmbeddingBatch = semanticMixedEmbeddingBatchForReadyRuntime(ctx, status, a.semanticModels, schedule)
	state.SemanticPriorityPublishReady = semanticCandidateNeedingPriorityIndexPublishWithLookup(profiles, statusLookup) != nil
	state.SemanticPublishReady = semanticCandidateNeedingIndexPublishWithLookup(profiles, func(profile catalog.SemanticModelProfileStatus) (bool, int, error) {
		return snapshot.indexPublishNeed(ctx, catalogService, a.semanticModels, profile)
	}) != nil
	if candidate, backfill := semanticCandidateNeedingMixedBackfillWithLookup(profiles, schedule, statusLookup); candidate != nil && backfill != nil {
		if batchSize, ok := semanticMixedIndexingBatchSizeForStatus(schedule, *backfill); ok {
			state.SemanticMixedEmbeddingQueued = semanticIndexEmbeddingQueued(*backfill)
			state.SemanticMixedEmbeddingBatch = batchSize
			state.applySemanticBackfillStatus(*candidate, *backfill)
		}
	}
	if candidate := semanticCandidateNeedingBackfillWithLookup(profiles, statusLookup); candidate != nil {
		if schedule.TargetCompletedVectors <= 0 {
			state.SemanticEmbeddingReady = schedule.BatchSize > 0
			state.SemanticEmbeddingBatchSize = schedule.BatchSize
			if backfill, err := statusLookup(*candidate); err == nil && backfill != nil {
				state.applySemanticBackfillStatus(*candidate, *backfill)
			}
			return
		}
		if backfill, err := statusLookup(*candidate); err == nil && backfill != nil {
			if batchSize, ok := semanticIndexingBatchSizeForStatus(schedule, backfill.CompletedVectorCount); ok {
				state.SemanticEmbeddingReady = true
				state.SemanticEmbeddingBatchSize = batchSize
			}
			state.applySemanticBackfillStatus(*candidate, *backfill)
		}
	}
}

func semanticSchedulerNextEligibleAt(ctx context.Context, catalogService *catalog.Service, status catalog.SemanticModelRegistryStatus, modelStore *catalog.SemanticModelPackStore) *time.Time {
	if catalogService == nil {
		return nil
	}
	profiles := semanticBackfillCandidateProfiles(ctx, status, modelStore)
	return semanticSchedulerNextEligibleAtWithLookup(profiles, func(profile catalog.SemanticModelProfileStatus) (*catalog.SemanticModelBackfillStatus, error) {
		return catalogService.SemanticModelBackfillStatus(ctx, profile)
	})
}

func semanticSchedulerNextEligibleAtWithLookup(profiles []catalog.SemanticModelProfileStatus, lookup semanticBackfillStatusLookup) *time.Time {
	var nextEligibleAt *time.Time
	for _, profile := range profiles {
		if semanticBackfillRolePriority(profile) == 0 ||
			profile.Runtime == nil ||
			!profile.Runtime.Loaded ||
			!profile.Runtime.CanEmbed ||
			lookup == nil {
			continue
		}
		backfill, err := lookup(profile)
		if err != nil || backfill == nil || backfill.NextEligibleAt == nil {
			continue
		}
		if nextEligibleAt == nil || backfill.NextEligibleAt.Before(*nextEligibleAt) {
			value := backfill.NextEligibleAt.UTC()
			nextEligibleAt = &value
		}
	}
	return nextEligibleAt
}

func semanticSchedulerRuntimeRetryNeeded(ctx context.Context, status catalog.SemanticModelRegistryStatus, modelStore *catalog.SemanticModelPackStore, onnxStatus SemanticONNXRuntimeResponse) bool {
	if onnxStatus.Status != semanticONNXRuntimeStatusRunning || onnxStatus.ProcessCount <= 0 {
		return false
	}
	for _, profile := range semanticBackfillCandidateProfiles(ctx, status, modelStore) {
		if semanticBackfillRolePriority(profile) == 0 ||
			profile.Runtime == nil ||
			strings.TrimSpace(profile.Runtime.Runtime) != semanticONNXRuntimeName {
			continue
		}
		if !profile.Runtime.Loaded || !profile.Runtime.CanEmbed {
			return true
		}
	}
	return false
}

func semanticMixedEmbeddingBatchForReadyRuntime(ctx context.Context, status catalog.SemanticModelRegistryStatus, modelStore *catalog.SemanticModelPackStore, schedule semanticIndexingSchedule) int {
	if modelStore == nil || schedule.BatchSize <= 0 || schedule.TargetCompletedVectors > 0 {
		return 0
	}
	for _, profile := range semanticBackfillCandidateProfiles(ctx, status, modelStore) {
		if semanticBackfillRolePriority(profile) > 0 &&
			profile.Runtime != nil &&
			profile.Runtime.Loaded &&
			profile.Runtime.CanEmbed {
			return schedule.BatchSize
		}
	}
	return 0
}

func (s *schedulerWorkState) applySemanticBackfillStatus(candidate catalog.SemanticModelProfileStatus, status catalog.SemanticModelBackfillStatus) {
	if s == nil {
		return
	}
	s.SemanticSelectedModelID = candidate.ModelID
	s.SemanticSelectedVectorSpaceID = candidate.VectorSpaceID
	s.SemanticEligibleVectors = max(status.EligibleAssetCount, 0)
	s.SemanticCompletedVectors = max(status.CompletedVectorCount, 0)
	s.SemanticFailedVectors = max(status.FailedVectorCount, 0)
	s.SemanticIndexedVectors = max(status.IndexedVectorCount, 0)
	s.SemanticPendingIndexJobs = max(status.PendingIndexJobCount, 0)
	s.SemanticFailedIndexJobs = max(status.FailedIndexJobCount, 0)
	s.SemanticEligibleIndexJobs = max(status.EligibleIndexJobCount, 0)
	s.SemanticNextEligibleAt = status.NextEligibleAt
	embeddingQueued := semanticIndexEmbeddingQueued(status)
	if embeddingQueued > s.SemanticMixedEmbeddingQueued {
		s.SemanticMixedEmbeddingQueued = embeddingQueued
	}
	publishQueued := semanticIndexPublishQueued(status)
	s.SemanticWaitingQueuedTarget = 0
	if target, ok := semanticIndexPartialPublishWaitTarget(status.IndexedVectorCount, publishQueued, embeddingQueued, status.FailedIndexJobCount); ok {
		s.SemanticWaitingQueuedTarget = target
	}
}

func (s schedulerWorkState) semanticMixedEmbeddingScheduledQueued() int {
	return max(s.SemanticMixedEmbeddingQueued, 0) + max(s.SemanticMixedEmbeddingHint, 0)
}

func (a *AgentRuntime) schedulerWorkStateAssign(phase string, planned int) {
	if a == nil || planned <= 0 {
		return
	}
	a.schedulerWorkStateMu.Lock()
	defer a.schedulerWorkStateMu.Unlock()
	switch phase {
	case "metadata":
		a.schedulerWorkState.MetadataQueued = max(a.schedulerWorkState.MetadataQueued-planned, 0)
	case "thumbnails":
		a.schedulerWorkState.ThumbnailQueued = max(a.schedulerWorkState.ThumbnailQueued-planned, 0)
	case "embeddings":
		a.schedulerWorkState.SemanticMixedEmbeddingQueued = max(a.schedulerWorkState.SemanticMixedEmbeddingQueued-planned, 0)
	case "content_verification":
		a.schedulerWorkState.ContentVerificationReady = false
	}
	a.schedulerWorkStateSeq++
}

func (a *AgentRuntime) schedulerWorkStateRelease(phase string, count int) {
	if a == nil || count <= 0 {
		return
	}
	a.schedulerWorkStateMu.Lock()
	defer a.schedulerWorkStateMu.Unlock()
	switch phase {
	case "metadata":
		a.schedulerWorkState.MetadataQueued += count
	case "thumbnails":
		a.schedulerWorkState.ThumbnailQueued += count
	case "embeddings":
		a.schedulerWorkState.SemanticMixedEmbeddingQueued += count
	}
	a.schedulerWorkStateSeq++
}

func (a *AgentRuntime) schedulerWorkStateDiscardMixedEmbeddingWork() {
	if a == nil {
		return
	}
	a.schedulerWorkStateMu.Lock()
	defer a.schedulerWorkStateMu.Unlock()
	// A mixed embedding selection always validates the real catalog before
	// processing. If that lookup finds no work, neither a stale exact count nor
	// its thumbnail-derived hint may be returned to the scheduler as work.
	// Mark the state dirty so the next decision reconciles from the catalog.
	a.schedulerWorkState.SemanticMixedEmbeddingQueued = 0
	a.schedulerWorkState.SemanticMixedEmbeddingHint = 0
	a.schedulerWorkState.Dirty = true
	a.schedulerWorkStateSeq++
}

func (a *AgentRuntime) schedulerWorkStateDeferScheduledEmbeddingWork(nextEligibleAt time.Time) {
	if a == nil {
		return
	}
	a.schedulerWorkStateMu.Lock()
	defer a.schedulerWorkStateMu.Unlock()
	a.schedulerWorkState.SemanticEmbeddingReady = false
	a.schedulerWorkState.SemanticEmbeddingBatchSize = 0
	utc := nextEligibleAt.UTC()
	if a.schedulerWorkState.SemanticNextEligibleAt == nil ||
		!a.schedulerWorkState.SemanticNextEligibleAt.After(time.Now().UTC()) ||
		utc.Before(*a.schedulerWorkState.SemanticNextEligibleAt) {
		a.schedulerWorkState.SemanticNextEligibleAt = &utc
	}
	a.schedulerWorkStateSeq++
}

func (a *AgentRuntime) schedulerWorkStateCompleteLocalMetadata(result catalog.LocalMetadataBatchResult, planned int) {
	if a == nil {
		return
	}
	a.schedulerWorkStateMu.Lock()
	defer a.schedulerWorkStateMu.Unlock()
	if planned > 0 {
		if remainder := planned - result.ProcessedJobs; remainder > 0 {
			a.schedulerWorkState.MetadataQueued += remainder
		}
	} else {
		a.schedulerWorkState.MetadataQueued = max(a.schedulerWorkState.MetadataQueued-result.ProcessedJobs, 0)
	}
	a.schedulerWorkState.ThumbnailQueued += max(result.RegisteredAssets, 0)
	if result.DeferredJobs > 0 || result.SettlingJobs > 0 || (result.ProcessedJobs == 0 && planned > 0) {
		a.schedulerWorkState.Dirty = true
	}
	a.schedulerWorkStateSeq++
}

func (a *AgentRuntime) schedulerWorkStateCompleteLocalThumbnail(result catalog.LocalThumbnailBatchResult, planned int) {
	if a == nil {
		return
	}
	a.schedulerWorkStateMu.Lock()
	defer a.schedulerWorkStateMu.Unlock()
	if planned > 0 {
		if remainder := planned - result.ProcessedJobs; remainder > 0 {
			a.schedulerWorkState.ThumbnailQueued += remainder
		}
	} else {
		a.schedulerWorkState.ThumbnailQueued = max(a.schedulerWorkState.ThumbnailQueued-result.ProcessedJobs, 0)
	}
	// A successful Local image rendition is a new candidate for the current
	// image embedding model. Keep that delta in memory for weighted scheduling;
	// the embedding worker validates and reconciles the exact catalog work only
	// when it is selected.
	if result.GeneratedImageAssets > 0 && a.schedulerWorkState.SemanticMixedEmbeddingBatch > 0 {
		a.schedulerWorkState.SemanticMixedEmbeddingHint += result.GeneratedImageAssets
	}
	if result.DeferredJobs > 0 || result.ResettledAssets > 0 || (result.ProcessedJobs == 0 && planned > 0) {
		a.schedulerWorkState.Dirty = true
	}
	a.schedulerWorkStateSeq++
}

func (a *AgentRuntime) schedulerWorkStateApplySemanticStatus(status catalog.SemanticModelBackfillStatus) {
	a.schedulerWorkStateApplySemanticStatusAfterEmbedding(status, 0)
}

func (a *AgentRuntime) schedulerWorkStateEmbeddingHintSnapshot() int {
	if a == nil {
		return 0
	}
	a.schedulerWorkStateMu.Lock()
	defer a.schedulerWorkStateMu.Unlock()
	return max(a.schedulerWorkState.SemanticMixedEmbeddingHint, 0)
}

func (a *AgentRuntime) schedulerWorkStateApplySemanticStatusAfterEmbedding(status catalog.SemanticModelBackfillStatus, consumedHint int) {
	if a == nil {
		return
	}
	a.schedulerWorkStateMu.Lock()
	defer a.schedulerWorkStateMu.Unlock()
	a.schedulerWorkState.SemanticEligibleVectors = max(status.EligibleAssetCount, 0)
	a.schedulerWorkState.SemanticCompletedVectors = max(status.CompletedVectorCount, 0)
	a.schedulerWorkState.SemanticFailedVectors = max(status.FailedVectorCount, 0)
	a.schedulerWorkState.SemanticIndexedVectors = max(status.IndexedVectorCount, 0)
	a.schedulerWorkState.SemanticPendingIndexJobs = max(status.PendingIndexJobCount, 0)
	a.schedulerWorkState.SemanticFailedIndexJobs = max(status.FailedIndexJobCount, 0)
	a.schedulerWorkState.SemanticEligibleIndexJobs = max(status.EligibleIndexJobCount, 0)
	a.schedulerWorkState.SemanticNextEligibleAt = status.NextEligibleAt
	a.schedulerWorkState.SemanticMixedEmbeddingQueued = semanticIndexEmbeddingQueued(status)
	// Only hints observed before this embedding run started can be represented
	// by its catalog status. Keep any thumbnail work that arrived in parallel.
	a.schedulerWorkState.SemanticMixedEmbeddingHint = max(a.schedulerWorkState.SemanticMixedEmbeddingHint-max(consumedHint, 0), 0)
	if a.schedulerWorkState.SemanticMixedEmbeddingQueued <= 0 {
		a.schedulerWorkState.SemanticEmbeddingReady = false
		a.schedulerWorkState.SemanticEmbeddingBatchSize = 0
	}
	publishQueued := semanticIndexPublishQueued(status)
	hasIndexJobs := status.PendingIndexJobCount+status.FailedIndexJobCount > 0
	a.schedulerWorkState.SemanticPriorityPublishReady = semanticPriorityIndexPublishDue(status) && (!hasIndexJobs || status.EligibleIndexJobCount > 0)
	a.schedulerWorkState.SemanticPublishReady = status.EligibleIndexJobCount > 0
	a.schedulerWorkState.SemanticWaitingQueuedTarget = 0
	if target, ok := semanticIndexPartialPublishWaitTarget(status.IndexedVectorCount, publishQueued, a.schedulerWorkState.SemanticMixedEmbeddingQueued, status.FailedIndexJobCount); ok {
		a.schedulerWorkState.SemanticWaitingQueuedTarget = target
	}
	a.schedulerWorkStateSeq++
}

func (a *AgentRuntime) schedulerWorkStateMarkDirty() {
	if a == nil {
		return
	}
	a.schedulerWorkStateMu.Lock()
	a.schedulerWorkState.Dirty = true
	a.schedulerWorkStateSeq++
	a.schedulerWorkStateMu.Unlock()
}
