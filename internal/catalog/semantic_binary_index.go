package catalog

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/atomicfile"
)

const (
	semanticBinaryIndexDirName        = "semantic-search-indexes"
	semanticBinaryIndexMagic          = "TIMBIDX1"
	semanticBinaryIndexVersion        = 3
	semanticBinaryIndexHeaderBytes    = 4096
	semanticBinaryIndexNodeRecordSize = 64
	semanticBinaryIndexEdgeRecordSize = 8
	semanticBinaryIndexNoDuration     = ^uint32(0)
	semanticSearchIndexPrecision      = "fp32"
)

var errSemanticBinaryIndexUnavailable = errors.New("semantic binary index unavailable")

type semanticBinaryIndexHeader struct {
	Version            int    `json:"version"`
	Precision          string `json:"precision"`
	SourceKey          string `json:"sourceKey"`
	ModelID            string `json:"modelId"`
	VectorSpaceID      string `json:"vectorSpaceId"`
	EmbeddingDim       int    `json:"embeddingDim"`
	IndexedVectorCount int    `json:"indexedVectorCount"`
	BuiltAt            string `json:"builtAt"`
	AssetGeneration    int64  `json:"assetGeneration"`
	NodeCount          int    `json:"nodeCount"`
	EdgeCount          int    `json:"edgeCount"`
	NodeOffset         int64  `json:"nodeOffset"`
	VectorOffset       int64  `json:"vectorOffset"`
	EdgeOffset         int64  `json:"edgeOffset"`
	StringOffset       int64  `json:"stringOffset"`
	VectorStride       int    `json:"vectorStride"`
	NodeRecordSize     int    `json:"nodeRecordSize"`
	EdgeRecordSize     int    `json:"edgeRecordSize"`
}

type semanticBinaryActiveManifest struct {
	Header     semanticBinaryIndexHeader `json:"header"`
	FileSize   int64                     `json:"fileSize"`
	FileSHA256 string                    `json:"fileSha256"`
}

type semanticBinaryIntegrityCacheEntry struct {
	fileInfo   os.FileInfo
	fileSHA256 string
}

type semanticBinaryNodeRecord struct {
	CapturedAtUnixNano int64
	VectorOffset       int64
	EdgeOffset         int64
	StringOffset       int64
	EdgeCount          uint32
	StringLength       uint32
	MaxLevel           uint16
}

type semanticBinaryEdgeRecord struct {
	Neighbor uint32
	Level    uint16
	Rank     uint16
}

type semanticBinaryIndexReader struct {
	file   *os.File
	header semanticBinaryIndexHeader
	nodes  []semanticBinaryNodeRecord
}

type semanticBinaryScoredOrdinal struct {
	Ordinal    uint32
	Similarity float32
}

type semanticBinarySearchSession struct {
	reader        *semanticBinaryIndexReader
	query         []float32
	vectorScratch []byte
	candidates    *semanticBinaryMaxHeap
	visited       map[uint32]struct{}
	processed     int
}

type semanticBinaryMaxHeap []semanticBinaryScoredOrdinal

func (h semanticBinaryMaxHeap) Len() int { return len(h) }

func (h semanticBinaryMaxHeap) Less(i, j int) bool {
	if h[i].Similarity == h[j].Similarity {
		return h[i].Ordinal < h[j].Ordinal
	}
	return h[i].Similarity > h[j].Similarity
}

func (h semanticBinaryMaxHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *semanticBinaryMaxHeap) Push(value any) {
	*h = append(*h, value.(semanticBinaryScoredOrdinal))
}

func (h *semanticBinaryMaxHeap) Pop() any {
	old := *h
	item := old[len(old)-1]
	*h = old[:len(old)-1]
	return item
}

type semanticIndexFileProfile struct {
	modelID       string
	vectorSpaceID string
	embeddingDim  int
}

func (p semanticIndexFileProfile) ModelID() string                           { return p.modelID }
func (p semanticIndexFileProfile) VectorSpaceID() string                     { return p.vectorSpaceID }
func (p semanticIndexFileProfile) EmbeddingDim() int                         { return p.embeddingDim }
func (p semanticIndexFileProfile) ProfileKind() string                       { return semanticProfileKindModelPack }
func (p semanticIndexFileProfile) InputKind() string                         { return semanticInputKindImage }
func (p semanticIndexFileProfile) ModelPackStatus() *SemanticModelPackStatus { return nil }
func (p semanticIndexFileProfile) EmbedSemanticAsset(context.Context, semanticAssetEmbeddingInput) (semanticEmbeddingResult, error) {
	return semanticEmbeddingResult{}, ErrSemanticModelPackInvalid
}
func (p semanticIndexFileProfile) EmbedText(context.Context, string) ([]float32, error) {
	return nil, ErrSemanticModelPackInvalid
}

