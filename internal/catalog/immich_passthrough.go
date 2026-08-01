package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

type immichStatisticsTotalCacheKey struct {
	datasourceGeneration uint64
	filters              string
}

type immichStatisticsTotalCacheEntry struct {
	total     int
	expiresAt time.Time
	failed    bool
}

const immichStatisticsTotalCacheMaxEntries = 128

var errImmichStatisticsTemporarilyUnavailable = errors.New("Immich statistics temporarily unavailable during backoff")

// searchImmichPassthroughAssets translates the Timich catalog contract into
// the allowlisted Immich search endpoints without populating the local catalog.
func (s *Service) searchImmichPassthroughAssets(
	ctx context.Context,
	state *serviceDatasourceState,
	normalized normalizedAssetSearch,
) (AssetSearchPage, error) {
	if state == nil || state.primary == nil {
		return AssetSearchPage{}, ErrNoDatasourceConfigured
	}
	datasource := state.primary
	body, endpoint, totalAccuracy, err := immichPassthroughSearchRequest(normalized)
	if err != nil {
		return AssetSearchPage{}, err
	}

	request, err := s.newRequestForDatasource(datasource, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return AssetSearchPage{}, err
	}
	request = request.WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")

	response, err := s.client.Do(request)
	if err != nil {
		return AssetSearchPage{}, fmt.Errorf("perform Immich passthrough search request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return AssetSearchPage{}, fmt.Errorf("Immich passthrough search returned status %d", response.StatusCode)
	}

	var envelope searchAssetsEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return AssetSearchPage{}, fmt.Errorf("decode Immich passthrough search response: %w", err)
	}

	items := make([]Asset, 0, len(envelope.Assets.Items))
	for _, asset := range envelope.Assets.Items {
		items = append(items, Asset{
			ID:         asset.ID,
			Type:       normalizeAssetType(asset.Type),
			Filename:   asset.OriginalFileName,
			CapturedAt: asset.FileCreatedAt.Time.UTC(),
			Duration:   asset.Duration,
		})
	}

	var nextPageIndex *int
	if envelope.Assets.NextPage != nil {
		next := *envelope.Assets.NextPage - 1
		if next >= 0 {
			nextPageIndex = &next
		}
	}

	total := envelope.Assets.Total
	if shouldUseTimelineStatisticsTotal(normalized) || shouldUseFilteredTimelineStatisticsTotal(normalized) {
		if statisticsTotal, err := s.immichPassthroughTimelineAssetTotal(ctx, state, normalized.Request.Collection.Filters); err == nil {
			total = max(total, statisticsTotal)
			totalAccuracy = TotalAccuracyExact
		}
	}
	if len(items) > 0 {
		observedTotal := normalized.Request.Page.Index*normalized.Request.Page.Size + len(items)
		if total < observedTotal {
			total = observedTotal
			totalAccuracy = TotalAccuracyLowerBound
		}
	}
	if nextPageIndex != nil && total <= *nextPageIndex*normalized.Request.Page.Size {
		// Immich can report only the current page size in assets.total. Keep the
		// following page reachable with the smallest truthful lower bound: an
		// advertised next page contains at least its first item.
		total = *nextPageIndex*normalized.Request.Page.Size + 1
		totalAccuracy = TotalAccuracyLowerBound
	}

	return AssetSearchPage{
		CollectionKey: normalized.CollectionKey,
		Page:          normalized.Request.Page,
		Items:         items,
		Total:         total,
		TotalAccuracy: totalAccuracy,
		NextPageIndex: nextPageIndex,
		Boundary:      searchBoundary(normalized.Request.Page, len(items)),
		Resolved:      normalized.Resolved,
	}, nil
}

