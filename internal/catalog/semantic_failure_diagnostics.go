package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const semanticEmbeddingFailureRecommendedAction = "Inspect the source media; repair, replace, or remove it, then retry failed embeddings."

// SemanticEmbeddingFailureDiagnosticRow describes one current-profile failure
// for an Admin-only diagnostics export.
type SemanticEmbeddingFailureDiagnosticRow struct {
	SourceKey         string
	DatasourceKind    string
	AssetID           string
	Filename          string
	MediaType         string
	CapturedAt        string
	ModelID           string
	VectorSpaceID     string
	Status            string
	LastError         string
	LastAttemptedAt   string
	RetryEligibleAt   string
	RetryRequestedAt  string
	RetryState        string
	RecommendedAction string
}

// SemanticEmbeddingFailureRetryResult reports current-profile failed rows
// marked eligible for the next background-worker assignment.
type SemanticEmbeddingFailureRetryResult struct {
	RequestedCount int       `json:"requestedCount"`
	RequestedAt    time.Time `json:"requestedAt"`
}

// SemanticEmbeddingFailureDiagnostics incrementally reads one diagnostic row
// at a time from the catalog. Callers must close it.
type SemanticEmbeddingFailureDiagnostics struct {
	rows *sql.Rows
	now  time.Time
}

func SemanticEmbeddingFailureDiagnosticCSVHeader() []string {
	return []string{
		"source_key",
		"datasource_kind",
		"asset_id",
		"filename",
		"media_type",
		"captured_at",
		"model_id",
		"vector_space_id",
		"status",
		"last_error",
		"last_attempted_at",
		"retry_eligible_at",
		"retry_requested_at",
		"retry_state",
		"recommended_action",
	}
}

func (row SemanticEmbeddingFailureDiagnosticRow) CSVRecord() []string {
	return []string{
		row.SourceKey,
		row.DatasourceKind,
		row.AssetID,
		row.Filename,
		row.MediaType,
		row.CapturedAt,
		row.ModelID,
		row.VectorSpaceID,
		row.Status,
		row.LastError,
		row.LastAttemptedAt,
		row.RetryEligibleAt,
		row.RetryRequestedAt,
		row.RetryState,
		row.RecommendedAction,
	}
}

func (s *Service) OpenSemanticEmbeddingFailureDiagnostics(ctx context.Context, profile SemanticModelProfileStatus, sourceKeys []string) (*SemanticEmbeddingFailureDiagnostics, error) {
	if !s.catalogStoreEnabled() {
		return nil, ErrCatalogNotConfigured
	}
	sourceKeys = s.semanticDatasourceSourceKeysFor(sourceKeys)
	if len(sourceKeys) == 0 {
		return nil, ErrNoDatasourceConfigured
	}
	return s.catalog.openSemanticEmbeddingFailureDiagnostics(ctx, profile, sourceKeys, time.Now().UTC())
}

func (s *Service) RequestSemanticEmbeddingFailureRetry(ctx context.Context, profile SemanticModelProfileStatus, sourceKeys []string, requestedAt time.Time) (SemanticEmbeddingFailureRetryResult, error) {
	if !s.catalogStoreEnabled() {
		return SemanticEmbeddingFailureRetryResult{}, ErrCatalogNotConfigured
	}
	sourceKeys = s.semanticDatasourceSourceKeysFor(sourceKeys)
	if len(sourceKeys) == 0 {
		return SemanticEmbeddingFailureRetryResult{}, ErrNoDatasourceConfigured
	}
	return s.catalog.requestSemanticEmbeddingFailureRetry(ctx, profile, sourceKeys, requestedAt.UTC())
}

