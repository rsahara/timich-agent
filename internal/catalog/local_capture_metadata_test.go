package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveLocalCaptureDate(t *testing.T) {
	location, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		candidate localCaptureDateCandidate
		want      string
	}{
		{"original offset subseconds", localCaptureDateCandidate{Value: "2026:09:06 12:34:56", Offset: "+09:00", Subsecond: "123", Source: "exif_original"}, "2026-09-06T03:34:56.123Z"},
		{"assumed timezone", localCaptureDateCandidate{Value: "2026:09:06 12:34:56", Source: "exif_digitized"}, "2026-09-06T03:34:56Z"},
		{"quicktime offset", localCaptureDateCandidate{Value: "2026-09-06T12:34:56+0900", Source: "quicktime_creationdate"}, "2026-09-06T03:34:56Z"},
		{"utc container", localCaptureDateCandidate{Value: "2026-09-06T03:34:56.123456Z", Source: "container_creation_time"}, "2026-09-06T03:34:56.123456Z"},
		{"invalid date", localCaptureDateCandidate{Value: "2026:02:30 12:34:56", Source: "exif_original"}, ""},
		{"missing date", localCaptureDateCandidate{Value: "0000:00:00 00:00:00", Source: "exif_original"}, ""},
		{"quicktime epoch", localCaptureDateCandidate{Value: "1904-01-01T00:00:00Z", Source: "container_creation_time"}, ""},
		{"unix epoch", localCaptureDateCandidate{Value: "1970-01-01T00:00:00Z", Source: "video_stream_creation_time"}, ""},
		{"invalid offset", localCaptureDateCandidate{Value: "2026:09:06 12:34:56", Offset: "bogus", Source: "exif_original"}, ""},
		{"filename excluded", localCaptureDateCandidate{Value: "2026-09-06T12:34:56Z", Source: "filename"}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := resolveLocalCaptureDate(test.candidate, location)
			if ok != (test.want != "") || (ok && got.Format(time.RFC3339Nano) != test.want) {
				t.Fatalf("date=%v valid=%v, want %s", got, ok, test.want)
			}
		})
	}
	scanTime := time.Date(2026, 9, 14, 1, 2, 3, 0, time.UTC)
	if got := fallbackLocalCaptureMetadata(time.Time{}, scanTime); got.Source != "scan_time" || !got.CapturedAt.Equal(scanTime) {
		t.Fatalf("scan fallback: %+v", got)
	}
}

