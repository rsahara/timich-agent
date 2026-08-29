package runtime

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/store"
	"golang.org/x/sync/semaphore"
)

const (
	defaultLocalQuickScanInterval           = 5 * time.Minute
	defaultLocalReconciliationTime          = "04:00"
	defaultLocalContentVerificationTime     = "04:00"
	defaultLocalContentVerificationDuration = 30 * time.Minute
	localContentVerificationAdmissionGrace  = time.Minute
	localBackgroundWorkerRetryDelay         = 30 * time.Second
	localPhase0ScanTimeout                  = 12 * time.Hour
	localMetadataBatchInterval              = 5 * time.Second
	localWorkerCountTimeout                 = 15 * time.Second
	localMetadataJobsPerWorker              = 48
	localThumbnailJobsPerWorker             = 16
	localEmbeddingJobsPerWorker             = 8

	localPhase3KindThumbnail = "thumbnail"
	localPhase3KindEmbedding = "embedding"
)

type localDatasourceScanSchedule struct {
	Interval time.Duration
}

type localContentVerificationSchedule struct {
	SourceKey string
	Time      string
	Duration  time.Duration
}

type localUploadTarget struct {
	SourceKey string
}

type localDiscoverySingleFlight struct {
	once sync.Once
	gate *semaphore.Weighted
}

func (singleFlight *localDiscoverySingleFlight) acquire(ctx context.Context) (func(), error) {
	singleFlight.once.Do(func() {
		singleFlight.gate = semaphore.NewWeighted(1)
	})
	if err := singleFlight.gate.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	return func() {
		singleFlight.gate.Release(1)
	}, nil
}

// StartLocalDatasourceScan starts automatic local filesystem Phase 0 scans.
func (a *AgentRuntime) StartLocalDatasourceScan() bool {
	if a == nil {
		return false
	}
	if _, ok := a.localDatasourceScanSchedule(); !ok {
		return false
	}
	a.localScanSchedulerMu.Lock()
	defer a.localScanSchedulerMu.Unlock()
	if a.localScanCancel != nil {
		notifyScheduleReset(a.localScanScheduleReset)
		notifyScheduleReset(a.localVerifyScheduleReset)
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	if a.localScanScheduleReset == nil {
		a.localScanScheduleReset = make(chan struct{}, 1)
	}
	if a.localVerifyScheduleReset == nil {
		a.localVerifyScheduleReset = make(chan struct{}, 1)
	}
	a.localScanCancel = cancel
	a.localScanWG.Add(2)
	go a.runLocalDatasourceScanLoop(ctx)
	go a.runLocalContentVerificationScheduleLoop(ctx)
	return true
}

func (a *AgentRuntime) stopLocalDatasourceScan() {
	a.localScanSchedulerMu.Lock()
	cancel := a.localScanCancel
	a.localScanCancel = nil
	a.localScanSchedulerMu.Unlock()
	if cancel != nil {
		cancel()
		a.localScanWG.Wait()
	}
}

func (a *AgentRuntime) runLocalDatasourceScanLoop(ctx context.Context) {
	defer a.localScanWG.Done()
	a.runScheduledLocalPhase0Scan(ctx, "startup")
	a.localScanSchedulerMu.Lock()
	resetC := a.localScanScheduleReset
	a.localScanSchedulerMu.Unlock()

	var ticker *time.Ticker
	var tickerC <-chan time.Time
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()

	resetSchedule := func() bool {
		schedule, ok := a.localDatasourceScanSchedule()
		if !ok {
			return false
		}
		if ticker != nil {
			ticker.Stop()
		}
		ticker = time.NewTicker(schedule.Interval)
		tickerC = ticker.C
		return true
	}
	if !resetSchedule() {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tickerC:
			a.runScheduledLocalPhase0Scan(ctx, "interval")
			if !resetSchedule() {
				return
			}
		case <-resetC:
			if !resetSchedule() {
				return
			}
		}
	}
}

