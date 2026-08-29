package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	imagedraw "image/draw"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
	_ "golang.org/x/image/webp"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
)

var ErrNoDatasourceConfigured = errors.New("no datasource configured")
var ErrMediaTooLarge = errors.New("media response too large")
var ErrMediaInvalid = errors.New("media response invalid")
var ErrAssetNotFound = errors.New("asset not found")
var ErrDatasourceUnavailable = errors.New("datasource unavailable")
var ErrSemanticAssetInput = errors.New("semantic asset input invalid")
var ErrSemanticSourceUnavailable = errors.New("semantic datasource unavailable")
var ErrSemanticRuntimeUnavailable = errors.New("semantic embedding runtime unavailable")
var ErrInvalidSearchRequest = errors.New("invalid search request")
var ErrUnsupportedSearch = errors.New("unsupported search")

const (
	previewSize                = "thumbnail"
	detailPreviewSize          = "preview"
	previewMaxEdgePixels       = 512
	previewMaxBytes            = 128 << 10
	detailPreviewMaxEdgePixels = 2560
	detailPreviewJPEGQuality   = 82
	detailPreviewMaxBytes      = 1 << 20
	detailPreviewMaxSource     = 32 << 20
	statisticsTotalCacheTTL    = time.Minute
	statisticsFailureCacheTTL  = 15 * time.Second
	statisticsRequestTimeout   = 2 * time.Second
	defaultPageSize            = 60
	maxPageSize                = 200
	semanticRetryBaseInterval  = 30 * time.Second
	semanticRetryMaxInterval   = 30 * time.Minute
	mediaFallbackSourceTimeout = 3 * time.Second
	mediaFallbackSourceBackoff = 30 * time.Second
)

var (
	previewJPEGQualities       = []int{58, 50, 42}
	detailPreviewJPEGQualities = []int{detailPreviewJPEGQuality, 70, 58, 50, 42}

	// ErrSemanticSearchIndexNotReady means no searchable image semantic index is
	// currently available to load into the in-memory search cache.
	ErrSemanticSearchIndexNotReady = errors.New("semantic search index is not ready")
)

// Asset matches the Timich app-facing asset model returned to clients.
type Asset struct {
	SourceKey     string    `json:"-"`
	ID            string    `json:"id"`
	Type          string    `json:"type"`
	Filename      string    `json:"filename"`
	CapturedAt    time.Time `json:"capturedAt"`
	Duration      *string   `json:"duration,omitempty"`
	SemanticScore *float32  `json:"semanticScore,omitempty"`
}

// AssetSearchRequest describes a page from a browsable Timich asset collection.
type AssetSearchRequest struct {
	Collection AssetCollectionRequest `json:"collection"`
	Page       AssetSearchPageRequest `json:"page"`
}

type AssetCollectionRequest struct {
	Kind    string             `json:"kind"`
	Query   *AssetSearchQuery  `json:"query,omitempty"`
	Filters AssetSearchFilters `json:"filters,omitempty"`
	Sort    *AssetSearchSort   `json:"sort,omitempty"`
}

type AssetSearchQuery struct {
	Text string `json:"text,omitempty"`
	Mode string `json:"mode,omitempty"`
}

type AssetSearchFilters struct {
	MediaTypes []string                 `json:"mediaTypes,omitempty"`
	CapturedAt *AssetSearchCapturedTime `json:"capturedAt,omitempty"`
}

type AssetSearchCapturedTime struct {
	From *time.Time `json:"from,omitempty"`
	To   *time.Time `json:"to,omitempty"`
}

func (f *AssetSearchCapturedTime) UnmarshalJSON(data []byte) error {
	var raw struct {
		From *string `json:"from"`
		To   *string `json:"to"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.From != nil {
		from, err := parseUTCSearchTime(*raw.From)
		if err != nil {
			return err
		}
		f.From = &from
	}
	if raw.To != nil {
		to, err := parseUTCSearchTime(*raw.To)
		if err != nil {
			return err
		}
		f.To = &to
	}
	return nil
}

type AssetSearchSort struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
}

type AssetSearchPageRequest struct {
	Index int `json:"index"`
	Size  int `json:"size"`
}

// AssetSearchPage summarizes one paginated asset collection response.
type AssetSearchPage struct {
	CollectionKey string                 `json:"collectionKey"`
	Page          AssetSearchPageRequest `json:"page"`
	Items         []Asset                `json:"items"`
	Total         int                    `json:"total"`
	TotalAccuracy string                 `json:"totalAccuracy"`
	ElapsedMs     int64                  `json:"elapsedMs,omitempty"`
	NextPageIndex *int                   `json:"nextPageIndex,omitempty"`
	Boundary      *AssetSearchBoundary   `json:"boundary,omitempty"`
	Resolved      AssetSearchResolved    `json:"resolved"`
}

type AssetSearchBoundary struct {
	Kind string `json:"kind"`
}

type AssetSearchResolved struct {
	CollectionKind string                         `json:"collectionKind"`
	QueryMode      string                         `json:"queryMode"`
	Sort           AssetSearchSort                `json:"sort"`
	TimelineLike   bool                           `json:"timelineLike"`
	Semantic       *AssetSearchSemanticResolution `json:"semantic,omitempty"`
}

type AssetSearchSemanticResolution struct {
	Status               string                   `json:"status"`
	Eligible             bool                     `json:"eligible"`
	ModelID              string                   `json:"modelId,omitempty"`
	VectorSpaceID        string                   `json:"vectorSpaceId,omitempty"`
	EmbeddingDim         int                      `json:"embeddingDim,omitempty"`
	ProfileKind          string                   `json:"profileKind,omitempty"`
	InputKind            string                   `json:"inputKind,omitempty"`
	CompletedVectorCount int                      `json:"completedVectorCount"`
	IndexedVectorCount   int                      `json:"indexedVectorCount"`
	MessageCode          string                   `json:"messageCode,omitempty"`
	FallbackQueryMode    string                   `json:"fallbackQueryMode,omitempty"`
	ModelPack            *SemanticModelPackStatus `json:"modelPack,omitempty"`
}

type AssetSearchCapabilities struct {
	QueryModes    []string                         `json:"queryModes"`
	Filters       AssetSearchFilterCapabilities    `json:"filters"`
	Sorts         []AssetSearchSortCapability      `json:"sorts"`
	TotalAccuracy []string                         `json:"totalAccuracy"`
	Page          AssetSearchPageCapabilities      `json:"page"`
	Semantic      *AssetSearchSemanticCapabilities `json:"semantic,omitempty"`
}

type AssetSearchSemanticCapabilities struct {
	Status               string                   `json:"status"`
	ModelID              string                   `json:"modelId,omitempty"`
	VectorSpaceID        string                   `json:"vectorSpaceId,omitempty"`
	EmbeddingDim         int                      `json:"embeddingDim,omitempty"`
	ProfileKind          string                   `json:"profileKind,omitempty"`
	InputKind            string                   `json:"inputKind,omitempty"`
	CompletedVectorCount int                      `json:"completedVectorCount"`
	IndexedVectorCount   int                      `json:"indexedVectorCount"`
	MessageCode          string                   `json:"messageCode,omitempty"`
	ModelPack            *SemanticModelPackStatus `json:"modelPack,omitempty"`
}

type SemanticModelPackStatus struct {
	ID             string                       `json:"id,omitempty"`
	Name           string                       `json:"name,omitempty"`
	Version        string                       `json:"version,omitempty"`
	Role           string                       `json:"role,omitempty"`
	Status         string                       `json:"status,omitempty"`
	Source         string                       `json:"source,omitempty"`
	InputKind      string                       `json:"inputKind,omitempty"`
	VectorSpaceID  string                       `json:"vectorSpaceId,omitempty"`
	EmbeddingDim   int                          `json:"embeddingDim,omitempty"`
	QueryLanguages []string                     `json:"queryLanguages,omitempty"`
	Runtime        string                       `json:"runtime,omitempty"`
	Quantization   string                       `json:"quantization,omitempty"`
	SizeBytes      int64                        `json:"sizeBytes,omitempty"`
	License        string                       `json:"license,omitempty"`
	Artifact       *SemanticModelArtifactStatus `json:"artifact,omitempty"`
	Installed      bool                         `json:"installed"`
	InstalledAt    time.Time                    `json:"installedAt,omitempty"`
}

type SemanticModelArtifactStatus struct {
	Filename  string `json:"filename,omitempty"`
	URL       string `json:"url,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

type AssetSearchFilterCapabilities struct {
	MediaTypes []string `json:"mediaTypes"`
	CapturedAt bool     `json:"capturedAt"`
}

type AssetSearchSortCapability struct {
	Field      string   `json:"field"`
	Directions []string `json:"directions"`
}

type AssetSearchPageCapabilities struct {
	MaxSize int `json:"maxSize"`
}

type normalizedAssetSearch struct {
	Request       AssetSearchRequest
	Resolved      AssetSearchResolved
	CollectionKey string
}

type AssetSearchOptions struct {
	IncludeSemanticScores bool
	SemanticModelID       string
	SemanticVectorSpaceID string
}

const (
	CollectionKindTimeline = "timeline"
	CollectionKindSearch   = "search"

	QueryModeNone     = "none"
	QueryModeAuto     = "auto"
	QueryModeSemantic = "semantic"
	QueryModeFilename = "filename"

	SortFieldCapturedAt = "capturedAt"
	SortFieldRelevance  = "relevance"
	SortDirectionDesc   = "desc"

	TotalAccuracyExact      = "exact"
	TotalAccuracyEstimated  = "estimated"
	TotalAccuracyLowerBound = "lowerBound"

	BoundaryPastEnd = "pastEnd"
)

// UpstreamMediaResponse holds a proxied upstream media response.
type UpstreamMediaResponse struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

type photoDetailTiming struct {
	Profile         string
	UpstreamHeaders time.Duration
	ReadOriginal    time.Duration
	Decode          time.Duration
	Transform       time.Duration
	Encode          time.Duration
	Total           time.Duration
	OriginalBytes   int
	OutputBytes     int
	SourceWidth     int
	SourceHeight    int
	OutputWidth     int
	OutputHeight    int
	Format          string
}

type hostedImageProfile struct {
	Name           string
	UpstreamSize   string
	MaxEdgePixels  int
	MaxBytes       int
	JPEGQualities  []int
	FileNameBase   string
	FileNameSuffix string
	ForceJPEG      bool
}

// Service proxies configured datasources for local catalog/media reads.
type Service struct {
	client *http.Client

	mu                           sync.Mutex
	localRootTransitionGatesMu   sync.Mutex
	localRootTransitionGates     map[localMediaRootTransitionKey]*localMediaRootTransitionGate
	dataDir                      string
	mediaHelperPath              string
	mediaHelperAuto              bool
	mediaHelperCheck             localMediaHelperCapabilityStatus
	mediaVipsPath                string
	mediaVipsAuto                bool
	mediaVipsBundle              bool
	mediaFFmpegPath              string
	mediaFFmpegAuto              bool
	mediaFFmpegCheck             localFFmpegCapabilityStatus
	stateWriteCheck              func() error
	statisticsTotalCache         map[immichStatisticsTotalCacheKey]immichStatisticsTotalCacheEntry
	semanticSourceRetry          map[string]semanticSourceRetryState
	semanticSourceNow            func() time.Time
	mediaSourceRetry             map[string]time.Time
	localWorkNotify              func()
	localClaimRecoveryMu         sync.Mutex
	localMetadataRecoveries      map[int64]localMetadataJob
	localThumbnailRecoveries     map[int64]localThumbnailJob
	localContentVerificationHash func(context.Context, *os.File) (string, int64, os.FileInfo, error)
	datasourceGeneration         atomic.Uint64
	datasourceState              atomic.Pointer[serviceDatasourceState]
	catalog                      *CatalogStore
	semanticModels               *SemanticModelPackStore
	semanticText                 *semanticTextEmbeddingCache
}

// SetLocalWorkNotifier installs the runtime wake hook used when Local work is
// committed or request-time validation discovers work that must be regenerated.
func (s *Service) SetLocalWorkNotifier(notify func()) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.localWorkNotify = notify
	s.mu.Unlock()
}

func (s *Service) notifyLocalWorkQueued() {
	if s == nil {
		return
	}
	s.mu.Lock()
	notify := s.localWorkNotify
	s.mu.Unlock()
	if notify != nil {
		notify()
	}
}

type semanticSourceRetryState struct {
	Attempts  int
	NotBefore time.Time
}

type serviceDatasourceState struct {
	generation                      uint64
	primary                         *config.DatasourceConfig
	datasources                     map[string]config.DatasourceConfig
	localRoots                      map[string]config.LocalMediaRootConfig
	staticDemo                      *staticDemoSource
	staticDemoErr                   error
	galleryReadiness                catalogGalleryReadiness
	externalContentIdentityMappings []immichExternalLibraryMapping
	externalContentIdentityScopeKey string
}

func (s *serviceDatasourceState) ready() bool {
	if s == nil || s.primary == nil {
		return false
	}
	if s.primary.Kind == config.DatasourceKindStaticDemo {
		return s.staticDemo != nil && s.staticDemoErr == nil
	}
	return true
}

// ServiceOptions configures optional local catalog state.
type ServiceOptions struct {
	DataDir                   string
	LocalRoots                []config.LocalMediaRootConfig
	SemanticModels            *SemanticModelPackStore
	MediaHelperPath           string
	MediaVipsPath             string
	MediaFFmpegPath           string
	StateWriteCheck           func() error
	EnableDatasourceHotReload bool
}

type LocalMediaRuntimeStatus struct {
	Renderer                     string `json:"renderer"`
	MediaHelperPath              string `json:"mediaHelperPath,omitempty"`
	MediaHelperAvailable         bool   `json:"mediaHelperAvailable"`
	MediaHelperAuto              bool   `json:"mediaHelperAutoDetected"`
	MediaHelperUsable            bool   `json:"mediaHelperUsable"`
	MediaHelperStatus            string `json:"mediaHelperStatus,omitempty"`
	MediaHelperVersion           string `json:"mediaHelperVersion,omitempty"`
	MediaHelperPlatform          string `json:"mediaHelperPlatform,omitempty"`
	MediaHelperRenderImage       bool   `json:"mediaHelperRenderImage"`
	MediaHelperRenderVideoPoster bool   `json:"mediaHelperRenderVideoPoster"`
	MediaHelperInspectImage      bool   `json:"mediaHelperInspectImage"`
	MediaHelperInspectVideo      bool   `json:"mediaHelperInspectVideo"`
	MediaHelperLastError         string `json:"mediaHelperLastError,omitempty"`
	VipsPath                     string `json:"vipsPath,omitempty"`
	VipsAvailable                bool   `json:"vipsAvailable"`
	VipsAutoDetected             bool   `json:"vipsAutoDetected"`
	VipsBundled                  bool   `json:"vipsBundled"`
	FFmpegPath                   string `json:"ffmpegPath,omitempty"`
	FFmpegAvailable              bool   `json:"ffmpegAvailable"`
	FFmpegAuto                   bool   `json:"ffmpegAutoDetected"`
	FFmpegUsable                 bool   `json:"ffmpegUsable"`
	FFmpegStatus                 string `json:"ffmpegStatus,omitempty"`
	FFmpegVersion                string `json:"ffmpegVersion,omitempty"`
	FFmpegDecoders               string `json:"ffmpegDecoders,omitempty"`
	FFmpegLastError              string `json:"ffmpegLastError,omitempty"`
}

type SemanticModelBackfillOptions struct {
	MaxAssets                int
	Workers                  int
	SourceKeys               []string
	DrainIndexJobs           bool
	AllowPartialIndexPublish bool
	BeforeEmbed              func(context.Context) error
}

// NewService creates a local catalog/media proxy for the first configured datasource.
func NewService(datasources []config.DatasourceConfig) *Service {
	service, _ := NewServiceWithOptions(datasources, ServiceOptions{})
	return service
}

// NewServiceWithOptions creates a catalog service with optional persistent catalog state.
func NewServiceWithOptions(datasources []config.DatasourceConfig, options ServiceOptions) (*Service, error) {
	service := &Service{
		client:            &http.Client{Timeout: 30 * time.Second},
		dataDir:           strings.TrimSpace(options.DataDir),
		semanticModels:    options.SemanticModels,
		semanticText:      newSemanticTextEmbeddingCache(semanticTextEmbeddingCacheSize),
		stateWriteCheck:   options.StateWriteCheck,
		semanticSourceNow: time.Now,
		mediaSourceRetry:  map[string]time.Time{},
	}
	service.mediaHelperPath, service.mediaHelperAuto = resolveMediaHelperPath(options.MediaHelperPath)
	service.mediaVipsPath, service.mediaVipsAuto = resolveMediaVipsPath(options.MediaVipsPath)
	service.mediaVipsBundle = localMediaVipsBundleRoot(service.mediaVipsPath) != ""
	service.mediaFFmpegPath, service.mediaFFmpegAuto = resolveMediaFFmpegPath(options.MediaFFmpegPath)
	state := newServiceDatasourceState(datasources, options.LocalRoots, service.datasourceGeneration.Add(1))
	service.datasourceState.Store(state)
	if strings.TrimSpace(options.DataDir) != "" && (options.EnableDatasourceHotReload || catalogStoreNeededForDatasources(datasources)) {
		catalogStore, err := LoadOrCreateCatalogStore(options.DataDir)
		if err != nil {
			return nil, err
		}
		catalogStore.datasourceState = &service.datasourceState
		if _, err := catalogStore.reconcileConfiguredImmichExternalIdentities(context.Background(), datasources); err != nil {
			_ = catalogStore.Close()
			return nil, fmt.Errorf("prepare external content identities: %w", err)
		}
		if err := catalogStore.ensureGalleryTimeline(context.Background(), state.galleryReadiness); err != nil {
			_ = catalogStore.Close()
			return nil, fmt.Errorf("prepare gallery timeline: %w", err)
		}
		if err := catalogStore.ensureGalleryProjection(context.Background(), state.galleryReadiness); err != nil {
			_ = catalogStore.Close()
			return nil, fmt.Errorf("prepare mixed gallery projection: %w", err)
		}
		service.catalog = catalogStore
	}
	return service, nil
}

func catalogStoreNeededForDatasources(datasources []config.DatasourceConfig) bool {
	for _, datasource := range datasources {
		if config.IsIndexedDatasourceKind(datasource.Kind) {
			return true
		}
	}
	return false
}

func newServiceDatasourceState(datasources []config.DatasourceConfig, localRoots []config.LocalMediaRootConfig, generation uint64) *serviceDatasourceState {
	state := &serviceDatasourceState{
		generation:                      generation,
		externalContentIdentityMappings: configuredImmichExternalLibraryMappings(datasources),
		externalContentIdentityScopeKey: immichExternalIdentityScopeKey(datasources),
	}
	if len(localRoots) > 0 {
		state.localRoots = make(map[string]config.LocalMediaRootConfig, len(localRoots))
		for _, root := range localRoots {
			key := strings.TrimSpace(root.Key)
			if key != "" {
				state.localRoots[key] = root
			}
		}
	}
	if len(datasources) > 0 {
		state.datasources = make(map[string]config.DatasourceConfig, len(datasources))
		for index, datasource := range datasources {
			datasource = cloneServiceDatasourceConfig(datasource)
			if index == 0 {
				primary := datasource
				state.primary = &primary
				if datasource.Kind == config.DatasourceKindStaticDemo {
					state.staticDemo, state.staticDemoErr = newStaticDemoSource(datasource.URL)
				}
			}
			sourceKey := strings.TrimSpace(datasource.SourceKey)
			if sourceKey != "" {
				state.datasources[sourceKey] = datasource
			}
		}
	}
	state.galleryReadiness = catalogGalleryReadinessForDatasources(datasources)
	return state
}

func cloneServiceDatasourceConfig(datasource config.DatasourceConfig) config.DatasourceConfig {
	if datasource.Indexing != nil {
		indexing := *datasource.Indexing
		datasource.Indexing = &indexing
	}
	if datasource.Scan != nil {
		scan := *datasource.Scan
		if scan.ImmichExternalLibraryMappings != nil {
			scan.ImmichExternalLibraryMappings = append([]config.LocalDatasourceImmichExternalLibraryMapping(nil), scan.ImmichExternalLibraryMappings...)
		}
		if scan.ImmichFallbackEnabled != nil {
			enabled := *scan.ImmichFallbackEnabled
			scan.ImmichFallbackEnabled = &enabled
		}
		datasource.Scan = &scan
	}
	return datasource
}

func (s *Service) datasourceStateSnapshot() *serviceDatasourceState {
	if s == nil {
		return nil
	}
	return s.datasourceState.Load()
}

// ReconfigureDatasources atomically replaces immutable datasource policy while
// retaining the Catalog's long-lived database and media runtime resources.
func (s *Service) ReconfigureDatasources(datasources []config.DatasourceConfig) error {
	if s == nil {
		return nil
	}
	current := s.datasourceStateSnapshot()
	localRoots := []config.LocalMediaRootConfig{}
	if current != nil {
		localRoots = make([]config.LocalMediaRootConfig, 0, len(current.localRoots))
		for _, root := range current.localRoots {
			localRoots = append(localRoots, root)
		}
	}
	nextState := newServiceDatasourceState(datasources, localRoots, s.datasourceGeneration.Add(1))
	if s.catalog != nil {
		// Reconcile the durable worker commit fence before touching derived read
		// models. A failure therefore leaves the external-identity, timeline, and
		// projection scopes aligned with the still-published datasource state.
		if _, err := s.catalog.reconcileConfiguredImmichExternalIdentities(context.Background(), datasources); err != nil {
			return fmt.Errorf("reconcile external content identities for datasource reconfiguration: %w", err)
		}
		s.ensureGalleryReadModels(nextState.galleryReadiness, "reconfigure")
	}
	s.mu.Lock()
	s.datasourceState.Store(nextState)
	s.statisticsTotalCache = nil
	s.semanticSourceRetry = nil
	s.mu.Unlock()
	// Re-check after publication so a Local worker that committed between the
	// prebuild and the atomic datasource-state swap cannot leave the new scope
	// without a current Gallery generation.
	if s.catalog != nil {
		s.ensureGalleryReadModels(nextState.galleryReadiness, "post-reconfigure repair")
	}
	return nil
}

func (s *Service) ensureGalleryReadModels(readiness catalogGalleryReadiness, phase string) {
	if s == nil || s.catalog == nil {
		return
	}
	ensureTimeline := func() {
		if err := s.catalog.ensureGalleryTimeline(context.Background(), readiness); err != nil {
			log.Printf("timich-agent gallery timeline %s failed error=%v", phase, err)
		}
	}
	ensureProjection := func() {
		if err := s.catalog.ensureGalleryProjection(context.Background(), readiness); err != nil {
			log.Printf("timich-agent mixed gallery projection %s failed error=%v", phase, err)
		}
	}
	// Build the read model required by the next state before retiring the one
	// used by the current state. Its transaction keeps the old model visible to
	// concurrent readers until the replacement commits.
	if readiness.immichOnly {
		ensureTimeline()
		ensureProjection()
		return
	}
	ensureProjection()
	ensureTimeline()
}

func semanticRetryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := semanticRetryBaseInterval
	for step := 1; step < attempts && delay < semanticRetryMaxInterval; step++ {
		if delay > semanticRetryMaxInterval/2 {
			return semanticRetryMaxInterval
		}
		delay *= 2
	}
	return min(delay, semanticRetryMaxInterval)
}

