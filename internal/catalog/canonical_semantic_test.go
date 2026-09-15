package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

type recordedSemanticImageLoad struct {
	sourceKey string
	assetID   string
}

type recordingSemanticImageLoader struct {
	loads []recordedSemanticImageLoad
}

func (l *recordingSemanticImageLoader) LoadSemanticImage(_ context.Context, sourceKey string, assetID string) (*semanticImageEmbeddingInput, error) {
	l.loads = append(l.loads, recordedSemanticImageLoad{sourceKey: sourceKey, assetID: assetID})
	return &semanticImageEmbeddingInput{
		Bytes:       []byte("canonical-semantic-test-image"),
		ContentType: "image/jpeg",
		Source:      "test",
	}, nil
}

func TestCanonicalSemanticCorpusIndexesOneDuplicateAndReturnsGalleryIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	const (
		immichSource = "1111111111111111"
		localSource  = "2222222222222222"
		immichAsset  = "immich-family"
		localAsset   = "local-family"
	)
	const contentSHA1 = "0123456789abcdef0123456789abcdef01234567"
	const contentSize = int64(3397077)
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	nowText := formatCatalogTime(now)
	if _, err := store.ReplaceFull(ctx, immichSource, []ImmichMirrorAsset{{
		UpstreamAssetID:  immichAsset,
		MediaType:        "image",
		Filename:         "family-immich.jpg",
		CapturedAt:       now,
		ContentSHA1Hex:   contentSHA1,
		ContentSizeBytes: contentSize,
	}}, 0, now); err != nil {
		t.Fatalf("ReplaceFull() error = %v", err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{
			query: `INSERT INTO local_assets (
				source_key, asset_id, sha1_hex, content_size_bytes, media_type, filename,
				captured_at, captured_at_source, visibility_status, thumbnail_status,
				first_seen_at, updated_at
			) VALUES (?, ?, ?, ?, 'image', 'family-local.jpg', ?, 'exif', 'active', 'ready', ?, ?)`,
			args: []any{localSource, localAsset, contentSHA1, contentSize, nowText, nowText, nowText},
		},
		{
			query: `INSERT INTO local_renditions (
				source_key, asset_id, kind, status, relative_path, width, height, size_bytes,
				content_sha256, generated_at, source_sha1_hex
			) VALUES (?, ?, 'detail_preview', 'ready', 'previews/family.jpg', 512, 512, 1024, ?, ?, ?)`,
			args: []any{localSource, localAsset, strings.Repeat("a", 64), nowText, contentSHA1},
		},
		{
			query: `INSERT INTO catalog_assets (
				source_key, datasource_kind, upstream_asset_id, media_type, filename,
				captured_at, visibility_status, upstream_checksum_algorithm,
				content_sha1_hex, content_size_bytes, canonical_content_sha1_hex,
				canonical_content_size_bytes, first_seen_at, updated_at
			) VALUES (?, 'local_filesystem', ?, 'image', 'family-local.jpg', ?, 'active', 'sha1', ?, ?, ?, ?, ?, ?)`,
			args: []any{localSource, localAsset, nowText, contentSHA1, contentSize, contentSHA1, contentSize, nowText, nowText},
		},
	} {
		if _, err := store.db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed canonical semantic fixture: %v", err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO local_renditions (
		source_key, asset_id, kind, status, relative_path, width, height, size_bytes,
		content_sha256, generated_at, source_sha1_hex
	) VALUES (?, ?, 'preview', 'ready', 'previews/family-small.jpg', 256, 256, 512, ?, ?, ?)`,
		localSource, localAsset, strings.Repeat("c", 64), nowText, contentSHA1); err != nil {
		t.Fatalf("insert Local preview rendition: %v", err)
	}
	if _, err := store.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}

	var canonicalAssetID string
	var primarySource string
	if err := store.db.QueryRowContext(ctx, `SELECT canonical_asset_id, primary_source_key
		FROM catalog_canonical_assets`).Scan(&canonicalAssetID, &primarySource); err != nil {
		t.Fatalf("read canonical fixture asset: %v", err)
	}
	if primarySource != localSource {
		t.Fatalf("canonical primary source = %q, want Local %q", primarySource, localSource)
	}

	profile := testImageSemanticProfile{}
	scopedAssets, err := store.loadCanonicalSemanticBackfillAssets(ctx, profile, 10, []string{localSource}, "")
	if err != nil {
		t.Fatalf("loadCanonicalSemanticBackfillAssets(Local) error = %v", err)
	}
	if len(scopedAssets) != 1 || scopedAssets[0].ID != canonicalAssetID {
		t.Fatalf("Local-scoped canonical assets = %#v, want %q", scopedAssets, canonicalAssetID)
	}
	nonMatchingAssets, err := store.loadCanonicalSemanticBackfillAssets(ctx, profile, 10, []string{"3333333333333333"}, "")
	if err != nil {
		t.Fatalf("loadCanonicalSemanticBackfillAssets(non-matching) error = %v", err)
	}
	if len(nonMatchingAssets) != 0 {
		t.Fatalf("non-matching source scope assets = %#v, want none", nonMatchingAssets)
	}
	loader := &recordingSemanticImageLoader{}
	backfill, err := store.BackfillSemanticVectors(ctx, canonicalSemanticCorpusSourceKey, profile, now, SemanticBackfillOptions{
		ImageLoader: loader,
		MaxAssets:   10,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticVectors(canonical) error = %v", err)
	}
	if backfill.ProcessedVectorCount != 1 || backfill.Status.CompletedVectorCount != 1 {
		t.Fatalf("canonical backfill = %#v, want one completed canonical vector", backfill)
	}
	if len(loader.loads) != 1 || loader.loads[0] != (recordedSemanticImageLoad{sourceKey: localSource, assetID: localAsset}) {
		t.Fatalf("canonical image loads = %#v, want Local detail preview", loader.loads)
	}

	var vectorCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM semantic_vectors
		WHERE source_key = ? AND model_id = ?`, canonicalSemanticCorpusSourceKey, profile.ModelID()).Scan(&vectorCount); err != nil {
		t.Fatalf("count canonical semantic vectors: %v", err)
	}
	if vectorCount != 1 {
		t.Fatalf("canonical semantic vector count = %d, want one", vectorCount)
	}
	var representativeSource string
	var representativeAsset string
	var input string
	var fingerprint string
	if err := store.db.QueryRowContext(ctx, `SELECT representative_source_key, representative_upstream_asset_id, embedding_input, input_fingerprint
		FROM canonical_semantic_vector_inputs
		WHERE canonical_asset_id = ? AND model_id = ?`, canonicalAssetID, profile.ModelID()).Scan(&representativeSource, &representativeAsset, &input, &fingerprint); err != nil {
		t.Fatalf("read canonical vector input: %v", err)
	}
	if representativeSource != localSource || representativeAsset != localAsset || input != "local_detail_preview" {
		t.Fatalf("canonical vector input = (%q, %q, %q), want Local detail preview", representativeSource, representativeAsset, input)
	}
	digest := sha256.Sum256([]byte("canonical-semantic-test-image"))
	if !strings.Contains(fingerprint, hex.EncodeToString(digest[:])) {
		t.Fatalf("canonical input fingerprint = %q, want loaded image digest", fingerprint)
	}

	queued, err := store.ReconcileSemanticIndexJobs(ctx, []string{canonicalSemanticCorpusSourceKey}, profile, false, now)
	if err != nil {
		t.Fatalf("ReconcileSemanticIndexJobs(canonical) error = %v", err)
	}
	if queued != 1 {
		status, statusErr := store.SemanticBackfillStatus(ctx, canonicalSemanticCorpusSourceKey, SemanticModelProfileStatus{
			ModelID: profile.ModelID(), VectorSpaceID: profile.VectorSpaceID(), EmbeddingDim: profile.EmbeddingDim(), InputKind: profile.InputKind(),
		})
		t.Fatalf("canonical index jobs enqueued = %d, want 1 (status=%#v statusErr=%v)", queued, status, statusErr)
	}
	published, err := store.PublishNextSemanticIndexJob(ctx, []string{canonicalSemanticCorpusSourceKey}, profile, now)
	if err != nil {
		t.Fatalf("PublishNextSemanticIndexJob(canonical) error = %v", err)
	}
	if !published.Published || published.IndexedVectorCount != 1 {
		t.Fatalf("canonical index publish = %#v, want one published node", published)
	}
	var membershipCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM semantic_index_membership
		WHERE source_key = ? AND model_id = ?`, canonicalSemanticCorpusSourceKey, profile.ModelID()).Scan(&membershipCount); err != nil {
		t.Fatalf("count canonical semantic membership: %v", err)
	}
	if membershipCount != 1 {
		t.Fatalf("canonical semantic membership = %d, want one", membershipCount)
	}

	service := &Service{catalog: store}
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindSearch, Query: &AssetSearchQuery{
			Text: "family", Mode: QueryModeAuto,
		}},
		Page: AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("normalize semantic search: %v", err)
	}
	page, err := service.searchCatalogSemanticAssets(ctx, normalized, profile, AssetSearchOptions{IncludeSemanticScores: true})
	if err != nil {
		t.Fatalf("searchCatalogSemanticAssets() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("canonical semantic search page = %#v, want one Gallery result", page)
	}
	if item := page.Items[0]; item.SourceKey != localSource || item.ID != localAsset || item.SemanticScore == nil {
		t.Fatalf("canonical semantic search item = %#v, want Local Gallery identity and score", item)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE local_renditions
		SET content_sha256 = ? WHERE source_key = ? AND asset_id = ? AND kind = 'detail_preview'`, strings.Repeat("b", 64), localSource, localAsset); err != nil {
		t.Fatalf("update Local rendition: %v", err)
	}
	var refreshRequired int
	if err := store.db.QueryRowContext(ctx, `SELECT refresh_required FROM canonical_semantic_vector_inputs
		WHERE canonical_asset_id = ? AND model_id = ?`, canonicalAssetID, profile.ModelID()).Scan(&refreshRequired); err != nil {
		t.Fatalf("read canonical refresh state: %v", err)
	}
	if refreshRequired != 1 {
		t.Fatalf("canonical refresh_required = %d, want 1 after rendition change", refreshRequired)
	}
	if !store.canonicalSemanticCorpusExists(ctx, profile) {
		t.Fatal("canonical semantic corpus should remain available through a refresh")
	}

}
