package runtime

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/compatibility"
	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/pairing"
	"github.com/rsahara/timich-agent/internal/security"
	"github.com/rsahara/timich-agent/internal/store"
	"github.com/rsahara/timich-agent/internal/webrtcmedia"
)

const MinAdminTokenLength = 16

var (
	ErrAdminTokenAlreadyConfigured         = errors.New("admin token already configured")
	ErrAdminTokenTooShort                  = errors.New("admin token too short")
	ErrPrimaryDatasourceRequired           = errors.New("primary datasource is required")
	ErrDatasourceAccessTokenNeeded         = errors.New("datasource access token is required")
	ErrDatasourceAlreadyConfigured         = errors.New("datasource already configured")
	ErrDatasourceNotFound                  = errors.New("datasource not found")
	ErrDatasourceDiscoveryAlreadyRunning   = errors.New("datasource discovery already running")
	ErrUploadPolicyInvalid                 = errors.New("upload policy invalid")
	ErrUploadRootNotFound                  = errors.New("upload root not found")
	ErrUploadResetRangeRequired            = errors.New("upload reset range required")
	ErrUploadResetInvalid                  = errors.New("upload reset invalid")
	ErrSemanticCandidateUnavailable        = errors.New("semantic candidate model unavailable")
	ErrSemanticCandidateRuntimeUnavailable = errors.New("semantic candidate runtime unavailable")
	ErrSemanticIndexingIncomplete          = errors.New("semantic indexing incomplete")
	ErrSemanticModelPackMigrating          = errors.New("semantic model pack migration in progress")
)

// AgentRuntime exposes redacted runtime state to the local admin and media APIs.
type AgentRuntime struct {
	mu                              sync.RWMutex
	configMutationMu                sync.Mutex
	build                           BuildInfo
	config                          config.ResolvedConfig
	state                           store.LoadedState
	assetIDKey                      assetIDKeys
	startedAt                       time.Time
	registry                        *store.DeviceRegistryStore
	profiles                        *store.DeviceProfileStore
	uploads                         *store.UploadStore
	uploadMu                        sync.Mutex
	maintenanceMu                   sync.Mutex
	maintenanceCancel               context.CancelFunc
	maintenanceWG                   sync.WaitGroup
	mirrorSyncMu                    sync.Mutex
	datasourceTaskMu                sync.Mutex
	datasourceDiscoveryActive       int
	datasourceTaskActive            map[string]int
	datasourceSnapshot              *DatasourceIndexingResponse
	datasourceSnapshotAt            time.Time
	datasourceSnapshotHash          string
	datasourceSnapshotBusy          bool
	datasourceSnapshotStarted       time.Time
	datasourceSnapshotFinished      time.Time
	datasourceSnapshotInvalid       bool
	datasourceStatusLastAdminAccess time.Time
	datasourceStatusRefreshMu       sync.Mutex
	datasourceStatusCancel          context.CancelFunc
	datasourceStatusWG              sync.WaitGroup
	semanticModelSnapshotMu         sync.Mutex
	semanticModelSnapshot           *catalog.SemanticModelRegistryStatus
	semanticModelSnapshotAt         time.Time
	mirrorSchedulerMu               sync.Mutex
	mirrorCancel                    context.CancelFunc
	mirrorWG                        sync.WaitGroup
	localScanMu                     sync.Mutex
	localDiscovery                  localDiscoverySingleFlight
	localScanSchedulerMu            sync.Mutex
	localScanCancel                 context.CancelFunc
	localScanWG                     sync.WaitGroup
	localScanScheduleReset          chan struct{}
	localVerifyScheduleReset        chan struct{}
	localBackgroundWorkMu           sync.Mutex
	localContentVerificationLast    string
	localBackgroundWorkRetryAt      map[string]time.Time
	localPhase3Mu                   sync.Mutex
	localPhase3Next                 string
	semanticBackfillMu              sync.Mutex
	semanticBackfillCancel          context.CancelFunc
	backgroundWorkerWake            chan struct{}
	backgroundWorkerMu              sync.Mutex
	backgroundWorkerActive          map[string]int
	backgroundWorkerRandom          func() float64
	schedulerWorkStateMu            sync.Mutex
	schedulerWorkState              schedulerWorkState
	schedulerWorkStateSeq           uint64
	semanticBackfillWG              sync.WaitGroup
	semanticWorkMu                  sync.Mutex
	semanticTaskMu                  sync.Mutex
	semanticActiveWorkers           int
	semanticIndexActive             int
	semanticIndexingNextRun         *time.Time
	semanticIndexingRetryNotBefore  *time.Time
	semanticPublishRetryNotBefore   *time.Time
	pairing                         *pairing.Service
	catalog                         *catalog.Service
	semanticModels                  *catalog.SemanticModelPackStore
	semanticRuntimePacks            *catalog.SemanticRuntimePackStore
	semanticPackLifecycleMu         sync.Mutex
	semanticTopologyGeneration      uint64
	semanticONNXRuntime             *semanticONNXRuntimeManager
	webrtc                          *webrtcmedia.Manager
}

type BuildInfo struct {
	Version    string `json:"version"`
	Commit     string `json:"commit,omitempty"`
	BuiltAt    string `json:"builtAt,omitempty"`
	ReleaseTag string `json:"releaseTag,omitempty"`
}

// RemoteBrowsingSummary provides a redacted remote browsing view.
type RemoteBrowsingSummary struct {
	Enabled                 bool       `json:"enabled"`
	ServerURL               string     `json:"serverURL"`
	RegistrationStatus      string     `json:"registrationStatus"`
	RegistrationReady       bool       `json:"registrationReady"`
	RegistrationBlockedBy   []string   `json:"registrationBlockedBy,omitempty"`
	RelayCredentialSyncedAt *time.Time `json:"relayCredentialSyncedAt,omitempty"`
}

// DatasourceSummary provides a redacted datasource view.
type DatasourceSummary struct {
	SourceKey             string `json:"sourceKey"`
	Name                  string `json:"name"`
	Kind                  string `json:"kind"`
	URL                   string `json:"url,omitempty"`
	HasAccessToken        bool   `json:"hasAccessToken"`
	RootKey               string `json:"rootKey,omitempty"`
	RootPath              string `json:"rootPath,omitempty"`
	IndexingEnabled       bool   `json:"indexingEnabled,omitempty"`
	ImmichFallbackEnabled bool   `json:"immichFallbackEnabled"`
}

// PrimaryDatasourceResponse exposes the single datasource the current proxy uses.
type PrimaryDatasourceResponse struct {
	Configured      bool   `json:"configured"`
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	URL             string `json:"url"`
	HasAccessToken  bool   `json:"hasAccessToken"`
	AdditionalCount int    `json:"additionalCount"`
}

// LocalMediaRootSummary exposes administrator-configured read-only media roots.
type LocalMediaRootSummary struct {
	Key                    string `json:"key"`
	Path                   string `json:"path"`
	Status                 string `json:"status,omitempty"`
	Readable               bool   `json:"readable,omitempty"`
	Message                string `json:"message,omitempty"`
	ObservedRootIdentity   string `json:"observedRootIdentity,omitempty"`
	RootAcceptanceRequired bool   `json:"rootAcceptanceRequired,omitempty"`
}

// UploadRootSummary exposes administrator-configured upload roots.
type UploadRootSummary struct {
	Key      string `json:"key"`
	Path     string `json:"path"`
	TempPath string `json:"tempPath,omitempty"`
	Status   string `json:"status,omitempty"`
	Writable bool   `json:"writable,omitempty"`
	Message  string `json:"message,omitempty"`
}

// LocalDatasourceScanResponse reports configured local roots and scan state.
type LocalDatasourceScanResponse struct {
	Roots       []LocalMediaRootSummary             `json:"roots"`
	Datasources []catalog.LocalDatasourceScanStatus `json:"datasources"`
}

// LocalDatasourcePhase0ScanResponse reports a manual local scan kick.
type LocalDatasourcePhase0ScanResponse struct {
	Phase0    []catalog.LocalPhase0ScanResult   `json:"phase0"`
	Metadata  catalog.LocalMetadataBatchResult  `json:"metadata"`
	Thumbnail catalog.LocalThumbnailBatchResult `json:"thumbnail"`
}

type LocalMediaRootAcceptanceResponse struct {
	Acceptance catalog.LocalMediaRootAcceptanceResult `json:"acceptance"`
	Phase0     catalog.LocalPhase0ScanResult          `json:"phase0"`
	ScanStatus string                                 `json:"scanStatus"`
	ScanError  string                                 `json:"scanError,omitempty"`
}

// LocalDatasourceThumbnailRequeueResponse reports failed thumbnails moved back
// to the asynchronous local thumbnail queue.
type LocalDatasourceThumbnailRequeueResponse = catalog.LocalThumbnailRequeueResult

// LocalDatasourceMetadataRequeueResponse reports failed metadata moved back
// to the asynchronous local metadata queue.
type LocalDatasourceMetadataRequeueResponse = catalog.LocalMetadataRequeueResult

// LocalDatasourceEmbeddingRepairResponse reports an explicit local embedding repair kick.
type LocalDatasourceEmbeddingRepairResponse = catalog.SemanticBackfillResult