// Keep durable vector/retry identity separate from the source identity shown to
// the operator. Legacy failures remain visible until their canonical vector is
// written. Both diagnostics and retries use exactly the same target selection.
func semanticEmbeddingFailuresSQL(profile SemanticModelProfileStatus, sourceKeys []string) (string, []any) {
	inputsCTE, args := canonicalSemanticInputs(sourceKeys)
	where, whereArgs := semanticCatalogEligibilityWhere("", profile.InputKind, "a")
	args = append(args, whereArgs...)
	for _, key := range sourceKeys {
		args = append(args, key)
	}
	args = append(args, canonicalSemanticCorpusSourceKey, strings.TrimSpace(profile.ModelID), canonicalSemanticCorpusSourceKey,
		strings.TrimSpace(profile.ModelID), strings.TrimSpace(profile.VectorSpaceID), profile.EmbeddingDim)
	query := inputsCTE + `, failure_assets AS (
		SELECT a.source_key AS storage_source_key, a.upstream_asset_id AS storage_asset_id,
			a.source_key, a.upstream_asset_id AS asset_id, a.datasource_kind, a.filename, a.media_type, a.captured_at
		FROM catalog_assets a
		` + where + ` AND a.source_key IN (` + strings.TrimSuffix(strings.Repeat("?,", len(sourceKeys)), ",") + `)
			AND NOT EXISTS (
				SELECT 1 FROM semantic_vectors canonical
				WHERE canonical.source_key = ? AND canonical.upstream_asset_id = a.canonical_asset_id AND canonical.model_id = ?
			)
		UNION ALL
		SELECT ?, input.canonical_asset_id, a.source_key, a.upstream_asset_id,
			a.datasource_kind, a.filename, a.media_type, a.captured_at
		FROM resolved_inputs input
		JOIN catalog_assets a ON a.source_key = input.embedding_source_key AND a.upstream_asset_id = input.embedding_upstream_asset_id
		WHERE input.embedding_source_key <> ''
	)
	SELECT f.source_key, f.datasource_kind, f.asset_id, f.filename, f.media_type, f.captured_at,
		v.model_id, v.vector_space_id, v.status, COALESCE(v.last_error, '') AS last_error, v.generated_at,
		COALESCE(r.requested_at, '') AS requested_at, v.source_key AS storage_source_key, v.upstream_asset_id AS storage_asset_id
	FROM failure_assets f
	JOIN semantic_vectors v ON v.source_key = f.storage_source_key AND v.upstream_asset_id = f.storage_asset_id
	LEFT JOIN semantic_vector_retry_requests r
		ON r.source_key = v.source_key AND r.upstream_asset_id = v.upstream_asset_id AND r.model_id = v.model_id
	WHERE v.model_id = ? AND v.vector_space_id = ? AND v.embedding_dim = ? AND v.status = 'failed'`
	return query, args
}

