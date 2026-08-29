package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	catalogGalleryProjectionStateID               = 1
	catalogGalleryProjectionVersion               = 2
	catalogGalleryProjectionDayIndexStateID       = 1
	catalogGalleryProjectionDayIndexSchemaVersion = 1
)

const galleryProjectionSelectColumns = `source_key, upstream_asset_id,
	media_type, filename, captured_at, duration`

const galleryProjectionDayAtOffsetSQL = `WITH positioned_days AS (
		SELECT captured_day, item_count,
			COALESCE(SUM(item_count) OVER (
				ORDER BY captured_day DESC
				ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING
			), 0) AS start_position
		FROM catalog_gallery_projection_days
	)
	SELECT captured_day, start_position
	FROM positioned_days
	WHERE start_position <= ?
		AND ? < start_position + item_count
	ORDER BY captured_day DESC
	LIMIT 1`

const galleryProjectionAnchorWithinDaySQL = `SELECT captured_at, canonical_asset_id
	FROM catalog_gallery_projection
	WHERE captured_at >= ? AND captured_at < ?
	ORDER BY captured_at DESC, canonical_asset_id ASC
	LIMIT 1 OFFSET ?`

const galleryProjectionAfterAnchorSQL = `SELECT ` + galleryProjectionSelectColumns + `
	FROM catalog_gallery_projection
	WHERE captured_at <= ?
		AND (captured_at < ? OR canonical_asset_id >= ?)
	ORDER BY captured_at DESC, canonical_asset_id ASC
	LIMIT ?`

func normalizedGallerySourceKeys(keys []string) []string {
	normalized := append([]string(nil), keys...)
	for index := range normalized {
		normalized[index] = strings.TrimSpace(normalized[index])
	}
	sort.Strings(normalized)
	unique := normalized[:0]
	for _, key := range normalized {
		if key == "" || (len(unique) > 0 && unique[len(unique)-1] == key) {
			continue
		}
		unique = append(unique, key)
	}
	return unique
}

func catalogGalleryProjectionScopeKey(readiness catalogGalleryReadiness) (string, bool) {
	if readiness.immichOnly {
		return "", false
	}
	localKeys := normalizedGallerySourceKeys(readiness.localSourceKeys)
	immichKeys := normalizedGallerySourceKeys(readiness.immichSourceKeys)
	fallbackKeys := normalizedGallerySourceKeys(readiness.localImmichFallbackSourceKeys)
	if len(localKeys) == 0 && len(immichKeys) == 0 {
		return "", false
	}
	return fmt.Sprintf("mixed-v%d;local=", catalogGalleryProjectionVersion) + strings.Join(localKeys, ",") +
		";immich=" + strings.Join(immichKeys, ",") +
		";fallback=" + strings.Join(fallbackKeys, ","), true
}

