package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CatalogDeduplicationStatus summarizes the source-row to canonical-asset map.
type CatalogDeduplicationStatus struct {
	SourceRows          int  `json:"sourceRows"`
	CanonicalAssets     int  `json:"canonicalAssets"`
	ActiveAssets        int  `json:"activeAssets"`
	DuplicateSourceRows int  `json:"duplicateSourceRows"`
	UnlinkedSourceRows  int  `json:"unlinkedSourceRows"`
	OrphanCanonicalRows int  `json:"orphanCanonicalRows"`
	NeedsRepair         bool `json:"needsRepair"`
}

type catalogCanonicalSourceRow struct {
	SourceKey                 string
	DatasourceKind            string
	UpstreamAssetID           string
	CanonicalAssetID          string
	UpstreamChecksumAlgorithm string
	ContentSHA1Hex            sql.NullString
	ContentSizeBytes          sql.NullInt64
	CanonicalContentSHA1Hex   sql.NullString
	CanonicalContentSizeBytes sql.NullInt64
	MediaType                 string
	Filename                  string
	CapturedAtText            string
	Duration                  sql.NullString
	VisibilityStatus          string
	IsFavorite                bool
	PlaceLabel                sql.NullString
	Description               sql.NullString
	FirstSeenAtText           string
	UpdatedAtText             string
}

type catalogMediaSource struct {
	SourceKey       string
	UpstreamAssetID string
}

func (s *Service) CatalogDeduplicationStatus(ctx context.Context) (CatalogDeduplicationStatus, error) {
	if s == nil || s.catalog == nil {
		return CatalogDeduplicationStatus{}, ErrCatalogNotConfigured
	}
	return s.catalog.CatalogDeduplicationStatus(ctx)
}

func (s *Service) RebuildCatalogCanonicalAssets(ctx context.Context) (CatalogDeduplicationStatus, error) {
	if s == nil || s.catalog == nil {
		return CatalogDeduplicationStatus{}, ErrCatalogNotConfigured
	}
	return s.catalog.RebuildCatalogCanonicalAssets(ctx)
}

func (s *CatalogStore) CatalogDeduplicationStatus(ctx context.Context) (CatalogDeduplicationStatus, error) {
	if s == nil || s.db == nil {
		return CatalogDeduplicationStatus{}, ErrCatalogNotConfigured
	}
	db := s.queryDB()
	var status CatalogDeduplicationStatus
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_assets`).Scan(&status.SourceRows); err != nil {
		return CatalogDeduplicationStatus{}, fmt.Errorf("count catalog source rows: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_canonical_assets`).Scan(&status.CanonicalAssets); err != nil {
		return CatalogDeduplicationStatus{}, fmt.Errorf("count catalog canonical assets: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_canonical_assets WHERE visibility_status = 'active'`).Scan(&status.ActiveAssets); err != nil {
		return CatalogDeduplicationStatus{}, fmt.Errorf("count active catalog canonical assets: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(duplicate_source_count), 0) FROM catalog_canonical_assets`).Scan(&status.DuplicateSourceRows); err != nil {
		return CatalogDeduplicationStatus{}, fmt.Errorf("count duplicate catalog sources: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM catalog_assets a
		LEFT JOIN catalog_canonical_assets c ON c.canonical_asset_id = a.canonical_asset_id
		WHERE a.canonical_asset_id IS NULL OR c.canonical_asset_id IS NULL`).Scan(&status.UnlinkedSourceRows); err != nil {
		return CatalogDeduplicationStatus{}, fmt.Errorf("count unlinked catalog sources: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM catalog_canonical_assets c
		LEFT JOIN catalog_assets a ON a.canonical_asset_id = c.canonical_asset_id
		WHERE a.canonical_asset_id IS NULL`).Scan(&status.OrphanCanonicalRows); err != nil {
		return CatalogDeduplicationStatus{}, fmt.Errorf("count orphan catalog canonical rows: %w", err)
	}
	status.NeedsRepair = status.UnlinkedSourceRows > 0 || status.OrphanCanonicalRows > 0
	return status, nil
}

func (s *CatalogStore) RebuildCatalogCanonicalAssets(ctx context.Context) (CatalogDeduplicationStatus, error) {
	if s == nil || s.db == nil {
		return CatalogDeduplicationStatus{}, ErrCatalogNotConfigured
	}
	nowText := formatCatalogTime(time.Now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CatalogDeduplicationStatus{}, fmt.Errorf("begin catalog canonical rebuild: %w", err)
	}
	if err := s.rebuildCatalogCanonicalAssetsInTx(ctx, tx, nowText); err != nil {
		_ = tx.Rollback()
		return CatalogDeduplicationStatus{}, err
	}
	if err := s.commitCatalogAssetChanges(ctx, tx, true); err != nil {
		return CatalogDeduplicationStatus{}, fmt.Errorf("commit catalog canonical rebuild: %w", err)
	}
	return s.CatalogDeduplicationStatus(ctx)
}

func (s *CatalogStore) catalogCanonicalLinksNeedRepair(ctx context.Context) (bool, error) {
	var unlinked int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1
		FROM catalog_assets a
		LEFT JOIN catalog_canonical_assets c ON c.canonical_asset_id = a.canonical_asset_id
		WHERE a.canonical_asset_id IS NULL OR c.canonical_asset_id IS NULL
		LIMIT 1
	)`).Scan(&unlinked); err != nil {
		return false, fmt.Errorf("check unlinked catalog sources: %w", err)
	}
	if unlinked != 0 {
		return true, nil
	}
	var orphan int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1
		FROM catalog_canonical_assets c
		LEFT JOIN catalog_assets a ON a.canonical_asset_id = c.canonical_asset_id
		WHERE a.canonical_asset_id IS NULL
		LIMIT 1
	)`).Scan(&orphan); err != nil {
		return false, fmt.Errorf("check orphan catalog canonical assets: %w", err)
	}
	return orphan != 0, nil
}

