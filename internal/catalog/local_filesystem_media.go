package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
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

	"github.com/rsahara/timich-agent/internal/config"
)

type localActiveLocation struct {
	ID            int64
	SourceKey     string
	RootKey       string
	RelativePath  string
	SizeBytes     int64
	MTime         string
	FastSignature string
	FileIdentity  string
}

func (s *Service) localOriginalMediaResponse(clientRequest *http.Request, datasource *config.DatasourceConfig, assetID string) (*UpstreamMediaResponse, error) {
	if datasource == nil {
		return nil, ErrNoDatasourceConfigured
	}
	ctx := contextFromRequest(clientRequest)
	trustedRoot, err := s.acquireTrustedLocalMediaRoot(ctx, datasource.SourceKey)
	if err != nil {
		if errors.Is(err, ErrLocalMediaRootNotTrusted) {
			return nil, ErrAssetNotFound
		}
		return nil, err
	}
	defer trustedRoot.Close()
	location, err := s.localActiveLocation(ctx, datasource.SourceKey, trustedRoot.root.Key, assetID)
	if err != nil {
		return nil, err
	}
	if location.RootKey != trustedRoot.root.Key {
		return nil, ErrAssetNotFound
	}
	file, info, err := openLocalRootFileFromPinnedRoot(trustedRoot.handle, location.RelativePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errLocalRootChildUnavailable) {
			return nil, ErrAssetNotFound
		}
		return nil, fmt.Errorf("inspect local original: %w", err)
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()
	if !localActiveLocationMatchesFileInfo(location, info) {
		asset := localThumbnailAsset{SourceKey: location.SourceKey, AssetID: assetID}
		if err := s.resettleLocalOriginalSource(ctx, trustedRoot, asset, location, info, "source_changed_before_original"); err != nil {
			return nil, err
		}
		s.notifyLocalWorkQueued()
		return nil, ErrAssetNotFound
	}
	statusCode := http.StatusOK
	offset := int64(0)
	length := info.Size()
	header := http.Header{}
	header.Set("Accept-Ranges", "bytes")
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(location.RelativePath)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	header.Set("Content-Type", contentType)
	header.Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	header.Set("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape(filepath.Base(location.RelativePath)))
	if clientRequest != nil {
		if rangeHeader := strings.TrimSpace(clientRequest.Header.Get("Range")); rangeHeader != "" && localIfRangeMatches(clientRequest.Header.Get("If-Range"), info.ModTime()) {
			rangeStart, rangeEnd, ok := parseSingleByteRange(rangeHeader, info.Size())
			if !ok {
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

	if clientRequest != nil && clientRequest.Method == http.MethodHead {
		return &UpstreamMediaResponse{
			StatusCode: statusCode,
			Header:     header,
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil
	}
	closeFile = false
	return &UpstreamMediaResponse{
		StatusCode: statusCode,
		Header:     header,
		Body:       &fileSectionReadCloser{SectionReader: io.NewSectionReader(file, offset, length), closer: file},
	}, nil
}

func localIfRangeMatches(value string, modTime time.Time) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	// Local originals currently expose Last-Modified, not a strong ETag. An
	// entity-tag If-Range therefore cannot match this representation.
	if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "W/\"") {
		return false
	}
	parsed, err := http.ParseTime(value)
	if err != nil {
		return false
	}
	return modTime.UTC().Truncate(time.Second).Equal(parsed.UTC().Truncate(time.Second))
}

func (s *Service) localActiveLocation(ctx context.Context, sourceKey string, rootKey string, assetID string) (localActiveLocation, error) {
	sourceKey = strings.TrimSpace(sourceKey)
	rootKey = strings.TrimSpace(rootKey)
	assetID = strings.TrimSpace(assetID)
	if sourceKey == "" || rootKey == "" || assetID == "" {
		return localActiveLocation{}, ErrAssetNotFound
	}
	var location localActiveLocation
	err := s.catalog.db.QueryRowContext(ctx, `SELECT l.id, l.source_key, l.root_key, l.relative_path, l.size_bytes, l.mtime, l.fast_signature, l.file_identity
		FROM local_asset_locations l
		LEFT JOIN local_assets a
			ON a.source_key = l.source_key AND a.asset_id = l.asset_id
		WHERE l.source_key = ?
			AND l.root_key = ?
			AND l.asset_id = ?
			AND l.status = 'active'
		ORDER BY CASE WHEN a.primary_location_id = l.id THEN 0 ELSE 1 END,
			COALESCE(l.verified_at, '') DESC,
			l.mtime DESC,
			l.root_key ASC,
			l.relative_path ASC,
			l.id ASC
		LIMIT 1`, sourceKey, rootKey, assetID).
		Scan(&location.ID, &location.SourceKey, &location.RootKey, &location.RelativePath, &location.SizeBytes, &location.MTime, &location.FastSignature, &location.FileIdentity)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return localActiveLocation{}, ErrAssetNotFound
		}
		return localActiveLocation{}, fmt.Errorf("read local active location: %w", err)
	}
	return location, nil
}

func contextFromRequest(request *http.Request) context.Context {
	if request != nil {
		return request.Context()
	}
	return context.Background()
}
