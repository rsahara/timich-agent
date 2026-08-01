package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// LocalPhase0DiagnosticRow describes one known local filesystem location for
// Admin-only CSV diagnostics.
type LocalPhase0DiagnosticRow struct {
	SourceKey             string
	RootKey               string
	ScanRunID             string
	RootStatus            string
	Phase0Status          string
	RelativePath          string
	ReasonCode            string
	LocationStatus        string
	StatusReason          string
	SizeBytes             int64
	MTime                 string
	FastSignature         string
	SHA1Hex               string
	AssetID               string
	MediaType             string
	AssetVisibilityStatus string
	ThumbnailStatus       string
	CapturedAt            string
	FirstSeenAt           string
	LastSeenAt            string
	VerifiedAt            string
	UpdatedAt             string
}

// LocalFailureDiagnosticRow describes one failed local datasource item for
// Admin-only CSV diagnostics and support bundles.
type LocalFailureDiagnosticRow struct {
	SourceKey    string
	RootKey      string
	RelativePath string
	AssetID      string
	MediaType    string
	FailureKind  string
	Component    string
	Status       string
	Attempts     int
	LastError    string
	UpdatedAt    string
}

func LocalPhase0DiagnosticCSVHeader() []string {
	return []string{
		"source_key",
		"root_key",
		"scan_run_id",
		"root_status",
		"phase0_status",
		"relative_path",
		"reason_code",
		"location_status",
		"status_reason",
		"size_bytes",
		"mtime",
		"fast_signature",
		"sha1_hex",
		"asset_id",
		"media_type",
		"asset_visibility_status",
		"thumbnail_status",
		"captured_at",
		"first_seen_at",
		"last_seen_at",
		"verified_at",
		"updated_at",
	}
}

func LocalFailureDiagnosticCSVHeader() []string {
	return []string{
		"source_key",
		"root_key",
		"relative_path",
		"asset_id",
		"media_type",
		"failure_kind",
		"component",
		"status",
		"attempts",
		"last_error",
		"updated_at",
	}
}

func (row LocalPhase0DiagnosticRow) CSVRecord() []string {
	return []string{
		row.SourceKey,
		row.RootKey,
		row.ScanRunID,
		row.RootStatus,
		row.Phase0Status,
		row.RelativePath,
		row.ReasonCode,
		row.LocationStatus,
		row.StatusReason,
		strconv.FormatInt(row.SizeBytes, 10),
		row.MTime,
		row.FastSignature,
		row.SHA1Hex,
		row.AssetID,
		row.MediaType,
		row.AssetVisibilityStatus,
		row.ThumbnailStatus,
		row.CapturedAt,
		row.FirstSeenAt,
		row.LastSeenAt,
		row.VerifiedAt,
		row.UpdatedAt,
	}
}

func (row LocalFailureDiagnosticRow) CSVRecord() []string {
	return []string{
		row.SourceKey,
		row.RootKey,
		row.RelativePath,
		row.AssetID,
		row.MediaType,
		row.FailureKind,
		row.Component,
		row.Status,
		strconv.Itoa(row.Attempts),
		row.LastError,
		row.UpdatedAt,
	}
}

