package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"
)

type catalogSemanticCandidateTraversal interface {
	Advance(context.Context, int) ([]semanticScoredAsset, error)
	Close() error
	Done() bool
}

type catalogSemanticSourceTraversal struct {
	traversal catalogSemanticCandidateTraversal
	sourceKey string
	reader    *semanticBinaryIndexReader
	visits    int
}

type catalogSemanticTraversalStats struct {
	CandidateVisits    int
	MetadataCandidates int
	Rounds             int
}

type catalogSemanticMetadataCandidateRef struct {
	AssetID string
	Ordinal int
}

func (s *Service) searchCatalogWithoutSemanticProfile(ctx context.Context, normalized normalizedAssetSearch) (AssetSearchPage, error) {
	return s.searchCatalogWithoutUsableSemanticIndex(ctx, normalized, CatalogSemanticStatus{Status: "missing"}, nil)
}

func (s *Service) searchCatalogWithoutUsableSemanticIndex(ctx context.Context, normalized normalizedAssetSearch, semantic CatalogSemanticStatus, profile semanticEmbeddingProfile) (AssetSearchPage, error) {
	semantic = normalizeCatalogSemanticStatus(semantic, profile)
	if semanticAutoRequested(normalized) {
		fallback, err := semanticFilenameFallback(normalized, semantic, profile)
		if err != nil {
			return AssetSearchPage{}, err
		}
		return s.catalog.SearchCatalogAssets(ctx, fallback)
	}
	resolved := normalized.Resolved
	resolved.Semantic = semanticResolution(semantic, false, profile)
	return AssetSearchPage{
		CollectionKey: normalized.CollectionKey,
		Page:          normalized.Request.Page,
		Items:         []Asset{},
		Total:         0,
		TotalAccuracy: TotalAccuracyEstimated,
		Resolved:      resolved,
	}, nil
}

