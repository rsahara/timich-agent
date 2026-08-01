package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

const (
	defaultLocalThumbnailBatchSize = 20
	localThumbnailJobKind          = "thumbnail"
	localMediaVipsRenderTimeout    = 60 * time.Second
	localMediaFFmpegPosterTimeout  = 60 * time.Second
	defaultFirstViewThumbnailCount = 120

	localThumbnailRepairPriority     = 1
	localThumbnailFirstViewPriority  = 2
	localThumbnailBackgroundPriority = 5

	localRenditionKindPreview       = "preview"
	localRenditionKindDetailPreview = "detail_preview"
	localRenditionsDirName          = "local-renditions"

	localRenditionResizeFallbackAttempts = 2
	localRenditionResizeQuantumPixels    = 16
	localRenditionResizeSafetyFactor     = 0.97
)

var (
	localPreviewJPEGQualities       = []int{58, 42}
	localDetailPreviewJPEGQualities = []int{detailPreviewJPEGQuality, 62, 42}
)

type LocalThumbnailBatchResult struct {
	ProcessedJobs        int       `json:"processedJobs"`
	CompletedJobs        int       `json:"completedJobs"`
	FailedJobs           int       `json:"failedJobs"`
	DeferredJobs         int       `json:"deferredJobs"`
	GeneratedAssets      int       `json:"generatedAssets"`
	GeneratedImageAssets int       `json:"generatedImageAssets"`
	ResettledAssets      int       `json:"resettledAssets"`
	StartedAt            time.Time `json:"startedAt"`
	CompletedAt          time.Time `json:"completedAt"`
}

type LocalThumbnailRequeueResult struct {
	Queued int `json:"queued"`
}

type localThumbnailJob struct {
	ID             int64
	SourceKey      string
	RootKey        string
	RootGeneration int64
	AssetID        string
	MediaType      string
	Priority       int
	SortAt         string
	ScheduledAt    string
}

type localThumbnailAsset struct {
	SourceKey        string
	AssetID          string
	SHA1Hex          string
	MediaType        string
	VisibilityStatus string
	ThumbnailStatus  string
}

var errLocalThumbnailSourceChanged = errors.New("local thumbnail source changed")

type localRenderedRendition struct {
	Kind         string
	RelativePath string
	Bytes        []byte
	Width        int
	Height       int
}

type localReadyRendition struct {
	Kind         string
	RelativePath string
	SizeBytes    int64
	SHA256       string
}

type localMediaInput struct {
	Path         string
	File         *os.File
	Root         *os.Root
	RelativePath string
}

func (input localMediaInput) helperPath() string {
	if input.File != nil {
		return "/dev/fd/3"
	}
	return input.Path
}

func (input localMediaInput) stat() (os.FileInfo, error) {
	if input.File != nil {
		return input.File.Stat()
	}
	return os.Stat(input.Path)
}

func (s *Service) RunLocalThumbnailBatch(ctx context.Context, maxJobs int) (LocalThumbnailBatchResult, error) {
	return s.runLocalThumbnailBatch(ctx, "", maxJobs, 1, true)
}

func (s *Service) RunLocalThumbnailBatchForSource(ctx context.Context, sourceKey string, maxJobs int) (LocalThumbnailBatchResult, error) {
	return s.runLocalThumbnailBatch(ctx, sourceKey, maxJobs, 1, true)
}

func (s *Service) RunLocalThumbnailBatchWithWorkers(ctx context.Context, maxJobs int, workers int) (LocalThumbnailBatchResult, error) {
	return s.runLocalThumbnailBatch(ctx, "", maxJobs, workers, true)
}

func (s *Service) RequeueFailedLocalThumbnails(ctx context.Context) (LocalThumbnailRequeueResult, error) {
	if s == nil || s.catalog == nil {
		return LocalThumbnailRequeueResult{}, ErrNoDatasourceConfigured
	}
	if err := s.ensureStateWritesAvailable(); err != nil {
		return LocalThumbnailRequeueResult{}, err
	}
	sourceKeys := s.LocalDatasourceSourceKeys()
	if len(sourceKeys) == 0 {
		return LocalThumbnailRequeueResult{}, ErrNoDatasourceConfigured
	}
	totalQueued := 0
	for _, sourceKey := range sourceKeys {
		queued, err := s.requeueFailedLocalThumbnailsForSource(ctx, sourceKey)
		if err != nil {
			return LocalThumbnailRequeueResult{Queued: totalQueued}, err
		}
		totalQueued += queued
	}
	return LocalThumbnailRequeueResult{Queued: totalQueued}, nil
}

func (s *Service) runLocalThumbnailBatch(ctx context.Context, sourceKey string, maxJobs int, workers int, queuePending bool) (LocalThumbnailBatchResult, error) {
	if s == nil || s.catalog == nil {
		return LocalThumbnailBatchResult{}, ErrNoDatasourceConfigured
	}
	if err := s.ensureStateWritesAvailable(); err != nil {
		return LocalThumbnailBatchResult{}, err
	}
	if err := s.recoverRememberedLocalClaims(ctx); err != nil {
		return LocalThumbnailBatchResult{}, err
	}
	sourceKey = strings.TrimSpace(sourceKey)
	var datasource *config.DatasourceConfig
	if sourceKey != "" {
		var err error
		datasource, _, err = s.localDatasourceAndRoot(sourceKey)
		if err != nil {
			return LocalThumbnailBatchResult{}, err
		}
	}
	if maxJobs <= 0 {
		maxJobs = defaultLocalThumbnailBatchSize
	}
	workers = min(max(workers, 1), maxJobs)
	startedAt := time.Now().UTC()
	result := LocalThumbnailBatchResult{StartedAt: startedAt}
	if queuePending {
		if sourceKey == "" {
			if err := s.queuePendingLocalThumbnailJobs(ctx, maxJobs); err != nil {
				return LocalThumbnailBatchResult{}, err
			}
		} else if err := s.queuePendingLocalThumbnailJobsForSource(ctx, datasource.SourceKey, effectiveFirstViewThumbnailCount(*datasource), maxJobs); err != nil {
			return LocalThumbnailBatchResult{}, err
		}
	}
	jobs, err := s.nextLocalThumbnailJobs(ctx, sourceKey, maxJobs)
	if err != nil {
		return LocalThumbnailBatchResult{}, err
	}
	jobsChannel := make(chan localThumbnailJob)
	var resultMu sync.Mutex
	var batchErr error
	var wait sync.WaitGroup
	workerCount := min(workers, len(jobs))
	for range workerCount {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for job := range jobsChannel {
				if ctx.Err() != nil {
					return
				}
				generated, jobErr := s.processLocalThumbnailJob(ctx, job)
				if ctx.Err() != nil {
					if _, recoveryErr := s.deferClaimedLocalThumbnailJob(job); recoveryErr != nil {
						log.Printf("timich-agent local thumbnail canceled job recovery failed job_id=%d error=%v", job.ID, recoveryErr)
						s.rememberLocalThumbnailClaimRecovery(job)
					}
					return
				}
				deferred := errors.Is(jobErr, ErrLocalMediaRootNotTrusted)
				if deferred {
					if _, recoveryErr := s.deferClaimedLocalThumbnailJob(job); recoveryErr != nil {
						log.Printf("timich-agent local thumbnail deferred job recovery failed job_id=%d error=%v", job.ID, recoveryErr)
						s.rememberLocalThumbnailClaimRecovery(job)
						resultMu.Lock()
						batchErr = errors.Join(batchErr, fmt.Errorf("defer local thumbnail job %d: %w", job.ID, recoveryErr))
						resultMu.Unlock()
					}
				} else if jobErr != nil && !errors.Is(jobErr, errLocalThumbnailSourceChanged) {
					failureDeferred, failureErr := s.failLocalThumbnailJob(ctx, job, jobErr)
					if failureErr != nil {
						recovered, recoveryErr := s.deferClaimedLocalThumbnailJob(job)
						if recoveryErr != nil {
							log.Printf("timich-agent local thumbnail failure publication and claim recovery failed job_id=%d publication_error=%v recovery_error=%v", job.ID, failureErr, recoveryErr)
							s.rememberLocalThumbnailClaimRecovery(job)
							resultMu.Lock()
							batchErr = errors.Join(batchErr, fmt.Errorf("recover local thumbnail job %d after failure publication: %w", job.ID, errors.Join(failureErr, recoveryErr)))
							resultMu.Unlock()
						} else if !recovered {
							log.Printf("timich-agent local thumbnail failure publication failed and claim was not recoverable job_id=%d error=%v", job.ID, failureErr)
							resultMu.Lock()
							batchErr = errors.Join(batchErr, fmt.Errorf("recover local thumbnail job %d after failure publication: exact running claim not found: %w", job.ID, failureErr))
							resultMu.Unlock()
						} else {
							log.Printf("timich-agent local thumbnail failure publication failed; exact claim deferred job_id=%d error=%v", job.ID, failureErr)
							failureDeferred = true
						}
					}
					deferred = failureDeferred
				}
				resultMu.Lock()
				result.ProcessedJobs++
				switch {
				case deferred:
					result.DeferredJobs++
				case errors.Is(jobErr, errLocalThumbnailSourceChanged):
					result.ResettledAssets++
					result.CompletedJobs++
				case jobErr != nil:
					result.FailedJobs++
				default:
					result.CompletedJobs++
					if generated {
						result.GeneratedAssets++
						if job.MediaType == "image" {
							result.GeneratedImageAssets++
						}
					}
				}
				resultMu.Unlock()
			}
		}()
	}
