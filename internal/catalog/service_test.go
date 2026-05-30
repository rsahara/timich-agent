package catalog

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestSearchAssetsItemsUnmarshalSupportsStringNextPage(t *testing.T) {
	t.Parallel()

	var items searchAssetsItems
	payload := []byte(`{
		"items": [
			{
				"id": "asset-123",
				"type": "IMAGE",
				"originalFileName": "photo.jpg",
				"fileCreatedAt": "2026-04-07T09:57:15.053Z"
			}
		],
		"total": 1,
		"nextPage": "2"
	}`)

	if err := json.Unmarshal(payload, &items); err != nil {
		t.Fatalf("unmarshal search assets items: %v", err)
	}

	if items.Total != 1 {
		t.Fatalf("expected total 1, got %d", items.Total)
	}
	if items.NextPage == nil || *items.NextPage != 2 {
		t.Fatalf("expected nextPage 2, got %#v", items.NextPage)
	}
	if len(items.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items.Items))
	}
}

func TestCatalogPageUsesStatisticsTotal(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})
	statisticsRequests := 0
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var body string
			switch r.URL.Path {
			case "/api/search/metadata":
				body = `{
					"assets": {
						"total": 1,
						"items": [
							{
								"id": "asset-123",
								"type": "IMAGE",
								"originalFileName": "photo.jpg",
								"fileCreatedAt": "2026-04-07T09:57:15.053Z"
							}
						],
						"nextPage": "2"
					}
				}`
			case "/api/search/statistics":
				statisticsRequests++
				body = `{
					"images": 124,
					"videos": 6
				}`
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{}`)),
				}, nil
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	page, err := service.CatalogPage(0, 60)
	if err != nil {
		t.Fatalf("catalog page: %v", err)
	}

	if page.Total != 130 {
		t.Fatalf("expected total 130, got %d", page.Total)
	}
	if page.NextPageIndex == nil || *page.NextPageIndex != 1 {
		t.Fatalf("expected nextPageIndex 1, got %#v", page.NextPageIndex)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(page.Items))
	}

	page, err = service.CatalogPage(1, 60)
	if err != nil {
		t.Fatalf("second catalog page: %v", err)
	}
	if page.Total != 130 {
		t.Fatalf("second page total = %d, want cached total 130", page.Total)
	}
	if statisticsRequests != 1 {
		t.Fatalf("statistics requests = %d, want cached total to avoid repeat request", statisticsRequests)
	}
}

func TestTimelineSearchWithFiltersUsesStatisticsTotal(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})
	metadataRequests := 0
	statisticsRequests := 0
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var body string
			switch r.URL.Path {
			case "/api/search/metadata":
				metadataRequests++
				var requestBody map[string]any
				if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
					t.Fatalf("decode metadata request body: %v", err)
				}
				if requestBody["type"] != "VIDEO" {
					t.Fatalf("metadata request type = %v, want VIDEO", requestBody["type"])
				}
				if requestBody["takenAfter"] != from.Format(time.RFC3339Nano) {
					t.Fatalf("metadata request takenAfter = %v, want %s", requestBody["takenAfter"], from.Format(time.RFC3339Nano))
				}
				if requestBody["takenBefore"] != to.Format(time.RFC3339Nano) {
					t.Fatalf("metadata request takenBefore = %v, want %s", requestBody["takenBefore"], to.Format(time.RFC3339Nano))
				}
				body = `{
					"assets": {
						"total": 1,
						"items": [
							{
								"id": "video-123",
								"type": "VIDEO",
								"originalFileName": "clip.mov",
								"fileCreatedAt": "2026-01-07T09:57:15.053Z",
								"duration": "0:00:10.000000"
							}
						]
					}
				}`
			case "/api/search/statistics":
				statisticsRequests++
				var requestBody map[string]any
				if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
					t.Fatalf("decode statistics request body: %v", err)
				}
				if requestBody["type"] != "VIDEO" {
					t.Fatalf("statistics request type = %v, want VIDEO", requestBody["type"])
				}
				if requestBody["takenAfter"] != from.Format(time.RFC3339Nano) {
					t.Fatalf("statistics request takenAfter = %v, want %s", requestBody["takenAfter"], from.Format(time.RFC3339Nano))
				}
				if requestBody["takenBefore"] != to.Format(time.RFC3339Nano) {
					t.Fatalf("statistics request takenBefore = %v, want %s", requestBody["takenBefore"], to.Format(time.RFC3339Nano))
				}
				body = `{
					"images": 0,
					"videos": 6
				}`
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{}`)),
				}, nil
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	}

	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{
			Kind: CollectionKindTimeline,
			Filters: AssetSearchFilters{
				MediaTypes: []string{"video"},
				CapturedAt: &AssetSearchCapturedTime{
					From: &from,
					To:   &to,
				},
			},
		},
		Page: AssetSearchPageRequest{
			Index: 0,
			Size:  60,
		},
	})
	if err != nil {
		t.Fatalf("filtered timeline search: %v", err)
	}

	if page.Total != 6 {
		t.Fatalf("filtered timeline total = %d, want statistics total 6", page.Total)
	}
	if page.TotalAccuracy != TotalAccuracyExact {
		t.Fatalf("filtered timeline total accuracy = %q, want %q", page.TotalAccuracy, TotalAccuracyExact)
	}
	if metadataRequests != 1 {
		t.Fatalf("metadata requests = %d, want 1", metadataRequests)
	}
	if statisticsRequests != 1 {
		t.Fatalf("statistics requests = %d, want 1", statisticsRequests)
	}
	if len(page.Items) != 1 || page.Items[0].Type != "video" {
		t.Fatalf("items = %#v, want one video item", page.Items)
	}
}