func writeCaptureMetadataHelper(t testing.TB, candidates []localCaptureDateCandidate) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "helper")
	imageResponse, _ := json.Marshal(map[string]any{"schemaVersion": 1, "ok": true, "operation": "inspect-image", "media": map[string]any{"mediaType": "image", "captureDates": candidates}})
	videoResponse, _ := json.Marshal(map[string]any{"schemaVersion": 1, "ok": true, "operation": "inspect-video", "media": map[string]any{"mediaType": "video", "durationMs": 12345, "captureDates": candidates}})
	body := fmt.Sprintf(`#!/bin/sh
case "$1" in
health) printf '%%s\n' '{"schemaVersion":1,"ok":true,"capabilities":{"captureImage":true,"captureVideo":true,"inspectImage":true,"inspectVideo":true}}' ;;
inspect-image) printf '%%s\n' '%s' ;;
inspect-video) printf '%%s\n' '%s' ;;
*) exit 1 ;;
esac
`, imageResponse, videoResponse)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocalCaptureMetadataRegistrationAndRefreshPreserveDerivedWork(t *testing.T) {
	for _, mediaType := range []string{"image", "video"} {
		t.Run(mediaType, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			extension := ".jpg"
			if mediaType == "video" {
				extension = ".mp4"
			}
			path := filepath.Join(root, "media"+extension)
			if err := os.WriteFile(path, []byte("media-content"), 0o600); err != nil {
				t.Fatal(err)
			}
			mtime := time.Date(2026, 9, 13, 13, 19, 29, 0, time.UTC)
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			s := newLocalVideoMetadataTestService(t, root, "")
			if _, err := s.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RunLocalMetadataBatch(ctx, 10); err != nil {
				t.Fatal(err)
			}
			var assetID string
			if err := s.catalog.queryDB().QueryRow(`SELECT asset_id FROM local_assets`).Scan(&assetID); err != nil {
				t.Fatal(err)
			}
			// Guard existing derived state against any write from metadata-only work.
			if _, err := s.catalog.db.Exec(`UPDATE local_assets SET thumbnail_status='ready';
				INSERT INTO local_renditions(source_key,asset_id,kind,status,relative_path,source_sha1_hex)
				SELECT source_key,asset_id,'preview','ready','preview.jpg',sha1_hex FROM local_assets;
				INSERT INTO local_renditions(source_key,asset_id,kind,status,relative_path,source_sha1_hex)
				SELECT source_key,asset_id,'detail_preview','ready','detail.jpg',sha1_hex FROM local_assets;
			CREATE TRIGGER capture_guard_thumbnail BEFORE UPDATE OF thumbnail_status ON local_assets BEGIN SELECT RAISE(ABORT,'thumbnail reset'); END;
			CREATE TRIGGER capture_guard_jobs BEFORE INSERT ON local_scan_jobs WHEN NEW.job_kind != 'metadata' BEGIN SELECT RAISE(ABORT,'derived job queued'); END;
			CREATE TRIGGER capture_guard_rendition_updates BEFORE UPDATE ON local_renditions BEGIN SELECT RAISE(ABORT,'rendition updated'); END;
			CREATE TRIGGER capture_guard_renditions BEFORE DELETE ON local_renditions BEGIN SELECT RAISE(ABORT,'renditions removed'); END;
			CREATE TRIGGER capture_guard_vectors BEFORE DELETE ON semantic_vectors BEGIN SELECT RAISE(ABORT,'vectors removed'); END;`); err != nil {
				t.Fatal(err)
			}
			if mediaType == "image" {
				var canonicalID string
				if err := s.catalog.queryDB().QueryRow(`SELECT canonical_asset_id FROM catalog_assets`).Scan(&canonicalID); err != nil {
					t.Fatal(err)
				}
				for _, model := range []string{"current", "already-dirty", "other-representative"} {
					representative := "1111111111111111"
					if model == "other-representative" {
						representative = "2222222222222222"
					}
					insertCanonicalSemanticVectorForTest(t, s.catalog, ctx, canonicalID, representative, assetID, model, "test-space", 4, []float32{1, 0, 0, 0}, "local_detail_preview", formatCatalogTime(mtime))
				}
				if _, err := s.catalog.db.Exec(`UPDATE canonical_semantic_vector_inputs SET refresh_required=1 WHERE model_id='already-dirty';
					CREATE TRIGGER capture_guard_vector_updates BEFORE UPDATE ON semantic_vectors BEGIN SELECT RAISE(ABORT,'vectors updated'); END;`); err != nil {
					t.Fatal(err)
				}
			}
			helper := writeCaptureMetadataHelper(t, []localCaptureDateCandidate{
				{Value: "0000:00:00 00:00:00", Source: "exif_original"},
				{Value: "2026:09:06 12:00:00", Offset: "+09:00", Source: "exif_digitized"},
			})
			s.mediaHelperPath, s.mediaHelperCheck = helper, localMediaHelperCapabilityStatus{}
			repair, err := s.RepairLocalMetadata(ctx)
			if err != nil || repair.CaptureDateQueued != 1 || repair.Queued != 1 {
				t.Fatalf("repair=%+v err=%v", repair, err)
			}
			repeated, err := s.RepairLocalMetadata(ctx)
			if err != nil || repeated.Queued != 0 {
				t.Fatalf("repeated=%+v err=%v", repeated, err)
			}
			result, err := s.RunLocalMetadataBatch(ctx, 10)
			if err != nil || result.CompletedJobs != 1 || result.RegisteredAssets != 0 || result.FailedJobs != 0 {
				t.Fatalf("refresh=%+v err=%v", result, err)
			}
			if mediaType == "image" {
				assertLocalScanCount(t, s, `SELECT COUNT(*) FROM semantic_vectors WHERE status='ready'`, 3)
				assertLocalScanCount(t, s, `SELECT COUNT(*) FROM canonical_semantic_vector_inputs WHERE model_id='current' AND refresh_required=0`, 1)
				assertLocalScanCount(t, s, `SELECT COUNT(*) FROM canonical_semantic_vector_inputs WHERE model_id IN ('already-dirty','other-representative') AND refresh_required=1`, 2)
			}
			for _, table := range []string{"local_assets", "catalog_assets", "catalog_canonical_assets", "catalog_gallery_projection"} {
				var date string
				if err := s.catalog.queryDB().QueryRow(`SELECT captured_at FROM ` + table).Scan(&date); err != nil {
					t.Fatalf("%s: %v", table, err)
				}
				if date != "2026-09-06T03:00:00.000000000Z" {
					t.Fatalf("%s date=%s", table, date)
				}
			}
			var afterID, provenance, thumbnail string
			if err := s.catalog.queryDB().QueryRow(`SELECT asset_id,captured_at_source,thumbnail_status FROM local_assets`).Scan(&afterID, &provenance, &thumbnail); err != nil {
				t.Fatal(err)
			}
			if afterID != assetID || provenance != "exif_digitized" || thumbnail != "ready" {
				t.Fatalf("state=%s %s %s", afterID, provenance, thumbnail)
			}
			info, err := os.Stat(path)
			if err != nil || !info.ModTime().Equal(mtime) {
				t.Fatalf("original changed: %v %v", info, err)
			}
			repair, err = s.RepairLocalMetadata(ctx)
			if err != nil || repair.Queued != 0 {
				t.Fatalf("completed repair=%+v err=%v", repair, err)
			}
			var day string
			if err := s.catalog.queryDB().QueryRow(`SELECT captured_day FROM catalog_gallery_projection_days WHERE item_count > 0`).Scan(&day); err != nil || day != "2026-09-06" {
				t.Fatalf("day=%s err=%v", day, err)
			}
		})
	}
}

