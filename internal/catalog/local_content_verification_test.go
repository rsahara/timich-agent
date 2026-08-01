package catalog

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestLocalContentVerificationRunsAfterReconciliationWithoutChangingRegistrationTimestamp(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "family.jpg")
	if err := os.WriteFile(mediaPath, []byte("original-photo"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(initial) error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v, want one registered asset", result, err)
	}

	oldVerifiedAt := formatCatalogTime(time.Now().UTC().Add(-7 * 24 * time.Hour))
	if _, err := service.catalog.db.Exec(`UPDATE local_asset_locations
		SET content_verified_at = ?, content_verification_attempted_at = ?
		WHERE relative_path = 'family.jpg'`,
		oldVerifiedAt,
		oldVerifiedAt,
	); err != nil {
		t.Fatalf("age content verification timestamp: %v", err)
	}
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(second) error = %v", err)
	}
	var afterReconciliation string
	if err := service.catalog.db.QueryRow(`SELECT content_verified_at
		FROM local_asset_locations WHERE relative_path = 'family.jpg'`).Scan(&afterReconciliation); err != nil {
		t.Fatalf("read content verification timestamp after reconciliation: %v", err)
	}
	if afterReconciliation != oldVerifiedAt {
		t.Fatalf("content_verified_at after reconciliation = %q, want unchanged %q", afterReconciliation, oldVerifiedAt)
	}

	startLocalContentVerificationTestWindow(t, service, time.Minute)
	runnable, err := service.LocalContentVerificationRunnableSourceKeys(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("LocalContentVerificationRunnableSourceKeys(before run) error = %v", err)
	}
	if len(runnable) != 1 || runnable[0] != "1111111111111111" {
		t.Fatalf("runnable source keys before run = %#v, want local source", runnable)
	}
	statuses, err := service.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses(before run) error = %v", err)
	}
	if len(statuses) != 1 || statuses[0].ContentVerificationStatus != LocalContentVerificationStatusRunning {
		t.Fatalf("scan statuses before run = %+v, want running content verification", statuses)
	}
	result, err := service.RunLocalContentVerification(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalContentVerification() error = %v", err)
	}
	if result.ProcessedFiles != 1 || result.VerifiedFiles != 1 || result.ChangedFiles != 0 || result.FailedFiles != 0 {
		t.Fatalf("content verification result = %+v, want one unchanged verified file", result)
	}
	var contentVerifiedAt string
	var generalVerifiedAt sql.NullString
	if err := service.catalog.db.QueryRow(`SELECT content_verified_at, verified_at
		FROM local_asset_locations WHERE relative_path = 'family.jpg'`).Scan(&contentVerifiedAt, &generalVerifiedAt); err != nil {
		t.Fatalf("read verification timestamps: %v", err)
	}
	if contentVerifiedAt == oldVerifiedAt {
		t.Fatalf("content_verified_at = %q, want refreshed timestamp", contentVerifiedAt)
	}
	if !generalVerifiedAt.Valid {
		t.Fatal("verified_at is NULL, want registration verification retained")
	}
	runnable, err = service.LocalContentVerificationRunnableSourceKeys(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("LocalContentVerificationRunnableSourceKeys(after run) error = %v", err)
	}
	if len(runnable) != 0 {
		t.Fatalf("runnable source keys after run = %#v, want none", runnable)
	}
	statuses, err = service.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses(after run) error = %v", err)
	}
	if len(statuses) != 1 ||
		statuses[0].ContentVerificationStatus != LocalContentVerificationStatusCompleted ||
		statuses[0].ContentVerificationProcessedFiles != 1 ||
		statuses[0].ContentVerificationVerifiedFiles != 1 {
		t.Fatalf("scan statuses after run = %+v, want content verification complete", statuses)
	}
}

