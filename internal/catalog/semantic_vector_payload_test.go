package catalog

import (
	"context"
	"testing"
)

func TestSemanticVectorPayloadCacheUsesByteBoundedLRU(t *testing.T) {
	t.Parallel()

	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	defer store.Close()
	store.semanticVectorPayloadMaxBytes = 32
	store.semanticVectorPayloadMaxItems = 3
	ctx := context.Background()
	const sourceKey = "1111111111111111"
	refs := make([]semanticVectorPayloadRef, 0, 3)
	for _, vector := range [][]float32{{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}} {
		ref, err := store.appendSemanticVectorPayloadBlob(ctx, sourceKey, "test-model", "test-space", len(vector), encodeSemanticVector(vector))
		if err != nil {
			t.Fatalf("appendSemanticVectorPayloadBlob() error = %v", err)
		}
		refs = append(refs, ref)
	}
	for _, ref := range refs[:2] {
		if _, err := store.loadSemanticVectorPayloadBatch(ctx, ref.BatchID, false); err != nil {
			t.Fatalf("load initial semantic vector payload: %v", err)
		}
	}
	if _, err := store.loadSemanticVectorPayloadBatch(ctx, refs[0].BatchID, false); err != nil {
		t.Fatalf("touch first semantic vector payload: %v", err)
	}
	if _, err := store.loadSemanticVectorPayloadBatch(ctx, refs[2].BatchID, false); err != nil {
		t.Fatalf("load third semantic vector payload: %v", err)
	}

	store.semanticVectorPayloadCacheMu.Lock()
	defer store.semanticVectorPayloadCacheMu.Unlock()
	if store.semanticVectorPayloadBytes > store.semanticVectorPayloadMaxBytes {
		t.Fatalf("payload cache bytes = %d, limit = %d", store.semanticVectorPayloadBytes, store.semanticVectorPayloadMaxBytes)
	}
	if len(store.semanticVectorPayloadCache) != 2 {
		t.Fatalf("payload cache entries = %d, want 2", len(store.semanticVectorPayloadCache))
	}
	if store.semanticVectorPayloadCache[refs[0].BatchID] == nil || store.semanticVectorPayloadCache[refs[2].BatchID] == nil {
		t.Fatalf("payload cache keys = %v, want first and third after LRU touch", store.semanticVectorPayloadOrder)
	}
	if store.semanticVectorPayloadCache[refs[1].BatchID] != nil {
		t.Fatalf("payload cache retained least-recently-used batch %q", refs[1].BatchID)
	}
}
