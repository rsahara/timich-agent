package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

const (
	localPhase0StatusCompleted              = "completed"
	localPhase0StatusBlocked                = "blocked"
	localPhase0StatusFailed                 = "failed"
	localPhase0ScanModeReconciliation       = "reconciliation"
	localPhase0ScanModeQuick                = "quick"
	defaultLocalQuickScanInterval           = 5 * time.Minute
	defaultLocalReconciliationTime          = "04:00"
	defaultLocalContentVerificationTime     = "04:00"
	defaultLocalContentVerificationDuration = 30 * time.Minute
	defaultLocalSettlingDuration            = 2 * time.Minute

	localMetadataJobKind              = "metadata"
	localMetadataRepairPriority       = 0
	localMetadataBackgroundPriority   = 1
	localPhase0WriteBatchSize         = 256
	localPhase0MissingUpdateBatchSize = 500
	localScanRunRetentionPerMode      = 64
	localPhase0FinishTimeout          = 5 * time.Second
)

const LocalMediaRootStatusIdentityChanged = "identity_changed"

var localPhase0WalkDir = func(_ string, rootFS fs.FS, fn fs.WalkDirFunc) error {
	return fs.WalkDir(rootFS, ".", fn)
}

var errLocalPhase0UnsafeEmptyReconciliation = errors.New("local media root appears empty; refusing to mark every known location missing")

var ErrLocalMediaRootIdentityChanged = errors.New("local media root identity changed; explicit administrator acceptance is required")
var ErrLocalMediaRootAcceptanceStale = errors.New("local media root acceptance is stale")
var ErrLocalMediaRootAcceptanceNotRequired = errors.New("local media root acceptance is not required")

type LocalPhase0ScanResult struct {
	SourceKey       string         `json:"sourceKey"`
	RootKey         string         `json:"rootKey"`
	ScanMode        string         `json:"scanMode"`
	Status          string         `json:"status"`
	RootStatus      string         `json:"rootStatus"`
	DiscoveredPaths int            `json:"discoveredPaths"`
	ChangedPaths    int            `json:"changedPaths"`
	QueuedMetadata  int            `json:"queuedMetadata"`
	MissingPaths    int            `json:"missingPaths"`
	SkippedPaths    int            `json:"skippedPaths"`
	SkipCounts      map[string]int `json:"skipCounts,omitempty"`
	StartedAt       time.Time      `json:"startedAt"`
	CompletedAt     time.Time      `json:"completedAt"`
	LastError       string         `json:"lastError,omitempty"`
}

type LocalDatasourceScanStatus struct {
	SourceKey                         string               `json:"sourceKey"`
	Name                              string               `json:"name"`
	RootKey                           string               `json:"rootKey"`
	RootPath                          string               `json:"rootPath"`
	RootStatus                        string               `json:"rootStatus"`
	Phase0Status                      string               `json:"phase0Status"`
	NextPhase0At                      *time.Time           `json:"nextPhase0At,omitempty"`
	UpdatedAt                         *time.Time           `json:"updatedAt,omitempty"`
	LastQuickScanAt                   *time.Time           `json:"lastQuickScanAt,omitempty"`
	LastReconciliationAt              *time.Time           `json:"lastReconciliationAt,omitempty"`
	LastContentVerificationAt         *time.Time           `json:"lastContentVerificationAt,omitempty"`
	ContentVerificationStartedAt      *time.Time           `json:"contentVerificationStartedAt,omitempty"`
	ContentVerificationStatus         string               `json:"contentVerificationStatus,omitempty"`
	ContentVerificationSkipReason     string               `json:"contentVerificationSkipReason,omitempty"`
	ContentVerificationProcessedFiles int                  `json:"contentVerificationProcessedFiles,omitempty"`
	ContentVerificationVerifiedFiles  int                  `json:"contentVerificationVerifiedFiles,omitempty"`
	ContentVerificationChangedFiles   int                  `json:"contentVerificationChangedFiles,omitempty"`
	ContentVerificationRunFailures    int                  `json:"contentVerificationRunFailures,omitempty"`
	ContentVerificationReadBytes      int64                `json:"contentVerificationReadBytes,omitempty"`
	ContentVerificationFailures       int                  `json:"contentVerificationFailures,omitempty"`
	LastError                         string               `json:"lastError,omitempty"`
	ObservedRootIdentity              string               `json:"observedRootIdentity,omitempty"`
	RootAcceptanceRequired            bool                 `json:"rootAcceptanceRequired,omitempty"`
	RunningPhase0Scans                int                  `json:"runningPhase0Scans"`
	DiscoveredLocations               int                  `json:"discoveredLocations"`
	ActiveLocations                   int                  `json:"activeLocations"`
	MissingLocations                  int                  `json:"missingLocations"`
	BlockedLocations                  int                  `json:"blockedLocations"`
	ActiveAssets                      int                  `json:"activeAssets"`
	QueuedMetadataJobs                int                  `json:"queuedMetadataJobs"`
	SettlingMetadataJobs              int                  `json:"settlingMetadataJobs"`
	RunningMetadataJobs               int                  `json:"runningMetadataJobs"`
	FailedMetadataJobs                int                  `json:"failedMetadataJobs"`
	PendingThumbnailJobs              int                  `json:"pendingThumbnailJobs"`
	QueuedThumbnailJobs               int                  `json:"queuedThumbnailJobs"`
	RunningThumbnailJobs              int                  `json:"runningThumbnailJobs"`
	FailedThumbnailJobs               int                  `json:"failedThumbnailJobs"`
	EmbeddingModelID                  string               `json:"embeddingModelId,omitempty"`
	EmbeddingVectorSpace              string               `json:"embeddingVectorSpaceId,omitempty"`
	EmbeddingStatus                   string               `json:"embeddingStatus,omitempty"`
	EmbeddingMessageCode              string               `json:"embeddingMessageCode,omitempty"`
	EmbeddingLastError                string               `json:"embeddingLastError,omitempty"`
	EmbeddingEligible                 int                  `json:"embeddingEligibleAssets,omitempty"`
	EmbeddingCompleted                int                  `json:"embeddingCompletedVectors,omitempty"`
	EmbeddingIndexed                  int                  `json:"embeddingIndexedVectors,omitempty"`
	FailedEmbeddingJobs               int                  `json:"failedEmbeddingJobs,omitempty"`
	EmbeddingRemaining                int                  `json:"embeddingRemainingVectors,omitempty"`
	LastRun                           *LocalScanRunSummary `json:"lastRun,omitempty"`
}

type LocalMediaRootContinuityStatus struct {
	SourceKey            string `json:"sourceKey"`
	RootKey              string `json:"rootKey"`
	RootStatus           string `json:"rootStatus"`
	ObservedRootIdentity string `json:"observedRootIdentity,omitempty"`
	AcceptanceRequired   bool   `json:"acceptanceRequired,omitempty"`
	Message              string `json:"message,omitempty"`
}

type LocalMediaRootAcceptanceResult struct {
	SourceKey string `json:"sourceKey"`
	RootKey   string `json:"rootKey"`
	Accepted  bool   `json:"accepted"`
}

type LocalScanRunSummary struct {
	ID                int64      `json:"id"`
	ScanMode          string     `json:"scanMode"`
	Status            string     `json:"status"`
	StartedAt         *time.Time `json:"startedAt,omitempty"`
	CompletedAt       *time.Time `json:"completedAt,omitempty"`
	RootStatusAtStart string     `json:"rootStatusAtStart"`
	DiscoveredPaths   int        `json:"discoveredPaths"`
	ChangedPaths      int        `json:"changedPaths"`
	QueuedMetadata    int        `json:"queuedMetadata"`
	MissingPaths      int        `json:"missingPaths"`
	SkippedPaths      int        `json:"skippedPaths"`
	LastError         string     `json:"lastError,omitempty"`
}

type LocalMetadataQueueState struct {
	Queued         int
	Settling       int
	NextEligibleAt *time.Time
}

// QueueCommittedLocalUpload makes one verified, atomically committed Agent
// upload immediately eligible for metadata without scanning its directory. It
// reports whether a queued job was inserted or its eligibility was refreshed.
func (s *Service) QueueCommittedLocalUpload(ctx context.Context, sourceKey string, fullPath string) (bool, error) {
	trustedRoot, err := s.acquireTrustedLocalMediaRoot(ctx, sourceKey)
	if err != nil {
		return false, err
	}
	defer trustedRoot.Close()
	rootAbs, err := filepath.Abs(trustedRoot.root.Path)
	if err != nil {
		return false, fmt.Errorf("resolve local upload root: %w", err)
	}
	fullAbs, err := filepath.Abs(fullPath)
	if err != nil {
		return false, fmt.Errorf("resolve committed local upload: %w", err)
	}
	relativePath, err := filepath.Rel(rootAbs, fullAbs)
	if err != nil || relativePath == "." || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		return false, fmt.Errorf("committed local upload is outside datasource root")
	}
	relativePath = filepath.ToSlash(relativePath)
	if !localPhase0SupportedMedia(relativePath) {
		return false, nil
	}
	file, info, err := openLocalRootFileFromPinnedRoot(trustedRoot.handle, relativePath)
	if err != nil {
		return false, err
	}
	defer file.Close()
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("committed local upload is not a regular file")
	}
	now := time.Now().UTC()
	scanner := &localPhase0Scanner{
		service:               s,
		datasource:            trustedRoot.datasource,
		root:                  trustedRoot.root,
		startedAt:             now,
		nowText:               formatCatalogTime(now),
		settlingDuration:      0,
		rootGeneration:        trustedRoot.rootGeneration,
		reconciliationPending: trustedRoot.reconciliationPending,
	}
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin committed local upload queue: %w", err)
	}
	locationID, _, needsMetadata, visibilityDirty, err := scanner.upsertLocationInTx(ctx, tx, relativePath, info)
	if err != nil {
		_ = tx.Rollback()
		return false, err
	}
	queued := false
	if needsMetadata {
		if _, err := tx.ExecContext(ctx, `UPDATE local_asset_locations
			SET metadata_not_before = ?, updated_at = ?
			WHERE id = ?`, scanner.nowText, scanner.nowText, locationID); err != nil {
			_ = tx.Rollback()
			return false, fmt.Errorf("make committed local upload metadata eligible: %w", err)
		}
		queued, err = scanner.queueMetadataJobInTx(ctx, tx, locationID, formatCatalogTime(info.ModTime().UTC()))
		if err != nil {
			_ = tx.Rollback()
			return false, err
		}
	}
	canonicalChanged := false
	if visibilityDirty {
		changedCanonicalIDs, err := refreshLocalAssetVisibilityInTx(ctx, tx, trustedRoot.datasource.SourceKey, trustedRoot.root.Key, scanner.nowText)
		if err != nil {
			_ = tx.Rollback()
			return false, err
		}
		if err := s.catalog.rebuildCatalogCanonicalIDsInTx(ctx, tx, changedCanonicalIDs, scanner.nowText); err != nil {
			_ = tx.Rollback()
			return false, err
		}
		canonicalChanged = len(changedCanonicalIDs) > 0
	}
	if err := s.catalog.commitCatalogAssetChanges(ctx, tx, canonicalChanged); err != nil {
		return false, fmt.Errorf("commit local upload metadata queue: %w", err)
	}
	return queued, nil
}

type localPhase0Scanner struct {
	service          *Service
	datasource       config.DatasourceConfig
	root             config.LocalMediaRootConfig
	result           LocalPhase0ScanResult
	scanRunID        int64
	startedAt        time.Time
	seen             map[string]struct{}
	blocked          []string
	pending          []localPhase0PendingPath
	nowText          string
	visibilityDirty  bool
	scanMode         string
	directoryStates  map[string]string
	seenDirectories  map[string]string
	checkedDirs      map[string]struct{}
	retryDirs        map[string]struct{}
	removedDirs      []string
	settlingDuration time.Duration

	rootIdentity          string
	previousRootIdentity  string
	rootGeneration        int64
	reconciliationPending bool
}

