package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
)

const (
	datasourceIngestionRemoteAPI          = "remote_api"
	datasourceIngestionFilesystem         = "filesystem"
	datasourceLocalScanModeQuick          = "quick"
	datasourceLocalScanModeReconciliation = "reconciliation"
	datasourceEmbeddingUnavailable        = "unavailable"
	datasourceExpensiveStatusTimeout      = 6 * time.Second
	localDatasourceEmbeddingStatusTimeout = 5 * time.Second
	datasourceIndexingStatusSnapshotKey   = "datasource_indexing"
	datasourceIndexingSnapshotTimeout     = 2 * time.Minute
	datasourceIndexingSnapshotSaveTimeout = 500 * time.Millisecond
	datasourceIndexingSnapshotStaleAfter  = datasourceIndexingSnapshotTimeout + 5*time.Second
	datasourceIndexingAdminActiveWindow   = 30 * time.Minute
	assetProcessingStatsRefreshMinAge     = 15 * time.Second
	assetProcessingStatsRefreshTimeout    = 90 * time.Second
	datasourceTaskNoteMediaDiscovery      = "Quick discovery finds ordinary additions, removals, and moves. Reconciliation inspects every supported file daily at 04:00 in the Agent timezone; Run reconciliation now starts it manually."
	datasourceTaskNoteContentVerification = "At the configured daily time, uses an idle heavy-task worker to compare saved content hashes. If no worker is idle, that day's run is skipped. The default duration is 30 minutes; set contentVerificationDuration to 0 to disable it."
	datasourceTaskNoteMetadata            = "Registers media information in the media database. Recently added or changed files remain settling before metadata processing (2 minutes by default). Requeue failed moves failed metadata jobs back to the queue at repair priority. Processing starts after settling when a worker is available, and jobs that fail again return to failed."
	datasourceTaskNoteThumbnails          = "Generates thumbnails so media can be previewed quickly. Requeue failed moves failed thumbnails back to the queue at repair priority. Processing starts when a worker is available, and items that fail again return to failed."
	datasourceTaskNoteEmbeddings          = "Analyzes media features for visual search. Within each datasource, first-attempt work is prioritized ahead of eligible retries, so the failed count may remain unchanged while new embeddings complete. Search becomes available after the Search index processes the embeddings."
	datasourceTaskNoteSearchIndex         = "Updates the search index so media can be searched. Publishing can take several hours for a large library. An existing published index remains searchable while publishing runs. Failed publish jobs are retried automatically on the next eligible run."
	datasourceTaskNoteSearchModelRequired = "Install and activate a semantic model in the Search tab to enable it."
	datasourceTaskSetupSearchModel        = "search_model"
	datasourceTaskWaitingSearchIndex      = "search_index"
	datasourceTaskWaitingPaused           = "paused"
	datasourceTaskWaitingWorker           = "worker"
	datasourceTaskWaitingQueuedTarget     = "queued_target"
	datasourceTaskWaitingScheduled        = "scheduled"
	datasourceTaskFailureUnitItems        = "items"
	datasourceTaskFailureUnitPublishJobs  = "publish_jobs"
)

type datasourceIndexingSnapshotPayload struct {
	Version    int                        `json:"version"`
	ConfigHash string                     `json:"configHash"`
	Response   DatasourceIndexingResponse `json:"response"`
}

// DatasourceIndexingResponse reports datasource ingestion state without exposing
// the old mirror/local split as an Admin-facing concept.
type DatasourceIndexingResponse struct {
	Roots              []LocalMediaRootSummary    `json:"roots,omitempty"`
	Tasks              []DatasourceTaskStatus     `json:"tasks,omitempty"`
	Datasources        []DatasourceIndexingStatus `json:"datasources"`
	StatusSnapshotAt   *time.Time                 `json:"statusSnapshotAt,omitempty"`
	StatusSnapshotUsed bool                       `json:"statusSnapshotUsed,omitempty"`
	snapshotConfigHash string
}

// DatasourceTaskStatus summarizes active and remaining work by scan/index phase.
type DatasourceTaskStatus struct {
	Phase                string     `json:"phase"`
	Label                string     `json:"label"`
	ActiveTasks          int        `json:"activeTasks"`
	ActiveTasksUnknown   bool       `json:"activeTasksUnknown,omitempty"`
	QueuedTasks          int        `json:"queuedTasks"`
	SettlingTasks        int        `json:"settlingTasks,omitempty"`
	QueuedTasksUnknown   bool       `json:"queuedTasksUnknown,omitempty"`
	CompletedTasks       int        `json:"completedTasks,omitempty"`
	TotalTasks           int        `json:"totalTasks,omitempty"`
	FailedTasks          int        `json:"failedTasks,omitempty"`
	FailedTasksUnknown   bool       `json:"failedTasksUnknown,omitempty"`
	FailureUnit          string     `json:"failureUnit,omitempty"`
	Status               string     `json:"status"`
	Note                 string     `json:"note,omitempty"`
	SetupRequired        string     `json:"setupRequired,omitempty"`
	WaitingReason        string     `json:"waitingReason,omitempty"`
	WaitingQueuedTarget  int        `json:"waitingQueuedTarget,omitempty"`
	NextRunAt            *time.Time `json:"nextRunAt,omitempty"`
	LastCompletedAt      *time.Time `json:"lastCompletedAt,omitempty"`
	LastQuickScanAt      *time.Time `json:"lastQuickScanAt,omitempty"`
	LastReconciliationAt *time.Time `json:"lastReconciliationAt,omitempty"`
	LastRunStartedAt     *time.Time `json:"lastRunStartedAt,omitempty"`
	LastRunStatus        string     `json:"lastRunStatus,omitempty"`
	LastRunReason        string     `json:"lastRunReason,omitempty"`
	LastProcessedFiles   int        `json:"lastProcessedFiles,omitempty"`
	LastVerifiedFiles    int        `json:"lastVerifiedFiles,omitempty"`
	LastChangedFiles     int        `json:"lastChangedFiles,omitempty"`
	LastFailedFiles      int        `json:"lastFailedFiles,omitempty"`
	LastReadBytes        int64      `json:"lastReadBytes,omitempty"`
}

// DatasourceCoverage is the cheap Admin read model for one datasource's
// operator-facing media coverage.
type DatasourceCoverage struct {
	FoundMedias      DatasourceCoverageMetric `json:"foundMedias"`
	BrowsableMedias  DatasourceCoverageMetric `json:"browsableMedias"`
	SearchableMedias DatasourceCoverageMetric `json:"searchableMedias"`
	Issues           DatasourceCoverageMetric `json:"issues"`
}

// DatasourceCoverageMetric is a single datasource-scoped coverage count from
// the Admin stats snapshot.
type DatasourceCoverageMetric struct {
	Status     string     `json:"status"`
	Count      int        `json:"count"`
	TotalCount int        `json:"totalCount,omitempty"`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty"`
}

// DatasourceIndexingStatus is the Admin-facing ingestion status for one datasource.
type DatasourceIndexingStatus struct {
	SourceKey                         string              `json:"sourceKey"`
	Name                              string              `json:"name"`
	Kind                              string              `json:"kind"`
	URL                               string              `json:"url,omitempty"`
	RootKey                           string              `json:"rootKey,omitempty"`
	RootPath                          string              `json:"rootPath,omitempty"`
	IngestionKind                     string              `json:"ingestionKind"`
	IndexingEnabled                   bool                `json:"indexingEnabled"`
	Status                            string              `json:"status"`
	LatestAssetLimit                  int                 `json:"latestAssetLimit,omitempty"`
	ActiveAssets                      int                 `json:"activeAssets"`
	OutOfScopeAssets                  int                 `json:"outOfScopeAssets,omitempty"`
	MissingAssets                     int                 `json:"missingAssets,omitempty"`
	DiscoveredLocations               int                 `json:"discoveredLocations,omitempty"`
	ActiveLocations                   int                 `json:"activeLocations,omitempty"`
	MissingLocations                  int                 `json:"missingLocations,omitempty"`
	BlockedLocations                  int                 `json:"blockedLocations,omitempty"`
	RunningPhase0Scans                int                 `json:"runningPhase0Scans,omitempty"`
	QueuedMetadataJobs                int                 `json:"queuedMetadataJobs,omitempty"`
	SettlingMetadataJobs              int                 `json:"settlingMetadataJobs,omitempty"`
	RunningMetadataJobs               int                 `json:"runningMetadataJobs,omitempty"`
	FailedMetadataJobs                int                 `json:"failedMetadataJobs,omitempty"`
	PendingThumbnailJobs              int                 `json:"pendingThumbnailJobs,omitempty"`
	QueuedThumbnailJobs               int                 `json:"queuedThumbnailJobs,omitempty"`
	RunningThumbnailJobs              int                 `json:"runningThumbnailJobs,omitempty"`
	FailedThumbnailJobs               int                 `json:"failedThumbnailJobs,omitempty"`
	EmbeddingStatus                   string              `json:"embeddingStatus,omitempty"`
	EmbeddingModelID                  string              `json:"embeddingModelId,omitempty"`
	EmbeddingEligible                 int                 `json:"embeddingEligibleAssets,omitempty"`
	EmbeddingCompleted                int                 `json:"embeddingCompletedVectors,omitempty"`
	EmbeddingIndexed                  int                 `json:"embeddingIndexedVectors,omitempty"`
	FailedEmbeddingJobs               int                 `json:"failedEmbeddingJobs,omitempty"`
	EmbeddingPendingIndexJobs         int                 `json:"embeddingPendingIndexJobs,omitempty"`
	EmbeddingFailedIndexJobs          int                 `json:"embeddingFailedIndexJobs,omitempty"`
	EmbeddingLastPublishedAt          *time.Time          `json:"embeddingLastPublishedAt,omitempty"`
	EmbeddingRemaining                int                 `json:"embeddingRemainingVectors,omitempty"`
	EmbeddingLastError                string              `json:"embeddingLastError,omitempty"`
	Coverage                          *DatasourceCoverage `json:"coverage,omitempty"`
	LastRunStatus                     string              `json:"lastRunStatus,omitempty"`
	LastRunAt                         *time.Time          `json:"lastRunAt,omitempty"`
	LastQuickScanAt                   *time.Time          `json:"lastQuickScanAt,omitempty"`
	LastReconciliationAt              *time.Time          `json:"lastReconciliationAt,omitempty"`
	LastContentVerificationAt         *time.Time          `json:"lastContentVerificationAt,omitempty"`
	ContentVerificationStartedAt      *time.Time          `json:"contentVerificationStartedAt,omitempty"`
	ContentVerificationStatus         string              `json:"contentVerificationStatus,omitempty"`
	ContentVerificationSkipReason     string              `json:"contentVerificationSkipReason,omitempty"`
	ContentVerificationProcessedFiles int                 `json:"contentVerificationProcessedFiles,omitempty"`
	ContentVerificationVerifiedFiles  int                 `json:"contentVerificationVerifiedFiles,omitempty"`
	ContentVerificationChangedFiles   int                 `json:"contentVerificationChangedFiles,omitempty"`
	ContentVerificationRunFailures    int                 `json:"contentVerificationRunFailures,omitempty"`
	ContentVerificationReadBytes      int64               `json:"contentVerificationReadBytes,omitempty"`
	ContentVerificationFailures       int                 `json:"contentVerificationFailures,omitempty"`
	LastFullSyncAt                    *time.Time          `json:"lastFullSyncAt,omitempty"`
	LastIncrementalSyncAt             *time.Time          `json:"lastIncrementalSyncAt,omitempty"`
	LastError                         string              `json:"lastError,omitempty"`
}

// DatasourceIndexingRunOptions selects a manual datasource ingestion kick.
type DatasourceIndexingRunOptions struct {
	SourceKey string
	Kind      string
	Mode      string
}

// DatasourceIndexingRunResponse reports one manual ingestion kick across one or
// more datasource adapters.
type DatasourceIndexingRunResponse struct {
	Results   []DatasourceIndexingRunResult      `json:"results"`
	Metadata  *catalog.LocalMetadataBatchResult  `json:"metadata,omitempty"`
	Thumbnail *catalog.LocalThumbnailBatchResult `json:"thumbnail,omitempty"`
}

// DatasourceIndexingRunResult is a normalized result for an ingestion adapter.
type DatasourceIndexingRunResult struct {
	SourceKey        string    `json:"sourceKey"`
	Kind             string    `json:"kind"`
	IngestionKind    string    `json:"ingestionKind"`
	Mode             string    `json:"mode,omitempty"`
	Status           string    `json:"status"`
	FetchedAssets    int       `json:"fetchedAssets,omitempty"`
	ActiveAssets     int       `json:"activeAssets,omitempty"`
	OutOfScopeAssets int       `json:"outOfScopeAssets,omitempty"`
	MissingAssets    int       `json:"missingAssets,omitempty"`
	DiscoveredPaths  int       `json:"discoveredPaths,omitempty"`
	ChangedPaths     int       `json:"changedPaths,omitempty"`
	QueuedMetadata   int       `json:"queuedMetadata,omitempty"`
	SkippedPaths     int       `json:"skippedPaths,omitempty"`
	StartedAt        time.Time `json:"startedAt"`
	CompletedAt      time.Time `json:"completedAt"`
	LastError        string    `json:"lastError,omitempty"`
}

