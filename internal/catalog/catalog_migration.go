package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const catalogPreReleaseMigrationFromVersion = 2

type CatalogPreReleaseMigrationResult struct {
	FromVersion                 int    `json:"fromVersion"`
	ToVersion                   int    `json:"toVersion"`
	BackupPath                  string `json:"backupPath,omitempty"`
	AlreadyCurrent              bool   `json:"alreadyCurrent"`
	CatalogAssetCount           int    `json:"catalogAssetCount"`
	ActiveSemanticManifestCount int    `json:"activeSemanticManifestCount"`
	SemanticMembershipCount     int    `json:"semanticMembershipCount"`
}

// MigratePreReleaseCatalogV2ToV3 performs the one supported pre-release catalog
// migration. Timich Agent must be stopped. The migration preserves source and
// canonical assets, vector payload metadata, and semantic binary files while it
// rebuilds only the derived metadata-search and binary-membership projections.
func MigratePreReleaseCatalogV2ToV3(ctx context.Context, dataDir string, backupPath string) (CatalogPreReleaseMigrationResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	dataDir = strings.TrimSpace(dataDir)
	backupPath = strings.TrimSpace(backupPath)
	if dataDir == "" {
		return CatalogPreReleaseMigrationResult{}, errors.New("data directory is required")
	}
	root := filepath.Join(dataDir, catalogStateDirName)
	databasePath := filepath.Join(root, catalogDatabaseName)
	db, err := openCatalogMigrationDB(ctx, databasePath)
	if err != nil {
		return CatalogPreReleaseMigrationResult{}, err
	}
	version, applicationID, err := catalogMigrationIdentity(ctx, db)
	if err != nil {
		_ = db.Close()
		return CatalogPreReleaseMigrationResult{}, err
	}
	if applicationID != catalogApplicationID {
		_ = db.Close()
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("catalog application id %#x is not Timich %#x", applicationID, catalogApplicationID)
	}
	if version == catalogSchemaVersion {
		if err := db.Close(); err != nil {
			return CatalogPreReleaseMigrationResult{}, err
		}
		return inspectMigratedCatalog(ctx, dataDir, "", true)
	}
	if version != catalogPreReleaseMigrationFromVersion {
		_ = db.Close()
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("catalog schema version %d cannot be migrated; want pre-release version %d", version, catalogPreReleaseMigrationFromVersion)
	}
	if backupPath == "" {
		_ = db.Close()
		return CatalogPreReleaseMigrationResult{}, errors.New("backup path is required for the V2 to V3 migration")
	}
	var quickCheck string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&quickCheck); err != nil || quickCheck != "ok" {
		_ = db.Close()
		if err != nil {
			return CatalogPreReleaseMigrationResult{}, fmt.Errorf("check catalog before migration: %w", err)
		}
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("catalog quick check failed: %s", quickCheck)
	}
	var busy, logFrames, checkpointedFrames int
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		_ = db.Close()
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("checkpoint catalog before migration: %w", err)
	}
	if busy != 0 {
		_ = db.Close()
		return CatalogPreReleaseMigrationResult{}, errors.New("catalog checkpoint is busy; stop Timich Agent before migration")
	}
	if err := db.Close(); err != nil {
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("close catalog before backup: %w", err)
	}
	if err := copyCatalogMigrationBackup(databasePath, backupPath); err != nil {
		return CatalogPreReleaseMigrationResult{}, err
	}

	db, err = openCatalogMigrationDB(ctx, databasePath)
	if err != nil {
		return CatalogPreReleaseMigrationResult{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("begin catalog V2 to V3 migration: %w", err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS catalog_assets_metadata_fts_insert`,
		`DROP TRIGGER IF EXISTS catalog_assets_metadata_fts_delete`,
		`DROP TRIGGER IF EXISTS catalog_assets_metadata_fts_update`,
		`DROP TABLE IF EXISTS catalog_assets_metadata_fts`,
		`DROP TABLE IF EXISTS semantic_index_membership`,
		`DROP TABLE IF EXISTS semantic_index_membership_state`,
		`DROP INDEX IF EXISTS idx_semantic_index_membership_generation_ordinal`,
		`DROP INDEX IF EXISTS idx_catalog_assets_metadata_favorite`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return CatalogPreReleaseMigrationResult{}, fmt.Errorf("clear pre-release search projection: %w", err)
		}
	}
	for _, statement := range catalogSearchProjectionSchemaStatements() {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return CatalogPreReleaseMigrationResult{}, fmt.Errorf("create V3 search projection: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_assets_metadata_fts(catalog_assets_metadata_fts) VALUES ('rebuild')`); err != nil {
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("rebuild V3 metadata search projection: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", catalogSchemaVersion)); err != nil {
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("write V3 catalog schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("commit catalog V2 to V3 migration: %w", err)
	}
	if err := db.Close(); err != nil {
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("close migrated catalog: %w", err)
	}
	return inspectMigratedCatalog(ctx, dataDir, backupPath, false)
}

func openCatalogMigrationDB(ctx context.Context, path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open catalog migration source %q: %w", path, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open catalog migration source: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("prepare catalog migration: %w", err)
		}
	}
	return db, nil
}