type localPhase0PendingPath struct {
	relativePath string
	info         fs.FileInfo
}

func (s *Service) LocalDatasourceScanStatuses(ctx context.Context) ([]LocalDatasourceScanStatus, error) {
	if s == nil || s.catalog == nil {
		return nil, ErrNoDatasourceConfigured
	}
	sourceKeys := s.LocalDatasourceSourceKeys()
	if len(sourceKeys) == 0 {
		return nil, ErrNoDatasourceConfigured
	}
	statuses := make([]LocalDatasourceScanStatus, 0, len(sourceKeys))
	for _, sourceKey := range sourceKeys {
		datasource, root, err := s.localDatasourceAndRoot(sourceKey)
		if err != nil {
			return nil, err
		}
		status := LocalDatasourceScanStatus{
			SourceKey:    datasource.SourceKey,
			Name:         datasource.Name,
			RootKey:      root.Key,
			RootPath:     root.Path,
			RootStatus:   "not_scanned",
			Phase0Status: "idle",
		}
		if err := s.populateLocalDatasourceScanStatus(ctx, &status); err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// LocalMediaRootContinuityStatuses returns the lightweight live root identity
// state used by every administrator-facing readiness surface.
func (s *Service) LocalMediaRootContinuityStatuses(ctx context.Context) ([]LocalMediaRootContinuityStatus, error) {
	if s == nil || s.catalog == nil {
		return nil, ErrNoDatasourceConfigured
	}
	sourceKeys := s.LocalDatasourceSourceKeys()
	if len(sourceKeys) == 0 {
		return nil, ErrNoDatasourceConfigured
	}
	statuses := make([]LocalMediaRootContinuityStatus, 0, len(sourceKeys))
	for _, sourceKey := range sourceKeys {
		datasource, root, err := s.localDatasourceAndRoot(sourceKey)
		if err != nil {
			return nil, err
		}
		status, err := s.localMediaRootContinuityStatus(ctx, *datasource, *root)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func (s *Service) localMediaRootContinuityStatus(ctx context.Context, datasource config.DatasourceConfig, root config.LocalMediaRootConfig) (LocalMediaRootContinuityStatus, error) {
	status := LocalMediaRootContinuityStatus{
		SourceKey:  datasource.SourceKey,
		RootKey:    root.Key,
		RootStatus: LocalMediaRootStatusReady,
	}
	trustedIdentity, err := s.trustedLocalMediaRootIdentity(ctx, datasource.SourceKey, root.Key)
	if err != nil {
		return LocalMediaRootContinuityStatus{}, err
	}
	inspection, pinnedRoot, inspectErr := openLocalMediaRoot(root.Path)
	if pinnedRoot != nil {
		_ = pinnedRoot.Close()
	}
	if inspectErr != nil {
		status.RootStatus = inspection.status
		status.Message = "Local media root is not safely readable."
		return status, nil
	}
	if trustedIdentity != "" && inspection.identity != trustedIdentity {
		status.RootStatus = LocalMediaRootStatusIdentityChanged
		status.ObservedRootIdentity = inspection.identity
		status.AcceptanceRequired = true
		status.Message = "Local media root no longer matches the last successful scan. Explicit administrator acceptance is required."
	}
	return status, nil
}

func (s *Service) trustedLocalMediaRootIdentity(ctx context.Context, sourceKey string, rootKey string) (string, error) {
	state, err := s.localMediaRootWorkState(ctx, sourceKey, rootKey)
	return state.identity, err
}

func (s *Service) localMediaRootWorkState(ctx context.Context, sourceKey string, rootKey string) (localMediaRootWorkState, error) {
	state := localMediaRootWorkState{generation: initialLocalMediaRootGeneration}
	var reconciliationPending int
	err := s.catalog.queryDB().QueryRowContext(ctx, `SELECT root_identity, root_generation, reconciliation_pending
		FROM local_scan_root_state
		WHERE source_key = ? AND root_key = ?`, sourceKey, rootKey).
		Scan(&state.identity, &state.generation, &reconciliationPending)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return localMediaRootWorkState{}, fmt.Errorf("load trusted local media root work state: %w", err)
	}
	state.identity = strings.TrimSpace(state.identity)
	state.generation = normalizeLocalMediaRootGeneration(state.generation)
	state.reconciliationPending = reconciliationPending != 0
	return state, nil
}

// AcceptLocalMediaRootIdentity explicitly promotes the currently pinned root
// after verifying that the administrator approved the exact observed identity.
func (s *Service) AcceptLocalMediaRootIdentity(ctx context.Context, sourceKey string, rootKey string, observedIdentity string) (LocalMediaRootAcceptanceResult, error) {
	if err := s.ensureStateWritesAvailable(); err != nil {
		return LocalMediaRootAcceptanceResult{}, err
	}
	datasource, root, err := s.localDatasourceAndRoot(sourceKey)
	if err != nil {
		return LocalMediaRootAcceptanceResult{}, err
	}
	if strings.TrimSpace(rootKey) == "" || strings.TrimSpace(rootKey) != strings.TrimSpace(root.Key) {
		return LocalMediaRootAcceptanceResult{}, ErrNoDatasourceConfigured
	}
	transition, _, err := s.acquireLocalRootTransition(ctx, *datasource, *root, true, true)
	if err != nil {
		return LocalMediaRootAcceptanceResult{}, err
	}
	defer transition.release()
	inspection, pinnedRoot, err := openLocalMediaRoot(root.Path)
	if err != nil {
		return LocalMediaRootAcceptanceResult{}, err
	}
	defer pinnedRoot.Close()
	approvedIdentity := strings.TrimSpace(observedIdentity)
	if approvedIdentity == "" || approvedIdentity != inspection.identity {
		return LocalMediaRootAcceptanceResult{}, ErrLocalMediaRootAcceptanceStale
	}

	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return LocalMediaRootAcceptanceResult{}, fmt.Errorf("begin local media root acceptance: %w", err)
	}
	var trustedIdentity string
	err = tx.QueryRowContext(ctx, `SELECT root_identity
		FROM local_scan_root_state
		WHERE source_key = ? AND root_key = ?`, datasource.SourceKey, root.Key).Scan(&trustedIdentity)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return LocalMediaRootAcceptanceResult{}, ErrLocalMediaRootAcceptanceNotRequired
	}
	if err != nil {
		_ = tx.Rollback()
		return LocalMediaRootAcceptanceResult{}, fmt.Errorf("read trusted local media root identity for acceptance: %w", err)
	}
	if strings.TrimSpace(trustedIdentity) == "" || strings.TrimSpace(trustedIdentity) == inspection.identity {
		_ = tx.Rollback()
		return LocalMediaRootAcceptanceResult{}, ErrLocalMediaRootAcceptanceNotRequired
	}
	nowText := formatCatalogTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `UPDATE local_scan_root_state
		SET last_content_verification_at = ?,
			content_verification_window_started_at = NULL,
			content_verification_window_deadline_at = NULL,
			content_verification_status = ?,
			content_verification_skip_reason = ?,
			updated_at = ?
		WHERE source_key = ?
			AND root_key = ?
			AND content_verification_status = ?`,
		nowText,
		LocalContentVerificationStatusSkipped,
		LocalContentVerificationSkipRootChanged,
		nowText,
		datasource.SourceKey,
		root.Key,
		LocalContentVerificationStatusRunning,
	); err != nil {
		_ = tx.Rollback()
		return LocalMediaRootAcceptanceResult{}, fmt.Errorf("cancel local content verification after root acceptance: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_scan_root_state
		SET root_identity = ?,
			root_generation = root_generation + 1,
			reconciliation_pending = 1,
			root_status = ?,
			root_last_checked_at = ?,
			root_last_error = NULL,
			phase0_status = 'idle',
			next_phase0_at = ?,
			updated_at = ?
		WHERE source_key = ? AND root_key = ?`,
		inspection.identity,
		LocalMediaRootStatusReady,
		nowText,
		nowText,
		nowText,
		datasource.SourceKey,
		root.Key,
	); err != nil {
		_ = tx.Rollback()
		return LocalMediaRootAcceptanceResult{}, fmt.Errorf("accept local media root identity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_scan_directories
		WHERE source_key = ? AND root_key = ?`, datasource.SourceKey, root.Key); err != nil {
		_ = tx.Rollback()
		return LocalMediaRootAcceptanceResult{}, fmt.Errorf("clear local media root scan checkpoints: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return LocalMediaRootAcceptanceResult{}, fmt.Errorf("commit local media root acceptance: %w", err)
	}
	return LocalMediaRootAcceptanceResult{
		SourceKey: datasource.SourceKey,
		RootKey:   root.Key,
		Accepted:  true,
	}, nil
}

// QueuedLocalMetadataJobs returns the number of local metadata jobs waiting for
// background processing across all configured local datasources.
func (s *Service) QueuedLocalMetadataJobs(ctx context.Context) (int, error) {
	state, err := s.LocalMetadataQueueState(ctx)
	return state.Queued, err
}

func (s *Service) LocalMetadataQueueState(ctx context.Context) (LocalMetadataQueueState, error) {
	if s == nil || s.catalog == nil {
		return LocalMetadataQueueState{}, ErrNoDatasourceConfigured
	}
	if err := s.recoverRememberedLocalClaims(ctx); err != nil {
		return LocalMetadataQueueState{}, err
	}
	trustedRoots, err := s.trustedLocalMediaRootReferences(ctx, "")
	if err != nil {
		return LocalMetadataQueueState{}, err
	}
	if len(trustedRoots) == 0 {
		return LocalMetadataQueueState{}, nil
	}
	nowText := formatCatalogTime(time.Now().UTC())
	var state LocalMetadataQueueState
	var nextEligible sql.NullString
	for _, trustedRoot := range trustedRoots {
		var rootState LocalMetadataQueueState
		var rootNextEligible sql.NullString
		err = s.catalog.queryDB().QueryRowContext(ctx, `SELECT
				COALESCE(SUM(CASE WHEN MAX(j.scheduled_at, COALESCE(l.metadata_not_before, j.scheduled_at)) <= ? THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN MAX(j.scheduled_at, COALESCE(l.metadata_not_before, j.scheduled_at)) > ? THEN 1 ELSE 0 END), 0),
				MIN(CASE WHEN MAX(j.scheduled_at, COALESCE(l.metadata_not_before, j.scheduled_at)) > ?
					THEN MAX(j.scheduled_at, COALESCE(l.metadata_not_before, j.scheduled_at)) END)
			FROM local_scan_jobs AS j INDEXED BY idx_local_scan_jobs_source_metadata_ready
			JOIN local_asset_locations l
				ON l.id = j.location_id
				AND l.root_key = j.root_key
			WHERE j.source_key = ?
				AND j.root_key = ?
				AND j.root_generation = ?
				AND j.job_kind = ?
				AND j.status = 'queued'
				AND j.location_id IS NOT NULL`,
			nowText,
			nowText,
			nowText,
			trustedRoot.sourceKey,
			trustedRoot.rootKey,
			trustedRoot.rootGeneration,
			localMetadataJobKind,
		).Scan(&rootState.Queued, &rootState.Settling, &rootNextEligible)
		if err != nil {
			return LocalMetadataQueueState{}, fmt.Errorf("read local metadata queue state for %s: %w", trustedRoot.sourceKey, err)
		}
		state.Queued += rootState.Queued
		state.Settling += rootState.Settling
		if rootNextEligible.Valid && (!nextEligible.Valid || rootNextEligible.String < nextEligible.String) {
			nextEligible = rootNextEligible
		}
	}
	state.NextEligibleAt = parseLocalOptionalTime(nextEligible)
	return state, nil
}

// PendingLocalThumbnailJobs returns the number of local assets that still need
// a preview/detail preview or video poster generated.
func (s *Service) PendingLocalThumbnailJobs(ctx context.Context) (int, error) {
	if s == nil || strings.TrimSpace(s.mediaHelperPath) == "" {
		return 0, nil
	}
	if err := s.recoverRememberedLocalClaims(ctx); err != nil {
		return 0, err
	}
	trustedRoots, err := s.trustedLocalMediaRootReferences(ctx, "")
	if err != nil {
		return 0, err
	}
	if len(trustedRoots) == 0 {
		return 0, nil
	}
	pending := 0
	for _, trustedRoot := range trustedRoots {
		if trustedRoot.reconciliationPending {
			continue
		}
		count, err := s.countLocalRows(ctx, `SELECT COUNT(*)
			FROM local_assets a
			WHERE (a.media_type = 'image' OR (? = 1 AND a.media_type = 'video'))
				AND a.visibility_status = 'active'
				AND a.thumbnail_status = 'pending'
				AND a.source_key = ?`,
			boolInt(s.localVideoPosterEnabled()),
			trustedRoot.sourceKey,
		)
		if err != nil {
			return 0, err
		}
		pending += count
	}
	return pending, nil
}

func (s *Service) countLocalRows(ctx context.Context, query string, args ...any) (int, error) {
	if s == nil || s.catalog == nil {
		return 0, ErrNoDatasourceConfigured
	}
	if len(s.LocalDatasourceSourceKeys()) == 0 {
		return 0, nil
	}
	var count int
	if err := s.catalog.queryDB().QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Service) populateLocalDatasourceScanStatus(ctx context.Context, status *LocalDatasourceScanStatus) error {
	var nextPhase0 sql.NullString
	var updatedAt sql.NullString
	var lastError sql.NullString
	var lastRunID sql.NullInt64
	var lastQuickScanAt sql.NullString
	var lastReconciliationAt sql.NullString
	var lastContentVerificationAt sql.NullString
	var contentVerificationStartedAt sql.NullString
	var contentVerificationSkipReason sql.NullString
	rootGeneration := int64(initialLocalMediaRootGeneration)
	err := s.catalog.db.QueryRowContext(ctx, `SELECT root_status, phase0_status, root_last_error, next_phase0_at,
				last_phase0_run_id, last_quick_scan_at, last_reconciliation_at,
				last_content_verification_at, content_verification_window_started_at,
				content_verification_status, content_verification_skip_reason,
				content_verification_processed_files, content_verification_verified_files,
				content_verification_changed_files, content_verification_failed_files,
				content_verification_read_bytes, root_generation, updated_at
			FROM local_scan_root_state
			WHERE source_key = ? AND root_key = ?`, status.SourceKey, status.RootKey).
		Scan(
			&status.RootStatus,
			&status.Phase0Status,
			&lastError,
			&nextPhase0,
			&lastRunID,
			&lastQuickScanAt,
			&lastReconciliationAt,
			&lastContentVerificationAt,
			&contentVerificationStartedAt,
			&status.ContentVerificationStatus,
			&contentVerificationSkipReason,
			&status.ContentVerificationProcessedFiles,
			&status.ContentVerificationVerifiedFiles,
			&status.ContentVerificationChangedFiles,
			&status.ContentVerificationRunFailures,
			&status.ContentVerificationReadBytes,
			&rootGeneration,
			&updatedAt,
		)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read local scan root state: %w", err)
	}
	rootGeneration = normalizeLocalMediaRootGeneration(rootGeneration)
	if lastError.Valid {
		status.LastError = lastError.String
	}
	status.NextPhase0At = parseLocalOptionalTime(nextPhase0)
	status.UpdatedAt = parseLocalOptionalTime(updatedAt)
	status.LastQuickScanAt = parseLocalOptionalTime(lastQuickScanAt)
	status.LastReconciliationAt = parseLocalOptionalTime(lastReconciliationAt)
	status.LastContentVerificationAt = parseLocalOptionalTime(lastContentVerificationAt)
	status.ContentVerificationStartedAt = parseLocalOptionalTime(contentVerificationStartedAt)
	if contentVerificationSkipReason.Valid {
		status.ContentVerificationSkipReason = contentVerificationSkipReason.String
	}
	datasource, root, continuityErr := s.localDatasourceAndRoot(status.SourceKey)
	if continuityErr != nil {
		return continuityErr
	}
	continuity, continuityErr := s.localMediaRootContinuityStatus(ctx, *datasource, *root)
	if continuityErr != nil {
		return continuityErr
	}
	if continuity.AcceptanceRequired {
		status.RootStatus = LocalMediaRootStatusIdentityChanged
		status.Phase0Status = localPhase0StatusBlocked
		status.ObservedRootIdentity = continuity.ObservedRootIdentity
		status.RootAcceptanceRequired = true
		status.LastError = continuity.Message
	} else if status.RootStatus == LocalMediaRootStatusIdentityChanged {
		status.RootStatus = continuity.RootStatus
	}
	nowText := formatCatalogTime(time.Now().UTC())
	counts := []struct {
		target *int
		query  string
		args   []any
	}{
		{&status.DiscoveredLocations, `SELECT COUNT(*) FROM local_asset_locations WHERE source_key = ? AND root_key = ? AND status = 'discovered'`, []any{status.SourceKey, status.RootKey}},
		{&status.ActiveLocations, `SELECT COUNT(*) FROM local_asset_locations WHERE source_key = ? AND root_key = ? AND status = 'active'`, []any{status.SourceKey, status.RootKey}},
		{&status.MissingLocations, `SELECT COUNT(*) FROM local_asset_locations WHERE source_key = ? AND root_key = ? AND status = 'missing'`, []any{status.SourceKey, status.RootKey}},
		{&status.BlockedLocations, `SELECT COUNT(*) FROM local_asset_locations WHERE source_key = ? AND root_key = ? AND status = 'permission_blocked'`, []any{status.SourceKey, status.RootKey}},
		{&status.ContentVerificationFailures, `SELECT COUNT(*) FROM local_asset_locations WHERE source_key = ? AND root_key = ? AND status = 'active' AND content_verification_error IS NOT NULL`, []any{status.SourceKey, status.RootKey}},
		{&status.ActiveAssets, `SELECT COUNT(*) FROM local_assets WHERE source_key = ? AND visibility_status = 'active'`, []any{status.SourceKey}},
		{&status.RunningPhase0Scans, `SELECT COUNT(*) FROM local_scan_runs WHERE source_key = ? AND root_key = ? AND status = 'running'`, []any{status.SourceKey, status.RootKey}},
		{&status.QueuedMetadataJobs, `SELECT COUNT(*) FROM local_scan_jobs WHERE source_key = ? AND root_key = ? AND root_generation = ? AND job_kind = ? AND status = 'queued' AND scheduled_at <= ?`, []any{status.SourceKey, status.RootKey, rootGeneration, localMetadataJobKind, nowText}},
		{&status.SettlingMetadataJobs, `SELECT COUNT(*) FROM local_scan_jobs WHERE source_key = ? AND root_key = ? AND root_generation = ? AND job_kind = ? AND status = 'queued' AND scheduled_at > ?`, []any{status.SourceKey, status.RootKey, rootGeneration, localMetadataJobKind, nowText}},
		{&status.RunningMetadataJobs, `SELECT COUNT(*) FROM local_scan_jobs WHERE source_key = ? AND root_key = ? AND root_generation = ? AND job_kind = ? AND status = 'running'`, []any{status.SourceKey, status.RootKey, rootGeneration, localMetadataJobKind}},
		{&status.FailedMetadataJobs, `SELECT COUNT(*) FROM local_scan_jobs WHERE source_key = ? AND root_key = ? AND root_generation = ? AND job_kind = ? AND status = 'failed'`, []any{status.SourceKey, status.RootKey, rootGeneration, localMetadataJobKind}},
		{&status.PendingThumbnailJobs, `SELECT COUNT(*) FROM local_assets WHERE source_key = ? AND media_type IN ('image', 'video') AND visibility_status = 'active' AND thumbnail_status = 'pending'`, []any{status.SourceKey}},
		{&status.QueuedThumbnailJobs, `SELECT COUNT(*) FROM local_scan_jobs WHERE source_key = ? AND root_key = ? AND root_generation = ? AND job_kind = ? AND status = 'queued'`, []any{status.SourceKey, status.RootKey, rootGeneration, localThumbnailJobKind}},
		{&status.RunningThumbnailJobs, `SELECT COUNT(*) FROM local_scan_jobs WHERE source_key = ? AND root_key = ? AND root_generation = ? AND job_kind = ? AND status = 'running'`, []any{status.SourceKey, status.RootKey, rootGeneration, localThumbnailJobKind}},
		{&status.FailedThumbnailJobs, `SELECT COUNT(*) FROM local_assets WHERE source_key = ? AND media_type IN ('image', 'video') AND visibility_status = 'active' AND thumbnail_status = 'failed'`, []any{status.SourceKey}},
		{&status.FailedEmbeddingJobs, `SELECT COUNT(*)
			FROM semantic_vectors v
			JOIN local_assets a
				ON a.source_key = v.source_key
				AND a.asset_id = v.upstream_asset_id
			WHERE v.source_key = ? AND v.status = 'failed'`, []any{status.SourceKey}},
	}
	for _, count := range counts {
		if err := s.catalog.db.QueryRowContext(ctx, count.query, count.args...).Scan(count.target); err != nil {
			return fmt.Errorf("count local scan status: %w", err)
		}
	}
	if lastRunID.Valid {
		run, err := s.localScanRunSummary(ctx, lastRunID.Int64)
		if err != nil {
			return err
		}
		status.LastRun = run
	}
	return nil
}

func (s *Service) localScanRunSummary(ctx context.Context, runID int64) (*LocalScanRunSummary, error) {
	var run LocalScanRunSummary
	var startedAt sql.NullString
	var completedAt sql.NullString
	var lastError sql.NullString
	err := s.catalog.db.QueryRowContext(ctx, `SELECT id, scan_mode, status, started_at, completed_at, root_status_at_start,
			discovered_paths, changed_paths, queued_metadata, missing_paths, skipped_paths, root_failure_reason
		FROM local_scan_runs
		WHERE id = ?`, runID).
		Scan(
			&run.ID,
			&run.ScanMode,
			&run.Status,
			&startedAt,
			&completedAt,
			&run.RootStatusAtStart,
			&run.DiscoveredPaths,
			&run.ChangedPaths,
			&run.QueuedMetadata,
			&run.MissingPaths,
			&run.SkippedPaths,
			&lastError,
		)
	if err != nil {
		return nil, fmt.Errorf("read local scan run summary: %w", err)
	}
	run.StartedAt = parseLocalOptionalTime(startedAt)
	run.CompletedAt = parseLocalOptionalTime(completedAt)
	if lastError.Valid {
		run.LastError = lastError.String
	}
	return &run, nil
}

func parseLocalOptionalTime(value sql.NullString) *time.Time {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func (s *Service) RunLocalReconciliationScans(ctx context.Context) ([]LocalPhase0ScanResult, error) {
	return s.runLocalPhase0Scans(ctx, localPhase0ScanModeReconciliation)
}

// RunLocalQuickDiscoveryScans traverses every eligible directory but reconciles
// files only where the immediate directory mtime changed.
func (s *Service) RunLocalQuickDiscoveryScans(ctx context.Context) ([]LocalPhase0ScanResult, error) {
	return s.runLocalPhase0Scans(ctx, localPhase0ScanModeQuick)
}

func (s *Service) runLocalPhase0Scans(ctx context.Context, scanMode string) ([]LocalPhase0ScanResult, error) {
	if s == nil || s.catalog == nil {
		return nil, ErrNoDatasourceConfigured
	}
	sourceKeys := s.LocalDatasourceSourceKeys()
	if len(sourceKeys) == 0 {
		return nil, ErrNoDatasourceConfigured
	}
	results := make([]LocalPhase0ScanResult, 0, len(sourceKeys))
	for _, sourceKey := range sourceKeys {
		result, err := s.runLocalPhase0Scan(ctx, sourceKey, scanMode)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (s *Service) RunLocalReconciliationScan(ctx context.Context, sourceKey string) (LocalPhase0ScanResult, error) {
	return s.runLocalPhase0Scan(ctx, sourceKey, localPhase0ScanModeReconciliation)
}

// RunLocalQuickDiscoveryScan runs quick discovery for one local datasource.
func (s *Service) RunLocalQuickDiscoveryScan(ctx context.Context, sourceKey string) (LocalPhase0ScanResult, error) {
	return s.runLocalPhase0Scan(ctx, sourceKey, localPhase0ScanModeQuick)
}

// LocalReconciliationDue reports whether automatic discovery must use
// reconciliation mode for the latest scheduled daily occurrence.
func (s *Service) LocalReconciliationDue(ctx context.Context, sourceKey string, scheduledAt time.Time) (bool, error) {
	datasource, root, err := s.localDatasourceAndRoot(sourceKey)
	if err != nil {
		return false, err
	}
	workState, err := s.localMediaRootWorkState(ctx, datasource.SourceKey, root.Key)
	if err != nil {
		return false, err
	}
	if workState.reconciliationPending {
		return true, nil
	}
	var completedAtText string
	var skipCountsJSON string
	err = s.catalog.queryDB().QueryRowContext(ctx, `SELECT completed_at, skip_counts_json
		FROM local_scan_runs
		WHERE source_key = ? AND root_key = ? AND scan_mode = ? AND status = ? AND completed_at IS NOT NULL
		ORDER BY completed_at DESC
		LIMIT 1`, datasource.SourceKey, root.Key, localPhase0ScanModeReconciliation, localPhase0StatusCompleted).Scan(&completedAtText, &skipCountsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read last completed local reconciliation scan: %w", err)
	}
	var skipCounts map[string]int
	if err := json.Unmarshal([]byte(skipCountsJSON), &skipCounts); err != nil {
		return true, nil
	}
	if skipCounts["read_error"] > 0 || skipCounts["directory_stat_error"] > 0 || skipCounts["stat_error"] > 0 {
		return true, nil
	}
	completedAt, err := time.Parse(time.RFC3339Nano, completedAtText)
	if err != nil {
		return true, nil
	}
	return completedAt.Before(scheduledAt.UTC()), nil
}

// LocalQuickDiscoveryDue reports whether discovery is due based on the most
// recent completed quick or reconciliation scan for this datasource.
func (s *Service) LocalQuickDiscoveryDue(ctx context.Context, sourceKey string, now time.Time, interval time.Duration) (bool, error) {
	datasource, root, err := s.localDatasourceAndRoot(sourceKey)
	if err != nil {
		return false, err
	}
	if interval <= 0 {
		return true, nil
	}
	var lastQuick sql.NullString
	var lastReconciliation sql.NullString
	err = s.catalog.queryDB().QueryRowContext(ctx, `SELECT last_quick_scan_at, last_reconciliation_at
		FROM local_scan_root_state
		WHERE source_key = ? AND root_key = ?`, datasource.SourceKey, root.Key).Scan(&lastQuick, &lastReconciliation)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read last completed local scan: %w", err)
	}
	completedAt := time.Time{}
	for _, value := range []sql.NullString{lastQuick, lastReconciliation} {
		if !value.Valid {
			continue
		}
		parsed, parseErr := time.Parse(time.RFC3339Nano, value.String)
		if parseErr == nil && parsed.After(completedAt) {
			completedAt = parsed
		}
	}
	if completedAt.IsZero() {
		return true, nil
	}
	return !completedAt.Add(interval).After(now.UTC()), nil
}

func (s *Service) runLocalPhase0Scan(ctx context.Context, sourceKey string, scanMode string) (LocalPhase0ScanResult, error) {
	if err := s.ensureStateWritesAvailable(); err != nil {
		return LocalPhase0ScanResult{}, err
	}
	datasource, root, err := s.localDatasourceAndRoot(sourceKey)
	if err != nil {
		return LocalPhase0ScanResult{}, err
	}
	startedAt := time.Now().UTC()
	if scanMode != localPhase0ScanModeQuick {
		scanMode = localPhase0ScanModeReconciliation
	}
	scanner := &localPhase0Scanner{
		service:          s,
		datasource:       *datasource,
		root:             *root,
		startedAt:        startedAt,
		nowText:          formatCatalogTime(startedAt),
		seen:             map[string]struct{}{},
		scanMode:         scanMode,
		directoryStates:  map[string]string{},
		seenDirectories:  map[string]string{},
		checkedDirs:      map[string]struct{}{},
		retryDirs:        map[string]struct{}{},
		settlingDuration: localDatasourceSettlingDuration(*datasource),
		result: LocalPhase0ScanResult{
			SourceKey:  datasource.SourceKey,
			RootKey:    root.Key,
			ScanMode:   scanMode,
			Status:     localPhase0StatusCompleted,
			RootStatus: LocalMediaRootStatusReady,
			SkipCounts: map[string]int{},
			StartedAt:  startedAt,
		},
	}
	return scanner.run(ctx)
}

func (s *Service) localDatasourceAndRoot(sourceKey string) (*config.DatasourceConfig, *config.LocalMediaRootConfig, error) {
	if s == nil || s.catalog == nil {
		return nil, nil, ErrNoDatasourceConfigured
	}
	state := s.datasourceStateSnapshot()
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" {
		if state == nil || state.primary == nil || state.primary.Kind != config.DatasourceKindLocalFiles {
			return nil, nil, ErrNoDatasourceConfigured
		}
		sourceKey = state.primary.SourceKey
	}
	datasource, ok := state.datasources[sourceKey]
	if !ok || datasource.Kind != config.DatasourceKindLocalFiles {
		return nil, nil, ErrNoDatasourceConfigured
	}
	rootKey := strings.TrimSpace(datasource.RootKey)
	root, ok := state.localRoots[rootKey]
	if !ok {
		return nil, nil, ErrNoDatasourceConfigured
	}
	return &datasource, &root, nil
}

func (s *Service) LocalDatasourceScanInterval() (time.Duration, bool) {
	state := s.datasourceStateSnapshot()
	if state == nil || len(state.datasources) == 0 {
		return 0, false
	}
	interval := time.Duration(0)
	for _, sourceKey := range s.LocalDatasourceSourceKeys() {
		datasource := state.datasources[sourceKey]
		datasourceInterval := localDatasourceQuickScanInterval(datasource)
		if interval == 0 || datasourceInterval < interval {
			interval = datasourceInterval
		}
	}
	return interval, interval > 0
}

func (s *Service) LocalDatasourceReconciliationTime(sourceKey string) (string, error) {
	datasource, _, err := s.localDatasourceAndRoot(sourceKey)
	if err != nil {
		return "", err
	}
	return localDatasourceReconciliationTime(*datasource), nil
}

func (s *Service) LocalDatasourceContentVerificationSchedule(sourceKey string) (string, time.Duration, error) {
	datasource, _, err := s.localDatasourceAndRoot(sourceKey)
	if err != nil {
		return "", 0, err
	}
	return localDatasourceContentVerificationTime(*datasource), localDatasourceContentVerificationDuration(*datasource), nil
}

func (s *Service) LocalDatasourceQuickScanInterval(sourceKey string) (time.Duration, error) {
	datasource, _, err := s.localDatasourceAndRoot(sourceKey)
	if err != nil {
		return 0, err
	}
	return localDatasourceQuickScanInterval(*datasource), nil
}

func localDatasourceQuickScanInterval(datasource config.DatasourceConfig) time.Duration {
	if datasource.Scan != nil {
		raw := strings.TrimSpace(datasource.Scan.QuickScanInterval)
		if raw != "" {
			if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
				return parsed
			}
		}
	}
	return defaultLocalQuickScanInterval
}

func localDatasourceReconciliationTime(datasource config.DatasourceConfig) string {
	if datasource.Scan != nil {
		if raw := strings.TrimSpace(datasource.Scan.ReconciliationTime); raw != "" {
			if _, err := time.Parse("15:04", raw); err == nil {
				return raw
			}
		}
	}
	return defaultLocalReconciliationTime
}

func localDatasourceContentVerificationTime(datasource config.DatasourceConfig) string {
	if datasource.Scan != nil {
		if raw := strings.TrimSpace(datasource.Scan.ContentVerificationTime); raw != "" {
			if _, err := time.Parse("15:04", raw); err == nil {
				return raw
			}
		}
	}
	return defaultLocalContentVerificationTime
}

func localDatasourceContentVerificationDuration(datasource config.DatasourceConfig) time.Duration {
	if datasource.Scan != nil {
		if raw := strings.TrimSpace(datasource.Scan.ContentVerificationDuration); raw != "" {
			if parsed, err := time.ParseDuration(raw); err == nil && parsed >= 0 {
				return parsed
			}
		}
	}
	return defaultLocalContentVerificationDuration
}

func localDatasourceSettlingDuration(datasource config.DatasourceConfig) time.Duration {
	if datasource.Scan != nil {
		if raw := strings.TrimSpace(datasource.Scan.SettlingDuration); raw != "" {
			if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
				return parsed
			}
		}
	}
	return defaultLocalSettlingDuration
}

