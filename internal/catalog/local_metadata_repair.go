package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// LocalMetadataRepairResult reports metadata work moved back to the asynchronous
// metadata queue, including capture-date checks and video-duration backfill.
type LocalMetadataRepairResult struct {
	Queued               int `json:"queued"`
	FailedQueued         int `json:"failedQueued"`
	VideoDurationQueued  int `json:"videoDurationQueued"`
	VideoDurationSkipped int `json:"videoDurationSkipped"`
	CaptureDateQueued    int `json:"captureDateQueued"`
	CaptureDateSkipped   int `json:"captureDateSkipped"`
}

// Resolve the asset's locations before probing pending jobs. A datasource-wide
// job scan inside each target's EXISTS makes repeated repair quadratic.
const localMetadataRepairActiveJobQuery = `SELECT 1
	FROM local_asset_locations active_location INDEXED BY idx_local_asset_locations_asset_status
	WHERE active_location.source_key = target.source_key
		AND active_location.asset_id = target.asset_id
		AND EXISTS (
			SELECT 1
			FROM local_scan_jobs active_job
			JOIN local_scan_root_state active_root
				ON active_root.source_key = active_job.source_key
				AND active_root.root_key = active_location.root_key
				AND active_root.root_generation = active_job.root_generation
			WHERE active_job.source_key = active_location.source_key
				AND active_job.location_id = active_location.id
				AND active_job.job_kind = ?
				AND active_job.status IN ('queued', 'running')
		)`

func (s *Service) RepairLocalMetadata(ctx context.Context) (LocalMetadataRepairResult, error) {
	if s == nil || s.catalog == nil {
		return LocalMetadataRepairResult{}, ErrNoDatasourceConfigured
	}
	if err := s.ensureStateWritesAvailable(); err != nil {
		return LocalMetadataRepairResult{}, err
	}
	sourceKeys := s.LocalDatasourceSourceKeys()
	if len(sourceKeys) == 0 {
		return LocalMetadataRepairResult{}, nil
	}
	capability := s.localMediaHelperCapabilityStatusWithContext(ctx)
	inspectionAvailable := capability.Usable && capability.InspectVideo
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return LocalMetadataRepairResult{}, fmt.Errorf("begin local metadata repair: %w", err)
	}
	nowText := formatCatalogTime(time.Now().UTC())
	captureDateQueued, captureDateSkipped, err := s.repairLocalCaptureDatesInTx(ctx, tx, sourceKeys, capability, nowText)
	if err != nil {
		_ = tx.Rollback()
		return LocalMetadataRepairResult{}, err
	}
	videoDurationQueued, videoDurationSkipped, err := s.repairMissingLocalVideoDurationsInTx(
		ctx,
		tx,
		sourceKeys,
		inspectionAvailable,
		nowText,
	)
	if err != nil {
		_ = tx.Rollback()
		return LocalMetadataRepairResult{}, err
	}
	failedQueued, err := s.requeueFailedLocalMetadataJobsInTx(ctx, tx, sourceKeys, true, nowText)
	if err != nil {
		_ = tx.Rollback()
		return LocalMetadataRepairResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return LocalMetadataRepairResult{}, fmt.Errorf("commit local metadata repair: %w", err)
	}
	return LocalMetadataRepairResult{
		Queued:               captureDateQueued + videoDurationQueued + failedQueued,
		FailedQueued:         failedQueued,
		VideoDurationQueued:  videoDurationQueued,
		VideoDurationSkipped: videoDurationSkipped,
		CaptureDateQueued:    captureDateQueued,
		CaptureDateSkipped:   captureDateSkipped,
	}, nil
}

