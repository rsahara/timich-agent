package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
)

type CatalogGalleryMigrationResult struct {
	FromVersion int    `json:"fromVersion"`
	ToVersion   int    `json:"toVersion"`
	OutputPath  string `json:"outputPath"`
	GalleryRows int64  `json:"galleryRows"`
}

// MigratePreReleaseCatalogV4ToV5 publishes a verified standalone V5 copy at a
// new path. It never replaces the source, starts workers, or changes external
// payload files. Stop all Agent writers before taking the cutover copy; backups,
// Admin state, old binaries, and the eventual file switch are operator-owned.
func MigratePreReleaseCatalogV4ToV5(ctx context.Context, sourcePath, outputPath string) (CatalogGalleryMigrationResult, error) {
	var result CatalogGalleryMigrationResult
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(sourcePath) == "" || strings.TrimSpace(outputPath) == "" {
		return result, errors.New("source and output paths are required")
	}
	sourcePath, err := filepath.Abs(sourcePath)
	if err != nil {
		return result, err
	}
	outputPath, err = filepath.Abs(outputPath)
	if err != nil {
		return result, err
	}
	if sourcePath == outputPath {
		return result, errors.New("output must not replace the source")
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, errors.New("source must be a regular database file")
	}
	for _, path := range []string{outputPath, outputPath + "-wal", outputPath + "-shm", outputPath + "-journal"} {
		if _, err := os.Lstat(path); err == nil {
			return result, fmt.Errorf("output or sidecar already exists: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	sourceURL := url.URL{Scheme: "file", Path: filepath.ToSlash(sourcePath), RawQuery: "mode=ro"}
	source, err := sql.Open("sqlite", sourceURL.String())
	if err != nil {
		return result, err
	}
	source.SetMaxOpenConns(1)
	defer source.Close()
	version, appID, err := catalogMigrationIdentity(ctx, source)
	if err != nil {
		return result, err
	}
	if version != 4 || appID != catalogApplicationID {
		return result, fmt.Errorf("migration requires Timich catalog V4; found version %d application id %#x", version, appID)
	}
	if err := validateGalleryStorageSchema(ctx, source, false); err != nil {
		return result, err
	}
	// Work and publication are on the same volume. The requested output remains
	// absent on failure; hard-link publication refuses overwrite atomically.
	stage, err := os.MkdirTemp(filepath.Dir(outputPath), ".timich-gallery-v5-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(stage)
	working := filepath.Join(stage, "catalog.db")
	if err := copyCatalogSnapshot(ctx, source, working); err != nil {
		return result, fmt.Errorf("copy V4 snapshot: %w", err)
	}
	db, err := openCatalogMigrationDB(ctx, working)
	if err != nil {
		return result, err
	}
	defer db.Close()
	version, appID, err = catalogMigrationIdentity(ctx, db)
	if err != nil {
		return result, err
	}
	if version != 4 || appID != catalogApplicationID {
		return result, errors.New("copied catalog identity changed")
	}
	if err := validateGalleryStorageSchema(ctx, db, false); err != nil {
		return result, err
	}
	if err := checkGalleryMigrationIntegrity(ctx, db); err != nil {
		return result, err
	}
	// No other connection sees this private copy. DELETE mode produces one
	// self-contained output file with no WAL/SHM to accidentally misapply.
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = DELETE`); err != nil {
		return result, err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA synchronous = FULL`); err != nil {
		return result, err
	}
	count, err := migrateGalleryStorage(ctx, db)
	if err != nil {
		return result, err
	}
	if err := validateGalleryStorageSchema(ctx, db, true); err != nil {
		return result, err
	}
	if err := checkGalleryMigrationIntegrity(ctx, db); err != nil {
		return result, err
	}
	if err := db.Close(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	file, err := os.OpenFile(working, os.O_RDWR, 0)
	if err != nil {
		return result, err
	}
	err = file.Chmod(0o600)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return result, err
	}
	if closeErr != nil {
		return result, closeErr
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := os.Link(working, outputPath); err != nil {
		return result, fmt.Errorf("publish V5 without overwrite: %w", err)
	}
	if err := syncDirectory(filepath.Dir(outputPath)); err != nil {
		return result, fmt.Errorf("V5 output exists but directory sync failed; preserve and inspect it: %w", err)
	}
	return CatalogGalleryMigrationResult{FromVersion: 4, ToVersion: 5, OutputPath: outputPath, GalleryRows: count}, nil
}

// SQLite's backup API includes committed WAL pages without compacting or
// renumbering unrelated rowid tables. Each step is bounded for cancellation.
func copyCatalogSnapshot(ctx context.Context, source *sql.DB, output string) error {
	conn, err := source.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(raw any) (resultErr error) {
		backuper, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite driver has no backup support")
		}
		backup, err := backuper.NewBackup(output)
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, backup.Finish()) }()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := backup.Step(256)
			if err != nil {
				return err
			}
			if !more {
				return nil
			}
		}
	})
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
	for _, statement := range []string{`PRAGMA foreign_keys = ON`, `PRAGMA busy_timeout = 5000`} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("prepare catalog migration: %w", err)
		}
	}
	return db, nil
}

