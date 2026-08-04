package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLocalFilesystemCatalogSchemaCreatesCoreTables(t *testing.T) {
	t.Parallel()

	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	var schemaVersion int
	if err := store.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&schemaVersion); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if schemaVersion != catalogSchemaVersion {
		t.Fatalf("schema version = %d, want %d", schemaVersion, catalogSchemaVersion)
	}
	var applicationID int
	if err := store.db.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&applicationID); err != nil {
		t.Fatalf("read application id: %v", err)
	}
	if applicationID != catalogApplicationID {
		t.Fatalf("application id = %#x, want %#x", applicationID, catalogApplicationID)
	}
	for _, table := range []string{
		"catalog_canonical_state",
		"catalog_gallery_timeline_state",
		"catalog_gallery_timeline",
		"local_assets",
		"local_asset_locations",
		"local_renditions",
		"local_scan_jobs",
		"local_scan_root_state",
		"local_scan_directories",
		"local_scan_runs",
		"semantic_backfill_scheduler_state",
	} {
		var count int
		if err := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
			table,
		).Scan(&count); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("table %s count = %d, want 1", table, count)
		}
	}
	runColumns, err := store.tableColumns("local_scan_runs")
	if err != nil {
		t.Fatalf("inspect local_scan_runs columns: %v", err)
	}
	if !runColumns["scan_mode"] {
		t.Fatalf("local_scan_runs columns = %#v, want scan_mode", runColumns)
	}
	rootStateColumns, err := store.tableColumns("local_scan_root_state")
	if err != nil {
		t.Fatalf("inspect local_scan_root_state columns: %v", err)
	}
	if !rootStateColumns["last_quick_scan_at"] ||
		!rootStateColumns["last_reconciliation_at"] ||
		!rootStateColumns["content_verification_scheduled_at"] ||
		!rootStateColumns["content_verification_window_started_at"] ||
		!rootStateColumns["content_verification_window_deadline_at"] ||
		!rootStateColumns["content_verification_status"] ||
		!rootStateColumns["content_verification_skip_reason"] ||
		!rootStateColumns["content_verification_processed_files"] ||
		!rootStateColumns["content_verification_read_bytes"] ||
		!rootStateColumns["root_identity"] ||
		!rootStateColumns["root_generation"] ||
		!rootStateColumns["reconciliation_pending"] {
		t.Fatalf("local_scan_root_state columns = %#v, want durable scan times, scheduled verification result, root identity, and reconciliation generation", rootStateColumns)
	}
	renditionColumns, err := store.tableColumns("local_renditions")
	if err != nil {
		t.Fatalf("inspect local_renditions columns: %v", err)
	}
	if !renditionColumns["content_sha256"] {
		t.Fatalf("local_renditions columns = %#v, want content_sha256", renditionColumns)
	}
	locationColumns, err := store.tableColumns("local_asset_locations")
	if err != nil {
		t.Fatalf("inspect local_asset_locations columns: %v", err)
	}
	if !locationColumns["metadata_not_before"] || !locationColumns["file_identity"] {
		t.Fatalf("local_asset_locations columns = %#v, want metadata_not_before and file_identity", locationColumns)
	}
	columns, err := store.tableColumns("local_scan_jobs")
	if err != nil {
		t.Fatalf("inspect local_scan_jobs columns: %v", err)
	}
	if !columns["sort_at"] || !columns["root_generation"] {
		t.Fatalf("local_scan_jobs columns = %#v, want sort_at and root_generation", columns)
	}
	for _, indexName := range []string{"idx_local_scan_jobs_metadata_ready", "idx_local_scan_jobs_source_metadata_ready", "idx_local_scan_jobs_thumbnail_ready", "idx_local_scan_jobs_source_thumbnail_ready", "idx_local_scan_jobs_location_pending"} {
		var indexSQL string
		if err := store.db.QueryRowContext(ctx,
			`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'index' AND name = ?`,
			indexName,
		).Scan(&indexSQL); err != nil {
			t.Fatalf("inspect index %s: %v", indexName, err)
		}
		if !strings.Contains(indexSQL, "root_generation") {
			t.Fatalf("index %s = %q, want root_generation equality prefix", indexName, indexSQL)
		}
		if indexName != "idx_local_scan_jobs_location_pending" && !strings.Contains(indexSQL, "sort_at") {
			t.Fatalf("index %s = %q, want sort_at ordering", indexName, indexSQL)
		}
	}
	contentVerificationIndex, err := store.indexColumns("idx_local_asset_locations_content_verification")
	if err != nil {
		t.Fatalf("inspect content verification index: %v", err)
	}
	if !hasStringPrefix(contentVerificationIndex, []string{"source_key", "status", "content_verification_attempted_at", "id"}) {
		t.Fatalf("content verification index = %#v, want durable attempt ordering", contentVerificationIndex)
	}
	gallerySourceIndex, err := store.indexColumns(catalogGallerySourceCanonicalIndex)
	if err != nil {
		t.Fatalf("inspect gallery source index: %v", err)
	}
	if !hasStringPrefix(gallerySourceIndex, []string{"source_key", "datasource_kind", "visibility_status", "canonical_asset_id"}) {
		t.Fatalf("gallery source index = %#v, want configured-source count coverage", gallerySourceIndex)
	}
	galleryTimelineIndex, err := store.indexColumns("idx_catalog_gallery_timeline_captured")
	if err != nil {
		t.Fatalf("inspect gallery timeline date index: %v", err)
	}
	if !hasStringPrefix(galleryTimelineIndex, []string{"generation", "captured_at", "canonical_asset_id", "global_position"}) {
		t.Fatalf("gallery timeline date index = %#v, want generation-scoped date lookup", galleryTimelineIndex)
	}
	galleryTimelineStateColumns, err := store.tableColumns("catalog_gallery_timeline_state")
	if err != nil {
		t.Fatalf("inspect gallery timeline state columns: %v", err)
	}
	if !galleryTimelineStateColumns["canonical_generation"] {
		t.Fatalf("gallery timeline state columns = %#v, want canonical_generation", galleryTimelineStateColumns)
	}
	var canonicalGeneration int64
	if err := store.db.QueryRowContext(ctx, `SELECT generation FROM catalog_canonical_state WHERE singleton_id = 1`).Scan(&canonicalGeneration); err != nil {
		t.Fatalf("read initial canonical generation: %v", err)
	}
	if canonicalGeneration != 0 {
		t.Fatalf("initial canonical generation = %d, want 0", canonicalGeneration)
	}

	now := formatCatalogTime(time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC))
	if _, err := store.db.ExecContext(ctx, `INSERT INTO local_assets (
			source_key, asset_id, sha1_hex, media_type, filename, captured_at,
			captured_at_source, visibility_status, thumbnail_status, first_seen_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"1111111111111111",
		"asset-local-1",
		"0123456789abcdef0123456789abcdef01234567",
		"image",
		"family.jpg",
		now,
		"exif",
		"active",
		"pending",
		now,
		now,
	); err != nil {
		t.Fatalf("insert local asset: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO local_asset_locations (
			source_key, asset_id, root_key, relative_path, size_bytes, mtime,
			fast_signature, sha1_hex, status, first_seen_at, last_seen_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"1111111111111111",
		"asset-local-1",
		"nas-photos",
		"2026/family.jpg",
		12345,
		now,
		"12345:2026-06-13T12:00:00Z",
		"0123456789abcdef0123456789abcdef01234567",
		"active",
		now,
		now,
		now,
	); err != nil {
		t.Fatalf("insert local asset location: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO local_scan_root_state (
			source_key, root_key, root_status, phase0_status, updated_at
		) VALUES (?, ?, ?, ?, ?)`,
		"1111111111111111",
		"nas-photos",
		"ready",
		"idle",
		now,
	); err != nil {
		t.Fatalf("insert local scan root state: %v", err)
	}
}

