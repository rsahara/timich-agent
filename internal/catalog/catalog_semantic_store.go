package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	semanticHNSWMaxNeighbors   = 16
	semanticHNSWMaxLevel       = 5
	semanticHNSWEfSearch       = 256
	semanticHNSWEfConstruction = 512

	semanticDiversityMinCandidates               = 80
	semanticDiversityCandidateMultiplier         = 4
	semanticSearchVisitBudget                    = 4096
	semanticNearDuplicateSimilarity      float32 = 0.86
	semanticBinaryStatusOverlayTimeout           = 100 * time.Millisecond
	semanticBackfillFailureRetryInterval         = 30 * time.Minute
	semanticIndexJobLeaseDuration                = 11 * time.Minute
	semanticIndexJobLeaseRenewInterval           = time.Minute
	semanticIndexJobTransitionTimeout            = 5 * time.Second

	semanticMessageIndexMissing             = "semantic_index_missing"
	semanticMessageIndexUnavailable         = "semantic_index_unavailable"
	semanticMessageIndexBackfilling         = "semantic_index_backfilling"
	semanticMessageIndexMissingFallback     = "semantic_index_missing_filename_fallback"
	semanticMessageIndexUnavailableFallback = "semantic_index_unavailable_filename_fallback"
)

type semanticAsset struct {
	SourceKey     string
	ID            string
	MediaType     string
	Filename      string
	CapturedAt    time.Time
	Duration      *string
	Vector        []float32
	VectorInput   string
	VectorPayload semanticVectorPayloadRef
	MaxLevel      int
}

type semanticScoredAsset struct {
	Asset      semanticAsset
	Similarity float32
}

type SemanticBackfillOptions struct {
	ImageLoader SemanticImageLoader
	MaxAssets   int
	Workers     int
}

type SemanticBackfillResult struct {
	ProcessedVectorCount int                         `json:"processedVectorCount"`
	IndexedVectorCount   int                         `json:"indexedVectorCount"`
	StartedAt            time.Time                   `json:"startedAt"`
	CompletedAt          time.Time                   `json:"completedAt"`
	Status               SemanticModelBackfillStatus `json:"status"`
	SourceStatuses       []SemanticBackfillSource    `json:"sourceStatuses,omitempty"`
}

type semanticBackfillAssetFailure struct {
	Asset semanticAsset
	Err   error
}

type SemanticIndexPublishResult struct {
	Published          bool                        `json:"published"`
	SourceKey          string                      `json:"sourceKey,omitempty"`
	ModelID            string                      `json:"modelId,omitempty"`
	VectorSpaceID      string                      `json:"vectorSpaceId,omitempty"`
	IndexedVectorCount int                         `json:"indexedVectorCount"`
	StartedAt          time.Time                   `json:"startedAt,omitempty"`
	CompletedAt        time.Time                   `json:"completedAt,omitempty"`
	Status             SemanticModelBackfillStatus `json:"status"`
}

type SemanticBackfillSource struct {
	SourceKey string                      `json:"sourceKey"`
	Status    SemanticModelBackfillStatus `json:"status"`
}

type SemanticImageLoader interface {
	LoadSemanticImage(ctx context.Context, sourceKey string, upstreamAssetID string) (*semanticImageEmbeddingInput, error)
}

type semanticIndexJob struct {
	ID              int64
	SourceKey       string
	ModelID         string
	VectorSpaceID   string
	EmbeddingDim    int
	Attempts        int
	AssetGeneration int64
}

type semanticIndexTraversalSession struct {
	reader       *semanticBinaryIndexReader
	traversal    *semanticBinarySearchSession
	Semantic     CatalogSemanticStatus
	IndexedCount int
	Exhausted    bool
}

func (s *semanticIndexTraversalSession) Close() error {
	if s == nil || s.reader == nil {
		return nil
	}
	return s.reader.Close()
}

func (s *semanticIndexTraversalSession) Advance(ctx context.Context, additionalVisits int) ([]semanticScoredAsset, error) {
	if s == nil || s.Exhausted || s.traversal == nil {
		return []semanticScoredAsset{}, nil
	}
	scored, exhausted, err := s.traversal.advance(ctx, additionalVisits)
	if err != nil {
		return nil, err
	}
	s.Exhausted = exhausted
	return scored, nil
}

func (s *CatalogStore) SearchSemanticCapabilities(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile) *AssetSearchSemanticCapabilities {
	if profile == nil {
		return &AssetSearchSemanticCapabilities{
			Status:      "missing",
			MessageCode: semanticMessageIndexMissing,
		}
	}
	if s == nil || s.db == nil {
		return &AssetSearchSemanticCapabilities{
			Status:      "missing",
			MessageCode: semanticMessageIndexMissing,
		}
	}
	status, err := s.semanticStatus(ctx, strings.TrimSpace(sourceKey), profile)
	if err != nil {
		return &AssetSearchSemanticCapabilities{
			Status:      "unknown",
			MessageCode: semanticMessageIndexUnavailable,
		}
	}
	return semanticCapabilities(status, profile)
}

func scanSemanticAssets(rows *sql.Rows, label string) ([]semanticAsset, error) {
	assets := []semanticAsset{}
	for rows.Next() {
		var asset semanticAsset
		var capturedAtText string
		var duration sql.NullString
		if err := rows.Scan(&asset.SourceKey, &asset.ID, &asset.MediaType, &asset.Filename, &capturedAtText, &duration); err != nil {
			return nil, fmt.Errorf("scan catalog semantic %s asset: %w", label, err)
		}
		asset.SourceKey = strings.TrimSpace(asset.SourceKey)
		capturedAt, err := time.Parse(time.RFC3339Nano, capturedAtText)
		if err != nil {
			return nil, fmt.Errorf("parse catalog semantic %s captured_at: %w", label, err)
		}
		asset.CapturedAt = capturedAt.UTC()
		if duration.Valid {
			value := duration.String
			asset.Duration = &value
		}
		assets = append(assets, asset)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate catalog semantic %s assets: %w", label, err)
	}
	return assets, nil
}

