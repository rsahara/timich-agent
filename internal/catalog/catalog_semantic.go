package catalog

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

func (s *Service) searchCatalogWithoutSemanticProfile(ctx context.Context, normalized normalizedAssetSearch) (AssetSearchPage, error) {
	semantic := CatalogSemanticStatus{Status: "missing"}
	if semanticAutoRequested(normalized) {
		fallback, err := semanticFilenameFallback(normalized, semantic, nil)
		if err != nil {
			return AssetSearchPage{}, err
		}
		return s.catalog.SearchCatalogAssets(ctx, fallback)
	}
	resolved := normalized.Resolved
	resolved.Semantic = semanticResolution(semantic, false, nil)
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
	offset := normalized.Request.Page.Index * normalized.Request.Page.Size
	semantic = baseCatalogSemanticStatus(profile)
	directStatusSeen = false
	directStatusAllReady = true
	type sourceTraversal struct {
		session *semanticIndexTraversalSession
	}
	traversals := make([]sourceTraversal, 0, len(sourceKeys))
	defer func() {
		for _, traversal := range traversals {
			_ = traversal.session.Close()
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
		traversals = append(traversals, sourceTraversal{session: session})
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
	scored := make([]semanticScoredAsset, 0, len(traversals)*semanticSearchVisitBudget)
	candidateSeen := false
	for _, traversal := range traversals {
		batch, err := traversal.session.Advance(ctx, semanticSearchVisitBudget)
		if err != nil {
			return AssetSearchPage{}, err
		}
		if len(batch) == 0 {
			continue
		}
		candidateSeen = true
		scored = append(scored, batch...)
	}
	sortSemanticScoredAssets(scored)
	if semanticAutoRequested(normalized) {
		metadataMatches, err := s.catalogSemanticMetadataMatchKeys(ctx, normalized, scored)
		if err != nil {
			return AssetSearchPage{}, err
		}
		scored = promoteCatalogMetadataMatches(scored, metadataMatches)
	}
	scored = diversifySemanticCandidateSnapshot(scored)
	canonicalItems, err := s.catalog.canonicalAssetsForScoredSources(ctx, scored, true)
	if err != nil {
		return AssetSearchPage{}, err
	}
	filteredCanonicalItems := canonicalItems[:0]
	for _, item := range canonicalItems {
		if catalogAssetMatchesSearchFilters(item, normalized.Request.Collection.Filters) {
			filteredCanonicalItems = append(filteredCanonicalItems, item)
		}
	}
	canonicalItems = filteredCanonicalItems
	if !options.IncludeSemanticScores {
		for index := range canonicalItems {
			canonicalItems[index].SemanticScore = nil
		}
	}
	switch {
	case semantic.CompletedVectorCount > 0 && semantic.IndexedVectorCount == 0:
		semantic.Status = "backfilling"
	case semantic.IndexedVectorCount == 0:
		semantic.Status = "missing"
	case directStatusSeen && directStatusAllReady:
		semantic.Status = "ready"
	default:
		semantic.Status = "backfilling"
	}
	semantic = normalizeCatalogSemanticStatus(semantic, profile)
	log.Printf(
		"timich-agent catalog semantic search %s status completed model=%s vector_space=%s status=%s completed=%d indexed=%d elapsed=%s",
		statusMode,
		profile.ModelID(),
		profile.VectorSpaceID(),
		semantic.Status,
		semantic.CompletedVectorCount,
		semantic.IndexedVectorCount,
		time.Since(directStatusStarted).Round(time.Millisecond),
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

	end := min(offset+normalized.Request.Page.Size, len(canonicalItems))
	items := []Asset{}
	if offset < len(canonicalItems) {
		items = append(items, canonicalItems[offset:end]...)
	}
	var nextPageIndex *int
	if end < len(canonicalItems) {
		next := normalized.Request.Page.Index + 1
		nextPageIndex = &next
	}
	resolved := normalized.Resolved
	resolved.Semantic = semanticResolution(semantic, true, profile)
	return AssetSearchPage{
		CollectionKey: normalized.CollectionKey,
		Page:          normalized.Request.Page,
		Items:         items,
		Total:         len(canonicalItems),
		TotalAccuracy: TotalAccuracyEstimated,
		NextPageIndex: nextPageIndex,
		Boundary:      searchBoundary(normalized.Request.Page, len(items)),
		Resolved:      resolved,
	}, nil
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
		rows, err := s.catalog.db.QueryContext(ctx, builder.String(), args...)
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

func catalogSemanticMetadataWhere(normalized normalizedAssetSearch, query string, alias string) (string, []any) {
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
	like := "%" + escapeCatalogLike(strings.ToLower(query)) + "%"
	clauses = append(clauses, `(
		lower(`+prefix+`filename) LIKE ? ESCAPE '\'
		OR lower(coalesce(`+prefix+`place_label, '')) LIKE ? ESCAPE '\'
		OR lower(coalesce(`+prefix+`description, '')) LIKE ? ESCAPE '\'
		OR (`+prefix+`is_favorite = 1 AND ? = 1)
	)`)
	args = append(args, like, like, like, boolToSQLiteInt(semanticFavoriteQuery(query)))
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