func (s *Service) searchCatalogSemanticAssets(ctx context.Context, normalized normalizedAssetSearch, profile semanticEmbeddingProfile, options AssetSearchOptions) (AssetSearchPage, error) {
	if s == nil || s.catalog == nil {
		return AssetSearchPage{}, ErrCatalogNotConfigured
	}
	query := ""
	if normalized.Request.Collection.Query != nil {
		query = normalized.Request.Collection.Query.Text
	}
	queryVector, err := profile.EmbedText(ctx, query)
	if err != nil {
		return AssetSearchPage{}, fmt.Errorf("embed catalog query: %w", err)
	}
	directIndexStatus := true
	statusMode := "binary direct"
	semantic := baseCatalogSemanticStatus(profile)
	log.Printf(
		"timich-agent catalog semantic search %s status started model=%s vector_space=%s",
		statusMode,
		profile.ModelID(),
		profile.VectorSpaceID(),
	)
	directStatusSeen := false
	directStatusAllReady := true
	directStatusStarted := time.Now()
	sourceKeys := s.semanticDatasourceSourceKeys()
	semantic = baseCatalogSemanticStatus(profile)
	directStatusSeen = false
	directStatusAllReady = true
	usableIndexSeen := false
	traversals := make([]catalogSemanticSourceTraversal, 0, len(sourceKeys))
	defer func() {
		for _, traversal := range traversals {
			if traversal.traversal != nil {
				_ = traversal.traversal.Close()
			}
		}
	}()
	for _, sourceKey := range sourceKeys {
		session, ok, err := s.catalog.openSemanticIndexTraversal(ctx, sourceKey, profile, queryVector)
		if err != nil {
			return AssetSearchPage{}, err
		}
		if !ok {
			continue
		}
		traversals = append(traversals, catalogSemanticSourceTraversal{
			traversal: session,
			sourceKey: sourceKey,
			reader:    session.reader,
		})
		if session.reader != nil && session.traversal != nil {
			usableIndexSeen = true
		}
		if directIndexStatus {
			status := strings.TrimSpace(session.Semantic.Status)
			contributes := session.Semantic.CompletedVectorCount > 0 ||
				session.Semantic.IndexedVectorCount > 0 ||
				(status != "" && status != "missing")
			if contributes {
				directStatusSeen = true
				if status != "ready" {
					directStatusAllReady = false
				}
			}
			semantic.CompletedVectorCount += session.Semantic.CompletedVectorCount
			semantic.IndexedVectorCount += session.Semantic.IndexedVectorCount
			if session.Semantic.BuiltAt != nil && (semantic.BuiltAt == nil || session.Semantic.BuiltAt.After(*semantic.BuiltAt)) {
				builtAt := session.Semantic.BuiltAt.UTC()
				semantic.BuiltAt = &builtAt
			}
		}
	}
	if !usableIndexSeen {
		semantic = finalizeCatalogSemanticDirectStatus(semantic, directStatusSeen, directStatusAllReady, profile)
		log.Printf(
			"timich-agent catalog semantic search %s unavailable; using configured fallback model=%s vector_space=%s status=%s elapsed=%s",
			statusMode,
			profile.ModelID(),
			profile.VectorSpaceID(),
			semantic.Status,
			time.Since(directStatusStarted).Round(time.Millisecond),
		)
		return s.searchCatalogWithoutUsableSemanticIndex(ctx, normalized, semantic, profile)
	}
	metadataCandidates := []semanticScoredAsset{}
	if semanticAutoRequested(normalized) {
		metadataCandidates, err = s.catalogSemanticMetadataCandidates(ctx, normalized, queryVector, traversals)
		if err != nil {
			return AssetSearchPage{}, err
		}
	}
	resolvedPage, traversalStats, err := s.resolveCatalogSemanticTraversalPage(
		ctx,
		normalized,
		traversals,
		metadataCandidates,
		options.IncludeSemanticScores,
	)
	if err != nil {
		return AssetSearchPage{}, err
	}
	candidateSeen := traversalStats.CandidateVisits > 0 || traversalStats.MetadataCandidates > 0
	semantic = finalizeCatalogSemanticDirectStatus(semantic, directStatusSeen, directStatusAllReady, profile)
	log.Printf(
		"timich-agent catalog semantic search %s status completed model=%s vector_space=%s status=%s completed=%d indexed=%d metadata_candidates=%d visits=%d rounds=%d elapsed=%s",
		statusMode,
		profile.ModelID(),
		profile.VectorSpaceID(),
		semantic.Status,
		semantic.CompletedVectorCount,
		semantic.IndexedVectorCount,
		traversalStats.MetadataCandidates,
		traversalStats.CandidateVisits,
		traversalStats.Rounds,
		time.Since(directStatusStarted).Round(time.Millisecond),
	)
	log.Printf(
		"timich-agent catalog semantic search result resolution projection=%s candidates=%d results=%d has_more=%t",
		resolvedPage.Projection,
		resolvedPage.CandidateCount,
		resolvedPage.Total,
		resolvedPage.HasMore,
	)

	if !candidateSeen {
		resolved := normalized.Resolved
		resolved.Semantic = semanticResolution(semantic, true, profile)
		return AssetSearchPage{
			CollectionKey: normalized.CollectionKey,
			Page:          normalized.Request.Page,
			Items:         []Asset{},
			Total:         0,
			TotalAccuracy: TotalAccuracyEstimated,
			Resolved:      resolved,
		}, nil
	}

	var nextPageIndex *int
	if resolvedPage.HasMore {
		next := normalized.Request.Page.Index + 1
		nextPageIndex = &next
	}
	resolved := normalized.Resolved
	resolved.Semantic = semanticResolution(semantic, true, profile)
	return AssetSearchPage{
		CollectionKey: normalized.CollectionKey,
		Page:          normalized.Request.Page,
		Items:         resolvedPage.Items,
		Total:         resolvedPage.Total,
		TotalAccuracy: TotalAccuracyEstimated,
		NextPageIndex: nextPageIndex,
		Boundary:      searchBoundary(normalized.Request.Page, len(resolvedPage.Items)),
		Resolved:      resolved,
	}, nil
}