func (s *localPhase0Scanner) run(ctx context.Context) (returned LocalPhase0ScanResult, returnedErr error) {
	transition, _, err := s.service.acquireLocalRootTransition(ctx, s.datasource, s.root, false, true)
	if err != nil {
		return LocalPhase0ScanResult{}, err
	}
	defer transition.release()
	workState, err := s.service.localMediaRootWorkState(ctx, s.datasource.SourceKey, s.root.Key)
	if err != nil {
		return LocalPhase0ScanResult{}, err
	}
	s.previousRootIdentity = workState.identity
	s.rootGeneration = normalizeLocalMediaRootGeneration(workState.generation)
	s.reconciliationPending = workState.reconciliationPending
	rootInspection, pinnedRoot, rootErr := openLocalMediaRoot(s.root.Path)
	if pinnedRoot != nil {
		defer pinnedRoot.Close()
	}
	rootStatus := rootInspection.status
	s.result.RootStatus = rootStatus
	if rootErr != nil {
		s.result.LastError = rootErr.Error()
	}
	runID, err := s.insertRun(ctx, rootStatus, rootErr)
	if err != nil {
		return LocalPhase0ScanResult{}, err
	}
	s.scanRunID = runID
	finished := false
	terminalErr := error(nil)
	rootWalkFailed := false
	defer func() {
		if finished {
			return
		}
		if s.result.CompletedAt.IsZero() {
			s.result.CompletedAt = time.Now().UTC()
		}
		if terminalErr == nil {
			terminalErr = returnedErr
		}
		if terminalErr == nil {
			terminalErr = ctx.Err()
		}
		if terminalErr == nil {
			terminalErr = errors.New("local phase0 scan did not reach a terminal transition")
		}
		if strings.TrimSpace(s.result.LastError) == "" {
			s.result.LastError = terminalErr.Error()
		}
		s.result.Status = localPhase0StatusFailed
		cleanupCtx, cancel := context.WithTimeout(context.Background(), localPhase0FinishTimeout)
		finishErr := s.finishFailedRun(cleanupCtx, terminalErr, rootWalkFailed)
		cancel()
		if finishErr != nil {
			returnedErr = errors.Join(returnedErr, finishErr)
		}
		returned = s.result
	}()
	finishWalkFailure := func(walkErr error) (LocalPhase0ScanResult, error) {
		if errors.Is(walkErr, ErrLocalMediaRootIdentityChanged) {
			s.result.RootStatus = LocalMediaRootStatusIdentityChanged
		} else if rootWalkFailed {
			s.result.RootStatus = LocalMediaRootStatusUnreadable
		}
		s.result.Status = localPhase0StatusFailed
		s.result.LastError = walkErr.Error()
		s.result.CompletedAt = time.Now().UTC()
		terminalErr = walkErr
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			return s.result, walkErr
		}
		return s.result, nil
	}
	if rootStatus != LocalMediaRootStatusReady {
		s.result.Status = localPhase0StatusBlocked
		s.result.CompletedAt = time.Now().UTC()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), localPhase0FinishTimeout)
		err := s.finishBlockedRun(cleanupCtx)
		cancel()
		if err != nil {
			return LocalPhase0ScanResult{}, err
		}
		finished = true
		return s.result, nil
	}
	s.rootIdentity = rootInspection.identity
	if s.previousRootIdentity != "" && s.previousRootIdentity != s.rootIdentity {
		rootWalkFailed = true
		return finishWalkFailure(ErrLocalMediaRootIdentityChanged)
	}
	if err := s.loadDirectoryStates(ctx); err != nil {
		return LocalPhase0ScanResult{}, err
	}
	if s.scanMode == localPhase0ScanModeQuick {
		if err := s.loadQuickRetryDirectories(ctx); err != nil {
			return LocalPhase0ScanResult{}, err
		}
	}

	rootFS := pinnedRoot.FS()
	walkErr := localPhase0WalkDir(s.root.Path, rootFS, func(walkPath string, entry fs.DirEntry, walkErr error) error {
		path := localPhase0LogicalWalkPath(s.root.Path, walkPath)
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if path == s.root.Path {
				rootWalkFailed = true
				return walkErr
			}
			s.recordBlockedPath(path)
			s.recordSkip("read_error")
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == s.root.Path {
			if entry == nil {
				rootWalkFailed = true
				return errors.New("local media root walk returned no root entry")
			}
			info, err := entry.Info()
			if err != nil {
				rootWalkFailed = true
				return fmt.Errorf("inspect local media root walk entry: %w", err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				rootWalkFailed = true
				return ErrLocalMediaRootSymlink
			}
			if !info.IsDir() || rootInspection.info == nil || !os.SameFile(rootInspection.info, info) {
				rootWalkFailed = true
				return errLocalMediaRootChanged
			}
		}
		return s.handlePath(ctx, rootFS, path, entry)
	})
	if walkErr != nil {
		return finishWalkFailure(walkErr)
	}
	if err := s.flushPendingPaths(ctx); err != nil {
		return LocalPhase0ScanResult{}, err
	}
	s.collectRemovedDirectories()
	if err := validateLocalMediaRootIdentity(s.root.Path, rootInspection.info); err != nil {
		rootWalkFailed = true
		return finishWalkFailure(err)
	}
	if err := s.markMissing(ctx); err != nil {
		if errors.Is(err, errLocalPhase0UnsafeEmptyReconciliation) {
			rootWalkFailed = true
			return finishWalkFailure(err)
		}
		return LocalPhase0ScanResult{}, err
	}
	if err := s.persistDirectoryStates(ctx); err != nil {
		return LocalPhase0ScanResult{}, err
	}
	tx, err := s.service.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return LocalPhase0ScanResult{}, fmt.Errorf("begin local phase0 scan finish: %w", err)
	}
	// Reconciliation is also the correctness boundary for a configured root-key
	// change. Former-root locations can remain as history, but they must not keep
	// assets visible or retain the primary pointer for the current root.
	canonicalChanged := false
	if s.visibilityDirty || s.scanMode == localPhase0ScanModeReconciliation {
		changedCanonicalIDs, err := refreshLocalAssetVisibilityInTx(ctx, tx, s.datasource.SourceKey, s.root.Key, s.nowText)
		if err != nil {
			_ = tx.Rollback()
			return LocalPhase0ScanResult{}, err
		}
		if err := s.service.catalog.rebuildCatalogCanonicalIDsInTx(ctx, tx, changedCanonicalIDs, s.nowText); err != nil {
			_ = tx.Rollback()
			return LocalPhase0ScanResult{}, err
		}
		canonicalChanged = len(changedCanonicalIDs) > 0
	}
	if s.scanMode == localPhase0ScanModeReconciliation && s.reconciliationPending {
		if err := s.deleteStaleRootJobsInTx(ctx, tx); err != nil {
			_ = tx.Rollback()
			return LocalPhase0ScanResult{}, err
		}
	}
	s.result.CompletedAt = time.Now().UTC()
	if err := s.upsertRootStateInTx(ctx, tx, LocalMediaRootStatusReady, "", localPhase0StatusCompleted); err != nil {
		_ = tx.Rollback()
		return LocalPhase0ScanResult{}, err
	}
	if err := s.finishRunInTx(ctx, tx, localPhase0StatusCompleted, ""); err != nil {
		_ = tx.Rollback()
		return LocalPhase0ScanResult{}, err
	}
	if err := s.service.catalog.commitCatalogAssetChanges(ctx, tx, canonicalChanged); err != nil {
		return LocalPhase0ScanResult{}, fmt.Errorf("commit local phase0 scan: %w", err)
	}
	finished = true
	if len(s.result.SkipCounts) == 0 {
		s.result.SkipCounts = nil
	}
	return s.result, nil
}

