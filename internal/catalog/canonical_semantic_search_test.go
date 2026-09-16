package catalog

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestCanonicalSemanticSearchUsesPublishedCorpus(t *testing.T) {
	for _, active := range []bool{false, true} {
		role := "candidate"
		if active {
			role = "active"
		}
		t.Run(role, func(t *testing.T) {
			ctx := context.Background()
			dataDir := t.TempDir()
			models, pack := installSemanticRuntimeStatusTestModel(t, dataDir)
			const sourceKey = "1111111111111111"
			service, err := NewServiceWithOptions([]config.DatasourceConfig{{
				SourceKey: sourceKey, Name: "Photos", Kind: config.DatasourceKindImmichIndexed,
				URL: "http://immich.test", AccessToken: "test-key",
			}}, ServiceOptions{DataDir: dataDir, SemanticModels: models})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			profile, ok := models.CandidateEmbeddingProfile(pack.ID, pack.VectorSpaceID)
			if !ok {
				t.Fatal("candidate embedding profile unavailable")
			}
			if active {
				if _, err := models.ActivatePack(pack.ID, pack.VectorSpaceID); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC().Add(-time.Minute)
			if _, err := service.catalog.ReplaceFull(ctx, sourceKey, []ImmichMirrorAsset{{
				UpstreamAssetID: "beach", MediaType: "image", Filename: "Beach.jpg", CapturedAt: now,
			}}, 0, now); err != nil {
				t.Fatal(err)
			}
			backfill, err := service.catalog.BackfillSemanticVectors(ctx, canonicalSemanticCorpusSourceKey, profile, now, SemanticBackfillOptions{
				ImageLoader: staticSemanticImageLoader{}, MaxAssets: 1,
			})
			if err != nil || backfill.ProcessedVectorCount != 1 {
				t.Fatalf("canonical backfill = %+v, %v", backfill, err)
			}
			if service.semanticSearchHasPublishedIndex(ctx, profile) {
				t.Fatal("unpublished canonical vectors must not admit semantic search")
			}
			if _, err := service.catalog.ReconcileSemanticIndexJobs(ctx, []string{canonicalSemanticCorpusSourceKey}, profile, false, now); err != nil {
				t.Fatal(err)
			}
			published, err := service.catalog.PublishNextSemanticIndexJob(ctx, []string{canonicalSemanticCorpusSourceKey}, profile, now)
			if err != nil || !published.Published || published.IndexedVectorCount != 1 {
				t.Fatalf("canonical publish = %+v, %v", published, err)
			}
			// Only the canonical corpus has a binary; configured sources do not.
			if available, err := service.catalog.hasPublishedSemanticBinaryIndex(ctx, sourceKey, profile); err != nil || available {
				t.Fatalf("source-scoped binary = %t, %v, want absent", available, err)
			}
			for _, mode := range []string{QueryModeAuto, QueryModeSemantic} {
				page, err := service.SearchAssetsWithContext(ctx, AssetSearchRequest{
					Collection: AssetCollectionRequest{Kind: CollectionKindSearch, Query: &AssetSearchQuery{Text: "Beach", Mode: mode}},
					Page:       AssetSearchPageRequest{Index: 0, Size: 10},
				})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Items) != 1 || page.Total != 1 || page.Items[0].ID != "beach" || page.Items[0].SourceKey != sourceKey || page.Resolved.QueryMode != QueryModeSemantic {
					t.Fatalf("%s search = %+v, want source identity from the canonical index", mode, page)
				}
				if semantic := page.Resolved.Semantic; semantic == nil || semantic.ModelID != pack.ID || semantic.IndexedVectorCount != 1 || semantic.FallbackQueryMode != "" {
					t.Fatalf("%s semantic resolution = %+v", mode, semantic)
				}
			}
			if err := os.Remove(service.catalog.semanticBinaryActiveManifestPath(canonicalSemanticCorpusSourceKey, profile)); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{QueryModeAuto, QueryModeSemantic} {
				page, err := service.SearchAssetsWithContext(ctx, AssetSearchRequest{
					Collection: AssetCollectionRequest{Kind: CollectionKindSearch, Query: &AssetSearchQuery{Text: "Beach", Mode: mode}},
					Page:       AssetSearchPageRequest{Index: 0, Size: 10},
				})
				if err != nil {
					t.Fatal(err)
				}
				wantItems, wantMode := 0, QueryModeSemantic
				if mode == QueryModeAuto {
					wantItems, wantMode = 1, QueryModeFilename
				}
				if len(page.Items) != wantItems || page.Resolved.QueryMode != wantMode {
					t.Fatalf("%s search without manifest = %+v", mode, page)
				}
				if semantic := page.Resolved.Semantic; semantic == nil || semantic.ModelID != pack.ID || semantic.Eligible || semantic.Status != semanticBackfillStatusIndexing || semantic.CompletedVectorCount != 1 || semantic.IndexedVectorCount != 1 {
					t.Fatalf("%s unavailable canonical status = %+v", mode, semantic)
				}
			}
		})
	}
}