func (s *CatalogStore) semanticBinaryIndexMatchesBackfillStatus(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile, status SemanticModelBackfillStatus) bool {
	if s == nil || strings.TrimSpace(s.path) == "" || profile == nil {
		return false
	}
	if err := ctx.Err(); err != nil {
		return false
	}
	semantic := semanticStatusFromBackfillStatus(status, profile)
	manifest, err := readSemanticBinaryActiveManifest(s.semanticBinaryActiveManifestPath(sourceKey, profile))
	if err != nil {
		return false
	}
	header := manifest.Header
	if err := validateSemanticBinaryIndexHeaderIdentity(header, sourceKey, profile); err != nil {
		return false
	}
	if validateSemanticBinaryIndexHeaderStatus(header, semantic) != nil {
		return false
	}
	path := s.semanticBinaryIndexPath(sourceKey, profile, header.AssetGeneration)
	return s.verifySemanticBinaryActiveFile(ctx, path, manifest, false) == nil
}

func (s *CatalogStore) semanticBinaryIndexPath(sourceKey string, profile semanticEmbeddingProfile, assetGeneration int64) string {
	root := filepath.Join(s.root, semanticBinaryIndexDirName)
	base := s.semanticBinaryIndexBaseName(sourceKey, profile)
	return filepath.Join(root, fmt.Sprintf("%s.g%d.tidx", base, assetGeneration))
}

func (s *CatalogStore) semanticBinaryActiveManifestPath(sourceKey string, profile semanticEmbeddingProfile) string {
	root := filepath.Join(s.root, semanticBinaryIndexDirName)
	return filepath.Join(root, s.semanticBinaryIndexBaseName(sourceKey, profile)+".active.json")
}

func readSemanticBinaryActiveManifest(path string) (semanticBinaryActiveManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return semanticBinaryActiveManifest{}, err
	}
	var manifest semanticBinaryActiveManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return semanticBinaryActiveManifest{}, fmt.Errorf("decode semantic binary active manifest: %w", err)
	}
	if manifest.FileSize < semanticBinaryIndexHeaderBytes || len(manifest.FileSHA256) != sha256.Size*2 {
		return semanticBinaryActiveManifest{}, errSemanticBinaryIndexUnavailable
	}
	if _, err := hex.DecodeString(manifest.FileSHA256); err != nil {
		return semanticBinaryActiveManifest{}, errSemanticBinaryIndexUnavailable
	}
	return manifest, nil
}

func (s *CatalogStore) verifySemanticBinaryActiveFile(ctx context.Context, path string, manifest semanticBinaryActiveManifest, forceDigest bool) error {
	header, fileSize, _, err := inspectSemanticBinaryIndexFile(ctx, path, false)
	if err != nil {
		s.forgetSemanticBinaryIntegrity(path)
		return err
	}
	if header != manifest.Header || fileSize != manifest.FileSize {
		s.forgetSemanticBinaryIntegrity(path)
		return errSemanticBinaryIndexUnavailable
	}
	info, err := os.Stat(path)
	if err != nil {
		s.forgetSemanticBinaryIntegrity(path)
		return err
	}
	if !forceDigest && s.semanticBinaryIntegrityMatches(path, info, manifest.FileSHA256) {
		return nil
	}
	verifiedHeader, verifiedSize, digest, err := inspectSemanticBinaryIndexFile(ctx, path, true)
	if err != nil {
		s.forgetSemanticBinaryIntegrity(path)
		return err
	}
	if verifiedHeader != manifest.Header || verifiedSize != manifest.FileSize || digest != manifest.FileSHA256 {
		s.forgetSemanticBinaryIntegrity(path)
		return errSemanticBinaryIndexUnavailable
	}
	verifiedInfo, err := os.Stat(path)
	if err != nil || !sameSemanticBinaryFileInfo(info, verifiedInfo) {
		s.forgetSemanticBinaryIntegrity(path)
		return errSemanticBinaryIndexUnavailable
	}
	s.rememberSemanticBinaryIntegrity(path, verifiedInfo, digest)
	return nil
}

func (s *CatalogStore) semanticBinaryIntegrityMatches(path string, info os.FileInfo, digest string) bool {
	if s == nil || info == nil {
		return false
	}
	s.semanticBinaryIntegrityMu.Lock()
	defer s.semanticBinaryIntegrityMu.Unlock()
	entry, ok := s.semanticBinaryIntegrity[path]
	return ok && entry.fileSHA256 == digest && sameSemanticBinaryFileInfo(entry.fileInfo, info)
}

func (s *CatalogStore) rememberSemanticBinaryIntegrity(path string, info os.FileInfo, digest string) {
	if s == nil || info == nil {
		return
	}
	s.semanticBinaryIntegrityMu.Lock()
	defer s.semanticBinaryIntegrityMu.Unlock()
	if s.semanticBinaryIntegrity == nil {
		s.semanticBinaryIntegrity = make(map[string]semanticBinaryIntegrityCacheEntry)
	}
	s.semanticBinaryIntegrity[path] = semanticBinaryIntegrityCacheEntry{fileInfo: info, fileSHA256: digest}
}

func (s *CatalogStore) forgetSemanticBinaryIntegrity(path string) {
	if s == nil {
		return
	}
	s.semanticBinaryIntegrityMu.Lock()
	defer s.semanticBinaryIntegrityMu.Unlock()
	delete(s.semanticBinaryIntegrity, path)
}