func TestLocalContentVerificationSelectsOldestLocationFirst(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	for name, contents := range map[string]string{
		"older.jpg": "older-photo",
		"newer.jpg": "newer-photo",
	} {
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 2 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v, want two registered assets", result, err)
	}
	olderAt := formatCatalogTime(time.Now().UTC().Add(-14 * 24 * time.Hour))
	newerAt := formatCatalogTime(time.Now().UTC().Add(-7 * 24 * time.Hour))
	if _, err := service.catalog.db.Exec(`UPDATE local_asset_locations
		SET content_verified_at = CASE relative_path
				WHEN 'older.jpg' THEN ?
				ELSE ?
			END,
			content_verification_attempted_at = CASE relative_path
				WHEN 'older.jpg' THEN ?
				ELSE ?
			END`,
		olderAt,
		newerAt,
		olderAt,
		newerAt,
	); err != nil {
		t.Fatalf("set content verification order: %v", err)
	}

	outcome, err := service.verifyNextLocalContent(
		context.Background(),
		"1111111111111111",
		formatCatalogTime(time.Now().UTC()),
	)
	if err != nil {
		t.Fatalf("verifyNextLocalContent() error = %v", err)
	}
	if !outcome.found || !outcome.verified || outcome.changed || outcome.failed {
		t.Fatalf("verification outcome = %+v, want oldest file verified", outcome)
	}
	var olderAfter string
	var newerAfter string
	if err := service.catalog.db.QueryRow(`SELECT
			MAX(CASE WHEN relative_path = 'older.jpg' THEN content_verified_at END),
			MAX(CASE WHEN relative_path = 'newer.jpg' THEN content_verified_at END)
		FROM local_asset_locations`).Scan(&olderAfter, &newerAfter); err != nil {
		t.Fatalf("read ordered verification timestamps: %v", err)
	}
	if olderAfter == olderAt {
		t.Fatalf("older content_verified_at = %q, want refreshed timestamp", olderAfter)
	}
	if newerAfter != newerAt {
		t.Fatalf("newer content_verified_at = %q, want unchanged %q", newerAfter, newerAt)
	}
}

func TestLocalContentVerificationSkipRecordsOneDailyResultWithoutQueue(t *testing.T) {
	t.Parallel()

	service := newLocalPhase0TestService(t, t.TempDir())
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	scheduledAt := time.Now().UTC()
	recorded, err := service.SkipLocalContentVerificationWindow(
		context.Background(),
		"1111111111111111",
		scheduledAt,
		LocalContentVerificationSkipNoIdleWorker,
	)
	if err != nil {
		t.Fatalf("SkipLocalContentVerificationWindow() error = %v", err)
	}
	if !recorded {
		t.Fatal("SkipLocalContentVerificationWindow() = false, want first occurrence recorded")
	}
	recorded, err = service.SkipLocalContentVerificationWindow(
		context.Background(),
		"1111111111111111",
		scheduledAt,
		LocalContentVerificationSkipNoIdleWorker,
	)
	if err != nil {
		t.Fatalf("SkipLocalContentVerificationWindow(repeat) error = %v", err)
	}
	if recorded {
		t.Fatal("SkipLocalContentVerificationWindow(repeat) = true, want idempotent daily result")
	}
	statuses, err := service.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses() error = %v", err)
	}
	if len(statuses) != 1 ||
		statuses[0].ContentVerificationStatus != LocalContentVerificationStatusSkipped ||
		statuses[0].ContentVerificationSkipReason != LocalContentVerificationSkipNoIdleWorker ||
		statuses[0].ContentVerificationProcessedFiles != 0 {
		t.Fatalf("content verification skip status = %+v", statuses)
	}
	runnable, err := service.LocalContentVerificationRunnableSourceKeys(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("LocalContentVerificationRunnableSourceKeys() error = %v", err)
	}
	if len(runnable) != 0 {
		t.Fatalf("runnable source keys = %#v, want no accumulated work after skip", runnable)
	}
}

type localContentVerificationWindowStateForTest struct {
	ScheduledAt string
	StartedAt   string
	DeadlineAt  string
	Status      string
	SkipReason  sql.NullString
	Processed   int
	Verified    int
	Changed     int
	Failed      int
	ReadBytes   int64
}

func readLocalContentVerificationWindowStateForTest(t *testing.T, service *Service) localContentVerificationWindowStateForTest {
	t.Helper()
	var state localContentVerificationWindowStateForTest
	if err := service.catalog.db.QueryRow(`SELECT content_verification_scheduled_at,
			content_verification_window_started_at,
			content_verification_window_deadline_at,
			content_verification_status,
			content_verification_skip_reason,
			content_verification_processed_files,
			content_verification_verified_files,
			content_verification_changed_files,
			content_verification_failed_files,
			content_verification_read_bytes
		FROM local_scan_root_state
		WHERE source_key = '1111111111111111' AND root_key = 'nas-photos'`).Scan(
		&state.ScheduledAt,
		&state.StartedAt,
		&state.DeadlineAt,
		&state.Status,
		&state.SkipReason,
		&state.Processed,
		&state.Verified,
		&state.Changed,
		&state.Failed,
		&state.ReadBytes,
	); err != nil {
		t.Fatalf("read content verification window: %v", err)
	}
	return state
}