func TestStaticDemoCatalogAndMedia(t *testing.T) {
	t.Parallel()

	bundleDir := t.TempDir()
	assetDir := filepath.Join(bundleDir, "assets", "demo-0001")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	imageBody := encodeJPEGForTest(t, 320, 240)
	for _, name := range []string{"preview.jpg", "detail_preview.jpg", "original.jpg"} {
		if err := os.WriteFile(filepath.Join(assetDir, name), imageBody, 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	manifest := `{
		"version": 1,
		"assets": [
			{
				"id": "demo-0001",
				"type": "IMAGE",
				"originalFileName": "demo-0001.jpg",
				"fileCreatedAt": "2026-01-01T12:00:00Z",
				"previewPath": "assets/demo-0001/preview.jpg",
				"detailPreviewPath": "assets/demo-0001/detail_preview.jpg",
				"originalPath": "assets/demo-0001/original.jpg"
			}
		]
	}`
	manifestPath := filepath.Join(bundleDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("WriteFile(manifest) error = %v", err)
	}
	service := NewService([]config.DatasourceConfig{{
		Name: "Review Demo",
		Kind: config.DatasourceKindStaticDemo,
		URL:  bundleDir,
	}})
	if !service.Ready() {
		t.Fatal("Ready() = false, want directory-backed static demo ready")
	}

	page, err := service.CatalogPage(0, 60)
	if err != nil {
		t.Fatalf("CatalogPage() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("page = %#v, want one static asset", page)
	}
	if !page.Items[0].CapturedAt.Equal(time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("CapturedAt = %v", page.Items[0].CapturedAt)
	}

	response, err := service.Preview(nil, "demo-0001")
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", response.Header.Get("Content-Type"))
	}
}

func TestStaticDemoBrokenManifestIsNotReady(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name: "Review Demo",
		Kind: config.DatasourceKindStaticDemo,
		URL:  filepath.Join(t.TempDir(), "missing-bundle"),
	}})
	if service.Ready() {
		t.Fatal("Ready() = true, want broken static demo datasource to be not ready")
	}
	if _, err := service.CatalogPage(0, 60); !errors.Is(err, ErrNoDatasourceConfigured) {
		t.Fatalf("CatalogPage() error = %v, want ErrNoDatasourceConfigured", err)
	}
}

