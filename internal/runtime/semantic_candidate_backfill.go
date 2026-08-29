package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
)

const (
	defaultSemanticIndexingInterval  = 30 * time.Second
	defaultSemanticIndexingBatchSize = 100
	semanticIndexingMaxBatchSize     = 100
	semanticIndexingTimeout          = 10 * time.Minute
	// During continuous embedding backfill, rebuild after one fifth of the
	// published corpus is ready. A drained embedding queue still takes the
	// normal publish path below this threshold.
	semanticIndexPartialPublishDivisor = 5
	backgroundWorkerActiveDelay        = 500 * time.Millisecond
	mixedMetadataWeightMultiplier      = 3.0
	mixedThumbnailWeightMultiplier     = 5.0
	mixedMetadataQueueWeightCap        = 300
	mixedThumbnailQueueWeightCap       = 300
	mixedEmbeddingQueueWeightCap       = 500
)

type semanticIndexingSchedule struct {
	Interval               time.Duration
	BatchSize              int
	Workers                int
	TargetCompletedVectors int
}

type backgroundWorkerAssignment struct {
	phase   string
	workers int
	planned int
	run     func(context.Context) bool
}

// StartBackgroundWorkerScheduler starts the unified heavy background worker
// scheduler. Media discovery runs outside this scheduler; metadata and later
// datasource processing phases are assigned here.
func (a *AgentRuntime) StartBackgroundWorkerScheduler() bool {
	if a == nil {
		return false
	}
	if !a.backgroundWorkerSchedulerConfigured() {
		return false
	}
	a.semanticBackfillMu.Lock()
	defer a.semanticBackfillMu.Unlock()
	if a.semanticBackfillCancel != nil {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	wake := make(chan struct{}, 1)
	a.semanticBackfillCancel = cancel
	a.backgroundWorkerWake = wake
	if catalogService := a.catalogService(); catalogService != nil {
		resetCtx, resetCancel := context.WithTimeout(ctx, 5*time.Second)
		if count, err := catalogService.ResetRunningSemanticIndexJobs(resetCtx); err == nil && count > 0 {
			log.Printf("timich-agent semantic hnsw publish reset running jobs count=%d", count)
		}
		resetCancel()
	}
	a.semanticBackfillWG.Add(1)
	go a.runBackgroundWorkerSchedulerLoop(ctx, wake)
	log.Printf("timich-agent background worker scheduler started workers=%d idle_interval=%s", a.effectiveHeavyTaskWorkers(), a.backgroundWorkerIdleInterval())
	return true
}

func (a *AgentRuntime) syncBackgroundWorkerScheduler() {
	if a.backgroundWorkerSchedulerConfigured() {
		if !a.StartBackgroundWorkerScheduler() {
			a.wakeBackgroundWorkerScheduler()
		}
	} else {
		a.stopBackgroundWorkerScheduler()
	}
}

func (a *AgentRuntime) stopBackgroundWorkerScheduler() {
	a.semanticBackfillMu.Lock()
	cancel := a.semanticBackfillCancel
	a.semanticBackfillCancel = nil
	a.backgroundWorkerWake = nil
	a.semanticBackfillMu.Unlock()
	if cancel != nil {
		cancel()
		a.semanticBackfillWG.Wait()
	}
	a.backgroundWorkerMu.Lock()
	a.backgroundWorkerActive = nil
	a.backgroundWorkerMu.Unlock()
	a.setSemanticIndexingNextRunAt(nil)
	a.setSemanticIndexingRetryNotBefore(nil)
	a.setSemanticPublishRetryNotBefore(nil)
	a.clearLocalBackgroundWorkerRetries()
}

func (a *AgentRuntime) wakeBackgroundWorkerScheduler() {
	if a == nil {
		return
	}
	a.semanticBackfillMu.Lock()
	wake := a.backgroundWorkerWake
	a.semanticBackfillMu.Unlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (a *AgentRuntime) backgroundWorkerSchedulerConfigured() bool {
	if a == nil {
		return false
	}
	if _, ok := a.localDatasourceScanSchedule(); ok {
		return true
	}
	if _, ok := a.semanticIndexingSchedule(); ok {
		return true
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), localWorkerCountTimeout)
	defer cancel()
	pending, err := catalogService.PendingSemanticIndexJobs(ctx)
	return err == nil && pending
}

func (a *AgentRuntime) backgroundWorkerIdleInterval() time.Duration {
	interval := schedulerWorkStateStaleAfter
	if _, ok := a.localDatasourceScanSchedule(); ok {
		a.schedulerWorkStateMu.Lock()
		state := a.schedulerWorkState
		a.schedulerWorkStateMu.Unlock()
		now := time.Now().UTC()
		metadataDeferred := a.localBackgroundWorkerRetryDeferred("metadata", now)
		thumbnailDeferred := a.localBackgroundWorkerRetryDeferred("thumbnails", now)
		if (state.MetadataQueued > 0 && !metadataDeferred) ||
			(state.ThumbnailQueued > 0 && !thumbnailDeferred) ||
			state.Dirty ||
			state.empty() {
			interval = localMetadataBatchInterval
		} else if state.MetadataNextEligibleAt != nil {
			until := time.Until(*state.MetadataNextEligibleAt)
			if until <= 0 {
				if a.effectiveHeavyTaskWorkers() > 0 {
					return 0
				}
			} else if until < interval {
				interval = until
			}
		} else if remaining := schedulerWorkStateStaleAfter - time.Since(state.UpdatedAt); remaining > 0 && remaining < interval {
			interval = remaining
		}
	}
	a.schedulerWorkStateMu.Lock()
	semanticNextEligibleAt := a.schedulerWorkState.SemanticNextEligibleAt
	a.schedulerWorkStateMu.Unlock()
	if semanticNextEligibleAt != nil {
		until := time.Until(*semanticNextEligibleAt)
		if until <= 0 {
			if a.effectiveHeavyTaskWorkers() > 0 {
				return 0
			}
		} else if until < interval {
			interval = until
		}
	}
	if retryNotBefore := a.nextLocalBackgroundWorkerRetryNotBeforeAt(); retryNotBefore != nil {
		if until := time.Until(*retryNotBefore); until > 0 && until < interval {
			interval = until
		}
	}
	if schedule, ok := a.semanticIndexingSchedule(); ok && schedule.Interval > 0 && (interval == 0 || schedule.Interval < interval) {
		interval = schedule.Interval
	}
	if interval <= 0 {
		return localMetadataBatchInterval
	}
	return interval
}

func (a *AgentRuntime) runBackgroundWorkerSchedulerLoop(ctx context.Context, wake <-chan struct{}) {
	defer func() {
		a.semanticBackfillMu.Lock()
		if a.backgroundWorkerWake == wake {
			a.semanticBackfillCancel = nil
			a.backgroundWorkerWake = nil
		}
		a.semanticBackfillMu.Unlock()
		a.semanticBackfillWG.Done()
	}()

	timer := time.NewTimer(0)
	defer timer.Stop()
	resetTimer := func(delay time.Duration) {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-timer.C:
		}
		if !a.backgroundWorkerSchedulerConfigured() {
			return
		}
		delay := a.backgroundWorkerIdleInterval()
		if a.effectiveHeavyTaskWorkers() > 0 && a.startBackgroundWorkerAssignments(ctx) > 0 {
			delay = backgroundWorkerActiveDelay
		} else {
			delay = a.backgroundWorkerIdleInterval()
		}
		resetTimer(delay)
	}
}