func semanticCatalogEligibilityWhere(sourceKey string, inputKind string, alias string) (string, []any) {
	prefix := strings.TrimSpace(alias)
	if prefix != "" {
		prefix += "."
	}
	clauses := []string{
		prefix + "visibility_status = 'active'",
	}
	args := []any{}
	if sourceKey = strings.TrimSpace(sourceKey); sourceKey != "" {
		clauses = append([]string{prefix + "source_key = ?"}, clauses...)
		args = append(args, sourceKey)
	}
	if strings.TrimSpace(inputKind) == semanticInputKindImage {
		clauses = append(clauses, `(
			`+prefix+`datasource_kind != 'local_filesystem'
			OR (
				`+prefix+`datasource_kind = 'local_filesystem'
				AND `+prefix+`media_type = 'image'
				AND EXISTS (
					SELECT 1 FROM local_renditions r
					WHERE r.source_key = `+prefix+`source_key
						AND r.asset_id = `+prefix+`upstream_asset_id
						AND r.kind IN ('detail_preview', 'preview')
						AND r.status = 'ready'
						AND r.relative_path IS NOT NULL
				)
			)
		)`)
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

func (s *CatalogStore) upsertSemanticStateInTx(ctx context.Context, tx *sql.Tx, sourceKey string, semantic CatalogSemanticStatus, now time.Time) error {
	var builtAt any
	if semantic.BuiltAt != nil {
		builtAt = formatCatalogTime(semantic.BuiltAt.UTC())
	}
	var lastError any
	if strings.TrimSpace(semantic.LastError) != "" {
		lastError = semantic.LastError
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			built_at, last_error, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_key, model_id) DO UPDATE SET
			vector_space_id = excluded.vector_space_id,
			status = excluded.status,
			embedding_dim = excluded.embedding_dim,
			completed_vector_count = excluded.completed_vector_count,
			last_error = excluded.last_error,
			updated_at = excluded.updated_at`,
		sourceKey,
		semantic.ModelID,
		semantic.VectorSpaceID,
		semantic.Status,
		semantic.EmbeddingDim,
		semantic.CompletedVectorCount,
		semantic.IndexedVectorCount,
		semantic.AssetGeneration,
		semantic.IndexedGeneration,
		builtAt,
		lastError,
		formatCatalogTime(now.UTC()),
	)
	if err != nil {
		return fmt.Errorf("upsert catalog semantic state: %w", err)
	}
	return nil
}

func (s *CatalogStore) publishSemanticStateInTx(ctx context.Context, tx *sql.Tx, sourceKey string, semantic CatalogSemanticStatus, expectedGeneration int64, now time.Time) error {
	var builtAt any
	if semantic.BuiltAt != nil {
		builtAt = formatCatalogTime(semantic.BuiltAt.UTC())
	}
	var lastError any
	if strings.TrimSpace(semantic.LastError) != "" {
		lastError = semantic.LastError
	}
	nowText := formatCatalogTime(now.UTC())
	result, err := tx.ExecContext(ctx, `UPDATE semantic_state SET
			vector_space_id = ?, status = ?, embedding_dim = ?,
			completed_vector_count = ?, indexed_vector_count = ?, indexed_generation = ?,
			built_at = ?, last_error = ?, updated_at = ?
		WHERE source_key = ? AND model_id = ? AND asset_generation = ?`,
		semantic.VectorSpaceID,
		semantic.Status,
		semantic.EmbeddingDim,
		semantic.CompletedVectorCount,
		semantic.IndexedVectorCount,
		expectedGeneration,
		builtAt,
		lastError,
		nowText,
		strings.TrimSpace(sourceKey),
		semantic.ModelID,
		expectedGeneration,
	)
	if err != nil {
		return fmt.Errorf("update published catalog semantic state: %w", err)
	}
	if count, _ := result.RowsAffected(); count == 1 {
		return nil
	}
	result, err = tx.ExecContext(ctx, `INSERT INTO semantic_state (
			source_key, model_id, vector_space_id, status, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			built_at, last_error, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_key, model_id) DO NOTHING`,
		strings.TrimSpace(sourceKey),
		semantic.ModelID,
		semantic.VectorSpaceID,
		semantic.Status,
		semantic.EmbeddingDim,
		semantic.CompletedVectorCount,
		semantic.IndexedVectorCount,
		expectedGeneration,
		expectedGeneration,
		builtAt,
		lastError,
		nowText,
	)
	if err != nil {
		return fmt.Errorf("insert published catalog semantic state: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("semantic asset generation changed before publish finalization: want %d", expectedGeneration)
	}
	return nil
}

func (s *CatalogStore) semanticStatus(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile) (CatalogSemanticStatus, error) {
	if s == nil || s.db == nil {
		return CatalogSemanticStatus{}, ErrCatalogNotConfigured
	}
	return semanticStatusFromDB(ctx, s.queryDB(), sourceKey, profile)
}

func semanticStatusFromDB(ctx context.Context, db *sql.DB, sourceKey string, profile semanticEmbeddingProfile) (CatalogSemanticStatus, error) {
	if db == nil {
		return CatalogSemanticStatus{}, ErrCatalogNotConfigured
	}
	var status CatalogSemanticStatus
	var builtAt sql.NullString
	var lastError sql.NullString
	err := db.QueryRowContext(ctx, `SELECT status, model_id, vector_space_id, embedding_dim,
			completed_vector_count, indexed_vector_count, asset_generation, indexed_generation,
			built_at, last_error
		FROM semantic_state WHERE source_key = ? AND model_id = ?`, sourceKey, profile.ModelID()).
		Scan(
			&status.Status,
			&status.ModelID,
			&status.VectorSpaceID,
			&status.EmbeddingDim,
			&status.CompletedVectorCount,
			&status.IndexedVectorCount,
			&status.AssetGeneration,
			&status.IndexedGeneration,
			&builtAt,
			&lastError,
		)
	if err != nil {
		if err == sql.ErrNoRows {
			return CatalogSemanticStatus{
				Status:            "missing",
				ModelID:           profile.ModelID(),
				VectorSpaceID:     profile.VectorSpaceID(),
				EmbeddingDim:      profile.EmbeddingDim(),
				IndexedGeneration: -1,
				ProfileKind:       profile.ProfileKind(),
				InputKind:         profile.InputKind(),
				ModelPack:         profile.ModelPackStatus(),
			}, nil
		}
		return CatalogSemanticStatus{}, fmt.Errorf("read catalog semantic state: %w", err)
	}
	if builtAt.Valid {
		parsed, parseErr := time.Parse(time.RFC3339Nano, builtAt.String)
		if parseErr == nil {
			status.BuiltAt = &parsed
		}
	}
	if lastError.Valid {
		status.LastError = lastError.String
	}
	if status.AssetGeneration != status.IndexedGeneration && status.IndexedGeneration >= 0 && status.IndexedVectorCount > 0 {
		status.Status = semanticBackfillStatusIndexing
	}
	return normalizeCatalogSemanticStatus(status, profile), nil
}

func (s *CatalogStore) SemanticBackfillStatus(ctx context.Context, sourceKey string, profile SemanticModelProfileStatus) (SemanticModelBackfillStatus, error) {
	if s == nil || s.db == nil {
		return SemanticModelBackfillStatus{}, ErrCatalogNotConfigured
	}
	started := time.Now()
	sourceKey = strings.TrimSpace(sourceKey)
	modelID := strings.TrimSpace(profile.ModelID)
	vectorSpaceID := strings.TrimSpace(profile.VectorSpaceID)
	if sourceKey == "" || modelID == "" || vectorSpaceID == "" {
		return SemanticModelBackfillStatus{}, ErrCatalogNotConfigured
	}

	status := SemanticModelBackfillStatus{
		SourceKind:        "catalog",
		ModelID:           modelID,
		VectorSpaceID:     vectorSpaceID,
		EmbeddingDim:      profile.EmbeddingDim,
		IndexedGeneration: -1,
	}
	assetGeneration, indexedGeneration, err := s.semanticIndexGenerations(ctx, sourceKey, modelID)
	if err != nil {
		return SemanticModelBackfillStatus{}, err
	}
	status.AssetGeneration = assetGeneration
	status.IndexedGeneration = indexedGeneration
	if assetGeneration != indexedGeneration {
		status.GenerationMismatchSourceCount = 1
	}
	stepStarted := time.Now()
	log.Printf(
		"timich-agent catalog semantic backfill status step started source_key=%s model=%s vector_space=%s step=last_published",
		sourceKey,
		modelID,
		vectorSpaceID,
	)
	lastPublishedAt, err := s.semanticBackfillLastPublishedAt(ctx, sourceKey, modelID, vectorSpaceID)
	if err != nil {
		return SemanticModelBackfillStatus{}, err
	}
	log.Printf(
		"timich-agent catalog semantic backfill status step completed source_key=%s model=%s vector_space=%s step=last_published elapsed=%s",
		sourceKey,
		modelID,
		vectorSpaceID,
		time.Since(stepStarted).Round(time.Millisecond),
	)
	status.LastPublishedAt = lastPublishedAt
	stepStarted = time.Now()
	log.Printf(
		"timich-agent catalog semantic backfill status step started source_key=%s model=%s vector_space=%s step=eligible_count",
		sourceKey,
		modelID,
		vectorSpaceID,
	)
	eligibleCount, err := s.semanticEligibleAssetCount(ctx, sourceKey, profile.InputKind)
	if err != nil {
		return SemanticModelBackfillStatus{}, err
	}
	log.Printf(
		"timich-agent catalog semantic backfill status step completed source_key=%s model=%s vector_space=%s step=eligible_count count=%d elapsed=%s",
		sourceKey,
		modelID,
		vectorSpaceID,
		eligibleCount,
		time.Since(stepStarted).Round(time.Millisecond),
	)
	status.EligibleAssetCount = eligibleCount
	stepStarted = time.Now()
	log.Printf(
		"timich-agent catalog semantic backfill status step started source_key=%s model=%s vector_space=%s step=vector_progress",
		sourceKey,
		modelID,
		vectorSpaceID,
	)
	completedCount, indexedCount, err := s.semanticVectorProgressCounts(ctx, sourceKey, modelID, vectorSpaceID, profile.InputKind)
	if err != nil {
		return SemanticModelBackfillStatus{}, err
	}
	log.Printf(
		"timich-agent catalog semantic backfill status step completed source_key=%s model=%s vector_space=%s step=vector_progress completed=%d indexed=%d elapsed=%s",
		sourceKey,
		modelID,
		vectorSpaceID,
		completedCount,
		indexedCount,
		time.Since(stepStarted).Round(time.Millisecond),
	)
	status.CompletedVectorCount = completedCount
	status.IndexedVectorCount = indexedCount
	eligibleNow, nextEligibleAt, err := s.semanticBackfillEligibilityState(ctx, sourceKey, profile, time.Now().UTC())
	if err != nil {
		return SemanticModelBackfillStatus{}, err
	}
	status.EligibleNowVectorCount = eligibleNow
	status.NextEligibleAt = nextEligibleAt
	stepStarted = time.Now()
	log.Printf(
		"timich-agent catalog semantic backfill status step started source_key=%s model=%s vector_space=%s step=index_jobs",
		sourceKey,
		modelID,
		vectorSpaceID,
	)
	pendingJobs, failedJobs, eligibleIndexJobs, nextIndexJobEligibleAt, err := s.semanticIndexJobState(ctx, sourceKey, modelID, vectorSpaceID, time.Now().UTC())
	if err != nil {
		return SemanticModelBackfillStatus{}, err
	}
	log.Printf(
		"timich-agent catalog semantic backfill status step completed source_key=%s model=%s vector_space=%s step=index_jobs pending=%d failed=%d elapsed=%s total_elapsed=%s",
		sourceKey,
		modelID,
		vectorSpaceID,
		pendingJobs,
		failedJobs,
		time.Since(stepStarted).Round(time.Millisecond),
		time.Since(started).Round(time.Millisecond),
	)
	status.PendingIndexJobCount = pendingJobs
	status.FailedIndexJobCount = failedJobs
	status.EligibleIndexJobCount = eligibleIndexJobs
	if nextIndexJobEligibleAt != nil && (status.NextEligibleAt == nil || nextIndexJobEligibleAt.Before(*status.NextEligibleAt)) {
		nextEligibleAt := nextIndexJobEligibleAt.UTC()
		status.NextEligibleAt = &nextEligibleAt
	}
	status.RemainingVectorCount = status.EligibleAssetCount - status.CompletedVectorCount
	if status.RemainingVectorCount < 0 {
		status.RemainingVectorCount = 0
	}
	status.Status = semanticBackfillStatusReady
	status.MessageCode = semanticBackfillMessageReady
	switch {
	case status.CompletedVectorCount == 0 && status.EligibleAssetCount > 0:
		status.Status = semanticBackfillStatusPending
		status.MessageCode = semanticBackfillMessagePending
	case status.CompletedVectorCount < status.EligibleAssetCount:
		status.Status = semanticBackfillStatusBackfilling
		status.MessageCode = semanticBackfillMessageIncomplete
	case status.AssetGeneration != status.IndexedGeneration ||
		status.IndexedVectorCount < status.CompletedVectorCount ||
		status.PendingIndexJobCount > 0 || status.FailedIndexJobCount > 0:
		status.Status = semanticBackfillStatusIndexing
		status.MessageCode = semanticBackfillMessageIndexing
	}
	return status, nil
}

func (s *CatalogStore) semanticEligibleAssetCount(ctx context.Context, sourceKey string, inputKind string) (int, error) {
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" {
		return 0, ErrCatalogNotConfigured
	}
	db := s.queryDB()
	if strings.TrimSpace(inputKind) == semanticInputKindImage {
		var datasourceKind string
		err := db.QueryRowContext(ctx, `SELECT datasource_kind
			FROM catalog_assets
			WHERE source_key = ?
			LIMIT 1`, sourceKey).Scan(&datasourceKind)
		if err != nil && err != sql.ErrNoRows {
			return 0, fmt.Errorf("read catalog semantic datasource kind: %w", err)
		}
		if err == sql.ErrNoRows {
			return 0, nil
		}
		if datasourceKind != "local_filesystem" {
			var count int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*)
				FROM catalog_assets
				WHERE source_key = ? AND visibility_status = 'active'`, sourceKey).Scan(&count); err != nil {
				return 0, fmt.Errorf("count catalog candidate-eligible assets: %w", err)
			}
			return count, nil
		}
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*)
			FROM catalog_assets a
			WHERE a.source_key = ?
				AND a.datasource_kind = 'local_filesystem'
				AND a.visibility_status = 'active'
				AND a.media_type = 'image'
				AND EXISTS (
					SELECT 1 FROM local_renditions r
					WHERE r.source_key = a.source_key
						AND r.asset_id = a.upstream_asset_id
						AND r.kind IN ('detail_preview', 'preview')
						AND r.status = 'ready'
						AND r.relative_path IS NOT NULL
				)`, sourceKey).Scan(&count); err != nil {
			return 0, fmt.Errorf("count local catalog candidate-eligible assets: %w", err)
		}
		return count, nil
	}

	where, args := semanticCatalogEligibilityWhere(sourceKey, inputKind, "a")
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM catalog_assets a `+where, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count catalog candidate-eligible assets: %w", err)
	}
	return count, nil
}