func (a *AgentRuntime) runLocalContentVerificationScheduleLoop(ctx context.Context) {
	defer a.localScanWG.Done()
	a.localScanSchedulerMu.Lock()
	resetC := a.localVerifyScheduleReset
	a.localScanSchedulerMu.Unlock()

	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	resetTimer := func(next *time.Time) {
		if timer != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if next == nil {
			timerC = nil
			return
		}
		delay := time.Until(*next)
		if delay < 0 {
			delay = 0
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		timerC = timer.C
	}

	for {
		now := time.Now().UTC()
		a.reconcileLocalContentVerificationSchedule(ctx, now)
		next := a.nextLocalContentVerificationScheduleEvent(ctx, now)
		resetTimer(next)
		select {
		case <-ctx.Done():
			return
		case <-resetC:
		case <-timerC:
		}
	}
}

func (a *AgentRuntime) reconcileLocalContentVerificationSchedule(ctx context.Context, now time.Time) {
	catalogService := a.catalogService()
	if catalogService == nil || ctx.Err() != nil {
		return
	}
	changed := false
	if !a.backgroundWorkerPhaseActive("content_verification") {
		finishCtx, finishCancel := context.WithTimeout(ctx, localWorkerCountTimeout)
		finalized, err := catalogService.FinalizeExpiredLocalContentVerificationWindows(finishCtx, now)
		finishCancel()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("timich-agent local content verification expiration failed error=%v", err)
			}
		} else if finalized > 0 {
			changed = true
			log.Printf("timich-agent local content verification windows completed expired=%d", finalized)
		}
	}

	activeWorkers, _ := a.backgroundWorkerActiveState()
	hasIdleWorker := a.effectiveHeavyTaskWorkers() > activeWorkers
	location := a.localScheduleLocation()
	for _, schedule := range a.localContentVerificationSchedules() {
		if schedule.Duration <= 0 {
			continue
		}
		scheduledAt := latestLocalDailySchedule(now, location, schedule.Time)
		deadlineAt := scheduledAt.Add(schedule.Duration)
		actionCtx, actionCancel := context.WithTimeout(ctx, localWorkerCountTimeout)
		var admitted bool
		var err error
		switch {
		case !now.Before(deadlineAt) || now.After(scheduledAt.Add(localContentVerificationAdmissionGrace)):
			admitted, err = catalogService.SkipLocalContentVerificationWindow(
				actionCtx,
				schedule.SourceKey,
				scheduledAt,
				catalog.LocalContentVerificationSkipMissedWindow,
			)
		case !hasIdleWorker:
			admitted, err = catalogService.SkipLocalContentVerificationWindow(
				actionCtx,
				schedule.SourceKey,
				scheduledAt,
				catalog.LocalContentVerificationSkipNoIdleWorker,
			)
		default:
			admitted, err = catalogService.StartLocalContentVerificationWindow(
				actionCtx,
				schedule.SourceKey,
				scheduledAt,
				deadlineAt,
			)
		}
		actionCancel()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("timich-agent local content verification admission failed source_key=%s error=%v", schedule.SourceKey, err)
			}
			continue
		}
		if admitted {
			changed = true
		}
	}
	if !changed {
		return
	}
	a.schedulerWorkStateMarkDirty()
	a.wakeBackgroundWorkerScheduler()
	a.invalidateDatasourceIndexingSnapshot(catalogService)
}

func (a *AgentRuntime) nextLocalContentVerificationScheduleEvent(ctx context.Context, now time.Time) *time.Time {
	var next *time.Time
	location := a.localScheduleLocation()
	for _, schedule := range a.localContentVerificationSchedules() {
		if schedule.Duration <= 0 {
			continue
		}
		latest := latestLocalDailySchedule(now, location, schedule.Time)
		admissionClose := latest.Add(localContentVerificationAdmissionGrace)
		if deadline := latest.Add(schedule.Duration); deadline.Before(admissionClose) {
			admissionClose = deadline
		}
		if !now.Before(latest) && now.Before(admissionClose) {
			next = earliestTimePtr(next, &admissionClose)
		}
		candidate := nextLocalDailySchedule(now, location, schedule.Time)
		next = earliestTimePtr(next, &candidate)
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return next
	}
	deadlineCtx, deadlineCancel := context.WithTimeout(ctx, localWorkerCountTimeout)
	deadline, err := catalogService.LocalContentVerificationNextDeadline(deadlineCtx)
	deadlineCancel()
	if err == nil && deadline != nil && deadline.After(now) {
		next = earliestTimePtr(next, deadline)
	}
	return next
}

func earliestTimePtr(current *time.Time, candidate *time.Time) *time.Time {
	if candidate == nil || candidate.IsZero() {
		return current
	}
	utc := candidate.UTC()
	if current == nil || utc.Before(current.UTC()) {
		return &utc
	}
	return current
}

func (a *AgentRuntime) localContentVerificationSchedules() []localContentVerificationSchedule {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	schedules := make([]localContentVerificationSchedule, 0, len(a.config.Datasources))
	for _, datasource := range a.config.Datasources {
		if datasource.Kind != config.DatasourceKindLocalFiles || strings.TrimSpace(datasource.SourceKey) == "" {
			continue
		}
		verificationTime := defaultLocalContentVerificationTime
		duration := defaultLocalContentVerificationDuration
		if datasource.Scan != nil {
			if raw := strings.TrimSpace(datasource.Scan.ContentVerificationTime); raw != "" {
				verificationTime = raw
			}
			if raw := strings.TrimSpace(datasource.Scan.ContentVerificationDuration); raw != "" {
				parsed, err := time.ParseDuration(raw)
				if err != nil || parsed < 0 {
					continue
				}
				duration = parsed
			}
		}
		schedules = append(schedules, localContentVerificationSchedule{
			SourceKey: strings.TrimSpace(datasource.SourceKey),
			Time:      verificationTime,
			Duration:  duration,
		})
	}
	return schedules
}

func (a *AgentRuntime) notifyLocalScanCompleted() {
	if a == nil {
		return
	}
	a.localScanSchedulerMu.Lock()
	resetC := a.localScanScheduleReset
	verificationResetC := a.localVerifyScheduleReset
	a.localScanSchedulerMu.Unlock()
	notifyScheduleReset(resetC)
	notifyScheduleReset(verificationResetC)
}