func catalogMigrationIdentity(ctx context.Context, db *sql.DB) (int, int, error) {
	var version, applicationID int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return 0, 0, fmt.Errorf("read migration source schema version: %w", err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&applicationID); err != nil {
		return 0, 0, fmt.Errorf("read migration source application id: %w", err)
	}
	return version, applicationID, nil
}

func checkGalleryMigrationIntegrity(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var message string
		if err := rows.Scan(&message); err != nil {
			_ = rows.Close()
			return err
		}
		if message != "ok" {
			_ = rows.Close()
			return fmt.Errorf("catalog integrity check: %s", message)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	rows, err = db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("catalog foreign key check failed")
	}
	return rows.Err()
}

func galleryMigrationTriggerNames() map[string]bool {
	names := make(map[string]bool)
	for _, table := range []string{"canonical", "source", "local_asset", "rendition"} {
		for _, event := range []string{"insert", "update", "delete"} {
			names["trg_catalog_gallery_projection_"+table+"_"+event+"_v2"] = true
		}
	}
	for _, event := range []string{"insert", "delete", "update"} {
		names["trg_catalog_gallery_projection_day_"+event+"_v1"] = true
	}
	return names
}

func migrateGalleryStorage(ctx context.Context, db *sql.DB) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Recreating a table with live referring triggers makes ALTER TABLE fail,
	// or rewrites references if the old table is renamed first. Detach all
	// known referring triggers, copy, drop old, rename new, then restore.
	rows, err := tx.QueryContext(ctx, `SELECT name, sql FROM sqlite_schema
		WHERE type = 'trigger' AND instr(lower(sql), 'catalog_gallery_projection') > 0 ORDER BY name`)
	if err != nil {
		return 0, err
	}
	names := galleryMigrationTriggerNames()
	type trigger struct{ name, sql string }
	var triggers []trigger
	for rows.Next() {
		var tr trigger
		if err := rows.Scan(&tr.name, &tr.sql); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if !names[tr.name] {
			_ = rows.Close()
			return 0, fmt.Errorf("unsupported Gallery trigger %q", tr.name)
		}
		delete(names, tr.name)
		triggers = append(triggers, tr)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	if len(names) != 0 {
		return 0, errors.New("V4 catalog is missing Gallery maintenance triggers")
	}
	var views int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'view'
		AND instr(lower(sql), 'catalog_gallery_projection') > 0`).Scan(&views); err != nil {
		return 0, err
	}
	if views != 0 {
		return 0, errors.New("unsupported view references Gallery projection")
	}
	// DROP TABLE can invoke ON DELETE CASCADE. A customized schema must not
	// silently lose dependent rows even if the final foreign_key_check passes.
	var incomingForeignKeys int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema s
		JOIN pragma_foreign_key_list(s.name) f
		WHERE s.type='table' AND lower(f."table")='catalog_gallery_projection'`).Scan(&incomingForeignKeys); err != nil {
		return 0, err
	}
	if incomingForeignKeys != 0 {
		return 0, errors.New("unsupported foreign key references Gallery projection")
	}
	for _, tr := range triggers {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER "`+tr.name+`"`); err != nil {
			return 0, err
		}
	}
	create := strings.Replace(galleryProjectionTableSQL, "IF NOT EXISTS catalog_gallery_projection", "catalog_gallery_projection_v5", 1)
	if _, err := tx.ExecContext(ctx, create); err != nil {
		return 0, err
	}
	const columns = "canonical_asset_id, " + galleryProjectionSelectColumns
	if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_gallery_projection_v5 (`+columns+`)
		SELECT `+columns+` FROM catalog_gallery_projection`); err != nil {
		return 0, fmt.Errorf("copy Gallery rows: %w", err)
	}
	for _, pair := range [][2]string{{"catalog_gallery_projection", "catalog_gallery_projection_v5"}, {"catalog_gallery_projection_v5", "catalog_gallery_projection"}} {
		var mismatch int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT `+columns+` FROM `+pair[0]+
			` EXCEPT SELECT `+columns+` FROM `+pair[1]+`)`).Scan(&mismatch); err != nil {
			return 0, err
		}
		if mismatch != 0 {
			return 0, errors.New("Gallery row identity changed during migration")
		}
	}
	for _, statement := range []string{
		`DROP TABLE catalog_gallery_projection`,
		`ALTER TABLE catalog_gallery_projection_v5 RENAME TO catalog_gallery_projection`,
		galleryProjectionSeekIndexSQL,
		`CREATE INDEX idx_catalog_gallery_projection_media_captured ON catalog_gallery_projection(media_type, captured_at DESC, canonical_asset_id)`,
		`DELETE FROM catalog_gallery_projection_days`,
		`INSERT INTO catalog_gallery_projection_days(captured_day, item_count)
		 SELECT substr(captured_at, 1, 10), COUNT(*) FROM catalog_gallery_projection GROUP BY substr(captured_at, 1, 10)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return 0, fmt.Errorf("replace Gallery layout: %w", err)
		}
	}
	for _, tr := range triggers {
		if _, err := tx.ExecContext(ctx, tr.sql); err != nil {
			return 0, fmt.Errorf("restore Gallery trigger %s: %w", tr.name, err)
		}
	}
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_gallery_projection`).Scan(&count); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 5`); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}