// DeviceUploadPolicy exposes one paired device upload policy.
type DeviceUploadPolicy struct {
	Enabled       bool       `json:"enabled"`
	RootKey       string     `json:"rootKey,omitempty"`
	PathPattern   string     `json:"pathPattern"`
	CapturedAfter *time.Time `json:"capturedAfter,omitempty"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// DeviceUploadPolicyStatus summarizes whether a device upload policy can run.
type DeviceUploadPolicyStatus struct {
	State  string             `json:"state"`
	Reason string             `json:"reason,omitempty"`
	Root   *UploadRootSummary `json:"root,omitempty"`
}

// DeviceUploadPolicyResponse is the admin view of one device upload policy.
type DeviceUploadPolicyResponse struct {
	DeviceID   string                   `json:"deviceId"`
	DeviceName string                   `json:"deviceName"`
	Upload     DeviceUploadPolicy       `json:"upload"`
	Status     DeviceUploadPolicyStatus `json:"status"`
}

// DeviceUploadPolicyUpdate is an administrator-owned policy update.
type DeviceUploadPolicyUpdate struct {
	Enabled       bool       `json:"enabled"`
	RootKey       string     `json:"rootKey"`
	PathPattern   string     `json:"pathPattern"`
	CapturedAfter *time.Time `json:"capturedAfter,omitempty"`
}

// DeviceUpdate is administrator-owned paired-device metadata.
type DeviceUpdate struct {
	DeviceName string `json:"deviceName"`
}

// UploadResetInput describes an administrator reset request for a device/range.
type UploadResetInput struct {
	DeviceID         string     `json:"deviceId"`
	CapturedAfter    *time.Time `json:"capturedAfter,omitempty"`
	CapturedBefore   *time.Time `json:"capturedBefore,omitempty"`
	Reason           string     `json:"reason,omitempty"`
	RequireDateRange bool       `json:"-"`
}

// UploadResetResponse reports the local state removed by an upload reset.
type UploadResetResponse struct {
	DeviceID              string     `json:"deviceId"`
	CapturedAfter         *time.Time `json:"capturedAfter,omitempty"`
	CapturedBefore        *time.Time `json:"capturedBefore,omitempty"`
	ResetAt               time.Time  `json:"resetAt"`
	RemovedUploadedAssets int64      `json:"removedUploadedAssets"`
	RemovedSessions       int64      `json:"removedSessions"`
	RemovedTempFiles      int64      `json:"removedTempFiles"`
	TempCleanupErrors     []string   `json:"tempCleanupErrors,omitempty"`
}

// DeviceSummary is the admin-safe view of a paired app device.
type DeviceSummary struct {
	DeviceID              string    `json:"deviceId"`
	DeviceName            string    `json:"deviceName"`
	CreatedAt             time.Time `json:"createdAt"`
	LastRefreshedAt       time.Time `json:"lastRefreshedAt"`
	RefreshTokenExpiresAt time.Time `json:"refreshTokenExpiresAt"`
}

// SetupTask summarizes one operator-facing setup prerequisite.
type SetupTask struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
}

// StatusResponse summarizes the runtime state for diagnostics.
type StatusResponse struct {
	Service               string                   `json:"service"`
	Version               string                   `json:"version"`
	Commit                string                   `json:"commit,omitempty"`
	BuiltAt               string                   `json:"builtAt,omitempty"`
	ReleaseTag            string                   `json:"releaseTag,omitempty"`
	Mode                  string                   `json:"mode"`
	AgentID               string                   `json:"agentId"`
	AgentName             string                   `json:"agentName"`
	StartedAt             time.Time                `json:"startedAt"`
	UptimeSeconds         int64                    `json:"uptimeSeconds"`
	ConfigSource          string                   `json:"configSource"`
	ConfigPath            string                   `json:"configPath"`
	DataDir               string                   `json:"dataDir"`
	StatePath             string                   `json:"statePath"`
	Timezone              string                   `json:"timezone,omitempty"`
	AdminListenAddress    string                   `json:"adminListenAddress"`
	MediaListenAddress    string                   `json:"mediaListenAddress"`
	MediaPublishedAddress string                   `json:"mediaPublishedAddress,omitempty"`
	DeviceLimit           int                      `json:"deviceLimit"`
	PairedDeviceCount     int                      `json:"pairedDeviceCount"`
	ActivePairingCount    int                      `json:"activePairingCount"`
	SessionKeyReady       bool                     `json:"sessionKeyReady"`
	AdminAuthReady        bool                     `json:"adminAuthReady"`
	RemoteBrowsing        RemoteBrowsingSummary    `json:"remoteBrowsing"`
	StorageGuardrail      StorageGuardrailResponse `json:"storageGuardrail"`
	MediaRuntime          MediaRuntimeResponse     `json:"mediaRuntime"`
	SemanticRuntime       SemanticRuntimeResponse  `json:"semanticRuntime"`
	WorkerRuntime         WorkerRuntimeResponse    `json:"workerRuntime"`
	Datasources           []DatasourceSummary      `json:"datasources"`
	LocalMediaRoots       []LocalMediaRootSummary  `json:"localMediaRoots"`
	UploadRoots           []UploadRootSummary      `json:"uploadRoots"`
	SetupTasks            []SetupTask              `json:"setupTasks"`
}

// ConfigResponse exposes the current redacted config plus state location.
type ConfigResponse struct {
	AgentName             string                   `json:"agentName"`
	ConfigSource          string                   `json:"configSource"`
	ConfigPath            string                   `json:"configPath"`
	DataDir               string                   `json:"dataDir"`
	StatePath             string                   `json:"statePath"`
	Timezone              string                   `json:"timezone,omitempty"`
	AdminListenAddress    string                   `json:"adminListenAddress"`
	MediaListenAddress    string                   `json:"mediaListenAddress"`
	MediaPublishedAddress string                   `json:"mediaPublishedAddress,omitempty"`
	DeviceLimit           int                      `json:"deviceLimit"`
	AppLinkBaseURL        string                   `json:"appLinkBaseURL"`
	AdminAuthReady        bool                     `json:"adminAuthReady"`
	RemoteBrowsing        RemoteBrowsingSummary    `json:"remoteBrowsing"`
	StorageGuardrail      StorageGuardrailResponse `json:"storageGuardrail"`
	MediaRuntime          MediaRuntimeResponse     `json:"mediaRuntime"`
	SemanticRuntime       SemanticRuntimeResponse  `json:"semanticRuntime"`
	WorkerRuntime         WorkerRuntimeResponse    `json:"workerRuntime"`
	Datasources           []DatasourceSummary      `json:"datasources"`
	LocalMediaRoots       []LocalMediaRootSummary  `json:"localMediaRoots"`
	UploadRoots           []UploadRootSummary      `json:"uploadRoots"`
}

// WorkerRuntimeResponse reports effective and configured background work capacity.
type WorkerRuntimeResponse struct {
	HeavyTaskWorkers           int  `json:"heavyTaskWorkers"`
	ConfiguredHeavyTaskWorkers *int `json:"configuredHeavyTaskWorkers,omitempty"`
	AutoHeavyTaskWorkers       bool `json:"autoHeavyTaskWorkers"`
	PausedHeavyTaskWorkers     bool `json:"pausedHeavyTaskWorkers"`
	HostCPUCount               int  `json:"hostCpuCount"`
	LocalDatasourceWorkers     int  `json:"localDatasourceWorkers"`
	LocalMetadataBatchSize     int  `json:"localMetadataBatchSize"`
	LocalThumbnailBatchSize    int  `json:"localThumbnailBatchSize"`
	SemanticIndexingWorkers    int  `json:"semanticIndexingWorkers"`
}

type MediaRuntimeResponse struct {
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

// InfoResponse is the public LAN-facing metadata summary for the media API.
type InfoResponse struct {
	Service        string                `json:"service"`
	Version        string                `json:"version"`
	Commit         string                `json:"commit,omitempty"`
	BuiltAt        string                `json:"builtAt,omitempty"`
	ReleaseTag     string                `json:"releaseTag,omitempty"`
	AgentID        string                `json:"agentId"`
	AgentName      string                `json:"agentName"`
	Mode           string                `json:"mode"`
	DeviceLimit    int                   `json:"deviceLimit"`
	PairedDevices  int                   `json:"pairedDevices"`
	RemoteBrowsing RemoteBrowsingSummary `json:"remoteBrowsing"`
	Hosted         RemoteBrowsingSummary `json:"hosted"`
	MediaAPI       string                `json:"mediaAPI"`
	PairingStatus  pairing.PairingStatus `json:"pairingStatus"`
}

// NewAgentRuntime builds the redacted runtime view shared by local HTTP surfaces.
func NewAgentRuntime(build BuildInfo, cfg config.ResolvedConfig, state store.LoadedState, startedAt time.Time) (*AgentRuntime, error) {
	normalizedBuild := build.withDefaults()
	if _, err := config.EnsureDatasourceSourceKeys(&cfg.Config); err != nil {
		return nil, err
	}
	assetIDKey, err := deriveAssetIDKeys(state.State.SessionSigningKey)
	if err != nil {
		return nil, err
	}
	registry, err := store.LoadOrCreateDeviceRegistry(cfg.DataDir, cfg.DeviceLimit)
	if err != nil {
		return nil, err
	}
	profiles, err := store.LoadOrCreateDeviceProfileStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	uploads, err := store.LoadOrCreateUploadStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	pairingService, err := pairing.NewService(
		state.State.AgentID,
		cfg.AgentName,
		state.State.SessionSigningKey,
		registry,
	)
	if err != nil {
		_ = uploads.Close()
		return nil, err
	}
	semanticModels, err := catalog.LoadOrCreateSemanticModelPackStoreWithOptions(cfg.DataDir, catalog.SemanticModelPackStoreOptions{
		RuntimeHelperPath: cfg.SemanticRuntime.HelperPath,
	})
	if err != nil {
		_ = uploads.Close()
		return nil, err
	}
	semanticRuntimePacks, err := catalog.LoadOrCreateSemanticRuntimePackStore(cfg.DataDir)
	if err != nil {
		_ = uploads.Close()
		return nil, err
	}
	catalogService, err := catalog.NewServiceWithOptions(cfg.Datasources, catalog.ServiceOptions{
		DataDir:                   cfg.DataDir,
		LocalRoots:                cfg.LocalMediaRoots,
		SemanticModels:            semanticModels,
		MediaHelperPath:           cfg.MediaRuntime.HelperPath,
		MediaVipsPath:             cfg.MediaRuntime.VipsPath,
		MediaFFmpegPath:           cfg.MediaRuntime.FFmpegPath,
		StateWriteCheck:           func() error { return ensureWritesAvailableForPath(cfg.DataDir) },
		EnableDatasourceHotReload: true,
	})
	if err != nil {
		_ = uploads.Close()
		return nil, err
	}
	resetCtx, resetCancel := context.WithTimeout(context.Background(), 5*time.Second)
	resetCount, resetErr := catalogService.ResetRunningLocalScanJobs(resetCtx)
	resetCancel()
	if resetErr != nil && !errors.Is(resetErr, catalog.ErrNoDatasourceConfigured) {
		log.Printf("timich-agent local job recovery failed error=%v", resetErr)
	} else if resetCount > 0 {
		log.Printf("timich-agent local job recovery requeued=%d", resetCount)
	}
	runtime := &AgentRuntime{
		build:                    normalizedBuild,
		config:                   cfg,
		state:                    state,
		assetIDKey:               assetIDKey,
		startedAt:                startedAt.UTC(),
		registry:                 registry,
		profiles:                 profiles,
		uploads:                  uploads,
		pairing:                  pairingService,
		catalog:                  catalogService,
		semanticModels:           semanticModels,
		semanticRuntimePacks:     semanticRuntimePacks,
		localScanScheduleReset:   make(chan struct{}, 1),
		localVerifyScheduleReset: make(chan struct{}, 1),
	}
	catalogService.SetLocalWorkNotifier(func() {
		runtime.schedulerWorkStateMarkDirty()
		runtime.wakeBackgroundWorkerScheduler()
	})
	runtime.garbageCollectSemanticModelCorporaLocked()
	runtime.semanticONNXRuntime = newSemanticONNXRuntimeManager(cfg.SemanticRuntime.ONNXRuntime, semanticModels, semanticRuntimePacks)
	runtime.semanticONNXRuntime.SetStateChangeCallback(func() {
		runtime.schedulerWorkStateMarkDirty()
		runtime.wakeBackgroundWorkerScheduler()
	})
	runtime.webrtc = webrtcmedia.NewManager(runtime.Original)
	return runtime, nil
}

// Close releases runtime-owned local resources. Long-running agent processes
// normally close via process shutdown; tests use this to release SQLite handles.
func (a *AgentRuntime) Close() error {
	if a == nil {
		return nil
	}
	a.stopUploadMaintenance()
	a.stopDatasourceMirrorSync()
	a.stopLocalDatasourceScan()
	a.stopBackgroundWorkerScheduler()
	a.stopDatasourceIndexingStatusRefresh()
	if a.semanticONNXRuntime != nil {
		a.semanticONNXRuntime.Close()
	}
	var err error
	if a.uploads != nil {
		err = errors.Join(err, a.uploads.Close())
	}
	if a.catalog != nil {
		err = errors.Join(err, a.catalog.Close())
	}
	return err
}

func newCompatibilityService(
	build BuildInfo,
	state store.LoadedState,
	cfg config.ResolvedConfig,
	catalogService *catalog.Service,
	registrationReady bool,
	registrationBlockedBy []string,
) *compatibility.Service {
	return compatibility.NewService(
		build.Version,
		state.State.AgentID,
		state.State.RelayKeyID,
		state.State.RelayPrivateKey,
		cfg,
		catalogService,
		compatibility.RelayRegistrationState{
			CredentialSynced: state.State.RelayCredentialSyncedAt != nil,
			Ready:            registrationReady,
			BlockedBy:        registrationBlockedBy,
		},
	)
}

// StatusResponse returns the admin diagnostics summary.
func (a *AgentRuntime) StatusResponse() StatusResponse {
	return a.StatusResponseWithContext(context.Background())
}

// StatusResponseWithContext returns the admin diagnostics summary with bounded helper inspections.
func (a *AgentRuntime) StatusResponseWithContext(ctx context.Context) StatusResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	snapshot := a.registry.Snapshot()
	localMediaRoots := a.localMediaRootSummariesLocked(ctx)
	return StatusResponse{
		Service:               "timich-agent",
		Version:               a.build.Version,
		Commit:                emptyIfUnknown(a.build.Commit),
		BuiltAt:               emptyIfUnknown(a.build.BuiltAt),
		ReleaseTag:            strings.TrimSpace(a.build.ReleaseTag),
		Mode:                  a.modeLocked(),
		AgentID:               a.state.State.AgentID,
		AgentName:             a.config.AgentName,
		StartedAt:             a.startedAt,
		UptimeSeconds:         int64(time.Since(a.startedAt).Seconds()),
		ConfigSource:          a.config.ConfigSource,
		ConfigPath:            a.config.ConfigPath,
		DataDir:               a.config.DataDir,
		StatePath:             a.state.Path,
		Timezone:              a.config.Timezone,
		AdminListenAddress:    a.config.AdminListenAddress,
		MediaListenAddress:    a.config.MediaListenAddress,
		MediaPublishedAddress: a.config.MediaPublishedAddress,
		DeviceLimit:           a.config.DeviceLimit,
		PairedDeviceCount:     len(snapshot.Devices),
		ActivePairingCount:    len(snapshot.PairingSessions),
		SessionKeyReady:       a.state.State.SessionSigningKey != "",
		AdminAuthReady:        a.adminAuthReadyLocked(),
		RemoteBrowsing:        a.remoteBrowsingSummaryLocked(),
		StorageGuardrail:      a.storageGuardrailStatusLocked(),
		MediaRuntime:          a.mediaRuntimeResponseLocked(ctx),
		SemanticRuntime:       a.semanticRuntimeResponseLocked(),
		WorkerRuntime:         a.workerRuntimeResponseLocked(),
		Datasources:           a.datasourceSummariesLocked(),
		LocalMediaRoots:       localMediaRoots,
		UploadRoots:           a.uploadRootSummariesLocked(),
		SetupTasks:            a.setupTasksLocked(snapshot, localMediaRoots),
	}
}

// ConfigResponse returns the redacted live config.
func (a *AgentRuntime) ConfigResponse() ConfigResponse {
	return a.ConfigResponseWithContext(context.Background())
}

func (a *AgentRuntime) ConfigResponseWithContext(ctx context.Context) ConfigResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	localMediaRoots := a.localMediaRootSummariesLocked(ctx)
	return ConfigResponse{
		AgentName:             a.config.AgentName,
		ConfigSource:          a.config.ConfigSource,
		ConfigPath:            a.config.ConfigPath,
		DataDir:               a.config.DataDir,
		StatePath:             a.state.Path,
		Timezone:              a.config.Timezone,
		AdminListenAddress:    a.config.AdminListenAddress,
		MediaListenAddress:    a.config.MediaListenAddress,
		MediaPublishedAddress: a.config.MediaPublishedAddress,
		DeviceLimit:           a.config.DeviceLimit,
		AppLinkBaseURL:        a.config.AppLinkBaseURL,
		AdminAuthReady:        a.adminAuthReadyLocked(),
		RemoteBrowsing:        a.remoteBrowsingSummaryLocked(),
		StorageGuardrail:      a.storageGuardrailStatusLocked(),
		MediaRuntime:          a.mediaRuntimeResponseLocked(ctx),
		SemanticRuntime:       a.semanticRuntimeResponseLocked(),
		WorkerRuntime:         a.workerRuntimeResponseLocked(),
		Datasources:           a.datasourceSummariesLocked(),
		LocalMediaRoots:       localMediaRoots,
		UploadRoots:           a.uploadRootSummariesLocked(),
	}
}

// WorkerRuntimeStatus returns the current effective background worker settings.
func (a *AgentRuntime) WorkerRuntimeStatus() WorkerRuntimeResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.workerRuntimeResponseLocked()
}

// Datasources returns redacted datasource summaries without running helper status probes.
func (a *AgentRuntime) Datasources() []DatasourceSummary {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.datasourceSummariesLocked()
}

func (a *AgentRuntime) mediaRuntimeResponseLocked(ctx context.Context) MediaRuntimeResponse {
	if a == nil || a.catalog == nil {
		return MediaRuntimeResponse{Renderer: "unavailable"}
	}
	status := a.catalog.LocalMediaRuntimeStatusWithContext(ctx)
	return MediaRuntimeResponse{
		Renderer:                     status.Renderer,
		MediaHelperPath:              status.MediaHelperPath,
		MediaHelperAvailable:         status.MediaHelperAvailable,
		MediaHelperAuto:              status.MediaHelperAuto,
		MediaHelperUsable:            status.MediaHelperUsable,
		MediaHelperStatus:            status.MediaHelperStatus,
		MediaHelperVersion:           status.MediaHelperVersion,
		MediaHelperPlatform:          status.MediaHelperPlatform,
		MediaHelperRenderImage:       status.MediaHelperRenderImage,
		MediaHelperRenderVideoPoster: status.MediaHelperRenderVideoPoster,
		MediaHelperInspectImage:      status.MediaHelperInspectImage,
		MediaHelperInspectVideo:      status.MediaHelperInspectVideo,
		MediaHelperLastError:         status.MediaHelperLastError,
		VipsPath:                     status.VipsPath,
		VipsAvailable:                status.VipsAvailable,
		VipsAutoDetected:             status.VipsAutoDetected,
		VipsBundled:                  status.VipsBundled,
		FFmpegPath:                   status.FFmpegPath,
		FFmpegAvailable:              status.FFmpegAvailable,
		FFmpegAuto:                   status.FFmpegAuto,
		FFmpegUsable:                 status.FFmpegUsable,
		FFmpegStatus:                 status.FFmpegStatus,
		FFmpegVersion:                status.FFmpegVersion,
		FFmpegDecoders:               status.FFmpegDecoders,
		FFmpegLastError:              status.FFmpegLastError,
	}
}

func (a *AgentRuntime) semanticRuntimeResponseLocked() SemanticRuntimeResponse {
	if a == nil {
		return SemanticRuntimeResponse{}
	}
	response := SemanticRuntimeResponse{
		HelperPath: a.config.SemanticRuntime.HelperPath,
	}
	if a.semanticONNXRuntime != nil {
		response.ONNXRuntime = a.semanticONNXRuntime.Status()
	}
	return response
}

func (a *AgentRuntime) workerRuntimeResponseLocked() WorkerRuntimeResponse {
	heavyTaskWorkers := effectiveHeavyTaskWorkers(a.config.WorkerRuntime.HeavyTaskWorkers)
	response := WorkerRuntimeResponse{
		HeavyTaskWorkers:           heavyTaskWorkers,
		ConfiguredHeavyTaskWorkers: a.config.WorkerRuntime.HeavyTaskWorkers,
		AutoHeavyTaskWorkers:       a.config.WorkerRuntime.HeavyTaskWorkers == nil,
		PausedHeavyTaskWorkers:     workerRuntimePaused(a.config.WorkerRuntime.HeavyTaskWorkers),
		HostCPUCount:               goruntime.NumCPU(),
		LocalDatasourceWorkers:     0,
		LocalMetadataBatchSize:     0,
		LocalThumbnailBatchSize:    0,
		SemanticIndexingWorkers:    0,
	}
	if localDatasourceScanScheduledForConfig(a.config.Config) && heavyTaskWorkers > 0 {
		response.LocalDatasourceWorkers = heavyTaskWorkers
		response.LocalMetadataBatchSize = localMetadataBatchSizeForWorkers(heavyTaskWorkers)
		response.LocalThumbnailBatchSize = localThumbnailBatchSizeForWorkers(heavyTaskWorkers)
	}
	if semanticIndexingScheduledForConfig(a.config.Config) {
		response.SemanticIndexingWorkers = min(heavyTaskWorkers, 1)
	}
	return response
}

func (a *AgentRuntime) effectiveHeavyTaskWorkers() int {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return effectiveHeavyTaskWorkers(a.config.WorkerRuntime.HeavyTaskWorkers)
}

func effectiveHeavyTaskWorkers(configured *int) int {
	if configured != nil {
		if *configured > 0 {
			return *configured
		}
		return 0
	}
	return autoHeavyTaskWorkersForCPU(goruntime.NumCPU())
}

func autoHeavyTaskWorkersForCPU(cpuCount int) int {
	return max(1, cpuCount/2)
}

func workerRuntimePaused(configured *int) bool {
	return configured != nil && *configured == 0
}

// InfoResponse returns the LAN-facing media API summary.
func (a *AgentRuntime) InfoResponse() InfoResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	snapshot := a.registry.Snapshot()
	remoteBrowsing := a.remoteBrowsingSummaryLocked()
	return InfoResponse{
		Service:        "timich-agent",
		Version:        a.build.Version,
		Commit:         emptyIfUnknown(a.build.Commit),
		BuiltAt:        emptyIfUnknown(a.build.BuiltAt),
		ReleaseTag:     strings.TrimSpace(a.build.ReleaseTag),
		AgentID:        a.publicInfoAgentIDLocked(),
		AgentName:      a.config.AgentName,
		Mode:           a.modeLocked(),
		DeviceLimit:    a.config.DeviceLimit,
		PairedDevices:  len(snapshot.Devices),
		RemoteBrowsing: remoteBrowsing,
		Hosted:         remoteBrowsing,
		MediaAPI:       a.config.MediaListenAddress,
		PairingStatus:  a.pairingStatusLocked(),
	}
}

func (a *AgentRuntime) publicInfoAgentIDLocked() string {
	if a.config.Hosted.Enabled && a.state.State.RelayCredentialSyncedAt == nil {
		return ""
	}
	return a.state.State.AgentID
}

func (b BuildInfo) withDefaults() BuildInfo {
	if b.Version == "" {
		b.Version = "dev"
	}
	if b.Commit == "" {
		b.Commit = "unknown"
	}
	if b.BuiltAt == "" {
		b.BuiltAt = "unknown"
	}
	b.ReleaseTag = strings.TrimSpace(b.ReleaseTag)
	return b
}

func emptyIfUnknown(value string) string {
	if value == "unknown" {
		return ""
	}
	return value
}

func (a *AgentRuntime) modeLocked() string {
	if a.config.Hosted.Enabled {
		return "remote_browsing+local"
	}
	return "local-only"
}

func (a *AgentRuntime) remoteBrowsingSummaryLocked() RemoteBrowsingSummary {
	ready, blockedBy := a.remoteRegistrationReadyLocked(a.registry.Snapshot())
	status := "disabled"
	if a.config.Hosted.Enabled {
		switch {
		case a.state.State.RelayCredentialSyncedAt != nil:
			status = "registered"
		case ready:
			status = "pending"
		default:
			status = "waiting_for_setup"
		}
	}
	return RemoteBrowsingSummary{
		Enabled:                 a.config.Hosted.Enabled,
		ServerURL:               a.config.Hosted.ServerURL,
		RegistrationStatus:      status,
		RegistrationReady:       ready,
		RegistrationBlockedBy:   blockedBy,
		RelayCredentialSyncedAt: a.state.State.RelayCredentialSyncedAt,
	}
}

func (a *AgentRuntime) setupTasksLocked(snapshot store.DeviceRegistry, localMediaRoots []LocalMediaRootSummary) []SetupTask {
	datasourceReady := a.datasourceReadyLocked() && a.localDatasourceRootsReadyLocked(localMediaRoots)
	indexingConfigured := a.datasourceIndexingConfiguredLocked()
	pairedDeviceReady := len(snapshot.Devices) > 0
	remoteReady, remoteBlockedBy := a.remoteRegistrationReadyLocked(snapshot)

	remoteStatus := "pending"
	remoteSummary := "Remote registration will start automatically after setup is complete."
	if !a.config.Hosted.Enabled {
		remoteStatus = "skipped"
		remoteSummary = "Remote browsing is disabled in the agent config."
	} else if a.state.State.RelayCredentialSyncedAt != nil {
		remoteStatus = "complete"
		remoteSummary = "Relay credential is registered with the relay server."
	} else if remoteReady {
		remoteSummary = "Waiting for the relay registration retry loop to complete."
	} else if len(remoteBlockedBy) > 0 {
		remoteSummary = "Waiting for " + strings.Join(remoteBlockedBy, ", ") + "."
	}

	tasks := []SetupTask{
		{
			ID:      "admin_token",
			Label:   "Admin token",
			Status:  completeStatus(a.adminAuthReadyLocked()),
			Summary: boolSummary(a.adminAuthReadyLocked(), "Admin access is protected.", "Create the initial admin token."),
		},
		{
			ID:      "datasource",
			Label:   "Datasources",
			Status:  completeStatus(datasourceReady),
			Summary: boolSummary(datasourceReady, "At least one datasource is configured.", "Add a datasource or resolve the local media root warning."),
		},
		{
			ID:      "paired_device",
			Label:   "Paired device",
			Status:  completeStatus(pairedDeviceReady),
			Summary: boolSummary(pairedDeviceReady, "At least one app device is paired.", "Create a pairing code and pair an app device."),
		},
	}
	if indexingConfigured {
		tasks = append(tasks, a.searchModelSetupTaskLocked())
	}
	tasks = append(tasks, SetupTask{
		ID:      "remote_registration",
		Label:   "Relay connection",
		Status:  remoteStatus,
		Summary: remoteSummary,
	})
	return tasks
}

func (a *AgentRuntime) localDatasourceRootsReadyLocked(roots []LocalMediaRootSummary) bool {
	byKey := make(map[string]LocalMediaRootSummary, len(roots))
	for _, root := range roots {
		byKey[strings.TrimSpace(root.Key)] = root
	}
	for _, datasource := range a.config.Datasources {
		if datasource.Kind != config.DatasourceKindLocalFiles {
			continue
		}
		root, ok := byKey[strings.TrimSpace(datasource.RootKey)]
		if !ok || root.Status != "ready" || !root.Readable {
			return false
		}
	}
	return true
}

func (a *AgentRuntime) searchModelSetupTaskLocked() SetupTask {
	task := SetupTask{
		ID:      "search_model",
		Label:   "Search model",
		Status:  "pending",
		Summary: "Install a semantic search model.",
	}
	if a == nil || a.semanticModels == nil {
		return task
	}
	profile, installed := a.semanticModels.InstalledActiveProfile()
	active := installed
	if !installed {
		profile, installed = a.semanticModels.InstalledCandidateProfile()
	}
	if !installed {
		return task
	}
	if !a.semanticModelRuntimeReadyLocked(profile) {
		task.Summary = "Semantic model is installed, but its runtime is unavailable."
		return task
	}
	task.Status = "complete"
	if active {
		task.Summary = "Semantic search has an active model."
		return task
	}
	if workerRuntimePaused(a.config.WorkerRuntime.HeavyTaskWorkers) {
		task.Summary = "Semantic model and runtime are installed. Indexing is paused."
		return task
	}
	task.Summary = "Semantic model and runtime are installed. Indexing runs in the background; activate it in Search when ready."
	return task
}

func (a *AgentRuntime) datasourceIndexingConfiguredLocked() bool {
	for _, datasource := range a.config.Datasources {
		if datasourceIndexingEnabled(datasource) {
			return true
		}
	}
	return false
}

func (a *AgentRuntime) semanticModelRuntimeReadyLocked(profile catalog.SemanticModelProfileStatus) bool {
	if a == nil || a.semanticONNXRuntime == nil {
		return false
	}
	status := a.semanticONNXRuntime.Status()
	if status.Status != semanticONNXRuntimeStatusRunning {
		return false
	}
	for _, runtime := range status.Runtimes {
		if runtime.ModelID == profile.ModelID &&
			runtime.VectorSpaceID == profile.VectorSpaceID &&
			runtime.Status == semanticONNXRuntimeStatusRunning {
			return true
		}
	}
	return false
}

func completeStatus(complete bool) string {
	if complete {
		return "complete"
	}
	return "pending"
}

func boolSummary(complete bool, completeSummary string, pendingSummary string) string {
	if complete {
		return completeSummary
	}
	return pendingSummary
}

func (a *AgentRuntime) datasourceReadyLocked() bool {
	if len(a.config.Datasources) == 0 {
		return false
	}
	datasource := a.config.Datasources[0]
	if !a.datasourceConfigReadyLocked(datasource) {
		return false
	}
	return a.catalog.Ready()
}

func (a *AgentRuntime) datasourceConfigReadyLocked(datasource config.DatasourceConfig) bool {
	switch strings.TrimSpace(datasource.Kind) {
	case "", config.DatasourceKindImmich, config.DatasourceKindImmichIndexed:
		return strings.TrimSpace(datasource.URL) != "" &&
			strings.TrimSpace(datasource.AccessToken) != ""
	case config.DatasourceKindLocalFiles:
		return strings.TrimSpace(datasource.RootKey) != "" &&
			a.localMediaRootConfiguredLocked(datasource.RootKey)
	case config.DatasourceKindStaticDemo:
		return strings.TrimSpace(datasource.URL) != ""
	default:
		return false
	}
}

func (a *AgentRuntime) localMediaRootConfiguredLocked(rootKey string) bool {
	rootKey = strings.TrimSpace(rootKey)
	if rootKey == "" {
		return false
	}
	for _, root := range a.config.LocalMediaRoots {
		if strings.TrimSpace(root.Key) == rootKey {
			return true
		}
	}
	return false
}

func datasourceRequiresAccessToken(kind string) bool {
	return strings.TrimSpace(kind) == "" || config.IsImmichDatasourceKind(kind)
}

func (a *AgentRuntime) remoteRegistrationReadyLocked(snapshot store.DeviceRegistry) (bool, []string) {
	var blockedBy []string
	if !a.adminAuthReadyLocked() {
		blockedBy = append(blockedBy, "admin token")
	}
	if !a.datasourceReadyLocked() {
		blockedBy = append(blockedBy, "datasource")
	}
	if len(snapshot.Devices) == 0 {
		blockedBy = append(blockedBy, "paired device")
	}
	return len(blockedBy) == 0, blockedBy
}

func (a *AgentRuntime) datasourceSummariesLocked() []DatasourceSummary {
	if len(a.config.Datasources) == 0 {
		return []DatasourceSummary{}
	}

	summaries := make([]DatasourceSummary, 0, len(a.config.Datasources))
	for _, datasource := range a.config.Datasources {
		summary := DatasourceSummary{
			SourceKey:             datasource.SourceKey,
			Name:                  datasource.Name,
			Kind:                  datasource.Kind,
			URL:                   datasource.URL,
			HasAccessToken:        datasource.AccessToken != "",
			RootKey:               datasource.RootKey,
			IndexingEnabled:       datasourceIndexingEnabled(datasource),
			ImmichFallbackEnabled: config.LocalDatasourceImmichFallbackEnabled(datasource),
		}
		if datasource.RootKey != "" {
			for _, root := range a.config.LocalMediaRoots {
				if root.Key == datasource.RootKey {
					summary.RootPath = root.Path
					break
				}
			}
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

func (a *AgentRuntime) localMediaRootSummariesLocked(ctx context.Context) []LocalMediaRootSummary {
	return localMediaRootStatusSummariesWithCatalog(ctx, a.config.LocalMediaRoots, a.catalog)
}

// LocalMediaRootStatuses checks administrator-configured read-only media roots.
func (a *AgentRuntime) LocalMediaRootStatuses() []LocalMediaRootSummary {
	a.mu.RLock()
	roots := append([]config.LocalMediaRootConfig(nil), a.config.LocalMediaRoots...)
	catalogService := a.catalog
	a.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return localMediaRootStatusSummariesWithCatalog(ctx, roots, catalogService)
}

func (a *AgentRuntime) LocalDatasourceScanStatus(ctx context.Context) (LocalDatasourceScanResponse, error) {
	roots := a.LocalMediaRootStatuses()
	catalogService := a.catalogService()
	if catalogService == nil {
		return LocalDatasourceScanResponse{Roots: roots}, catalog.ErrNoDatasourceConfigured
	}
	statuses, err := catalogService.LocalDatasourceScanStatuses(ctx)
	if err != nil {
		return LocalDatasourceScanResponse{Roots: roots}, err
	}
	coverageCtx, cancelCoverage := context.WithTimeout(ctx, localDatasourceEmbeddingStatusTimeout)
	if err := a.enrichLocalDatasourceEmbeddingCoverage(coverageCtx, catalogService, statuses); err != nil {
		cancelCoverage()
		return LocalDatasourceScanResponse{Roots: roots}, err
	}
	cancelCoverage()
	return LocalDatasourceScanResponse{
		Roots:       roots,
		Datasources: statuses,
	}, nil
}

func (a *AgentRuntime) RunLocalDatasourcePhase0Scans(ctx context.Context) (LocalDatasourcePhase0ScanResponse, error) {
	return a.RunLocalDatasourceDiscoveryScans(ctx)
}

// AcceptLocalMediaRoot promotes one explicitly approved replacement root, then
// reports the separate reconciliation outcome while discovery remains single-flight.
func (a *AgentRuntime) AcceptLocalMediaRoot(ctx context.Context, sourceKey string, rootKey string, observedIdentity string) (LocalMediaRootAcceptanceResponse, error) {
	releaseDiscovery, acquireErr := a.localDiscovery.acquire(ctx)
	if acquireErr != nil {
		return LocalMediaRootAcceptanceResponse{}, acquireErr
	}
	defer releaseDiscovery()

	catalogService := a.catalogService()
	if catalogService == nil {
		return LocalMediaRootAcceptanceResponse{}, catalog.ErrNoDatasourceConfigured
	}
	defer func() {
		a.schedulerWorkStateMarkDirty()
		a.wakeBackgroundWorkerScheduler()
	}()
	acceptance, err := catalogService.AcceptLocalMediaRootIdentity(ctx, sourceKey, rootKey, observedIdentity)
	if err != nil {
		return LocalMediaRootAcceptanceResponse{}, err
	}
	a.schedulerWorkStateMarkDirty()
	a.wakeBackgroundWorkerScheduler()
	defer a.notifyLocalScanCompleted()
	clearActive := a.setDatasourceDiscoveryActive(1)
	defer clearActive()
	a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, true, nil)
	phase0, err := catalogService.RunLocalReconciliationScan(ctx, sourceKey)
	if err != nil {
		if strings.TrimSpace(phase0.SourceKey) == "" {
			a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, false, nil)
		} else {
			a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, false, nil, phase0)
		}
		return localMediaRootAcceptanceResponse(acceptance, phase0, err), nil
	}
	a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, false, localPhase0ScanResultsCompletedAt([]catalog.LocalPhase0ScanResult{phase0}), phase0)
	return localMediaRootAcceptanceResponse(acceptance, phase0, nil), nil
}

func localMediaRootAcceptanceResponse(acceptance catalog.LocalMediaRootAcceptanceResult, phase0 catalog.LocalPhase0ScanResult, scanErr error) LocalMediaRootAcceptanceResponse {
	response := LocalMediaRootAcceptanceResponse{
		Acceptance: acceptance,
		Phase0:     phase0,
		ScanStatus: "failed",
	}
	if scanErr != nil {
		response.ScanError = scanErr.Error()
		return response
	}
	response.ScanStatus = strings.TrimSpace(phase0.Status)
	if response.ScanStatus == "" {
		response.ScanStatus = "failed"
	}
	if response.ScanStatus != "completed" {
		response.ScanError = strings.TrimSpace(phase0.LastError)
	}
	return response
}

func (a *AgentRuntime) RunLocalDatasourceDiscoveryScans(ctx context.Context) (LocalDatasourcePhase0ScanResponse, error) {
	releaseDiscovery, acquireErr := a.localDiscovery.acquire(ctx)
	if acquireErr != nil {
		return LocalDatasourcePhase0ScanResponse{}, acquireErr
	}
	defer releaseDiscovery()

	catalogService := a.catalogService()
	if catalogService == nil {
		return LocalDatasourcePhase0ScanResponse{}, catalog.ErrNoDatasourceConfigured
	}
	clearActive := a.setDatasourceDiscoveryActive(1)
	defer clearActive()
	a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, true, nil)
	phase0, err := catalogService.RunLocalReconciliationScans(ctx)
	if err != nil {
		a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, false, nil)
		return LocalDatasourcePhase0ScanResponse{}, err
	}
	a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, false, localPhase0ScanResultsCompletedAt(phase0), phase0...)
	a.schedulerWorkStateMarkDirty()
	a.wakeBackgroundWorkerScheduler()
	a.notifyLocalScanCompleted()
	return LocalDatasourcePhase0ScanResponse{Phase0: phase0}, nil
}

func (a *AgentRuntime) RunLocalDatasourceDiscoveryScan(ctx context.Context, sourceKey string) (LocalDatasourcePhase0ScanResponse, error) {
	return a.runLocalDatasourceDiscoveryScan(ctx, sourceKey, false)
}

func (a *AgentRuntime) runAutomaticLocalDatasourceDiscoveryScan(ctx context.Context, sourceKey string) (LocalDatasourcePhase0ScanResponse, error) {
	return a.runLocalDatasourceDiscoveryScan(ctx, sourceKey, true)
}

func (a *AgentRuntime) runLocalDatasourceDiscoveryScan(ctx context.Context, sourceKey string, automatic bool) (LocalDatasourcePhase0ScanResponse, error) {
	releaseDiscovery, acquireErr := a.localDiscovery.acquire(ctx)
	if acquireErr != nil {
		return LocalDatasourcePhase0ScanResponse{}, acquireErr
	}
	defer releaseDiscovery()

	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" {
		return LocalDatasourcePhase0ScanResponse{}, catalog.ErrNoDatasourceConfigured
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return LocalDatasourcePhase0ScanResponse{}, catalog.ErrNoDatasourceConfigured
	}
	clearActive := a.setDatasourceDiscoveryActive(1)
	defer clearActive()
	a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, true, nil)
	var phase0 catalog.LocalPhase0ScanResult
	var err error
	if automatic {
		phase0, err = runAutomaticLocalPhase0Scan(ctx, catalogService, sourceKey, time.Now().UTC(), a.localScheduleLocation())
	} else {
		phase0, err = catalogService.RunLocalReconciliationScan(ctx, sourceKey)
	}
	if err != nil {
		a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, false, nil)
		return LocalDatasourcePhase0ScanResponse{}, err
	}
	a.rememberDatasourceDiscoveryTaskSnapshot(catalogService, false, localPhase0ScanResultsCompletedAt([]catalog.LocalPhase0ScanResult{phase0}), phase0)
	a.schedulerWorkStateMarkDirty()
	a.wakeBackgroundWorkerScheduler()
	a.notifyLocalScanCompleted()
	return LocalDatasourcePhase0ScanResponse{Phase0: []catalog.LocalPhase0ScanResult{phase0}}, nil
}

func (a *AgentRuntime) LocalPhase0DiagnosticRows(ctx context.Context, sourceKey string) ([]catalog.LocalPhase0DiagnosticRow, error) {
	catalogService := a.catalogService()
	if catalogService == nil {
		return nil, catalog.ErrNoDatasourceConfigured
	}
	return catalogService.LocalPhase0DiagnosticRows(ctx, sourceKey)
}

func (a *AgentRuntime) LocalFailureDiagnosticRows(ctx context.Context, sourceKey string) ([]catalog.LocalFailureDiagnosticRow, error) {
	catalogService := a.catalogService()
	if catalogService == nil {
		return nil, catalog.ErrNoDatasourceConfigured
	}
	return catalogService.LocalFailureDiagnosticRows(ctx, sourceKey)
}

func (a *AgentRuntime) RunLocalDatasourcePhase0Scan(ctx context.Context, sourceKey string) (LocalDatasourcePhase0ScanResponse, error) {
	return a.RunLocalDatasourceDiscoveryScan(ctx, sourceKey)
}

func (a *AgentRuntime) RequeueFailedLocalDatasourceThumbnails(ctx context.Context) (LocalDatasourceThumbnailRequeueResponse, error) {
	a.localScanMu.Lock()
	defer a.localScanMu.Unlock()

	catalogService := a.catalogService()
	if catalogService == nil {
		return LocalDatasourceThumbnailRequeueResponse{}, catalog.ErrNoDatasourceConfigured
	}
	result, err := catalogService.RequeueFailedLocalThumbnails(ctx)
	if err == nil {
		a.schedulerWorkStateMarkDirty()
		a.wakeBackgroundWorkerScheduler()
	}
	return result, err
}

func (a *AgentRuntime) RequeueFailedLocalDatasourceMetadata(ctx context.Context) (LocalDatasourceMetadataRequeueResponse, error) {
	a.localScanMu.Lock()
	defer a.localScanMu.Unlock()

	catalogService := a.catalogService()
	if catalogService == nil {
		return LocalDatasourceMetadataRequeueResponse{}, catalog.ErrNoDatasourceConfigured
	}
	result, err := catalogService.RequeueFailedLocalMetadata(ctx)
	if err == nil {
		a.schedulerWorkStateMarkDirty()
		a.wakeBackgroundWorkerScheduler()
	}
	return result, err
}

func (a *AgentRuntime) RepairLocalDatasourceEmbeddings(ctx context.Context) (LocalDatasourceEmbeddingRepairResponse, error) {
	catalogService := a.catalogService()
	if catalogService == nil {
		return LocalDatasourceEmbeddingRepairResponse{}, catalog.ErrNoDatasourceConfigured
	}
	sourceKeys := catalogService.LocalDatasourceSourceKeys()
	if len(sourceKeys) == 0 {
		return LocalDatasourceEmbeddingRepairResponse{}, catalog.ErrNoDatasourceConfigured
	}
	batchSize := a.localEmbeddingBatchSize()
	if batchSize <= 0 {
		return LocalDatasourceEmbeddingRepairResponse{}, nil
	}
	return a.backfillSemanticModelCandidate(ctx, batchSize, catalog.SemanticModelBackfillOptions{
		Workers:    a.effectiveHeavyTaskWorkers(),
		SourceKeys: sourceKeys,
	})
}

func (a *AgentRuntime) enrichDatasourceEmbeddingCoverage(ctx context.Context, catalogService *catalog.Service, statuses []DatasourceIndexingStatus) error {
	if a == nil || catalogService == nil || len(statuses) == 0 {
		return nil
	}
	profile := a.semanticEmbeddingCoverageProfile(ctx, catalogService)
	return a.enrichDatasourceEmbeddingCoverageWithProfile(ctx, catalogService, statuses, profile)
}

func (a *AgentRuntime) enrichDatasourceEmbeddingCoverageWithProfile(ctx context.Context, catalogService *catalog.Service, statuses []DatasourceIndexingStatus, profile *catalog.SemanticModelProfileStatus) error {
	if a == nil || catalogService == nil || len(statuses) == 0 {
		return nil
	}
	if profile == nil {
		return nil
	}
	for index := range statuses {
		backfill, err := catalogService.SemanticModelBackfillStatusForDatasource(ctx, statuses[index].SourceKey, *profile)
		if err != nil {
			statuses[index].EmbeddingModelID = profile.ModelID
			if errors.Is(err, context.DeadlineExceeded) {
				statuses[index].EmbeddingStatus = "busy"
			} else {
				statuses[index].EmbeddingStatus = datasourceEmbeddingUnavailable
			}
			statuses[index].EmbeddingLastError = err.Error()
			continue
		}
		if backfill == nil {
			continue
		}
		statuses[index].EmbeddingModelID = backfill.ModelID
		statuses[index].EmbeddingStatus = backfill.Status
		statuses[index].EmbeddingEligible = backfill.EligibleAssetCount
		statuses[index].EmbeddingCompleted = backfill.CompletedVectorCount
		statuses[index].EmbeddingIndexed = backfill.IndexedVectorCount
		statuses[index].EmbeddingPendingIndexJobs = backfill.PendingIndexJobCount
		statuses[index].EmbeddingFailedIndexJobs = backfill.FailedIndexJobCount
		statuses[index].EmbeddingLastPublishedAt = backfill.LastPublishedAt
		statuses[index].EmbeddingRemaining = backfill.RemainingVectorCount
	}
	return nil
}

func (a *AgentRuntime) enrichLocalDatasourceEmbeddingCoverage(ctx context.Context, catalogService *catalog.Service, statuses []catalog.LocalDatasourceScanStatus) error {
	if a == nil || catalogService == nil || len(statuses) == 0 {
		return nil
	}
	profile := a.semanticEmbeddingCoverageProfile(ctx, catalogService)
	if profile == nil {
		return nil
	}
	for index := range statuses {
		backfill, err := catalogService.SemanticModelBackfillStatusForDatasource(ctx, statuses[index].SourceKey, *profile)
		if err != nil {
			statuses[index].EmbeddingModelID = profile.ModelID
			statuses[index].EmbeddingStatus = datasourceEmbeddingUnavailable
			statuses[index].EmbeddingMessageCode = datasourceEmbeddingUnavailable
			statuses[index].EmbeddingLastError = err.Error()
			continue
		}
		if backfill == nil {
			continue
		}
		statuses[index].EmbeddingModelID = backfill.ModelID
		statuses[index].EmbeddingVectorSpace = backfill.VectorSpaceID
		statuses[index].EmbeddingStatus = backfill.Status
		statuses[index].EmbeddingMessageCode = backfill.MessageCode
		statuses[index].EmbeddingEligible = backfill.EligibleAssetCount
		statuses[index].EmbeddingCompleted = backfill.CompletedVectorCount
		statuses[index].EmbeddingIndexed = backfill.IndexedVectorCount
		statuses[index].EmbeddingRemaining = backfill.RemainingVectorCount
	}
	return nil
}

func (a *AgentRuntime) semanticEmbeddingCoverageProfile(ctx context.Context, catalogService *catalog.Service) *catalog.SemanticModelProfileStatus {
	if a == nil || catalogService == nil || a.semanticModels == nil {
		return nil
	}
	if installed := a.installedSemanticEmbeddingCoverageProfile(ctx); installed != nil {
		return installed
	}
	status := a.SemanticModelRegistryStatusWithContext(ctx)
	if candidate := semanticCandidateNeedingBackfill(ctx, catalogService, status, a.semanticModels); candidate != nil {
		return candidate
	}
	if candidate := semanticReadySemanticCandidate(ctx, catalogService, status, a.semanticModels); candidate != nil {
		return candidate
	}
	if status.Candidate != nil {
		return status.Candidate
	}
	if status.Active.ProfileKind == catalog.SemanticProfileKindModelPack {
		active := status.Active
		return &active
	}
	return nil
}

func (a *AgentRuntime) installedSemanticEmbeddingCoverageProfile(ctx context.Context) *catalog.SemanticModelProfileStatus {
	if a == nil || a.semanticModels == nil {
		return nil
	}
	var selected *catalog.SemanticModelProfileStatus
	for _, profile := range a.semanticModels.InstalledProfilesWithContext(ctx) {
		if profile.ProfileKind != catalog.SemanticProfileKindModelPack ||
			strings.TrimSpace(profile.ModelID) == "" ||
			strings.TrimSpace(profile.VectorSpaceID) == "" ||
			semanticBackfillRolePriority(profile) == 0 {
			continue
		}
		if !semanticInstalledCoverageProfilePreferred(profile, selected) {
			continue
		}
		candidate := profile
		selected = &candidate
	}
	return selected
}

func semanticInstalledCoverageProfilePreferred(candidate catalog.SemanticModelProfileStatus, selected *catalog.SemanticModelProfileStatus) bool {
	candidatePriority := semanticBackfillRolePriority(candidate)
	if candidatePriority == 0 {
		return false
	}
	if selected == nil {
		return true
	}
	selectedPriority := semanticBackfillRolePriority(*selected)
	if candidatePriority != selectedPriority {
		return candidatePriority > selectedPriority
	}
	candidateInstalledAt := semanticRuntimeCandidateInstalledAt(candidate)
	selectedInstalledAt := semanticRuntimeCandidateInstalledAt(*selected)
	if !candidateInstalledAt.Equal(selectedInstalledAt) {
		return candidateInstalledAt.After(selectedInstalledAt)
	}
	if candidate.ModelID != selected.ModelID {
		return candidate.ModelID > selected.ModelID
	}
	return candidate.VectorSpaceID > selected.VectorSpaceID
}

func localMediaRootStatusSummaries(roots []config.LocalMediaRootConfig) []LocalMediaRootSummary {
	if len(roots) == 0 {
		return []LocalMediaRootSummary{}
	}
	summaries := make([]LocalMediaRootSummary, 0, len(roots))
	for _, root := range roots {
		summaries = append(summaries, localMediaRootStatus(root))
	}
	return summaries
}

func localMediaRootStatusSummariesWithCatalog(ctx context.Context, roots []config.LocalMediaRootConfig, catalogService *catalog.Service) []LocalMediaRootSummary {
	summaries := localMediaRootStatusSummaries(roots)
	if catalogService == nil || len(summaries) == 0 {
		return summaries
	}
	continuity, err := catalogService.LocalMediaRootContinuityStatuses(ctx)
	if err != nil {
		return summaries
	}
	byRootKey := make(map[string]catalog.LocalMediaRootContinuityStatus, len(continuity))
	for _, status := range continuity {
		if status.AcceptanceRequired {
			byRootKey[status.RootKey] = status
		}
	}
	for index := range summaries {
		status, ok := byRootKey[summaries[index].Key]
		if !ok {
			continue
		}
		summaries[index].Status = "blocked"
		summaries[index].Readable = false
		summaries[index].Message = status.Message
		summaries[index].ObservedRootIdentity = status.ObservedRootIdentity
		summaries[index].RootAcceptanceRequired = true
	}
	return summaries
}

func localMediaRootStatus(root config.LocalMediaRootConfig) LocalMediaRootSummary {
	key := strings.TrimSpace(root.Key)
	rootPath := strings.TrimSpace(root.Path)
	summary := LocalMediaRootSummary{
		Key:    key,
		Path:   rootPath,
		Status: "blocked",
	}
	rootStatus, err := catalog.InspectLocalMediaRoot(rootPath)
	if err != nil {
		switch rootStatus {
		case catalog.LocalMediaRootStatusMissing:
			summary.Message = "Local media root path does not exist."
		case catalog.LocalMediaRootStatusNotDirectory:
			summary.Message = "Local media root path is not a directory."
		case catalog.LocalMediaRootStatusUnreadable:
			if errors.Is(err, catalog.ErrLocalMediaRootSymlink) {
				summary.Message = "Local media root path must not be a symbolic link."
			} else {
				summary.Message = "Local media root path is not readable."
			}
		default:
			summary.Message = "Local media root path could not be inspected."
		}
		return summary
	}
	summary.Status = "ready"
	summary.Readable = true
	return summary
}

func (a *AgentRuntime) uploadRootSummariesLocked() []UploadRootSummary {
	if len(a.config.UploadRoots) == 0 {
		return []UploadRootSummary{}
	}
	summaries := make([]UploadRootSummary, 0, len(a.config.UploadRoots))
	for _, root := range a.config.UploadRoots {
		root = normalizedUploadRootConfig(root)
		summaries = append(summaries, UploadRootSummary{
			Key:      root.Key,
			Path:     root.Path,
			TempPath: root.TempPath,
		})
	}
	return summaries
}

// UploadRootStatuses checks administrator-configured upload roots.
func (a *AgentRuntime) UploadRootStatuses() []UploadRootSummary {
	a.mu.RLock()
	roots := append([]config.UploadRootConfig(nil), a.config.UploadRoots...)
	a.mu.RUnlock()

	return uploadRootStatusSummaries(roots)
}

func uploadRootStatusSummaries(roots []config.UploadRootConfig) []UploadRootSummary {
	if len(roots) == 0 {
		return []UploadRootSummary{}
	}
	summaries := make([]UploadRootSummary, 0, len(roots))
	for _, root := range roots {
		summaries = append(summaries, uploadRootStatus(root))
	}
	return summaries
}

func uploadRootStatus(root config.UploadRootConfig) UploadRootSummary {
	root = normalizedUploadRootConfig(root)
	summary := UploadRootSummary{
		Key:      root.Key,
		Path:     root.Path,
		TempPath: root.TempPath,
		Status:   "blocked",
	}
	info, err := os.Stat(root.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			summary.Message = "Upload root path does not exist."
			return summary
		}
		summary.Message = "Upload root path could not be inspected."
		return summary
	}
	if !info.IsDir() {
		summary.Message = "Upload root path is not a directory."
		return summary
	}
	if err := probeWritableDirectory(root.Path, ".timich-upload-root-check-*"); err != nil {
		summary.Message = "Upload root path is not writable."
		return summary
	}
	if err := ensureUploadTempDir(root); err != nil {
		summary.Message = "Upload temp path could not be prepared."
		return summary
	}
	tempDir, err := uploadTempAbsoluteDir(root)
	if err != nil {
		summary.Message = "Upload temp path is invalid."
		return summary
	}
	if err := probeWritableDirectory(tempDir, ".timich-upload-temp-check-*"); err != nil {
		summary.Message = "Upload temp path is not writable."
		return summary
	}
	summary.Status = "ready"
	summary.Writable = true
	summary.Message = "Upload root is writable."
	return summary
}

func probeWritableDirectory(dir string, pattern string) error {
	probe, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return err
	}
	probePath := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(probePath)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}

func (a *AgentRuntime) primaryDatasourceLocked() PrimaryDatasourceResponse {
	if len(a.config.Datasources) == 0 {
		return PrimaryDatasourceResponse{}
	}
	datasource := a.config.Datasources[0]
	return PrimaryDatasourceResponse{
		Configured:      true,
		Name:            datasource.Name,
		Kind:            datasource.Kind,
		URL:             datasource.URL,
		HasAccessToken:  datasource.AccessToken != "",
		AdditionalCount: max(len(a.config.Datasources)-1, 0),
	}
}

// LocalConfigPath returns the current config path relative to the working directory when possible.
func (a *AgentRuntime) LocalConfigPath() string {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if relative, err := filepath.Rel(".", a.config.ConfigPath); err == nil {
		return relative
	}
	return a.config.ConfigPath
}

// PrimaryDatasource returns the datasource currently used by the local media proxy.
func (a *AgentRuntime) PrimaryDatasource() PrimaryDatasourceResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.primaryDatasourceLocked()
}

// SetAdminToken stores the first browser-provided admin token.
func (a *AgentRuntime) SetAdminToken(token string) error {
	normalizedToken := strings.TrimSpace(token)
	if len(normalizedToken) < MinAdminTokenLength {
		return ErrAdminTokenTooShort
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.adminAuthReadyLocked() {
		return ErrAdminTokenAlreadyConfigured
	}
	next := a.state
	next.State.AdminToken = normalizedToken
	if err := store.SaveLoadedState(next); err != nil {
		return err
	}
	a.state = next
	return nil
}

// UpdateRelayCredentialSyncedAt persists the latest relay credential sync time.
func (a *AgentRuntime) UpdateRelayCredentialSyncedAt(syncedAt time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	next := a.state
	next.State.RelayCredentialSyncedAt = &syncedAt
	if err := store.SaveLoadedState(next); err != nil {
		return err
	}
	a.state = next
	return nil
}

// RemoteRegistrationReady reports whether the agent should attempt relay credential registration.
func (a *AgentRuntime) RemoteRegistrationReady() (bool, string) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	ready, blockedBy := a.remoteRegistrationReadyLocked(a.registry.Snapshot())
	if ready {
		return true, ""
	}
	return false, strings.Join(blockedBy, ", ")
}

func cloneDatasourceIndexingConfig(value *config.DatasourceIndexingConfig) *config.DatasourceIndexingConfig {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// UpdatePrimaryDatasource persists and applies the primary Immich datasource.
func (a *AgentRuntime) UpdatePrimaryDatasource(input config.DatasourceConfig) (PrimaryDatasourceResponse, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = "Immich"
	}
	kind := strings.TrimSpace(input.Kind)
	if kind == "" {
		kind = config.DatasourceKindImmich
	}
	nextDatasource := config.DatasourceConfig{
		SourceKey:   strings.TrimSpace(input.SourceKey),
		Name:        name,
		Kind:        kind,
		URL:         strings.TrimSpace(input.URL),
		AccessToken: strings.TrimSpace(input.AccessToken),
		Indexing:    cloneDatasourceIndexingConfig(input.Indexing),
	}
	if nextDatasource.URL == "" {
		return PrimaryDatasourceResponse{}, ErrPrimaryDatasourceRequired
	}
	a.configMutationMu.Lock()
	defer a.configMutationMu.Unlock()

	a.mu.Lock()
	nextConfig := a.config
	nextConfig.Datasources = append([]config.DatasourceConfig(nil), a.config.Datasources...)
	if nextDatasource.SourceKey == "" && len(nextConfig.Datasources) > 0 {
		nextDatasource.SourceKey = nextConfig.Datasources[0].SourceKey
	}
	if nextDatasource.SourceKey == "" {
		sourceKey, err := config.GenerateDatasourceSourceKey()
		if err != nil {
			a.mu.Unlock()
			return PrimaryDatasourceResponse{}, err
		}
		nextDatasource.SourceKey = sourceKey
	}
	if datasourceRequiresAccessToken(nextDatasource.Kind) && nextDatasource.AccessToken == "" && len(nextConfig.Datasources) > 0 {
		nextDatasource.AccessToken = nextConfig.Datasources[0].AccessToken
	}
	if nextDatasource.Kind == config.DatasourceKindImmichIndexed && nextDatasource.Indexing == nil && len(nextConfig.Datasources) > 0 {
		nextDatasource.Indexing = cloneDatasourceIndexingConfig(nextConfig.Datasources[0].Indexing)
	}
	if nextDatasource.Kind == config.DatasourceKindImmich {
		nextDatasource.Indexing = nil
	}
	if datasourceRequiresAccessToken(nextDatasource.Kind) && nextDatasource.AccessToken == "" {
		a.mu.Unlock()
		return PrimaryDatasourceResponse{}, ErrDatasourceAccessTokenNeeded
	}
	if len(nextConfig.Datasources) == 0 {
		nextConfig.Datasources = []config.DatasourceConfig{nextDatasource}
	} else {
		nextConfig.Datasources[0] = nextDatasource
	}
	a.mu.Unlock()
	if err := config.Validate(nextConfig.Config); err != nil {
		return PrimaryDatasourceResponse{}, err
	}

	catalogService := a.catalogService()
	if catalogService == nil {
		return PrimaryDatasourceResponse{}, catalog.ErrNoDatasourceConfigured
	}
	if _, err := config.UpdatePrimaryDatasourceFile(nextConfig.ConfigPath, nextDatasource); err != nil {
		return PrimaryDatasourceResponse{}, err
	}

	a.mirrorSyncMu.Lock()
	catalogService.ReconfigureDatasources(nextConfig.Datasources)
	a.mu.Lock()
	nextConfig.ConfigSource = "file"
	a.config = nextConfig
	a.semanticTopologyGeneration++
	response := a.primaryDatasourceLocked()
	a.mu.Unlock()
	a.mirrorSyncMu.Unlock()
	a.invalidateDatasourceIndexingSnapshot(catalogService)
	if _, ok := a.datasourceMirrorSchedule(); ok {
		a.StartDatasourceMirrorSync()
	} else {
		a.stopDatasourceMirrorSync()
	}
	if _, ok := a.localDatasourceScanSchedule(); ok {
		a.StartLocalDatasourceScan()
	} else {
		a.stopLocalDatasourceScan()
	}
	a.syncBackgroundWorkerScheduler()
	a.reconcileSemanticModelRuntime()
	return response, nil
}

// AddDatasource persists and applies a new datasource without editing existing entries.
func (a *AgentRuntime) AddDatasource(input config.DatasourceConfig) (DatasourceSummary, error) {
	nextDatasource, err := a.normalizeNewDatasource(input)
	if err != nil {
		return DatasourceSummary{}, err
	}
	a.configMutationMu.Lock()
	defer a.configMutationMu.Unlock()

	a.mu.Lock()
	for _, existing := range a.config.Datasources {
		if sameDatasourceTarget(existing, nextDatasource) {
			a.mu.Unlock()
			return DatasourceSummary{}, ErrDatasourceAlreadyConfigured
		}
	}
	nextConfig := a.config
	nextConfig.Datasources = append([]config.DatasourceConfig(nil), a.config.Datasources...)
	nextConfig.Datasources = append(nextConfig.Datasources, nextDatasource)
	a.mu.Unlock()
	if err := config.Validate(nextConfig.Config); err != nil {
		return DatasourceSummary{}, err
	}

	catalogService := a.catalogService()
	if catalogService == nil {
		return DatasourceSummary{}, catalog.ErrNoDatasourceConfigured
	}
	if _, err := config.AddDatasourceFile(nextConfig.ConfigPath, nextDatasource); err != nil {
		return DatasourceSummary{}, err
	}
	nextConfig.ConfigSource = "file"

	a.mirrorSyncMu.Lock()
	catalogService.ReconfigureDatasources(nextConfig.Datasources)
	a.mu.Lock()
	a.config = nextConfig
	a.semanticTopologyGeneration++
	var response DatasourceSummary
	for _, summary := range a.datasourceSummariesLocked() {
		if summary.SourceKey == nextDatasource.SourceKey {
			response = summary
			break
		}
	}
	a.mu.Unlock()
	a.mirrorSyncMu.Unlock()
	a.invalidateDatasourceIndexingSnapshot(catalogService)
	if _, ok := a.datasourceMirrorSchedule(); ok {
		a.StartDatasourceMirrorSync()
	} else {
		a.stopDatasourceMirrorSync()
	}
	if _, ok := a.localDatasourceScanSchedule(); ok {
		started := a.StartLocalDatasourceScan()
		if nextDatasource.Kind == config.DatasourceKindLocalFiles && !started {
			a.triggerLocalPhase0Scan("datasource_added")
		}
	} else {
		a.stopLocalDatasourceScan()
	}
	a.syncBackgroundWorkerScheduler()
	a.reconcileSemanticModelRuntime()
	return response, nil
}

// UpdateLocalDatasourceImmichFallback persists and immediately applies the
// Gallery/media fallback policy for one local datasource.
func (a *AgentRuntime) UpdateLocalDatasourceImmichFallback(sourceKey string, enabled bool) (DatasourceSummary, error) {
	sourceKey = strings.TrimSpace(sourceKey)
	a.configMutationMu.Lock()
	defer a.configMutationMu.Unlock()
	a.mu.Lock()
	nextConfig := a.config
	nextConfig.Datasources = append([]config.DatasourceConfig(nil), a.config.Datasources...)
	found := false
	for index := range nextConfig.Datasources {
		datasource := &nextConfig.Datasources[index]
		if datasource.SourceKey != sourceKey || datasource.Kind != config.DatasourceKindLocalFiles {
			continue
		}
		scan := config.LocalDatasourceScanConfig{}
		if datasource.Scan != nil {
			scan = *datasource.Scan
		}
		enabledCopy := enabled
		scan.ImmichFallbackEnabled = &enabledCopy
		datasource.Scan = &scan
		found = true
		break
	}
	a.mu.Unlock()
	if !found {
		return DatasourceSummary{}, ErrDatasourceNotFound
	}

	catalogService := a.catalogService()
	if catalogService == nil {
		return DatasourceSummary{}, catalog.ErrNoDatasourceConfigured
	}
	if _, err := config.UpdateLocalDatasourceImmichFallbackFile(nextConfig.ConfigPath, sourceKey, enabled); err != nil {
		return DatasourceSummary{}, err
	}

	a.mirrorSyncMu.Lock()
	catalogService.ReconfigureDatasources(nextConfig.Datasources)
	a.mu.Lock()
	nextConfig.ConfigSource = "file"
	a.config = nextConfig
	response := DatasourceSummary{}
	for _, summary := range a.datasourceSummariesLocked() {
		if summary.SourceKey == sourceKey {
			response = summary
			break
		}
	}
	a.mu.Unlock()
	a.mirrorSyncMu.Unlock()
	a.invalidateDatasourceIndexingSnapshot(catalogService)
	return response, nil
}

func sameDatasourceTarget(left config.DatasourceConfig, right config.DatasourceConfig) bool {
	leftKind := strings.TrimSpace(left.Kind)
	rightKind := strings.TrimSpace(right.Kind)
	if config.IsImmichDatasourceKind(leftKind) && config.IsImmichDatasourceKind(rightKind) {
		return strings.TrimSpace(left.URL) != "" && strings.TrimSpace(left.URL) == strings.TrimSpace(right.URL)
	}
	if leftKind != rightKind {
		return false
	}
	switch leftKind {
	case config.DatasourceKindLocalFiles:
		return strings.TrimSpace(left.RootKey) != "" && strings.TrimSpace(left.RootKey) == strings.TrimSpace(right.RootKey)
	default:
		return false
	}
}

func (a *AgentRuntime) normalizeNewDatasource(input config.DatasourceConfig) (config.DatasourceConfig, error) {
	kind := strings.TrimSpace(input.Kind)
	if kind == "" {
		kind = config.DatasourceKindImmich
	}
	name := strings.TrimSpace(input.Name)
	switch kind {
	case config.DatasourceKindImmich, config.DatasourceKindImmichIndexed:
		if name == "" {
			name = "Immich"
		}
		next := config.DatasourceConfig{
			Name:        name,
			Kind:        kind,
			URL:         strings.TrimSpace(input.URL),
			AccessToken: strings.TrimSpace(input.AccessToken),
			Indexing:    input.Indexing,
		}
		if next.URL == "" {
			return config.DatasourceConfig{}, ErrPrimaryDatasourceRequired
		}
		if next.AccessToken == "" {
			return config.DatasourceConfig{}, ErrDatasourceAccessTokenNeeded
		}
		sourceKey, err := config.GenerateDatasourceSourceKey()
		if err != nil {
			return config.DatasourceConfig{}, err
		}
		next.SourceKey = sourceKey
		return next, nil
	case config.DatasourceKindLocalFiles:
		if name == "" {
			name = "Local Files"
		}
		next := config.DatasourceConfig{
			Name:    name,
			Kind:    config.DatasourceKindLocalFiles,
			RootKey: strings.TrimSpace(input.RootKey),
			Scan:    input.Scan,
		}
		if next.RootKey == "" || !a.localRootConfigured(next.RootKey) {
			return config.DatasourceConfig{}, ErrUploadRootNotFound
		}
		sourceKey, err := config.GenerateDatasourceSourceKey()
		if err != nil {
			return config.DatasourceConfig{}, err
		}
		next.SourceKey = sourceKey
		return next, nil
	default:
		return config.DatasourceConfig{}, ErrPrimaryDatasourceRequired
	}
}

func (a *AgentRuntime) localRootConfigured(rootKey string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	rootKey = strings.TrimSpace(rootKey)
	for _, root := range a.config.LocalMediaRoots {
		if root.Key == rootKey {
			return true
		}
	}
	return false
}

// CreatePairingSession issues a local one-time pairing session on the admin surface.
func (a *AgentRuntime) CreatePairingSession() (pairing.PairingSessionResponse, error) {
	return a.pairing.CreatePairingSession()
}

// ActivePairingSession returns the current active pairing session for a code.
func (a *AgentRuntime) ActivePairingSession(code string) (pairing.PairingSessionResponse, error) {
	return a.pairing.ActivePairingSession(code)
}

// CreateNearbyLink starts a LAN-local approval request from an app device.
func (a *AgentRuntime) CreateNearbyLink(deviceName string, deviceKind string) (pairing.NearbyLinkResponse, error) {
	return a.pairing.CreateNearbyLink(deviceName, deviceKind)
}

// NearbyLinks returns active LAN-local approval requests for the admin surface.
func (a *AgentRuntime) NearbyLinks() ([]pairing.NearbyLinkResponse, error) {
	return a.pairing.NearbyLinks()
}

// ApproveNearbyLink approves a pending app device by Link Code.
func (a *AgentRuntime) ApproveNearbyLink(linkCode string) (pairing.NearbyLinkResponse, error) {
	return a.pairing.ApproveNearbyLink(linkCode)
}

// DenyNearbyLink rejects a pending app device approval request.
func (a *AgentRuntime) DenyNearbyLink(linkID string) (pairing.NearbyLinkResponse, error) {
	return a.pairing.DenyNearbyLink(linkID)
}

// CancelNearbyLink cancels a requesting app's Nearby Link using its poll token.
func (a *AgentRuntime) CancelNearbyLink(linkID string, pollToken string) (pairing.NearbyLinkResponse, error) {
	return a.pairing.CancelNearbyLink(linkID, pollToken)
}

// PollNearbyLink returns pending/denied state or consumes an approved request into an app session.
func (a *AgentRuntime) PollNearbyLink(linkID string, pollToken string, baseURL string) (pairing.NearbyLinkPollResponse, error) {
	response, err := a.pairing.PollNearbyLink(linkID, pollToken, baseURL)
	if err != nil || response.Session == nil {
		return response, err
	}
	if err := a.ensureDeviceProfile(response.Session.DeviceID); err != nil {
		_ = a.registry.RevokeDevice(response.Session.DeviceID)
		_ = a.profiles.DeleteProfile(response.Session.DeviceID)
		return pairing.NearbyLinkPollResponse{}, err
	}
	return response, nil
}

// RedeemPairing redeems a one-time pairing code on the LAN-facing media surface.
func (a *AgentRuntime) RedeemPairing(code string, deviceName string, baseURL string) (pairing.SessionBundle, error) {
	bundle, err := a.pairing.RedeemPairing(code, deviceName, baseURL)
	if err != nil {
		return pairing.SessionBundle{}, err
	}
	if err := a.ensureDeviceProfile(bundle.DeviceID); err != nil {
		_ = a.registry.RevokeDevice(bundle.DeviceID)
		_ = a.profiles.DeleteProfile(bundle.DeviceID)
		return pairing.SessionBundle{}, err
	}
	return bundle, nil
}

// CreateHostedSession provisions a remote app session for relay-backed browsing.
func (a *AgentRuntime) CreateHostedSession(deviceName string, baseURL string) (pairing.SessionBundle, error) {
	bundle, err := a.pairing.CreateHostedSession(deviceName, baseURL)
	if err != nil {
		return pairing.SessionBundle{}, err
	}
	if err := a.ensureDeviceProfile(bundle.DeviceID); err != nil {
		_ = a.registry.RevokeDevice(bundle.DeviceID)
		_ = a.profiles.DeleteProfile(bundle.DeviceID)
		return pairing.SessionBundle{}, err
	}
	return bundle, nil
}

// RefreshAppSession rotates an app-device refresh token family and returns a new session bundle.
func (a *AgentRuntime) RefreshAppSession(refreshToken string, baseURL string) (pairing.SessionBundle, error) {
	return a.pairing.RefreshSession(refreshToken, baseURL)
}

// AuthenticateAccessToken verifies the app bearer token used for media/catalog access.
func (a *AgentRuntime) AuthenticateAccessToken(token string) (security.AccessTokenClaims, error) {
	return a.pairing.AuthenticateAccessToken(token)
}

// AdminAuthReady reports whether this runtime has an admin token configured.
func (a *AgentRuntime) AdminAuthReady() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.adminAuthReadyLocked()
}

// AuthenticateAdminToken checks the local admin bearer token.
func (a *AgentRuntime) AuthenticateAdminToken(token string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	expected := strings.TrimSpace(a.state.State.AdminToken)
	actual := strings.TrimSpace(token)
	if expected == "" || actual == "" {
		return false
	}
	if len(expected) != len(actual) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) == 1
}

func (a *AgentRuntime) adminAuthReadyLocked() bool {
	return strings.TrimSpace(a.state.State.AdminToken) != ""
}

// DeviceSummaries returns admin-safe paired-device metadata.
func (a *AgentRuntime) DeviceSummaries() []DeviceSummary {
	snapshot := a.registry.Snapshot()
	devices := make([]DeviceSummary, 0, len(snapshot.Devices))
	for _, device := range snapshot.Devices {
		devices = append(devices, deviceSummary(device))
	}
	return devices
}

// UpdateDevice updates administrator-visible paired-device metadata.
func (a *AgentRuntime) UpdateDevice(deviceID string, input DeviceUpdate) (DeviceSummary, error) {
	device, err := a.registry.RenameDevice(deviceID, input.DeviceName)
	if err != nil {
		return DeviceSummary{}, err
	}
	if _, err := a.profiles.EnsureProfile(device, time.Now().UTC()); err != nil {
		return DeviceSummary{}, err
	}
	return deviceSummary(device), nil
}

// DeviceUploadPolicy returns the admin upload policy for a paired device.
func (a *AgentRuntime) DeviceUploadPolicy(deviceID string) (DeviceUploadPolicyResponse, error) {
	device, err := a.deviceRecord(deviceID)
	if err != nil {
		return DeviceUploadPolicyResponse{}, err
	}
	profile, err := a.profiles.EnsureProfile(device, time.Now().UTC())
	if err != nil {
		return DeviceUploadPolicyResponse{}, err
	}
	return a.deviceUploadPolicyResponse(device, profile), nil
}

// UpdateDeviceUploadPolicy updates administrator-owned upload policy for one paired device.
func (a *AgentRuntime) UpdateDeviceUploadPolicy(deviceID string, input DeviceUploadPolicyUpdate) (DeviceUploadPolicyResponse, error) {
	device, err := a.deviceRecord(deviceID)
	if err != nil {
		return DeviceUploadPolicyResponse{}, err
	}
	upload, err := a.normalizedDeviceUploadPolicy(input)
	if err != nil {
		return DeviceUploadPolicyResponse{}, err
	}
	now := time.Now().UTC()
	if _, err := a.profiles.EnsureProfile(device, now); err != nil {
		return DeviceUploadPolicyResponse{}, err
	}
	profile, err := a.profiles.UpdateUploadProfile(device.DeviceID, upload, now)
	if err != nil {
		return DeviceUploadPolicyResponse{}, err
	}
	return a.deviceUploadPolicyResponse(device, profile), nil
}

func (a *AgentRuntime) normalizedDeviceUploadPolicy(input DeviceUploadPolicyUpdate) (store.DeviceUploadProfile, error) {
	rootKey := strings.TrimSpace(input.RootKey)
	pathPattern := strings.TrimSpace(input.PathPattern)
	if pathPattern == "" {
		pathPattern = store.DefaultUploadPathPattern()
	}
	if err := validateUploadPathPattern(pathPattern); err != nil {
		return store.DeviceUploadProfile{}, err
	}
	if input.Enabled {
		if rootKey == "" {
			return store.DeviceUploadProfile{}, ErrUploadRootNotFound
		}
		if !a.uploadRootConfigured(rootKey) {
			return store.DeviceUploadProfile{}, ErrUploadRootNotFound
		}
	} else if rootKey != "" && !a.uploadRootConfigured(rootKey) {
		return store.DeviceUploadProfile{}, ErrUploadRootNotFound
	}
	if input.CapturedAfter != nil {
		capturedAfter := input.CapturedAfter.UTC()
		input.CapturedAfter = &capturedAfter
	}
	return store.DeviceUploadProfile{
		Enabled:       input.Enabled,
		RootKey:       rootKey,
		PathPattern:   pathPattern,
		CapturedAfter: input.CapturedAfter,
	}, nil
}

func validateUploadPathPattern(pattern string) error {
	if strings.TrimSpace(pattern) == "" {
		return ErrUploadPolicyInvalid
	}
	if path.IsAbs(pattern) {
		return ErrUploadPolicyInvalid
	}
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return ErrUploadPolicyInvalid
		}
	}
	if strings.Contains(pattern, "\\") {
		return ErrUploadPolicyInvalid
	}
	return nil
}

func (a *AgentRuntime) uploadRootConfigured(rootKey string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	for _, root := range a.config.UploadRoots {
		if root.Key == rootKey {
			return true
		}
	}
	return false
}

func (a *AgentRuntime) deviceUploadPolicyResponse(device store.DeviceRecord, profile store.DeviceProfile) DeviceUploadPolicyResponse {
	return DeviceUploadPolicyResponse{
		DeviceID:   device.DeviceID,
		DeviceName: device.DeviceName,
		Upload: DeviceUploadPolicy{
			Enabled:       profile.Upload.Enabled,
			RootKey:       profile.Upload.RootKey,
			PathPattern:   profile.Upload.PathPattern,
			CapturedAfter: profile.Upload.CapturedAfter,
			UpdatedAt:     profile.Upload.UpdatedAt,
		},
		Status: a.deviceUploadPolicyStatus(profile.Upload),
	}
}

func deviceSummary(device store.DeviceRecord) DeviceSummary {
	return DeviceSummary{
		DeviceID:              device.DeviceID,
		DeviceName:            device.DeviceName,
		CreatedAt:             device.CreatedAt,
		LastRefreshedAt:       device.LastRefreshedAt,
		RefreshTokenExpiresAt: device.RefreshTokenExpiresAt,
	}
}

func (a *AgentRuntime) deviceUploadPolicyStatus(upload store.DeviceUploadProfile) DeviceUploadPolicyStatus {
	if !upload.Enabled {
		return DeviceUploadPolicyStatus{
			State:  "disabled",
			Reason: "Upload is disabled for this device.",
		}
	}
	if strings.TrimSpace(upload.RootKey) == "" {
		return DeviceUploadPolicyStatus{
			State:  "blocked",
			Reason: "Upload root is not selected.",
		}
	}
	root, ok := a.uploadRootConfig(upload.RootKey)
	if !ok {
		return DeviceUploadPolicyStatus{
			State:  "blocked",
			Reason: "Upload root is not configured.",
		}
	}
	rootStatus := uploadRootStatus(root)
	if !rootStatus.Writable {
		return DeviceUploadPolicyStatus{
			State:  "blocked",
			Reason: rootStatus.Message,
			Root:   &rootStatus,
		}
	}
	return DeviceUploadPolicyStatus{
		State: "ready",
		Root:  &rootStatus,
	}
}

// RevokeDevice removes a paired app device from the local registry.
func (a *AgentRuntime) RevokeDevice(deviceID string) error {
	registryErr := a.registry.RevokeDevice(deviceID)
	if registryErr != nil && !errors.Is(registryErr, store.ErrDeviceNotFound) {
		return registryErr
	}
	if _, err := a.resetDeviceUploadState(UploadResetInput{DeviceID: deviceID}); err != nil && !errors.Is(err, store.ErrDeviceNotFound) {
		return err
	}
	if err := a.profiles.DeleteProfile(deviceID); err != nil {
		return err
	}
	return registryErr
}

// ResetDeviceUploadState removes local upload state for a paired device/date range.
func (a *AgentRuntime) ResetDeviceUploadState(input UploadResetInput) (UploadResetResponse, error) {
	input.DeviceID = strings.TrimSpace(input.DeviceID)
	if input.DeviceID == "" {
		return UploadResetResponse{}, store.ErrDeviceNotFound
	}
	if _, err := a.deviceRecord(input.DeviceID); err != nil {
		return UploadResetResponse{}, err
	}
	input.RequireDateRange = true
	return a.resetDeviceUploadState(input)
}

func (a *AgentRuntime) resetDeviceUploadState(input UploadResetInput) (UploadResetResponse, error) {
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()

	if input.RequireDateRange && (input.CapturedAfter == nil || input.CapturedBefore == nil) {
		return UploadResetResponse{}, ErrUploadResetRangeRequired
	}
	if input.CapturedAfter != nil && input.CapturedBefore != nil && !input.CapturedAfter.Before(*input.CapturedBefore) {
		return UploadResetResponse{}, ErrUploadResetInvalid
	}
	result, err := a.uploads.ResetDeviceUploadState(store.UploadResetInput{
		DeviceID:       input.DeviceID,
		CapturedAfter:  input.CapturedAfter,
		CapturedBefore: input.CapturedBefore,
		Reason:         input.Reason,
		Now:            time.Now().UTC(),
	})
	if err != nil {
		return UploadResetResponse{}, err
	}
	removed, cleanupErrors := a.removeUploadTempFiles(result.TempFiles)
	return UploadResetResponse{
		DeviceID:              result.DeviceID,
		CapturedAfter:         result.CapturedAfter,
		CapturedBefore:        result.CapturedBefore,
		ResetAt:               result.ResetAt,
		RemovedUploadedAssets: result.RemovedUploadedAssets,
		RemovedSessions:       result.RemovedSessions,
		RemovedTempFiles:      removed,
		TempCleanupErrors:     cleanupErrors,
	}, nil
}

func (a *AgentRuntime) removeUploadTempFiles(files []store.UploadTempFile) (int64, []string) {
	if len(files) == 0 {
		return 0, nil
	}
	a.mu.RLock()
	roots := make(map[string]config.UploadRootConfig, len(a.config.UploadRoots))
	for _, root := range a.config.UploadRoots {
		root = normalizedUploadRootConfig(root)
		roots[root.Key] = root
	}
	a.mu.RUnlock()
	var removed int64
	var cleanupErrors []string
	for _, file := range files {
		root, ok := roots[file.RootKey]
		if !ok {
			cleanupErrors = append(cleanupErrors, "upload root is not configured for temp cleanup")
			continue
		}
		didRemove, err := removeUploadTempFile(root, file.RelativePath)
		if err != nil {
			cleanupErrors = append(cleanupErrors, err.Error())
			continue
		}
		if didRemove {
			removed++
		}
	}
	return removed, cleanupErrors
}

func normalizedUploadRootConfig(root config.UploadRootConfig) config.UploadRootConfig {
	if tempPath, err := config.ValidateUploadRootTempPath(root.TempPath); err == nil {
		root.TempPath = tempPath
	}
	return root
}

func removeUploadTempFile(root config.UploadRootConfig, relativePath string) (bool, error) {
	cleanRelative := path.Clean(strings.TrimSpace(relativePath))
	cleanTempDir, err := uploadTempRelativeDir(root)
	if err != nil {
		return false, err
	}
	if cleanRelative == "." || path.IsAbs(cleanRelative) || path.Dir(cleanRelative) != cleanTempDir {
		return false, ErrUploadPolicyInvalid
	}
	tempDirExists, err := uploadTempDirExists(root)
	if err != nil {
		return false, err
	}
	if !tempDirExists {
		return false, nil
	}
	fullPath := filepath.Join(root.Path, filepath.FromSlash(cleanRelative))
	if info, err := os.Lstat(fullPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	} else if info.IsDir() {
		return false, ErrUploadPolicyInvalid
	}
	if err := os.Remove(fullPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (a *AgentRuntime) deviceRecord(deviceID string) (store.DeviceRecord, error) {
	normalizedDeviceID := strings.TrimSpace(deviceID)
	if normalizedDeviceID == "" {
		return store.DeviceRecord{}, store.ErrDeviceNotFound
	}
	snapshot := a.registry.Snapshot()
	for _, device := range snapshot.Devices {
		if device.DeviceID == normalizedDeviceID {
			return device, nil
		}
	}
	return store.DeviceRecord{}, store.ErrDeviceNotFound
}

func (a *AgentRuntime) ensureDeviceProfile(deviceID string) error {
	snapshot := a.registry.Snapshot()
	for _, device := range snapshot.Devices {
		if device.DeviceID != deviceID {
			continue
		}
		_, err := a.profiles.EnsureProfile(device, time.Now().UTC())
		return err
	}
	return store.ErrDeviceNotFound
}

// SearchAssets returns one app-facing search page with Timich-owned opaque asset IDs.
func (a *AgentRuntime) SearchAssets(request catalog.AssetSearchRequest) (catalog.AssetSearchPage, error) {
	return a.SearchAssetsWithContext(context.Background(), request)
}

// SearchAssetsWithContext returns one app-facing search page with Timich-owned opaque asset IDs.
func (a *AgentRuntime) SearchAssetsWithContext(ctx context.Context, request catalog.AssetSearchRequest) (catalog.AssetSearchPage, error) {
	catalogService, assetIDKey, sourceKey := a.catalogSnapshot()
	page, err := catalogService.SearchAssetsWithOptionsContext(ctx, request, catalog.AssetSearchOptions{})
	if err != nil {
		return catalog.AssetSearchPage{}, err
	}
	return signAssetSearchPage(page, assetIDKey, sourceKey)
}

// SearchAssetsForAdminPreview returns one signed page with Admin-only search diagnostics.
func (a *AgentRuntime) SearchAssetsForAdminPreview(ctx context.Context, request catalog.AssetSearchRequest, options catalog.AssetSearchOptions) (catalog.AssetSearchPage, error) {
	catalogService, assetIDKey, sourceKey := a.catalogSnapshot()
	options.IncludeSemanticScores = true
	page, err := catalogService.SearchAssetsWithOptionsContext(ctx, request, options)
	if err != nil {
		return catalog.AssetSearchPage{}, err
	}
	return signAssetSearchPage(page, assetIDKey, sourceKey)
}

// SearchCapabilities returns the search features supported by the active datasource.
func (a *AgentRuntime) SearchCapabilities() catalog.AssetSearchCapabilities {
	catalogService := a.catalogService()
	return catalogService.SearchCapabilities()
}

// SemanticModelRegistryStatus returns the semantic embedding profiles available locally.
func (a *AgentRuntime) SemanticModelRegistryStatus() catalog.SemanticModelRegistryStatus {
	return a.SemanticModelRegistryStatusWithContext(context.Background())
}

// SemanticModelRegistryStatusWithContext returns semantic model status with runtime-local state.
func (a *AgentRuntime) SemanticModelRegistryStatusWithContext(ctx context.Context) catalog.SemanticModelRegistryStatus {
	return a.withSemanticModelBackfillStatus(ctx, a.SemanticModelRegistryInstalledStatusWithContext(ctx))
}

// SemanticModelRegistryInstalledStatusWithContext returns installed model and
// runtime-pack state without doing live catalog coverage counts.
func (a *AgentRuntime) SemanticModelRegistryInstalledStatusWithContext(ctx context.Context) catalog.SemanticModelRegistryStatus {
	return a.withSemanticIndexingWorkerStatus(
		a.withInstalledSemanticRuntimePacks(a.withInstalledSemanticModelProfiles(ctx, catalog.SemanticModelRegistry())),
	)
}

// EnrichSemanticModelRegistryStatus overlays locally installed model-pack state.
func (a *AgentRuntime) EnrichSemanticModelRegistryStatus(status catalog.SemanticModelRegistryStatus) catalog.SemanticModelRegistryStatus {
	return a.EnrichSemanticModelRegistryStatusWithContext(context.Background(), status)
}

// EnrichSemanticModelRegistryStatusWithContext overlays installed and backfill state.
func (a *AgentRuntime) EnrichSemanticModelRegistryStatusWithContext(ctx context.Context, status catalog.SemanticModelRegistryStatus) catalog.SemanticModelRegistryStatus {
	return a.withSemanticModelBackfillStatus(ctx, a.EnrichSemanticModelRegistryInstalledStatusWithContext(ctx, status))
}

// EnrichSemanticModelRegistryInstalledStatusWithContext overlays installed
// model-pack and runtime-pack state without live catalog coverage counts.
func (a *AgentRuntime) EnrichSemanticModelRegistryInstalledStatusWithContext(ctx context.Context, status catalog.SemanticModelRegistryStatus) catalog.SemanticModelRegistryStatus {
	return a.withSemanticIndexingWorkerStatus(
		a.withInstalledSemanticRuntimePacks(a.withInstalledSemanticModelProfiles(ctx, status)),
	)
}

// EnrichSemanticModelRegistryCachedIndexingStatusWithContext overlays cached
// semantic indexing progress from the Admin read model when available.
func (a *AgentRuntime) EnrichSemanticModelRegistryCachedIndexingStatusWithContext(ctx context.Context, status catalog.SemanticModelRegistryStatus) catalog.SemanticModelRegistryStatus {
	return a.withCachedSemanticModelBackfillStatus(ctx, a.EnrichSemanticModelRegistryInstalledStatusWithContext(ctx, status))
}

// InstallSemanticModelPack verifies and persists one semantic model-pack artifact.
func (a *AgentRuntime) InstallSemanticModelPack(ctx context.Context, pack catalog.SemanticModelPackStatus, reader io.Reader) (catalog.SemanticModelPackInstallResult, error) {
	if a == nil || a.semanticModels == nil {
		return catalog.SemanticModelPackInstallResult{}, catalog.ErrSemanticModelPackInvalid
	}
	a.semanticPackLifecycleMu.Lock()
	defer a.semanticPackLifecycleMu.Unlock()
	semanticWorkLocked := false
	result, err := a.semanticModels.InstallPackWithCommitHook(ctx, pack, reader, func() {
		a.semanticWorkMu.Lock()
		semanticWorkLocked = true
	})
	if semanticWorkLocked {
		defer a.semanticWorkMu.Unlock()
	}
	if err != nil {
		return catalog.SemanticModelPackInstallResult{}, err
	}
	layout, ok := a.semanticModels.InstallRuntimeLayout(result.ModelPack.ID, result.ModelPack.VectorSpaceID, result.InstallID)
	if !ok {
		rollbackErr := a.semanticModels.RollbackPackInstall(result.ModelPack.ID, result.ModelPack.VectorSpaceID, result.InstallID)
		return catalog.SemanticModelPackInstallResult{}, errors.Join(catalog.ErrSemanticModelPackInvalid, rollbackErr)
	}
	var probeErr error
	if strings.TrimSpace(layout.Runtime) == "onnxruntime" {
		probeErr = a.semanticONNXRuntime.ProbeModelLayout(ctx, layout, result.InstallID)
	} else {
		probeErr = a.semanticModels.ProbeInstallRuntime(ctx, result.ModelPack.ID, result.ModelPack.VectorSpaceID, result.InstallID)
	}
	if probeErr != nil {
		rollbackErr := a.semanticModels.RollbackPackInstall(result.ModelPack.ID, result.ModelPack.VectorSpaceID, result.InstallID)
		return catalog.SemanticModelPackInstallResult{}, errors.Join(probeErr, rollbackErr)
	}
	if err := a.semanticModels.FinalizePackInstall(result.ModelPack.ID, result.ModelPack.VectorSpaceID, result.InstallID); err != nil {
		return catalog.SemanticModelPackInstallResult{}, err
	}
	a.garbageCollectSemanticModelCorporaLocked()
	a.reconcileSemanticModelRuntime()
	return result, nil
}

func (a *AgentRuntime) UninstallSemanticModelPack(ctx context.Context, modelID string, vectorSpaceID string) (catalog.SemanticModelUninstallResult, error) {
	if a == nil || a.semanticModels == nil {
		return catalog.SemanticModelUninstallResult{}, catalog.ErrSemanticModelPackInvalid
	}
	a.semanticPackLifecycleMu.Lock()
	defer a.semanticPackLifecycleMu.Unlock()
	a.semanticWorkMu.Lock()
	defer a.semanticWorkMu.Unlock()
	a.mirrorSyncMu.Lock()
	defer a.mirrorSyncMu.Unlock()
	if a.semanticModelPackMigrating(ctx, modelID, vectorSpaceID) {
		return catalog.SemanticModelUninstallResult{}, ErrSemanticModelPackMigrating
	}
	result, err := a.semanticModels.UninstallPack(modelID, vectorSpaceID)
	if err != nil {
		return catalog.SemanticModelUninstallResult{}, err
	}
	a.garbageCollectSemanticModelCorporaLocked()
	a.reconcileSemanticModelRuntime()
	return result, nil
}

func (a *AgentRuntime) garbageCollectSemanticModelCorporaLocked() {
	if a == nil || a.semanticModels == nil || a.catalogService() == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	removed, err := a.catalogService().GarbageCollectSemanticModelCorpora(ctx, a.semanticModels.ReachableCorpusIdentities())
	if removed > 0 {
		log.Printf("timich-agent semantic corpus GC removed_identities=%d", removed)
		a.schedulerWorkStateMarkDirty()
		a.wakeBackgroundWorkerScheduler()
	}
	if err != nil && !errors.Is(err, catalog.ErrCatalogNotConfigured) {
		log.Printf("timich-agent semantic corpus GC deferred error=%v", err)
		return
	}
}

func (a *AgentRuntime) semanticModelPackMigrating(ctx context.Context, modelID string, vectorSpaceID string) bool {
	modelID = strings.TrimSpace(modelID)
	vectorSpaceID = strings.TrimSpace(vectorSpaceID)
	if modelID == "" || vectorSpaceID == "" {
		return false
	}
	status := a.SemanticModelRegistryStatusWithContext(ctx)
	if semanticModelProfileMatchesIdentity(status.Active, modelID, vectorSpaceID) {
		return false
	}
	if status.Indexing == nil ||
		!semanticModelBackfillMatchesIdentity(*status.Indexing, modelID, vectorSpaceID) {
		return false
	}
	switch status.Indexing.Status {
	case catalog.SemanticBackfillStatusPending,
		catalog.SemanticBackfillStatusBackfilling,
		catalog.SemanticBackfillStatusIndexing:
		return true
	default:
		return false
	}
}

// InstallSemanticRuntimePack verifies and persists one semantic runtime-pack artifact.
func (a *AgentRuntime) InstallSemanticRuntimePack(ctx context.Context, pack catalog.SemanticRuntimePackStatus, reader io.Reader) (catalog.SemanticRuntimePackInstallResult, error) {
	if a == nil || a.semanticRuntimePacks == nil {
		return catalog.SemanticRuntimePackInstallResult{}, catalog.ErrSemanticRuntimePackInvalid
	}
	a.semanticPackLifecycleMu.Lock()
	defer a.semanticPackLifecycleMu.Unlock()
	semanticWorkLocked := false
	result, err := a.semanticRuntimePacks.InstallPackWithCommitHook(ctx, pack, reader, func() {
		a.semanticWorkMu.Lock()
		semanticWorkLocked = true
	})
	if semanticWorkLocked {
		defer a.semanticWorkMu.Unlock()
	}
	if err != nil {
		return catalog.SemanticRuntimePackInstallResult{}, err
	}
	if err := a.semanticONNXRuntime.ProbeRuntimePack(ctx, result.RuntimePack, result.InstallID); err != nil {
		rollbackErr := a.semanticRuntimePacks.RollbackPackInstall(result.RuntimePack.ID, result.RuntimePack.Version, result.InstallID)
		return catalog.SemanticRuntimePackInstallResult{}, errors.Join(err, rollbackErr)
	}
	if err := a.semanticRuntimePacks.FinalizePackInstall(result.RuntimePack.ID, result.RuntimePack.Version, result.InstallID); err != nil {
		return catalog.SemanticRuntimePackInstallResult{}, err
	}
	// Only the health-verified artifact becomes visible to managed processes.
	a.semanticONNXRuntime.InvalidateLaunchIdentities()
	a.reconcileSemanticModelRuntime()
	return result, nil
}

// UpdateSemanticIndexing persists and applies semantic search indexing settings.
func (a *AgentRuntime) UpdateSemanticIndexing(input config.SemanticIndexingConfig) (config.SemanticIndexingConfig, error) {
	if a == nil {
		return config.SemanticIndexingConfig{}, ErrSemanticCandidateUnavailable
	}
	a.configMutationMu.Lock()
	defer a.configMutationMu.Unlock()
	return a.updateSemanticIndexing(input)
}

func (a *AgentRuntime) updateSemanticIndexing(input config.SemanticIndexingConfig) (config.SemanticIndexingConfig, error) {
	// Callers hold configMutationMu so the file and in-memory config update as
	// one transaction with every other agent configuration mutation.
	a.mu.Lock()
	nextConfig := a.config
	nextConfig.SemanticRuntime.Indexing = input
	configPath := nextConfig.ConfigPath
	a.mu.Unlock()

	updatedConfig, err := config.UpdateSemanticIndexingFile(configPath, input)
	if err != nil {
		return config.SemanticIndexingConfig{}, err
	}

	a.mu.Lock()
	a.config.SemanticRuntime.Indexing = updatedConfig.SemanticRuntime.Indexing
	a.config.ConfigSource = "file"
	a.mu.Unlock()

	a.schedulerWorkStateMarkDirty()
	a.syncBackgroundWorkerScheduler()
	return input, nil
}

// UpdateWorkerRuntime persists and applies shared background worker settings.
func (a *AgentRuntime) UpdateWorkerRuntime(input config.WorkerRuntimeConfig) (WorkerRuntimeResponse, error) {
	if a == nil {
		return WorkerRuntimeResponse{}, ErrSemanticCandidateUnavailable
	}
	a.configMutationMu.Lock()
	defer a.configMutationMu.Unlock()
	a.mu.RLock()
	configPath := a.config.ConfigPath
	a.mu.RUnlock()

	updatedConfig, err := config.UpdateWorkerRuntimeFile(configPath, input)
	if err != nil {
		return WorkerRuntimeResponse{}, err
	}
	// Applying the new limit and admitting a new assignment share
	// backgroundWorkerMu. An assignment is therefore either admitted before
	// this update and allowed to finish, or evaluated against the new limit.
	a.backgroundWorkerMu.Lock()
	a.mu.Lock()
	a.config.WorkerRuntime = updatedConfig.WorkerRuntime
	a.config.SemanticRuntime.Indexing = updatedConfig.SemanticRuntime.Indexing
	a.config.ConfigSource = "file"
	a.mu.Unlock()
	a.backgroundWorkerMu.Unlock()

	a.schedulerWorkStateMarkDirty()
	a.syncBackgroundWorkerScheduler()
	return a.WorkerRuntimeStatus(), nil
}

// RunSemanticIndexing processes one bounded semantic indexing batch. A zero
// maxAssets value only reconciles a durable HNSW publish job; the background
// worker owns the potentially long-running build after this method returns.
func (a *AgentRuntime) RunSemanticIndexing(ctx context.Context, maxAssets int) (catalog.SemanticBackfillResult, error) {
	result, err := a.backfillSemanticModelCandidate(ctx, maxAssets, catalog.SemanticModelBackfillOptions{})
	if err == nil && maxAssets == 0 {
		a.schedulerWorkStateMarkDirty()
		a.syncBackgroundWorkerScheduler()
	}
	return result, err
}

func (a *AgentRuntime) backfillSemanticModelCandidate(ctx context.Context, maxAssets int, options catalog.SemanticModelBackfillOptions) (catalog.SemanticBackfillResult, error) {
	if a == nil || a.semanticModels == nil {
		return catalog.SemanticBackfillResult{}, ErrSemanticCandidateUnavailable
	}
	a.semanticWorkMu.Lock()
	defer a.semanticWorkMu.Unlock()
	a.reconcileSemanticModelRuntime()
	a.mirrorSyncMu.Lock()
	defer a.mirrorSyncMu.Unlock()

	status := a.SemanticModelRegistryStatusWithContext(ctx)
	catalogService := a.catalogService()
	if catalogService == nil {
		return catalog.SemanticBackfillResult{}, catalog.ErrCatalogNotConfigured
	}
	candidate := semanticCandidateNeedingBackfill(ctx, catalogService, status, a.semanticModels)
	if candidate == nil {
		candidate = semanticReadySemanticCandidate(ctx, catalogService, status, a.semanticModels)
	}
	if candidate == nil {
		candidate = status.Candidate
	}
	if candidate == nil {
		return catalog.SemanticBackfillResult{}, ErrSemanticCandidateUnavailable
	}
	if candidate.Runtime == nil || !candidate.Runtime.Loaded || !candidate.Runtime.CanEmbed {
		return catalog.SemanticBackfillResult{}, ErrSemanticCandidateRuntimeUnavailable
	}
	options.MaxAssets = maxAssets
	if maxAssets == 0 {
		options.AllowPartialIndexPublish = true
	}
	if options.Workers <= 0 && maxAssets > 0 {
		options.Workers = a.effectiveHeavyTaskWorkers()
	}
	if options.Workers <= 0 && maxAssets > 0 {
		result := catalog.SemanticBackfillResult{}
		if backfill, err := catalogService.SemanticModelBackfillStatus(ctx, *candidate); err == nil && backfill != nil {
			result.Status = *backfill
			result.IndexedVectorCount = backfill.IndexedVectorCount
			a.rememberSemanticIndexingProgressSnapshot(catalogService, nil, result.Status)
		}
		return result, nil
	}
	activityPhase, activeWorkers := semanticBackfillActivitySnapshot(maxAssets, options)
	clearActiveWorkers := func() {}
	if activityPhase == "search_index" {
		clearActiveWorkers = a.setSemanticIndexPublishActive(activeWorkers)
	} else {
		clearActiveWorkers = a.setSemanticBackfillActiveWorkers(activeWorkers)
	}
	a.rememberDatasourceTaskActivitySnapshot(catalogService, activityPhase, activeWorkers)
	clearedActiveWorkers := false
	defer func() {
		if !clearedActiveWorkers {
			clearActiveWorkers()
			a.rememberDatasourceTaskActivitySnapshot(catalogService, activityPhase, 0)
		}
	}()
	consumedEmbeddingHint := a.schedulerWorkStateEmbeddingHintSnapshot()
	result, err := catalogService.BackfillSemanticModelCandidateWithOptions(ctx, a.semanticModels, *candidate, options)
	clearActiveWorkers()
	clearedActiveWorkers = true
	a.rememberDatasourceTaskActivitySnapshot(catalogService, activityPhase, 0)
	if err == nil {
		a.schedulerWorkStateApplySemanticStatusAfterEmbedding(result.Status, consumedEmbeddingHint)
		a.rememberSemanticIndexingProgressSnapshot(catalogService, result.SourceStatuses, result.Status)
	} else {
		a.rememberDatasourceTaskActivitySnapshot(catalogService, activityPhase, 0)
	}
	return result, err
}

func semanticBackfillActivitySnapshot(maxAssets int, options catalog.SemanticModelBackfillOptions) (string, int) {
	if maxAssets == 0 && options.DrainIndexJobs {
		return "search_index", 1
	}
	return "embeddings", min(options.Workers, max(maxAssets, 0))
}

func (a *AgentRuntime) setSemanticBackfillActiveWorkers(workers int) func() {
	if a == nil || workers <= 0 {
		return func() {}
	}
	a.semanticTaskMu.Lock()
	a.semanticActiveWorkers = workers
	a.semanticTaskMu.Unlock()
	return func() {
		a.semanticTaskMu.Lock()
		a.semanticActiveWorkers = 0
		a.semanticTaskMu.Unlock()
	}
}

func (a *AgentRuntime) semanticBackfillActiveWorkers() int {
	if a == nil {
		return 0
	}
	a.semanticTaskMu.Lock()
	defer a.semanticTaskMu.Unlock()
	return a.semanticActiveWorkers
}

func (a *AgentRuntime) setSemanticIndexPublishActive(active int) func() {
	if a == nil || active <= 0 {
		return func() {}
	}
	a.semanticTaskMu.Lock()
	a.semanticIndexActive = active
	a.semanticTaskMu.Unlock()
	return func() {
		a.semanticTaskMu.Lock()
		a.semanticIndexActive = 0
		a.semanticTaskMu.Unlock()
	}
}

func (a *AgentRuntime) semanticIndexPublishActive() int {
	if a == nil {
		return 0
	}
	a.semanticTaskMu.Lock()
	defer a.semanticTaskMu.Unlock()
	return a.semanticIndexActive
}

func (a *AgentRuntime) setSemanticIndexingNextRunAt(next *time.Time) {
	if a == nil {
		return
	}
	a.semanticTaskMu.Lock()
	defer a.semanticTaskMu.Unlock()
	if next == nil || next.IsZero() {
		a.semanticIndexingNextRun = nil
		return
	}
	utc := next.UTC()
	a.semanticIndexingNextRun = &utc
}

func (a *AgentRuntime) semanticIndexingNextRunAt() *time.Time {
	if a == nil {
		return nil
	}
	a.semanticTaskMu.Lock()
	defer a.semanticTaskMu.Unlock()
	if a.semanticIndexingNextRun == nil {
		return nil
	}
	utc := a.semanticIndexingNextRun.UTC()
	return &utc
}

func (a *AgentRuntime) setSemanticIndexingRetryNotBefore(next *time.Time) {
	if a == nil {
		return
	}
	a.semanticTaskMu.Lock()
	defer a.semanticTaskMu.Unlock()
	if next == nil || next.IsZero() {
		a.semanticIndexingRetryNotBefore = nil
		return
	}
	utc := next.UTC()
	a.semanticIndexingRetryNotBefore = &utc
}

func (a *AgentRuntime) semanticIndexingRetryNotBeforeAt() *time.Time {
	if a == nil {
		return nil
	}
	a.semanticTaskMu.Lock()
	defer a.semanticTaskMu.Unlock()
	if a.semanticIndexingRetryNotBefore == nil {
		return nil
	}
	utc := a.semanticIndexingRetryNotBefore.UTC()
	return &utc
}

func (a *AgentRuntime) setSemanticPublishRetryNotBefore(next *time.Time) {
	if a == nil {
		return
	}
	a.semanticTaskMu.Lock()
	defer a.semanticTaskMu.Unlock()
	if next == nil || next.IsZero() {
		a.semanticPublishRetryNotBefore = nil
		return
	}
	utc := next.UTC()
	a.semanticPublishRetryNotBefore = &utc
}

func (a *AgentRuntime) semanticPublishRetryNotBeforeAt() *time.Time {
	if a == nil {
		return nil
	}
	a.semanticTaskMu.Lock()
	defer a.semanticTaskMu.Unlock()
	if a.semanticPublishRetryNotBefore == nil {
		return nil
	}
	utc := a.semanticPublishRetryNotBefore.UTC()
	return &utc
}

func (a *AgentRuntime) ActivateSemanticModel(ctx context.Context, modelID string, vectorSpaceID string) (catalog.SemanticModelActivationResult, error) {
	if a == nil || a.semanticModels == nil {
		return catalog.SemanticModelActivationResult{}, ErrSemanticCandidateUnavailable
	}
	a.semanticPackLifecycleMu.Lock()
	defer a.semanticPackLifecycleMu.Unlock()
	a.semanticWorkMu.Lock()
	defer a.semanticWorkMu.Unlock()
	a.reconcileSemanticModelRuntime()
	a.mirrorSyncMu.Lock()
	defer a.mirrorSyncMu.Unlock()

	modelID = strings.TrimSpace(modelID)
	vectorSpaceID = strings.TrimSpace(vectorSpaceID)
	if modelID == "" || vectorSpaceID == "" {
		return catalog.SemanticModelActivationResult{}, ErrSemanticCandidateUnavailable
	}

	status := a.SemanticModelRegistryStatusWithContext(ctx)
	profile := semanticProfileByIdentity(status, modelID, vectorSpaceID)
	if profile == nil {
		return catalog.SemanticModelActivationResult{}, ErrSemanticCandidateUnavailable
	}
	if profile.Runtime == nil || !profile.Runtime.Loaded || !profile.Runtime.CanEmbed {
		return catalog.SemanticModelActivationResult{}, ErrSemanticCandidateRuntimeUnavailable
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return catalog.SemanticModelActivationResult{}, catalog.ErrCatalogNotConfigured
	}
	backfill, err := catalogService.SemanticModelBackfillStatus(ctx, *profile)
	if err != nil ||
		backfill == nil ||
		backfill.Status != catalog.SemanticBackfillStatusReady ||
		backfill.CompletedVectorCount < backfill.EligibleAssetCount ||
		backfill.IndexedVectorCount < backfill.CompletedVectorCount {
		return catalog.SemanticModelActivationResult{}, ErrSemanticIndexingIncomplete
	}
	result, err := a.semanticModels.ActivatePack(profile.ModelID, profile.VectorSpaceID)
	if err != nil {
		return catalog.SemanticModelActivationResult{}, err
	}
	a.garbageCollectSemanticModelCorporaLocked()
	return result, nil
}

func semanticProfileByIdentity(status catalog.SemanticModelRegistryStatus, modelID string, vectorSpaceID string) *catalog.SemanticModelProfileStatus {
	modelID = strings.TrimSpace(modelID)
	vectorSpaceID = strings.TrimSpace(vectorSpaceID)
	matches := func(profile catalog.SemanticModelProfileStatus) bool {
		return semanticModelProfileMatchesIdentity(profile, modelID, vectorSpaceID)
	}
	if matches(status.Active) {
		profile := status.Active
		return &profile
	}
	if status.Candidate != nil && matches(*status.Candidate) {
		profile := *status.Candidate
		return &profile
	}
	if status.Recommended != nil && matches(*status.Recommended) {
		profile := *status.Recommended
		return &profile
	}
	for _, profile := range status.Profiles {
		if matches(profile) {
			profile := profile
			return &profile
		}
	}
	return nil
}

func semanticModelProfileMatchesIdentity(profile catalog.SemanticModelProfileStatus, modelID string, vectorSpaceID string) bool {
	return strings.TrimSpace(profile.ModelID) == modelID && strings.TrimSpace(profile.VectorSpaceID) == vectorSpaceID
}

func semanticModelBackfillMatchesIdentity(status catalog.SemanticModelBackfillStatus, modelID string, vectorSpaceID string) bool {
	return strings.TrimSpace(status.ModelID) == modelID && strings.TrimSpace(status.VectorSpaceID) == vectorSpaceID
}

func semanticCandidateNeedingBackfill(ctx context.Context, catalogService *catalog.Service, status catalog.SemanticModelRegistryStatus, modelStore *catalog.SemanticModelPackStore) *catalog.SemanticModelProfileStatus {
	var selected *catalog.SemanticModelProfileStatus
	selectedRemaining := 0
	selectedPriority := 0
	for _, profile := range semanticBackfillCandidateProfiles(ctx, status, modelStore) {
		rolePriority := semanticBackfillRolePriority(profile)
		if rolePriority == 0 ||
			profile.Runtime == nil ||
			!profile.Runtime.Loaded ||
			!profile.Runtime.CanEmbed {
			continue
		}
		backfill, err := catalogService.SemanticModelBackfillStatus(ctx, profile)
		workCount := semanticCandidateVectorBackfillWorkCount(backfill)
		if err != nil || backfill == nil || workCount <= 0 {
			continue
		}
		if rolePriority < selectedPriority || (rolePriority == selectedPriority && workCount <= selectedRemaining) {
			continue
		}
		candidate := profile
		selected = &candidate
		selectedRemaining = workCount
		selectedPriority = rolePriority
	}
	return selected
}

func semanticCandidateNeedingMixedBackfill(ctx context.Context, catalogService *catalog.Service, status catalog.SemanticModelRegistryStatus, modelStore *catalog.SemanticModelPackStore, schedule semanticIndexingSchedule) (*catalog.SemanticModelProfileStatus, *catalog.SemanticModelBackfillStatus) {
	var selected *catalog.SemanticModelProfileStatus
	var selectedStatus *catalog.SemanticModelBackfillStatus
	selectedQueued := 0
	selectedPriority := 0
	for _, profile := range semanticBackfillCandidateProfiles(ctx, status, modelStore) {
		rolePriority := semanticBackfillRolePriority(profile)
		if rolePriority == 0 ||
			profile.Runtime == nil ||
			!profile.Runtime.Loaded ||
			!profile.Runtime.CanEmbed {
			continue
		}
		backfill, err := catalogService.SemanticModelBackfillStatus(ctx, profile)
		if err != nil || backfill == nil {
			continue
		}
		if _, ok := semanticMixedIndexingBatchSizeForStatus(schedule, *backfill); !ok {
			continue
		}
		queued := semanticIndexEmbeddingQueued(*backfill)
		if rolePriority < selectedPriority || (rolePriority == selectedPriority && queued <= selectedQueued) {
			continue
		}
		candidate := profile
		backfillCopy := *backfill
		selected = &candidate
		selectedStatus = &backfillCopy
		selectedQueued = queued
		selectedPriority = rolePriority
	}
	return selected, selectedStatus
}

func semanticCandidateNeedingIndexPublish(ctx context.Context, catalogService *catalog.Service, status catalog.SemanticModelRegistryStatus, modelStore *catalog.SemanticModelPackStore) *catalog.SemanticModelProfileStatus {
	var selected *catalog.SemanticModelProfileStatus
	selectedWork := 0
	selectedPriority := 0
	for _, profile := range semanticBackfillCandidateProfiles(ctx, status, modelStore) {
		rolePriority := semanticBackfillRolePriority(profile)
		if rolePriority == 0 ||
			profile.Runtime == nil ||
			!profile.Runtime.Loaded ||
			!profile.Runtime.CanEmbed {
			continue
		}
		needed, workCount, err := catalogService.SemanticModelIndexPublishNeeded(ctx, modelStore, profile, nil, false)
		if err != nil || !needed || workCount <= 0 {
			continue
		}
		if rolePriority < selectedPriority || (rolePriority == selectedPriority && workCount <= selectedWork) {
			continue
		}
		candidate := profile
		selected = &candidate
		selectedWork = workCount
		selectedPriority = rolePriority
	}
	return selected
}

func semanticCandidateNeedingPriorityIndexPublish(ctx context.Context, catalogService *catalog.Service, status catalog.SemanticModelRegistryStatus, modelStore *catalog.SemanticModelPackStore) *catalog.SemanticModelProfileStatus {
	var selected *catalog.SemanticModelProfileStatus
	selectedWork := 0
	selectedPriority := 0
	for _, profile := range semanticBackfillCandidateProfiles(ctx, status, modelStore) {
		rolePriority := semanticBackfillRolePriority(profile)
		if rolePriority == 0 ||
			profile.Runtime == nil ||
			!profile.Runtime.Loaded ||
			!profile.Runtime.CanEmbed {
			continue
		}
		backfill, err := catalogService.SemanticModelBackfillStatus(ctx, profile)
		if err != nil || backfill == nil || !semanticPriorityIndexPublishDue(*backfill) {
			continue
		}
		if backfill.PendingIndexJobCount+backfill.FailedIndexJobCount > 0 && backfill.EligibleIndexJobCount <= 0 {
			continue
		}
		workCount := semanticCandidateIndexPublishWorkCount(backfill)
		if workCount <= 0 {
			continue
		}
		if rolePriority < selectedPriority || (rolePriority == selectedPriority && workCount <= selectedWork) {
			continue
		}
		candidate := profile
		selected = &candidate
		selectedWork = workCount
		selectedPriority = rolePriority
	}
	return selected
}

func semanticReadySemanticCandidate(ctx context.Context, catalogService *catalog.Service, status catalog.SemanticModelRegistryStatus, modelStore *catalog.SemanticModelPackStore) *catalog.SemanticModelProfileStatus {
	var selected *catalog.SemanticModelProfileStatus
	var selectedBackfill *catalog.SemanticModelBackfillStatus
	selectedPriority := 0
	for _, profile := range semanticBackfillCandidateProfiles(ctx, status, modelStore) {
		rolePriority := semanticBackfillRolePriority(profile)
		if rolePriority == 0 ||
			profile.Runtime == nil ||
			!profile.Runtime.Loaded ||
			!profile.Runtime.CanEmbed {
			continue
		}
		backfill, err := catalogService.SemanticModelBackfillStatus(ctx, profile)
		if err != nil ||
			backfill == nil ||
			backfill.Status != catalog.SemanticBackfillStatusReady ||
			backfill.CompletedVectorCount < backfill.EligibleAssetCount ||
			backfill.IndexedVectorCount < backfill.CompletedVectorCount {
			continue
		}
		if rolePriority < selectedPriority ||
			(rolePriority == selectedPriority && !semanticIndexingPreferredForRuntime(profile, backfill, selected, selectedBackfill)) {
			continue
		}
		candidate := profile
		selected = &candidate
		selectedBackfill = backfill
		selectedPriority = rolePriority
	}
	return selected
}

func semanticBackfillCandidateProfiles(ctx context.Context, status catalog.SemanticModelRegistryStatus, modelStore *catalog.SemanticModelPackStore) []catalog.SemanticModelProfileStatus {
	profiles := make([]catalog.SemanticModelProfileStatus, 0, len(status.Profiles)+4)
	add := func(profile catalog.SemanticModelProfileStatus) {
		if profile.ProfileKind != catalog.SemanticProfileKindModelPack ||
			strings.TrimSpace(profile.ModelID) == "" ||
			strings.TrimSpace(profile.VectorSpaceID) == "" ||
			semanticModelProfileListed(profiles, profile) {
			return
		}
		profiles = append(profiles, profile)
	}
	if status.Candidate != nil {
		add(*status.Candidate)
	}
	if status.Recommended != nil {
		add(*status.Recommended)
	}
	add(status.Active)
	for _, profile := range status.Profiles {
		add(profile)
	}
	if modelStore != nil {
		for _, profile := range modelStore.InstalledProfilesWithContext(ctx) {
			add(profile)
		}
	}
	return profiles
}

func semanticBackfillRolePriority(profile catalog.SemanticModelProfileStatus) int {
	if profile.ProfileKind != catalog.SemanticProfileKindModelPack {
		return 0
	}
	switch profile.Role {
	case catalog.SemanticModelRoleCandidate:
		return 2
	case catalog.SemanticModelRoleActive:
		return 1
	default:
		return 0
	}
}

func semanticIndexingPreferredForRuntime(candidate catalog.SemanticModelProfileStatus, status *catalog.SemanticModelBackfillStatus, selected *catalog.SemanticModelProfileStatus, selectedStatus *catalog.SemanticModelBackfillStatus) bool {
	if status == nil {
		return false
	}
	if selected == nil || selectedStatus == nil {
		return true
	}
	if status.IndexedVectorCount != selectedStatus.IndexedVectorCount {
		return status.IndexedVectorCount > selectedStatus.IndexedVectorCount
	}
	candidateInstalledAt := semanticRuntimeCandidateInstalledAt(candidate)
	selectedInstalledAt := semanticRuntimeCandidateInstalledAt(*selected)
	if !candidateInstalledAt.Equal(selectedInstalledAt) {
		return candidateInstalledAt.After(selectedInstalledAt)
	}
	if candidate.ModelID != selected.ModelID {
		return candidate.ModelID > selected.ModelID
	}
	return candidate.VectorSpaceID > selected.VectorSpaceID
}

func semanticRuntimeCandidateInstalledAt(candidate catalog.SemanticModelProfileStatus) time.Time {
	if candidate.ModelPack == nil {
		return time.Time{}
	}
	return candidate.ModelPack.InstalledAt
}

func semanticCandidateVectorBackfillWorkCount(status *catalog.SemanticModelBackfillStatus) int {
	if status == nil {
		return 0
	}
	return max(status.EligibleNowVectorCount, 0)
}

func semanticCandidateIndexPublishWorkCount(status *catalog.SemanticModelBackfillStatus) int {
	if status == nil {
		return 0
	}
	if status.PendingIndexJobCount+status.FailedIndexJobCount > 0 {
		return max(status.EligibleIndexJobCount, 0)
	}
	if remaining := status.CompletedVectorCount - status.IndexedVectorCount; remaining > 0 {
		return remaining
	}
	return 0
}

func semanticPriorityIndexPublishDue(status catalog.SemanticModelBackfillStatus) bool {
	queued := semanticIndexPublishQueued(status)
	if queued <= 0 {
		return false
	}
	return queued >= semanticIndexPartialPublishQueuedThreshold(status.IndexedVectorCount)
}

func (a *AgentRuntime) withInstalledSemanticModelProfiles(ctx context.Context, status catalog.SemanticModelRegistryStatus) catalog.SemanticModelRegistryStatus {
	if a == nil || a.semanticModels == nil {
		return status
	}
	if active, ok := a.semanticModels.ActiveProfileWithContext(ctx); ok {
		status.Active = active
	} else {
		status.Active = a.semanticModels.InstalledProfileWithContext(ctx, status.Active)
	}
	if status.Recommended != nil {
		recommended := a.semanticModels.InstalledProfileWithContext(ctx, *status.Recommended)
		status.Recommended = &recommended
		if recommended.Role == catalog.SemanticModelRoleCandidate {
			status.Candidate = &recommended
		}
	}
	for index := range status.Profiles {
		status.Profiles[index] = a.semanticModels.InstalledProfileWithContext(ctx, status.Profiles[index])
		if status.Profiles[index].Role == catalog.SemanticModelRoleCandidate {
			candidate := status.Profiles[index]
			status.Candidate = &candidate
		}
	}
	for _, installed := range a.semanticModels.InstalledProfilesWithContext(ctx) {
		if !semanticModelProfileListed(status.Profiles, installed) {
			status.Profiles = append(status.Profiles, installed)
		}
		if installed.Role == catalog.SemanticModelRoleCandidate && status.Candidate == nil {
			candidate := installed
			status.Candidate = &candidate
		}
	}
	if status.Candidate != nil && status.Candidate.Role != catalog.SemanticModelRoleCandidate {
		status.Candidate = nil
	}
	return status
}

func (a *AgentRuntime) withInstalledSemanticRuntimePacks(status catalog.SemanticModelRegistryStatus) catalog.SemanticModelRegistryStatus {
	if a == nil || a.semanticRuntimePacks == nil {
		return status
	}
	if status.RecommendedRuntimePack != nil {
		recommended := a.semanticRuntimePacks.InstalledPackStatus(*status.RecommendedRuntimePack)
		status.RecommendedRuntimePack = &recommended
		if !semanticRuntimePackListed(status.RuntimePacks, recommended) {
			status.RuntimePacks = append(status.RuntimePacks, recommended)
		}
	}
	for index := range status.RuntimePacks {
		status.RuntimePacks[index] = a.semanticRuntimePacks.InstalledPackStatus(status.RuntimePacks[index])
	}
	for _, installed := range a.semanticRuntimePacks.InstalledPacks() {
		if !semanticRuntimePackListed(status.RuntimePacks, installed) {
			status.RuntimePacks = append(status.RuntimePacks, installed)
		}
	}
	return status
}

func semanticRuntimePackListed(packs []catalog.SemanticRuntimePackStatus, pack catalog.SemanticRuntimePackStatus) bool {
	for _, existing := range packs {
		if existing.ID == pack.ID && existing.Version == pack.Version {
			return true
		}
	}
	return false
}

func semanticModelProfileListed(profiles []catalog.SemanticModelProfileStatus, profile catalog.SemanticModelProfileStatus) bool {
	for _, existing := range profiles {
		if existing.ModelID == profile.ModelID && existing.VectorSpaceID == profile.VectorSpaceID {
			return true
		}
	}
	return false
}

func (a *AgentRuntime) withSemanticModelBackfillStatus(ctx context.Context, status catalog.SemanticModelRegistryStatus) catalog.SemanticModelRegistryStatus {
	if a == nil {
		return status
	}
	catalogService := a.catalogService()
	if catalogService == nil {
		return status
	}
	candidate := semanticCandidateNeedingBackfill(ctx, catalogService, status, a.semanticModels)
	if candidate == nil {
		candidate = semanticReadySemanticCandidate(ctx, catalogService, status, a.semanticModels)
	}
	if candidate == nil {
		candidate = status.Candidate
	}
	if candidate == nil {
		return status
	}
	candidateCopy := *candidate
	status.Candidate = &candidateCopy
	backfill, err := catalogService.SemanticModelBackfillStatus(ctx, candidateCopy)
	if err != nil {
		status.Indexing = &catalog.SemanticModelBackfillStatus{
			Status:        catalog.SemanticBackfillStatusUnavailable,
			ModelID:       candidateCopy.ModelID,
			VectorSpaceID: candidateCopy.VectorSpaceID,
			EmbeddingDim:  candidateCopy.EmbeddingDim,
			MessageCode:   catalog.SemanticBackfillMessageUnavailable,
		}
		return status
	}
	status.Indexing = backfill
	return status
}

func (a *AgentRuntime) withCachedSemanticModelBackfillStatus(ctx context.Context, status catalog.SemanticModelRegistryStatus) catalog.SemanticModelRegistryStatus {
	if a == nil {
		return status
	}
	candidate := semanticInstalledIndexingProfile(status)
	if candidate == nil {
		return status
	}
	candidateCopy := *candidate
	if status.Candidate == nil && candidateCopy.Role == catalog.SemanticModelRoleCandidate {
		status.Candidate = &candidateCopy
	}

	catalogService := a.catalogService()
	if catalogService == nil {
		return status
	}
	stats, err := catalogService.AssetProcessingStats(ctx)
	if err != nil || stats.Empty() {
		status.Indexing = &catalog.SemanticModelBackfillStatus{
			Status:        catalog.SemanticBackfillStatusUnavailable,
			ModelID:       candidateCopy.ModelID,
			VectorSpaceID: candidateCopy.VectorSpaceID,
			EmbeddingDim:  candidateCopy.EmbeddingDim,
			MessageCode:   catalog.SemanticBackfillMessageUnavailable,
		}
		return status
	}
	if indexing := catalog.SemanticBackfillStatusFromAssetProcessingStats(stats, candidateCopy); indexing != nil {
		status.Indexing = indexing
	}
	return status
}

func semanticInstalledIndexingProfile(status catalog.SemanticModelRegistryStatus) *catalog.SemanticModelProfileStatus {
	candidates := make([]catalog.SemanticModelProfileStatus, 0, len(status.Profiles)+3)
	if status.Candidate != nil {
		candidates = append(candidates, *status.Candidate)
	}
	if strings.TrimSpace(status.Active.ModelID) != "" {
		candidates = append(candidates, status.Active)
	}
	if status.Recommended != nil {
		candidates = append(candidates, *status.Recommended)
	}
	candidates = append(candidates, status.Profiles...)

	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		key := semanticModelProfileIdentityKey(candidate)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if candidate.ProfileKind != catalog.SemanticProfileKindModelPack ||
			candidate.ModelPack == nil ||
			!candidate.ModelPack.Installed {
			continue
		}
		copy := candidate
		return &copy
	}
	return nil
}

func semanticModelProfileIdentityKey(profile catalog.SemanticModelProfileStatus) string {
	modelID := strings.TrimSpace(profile.ModelID)
	vectorSpaceID := strings.TrimSpace(profile.VectorSpaceID)
	if modelID == "" || vectorSpaceID == "" {
		return ""
	}
	return modelID + "\x00" + vectorSpaceID
}

func (a *AgentRuntime) withSemanticIndexingWorkerStatus(status catalog.SemanticModelRegistryStatus) catalog.SemanticModelRegistryStatus {
	status.IndexingWorker = &catalog.SemanticIndexingWorkerStatus{
		Status:      "disabled",
		MessageCode: "semantic_indexing_worker_disabled",
	}
	if a == nil {
		return status
	}

	schedule, scheduled := a.semanticIndexingSchedule()
	if scheduled {
		if schedule.Workers <= 0 {
			status.IndexingWorker = &catalog.SemanticIndexingWorkerStatus{
				Enabled:                true,
				Status:                 "paused",
				IntervalSeconds:        int64(schedule.Interval.Seconds()),
				BatchSize:              schedule.BatchSize,
				WorkerCount:            0,
				TargetCompletedVectors: schedule.TargetCompletedVectors,
				MessageCode:            "semantic_indexing_worker_paused",
			}
			return status
		}
		if schedule.TargetCompletedVectors > 0 &&
			status.Indexing != nil &&
			status.Indexing.CompletedVectorCount >= schedule.TargetCompletedVectors {
			status.IndexingWorker = &catalog.SemanticIndexingWorkerStatus{
				Enabled:                true,
				Status:                 "targetReached",
				IntervalSeconds:        int64(schedule.Interval.Seconds()),
				BatchSize:              schedule.BatchSize,
				WorkerCount:            schedule.Workers,
				TargetCompletedVectors: schedule.TargetCompletedVectors,
				MessageCode:            "semantic_indexing_worker_target_reached",
			}
			return status
		}
		status.IndexingWorker = &catalog.SemanticIndexingWorkerStatus{
			Enabled:                true,
			Status:                 "scheduled",
			IntervalSeconds:        int64(schedule.Interval.Seconds()),
			BatchSize:              schedule.BatchSize,
			WorkerCount:            schedule.Workers,
			TargetCompletedVectors: schedule.TargetCompletedVectors,
			MessageCode:            "semantic_indexing_worker_scheduled",
		}
		return status
	}

	a.mu.RLock()
	configured := a.config.SemanticRuntime.Indexing.Enabled
	a.mu.RUnlock()
	if configured {
		status.IndexingWorker = &catalog.SemanticIndexingWorkerStatus{
			Enabled:     true,
			Status:      "blocked",
			MessageCode: "semantic_indexing_worker_requires_catalog_datasource",
		}
	}
	return status
}

// PrimaryDatasourceMirrorStatus returns the Agent-owned mirror status for the primary datasource.
func (a *AgentRuntime) PrimaryDatasourceMirrorStatus(ctx context.Context) (catalog.MirrorStatus, error) {
	catalogService := a.catalogService()
	if catalogService == nil {
		return catalog.MirrorStatus{}, catalog.ErrCatalogNotConfigured
	}
	return catalogService.MirrorStatus(ctx)
}

// SyncPrimaryDatasourceMirror runs a manual mirror sync for the primary datasource.
func (a *AgentRuntime) SyncPrimaryDatasourceMirror(ctx context.Context, mode string) (catalog.MirrorSyncResult, error) {
	a.mirrorSyncMu.Lock()
	defer a.mirrorSyncMu.Unlock()
	catalogService := a.catalogService()
	if catalogService == nil {
		return catalog.MirrorSyncResult{}, catalog.ErrCatalogNotConfigured
	}
	result, err := catalogService.SyncMirror(ctx, mode)
	if err != nil {
		return catalog.MirrorSyncResult{}, err
	}
	a.notifyDatasourceMirrorSyncCompleted()
	return result, nil
}

// Asset returns metadata for one Timich-owned opaque asset ID.
func (a *AgentRuntime) Asset(assetID string) (catalog.Asset, error) {
	catalogService, assetIDKey, sourceKey := a.catalogSnapshot()
	assetSourceKey, upstreamAssetID, err := decodeClientAssetID(assetIDKey, sourceKey, assetID)
	if err != nil {
		return catalog.Asset{}, err
	}
	asset, err := catalogService.AssetFromSource(assetSourceKey, upstreamAssetID)
	if err != nil {
		return catalog.Asset{}, err
	}
	asset.ID = assetID
	asset.SourceKey = ""
	return asset, nil
}

// CatalogPage returns one local timeline page from the configured datasource proxy.
func (a *AgentRuntime) CatalogPage(pageIndex int, pageSize int) (catalog.AssetSearchPage, error) {
	return a.SearchAssets(catalog.AssetSearchRequest{
		Collection: catalog.AssetCollectionRequest{Kind: catalog.CollectionKindTimeline},
		Page: catalog.AssetSearchPageRequest{
			Index: pageIndex,
			Size:  pageSize,
		},
	})
}

// Preview proxies a preview image response for a local client.
func (a *AgentRuntime) Preview(request *http.Request, assetID string) (*catalog.UpstreamMediaResponse, error) {
	catalogService, assetIDKey, sourceKey := a.catalogSnapshot()
	assetSourceKey, upstreamAssetID, err := decodeClientAssetID(assetIDKey, sourceKey, assetID)
	if err != nil {
		return nil, err
	}
	return catalogService.PreviewFromSource(request, assetSourceKey, upstreamAssetID)
}

// DetailPreview returns a detail-preview image response for a client.
func (a *AgentRuntime) DetailPreview(request *http.Request, assetID string) (*catalog.UpstreamMediaResponse, error) {
	catalogService, assetIDKey, sourceKey := a.catalogSnapshot()
	assetSourceKey, upstreamAssetID, err := decodeClientAssetID(assetIDKey, sourceKey, assetID)
	if err != nil {
		return nil, err
	}
	return catalogService.DetailPreviewFromSource(request, assetSourceKey, upstreamAssetID)
}

// Original proxies an original asset response for a local client.
func (a *AgentRuntime) Original(request *http.Request, assetID string) (*catalog.UpstreamMediaResponse, error) {
	catalogService, assetIDKey, sourceKey := a.catalogSnapshot()
	assetSourceKey, upstreamAssetID, err := decodeClientAssetID(assetIDKey, sourceKey, assetID)
	if err != nil {
		return nil, err
	}
	return catalogService.OriginalFromSource(request, assetSourceKey, upstreamAssetID)
}

// AnswerWebRTCOffer answers a remote media DataChannel offer for prototype streaming.
func (a *AgentRuntime) AnswerWebRTCOffer(ctx context.Context, request webrtcmedia.OfferRequest) (webrtcmedia.OfferResponse, error) {
	webrtcManager := a.webrtcManager()
	return webrtcManager.AnswerOffer(ctx, request)
}

// CompatibilityCheck runs a remote browsing readiness probe from the agent runtime.
func (a *AgentRuntime) CompatibilityCheck(ctx context.Context) compatibility.Report {
	checker := a.compatibilityChecker()
	return checker.Run(ctx)
}

// DatasourceCheck verifies the active datasource from the agent runtime.
func (a *AgentRuntime) DatasourceCheck(ctx context.Context) compatibility.Check {
	checker := a.compatibilityChecker()
	return checker.RunDatasourceCheck(ctx)
}

func (a *AgentRuntime) catalogService() *catalog.Service {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.catalog
}

func (a *AgentRuntime) catalogSnapshot() (*catalog.Service, assetIDKeys, string) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.catalog, a.assetIDKey.clone(), a.primaryDatasourceSourceKeyLocked()
}

func (a *AgentRuntime) primaryDatasourceSourceKeyLocked() string {
	if len(a.config.Datasources) == 0 {
		return ""
	}
	return strings.TrimSpace(a.config.Datasources[0].SourceKey)
}

func signAssetSearchPage(
	page catalog.AssetSearchPage,
	assetIDKey assetIDKeys,
	sourceKey string,
) (catalog.AssetSearchPage, error) {
	if strings.TrimSpace(sourceKey) == "" {
		return catalog.AssetSearchPage{}, catalog.ErrNoDatasourceConfigured
	}
	for index := range page.Items {
		itemSourceKey := strings.TrimSpace(page.Items[index].SourceKey)
		if itemSourceKey == "" {
			itemSourceKey = sourceKey
		}
		assetID, err := encodeTimichAssetID(assetIDKey, itemSourceKey, page.Items[index].ID)
		if err != nil {
			return catalog.AssetSearchPage{}, err
		}
		page.Items[index].ID = assetID
		page.Items[index].SourceKey = ""
	}
	return page, nil
}

func decodeClientAssetID(assetIDKey assetIDKeys, fallbackSourceKey string, assetID string) (string, string, error) {
	sourceKey, upstreamAssetID, err := decodeTimichAssetID(assetIDKey, assetID)
	if err != nil {
		return "", "", catalog.ErrAssetNotFound
	}
	if strings.TrimSpace(sourceKey) == "" {
		sourceKey = strings.TrimSpace(fallbackSourceKey)
	}
	if strings.TrimSpace(sourceKey) == "" {
		return "", "", catalog.ErrAssetNotFound
	}
	return sourceKey, upstreamAssetID, nil
}

func (a *AgentRuntime) compatibilityChecker() *compatibility.Service {
	a.mu.RLock()
	defer a.mu.RUnlock()

	ready, blockedBy := a.remoteRegistrationReadyLocked(a.registry.Snapshot())
	return newCompatibilityService(a.build, a.state, a.config, a.catalog, ready, blockedBy)
}

func (a *AgentRuntime) webrtcManager() *webrtcmedia.Manager {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.webrtc
}

func (a *AgentRuntime) pairingStatusLocked() pairing.PairingStatus {
	if !a.catalog.Ready() {
		return pairing.PairingStatusNoDatasourceConfigured
	}
	return a.pairing.PairingStatus()
}