sendJobs:
	for _, job := range jobs {
		select {
		case jobsChannel <- job:
		case <-ctx.Done():
			break sendJobs
		}
	}
	close(jobsChannel)
	wait.Wait()
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), localClaimedJobRecoveryTimeout)
	if recoveryErr := s.recoverRememberedLocalClaims(recoveryCtx); recoveryErr != nil {
		batchErr = errors.Join(batchErr, recoveryErr)
	}
	recoveryCancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.CompletedAt = time.Now().UTC()
	if batchErr != nil {
		return result, batchErr
	}
	return result, nil
}

func (s *Service) queuePendingLocalThumbnailJobs(ctx context.Context, limit int) error {
	if limit <= 0 {
		return nil
	}
	trustedRoots, err := s.trustedLocalMediaRootReferences(ctx, "")
	if err != nil {
		return err
	}
	if len(trustedRoots) == 0 {
		return nil
	}
	perSourceLimit := max(1, limit)
	if len(trustedRoots) > 1 {
		perSourceLimit = max(1, (limit+len(trustedRoots)-1)/len(trustedRoots))
	}
	for _, trustedRoot := range trustedRoots {
		datasource, _, err := s.localDatasourceAndRoot(trustedRoot.sourceKey)
		if err != nil {
			return err
		}
		if err := s.queuePendingLocalThumbnailJobsForSource(ctx, datasource.SourceKey, effectiveFirstViewThumbnailCount(*datasource), perSourceLimit); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) queuePendingLocalThumbnailJobsForSource(ctx context.Context, sourceKey string, firstViewCount int, limit int) error {
	if limit <= 0 || strings.TrimSpace(s.mediaHelperPath) == "" {
		return nil
	}
	trustedRoot, err := s.acquireTrustedLocalMediaRoot(ctx, sourceKey)
	if err != nil {
		if errors.Is(err, ErrLocalMediaRootNotTrusted) {
			return nil
		}
		return err
	}
	defer trustedRoot.Close()
	if trustedRoot.reconciliationPending {
		return nil
	}
	nowText := formatCatalogTime(time.Now().UTC())
	videoEnabled := boolInt(s.localVideoPosterEnabled())
	_, err = s.catalog.db.ExecContext(ctx, `WITH ranked AS (
			SELECT a.source_key,
				a.asset_id,
				a.thumbnail_status,
				a.captured_at,
				row_number() OVER (ORDER BY a.captured_at DESC, a.asset_id ASC) AS rank
			FROM local_assets a
			WHERE a.source_key = ?
				AND (a.media_type = 'image' OR (? = 1 AND a.media_type = 'video'))
				AND a.visibility_status = 'active'
		)
		INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, asset_id, status, scheduled_at, sort_at
		)
		SELECT ranked.source_key,
			?,
			CASE
				WHEN ? > 0 AND ranked.rank <= ? THEN ?
				ELSE ?
			END,
			?,
			?,
			ranked.asset_id,
			'queued',
			?,
			ranked.captured_at
		FROM ranked
		WHERE ranked.thumbnail_status = 'pending'
			AND NOT EXISTS (
				SELECT 1
				FROM local_scan_jobs j
				WHERE j.source_key = ranked.source_key
					AND j.asset_id = ranked.asset_id
					AND j.job_kind = ?
					AND j.root_key = ?
					AND j.root_generation = ?
					AND j.status IN ('queued', 'running')
			)
		ORDER BY CASE
				WHEN ? > 0 AND ranked.rank <= ? THEN ranked.rank
				ELSE 1000000
			END ASC,
			substr(ranked.asset_id, -8) ASC,
			ranked.asset_id ASC
		LIMIT ?`,
		sourceKey,
		videoEnabled,
		localThumbnailJobKind,
		firstViewCount,
		firstViewCount,
		localThumbnailFirstViewPriority,
		localThumbnailBackgroundPriority,
		trustedRoot.root.Key,
		trustedRoot.rootGeneration,
		nowText,
		localThumbnailJobKind,
		trustedRoot.root.Key,
		trustedRoot.rootGeneration,
		firstViewCount,
		firstViewCount,
		limit,
	)
	if err != nil {
		return fmt.Errorf("queue pending local thumbnail jobs: %w", err)
	}
	return nil
}

func (s *Service) requeueFailedLocalThumbnailsForSource(ctx context.Context, sourceKey string) (int, error) {
	trustedRoot, err := s.acquireTrustedLocalMediaRoot(ctx, sourceKey)
	if err != nil {
		if errors.Is(err, ErrLocalMediaRootNotTrusted) {
			return 0, nil
		}
		return 0, err
	}
	defer trustedRoot.Close()
	if trustedRoot.reconciliationPending {
		return 0, nil
	}
	nowText := formatCatalogTime(time.Now().UTC())
	videoEnabled := boolInt(s.localVideoPosterEnabled())
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin failed local thumbnail requeue: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_scan_jobs
		SET priority = ?,
			status = 'queued',
			attempts = 0,
			scheduled_at = ?,
			sort_at = COALESCE((
				SELECT a.captured_at
				FROM local_assets a
				WHERE a.source_key = local_scan_jobs.source_key
					AND a.asset_id = local_scan_jobs.asset_id
			), sort_at),
			locked_at = NULL,
			completed_at = NULL,
			last_error = NULL
		WHERE id IN (
			SELECT COALESCE(
				MAX(CASE WHEN j.status = 'queued' THEN j.id END),
				MAX(j.id)
			)
			FROM local_scan_jobs j
			JOIN local_assets a
				ON a.source_key = j.source_key
				AND a.asset_id = j.asset_id
			WHERE a.source_key = ?
				AND (a.media_type = 'image' OR (? = 1 AND a.media_type = 'video'))
				AND a.visibility_status = 'active'
				AND a.thumbnail_status = 'failed'
				AND j.job_kind = ?
				AND j.root_key = ?
				AND j.root_generation = ?
				AND j.status IN ('queued', 'failed')
				AND NOT EXISTS (
					SELECT 1
					FROM local_scan_jobs active_job
					WHERE active_job.source_key = a.source_key
						AND active_job.asset_id = a.asset_id
						AND active_job.job_kind = ?
						AND active_job.root_key = j.root_key
						AND active_job.root_generation = j.root_generation
						AND active_job.status = 'running'
				)
			GROUP BY j.source_key, j.asset_id
		)`,
		localThumbnailRepairPriority,
		nowText,
		sourceKey,
		videoEnabled,
		localThumbnailJobKind,
		trustedRoot.root.Key,
		trustedRoot.rootGeneration,
		localThumbnailJobKind,
	); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("prioritize or requeue failed local thumbnail jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, asset_id, status, scheduled_at, sort_at
		)
		SELECT a.source_key,
			?,
			?,
			?,
			?,
			a.asset_id,
			'queued',
			?,
			a.captured_at
		FROM local_assets a
		WHERE a.source_key = ?
			AND (a.media_type = 'image' OR (? = 1 AND a.media_type = 'video'))
			AND a.visibility_status = 'active'
			AND a.thumbnail_status = 'failed'
			AND NOT EXISTS (
				SELECT 1
				FROM local_scan_jobs j
				WHERE j.source_key = a.source_key
					AND j.asset_id = a.asset_id
					AND j.job_kind = ?
					AND j.root_key = ?
					AND j.root_generation = ?
					AND j.status IN ('queued', 'running')
			)
		ORDER BY a.captured_at DESC, a.asset_id ASC`,
		localThumbnailJobKind,
		localThumbnailRepairPriority,
		trustedRoot.root.Key,
		trustedRoot.rootGeneration,
		nowText,
		sourceKey,
		videoEnabled,
		localThumbnailJobKind,
		trustedRoot.root.Key,
		trustedRoot.rootGeneration,
	); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("insert failed local thumbnail jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_scan_jobs
		WHERE source_key = ?
			AND job_kind = ?
			AND root_key = ?
			AND root_generation = ?
			AND status = 'failed'
			AND EXISTS (
				SELECT 1
				FROM local_assets a
				WHERE a.source_key = local_scan_jobs.source_key
					AND a.asset_id = local_scan_jobs.asset_id
					AND (a.media_type = 'image' OR (? = 1 AND a.media_type = 'video'))
					AND a.visibility_status = 'active'
					AND a.thumbnail_status = 'failed'
			)
			AND EXISTS (
				SELECT 1
				FROM local_scan_jobs active_job
				WHERE active_job.source_key = local_scan_jobs.source_key
					AND active_job.asset_id = local_scan_jobs.asset_id
					AND active_job.job_kind = ?
					AND active_job.root_key = local_scan_jobs.root_key
					AND active_job.root_generation = local_scan_jobs.root_generation
					AND active_job.status IN ('queued', 'running')
			)`,
		sourceKey,
		localThumbnailJobKind,
		trustedRoot.root.Key,
		trustedRoot.rootGeneration,
		videoEnabled,
		localThumbnailJobKind,
	); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("complete superseded failed local thumbnail jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_renditions
		SET status = 'pending',
			last_error = NULL
		WHERE source_key = ?
			AND status = 'failed'
			AND EXISTS (
				SELECT 1
				FROM local_assets a
				WHERE a.source_key = local_renditions.source_key
					AND a.asset_id = local_renditions.asset_id
					AND (a.media_type = 'image' OR (? = 1 AND a.media_type = 'video'))
					AND a.visibility_status = 'active'
					AND a.thumbnail_status = 'failed'
			)
			AND EXISTS (
				SELECT 1
				FROM local_scan_jobs active_job
				WHERE active_job.source_key = local_renditions.source_key
					AND active_job.asset_id = local_renditions.asset_id
					AND active_job.job_kind = ?
					AND active_job.root_key = ?
					AND active_job.root_generation = ?
					AND active_job.status IN ('queued', 'running')
			)`,
		sourceKey,
		videoEnabled,
		localThumbnailJobKind,
		trustedRoot.root.Key,
		trustedRoot.rootGeneration,
	); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("reset failed local renditions to pending: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE local_assets
		SET thumbnail_status = 'pending',
			updated_at = ?
		WHERE source_key = ?
			AND (media_type = 'image' OR (? = 1 AND media_type = 'video'))
			AND visibility_status = 'active'
			AND thumbnail_status = 'failed'
			AND EXISTS (
				SELECT 1
				FROM local_scan_jobs active_job
				WHERE active_job.source_key = local_assets.source_key
					AND active_job.asset_id = local_assets.asset_id
					AND active_job.job_kind = ?
					AND active_job.root_key = ?
					AND active_job.root_generation = ?
					AND active_job.status IN ('queued', 'running')
			)`,
		nowText,
		sourceKey,
		videoEnabled,
		localThumbnailJobKind,
		trustedRoot.root.Key,
		trustedRoot.rootGeneration,
	)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("reset failed local thumbnail assets to pending: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("read requeued failed local thumbnail rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit failed local thumbnail requeue: %w", err)
	}
	return int(affected), nil
}

func (s *Service) nextLocalThumbnailJobs(ctx context.Context, sourceKey string, limit int) ([]localThumbnailJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	trustedRoots, err := s.trustedLocalMediaRootReferences(ctx, sourceKey)
	if err != nil {
		return nil, err
	}
	if len(trustedRoots) == 0 {
		return nil, nil
	}
	db := s.catalog.queryDB()
	jobs := make([]localThumbnailJob, 0, min(limit*len(trustedRoots), limit+len(trustedRoots)))
	for _, trustedRoot := range trustedRoots {
		if trustedRoot.reconciliationPending {
			continue
		}
		rows, err := db.QueryContext(ctx, `SELECT
				j.id,
				j.source_key,
				COALESCE(j.root_key, ''),
				j.root_generation,
				j.asset_id,
				COALESCE(a.media_type, ''),
				j.priority,
				j.sort_at,
				j.scheduled_at
			FROM local_scan_jobs AS j INDEXED BY idx_local_scan_jobs_source_thumbnail_ready
			JOIN local_scan_root_state rs
				ON rs.source_key = j.source_key
				AND rs.root_key = j.root_key
				AND rs.root_generation = j.root_generation
				AND rs.reconciliation_pending = 0
			LEFT JOIN local_assets a ON a.source_key = j.source_key AND a.asset_id = j.asset_id
			WHERE j.source_key = ?
				AND j.root_key = ?
				AND j.root_generation = ?
				AND j.job_kind = ?
				AND j.status = 'queued'
				AND j.asset_id IS NOT NULL
			ORDER BY j.priority ASC, j.sort_at DESC, j.scheduled_at ASC, j.id ASC
			LIMIT ?`,
			trustedRoot.sourceKey,
			trustedRoot.rootKey,
			trustedRoot.rootGeneration,
			localThumbnailJobKind,
			limit,
		)
		if err != nil {
			return nil, fmt.Errorf("query local thumbnail jobs for %s: %w", trustedRoot.sourceKey, err)
		}
		for rows.Next() {
			var job localThumbnailJob
			if err := rows.Scan(
				&job.ID,
				&job.SourceKey,
				&job.RootKey,
				&job.RootGeneration,
				&job.AssetID,
				&job.MediaType,
				&job.Priority,
				&job.SortAt,
				&job.ScheduledAt,
			); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan local thumbnail job: %w", err)
			}
			jobs = append(jobs, job)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate local thumbnail jobs: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close local thumbnail jobs: %w", err)
		}
	}
	sort.Slice(jobs, func(left int, right int) bool {
		if jobs[left].Priority != jobs[right].Priority {
			return jobs[left].Priority < jobs[right].Priority
		}
		if jobs[left].SortAt != jobs[right].SortAt {
			return jobs[left].SortAt > jobs[right].SortAt
		}
		if jobs[left].ScheduledAt != jobs[right].ScheduledAt {
			return jobs[left].ScheduledAt < jobs[right].ScheduledAt
		}
		return jobs[left].ID < jobs[right].ID
	})
	if len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, nil
}

func (s *Service) processLocalThumbnailJob(ctx context.Context, job localThumbnailJob) (bool, error) {
	nowText := formatCatalogTime(time.Now().UTC())
	trustedRoot, err := s.acquireTrustedLocalMediaRoot(ctx, job.SourceKey)
	if err != nil {
		return false, err
	}
	defer trustedRoot.Close()
	if !trustedRoot.matchesJobRoot(job.RootKey, job.RootGeneration) {
		return false, ErrLocalMediaRootNotTrusted
	}
	claimed, err := s.claimLocalThumbnailJob(ctx, job, trustedRoot, nowText)
	if err != nil || !claimed {
		return false, err
	}
	asset, err := s.localThumbnailAsset(ctx, job.SourceKey, job.AssetID)
	if err != nil {
		if errors.Is(err, ErrAssetNotFound) {
			return false, s.completeLocalScanJob(ctx, job.ID, nowText)
		}
		return false, err
	}
	if !s.localThumbnailGenerationEnabled(asset.MediaType) || asset.VisibilityStatus != "active" {
		return false, s.completeLocalScanJob(ctx, job.ID, nowText)
	}
	if ready, err := s.localThumbnailRenditionsReady(ctx, asset); err != nil {
		return false, err
	} else if ready {
		return false, s.completeLocalThumbnailJob(ctx, job.ID, asset, nowText)
	}
	if err := s.generateLocalThumbnailRenditions(ctx, asset, trustedRoot); err != nil {
		return false, err
	}
	return true, s.completeLocalThumbnailJob(ctx, job.ID, asset, nowText)
}

func (s *Service) claimLocalThumbnailJob(ctx context.Context, job localThumbnailJob, trustedRoot *trustedLocalMediaRoot, nowText string) (bool, error) {
	if !trustedRoot.matchesJobRoot(job.RootKey, job.RootGeneration) {
		return false, nil
	}
	result, err := s.catalog.db.ExecContext(ctx, `UPDATE local_scan_jobs
		SET status = 'running',
			attempts = attempts + 1,
			locked_at = ?
		WHERE id = ?
			AND source_key = ?
			AND root_key = ?
			AND root_generation = ?
			AND job_kind = ?
			AND asset_id = ?
			AND status = 'queued'
			AND EXISTS (
				SELECT 1
				FROM local_scan_root_state rs
				WHERE rs.source_key = local_scan_jobs.source_key
					AND rs.root_key = local_scan_jobs.root_key
					AND rs.root_generation = local_scan_jobs.root_generation
					AND rs.reconciliation_pending = 0
			)`,
		nowText,
		job.ID,
		job.SourceKey,
		job.RootKey,
		normalizeLocalMediaRootGeneration(job.RootGeneration),
		localThumbnailJobKind,
		job.AssetID,
	)
	if err != nil {
		return false, fmt.Errorf("claim local thumbnail job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read local thumbnail claim rows: %w", err)
	}
	return affected > 0, nil
}

func (s *Service) localThumbnailAsset(ctx context.Context, sourceKey string, assetID string) (localThumbnailAsset, error) {
	var asset localThumbnailAsset
	err := s.catalog.db.QueryRowContext(ctx, `SELECT source_key, asset_id, sha1_hex, media_type, visibility_status, thumbnail_status
		FROM local_assets
		WHERE source_key = ? AND asset_id = ?`, sourceKey, assetID).
		Scan(&asset.SourceKey, &asset.AssetID, &asset.SHA1Hex, &asset.MediaType, &asset.VisibilityStatus, &asset.ThumbnailStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return localThumbnailAsset{}, ErrAssetNotFound
		}
		return localThumbnailAsset{}, fmt.Errorf("read local thumbnail asset: %w", err)
	}
	return asset, nil
}

func (s *Service) localThumbnailRenditionsReady(ctx context.Context, asset localThumbnailAsset) (bool, error) {
	var count int
	err := s.catalog.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM local_renditions
		WHERE source_key = ?
			AND asset_id = ?
			AND kind IN (?, ?)
			AND status = 'ready'
			AND source_sha1_hex = ?
			AND relative_path IS NOT NULL`,
		asset.SourceKey,
		asset.AssetID,
		localRenditionKindPreview,
		localRenditionKindDetailPreview,
		asset.SHA1Hex,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("count local thumbnail renditions: %w", err)
	}
	return count == 2, nil
}