func TestLocalContentVerificationRunningWindowRejectsLaterOccurrence(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		attempt func(*Service, time.Time) (bool, error)
	}{
		{
			name: "start",
			attempt: func(service *Service, scheduledAt time.Time) (bool, error) {
				return service.StartLocalContentVerificationWindow(
					context.Background(),
					"1111111111111111",
					scheduledAt,
					scheduledAt.Add(48*time.Hour),
				)
			},
		},
		{
			name: "skip",
			attempt: func(service *Service, scheduledAt time.Time) (bool, error) {
				return service.SkipLocalContentVerificationWindow(
					context.Background(),
					"1111111111111111",
					scheduledAt,
					LocalContentVerificationSkipNoIdleWorker,
				)
			},
		},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			service := newLocalPhase0TestService(t, t.TempDir())
			if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
				t.Fatalf("RunLocalReconciliationScan() error = %v", err)
			}
			firstScheduledAt := time.Date(2026, 7, 30, 4, 0, 0, 0, time.UTC)
			started, err := service.StartLocalContentVerificationWindow(
				context.Background(),
				"1111111111111111",
				firstScheduledAt,
				firstScheduledAt.Add(48*time.Hour),
			)
			if err != nil {
				t.Fatalf("StartLocalContentVerificationWindow(first) error = %v", err)
			}
			if !started {
				t.Fatal("StartLocalContentVerificationWindow(first) = false, want running window")
			}
			if _, err := service.catalog.db.Exec(`UPDATE local_scan_root_state
				SET content_verification_processed_files = 7,
					content_verification_verified_files = 5,
					content_verification_changed_files = 1,
					content_verification_failed_files = 1,
					content_verification_read_bytes = 1234
				WHERE source_key = '1111111111111111' AND root_key = 'nas-photos'`); err != nil {
				t.Fatalf("seed running window progress: %v", err)
			}

			before := readLocalContentVerificationWindowStateForTest(t, service)

			changed, err := testCase.attempt(service, firstScheduledAt.Add(24*time.Hour))
			if err != nil {
				t.Fatalf("%s later occurrence error = %v", testCase.name, err)
			}
			if changed {
				t.Fatalf("%s later occurrence = true, want running window retained", testCase.name)
			}

			after := readLocalContentVerificationWindowStateForTest(t, service)
			if after != before {
				t.Fatalf("running window changed after %s: before=%+v after=%+v", testCase.name, before, after)
			}
		})
	}
}