func finalizeCatalogSemanticDirectStatus(semantic CatalogSemanticStatus, directStatusSeen bool, directStatusAllReady bool, profile semanticEmbeddingProfile) CatalogSemanticStatus {
	switch {
	case semantic.CompletedVectorCount > 0 && semantic.IndexedVectorCount == 0:
		semantic.Status = "backfilling"
	case semantic.IndexedVectorCount == 0 && directStatusSeen:
		semantic.Status = "backfilling"
	case semantic.IndexedVectorCount == 0:
		semantic.Status = "missing"
	case directStatusSeen && directStatusAllReady:
		semantic.Status = "ready"
	default:
		semantic.Status = "backfilling"
	}
	return normalizeCatalogSemanticStatus(semantic, profile)
}

func (s *Service) resolveCatalogSemanticTraversalPage(
	ctx context.Context,
	normalized normalizedAssetSearch,
	traversals []catalogSemanticSourceTraversal,
	metadataCandidates []semanticScoredAsset,
	includeSemanticScores bool,
) (catalogSemanticResolvedPage, catalogSemanticTraversalStats, error) {
	accumulator, err := s.newCatalogSemanticResultAccumulator(ctx, normalized, includeSemanticScores)
	if err != nil {
		return catalogSemanticResolvedPage{}, catalogSemanticTraversalStats{}, err
	}
	defer accumulator.Close()

	stats := catalogSemanticTraversalStats{MetadataCandidates: len(metadataCandidates)}
	if len(metadataCandidates) > 0 {
		sortSemanticScoredAssets(metadataCandidates)
		metadataCandidates = diversifySemanticCandidateSnapshot(metadataCandidates)
		if err := accumulator.Add(ctx, metadataCandidates); err != nil {
			return catalogSemanticResolvedPage{}, stats, err
		}
		// Auto-mode metadata matches rank ahead of semantic candidates. Once
		// they provide the complete requested window, later semantic candidates
		// cannot change this page.
		if !accumulator.NeedsMore() {
			return accumulator.Page(), stats, nil
		}
	}
	ranked := make([]semanticScoredAsset, 0, len(traversals)*semanticSearchVisitBudget)
	for {
		chunk := make([]semanticScoredAsset, 0, len(traversals)*semanticHNSWEfSearch)
		for index := range traversals {
			traversal := &traversals[index]
			if traversal.traversal == nil || traversal.traversal.Done() || traversal.visits >= semanticSearchVisitBudget {
				continue
			}
			visitCount := min(semanticHNSWEfSearch, semanticSearchVisitBudget-traversal.visits)
			batch, err := traversal.traversal.Advance(ctx, visitCount)
			if err != nil {
				return catalogSemanticResolvedPage{}, stats, err
			}
			traversal.visits += len(batch)
			stats.CandidateVisits += len(batch)
			chunk = append(chunk, batch...)
		}
		if len(chunk) == 0 {
			break
		}
		stats.Rounds++
		ranked = append(ranked, chunk...)
	}
	// A best-first graph traversal cannot prove that a later chunk will not
	// discover a higher-scoring node. Rank the complete bounded candidate
	// snapshot before pagination so relevance order is stable across pages.
	sortSemanticScoredAssets(ranked)
	if semanticAutoRequested(normalized) {
		metadataMatches, err := s.catalogSemanticMetadataMatchKeys(ctx, normalized, ranked)
		if err != nil {
			return catalogSemanticResolvedPage{}, stats, err
		}
		ranked = promoteCatalogMetadataMatches(ranked, metadataMatches)
	}
	ranked = diversifySemanticCandidateSnapshot(ranked)
	if err := accumulator.Add(ctx, ranked); err != nil {
		return catalogSemanticResolvedPage{}, stats, err
	}
	return accumulator.Page(), stats, nil
}

