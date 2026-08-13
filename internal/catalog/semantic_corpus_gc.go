package catalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type semanticCorpusTarget struct {
	sourceKey     string
	modelID       string
	vectorSpaceID string
	embeddingDim  int
}

// GarbageCollectSemanticModelCorpora removes vector payload references, index
// state, jobs, and binary index files that are not reachable from an installed
// active or candidate model role.
func (s *Service) GarbageCollectSemanticModelCorpora(ctx context.Context, reachable []SemanticModelCorpusIdentity) (int, error) {
	if s == nil || s.catalog == nil || s.catalog.db == nil {
		return 0, ErrCatalogNotConfigured
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reachableKeys := make(map[string]struct{}, len(reachable))
	for _, identity := range reachable {
		modelID := strings.TrimSpace(identity.ModelID)
		vectorSpaceID := strings.TrimSpace(identity.VectorSpaceID)
		if modelID != "" && vectorSpaceID != "" {
			reachableKeys[modelID+"\x00"+vectorSpaceID] = struct{}{}
		}
	}

	targets, identities, err := s.catalog.unreachableSemanticCorpusTargets(ctx, reachableKeys)
	if err != nil || len(identities) == 0 {
		return 0, err
	}
	// Remove identity-addressed binary files while their source/dimension rows
	// still exist. On failure the durable metadata remains available so startup
	// or the next role transition can retry the same cleanup.
	for _, target := range targets {
		profile := semanticIndexFileProfile{
			modelID:       target.modelID,
			vectorSpaceID: target.vectorSpaceID,
			embeddingDim:  target.embeddingDim,
		}
		if err := s.catalog.removeSemanticBinaryIndexCorpus(ctx, target.sourceKey, profile); err != nil {
			return 0, err
		}
	}
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin semantic corpus GC: %w", err)
	}
	defer tx.Rollback()
	for _, identity := range identities {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO semantic_vector_payload_gc_candidates(batch_id)
			SELECT payload_batch_id
			FROM semantic_vectors
			WHERE model_id = ? AND vector_space_id = ? AND payload_batch_id IS NOT NULL`, identity.ModelID, identity.VectorSpaceID); err != nil {
			return 0, fmt.Errorf("queue retired semantic payloads: %w", err)
		}
		for _, statement := range []string{
			`DELETE FROM semantic_index_jobs WHERE model_id = ? AND vector_space_id = ?`,
			`DELETE FROM semantic_index_membership WHERE model_id = ? AND vector_space_id = ?`,
			`DELETE FROM semantic_index_membership_state WHERE model_id = ? AND vector_space_id = ?`,
			`DELETE FROM semantic_vectors WHERE model_id = ? AND vector_space_id = ?`,
			`DELETE FROM semantic_state WHERE model_id = ? AND vector_space_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, statement, identity.ModelID, identity.VectorSpaceID); err != nil {
				return 0, fmt.Errorf("remove retired semantic corpus metadata: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit semantic corpus GC: %w", err)
	}

	if err := s.catalog.cleanupSemanticVectorPayloadCandidates(ctx); err != nil {
		return len(identities), err
	}
	return len(identities), nil
}

func (s *CatalogStore) unreachableSemanticCorpusTargets(ctx context.Context, reachable map[string]struct{}) ([]semanticCorpusTarget, []SemanticModelCorpusIdentity, error) {
	rows, err := s.queryDB().QueryContext(ctx, `SELECT source_key, model_id, vector_space_id, embedding_dim FROM semantic_state
		UNION
		SELECT source_key, model_id, vector_space_id, embedding_dim FROM semantic_vectors
		UNION
		SELECT source_key, model_id, vector_space_id, embedding_dim FROM semantic_index_jobs
		ORDER BY model_id, vector_space_id, source_key, embedding_dim`)
	if err != nil {
		return nil, nil, fmt.Errorf("query semantic corpus identities: %w", err)
	}
	defer rows.Close()
	targets := []semanticCorpusTarget{}
	identities := []SemanticModelCorpusIdentity{}
	seenIdentities := map[string]struct{}{}
	for rows.Next() {
		var target semanticCorpusTarget
		if err := rows.Scan(&target.sourceKey, &target.modelID, &target.vectorSpaceID, &target.embeddingDim); err != nil {
			return nil, nil, fmt.Errorf("scan semantic corpus identity: %w", err)
		}
		key := target.modelID + "\x00" + target.vectorSpaceID
		if _, ok := reachable[key]; ok {
			continue
		}
		targets = append(targets, target)
		if _, ok := seenIdentities[key]; !ok {
			seenIdentities[key] = struct{}{}
			identities = append(identities, SemanticModelCorpusIdentity{
				ModelID:       target.modelID,
				VectorSpaceID: target.vectorSpaceID,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate semantic corpus identities: %w", err)
	}
	return targets, identities, nil
}

func (s *CatalogStore) removeSemanticBinaryIndexCorpus(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile) error {
	if s == nil || profile == nil {
		return nil
	}
	root := filepath.Join(s.root, semanticBinaryIndexDirName)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read semantic index corpus directory: %w", err)
	}
	base := s.semanticBinaryIndexBaseName(sourceKey, profile)
	removed := false
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, base+".") {
			continue
		}
		if name != base+".active.json" && name != base+".tidx" &&
			!strings.HasSuffix(name, ".tidx") &&
			!strings.Contains(name, ".build.db") {
			continue
		}
		path := filepath.Join(root, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove retired semantic index file %s: %w", path, err)
		}
		s.forgetSemanticBinaryIntegrity(path)
		removed = true
	}
	if removed {
		return syncDirectory(root)
	}
	return nil
}