func localPhase0LogicalWalkPath(rootPath string, walkPath string) string {
	if walkPath == "." {
		return rootPath
	}
	return filepath.Join(rootPath, filepath.FromSlash(walkPath))
}

func (s *localPhase0Scanner) handlePath(ctx context.Context, rootFS fs.FS, path string, entry fs.DirEntry) error {
	if path == s.root.Path || (entry != nil && entry.IsDir()) {
		if path != s.root.Path {
			name := entry.Name()
			if localPhase0SystemDirectory(name) {
				s.recordSkip("system_directory")
				return filepath.SkipDir
			}
			if s.shouldSkipHidden(name) {
				s.recordSkip("hidden_directory")
				return filepath.SkipDir
			}
			if localPhase0PackageLikeDirectory(name) {
				s.recordSkip("package_directory")
				return filepath.SkipDir
			}
		}
		return s.handleDirectory(path, entry)
	}
	if entry == nil {
		return nil
	}
	relativePath, err := filepath.Rel(s.root.Path, path)
	if err != nil {
		return fmt.Errorf("rel local media path: %w", err)
	}
	relativePath = filepath.ToSlash(relativePath)
	if s.scanMode == localPhase0ScanModeQuick {
		parentDirectory := pathParentDirectory(relativePath)
		if _, ok := s.checkedDirs[parentDirectory]; !ok {
			s.recordSkip("unchanged_directory_file")
			return nil
		}
	}
	name := entry.Name()
	if entry.Type()&fs.ModeSymlink != 0 {
		s.recordSkip("symlink")
		return nil
	}
	if s.shouldSkipHidden(name) {
		s.recordSkip("hidden_file")
		return nil
	}
	if !localPhase0SupportedMedia(name) {
		s.recordSkip("unsupported_extension")
		return nil
	}
	info, err := entry.Info()
	if errors.Is(err, fs.ErrNotExist) {
		// Some Unix DirEntry implementations resolve Info through the original
		// pathname. If the configured path is temporarily replaced while the
		// scan is using its pinned root, retry against that pinned root instead.
		info, err = fs.Stat(rootFS, relativePath)
	}
	if err != nil {
		s.seen[relativePath] = struct{}{}
		s.retryDirs[pathParentDirectory(relativePath)] = struct{}{}
		s.recordSkip("stat_error")
		return nil
	}
	if !info.Mode().IsRegular() {
		s.recordSkip("not_regular_file")
		return nil
	}
	s.seen[relativePath] = struct{}{}
	s.result.DiscoveredPaths++
	s.pending = append(s.pending, localPhase0PendingPath{
		relativePath: relativePath,
		info:         info,
	})
	if len(s.pending) >= localPhase0WriteBatchSize {
		return s.flushPendingPaths(ctx)
	}
	return nil
}