func (a *AgentRuntime) notifyLocalContentVerificationSchedule() {
	if a == nil {
		return
	}
	a.localScanSchedulerMu.Lock()
	resetC := a.localVerifyScheduleReset
	a.localScanSchedulerMu.Unlock()
	notifyScheduleReset(resetC)
}

func notifyScheduleReset(resetC chan<- struct{}) {
	if resetC == nil {
		return
	}
	select {
	case resetC <- struct{}{}:
	default:
	}
}

func (a *AgentRuntime) scheduleLocalDiscoveryAfterUpload(asset store.UploadedAsset) {
	if a == nil {
		return
	}
	root, ok := a.uploadRootConfig(asset.SelectedRootKey)
	if !ok {
		return
	}
	finalPath, err := uploadRootChildPath(root.Path, asset.FinalRelativePath)
	if err != nil {
		return
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return
	}
	queueChanged := false
	needsWake := false
	for _, target := range a.localUploadTargets(asset) {
		ctx, cancel := context.WithTimeout(context.Background(), localWorkerCountTimeout)
		changed, err := catalogService.QueueCommittedLocalUpload(ctx, target.SourceKey, finalPath)
		cancel()
		if err != nil {
			if errors.Is(err, catalog.ErrLocalMediaRootNotTrusted) {
				log.Printf("timich-agent committed upload metadata queue deferred source_key=%s path=%s reason=local_root_not_trusted", target.SourceKey, asset.FinalRelativePath)
				continue
			}
			log.Printf("timich-agent committed upload metadata queue failed source_key=%s path=%s error=%v", target.SourceKey, asset.FinalRelativePath, err)
			a.schedulerWorkStateMarkDirty()
			needsWake = true
			continue
		}
		if changed {
			queueChanged = true
		}
	}
	if queueChanged {
		a.schedulerWorkStateMarkDirty()
		needsWake = true
	}
	if needsWake {
		a.wakeBackgroundWorkerScheduler()
	}
}

func (a *AgentRuntime) localUploadTargets(asset store.UploadedAsset) []localUploadTarget {
	root, ok := a.uploadRootConfig(asset.SelectedRootKey)
	if !ok {
		return nil
	}
	finalPath, err := uploadRootChildPath(root.Path, asset.FinalRelativePath)
	if err != nil {
		return nil
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	localRootsByKey := make(map[string]config.LocalMediaRootConfig, len(a.config.LocalMediaRoots))
	for _, localRoot := range a.config.LocalMediaRoots {
		localRootsByKey[strings.TrimSpace(localRoot.Key)] = localRoot
	}
	targets := make([]localUploadTarget, 0, len(a.config.Datasources))
	deepestRootDepth := -1
	for _, datasource := range a.config.Datasources {
		if datasource.Kind != config.DatasourceKindLocalFiles || strings.TrimSpace(datasource.RootKey) == "" {
			continue
		}
		sourceKey := strings.TrimSpace(datasource.SourceKey)
		if sourceKey == "" {
			continue
		}
		localRoot, ok := localRootsByKey[strings.TrimSpace(datasource.RootKey)]
		if !ok || !localPathContains(localRoot.Path, finalPath) {
			continue
		}
		rootDepth := localPathDepth(localRoot.Path)
		if rootDepth > deepestRootDepth {
			targets = targets[:0]
			deepestRootDepth = rootDepth
		}
		if rootDepth < deepestRootDepth {
			continue
		}
		targets = append(targets, localUploadTarget{
			SourceKey: sourceKey,
		})
	}
	return targets
}

func localPhase0QueuedMetadata(results []catalog.LocalPhase0ScanResult) int {
	queued := 0
	for _, result := range results {
		queued += max(result.QueuedMetadata, 0)
	}
	return queued
}

func (a *AgentRuntime) runScheduledLocalPhase0Scan(ctx context.Context, reason string) {
	if _, ok := a.localDatasourceScanSchedule(); !ok {
		return
	}
	scanCtx, cancel := context.WithTimeout(ctx, localPhase0ScanTimeout)
	defer cancel()
	a.syncLocalPhase0Scans(scanCtx, reason)
}

func (a *AgentRuntime) triggerLocalPhase0Scan(reason string) {
	if a == nil {
		return
	}
	if _, ok := a.localDatasourceScanSchedule(); !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), localPhase0ScanTimeout)
		defer cancel()
		a.syncLocalPhase0Scans(ctx, reason)
	}()
}

