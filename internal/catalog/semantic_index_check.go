package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SemanticIndexCheckOptions selects a consistency check scope.
type SemanticIndexCheckOptions struct {
	SourceKey     string
	ModelID       string
	VectorSpaceID string
	Deep          bool
}

// SemanticIndexCheckResult reports semantic vector payload and binary index
// consistency without loading the full search index into memory.
type SemanticIndexCheckResult struct {
	StartedAt          time.Time                  `json:"startedAt"`
	CompletedAt        time.Time                  `json:"completedAt"`
	Deep               bool                       `json:"deep"`
	SourceCount        int                        `json:"sourceCount"`
	CheckedVectorCount int                        `json:"checkedVectorCount"`
	WarningCount       int                        `json:"warningCount"`
	ErrorCount         int                        `json:"errorCount"`
	Sources            []SemanticIndexCheckSource `json:"sources"`
	Issues             []SemanticIndexCheckIssue  `json:"issues,omitempty"`
}

// SemanticIndexCheckSource reports one source/model/vector-space target.
type SemanticIndexCheckSource struct {
	SourceKey             string `json:"sourceKey"`
	ModelID               string `json:"modelId"`
	VectorSpaceID         string `json:"vectorSpaceId"`
	EmbeddingDim          int    `json:"embeddingDim"`
	ReadyVectorCount      int    `json:"readyVectorCount"`
	IndexedVectorCount    int    `json:"indexedVectorCount"`
	StateStatus           string `json:"stateStatus,omitempty"`
	StateCompletedVectors int    `json:"stateCompletedVectors,omitempty"`
	StateIndexedVectors   int    `json:"stateIndexedVectors,omitempty"`
	BinaryPath            string `json:"binaryPath,omitempty"`
	BinaryNodeCount       int    `json:"binaryNodeCount,omitempty"`
	BinaryEdgeCount       int    `json:"binaryEdgeCount,omitempty"`
	PayloadBatchCount     int    `json:"payloadBatchCount,omitempty"`
	PayloadPath           string `json:"payloadPath,omitempty"`
	PayloadSizeBytes      int64  `json:"payloadSizeBytes,omitempty"`
}

// SemanticIndexCheckIssue is one consistency issue.
type SemanticIndexCheckIssue struct {
	Severity      string `json:"severity"`
	Component     string `json:"component"`
	SourceKey     string `json:"sourceKey,omitempty"`
	ModelID       string `json:"modelId,omitempty"`
	VectorSpaceID string `json:"vectorSpaceId,omitempty"`
	Message       string `json:"message"`
}

type semanticIndexCheckTarget struct {
	SourceKey              string
	ModelID                string
	VectorSpaceID          string
	EmbeddingDim           int
	ReadyVectorCount       int
	StateStatus            string
	StateCompletedVectors  int
	StateIndexedVectors    int
	StateIndexedGeneration int64
}

// CheckSemanticIndex verifies the fp32 file-backed semantic search index and
// the SQLite metadata rows that point to vector payload files.
func (s *CatalogStore) CheckSemanticIndex(ctx context.Context, options SemanticIndexCheckOptions) (SemanticIndexCheckResult, error) {
	if s == nil || s.db == nil {
		return SemanticIndexCheckResult{}, ErrCatalogNotConfigured
	}
	if ctx == nil {
		ctx = context.Background()
	}
	startedAt := time.Now().UTC()
	result := SemanticIndexCheckResult{
		StartedAt: startedAt,
		Deep:      options.Deep,
	}
	if err := s.checkSemanticVectorSchema(ctx, &result); err != nil {
		return result, err
	}
	targets, err := s.semanticIndexCheckTargets(ctx, options)
	if err != nil {
		return result, err
	}
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		source := SemanticIndexCheckSource{
			SourceKey:             target.SourceKey,
			ModelID:               target.ModelID,
			VectorSpaceID:         target.VectorSpaceID,
			EmbeddingDim:          target.EmbeddingDim,
			ReadyVectorCount:      target.ReadyVectorCount,
			IndexedVectorCount:    target.StateIndexedVectors,
			StateStatus:           target.StateStatus,
			StateCompletedVectors: target.StateCompletedVectors,
			StateIndexedVectors:   target.StateIndexedVectors,
		}
		s.checkSemanticVectorPayload(ctx, options, target, &source, &result)
		s.checkSemanticBinaryIndex(ctx, target, &source, &result)
		result.CheckedVectorCount += target.ReadyVectorCount
		result.Sources = append(result.Sources, source)
	}
	result.SourceCount = len(result.Sources)
	result.CompletedAt = time.Now().UTC()
	return result, nil
}