func (a *AgentRuntime) runNextBackgroundWorkerTask(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	schedule, semanticScheduled := a.semanticIndexingSchedule()
	state, stateOK := a.schedulerWorkStateSnapshot(ctx, semanticSingleWorkerSchedule(schedule), semanticScheduled)
	assignment, ok := a.nextBackgroundWorkerAssignment(ctx, max(a.effectiveHeavyTaskWorkers(), 1), false, schedule, semanticScheduled, state, stateOK)
	if !ok {
		return false
	}
	a.schedulerWorkStateAssign(assignment.phase, assignment.planned)
	if assignment.phase == "embeddings" || assignment.phase == "search_index" || assignment.phase == "content_verification" {
		return a.runForegroundCancelableBackgroundAssignment(ctx, assignment.run)
	}
	return assignment.run(ctx)
}

func (a *AgentRuntime) startBackgroundWorkerAssignments(ctx context.Context) int {
	if a == nil || ctx.Err() != nil {
		return 0
	}
	maxWorkers := a.effectiveHeavyTaskWorkers()
	if maxWorkers <= 0 {
		return 0
	}
	started := 0
	for {
		activeWorkers, semanticActive := a.backgroundWorkerActiveState()
		availableWorkers := maxWorkers - activeWorkers
		if availableWorkers <= 0 {
			return started
		}
		schedule, semanticScheduled := a.semanticIndexingSchedule()
		state, stateOK := a.schedulerWorkStateSnapshot(ctx, semanticSingleWorkerSchedule(schedule), semanticScheduled)
		assignment, ok := a.nextBackgroundWorkerAssignment(ctx, availableWorkers, semanticActive, schedule, semanticScheduled, state, stateOK)
		if !ok {
			return started
		}
		assignment.workers = min(max(assignment.workers, 1), availableWorkers)
		assignment.planned = max(assignment.planned, 0)
		if !a.launchBackgroundWorkerAssignment(ctx, assignment) {
			return started
		}
		started++
	}
}