func sameSemanticBinaryFileInfo(left os.FileInfo, right os.FileInfo) bool {
	return left != nil && right != nil &&
		left.Size() == right.Size() &&
		left.ModTime().Equal(right.ModTime()) &&
		os.SameFile(left, right)
}

func (s *CatalogStore) activateSemanticBinaryIndex(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile, assetGeneration int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := s.semanticBinaryIndexPath(sourceKey, profile, assetGeneration)
	header, fileSize, digest, err := inspectSemanticBinaryIndexFile(ctx, path, true)
	if err != nil {
		return fmt.Errorf("read semantic binary generation before activation: %w", err)
	}
	if err := validateSemanticBinaryIndexHeaderIdentity(header, sourceKey, profile); err != nil {
		return err
	}
	if header.AssetGeneration != assetGeneration {
		return errSemanticBinaryIndexUnavailable
	}
	manifest := semanticBinaryActiveManifest{
		Header:     header,
		FileSize:   fileSize,
		FileSHA256: digest,
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode semantic binary active manifest: %w", err)
	}
	if err := atomicfile.WriteFile(s.semanticBinaryActiveManifestPath(sourceKey, profile), raw, 0o600); err != nil {
		return fmt.Errorf("publish semantic binary active manifest: %w", err)
	}
	if info, statErr := os.Stat(path); statErr == nil {
		s.rememberSemanticBinaryIntegrity(path, info, digest)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (s *CatalogStore) semanticBinaryIndexBaseName(sourceKey string, profile semanticEmbeddingProfile) string {
	identity := strings.Join([]string{
		strings.TrimSpace(sourceKey),
		profile.ModelID(),
		profile.VectorSpaceID(),
		fmt.Sprintf("%d", profile.EmbeddingDim()),
		semanticSearchIndexPrecision,
	}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func (s *CatalogStore) cleanupSemanticBinaryIndexGenerations(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile, publishedGeneration *int64, keepPublished bool) error {
	if s == nil || strings.TrimSpace(s.path) == "" || profile == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root := filepath.Join(s.root, semanticBinaryIndexDirName)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read semantic binary index directory: %w", err)
	}
	base := s.semanticBinaryIndexBaseName(sourceKey, profile)
	keepName := ""
	if publishedGeneration != nil && keepPublished {
		keepName = filepath.Base(s.semanticBinaryIndexPath(sourceKey, profile, *publishedGeneration))
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() || name == keepName {
			continue
		}
		if name != base+".tidx" && !(strings.HasPrefix(name, base+".g") && strings.HasSuffix(name, ".tidx")) {
			continue
		}
		if publishedGeneration != nil && name != base+".tidx" {
			rawGeneration := strings.TrimSuffix(strings.TrimPrefix(name, base+".g"), ".tidx")
			generation, parseErr := strconv.ParseInt(rawGeneration, 10, 64)
			if parseErr == nil && generation > *publishedGeneration {
				continue
			}
		}
		path := filepath.Join(root, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove semantic binary index %s: %w", path, err)
		}
		s.forgetSemanticBinaryIntegrity(path)
	}
	return nil
}

func writeSemanticBinaryHeader(writer io.Writer, header semanticBinaryIndexHeader) error {
	raw, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("encode semantic binary index header: %w", err)
	}
	if len(raw) > semanticBinaryIndexHeaderBytes-len(semanticBinaryIndexMagic)-4 {
		return fmt.Errorf("semantic binary index header too large: %d bytes", len(raw))
	}
	buffer := make([]byte, semanticBinaryIndexHeaderBytes)
	copy(buffer[:len(semanticBinaryIndexMagic)], semanticBinaryIndexMagic)
	binary.LittleEndian.PutUint32(buffer[len(semanticBinaryIndexMagic):len(semanticBinaryIndexMagic)+4], uint32(len(raw)))
	copy(buffer[len(semanticBinaryIndexMagic)+4:], raw)
	if _, err := writer.Write(buffer); err != nil {
		return fmt.Errorf("write semantic binary index header: %w", err)
	}
	return nil
}

func readSemanticBinaryIndexHeader(path string) (semanticBinaryIndexHeader, error) {
	header, _, _, err := inspectSemanticBinaryIndexFile(context.Background(), path, false)
	return header, err
}

func inspectSemanticBinaryIndexFile(ctx context.Context, path string, includeDigest bool) (semanticBinaryIndexHeader, int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return semanticBinaryIndexHeader{}, 0, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return semanticBinaryIndexHeader{}, 0, "", err
	}
	header, err := readSemanticBinaryIndexHeaderFromFile(file)
	if err != nil {
		return semanticBinaryIndexHeader{}, 0, "", err
	}
	if err := validateSemanticBinaryIndexLayout(header, info.Size()); err != nil {
		return semanticBinaryIndexHeader{}, 0, "", err
	}
	if !includeDigest {
		return header, info.Size(), "", nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return semanticBinaryIndexHeader{}, 0, "", err
	}
	hasher := sha256.New()
	buffer := make([]byte, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return semanticBinaryIndexHeader{}, 0, "", err
		}
		readCount, readErr := file.Read(buffer)
		if readCount > 0 {
			_, _ = hasher.Write(buffer[:readCount])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return semanticBinaryIndexHeader{}, 0, "", readErr
		}
	}
	return header, info.Size(), hex.EncodeToString(hasher.Sum(nil)), nil
}

