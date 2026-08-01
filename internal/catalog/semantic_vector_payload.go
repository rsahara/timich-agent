package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	semanticVectorPayloadDirName    = "semantic-vector-payloads"
	semanticVectorPayloadExt        = ".vecs"
	semanticVectorPayloadCacheLimit = 32
)

var semanticVectorPayloadHashBufferPool = sync.Pool{New: func() any {
	buffer := make([]byte, 1024*1024)
	return &buffer
}}

type semanticVectorPayloadRef struct {
	BatchID string
	Offset  int64
	Length  int
}

type semanticVectorPayloadBatch struct {
	ID           string
	RelativePath string
	SizeBytes    int64
	SHA256       string
	CreatedAt    time.Time
}

func (s *CatalogStore) semanticVectorPayloadPath(batchID string) string {
	return filepath.Join(s.root, semanticVectorPayloadDirName, strings.ToLower(strings.TrimSpace(batchID))+semanticVectorPayloadExt)
}

func (s *CatalogStore) writeSemanticVectorPayloadBatch(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile, assets []semanticAsset) (semanticVectorPayloadBatch, error) {
	if profile == nil || len(assets) == 0 {
		return semanticVectorPayloadBatch{}, ErrCatalogNotConfigured
	}
	raw := make([]byte, 0, len(assets)*profile.EmbeddingDim()*4)
	for index := range assets {
		if len(assets[index].Vector) != profile.EmbeddingDim() {
			return semanticVectorPayloadBatch{}, fmt.Errorf("semantic vector payload %q dim = %d, want %d", assets[index].ID, len(assets[index].Vector), profile.EmbeddingDim())
		}
		offset := int64(len(raw))
		encoded := encodeSemanticVector(assets[index].Vector)
		raw = append(raw, encoded...)
		assets[index].VectorPayload = semanticVectorPayloadRef{Offset: offset, Length: len(encoded)}
	}
	batch, err := s.writeSemanticVectorPayloadBytes(ctx, sourceKey, profile.ModelID(), profile.VectorSpaceID(), profile.EmbeddingDim(), raw)
	if err != nil {
		return semanticVectorPayloadBatch{}, err
	}
	for index := range assets {
		assets[index].VectorPayload.BatchID = batch.ID
	}
	return batch, nil
}

func (s *CatalogStore) appendSemanticVectorPayloadBlob(ctx context.Context, sourceKey string, modelID string, vectorSpaceID string, embeddingDim int, raw []byte) (semanticVectorPayloadRef, error) {
	s.semanticVectorPayloadMu.Lock()
	defer s.semanticVectorPayloadMu.Unlock()
	batch, err := s.writeSemanticVectorPayloadBytes(ctx, sourceKey, modelID, vectorSpaceID, embeddingDim, raw)
	if err != nil {
		return semanticVectorPayloadRef{}, err
	}
	if err := s.registerSemanticVectorPayloadBatch(ctx, nil, batch); err != nil {
		return semanticVectorPayloadRef{}, err
	}
	return semanticVectorPayloadRef{BatchID: batch.ID, Offset: 0, Length: len(raw)}, nil
}