func (s *localPhase0Scanner) handleDirectory(path string, entry fs.DirEntry) error {
	relativePath := ""
	if path != s.root.Path {
		value, err := filepath.Rel(s.root.Path, path)
		if err != nil {
			return fmt.Errorf("rel local media directory: %w", err)
		}
		relativePath = filepath.ToSlash(value)
	}
	var info fs.FileInfo
	var err error
	if entry != nil {
		info, err = entry.Info()
	} else {
		info, err = os.Stat(path)
	}
	if err != nil {
		s.recordSkip("directory_stat_error")
		s.checkedDirs[relativePath] = struct{}{}
		return nil
	}
	mtimeText := formatCatalogTime(info.ModTime().UTC())
	s.seenDirectories[relativePath] = mtimeText
	previousMtime, found := s.directoryStates[relativePath]
	if s.scanMode == localPhase0ScanModeReconciliation || !found || previousMtime != mtimeText {
		s.checkedDirs[relativePath] = struct{}{}
	}
	return nil
}

func pathParentDirectory(relativePath string) string {
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relativePath)))
	if parent == "." {
		return ""
	}
	return parent
}

func (s *localPhase0Scanner) flushPendingPaths(ctx context.Context) error {
	if len(s.pending) == 0 {
		return nil
	}
	tx, err := s.service.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin local phase0 path batch: %w", err)
	}
	changedCount := 0
	queuedMetadataCount := 0
	for _, item := range s.pending {
		locationID, changed, needsMetadata, visibilityDirty, err := s.upsertLocationInTx(ctx, tx, item.relativePath, item.info)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if changed {
			changedCount++
		}
		if visibilityDirty {
			s.visibilityDirty = true
		}
		if needsMetadata {
			sortAt := formatCatalogTime(item.info.ModTime().UTC())
			queued, err := s.queueMetadataJobInTx(ctx, tx, locationID, sortAt)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			if queued {
				queuedMetadataCount++
			}
		} else if s.reconciliationPending {
			refreshed, err := s.refreshMetadataJobGenerationInTx(ctx, tx, locationID)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			if refreshed {
				queuedMetadataCount++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit local phase0 path batch: %w", err)
	}
	s.result.ChangedPaths += changedCount
	s.result.QueuedMetadata += queuedMetadataCount
	s.pending = s.pending[:0]
	if queuedMetadataCount > 0 {
		s.service.notifyLocalWorkQueued()
	}
	return nil
}

func (s *localPhase0Scanner) shouldSkipHidden(name string) bool {
	if s.datasource.Scan != nil && s.datasource.Scan.IncludeHiddenDirs {
		return false
	}
	return strings.HasPrefix(name, ".")
}

func localPhase0PackageLikeDirectory(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".app", ".bundle", ".photoslibrary", ".library":
		return true
	default:
		return false
	}
}

func localPhase0SystemDirectory(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "@eadir", "@recycle", "#recycle", "@recently-snapshot":
		return true
	default:
		return false
	}
}

func localPhase0SupportedMedia(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".heic", ".heif", ".mp4", ".mov":
		return true
	default:
		return false
	}
}

func (s *localPhase0Scanner) recordSkip(reason string) {
	s.result.SkippedPaths++
	if s.result.SkipCounts == nil {
		s.result.SkipCounts = map[string]int{}
	}
	s.result.SkipCounts[reason]++
}

func (s *localPhase0Scanner) recordBlockedPath(path string) {
	relativePath, err := filepath.Rel(s.root.Path, path)
	if err != nil || relativePath == "." {
		return
	}
	relativePath = filepath.ToSlash(relativePath)
	if relativePath == "" || relativePath == ".." || strings.HasPrefix(relativePath, "../") {
		return
	}
	for _, existing := range s.blocked {
		if existing == relativePath {
			return
		}
	}
	s.blocked = append(s.blocked, relativePath)
}

