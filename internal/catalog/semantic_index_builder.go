package catalog

import (
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	modernsqlite "modernc.org/sqlite"
)

const (
	semanticIndexBuilderVectorCacheLimit = 4096
	semanticIndexBuilderTransactionSize  = 128
	semanticIndexBuilderFormatVersion    = "2"
)

type semanticIndexBuilderQueryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type semanticIndexBuilder struct {
	store           *CatalogStore
	db              *sql.DB
	path            string
	sourceKey       string
	profile         semanticEmbeddingProfile
	assetGeneration int64
	checkpoint      func(context.Context) error
	vectorCache     map[int][]float32
	vectorOrder     []int
}

type semanticIndexBuilderNode struct {
	Ordinal  int
	AssetID  string
	MaxLevel int
}

type semanticIndexBuilderEdge struct {
	Neighbor   int
	Level      int
	Rank       int
	Similarity float32
}

type semanticIndexBuilderScored struct {
	Ordinal    int
	Similarity float32
}

type semanticIndexBuilderMaxHeap []semanticIndexBuilderScored

func (h semanticIndexBuilderMaxHeap) Len() int { return len(h) }
func (h semanticIndexBuilderMaxHeap) Less(i, j int) bool {
	if h[i].Similarity == h[j].Similarity {
		return h[i].Ordinal < h[j].Ordinal
	}
	return h[i].Similarity > h[j].Similarity
}
func (h semanticIndexBuilderMaxHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *semanticIndexBuilderMaxHeap) Push(value any) {
	*h = append(*h, value.(semanticIndexBuilderScored))
}
func (h *semanticIndexBuilderMaxHeap) Pop() any {
	old := *h
	item := old[len(old)-1]
	*h = old[:len(old)-1]
	return item
}

func (s *CatalogStore) prepareSemanticIndexBuilder(ctx context.Context, job *semanticIndexJob, profile semanticEmbeddingProfile) (*semanticIndexBuilder, error) {
	if s == nil || job == nil || profile == nil {
		return nil, ErrCatalogNotConfigured
	}
	root := filepath.Join(s.root, semanticBinaryIndexDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create semantic index builder directory: %w", err)
	}
	base := s.semanticBinaryIndexBaseName(job.SourceKey, profile)
	path := filepath.Join(root, fmt.Sprintf("%s.g%d.build.db", base, job.AssetGeneration))
	cleanupStaleSemanticIndexBuilders(root, base, filepath.Base(path))
	if err := cleanupSemanticIndexCrashTemps(root); err != nil {
		return nil, err
	}
	builder, err := openSemanticIndexBuilder(s, path, job.SourceKey, profile, job.AssetGeneration, s.semanticIndexLeaseCheckpoint(job))
	if err != nil {
		if !semanticIndexBuilderCorruption(err) {
			return nil, err
		}
		removeSemanticIndexBuilderFiles(path)
		return createPopulatedSemanticIndexBuilder(ctx, s, path, job, profile)
	}
	healthy, err := builder.quickCheck(ctx)
	if err != nil || !healthy {
		_ = builder.Close()
		if err != nil && !semanticIndexBuilderCorruption(err) {
			return nil, err
		}
		removeSemanticIndexBuilderFiles(path)
		return createPopulatedSemanticIndexBuilder(ctx, s, path, job, profile)
	}
	valid, err := builder.identityMatches(ctx)
	if err != nil {
		_ = builder.Close()
		if !semanticIndexBuilderCorruption(err) {
			return nil, err
		}
		removeSemanticIndexBuilderFiles(path)
		return createPopulatedSemanticIndexBuilder(ctx, s, path, job, profile)
	}
	if valid {
		return builder, nil
	}
	_ = builder.Close()
	removeSemanticIndexBuilderFiles(path)
	return createPopulatedSemanticIndexBuilder(ctx, s, path, job, profile)
}