func TestLocalCaptureMetadataAbsentAndNewAssets(t *testing.T) {
	for _, dates := range [][]localCaptureDateCandidate{{}, {{Value: "2020:02:03 12:00:00", Source: "exif_original"}}} {
		ctx := context.Background()
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "image.jpg"), []byte("image"), 0o600); err != nil {
			t.Fatal(err)
		}
		s := newLocalVideoMetadataTestService(t, root, writeCaptureMetadataHelper(t, dates))
		if _, err := s.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
			t.Fatal(err)
		}
		result, err := s.RunLocalMetadataBatch(ctx, 10)
		if err != nil || result.RegisteredAssets != 1 || result.FailedJobs != 0 {
			t.Fatalf("registration=%+v err=%v", result, err)
		}
		var source string
		if err := s.catalog.queryDB().QueryRow(`SELECT captured_at_source FROM local_assets`).Scan(&source); err != nil {
			t.Fatal(err)
		}
		if (len(dates) == 0 && source != "file_mtime") || (len(dates) > 0 && source != "exif_original") {
			t.Fatalf("source=%s", source)
		}
		repair, err := s.RepairLocalMetadata(ctx)
		if err != nil || repair.Queued != 0 {
			t.Fatalf("repaired known metadata=%+v err=%v", repair, err)
		}
		// Changing the assumed timezone makes one refresh eligible again.
		s.captureLocation = time.FixedZone("changed-zone", 3600)
		repair, err = s.RepairLocalMetadata(ctx)
		if err != nil || repair.CaptureDateQueued != 1 {
			t.Fatalf("timezone refresh=%+v err=%v", repair, err)
		}
	}
}