func (s *CatalogStore) writeSemanticVectorPayloadBytes(ctx context.Context, sourceKey string, modelID string, vectorSpaceID string, embeddingDim int, raw []byte) (semanticVectorPayloadBatch, error) {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return semanticVectorPayloadBatch{}, ErrCatalogNotConfigured
	}
	if err := ctx.Err(); err != nil {
		return semanticVectorPayloadBatch{}, err
	}
	if embeddingDim <= 0 || len(raw) == 0 || len(raw)%(embeddingDim*4) != 0 {
		return semanticVectorPayloadBatch{}, fmt.Errorf("semantic vector payload bytes = %d for dim %d", len(raw), embeddingDim)
	}
	contentSum := sha256.Sum256(raw)
	contentSHA := hex.EncodeToString(contentSum[:])
	identityHasher := sha256.New()
	_, _ = io.WriteString(identityHasher, strings.TrimSpace(sourceKey))
	_, _ = identityHasher.Write([]byte{0})
	_, _ = io.WriteString(identityHasher, strings.TrimSpace(modelID))
	_, _ = identityHasher.Write([]byte{0})
	_, _ = io.WriteString(identityHasher, strings.TrimSpace(vectorSpaceID))
	_, _ = identityHasher.Write([]byte{0})
	_, _ = io.WriteString(identityHasher, fmt.Sprintf("%d", embeddingDim))
	_, _ = identityHasher.Write([]byte{0})
	_, _ = identityHasher.Write(raw)
	batchID := hex.EncodeToString(identityHasher.Sum(nil))
	root := filepath.Join(s.root, semanticVectorPayloadDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return semanticVectorPayloadBatch{}, fmt.Errorf("create semantic vector payload directory: %w", err)
	}
	path := s.semanticVectorPayloadPath(batchID)
	if info, err := os.Stat(path); err == nil {
		if info.Size() != int64(len(raw)) {
			return semanticVectorPayloadBatch{}, fmt.Errorf("semantic vector payload collision: size=%d want=%d", info.Size(), len(raw))
		}
		existingSHA, err := semanticVectorPayloadFileSHA256(ctx, path)
		if err != nil {
			return semanticVectorPayloadBatch{}, fmt.Errorf("verify existing semantic vector payload: %w", err)
		}
		if existingSHA != contentSHA {
			return semanticVectorPayloadBatch{}, fmt.Errorf("semantic vector payload collision: sha256=%s want=%s", existingSHA, contentSHA)
		}
		return semanticVectorPayloadBatch{
			ID:           batchID,
			RelativePath: filepath.Base(path),
			SizeBytes:    int64(len(raw)),
			SHA256:       contentSHA,
			CreatedAt:    time.Now().UTC(),
		}, nil
	} else if !os.IsNotExist(err) {
		return semanticVectorPayloadBatch{}, fmt.Errorf("inspect semantic vector payload: %w", err)
	}
	temp, err := os.CreateTemp(root, ".payload-*")
	if err != nil {
		return semanticVectorPayloadBatch{}, fmt.Errorf("create semantic vector payload temp file: %w", err)
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		_ = temp.Close()
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return semanticVectorPayloadBatch{}, fmt.Errorf("chmod semantic vector payload: %w", err)
	}
	if _, err := temp.Write(raw); err != nil {
		return semanticVectorPayloadBatch{}, fmt.Errorf("write semantic vector payload: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return semanticVectorPayloadBatch{}, fmt.Errorf("sync semantic vector payload: %w", err)
	}
	if err := temp.Close(); err != nil {
		return semanticVectorPayloadBatch{}, fmt.Errorf("close semantic vector payload: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return semanticVectorPayloadBatch{}, fmt.Errorf("activate semantic vector payload: %w", err)
	}
	cleanup = false
	if err := syncSemanticVectorPayloadDirectory(root); err != nil {
		return semanticVectorPayloadBatch{}, fmt.Errorf("sync semantic vector payload directory: %w", err)
	}
	return semanticVectorPayloadBatch{
		ID:           batchID,
		RelativePath: filepath.Base(path),
		SizeBytes:    int64(len(raw)),
		SHA256:       contentSHA,
		CreatedAt:    time.Now().UTC(),
	}, nil
}

func syncSemanticVectorPayloadDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func semanticVectorPayloadFileSHA256(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	bufferRef := semanticVectorPayloadHashBufferPool.Get().(*[]byte)
	buffer := *bufferRef
	defer semanticVectorPayloadHashBufferPool.Put(bufferRef)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			_, _ = hasher.Write(buffer[:count])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

type semanticVectorPayloadBatchExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *CatalogStore) registerSemanticVectorPayloadBatch(ctx context.Context, tx semanticVectorPayloadBatchExecer, batch semanticVectorPayloadBatch) error {
	if batch.ID == "" || batch.RelativePath == "" || batch.SizeBytes <= 0 || !validSHA256Hex(batch.SHA256) {
		return fmt.Errorf("semantic vector payload batch is invalid")
	}
	execer := semanticVectorPayloadBatchExecer(s.db)
	if tx != nil {
		execer = tx
	}
	_, err := execer.ExecContext(ctx, `INSERT INTO semantic_vector_payload_batches (
		batch_id, relative_path, size_bytes, sha256, created_at
	) VALUES (?, ?, ?, ?, ?)
	ON CONFLICT(batch_id) DO UPDATE SET
		relative_path = excluded.relative_path,
		size_bytes = excluded.size_bytes,
		sha256 = excluded.sha256`,
		batch.ID, batch.RelativePath, batch.SizeBytes, batch.SHA256, formatCatalogTime(batch.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("register semantic vector payload batch: %w", err)
	}
	return nil
}

func (s *CatalogStore) readSemanticVectorPayload(ctx context.Context, batchID string, embeddingDim int, offset int64, length int) ([]float32, error) {
	raw, err := s.readSemanticVectorPayloadBytes(ctx, batchID, embeddingDim, offset, length)
	if err != nil {
		return nil, err
	}
	return decodeSemanticVector(raw, embeddingDim)
}

func (s *CatalogStore) readSemanticVectorPayloadBytes(ctx context.Context, batchID string, embeddingDim int, offset int64, length int) ([]byte, error) {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return nil, ErrCatalogNotConfigured
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	expectedLength := embeddingDim * 4
	batchID = strings.ToLower(strings.TrimSpace(batchID))
	if !validSHA256Hex(batchID) || embeddingDim <= 0 || offset < 0 || length != expectedLength {
		return nil, fmt.Errorf("semantic vector payload ref invalid: batch=%q dim=%d offset=%d length=%d want_length=%d", batchID, embeddingDim, offset, length, expectedLength)
	}
	rawBatch, err := s.loadSemanticVectorPayloadBatch(ctx, batchID, false)
	if err != nil {
		return nil, err
	}
	end := offset + int64(length)
	if end < offset || end > int64(len(rawBatch)) {
		return nil, fmt.Errorf("read semantic vector payload: offset=%d length=%d size=%d", offset, length, len(rawBatch))
	}
	return rawBatch[offset:end], nil
}

func (s *CatalogStore) loadSemanticVectorPayloadBatch(ctx context.Context, batchID string, forceVerify bool) ([]byte, error) {
	batchID = strings.ToLower(strings.TrimSpace(batchID))
	if !forceVerify {
		s.semanticVectorPayloadCacheMu.Lock()
		if raw := s.semanticVectorPayloadCache[batchID]; raw != nil {
			s.semanticVectorPayloadCacheMu.Unlock()
			return raw, nil
		}
		s.semanticVectorPayloadCacheMu.Unlock()
	}
	var relativePath string
	var sizeBytes int64
	var expectedSHA string
	if err := s.queryDB().QueryRowContext(ctx, `SELECT relative_path, size_bytes, sha256
		FROM semantic_vector_payload_batches WHERE batch_id = ?`, batchID).Scan(&relativePath, &sizeBytes, &expectedSHA); err != nil {
		return nil, fmt.Errorf("read semantic vector payload metadata: %w", err)
	}
	path := filepath.Join(s.root, semanticVectorPayloadDirName, filepath.Base(relativePath))
	info, err := os.Stat(path)
	if err != nil {
		if !forceVerify && errors.Is(err, os.ErrNotExist) {
			_ = s.invalidateSemanticVectorPayloadBatch(ctx, batchID, "semantic vector payload unreadable", path)
		}
		return nil, fmt.Errorf("stat semantic vector payload: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != sizeBytes {
		if !forceVerify {
			_ = s.invalidateSemanticVectorPayloadBatch(ctx, batchID, "semantic vector payload missing or truncated", path)
		}
		return nil, fmt.Errorf("semantic vector payload size=%d want=%d", info.Size(), sizeBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read semantic vector payload: %w", err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != strings.ToLower(strings.TrimSpace(expectedSHA)) {
		if !forceVerify {
			_ = s.invalidateSemanticVectorPayloadBatch(ctx, batchID, "semantic vector payload checksum mismatch", path)
		}
		return nil, errors.New("semantic vector payload checksum mismatch")
	}
	if forceVerify {
		return raw, nil
	}
	s.semanticVectorPayloadCacheMu.Lock()
	if existing := s.semanticVectorPayloadCache[batchID]; existing != nil {
		raw = existing
	} else {
		if s.semanticVectorPayloadCache == nil {
			s.semanticVectorPayloadCache = make(map[string][]byte)
		}
		if len(s.semanticVectorPayloadOrder) >= semanticVectorPayloadCacheLimit {
			oldest := s.semanticVectorPayloadOrder[0]
			delete(s.semanticVectorPayloadCache, oldest)
			s.semanticVectorPayloadOrder = s.semanticVectorPayloadOrder[1:]
		}
		s.semanticVectorPayloadCache[batchID] = raw
		s.semanticVectorPayloadOrder = append(s.semanticVectorPayloadOrder, batchID)
	}
	s.semanticVectorPayloadCacheMu.Unlock()
	return raw, nil
}

func (s *CatalogStore) invalidateSemanticVectorPayloadBatch(ctx context.Context, batchID string, failure string, path string) error {
	s.semanticVectorPayloadMu.Lock()
	defer s.semanticVectorPayloadMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	nowText := formatCatalogTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `UPDATE semantic_vectors
		SET payload_batch_id = NULL, vector_offset = 0, vector_length = 0,
			status = 'pending', last_error = ?, generated_at = ?
		WHERE payload_batch_id = ? AND status = 'ready'`, failure, nowText, batchID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM semantic_vector_payload_batches WHERE batch_id = ?`, batchID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.dropSemanticVectorPayloadCache(batchID)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *CatalogStore) reconcileSemanticVectorPayloads(ctx context.Context) error {
	if s == nil || s.db == nil {
		return ErrCatalogNotConfigured
	}
	s.semanticVectorPayloadMu.Lock()
	defer s.semanticVectorPayloadMu.Unlock()
	if err := cleanupSemanticVectorPayloadTemps(filepath.Join(s.root, semanticVectorPayloadDirName)); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT batch_id, relative_path, size_bytes, sha256 FROM semantic_vector_payload_batches`)
	if err != nil {
		return fmt.Errorf("query semantic vector payload batches: %w", err)
	}
	type payload struct {
		id       string
		relative string
		size     int64
		sha256   string
	}
	payloads := []payload{}
	for rows.Next() {
		var item payload
		if err := rows.Scan(&item.id, &item.relative, &item.size, &item.sha256); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan semantic vector payload batch: %w", err)
		}
		payloads = append(payloads, item)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close semantic vector payload batches: %w", err)
	}
	for _, item := range payloads {
		path := filepath.Join(s.root, semanticVectorPayloadDirName, filepath.Base(item.relative))
		info, statErr := os.Stat(path)
		failure := "semantic vector payload missing or truncated"
		if statErr == nil && info.Mode().IsRegular() && info.Size() == item.size {
			// Full SHA-256 verification is lazy on first use and forced by deep
			// consistency checks. Startup stays proportional to metadata, not
			// the full vector corpus.
			continue
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect semantic vector payload %q: %w", item.id, statErr)
		}
		tx, beginErr := s.db.BeginTx(ctx, nil)
		if beginErr != nil {
			return fmt.Errorf("begin semantic vector payload repair: %w", beginErr)
		}
		nowText := formatCatalogTime(time.Now().UTC())
		if _, updateErr := tx.ExecContext(ctx, `UPDATE semantic_vectors
			SET payload_batch_id = NULL, vector_offset = 0, vector_length = 0,
				status = 'pending', last_error = ?, generated_at = ?
			WHERE payload_batch_id = ? AND status = 'ready'`, failure, nowText, item.id); updateErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("reset unreadable semantic vector payload rows: %w", updateErr)
		}
		if _, deleteErr := tx.ExecContext(ctx, `DELETE FROM semantic_vector_payload_batches WHERE batch_id = ?`, item.id); deleteErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("delete unreadable semantic vector payload batch: %w", deleteErr)
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("commit semantic vector payload repair: %w", commitErr)
		}
		s.dropSemanticVectorPayloadCache(item.id)
		_ = os.Remove(path)
	}
	if err := s.cleanupUnreferencedSemanticVectorPayloadsLocked(ctx); err != nil {
		return err
	}
	return s.cleanupSemanticVectorPayloadCandidatesLocked(ctx)
}