func createPopulatedSemanticIndexBuilder(ctx context.Context, store *CatalogStore, path string, job *semanticIndexJob, profile semanticEmbeddingProfile) (*semanticIndexBuilder, error) {
	builder, err := openSemanticIndexBuilder(store, path, job.SourceKey, profile, job.AssetGeneration, store.semanticIndexLeaseCheckpoint(job))
	if err != nil {
		return nil, err
	}
	if err := builder.populate(ctx); err != nil {
		_ = builder.Close()
		return nil, err
	}
	return builder, nil
}

func (b *semanticIndexBuilder) quickCheck(ctx context.Context) (bool, error) {
	var result string
	if err := b.db.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&result); err != nil {
		return false, fmt.Errorf("quick-check semantic index builder: %w", err)
	}
	return strings.TrimSpace(result) == "ok", nil
}

func semanticIndexBuilderCorruption(err error) bool {
	var sqliteErr *modernsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case 11, 26: // SQLITE_CORRUPT, SQLITE_NOTADB
		return true
	default:
		return false
	}
}

func openSemanticIndexBuilder(store *CatalogStore, path string, sourceKey string, profile semanticEmbeddingProfile, generation int64, checkpoint func(context.Context) error) (*semanticIndexBuilder, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open semantic index builder: %w", err)
	}
	db.SetMaxOpenConns(1)
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = FULL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS build_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS build_nodes (
			ordinal INTEGER PRIMARY KEY,
			asset_id TEXT NOT NULL UNIQUE,
			captured_at_ns INTEGER NOT NULL,
			max_level INTEGER NOT NULL,
			payload_batch_id TEXT NOT NULL,
			vector_offset INTEGER NOT NULL,
			vector_length INTEGER NOT NULL,
			metadata BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS build_edges (
			source_ordinal INTEGER NOT NULL,
			level INTEGER NOT NULL,
			neighbor_ordinal INTEGER NOT NULL,
			rank INTEGER NOT NULL,
			similarity REAL NOT NULL,
			PRIMARY KEY(source_ordinal, level, neighbor_ordinal)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_build_edges_source_rank ON build_edges(source_ordinal, level DESC, rank)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialize semantic index builder: %w", err)
		}
	}
	return &semanticIndexBuilder{
		store:           store,
		db:              db,
		path:            path,
		sourceKey:       strings.TrimSpace(sourceKey),
		profile:         profile,
		assetGeneration: generation,
		checkpoint:      checkpoint,
		vectorCache:     make(map[int][]float32),
	}, nil
}

func (b *semanticIndexBuilder) Close() error {
	if b == nil || b.db == nil {
		return nil
	}
	err := b.db.Close()
	b.db = nil
	return err
}

func (b *semanticIndexBuilder) Remove() {
	if b == nil {
		return
	}
	_ = b.Close()
	removeSemanticIndexBuilderFiles(b.path)
}

func removeSemanticIndexBuilderFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

func cleanupStaleSemanticIndexBuilders(root string, base string, keepName string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	prefix := base + ".g"
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == keepName || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".build.db") {
			continue
		}
		removeSemanticIndexBuilderFiles(filepath.Join(root, name))
	}
}

func cleanupSemanticIndexCrashTemps(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("list semantic index crash temps: %w", err)
	}
	changed := false
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.Contains(name, ".tidx.") || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove semantic index crash temp: %w", err)
		}
		changed = true
	}
	if changed {
		return syncDirectory(root)
	}
	return nil
}