func (s *Service) generateLocalThumbnailRenditions(ctx context.Context, asset localThumbnailAsset, trustedRoot *trustedLocalMediaRoot) error {
	if strings.TrimSpace(s.dataDir) == "" {
		return fmt.Errorf("local thumbnail data directory is not configured")
	}
	if !localThumbnailSupportsMediaType(asset.MediaType) {
		return fmt.Errorf("local thumbnail generation supports images and videos only")
	}
	source, location, infoBefore, err := s.localThumbnailSource(ctx, asset.SourceKey, asset.AssetID, trustedRoot)
	if err != nil {
		return err
	}
	if source.File != nil {
		defer source.File.Close()
	}
	if !localActiveLocationMatchesFileInfo(location, infoBefore) {
		if err := s.resettleLocalThumbnailSource(ctx, asset, location, infoBefore, "source_changed_before_thumbnail"); err != nil {
			return err
		}
		return errLocalThumbnailSourceChanged
	}
	if asset.MediaType == "video" {
		renditions, err := s.generateLocalVideoPosterRenditions(ctx, asset, source)
		if err != nil {
			return err
		}
		if err := s.verifyLocalThumbnailSourceAfterRender(ctx, asset, location, source, infoBefore); err != nil {
			return err
		}
		return s.persistLocalRenderedRenditions(ctx, asset, renditions)
	}

	preview, err := s.renderLocalRendition(ctx, asset, localRenditionKindPreview, source, hostedImageProfile{
		Name:          localRenditionKindPreview,
		MaxEdgePixels: previewMaxEdgePixels,
		MaxBytes:      previewMaxBytes,
		JPEGQualities: localPreviewJPEGQualities,
		ForceJPEG:     true,
	})
	if err != nil {
		return err
	}
	detailPreview, err := s.renderLocalRendition(ctx, asset, localRenditionKindDetailPreview, source, hostedImageProfile{
		Name:          localRenditionKindDetailPreview,
		MaxEdgePixels: detailPreviewMaxEdgePixels,
		MaxBytes:      detailPreviewMaxBytes,
		JPEGQualities: localDetailPreviewJPEGQualities,
		ForceJPEG:     true,
	})
	if err != nil {
		return err
	}
	if err := s.verifyLocalThumbnailSourceAfterRender(ctx, asset, location, source, infoBefore); err != nil {
		return err
	}
	return s.persistLocalRenderedRenditions(ctx, asset, []localRenderedRendition{preview, detailPreview})
}