func TestStaticDemoOriginalSupportsByteRange(t *testing.T) {
	t.Parallel()

	bundleDir := t.TempDir()
	assetDir := filepath.Join(bundleDir, "assets", "demo-0001")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	for name, body := range map[string][]byte{
		"preview.jpg":        encodeJPEGForTest(t, 32, 32),
		"detail_preview.jpg": encodeJPEGForTest(t, 64, 64),
		"original.mp4":       []byte("0123456789"),
	} {
		if err := os.WriteFile(filepath.Join(assetDir, name), body, 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	manifest := `{
		"version": 1,
		"assets": [
			{
				"id": "demo-0001",
				"type": "VIDEO",
				"originalFileName": "demo-0001.mp4",
				"fileCreatedAt": "2026-01-01T12:00:00Z",
				"duration": "0:00:10.000000",
				"previewPath": "assets/demo-0001/preview.jpg",
				"detailPreviewPath": "assets/demo-0001/detail_preview.jpg",
				"originalPath": "assets/demo-0001/original.mp4"
			}
		]
	}`
	manifestPath := filepath.Join(bundleDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("WriteFile(manifest) error = %v", err)
	}
	service := NewService([]config.DatasourceConfig{{
		Name: "Review Demo",
		Kind: config.DatasourceKindStaticDemo,
		URL:  manifestPath,
	}})
	request, err := http.NewRequest(http.MethodGet, "http://agent.test/v1/assets/demo-0001/original", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Range", "bytes=2-5")

	response, err := service.Original(request, "demo-0001")
	if err != nil {
		t.Fatalf("Original() error = %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if response.StatusCode != http.StatusPartialContent {
		t.Fatalf("StatusCode = %d, want 206", response.StatusCode)
	}
	if string(body) != "2345" {
		t.Fatalf("body = %q, want 2345", string(body))
	}
	if response.Header.Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q", response.Header.Get("Content-Range"))
	}
}

func TestOriginalForwardsRangeHeaders(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	var receivedMethod string
	var receivedRange string
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			receivedMethod = r.Method
			receivedRange = r.Header.Get("Range")
			return &http.Response{
				StatusCode: http.StatusPartialContent,
				Header: http.Header{
					"Content-Type":   []string{"video/mp4"},
					"Accept-Ranges":  []string{"bytes"},
					"Content-Range":  []string{"bytes 0-1/100"},
					"Content-Length": []string{"2"},
				},
				Body: io.NopCloser(strings.NewReader("ok")),
			}, nil
		}),
	}

	request, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/original", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Range", "bytes=0-1")

	response, err := service.Original(request, "asset-123")
	if err != nil {
		t.Fatalf("original proxy: %v", err)
	}
	defer response.Body.Close()

	if receivedMethod != http.MethodGet {
		t.Fatalf("expected method GET, got %s", receivedMethod)
	}
	if receivedRange != "bytes=0-1" {
		t.Fatalf("expected Range header to be forwarded, got %q", receivedRange)
	}
	if response.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected status 206, got %d", response.StatusCode)
	}
	if response.Header.Get("Content-Range") != "bytes 0-1/100" {
		t.Fatalf("expected Content-Range header to survive, got %q", response.Header.Get("Content-Range"))
	}
}

func TestPreviewUsesDeterministicThumbnailProfile(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	var requestedPaths []string
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requestedPaths = append(requestedPaths, r.URL.RequestURI())
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/jpeg"},
				},
				Body: io.NopCloser(strings.NewReader("ok")),
			}, nil
		}),
	}

	localRequest, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/preview", nil)
	if err != nil {
		t.Fatalf("new local request: %v", err)
	}
	if _, err := service.Preview(localRequest, "asset-123"); err != nil {
		t.Fatalf("local preview proxy: %v", err)
	}

	hostedRequest, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/preview", nil)
	if err != nil {
		t.Fatalf("new hosted request: %v", err)
	}
	hostedRequest.Header.Set("X-Timich-Hosted-Base-URL", "https://timich.runo.jp")
	if _, err := service.Preview(hostedRequest, "asset-123"); err != nil {
		t.Fatalf("hosted asset preview proxy: %v", err)
	}

	if len(requestedPaths) != 2 {
		t.Fatalf("expected 2 preview requests, got %d", len(requestedPaths))
	}
	if requestedPaths[0] != "/api/assets/asset-123/thumbnail?size=thumbnail" {
		t.Fatalf("expected local preview path, got %q", requestedPaths[0])
	}
	if requestedPaths[1] != "/api/assets/asset-123/thumbnail?size=thumbnail" {
		t.Fatalf("expected hosted asset preview path, got %q", requestedPaths[1])
	}
}

func TestPreviewReturnsSmallPreviewUnchanged(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeJPEGForTest(t, 320, 240)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.RequestURI() != "/api/assets/asset-123/thumbnail?size=thumbnail" {
				t.Fatalf("unexpected request path %q", r.URL.RequestURI())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/jpeg"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.Preview(newHostedAssetPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("hosted asset preview proxy: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read hosted asset preview body: %v", err)
	}
	if !bytes.Equal(body, originalBody) {
		t.Fatalf("expected small hosted asset preview passthrough")
	}
}

