package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestImmichPassthroughSearchUsesImmichWithoutCatalogSync(t *testing.T) {
	t.Parallel()

	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmich,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	metadataRequests := 0
	statisticsRequests := 0
	service.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("x-api-key") != "test-key" {
			t.Fatalf("x-api-key = %q, want configured key", request.Header.Get("x-api-key"))
		}
		var body string
		switch request.URL.Path {
		case "/api/search/metadata":
			metadataRequests++
			body = `{"assets":{"total":1,"items":[{"id":"asset-123","type":"IMAGE","originalFileName":"photo.jpg","fileCreatedAt":"2026-04-07T09:57:15.053Z"}]}}`
		case "/api/search/statistics":
			statisticsRequests++
			body = `{"images":124,"videos":6}`
		default:
			t.Fatalf("unexpected request path %q", request.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}

	page, err := service.CatalogPage(0, 60)
	if err != nil {
		t.Fatalf("CatalogPage() error = %v", err)
	}
	if page.Total != 130 || len(page.Items) != 1 || page.Items[0].ID != "asset-123" || page.Items[0].SourceKey != "" {
		t.Fatalf("CatalogPage() = %#v, want direct Immich item and statistics total", page)
	}
	if metadataRequests != 1 || statisticsRequests != 1 {
		t.Fatalf("requests metadata=%d statistics=%d, want 1 each", metadataRequests, statisticsRequests)
	}
	if len(service.MirrorDatasourceSourceKeys()) != 0 {
		t.Fatalf("MirrorDatasourceSourceKeys() = %#v, want passthrough excluded", service.MirrorDatasourceSourceKeys())
	}
	if _, err := service.SyncDatasourceMirror(context.Background(), sourceKey, MirrorSyncModeFull); !errors.Is(err, ErrCatalogNotConfigured) {
		t.Fatalf("SyncDatasourceMirror() error = %v, want ErrCatalogNotConfigured", err)
	}

	capabilities := service.SearchCapabilities()
	if len(capabilities.QueryModes) != 3 || capabilities.Semantic != nil {
		t.Fatalf("SearchCapabilities() = %#v, want Immich query modes without Timich semantic status", capabilities)
	}
}

func TestImmichPassthroughSemanticSearchUsesSmartEndpoint(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmich,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})
	service.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/search/smart" {
			t.Fatalf("request path = %q, want smart search", request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body["query"] != "beach sunset" || body["type"] != "IMAGE" {
			t.Fatalf("request body = %#v", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"assets":{"total":0,"items":[]}}`)),
		}, nil
	})}

	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind:    CollectionKindSearch,
			Query:   &AssetSearchQuery{Text: "beach sunset", Mode: QueryModeSemantic},
			Filters: AssetSearchFilters{MediaTypes: []string{"image"}},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 60},
	})
	if err != nil {
		t.Fatalf("SearchAssets() error = %v", err)
	}
	if page.TotalAccuracy != TotalAccuracyEstimated || page.Resolved.QueryMode != QueryModeSemantic {
		t.Fatalf("SearchAssets() = %#v", page)
	}
}

func TestImmichPassthroughNextPageMakesConflictingTotalALowerBound(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmich,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})
	service.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/search/metadata" {
			return nil, fmt.Errorf("unexpected request path %q", request.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"assets":{"total":60,"nextPage":2,"items":[]}}`)),
		}, nil
	})}

	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind:  CollectionKindSearch,
			Query: &AssetSearchQuery{Text: "photo", Mode: QueryModeFilename},
		},
		Page: AssetSearchPageRequest{Index: 0, Size: 60},
	})
	if err != nil {
		t.Fatalf("SearchAssets() error = %v", err)
	}
	if page.Total != 61 || page.TotalAccuracy != TotalAccuracyLowerBound || page.NextPageIndex == nil || *page.NextPageIndex != 1 {
		t.Fatalf("SearchAssets() = %#v, want reachable next page with lower-bound total", page)
	}
}

func TestImmichPassthroughTimelineKeepsNextPageReachableWhenStatisticsFail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		statisticsResponse *http.Response
		statisticsError    error
	}{
		{
			name: "server error",
			statisticsResponse: &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
			},
		},
		{
			name: "invalid response",
			statisticsResponse: &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"images":`)),
			},
		},
		{
			name:            "transport error",
			statisticsError: errors.New("statistics unavailable"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			service := NewService([]config.DatasourceConfig{{
				SourceKey:   "1111111111111111",
				Name:        "Home Immich",
				Kind:        config.DatasourceKindImmich,
				URL:         "http://immich.test",
				AccessToken: "test-key",
			}})
			statisticsRequests := 0
			service.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch request.URL.Path {
				case "/api/search/metadata":
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"assets":{"total":60,"nextPage":2,"items":[]}}`)),
					}, nil
				case "/api/search/statistics":
					statisticsRequests++
					return test.statisticsResponse, test.statisticsError
				default:
					return nil, fmt.Errorf("unexpected request path %q", request.URL.Path)
				}
			})}

			page, err := service.CatalogPage(0, 60)
			if err != nil {
				t.Fatalf("CatalogPage() error = %v", err)
			}
			if page.Total != 61 || page.TotalAccuracy != TotalAccuracyLowerBound || page.NextPageIndex == nil || *page.NextPageIndex != 1 {
				t.Fatalf("CatalogPage() = %#v, want reachable next page after statistics failure", page)
			}
			page, err = service.CatalogPage(0, 60)
			if err != nil {
				t.Fatalf("second CatalogPage() error = %v", err)
			}
			if page.Total != 61 || page.TotalAccuracy != TotalAccuracyLowerBound {
				t.Fatalf("second CatalogPage() = %#v, want cached statistics failure fallback", page)
			}
			if statisticsRequests != 1 {
				t.Fatalf("statistics requests = %d, want one request during failure backoff", statisticsRequests)
			}
		})
	}
}

