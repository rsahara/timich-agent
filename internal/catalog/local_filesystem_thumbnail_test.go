package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestLocalRenditionResponsesArePrivateAndHideInternalAssetID(t *testing.T) {
	t.Parallel()

	datasource := config.DatasourceConfig{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}
	service, err := NewServiceWithOptions([]config.DatasourceConfig{datasource}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  datasource.RootKey,
			Path: t.TempDir(),
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	const assetID = "internal-asset-id-with-content-hash"
	if _, err := service.catalog.db.Exec(`INSERT INTO local_assets (
		source_key, asset_id, sha1_hex, content_size_bytes, media_type, filename,
		captured_at, captured_at_source, visibility_status, thumbnail_status,
		first_seen_at, updated_at
	) VALUES (?, ?, ?, 4, 'image', 'private.jpg', '2026-07-20T00:00:00Z', 'filesystem',
		'active', 'ready', '2026-07-20T00:00:00Z', '2026-07-20T00:00:00Z')`,
		datasource.SourceKey, assetID, strings.Repeat("a", 40)); err != nil {
		t.Fatalf("insert local asset: %v", err)
	}
	for _, test := range []struct {
		kind     string
		filename string
	}{
		{kind: localRenditionKindPreview, filename: "preview.jpg"},
		{kind: localRenditionKindDetailPreview, filename: "detail-preview.jpg"},
	} {
		relativePath := localRenditionRelativePath(datasource.SourceKey, assetID, test.kind)
		fullPath, err := service.localStateChildPath(relativePath)
		if err != nil {
			t.Fatalf("localStateChildPath(%s) error = %v", test.kind, err)
		}
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", test.kind, err)
		}
		if err := os.WriteFile(fullPath, []byte("jpeg"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", test.kind, err)
		}
		content := []byte("jpeg")
		digest := sha256.Sum256(content)
		if _, err := service.catalog.db.Exec(`INSERT INTO local_renditions (
			source_key, asset_id, kind, status, relative_path, size_bytes, content_sha256,
			generated_at, source_sha1_hex
		) VALUES (?, ?, ?, 'ready', ?, ?, ?, '2026-07-20T00:00:00Z', ?)`,
			datasource.SourceKey, assetID, test.kind, relativePath, len(content), hex.EncodeToString(digest[:]), strings.Repeat("a", 40)); err != nil {
			t.Fatalf("insert %s rendition: %v", test.kind, err)
		}

		request, err := http.NewRequest(http.MethodGet, "http://agent.test/v1/assets/opaque/"+test.kind, nil)
		if err != nil {
			t.Fatalf("NewRequest(%s) error = %v", test.kind, err)
		}
		response, err := service.localRenditionMediaResponse(request, &datasource, assetID, test.kind)
		if err != nil {
			t.Fatalf("localRenditionMediaResponse(%s) error = %v", test.kind, err)
		}
		response.Body.Close()
		if got := response.Header.Get("Cache-Control"); got != "private, max-age=3600" {
			t.Fatalf("%s Cache-Control = %q", test.kind, got)
		}
		if got := response.Header.Get("Content-Disposition"); got != "inline; filename*=UTF-8''"+test.filename || strings.Contains(got, assetID) {
			t.Fatalf("%s Content-Disposition = %q", test.kind, got)
		}
	}
	previewRelativePath := localRenditionRelativePath(datasource.SourceKey, assetID, localRenditionKindPreview)
	previewPath, err := service.localStateChildPath(previewRelativePath)
	if err != nil {
		t.Fatalf("localStateChildPath(preview repair) error = %v", err)
	}
	if err := os.Remove(previewPath); err != nil {
		t.Fatalf("Remove(preview rendition) error = %v", err)
	}
	notifications := 0
	service.SetLocalWorkNotifier(func() { notifications++ })
	request, err := http.NewRequest(http.MethodGet, "http://agent.test/v1/assets/opaque/preview", nil)
	if err != nil {
		t.Fatalf("NewRequest(preview repair) error = %v", err)
	}
	if _, err := service.localRenditionMediaResponse(request, &datasource, assetID, localRenditionKindPreview); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("localRenditionMediaResponse(missing preview) error = %v, want ErrAssetNotFound", err)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions WHERE status = 'pending'`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'pending'`, 1)
	if notifications != 1 {
		t.Fatalf("local work notifications = %d, want 1", notifications)
	}
}