// DatasourceIndexingStatus returns the Admin UI read model for datasource work.
// It never waits for expensive live catalog aggregation. If no snapshot exists
// yet, it returns a lightweight config/active-worker view and starts a best
// effort refresh in the background.
func (a *AgentRuntime) DatasourceIndexingStatus(ctx context.Context) (DatasourceIndexingResponse, error) {
	a.markDatasourceIndexingAdminAccess(time.Now().UTC())
	catalogService := a.catalogService()
	if snapshot, ok := a.datasourceIndexingSnapshot(ctx, catalogService); ok {
		a.scheduleDatasourceIndexingSnapshotRefresh(catalogService)
		return snapshot, nil
	}
	response := a.emptyDatasourceIndexingResponse()
	if stats := a.cachedAssetProcessingStatsForAdmin(ctx, catalogService); !datasourceStatusesArePassthroughOnly(response.Datasources) && !stats.Empty() {
		response.Datasources = applyAggregateProcessingStatsToDatasourceStatus(response.Datasources, stats)
		response.Tasks = a.datasourceTaskStatuses(ctx, response.Datasources, stats)
		response.StatusSnapshotAt = timePtr(stats.RefreshedAt)
		response.StatusSnapshotUsed = true
	}
	a.scheduleDatasourceIndexingSnapshotRefresh(catalogService)
	return response, nil
}

// StartDatasourceIndexingStatusRefresh warms the Admin datasource task read model
// while an Admin client has recently requested datasource status.
func (a *AgentRuntime) StartDatasourceIndexingStatusRefresh() {
	if a == nil {
		return
	}
	a.datasourceStatusRefreshMu.Lock()
	defer a.datasourceStatusRefreshMu.Unlock()
	if a.datasourceStatusCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.datasourceStatusCancel = cancel
	a.datasourceStatusWG.Add(1)
	go a.runDatasourceIndexingStatusRefreshLoop(ctx)
}

func (a *AgentRuntime) stopDatasourceIndexingStatusRefresh() {
	a.datasourceStatusRefreshMu.Lock()
	cancel := a.datasourceStatusCancel
	a.datasourceStatusCancel = nil
	a.datasourceStatusRefreshMu.Unlock()
	if cancel != nil {
		cancel()
		a.datasourceStatusWG.Wait()
	}
}

func (a *AgentRuntime) runDatasourceIndexingStatusRefreshLoop(ctx context.Context) {
	defer a.datasourceStatusWG.Done()
	for {
		if !a.datasourceIndexingAdminActive(time.Now().UTC()) {
			if !sleepDatasourceIndexingRefreshLoop(ctx, assetProcessingStatsRefreshMinAge) {
				return
			}
			continue
		}
		a.runDatasourceIndexingSnapshotRefresh(a.catalogService(), assetProcessingStatsRefreshMinAge)
		if !sleepDatasourceIndexingRefreshLoop(ctx, assetProcessingStatsRefreshMinAge) {
			return
		}
	}
}

func sleepDatasourceIndexingRefreshLoop(ctx context.Context, interval time.Duration) bool {
	if interval <= 0 {
		return true
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (a *AgentRuntime) markDatasourceIndexingAdminAccess(now time.Time) {
	if a == nil {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	a.datasourceTaskMu.Lock()
	a.datasourceStatusLastAdminAccess = now.UTC()
	a.datasourceTaskMu.Unlock()
}

func (a *AgentRuntime) datasourceIndexingAdminActive(now time.Time) bool {
	if a == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	a.datasourceTaskMu.Lock()
	lastAccess := a.datasourceStatusLastAdminAccess
	a.datasourceTaskMu.Unlock()
	return !lastAccess.IsZero() && now.Sub(lastAccess) <= datasourceIndexingAdminActiveWindow
}

// RefreshDatasourceIndexingStatus runs a live datasource status refresh and
// stores the result for subsequent UI reads.
func (a *AgentRuntime) RefreshDatasourceIndexingStatus(ctx context.Context) (DatasourceIndexingResponse, error) {
	a.markDatasourceIndexingAdminAccess(time.Now().UTC())
	return a.refreshDatasourceIndexingSnapshot(ctx, a.catalogService())
}

// CachedDatasourceIndexingStatus returns the last successful Admin datasource
// status snapshot without starting a live refresh.
func (a *AgentRuntime) CachedDatasourceIndexingStatus(ctx context.Context) (DatasourceIndexingResponse, bool) {
	return a.datasourceIndexingSnapshot(ctx, a.catalogService())
}

func (a *AgentRuntime) buildDatasourceIndexingStatus(ctx context.Context, catalogService *catalog.Service) (DatasourceIndexingResponse, error) {
	snapshotConfigHash := a.datasourceIndexingConfigHash()
	roots := a.LocalMediaRootStatuses()
	a.mu.RLock()
	summaries := a.datasourceSummariesLocked()
	a.mu.RUnlock()

	response := DatasourceIndexingResponse{
		Roots:              roots,
		Datasources:        make([]DatasourceIndexingStatus, 0, len(summaries)),
		snapshotConfigHash: snapshotConfigHash,
	}
	if catalogService == nil {
		for _, summary := range summaries {
			status := datasourceIndexingStatusFromSummary(summary)
			status.Status = "not_configured"
			response.Datasources = append(response.Datasources, status)
		}
		return response, catalog.ErrNoDatasourceConfigured
	}
	if datasourceSummariesArePassthroughOnly(summaries) {
		for _, summary := range summaries {
			response.Datasources = append(response.Datasources, datasourceIndexingStatusFromSummary(summary))
		}
		return response, nil
	}

	profile := a.semanticEmbeddingCoverageProfile(ctx, catalogService)
	processingStats := a.refreshAssetProcessingStatsForAdmin(ctx, catalogService, profile)

	localStatuses := map[string]catalog.LocalDatasourceScanStatus{}
	if statuses, err := catalogService.LocalDatasourceScanStatuses(ctx); err == nil {
		for _, status := range statuses {
			localStatuses[status.SourceKey] = status
		}
	} else if !errors.Is(err, catalog.ErrNoDatasourceConfigured) {
		return response, err
	}

	for _, summary := range summaries {
		status := datasourceIndexingStatusFromSummary(summary)
		switch normalizedDatasourceKind(summary.Kind) {
		case config.DatasourceKindLocalFiles:
			if localStatus, ok := localStatuses[summary.SourceKey]; ok {
				status.applyLocalStatus(localStatus)
			}
		case config.DatasourceKindImmichIndexed:
			statusCtx, cancelStatus := context.WithTimeout(ctx, datasourceExpensiveStatusTimeout)
			mirrorStatus, err := catalogService.MirrorStatusForDatasource(statusCtx, summary.SourceKey)
			cancelStatus()
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					status.Status = "busy"
				} else {
					status.Status = "unavailable"
				}
				status.LastError = err.Error()
			} else {
				status.applyRemoteStatus(mirrorStatus)
			}
		case config.DatasourceKindImmich:
			status.Status = "passthrough"
		}
		response.Datasources = append(response.Datasources, status)
	}
	coverageCtx, cancelCoverage := context.WithTimeout(ctx, datasourceExpensiveStatusTimeout)
	if err := a.enrichDatasourceEmbeddingCoverageWithProfile(coverageCtx, catalogService, response.Datasources, profile); err != nil {
		cancelCoverage()
		return response, err
	}
	cancelCoverage()
	response.Datasources = applyAggregateProcessingStatsToDatasourceStatus(response.Datasources, processingStats)
	response.Tasks = a.datasourceTaskStatuses(ctx, response.Datasources, processingStats)
	return response, nil
}

func (a *AgentRuntime) refreshAssetProcessingStatsForAdmin(ctx context.Context, catalogService *catalog.Service, profile *catalog.SemanticModelProfileStatus) catalog.AssetProcessingStatsSnapshot {
	if a == nil || catalogService == nil {
		return catalog.AssetProcessingStatsSnapshot{}
	}
	refreshCtx, cancel := context.WithTimeout(ctx, assetProcessingStatsRefreshTimeout)
	stats, err := catalogService.RefreshAssetProcessingStats(refreshCtx, profile, assetProcessingStatsRefreshMinAge)
	cancel()
	if err == nil && !stats.Empty() && a.assetProcessingStatsMatchCurrentSemanticProfile(stats) {
		return stats
	}
	loadCtx, cancelLoad := context.WithTimeout(ctx, datasourceIndexingSnapshotSaveTimeout)
	defer cancelLoad()
	stats, err = catalogService.AssetProcessingStats(loadCtx)
	if err != nil || !a.assetProcessingStatsMatchCurrentSemanticProfile(stats) {
		return catalog.AssetProcessingStatsSnapshot{}
	}
	return stats
}

func (a *AgentRuntime) refreshAssetProcessingStatsReadModel(ctx context.Context, catalogService *catalog.Service) (catalog.AssetProcessingStatsSnapshot, error) {
	if a == nil || catalogService == nil {
		return catalog.AssetProcessingStatsSnapshot{}, nil
	}
	profileCtx, cancelProfile := context.WithTimeout(ctx, datasourceExpensiveStatusTimeout)
	profile := a.semanticEmbeddingCoverageProfile(profileCtx, catalogService)
	cancelProfile()

	refreshCtx, cancelRefresh := context.WithTimeout(ctx, assetProcessingStatsRefreshTimeout)
	defer cancelRefresh()
	stats, err := catalogService.RefreshAssetProcessingStats(refreshCtx, profile, assetProcessingStatsRefreshMinAge)
	return stats, err
}

func (a *AgentRuntime) cachedAssetProcessingStatsForAdmin(ctx context.Context, catalogService *catalog.Service) catalog.AssetProcessingStatsSnapshot {
	if a == nil || catalogService == nil {
		return catalog.AssetProcessingStatsSnapshot{}
	}
	loadCtx, cancel := context.WithTimeout(ctx, datasourceIndexingSnapshotSaveTimeout)
	defer cancel()
	stats, err := catalogService.AssetProcessingStats(loadCtx)
	if err != nil || !a.assetProcessingStatsMatchCurrentSemanticProfile(stats) {
		return catalog.AssetProcessingStatsSnapshot{}
	}
	return stats
}

func (a *AgentRuntime) assetProcessingStatsMatchCurrentSemanticProfile(stats catalog.AssetProcessingStatsSnapshot) bool {
	return stats.MatchesSemanticProfile(a.datasourceIndexingSemanticProfile())
}

// datasourceIndexingSemanticProfile returns the same durable semantic role
// preferred by datasource coverage without probing model runtimes or querying
// live catalog state. A candidate is indexed before it can replace the active
// model, so its read-model identity takes precedence while both are installed.
func (a *AgentRuntime) datasourceIndexingSemanticProfile() *catalog.SemanticModelProfileStatus {
	if a == nil || a.semanticModels == nil {
		return nil
	}
	if candidate, ok := a.semanticModels.InstalledCandidateProfile(); ok {
		return &candidate
	}
	if active, ok := a.semanticModels.InstalledActiveProfile(); ok {
		return &active
	}
	return nil
}

func (a *AgentRuntime) refreshDatasourceIndexingSnapshot(ctx context.Context, catalogService *catalog.Service) (DatasourceIndexingResponse, error) {
	response, err := a.buildDatasourceIndexingStatus(ctx, catalogService)
	if err != nil {
		if datasourceIndexingSnapshotFallbackAllowed(err) {
			if snapshot, ok := a.datasourceIndexingSnapshot(ctx, catalogService); ok {
				return snapshot, nil
			}
			return response, err
		}
		a.invalidateDatasourceIndexingSnapshot(catalogService)
		return response, err
	}
	if !a.rememberDatasourceIndexingSnapshot(catalogService, response) {
		return a.emptyDatasourceIndexingResponse(), nil
	}
	return response, nil
}

func (a *AgentRuntime) scheduleDatasourceIndexingSnapshotRefresh(catalogService *catalog.Service) {
	if a == nil || catalogService == nil {
		return
	}
	if !a.datasourceIndexingAdminActive(time.Now().UTC()) {
		return
	}
	go a.runDatasourceIndexingSnapshotRefresh(catalogService, assetProcessingStatsRefreshMinAge)
}

func (a *AgentRuntime) runDatasourceIndexingSnapshotRefresh(catalogService *catalog.Service, minInterval time.Duration) {
	if a == nil || catalogService == nil {
		return
	}
	a.datasourceTaskMu.Lock()
	if a.datasourceSnapshotBusy {
		if a.datasourceSnapshotStarted.IsZero() || time.Since(a.datasourceSnapshotStarted) < datasourceIndexingSnapshotStaleAfter {
			a.datasourceTaskMu.Unlock()
			return
		}
		a.datasourceSnapshotBusy = false
		a.datasourceSnapshotStarted = time.Time{}
	}
	if minInterval > 0 && !a.datasourceSnapshotFinished.IsZero() && time.Since(a.datasourceSnapshotFinished) < minInterval {
		a.datasourceTaskMu.Unlock()
		return
	}
	a.datasourceSnapshotBusy = true
	a.datasourceSnapshotStarted = time.Now().UTC()
	a.datasourceTaskMu.Unlock()

	defer func() {
		a.datasourceTaskMu.Lock()
		a.datasourceSnapshotBusy = false
		a.datasourceSnapshotStarted = time.Time{}
		a.datasourceSnapshotFinished = time.Now().UTC()
		a.datasourceTaskMu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), datasourceIndexingSnapshotTimeout)
	defer cancel()
	if stats, err := a.refreshAssetProcessingStatsReadModel(ctx, catalogService); err != nil {
		log.Printf("timich-agent datasource task stats refresh failed error=%v", err)
	} else {
		a.rememberDatasourceTaskStatsSnapshot(context.Background(), catalogService, stats)
	}
	if _, err := a.refreshDatasourceIndexingSnapshot(ctx, catalogService); err != nil && !datasourceIndexingSnapshotFallbackAllowed(err) {
		log.Printf("timich-agent datasource indexing snapshot refresh failed error=%v", err)
	}
}