func (a *AgentRuntime) nextBackgroundWorkerAssignment(ctx context.Context, availableWorkers int, semanticActive bool, schedule semanticIndexingSchedule, semanticScheduled bool, state schedulerWorkState, stateOK bool) (backgroundWorkerAssignment, bool) {
	if availableWorkers <= 0 || !stateOK {
		return backgroundWorkerAssignment{}, false
	}
	now := time.Now().UTC()
	semanticEmbeddingDeferred := false
	if retryNotBefore := a.semanticIndexingRetryNotBeforeAt(); retryNotBefore != nil {
		semanticEmbeddingDeferred = retryNotBefore.After(now)
	}
	semanticPublishDeferred := false
	if retryNotBefore := a.semanticPublishRetryNotBeforeAt(); retryNotBefore != nil {
		semanticPublishDeferred = retryNotBefore.After(now)
	}
	metadataDeferred := a.localBackgroundWorkerRetryDeferred("metadata", now)
	thumbnailDeferred := a.localBackgroundWorkerRetryDeferred("thumbnails", now)
	contentVerificationDeferred := a.localBackgroundWorkerRetryDeferred("content_verification", now)
	singleWorkerSchedule := semanticSingleWorkerSchedule(schedule)
	if singleWorkerSchedule.Workers <= 0 {
		singleWorkerSchedule.Workers = 1
	}
	if !semanticActive && !semanticPublishDeferred && state.SemanticPriorityPublishReady {
		return backgroundWorkerAssignment{
			phase:   "search_index",
			workers: 1,
			run: func(ctx context.Context) bool {
				return a.runPrioritySemanticIndexPublish(ctx, singleWorkerSchedule)
			},
		}, true
	}
	mixedState := state
	if metadataDeferred {
		mixedState.MetadataQueued = 0
	}
	if thumbnailDeferred {
		mixedState.ThumbnailQueued = 0
	}
	if assignment, ok := a.mixedBackgroundWorkerAssignment(availableWorkers, !semanticActive && semanticScheduled && !semanticEmbeddingDeferred, singleWorkerSchedule, mixedState); ok {
		return assignment, true
	}
	if !semanticActive && semanticScheduled && !semanticEmbeddingDeferred && state.SemanticEmbeddingReady {
		batchSize := max(state.SemanticEmbeddingBatchSize, 0)
		return backgroundWorkerAssignment{
			phase:   "embeddings",
			workers: 1,
			planned: batchSize,
			run: func(ctx context.Context) bool {
				processed := a.runScheduledSemanticIndexing(ctx, singleWorkerSchedule)
				if processed && singleWorkerSchedule.Interval > 0 {
					next := time.Now().UTC().Add(singleWorkerSchedule.Interval)
					a.setSemanticIndexingNextRunAt(&next)
				}
				return processed
			},
		}, true
	}
	if !semanticActive && !semanticPublishDeferred && state.SemanticPublishReady {
		return backgroundWorkerAssignment{
			phase:   "search_index",
			workers: 1,
			run: func(ctx context.Context) bool {
				return a.runScheduledSemanticIndexPublish(ctx, singleWorkerSchedule)
			},
		}, true
	}
	// Content verification is deliberately lowest priority: reconciliation can
	// enqueue metadata, thumbnails, and semantic work that makes new media
	// browsable. Do not start a potentially long file hash while metadata is
	// still settling; its existing eligibility timer will wake the scheduler.
	// After settling and runnable work drain, each verification assignment checks
	// one Location total so priorities are decided again before another source.
	if !contentVerificationDeferred && state.ContentVerificationReady && state.MetadataSettling == 0 {
		return backgroundWorkerAssignment{
			phase:   "content_verification",
			workers: 1,
			planned: 1,
			run: func(ctx context.Context) bool {
				return a.runScheduledLocalContentVerification(ctx)
			},
		}, true
	}
	return backgroundWorkerAssignment{}, false
}

func (a *AgentRuntime) mixedBackgroundWorkerAssignment(availableWorkers int, semanticAvailable bool, schedule semanticIndexingSchedule, state schedulerWorkState) (backgroundWorkerAssignment, bool) {
	if availableWorkers <= 0 {
		return backgroundWorkerAssignment{}, false
	}
	metadataQueued := state.MetadataQueued
	thumbnailQueued := state.ThumbnailQueued
	embeddingQueued := state.semanticMixedEmbeddingScheduledQueued()
	embeddingBatchSize := state.SemanticMixedEmbeddingBatch
	if semanticAvailable {
		embeddingQueued = state.semanticMixedEmbeddingScheduledQueued()
		embeddingBatchSize = state.SemanticMixedEmbeddingBatch
	} else {
		embeddingQueued = 0
		embeddingBatchSize = 0
	}
	phase := chooseMixedBackgroundWorkerPhase(metadataQueued, thumbnailQueued, embeddingQueued, embeddingBatchSize, a.backgroundWorkerRandomFloat64())
	switch phase {
	case "metadata":
		batchSize := min(localMetadataBatchSizeForWorkers(availableWorkers), max(metadataQueued, 0))
		return backgroundWorkerAssignment{
			phase:   phase,
			workers: availableWorkers,
			planned: batchSize,
			run: func(ctx context.Context) bool {
				return a.runScheduledLocalMetadataBatchWithWorkers(ctx, "worker_scheduler", availableWorkers, batchSize)
			},
		}, true
	case "thumbnails":
		batchSize := min(localThumbnailBatchSizeForWorkers(availableWorkers), max(thumbnailQueued, 0))
		return backgroundWorkerAssignment{
			phase:   phase,
			workers: availableWorkers,
			planned: batchSize,
			run: func(ctx context.Context) bool {
				return a.runScheduledLocalThumbnailBatchWithWorkers(ctx, "worker_scheduler", availableWorkers, batchSize)
			},
		}, true
	case "embeddings":
		batchSize := min(embeddingBatchSize, max(embeddingQueued, 0))
		return backgroundWorkerAssignment{
			phase:   phase,
			workers: 1,
			planned: batchSize,
			run: func(ctx context.Context) bool {
				processed := a.runMixedSemanticIndexing(ctx, schedule)
				if processed && schedule.Interval > 0 {
					next := time.Now().UTC().Add(schedule.Interval)
					a.setSemanticIndexingNextRunAt(&next)
				}
				return processed
			},
		}, true
	default:
		return backgroundWorkerAssignment{}, false
	}
}