func (s *localPhase0Scanner) isBlockedRelativePath(relativePath string) bool {
	for _, blocked := range s.blocked {
		if relativePath == blocked || strings.HasPrefix(relativePath, blocked+"/") {
			return true
		}
	}
	return false
}

type localLocationPathRow struct {
	id            int64
	assetID       sql.NullString
	sizeBytes     int64
	mtime         string
	fastSignature string
	fileIdentity  string
	status        string
}

func (s *localPhase0Scanner) upsertLocationInTx(ctx context.Context, tx *sql.Tx, relativePath string, info fs.FileInfo) (int64, bool, bool, bool, error) {
	mtime := info.ModTime().UTC()
	mtimeText := formatCatalogTime(mtime)
	fastSignature := fmt.Sprintf("%d:%s", info.Size(), mtimeText)
	fileIdentity := localFileIdentity(info)

	existing, found, err := s.locationByPathInTx(ctx, tx, relativePath, false)
	if err != nil {
		return 0, false, false, false, fmt.Errorf("read local asset location: %w", err)
	}
	if !found {
		existing, found, err = s.locationByPathInTx(ctx, tx, relativePath, true)
		if err != nil {
			return 0, false, false, false, fmt.Errorf("read missing local asset location: %w", err)
		}
	}
	if found {
		sameSignature := existing.sizeBytes == info.Size() &&
			existing.mtime == mtimeText &&
			existing.fastSignature == fastSignature &&
			existing.fileIdentity == fileIdentity
		if sameSignature {
			switch {
			case existing.status == "active":
				if err := s.touchLocationInTx(ctx, tx, existing.id); err != nil {
					return 0, false, false, false, err
				}
				return existing.id, false, false, false, nil
			case (existing.status == "missing" || existing.status == "permission_blocked") && existing.assetID.Valid && existing.assetID.String != "":
				if err := s.restoreLocationInTx(ctx, tx, existing.id); err != nil {
					return 0, false, false, false, err
				}
				return existing.id, true, false, true, nil
			case existing.status == "discovered":
				if err := s.touchLocationInTx(ctx, tx, existing.id); err != nil {
					return 0, false, false, false, err
				}
				return existing.id, false, true, false, nil
			}
			visibilityDirty := existing.assetID.Valid && existing.assetID.String != ""
			if err := s.resetLocationForMetadataInTx(ctx, tx, existing.id, info.Size(), mtimeText, fastSignature, fileIdentity); err != nil {
				return 0, false, false, false, err
			}
			return existing.id, true, true, visibilityDirty, nil
		}
		if existing.status == "missing" {
			id, err := s.insertLocationInTx(ctx, tx, relativePath, info.Size(), mtimeText, fastSignature, fileIdentity)
			if err != nil {
				return 0, false, false, false, err
			}
			return id, true, true, false, nil
		}
		visibilityDirty := existing.assetID.Valid && existing.assetID.String != ""
		if err := s.resetLocationForMetadataInTx(ctx, tx, existing.id, info.Size(), mtimeText, fastSignature, fileIdentity); err != nil {
			return 0, false, false, false, err
		}
		return existing.id, true, true, visibilityDirty, nil
	}
	id, err := s.insertLocationInTx(ctx, tx, relativePath, info.Size(), mtimeText, fastSignature, fileIdentity)
	if err != nil {
		return 0, false, false, false, err
	}
	return id, true, true, false, nil
}

func (s *localPhase0Scanner) locationByPathInTx(ctx context.Context, tx *sql.Tx, relativePath string, missing bool) (localLocationPathRow, bool, error) {
	query := `SELECT id, asset_id, size_bytes, mtime, fast_signature, file_identity, status
		FROM local_asset_locations
		WHERE source_key = ? AND root_key = ? AND relative_path = ? AND status != 'missing'
		LIMIT 1`
	if missing {
		query = `SELECT id, asset_id, size_bytes, mtime, fast_signature, file_identity, status
			FROM local_asset_locations
			WHERE source_key = ? AND root_key = ? AND relative_path = ? AND status = 'missing'
			ORDER BY updated_at DESC, id DESC
			LIMIT 1`
	}
	var location localLocationPathRow
	err := tx.QueryRowContext(ctx, query, s.datasource.SourceKey, s.root.Key, relativePath).
		Scan(&location.id, &location.assetID, &location.sizeBytes, &location.mtime, &location.fastSignature, &location.fileIdentity, &location.status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return localLocationPathRow{}, false, nil
		}
		return localLocationPathRow{}, false, err
	}
	return location, true, nil
}

func (s *localPhase0Scanner) touchLocationInTx(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE local_asset_locations
		SET last_seen_at = ?, updated_at = ?
		WHERE id = ?`, s.nowText, s.nowText, id); err != nil {
		return fmt.Errorf("touch local asset location: %w", err)
	}
	return nil
}

func (s *localPhase0Scanner) restoreLocationInTx(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE local_asset_locations
		SET status = 'active',
			status_reason = NULL,
			last_seen_at = ?,
			verified_at = ?,
			updated_at = ?
		WHERE id = ?`, s.nowText, s.nowText, s.nowText, id); err != nil {
		return fmt.Errorf("restore local asset location: %w", err)
	}
	return nil
}

func (s *localPhase0Scanner) resetLocationForMetadataInTx(ctx context.Context, tx *sql.Tx, id int64, sizeBytes int64, mtimeText string, fastSignature string, fileIdentity string) error {
	metadataNotBefore := formatCatalogTime(s.startedAt.Add(s.settlingDuration))
	if _, err := tx.ExecContext(ctx, `UPDATE local_asset_locations
		SET asset_id = NULL,
			size_bytes = ?,
			mtime = ?,
			fast_signature = ?,
			file_identity = ?,
			sha1_hex = NULL,
			status = 'discovered',
			status_reason = NULL,
			last_seen_at = ?,
			verified_at = NULL,
			content_verified_at = NULL,
			content_verification_attempted_at = NULL,
			content_verification_error = NULL,
			metadata_not_before = ?,
			superseded_at = NULL,
			updated_at = ?
		WHERE id = ?`, sizeBytes, mtimeText, fastSignature, fileIdentity, s.nowText, metadataNotBefore, s.nowText, id); err != nil {
		return fmt.Errorf("update local asset location: %w", err)
	}
	return nil
}

func (s *localPhase0Scanner) insertLocationInTx(ctx context.Context, tx *sql.Tx, relativePath string, sizeBytes int64, mtimeText string, fastSignature string, fileIdentity string) (int64, error) {
	metadataNotBefore := formatCatalogTime(s.startedAt.Add(s.settlingDuration))
	result, err := tx.ExecContext(ctx, `INSERT INTO local_asset_locations (
			source_key, root_key, relative_path, size_bytes, mtime, fast_signature, file_identity,
			status, first_seen_at, last_seen_at, metadata_not_before, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'discovered', ?, ?, ?, ?)`,
		s.datasource.SourceKey,
		s.root.Key,
		relativePath,
		sizeBytes,
		mtimeText,
		fastSignature,
		fileIdentity,
		s.nowText,
		s.nowText,
		metadataNotBefore,
		s.nowText,
	)
	if err != nil {
		return 0, fmt.Errorf("insert local asset location: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read local asset location id: %w", err)
	}
	return id, nil
}

func (s *localPhase0Scanner) queueMetadataJobInTx(ctx context.Context, tx *sql.Tx, locationID int64, sortAt string) (bool, error) {
	var scheduledAt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT metadata_not_before FROM local_asset_locations WHERE id = ?`, locationID).Scan(&scheduledAt); err != nil {
		return false, fmt.Errorf("read local metadata eligibility: %w", err)
	}
	if !scheduledAt.Valid || strings.TrimSpace(scheduledAt.String) == "" {
		scheduledAt = sql.NullString{String: s.nowText, Valid: true}
	}
	result, err := tx.ExecContext(ctx, `UPDATE local_scan_jobs INDEXED BY idx_local_scan_jobs_location_pending
		SET scheduled_at = ?, sort_at = ?, root_key = ?, root_generation = ?
		WHERE source_key = ?
			AND job_kind = ?
			AND location_id = ?
			AND status = 'queued'
			AND (scheduled_at != ? OR sort_at != ? OR COALESCE(root_key, '') != ? OR root_generation != ?)`,
		scheduledAt.String,
		sortAt,
		s.root.Key,
		normalizeLocalMediaRootGeneration(s.rootGeneration),
		s.datasource.SourceKey,
		localMetadataJobKind,
		locationID,
		scheduledAt.String,
		sortAt,
		s.root.Key,
		normalizeLocalMediaRootGeneration(s.rootGeneration),
	)
	if err != nil {
		return false, fmt.Errorf("refresh local metadata job: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read refreshed local metadata jobs: %w", err)
	}
	if updated > 0 {
		return true, nil
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)
			FROM local_scan_jobs
			WHERE source_key = ?
				AND root_generation = ?
				AND job_kind = ?
				AND location_id = ?
				AND status IN ('queued', 'running')`,
		s.datasource.SourceKey,
		normalizeLocalMediaRootGeneration(s.rootGeneration),
		localMetadataJobKind,
		locationID,
	).Scan(&existing); err != nil {
		return false, fmt.Errorf("count local metadata jobs: %w", err)
	}
	if existing > 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, location_id, status, scheduled_at, sort_at
		) VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?)`,
		s.datasource.SourceKey,
		localMetadataJobKind,
		localMetadataBackgroundPriority,
		s.root.Key,
		normalizeLocalMediaRootGeneration(s.rootGeneration),
		locationID,
		scheduledAt.String,
		sortAt,
	); err != nil {
		return false, fmt.Errorf("queue local metadata job: %w", err)
	}
	return true, nil
}

func (s *localPhase0Scanner) refreshMetadataJobGenerationInTx(ctx context.Context, tx *sql.Tx, locationID int64) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE local_scan_jobs INDEXED BY idx_local_scan_jobs_location_pending
		SET root_key = ?, root_generation = ?
		WHERE source_key = ?
			AND job_kind = ?
			AND location_id = ?
			AND status = 'queued'
			AND (COALESCE(root_key, '') != ? OR root_generation != ?)`,
		s.root.Key,
		normalizeLocalMediaRootGeneration(s.rootGeneration),
		s.datasource.SourceKey,
		localMetadataJobKind,
		locationID,
		s.root.Key,
		normalizeLocalMediaRootGeneration(s.rootGeneration),
	)
	if err != nil {
		return false, fmt.Errorf("refresh local metadata job generation: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read refreshed local metadata job generation: %w", err)
	}
	return updated > 0, nil
}

