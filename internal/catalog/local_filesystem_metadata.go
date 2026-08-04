package catalog

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

const defaultLocalMetadataBatchSize = 100
const localMetadataSlowStepThreshold = 2 * time.Second
const localMetadataLargeFileLogThreshold = 100 * 1024 * 1024

var errLocalMetadataSourceChanged = errors.New("local metadata source changed")

type LocalMetadataBatchResult struct {
	ProcessedJobs    int       `json:"processedJobs"`
	CompletedJobs    int       `json:"completedJobs"`
	FailedJobs       int       `json:"failedJobs"`
	DeferredJobs     int       `json:"deferredJobs"`
	RegisteredAssets int       `json:"registeredAssets"`
	SettlingJobs     int       `json:"settlingJobs"`
	StartedAt        time.Time `json:"startedAt"`
	CompletedAt      time.Time `json:"completedAt"`
}

type LocalMetadataRequeueResult struct {
	Queued int `json:"queued"`
}

type localMetadataJob struct {
	ID             int64
	SourceKey      string
	RootKey        string
	RootGeneration int64
	LocationID     int64
	Priority       int
	SortAt         string
}

type localLocationForMetadata struct {
	ID            int64
	SourceKey     string
	RootKey       string
	RelativePath  string
	Status        string
	SizeBytes     int64
	MTime         string
	FastSignature string
	FileIdentity  string
}

func (s *Service) RunLocalMetadataBatch(ctx context.Context, maxJobs int) (LocalMetadataBatchResult, error) {
	return s.runLocalMetadataBatch(ctx, "", maxJobs, 1)
}

func (s *Service) RunLocalMetadataBatchForSource(ctx context.Context, sourceKey string, maxJobs int) (LocalMetadataBatchResult, error) {
	return s.runLocalMetadataBatch(ctx, sourceKey, maxJobs, 1)
}

func (s *Service) RunLocalMetadataBatchWithWorkers(ctx context.Context, maxJobs int, workers int) (LocalMetadataBatchResult, error) {
	return s.runLocalMetadataBatch(ctx, "", maxJobs, workers)
}

func (s *Service) RequeueFailedLocalMetadata(ctx context.Context) (LocalMetadataRequeueResult, error) {
	if s == nil || s.catalog == nil {
		return LocalMetadataRequeueResult{}, ErrNoDatasourceConfigured
	}
	if err := s.ensureStateWritesAvailable(); err != nil {
		return LocalMetadataRequeueResult{}, err
	}
	queued, err := s.requeueFailedLocalMetadataJobs(ctx)
	if err != nil {
		return LocalMetadataRequeueResult{}, err
	}
	return LocalMetadataRequeueResult{Queued: queued}, nil
}