type catalogSemanticResolvedPage struct {
	Items          []Asset
	Total          int
	HasMore        bool
	CandidateCount int
	Projection     string
}

type catalogSemanticResultAccumulator struct {
	resolver              *catalogSemanticResultResolver
	normalized            normalizedAssetSearch
	includeSemanticScores bool
	offset                int
	pageEnd               int
	wanted                int
	matched               []Asset
	candidateCount        int
}

func (s *Service) newCatalogSemanticResultAccumulator(
	ctx context.Context,
	normalized normalizedAssetSearch,
	includeSemanticScores bool,
) (*catalogSemanticResultAccumulator, error) {
	resolver, err := s.catalog.newCatalogSemanticResultResolver(ctx)
	if err != nil {
		return nil, err
	}
	offset := normalized.Request.Page.Index * normalized.Request.Page.Size
	pageEnd := offset + normalized.Request.Page.Size
	wanted := pageEnd
	if wanted < math.MaxInt {
		wanted++
	}
	return &catalogSemanticResultAccumulator{
		resolver:              resolver,
		normalized:            normalized,
		includeSemanticScores: includeSemanticScores,
		offset:                offset,
		pageEnd:               pageEnd,
		wanted:                wanted,
		matched:               make([]Asset, 0, min(wanted, semanticSearchVisitBudget)),
	}, nil
}

func (a *catalogSemanticResultAccumulator) Close() error {
	if a == nil || a.resolver == nil {
		return nil
	}
	return a.resolver.Close()
}

func (a *catalogSemanticResultAccumulator) NeedsMore() bool {
	return a != nil && len(a.matched) < a.wanted
}

func (a *catalogSemanticResultAccumulator) Add(ctx context.Context, scored []semanticScoredAsset) error {
	if a == nil || a.resolver == nil || len(scored) == 0 || !a.NeedsMore() {
		return nil
	}
	chunkSize := a.resolver.chunkSize()
	for start := 0; start < len(scored) && a.NeedsMore(); start += chunkSize {
		end := min(start+chunkSize, len(scored))
		resolved, err := a.resolver.resolve(
			ctx,
			scored[start:end],
			a.candidateCount,
			a.includeSemanticScores,
		)
		if err != nil {
			return err
		}
		a.candidateCount += end - start
		for _, item := range resolved {
			if !catalogAssetMatchesSearchFilters(item, a.normalized.Request.Collection.Filters) {
				continue
			}
			if !a.includeSemanticScores {
				item.SemanticScore = nil
			}
			a.matched = append(a.matched, item)
			if !a.NeedsMore() {
				break
			}
		}
	}
	return nil
}

func (a *catalogSemanticResultAccumulator) Page() catalogSemanticResolvedPage {
	if a == nil || a.resolver == nil {
		return catalogSemanticResolvedPage{}
	}
	hasMore := len(a.matched) > a.pageEnd
	end := min(a.pageEnd, len(a.matched))
	items := []Asset{}
	if a.offset < len(a.matched) {
		items = append(items, a.matched[a.offset:end]...)
	}
	total := len(a.matched)
	if hasMore {
		total = a.offset + len(items) + 1
	}
	return catalogSemanticResolvedPage{
		Items:          items,
		Total:          total,
		HasMore:        hasMore,
		CandidateCount: a.candidateCount,
		Projection:     a.resolver.projection,
	}
}

func (s *Service) resolveCatalogSemanticResultPage(
	ctx context.Context,
	normalized normalizedAssetSearch,
	scored []semanticScoredAsset,
	includeSemanticScores bool,
) (catalogSemanticResolvedPage, error) {
	accumulator, err := s.newCatalogSemanticResultAccumulator(ctx, normalized, includeSemanticScores)
	if err != nil {
		return catalogSemanticResolvedPage{}, err
	}
	defer accumulator.Close()
	if err := accumulator.Add(ctx, scored); err != nil {
		return catalogSemanticResolvedPage{}, err
	}
	return accumulator.Page(), nil
}