func (a *AgentRuntime) syncLocalPhase0Scans(ctx context.Context, reason string) {
	releaseDiscovery, acquireErr := a.localDiscovery.acquire(ctx)
	if acquireErr != nil {
		return
	}
	defer releaseDiscovery()

	catalogService := a.catalogService()
	if catalogService == nil {
		return
	}
	clearActive := a.setDatasourceDiscoveryActive(1)
	defer clearActive()
	a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, true, nil)
	log.Printf("timich-agent local phase0 scan started reason=%s", reason)
	var results []catalog.LocalPhase0ScanResult
	var err error
	results, err = runAutomaticLocalPhase0Scans(ctx, catalogService, time.Now().UTC(), a.localScheduleLocation())
	if err != nil {
		a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, false, nil)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrStorageWriteBlocked) {
			log.Printf("timich-agent local phase0 scan skipped reason=%s blocked_by=storage error=%v", reason, err)
			return
		}
		log.Printf("timich-agent local phase0 scan failed reason=%s error=%v", reason, err)
		return
	}
	a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, false, localPhase0ScanResultsCompletedAt(results), results...)
	for _, result := range results {
		log.Printf(
			"timich-agent local phase0 scan completed reason=%s source_key=%s root_key=%s scan_mode=%s status=%s root_status=%s discovered=%d changed=%d queued_metadata=%d missing=%d skipped=%d",
			reason,
			result.SourceKey,
			result.RootKey,
			result.ScanMode,
			result.Status,
			result.RootStatus,
			result.DiscoveredPaths,
			result.ChangedPaths,
			result.QueuedMetadata,
			result.MissingPaths,
			result.SkippedPaths,
		)
	}
	a.schedulerWorkStateMarkDirty()
	a.wakeBackgroundWorkerScheduler()
	a.notifyLocalScanCompleted()
}

func (a *AgentRuntime) runScheduledLocalContentVerification(ctx context.Context) bool {
	catalogService := a.catalogService()
	if catalogService == nil || ctx.Err() != nil {
		return false
	}
	runnableSourceKeys, err := catalogService.LocalContentVerificationRunnableSourceKeys(ctx, time.Now().UTC())
	if err != nil {
		if ctx.Err() == nil {
			retryAt := a.deferLocalBackgroundWorkerRetry("content_verification")
			log.Printf("timich-agent local content verification schedule failed retry_at=%s error=%v", retryAt.Format(time.RFC3339Nano), err)
			a.schedulerWorkStateMarkDirty()
		}
		return false
	}
	if len(runnableSourceKeys) == 0 {
		a.setLocalBackgroundWorkerRetryNotBefore("content_verification", nil)
		a.schedulerWorkStateMarkDirty()
		return false
	}
	sourceKey, ok := a.nextLocalContentVerificationSource(runnableSourceKeys)
	if !ok {
		a.setLocalBackgroundWorkerRetryNotBefore("content_verification", nil)
		a.schedulerWorkStateMarkDirty()
		return false
	}
	return a.runLocalContentVerificationSource(ctx, catalogService, sourceKey, "worker_scheduler")
}

func (a *AgentRuntime) runLocalContentVerificationSource(ctx context.Context, catalogService *catalog.Service, sourceKey string, reason string) bool {
	a.rememberDatasourceTaskActivitySnapshot(catalogService, "content_verification", 1)
	defer a.rememberDatasourceTaskActivitySnapshot(catalogService, "content_verification", 0)
	defer func() {
		a.schedulerWorkStateMarkDirty()
		a.wakeBackgroundWorkerScheduler()
		a.invalidateDatasourceIndexingSnapshot(catalogService)
	}()
	if ctx.Err() != nil || a.effectiveHeavyTaskWorkers() <= 0 {
		return false
	}
	result, err := catalogService.RunLocalContentVerification(ctx, sourceKey)
	if err != nil {
		if ctx.Err() == nil {
			retryAt := a.deferLocalBackgroundWorkerRetry("content_verification")
			log.Printf(
				"timich-agent local content verification failed source_key=%s reason=%s retry_at=%s error=%v",
				sourceKey,
				reason,
				retryAt.Format(time.RFC3339Nano),
				err,
			)
		}
		return false
	}
	a.setLocalBackgroundWorkerRetryNotBefore("content_verification", nil)
	log.Printf(
		"timich-agent local content verification slice completed source_key=%s reason=%s processed=%d verified=%d changed=%d failed=%d bytes=%d window_expired=%t window_complete=%t",
		sourceKey,
		reason,
		result.ProcessedFiles,
		result.VerifiedFiles,
		result.ChangedFiles,
		result.FailedFiles,
		result.ReadBytes,
		result.WindowExpired,
		result.WindowComplete,
	)
	return result.ProcessedFiles > 0
}

func (a *AgentRuntime) nextLocalContentVerificationSource(sourceKeys []string) (string, bool) {
	if a == nil || len(sourceKeys) == 0 {
		return "", false
	}
	a.localBackgroundWorkMu.Lock()
	defer a.localBackgroundWorkMu.Unlock()
	selected := ""
	for _, sourceKey := range sourceKeys {
		sourceKey = strings.TrimSpace(sourceKey)
		if sourceKey == "" {
			continue
		}
		if selected == "" {
			selected = sourceKey
		}
		if sourceKey > a.localContentVerificationLast {
			selected = sourceKey
			break
		}
	}
	if selected == "" {
		return "", false
	}
	a.localContentVerificationLast = selected
	return selected, true
}

func (a *AgentRuntime) deferLocalBackgroundWorkerRetry(phase string) time.Time {
	retryAt := time.Now().UTC().Add(localBackgroundWorkerRetryDelay)
	a.setLocalBackgroundWorkerRetryNotBefore(phase, &retryAt)
	return retryAt
}

