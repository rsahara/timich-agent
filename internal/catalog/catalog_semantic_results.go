package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	catalogSemanticResultProjectionCanonical = "canonical"
	catalogSemanticResultProjectionGallery   = "gallery"
	catalogSemanticCanonicalResultChunkSize  = 200
	catalogSemanticGalleryResultChunkSize    = 64
)

type catalogSemanticResultResolver struct {
	store      *CatalogStore
	tx         *sql.Tx
	readiness  catalogGalleryReadiness
	generation int64
	projection string
	seen       map[string]struct{}
}

func (s *CatalogStore) newCatalogSemanticResultResolver(ctx context.Context) (*catalogSemanticResultResolver, error) {
	if s == nil || s.db == nil {
		return nil, ErrCatalogNotConfigured
	}
	readiness := s.galleryReadinessSnapshot()
	resolver := &catalogSemanticResultResolver{
		store:      s,
		readiness:  readiness,
		projection: catalogSemanticResultProjectionCanonical,
		seen:       map[string]struct{}{},
	}
	scopeKey, supported := catalogGalleryTimelineScopeKey(readiness)
	if !supported {
		return resolver, nil
	}
	tx, err := s.queryDB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin semantic gallery result query: %w", err)
	}
	state, err := readCatalogGalleryTimelineState(ctx, tx, scopeKey)
	if err != nil {
		_ = tx.Rollback()
		if err == sql.ErrNoRows {
			return resolver, nil
		}
		return nil, fmt.Errorf("read semantic gallery result generation: %w", err)
	}
	resolver.tx = tx
	resolver.generation = state.Generation
	resolver.projection = catalogSemanticResultProjectionGallery
	return resolver, nil
}

func (r *catalogSemanticResultResolver) Close() error {
	if r == nil || r.tx == nil {
		return nil
	}
	return r.tx.Rollback()
}

func (r *catalogSemanticResultResolver) chunkSize() int {
	if r != nil && r.projection == catalogSemanticResultProjectionGallery {
		return catalogSemanticGalleryResultChunkSize
	}
	return catalogSemanticCanonicalResultChunkSize
}

func (r *catalogSemanticResultResolver) resolve(
	ctx context.Context,
	scored []semanticScoredAsset,
	scoreOffset int,
	includeSemanticScores bool,
) ([]Asset, error) {
	if r == nil || r.store == nil || len(scored) == 0 {
		return nil, nil
	}
	if scored[0].Asset.SourceKey == canonicalSemanticCorpusSourceKey {
		return r.resolveCanonical(ctx, scored, scoreOffset, includeSemanticScores)
	}
	if r.tx == nil || r.projection != catalogSemanticResultProjectionGallery {
		return r.store.canonicalAssetsForScoredSourceChunk(
			ctx,
			scored,
			includeSemanticScores,
			scoreOffset,
			r.seen,
			r.readiness,
		)
	}
	return r.resolveGallery(ctx, scored, scoreOffset, includeSemanticScores)
}