func catalogAssetMatchesSearchFilters(asset Asset, filters AssetSearchFilters) bool {
	if len(filters.MediaTypes) > 0 {
		matches := false
		for _, mediaType := range filters.MediaTypes {
			if asset.Type == mediaType {
				matches = true
				break
			}
		}
		if !matches {
			return false
		}
	}
	if filters.CapturedAt != nil {
		if filters.CapturedAt.From != nil && asset.CapturedAt.Before(*filters.CapturedAt.From) {
			return false
		}
		if filters.CapturedAt.To != nil && !asset.CapturedAt.Before(*filters.CapturedAt.To) {
			return false
		}
	}
	return true
}

func (s *Service) catalogSemanticCapabilities(ctx context.Context, profile semanticEmbeddingProfile) *AssetSearchSemanticCapabilities {
	if s == nil || !s.catalogStoreEnabled() {
		return &AssetSearchSemanticCapabilities{
			Status:      "missing",
			MessageCode: semanticMessageIndexMissing,
		}
	}
	return semanticCapabilities(s.catalogSemanticStatusForProfile(ctx, profile), profile)
}

func (s *Service) catalogSemanticStatusForProfile(ctx context.Context, profile semanticEmbeddingProfile) CatalogSemanticStatus {
	status := baseCatalogSemanticStatus(profile)
	if profile == nil {
		return status
	}
	sourceKeys := s.semanticDatasourceSourceKeys()
	if len(sourceKeys) == 0 {
		return normalizeCatalogSemanticStatus(status, profile)
	}
	allReady := true
	for _, sourceKey := range sourceKeys {
		sourceStatus, err := s.catalog.semanticStatus(ctx, sourceKey, profile)
		if err != nil {
			return status
		}
		status.CompletedVectorCount += sourceStatus.CompletedVectorCount
		status.IndexedVectorCount += sourceStatus.IndexedVectorCount
		if sourceStatus.BuiltAt != nil && (status.BuiltAt == nil || sourceStatus.BuiltAt.After(*status.BuiltAt)) {
			builtAt := sourceStatus.BuiltAt.UTC()
			status.BuiltAt = &builtAt
		}
		if sourceStatus.Status != "ready" {
			allReady = false
		}
	}
	switch {
	case status.CompletedVectorCount > 0 && status.IndexedVectorCount == 0:
		status.Status = "backfilling"
	case status.IndexedVectorCount == 0:
		status.Status = "missing"
	case allReady:
		status.Status = "ready"
	default:
		status.Status = "backfilling"
	}
	return normalizeCatalogSemanticStatus(status, profile)
}

func baseCatalogSemanticStatus(profile semanticEmbeddingProfile) CatalogSemanticStatus {
	if profile == nil {
		return CatalogSemanticStatus{Status: "missing"}
	}
	return CatalogSemanticStatus{
		Status:        "missing",
		ModelID:       profile.ModelID(),
		VectorSpaceID: profile.VectorSpaceID(),
		EmbeddingDim:  profile.EmbeddingDim(),
		ProfileKind:   profile.ProfileKind(),
		InputKind:     profile.InputKind(),
		ModelPack:     profile.ModelPackStatus(),
	}
}

func sortSemanticScoredAssets(scored []semanticScoredAsset) {
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Similarity == scored[j].Similarity {
			if scored[i].Asset.CapturedAt.Equal(scored[j].Asset.CapturedAt) {
				if scored[i].Asset.SourceKey == scored[j].Asset.SourceKey {
					return scored[i].Asset.ID < scored[j].Asset.ID
				}
				return scored[i].Asset.SourceKey < scored[j].Asset.SourceKey
			}
			return scored[i].Asset.CapturedAt.After(scored[j].Asset.CapturedAt)
		}
		return scored[i].Similarity > scored[j].Similarity
	})
}

