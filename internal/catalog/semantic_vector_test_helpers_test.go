package catalog

import (
	"context"
	"testing"
)

func insertSemanticVectorForTest(
	t *testing.T,
	catalogStore *CatalogStore,
	ctx context.Context,
	sourceKey string,
	assetID string,
	modelID string,
	vectorSpaceID string,
	embeddingDim int,
	vector []float32,
	embeddingInput string,
	status string,
	lastError any,
	generatedAt string,
	_ any,
) {
	t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	if embeddingInput == "" {
		embeddingInput = "test"
	}
	if status == "" {
		status = "ready"
	}
	ref, err := catalogStore.appendSemanticVectorPayloadBlob(ctx, sourceKey, modelID, vectorSpaceID, embeddingDim, encodeSemanticVector(vector))
	if err != nil {
		t.Fatalf("append semantic vector payload %q: %v", assetID, err)
	}
	if _, err := catalogStore.db.ExecContext(ctx, `INSERT INTO semantic_vectors (
			source_key, upstream_asset_id, model_id, vector_space_id, embedding_dim,
			payload_batch_id, vector_offset, vector_length, embedding_input, status, last_error, generated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sourceKey,
		assetID,
		modelID,
		vectorSpaceID,
		embeddingDim,
		ref.BatchID,
		ref.Offset,
		ref.Length,
		embeddingInput,
		status,
		lastError,
		generatedAt,
	); err != nil {
		t.Fatalf("insert semantic vector %q: %v", assetID, err)
	}
}