func (s *Service) LocalPhase0DiagnosticRows(ctx context.Context, sourceKey string) ([]LocalPhase0DiagnosticRow, error) {
	if s == nil || s.catalog == nil {
		return nil, ErrNoDatasourceConfigured
	}
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey != "" {
		if _, _, err := s.localDatasourceAndRoot(sourceKey); err != nil {
			return nil, err
		}
	} else if len(s.LocalDatasourceSourceKeys()) == 0 {
		return nil, ErrNoDatasourceConfigured
	}

	query := `SELECT
			l.source_key,
			l.root_key,
			rs.last_phase0_run_id,
			rs.root_status,
			rs.phase0_status,
			l.relative_path,
			l.status,
			l.status_reason,
			l.size_bytes,
			l.mtime,
			l.fast_signature,
			l.sha1_hex,
			l.asset_id,
			l.first_seen_at,
			l.last_seen_at,
			l.verified_at,
			l.updated_at,
			a.media_type,
			a.visibility_status,
			a.thumbnail_status,
			a.captured_at
		FROM local_asset_locations l
		LEFT JOIN local_scan_root_state rs
			ON rs.source_key = l.source_key AND rs.root_key = l.root_key
		LEFT JOIN local_assets a
			ON a.source_key = l.source_key AND a.asset_id = l.asset_id
		WHERE (? = '' OR l.source_key = ?)
		ORDER BY l.source_key, l.root_key, l.relative_path, l.id`
	rows, err := s.catalog.db.QueryContext(ctx, query, sourceKey, sourceKey)
	if err != nil {
		return nil, fmt.Errorf("query local phase0 diagnostics: %w", err)
	}
	defer rows.Close()

	diagnostics := []LocalPhase0DiagnosticRow{}
	for rows.Next() {
		var row LocalPhase0DiagnosticRow
		var scanRunID sql.NullInt64
		var rootStatus sql.NullString
		var phase0Status sql.NullString
		var statusReason sql.NullString
		var sha1Hex sql.NullString
		var assetID sql.NullString
		var verifiedAt sql.NullString
		var mediaType sql.NullString
		var visibilityStatus sql.NullString
		var thumbnailStatus sql.NullString
		var capturedAt sql.NullString
		if err := rows.Scan(
			&row.SourceKey,
			&row.RootKey,
			&scanRunID,
			&rootStatus,
			&phase0Status,
			&row.RelativePath,
			&row.LocationStatus,
			&statusReason,
			&row.SizeBytes,
			&row.MTime,
			&row.FastSignature,
			&sha1Hex,
			&assetID,
			&row.FirstSeenAt,
			&row.LastSeenAt,
			&verifiedAt,
			&row.UpdatedAt,
			&mediaType,
			&visibilityStatus,
			&thumbnailStatus,
			&capturedAt,
		); err != nil {
			return nil, fmt.Errorf("scan local phase0 diagnostic row: %w", err)
		}
		if scanRunID.Valid {
			row.ScanRunID = strconv.FormatInt(scanRunID.Int64, 10)
		}
		row.RootStatus = nullableString(rootStatus)
		row.Phase0Status = nullableString(phase0Status)
		row.StatusReason = nullableString(statusReason)
		row.SHA1Hex = nullableString(sha1Hex)
		row.AssetID = nullableString(assetID)
		row.VerifiedAt = nullableString(verifiedAt)
		row.MediaType = nullableString(mediaType)
		row.AssetVisibilityStatus = nullableString(visibilityStatus)
		row.ThumbnailStatus = nullableString(thumbnailStatus)
		row.CapturedAt = nullableString(capturedAt)
		row.ReasonCode = localPhase0DiagnosticReason(row.LocationStatus, row.StatusReason)
		diagnostics = append(diagnostics, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate local phase0 diagnostics: %w", err)
	}
	return diagnostics, nil
}

func (s *Service) LocalFailureDiagnosticRows(ctx context.Context, sourceKey string) ([]LocalFailureDiagnosticRow, error) {
	if s == nil || s.catalog == nil {
		return nil, ErrNoDatasourceConfigured
	}
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey != "" {
		if _, _, err := s.localDatasourceAndRoot(sourceKey); err != nil {
			return nil, err
		}
	} else if len(s.LocalDatasourceSourceKeys()) == 0 {
		return nil, ErrNoDatasourceConfigured
	}

	diagnostics := []LocalFailureDiagnosticRow{}
	queries := []string{
		`SELECT
			rs.source_key,
			rs.root_key,
			'',
			'',
			'',
			'media_discovery',
			'root',
			rs.root_status,
			0,
			COALESCE(rs.root_last_error, ''),
			rs.updated_at
		FROM local_scan_root_state rs
		WHERE (? = '' OR rs.source_key = ?)
			AND (
				COALESCE(TRIM(rs.root_last_error), '') != ''
				OR rs.root_status IN ('missing', 'not_directory', 'unreadable', 'failed')
				OR rs.phase0_status = 'failed'
			)`,
		`SELECT
			j.source_key,
			COALESCE(loc_id.root_key, MIN(loc_asset.root_key), j.root_key, ''),
			COALESCE(loc_id.relative_path, MIN(loc_asset.relative_path), ''),
			COALESCE(j.asset_id, ''),
			COALESCE(a.media_type, ''),
			j.job_kind,
			'job:' || j.id,
			j.status,
			j.attempts,
			COALESCE(j.last_error, ''),
			COALESCE(j.completed_at, j.locked_at, j.scheduled_at, '')
		FROM local_scan_jobs j
		JOIN local_scan_root_state current_root
			ON current_root.source_key = j.source_key
			AND current_root.root_key = j.root_key
			AND current_root.root_generation = j.root_generation
		LEFT JOIN local_asset_locations loc_id
			ON loc_id.id = j.location_id
		LEFT JOIN local_asset_locations loc_asset
			ON loc_asset.source_key = j.source_key
			AND loc_asset.asset_id = j.asset_id
			AND loc_asset.status = 'active'
		LEFT JOIN local_assets a
			ON a.source_key = j.source_key
			AND a.asset_id = j.asset_id
		WHERE (? = '' OR j.source_key = ?)
			AND j.status = 'failed'
			AND j.job_kind != 'thumbnail'
		GROUP BY j.id`,
		`SELECT
			a.source_key,
			COALESCE(MIN(l.root_key), ''),
			COALESCE(MIN(l.relative_path), ''),
			a.asset_id,
			a.media_type,
			'thumbnail',
			COALESCE(fr.components, 'rendition'),
			a.thumbnail_status,
			0,
			COALESCE(fr.last_error, ''),
			a.updated_at
		FROM local_assets a
		LEFT JOIN local_asset_locations l
			ON l.source_key = a.source_key
			AND l.asset_id = a.asset_id
			AND l.status = 'active'
		LEFT JOIN (
			SELECT source_key, asset_id, group_concat(kind, '+') AS components, MAX(COALESCE(last_error, '')) AS last_error
			FROM local_renditions
			WHERE status = 'failed'
			GROUP BY source_key, asset_id
		) fr
			ON fr.source_key = a.source_key
			AND fr.asset_id = a.asset_id
		WHERE (? = '' OR a.source_key = ?)
			AND a.thumbnail_status = 'failed'
		GROUP BY a.source_key, a.asset_id`,
		`SELECT
			l.source_key,
			l.root_key,
			l.relative_path,
			COALESCE(l.asset_id, ''),
			COALESCE(a.media_type, ''),
			'content_verification',
			'sha1',
			'failed',
			1,
			COALESCE(l.content_verification_error, ''),
			COALESCE(l.content_verification_attempted_at, l.updated_at)
		FROM local_asset_locations l
		LEFT JOIN local_assets a
			ON a.source_key = l.source_key
			AND a.asset_id = l.asset_id
		WHERE (? = '' OR l.source_key = ?)
			AND l.status = 'active'
			AND l.content_verification_error IS NOT NULL`,
		`SELECT
			v.source_key,
			COALESCE(MIN(l.root_key), ''),
			COALESCE(MIN(l.relative_path), ''),
			v.upstream_asset_id,
			a.media_type,
			'embedding',
			v.model_id,
			v.status,
			0,
			COALESCE(v.last_error, ''),
			COALESCE(v.generated_at, '')
		FROM semantic_vectors v
		JOIN local_assets a
			ON a.source_key = v.source_key
			AND a.asset_id = v.upstream_asset_id
		LEFT JOIN local_asset_locations l
			ON l.source_key = a.source_key
			AND l.asset_id = a.asset_id
			AND l.status = 'active'
		WHERE (? = '' OR v.source_key = ?)
			AND v.status = 'failed'
		GROUP BY v.source_key, v.upstream_asset_id, v.model_id`,
	}
	for _, query := range queries {
		rows, err := s.catalog.db.QueryContext(ctx, query, sourceKey, sourceKey)
		if err != nil {
			return nil, fmt.Errorf("query local failure diagnostics: %w", err)
		}
		if err := appendLocalFailureDiagnosticRows(rows, &diagnostics); err != nil {
			return nil, err
		}
	}
	return diagnostics, nil
}

func appendLocalFailureDiagnosticRows(rows *sql.Rows, diagnostics *[]LocalFailureDiagnosticRow) error {
	defer rows.Close()
	for rows.Next() {
		var row LocalFailureDiagnosticRow
		var rootKey sql.NullString
		var relativePath sql.NullString
		var assetID sql.NullString
		var mediaType sql.NullString
		var lastError sql.NullString
		var updatedAt sql.NullString
		if err := rows.Scan(
			&row.SourceKey,
			&rootKey,
			&relativePath,
			&assetID,
			&mediaType,
			&row.FailureKind,
			&row.Component,
			&row.Status,
			&row.Attempts,
			&lastError,
			&updatedAt,
		); err != nil {
			return fmt.Errorf("scan local failure diagnostic row: %w", err)
		}
		row.RootKey = nullableString(rootKey)
		row.RelativePath = nullableString(relativePath)
		row.AssetID = nullableString(assetID)
		row.MediaType = nullableString(mediaType)
		row.LastError = nullableString(lastError)
		row.UpdatedAt = nullableString(updatedAt)
		*diagnostics = append(*diagnostics, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate local failure diagnostics: %w", err)
	}
	return nil
}

func nullableString(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func localPhase0DiagnosticReason(locationStatus string, statusReason string) string {
	statusReason = strings.TrimSpace(statusReason)
	if statusReason != "" {
		return statusReason
	}
	switch strings.TrimSpace(locationStatus) {
	case "active":
		return "active"
	case "discovered":
		return "metadata_pending"
	case "missing":
		return "phase0_absent"
	case "permission_blocked":
		return "phase0_read_error"
	case "failed":
		return "failed"
	default:
		return strings.TrimSpace(locationStatus)
	}
}