func (s *Service) catalogSemanticMetadataMatchKeys(ctx context.Context, normalized normalizedAssetSearch, scored []semanticScoredAsset) (map[string]struct{}, error) {
	if normalized.Request.Collection.Query == nil {
		return nil, nil
	}
	query := strings.TrimSpace(normalized.Request.Collection.Query.Text)
	if query == "" || len(scored) == 0 {
		return nil, nil
	}
	keys := map[string]struct{}{}
	const chunkSize = 200
	for start := 0; start < len(scored); start += chunkSize {
		end := min(start+chunkSize, len(scored))
		var builder strings.Builder
		builder.WriteString(`WITH candidates(source_key, upstream_asset_id) AS (VALUES `)
		args := make([]any, 0, (end-start)*2+8)
		for index, candidate := range scored[start:end] {
			if index > 0 {
				builder.WriteString(",")
			}
			builder.WriteString("(?, ?)")
			args = append(args, candidate.Asset.SourceKey, candidate.Asset.ID)
		}
		where, whereArgs := catalogSemanticMetadataWhere(normalized, query, "a")
		args = append(args, whereArgs...)
		builder.WriteString(`)
			SELECT a.source_key, a.upstream_asset_id
			FROM candidates c
			JOIN catalog_assets a
				ON a.source_key = c.source_key AND a.upstream_asset_id = c.upstream_asset_id `)
		builder.WriteString(where)
		rows, err := s.catalog.queryDB().QueryContext(ctx, builder.String(), args...)
		if err != nil {
			return nil, fmt.Errorf("query catalog semantic candidate metadata matches: %w", err)
		}
		for rows.Next() {
			var sourceKey string
			var id string
			if err := rows.Scan(&sourceKey, &id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan catalog semantic metadata match: %w", err)
			}
			keys[semanticCatalogAssetKey(sourceKey, id)] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate catalog semantic metadata matches: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close catalog semantic metadata matches: %w", err)
		}
	}
	return keys, nil
}

func (s *Service) catalogSemanticMetadataCandidates(
	ctx context.Context,
	normalized normalizedAssetSearch,
	queryVector []float32,
	traversals []catalogSemanticSourceTraversal,
) ([]semanticScoredAsset, error) {
	if normalized.Request.Collection.Query == nil || len(queryVector) == 0 || len(traversals) == 0 {
		return nil, nil
	}
	query := strings.TrimSpace(normalized.Request.Collection.Query.Text)
	if query == "" {
		return nil, nil
	}

	scored := make([]semanticScoredAsset, 0)
	seen := map[string]struct{}{}
	for _, traversal := range traversals {
		sourceKey := strings.TrimSpace(traversal.sourceKey)
		if sourceKey == "" || traversal.reader == nil {
			continue
		}
		header := traversal.reader.header
		refs, err := s.catalogSemanticMetadataCandidateRefs(ctx, normalized, query, header)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			assetID := ref.AssetID
			ordinal := ref.Ordinal
			if ordinal < 0 || ordinal >= len(traversal.reader.nodes) {
				return nil, errSemanticBinaryIndexUnavailable
			}
			key := semanticCatalogAssetKey(sourceKey, assetID)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			asset, err := traversal.reader.assetForOrdinal(ctx, uint32(ordinal))
			if err != nil {
				return nil, fmt.Errorf("read catalog semantic published metadata candidate %q: %w", assetID, err)
			}
			scored = append(scored, semanticScoredAsset{
				Asset:      asset,
				Similarity: semanticDot(queryVector, asset.Vector),
			})
		}
	}

	sortSemanticScoredAssets(scored)
	return scored, nil
}