func (s *CatalogStore) checkSemanticVectorSchema(ctx context.Context, result *SemanticIndexCheckResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	columns, err := s.tableColumns("semantic_vectors")
	if err != nil {
		return err
	}
	if !columns["payload_batch_id"] || !columns["vector_offset"] || !columns["vector_length"] {
		result.addSemanticIndexIssue("error", "schema", "", "", "", "semantic_vectors is missing current payload batch references")
	}
	return nil
}

func (s *CatalogStore) semanticIndexCheckTargets(ctx context.Context, options SemanticIndexCheckOptions) ([]semanticIndexCheckTarget, error) {
	clauses := []string{"1 = 1"}
	args := []any{}
	if sourceKey := strings.TrimSpace(options.SourceKey); sourceKey != "" {
		clauses = append(clauses, "identity.source_key = ?")
		args = append(args, sourceKey)
	}
	if modelID := strings.TrimSpace(options.ModelID); modelID != "" {
		clauses = append(clauses, "identity.model_id = ?")
		args = append(args, modelID)
	}
	if vectorSpaceID := strings.TrimSpace(options.VectorSpaceID); vectorSpaceID != "" {
		clauses = append(clauses, "identity.vector_space_id = ?")
		args = append(args, vectorSpaceID)
	}
	db := s.queryDB()
	rows, err := db.QueryContext(ctx, `WITH identity AS (
			SELECT source_key, model_id, vector_space_id, embedding_dim FROM semantic_state
			UNION
			SELECT source_key, model_id, vector_space_id, embedding_dim FROM semantic_vectors
		)
		SELECT
			identity.source_key,
			identity.model_id,
			identity.vector_space_id,
			identity.embedding_dim,
			COUNT(v.upstream_asset_id) AS ready_vectors,
			COALESCE(st.status, ''),
			COALESCE(st.completed_vector_count, 0),
			COALESCE(st.indexed_vector_count, 0),
			COALESCE(st.indexed_generation, -1)
		FROM identity
		LEFT JOIN semantic_vectors v
			ON v.source_key = identity.source_key
			AND v.model_id = identity.model_id
			AND v.vector_space_id = identity.vector_space_id
			AND v.embedding_dim = identity.embedding_dim
			AND v.status = 'ready'
		LEFT JOIN semantic_state st
			ON st.source_key = identity.source_key AND st.model_id = identity.model_id
		WHERE `+strings.Join(clauses, " AND ")+`
		GROUP BY identity.source_key, identity.model_id, identity.vector_space_id, identity.embedding_dim,
			st.status, st.completed_vector_count, st.indexed_vector_count, st.indexed_generation
		ORDER BY identity.source_key, identity.model_id, identity.vector_space_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("query semantic index check targets: %w", err)
	}
	defer rows.Close()
	targets := []semanticIndexCheckTarget{}
	seen := map[string]struct{}{}
	for rows.Next() {
		var target semanticIndexCheckTarget
		if err := rows.Scan(
			&target.SourceKey,
			&target.ModelID,
			&target.VectorSpaceID,
			&target.EmbeddingDim,
			&target.ReadyVectorCount,
			&target.StateStatus,
			&target.StateCompletedVectors,
			&target.StateIndexedVectors,
			&target.StateIndexedGeneration,
		); err != nil {
			return nil, fmt.Errorf("scan semantic index check target: %w", err)
		}
		targets = append(targets, target)
		seen[semanticIndexCheckTargetKey(target)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate semantic index check targets: %w", err)
	}
	manifestTargets, err := s.semanticIndexManifestTargets(ctx, options, seen)
	if err != nil {
		return nil, err
	}
	targets = append(targets, manifestTargets...)
	return targets, nil
}

func (s *CatalogStore) semanticIndexManifestTargets(ctx context.Context, options SemanticIndexCheckOptions, seen map[string]struct{}) ([]semanticIndexCheckTarget, error) {
	root := filepath.Join(s.root, semanticBinaryIndexDirName)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read semantic index manifests: %w", err)
	}
	targets := []semanticIndexCheckTarget{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".active.json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}
		var manifest semanticBinaryActiveManifest
		if json.Unmarshal(raw, &manifest) != nil {
			continue
		}
		target := semanticIndexCheckTarget{
			SourceKey:              strings.TrimSpace(manifest.Header.SourceKey),
			ModelID:                strings.TrimSpace(manifest.Header.ModelID),
			VectorSpaceID:          strings.TrimSpace(manifest.Header.VectorSpaceID),
			EmbeddingDim:           manifest.Header.EmbeddingDim,
			StateIndexedGeneration: -1,
		}
		if target.SourceKey == "" || target.ModelID == "" || target.VectorSpaceID == "" || target.EmbeddingDim <= 0 ||
			(strings.TrimSpace(options.SourceKey) != "" && strings.TrimSpace(options.SourceKey) != target.SourceKey) ||
			(strings.TrimSpace(options.ModelID) != "" && strings.TrimSpace(options.ModelID) != target.ModelID) ||
			(strings.TrimSpace(options.VectorSpaceID) != "" && strings.TrimSpace(options.VectorSpaceID) != target.VectorSpaceID) {
			continue
		}
		key := semanticIndexCheckTargetKey(target)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		targets = append(targets, target)
	}
	return targets, nil
}

func semanticIndexCheckTargetKey(target semanticIndexCheckTarget) string {
	return strings.Join([]string{target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("%d", target.EmbeddingDim)}, "\x00")
}

func (s *CatalogStore) checkSemanticVectorPayload(ctx context.Context, options SemanticIndexCheckOptions, target semanticIndexCheckTarget, source *SemanticIndexCheckSource, result *SemanticIndexCheckResult) {
	rows, err := s.queryDB().QueryContext(ctx, `SELECT DISTINCT b.batch_id, b.relative_path, b.size_bytes
		FROM semantic_vectors v
		JOIN semantic_vector_payload_batches b ON b.batch_id = v.payload_batch_id
		WHERE v.source_key = ? AND v.model_id = ? AND v.vector_space_id = ? AND v.embedding_dim = ? AND v.status = 'ready'
		ORDER BY b.batch_id`, target.SourceKey, target.ModelID, target.VectorSpaceID, target.EmbeddingDim)
	if err != nil {
		result.addSemanticIndexIssue("error", "payload", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("query semantic vector payload batches failed: %v", err))
		return
	}
	defer rows.Close()
	for rows.Next() {
		var batchID, relativePath string
		var expectedSize int64
		if err := rows.Scan(&batchID, &relativePath, &expectedSize); err != nil {
			result.addSemanticIndexIssue("error", "payload", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("scan semantic vector payload batch failed: %v", err))
			return
		}
		path := filepath.Join(s.root, semanticVectorPayloadDirName, filepath.Base(relativePath))
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
			result.addSemanticIndexIssue("error", "payload", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("semantic vector payload batch %s is missing or truncated: %v", batchID, statErr))
			continue
		}
		source.PayloadBatchCount++
		source.PayloadSizeBytes += info.Size()
		if source.PayloadBatchCount == 1 {
			source.PayloadPath = path
		} else {
			source.PayloadPath = ""
		}
	}
	if err := rows.Err(); err != nil {
		result.addSemanticIndexIssue("error", "payload", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("iterate semantic vector payload batches failed: %v", err))
		return
	}
	var invalidRefs int
	db := s.queryDB()
	if err := db.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(CASE
				WHEN v.payload_batch_id IS NULL OR b.batch_id IS NULL OR v.vector_offset < 0
					OR v.vector_length != v.embedding_dim * 4 OR v.vector_offset + v.vector_length > b.size_bytes
				THEN 1 ELSE 0 END), 0)
		FROM semantic_vectors v
		LEFT JOIN semantic_vector_payload_batches b ON b.batch_id = v.payload_batch_id
		WHERE v.source_key = ? AND v.model_id = ? AND v.vector_space_id = ? AND v.embedding_dim = ? AND v.status = 'ready'`,
		target.SourceKey, target.ModelID, target.VectorSpaceID, target.EmbeddingDim,
	).Scan(&invalidRefs); err != nil {
		result.addSemanticIndexIssue("error", "payload", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("query semantic vector payload refs failed: %v", err))
		return
	}
	if invalidRefs > 0 {
		result.addSemanticIndexIssue("error", "payload", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("%d vector payload refs have invalid offsets or lengths", invalidRefs))
	}
	if options.Deep {
		s.deepCheckSemanticVectorPayload(ctx, target, result)
	}
}