func (s *CatalogStore) openSemanticEmbeddingFailureDiagnostics(ctx context.Context, profile SemanticModelProfileStatus, sourceKeys []string, now time.Time) (*SemanticEmbeddingFailureDiagnostics, error) {
	if s == nil || s.db == nil {
		return nil, ErrCatalogNotConfigured
	}
	if strings.TrimSpace(profile.ModelID) == "" || strings.TrimSpace(profile.VectorSpaceID) == "" || profile.EmbeddingDim <= 0 || len(sourceKeys) == 0 {
		return nil, ErrSemanticModelPackInvalid
	}
	query, args := semanticEmbeddingFailuresSQL(profile, sourceKeys)
	rows, err := s.queryDB().QueryContext(ctx, `SELECT source_key, datasource_kind, asset_id, filename, media_type, captured_at,
		model_id, vector_space_id, status, last_error, generated_at, requested_at
		FROM (`+query+`) ORDER BY generated_at DESC, source_key, asset_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("query semantic embedding failure diagnostics: %w", err)
	}
	return &SemanticEmbeddingFailureDiagnostics{rows: rows, now: now.UTC()}, nil
}

func (diagnostics *SemanticEmbeddingFailureDiagnostics) Next() (SemanticEmbeddingFailureDiagnosticRow, bool, error) {
	if diagnostics == nil || diagnostics.rows == nil {
		return SemanticEmbeddingFailureDiagnosticRow{}, false, nil
	}
	if !diagnostics.rows.Next() {
		if err := diagnostics.rows.Err(); err != nil {
			return SemanticEmbeddingFailureDiagnosticRow{}, false, fmt.Errorf("iterate semantic embedding failure diagnostics: %w", err)
		}
		return SemanticEmbeddingFailureDiagnosticRow{}, false, nil
	}
	var row SemanticEmbeddingFailureDiagnosticRow
	if err := diagnostics.rows.Scan(
		&row.SourceKey,
		&row.DatasourceKind,
		&row.AssetID,
		&row.Filename,
		&row.MediaType,
		&row.CapturedAt,
		&row.ModelID,
		&row.VectorSpaceID,
		&row.Status,
		&row.LastError,
		&row.LastAttemptedAt,
		&row.RetryRequestedAt,
	); err != nil {
		return SemanticEmbeddingFailureDiagnosticRow{}, false, fmt.Errorf("scan semantic embedding failure diagnostic: %w", err)
	}
	attemptedAt, parseErr := time.Parse(time.RFC3339Nano, row.LastAttemptedAt)
	if parseErr == nil {
		retryEligibleAt := attemptedAt.UTC().Add(semanticBackfillFailureRetryInterval)
		row.RetryEligibleAt = retryEligibleAt.Format(time.RFC3339Nano)
		switch {
		case strings.TrimSpace(row.RetryRequestedAt) != "":
			row.RetryState = "requested"
		case !retryEligibleAt.After(diagnostics.now):
			row.RetryState = "eligible"
		default:
			row.RetryState = "scheduled"
		}
	} else {
		row.RetryState = "eligible"
	}
	row.RecommendedAction = semanticEmbeddingFailureRecommendedAction
	return row, true, nil
}

func (diagnostics *SemanticEmbeddingFailureDiagnostics) Close() error {
	if diagnostics == nil || diagnostics.rows == nil {
		return nil
	}
	return diagnostics.rows.Close()
}

func (s *CatalogStore) requestSemanticEmbeddingFailureRetry(ctx context.Context, profile SemanticModelProfileStatus, sourceKeys []string, requestedAt time.Time) (SemanticEmbeddingFailureRetryResult, error) {
	if s == nil || s.db == nil {
		return SemanticEmbeddingFailureRetryResult{}, ErrCatalogNotConfigured
	}
	if strings.TrimSpace(profile.ModelID) == "" || strings.TrimSpace(profile.VectorSpaceID) == "" || profile.EmbeddingDim <= 0 || len(sourceKeys) == 0 {
		return SemanticEmbeddingFailureRetryResult{}, ErrSemanticModelPackInvalid
	}
	query, queryArgs := semanticEmbeddingFailuresSQL(profile, sourceKeys)
	args := append([]any{formatCatalogTime(requestedAt.UTC())}, queryArgs...)
	result, err := s.db.ExecContext(ctx, `INSERT INTO semantic_vector_retry_requests (
			source_key, upstream_asset_id, model_id, requested_at
		)
		SELECT storage_source_key, storage_asset_id, model_id, ? FROM (`+query+`) WHERE 1 = 1
		ON CONFLICT(source_key, upstream_asset_id, model_id) DO UPDATE SET
			requested_at = excluded.requested_at`, args...)
	if err != nil {
		return SemanticEmbeddingFailureRetryResult{}, fmt.Errorf("request semantic embedding failure retry: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return SemanticEmbeddingFailureRetryResult{}, fmt.Errorf("count semantic embedding failure retry requests: %w", err)
	}
	return SemanticEmbeddingFailureRetryResult{RequestedCount: int(count), RequestedAt: requestedAt.UTC()}, nil
}

func clearSemanticEmbeddingRetryRequestInTx(ctx context.Context, tx *sql.Tx, sourceKey string, assetID string, modelID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM semantic_vector_retry_requests
		WHERE source_key = ? AND upstream_asset_id = ? AND model_id = ?`, sourceKey, assetID, modelID); err != nil {
		return fmt.Errorf("clear semantic embedding retry request %q: %w", assetID, err)
	}
	return nil
}

func consumeSelectedSemanticEmbeddingRetryRequestInTx(ctx context.Context, tx *sql.Tx, sourceKey string, assetID string, modelID string, selectedRequestedAt string) error {
	selectedRequestedAt = strings.TrimSpace(selectedRequestedAt)
	if selectedRequestedAt == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM semantic_vector_retry_requests
		WHERE source_key = ? AND upstream_asset_id = ? AND model_id = ? AND requested_at = ?`,
		sourceKey, assetID, modelID, selectedRequestedAt,
	); err != nil {
		return fmt.Errorf("consume semantic embedding retry request %q: %w", assetID, err)
	}
	return nil
}
