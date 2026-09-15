package catalog

import (
	"context"
	"database/sql"
	"strings"
)

func (s *Service) repairLocalCaptureDatesInTx(ctx context.Context, tx *sql.Tx, sourceKeys []string, capability localMediaHelperCapabilityStatus, nowText string) (int, int, error) {
	predicate := `NOT EXISTS (SELECT 1 FROM local_capture_metadata_state m
		WHERE m.source_key = a.source_key AND m.asset_id = a.asset_id
		AND m.extractor_version = ? AND m.timezone = ?)`
	args := []any{localCaptureMetadataVersion, s.localCaptureLocation().String()}
	available := `((a.media_type = 'image' AND ?) OR (a.media_type = 'video' AND ?))`
	args = append(args, capability.canCapture("image"), capability.canCapture("video"))
	countArgs := make([]any, 0, len(sourceKeys)+len(args))
	for _, source := range sourceKeys {
		countArgs = append(countArgs, source)
	}
	countArgs = append(countArgs, args...)
	var skipped int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM local_assets a
		WHERE a.source_key IN (`+strings.TrimRight(strings.Repeat("?,", len(sourceKeys)), ",")+`)
		AND a.visibility_status = 'active' AND `+predicate+` AND NOT `+available, countArgs...).Scan(&skipped)
	if err != nil {
		return 0, 0, err
	}
	if !capability.canCapture("image") && !capability.canCapture("video") {
		return 0, skipped, nil
	}
	queued, err := s.queueLocalMetadataTargetsInTx(ctx, tx, sourceKeys, predicate+` AND `+available, args, nowText)
	return queued, skipped, err
}
