package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type catalogGalleryReadiness struct {
	localImmichFallbackSourceKeys []string
	immichSourceKeys              []string
}

func (s *CatalogStore) SearchCatalogAssets(ctx context.Context, normalized normalizedAssetSearch) (AssetSearchPage, error) {
	if s == nil || s.db == nil {
		return AssetSearchPage{}, ErrCatalogNotConfigured
	}
	if normalized.Resolved.QueryMode == QueryModeSemantic {
		return AssetSearchPage{}, ErrUnsupportedSearch
	}
	db := s.queryDB()
	where, args := catalogSearchWhere(normalized, "c", s.galleryReadinessSnapshot())
	countQuery := `SELECT COUNT(*) FROM catalog_canonical_assets c ` + where
	var total int
	if err := db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return AssetSearchPage{}, fmt.Errorf("count catalog assets: %w", err)
	}

	limit := normalized.Request.Page.Size
	offset := normalized.Request.Page.Index * normalized.Request.Page.Size
	query := `SELECT primary_source_key, primary_upstream_asset_id, media_type, filename, captured_at, duration
		FROM catalog_canonical_assets c ` + where + `
		ORDER BY c.captured_at DESC, c.primary_source_key ASC, c.primary_upstream_asset_id ASC
		LIMIT ? OFFSET ?`
	queryArgs := append(append([]any{}, args...), limit, offset)
	rows, err := db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return AssetSearchPage{}, fmt.Errorf("query catalog assets: %w", err)
	}
	defer rows.Close()

	items := []Asset{}
	for rows.Next() {
		var sourceKey string
		var id string
		var mediaType string
		var filename string
		var capturedAtText string
		var duration sql.NullString
		if err := rows.Scan(&sourceKey, &id, &mediaType, &filename, &capturedAtText, &duration); err != nil {
			return AssetSearchPage{}, fmt.Errorf("scan catalog asset: %w", err)
		}
		capturedAt, err := time.Parse(time.RFC3339Nano, capturedAtText)
		if err != nil {
			return AssetSearchPage{}, fmt.Errorf("parse catalog captured_at: %w", err)
		}
		var durationPtr *string
		if duration.Valid {
			value := duration.String
			durationPtr = &value
		}
		items = append(items, Asset{
			SourceKey:  sourceKey,
			ID:         id,
			Type:       mediaType,
			Filename:   filename,
			CapturedAt: capturedAt.UTC(),
			Duration:   durationPtr,
		})
	}
	if err := rows.Err(); err != nil {
		return AssetSearchPage{}, fmt.Errorf("iterate catalog assets: %w", err)
	}

	var nextPageIndex *int
	if offset+len(items) < total {
		next := normalized.Request.Page.Index + 1
		nextPageIndex = &next
	}
	return AssetSearchPage{
		CollectionKey: normalized.CollectionKey,
		Page:          normalized.Request.Page,
		Items:         items,
		Total:         total,
		TotalAccuracy: TotalAccuracyExact,
		NextPageIndex: nextPageIndex,
		Boundary:      searchBoundary(normalized.Request.Page, len(items)),
		Resolved:      normalized.Resolved,
	}, nil
}

func catalogSearchWhere(normalized normalizedAssetSearch, canonicalAlias string, readiness catalogGalleryReadiness) (string, []any) {
	column := func(name string) string {
		if canonicalAlias == "" {
			return name
		}
		return canonicalAlias + "." + name
	}
	clauses := []string{
		"WHERE " + column("visibility_status") + " = 'active'",
	}
	args := []any{}
	readinessClause, readinessArgs := catalogGalleryReadinessClause(column, readiness)
	clauses = append(clauses, readinessClause)
	args = append(args, readinessArgs...)
	if mediaTypes := normalized.Request.Collection.Filters.MediaTypes; len(mediaTypes) > 0 {
		placeholders := make([]string, 0, len(mediaTypes))
		for _, mediaType := range mediaTypes {
			placeholders = append(placeholders, "?")
			args = append(args, mediaType)
		}
		clauses = append(clauses, column("media_type")+" IN ("+strings.Join(placeholders, ", ")+")")
	}
	if capturedAt := normalized.Request.Collection.Filters.CapturedAt; capturedAt != nil {
		if capturedAt.From != nil {
			clauses = append(clauses, column("captured_at")+" >= ?")
			args = append(args, formatCatalogTime(capturedAt.From.UTC()))
		}
		if capturedAt.To != nil {
			clauses = append(clauses, column("captured_at")+" < ?")
			args = append(args, formatCatalogTime(capturedAt.To.UTC()))
		}
	}
	if normalized.Resolved.QueryMode == QueryModeFilename && normalized.Request.Collection.Query != nil {
		query := strings.ToLower(strings.TrimSpace(normalized.Request.Collection.Query.Text))
		if query != "" {
			clauses = append(clauses, `EXISTS (
				SELECT 1
				FROM catalog_assets source
				WHERE source.canonical_asset_id = `+column("canonical_asset_id")+`
					AND source.visibility_status = 'active'
					AND lower(source.filename) LIKE ? ESCAPE '\'
			)`)
			args = append(args, "%"+escapeCatalogLike(query)+"%")
		}
	}
	return strings.Join(clauses, " AND "), args
}

func catalogGalleryReadinessClause(column func(string) string, readiness catalogGalleryReadiness) (string, []any) {
	localPrimary := `EXISTS (
		SELECT 1
		FROM catalog_assets primary_source
		WHERE primary_source.source_key = ` + column("primary_source_key") + `
			AND primary_source.upstream_asset_id = ` + column("primary_upstream_asset_id") + `
			AND primary_source.datasource_kind = 'local_filesystem'
	)`
	localRenditionsReady := `EXISTS (
		SELECT 1
		FROM local_assets local_asset
		WHERE local_asset.source_key = ` + column("primary_source_key") + `
			AND local_asset.asset_id = ` + column("primary_upstream_asset_id") + `
			AND local_asset.visibility_status = 'active'
			AND local_asset.thumbnail_status = 'ready'
			AND (
				SELECT COUNT(DISTINCT rendition.kind)
				FROM local_renditions rendition
				WHERE rendition.source_key = local_asset.source_key
					AND rendition.asset_id = local_asset.asset_id
					AND rendition.kind IN ('preview', 'detail_preview')
					AND rendition.status = 'ready'
					AND rendition.relative_path IS NOT NULL
					AND trim(rendition.relative_path) <> ''
					AND rendition.source_sha1_hex = local_asset.sha1_hex
			) = 2
	)`

	clauses := []string{"NOT (" + localPrimary + ")", localRenditionsReady}
	args := []any{}
	if len(readiness.localImmichFallbackSourceKeys) > 0 && len(readiness.immichSourceKeys) > 0 {
		localPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(readiness.localImmichFallbackSourceKeys)), ",")
		immichPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(readiness.immichSourceKeys)), ",")
		clauses = append(clauses, `(`+column("primary_source_key")+` IN (`+localPlaceholders+`)
			AND EXISTS (
				SELECT 1
				FROM catalog_assets fallback_source
				WHERE fallback_source.canonical_asset_id = `+column("canonical_asset_id")+`
					AND fallback_source.datasource_kind = 'immich'
					AND fallback_source.visibility_status = 'active'
					AND fallback_source.source_key IN (`+immichPlaceholders+`)
			)
		)`)
		for _, sourceKey := range readiness.localImmichFallbackSourceKeys {
			args = append(args, sourceKey)
		}
		for _, sourceKey := range readiness.immichSourceKeys {
			args = append(args, sourceKey)
		}
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}
