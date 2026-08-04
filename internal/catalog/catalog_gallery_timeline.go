package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

const catalogGalleryTimelineStateID = 1

type catalogGalleryTimelineState struct {
	Generation          int64
	CanonicalGeneration int64
	ScopeKey            string
	Total               int
}

func catalogGalleryTimelineScopeKey(readiness catalogGalleryReadiness) (string, bool) {
	if !readiness.immichOnly || len(readiness.immichSourceKeys) == 0 {
		return "", false
	}
	keys := append([]string(nil), readiness.immichSourceKeys...)
	for index := range keys {
		keys[index] = strings.TrimSpace(keys[index])
	}
	sort.Strings(keys)
	unique := keys[:0]
	for _, key := range keys {
		if key == "" || (len(unique) > 0 && unique[len(unique)-1] == key) {
			continue
		}
		unique = append(unique, key)
	}
	if len(unique) == 0 {
		return "", false
	}
	return "immich:" + strings.Join(unique, ","), true
}

func (s *CatalogStore) ensureGalleryTimeline(ctx context.Context, readiness catalogGalleryReadiness) error {
	if s == nil || s.db == nil {
		return ErrCatalogNotConfigured
	}
	scopeKey, supported := catalogGalleryTimelineScopeKey(readiness)
	if supported {
		var currentScope string
		var timelineCanonicalGeneration int64
		var currentCanonicalGeneration int64
		err := s.db.QueryRowContext(ctx, `SELECT timeline.scope_key,
				timeline.canonical_generation, canonical.generation
			FROM catalog_gallery_timeline_state timeline
			JOIN catalog_canonical_state canonical ON canonical.singleton_id = timeline.singleton_id
			WHERE timeline.singleton_id = ?`, catalogGalleryTimelineStateID).Scan(
			&currentScope,
			&timelineCanonicalGeneration,
			&currentCanonicalGeneration,
		)
		if err == nil && currentScope == scopeKey && timelineCanonicalGeneration == currentCanonicalGeneration {
			return nil
		}
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("read gallery timeline state: %w", err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin gallery timeline refresh: %w", err)
	}
	if err := s.rebuildGalleryTimelineInTx(ctx, tx, readiness); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit gallery timeline refresh: %w", err)
	}
	s.clearGalleryTotalCache()
	return nil
}

