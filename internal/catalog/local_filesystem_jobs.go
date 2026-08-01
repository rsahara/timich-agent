package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const localClaimedJobRecoveryTimeout = 5 * time.Second

// ResetRunningLocalScanJobs returns claimed Local jobs to the queue. Callers
// must first establish that no live Local worker owns them, either because a new
// runtime is taking ownership of the data directory or because a canceled
// assignment has fully stopped.
func (s *Service) ResetRunningLocalScanJobs(ctx context.Context) (int, error) {
	if s == nil || s.catalog == nil {
		return 0, ErrNoDatasourceConfigured
	}
	nowText := formatCatalogTime(time.Now().UTC())
	result, err := s.catalog.db.ExecContext(ctx, `UPDATE local_scan_jobs
		SET status = 'queued',
			scheduled_at = CASE
				WHEN job_kind = ? THEN MAX(?, COALESCE((
					SELECT l.metadata_not_before
					FROM local_asset_locations l
					WHERE l.id = local_scan_jobs.location_id
				), ?))
				ELSE ?
			END,
			sort_at = CASE
				WHEN job_kind = ? THEN COALESCE((
					SELECT l.mtime
					FROM local_asset_locations l
					WHERE l.id = local_scan_jobs.location_id
				), sort_at)
				ELSE sort_at
			END,
			locked_at = NULL,
			completed_at = NULL,
			last_error = NULL
		WHERE status = 'running'
			AND job_kind IN (?, ?)`,
		localMetadataJobKind,
		nowText,
		nowText,
		nowText,
		localMetadataJobKind,
		localMetadataJobKind,
		localThumbnailJobKind,
	)
	if err != nil {
		return 0, fmt.Errorf("reset running local scan jobs: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read reset running local scan job count: %w", err)
	}
	return int(count), nil
}

func (s *Service) deferClaimedLocalMetadataJob(job localMetadataJob) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), localClaimedJobRecoveryTimeout)
	defer cancel()
	return s.deferClaimedLocalMetadataJobWithContext(ctx, job)
}

func (s *Service) deferClaimedLocalMetadataJobWithContext(ctx context.Context, job localMetadataJob) (bool, error) {
	nowText := formatCatalogTime(time.Now().UTC())
	result, err := s.catalog.db.ExecContext(ctx, `UPDATE local_scan_jobs
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
			AND source_key = ?
			AND root_key = ?
			AND root_generation = ?
			AND job_kind = ?
			AND location_id = ?
			AND status = 'running'`,
		nowText,
		nowText,
		job.ID,
		job.SourceKey,
		job.RootKey,
		normalizeLocalMediaRootGeneration(job.RootGeneration),
		localMetadataJobKind,
		job.LocationID,
	)
	if err != nil {
		return false, fmt.Errorf("defer claimed local metadata job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read deferred local metadata job count: %w", err)
	}
	if affected > 0 {
		s.notifyLocalWorkQueued()
	}
	return affected > 0, nil
}

func (s *Service) deferClaimedLocalThumbnailJob(job localThumbnailJob) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), localClaimedJobRecoveryTimeout)
	defer cancel()
	return s.deferClaimedLocalThumbnailJobWithContext(ctx, job)
}

func (s *Service) deferClaimedLocalThumbnailJobWithContext(ctx context.Context, job localThumbnailJob) (bool, error) {
	nowText := formatCatalogTime(time.Now().UTC())
	result, err := s.catalog.db.ExecContext(ctx, `UPDATE local_scan_jobs
		SET status = 'queued',
			scheduled_at = ?,
			locked_at = NULL,
			completed_at = NULL,
			last_error = NULL
		WHERE id = ?
			AND source_key = ?
			AND root_key = ?
			AND root_generation = ?
			AND job_kind = ?
			AND asset_id = ?
			AND status = 'running'`,
		nowText,
		job.ID,
		job.SourceKey,
		job.RootKey,
		normalizeLocalMediaRootGeneration(job.RootGeneration),
		localThumbnailJobKind,
		job.AssetID,
	)
	if err != nil {
		return false, fmt.Errorf("defer claimed local thumbnail job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read deferred local thumbnail job count: %w", err)
	}
	if affected > 0 {
		s.notifyLocalWorkQueued()
	}
	return affected > 0, nil
}

func (s *Service) rememberLocalMetadataClaimRecovery(job localMetadataJob) {
	if s == nil {
		return
	}
	s.localClaimRecoveryMu.Lock()
	if s.localMetadataRecoveries == nil {
		s.localMetadataRecoveries = make(map[int64]localMetadataJob)
	}
	s.localMetadataRecoveries[job.ID] = job
	s.localClaimRecoveryMu.Unlock()
	s.notifyLocalWorkQueued()
}

func (s *Service) rememberLocalThumbnailClaimRecovery(job localThumbnailJob) {
	if s == nil {
		return
	}
	s.localClaimRecoveryMu.Lock()
	if s.localThumbnailRecoveries == nil {
		s.localThumbnailRecoveries = make(map[int64]localThumbnailJob)
	}
	s.localThumbnailRecoveries[job.ID] = job
	s.localClaimRecoveryMu.Unlock()
	s.notifyLocalWorkQueued()
}

func (s *Service) recoverRememberedLocalClaims(ctx context.Context) error {
	if s == nil || s.catalog == nil {
		return ErrNoDatasourceConfigured
	}
	s.localClaimRecoveryMu.Lock()
	metadataJobs := make([]localMetadataJob, 0, len(s.localMetadataRecoveries))
	for _, job := range s.localMetadataRecoveries {
		metadataJobs = append(metadataJobs, job)
	}
	thumbnailJobs := make([]localThumbnailJob, 0, len(s.localThumbnailRecoveries))
	for _, job := range s.localThumbnailRecoveries {
		thumbnailJobs = append(thumbnailJobs, job)
	}
	s.localClaimRecoveryMu.Unlock()

	var recoveryErr error
	for _, job := range metadataJobs {
		_, err := s.deferClaimedLocalMetadataJobWithContext(ctx, job)
		if err != nil {
			recoveryErr = errors.Join(recoveryErr, fmt.Errorf("recover remembered local metadata job %d: %w", job.ID, err))
			continue
		}
		s.localClaimRecoveryMu.Lock()
		delete(s.localMetadataRecoveries, job.ID)
		s.localClaimRecoveryMu.Unlock()
	}
	for _, job := range thumbnailJobs {
		_, err := s.deferClaimedLocalThumbnailJobWithContext(ctx, job)
		if err != nil {
			recoveryErr = errors.Join(recoveryErr, fmt.Errorf("recover remembered local thumbnail job %d: %w", job.ID, err))
			continue
		}
		s.localClaimRecoveryMu.Lock()
		delete(s.localThumbnailRecoveries, job.ID)
		s.localClaimRecoveryMu.Unlock()
	}
	return recoveryErr
}
