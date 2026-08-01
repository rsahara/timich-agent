package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestResetRunningLocalScanJobsRequeuesMetadataAndThumbnails(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "settling.jpg"), []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1h"},
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `UPDATE local_scan_jobs
		SET status = 'running',
			attempts = 2,
			locked_at = '2026-07-16T01:00:00Z',
			completed_at = '2026-07-16T01:01:00Z',
			last_error = 'old metadata error'
		WHERE job_kind = ?`, localMetadataJobKind); err != nil {
		t.Fatalf("mark metadata running: %v", err)
	}
	if _, err := service.catalog.db.ExecContext(context.Background(), `INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, asset_id, status, attempts,
			scheduled_at, sort_at, locked_at, completed_at, last_error
		) VALUES (?, ?, ?, 'asset-1', 'running', 3, ?, 'old-sort', ?, ?, ?)`,
		"1111111111111111",
		localThumbnailJobKind,
		localThumbnailRepairPriority,
		"2026-07-16T01:00:00Z",
		"2026-07-16T01:00:00Z",
		"2026-07-16T01:01:00Z",
		"old thumbnail error",
	); err != nil {
		t.Fatalf("insert running thumbnail job: %v", err)
	}

	before := formatCatalogTime(time.Now().UTC())
	recovered, err := service.ResetRunningLocalScanJobs(context.Background())
	after := formatCatalogTime(time.Now().UTC())
	if err != nil {
		t.Fatalf("ResetRunningLocalScanJobs() error = %v", err)
	}
	if recovered != 2 {
		t.Fatalf("ResetRunningLocalScanJobs() = %d, want 2", recovered)
	}

	var (
		metadataStatus       string
		metadataPriority     int
		metadataAttempts     int
		metadataScheduledAt  string
		metadataSortAt       string
		metadataLockedAt     sql.NullString
		metadataCompletedAt  sql.NullString
		metadataLastError    sql.NullString
		metadataNotBefore    string
		metadataLocationTime string
	)
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT
			j.status, j.priority, j.attempts, j.scheduled_at, j.sort_at,
			j.locked_at, j.completed_at, j.last_error,
			l.metadata_not_before, l.mtime
		FROM local_scan_jobs j
		JOIN local_asset_locations l ON l.id = j.location_id
		WHERE j.job_kind = ?`, localMetadataJobKind).
		Scan(
			&metadataStatus,
			&metadataPriority,
			&metadataAttempts,
			&metadataScheduledAt,
			&metadataSortAt,
			&metadataLockedAt,
			&metadataCompletedAt,
			&metadataLastError,
			&metadataNotBefore,
			&metadataLocationTime,
		); err != nil {
		t.Fatalf("read recovered metadata job: %v", err)
	}
	if metadataStatus != "queued" ||
		metadataPriority != localMetadataBackgroundPriority ||
		metadataAttempts != 2 ||
		metadataScheduledAt != metadataNotBefore ||
		metadataSortAt != metadataLocationTime ||
		metadataLockedAt.Valid ||
		metadataCompletedAt.Valid ||
		metadataLastError.Valid {
		t.Fatalf(
			"recovered metadata = status %q priority %d attempts %d scheduled %q deadline %q sort %q mtime %q locked %v completed %v error %v",
			metadataStatus,
			metadataPriority,
			metadataAttempts,
			metadataScheduledAt,
			metadataNotBefore,
			metadataSortAt,
			metadataLocationTime,
			metadataLockedAt,
			metadataCompletedAt,
			metadataLastError,
		)
	}

	var (
		thumbnailStatus      string
		thumbnailPriority    int
		thumbnailAttempts    int
		thumbnailScheduledAt string
		thumbnailSortAt      string
		thumbnailLockedAt    sql.NullString
		thumbnailCompletedAt sql.NullString
		thumbnailLastError   sql.NullString
	)
	if err := service.catalog.db.QueryRowContext(context.Background(), `SELECT
			status, priority, attempts, scheduled_at, sort_at,
			locked_at, completed_at, last_error
		FROM local_scan_jobs
		WHERE job_kind = ?`, localThumbnailJobKind).
		Scan(
			&thumbnailStatus,
			&thumbnailPriority,
			&thumbnailAttempts,
			&thumbnailScheduledAt,
			&thumbnailSortAt,
			&thumbnailLockedAt,
			&thumbnailCompletedAt,
			&thumbnailLastError,
		); err != nil {
		t.Fatalf("read recovered thumbnail job: %v", err)
	}
	if thumbnailStatus != "queued" ||
		thumbnailPriority != localThumbnailRepairPriority ||
		thumbnailAttempts != 3 ||
		thumbnailScheduledAt < before ||
		thumbnailScheduledAt > after ||
		thumbnailSortAt != "old-sort" ||
		thumbnailLockedAt.Valid ||
		thumbnailCompletedAt.Valid ||
		thumbnailLastError.Valid {
		t.Fatalf(
			"recovered thumbnail = status %q priority %d attempts %d scheduled %q range [%q,%q] sort %q locked %v completed %v error %v",
			thumbnailStatus,
			thumbnailPriority,
			thumbnailAttempts,
			thumbnailScheduledAt,
			before,
			after,
			thumbnailSortAt,
			thumbnailLockedAt,
			thumbnailCompletedAt,
			thumbnailLastError,
		)
	}
	jobs, err := service.nextLocalMetadataJobs(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("nextLocalMetadataJobs() error = %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("nextLocalMetadataJobs() = %+v, want recovered metadata to keep settling", jobs)
	}
}

func TestRootRevalidationFailureDefersOnlyClaimedLocalJobs(t *testing.T) {
	t.Parallel()

	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeJPEGForTest(t, 800, 600), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	helperPath, _ := writeFakeMediaHelperImageScript(t)
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
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
	defer service.Close()
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v", result, err)
	}
	var (
		locationID int64
		assetID    string
		mtime      string
	)
	if err := service.catalog.db.QueryRow(`SELECT id, asset_id, mtime
		FROM local_asset_locations
		WHERE source_key = ? AND status = 'active'`,
		"1111111111111111",
	).Scan(&locationID, &assetID, &mtime); err != nil {
		t.Fatalf("read active location: %v", err)
	}
	past := formatCatalogTime(time.Now().UTC().Add(-time.Minute))
	metadataResult, err := service.catalog.db.Exec(`INSERT INTO local_scan_jobs (
			source_key, job_kind, priority, root_key, root_generation, location_id,
			status, scheduled_at, sort_at
		) VALUES (?, ?, ?, ?, 1, ?, 'queued', ?, ?)`,
		"1111111111111111",
		localMetadataJobKind,
		localMetadataBackgroundPriority,
		"nas-photos",
		locationID,
		past,
		mtime,
	)
	if err != nil {
		t.Fatalf("insert metadata retry job: %v", err)
	}
	metadataJobID, err := metadataResult.LastInsertId()
	if err != nil {
		t.Fatalf("read metadata retry job ID: %v", err)
	}
	if err := service.queuePendingLocalThumbnailJobsForSource(context.Background(), "1111111111111111", 10, 10); err != nil {
		t.Fatalf("queuePendingLocalThumbnailJobsForSource() error = %v", err)
	}
	metadataJobs, err := service.nextLocalMetadataJobs(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("nextLocalMetadataJobs() error = %v", err)
	}
	if len(metadataJobs) != 1 || metadataJobs[0].ID != metadataJobID {
		t.Fatalf("metadata jobs = %+v, want inserted job %d", metadataJobs, metadataJobID)
	}
	thumbnailJobs, err := service.nextLocalThumbnailJobs(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("nextLocalThumbnailJobs() error = %v", err)
	}
	if len(thumbnailJobs) != 1 || thumbnailJobs[0].AssetID != assetID {
		t.Fatalf("thumbnail jobs = %+v, want asset %s", thumbnailJobs, assetID)
	}
	metadataJob := metadataJobs[0]
	thumbnailJob := thumbnailJobs[0]
	nowText := formatCatalogTime(time.Now().UTC())
	trustedRoot, err := service.acquireTrustedLocalMediaRoot(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("acquireTrustedLocalMediaRoot(metadata) error = %v", err)
	}
	claimed, err := service.claimLocalMetadataJob(context.Background(), metadataJob, trustedRoot, nowText)
	_ = trustedRoot.Close()
	if err != nil || !claimed {
		t.Fatalf("claimLocalMetadataJob() = %t, %v", claimed, err)
	}
	trustedRoot, err = service.acquireTrustedLocalMediaRoot(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("acquireTrustedLocalMediaRoot(thumbnail) error = %v", err)
	}
	claimed, err = service.claimLocalThumbnailJob(context.Background(), thumbnailJob, trustedRoot, nowText)
	_ = trustedRoot.Close()
	if err != nil || !claimed {
		t.Fatalf("claimLocalThumbnailJob() = %t, %v", claimed, err)
	}

	notifications := 0
	service.SetLocalWorkNotifier(func() { notifications++ })
	trustedPath := filepath.Join(parentPath, "photos-trusted")
	if err := os.Rename(rootPath, trustedPath); err != nil {
		t.Fatalf("Rename(trusted root) error = %v", err)
	}
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement root) error = %v", err)
	}
	if deferred, err := service.failLocalMetadataJob(context.Background(), metadataJob, errors.New("metadata source unavailable")); err != nil || !deferred {
		t.Fatalf("failLocalMetadataJob() deferred=%t error=%v", deferred, err)
	}
	if deferred, err := service.failLocalThumbnailJob(context.Background(), thumbnailJob, errors.New("thumbnail source unavailable")); err != nil || !deferred {
		t.Fatalf("failLocalThumbnailJob() deferred=%t error=%v", deferred, err)
	}
	if notifications != 2 {
		t.Fatalf("local work notifications = %d, want 2", notifications)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE id IN (`+fmt.Sprint(metadataJob.ID)+`, `+fmt.Sprint(thumbnailJob.ID)+`)
			AND status = 'queued' AND locked_at IS NULL AND completed_at IS NULL AND last_error IS NULL`, 2)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations
		WHERE id = `+fmt.Sprint(locationID)+` AND status = 'active' AND asset_id = '`+assetID+`'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets
		WHERE asset_id = '`+assetID+`' AND thumbnail_status = 'pending'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions
		WHERE asset_id = '`+assetID+`' AND status = 'failed'`, 0)

	replacementPath := filepath.Join(parentPath, "photos-replacement")
	if err := os.Rename(rootPath, replacementPath); err != nil {
		t.Fatalf("Rename(replacement root) error = %v", err)
	}
	if err := os.Rename(trustedPath, rootPath); err != nil {
		t.Fatalf("Rename(trusted root back) error = %v", err)
	}
	metadataJobs, err = service.nextLocalMetadataJobs(context.Background(), "", 10)
	if err != nil || len(metadataJobs) != 1 || metadataJobs[0].ID != metadataJob.ID {
		t.Fatalf("nextLocalMetadataJobs(restored) = %+v, %v", metadataJobs, err)
	}
	thumbnailJobs, err = service.nextLocalThumbnailJobs(context.Background(), "", 10)
	if err != nil || len(thumbnailJobs) != 1 || thumbnailJobs[0].ID != thumbnailJob.ID {
		t.Fatalf("nextLocalThumbnailJobs(restored) = %+v, %v", thumbnailJobs, err)
	}
}

func TestMetadataFailurePublicationErrorDefersExactClaim(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeJPEGForTest(t, 320, 240), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.catalog.db.Exec(`CREATE TRIGGER test_fail_metadata_registration
		BEFORE INSERT ON local_assets
		BEGIN
			SELECT RAISE(FAIL, 'forced metadata registration failure');
		END`); err != nil {
		t.Fatalf("create metadata registration failure trigger: %v", err)
	}
	if _, err := service.catalog.db.Exec(`CREATE TRIGGER test_fail_metadata_publication
		BEFORE UPDATE OF status ON local_scan_jobs
		WHEN NEW.job_kind = 'metadata' AND NEW.status = 'failed'
		BEGIN
			SELECT RAISE(FAIL, 'forced metadata failure publication error');
		END`); err != nil {
		t.Fatalf("create metadata publication failure trigger: %v", err)
	}

	result, err := service.RunLocalMetadataBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch() error = %v", err)
	}
	if result.ProcessedJobs != 1 || result.DeferredJobs != 1 || result.FailedJobs != 0 {
		t.Fatalf("RunLocalMetadataBatch() result = %+v, want one deferred exact claim", result)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE job_kind = 'metadata' AND status = 'queued'
			AND locked_at IS NULL AND completed_at IS NULL AND last_error IS NULL`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets`, 0)
}

func TestThumbnailFailurePublicationErrorDefersExactClaim(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeJPEGForTest(t, 320, 240), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	helperPath, _ := writeFakeMediaHelperImageScript(t)
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
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
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v", result, err)
	}
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("replace media helper: %v", err)
	}
	if _, err := service.catalog.db.Exec(`CREATE TRIGGER test_fail_thumbnail_publication
		BEFORE UPDATE OF status ON local_scan_jobs
		WHEN NEW.job_kind = 'thumbnail' AND NEW.status = 'failed'
		BEGIN
			SELECT RAISE(FAIL, 'forced thumbnail failure publication error');
		END`); err != nil {
		t.Fatalf("create thumbnail publication failure trigger: %v", err)
	}

	result, err := service.RunLocalThumbnailBatch(context.Background(), 10)
	if err != nil {
		t.Fatalf("RunLocalThumbnailBatch() error = %v", err)
	}
	if result.ProcessedJobs != 1 || result.DeferredJobs != 1 || result.FailedJobs != 0 {
		t.Fatalf("RunLocalThumbnailBatch() result = %+v, want one deferred exact claim", result)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE job_kind = 'thumbnail' AND status = 'queued'
			AND locked_at IS NULL AND completed_at IS NULL AND last_error IS NULL`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_assets
		WHERE thumbnail_status = 'pending'`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_renditions
		WHERE status = 'failed'`, 0)
}

