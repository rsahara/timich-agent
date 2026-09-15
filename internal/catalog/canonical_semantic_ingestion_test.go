package catalog

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCanonicalSemanticWaitsForLocalInput(t *testing.T) {
	for _, mediaType := range []string{"image", "video"} {
		for _, alreadyEmbedded := range []bool{false, true} {
			name := mediaType + "/new"
			if alreadyEmbedded {
				name = mediaType + "/published_immich"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				store, err := LoadOrCreateCatalogStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				const immichSource = "1111111111111111"
				const localSource = "2222222222222222"
				const sha = "0123456789abcdef0123456789abcdef01234567"
				now := time.Now().UTC().Add(-time.Minute)
				stamp := formatCatalogTime(now)
				profile := testImageSemanticProfile{}
				profileStatus := SemanticModelProfileStatus{ModelID: profile.ModelID(), VectorSpaceID: profile.VectorSpaceID(), EmbeddingDim: profile.EmbeddingDim(), InputKind: profile.InputKind()}
				if _, err := store.ReplaceFull(ctx, immichSource, []ImmichMirrorAsset{{
					UpstreamAssetID: "immich", MediaType: mediaType, Filename: "media.jpg", CapturedAt: now, ContentSHA1Hex: sha, ContentSizeBytes: 1024,
				}}, 0, now); err != nil {
					t.Fatal(err)
				}
				loader := &recordingSemanticImageLoader{}
				backfill := func() SemanticBackfillResult {
					t.Helper()
					result, err := store.BackfillSemanticVectors(ctx, canonicalSemanticCorpusSourceKey, profile, now, SemanticBackfillOptions{ImageLoader: loader, MaxAssets: 10})
					if err != nil {
						t.Fatal(err)
					}
					return result
				}
				if alreadyEmbedded {
					if got := backfill(); got.ProcessedVectorCount != 1 {
						t.Fatalf("initial backfill = %+v", got)
					}
					if _, err := store.ReconcileSemanticIndexJobs(ctx, []string{canonicalSemanticCorpusSourceKey}, profile, false, now); err != nil {
						t.Fatal(err)
					}
					if published, err := store.PublishNextSemanticIndexJob(ctx, []string{canonicalSemanticCorpusSourceKey}, profile, now); err != nil || !published.Published {
						t.Fatalf("publish = %+v, %v", published, err)
					}
				}
				for _, statement := range []struct {
					query string
					args  []any
				}{
					{`INSERT INTO local_assets (source_key, asset_id, sha1_hex, content_size_bytes, media_type, filename, captured_at, captured_at_source, visibility_status, thumbnail_status, first_seen_at, updated_at)
					VALUES (?, 'local', ?, 1024, ?, 'media.jpg', ?, 'exif', 'active', 'pending', ?, ?)`, []any{localSource, sha, mediaType, stamp, stamp, stamp}},
					{`INSERT INTO catalog_assets (source_key, datasource_kind, upstream_asset_id, media_type, filename, captured_at, visibility_status, upstream_checksum_algorithm, content_sha1_hex, content_size_bytes, canonical_content_sha1_hex, canonical_content_size_bytes, first_seen_at, updated_at)
					VALUES (?, 'local_filesystem', 'local', ?, 'media.jpg', ?, 'active', 'sha1', ?, 1024, ?, 1024, ?, ?)`, []any{localSource, mediaType, stamp, sha, sha, stamp, stamp}},
				} {
					if _, err := store.db.ExecContext(ctx, statement.query, statement.args...); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := store.RebuildCatalogCanonicalAssets(ctx); err != nil {
					t.Fatal(err)
				}
				before := len(loader.loads)
				for _, thumbnailStatus := range []string{"pending", "failed"} {
					if _, err := store.db.ExecContext(ctx, `UPDATE local_assets SET thumbnail_status = ?`, thumbnailStatus); err != nil {
						t.Fatal(err)
					}
					got := backfill()
					wantCompleted := 0
					if alreadyEmbedded {
						wantCompleted = 1
					}
					if got.ProcessedVectorCount != 0 || len(loader.loads) != before || got.Status.EligibleNowVectorCount != 0 || got.Status.CompletedVectorCount != wantCompleted || got.Status.EligibleAssetCount != wantCompleted {
						t.Fatalf("%s Local input: backfill = %+v, loads = %+v", thumbnailStatus, got, loader.loads)
					}
					if alreadyEmbedded && got.Status.IndexedVectorCount != 1 {
						t.Fatalf("published coverage lost: %+v", got.Status)
					}
				}
				if alreadyEmbedded {
					// Unusable replacement input must not leave phantom failed or
					// wrong-profile work queued, or keep waking the retry scheduler.
					for _, vectorState := range []struct{ status, space string }{{"failed", profile.VectorSpaceID()}, {"ready", "old-space"}} {
						if _, err := store.db.ExecContext(ctx, `UPDATE semantic_vectors SET status = ?, vector_space_id = ?, generated_at = ?`, vectorState.status, vectorState.space, formatCatalogTime(time.Now().UTC())); err != nil {
							t.Fatal(err)
						}
						got, err := store.SemanticBackfillStatus(ctx, canonicalSemanticCorpusSourceKey, profileStatus)
						if err != nil || got.EligibleAssetCount != 0 || got.CompletedVectorCount != 0 || got.FailedVectorCount != 0 || got.EligibleNowVectorCount != 0 || got.NextEligibleAt != nil {
							t.Fatalf("unavailable input status = %+v, %v", got, err)
						}
					}
					if _, err := store.db.ExecContext(ctx, `UPDATE semantic_vectors SET status = 'ready', vector_space_id = ?`, profile.VectorSpaceID()); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := store.db.ExecContext(ctx, `INSERT INTO local_renditions (source_key, asset_id, kind, status, relative_path, width, height, size_bytes, content_sha256, generated_at, source_sha1_hex)
				VALUES (?, 'local', 'detail_preview', 'ready', 'media.jpg', 512, 512, 1024, ?, ?, ?)`, localSource, strings.Repeat("a", 64), stamp, sha); err != nil {
					t.Fatal(err)
				}
				status, err := store.SemanticBackfillStatus(ctx, canonicalSemanticCorpusSourceKey, profileStatus)
				if err != nil || status.EligibleNowVectorCount != 1 {
					t.Fatalf("ready Local status = %+v, %v", status, err)
				}
				if got := backfill(); got.ProcessedVectorCount != 1 || got.Status.CompletedVectorCount != 1 {
					t.Fatalf("Local backfill = %+v", got)
				}
				if len(loader.loads) != before+1 || loader.loads[before].sourceKey != localSource {
					t.Fatalf("loads = %+v, want one Local inference", loader.loads)
				}
				if got := backfill(); got.ProcessedVectorCount != 0 {
					t.Fatalf("repeated Local backfill = %+v", got)
				}
				if _, err := store.db.ExecContext(ctx, `UPDATE catalog_assets SET visibility_status = 'missing' WHERE source_key = ?`, localSource); err != nil {
					t.Fatal(err)
				}
				if _, err := store.RebuildCatalogCanonicalAssets(ctx); err != nil {
					t.Fatal(err)
				}
				if got := backfill(); got.ProcessedVectorCount != 1 || loader.loads[len(loader.loads)-1].sourceKey != immichSource {
					t.Fatalf("Local removal should allow remaining Immich input: %+v, %+v", got, loader.loads)
				}
				var count int
				if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM semantic_vectors WHERE source_key = ?`, canonicalSemanticCorpusSourceKey).Scan(&count); err != nil || count != 1 {
					t.Fatalf("vector count = %d, %v, want one canonical vector", count, err)
				}
			})
		}
	}
}
