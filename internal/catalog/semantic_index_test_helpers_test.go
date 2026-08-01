package catalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func seedAndWriteSemanticBinaryIndexForTest(
	t *testing.T,
	catalogStore *CatalogStore,
	ctx context.Context,
	sourceKey string,
	profile semanticEmbeddingProfile,
	assets []semanticAsset,
	builtAt time.Time,
	generation int64,
) {
	t.Helper()
	nowText := formatCatalogTime(builtAt)
	tx, err := catalogStore.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin semantic binary fixture transaction: %v", err)
	}
	statement, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO catalog_assets (
			source_key, datasource_kind, upstream_asset_id, media_type, filename,
			captured_at, duration, visibility_status, source_updated_at, is_favorite,
			content_sha1_hex, content_size_bytes, place_label, description, first_seen_at,
			updated_at
		) VALUES (?, 'immich', ?, ?, ?, ?, ?, 'active', ?, 0, ?, ?, NULL, NULL, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare semantic binary fixture asset insert: %v", err)
	}
	for index, asset := range assets {
		var duration any
		if asset.Duration != nil {
			duration = *asset.Duration
		}
		if _, err := statement.ExecContext(
			ctx,
			sourceKey,
			asset.ID,
			asset.MediaType,
			asset.Filename,
			formatCatalogTime(asset.CapturedAt),
			duration,
			nowText,
			fmt.Sprintf("%040x", index+1),
			int64(10_000+index),
			nowText,
			nowText,
		); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			t.Fatalf("insert semantic binary fixture asset %q: %v", asset.ID, err)
		}
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close semantic binary fixture asset insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit semantic binary fixture transaction: %v", err)
	}
	if len(assets) > 0 {
		if err := catalogStore.upsertSemanticVectors(ctx, sourceKey, profile, assets, builtAt); err != nil {
			t.Fatalf("upsert semantic binary fixture vectors: %v", err)
		}
	}
	writeSemanticBinaryIndexWithBuilderForTest(t, catalogStore, ctx, sourceKey, profile, builtAt, generation)
}

func writeSemanticBinaryIndexWithBuilderForTest(
	t *testing.T,
	catalogStore *CatalogStore,
	ctx context.Context,
	sourceKey string,
	profile semanticEmbeddingProfile,
	builtAt time.Time,
	generation int64,
) {
	t.Helper()
	root := filepath.Join(catalogStore.root, semanticBinaryIndexDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create semantic binary fixture directory: %v", err)
	}
	path := filepath.Join(root, fmt.Sprintf("%s.g%d.test.build.db", catalogStore.semanticBinaryIndexBaseName(sourceKey, profile), generation))
	removeSemanticIndexBuilderFiles(path)
	builder, err := openSemanticIndexBuilder(catalogStore, path, sourceKey, profile, generation, nil)
	if err != nil {
		t.Fatalf("open semantic binary fixture builder: %v", err)
	}
	defer builder.Remove()
	if err := builder.populate(ctx); err != nil {
		t.Fatalf("populate semantic binary fixture builder: %v", err)
	}
	if err := builder.WriteBinary(ctx, builtAt); err != nil {
		t.Fatalf("write semantic binary fixture: %v", err)
	}
	if err := catalogStore.activateSemanticBinaryIndex(ctx, sourceKey, profile, generation); err != nil {
		t.Fatalf("activate semantic binary fixture: %v", err)
	}
}