func (a *AgentRuntime) datasourceIndexingSnapshot(ctx context.Context, catalogService *catalog.Service) (DatasourceIndexingResponse, bool) {
	if a == nil {
		return DatasourceIndexingResponse{}, false
	}
	a.datasourceTaskMu.Lock()
	if a.datasourceSnapshotInvalid {
		a.datasourceTaskMu.Unlock()
		return DatasourceIndexingResponse{}, false
	}
	if a.datasourceSnapshot != nil {
		if a.datasourceSnapshotHash != "" && a.datasourceSnapshotHash != a.datasourceIndexingConfigHash() {
			a.datasourceSnapshot = nil
			a.datasourceSnapshotAt = time.Time{}
			a.datasourceSnapshotHash = ""
			a.datasourceTaskMu.Unlock()
			return DatasourceIndexingResponse{}, false
		}
		snapshot := *a.datasourceSnapshot
		snapshotAt := a.datasourceSnapshotAt
		a.datasourceTaskMu.Unlock()
		snapshot.StatusSnapshotAt = timePtr(snapshotAt)
		snapshot.StatusSnapshotUsed = true
		snapshot = a.normalizeDatasourceIndexingSnapshot(snapshot)
		return snapshot, true
	}
	a.datasourceTaskMu.Unlock()

	if catalogService == nil {
		return DatasourceIndexingResponse{}, false
	}
	loadCtx, cancel := context.WithTimeout(ctx, datasourceIndexingSnapshotSaveTimeout)
	defer cancel()
	stored, ok, err := catalogService.AdminStatusSnapshot(loadCtx, datasourceIndexingStatusSnapshotKey)
	if err != nil || !ok || len(stored.Payload) == 0 {
		return DatasourceIndexingResponse{}, false
	}
	var payload datasourceIndexingSnapshotPayload
	if err := json.Unmarshal(stored.Payload, &payload); err != nil || payload.Version == 0 {
		_ = catalogService.DeleteAdminStatusSnapshot(loadCtx, datasourceIndexingStatusSnapshotKey)
		return DatasourceIndexingResponse{}, false
	}
	if payload.ConfigHash == "" || payload.ConfigHash != a.datasourceIndexingConfigHash() {
		_ = catalogService.DeleteAdminStatusSnapshot(loadCtx, datasourceIndexingStatusSnapshotKey)
		return DatasourceIndexingResponse{}, false
	}
	snapshot := a.normalizePersistedDatasourceIndexingSnapshot(payload.Response)
	snapshot.snapshotConfigHash = payload.ConfigHash
	if !stored.UpdatedAt.IsZero() {
		snapshot.StatusSnapshotAt = timePtr(stored.UpdatedAt)
	}
	snapshot.StatusSnapshotUsed = true
	a.datasourceTaskMu.Lock()
	if a.datasourceSnapshotInvalid {
		a.datasourceTaskMu.Unlock()
		return DatasourceIndexingResponse{}, false
	}
	if cloned, ok := cloneDatasourceIndexingResponse(snapshot); ok {
		cloned.StatusSnapshotUsed = false
		a.datasourceSnapshot = &cloned
		a.datasourceSnapshotHash = payload.ConfigHash
		if snapshot.StatusSnapshotAt != nil {
			a.datasourceSnapshotAt = *snapshot.StatusSnapshotAt
		}
	}
	a.datasourceTaskMu.Unlock()
	snapshot = a.normalizeDatasourceIndexingSnapshot(snapshot)
	return snapshot, true
}

func (a *AgentRuntime) normalizeDatasourceIndexingSnapshot(response DatasourceIndexingResponse) DatasourceIndexingResponse {
	response.Tasks = preferSearchIndexQueuedTargetWait(response.Tasks)
	response.Tasks = normalizeDatasourceTaskWorkerState(response.Tasks, a.effectiveHeavyTaskWorkers())
	a.applyDatasourceTaskActiveSnapshot(response.Tasks)
	response.Tasks = normalizeDatasourceTaskDependencies(response.Tasks)
	return response
}

func preferSearchIndexQueuedTargetWait(tasks []DatasourceTaskStatus) []DatasourceTaskStatus {
	embeddingIndex := -1
	searchIndex := -1
	for index, task := range tasks {
		switch task.Phase {
		case "embeddings":
			embeddingIndex = index
		case "search_index":
			searchIndex = index
		}
	}
	if embeddingIndex < 0 || searchIndex < 0 {
		return tasks
	}
	searchTask := tasks[searchIndex]
	if searchTask.ActiveTasks > 0 || searchTask.Status == "running" || searchTask.FailedTasks > 0 {
		return tasks
	}
	embeddingQueued := tasks[embeddingIndex].QueuedTasks
	if target, ok := semanticIndexPartialPublishWaitTarget(searchTask.CompletedTasks, searchTask.QueuedTasks, embeddingQueued, searchTask.FailedTasks); ok {
		searchTask.WaitingReason = datasourceTaskWaitingQueuedTarget
		searchTask.WaitingQueuedTarget = target
		searchTask.NextRunAt = nil
	} else if searchTask.WaitingReason == datasourceTaskWaitingQueuedTarget {
		searchTask.WaitingReason = ""
		searchTask.WaitingQueuedTarget = 0
		searchTask.NextRunAt = nil
	}
	tasks[searchIndex] = searchTask
	return tasks
}

func (a *AgentRuntime) normalizePersistedDatasourceIndexingSnapshot(response DatasourceIndexingResponse) DatasourceIndexingResponse {
	for index := range response.Datasources {
		response.Datasources[index].RunningPhase0Scans = 0
		response.Datasources[index].RunningMetadataJobs = 0
		response.Datasources[index].RunningThumbnailJobs = 0
		if response.Datasources[index].Status == "running" {
			response.Datasources[index].Status = "idle"
		}
	}
	for index := range response.Tasks {
		response.Tasks[index].ActiveTasks = 0
		response.Tasks[index].ActiveTasksUnknown = false
		if response.Tasks[index].Status == "running" {
			response.Tasks[index].Status = ""
		}
	}
	return a.normalizeDatasourceIndexingSnapshot(response)
}

func (a *AgentRuntime) rememberDatasourceIndexingSnapshot(catalogService *catalog.Service, response DatasourceIndexingResponse) bool {
	if a == nil {
		return false
	}
	return a.rememberDatasourceIndexingSnapshotAt(catalogService, response, time.Now().UTC())
}

func (a *AgentRuntime) rememberDatasourceIndexingSnapshotAt(catalogService *catalog.Service, response DatasourceIndexingResponse, snapshotAt time.Time) bool {
	if a == nil {
		return false
	}
	if snapshotAt.IsZero() {
		snapshotAt = time.Now().UTC()
	}
	if strings.TrimSpace(response.snapshotConfigHash) == "" {
		response.snapshotConfigHash = a.datasourceIndexingConfigHash()
	}
	expectedConfigHash := response.snapshotConfigHash
	if expectedConfigHash == "" || expectedConfigHash != a.datasourceIndexingConfigHash() {
		return false
	}
	snapshot, ok := a.rememberDatasourceIndexingSnapshotInMemory(response, snapshotAt)
	if !ok {
		return false
	}

	if catalogService == nil {
		return expectedConfigHash == a.datasourceIndexingConfigHash()
	}
	persistedSnapshot, ok := cloneDatasourceIndexingResponse(snapshot)
	if !ok {
		return false
	}
	persistedSnapshot = a.normalizePersistedDatasourceIndexingSnapshot(persistedSnapshot)
	persistedSnapshot.StatusSnapshotAt = nil
	persistedSnapshot.StatusSnapshotUsed = false
	payload, err := json.Marshal(datasourceIndexingSnapshotPayload{
		Version:    1,
		ConfigHash: expectedConfigHash,
		Response:   persistedSnapshot,
	})
	if err != nil {
		return false
	}
	if expectedConfigHash != a.datasourceIndexingConfigHash() {
		return false
	}
	saveCtx, cancel := context.WithTimeout(context.Background(), datasourceIndexingSnapshotSaveTimeout)
	defer cancel()
	_ = catalogService.SaveAdminStatusSnapshot(saveCtx, datasourceIndexingStatusSnapshotKey, payload, snapshotAt)
	return expectedConfigHash == a.datasourceIndexingConfigHash()
}

func (a *AgentRuntime) rememberDatasourceIndexingSnapshotInMemory(response DatasourceIndexingResponse, snapshotAt time.Time) (DatasourceIndexingResponse, bool) {
	if a == nil {
		return DatasourceIndexingResponse{}, false
	}
	if strings.TrimSpace(response.snapshotConfigHash) == "" {
		response.snapshotConfigHash = a.datasourceIndexingConfigHash()
	}
	expectedConfigHash := response.snapshotConfigHash
	if expectedConfigHash == "" || expectedConfigHash != a.datasourceIndexingConfigHash() {
		return DatasourceIndexingResponse{}, false
	}
	response.StatusSnapshotAt = timePtr(snapshotAt)
	response.StatusSnapshotUsed = false
	response = a.mergeDatasourceDiscoveryLastCompletedAt(response)
	response.Tasks = normalizeDatasourceTaskDependencies(response.Tasks)
	snapshot, ok := cloneDatasourceIndexingResponse(response)
	if !ok {
		return DatasourceIndexingResponse{}, false
	}
	a.datasourceTaskMu.Lock()
	a.datasourceSnapshot = &snapshot
	a.datasourceSnapshotAt = snapshotAt
	a.datasourceSnapshotHash = expectedConfigHash
	a.datasourceSnapshotInvalid = false
	a.datasourceTaskMu.Unlock()
	return snapshot, true
}

// Task transitions update the Admin read model as part of the task contract.
// The UI should normally read this cheap snapshot instead of doing live counts.
func (a *AgentRuntime) rememberDatasourceTaskReadModel(catalogService *catalog.Service, response DatasourceIndexingResponse) {
	if catalogService != nil {
		a.rememberDatasourceIndexingSnapshot(catalogService, response)
		return
	}
	a.rememberDatasourceIndexingSnapshotInMemory(response, time.Now().UTC())
}

func (a *AgentRuntime) rememberDatasourceTaskStatsSnapshot(ctx context.Context, catalogService *catalog.Service, stats catalog.AssetProcessingStatsSnapshot) {
	if a == nil || stats.Empty() || (catalogService != nil && !a.assetProcessingStatsMatchCurrentSemanticProfile(stats)) {
		return
	}
	response, ok := a.datasourceIndexingSnapshot(ctx, catalogService)
	if !ok {
		response = a.emptyDatasourceIndexingResponse()
	}
	response.StatusSnapshotUsed = false
	response.StatusSnapshotAt = nil
	if datasourceStatusesArePassthroughOnly(response.Datasources) {
		response.Tasks = nil
		a.rememberDatasourceIndexingSnapshotAt(catalogService, response, stats.RefreshedAt)
		return
	}
	response.Datasources = applyAggregateProcessingStatsToDatasourceStatus(response.Datasources, stats)
	response.Tasks = a.datasourceTaskStatuses(ctx, response.Datasources, stats)
	a.rememberDatasourceIndexingSnapshotAt(catalogService, response, stats.RefreshedAt)
}