func (s *Service) runLocalMetadataBatch(ctx context.Context, sourceKey string, maxJobs int, workers int) (LocalMetadataBatchResult, error) {
	if s == nil || s.catalog == nil {
		return LocalMetadataBatchResult{}, ErrNoDatasourceConfigured
	}
	if err := s.ensureStateWritesAvailable(); err != nil {
		return LocalMetadataBatchResult{}, err
	}
	if err := s.recoverRememberedLocalClaims(ctx); err != nil {
		return LocalMetadataBatchResult{}, err
	}
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey != "" {
		if _, _, err := s.localDatasourceAndRoot(sourceKey); err != nil {
			return LocalMetadataBatchResult{}, err
		}
	}
	if maxJobs <= 0 {
		maxJobs = defaultLocalMetadataBatchSize
	}
	workers = min(max(workers, 1), maxJobs)
	startedAt := time.Now().UTC()
	result := LocalMetadataBatchResult{StartedAt: startedAt}
	selectStarted := time.Now()
	jobs, err := s.nextLocalMetadataJobs(ctx, sourceKey, maxJobs)
	if err != nil {
		return LocalMetadataBatchResult{}, err
	}
	if elapsed := time.Since(selectStarted); elapsed > localMetadataSlowStepThreshold {
		log.Printf("timich-agent local metadata job select slow count=%d elapsed=%s", len(jobs), elapsed.Round(time.Millisecond))
	}
	jobsChannel := make(chan localMetadataJob)
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
				jobStarted := time.Now()
				processed, jobErr := s.processLocalMetadataJob(ctx, job)
				if elapsed := time.Since(jobStarted); elapsed > localMetadataSlowStepThreshold {
					log.Printf("timich-agent local metadata job slow job_id=%d location_id=%d processed=%t elapsed=%s", job.ID, job.LocationID, processed, elapsed.Round(time.Millisecond))
				}
				if ctx.Err() != nil {
					if _, recoveryErr := s.deferClaimedLocalMetadataJob(job); recoveryErr != nil {
						log.Printf("timich-agent local metadata canceled job recovery failed job_id=%d error=%v", job.ID, recoveryErr)
						s.rememberLocalMetadataClaimRecovery(job)
					}
					return
				}
				deferred := errors.Is(jobErr, ErrLocalMediaRootNotTrusted)
				if deferred {
					if _, recoveryErr := s.deferClaimedLocalMetadataJob(job); recoveryErr != nil {
						log.Printf("timich-agent local metadata deferred job recovery failed job_id=%d error=%v", job.ID, recoveryErr)
						s.rememberLocalMetadataClaimRecovery(job)
						resultMu.Lock()
						batchErr = errors.Join(batchErr, fmt.Errorf("defer local metadata job %d: %w", job.ID, recoveryErr))
						resultMu.Unlock()
					}
				} else if jobErr != nil && !errors.Is(jobErr, errLocalMetadataSourceChanged) {
					failureDeferred, failureErr := s.failLocalMetadataJob(ctx, job, jobErr)
					if failureErr != nil {
						recovered, recoveryErr := s.deferClaimedLocalMetadataJob(job)
						if recoveryErr != nil {
							log.Printf("timich-agent local metadata failure publication and claim recovery failed job_id=%d publication_error=%v recovery_error=%v", job.ID, failureErr, recoveryErr)
							s.rememberLocalMetadataClaimRecovery(job)
							resultMu.Lock()
							batchErr = errors.Join(batchErr, fmt.Errorf("recover local metadata job %d after failure publication: %w", job.ID, errors.Join(failureErr, recoveryErr)))
							resultMu.Unlock()
						} else if !recovered {
							log.Printf("timich-agent local metadata failure publication failed and claim was not recoverable job_id=%d error=%v", job.ID, failureErr)
							resultMu.Lock()
							batchErr = errors.Join(batchErr, fmt.Errorf("recover local metadata job %d after failure publication: exact running claim not found: %w", job.ID, failureErr))
							resultMu.Unlock()
						} else {
							log.Printf("timich-agent local metadata failure publication failed; exact claim deferred job_id=%d error=%v", job.ID, failureErr)
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
				case errors.Is(jobErr, errLocalMetadataSourceChanged):
					result.SettlingJobs++
					result.CompletedJobs++
				case jobErr != nil:
					result.FailedJobs++
				case processed:
					result.CompletedJobs++
					result.RegisteredAssets++
				default:
					result.CompletedJobs++
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

func (s *Service) nextLocalMetadataJobs(ctx context.Context, sourceKey string, limit int) ([]localMetadataJob, error) {
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
	nowText := formatCatalogTime(time.Now().UTC())
	jobs := make([]localMetadataJob, 0, min(limit*len(trustedRoots), limit+len(trustedRoots)))
	for _, trustedRoot := range trustedRoots {
		rows, err := db.QueryContext(ctx, `SELECT
					j.id,
					j.source_key,
					COALESCE(j.root_key, ''),
					j.root_generation,
					j.location_id,
					j.priority,
					j.sort_at
					FROM local_scan_jobs AS j INDEXED BY idx_local_scan_jobs_source_metadata_ready
					JOIN local_scan_root_state rs
						ON rs.source_key = j.source_key
						AND rs.root_key = j.root_key
						AND rs.root_generation = j.root_generation
					WHERE j.source_key = ?
						AND j.root_key = ?
						AND j.root_generation = ?
						AND j.job_kind = ?
						AND j.status = 'queued'
						AND j.scheduled_at <= ?
						AND j.location_id IS NOT NULL
						AND NOT EXISTS (
							SELECT 1 FROM local_asset_locations l
							WHERE l.id = j.location_id AND l.metadata_not_before > ?
						)
					ORDER BY j.priority ASC, j.sort_at DESC, j.id ASC
					LIMIT ?`,
			trustedRoot.sourceKey,
			trustedRoot.rootKey,
			trustedRoot.rootGeneration,
			localMetadataJobKind,
			nowText,
			nowText,
			limit,
		)
		if err != nil {
			return nil, fmt.Errorf("query local metadata jobs for %s: %w", trustedRoot.sourceKey, err)
		}
		for rows.Next() {
			var job localMetadataJob
			if err := rows.Scan(
				&job.ID,
				&job.SourceKey,
				&job.RootKey,
				&job.RootGeneration,
				&job.LocationID,
				&job.Priority,
				&job.SortAt,
			); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan local metadata job: %w", err)
			}
			jobs = append(jobs, job)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate local metadata jobs: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close local metadata jobs: %w", err)
		}
	}
	sort.Slice(jobs, func(left int, right int) bool {
		if jobs[left].Priority != jobs[right].Priority {
			return jobs[left].Priority < jobs[right].Priority
		}
		if jobs[left].SortAt != jobs[right].SortAt {
			return jobs[left].SortAt > jobs[right].SortAt
		}
		return jobs[left].ID < jobs[right].ID
	})
	if len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, nil
}

func (s *Service) requeueFailedLocalMetadataJobs(ctx context.Context) (int, error) {
	sourceKeys := s.LocalDatasourceSourceKeys()
	if len(sourceKeys) == 0 {
		return 0, nil
	}
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin failed local metadata requeue: %w", err)
	}
	sourcePlaceholders := strings.TrimRight(strings.Repeat("?,", len(sourceKeys)), ",")
	nowText := formatCatalogTime(time.Now().UTC())
	updateArgs := []any{localMetadataRepairPriority, nowText, nowText, nowText, localMetadataJobKind}
	for _, sourceKey := range sourceKeys {
		updateArgs = append(updateArgs, sourceKey)
	}
	updateArgs = append(updateArgs, localMetadataJobKind, localMetadataJobKind)
	result, err := tx.ExecContext(ctx, `UPDATE local_scan_jobs
		SET status = 'queued',
			priority = ?,
			attempts = 0,
			scheduled_at = MAX(?, COALESCE((
				SELECT l.metadata_not_before
				FROM local_asset_locations l
				WHERE l.id = local_scan_jobs.location_id
			), ?)),
			sort_at = COALESCE((
				SELECT l.mtime
				FROM local_asset_locations l
				WHERE l.id = local_scan_jobs.location_id
			), ?),
			locked_at = NULL,
			completed_at = NULL,
			last_error = NULL
		WHERE id IN (
			SELECT COALESCE(
				MAX(CASE WHEN j.status = 'queued' THEN j.id END),
				MAX(CASE WHEN j.status = 'failed' THEN j.id END)
			)
			FROM local_scan_jobs j
			JOIN local_asset_locations l ON l.id = j.location_id
			JOIN local_scan_root_state rs
				ON rs.source_key = j.source_key
				AND rs.root_key = l.root_key
				AND rs.root_generation = j.root_generation
			WHERE j.job_kind = ?
				AND j.source_key IN (`+sourcePlaceholders+`)
				AND j.location_id IS NOT NULL
				AND j.status IN ('queued', 'failed')
				AND EXISTS (
					SELECT 1
					FROM local_scan_jobs failed_job
					WHERE failed_job.source_key = j.source_key
						AND failed_job.job_kind = ?
						AND failed_job.location_id = j.location_id
						AND failed_job.root_generation = j.root_generation
						AND failed_job.status = 'failed'
				)
				AND NOT EXISTS (
					SELECT 1
					FROM local_scan_jobs running_job
					WHERE running_job.source_key = j.source_key
						AND running_job.job_kind = ?
						AND running_job.location_id = j.location_id
						AND running_job.root_generation = j.root_generation
						AND running_job.status = 'running'
				)
			GROUP BY j.source_key, j.location_id
		)`, updateArgs...)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("requeue failed local metadata jobs: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("read requeued failed local metadata rows: %w", err)
	}
	cleanupArgs := []any{localMetadataJobKind}
	for _, sourceKey := range sourceKeys {
		cleanupArgs = append(cleanupArgs, sourceKey)
	}
	cleanupArgs = append(cleanupArgs, localMetadataJobKind)
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_scan_jobs
		WHERE job_kind = ?
			AND source_key IN (`+sourcePlaceholders+`)
			AND location_id IS NOT NULL
			AND status = 'failed'
			AND EXISTS (
				SELECT 1
				FROM local_scan_jobs active_job
				WHERE active_job.source_key = local_scan_jobs.source_key
					AND active_job.job_kind = ?
					AND active_job.location_id = local_scan_jobs.location_id
					AND active_job.root_generation = local_scan_jobs.root_generation
					AND active_job.status IN ('queued', 'running')
			)
			AND EXISTS (
				SELECT 1
				FROM local_asset_locations l
				JOIN local_scan_root_state rs
					ON rs.source_key = local_scan_jobs.source_key
					AND rs.root_key = l.root_key
					AND rs.root_generation = local_scan_jobs.root_generation
				WHERE l.id = local_scan_jobs.location_id
			)`, cleanupArgs...); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("complete superseded failed local metadata jobs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit failed local metadata requeue: %w", err)
	}
	return int(affected), nil
}

func (s *Service) processLocalMetadataJob(ctx context.Context, job localMetadataJob) (bool, error) {
	now := time.Now().UTC()
	nowText := formatCatalogTime(now)
	trustedRoot, err := s.acquireTrustedLocalMediaRoot(ctx, job.SourceKey)
	if err != nil {
		return false, err
	}
	defer trustedRoot.Close()
	if !trustedRoot.matchesJobRoot(job.RootKey, job.RootGeneration) {
		return false, ErrLocalMediaRootNotTrusted
	}
	locationStarted := time.Now()
	location, err := s.localLocationForMetadata(ctx, job.LocationID)
	if err != nil {
		return false, err
	}
	if elapsed := time.Since(locationStarted); elapsed > localMetadataSlowStepThreshold {
		log.Printf("timich-agent local metadata location read slow job_id=%d location_id=%d elapsed=%s", job.ID, job.LocationID, elapsed.Round(time.Millisecond))
	}
	if location.RootKey != trustedRoot.root.Key {
		return false, ErrLocalMediaRootNotTrusted
	}
	claimStarted := time.Now()
	claimed, err := s.claimLocalMetadataJob(ctx, job, trustedRoot, nowText)
	if err != nil || !claimed {
		return false, err
	}
	if elapsed := time.Since(claimStarted); elapsed > localMetadataSlowStepThreshold {
		log.Printf("timich-agent local metadata claim slow job_id=%d location_id=%d elapsed=%s", job.ID, job.LocationID, elapsed.Round(time.Millisecond))
	}
	if location.Status == "missing" {
		return false, s.completeLocalMetadataJob(ctx, job.ID, nowText)
	}
	file, info, err := openLocalRootFileFromPinnedRoot(trustedRoot.handle, location.RelativePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if markErr := s.markLocalLocationMissing(ctx, location.ID, nowText, "metadata_absent"); markErr != nil {
				return false, markErr
			}
		}
		return false, err
	}
	defer file.Close()
	if !localLocationMatchesFileInfo(location, info) {
		if err := s.resettleLocalMetadataJob(ctx, job.ID, location, info, "source_changed_before_metadata"); err != nil {
			return false, err
		}
		return false, errLocalMetadataSourceChanged
	}
	if info.Size() >= localMetadataLargeFileLogThreshold {
		log.Printf("timich-agent local metadata sha1 starting job_id=%d location_id=%d size_bytes=%d", job.ID, job.LocationID, info.Size())
	}
	shaStarted := time.Now()
	sha1Hex, hashedBytes, infoAfter, err := sha1OpenFile(ctx, file)
	if err != nil {
		return false, err
	}
	if elapsed := time.Since(shaStarted); elapsed > localMetadataSlowStepThreshold {
		log.Printf("timich-agent local metadata sha1 slow job_id=%d location_id=%d size_bytes=%d elapsed=%s", job.ID, job.LocationID, info.Size(), elapsed.Round(time.Millisecond))
	}
	if hashedBytes != info.Size() || !localFileInfoUnchanged(info, infoAfter) {
		if err := s.resettleLocalMetadataJob(ctx, job.ID, location, infoAfter, "source_changed_during_metadata"); err != nil {
			return false, err
		}
		return false, errLocalMetadataSourceChanged
	}
	pathFile, pathInfo, pathnameMatches, err := reopenPinnedLocalRootFileAndMatch(trustedRoot.handle, location.RelativePath, infoAfter)
	if err != nil {
		return false, err
	}
	defer pathFile.Close()
	if !pathnameMatches {
		if err := s.resettleLocalMetadataJob(ctx, job.ID, location, pathInfo, "source_path_replaced_during_metadata"); err != nil {
			return false, err
		}
		return false, errLocalMetadataSourceChanged
	}
	mediaType, ok := localMediaTypeFromFilename(location.RelativePath)
	if !ok {
		return false, fmt.Errorf("unsupported local media extension")
	}
	filename := filepath.Base(location.RelativePath)
	capturedAt := info.ModTime().UTC()
	registerStarted := time.Now()
	registrationNowText := formatCatalogTime(time.Now().UTC())
	err = s.registerLocalMetadata(ctx, trustedRoot.datasource, location, sha1Hex, mediaType, filename, capturedAt, infoAfter, registrationNowText, job.ID)
	if elapsed := time.Since(registerStarted); elapsed > localMetadataSlowStepThreshold {
		log.Printf("timich-agent local metadata register slow job_id=%d location_id=%d size_bytes=%d elapsed=%s", job.ID, job.LocationID, info.Size(), elapsed.Round(time.Millisecond))
	}
	return true, err
}

func (s *Service) claimLocalMetadataJob(ctx context.Context, job localMetadataJob, trustedRoot *trustedLocalMediaRoot, nowText string) (bool, error) {
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
			AND status = 'queued'
			AND scheduled_at <= ?
			AND EXISTS (
				SELECT 1
				FROM local_asset_locations l
				JOIN local_scan_root_state rs
					ON rs.source_key = local_scan_jobs.source_key
					AND rs.root_key = local_scan_jobs.root_key
					AND rs.root_generation = local_scan_jobs.root_generation
				WHERE l.id = local_scan_jobs.location_id
					AND l.root_key = local_scan_jobs.root_key
					AND (l.metadata_not_before IS NULL OR l.metadata_not_before <= ?)
			)`,
		nowText,
		job.ID,
		job.SourceKey,
		job.RootKey,
		normalizeLocalMediaRootGeneration(job.RootGeneration),
		localMetadataJobKind,
		nowText,
		nowText,
	)
	if err != nil {
		return false, fmt.Errorf("claim local metadata job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read local metadata claim rows: %w", err)
	}
	return affected > 0, nil
}

func (s *Service) localLocationForMetadata(ctx context.Context, locationID int64) (localLocationForMetadata, error) {
	var location localLocationForMetadata
	if err := s.catalog.queryDB().QueryRowContext(ctx, `SELECT id, source_key, root_key, relative_path, status, size_bytes, mtime, fast_signature, file_identity
		FROM local_asset_locations
		WHERE id = ?`, locationID).
		Scan(&location.ID, &location.SourceKey, &location.RootKey, &location.RelativePath, &location.Status, &location.SizeBytes, &location.MTime, &location.FastSignature, &location.FileIdentity); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return localLocationForMetadata{}, ErrAssetNotFound
		}
		return localLocationForMetadata{}, fmt.Errorf("read local metadata location: %w", err)
	}
	return location, nil
}