func TestPreviewResizesLargePreviewTo512(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeJPEGForTest(t, 1200, 800)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/jpeg"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.Preview(newHostedAssetPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("hosted asset preview proxy: %v", err)
	}
	defer response.Body.Close()

	encodedBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read hosted asset preview body: %v", err)
	}
	if len(encodedBody) > previewMaxBytes {
		t.Fatalf("hosted asset preview bytes = %d, want <= %d", len(encodedBody), previewMaxBytes)
	}
	previewImage, _, err := image.Decode(bytes.NewReader(encodedBody))
	if err != nil {
		t.Fatalf("decode hosted asset preview response: %v", err)
	}
	if previewImage.Bounds().Dx() != 512 || previewImage.Bounds().Dy() != 341 {
		t.Fatalf("expected 512x341 hosted asset preview, got %dx%d", previewImage.Bounds().Dx(), previewImage.Bounds().Dy())
	}
}

func TestDetailPreviewLocalUsesPreviewProfile(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeJPEGForTest(t, 1440, 960)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.RequestURI() != "/api/assets/asset-123/thumbnail?size=preview" {
				t.Fatalf("unexpected request path %q", r.URL.RequestURI())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type":        []string{"image/jpeg"},
					"Content-Disposition": []string{"inline; filename*=UTF-8''asset-123.jpg"},
					"Cache-Control":       []string{"private, max-age=86400"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	request, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/detail_preview", nil)
	if err != nil {
		t.Fatalf("new detail preview request: %v", err)
	}
	response, err := service.DetailPreview(request, "asset-123")
	if err != nil {
		t.Fatalf("detail preview proxy: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read detail preview response body: %v", err)
	}
	if !bytes.Equal(body, originalBody) {
		t.Fatalf("expected small detail preview passthrough")
	}
	if response.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("expected original content-type, got %q", response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Content-Disposition") != "inline; filename*=UTF-8''asset-123.jpg" {
		t.Fatalf("expected original content-disposition, got %q", response.Header.Get("Content-Disposition"))
	}
}

func TestDetailPreviewResizesLargeImagesToJPEG(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeJPEGForTest(t, 4000, 3000)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.RequestURI() != "/api/assets/asset-123/thumbnail?size=preview" {
				t.Fatalf("unexpected request path %q", r.URL.RequestURI())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type":        []string{"image/jpeg"},
					"Content-Disposition": []string{"inline; filename*=UTF-8''asset-123.jpg"},
					"Cache-Control":       []string{"private, max-age=86400"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.DetailPreview(newHostedDetailPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("detail preview proxy: %v", err)
	}
	defer response.Body.Close()

	encodedBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read detail preview response body: %v", err)
	}
	if response.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("expected jpeg content-type, got %q", response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Content-Disposition") != "inline; filename*=UTF-8''asset-123_detail_preview.jpg" {
		t.Fatalf("unexpected content-disposition %q", response.Header.Get("Content-Disposition"))
	}
	if serverTiming := response.Header.Get("Server-Timing"); !strings.Contains(serverTiming, "read_original;dur=") ||
		!strings.Contains(serverTiming, "transform;dur=") ||
		!strings.Contains(serverTiming, "total;dur=") {
		t.Fatalf("expected detail preview server timing stages, got %q", serverTiming)
	}
	detailImage, _, err := image.Decode(bytes.NewReader(encodedBody))
	if err != nil {
		t.Fatalf("decode detail preview response: %v", err)
	}
	if detailImage.Bounds().Dx() != 2560 || detailImage.Bounds().Dy() != 1920 {
		t.Fatalf("expected 2560x1920 detail preview, got %dx%d", detailImage.Bounds().Dx(), detailImage.Bounds().Dy())
	}
}

func TestDetailPreviewReturnsSmallPreviewUnchanged(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeJPEGForTest(t, 1440, 960)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.RequestURI() != "/api/assets/asset-123/thumbnail?size=preview" {
				t.Fatalf("unexpected request path %q", r.URL.RequestURI())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/jpeg"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.DetailPreview(newHostedDetailPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("detail preview proxy: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read detail preview response body: %v", err)
	}
	if !bytes.Equal(body, originalBody) {
		t.Fatalf("expected small detail preview passthrough")
	}
}

func TestDetailPreviewAppliesJPEGOrientationBeforeResize(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeJPEGWithOrientationForTest(t, 3000, 4000, 6)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/jpeg"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.DetailPreview(newHostedDetailPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("detail preview proxy: %v", err)
	}
	defer response.Body.Close()

	encodedBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read detail preview response body: %v", err)
	}

	detailImage, _, err := image.Decode(bytes.NewReader(encodedBody))
	if err != nil {
		t.Fatalf("decode oriented detail preview response: %v", err)
	}
	if detailImage.Bounds().Dx() != 2560 || detailImage.Bounds().Dy() != 1920 {
		t.Fatalf("expected oriented 2560x1920 detail preview, got %dx%d", detailImage.Bounds().Dx(), detailImage.Bounds().Dy())
	}
}

func TestDetailPreviewReturnsSmallOrientedPreviewUnchanged(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := encodeQuadrantJPEGWithOrientationForTest(t, 600, 400, 6)
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/jpeg"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.DetailPreview(newHostedDetailPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("detail preview proxy: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read detail preview response body: %v", err)
	}
	if !bytes.Equal(body, originalBody) {
		t.Fatalf("expected small oriented detail preview passthrough")
	}
}

