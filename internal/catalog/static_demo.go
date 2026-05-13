package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type staticDemoManifest struct {
	Version int               `json:"version"`
	Assets  []staticDemoAsset `json:"assets"`
}

type staticDemoAsset struct {
	ID                string    `json:"id"`
	Type              string    `json:"type"`
	OriginalFileName  string    `json:"originalFileName"`
	FileCreatedAt     time.Time `json:"fileCreatedAt"`
	Duration          *string   `json:"duration,omitempty"`
	PreviewPath       string    `json:"previewPath"`
	DetailPreviewPath string    `json:"detailPreviewPath"`
	OriginalPath      string    `json:"originalPath"`
}

type staticDemoSource struct {
	root   string
	assets []staticDemoAsset
	byID   map[string]staticDemoAsset
}

func newStaticDemoSource(rawURL string) (*staticDemoSource, error) {
	manifestPath, err := staticDemoManifestPath(rawURL)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read static demo manifest: %w", err)
	}

	var manifest staticDemoManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("decode static demo manifest: %w", err)
	}
	if manifest.Version != 1 {
		return nil, fmt.Errorf("unsupported static demo manifest version %d", manifest.Version)
	}

	source := &staticDemoSource{
		root:   filepath.Dir(manifestPath),
		assets: make([]staticDemoAsset, 0, len(manifest.Assets)),
		byID:   make(map[string]staticDemoAsset, len(manifest.Assets)),
	}
	for index, asset := range manifest.Assets {
		normalized, err := normalizeStaticDemoAsset(source.root, index, asset)
		if err != nil {
			return nil, err
		}
		if _, exists := source.byID[normalized.ID]; exists {
			return nil, fmt.Errorf("static demo asset %q: duplicate id", normalized.ID)
		}
		source.assets = append(source.assets, normalized)
		source.byID[normalized.ID] = normalized
	}
	return source, nil
}

func staticDemoManifestPath(rawURL string) (string, error) {
	value := strings.TrimSpace(rawURL)
	if value == "" {
		return "", fmt.Errorf("static demo manifest path is empty")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse static demo manifest URL: %w", err)
	}
	if parsed.Scheme == "" {
		return staticDemoManifestPathFromLocalPath(value)
	}
	if parsed.Scheme != "file" || parsed.Path == "" {
		return "", fmt.Errorf("static demo manifest URL must be a file URL")
	}
	if parsed.Host != "" && parsed.Host != "localhost" {
		return "", fmt.Errorf("static demo manifest file URL host must be empty or localhost")
	}
	return staticDemoManifestPathFromLocalPath(parsed.Path)
}

func staticDemoManifestPathFromLocalPath(path string) (string, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolutePath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return filepath.Join(absolutePath, "manifest.json"), nil
	}
	return absolutePath, nil
}

func normalizeStaticDemoAsset(root string, index int, asset staticDemoAsset) (staticDemoAsset, error) {
	asset.ID = strings.TrimSpace(asset.ID)
	asset.Type = strings.TrimSpace(asset.Type)
	asset.OriginalFileName = strings.TrimSpace(asset.OriginalFileName)
	if asset.ID == "" {
		return staticDemoAsset{}, fmt.Errorf("static demo asset %d: id is required", index)
	}
	if asset.Type != "IMAGE" && asset.Type != "VIDEO" {
		return staticDemoAsset{}, fmt.Errorf("static demo asset %q: unsupported type %q", asset.ID, asset.Type)
	}
	if asset.OriginalFileName == "" {
		asset.OriginalFileName = asset.ID + ".jpg"
	}
	if asset.FileCreatedAt.IsZero() {
		return staticDemoAsset{}, fmt.Errorf("static demo asset %q: fileCreatedAt is required", asset.ID)
	}
	for name, relPath := range map[string]string{
		"previewPath":       asset.PreviewPath,
		"detailPreviewPath": asset.DetailPreviewPath,
		"originalPath":      asset.OriginalPath,
	} {
		if _, err := staticDemoFilePath(root, relPath); err != nil {
			return staticDemoAsset{}, fmt.Errorf("static demo asset %q: %s: %w", asset.ID, name, err)
		}
	}
	return asset, nil
}

func staticDemoFilePath(root string, relPath string) (string, error) {
	relPath = strings.TrimSpace(relPath)
	if relPath == "" {
		return "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	cleaned := filepath.Clean(relPath)
	if cleaned == "." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
		return "", fmt.Errorf("path escapes bundle root")
	}
	fullPath := filepath.Join(root, cleaned)
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	fullAbs, err := filepath.Abs(fullPath)
	if err != nil {
		return "", err
	}
	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes bundle root")
	}
	return fullAbs, nil
}

