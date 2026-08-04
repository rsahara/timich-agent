package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const (
	catalogStateDirName                = "catalog-state-v1"
	catalogDatabaseName                = "catalog.db"
	catalogAdminDBName                 = "catalog-admin.db"
	catalogReadConns                   = 4
	catalogSchemaVersion               = 2
	catalogGallerySourceCanonicalIndex = "idx_catalog_assets_gallery_source_canonical"
	// "TMCH" identifies the final Timich catalog format. Earlier unreleased
	// development databases reused user_version=1 without this marker.
	catalogApplicationID = 0x544d4348

	MirrorSyncModeFull        = "full"
	MirrorSyncModeIncremental = "incremental"

	MirrorVisibilityActive     = "active"
	MirrorVisibilityMissing    = "missing"
	MirrorVisibilityOutOfScope = "out_of_scope"
)

var (
	ErrCatalogNotConfigured       = errors.New("catalog is not configured")
	ErrCatalogSchemaResetRequired = errors.New("catalog database reset is required")
)

// ImmichMirrorAsset is one normalized Immich metadata row stored in the Agent
// catalog.
type ImmichMirrorAsset struct {
	UpstreamAssetID  string
	MediaType        string
	Filename         string
	CapturedAt       time.Time
	Duration         *string
	SourceUpdatedAt  *time.Time
	ContentSHA1Hex   string
	ContentSizeBytes int64
	IsFavorite       bool
	City             string
	State            string
	Country          string
	PlaceLabel       string
	Description      string
}

// MirrorStatus summarizes the Agent-owned datasource mirror.
type MirrorStatus struct {
	Enabled               bool                  `json:"enabled"`
	Status                string                `json:"status,omitempty"`
	LatestAssetLimit      int                   `json:"latestAssetLimit,omitempty"`
	ActiveCount           int                   `json:"activeCount"`
	OutOfScopeCount       int                   `json:"outOfScopeCount"`
	MissingCount          int                   `json:"missingCount"`
	Semantic              CatalogSemanticStatus `json:"semantic"`
	LastFullSyncAt        *time.Time            `json:"lastFullSyncAt,omitempty"`
	LastIncrementalSyncAt *time.Time            `json:"lastIncrementalSyncAt,omitempty"`
	LastError             string                `json:"lastError,omitempty"`
}

// CatalogSemanticStatus summarizes a Timich-owned datasource semantic index.
type CatalogSemanticStatus struct {
	Status               string                   `json:"status,omitempty"`
	ModelID              string                   `json:"modelId,omitempty"`
	VectorSpaceID        string                   `json:"vectorSpaceId,omitempty"`
	EmbeddingDim         int                      `json:"embeddingDim,omitempty"`
	ProfileKind          string                   `json:"profileKind,omitempty"`
	InputKind            string                   `json:"inputKind,omitempty"`
	CompletedVectorCount int                      `json:"completedVectorCount"`
	IndexedVectorCount   int                      `json:"indexedVectorCount"`
	BuiltAt              *time.Time               `json:"builtAt,omitempty"`
	LastError            string                   `json:"lastError,omitempty"`
	ModelPack            *SemanticModelPackStatus `json:"modelPack,omitempty"`
	AssetGeneration      int64                    `json:"-"`
	IndexedGeneration    int64                    `json:"-"`
}

// MirrorSyncResult reports one manual mirror sync.
type MirrorSyncResult struct {
	Mode             string       `json:"mode"`
	Status           string       `json:"status"`
	LatestAssetLimit int          `json:"latestAssetLimit,omitempty"`
	FetchedCount     int          `json:"fetchedCount"`
	ActiveCount      int          `json:"activeCount"`
	OutOfScopeCount  int          `json:"outOfScopeCount"`
	MissingCount     int          `json:"missingCount"`
	StartedAt        time.Time    `json:"startedAt"`
	CompletedAt      time.Time    `json:"completedAt"`
	Error            string       `json:"error,omitempty"`
	Mirror           MirrorStatus `json:"mirror"`
}

type CatalogStore struct {
	root                         string
	path                         string
	db                           *sql.DB
	readDB                       *sql.DB
	semanticBinaryIntegrityMu    sync.Mutex
	semanticBinaryIntegrity      map[string]semanticBinaryIntegrityCacheEntry
	semanticVectorPayloadMu      sync.Mutex
	semanticVectorPayloadCacheMu sync.Mutex
	semanticVectorPayloadCache   map[string][]byte
	semanticVectorPayloadOrder   []string
	galleryTotalMu               sync.Mutex
	galleryTotalCache            map[string]int
	datasourceState              *atomic.Pointer[serviceDatasourceState]
	standaloneGalleryReadiness   atomic.Pointer[catalogGalleryReadiness]
}

func (s *CatalogStore) galleryReadinessSnapshot() catalogGalleryReadiness {
	if s == nil {
		return catalogGalleryReadiness{}
	}
	if s.datasourceState != nil {
		if state := s.datasourceState.Load(); state != nil {
			return state.galleryReadiness
		}
	}
	if readiness := s.standaloneGalleryReadiness.Load(); readiness != nil {
		return *readiness
	}
	return catalogGalleryReadiness{}
}

func (s *CatalogStore) setStandaloneGalleryReadiness(readiness catalogGalleryReadiness) {
	if s == nil {
		return
	}
	s.standaloneGalleryReadiness.Store(&readiness)
	if err := s.ensureGalleryTimeline(context.Background(), readiness); err != nil {
		log.Printf("timich-agent gallery timeline refresh failed error=%v", err)
	}
}

func LoadOrCreateCatalogStore(dataDir string) (*CatalogStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("data directory must not be empty")
	}
	root := filepath.Join(dataDir, catalogStateDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create catalog state directory: %w", err)
	}
	path := filepath.Join(root, catalogDatabaseName)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open catalog store: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &CatalogStore{
		root:                       root,
		path:                       path,
		db:                         db,
		semanticBinaryIntegrity:    make(map[string]semanticBinaryIntegrityCacheEntry),
		semanticVectorPayloadCache: make(map[string][]byte),
		galleryTotalCache:          make(map[string]int),
	}
	if err := store.ensureCatalogSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := cleanupSemanticIndexCrashTemps(filepath.Join(root, semanticBinaryIndexDirName)); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.reconcileSemanticVectorPayloads(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	readDB, err := openCatalogReadOnlyDB(context.Background(), path, catalogReadConns)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store.readDB = readDB
	return store, nil
}

func openCatalogReadOnlyDB(ctx context.Context, path string, maxOpenConns int) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, ErrCatalogNotConfigured
	}
	if maxOpenConns <= 0 {
		maxOpenConns = 1
	}
	values := url.Values{}
	values.Add("mode", "ro")
	values.Add("_pragma", "busy_timeout(5000)")
	values.Add("_pragma", "query_only(ON)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+values.Encode())
	if err != nil {
		return nil, fmt.Errorf("open read-only catalog store: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping read-only catalog store: %w", err)
	}
	return db, nil
}

func (s *CatalogStore) openReadOnlyDB(ctx context.Context) (*sql.DB, error) {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return nil, ErrCatalogNotConfigured
	}
	return openCatalogReadOnlyDB(ctx, s.path, 1)
}

func (s *CatalogStore) queryDB() *sql.DB {
	if s == nil {
		return nil
	}
	if s.readDB != nil {
		return s.readDB
	}
	return s.db
}

func (s *CatalogStore) commitCatalogAssetChanges(ctx context.Context, tx *sql.Tx, canonicalChanged bool) error {
	s.galleryTotalMu.Lock()
	defer s.galleryTotalMu.Unlock()
	if canonicalChanged {
		if err := advanceCatalogCanonicalGenerationInTx(ctx, tx); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	readiness := s.galleryReadinessSnapshot()
	refreshGallery, err := galleryTimelineNeedsRefreshInTx(ctx, tx, readiness)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if refreshGallery {
		if err := s.rebuildGalleryTimelineInTx(ctx, tx, readiness); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	clear(s.galleryTotalCache)
	return nil
}

func (s *CatalogStore) clearGalleryTotalCache() {
	if s == nil {
		return
	}
	s.galleryTotalMu.Lock()
	clear(s.galleryTotalCache)
	s.galleryTotalMu.Unlock()
}

func (s *CatalogStore) openStatsWriteDB(ctx context.Context) (*sql.DB, error) {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return nil, ErrCatalogNotConfigured
	}
	return s.openAdminWriteDB(ctx)
}

func (s *CatalogStore) adminStatusPath() string {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return ""
	}
	return filepath.Join(s.root, catalogAdminDBName)
}