func TestImmichPassthroughStatisticsUsesShortTimeoutAndFailureBackoff(t *testing.T) {
	service := NewService([]config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmich,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})
	statisticsRequests := 0
	service.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/search/metadata":
			var body struct {
				Size int `json:"size"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
					`{"assets":{"total":%d,"nextPage":2,"items":[]}}`,
					body.Size,
				))),
			}, nil
		case "/api/search/statistics":
			statisticsRequests++
			<-request.Context().Done()
			return nil, request.Context().Err()
		default:
			return nil, fmt.Errorf("unexpected request path %q", request.URL.Path)
		}
	})}

	startedAt := time.Now()
	first, err := service.CatalogPage(0, 1)
	if err != nil {
		t.Fatalf("first CatalogPage() error = %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= 5*time.Second {
		t.Fatalf("first CatalogPage() elapsed = %s, want short statistics-specific timeout", elapsed)
	}
	if first.Total != 2 || first.TotalAccuracy != TotalAccuracyLowerBound {
		t.Fatalf("first CatalogPage() = %#v, want next-page lower bound after timeout", first)
	}

	startedAt = time.Now()
	second, err := service.CatalogPage(0, 60)
	if err != nil {
		t.Fatalf("second CatalogPage() error = %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		t.Fatalf("second CatalogPage() elapsed = %s, want cached failure without another timeout", elapsed)
	}
	if second.Total != 61 || second.TotalAccuracy != TotalAccuracyLowerBound {
		t.Fatalf("second CatalogPage() = %#v, want next-page lower bound during backoff", second)
	}
	if statisticsRequests != 1 {
		t.Fatalf("statistics requests = %d, want one timed-out request across both initial reads", statisticsRequests)
	}
}

func TestImmichPassthroughStatisticsPreservesConcurrentSuccessfulTotal(t *testing.T) {
	service := NewService([]config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmich,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})
	failureStarted := make(chan struct{})
	releaseFailure := make(chan struct{})
	failureReleased := false
	defer func() {
		if !failureReleased {
			close(releaseFailure)
		}
	}()
	var statisticsRequests atomic.Int32
	service.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/search/metadata":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"assets":{"total":60,"nextPage":2,"items":[]}}`)),
			}, nil
		case "/api/search/statistics":
			switch statisticsRequests.Add(1) {
			case 1:
				close(failureStarted)
				<-releaseFailure
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{}`)),
				}, nil
			case 2:
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"images":90,"videos":10}`)),
				}, nil
			default:
				return nil, errors.New("unexpected extra statistics request")
			}
		default:
			return nil, fmt.Errorf("unexpected request path %q", request.URL.Path)
		}
	})}

	type searchResult struct {
		page AssetSearchPage
		err  error
	}
	failureResult := make(chan searchResult, 1)
	go func() {
		page, err := service.CatalogPage(0, 60)
		failureResult <- searchResult{page: page, err: err}
	}()
	select {
	case <-failureStarted:
	case result := <-failureResult:
		t.Fatalf("first CatalogPage() completed before statistics request: page=%#v error=%v", result.page, result.err)
	case <-time.After(time.Second):
		t.Fatal("first statistics request did not start")
	}

	successPage, successErr := service.CatalogPage(0, 60)
	close(releaseFailure)
	failureReleased = true
	failed := <-failureResult
	if successErr != nil {
		t.Fatalf("successful CatalogPage() error = %v", successErr)
	}
	if successPage.Total != 100 || successPage.TotalAccuracy != TotalAccuracyExact {
		t.Fatalf("successful CatalogPage() = %#v, want exact concurrent statistics total", successPage)
	}
	if failed.err != nil {
		t.Fatalf("failed-statistics CatalogPage() error = %v", failed.err)
	}
	if failed.page.Total != 61 || failed.page.TotalAccuracy != TotalAccuracyLowerBound {
		t.Fatalf("failed-statistics CatalogPage() = %#v, want lower-bound fallback", failed.page)
	}

	cached, err := service.CatalogPage(0, 60)
	if err != nil {
		t.Fatalf("cached CatalogPage() error = %v", err)
	}
	if cached.Total != 100 || cached.TotalAccuracy != TotalAccuracyExact {
		t.Fatalf("cached CatalogPage() = %#v, want preserved successful total", cached)
	}
	if got := statisticsRequests.Load(); got != 2 {
		t.Fatalf("statistics requests = %d, want two concurrent cache misses and no downgrade retry", got)
	}
}