func (s *Service) catalogSemanticMetadataCandidateRefs(
	ctx context.Context,
	normalized normalizedAssetSearch,
	query string,
	header semanticBinaryIndexHeader,
) ([]catalogSemanticMetadataCandidateRef, error) {
	if len([]rune(query)) < 3 {
		return s.catalogSemanticShortMetadataCandidateRefs(ctx, normalized, query, header)
	}
	textWhere, textFilterArgs := catalogSemanticMetadataFilterWhere(normalized, "fa")
	favoriteWhere, favoriteFilterArgs := catalogSemanticMetadataFilterWhere(normalized, "fav")
	args := append([]any{}, textFilterArgs...)
	args = append(args,
		catalogSemanticFTSQuery(query),
		header.SourceKey,
		header.ModelID,
		header.VectorSpaceID,
		header.AssetGeneration,
		semanticSearchVisitBudget,
	)
	args = append(args, favoriteFilterArgs...)
	args = append(args,
		header.SourceKey,
		header.ModelID,
		header.VectorSpaceID,
		header.AssetGeneration,
		boolToSQLiteInt(semanticFavoriteQuery(query)),
		semanticSearchVisitBudget,
	)
	args = append(args, semanticSearchVisitBudget)
	rows, err := s.catalog.queryDB().QueryContext(ctx, `WITH
		text_rows(asset_id, ordinal) AS (
			SELECT fa.upstream_asset_id, tm.ordinal
			FROM catalog_assets_metadata_fts
			JOIN catalog_assets fa ON fa.rowid = catalog_assets_metadata_fts.rowid
			JOIN semantic_index_membership tm
				ON tm.source_key = fa.source_key
				AND tm.upstream_asset_id = fa.upstream_asset_id
			`+textWhere+`
				AND catalog_assets_metadata_fts MATCH ?
				AND tm.source_key = ?
				AND tm.model_id = ?
				AND tm.vector_space_id = ?
				AND tm.asset_generation = ?
			LIMIT ?
		),
		favorite_rows(asset_id, ordinal) AS (
			SELECT fav.upstream_asset_id, fm.ordinal
			FROM catalog_assets fav INDEXED BY idx_catalog_assets_metadata_favorite
			JOIN semantic_index_membership fm
				ON fm.source_key = fav.source_key
				AND fm.upstream_asset_id = fav.upstream_asset_id
			`+favoriteWhere+`
				AND fav.source_key = ?
				AND fm.model_id = ?
				AND fm.vector_space_id = ?
				AND fm.asset_generation = ?
				AND fav.is_favorite = 1
				AND ? = 1
			LIMIT ?
		),
		metadata_rows(asset_id, ordinal) AS (
			SELECT asset_id, ordinal FROM text_rows
			UNION
			SELECT asset_id, ordinal FROM favorite_rows
		)
		SELECT asset_id, ordinal
		FROM metadata_rows
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query catalog semantic metadata candidates: %w", err)
	}
	return scanCatalogSemanticMetadataCandidateRefs(rows)
}

func (s *Service) catalogSemanticShortMetadataCandidateRefs(
	ctx context.Context,
	normalized normalizedAssetSearch,
	query string,
	header semanticBinaryIndexHeader,
) ([]catalogSemanticMetadataCandidateRef, error) {
	where, filterArgs := catalogSemanticMetadataWhere(normalized, query, "a")
	args := append([]any{}, filterArgs...)
	args = append(args,
		header.SourceKey,
		header.ModelID,
		header.VectorSpaceID,
		header.AssetGeneration,
		semanticSearchVisitBudget,
	)
	rows, err := s.catalog.queryDB().QueryContext(ctx, `SELECT a.upstream_asset_id, m.ordinal
		FROM catalog_assets a
		JOIN semantic_index_membership m
			ON m.source_key = a.source_key
			AND m.upstream_asset_id = a.upstream_asset_id
		`+where+`
			AND m.source_key = ?
			AND m.model_id = ?
			AND m.vector_space_id = ?
			AND m.asset_generation = ?
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("query short catalog semantic metadata candidates: %w", err)
	}
	return scanCatalogSemanticMetadataCandidateRefs(rows)
}

