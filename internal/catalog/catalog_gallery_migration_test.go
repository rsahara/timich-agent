package catalog

import (
	"context"
	"crypto/sha256"
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

const galleryV4TableForTest = `CREATE TABLE catalog_gallery_projection (
	canonical_asset_id TEXT PRIMARY KEY, source_key TEXT NOT NULL,
	upstream_asset_id TEXT NOT NULL, media_type TEXT NOT NULL CHECK(media_type IN ('image','video')),
	filename TEXT NOT NULL, captured_at TEXT NOT NULL, duration TEXT)`

// Reconstruct the actual V4 table, not just its user_version marker.
func downgradeGalleryV4ForTest(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`SELECT name, sql FROM sqlite_schema
		WHERE type = 'trigger' AND instr(lower(sql), 'catalog_gallery_projection') > 0`)
	if err != nil {
		t.Fatal(err)
	}
	triggers := map[string]string{}
	for rows.Next() {
		var name, text string
		if err := rows.Scan(&name, &text); err != nil {
			t.Fatal(err)
		}
		triggers[name] = text
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for name := range triggers {
		if _, err := db.Exec(`DROP TRIGGER "` + name + `"`); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		`CREATE TEMP TABLE gallery_saved AS SELECT * FROM catalog_gallery_projection`,
		`DROP TABLE catalog_gallery_projection`, galleryV4TableForTest,
		`INSERT INTO catalog_gallery_projection SELECT * FROM gallery_saved`,
		`DROP TABLE gallery_saved`,
		`CREATE INDEX idx_catalog_gallery_projection_captured ON catalog_gallery_projection(captured_at DESC, canonical_asset_id)`,
		`CREATE INDEX idx_catalog_gallery_projection_media_captured ON catalog_gallery_projection(media_type, captured_at DESC, canonical_asset_id)`,
		`PRAGMA user_version = 4`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range triggers {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
}

func newV4GalleryFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := LoadOrCreateCatalogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.setStandaloneGalleryReadiness(catalogGalleryReadiness{
		localSourceKeys: []string{"2222222222222222"}, immichSourceKeys: []string{"1111111111111111"},
	})
	assets := make([]ImmichMirrorAsset, 80)
	for i := range assets {
		assets[i] = ImmichMirrorAsset{UpstreamAssetID: fmt.Sprintf("a-%03d", i), MediaType: "image",
			Filename: fmt.Sprintf("写真 %03d.jpg", i), CapturedAt: time.Date(2026, 9, 1, 12, 0, i/20, 0, time.UTC)}
		if i%7 == 0 {
			value := ""
			assets[i].Duration = &value
			assets[i].MediaType = "video"
		}
	}
	if _, err := store.ReplaceFull(context.Background(), "1111111111111111", assets, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	downgradeGalleryV4ForTest(t, store.db)
	path := store.Path()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func galleryRowsForMigrationTest(t *testing.T, db *sql.DB) [][]sql.NullString {
	t.Helper()
	rows, err := db.Query(`SELECT canonical_asset_id, ` + galleryProjectionSelectColumns + `
		FROM catalog_gallery_projection ORDER BY captured_at DESC, canonical_asset_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result [][]sql.NullString
	for rows.Next() {
		values := make([]sql.NullString, 7)
		if err := rows.Scan(&values[0], &values[1], &values[2], &values[3], &values[4], &values[5], &values[6]); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestGalleryMigrationV4ToV5PreservesRowsAndTriggers(t *testing.T) {
	dir, source := newV4GalleryFixture(t)
	ctx := context.Background()
	old, err := openCatalogMigrationDB(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	want := galleryRowsForMigrationTest(t, old)
	if len(want) != 80 {
		t.Fatalf("fixture rows = %d", len(want))
	}
	// Non-Gallery data and an explicit rowid must survive without VACUUM.
	if _, err := old.Exec(`CREATE TABLE migration_sentinel(payload BLOB);
		INSERT INTO migration_sentinel(rowid,payload) VALUES(999,x'00ff1234')`); err != nil {
		t.Fatal(err)
	}
	_ = old.Close()
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(filepath.Dir(source), "migrated-v5.db")
	result, err := MigratePreReleaseCatalogV4ToV5(ctx, source, output)
	if err != nil {
		t.Fatal(err)
	}
	if result.FromVersion != 4 || result.ToVersion != 5 || result.GalleryRows != 80 || result.OutputPath != output {
		t.Fatalf("%+v", result)
	}
	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("source database changed")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(output + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("output sidecar %s: %v", suffix, err)
		}
	}
	db, err := openCatalogMigrationDB(ctx, output)
	if err != nil {
		t.Fatal(err)
	}
	if got := galleryRowsForMigrationTest(t, db); !reflect.DeepEqual(got, want) {
		t.Fatal("returned fields, nulls or ordering changed")
	}
	assertGallerySeekIndex(t, db)
	var sentinel string
	if err := db.QueryRow(`SELECT hex(payload) FROM migration_sentinel WHERE rowid=999`).Scan(&sentinel); err != nil || sentinel != "00FF1234" {
		t.Fatalf("sentinel = %s, %v", sentinel, err)
	}
	assertGalleryProjectionDayIndexMatches(t, db)
	if err := validateGalleryStorageSchema(ctx, db, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE catalog_canonical_assets SET captured_at='2026-09-03T00:00:00Z', filename='changed.jpg'
		WHERE canonical_asset_id=?`, want[0][0].String); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE catalog_assets SET visibility_status='missing' WHERE source_key=? AND upstream_asset_id=?`, want[1][1].String, want[1][2].String); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM catalog_gallery_projection WHERE filename='changed.jpg'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("updated rows = %d, %v", count, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM catalog_gallery_projection`).Scan(&count); err != nil || count != 79 {
		t.Fatalf("active rows = %d, %v", count, err)
	}
	assertGalleryProjectionDayIndexMatches(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Exercise real startup only after manually installing the new file in this fixture.
	if err := os.Rename(source, source+".v4-backup"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(output, source); err != nil {
		t.Fatal(err)
	}
	current, err := LoadOrCreateCatalogStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if err := validateGalleryStorageSchema(ctx, current.db, true); err != nil {
		t.Fatal(err)
	}
	assertGallerySeekIndex(t, current.db)
}

func TestGalleryMigrationV4ToV5PreservesSemanticStateAndRecovery(t *testing.T) {
	for _, missingMembership := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_membership=%t", missingMembership), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store, err := LoadOrCreateCatalogStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			const sourceKey = "1111111111111111"
			profile := testImageSemanticProfile{}
			builtAt := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
			assets := []semanticAsset{
				{SourceKey: sourceKey, ID: "asset-a", MediaType: "image", Filename: "Kyoto morning.jpg", CapturedAt: builtAt, Vector: []float32{1, 0, 0, 0}},
				{SourceKey: sourceKey, ID: "asset-b", MediaType: "image", Filename: "Tokyo evening.jpg", CapturedAt: builtAt.Add(-time.Second), Vector: []float32{0, 1, 0, 0}},
			}
			seedAndWriteSemanticBinaryIndexForTest(t, store, ctx, sourceKey, profile, assets, builtAt, 5)
			// The source-index fixture inserts source rows directly. Populate the
			// canonical projection as well before checking preservation of both FTS tables.
			if _, err := store.RebuildCatalogCanonicalAssets(ctx); err != nil {
				t.Fatal(err)
			}
			manifestPath := store.semanticBinaryActiveManifestPath(sourceKey, profile)
			binaryPath := store.semanticBinaryIndexPath(sourceKey, profile, 5)
			manifest, err := readSemanticBinaryActiveManifest(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			readMembership := func(db *sql.DB) []string {
				t.Helper()
				rows, err := db.Query(`SELECT upstream_asset_id, ordinal FROM semantic_index_membership
					WHERE source_key=? AND model_id=? AND vector_space_id=? AND asset_generation=5 ORDER BY ordinal`,
					sourceKey, profile.ModelID(), profile.VectorSpaceID())
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var result []string
				for rows.Next() {
					var id string
					var ordinal int
					if err := rows.Scan(&id, &ordinal); err != nil {
						t.Fatal(err)
					}
					result = append(result, fmt.Sprintf("%d:%s", ordinal, id))
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				return result
			}
			wantMembership := readMembership(store.db)
			if len(wantMembership) != len(assets) {
				t.Fatalf("fixture membership = %v", wantMembership)
			}
			downgradeGalleryV4ForTest(t, store.db)
			if missingMembership {
				if _, err := store.db.Exec(`DELETE FROM semantic_index_membership`); err != nil {
					t.Fatal(err)
				}
			}
			source := store.Path()
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(filepath.Dir(source), "semantic-v5.db")
			if _, err := MigratePreReleaseCatalogV4ToV5(ctx, source, output); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(source)
			if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatalf("V4 source changed: %v", err)
			}
			db, err := openCatalogMigrationDB(ctx, output)
			if err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"catalog_assets_metadata_fts", "catalog_canonical_metadata_fts"} {
				var count int
				if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE ` + table + ` MATCH '"Kyoto"'`).Scan(&count); err != nil || count != 1 {
					t.Fatalf("%s Kyoto matches = %d, %v", table, count, err)
				}
			}
			var vectorCount int
			if err := db.QueryRow(`SELECT COUNT(*) FROM semantic_vectors`).Scan(&vectorCount); err != nil || vectorCount != len(assets) {
				t.Fatalf("V5 vectors = %d, %v", vectorCount, err)
			}
			gotMembership := readMembership(db)
			if missingMembership {
				if len(gotMembership) != 0 {
					t.Fatal("offline Gallery migration must not rebuild unrelated semantic state")
				}
			} else if !reflect.DeepEqual(gotMembership, wantMembership) {
				t.Fatalf("V5 membership changed: %v", gotMembership)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			// Simulate the operator's stopped-Agent cutover. Recovery belongs to
			// current V5 startup, not to a historical schema compatibility path.
			if err := os.Rename(source, source+".v4-backup"); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(output, source); err != nil {
				t.Fatal(err)
			}
			current, err := LoadOrCreateCatalogStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer current.Close()
			if got := readMembership(current.db); !reflect.DeepEqual(got, wantMembership) {
				t.Fatalf("V5 startup membership = %v, want %v", got, wantMembership)
			}
			if matches, err := current.semanticBinaryMembershipMatches(ctx, manifest); err != nil || !matches {
				t.Fatalf("V5 membership fingerprint mismatch: %v", err)
			}
			if got, err := readSemanticBinaryActiveManifest(manifestPath); err != nil || got != manifest {
				t.Fatalf("active manifest changed: %v", err)
			}
			if _, _, digest, err := inspectSemanticBinaryIndexFile(ctx, binaryPath, true); err != nil || digest != manifest.FileSHA256 {
				t.Fatalf("published binary changed: %v", err)
			}
		})
	}
}

func TestGalleryMigrationV4ToV5RejectsUnsafeInputs(t *testing.T) {
	for _, name := range []string{"same-path", "existing-output", "sidecar", "symlink", "wrong-version", "already-v5", "wrong-app", "wrong-layout", "missing-trigger", "unknown-trigger", "view", "foreign-key", "null-id", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			_, source := newV4GalleryFixture(t)
			output := filepath.Join(filepath.Dir(source), "out.db")
			ctx := context.Background()
			var mutate string
			switch name {
			case "same-path":
				output = source
			case "existing-output":
				if err := os.WriteFile(output, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "sidecar":
				if err := os.WriteFile(output+"-wal", []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(source, output); err != nil {
					t.Fatal(err)
				}
			case "wrong-version":
				mutate = `PRAGMA user_version=3`
			case "already-v5":
				mutate = `PRAGMA user_version=5`
			case "wrong-app":
				mutate = `PRAGMA application_id=1`
			case "wrong-layout":
				mutate = `ALTER TABLE catalog_gallery_projection ADD COLUMN extra TEXT`
			case "missing-trigger":
				mutate = `DROP TRIGGER trg_catalog_gallery_projection_source_update_v2`
			case "unknown-trigger":
				mutate = `CREATE TRIGGER custom_gallery AFTER INSERT ON catalog_gallery_projection BEGIN SELECT 1; END`
			case "view":
				mutate = `CREATE VIEW custom_gallery AS SELECT * FROM catalog_gallery_projection`
			case "foreign-key":
				mutate = `CREATE TABLE custom_gallery(id TEXT REFERENCES catalog_gallery_projection(canonical_asset_id) ON DELETE CASCADE);
					INSERT INTO custom_gallery SELECT canonical_asset_id FROM catalog_gallery_projection`
			case "null-id":
				// V4 permits this invalid identity. Fail during the transactional
				// copy, after triggers have been detached, without publishing it.
				mutate = `INSERT INTO catalog_gallery_projection SELECT NULL,source_key,upstream_asset_id,media_type,filename,captured_at,duration FROM catalog_gallery_projection LIMIT 1`
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if mutate != "" {
				db, err := openCatalogMigrationDB(ctx, source)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(mutate); err != nil {
					t.Fatal(err)
				}
				_ = db.Close()
			}
			before, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			_, err = MigratePreReleaseCatalogV4ToV5(ctx, source, output)
			if err == nil {
				t.Fatal("expected refusal")
			}
			if name == "null-id" && !strings.Contains(err.Error(), "copy Gallery rows") {
				t.Fatalf("did not exercise transactional copy failure: %v", err)
			}
			stages, globErr := filepath.Glob(filepath.Join(filepath.Dir(output), ".timich-gallery-v5-*"))
			if globErr != nil || len(stages) != 0 {
				t.Fatalf("failed migration left staging files: %v %v", stages, globErr)
			}
			after, readErr := os.ReadFile(source)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatal("source changed on failure")
			}
			if name != "same-path" && name != "existing-output" && name != "symlink" {
				if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("output published on failure: %v", err)
				}
			}
			if name == "existing-output" {
				data, _ := os.ReadFile(output)
				if string(data) != "keep" {
					t.Fatal("overwrote output")
				}
			}
		})
	}
}

func TestGalleryMigrationKeepsCommittedWALAndRefusesRepeat(t *testing.T) {
	_, source := newV4GalleryFixture(t)
	db, err := openCatalogMigrationDB(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;
		UPDATE catalog_gallery_projection SET filename='committed-wal.jpg'`); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(filepath.Dir(source), "out.db")
	if _, err := MigratePreReleaseCatalogV4ToV5(context.Background(), source, output); err != nil {
		t.Fatal(err)
	}
	copy, err := openCatalogMigrationDB(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var count int
	if err := copy.QueryRow(`SELECT COUNT(*) FROM catalog_gallery_projection WHERE filename='committed-wal.jpg'`).Scan(&count); err != nil || count != 80 {
		t.Fatalf("WAL rows = %d, %v", count, err)
	}
	if _, err := MigratePreReleaseCatalogV4ToV5(context.Background(), source, output); err == nil {
		t.Fatal("repeat overwrote output")
	}
	if _, err := MigratePreReleaseCatalogV4ToV5(context.Background(), output, output+".again"); err == nil {
		t.Fatal("V5 accepted as V4")
	}
}

func TestGalleryV4StartupRequiresManualMigrationWithoutReset(t *testing.T) {
	dir, source := newV4GalleryFixture(t)
	_, err := LoadOrCreateCatalogStore(dir)
	if !errors.Is(err, ErrCatalogMigrationRequired) || errors.Is(err, ErrCatalogSchemaResetRequired) || strings.Contains(err.Error(), "remove the catalog") {
		t.Fatalf("unexpected V4 startup result: %v", err)
	}
	db, err := openCatalogMigrationDB(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	version, _, err := catalogMigrationIdentity(context.Background(), db)
	if err != nil || version != 4 {
		t.Fatalf("V4 was modified: %d %v", version, err)
	}
	if _, err := db.Exec(`PRAGMA user_version=5`); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCatalogStore(dir); err == nil {
		t.Fatal("V5 marker alone accepted old physical layout")
	}
}