func galleryProjectionInsertSQL(canonicalPredicate string) string {
	return `INSERT INTO catalog_gallery_projection (
			canonical_asset_id, source_key, upstream_asset_id, media_type,
			filename, captured_at, duration
		)
		WITH target_canonical AS (
			SELECT c.canonical_asset_id, c.media_type, c.filename, c.captured_at, c.duration
			FROM catalog_canonical_assets c
			JOIN catalog_gallery_projection_state state ON state.singleton_id = 1
			WHERE (` + canonicalPredicate + `)
				AND c.visibility_status = 'active'
		),
		ranked_sources AS (
			SELECT source.canonical_asset_id, source.source_key, source.upstream_asset_id,
				source.datasource_kind,
				ROW_NUMBER() OVER (
					PARTITION BY source.canonical_asset_id
					ORDER BY CASE source.datasource_kind
						WHEN 'local_filesystem' THEN 0
						ELSE 1
					END,
						source.source_key,
						source.upstream_asset_id
				) AS source_rank
			FROM catalog_assets source
			JOIN target_canonical target
			  ON target.canonical_asset_id = source.canonical_asset_id
			JOIN catalog_gallery_projection_sources configured
			  ON configured.source_key = source.source_key
			 AND ((configured.role = 'local' AND source.datasource_kind = 'local_filesystem')
				OR (configured.role = 'immich' AND source.datasource_kind = 'immich'))
			WHERE source.visibility_status = 'active'
		),
		selected_source AS (
			SELECT canonical_asset_id, source_key, upstream_asset_id, datasource_kind
			FROM ranked_sources
			WHERE source_rank = 1
		)
		SELECT target.canonical_asset_id, selected.source_key, selected.upstream_asset_id,
			target.media_type, target.filename, target.captured_at, target.duration
		FROM target_canonical target
		JOIN selected_source selected
		  ON selected.canonical_asset_id = target.canonical_asset_id
		WHERE selected.datasource_kind <> 'local_filesystem'
			OR EXISTS (
				SELECT 1
				FROM catalog_gallery_projection_sources configured_fallback
				WHERE configured_fallback.role = 'local_fallback'
					AND configured_fallback.source_key = selected.source_key
					AND EXISTS (
						SELECT 1
						FROM ranked_sources fallback_source
						WHERE fallback_source.canonical_asset_id = target.canonical_asset_id
							AND fallback_source.datasource_kind = 'immich'
					)
			)
			OR (
				SELECT COUNT(DISTINCT rendition.kind)
				FROM local_renditions rendition
				WHERE rendition.source_key = selected.source_key
					AND rendition.asset_id = selected.upstream_asset_id
					AND rendition.kind IN ('preview', 'detail_preview')
					AND rendition.status = 'ready'
					AND rendition.relative_path IS NOT NULL
					AND trim(rendition.relative_path) <> ''
					AND rendition.source_sha1_hex = (
						SELECT local_asset.sha1_hex
						FROM local_assets local_asset
						WHERE local_asset.source_key = selected.source_key
							AND local_asset.asset_id = selected.upstream_asset_id
							AND local_asset.visibility_status = 'active'
							AND local_asset.thumbnail_status = 'ready'
					)
			) = 2`
}

func galleryProjectionTriggerSQL(name string, event string, table string, affected string, predicate string) string {
	return `CREATE TRIGGER IF NOT EXISTS ` + name + `
		AFTER ` + event + ` ON ` + table + `
		WHEN EXISTS (
			SELECT 1 FROM catalog_gallery_projection_state WHERE singleton_id = 1
		)
		BEGIN
			DELETE FROM catalog_gallery_projection
			WHERE canonical_asset_id IN (` + affected + `);
			` + galleryProjectionInsertSQL(predicate) + `;
		END`
}

func galleryProjectionDayInsertTriggerSQL() string {
	return `CREATE TRIGGER IF NOT EXISTS trg_catalog_gallery_projection_day_insert_v1
		AFTER INSERT ON catalog_gallery_projection
		BEGIN
			INSERT INTO catalog_gallery_projection_days(captured_day, item_count)
			VALUES (substr(NEW.captured_at, 1, 10), 1)
			ON CONFLICT(captured_day) DO UPDATE SET
				item_count = item_count + 1;
		END`
}

func galleryProjectionDayDeleteTriggerSQL() string {
	return `CREATE TRIGGER IF NOT EXISTS trg_catalog_gallery_projection_day_delete_v1
		AFTER DELETE ON catalog_gallery_projection
		BEGIN
			DELETE FROM catalog_gallery_projection_days
			WHERE captured_day = substr(OLD.captured_at, 1, 10)
				AND item_count = 1;
			UPDATE catalog_gallery_projection_days
			SET item_count = item_count - 1
			WHERE captured_day = substr(OLD.captured_at, 1, 10)
				AND item_count > 1;
		END`
}

func galleryProjectionDayUpdateTriggerSQL() string {
	return `CREATE TRIGGER IF NOT EXISTS trg_catalog_gallery_projection_day_update_v1
		AFTER UPDATE OF captured_at ON catalog_gallery_projection
		WHEN substr(OLD.captured_at, 1, 10) <> substr(NEW.captured_at, 1, 10)
		BEGIN
			DELETE FROM catalog_gallery_projection_days
			WHERE captured_day = substr(OLD.captured_at, 1, 10)
				AND item_count = 1;
			UPDATE catalog_gallery_projection_days
			SET item_count = item_count - 1
			WHERE captured_day = substr(OLD.captured_at, 1, 10)
				AND item_count > 1;
			INSERT INTO catalog_gallery_projection_days(captured_day, item_count)
			VALUES (substr(NEW.captured_at, 1, 10), 1)
			ON CONFLICT(captured_day) DO UPDATE SET
				item_count = item_count + 1;
		END`
}