func (s *localPhase0Scanner) loadDirectoryStates(ctx context.Context) error {
	rows, err := s.service.catalog.queryDB().QueryContext(ctx, `SELECT relative_path, mtime
		FROM local_scan_directories
		WHERE source_key = ? AND root_key = ?`, s.datasource.SourceKey, s.root.Key)
	if err != nil {
		return fmt.Errorf("query local scan directories: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var relativePath string
		var mtime string
		if err := rows.Scan(&relativePath, &mtime); err != nil {
			return fmt.Errorf("scan local directory state: %w", err)
		}
		s.directoryStates[relativePath] = mtime
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate local directory states: %w", err)
	}
	return nil
}

func (s *localPhase0Scanner) loadQuickRetryDirectories(ctx context.Context) error {
	rows, err := s.service.catalog.queryDB().QueryContext(ctx, `SELECT relative_path
		FROM local_asset_locations
		WHERE source_key = ? AND root_key = ? AND status = 'permission_blocked'`,
		s.datasource.SourceKey, s.root.Key)
	if err != nil {
		return fmt.Errorf("query local quick-discovery retry directories: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var relativePath string
		if err := rows.Scan(&relativePath); err != nil {
			return fmt.Errorf("scan local quick-discovery retry directory: %w", err)
		}
		s.checkedDirs[pathParentDirectory(relativePath)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate local quick-discovery retry directories: %w", err)
	}
	return nil
}

func (s *localPhase0Scanner) collectRemovedDirectories() {
	for relativePath := range s.directoryStates {
		if relativePath == "" {
			continue
		}
		if _, ok := s.seenDirectories[relativePath]; ok {
			continue
		}
		if s.isBlockedRelativePath(relativePath) {
			continue
		}
		s.removedDirs = append(s.removedDirs, relativePath)
	}
	sort.Strings(s.removedDirs)
}

func (s *localPhase0Scanner) persistDirectoryStates(ctx context.Context) error {
	tx, err := s.service.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin local directory state update: %w", err)
	}
	for relativePath, mtime := range s.seenDirectories {
		if _, retry := s.retryDirs[relativePath]; retry {
			continue
		}
		if s.scanMode == localPhase0ScanModeQuick && s.directoryStates[relativePath] == mtime {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO local_scan_directories (
				source_key, root_key, relative_path, mtime, updated_at
			) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(source_key, root_key, relative_path) DO UPDATE SET
				mtime = excluded.mtime,
				updated_at = excluded.updated_at`,
			s.datasource.SourceKey, s.root.Key, relativePath, mtime, s.nowText); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("upsert local directory state: %w", err)
		}
	}
	for _, relativePath := range s.removedDirs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM local_scan_directories
			WHERE source_key = ? AND root_key = ? AND relative_path = ?`,
			s.datasource.SourceKey, s.root.Key, relativePath); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete local directory state: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit local directory state update: %w", err)
	}
	return nil
}

func (s *localPhase0Scanner) markMissing(ctx context.Context) error {
	missingIDs, blockedIDs, currentLocationCount, err := s.missingLocationIDs(ctx)
	if err != nil {
		return err
	}
	rootContinuityUnverified := s.previousRootIdentity == "" || s.rootIdentity == "" || s.previousRootIdentity != s.rootIdentity
	if s.result.DiscoveredPaths == 0 && currentLocationCount > 0 && len(missingIDs) == currentLocationCount && rootContinuityUnverified {
		return fmt.Errorf("%w (%d known locations)", errLocalPhase0UnsafeEmptyReconciliation, currentLocationCount)
	}
	missingCount, err := s.markLocationIDs(ctx, missingIDs, "missing", "phase0_absent")
	if err != nil {
		return err
	}
	blockedCount, err := s.markLocationIDs(ctx, blockedIDs, "permission_blocked", "phase0_read_error")
	if err != nil {
		return err
	}
	if missingCount > 0 || blockedCount > 0 {
		s.visibilityDirty = true
	}
	s.result.MissingPaths = missingCount
	return nil
}

func (s *localPhase0Scanner) missingLocationIDs(ctx context.Context) ([]int64, []int64, int, error) {
	db := s.service.catalog.queryDB()
	rows, err := db.QueryContext(ctx, `SELECT id, relative_path
		FROM local_asset_locations
		WHERE source_key = ?
			AND root_key = ?
			AND status != 'missing'
			AND updated_at < ?`,
		s.datasource.SourceKey,
		s.root.Key,
		s.nowText,
	)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("query local paths for missing update: %w", err)
	}
	defer rows.Close()
	missingIDs := []int64{}
	blockedIDs := []int64{}
	currentLocationCount := 0
	for rows.Next() {
		var id int64
		var relativePath string
		if err := rows.Scan(&id, &relativePath); err != nil {
			return nil, nil, 0, fmt.Errorf("scan local path for missing update: %w", err)
		}
		currentLocationCount++
		if _, ok := s.seen[relativePath]; !ok {
			if s.isBlockedRelativePath(relativePath) {
				blockedIDs = append(blockedIDs, id)
				continue
			}
			if s.scanMode == localPhase0ScanModeQuick && !s.quickDiscoveryCoversPath(relativePath) {
				continue
			}
			missingIDs = append(missingIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, fmt.Errorf("iterate local paths for missing update: %w", err)
	}
	return missingIDs, blockedIDs, currentLocationCount, nil
}

func (s *localPhase0Scanner) quickDiscoveryCoversPath(relativePath string) bool {
	if _, ok := s.checkedDirs[pathParentDirectory(relativePath)]; ok {
		return true
	}
	for _, removedDirectory := range s.removedDirs {
		if relativePath == removedDirectory || strings.HasPrefix(relativePath, removedDirectory+"/") {
			return true
		}
	}
	return false
}

func (s *localPhase0Scanner) markLocationIDs(ctx context.Context, ids []int64, status string, reason string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	updated := 0
	for start := 0; start < len(ids); start += localPhase0MissingUpdateBatchSize {
		end := start + localPhase0MissingUpdateBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batchUpdated, err := s.markLocationIDBatch(ctx, ids[start:end], status, reason)
		if err != nil {
			return 0, err
		}
		updated += batchUpdated
	}
	return updated, nil
}

func (s *localPhase0Scanner) markLocationIDBatch(ctx context.Context, ids []int64, status string, reason string) (int, error) {
	tx, err := s.service.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin local phase0 location status update: %w", err)
	}
	statement, err := tx.PrepareContext(ctx, `UPDATE local_asset_locations
		SET status = ?,
			status_reason = ?,
			updated_at = ?
		WHERE id = ?
			AND source_key = ?
			AND root_key = ?
			AND status != 'missing'
			AND updated_at < ?`)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("prepare local phase0 location status update: %w", err)
	}
	updated := 0
	for _, id := range ids {
		result, err := statement.ExecContext(
			ctx,
			status,
			reason,
			s.nowText,
			id,
			s.datasource.SourceKey,
			s.root.Key,
			s.nowText,
		)
		if err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			return 0, fmt.Errorf("mark local asset location %s: %w", status, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			return 0, fmt.Errorf("read marked local asset location %s rows: %w", status, err)
		}
		updated += int(affected)
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("close local phase0 location status update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit local phase0 location status update: %w", err)
	}
	return updated, nil
}

func refreshLocalAssetVisibilityInTx(ctx context.Context, tx *sql.Tx, sourceKey string, rootKey string, nowText string) (map[string]struct{}, error) {
	sourceKey = strings.TrimSpace(sourceKey)
	rootKey = strings.TrimSpace(rootKey)
	changedCanonicalIDs := map[string]struct{}{}
	if sourceKey == "" || rootKey == "" {
		return changedCanonicalIDs, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_assets
		SET visibility_status = 'active',
			primary_location_id = (
				SELECT id
				FROM local_asset_locations
				WHERE local_asset_locations.source_key = local_assets.source_key
					AND local_asset_locations.root_key = ?
					AND local_asset_locations.asset_id = local_assets.asset_id
					AND local_asset_locations.status = 'active'
				ORDER BY COALESCE(local_asset_locations.verified_at, '') DESC,
					local_asset_locations.mtime DESC,
					local_asset_locations.root_key ASC,
					local_asset_locations.relative_path ASC,
					local_asset_locations.id ASC
				LIMIT 1
			),
			updated_at = ?
		WHERE source_key = ?
			AND EXISTS (
				SELECT 1
				FROM local_asset_locations
				WHERE local_asset_locations.source_key = local_assets.source_key
					AND local_asset_locations.root_key = ?
					AND local_asset_locations.asset_id = local_assets.asset_id
					AND local_asset_locations.status = 'active'
			)
			AND (
				visibility_status != 'active'
				OR primary_location_id IS NULL
				OR NOT EXISTS (
					SELECT 1
					FROM local_asset_locations primary_location
					WHERE primary_location.id = local_assets.primary_location_id
						AND primary_location.source_key = local_assets.source_key
						AND primary_location.root_key = ?
						AND primary_location.asset_id = local_assets.asset_id
						AND primary_location.status = 'active'
				)
			)`, rootKey, nowText, sourceKey, rootKey, rootKey); err != nil {
		return nil, fmt.Errorf("restore local asset visibility: %w", err)
	}
	if err := collectLocalCatalogCanonicalIDsInTx(ctx, tx, changedCanonicalIDs, `SELECT COALESCE(canonical_asset_id, '')
		FROM catalog_assets
		WHERE source_key = ?
			AND datasource_kind = ?
			AND visibility_status != 'active'
			AND EXISTS (
				SELECT 1
				FROM local_assets
				WHERE local_assets.source_key = catalog_assets.source_key
					AND local_assets.asset_id = catalog_assets.upstream_asset_id
					AND local_assets.visibility_status = 'active'
			)`, sourceKey, config.DatasourceKindLocalFiles); err != nil {
		return nil, fmt.Errorf("collect restored local catalog canonical ids: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE catalog_assets
		SET visibility_status = 'active',
			updated_at = ?
		WHERE source_key = ?
			AND datasource_kind = ?
			AND visibility_status != 'active'
			AND EXISTS (
				SELECT 1
				FROM local_assets
				WHERE local_assets.source_key = catalog_assets.source_key
					AND local_assets.asset_id = catalog_assets.upstream_asset_id
					AND local_assets.visibility_status = 'active'
			)`, nowText, sourceKey, config.DatasourceKindLocalFiles); err != nil {
		return nil, fmt.Errorf("restore local catalog visibility: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_assets
		SET visibility_status = 'permission_blocked',
			primary_location_id = NULL,
			updated_at = ?
		WHERE source_key = ?
			AND visibility_status != 'permission_blocked'
			AND NOT EXISTS (
				SELECT 1
				FROM local_asset_locations
				WHERE local_asset_locations.source_key = local_assets.source_key
					AND local_asset_locations.root_key = ?
					AND local_asset_locations.asset_id = local_assets.asset_id
					AND local_asset_locations.status = 'active'
			)
			AND EXISTS (
				SELECT 1
				FROM local_asset_locations
				WHERE local_asset_locations.source_key = local_assets.source_key
					AND local_asset_locations.root_key = ?
					AND local_asset_locations.asset_id = local_assets.asset_id
					AND local_asset_locations.status = 'permission_blocked'
			)`, nowText, sourceKey, rootKey, rootKey); err != nil {
		return nil, fmt.Errorf("refresh permission-blocked local asset visibility: %w", err)
	}
	if err := collectLocalCatalogCanonicalIDsInTx(ctx, tx, changedCanonicalIDs, `SELECT COALESCE(canonical_asset_id, '')
		FROM catalog_assets
		WHERE source_key = ?
			AND datasource_kind = ?
			AND visibility_status != 'permission_blocked'
			AND EXISTS (
				SELECT 1
				FROM local_assets
				WHERE local_assets.source_key = catalog_assets.source_key
					AND local_assets.asset_id = catalog_assets.upstream_asset_id
					AND local_assets.visibility_status = 'permission_blocked'
			)`, sourceKey, config.DatasourceKindLocalFiles); err != nil {
		return nil, fmt.Errorf("collect permission-blocked local catalog canonical ids: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE catalog_assets
		SET visibility_status = 'permission_blocked',
			updated_at = ?
		WHERE source_key = ?
			AND datasource_kind = ?
			AND visibility_status != 'permission_blocked'
			AND EXISTS (
				SELECT 1
				FROM local_assets
				WHERE local_assets.source_key = catalog_assets.source_key
					AND local_assets.asset_id = catalog_assets.upstream_asset_id
					AND local_assets.visibility_status = 'permission_blocked'
			)`, nowText, sourceKey, config.DatasourceKindLocalFiles); err != nil {
		return nil, fmt.Errorf("refresh permission-blocked local catalog visibility: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_assets
		SET visibility_status = 'missing',
			primary_location_id = NULL,
			updated_at = ?
		WHERE source_key = ?
			AND visibility_status != 'missing'
			AND NOT EXISTS (
				SELECT 1
				FROM local_asset_locations
				WHERE local_asset_locations.source_key = local_assets.source_key
					AND local_asset_locations.root_key = ?
					AND local_asset_locations.asset_id = local_assets.asset_id
					AND local_asset_locations.status = 'active'
			)
			AND NOT EXISTS (
				SELECT 1
				FROM local_asset_locations
				WHERE local_asset_locations.source_key = local_assets.source_key
					AND local_asset_locations.root_key = ?
					AND local_asset_locations.asset_id = local_assets.asset_id
					AND local_asset_locations.status = 'permission_blocked'
			)`, nowText, sourceKey, rootKey, rootKey); err != nil {
		return nil, fmt.Errorf("refresh local asset visibility: %w", err)
	}
	if err := collectLocalCatalogCanonicalIDsInTx(ctx, tx, changedCanonicalIDs, `SELECT COALESCE(canonical_asset_id, '')
		FROM catalog_assets
		WHERE source_key = ?
			AND datasource_kind = ?
			AND visibility_status != 'missing'
			AND NOT EXISTS (
				SELECT 1
				FROM local_assets
				WHERE local_assets.source_key = catalog_assets.source_key
					AND local_assets.asset_id = catalog_assets.upstream_asset_id
					AND local_assets.visibility_status = 'active'
			)
			AND NOT EXISTS (
				SELECT 1
				FROM local_assets
				WHERE local_assets.source_key = catalog_assets.source_key
					AND local_assets.asset_id = catalog_assets.upstream_asset_id
					AND local_assets.visibility_status = 'permission_blocked'
			)`, sourceKey, config.DatasourceKindLocalFiles); err != nil {
		return nil, fmt.Errorf("collect missing local catalog canonical ids: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE catalog_assets
		SET visibility_status = 'missing',
			updated_at = ?
		WHERE source_key = ?
			AND datasource_kind = ?
			AND visibility_status != 'missing'
			AND NOT EXISTS (
				SELECT 1
				FROM local_assets
				WHERE local_assets.source_key = catalog_assets.source_key
					AND local_assets.asset_id = catalog_assets.upstream_asset_id
					AND local_assets.visibility_status = 'active'
			)
			AND NOT EXISTS (
				SELECT 1
				FROM local_assets
				WHERE local_assets.source_key = catalog_assets.source_key
					AND local_assets.asset_id = catalog_assets.upstream_asset_id
					AND local_assets.visibility_status = 'permission_blocked'
			)`, nowText, sourceKey, config.DatasourceKindLocalFiles); err != nil {
		return nil, fmt.Errorf("refresh local catalog visibility: %w", err)
	}
	return changedCanonicalIDs, nil
}

func collectLocalCatalogCanonicalIDsInTx(ctx context.Context, tx *sql.Tx, ids map[string]struct{}, query string, args ...any) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		id = strings.TrimSpace(id)
		if id != "" {
			ids[id] = struct{}{}
		}
	}
	return rows.Err()
}

func (s *localPhase0Scanner) insertRun(ctx context.Context, rootStatus string, rootErr error) (int64, error) {
	rootFailure := ""
	if rootErr != nil {
		rootFailure = rootErr.Error()
	}
	result, err := s.service.catalog.db.ExecContext(ctx, `INSERT INTO local_scan_runs (
			source_key, root_key, scan_mode, started_at, status, root_status_at_start, root_failure_reason
		) VALUES (?, ?, ?, ?, 'running', ?, ?)`,
		s.datasource.SourceKey,
		s.root.Key,
		s.scanMode,
		s.nowText,
		rootStatus,
		nullableCatalogText(rootFailure),
	)
	if err != nil {
		return 0, fmt.Errorf("insert local scan run: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read local scan run id: %w", err)
	}
	return id, nil
}

func (s *localPhase0Scanner) finishBlockedRun(ctx context.Context) error {
	tx, err := s.service.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin blocked local phase0 scan finish: %w", err)
	}
	if err := s.upsertRootStateInTx(ctx, tx, s.result.RootStatus, s.result.LastError, localPhase0StatusBlocked); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.finishRunInTx(ctx, tx, localPhase0StatusBlocked, s.result.LastError); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit blocked local phase0 scan finish: %w", err)
	}
	return nil
}