func readSemanticBinaryIndexHeaderFromFile(file *os.File) (semanticBinaryIndexHeader, error) {
	headerBytes := make([]byte, semanticBinaryIndexHeaderBytes)
	if _, err := file.ReadAt(headerBytes, 0); err != nil {
		return semanticBinaryIndexHeader{}, err
	}
	if string(headerBytes[:len(semanticBinaryIndexMagic)]) != semanticBinaryIndexMagic {
		return semanticBinaryIndexHeader{}, errSemanticBinaryIndexUnavailable
	}
	headerLength := int(binary.LittleEndian.Uint32(headerBytes[len(semanticBinaryIndexMagic) : len(semanticBinaryIndexMagic)+4]))
	if headerLength <= 0 || headerLength > semanticBinaryIndexHeaderBytes-len(semanticBinaryIndexMagic)-4 {
		return semanticBinaryIndexHeader{}, fmt.Errorf("semantic binary index header length is invalid")
	}
	var header semanticBinaryIndexHeader
	if err := json.Unmarshal(headerBytes[len(semanticBinaryIndexMagic)+4:len(semanticBinaryIndexMagic)+4+headerLength], &header); err != nil {
		return semanticBinaryIndexHeader{}, err
	}
	return header, nil
}

func validateSemanticBinaryIndexLayout(header semanticBinaryIndexHeader, fileSize int64) error {
	if fileSize < semanticBinaryIndexHeaderBytes ||
		header.NodeCount < 0 || header.EdgeCount < 0 ||
		header.NodeRecordSize != semanticBinaryIndexNodeRecordSize ||
		header.EdgeRecordSize != semanticBinaryIndexEdgeRecordSize ||
		header.NodeOffset != semanticBinaryIndexHeaderBytes ||
		header.VectorStride != semanticBinaryVectorStride(header.EmbeddingDim) ||
		header.VectorStride <= 0 {
		return errSemanticBinaryIndexUnavailable
	}
	vectorOffset, ok := semanticBinaryCheckedSectionEnd(header.NodeOffset, header.NodeCount, semanticBinaryIndexNodeRecordSize)
	if !ok || header.VectorOffset != vectorOffset {
		return errSemanticBinaryIndexUnavailable
	}
	edgeOffset, ok := semanticBinaryCheckedSectionEnd(header.VectorOffset, header.NodeCount, header.VectorStride)
	if !ok || header.EdgeOffset != edgeOffset {
		return errSemanticBinaryIndexUnavailable
	}
	stringOffset, ok := semanticBinaryCheckedSectionEnd(header.EdgeOffset, header.EdgeCount, semanticBinaryIndexEdgeRecordSize)
	if !ok || header.StringOffset != stringOffset || stringOffset > fileSize {
		return errSemanticBinaryIndexUnavailable
	}
	return nil
}

func semanticBinaryCheckedSectionEnd(offset int64, count int, width int) (int64, bool) {
	if offset < 0 || count < 0 || width <= 0 {
		return 0, false
	}
	if count > 0 && int64(count) > (math.MaxInt64-offset)/int64(width) {
		return 0, false
	}
	return offset + int64(count)*int64(width), true
}

func writeSemanticBinaryNode(writer io.Writer, node semanticBinaryNodeRecord) error {
	buffer := make([]byte, semanticBinaryIndexNodeRecordSize)
	binary.LittleEndian.PutUint64(buffer[0:8], uint64(node.CapturedAtUnixNano))
	binary.LittleEndian.PutUint64(buffer[8:16], uint64(node.VectorOffset))
	binary.LittleEndian.PutUint64(buffer[16:24], uint64(node.EdgeOffset))
	binary.LittleEndian.PutUint64(buffer[24:32], uint64(node.StringOffset))
	binary.LittleEndian.PutUint32(buffer[32:36], node.EdgeCount)
	binary.LittleEndian.PutUint32(buffer[36:40], node.StringLength)
	binary.LittleEndian.PutUint16(buffer[40:42], node.MaxLevel)
	if _, err := writer.Write(buffer); err != nil {
		return fmt.Errorf("write semantic binary node: %w", err)
	}
	return nil
}

func writeSemanticBinaryEdge(writer io.Writer, edge semanticBinaryEdgeRecord) error {
	buffer := make([]byte, semanticBinaryIndexEdgeRecordSize)
	binary.LittleEndian.PutUint32(buffer[0:4], edge.Neighbor)
	binary.LittleEndian.PutUint16(buffer[4:6], edge.Level)
	binary.LittleEndian.PutUint16(buffer[6:8], edge.Rank)
	if _, err := writer.Write(buffer); err != nil {
		return fmt.Errorf("write semantic binary edge: %w", err)
	}
	return nil
}

