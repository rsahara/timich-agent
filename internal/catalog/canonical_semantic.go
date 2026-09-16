package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// canonicalSemanticCorpusSourceKey is an internal namespace for the one
// semantic corpus shared by all catalog datasources. It is deliberately not a
// configured datasource key: vector/index storage keeps its existing durable
// shape while HNSW node IDs are canonical asset IDs.
const canonicalSemanticCorpusSourceKey = "__timich_canonical__"

const canonicalSemanticPreprocessingVersion = "image-rendition-v1"

type canonicalSemanticInputCandidate struct {
	Asset semanticAsset
}

// canonicalSemanticInputsTemplate resolves one preferred embedding input for every
// canonical asset. Display identity remains the catalog primary/Gallery
// identity. An active Local copy waits for its own rendition instead of
// embedding an Immich duplicate that would be replaced after Local ingestion.
// Existing vectors remain available to the published index while waiting.
const canonicalSemanticInputsTemplate = `WITH canonical_inputs AS (
	SELECT c.canonical_asset_id, c.media_type, c.filename, c.captured_at, c.duration,
		COALESCE(c.content_sha1_hex, '') AS content_sha1_hex, COALESCE(c.content_size_bytes, 0) AS content_size_bytes,
		COALESCE(primary_source.source_key, '') AS primary_source_key, COALESCE(primary_source.upstream_asset_id, '') AS primary_upstream_asset_id,
		COALESCE(primary_source.datasource_kind, '') AS datasource_kind,
		COALESCE((
			SELECT rendition.kind
			FROM local_renditions rendition
			JOIN local_assets local_asset
				ON local_asset.source_key = rendition.source_key
				AND local_asset.asset_id = rendition.asset_id
			WHERE rendition.source_key = primary_source.source_key
				AND rendition.asset_id = primary_source.upstream_asset_id
				AND rendition.kind IN ('detail_preview', 'preview')
				AND rendition.status = 'ready'
				AND rendition.relative_path IS NOT NULL
				AND trim(rendition.relative_path) <> ''
				AND rendition.source_sha1_hex = local_asset.sha1_hex
				AND local_asset.visibility_status = 'active'
			ORDER BY CASE rendition.kind WHEN 'detail_preview' THEN 0 ELSE 1 END
			LIMIT 1
		), '') AS local_rendition_kind,
		COALESCE((
			SELECT rendition.content_sha256
			FROM local_renditions rendition
			JOIN local_assets local_asset
				ON local_asset.source_key = rendition.source_key
				AND local_asset.asset_id = rendition.asset_id
			WHERE rendition.source_key = primary_source.source_key
				AND rendition.asset_id = primary_source.upstream_asset_id
				AND rendition.kind IN ('detail_preview', 'preview')
				AND rendition.status = 'ready'
				AND rendition.relative_path IS NOT NULL
				AND trim(rendition.relative_path) <> ''
				AND rendition.source_sha1_hex = local_asset.sha1_hex
				AND local_asset.visibility_status = 'active'
			ORDER BY CASE rendition.kind WHEN 'detail_preview' THEN 0 ELSE 1 END
			LIMIT 1
		), '') AS local_rendition_sha256
	FROM catalog_canonical_assets c
	LEFT JOIN catalog_assets primary_source
		ON %s
	WHERE c.visibility_status = 'active'
), resolved_inputs AS (
	SELECT *,
		CASE
			WHEN datasource_kind = 'local_filesystem' AND local_rendition_kind <> '' THEN primary_source_key
			WHEN datasource_kind = 'local_filesystem' THEN ''
			ELSE primary_source_key
		END AS embedding_source_key,
		CASE
			WHEN datasource_kind = 'local_filesystem' AND local_rendition_kind <> '' THEN primary_upstream_asset_id
			WHEN datasource_kind = 'local_filesystem' THEN ''
			ELSE primary_upstream_asset_id
		END AS embedding_upstream_asset_id,
		CASE
			WHEN datasource_kind = 'local_filesystem' AND local_rendition_kind <> '' THEN 'local_' || local_rendition_kind
			WHEN datasource_kind = 'immich' THEN 'immich_preview'
			ELSE ''
		END AS embedding_input
	FROM canonical_inputs
)`

