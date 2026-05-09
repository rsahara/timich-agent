package catalog

import (
	"bytes"
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
)

var (
	previewJPEGQualities       = []int{58, 50, 42}
	detailPreviewJPEGQualities = []int{detailPreviewJPEGQuality, 70, 58, 50, 42}
)

// Asset matches the app-facing asset model returned to iOS clients.
type Asset struct {
	ID               string    `json:"id"`
	Type             string    `json:"type"`
	OriginalFileName string    `json:"originalFileName"`
	FileCreatedAt    time.Time `json:"fileCreatedAt"`
	Duration         *string   `json:"duration,omitempty"`
}

// AssetPage summarizes one paginated catalog response.
type AssetPage struct {
	PageIndex     int     `json:"pageIndex"`
	Items         []Asset `json:"items"`
	Total         int     `json:"total"`
	NextPageIndex *int    `json:"nextPageIndex,omitempty"`
}

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

// CatalogPage returns one paginated asset page proxied from the configured datasource.
func (s *Service) CatalogPage(pageIndex int, pageSize int) (AssetPage, error) {
	if !s.Ready() {
		return AssetPage{}, ErrNoDatasourceConfigured
	}
	if s.datasource.Kind == config.DatasourceKindStaticDemo {
		if s.staticDemoErr != nil {
			return AssetPage{}, s.staticDemoErr
		}
		return s.staticDemo.CatalogPage(pageIndex, pageSize)
	}

	apiPage := pageIndex + 1
	body, err := json.Marshal(map[string]any{
		"page":  apiPage,
		"size":  pageSize,
		"order": "desc",
	})
	if err != nil {
		return AssetPage{}, fmt.Errorf("marshal metadata request: %w", err)
	}

	request, err := s.newRequest(
		http.MethodPost,
		"/api/search/metadata",
		bytes.NewReader(body),
	)
	if err != nil {
		return AssetPage{}, err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := s.client.Do(request)
	if err != nil {
		return AssetPage{}, fmt.Errorf("perform metadata request: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return AssetPage{}, fmt.Errorf("metadata request returned status %d", response.StatusCode)
	}

	var envelope searchAssetsEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return AssetPage{}, fmt.Errorf("decode metadata response: %w", err)
	}

	items := make([]Asset, 0, len(envelope.Assets.Items))
	for _, asset := range envelope.Assets.Items {
		items = append(items, Asset{
			ID:               asset.ID,
			Type:             asset.Type,
			OriginalFileName: asset.OriginalFileName,
			FileCreatedAt:    asset.FileCreatedAt.Time.UTC(),
			Duration:         asset.Duration,
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
	if statisticsTotal, err := s.timelineAssetTotal(); err == nil {
		total = max(total, statisticsTotal)
	}
	if total == 0 && len(items) > 0 {
		total = pageIndex*pageSize + len(items)
	}

	return AssetPage{
		PageIndex:     pageIndex,
		Items:         items,
		Total:         total,
		NextPageIndex: nextPageIndex,
	}, nil
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
	request, err := s.newRequest(
		http.MethodPost,
		"/api/search/statistics",
		strings.NewReader(`{}`),
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