func TestLocalCaptureMetadataRefreshFailurePreservesDateAndCanRetry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "image.jpg"), []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newLocalVideoMetadataTestService(t, root, "")
	if _, err := s.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunLocalMetadataBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	helper := writeCaptureMetadataHelper(t, []localCaptureDateCandidate{})
	s.mediaHelperPath, s.mediaHelperCheck = helper, localMediaHelperCapabilityStatus{}
	if _, err := s.RepairLocalMetadata(ctx); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(helper)
	if err := os.WriteFile(helper, []byte(strings.Replace(string(body), "inspect-image) printf", "inspect-image) exit 1; printf", 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := s.RunLocalMetadataBatch(ctx, 10)
	if err != nil || result.FailedJobs != 1 {
		t.Fatalf("failed refresh=%+v err=%v", result, err)
	}
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_capture_metadata_state`, 0)
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_assets WHERE captured_at_source='file_mtime' AND visibility_status='active'`, 1)
	if err := os.WriteFile(helper, body, 0o700); err != nil {
		t.Fatal(err)
	}
	repair, err := s.RepairLocalMetadata(ctx)
	if err != nil || repair.Queued != 1 {
		t.Fatalf("retry=%+v err=%v", repair, err)
	}
	result, err = s.RunLocalMetadataBatch(ctx, 10)
	if err != nil || result.CompletedJobs != 1 || result.FailedJobs != 0 {
		t.Fatalf("retry complete=%+v err=%v", result, err)
	}
}

func TestLocalCaptureMetadataUpgradeAndRestartResume(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "image.jpg"), []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newLocalVideoMetadataTestService(t, root, "")
	if _, err := s.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunLocalMetadataBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	// Simulate the existing V5 schema, which has assets but no check-state table.
	if _, err := s.catalog.db.Exec(`DROP TABLE local_capture_metadata_state`); err != nil {
		t.Fatal(err)
	}
	reopen := func() {
		t.Helper()
		if err := s.catalog.Close(); err != nil {
			t.Fatal(err)
		}
		store, err := LoadOrCreateCatalogStore(s.dataDir)
		if err != nil {
			t.Fatal(err)
		}
		store.datasourceState = &s.datasourceState
		s.catalog = store
	}
	reopen()
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_assets`, 1)
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_capture_metadata_state`, 0)
	s.mediaHelperPath, s.mediaHelperCheck = writeCaptureMetadataHelper(t, []localCaptureDateCandidate{}), localMediaHelperCapabilityStatus{}
	if result, err := s.RepairLocalMetadata(ctx); err != nil || result.CaptureDateQueued != 1 {
		t.Fatalf("queue=%+v err=%v", result, err)
	}
	if _, err := s.catalog.db.Exec(`UPDATE local_scan_jobs SET status='running' WHERE job_kind='metadata' AND status='queued'`); err != nil {
		t.Fatal(err)
	}
	reopen()
	if count, err := s.ResetRunningLocalScanJobs(ctx); err != nil || count != 1 {
		t.Fatalf("recover=%d err=%v", count, err)
	}
	if result, err := s.RunLocalMetadataBatch(ctx, 10); err != nil || result.CompletedJobs != 1 || result.FailedJobs != 0 {
		t.Fatalf("resume=%+v err=%v", result, err)
	}
	reopen()
	if result, err := s.RepairLocalMetadata(ctx); err != nil || result.Queued != 0 {
		t.Fatalf("repeat after restart=%+v err=%v", result, err)
	}
}

func TestLocalCaptureMetadataDuplicateUsesPrimaryAndReregistersSameContent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for i, name := range []string{"one.jpg"} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("same-image"), 0o600); err != nil {
			t.Fatal(err)
		}
		mtime := time.Date(2026, 9, 6+i, 1, 0, 0, 0, time.UTC)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	s := newLocalVideoMetadataTestService(t, root, "")
	if _, err := s.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunLocalMetadataBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_assets`, 1)
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_asset_locations`, 1)
	var primary, mtime string
	if err := s.catalog.queryDB().QueryRow(`SELECT l.relative_path,l.mtime FROM local_assets a JOIN local_asset_locations l ON l.id=a.primary_location_id`).Scan(&primary, &mtime); err != nil {
		t.Fatal(err)
	}
	s.mediaHelperPath, s.mediaHelperCheck = writeCaptureMetadataHelper(t, []localCaptureDateCandidate{}), localMediaHelperCapabilityStatus{}
	if result, err := s.RepairLocalMetadata(ctx); err != nil || result.CaptureDateQueued != 1 {
		t.Fatalf("duplicate repair=%+v err=%v", result, err)
	}
	if result, err := s.RunLocalMetadataBatch(ctx, 10); err != nil || result.CompletedJobs != 1 || result.FailedJobs != 0 {
		t.Fatalf("duplicate refresh=%+v err=%v", result, err)
	}
	assertDate := func(want string) {
		t.Helper()
		for _, table := range []string{"local_assets", "catalog_assets"} {
			var got string
			if err := s.catalog.queryDB().QueryRow(`SELECT captured_at FROM ` + table).Scan(&got); err != nil || got != want {
				t.Fatalf("%s date=%s want=%s err=%v", table, got, want, err)
			}
		}
	}
	assertDate(mtime)
	// Same bytes at the same primary path, with a new mtime, go through normal
	// discovery/registration and must not retain a stale fallback date.
	changed := time.Date(2026, 9, 8, 1, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, primary), changed, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
		t.Fatal(err)
	}
	if result, err := s.RunLocalMetadataBatch(ctx, 10); err != nil || result.FailedJobs != 0 {
		t.Fatalf("reregister=%+v err=%v", result, err)
	}
	assertDate(formatCatalogTime(changed))
	duplicate := filepath.Join(root, "two.jpg")
	if err := os.WriteFile(duplicate, []byte("same-image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
		t.Fatal(err)
	}
	if result, err := s.RunLocalMetadataBatch(ctx, 10); err != nil || result.FailedJobs != 0 {
		t.Fatalf("duplicate registration=%+v err=%v", result, err)
	}
	assertDate(formatCatalogTime(changed))
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_assets`, 1)
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_asset_locations`, 2)
}