func writeSemanticBinaryVector(writer io.Writer, vector []float32, dim int) error {
	if len(vector) != dim {
		return fmt.Errorf("got %d dimensions, want %d", len(vector), dim)
	}
	buffer := make([]byte, dim*4)
	for index, value := range vector {
		binary.LittleEndian.PutUint32(buffer[index*4:index*4+4], math.Float32bits(value))
	}
	_, err := writer.Write(buffer)
	return err
}

func encodeSemanticBinaryMetadata(asset semanticAsset) []byte {
	id := []byte(asset.ID)
	mediaType := []byte(asset.MediaType)
	filename := []byte(asset.Filename)
	durationLength := semanticBinaryIndexNoDuration
	duration := []byte(nil)
	if asset.Duration != nil {
		duration = []byte(*asset.Duration)
		durationLength = uint32(len(duration))
	}
	buffer := make([]byte, 16+len(id)+len(mediaType)+len(filename)+len(duration))
	binary.LittleEndian.PutUint32(buffer[0:4], uint32(len(id)))
	binary.LittleEndian.PutUint32(buffer[4:8], uint32(len(mediaType)))
	binary.LittleEndian.PutUint32(buffer[8:12], uint32(len(filename)))
	binary.LittleEndian.PutUint32(buffer[12:16], durationLength)
	offset := 16
	copy(buffer[offset:], id)
	offset += len(id)
	copy(buffer[offset:], mediaType)
	offset += len(mediaType)
	copy(buffer[offset:], filename)
	offset += len(filename)
	copy(buffer[offset:], duration)
	return buffer
}

func decodeSemanticBinaryMetadata(raw []byte, sourceKey string, node semanticBinaryNodeRecord) (semanticAsset, error) {
	if len(raw) < 16 {
		return semanticAsset{}, fmt.Errorf("decode semantic binary metadata: got %d bytes", len(raw))
	}
	idLength := int(binary.LittleEndian.Uint32(raw[0:4]))
	mediaTypeLength := int(binary.LittleEndian.Uint32(raw[4:8]))
	filenameLength := int(binary.LittleEndian.Uint32(raw[8:12]))
	durationLength := binary.LittleEndian.Uint32(raw[12:16])
	offset := 16
	if idLength < 0 || mediaTypeLength < 0 || filenameLength < 0 || offset+idLength+mediaTypeLength+filenameLength > len(raw) {
		return semanticAsset{}, fmt.Errorf("decode semantic binary metadata: invalid string lengths")
	}
	asset := semanticAsset{
		SourceKey:  sourceKey,
		ID:         string(raw[offset : offset+idLength]),
		CapturedAt: time.Unix(0, node.CapturedAtUnixNano).UTC(),
		MaxLevel:   int(node.MaxLevel),
	}
	offset += idLength
	asset.MediaType = string(raw[offset : offset+mediaTypeLength])
	offset += mediaTypeLength
	asset.Filename = string(raw[offset : offset+filenameLength])
	offset += filenameLength
	if durationLength != semanticBinaryIndexNoDuration {
		if offset+int(durationLength) > len(raw) {
			return semanticAsset{}, fmt.Errorf("decode semantic binary metadata: invalid duration length")
		}
		value := string(raw[offset : offset+int(durationLength)])
		asset.Duration = &value
	}
	return asset, nil
}

func (s *CatalogStore) openSemanticBinaryIndexFile(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile) (*semanticBinaryIndexReader, CatalogSemanticStatus, error) {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return nil, CatalogSemanticStatus{}, ErrCatalogNotConfigured
	}
	manifest, err := readSemanticBinaryActiveManifest(s.semanticBinaryActiveManifestPath(sourceKey, profile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, CatalogSemanticStatus{}, errSemanticBinaryIndexUnavailable
		}
		return nil, CatalogSemanticStatus{}, fmt.Errorf("read semantic binary active manifest: %w", err)
	}
	if err := validateSemanticBinaryIndexHeaderIdentity(manifest.Header, sourceKey, profile); err != nil {
		return nil, CatalogSemanticStatus{}, err
	}
	status := semanticStatusFromBinaryIndexHeader(manifest.Header, profile)
	path := s.semanticBinaryIndexPath(sourceKey, profile, manifest.Header.AssetGeneration)
	if err := s.verifySemanticBinaryActiveFile(ctx, path, manifest, false); err != nil {
		return nil, status, err
	}
	reader, err := s.openSemanticBinaryIndexFileForGeneration(ctx, sourceKey, profile, manifest.Header.AssetGeneration)
	if err != nil {
		return nil, status, err
	}
	fileInfo, err := reader.file.Stat()
	if err != nil || fileInfo.Size() != manifest.FileSize {
		_ = reader.Close()
		return nil, status, errSemanticBinaryIndexUnavailable
	}
	if reader.header != manifest.Header {
		_ = reader.Close()
		return nil, status, errSemanticBinaryIndexUnavailable
	}
	if current, statusErr := s.semanticStatusForBinarySearch(ctx, sourceKey, profile); statusErr == nil {
		current.IndexedVectorCount = manifest.Header.IndexedVectorCount
		current.IndexedGeneration = manifest.Header.AssetGeneration
		current.BuiltAt = status.BuiltAt
		if current.AssetGeneration != current.IndexedGeneration && current.Status == semanticBackfillStatusReady {
			current.Status = semanticBackfillStatusIndexing
		}
		status = current
	}
	return reader, status, nil
}