func (s *CatalogStore) deepCheckSemanticVectorPayload(ctx context.Context, target semanticIndexCheckTarget, result *SemanticIndexCheckResult) {
	db := s.queryDB()
	rows, err := db.QueryContext(ctx, `SELECT upstream_asset_id, payload_batch_id, vector_offset, vector_length
		FROM semantic_vectors
		WHERE source_key = ? AND model_id = ? AND vector_space_id = ? AND embedding_dim = ? AND status = 'ready'
		ORDER BY payload_batch_id, vector_offset, upstream_asset_id`,
		target.SourceKey, target.ModelID, target.VectorSpaceID, target.EmbeddingDim,
	)
	if err != nil {
		result.addSemanticIndexIssue("error", "payload", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("query vector payload refs for deep check failed: %v", err))
		return
	}
	defer rows.Close()
	currentBatchID := ""
	var currentBatch []byte
	currentBatchValid := false
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			result.addSemanticIndexIssue("error", "payload", target.SourceKey, target.ModelID, target.VectorSpaceID, err.Error())
			return
		}
		var upstreamAssetID string
		var batchID string
		var offset int64
		var length int
		if err := rows.Scan(&upstreamAssetID, &batchID, &offset, &length); err != nil {
			result.addSemanticIndexIssue("error", "payload", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("scan vector payload ref failed: %v", err))
			return
		}
		if batchID != currentBatchID {
			currentBatchID = batchID
			currentBatch = nil
			currentBatchValid = false
			verified, err := s.loadSemanticVectorPayloadBatch(ctx, batchID, true)
			if err != nil {
				result.addSemanticIndexIssue("error", "payload", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("batch %s verification failed: %v", batchID, err))
				continue
			}
			currentBatch = verified
			currentBatchValid = true
		}
		if !currentBatchValid {
			continue
		}
		end := offset + int64(length)
		if offset < 0 || length != target.EmbeddingDim*4 || end < offset || end > int64(len(currentBatch)) {
			result.addSemanticIndexIssue("error", "payload", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("asset %s vector range is invalid: offset=%d length=%d batch_size=%d", upstreamAssetID, offset, length, len(currentBatch)))
		}
	}
	if err := rows.Err(); err != nil {
		result.addSemanticIndexIssue("error", "payload", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("iterate vector payload refs failed: %v", err))
	}
}