func chooseMixedBackgroundWorkerPhase(metadataQueued int, thumbnailQueued int, embeddingQueued int, embeddingBatchSize int, randomValue float64) string {
	// An initial filesystem reconciliation can queue hundreds of thousands of
	// metadata jobs before the first thumbnail job appears. Letting that queue
	// size dominate the random weight starves other work and delays browsable or
	// searchable media. Bound each phase's contribution while retaining its
	// relative priority, so a very large backlog cannot monopolize workers.
	metadataWeight := float64(min(max(metadataQueued, 0), mixedMetadataQueueWeightCap)) * mixedMetadataWeightMultiplier
	thumbnailWeight := float64(min(max(thumbnailQueued, 0), mixedThumbnailQueueWeightCap)) * mixedThumbnailWeightMultiplier
	embeddingWeight := 0.0
	if embeddingBatchSize > 0 && embeddingQueued >= embeddingBatchSize {
		embeddingWeight = float64(min(max(embeddingQueued, 0), mixedEmbeddingQueueWeightCap))
	}
	total := metadataWeight + thumbnailWeight + embeddingWeight
	if total <= 0 {
		return ""
	}
	if randomValue < 0 {
		randomValue = 0
	}
	if randomValue >= 1 {
		randomValue = 0.999999999999
	}
	offset := randomValue * total
	if metadataWeight > 0 && offset < metadataWeight {
		return "metadata"
	}
	offset -= metadataWeight
	if thumbnailWeight > 0 && offset < thumbnailWeight {
		return "thumbnails"
	}
	if embeddingWeight > 0 {
		return "embeddings"
	}
	return ""
}

func (a *AgentRuntime) backgroundWorkerRandomFloat64() float64 {
	if a != nil && a.backgroundWorkerRandom != nil {
		return a.backgroundWorkerRandom()
	}
	return rand.Float64()
}

func semanticSingleWorkerSchedule(schedule semanticIndexingSchedule) semanticIndexingSchedule {
	if schedule.Workers > 1 {
		schedule.Workers = 1
	}
	return schedule
}

func (a *AgentRuntime) backgroundWorkerActiveState() (int, bool) {
	if a == nil {
		return 0, false
	}
	a.backgroundWorkerMu.Lock()
	defer a.backgroundWorkerMu.Unlock()
	total := 0
	semanticActive := false
	for phase, workers := range a.backgroundWorkerActive {
		workers = max(workers, 0)
		total += workers
		if phase == "embeddings" || phase == "search_index" {
			semanticActive = semanticActive || workers > 0
		}
	}
	return total, semanticActive
}

func (a *AgentRuntime) backgroundWorkerPhaseActive(phase string) bool {
	if a == nil {
		return false
	}
	a.backgroundWorkerMu.Lock()
	defer a.backgroundWorkerMu.Unlock()
	return a.backgroundWorkerActive[phase] > 0
}

func (a *AgentRuntime) launchBackgroundWorkerAssignment(ctx context.Context, assignment backgroundWorkerAssignment) bool {
	if a == nil || ctx.Err() != nil || assignment.phase == "" || assignment.workers <= 0 || assignment.run == nil {
		return false
	}
	a.backgroundWorkerMu.Lock()
	defer a.backgroundWorkerMu.Unlock()
	maxWorkers := a.effectiveHeavyTaskWorkers()
	if maxWorkers <= 0 {
		return false
	}
	if a.backgroundWorkerActive == nil {
		a.backgroundWorkerActive = map[string]int{}
	}
	if a.backgroundWorkerActive[assignment.phase] > 0 {
		return false
	}
	if (assignment.phase == "metadata" && a.backgroundWorkerActive["thumbnails"] > 0) ||
		(assignment.phase == "thumbnails" && a.backgroundWorkerActive["metadata"] > 0) {
		return false
	}
	if (assignment.phase == "embeddings" || assignment.phase == "search_index") &&
		(a.backgroundWorkerActive["embeddings"] > 0 || a.backgroundWorkerActive["search_index"] > 0) {
		return false
	}
	total := 0
	for _, activeWorkers := range a.backgroundWorkerActive {
		total += max(activeWorkers, 0)
	}
	if total+assignment.workers > maxWorkers {
		return false
	}
	a.backgroundWorkerActive[assignment.phase] = assignment.workers
	a.schedulerWorkStateAssign(assignment.phase, assignment.planned)
	a.semanticBackfillWG.Add(1)
	go func(task backgroundWorkerAssignment) {
		defer a.semanticBackfillWG.Done()
		defer a.finishBackgroundWorkerAssignment(task.phase, task.workers)
		if task.phase == "embeddings" || task.phase == "search_index" || task.phase == "content_verification" {
			_ = a.runForegroundCancelableBackgroundAssignment(ctx, task.run)
		} else {
			_ = task.run(ctx)
		}
		if ctx.Err() != nil {
			a.recoverCanceledLocalBackgroundWorkerAssignment(task.phase)
		}
	}(assignment)
	return true
}