func (s *CatalogStore) semanticBackfillEligibilityState(ctx context.Context, sourceKey string, profile SemanticModelProfileStatus, now time.Time) (int, *time.Time, error) {
	where, whereArgs := semanticCatalogEligibilityWhere(sourceKey, profile.InputKind, "a")
	retryBefore := formatCatalogTime(now.UTC().Add(-semanticBackfillFailureRetryInterval))
	args := append([]any{strings.TrimSpace(profile.ModelID)}, whereArgs...)
	args = append(args,
		strings.TrimSpace(profile.VectorSpaceID), profile.EmbeddingDim, retryBefore,
		strings.TrimSpace(profile.VectorSpaceID), profile.EmbeddingDim, retryBefore,
	)
	var eligibleNow int
	var firstDeferredFailure sql.NullString
	err := s.queryDB().QueryRowContext(ctx, `WITH candidates AS (
			SELECT v.upstream_asset_id AS vector_asset_id,
				v.vector_space_id,
				v.embedding_dim,
				v.status,
				v.generated_at
			FROM catalog_assets a
			LEFT JOIN semantic_vectors v
				ON v.source_key = a.source_key
				AND v.upstream_asset_id = a.upstream_asset_id
				AND v.model_id = ?
			`+where+`
		)
		SELECT COALESCE(SUM(CASE WHEN
				vector_asset_id IS NULL
				OR vector_space_id != ?
				OR embedding_dim != ?
				OR status NOT IN ('ready', 'failed')
				OR (status = 'failed' AND (generated_at IS NULL OR generated_at <= ?))
			THEN 1 ELSE 0 END), 0),
			MIN(CASE WHEN
				vector_space_id = ?
				AND embedding_dim = ?
				AND status = 'failed'
				AND generated_at > ?
			THEN generated_at END)
		FROM candidates`, args...).Scan(&eligibleNow, &firstDeferredFailure)
	if err != nil {
		return 0, nil, fmt.Errorf("read semantic backfill eligibility state: %w", err)
	}
	if !firstDeferredFailure.Valid || strings.TrimSpace(firstDeferredFailure.String) == "" {
		return eligibleNow, nil, nil
	}
	generatedAt, err := time.Parse(time.RFC3339Nano, firstDeferredFailure.String)
	if err != nil {
		return 0, nil, fmt.Errorf("parse semantic backfill next eligible time: %w", err)
	}
	nextEligibleAt := generatedAt.UTC().Add(semanticBackfillFailureRetryInterval)
	return eligibleNow, &nextEligibleAt, nil
}

func (s *CatalogStore) semanticVectorProgressCounts(ctx context.Context, sourceKey string, modelID string, vectorSpaceID string, inputKind string) (int, int, error) {
	db := s.queryDB()
	var completed int
	var indexed int
	where, args := semanticCatalogEligibilityWhere(sourceKey, inputKind, "a")
	completedArgs := append(append([]any{}, args...), strings.TrimSpace(modelID), strings.TrimSpace(vectorSpaceID))
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM catalog_assets a
		JOIN semantic_vectors v
			ON v.source_key = a.source_key
			AND v.upstream_asset_id = a.upstream_asset_id
		`+where+`
			AND v.model_id = ?
			AND v.vector_space_id = ?
			AND v.status = 'ready'`,
		completedArgs...,
	).Scan(&completed); err != nil {
		return 0, 0, fmt.Errorf("count semantic vector progress: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(indexed_vector_count, 0)
		FROM semantic_state
		WHERE source_key = ? AND model_id = ? AND vector_space_id = ?`,
		strings.TrimSpace(sourceKey), strings.TrimSpace(modelID), strings.TrimSpace(vectorSpaceID),
	).Scan(&indexed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("count indexed semantic vector progress: %w", err)
	}
	return completed, indexed, nil
}

func (s *CatalogStore) semanticBackfillLastPublishedAt(ctx context.Context, sourceKey string, modelID string, vectorSpaceID string) (*time.Time, error) {
	db := s.queryDB()
	var indexedAt sql.NullString
	err := db.QueryRowContext(ctx, `SELECT built_at
		FROM semantic_state
		WHERE source_key = ?
			AND model_id = ?
			AND vector_space_id = ?`,
		sourceKey, modelID, vectorSpaceID,
	).Scan(&indexedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read semantic hnsw last publish time: %w", err)
	}
	if !indexedAt.Valid || strings.TrimSpace(indexedAt.String) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, indexedAt.String)
	if err != nil {
		return nil, nil
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func (s *CatalogStore) semanticIndexGenerations(ctx context.Context, sourceKey string, modelID string) (int64, int64, error) {
	var assetGeneration int64
	indexedGeneration := int64(-1)
	err := s.queryDB().QueryRowContext(ctx, `SELECT asset_generation, indexed_generation
		FROM semantic_state
		WHERE source_key = ? AND model_id = ?`, strings.TrimSpace(sourceKey), strings.TrimSpace(modelID)).
		Scan(&assetGeneration, &indexedGeneration)
	if err == sql.ErrNoRows {
		return 0, -1, nil
	}
	if err != nil {
		return 0, -1, fmt.Errorf("read semantic index generations: %w", err)
	}
	return assetGeneration, indexedGeneration, nil
}

func (s *CatalogStore) BackfillSemanticVectors(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile, startedAt time.Time, options SemanticBackfillOptions) (SemanticBackfillResult, error) {
	if s == nil || s.db == nil {
		return SemanticBackfillResult{}, ErrCatalogNotConfigured
	}
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" || profile == nil {
		return SemanticBackfillResult{}, ErrCatalogNotConfigured
	}
	limit := options.MaxAssets
	if limit < 0 {
		limit = 0
	}
	if limit > 100 {
		limit = 100
	}

	assets := []semanticAsset{}
	var err error
	if limit > 0 {
		assets, err = s.loadSemanticBackfillAssets(ctx, sourceKey, profile, limit)
		if err != nil {
			return SemanticBackfillResult{}, err
		}
	}
	embeddedAssets := assets
	failedAssets := []semanticBackfillAssetFailure{}
	if len(assets) > 0 {
		embeddedAssets, failedAssets, err = s.embedSemanticBackfillAssets(ctx, profile, assets, options)
		if err != nil {
			return SemanticBackfillResult{}, err
		}
	}

	now := time.Now().UTC()
	if len(embeddedAssets) > 0 {
		if err := s.upsertSemanticVectors(ctx, sourceKey, profile, embeddedAssets, now); err != nil {
			return SemanticBackfillResult{}, err
		}
	}
	if len(failedAssets) > 0 {
		if err := s.upsertSemanticVectorFailures(ctx, sourceKey, profile, failedAssets, now); err != nil {
			return SemanticBackfillResult{}, err
		}
	}
	profileStatus := SemanticModelProfileStatus{
		ModelID:       profile.ModelID(),
		VectorSpaceID: profile.VectorSpaceID(),
		EmbeddingDim:  profile.EmbeddingDim(),
		Role:          semanticModelRoleCandidate,
		ProfileKind:   profile.ProfileKind(),
		InputKind:     profile.InputKind(),
		ModelPack:     profile.ModelPackStatus(),
	}
	status, err := s.SemanticBackfillStatus(ctx, sourceKey, profileStatus)
	if err != nil {
		return SemanticBackfillResult{}, err
	}
	if err := s.upsertSemanticBackfillState(ctx, sourceKey, profile, status, now); err != nil {
		return SemanticBackfillResult{}, err
	}
	return SemanticBackfillResult{
		ProcessedVectorCount: len(assets),
		IndexedVectorCount:   status.IndexedVectorCount,
		StartedAt:            startedAt.UTC(),
		CompletedAt:          now,
		Status:               status,
	}, nil
}