func (s *CatalogStore) checkSemanticBinaryIndex(ctx context.Context, target semanticIndexCheckTarget, source *SemanticIndexCheckSource, result *SemanticIndexCheckResult) {
	profile := semanticIndexFileProfile{
		modelID:       target.ModelID,
		vectorSpaceID: target.VectorSpaceID,
		embeddingDim:  target.EmbeddingDim,
	}
	if target.StateIndexedVectors == 0 {
		manifestPath := s.semanticBinaryActiveManifestPath(target.SourceKey, profile)
		if _, err := os.Stat(manifestPath); err == nil {
			result.addSemanticIndexIssue("error", "binary_index", target.SourceKey, target.ModelID, target.VectorSpaceID, "active binary manifest remains while semantic state has zero indexed vectors")
		} else if !errors.Is(err, os.ErrNotExist) {
			result.addSemanticIndexIssue("error", "binary_index", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("inspect active binary manifest failed: %v", err))
		}
		if target.ReadyVectorCount == 0 && target.StateCompletedVectors > 0 {
			result.addSemanticIndexIssue("error", "state", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("semantic state reports %d completed vectors but no ready vector rows remain", target.StateCompletedVectors))
		}
		return
	}
	reader, semantic, err := s.openSemanticBinaryIndexFile(ctx, target.SourceKey, profile)
	if err != nil {
		result.addSemanticIndexIssue("error", "binary_index", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("open fp32 binary index failed: %v", err))
		return
	}
	defer reader.Close()
	path := s.semanticBinaryIndexPath(target.SourceKey, profile, reader.header.AssetGeneration)
	source.BinaryPath = path
	source.BinaryNodeCount = reader.header.NodeCount
	source.BinaryEdgeCount = reader.header.EdgeCount
	if reader.header.Precision != semanticSearchIndexPrecision {
		result.addSemanticIndexIssue("error", "binary_index", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("binary precision=%s want fp32", reader.header.Precision))
	}
	if semantic.IndexedVectorCount != target.StateIndexedVectors && target.StateIndexedVectors > 0 {
		result.addSemanticIndexIssue("error", "binary_index", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("binary indexed vectors=%d state indexed vectors=%d", semantic.IndexedVectorCount, target.StateIndexedVectors))
	}
	if reader.header.AssetGeneration != target.StateIndexedGeneration {
		result.addSemanticIndexIssue("error", "binary_index", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("active binary generation=%d state indexed generation=%d", reader.header.AssetGeneration, target.StateIndexedGeneration))
	}
	if reader.header.NodeCount != target.StateIndexedVectors {
		result.addSemanticIndexIssue("error", "binary_index", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("binary nodes=%d state indexed vectors=%d", reader.header.NodeCount, target.StateIndexedVectors))
	}
	if info, err := os.Stat(path); err == nil {
		if err := validateSemanticBinaryFileSize(info.Size(), reader.header, reader.nodes); err != nil {
			result.addSemanticIndexIssue("error", "binary_index", target.SourceKey, target.ModelID, target.VectorSpaceID, err.Error())
		}
	}
	if result.Deep {
		manifest, manifestErr := readSemanticBinaryActiveManifest(s.semanticBinaryActiveManifestPath(target.SourceKey, profile))
		if manifestErr != nil {
			result.addSemanticIndexIssue("error", "binary_index", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("read active manifest for digest check failed: %v", manifestErr))
		} else if verifyErr := s.verifySemanticBinaryActiveFile(ctx, path, manifest, true); verifyErr != nil {
			result.addSemanticIndexIssue("error", "binary_index", target.SourceKey, target.ModelID, target.VectorSpaceID, fmt.Sprintf("verify active binary digest failed: %v", verifyErr))
		}
	}
}