func TestCatalogStoreRestoresGallerySourceIndexForExistingCurrentSchema(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	store, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	if _, err := store.db.Exec(`DROP INDEX ` + catalogGallerySourceCanonicalIndex); err != nil {
		_ = store.Close()
		t.Fatalf("drop gallery source index: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store without gallery source index: %v", err)
	}

	reopened, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("reopen existing current-schema store: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	columns, err := reopened.indexColumns(catalogGallerySourceCanonicalIndex)
	if err != nil {
		t.Fatalf("inspect restored gallery source index: %v", err)
	}
	if !reflect.DeepEqual(columns, []string{"source_key", "datasource_kind", "visibility_status", "canonical_asset_id"}) {
		t.Fatalf("restored gallery source index = %#v", columns)
	}
}

func TestCatalogUsesFreshStateRootInsteadOfDevelopmentState(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	developmentRoot := filepath.Join(dataDir, "catalog-state")
	if err := os.MkdirAll(developmentRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll(development root) error = %v", err)
	}
	developmentDB, err := sql.Open("sqlite", filepath.Join(developmentRoot, catalogDatabaseName))
	if err != nil {
		t.Fatalf("sql.Open(development DB) error = %v", err)
	}
	if _, err := developmentDB.Exec(`CREATE TABLE development_only (id INTEGER PRIMARY KEY); PRAGMA user_version = 1`); err != nil {
		t.Fatalf("seed development DB: %v", err)
	}
	if err := developmentDB.Close(); err != nil {
		t.Fatalf("Close(development DB) error = %v", err)
	}

	store, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()
	if got, want := filepath.Dir(store.Path()), filepath.Join(dataDir, catalogStateDirName); got != want {
		t.Fatalf("catalog root = %q, want %q", got, want)
	}
	var developmentTableCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'development_only'`).Scan(&developmentTableCount); err != nil {
		t.Fatalf("inspect current product DB: %v", err)
	}
	if developmentTableCount != 0 {
		t.Fatalf("development table count = %d, want fresh product schema", developmentTableCount)
	}
	if _, err := os.Stat(filepath.Join(developmentRoot, catalogDatabaseName)); err != nil {
		t.Fatalf("development DB should remain outside product state root: %v", err)
	}
}

func TestCatalogSchemaCreationRollsBackPartialFailure(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	databaseDir := filepath.Join(dataDir, catalogStateDirName)
	if err := os.MkdirAll(databaseDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(database dir) error = %v", err)
	}
	databasePath := filepath.Join(databaseDir, catalogDatabaseName)
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if err := createCatalogSchema(context.Background(), db, []string{
		`CREATE TABLE partial_catalog_state (id INTEGER PRIMARY KEY)`,
		`THIS IS NOT VALID SQL`,
	}); err == nil {
		t.Fatal("createCatalogSchema() error = nil, want injected statement failure")
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 0 {
		t.Fatalf("user_version = %d, want 0 after rollback", version)
	}
	var productTables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&productTables); err != nil {
		t.Fatalf("count product tables: %v", err)
	}
	if productTables != 0 {
		t.Fatalf("product table count = %d, want no partial schema", productTables)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close(partial DB) error = %v", err)
	}

	store, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore(after rollback) error = %v", err)
	}
	defer store.Close()
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read recreated user_version: %v", err)
	}
	if version != catalogSchemaVersion {
		t.Fatalf("recreated user_version = %d, want %d", version, catalogSchemaVersion)
	}
}

func TestAdminStatusSnapshotsPersistAcrossStoreOpen(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	store, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	ctx := context.Background()
	updatedAt := time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC)
	if err := store.SaveAdminStatusSnapshot(ctx, "datasource_indexing", []byte(`{"tasks":[{"phase":"embeddings"}]}`), updatedAt); err != nil {
		t.Fatalf("SaveAdminStatusSnapshot() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("reopen LoadOrCreateCatalogStore() error = %v", err)
	}
	defer reopened.Close()
	snapshot, ok, err := reopened.AdminStatusSnapshot(ctx, "datasource_indexing")
	if err != nil {
		t.Fatalf("AdminStatusSnapshot() error = %v", err)
	}
	if !ok {
		t.Fatal("AdminStatusSnapshot() ok = false, want true")
	}
	if string(snapshot.Payload) != `{"tasks":[{"phase":"embeddings"}]}` ||
		!snapshot.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("snapshot = %+v payload=%s", snapshot, string(snapshot.Payload))
	}
	if _, err := os.Stat(reopened.adminStatusPath()); err != nil {
		t.Fatalf("admin status db stat error = %v", err)
	}
	for _, table := range []string{"asset_processing_stats", "admin_status_snapshots"} {
		var count int
		if err := reopened.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
			table,
		).Scan(&count); err != nil {
			t.Fatalf("check main table %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("main table %s count = %d, want 0", table, count)
		}
	}
	if err := reopened.DeleteAdminStatusSnapshot(ctx, "datasource_indexing"); err != nil {
		t.Fatalf("DeleteAdminStatusSnapshot() error = %v", err)
	}
	if snapshot, ok, err := reopened.AdminStatusSnapshot(ctx, "datasource_indexing"); err != nil || ok {
		t.Fatalf("AdminStatusSnapshot() after delete = snapshot:%+v ok:%v err:%v, want no snapshot", snapshot, ok, err)
	}
}

func TestCatalogStoreRejectsUnversionedDevelopmentDatabase(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	dbDir := filepath.Join(dataDir, catalogStateDirName)
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dbDir, catalogDatabaseName))
	if err != nil {
		t.Fatalf("open development db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE development_only (id INTEGER PRIMARY KEY)`); err != nil {
		_ = db.Close()
		t.Fatalf("create development table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close development db: %v", err)
	}
	for _, relative := range []string{
		filepath.Join(localRenditionsDirName, "1111111111111111", "asset-1", "preview.jpg"),
		filepath.Join(semanticVectorPayloadDirName, "stale.vecs"),
		filepath.Join(semanticBinaryIndexDirName, "stale.tidx"),
		catalogAdminDBName,
	} {
		path := filepath.Join(dbDir, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("stale catalog sidecar"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}

	if _, err := LoadOrCreateCatalogStore(dataDir); !errors.Is(err, ErrCatalogSchemaResetRequired) {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v, want ErrCatalogSchemaResetRequired", err)
	} else if !strings.Contains(err.Error(), dbDir) {
		t.Fatalf("reset-required error = %q, want catalog state root %q", err, dbDir)
	}

	if err := os.RemoveAll(dbDir); err != nil {
		t.Fatalf("RemoveAll(catalog state) error = %v", err)
	}
	reopened, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() after reset error = %v", err)
	}
	defer reopened.Close()
	for _, relative := range []string{
		filepath.Join(localRenditionsDirName, "1111111111111111", "asset-1", "preview.jpg"),
		filepath.Join(semanticVectorPayloadDirName, "stale.vecs"),
		filepath.Join(semanticBinaryIndexDirName, "stale.tidx"),
		catalogAdminDBName,
	} {
		if _, err := os.Stat(filepath.Join(dbDir, relative)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale catalog state %s error = %v, want removed", relative, err)
		}
	}
}

func TestCatalogStoreRejectsUnknownSchemaVersion(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	dbDir := filepath.Join(dataDir, catalogStateDirName)
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dbDir, catalogDatabaseName))
	if err != nil {
		t.Fatalf("open unknown-version db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE unknown_schema (id INTEGER PRIMARY KEY)`); err != nil {
		_ = db.Close()
		t.Fatalf("create unknown schema table: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		_ = db.Close()
		t.Fatalf("set unknown user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close unknown-version db: %v", err)
	}

	if _, err := LoadOrCreateCatalogStore(dataDir); !errors.Is(err, ErrCatalogSchemaResetRequired) {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v, want ErrCatalogSchemaResetRequired", err)
	} else if !strings.Contains(err.Error(), dbDir) {
		t.Fatalf("reset-required error = %q, want catalog state root %q", err, dbDir)
	}
}

func TestCatalogStoreRejectsPreviousSchemaVersion(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	dbDir := filepath.Join(dataDir, catalogStateDirName)
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dbDir, catalogDatabaseName))
	if err != nil {
		t.Fatalf("open previous-version db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE previous_schema (id INTEGER PRIMARY KEY)`); err != nil {
		_ = db.Close()
		t.Fatalf("create previous schema table: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		_ = db.Close()
		t.Fatalf("set previous schema version: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA application_id = %d`, catalogApplicationID)); err != nil {
		_ = db.Close()
		t.Fatalf("set catalog application ID: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close previous-version db: %v", err)
	}

	if _, err := LoadOrCreateCatalogStore(dataDir); !errors.Is(err, ErrCatalogSchemaResetRequired) {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v, want ErrCatalogSchemaResetRequired", err)
	} else if !strings.Contains(err.Error(), "found schema version 1") || !strings.Contains(err.Error(), "want version 2") {
		t.Fatalf("reset-required error = %q, want explicit V1 to V2 rebuild guidance", err)
	}
}

func TestCatalogStoreRejectsV1WithoutApplicationID(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	dbDir := filepath.Join(dataDir, catalogStateDirName)
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dbDir, catalogDatabaseName))
	if err != nil {
		t.Fatalf("open V1 db without application ID: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		_ = db.Close()
		t.Fatalf("seed V1 schema version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close V1 db without application ID: %v", err)
	}

	if _, err := LoadOrCreateCatalogStore(dataDir); !errors.Is(err, ErrCatalogSchemaResetRequired) {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v, want ErrCatalogSchemaResetRequired", err)
	} else if !strings.Contains(err.Error(), dbDir) {
		t.Fatalf("reset-required error = %q, want catalog state root %q", err, dbDir)
	} else if !strings.Contains(err.Error(), "application id 0x0") {
		t.Fatalf("reset-required error = %q, want missing application ID", err)
	}
}

func TestCatalogStoreRejectsV2WithoutRootWorkGeneration(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	dbDir := filepath.Join(dataDir, catalogStateDirName)
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dbDir, catalogDatabaseName))
	if err != nil {
		t.Fatalf("open V2 db without root work generation: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE local_scan_root_state (
		source_key TEXT NOT NULL,
		root_key TEXT NOT NULL,
		root_identity TEXT NOT NULL DEFAULT '',
		PRIMARY KEY(source_key, root_key)
	)`); err != nil {
		_ = db.Close()
		t.Fatalf("create root state without work generation: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE local_scan_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_key TEXT NOT NULL,
		job_kind TEXT NOT NULL
	)`); err != nil {
		_ = db.Close()
		t.Fatalf("create jobs without work generation: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE catalog_canonical_state (
		singleton_id INTEGER PRIMARY KEY,
		generation INTEGER NOT NULL
	);
	INSERT INTO catalog_canonical_state(singleton_id, generation) VALUES (1, 0);
	CREATE TABLE catalog_gallery_timeline_state (
		singleton_id INTEGER PRIMARY KEY,
		generation INTEGER NOT NULL,
		canonical_generation INTEGER NOT NULL,
		scope_key TEXT NOT NULL,
		total_count INTEGER NOT NULL,
		built_at TEXT NOT NULL
	)`); err != nil {
		_ = db.Close()
		t.Fatalf("create current gallery generation state: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, catalogSchemaVersion)); err != nil {
		_ = db.Close()
		t.Fatalf("seed V2 schema version: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA application_id = %d`, catalogApplicationID)); err != nil {
		_ = db.Close()
		t.Fatalf("seed catalog application ID: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close V2 db without root work generation: %v", err)
	}

	if _, err := LoadOrCreateCatalogStore(dataDir); !errors.Is(err, ErrCatalogSchemaResetRequired) {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v, want ErrCatalogSchemaResetRequired", err)
	} else if !strings.Contains(err.Error(), "missing required local scan state") {
		t.Fatalf("reset-required error = %q, want missing local scan state", err)
	}
}