func (s *CatalogStore) embedSemanticBackfillAssets(ctx context.Context, profile semanticEmbeddingProfile, assets []semanticAsset, options SemanticBackfillOptions) ([]semanticAsset, []semanticBackfillAssetFailure, error) {
	errorsByIndex := make([]error, len(assets))
	workerCount := semanticBackfillWorkerCount(options.Workers, len(assets))
	if workerCount <= 1 {
		for index := range assets {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			err := embedSemanticBackfillAsset(ctx, profile, options.ImageLoader, &assets[index])
			if err != nil && !errors.Is(err, ErrSemanticAssetInput) {
				return nil, nil, err
			}
			errorsByIndex[index] = err
		}
	} else {
		workCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		jobs := make(chan int)
		var wg sync.WaitGroup
		var fatalMu sync.Mutex
		var fatalErr error
		for worker := 0; worker < workerCount; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for index := range jobs {
					err := embedSemanticBackfillAsset(workCtx, profile, options.ImageLoader, &assets[index])
					if err != nil && !errors.Is(err, ErrSemanticAssetInput) {
						fatalMu.Lock()
						if fatalErr == nil {
							fatalErr = err
							cancel()
						}
						fatalMu.Unlock()
						continue
					}
					errorsByIndex[index] = err
				}
			}()
		}

	sendLoop:
		for index := range assets {
			select {
			case <-workCtx.Done():
				break sendLoop
			case jobs <- index:
			}
		}
		close(jobs)
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		fatalMu.Lock()
		fatal := fatalErr
		fatalMu.Unlock()
		if fatal != nil {
			return nil, nil, fatal
		}
	}

	embedded := make([]semanticAsset, 0, len(assets))
	failed := make([]semanticBackfillAssetFailure, 0)
	for index, asset := range assets {
		if errorsByIndex[index] != nil {
			failed = append(failed, semanticBackfillAssetFailure{Asset: asset, Err: errorsByIndex[index]})
			continue
		}
		embedded = append(embedded, asset)
	}
	return embedded, failed, nil
}

func embedSemanticBackfillAsset(ctx context.Context, profile semanticEmbeddingProfile, imageLoader SemanticImageLoader, asset *semanticAsset) error {
	var image *semanticImageEmbeddingInput
	if profile.InputKind() == semanticInputKindImage {
		if imageLoader == nil {
			return fmt.Errorf("semantic image input loader is not configured for model %q", profile.ModelID())
		}
		loaded, err := imageLoader.LoadSemanticImage(ctx, asset.SourceKey, asset.ID)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if errors.Is(err, ErrSemanticAssetInput) {
				return fmt.Errorf("load catalog semantic image %q: %w", asset.ID, err)
			}
			return fmt.Errorf("%w: load catalog semantic image %q: %v", ErrSemanticSourceUnavailable, asset.ID, err)
		}
		if loaded == nil || len(loaded.Bytes) == 0 {
			return fmt.Errorf("%w: load catalog semantic image %q: empty image", ErrSemanticAssetInput, asset.ID)
		}
		image = loaded
	}
	embedding, err := profile.EmbedSemanticAsset(ctx, semanticAssetEmbeddingInput{Asset: *asset, Image: image})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, ErrSemanticAssetInput) {
			return fmt.Errorf("embed semantic candidate asset %q: %w", asset.ID, err)
		}
		return fmt.Errorf("%w: embed semantic candidate asset %q: %v", ErrSemanticRuntimeUnavailable, asset.ID, err)
	}
	asset.Vector = embedding.Vector
	asset.VectorInput = embedding.Input
	asset.MaxLevel = semanticHNSWLevel(asset.ID)
	return nil
}

func semanticBackfillWorkerCount(configured int, assetCount int) int {
	if assetCount <= 1 {
		return 1
	}
	if configured <= 1 {
		return 1
	}
	return min(configured, assetCount)
}

func (s *CatalogStore) ReconcileSemanticIndexJobs(ctx context.Context, sourceKeys []string, profile semanticEmbeddingProfile, allowPartial bool, now time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, ErrCatalogNotConfigured
	}
	if profile == nil {
		return 0, ErrCatalogNotConfigured
	}
	sourceKeys = normalizeSemanticIndexJobSourceKeys(sourceKeys)
	if len(sourceKeys) == 0 {
		return 0, nil
	}
	profileStatus := SemanticModelProfileStatus{
		ModelID:       profile.ModelID(),
		VectorSpaceID: profile.VectorSpaceID(),
		EmbeddingDim:  profile.EmbeddingDim(),
		Role:          semanticModelRoleCandidate,
		ProfileKind:   profile.ProfileKind(),
		InputKind:     profile.InputKind(),
		ModelPack:     profile.ModelPackStatus(),
	}
	enqueued := 0
	for _, sourceKey := range sourceKeys {
		status, err := s.SemanticBackfillStatus(ctx, sourceKey, profileStatus)
		if err != nil {
			return enqueued, err
		}
		if !s.semanticIndexPublishNeeded(ctx, sourceKey, profile, status, allowPartial) {
			continue
		}
		if err := s.enqueueSemanticIndexJob(ctx, sourceKey, profile, now); err != nil {
			return enqueued, err
		}
		enqueued++
	}
	return enqueued, nil
}

func (s *CatalogStore) SemanticIndexPublishNeeded(ctx context.Context, sourceKeys []string, profile semanticEmbeddingProfile, allowPartial bool) (bool, int, error) {
	if s == nil || s.db == nil {
		return false, 0, ErrCatalogNotConfigured
	}
	if profile == nil {
		return false, 0, ErrCatalogNotConfigured
	}
	sourceKeys = normalizeSemanticIndexJobSourceKeys(sourceKeys)
	if len(sourceKeys) == 0 {
		return false, 0, nil
	}
	profileStatus := SemanticModelProfileStatus{
		ModelID:       profile.ModelID(),
		VectorSpaceID: profile.VectorSpaceID(),
		EmbeddingDim:  profile.EmbeddingDim(),
		Role:          semanticModelRoleCandidate,
		ProfileKind:   profile.ProfileKind(),
		InputKind:     profile.InputKind(),
		ModelPack:     profile.ModelPackStatus(),
	}
	needed := false
	workCount := 0
	for _, sourceKey := range sourceKeys {
		status, err := s.SemanticBackfillStatus(ctx, sourceKey, profileStatus)
		if err != nil {
			return needed, workCount, err
		}
		jobCount := status.PendingIndexJobCount + status.FailedIndexJobCount
		if jobCount > 0 && status.EligibleIndexJobCount <= 0 {
			continue
		}
		if jobCount <= 0 && !s.semanticIndexPublishNeeded(ctx, sourceKey, profile, status, allowPartial) {
			continue
		}
		if sourceWork := semanticIndexPublishNeededWorkCount(status); sourceWork > 0 {
			needed = true
			workCount += sourceWork
		}
	}
	return needed, workCount, nil
}

func (s *CatalogStore) semanticIndexPublishNeeded(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile, status SemanticModelBackfillStatus, allowPartial bool) bool {
	if status.AssetGeneration != status.IndexedGeneration {
		if status.CompletedVectorCount == 0 {
			return status.EligibleAssetCount == 0
		}
		return allowPartial || status.CompletedVectorCount >= status.EligibleAssetCount
	}
	if status.IndexedVectorCount >= status.CompletedVectorCount {
		return !s.semanticBinaryIndexMatchesBackfillStatus(ctx, sourceKey, profile, status)
	}
	if allowPartial {
		return true
	}
	return status.CompletedVectorCount >= status.EligibleAssetCount
}

func semanticIndexPublishNeededWorkCount(status SemanticModelBackfillStatus) int {
	if jobCount := status.PendingIndexJobCount + status.FailedIndexJobCount; jobCount > 0 {
		return max(status.EligibleIndexJobCount, 0)
	}
	if queued := status.CompletedVectorCount - status.IndexedVectorCount; queued > 0 {
		return queued
	}
	if status.AssetGeneration != status.IndexedGeneration {
		return max(status.CompletedVectorCount, 1)
	}
	return max(status.CompletedVectorCount, 1)
}