func (a *AgentRuntime) mergeDatasourceDiscoveryLastCompletedAt(response DatasourceIndexingResponse) DatasourceIndexingResponse {
	if datasourceStatusesArePassthroughOnly(response.Datasources) {
		response.Tasks = nil
		return response
	}
	previous, previousQuick, previousReconciliation := a.datasourceDiscoveryCompletionSnapshot()
	if previous == nil && previousQuick == nil && previousReconciliation == nil {
		return response
	}
	found := false
	for index := range response.Tasks {
		if response.Tasks[index].Phase != "phase0" {
			continue
		}
		found = true
		response.Tasks[index].LastCompletedAt = latestTimePtr(response.Tasks[index].LastCompletedAt, previous)
		response.Tasks[index].LastQuickScanAt = latestTimePtr(response.Tasks[index].LastQuickScanAt, previousQuick)
		response.Tasks[index].LastReconciliationAt = latestTimePtr(response.Tasks[index].LastReconciliationAt, previousReconciliation)
		break
	}
	if !found {
		response.Tasks = append(response.Tasks, DatasourceTaskStatus{
			Phase:                "phase0",
			Label:                "Media discovery",
			Note:                 datasourceTaskNoteMediaDiscovery,
			Status:               "idle",
			LastCompletedAt:      previous,
			LastQuickScanAt:      previousQuick,
			LastReconciliationAt: previousReconciliation,
		})
	}
	return response
}

func (a *AgentRuntime) datasourceDiscoveryCompletionSnapshot() (*time.Time, *time.Time, *time.Time) {
	if a == nil {
		return nil, nil, nil
	}
	a.datasourceTaskMu.Lock()
	defer a.datasourceTaskMu.Unlock()
	if a.datasourceSnapshot == nil {
		return nil, nil, nil
	}
	for _, task := range a.datasourceSnapshot.Tasks {
		if task.Phase != "phase0" {
			continue
		}
		var lastCompletedAt *time.Time
		var lastQuickScanAt *time.Time
		var lastReconciliationAt *time.Time
		if task.LastCompletedAt != nil && !task.LastCompletedAt.IsZero() {
			lastCompletedAt = timePtr(task.LastCompletedAt.UTC())
		}
		if task.LastQuickScanAt != nil && !task.LastQuickScanAt.IsZero() {
			lastQuickScanAt = timePtr(task.LastQuickScanAt.UTC())
		}
		if task.LastReconciliationAt != nil && !task.LastReconciliationAt.IsZero() {
			lastReconciliationAt = timePtr(task.LastReconciliationAt.UTC())
		}
		return lastCompletedAt, lastQuickScanAt, lastReconciliationAt
	}
	return nil, nil, nil
}

func cloneDatasourceIndexingResponse(response DatasourceIndexingResponse) (DatasourceIndexingResponse, bool) {
	snapshotConfigHash := response.snapshotConfigHash
	payload, err := json.Marshal(response)
	if err != nil {
		return DatasourceIndexingResponse{}, false
	}
	var cloned DatasourceIndexingResponse
	if err := json.Unmarshal(payload, &cloned); err != nil {
		return DatasourceIndexingResponse{}, false
	}
	cloned.snapshotConfigHash = snapshotConfigHash
	return cloned, true
}

func datasourceIndexingResponseSnapshotSafe(response DatasourceIndexingResponse) bool {
	return !datasourceIndexingResponseHasBusyState(response)
}

func datasourceIndexingResponseHasBusyState(response DatasourceIndexingResponse) bool {
	for _, datasource := range response.Datasources {
		if datasource.Status == "busy" || datasource.EmbeddingStatus == "busy" {
			return true
		}
	}
	for _, task := range response.Tasks {
		if task.Status == "busy" ||
			task.ActiveTasksUnknown ||
			task.QueuedTasksUnknown ||
			task.FailedTasksUnknown {
			return true
		}
	}
	return false
}

func datasourceIndexingSnapshotFallbackAllowed(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func (a *AgentRuntime) invalidateDatasourceIndexingSnapshot(catalogService *catalog.Service) {
	if a == nil {
		return
	}
	a.datasourceTaskMu.Lock()
	a.datasourceSnapshot = nil
	a.datasourceSnapshotAt = time.Time{}
	a.datasourceSnapshotHash = ""
	a.datasourceSnapshotStarted = time.Time{}
	a.datasourceSnapshotInvalid = true
	a.datasourceTaskMu.Unlock()

	if catalogService == nil {
		return
	}
	deleteCtx, cancel := context.WithTimeout(context.Background(), datasourceIndexingSnapshotSaveTimeout)
	defer cancel()
	_ = catalogService.DeleteAdminStatusSnapshot(deleteCtx, datasourceIndexingStatusSnapshotKey)
}

func (a *AgentRuntime) datasourceIndexingConfigHash() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	datasources := append([]config.DatasourceConfig(nil), a.config.Datasources...)
	roots := append([]config.LocalMediaRootConfig(nil), a.config.LocalMediaRoots...)
	a.mu.RUnlock()

	parts := make([]string, 0, len(datasources)+len(roots)+2)
	parts = append(parts, "snapshot_schema\x00semantic-profile-v1")
	semanticProfile := a.datasourceIndexingSemanticProfile()
	semanticModelID := ""
	semanticVectorSpaceID := ""
	if semanticProfile != nil {
		semanticModelID = strings.TrimSpace(semanticProfile.ModelID)
		semanticVectorSpaceID = strings.TrimSpace(semanticProfile.VectorSpaceID)
	}
	parts = append(parts, strings.Join([]string{
		"semantic_profile",
		semanticModelID,
		semanticVectorSpaceID,
	}, "\x00"))
	for _, datasource := range datasources {
		kind := normalizedDatasourceKind(datasource.Kind)
		parts = append(parts, strings.Join([]string{
			"datasource",
			strings.TrimSpace(datasource.SourceKey),
			kind,
			strings.TrimSpace(datasource.URL),
			strings.TrimSpace(datasource.RootKey),
			boolSnapshotValue(datasourceIndexingEnabled(datasource)),
		}, "\x00"))
	}
	for _, root := range roots {
		parts = append(parts, strings.Join([]string{
			"local_root",
			strings.TrimSpace(root.Key),
			strings.TrimSpace(root.Path),
		}, "\x00"))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])
}