func (s *CatalogStore) ensureGalleryProjectionSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return ErrCatalogNotConfigured
	}
	statements := []string{
		`DROP TRIGGER IF EXISTS trg_catalog_gallery_projection_canonical_insert_v1`,
		`DROP TRIGGER IF EXISTS trg_catalog_gallery_projection_canonical_update_v1`,
		`DROP TRIGGER IF EXISTS trg_catalog_gallery_projection_canonical_delete_v1`,
		`DROP TRIGGER IF EXISTS trg_catalog_gallery_projection_source_insert_v1`,
		`DROP TRIGGER IF EXISTS trg_catalog_gallery_projection_source_update_v1`,
		`DROP TRIGGER IF EXISTS trg_catalog_gallery_projection_source_delete_v1`,
		`DROP TRIGGER IF EXISTS trg_catalog_gallery_projection_local_asset_insert_v1`,
		`DROP TRIGGER IF EXISTS trg_catalog_gallery_projection_local_asset_update_v1`,
		`DROP TRIGGER IF EXISTS trg_catalog_gallery_projection_local_asset_delete_v1`,
		`DROP TRIGGER IF EXISTS trg_catalog_gallery_projection_rendition_insert_v1`,
		`DROP TRIGGER IF EXISTS trg_catalog_gallery_projection_rendition_update_v1`,
		`DROP TRIGGER IF EXISTS trg_catalog_gallery_projection_rendition_delete_v1`,
		`CREATE TABLE IF NOT EXISTS catalog_gallery_projection_state (
			singleton_id INTEGER PRIMARY KEY CHECK(singleton_id = 1),
			scope_key TEXT NOT NULL,
			built_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS catalog_gallery_projection_sources (
			role TEXT NOT NULL CHECK(role IN ('immich', 'local', 'local_fallback')),
			source_key TEXT NOT NULL,
			PRIMARY KEY(role, source_key)
		)`,
		`CREATE TABLE IF NOT EXISTS catalog_gallery_projection (
			canonical_asset_id TEXT PRIMARY KEY,
			source_key TEXT NOT NULL,
			upstream_asset_id TEXT NOT NULL,
			media_type TEXT NOT NULL CHECK(media_type IN ('image', 'video')),
			filename TEXT NOT NULL,
			captured_at TEXT NOT NULL,
			duration TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_gallery_projection_captured
			ON catalog_gallery_projection(captured_at DESC, canonical_asset_id)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_gallery_projection_media_captured
			ON catalog_gallery_projection(media_type, captured_at DESC, canonical_asset_id)`,
		`CREATE TABLE IF NOT EXISTS catalog_gallery_projection_days (
			captured_day TEXT PRIMARY KEY,
			item_count INTEGER NOT NULL CHECK(item_count > 0)
		)`,
		`CREATE TABLE IF NOT EXISTS catalog_gallery_projection_day_index_state (
			singleton_id INTEGER PRIMARY KEY CHECK(singleton_id = 1),
			schema_version INTEGER NOT NULL,
			built_at TEXT NOT NULL
		)`,
		galleryProjectionDayInsertTriggerSQL(),
		galleryProjectionDayDeleteTriggerSQL(),
		galleryProjectionDayUpdateTriggerSQL(),
		galleryProjectionTriggerSQL(
			"trg_catalog_gallery_projection_canonical_insert_v2", "INSERT", "catalog_canonical_assets",
			"NEW.canonical_asset_id", "c.canonical_asset_id = NEW.canonical_asset_id",
		),
		galleryProjectionTriggerSQL(
			"trg_catalog_gallery_projection_canonical_update_v2", "UPDATE", "catalog_canonical_assets",
			"OLD.canonical_asset_id, NEW.canonical_asset_id", "c.canonical_asset_id IN (OLD.canonical_asset_id, NEW.canonical_asset_id)",
		),
		galleryProjectionTriggerSQL(
			"trg_catalog_gallery_projection_canonical_delete_v2", "DELETE", "catalog_canonical_assets",
			"OLD.canonical_asset_id", "c.canonical_asset_id = OLD.canonical_asset_id",
		),
		galleryProjectionTriggerSQL(
			"trg_catalog_gallery_projection_source_insert_v2", "INSERT", "catalog_assets",
			"NEW.canonical_asset_id", "c.canonical_asset_id = NEW.canonical_asset_id",
		),
		galleryProjectionTriggerSQL(
			"trg_catalog_gallery_projection_source_update_v2", "UPDATE", "catalog_assets",
			"OLD.canonical_asset_id, NEW.canonical_asset_id", "c.canonical_asset_id IN (OLD.canonical_asset_id, NEW.canonical_asset_id)",
		),
		galleryProjectionTriggerSQL(
			"trg_catalog_gallery_projection_source_delete_v2", "DELETE", "catalog_assets",
			"OLD.canonical_asset_id", "c.canonical_asset_id = OLD.canonical_asset_id",
		),
		galleryProjectionTriggerSQL(
			"trg_catalog_gallery_projection_local_asset_insert_v2", "INSERT", "local_assets",
			`SELECT canonical_asset_id FROM catalog_assets
				WHERE source_key = NEW.source_key AND upstream_asset_id = NEW.asset_id
					AND canonical_asset_id IS NOT NULL`,
			`c.canonical_asset_id IN (SELECT canonical_asset_id FROM catalog_assets
				WHERE source_key = NEW.source_key AND upstream_asset_id = NEW.asset_id)`,
		),
		galleryProjectionTriggerSQL(
			"trg_catalog_gallery_projection_local_asset_update_v2", "UPDATE", "local_assets",
			`SELECT canonical_asset_id FROM catalog_assets
				WHERE (source_key = OLD.source_key AND upstream_asset_id = OLD.asset_id)
					OR (source_key = NEW.source_key AND upstream_asset_id = NEW.asset_id)`,
			`c.canonical_asset_id IN (SELECT canonical_asset_id FROM catalog_assets
				WHERE (source_key = OLD.source_key AND upstream_asset_id = OLD.asset_id)
					OR (source_key = NEW.source_key AND upstream_asset_id = NEW.asset_id))`,
		),
		galleryProjectionTriggerSQL(
			"trg_catalog_gallery_projection_local_asset_delete_v2", "DELETE", "local_assets",
			`SELECT canonical_asset_id FROM catalog_assets
				WHERE source_key = OLD.source_key AND upstream_asset_id = OLD.asset_id
					AND canonical_asset_id IS NOT NULL`,
			`c.canonical_asset_id IN (SELECT canonical_asset_id FROM catalog_assets
				WHERE source_key = OLD.source_key AND upstream_asset_id = OLD.asset_id)`,
		),
		galleryProjectionTriggerSQL(
			"trg_catalog_gallery_projection_rendition_insert_v2", "INSERT", "local_renditions",
			`SELECT canonical_asset_id FROM catalog_assets
				WHERE source_key = NEW.source_key AND upstream_asset_id = NEW.asset_id
					AND canonical_asset_id IS NOT NULL`,
			`c.canonical_asset_id IN (SELECT canonical_asset_id FROM catalog_assets
				WHERE source_key = NEW.source_key AND upstream_asset_id = NEW.asset_id)`,
		),
		galleryProjectionTriggerSQL(
			"trg_catalog_gallery_projection_rendition_update_v2", "UPDATE", "local_renditions",
			`SELECT canonical_asset_id FROM catalog_assets
				WHERE (source_key = OLD.source_key AND upstream_asset_id = OLD.asset_id)
					OR (source_key = NEW.source_key AND upstream_asset_id = NEW.asset_id)`,
			`c.canonical_asset_id IN (SELECT canonical_asset_id FROM catalog_assets
				WHERE (source_key = OLD.source_key AND upstream_asset_id = OLD.asset_id)
					OR (source_key = NEW.source_key AND upstream_asset_id = NEW.asset_id))`,
		),
		galleryProjectionTriggerSQL(
			"trg_catalog_gallery_projection_rendition_delete_v2", "DELETE", "local_renditions",
			`SELECT canonical_asset_id FROM catalog_assets
				WHERE source_key = OLD.source_key AND upstream_asset_id = OLD.asset_id
					AND canonical_asset_id IS NOT NULL`,
			`c.canonical_asset_id IN (SELECT canonical_asset_id FROM catalog_assets
				WHERE source_key = OLD.source_key AND upstream_asset_id = OLD.asset_id)`,
		),
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ensure mixed gallery projection schema: %w", err)
		}
	}
	return s.ensureGalleryProjectionDayIndex(ctx)
}

