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

// insertCanonicalSemanticVectorForTest mirrors the durable state produced by
// canonical semantic backfill: a canonical-corpus vector and its selected
// input ledger must advance together for status counts to consider it current.
func insertCanonicalSemanticVectorForTest(
	t *testing.T,
	catalogStore *CatalogStore,
	ctx context.Context,
	canonicalAssetID string,
	representativeSourceKey string,
	representativeAssetID string,
	modelID string,
	vectorSpaceID string,
	embeddingDim int,
	vector []float32,
	embeddingInput string,
	generatedAt string,
) {
	t.Helper()
	if embeddingInput == "" {
		embeddingInput = "immich_preview"
	}
	insertSemanticVectorForTest(
		t,
		catalogStore,
		ctx,
		canonicalSemanticCorpusSourceKey,
		canonicalAssetID,
		modelID,
		vectorSpaceID,
		embeddingDim,
		vector,
		embeddingInput,
		"ready",
		nil,
		generatedAt,
		nil,
	)
	if _, err := catalogStore.db.ExecContext(ctx, `INSERT INTO canonical_semantic_vector_inputs (
			canonical_asset_id, model_id, representative_source_key, representative_upstream_asset_id,
			embedding_input, input_fingerprint, rendition_sha256, preprocessing_version,
			refresh_required, updated_at
		) VALUES (?, ?, ?, ?, ?, 'test-fingerprint', '', ?, 0, ?)`,
		canonicalAssetID,
		modelID,
		representativeSourceKey,
		representativeAssetID,
		embeddingInput,
		canonicalSemanticPreprocessingVersion,
		generatedAt,
	); err != nil {
		t.Fatalf("insert canonical semantic vector input %q: %v", canonicalAssetID, err)
	}
}