func (a *AgentRuntime) finishBackgroundWorkerAssignment(phase string, workers int) {
	if a == nil || phase == "" {
		return
	}
	a.backgroundWorkerMu.Lock()
	if a.backgroundWorkerActive != nil {
		remaining := a.backgroundWorkerActive[phase] - max(workers, 1)
		if remaining > 0 {
			a.backgroundWorkerActive[phase] = remaining
		} else {
			delete(a.backgroundWorkerActive, phase)
		}
	}
	a.backgroundWorkerMu.Unlock()
	if phase == "content_verification" {
		a.notifyLocalContentVerificationSchedule()
	}
	a.wakeBackgroundWorkerScheduler()
}

func (a *AgentRuntime) prioritySemanticIndexPublishReady(ctx context.Context, schedule semanticIndexingSchedule) bool {
	if schedule.Workers <= 0 || a == nil || a.semanticModels == nil {
		return false
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return false
	}
	status := a.SemanticModelRegistryStatusWithContext(ctx)
	return semanticCandidateNeedingPriorityIndexPublish(ctx, catalogService, status, a.semanticModels) != nil
}

func (a *AgentRuntime) semanticIndexingReady(ctx context.Context, schedule semanticIndexingSchedule) bool {
	if schedule.Workers <= 0 {
		return false
	}
	_, ok := a.semanticIndexingWorkBatchSize(ctx, schedule)
	return ok
}

func (a *AgentRuntime) semanticIndexPublishReady(ctx context.Context, schedule semanticIndexingSchedule) bool {
	if schedule.Workers <= 0 || a == nil || a.semanticModels == nil {
		return false
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return false
	}
	status := a.SemanticModelRegistryStatusWithContext(ctx)
	if semanticCandidateNeedingIndexPublish(ctx, catalogService, status, a.semanticModels) != nil {
		return true
	}
	return false
}

func (a *AgentRuntime) runScheduledSemanticIndexing(ctx context.Context, schedule semanticIndexingSchedule) bool {
	if schedule.Workers <= 0 {
		return false
	}
	workBatchSize, ok := a.semanticIndexingWorkBatchSize(ctx, schedule)
	if !ok {
		a.deferScheduledSemanticIndexingNoWork(schedule)
		return false
	}
	backfillCtx, cancel := context.WithTimeout(ctx, semanticIndexingTimeout)
	defer cancel()
	result, err := a.backfillSemanticModelCandidate(backfillCtx, workBatchSize, catalog.SemanticModelBackfillOptions{
		Workers: schedule.Workers,
	})
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		if !errors.Is(err, ErrSemanticCandidateUnavailable) &&
			!errors.Is(err, ErrSemanticCandidateRuntimeUnavailable) &&
			!errors.Is(err, catalog.ErrCatalogNotConfigured) {
			log.Printf("timich-agent semantic indexing failed error=%v", err)
		}
		a.deferSemanticIndexingRetry(schedule)
		return false
	}
	a.setSemanticIndexingRetryNotBefore(nil)
	if result.ProcessedVectorCount <= 0 {
		return true
	}
	log.Printf(
		"timich-agent semantic indexing completed model=%s processed=%d indexed=%d remaining=%d",
		result.Status.ModelID,
		result.ProcessedVectorCount,
		result.IndexedVectorCount,
		result.Status.RemainingVectorCount,
	)
	return true
}

func (a *AgentRuntime) runMixedSemanticIndexing(ctx context.Context, schedule semanticIndexingSchedule) bool {
	if schedule.Workers <= 0 {
		return false
	}
	workBatchSize, ok := a.mixedSemanticIndexingWorkBatchSize(ctx, schedule)
	if !ok {
		a.schedulerWorkStateDiscardMixedEmbeddingWork()
		return false
	}
	backfillCtx, cancel := context.WithTimeout(ctx, semanticIndexingTimeout)
	defer cancel()
	result, err := a.backfillSemanticModelCandidate(backfillCtx, workBatchSize, catalog.SemanticModelBackfillOptions{
		Workers: schedule.Workers,
	})
	if err != nil {
		if ctx.Err() != nil {
			a.schedulerWorkStateDiscardMixedEmbeddingWork()
			return false
		}
		if !errors.Is(err, ErrSemanticCandidateUnavailable) &&
			!errors.Is(err, ErrSemanticCandidateRuntimeUnavailable) &&
			!errors.Is(err, catalog.ErrCatalogNotConfigured) {
			log.Printf("timich-agent semantic mixed indexing failed error=%v", err)
		}
		a.deferSemanticIndexingRetry(schedule)
		return false
	}
	a.setSemanticIndexingRetryNotBefore(nil)
	if result.ProcessedVectorCount <= 0 {
		return true
	}
	log.Printf(
		"timich-agent semantic mixed indexing completed model=%s processed=%d indexed=%d remaining=%d",
		result.Status.ModelID,
		result.ProcessedVectorCount,
		result.IndexedVectorCount,
		result.Status.RemainingVectorCount,
	)
	return true
}