func (b *semanticIndexBuilder) identityMatches(ctx context.Context) (bool, error) {
	values := map[string]string{}
	rows, err := b.db.QueryContext(ctx, `SELECT key, value FROM build_meta`)
	if err != nil {
		return false, fmt.Errorf("read semantic index builder identity: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return false, err
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return values["source_key"] == b.sourceKey &&
		values["builder_version"] == semanticIndexBuilderFormatVersion &&
		values["model_id"] == b.profile.ModelID() &&
		values["vector_space_id"] == b.profile.VectorSpaceID() &&
		values["embedding_dim"] == strconv.Itoa(b.profile.EmbeddingDim()) &&
		values["asset_generation"] == strconv.FormatInt(b.assetGeneration, 10) &&
		values["phase"] != "preparing", nil
}

func (b *semanticIndexBuilder) populate(ctx context.Context) error {
	if err := b.setMeta(ctx, "phase", "preparing"); err != nil {
		return err
	}
	where, args := semanticCatalogEligibilityWhere(b.sourceKey, b.profile.InputKind(), "a")
	queryArgs := append(append([]any{}, args...), b.profile.ModelID(), b.profile.VectorSpaceID())
	rows, err := b.store.queryDB().QueryContext(ctx, `SELECT
			a.upstream_asset_id, a.media_type, a.filename, a.captured_at, a.duration,
			v.embedding_input, v.payload_batch_id, v.vector_offset, v.vector_length
		FROM semantic_vectors v
		JOIN catalog_assets a ON a.source_key = v.source_key AND a.upstream_asset_id = v.upstream_asset_id
		`+where+`
			AND v.model_id = ? AND v.vector_space_id = ? AND v.status = 'ready'
		ORDER BY a.captured_at DESC, a.source_key ASC, a.upstream_asset_id ASC`, queryArgs...)
	if err != nil {
		return fmt.Errorf("query semantic index builder nodes: %w", err)
	}
	defer rows.Close()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin semantic index builder population: %w", err)
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO build_nodes (
		ordinal, asset_id, captured_at_ns, max_level, payload_batch_id, vector_offset, vector_length, metadata
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	ordinal := 0
	for rows.Next() {
		if ordinal%512 == 0 {
			if err := b.check(ctx); err != nil {
				_ = statement.Close()
				_ = tx.Rollback()
				return err
			}
		}
		var asset semanticAsset
		var capturedAtText string
		var duration sql.NullString
		var batchID string
		var offset int64
		var length int
		if err := rows.Scan(&asset.ID, &asset.MediaType, &asset.Filename, &capturedAtText, &duration, &asset.VectorInput, &batchID, &offset, &length); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			return fmt.Errorf("scan semantic index builder node: %w", err)
		}
		capturedAt, err := time.Parse(time.RFC3339Nano, capturedAtText)
		if err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			return fmt.Errorf("parse semantic index builder captured_at: %w", err)
		}
		asset.SourceKey = b.sourceKey
		asset.CapturedAt = capturedAt.UTC()
		asset.MaxLevel = semanticHNSWLevel(asset.ID)
		if duration.Valid {
			value := duration.String
			asset.Duration = &value
		}
		if _, err := statement.ExecContext(ctx, ordinal, asset.ID, asset.CapturedAt.UnixNano(), asset.MaxLevel, batchID, offset, length, encodeSemanticBinaryMetadata(asset)); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			return fmt.Errorf("insert semantic index builder node %q: %w", asset.ID, err)
		}
		ordinal++
	}
	if err := rows.Err(); err != nil {
		_ = statement.Close()
		_ = tx.Rollback()
		return err
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	meta := map[string]string{
		"builder_version":    semanticIndexBuilderFormatVersion,
		"source_key":         b.sourceKey,
		"model_id":           b.profile.ModelID(),
		"vector_space_id":    b.profile.VectorSpaceID(),
		"embedding_dim":      strconv.Itoa(b.profile.EmbeddingDim()),
		"asset_generation":   strconv.FormatInt(b.assetGeneration, 10),
		"node_count":         strconv.Itoa(ordinal),
		"next_ordinal":       "0",
		"chain_next_ordinal": "0",
		"entry_ordinal":      "-1",
		"entry_level":        "-1",
		"phase":              "building",
	}
	for key, value := range meta {
		if _, err := tx.ExecContext(ctx, `INSERT INTO build_meta(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit semantic index builder population: %w", err)
	}
	return nil
}

func (b *semanticIndexBuilder) Build(ctx context.Context) error {
	nodeCount, err := b.metaInt(ctx, b.db, "node_count")
	if err != nil {
		return err
	}
	next, err := b.metaInt(ctx, b.db, "next_ordinal")
	if err != nil {
		return err
	}
	for next < nodeCount {
		if err := b.check(ctx); err != nil {
			return err
		}
		end := min(next+semanticIndexBuilderTransactionSize, nodeCount)
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin semantic index builder chunk: %w", err)
		}
		for ordinal := next; ordinal < end; ordinal++ {
			if err := b.check(ctx); err != nil {
				_ = tx.Rollback()
				return err
			}
			if err := b.insert(ctx, tx, ordinal); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit semantic index builder chunk: %w", err)
		}
		next = end
	}
	phase, err := b.meta(ctx, b.db, "phase")
	if err != nil {
		return err
	}
	if phase != "ready" {
		if err := b.ensureLevelZeroChain(ctx, nodeCount); err != nil {
			return err
		}
		if err := b.setMeta(ctx, "phase", "ready"); err != nil {
			return err
		}
	}
	return nil
}

func (b *semanticIndexBuilder) insert(ctx context.Context, queryer semanticIndexBuilderQueryer, ordinal int) error {
	node, err := b.node(ctx, queryer, ordinal)
	if err != nil {
		return err
	}
	entryOrdinal, err := b.metaInt(ctx, queryer, "entry_ordinal")
	if err != nil {
		return err
	}
	entryLevel, err := b.metaInt(ctx, queryer, "entry_level")
	if err != nil {
		return err
	}
	if entryOrdinal < 0 {
		return b.recordInsertion(ctx, queryer, ordinal, node.MaxLevel, ordinal+1)
	}
	query, err := b.vector(ctx, queryer, ordinal)
	if err != nil {
		return err
	}
	current := entryOrdinal
	for level := entryLevel; level > node.MaxLevel; level-- {
		current, err = b.greedyNearest(ctx, queryer, query, current, level)
		if err != nil {
			return err
		}
	}
	for level := min(node.MaxLevel, entryLevel); level >= 0; level-- {
		candidates, err := b.searchLayer(ctx, queryer, query, current, level, semanticHNSWEfConstruction)
		if err != nil {
			return err
		}
		selected := candidates
		if len(selected) > semanticHNSWMaxNeighbors {
			selected = selected[:semanticHNSWMaxNeighbors]
		}
		if err := b.replaceEdges(ctx, queryer, ordinal, level, selected); err != nil {
			return err
		}
		for rank, edge := range selected {
			if err := b.addPrunedEdge(ctx, queryer, edge.Ordinal, level, semanticIndexBuilderScored{Ordinal: ordinal, Similarity: edge.Similarity}, rank == 0); err != nil {
				return err
			}
		}
		if len(selected) > 0 {
			current = selected[0].Ordinal
		}
	}
	if node.MaxLevel > entryLevel || (node.MaxLevel == entryLevel && ordinal < entryOrdinal) {
		entryOrdinal = ordinal
		entryLevel = node.MaxLevel
	}
	return b.recordInsertion(ctx, queryer, entryOrdinal, entryLevel, ordinal+1)
}

func (b *semanticIndexBuilder) recordInsertion(ctx context.Context, queryer semanticIndexBuilderQueryer, entryOrdinal int, entryLevel int, nextOrdinal int) error {
	values := map[string]string{
		"entry_ordinal": strconv.Itoa(entryOrdinal),
		"entry_level":   strconv.Itoa(entryLevel),
		"next_ordinal":  strconv.Itoa(nextOrdinal),
	}
	for key, value := range values {
		if _, err := queryer.ExecContext(ctx, `INSERT INTO build_meta(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
			return err
		}
	}
	return nil
}

func (b *semanticIndexBuilder) greedyNearest(ctx context.Context, queryer semanticIndexBuilderQueryer, query []float32, current int, level int) (int, error) {
	vector, err := b.vector(ctx, queryer, current)
	if err != nil {
		return current, err
	}
	currentScore := semanticDot(query, vector)
	for {
		improved := false
		edges, err := b.edges(ctx, queryer, current, level)
		if err != nil {
			return current, err
		}
		for _, edge := range edges {
			neighborVector, err := b.vector(ctx, queryer, edge.Neighbor)
			if err != nil {
				return current, err
			}
			score := semanticDot(query, neighborVector)
			if score > currentScore || (score == currentScore && edge.Neighbor < current) {
				current = edge.Neighbor
				currentScore = score
				improved = true
			}
		}
		if !improved {
			return current, nil
		}
	}
}

func (b *semanticIndexBuilder) searchLayer(ctx context.Context, queryer semanticIndexBuilderQueryer, query []float32, entry int, level int, limit int) ([]semanticIndexBuilderScored, error) {
	entryVector, err := b.vector(ctx, queryer, entry)
	if err != nil {
		return nil, err
	}
	visited := map[int]struct{}{entry: {}}
	candidates := &semanticIndexBuilderMaxHeap{{Ordinal: entry, Similarity: semanticDot(query, entryVector)}}
	heap.Init(candidates)
	results := map[int]float32{}
	for candidates.Len() > 0 && len(visited) <= max(limit, semanticHNSWMaxNeighbors) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item := heap.Pop(candidates).(semanticIndexBuilderScored)
		if previous, ok := results[item.Ordinal]; !ok || item.Similarity > previous {
			results[item.Ordinal] = item.Similarity
		}
		edges, err := b.edges(ctx, queryer, item.Ordinal, level)
		if err != nil {
			return nil, err
		}
		for _, edge := range edges {
			if _, ok := visited[edge.Neighbor]; ok {
				continue
			}
			visited[edge.Neighbor] = struct{}{}
			vector, err := b.vector(ctx, queryer, edge.Neighbor)
			if err != nil {
				return nil, err
			}
			heap.Push(candidates, semanticIndexBuilderScored{Ordinal: edge.Neighbor, Similarity: semanticDot(query, vector)})
		}
	}
	scored := make([]semanticIndexBuilderScored, 0, len(results))
	for ordinal, similarity := range results {
		scored = append(scored, semanticIndexBuilderScored{Ordinal: ordinal, Similarity: similarity})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Similarity == scored[j].Similarity {
			return scored[i].Ordinal < scored[j].Ordinal
		}
		return scored[i].Similarity > scored[j].Similarity
	})
	return scored, nil
}