func (s *CatalogStore) openAdminReadDB(ctx context.Context) (*sql.DB, bool, error) {
	path := s.adminStatusPath()
	if path == "" {
		return nil, false, ErrCatalogNotConfigured
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("inspect admin status store: %w", err)
	}
	values := url.Values{}
	values.Add("mode", "ro")
	values.Add("_pragma", "busy_timeout(5000)")
	values.Add("_pragma", "query_only(ON)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+values.Encode())
	if err != nil {
		return nil, false, fmt.Errorf("open admin status store: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, false, fmt.Errorf("ping admin status store: %w", err)
	}
	return db, true, nil
}

func (s *CatalogStore) openAdminWriteDB(ctx context.Context) (*sql.DB, error) {
	path := s.adminStatusPath()
	if path == "" {
		return nil, ErrCatalogNotConfigured
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create admin status store directory: %w", err)
	}
	values := url.Values{}
	values.Add("mode", "rwc")
	values.Add("_pragma", "busy_timeout(5000)")
	values.Add("_pragma", "foreign_keys(ON)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+values.Encode())
	if err != nil {
		return nil, fmt.Errorf("open admin status store writer: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping admin status store writer: %w", err)
	}
	if err := ensureAdminStatusSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func ensureAdminStatusSchema(ctx context.Context, db *sql.DB) error {
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS admin_status_snapshots (
			snapshot_key TEXT PRIMARY KEY,
			payload_json TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS asset_processing_stats (
			scope_key TEXT NOT NULL DEFAULT '',
			stage TEXT NOT NULL,
			variant TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			count INTEGER NOT NULL DEFAULT 0,
			total_count INTEGER NOT NULL DEFAULT 0,
			refreshed_at TEXT NOT NULL,
			PRIMARY KEY(scope_key, stage, variant, status)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_asset_processing_stats_stage
			ON asset_processing_stats(stage, variant, status)`,
		`CREATE INDEX IF NOT EXISTS idx_asset_processing_stats_refreshed
			ON asset_processing_stats(refreshed_at)`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ensure admin status store schema: %w", err)
		}
	}
	return nil
}

func (s *CatalogStore) Close() error {
	if s == nil {
		return nil
	}
	var err error
	if s.readDB != nil {
		err = s.readDB.Close()
	}
	if s.db != nil {
		if closeErr := s.db.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

func (s *CatalogStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *CatalogStore) ensureCatalogSchema() error {
	preludeStatements := []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
	}
	for _, statement := range preludeStatements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("prepare catalog store: %w", err)
		}
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read catalog schema version: %w", err)
	}
	var applicationID int
	if err := s.db.QueryRow(`PRAGMA application_id`).Scan(&applicationID); err != nil {
		return fmt.Errorf("read catalog application id: %w", err)
	}
	if version == catalogSchemaVersion && applicationID == catalogApplicationID {
		rootStateColumns, err := s.tableColumns("local_scan_root_state")
		if err != nil {
			return fmt.Errorf("inspect catalog root continuity schema: %w", err)
		}
		locationColumns, err := s.tableColumns("local_asset_locations")
		if err != nil {
			return fmt.Errorf("inspect catalog content verification schema: %w", err)
		}
		jobColumns, err := s.tableColumns("local_scan_jobs")
		if err != nil {
			return fmt.Errorf("inspect catalog local work schema: %w", err)
		}
		canonicalStateColumns, err := s.tableColumns("catalog_canonical_state")
		if err != nil {
			return fmt.Errorf("inspect catalog canonical generation schema: %w", err)
		}
		galleryStateColumns, err := s.tableColumns("catalog_gallery_timeline_state")
		if err != nil {
			return fmt.Errorf("inspect catalog gallery timeline state schema: %w", err)
		}
		if !canonicalStateColumns["generation"] || !galleryStateColumns["canonical_generation"] {
			return fmt.Errorf("%w: catalog schema is missing required gallery generation state; stop Timich Agent and remove the catalog state directory %q before restarting", ErrCatalogSchemaResetRequired, s.root)
		}
		var canonicalStateCount int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM catalog_canonical_state WHERE singleton_id = 1`).Scan(&canonicalStateCount); err != nil {
			return fmt.Errorf("inspect catalog canonical generation state: %w", err)
		}
		if canonicalStateCount != 1 {
			return fmt.Errorf("%w: catalog schema is missing the canonical generation singleton; stop Timich Agent and remove the catalog state directory %q before restarting", ErrCatalogSchemaResetRequired, s.root)
		}
		if !rootStateColumns["root_identity"] ||
			!rootStateColumns["root_generation"] ||
			!rootStateColumns["reconciliation_pending"] ||
			!rootStateColumns["last_reconciliation_at"] ||
			!rootStateColumns["last_content_verification_at"] ||
			!rootStateColumns["content_verification_scheduled_at"] ||
			!rootStateColumns["content_verification_window_started_at"] ||
			!rootStateColumns["content_verification_window_deadline_at"] ||
			!rootStateColumns["content_verification_status"] ||
			!rootStateColumns["content_verification_skip_reason"] ||
			!rootStateColumns["content_verification_processed_files"] ||
			!rootStateColumns["content_verification_verified_files"] ||
			!rootStateColumns["content_verification_changed_files"] ||
			!rootStateColumns["content_verification_failed_files"] ||
			!rootStateColumns["content_verification_read_bytes"] ||
			!locationColumns["content_verified_at"] ||
			!locationColumns["content_verification_attempted_at"] ||
			!locationColumns["content_verification_error"] ||
			!jobColumns["root_generation"] {
			return fmt.Errorf("%w: catalog schema is missing required local scan state; stop Timich Agent and remove the catalog state directory %q before restarting", ErrCatalogSchemaResetRequired, s.root)
		}
		requiredIndexPrefixes := map[string][]string{
			"idx_local_scan_jobs_metadata_ready":             {"job_kind", "status", "root_generation", "source_key"},
			"idx_local_scan_jobs_source_metadata_ready":      {"source_key", "root_key", "root_generation", "job_kind", "status"},
			"idx_local_scan_jobs_thumbnail_ready":            {"job_kind", "status", "root_generation", "source_key"},
			"idx_local_scan_jobs_source_thumbnail_ready":     {"source_key", "root_key", "root_generation", "job_kind", "status"},
			"idx_local_scan_jobs_location_pending":           {"source_key", "job_kind", "location_id", "status", "root_generation"},
			"idx_local_asset_locations_content_verification": {"source_key", "status", "content_verification_attempted_at", "id"},
		}
		for indexName, requiredPrefix := range requiredIndexPrefixes {
			indexColumns, err := s.indexColumns(indexName)
			if err != nil {
				return fmt.Errorf("inspect catalog local work index %s: %w", indexName, err)
			}
			if !hasStringPrefix(indexColumns, requiredPrefix) {
				return fmt.Errorf("%w: catalog schema has an outdated local work index %q; stop Timich Agent and remove the catalog state directory %q before restarting", ErrCatalogSchemaResetRequired, indexName, s.root)
			}
		}
		if err := s.ensureCatalogQueryIndexes(context.Background()); err != nil {
			return err
		}
		return nil
	}
	if version != 0 || applicationID != 0 {
		return fmt.Errorf("%w: found schema version %d and application id %#x, want version %d and application id %#x; stop Timich Agent and remove the catalog state directory %q before restarting", ErrCatalogSchemaResetRequired, version, applicationID, catalogSchemaVersion, catalogApplicationID, s.root)
	}
	var existingTables int
	if err := s.db.QueryRow(`SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&existingTables); err != nil {
		return fmt.Errorf("inspect catalog schema: %w", err)
	}
	if existingTables != 0 {
		return fmt.Errorf("%w: found an unversioned development catalog; stop Timich Agent and remove the catalog state directory %q before restarting", ErrCatalogSchemaResetRequired, s.root)
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS catalog_assets (
			source_key TEXT NOT NULL,
			datasource_kind TEXT NOT NULL,
			upstream_asset_id TEXT NOT NULL,
			media_type TEXT NOT NULL CHECK (media_type IN ('image', 'video')),
			filename TEXT NOT NULL,
			captured_at TEXT NOT NULL,
			duration TEXT,
			visibility_status TEXT NOT NULL CHECK (visibility_status IN ('active', 'missing', 'out_of_scope', 'permission_blocked')),
			source_updated_at TEXT,
			is_favorite INTEGER NOT NULL DEFAULT 0,
			canonical_asset_id TEXT,
			content_sha1_hex TEXT,
			content_size_bytes INTEGER,
			place_label TEXT,
			description TEXT,
			first_seen_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(source_key, upstream_asset_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_assets_visible_timeline
			ON catalog_assets(visibility_status, captured_at DESC, source_key, upstream_asset_id)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_assets_media_timeline
			ON catalog_assets(visibility_status, media_type, captured_at DESC, source_key, upstream_asset_id)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_assets_source_visible
			ON catalog_assets(source_key, visibility_status, captured_at DESC, upstream_asset_id)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_assets_semantic_status
			ON catalog_assets(source_key, datasource_kind, visibility_status, media_type, upstream_asset_id)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_assets_canonical
			ON catalog_assets(canonical_asset_id, visibility_status, source_key, upstream_asset_id)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_assets_gallery_source_canonical
			ON catalog_assets(source_key, datasource_kind, visibility_status, canonical_asset_id)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_assets_source_updated
			ON catalog_assets(source_key, datasource_kind, source_updated_at)`,
		`CREATE TABLE IF NOT EXISTS catalog_canonical_assets (
			canonical_asset_id TEXT PRIMARY KEY,
			content_sha1_hex TEXT,
			content_size_bytes INTEGER,
			media_type TEXT NOT NULL CHECK (media_type IN ('image', 'video')),
			filename TEXT NOT NULL,
			captured_at TEXT NOT NULL,
			duration TEXT,
			visibility_status TEXT NOT NULL CHECK (visibility_status IN ('active', 'missing', 'out_of_scope', 'permission_blocked')),
			primary_source_key TEXT NOT NULL,
			primary_upstream_asset_id TEXT NOT NULL,
			source_count INTEGER NOT NULL DEFAULT 1,
			duplicate_source_count INTEGER NOT NULL DEFAULT 0,
			is_favorite INTEGER NOT NULL DEFAULT 0,
			place_label TEXT,
			description TEXT,
			first_seen_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_canonical_visible_timeline
			ON catalog_canonical_assets(visibility_status, captured_at DESC, canonical_asset_id)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_canonical_media_timeline
			ON catalog_canonical_assets(visibility_status, media_type, captured_at DESC, canonical_asset_id)`,
		`CREATE TABLE IF NOT EXISTS catalog_canonical_state (
			singleton_id INTEGER PRIMARY KEY CHECK(singleton_id = 1),
			generation INTEGER NOT NULL CHECK(generation >= 0)
		)`,
		`INSERT INTO catalog_canonical_state(singleton_id, generation)
			VALUES (1, 0)
			ON CONFLICT(singleton_id) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS catalog_gallery_timeline_state (
			singleton_id INTEGER PRIMARY KEY CHECK(singleton_id = 1),
			generation INTEGER NOT NULL,
			canonical_generation INTEGER NOT NULL CHECK(canonical_generation >= 0),
			scope_key TEXT NOT NULL,
			total_count INTEGER NOT NULL CHECK(total_count >= 0),
			built_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS catalog_gallery_timeline (
			generation INTEGER NOT NULL,
			global_position INTEGER NOT NULL CHECK(global_position >= 0),
			canonical_asset_id TEXT NOT NULL,
			source_key TEXT NOT NULL,
			upstream_asset_id TEXT NOT NULL,
			media_type TEXT NOT NULL CHECK(media_type IN ('image', 'video')),
			filename TEXT NOT NULL,
			captured_at TEXT NOT NULL,
			duration TEXT,
			PRIMARY KEY(generation, global_position),
			UNIQUE(generation, canonical_asset_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_gallery_timeline_captured
			ON catalog_gallery_timeline(generation, captured_at DESC, canonical_asset_id, global_position)`,
		`CREATE INDEX IF NOT EXISTS idx_catalog_gallery_timeline_media_captured
			ON catalog_gallery_timeline(generation, media_type, captured_at DESC, canonical_asset_id)`,
		`CREATE TABLE IF NOT EXISTS local_assets (
			source_key TEXT NOT NULL,
			asset_id TEXT NOT NULL,
			sha1_hex TEXT NOT NULL,
			content_size_bytes INTEGER NOT NULL DEFAULT 0,
			media_type TEXT NOT NULL CHECK (media_type IN ('image', 'video')),
			filename TEXT NOT NULL,
			captured_at TEXT NOT NULL,
			captured_at_source TEXT NOT NULL,
			duration TEXT,
			width INTEGER,
			height INTEGER,
			primary_location_id INTEGER,
			visibility_status TEXT NOT NULL CHECK (visibility_status IN ('active', 'missing', 'permission_blocked', 'failed')),
			thumbnail_status TEXT NOT NULL,
			first_seen_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(source_key, asset_id),
			UNIQUE(source_key, sha1_hex, content_size_bytes)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_local_assets_visible_timeline
			ON local_assets(source_key, visibility_status, captured_at DESC, asset_id)`,
		`CREATE INDEX IF NOT EXISTS idx_local_assets_media_timeline
			ON local_assets(source_key, visibility_status, media_type, captured_at DESC, asset_id)`,
		`CREATE INDEX IF NOT EXISTS idx_local_assets_thumbnail_status
			ON local_assets(source_key, visibility_status, media_type, thumbnail_status)`,
		`CREATE INDEX IF NOT EXISTS idx_local_assets_thumbnail_status_all
			ON local_assets(visibility_status, media_type, thumbnail_status)`,
		`CREATE TABLE IF NOT EXISTS local_asset_locations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_key TEXT NOT NULL,
			asset_id TEXT,
			root_key TEXT NOT NULL,
			relative_path TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			mtime TEXT NOT NULL,
			fast_signature TEXT NOT NULL,
			file_identity TEXT NOT NULL DEFAULT '',
			sha1_hex TEXT,
			status TEXT NOT NULL CHECK (status IN ('discovered', 'active', 'missing', 'permission_blocked', 'failed')),
			status_reason TEXT,
			first_seen_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			verified_at TEXT,
			content_verified_at TEXT,
			content_verification_attempted_at TEXT,
			content_verification_error TEXT,
			metadata_not_before TEXT,
			superseded_at TEXT,
			updated_at TEXT NOT NULL,
			FOREIGN KEY(source_key, asset_id) REFERENCES local_assets(source_key, asset_id) ON DELETE CASCADE
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_local_asset_locations_current_path
			ON local_asset_locations(source_key, root_key, relative_path)
			WHERE status != 'missing'`,
		`CREATE INDEX IF NOT EXISTS idx_local_asset_locations_asset_status
			ON local_asset_locations(source_key, asset_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_local_asset_locations_scan
			ON local_asset_locations(source_key, root_key, status, relative_path)`,
		`CREATE INDEX IF NOT EXISTS idx_local_asset_locations_source_status
			ON local_asset_locations(source_key, status)`,
		`CREATE INDEX IF NOT EXISTS idx_local_asset_locations_metadata_due
			ON local_asset_locations(status, metadata_not_before, source_key, id)`,
		`CREATE INDEX IF NOT EXISTS idx_local_asset_locations_content_verification
			ON local_asset_locations(source_key, status, content_verification_attempted_at, id)
			WHERE asset_id IS NOT NULL AND sha1_hex IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS local_renditions (
			source_key TEXT NOT NULL,
			asset_id TEXT NOT NULL,
			kind TEXT NOT NULL CHECK (kind IN ('preview', 'detail_preview', 'video_frame')),
			status TEXT NOT NULL,
			relative_path TEXT,
			width INTEGER,
			height INTEGER,
			size_bytes INTEGER,
			content_sha256 TEXT,
			generated_at TEXT,
			source_sha1_hex TEXT,
			last_error TEXT,
			PRIMARY KEY(source_key, asset_id, kind),
			FOREIGN KEY(source_key, asset_id) REFERENCES local_assets(source_key, asset_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS local_scan_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_key TEXT NOT NULL,
			job_kind TEXT NOT NULL,
			priority INTEGER NOT NULL,
			root_key TEXT,
			root_generation INTEGER NOT NULL DEFAULT 1 CHECK (root_generation > 0),
			asset_id TEXT,
			location_id INTEGER,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			scheduled_at TEXT NOT NULL,
			sort_at TEXT NOT NULL DEFAULT '',
			locked_at TEXT,
			completed_at TEXT,
			last_error TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_jobs_ready
			ON local_scan_jobs(status, priority, sort_at DESC, scheduled_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_jobs_source_kind
			ON local_scan_jobs(source_key, job_kind, status)`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_jobs_source_status
			ON local_scan_jobs(source_key, status)`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_jobs_kind_status
			ON local_scan_jobs(job_kind, status)`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_jobs_metadata_ready
			ON local_scan_jobs(job_kind, status, root_generation, source_key, priority, sort_at DESC, id, location_id)
			WHERE location_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_jobs_source_metadata_ready
			ON local_scan_jobs(source_key, root_key, root_generation, job_kind, status, priority, sort_at DESC, id, location_id)
			WHERE location_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_jobs_thumbnail_ready
			ON local_scan_jobs(job_kind, status, root_generation, source_key, priority, sort_at DESC, scheduled_at, id, asset_id)
			WHERE asset_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_jobs_source_thumbnail_ready
			ON local_scan_jobs(source_key, root_key, root_generation, job_kind, status, priority, sort_at DESC, scheduled_at, id, asset_id)
			WHERE asset_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_jobs_location_pending
			ON local_scan_jobs(source_key, job_kind, location_id, status, root_generation)
			WHERE location_id IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS local_scan_root_state (
			source_key TEXT NOT NULL,
			root_key TEXT NOT NULL,
			root_status TEXT NOT NULL,
			root_last_checked_at TEXT,
			root_last_error TEXT,
			phase0_status TEXT NOT NULL,
			next_phase0_at TEXT,
			last_phase0_run_id INTEGER,
			last_quick_scan_at TEXT,
			last_reconciliation_at TEXT,
			last_content_verification_at TEXT,
			content_verification_scheduled_at TEXT,
			content_verification_window_started_at TEXT,
			content_verification_window_deadline_at TEXT,
			content_verification_status TEXT NOT NULL DEFAULT 'idle'
				CHECK (content_verification_status IN ('idle', 'running', 'completed', 'skipped')),
			content_verification_skip_reason TEXT,
			content_verification_processed_files INTEGER NOT NULL DEFAULT 0 CHECK (content_verification_processed_files >= 0),
			content_verification_verified_files INTEGER NOT NULL DEFAULT 0 CHECK (content_verification_verified_files >= 0),
			content_verification_changed_files INTEGER NOT NULL DEFAULT 0 CHECK (content_verification_changed_files >= 0),
			content_verification_failed_files INTEGER NOT NULL DEFAULT 0 CHECK (content_verification_failed_files >= 0),
			content_verification_read_bytes INTEGER NOT NULL DEFAULT 0 CHECK (content_verification_read_bytes >= 0),
			root_identity TEXT NOT NULL DEFAULT '',
			root_generation INTEGER NOT NULL DEFAULT 1 CHECK (root_generation > 0),
			reconciliation_pending INTEGER NOT NULL DEFAULT 0 CHECK (reconciliation_pending IN (0, 1)),
			updated_at TEXT NOT NULL,
			PRIMARY KEY(source_key, root_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_root_state_next_phase0
			ON local_scan_root_state(next_phase0_at)`,
		`CREATE TABLE IF NOT EXISTS local_scan_directories (
			source_key TEXT NOT NULL,
			root_key TEXT NOT NULL,
			relative_path TEXT NOT NULL,
			mtime TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(source_key, root_key, relative_path)
		)`,
		`CREATE TABLE IF NOT EXISTS local_scan_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_key TEXT NOT NULL,
			root_key TEXT NOT NULL,
			scan_mode TEXT NOT NULL DEFAULT 'reconciliation',
			started_at TEXT NOT NULL,
			completed_at TEXT,
			status TEXT NOT NULL,
			root_status_at_start TEXT NOT NULL,
			root_failure_reason TEXT,
			discovered_paths INTEGER NOT NULL DEFAULT 0,
			changed_paths INTEGER NOT NULL DEFAULT 0,
			queued_metadata INTEGER NOT NULL DEFAULT 0,
			missing_paths INTEGER NOT NULL DEFAULT 0,
			skipped_paths INTEGER NOT NULL DEFAULT 0,
			skip_counts_json TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_runs_source_started
			ON local_scan_runs(source_key, root_key, started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_runs_source_mode_started
			ON local_scan_runs(source_key, root_key, scan_mode, started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_runs_status
			ON local_scan_runs(status, source_key, root_key)`,
		`CREATE INDEX IF NOT EXISTS idx_local_scan_runs_reconciliation_completed
			ON local_scan_runs(source_key, root_key, scan_mode, status, completed_at DESC)`,
		`CREATE TABLE IF NOT EXISTS immich_mirror_state (
			source_key TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			latest_asset_limit INTEGER NOT NULL DEFAULT 0,
			last_full_sync_at TEXT,
			last_incremental_sync_at TEXT,
			last_error TEXT,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS semantic_vector_payload_batches (
			batch_id TEXT PRIMARY KEY,
			relative_path TEXT NOT NULL UNIQUE,
			size_bytes INTEGER NOT NULL,
			sha256 TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS semantic_backfill_scheduler_state (
			model_id TEXT NOT NULL,
			vector_space_id TEXT NOT NULL,
			next_source_key TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(model_id, vector_space_id)
		)`,
		`CREATE TABLE IF NOT EXISTS semantic_vector_payload_gc_candidates (
			batch_id TEXT PRIMARY KEY
		)`,
		`CREATE TABLE IF NOT EXISTS semantic_vectors (
			source_key TEXT NOT NULL,
			upstream_asset_id TEXT NOT NULL,
			model_id TEXT NOT NULL,
			vector_space_id TEXT NOT NULL,
			embedding_dim INTEGER NOT NULL,
			payload_batch_id TEXT,
			vector_offset INTEGER NOT NULL,
			vector_length INTEGER NOT NULL,
			embedding_input TEXT NOT NULL,
			status TEXT NOT NULL,
			last_error TEXT,
			generated_at TEXT NOT NULL,
			PRIMARY KEY(source_key, upstream_asset_id, model_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_semantic_vectors_model_status
			ON semantic_vectors(source_key, model_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_semantic_vectors_source_model_space_status
			ON semantic_vectors(source_key, model_id, vector_space_id, status, upstream_asset_id)`,
		`CREATE INDEX IF NOT EXISTS idx_semantic_vectors_payload_batch
			ON semantic_vectors(payload_batch_id)`,
		`CREATE TABLE IF NOT EXISTS semantic_state (
			source_key TEXT NOT NULL,
			model_id TEXT NOT NULL,
			vector_space_id TEXT NOT NULL,
			status TEXT NOT NULL,
			embedding_dim INTEGER NOT NULL,
			completed_vector_count INTEGER NOT NULL DEFAULT 0,
			indexed_vector_count INTEGER NOT NULL DEFAULT 0,
			asset_generation INTEGER NOT NULL DEFAULT 0,
			indexed_generation INTEGER NOT NULL DEFAULT -1,
			built_at TEXT,
			last_error TEXT,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(source_key, model_id)
		)`,
		`CREATE TABLE IF NOT EXISTS semantic_index_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_key TEXT NOT NULL,
			model_id TEXT NOT NULL,
			vector_space_id TEXT NOT NULL,
			embedding_dim INTEGER NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'failed', 'completed')),
			priority INTEGER NOT NULL DEFAULT 100,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			scheduled_at TEXT NOT NULL,
			started_at TEXT,
			lease_expires_at TEXT,
			completed_at TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(source_key, model_id, vector_space_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_semantic_index_jobs_ready
			ON semantic_index_jobs(status, priority, scheduled_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_semantic_index_jobs_source_model
			ON semantic_index_jobs(source_key, model_id, vector_space_id, status)`,
		`CREATE TABLE IF NOT EXISTS semantic_generation_suppression (
			source_key TEXT PRIMARY KEY
		)`,
	}
	return createCatalogSchema(context.Background(), s.db, statements)
}

func createCatalogSchema(ctx context.Context, db *sql.DB, statements []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin catalog schema creation: %w", err)
	}
	defer tx.Rollback()
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create catalog schema: %w", err)
		}
	}
	if err := createSemanticIndexGenerationTriggers(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id = %d", catalogApplicationID)); err != nil {
		return fmt.Errorf("write catalog application id: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", catalogSchemaVersion)); err != nil {
		return fmt.Errorf("write catalog schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit catalog schema creation: %w", err)
	}
	return nil
}

func (s *CatalogStore) ensureCatalogQueryIndexes(ctx context.Context) error {
	statements := []struct {
		name string
		sql  string
	}{
		{
			name: catalogGallerySourceCanonicalIndex,
			sql: `CREATE INDEX IF NOT EXISTS idx_catalog_assets_gallery_source_canonical
				ON catalog_assets(source_key, datasource_kind, visibility_status, canonical_asset_id)`,
		},
		{
			name: "idx_catalog_gallery_timeline_captured",
			sql: `CREATE INDEX IF NOT EXISTS idx_catalog_gallery_timeline_captured
				ON catalog_gallery_timeline(generation, captured_at DESC, canonical_asset_id, global_position)`,
		},
		{
			name: "idx_catalog_gallery_timeline_media_captured",
			sql: `CREATE INDEX IF NOT EXISTS idx_catalog_gallery_timeline_media_captured
				ON catalog_gallery_timeline(generation, media_type, captured_at DESC, canonical_asset_id)`,
		},
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement.sql); err != nil {
			return fmt.Errorf("ensure catalog query index %s: %w", statement.name, err)
		}
	}
	return nil
}

type catalogSchemaExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func createSemanticIndexGenerationTriggers(ctx context.Context, executor catalogSchemaExecutor) error {
	triggers := []string{
		`CREATE TRIGGER IF NOT EXISTS semantic_catalog_asset_insert_generation
		AFTER INSERT ON catalog_assets
		BEGIN
			UPDATE semantic_state SET asset_generation = asset_generation + 1
			WHERE source_key = NEW.source_key
				AND NOT EXISTS (SELECT 1 FROM semantic_generation_suppression WHERE source_key = NEW.source_key);
		END`,
		`CREATE TRIGGER IF NOT EXISTS semantic_catalog_asset_delete_generation
		AFTER DELETE ON catalog_assets
		BEGIN
			UPDATE semantic_state SET asset_generation = asset_generation + 1
			WHERE source_key = OLD.source_key
				AND NOT EXISTS (SELECT 1 FROM semantic_generation_suppression WHERE source_key = OLD.source_key);
		END`,
		`CREATE TRIGGER IF NOT EXISTS semantic_catalog_asset_update_generation
		AFTER UPDATE ON catalog_assets
		WHEN OLD.datasource_kind IS NOT NEW.datasource_kind
			OR OLD.media_type IS NOT NEW.media_type
			OR OLD.filename IS NOT NEW.filename
			OR OLD.captured_at IS NOT NEW.captured_at
			OR OLD.duration IS NOT NEW.duration
			OR OLD.visibility_status IS NOT NEW.visibility_status
			OR OLD.content_sha1_hex IS NOT NEW.content_sha1_hex
			OR OLD.content_size_bytes IS NOT NEW.content_size_bytes
		BEGIN
			INSERT OR IGNORE INTO semantic_vector_payload_gc_candidates(batch_id)
			SELECT payload_batch_id FROM semantic_vectors
			WHERE source_key = NEW.source_key
				AND upstream_asset_id = NEW.upstream_asset_id
				AND payload_batch_id IS NOT NULL
				AND (OLD.content_sha1_hex IS NOT NEW.content_sha1_hex
					OR OLD.content_size_bytes IS NOT NEW.content_size_bytes);
			UPDATE semantic_vectors
			SET payload_batch_id = NULL,
				vector_offset = 0,
				vector_length = 0,
				status = 'pending',
				last_error = NULL,
				generated_at = NEW.updated_at
			WHERE source_key = NEW.source_key
				AND upstream_asset_id = NEW.upstream_asset_id
				AND (OLD.content_sha1_hex IS NOT NEW.content_sha1_hex
					OR OLD.content_size_bytes IS NOT NEW.content_size_bytes);
			UPDATE semantic_state SET asset_generation = asset_generation + 1
			WHERE source_key = NEW.source_key
				AND NOT EXISTS (SELECT 1 FROM semantic_generation_suppression WHERE source_key = NEW.source_key);
		END`,
		`CREATE TRIGGER IF NOT EXISTS semantic_vector_insert_generation
		AFTER INSERT ON semantic_vectors
		BEGIN
			UPDATE semantic_state SET asset_generation = asset_generation + 1
			WHERE source_key = NEW.source_key AND model_id = NEW.model_id
				AND NOT EXISTS (SELECT 1 FROM semantic_generation_suppression WHERE source_key = NEW.source_key);
		END`,
		`CREATE TRIGGER IF NOT EXISTS semantic_vector_delete_generation
		AFTER DELETE ON semantic_vectors
		BEGIN
			UPDATE semantic_state SET asset_generation = asset_generation + 1
			WHERE source_key = OLD.source_key AND model_id = OLD.model_id
				AND NOT EXISTS (SELECT 1 FROM semantic_generation_suppression WHERE source_key = OLD.source_key);
		END`,
		`CREATE TRIGGER IF NOT EXISTS semantic_vector_update_generation
		AFTER UPDATE ON semantic_vectors
		WHEN OLD.vector_space_id IS NOT NEW.vector_space_id
			OR OLD.embedding_dim IS NOT NEW.embedding_dim
			OR OLD.payload_batch_id IS NOT NEW.payload_batch_id
			OR OLD.vector_offset IS NOT NEW.vector_offset
			OR OLD.vector_length IS NOT NEW.vector_length
			OR OLD.embedding_input IS NOT NEW.embedding_input
			OR OLD.status IS NOT NEW.status
			OR OLD.generated_at IS NOT NEW.generated_at
		BEGIN
			UPDATE semantic_state SET asset_generation = asset_generation + 1
			WHERE source_key = NEW.source_key AND model_id = NEW.model_id
				AND NOT EXISTS (SELECT 1 FROM semantic_generation_suppression WHERE source_key = NEW.source_key);
		END`,
		`CREATE TRIGGER IF NOT EXISTS semantic_local_rendition_insert_generation
		AFTER INSERT ON local_renditions
		BEGIN
			UPDATE semantic_state SET asset_generation = asset_generation + 1
			WHERE source_key = NEW.source_key
				AND NOT EXISTS (SELECT 1 FROM semantic_generation_suppression WHERE source_key = NEW.source_key);
		END`,
		`CREATE TRIGGER IF NOT EXISTS semantic_local_rendition_delete_generation
		AFTER DELETE ON local_renditions
		BEGIN
			UPDATE semantic_state SET asset_generation = asset_generation + 1
			WHERE source_key = OLD.source_key
				AND NOT EXISTS (SELECT 1 FROM semantic_generation_suppression WHERE source_key = OLD.source_key);
		END`,
		`CREATE TRIGGER IF NOT EXISTS semantic_local_rendition_update_generation
		AFTER UPDATE ON local_renditions
		WHEN OLD.kind IS NOT NEW.kind
			OR OLD.status IS NOT NEW.status
			OR OLD.relative_path IS NOT NEW.relative_path
			OR OLD.source_sha1_hex IS NOT NEW.source_sha1_hex
		BEGIN
			UPDATE semantic_state SET asset_generation = asset_generation + 1
			WHERE source_key = NEW.source_key
				AND NOT EXISTS (SELECT 1 FROM semantic_generation_suppression WHERE source_key = NEW.source_key);
		END`,
	}
	for _, statement := range triggers {
		if _, err := executor.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create semantic index generation trigger: %w", err)
		}
	}
	return nil
}

func (s *CatalogStore) tableColumns(table string) (map[string]bool, error) {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer rows.Close()

	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name string
		var columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return nil, fmt.Errorf("scan %s columns: %w", table, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s columns: %w", table, err)
	}
	return columns, nil
}

func (s *CatalogStore) indexColumns(index string) ([]string, error) {
	rows, err := s.db.Query("PRAGMA index_info(" + index + ")")
	if err != nil {
		return nil, fmt.Errorf("inspect %s columns: %w", index, err)
	}
	defer rows.Close()

	columns := []string{}
	for rows.Next() {
		var sequence int
		var columnID int
		var name string
		if err := rows.Scan(&sequence, &columnID, &name); err != nil {
			return nil, fmt.Errorf("scan %s columns: %w", index, err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s columns: %w", index, err)
	}
	return columns, nil
}

func hasStringPrefix(values []string, prefix []string) bool {
	if len(values) < len(prefix) {
		return false
	}
	for index := range prefix {
		if values[index] != prefix[index] {
			return false
		}
	}
	return true
}

func (s *CatalogStore) ReplaceFull(
	ctx context.Context,
	sourceKey string,
	assets []ImmichMirrorAsset,
	latestAssetLimit int,
	startedAt time.Time,
) (MirrorSyncResult, error) {
	if s == nil || s.db == nil {
		return MirrorSyncResult{}, ErrCatalogNotConfigured
	}
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" {
		return MirrorSyncResult{}, ErrCatalogNotConfigured
	}
	if latestAssetLimit < 0 {
		latestAssetLimit = 0
	}
	now := time.Now().UTC()
	result := MirrorSyncResult{
		Mode:             MirrorSyncModeFull,
		Status:           "ok",
		LatestAssetLimit: latestAssetLimit,
		FetchedCount:     len(assets),
		StartedAt:        startedAt.UTC(),
		CompletedAt:      now,
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MirrorSyncResult{}, fmt.Errorf("begin immich mirror sync: %w", err)
	}
	defer tx.Rollback()

	hiddenStatus := MirrorVisibilityMissing
	if latestAssetLimit > 0 {
		hiddenStatus = MirrorVisibilityOutOfScope
	}
	nowText := formatCatalogTime(now)
	if _, err = tx.ExecContext(ctx, `INSERT INTO semantic_generation_suppression(source_key)
		VALUES (?) ON CONFLICT(source_key) DO NOTHING`, sourceKey); err != nil {
		return MirrorSyncResult{}, fmt.Errorf("suppress per-row semantic generation: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.immich_full_stage`); err != nil {
		return MirrorSyncResult{}, fmt.Errorf("reset immich full-sync stage: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TEMP TABLE immich_full_stage (
		upstream_asset_id TEXT PRIMARY KEY,
		media_type TEXT NOT NULL,
		filename TEXT NOT NULL,
		captured_at TEXT NOT NULL,
		duration TEXT,
		source_updated_at TEXT,
		is_favorite INTEGER NOT NULL,
		content_sha1_hex TEXT,
		content_size_bytes INTEGER,
		place_label TEXT,
		description TEXT
	)`); err != nil {
		return MirrorSyncResult{}, fmt.Errorf("create immich full-sync stage: %w", err)
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO immich_full_stage (
			upstream_asset_id, media_type, filename, captured_at, duration, source_updated_at,
			is_favorite, content_sha1_hex, content_size_bytes, place_label, description
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(upstream_asset_id) DO UPDATE SET
			media_type = excluded.media_type,
			filename = excluded.filename,
			captured_at = excluded.captured_at,
			duration = excluded.duration,
			source_updated_at = excluded.source_updated_at,
			is_favorite = excluded.is_favorite,
			content_sha1_hex = excluded.content_sha1_hex,
			content_size_bytes = excluded.content_size_bytes,
			place_label = excluded.place_label,
			description = excluded.description`)
	if err != nil {
		return MirrorSyncResult{}, fmt.Errorf("prepare immich full-sync stage: %w", err)
	}
	defer statement.Close()

	for _, asset := range assets {
		if strings.TrimSpace(asset.UpstreamAssetID) == "" || asset.CapturedAt.IsZero() {
			continue
		}
		duration := sql.NullString{}
		if asset.Duration != nil {
			duration.Valid = true
			duration.String = *asset.Duration
		}
		updatedAt := sql.NullString{}
		if asset.SourceUpdatedAt != nil && !asset.SourceUpdatedAt.IsZero() {
			updatedAt.Valid = true
			updatedAt.String = formatCatalogTime(asset.SourceUpdatedAt.UTC())
		}
		_, err = statement.ExecContext(
			ctx,
			strings.TrimSpace(asset.UpstreamAssetID),
			normalizeMirrorMediaType(asset.MediaType),
			strings.TrimSpace(asset.Filename),
			formatCatalogTime(asset.CapturedAt.UTC()),
			duration,
			updatedAt,
			boolToSQLiteInt(asset.IsFavorite),
			nullableCatalogSHA1(asset.ContentSHA1Hex),
			nullablePositiveInt64(asset.ContentSizeBytes),
			nullableCatalogText(asset.PlaceLabel),
			nullableCatalogText(asset.Description),
		)
		if err != nil {
			return MirrorSyncResult{}, fmt.Errorf("stage immich mirror asset %q: %w", asset.UpstreamAssetID, err)
		}
	}
	if err = statement.Close(); err != nil {
		return MirrorSyncResult{}, fmt.Errorf("close immich full-sync stage: %w", err)
	}
	changedAssetIDs, semanticChanged, changedErr := immichFullSyncChangesInTx(ctx, tx, sourceKey, hiddenStatus)
	if changedErr != nil {
		return MirrorSyncResult{}, changedErr
	}
	if _, err = tx.ExecContext(ctx, `UPDATE catalog_assets
		SET visibility_status = ?, updated_at = ?
		WHERE source_key = ? AND datasource_kind = 'immich'
			AND visibility_status IS NOT ?
			AND NOT EXISTS (
				SELECT 1 FROM immich_full_stage stage
				WHERE stage.upstream_asset_id = catalog_assets.upstream_asset_id
			)`, hiddenStatus, nowText, sourceKey, hiddenStatus); err != nil {
		return MirrorSyncResult{}, fmt.Errorf("hide absent immich mirror rows: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename, captured_at, duration,
			visibility_status, source_updated_at, is_favorite, content_sha1_hex,
			content_size_bytes, place_label, description, first_seen_at, updated_at
		)
		SELECT ?, 'immich', upstream_asset_id, media_type, filename, captured_at, duration,
			'active', source_updated_at, is_favorite, content_sha1_hex,
			content_size_bytes, place_label, description, ?, ?
		FROM immich_full_stage
		WHERE 1
		ON CONFLICT(source_key, upstream_asset_id) DO UPDATE SET
			datasource_kind = excluded.datasource_kind,
			media_type = excluded.media_type,
			filename = excluded.filename,
			captured_at = excluded.captured_at,
			duration = excluded.duration,
			visibility_status = excluded.visibility_status,
			source_updated_at = excluded.source_updated_at,
			is_favorite = excluded.is_favorite,
			content_sha1_hex = excluded.content_sha1_hex,
			content_size_bytes = excluded.content_size_bytes,
			place_label = excluded.place_label,
			description = excluded.description,
			updated_at = excluded.updated_at
		WHERE catalog_assets.datasource_kind IS NOT excluded.datasource_kind
			OR catalog_assets.media_type IS NOT excluded.media_type
			OR catalog_assets.filename IS NOT excluded.filename
			OR catalog_assets.captured_at IS NOT excluded.captured_at
			OR catalog_assets.duration IS NOT excluded.duration
			OR catalog_assets.visibility_status IS NOT excluded.visibility_status
			OR catalog_assets.source_updated_at IS NOT excluded.source_updated_at
			OR catalog_assets.is_favorite IS NOT excluded.is_favorite
			OR catalog_assets.content_sha1_hex IS NOT excluded.content_sha1_hex
			OR catalog_assets.content_size_bytes IS NOT excluded.content_size_bytes
			OR catalog_assets.place_label IS NOT excluded.place_label
			OR catalog_assets.description IS NOT excluded.description`,
		sourceKey, nowText, nowText,
	); err != nil {
		return MirrorSyncResult{}, fmt.Errorf("apply immich full-sync stage: %w", err)
	}
	for _, upstreamAssetID := range changedAssetIDs {
		if err = s.refreshCatalogCanonicalAssetInTx(ctx, tx, sourceKey, upstreamAssetID, nowText); err != nil {
			return MirrorSyncResult{}, err
		}
	}
	if semanticChanged {
		if _, err = tx.ExecContext(ctx, `UPDATE semantic_state
			SET asset_generation = asset_generation + 1
			WHERE source_key = ?`, sourceKey); err != nil {
			return MirrorSyncResult{}, fmt.Errorf("advance full-sync semantic generation: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM semantic_generation_suppression WHERE source_key = ?`, sourceKey); err != nil {
		return MirrorSyncResult{}, fmt.Errorf("restore semantic generation triggers: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `DROP TABLE temp.immich_full_stage`); err != nil {
		return MirrorSyncResult{}, fmt.Errorf("drop immich full-sync stage: %w", err)
	}

	status, err := s.statusInTx(ctx, tx, sourceKey)
	if err != nil {
		return MirrorSyncResult{}, err
	}
	previousState, err := s.stateMetadataInTx(ctx, tx, sourceKey)
	if err != nil {
		return MirrorSyncResult{}, err
	}
	status.Enabled = true
	status.Status = "ok"
	status.LatestAssetLimit = latestAssetLimit
	status.LastFullSyncAt = &now
	status.LastIncrementalSyncAt = previousState.LastIncrementalSyncAt
	status.LastError = ""
	if err = s.upsertStateInTx(ctx, tx, sourceKey, status, now); err != nil {
		return MirrorSyncResult{}, err
	}
	if err = s.commitCatalogAssetChanges(ctx, tx, len(changedAssetIDs) > 0); err != nil {
		return MirrorSyncResult{}, fmt.Errorf("commit immich mirror sync: %w", err)
	}
	if cleanupErr := s.cleanupSemanticVectorPayloadCandidates(context.Background()); cleanupErr != nil {
		log.Printf("timich-agent semantic vector payload cleanup failed after full mirror source_key=%s error=%v", sourceKey, cleanupErr)
	}
	result.ActiveCount = status.ActiveCount
	result.OutOfScopeCount = status.OutOfScopeCount
	result.MissingCount = status.MissingCount
	result.Mirror = status
	return result, nil
}

func immichFullSyncChangesInTx(ctx context.Context, tx *sql.Tx, sourceKey string, hiddenStatus string) ([]string, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT upstream_asset_id, semantic_changed
		FROM (
			SELECT assets.upstream_asset_id,
				CASE
					WHEN stage.upstream_asset_id IS NULL THEN 1
					WHEN assets.datasource_kind IS NOT 'immich'
						OR assets.media_type IS NOT stage.media_type
						OR assets.filename IS NOT stage.filename
						OR assets.captured_at IS NOT stage.captured_at
						OR assets.duration IS NOT stage.duration
						OR assets.visibility_status IS NOT 'active'
						OR assets.content_sha1_hex IS NOT stage.content_sha1_hex
						OR assets.content_size_bytes IS NOT stage.content_size_bytes
					THEN 1
					ELSE 0
				END AS semantic_changed
			FROM catalog_assets assets
			LEFT JOIN immich_full_stage stage
				ON stage.upstream_asset_id = assets.upstream_asset_id
			WHERE assets.source_key = ?
				AND (
					(stage.upstream_asset_id IS NULL
						AND assets.datasource_kind = 'immich'
						AND assets.visibility_status IS NOT ?)
					OR (
						stage.upstream_asset_id IS NOT NULL
						AND (
							assets.datasource_kind IS NOT 'immich'
							OR assets.media_type IS NOT stage.media_type
							OR assets.filename IS NOT stage.filename
							OR assets.captured_at IS NOT stage.captured_at
							OR assets.duration IS NOT stage.duration
							OR assets.visibility_status IS NOT 'active'
							OR assets.source_updated_at IS NOT stage.source_updated_at
							OR assets.is_favorite IS NOT stage.is_favorite
							OR assets.content_sha1_hex IS NOT stage.content_sha1_hex
							OR assets.content_size_bytes IS NOT stage.content_size_bytes
							OR assets.place_label IS NOT stage.place_label
							OR assets.description IS NOT stage.description
						)
					)
				)
			UNION ALL
			SELECT stage.upstream_asset_id, 1
			FROM immich_full_stage stage
			LEFT JOIN catalog_assets assets
				ON assets.source_key = ? AND assets.upstream_asset_id = stage.upstream_asset_id
			WHERE assets.upstream_asset_id IS NULL
		)
		ORDER BY upstream_asset_id`, sourceKey, hiddenStatus, sourceKey)
	if err != nil {
		return nil, false, fmt.Errorf("query immich full-sync changes: %w", err)
	}
	defer rows.Close()

	changedAssetIDs := []string{}
	semanticChanged := false
	for rows.Next() {
		var upstreamAssetID string
		var semanticChangedValue int
		if err := rows.Scan(&upstreamAssetID, &semanticChangedValue); err != nil {
			return nil, false, fmt.Errorf("scan immich full-sync change: %w", err)
		}
		changedAssetIDs = append(changedAssetIDs, upstreamAssetID)
		semanticChanged = semanticChanged || semanticChangedValue != 0
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate immich full-sync changes: %w", err)
	}
	return changedAssetIDs, semanticChanged, nil
}

func (s *CatalogStore) MergeIncremental(
	ctx context.Context,
	sourceKey string,
	assets []ImmichMirrorAsset,
	startedAt time.Time,
) (MirrorSyncResult, error) {
	if s == nil || s.db == nil {
		return MirrorSyncResult{}, ErrCatalogNotConfigured
	}
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" {
		return MirrorSyncResult{}, ErrCatalogNotConfigured
	}
	now := time.Now().UTC()
	result := MirrorSyncResult{
		Mode:         MirrorSyncModeIncremental,
		Status:       "ok",
		FetchedCount: len(assets),
		StartedAt:    startedAt.UTC(),
		CompletedAt:  now,
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MirrorSyncResult{}, fmt.Errorf("begin immich mirror incremental sync: %w", err)
	}
	defer tx.Rollback()

	statement, err := tx.PrepareContext(ctx, `INSERT INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename, captured_at, duration,
			visibility_status, source_updated_at, is_favorite, content_sha1_hex,
			content_size_bytes, place_label, description,
			first_seen_at, updated_at
		) VALUES (?, 'immich', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_key, upstream_asset_id) DO UPDATE SET
			datasource_kind = excluded.datasource_kind,
			media_type = excluded.media_type,
			filename = excluded.filename,
			captured_at = excluded.captured_at,
			duration = excluded.duration,
			visibility_status = excluded.visibility_status,
			source_updated_at = excluded.source_updated_at,
			is_favorite = excluded.is_favorite,
			content_sha1_hex = excluded.content_sha1_hex,
			content_size_bytes = excluded.content_size_bytes,
			place_label = excluded.place_label,
			description = excluded.description,
			updated_at = excluded.updated_at
		WHERE catalog_assets.datasource_kind IS NOT excluded.datasource_kind
			OR catalog_assets.media_type IS NOT excluded.media_type
			OR catalog_assets.filename IS NOT excluded.filename
			OR catalog_assets.captured_at IS NOT excluded.captured_at
			OR catalog_assets.duration IS NOT excluded.duration
			OR catalog_assets.visibility_status IS NOT excluded.visibility_status
			OR catalog_assets.source_updated_at IS NOT excluded.source_updated_at
			OR catalog_assets.is_favorite IS NOT excluded.is_favorite
			OR catalog_assets.content_sha1_hex IS NOT excluded.content_sha1_hex
			OR catalog_assets.content_size_bytes IS NOT excluded.content_size_bytes
			OR catalog_assets.place_label IS NOT excluded.place_label
			OR catalog_assets.description IS NOT excluded.description`)
	if err != nil {
		return MirrorSyncResult{}, fmt.Errorf("prepare immich mirror incremental upsert: %w", err)
	}
	defer statement.Close()

	nowText := formatCatalogTime(now)
	upstreamAssetIDs := make([]string, 0, len(assets))
	seenUpstreamAssetIDs := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		upstreamAssetID := strings.TrimSpace(asset.UpstreamAssetID)
		if upstreamAssetID == "" || asset.CapturedAt.IsZero() {
			continue
		}
		duration := sql.NullString{}
		if asset.Duration != nil {
			duration.Valid = true
			duration.String = *asset.Duration
		}
		updatedAt := sql.NullString{}
		if asset.SourceUpdatedAt != nil && !asset.SourceUpdatedAt.IsZero() {
			updatedAt.Valid = true
			updatedAt.String = formatCatalogTime(asset.SourceUpdatedAt.UTC())
		}
		sqlResult, execErr := statement.ExecContext(
			ctx,
			sourceKey,
			upstreamAssetID,
			normalizeMirrorMediaType(asset.MediaType),
			strings.TrimSpace(asset.Filename),
			formatCatalogTime(asset.CapturedAt.UTC()),
			duration,
			MirrorVisibilityActive,
			updatedAt,
			boolToSQLiteInt(asset.IsFavorite),
			nullableCatalogSHA1(asset.ContentSHA1Hex),
			nullablePositiveInt64(asset.ContentSizeBytes),
			nullableCatalogText(asset.PlaceLabel),
			nullableCatalogText(asset.Description),
			nowText,
			nowText,
		)
		if execErr != nil {
			return MirrorSyncResult{}, fmt.Errorf("upsert immich mirror incremental asset %q: %w", asset.UpstreamAssetID, execErr)
		}
		rowsAffected, rowsErr := sqlResult.RowsAffected()
		if rowsErr != nil {
			return MirrorSyncResult{}, fmt.Errorf("inspect immich mirror incremental asset %q: %w", asset.UpstreamAssetID, rowsErr)
		}
		if rowsAffected == 0 {
			continue
		}
		if _, ok := seenUpstreamAssetIDs[upstreamAssetID]; !ok {
			seenUpstreamAssetIDs[upstreamAssetID] = struct{}{}
			upstreamAssetIDs = append(upstreamAssetIDs, upstreamAssetID)
		}
	}

	status, err := s.statusInTx(ctx, tx, sourceKey)
	if err != nil {
		return MirrorSyncResult{}, err
	}
	previousState, err := s.stateMetadataInTx(ctx, tx, sourceKey)
	if err != nil {
		return MirrorSyncResult{}, err
	}
	status.Enabled = true
	status.Status = "ok"
	status.LatestAssetLimit = previousState.LatestAssetLimit
	status.LastFullSyncAt = previousState.LastFullSyncAt
	status.LastIncrementalSyncAt = &now
	status.LastError = ""
	if err = s.upsertStateInTx(ctx, tx, sourceKey, status, now); err != nil {
		return MirrorSyncResult{}, err
	}
	for _, upstreamAssetID := range upstreamAssetIDs {
		if err = s.refreshCatalogCanonicalAssetInTx(ctx, tx, sourceKey, upstreamAssetID, nowText); err != nil {
			return MirrorSyncResult{}, err
		}
	}
	if err = s.commitCatalogAssetChanges(ctx, tx, len(upstreamAssetIDs) > 0); err != nil {
		return MirrorSyncResult{}, fmt.Errorf("commit immich mirror incremental sync: %w", err)
	}
	if cleanupErr := s.cleanupSemanticVectorPayloadCandidates(context.Background()); cleanupErr != nil {
		log.Printf("timich-agent semantic vector payload cleanup failed after incremental mirror source_key=%s error=%v", sourceKey, cleanupErr)
	}
	result.ActiveCount = status.ActiveCount
	result.OutOfScopeCount = status.OutOfScopeCount
	result.MissingCount = status.MissingCount
	result.Mirror = status
	return result, nil
}

func boolToSQLiteInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullableCatalogText(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func nullableCatalogSHA1(value string) any {
	value = normalizeCatalogSHA1Hex(value)
	if value == "" {
		return nil
	}
	return value
}

func nullablePositiveInt64(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

func (s *CatalogStore) Status(ctx context.Context, sourceKey string) (MirrorStatus, error) {
	if s == nil || s.db == nil {
		return MirrorStatus{}, ErrCatalogNotConfigured
	}
	status, err := s.status(ctx, strings.TrimSpace(sourceKey))
	if err != nil {
		return MirrorStatus{}, err
	}
	status.Enabled = true
	return status, nil
}

func (s *CatalogStore) status(ctx context.Context, sourceKey string) (MirrorStatus, error) {
	status, err := s.statusCounts(ctx, sourceKey)
	if err != nil {
		return MirrorStatus{}, err
	}
	status.Semantic = CatalogSemanticStatus{Status: "missing"}
	db := s.queryDB()
	var latestLimit int
	var stateStatus string
	var lastFull sql.NullString
	var lastIncremental sql.NullString
	var lastError sql.NullString
	err = db.QueryRowContext(ctx, `SELECT status, latest_asset_limit, last_full_sync_at, last_incremental_sync_at, last_error
		FROM immich_mirror_state WHERE source_key = ?`, sourceKey).
		Scan(&stateStatus, &latestLimit, &lastFull, &lastIncremental, &lastError)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return MirrorStatus{}, fmt.Errorf("read immich mirror state: %w", err)
	}
	if err == nil {
		status.Status = stateStatus
		status.LatestAssetLimit = latestLimit
		if lastFull.Valid {
			parsed, parseErr := time.Parse(time.RFC3339Nano, lastFull.String)
			if parseErr == nil {
				status.LastFullSyncAt = &parsed
			}
		}
		if lastIncremental.Valid {
			parsed, parseErr := time.Parse(time.RFC3339Nano, lastIncremental.String)
			if parseErr == nil {
				status.LastIncrementalSyncAt = &parsed
			}
		}
		if lastError.Valid {
			status.LastError = lastError.String
		}
	}
	return status, nil
}

func (s *CatalogStore) LatestSourceUpdatedAt(ctx context.Context, sourceKey string) (*time.Time, error) {
	if s == nil || s.db == nil {
		return nil, ErrCatalogNotConfigured
	}
	db := s.queryDB()
	var latest sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT MAX(source_updated_at)
		FROM catalog_assets
		WHERE source_key = ? AND datasource_kind = 'immich' AND source_updated_at IS NOT NULL`, strings.TrimSpace(sourceKey)).
		Scan(&latest); err != nil {
		return nil, fmt.Errorf("read immich mirror latest source update: %w", err)
	}
	if !latest.Valid {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, latest.String)
	if err != nil {
		return nil, fmt.Errorf("parse immich mirror latest source update: %w", err)
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func (s *CatalogStore) stateMetadataInTx(ctx context.Context, tx *sql.Tx, sourceKey string) (MirrorStatus, error) {
	var status MirrorStatus
	var lastFull sql.NullString
	var lastIncremental sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT latest_asset_limit, last_full_sync_at, last_incremental_sync_at
		FROM immich_mirror_state WHERE source_key = ?`, sourceKey).
		Scan(&status.LatestAssetLimit, &lastFull, &lastIncremental)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return status, nil
		}
		return MirrorStatus{}, fmt.Errorf("read immich mirror state metadata: %w", err)
	}
	if lastFull.Valid {
		if parsed, parseErr := time.Parse(time.RFC3339Nano, lastFull.String); parseErr == nil {
			status.LastFullSyncAt = &parsed
		}
	}
	if lastIncremental.Valid {
		if parsed, parseErr := time.Parse(time.RFC3339Nano, lastIncremental.String); parseErr == nil {
			status.LastIncrementalSyncAt = &parsed
		}
	}
	return status, nil
}

func (s *CatalogStore) statusInTx(ctx context.Context, tx *sql.Tx, sourceKey string) (MirrorStatus, error) {
	status := MirrorStatus{}
	rows, err := tx.QueryContext(ctx, `SELECT visibility_status, COUNT(*)
		FROM catalog_assets WHERE source_key = ? AND datasource_kind = 'immich' GROUP BY visibility_status`, sourceKey)
	if err != nil {
		return MirrorStatus{}, fmt.Errorf("count immich mirror status: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var visibility string
		var count int
		if err := rows.Scan(&visibility, &count); err != nil {
			return MirrorStatus{}, fmt.Errorf("scan immich mirror status: %w", err)
		}
		switch visibility {
		case MirrorVisibilityActive:
			status.ActiveCount = count
		case MirrorVisibilityOutOfScope:
			status.OutOfScopeCount = count
		case MirrorVisibilityMissing:
			status.MissingCount = count
		}
	}
	if err := rows.Err(); err != nil {
		return MirrorStatus{}, fmt.Errorf("iterate immich mirror status: %w", err)
	}
	return status, nil
}

func (s *CatalogStore) statusCounts(ctx context.Context, sourceKey string) (MirrorStatus, error) {
	db := s.queryDB()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MirrorStatus{}, fmt.Errorf("begin immich mirror status: %w", err)
	}
	defer tx.Rollback()
	return s.statusInTx(ctx, tx, sourceKey)
}

func (s *CatalogStore) upsertStateInTx(ctx context.Context, tx *sql.Tx, sourceKey string, status MirrorStatus, now time.Time) error {
	var lastFull any
	if status.LastFullSyncAt != nil {
		lastFull = formatCatalogTime(status.LastFullSyncAt.UTC())
	}
	var lastIncremental any
	if status.LastIncrementalSyncAt != nil {
		lastIncremental = formatCatalogTime(status.LastIncrementalSyncAt.UTC())
	}
	var lastError any
	if strings.TrimSpace(status.LastError) != "" {
		lastError = status.LastError
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO immich_mirror_state (
			source_key, status, latest_asset_limit, last_full_sync_at,
			last_incremental_sync_at, last_error, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_key) DO UPDATE SET
			status = excluded.status,
			latest_asset_limit = excluded.latest_asset_limit,
			last_full_sync_at = excluded.last_full_sync_at,
			last_incremental_sync_at = excluded.last_incremental_sync_at,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at`,
		sourceKey,
		status.Status,
		status.LatestAssetLimit,
		lastFull,
		lastIncremental,
		lastError,
		formatCatalogTime(now.UTC()),
	)
	if err != nil {
		return fmt.Errorf("upsert immich mirror state: %w", err)
	}
	return nil
}

func normalizeMirrorMediaType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "video":
		return "video"
	default:
		return "image"
	}
}

func formatCatalogTime(value time.Time) string {
	// SQLite compares the persisted timestamps as TEXT. RFC3339Nano trims
	// trailing fractional zeroes, so two chronologically ordered values such as
	// .184090 and .184091 can sort in the opposite order as strings. Keep the
	// nanosecond field fixed-width so lexical and chronological ordering agree.
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func escapeCatalogLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	value = strings.ReplaceAll(value, `_`, `\_`)
	return value
}