func (s *Service) registerLocalMetadata(ctx context.Context, datasource config.DatasourceConfig, location localLocationForMetadata, sha1Hex string, mediaType string, filename string, capturedAt time.Time, info os.FileInfo, nowText string, jobID int64) error {
	assetID := localAssetID(sha1Hex, info.Size())
	capturedAtText := formatCatalogTime(capturedAt)
	mtimeText := formatCatalogTime(info.ModTime().UTC())
	fastSignature := fmt.Sprintf("%d:%s", info.Size(), mtimeText)
	fileIdentity := localFileIdentity(info)
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin local metadata registration: %w", err)
	}
	eligible, err := lockLocalMetadataRegistrationInTx(ctx, tx, location, jobID, nowText)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if !eligible {
		if err := rescheduleLocalMetadataJobForLatestLocationInTx(ctx, tx, jobID, nowText); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit local metadata registration reschedule: %w", err)
		}
		return errLocalMetadataSourceChanged
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO local_assets (
			source_key, asset_id, sha1_hex, content_size_bytes, media_type, filename,
			captured_at, captured_at_source, primary_location_id, visibility_status,
			thumbnail_status, first_seen_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', 'pending', ?, ?)
		ON CONFLICT(source_key, asset_id) DO UPDATE SET
			content_size_bytes = excluded.content_size_bytes,
			visibility_status = 'active',
			primary_location_id = COALESCE(local_assets.primary_location_id, excluded.primary_location_id),
			updated_at = excluded.updated_at`,
		datasource.SourceKey,
		assetID,
		sha1Hex,
		info.Size(),
		mediaType,
		filename,
		capturedAtText,
		"file_mtime",
		location.ID,
		nowText,
		nowText,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("upsert local asset: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE local_asset_locations
		SET asset_id = ?,
			size_bytes = ?,
			mtime = ?,
			fast_signature = ?,
			file_identity = ?,
			sha1_hex = ?,
			status = 'active',
			status_reason = NULL,
			last_seen_at = ?,
			verified_at = ?,
			content_verified_at = ?,
			content_verification_attempted_at = ?,
			content_verification_error = NULL,
			metadata_not_before = NULL,
			updated_at = ?
		WHERE id = ?`,
		assetID,
		info.Size(),
		mtimeText,
		fastSignature,
		fileIdentity,
		sha1Hex,
		nowText,
		nowText,
		nowText,
		nowText,
		nowText,
		location.ID,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("update local asset location metadata: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename,
			captured_at, duration, visibility_status, source_updated_at, is_favorite,
			content_sha1_hex, content_size_bytes, place_label, description, first_seen_at,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, NULL, 'active', ?, 0, ?, ?, NULL, NULL, ?, ?)
		ON CONFLICT(source_key, upstream_asset_id) DO UPDATE SET
			datasource_kind = excluded.datasource_kind,
			media_type = excluded.media_type,
			filename = excluded.filename,
			captured_at = excluded.captured_at,
			visibility_status = 'active',
			source_updated_at = excluded.source_updated_at,
			content_sha1_hex = excluded.content_sha1_hex,
			content_size_bytes = excluded.content_size_bytes,
			updated_at = excluded.updated_at`,
		datasource.SourceKey,
		config.DatasourceKindLocalFiles,
		assetID,
		mediaType,
		filename,
		capturedAtText,
		mtimeText,
		sha1Hex,
		info.Size(),
		nowText,
		nowText,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("upsert local catalog asset: %w", err)
	}
	if err = s.catalog.refreshCatalogCanonicalAssetInTx(ctx, tx, datasource.SourceKey, assetID, nowText); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err = completeLocalScanJobInTx(ctx, tx, jobID, nowText); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.catalog.commitCatalogAssetChanges(ctx, tx, true); err != nil {
		return fmt.Errorf("commit local metadata registration: %w", err)
	}
	return nil
}

func lockLocalMetadataRegistrationInTx(ctx context.Context, tx *sql.Tx, location localLocationForMetadata, jobID int64, nowText string) (bool, error) {
	// This no-op update is intentionally the transaction's first write. It
	// validates the worker's Location generation and holds the SQLite writer
	// lock so a scan cannot advance the deadline before registration commits.
	result, err := tx.ExecContext(ctx, `UPDATE local_asset_locations
		SET updated_at = updated_at
		WHERE id = ?
			AND source_key = ?
			AND status = ?
			AND size_bytes = ?
			AND mtime = ?
			AND fast_signature = ?
			AND file_identity = ?
			AND (metadata_not_before IS NULL OR metadata_not_before <= ?)
			AND EXISTS (
				SELECT 1 FROM local_scan_jobs j
				WHERE j.id = ?
					AND j.job_kind = ?
					AND j.status = 'running'
					AND j.location_id = local_asset_locations.id
			)`,
		location.ID,
		location.SourceKey,
		location.Status,
		location.SizeBytes,
		location.MTime,
		location.FastSignature,
		location.FileIdentity,
		nowText,
		jobID,
		localMetadataJobKind,
	)
	if err != nil {
		return false, fmt.Errorf("lock local metadata registration eligibility: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read local metadata registration eligibility rows: %w", err)
	}
	return affected > 0, nil
}

func rescheduleLocalMetadataJobForLatestLocationInTx(ctx context.Context, tx *sql.Tx, jobID int64, nowText string) error {
	// Reuse the claimed job instead of creating a second worker candidate for
	// the same Location while the original worker is still unwinding.
	if _, err := tx.ExecContext(ctx, `UPDATE local_scan_jobs
		SET status = 'queued',
			scheduled_at = MAX(?, COALESCE((
				SELECT l.metadata_not_before
				FROM local_asset_locations l
				WHERE l.id = local_scan_jobs.location_id
			), ?)),
			sort_at = COALESCE((
				SELECT l.mtime
				FROM local_asset_locations l
				WHERE l.id = local_scan_jobs.location_id
			), sort_at),
			locked_at = NULL,
			completed_at = NULL,
			last_error = NULL
		WHERE id = ?
			AND job_kind = ?
			AND status = 'running'`, nowText, nowText, jobID, localMetadataJobKind); err != nil {
		return fmt.Errorf("reschedule local metadata job for latest location: %w", err)
	}
	return nil
}

func localAssetID(sha1Hex string, sizeBytes int64) string {
	return fmt.Sprintf("%s-%d", strings.ToLower(strings.TrimSpace(sha1Hex)), sizeBytes)
}

func (s *Service) completeLocalMetadataJob(ctx context.Context, jobID int64, nowText string) error {
	return s.completeLocalScanJob(ctx, jobID, nowText)
}

func (s *Service) completeLocalScanJob(ctx context.Context, jobID int64, nowText string) error {
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin local scan job completion: %w", err)
	}
	if err := completeLocalScanJobInTx(ctx, tx, jobID, nowText); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit local scan job completion: %w", err)
	}
	return nil
}

func completeLocalScanJobInTx(ctx context.Context, tx *sql.Tx, jobID int64, nowText string) error {
	_ = nowText
	if _, err := tx.ExecContext(ctx, `DELETE FROM local_scan_jobs WHERE id = ?`, jobID); err != nil {
		return fmt.Errorf("complete local scan job: %w", err)
	}
	return nil
}

func (s *Service) failLocalMetadataJob(ctx context.Context, job localMetadataJob, jobErr error) (bool, error) {
	trustedRoot, err := s.acquireTrustedLocalMediaRoot(ctx, job.SourceKey)
	if err != nil {
		if errors.Is(err, ErrLocalMediaRootNotTrusted) || errors.Is(err, ErrNoDatasourceConfigured) {
			_, recoveryErr := s.deferClaimedLocalMetadataJob(job)
			return recoveryErr == nil, recoveryErr
		}
		return false, err
	}
	defer trustedRoot.Close()
	if !trustedRoot.matchesJobRoot(job.RootKey, job.RootGeneration) {
		_, recoveryErr := s.deferClaimedLocalMetadataJob(job)
		return recoveryErr == nil, recoveryErr
	}
	nowText := formatCatalogTime(time.Now().UTC())
	message := ""
	if jobErr != nil {
		message = jobErr.Error()
	}
	_, err = s.catalog.db.ExecContext(ctx, `UPDATE local_scan_jobs
		SET status = 'failed',
			completed_at = ?,
			last_error = ?
		WHERE id = ?
			AND source_key = ?
			AND root_key = ?
			AND root_generation = ?
			AND job_kind = ?
			AND status IN ('queued', 'running')
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
		localMetadataJobKind,
	)
	if err != nil {
		return false, fmt.Errorf("fail local metadata job: %w", err)
	}
	return false, nil
}