func (s *CatalogStore) openSemanticBinaryIndexFileForGeneration(ctx context.Context, sourceKey string, profile semanticEmbeddingProfile, assetGeneration int64) (*semanticBinaryIndexReader, error) {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return nil, ErrCatalogNotConfigured
	}
	file, err := os.Open(s.semanticBinaryIndexPath(sourceKey, profile, assetGeneration))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errSemanticBinaryIndexUnavailable
		}
		return nil, fmt.Errorf("open semantic binary index: %w", err)
	}
	reader := &semanticBinaryIndexReader{file: file}
	if err := reader.readHeaderAndNodes(ctx); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := validateSemanticBinaryIndexHeaderIdentity(reader.header, sourceKey, profile); err != nil {
		_ = file.Close()
		return nil, err
	}
	return reader, nil
}

func (r *semanticBinaryIndexReader) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	return r.file.Close()
}

func (r *semanticBinaryIndexReader) readHeaderAndNodes(ctx context.Context) error {
	started := time.Now()
	info, err := r.file.Stat()
	if err != nil {
		return fmt.Errorf("stat semantic binary index: %w", err)
	}
	r.header, err = readSemanticBinaryIndexHeaderFromFile(r.file)
	if err != nil {
		return fmt.Errorf("read semantic binary index header: %w", err)
	}
	if err := validateSemanticBinaryIndexLayout(r.header, info.Size()); err != nil {
		return err
	}
	rawNodes := make([]byte, r.header.NodeCount*semanticBinaryIndexNodeRecordSize)
	if _, err := r.file.ReadAt(rawNodes, r.header.NodeOffset); err != nil {
		return fmt.Errorf("read semantic binary index nodes: %w", err)
	}
	r.nodes = make([]semanticBinaryNodeRecord, r.header.NodeCount)
	for index := range r.nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		offset := index * semanticBinaryIndexNodeRecordSize
		r.nodes[index] = semanticBinaryNodeRecord{
			CapturedAtUnixNano: int64(binary.LittleEndian.Uint64(rawNodes[offset : offset+8])),
			VectorOffset:       int64(binary.LittleEndian.Uint64(rawNodes[offset+8 : offset+16])),
			EdgeOffset:         int64(binary.LittleEndian.Uint64(rawNodes[offset+16 : offset+24])),
			StringOffset:       int64(binary.LittleEndian.Uint64(rawNodes[offset+24 : offset+32])),
			EdgeCount:          binary.LittleEndian.Uint32(rawNodes[offset+32 : offset+36]),
			StringLength:       binary.LittleEndian.Uint32(rawNodes[offset+36 : offset+40]),
			MaxLevel:           binary.LittleEndian.Uint16(rawNodes[offset+40 : offset+42]),
		}
		if err := validateSemanticBinaryNodeLayout(r.header, r.nodes[index], index, info.Size()); err != nil {
			return err
		}
	}
	log.Printf(
		"timich-agent semantic binary index open loaded nodes precision=%s nodes=%d node_bytes=%d elapsed=%s",
		r.header.Precision,
		r.header.NodeCount,
		len(rawNodes),
		time.Since(started).Round(time.Millisecond),
	)
	return nil
}

func validateSemanticBinaryNodeLayout(header semanticBinaryIndexHeader, node semanticBinaryNodeRecord, ordinal int, fileSize int64) error {
	expectedVectorOffset, ok := semanticBinaryCheckedSectionEnd(header.VectorOffset, ordinal, header.VectorStride)
	if !ok || node.VectorOffset != expectedVectorOffset {
		return errSemanticBinaryIndexUnavailable
	}
	vectorEnd, ok := semanticBinaryCheckedSectionEnd(node.VectorOffset, 1, header.VectorStride)
	if !ok || vectorEnd > header.EdgeOffset {
		return errSemanticBinaryIndexUnavailable
	}
	edgeEnd, ok := semanticBinaryCheckedSectionEnd(node.EdgeOffset, int(node.EdgeCount), semanticBinaryIndexEdgeRecordSize)
	if !ok || node.EdgeOffset < header.EdgeOffset || edgeEnd > header.StringOffset {
		return errSemanticBinaryIndexUnavailable
	}
	stringEnd, ok := semanticBinaryCheckedSectionEnd(node.StringOffset, int(node.StringLength), 1)
	if !ok || node.StringOffset < header.StringOffset || stringEnd > fileSize {
		return errSemanticBinaryIndexUnavailable
	}
	return nil
}

