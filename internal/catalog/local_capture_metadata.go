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

const localCaptureMetadataVersion = 1

type localCaptureDateCandidate struct {
	Value     string `json:"value"`
	Offset    string `json:"offset"`
	Subsecond string `json:"subsecond"`
	Source    string `json:"source"`
}

type localCaptureMetadata struct {
	CapturedAt time.Time
	Source     string
	Checked    bool
	Duration   *string
}

func (s *Service) localCaptureLocation() *time.Location {
	if s.captureLocation != nil {
		return s.captureLocation
	}
	return time.Local
}

func (c localMediaHelperCapabilityStatus) canCapture(mediaType string) bool {
	return c.Usable && ((mediaType == "image" && c.CaptureImage) || (mediaType == "video" && c.CaptureVideo))
}

func fallbackLocalCaptureMetadata(mtime, scanTime time.Time) localCaptureMetadata {
	if !mtime.IsZero() {
		return localCaptureMetadata{CapturedAt: mtime.UTC(), Source: "file_mtime"}
	}
	return localCaptureMetadata{CapturedAt: scanTime.UTC(), Source: "scan_time"}
}

func resolveLocalCaptureDate(candidate localCaptureDateCandidate, location *time.Location) (time.Time, bool) {
	switch candidate.Source {
	case "exif_original", "exif_digitized", "quicktime_creationdate", "container_creation_time", "video_stream_creation_time":
	default:
		return time.Time{}, false
	}
	value := strings.TrimSpace(candidate.Value)
	if len(value) < 19 || len(value) > 64 {
		return time.Time{}, false
	}
	// Normalize EXIF's date separators while preserving the time separators.
	if value[4] == ':' && value[7] == ':' {
		value = value[:4] + "-" + value[5:7] + "-" + value[8:]
	}
	if value[10] == ' ' {
		value = value[:10] + "T" + value[11:]
	}
	if sub := strings.TrimSpace(candidate.Subsecond); sub != "" && len(value) == 19 {
		if strings.IndexFunc(sub, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return time.Time{}, false
		}
		if len(sub) > 9 {
			sub = sub[:9]
		}
		value += "." + sub
	}
	if offset := strings.TrimSpace(candidate.Offset); offset != "" {
		value += offset
	}
	var parsed time.Time
	var err error
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05Z0700"} {
		parsed, err = time.Parse(layout, value)
		if err == nil {
			break
		}
	}
	if err != nil {
		// Explicit invalid offsets fail instead of silently becoming local time.
		parsed, err = time.ParseInLocation("2006-01-02T15:04:05", value, location)
	}
	if err != nil || parsed.Year() < 1 {
		return time.Time{}, false
	}
	// Zero QuickTime epochs are absence sentinels, not camera dates.
	if !strings.HasPrefix(candidate.Source, "exif_") &&
		(parsed.Equal(time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)) || parsed.Equal(time.Unix(0, 0))) {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func (s *Service) inspectLocalCaptureMetadata(ctx context.Context, file *os.File, mediaType string, mtime, scanTime time.Time) (localCaptureMetadata, error) {
	metadata := fallbackLocalCaptureMetadata(mtime, scanTime)
	operation := "inspect-" + mediaType
	output, err := runLocalMediaHelperCommandWithInputFileContext(ctx, localMediaVideoInspectTimeout,
		s.mediaHelperPath, s.mediaVipsPath, s.mediaFFmpegPath, file, operation, "--input", (localMediaInput{File: file}).helperPath())
	if err != nil {
		return metadata, fmt.Errorf("inspect local capture metadata: %w", err)
	}
	var response struct {
		SchemaVersion int    `json:"schemaVersion"`
		OK            bool   `json:"ok"`
		Operation     string `json:"operation"`
		Media         struct {
			MediaType    string                       `json:"mediaType"`
			DurationMS   int64                        `json:"durationMs"`
			CaptureDates *[]localCaptureDateCandidate `json:"captureDates"`
		} `json:"media"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return metadata, fmt.Errorf("parse capture metadata: %w", err)
	}
	if response.SchemaVersion != 1 || !response.OK || response.Operation != operation || response.Media.MediaType != mediaType || response.Media.CaptureDates == nil {
		return metadata, fmt.Errorf("invalid capture metadata response")
	}
	for _, candidate := range *response.Media.CaptureDates {
		if date, ok := resolveLocalCaptureDate(candidate, s.localCaptureLocation()); ok {
			metadata.CapturedAt, metadata.Source = date, candidate.Source
			break
		}
	}
	metadata.Checked = true
	if response.Media.DurationMS > 0 {
		duration := formatLocalVideoDuration(response.Media.DurationMS)
		metadata.Duration = &duration
	}
	return metadata, nil
}

// A separate additive table upgrades existing V5 catalogs without a rebuild.
// Recording successful absence avoids retrying files with no embedded date.
func (s *CatalogStore) ensureLocalCaptureMetadataSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS local_capture_metadata_state (
		source_key TEXT NOT NULL, asset_id TEXT NOT NULL,
		extractor_version INTEGER NOT NULL, timezone TEXT NOT NULL, checked_at TEXT NOT NULL,
		PRIMARY KEY(source_key, asset_id),
		FOREIGN KEY(source_key, asset_id) REFERENCES local_assets(source_key, asset_id) ON DELETE CASCADE
	)`)
	return err
}

func (s *Service) localCaptureMetadataPending(ctx context.Context, location localLocationForMetadata) (bool, error) {
	var pending bool
	err := s.catalog.queryDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM local_assets a
		WHERE a.source_key = ? AND a.asset_id = ? AND a.visibility_status = 'active'
		AND NOT EXISTS(SELECT 1 FROM local_capture_metadata_state m WHERE m.source_key = a.source_key
			AND m.asset_id = a.asset_id AND m.extractor_version = ? AND m.timezone = ?)
		AND ? = (SELECT l.id FROM local_asset_locations l WHERE l.source_key = a.source_key
			AND l.asset_id = a.asset_id AND l.root_key = ? AND l.status = 'active'
			ORDER BY CASE WHEN l.id = a.primary_location_id THEN 0 ELSE 1 END, l.id LIMIT 1))`,
		location.SourceKey, location.AssetID, localCaptureMetadataVersion, s.localCaptureLocation().String(), location.ID, location.RootKey).Scan(&pending)
	return pending, err
}

func (s *Service) markLocalCaptureMetadataInTx(ctx context.Context, tx *sql.Tx, sourceKey, assetID, nowText string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO local_capture_metadata_state(source_key, asset_id, extractor_version, timezone, checked_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(source_key, asset_id) DO UPDATE SET
		extractor_version = excluded.extractor_version, timezone = excluded.timezone, checked_at = excluded.checked_at`,
		sourceKey, assetID, localCaptureMetadataVersion, s.localCaptureLocation().String(), nowText)
	return err
}

