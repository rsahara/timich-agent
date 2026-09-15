package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const localMediaVideoInspectTimeout = 30 * time.Second

type localMediaHelperVideoInspectionResponse struct {
	SchemaVersion int    `json:"schemaVersion"`
	OK            bool   `json:"ok"`
	Operation     string `json:"operation"`
	Media         struct {
		MediaType  string `json:"mediaType"`
		DurationMS int64  `json:"durationMs"`
	} `json:"media"`
}

func (s *Service) repairMissingLocalVideoDurationsInTx(
	ctx context.Context,
	tx *sql.Tx,
	sourceKeys []string,
	inspectionAvailable bool,
	nowText string,
) (int, int, error) {
	sourcePlaceholders := strings.TrimRight(strings.Repeat("?,", len(sourceKeys)), ",")
	countArgs := make([]any, 0, len(sourceKeys))
	for _, sourceKey := range sourceKeys {
		countArgs = append(countArgs, sourceKey)
	}
	var missing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM local_assets
		WHERE source_key IN (`+sourcePlaceholders+`)
			AND media_type = 'video'
			AND visibility_status = 'active'
			AND TRIM(COALESCE(duration, '')) = ''`, countArgs...).Scan(&missing); err != nil {
		return 0, 0, fmt.Errorf("count local metadata repair targets: %w", err)
	}
	if missing == 0 {
		return 0, 0, nil
	}
	if !inspectionAvailable {
		return 0, missing, nil
	}
	queued, err := s.queueMissingLocalVideoDurationsInTx(ctx, tx, sourceKeys, nowText)
	if err != nil {
		return 0, 0, err
	}
	return queued, 0, nil
}

func (s *Service) queueMissingLocalVideoDurationsInTx(
	ctx context.Context,
	tx *sql.Tx,
	sourceKeys []string,
	nowText string,
) (int, error) {
	return s.queueLocalMetadataTargetsInTx(ctx, tx, sourceKeys,
		`a.media_type = 'video' AND TRIM(COALESCE(a.duration, '')) = ''`, nil, nowText)
}

func (s *Service) localVideoDurationMissing(ctx context.Context, sourceKey string, assetID string) (bool, error) {
	if strings.TrimSpace(assetID) == "" {
		return false, nil
	}
	var missing int
	if err := s.catalog.queryDB().QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1
		FROM local_assets
		WHERE source_key = ?
			AND asset_id = ?
			AND media_type = 'video'
			AND visibility_status = 'active'
			AND TRIM(COALESCE(duration, '')) = ''
	)`, sourceKey, assetID).Scan(&missing); err != nil {
		return false, fmt.Errorf("inspect local video duration state: %w", err)
	}
	return missing != 0, nil
}

func (s *Service) inspectLocalVideoDuration(ctx context.Context, input *os.File) (*string, error) {
	helperPath := strings.TrimSpace(s.mediaHelperPath)
	if helperPath == "" {
		return nil, fmt.Errorf("inspect local video duration: media helper is not configured")
	}
	output, err := runLocalMediaHelperCommandWithInputFileContext(
		ctx,
		localMediaVideoInspectTimeout,
		helperPath,
		s.mediaVipsPath,
		s.mediaFFmpegPath,
		input,
		"inspect-video",
		"--input", (localMediaInput{File: input}).helperPath(),
	)
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("inspect local video duration with media helper: %s", message)
	}
	var response localMediaHelperVideoInspectionResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, fmt.Errorf("parse media helper video inspection response: %w", err)
	}
	if response.SchemaVersion != 1 || !response.OK || response.Operation != "inspect-video" ||
		response.Media.MediaType != "video" || response.Media.DurationMS <= 0 {
		return nil, fmt.Errorf("media helper video inspection response is invalid")
	}
	duration := formatLocalVideoDuration(response.Media.DurationMS)
	return &duration, nil
}

func formatLocalVideoDuration(durationMS int64) string {
	seconds := durationMS / 1000
	microseconds := (durationMS % 1000) * 1000
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	remainingSeconds := seconds % 60
	return fmt.Sprintf("%d:%02d:%02d.%06d", hours, minutes, remainingSeconds, microseconds)
}

func (s *Service) updateLocalVideoDuration(
	ctx context.Context,
	location localLocationForMetadata,
	jobID int64,
	duration string,
	nowText string,
) error {
	duration = strings.TrimSpace(duration)
	if location.AssetID == "" || duration == "" {
		return fmt.Errorf("update local video duration: asset identity and duration are required")
	}
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin local video duration update: %w", err)
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
			return fmt.Errorf("commit local video duration reschedule: %w", err)
		}
		return errLocalMetadataSourceChanged
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_assets
		SET duration = ?, updated_at = ?
		WHERE source_key = ?
			AND asset_id = ?
			AND media_type = 'video'
			AND visibility_status = 'active'`, duration, nowText, location.SourceKey, location.AssetID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("update local asset video duration: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE catalog_assets
		SET duration = ?, updated_at = ?
		WHERE source_key = ?
			AND upstream_asset_id = ?
			AND datasource_kind = 'local_filesystem'
			AND media_type = 'video'
			AND visibility_status = 'active'`, duration, nowText, location.SourceKey, location.AssetID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("update local catalog video duration: %w", err)
	}
	if err := s.catalog.refreshCatalogCanonicalAssetInTx(ctx, tx, location.SourceKey, location.AssetID, nowText); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := completeLocalScanJobInTx(ctx, tx, jobID, nowText); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.catalog.commitCatalogAssetChanges(ctx, tx, true); err != nil {
		return fmt.Errorf("commit local video duration update: %w", err)
	}
	return nil
}

func nullStringFromOptionalString(value *string) sql.NullString {
	if value == nil || strings.TrimSpace(*value) == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: strings.TrimSpace(*value), Valid: true}
}