func (s *Service) generateLocalVideoPosterRenditions(ctx context.Context, asset localThumbnailAsset, source localMediaInput) ([]localRenderedRendition, error) {
	tempRoot, err := s.localStateChildPath(filepath.ToSlash(filepath.Join("tmp", "local-video-posters")))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(tempRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create local video poster temp directory: %w", err)
	}
	tempDir, err := os.MkdirTemp(tempRoot, "media-helper-*")
	if err != nil {
		return nil, fmt.Errorf("create local video poster temp directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	posterPath := filepath.Join(tempDir, "poster.jpg")
	if err := s.extractLocalVideoPoster(ctx, source, posterPath); err != nil {
		return nil, err
	}
	poster := localMediaInput{Path: posterPath}
	preview, err := s.renderLocalRendition(ctx, asset, localRenditionKindPreview, poster, hostedImageProfile{
		Name:          localRenditionKindPreview,
		MaxEdgePixels: previewMaxEdgePixels,
		MaxBytes:      previewMaxBytes,
		JPEGQualities: localPreviewJPEGQualities,
		ForceJPEG:     true,
	})
	if err != nil {
		return nil, err
	}
	detailPreview, err := s.renderLocalRendition(ctx, asset, localRenditionKindDetailPreview, poster, hostedImageProfile{
		Name:          localRenditionKindDetailPreview,
		MaxEdgePixels: detailPreviewMaxEdgePixels,
		MaxBytes:      detailPreviewMaxBytes,
		JPEGQualities: localDetailPreviewJPEGQualities,
		ForceJPEG:     true,
	})
	if err != nil {
		return nil, err
	}
	return []localRenderedRendition{preview, detailPreview}, nil
}

func (s *Service) extractLocalVideoPoster(ctx context.Context, source localMediaInput, outputPath string) error {
	return s.extractLocalVideoPosterWithMediaHelper(ctx, source, outputPath)
}

type localMediaHelperOperationResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	OK            bool   `json:"ok"`
	Operation     string `json:"operation"`
}

func (s *Service) extractLocalVideoPosterWithMediaHelper(ctx context.Context, source localMediaInput, outputPath string) error {
	helperPath := strings.TrimSpace(s.mediaHelperPath)
	if helperPath == "" {
		return fmt.Errorf("extract local video poster: media helper is not configured")
	}
	output, err := runLocalMediaHelperCommandWithInputFileContext(ctx, localMediaFFmpegPosterTimeout, helperPath, s.mediaVipsPath, s.mediaFFmpegPath, source.File,
		"render-video-poster",
		"--input", source.helperPath(),
		"--output", outputPath,
	)
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("extract local video poster with media helper: %s", message)
	}
	var response localMediaHelperOperationResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return fmt.Errorf("parse media helper poster response: %w", err)
	}
	if response.SchemaVersion != 1 || !response.OK || response.Operation != "render-video-poster" {
		return fmt.Errorf("media helper poster response is invalid")
	}
	body, err := os.ReadFile(outputPath)
	if err != nil {
		return fmt.Errorf("read local video poster output: %w", err)
	}
	if len(body) == 0 {
		return fmt.Errorf("extract local video poster: media helper produced an empty poster")
	}
	return nil
}