func TestDetailPreviewFallsBackToOriginalForUnsupportedFormats(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})

	originalBody := []byte("not-a-decodable-image")
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"image/heic"},
				},
				Body: io.NopCloser(bytes.NewReader(originalBody)),
			}, nil
		}),
	}

	response, err := service.DetailPreview(newHostedDetailPreviewRequest(t), "asset-123")
	if err != nil {
		t.Fatalf("detail preview proxy: %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read fallback body: %v", err)
	}
	if string(body) != string(originalBody) {
		t.Fatalf("expected original body fallback, got %q", string(body))
	}
	if response.Header.Get("Content-Type") != "image/heic" {
		t.Fatalf("expected original content-type, got %q", response.Header.Get("Content-Type"))
	}
}

func TestDetailPreviewRejectsOversizedOriginal(t *testing.T) {
	t.Parallel()

	service := NewService([]config.DatasourceConfig{{
		Name:        "test",
		Kind:        "immich",
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}})
	service.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type":   []string{"image/jpeg"},
					"Content-Length": []string{fmt.Sprintf("%d", detailPreviewMaxSource+1)},
				},
				Body: io.NopCloser(strings.NewReader("too-large")),
			}, nil
		}),
	}

	response, err := service.DetailPreview(newHostedDetailPreviewRequest(t), "asset-123")
	if err == nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatalf("expected oversized detail preview error")
	}
	if !errors.Is(err, ErrMediaTooLarge) {
		t.Fatalf("error = %v, want %v", err, ErrMediaTooLarge)
	}
}

func TestProfileHeadRequestsUseGetUpstreamAndReportRenderedLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		route        string
		upstreamPath string
		load         func(*Service, *http.Request, string) (*UpstreamMediaResponse, error)
	}{
		{
			name:         "preview",
			route:        "preview",
			upstreamPath: "/api/assets/asset-123/thumbnail?size=thumbnail",
			load:         (*Service).Preview,
		},
		{
			name:         "detail preview",
			route:        "detail_preview",
			upstreamPath: "/api/assets/asset-123/thumbnail?size=preview",
			load:         (*Service).DetailPreview,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			service := NewService([]config.DatasourceConfig{{
				Name:        "test",
				Kind:        "immich",
				URL:         "http://immich.test",
				AccessToken: "test-key",
			}})

			originalBody := encodeJPEGForTest(t, 3200, 1800)
			var upstreamMethod string
			var upstreamPath string
			service.client = &http.Client{
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					upstreamMethod = r.Method
					upstreamPath = r.URL.RequestURI()
					return &http.Response{
						StatusCode: http.StatusOK,
						Header: http.Header{
							"Content-Type": []string{"image/jpeg"},
						},
						Body: io.NopCloser(bytes.NewReader(originalBody)),
					}, nil
				}),
			}

			request, err := http.NewRequest(http.MethodHead, "http://timich-agent.test/v1/assets/asset-123/"+tt.route, nil)
			if err != nil {
				t.Fatalf("new head request: %v", err)
			}
			response, err := tt.load(service, request, "asset-123")
			if err != nil {
				t.Fatalf("profile head request: %v", err)
			}
			defer response.Body.Close()

			if upstreamMethod != http.MethodGet {
				t.Fatalf("upstream method = %q, want GET", upstreamMethod)
			}
			if upstreamPath != tt.upstreamPath {
				t.Fatalf("upstream path = %q, want %q", upstreamPath, tt.upstreamPath)
			}

			encodedBody, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read profile body: %v", err)
			}
			if len(encodedBody) == 0 {
				t.Fatalf("expected rendered body for header calculation")
			}
			if response.Header.Get("Content-Length") != fmt.Sprint(len(encodedBody)) {
				t.Fatalf("content-length = %q, want %d", response.Header.Get("Content-Length"), len(encodedBody))
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newHostedDetailPreviewRequest(t *testing.T) *http.Request {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/detail_preview", nil)
	if err != nil {
		t.Fatalf("new hosted detail preview request: %v", err)
	}
	request.Header.Set("X-Timich-Hosted-Base-URL", "https://timich.runo.jp")
	return request
}

func newHostedAssetPreviewRequest(t *testing.T) *http.Request {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, "http://timich-agent.test/v1/assets/asset-123/preview", nil)
	if err != nil {
		t.Fatalf("new hosted asset preview request: %v", err)
	}
	request.Header.Set("X-Timich-Hosted-Base-URL", "https://timich.runo.jp")
	return request
}