func (a *AgentRuntime) setLocalBackgroundWorkerRetryNotBefore(phase string, next *time.Time) {
	phase = strings.TrimSpace(phase)
	if a == nil || phase == "" {
		return
	}
	a.localBackgroundWorkMu.Lock()
	defer a.localBackgroundWorkMu.Unlock()
	if next == nil || next.IsZero() {
		delete(a.localBackgroundWorkRetryAt, phase)
		return
	}
	if a.localBackgroundWorkRetryAt == nil {
		a.localBackgroundWorkRetryAt = make(map[string]time.Time)
	}
	utc := next.UTC()
	a.localBackgroundWorkRetryAt[phase] = utc
}

func (a *AgentRuntime) localBackgroundWorkerRetryNotBeforeAt(phase string) *time.Time {
	phase = strings.TrimSpace(phase)
	if a == nil || phase == "" {
		return nil
	}
	a.localBackgroundWorkMu.Lock()
	defer a.localBackgroundWorkMu.Unlock()
	retryAt, ok := a.localBackgroundWorkRetryAt[phase]
	if !ok {
		return nil
	}
	utc := retryAt.UTC()
	if !utc.After(time.Now().UTC()) {
		delete(a.localBackgroundWorkRetryAt, phase)
		return nil
	}
	return &utc
}

func (a *AgentRuntime) localBackgroundWorkerRetryDeferred(phase string, now time.Time) bool {
	retryAt := a.localBackgroundWorkerRetryNotBeforeAt(phase)
	return retryAt != nil && retryAt.After(now.UTC())
}

func (a *AgentRuntime) nextLocalBackgroundWorkerRetryNotBeforeAt() *time.Time {
	if a == nil {
		return nil
	}
	a.localBackgroundWorkMu.Lock()
	defer a.localBackgroundWorkMu.Unlock()
	now := time.Now().UTC()
	var earliest *time.Time
	for phase, retryAt := range a.localBackgroundWorkRetryAt {
		utc := retryAt.UTC()
		if !utc.After(now) {
			delete(a.localBackgroundWorkRetryAt, phase)
			continue
		}
		if earliest == nil || utc.Before(*earliest) {
			copy := utc
			earliest = &copy
		}
	}
	return earliest
}

func (a *AgentRuntime) clearLocalBackgroundWorkerRetries() {
	if a == nil {
		return
	}
	a.localBackgroundWorkMu.Lock()
	a.localBackgroundWorkRetryAt = nil
	a.localBackgroundWorkMu.Unlock()
}