func validateSemanticBinaryIndexHeaderIdentity(header semanticBinaryIndexHeader, sourceKey string, profile semanticEmbeddingProfile) error {
	if header.Version != semanticBinaryIndexVersion ||
		header.Precision != semanticSearchIndexPrecision ||
		header.SourceKey != strings.TrimSpace(sourceKey) ||
		header.ModelID != profile.ModelID() ||
		header.VectorSpaceID != profile.VectorSpaceID() ||
		header.EmbeddingDim != profile.EmbeddingDim() ||
		header.NodeCount != header.IndexedVectorCount {
		return errSemanticBinaryIndexUnavailable
	}
	return nil
}

func validateSemanticBinaryIndexHeaderStatus(header semanticBinaryIndexHeader, status CatalogSemanticStatus) error {
	if header.IndexedVectorCount != status.IndexedVectorCount ||
		header.NodeCount != status.IndexedVectorCount {
		return errSemanticBinaryIndexUnavailable
	}
	if header.AssetGeneration != status.IndexedGeneration {
		return errSemanticBinaryIndexUnavailable
	}
	if status.BuiltAt != nil {
		builtAt, err := time.Parse(time.RFC3339Nano, header.BuiltAt)
		if err != nil || !builtAt.Equal(status.BuiltAt.UTC()) {
			return errSemanticBinaryIndexUnavailable
		}
	}
	return nil
}

func semanticStatusFromBinaryIndexHeader(header semanticBinaryIndexHeader, profile semanticEmbeddingProfile) CatalogSemanticStatus {
	status := CatalogSemanticStatus{
		Status:               "ready",
		ModelID:              header.ModelID,
		VectorSpaceID:        header.VectorSpaceID,
		EmbeddingDim:         header.EmbeddingDim,
		CompletedVectorCount: header.IndexedVectorCount,
		IndexedVectorCount:   header.IndexedVectorCount,
		AssetGeneration:      header.AssetGeneration,
		IndexedGeneration:    header.AssetGeneration,
		ProfileKind:          profile.ProfileKind(),
		InputKind:            profile.InputKind(),
		ModelPack:            profile.ModelPackStatus(),
	}
	if builtAt, err := time.Parse(time.RFC3339Nano, header.BuiltAt); err == nil {
		status.BuiltAt = &builtAt
	}
	return status
}

func (r *semanticBinaryIndexReader) newSearchSession(ctx context.Context, query []float32) (*semanticBinarySearchSession, error) {
	if r == nil || len(r.nodes) == 0 {
		return &semanticBinarySearchSession{}, nil
	}
	entryOrdinal := uint32(0)
	for index := range r.nodes {
		if r.nodes[index].MaxLevel > r.nodes[entryOrdinal].MaxLevel {
			entryOrdinal = uint32(index)
		}
	}
	currentOrdinal := entryOrdinal
	vectorScratch := make([]byte, r.header.VectorStride)
	currentScore, err := r.dotOrdinalWithBuffer(ctx, currentOrdinal, query, vectorScratch)
	if err != nil {
		return nil, err
	}
	for level := int(r.nodes[currentOrdinal].MaxLevel); level >= 1; level-- {
		improved := true
		for improved {
			improved = false
			neighbors, err := r.neighborOrdinals(ctx, currentOrdinal, level)
			if err != nil {
				return nil, err
			}
			for _, neighborOrdinal := range neighbors {
				score, err := r.dotOrdinalWithBuffer(ctx, neighborOrdinal, query, vectorScratch)
				if err != nil {
					return nil, err
				}
				if score > currentScore {
					currentOrdinal = neighborOrdinal
					currentScore = score
					improved = true
				}
			}
		}
	}

	candidates := &semanticBinaryMaxHeap{}
	heap.Push(candidates, semanticBinaryScoredOrdinal{Ordinal: currentOrdinal, Similarity: currentScore})
	return &semanticBinarySearchSession{
		reader:        r,
		query:         append([]float32(nil), query...),
		vectorScratch: vectorScratch,
		candidates:    candidates,
		visited:       map[uint32]struct{}{currentOrdinal: {}},
	}, nil
}

func (s *semanticBinarySearchSession) advance(ctx context.Context, additionalVisits int) ([]semanticScoredAsset, bool, error) {
	if s == nil || s.reader == nil || s.candidates == nil || additionalVisits <= 0 {
		return []semanticScoredAsset{}, true, nil
	}
	remaining := min(additionalVisits, len(s.reader.nodes)-s.processed)
	results := make([]semanticScoredAsset, 0, remaining)
	for s.candidates.Len() > 0 && len(results) < remaining {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		item := heap.Pop(s.candidates).(semanticBinaryScoredOrdinal)
		s.processed++
		asset, err := s.reader.assetForOrdinal(ctx, item.Ordinal)
		if err != nil {
			return nil, false, err
		}
		results = append(results, semanticScoredAsset{Asset: asset, Similarity: item.Similarity})
		neighbors, err := s.reader.neighborOrdinals(ctx, item.Ordinal, 0)
		if err != nil {
			return nil, false, err
		}
		for _, neighborOrdinal := range neighbors {
			if _, ok := s.visited[neighborOrdinal]; ok {
				continue
			}
			s.visited[neighborOrdinal] = struct{}{}
			score, err := s.reader.dotOrdinalWithBuffer(ctx, neighborOrdinal, s.query, s.vectorScratch)
			if err != nil {
				return nil, false, err
			}
			heap.Push(s.candidates, semanticBinaryScoredOrdinal{Ordinal: neighborOrdinal, Similarity: score})
		}
	}
	return results, s.candidates.Len() == 0, nil
}