func TestCatalogStoreRejectsCurrentSchemaWithOutdatedLocalWorkIndex(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	store, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	if _, err := store.db.Exec(`DROP INDEX idx_local_scan_jobs_source_metadata_ready`); err != nil {
		_ = store.Close()
		t.Fatalf("drop generation-aware metadata index: %v", err)
	}
	if _, err := store.db.Exec(`CREATE INDEX idx_local_scan_jobs_source_metadata_ready
		ON local_scan_jobs(source_key, job_kind, status, priority, sort_at DESC, id, location_id)
		WHERE location_id IS NOT NULL`); err != nil {
		_ = store.Close()
		t.Fatalf("create outdated metadata index: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store with outdated metadata index: %v", err)
	}

	if _, err := LoadOrCreateCatalogStore(dataDir); !errors.Is(err, ErrCatalogSchemaResetRequired) {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v, want ErrCatalogSchemaResetRequired", err)
	} else if !strings.Contains(err.Error(), "outdated local work index") {
		t.Fatalf("reset-required error = %q, want outdated local work index", err)
	}
}

func TestCatalogStoreStartupDoesNotRebuildHealthyCanonicalCatalog(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	store, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	ctx := context.Background()
	if _, err := store.ReplaceFull(ctx, "1111111111111111", []ImmichMirrorAsset{{
		UpstreamAssetID:  "asset-1",
		MediaType:        "image",
		Filename:         "beach.jpg",
		CapturedAt:       time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		ContentSHA1Hex:   "0123456789abcdef0123456789abcdef01234567",
		ContentSizeBytes: 1234,
	}}, 0, time.Now().UTC()); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}
	sentinel := formatCatalogTime(time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC))
	if _, err := store.db.ExecContext(ctx, `UPDATE catalog_canonical_assets SET updated_at = ?`, sentinel); err != nil {
		t.Fatalf("set sentinel canonical updated_at: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("reopen LoadOrCreateCatalogStore() error = %v", err)
	}
	defer reopened.Close()

	var updatedAt string
	if err := reopened.db.QueryRowContext(ctx, `SELECT updated_at FROM catalog_canonical_assets`).Scan(&updatedAt); err != nil {
		t.Fatalf("read canonical updated_at after reopen: %v", err)
	}
	if updatedAt != sentinel {
		t.Fatalf("canonical updated_at after reopen = %q, want sentinel %q", updatedAt, sentinel)
	}
}