func runAutomaticLocalPhase0Scans(ctx context.Context, catalogService *catalog.Service, now time.Time, location *time.Location) ([]catalog.LocalPhase0ScanResult, error) {
	if catalogService == nil {
		return nil, catalog.ErrNoDatasourceConfigured
	}
	sourceKeys := catalogService.LocalDatasourceSourceKeys()
	if len(sourceKeys) == 0 {
		return nil, catalog.ErrNoDatasourceConfigured
	}
	results := make([]catalog.LocalPhase0ScanResult, 0, len(sourceKeys))
	for _, sourceKey := range sourceKeys {
		reconciliationTime, err := catalogService.LocalDatasourceReconciliationTime(sourceKey)
		if err != nil {
			return results, err
		}
		reconciliationDue, err := catalogService.LocalReconciliationDue(
			ctx,
			sourceKey,
			latestLocalDailySchedule(now, location, reconciliationTime),
		)
		if err != nil {
			return results, err
		}
		if !reconciliationDue {
			quickInterval, err := catalogService.LocalDatasourceQuickScanInterval(sourceKey)
			if err != nil {
				return results, err
			}
			quickDue, err := catalogService.LocalQuickDiscoveryDue(ctx, sourceKey, now, quickInterval)
			if err != nil {
				return results, err
			}
			if !quickDue {
				continue
			}
		}
		result, err := runAutomaticLocalPhase0Scan(ctx, catalogService, sourceKey, now, location)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func runAutomaticLocalPhase0Scan(ctx context.Context, catalogService *catalog.Service, sourceKey string, now time.Time, location *time.Location) (catalog.LocalPhase0ScanResult, error) {
	reconciliationTime, err := catalogService.LocalDatasourceReconciliationTime(sourceKey)
	if err != nil {
		return catalog.LocalPhase0ScanResult{}, err
	}
	reconciliationDue, err := catalogService.LocalReconciliationDue(
		ctx,
		sourceKey,
		latestLocalDailySchedule(now, location, reconciliationTime),
	)
	if err != nil {
		return catalog.LocalPhase0ScanResult{}, err
	}
	if reconciliationDue {
		return catalogService.RunLocalReconciliationScan(ctx, sourceKey)
	}
	return catalogService.RunLocalQuickDiscoveryScan(ctx, sourceKey)
}

func (a *AgentRuntime) runScheduledLocalMetadataBatch(ctx context.Context, reason string) bool {
	return a.runScheduledLocalMetadataBatchWithWorkers(ctx, reason, a.effectiveHeavyTaskWorkers(), 0)
}

func (a *AgentRuntime) runScheduledLocalMetadataBatchWithWorkers(ctx context.Context, reason string, workers int, planned int) bool {
	if _, ok := a.localDatasourceScanSchedule(); !ok {
		return false
	}
	a.localScanMu.Lock()
	defer a.localScanMu.Unlock()

	catalogService := a.catalogService()
	if catalogService == nil {
		return false
	}
	if workers <= 0 {
		return false
	}
	batchSize := localMetadataBatchSizeForWorkers(workers)
	if batchSize <= 0 {
		return false
	}
	activeWorkers := localActiveWorkersForBatch(workers, batchSize, localMetadataJobsPerWorker)
	log.Printf("timich-agent local metadata batch starting reason=%s planned=%d batch_size=%d workers=%d", reason, planned, batchSize, activeWorkers)
	a.rememberDatasourceTaskActivitySnapshot(catalogService, "metadata", activeWorkers)
	result, err := catalogService.RunLocalMetadataBatchWithOptions(ctx, batchSize, activeWorkers, catalog.LocalBackgroundBatchOptions{
		BeforeJob: a.foregroundCatalog.waitUntilIdle,
	})
	a.rememberDatasourceTaskActivitySnapshot(catalogService, "metadata", 0)
	if err != nil {
		if result.ProcessedJobs > 0 {
			a.schedulerWorkStateCompleteLocalMetadata(result, planned)
		} else if planned > 0 {
			a.schedulerWorkStateRelease("metadata", planned)
		}
		a.schedulerWorkStateMarkDirty()
		if ctx.Err() != nil {
			return false
		}
		retryAt := a.deferLocalBackgroundWorkerRetry("metadata")
		if errors.Is(err, ErrStorageWriteBlocked) {
			log.Printf("timich-agent local metadata batch skipped reason=%s blocked_by=storage retry_at=%s error=%v", reason, retryAt.Format(time.RFC3339Nano), err)
			return false
		}
		log.Printf("timich-agent local metadata batch failed reason=%s retry_at=%s error=%v", reason, retryAt.Format(time.RFC3339Nano), err)
		return false
	}
	a.setLocalBackgroundWorkerRetryNotBefore("metadata", nil)
	a.schedulerWorkStateCompleteLocalMetadata(result, planned)
	if result.ProcessedJobs == 0 {
		return false
	}
	log.Printf(
		"timich-agent local metadata batch completed reason=%s processed=%d completed=%d failed=%d deferred=%d registered=%d",
		reason,
		result.ProcessedJobs,
		result.CompletedJobs,
		result.FailedJobs,
		result.DeferredJobs,
		result.RegisteredAssets,
	)
	return true
}

func (a *AgentRuntime) runScheduledLocalThumbnailBatch(ctx context.Context, reason string) bool {
	return a.runScheduledLocalThumbnailBatchWithWorkers(ctx, reason, a.effectiveHeavyTaskWorkers(), 0)
}

func (a *AgentRuntime) runScheduledLocalThumbnailBatchWithWorkers(ctx context.Context, reason string, workers int, planned int) bool {
	if _, ok := a.localDatasourceScanSchedule(); !ok {
		return false
	}
	a.localScanMu.Lock()
	defer a.localScanMu.Unlock()

	catalogService := a.catalogService()
	if catalogService == nil {
		return false
	}
	if workers <= 0 {
		return false
	}
	batchSize := localThumbnailBatchSizeForWorkers(workers)
	if batchSize <= 0 {
		return false
	}
	activeWorkers := localActiveWorkersForBatch(workers, batchSize, localThumbnailJobsPerWorker)
	log.Printf("timich-agent local thumbnail batch starting reason=%s planned=%d batch_size=%d workers=%d", reason, planned, batchSize, activeWorkers)
	a.rememberDatasourceTaskActivitySnapshot(catalogService, "thumbnails", activeWorkers)
	result, err := catalogService.RunLocalThumbnailBatchWithOptions(ctx, batchSize, activeWorkers, catalog.LocalBackgroundBatchOptions{
		BeforeJob: a.foregroundCatalog.waitUntilIdle,
	})
	a.rememberDatasourceTaskActivitySnapshot(catalogService, "thumbnails", 0)
	if err != nil {
		if result.ProcessedJobs > 0 {
			a.schedulerWorkStateCompleteLocalThumbnail(result, planned)
		} else if planned > 0 {
			a.schedulerWorkStateRelease("thumbnails", planned)
		}
		a.schedulerWorkStateMarkDirty()
		if ctx.Err() != nil {
			return false
		}
		retryAt := a.deferLocalBackgroundWorkerRetry("thumbnails")
		if errors.Is(err, ErrStorageWriteBlocked) {
			log.Printf("timich-agent local thumbnail batch skipped reason=%s blocked_by=storage retry_at=%s error=%v", reason, retryAt.Format(time.RFC3339Nano), err)
			return false
		}
		log.Printf("timich-agent local thumbnail batch failed reason=%s retry_at=%s error=%v", reason, retryAt.Format(time.RFC3339Nano), err)
		return false
	}
	a.setLocalBackgroundWorkerRetryNotBefore("thumbnails", nil)
	a.schedulerWorkStateCompleteLocalThumbnail(result, planned)
	if result.ProcessedJobs == 0 {
		return false
	}
	log.Printf(
		"timich-agent local thumbnail batch completed reason=%s processed=%d completed=%d failed=%d deferred=%d generated=%d",
		reason,
		result.ProcessedJobs,
		result.CompletedJobs,
		result.FailedJobs,
		result.DeferredJobs,
		result.GeneratedAssets,
	)
	return result.GeneratedAssets > 0
}

func (a *AgentRuntime) recoverCanceledLocalBackgroundWorkerAssignment(phase string) {
	if phase != "metadata" && phase != "thumbnails" {
		return
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return
	}
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), localWorkerCountTimeout)
	recovered, err := catalogService.ResetRunningLocalScanJobs(recoveryCtx)
	recoveryCancel()
	a.schedulerWorkStateMarkDirty()
	if err != nil {
		log.Printf("timich-agent canceled local assignment recovery failed phase=%s error=%v", phase, err)
		return
	}
	if recovered > 0 {
		log.Printf("timich-agent canceled local assignment recovery requeued=%d phase=%s", recovered, phase)
	}
}