func (a *AgentRuntime) deferSemanticIndexingRetry(schedule semanticIndexingSchedule) {
	interval := schedule.Interval
	if interval <= 0 {
		interval = defaultSemanticIndexingInterval
	}
	next := time.Now().UTC().Add(interval)
	a.setSemanticIndexingRetryNotBefore(&next)
	a.setSemanticIndexingNextRunAt(&next)
	a.schedulerWorkStateMarkDirty()
}

func (a *AgentRuntime) deferScheduledSemanticIndexingNoWork(schedule semanticIndexingSchedule) {
	interval := schedule.Interval
	if interval <= 0 {
		interval = defaultSemanticIndexingInterval
	}
	next := time.Now().UTC().Add(interval)
	a.setSemanticIndexingRetryNotBefore(&next)
	a.setSemanticIndexingNextRunAt(&next)
	a.schedulerWorkStateDeferScheduledEmbeddingWork(next)
}

func (a *AgentRuntime) deferSemanticIndexPublishRetry(schedule semanticIndexingSchedule) {
	interval := schedule.Interval
	if interval <= 0 {
		interval = defaultSemanticIndexingInterval
	}
	next := time.Now().UTC().Add(interval)
	a.setSemanticPublishRetryNotBefore(&next)
	a.schedulerWorkStateMarkDirty()
}

func (a *AgentRuntime) runScheduledSemanticIndexPublish(ctx context.Context, schedule semanticIndexingSchedule) bool {
	if schedule.Workers <= 0 || a == nil || a.semanticModels == nil {
		return false
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return false
	}
	a.semanticWorkMu.Lock()
	defer a.semanticWorkMu.Unlock()
	status := a.SemanticModelRegistryStatusWithContext(ctx)
	if err := a.reconcileScheduledSemanticIndexJobs(ctx, catalogService, status); err != nil {
		if ctx.Err() == nil {
			log.Printf("timich-agent semantic hnsw publish reconcile failed error=%v", err)
			a.deferSemanticIndexPublishRetry(schedule)
		}
		return false
	}
	candidate := semanticCandidateNeedingIndexPublish(ctx, catalogService, status, a.semanticModels)
	if candidate == nil {
		return false
	}
	clearActive := a.setSemanticIndexPublishActive(1)
	a.rememberDatasourceTaskActivitySnapshot(catalogService, "search_index", 1)
	clearedActive := false
	defer func() {
		if !clearedActive {
			clearActive()
			a.rememberDatasourceTaskActivitySnapshot(catalogService, "search_index", 0)
		}
	}()
	result, err := catalogService.PublishNextSemanticIndexJob(ctx, a.semanticModels, *candidate, nil)
	clearActive()
	clearedActive = true
	a.rememberDatasourceTaskActivitySnapshot(catalogService, "search_index", 0)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		if !errors.Is(err, ErrSemanticCandidateUnavailable) &&
			!errors.Is(err, catalog.ErrCatalogNotConfigured) &&
			!errors.Is(err, catalog.ErrSemanticModelPackInvalid) {
			log.Printf("timich-agent semantic hnsw publish failed error=%v", err)
		}
		a.deferSemanticIndexPublishRetry(schedule)
		return false
	}
	if !result.Published {
		a.rememberDatasourceTaskActivitySnapshot(catalogService, "search_index", 0)
		a.schedulerWorkStateMarkDirty()
		return false
	}
	// PublishNextSemanticIndexJob returns one datasource status. Recount the
	// aggregate before scheduling again so another source's eligible job is not
	// hidden by this source reaching ready.
	a.schedulerWorkStateMarkDirty()
	a.setSemanticPublishRetryNotBefore(nil)
	a.rememberSemanticIndexingProgressSnapshot(catalogService, []catalog.SemanticBackfillSource{{
		SourceKey: result.SourceKey,
		Status:    result.Status,
	}}, result.Status)
	log.Printf(
		"timich-agent semantic hnsw publish completed model=%s source=%s indexed=%d remaining=%d",
		result.Status.ModelID,
		result.SourceKey,
		result.IndexedVectorCount,
		result.Status.RemainingVectorCount,
	)
	return true
}