func TestSemanticIndexBuilderRollsBackEdgesWithCheckpoint(t *testing.T) {
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	const sourceKey = "1111111111111111"
	profile := testImageSemanticProfile{}
	assets := []semanticAsset{
		{SourceKey: sourceKey, ID: "asset-a", MediaType: "image", Filename: "asset-a.jpg", CapturedAt: time.Unix(2, 0).UTC(), Vector: []float32{1, 0, 0, 0}},
		{SourceKey: sourceKey, ID: "asset-b", MediaType: "image", Filename: "asset-b.jpg", CapturedAt: time.Unix(1, 0).UTC(), Vector: []float32{0, 1, 0, 0}},
	}
	seedAndWriteSemanticBinaryIndexForTest(t, store, ctx, sourceKey, profile, assets, time.Now().UTC(), 0)

	root := filepath.Join(store.root, semanticBinaryIndexDirName)
	path := filepath.Join(root, store.semanticBinaryIndexBaseName(sourceKey, profile)+".atomicity.build.db")
	builder, err := openSemanticIndexBuilder(store, path, sourceKey, profile, 1, nil)
	if err != nil {
		t.Fatalf("openSemanticIndexBuilder() error = %v", err)
	}
	defer builder.Remove()
	if err := builder.populate(ctx); err != nil {
		t.Fatalf("populate() error = %v", err)
	}
	if _, err := builder.db.ExecContext(ctx, `CREATE TRIGGER fail_next_ordinal
		BEFORE UPDATE OF value ON build_meta
		WHEN OLD.key = 'next_ordinal'
		BEGIN
			SELECT RAISE(ABORT, 'checkpoint failure');
		END`); err != nil {
		t.Fatalf("create checkpoint failure trigger: %v", err)
	}
	if err := builder.Build(ctx); err == nil {
		t.Fatal("Build() error = nil, want checkpoint failure")
	}
	var edgeCount int
	if err := builder.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM build_edges`).Scan(&edgeCount); err != nil {
		t.Fatalf("count edges after rollback: %v", err)
	}
	next, err := builder.metaInt(ctx, builder.db, "next_ordinal")
	if err != nil {
		t.Fatalf("read next ordinal after rollback: %v", err)
	}
	entry, err := builder.metaInt(ctx, builder.db, "entry_ordinal")
	if err != nil {
		t.Fatalf("read entry ordinal after rollback: %v", err)
	}
	if edgeCount != 0 || next != 0 || entry != -1 {
		t.Fatalf("rolled-back builder state edges=%d next=%d entry=%d, want 0/0/-1", edgeCount, next, entry)
	}
	if _, err := builder.db.ExecContext(ctx, `DROP TRIGGER fail_next_ordinal`); err != nil {
		t.Fatalf("drop checkpoint failure trigger: %v", err)
	}
	if err := builder.Build(ctx); err != nil {
		t.Fatalf("Build() after rollback error = %v", err)
	}
	next, err = builder.metaInt(ctx, builder.db, "next_ordinal")
	if err != nil {
		t.Fatalf("read completed next ordinal: %v", err)
	}
	if next != len(assets) {
		t.Fatalf("completed next ordinal = %d, want %d", next, len(assets))
	}
}

func TestPrepareSemanticIndexBuilderRecreatesCorruptDerivedDatabase(t *testing.T) {
	t.Parallel()

	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	const sourceKey = "1111111111111111"
	profile := testImageSemanticProfile{}
	assets := []semanticAsset{{
		SourceKey: sourceKey,
		ID:        "asset-a", MediaType: "image", Filename: "asset-a.jpg",
		CapturedAt: time.Unix(1, 0).UTC(), Vector: []float32{1, 0, 0, 0},
	}}
	seedAndWriteSemanticBinaryIndexForTest(t, store, ctx, sourceKey, profile, assets, time.Now().UTC(), 0)
	root := filepath.Join(store.root, semanticBinaryIndexDirName)
	path := filepath.Join(root, fmt.Sprintf("%s.g1.build.db", store.semanticBinaryIndexBaseName(sourceKey, profile)))
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt builder) error = %v", err)
	}
	job := &semanticIndexJob{SourceKey: sourceKey, AssetGeneration: 1}
	builder, err := store.prepareSemanticIndexBuilder(ctx, job, profile)
	if err != nil {
		t.Fatalf("prepareSemanticIndexBuilder() error = %v", err)
	}
	defer builder.Remove()
	var nodeCount int
	if err := builder.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM build_nodes`).Scan(&nodeCount); err != nil {
		t.Fatalf("count rebuilt nodes: %v", err)
	}
	if nodeCount != 1 {
		t.Fatalf("rebuilt node count = %d, want 1", nodeCount)
	}
}

func TestCatalogStartupRemovesOnlyKnownSemanticCrashTemps(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	stateRoot := filepath.Join(dataDir, catalogStateDirName)
	indexRoot := filepath.Join(stateRoot, semanticBinaryIndexDirName)
	payloadRoot := filepath.Join(stateRoot, semanticVectorPayloadDirName)
	if err := os.MkdirAll(indexRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll(index root) error = %v", err)
	}
	if err := os.MkdirAll(payloadRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll(payload root) error = %v", err)
	}
	indexTemp := filepath.Join(indexRoot, "source.g1.tidx.123.tmp")
	payloadTemp := filepath.Join(payloadRoot, ".payload-crash")
	indexSentinel := filepath.Join(indexRoot, "notes.tmp")
	payloadSentinel := filepath.Join(payloadRoot, "notes.tmp")
	for _, path := range []string{indexTemp, payloadTemp, indexSentinel, payloadSentinel} {
		if err := os.WriteFile(path, []byte("temp"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	store, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()
	for _, path := range []string{indexTemp, payloadTemp} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("crash temp %s still exists: %v", path, err)
		}
	}
	for _, path := range []string{indexSentinel, payloadSentinel} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("nonmatching sentinel %s was removed: %v", path, err)
		}
	}
}