func (s *CatalogStore) rebuildGalleryTimelineInTx(ctx context.Context, tx *sql.Tx, readiness catalogGalleryReadiness) error {
	scopeKey, supported := catalogGalleryTimelineScopeKey(readiness)
	if !supported {
		if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_gallery_timeline_state`); err != nil {
			return fmt.Errorf("clear gallery timeline state: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_gallery_timeline`); err != nil {
			return fmt.Errorf("clear gallery timeline rows: %w", err)
		}
		return nil
	}

	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((
		SELECT generation FROM catalog_gallery_timeline_state WHERE singleton_id = ?
	), 0) + 1`, catalogGalleryTimelineStateID).Scan(&generation); err != nil {
		return fmt.Errorf("allocate gallery timeline generation: %w", err)
	}
	var canonicalGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT generation
		FROM catalog_canonical_state
		WHERE singleton_id = ?`, catalogGalleryTimelineStateID).Scan(&canonicalGeneration); err != nil {
		return fmt.Errorf("read canonical generation for gallery timeline: %w", err)
	}

	keys := append([]string(nil), readiness.immichSourceKeys...)
	sort.Strings(keys)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	query := `WITH ranked_sources AS (
			SELECT a.canonical_asset_id, a.source_key, a.upstream_asset_id,
				ROW_NUMBER() OVER (
					PARTITION BY a.canonical_asset_id
					ORDER BY a.source_key ASC, a.upstream_asset_id ASC
				) AS source_rank
			FROM catalog_assets a
			WHERE a.source_key IN (` + placeholders + `)
				AND a.datasource_kind = 'immich'
				AND a.visibility_status = 'active'
				AND a.canonical_asset_id IS NOT NULL
		), selected_sources AS (
			SELECT canonical_asset_id, source_key, upstream_asset_id
			FROM ranked_sources
			WHERE source_rank = 1
		), ordered_gallery AS (
			SELECT c.canonical_asset_id, selected.source_key, selected.upstream_asset_id,
				c.media_type, c.filename, c.captured_at, c.duration,
				ROW_NUMBER() OVER (
					ORDER BY c.captured_at DESC, c.canonical_asset_id ASC
				) - 1 AS global_position
			FROM catalog_canonical_assets c
			JOIN selected_sources selected
				ON selected.canonical_asset_id = c.canonical_asset_id
			WHERE c.visibility_status = 'active'
		)
		INSERT INTO catalog_gallery_timeline (
			generation, global_position, canonical_asset_id, source_key,
			upstream_asset_id, media_type, filename, captured_at, duration
		)
		SELECT ?, global_position, canonical_asset_id, source_key,
			upstream_asset_id, media_type, filename, captured_at, duration
		FROM ordered_gallery`
	args := make([]any, 0, len(keys)+1)
	for _, key := range keys {
		args = append(args, strings.TrimSpace(key))
	}
	args = append(args, generation)
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("build gallery timeline generation: %w", err)
	}

	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM catalog_gallery_timeline
		WHERE generation = ?`, generation).Scan(&total); err != nil {
		return fmt.Errorf("count gallery timeline generation: %w", err)
	}
	builtAt := formatCatalogTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_gallery_timeline_state (
			singleton_id, generation, canonical_generation, scope_key, total_count, built_at
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(singleton_id) DO UPDATE SET
			generation = excluded.generation,
			canonical_generation = excluded.canonical_generation,
			scope_key = excluded.scope_key,
			total_count = excluded.total_count,
			built_at = excluded.built_at`,
		catalogGalleryTimelineStateID, generation, canonicalGeneration, scopeKey, total, builtAt); err != nil {
		return fmt.Errorf("publish gallery timeline generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_gallery_timeline
		WHERE generation <> ?`, generation); err != nil {
		return fmt.Errorf("prune old gallery timeline generations: %w", err)
	}
	return nil
}

func galleryTimelineNeedsRefreshInTx(
	ctx context.Context,
	tx *sql.Tx,
	readiness catalogGalleryReadiness,
) (bool, error) {
	scopeKey, supported := catalogGalleryTimelineScopeKey(readiness)
	var currentScope string
	var timelineCanonicalGeneration int64
	var currentCanonicalGeneration int64
	err := tx.QueryRowContext(ctx, `SELECT timeline.scope_key,
			timeline.canonical_generation, canonical.generation
		FROM catalog_gallery_timeline_state timeline
		JOIN catalog_canonical_state canonical ON canonical.singleton_id = timeline.singleton_id
		WHERE timeline.singleton_id = ?`, catalogGalleryTimelineStateID).Scan(
		&currentScope,
		&timelineCanonicalGeneration,
		&currentCanonicalGeneration,
	)
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("read gallery timeline state before catalog commit: %w", err)
	}
	if !supported {
		return err == nil, nil
	}
	return err == sql.ErrNoRows || currentScope != scopeKey || timelineCanonicalGeneration != currentCanonicalGeneration, nil
}

func advanceCatalogCanonicalGenerationInTx(ctx context.Context, tx *sql.Tx) error {
	result, err := tx.ExecContext(ctx, `UPDATE catalog_canonical_state
		SET generation = generation + 1
		WHERE singleton_id = ?`, catalogGalleryTimelineStateID)
	if err != nil {
		return fmt.Errorf("advance catalog canonical generation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect catalog canonical generation update: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("advance catalog canonical generation: updated %d rows, want 1", affected)
	}
	return nil
}

func readCatalogGalleryTimelineState(ctx context.Context, tx *sql.Tx, scopeKey string) (catalogGalleryTimelineState, error) {
	var state catalogGalleryTimelineState
	err := tx.QueryRowContext(ctx, `SELECT timeline.generation, timeline.canonical_generation,
			timeline.scope_key, timeline.total_count
		FROM catalog_gallery_timeline_state timeline
		JOIN catalog_canonical_state canonical
			ON canonical.singleton_id = timeline.singleton_id
			AND canonical.generation = timeline.canonical_generation
		WHERE timeline.singleton_id = ? AND timeline.scope_key = ?`,
		catalogGalleryTimelineStateID, scopeKey).Scan(
		&state.Generation,
		&state.CanonicalGeneration,
		&state.ScopeKey,
		&state.Total,
	)
	return state, err
}

func (s *CatalogStore) searchGalleryTimeline(
	ctx context.Context,
	normalized normalizedAssetSearch,
	readiness catalogGalleryReadiness,
) (AssetSearchPage, bool, error) {
	request := normalized.Request
	if request.Collection.Kind != CollectionKindTimeline || request.Collection.Query != nil {
		return AssetSearchPage{}, false, nil
	}
	scopeKey, supported := catalogGalleryTimelineScopeKey(readiness)
	if !supported {
		return AssetSearchPage{}, false, nil
	}
	db := s.queryDB()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AssetSearchPage{}, true, fmt.Errorf("begin gallery timeline query: %w", err)
	}
	defer tx.Rollback()
	state, err := readCatalogGalleryTimelineState(ctx, tx, scopeKey)
	if err != nil {
		if err == sql.ErrNoRows {
			return AssetSearchPage{}, false, nil
		}
		return AssetSearchPage{}, true, fmt.Errorf("read gallery timeline generation: %w", err)
	}

	limit := request.Page.Size
	offset := request.Page.Index * request.Page.Size
	includeTotal := catalogSearchIncludesExactTotal(normalized)
	mediaTypes := request.Collection.Filters.MediaTypes
	capturedAt := request.Collection.Filters.CapturedAt
	positionSeek := len(mediaTypes) == 0
	startPosition := 0
	endPosition := state.Total
	if positionSeek && capturedAt != nil {
		if capturedAt.To != nil {
			startPosition, err = galleryTimelineFirstPositionBefore(ctx, tx, state, capturedAt.To.UTC())
			if err != nil {
				return AssetSearchPage{}, true, err
			}
		}
		if capturedAt.From != nil {
			endPosition, err = galleryTimelineFirstPositionBefore(ctx, tx, state, capturedAt.From.UTC())
			if err != nil {
				return AssetSearchPage{}, true, err
			}
		}
		endPosition = max(startPosition, endPosition)
	}

	var total int
	if includeTotal {
		switch {
		case positionSeek:
			total = endPosition - startPosition
		default:
			where, args := galleryTimelineFilterWhere(state.Generation, mediaTypes, capturedAt)
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_gallery_timeline `+where, args...).Scan(&total); err != nil {
				return AssetSearchPage{}, true, fmt.Errorf("count gallery timeline: %w", err)
			}
		}
	}

	queryLimit := limit
	if !includeTotal {
		queryLimit++
	}
	var rows *sql.Rows
	if positionSeek {
		pageStart := startPosition + offset
		rows, err = tx.QueryContext(ctx, `SELECT source_key, upstream_asset_id,
				media_type, filename, captured_at, duration
			FROM catalog_gallery_timeline
			WHERE generation = ?
				AND global_position >= ?
				AND global_position < ?
			ORDER BY global_position ASC
			LIMIT ?`, state.Generation, pageStart, endPosition, queryLimit)
	} else {
		where, args := galleryTimelineFilterWhere(state.Generation, mediaTypes, capturedAt)
		query := `SELECT source_key, upstream_asset_id,
				media_type, filename, captured_at, duration
			FROM catalog_gallery_timeline ` + where + `
			ORDER BY captured_at DESC, canonical_asset_id ASC
			LIMIT ? OFFSET ?`
		args = append(args, queryLimit, offset)
		rows, err = tx.QueryContext(ctx, query, args...)
	}
	if err != nil {
		return AssetSearchPage{}, true, fmt.Errorf("query gallery timeline: %w", err)
	}
	defer rows.Close()

	items := []Asset{}
	for rows.Next() {
		var asset Asset
		var capturedAtText string
		var duration sql.NullString
		if err := rows.Scan(&asset.SourceKey, &asset.ID, &asset.Type, &asset.Filename, &capturedAtText, &duration); err != nil {
			return AssetSearchPage{}, true, fmt.Errorf("scan gallery timeline asset: %w", err)
		}
		captured, err := time.Parse(time.RFC3339Nano, capturedAtText)
		if err != nil {
			return AssetSearchPage{}, true, fmt.Errorf("parse gallery timeline captured_at: %w", err)
		}
		asset.CapturedAt = captured.UTC()
		if duration.Valid {
			value := duration.String
			asset.Duration = &value
		}
		items = append(items, asset)
	}
	if err := rows.Err(); err != nil {
		return AssetSearchPage{}, true, fmt.Errorf("iterate gallery timeline assets: %w", err)
	}
	if err := rows.Close(); err != nil {
		return AssetSearchPage{}, true, fmt.Errorf("close gallery timeline assets: %w", err)
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
		return AssetSearchPage{}, true, fmt.Errorf("finish gallery timeline query: %w", err)
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

func galleryTimelineFirstPositionBefore(
	ctx context.Context,
	tx *sql.Tx,
	state catalogGalleryTimelineState,
	boundary time.Time,
) (int, error) {
	var position int
	err := tx.QueryRowContext(ctx, `SELECT global_position
		FROM catalog_gallery_timeline
		WHERE generation = ? AND captured_at < ?
		ORDER BY captured_at DESC, canonical_asset_id ASC
		LIMIT 1`, state.Generation, formatCatalogTime(boundary)).Scan(&position)
	if err == sql.ErrNoRows {
		return state.Total, nil
	}
	if err != nil {
		return 0, fmt.Errorf("locate gallery timeline date boundary: %w", err)
	}
	return position, nil
}

func galleryTimelineFilterWhere(
	generation int64,
	mediaTypes []string,
	capturedAt *AssetSearchCapturedTime,
) (string, []any) {
	clauses := []string{"WHERE generation = ?"}
	args := []any{generation}
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
	return strings.Join(clauses, " AND "), args
}