func TestLocalCaptureMetadataRefreshRejectsReplacementAndRecoversCancellation(t *testing.T) {
	for _, cancelWork := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancelWork), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			root := t.TempDir()
			path := filepath.Join(root, "image.jpg")
			if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			s := newLocalVideoMetadataTestService(t, root, "")
			if _, err := s.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RunLocalMetadataBatch(ctx, 10); err != nil {
				t.Fatal(err)
			}
			helper, started, release := writeBlockingMediaHelperVideoInspectionScript(t, 12345)
			body, err := os.ReadFile(helper)
			if err != nil {
				t.Fatal(err)
			}
			script := strings.ReplaceAll(string(body), `"inspectVideo":true`, `"inspectVideo":true,"captureImage":true`)
			script = strings.ReplaceAll(script, "inspect-video", "inspect-image")
			script = strings.ReplaceAll(script, `"mediaType":"video"`, `"mediaType":"image","captureDates":[]`)
			if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			s.mediaHelperPath, s.mediaHelperCheck = helper, localMediaHelperCapabilityStatus{}
			if _, err := s.RepairLocalMetadata(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.WriteFile(release, []byte("release"), 0o600) })
			type outcome struct {
				result LocalMetadataBatchResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() { result, err := s.RunLocalMetadataBatch(ctx, 10); done <- outcome{result, err} }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(started); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("inspection did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if cancelWork {
				cancel()
			} else {
				replacement := filepath.Join(root, "replacement")
				if err := os.WriteFile(replacement, []byte("replaced"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case got := <-done:
				if cancelWork && got.err != context.Canceled {
					t.Fatalf("cancel: %+v", got)
				}
				if !cancelWork && (got.err != nil || got.result.SettlingJobs != 1 || got.result.FailedJobs != 0) {
					t.Fatalf("replacement: %+v", got)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("metadata did not stop")
			}
			assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_capture_metadata_state`, 0)
			assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind='metadata' AND status='running'`, 0)
			if cancelWork {
				assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind='metadata' AND status='queued'`, 1)
			}
		})
	}
}

// Run with TIMICH_TEST_MEDIA_HELPER pointing to a built Rust helper to exercise
// the actual parser through the Agent's pinned descriptor and durable repair.
func TestLocalCaptureMetadataLargePNGWithRealHelper(t *testing.T) {
	helper := os.Getenv("TIMICH_TEST_MEDIA_HELPER")
	if helper == "" {
		t.Skip("set TIMICH_TEST_MEDIA_HELPER to the built media helper")
	}
	helper, err := filepath.Abs(helper)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "large.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	err = encoder.Encode(file, image.NewNRGBA(image.Rect(0, 0, 2560, 1800)))
	closeErr := file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= 16*1024*1024 {
		t.Fatalf("PNG fixture too small: %d", info.Size())
	}
	s := newLocalVideoMetadataTestService(t, root, filepath.Join(root, "missing-helper"))
	ctx := context.Background()
	if _, err := s.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
		t.Fatal(err)
	}
	if result, err := s.RunLocalMetadataBatch(ctx, 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("register: %+v, %v", result, err)
	}
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_capture_metadata_state`, 0)
	s.mediaHelperPath, s.mediaHelperCheck = helper, localMediaHelperCapabilityStatus{}
	if result, err := s.RepairLocalMetadata(ctx); err != nil || result.CaptureDateQueued != 1 {
		t.Fatalf("repair: %+v, %v", result, err)
	}
	if result, err := s.RunLocalMetadataBatch(ctx, 10); err != nil || result.CompletedJobs != 1 || result.FailedJobs != 0 {
		t.Fatalf("refresh: %+v, %v", result, err)
	}
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_capture_metadata_state`, 1)
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_assets WHERE captured_at_source='file_mtime' AND visibility_status='active'`, 1)
	if result, err := s.RepairLocalMetadata(ctx); err != nil || result.Queued != 0 {
		t.Fatalf("repeat: %+v, %v", result, err)
	}
	t.Logf("repaired a valid %d-byte PNG without EXIF and recorded the completed check", info.Size())
}