func immichPassthroughSearchRequest(normalized normalizedAssetSearch) ([]byte, string, string, error) {
	body := map[string]any{
		"page": normalized.Request.Page.Index + 1,
		"size": normalized.Request.Page.Size,
	}
	applyImmichPassthroughSearchFilters(body, normalized.Request.Collection.Filters)

	switch normalized.Resolved.QueryMode {
	case QueryModeSemantic:
		if normalized.Request.Collection.Query == nil {
			return nil, "", "", ErrInvalidSearchRequest
		}
		body["query"] = normalized.Request.Collection.Query.Text
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, "", "", fmt.Errorf("marshal Immich smart search request: %w", err)
		}
		return raw, "/api/search/smart", TotalAccuracyEstimated, nil
	case QueryModeFilename:
		if normalized.Request.Collection.Query == nil {
			return nil, "", "", ErrInvalidSearchRequest
		}
		body["originalFileName"] = normalized.Request.Collection.Query.Text
		fallthrough
	case QueryModeNone:
		body["order"] = SortDirectionDesc
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, "", "", fmt.Errorf("marshal Immich metadata search request: %w", err)
		}
		return raw, "/api/search/metadata", TotalAccuracyExact, nil
	default:
		return nil, "", "", ErrUnsupportedSearch
	}
}

func applyImmichPassthroughSearchFilters(body map[string]any, filters AssetSearchFilters) {
	if len(filters.MediaTypes) == 1 {
		body["type"] = immichAssetType(filters.MediaTypes[0])
	}
	if filters.CapturedAt == nil {
		return
	}
	if filters.CapturedAt.From != nil {
		body["takenAfter"] = filters.CapturedAt.From.UTC().Format(time.RFC3339Nano)
	}
	if filters.CapturedAt.To != nil {
		// Immich's takenBefore comparison is inclusive, while Timich's To bound
		// is exclusive. Move to the immediately preceding representable instant.
		body["takenBefore"] = filters.CapturedAt.To.UTC().Add(-time.Nanosecond).Format(time.RFC3339Nano)
	}
}

func immichAssetType(value string) string {
	if value == "video" {
		return "VIDEO"
	}
	return "IMAGE"
}

func shouldUseTimelineStatisticsTotal(normalized normalizedAssetSearch) bool {
	return normalized.Resolved.CollectionKind == CollectionKindTimeline &&
		normalized.Resolved.QueryMode == QueryModeNone &&
		!hasAssetSearchFilters(normalized.Request.Collection.Filters)
}

func shouldUseFilteredTimelineStatisticsTotal(normalized normalizedAssetSearch) bool {
	return normalized.Resolved.CollectionKind == CollectionKindTimeline &&
		normalized.Resolved.QueryMode == QueryModeNone &&
		hasAssetSearchFilters(normalized.Request.Collection.Filters)
}

func hasAssetSearchFilters(filters AssetSearchFilters) bool {
	if len(filters.MediaTypes) > 0 {
		return true
	}
	return filters.CapturedAt != nil &&
		(filters.CapturedAt.From != nil || filters.CapturedAt.To != nil)
}