func (a *AgentRuntime) runPrioritySemanticIndexPublish(ctx context.Context, schedule semanticIndexingSchedule) bool {
	if schedule.Workers <= 0 || a == nil || a.semanticModels == nil {
		return false
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return false
	}
	a.semanticWorkMu.Lock()
	defer a.semanticWorkMu.Unlock()
	status := a.SemanticModelRegistryStatusWithContext(ctx)
	candidate := semanticCandidateNeedingPriorityIndexPublish(ctx, catalogService, status, a.semanticModels)
	if candidate == nil {
		return false
	}
	reconcileCtx, cancelReconcile := context.WithTimeout(ctx, semanticIndexingTimeout)
	_, err := catalogService.ReconcileSemanticIndexJobs(reconcileCtx, a.semanticModels, *candidate, nil, true)
	cancelReconcile()
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		if !errors.Is(err, catalog.ErrCatalogNotConfigured) &&
			!errors.Is(err, catalog.ErrSemanticModelPackInvalid) {
			log.Printf("timich-agent semantic priority hnsw publish reconcile failed model=%s error=%v", candidate.ModelID, err)
		}
		a.deferSemanticIndexPublishRetry(schedule)
		return false
	}
	clearActive := a.setSemanticIndexPublishActive(1)
	a.rememberDatasourceTaskActivitySnapshot(catalogService, "search_index", 1)
	clearedActive := false
	defer func() {
		if !clearedActive {
			clearActive()
			a.rememberDatasourceTaskActivitySnapshot(catalogService, "search_index", 0)
		}
	}()
	result, err := catalogService.PublishNextSemanticIndexJob(ctx, a.semanticModels, *candidate, nil)
	clearActive()
	clearedActive = true
	a.rememberDatasourceTaskActivitySnapshot(catalogService, "search_index", 0)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		if !errors.Is(err, ErrSemanticCandidateUnavailable) &&
			!errors.Is(err, catalog.ErrCatalogNotConfigured) &&
			!errors.Is(err, catalog.ErrSemanticModelPackInvalid) {
			log.Printf("timich-agent semantic priority hnsw publish failed model=%s error=%v", candidate.ModelID, err)
		}
		a.deferSemanticIndexPublishRetry(schedule)
		return false
	}
	if !result.Published {
		a.schedulerWorkStateMarkDirty()
		return false
	}
	a.schedulerWorkStateMarkDirty()
	a.setSemanticPublishRetryNotBefore(nil)
	a.rememberSemanticIndexingProgressSnapshot(catalogService, []catalog.SemanticBackfillSource{{
		SourceKey: result.SourceKey,
		Status:    result.Status,
	}}, result.Status)
	log.Printf(
		"timich-agent semantic priority hnsw publish completed model=%s source=%s indexed=%d remaining=%d",
		result.Status.ModelID,
		result.SourceKey,
		result.IndexedVectorCount,
		result.Status.RemainingVectorCount,
	)
	return true
}

func (a *AgentRuntime) reconcileScheduledSemanticIndexJobs(ctx context.Context, catalogService *catalog.Service, status catalog.SemanticModelRegistryStatus) error {
	if a == nil || a.semanticModels == nil || catalogService == nil {
		return nil
	}
	for _, profile := range semanticBackfillCandidateProfiles(ctx, status, a.semanticModels) {
		rolePriority := semanticBackfillRolePriority(profile)
		if rolePriority == 0 ||
			profile.Runtime == nil ||
			!profile.Runtime.Loaded ||
			!profile.Runtime.CanEmbed {
			continue
		}
		if _, err := catalogService.ReconcileSemanticIndexJobs(ctx, a.semanticModels, profile, nil, false); err != nil {
			if ctx.Err() != nil ||
				errors.Is(err, catalog.ErrCatalogNotConfigured) ||
				errors.Is(err, catalog.ErrSemanticModelPackInvalid) {
				continue
			}
			return fmt.Errorf("reconcile semantic hnsw publish jobs model=%s: %w", profile.ModelID, err)
		}
	}
	return nil
}

func (a *AgentRuntime) semanticDatasourceSourceKeysSnapshot() []string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	keys := make([]string, 0, len(a.config.Datasources))
	seen := map[string]struct{}{}
	for _, datasource := range a.config.Datasources {
		switch datasource.Kind {
		case config.DatasourceKindImmichIndexed:
		case config.DatasourceKindLocalFiles:
		default:
			continue
		}
		sourceKey := strings.TrimSpace(datasource.SourceKey)
		if sourceKey == "" {
			continue
		}
		if _, ok := seen[sourceKey]; ok {
			continue
		}
		seen[sourceKey] = struct{}{}
		keys = append(keys, sourceKey)
	}
	return keys
}

func semanticIndexPublishQueued(status catalog.SemanticModelBackfillStatus) int {
	return max(status.CompletedVectorCount-status.IndexedVectorCount, 0)
}

func semanticIndexEmbeddingQueued(status catalog.SemanticModelBackfillStatus) int {
	return max(status.EligibleNowVectorCount, 0)
}

func semanticIndexPartialPublishQueuedThreshold(indexedVectorCount int) int {
	if indexedVectorCount <= 0 {
		return 1
	}
	threshold := indexedVectorCount / semanticIndexPartialPublishDivisor
	if indexedVectorCount%semanticIndexPartialPublishDivisor != 0 {
		threshold++
	}
	return max(threshold, 1)
}

func semanticIndexPartialPublishQueuedTarget(indexedVectorCount, queuedVectorCount, queuedEmbeddingCount int) int {
	target := semanticIndexPartialPublishQueuedThreshold(indexedVectorCount)
	potential := max(queuedVectorCount, 0) + max(queuedEmbeddingCount, 0)
	if potential > 0 && potential < target {
		return potential
	}
	return target
}

