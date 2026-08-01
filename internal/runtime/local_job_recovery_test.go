package runtime

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/store"
)

func TestNewAgentRuntimeRecoversClaimedMetadataJobAfterRestart(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(family.jpg) error = %v", err)
	}
	cfg := config.ResolvedConfig{Config: config.Default()}
	cfg.AgentName = "restart-test-agent"
	cfg.DataDir = dataDir
	cfg.ConfigSource = "test"
	cfg.ConfigPath = filepath.Join(dataDir, "agent.json")
	cfg.Datasources = []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}
	cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
		Key:  "nas-photos",
		Path: rootPath,
	}}
	cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(0)
	loadedState, err := store.LoadOrCreate(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}

	first, err := NewAgentRuntime(BuildInfo{}, cfg, loadedState, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewAgentRuntime(first) error = %v", err)
	}
	if _, err := first.catalogService().RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		first.Close()
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}

	dbPath := filepath.Join(dataDir, "catalog-state-v1", "catalog.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		first.Close()
		t.Fatalf("open catalog DB: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE local_scan_jobs
		SET status = 'failed',
			completed_at = '2026-07-16T01:00:00Z',
			last_error = 'transient metadata failure'
		WHERE job_kind = 'metadata'`); err != nil {
		db.Close()
		first.Close()
		t.Fatalf("mark metadata failed: %v", err)
	}
	requeue, err := first.RequeueFailedLocalDatasourceMetadata(context.Background())
	if err != nil {
		db.Close()
		first.Close()
		t.Fatalf("RequeueFailedLocalDatasourceMetadata() error = %v", err)
	}
	if requeue.Queued != 1 {
		db.Close()
		first.Close()
		t.Fatalf("RequeueFailedLocalDatasourceMetadata() = %+v, want one queued job", requeue)
	}
	result, err := db.ExecContext(context.Background(), `UPDATE local_scan_jobs
		SET status = 'running',
			attempts = attempts + 1,
			locked_at = '2026-07-16T01:01:00Z'
		WHERE job_kind = 'metadata'
			AND status = 'queued'`)
	if err != nil {
		db.Close()
		first.Close()
		t.Fatalf("claim requeued metadata job: %v", err)
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		db.Close()
		first.Close()
		t.Fatalf("read claimed metadata rows: %v", err)
	}
	if claimed != 1 {
		db.Close()
		first.Close()
		t.Fatalf("claimed metadata rows = %d, want 1", claimed)
	}
	if err := db.Close(); err != nil {
		first.Close()
		t.Fatalf("close catalog DB: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	second, err := NewAgentRuntime(BuildInfo{}, cfg, loadedState, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewAgentRuntime(second) error = %v", err)
	}
	defer second.Close()

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen catalog DB: %v", err)
	}
	var (
		recoveredStatus   string
		recoveredPriority int
		recoveredLockedAt sql.NullString
	)
	if err := db.QueryRowContext(context.Background(), `SELECT status, priority, locked_at
		FROM local_scan_jobs
		WHERE job_kind = 'metadata'`).
		Scan(&recoveredStatus, &recoveredPriority, &recoveredLockedAt); err != nil {
		db.Close()
		t.Fatalf("read recovered metadata job: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close recovered catalog DB: %v", err)
	}
	if recoveredStatus != "queued" || recoveredPriority != 0 || recoveredLockedAt.Valid {
		t.Fatalf(
			"recovered metadata job = status %q priority %d locked %v, want queued repair priority without lock",
			recoveredStatus,
			recoveredPriority,
			recoveredLockedAt,
		)
	}
	statuses, err := second.catalogService().LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses() error = %v", err)
	}
	if len(statuses) != 1 ||
		statuses[0].QueuedMetadataJobs != 1 ||
		statuses[0].RunningMetadataJobs != 0 ||
		statuses[0].FailedMetadataJobs != 0 {
		t.Fatalf("status after restart = %+v, want recovered queued metadata", statuses)
	}
	batch, err := second.catalogService().RunLocalMetadataBatch(context.Background(), 1)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	if batch.ProcessedJobs != 1 || batch.RegisteredAssets != 1 || batch.FailedJobs != 0 {
		t.Fatalf("metadata batch after restart = %+v, want recovered job processed", batch)
	}
}

func TestUpdateWorkerRuntimePauseLetsActiveThumbnailJobComplete(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeRuntimeJPEGForTest(t, 96, 64), 0o644); err != nil {
		t.Fatalf("WriteFile(family.jpg) error = %v", err)
	}
	helperPath, helperStartedPath, helperReleasePath := writeCancelableRuntimeMediaHelperScript(t)
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.MediaRuntime.HelperPath = helperPath
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	})
	fileConfig := config.Default()
	fileConfig.AgentName = "file-agent"
	fileConfig.DataDir = runtime.ConfigResponse().DataDir
	fileConfig.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	if err := config.WriteFile(runtime.ConfigResponse().ConfigPath, fileConfig); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	catalogService := runtime.catalogService()
	if _, err := catalogService.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := catalogService.RunLocalMetadataBatch(context.Background(), 1); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() = %+v, %v, want one registered asset", result, err)
	}
	if !runtime.StartBackgroundWorkerScheduler() {
		t.Fatal("StartBackgroundWorkerScheduler() = false, want scheduler started")
	}

	waitForRuntimeTestFile(t, helperStartedPath, "media helper start")
	waitForLocalThumbnailStatus(t, catalogService, func(status catalog.LocalDatasourceScanStatus) bool {
		return status.RunningThumbnailJobs == 1
	}, "one running thumbnail job")

	if _, err := runtime.UpdateWorkerRuntime(config.WorkerRuntimeConfig{HeavyTaskWorkers: runtimeTestIntPtr(0)}); err != nil {
		t.Fatalf("UpdateWorkerRuntime(pause) error = %v", err)
	}
	waitForLocalThumbnailStatus(t, catalogService, func(status catalog.LocalDatasourceScanStatus) bool {
		return status.PendingThumbnailJobs == 1 &&
			status.QueuedThumbnailJobs == 0 &&
			status.RunningThumbnailJobs == 1 &&
			status.FailedThumbnailJobs == 0
	}, "active thumbnail remains running after pause")

	if err := os.WriteFile(helperReleasePath, []byte("released"), 0o600); err != nil {
		t.Fatalf("WriteFile(helper release) error = %v", err)
	}
	waitForLocalThumbnailStatus(t, catalogService, func(status catalog.LocalDatasourceScanStatus) bool {
		return status.PendingThumbnailJobs == 0 &&
			status.QueuedThumbnailJobs == 0 &&
			status.RunningThumbnailJobs == 0 &&
			status.FailedThumbnailJobs == 0
	}, "active thumbnail completes while new assignments remain paused")
}

func waitForRuntimeTestFile(t *testing.T, path string, label string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}

func waitForLocalThumbnailStatus(
	t *testing.T,
	catalogService *catalog.Service,
	matches func(catalog.LocalDatasourceScanStatus) bool,
	label string,
) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	var last []catalog.LocalDatasourceScanStatus
	for time.Now().Before(deadline) {
		statuses, err := catalogService.LocalDatasourceScanStatuses(context.Background())
		if err != nil {
			t.Fatalf("LocalDatasourceScanStatuses() error = %v", err)
		}
		last = statuses
		if len(statuses) == 1 && matches(statuses[0]) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; last status = %+v", label, last)
}

func writeCancelableRuntimeMediaHelperScript(t *testing.T) (string, string, string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "timich-media-helper")
	startedPath := filepath.Join(dir, "started")
	releasePath := filepath.Join(dir, "released")
	script := `#!/bin/sh
case "$1" in
health)
  printf '%s\n' '{"schemaVersion":1,"ok":true,"helper":{"version":"0.1.0-test","platform":"test-platform"},"capabilities":{"renderImage":true,"renderVideoPoster":false,"inspectImage":false,"inspectVideo":false}}'
  exit 0
  ;;
render-image)
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
    echo "missing image input or output" >&2
    exit 8
  fi
  if [ ! -f ` + shellQuoteRuntimeTest(releasePath) + ` ]; then
    printf '%s\n' started > ` + shellQuoteRuntimeTest(startedPath) + `
    while [ ! -f ` + shellQuoteRuntimeTest(releasePath) + ` ]; do
      sleep 0.05
    done
  fi
  cp "$input" "$output"
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
		t.Fatalf("write cancelable media helper script: %v", err)
	}
	return path, startedPath, releasePath
}