func (a *AgentRuntime) runScheduledLocalPhase3Batch(ctx context.Context, reason string) {
	switch a.nextLocalPhase3Kind() {
	case localPhase3KindEmbedding:
		if a.runScheduledLocalEmbeddingBatch(ctx, reason) {
			return
		}
		a.runScheduledLocalThumbnailBatch(ctx, reason+"_thumbnail_fallback")
	default:
		if a.runScheduledLocalThumbnailBatch(ctx, reason) {
			return
		}
		a.runScheduledLocalEmbeddingBatch(ctx, reason+"_embedding_fallback")
	}
}

func (a *AgentRuntime) nextLocalPhase3Kind() string {
	a.localPhase3Mu.Lock()
	defer a.localPhase3Mu.Unlock()
	if a.localPhase3Next == localPhase3KindEmbedding {
		a.localPhase3Next = localPhase3KindThumbnail
		return localPhase3KindEmbedding
	}
	a.localPhase3Next = localPhase3KindEmbedding
	return localPhase3KindThumbnail
}

func (a *AgentRuntime) runScheduledLocalEmbeddingBatch(ctx context.Context, reason string) bool {
	schedule, ok := a.semanticIndexingSchedule()
	if !ok {
		return false
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return false
	}
	sourceKeys := catalogService.LocalDatasourceSourceKeys()
	if len(sourceKeys) == 0 {
		return false
	}
	workBatchSize, ok := a.semanticIndexingWorkBatchSize(ctx, schedule)
	if !ok {
		return false
	}
	batchSize := min(workBatchSize, a.localEmbeddingBatchSize())
	if batchSize <= 0 || schedule.Workers <= 0 {
		return false
	}
	backfillCtx, cancel := context.WithTimeout(ctx, semanticIndexingTimeout)
	defer cancel()
	result, err := a.backfillSemanticModelCandidate(backfillCtx, batchSize, catalog.SemanticModelBackfillOptions{
		Workers:    schedule.Workers,
		SourceKeys: sourceKeys,
	})
	if err != nil {
		if ctx.Err() != nil ||
			errors.Is(err, ErrSemanticCandidateUnavailable) ||
			errors.Is(err, ErrSemanticCandidateRuntimeUnavailable) ||
			errors.Is(err, catalog.ErrCatalogNotConfigured) {
			return false
		}
		log.Printf("timich-agent local phase3 embedding batch failed reason=%s error=%v", reason, err)
		return false
	}
	if result.ProcessedVectorCount <= 0 {
		return false
	}
	log.Printf(
		"timich-agent local phase3 embedding batch completed reason=%s model=%s processed=%d indexed=%d remaining=%d",
		reason,
		result.Status.ModelID,
		result.ProcessedVectorCount,
		result.IndexedVectorCount,
		result.Status.RemainingVectorCount,
	)
	return true
}

func (a *AgentRuntime) localDatasourceScanSchedule() (localDatasourceScanSchedule, bool) {
	return a.localDatasourceScanScheduleAt(time.Now())
}