func (b *semanticIndexBuilder) replaceEdges(ctx context.Context, queryer semanticIndexBuilderQueryer, source int, level int, selected []semanticIndexBuilderScored) error {
	if _, err := queryer.ExecContext(ctx, `DELETE FROM build_edges WHERE source_ordinal = ? AND level = ?`, source, level); err != nil {
		return err
	}
	for rank, edge := range selected {
		if edge.Ordinal == source {
			continue
		}
		if _, err := queryer.ExecContext(ctx, `INSERT INTO build_edges(source_ordinal, level, neighbor_ordinal, rank, similarity)
			VALUES (?, ?, ?, ?, ?)`, source, level, edge.Ordinal, rank, edge.Similarity); err != nil {
			return err
		}
	}
	return nil
}

func (b *semanticIndexBuilder) addPrunedEdge(ctx context.Context, queryer semanticIndexBuilderQueryer, source int, level int, candidate semanticIndexBuilderScored, force bool) error {
	edges, err := b.edges(ctx, queryer, source, level)
	if err != nil {
		return err
	}
	scored := make([]semanticIndexBuilderScored, 0, len(edges)+1)
	found := false
	for _, edge := range edges {
		if edge.Neighbor == candidate.Ordinal {
			edge.Similarity = candidate.Similarity
			found = true
		}
		scored = append(scored, semanticIndexBuilderScored{Ordinal: edge.Neighbor, Similarity: edge.Similarity})
	}
	if !found {
		scored = append(scored, candidate)
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Similarity == scored[j].Similarity {
			return scored[i].Ordinal < scored[j].Ordinal
		}
		return scored[i].Similarity > scored[j].Similarity
	})
	if len(scored) > semanticHNSWMaxNeighbors {
		if force {
			kept := make([]semanticIndexBuilderScored, 0, semanticHNSWMaxNeighbors)
			for _, edge := range scored {
				if edge.Ordinal == candidate.Ordinal {
					continue
				}
				kept = append(kept, edge)
				if len(kept) == semanticHNSWMaxNeighbors-1 {
					break
				}
			}
			kept = append(kept, candidate)
			scored = kept
			sort.Slice(scored, func(i, j int) bool { return scored[i].Similarity > scored[j].Similarity })
		} else {
			scored = scored[:semanticHNSWMaxNeighbors]
		}
	}
	return b.replaceEdges(ctx, queryer, source, level, scored)
}

