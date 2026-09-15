package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestLocalVideoMetadataStoresInspectedDuration(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "clip.mov"), []byte("video"), 0o644); err != nil {
		t.Fatalf("WriteFile(video) error = %v", err)
	}
	helperPath, helperLogPath := writeFakeMediaHelperVideoInspectionScript(t, 12_345)
	service := newLocalVideoMetadataTestService(t, rootPath, helperPath)

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	metadata, err := service.RunLocalMetadataBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	if metadata.RegisteredAssets != 1 || metadata.FailedJobs != 0 {
		t.Fatalf("metadata batch = %+v, want one registered video", metadata)
	}

	for _, query := range []string{
		`SELECT duration FROM local_assets WHERE media_type = 'video'`,
		`SELECT duration FROM catalog_assets WHERE media_type = 'video'`,
		`SELECT duration FROM catalog_canonical_assets WHERE media_type = 'video'`,
	} {
		var duration string
		if err := service.catalog.queryDB().QueryRowContext(context.Background(), query).Scan(&duration); err != nil {
			t.Fatalf("read stored duration with %q: %v", query, err)
		}
		if duration != "0:00:12.345000" {
			t.Fatalf("stored duration = %q, want 0:00:12.345000", duration)
		}
	}
	logBody, err := os.ReadFile(helperLogPath)
	if err != nil {
		t.Fatalf("ReadFile(media helper log) error = %v", err)
	}
	if !strings.Contains(string(logBody), "inspect-video --input /dev/fd/3") {
		t.Fatalf("media helper log = %q, want pinned descriptor input", string(logBody))
	}
}