func (a *AgentRuntime) localDatasourceScanScheduleAt(now time.Time) (localDatasourceScanSchedule, bool) {
	if a == nil {
		return localDatasourceScanSchedule{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.config.Datasources) == 0 {
		return localDatasourceScanSchedule{}, false
	}
	location := localScheduleLocation(a.config.Timezone)
	interval := time.Duration(0)
	for _, datasource := range a.config.Datasources {
		if datasource.Kind != config.DatasourceKindLocalFiles || strings.TrimSpace(datasource.RootKey) == "" {
			continue
		}
		quickInterval := defaultLocalQuickScanInterval
		reconciliationTime := defaultLocalReconciliationTime
		if datasource.Scan != nil {
			raw := strings.TrimSpace(datasource.Scan.QuickScanInterval)
			if raw != "" {
				parsed, err := time.ParseDuration(raw)
				if err != nil || parsed <= 0 {
					return localDatasourceScanSchedule{}, false
				}
				quickInterval = parsed
			}
			if raw := strings.TrimSpace(datasource.Scan.ReconciliationTime); raw != "" {
				if _, err := time.Parse("15:04", raw); err != nil {
					return localDatasourceScanSchedule{}, false
				}
				reconciliationTime = raw
			}
		}
		datasourceInterval := quickInterval
		untilReconciliation := nextLocalDailySchedule(now, location, reconciliationTime).Sub(now)
		if untilReconciliation > 0 && untilReconciliation < datasourceInterval {
			datasourceInterval = untilReconciliation
		}
		if interval == 0 || datasourceInterval < interval {
			interval = datasourceInterval
		}
	}
	if interval == 0 {
		return localDatasourceScanSchedule{}, false
	}
	return localDatasourceScanSchedule{Interval: interval}, true
}

func (a *AgentRuntime) localScheduleLocation() *time.Location {
	if a == nil {
		return time.Local
	}
	a.mu.RLock()
	timezone := a.config.Timezone
	a.mu.RUnlock()
	return localScheduleLocation(timezone)
}

func localScheduleLocation(timezone string) *time.Location {
	timezone = strings.TrimSpace(timezone)
	if timezone == "" {
		return time.Local
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Local
	}
	return location
}

func latestLocalDailySchedule(now time.Time, location *time.Location, value string) time.Time {
	if location == nil {
		location = time.Local
	}
	hour, minute := localDailyScheduleClock(value)
	localNow := now.In(location)
	scheduled := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), hour, minute, 0, 0, location)
	if localNow.Before(scheduled) {
		scheduled = scheduled.AddDate(0, 0, -1)
	}
	return scheduled.UTC()
}

func nextLocalDailySchedule(now time.Time, location *time.Location, value string) time.Time {
	if location == nil {
		location = time.Local
	}
	hour, minute := localDailyScheduleClock(value)
	localNow := now.In(location)
	scheduled := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), hour, minute, 0, 0, location)
	if !localNow.Before(scheduled) {
		scheduled = scheduled.AddDate(0, 0, 1)
	}
	return scheduled.UTC()
}

func localDailyScheduleClock(value string) (int, int) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		parsed, _ = time.Parse("15:04", defaultLocalReconciliationTime)
	}
	return parsed.Hour(), parsed.Minute()
}

func localPathContains(parent string, child string) bool {
	parentPath, ok := cleanLocalAbsolutePath(parent)
	if !ok {
		return false
	}
	childPath, ok := cleanLocalAbsolutePath(child)
	if !ok {
		return false
	}
	relativePath, err := filepath.Rel(parentPath, childPath)
	if err != nil {
		return false
	}
	return relativePath == "." || (relativePath != ".." && !strings.HasPrefix(relativePath, ".."+string(filepath.Separator)))
}

func cleanLocalAbsolutePath(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", false
	}
	return filepath.Clean(absolute), true
}

func localPathDepth(value string) int {
	cleanPath, ok := cleanLocalAbsolutePath(value)
	if !ok {
		return 0
	}
	volumeName := filepath.VolumeName(cleanPath)
	trimmed := strings.Trim(cleanPath[len(volumeName):], string(filepath.Separator))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, string(filepath.Separator)))
}

func localDatasourceScanScheduledForConfig(cfg config.Config) bool {
	if len(cfg.Datasources) == 0 {
		return false
	}
	for _, datasource := range cfg.Datasources {
		if datasource.Kind == config.DatasourceKindLocalFiles && strings.TrimSpace(datasource.RootKey) != "" {
			return true
		}
	}
	return false
}

func (a *AgentRuntime) localMetadataBatchSize() int {
	workers := a.effectiveHeavyTaskWorkers()
	if workers <= 0 {
		return 0
	}
	return localMetadataBatchSizeForWorkers(workers)
}

func (a *AgentRuntime) localThumbnailBatchSize() int {
	workers := a.effectiveHeavyTaskWorkers()
	if workers <= 0 {
		return 0
	}
	return localThumbnailBatchSizeForWorkers(workers)
}

func (a *AgentRuntime) localEmbeddingBatchSize() int {
	workers := a.effectiveHeavyTaskWorkers()
	if workers <= 0 {
		return 0
	}
	return localEmbeddingBatchSizeForWorkers(workers)
}

func localMetadataBatchSizeForWorkers(workers int) int {
	if workers <= 0 {
		workers = 1
	}
	return workers * localMetadataJobsPerWorker
}

func localThumbnailBatchSizeForWorkers(workers int) int {
	if workers <= 0 {
		workers = 1
	}
	return workers * localThumbnailJobsPerWorker
}

func localEmbeddingBatchSizeForWorkers(workers int) int {
	if workers <= 0 {
		workers = 1
	}
	return workers * localEmbeddingJobsPerWorker
}

func localActiveWorkersForBatch(workers int, queued int, jobsPerWorker int) int {
	if workers <= 0 || queued <= 0 {
		return 0
	}
	if jobsPerWorker <= 0 {
		return workers
	}
	active := (queued + jobsPerWorker - 1) / jobsPerWorker
	if active < 1 {
		active = 1
	}
	if active > workers {
		active = workers
	}
	return active
}