func TestLocalRenditionJPEGQualityProfiles(t *testing.T) {
	t.Parallel()

	if !slices.Equal(localPreviewJPEGQualities, []int{58, 42}) {
		t.Fatalf("localPreviewJPEGQualities = %v, want [58 42]", localPreviewJPEGQualities)
	}
	if !slices.Equal(localDetailPreviewJPEGQualities, []int{82, 62, 42}) {
		t.Fatalf("localDetailPreviewJPEGQualities = %v, want [82 62 42]", localDetailPreviewJPEGQualities)
	}
}

func TestNextLocalRenditionMaxEdge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		width     int
		height    int
		sizeBytes int
		maxBytes  int
		want      int
	}{
		{
			name:      "nas boundary image",
			width:     2560,
			height:    1920,
			sizeBytes: 1049965,
			maxBytes:  1 << 20,
			want:      2480,
		},
		{
			name:      "round down to quantum",
			width:     320,
			height:    240,
			sizeBytes: 8192,
			maxBytes:  4096,
			want:      208,
		},
		{
			name:      "already within limit",
			width:     320,
			height:    240,
			sizeBytes: 4096,
			maxBytes:  4096,
			want:      0,
		},
		{
			name:      "minimum edge cannot shrink",
			width:     localRenditionResizeQuantumPixels,
			height:    localRenditionResizeQuantumPixels,
			sizeBytes: 8192,
			maxBytes:  4096,
			want:      0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := nextLocalRenditionMaxEdge(tt.width, tt.height, tt.sizeBytes, tt.maxBytes); got != tt.want {
				t.Fatalf("nextLocalRenditionMaxEdge() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRenderLocalRenditionFallsBackToMeasuredSmallerEdge(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake media helper shell script is unix-only")
	}

	oversizedPath := writePaddedLocalRenditionJPEG(t, 320, 240, 8192)
	fittedPath := writePaddedLocalRenditionJPEG(t, 208, 156, 2048)
	helperPath, helperLogPath := writeAdaptiveLocalRenditionHelper(t, map[int]string{
		320: oversizedPath,
		208: fittedPath,
	})
	service := &Service{
		dataDir:         t.TempDir(),
		mediaHelperPath: helperPath,
	}
	sourcePath := filepath.Join(t.TempDir(), "source.jpg")
	if err := os.WriteFile(sourcePath, []byte("source"), 0o644); err != nil {
		t.Fatalf("write source fixture: %v", err)
	}

	rendition, err := service.renderLocalRenditionWithMediaHelper(context.Background(), localThumbnailAsset{
		SourceKey: "1111111111111111",
		AssetID:   "asset-1",
	}, localRenditionKindPreview, localMediaInput{Path: sourcePath}, hostedImageProfile{
		MaxEdgePixels: 320,
		MaxBytes:      4096,
		JPEGQualities: []int{58, 42},
	})
	if err != nil {
		t.Fatalf("renderLocalRenditionWithMediaHelper() error = %v", err)
	}
	if rendition.Width != 208 || rendition.Height != 156 || len(rendition.Bytes) != 2048 {
		t.Fatalf("rendition = %dx%d %d bytes, want 208x156 2048 bytes", rendition.Width, rendition.Height, len(rendition.Bytes))
	}

	logBody, err := os.ReadFile(helperLogPath)
	if err != nil {
		t.Fatalf("read helper log: %v", err)
	}
	gotCalls := strings.Split(strings.TrimSpace(string(logBody)), "\n")
	wantCalls := []string{"320 58", "320 42", "208 42"}
	if !slices.Equal(gotCalls, wantCalls) {
		t.Fatalf("helper calls = %q, want %q", gotCalls, wantCalls)
	}
}

func TestRenderLocalRenditionBoundsResizeFallbackAttempts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake media helper shell script is unix-only")
	}

	helperPath, helperLogPath := writeAdaptiveLocalRenditionHelper(t, map[int]string{
		320: writePaddedLocalRenditionJPEG(t, 320, 240, 8192),
		208: writePaddedLocalRenditionJPEG(t, 208, 156, 8192),
		128: writePaddedLocalRenditionJPEG(t, 128, 96, 8192),
	})
	service := &Service{
		dataDir:         t.TempDir(),
		mediaHelperPath: helperPath,
	}
	sourcePath := filepath.Join(t.TempDir(), "source.jpg")
	if err := os.WriteFile(sourcePath, []byte("source"), 0o644); err != nil {
		t.Fatalf("write source fixture: %v", err)
	}

	_, err := service.renderLocalRenditionWithMediaHelper(context.Background(), localThumbnailAsset{
		SourceKey: "1111111111111111",
		AssetID:   "asset-1",
	}, localRenditionKindPreview, localMediaInput{Path: sourcePath}, hostedImageProfile{
		MaxEdgePixels: 320,
		MaxBytes:      4096,
		JPEGQualities: []int{42},
	})
	if !errors.Is(err, ErrMediaTooLarge) {
		t.Fatalf("renderLocalRenditionWithMediaHelper() error = %v, want %v", err, ErrMediaTooLarge)
	}

	logBody, err := os.ReadFile(helperLogPath)
	if err != nil {
		t.Fatalf("read helper log: %v", err)
	}
	gotCalls := strings.Split(strings.TrimSpace(string(logBody)), "\n")
	wantCalls := []string{"320 42", "208 42", "128 42"}
	if !slices.Equal(gotCalls, wantCalls) {
		t.Fatalf("helper calls = %q, want bounded calls %q", gotCalls, wantCalls)
	}
}