func (s *Service) immichPassthroughTimelineAssetTotal(
	ctx context.Context,
	state *serviceDatasourceState,
	filters AssetSearchFilters,
) (int, error) {
	if state == nil || state.primary == nil {
		return 0, ErrNoDatasourceConfigured
	}
	rawBody, err := immichPassthroughStatisticsRequestBody(filters)
	if err != nil {
		return 0, err
	}
	cacheKey := immichStatisticsTotalCacheKey{
		datasourceGeneration: state.generation,
		filters:              string(rawBody),
	}
	now := time.Now()
	s.mu.Lock()
	if cached, ok := s.statisticsTotalCache[cacheKey]; ok {
		if cached.expiresAt.After(now) {
			s.mu.Unlock()
			if cached.failed {
				return 0, errImmichStatisticsTemporarilyUnavailable
			}
			return cached.total, nil
		}
		delete(s.statisticsTotalCache, cacheKey)
	}
	s.mu.Unlock()

	total, err := s.fetchImmichPassthroughTimelineStatisticsTotal(ctx, state.primary, rawBody)
	if err != nil && ctx.Err() != nil {
		return 0, err
	}

	s.mu.Lock()
	current := s.datasourceStateSnapshot()
	if current != nil && current.generation == state.generation {
		if s.statisticsTotalCache == nil {
			s.statisticsTotalCache = map[immichStatisticsTotalCacheKey]immichStatisticsTotalCacheEntry{}
		}
		cachedAt := time.Now()
		cacheTTL := statisticsTotalCacheTTL
		if err != nil {
			cacheTTL = statisticsFailureCacheTTL
		}
		var oldestKey immichStatisticsTotalCacheKey
		var oldestExpiration time.Time
		for key, cached := range s.statisticsTotalCache {
			if !cached.expiresAt.After(cachedAt) {
				delete(s.statisticsTotalCache, key)
				continue
			}
			if oldestExpiration.IsZero() || cached.expiresAt.Before(oldestExpiration) {
				oldestKey = key
				oldestExpiration = cached.expiresAt
			}
		}
		existing, exists := s.statisticsTotalCache[cacheKey]
		preserveSuccessfulTotal := err != nil && exists && !existing.failed && existing.expiresAt.After(cachedAt)
		if !preserveSuccessfulTotal {
			if !exists && len(s.statisticsTotalCache) >= immichStatisticsTotalCacheMaxEntries && !oldestExpiration.IsZero() {
				delete(s.statisticsTotalCache, oldestKey)
			}
			s.statisticsTotalCache[cacheKey] = immichStatisticsTotalCacheEntry{
				total:     total,
				expiresAt: cachedAt.Add(cacheTTL),
				failed:    err != nil,
			}
		}
	}
	s.mu.Unlock()
	return total, err
}

func immichPassthroughStatisticsRequestBody(filters AssetSearchFilters) ([]byte, error) {
	body := map[string]any{}
	applyImmichPassthroughSearchFilters(body, filters)
	rawBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal Immich statistics request: %w", err)
	}
	return rawBody, nil
}

func (s *Service) fetchImmichPassthroughTimelineStatisticsTotal(
	ctx context.Context,
	datasource *config.DatasourceConfig,
	rawBody []byte,
) (int, error) {
	request, err := s.newRequestForDatasource(datasource, http.MethodPost, "/api/search/statistics", bytes.NewReader(rawBody))
	if err != nil {
		return 0, err
	}
	statisticsCtx, cancel := context.WithTimeout(ctx, statisticsRequestTimeout)
	defer cancel()
	request = request.WithContext(statisticsCtx)
	request.Header.Set("Content-Type", "application/json")

	response, err := s.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("perform Immich statistics request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return 0, fmt.Errorf("Immich statistics request returned status %d", response.StatusCode)
	}

	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode Immich statistics response: %w", err)
	}

	total, ok := parseStatisticsTotal(payload)
	if !ok {
		return 0, fmt.Errorf("Immich statistics response missing total")
	}
	return max(0, total), nil
}

func parseStatisticsTotal(payload map[string]any) (int, bool) {
	if total := decodeFlexibleAnyInt(payload["total"]); total != nil {
		return *total, true
	}
	if assets, ok := payload["assets"].(map[string]any); ok {
		if total := decodeFlexibleAnyInt(assets["total"]); total != nil {
			return *total, true
		}
	}
	if photos := decodeFlexibleAnyInt(payload["photos"]); photos != nil {
		if videos := decodeFlexibleAnyInt(payload["videos"]); videos != nil {
			return max(0, *photos+*videos), true
		}
	}
	if images := decodeFlexibleAnyInt(payload["images"]); images != nil {
		if videos := decodeFlexibleAnyInt(payload["videos"]); videos != nil {
			return max(0, *images+*videos), true
		}
	}
	return 0, false
}

func decodeFlexibleAnyInt(value any) *int {
	switch typedValue := value.(type) {
	case int:
		return &typedValue
	case float64:
		converted := int(typedValue)
		return &converted
	case string:
		converted, err := strconv.Atoi(typedValue)
		if err == nil {
			return &converted
		}
	case json.Number:
		if converted, err := typedValue.Int64(); err == nil {
			intValue := int(converted)
			return &intValue
		}
	}
	return nil
}