func (b *semanticIndexBuilder) ensureLevelZeroChain(ctx context.Context, nodeCount int) error {
	if nodeCount < 2 {
		return nil
	}
	next, err := b.metaInt(ctx, b.db, "chain_next_ordinal")
	if err != nil {
		return err
	}
	for next < nodeCount {
		if err := b.check(ctx); err != nil {
			return err
		}
		end := min(next+semanticIndexBuilderTransactionSize, nodeCount)
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin semantic index chain chunk: %w", err)
		}
		for ordinal := next; ordinal < end; ordinal++ {
			if err := b.check(ctx); err != nil {
				_ = tx.Rollback()
				return err
			}
			vector, err := b.vector(ctx, tx, ordinal)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			for _, neighbor := range []int{ordinal - 1, ordinal + 1} {
				if neighbor < 0 || neighbor >= nodeCount {
					continue
				}
				neighborVector, err := b.vector(ctx, tx, neighbor)
				if err != nil {
					_ = tx.Rollback()
					return err
				}
				edge := semanticIndexBuilderScored{Ordinal: neighbor, Similarity: semanticDot(vector, neighborVector)}
				if err := b.addPrunedEdge(ctx, tx, ordinal, 0, edge, true); err != nil {
					_ = tx.Rollback()
					return err
				}
			}
		}
		if err := b.setMetaWith(ctx, tx, "chain_next_ordinal", strconv.Itoa(end)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit semantic index chain chunk: %w", err)
		}
		next = end
	}
	return nil
}