func boolSnapshotValue(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func timePtr(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

// CatalogDeduplicationStatus returns the canonical catalog integrity summary on demand.
func (a *AgentRuntime) CatalogDeduplicationStatus(ctx context.Context) (catalog.CatalogDeduplicationStatus, error) {
	catalogService := a.catalogService()
	if catalogService == nil {
		return catalog.CatalogDeduplicationStatus{}, catalog.ErrNoDatasourceConfigured
	}
	return catalogService.CatalogDeduplicationStatus(ctx)
}

// RepairCatalogDeduplication rebuilds canonical catalog rows from datasource source rows.
func (a *AgentRuntime) RepairCatalogDeduplication(ctx context.Context) (catalog.CatalogDeduplicationStatus, error) {
	catalogService := a.catalogService()
	if catalogService == nil {
		return catalog.CatalogDeduplicationStatus{}, catalog.ErrNoDatasourceConfigured
	}
	return catalogService.RebuildCatalogCanonicalAssets(ctx)
}

func datasourceIndexingStatusFromSummary(summary DatasourceSummary) DatasourceIndexingStatus {
	kind := normalizedDatasourceKind(summary.Kind)
	status := DatasourceIndexingStatus{
		SourceKey: summary.SourceKey,
		Name:      summary.Name,
		Kind:      kind,
		URL:       summary.URL,
		RootKey:   summary.RootKey,
		RootPath:  summary.RootPath,
		Status:    "not_indexed",
	}
	switch kind {
	case config.DatasourceKindLocalFiles:
		status.IngestionKind = datasourceIngestionFilesystem
		status.IndexingEnabled = strings.TrimSpace(summary.RootKey) != ""
	case config.DatasourceKindImmichIndexed:
		status.IngestionKind = datasourceIngestionRemoteAPI
		status.IndexingEnabled = true
	case config.DatasourceKindImmich:
		status.IngestionKind = datasourceIngestionRemoteAPI
		status.Status = "passthrough"
	}
	return status
}

func applyAggregateProcessingStatsToDatasourceStatus(datasources []DatasourceIndexingStatus, stats catalog.AssetProcessingStatsSnapshot) []DatasourceIndexingStatus {
	if stats.Empty() {
		return datasources
	}
	datasources = applyDatasourceCoverageStats(datasources, stats)
	if aggregateProcessingStatsTotal(stats) == 0 {
		return datasources
	}
	for index := range datasources {
		datasource := datasources[index]
		if !datasource.IndexingEnabled || datasourceHasCatalogSignal(datasource) {
			continue
		}
		switch strings.TrimSpace(datasource.Status) {
		case "", "not_indexed", "idle":
			datasources[index].Status = "indexing"
		}
	}
	return datasources
}

func applyDatasourceCoverageStats(datasources []DatasourceIndexingStatus, stats catalog.AssetProcessingStatsSnapshot) []DatasourceIndexingStatus {
	if stats.Empty() {
		return datasources
	}
	for index := range datasources {
		coverage := datasourceCoverageFromStats(stats, datasources[index].SourceKey)
		if coverage == nil {
			continue
		}
		datasources[index].Coverage = coverage
	}
	return datasources
}

func datasourceCoverageFromStats(stats catalog.AssetProcessingStatsSnapshot, sourceKey string) *DatasourceCoverage {
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" {
		return nil
	}
	stages := []string{
		catalog.AssetProcessingStageFoundMedias,
		catalog.AssetProcessingStageBrowsable,
		catalog.AssetProcessingStageSearchable,
		catalog.AssetProcessingStageIssues,
	}
	hasCoverage := false
	for _, stage := range stages {
		if stats.HasStageForScope(sourceKey, stage) {
			hasCoverage = true
			break
		}
	}
	if !hasCoverage {
		return nil
	}
	return &DatasourceCoverage{
		FoundMedias:      datasourceCoverageMetricFromStats(stats, sourceKey, catalog.AssetProcessingStageFoundMedias),
		BrowsableMedias:  datasourceCoverageMetricFromStats(stats, sourceKey, catalog.AssetProcessingStageBrowsable),
		SearchableMedias: datasourceCoverageMetricFromStats(stats, sourceKey, catalog.AssetProcessingStageSearchable),
		Issues:           datasourceCoverageMetricFromStats(stats, sourceKey, catalog.AssetProcessingStageIssues),
	}
}

func datasourceCoverageMetricFromStats(stats catalog.AssetProcessingStatsSnapshot, sourceKey string, stage string) DatasourceCoverageMetric {
	stat, ok := stats.StatForScope(sourceKey, stage)
	if !ok {
		return DatasourceCoverageMetric{Status: catalog.AssetProcessingStatusUpdating}
	}
	status := strings.TrimSpace(stat.Status)
	if status == "" {
		status = catalog.AssetProcessingStatusReady
	}
	return DatasourceCoverageMetric{
		Status:     status,
		Count:      max(stat.Count, 0),
		TotalCount: max(stat.TotalCount, 0),
		UpdatedAt:  timePtr(stat.RefreshedAt),
	}
}

func aggregateProcessingStatsTotal(stats catalog.AssetProcessingStatsSnapshot) int {
	total := 0
	for _, stage := range []string{
		catalog.AssetProcessingStageMetadata,
		catalog.AssetProcessingStageThumbnails,
		catalog.AssetProcessingStageEmbeddings,
		catalog.AssetProcessingStageSearchIndex,
	} {
		total = max(total, stats.Total(stage))
	}
	return total
}

func datasourceHasCatalogSignal(datasource DatasourceIndexingStatus) bool {
	if datasourceCoverageHasSignal(datasource.Coverage) {
		return true
	}
	return datasource.ActiveAssets > 0 ||
		datasource.OutOfScopeAssets > 0 ||
		datasource.MissingAssets > 0 ||
		datasource.DiscoveredLocations > 0 ||
		datasource.ActiveLocations > 0 ||
		datasource.MissingLocations > 0 ||
		datasource.BlockedLocations > 0 ||
		datasource.RunningPhase0Scans > 0 ||
		datasource.QueuedMetadataJobs > 0 ||
		datasource.RunningMetadataJobs > 0 ||
		datasource.FailedMetadataJobs > 0 ||
		datasource.PendingThumbnailJobs > 0 ||
		datasource.QueuedThumbnailJobs > 0 ||
		datasource.RunningThumbnailJobs > 0 ||
		datasource.FailedThumbnailJobs > 0 ||
		datasource.EmbeddingEligible > 0 ||
		datasource.EmbeddingCompleted > 0 ||
		datasource.EmbeddingIndexed > 0 ||
		datasource.EmbeddingRemaining > 0 ||
		datasource.FailedEmbeddingJobs > 0
}

func datasourceCoverageHasSignal(coverage *DatasourceCoverage) bool {
	if coverage == nil {
		return false
	}
	return coverage.FoundMedias.Count > 0 ||
		coverage.BrowsableMedias.Count > 0 ||
		coverage.SearchableMedias.Count > 0 ||
		coverage.Issues.Count > 0
}

func datasourceIndexingEnabled(datasource config.DatasourceConfig) bool {
	switch normalizedDatasourceKind(datasource.Kind) {
	case config.DatasourceKindLocalFiles:
		return strings.TrimSpace(datasource.RootKey) != ""
	case config.DatasourceKindImmichIndexed:
		return strings.TrimSpace(datasource.URL) != ""
	default:
		return false
	}
}

func (s *DatasourceIndexingStatus) applyRemoteStatus(status catalog.MirrorStatus) {
	s.IndexingEnabled = status.Enabled
	s.Status = status.Status
	if s.Status == "" {
		s.Status = "idle"
	}
	s.LatestAssetLimit = status.LatestAssetLimit
	s.ActiveAssets = status.ActiveCount
	s.OutOfScopeAssets = status.OutOfScopeCount
	s.MissingAssets = status.MissingCount
	s.LastFullSyncAt = status.LastFullSyncAt
	s.LastIncrementalSyncAt = status.LastIncrementalSyncAt
	s.LastError = status.LastError
}

func (s *DatasourceIndexingStatus) applyLocalStatus(status catalog.LocalDatasourceScanStatus) {
	s.Status = status.Phase0Status
	if s.Status == "" {
		s.Status = "idle"
	}
	s.RootKey = status.RootKey
	s.RootPath = status.RootPath
	s.DiscoveredLocations = status.DiscoveredLocations
	s.ActiveLocations = status.ActiveLocations
	s.MissingLocations = status.MissingLocations
	s.BlockedLocations = status.BlockedLocations
	s.ActiveAssets = status.ActiveAssets
	s.RunningPhase0Scans = status.RunningPhase0Scans
	s.QueuedMetadataJobs = status.QueuedMetadataJobs
	s.SettlingMetadataJobs = status.SettlingMetadataJobs
	s.RunningMetadataJobs = status.RunningMetadataJobs
	s.FailedMetadataJobs = status.FailedMetadataJobs
	s.PendingThumbnailJobs = status.PendingThumbnailJobs
	s.QueuedThumbnailJobs = status.QueuedThumbnailJobs
	s.RunningThumbnailJobs = status.RunningThumbnailJobs
	s.FailedThumbnailJobs = status.FailedThumbnailJobs
	s.EmbeddingStatus = status.EmbeddingStatus
	s.EmbeddingModelID = status.EmbeddingModelID
	s.EmbeddingEligible = status.EmbeddingEligible
	s.EmbeddingCompleted = status.EmbeddingCompleted
	s.EmbeddingIndexed = status.EmbeddingIndexed
	s.EmbeddingRemaining = status.EmbeddingRemaining
	s.EmbeddingLastError = status.EmbeddingLastError
	s.LastError = status.LastError
	s.LastQuickScanAt = status.LastQuickScanAt
	s.LastReconciliationAt = status.LastReconciliationAt
	s.LastContentVerificationAt = status.LastContentVerificationAt
	s.ContentVerificationStartedAt = status.ContentVerificationStartedAt
	s.ContentVerificationStatus = status.ContentVerificationStatus
	s.ContentVerificationSkipReason = status.ContentVerificationSkipReason
	s.ContentVerificationProcessedFiles = status.ContentVerificationProcessedFiles
	s.ContentVerificationVerifiedFiles = status.ContentVerificationVerifiedFiles
	s.ContentVerificationChangedFiles = status.ContentVerificationChangedFiles
	s.ContentVerificationRunFailures = status.ContentVerificationRunFailures
	s.ContentVerificationReadBytes = status.ContentVerificationReadBytes
	s.ContentVerificationFailures = status.ContentVerificationFailures
	if status.LastRun != nil {
		s.LastRunStatus = status.LastRun.Status
		s.LastRunAt = status.LastRun.CompletedAt
		if s.LastRunAt == nil {
			s.LastRunAt = status.LastRun.StartedAt
		}
	}
}

func (s *DatasourceIndexingStatus) applySemanticBackfillStatus(status catalog.SemanticModelBackfillStatus) {
	if s == nil || (status.ModelID == "" && status.VectorSpaceID == "") {
		return
	}
	s.EmbeddingStatus = status.Status
	s.EmbeddingModelID = status.ModelID
	s.EmbeddingEligible = status.EligibleAssetCount
	s.EmbeddingCompleted = status.CompletedVectorCount
	s.EmbeddingIndexed = status.IndexedVectorCount
	s.FailedEmbeddingJobs = status.FailedVectorCount
	s.EmbeddingPendingIndexJobs = status.PendingIndexJobCount
	s.EmbeddingFailedIndexJobs = status.FailedIndexJobCount
	s.EmbeddingLastPublishedAt = status.LastPublishedAt
	s.EmbeddingRemaining = status.RemainingVectorCount
	s.EmbeddingLastError = ""
}

func (a *AgentRuntime) rememberDatasourceDiscoveryTaskSnapshot(catalogService *catalog.Service, active bool, lastCompletedAt *time.Time, scanResults ...catalog.LocalPhase0ScanResult) {
	if a == nil {
		return
	}
	response, ok := a.datasourceIndexingSnapshot(context.Background(), catalogService)
	if !ok {
		response = a.emptyDatasourceIndexingResponse()
	}
	if datasourceStatusesArePassthroughOnly(response.Datasources) {
		return
	}
	response.StatusSnapshotUsed = false
	response.StatusSnapshotAt = nil
	if len(response.Tasks) == 0 {
		response.Tasks = a.datasourceTaskStatuses(context.Background(), response.Datasources)
	}
	lastQuickScanAt := localPhase0ScanModeCompletedAt(scanResults, datasourceLocalScanModeQuick)
	lastReconciliationAt := localPhase0ScanModeCompletedAt(scanResults, datasourceLocalScanModeReconciliation)
	found := false
	for index := range response.Tasks {
		if response.Tasks[index].Phase != "phase0" {
			continue
		}
		found = true
		response.Tasks[index].ActiveTasksUnknown = false
		response.Tasks[index].QueuedTasks = 0
		response.Tasks[index].QueuedTasksUnknown = false
		response.Tasks[index].WaitingReason = ""
		response.Tasks[index].WaitingQueuedTarget = 0
		response.Tasks[index].NextRunAt = nil
		if active {
			response.Tasks[index].ActiveTasks = 1
		} else {
			response.Tasks[index].ActiveTasks = 0
		}
		if lastCompletedAt != nil {
			response.Tasks[index].LastCompletedAt = timePtr(lastCompletedAt.UTC())
		}
		response.Tasks[index].LastQuickScanAt = latestTimePtr(response.Tasks[index].LastQuickScanAt, lastQuickScanAt)
		response.Tasks[index].LastReconciliationAt = latestTimePtr(response.Tasks[index].LastReconciliationAt, lastReconciliationAt)
		response.Tasks[index].Status = datasourceTaskStatus(response.Tasks[index])
		break
	}
	if !found {
		task := DatasourceTaskStatus{
			Phase:  "phase0",
			Label:  "Media discovery",
			Note:   datasourceTaskNoteMediaDiscovery,
			Status: "idle",
		}
		if active {
			task.ActiveTasks = 1
		}
		if lastCompletedAt != nil {
			task.LastCompletedAt = timePtr(lastCompletedAt.UTC())
		}
		task.LastQuickScanAt = lastQuickScanAt
		task.LastReconciliationAt = lastReconciliationAt
		task.Status = datasourceTaskStatus(task)
		response.Tasks = append(response.Tasks, task)
	}
	a.rememberDatasourceTaskReadModel(catalogService, response)
}

func (a *AgentRuntime) rememberDatasourceTaskActivitySnapshot(catalogService *catalog.Service, phase string, active int) {
	if a == nil {
		return
	}
	phase = strings.TrimSpace(phase)
	if phase == "" {
		return
	}
	a.setDatasourceTaskActive(phase, active)
	response, ok := a.datasourceIndexingSnapshot(context.Background(), catalogService)
	if !ok {
		response = a.emptyDatasourceIndexingResponse()
	}
	if datasourceStatusesArePassthroughOnly(response.Datasources) {
		return
	}
	response.StatusSnapshotUsed = false
	response.StatusSnapshotAt = nil
	if len(response.Tasks) == 0 {
		response.Tasks = a.datasourceTaskStatuses(context.Background(), response.Datasources)
	}
	found := false
	for index := range response.Tasks {
		if response.Tasks[index].Phase != phase {
			continue
		}
		found = true
		response.Tasks[index].ActiveTasks = max(active, 0)
		response.Tasks[index].ActiveTasksUnknown = false
		if active > 0 {
			response.Tasks[index].WaitingReason = ""
			response.Tasks[index].WaitingQueuedTarget = 0
			response.Tasks[index].NextRunAt = nil
		}
		break
	}
	if !found {
		task, ok := datasourceTaskTemplate(phase)
		if !ok {
			return
		}
		task.ActiveTasks = max(active, 0)
		response.Tasks = append(response.Tasks, task)
	}
	response.Tasks = normalizeDatasourceTaskWorkerState(response.Tasks, a.effectiveHeavyTaskWorkers())
	a.applyDatasourceTaskActiveSnapshot(response.Tasks)
	a.rememberDatasourceTaskReadModel(catalogService, response)
}

func datasourceTaskTemplate(phase string) (DatasourceTaskStatus, bool) {
	switch phase {
	case "phase0":
		return DatasourceTaskStatus{Phase: phase, Label: "Media discovery", Note: datasourceTaskNoteMediaDiscovery, Status: "idle"}, true
	case "metadata":
		return DatasourceTaskStatus{Phase: phase, Label: "Metadata", Note: datasourceTaskNoteMetadata, Status: "idle"}, true
	case "thumbnails":
		return DatasourceTaskStatus{Phase: phase, Label: "Thumbnails", Note: datasourceTaskNoteThumbnails, Status: "idle"}, true
	case "content_verification":
		return DatasourceTaskStatus{Phase: phase, Label: "Content verification", Note: datasourceTaskNoteContentVerification, Status: "idle"}, true
	case "embeddings":
		return DatasourceTaskStatus{Phase: phase, Label: "Embeddings", Note: datasourceTaskNoteEmbeddings, Status: "idle"}, true
	case "search_index":
		return DatasourceTaskStatus{Phase: phase, Label: "Search index", Note: datasourceTaskNoteSearchIndex, Status: "idle"}, true
	default:
		return DatasourceTaskStatus{}, false
	}
}

func (a *AgentRuntime) rememberSemanticIndexingProgressSnapshot(catalogService *catalog.Service, sourceStatuses []catalog.SemanticBackfillSource, aggregate catalog.SemanticModelBackfillStatus) {
	if a == nil {
		return
	}
	response, ok := a.datasourceIndexingSnapshot(context.Background(), catalogService)
	if !ok {
		response = a.emptyDatasourceIndexingResponse()
	}
	if datasourceStatusesArePassthroughOnly(response.Datasources) {
		response.Tasks = nil
		a.rememberDatasourceIndexingSnapshot(catalogService, response)
		return
	}
	response.StatusSnapshotUsed = false
	response.StatusSnapshotAt = nil
	changedDatasource := false
	for _, sourceStatus := range sourceStatuses {
		sourceKey := strings.TrimSpace(sourceStatus.SourceKey)
		if sourceKey == "" {
			continue
		}
		for index := range response.Datasources {
			if response.Datasources[index].SourceKey != sourceKey {
				continue
			}
			response.Datasources[index].applySemanticBackfillStatus(sourceStatus.Status)
			changedDatasource = true
			break
		}
	}
	if len(response.Tasks) == 0 || changedDatasource {
		response.Tasks = a.datasourceTaskStatuses(context.Background(), response.Datasources)
	}
	if len(sourceStatuses) == 0 && !semanticBackfillStatusEmpty(aggregate) {
		response.Tasks = applySemanticAggregateToDatasourceTasks(response.Tasks, aggregate, a.effectiveHeavyTaskWorkers())
	}
	a.rememberDatasourceIndexingSnapshot(catalogService, response)
}

func (a *AgentRuntime) emptyDatasourceIndexingResponse() DatasourceIndexingResponse {
	if a == nil {
		return DatasourceIndexingResponse{}
	}
	a.mu.RLock()
	summaries := a.datasourceSummariesLocked()
	a.mu.RUnlock()
	response := DatasourceIndexingResponse{
		Roots:              a.LocalMediaRootStatuses(),
		Datasources:        make([]DatasourceIndexingStatus, 0, len(summaries)),
		snapshotConfigHash: a.datasourceIndexingConfigHash(),
	}
	for _, summary := range summaries {
		response.Datasources = append(response.Datasources, datasourceIndexingStatusFromSummary(summary))
	}
	if !datasourceStatusesArePassthroughOnly(response.Datasources) {
		response.Tasks = a.datasourceTaskStatuses(context.Background(), response.Datasources)
	}
	return response
}

func semanticBackfillStatusEmpty(status catalog.SemanticModelBackfillStatus) bool {
	return status.ModelID == "" &&
		status.VectorSpaceID == "" &&
		status.EligibleAssetCount == 0 &&
		status.CompletedVectorCount == 0 &&
		status.IndexedVectorCount == 0 &&
		status.RemainingVectorCount == 0 &&
		status.FailedVectorCount == 0 &&
		status.PendingIndexJobCount == 0 &&
		status.FailedIndexJobCount == 0
}

func applySemanticAggregateToDatasourceTasks(tasks []DatasourceTaskStatus, status catalog.SemanticModelBackfillStatus, workers int) []DatasourceTaskStatus {
	if len(tasks) == 0 {
		tasks = []DatasourceTaskStatus{
			{Phase: "embeddings", Label: "Embeddings", Note: datasourceTaskNoteEmbeddings},
			{Phase: "search_index", Label: "Search index", Note: datasourceTaskNoteSearchIndex},
		}
	}
	byPhase := map[string]int{}
	for index, task := range tasks {
		byPhase[task.Phase] = index
	}
	ensurePhase := func(phase, label, note string) int {
		if index, ok := byPhase[phase]; ok {
			return index
		}
		tasks = append(tasks, DatasourceTaskStatus{
			Phase:  phase,
			Label:  label,
			Note:   note,
			Status: "idle",
		})
		index := len(tasks) - 1
		byPhase[phase] = index
		return index
	}

	embeddingIndex := ensurePhase("embeddings", "Embeddings", datasourceTaskNoteEmbeddings)
	embeddingTask := tasks[embeddingIndex]
	embeddingActive := max(embeddingTask.ActiveTasks, 0)
	embeddingTask.ActiveTasks = embeddingActive
	embeddingTask.ActiveTasksUnknown = false
	embeddingTask.FailedTasks = max(status.FailedVectorCount, 0)
	embeddingTask.QueuedTasks = max(status.EligibleAssetCount-status.CompletedVectorCount-embeddingTask.FailedTasks, 0)
	embeddingTask.QueuedTasksUnknown = false
	embeddingTask.CompletedTasks = status.CompletedVectorCount
	embeddingTask.TotalTasks = status.EligibleAssetCount
	embeddingTask.FailedTasksUnknown = false
	embeddingTask.WaitingReason = ""
	embeddingTask.WaitingQueuedTarget = 0
	embeddingTask.NextRunAt = nil
	tasks[embeddingIndex] = embeddingTask

	searchIndex := ensurePhase("search_index", "Search index", datasourceTaskNoteSearchIndex)
	searchTask := tasks[searchIndex]
	searchActive := max(searchTask.ActiveTasks, 0)
	searchTask.ActiveTasks = searchActive
	searchTask.ActiveTasksUnknown = false
	searchTask.QueuedTasks = max(status.CompletedVectorCount-status.IndexedVectorCount, 0)
	searchTask.QueuedTasksUnknown = false
	searchTask.CompletedTasks = status.IndexedVectorCount
	searchTask.TotalTasks = status.CompletedVectorCount
	searchTask.FailedTasks = status.FailedIndexJobCount
	searchTask.FailedTasksUnknown = false
	searchTask.WaitingReason = ""
	searchTask.WaitingQueuedTarget = 0
	searchTask.NextRunAt = nil
	if searchTask.ActiveTasks == 0 {
		if target, ok := semanticIndexPartialPublishWaitTarget(
			status.IndexedVectorCount,
			searchTask.QueuedTasks,
			semanticIndexEmbeddingQueued(status),
			status.FailedIndexJobCount,
		); ok {
			searchTask.WaitingReason = datasourceTaskWaitingQueuedTarget
			searchTask.WaitingQueuedTarget = target
		}
	}
	tasks[searchIndex] = searchTask

	if searchTask.ActiveTasks > 0 && embeddingTask.QueuedTasks > 0 {
		embeddingTask.WaitingReason = "search_index"
		embeddingTask.WaitingQueuedTarget = 0
		tasks[embeddingIndex] = embeddingTask
	}
	tasks = normalizeDatasourceTaskWorkerState(tasks, workers)
	return normalizeDatasourceTaskDependencies(tasks)
}

func (a *AgentRuntime) datasourceTaskStatuses(ctx context.Context, datasources []DatasourceIndexingStatus, statsSnapshots ...catalog.AssetProcessingStatsSnapshot) []DatasourceTaskStatus {
	tasks := []DatasourceTaskStatus{
		{
			Phase:  "phase0",
			Label:  "Media discovery",
			Note:   datasourceTaskNoteMediaDiscovery,
			Status: "idle",
		},
		{
			Phase:  "metadata",
			Label:  "Metadata",
			Note:   datasourceTaskNoteMetadata,
			Status: "idle",
		},
		{
			Phase:  "thumbnails",
			Label:  "Thumbnails",
			Note:   datasourceTaskNoteThumbnails,
			Status: "idle",
		},
		{
			Phase:  "embeddings",
			Label:  "Embeddings",
			Note:   datasourceTaskNoteEmbeddings,
			Status: "idle",
		},
		{
			Phase:  "search_index",
			Label:  "Search index",
			Note:   datasourceTaskNoteSearchIndex,
			Status: "idle",
		},
		{
			Phase:  "content_verification",
			Label:  "Content verification",
			Note:   datasourceTaskNoteContentVerification,
			Status: "idle",
		},
	}

	remoteActiveFromStatus := 0
	var mediaDiscoveryLastCompletedAt *time.Time
	var mediaDiscoveryLastQuickScanAt *time.Time
	var mediaDiscoveryLastReconciliationAt *time.Time
	contentVerificationEnabled := false
	localDatasourceCount := 0
	var contentVerificationLatestEventAt *time.Time
	for _, datasource := range datasources {
		if datasource.IngestionKind == datasourceIngestionRemoteAPI && datasource.IndexingEnabled && datasource.Status == "running" {
			tasks[0].ActiveTasks++
			remoteActiveFromStatus++
		}
		mediaDiscoveryLastCompletedAt = latestTimePtr(mediaDiscoveryLastCompletedAt, datasource.LastRunAt)
		mediaDiscoveryLastCompletedAt = latestTimePtr(mediaDiscoveryLastCompletedAt, datasource.LastFullSyncAt)
		mediaDiscoveryLastCompletedAt = latestTimePtr(mediaDiscoveryLastCompletedAt, datasource.LastIncrementalSyncAt)
		if datasource.IngestionKind != datasourceIngestionFilesystem {
			continue
		}
		localDatasourceCount++
		mediaDiscoveryLastQuickScanAt = latestTimePtr(mediaDiscoveryLastQuickScanAt, datasource.LastQuickScanAt)
		mediaDiscoveryLastReconciliationAt = latestTimePtr(mediaDiscoveryLastReconciliationAt, datasource.LastReconciliationAt)
		eventAt := latestTimePtr(datasource.LastContentVerificationAt, datasource.ContentVerificationStartedAt)
		if eventAt != nil && (contentVerificationLatestEventAt == nil || eventAt.After(contentVerificationLatestEventAt.UTC())) {
			contentVerificationLatestEventAt = timePtr(eventAt.UTC())
			tasks[5].LastCompletedAt = datasource.LastContentVerificationAt
			tasks[5].LastRunStartedAt = datasource.ContentVerificationStartedAt
			tasks[5].LastRunStatus = datasource.ContentVerificationStatus
			tasks[5].LastRunReason = datasource.ContentVerificationSkipReason
			tasks[5].LastProcessedFiles = datasource.ContentVerificationProcessedFiles
			tasks[5].LastVerifiedFiles = datasource.ContentVerificationVerifiedFiles
			tasks[5].LastChangedFiles = datasource.ContentVerificationChangedFiles
			tasks[5].LastFailedFiles = datasource.ContentVerificationRunFailures
			tasks[5].LastReadBytes = datasource.ContentVerificationReadBytes
		}
		tasks[1].QueuedTasks += datasource.QueuedMetadataJobs
		tasks[1].SettlingTasks += datasource.SettlingMetadataJobs
		tasks[1].FailedTasks += datasource.FailedMetadataJobs
		tasks[2].QueuedTasks += datasource.PendingThumbnailJobs
		tasks[2].FailedTasks += datasource.FailedThumbnailJobs
		tasks[3].FailedTasks += datasource.FailedEmbeddingJobs
	}
	for _, schedule := range a.localContentVerificationSchedules() {
		if schedule.Duration > 0 {
			contentVerificationEnabled = true
			break
		}
	}
	if localDatasourceCount == 0 {
		tasks[5].Status = "not_applicable"
	} else if !contentVerificationEnabled {
		tasks[5].Status = "disabled"
	}
	if active := a.datasourceDiscoveryActiveCount(); active > remoteActiveFromStatus {
		tasks[0].ActiveTasks += active - remoteActiveFromStatus
	}
	tasks[0].LastCompletedAt = mediaDiscoveryLastCompletedAt
	tasks[0].LastQuickScanAt = mediaDiscoveryLastQuickScanAt
	tasks[0].LastReconciliationAt = mediaDiscoveryLastReconciliationAt

	var processingStats catalog.AssetProcessingStatsSnapshot
	if len(statsSnapshots) > 0 {
		processingStats = statsSnapshots[0]
	}
	applyAssetProcessingStatsToDatasourceTasks(tasks, processingStats)

	tasks[3].ActiveTasks = a.semanticBackfillActiveWorkers()
	tasks[4].ActiveTasks = a.semanticIndexPublishActive()
	a.applyDatasourceTaskActiveSnapshot(tasks)
	semanticTaskCounts := datasourceSemanticTaskCounts(datasources)
	if statsSummary, ok := datasourceSemanticTaskCountsFromAssetStats(processingStats, semanticTaskCounts); ok {
		semanticTaskCounts = statsSummary
	}
	if semanticTaskCounts.known {
		tasks[3].QueuedTasks = semanticTaskCounts.embeddingQueued
		tasks[3].CompletedTasks = semanticTaskCounts.completed
		tasks[3].TotalTasks = semanticTaskCounts.eligible
		tasks[3].FailedTasks = semanticTaskCounts.failedEmbeddingJobs
		tasks[4].QueuedTasks = semanticTaskCounts.unindexed
		tasks[4].CompletedTasks = semanticTaskCounts.indexed
		tasks[4].TotalTasks = semanticTaskCounts.completed
		tasks[4].FailedTasks = semanticTaskCounts.failedIndexJobs
		tasks[4].WaitingReason = ""
		tasks[4].WaitingQueuedTarget = 0
		tasks[4].NextRunAt = nil
	} else if semanticTaskCounts.busy {
		tasks[3].QueuedTasksUnknown = true
		tasks[3].FailedTasksUnknown = true
		tasks[4].QueuedTasksUnknown = true
		tasks[4].FailedTasksUnknown = true
	}

	if tasks[4].ActiveTasks > 0 && tasks[3].QueuedTasks > 0 {
		tasks[3].ActiveTasks = 0
		tasks[3].ActiveTasksUnknown = false
		tasks[3].WaitingReason = datasourceTaskWaitingSearchIndex
		tasks[3].WaitingQueuedTarget = 0
	}
	semanticScheduled := false
	schedule, scheduleOK := a.semanticIndexingSchedule()
	if scheduleOK && schedule.Workers > 0 {
		semanticScheduled = true
	}
	schedulerStateApplied := false
	if state, ok := a.cachedSchedulerWorkStateForDisplay(semanticSingleWorkerSchedule(schedule), semanticScheduled); ok {
		applySchedulerWorkStateToDatasourceTasks(tasks, state)
		schedulerStateApplied = true
	}
	workers := a.effectiveHeavyTaskWorkers()
	capDatasourceTaskActiveWorkers(tasks, workers)
	// The cap removes stale persisted activity from older snapshots. Restore
	// process-local activity afterwards so assignments admitted before a worker
	// reduction remain visible until they actually finish.
	a.applyDatasourceTaskActiveSnapshot(tasks)
	applyWorkerWaitToDatasourceTasks(tasks, workers)
	if next := a.semanticIndexingNextRunAt(); semanticScheduled &&
		next != nil &&
		tasks[3].ActiveTasks == 0 &&
		tasks[3].QueuedTasks > 0 &&
		tasks[3].WaitingReason == "" &&
		workers > datasourceTaskActiveHeavyWorkers(tasks) &&
		next.After(time.Now().UTC()) {
		tasks[3].WaitingReason = datasourceTaskWaitingScheduled
		tasks[3].WaitingQueuedTarget = 0
		tasks[3].NextRunAt = next
	}
	if !schedulerStateApplied &&
		semanticTaskCounts.waitingQueuedTarget > 0 &&
		semanticScheduled &&
		tasks[4].ActiveTasks == 0 &&
		tasks[4].QueuedTasks > 0 &&
		semanticTaskCounts.failedIndexJobs == 0 {
		tasks[4].WaitingReason = datasourceTaskWaitingQueuedTarget
		tasks[4].WaitingQueuedTarget = semanticTaskCounts.waitingQueuedTarget
		tasks[4].NextRunAt = nil
	}
	if len(datasources) > 0 && a.installedSemanticEmbeddingCoverageProfile(ctx) == nil {
		markDatasourceTasksSearchModelRequired(tasks)
	}

	tasks = normalizeDatasourceTaskDependencies(tasks)
	return tasks
}

func markDatasourceTasksSearchModelRequired(tasks []DatasourceTaskStatus) {
	for index := range tasks {
		if tasks[index].Phase != "embeddings" && tasks[index].Phase != "search_index" {
			continue
		}
		tasks[index].SetupRequired = datasourceTaskSetupSearchModel
		tasks[index].Note = tasks[index].Note + " " + datasourceTaskNoteSearchModelRequired
	}
}

func applySchedulerWorkStateToDatasourceTasks(tasks []DatasourceTaskStatus, state schedulerWorkState) {
	byPhase := make(map[string]int, len(tasks))
	for index, task := range tasks {
		byPhase[task.Phase] = index
	}
	if index, ok := byPhase["metadata"]; ok {
		tasks[index].QueuedTasks = max(state.MetadataQueued, 0)
		tasks[index].SettlingTasks = max(state.MetadataSettling, 0)
		tasks[index].QueuedTasksUnknown = false
		if state.MetadataQueued == 0 && state.MetadataSettling > 0 && tasks[index].ActiveTasks == 0 {
			tasks[index].WaitingReason = datasourceTaskWaitingScheduled
			tasks[index].NextRunAt = state.MetadataNextEligibleAt
		}
	}
	if index, ok := byPhase["thumbnails"]; ok {
		tasks[index].QueuedTasks = max(state.ThumbnailQueued, 0)
		tasks[index].QueuedTasksUnknown = false
	}
	if index, ok := byPhase["embeddings"]; ok && state.SemanticScheduled {
		if state.SemanticEligibleVectors > 0 || state.SemanticCompletedVectors > 0 {
			tasks[index].FailedTasks = max(state.SemanticFailedVectors, 0)
			tasks[index].FailedTasksUnknown = false
			tasks[index].QueuedTasks = max(state.SemanticEligibleVectors-state.SemanticCompletedVectors-tasks[index].FailedTasks, 0)
			tasks[index].CompletedTasks = max(state.SemanticCompletedVectors, 0)
			tasks[index].TotalTasks = max(state.SemanticEligibleVectors, 0)
			tasks[index].QueuedTasksUnknown = false
		} else if state.SemanticMixedEmbeddingQueued > 0 {
			tasks[index].QueuedTasks = max(state.SemanticMixedEmbeddingQueued, 0)
			tasks[index].QueuedTasksUnknown = false
		}
	}
	if index, ok := byPhase["search_index"]; ok && state.SemanticScheduled {
		if state.SemanticCompletedVectors > 0 || state.SemanticIndexedVectors > 0 {
			tasks[index].QueuedTasks = max(state.SemanticCompletedVectors-state.SemanticIndexedVectors, 0)
			tasks[index].CompletedTasks = max(state.SemanticIndexedVectors, 0)
			tasks[index].TotalTasks = max(state.SemanticCompletedVectors, 0)
			tasks[index].FailedTasks = max(state.SemanticFailedIndexJobs, 0)
			tasks[index].QueuedTasksUnknown = false
			tasks[index].FailedTasksUnknown = false
		}
		if state.SemanticWaitingQueuedTarget > 0 &&
			tasks[index].ActiveTasks == 0 &&
			tasks[index].QueuedTasks > 0 &&
			state.SemanticFailedIndexJobs == 0 {
			tasks[index].WaitingReason = datasourceTaskWaitingQueuedTarget
			tasks[index].WaitingQueuedTarget = state.SemanticWaitingQueuedTarget
			tasks[index].NextRunAt = nil
		}
	}
}

func applyAssetProcessingStatsToDatasourceTasks(tasks []DatasourceTaskStatus, stats catalog.AssetProcessingStatsSnapshot) {
	if stats.Empty() {
		return
	}
	byPhase := make(map[string]int, len(tasks))
	for index, task := range tasks {
		byPhase[task.Phase] = index
	}
	applyStage := func(phase string, stage string) {
		index, ok := byPhase[phase]
		if !ok || !stats.HasStage(stage) {
			return
		}
		task := tasks[index]
		task.QueuedTasks = stats.Pending(stage)
		if phase == "metadata" {
			task.SettlingTasks = stats.Settling(stage)
		}
		task.QueuedTasksUnknown = false
		task.CompletedTasks = stats.Ready(stage)
		task.TotalTasks = stats.Total(stage)
		task.FailedTasks = stats.Failed(stage)
		task.FailedTasksUnknown = false
		if running := stats.Running(stage); running > task.ActiveTasks {
			task.ActiveTasks = running
		}
		task.ActiveTasksUnknown = false
		if task.ActiveTasks > 0 {
			task.WaitingReason = ""
			task.WaitingQueuedTarget = 0
			task.NextRunAt = nil
		}
		tasks[index] = task
	}
	applyStage("metadata", catalog.AssetProcessingStageMetadata)
	applyStage("thumbnails", catalog.AssetProcessingStageThumbnails)
}

func applyWorkerWaitToDatasourceTasks(tasks []DatasourceTaskStatus, workers int) {
	activeHeavyTasks := datasourceTaskActiveHeavyWorkers(tasks)
	workersPaused := workers <= 0
	workerUnavailable := workersPaused || activeHeavyTasks >= workers
	if !workerUnavailable {
		return
	}
	for index := range tasks {
		task := tasks[index]
		if !datasourceTaskUsesHeavyWorker(task.Phase) ||
			(task.QueuedTasks <= 0 && !task.QueuedTasksUnknown && task.ActiveTasks <= 0) {
			continue
		}
		if workersPaused && task.ActiveTasks > 0 {
			task.WaitingReason = ""
			task.WaitingQueuedTarget = 0
			task.NextRunAt = nil
			tasks[index] = task
			continue
		}
		if !workersPaused && (task.ActiveTasks > 0 || task.WaitingReason != "") {
			continue
		}
		task.ActiveTasks = 0
		task.ActiveTasksUnknown = false
		if workersPaused {
			task.WaitingReason = datasourceTaskWaitingPaused
			task.WaitingQueuedTarget = 0
		}
		tasks[index] = task
	}
}

func datasourceTaskActiveHeavyWorkers(tasks []DatasourceTaskStatus) int {
	activeHeavyTasks := 0
	for _, task := range tasks {
		if !datasourceTaskUsesHeavyWorker(task.Phase) {
			continue
		}
		activeHeavyTasks += max(task.ActiveTasks, 0)
	}
	return activeHeavyTasks
}

func normalizeDatasourceTaskWorkerState(tasks []DatasourceTaskStatus, workers int) []DatasourceTaskStatus {
	if len(tasks) == 0 {
		return tasks
	}
	normalized := make([]DatasourceTaskStatus, len(tasks))
	copy(normalized, tasks)
	for index := range normalized {
		switch normalized[index].WaitingReason {
		case datasourceTaskWaitingPaused, datasourceTaskWaitingWorker:
			normalized[index].WaitingReason = ""
			normalized[index].WaitingQueuedTarget = 0
		}
	}
	capDatasourceTaskActiveWorkers(normalized, workers)
	applyWorkerWaitToDatasourceTasks(normalized, workers)
	return normalized
}

func capDatasourceTaskActiveWorkers(tasks []DatasourceTaskStatus, workers int) {
	if workers <= 0 {
		return
	}
	total := datasourceTaskActiveHeavyWorkers(tasks)
	if total <= workers {
		return
	}
	remaining := workers
	phaseOrder := []string{"search_index", "embeddings", "metadata", "thumbnails", "content_verification"}
	keptPhases := map[string]bool{}
	for _, phase := range phaseOrder {
		for index := range tasks {
			if tasks[index].Phase != phase {
				continue
			}
			active := max(tasks[index].ActiveTasks, 0)
			if active == 0 {
				break
			}
			kept := min(active, remaining)
			tasks[index].ActiveTasks = kept
			tasks[index].ActiveTasksUnknown = false
			if kept == 0 {
				tasks[index].WaitingReason = ""
				tasks[index].WaitingQueuedTarget = 0
				tasks[index].NextRunAt = nil
			} else {
				keptPhases[phase] = true
			}
			remaining -= kept
			break
		}
		if remaining <= 0 {
			break
		}
	}
	if remaining > 0 {
		return
	}
	for index := range tasks {
		if !datasourceTaskUsesHeavyWorker(tasks[index].Phase) {
			continue
		}
		if keptPhases[tasks[index].Phase] {
			continue
		}
		tasks[index].ActiveTasks = 0
		tasks[index].ActiveTasksUnknown = false
		tasks[index].WaitingReason = ""
		tasks[index].WaitingQueuedTarget = 0
		tasks[index].NextRunAt = nil
	}
}

func datasourceTaskUsesHeavyWorker(phase string) bool {
	switch phase {
	case "metadata", "thumbnails", "embeddings", "search_index", "content_verification":
		return true
	default:
		return false
	}
}

func normalizeDatasourceTaskDependencies(tasks []DatasourceTaskStatus) []DatasourceTaskStatus {
	embeddingIndex := -1
	searchIndex := -1
	for index, task := range tasks {
		switch task.Phase {
		case "embeddings":
			embeddingIndex = index
		case "search_index":
			searchIndex = index
		}
	}
	if embeddingIndex >= 0 && searchIndex >= 0 {
		searchTask := tasks[searchIndex]
		searchActive := searchTask.ActiveTasks > 0 || searchTask.Status == "running"
		embeddingTask := tasks[embeddingIndex]
		embeddingHasWork := embeddingTask.QueuedTasks > 0 ||
			embeddingTask.QueuedTasksUnknown ||
			embeddingTask.ActiveTasks > 0 ||
			embeddingTask.Status == "running" ||
			embeddingTask.Status == "queued"
		if searchActive && embeddingHasWork {
			embeddingTask.ActiveTasks = 0
			embeddingTask.ActiveTasksUnknown = false
			embeddingTask.WaitingReason = datasourceTaskWaitingSearchIndex
			embeddingTask.WaitingQueuedTarget = 0
			tasks[embeddingIndex] = embeddingTask
		} else if !searchActive && embeddingTask.WaitingReason == datasourceTaskWaitingSearchIndex {
			embeddingTask.WaitingReason = ""
			embeddingTask.WaitingQueuedTarget = 0
			tasks[embeddingIndex] = embeddingTask
		}
	}
	for index := range tasks {
		tasks[index].FailureUnit = datasourceTaskFailureUnitForPhase(tasks[index].Phase)
		tasks[index].Status = datasourceTaskStatus(tasks[index])
	}
	return tasks
}

func datasourceTaskFailureUnitForPhase(phase string) string {
	switch phase {
	case "metadata", "thumbnails", "embeddings":
		return datasourceTaskFailureUnitItems
	case "search_index":
		return datasourceTaskFailureUnitPublishJobs
	default:
		return ""
	}
}

type datasourceSemanticTaskCountSummary struct {
	known               bool
	busy                bool
	eligible            int
	completed           int
	indexed             int
	unindexed           int
	embeddingQueued     int
	failedEmbeddingJobs int
	pendingIndexJobs    int
	failedIndexJobs     int
	waitingQueuedTarget int
}

func datasourceSemanticTaskCountsFromAssetStats(stats catalog.AssetProcessingStatsSnapshot, fallback datasourceSemanticTaskCountSummary) (datasourceSemanticTaskCountSummary, bool) {
	if stats.Empty() ||
		(!stats.HasStage(catalog.AssetProcessingStageEmbeddings) && !stats.HasStage(catalog.AssetProcessingStageSearchIndex)) {
		return fallback, false
	}
	summary := fallback
	summary.known = true
	summary.busy = false
	if stats.HasStage(catalog.AssetProcessingStageEmbeddings) {
		summary.eligible = stats.Total(catalog.AssetProcessingStageEmbeddings)
		summary.completed = stats.Ready(catalog.AssetProcessingStageEmbeddings)
		summary.embeddingQueued = stats.Pending(catalog.AssetProcessingStageEmbeddings)
		summary.failedEmbeddingJobs = stats.Failed(catalog.AssetProcessingStageEmbeddings)
		if summary.eligible < summary.completed+summary.embeddingQueued {
			summary.eligible = summary.completed + summary.embeddingQueued
		}
		if summary.eligible < summary.completed+summary.failedEmbeddingJobs {
			summary.eligible = summary.completed + summary.failedEmbeddingJobs
		}
	}
	if stats.HasStage(catalog.AssetProcessingStageSearchIndex) {
		summary.indexed = stats.Ready(catalog.AssetProcessingStageSearchIndex)
		summary.unindexed = stats.Pending(catalog.AssetProcessingStageSearchIndex)
		summary.failedIndexJobs = max(summary.failedIndexJobs, stats.Failed(catalog.AssetProcessingStageSearchIndex))
		searchTotal := stats.Total(catalog.AssetProcessingStageSearchIndex)
		if summary.completed < searchTotal {
			summary.completed = searchTotal
		}
		if summary.completed < summary.indexed+summary.unindexed {
			summary.completed = summary.indexed + summary.unindexed
		}
		if summary.eligible < summary.completed {
			summary.eligible = summary.completed
		}
	}
	return finalizeDatasourceSemanticTaskCounts(summary), true
}

func datasourceSemanticTaskCounts(datasources []DatasourceIndexingStatus) datasourceSemanticTaskCountSummary {
	var summary datasourceSemanticTaskCountSummary
	for _, datasource := range datasources {
		if datasource.EmbeddingModelID == "" && datasource.EmbeddingStatus == "" {
			continue
		}
		switch datasource.EmbeddingStatus {
		case "busy", datasourceEmbeddingUnavailable:
			summary.busy = true
			continue
		}
		if !datasourceHasSemanticCoverage(datasource) {
			continue
		}
		summary.known = true
		summary.eligible += datasource.EmbeddingEligible
		summary.completed += datasource.EmbeddingCompleted
		summary.indexed += datasource.EmbeddingIndexed
		failedEmbeddings := max(datasource.FailedEmbeddingJobs, 0)
		summary.failedEmbeddingJobs += failedEmbeddings
		embeddingQueued := max(datasource.EmbeddingRemaining-failedEmbeddings, datasource.EmbeddingEligible-datasource.EmbeddingCompleted-failedEmbeddings)
		if embeddingQueued > 0 {
			summary.embeddingQueued += embeddingQueued
		}
		if unindexed := datasource.EmbeddingCompleted - datasource.EmbeddingIndexed; unindexed > 0 {
			summary.unindexed += unindexed
		}
		summary.pendingIndexJobs += datasource.EmbeddingPendingIndexJobs
		summary.failedIndexJobs += datasource.EmbeddingFailedIndexJobs
	}
	return finalizeDatasourceSemanticTaskCounts(summary)
}

func finalizeDatasourceSemanticTaskCounts(summary datasourceSemanticTaskCountSummary) datasourceSemanticTaskCountSummary {
	summary.waitingQueuedTarget = 0
	if summary.known {
		if target, ok := semanticIndexPartialPublishWaitTarget(summary.indexed, summary.unindexed, summary.embeddingQueued, summary.failedIndexJobs); ok {
			summary.waitingQueuedTarget = target
		}
	}
	return summary
}

func datasourceHasSemanticCoverage(datasource DatasourceIndexingStatus) bool {
	if datasource.EmbeddingEligible > 0 ||
		datasource.EmbeddingCompleted > 0 ||
		datasource.EmbeddingIndexed > 0 ||
		datasource.EmbeddingRemaining > 0 ||
		datasource.FailedEmbeddingJobs > 0 {
		return true
	}
	switch datasource.EmbeddingStatus {
	case catalog.SemanticBackfillStatusPending,
		catalog.SemanticBackfillStatusBackfilling,
		catalog.SemanticBackfillStatusIndexing,
		catalog.SemanticBackfillStatusReady:
		return true
	default:
		return false
	}
}

func datasourceTaskStatus(task DatasourceTaskStatus) string {
	switch {
	case task.Status == "not_applicable" && task.ActiveTasks <= 0:
		return "not_applicable"
	case task.Status == "disabled" && task.ActiveTasks <= 0:
		return "disabled"
	case task.FailedTasks > 0:
		return "attention"
	case task.WaitingReason == datasourceTaskWaitingPaused:
		return "paused"
	case task.WaitingReason != "":
		return "waiting"
	case task.ActiveTasks > 0:
		return "running"
	case task.QueuedTasks > 0:
		return "queued"
	case task.ActiveTasksUnknown || task.QueuedTasksUnknown || task.FailedTasksUnknown:
		return "busy"
	default:
		return "idle"
	}
}

func latestTimePtr(current *time.Time, candidate *time.Time) *time.Time {
	if candidate == nil || candidate.IsZero() {
		return current
	}
	utc := candidate.UTC()
	if current == nil || utc.After(current.UTC()) {
		return &utc
	}
	return current
}

// RunDatasourceIndexing manually kicks media discovery for configured datasource adapters.
func (a *AgentRuntime) RunDatasourceIndexing(ctx context.Context, options DatasourceIndexingRunOptions) (DatasourceIndexingRunResponse, error) {
	catalogService := a.catalogService()
	if catalogService == nil {
		return DatasourceIndexingRunResponse{}, catalog.ErrNoDatasourceConfigured
	}
	clearActive, ok := a.trySetDatasourceDiscoveryActive(catalogService)
	if !ok {
		return DatasourceIndexingRunResponse{}, ErrDatasourceDiscoveryAlreadyRunning
	}
	var completedAt *time.Time
	defer func() {
		clearActive(completedAt)
	}()

	sourceKey := strings.TrimSpace(options.SourceKey)
	kind := strings.TrimSpace(options.Kind)
	mode := strings.TrimSpace(options.Mode)
	var response DatasourceIndexingRunResponse

	if kind == "" || normalizedDatasourceKind(kind) == config.DatasourceKindImmichIndexed {
		sourceKeys := catalogService.MirrorDatasourceSourceKeys()
		for _, key := range sourceKeys {
			if sourceKey != "" && sourceKey != key {
				continue
			}
			result, err := a.syncDatasourceIndexing(ctx, key, mode)
			if err != nil {
				return DatasourceIndexingRunResponse{}, err
			}
			response.Results = append(response.Results, datasourceIndexingRunResultFromRemote(key, result))
		}
	}

	if kind == "" || normalizedDatasourceKind(kind) == config.DatasourceKindLocalFiles {
		localKeys := catalogService.LocalDatasourceSourceKeys()
		if len(localKeys) > 0 {
			var localResult LocalDatasourcePhase0ScanResponse
			var err error
			switch {
			case sourceKey == "":
				localResult, err = a.RunLocalDatasourceDiscoveryScans(ctx)
			case containsString(localKeys, sourceKey):
				localResult, err = a.RunLocalDatasourceDiscoveryScan(ctx, sourceKey)
			default:
				err = nil
			}
			if err != nil {
				return DatasourceIndexingRunResponse{}, err
			}
			if len(localResult.Phase0) > 0 {
				for _, result := range localResult.Phase0 {
					response.Results = append(response.Results, datasourceIndexingRunResultFromLocal(result))
				}
			}
		}
	}

	if len(response.Results) == 0 {
		return DatasourceIndexingRunResponse{}, catalog.ErrNoDatasourceConfigured
	}
	completedAt = datasourceIndexingRunResponseCompletedAt(response)
	return response, nil
}

func (a *AgentRuntime) trySetDatasourceDiscoveryActive(catalogService *catalog.Service) (func(*time.Time), bool) {
	if a == nil {
		return func(*time.Time) {}, true
	}
	a.datasourceTaskMu.Lock()
	if a.datasourceDiscoveryActive > 0 {
		a.datasourceTaskMu.Unlock()
		return nil, false
	}
	a.datasourceDiscoveryActive = 1
	a.datasourceTaskMu.Unlock()
	a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, true, nil)
	return func(completedAt *time.Time) {
		a.datasourceTaskMu.Lock()
		a.datasourceDiscoveryActive = 0
		a.datasourceTaskMu.Unlock()
		a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, false, completedAt)
	}, true
}