func cleanupSemanticVectorPayloadTemps(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("list semantic vector payload temps: %w", err)
	}
	changed := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".payload-") {
			continue
		}
		if err := os.Remove(filepath.Join(root, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale semantic vector payload temp: %w", err)
		}
		changed = true
	}
	if changed {
		return syncSemanticVectorPayloadDirectory(root)
	}
	return nil
}

func (s *CatalogStore) cleanupUnreferencedSemanticVectorPayloads(ctx context.Context) error {
	s.semanticVectorPayloadMu.Lock()
	defer s.semanticVectorPayloadMu.Unlock()
	return s.cleanupUnreferencedSemanticVectorPayloadsLocked(ctx)
}

func queueSemanticVectorPayloadGCInTx(ctx context.Context, tx *sql.Tx, sourceKey string, upstreamAssetID string, modelID string, replacementBatchID string) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO semantic_vector_payload_gc_candidates(batch_id)
		SELECT payload_batch_id
		FROM semantic_vectors
		WHERE source_key = ?
			AND upstream_asset_id = ?
			AND model_id = ?
			AND payload_batch_id IS NOT NULL
			AND payload_batch_id IS NOT ?`, sourceKey, upstreamAssetID, modelID, nullableCatalogText(replacementBatchID))
	if err != nil {
		return fmt.Errorf("queue semantic vector payload GC candidate: %w", err)
	}
	return nil
}

func (s *CatalogStore) cleanupSemanticVectorPayloadCandidates(ctx context.Context) error {
	s.semanticVectorPayloadMu.Lock()
	defer s.semanticVectorPayloadMu.Unlock()
	return s.cleanupSemanticVectorPayloadCandidatesLocked(ctx)
}

func (s *CatalogStore) cleanupSemanticVectorPayloadCandidatesLocked(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT candidate.batch_id, COALESCE(batch.relative_path, '')
		FROM semantic_vector_payload_gc_candidates candidate
		LEFT JOIN semantic_vector_payload_batches batch ON batch.batch_id = candidate.batch_id
		ORDER BY candidate.batch_id`)
	if err != nil {
		return fmt.Errorf("query semantic vector payload GC candidates: %w", err)
	}
	type candidate struct{ id, relative string }
	candidates := []candidate{}
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.relative); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan semantic vector payload GC candidate: %w", err)
		}
		candidates = append(candidates, item)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close semantic vector payload GC candidates: %w", err)
	}
	root := filepath.Join(s.root, semanticVectorPayloadDirName)
	removedFile := false
	for _, item := range candidates {
		result, err := s.db.ExecContext(ctx, `DELETE FROM semantic_vector_payload_batches
			WHERE batch_id = ?
				AND NOT EXISTS (SELECT 1 FROM semantic_vectors WHERE payload_batch_id = ?)`, item.id, item.id)
		if err != nil {
			return fmt.Errorf("delete semantic vector payload GC batch: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("inspect semantic vector payload GC batch: %w", err)
		}
		if deleted == 1 && item.relative != "" {
			s.dropSemanticVectorPayloadCache(item.id)
			if err := os.Remove(filepath.Join(root, filepath.Base(item.relative))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove semantic vector payload GC file: %w", err)
			}
			removedFile = true
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM semantic_vector_payload_gc_candidates WHERE batch_id = ?`, item.id); err != nil {
			return fmt.Errorf("clear semantic vector payload GC candidate: %w", err)
		}
	}
	if removedFile {
		return syncSemanticVectorPayloadDirectory(root)
	}
	return nil
}

func (s *CatalogStore) cleanupUnreferencedSemanticVectorPayloadsLocked(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT b.batch_id, b.relative_path
		FROM semantic_vector_payload_batches b
		LEFT JOIN semantic_vectors v ON v.payload_batch_id = b.batch_id
		WHERE v.payload_batch_id IS NULL`)
	if err != nil {
		return fmt.Errorf("query unreferenced semantic vector payloads: %w", err)
	}
	type orphan struct{ id, relative string }
	orphans := []orphan{}
	for rows.Next() {
		var item orphan
		if err := rows.Scan(&item.id, &item.relative); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan unreferenced semantic vector payload: %w", err)
		}
		orphans = append(orphans, item)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close unreferenced semantic vector payloads: %w", err)
	}
	for _, item := range orphans {
		result, err := s.db.ExecContext(ctx, `DELETE FROM semantic_vector_payload_batches WHERE batch_id = ?
			AND NOT EXISTS (SELECT 1 FROM semantic_vectors WHERE payload_batch_id = ?)`, item.id, item.id)
		if err != nil {
			return fmt.Errorf("delete unreferenced semantic vector payload batch: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("inspect deleted semantic vector payload batch: %w", err)
		}
		if deleted == 1 {
			s.dropSemanticVectorPayloadCache(item.id)
			_ = os.Remove(filepath.Join(s.root, semanticVectorPayloadDirName, filepath.Base(item.relative)))
		}
	}
	root := filepath.Join(s.root, semanticVectorPayloadDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("list semantic vector payload directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != semanticVectorPayloadExt {
			continue
		}
		batchID := strings.TrimSuffix(entry.Name(), semanticVectorPayloadExt)
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM semantic_vector_payload_batches WHERE batch_id = ?`, batchID).Scan(&exists); err != nil {
			return fmt.Errorf("inspect semantic vector payload ownership: %w", err)
		}
		if exists == 0 {
			_ = os.Remove(filepath.Join(root, entry.Name()))
		}
	}
	return syncSemanticVectorPayloadDirectory(root)
}

func (s *CatalogStore) dropSemanticVectorPayloadCache(batchID string) {
	s.semanticVectorPayloadCacheMu.Lock()
	defer s.semanticVectorPayloadCacheMu.Unlock()
	delete(s.semanticVectorPayloadCache, batchID)
	for index, cachedID := range s.semanticVectorPayloadOrder {
		if cachedID == batchID {
			s.semanticVectorPayloadOrder = append(s.semanticVectorPayloadOrder[:index], s.semanticVectorPayloadOrder[index+1:]...)
			break
		}
	}
}