func (r *semanticBinaryIndexReader) assetForOrdinal(ctx context.Context, ordinal uint32) (semanticAsset, error) {
	if int(ordinal) >= len(r.nodes) {
		return semanticAsset{}, errSemanticBinaryIndexUnavailable
	}
	if err := ctx.Err(); err != nil {
		return semanticAsset{}, err
	}
	node := r.nodes[ordinal]
	raw := make([]byte, node.StringLength)
	if _, err := r.file.ReadAt(raw, node.StringOffset); err != nil {
		return semanticAsset{}, fmt.Errorf("read semantic binary metadata: %w", err)
	}
	asset, err := decodeSemanticBinaryMetadata(raw, r.header.SourceKey, node)
	if err != nil {
		return semanticAsset{}, err
	}
	vector, err := r.vectorForOrdinal(ctx, ordinal)
	if err != nil {
		return semanticAsset{}, err
	}
	asset.Vector = vector
	return asset, nil
}

func (r *semanticBinaryIndexReader) vectorForOrdinal(ctx context.Context, ordinal uint32) ([]float32, error) {
	if int(ordinal) >= len(r.nodes) {
		return nil, errSemanticBinaryIndexUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	node := r.nodes[ordinal]
	raw := make([]byte, r.header.VectorStride)
	if _, err := r.file.ReadAt(raw, node.VectorOffset); err != nil {
		return nil, fmt.Errorf("read semantic binary vector: %w", err)
	}
	vector := make([]float32, r.header.EmbeddingDim)
	if r.header.Precision != semanticSearchIndexPrecision {
		return nil, errSemanticBinaryIndexUnavailable
	}
	for index := range vector {
		vector[index] = math.Float32frombits(binary.LittleEndian.Uint32(raw[index*4 : index*4+4]))
	}
	return vector, nil
}

func (r *semanticBinaryIndexReader) dotOrdinalWithBuffer(ctx context.Context, ordinal uint32, query []float32, raw []byte) (float32, error) {
	if int(ordinal) >= len(r.nodes) {
		return 0, errSemanticBinaryIndexUnavailable
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	node := r.nodes[ordinal]
	if len(raw) < r.header.VectorStride {
		raw = make([]byte, r.header.VectorStride)
	} else {
		raw = raw[:r.header.VectorStride]
	}
	if _, err := r.file.ReadAt(raw, node.VectorOffset); err != nil {
		return 0, fmt.Errorf("read semantic binary vector: %w", err)
	}
	dim := min(len(query), r.header.EmbeddingDim)
	var sum float32
	if r.header.Precision != semanticSearchIndexPrecision {
		return 0, errSemanticBinaryIndexUnavailable
	}
	for index := 0; index < dim; index++ {
		sum += query[index] * math.Float32frombits(binary.LittleEndian.Uint32(raw[index*4:index*4+4]))
	}
	return sum, nil
}

func (r *semanticBinaryIndexReader) neighborOrdinals(ctx context.Context, ordinal uint32, level int) ([]uint32, error) {
	records, err := r.edgeRecords(ctx, ordinal)
	if err != nil {
		return nil, err
	}
	neighbors := make([]uint32, 0, len(records))
	for _, record := range records {
		if int(record.Level) == level {
			neighbors = append(neighbors, record.Neighbor)
		}
	}
	return neighbors, nil
}

func (r *semanticBinaryIndexReader) edgeRecords(ctx context.Context, ordinal uint32) ([]semanticBinaryEdgeRecord, error) {
	if int(ordinal) >= len(r.nodes) {
		return nil, errSemanticBinaryIndexUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	node := r.nodes[ordinal]
	raw := make([]byte, int(node.EdgeCount)*semanticBinaryIndexEdgeRecordSize)
	if len(raw) == 0 {
		return []semanticBinaryEdgeRecord{}, nil
	}
	if _, err := r.file.ReadAt(raw, node.EdgeOffset); err != nil {
		return nil, fmt.Errorf("read semantic binary edges: %w", err)
	}
	records := make([]semanticBinaryEdgeRecord, node.EdgeCount)
	for index := range records {
		offset := index * semanticBinaryIndexEdgeRecordSize
		records[index] = semanticBinaryEdgeRecord{
			Neighbor: binary.LittleEndian.Uint32(raw[offset : offset+4]),
			Level:    binary.LittleEndian.Uint16(raw[offset+4 : offset+6]),
			Rank:     binary.LittleEndian.Uint16(raw[offset+6 : offset+8]),
		}
	}
	return records, nil
}

func semanticBinaryVectorStride(dim int) int {
	if dim <= 0 || dim > int(^uint(0)>>1)/4 {
		return 0
	}
	return dim * 4
}