func (s *CatalogStore) enqueueSemanticIndexJob(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile, now time.Time) error {
	if s == nil || s.db == nil {
		return ErrCatalogNotConfigured
	}
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" || profile == nil {
		return ErrCatalogNotConfigured
	}
	nowText := formatCatalogTime(now.UTC())
	_, err := s.db.ExecContext(ctx, `INSERT INTO semantic_index_jobs (
			source_key, model_id, vector_space_id, embedding_dim,
			status, priority, attempts, last_error, scheduled_at,
			started_at, lease_expires_at, completed_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 100, 0, NULL, ?, NULL, NULL, NULL, ?, ?)
		ON CONFLICT(source_key, model_id, vector_space_id) DO UPDATE SET
			embedding_dim = excluded.embedding_dim,
			status = CASE
				WHEN semantic_index_jobs.status IN ('queued', 'running', 'failed') THEN semantic_index_jobs.status
				ELSE excluded.status
			END,
			priority = excluded.priority,
			attempts = CASE
				WHEN semantic_index_jobs.status IN ('queued', 'running', 'failed') THEN semantic_index_jobs.attempts
				ELSE 0
			END,
			last_error = CASE
				WHEN semantic_index_jobs.status IN ('running', 'failed') THEN semantic_index_jobs.last_error
				ELSE NULL
			END,
			scheduled_at = CASE
				WHEN semantic_index_jobs.status IN ('queued', 'running', 'failed') THEN semantic_index_jobs.scheduled_at
				ELSE excluded.scheduled_at
			END,
			started_at = CASE
				WHEN semantic_index_jobs.status = 'running' THEN semantic_index_jobs.started_at
				ELSE NULL
			END,
			lease_expires_at = CASE
				WHEN semantic_index_jobs.status = 'running' THEN semantic_index_jobs.lease_expires_at
				ELSE NULL
			END,
			completed_at = NULL,
			updated_at = excluded.updated_at`,
		sourceKey,
		profile.ModelID(),
		profile.VectorSpaceID(),
		profile.EmbeddingDim(),
		semanticIndexJobStatusQueued,
		nowText,
		nowText,
		nowText,
	)
	if err != nil {
		return fmt.Errorf("enqueue semantic hnsw publish job: %w", err)
	}
	return nil
}

func (s *CatalogStore) ResetRunningSemanticIndexJobs(ctx context.Context, now time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, ErrCatalogNotConfigured
	}
	nowText := formatCatalogTime(now.UTC())
	result, err := s.db.ExecContext(ctx, `UPDATE semantic_index_jobs
		SET status = ?, scheduled_at = ?, started_at = NULL, lease_expires_at = NULL, updated_at = ?
		WHERE status = ?`,
		semanticIndexJobStatusQueued,
		nowText,
		nowText,
		semanticIndexJobStatusRunning,
	)
	if err != nil {
		return 0, fmt.Errorf("reset running semantic hnsw publish jobs: %w", err)
	}
	count, _ := result.RowsAffected()
	return int(count), nil
}