func (s *Service) updateLocalCaptureMetadata(ctx context.Context, location localLocationForMetadata, jobID int64, metadata localCaptureMetadata, nowText string) error {
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	eligible, err := lockLocalMetadataRegistrationInTx(ctx, tx, location, jobID, nowText)
	if err != nil {
		return err
	}
	if !eligible {
		if err := rescheduleLocalMetadataJobForLatestLocationInTx(ctx, tx, jobID, nowText); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return errLocalMetadataSourceChanged
	}
	// Catalog date triggers also mark image embeddings dirty. Remember only
	// already-current inputs, so a date refresh never clears unrelated work.
	inputs, err := currentLocalCaptureSemanticInputs(ctx, tx, location)
	if err != nil {
		return err
	}
	date := formatCatalogTime(metadata.CapturedAt)
	duration := nullStringToAny(nullStringFromOptionalString(metadata.Duration))
	if _, err := tx.ExecContext(ctx, `UPDATE local_assets SET captured_at = ?, captured_at_source = ?,
		duration = COALESCE(?, duration), updated_at = ?
		WHERE source_key = ? AND asset_id = ? AND visibility_status = 'active'
		AND (captured_at != ? OR captured_at_source != ? OR (? IS NOT NULL AND duration IS NOT ?))`,
		date, metadata.Source, duration, nowText, location.SourceKey, location.AssetID, date, metadata.Source, duration, duration); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE catalog_assets SET captured_at = ?, duration = COALESCE(?, duration), updated_at = ?
		WHERE source_key = ? AND upstream_asset_id = ? AND datasource_kind = 'local_filesystem'
		AND (captured_at != ? OR (? IS NOT NULL AND duration IS NOT ?))`,
		date, duration, nowText, location.SourceKey, location.AssetID, date, duration, duration)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if err := s.markLocalCaptureMetadataInTx(ctx, tx, location.SourceKey, location.AssetID, nowText); err != nil {
		return err
	}
	if changed > 0 {
		if err := s.catalog.refreshCatalogCanonicalAssetInTx(ctx, tx, location.SourceKey, location.AssetID, nowText); err != nil {
			return err
		}
		for _, input := range inputs {
			// A changed canonical representative still needs ordinary input
			// reconciliation; only an identical representative can be retained.
			if _, err := tx.ExecContext(ctx, `UPDATE canonical_semantic_vector_inputs
				SET refresh_required = 0, updated_at = ?
				WHERE canonical_asset_id = ? AND model_id = ?
				AND EXISTS (SELECT 1 FROM catalog_canonical_assets c
					WHERE c.canonical_asset_id = canonical_semantic_vector_inputs.canonical_asset_id
					AND c.primary_source_key = representative_source_key
					AND c.primary_upstream_asset_id = representative_upstream_asset_id)`,
				input.updatedAt, input.canonicalID, input.modelID); err != nil {
				return err
			}
		}
	}
	if err := completeLocalScanJobInTx(ctx, tx, jobID, nowText); err != nil {
		return err
	}
	return s.catalog.commitCatalogAssetChanges(ctx, tx, changed > 0)
}

type localCaptureSemanticInput struct{ canonicalID, modelID, updatedAt string }

func currentLocalCaptureSemanticInputs(ctx context.Context, tx *sql.Tx, location localLocationForMetadata) ([]localCaptureSemanticInput, error) {
	rows, err := tx.QueryContext(ctx, `SELECT i.canonical_asset_id, i.model_id, i.updated_at
		FROM canonical_semantic_vector_inputs i JOIN catalog_assets a ON a.canonical_asset_id = i.canonical_asset_id
		WHERE a.source_key = ? AND a.upstream_asset_id = ? AND i.refresh_required = 0`, location.SourceKey, location.AssetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var inputs []localCaptureSemanticInput
	for rows.Next() {
		var input localCaptureSemanticInput
		if err := rows.Scan(&input.canonicalID, &input.modelID, &input.updatedAt); err != nil {
			return nil, err
		}
		inputs = append(inputs, input)
	}
	return inputs, rows.Err()
}

func (s *Service) revalidateLocalMetadataAfterInspection(
	ctx context.Context,
	trustedRoot *trustedLocalMediaRoot,
	jobID int64,
	location localLocationForMetadata,
	file *os.File,
	before os.FileInfo,
) error {
	after, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat local media after metadata inspection: %w", err)
	}
	if !localFileInfoUnchanged(before, after) {
		if err := s.resettleLocalMetadataJob(ctx, trustedRoot, jobID, location, after, "source_changed_during_metadata_inspection"); err != nil {
			return err
		}
		return errLocalMetadataSourceChanged
	}
	pathFile, pathInfo, pathnameMatches, err := reopenPinnedLocalRootFileAndMatch(trustedRoot.handle, location.RelativePath, after)
	if err != nil {
		return err
	}
	_ = pathFile.Close()
	if !pathnameMatches {
		if err := s.resettleLocalMetadataJob(ctx, trustedRoot, jobID, location, pathInfo, "source_path_replaced_during_video_inspection"); err != nil {
			return err
		}
		return errLocalMetadataSourceChanged
	}
	return nil
}