func (s *staticDemoSource) SearchAssets(normalized normalizedAssetSearch) (AssetSearchPage, error) {
	if normalized.Resolved.QueryMode != QueryModeNone {
		return AssetSearchPage{}, ErrUnsupportedSearch
	}
	filtered := s.filteredAssets(normalized.Request.Collection.Filters)
	pageIndex := normalized.Request.Page.Index
	pageSize := normalized.Request.Page.Size
	start := pageIndex * pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}

	items := make([]Asset, 0, end-start)
	for _, asset := range filtered[start:end] {
		items = append(items, Asset{
			ID:         asset.ID,
			Type:       normalizeAssetType(asset.Type),
			Filename:   asset.OriginalFileName,
			CapturedAt: asset.FileCreatedAt.UTC(),
			Duration:   asset.Duration,
		})
	}

	var nextPageIndex *int
	if end < len(filtered) {
		next := pageIndex + 1
		nextPageIndex = &next
	}
	total := len(filtered)
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

func (s *staticDemoSource) filteredAssets(filters AssetSearchFilters) []staticDemoAsset {
	result := make([]staticDemoAsset, 0, len(s.assets))
	mediaTypes := map[string]struct{}{}
	for _, mediaType := range filters.MediaTypes {
		mediaTypes[mediaType] = struct{}{}
	}
	for _, asset := range s.assets {
		if len(mediaTypes) > 0 {
			if _, ok := mediaTypes[normalizeAssetType(asset.Type)]; !ok {
				continue
			}
		}
		if filters.CapturedAt != nil {
			capturedAt := asset.FileCreatedAt.UTC()
			if filters.CapturedAt.From != nil && capturedAt.Before(filters.CapturedAt.From.UTC()) {
				continue
			}
			if filters.CapturedAt.To != nil && !capturedAt.Before(filters.CapturedAt.To.UTC()) {
				continue
			}
		}
		result = append(result, asset)
	}
	return result
}

func (s *staticDemoSource) MediaResponse(clientRequest *http.Request, assetID string, variant string) (*UpstreamMediaResponse, error) {
	asset, ok := s.byID[strings.TrimSpace(assetID)]
	if !ok {
		return nil, ErrAssetNotFound
	}

	relPath := ""
	switch variant {
	case "preview":
		relPath = asset.PreviewPath
	case "detail_preview":
		relPath = asset.DetailPreviewPath
	case "original":
		relPath = asset.OriginalPath
	default:
		return nil, ErrAssetNotFound
	}

	path, err := staticDemoFilePath(s.root, relPath)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrAssetNotFound
		}
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if info.IsDir() {
		file.Close()
		return nil, ErrAssetNotFound
	}

	statusCode := http.StatusOK
	offset := int64(0)
	length := info.Size()
	header := make(http.Header)
	header.Set("Accept-Ranges", "bytes")
	header.Set("Cache-Control", "public, max-age=3600")
	header.Set("Content-Type", staticDemoContentType(path))
	header.Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	header.Set("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape(staticDemoFileName(asset, variant, path)))

	if clientRequest != nil {
		if rangeHeader := strings.TrimSpace(clientRequest.Header.Get("Range")); rangeHeader != "" {
			rangeStart, rangeEnd, ok := parseStaticDemoRange(rangeHeader, info.Size())
			if !ok {
				file.Close()
				header.Set("Content-Range", fmt.Sprintf("bytes */%d", info.Size()))
				header.Set("Content-Length", "0")
				return &UpstreamMediaResponse{
					StatusCode: http.StatusRequestedRangeNotSatisfiable,
					Header:     header,
					Body:       io.NopCloser(bytes.NewReader(nil)),
				}, nil
			}
			offset = rangeStart
			length = rangeEnd - rangeStart + 1
			statusCode = http.StatusPartialContent
			header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rangeStart, rangeEnd, info.Size()))
		}
	}

	header.Set("Content-Length", strconv.FormatInt(length, 10))
	return &UpstreamMediaResponse{
		StatusCode: statusCode,
		Header:     header,
		Body:       &staticDemoSectionReadCloser{SectionReader: io.NewSectionReader(file, offset, length), closer: file},
	}, nil
}

func parseStaticDemoRange(header string, size int64) (int64, int64, bool) {
	if size <= 0 || !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return 0, 0, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	startRaw, endRaw, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false
	}
	if startRaw == "" {
		suffix, err := strconv.ParseInt(strings.TrimSpace(endRaw), 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, true
	}
	start, err := strconv.ParseInt(strings.TrimSpace(startRaw), 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if strings.TrimSpace(endRaw) != "" {
		parsedEnd, err := strconv.ParseInt(strings.TrimSpace(endRaw), 10, 64)
		if err != nil || parsedEnd < start {
			return 0, 0, false
		}
		end = min(parsedEnd, size-1)
	}
	return start, end, true
}

func staticDemoContentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	default:
		if contentType := mime.TypeByExtension(filepath.Ext(path)); contentType != "" {
			return contentType
		}
		return "application/octet-stream"
	}
}

func staticDemoFileName(asset staticDemoAsset, variant string, path string) string {
	if variant == "original" {
		return asset.OriginalFileName
	}
	base := strings.TrimSuffix(filepath.Base(asset.OriginalFileName), filepath.Ext(asset.OriginalFileName))
	if base == "" || base == "." {
		base = asset.ID
	}
	return base + "_" + variant + ".jpg"
}

type staticDemoSectionReadCloser struct {
	*io.SectionReader
	closer io.Closer
}

func (r *staticDemoSectionReadCloser) Close() error {
	return r.closer.Close()
}
