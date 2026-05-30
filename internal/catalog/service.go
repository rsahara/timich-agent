package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	imagedraw "image/draw"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
	_ "golang.org/x/image/webp"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
)

var ErrNoDatasourceConfigured = errors.New("no datasource configured")
var ErrMediaTooLarge = errors.New("media response too large")
var ErrAssetNotFound = errors.New("asset not found")
var ErrInvalidSearchRequest = errors.New("invalid search request")
var ErrUnsupportedSearch = errors.New("unsupported search")

const (
	previewSize                = "thumbnail"
	detailPreviewSize          = "preview"
	previewMaxEdgePixels       = 512
	previewMaxBytes            = 128 << 10
	detailPreviewMaxEdgePixels = 2560
	detailPreviewJPEGQuality   = 82
	detailPreviewMaxBytes      = 1 << 20
	detailPreviewMaxSource     = 32 << 20
	statisticsTotalCacheTTL    = time.Minute
	defaultPageSize            = 60
	maxPageSize                = 200
)

var (
	previewJPEGQualities       = []int{58, 50, 42}
	detailPreviewJPEGQualities = []int{detailPreviewJPEGQuality, 70, 58, 50, 42}
)

// Asset matches the Timich app-facing asset model returned to clients.
type Asset struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Filename   string    `json:"filename"`
	CapturedAt time.Time `json:"capturedAt"`
	Duration   *string   `json:"duration,omitempty"`
}

// AssetSearchRequest describes a page from a browsable Timich asset collection.
type AssetSearchRequest struct {
	Collection AssetCollectionRequest `json:"collection"`
	Page       AssetSearchPageRequest `json:"page"`
}

type AssetCollectionRequest struct {
	Kind    string             `json:"kind"`
	Query   *AssetSearchQuery  `json:"query,omitempty"`
	Filters AssetSearchFilters `json:"filters,omitempty"`
	Sort    *AssetSearchSort   `json:"sort,omitempty"`
}

type AssetSearchQuery struct {
	Text string `json:"text,omitempty"`
	Mode string `json:"mode,omitempty"`
}

type AssetSearchFilters struct {
	MediaTypes []string                 `json:"mediaTypes,omitempty"`
	CapturedAt *AssetSearchCapturedTime `json:"capturedAt,omitempty"`
}

type AssetSearchCapturedTime struct {
	From *time.Time `json:"from,omitempty"`
	To   *time.Time `json:"to,omitempty"`
}