func (r *catalogSemanticResultResolver) resolveCanonical(
	ctx context.Context,
	scored []semanticScoredAsset,
	scoreOffset int,
	includeSemanticScores bool,
) ([]Asset, error) {
	var builder strings.Builder
	args := make([]any, 0, len(scored)*2+1)
	builder.WriteString(`WITH requested(canonical_asset_id, score_order, semantic_score) AS (VALUES `)
	for index, candidate := range scored {
		if index > 0 {
			builder.WriteString(",")
		}
		builder.WriteString("(?, ?, ?)")
		args = append(args, candidate.Asset.ID, scoreOffset+index, float64(candidate.Similarity))
	}
	builder.WriteString(`)`)
	var queryer interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	} = r.store.queryDB()
	sourceKeyColumn := "canonical.primary_source_key"
	upstreamAssetIDColumn := "canonical.primary_upstream_asset_id"
	where := "canonical.visibility_status = 'active'"
	if r.tx != nil && r.projection == catalogSemanticResultProjectionGallery {
		builder.WriteString(`
			SELECT requested.score_order, requested.semantic_score,
				gallery.canonical_asset_id, gallery.source_key, gallery.upstream_asset_id,
				gallery.media_type, gallery.filename, gallery.captured_at, gallery.duration
			FROM requested
			JOIN catalog_gallery_timeline gallery
				ON gallery.generation = ? AND gallery.canonical_asset_id = requested.canonical_asset_id
			ORDER BY requested.score_order ASC`)
		args = append(args, r.generation)
		queryer = r.tx
	} else {
		canonicalColumn := func(name string) string { return "canonical." + name }
		var sourceArgs []any
		sourceKeyColumn, upstreamAssetIDColumn, sourceArgs = catalogGallerySourceProjection(canonicalColumn, r.readiness)
		readinessClause, readinessArgs := catalogGalleryReadinessClause(canonicalColumn, r.readiness)
		if readinessClause != "" {
			where += " AND " + readinessClause
		}
		builder.WriteString(`
			SELECT requested.score_order, requested.semantic_score,
				canonical.canonical_asset_id, ` + sourceKeyColumn + `, ` + upstreamAssetIDColumn + `,
				canonical.media_type, canonical.filename, canonical.captured_at, canonical.duration
			FROM requested
			JOIN catalog_canonical_assets canonical
				ON canonical.canonical_asset_id = requested.canonical_asset_id
			WHERE ` + where + `
			ORDER BY requested.score_order ASC`)
		args = append(args, sourceArgs...)
		args = append(args, readinessArgs...)
	}
	rows, err := queryer.QueryContext(ctx, builder.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("read canonical semantic results: %w", err)
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
			return nil, fmt.Errorf("scan canonical semantic result: %w", err)
		}
		if _, ok := r.seen[canonicalID]; ok {
			continue
		}
		capturedAt, err := time.Parse(time.RFC3339Nano, capturedAtText)
		if err != nil {
			return nil, fmt.Errorf("parse canonical semantic result captured_at: %w", err)
		}
		asset.CapturedAt = capturedAt.UTC()
		if duration.Valid {
			value := duration.String
			asset.Duration = &value
		}
		r.seen[canonicalID] = struct{}{}
		if includeSemanticScores && semanticScore.Valid {
			score := float32(semanticScore.Float64)
			asset.SemanticScore = &score
		}
		items = append(items, asset)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate canonical semantic results: %w", err)
	}
	return items, nil
}

func (r *catalogSemanticResultResolver) resolveGallery(
	ctx context.Context,
	scored []semanticScoredAsset,
	scoreOffset int,
	includeSemanticScores bool,
) ([]Asset, error) {
	var builder strings.Builder
	args := make([]any, 0, len(scored)*4+1)
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
			gallery.canonical_asset_id, gallery.source_key, gallery.upstream_asset_id,
			gallery.media_type, gallery.filename, gallery.captured_at, gallery.duration
		FROM requested r
		JOIN catalog_assets source
			ON source.source_key = r.source_key
			AND source.upstream_asset_id = r.upstream_asset_id
		JOIN catalog_gallery_timeline gallery
			ON gallery.generation = ?
			AND gallery.canonical_asset_id = source.canonical_asset_id
		ORDER BY r.score_order ASC`)
	args = append(args, r.generation)
	rows, err := r.tx.QueryContext(ctx, builder.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("read catalog semantic Gallery results: %w", err)
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
		if err := rows.Scan(
			&scoreOrder,
			&semanticScore,
			&canonicalID,
			&asset.SourceKey,
			&asset.ID,
			&asset.Type,
			&asset.Filename,
			&capturedAtText,
			&duration,
		); err != nil {
			return nil, fmt.Errorf("scan catalog semantic Gallery result: %w", err)
		}
		if _, ok := r.seen[canonicalID]; ok {
			continue
		}
		capturedAt, err := time.Parse(time.RFC3339Nano, capturedAtText)
		if err != nil {
			return nil, fmt.Errorf("parse catalog semantic Gallery result captured_at: %w", err)
		}
		asset.CapturedAt = capturedAt.UTC()
		if duration.Valid {
			value := duration.String
			asset.Duration = &value
		}
		r.seen[canonicalID] = struct{}{}
		if includeSemanticScores && semanticScore.Valid {
			score := float32(semanticScore.Float64)
			asset.SemanticScore = &score
		}
		items = append(items, asset)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate catalog semantic Gallery results: %w", err)
	}
	return items, nil
}