func (s *Service) persistLocalRenderedRenditions(ctx context.Context, asset localThumbnailAsset, renditions []localRenderedRendition) error {
	if len(renditions) == 0 {
		return fmt.Errorf("persist local thumbnail renditions: no renditions generated")
	}
	for _, rendition := range renditions {
		fullPath, err := s.localStateChildPath(rendition.RelativePath)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(fullPath, rendition.Bytes, 0o644); err != nil {
			return fmt.Errorf("write local %s rendition: %w", rendition.Kind, err)
		}
	}
	nowText := formatCatalogTime(time.Now().UTC())
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin local thumbnail registration: %w", err)
	}
	for _, rendition := range renditions {
		digest := sha256.Sum256(rendition.Bytes)
		if _, err := tx.ExecContext(ctx, `INSERT INTO local_renditions (
				source_key, asset_id, kind, status, relative_path, width, height,
				size_bytes, content_sha256, generated_at, source_sha1_hex, last_error
			) VALUES (?, ?, ?, 'ready', ?, ?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT(source_key, asset_id, kind) DO UPDATE SET
				status = 'ready',
				relative_path = excluded.relative_path,
				width = excluded.width,
				height = excluded.height,
				size_bytes = excluded.size_bytes,
				content_sha256 = excluded.content_sha256,
				generated_at = excluded.generated_at,
				source_sha1_hex = excluded.source_sha1_hex,
				last_error = NULL`,
			asset.SourceKey,
			asset.AssetID,
			rendition.Kind,
			rendition.RelativePath,
			rendition.Width,
			rendition.Height,
			len(rendition.Bytes),
			hex.EncodeToString(digest[:]),
			nowText,
			asset.SHA1Hex,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("upsert local rendition: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_assets
		SET thumbnail_status = 'ready',
			updated_at = ?
		WHERE source_key = ? AND asset_id = ?`, nowText, asset.SourceKey, asset.AssetID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("mark local thumbnail ready: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit local thumbnail registration: %w", err)
	}
	return nil
}

func (s *Service) renderLocalRendition(ctx context.Context, asset localThumbnailAsset, kind string, source localMediaInput, profile hostedImageProfile) (localRenderedRendition, error) {
	return s.renderLocalRenditionWithMediaHelper(ctx, asset, kind, source, profile)
}

func (s *Service) renderLocalRenditionWithMediaHelper(ctx context.Context, asset localThumbnailAsset, kind string, source localMediaInput, profile hostedImageProfile) (localRenderedRendition, error) {
	helperPath := strings.TrimSpace(s.mediaHelperPath)
	if helperPath == "" {
		return localRenderedRendition{}, fmt.Errorf("render local %s rendition: media helper is not configured", kind)
	}
	tempRoot, err := s.localStateChildPath(filepath.ToSlash(filepath.Join("tmp", "local-thumbnails")))
	if err != nil {
		return localRenderedRendition{}, err
	}
	if err := os.MkdirAll(tempRoot, 0o755); err != nil {
		return localRenderedRendition{}, fmt.Errorf("create local thumbnail temp directory: %w", err)
	}
	tempDir, err := os.MkdirTemp(tempRoot, "media-helper-*")
	if err != nil {
		return localRenderedRendition{}, fmt.Errorf("create local thumbnail temp directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	qualities := profile.JPEGQualities
	if len(qualities) == 0 {
		qualities = localDetailPreviewJPEGQualities
	}
	if profile.MaxEdgePixels <= 0 || profile.MaxBytes <= 0 {
		return localRenderedRendition{}, fmt.Errorf("render local %s rendition: invalid size profile", kind)
	}

	var oversized localRenderedRendition
	for _, quality := range qualities {
		rendition, err := s.renderLocalRenditionAttemptWithMediaHelper(ctx, asset, kind, source, tempDir, profile.MaxEdgePixels, quality)
		if err != nil {
			return localRenderedRendition{}, err
		}
		if len(rendition.Bytes) > profile.MaxBytes {
			oversized = rendition
			continue
		}
		return rendition, nil
	}
	if len(oversized.Bytes) == 0 {
		return localRenderedRendition{}, fmt.Errorf("render local %s rendition with media helper: no jpeg qualities configured", kind)
	}

	quality := qualities[len(qualities)-1]
	for attempt := 0; attempt < localRenditionResizeFallbackAttempts; attempt++ {
		maxEdge := nextLocalRenditionMaxEdge(oversized.Width, oversized.Height, len(oversized.Bytes), profile.MaxBytes)
		if maxEdge == 0 {
			break
		}
		log.Printf(
			"timich-agent local rendition size fallback source_key=%s asset_id=%s kind=%s quality=%d max_edge=%d previous_bytes=%d max_bytes=%d",
			asset.SourceKey,
			asset.AssetID,
			kind,
			quality,
			maxEdge,
			len(oversized.Bytes),
			profile.MaxBytes,
		)
		rendition, err := s.renderLocalRenditionAttemptWithMediaHelper(ctx, asset, kind, source, tempDir, maxEdge, quality)
		if err != nil {
			return localRenderedRendition{}, err
		}
		if len(rendition.Bytes) <= profile.MaxBytes {
			return rendition, nil
		}
		oversized = rendition
	}
	return localRenderedRendition{}, fmt.Errorf("render local %s rendition with media helper: %w", kind, ErrMediaTooLarge)
}

func (s *Service) renderLocalRenditionAttemptWithMediaHelper(
	ctx context.Context,
	asset localThumbnailAsset,
	kind string,
	source localMediaInput,
	tempDir string,
	maxEdge int,
	quality int,
) (localRenderedRendition, error) {
	outputPath := filepath.Join(tempDir, kind+"-"+strconv.Itoa(maxEdge)+"-"+strconv.Itoa(quality)+".jpg")
	output, err := runLocalMediaHelperCommandWithInputFileContext(ctx, localMediaVipsRenderTimeout, strings.TrimSpace(s.mediaHelperPath), s.mediaVipsPath, s.mediaFFmpegPath, source.File,
		"render-image",
		"--input", source.helperPath(),
		"--output", outputPath,
		"--max-edge", strconv.Itoa(maxEdge),
		"--quality", strconv.Itoa(quality),
	)
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return localRenderedRendition{}, fmt.Errorf("render local %s rendition with media helper: %s", kind, message)
	}
	var response localMediaHelperOperationResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return localRenderedRendition{}, fmt.Errorf("parse media helper image response: %w", err)
	}
	if response.SchemaVersion != 1 || !response.OK || response.Operation != "render-image" {
		return localRenderedRendition{}, fmt.Errorf("media helper image response is invalid")
	}
	body, err := os.ReadFile(outputPath)
	if err != nil {
		return localRenderedRendition{}, fmt.Errorf("read local %s media helper output: %w", kind, err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		return localRenderedRendition{}, fmt.Errorf("decode local %s media helper output config: %w", kind, err)
	}
	if !strings.EqualFold(format, "jpeg") && !strings.EqualFold(format, "jpg") {
		return localRenderedRendition{}, fmt.Errorf("render local %s rendition with media helper: output format %q is not jpeg", kind, format)
	}
	if config.Width <= 0 || config.Height <= 0 {
		return localRenderedRendition{}, fmt.Errorf("render local %s rendition with media helper: invalid output dimensions", kind)
	}
	return localRenderedRendition{
		Kind:         kind,
		RelativePath: localRenditionRelativePath(asset.SourceKey, asset.AssetID, kind),
		Bytes:        body,
		Width:        config.Width,
		Height:       config.Height,
	}, nil
}

func nextLocalRenditionMaxEdge(width int, height int, sizeBytes int, maxBytes int) int {
	currentEdge := max(width, height)
	if currentEdge <= localRenditionResizeQuantumPixels || sizeBytes <= 0 || sizeBytes <= maxBytes || maxBytes <= 0 {
		return 0
	}
	scale := math.Sqrt(float64(maxBytes)/float64(sizeBytes)) * localRenditionResizeSafetyFactor
	nextEdge := int(math.Floor(float64(currentEdge) * scale))
	nextEdge -= nextEdge % localRenditionResizeQuantumPixels
	if nextEdge < localRenditionResizeQuantumPixels {
		nextEdge = localRenditionResizeQuantumPixels
	}
	if nextEdge >= currentEdge {
		nextEdge = currentEdge - localRenditionResizeQuantumPixels
		nextEdge -= nextEdge % localRenditionResizeQuantumPixels
	}
	if nextEdge < localRenditionResizeQuantumPixels || nextEdge >= currentEdge {
		return 0
	}
	return nextEdge
}

func (s *Service) localThumbnailSource(ctx context.Context, sourceKey string, assetID string, trustedRoot *trustedLocalMediaRoot) (localMediaInput, localActiveLocation, os.FileInfo, error) {
	if trustedRoot == nil || trustedRoot.handle == nil || trustedRoot.datasource.SourceKey != sourceKey {
		return localMediaInput{}, localActiveLocation{}, nil, ErrLocalMediaRootNotTrusted
	}
	location, err := s.localActiveLocation(ctx, sourceKey, trustedRoot.root.Key, assetID)
	if err != nil {
		return localMediaInput{}, localActiveLocation{}, nil, err
	}
	if location.RootKey != trustedRoot.root.Key {
		return localMediaInput{}, localActiveLocation{}, nil, ErrAssetNotFound
	}
	file, info, err := openLocalRootFileFromPinnedRoot(trustedRoot.handle, location.RelativePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errLocalRootChildUnavailable) {
			return localMediaInput{}, localActiveLocation{}, nil, ErrAssetNotFound
		}
		return localMediaInput{}, localActiveLocation{}, nil, err
	}
	return localMediaInput{File: file, Root: trustedRoot.handle, RelativePath: location.RelativePath}, location, info, nil
}

func localActiveLocationMatchesFileInfo(location localActiveLocation, info os.FileInfo) bool {
	if info == nil {
		return false
	}
	mtimeText := formatCatalogTime(info.ModTime().UTC())
	return location.SizeBytes == info.Size() &&
		location.MTime == mtimeText &&
		location.FastSignature == fmt.Sprintf("%d:%s", info.Size(), mtimeText) &&
		location.FileIdentity == localFileIdentity(info)
}

func (s *Service) verifyLocalThumbnailSourceAfterRender(ctx context.Context, asset localThumbnailAsset, location localActiveLocation, source localMediaInput, before os.FileInfo) error {
	after, err := source.stat()
	if err != nil {
		return err
	}
	pathFile, pathInfo, pathnameMatches, err := reopenPinnedLocalRootFileAndMatch(source.Root, source.RelativePath, after)
	if err != nil {
		return err
	}
	defer pathFile.Close()
	if localFileInfoUnchanged(before, after) && pathnameMatches && localActiveLocationMatchesFileInfo(location, pathInfo) {
		return nil
	}
	if err := s.resettleLocalThumbnailSource(ctx, asset, location, pathInfo, "source_changed_during_thumbnail"); err != nil {
		return err
	}
	return errLocalThumbnailSourceChanged
}

func (s *Service) resettleLocalThumbnailSource(ctx context.Context, asset localThumbnailAsset, location localActiveLocation, info os.FileInfo, reason string) error {
	return s.resettleLocalSource(ctx, asset, location, info, reason, true)
}

func (s *Service) resettleLocalOriginalSource(ctx context.Context, asset localThumbnailAsset, location localActiveLocation, info os.FileInfo, reason string) error {
	return s.resettleLocalSource(ctx, asset, location, info, reason, false)
}

func (s *Service) resettleLocalSource(ctx context.Context, asset localThumbnailAsset, location localActiveLocation, info os.FileInfo, reason string, invalidateThumbnail bool) error {
	if info == nil || !info.Mode().IsRegular() {
		return fmt.Errorf("resettle local source: path is not a regular file")
	}
	datasource, _, err := s.localDatasourceAndRoot(location.SourceKey)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	nowText := formatCatalogTime(now)
	mtimeText := formatCatalogTime(info.ModTime().UTC())
	fastSignature := fmt.Sprintf("%d:%s", info.Size(), mtimeText)
	fileIdentity := localFileIdentity(info)
	notBeforeText := formatCatalogTime(now.Add(localDatasourceSettlingDuration(*datasource)))
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin local source resettle: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_asset_locations
		SET asset_id = NULL,
			size_bytes = ?,
			mtime = ?,
			fast_signature = ?,
			file_identity = ?,
			sha1_hex = NULL,
			status = 'discovered',
			status_reason = ?,
			last_seen_at = ?,
			verified_at = NULL,
			content_verified_at = NULL,
			content_verification_attempted_at = NULL,
			content_verification_error = NULL,
			metadata_not_before = ?,
			superseded_at = NULL,
			updated_at = ?
		WHERE id = ?`, info.Size(), mtimeText, fastSignature, fileIdentity, reason, nowText, notBeforeText, nowText, location.ID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("resettle local source location: %w", err)
	}
	if invalidateThumbnail {
		if _, err := tx.ExecContext(ctx, `DELETE FROM local_scan_jobs
			WHERE source_key = ? AND asset_id = ? AND job_kind = ? AND status IN ('queued', 'running')`,
			asset.SourceKey, asset.AssetID, localThumbnailJobKind); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("complete stale local thumbnail jobs: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, location_id, status, scheduled_at, sort_at
		)
		SELECT ?, ?, ?, ?, rs.root_generation, ?, 'queued', ?, ?
		FROM local_scan_root_state rs
		WHERE rs.source_key = ? AND rs.root_key = ?
			AND rs.root_generation > 0
			AND NOT EXISTS (
				SELECT 1 FROM local_scan_jobs
				WHERE source_key = ? AND job_kind = ? AND location_id = ? AND status IN ('queued', 'running')
			)`, asset.SourceKey, localMetadataJobKind, localMetadataBackgroundPriority, location.RootKey, location.ID, notBeforeText, mtimeText,
		asset.SourceKey, location.RootKey,
		asset.SourceKey, localMetadataJobKind, location.ID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("queue resettled local metadata job: %w", err)
	}
	if invalidateThumbnail {
		if _, err := tx.ExecContext(ctx, `UPDATE local_assets SET thumbnail_status = 'pending', updated_at = ?
			WHERE source_key = ? AND asset_id = ?`, nowText, asset.SourceKey, asset.AssetID); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("reset local thumbnail asset: %w", err)
		}
	}
	changedCanonicalIDs, err := refreshLocalAssetVisibilityInTx(ctx, tx, asset.SourceKey, location.RootKey, nowText)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.catalog.rebuildCatalogCanonicalIDsInTx(ctx, tx, changedCanonicalIDs, nowText); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit local source resettle: %w", err)
	}
	return nil
}

func (s *Service) completeLocalThumbnailJob(ctx context.Context, jobID int64, asset localThumbnailAsset, nowText string) error {
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin local thumbnail job completion: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_assets
		SET thumbnail_status = 'ready',
			updated_at = ?
		WHERE source_key = ? AND asset_id = ?`, nowText, asset.SourceKey, asset.AssetID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("mark local thumbnail asset ready: %w", err)
	}
	if err := completeLocalScanJobInTx(ctx, tx, jobID, nowText); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := completeLocalThumbnailJobsForAssetInTx(ctx, tx, asset.SourceKey, asset.AssetID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit local thumbnail job completion: %w", err)
	}
	return nil
}

func (s *Service) failLocalThumbnailJob(ctx context.Context, job localThumbnailJob, jobErr error) (bool, error) {
	trustedRoot, err := s.acquireTrustedLocalMediaRoot(ctx, job.SourceKey)
	if err != nil {
		if errors.Is(err, ErrLocalMediaRootNotTrusted) || errors.Is(err, ErrNoDatasourceConfigured) {
			_, recoveryErr := s.deferClaimedLocalThumbnailJob(job)
			return recoveryErr == nil, recoveryErr
		}
		return false, err
	}
	defer trustedRoot.Close()
	if !trustedRoot.matchesJobRoot(job.RootKey, job.RootGeneration) {
		_, recoveryErr := s.deferClaimedLocalThumbnailJob(job)
		return recoveryErr == nil, recoveryErr
	}
	nowText := formatCatalogTime(time.Now().UTC())
	message := ""
	if jobErr != nil {
		message = jobErr.Error()
	}
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin local thumbnail job failure: %w", err)
	}
	failedJob, err := tx.ExecContext(ctx, `UPDATE local_scan_jobs
		SET status = 'failed',
			completed_at = ?,
			last_error = ?
		WHERE id = ?
			AND source_key = ?
			AND root_key = ?
			AND root_generation = ?
			AND asset_id = ?
			AND job_kind = ?
			AND status = 'running'
			AND EXISTS (
				SELECT 1
				FROM local_scan_root_state rs
				WHERE rs.source_key = local_scan_jobs.source_key
					AND rs.root_key = local_scan_jobs.root_key
					AND rs.root_generation = local_scan_jobs.root_generation
			)`,
		nowText,
		nullableCatalogText(message),
		job.ID,
		job.SourceKey,
		job.RootKey,
		normalizeLocalMediaRootGeneration(job.RootGeneration),
		job.AssetID,
		localThumbnailJobKind,
	)
	if err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("fail current-generation local thumbnail job: %w", err)
	}
	affected, err := failedJob.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("read failed current-generation local thumbnail job: %w", err)
	}
	if affected == 0 {
		_ = tx.Rollback()
		return false, nil
	}
	for _, kind := range []string{localRenditionKindPreview, localRenditionKindDetailPreview} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO local_renditions (
				source_key, asset_id, kind, status, last_error
			) VALUES (?, ?, ?, 'failed', ?)
			ON CONFLICT(source_key, asset_id, kind) DO UPDATE SET
				status = 'failed',
				relative_path = NULL,
				width = NULL,
				height = NULL,
				size_bytes = NULL,
				content_sha256 = NULL,
				generated_at = NULL,
				source_sha1_hex = NULL,
				last_error = excluded.last_error`,
			job.SourceKey,
			job.AssetID,
			kind,
			nullableCatalogText(message),
		); err != nil {
			_ = tx.Rollback()
			return false, fmt.Errorf("mark local rendition failed: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_assets
		SET thumbnail_status = 'failed',
			updated_at = ?
		WHERE source_key = ? AND asset_id = ?`, nowText, job.SourceKey, job.AssetID); err != nil {
		_ = tx.Rollback()
		return false, fmt.Errorf("mark local thumbnail asset failed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit local thumbnail job failure: %w", err)
	}
	return false, nil
}

