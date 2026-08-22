package catalog

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestCatalogOpenAddsSemanticFailureRetrySchemaToCurrentDatabase(t *testing.T) {
	dataDir := t.TempDir()
	store, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	if _, err := store.db.Exec(`DROP TABLE semantic_vector_retry_requests`); err != nil {
		store.Close()
		t.Fatalf("drop retry request table: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close catalog without retry request table: %v", err)
	}

	reopened, err := LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatalf("reopen current catalog: %v", err)
	}
	defer reopened.Close()
	var tableCount int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'semantic_vector_retry_requests'`).Scan(&tableCount); err != nil {
		t.Fatalf("inspect retry request table: %v", err)
	}
	if tableCount != 1 {
		t.Fatalf("retry request table count = %d, want 1", tableCount)
	}
}

func TestSemanticEmbeddingFailureDiagnosticsAndImmediateRetry(t *testing.T) {
	const sourceKey = "1111111111111111"
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	ctx := context.Background()
	profileImpl := testImageSemanticProfile{}
	profile := SemanticModelProfileStatus{
		ModelID:       profileImpl.ModelID(),
		VectorSpaceID: profileImpl.VectorSpaceID(),
		EmbeddingDim:  profileImpl.EmbeddingDim(),
		ProfileKind:   profileImpl.ProfileKind(),
		InputKind:     profileImpl.InputKind(),
	}
	lastAttemptedAt := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Microsecond)
	nowText := formatCatalogTime(lastAttemptedAt)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{
			query: `INSERT INTO catalog_assets (
				source_key, datasource_kind, upstream_asset_id, media_type, filename, captured_at,
				visibility_status, first_seen_at, updated_at
			) VALUES (?, 'immich', 'failed-asset', 'image', 'broken.jpg', ?, 'active', ?, ?)`,
			args: []any{sourceKey, nowText, nowText, nowText},
		},
		{
			query: `INSERT INTO catalog_assets (
				source_key, datasource_kind, upstream_asset_id, media_type, filename, captured_at,
				visibility_status, first_seen_at, updated_at
			) VALUES (?, 'immich', 'hidden-asset', 'image', 'hidden.jpg', ?, 'missing', ?, ?)`,
			args: []any{sourceKey, nowText, nowText, nowText},
		},
		{
			query: `INSERT INTO semantic_state (
				source_key, model_id, vector_space_id, status, embedding_dim,
				completed_vector_count, indexed_vector_count, asset_generation, indexed_generation, updated_at
			) VALUES (?, ?, ?, 'ready', ?, 0, 0, 7, 7, ?)`,
			args: []any{sourceKey, profile.ModelID, profile.VectorSpaceID, profile.EmbeddingDim, nowText},
		},
		{
			query: `INSERT INTO semantic_vectors (
				source_key, upstream_asset_id, model_id, vector_space_id, embedding_dim,
				payload_batch_id, vector_offset, vector_length, embedding_input, status, last_error, generated_at
			) VALUES (?, 'failed-asset', ?, ?, ?, NULL, 0, 0, 'image', 'failed', 'unsupported image', ?)`,
			args: []any{sourceKey, profile.ModelID, profile.VectorSpaceID, profile.EmbeddingDim, nowText},
		},
		{
			query: `INSERT INTO semantic_vectors (
				source_key, upstream_asset_id, model_id, vector_space_id, embedding_dim,
				payload_batch_id, vector_offset, vector_length, embedding_input, status, last_error, generated_at
			) VALUES (?, 'hidden-asset', ?, ?, ?, NULL, 0, 0, 'image', 'failed', 'hidden failure', ?)`,
			args: []any{sourceKey, profile.ModelID, profile.VectorSpaceID, profile.EmbeddingDim, nowText},
		},
	} {
		if _, err := service.catalog.db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("prepare failure diagnostics fixture: %v", err)
		}
	}

	diagnostics, err := collectSemanticEmbeddingFailureDiagnostics(ctx, service, profile)
	if err != nil {
		t.Fatalf("collectSemanticEmbeddingFailureDiagnostics() error = %v", err)
	}
	if len(diagnostics) != 1 {
		t.Fatalf("failure diagnostics = %#v, want one active current-profile failure", diagnostics)
	}
	if diagnostics[0].AssetID != "failed-asset" || diagnostics[0].LastError != "unsupported image" || diagnostics[0].RetryState != "scheduled" {
		t.Fatalf("failure diagnostic = %#v", diagnostics[0])
	}
	if diagnostics[0].LastAttemptedAt != nowText || diagnostics[0].RetryEligibleAt == "" {
		t.Fatalf("failure diagnostic times = %#v", diagnostics[0])
	}

	assets, err := service.catalog.loadSemanticBackfillAssets(ctx, sourceKey, profileImpl, 10)
	if err != nil {
		t.Fatalf("loadSemanticBackfillAssets(before retry) error = %v", err)
	}
	if len(assets) != 0 {
		t.Fatalf("assets before retry request = %#v, want deferred failure", assets)
	}

	var generationBefore int64
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT asset_generation FROM semantic_state
		WHERE source_key = ? AND model_id = ?`, sourceKey, profile.ModelID).Scan(&generationBefore); err != nil {
		t.Fatalf("read generation before retry request: %v", err)
	}
	requestedAt := time.Now().UTC().Truncate(time.Microsecond)
	retry, err := service.RequestSemanticEmbeddingFailureRetry(ctx, profile, nil, requestedAt)
	if err != nil {
		t.Fatalf("RequestSemanticEmbeddingFailureRetry() error = %v", err)
	}
	if retry.RequestedCount != 1 || !retry.RequestedAt.Equal(requestedAt) {
		t.Fatalf("retry result = %#v", retry)
	}
	var generationAfter int64
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT asset_generation FROM semantic_state
		WHERE source_key = ? AND model_id = ?`, sourceKey, profile.ModelID).Scan(&generationAfter); err != nil {
		t.Fatalf("read generation after retry request: %v", err)
	}
	if generationAfter != generationBefore {
		t.Fatalf("generation after retry request = %d, want unchanged %d", generationAfter, generationBefore)
	}

	diagnostics, err = collectSemanticEmbeddingFailureDiagnostics(ctx, service, profile)
	if err != nil {
		t.Fatalf("collectSemanticEmbeddingFailureDiagnostics(after request) error = %v", err)
	}
	if len(diagnostics) != 1 || diagnostics[0].RetryState != "requested" || diagnostics[0].RetryRequestedAt != formatCatalogTime(requestedAt) {
		t.Fatalf("requested failure diagnostic = %#v", diagnostics)
	}
	if diagnostics[0].LastAttemptedAt != nowText {
		t.Fatalf("last attempted time changed by retry request: got %q want %q", diagnostics[0].LastAttemptedAt, nowText)
	}
	assets, err = service.catalog.loadSemanticBackfillAssets(ctx, sourceKey, profileImpl, 10)
	if err != nil {
		t.Fatalf("loadSemanticBackfillAssets(after retry) error = %v", err)
	}
	if len(assets) != 1 || assets[0].ID != "failed-asset" {
		t.Fatalf("assets after retry request = %#v, want failed-asset", assets)
	}

	failureAt := requestedAt.Add(time.Minute)
	if err := service.catalog.upsertSemanticVectorFailures(ctx, sourceKey, profileImpl, []semanticBackfillAssetFailure{{
		Asset: assets[0],
		Err:   errors.New("still unsupported"),
	}}, failureAt); err != nil {
		t.Fatalf("upsertSemanticVectorFailures(retry) error = %v", err)
	}
	var retryRequests int
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM semantic_vector_retry_requests`).Scan(&retryRequests); err != nil {
		t.Fatalf("count cleared retry requests: %v", err)
	}
	if retryRequests != 0 {
		t.Fatalf("retry request count after attempt = %d, want 0", retryRequests)
	}
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT asset_generation FROM semantic_state
		WHERE source_key = ? AND model_id = ?`, sourceKey, profile.ModelID).Scan(&generationAfter); err != nil {
		t.Fatalf("read generation after repeated failure: %v", err)
	}
	if generationAfter != generationBefore {
		t.Fatalf("generation after repeated failure = %d, want unchanged %d", generationAfter, generationBefore)
	}

	automaticRetryAt := time.Now().UTC().Add(-semanticBackfillFailureRetryInterval - time.Minute).Truncate(time.Microsecond)
	if _, err := service.catalog.db.ExecContext(ctx, `UPDATE semantic_vectors SET generated_at = ?
		WHERE source_key = ? AND upstream_asset_id = 'failed-asset' AND model_id = ?`,
		formatCatalogTime(automaticRetryAt), sourceKey, profile.ModelID,
	); err != nil {
		t.Fatalf("make failure automatically eligible: %v", err)
	}
	assets, err = service.catalog.loadSemanticBackfillAssets(ctx, sourceKey, profileImpl, 10)
	if err != nil {
		t.Fatalf("loadSemanticBackfillAssets(automatic retry) error = %v", err)
	}
	if len(assets) != 1 || assets[0].SelectedRetryRequestedAt != "" {
		t.Fatalf("automatic retry asset = %#v, want no selected manual request", assets)
	}

	requestDuringAttemptAt := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	retry, err = service.RequestSemanticEmbeddingFailureRetry(ctx, profile, nil, requestDuringAttemptAt)
	if err != nil {
		t.Fatalf("RequestSemanticEmbeddingFailureRetry(during attempt) error = %v", err)
	}
	if retry.RequestedCount != 1 {
		t.Fatalf("retry during attempt result = %#v, want one request", retry)
	}
	retryFailureAt := requestDuringAttemptAt.Add(time.Minute)
	if err := service.catalog.upsertSemanticVectorFailures(ctx, sourceKey, profileImpl, []semanticBackfillAssetFailure{{
		Asset: assets[0],
		Err:   errors.New("still unsupported after concurrent request"),
	}}, retryFailureAt); err != nil {
		t.Fatalf("upsertSemanticVectorFailures(concurrent request) error = %v", err)
	}
	var remainingRequestedAt string
	if err := service.catalog.db.QueryRowContext(ctx, `SELECT requested_at FROM semantic_vector_retry_requests
		WHERE source_key = ? AND upstream_asset_id = 'failed-asset' AND model_id = ?`, sourceKey, profile.ModelID).
		Scan(&remainingRequestedAt); err != nil {
		t.Fatalf("read retry request created during attempt: %v", err)
	}
	if remainingRequestedAt != formatCatalogTime(requestDuringAttemptAt) {
		t.Fatalf("remaining retry request = %q, want %q", remainingRequestedAt, formatCatalogTime(requestDuringAttemptAt))
	}
	assets, err = service.catalog.loadSemanticBackfillAssets(ctx, sourceKey, profileImpl, 10)
	if err != nil {
		t.Fatalf("loadSemanticBackfillAssets(after concurrent request failure) error = %v", err)
	}
	if len(assets) != 1 || assets[0].SelectedRetryRequestedAt != remainingRequestedAt {
		t.Fatalf("asset after concurrent request failure = %#v, want preserved immediate retry", assets)
	}
}

func collectSemanticEmbeddingFailureDiagnostics(ctx context.Context, service *Service, profile SemanticModelProfileStatus) ([]SemanticEmbeddingFailureDiagnosticRow, error) {
	diagnostics, err := service.OpenSemanticEmbeddingFailureDiagnostics(ctx, profile, nil)
	if err != nil {
		return nil, err
	}
	defer diagnostics.Close()
	rows := []SemanticEmbeddingFailureDiagnosticRow{}
	for {
		row, ok, err := diagnostics.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return rows, nil
		}
		rows = append(rows, row)
	}
}

func TestSemanticEmbeddingFailureDiagnosticsStreamsLargeResult(t *testing.T) {
	const (
		sourceKey    = "2222222222222222"
		failureCount = 5000
	)
	service, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey:   sourceKey,
		Name:        "Large Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.test",
		AccessToken: "test-key",
	}}, ServiceOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewServiceWithOptions() error = %v", err)
	}
	defer service.Close()

	profileImpl := testImageSemanticProfile{}
	profile := SemanticModelProfileStatus{
		ModelID:       profileImpl.ModelID(),
		VectorSpaceID: profileImpl.VectorSpaceID(),
		EmbeddingDim:  profileImpl.EmbeddingDim(),
		ProfileKind:   profileImpl.ProfileKind(),
		InputKind:     profileImpl.InputKind(),
	}
	nowText := formatCatalogTime(time.Now().UTC().Add(-time.Hour))
	tx, err := service.catalog.db.Begin()
	if err != nil {
		t.Fatalf("begin large diagnostics fixture: %v", err)
	}
	defer tx.Rollback()
	assetStatement, err := tx.Prepare(`INSERT INTO catalog_assets (
		source_key, datasource_kind, upstream_asset_id, media_type, filename, captured_at,
		visibility_status, first_seen_at, updated_at
	) VALUES (?, 'immich', ?, 'image', ?, ?, 'active', ?, ?)`)
	if err != nil {
		t.Fatalf("prepare large diagnostic assets: %v", err)
	}
	defer assetStatement.Close()
	vectorStatement, err := tx.Prepare(`INSERT INTO semantic_vectors (
		source_key, upstream_asset_id, model_id, vector_space_id, embedding_dim,
		payload_batch_id, vector_offset, vector_length, embedding_input, status, last_error, generated_at
	) VALUES (?, ?, ?, ?, ?, NULL, 0, 0, 'image', 'failed', ?, ?)`)
	if err != nil {
		t.Fatalf("prepare large diagnostic vectors: %v", err)
	}
	defer vectorStatement.Close()
	for index := range failureCount {
		assetID := fmt.Sprintf("failed-%05d", index)
		if _, err := assetStatement.Exec(sourceKey, assetID, assetID+".jpg", nowText, nowText, nowText); err != nil {
			t.Fatalf("insert large diagnostic asset %d: %v", index, err)
		}
		if _, err := vectorStatement.Exec(sourceKey, assetID, profile.ModelID, profile.VectorSpaceID, profile.EmbeddingDim, "systemic model failure", nowText); err != nil {
			t.Fatalf("insert large diagnostic vector %d: %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit large diagnostics fixture: %v", err)
	}

	diagnostics, err := service.OpenSemanticEmbeddingFailureDiagnostics(context.Background(), profile, nil)
	if err != nil {
		t.Fatalf("OpenSemanticEmbeddingFailureDiagnostics() error = %v", err)
	}
	defer diagnostics.Close()
	count := 0
	for {
		_, ok, err := diagnostics.Next()
		if err != nil {
			t.Fatalf("stream diagnostic row %d: %v", count, err)
		}
		if !ok {
			break
		}
		count++
	}
	if count != failureCount {
		t.Fatalf("streamed failure count = %d, want %d", count, failureCount)
	}
}