func (s *CatalogStore) semanticIndexJobState(ctx context.Context, sourceKey string, modelID string, vectorSpaceID string, now time.Time) (int, int, int, *time.Time, error) {
	if s == nil || s.db == nil {
		return 0, 0, 0, nil, ErrCatalogNotConfigured
	}
	db := s.queryDB()
	var pending int
	var failed int
	var eligible int
	var nextEligible sql.NullString
	nowText := formatCatalogTime(now.UTC())
	err := db.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(CASE WHEN status IN ('queued', 'running') THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN
				(status IN ('queued', 'failed') AND scheduled_at <= ?)
				OR (status = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?)
			THEN 1 ELSE 0 END), 0),
			MIN(CASE
				WHEN status IN ('queued', 'failed') AND scheduled_at > ? THEN scheduled_at
				WHEN status = 'running' AND lease_expires_at > ? THEN lease_expires_at
			END)
		FROM semantic_index_jobs
		WHERE source_key = ? AND model_id = ? AND vector_space_id = ?`,
		nowText,
		nowText,
		nowText,
		nowText,
		strings.TrimSpace(sourceKey),
		strings.TrimSpace(modelID),
		strings.TrimSpace(vectorSpaceID),
	).Scan(&pending, &failed, &eligible, &nextEligible)
	if err != nil {
		return 0, 0, 0, nil, fmt.Errorf("read semantic hnsw publish job state: %w", err)
	}
	if !nextEligible.Valid || strings.TrimSpace(nextEligible.String) == "" {
		return pending, failed, eligible, nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, nextEligible.String)
	if err != nil {
		return 0, 0, 0, nil, fmt.Errorf("parse semantic hnsw publish eligibility: %w", err)
	}
	parsed = parsed.UTC()
	return pending, failed, eligible, &parsed, nil
}

func (s *CatalogStore) PublishNextSemanticIndexJob(ctx context.Context, sourceKeys []string, profile semanticEmbeddingProfile, startedAt time.Time) (SemanticIndexPublishResult, error) {
	if s == nil || s.db == nil {
		return SemanticIndexPublishResult{}, ErrCatalogNotConfigured
	}
	if profile == nil {
		return SemanticIndexPublishResult{}, ErrCatalogNotConfigured
	}
	sourceKeys = normalizeSemanticIndexJobSourceKeys(sourceKeys)
	if len(sourceKeys) == 0 {
		return SemanticIndexPublishResult{}, nil
	}
	now := time.Now().UTC()
	job, err := s.claimSemanticIndexJob(ctx, sourceKeys, profile, now)
	if err != nil || job == nil {
		return SemanticIndexPublishResult{}, err
	}
	profileStatus := SemanticModelProfileStatus{
		ModelID:       profile.ModelID(),
		VectorSpaceID: profile.VectorSpaceID(),
		EmbeddingDim:  profile.EmbeddingDim(),
		Role:          semanticModelRoleCandidate,
		ProfileKind:   profile.ProfileKind(),
		InputKind:     profile.InputKind(),
		ModelPack:     profile.ModelPackStatus(),
	}
	failJob := func(cause error) (SemanticIndexPublishResult, error) {
		transitionCtx, cancel := context.WithTimeout(context.Background(), semanticIndexJobTransitionTimeout)
		defer cancel()
		if markErr := s.failSemanticIndexJob(transitionCtx, job.ID, job.Attempts, cause, time.Now().UTC()); markErr != nil {
			return SemanticIndexPublishResult{}, errors.Join(cause, markErr)
		}
		return SemanticIndexPublishResult{}, cause
	}
	job.AssetGeneration, _, err = s.semanticIndexGenerations(ctx, job.SourceKey, profile.ModelID())
	if err != nil {
		return failJob(err)
	}

	builder, err := s.prepareSemanticIndexBuilder(ctx, job, profile)
	if err != nil {
		return failJob(err)
	}
	defer builder.Close()
	indexedVectorCount, err := builder.metaInt(ctx, builder.db, "node_count")
	if err != nil {
		return failJob(err)
	}
	completedAt, err := s.publishSemanticBackfillIndex(ctx, job, profile, builder)
	if err != nil {
		return failJob(err)
	}
	status, err := s.SemanticBackfillStatus(ctx, job.SourceKey, profileStatus)
	if err != nil {
		return failJob(err)
	}
	status.IndexedVectorCount = indexedVectorCount
	status.IndexedGeneration = job.AssetGeneration
	status = semanticBackfillStatusAfterCompletedIndexJob(status, completedAt)
	transitionCtx, cancelTransition := context.WithTimeout(context.Background(), semanticIndexJobTransitionTimeout)
	defer cancelTransition()
	if err := s.finalizeSemanticIndexJob(transitionCtx, job, profile, status, completedAt); err != nil {
		return failJob(err)
	}
	if err := s.activateSemanticBinaryIndex(ctx, job.SourceKey, profile, job.AssetGeneration); err != nil {
		return failJob(err)
	}
	if cleanupErr := s.cleanupSemanticBinaryIndexGenerations(context.Background(), job.SourceKey, profile, &job.AssetGeneration, true); cleanupErr != nil {
		log.Printf("timich-agent semantic binary index generation cleanup failed source_key=%s model=%s generation=%d error=%v", job.SourceKey, profile.ModelID(), job.AssetGeneration, cleanupErr)
	}
	builder.Remove()
	return SemanticIndexPublishResult{
		Published:          true,
		SourceKey:          job.SourceKey,
		ModelID:            profile.ModelID(),
		VectorSpaceID:      profile.VectorSpaceID(),
		IndexedVectorCount: status.IndexedVectorCount,
		StartedAt:          startedAt.UTC(),
		CompletedAt:        completedAt,
		Status:             status,
	}, nil
}

func normalizeSemanticIndexJobSourceKeys(sourceKeys []string) []string {
	normalized := make([]string, 0, len(sourceKeys))
	seen := map[string]struct{}{}
	for _, sourceKey := range sourceKeys {
		sourceKey = strings.TrimSpace(sourceKey)
		if sourceKey == "" {
			continue
		}
		if _, ok := seen[sourceKey]; ok {
			continue
		}
		seen[sourceKey] = struct{}{}
		normalized = append(normalized, sourceKey)
	}
	sort.Strings(normalized)
	return normalized
}

func (s *CatalogStore) claimSemanticIndexJob(ctx context.Context, sourceKeys []string, profile semanticEmbeddingProfile, now time.Time) (*semanticIndexJob, error) {
	nowText := formatCatalogTime(now.UTC())
	if _, err := s.recoverExpiredSemanticIndexJobs(ctx, now); err != nil {
		return nil, err
	}
	leaseExpiresText := formatCatalogTime(now.UTC().Add(semanticIndexJobLeaseDuration))
	for _, sourceKey := range sourceKeys {
		var job semanticIndexJob
		err := s.db.QueryRowContext(ctx, `SELECT id, source_key, model_id, vector_space_id, embedding_dim, attempts
			FROM semantic_index_jobs
			WHERE source_key = ?
				AND model_id = ?
				AND vector_space_id = ?
				AND status IN ('queued', 'failed')
				AND scheduled_at <= ?
			ORDER BY priority ASC, scheduled_at ASC, id ASC
			LIMIT 1`,
			sourceKey,
			profile.ModelID(),
			profile.VectorSpaceID(),
			nowText,
		).Scan(&job.ID, &job.SourceKey, &job.ModelID, &job.VectorSpaceID, &job.EmbeddingDim, &job.Attempts)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("claim semantic hnsw publish job: %w", err)
		}
		result, err := s.db.ExecContext(ctx, `UPDATE semantic_index_jobs
			SET status = ?, attempts = attempts + 1, last_error = NULL, started_at = ?, lease_expires_at = ?, completed_at = NULL, updated_at = ?
			WHERE id = ? AND status IN ('queued', 'failed')`,
			semanticIndexJobStatusRunning,
			nowText,
			leaseExpiresText,
			nowText,
			job.ID,
		)
		if err != nil {
			return nil, fmt.Errorf("mark semantic hnsw publish job running: %w", err)
		}
		if count, _ := result.RowsAffected(); count == 0 {
			continue
		}
		job.Attempts++
		return &job, nil
	}
	return nil, nil
}

func (s *CatalogStore) recoverExpiredSemanticIndexJobs(ctx context.Context, now time.Time) (int, error) {
	nowText := formatCatalogTime(now.UTC())
	result, err := s.db.ExecContext(ctx, `UPDATE semantic_index_jobs
		SET status = ?, last_error = ?, scheduled_at = ?, started_at = NULL,
			lease_expires_at = NULL, completed_at = NULL, updated_at = ?
		WHERE status = ? AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?`,
		semanticIndexJobStatusFailed,
		"semantic hnsw publish lease expired",
		nowText,
		nowText,
		semanticIndexJobStatusRunning,
		nowText,
	)
	if err != nil {
		return 0, fmt.Errorf("recover expired semantic hnsw publish jobs: %w", err)
	}
	count, _ := result.RowsAffected()
	return int(count), nil
}

func semanticBackfillStatusAfterCompletedIndexJob(status SemanticModelBackfillStatus, completedAt time.Time) SemanticModelBackfillStatus {
	status.PendingIndexJobCount = max(0, status.PendingIndexJobCount-1)
	status.EligibleIndexJobCount = 0
	completedAt = completedAt.UTC()
	status.LastPublishedAt = &completedAt
	status.Status = semanticBackfillStatusReady
	status.MessageCode = semanticBackfillMessageReady
	switch {
	case status.CompletedVectorCount == 0 && status.EligibleAssetCount > 0:
		status.Status = semanticBackfillStatusPending
		status.MessageCode = semanticBackfillMessagePending
	case status.CompletedVectorCount < status.EligibleAssetCount:
		status.Status = semanticBackfillStatusBackfilling
		status.MessageCode = semanticBackfillMessageIncomplete
	case status.AssetGeneration != status.IndexedGeneration ||
		status.IndexedVectorCount < status.CompletedVectorCount ||
		status.PendingIndexJobCount > 0 || status.FailedIndexJobCount > 0:
		status.Status = semanticBackfillStatusIndexing
		status.MessageCode = semanticBackfillMessageIndexing
	}
	if status.Status == semanticBackfillStatusReady {
		status.NextEligibleAt = nil
	}
	return status
}

func (s *CatalogStore) finalizeSemanticIndexJob(ctx context.Context, job *semanticIndexJob, profile semanticEmbeddingProfile, status SemanticModelBackfillStatus, now time.Time) error {
	if job == nil {
		return ErrCatalogNotConfigured
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin semantic hnsw publish finalization: %w", err)
	}
	semantic := semanticStatusFromBackfillStatus(status, profile)
	if err := s.publishSemanticStateInTx(ctx, tx, job.SourceKey, semantic, job.AssetGeneration, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	nowText := formatCatalogTime(now.UTC())
	result, err := tx.ExecContext(ctx, `UPDATE semantic_index_jobs
		SET status = ?, last_error = NULL, lease_expires_at = NULL, completed_at = ?, updated_at = ?
		WHERE id = ? AND status = ? AND attempts = ?`,
		semanticIndexJobStatusCompleted,
		nowText,
		nowText,
		job.ID,
		semanticIndexJobStatusRunning,
		job.Attempts,
	)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("complete semantic hnsw publish job: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		_ = tx.Rollback()
		return fmt.Errorf("semantic hnsw publish job ownership was lost before finalization")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit semantic hnsw publish finalization: %w", err)
	}
	return nil
}

func (s *CatalogStore) failSemanticIndexJob(ctx context.Context, jobID int64, attempts int, cause error, now time.Time) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	nowText := formatCatalogTime(now.UTC())
	retryAtText := formatCatalogTime(now.UTC().Add(semanticRetryDelay(attempts)))
	result, err := s.db.ExecContext(ctx, `UPDATE semantic_index_jobs
		SET status = ?, last_error = ?, scheduled_at = ?, lease_expires_at = NULL, completed_at = NULL, updated_at = ?
		WHERE id = ? AND status IN (?, ?) AND attempts = ?`,
		semanticIndexJobStatusFailed,
		message,
		retryAtText,
		nowText,
		jobID,
		semanticIndexJobStatusRunning,
		semanticIndexJobStatusCompleted,
		attempts,
	)
	if err != nil {
		return fmt.Errorf("fail semantic hnsw publish job: %w", err)
	}
	if count, _ := result.RowsAffected(); count > 1 {
		return fmt.Errorf("fail semantic hnsw publish job: unexpected update count %d", count)
	}
	return nil
}

func (s *CatalogStore) renewSemanticIndexJobLease(ctx context.Context, job *semanticIndexJob, now time.Time) error {
	if job == nil {
		return ErrCatalogNotConfigured
	}
	now = now.UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE semantic_index_jobs
		SET lease_expires_at = ?, updated_at = ?
		WHERE id = ? AND status = ? AND attempts = ?`,
		formatCatalogTime(now.Add(semanticIndexJobLeaseDuration)),
		formatCatalogTime(now),
		job.ID,
		semanticIndexJobStatusRunning,
		job.Attempts,
	)
	if err != nil {
		return fmt.Errorf("renew semantic hnsw publish job lease: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("semantic hnsw publish job ownership was lost during build")
	}
	return nil
}

func (s *CatalogStore) semanticIndexLeaseCheckpoint(job *semanticIndexJob) func(context.Context) error {
	nextRenewal := time.Now().UTC().Add(semanticIndexJobLeaseRenewInterval)
	return func(ctx context.Context) error {
		now := time.Now().UTC()
		if now.Before(nextRenewal) {
			return nil
		}
		if err := s.renewSemanticIndexJobLease(ctx, job, now); err != nil {
			return err
		}
		nextRenewal = now.Add(semanticIndexJobLeaseRenewInterval)
		return nil
	}
}

func (s *CatalogStore) publishSemanticBackfillIndex(ctx context.Context, job *semanticIndexJob, profile semanticEmbeddingProfile, builder *semanticIndexBuilder) (time.Time, error) {
	if job == nil {
		return time.Time{}, ErrCatalogNotConfigured
	}
	if builder == nil {
		return time.Time{}, ErrCatalogNotConfigured
	}
	if err := builder.Build(ctx); err != nil {
		return time.Time{}, err
	}
	var currentGeneration int64
	if err := s.db.QueryRowContext(ctx, `SELECT asset_generation
		FROM semantic_state
		WHERE source_key = ? AND model_id = ?`, job.SourceKey, profile.ModelID()).Scan(&currentGeneration); err != nil {
		return time.Time{}, fmt.Errorf("read semantic generation before publish: %w", err)
	}
	if currentGeneration != job.AssetGeneration {
		return time.Time{}, fmt.Errorf("semantic asset generation changed during publish: got %d want %d", currentGeneration, job.AssetGeneration)
	}
	publishedAt := time.Now().UTC()
	if err := builder.WriteBinary(ctx, publishedAt); err != nil {
		return time.Time{}, err
	}
	return publishedAt, nil
}

func (s *CatalogStore) upsertSemanticBackfillState(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile, status SemanticModelBackfillStatus, now time.Time) error {
	return s.upsertSemanticState(ctx, sourceKey, semanticStatusFromBackfillStatus(status, profile), now)
}

