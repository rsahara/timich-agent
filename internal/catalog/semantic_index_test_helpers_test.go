package catalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

func TestSemanticBinaryMembershipReconcilesExistingActiveIndex(t *testing.T) {
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
		{SourceKey: sourceKey, ID: "asset-a", MediaType: "image", Filename: "asset-a.jpg", CapturedAt: builtAt, Vector: []float32{1, 0, 0, 0}},
		{SourceKey: sourceKey, ID: "asset-b", MediaType: "image", Filename: "asset-b.jpg", CapturedAt: builtAt.Add(-time.Second), Vector: []float32{0, 1, 0, 0}},
	}
	seedAndWriteSemanticBinaryIndexForTest(t, store, ctx, sourceKey, profile, assets, builtAt, 3)
	if _, err := store.db.ExecContext(ctx, `DELETE FROM semantic_index_membership`); err != nil {
		_ = store.Close()
		t.Fatalf("clear semantic binary membership: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close catalog before membership reconciliation: %v", err)
	}

	reopened, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("reopen catalog with active binary: %v", err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM semantic_index_membership
		WHERE source_key = ? AND model_id = ? AND vector_space_id = ? AND asset_generation = 3`,
		sourceKey,
		profile.ModelID(),
		profile.VectorSpaceID(),
	).Scan(&count); err != nil {
		t.Fatalf("count reconciled semantic binary membership: %v", err)
	}
	if count != len(assets) {
		t.Fatalf("reconciled semantic binary membership = %d, want %d", count, len(assets))
	}
}

func TestSemanticBinaryMembershipTracksSameGenerationBinaryFingerprint(t *testing.T) {
	dataDir := t.TempDir()
	store, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	const sourceKey = "1111111111111111"
	profile := testImageSemanticProfile{}
	firstBuiltAt := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	seedAndWriteSemanticBinaryIndexForTest(t, store, ctx, sourceKey, profile, []semanticAsset{
		{SourceKey: sourceKey, ID: "asset-a", MediaType: "image", Filename: "asset-a.jpg", CapturedAt: firstBuiltAt, Vector: []float32{1, 0, 0, 0}},
		{SourceKey: sourceKey, ID: "asset-b", MediaType: "image", Filename: "asset-b.jpg", CapturedAt: firstBuiltAt.Add(-time.Second), Vector: []float32{0, 1, 0, 0}},
	}, firstBuiltAt, 3)
	manifestPath := store.semanticBinaryActiveManifestPath(sourceKey, profile)
	firstManifest, err := readSemanticBinaryActiveManifest(manifestPath)
	if err != nil {
		t.Fatalf("read first active manifest: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE catalog_assets
		SET visibility_status = 'missing'
		WHERE source_key = ? AND upstream_asset_id = 'asset-b'`, sourceKey); err != nil {
		t.Fatalf("hide old binary member: %v", err)
	}
	secondBuiltAt := firstBuiltAt.Add(time.Hour)
	seedAndWriteSemanticBinaryIndexForTest(t, store, ctx, sourceKey, profile, []semanticAsset{
		{SourceKey: sourceKey, ID: "asset-c", MediaType: "image", Filename: "asset-c.jpg", CapturedAt: secondBuiltAt, Vector: []float32{0, 0, 1, 0}},
	}, secondBuiltAt, 3)
	secondManifest, err := readSemanticBinaryActiveManifest(manifestPath)
	if err != nil {
		t.Fatalf("read second active manifest: %v", err)
	}
	if firstManifest.Header.NodeCount != secondManifest.Header.NodeCount || firstManifest.FileSHA256 == secondManifest.FileSHA256 {
		t.Fatalf("binary manifests = first:%#v second:%#v, want same count and different fingerprint", firstManifest, secondManifest)
	}

	rows, err := store.db.QueryContext(ctx, `SELECT upstream_asset_id
		FROM semantic_index_membership
		WHERE source_key = ? AND model_id = ? AND vector_space_id = ? AND asset_generation = 3`,
		sourceKey,
		profile.ModelID(),
		profile.VectorSpaceID(),
	)
	if err != nil {
		t.Fatalf("query replacement membership: %v", err)
	}
	defer rows.Close()
	assetIDs := map[string]bool{}
	for rows.Next() {
		var assetID string
		if err := rows.Scan(&assetID); err != nil {
			t.Fatalf("scan replacement membership: %v", err)
		}
		assetIDs[assetID] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate replacement membership: %v", err)
	}
	if len(assetIDs) != 2 || !assetIDs["asset-a"] || !assetIDs["asset-c"] || assetIDs["asset-b"] {
		t.Fatalf("replacement membership = %#v, want asset-a and asset-c", assetIDs)
	}
	var stateDigest string
	if err := store.db.QueryRowContext(ctx, `SELECT binary_sha256
		FROM semantic_index_membership_state
		WHERE source_key = ? AND model_id = ? AND vector_space_id = ? AND asset_generation = 3`,
		sourceKey,
		profile.ModelID(),
		profile.VectorSpaceID(),
	).Scan(&stateDigest); err != nil {
		t.Fatalf("read replacement membership identity: %v", err)
	}
	if stateDigest != secondManifest.FileSHA256 {
		t.Fatalf("membership digest = %q, want %q", stateDigest, secondManifest.FileSHA256)
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

func TestSemanticIndexBuilderUsesRecoverableNormalDurability(t *testing.T) {
	t.Parallel()

	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()
	path := filepath.Join(store.root, semanticBinaryIndexDirName, "durability.build.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	builder, err := openSemanticIndexBuilder(store, path, "1111111111111111", testImageSemanticProfile{}, 1, nil)
	if err != nil {
		t.Fatalf("openSemanticIndexBuilder() error = %v", err)
	}
	defer builder.Remove()

	var journalMode string
	if err := builder.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read builder journal mode: %v", err)
	}
	var synchronous int
	if err := builder.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatalf("read builder synchronous mode: %v", err)
	}
	if journalMode != "wal" || synchronous != 1 {
		t.Fatalf("builder durability journal=%q synchronous=%d, want wal/NORMAL(1)", journalMode, synchronous)
	}
	if semanticHNSWEfConstruction != 192 {
		t.Fatalf("efConstruction = %d, want 192", semanticHNSWEfConstruction)
	}
	if builder.vectorCacheMaxBytes != semanticIndexBuilderVectorCacheMaxBytes {
		t.Fatalf("vector cache limit = %d, want %d", builder.vectorCacheMaxBytes, semanticIndexBuilderVectorCacheMaxBytes)
	}
}