func (s *CatalogStore) rebuildCatalogCanonicalAssetsInTx(ctx context.Context, tx *sql.Tx, nowText string) error {
	rows, err := tx.QueryContext(ctx, `SELECT source_key, datasource_kind, upstream_asset_id, COALESCE(canonical_asset_id, ''),
			upstream_checksum_algorithm, content_sha1_hex, content_size_bytes,
			canonical_content_sha1_hex, canonical_content_size_bytes,
			media_type, filename, captured_at, duration,
			visibility_status, is_favorite, place_label, description, first_seen_at, updated_at
		FROM catalog_assets`)
	if err != nil {
		return fmt.Errorf("query catalog source rows for canonical rebuild: %w", err)
	}
	defer rows.Close()

	ids := map[string]struct{}{}
	sources := []catalogCanonicalSourceRow{}
	for rows.Next() {
		row, err := scanCatalogCanonicalSourceRow(rows)
		if err != nil {
			return err
		}
		sources = append(sources, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate catalog source rows for canonical rebuild: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close catalog source rows for canonical rebuild: %w", err)
	}
	for _, row := range sources {
		id := catalogCanonicalAssetID(row)
		ids[id] = struct{}{}
		if row.CanonicalAssetID != id {
			if _, err := tx.ExecContext(ctx, `UPDATE catalog_assets
				SET canonical_asset_id = ?, updated_at = ?
				WHERE source_key = ? AND upstream_asset_id = ?`,
				id, nowText, row.SourceKey, row.UpstreamAssetID); err != nil {
				return fmt.Errorf("update catalog canonical link: %w", err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_canonical_assets`); err != nil {
		return fmt.Errorf("clear catalog canonical assets: %w", err)
	}
	return s.rebuildCatalogCanonicalIDsInTx(ctx, tx, ids, nowText)
}

func (s *CatalogStore) refreshCatalogCanonicalAssetInTx(ctx context.Context, tx *sql.Tx, sourceKey string, upstreamAssetID string, nowText string) error {
	row := tx.QueryRowContext(ctx, `SELECT source_key, datasource_kind, upstream_asset_id, COALESCE(canonical_asset_id, ''),
			upstream_checksum_algorithm, content_sha1_hex, content_size_bytes,
			canonical_content_sha1_hex, canonical_content_size_bytes,
			media_type, filename, captured_at, duration,
			visibility_status, is_favorite, place_label, description, first_seen_at, updated_at
		FROM catalog_assets
		WHERE source_key = ? AND upstream_asset_id = ?`, strings.TrimSpace(sourceKey), strings.TrimSpace(upstreamAssetID))
	source, err := scanCatalogCanonicalSourceRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	ids := map[string]struct{}{}
	if source.CanonicalAssetID != "" {
		ids[source.CanonicalAssetID] = struct{}{}
	}
	id := catalogCanonicalAssetID(source)
	ids[id] = struct{}{}
	if source.CanonicalAssetID != id {
		if _, err := tx.ExecContext(ctx, `UPDATE catalog_assets
			SET canonical_asset_id = ?, updated_at = ?
			WHERE source_key = ? AND upstream_asset_id = ?`,
			id, nowText, source.SourceKey, source.UpstreamAssetID); err != nil {
			return fmt.Errorf("update catalog canonical asset link: %w", err)
		}
	}
	return s.rebuildCatalogCanonicalIDsInTx(ctx, tx, ids, nowText)
}

func (s *CatalogStore) rebuildCatalogCanonicalIDsInTx(ctx context.Context, tx *sql.Tx, ids map[string]struct{}, nowText string) error {
	if len(ids) == 0 {
		return nil
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		if strings.TrimSpace(id) != "" {
			ordered = append(ordered, id)
		}
	}
	sort.Strings(ordered)
	for _, id := range ordered {
		if err := s.rebuildCatalogCanonicalIDInTx(ctx, tx, id, nowText); err != nil {
			return err
		}
	}
	return nil
}

func (s *CatalogStore) rebuildCatalogCanonicalIDInTx(ctx context.Context, tx *sql.Tx, id string, nowText string) error {
	rows, err := tx.QueryContext(ctx, `SELECT source_key, datasource_kind, upstream_asset_id, COALESCE(canonical_asset_id, ''),
			upstream_checksum_algorithm, content_sha1_hex, content_size_bytes,
			canonical_content_sha1_hex, canonical_content_size_bytes,
			media_type, filename, captured_at, duration,
			visibility_status, is_favorite, place_label, description, first_seen_at, updated_at
		FROM catalog_assets
		WHERE canonical_asset_id = ?`, id)
	if err != nil {
		return fmt.Errorf("query catalog canonical sources: %w", err)
	}
	defer rows.Close()

	sources := []catalogCanonicalSourceRow{}
	for rows.Next() {
		row, err := scanCatalogCanonicalSourceRow(rows)
		if err != nil {
			return err
		}
		sources = append(sources, row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate catalog canonical sources: %w", err)
	}
	if len(sources) == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_canonical_assets WHERE canonical_asset_id = ?`, id); err != nil {
			return fmt.Errorf("delete empty catalog canonical asset: %w", err)
		}
		return nil
	}

	sort.SliceStable(sources, func(i, j int) bool {
		leftRank := catalogVisibilityRank(sources[i].VisibilityStatus)
		rightRank := catalogVisibilityRank(sources[j].VisibilityStatus)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		leftSourceRank := catalogDatasourcePreferenceRank(sources[i].DatasourceKind)
		rightSourceRank := catalogDatasourcePreferenceRank(sources[j].DatasourceKind)
		if leftSourceRank != rightSourceRank {
			return leftSourceRank < rightSourceRank
		}
		if sources[i].CapturedAtText != sources[j].CapturedAtText {
			return sources[i].CapturedAtText > sources[j].CapturedAtText
		}
		if sources[i].SourceKey != sources[j].SourceKey {
			return sources[i].SourceKey < sources[j].SourceKey
		}
		return sources[i].UpstreamAssetID < sources[j].UpstreamAssetID
	})
	primary := sources[0]
	primaryContentSHA1Hex, primaryContentSizeBytes := catalogCanonicalContentIdentity(primary)
	firstSeen := primary.FirstSeenAtText
	isFavorite := primary.IsFavorite
	for _, source := range sources {
		if source.FirstSeenAtText < firstSeen {
			firstSeen = source.FirstSeenAtText
		}
		isFavorite = isFavorite || source.IsFavorite
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO catalog_canonical_assets (
			canonical_asset_id, content_sha1_hex, content_size_bytes, media_type, filename,
			captured_at, duration, visibility_status, primary_source_key, primary_upstream_asset_id,
			source_count, duplicate_source_count, is_favorite, place_label, description,
			first_seen_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(canonical_asset_id) DO UPDATE SET
			content_sha1_hex = excluded.content_sha1_hex,
			content_size_bytes = excluded.content_size_bytes,
			media_type = excluded.media_type,
			filename = excluded.filename,
			captured_at = excluded.captured_at,
			duration = excluded.duration,
			visibility_status = excluded.visibility_status,
			primary_source_key = excluded.primary_source_key,
			primary_upstream_asset_id = excluded.primary_upstream_asset_id,
			source_count = excluded.source_count,
			duplicate_source_count = excluded.duplicate_source_count,
			is_favorite = excluded.is_favorite,
			place_label = excluded.place_label,
			description = excluded.description,
			updated_at = excluded.updated_at`,
		id,
		nullStringToAny(primaryContentSHA1Hex),
		nullInt64ToAny(primaryContentSizeBytes),
		primary.MediaType,
		primary.Filename,
		primary.CapturedAtText,
		primary.Duration,
		primary.VisibilityStatus,
		primary.SourceKey,
		primary.UpstreamAssetID,
		len(sources),
		max(0, len(sources)-1),
		boolToSQLiteInt(isFavorite),
		primary.PlaceLabel,
		primary.Description,
		firstSeen,
		nowText,
	)
	if err != nil {
		return fmt.Errorf("upsert catalog canonical asset: %w", err)
	}
	return nil
}

func catalogCanonicalAssetID(row catalogCanonicalSourceRow) string {
	sha1Value, sizeValue := catalogCanonicalContentIdentity(row)
	sha1Hex := normalizeCatalogSHA1Hex(nullStringValue(sha1Value))
	if sha1Hex != "" && sizeValue.Valid && sizeValue.Int64 > 0 {
		return hashCatalogCanonicalID("content", row.MediaType, sha1Hex, strconv.FormatInt(sizeValue.Int64, 10))
	}
	return hashCatalogCanonicalID("source", row.SourceKey, row.UpstreamAssetID)
}

func catalogCanonicalContentIdentity(row catalogCanonicalSourceRow) (sql.NullString, sql.NullInt64) {
	if normalizeCatalogSHA1Hex(nullStringValue(row.CanonicalContentSHA1Hex)) != "" &&
		row.CanonicalContentSizeBytes.Valid && row.CanonicalContentSizeBytes.Int64 > 0 {
		return row.CanonicalContentSHA1Hex, row.CanonicalContentSizeBytes
	}
	algorithm := strings.TrimSpace(row.UpstreamChecksumAlgorithm)
	if algorithm == "" || algorithm == upstreamChecksumAlgorithmSHA1 {
		return row.ContentSHA1Hex, row.ContentSizeBytes
	}
	return sql.NullString{}, sql.NullInt64{}
}

func hashCatalogCanonicalID(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		hash.Write([]byte(strings.TrimSpace(part)))
		hash.Write([]byte{0})
	}
	return "ca1_" + hex.EncodeToString(hash.Sum(nil))
}

func normalizeCatalogSHA1Hex(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 40 {
		return ""
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 20 {
		return ""
	}
	return value
}

func scanCatalogCanonicalSourceRow(scanner interface {
	Scan(dest ...any) error
}) (catalogCanonicalSourceRow, error) {
	var row catalogCanonicalSourceRow
	var isFavorite int
	if err := scanner.Scan(
		&row.SourceKey,
		&row.DatasourceKind,
		&row.UpstreamAssetID,
		&row.CanonicalAssetID,
		&row.UpstreamChecksumAlgorithm,
		&row.ContentSHA1Hex,
		&row.ContentSizeBytes,
		&row.CanonicalContentSHA1Hex,
		&row.CanonicalContentSizeBytes,
		&row.MediaType,
		&row.Filename,
		&row.CapturedAtText,
		&row.Duration,
		&row.VisibilityStatus,
		&isFavorite,
		&row.PlaceLabel,
		&row.Description,
		&row.FirstSeenAtText,
		&row.UpdatedAtText,
	); err != nil {
		return catalogCanonicalSourceRow{}, err
	}
	row.IsFavorite = isFavorite != 0
	return row, nil
}

func catalogVisibilityRank(status string) int {
	switch strings.TrimSpace(status) {
	case "active":
		return 0
	case "permission_blocked":
		return 1
	case "out_of_scope":
		return 2
	case "missing":
		return 3
	default:
		return 4
	}
}

func catalogDatasourcePreferenceRank(kind string) int {
	switch strings.TrimSpace(kind) {
	case "local_filesystem":
		return 0
	case "immich":
		return 1
	default:
		return 2
	}
}

func nullStringValue(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func nullStringToAny(value sql.NullString) any {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	return strings.TrimSpace(value.String)
}

func nullInt64ToAny(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}

func (s *CatalogStore) canonicalAssetsForScoredSources(ctx context.Context, scored []semanticScoredAsset, includeSemanticScores bool) ([]Asset, error) {
	if len(scored) == 0 {
		return nil, nil
	}
	const chunkSize = 200
	readiness := s.galleryReadinessSnapshot()
	seen := map[string]struct{}{}
	items := make([]Asset, 0, len(scored))
	for start := 0; start < len(scored); start += chunkSize {
		end := min(start+chunkSize, len(scored))
		chunk, err := s.canonicalAssetsForScoredSourceChunk(ctx, scored[start:end], includeSemanticScores, start, seen, readiness)
		if err != nil {
			return nil, err
		}
		items = append(items, chunk...)
	}
	return items, nil
}

func (s *CatalogStore) canonicalAssetsForScoredSourceChunk(ctx context.Context, scored []semanticScoredAsset, includeSemanticScores bool, scoreOffset int, seen map[string]struct{}, readiness catalogGalleryReadiness) ([]Asset, error) {
	var builder strings.Builder
	args := make([]any, 0, len(scored)*4)
	canonicalColumn := func(name string) string {
		return "c." + name
	}
	sourceKeyColumn, upstreamAssetIDColumn, sourceArgs := catalogGallerySourceProjection(canonicalColumn, readiness)
	builder.WriteString(`WITH requested(source_key, upstream_asset_id, score_order, semantic_score) AS (VALUES `)
	for index, candidate := range scored {
		if index > 0 {
			builder.WriteString(",")
		}
		builder.WriteString("(?, ?, ?, ?)")
		args = append(args, candidate.Asset.SourceKey, candidate.Asset.ID, scoreOffset+index, float64(candidate.Similarity))
	}
	builder.WriteString(`)
		SELECT r.score_order, r.semantic_score,
			c.canonical_asset_id, ` + sourceKeyColumn + `, ` + upstreamAssetIDColumn + `,
			c.media_type, c.filename, c.captured_at, c.duration
		FROM requested r
		JOIN catalog_assets a
			ON a.source_key = r.source_key AND a.upstream_asset_id = r.upstream_asset_id
		JOIN catalog_canonical_assets c
			ON c.canonical_asset_id = a.canonical_asset_id
		WHERE c.visibility_status = 'active'
			AND `)
	args = append(args, sourceArgs...)
	readinessClause, readinessArgs := catalogGalleryReadinessClause(canonicalColumn, readiness)
	builder.WriteString(readinessClause)
	builder.WriteString(`
		ORDER BY r.score_order ASC`)
	args = append(args, readinessArgs...)
	db := s.queryDB()
	rows, err := db.QueryContext(ctx, builder.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("read catalog canonical semantic results: %w", err)
	}
	defer rows.Close()

	items := make([]Asset, 0, len(scored))
	for rows.Next() {
		var scoreOrder int
		var semanticScore sql.NullFloat64
		var canonicalID string
		var asset Asset
		var capturedAtText string
		var duration sql.NullString
		if err := rows.Scan(&scoreOrder, &semanticScore, &canonicalID, &asset.SourceKey, &asset.ID, &asset.Type, &asset.Filename, &capturedAtText, &duration); err != nil {
			return nil, fmt.Errorf("scan catalog canonical semantic result: %w", err)
		}
		if _, ok := seen[canonicalID]; ok {
			continue
		}
		capturedAt, err := time.Parse(time.RFC3339Nano, capturedAtText)
		if err != nil {
			return nil, fmt.Errorf("parse catalog canonical semantic result captured_at: %w", err)
		}
		asset.CapturedAt = capturedAt.UTC()
		if duration.Valid {
			value := duration.String
			asset.Duration = &value
		}
		seen[canonicalID] = struct{}{}
		if includeSemanticScores && semanticScore.Valid {
			score := float32(semanticScore.Float64)
			asset.SemanticScore = &score
		}
		items = append(items, asset)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate catalog canonical semantic results: %w", err)
	}
	return items, nil
}

func (s *CatalogStore) catalogCanonicalAssetForSource(ctx context.Context, sourceKey string, upstreamAssetID string) (string, Asset, error) {
	db := s.queryDB()
	row := db.QueryRowContext(ctx, `SELECT c.canonical_asset_id, c.primary_source_key, c.primary_upstream_asset_id,
			c.media_type, c.filename, c.captured_at, c.duration
		FROM catalog_assets a
		JOIN catalog_canonical_assets c ON c.canonical_asset_id = a.canonical_asset_id
		WHERE a.source_key = ? AND a.upstream_asset_id = ? AND c.visibility_status = 'active'`,
		strings.TrimSpace(sourceKey), strings.TrimSpace(upstreamAssetID))
	var canonicalID string
	var asset Asset
	var capturedAtText string
	var duration sql.NullString
	if err := row.Scan(&canonicalID, &asset.SourceKey, &asset.ID, &asset.Type, &asset.Filename, &capturedAtText, &duration); err != nil {
		if err == sql.ErrNoRows {
			return "", Asset{}, nil
		}
		return "", Asset{}, fmt.Errorf("read catalog canonical asset: %w", err)
	}
	capturedAt, err := time.Parse(time.RFC3339Nano, capturedAtText)
	if err != nil {
		return "", Asset{}, fmt.Errorf("parse catalog canonical captured_at: %w", err)
	}
	asset.CapturedAt = capturedAt.UTC()
	if duration.Valid {
		value := duration.String
		asset.Duration = &value
	}
	return canonicalID, asset, nil
}

func (s *CatalogStore) activeImmichSourcesForCanonicalAsset(ctx context.Context, sourceKey string, upstreamAssetID string) ([]catalogMediaSource, error) {
	if s == nil || s.db == nil {
		return nil, ErrCatalogNotConfigured
	}
	db := s.queryDB()
	rows, err := db.QueryContext(ctx, `SELECT candidate.source_key, candidate.upstream_asset_id
		FROM catalog_assets requested
		JOIN catalog_canonical_assets canonical
			ON canonical.canonical_asset_id = requested.canonical_asset_id
		JOIN catalog_assets candidate
			ON candidate.canonical_asset_id = requested.canonical_asset_id
		WHERE requested.source_key = ?
			AND requested.upstream_asset_id = ?
			AND canonical.visibility_status = 'active'
			AND candidate.datasource_kind = 'immich'
			AND candidate.visibility_status = 'active'
		ORDER BY candidate.source_key ASC, candidate.upstream_asset_id ASC`,
		strings.TrimSpace(sourceKey), strings.TrimSpace(upstreamAssetID))
	if err != nil {
		return nil, fmt.Errorf("read catalog canonical immich media fallbacks: %w", err)
	}
	defer rows.Close()

	sources := []catalogMediaSource{}
	for rows.Next() {
		var source catalogMediaSource
		if err := rows.Scan(&source.SourceKey, &source.UpstreamAssetID); err != nil {
			return nil, fmt.Errorf("scan catalog canonical immich media fallback: %w", err)
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate catalog canonical immich media fallbacks: %w", err)
	}
	return sources, nil
}