func encodeJPEGForTest(t *testing.T, width int, height int) []byte {
	t.Helper()

	imageBuffer := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			imageBuffer.Set(x, y, color.RGBA{
				R: uint8(x % 255),
				G: uint8(y % 255),
				B: uint8((x + y) % 255),
				A: 255,
			})
		}
	}

	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, imageBuffer, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpeg for test: %v", err)
	}
	return encoded.Bytes()
}

func encodeJPEGWithOrientationForTest(t *testing.T, width int, height int, orientation uint16) []byte {
	t.Helper()

	body := encodeJPEGForTest(t, width, height)
	return injectJPEGOrientationForTest(t, body, orientation)
}

func encodeQuadrantJPEGWithOrientationForTest(t *testing.T, width int, height int, orientation uint16) []byte {
	t.Helper()

	imageBuffer := image.NewRGBA(image.Rect(0, 0, width, height))
	midX := width / 2
	midY := height / 2
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			var pixel color.RGBA
			switch {
			case x < midX && y < midY:
				pixel = color.RGBA{R: 255, A: 255}
			case x >= midX && y < midY:
				pixel = color.RGBA{G: 255, A: 255}
			case x < midX && y >= midY:
				pixel = color.RGBA{B: 255, A: 255}
			default:
				pixel = color.RGBA{R: 255, G: 255, A: 255}
			}
			imageBuffer.Set(x, y, pixel)
		}
	}

	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, imageBuffer, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode quadrant jpeg for test: %v", err)
	}
	return injectJPEGOrientationForTest(t, encoded.Bytes(), orientation)
}

func injectJPEGOrientationForTest(t *testing.T, body []byte, orientation uint16) []byte {
	t.Helper()

	if len(body) < 2 || body[0] != 0xFF || body[1] != 0xD8 {
		t.Fatal("expected jpeg body with SOI marker")
	}

	var exif bytes.Buffer
	exif.WriteString("Exif")
	exif.Write([]byte{0x00, 0x00})
	exif.Write([]byte{'I', 'I'})
	_ = binary.Write(&exif, binary.LittleEndian, uint16(42))
	_ = binary.Write(&exif, binary.LittleEndian, uint32(8))
	_ = binary.Write(&exif, binary.LittleEndian, uint16(1))
	_ = binary.Write(&exif, binary.LittleEndian, uint16(0x0112))
	_ = binary.Write(&exif, binary.LittleEndian, uint16(3))
	_ = binary.Write(&exif, binary.LittleEndian, uint32(1))
	_ = binary.Write(&exif, binary.LittleEndian, orientation)
	_ = binary.Write(&exif, binary.LittleEndian, uint16(0))
	_ = binary.Write(&exif, binary.LittleEndian, uint32(0))

	segmentBody := exif.Bytes()
	segmentLength := uint16(len(segmentBody) + 2)

	var encoded bytes.Buffer
	encoded.Write(body[:2])
	encoded.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&encoded, binary.BigEndian, segmentLength)
	encoded.Write(segmentBody)
	encoded.Write(body[2:])
	return encoded.Bytes()
}

func assertApproxColorAt(t *testing.T, img image.Image, x int, y int, want color.RGBA) {
	t.Helper()

	got := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
	const tolerance = 48
	if absInt(int(got.R)-int(want.R)) > tolerance ||
		absInt(int(got.G)-int(want.G)) > tolerance ||
		absInt(int(got.B)-int(want.B)) > tolerance {
		t.Fatalf(
			"unexpected color at (%d,%d): got rgba(%d,%d,%d,%d), want approx rgba(%d,%d,%d,%d)",
			x,
			y,
			got.R,
			got.G,
			got.B,
			got.A,
			want.R,
			want.G,
			want.B,
			want.A,
		)
	}
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