func (s *Service) localRenditionMediaResponse(clientRequest *http.Request, datasource *config.DatasourceConfig, assetID string, kind string) (*UpstreamMediaResponse, error) {
	if datasource == nil {
		return nil, ErrNoDatasourceConfigured
	}
	ctx := contextFromRequest(clientRequest)
	rendition, err := s.localReadyRenditionRecord(ctx, datasource.SourceKey, assetID, kind)
	if err != nil {
		return nil, err
	}
	file, info, err := s.openVerifiedLocalRendition(ctx, rendition)
	if err != nil {
		if errors.Is(err, errLocalRenditionInvalid) {
			if repairErr := s.markLocalRenditionsPending(ctx, datasource.SourceKey, assetID, err); repairErr != nil {
				return nil, errors.Join(ErrAssetNotFound, repairErr)
			}
			return nil, ErrAssetNotFound
		}
		return nil, fmt.Errorf("inspect local rendition: %w", err)
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()
	header := http.Header{}
	header.Set("Cache-Control", "private, max-age=3600")
	header.Set("Content-Type", "image/jpeg")
	header.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	header.Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	header.Set("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape(localRenditionFileName(kind)))
	if clientRequest != nil && clientRequest.Method == http.MethodHead {
		return &UpstreamMediaResponse{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil
	}
	closeFile = false
	return &UpstreamMediaResponse{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       file,
	}, nil
}

func completeLocalThumbnailJobsForAssetInTx(ctx context.Context, tx *sql.Tx, sourceKey string, assetID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_scan_jobs
		WHERE source_key = ?
			AND asset_id = ?
			AND job_kind = ?
			AND status IN ('queued', 'running', 'failed')`,
		sourceKey,
		assetID,
		localThumbnailJobKind,
	); err != nil {
		return fmt.Errorf("complete local thumbnail jobs for asset: %w", err)
	}
	return nil
}

var errLocalRenditionInvalid = errors.New("local rendition is missing or corrupt")

func (s *Service) localReadyRenditionRecord(ctx context.Context, sourceKey string, assetID string, kind string) (localReadyRendition, error) {
	var rendition localReadyRendition
	err := s.catalog.db.QueryRowContext(ctx, `SELECT r.kind, r.relative_path,
			COALESCE(r.size_bytes, -1), COALESCE(r.content_sha256, '')
			FROM local_renditions r
		LEFT JOIN local_assets a
			ON a.source_key = r.source_key AND a.asset_id = r.asset_id
		WHERE r.source_key = ?
			AND r.asset_id = ?
			AND r.kind = ?
			AND r.status = 'ready'
			AND r.relative_path IS NOT NULL
			AND trim(r.relative_path) <> ''
			AND r.source_sha1_hex = a.sha1_hex
			AND a.visibility_status = 'active'
			LIMIT 1`, sourceKey, assetID, kind).Scan(&rendition.Kind, &rendition.RelativePath, &rendition.SizeBytes, &rendition.SHA256)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return localReadyRendition{}, ErrAssetNotFound
		}
		return localReadyRendition{}, fmt.Errorf("read local rendition path: %w", err)
	}
	return rendition, nil
}

func (s *Service) openVerifiedLocalRendition(ctx context.Context, rendition localReadyRendition) (*os.File, os.FileInfo, error) {
	root := s.localStateRootPath()
	if root == "" || strings.TrimSpace(rendition.RelativePath) == "" || rendition.SizeBytes < 0 || len(strings.TrimSpace(rendition.SHA256)) != sha256.Size*2 {
		return nil, nil, errLocalRenditionInvalid
	}
	file, info, err := openLocalRootFile(root, rendition.RelativePath)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errLocalRenditionInvalid, err)
	}
	if info.Size() != rendition.SizeBytes {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: size mismatch", errLocalRenditionInvalid)
	}
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, nil, err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = file.Close()
			return nil, nil, fmt.Errorf("%w: read failed: %v", errLocalRenditionInvalid, readErr)
		}
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), strings.TrimSpace(rendition.SHA256)) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: digest mismatch", errLocalRenditionInvalid)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: rewind failed: %v", errLocalRenditionInvalid, err)
	}
	return file, info, nil
}

func (s *Service) markLocalRenditionsPending(ctx context.Context, sourceKey string, assetID string, cause error) error {
	nowText := formatCatalogTime(time.Now().UTC())
	message := "local rendition requires regeneration"
	if cause != nil {
		message = cause.Error()
	}
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin local rendition repair: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_renditions
		SET status = 'pending', relative_path = NULL, width = NULL, height = NULL,
			size_bytes = NULL, content_sha256 = NULL, generated_at = NULL,
			source_sha1_hex = NULL, last_error = ?
		WHERE source_key = ? AND asset_id = ?`, message, sourceKey, assetID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("mark local renditions pending: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_assets
		SET thumbnail_status = 'pending', updated_at = ?
		WHERE source_key = ? AND asset_id = ?`, nowText, sourceKey, assetID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("mark local thumbnail pending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit local rendition repair: %w", err)
	}
	s.notifyLocalWorkQueued()
	return nil
}

func (s *Service) localStateChildPath(relativePath string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("catalog state directory is not configured")
	}
	root := s.localStateRootPath()
	if root == "" {
		return "", fmt.Errorf("catalog state directory is not configured")
	}
	return localRootChildPath(root, relativePath)
}

func (s *Service) localStateRootPath() string {
	root := ""
	if s != nil && s.catalog != nil {
		root = strings.TrimSpace(s.catalog.root)
	}
	if root == "" && s != nil && strings.TrimSpace(s.dataDir) != "" {
		root = filepath.Join(strings.TrimSpace(s.dataDir), catalogStateDirName)
	}
	return root
}

func localThumbnailSupportsMediaType(mediaType string) bool {
	switch strings.TrimSpace(mediaType) {
	case "image", "video":
		return true
	default:
		return false
	}
}

func (s *Service) localThumbnailGenerationEnabled(mediaType string) bool {
	switch strings.TrimSpace(mediaType) {
	case "image":
		return s.localImageRenditionEnabled()
	case "video":
		return s.localVideoPosterEnabled()
	default:
		return false
	}
}

func (s *Service) localImageRenditionEnabled() bool {
	return s != nil && strings.TrimSpace(s.mediaHelperPath) != ""
}

func (s *Service) localVideoPosterEnabled() bool {
	return s != nil && strings.TrimSpace(s.mediaHelperPath) != ""
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func localRenditionRelativePath(sourceKey string, assetID string, kind string) string {
	return filepath.ToSlash(filepath.Join(localRenditionsDirName, sourceKey, assetID, kind+".jpg"))
}

func localRenditionFileName(kind string) string {
	switch strings.TrimSpace(kind) {
	case localRenditionKindDetailPreview:
		return "detail-preview.jpg"
	default:
		return "preview.jpg"
	}
}

func effectiveFirstViewThumbnailCount(datasource config.DatasourceConfig) int {
	if datasource.Scan == nil || datasource.Scan.FirstViewThumbnailCount == 0 {
		return defaultFirstViewThumbnailCount
	}
	return datasource.Scan.FirstViewThumbnailCount
}

func resolveMediaVipsPath(configuredPath string) (string, bool) {
	configuredPath = strings.TrimSpace(configuredPath)
	if configuredPath != "" {
		return configuredPath, false
	}
	path, err := exec.LookPath("vips")
	if err != nil {
		return "", false
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return path, true
	}
	return absolutePath, true
}

func resolveMediaFFmpegPath(configuredPath string) (string, bool) {
	configuredPath = strings.TrimSpace(configuredPath)
	if configuredPath != "" {
		return configuredPath, false
	}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", false
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return path, true
	}
	return absolutePath, true
}

func localMediaVipsCommandEnv(base []string, vipsPath string) []string {
	bundleRoot := localMediaVipsBundleRoot(vipsPath)
	if bundleRoot == "" {
		return base
	}
	env := append([]string{}, base...)
	binDir := filepath.Join(bundleRoot, "bin")
	libDir := filepath.Join(bundleRoot, "lib")
	shareDir := filepath.Join(bundleRoot, "share")
	env = prependPathEnv(env, "PATH", binDir)
	env = prependPathEnv(env, "LD_LIBRARY_PATH", libDir)
	env = prependPathEnv(env, "DYLD_LIBRARY_PATH", libDir)
	env = prependPathEnv(env, "DYLD_FALLBACK_LIBRARY_PATH", libDir)
	env = prependPathEnv(env, "GI_TYPELIB_PATH", filepath.Join(libDir, "girepository-1.0"))
	env = prependPathEnv(env, "XDG_DATA_DIRS", shareDir)
	for _, modulesDir := range localMediaVipsModuleDirs(libDir) {
		env = prependPathEnv(env, "VIPS_MODULE_PATH", modulesDir)
	}
	return env
}

func localMediaFFmpegCommandEnv(base []string, ffmpegPath string) []string {
	bundleRoot := localMediaFFmpegBundleRoot(ffmpegPath)
	if bundleRoot == "" {
		return base
	}
	env := append([]string{}, base...)
	libDir := filepath.Join(bundleRoot, "lib")
	env = prependPathEnv(env, "PATH", filepath.Join(bundleRoot, "bin"))
	env = prependPathEnv(env, "LD_LIBRARY_PATH", libDir)
	env = prependPathEnv(env, "DYLD_LIBRARY_PATH", libDir)
	env = prependPathEnv(env, "DYLD_FALLBACK_LIBRARY_PATH", libDir)
	return env
}

func localMediaVipsBundleRoot(vipsPath string) string {
	vipsPath = strings.TrimSpace(vipsPath)
	if vipsPath == "" {
		return ""
	}
	clean := filepath.Clean(vipsPath)
	if filepath.Base(clean) != mediaVipsBinaryName() {
		return ""
	}
	binDir := filepath.Dir(clean)
	if filepath.Base(binDir) != "bin" {
		return ""
	}
	root := filepath.Dir(binDir)
	if filepath.Base(root) != "libvips" {
		return ""
	}
	if filepath.Base(filepath.Dir(root)) != "media-runtime" {
		return ""
	}
	return root
}

func localMediaFFmpegBundleRoot(ffmpegPath string) string {
	ffmpegPath = strings.TrimSpace(ffmpegPath)
	if ffmpegPath == "" {
		return ""
	}
	clean := filepath.Clean(ffmpegPath)
	if filepath.Base(clean) != mediaFFmpegBinaryName() {
		return ""
	}
	binDir := filepath.Dir(clean)
	if filepath.Base(binDir) != "bin" {
		return ""
	}
	root := filepath.Dir(binDir)
	if filepath.Base(root) != "ffmpeg" {
		return ""
	}
	if filepath.Base(filepath.Dir(root)) != "media-runtime" {
		return ""
	}
	return root
}

func localMediaVipsModuleDirs(libDir string) []string {
	entries, err := os.ReadDir(libDir)
	if err != nil {
		return []string{}
	}
	dirs := []string{}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "vips-modules-") {
			continue
		}
		dirs = append(dirs, filepath.Join(libDir, entry.Name()))
	}
	return dirs
}

func prependPathEnv(env []string, key string, value string) []string {
	if strings.TrimSpace(value) == "" {
		return env
	}
	prefix := key + "="
	for index, item := range env {
		if !strings.HasPrefix(item, prefix) {
			continue
		}
		current := strings.TrimPrefix(item, prefix)
		if current == "" {
			env[index] = prefix + value
			return env
		}
		env[index] = prefix + value + string(os.PathListSeparator) + current
		return env
	}
	return append(env, prefix+value)
}

func mediaVipsBinaryName() string {
	if os.PathSeparator == '\\' {
		return "vips.exe"
	}
	return "vips"
}

func mediaFFmpegBinaryName() string {
	if os.PathSeparator == '\\' {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
}

func writeFileAtomic(path string, body []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(perm); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