func (f *AssetSearchCapturedTime) UnmarshalJSON(data []byte) error {
	var raw struct {
		From *string `json:"from"`
		To   *string `json:"to"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.From != nil {
		from, err := parseUTCSearchTime(*raw.From)
		if err != nil {
			return err
		}
		f.From = &from
	}
	if raw.To != nil {
		to, err := parseUTCSearchTime(*raw.To)
		if err != nil {
			return err
		}
		f.To = &to
	}
	return nil
}

type AssetSearchSort struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
}

type AssetSearchPageRequest struct {
	Index int `json:"index"`
	Size  int `json:"size"`
}

// AssetSearchPage summarizes one paginated asset collection response.
type AssetSearchPage struct {
	CollectionKey string                 `json:"collectionKey"`
	Page          AssetSearchPageRequest `json:"page"`
	Items         []Asset                `json:"items"`
	Total         int                    `json:"total"`
	TotalAccuracy string                 `json:"totalAccuracy"`
	NextPageIndex *int                   `json:"nextPageIndex,omitempty"`
	Boundary      *AssetSearchBoundary   `json:"boundary,omitempty"`
	Resolved      AssetSearchResolved    `json:"resolved"`
}

type AssetSearchBoundary struct {
	Kind string `json:"kind"`
}

type AssetSearchResolved struct {
	CollectionKind string          `json:"collectionKind"`
	QueryMode      string          `json:"queryMode"`
	Sort           AssetSearchSort `json:"sort"`
	TimelineLike   bool            `json:"timelineLike"`
}

type AssetSearchCapabilities struct {
	QueryModes    []string                      `json:"queryModes"`
	Filters       AssetSearchFilterCapabilities `json:"filters"`
	Sorts         []AssetSearchSortCapability   `json:"sorts"`
	TotalAccuracy []string                      `json:"totalAccuracy"`
	Page          AssetSearchPageCapabilities   `json:"page"`
}

type AssetSearchFilterCapabilities struct {
	MediaTypes []string `json:"mediaTypes"`
	CapturedAt bool     `json:"capturedAt"`
}

type AssetSearchSortCapability struct {
	Field      string   `json:"field"`
	Directions []string `json:"directions"`
}

type AssetSearchPageCapabilities struct {
	MaxSize int `json:"maxSize"`
}

type normalizedAssetSearch struct {
	Request       AssetSearchRequest
	Resolved      AssetSearchResolved
	CollectionKey string
}

const (
	CollectionKindTimeline = "timeline"
	CollectionKindSearch   = "search"

	QueryModeNone     = "none"
	QueryModeAuto     = "auto"
	QueryModeSemantic = "semantic"
	QueryModeFilename = "filename"

	SortFieldCapturedAt = "capturedAt"
	SortFieldRelevance  = "relevance"
	SortDirectionDesc   = "desc"

	TotalAccuracyExact      = "exact"
	TotalAccuracyEstimated  = "estimated"
	TotalAccuracyLowerBound = "lowerBound"

	BoundaryPastEnd = "pastEnd"
)

// UpstreamMediaResponse holds a proxied upstream media response.
type UpstreamMediaResponse struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

type photoDetailTiming struct {
	Profile         string
	UpstreamHeaders time.Duration
	ReadOriginal    time.Duration
	Decode          time.Duration
	Transform       time.Duration
	Encode          time.Duration
	Total           time.Duration
	OriginalBytes   int
	OutputBytes     int
	SourceWidth     int
	SourceHeight    int
	OutputWidth     int
	OutputHeight    int
	Format          string
}

type hostedImageProfile struct {
	Name           string
	UpstreamSize   string
	MaxEdgePixels  int
	MaxBytes       int
	JPEGQualities  []int
	FileNameBase   string
	FileNameSuffix string
	ForceJPEG      bool
}

// Service proxies the first configured Immich datasource for local catalog/media reads.
type Service struct {
	client *http.Client

	mu            sync.Mutex
	datasource    *config.DatasourceConfig
	staticDemo    *staticDemoSource
	staticDemoErr error
	cachedTotal   int
	cachedTotalAt time.Time
}

// NewService creates a local catalog/media proxy for the first configured datasource.
func NewService(datasources []config.DatasourceConfig) *Service {
	service := &Service{
		client: &http.Client{Timeout: 30 * time.Second},
	}
	if len(datasources) > 0 {
		datasource := datasources[0]
		service.datasource = &datasource
		if datasource.Kind == config.DatasourceKindStaticDemo {
			service.staticDemo, service.staticDemoErr = newStaticDemoSource(datasource.URL)
		}
	}
	return service
}

// Ready reports whether an upstream datasource is configured.
func (s *Service) Ready() bool {
	if s.datasource == nil {
		return false
	}
	if s.datasource.Kind == config.DatasourceKindStaticDemo {
		return s.staticDemo != nil && s.staticDemoErr == nil
	}
	return true
}

// SearchAssets returns one paginated asset page from the configured datasource.
func (s *Service) SearchAssets(searchRequest AssetSearchRequest) (AssetSearchPage, error) {
	if !s.Ready() {
		return AssetSearchPage{}, ErrNoDatasourceConfigured
	}
	normalized, err := normalizeAssetSearchRequest(searchRequest)
	if err != nil {
		return AssetSearchPage{}, err
	}
	if s.datasource.Kind == config.DatasourceKindStaticDemo {
		if s.staticDemoErr != nil {
			return AssetSearchPage{}, s.staticDemoErr
		}
		return s.staticDemo.SearchAssets(normalized)
	}

	return s.searchImmichAssets(normalized)
}

// Probe verifies that the active datasource is reachable from the agent runtime.
func (s *Service) Probe(ctx context.Context) error {
	if !s.Ready() {
		return ErrNoDatasourceConfigured
	}
	if s.datasource.Kind == config.DatasourceKindStaticDemo {
		return nil
	}

	request, err := s.newRequest(
		http.MethodPost,
		"/api/search/metadata",
		strings.NewReader(`{"page":1,"size":1,"order":"desc"}`),
	)
	if err != nil {
		return err
	}
	request = request.WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")

	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("perform datasource probe: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("datasource probe returned status %d", response.StatusCode)
	}
	return nil
}

func (s *Service) searchImmichAssets(normalized normalizedAssetSearch) (AssetSearchPage, error) {
	body, endpoint, totalAccuracy, err := immichSearchRequest(normalized)
	if err != nil {
		return AssetSearchPage{}, err
	}

	request, err := s.newRequest(
		http.MethodPost,
		endpoint,
		bytes.NewReader(body),
	)
	if err != nil {
		return AssetSearchPage{}, err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := s.client.Do(request)
	if err != nil {
		return AssetSearchPage{}, fmt.Errorf("perform search request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return AssetSearchPage{}, fmt.Errorf("search request returned status %d", response.StatusCode)
	}

	var envelope searchAssetsEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return AssetSearchPage{}, fmt.Errorf("decode search response: %w", err)
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
	if shouldUseTimelineStatisticsTotal(normalized) {
		if statisticsTotal, err := s.timelineAssetTotal(); err == nil {
			total = max(total, statisticsTotal)
			totalAccuracy = TotalAccuracyExact
		}
	} else if shouldUseFilteredTimelineStatisticsTotal(normalized) {
		if statisticsTotal, err := s.fetchTimelineStatisticsTotal(normalized.Request.Collection.Filters); err == nil {
			total = max(total, statisticsTotal)
			totalAccuracy = TotalAccuracyExact
		}
	}
	if total == 0 && len(items) > 0 {
		total = normalized.Request.Page.Index*normalized.Request.Page.Size + len(items)
		totalAccuracy = TotalAccuracyLowerBound
	}

	boundary := searchBoundary(normalized.Request.Page, len(items))
	return AssetSearchPage{
		CollectionKey: normalized.CollectionKey,
		Page:          normalized.Request.Page,
		Items:         items,
		Total:         total,
		TotalAccuracy: totalAccuracy,
		NextPageIndex: nextPageIndex,
		Boundary:      boundary,
		Resolved:      normalized.Resolved,
	}, nil
}

func shouldUseTimelineStatisticsTotal(normalized normalizedAssetSearch) bool {
	if normalized.Resolved.CollectionKind != CollectionKindTimeline || normalized.Resolved.QueryMode != QueryModeNone {
		return false
	}
	return !hasAssetSearchFilters(normalized.Request.Collection.Filters)
}

func shouldUseFilteredTimelineStatisticsTotal(normalized normalizedAssetSearch) bool {
	if normalized.Resolved.CollectionKind != CollectionKindTimeline || normalized.Resolved.QueryMode != QueryModeNone {
		return false
	}
	return hasAssetSearchFilters(normalized.Request.Collection.Filters)
}

func hasAssetSearchFilters(filters AssetSearchFilters) bool {
	if len(filters.MediaTypes) > 0 {
		return true
	}
	return filters.CapturedAt != nil &&
		(filters.CapturedAt.From != nil || filters.CapturedAt.To != nil)
}

// CatalogPage preserves internal callers while the public API moves to SearchAssets.
func (s *Service) CatalogPage(pageIndex int, pageSize int) (AssetSearchPage, error) {
	return s.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page: AssetSearchPageRequest{
			Index: pageIndex,
			Size:  pageSize,
		},
	})
}

func searchBoundary(page AssetSearchPageRequest, itemCount int) *AssetSearchBoundary {
	if page.Index <= 0 || itemCount > 0 {
		return nil
	}
	return &AssetSearchBoundary{Kind: BoundaryPastEnd}
}

func immichSearchRequest(normalized normalizedAssetSearch) ([]byte, string, string, error) {
	body := map[string]any{
		"page": normalized.Request.Page.Index + 1,
		"size": normalized.Request.Page.Size,
	}
	applyImmichSearchFilters(body, normalized.Request.Collection.Filters)

	switch normalized.Resolved.QueryMode {
	case QueryModeSemantic:
		if normalized.Request.Collection.Query == nil {
			return nil, "", "", ErrInvalidSearchRequest
		}
		body["query"] = normalized.Request.Collection.Query.Text
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, "", "", fmt.Errorf("marshal smart search request: %w", err)
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
			return nil, "", "", fmt.Errorf("marshal metadata search request: %w", err)
		}
		return raw, "/api/search/metadata", TotalAccuracyExact, nil
	default:
		return nil, "", "", ErrUnsupportedSearch
	}
}

func applyImmichSearchFilters(body map[string]any, filters AssetSearchFilters) {
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
		body["takenBefore"] = filters.CapturedAt.To.UTC().Format(time.RFC3339Nano)
	}
}

func normalizeAssetSearchRequest(searchRequest AssetSearchRequest) (normalizedAssetSearch, error) {
	request := searchRequest
	request.Collection.Kind = strings.TrimSpace(request.Collection.Kind)
	if request.Collection.Kind == "" {
		request.Collection.Kind = CollectionKindTimeline
	}
	if request.Collection.Kind != CollectionKindTimeline && request.Collection.Kind != CollectionKindSearch {
		return normalizedAssetSearch{}, fmt.Errorf("%w: unsupported collection kind", ErrInvalidSearchRequest)
	}

	if request.Page.Index < 0 {
		return normalizedAssetSearch{}, fmt.Errorf("%w: page index must be non-negative", ErrInvalidSearchRequest)
	}
	if request.Page.Size == 0 {
		request.Page.Size = defaultPageSize
	}
	if request.Page.Size < 1 {
		return normalizedAssetSearch{}, fmt.Errorf("%w: page size must be positive", ErrInvalidSearchRequest)
	}
	if request.Page.Size > maxPageSize {
		request.Page.Size = maxPageSize
	}

	mediaTypes, err := normalizeMediaTypes(request.Collection.Filters.MediaTypes)
	if err != nil {
		return normalizedAssetSearch{}, err
	}
	request.Collection.Filters.MediaTypes = mediaTypes
	if request.Collection.Filters.CapturedAt != nil {
		from := request.Collection.Filters.CapturedAt.From
		to := request.Collection.Filters.CapturedAt.To
		if from != nil {
			utc := from.UTC()
			request.Collection.Filters.CapturedAt.From = &utc
		}
		if to != nil {
			utc := to.UTC()
			request.Collection.Filters.CapturedAt.To = &utc
		}
		if request.Collection.Filters.CapturedAt.From != nil &&
			request.Collection.Filters.CapturedAt.To != nil &&
			!request.Collection.Filters.CapturedAt.From.Before(*request.Collection.Filters.CapturedAt.To) {
			return normalizedAssetSearch{}, fmt.Errorf("%w: capturedAt from must be before to", ErrInvalidSearchRequest)
		}
	}

	var text string
	queryMode := QueryModeNone
	if request.Collection.Query != nil {
		text = strings.TrimSpace(request.Collection.Query.Text)
		mode := strings.TrimSpace(request.Collection.Query.Mode)
		if mode == "" {
			mode = QueryModeAuto
		}
		request.Collection.Query.Text = text
		request.Collection.Query.Mode = mode
		switch mode {
		case QueryModeAuto:
			if text != "" {
				queryMode = QueryModeSemantic
			}
		case QueryModeSemantic, QueryModeFilename:
			if text == "" {
				return normalizedAssetSearch{}, fmt.Errorf("%w: query text is required", ErrInvalidSearchRequest)
			}
			queryMode = mode
		default:
			return normalizedAssetSearch{}, fmt.Errorf("%w: unsupported query mode", ErrInvalidSearchRequest)
		}
	}
	if request.Collection.Kind == CollectionKindTimeline && queryMode != QueryModeNone {
		return normalizedAssetSearch{}, fmt.Errorf("%w: timeline collection does not accept query text", ErrInvalidSearchRequest)
	}

	resolvedSort := AssetSearchSort{Field: SortFieldCapturedAt, Direction: SortDirectionDesc}
	if queryMode == QueryModeSemantic {
		resolvedSort.Field = SortFieldRelevance
	}
	if request.Collection.Sort != nil {
		field := strings.TrimSpace(request.Collection.Sort.Field)
		direction := strings.TrimSpace(request.Collection.Sort.Direction)
		if field == "" {
			field = resolvedSort.Field
		}
		if direction == "" {
			direction = SortDirectionDesc
		}
		if direction != SortDirectionDesc || field != resolvedSort.Field {
			return normalizedAssetSearch{}, fmt.Errorf("%w: unsupported sort", ErrUnsupportedSearch)
		}
		request.Collection.Sort = &AssetSearchSort{Field: field, Direction: direction}
	} else {
		request.Collection.Sort = &resolvedSort
	}

	resolved := AssetSearchResolved{
		CollectionKind: request.Collection.Kind,
		QueryMode:      queryMode,
		Sort:           resolvedSort,
		TimelineLike:   queryMode != QueryModeSemantic,
	}
	key, err := collectionKey(request, resolved)
	if err != nil {
		return normalizedAssetSearch{}, err
	}
	return normalizedAssetSearch{Request: request, Resolved: resolved, CollectionKey: key}, nil
}

func normalizeMediaTypes(values []string) ([]string, error) {
	seen := map[string]struct{}{}
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "":
		case "image":
			seen["image"] = struct{}{}
		case "video":
			seen["video"] = struct{}{}
		default:
			return nil, fmt.Errorf("%w: unsupported media type", ErrInvalidSearchRequest)
		}
	}
	result := make([]string, 0, len(seen))
	for _, value := range []string{"image", "video"} {
		if _, ok := seen[value]; ok {
			result = append(result, value)
		}
	}
	return result, nil
}

func parseUTCSearchTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasSuffix(value, "Z") {
		return time.Time{}, fmt.Errorf("%w: capturedAt filters require UTC RFC3339 timestamps", ErrInvalidSearchRequest)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: capturedAt filters require UTC RFC3339 timestamps", ErrInvalidSearchRequest)
	}
	return parsed.UTC(), nil
}

func collectionKey(request AssetSearchRequest, resolved AssetSearchResolved) (string, error) {
	canonical := struct {
		Collection AssetCollectionRequest `json:"collection"`
		Resolved   AssetSearchResolved    `json:"resolved"`
		Version    int                    `json:"version"`
	}{
		Collection: request.Collection,
		Resolved:   resolved,
		Version:    1,
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "search_v1:" + base64.RawURLEncoding.EncodeToString(sum[:16]), nil
}

func normalizeAssetType(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "VIDEO":
		return "video"
	default:
		return "image"
	}
}

func immichAssetType(value string) string {
	switch value {
	case "video":
		return "VIDEO"
	default:
		return "IMAGE"
	}
}

// SearchCapabilities returns the search features supported by the active datasource.
func (s *Service) SearchCapabilities() AssetSearchCapabilities {
	queryModes := []string{}
	sorts := []AssetSearchSortCapability{
		{Field: SortFieldCapturedAt, Directions: []string{SortDirectionDesc}},
	}
	totalAccuracy := []string{TotalAccuracyExact}
	if s.datasource != nil && s.datasource.Kind != config.DatasourceKindStaticDemo {
		queryModes = []string{QueryModeAuto, QueryModeSemantic, QueryModeFilename}
		sorts = append(sorts, AssetSearchSortCapability{
			Field:      SortFieldRelevance,
			Directions: []string{SortDirectionDesc},
		})
		totalAccuracy = []string{TotalAccuracyExact, TotalAccuracyEstimated, TotalAccuracyLowerBound}
	}
	return AssetSearchCapabilities{
		QueryModes: queryModes,
		Filters: AssetSearchFilterCapabilities{
			MediaTypes: []string{"image", "video"},
			CapturedAt: true,
		},
		Sorts:         sorts,
		TotalAccuracy: totalAccuracy,
		Page:          AssetSearchPageCapabilities{MaxSize: maxPageSize},
	}
}

func (s *Service) timelineAssetTotal() (int, error) {
	now := time.Now()
	s.mu.Lock()
	if !s.cachedTotalAt.IsZero() && now.Sub(s.cachedTotalAt) < statisticsTotalCacheTTL {
		total := s.cachedTotal
		s.mu.Unlock()
		return total, nil
	}
	s.mu.Unlock()

	total, err := s.fetchTimelineAssetTotal()
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	s.cachedTotal = total
	s.cachedTotalAt = now
	s.mu.Unlock()
	return total, nil
}

func (s *Service) fetchTimelineAssetTotal() (int, error) {
	return s.fetchTimelineStatisticsTotal(AssetSearchFilters{})
}

func (s *Service) fetchTimelineStatisticsTotal(filters AssetSearchFilters) (int, error) {
	body := map[string]any{}
	applyImmichSearchFilters(body, filters)
	rawBody, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("marshal statistics request: %w", err)
	}

	request, err := s.newRequest(
		http.MethodPost,
		"/api/search/statistics",
		bytes.NewReader(rawBody),
	)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := s.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("perform statistics request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return 0, fmt.Errorf("statistics request returned status %d", response.StatusCode)
	}

	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode statistics response: %w", err)
	}

	total, ok := parseStatisticsTotal(payload)
	if !ok {
		return 0, fmt.Errorf("statistics response missing total")
	}
	return max(0, total), nil
}

// Preview returns the lightweight image profile from the configured datasource.
func (s *Service) Preview(clientRequest *http.Request, assetID string) (*UpstreamMediaResponse, error) {
	if !s.Ready() {
		return nil, ErrNoDatasourceConfigured
	}
	if s.datasource.Kind == config.DatasourceKindStaticDemo {
		if s.staticDemoErr != nil {
			return nil, s.staticDemoErr
		}
		return s.staticDemo.MediaResponse(clientRequest, assetID, "preview")
	}
	return s.profileImage(clientRequest, assetID, hostedImageProfile{
		Name:           "preview",
		UpstreamSize:   previewSize,
		MaxEdgePixels:  previewMaxEdgePixels,
		MaxBytes:       previewMaxBytes,
		JPEGQualities:  previewJPEGQualities,
		FileNameBase:   "preview",
		FileNameSuffix: "_preview",
	})
}

// DetailPreview returns the detail-preview image profile from the configured datasource.
func (s *Service) DetailPreview(clientRequest *http.Request, assetID string) (*UpstreamMediaResponse, error) {
	if !s.Ready() {
		return nil, ErrNoDatasourceConfigured
	}
	if s.datasource.Kind == config.DatasourceKindStaticDemo {
		if s.staticDemoErr != nil {
			return nil, s.staticDemoErr
		}
		return s.staticDemo.MediaResponse(clientRequest, assetID, "detail_preview")
	}
	return s.profileImage(clientRequest, assetID, hostedImageProfile{
		Name:           "detail_preview",
		UpstreamSize:   detailPreviewSize,
		MaxEdgePixels:  detailPreviewMaxEdgePixels,
		MaxBytes:       detailPreviewMaxBytes,
		JPEGQualities:  detailPreviewJPEGQualities,
		FileNameBase:   "detail_preview",
		FileNameSuffix: "_detail_preview",
	})
}

func (s *Service) profileImage(
	clientRequest *http.Request,
	assetID string,
	profile hostedImageProfile,
) (*UpstreamMediaResponse, error) {
	totalStartedAt := time.Now()
	upstreamStartedAt := time.Now()
	upstreamSize := profile.UpstreamSize
	if upstreamSize == "" {
		upstreamSize = detailPreviewSize
	}
	upstream, err := s.proxyMedia(profileUpstreamRequest(clientRequest), "/api/assets/"+url.PathEscape(assetID)+"/thumbnail?size="+upstreamSize, nil)
	if err != nil {
		return nil, err
	}
	timing := photoDetailTiming{
		UpstreamHeaders: time.Since(upstreamStartedAt),
		Profile:         profile.Name,
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		timing.Total = time.Since(totalStartedAt)
		upstream.Header.Set("Server-Timing", photoDetailServerTiming(timing))
		logPhotoDetailTiming(clientRequest, upstream.StatusCode, false, timing)
		return upstream, nil
	}
	if contentLengthExceeds(upstream.Header.Get("Content-Length"), detailPreviewMaxSource) {
		upstream.Body.Close()
		return nil, ErrMediaTooLarge
	}

	readStartedAt := time.Now()
	body, err := readAtMost(upstream.Body, detailPreviewMaxSource)
	upstream.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read hosted media response: %w", err)
	}
	timing.ReadOriginal = time.Since(readStartedAt)
	timing.OriginalBytes = len(body)

	encodedBody, ok, resizeTiming, err := renderHostedImage(body, profile)
	timing.Decode = resizeTiming.Decode
	timing.Transform = resizeTiming.Transform
	timing.Encode = resizeTiming.Encode
	timing.SourceWidth = resizeTiming.SourceWidth
	timing.SourceHeight = resizeTiming.SourceHeight
	timing.OutputWidth = resizeTiming.OutputWidth
	timing.OutputHeight = resizeTiming.OutputHeight
	timing.OutputBytes = resizeTiming.OutputBytes
	timing.Format = resizeTiming.Format
	if err != nil {
		return nil, fmt.Errorf("render hosted media: %w", err)
	}
	timing.Total = time.Since(totalStartedAt)
	if !ok {
		upstream.Body = io.NopCloser(bytes.NewReader(body))
		upstream.Header.Set("Content-Length", strconv.Itoa(len(body)))
		upstream.Header.Set("Server-Timing", photoDetailServerTiming(timing))
		logPhotoDetailTiming(clientRequest, upstream.StatusCode, false, timing)
		return upstream, nil
	}
	timing.OutputBytes = len(encodedBody)

	header := make(http.Header)
	if cacheControl := upstream.Header.Get("Cache-Control"); cacheControl != "" {
		header.Set("Cache-Control", cacheControl)
	}
	if lastModified := upstream.Header.Get("Last-Modified"); lastModified != "" {
		header.Set("Last-Modified", lastModified)
	}
	if fileName := hostedImageFileName(upstream.Header.Get("Content-Disposition"), assetID, profile); fileName != "" {
		header.Set("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape(fileName))
	}
	header.Set("Content-Type", "image/jpeg")
	header.Set("Content-Length", strconv.Itoa(len(encodedBody)))
	header.Set("Server-Timing", photoDetailServerTiming(timing))
	logPhotoDetailTiming(clientRequest, upstream.StatusCode, true, timing)

	return &UpstreamMediaResponse{
		StatusCode: upstream.StatusCode,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(encodedBody)),
	}, nil
}

// Original proxies an original asset request from the configured datasource.
func (s *Service) Original(clientRequest *http.Request, assetID string) (*UpstreamMediaResponse, error) {
	if !s.Ready() {
		return nil, ErrNoDatasourceConfigured
	}
	if s.datasource.Kind == config.DatasourceKindStaticDemo {
		if s.staticDemoErr != nil {
			return nil, s.staticDemoErr
		}
		return s.staticDemo.MediaResponse(clientRequest, assetID, "original")
	}
	return s.proxyMedia(clientRequest, "/api/assets/"+url.PathEscape(assetID)+"/original", nil)
}

func (s *Service) proxyMedia(clientRequest *http.Request, path string, body io.Reader) (*UpstreamMediaResponse, error) {
	if !s.Ready() {
		return nil, ErrNoDatasourceConfigured
	}

	method := http.MethodGet
	if clientRequest != nil && clientRequest.Method != "" {
		method = clientRequest.Method
	}

	request, err := s.newRequest(method, path, body)
	if err != nil {
		return nil, err
	}
	if clientRequest != nil {
		request = request.WithContext(clientRequest.Context())
	}
	applyProxyRequestHeaders(request, clientRequest)
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("perform media request: %w", err)
	}
	return &UpstreamMediaResponse{
		StatusCode: response.StatusCode,
		Header:     response.Header.Clone(),
		Body:       response.Body,
	}, nil
}

func profileUpstreamRequest(clientRequest *http.Request) *http.Request {
	if clientRequest == nil || clientRequest.Method != http.MethodHead {
		return clientRequest
	}
	upstreamRequest := clientRequest.Clone(clientRequest.Context())
	upstreamRequest.Method = http.MethodGet
	return upstreamRequest
}

func (s *Service) newRequest(method string, path string, body io.Reader) (*http.Request, error) {
	baseURL, err := url.Parse(s.datasource.URL)
	if err != nil {
		return nil, fmt.Errorf("parse datasource URL: %w", err)
	}
	resolvedURL, err := baseURL.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("resolve datasource path: %w", err)
	}
	request, err := http.NewRequest(method, resolvedURL.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create upstream request: %w", err)
	}
	applyDatasourceAuth(request, s.datasource.AccessToken)
	return request, nil
}

func applyDatasourceAuth(request *http.Request, accessToken string) {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		return
	}
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		request.Header.Set("Authorization", token)
		return
	}
	request.Header.Set("x-api-key", token)
}

func applyProxyRequestHeaders(upstreamRequest *http.Request, clientRequest *http.Request) {
	if clientRequest == nil {
		return
	}
	for _, headerName := range []string{
		"Range",
		"If-Range",
		"If-None-Match",
		"If-Modified-Since",
		"Accept",
		"Accept-Encoding",
	} {
		if headerValue := clientRequest.Header.Get(headerName); headerValue != "" {
			upstreamRequest.Header.Set(headerName, headerValue)
		}
	}
}

func isHostedRequest(clientRequest *http.Request) bool {
	if clientRequest == nil {
		return false
	}
	return strings.TrimSpace(clientRequest.Header.Get("X-Timich-Hosted-Base-URL")) != ""
}

func photoDetailServerTiming(timing photoDetailTiming) string {
	parts := []string{
		serverTimingMetric("immich_headers", timing.UpstreamHeaders),
		serverTimingMetric("read_original", timing.ReadOriginal),
		serverTimingMetric("decode", timing.Decode),
		serverTimingMetric("transform", timing.Transform),
		serverTimingMetric("encode", timing.Encode),
		serverTimingMetric("total", timing.Total),
	}
	return strings.Join(parts, ", ")
}

func serverTimingMetric(name string, duration time.Duration) string {
	return fmt.Sprintf("%s;dur=%.1f", name, float64(duration.Microseconds())/1000)
}

func logPhotoDetailTiming(
	clientRequest *http.Request,
	statusCode int,
	resized bool,
	timing photoDetailTiming,
) {
	log.Printf(
		"hosted media timing profile=%q hosted=%t status=%d resized=%t format=%q original_bytes=%d original_mib=%.2f output_bytes=%d output_kib=%.1f compression_ratio=%.3f src=%dx%d dst=%dx%d immich_headers_ms=%d read_source_ms=%d decode_ms=%d transform_ms=%d encode_ms=%d total_ms=%d",
		timing.Profile,
		isHostedRequest(clientRequest),
		statusCode,
		resized,
		timing.Format,
		timing.OriginalBytes,
		bytesToMiB(timing.OriginalBytes),
		timing.OutputBytes,
		bytesToKiB(timing.OutputBytes),
		compressionRatio(timing.OutputBytes, timing.OriginalBytes),
		timing.SourceWidth,
		timing.SourceHeight,
		timing.OutputWidth,
		timing.OutputHeight,
		timing.UpstreamHeaders.Milliseconds(),
		timing.ReadOriginal.Milliseconds(),
		timing.Decode.Milliseconds(),
		timing.Transform.Milliseconds(),
		timing.Encode.Milliseconds(),
		timing.Total.Milliseconds(),
	)
}

func bytesToKiB(value int) float64 {
	return float64(max(0, value)) / 1024
}

func bytesToMiB(value int) float64 {
	return float64(max(0, value)) / (1024 * 1024)
}

func compressionRatio(outputBytes int, originalBytes int) float64 {
	if originalBytes <= 0 {
		return 0
	}
	return float64(max(0, outputBytes)) / float64(originalBytes)
}

func contentLengthExceeds(rawValue string, maxBytes int64) bool {
	contentLength, err := strconv.ParseInt(strings.TrimSpace(rawValue), 10, 64)
	return err == nil && contentLength > maxBytes
}

func readAtMost(reader io.Reader, maxBytes int64) ([]byte, error) {
	var body bytes.Buffer
	if _, err := body.ReadFrom(io.LimitReader(reader, maxBytes+1)); err != nil {
		return nil, err
	}
	if int64(body.Len()) > maxBytes {
		return nil, ErrMediaTooLarge
	}
	return body.Bytes(), nil
}

func renderHostedImage(body []byte, profile hostedImageProfile) ([]byte, bool, photoDetailTiming, error) {
	timing := photoDetailTiming{}
	decodeStartedAt := time.Now()
	srcImage, format, err := image.Decode(bytes.NewReader(body))
	timing.Decode = time.Since(decodeStartedAt)
	timing.Format = format
	if err != nil {
		if len(body) <= profile.MaxBytes {
			timing.OutputBytes = len(body)
			return body, false, timing, nil
		}
		return nil, false, timing, ErrMediaTooLarge
	}
	if !supportsHostedImageFormat(format) {
		if len(body) <= profile.MaxBytes {
			timing.OutputBytes = len(body)
			return body, false, timing, nil
		}
		return nil, false, timing, ErrMediaTooLarge
	}

	bounds := srcImage.Bounds()
	srcWidth := bounds.Dx()
	srcHeight := bounds.Dy()
	timing.SourceWidth = srcWidth
	timing.SourceHeight = srcHeight
	if srcWidth < 1 || srcHeight < 1 {
		return nil, false, timing, fmt.Errorf("decoded image has invalid bounds %dx%d", srcWidth, srcHeight)
	}

	orientation := 1
	if strings.EqualFold(format, "jpeg") || strings.EqualFold(format, "jpg") {
		orientation = jpegOrientation(body)
	}

	orientedWidth, orientedHeight := orientedDimensions(srcWidth, srcHeight, orientation)
	if !profile.ForceJPEG && max(orientedWidth, orientedHeight) <= profile.MaxEdgePixels && len(body) <= profile.MaxBytes {
		timing.OutputWidth = orientedWidth
		timing.OutputHeight = orientedHeight
		timing.OutputBytes = len(body)
		return body, false, timing, nil
	}

	scale := imageScale(orientedWidth, orientedHeight, profile.MaxEdgePixels)
	dstWidth, dstHeight := scaledDimensions(orientedWidth, orientedHeight, scale)
	timing.OutputWidth = dstWidth
	timing.OutputHeight = dstHeight
	dstImage := image.NewRGBA(image.Rect(0, 0, dstWidth, dstHeight))
	transformStartedAt := time.Now()
	imagedraw.Draw(dstImage, dstImage.Bounds(), &image.Uniform{C: color.White}, image.Point{}, imagedraw.Src)
	if orientation == 1 {
		xdraw.ApproxBiLinear.Scale(dstImage, dstImage.Bounds(), srcImage, bounds, xdraw.Over, nil)
	} else {
		xdraw.ApproxBiLinear.Transform(
			dstImage,
			photoDetailTransform(dstWidth, dstHeight, scale, orientation),
			srcImage,
			bounds,
			xdraw.Over,
			nil,
		)
	}
	timing.Transform = time.Since(transformStartedAt)

	encodeStartedAt := time.Now()
	var encoded bytes.Buffer
	qualities := profile.JPEGQualities
	if len(qualities) == 0 {
		qualities = detailPreviewJPEGQualities
	}
	for _, quality := range qualities {
		encoded.Reset()
		if err := jpeg.Encode(&encoded, dstImage, &jpeg.Options{Quality: quality}); err != nil {
			timing.Encode = time.Since(encodeStartedAt)
			return nil, false, timing, err
		}
		if encoded.Len() <= profile.MaxBytes {
			timing.Encode = time.Since(encodeStartedAt)
			timing.OutputBytes = encoded.Len()
			return encoded.Bytes(), true, timing, nil
		}
	}
	timing.Encode = time.Since(encodeStartedAt)
	timing.OutputBytes = encoded.Len()
	return nil, false, timing, ErrMediaTooLarge
}

type ImageVariantOptions struct {
	MaxEdgePixels int
	MaxBytes      int
	JPEGQualities []int
}

func RenderImageVariant(body []byte, options ImageVariantOptions) ([]byte, error) {
	if options.MaxEdgePixels <= 0 {
		return nil, ErrMediaTooLarge
	}
	if options.MaxBytes <= 0 {
		return nil, ErrMediaTooLarge
	}
	rendered, _, _, err := renderHostedImage(body, hostedImageProfile{
		Name:          "static_demo",
		MaxEdgePixels: options.MaxEdgePixels,
		MaxBytes:      options.MaxBytes,
		JPEGQualities: options.JPEGQualities,
		ForceJPEG:     true,
	})
	return rendered, err
}

func RenderStaticDemoPreview(body []byte) ([]byte, error) {
	return RenderImageVariant(body, ImageVariantOptions{
		MaxEdgePixels: previewMaxEdgePixels,
		MaxBytes:      previewMaxBytes,
		JPEGQualities: previewJPEGQualities,
	})
}

func RenderStaticDemoDetailPreview(body []byte) ([]byte, error) {
	return RenderImageVariant(body, ImageVariantOptions{
		MaxEdgePixels: detailPreviewMaxEdgePixels,
		MaxBytes:      detailPreviewMaxBytes,
		JPEGQualities: detailPreviewJPEGQualities,
	})
}

func RenderStaticDemoOriginal(body []byte) ([]byte, error) {
	return RenderImageVariant(body, ImageVariantOptions{
		MaxEdgePixels: 2400,
		MaxBytes:      2 << 20,
		JPEGQualities: []int{88, 82, 76, 70, 58, 50},
	})
}

func jpegOrientation(body []byte) int {
	if len(body) < 4 || body[0] != 0xFF || body[1] != 0xD8 {
		return 1
	}

	offset := 2
	for offset+4 <= len(body) {
		if body[offset] != 0xFF {
			break
		}
		marker := body[offset+1]
		offset += 2

		if marker == 0xD9 || marker == 0xDA {
			break
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}
		if offset+2 > len(body) {
			break
		}

		segmentLength := int(binary.BigEndian.Uint16(body[offset : offset+2]))
		if segmentLength < 2 || offset+segmentLength > len(body) {
			break
		}
		segmentData := body[offset+2 : offset+segmentLength]
		if marker == 0xE1 {
			if orientation, ok := exifOrientation(segmentData); ok {
				return orientation
			}
		}
		offset += segmentLength
	}

	return 1
}

func exifOrientation(segment []byte) (int, bool) {
	if len(segment) < 14 || !bytes.Equal(segment[:6], []byte("Exif\x00\x00")) {
		return 0, false
	}

	tiff := segment[6:]
	byteOrder := tiff[:2]
	var order binary.ByteOrder
	switch string(byteOrder) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0, false
	}

	if order.Uint16(tiff[2:4]) != 42 {
		return 0, false
	}

	ifdOffset := int(order.Uint32(tiff[4:8]))
	if ifdOffset < 8 || ifdOffset+2 > len(tiff) {
		return 0, false
	}

	entryCount := int(order.Uint16(tiff[ifdOffset : ifdOffset+2]))
	entryOffset := ifdOffset + 2
	for index := 0; index < entryCount; index++ {
		start := entryOffset + index*12
		end := start + 12
		if end > len(tiff) {
			return 0, false
		}

		entry := tiff[start:end]
		tag := order.Uint16(entry[0:2])
		if tag != 0x0112 {
			continue
		}
		fieldType := order.Uint16(entry[2:4])
		componentCount := order.Uint32(entry[4:8])
		if fieldType != 3 || componentCount != 1 {
			return 0, false
		}

		value := int(order.Uint16(entry[8:10]))
		if value < 1 || value > 8 {
			return 0, false
		}
		return value, true
	}

	return 0, false
}

func supportsHostedImageFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpeg", "jpg", "png", "gif", "webp":
		return true
	default:
		return false
	}
}

func imageScale(width int, height int, maxEdgePixels int) float64 {
	maxEdge := max(width, height)
	if maxEdge <= maxEdgePixels {
		return 1
	}
	return float64(maxEdgePixels) / float64(maxEdge)
}

func scaledDimensions(width int, height int, scale float64) (int, int) {
	resizedWidth := max(1, int(float64(width)*scale+0.5))
	resizedHeight := max(1, int(float64(height)*scale+0.5))
	return resizedWidth, resizedHeight
}

func orientedDimensions(width int, height int, orientation int) (int, int) {
	switch orientation {
	case 5, 6, 7, 8:
		return height, width
	default:
		return width, height
	}
}

func photoDetailTransform(dstWidth int, dstHeight int, scale float64, orientation int) f64.Aff3 {
	switch orientation {
	case 2:
		return f64.Aff3{-scale, 0, float64(dstWidth), 0, scale, 0}
	case 3:
		return f64.Aff3{-scale, 0, float64(dstWidth), 0, -scale, float64(dstHeight)}
	case 4:
		return f64.Aff3{scale, 0, 0, 0, -scale, float64(dstHeight)}
	case 5:
		return f64.Aff3{0, scale, 0, scale, 0, 0}
	case 6:
		return f64.Aff3{0, -scale, float64(dstWidth), scale, 0, 0}
	case 7:
		return f64.Aff3{0, -scale, float64(dstWidth), -scale, 0, float64(dstHeight)}
	case 8:
		return f64.Aff3{0, scale, 0, -scale, 0, float64(dstHeight)}
	default:
		return f64.Aff3{scale, 0, 0, 0, scale, 0}
	}
}

func hostedImageFileName(contentDisposition string, assetID string, profile hostedImageProfile) string {
	name := strings.TrimSpace(assetID)
	if contentDisposition != "" {
		if filename := contentDispositionFilename(contentDisposition); filename != "" {
			name = filename
		}
	}
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = strings.TrimSpace(assetID)
	}
	if base == "" {
		base = strings.TrimSpace(profile.FileNameBase)
	}
	if base == "" {
		base = "image"
	}
	return base + profile.FileNameSuffix + ".jpg"
}

func contentDispositionFilename(contentDisposition string) string {
	lowerValue := strings.ToLower(contentDisposition)
	if index := strings.Index(lowerValue, "filename*=utf-8''"); index >= 0 {
		rawValue := strings.TrimSpace(contentDisposition[index+len("filename*=UTF-8''"):])
		if separator := strings.Index(rawValue, ";"); separator >= 0 {
			rawValue = rawValue[:separator]
		}
		if decodedValue, err := url.PathUnescape(strings.Trim(rawValue, "\"")); err == nil {
			return decodedValue
		}
	}
	if index := strings.Index(lowerValue, "filename="); index >= 0 {
		rawValue := strings.TrimSpace(contentDisposition[index+len("filename="):])
		if separator := strings.Index(rawValue, ";"); separator >= 0 {
			rawValue = rawValue[:separator]
		}
		return strings.Trim(rawValue, "\"")
	}
	return ""
}

type searchAssetsEnvelope struct {
	Assets searchAssetsItems `json:"assets"`
}

type searchAssetsItems struct {
	Items    []immichAsset `json:"items"`
	Total    int           `json:"total"`
	NextPage *int          `json:"nextPage,omitempty"`
}

func (s *searchAssetsItems) UnmarshalJSON(data []byte) error {
	type rawSearchAssetsItems struct {
		Items    []immichAsset    `json:"items"`
		Total    json.RawMessage  `json:"total"`
		NextPage *json.RawMessage `json:"nextPage,omitempty"`
	}

	var raw rawSearchAssetsItems
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	total, err := decodeFlexibleInt(raw.Total)
	if err != nil {
		return fmt.Errorf("decode total: %w", err)
	}

	var nextPage *int
	if raw.NextPage != nil {
		decodedNextPage, err := decodeFlexibleInt(*raw.NextPage)
		if err != nil {
			return fmt.Errorf("decode nextPage: %w", err)
		}
		nextPage = &decodedNextPage
	}

	s.Items = raw.Items
	s.Total = total
	s.NextPage = nextPage
	return nil
}

type immichAsset struct {
	ID               string       `json:"id"`
	Type             string       `json:"type"`
	OriginalFileName string       `json:"originalFileName"`
	FileCreatedAt    flexibleTime `json:"fileCreatedAt"`
	Duration         *string      `json:"duration,omitempty"`
}

type flexibleTime struct {
	time.Time
}

func (f *flexibleTime) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			f.Time = parsed.UTC()
			return nil
		}
	}
	return fmt.Errorf("parse flexible time %q", raw)
}

func decodeFlexibleInt(data json.RawMessage) (int, error) {
	var intValue int
	if err := json.Unmarshal(data, &intValue); err == nil {
		return intValue, nil
	}

	var stringValue string
	if err := json.Unmarshal(data, &stringValue); err == nil {
		parsedValue, err := strconv.Atoi(stringValue)
		if err != nil {
			return 0, err
		}
		return parsedValue, nil
	}

	return 0, fmt.Errorf("unsupported integer payload: %s", string(data))
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