func (s *Service) resettleLocalMetadataJob(ctx context.Context, jobID int64, location localLocationForMetadata, info os.FileInfo, reason string) error {
	if info == nil || !info.Mode().IsRegular() {
		return fmt.Errorf("resettle local metadata source: path is not a regular file")
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
		return fmt.Errorf("begin local metadata resettle: %w", err)
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
		return fmt.Errorf("resettle local asset location: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_scan_jobs
		SET status = 'queued',
			scheduled_at = ?,
			locked_at = NULL,
			completed_at = NULL,
			last_error = NULL
		WHERE id = ?`, notBeforeText, jobID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("reschedule local metadata job: %w", err)
	}
	changedCanonicalIDs, err := refreshLocalAssetVisibilityInTx(ctx, tx, location.SourceKey, location.RootKey, nowText)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.catalog.rebuildCatalogCanonicalIDsInTx(ctx, tx, changedCanonicalIDs, nowText); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.catalog.commitCatalogAssetChanges(ctx, tx, len(changedCanonicalIDs) > 0); err != nil {
		return fmt.Errorf("commit local metadata resettle: %w", err)
	}
	return nil
}

func (s *Service) markLocalLocationMissing(ctx context.Context, locationID int64, nowText string, reason string) error {
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin local metadata missing update: %w", err)
	}
	var sourceKey string
	var rootKey string
	if err := tx.QueryRowContext(ctx, `SELECT source_key, root_key
		FROM local_asset_locations
		WHERE id = ?`, locationID).Scan(&sourceKey, &rootKey); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("read local metadata missing source key: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_asset_locations
		SET status = 'missing',
			status_reason = ?,
			updated_at = ?
		WHERE id = ?`, reason, nowText, locationID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("mark local metadata location missing: %w", err)
	}
	changedCanonicalIDs, err := refreshLocalAssetVisibilityInTx(ctx, tx, sourceKey, rootKey, nowText)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.catalog.rebuildCatalogCanonicalIDsInTx(ctx, tx, changedCanonicalIDs, nowText); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.catalog.commitCatalogAssetChanges(ctx, tx, len(changedCanonicalIDs) > 0); err != nil {
		return fmt.Errorf("commit local metadata missing update: %w", err)
	}
	return nil
}

func sha1File(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open local media for sha1: %w", err)
	}
	defer file.Close()
	hash, written, _, err := sha1OpenFile(context.Background(), file)
	return hash, written, err
}

