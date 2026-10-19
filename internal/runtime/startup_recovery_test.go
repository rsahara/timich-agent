package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/store"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestNewAgentRuntimeRejectsFailedLocalJobRecovery(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"metadata", "thumbnail"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			cfg := config.ResolvedConfig{Config: config.Default()}
			cfg.DataDir = t.TempDir()
			cfg.ConfigPath = filepath.Join(cfg.DataDir, "agent.json")
			root := t.TempDir()
			cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "photos", Path: root}}
			cfg.Datasources = []config.DatasourceConfig{{
				SourceKey: "1111111111111111", Name: "Photos", Kind: config.DatasourceKindLocalFiles,
				RootKey: "photos", Scan: &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
			}}
			for _, name := range []string{"first.jpg", "second.jpg"} {
				if err := os.WriteFile(filepath.Join(root, name), encodeRuntimeJPEGForTest(t, 96, 64), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			state, err := store.LoadOrCreate(cfg.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			first, err := NewAgentRuntime(BuildInfo{}, cfg, state, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { first.Close() })
			if _, err := first.catalogService().RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
				t.Fatal(err)
			}
			if result, err := first.catalogService().RunLocalMetadataBatch(context.Background(), 1); err != nil || result.RegisteredAssets != 1 {
				t.Fatalf("seed metadata and thumbnail jobs: %+v, %v", result, err)
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			dbPath := filepath.Join(cfg.DataDir, "catalog-state-v1", "catalog.db")
			// The completed metadata row is deleted. Keep the second photo's metadata
			// job and seed an interrupted thumbnail for the registered first photo.
			execStartupRecoverySQL(t, dbPath, `INSERT INTO local_scan_jobs (
				source_key, job_kind, priority, root_key, root_generation, asset_id, status, scheduled_at, sort_at
			) SELECT a.source_key, 'thumbnail', 1, rs.root_key, rs.root_generation, a.asset_id,
				'queued', '2026-10-18T00:00:00Z', a.captured_at
				FROM local_assets a JOIN local_scan_root_state rs ON rs.source_key = a.source_key`)
			execStartupRecoverySQL(t, dbPath, `UPDATE local_scan_jobs SET status = 'running', attempts = 2,
				locked_at = '2026-10-18T00:00:00Z' WHERE job_kind IN ('metadata', 'thumbnail')`)
			execStartupRecoverySQL(t, dbPath, fmt.Sprintf(`CREATE TRIGGER fail_startup_recovery
				BEFORE UPDATE OF status ON local_scan_jobs
				WHEN OLD.status = 'running' AND NEW.status = 'queued' AND OLD.job_kind = '%s'
				BEGIN SELECT RAISE(ABORT, 'injected startup recovery failure'); END`, kind))

			for attempt := 0; attempt < 2; attempt++ {
				got, err := NewAgentRuntime(BuildInfo{}, cfg, state, time.Now())
				if got != nil {
					got.Close()
					t.Fatal("failed recovery returned a usable runtime")
				}
				var sqlErr *sqlite.Error
				if !errors.As(err, &sqlErr) || sqlErr.Code() != sqlite3.SQLITE_CONSTRAINT_TRIGGER ||
					!strings.Contains(err.Error(), "recover local scan jobs during startup") ||
					!strings.Contains(err.Error(), "injected startup recovery failure") {
					t.Fatalf("startup error lost recovery context or SQLite cause: %v", err)
				}
				assertStartupRecoveryStoresClosed(t, cfg.DataDir)
			}

			execStartupRecoverySQL(t, dbPath, `DROP TRIGGER fail_startup_recovery`)
			recovered, err := NewAgentRuntime(BuildInfo{}, cfg, state, time.Now())
			if err != nil {
				t.Fatalf("restart after removing fault: %v", err)
			}
			defer recovered.Close()
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM local_scan_jobs
				WHERE job_kind IN ('metadata', 'thumbnail') AND status = 'queued'
				AND attempts = 2 AND locked_at IS NULL`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("recovered jobs = %d, %v; want both queued with preserved attempts", count, err)
			}
		})
	}
}

func execStartupRecoverySQL(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func assertStartupRecoveryStoresClosed(t *testing.T, dataDir string) {
	t.Helper()
	if goruntime.GOOS != "linux" {
		return // The real handle-leak assertion uses Linux procfs.
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if os.IsNotExist(err) {
			continue // Other parallel tests can close descriptors during enumeration.
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(target, dataDir+string(os.PathSeparator)) {
			t.Errorf("failed constructor left a store handle open: %s", target)
		}
	}
}

type startupRecoveryCloser func() error

func (close startupRecoveryCloser) Close() error { return close() }

func TestFailedLocalJobRecoveryPreservesCauseAndClosesBothStores(t *testing.T) {
	t.Parallel()
	cause := errors.New("reset failed")
	catalogErr := errors.New("catalog close failed")
	uploadErr := errors.New("upload close failed")
	for _, tc := range []struct {
		name             string
		catalog, uploads error
	}{
		{name: "closed"},
		{name: "catalog close fails", catalog: catalogErr},
		{name: "upload close fails", uploads: uploadErr},
		{name: "both close operations fail", catalog: catalogErr, uploads: uploadErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalogClosed, uploadsClosed := false, false
			err := failedLocalJobRecovery(cause, startupRecoveryCloser(func() error {
				catalogClosed = true
				return tc.catalog
			}), startupRecoveryCloser(func() error {
				uploadsClosed = true
				return tc.uploads
			}))
			if !catalogClosed || !uploadsClosed || !errors.Is(err, cause) {
				t.Fatalf("cleanup lost a resource or the primary cause: catalog=%v uploads=%v err=%v", catalogClosed, uploadsClosed, err)
			}
			for _, expected := range []error{tc.catalog, tc.uploads} {
				if expected != nil && !errors.Is(err, expected) {
					t.Errorf("cleanup lost %v: %v", expected, err)
				}
			}
		})
	}
}