// Resolve inputs only from configured sources. A removed catalog primary may
// still have an active duplicate on a configured source; prefer that copy.
func canonicalSemanticInputs(sourceKeys []string) (string, []any) {
	keys := canonicalSemanticSourceKeys(sourceKeys)
	join := `primary_source.source_key = c.primary_source_key
		AND primary_source.upstream_asset_id = c.primary_upstream_asset_id`
	var args []any
	if len(keys) > 0 {
		join = `primary_source.rowid = (
			SELECT source.rowid FROM catalog_assets source
			WHERE source.canonical_asset_id = c.canonical_asset_id
				AND source.visibility_status = 'active'
				AND source.source_key IN (` + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + `)
			ORDER BY CASE source.datasource_kind WHEN 'local_filesystem' THEN 0 WHEN 'immich' THEN 1 ELSE 2 END,
				source.captured_at DESC, source.source_key, source.upstream_asset_id
			LIMIT 1
		)`
		for _, key := range keys {
			args = append(args, key)
		}
	}
	return fmt.Sprintf(canonicalSemanticInputsTemplate, join), args
}

func canonicalSemanticInputFingerprint(candidate semanticAsset, input string, contentSHA1 string, contentSize int64) string {
	return strings.Join([]string{
		canonicalSemanticPreprocessingVersion,
		strings.TrimSpace(input),
		strings.TrimSpace(candidate.EmbeddingSourceKey),
		strings.TrimSpace(candidate.EmbeddingUpstreamAssetID),
		strings.ToLower(strings.TrimSpace(contentSHA1)),
		fmt.Sprintf("%d", contentSize),
		strings.ToLower(strings.TrimSpace(candidate.RenditionSHA256)),
	}, "\x00")
}

func canonicalSemanticSourceKeys(sourceKeys []string) []string {
	keys := make([]string, 0, len(sourceKeys))
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
		keys = append(keys, sourceKey)
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	return keys
}

func canonicalSemanticScopeClause(sourceKeys []string) (string, []any) {
	keys := canonicalSemanticSourceKeys(sourceKeys)
	if len(keys) == 0 {
		return "", nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys))
	for _, sourceKey := range keys {
		args = append(args, sourceKey)
	}
	return ` AND EXISTS (
		SELECT 1 FROM catalog_assets scoped_source
		WHERE scoped_source.canonical_asset_id = input.canonical_asset_id
			AND scoped_source.visibility_status = 'active'
			AND scoped_source.source_key IN (` + placeholders + `)
	)`, args
}