func TestLocalContentVerificationLaterOccurrenceDoesNotReplaceActiveHash(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "family.jpg")
	if err := os.WriteFile(mediaPath, []byte("original-photo"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v, want one registered asset", result, err)
	}
	oldAttempt := formatCatalogTime(time.Now().UTC().Add(-7 * 24 * time.Hour))
	if _, err := service.catalog.db.Exec(`UPDATE local_asset_locations
		SET content_verified_at = ?, content_verification_attempted_at = ?
		WHERE relative_path = 'family.jpg'`,
		oldAttempt,
		oldAttempt,
	); err != nil {
		t.Fatalf("age content verification timestamp: %v", err)
	}

	firstScheduledAt := time.Now().UTC().Add(-24 * time.Hour)
	started, err := service.StartLocalContentVerificationWindow(
		context.Background(),
		"1111111111111111",
		firstScheduledAt,
		firstScheduledAt.Add(48*time.Hour),
	)
	if err != nil {
		t.Fatalf("StartLocalContentVerificationWindow() error = %v", err)
	}
	if !started {
		t.Fatal("StartLocalContentVerificationWindow() = false, want running window")
	}

	hashStarted := make(chan struct{})
	releaseHash := make(chan struct{}, 1)
	t.Cleanup(func() {
		close(releaseHash)
	})
	service.localContentVerificationHash = func(ctx context.Context, file *os.File) (string, int64, os.FileInfo, error) {
		close(hashStarted)
		<-releaseHash
		return sha1OpenFile(ctx, file)
	}
	type runResult struct {
		result LocalContentVerificationResult
		err    error
	}
	resultC := make(chan runResult, 1)
	go func() {
		result, runErr := service.RunLocalContentVerification(context.Background(), "1111111111111111")
		resultC <- runResult{result: result, err: runErr}
	}()

	select {
	case <-hashStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("content verification hash did not start")
	}
	skipped, err := service.SkipLocalContentVerificationWindow(
		context.Background(),
		"1111111111111111",
		firstScheduledAt.Add(24*time.Hour),
		LocalContentVerificationSkipNoIdleWorker,
	)
	if err != nil {
		t.Fatalf("SkipLocalContentVerificationWindow(next day) error = %v", err)
	}
	if skipped {
		t.Fatal("SkipLocalContentVerificationWindow(next day) = true, want active hash retained")
	}
	releaseHash <- struct{}{}

	var run runResult
	select {
	case run = <-resultC:
	case <-time.After(5 * time.Second):
		t.Fatal("content verification hash did not finish")
	}
	if run.err != nil {
		t.Fatalf("RunLocalContentVerification() error = %v", run.err)
	}
	if run.result.ProcessedFiles != 1 ||
		run.result.VerifiedFiles != 1 ||
		run.result.WindowComplete != true {
		t.Fatalf("content verification result = %+v, want old window completed with one verified file", run.result)
	}

	var scheduledAt string
	var status string
	var reason sql.NullString
	var processed int
	var verified int
	if err := service.catalog.db.QueryRow(`SELECT content_verification_scheduled_at,
			content_verification_status,
			content_verification_skip_reason,
			content_verification_processed_files,
			content_verification_verified_files
		FROM local_scan_root_state
		WHERE source_key = '1111111111111111' AND root_key = 'nas-photos'`).Scan(
		&scheduledAt,
		&status,
		&reason,
		&processed,
		&verified,
	); err != nil {
		t.Fatalf("read completed window: %v", err)
	}
	if scheduledAt != formatCatalogTime(firstScheduledAt) ||
		status != LocalContentVerificationStatusCompleted ||
		reason.Valid ||
		processed != 1 ||
		verified != 1 {
		t.Fatalf(
			"completed window = scheduled %q status %q reason %+v processed %d verified %d, want old occurrence completed",
			scheduledAt,
			status,
			reason,
			processed,
			verified,
		)
	}
}

func TestLocalContentVerificationZeroDurationSuppressesPersistedWindow(t *testing.T) {
	t.Parallel()

	service := newLocalPhase0TestService(t, t.TempDir())
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	startLocalContentVerificationTestWindow(t, service, time.Minute)
	service.ReconfigureDatasources([]config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			ContentVerificationDuration: "0",
		},
	}})
	runnable, err := service.LocalContentVerificationRunnableSourceKeys(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("LocalContentVerificationRunnableSourceKeys() error = %v", err)
	}
	if len(runnable) != 0 {
		t.Fatalf("runnable source keys = %#v, want explicit zero duration to disable persisted work", runnable)
	}
}

func TestLocalContentVerificationProcessesOneLocationPerDurableWindowSlice(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	for name, contents := range map[string]string{
		"first.jpg":  "first-photo",
		"second.jpg": "second-photo",
	} {
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(initial) error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 2 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v, want two registered assets", result, err)
	}
	oldestAt := formatCatalogTime(time.Now().UTC().Add(-14 * 24 * time.Hour))
	olderAt := formatCatalogTime(time.Now().UTC().Add(-7 * 24 * time.Hour))
	if _, err := service.catalog.db.Exec(`UPDATE local_asset_locations
		SET content_verified_at = CASE relative_path
				WHEN 'first.jpg' THEN ?
				ELSE ?
			END,
			content_verification_attempted_at = CASE relative_path
				WHEN 'first.jpg' THEN ?
				ELSE ?
			END`,
		oldestAt,
		olderAt,
		oldestAt,
		olderAt,
	); err != nil {
		t.Fatalf("age content verification timestamps: %v", err)
	}
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(second) error = %v", err)
	}

	startLocalContentVerificationTestWindow(t, service, time.Minute)
	firstResult, err := service.RunLocalContentVerification(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalContentVerification(first slice) error = %v", err)
	}
	if firstResult.ProcessedFiles != 1 || firstResult.VerifiedFiles != 1 || firstResult.WindowComplete {
		t.Fatalf("first verification slice = %+v, want one file and an open window", firstResult)
	}
	var windowStarted sql.NullString
	if err := service.catalog.db.QueryRow(`SELECT content_verification_window_started_at
		FROM local_scan_root_state
		WHERE source_key = '1111111111111111' AND root_key = 'nas-photos'`).Scan(&windowStarted); err != nil {
		t.Fatalf("read open verification window: %v", err)
	}
	if !windowStarted.Valid {
		t.Fatal("content_verification_window_started_at is NULL after first slice")
	}

	secondResult, err := service.RunLocalContentVerification(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalContentVerification(second slice) error = %v", err)
	}
	if secondResult.ProcessedFiles != 1 || secondResult.VerifiedFiles != 1 || !secondResult.WindowComplete {
		t.Fatalf("second verification slice = %+v, want final file and completed window", secondResult)
	}
	if err := service.catalog.db.QueryRow(`SELECT content_verification_window_started_at
		FROM local_scan_root_state
		WHERE source_key = '1111111111111111' AND root_key = 'nas-photos'`).Scan(&windowStarted); err != nil {
		t.Fatalf("read completed verification window: %v", err)
	}
	if windowStarted.Valid {
		t.Fatalf("content_verification_window_started_at = %q, want NULL after final slice", windowStarted.String)
	}
	var refreshed int
	if err := service.catalog.db.QueryRow(`SELECT COUNT(*) FROM local_asset_locations
		WHERE content_verified_at > ?`, olderAt).Scan(&refreshed); err != nil {
		t.Fatalf("count refreshed verification timestamps: %v", err)
	}
	if refreshed != 2 {
		t.Fatalf("refreshed verification timestamps = %d, want 2", refreshed)
	}
}