func TestLocalVideoMetadataResettlesWhenPathChangesDuringInspection(t *testing.T) {
	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "clip.mov")
	if err := os.WriteFile(mediaPath, []byte("original-video"), 0o644); err != nil {
		t.Fatalf("WriteFile(original video) error = %v", err)
	}
	helperPath, inspectionStarted, releaseInspection := writeBlockingMediaHelperVideoInspectionScript(t, 12_345)
	service := newLocalVideoMetadataTestService(t, rootPath, helperPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}

	t.Cleanup(func() { _ = os.WriteFile(releaseInspection, []byte("release"), 0o600) })
	type batchOutcome struct {
		result LocalMetadataBatchResult
		err    error
	}
	batchDone := make(chan batchOutcome, 1)
	go func() {
		result, err := service.RunLocalMetadataBatch(context.Background(), 10)
		batchDone <- batchOutcome{result: result, err: err}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(inspectionStarted); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Stat(inspection started) error = %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("video inspection did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	replacementPath := filepath.Join(rootPath, "replacement.mov")
	if err := os.WriteFile(replacementPath, []byte("replacement-video"), 0o644); err != nil {
		t.Fatalf("WriteFile(replacement video) error = %v", err)
	}
	if err := os.Rename(replacementPath, mediaPath); err != nil {
		t.Fatalf("Rename(replacement video) error = %v", err)
	}
	if err := os.WriteFile(releaseInspection, []byte("release"), 0o600); err != nil {
		t.Fatalf("release video inspection: %v", err)
	}

	select {
	case outcome := <-batchDone:
		if outcome.err != nil {
			t.Fatalf("RunLocalMetadataBatch() error = %v", outcome.err)
		}
		if outcome.result.ProcessedJobs != 1 || outcome.result.SettlingJobs != 1 || outcome.result.RegisteredAssets != 0 || outcome.result.FailedJobs != 0 {
			t.Fatalf("metadata batch = %+v, want replaced video returned to settling", outcome.result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("metadata batch did not finish after releasing video inspection")
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM catalog_assets`, 0)
}

func TestRepairLocalMetadataBackfillsVideoDurationWithoutReregisteringAsset(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "clip.mov"), []byte("video"), 0o644); err != nil {
		t.Fatalf("WriteFile(video) error = %v", err)
	}
	service := newLocalVideoMetadataTestService(t, rootPath, "")
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("initial metadata batch = %+v, %v, want one registered video", result, err)
	}
	var before sql.NullString
	if err := service.catalog.queryDB().QueryRowContext(context.Background(), `SELECT duration FROM local_assets WHERE media_type = 'video'`).Scan(&before); err != nil {
		t.Fatalf("read duration before repair: %v", err)
	}
	if before.Valid {
		t.Fatalf("duration before repair = %q, want NULL", before.String)
	}
	unavailableRepair, err := service.RepairLocalMetadata(context.Background())
	if err != nil {
		t.Fatalf("RepairLocalMetadata(without inspection helper) error = %v", err)
	}
	if unavailableRepair.Queued != 0 || unavailableRepair.VideoDurationQueued != 0 || unavailableRepair.VideoDurationSkipped != 1 {
		t.Fatalf("metadata repair without inspection helper = %+v, want one skipped duration", unavailableRepair)
	}

	helperPath, _ := writeFakeMediaHelperVideoInspectionScript(t, 65_432)
	service.mediaHelperPath = helperPath
	service.mediaHelperAuto = false
	service.mediaHelperCheck = localMediaHelperCapabilityStatus{}
	repair, err := service.RepairLocalMetadata(context.Background())
	if err != nil {
		t.Fatalf("RepairLocalMetadata() error = %v", err)
	}
	if repair.Queued != 1 || repair.VideoDurationQueued != 1 || repair.FailedQueued != 0 {
		t.Fatalf("metadata repair = %+v, want one duration backfill", repair)
	}

	metadata, err := service.RunLocalMetadataBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch(backfill) error = %v", err)
	}
	if metadata.ProcessedJobs != 1 || metadata.CompletedJobs != 1 || metadata.RegisteredAssets != 0 || metadata.FailedJobs != 0 {
		t.Fatalf("duration backfill batch = %+v, want one metadata-only completion without registration", metadata)
	}
	var duration string
	if err := service.catalog.queryDB().QueryRowContext(context.Background(), `SELECT duration FROM local_assets WHERE media_type = 'video'`).Scan(&duration); err != nil {
		t.Fatalf("read duration after repair: %v", err)
	}
	if duration != "0:01:05.432000" {
		t.Fatalf("duration after repair = %q, want 0:01:05.432000", duration)
	}

	repair, err = service.RepairLocalMetadata(context.Background())
	if err != nil {
		t.Fatalf("second RepairLocalMetadata() error = %v", err)
	}
	if repair.Queued != 0 || repair.VideoDurationQueued != 0 {
		t.Fatalf("second metadata repair = %+v, want idempotent no-op", repair)
	}
}

func TestRepairLocalMetadataQueuesFailedMissingVideoDurationOnce(t *testing.T) {
	t.Parallel()

	service := newFailedMissingVideoDurationTestService(t)
	helperPath, _ := writeFakeMediaHelperVideoInspectionScript(t, 65_432)
	service.mediaHelperPath = helperPath
	service.mediaHelperAuto = false
	service.mediaHelperCheck = localMediaHelperCapabilityStatus{}

	repair, err := service.RepairLocalMetadata(context.Background())
	if err != nil {
		t.Fatalf("RepairLocalMetadata() error = %v", err)
	}
	if repair.Queued != 1 || repair.VideoDurationQueued != 1 || repair.FailedQueued != 0 || repair.VideoDurationSkipped != 0 {
		t.Fatalf("metadata repair = %+v, want one duration queue item without failed-job double count", repair)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'failed'`, 0)
}

func TestRepairLocalMetadataDoesNotRecountAlreadyQueuedMissingVideoDuration(t *testing.T) {
	t.Parallel()

	service := newFailedMissingVideoDurationTestService(t)
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation,
			location_id, status, scheduled_at, sort_at
		)
		SELECT source_key, job_kind, ?, root_key, root_generation,
			location_id, 'queued', ?, sort_at
		FROM local_scan_jobs
		WHERE job_kind = 'metadata' AND status = 'failed'`,
		localMetadataBackgroundPriority,
		formatCatalogTime(time.Now().UTC()),
	); err != nil {
		t.Fatalf("insert existing queued duration job: %v", err)
	}
	helperPath, _ := writeFakeMediaHelperVideoInspectionScript(t, 65_432)
	service.mediaHelperPath = helperPath
	service.mediaHelperAuto = false
	service.mediaHelperCheck = localMediaHelperCapabilityStatus{}

	repair, err := service.RepairLocalMetadata(context.Background())
	if err != nil {
		t.Fatalf("RepairLocalMetadata() error = %v", err)
	}
	if repair.Queued != 0 || repair.VideoDurationQueued != 0 || repair.FailedQueued != 0 || repair.VideoDurationSkipped != 0 {
		t.Fatalf("metadata repair = %+v, want existing duration queue excluded from failed repair", repair)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'failed'`, 0)
}

func TestRepairLocalMetadataLeavesFailedMissingVideoDurationWhenInspectionUnavailable(t *testing.T) {
	t.Parallel()

	service := newFailedMissingVideoDurationTestService(t)
	repair, err := service.RepairLocalMetadata(context.Background())
	if err != nil {
		t.Fatalf("RepairLocalMetadata() error = %v", err)
	}
	if repair.Queued != 0 || repair.VideoDurationQueued != 0 || repair.FailedQueued != 0 || repair.VideoDurationSkipped != 1 {
		t.Fatalf("metadata repair = %+v, want failed duration item skipped without requeue", repair)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued'`, 0)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'failed'`, 1)
}

func newFailedMissingVideoDurationTestService(t *testing.T) *Service {
	t.Helper()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "clip.mov"), []byte("video"), 0o644); err != nil {
		t.Fatalf("WriteFile(video) error = %v", err)
	}
	service := newLocalVideoMetadataTestService(t, rootPath, "")
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("initial metadata batch = %+v, %v, want one registered video", result, err)
	}
	nowText := formatCatalogTime(time.Now().UTC())
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation,
			location_id, status, scheduled_at, sort_at, completed_at, last_error
		)
		SELECT l.source_key, ?, ?, l.root_key, rs.root_generation,
			l.id, 'failed', ?, l.mtime, ?, 'video duration inspection failed'
		FROM local_asset_locations l
		JOIN local_scan_root_state rs
			ON rs.source_key = l.source_key
			AND rs.root_key = l.root_key
		WHERE l.status = 'active' AND COALESCE(l.asset_id, '') != ''`,
		localMetadataJobKind,
		localMetadataBackgroundPriority,
		nowText,
		nowText,
	); err != nil {
		t.Fatalf("insert failed duration metadata job: %v", err)
	}
	return service
}

func newLocalVideoMetadataTestService(t testing.TB, rootPath string, helperPath string) *Service {
	t.Helper()
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			SettlingDuration: "1ns",
		},
	}}, ServiceOptions{
		DataDir:         t.TempDir(),
		MediaHelperPath: helperPath,
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service
}

func writeFakeMediaHelperVideoInspectionScript(t *testing.T, durationMS int64) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, mediaHelperBinaryName())
	logPath := filepath.Join(dir, "media-helper.log")
	script := `#!/bin/sh