func (s *CatalogStore) loadCanonicalSemanticBackfillAssets(ctx context.Context, profile semanticEmbeddingProfile, limit int, sourceKeys []string, embeddingSourceKey string) ([]semanticAsset, error) {
	if limit <= 0 {
		return []semanticAsset{}, nil
	}
	retryBefore := formatCatalogTime(time.Now().UTC().Add(-semanticBackfillFailureRetryInterval))
	scopeClause, scopeArgs := canonicalSemanticScopeClause(sourceKeys)
	if embeddingSourceKey != "" {
		scopeClause += " AND input.embedding_source_key = ?"
		scopeArgs = append(scopeArgs, embeddingSourceKey)
	}
	inputsCTE, queryArgs := canonicalSemanticInputs(sourceKeys)
	queryArgs = append(queryArgs,
		canonicalSemanticCorpusSourceKey,
		profile.ModelID(),
		profile.ModelID(),
		profile.VectorSpaceID(),
		profile.EmbeddingDim(),
		retryBefore,
	)
	queryArgs = append(queryArgs, scopeArgs...)
	queryArgs = append(queryArgs, limit)
	rows, err := s.backgroundQueryDB().QueryContext(ctx, inputsCTE+`
	SELECT input.canonical_asset_id, input.media_type, input.filename, input.captured_at, input.duration,
		input.embedding_source_key, input.embedding_upstream_asset_id, input.embedding_input,
		input.local_rendition_sha256, input.content_sha1_hex, input.content_size_bytes,
		COALESCE(retry.requested_at, '')
	FROM resolved_inputs input
	LEFT JOIN semantic_vectors vector
		ON vector.source_key = ?
		AND vector.upstream_asset_id = input.canonical_asset_id
		AND vector.model_id = ?
	LEFT JOIN canonical_semantic_vector_inputs stored_input
		ON stored_input.canonical_asset_id = input.canonical_asset_id
		AND stored_input.model_id = ?
	LEFT JOIN semantic_vector_retry_requests retry
		ON retry.source_key = vector.source_key
		AND retry.upstream_asset_id = vector.upstream_asset_id
		AND retry.model_id = vector.model_id
	WHERE input.embedding_source_key <> ''
		AND (
			vector.upstream_asset_id IS NULL
			OR vector.vector_space_id != ?
			OR vector.embedding_dim != ?
			OR vector.status NOT IN ('ready', 'failed')
			OR (vector.status = 'failed' AND (
				vector.generated_at IS NULL
				OR vector.generated_at <= ?
				OR retry.requested_at IS NOT NULL
			))
			OR stored_input.refresh_required != 0
			OR (vector.status = 'ready' AND stored_input.canonical_asset_id IS NULL)
		)
		`+scopeClause+`
	ORDER BY CASE WHEN vector.status = 'failed' THEN 1 ELSE 0 END ASC,
		input.captured_at DESC, input.canonical_asset_id ASC
	LIMIT ?`,
		queryArgs...,
	)
	if err != nil {
		return nil, fmt.Errorf("query canonical semantic indexing assets: %w", err)
	}
	defer rows.Close()

	assets := make([]semanticAsset, 0, limit)
	for rows.Next() {
		var asset semanticAsset
		var capturedAtText string
		var duration sql.NullString
		var contentSHA1 string
		var contentSize int64
		if err := rows.Scan(
			&asset.ID,
			&asset.MediaType,
			&asset.Filename,
			&capturedAtText,
			&duration,
			&asset.EmbeddingSourceKey,
			&asset.EmbeddingUpstreamAssetID,
			&asset.EmbeddingInput,
			&asset.RenditionSHA256,
			&contentSHA1,
			&contentSize,
			&asset.SelectedRetryRequestedAt,
		); err != nil {
			return nil, fmt.Errorf("scan canonical semantic indexing asset: %w", err)
		}
		capturedAt, err := time.Parse(time.RFC3339Nano, capturedAtText)
		if err != nil {
			return nil, fmt.Errorf("parse canonical semantic indexing captured_at: %w", err)
		}
		asset.CapturedAt = capturedAt.UTC()
		if duration.Valid {
			value := duration.String
			asset.Duration = &value
		}
		asset.SourceKey = canonicalSemanticCorpusSourceKey
		asset.EmbeddingContentSHA1 = contentSHA1
		asset.EmbeddingContentSize = contentSize
		assets = append(assets, asset)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate canonical semantic indexing assets: %w", err)
	}
	return assets, nil
}