func TestSemanticIndexBuilderPrefetchUsesByteBoundedCache(t *testing.T) {
	t.Parallel()

	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	const sourceKey = "1111111111111111"
	profile := testImageSemanticProfile{}
	builtAt := time.Now().UTC()
	assets := []semanticAsset{
		{SourceKey: sourceKey, ID: "asset-a", MediaType: "image", Filename: "a.jpg", CapturedAt: builtAt, Vector: []float32{1, 0, 0, 0}},
		{SourceKey: sourceKey, ID: "asset-b", MediaType: "image", Filename: "b.jpg", CapturedAt: builtAt.Add(-time.Second), Vector: []float32{0, 1, 0, 0}},
		{SourceKey: sourceKey, ID: "asset-c", MediaType: "image", Filename: "c.jpg", CapturedAt: builtAt.Add(-2 * time.Second), Vector: []float32{0, 0, 1, 0}},
	}
	seedAndWriteSemanticBinaryIndexForTest(t, store, ctx, sourceKey, profile, assets, builtAt, 0)
	path := filepath.Join(store.root, semanticBinaryIndexDirName, "prefetch.build.db")
	builder, err := openSemanticIndexBuilder(store, path, sourceKey, profile, 1, nil)
	if err != nil {
		t.Fatalf("openSemanticIndexBuilder() error = %v", err)
	}
	defer builder.Remove()
	if err := builder.populate(ctx); err != nil {
		t.Fatalf("populate() error = %v", err)
	}
	builder.vectorCacheMaxBytes = 2 * int64(profile.EmbeddingDim()*4)
	if err := builder.prefetchVectors(ctx, builder.db, []int{0, 1, 2, 2}); err != nil {
		t.Fatalf("prefetchVectors() error = %v", err)
	}
	metrics := builder.metrics()
	if metrics.VectorPrefetchQueries != 1 || metrics.VectorPrefetched != 3 {
		t.Fatalf("prefetch metrics = %#v, want one query and three vectors", metrics)
	}
	if builder.vectorCacheBytes > builder.vectorCacheMaxBytes || len(builder.vectorCache) != 2 {
		t.Fatalf("builder vector cache bytes=%d entries=%d limit=%d, want two bounded vectors", builder.vectorCacheBytes, len(builder.vectorCache), builder.vectorCacheMaxBytes)
	}
}