func scanCatalogSemanticMetadataCandidateRefs(rows *sql.Rows) ([]catalogSemanticMetadataCandidateRef, error) {
	defer rows.Close()
	refs := make([]catalogSemanticMetadataCandidateRef, 0, semanticSearchVisitBudget)
	for rows.Next() {
		var ref catalogSemanticMetadataCandidateRef
		if err := rows.Scan(&ref.AssetID, &ref.Ordinal); err != nil {
			return nil, fmt.Errorf("scan catalog semantic metadata candidate: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate catalog semantic metadata candidates: %w", err)
	}
	return refs, nil
}

func catalogSemanticFTSQuery(query string) string {
	return `"` + strings.ReplaceAll(strings.TrimSpace(query), `"`, `""`) + `"`
}

func catalogSemanticMetadataWhere(normalized normalizedAssetSearch, query string, alias string) (string, []any) {
	where, args := catalogSemanticMetadataFilterWhere(normalized, alias)
	prefix := ""
	if strings.TrimSpace(alias) != "" {
		prefix = strings.TrimSpace(alias) + "."
	}
	like := "%" + escapeCatalogLike(strings.ToLower(query)) + "%"
	where += ` AND (
		lower(` + prefix + `filename) LIKE ? ESCAPE '\'
		OR lower(coalesce(` + prefix + `place_label, '')) LIKE ? ESCAPE '\'
		OR lower(coalesce(` + prefix + `description, '')) LIKE ? ESCAPE '\'
		OR (` + prefix + `is_favorite = 1 AND ? = 1)
	)`
	args = append(args, like, like, like, boolToSQLiteInt(semanticFavoriteQuery(query)))
	return where, args
}

func catalogSemanticMetadataFilterWhere(normalized normalizedAssetSearch, alias string) (string, []any) {
	prefix := ""
	if strings.TrimSpace(alias) != "" {
		prefix = strings.TrimSpace(alias) + "."
	}
	clauses := []string{
		"WHERE " + prefix + "visibility_status = 'active'",
	}
	args := []any{}
	if mediaTypes := normalized.Request.Collection.Filters.MediaTypes; len(mediaTypes) > 0 {
		placeholders := make([]string, 0, len(mediaTypes))
		for _, mediaType := range mediaTypes {
			placeholders = append(placeholders, "?")
			args = append(args, mediaType)
		}
		clauses = append(clauses, prefix+"media_type IN ("+strings.Join(placeholders, ", ")+")")
	}
	if capturedAt := normalized.Request.Collection.Filters.CapturedAt; capturedAt != nil {
		if capturedAt.From != nil {
			clauses = append(clauses, prefix+"captured_at >= ?")
			args = append(args, formatCatalogTime(capturedAt.From.UTC()))
		}
		if capturedAt.To != nil {
			clauses = append(clauses, prefix+"captured_at < ?")
			args = append(args, formatCatalogTime(capturedAt.To.UTC()))
		}
	}
	return strings.Join(clauses, " AND "), args
}

func promoteCatalogMetadataMatches(scored []semanticScoredAsset, keys map[string]struct{}) []semanticScoredAsset {
	if len(scored) == 0 || len(keys) == 0 {
		return scored
	}
	promoted := make([]semanticScoredAsset, 0, len(keys))
	rest := make([]semanticScoredAsset, 0, len(scored))
	for _, candidate := range scored {
		if _, ok := keys[semanticCatalogAssetKey(candidate.Asset.SourceKey, candidate.Asset.ID)]; ok {
			promoted = append(promoted, candidate)
			continue
		}
		rest = append(rest, candidate)
	}
	if len(promoted) == 0 {
		return scored
	}
	result := make([]semanticScoredAsset, 0, len(scored))
	result = append(result, promoted...)
	result = append(result, rest...)
	return result
}

func semanticCatalogAssetKey(sourceKey string, assetID string) string {
	return strings.TrimSpace(sourceKey) + "\x00" + strings.TrimSpace(assetID)
}