func (s *Service) queueLocalMetadataTargetsInTx(ctx context.Context, tx *sql.Tx, sourceKeys []string, predicate string, predicateArgs []any, nowText string) (int, error) {
	sourcePlaceholders := strings.TrimRight(strings.Repeat("?,", len(sourceKeys)), ",")
	rootPredicates := make([]string, 0, len(sourceKeys))
	rootArgs := make([]any, 0, 2*len(sourceKeys))
	for _, sourceKey := range sourceKeys {
		_, root, err := s.localDatasourceAndRoot(sourceKey)
		if err != nil {
			return 0, err
		}
		rootPredicates = append(rootPredicates, `(a.source_key = ? AND l.root_key = ?)`)
		rootArgs = append(rootArgs, sourceKey, root.Key)
	}
	targetsQuery := `SELECT a.source_key, a.asset_id, l.id AS location_id,
			l.root_key, rs.root_generation, l.mtime
		FROM local_assets a
		JOIN local_asset_locations l
			ON l.source_key = a.source_key
			AND l.asset_id = a.asset_id
			AND l.status = 'active'
		JOIN local_scan_root_state rs
			ON rs.source_key = l.source_key
			AND rs.root_key = l.root_key
		WHERE a.source_key IN (` + sourcePlaceholders + `)
			AND a.visibility_status = 'active'
			AND (` + predicate + `)
			AND (` + strings.Join(rootPredicates, " OR ") + `)
			AND l.id = (
				SELECT candidate.id
				FROM local_asset_locations candidate
				WHERE candidate.source_key = a.source_key
					AND candidate.asset_id = a.asset_id
					AND candidate.status = 'active'
					AND candidate.root_key = l.root_key
				ORDER BY CASE WHEN candidate.id = a.primary_location_id THEN 0 ELSE 1 END,
					candidate.id ASC
				LIMIT 1
			)`
	sourceArgs := make([]any, 0, len(sourceKeys))
	for _, sourceKey := range sourceKeys {
		sourceArgs = append(sourceArgs, sourceKey)
	}
	sourceArgs = append(sourceArgs, predicateArgs...)
	sourceArgs = append(sourceArgs, rootArgs...)

	updateArgs := append([]any{}, sourceArgs...)
	updateArgs = append(updateArgs, localMetadataRepairPriority, nowText, localMetadataJobKind, localMetadataJobKind)
	updatedResult, err := tx.ExecContext(ctx, `WITH metadata_targets AS (`+targetsQuery+`)
		UPDATE local_scan_jobs
		SET status = 'queued',
			priority = ?,
			attempts = 0,
			scheduled_at = ?,
			sort_at = COALESCE((
				SELECT target.mtime
				FROM metadata_targets target
				WHERE target.location_id = local_scan_jobs.location_id
					AND target.root_generation = local_scan_jobs.root_generation
			), sort_at),
			locked_at = NULL,
			completed_at = NULL,
			last_error = NULL
		WHERE id IN (
			SELECT MAX(failed_job.id)
			FROM metadata_targets target
			JOIN local_scan_jobs failed_job
				ON failed_job.source_key = target.source_key
				AND failed_job.location_id = target.location_id
				AND failed_job.root_generation = target.root_generation
			WHERE failed_job.job_kind = ?
				AND failed_job.status = 'failed'
				AND NOT EXISTS (
					`+localMetadataRepairActiveJobQuery+`
				)
			GROUP BY target.source_key, target.asset_id
		)`, updateArgs...)
	if err != nil {
		return 0, fmt.Errorf("requeue failed local metadata repair jobs: %w", err)
	}
	updated, err := updatedResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read requeued failed local metadata repair rows: %w", err)
	}

	insertArgs := append([]any{}, sourceArgs...)
	insertArgs = append(insertArgs, localMetadataJobKind, localMetadataRepairPriority, nowText, localMetadataJobKind)
	insertedResult, err := tx.ExecContext(ctx, `WITH metadata_targets AS (`+targetsQuery+`)
		INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation,
			location_id, status, scheduled_at, sort_at
		)
		SELECT target.source_key, ?, ?, target.root_key, target.root_generation,
			target.location_id, 'queued', ?, target.mtime
		FROM metadata_targets target
		WHERE NOT EXISTS (
				`+localMetadataRepairActiveJobQuery+`
			)`, insertArgs...)
	if err != nil {
		return 0, fmt.Errorf("queue local metadata repair targets: %w", err)
	}
	inserted, err := insertedResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read queued local metadata repair rows: %w", err)
	}

	cleanupArgs := append([]any{}, sourceArgs...)
	cleanupArgs = append(cleanupArgs, localMetadataJobKind, localMetadataJobKind)
	if _, err := tx.ExecContext(ctx, `WITH metadata_targets AS (`+targetsQuery+`)
		DELETE FROM local_scan_jobs
		WHERE job_kind = ?
			AND status = 'failed'
			AND EXISTS (
				SELECT 1
				FROM local_asset_locations failed_location
				JOIN metadata_targets target
					ON target.source_key = failed_location.source_key
					AND target.asset_id = failed_location.asset_id
				WHERE failed_location.id = local_scan_jobs.location_id
					AND EXISTS (
						`+localMetadataRepairActiveJobQuery+`
					)
			)`, cleanupArgs...); err != nil {
		return 0, fmt.Errorf("remove superseded failed local metadata repair jobs: %w", err)
	}
	return int(updated + inserted), nil
}