func (b *semanticIndexBuilder) WriteBinary(ctx context.Context, builtAt time.Time) error {
	if err := b.Build(ctx); err != nil {
		return err
	}
	nodeCount, err := b.metaInt(ctx, b.db, "node_count")
	if err != nil {
		return err
	}
	var edgeCount int
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM build_edges`).Scan(&edgeCount); err != nil {
		return err
	}
	stride := semanticBinaryVectorStride(b.profile.EmbeddingDim())
	header := semanticBinaryIndexHeader{
		Version:            semanticBinaryIndexVersion,
		Precision:          semanticSearchIndexPrecision,
		SourceKey:          b.sourceKey,
		ModelID:            b.profile.ModelID(),
		VectorSpaceID:      b.profile.VectorSpaceID(),
		EmbeddingDim:       b.profile.EmbeddingDim(),
		IndexedVectorCount: nodeCount,
		BuiltAt:            builtAt.UTC().Format(time.RFC3339Nano),
		AssetGeneration:    b.assetGeneration,
		NodeCount:          nodeCount,
		EdgeCount:          edgeCount,
		NodeOffset:         semanticBinaryIndexHeaderBytes,
		VectorStride:       stride,
		NodeRecordSize:     semanticBinaryIndexNodeRecordSize,
		EdgeRecordSize:     semanticBinaryIndexEdgeRecordSize,
	}
	header.VectorOffset = header.NodeOffset + int64(nodeCount*semanticBinaryIndexNodeRecordSize)
	header.EdgeOffset = header.VectorOffset + int64(nodeCount*stride)
	header.StringOffset = header.EdgeOffset + int64(edgeCount*semanticBinaryIndexEdgeRecordSize)
	root := filepath.Join(b.store.root, semanticBinaryIndexDirName)
	path := b.store.semanticBinaryIndexPath(b.sourceKey, b.profile, b.assetGeneration)
	temp, err := os.CreateTemp(root, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create semantic binary index temp file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	fail := func(err error) error {
		_ = temp.Close()
		return err
	}
	if err := writeSemanticBinaryHeader(temp, header); err != nil {
		return fail(err)
	}
	nodeRows, err := b.db.QueryContext(ctx, `SELECT n.ordinal, n.captured_at_ns, n.max_level, length(n.metadata), COUNT(e.neighbor_ordinal)
		FROM build_nodes n LEFT JOIN build_edges e ON e.source_ordinal = n.ordinal
		GROUP BY n.ordinal, n.captured_at_ns, n.max_level, length(n.metadata)
		ORDER BY n.ordinal`)
	if err != nil {
		return fail(err)
	}
	nextEdgeOffset := header.EdgeOffset
	nextStringOffset := header.StringOffset
	for nodeRows.Next() {
		var ordinal, maxLevel, metadataLength, nodeEdges int
		var capturedAtNS int64
		if err := nodeRows.Scan(&ordinal, &capturedAtNS, &maxLevel, &metadataLength, &nodeEdges); err != nil {
			_ = nodeRows.Close()
			return fail(err)
		}
		if ordinal%512 == 0 {
			if err := b.check(ctx); err != nil {
				_ = nodeRows.Close()
				return fail(err)
			}
		}
		record := semanticBinaryNodeRecord{
			CapturedAtUnixNano: capturedAtNS,
			VectorOffset:       header.VectorOffset + int64(ordinal*stride),
			EdgeOffset:         nextEdgeOffset,
			StringOffset:       nextStringOffset,
			EdgeCount:          uint32(nodeEdges),
			StringLength:       uint32(metadataLength),
			MaxLevel:           uint16(max(0, maxLevel)),
		}
		if err := writeSemanticBinaryNode(temp, record); err != nil {
			_ = nodeRows.Close()
			return fail(err)
		}
		nextEdgeOffset += int64(nodeEdges * semanticBinaryIndexEdgeRecordSize)
		nextStringOffset += int64(metadataLength)
	}
	if err := nodeRows.Close(); err != nil {
		return fail(err)
	}
	if _, err := temp.Seek(header.VectorOffset, io.SeekStart); err != nil {
		return fail(err)
	}
	for ordinal := 0; ordinal < nodeCount; ordinal++ {
		if err := b.check(ctx); err != nil {
			return fail(err)
		}
		vector, err := b.vector(ctx, b.db, ordinal)
		if err != nil {
			return fail(err)
		}
		if err := writeSemanticBinaryVector(temp, vector, b.profile.EmbeddingDim()); err != nil {
			return fail(err)
		}
	}
	if _, err := temp.Seek(header.EdgeOffset, io.SeekStart); err != nil {
		return fail(err)
	}
	edgeRows, err := b.db.QueryContext(ctx, `SELECT source_ordinal, neighbor_ordinal, level, rank
		FROM build_edges ORDER BY source_ordinal, level DESC, rank, neighbor_ordinal`)
	if err != nil {
		return fail(err)
	}
	edgeIndex := 0
	for edgeRows.Next() {
		var source, neighbor, level, rank int
		if err := edgeRows.Scan(&source, &neighbor, &level, &rank); err != nil {
			_ = edgeRows.Close()
			return fail(err)
		}
		if edgeIndex%2048 == 0 {
			if err := b.check(ctx); err != nil {
				_ = edgeRows.Close()
				return fail(err)
			}
		}
		if source < 0 || neighbor < 0 || neighbor > math.MaxUint32 || level < 0 || level > math.MaxUint16 || rank < 0 || rank > math.MaxUint16 {
			_ = edgeRows.Close()
			return fail(fmt.Errorf("semantic index builder edge is out of range"))
		}
		if err := writeSemanticBinaryEdge(temp, semanticBinaryEdgeRecord{Neighbor: uint32(neighbor), Level: uint16(level), Rank: uint16(rank)}); err != nil {
			_ = edgeRows.Close()
			return fail(err)
		}
		edgeIndex++
	}
	if err := edgeRows.Close(); err != nil {
		return fail(err)
	}
	if _, err := temp.Seek(header.StringOffset, io.SeekStart); err != nil {
		return fail(err)
	}
	metadataRows, err := b.db.QueryContext(ctx, `SELECT metadata FROM build_nodes ORDER BY ordinal`)
	if err != nil {
		return fail(err)
	}
	metadataIndex := 0
	for metadataRows.Next() {
		var metadata []byte
		if err := metadataRows.Scan(&metadata); err != nil {
			_ = metadataRows.Close()
			return fail(err)
		}
		if metadataIndex%512 == 0 {
			if err := b.check(ctx); err != nil {
				_ = metadataRows.Close()
				return fail(err)
			}
		}
		if _, err := temp.Write(metadata); err != nil {
			_ = metadataRows.Close()
			return fail(err)
		}
		metadataIndex++
	}
	if err := metadataRows.Close(); err != nil {
		return fail(err)
	}
	if err := temp.Sync(); err != nil {
		return fail(err)
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish semantic binary index: %w", err)
	}
	return syncDirectory(root)
}

func (b *semanticIndexBuilder) node(ctx context.Context, queryer semanticIndexBuilderQueryer, ordinal int) (semanticIndexBuilderNode, error) {
	var node semanticIndexBuilderNode
	err := queryer.QueryRowContext(ctx, `SELECT ordinal, asset_id, max_level FROM build_nodes WHERE ordinal = ?`, ordinal).Scan(&node.Ordinal, &node.AssetID, &node.MaxLevel)
	if err != nil {
		return node, fmt.Errorf("read semantic index builder node %d: %w", ordinal, err)
	}
	return node, nil
}

func (b *semanticIndexBuilder) vector(ctx context.Context, queryer semanticIndexBuilderQueryer, ordinal int) ([]float32, error) {
	if vector, ok := b.vectorCache[ordinal]; ok {
		return vector, nil
	}
	var batchID string
	var offset int64
	var length int
	if err := queryer.QueryRowContext(ctx, `SELECT payload_batch_id, vector_offset, vector_length FROM build_nodes WHERE ordinal = ?`, ordinal).Scan(&batchID, &offset, &length); err != nil {
		return nil, fmt.Errorf("read semantic index builder vector ref %d: %w", ordinal, err)
	}
	vector, err := b.store.readSemanticVectorPayload(ctx, batchID, b.profile.EmbeddingDim(), offset, length)
	if err != nil {
		return nil, err
	}
	if len(b.vectorOrder) >= semanticIndexBuilderVectorCacheLimit {
		delete(b.vectorCache, b.vectorOrder[0])
		b.vectorOrder = b.vectorOrder[1:]
	}
	b.vectorCache[ordinal] = vector
	b.vectorOrder = append(b.vectorOrder, ordinal)
	return vector, nil
}

func (b *semanticIndexBuilder) edges(ctx context.Context, queryer semanticIndexBuilderQueryer, source int, level int) ([]semanticIndexBuilderEdge, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT neighbor_ordinal, level, rank, similarity
		FROM build_edges WHERE source_ordinal = ? AND level = ? ORDER BY rank, neighbor_ordinal`, source, level)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	edges := []semanticIndexBuilderEdge{}
	for rows.Next() {
		var edge semanticIndexBuilderEdge
		if err := rows.Scan(&edge.Neighbor, &edge.Level, &edge.Rank, &edge.Similarity); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	return edges, rows.Err()
}

func (b *semanticIndexBuilder) meta(ctx context.Context, queryer semanticIndexBuilderQueryer, key string) (string, error) {
	var value string
	if err := queryer.QueryRowContext(ctx, `SELECT value FROM build_meta WHERE key = ?`, key).Scan(&value); err != nil {
		return "", fmt.Errorf("read semantic index builder %s: %w", key, err)
	}
	return value, nil
}

func (b *semanticIndexBuilder) metaInt(ctx context.Context, queryer semanticIndexBuilderQueryer, key string) (int, error) {
	value, err := b.meta(ctx, queryer, key)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse semantic index builder %s: %w", key, err)
	}
	return parsed, nil
}

func (b *semanticIndexBuilder) setMeta(ctx context.Context, key string, value string) error {
	return b.setMetaWith(ctx, b.db, key, value)
}

func (b *semanticIndexBuilder) setMetaWith(ctx context.Context, queryer semanticIndexBuilderQueryer, key string, value string) error {
	_, err := queryer.ExecContext(ctx, `INSERT INTO build_meta(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (b *semanticIndexBuilder) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.checkpoint != nil {
		return b.checkpoint(ctx)
	}
	return nil
}
