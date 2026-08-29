package catalog

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestLocalCorruptMediaFailuresAreIsolatedFromGallery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake media helper shell script is unix-only")
	}
	t.Parallel()

	const sourceKey = "1111111111111111"
	rootPath := t.TempDir()
	validJPEG := encodeJPEGForTest(t, 640, 480)
	fixtures := map[string][]byte{
		"valid-family.jpg":           validJPEG,
		"corrupt-restart-marker.jpg": []byte("\xff\xd8\xff\xe0synthetic-corrupt-restart-marker-without-image-data"),
		"truncated-scan.jpg":         validJPEG[:64],
		"empty-a.mp4":                {},
		"empty-b.mp4":                {},
		"empty-c.mp4":                {},
	}
	for name, body := range fixtures {
		if err := os.WriteFile(filepath.Join(rootPath, name), body, 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}

	validRenditionPath := filepath.Join(t.TempDir(), "valid-rendition.jpg")
	if err := os.WriteFile(validRenditionPath, validJPEG, 0o644); err != nil {
		t.Fatalf("WriteFile(valid rendition) error = %v", err)
	}
	helperPath := writeCorruptMediaAwareHelperScript(t, validRenditionPath)
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: sourceKey,
		Name:      "NAS Media",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-media",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaHelperPath: helperPath,
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-media",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	scan, err := service.RunLocalReconciliationScan(context.Background(), sourceKey)
	if err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if scan.DiscoveredPaths != 6 || scan.QueuedMetadata != 6 {
		t.Fatalf("scan result = %+v, want all six paths discovered and queued", scan)
	}
	metadata, err := service.RunLocalMetadataBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	if metadata.ProcessedJobs != 6 || metadata.CompletedJobs != 6 || metadata.FailedJobs != 0 || metadata.RegisteredAssets != 6 {
		t.Fatalf("metadata result = %+v, want all six paths registered without decoding", metadata)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations WHERE status = 'active'`, 6)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'active'`, 4)
	assertLocalScanCount(t, service, `SELECT COUNT(DISTINCT asset_id) FROM local_asset_locations WHERE relative_path LIKE 'empty-%.mp4'`, 1)

	thumbnails, err := service.RunLocalThumbnailBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalThumbnailBatch() error = %v", err)
	}
	if thumbnails.ProcessedJobs != 4 || thumbnails.CompletedJobs != 1 || thumbnails.FailedJobs != 3 || thumbnails.GeneratedAssets != 1 {
		t.Fatalf("thumbnail result = %+v, want one valid asset and three isolated content failures", thumbnails)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'ready'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'failed'`, 3)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions WHERE status = 'ready'`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions WHERE status = 'failed'`, 6)

	page, err := service.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Filename != "valid-family.jpg" {
		t.Fatalf("timeline page = %+v, want only the renderable asset", page)
	}

	diagnostics, err := service.LocalFailureDiagnosticRows(context.Background(), sourceKey)
	if err != nil {
		t.Fatalf("LocalFailureDiagnosticRows() error = %v", err)
	}
	if len(diagnostics) != 3 {
		t.Fatalf("failure diagnostics = %#v, want three content-level failures", diagnostics)
	}
	failurePaths := map[string]bool{}
	for _, diagnostic := range diagnostics {
		if diagnostic.FailureKind != "thumbnail" || diagnostic.Status != "failed" ||
			!strings.Contains(diagnostic.LastError, "synthetic decoder rejected empty or corrupt media") {
			t.Fatalf("failure diagnostic = %+v, want a decoder thumbnail failure", diagnostic)
		}
		failurePaths[diagnostic.RelativePath] = true
	}
	for _, path := range []string{"corrupt-restart-marker.jpg", "truncated-scan.jpg", "empty-a.mp4"} {
		if !failurePaths[path] {
			t.Fatalf("failure diagnostics paths = %#v, want %q", failurePaths, path)
		}
	}

	requeue, err := service.RequeueFailedLocalThumbnails(context.Background())
	if err != nil {
		t.Fatalf("RequeueFailedLocalThumbnails() error = %v", err)
	}
	if requeue.Queued != 3 {
		t.Fatalf("requeue result = %+v, want all three failed assets queued", requeue)
	}
	retry, err := service.RunLocalThumbnailBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalThumbnailBatch(retry) error = %v", err)
	}
	if retry.ProcessedJobs != 3 || retry.FailedJobs != 3 || retry.GeneratedAssets != 0 {
		t.Fatalf("retry result = %+v, want repeat failures without affecting ready media", retry)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'ready'`, 1)
}

func writeCorruptMediaAwareHelperScript(t *testing.T, validRenditionPath string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), mediaHelperBinaryName())
	script := `#!/bin/sh
case "$1" in
health)
  printf '%s\n' '{"schemaVersion":1,"ok":true,"helper":{"version":"0.1.0-test","platform":"test-platform"},"capabilities":{"renderImage":true,"renderVideoPoster":true,"inspectImage":false,"inspectVideo":false}}'
  exit 0
  ;;
render-image|render-video-poster)
  operation="$1"
  shift
  input=""
  output=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
    --input)
      input="$2"
      shift 2
      ;;
    --output)
      output="$2"
      shift 2
      ;;
    *)
      shift
      ;;
    esac
  done
  if [ -z "$input" ] || [ -z "$output" ]; then
    echo "missing media input or output" >&2
    exit 8
  fi
  size="$(wc -c < "$input" | tr -d ' ')"
  if [ "$size" -lt 128 ]; then
    echo "synthetic decoder rejected empty or corrupt media" >&2
    exit 42
  fi
  cp ` + shellQuoteForTest(validRenditionPath) + ` "$output"
  printf '{"schemaVersion":1,"ok":true,"operation":"%s","backend":"synthetic-test","outputPath":"%s"}\n' "$operation" "$output"
  exit 0
  ;;
*)
  echo "unexpected command: $1" >&2
  exit 2
  ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write corrupt-media-aware helper script: %v", err)
	}
	return path
}