func (s *CatalogStore) ensureGalleryProjectionDayIndex(ctx context.Context) error {
	var schemaVersion int
	err := s.db.QueryRowContext(ctx, `SELECT schema_version
		FROM catalog_gallery_projection_day_index_state
		WHERE singleton_id = ?`, catalogGalleryProjectionDayIndexStateID).Scan(&schemaVersion)
	if err == nil && schemaVersion == catalogGalleryProjectionDayIndexSchemaVersion {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read mixed gallery day index state: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mixed gallery day index rebuild: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_gallery_projection_days`); err != nil {
		return fmt.Errorf("clear mixed gallery day index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_gallery_projection_days(captured_day, item_count)
		SELECT substr(captured_at, 1, 10), COUNT(*)
		FROM catalog_gallery_projection
		GROUP BY substr(captured_at, 1, 10)`); err != nil {
		return fmt.Errorf("build mixed gallery day index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_gallery_projection_day_index_state(
			singleton_id, schema_version, built_at
		) VALUES (?, ?, ?)
		ON CONFLICT(singleton_id) DO UPDATE SET
			schema_version = excluded.schema_version,
			built_at = excluded.built_at`,
		catalogGalleryProjectionDayIndexStateID,
		catalogGalleryProjectionDayIndexSchemaVersion,
		formatCatalogTime(time.Now().UTC()),
	); err != nil {
		return fmt.Errorf("publish mixed gallery day index: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mixed gallery day index rebuild: %w", err)
	}
	return nil
}

func (s *CatalogStore) ensureGalleryProjection(ctx context.Context, readiness catalogGalleryReadiness) error {
	if s == nil || s.db == nil {
		return ErrCatalogNotConfigured
	}
	scopeKey, supported := catalogGalleryProjectionScopeKey(readiness)
	if supported {
		var currentScope string
		err := s.db.QueryRowContext(ctx, `SELECT scope_key
			FROM catalog_gallery_projection_state
			WHERE singleton_id = ?`, catalogGalleryProjectionStateID).Scan(&currentScope)
		if err == nil && currentScope == scopeKey {
			return nil
		}
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("read mixed gallery projection scope: %w", err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mixed gallery projection refresh: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_gallery_projection_state`); err != nil {
		return fmt.Errorf("clear mixed gallery projection state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_gallery_projection`); err != nil {
		return fmt.Errorf("clear mixed gallery projection rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_gallery_projection_sources`); err != nil {
		return fmt.Errorf("clear mixed gallery projection sources: %w", err)
	}
	if supported {
		for role, keys := range map[string][]string{
			"immich":         normalizedGallerySourceKeys(readiness.immichSourceKeys),
			"local":          normalizedGallerySourceKeys(readiness.localSourceKeys),
			"local_fallback": normalizedGallerySourceKeys(readiness.localImmichFallbackSourceKeys),
		} {
			for _, key := range keys {
				if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_gallery_projection_sources(role, source_key)
					VALUES (?, ?)`, role, key); err != nil {
					return fmt.Errorf("write mixed gallery projection source: %w", err)
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_gallery_projection_state(singleton_id, scope_key, built_at)
			VALUES (?, ?, ?)`, catalogGalleryProjectionStateID, scopeKey, formatCatalogTime(time.Now().UTC())); err != nil {
			return fmt.Errorf("publish mixed gallery projection state: %w", err)
		}
		if _, err := tx.ExecContext(ctx, galleryProjectionInsertSQL("1 = 1")); err != nil {
			return fmt.Errorf("build mixed gallery projection: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mixed gallery projection refresh: %w", err)
	}
	return nil
}

func (s *CatalogStore) searchGalleryProjection(
	ctx context.Context,
	normalized normalizedAssetSearch,
	readiness catalogGalleryReadiness,
) (AssetSearchPage, bool, error) {
	request := normalized.Request
	if request.Collection.Kind != CollectionKindTimeline || request.Collection.Query != nil {
		return AssetSearchPage{}, false, nil
	}
	scopeKey, supported := catalogGalleryProjectionScopeKey(readiness)
	if !supported {
		return AssetSearchPage{}, false, nil
	}
	db := s.queryDB()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AssetSearchPage{}, true, fmt.Errorf("begin mixed gallery projection query: %w", err)
	}
	defer tx.Rollback()
	var currentScope string
	if err := tx.QueryRowContext(ctx, `SELECT scope_key FROM catalog_gallery_projection_state
		WHERE singleton_id = ?`, catalogGalleryProjectionStateID).Scan(&currentScope); err != nil {
		if err == sql.ErrNoRows {
			return AssetSearchPage{}, false, nil
		}
		return AssetSearchPage{}, true, fmt.Errorf("read mixed gallery projection state: %w", err)
	}
	if currentScope != scopeKey {
		return AssetSearchPage{}, false, nil
	}

	where, args := galleryProjectionFilterWhere(
		request.Collection.Filters.MediaTypes,
		request.Collection.Filters.CapturedAt,
	)
	includeTotal := catalogSearchIncludesExactTotal(normalized)
	limit := request.Page.Size
	offset := request.Page.Index * request.Page.Size
	var total int
	if includeTotal {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_gallery_projection `+where, args...).Scan(&total); err != nil {
			return AssetSearchPage{}, true, fmt.Errorf("count mixed gallery projection: %w", err)
		}
	}
	queryLimit := limit
	if !includeTotal {
		queryLimit++
	}
	var rows *sql.Rows
	if offset > 0 && len(request.Collection.Filters.MediaTypes) == 0 && request.Collection.Filters.CapturedAt == nil {
		var found bool
		var anchorCapturedAt string
		var anchorCanonicalAssetID string
		anchorCapturedAt, anchorCanonicalAssetID, found, err = galleryProjectionAnchorAtOffset(ctx, tx, offset)
		if err == nil && found {
			rows, err = tx.QueryContext(
				ctx,
				galleryProjectionAfterAnchorSQL,
				anchorCapturedAt,
				anchorCapturedAt,
				anchorCanonicalAssetID,
				queryLimit,
			)
		}
		if err == nil && !found {
			rows, err = tx.QueryContext(ctx, `SELECT `+galleryProjectionSelectColumns+`
				FROM catalog_gallery_projection
				WHERE 0`)
		}
	} else {
		queryArgs := append(append([]any(nil), args...), queryLimit, offset)
		rows, err = tx.QueryContext(ctx, `SELECT `+galleryProjectionSelectColumns+`
			FROM catalog_gallery_projection `+where+`
			ORDER BY captured_at DESC, canonical_asset_id ASC
			LIMIT ? OFFSET ?`, queryArgs...)
	}
	if err != nil {
		return AssetSearchPage{}, true, fmt.Errorf("query mixed gallery projection: %w", err)
	}
	defer rows.Close()
	items := []Asset{}
	for rows.Next() {
		var asset Asset
		var capturedAtText string
		var duration sql.NullString
		if err := rows.Scan(&asset.SourceKey, &asset.ID, &asset.Type, &asset.Filename, &capturedAtText, &duration); err != nil {
			return AssetSearchPage{}, true, fmt.Errorf("scan mixed gallery projection asset: %w", err)
		}
		capturedAt, err := time.Parse(time.RFC3339Nano, capturedAtText)
		if err != nil {
			return AssetSearchPage{}, true, fmt.Errorf("parse mixed gallery projection captured_at: %w", err)
		}
		asset.CapturedAt = capturedAt.UTC()
		if duration.Valid {
			value := duration.String
			asset.Duration = &value
		}
		items = append(items, asset)
	}
	if err := rows.Err(); err != nil {
		return AssetSearchPage{}, true, fmt.Errorf("iterate mixed gallery projection assets: %w", err)
	}
	if err := rows.Close(); err != nil {
		return AssetSearchPage{}, true, fmt.Errorf("close mixed gallery projection assets: %w", err)
	}

	var nextPageIndex *int
	totalAccuracy := TotalAccuracyExact
	if !includeTotal {
		hasMore := len(items) > limit
		if hasMore {
			items = items[:limit]
			total = offset + len(items) + 1
			totalAccuracy = TotalAccuracyLowerBound
			next := request.Page.Index + 1
			nextPageIndex = &next
		} else {
			total = offset + len(items)
			if len(items) == 0 && request.Page.Index > 0 {
				total = 0
				totalAccuracy = TotalAccuracyLowerBound
			}
		}
	} else if offset+len(items) < total {
		next := request.Page.Index + 1
		nextPageIndex = &next
	}
	if err := tx.Commit(); err != nil {
		return AssetSearchPage{}, true, fmt.Errorf("finish mixed gallery projection query: %w", err)
	}
	return AssetSearchPage{
		CollectionKey: normalized.CollectionKey,
		Page:          request.Page,
		Items:         items,
		Total:         total,
		TotalAccuracy: totalAccuracy,
		NextPageIndex: nextPageIndex,
		Boundary:      searchBoundary(request.Page, len(items)),
		Resolved:      normalized.Resolved,
	}, true, nil
}

func galleryProjectionAnchorAtOffset(
	ctx context.Context,
	tx *sql.Tx,
	offset int,
) (string, string, bool, error) {
	var capturedDay string
	var dayStartPosition int64
	err := tx.QueryRowContext(ctx, galleryProjectionDayAtOffsetSQL, offset, offset).Scan(
		&capturedDay,
		&dayStartPosition,
	)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("locate mixed gallery projection day: %w", err)
	}
	dayStart, err := time.Parse("2006-01-02", capturedDay)
	if err != nil {
		return "", "", false, fmt.Errorf("parse mixed gallery projection day: %w", err)
	}
	anchorOffset := int64(offset) - dayStartPosition
	var anchorCapturedAt string
	var anchorCanonicalAssetID string
	err = tx.QueryRowContext(
		ctx,
		galleryProjectionAnchorWithinDaySQL,
		formatCatalogTime(dayStart),
		formatCatalogTime(dayStart.AddDate(0, 0, 1)),
		anchorOffset,
	).Scan(&anchorCapturedAt, &anchorCanonicalAssetID)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("locate mixed gallery projection anchor: %w", err)
	}
	return anchorCapturedAt, anchorCanonicalAssetID, true, nil
}

func galleryProjectionFilterWhere(mediaTypes []string, capturedAt *AssetSearchCapturedTime) (string, []any) {
	clauses := []string{}
	args := []any{}
	if len(mediaTypes) > 0 {
		placeholders := make([]string, 0, len(mediaTypes))
		for _, mediaType := range mediaTypes {
			placeholders = append(placeholders, "?")
			args = append(args, mediaType)
		}
		clauses = append(clauses, "media_type IN ("+strings.Join(placeholders, ", ")+")")
	}
	if capturedAt != nil {
		if capturedAt.From != nil {
			clauses = append(clauses, "captured_at >= ?")
			args = append(args, formatCatalogTime(capturedAt.From.UTC()))
		}
		if capturedAt.To != nil {
			clauses = append(clauses, "captured_at < ?")
			args = append(args, formatCatalogTime(capturedAt.To.UTC()))
		}
	}
	if len(clauses) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}