func semanticIndexPartialPublishWaitTarget(indexedVectorCount, queuedVectorCount, queuedEmbeddingCount, failedIndexJobCount int) (int, bool) {
	queuedVectorCount = max(queuedVectorCount, 0)
	if queuedVectorCount <= 0 || queuedEmbeddingCount <= 0 || failedIndexJobCount > 0 {
		return 0, false
	}
	if queuedVectorCount >= semanticIndexPartialPublishQueuedThreshold(indexedVectorCount) {
		return 0, false
	}
	return semanticIndexPartialPublishQueuedTarget(indexedVectorCount, queuedVectorCount, queuedEmbeddingCount), true
}

func (a *AgentRuntime) semanticIndexingWorkBatchSize(ctx context.Context, schedule semanticIndexingSchedule) (int, bool) {
	if a == nil || a.semanticModels == nil {
		return 0, false
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return 0, false
	}
	status := a.SemanticModelRegistryStatusWithContext(ctx)
	candidate := semanticCandidateNeedingBackfill(ctx, catalogService, status, a.semanticModels)
	if candidate == nil {
		return 0, false
	}
	if schedule.TargetCompletedVectors <= 0 {
		return schedule.BatchSize, true
	}
	backfill, err := catalogService.SemanticModelBackfillStatus(ctx, *candidate)
	if err != nil || backfill == nil {
		return 0, false
	}
	return semanticIndexingBatchSizeForStatus(schedule, backfill.CompletedVectorCount)
}

func (a *AgentRuntime) mixedSemanticIndexingWork(ctx context.Context, schedule semanticIndexingSchedule) (int, int, bool) {
	if a == nil || a.semanticModels == nil {
		return 0, 0, false
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return 0, 0, false
	}
	status := a.SemanticModelRegistryStatusWithContext(ctx)
	candidate, backfill := semanticCandidateNeedingMixedBackfill(ctx, catalogService, status, a.semanticModels, schedule)
	if candidate == nil || backfill == nil {
		return 0, 0, false
	}
	batchSize, ok := semanticMixedIndexingBatchSizeForStatus(schedule, *backfill)
	if !ok {
		return 0, 0, false
	}
	return semanticIndexEmbeddingQueued(*backfill), batchSize, true
}

func (a *AgentRuntime) mixedSemanticIndexingWorkBatchSize(ctx context.Context, schedule semanticIndexingSchedule) (int, bool) {
	queued, batchSize, ok := a.mixedSemanticIndexingWork(ctx, schedule)
	if !ok {
		return 0, false
	}
	return min(batchSize, queued), true
}

func semanticIndexingBatchSizeForStatus(schedule semanticIndexingSchedule, completedVectorCount int) (int, bool) {
	if schedule.TargetCompletedVectors <= 0 {
		return schedule.BatchSize, schedule.BatchSize > 0
	}
	remainingToTarget := schedule.TargetCompletedVectors - completedVectorCount
	if remainingToTarget <= 0 {
		return 0, false
	}
	return min(schedule.BatchSize, remainingToTarget), true
}

func semanticMixedIndexingBatchSizeForStatus(schedule semanticIndexingSchedule, status catalog.SemanticModelBackfillStatus) (int, bool) {
	batchSize, ok := semanticIndexingBatchSizeForStatus(schedule, status.CompletedVectorCount)
	if !ok || batchSize <= 0 {
		return 0, false
	}
	queued := semanticIndexEmbeddingQueued(status)
	if queued < batchSize {
		return 0, false
	}
	return min(batchSize, queued), true
}

func (a *AgentRuntime) semanticIndexingSchedule() (semanticIndexingSchedule, bool) {
	if a == nil {
		return semanticIndexingSchedule{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	if !semanticIndexingScheduledForConfig(a.config.Config) {
		return semanticIndexingSchedule{}, false
	}
	backfill := a.config.SemanticRuntime.Indexing

	interval := defaultSemanticIndexingInterval
	if rawInterval := strings.TrimSpace(backfill.Interval); rawInterval != "" {
		parsed, err := time.ParseDuration(rawInterval)
		if err != nil || parsed <= 0 {
			return semanticIndexingSchedule{}, false
		}
		interval = parsed
	}
	batchSize := backfill.BatchSize
	if batchSize <= 0 {
		batchSize = defaultSemanticIndexingBatchSize
	}
	if batchSize > semanticIndexingMaxBatchSize {
		batchSize = semanticIndexingMaxBatchSize
	}
	return semanticIndexingSchedule{
		Interval:               interval,
		BatchSize:              batchSize,
		Workers:                min(effectiveHeavyTaskWorkers(a.config.WorkerRuntime.HeavyTaskWorkers), 1),
		TargetCompletedVectors: backfill.TargetCompletedVectors,
	}, true
}

func semanticIndexingScheduledForConfig(cfg config.Config) bool {
	backfill := cfg.SemanticRuntime.Indexing
	if !backfill.Enabled {
		return false
	}
	if len(cfg.Datasources) == 0 {
		return false
	}
	for _, datasource := range cfg.Datasources {
		switch datasource.Kind {
		case config.DatasourceKindImmichIndexed:
			return true
		case config.DatasourceKindLocalFiles:
			return true
		}
	}
	return false
}