func (a *AgentRuntime) setDatasourceDiscoveryActive(active int) func() {
	if a == nil || active <= 0 {
		return func() {}
	}
	a.datasourceTaskMu.Lock()
	alreadyActive := a.datasourceDiscoveryActive > 0
	if !alreadyActive {
		a.datasourceDiscoveryActive = 1
	}
	a.datasourceTaskMu.Unlock()
	if alreadyActive {
		return func() {}
	}
	return func() {
		a.datasourceTaskMu.Lock()
		a.datasourceDiscoveryActive = 0
		a.datasourceTaskMu.Unlock()
	}
}

func datasourceIndexingRunResponseCompletedAt(response DatasourceIndexingRunResponse) *time.Time {
	var completedAt *time.Time
	for _, result := range response.Results {
		if result.CompletedAt.IsZero() {
			continue
		}
		completedAt = latestTimePtr(completedAt, &result.CompletedAt)
	}
	return completedAt
}

func localPhase0ScanResultsCompletedAt(results []catalog.LocalPhase0ScanResult) *time.Time {
	var completedAt *time.Time
	for _, result := range results {
		if result.CompletedAt.IsZero() {
			continue
		}
		completedAt = latestTimePtr(completedAt, &result.CompletedAt)
	}
	return completedAt
}