func (s *localPhase0Scanner) finishFailedRun(ctx context.Context, runErr error, rootWalkFailed bool) error {
	tx, err := s.service.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin failed local phase0 scan finish: %w", err)
	}
	rootStatus := LocalMediaRootStatusReady
	rootError := ""
	if s.result.RootStatus == LocalMediaRootStatusIdentityChanged {
		rootStatus = LocalMediaRootStatusIdentityChanged
		rootError = runErr.Error()
	} else if rootWalkFailed {
		rootStatus = LocalMediaRootStatusUnreadable
		rootError = runErr.Error()
	}
	if err := s.upsertFailedRootStateInTx(ctx, tx, rootStatus, rootError); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.finishRunInTx(ctx, tx, localPhase0StatusFailed, runErr.Error()); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit failed local phase0 scan finish: %w", err)
	}
	return nil
}

func (s *localPhase0Scanner) upsertFailedRootStateInTx(ctx context.Context, tx *sql.Tx, rootStatus string, lastError string) error {
	nextPhase0 := formatCatalogTime(s.result.CompletedAt.Add(localDatasourceQuickScanInterval(s.datasource)))
	_, err := tx.ExecContext(ctx, `INSERT INTO local_scan_root_state (
			source_key, root_key, root_status, root_last_checked_at, root_last_error,
			phase0_status, next_phase0_at, last_phase0_run_id, root_generation,
			reconciliation_pending, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_key, root_key) DO UPDATE SET
			root_status = excluded.root_status,
			root_last_checked_at = excluded.root_last_checked_at,
			root_last_error = excluded.root_last_error,
			phase0_status = excluded.phase0_status,
			next_phase0_at = excluded.next_phase0_at,
			last_phase0_run_id = excluded.last_phase0_run_id,
			updated_at = excluded.updated_at`,
		s.datasource.SourceKey,
		s.root.Key,
		rootStatus,
		formatCatalogTime(s.startedAt),
		nullableCatalogText(lastError),
		localPhase0StatusFailed,
		nextPhase0,
		s.scanRunID,
		normalizeLocalMediaRootGeneration(s.rootGeneration),
		boolInt(s.reconciliationPending),
		formatCatalogTime(s.result.CompletedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert failed local scan root state: %w", err)
	}
	return nil
}

func (s *localPhase0Scanner) upsertRootStateInTx(ctx context.Context, tx *sql.Tx, rootStatus string, lastError string, phase0Status string) error {
	nextPhase0 := formatCatalogTime(s.result.CompletedAt.Add(localDatasourceQuickScanInterval(s.datasource)))
	lastQuickScanAt := ""
	lastReconciliationAt := ""
	if phase0Status == localPhase0StatusCompleted {
		if s.scanMode == localPhase0ScanModeQuick {
			lastQuickScanAt = formatCatalogTime(s.result.CompletedAt)
		} else {
			lastReconciliationAt = formatCatalogTime(s.result.CompletedAt)
		}
	}
	reconciliationPending := s.reconciliationPending
	if phase0Status == localPhase0StatusCompleted && s.scanMode == localPhase0ScanModeReconciliation {
		reconciliationPending = false
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO local_scan_root_state (
			source_key, root_key, root_status, root_last_checked_at, root_last_error,
			phase0_status, next_phase0_at, last_phase0_run_id, last_quick_scan_at, last_reconciliation_at,
			root_identity, root_generation, reconciliation_pending, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_key, root_key) DO UPDATE SET
			root_status = excluded.root_status,
			root_last_checked_at = excluded.root_last_checked_at,
			root_last_error = excluded.root_last_error,
			phase0_status = excluded.phase0_status,
			next_phase0_at = excluded.next_phase0_at,
			last_phase0_run_id = excluded.last_phase0_run_id,
			last_quick_scan_at = COALESCE(excluded.last_quick_scan_at, local_scan_root_state.last_quick_scan_at),
			last_reconciliation_at = COALESCE(excluded.last_reconciliation_at, local_scan_root_state.last_reconciliation_at),
			root_identity = CASE
				WHEN excluded.root_identity <> '' THEN excluded.root_identity
				ELSE local_scan_root_state.root_identity
			END,
			root_generation = CASE
				WHEN excluded.phase0_status = 'completed' THEN excluded.root_generation
				ELSE local_scan_root_state.root_generation
			END,
			reconciliation_pending = CASE
				WHEN excluded.phase0_status = 'completed' THEN excluded.reconciliation_pending
				ELSE local_scan_root_state.reconciliation_pending
			END,
			updated_at = excluded.updated_at`,
		s.datasource.SourceKey,
		s.root.Key,
		rootStatus,
		formatCatalogTime(s.result.CompletedAt),
		nullableCatalogText(lastError),
		phase0Status,
		nullableCatalogText(nextPhase0),
		s.scanRunID,
		nullableCatalogText(lastQuickScanAt),
		nullableCatalogText(lastReconciliationAt),
		s.rootIdentity,
		normalizeLocalMediaRootGeneration(s.rootGeneration),
		boolInt(reconciliationPending),
		formatCatalogTime(s.result.CompletedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert local scan root state: %w", err)
	}
	return nil
}

func (s *localPhase0Scanner) deleteStaleRootJobsInTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_scan_jobs
		WHERE source_key = ?
			AND (
				COALESCE(root_key, '') != ?
				OR root_generation != ?
			)`,
		s.datasource.SourceKey,
		s.root.Key,
		normalizeLocalMediaRootGeneration(s.rootGeneration),
	); err != nil {
		return fmt.Errorf("delete stale local root jobs: %w", err)
	}
	return nil
}

func (s *localPhase0Scanner) finishRunInTx(ctx context.Context, tx *sql.Tx, status string, lastError string) error {
	skipCounts := "{}"
	if len(s.result.SkipCounts) > 0 {
		keys := make([]string, 0, len(s.result.SkipCounts))
		for key := range s.result.SkipCounts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		ordered := map[string]int{}
		for _, key := range keys {
			ordered[key] = s.result.SkipCounts[key]
		}
		raw, err := json.Marshal(ordered)
		if err != nil {
			return fmt.Errorf("marshal local phase0 skip counts: %w", err)
		}
		skipCounts = string(raw)
	}
	_, err := tx.ExecContext(ctx, `UPDATE local_scan_runs
		SET completed_at = ?,
			status = ?,
			root_failure_reason = ?,
			discovered_paths = ?,
			changed_paths = ?,
			queued_metadata = ?,
			missing_paths = ?,
			skipped_paths = ?,
			skip_counts_json = ?
		WHERE id = ?`,
		formatCatalogTime(s.result.CompletedAt),
		status,
		nullableCatalogText(lastError),
		s.result.DiscoveredPaths,
		s.result.ChangedPaths,
		s.result.QueuedMetadata,
		s.result.MissingPaths,
		s.result.SkippedPaths,
		skipCounts,
		s.scanRunID,
	)
	if err != nil {
		return fmt.Errorf("finish local scan run: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_scan_runs
		WHERE id IN (
			SELECT id
			FROM local_scan_runs
			WHERE source_key = ? AND root_key = ? AND scan_mode = ? AND status != 'running'
			ORDER BY started_at DESC, id DESC
			LIMIT -1 OFFSET ?
		)`, s.datasource.SourceKey, s.root.Key, s.scanMode, localScanRunRetentionPerMode); err != nil {
		return fmt.Errorf("prune local scan run history: %w", err)
	}
	return nil
}