func catalogMigrationIdentity(ctx context.Context, db *sql.DB) (int, int, error) {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return 0, 0, fmt.Errorf("read migration source schema version: %w", err)
	}
	var applicationID int
	if err := db.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&applicationID); err != nil {
		return 0, 0, fmt.Errorf("read migration source application id: %w", err)
	}
	return version, applicationID, nil
}

func copyCatalogMigrationBackup(sourcePath string, backupPath string) error {
	if err := os.MkdirAll(filepath.Dir(backupPath), 0o700); err != nil {
		return fmt.Errorf("create catalog migration backup directory: %w", err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open catalog migration backup source: %w", err)
	}
	defer source.Close()
	target, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create catalog migration backup: %w", err)
	}
	keep := false
	defer func() {
		_ = target.Close()
		if !keep {
			_ = os.Remove(backupPath)
		}
	}()
	if _, err := io.Copy(target, source); err != nil {
		return fmt.Errorf("copy catalog migration backup: %w", err)
	}
	if err := target.Sync(); err != nil {
		return fmt.Errorf("sync catalog migration backup: %w", err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close catalog migration backup: %w", err)
	}
	if err := syncDirectory(filepath.Dir(backupPath)); err != nil {
		return fmt.Errorf("sync catalog migration backup directory: %w", err)
	}
	keep = true
	return nil
}

func inspectMigratedCatalog(ctx context.Context, dataDir string, backupPath string, alreadyCurrent bool) (CatalogPreReleaseMigrationResult, error) {
	store, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("open migrated catalog: %w", err)
	}
	defer store.Close()
	result := CatalogPreReleaseMigrationResult{
		FromVersion:    catalogPreReleaseMigrationFromVersion,
		ToVersion:      catalogSchemaVersion,
		BackupPath:     backupPath,
		AlreadyCurrent: alreadyCurrent,
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_assets`).Scan(&result.CatalogAssetCount); err != nil {
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("count migrated catalog assets: %w", err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM semantic_index_membership`).Scan(&result.SemanticMembershipCount); err != nil {
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("count migrated semantic membership: %w", err)
	}
	root := filepath.Join(store.root, semanticBinaryIndexDirName)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return CatalogPreReleaseMigrationResult{}, fmt.Errorf("inspect migrated semantic manifests: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".active.json") {
			continue
		}
		manifest, err := readSemanticBinaryActiveManifest(filepath.Join(root, entry.Name()))
		if err != nil {
			return CatalogPreReleaseMigrationResult{}, fmt.Errorf("read migrated semantic manifest %q: %w", entry.Name(), err)
		}
		matches, err := store.semanticBinaryMembershipMatches(ctx, manifest)
		if err != nil {
			return CatalogPreReleaseMigrationResult{}, err
		}
		if !matches {
			return CatalogPreReleaseMigrationResult{}, fmt.Errorf("semantic membership does not match active manifest %q", entry.Name())
		}
		result.ActiveSemanticManifestCount++
	}
	return result, nil
}
