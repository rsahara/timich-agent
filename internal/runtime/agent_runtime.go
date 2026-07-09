package runtime

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
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
	ErrAdminTokenAlreadyConfigured = errors.New("admin token already configured")
	ErrAdminTokenTooShort          = errors.New("admin token too short")
	ErrPrimaryDatasourceRequired   = errors.New("primary datasource is required")
	ErrDatasourceAccessTokenNeeded = errors.New("datasource access token is required")
	ErrUploadPolicyInvalid         = errors.New("upload policy invalid")
	ErrUploadRootNotFound          = errors.New("upload root not found")
	ErrUploadResetRangeRequired    = errors.New("upload reset range required")
	ErrUploadResetInvalid          = errors.New("upload reset invalid")
)

// AgentRuntime exposes redacted runtime state to the local admin and media APIs.
type AgentRuntime struct {
	mu                sync.RWMutex
	build             BuildInfo
	config            config.ResolvedConfig
	state             store.LoadedState
	assetIDKey        []byte
	startedAt         time.Time
	registry          *store.DeviceRegistryStore
	profiles          *store.DeviceProfileStore
	uploads           *store.UploadStore
	uploadMu          sync.Mutex
	maintenanceMu     sync.Mutex
	maintenanceCancel context.CancelFunc
	maintenanceWG     sync.WaitGroup
	pairing           *pairing.Service
	catalog           *catalog.Service
	webrtc            *webrtcmedia.Manager
}

type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	BuiltAt string `json:"builtAt,omitempty"`
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
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	URL            string `json:"url"`
	HasAccessToken bool   `json:"hasAccessToken"`
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

// UploadRootSummary exposes administrator-configured upload roots.
type UploadRootSummary struct {
	Key      string `json:"key"`
	Path     string `json:"path"`
	TempPath string `json:"tempPath,omitempty"`
	Status   string `json:"status,omitempty"`
	Writable bool   `json:"writable,omitempty"`
	Message  string `json:"message,omitempty"`
}

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
	Service               string                `json:"service"`
	Version               string                `json:"version"`
	Commit                string                `json:"commit,omitempty"`
	BuiltAt               string                `json:"builtAt,omitempty"`
	Mode                  string                `json:"mode"`
	AgentID               string                `json:"agentId"`
	AgentName             string                `json:"agentName"`
	StartedAt             time.Time             `json:"startedAt"`
	UptimeSeconds         int64                 `json:"uptimeSeconds"`
	ConfigSource          string                `json:"configSource"`
	ConfigPath            string                `json:"configPath"`
	DataDir               string                `json:"dataDir"`
	StatePath             string                `json:"statePath"`
	Timezone              string                `json:"timezone,omitempty"`
	AdminListenAddress    string                `json:"adminListenAddress"`
	MediaListenAddress    string                `json:"mediaListenAddress"`
	MediaPublishedAddress string                `json:"mediaPublishedAddress,omitempty"`
	DeviceLimit           int                   `json:"deviceLimit"`
	PairedDeviceCount     int                   `json:"pairedDeviceCount"`
	ActivePairingCount    int                   `json:"activePairingCount"`
	SessionKeyReady       bool                  `json:"sessionKeyReady"`
	AdminAuthReady        bool                  `json:"adminAuthReady"`
	RemoteBrowsing        RemoteBrowsingSummary `json:"remoteBrowsing"`
	Datasources           []DatasourceSummary   `json:"datasources"`
	UploadRoots           []UploadRootSummary   `json:"uploadRoots"`
	SetupTasks            []SetupTask           `json:"setupTasks"`
}

// ConfigResponse exposes the current redacted config plus state location.
type ConfigResponse struct {
	AgentName             string                `json:"agentName"`
	ConfigSource          string                `json:"configSource"`
	ConfigPath            string                `json:"configPath"`
	DataDir               string                `json:"dataDir"`
	StatePath             string                `json:"statePath"`
	Timezone              string                `json:"timezone,omitempty"`
	AdminListenAddress    string                `json:"adminListenAddress"`
	MediaListenAddress    string                `json:"mediaListenAddress"`
	MediaPublishedAddress string                `json:"mediaPublishedAddress,omitempty"`
	DeviceLimit           int                   `json:"deviceLimit"`
	AppLinkBaseURL        string                `json:"appLinkBaseURL"`
	AdminAuthReady        bool                  `json:"adminAuthReady"`
	RemoteBrowsing        RemoteBrowsingSummary `json:"remoteBrowsing"`
	Datasources           []DatasourceSummary   `json:"datasources"`
	UploadRoots           []UploadRootSummary   `json:"uploadRoots"`
}