func TestLocalContentVerificationFinishesCurrentFileAfterWindowDeadline(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	for name, contents := range map[string]string{
		"blocked.jpg": "blocked-photo",
		"later.jpg":   "later-photo",
	} {
		if err := os.WriteFile(filepath.Join(rootPath, name), []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(initial) error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 2 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v, want two registered assets", result, err)
	}
	blockedAt := formatCatalogTime(time.Now().UTC().Add(-14 * 24 * time.Hour))
	laterAt := formatCatalogTime(time.Now().UTC().Add(-7 * 24 * time.Hour))
	if _, err := service.catalog.db.Exec(`UPDATE local_asset_locations
		SET content_verified_at = CASE relative_path
				WHEN 'blocked.jpg' THEN ?
				ELSE ?
			END,
			content_verification_attempted_at = CASE relative_path
				WHEN 'blocked.jpg' THEN ?
				ELSE ?
			END`,
		blockedAt,
		laterAt,
		blockedAt,
		laterAt,
	); err != nil {
		t.Fatalf("age content verification timestamps: %v", err)
	}
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(second) error = %v", err)
	}

	service.localContentVerificationHash = func(ctx context.Context, file *os.File) (string, int64, os.FileInfo, error) {
		time.Sleep(25 * time.Millisecond)
		return sha1OpenFile(ctx, file)
	}
	startLocalContentVerificationTestWindow(t, service, 10*time.Millisecond)
	deadlineResult, err := service.RunLocalContentVerification(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalContentVerification(deadline) error = %v", err)
	}
	if deadlineResult.ProcessedFiles != 1 ||
		deadlineResult.VerifiedFiles != 1 ||
		deadlineResult.FailedFiles != 0 ||
		!deadlineResult.WindowExpired ||
		!deadlineResult.WindowComplete {
		t.Fatalf("deadline verification result = %+v, want the active file to finish cleanly", deadlineResult)
	}
	startLocalContentVerificationTestWindow(t, service, time.Second)
	nextResult, err := service.RunLocalContentVerification(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalContentVerification(next window) error = %v", err)
	}
	if nextResult.ProcessedFiles != 1 || nextResult.VerifiedFiles != 1 {
		t.Fatalf("next verification result = %+v, want later location verified", nextResult)
	}
	var blockedAfter string
	var laterAfter string
	if err := service.catalog.db.QueryRow(`SELECT
			MAX(CASE WHEN relative_path = 'blocked.jpg' THEN content_verified_at END),
			MAX(CASE WHEN relative_path = 'later.jpg' THEN content_verified_at END)
		FROM local_asset_locations`).Scan(&blockedAfter, &laterAfter); err != nil {
		t.Fatalf("read rotated verification timestamps: %v", err)
	}
	if blockedAfter == blockedAt {
		t.Fatalf("blocked content_verified_at = %q, want refreshed after finishing beyond the deadline", blockedAfter)
	}
	if laterAfter == laterAt {
		t.Fatalf("later content_verified_at = %q, want refreshed in the next daily window", laterAfter)
	}
}