func (s *Service) semanticSourceRetryTime() time.Time {
	if s != nil && s.semanticSourceNow != nil {
		return s.semanticSourceNow().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) semanticSourceRetryDeadline(sourceKey string, now time.Time) (*time.Time, bool) {
	if s == nil {
		return nil, false
	}
	sourceKey = strings.TrimSpace(sourceKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.semanticSourceRetry[sourceKey]
	if !ok || state.NotBefore.IsZero() || !state.NotBefore.After(now.UTC()) {
		return nil, false
	}
	deadline := state.NotBefore.UTC()
	return &deadline, true
}

func (s *Service) deferSemanticSourceRetry(sourceKey string, now time.Time) time.Time {
	sourceKey = strings.TrimSpace(sourceKey)
	if s == nil || sourceKey == "" {
		return now.UTC().Add(semanticRetryBaseInterval)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.semanticSourceRetry == nil {
		s.semanticSourceRetry = map[string]semanticSourceRetryState{}
	}
	state := s.semanticSourceRetry[sourceKey]
	state.Attempts++
	state.NotBefore = now.UTC().Add(semanticRetryDelay(state.Attempts))
	s.semanticSourceRetry[sourceKey] = state
	return state.NotBefore
}

func (s *Service) clearSemanticSourceRetry(sourceKey string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.semanticSourceRetry, strings.TrimSpace(sourceKey))
	s.mu.Unlock()
}

func catalogGalleryReadinessForDatasources(datasources []config.DatasourceConfig) catalogGalleryReadiness {
	readiness := catalogGalleryReadiness{immichOnly: len(datasources) > 0}
	for _, datasource := range datasources {
		sourceKey := strings.TrimSpace(datasource.SourceKey)
		if sourceKey == "" {
			continue
		}
		switch datasource.Kind {
		case config.DatasourceKindLocalFiles:
			readiness.immichOnly = false
			readiness.localSourceKeys = append(readiness.localSourceKeys, sourceKey)
			if config.LocalDatasourceImmichFallbackEnabled(datasource) {
				readiness.localImmichFallbackSourceKeys = append(readiness.localImmichFallbackSourceKeys, sourceKey)
			}
		case config.DatasourceKindImmichIndexed:
			readiness.immichSourceKeys = append(readiness.immichSourceKeys, sourceKey)
		default:
			readiness.immichOnly = false
		}
	}
	readiness.immichOnly = readiness.immichOnly && len(readiness.immichSourceKeys) > 0
	return readiness
}

func (s *Service) ensureStateWritesAvailable() error {
	if s == nil || s.stateWriteCheck == nil {
		return nil
	}
	return s.stateWriteCheck()
}

func (s *Service) LocalMediaRuntimeStatus() LocalMediaRuntimeStatus {
	return s.LocalMediaRuntimeStatusWithContext(context.Background())
}

func (s *Service) LocalMediaRuntimeStatusWithContext(ctx context.Context) LocalMediaRuntimeStatus {
	if s == nil {
		return LocalMediaRuntimeStatus{Renderer: "unavailable"}
	}
	helperStatus := s.localMediaHelperCapabilityStatusWithContext(ctx)
	ffmpegStatus := s.localFFmpegCapabilityStatusWithContext(ctx)
	renderer := "unavailable"
	if strings.TrimSpace(s.mediaHelperPath) != "" {
		renderer = "media-helper"
	}
	base := LocalMediaRuntimeStatus{
		Renderer:                     renderer,
		MediaHelperPath:              strings.TrimSpace(s.mediaHelperPath),
		MediaHelperAvailable:         strings.TrimSpace(s.mediaHelperPath) != "",
		MediaHelperAuto:              s.mediaHelperAuto,
		MediaHelperUsable:            helperStatus.Usable,
		MediaHelperStatus:            helperStatus.Status,
		MediaHelperVersion:           helperStatus.Version,
		MediaHelperPlatform:          helperStatus.Platform,
		MediaHelperRenderImage:       helperStatus.RenderImage,
		MediaHelperRenderVideoPoster: helperStatus.RenderVideoPoster,
		MediaHelperInspectImage:      helperStatus.InspectImage,
		MediaHelperInspectVideo:      helperStatus.InspectVideo,
		MediaHelperLastError:         helperStatus.LastError,
		FFmpegPath:                   strings.TrimSpace(s.mediaFFmpegPath),
		FFmpegAvailable:              strings.TrimSpace(s.mediaFFmpegPath) != "",
		FFmpegAuto:                   s.mediaFFmpegAuto,
		FFmpegUsable:                 ffmpegStatus.Usable,
		FFmpegStatus:                 ffmpegStatus.Status,
		FFmpegVersion:                ffmpegStatus.Version,
		FFmpegDecoders:               strings.Join(ffmpegStatus.Decoders, ", "),
		FFmpegLastError:              ffmpegStatus.LastError,
	}
	if strings.TrimSpace(s.mediaVipsPath) == "" {
		return base
	}
	base.VipsPath = s.mediaVipsPath
	base.VipsAvailable = true
	base.VipsAutoDetected = s.mediaVipsAuto
	base.VipsBundled = s.mediaVipsBundle
	return base
}

// Close releases local catalog resources.
func (s *Service) Close() error {
	if s == nil || s.catalog == nil {
		return nil
	}
	return s.catalog.Close()
}

// Ready reports whether a configured datasource can serve catalog requests.
func (s *Service) Ready() bool {
	state := s.datasourceStateSnapshot()
	return state.ready()
}

// SearchAssets returns one paginated asset page from the configured datasource.
func (s *Service) SearchAssets(searchRequest AssetSearchRequest) (AssetSearchPage, error) {
	return s.SearchAssetsWithOptionsContext(context.Background(), searchRequest, AssetSearchOptions{})
}

// SearchAssetsWithContext returns one paginated asset page from the configured datasource.
func (s *Service) SearchAssetsWithContext(ctx context.Context, searchRequest AssetSearchRequest) (AssetSearchPage, error) {
	return s.SearchAssetsWithOptionsContext(ctx, searchRequest, AssetSearchOptions{})
}

// SearchAssetsWithOptions returns one asset page with optional diagnostics for Admin tooling.
func (s *Service) SearchAssetsWithOptions(searchRequest AssetSearchRequest, options AssetSearchOptions) (AssetSearchPage, error) {
	return s.SearchAssetsWithOptionsContext(context.Background(), searchRequest, options)
}

// SearchAssetsWithOptionsContext returns one asset page with optional diagnostics for Admin tooling.
func (s *Service) SearchAssetsWithOptionsContext(ctx context.Context, searchRequest AssetSearchRequest, options AssetSearchOptions) (AssetSearchPage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	state := s.datasourceStateSnapshot()
	if !state.ready() {
		return AssetSearchPage{}, ErrNoDatasourceConfigured
	}
	normalized, err := normalizeAssetSearchRequest(searchRequest)
	if err != nil {
		return AssetSearchPage{}, err
	}
	if state.primary.Kind == config.DatasourceKindStaticDemo {
		return state.staticDemo.SearchAssets(normalized)
	}
	if config.IsImmichPassthroughDatasourceKind(state.primary.Kind) {
		return s.searchImmichPassthroughAssets(ctx, state, normalized)
	}
	if !s.catalogStoreEnabled() {
		return AssetSearchPage{}, ErrCatalogNotConfigured
	}
	if normalized.Resolved.QueryMode == QueryModeSemantic {
		selection := s.semanticSearchProfileSelection(ctx, options)
		if selection.profile == nil {
			return s.searchCatalogWithoutSemanticProfile(ctx, normalized)
		}
		if !selection.published {
			semantic := s.semanticSearchUnavailableStatus(ctx, selection.profile)
			return s.searchCatalogWithoutUsableSemanticIndex(ctx, normalized, semantic, selection.profile)
		}
		return s.searchCatalogSemanticAssets(ctx, normalized, selection.profile, options)
	}
	return s.catalog.SearchCatalogAssets(ctx, normalized)
}

type semanticSearchProfileSelection struct {
	profile   semanticEmbeddingProfile
	published bool
}

func (s *Service) semanticSearchProfile(ctx context.Context, options AssetSearchOptions) semanticEmbeddingProfile {
	return s.semanticSearchProfileSelection(ctx, options).profile
}

func (s *Service) semanticSearchProfileSelection(ctx context.Context, options AssetSearchOptions) semanticSearchProfileSelection {
	if profile := s.preferredSemanticSearchCandidateProfile(ctx, options); profile != nil {
		return semanticSearchProfileSelection{
			profile:   cachedSemanticTextProfileFor(profile, s.semanticText),
			published: s.semanticSearchHasPublishedIndex(ctx, profile),
		}
	}
	active := s.semanticSearchActiveProfile(ctx)
	if semanticSearchableImageProfile(active) {
		if s.semanticSearchHasPublishedIndex(ctx, active) {
			return semanticSearchProfileSelection{
				profile:   cachedSemanticTextProfileFor(active, s.semanticText),
				published: true,
			}
		}
	}
	candidate := s.semanticSearchCandidateProfile(ctx, options)
	if candidate != nil && (active == nil || candidate.ModelID() != active.ModelID() || candidate.VectorSpaceID() != active.VectorSpaceID()) {
		if s.semanticSearchHasPublishedIndex(ctx, candidate) {
			return semanticSearchProfileSelection{
				profile:   cachedSemanticTextProfileFor(candidate, s.semanticText),
				published: true,
			}
		}
	}
	if active != nil && !semanticSearchableImageProfile(active) {
		if s.semanticSearchHasPublishedIndex(ctx, active) {
			return semanticSearchProfileSelection{
				profile:   cachedSemanticTextProfileFor(active, s.semanticText),
				published: true,
			}
		}
	}
	if semanticSearchableImageProfile(active) {
		return semanticSearchProfileSelection{profile: cachedSemanticTextProfileFor(active, s.semanticText)}
	}
	if candidate != nil && (active == nil || candidate.ModelID() != active.ModelID() || candidate.VectorSpaceID() != active.VectorSpaceID()) {
		return semanticSearchProfileSelection{profile: cachedSemanticTextProfileFor(candidate, s.semanticText)}
	}
	if active != nil {
		return semanticSearchProfileSelection{profile: cachedSemanticTextProfileFor(active, s.semanticText)}
	}
	return semanticSearchProfileSelection{}
}

func semanticSearchableImageProfile(profile semanticEmbeddingProfile) bool {
	return profile != nil &&
		profile.ProfileKind() == semanticProfileKindModelPack &&
		profile.InputKind() == semanticInputKindImage
}

func (s *Service) semanticSearchCandidateProfile(ctx context.Context, options AssetSearchOptions) semanticEmbeddingProfile {
	if s == nil || s.semanticModels == nil || !s.catalogStoreEnabled() {
		return nil
	}
	if strings.TrimSpace(options.SemanticModelID) != "" || strings.TrimSpace(options.SemanticVectorSpaceID) != "" {
		return nil
	}
	candidate, ok := s.semanticModels.InstalledCandidateProfile()
	if !ok || candidate.Role != semanticModelRoleCandidate {
		return nil
	}
	candidateStarted := time.Now()
	profile, ok := s.semanticModels.CandidateEmbeddingProfileWithContext(ctx, candidate.ModelID, candidate.VectorSpaceID)
	if !ok {
		log.Printf(
			"timich-agent semantic search candidate profile missing runtime model=%s vector_space=%s elapsed=%s",
			candidate.ModelID,
			candidate.VectorSpaceID,
			time.Since(candidateStarted).Round(time.Millisecond),
		)
		return nil
	}
	log.Printf(
		"timich-agent semantic search candidate runtime profile selected model=%s vector_space=%s elapsed=%s",
		candidate.ModelID,
		candidate.VectorSpaceID,
		time.Since(candidateStarted).Round(time.Millisecond),
	)
	return profile
}

func (s *Service) semanticSearchActiveProfile(ctx context.Context) semanticEmbeddingProfile {
	if s == nil || s.semanticModels == nil || !s.catalogStoreEnabled() {
		return nil
	}
	started := time.Now()
	log.Printf("timich-agent semantic search active profile store lookup started")
	active, ok := s.semanticModels.ActiveProfileWithContext(ctx)
	if !ok || active.Role != semanticModelRoleActive {
		log.Printf(
			"timich-agent semantic search active profile store lookup none elapsed=%s",
			time.Since(started).Round(time.Millisecond),
		)
		return nil
	}
	profile, ok := s.semanticModels.ActiveEmbeddingProfileWithContext(ctx)
	if !ok {
		log.Printf(
			"timich-agent semantic search active profile runtime missing model=%s vector_space=%s elapsed=%s",
			active.ModelID,
			active.VectorSpaceID,
			time.Since(started).Round(time.Millisecond),
		)
		return nil
	}
	log.Printf(
		"timich-agent semantic search active runtime profile selected model=%s vector_space=%s elapsed=%s",
		active.ModelID,
		active.VectorSpaceID,
		time.Since(started).Round(time.Millisecond),
	)
	return profile
}

func (s *Service) semanticSearchHasPublishedIndex(ctx context.Context, profile semanticEmbeddingProfile) bool {
	if s == nil || s.catalog == nil || profile == nil {
		return false
	}
	for _, sourceKey := range s.semanticDatasourceSourceKeys() {
		available, err := s.catalog.hasPublishedSemanticBinaryIndex(ctx, sourceKey, profile)
		if err != nil {
			log.Printf(
				"timich-agent semantic search published index unavailable source_key=%s model=%s vector_space=%s error=%v",
				sourceKey,
				profile.ModelID(),
				profile.VectorSpaceID(),
				err,
			)
			continue
		}
		if available {
			return true
		}
	}
	return false
}

func (s *Service) semanticSearchUnavailableStatus(ctx context.Context, profile semanticEmbeddingProfile) CatalogSemanticStatus {
	semantic := baseCatalogSemanticStatus(profile)
	directStatusSeen := false
	directStatusAllReady := true
	statusReadSucceeded := false
	for _, sourceKey := range s.semanticDatasourceSourceKeys() {
		current, err := s.catalog.semanticStatusForBinarySearch(ctx, sourceKey, profile)
		if err != nil {
			log.Printf(
				"timich-agent semantic search unavailable status skipped source_key=%s model=%s vector_space=%s error=%v",
				sourceKey,
				profile.ModelID(),
				profile.VectorSpaceID(),
				err,
			)
			continue
		}
		statusReadSucceeded = true
		status := strings.TrimSpace(current.Status)
		contributes := current.CompletedVectorCount > 0 ||
			current.IndexedVectorCount > 0 ||
			(status != "" && status != "missing")
		if contributes {
			directStatusSeen = true
			if status != semanticBackfillStatusReady {
				directStatusAllReady = false
			}
		}
		semantic.CompletedVectorCount += current.CompletedVectorCount
		semantic.IndexedVectorCount += current.IndexedVectorCount
		if current.BuiltAt != nil && (semantic.BuiltAt == nil || current.BuiltAt.After(*semantic.BuiltAt)) {
			builtAt := current.BuiltAt.UTC()
			semantic.BuiltAt = &builtAt
		}
	}
	if !statusReadSucceeded {
		semantic.Status = "unavailable"
		return normalizeCatalogSemanticStatus(semantic, profile)
	}
	semantic = finalizeCatalogSemanticDirectStatus(semantic, directStatusSeen, directStatusAllReady, profile)
	return semanticBinaryUnavailableSearchStatus(semantic, profile)
}

func semanticBackfillStatusLogValue(status *SemanticModelBackfillStatus) string {
	if status == nil {
		return ""
	}
	return status.Status
}

func semanticBackfillIndexedLogValue(status *SemanticModelBackfillStatus) int {
	if status == nil {
		return 0
	}
	return status.IndexedVectorCount
}

func semanticBackfillCompletedLogValue(status *SemanticModelBackfillStatus) int {
	if status == nil {
		return 0
	}
	return status.CompletedVectorCount
}

func semanticBackfillEligibleLogValue(status *SemanticModelBackfillStatus) int {
	if status == nil {
		return 0
	}
	return status.EligibleAssetCount
}

func (s *Service) preferredSemanticSearchCandidateProfile(ctx context.Context, options AssetSearchOptions) semanticEmbeddingProfile {
	modelID := strings.TrimSpace(options.SemanticModelID)
	vectorSpaceID := strings.TrimSpace(options.SemanticVectorSpaceID)
	if modelID == "" && vectorSpaceID == "" {
		return nil
	}
	candidate, ok := s.semanticModels.InstalledCandidateProfile()
	if !ok || (modelID != "" && candidate.ModelID != modelID) ||
		(vectorSpaceID != "" && candidate.VectorSpaceID != vectorSpaceID) {
		return nil
	}
	profile, ok := s.semanticModels.CandidateEmbeddingProfileWithContext(ctx, candidate.ModelID, candidate.VectorSpaceID)
	if !ok {
		return nil
	}
	return profile
}

// Asset returns app-facing metadata for one asset from the primary datasource.
func (s *Service) Asset(assetID string) (Asset, error) {
	return s.AssetFromSource("", assetID)
}

// AssetFromSource returns app-facing metadata for one datasource asset.
func (s *Service) AssetFromSource(sourceKey string, assetID string) (Asset, error) {
	state, datasource, err := s.datasourceForMedia(sourceKey)
	if err != nil {
		return Asset{}, err
	}
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return Asset{}, ErrAssetNotFound
	}
	if datasource.Kind == config.DatasourceKindStaticDemo {
		if state.staticDemoErr != nil {
			return Asset{}, state.staticDemoErr
		}
		if state.staticDemo == nil {
			return Asset{}, ErrNoDatasourceConfigured
		}
		return state.staticDemo.Asset(assetID)
	}
	if config.IsImmichPassthroughDatasourceKind(datasource.Kind) {
		request, err := s.newRequestForDatasource(datasource, http.MethodGet, "/api/assets/"+url.PathEscape(assetID), nil)
		if err != nil {
			return Asset{}, err
		}
		response, err := s.client.Do(request)
		if err != nil {
			return Asset{}, fmt.Errorf("perform asset request: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusNotFound {
			return Asset{}, ErrAssetNotFound
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return Asset{}, fmt.Errorf("asset request returned status %d", response.StatusCode)
		}

		var upstream immichAsset
		if err := json.NewDecoder(response.Body).Decode(&upstream); err != nil {
			return Asset{}, fmt.Errorf("decode asset response: %w", err)
		}
		if strings.TrimSpace(upstream.ID) == "" || upstream.FileCreatedAt.Time.IsZero() {
			return Asset{}, fmt.Errorf("asset response is missing required metadata")
		}
		asset := appAsset(upstream)
		asset.SourceKey = datasource.SourceKey
		return asset, nil
	}
	if s.catalog == nil {
		return Asset{}, ErrCatalogNotConfigured
	}
	_, asset, err := s.catalog.catalogCanonicalAssetForSource(context.Background(), datasource.SourceKey, assetID)
	if err != nil {
		return Asset{}, err
	}
	if strings.TrimSpace(asset.ID) == "" {
		return Asset{}, ErrAssetNotFound
	}
	return asset, nil
}

func appAsset(asset immichAsset) Asset {
	return Asset{
		ID:         asset.ID,
		Type:       normalizeAssetType(asset.Type),
		Filename:   asset.OriginalFileName,
		CapturedAt: asset.FileCreatedAt.Time.UTC(),
		Duration:   asset.Duration,
	}
}

// Probe verifies that the active datasource is reachable from the agent runtime.
func (s *Service) Probe(ctx context.Context) error {
	state := s.datasourceStateSnapshot()
	if !state.ready() {
		return ErrNoDatasourceConfigured
	}
	if state.primary.Kind == config.DatasourceKindStaticDemo {
		return nil
	}

	request, err := s.newRequestForDatasource(
		state.primary,
		http.MethodPost,
		"/api/search/metadata",
		strings.NewReader(`{"page":1,"size":1,"order":"desc"}`),
	)
	if err != nil {
		return err
	}
	request = request.WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")

	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("perform datasource probe: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("datasource probe returned status %d", response.StatusCode)
	}
	return nil
}

// MirrorStatus returns the active datasource mirror status when configured.
func (s *Service) MirrorStatus(ctx context.Context) (MirrorStatus, error) {
	return s.MirrorStatusForDatasource(ctx, "")
}

// MirrorStatusForDatasource returns one configured Immich mirror status by source key.
func (s *Service) MirrorStatusForDatasource(ctx context.Context, sourceKey string) (MirrorStatus, error) {
	datasource, err := s.mirrorDatasource(sourceKey)
	if err != nil {
		return MirrorStatus{}, err
	}
	status, err := s.catalog.Status(ctx, datasource.SourceKey)
	if err != nil {
		return MirrorStatus{}, err
	}
	status.LatestAssetLimit = datasourceIndexingConfig(datasource).LatestAssetLimit
	if status.Status == "" {
		status.Status = "idle"
	}
	return status, nil
}

// SemanticModelBackfillStatus reports candidate semantic vector coverage for all indexed datasources.
func (s *Service) SemanticModelBackfillStatus(ctx context.Context, candidate SemanticModelProfileStatus) (*SemanticModelBackfillStatus, error) {
	snapshot, err := s.SemanticModelBackfillSnapshot(ctx, candidate)
	if err != nil || snapshot == nil {
		return nil, err
	}
	status := snapshot.Status
	return &status, nil
}

// SemanticModelBackfillSnapshot reports aggregate and per-datasource semantic
// progress from one pass over the catalog. Scheduler decisions reuse this
// snapshot instead of issuing the same large-library counts repeatedly.
func (s *Service) SemanticModelBackfillSnapshot(ctx context.Context, candidate SemanticModelProfileStatus) (*SemanticModelBackfillSnapshot, error) {
	if !s.catalogStoreEnabled() {
		return nil, nil
	}
	if strings.TrimSpace(candidate.ModelID) == "" || strings.TrimSpace(candidate.VectorSpaceID) == "" {
		return nil, nil
	}
	started := time.Now()
	sourceKeys := s.semanticDatasourceSourceKeysFor(nil)
	if len(sourceKeys) == 0 {
		log.Printf(
			"timich-agent semantic model backfill status skipped model=%s vector_space=%s reason=no_sources elapsed=%s",
			candidate.ModelID,
			candidate.VectorSpaceID,
			time.Since(started).Round(time.Millisecond),
		)
		return nil, nil
	}
	log.Printf(
		"timich-agent semantic model backfill status started model=%s vector_space=%s input=%s sources=%d",
		candidate.ModelID,
		candidate.VectorSpaceID,
		candidate.InputKind,
		len(sourceKeys),
	)
	snapshot, err := s.semanticModelBackfillSnapshotForSourceKeys(ctx, sourceKeys, candidate)
	var status *SemanticModelBackfillStatus
	if snapshot != nil {
		status = &snapshot.Status
	}
	log.Printf(
		"timich-agent semantic model backfill status completed model=%s vector_space=%s sources=%d elapsed=%s err=%v status=%s indexed=%d completed=%d eligible=%d",
		candidate.ModelID,
		candidate.VectorSpaceID,
		len(sourceKeys),
		time.Since(started).Round(time.Millisecond),
		err,
		semanticBackfillStatusLogValue(status),
		semanticBackfillIndexedLogValue(status),
		semanticBackfillCompletedLogValue(status),
		semanticBackfillEligibleLogValue(status),
	)
	return snapshot, err
}

// SemanticModelBackfillStatusForDatasource reports semantic vector coverage for one datasource.
func (s *Service) SemanticModelBackfillStatusForDatasource(ctx context.Context, sourceKey string, candidate SemanticModelProfileStatus) (*SemanticModelBackfillStatus, error) {
	if !s.catalogStoreEnabled() {
		return nil, nil
	}
	if strings.TrimSpace(candidate.ModelID) == "" || strings.TrimSpace(candidate.VectorSpaceID) == "" {
		return nil, nil
	}
	sourceKeys := s.semanticDatasourceSourceKeysFor([]string{sourceKey})
	if len(sourceKeys) == 0 {
		return nil, nil
	}
	return s.semanticModelBackfillStatusForSourceKeys(ctx, sourceKeys, candidate)
}

func (s *Service) semanticModelBackfillStatusForSourceKeys(ctx context.Context, sourceKeys []string, candidate SemanticModelProfileStatus) (*SemanticModelBackfillStatus, error) {
	snapshot, err := s.semanticModelBackfillSnapshotForSourceKeys(ctx, sourceKeys, candidate)
	if err != nil || snapshot == nil {
		return nil, err
	}
	status := snapshot.Status
	return &status, nil
}

func (s *Service) semanticModelBackfillSnapshotForSourceKeys(ctx context.Context, sourceKeys []string, candidate SemanticModelProfileStatus) (*SemanticModelBackfillSnapshot, error) {
	status := SemanticModelBackfillStatus{
		SourceKind:    "catalog",
		ModelID:       strings.TrimSpace(candidate.ModelID),
		VectorSpaceID: strings.TrimSpace(candidate.VectorSpaceID),
		EmbeddingDim:  candidate.EmbeddingDim,
	}
	sourceStatuses := make([]SemanticBackfillSource, 0, len(sourceKeys))
	for _, sourceKey := range sourceKeys {
		sourceStarted := time.Now()
		log.Printf(
			"timich-agent semantic model backfill source status started source_key=%s model=%s vector_space=%s",
			sourceKey,
			candidate.ModelID,
			candidate.VectorSpaceID,
		)
		sourceStatus, err := s.catalog.SemanticBackfillStatus(ctx, sourceKey, candidate)
		if err != nil {
			log.Printf(
				"timich-agent semantic model backfill source status failed source_key=%s model=%s vector_space=%s elapsed=%s error=%v",
				sourceKey,
				candidate.ModelID,
				candidate.VectorSpaceID,
				time.Since(sourceStarted).Round(time.Millisecond),
				err,
			)
			return nil, err
		}
		if retryAt, deferred := s.semanticSourceRetryDeadline(sourceKey, s.semanticSourceRetryTime()); deferred {
			sourceStatus.EligibleNowVectorCount = 0
			if sourceStatus.NextEligibleAt == nil || retryAt.Before(*sourceStatus.NextEligibleAt) {
				nextEligibleAt := retryAt.UTC()
				sourceStatus.NextEligibleAt = &nextEligibleAt
			}
		}
		sourceStatusCopy := sourceStatus
		sourceStatuses = append(sourceStatuses, SemanticBackfillSource{SourceKey: sourceKey, Status: sourceStatusCopy})
		log.Printf(
			"timich-agent semantic model backfill source status completed source_key=%s model=%s vector_space=%s status=%s indexed=%d completed=%d eligible=%d pending_index=%d elapsed=%s",
			sourceKey,
			candidate.ModelID,
			candidate.VectorSpaceID,
			sourceStatus.Status,
			sourceStatus.IndexedVectorCount,
			sourceStatus.CompletedVectorCount,
			sourceStatus.EligibleAssetCount,
			sourceStatus.PendingIndexJobCount,
			time.Since(sourceStarted).Round(time.Millisecond),
		)
		status.EligibleAssetCount += sourceStatus.EligibleAssetCount
		status.EligibleNowVectorCount += sourceStatus.EligibleNowVectorCount
		status.CompletedVectorCount += sourceStatus.CompletedVectorCount
		status.FailedVectorCount += sourceStatus.FailedVectorCount
		status.IndexedVectorCount += sourceStatus.IndexedVectorCount
		status.PendingIndexJobCount += sourceStatus.PendingIndexJobCount
		status.FailedIndexJobCount += sourceStatus.FailedIndexJobCount
		status.EligibleIndexJobCount += sourceStatus.EligibleIndexJobCount
		if sourceStatus.AssetGeneration != sourceStatus.IndexedGeneration {
			status.GenerationMismatchSourceCount++
		}
		if sourceStatus.LastPublishedAt != nil && (status.LastPublishedAt == nil || sourceStatus.LastPublishedAt.After(*status.LastPublishedAt)) {
			lastPublishedAt := sourceStatus.LastPublishedAt.UTC()
			status.LastPublishedAt = &lastPublishedAt
		}
		if sourceStatus.NextEligibleAt != nil && (status.NextEligibleAt == nil || sourceStatus.NextEligibleAt.Before(*status.NextEligibleAt)) {
			nextEligibleAt := sourceStatus.NextEligibleAt.UTC()
			status.NextEligibleAt = &nextEligibleAt
		}
	}
	finalizeSemanticBackfillStatus(&status)
	return &SemanticModelBackfillSnapshot{Status: status, SourceStatuses: sourceStatuses}, nil
}

func (s *Service) BackfillSemanticModelCandidateWithOptions(ctx context.Context, modelStore *SemanticModelPackStore, candidate SemanticModelProfileStatus, options SemanticModelBackfillOptions) (SemanticBackfillResult, error) {
	if !s.catalogStoreEnabled() {
		return SemanticBackfillResult{}, ErrCatalogNotConfigured
	}
	if modelStore == nil {
		return SemanticBackfillResult{}, ErrSemanticModelPackInvalid
	}
	profile, ok := modelStore.CandidateEmbeddingProfileWithContext(ctx, candidate.ModelID, candidate.VectorSpaceID)
	if !ok {
		return SemanticBackfillResult{}, ErrSemanticModelPackInvalid
	}
	sourceKeys := s.semanticDatasourceSourceKeysFor(options.SourceKeys)
	if len(sourceKeys) == 0 {
		return SemanticBackfillResult{}, ErrCatalogNotConfigured
	}
	startedAt := time.Now().UTC()
	sourceKeys = s.semanticBackfillSourceOrder(ctx, candidate, sourceKeys)
	remaining := options.MaxAssets
	if remaining < 0 {
		remaining = 0
	}
	result := SemanticBackfillResult{StartedAt: startedAt}
	var sourceErrors []error
	upsertSourceStatus := func(sourceKey string, status SemanticModelBackfillStatus) {
		for index := range result.SourceStatuses {
			if result.SourceStatuses[index].SourceKey == sourceKey {
				result.SourceStatuses[index].Status = status
				return
			}
		}
		result.SourceStatuses = append(result.SourceStatuses, SemanticBackfillSource{SourceKey: sourceKey, Status: status})
	}
	processSource := func(sourceKey string, maxAssets int) (int, bool, error) {
		now := s.semanticSourceRetryTime()
		if retryAt, deferred := s.semanticSourceRetryDeadline(sourceKey, now); deferred {
			sourceErrors = append(sourceErrors, fmt.Errorf(
				"semantic source %s: %w until %s",
				sourceKey,
				ErrSemanticSourceUnavailable,
				retryAt.Format(time.RFC3339Nano),
			))
			return 0, false, nil
		}
		sourceResult, err := s.catalog.BackfillSemanticVectors(ctx, sourceKey, profile, startedAt, SemanticBackfillOptions{
			ImageLoader: s,
			MaxAssets:   maxAssets,
			Workers:     options.Workers,
			BeforeEmbed: options.BeforeEmbed,
		})
		if err != nil {
			if errors.Is(err, ErrSemanticSourceUnavailable) {
				retryAt := s.deferSemanticSourceRetry(sourceKey, now)
				log.Printf("timich-agent semantic source backfill deferred source_key=%s retry_at=%s error=%v", sourceKey, retryAt.Format(time.RFC3339Nano), err)
				sourceErrors = append(sourceErrors, fmt.Errorf("semantic source %s: %w", sourceKey, err))
				return 0, false, nil
			}
			return 0, false, err
		}
		s.clearSemanticSourceRetry(sourceKey)
		result.ProcessedVectorCount += sourceResult.ProcessedVectorCount
		result.CompletedAt = sourceResult.CompletedAt
		if sourceResult.Status.ModelID != "" || sourceResult.Status.VectorSpaceID != "" {
			upsertSourceStatus(sourceKey, sourceResult.Status)
		}
		return sourceResult.ProcessedVectorCount, maxAssets > 0 && sourceResult.ProcessedVectorCount >= maxAssets, nil
	}

	sourceIndexes := make(map[string]int, len(sourceKeys))
	for index, sourceKey := range sourceKeys {
		sourceIndexes[sourceKey] = index
	}
	lastAttempted := -1
	attemptedSources := make(map[string]struct{}, len(sourceKeys))
	if options.MaxAssets <= 0 {
		for index, sourceKey := range sourceKeys {
			if _, _, err := processSource(sourceKey, 0); err != nil {
				return SemanticBackfillResult{}, err
			}
			lastAttempted = index
			attemptedSources[sourceKey] = struct{}{}
		}
	} else {
		activeSources := append([]string(nil), sourceKeys...)
		for remaining > 0 && len(activeSources) > 0 {
			roundBudget := remaining
			quotas := semanticBackfillRoundQuotas(roundBudget, len(activeSources))
			nextRound := make([]string, 0, len(activeSources))
			for index, sourceKey := range activeSources {
				quota := quotas[index]
				if quota <= 0 {
					nextRound = append(nextRound, sourceKey)
					continue
				}
				processed, canFill, err := processSource(sourceKey, quota)
				if err != nil {
					return SemanticBackfillResult{}, err
				}
				remaining -= processed
				lastAttempted = sourceIndexes[sourceKey]
				attemptedSources[sourceKey] = struct{}{}
				if canFill {
					nextRound = append(nextRound, sourceKey)
				}
			}
			activeSources = nextRound
		}
	}
	if lastAttempted >= 0 {
		nextSource := nextSemanticBackfillSource(sourceKeys, lastAttempted)
		if len(attemptedSources) == len(sourceKeys) && len(sourceKeys) > 1 {
			nextSource = sourceKeys[1]
		}
		if err := s.rememberSemanticBackfillSource(ctx, candidate, nextSource); err != nil {
			log.Printf("timich-agent semantic source cursor update failed model=%s source_key=%s error=%v", candidate.ModelID, nextSource, err)
		}
	}
	if result.ProcessedVectorCount == 0 && len(sourceErrors) > 0 {
		return result, errors.Join(sourceErrors...)
	}
	if _, err := s.catalog.ReconcileSemanticIndexJobs(ctx, sourceKeys, profile, options.AllowPartialIndexPublish, time.Now().UTC()); err != nil {
		return SemanticBackfillResult{}, err
	}
	if options.DrainIndexJobs {
		publish, err := s.catalog.PublishNextSemanticIndexJob(ctx, sourceKeys, profile, time.Now().UTC())
		if err != nil {
			return SemanticBackfillResult{}, err
		}
		if publish.Published {
			result.CompletedAt = publish.CompletedAt
		}
	}
	status, err := s.semanticModelBackfillStatusForSourceKeys(ctx, sourceKeys, candidate)
	if err != nil {
		return SemanticBackfillResult{}, err
	}
	if status != nil {
		result.Status = *status
		result.IndexedVectorCount = status.IndexedVectorCount
	}
	if result.CompletedAt.IsZero() {
		result.CompletedAt = time.Now().UTC()
	}
	return result, nil
}

func semanticBackfillRoundQuotas(budget int, sourceCount int) []int {
	if budget <= 0 || sourceCount <= 0 {
		return nil
	}
	quotas := make([]int, sourceCount)
	base := budget / sourceCount
	extra := budget % sourceCount
	for index := range quotas {
		quotas[index] = base
		if index < extra {
			quotas[index]++
		}
	}
	return quotas
}

func nextSemanticBackfillSource(sourceKeys []string, lastAttempted int) string {
	if len(sourceKeys) == 0 || lastAttempted < 0 {
		return ""
	}
	next := (lastAttempted + 1) % len(sourceKeys)
	if next == 0 && len(sourceKeys) > 1 {
		// When every source was visited, rotate the next batch's starting source
		// instead of persisting the same start forever.
		next = 1
	}
	return sourceKeys[next]
}

func (s *Service) PublishNextSemanticIndexJob(ctx context.Context, modelStore *SemanticModelPackStore, candidate SemanticModelProfileStatus, sourceKeys []string) (SemanticIndexPublishResult, error) {
	if !s.catalogStoreEnabled() {
		return SemanticIndexPublishResult{}, ErrCatalogNotConfigured
	}
	if modelStore == nil {
		return SemanticIndexPublishResult{}, ErrSemanticModelPackInvalid
	}
	profile, ok := modelStore.CandidateEmbeddingProfileWithContext(ctx, candidate.ModelID, candidate.VectorSpaceID)
	if !ok {
		return SemanticIndexPublishResult{}, ErrSemanticModelPackInvalid
	}
	sourceKeys = s.semanticDatasourceSourceKeysFor(sourceKeys)
	if len(sourceKeys) == 0 {
		return SemanticIndexPublishResult{}, ErrCatalogNotConfigured
	}
	return s.catalog.PublishNextSemanticIndexJob(ctx, sourceKeys, profile, time.Now().UTC())
}

func (s *Service) ReconcileSemanticIndexJobs(ctx context.Context, modelStore *SemanticModelPackStore, candidate SemanticModelProfileStatus, sourceKeys []string, allowPartial bool) (int, error) {
	if !s.catalogStoreEnabled() {
		return 0, ErrCatalogNotConfigured
	}
	if modelStore == nil {
		return 0, ErrSemanticModelPackInvalid
	}
	profile, ok := modelStore.CandidateEmbeddingProfileWithContext(ctx, candidate.ModelID, candidate.VectorSpaceID)
	if !ok {
		return 0, ErrSemanticModelPackInvalid
	}
	sourceKeys = s.semanticDatasourceSourceKeysFor(sourceKeys)
	if len(sourceKeys) == 0 {
		return 0, ErrCatalogNotConfigured
	}
	return s.catalog.ReconcileSemanticIndexJobs(ctx, sourceKeys, profile, allowPartial, time.Now().UTC())
}

func (s *Service) SemanticModelIndexPublishNeeded(ctx context.Context, modelStore *SemanticModelPackStore, candidate SemanticModelProfileStatus, sourceKeys []string, allowPartial bool) (bool, int, error) {
	if !s.catalogStoreEnabled() {
		return false, 0, ErrCatalogNotConfigured
	}
	if modelStore == nil {
		return false, 0, ErrSemanticModelPackInvalid
	}
	profile, ok := modelStore.CandidateEmbeddingProfileWithContext(ctx, candidate.ModelID, candidate.VectorSpaceID)
	if !ok {
		return false, 0, ErrSemanticModelPackInvalid
	}
	sourceKeys = s.semanticDatasourceSourceKeysFor(sourceKeys)
	if len(sourceKeys) == 0 {
		return false, 0, ErrCatalogNotConfigured
	}
	return s.catalog.SemanticIndexPublishNeeded(ctx, sourceKeys, profile, allowPartial)
}

// SemanticModelIndexPublishNeededFromSnapshot preserves the normal per-source
// publication checks while reusing progress counts already collected for the
// current scheduler decision.
func (s *Service) SemanticModelIndexPublishNeededFromSnapshot(ctx context.Context, modelStore *SemanticModelPackStore, candidate SemanticModelProfileStatus, snapshot *SemanticModelBackfillSnapshot, allowPartial bool) (bool, int, error) {
	if snapshot == nil {
		return s.SemanticModelIndexPublishNeeded(ctx, modelStore, candidate, nil, allowPartial)
	}
	if !s.catalogStoreEnabled() {
		return false, 0, ErrCatalogNotConfigured
	}
	if modelStore == nil {
		return false, 0, ErrSemanticModelPackInvalid
	}
	profile, ok := modelStore.CandidateEmbeddingProfileWithContext(ctx, candidate.ModelID, candidate.VectorSpaceID)
	if !ok {
		return false, 0, ErrSemanticModelPackInvalid
	}
	allowedSourceKeys := s.semanticDatasourceSourceKeysFor(nil)
	allowed := make(map[string]struct{}, len(allowedSourceKeys))
	for _, sourceKey := range allowedSourceKeys {
		allowed[sourceKey] = struct{}{}
	}
	needed := false
	workCount := 0
	for _, source := range snapshot.SourceStatuses {
		if _, ok := allowed[strings.TrimSpace(source.SourceKey)]; !ok {
			continue
		}
		status := source.Status
		if !semanticModelBackfillSnapshotMatchesProfile(status, candidate) {
			return false, 0, ErrSemanticModelPackInvalid
		}
		jobCount := status.PendingIndexJobCount + status.FailedIndexJobCount
		if jobCount > 0 && status.EligibleIndexJobCount <= 0 {
			continue
		}
		if jobCount <= 0 && !s.catalog.semanticIndexPublishNeeded(ctx, source.SourceKey, profile, status, allowPartial) {
			continue
		}
		if sourceWork := semanticIndexPublishNeededWorkCount(status); sourceWork > 0 {
			needed = true
			workCount += sourceWork
		}
	}
	return needed, workCount, nil
}

func semanticModelBackfillSnapshotMatchesProfile(status SemanticModelBackfillStatus, profile SemanticModelProfileStatus) bool {
	return strings.TrimSpace(status.ModelID) == strings.TrimSpace(profile.ModelID) &&
		strings.TrimSpace(status.VectorSpaceID) == strings.TrimSpace(profile.VectorSpaceID) &&
		status.EmbeddingDim == profile.EmbeddingDim
}

func (s *Service) ResetRunningSemanticIndexJobs(ctx context.Context) (int, error) {
	if !s.catalogStoreEnabled() {
		return 0, ErrCatalogNotConfigured
	}
	return s.catalog.ResetRunningSemanticIndexJobs(ctx, time.Now().UTC())
}

// PendingSemanticIndexJobs reports whether a durable HNSW publish job still
// needs to be completed. It intentionally includes failed jobs because their
// scheduled_at deadline is the durable retry policy used across restarts.
func (s *Service) PendingSemanticIndexJobs(ctx context.Context) (bool, error) {
	if !s.catalogStoreEnabled() {
		return false, ErrCatalogNotConfigured
	}
	var pending int
	err := s.catalog.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1
		FROM semantic_index_jobs
		WHERE status IN ('queued', 'running', 'failed')
	)`).Scan(&pending)
	return pending != 0, err
}

func (s *Service) semanticDatasourceSourceKeys() []string {
	return s.semanticDatasourceSourceKeysFor(nil)
}

func (s *Service) semanticDatasourceSourceKeysFor(requested []string) []string {
	if !s.catalogStoreEnabled() {
		return nil
	}
	allSourceKeys := append([]string{}, s.MirrorDatasourceSourceKeys()...)
	allSourceKeys = append(allSourceKeys, s.LocalDatasourceSourceKeys()...)
	allowed := make(map[string]struct{}, len(allSourceKeys))
	for _, sourceKey := range allSourceKeys {
		allowed[sourceKey] = struct{}{}
	}
	if len(requested) == 0 {
		sort.Strings(allSourceKeys)
		return allSourceKeys
	}
	sourceKeys := make([]string, 0, len(requested))
	seen := map[string]struct{}{}
	for _, sourceKey := range requested {
		sourceKey = strings.TrimSpace(sourceKey)
		if sourceKey == "" {
			continue
		}
		if _, ok := allowed[sourceKey]; !ok {
			continue
		}
		if _, ok := seen[sourceKey]; ok {
			continue
		}
		seen[sourceKey] = struct{}{}
		sourceKeys = append(sourceKeys, sourceKey)
	}
	sort.Strings(sourceKeys)
	return sourceKeys
}

func (s *Service) semanticBackfillSourceOrder(ctx context.Context, candidate SemanticModelProfileStatus, sourceKeys []string) []string {
	ordered := append([]string(nil), sourceKeys...)
	if s == nil || s.catalog == nil || len(ordered) < 2 {
		return ordered
	}
	var nextSource string
	err := s.catalog.queryDB().QueryRowContext(ctx, `SELECT next_source_key
		FROM semantic_backfill_scheduler_state
		WHERE model_id = ? AND vector_space_id = ?`,
		strings.TrimSpace(candidate.ModelID), strings.TrimSpace(candidate.VectorSpaceID),
	).Scan(&nextSource)
	if err != nil {
		return ordered
	}
	for index, sourceKey := range ordered {
		if sourceKey == nextSource {
			return append(ordered[index:], ordered[:index]...)
		}
	}
	return ordered
}

func (s *Service) rememberSemanticBackfillSource(ctx context.Context, candidate SemanticModelProfileStatus, sourceKey string) error {
	if s == nil || s.catalog == nil || strings.TrimSpace(sourceKey) == "" {
		return nil
	}
	_, err := s.catalog.db.ExecContext(ctx, `INSERT INTO semantic_backfill_scheduler_state (
			model_id, vector_space_id, next_source_key, updated_at
		) VALUES (?, ?, ?, ?)
		ON CONFLICT(model_id, vector_space_id) DO UPDATE SET
			next_source_key = excluded.next_source_key,
			updated_at = excluded.updated_at`,
		strings.TrimSpace(candidate.ModelID),
		strings.TrimSpace(candidate.VectorSpaceID),
		strings.TrimSpace(sourceKey),
		formatCatalogTime(time.Now().UTC()),
	)
	if err != nil {
		return fmt.Errorf("remember semantic backfill source: %w", err)
	}
	return nil
}

func finalizeSemanticBackfillStatus(status *SemanticModelBackfillStatus) {
	if status == nil {
		return
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
	case status.GenerationMismatchSourceCount > 0 || status.IndexedVectorCount < status.CompletedVectorCount || status.PendingIndexJobCount > 0 || status.FailedIndexJobCount > 0:
		status.Status = semanticBackfillStatusIndexing
		status.MessageCode = semanticBackfillMessageIndexing
	}
}

type immichMirrorFetchOptions struct {
	LatestAssetLimit int
	UpdatedAfter     *time.Time
	DetailLimit      int
}

// SyncMirror refreshes the active Immich mirror.
func (s *Service) SyncMirror(ctx context.Context, mode string) (MirrorSyncResult, error) {
	datasource, err := s.mirrorDatasource("")
	if err != nil {
		return MirrorSyncResult{}, err
	}
	return s.syncMirrorDatasource(ctx, datasource, mode)
}

// SyncDatasourceMirror refreshes one configured Immich mirror by source key.
func (s *Service) SyncDatasourceMirror(ctx context.Context, sourceKey string, mode string) (MirrorSyncResult, error) {
	datasource, err := s.mirrorDatasource(sourceKey)
	if err != nil {
		return MirrorSyncResult{}, err
	}
	return s.syncMirrorDatasource(ctx, datasource, mode)
}

func (s *Service) syncMirrorDatasource(ctx context.Context, datasource *config.DatasourceConfig, mode string) (MirrorSyncResult, error) {
	if !s.immichDatasourceIndexed(datasource) {
		return MirrorSyncResult{}, ErrCatalogNotConfigured
	}
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = MirrorSyncModeFull
	}
	if mode != MirrorSyncModeFull && mode != MirrorSyncModeIncremental {
		return MirrorSyncResult{}, ErrUnsupportedSearch
	}
	startedAt := time.Now().UTC()
	sourceKey := strings.TrimSpace(datasource.SourceKey)
	indexing := datasourceIndexingConfig(datasource)
	limit := indexing.LatestAssetLimit
	detailLimit := indexing.MetadataDetailLimit
	var result MirrorSyncResult
	var err error
	switch mode {
	case MirrorSyncModeFull:
		assets, fetchErr := s.fetchImmichMirrorAssets(ctx, datasource, immichMirrorFetchOptions{
			LatestAssetLimit: limit,
			DetailLimit:      detailLimit,
		})
		if fetchErr != nil {
			return MirrorSyncResult{}, fetchErr
		}
		result, err = s.catalog.ReplaceFull(ctx, sourceKey, assets, limit, startedAt)
		if err != nil {
			return MirrorSyncResult{}, err
		}
	case MirrorSyncModeIncremental:
		needsFull, fullCheckErr := s.mirrorSyncNeedsFullForConfiguredLimit(ctx, sourceKey, limit)
		if fullCheckErr != nil {
			return MirrorSyncResult{}, fullCheckErr
		}
		if needsFull {
			return s.syncMirrorDatasource(ctx, datasource, MirrorSyncModeFull)
		}
		updatedAfter, cursorErr := s.catalog.LatestSourceUpdatedAt(ctx, sourceKey)
		if cursorErr != nil {
			return MirrorSyncResult{}, cursorErr
		}
		if updatedAfter == nil {
			return s.syncMirrorDatasource(ctx, datasource, MirrorSyncModeFull)
		}
		assets, fetchErr := s.fetchImmichMirrorAssets(ctx, datasource, immichMirrorFetchOptions{
			UpdatedAfter: updatedAfter,
			DetailLimit:  detailLimit,
		})
		if fetchErr != nil {
			return MirrorSyncResult{}, fetchErr
		}
		result, err = s.catalog.MergeIncremental(ctx, sourceKey, assets, startedAt)
		if err != nil {
			return MirrorSyncResult{}, err
		}
	}
	status, err := s.catalog.Status(ctx, sourceKey)
	if err != nil {
		return MirrorSyncResult{}, err
	}
	status.LatestAssetLimit = limit
	result.Mirror = status
	return result, nil
}

func (s *Service) mirrorSyncNeedsFullForConfiguredLimit(ctx context.Context, sourceKey string, configuredLimit int) (bool, error) {
	status, err := s.catalog.Status(ctx, sourceKey)
	if err != nil {
		return false, err
	}
	if status.LastFullSyncAt == nil {
		return true, nil
	}
	return status.LatestAssetLimit != configuredLimit, nil
}

func (s *Service) LoadSemanticImage(ctx context.Context, sourceKey string, upstreamAssetID string) (*semanticImageEmbeddingInput, error) {
	_, datasource, err := s.datasourceForMedia(sourceKey)
	if err != nil {
		return nil, err
	}
	if datasource.Kind == config.DatasourceKindLocalFiles {
		return s.loadLocalSemanticImage(ctx, datasource.SourceKey, upstreamAssetID)
	}
	if !s.immichDatasourceIndexed(datasource) {
		return nil, ErrCatalogNotConfigured
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://timich-agent.local/internal/semantic-image", nil)
	if err != nil {
		return nil, err
	}
	response, err := s.profileImageForDatasource(request, datasource, upstreamAssetID, hostedImageProfile{
		Name:           "semantic_embedding",
		UpstreamSize:   detailPreviewSize,
		MaxEdgePixels:  detailPreviewMaxEdgePixels,
		MaxBytes:       detailPreviewMaxBytes,
		JPEGQualities:  detailPreviewJPEGQualities,
		FileNameBase:   "semantic_embedding",
		FileNameSuffix: "_semantic_embedding",
		ForceJPEG:      true,
	})
	if err != nil {
		if errors.Is(err, ErrAssetNotFound) || errors.Is(err, ErrMediaTooLarge) || errors.Is(err, ErrMediaInvalid) {
			return nil, fmt.Errorf("%w: %v", ErrSemanticAssetInput, err)
		}
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if semanticImageStatusIsAssetSpecific(response.StatusCode) {
			return nil, fmt.Errorf("%w: semantic image request returned status %d", ErrSemanticAssetInput, response.StatusCode)
		}
		return nil, fmt.Errorf("semantic image request returned status %d", response.StatusCode)
	}
	body, err := readAtMost(response.Body, detailPreviewMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("read semantic image response: %w", err)
	}
	contentType := strings.TrimSpace(response.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return &semanticImageEmbeddingInput{
		Bytes:       body,
		ContentType: contentType,
		Source:      "immich_preview",
	}, nil
}

func semanticImageStatusIsAssetSpecific(statusCode int) bool {
	switch statusCode {
	case http.StatusNotFound,
		http.StatusGone,
		http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func (s *Service) loadLocalSemanticImage(ctx context.Context, sourceKey string, assetID string) (*semanticImageEmbeddingInput, error) {
	rendition, err := s.localReadySemanticRendition(ctx, sourceKey, assetID)
	if err != nil {
		if errors.Is(err, ErrAssetNotFound) {
			return nil, fmt.Errorf("%w: %v", ErrSemanticAssetInput, err)
		}
		return nil, err
	}
	file, _, err := s.openVerifiedLocalRendition(ctx, rendition)
	if err != nil {
		if errors.Is(err, errLocalRenditionInvalid) {
			if repairErr := s.markLocalRenditionsPending(ctx, sourceKey, assetID, err); repairErr != nil {
				return nil, errors.Join(fmt.Errorf("%w: %v", ErrSemanticAssetInput, ErrAssetNotFound), repairErr)
			}
			return nil, fmt.Errorf("%w: %v", ErrSemanticAssetInput, ErrAssetNotFound)
		}
		return nil, fmt.Errorf("open local semantic image: %w", err)
	}
	defer file.Close()
	body, err := readAtMost(file, detailPreviewMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("read local semantic image: %w", err)
	}
	return &semanticImageEmbeddingInput{
		Bytes:       body,
		ContentType: "image/jpeg",
		Source:      "local_" + rendition.Kind,
	}, nil
}

func (s *Service) localReadySemanticRendition(ctx context.Context, sourceKey string, assetID string) (localReadyRendition, error) {
	if s == nil || s.catalog == nil || s.catalog.db == nil {
		return localReadyRendition{}, ErrCatalogNotConfigured
	}
	rows, err := s.catalog.db.QueryContext(ctx, `SELECT kind, relative_path,
			COALESCE(size_bytes, -1), COALESCE(content_sha256, '')
		FROM local_renditions
		WHERE source_key = ?
			AND asset_id = ?
			AND kind IN (?, ?)
			AND status = 'ready'
			AND relative_path IS NOT NULL
		ORDER BY CASE kind WHEN ? THEN 0 ELSE 1 END
		LIMIT 1`,
		strings.TrimSpace(sourceKey),
		strings.TrimSpace(assetID),
		localRenditionKindDetailPreview,
		localRenditionKindPreview,
		localRenditionKindDetailPreview,
	)
	if err != nil {
		return localReadyRendition{}, fmt.Errorf("query local semantic rendition: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return localReadyRendition{}, fmt.Errorf("iterate local semantic rendition: %w", err)
		}
		return localReadyRendition{}, ErrAssetNotFound
	}
	var rendition localReadyRendition
	if err := rows.Scan(&rendition.Kind, &rendition.RelativePath, &rendition.SizeBytes, &rendition.SHA256); err != nil {
		return localReadyRendition{}, fmt.Errorf("scan local semantic rendition: %w", err)
	}
	return rendition, rows.Err()
}

func (s *Service) primaryImmichIndexed() bool {
	state := s.datasourceStateSnapshot()
	if state == nil {
		return false
	}
	return s.immichDatasourceIndexed(state.primary)
}

func (s *Service) immichCatalogEnabled() bool {
	return len(s.MirrorDatasourceSourceKeys()) > 0
}

func (s *Service) catalogStoreEnabled() bool {
	return s != nil && s.catalog != nil && (len(s.MirrorDatasourceSourceKeys()) > 0 || len(s.LocalDatasourceSourceKeys()) > 0)
}

func (s *Service) immichDatasourceIndexed(datasource *config.DatasourceConfig) bool {
	return s != nil &&
		datasource != nil &&
		datasource.Kind == config.DatasourceKindImmichIndexed &&
		strings.TrimSpace(datasource.SourceKey) != "" &&
		s.catalog != nil
}

func datasourceIndexingConfig(datasource *config.DatasourceConfig) config.DatasourceIndexingConfig {
	if datasource == nil || datasource.Indexing == nil {
		return config.DatasourceIndexingConfig{}
	}
	return *datasource.Indexing
}

func (s *Service) mirrorDatasource(sourceKey string) (*config.DatasourceConfig, error) {
	state := s.datasourceStateSnapshot()
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" {
		if state != nil && s.immichDatasourceIndexed(state.primary) {
			return state.primary, nil
		}
		return nil, ErrCatalogNotConfigured
	}
	if state == nil || state.datasources == nil {
		return nil, ErrCatalogNotConfigured
	}
	datasource, ok := state.datasources[sourceKey]
	if !ok || !s.immichDatasourceIndexed(&datasource) {
		return nil, ErrCatalogNotConfigured
	}
	return &datasource, nil
}

// MirrorDatasourceSourceKeys returns configured Immich mirror source keys in stable order.
func (s *Service) MirrorDatasourceSourceKeys() []string {
	state := s.datasourceStateSnapshot()
	if s == nil || s.catalog == nil || state == nil || len(state.datasources) == 0 {
		return nil
	}
	sourceKeys := make([]string, 0, len(state.datasources))
	for sourceKey, datasource := range state.datasources {
		datasource.SourceKey = sourceKey
		if s.immichDatasourceIndexed(&datasource) {
			sourceKeys = append(sourceKeys, sourceKey)
		}
	}
	sort.Strings(sourceKeys)
	return sourceKeys
}

// LocalDatasourceSourceKeys returns configured local filesystem source keys in stable order.
func (s *Service) LocalDatasourceSourceKeys() []string {
	state := s.datasourceStateSnapshot()
	if s == nil || s.catalog == nil || state == nil || len(state.datasources) == 0 {
		return nil
	}
	sourceKeys := make([]string, 0, len(state.datasources))
	for sourceKey, datasource := range state.datasources {
		if datasource.Kind == config.DatasourceKindLocalFiles && strings.TrimSpace(datasource.RootKey) != "" {
			sourceKeys = append(sourceKeys, sourceKey)
		}
	}
	sort.Strings(sourceKeys)
	return sourceKeys
}

func (s *Service) fetchImmichMirrorAssets(ctx context.Context, datasource *config.DatasourceConfig, options immichMirrorFetchOptions) ([]ImmichMirrorAsset, error) {
	latestAssetLimit := options.LatestAssetLimit
	if latestAssetLimit < 0 {
		latestAssetLimit = 0
	}
	detailLimit := options.DetailLimit
	if detailLimit < 0 {
		detailLimit = 0
	}
	const pageSize = maxPageSize
	assets := []ImmichMirrorAsset{}
	datasourceState := s.datasourceStateSnapshot()
	datasourceConfigs := datasourceConfigsFromState(datasourceState)
	externalMappings := configuredImmichExternalLibraryMappings(datasourceConfigs)
	externalContentIdentityScopeKey := ""
	if datasourceState != nil {
		externalContentIdentityScopeKey = datasourceState.externalContentIdentityScopeKey
	}
	for page := 1; ; page++ {
		body := map[string]any{
			"page":  page,
			"size":  pageSize,
			"order": SortDirectionDesc,
		}
		if options.UpdatedAfter != nil && !options.UpdatedAfter.IsZero() {
			body["updatedAfter"] = options.UpdatedAfter.UTC().Format(time.RFC3339Nano)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal immich mirror sync request: %w", err)
		}
		request, err := s.newRequestForDatasource(datasource, http.MethodPost, "/api/search/metadata", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		request = request.WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		response, err := s.client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("perform immich mirror sync request: %w", err)
		}
		var envelope searchAssetsEnvelope
		decodeErr := func() error {
			defer response.Body.Close()
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				return fmt.Errorf("immich mirror sync returned status %d", response.StatusCode)
			}
			if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				return fmt.Errorf("decode immich mirror sync response: %w", err)
			}
			return nil
		}()
		if decodeErr != nil {
			return nil, decodeErr
		}
		for _, asset := range envelope.Assets.Items {
			if !asset.ShouldMirror() || strings.TrimSpace(asset.ID) == "" || asset.FileCreatedAt.IsZero() {
				continue
			}
			var updatedAt *time.Time
			if asset.UpdatedAt != nil && !asset.UpdatedAt.IsZero() {
				value := asset.UpdatedAt.Time.UTC()
				updatedAt = &value
			}
			city, state, country, description := asset.LocationMetadata()
			checksumAlgorithm, checksumHex := asset.UpstreamChecksumIdentity()
			contentSizeBytes := int64(0)
			if checksumAlgorithm == upstreamChecksumAlgorithmSHA1 {
				contentSizeBytes = asset.ContentSizeBytes()
			}
			canonicalContentSHA1Hex, canonicalContentSizeBytes := asset.ContentIdentity()
			mappedLocal, mappedLocalRelativePath, _ := mappedImmichExternalPath(
				externalMappings,
				datasource.SourceKey,
				asset.OriginalPath,
			)
			assets = append(assets, ImmichMirrorAsset{
				UpstreamAssetID:                 asset.ID,
				MediaType:                       normalizeAssetType(asset.Type),
				Filename:                        asset.OriginalFileName,
				CapturedAt:                      asset.FileCreatedAt.Time.UTC(),
				Duration:                        asset.Duration,
				SourceUpdatedAt:                 updatedAt,
				UpstreamChecksumAlgorithm:       checksumAlgorithm,
				ContentSHA1Hex:                  checksumHex,
				ContentSizeBytes:                contentSizeBytes,
				CanonicalContentSHA1Hex:         canonicalContentSHA1Hex,
				CanonicalContentSizeBytes:       canonicalContentSizeBytes,
				MappedLocalSourceKey:            mappedLocal.LocalSourceKey,
				MappedLocalRootKey:              mappedLocal.LocalRootKey,
				MappedLocalRelativePath:         mappedLocalRelativePath,
				ExternalContentIdentityScopeKey: externalContentIdentityScopeKey,
				IsFavorite:                      asset.IsFavorite,
				City:                            city,
				State:                           state,
				Country:                         country,
				PlaceLabel:                      mirrorPlaceLabel(city, state, country),
				Description:                     description,
			})
			if detailLimit > 0 && !mirrorAssetHasRichMetadata(assets[len(assets)-1]) {
				detail, detailErr := s.fetchImmichMirrorAssetDetail(ctx, datasource, asset.ID)
				if detailErr != nil {
					return nil, detailErr
				}
				detailLimit--
				if !detail.ShouldMirror() {
					assets = assets[:len(assets)-1]
					continue
				}
				enrichMirrorAssetFromImmichAsset(&assets[len(assets)-1], detail)
			}
			if latestAssetLimit > 0 && len(assets) >= latestAssetLimit {
				return assets, nil
			}
		}
		if envelope.Assets.NextPage == nil {
			return assets, nil
		}
		if *envelope.Assets.NextPage <= page {
			return assets, nil
		}
	}
}

func (s *Service) fetchImmichMirrorAssetDetail(ctx context.Context, datasource *config.DatasourceConfig, upstreamAssetID string) (immichAsset, error) {
	upstreamAssetID = strings.TrimSpace(upstreamAssetID)
	if upstreamAssetID == "" {
		return immichAsset{}, ErrInvalidSearchRequest
	}
	request, err := s.newRequestForDatasource(datasource, http.MethodGet, "/api/assets/"+url.PathEscape(upstreamAssetID), nil)
	if err != nil {
		return immichAsset{}, err
	}
	request = request.WithContext(ctx)
	response, err := s.client.Do(request)
	if err != nil {
		return immichAsset{}, fmt.Errorf("perform immich asset detail request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return immichAsset{}, fmt.Errorf("immich asset detail returned status %d", response.StatusCode)
	}
	var asset immichAsset
	if err := json.NewDecoder(response.Body).Decode(&asset); err != nil {
		return immichAsset{}, fmt.Errorf("decode immich asset detail response: %w", err)
	}
	return asset, nil
}

func mirrorAssetHasRichMetadata(asset ImmichMirrorAsset) bool {
	hasContentIdentity := strings.TrimSpace(asset.CanonicalContentSHA1Hex) != "" && asset.CanonicalContentSizeBytes > 0
	hasRichMetadata := asset.IsFavorite ||
		strings.TrimSpace(asset.City) != "" ||
		strings.TrimSpace(asset.State) != "" ||
		strings.TrimSpace(asset.Country) != "" ||
		strings.TrimSpace(asset.Description) != ""
	return hasContentIdentity && hasRichMetadata
}

func enrichMirrorAssetFromImmichAsset(target *ImmichMirrorAsset, asset immichAsset) {
	if target == nil {
		return
	}
	if mediaType := normalizeAssetType(asset.Type); mediaType != "" {
		target.MediaType = mediaType
	}
	if filename := strings.TrimSpace(asset.OriginalFileName); filename != "" {
		target.Filename = filename
	}
	if !asset.FileCreatedAt.IsZero() {
		target.CapturedAt = asset.FileCreatedAt.Time.UTC()
	}
	if asset.Duration != nil {
		target.Duration = asset.Duration
	}
	if asset.UpdatedAt != nil && !asset.UpdatedAt.IsZero() {
		value := asset.UpdatedAt.Time.UTC()
		target.SourceUpdatedAt = &value
	}
	checksumAlgorithm, checksumHex := asset.UpstreamChecksumIdentity()
	target.UpstreamChecksumAlgorithm = checksumAlgorithm
	target.ContentSHA1Hex = checksumHex
	target.ContentSizeBytes = 0
	if checksumAlgorithm == upstreamChecksumAlgorithmSHA1 {
		target.ContentSizeBytes = asset.ContentSizeBytes()
	}
	target.CanonicalContentSHA1Hex, target.CanonicalContentSizeBytes = asset.ContentIdentity()
	target.IsFavorite = asset.IsFavorite
	city, state, country, description := asset.LocationMetadata()
	target.City = city
	target.State = state
	target.Country = country
	target.PlaceLabel = mirrorPlaceLabel(city, state, country)
	target.Description = description
}

func mirrorPlaceLabel(parts ...string) string {
	values := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key := strings.ToLower(part)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		values = append(values, part)
	}
	return strings.Join(values, ", ")
}

// CatalogPage preserves internal callers while the public API moves to SearchAssets.
func (s *Service) CatalogPage(pageIndex int, pageSize int) (AssetSearchPage, error) {
	return s.SearchAssets(AssetSearchRequest{
		Collection: AssetCollectionRequest{Kind: CollectionKindTimeline},
		Page: AssetSearchPageRequest{
			Index: pageIndex,
			Size:  pageSize,
		},
	})
}

func searchBoundary(page AssetSearchPageRequest, itemCount int) *AssetSearchBoundary {
	if page.Index <= 0 || itemCount > 0 {
		return nil
	}
	return &AssetSearchBoundary{Kind: BoundaryPastEnd}
}

func normalizeAssetSearchRequest(searchRequest AssetSearchRequest) (normalizedAssetSearch, error) {
	request := searchRequest
	request.Collection.Kind = strings.TrimSpace(request.Collection.Kind)
	if request.Collection.Kind == "" {
		request.Collection.Kind = CollectionKindTimeline
	}
	if request.Collection.Kind != CollectionKindTimeline && request.Collection.Kind != CollectionKindSearch {
		return normalizedAssetSearch{}, fmt.Errorf("%w: unsupported collection kind", ErrInvalidSearchRequest)
	}

	if request.Page.Index < 0 {
		return normalizedAssetSearch{}, fmt.Errorf("%w: page index must be non-negative", ErrInvalidSearchRequest)
	}
	if request.Page.Size == 0 {
		request.Page.Size = defaultPageSize
	}
	if request.Page.Size < 1 {
		return normalizedAssetSearch{}, fmt.Errorf("%w: page size must be positive", ErrInvalidSearchRequest)
	}
	if request.Page.Size > maxPageSize {
		request.Page.Size = maxPageSize
	}
	if request.Page.Index > (math.MaxInt-request.Page.Size)/request.Page.Size {
		return normalizedAssetSearch{}, fmt.Errorf("%w: page offset is too large", ErrInvalidSearchRequest)
	}

	mediaTypes, err := normalizeMediaTypes(request.Collection.Filters.MediaTypes)
	if err != nil {
		return normalizedAssetSearch{}, err
	}
	request.Collection.Filters.MediaTypes = mediaTypes
	if request.Collection.Filters.CapturedAt != nil {
		from := request.Collection.Filters.CapturedAt.From
		to := request.Collection.Filters.CapturedAt.To
		if from != nil {
			utc := from.UTC()
			request.Collection.Filters.CapturedAt.From = &utc
		}
		if to != nil {
			utc := to.UTC()
			request.Collection.Filters.CapturedAt.To = &utc
		}
		if request.Collection.Filters.CapturedAt.From != nil &&
			request.Collection.Filters.CapturedAt.To != nil &&
			!request.Collection.Filters.CapturedAt.From.Before(*request.Collection.Filters.CapturedAt.To) {
			return normalizedAssetSearch{}, fmt.Errorf("%w: capturedAt from must be before to", ErrInvalidSearchRequest)
		}
	}

	var text string
	queryMode := QueryModeNone
	if request.Collection.Query != nil {
		text = strings.TrimSpace(request.Collection.Query.Text)
		mode := strings.TrimSpace(request.Collection.Query.Mode)
		if mode == "" {
			mode = QueryModeAuto
		}
		request.Collection.Query.Text = text
		request.Collection.Query.Mode = mode
		switch mode {
		case QueryModeAuto:
			if text != "" {
				queryMode = QueryModeSemantic
			}
		case QueryModeSemantic, QueryModeFilename:
			if text == "" {
				return normalizedAssetSearch{}, fmt.Errorf("%w: query text is required", ErrInvalidSearchRequest)
			}
			queryMode = mode
		default:
			return normalizedAssetSearch{}, fmt.Errorf("%w: unsupported query mode", ErrInvalidSearchRequest)
		}
	}
	if request.Collection.Kind == CollectionKindTimeline && queryMode != QueryModeNone {
		return normalizedAssetSearch{}, fmt.Errorf("%w: timeline collection does not accept query text", ErrInvalidSearchRequest)
	}

	resolvedSort := AssetSearchSort{Field: SortFieldCapturedAt, Direction: SortDirectionDesc}
	if queryMode == QueryModeSemantic {
		resolvedSort.Field = SortFieldRelevance
	}
	if request.Collection.Sort != nil {
		field := strings.TrimSpace(request.Collection.Sort.Field)
		direction := strings.TrimSpace(request.Collection.Sort.Direction)
		if field == "" {
			field = resolvedSort.Field
		}
		if direction == "" {
			direction = SortDirectionDesc
		}
		if direction != SortDirectionDesc || field != resolvedSort.Field {
			return normalizedAssetSearch{}, fmt.Errorf("%w: unsupported sort", ErrUnsupportedSearch)
		}
		request.Collection.Sort = &AssetSearchSort{Field: field, Direction: direction}
	} else {
		request.Collection.Sort = &resolvedSort
	}

	resolved := AssetSearchResolved{
		CollectionKind: request.Collection.Kind,
		QueryMode:      queryMode,
		Sort:           resolvedSort,
		TimelineLike:   queryMode != QueryModeSemantic,
	}
	key, err := collectionKey(request, resolved)
	if err != nil {
		return normalizedAssetSearch{}, err
	}
	return normalizedAssetSearch{Request: request, Resolved: resolved, CollectionKey: key}, nil
}

func normalizeMediaTypes(values []string) ([]string, error) {
	seen := map[string]struct{}{}
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "":
		case "image":
			seen["image"] = struct{}{}
		case "video":
			seen["video"] = struct{}{}
		default:
			return nil, fmt.Errorf("%w: unsupported media type", ErrInvalidSearchRequest)
		}
	}
	result := make([]string, 0, len(seen))
	for _, value := range []string{"image", "video"} {
		if _, ok := seen[value]; ok {
			result = append(result, value)
		}
	}
	return result, nil
}

func parseUTCSearchTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasSuffix(value, "Z") {
		return time.Time{}, fmt.Errorf("%w: capturedAt filters require UTC RFC3339 timestamps", ErrInvalidSearchRequest)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: capturedAt filters require UTC RFC3339 timestamps", ErrInvalidSearchRequest)
	}
	return parsed.UTC(), nil
}

func collectionKey(request AssetSearchRequest, resolved AssetSearchResolved) (string, error) {
	canonical := struct {
		Collection AssetCollectionRequest `json:"collection"`
		Resolved   AssetSearchResolved    `json:"resolved"`
		Version    int                    `json:"version"`
	}{
		Collection: request.Collection,
		Resolved:   resolved,
		Version:    1,
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "search_v1:" + base64.RawURLEncoding.EncodeToString(sum[:16]), nil
}

func normalizeAssetType(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "VIDEO":
		return "video"
	default:
		return "image"
	}
}

// SearchCapabilities returns the search features supported by the active datasource.
func (s *Service) SearchCapabilities() AssetSearchCapabilities {
	queryModes := []string{}
	sorts := []AssetSearchSortCapability{
		{Field: SortFieldCapturedAt, Directions: []string{SortDirectionDesc}},
	}
	totalAccuracy := []string{TotalAccuracyExact}
	state := s.datasourceStateSnapshot()
	if state != nil && state.primary != nil && config.IsImmichPassthroughDatasourceKind(state.primary.Kind) {
		queryModes = []string{QueryModeAuto, QueryModeSemantic, QueryModeFilename}
		sorts = append(sorts, AssetSearchSortCapability{
			Field:      SortFieldRelevance,
			Directions: []string{SortDirectionDesc},
		})
		totalAccuracy = []string{TotalAccuracyExact, TotalAccuracyEstimated, TotalAccuracyLowerBound}
		return AssetSearchCapabilities{
			QueryModes: queryModes,
			Filters: AssetSearchFilterCapabilities{
				MediaTypes: []string{"image", "video"},
				CapturedAt: true,
			},
			Sorts:         sorts,
			TotalAccuracy: totalAccuracy,
			Page:          AssetSearchPageCapabilities{MaxSize: maxPageSize},
		}
	}
	if s.catalogStoreEnabled() {
		ctx := context.Background()
		profile := s.semanticSearchProfile(ctx, AssetSearchOptions{})
		queryModes = []string{QueryModeAuto, QueryModeSemantic, QueryModeFilename}
		sorts = append(sorts, AssetSearchSortCapability{
			Field:      SortFieldRelevance,
			Directions: []string{SortDirectionDesc},
		})
		totalAccuracy = []string{TotalAccuracyExact, TotalAccuracyEstimated, TotalAccuracyLowerBound}
		return AssetSearchCapabilities{
			QueryModes: queryModes,
			Filters: AssetSearchFilterCapabilities{
				MediaTypes: []string{"image", "video"},
				CapturedAt: true,
			},
			Sorts:         sorts,
			TotalAccuracy: totalAccuracy,
			Page:          AssetSearchPageCapabilities{MaxSize: maxPageSize},
			Semantic:      s.catalogSemanticCapabilities(ctx, profile),
		}
	}
	return AssetSearchCapabilities{
		QueryModes: queryModes,
		Filters: AssetSearchFilterCapabilities{
			MediaTypes: []string{"image", "video"},
			CapturedAt: true,
		},
		Sorts:         sorts,
		TotalAccuracy: totalAccuracy,
		Page:          AssetSearchPageCapabilities{MaxSize: maxPageSize},
	}
}

// Preview returns the lightweight image profile from the configured datasource.
func (s *Service) Preview(clientRequest *http.Request, assetID string) (*UpstreamMediaResponse, error) {
	return s.PreviewFromSource(clientRequest, "", assetID)
}

// PreviewFromSource returns the lightweight image profile for one datasource asset.
func (s *Service) PreviewFromSource(clientRequest *http.Request, sourceKey string, assetID string) (*UpstreamMediaResponse, error) {
	state, datasource, err := s.datasourceForMedia(sourceKey)
	if err != nil {
		return nil, err
	}
	if datasource.Kind == config.DatasourceKindStaticDemo {
		if state.staticDemoErr != nil {
			return nil, state.staticDemoErr
		}
		if state.staticDemo == nil {
			return nil, ErrNoDatasourceConfigured
		}
		return state.staticDemo.MediaResponse(clientRequest, assetID, "preview")
	}
	profile := hostedImageProfile{
		Name:           "preview",
		UpstreamSize:   previewSize,
		MaxEdgePixels:  previewMaxEdgePixels,
		MaxBytes:       previewMaxBytes,
		JPEGQualities:  previewJPEGQualities,
		FileNameBase:   "preview",
		FileNameSuffix: "_preview",
	}
	if datasource.Kind == config.DatasourceKindLocalFiles {
		return s.localRenditionMediaResponseWithImmichFallback(clientRequest, datasource, assetID, localRenditionKindPreview, profile)
	}
	return s.profileImageForDatasource(clientRequest, datasource, assetID, profile)
}

// DetailPreview returns the detail-preview image profile from the configured datasource.
func (s *Service) DetailPreview(clientRequest *http.Request, assetID string) (*UpstreamMediaResponse, error) {
	return s.DetailPreviewFromSource(clientRequest, "", assetID)
}

// DetailPreviewFromSource returns the detail-preview image profile for one datasource asset.
func (s *Service) DetailPreviewFromSource(clientRequest *http.Request, sourceKey string, assetID string) (*UpstreamMediaResponse, error) {
	state, datasource, err := s.datasourceForMedia(sourceKey)
	if err != nil {
		return nil, err
	}
	if datasource.Kind == config.DatasourceKindStaticDemo {
		if state.staticDemoErr != nil {
			return nil, state.staticDemoErr
		}
		if state.staticDemo == nil {
			return nil, ErrNoDatasourceConfigured
		}
		return state.staticDemo.MediaResponse(clientRequest, assetID, "detail_preview")
	}
	profile := hostedImageProfile{
		Name:           "detail_preview",
		UpstreamSize:   detailPreviewSize,
		MaxEdgePixels:  detailPreviewMaxEdgePixels,
		MaxBytes:       detailPreviewMaxBytes,
		JPEGQualities:  detailPreviewJPEGQualities,
		FileNameBase:   "detail_preview",
		FileNameSuffix: "_detail_preview",
	}
	if datasource.Kind == config.DatasourceKindLocalFiles {
		return s.localRenditionMediaResponseWithImmichFallback(clientRequest, datasource, assetID, localRenditionKindDetailPreview, profile)
	}
	return s.profileImageForDatasource(clientRequest, datasource, assetID, profile)
}

func (s *Service) localRenditionMediaResponseWithImmichFallback(
	clientRequest *http.Request,
	datasource *config.DatasourceConfig,
	assetID string,
	kind string,
	profile hostedImageProfile,
) (*UpstreamMediaResponse, error) {
	response, err := s.localRenditionMediaResponse(clientRequest, datasource, assetID, kind)
	if err == nil || !errors.Is(err, ErrAssetNotFound) {
		return response, err
	}
	if !config.LocalDatasourceImmichFallbackEnabled(*datasource) {
		return nil, err
	}
	fallback, ok, fallbackErr := s.immichDuplicateProfileImageFallback(clientRequest, datasource.SourceKey, assetID, profile)
	if fallbackErr != nil {
		return nil, fallbackErr
	}
	if ok {
		return fallback, nil
	}
	return nil, err
}

func (s *Service) immichDuplicateProfileImageFallback(
	clientRequest *http.Request,
	localSourceKey string,
	assetID string,
	profile hostedImageProfile,
) (*UpstreamMediaResponse, bool, error) {
	if s == nil || s.catalog == nil {
		return nil, false, nil
	}
	sources, err := s.catalog.activeImmichSourcesForCanonicalAsset(contextFromRequest(clientRequest), localSourceKey, assetID)
	if err != nil {
		return nil, false, err
	}
	sources, deferredSources := s.eligibleImmichMediaFallbackSources(sources, time.Now().UTC())
	if len(sources) == 0 && deferredSources > 0 {
		return nil, false, fmt.Errorf("%w: duplicate media sources are in retry backoff", ErrDatasourceUnavailable)
	}
	var sourceErr error
	failedSources := map[string]struct{}{}
	for _, source := range sources {
		if _, failed := failedSources[source.SourceKey]; failed {
			continue
		}
		_, datasource, err := s.datasourceForMedia(source.SourceKey)
		if err != nil {
			if errors.Is(err, ErrAssetNotFound) || errors.Is(err, ErrNoDatasourceConfigured) {
				failedSources[source.SourceKey] = struct{}{}
				continue
			}
			return nil, false, err
		}
		if !config.IsImmichDatasourceKind(datasource.Kind) {
			continue
		}
		parentCtx := contextFromRequest(clientRequest)
		sourceCtx, cancel := context.WithTimeout(parentCtx, mediaFallbackSourceTimeout)
		sourceRequest := requestWithContext(clientRequest, sourceCtx)
		response, err := s.profileImageForDatasource(sourceRequest, datasource, source.UpstreamAssetID, profile)
		cancel()
		if err != nil {
			if ctxErr := parentCtx.Err(); ctxErr != nil {
				return nil, false, ctxErr
			}
			if !errors.Is(err, ErrAssetNotFound) {
				sourceErr = err
				if immichMediaFallbackSourceFailure(err) {
					failedSources[source.SourceKey] = struct{}{}
					s.markImmichMediaFallbackFailure(source.SourceKey, time.Now().UTC())
				}
			}
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			response.Body.Close()
			sourceErr = fmt.Errorf("%w: unexpected profile status %d", ErrDatasourceUnavailable, response.StatusCode)
			failedSources[source.SourceKey] = struct{}{}
			s.markImmichMediaFallbackFailure(source.SourceKey, time.Now().UTC())
			continue
		}
		s.clearImmichMediaFallbackFailure(source.SourceKey)
		return response, true, nil
	}
	if sourceErr != nil {
		return nil, false, sourceErr
	}
	return nil, false, nil
}

func immichMediaFallbackSourceFailure(err error) bool {
	return errors.Is(err, ErrDatasourceUnavailable)
}

func requestWithContext(request *http.Request, ctx context.Context) *http.Request {
	if request != nil {
		return request.Clone(ctx)
	}
	return (&http.Request{Header: http.Header{}}).WithContext(ctx)
}

func (s *Service) eligibleImmichMediaFallbackSources(sources []catalogMediaSource, now time.Time) ([]catalogMediaSource, int) {
	if s == nil || len(sources) == 0 {
		return append([]catalogMediaSource(nil), sources...), 0
	}
	s.mu.Lock()
	retry := make(map[string]time.Time, len(s.mediaSourceRetry))
	for sourceKey, notBefore := range s.mediaSourceRetry {
		retry[sourceKey] = notBefore
	}
	s.mu.Unlock()
	eligible := make([]catalogMediaSource, 0, len(sources))
	deferred := 0
	for _, source := range sources {
		if retry[source.SourceKey].After(now) {
			deferred++
			continue
		}
		eligible = append(eligible, source)
	}
	return eligible, deferred
}

func (s *Service) markImmichMediaFallbackFailure(sourceKey string, now time.Time) {
	if s == nil || strings.TrimSpace(sourceKey) == "" {
		return
	}
	s.mu.Lock()
	if s.mediaSourceRetry == nil {
		s.mediaSourceRetry = map[string]time.Time{}
	}
	s.mediaSourceRetry[sourceKey] = now.UTC().Add(mediaFallbackSourceBackoff)
	s.mu.Unlock()
}

func (s *Service) clearImmichMediaFallbackFailure(sourceKey string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.mediaSourceRetry, sourceKey)
	s.mu.Unlock()
}

func (s *Service) profileImage(
	clientRequest *http.Request,
	assetID string,
	profile hostedImageProfile,
) (*UpstreamMediaResponse, error) {
	state := s.datasourceStateSnapshot()
	if state == nil {
		return nil, ErrNoDatasourceConfigured
	}
	return s.profileImageForDatasource(clientRequest, state.primary, assetID, profile)
}

func (s *Service) profileImageForDatasource(
	clientRequest *http.Request,
	datasource *config.DatasourceConfig,
	assetID string,
	profile hostedImageProfile,
) (*UpstreamMediaResponse, error) {
	if datasource == nil {
		return nil, ErrNoDatasourceConfigured
	}
	totalStartedAt := time.Now()
	upstreamStartedAt := time.Now()
	upstreamSize := profile.UpstreamSize
	if upstreamSize == "" {
		upstreamSize = detailPreviewSize
	}
	upstream, err := s.proxyMediaForDatasource(datasource, profileUpstreamRequest(clientRequest), "/api/assets/"+url.PathEscape(assetID)+"/thumbnail?size="+upstreamSize, nil)
	if err != nil {
		return nil, err
	}
	timing := photoDetailTiming{
		UpstreamHeaders: time.Since(upstreamStartedAt),
		Profile:         profile.Name,
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		timing.Total = time.Since(totalStartedAt)
		logPhotoDetailTiming(clientRequest, upstream.StatusCode, false, timing)
		status := upstream.StatusCode
		upstream.Body.Close()
		if status == http.StatusNotFound {
			return nil, ErrAssetNotFound
		}
		return nil, fmt.Errorf("%w: profile upstream status %d", ErrDatasourceUnavailable, status)
	}
	if contentLengthExceeds(upstream.Header.Get("Content-Length"), detailPreviewMaxSource) {
		upstream.Body.Close()
		return nil, ErrMediaTooLarge
	}

	readStartedAt := time.Now()
	body, err := readAtMost(upstream.Body, detailPreviewMaxSource)
	upstream.Body.Close()
	if err != nil {
		if errors.Is(err, ErrMediaTooLarge) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: read hosted media response: %w", ErrDatasourceUnavailable, err)
	}
	timing.ReadOriginal = time.Since(readStartedAt)
	timing.OriginalBytes = len(body)

	encodedBody, ok, resizeTiming, err := renderHostedImage(body, profile)
	timing.Decode = resizeTiming.Decode
	timing.Transform = resizeTiming.Transform
	timing.Encode = resizeTiming.Encode
	timing.SourceWidth = resizeTiming.SourceWidth
	timing.SourceHeight = resizeTiming.SourceHeight
	timing.OutputWidth = resizeTiming.OutputWidth
	timing.OutputHeight = resizeTiming.OutputHeight
	timing.OutputBytes = resizeTiming.OutputBytes
	timing.Format = resizeTiming.Format
	if err != nil {
		return nil, fmt.Errorf("%w: render hosted media: %v", ErrMediaInvalid, err)
	}
	timing.Total = time.Since(totalStartedAt)
	if !ok {
		upstream.Body = io.NopCloser(bytes.NewReader(body))
		upstream.Header.Set("Content-Length", strconv.Itoa(len(body)))
		upstream.Header.Set("Content-Disposition", hostedImageContentDisposition(profile))
		upstream.Header.Set("Server-Timing", photoDetailServerTiming(timing))
		logPhotoDetailTiming(clientRequest, upstream.StatusCode, false, timing)
		return upstream, nil
	}
	timing.OutputBytes = len(encodedBody)

	header := make(http.Header)
	if lastModified := upstream.Header.Get("Last-Modified"); lastModified != "" {
		header.Set("Last-Modified", lastModified)
	}
	header.Set("Content-Disposition", hostedImageContentDisposition(profile))
	header.Set("Content-Type", "image/jpeg")
	header.Set("Content-Length", strconv.Itoa(len(encodedBody)))
	header.Set("Server-Timing", photoDetailServerTiming(timing))
	logPhotoDetailTiming(clientRequest, upstream.StatusCode, true, timing)

	return &UpstreamMediaResponse{
		StatusCode: upstream.StatusCode,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(encodedBody)),
	}, nil
}

// Original proxies an original asset request from the configured datasource.
func (s *Service) Original(clientRequest *http.Request, assetID string) (*UpstreamMediaResponse, error) {
	return s.OriginalFromSource(clientRequest, "", assetID)
}

// OriginalFromSource proxies an original asset request from one datasource.
func (s *Service) OriginalFromSource(clientRequest *http.Request, sourceKey string, assetID string) (*UpstreamMediaResponse, error) {
	state, datasource, err := s.datasourceForMedia(sourceKey)
	if err != nil {
		return nil, err
	}
	if datasource.Kind == config.DatasourceKindStaticDemo {
		if state.staticDemoErr != nil {
			return nil, state.staticDemoErr
		}
		if state.staticDemo == nil {
			return nil, ErrNoDatasourceConfigured
		}
		return state.staticDemo.MediaResponse(clientRequest, assetID, "original")
	}
	if datasource.Kind == config.DatasourceKindLocalFiles {
		return s.localOriginalMediaResponse(clientRequest, datasource, assetID)
	}
	response, err := s.proxyMediaForDatasource(datasource, clientRequest, "/api/assets/"+url.PathEscape(assetID)+"/original", nil)
	if err != nil {
		return nil, err
	}
	return normalizeOriginalUpstreamResponse(response)
}

func normalizeOriginalUpstreamResponse(response *UpstreamMediaResponse) (*UpstreamMediaResponse, error) {
	if response == nil {
		return nil, fmt.Errorf("%w: original upstream response is missing", ErrDatasourceUnavailable)
	}
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return response, nil
	case response.StatusCode == http.StatusNotModified,
		response.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		return response, nil
	case response.StatusCode == http.StatusNotFound:
		response.Body.Close()
		return nil, ErrAssetNotFound
	default:
		status := response.StatusCode
		response.Body.Close()
		return nil, fmt.Errorf("%w: original upstream status %d", ErrDatasourceUnavailable, status)
	}
}

func (s *Service) proxyMedia(clientRequest *http.Request, path string, body io.Reader) (*UpstreamMediaResponse, error) {
	state := s.datasourceStateSnapshot()
	if state == nil {
		return nil, ErrNoDatasourceConfigured
	}
	return s.proxyMediaForDatasource(state.primary, clientRequest, path, body)
}

func (s *Service) proxyMediaForDatasource(datasource *config.DatasourceConfig, clientRequest *http.Request, path string, body io.Reader) (*UpstreamMediaResponse, error) {
	if !s.Ready() {
		return nil, ErrNoDatasourceConfigured
	}
	if datasource == nil {
		return nil, ErrNoDatasourceConfigured
	}

	method := http.MethodGet
	if clientRequest != nil && clientRequest.Method != "" {
		method = clientRequest.Method
	}

	request, err := s.newRequestForDatasource(datasource, method, path, body)
	if err != nil {
		return nil, err
	}
	if clientRequest != nil {
		request = request.WithContext(clientRequest.Context())
	}
	applyProxyRequestHeaders(request, clientRequest)
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: perform media request: %w", ErrDatasourceUnavailable, err)
	}
	return &UpstreamMediaResponse{
		StatusCode: response.StatusCode,
		Header:     response.Header.Clone(),
		Body:       response.Body,
	}, nil
}

func (s *Service) datasourceForMedia(sourceKey string) (*serviceDatasourceState, *config.DatasourceConfig, error) {
	state := s.datasourceStateSnapshot()
	if !state.ready() {
		return nil, nil, ErrNoDatasourceConfigured
	}
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" {
		if state.primary == nil {
			return nil, nil, ErrNoDatasourceConfigured
		}
		return state, state.primary, nil
	}
	if state.datasources != nil {
		if datasource, ok := state.datasources[sourceKey]; ok {
			return state, &datasource, nil
		}
	}
	return state, nil, ErrAssetNotFound
}

func profileUpstreamRequest(clientRequest *http.Request) *http.Request {
	if clientRequest == nil {
		return nil
	}
	upstreamRequest := clientRequest.Clone(clientRequest.Context())
	if upstreamRequest.Method == http.MethodHead {
		upstreamRequest.Method = http.MethodGet
	}
	// Profile responses are new Agent-owned representations. Byte ranges,
	// validators, and content encodings for that representation cannot be
	// applied to the Immich source object before decode and transformation.
	for _, headerName := range []string{
		"Range",
		"If-Range",
		"If-None-Match",
		"If-Modified-Since",
		"Accept-Encoding",
	} {
		upstreamRequest.Header.Del(headerName)
	}
	return upstreamRequest
}

func (s *Service) newRequest(method string, path string, body io.Reader) (*http.Request, error) {
	state := s.datasourceStateSnapshot()
	if state == nil {
		return nil, ErrNoDatasourceConfigured
	}
	return s.newRequestForDatasource(state.primary, method, path, body)
}

func (s *Service) newRequestForDatasource(datasource *config.DatasourceConfig, method string, path string, body io.Reader) (*http.Request, error) {
	if datasource == nil {
		return nil, ErrNoDatasourceConfigured
	}
	baseURL, err := url.Parse(datasource.URL)
	if err != nil {
		return nil, fmt.Errorf("parse datasource URL: %w", err)
	}
	resolvedURL, err := baseURL.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("resolve datasource path: %w", err)
	}
	request, err := http.NewRequest(method, resolvedURL.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create upstream request: %w", err)
	}
	applyDatasourceAuth(request, datasource.AccessToken)
	return request, nil
}

func applyDatasourceAuth(request *http.Request, accessToken string) {
	token := strings.TrimSpace(accessToken)
	if token == "" {
		return
	}
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		request.Header.Set("Authorization", token)
		return
	}
	request.Header.Set("x-api-key", token)
}

func applyProxyRequestHeaders(upstreamRequest *http.Request, clientRequest *http.Request) {
	if clientRequest == nil {
		return
	}
	for _, headerName := range []string{
		"Range",
		"If-Range",
		"If-None-Match",
		"If-Modified-Since",
		"Accept",
		"Accept-Encoding",
	} {
		if headerValue := clientRequest.Header.Get(headerName); headerValue != "" {
			upstreamRequest.Header.Set(headerName, headerValue)
		}
	}
}

func isHostedRequest(clientRequest *http.Request) bool {
	if clientRequest == nil {
		return false
	}
	return strings.TrimSpace(clientRequest.Header.Get("X-Timich-Hosted-Base-URL")) != ""
}

func photoDetailServerTiming(timing photoDetailTiming) string {
	parts := []string{
		serverTimingMetric("immich_headers", timing.UpstreamHeaders),
		serverTimingMetric("read_original", timing.ReadOriginal),
		serverTimingMetric("decode", timing.Decode),
		serverTimingMetric("transform", timing.Transform),
		serverTimingMetric("encode", timing.Encode),
		serverTimingMetric("total", timing.Total),
	}
	return strings.Join(parts, ", ")
}

func serverTimingMetric(name string, duration time.Duration) string {
	return fmt.Sprintf("%s;dur=%.1f", name, float64(duration.Microseconds())/1000)
}

func logPhotoDetailTiming(
	clientRequest *http.Request,
	statusCode int,
	resized bool,
	timing photoDetailTiming,
) {
	log.Printf(
		"hosted media timing profile=%q hosted=%t status=%d resized=%t format=%q original_bytes=%d original_mib=%.2f output_bytes=%d output_kib=%.1f compression_ratio=%.3f src=%dx%d dst=%dx%d immich_headers_ms=%d read_source_ms=%d decode_ms=%d transform_ms=%d encode_ms=%d total_ms=%d",
		timing.Profile,
		isHostedRequest(clientRequest),
		statusCode,
		resized,
		timing.Format,
		timing.OriginalBytes,
		bytesToMiB(timing.OriginalBytes),
		timing.OutputBytes,
		bytesToKiB(timing.OutputBytes),
		compressionRatio(timing.OutputBytes, timing.OriginalBytes),
		timing.SourceWidth,
		timing.SourceHeight,
		timing.OutputWidth,
		timing.OutputHeight,
		timing.UpstreamHeaders.Milliseconds(),
		timing.ReadOriginal.Milliseconds(),
		timing.Decode.Milliseconds(),
		timing.Transform.Milliseconds(),
		timing.Encode.Milliseconds(),
		timing.Total.Milliseconds(),
	)
}

func bytesToKiB(value int) float64 {
	return float64(max(0, value)) / 1024
}

func bytesToMiB(value int) float64 {
	return float64(max(0, value)) / (1024 * 1024)
}

func compressionRatio(outputBytes int, originalBytes int) float64 {
	if originalBytes <= 0 {
		return 0
	}
	return float64(max(0, outputBytes)) / float64(originalBytes)
}

func contentLengthExceeds(rawValue string, maxBytes int64) bool {
	contentLength, err := strconv.ParseInt(strings.TrimSpace(rawValue), 10, 64)
	return err == nil && contentLength > maxBytes
}

func readAtMost(reader io.Reader, maxBytes int64) ([]byte, error) {
	var body bytes.Buffer
	if _, err := body.ReadFrom(io.LimitReader(reader, maxBytes+1)); err != nil {
		return nil, err
	}
	if int64(body.Len()) > maxBytes {
		return nil, ErrMediaTooLarge
	}
	return body.Bytes(), nil
}

func renderHostedImage(body []byte, profile hostedImageProfile) ([]byte, bool, photoDetailTiming, error) {
	timing := photoDetailTiming{}
	decodeStartedAt := time.Now()
	srcImage, format, err := image.Decode(bytes.NewReader(body))
	timing.Decode = time.Since(decodeStartedAt)
	timing.Format = format
	if err != nil {
		if profile.ForceJPEG {
			return nil, false, timing, fmt.Errorf("decode image: %w", err)
		}
		if len(body) <= profile.MaxBytes {
			timing.OutputBytes = len(body)
			return body, false, timing, nil
		}
		return nil, false, timing, ErrMediaTooLarge
	}
	if !supportsHostedImageFormat(format) {
		if profile.ForceJPEG {
			return nil, false, timing, fmt.Errorf("unsupported image format %q", format)
		}
		if len(body) <= profile.MaxBytes {
			timing.OutputBytes = len(body)
			return body, false, timing, nil
		}
		return nil, false, timing, ErrMediaTooLarge
	}

	bounds := srcImage.Bounds()
	srcWidth := bounds.Dx()
	srcHeight := bounds.Dy()
	timing.SourceWidth = srcWidth
	timing.SourceHeight = srcHeight
	if srcWidth < 1 || srcHeight < 1 {
		return nil, false, timing, fmt.Errorf("decoded image has invalid bounds %dx%d", srcWidth, srcHeight)
	}

	orientation := 1
	if strings.EqualFold(format, "jpeg") || strings.EqualFold(format, "jpg") {
		orientation = jpegOrientation(body)
	}

	orientedWidth, orientedHeight := orientedDimensions(srcWidth, srcHeight, orientation)
	if !profile.ForceJPEG && max(orientedWidth, orientedHeight) <= profile.MaxEdgePixels && len(body) <= profile.MaxBytes {
		timing.OutputWidth = orientedWidth
		timing.OutputHeight = orientedHeight
		timing.OutputBytes = len(body)
		return body, false, timing, nil
	}

	scale := imageScale(orientedWidth, orientedHeight, profile.MaxEdgePixels)
	dstWidth, dstHeight := scaledDimensions(orientedWidth, orientedHeight, scale)
	timing.OutputWidth = dstWidth
	timing.OutputHeight = dstHeight
	dstImage := image.NewRGBA(image.Rect(0, 0, dstWidth, dstHeight))
	transformStartedAt := time.Now()
	imagedraw.Draw(dstImage, dstImage.Bounds(), &image.Uniform{C: color.White}, image.Point{}, imagedraw.Src)
	if orientation == 1 {
		xdraw.ApproxBiLinear.Scale(dstImage, dstImage.Bounds(), srcImage, bounds, xdraw.Over, nil)
	} else {
		xdraw.ApproxBiLinear.Transform(
			dstImage,
			photoDetailTransform(dstWidth, dstHeight, scale, orientation),
			srcImage,
			bounds,
			xdraw.Over,
			nil,
		)
	}
	timing.Transform = time.Since(transformStartedAt)

	encodeStartedAt := time.Now()
	var encoded bytes.Buffer
	qualities := profile.JPEGQualities
	if len(qualities) == 0 {
		qualities = detailPreviewJPEGQualities
	}
	for _, quality := range qualities {
		encoded.Reset()
		if err := jpeg.Encode(&encoded, dstImage, &jpeg.Options{Quality: quality}); err != nil {
			timing.Encode = time.Since(encodeStartedAt)
			return nil, false, timing, err
		}
		if encoded.Len() <= profile.MaxBytes {
			timing.Encode = time.Since(encodeStartedAt)
			timing.OutputBytes = encoded.Len()
			return encoded.Bytes(), true, timing, nil
		}
	}
	timing.Encode = time.Since(encodeStartedAt)
	timing.OutputBytes = encoded.Len()
	return nil, false, timing, ErrMediaTooLarge
}

type ImageVariantOptions struct {
	MaxEdgePixels int
	MaxBytes      int
	JPEGQualities []int
}

func RenderImageVariant(body []byte, options ImageVariantOptions) ([]byte, error) {
	if options.MaxEdgePixels <= 0 {
		return nil, ErrMediaTooLarge
	}
	if options.MaxBytes <= 0 {
		return nil, ErrMediaTooLarge
	}
	rendered, _, _, err := renderHostedImage(body, hostedImageProfile{
		Name:          "static_demo",
		MaxEdgePixels: options.MaxEdgePixels,
		MaxBytes:      options.MaxBytes,
		JPEGQualities: options.JPEGQualities,
		ForceJPEG:     true,
	})
	return rendered, err
}

func RenderStaticDemoPreview(body []byte) ([]byte, error) {
	return RenderImageVariant(body, ImageVariantOptions{
		MaxEdgePixels: previewMaxEdgePixels,
		MaxBytes:      previewMaxBytes,
		JPEGQualities: previewJPEGQualities,
	})
}

func RenderStaticDemoDetailPreview(body []byte) ([]byte, error) {
	return RenderImageVariant(body, ImageVariantOptions{
		MaxEdgePixels: detailPreviewMaxEdgePixels,
		MaxBytes:      detailPreviewMaxBytes,
		JPEGQualities: detailPreviewJPEGQualities,
	})
}

func RenderStaticDemoOriginal(body []byte) ([]byte, error) {
	return RenderImageVariant(body, ImageVariantOptions{
		MaxEdgePixels: 2400,
		MaxBytes:      2 << 20,
		JPEGQualities: []int{88, 82, 76, 70, 58, 50},
	})
}

func jpegOrientation(body []byte) int {
	if len(body) < 4 || body[0] != 0xFF || body[1] != 0xD8 {
		return 1
	}

	offset := 2
	for offset+4 <= len(body) {
		if body[offset] != 0xFF {
			break
		}
		marker := body[offset+1]
		offset += 2

		if marker == 0xD9 || marker == 0xDA {
			break
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}
		if offset+2 > len(body) {
			break
		}

		segmentLength := int(binary.BigEndian.Uint16(body[offset : offset+2]))
		if segmentLength < 2 || offset+segmentLength > len(body) {
			break
		}
		segmentData := body[offset+2 : offset+segmentLength]
		if marker == 0xE1 {
			if orientation, ok := exifOrientation(segmentData); ok {
				return orientation
			}
		}
		offset += segmentLength
	}

	return 1
}

func exifOrientation(segment []byte) (int, bool) {
	if len(segment) < 14 || !bytes.Equal(segment[:6], []byte("Exif\x00\x00")) {
		return 0, false
	}

	tiff := segment[6:]
	byteOrder := tiff[:2]
	var order binary.ByteOrder
	switch string(byteOrder) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0, false
	}

	if order.Uint16(tiff[2:4]) != 42 {
		return 0, false
	}

	ifdOffset := int(order.Uint32(tiff[4:8]))
	if ifdOffset < 8 || ifdOffset+2 > len(tiff) {
		return 0, false
	}

	entryCount := int(order.Uint16(tiff[ifdOffset : ifdOffset+2]))
	entryOffset := ifdOffset + 2
	for index := 0; index < entryCount; index++ {
		start := entryOffset + index*12
		end := start + 12
		if end > len(tiff) {
			return 0, false
		}

		entry := tiff[start:end]
		tag := order.Uint16(entry[0:2])
		if tag != 0x0112 {
			continue
		}
		fieldType := order.Uint16(entry[2:4])
		componentCount := order.Uint32(entry[4:8])
		if fieldType != 3 || componentCount != 1 {
			return 0, false
		}

		value := int(order.Uint16(entry[8:10]))
		if value < 1 || value > 8 {
			return 0, false
		}
		return value, true
	}

	return 0, false
}

func supportsHostedImageFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpeg", "jpg", "png", "gif", "webp":
		return true
	default:
		return false
	}
}

func imageScale(width int, height int, maxEdgePixels int) float64 {
	maxEdge := max(width, height)
	if maxEdge <= maxEdgePixels {
		return 1
	}
	return float64(maxEdgePixels) / float64(maxEdge)
}

func scaledDimensions(width int, height int, scale float64) (int, int) {
	resizedWidth := max(1, int(float64(width)*scale+0.5))
	resizedHeight := max(1, int(float64(height)*scale+0.5))
	return resizedWidth, resizedHeight
}

func orientedDimensions(width int, height int, orientation int) (int, int) {
	switch orientation {
	case 5, 6, 7, 8:
		return height, width
	default:
		return width, height
	}
}

func photoDetailTransform(dstWidth int, dstHeight int, scale float64, orientation int) f64.Aff3 {
	switch orientation {
	case 2:
		return f64.Aff3{-scale, 0, float64(dstWidth), 0, scale, 0}
	case 3:
		return f64.Aff3{-scale, 0, float64(dstWidth), 0, -scale, float64(dstHeight)}
	case 4:
		return f64.Aff3{scale, 0, 0, 0, -scale, float64(dstHeight)}
	case 5:
		return f64.Aff3{0, scale, 0, scale, 0, 0}
	case 6:
		return f64.Aff3{0, -scale, float64(dstWidth), scale, 0, 0}
	case 7:
		return f64.Aff3{0, -scale, float64(dstWidth), -scale, 0, float64(dstHeight)}
	case 8:
		return f64.Aff3{0, scale, 0, -scale, 0, float64(dstHeight)}
	default:
		return f64.Aff3{scale, 0, 0, 0, scale, 0}
	}
}

func hostedImageContentDisposition(profile hostedImageProfile) string {
	base := strings.ReplaceAll(strings.TrimSpace(profile.FileNameBase), "_", "-")
	if base == "" {
		base = "image"
	}
	return "inline; filename*=UTF-8''" + url.PathEscape(base+".jpg")
}

type searchAssetsEnvelope struct {
	Assets searchAssetsItems `json:"assets"`
}

type searchAssetsItems struct {
	Items    []immichAsset `json:"items"`
	Total    int           `json:"total"`
	NextPage *int          `json:"nextPage,omitempty"`
}

func (s *searchAssetsItems) UnmarshalJSON(data []byte) error {
	type rawSearchAssetsItems struct {
		Items    []immichAsset    `json:"items"`
		Total    json.RawMessage  `json:"total"`
		NextPage *json.RawMessage `json:"nextPage,omitempty"`
	}

	var raw rawSearchAssetsItems
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	total, err := decodeFlexibleInt(raw.Total)
	if err != nil {
		return fmt.Errorf("decode total: %w", err)
	}

	var nextPage *int
	if raw.NextPage != nil {
		decodedNextPage, err := decodeFlexibleInt(*raw.NextPage)
		if err != nil {
			return fmt.Errorf("decode nextPage: %w", err)
		}
		nextPage = &decodedNextPage
	}

	s.Items = raw.Items
	s.Total = total
	s.NextPage = nextPage
	return nil
}

type immichAsset struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	OriginalFileName string          `json:"originalFileName"`
	FileCreatedAt    flexibleTime    `json:"fileCreatedAt"`
	UpdatedAt        *flexibleTime   `json:"updatedAt,omitempty"`
	Duration         *string         `json:"duration,omitempty"`
	IsArchived       bool            `json:"isArchived,omitempty"`
	IsTrashed        bool            `json:"isTrashed,omitempty"`
	IsFavorite       bool            `json:"isFavorite,omitempty"`
	Checksum         string          `json:"checksum,omitempty"`
	OriginalPath     string          `json:"originalPath,omitempty"`
	FileSizeInByte   json.RawMessage `json:"fileSizeInByte,omitempty"`
	FileSize         json.RawMessage `json:"fileSize,omitempty"`
	Visibility       string          `json:"visibility,omitempty"`
	DeletedAt        *flexibleTime   `json:"deletedAt,omitempty"`
	TrashedAt        *flexibleTime   `json:"trashedAt,omitempty"`
	ExifInfo         *immichExif     `json:"exifInfo,omitempty"`
}

func (a immichAsset) ShouldMirror() bool {
	if a.IsArchived || a.IsTrashed {
		return false
	}
	visibility := strings.TrimSpace(strings.ToLower(a.Visibility))
	if visibility != "" && visibility != "timeline" {
		return false
	}
	if a.DeletedAt != nil && !a.DeletedAt.IsZero() {
		return false
	}
	return a.TrashedAt == nil || a.TrashedAt.IsZero()
}

func (a immichAsset) LocationMetadata() (string, string, string, string) {
	if a.ExifInfo == nil {
		return "", "", "", ""
	}
	return strings.TrimSpace(a.ExifInfo.City),
		strings.TrimSpace(a.ExifInfo.State),
		strings.TrimSpace(a.ExifInfo.Country),
		strings.TrimSpace(a.ExifInfo.Description)
}

func (a immichAsset) ContentIdentity() (string, int64) {
	algorithm, sha1Hex := a.UpstreamChecksumIdentity()
	if algorithm != upstreamChecksumAlgorithmSHA1 {
		return "", 0
	}
	return sha1Hex, a.ContentSizeBytes()
}

func (a immichAsset) ContentSizeBytes() int64 {
	sizeBytes := decodeFlexibleInt64Raw(a.FileSizeInByte)
	if sizeBytes <= 0 {
		sizeBytes = decodeFlexibleInt64Raw(a.FileSize)
	}
	if sizeBytes <= 0 && a.ExifInfo != nil {
		sizeBytes = decodeFlexibleInt64Raw(a.ExifInfo.FileSizeInByte)
	}
	return sizeBytes
}

func (a immichAsset) UpstreamChecksumIdentity() (string, string) {
	sha1Hex := normalizeImmichSHA1Checksum(a.Checksum)
	if sha1Hex == "" {
		return upstreamChecksumAlgorithmUnknown, ""
	}
	originalPath := strings.TrimSpace(a.OriginalPath)
	if originalPath != "" && sha1Hex == immichExternalPathChecksumHex(originalPath) {
		return upstreamChecksumAlgorithmSHA1Path, sha1Hex
	}
	return upstreamChecksumAlgorithmSHA1, sha1Hex
}

type immichExif struct {
	City           string          `json:"city,omitempty"`
	State          string          `json:"state,omitempty"`
	Country        string          `json:"country,omitempty"`
	Description    string          `json:"description,omitempty"`
	FileSizeInByte json.RawMessage `json:"fileSizeInByte,omitempty"`
}

type flexibleTime struct {
	time.Time
}

func (f *flexibleTime) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			f.Time = parsed.UTC()
			return nil
		}
	}
	return fmt.Errorf("parse flexible time %q", raw)
}

func decodeFlexibleInt(data json.RawMessage) (int, error) {
	var intValue int
	if err := json.Unmarshal(data, &intValue); err == nil {
		return intValue, nil
	}

	var stringValue string
	if err := json.Unmarshal(data, &stringValue); err == nil {
		parsedValue, err := strconv.Atoi(stringValue)
		if err != nil {
			return 0, err
		}
		return parsedValue, nil
	}

	return 0, fmt.Errorf("unsupported integer payload: %s", string(data))
}

func decodeFlexibleInt64Raw(data json.RawMessage) int64 {
	if len(data) == 0 || string(data) == "null" {
		return 0
	}
	var intValue int64
	if err := json.Unmarshal(data, &intValue); err == nil {
		return intValue
	}
	var floatValue float64
	if err := json.Unmarshal(data, &floatValue); err == nil {
		return int64(floatValue)
	}
	var stringValue string
	if err := json.Unmarshal(data, &stringValue); err == nil {
		parsed, err := strconv.ParseInt(strings.TrimSpace(stringValue), 10, 64)
		if err == nil {
			return parsed
		}
	}
	return 0
}

func normalizeImmichSHA1Checksum(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if normalized := normalizeCatalogSHA1Hex(value); normalized != "" {
		return normalized
	}
	decoders := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, decoder := range decoders {
		decoded, err := decoder.DecodeString(value)
		if err != nil || len(decoded) != 20 {
			continue
		}
		return hex.EncodeToString(decoded)
	}
	return ""
}