func TestImmichPassthroughCachesFilteredStatisticsAndConvertsExclusiveUpperBound(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmich,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})
	metadataRequests := 0
	statisticsRequests := 0
	to := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	wantTakenBefore := to.Add(-time.Nanosecond).Format(time.RFC3339Nano)
	service.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			return nil, fmt.Errorf("decode %s request: %w", request.URL.Path, err)
		}
		if body["takenBefore"] != wantTakenBefore {
			return nil, fmt.Errorf("takenBefore = %#v, want exclusive upper bound translated to %q", body["takenBefore"], wantTakenBefore)
		}
		responseBody := `{"assets":{"total":0,"items":[]}}`
		switch request.URL.Path {
		case "/api/search/metadata":
			metadataRequests++
		case "/api/search/statistics":
			statisticsRequests++
			responseBody = `{"images":5,"videos":1}`
		default:
			return nil, fmt.Errorf("unexpected request path %q", request.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(responseBody)),
		}, nil
	})}

	search := func(pageIndex int, mediaTypes ...string) AssetSearchPage {
		t.Helper()
		page, err := service.SearchAssets(AssetSearchRequest{
			Collection: AssetCollectionRequest{
				Kind: CollectionKindTimeline,
				Filters: AssetSearchFilters{
					MediaTypes: mediaTypes,
					CapturedAt: &AssetSearchCapturedTime{To: &to},
				},
			},
			Page: AssetSearchPageRequest{Index: pageIndex, Size: 60},
		})
		if err != nil {
			t.Fatalf("SearchAssets(page=%d) error = %v", pageIndex, err)
		}
		return page
	}

	if page := search(0); page.Total != 6 || page.TotalAccuracy != TotalAccuracyExact {
		t.Fatalf("first SearchAssets() = %#v, want exact statistics total", page)
	}
	if page := search(1); page.Total != 6 || page.TotalAccuracy != TotalAccuracyExact {
		t.Fatalf("second SearchAssets() = %#v, want cached exact statistics total", page)
	}
	_ = search(0, "image")
	if metadataRequests != 3 || statisticsRequests != 2 {
		t.Fatalf("requests metadata=%d statistics=%d, want filtered total cached by filter key", metadataRequests, statisticsRequests)
	}
}

func TestImmichPassthroughDoesNotCacheStatisticsFromOldDatasourceGeneration(t *testing.T) {
	t.Parallel()

	oldDatasource := config.DatasourceConfig{
		SourceKey:   "1111111111111111",
		Name:        "Old Immich",
		Kind:        config.DatasourceKindImmich,
		URL:         "http://old.immich",
		AccessToken: "old-key",
	}
	newDatasource := config.DatasourceConfig{
		SourceKey:   "2222222222222222",
		Name:        "New Immich",
		Kind:        config.DatasourceKindImmich,
		URL:         "http://new.immich",
		AccessToken: "new-key",
	}
	service := NewService([]config.DatasourceConfig{oldDatasource})
	oldStatisticsStarted := make(chan struct{})
	releaseOldStatistics := make(chan struct{})
	service.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		responseBody := `{"assets":{"total":0,"items":[]}}`
		switch {
		case request.URL.Host == "old.immich" && request.URL.Path == "/api/search/metadata":
		case request.URL.Host == "old.immich" && request.URL.Path == "/api/search/statistics":
			close(oldStatisticsStarted)
			<-releaseOldStatistics
			responseBody = `{"images":999,"videos":0}`
		case request.URL.Host == "new.immich" && request.URL.Path == "/api/search/metadata":
		case request.URL.Host == "new.immich" && request.URL.Path == "/api/search/statistics":
			responseBody = `{"images":5,"videos":0}`
		default:
			return nil, fmt.Errorf("unexpected request %s%s", request.URL.Host, request.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(responseBody)),
		}, nil
	})}

	oldSearchDone := make(chan error, 1)
	go func() {
		_, err := service.CatalogPage(0, 60)
		oldSearchDone <- err
	}()
	<-oldStatisticsStarted
	service.ReconfigureDatasources([]config.DatasourceConfig{newDatasource})
	close(releaseOldStatistics)
	if err := <-oldSearchDone; err != nil {
		t.Fatalf("old CatalogPage() error = %v", err)
	}

	service.mu.Lock()
	cachedAfterOldRequest := len(service.statisticsTotalCache)
	service.mu.Unlock()
	if cachedAfterOldRequest != 0 {
		t.Fatalf("statistics cache entries after old request = %d, want 0", cachedAfterOldRequest)
	}

	page, err := service.CatalogPage(0, 60)
	if err != nil {
		t.Fatalf("new CatalogPage() error = %v", err)
	}
	if page.Total != 5 || page.TotalAccuracy != TotalAccuracyExact {
		t.Fatalf("new CatalogPage() = %#v, want new datasource statistics", page)
	}
}