func semanticStatusFromBackfillStatus(status SemanticModelBackfillStatus, profile semanticEmbeddingProfile) CatalogSemanticStatus {
	return normalizeCatalogSemanticStatus(CatalogSemanticStatus{
		Status:               status.Status,
		ModelID:              status.ModelID,
		VectorSpaceID:        status.VectorSpaceID,
		EmbeddingDim:         status.EmbeddingDim,
		ProfileKind:          profile.ProfileKind(),
		InputKind:            profile.InputKind(),
		CompletedVectorCount: status.CompletedVectorCount,
		IndexedVectorCount:   status.IndexedVectorCount,
		AssetGeneration:      status.AssetGeneration,
		IndexedGeneration:    status.IndexedGeneration,
		BuiltAt:              status.LastPublishedAt,
		ModelPack:            profile.ModelPackStatus(),
	}, profile)
}

func (s *CatalogStore) upsertSemanticState(ctx context.Context, sourceKey string, semantic CatalogSemanticStatus, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin catalog semantic state upsert: %w", err)
	}
	if err := s.upsertSemanticStateInTx(ctx, tx, sourceKey, semantic, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit catalog semantic state upsert: %w", err)
	}
	return nil
}

func (s *CatalogStore) loadSemanticBackfillAssets(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile, limit int) ([]semanticAsset, error) {
	where, args := semanticCatalogEligibilityWhere(sourceKey, profile.InputKind(), "a")
	queryArgs := append([]any{profile.ModelID()}, args...)
	retryBefore := formatCatalogTime(time.Now().UTC().Add(-semanticBackfillFailureRetryInterval))
	queryArgs = append(queryArgs, profile.VectorSpaceID(), profile.EmbeddingDim(), retryBefore, limit)
	db := s.queryDB()
	rows, err := db.QueryContext(ctx, `SELECT a.source_key, a.upstream_asset_id, a.media_type, a.filename, a.captured_at, a.duration
		FROM catalog_assets a
		LEFT JOIN semantic_vectors v
			ON v.source_key = a.source_key
			AND v.upstream_asset_id = a.upstream_asset_id
			AND v.model_id = ?
		`+where+`
			AND (
				v.upstream_asset_id IS NULL
				OR v.vector_space_id != ?
				OR v.embedding_dim != ?
				OR v.status NOT IN ('ready', 'failed')
				OR (v.status = 'failed' AND (v.generated_at IS NULL OR v.generated_at <= ?))
			)
		ORDER BY CASE WHEN v.status = 'failed' THEN 1 ELSE 0 END ASC,
			a.captured_at DESC, a.source_key ASC, a.upstream_asset_id ASC
		LIMIT ?`, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("query semantic indexing assets: %w", err)
	}
	defer rows.Close()
	return scanSemanticAssets(rows, "semantic indexing")
}

func (s *CatalogStore) upsertSemanticVectors(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile, assets []semanticAsset, now time.Time) error {
	s.semanticVectorPayloadMu.Lock()
	defer s.semanticVectorPayloadMu.Unlock()
	batch, err := s.writeSemanticVectorPayloadBatch(ctx, sourceKey, profile, assets)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin catalog semantic vector upsert: %w", err)
	}
	if err := s.registerSemanticVectorPayloadBatch(ctx, tx, batch); err != nil {
		_ = tx.Rollback()
		return err
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO semantic_vectors (
			source_key, upstream_asset_id, model_id, vector_space_id, embedding_dim,
			payload_batch_id, vector_offset, vector_length, embedding_input, status, last_error, generated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready', NULL, ?)
		ON CONFLICT(source_key, upstream_asset_id, model_id) DO UPDATE SET
			vector_space_id = excluded.vector_space_id,
			embedding_dim = excluded.embedding_dim,
			payload_batch_id = excluded.payload_batch_id,
			vector_offset = excluded.vector_offset,
			vector_length = excluded.vector_length,
			embedding_input = excluded.embedding_input,
			status = 'ready',
			last_error = NULL,
			generated_at = excluded.generated_at`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare catalog semantic vector upsert: %w", err)
	}
	nowText := formatCatalogTime(now)
	for _, asset := range assets {
		ref := asset.VectorPayload
		if ref.Length <= 0 {
			_ = statement.Close()
			_ = tx.Rollback()
			return fmt.Errorf("catalog semantic vector payload ref missing for %q", asset.ID)
		}
		if err := queueSemanticVectorPayloadGCInTx(ctx, tx, sourceKey, asset.ID, profile.ModelID(), ref.BatchID); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			return err
		}
		if _, err = statement.ExecContext(ctx, sourceKey, asset.ID, profile.ModelID(), profile.VectorSpaceID(), profile.EmbeddingDim(), ref.BatchID, ref.Offset, ref.Length, asset.VectorInput, nowText); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			return fmt.Errorf("upsert catalog semantic vector %q: %w", asset.ID, err)
		}
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("close catalog semantic vector upsert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit catalog semantic vector upsert: %w", err)
	}
	if cleanupErr := s.cleanupSemanticVectorPayloadCandidatesLocked(context.Background()); cleanupErr != nil {
		log.Printf("timich-agent semantic vector payload cleanup failed error=%v", cleanupErr)
	}
	return nil
}

func (s *CatalogStore) upsertSemanticVectorFailures(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile, failures []semanticBackfillAssetFailure, now time.Time) error {
	if len(failures) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin catalog semantic vector failure upsert: %w", err)
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO semantic_vectors (
			source_key, upstream_asset_id, model_id, vector_space_id, embedding_dim,
			payload_batch_id, vector_offset, vector_length, embedding_input, status, last_error, generated_at
		) VALUES (?, ?, ?, ?, ?, NULL, 0, 0, ?, 'failed', ?, ?)
		ON CONFLICT(source_key, upstream_asset_id, model_id) DO UPDATE SET
			vector_space_id = excluded.vector_space_id,
			embedding_dim = excluded.embedding_dim,
			payload_batch_id = NULL,
			vector_offset = 0,
			vector_length = 0,
			embedding_input = excluded.embedding_input,
			status = 'failed',
			last_error = excluded.last_error,
			generated_at = excluded.generated_at`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("prepare catalog semantic vector failure upsert: %w", err)
	}
	nowText := formatCatalogTime(now)
	for _, failure := range failures {
		message := strings.TrimSpace(failure.Err.Error())
		if len(message) > 2048 {
			message = message[:2048]
		}
		if err := queueSemanticVectorPayloadGCInTx(ctx, tx, sourceKey, failure.Asset.ID, profile.ModelID(), ""); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			return err
		}
		if _, err = statement.ExecContext(ctx, sourceKey, failure.Asset.ID, profile.ModelID(), profile.VectorSpaceID(), profile.EmbeddingDim(), profile.InputKind(), message, nowText); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			return fmt.Errorf("upsert catalog semantic vector failure %q: %w", failure.Asset.ID, err)
		}
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("close catalog semantic vector failure upsert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit catalog semantic vector failure upsert: %w", err)
	}
	if cleanupErr := s.cleanupSemanticVectorPayloadCandidates(context.Background()); cleanupErr != nil {
		log.Printf("timich-agent semantic vector payload cleanup failed error=%v", cleanupErr)
	}
	return nil
}

func (s *CatalogStore) openSemanticIndexTraversal(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile, queryVector []float32) (*semanticIndexTraversalSession, bool, error) {
	started := time.Now()
	reader, semantic, err := s.openSemanticBinaryIndexFile(ctx, sourceKey, profile)
	if err != nil {
		log.Printf(
			"timich-agent semantic search binary direct skipped source_key=%s elapsed=%s error=%v",
			sourceKey,
			time.Since(started).Round(time.Millisecond),
			err,
		)
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		stateSemantic := semantic
		if strings.TrimSpace(stateSemantic.ModelID) == "" {
			if loaded, statusErr := s.semanticStatusForBinarySearch(ctx, sourceKey, profile); statusErr == nil {
				stateSemantic = loaded
			}
		}
		if stateSemantic.IndexedVectorCount == 0 &&
			stateSemantic.AssetGeneration == stateSemantic.IndexedGeneration &&
			strings.TrimSpace(stateSemantic.Status) != "missing" {
			return &semanticIndexTraversalSession{Semantic: stateSemantic, Exhausted: true}, true, nil
		}
		if stateSemantic.CompletedVectorCount > 0 || stateSemantic.IndexedVectorCount > 0 || strings.TrimSpace(stateSemantic.Status) != "missing" {
			return &semanticIndexTraversalSession{Semantic: semanticBinaryUnavailableSearchStatus(stateSemantic, profile), Exhausted: true}, true, nil
		}
		return nil, false, nil
	}
	indexedCount := len(reader.nodes)
	if strings.TrimSpace(semantic.Status) == "missing" {
		_ = reader.Close()
		return &semanticIndexTraversalSession{Semantic: semanticBinaryUnavailableSearchStatus(semantic, profile), IndexedCount: indexedCount, Exhausted: true}, true, nil
	}
	if !semanticSearchUsable(semantic, indexedCount) {
		_ = reader.Close()
		return &semanticIndexTraversalSession{Semantic: semantic, IndexedCount: indexedCount, Exhausted: true}, true, nil
	}
	traversal, err := reader.newSearchSession(ctx, queryVector)
	if err != nil {
		_ = reader.Close()
		return nil, false, err
	}
	return &semanticIndexTraversalSession{
		reader:       reader,
		traversal:    traversal,
		Semantic:     semantic,
		IndexedCount: indexedCount,
		Exhausted:    indexedCount == 0,
	}, true, nil
}