// InfoResponse is the public LAN-facing metadata summary for the media API.
type InfoResponse struct {
	Service        string                `json:"service"`
	Version        string                `json:"version"`
	Commit         string                `json:"commit,omitempty"`
	BuiltAt        string                `json:"builtAt,omitempty"`
	AgentID        string                `json:"agentId"`
	AgentName      string                `json:"agentName"`
	Mode           string                `json:"mode"`
	DeviceLimit    int                   `json:"deviceLimit"`
	PairedDevices  int                   `json:"pairedDevices"`
	RemoteBrowsing RemoteBrowsingSummary `json:"remoteBrowsing"`
	Hosted         RemoteBrowsingSummary `json:"hosted"`
	Datasources    []DatasourceSummary   `json:"datasources"`
	MediaAPI       string                `json:"mediaAPI"`
	PairingStatus  pairing.PairingStatus `json:"pairingStatus"`
}

// NewAgentRuntime builds the redacted runtime view shared by local HTTP surfaces.
func NewAgentRuntime(build BuildInfo, cfg config.ResolvedConfig, state store.LoadedState, startedAt time.Time) (*AgentRuntime, error) {
	normalizedBuild := build.withDefaults()
	if _, err := config.EnsureDatasourceSourceKeys(&cfg.Config); err != nil {
		return nil, err
	}
	assetIDKey, err := deriveAssetIDKey(state.State.SessionSigningKey)
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
	catalogService := catalog.NewService(cfg.Datasources)
	runtime := &AgentRuntime{
		build:      normalizedBuild,
		config:     cfg,
		state:      state,
		assetIDKey: assetIDKey,
		startedAt:  startedAt.UTC(),
		registry:   registry,
		profiles:   profiles,
		uploads:    uploads,
		pairing:    pairingService,
		catalog:    catalogService,
	}
	runtime.webrtc = webrtcmedia.NewManager(runtime.Original)
	return runtime, nil
}

// Close releases runtime-owned local resources. Long-running agent processes
// normally close via process shutdown; tests use this to release SQLite handles.
func (a *AgentRuntime) Close() error {
	if a == nil || a.uploads == nil {
		return nil
	}
	a.stopUploadMaintenance()
	return a.uploads.Close()
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
	a.mu.RLock()
	defer a.mu.RUnlock()

	snapshot := a.registry.Snapshot()
	return StatusResponse{
		Service:               "timich-agent",
		Version:               a.build.Version,
		Commit:                emptyIfUnknown(a.build.Commit),
		BuiltAt:               emptyIfUnknown(a.build.BuiltAt),
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
		Datasources:           a.datasourceSummariesLocked(),
		UploadRoots:           a.uploadRootSummariesLocked(),
		SetupTasks:            a.setupTasksLocked(snapshot),
	}
}

// ConfigResponse returns the redacted live config.
func (a *AgentRuntime) ConfigResponse() ConfigResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

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
		Datasources:           a.datasourceSummariesLocked(),
		UploadRoots:           a.uploadRootSummariesLocked(),
	}
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
		AgentID:        a.publicInfoAgentIDLocked(),
		AgentName:      a.config.AgentName,
		Mode:           a.modeLocked(),
		DeviceLimit:    a.config.DeviceLimit,
		PairedDevices:  len(snapshot.Devices),
		RemoteBrowsing: remoteBrowsing,
		Hosted:         remoteBrowsing,
		Datasources:    a.datasourceSummariesLocked(),
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

func (a *AgentRuntime) setupTasksLocked(snapshot store.DeviceRegistry) []SetupTask {
	datasourceReady := a.datasourceReadyLocked()
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

	return []SetupTask{
		{
			ID:      "admin_token",
			Label:   "Admin token",
			Status:  completeStatus(a.adminAuthReadyLocked()),
			Summary: boolSummary(a.adminAuthReadyLocked(), "Admin access is protected.", "Create the initial admin token."),
		},
		{
			ID:      "datasource",
			Label:   "Datasource",
			Status:  completeStatus(datasourceReady),
			Summary: boolSummary(datasourceReady, "Datasource is configured.", "Add a datasource."),
		},
		{
			ID:      "paired_device",
			Label:   "Paired device",
			Status:  completeStatus(pairedDeviceReady),
			Summary: boolSummary(pairedDeviceReady, "At least one app device is paired.", "Create a pairing code and pair an app device."),
		},
		{
			ID:      "remote_registration",
			Label:   "Relay connection",
			Status:  remoteStatus,
			Summary: remoteSummary,
		},
	}
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
	if strings.TrimSpace(datasource.URL) == "" {
		return false
	}
	if datasourceRequiresAccessToken(datasource.Kind) && strings.TrimSpace(datasource.AccessToken) == "" {
		return false
	}
	return a.catalog.Ready()
}

func datasourceRequiresAccessToken(kind string) bool {
	return strings.TrimSpace(kind) == "" || strings.TrimSpace(kind) == config.DatasourceKindImmich
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
		summaries = append(summaries, DatasourceSummary{
			Name:           datasource.Name,
			Kind:           datasource.Kind,
			URL:            datasource.URL,
			HasAccessToken: datasource.AccessToken != "",
		})
	}
	return summaries
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