func validateSemanticBinaryFileSize(size int64, header semanticBinaryIndexHeader, nodes []semanticBinaryNodeRecord) error {
	minSize := header.StringOffset
	for _, node := range nodes {
		vectorEnd := node.VectorOffset + int64(header.VectorStride)
		if vectorEnd > minSize {
			minSize = vectorEnd
		}
		edgeEnd := node.EdgeOffset + int64(node.EdgeCount)*semanticBinaryIndexEdgeRecordSize
		if edgeEnd > minSize {
			minSize = edgeEnd
		}
		stringEnd := node.StringOffset + int64(node.StringLength)
		if stringEnd > minSize {
			minSize = stringEnd
		}
	}
	if minSize > size {
		return fmt.Errorf("binary index refs exceed file size: max_end=%d size=%d", minSize, size)
	}
	return nil
}

func (result *SemanticIndexCheckResult) addSemanticIndexIssue(severity string, component string, sourceKey string, modelID string, vectorSpaceID string, message string) {
	if result == nil {
		return
	}
	switch severity {
	case "error":
		result.ErrorCount++
	case "warning":
		result.WarningCount++
	default:
		severity = "warning"
		result.WarningCount++
	}
	result.Issues = append(result.Issues, SemanticIndexCheckIssue{
		Severity:      severity,
		Component:     component,
		SourceKey:     strings.TrimSpace(sourceKey),
		ModelID:       strings.TrimSpace(modelID),
		VectorSpaceID: strings.TrimSpace(vectorSpaceID),
		Message:       message,
	})
}