func TestLocalContentVerificationResettlesHashMismatchWithUnchangedStat(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	mediaPath := filepath.Join(rootPath, "family.jpg")
	if err := os.WriteFile(mediaPath, []byte("photo-version-a"), 0o644); err != nil {
		t.Fatalf("WriteFile(initial) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v, want one registered asset", result, err)
	}
	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatalf("Stat(initial) error = %v", err)
	}
	oldVerifiedAt := formatCatalogTime(time.Now().UTC().Add(-7 * 24 * time.Hour))
	if _, err := service.catalog.db.Exec(`UPDATE local_asset_locations
		SET content_verified_at = ? WHERE relative_path = 'family.jpg'`, oldVerifiedAt); err != nil {
		t.Fatalf("age content verification timestamp: %v", err)
	}
	if err := os.WriteFile(mediaPath, []byte("photo-version-b"), 0o644); err != nil {
		t.Fatalf("WriteFile(replacement) error = %v", err)
	}
	if err := os.Chtimes(mediaPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("Chtimes(replacement) error = %v", err)
	}

	startLocalContentVerificationTestWindow(t, service, time.Minute)
	result, err := service.RunLocalContentVerification(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalContentVerification() error = %v", err)
	}
	if result.ProcessedFiles != 1 || result.ChangedFiles != 1 || result.VerifiedFiles != 0 || result.FailedFiles != 0 {
		t.Fatalf("content verification result = %+v, want one changed file", result)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations
		WHERE relative_path = 'family.jpg'
			AND status = 'discovered'
			AND asset_id IS NULL
			AND sha1_hex IS NULL
			AND content_verified_at IS NULL`, 1)
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs
		WHERE job_kind = 'metadata' AND status = 'queued'`, 1)
}

func TestLocalContentVerificationFailureAppearsInFailureDiagnostics(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), []byte("photo"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v, want one registered asset", result, err)
	}
	attemptedAt := formatCatalogTime(time.Now().UTC())
	if _, err := service.catalog.db.Exec(`UPDATE local_asset_locations
		SET content_verification_attempted_at = ?,
			content_verification_error = 'temporary read failure'
		WHERE relative_path = 'family.jpg'`, attemptedAt); err != nil {
		t.Fatalf("seed content verification failure: %v", err)
	}

	rows, err := service.LocalFailureDiagnosticRows(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("LocalFailureDiagnosticRows() error = %v", err)
	}
	var found bool
	for _, row := range rows {
		if row.FailureKind != "content_verification" {
			continue
		}
		found = true
		if row.RelativePath != "family.jpg" ||
			row.Component != "sha1" ||
			row.LastError != "temporary read failure" ||
			row.UpdatedAt != attemptedAt {
			t.Fatalf("content verification diagnostic = %+v", row)
		}
	}
	if !found {
		t.Fatalf("failure diagnostics = %+v, want content verification row", rows)
	}
}

func TestLocalContentVerificationDefersUnavailableRootUntilNextReconciliation(t *testing.T) {
	t.Parallel()

	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), []byte("photo"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := service.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v, want one registered asset", result, err)
	}
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(second) error = %v", err)
	}
	if err := os.Rename(rootPath, filepath.Join(parentPath, "unmounted")); err != nil {
		t.Fatalf("Rename(root) error = %v", err)
	}
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(empty mountpoint) error = %v", err)
	}

	startLocalContentVerificationTestWindow(t, service, time.Minute)
	result, err := service.RunLocalContentVerification(context.Background(), "1111111111111111")
	if err != nil {
		t.Fatalf("RunLocalContentVerification() error = %v", err)
	}
	if result.ProcessedFiles != 0 || result.FailedFiles != 1 {
		t.Fatalf("content verification result = %+v, want one root-level failed attempt", result)
	}
	runnable, err := service.LocalContentVerificationRunnableSourceKeys(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("LocalContentVerificationRunnableSourceKeys() error = %v", err)
	}
	if len(runnable) != 0 {
		t.Fatalf("runnable source keys = %#v, want this daily window completed", runnable)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_asset_locations
		WHERE relative_path = 'family.jpg' AND status = 'active'`, 1)
}

func startLocalContentVerificationTestWindow(t *testing.T, service *Service, duration time.Duration) {
	t.Helper()
	now := time.Now().UTC()
	started, err := service.StartLocalContentVerificationWindow(
		context.Background(),
		"1111111111111111",
		now,
		now.Add(duration),
	)
	if err != nil {
		t.Fatalf("StartLocalContentVerificationWindow() error = %v", err)
	}
	if !started {
		t.Fatal("StartLocalContentVerificationWindow() = false, want a new daily window")
	}
}