func (s *CatalogStore) backfillCanonicalSemanticVectors(ctx context.Context, profile semanticEmbeddingProfile, startedAt time.Time, options SemanticBackfillOptions) (SemanticBackfillResult, error) {
	if s == nil || s.db == nil || profile == nil {
		return SemanticBackfillResult{}, ErrCatalogNotConfigured
	}
	limit := max(0, min(options.MaxAssets, 100))
	assets, err := s.loadCanonicalSemanticBackfillAssets(ctx, profile, limit, options.CanonicalSourceKeys, options.CanonicalEmbeddingSourceKey)
	if err != nil {
		return SemanticBackfillResult{}, err
	}
	embedded := assets
	failed := []semanticBackfillAssetFailure{}
	if len(assets) > 0 {
		embedded, failed, err = s.embedSemanticBackfillAssets(ctx, profile, assets, options)
		if err != nil {
			return SemanticBackfillResult{}, err
		}
	}
	now := time.Now().UTC()
	if len(embedded) > 0 {
		if err := s.upsertCanonicalSemanticVectors(ctx, profile, embedded, now); err != nil {
			return SemanticBackfillResult{}, err
		}
	}
	if len(failed) > 0 {
		if err := s.upsertSemanticVectorFailures(ctx, canonicalSemanticCorpusSourceKey, profile, failed, now); err != nil {
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
	status, err := s.canonicalSemanticBackfillStatusForScope(ctx, profileStatus, options.CanonicalSourceKeys)
	if err != nil {
		return SemanticBackfillResult{}, err
	}
	if err := s.upsertSemanticBackfillState(ctx, canonicalSemanticCorpusSourceKey, profile, status, now); err != nil {
		return SemanticBackfillResult{}, err
	}
	return SemanticBackfillResult{
		ProcessedVectorCount: len(embedded) + len(failed),
		IndexedVectorCount:   status.IndexedVectorCount,
		StartedAt:            startedAt.UTC(),
		CompletedAt:          time.Now().UTC(),
		Status:               status,
	}, nil
}

func (s *CatalogStore) upsertCanonicalSemanticVectors(ctx context.Context, profile semanticEmbeddingProfile, assets []semanticAsset, now time.Time) error {
	if err := s.upsertSemanticVectors(ctx, canonicalSemanticCorpusSourceKey, profile, assets, now); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin canonical semantic input upsert: %w", err)
	}
	defer tx.Rollback()
	if err := upsertCanonicalSemanticInputsInTx(ctx, tx, profile, assets, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit canonical semantic input upsert: %w", err)
	}
	return nil
}

func upsertCanonicalSemanticInputsInTx(ctx context.Context, tx *sql.Tx, profile semanticEmbeddingProfile, assets []semanticAsset, now time.Time) error {
	statement, err := tx.PrepareContext(ctx, `INSERT INTO canonical_semantic_vector_inputs (
			canonical_asset_id, model_id, representative_source_key, representative_upstream_asset_id,
			embedding_input, input_fingerprint, rendition_sha256, preprocessing_version,
			refresh_required, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT(canonical_asset_id, model_id) DO UPDATE SET
			representative_source_key = excluded.representative_source_key,
			representative_upstream_asset_id = excluded.representative_upstream_asset_id,
			embedding_input = excluded.embedding_input,
			input_fingerprint = excluded.input_fingerprint,
			rendition_sha256 = excluded.rendition_sha256,
			preprocessing_version = excluded.preprocessing_version,
			refresh_required = 0,
			updated_at = excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("prepare canonical semantic input upsert: %w", err)
	}
	defer statement.Close()
	for _, asset := range assets {
		if asset.InputFingerprint == "" {
			asset.InputFingerprint = canonicalSemanticInputFingerprint(asset, asset.EmbeddingInput, asset.EmbeddingContentSHA1, asset.EmbeddingContentSize)
		}
		if _, err := statement.ExecContext(ctx,
			asset.ID,
			profile.ModelID(),
			asset.EmbeddingSourceKey,
			asset.EmbeddingUpstreamAssetID,
			asset.EmbeddingInput,
			asset.InputFingerprint,
			asset.RenditionSHA256,
			canonicalSemanticPreprocessingVersion,
			formatCatalogTime(now),
		); err != nil {
			return fmt.Errorf("upsert canonical semantic input %q: %w", asset.ID, err)
		}
	}
	return nil
}

func (s *CatalogStore) canonicalSemanticBackfillStatus(ctx context.Context, profile SemanticModelProfileStatus) (SemanticModelBackfillStatus, error) {
	return s.canonicalSemanticBackfillStatusForScope(ctx, profile, nil)
}

func (s *CatalogStore) canonicalSemanticBackfillStatusForSourceKeys(ctx context.Context, profile SemanticModelProfileStatus, sourceKeys []string) (*SemanticModelBackfillStatus, error) {
	status, err := s.canonicalSemanticBackfillStatusForScope(ctx, profile, sourceKeys)
	if err != nil {
		return nil, err
	}
	return &status, nil
}

func (s *CatalogStore) canonicalSemanticBackfillStatusForScope(ctx context.Context, profile SemanticModelProfileStatus, sourceKeys []string, deferredSourceKeys ...string) (SemanticModelBackfillStatus, error) {
	modelID := strings.TrimSpace(profile.ModelID)
	vectorSpaceID := strings.TrimSpace(profile.VectorSpaceID)
	if s == nil || s.db == nil || modelID == "" || vectorSpaceID == "" {
		return SemanticModelBackfillStatus{}, ErrCatalogNotConfigured
	}
	status := SemanticModelBackfillStatus{
		SourceKind:        "canonical_catalog",
		ModelID:           modelID,
		VectorSpaceID:     vectorSpaceID,
		EmbeddingDim:      profile.EmbeddingDim,
		IndexedGeneration: -1,
	}
	assetGeneration, indexedGeneration, err := s.semanticIndexGenerations(ctx, canonicalSemanticCorpusSourceKey, modelID)
	if err != nil {
		return SemanticModelBackfillStatus{}, err
	}
	status.AssetGeneration = assetGeneration
	status.IndexedGeneration = indexedGeneration
	if assetGeneration != indexedGeneration {
		status.GenerationMismatchSourceCount = 1
	}
	lastPublishedAt, err := s.semanticBackfillLastPublishedAt(ctx, canonicalSemanticCorpusSourceKey, modelID, vectorSpaceID)
	if err != nil {
		return SemanticModelBackfillStatus{}, err
	}
	status.LastPublishedAt = lastPublishedAt
	counts, err := s.canonicalSemanticCounts(ctx, profile, time.Now().UTC(), sourceKeys, deferredSourceKeys...)
	if err != nil {
		return SemanticModelBackfillStatus{}, err
	}
	status.EligibleAssetCount = counts.eligible
	status.CompletedVectorCount = counts.completed
	status.FailedVectorCount = counts.failed
	status.EligibleNowVectorCount = counts.eligibleNow
	status.NextEligibleAt = counts.nextEligibleAt
	indexedCount, err := s.canonicalSemanticIndexedVectorCount(ctx, profile, sourceKeys)
	if err != nil {
		return SemanticModelBackfillStatus{}, fmt.Errorf("read canonical indexed vector count: %w", err)
	}
	status.IndexedVectorCount = indexedCount
	pendingJobs, failedJobs, eligibleJobs, nextJobAt, err := s.semanticIndexJobState(ctx, canonicalSemanticCorpusSourceKey, modelID, vectorSpaceID, time.Now().UTC())
	if err != nil {
		return SemanticModelBackfillStatus{}, err
	}
	status.PendingIndexJobCount = pendingJobs
	status.FailedIndexJobCount = failedJobs
	status.EligibleIndexJobCount = eligibleJobs
	if nextJobAt != nil && (status.NextEligibleAt == nil || nextJobAt.Before(*status.NextEligibleAt)) {
		value := nextJobAt.UTC()
		status.NextEligibleAt = &value
	}
	status.RemainingVectorCount = max(0, status.EligibleAssetCount-status.CompletedVectorCount)
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

type canonicalSemanticCount struct {
	eligible       int
	completed      int
	failed         int
	eligibleNow    int
	nextEligibleAt *time.Time
}

func (s *CatalogStore) canonicalSemanticIndexedVectorCount(ctx context.Context, profile SemanticModelProfileStatus, sourceKeys []string) (int, error) {
	keys := canonicalSemanticSourceKeys(sourceKeys)
	if len(keys) == 0 {
		var indexed int
		err := s.backgroundQueryDB().QueryRowContext(ctx, `SELECT COALESCE(indexed_vector_count, 0)
			FROM semantic_state WHERE source_key = ? AND model_id = ? AND vector_space_id = ?`,
			canonicalSemanticCorpusSourceKey, strings.TrimSpace(profile.ModelID), strings.TrimSpace(profile.VectorSpaceID)).Scan(&indexed)
		if err != nil && err != sql.ErrNoRows {
			return 0, err
		}
		return indexed, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := []any{
		canonicalSemanticCorpusSourceKey,
		strings.TrimSpace(profile.ModelID),
		strings.TrimSpace(profile.VectorSpaceID),
	}
	for _, sourceKey := range keys {
		args = append(args, sourceKey)
	}
	var indexed int
	err := s.backgroundQueryDB().QueryRowContext(ctx, `SELECT COUNT(DISTINCT membership.upstream_asset_id)
		FROM semantic_index_membership membership
		JOIN semantic_state state
			ON state.source_key = membership.source_key
			AND state.model_id = membership.model_id
			AND state.vector_space_id = membership.vector_space_id
			AND state.indexed_generation = membership.asset_generation
		JOIN catalog_assets source
			ON source.canonical_asset_id = membership.upstream_asset_id
		WHERE membership.source_key = ?
			AND membership.model_id = ?
			AND membership.vector_space_id = ?
			AND source.visibility_status = 'active'
			AND source.source_key IN (`+placeholders+`)`, args...).Scan(&indexed)
	if err != nil {
		return 0, err
	}
	return indexed, nil
}

func (s *CatalogStore) canonicalSemanticCounts(ctx context.Context, profile SemanticModelProfileStatus, now time.Time, sourceKeys []string, deferredSourceKeys ...string) (canonicalSemanticCount, error) {
	retryBefore := formatCatalogTime(now.UTC().Add(-semanticBackfillFailureRetryInterval))
	var count canonicalSemanticCount
	var nextFailure sql.NullString
	scopeClause, scopeArgs := canonicalSemanticScopeClause(sourceKeys)
	inputsCTE, queryArgs := canonicalSemanticInputs(sourceKeys)
	readyClause := ""
	if len(deferredSourceKeys) > 0 {
		readyClause = " AND input.embedding_source_key NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(deferredSourceKeys)), ",") + ")"
	}
	queryArgs = append(queryArgs,
		profile.VectorSpaceID,
		profile.EmbeddingDim,
		profile.VectorSpaceID,
		profile.EmbeddingDim,
		profile.VectorSpaceID,
		profile.EmbeddingDim,
	)
	for _, key := range deferredSourceKeys {
		queryArgs = append(queryArgs, key)
	}
	queryArgs = append(queryArgs,
		profile.VectorSpaceID,
		profile.EmbeddingDim,
		retryBefore,
		retryBefore,
		canonicalSemanticCorpusSourceKey,
		profile.ModelID,
		profile.ModelID,
	)
	queryArgs = append(queryArgs, scopeArgs...)
	err := s.backgroundQueryDB().QueryRowContext(ctx, inputsCTE+`
	SELECT
		COALESCE(SUM(CASE WHEN input.embedding_source_key <> '' OR (
			vector.status = 'ready' AND vector.vector_space_id = ? AND vector.embedding_dim = ?
		) THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN vector.status = 'ready'
			AND vector.vector_space_id = ? AND vector.embedding_dim = ?
			AND (COALESCE(stored_input.refresh_required, 1) = 0
				OR input.embedding_source_key = '') THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN vector.status = 'failed'
			AND input.embedding_source_key <> ''
			AND vector.vector_space_id = ? AND vector.embedding_dim = ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN input.embedding_source_key <> ''`+readyClause+` AND (
			vector.upstream_asset_id IS NULL
			OR vector.vector_space_id != ?
			OR vector.embedding_dim != ?
			OR vector.status NOT IN ('ready', 'failed')
			OR (vector.status = 'failed' AND (retry.requested_at IS NOT NULL OR vector.generated_at IS NULL OR vector.generated_at <= ?))
			OR stored_input.refresh_required != 0
			OR (vector.status = 'ready' AND stored_input.canonical_asset_id IS NULL)
		) THEN 1 ELSE 0 END), 0),
		MIN(CASE WHEN vector.status = 'failed'
			AND input.embedding_source_key <> ''
			AND retry.requested_at IS NULL AND vector.generated_at > ?
			THEN vector.generated_at END)
	FROM resolved_inputs input
	LEFT JOIN semantic_vectors vector
		ON vector.source_key = ? AND vector.upstream_asset_id = input.canonical_asset_id AND vector.model_id = ?
	LEFT JOIN canonical_semantic_vector_inputs stored_input
		ON stored_input.canonical_asset_id = input.canonical_asset_id AND stored_input.model_id = ?
	LEFT JOIN semantic_vector_retry_requests retry
		ON retry.source_key = vector.source_key AND retry.upstream_asset_id = vector.upstream_asset_id AND retry.model_id = vector.model_id
	WHERE 1 = 1`+scopeClause,
		queryArgs...,
	).Scan(&count.eligible, &count.completed, &count.failed, &count.eligibleNow, &nextFailure)
	if err != nil {
		return canonicalSemanticCount{}, fmt.Errorf("count canonical semantic progress: %w", err)
	}
	if nextFailure.Valid && strings.TrimSpace(nextFailure.String) != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, nextFailure.String)
		if parseErr != nil {
			return canonicalSemanticCount{}, fmt.Errorf("parse canonical semantic retry time: %w", parseErr)
		}
		next := parsed.UTC().Add(semanticBackfillFailureRetryInterval)
		count.nextEligibleAt = &next
	}
	return count, nil
}

func (s *CatalogStore) canonicalSemanticCorpusExists(ctx context.Context, profile semanticEmbeddingProfile) bool {
	if s == nil || s.db == nil || profile == nil {
		return false
	}
	var exists int
	err := s.queryDB().QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM semantic_state
		WHERE source_key = ? AND model_id = ? AND indexed_generation >= 0
	)`, canonicalSemanticCorpusSourceKey, profile.ModelID()).Scan(&exists)
	return err == nil && exists != 0
}