func writePaddedLocalRenditionJPEG(t *testing.T, width int, height int, sizeBytes int) string {
	t.Helper()

	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, width, height)), &jpeg.Options{Quality: 42}); err != nil {
		t.Fatalf("encode jpeg fixture: %v", err)
	}
	if encoded.Len() > sizeBytes {
		t.Fatalf("jpeg fixture size = %d, want <= %d before padding", encoded.Len(), sizeBytes)
	}
	encoded.Write(make([]byte, sizeBytes-encoded.Len()))
	path := filepath.Join(t.TempDir(), fmt.Sprintf("%dx%d-%d.jpg", width, height, sizeBytes))
	if err := os.WriteFile(path, encoded.Bytes(), 0o644); err != nil {
		t.Fatalf("write jpeg fixture: %v", err)
	}
	return path
}

func writeAdaptiveLocalRenditionHelper(t *testing.T, outputs map[int]string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, mediaHelperBinaryName())
	logPath := filepath.Join(dir, "media-helper.log")
	var cases strings.Builder
	edges := make([]int, 0, len(outputs))
	for edge := range outputs {
		edges = append(edges, edge)
	}
	sort.Ints(edges)
	for _, edge := range edges {
		outputPath := outputs[edge]
		fmt.Fprintf(&cases, "%d) cp %s \"$output\" ;;\n", edge, shellQuoteForTest(outputPath))
	}
	script := `#!/bin/sh
case "$1" in
render-image)
  shift
  output=""
  max_edge=""
  quality=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
    --output)
      output="$2"
      shift 2
      ;;
    --max-edge)
      max_edge="$2"
      shift 2
      ;;
    --quality)
      quality="$2"
      shift 2
      ;;
    *)
      shift
      ;;
    esac
  done
  printf '%s %s\n' "$max_edge" "$quality" >> ` + shellQuoteForTest(logPath) + `
  case "$max_edge" in
` + cases.String() + `  *)
    echo "unexpected max edge: $max_edge" >&2
    exit 9
    ;;
  esac
  printf '%s\n' '{"schemaVersion":1,"ok":true,"operation":"render-image","backend":"libvips-cli","outputPath":"rendition.jpg"}'
  exit 0
  ;;
*)
  echo "unexpected command: $1" >&2
  exit 2
  ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write adaptive media helper: %v", err)
	}
	return path, logPath
}