func (s *CatalogStore) semanticStatusForBinarySearch(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile) (CatalogSemanticStatus, error) {
	if err := ctx.Err(); err != nil {
		return CatalogSemanticStatus{}, err
	}
	statusCtx, cancel := context.WithTimeout(ctx, semanticBinaryStatusOverlayTimeout)
	defer cancel()
	return s.semanticStatus(statusCtx, sourceKey, profile)
}

func semanticSearchUsable(status CatalogSemanticStatus, assetCount int) bool {
	return assetCount > 0 && status.IndexedVectorCount > 0 && strings.TrimSpace(status.Status) != "missing"
}

func semanticBinaryUnavailableSearchStatus(status CatalogSemanticStatus, profile semanticEmbeddingProfile) CatalogSemanticStatus {
	status = normalizeCatalogSemanticStatus(status, profile)
	if status.Status == semanticBackfillStatusReady {
		status.Status = semanticBackfillStatusIndexing
		if strings.TrimSpace(status.LastError) == "" {
			status.LastError = "semantic binary index is missing or stale; waiting for publish"
		}
	}
	return status
}

func semanticCapabilities(status CatalogSemanticStatus, profile semanticEmbeddingProfile) *AssetSearchSemanticCapabilities {
	status = normalizeCatalogSemanticStatus(status, profile)
	return &AssetSearchSemanticCapabilities{
		Status:               status.Status,
		ModelID:              status.ModelID,
		VectorSpaceID:        status.VectorSpaceID,
		EmbeddingDim:         status.EmbeddingDim,
		ProfileKind:          status.ProfileKind,
		InputKind:            status.InputKind,
		CompletedVectorCount: status.CompletedVectorCount,
		IndexedVectorCount:   status.IndexedVectorCount,
		MessageCode:          semanticMessageCode(status, profile),
		ModelPack:            status.ModelPack,
	}
}

func semanticResolution(status CatalogSemanticStatus, eligible bool, profile semanticEmbeddingProfile) *AssetSearchSemanticResolution {
	status = normalizeCatalogSemanticStatus(status, profile)
	return &AssetSearchSemanticResolution{
		Status:               status.Status,
		Eligible:             eligible,
		ModelID:              status.ModelID,
		VectorSpaceID:        status.VectorSpaceID,
		EmbeddingDim:         status.EmbeddingDim,
		ProfileKind:          status.ProfileKind,
		InputKind:            status.InputKind,
		CompletedVectorCount: status.CompletedVectorCount,
		IndexedVectorCount:   status.IndexedVectorCount,
		MessageCode:          semanticMessageCode(status, profile),
		ModelPack:            status.ModelPack,
	}
}

func semanticFilenameFallback(normalized normalizedAssetSearch, status CatalogSemanticStatus, profile semanticEmbeddingProfile) (normalizedAssetSearch, error) {
	fallback := normalized
	fallback.Resolved.QueryMode = QueryModeFilename
	fallback.Resolved.Sort = AssetSearchSort{Field: SortFieldCapturedAt, Direction: SortDirectionDesc}
	fallback.Resolved.TimelineLike = true
	fallback.Resolved.Semantic = semanticResolution(status, false, profile)
	fallback.Resolved.Semantic.FallbackQueryMode = QueryModeFilename
	fallback.Resolved.Semantic.MessageCode = semanticFallbackMessageCode(status, profile)
	if fallback.Request.Collection.Query != nil {
		fallback.Request.Collection.Query.Mode = QueryModeFilename
	}
	fallback.Request.Collection.Sort = &fallback.Resolved.Sort
	keyResolved := fallback.Resolved
	keyResolved.Semantic = nil
	key, err := collectionKey(fallback.Request, keyResolved)
	if err != nil {
		return normalizedAssetSearch{}, err
	}
	fallback.CollectionKey = key
	return fallback, nil
}

func semanticAutoRequested(normalized normalizedAssetSearch) bool {
	return normalized.Request.Collection.Query != nil && normalized.Request.Collection.Query.Mode == QueryModeAuto
}

func semanticFavoriteQuery(query string) bool {
	switch strings.ToLower(strings.TrimSpace(query)) {
	case "favorite", "favorites", "favourite", "favourites", "starred", "お気に入り":
		return true
	default:
		return false
	}
}

func normalizeCatalogSemanticStatus(status CatalogSemanticStatus, profile semanticEmbeddingProfile) CatalogSemanticStatus {
	if strings.TrimSpace(status.Status) == "" {
		status.Status = "missing"
	}
	if profile == nil {
		return status
	}
	if strings.TrimSpace(status.ModelID) == "" {
		status.ModelID = profile.ModelID()
	}
	if strings.TrimSpace(status.VectorSpaceID) == "" {
		status.VectorSpaceID = profile.VectorSpaceID()
	}
	if status.EmbeddingDim == 0 {
		status.EmbeddingDim = profile.EmbeddingDim()
	}
	if strings.TrimSpace(status.ProfileKind) == "" {
		status.ProfileKind = profile.ProfileKind()
	}
	if strings.TrimSpace(status.InputKind) == "" {
		status.InputKind = profile.InputKind()
	}
	if status.ModelPack == nil {
		status.ModelPack = profile.ModelPackStatus()
	}
	return status
}

func semanticMessageCode(status CatalogSemanticStatus, profile semanticEmbeddingProfile) string {
	status = normalizeCatalogSemanticStatus(status, profile)
	if status.Status != "ready" {
		if status.Status == semanticBackfillStatusBackfilling || status.Status == semanticBackfillStatusIndexing {
			return semanticMessageIndexBackfilling
		}
		if status.Status == "missing" {
			return semanticMessageIndexMissing
		}
		if status.IndexedVectorCount > 0 {
			return semanticMessageIndexBackfilling
		}
		return semanticMessageIndexUnavailable
	}
	return ""
}

func semanticFallbackMessageCode(status CatalogSemanticStatus, profile semanticEmbeddingProfile) string {
	status = normalizeCatalogSemanticStatus(status, profile)
	if status.Status == "missing" {
		return semanticMessageIndexMissingFallback
	}
	return semanticMessageIndexUnavailableFallback
}

func diversifySemanticCandidateSnapshot(scored []semanticScoredAsset) []semanticScoredAsset {
	return diversifySemanticCandidates(scored, semanticDiversityMinCandidates)
}

func diversifySemanticCandidates(scored []semanticScoredAsset, candidateCount int) []semanticScoredAsset {
	if len(scored) < 2 || candidateCount <= 0 {
		return scored
	}
	if candidateCount > len(scored) {
		candidateCount = len(scored)
	}
	diverse := make([]semanticScoredAsset, 0, candidateCount)
	delayed := make([]semanticScoredAsset, 0)
	for _, candidate := range scored[:candidateCount] {
		if semanticNearDuplicate(candidate, diverse) {
			delayed = append(delayed, candidate)
			continue
		}
		diverse = append(diverse, candidate)
	}
	if len(delayed) == 0 {
		return scored
	}
	result := make([]semanticScoredAsset, 0, len(scored))
	result = append(result, diverse...)
	result = append(result, delayed...)
	result = append(result, scored[candidateCount:]...)
	return result
}

func semanticNearDuplicate(candidate semanticScoredAsset, selected []semanticScoredAsset) bool {
	for _, item := range selected {
		if semanticDot(candidate.Asset.Vector, item.Asset.Vector) >= semanticNearDuplicateSimilarity {
			return true
		}
	}
	return false
}

type semanticMaxHeap []semanticScoredAsset

func (h semanticMaxHeap) Len() int { return len(h) }

func (h semanticMaxHeap) Less(i, j int) bool {
	if h[i].Similarity == h[j].Similarity {
		return h[i].Asset.ID < h[j].Asset.ID
	}
	return h[i].Similarity > h[j].Similarity
}

func (h semanticMaxHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *semanticMaxHeap) Push(value any) {
	*h = append(*h, value.(semanticScoredAsset))
}

func (h *semanticMaxHeap) Pop() any {
	old := *h
	item := old[len(old)-1]
	*h = old[:len(old)-1]
	return item
}

func semanticDot(left []float32, right []float32) float32 {
	limit := min(len(left), len(right))
	var sum float32
	for index := 0; index < limit; index++ {
		sum += left[index] * right[index]
	}
	return sum
}

func encodeSemanticVector(vector []float32) []byte {
	var buffer bytes.Buffer
	for _, value := range vector {
		_ = binary.Write(&buffer, binary.LittleEndian, value)
	}
	return buffer.Bytes()
}

func decodeSemanticVector(raw []byte, dim int) ([]float32, error) {
	if len(raw) != dim*4 {
		return nil, fmt.Errorf("decode semantic vector: got %d bytes, want %d", len(raw), dim*4)
	}
	vector := make([]float32, dim)
	for index := range vector {
		vector[index] = math.Float32frombits(binary.LittleEndian.Uint32(raw[index*4 : index*4+4]))
	}
	return vector, nil
}

func semanticHNSWLevel(assetID string) int {
	sum := sha256.Sum256([]byte(assetID))
	level := 0
	for index := 0; index < len(sum) && level < semanticHNSWMaxLevel; index++ {
		if sum[index]&0x03 != 0 {
			break
		}
		level++
	}
	return level
}