func sha1OpenFile(ctx context.Context, file *os.File) (string, int64, os.FileInfo, error) {
	if file == nil {
		return "", 0, nil, fmt.Errorf("hash local media: file is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stopWatcher := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			_ = file.Close()
		case <-stopWatcher:
		}
	}()
	defer func() {
		close(stopWatcher)
		<-watcherDone
	}()

	hash := sha1.New()
	buffer := make([]byte, 1024*1024)
	var readBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return "", readBytes, nil, err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			if _, err := hash.Write(buffer[:count]); err != nil {
				return "", readBytes, nil, fmt.Errorf("hash local media: %w", err)
			}
			readBytes += int64(count)
		}
		if readErr != nil {
			if errors.Is(readErr, os.ErrClosed) && ctx.Err() != nil {
				return "", readBytes, nil, ctx.Err()
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			return "", readBytes, nil, fmt.Errorf("hash local media: %w", readErr)
		}
	}
	info, err := file.Stat()
	if err != nil {
		return "", readBytes, nil, fmt.Errorf("inspect hashed local media: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), readBytes, info, nil
}

func localLocationMatchesFileInfo(location localLocationForMetadata, info os.FileInfo) bool {
	if info == nil {
		return false
	}
	mtimeText := formatCatalogTime(info.ModTime().UTC())
	return location.SizeBytes == info.Size() &&
		location.MTime == mtimeText &&
		location.FastSignature == fmt.Sprintf("%d:%s", info.Size(), mtimeText) &&
		location.FileIdentity == localFileIdentity(info)
}

func localFileInfoUnchanged(before os.FileInfo, after os.FileInfo) bool {
	if before == nil || after == nil || !os.SameFile(before, after) {
		return false
	}
	return before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func localMediaTypeFromFilename(name string) (string, bool) {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".mov":
		return "video", true
	case ".jpg", ".jpeg", ".png", ".webp", ".heic", ".heif":
		return "image", true
	default:
		return "", false
	}
}

func localRootChildPath(rootPath string, relativePath string) (string, error) {
	rootAbs, err := filepath.Abs(rootPath)
	if err != nil {
		return "", fmt.Errorf("resolve local root path: %w", err)
	}
	childAbs, err := filepath.Abs(filepath.Join(rootAbs, filepath.FromSlash(relativePath)))
	if err != nil {
		return "", fmt.Errorf("resolve local child path: %w", err)
	}
	if childAbs != rootAbs && !strings.HasPrefix(childAbs, rootAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("local child path escapes root")
	}
	if childAbs == rootAbs {
		return "", fmt.Errorf("local child path points at root")
	}
	return childAbs, nil
}
