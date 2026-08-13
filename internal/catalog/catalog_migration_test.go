package catalog

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestMigratePreReleaseCatalogV2ToV3PreservesSemanticState(t *testing.T) {
	dataDir := t.TempDir()
	store, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	ctx := context.Background()
	const sourceKey = "1111111111111111"
	profile := testImageSemanticProfile{}
	builtAt := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	assets := []semanticAsset{
		{SourceKey: sourceKey, ID: "asset-a", MediaType: "image", Filename: "Kyoto morning.jpg", CapturedAt: builtAt, Vector: []float32{1, 0, 0, 0}},
		{SourceKey: sourceKey, ID: "asset-b", MediaType: "image", Filename: "Tokyo evening.jpg", CapturedAt: builtAt.Add(-time.Second), Vector: []float32{0, 1, 0, 0}},
	}
	seedAndWriteSemanticBinaryIndexForTest(t, store, ctx, sourceKey, profile, assets, builtAt, 5)
	manifestPath := store.semanticBinaryActiveManifestPath(sourceKey, profile)
	manifestBefore, err := readSemanticBinaryActiveManifest(manifestPath)
	if err != nil {
		t.Fatalf("read active manifest before migration: %v", err)
	}
	databasePath := store.Path()
	if err := store.Close(); err != nil {
		t.Fatalf("close V3 fixture: %v", err)
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open catalog for V2 fixture downgrade: %v", err)
	}
	for _, statement := range []string{
		`PRAGMA foreign_keys = OFF`,
		`DROP TRIGGER IF EXISTS catalog_assets_metadata_fts_insert`,
		`DROP TRIGGER IF EXISTS catalog_assets_metadata_fts_delete`,
		`DROP TRIGGER IF EXISTS catalog_assets_metadata_fts_update`,
		`DROP TABLE IF EXISTS catalog_assets_metadata_fts`,
		`DROP TABLE IF EXISTS semantic_index_membership`,
		`DROP TABLE IF EXISTS semantic_index_membership_state`,
		`DROP INDEX IF EXISTS idx_catalog_assets_metadata_favorite`,
		`PRAGMA user_version = 2`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			t.Fatalf("prepare V2 migration fixture: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close V2 migration fixture: %v", err)
	}
	if _, err := LoadOrCreateCatalogStore(dataDir); err == nil {
		t.Fatal("LoadOrCreateCatalogStore(V2) error = nil, want explicit offline migration requirement")
	}

	backupPath := filepath.Join(t.TempDir(), "catalog-v2.db")
	result, err := MigratePreReleaseCatalogV2ToV3(ctx, dataDir, backupPath)
	if err != nil {
		t.Fatalf("MigratePreReleaseCatalogV2ToV3() error = %v", err)
	}
	if result.FromVersion != 2 || result.ToVersion != 3 || result.AlreadyCurrent ||
		result.BackupPath != backupPath || result.CatalogAssetCount != len(assets) ||
		result.ActiveSemanticManifestCount != 1 || result.SemanticMembershipCount != len(assets) {
		t.Fatalf("migration result = %#v", result)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("migration backup stat: %v", err)
	}
	backupDB, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatalf("open migration backup: %v", err)
	}
	var backupVersion int
	if err := backupDB.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&backupVersion); err != nil {
		_ = backupDB.Close()
		t.Fatalf("read migration backup version: %v", err)
	}
	if err := backupDB.Close(); err != nil {
		t.Fatalf("close migration backup: %v", err)
	}
	if backupVersion != 2 {
		t.Fatalf("migration backup version = %d, want 2", backupVersion)
	}

	migrated, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("open migrated catalog: %v", err)
	}
	defer migrated.Close()
	var kyotoMatches int
	if err := migrated.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM catalog_assets_metadata_fts
		WHERE catalog_assets_metadata_fts MATCH '"Kyoto"'`).Scan(&kyotoMatches); err != nil {
		t.Fatalf("query migrated metadata index: %v", err)
	}
	if kyotoMatches != 1 {
		t.Fatalf("migrated Kyoto matches = %d, want 1", kyotoMatches)
	}
	manifestAfter, err := readSemanticBinaryActiveManifest(manifestPath)
	if err != nil {
		t.Fatalf("read active manifest after migration: %v", err)
	}
	if manifestAfter != manifestBefore {
		t.Fatalf("active manifest changed during migration: before=%#v after=%#v", manifestBefore, manifestAfter)
	}
	var stateDigest string
	if err := migrated.db.QueryRowContext(ctx, `SELECT binary_sha256
		FROM semantic_index_membership_state
		WHERE source_key = ? AND model_id = ? AND vector_space_id = ? AND asset_generation = 5`,
		sourceKey,
		profile.ModelID(),
		profile.VectorSpaceID(),
	).Scan(&stateDigest); err != nil {
		t.Fatalf("read migrated membership identity: %v", err)
	}
	if stateDigest != manifestBefore.FileSHA256 {
		t.Fatalf("migrated membership digest = %q, want %q", stateDigest, manifestBefore.FileSHA256)
	}
	var vectorCount int
	if err := migrated.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM semantic_vectors`).Scan(&vectorCount); err != nil {
		t.Fatalf("count preserved semantic vectors: %v", err)
	}
	if vectorCount != len(assets) {
		t.Fatalf("preserved semantic vectors = %d, want %d", vectorCount, len(assets))
	}

	current, err := MigratePreReleaseCatalogV2ToV3(ctx, dataDir, "")
	if err != nil {
		t.Fatalf("repeat current migration check: %v", err)
	}
	if !current.AlreadyCurrent || current.ToVersion != 3 {
		t.Fatalf("current migration result = %#v", current)
	}
}