func TestSemanticIndexBuilderAbruptTerminationResumesCommittedChunk(t *testing.T) {
	const (
		helperEnv = "TIMICH_TEST_SEMANTIC_BUILDER_ABRUPT_EXIT"
		dataEnv   = "TIMICH_TEST_SEMANTIC_BUILDER_DATA_DIR"
		exitCode  = 86
		sourceKey = "1111111111111111"
	)
	if os.Getenv(helperEnv) == "1" {
		store, err := LoadOrCreateCatalogStore(os.Getenv(dataEnv))
		if err != nil {
			os.Exit(87)
		}
		job := &semanticIndexJob{SourceKey: sourceKey, AssetGeneration: 1}
		builder, err := store.prepareSemanticIndexBuilder(context.Background(), job, testImageSemanticProfile{})
		if err != nil {
			os.Exit(88)
		}
		checks := 0
		builder.checkpoint = func(context.Context) error {
			checks++
			if checks == semanticIndexBuilderTransactionSize+2 {
				os.Exit(exitCode)
			}
			return nil
		}
		_ = builder.Build(context.Background())
		os.Exit(89)
	}

	dataDir := t.TempDir()
	store, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	profile := testImageSemanticProfile{}
	assets := make([]semanticAsset, semanticIndexBuilderTransactionSize+2)
	builtAt := time.Now().UTC()
	for index := range assets {
		assets[index] = semanticAsset{
			SourceKey:  sourceKey,
			ID:         "asset-" + strconv.Itoa(index),
			MediaType:  "image",
			Filename:   "asset-" + strconv.Itoa(index) + ".jpg",
			CapturedAt: builtAt.Add(-time.Duration(index) * time.Second),
			Vector:     []float32{float32(index%3) / 2, float32((index+1)%3) / 2, 0.5, 0.25},
		}
	}
	seedAndWriteSemanticBinaryIndexForTest(t, store, context.Background(), sourceKey, profile, assets, builtAt, 0)
	if err := store.Close(); err != nil {
		t.Fatalf("close catalog before abrupt builder process: %v", err)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestSemanticIndexBuilderAbruptTerminationResumesCommittedChunk$")
	command.Env = append(os.Environ(), helperEnv+"=1", dataEnv+"="+dataDir)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitCode {
		t.Fatalf("abrupt builder helper error=%v output=%s, want exit %d", err, output, exitCode)
	}

	reopened, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("reopen catalog after abrupt builder exit: %v", err)
	}
	defer reopened.Close()
	job := &semanticIndexJob{SourceKey: sourceKey, AssetGeneration: 1}
	builder, err := reopened.prepareSemanticIndexBuilder(context.Background(), job, profile)
	if err != nil {
		t.Fatalf("prepareSemanticIndexBuilder(after abrupt exit) error = %v", err)
	}
	defer builder.Remove()
	next, err := builder.metaInt(context.Background(), builder.db, "next_ordinal")
	if err != nil {
		t.Fatalf("read resumed next ordinal: %v", err)
	}
	if next != semanticIndexBuilderTransactionSize {
		t.Fatalf("resumed next ordinal = %d, want committed chunk %d", next, semanticIndexBuilderTransactionSize)
	}
	if err := builder.Build(context.Background()); err != nil {
		t.Fatalf("resume builder after abrupt exit: %v", err)
	}
	next, err = builder.metaInt(context.Background(), builder.db, "next_ordinal")
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
	activePath := store.semanticBinaryActiveManifestPath(sourceKey, profile)
	activeBefore, err := readSemanticBinaryActiveManifest(activePath)
	if err != nil {
		t.Fatalf("read active manifest before builder corruption: %v", err)
	}
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
	activeAfter, err := readSemanticBinaryActiveManifest(activePath)
	if err != nil {
		t.Fatalf("read active manifest after builder recovery: %v", err)
	}
	if activeAfter.FileSHA256 != activeBefore.FileSHA256 || activeAfter.Header.AssetGeneration != activeBefore.Header.AssetGeneration {
		t.Fatalf("active manifest changed during derived-builder recovery: before=%#v after=%#v", activeBefore, activeAfter)
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