case "$1" in
health)
  printf '%s\n' '{"schemaVersion":1,"ok":true,"helper":{"version":"0.1.0-test","platform":"test-platform"},"capabilities":{"renderImage":false,"renderVideoPoster":false,"inspectImage":false,"inspectVideo":true}}'
  exit 0
  ;;
inspect-video)
  printf '%s\n' "$*" >> ` + shellQuoteForTest(logPath) + `
  printf '%s\n' '{"schemaVersion":1,"ok":true,"operation":"inspect-video","backend":"ffprobe-cli","media":{"mediaType":"video","durationMs":` + fmt.Sprintf("%d", durationMS) + `}}'
  exit 0
  ;;
*)
  echo "unexpected command: $1" >&2
  exit 2
  ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake video inspection helper: %v", err)
	}
	return path, logPath
}

func writeBlockingMediaHelperVideoInspectionScript(t *testing.T, durationMS int64) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, mediaHelperBinaryName())
	startedPath := filepath.Join(dir, "inspection-started")
	releasePath := filepath.Join(dir, "release-inspection")
	script := `#!/bin/sh
case "$1" in
health)
  printf '%s\n' '{"schemaVersion":1,"ok":true,"helper":{"version":"0.1.0-test","platform":"test-platform"},"capabilities":{"renderImage":false,"renderVideoPoster":false,"inspectImage":false,"inspectVideo":true}}'
  exit 0
  ;;
inspect-video)
  : > ` + shellQuoteForTest(startedPath) + `
  while [ ! -f ` + shellQuoteForTest(releasePath) + ` ]; do sleep 0.01; done
  printf '%s\n' '{"schemaVersion":1,"ok":true,"operation":"inspect-video","backend":"ffprobe-cli","media":{"mediaType":"video","durationMs":` + fmt.Sprintf("%d", durationMS) + `}}'
  exit 0
  ;;
*)
  echo "unexpected command: $1" >&2
  exit 2
  ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write blocking video inspection helper: %v", err)
	}
	return path, startedPath, releasePath
}