func localPhase0ScanModeCompletedAt(results []catalog.LocalPhase0ScanResult, scanMode string) *time.Time {
	var completedAt *time.Time
	for _, result := range results {
		if result.ScanMode != scanMode || result.CompletedAt.IsZero() {
			continue
		}
		completedAt = latestTimePtr(completedAt, &result.CompletedAt)
	}
	return completedAt
}

func (a *AgentRuntime) setDatasourceTaskActive(phase string, active int) {
	if a == nil {
		return
	}
	phase = strings.TrimSpace(phase)
	if phase == "" {
		return
	}
	a.datasourceTaskMu.Lock()
	if active <= 0 {
		delete(a.datasourceTaskActive, phase)
		a.datasourceTaskMu.Unlock()
		return
	}
	if a.datasourceTaskActive == nil {
		a.datasourceTaskActive = make(map[string]int)
	}
	a.datasourceTaskActive[phase] = active
	a.datasourceTaskMu.Unlock()
}

func (a *AgentRuntime) datasourceTaskActiveSnapshot() map[string]int {
	if a == nil {
		return nil
	}
	a.datasourceTaskMu.Lock()
	defer a.datasourceTaskMu.Unlock()
	if len(a.datasourceTaskActive) == 0 {
		return nil
	}
	snapshot := make(map[string]int, len(a.datasourceTaskActive))
	for phase, active := range a.datasourceTaskActive {
		snapshot[phase] = active
	}
	return snapshot
}