func TestMetadataClaimRecoveryRetriesAfterDatabaseRecovers(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeJPEGForTest(t, 320, 240), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, ServiceOptions{
		DataDir: t.TempDir(),
		LocalRoots: []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}},
	})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if _, err := service.catalog.db.Exec(`CREATE TRIGGER test_fail_metadata_registration_until_recovery
		BEFORE INSERT ON local_assets
		BEGIN
			SELECT RAISE(FAIL, 'forced metadata registration failure');
		END`); err != nil {
		t.Fatalf("create metadata registration failure trigger: %v", err)
	}
	if _, err := service.catalog.db.Exec(`CREATE TRIGGER test_block_metadata_claim_finalization
		BEFORE UPDATE OF status ON local_scan_jobs
		WHEN OLD.status = 'running'
			AND NEW.job_kind = 'metadata'
			AND NEW.status IN ('failed', 'queued')
		BEGIN
			SELECT RAISE(FAIL, 'forced metadata claim finalization failure');
		END`); err != nil {
		t.Fatalf("create metadata claim finalization failure trigger: %v", err)
	}

	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err == nil {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=nil, want recovery error", result)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE job_kind = 'metadata' AND status = 'running'`, 1)

	if _, err := service.catalog.db.Exec(`DROP TRIGGER test_block_metadata_claim_finalization`); err != nil {
		t.Fatalf("drop metadata claim finalization trigger: %v", err)
	}
	state, err := service.LocalMetadataQueueState(context.Background())
	if err != nil {
		t.Fatalf("LocalMetadataQueueState() error = %v", err)
	}
	if state.Queued != 1 {
		t.Fatalf("LocalMetadataQueueState() = %+v, want recovered queued claim", state)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE job_kind = 'metadata' AND status = 'queued'
			AND locked_at IS NULL AND completed_at IS NULL AND last_error IS NULL`, 1)
}

func TestThumbnailClaimRecoveryRetriesAfterDatabaseRecovers(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeJPEGForTest(t, 320, 240), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	helperPath, _ := writeFakeMediaHelperImageScript(t)
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
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
	defer service.Close()

	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v", result, err)
	}
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("replace media helper: %v", err)
	}
	if _, err := service.catalog.db.Exec(`CREATE TRIGGER test_block_thumbnail_claim_finalization
		BEFORE UPDATE OF status ON local_scan_jobs
		WHEN OLD.status = 'running'
			AND NEW.job_kind = 'thumbnail'
			AND NEW.status IN ('failed', 'queued')
		BEGIN
			SELECT RAISE(FAIL, 'forced thumbnail claim finalization failure');
		END`); err != nil {
		t.Fatalf("create thumbnail claim finalization failure trigger: %v", err)
	}

	if result, err := service.RunLocalThumbnailBatch(context.Background(), 10); err == nil {
		t.Fatalf("RunLocalThumbnailBatch() result=%+v error=nil, want recovery error", result)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE job_kind = 'thumbnail' AND status = 'running'`, 1)

	if _, err := service.catalog.db.Exec(`DROP TRIGGER test_block_thumbnail_claim_finalization`); err != nil {
		t.Fatalf("drop thumbnail claim finalization trigger: %v", err)
	}
	pending, err := service.PendingLocalThumbnailJobs(context.Background())
	if err != nil {
		t.Fatalf("PendingLocalThumbnailJobs() error = %v", err)
	}
	if pending != 1 {
		t.Fatalf("PendingLocalThumbnailJobs() = %d, want one pending asset", pending)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE job_kind = 'thumbnail' AND status = 'queued'
			AND locked_at IS NULL AND completed_at IS NULL AND last_error IS NULL`, 1)
}
