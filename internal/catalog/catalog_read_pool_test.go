package catalog

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestForegroundReadPoolRemainsAvailableWhenBackgroundPoolIsBusy(t *testing.T) {
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()

	backgroundConn, err := store.backgroundReadDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("backgroundReadDB.Conn() error = %v", err)
	}
	defer backgroundConn.Close()

	foregroundCtx, cancelForeground := context.WithTimeout(context.Background(), time.Second)
	defer cancelForeground()
	normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page:       AssetSearchPageRequest{Index: 0, Size: 60},
	})
	if err != nil {
		t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
	}
	if _, err := store.SearchCatalogAssets(foregroundCtx, normalized); err != nil {
		t.Fatalf("SearchCatalogAssets() error while background pool is occupied = %v", err)
	}

	backgroundCtx, cancelBackground := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelBackground()
	_, err = store.SemanticBackfillStatus(backgroundCtx, "source-a", SemanticModelProfileStatus{
		ModelID:       "model-a",
		VectorSpaceID: "model-a/d4",
		EmbeddingDim:  4,
		InputKind:     semanticInputKindImage,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SemanticBackfillStatus() error = %v, want background pool deadline", err)
	}
}