func (a *AgentRuntime) applyDatasourceTaskActiveSnapshot(tasks []DatasourceTaskStatus) {
	activeByPhase := a.datasourceTaskActiveSnapshot()
	if len(activeByPhase) == 0 {
		return
	}
	for index := range tasks {
		active := activeByPhase[tasks[index].Phase]
		if active <= tasks[index].ActiveTasks {
			continue
		}
		tasks[index].ActiveTasks = active
		tasks[index].ActiveTasksUnknown = false
		tasks[index].WaitingReason = ""
		tasks[index].WaitingQueuedTarget = 0
		tasks[index].NextRunAt = nil
	}
}

func (a *AgentRuntime) datasourceDiscoveryActiveCount() int {
	if a == nil {
		return 0
	}
	a.datasourceTaskMu.Lock()
	defer a.datasourceTaskMu.Unlock()
	return a.datasourceDiscoveryActive
}

func (a *AgentRuntime) syncDatasourceIndexing(ctx context.Context, sourceKey string, mode string) (catalog.MirrorSyncResult, error) {
	a.mirrorSyncMu.Lock()
	defer a.mirrorSyncMu.Unlock()
	catalogService := a.catalogService()
	if catalogService == nil {
		return catalog.MirrorSyncResult{}, catalog.ErrCatalogNotConfigured
	}
	result, err := catalogService.SyncDatasourceMirror(ctx, sourceKey, mode)
	if err != nil {
		return catalog.MirrorSyncResult{}, err
	}
	a.notifyDatasourceMirrorSyncCompleted()
	return result, nil
}

func datasourceIndexingRunResultFromRemote(sourceKey string, result catalog.MirrorSyncResult) DatasourceIndexingRunResult {
	return DatasourceIndexingRunResult{
		SourceKey:        sourceKey,
		Kind:             config.DatasourceKindImmichIndexed,
		IngestionKind:    datasourceIngestionRemoteAPI,
		Mode:             result.Mode,
		Status:           result.Status,
		FetchedAssets:    result.FetchedCount,
		ActiveAssets:     result.ActiveCount,
		OutOfScopeAssets: result.OutOfScopeCount,
		MissingAssets:    result.MissingCount,
		StartedAt:        result.StartedAt,
		CompletedAt:      result.CompletedAt,
		LastError:        result.Error,
	}
}

func datasourceIndexingRunResultFromLocal(result catalog.LocalPhase0ScanResult) DatasourceIndexingRunResult {
	return DatasourceIndexingRunResult{
		SourceKey:       result.SourceKey,
		Kind:            config.DatasourceKindLocalFiles,
		IngestionKind:   datasourceIngestionFilesystem,
		Mode:            result.ScanMode,
		Status:          result.Status,
		DiscoveredPaths: result.DiscoveredPaths,
		ChangedPaths:    result.ChangedPaths,
		QueuedMetadata:  result.QueuedMetadata,
		MissingAssets:   result.MissingPaths,
		SkippedPaths:    result.SkippedPaths,
		StartedAt:       result.StartedAt,
		CompletedAt:     result.CompletedAt,
		LastError:       result.LastError,
	}
}

func normalizedDatasourceKind(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return config.DatasourceKindImmich
	}
	return kind
}

func datasourceStatusesArePassthroughOnly(datasources []DatasourceIndexingStatus) bool {
	return len(datasources) == 1 &&
		strings.TrimSpace(datasources[0].Kind) == config.DatasourceKindImmich &&
		!datasources[0].IndexingEnabled
}

func datasourceSummariesArePassthroughOnly(summaries []DatasourceSummary) bool {
	return len(summaries) == 1 && strings.TrimSpace(summaries[0].Kind) == config.DatasourceKindImmich
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