// UpdatePrimaryDatasource persists and applies the primary Immich datasource.
func (a *AgentRuntime) UpdatePrimaryDatasource(input config.DatasourceConfig) (PrimaryDatasourceResponse, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = "Immich"
	}
	kind := strings.TrimSpace(input.Kind)
	if kind == "" {
		kind = "immich"
	}
	nextDatasource := config.DatasourceConfig{
		SourceKey:   strings.TrimSpace(input.SourceKey),
		Name:        name,
		Kind:        kind,
		URL:         strings.TrimSpace(input.URL),
		AccessToken: strings.TrimSpace(input.AccessToken),
	}
	if nextDatasource.URL == "" {
		return PrimaryDatasourceResponse{}, ErrPrimaryDatasourceRequired
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	nextConfig := a.config
	nextConfig.Datasources = append([]config.DatasourceConfig(nil), a.config.Datasources...)
	if nextDatasource.SourceKey == "" && len(nextConfig.Datasources) > 0 {
		nextDatasource.SourceKey = nextConfig.Datasources[0].SourceKey
	}
	if nextDatasource.SourceKey == "" {
		sourceKey, err := config.GenerateDatasourceSourceKey()
		if err != nil {
			return PrimaryDatasourceResponse{}, err
		}
		nextDatasource.SourceKey = sourceKey
	}
	if datasourceRequiresAccessToken(nextDatasource.Kind) && nextDatasource.AccessToken == "" && len(nextConfig.Datasources) > 0 {
		nextDatasource.AccessToken = nextConfig.Datasources[0].AccessToken
	}
	if datasourceRequiresAccessToken(nextDatasource.Kind) && nextDatasource.AccessToken == "" {
		return PrimaryDatasourceResponse{}, ErrDatasourceAccessTokenNeeded
	}
	if len(nextConfig.Datasources) == 0 {
		nextConfig.Datasources = []config.DatasourceConfig{nextDatasource}
	} else {
		nextConfig.Datasources[0] = nextDatasource
	}

	if _, err := config.UpdatePrimaryDatasourceFile(nextConfig.ConfigPath, nextDatasource); err != nil {
		return PrimaryDatasourceResponse{}, err
	}

	catalogService := catalog.NewService(nextConfig.Datasources)
	nextConfig.ConfigSource = "file"
	a.config = nextConfig
	a.catalog = catalogService
	a.webrtc = webrtcmedia.NewManager(a.Original)
	return a.primaryDatasourceLocked(), nil
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
	catalogService, assetIDKey, sourceKey := a.catalogSnapshot()
	page, err := catalogService.SearchAssets(request)
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
	upstreamAssetID, err := decodeClientAssetID(assetIDKey, sourceKey, assetID)
	if err != nil {
		return nil, err
	}
	return catalogService.Preview(request, upstreamAssetID)
}

// DetailPreview returns a detail-preview image response for a client.
func (a *AgentRuntime) DetailPreview(request *http.Request, assetID string) (*catalog.UpstreamMediaResponse, error) {
	catalogService, assetIDKey, sourceKey := a.catalogSnapshot()
	upstreamAssetID, err := decodeClientAssetID(assetIDKey, sourceKey, assetID)
	if err != nil {
		return nil, err
	}
	return catalogService.DetailPreview(request, upstreamAssetID)
}

// Original proxies an original asset response for a local client.
func (a *AgentRuntime) Original(request *http.Request, assetID string) (*catalog.UpstreamMediaResponse, error) {
	catalogService, assetIDKey, sourceKey := a.catalogSnapshot()
	upstreamAssetID, err := decodeClientAssetID(assetIDKey, sourceKey, assetID)
	if err != nil {
		return nil, err
	}
	return catalogService.Original(request, upstreamAssetID)
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

func (a *AgentRuntime) catalogSnapshot() (*catalog.Service, []byte, string) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.catalog, append([]byte(nil), a.assetIDKey...), a.primaryDatasourceSourceKeyLocked()
}

func (a *AgentRuntime) primaryDatasourceSourceKeyLocked() string {
	if len(a.config.Datasources) == 0 {
		return ""
	}
	return strings.TrimSpace(a.config.Datasources[0].SourceKey)
}

func signAssetSearchPage(
	page catalog.AssetSearchPage,
	assetIDKey []byte,
	sourceKey string,
) (catalog.AssetSearchPage, error) {
	if strings.TrimSpace(sourceKey) == "" {
		return catalog.AssetSearchPage{}, catalog.ErrNoDatasourceConfigured
	}
	for index := range page.Items {
		assetID, err := encodeTimichAssetID(assetIDKey, sourceKey, page.Items[index].ID)
		if err != nil {
			return catalog.AssetSearchPage{}, err
		}
		page.Items[index].ID = assetID
	}
	return page, nil
}

func decodeClientAssetID(assetIDKey []byte, expectedSourceKey string, assetID string) (string, error) {
	sourceKey, upstreamAssetID, err := decodeTimichAssetID(assetIDKey, assetID)
	if err != nil {
		return "", catalog.ErrAssetNotFound
	}
	if strings.TrimSpace(expectedSourceKey) == "" || sourceKey != strings.TrimSpace(expectedSourceKey) {
		return "", catalog.ErrAssetNotFound
	}
	return upstreamAssetID, nil
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
