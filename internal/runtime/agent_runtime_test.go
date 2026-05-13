package runtime

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/pairing"
	"github.com/rsahara/timich-agent/internal/store"
)

func TestResponsesUseRedactedConfigAndBuildDefaults(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name:        "home-immich",
			Kind:        "immich",
			URL:         "https://immich.example",
			AccessToken: "secret-token",
		},
	})

	status := runtime.StatusResponse()
	if status.Version != "dev" {
		t.Fatalf("Version = %q, want dev", status.Version)
	}
	if status.Commit != "" || status.BuiltAt != "" {
		t.Fatalf("Commit/BuiltAt = %q/%q, want unknown values omitted", status.Commit, status.BuiltAt)
	}
	if status.Mode != "remote_browsing+local" {
		t.Fatalf("Mode = %q, want remote_browsing+local", status.Mode)
	}
	if !status.SessionKeyReady {
		t.Fatal("SessionKeyReady = false, want true")
	}
	if !status.AdminAuthReady {
		t.Fatal("AdminAuthReady = false, want true")
	}
	if len(status.Datasources) != 1 || !status.Datasources[0].HasAccessToken {
		t.Fatalf("Datasources = %+v, want redacted datasource with token presence", status.Datasources)
	}

	configResponse := runtime.ConfigResponse()
	if configResponse.Datasources[0].URL != "https://immich.example" {
		t.Fatalf("Datasource URL = %q, want upstream URL", configResponse.Datasources[0].URL)
	}
	if !configResponse.RemoteBrowsing.Enabled {
		t.Fatal("RemoteBrowsing.Enabled = false, want true")
	}
}

func TestInfoResponseReportsNoDatasourcePairingStatus(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{
		Version: "test-version",
		Commit:  "test-commit",
		BuiltAt: "2026-04-25T00:00:00Z",
	}, nil)

	info := runtime.InfoResponse()
	if info.Version != "test-version" || info.Commit != "test-commit" {
		t.Fatalf("Info build = %+v, want explicit build info", info)
	}
	if info.PairingStatus != pairing.PairingStatusNoDatasourceConfigured {
		t.Fatalf("PairingStatus = %q, want no datasource configured", info.PairingStatus)
	}
	if info.PairedDevices != 0 {
		t.Fatalf("PairedDevices = %d, want 0", info.PairedDevices)
	}
	if !reflect.DeepEqual(info.Hosted, info.RemoteBrowsing) {
		t.Fatalf("Hosted = %+v, want remote browsing compatibility alias %+v", info.Hosted, info.RemoteBrowsing)
	}
	payload, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("json.Marshal(info) error = %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("json.Unmarshal(info) error = %v", err)
	}
	if _, ok := body["hosted"]; !ok {
		t.Fatalf("InfoResponse JSON = %s, want hosted compatibility alias", payload)
	}
	if _, ok := body["remoteBrowsing"]; !ok {
		t.Fatalf("InfoResponse JSON = %s, want remoteBrowsing field", payload)
	}
}

func TestInfoResponseHidesAgentIDUntilRelayCredentialRegistered(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name:        "Home Immich",
			Kind:        "immich",
			URL:         "http://immich.local:2283",
			AccessToken: "immich-token",
		},
	})

	if got := runtime.InfoResponse().AgentID; got != "" {
		t.Fatalf("InfoResponse().AgentID = %q, want hidden before relay registration", got)
	}
	if got := runtime.StatusResponse().AgentID; got != "agent-test" {
		t.Fatalf("StatusResponse().AgentID = %q, want admin status to keep agent ID", got)
	}

	if err := runtime.UpdateRelayCredentialSyncedAt(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("UpdateRelayCredentialSyncedAt() error = %v", err)
	}
	if got := runtime.InfoResponse().AgentID; got != "agent-test" {
		t.Fatalf("InfoResponse().AgentID = %q, want visible after relay registration", got)
	}
}

func TestSetAdminTokenPersistsFirstRunToken(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "")
	if runtime.AdminAuthReady() {
		t.Fatal("AdminAuthReady = true, want first-run setup")
	}
	if err := runtime.SetAdminToken("created-admin-token"); err != nil {
		t.Fatalf("SetAdminToken() error = %v", err)
	}
	if !runtime.AuthenticateAdminToken("created-admin-token") {
		t.Fatal("created admin token was not accepted")
	}
	if err := runtime.SetAdminToken("different-admin-token"); !errors.Is(err, ErrAdminTokenAlreadyConfigured) {
		t.Fatalf("SetAdminToken() second error = %v, want %v", err, ErrAdminTokenAlreadyConfigured)
	}
}

func TestRelayCredentialSyncPreservesFirstRunAdminToken(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "")
	if err := runtime.SetAdminToken("created-admin-token"); err != nil {
		t.Fatalf("SetAdminToken() error = %v", err)
	}

	syncedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := runtime.UpdateRelayCredentialSyncedAt(syncedAt); err != nil {
		t.Fatalf("UpdateRelayCredentialSyncedAt() error = %v", err)
	}

	reloaded, err := store.LoadOrCreate(runtime.ConfigResponse().DataDir)
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	if reloaded.State.AdminToken != "created-admin-token" {
		t.Fatalf("AdminToken = %q, want created token preserved", reloaded.State.AdminToken)
	}
	if reloaded.State.RelayCredentialSyncedAt == nil || !reloaded.State.RelayCredentialSyncedAt.Equal(syncedAt) {
		t.Fatalf("RelayCredentialSyncedAt = %v, want %v", reloaded.State.RelayCredentialSyncedAt, syncedAt)
	}
}

func TestRemoteRegistrationReadyRequiresDatasourceAndPairedDevice(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name:        "Home Immich",
			Kind:        "immich",
			URL:         "http://immich.local:2283",
			AccessToken: "immich-token",
		},
	})

	ready, reason := runtime.RemoteRegistrationReady()
	if ready || !strings.Contains(reason, "paired device") {
		t.Fatalf("RemoteRegistrationReady() = %v/%q, want paired device blocker", ready, reason)
	}
	status := runtime.StatusResponse()
	if len(status.SetupTasks) != 4 {
		t.Fatalf("SetupTasks length = %d, want 4", len(status.SetupTasks))
	}

	pairingSession, err := runtime.CreatePairingSession()
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}
	if _, err := runtime.RedeemPairing(pairingSession.PairingCode, "Test iPhone", "http://127.0.0.1:8082"); err != nil {
		t.Fatalf("RedeemPairing() error = %v", err)
	}

	ready, reason = runtime.RemoteRegistrationReady()
	if !ready || reason != "" {
		t.Fatalf("RemoteRegistrationReady() = %v/%q, want ready", ready, reason)
	}
	if got := runtime.StatusResponse().RemoteBrowsing.RegistrationStatus; got != "pending" {
		t.Fatalf("RegistrationStatus = %q, want pending before relay sync", got)
	}
}

func TestRemoteRegistrationReadyAllowsStaticDemoWithoutAccessToken(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name: "Static demo",
			Kind: config.DatasourceKindStaticDemo,
			URL:  writeStaticDemoManifest(t),
		},
	})
	if status := runtime.StatusResponse(); len(status.Datasources) != 1 || status.Datasources[0].HasAccessToken {
		t.Fatalf("Datasources = %+v, want static demo datasource without access token", status.Datasources)
	}

	pairingSession, err := runtime.CreatePairingSession()
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}
	if _, err := runtime.RedeemPairing(pairingSession.PairingCode, "Test iPhone", "http://127.0.0.1:8082"); err != nil {
		t.Fatalf("RedeemPairing() error = %v", err)
	}

	ready, reason := runtime.RemoteRegistrationReady()
	if !ready || reason != "" {
		t.Fatalf("RemoteRegistrationReady() = %v/%q, want ready", ready, reason)
	}
}

func TestSearchAssetsReturnsSignedTimichAssetIDs(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name: "Static demo",
			Kind: config.DatasourceKindStaticDemo,
			URL:  writeStaticDemoManifest(t),
		},
	})

	page, err := runtime.SearchAssets(catalog.AssetSearchRequest{
		Collection: catalog.AssetCollectionRequest{Kind: catalog.CollectionKindTimeline},
		Page:       catalog.AssetSearchPageRequest{Index: 0, Size: 1},
	})
	if err != nil {
		t.Fatalf("SearchAssets() error = %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("Items length = %d, want 1", len(page.Items))
	}
	if !strings.HasPrefix(page.Items[0].ID, timichAssetIDPrefix) {
		t.Fatalf("asset ID = %q, want Timich asset ID prefix", page.Items[0].ID)
	}

	sourceKey, upstreamAssetID, err := decodeTimichAssetID(runtime.assetIDKey, page.Items[0].ID)
	if err != nil {
		t.Fatalf("decodeTimichAssetID() error = %v", err)
	}
	if sourceKey != runtime.config.Datasources[0].SourceKey {
		t.Fatalf("sourceKey = %q, want %q", sourceKey, runtime.config.Datasources[0].SourceKey)
	}
	if upstreamAssetID != "demo-0001" {
		t.Fatalf("upstreamAssetID = %q, want demo-0001", upstreamAssetID)
	}
}

func TestMediaRoutesRejectUnsignedAssetIDs(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name: "Static demo",
			Kind: config.DatasourceKindStaticDemo,
			URL:  writeStaticDemoManifest(t),
		},
	})

	if _, err := runtime.Preview(nil, "demo-0001"); !errors.Is(err, catalog.ErrAssetNotFound) {
		t.Fatalf("Preview(unsigned ID) error = %v, want ErrAssetNotFound", err)
	}
}

func TestUpdatePrimaryDatasourcePersistsAndPreservesToken(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name:        "Old",
			Kind:        "immich",
			URL:         "http://old-immich.local:2283",
			AccessToken: "existing-token",
		},
	})

	updated, err := runtime.UpdatePrimaryDatasource(config.DatasourceConfig{
		Name: "Home Immich",
		Kind: "immich",
		URL:  "http://immich.local:2283",
	})
	if err != nil {
		t.Fatalf("UpdatePrimaryDatasource() error = %v", err)
	}
	if !updated.Configured || updated.Name != "Home Immich" || updated.URL != "http://immich.local:2283" || !updated.HasAccessToken {
		t.Fatalf("updated datasource = %+v", updated)
	}

	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Datasources[0].AccessToken != "existing-token" {
		t.Fatalf("AccessToken = %q, want preserved token", loaded.Datasources[0].AccessToken)
	}
}

func TestUpdatePrimaryDatasourceAllowsStaticDemoWithoutAccessToken(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name:        "Old",
			Kind:        "immich",
			URL:         "http://old-immich.local:2283",
			AccessToken: "existing-token",
		},
	})

	updated, err := runtime.UpdatePrimaryDatasource(config.DatasourceConfig{
		Name: "Static demo",
		Kind: config.DatasourceKindStaticDemo,
		URL:  writeStaticDemoManifest(t),
	})
	if err != nil {
		t.Fatalf("UpdatePrimaryDatasource() error = %v", err)
	}
	if !updated.Configured || updated.Name != "Static demo" || updated.HasAccessToken {
		t.Fatalf("updated datasource = %+v, want static demo without access token", updated)
	}

	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Datasources[0].AccessToken != "" {
		t.Fatalf("AccessToken = %q, want empty token for static demo", loaded.Datasources[0].AccessToken)
	}
}

func TestUpdatePrimaryDatasourceDoesNotPersistRuntimeOverrides(t *testing.T) {
	t.Setenv("TIMICH_AGENT_NAME", "env-agent")

	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "agent.json")
	cfg := config.Default()
	cfg.AdminListenAddress = "127.0.0.1:8081"
	cfg.DataDir = "state"
	cfg.Datasources = []config.DatasourceConfig{
		{
			Name:        "Old",
			Kind:        "immich",
			URL:         "http://old-immich.local:2283",
			AccessToken: "existing-token",
		},
	}
	if err := config.WriteFile(configPath, cfg); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	resolved, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	resolved, err = config.ApplyRuntimeOverrides(resolved, "0.0.0.0:18081", "", filepath.Join(dataDir, "runtime-state"))
	if err != nil {
		t.Fatalf("ApplyRuntimeOverrides() error = %v", err)
	}

	signingKey := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	relayPublicKey, relayPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	runtime, err := NewAgentRuntime(BuildInfo{}, resolved, store.LoadedState{
		Path: filepath.Join(dataDir, "agent-state.json"),
		State: store.State{
			AgentID:           "agent-test",
			CreatedAt:         time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			SessionSigningKey: signingKey,
			AdminToken:        "test-admin-token",
			RelayKeyID:        "relay-key",
			RelayPrivateKey:   base64.RawStdEncoding.EncodeToString(relayPrivateKey),
			RelayPublicKey:    base64.RawStdEncoding.EncodeToString(relayPublicKey),
		},
	}, time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}

	if _, err := runtime.UpdatePrimaryDatasource(config.DatasourceConfig{
		Name: "Home Immich",
		Kind: "immich",
		URL:  "http://immich.local:2283",
	}); err != nil {
		t.Fatalf("UpdatePrimaryDatasource() error = %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	written := string(raw)
	if strings.Contains(written, "env-agent") {
		t.Fatalf("config file persisted env override: %s", written)
	}
	if strings.Contains(written, "18081") {
		t.Fatalf("config file persisted serve-time admin override: %s", written)
	}
	if strings.Contains(written, "runtime-state") {
		t.Fatalf("config file persisted serve-time data-dir override: %s", written)
	}
}

func TestUpdatePrimaryDatasourceErrorDoesNotMutateLiveConfig(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name:        "Old",
			Kind:        "immich",
			URL:         "http://old-immich.local:2283",
			AccessToken: "existing-token",
		},
	})

	if _, err := runtime.UpdatePrimaryDatasource(config.DatasourceConfig{
		Name:        "Rejected",
		Kind:        "unsupported",
		URL:         "http://immich.local:2283",
		AccessToken: "new-token",
	}); err == nil {
		t.Fatal("UpdatePrimaryDatasource() error = nil, want validation error")
	}

	response := runtime.ConfigResponse()
	if len(response.Datasources) != 1 {
		t.Fatalf("Datasources length = %d, want 1", len(response.Datasources))
	}
	if response.Datasources[0].Name != "Old" || response.Datasources[0].Kind != "immich" || response.Datasources[0].URL != "http://old-immich.local:2283" {
		t.Fatalf("live datasource mutated after failed update: %+v", response.Datasources[0])
	}
}

func newTestAgentRuntime(t *testing.T, build BuildInfo, datasources []config.DatasourceConfig) *AgentRuntime {
	t.Helper()

	return newTestAgentRuntimeWithAdminToken(t, build, datasources, "test-admin-token")
}

func writeStaticDemoManifest(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "manifest.json")
	raw := []byte(`{"version":1,"assets":[{"id":"demo-0001","type":"IMAGE","originalFileName":"demo.jpg","fileCreatedAt":"2026-01-01T00:00:00Z","previewPath":"preview.jpg","detailPreviewPath":"detail_preview.jpg","originalPath":"original.jpg"}]}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func newTestAgentRuntimeWithAdminToken(
	t *testing.T,
	build BuildInfo,
	datasources []config.DatasourceConfig,
	adminToken string,
) *AgentRuntime {
	t.Helper()

	dataDir := t.TempDir()
	cfg := config.ResolvedConfig{
		Config: config.Default(),
	}
	cfg.AgentName = "test-agent"
	cfg.DataDir = dataDir
	cfg.ConfigSource = "test"
	cfg.ConfigPath = filepath.Join(dataDir, "agent.json")
	cfg.Hosted.Enabled = true
	cfg.Hosted.ServerURL = "https://timich.example"
	cfg.Datasources = datasources

	signingKey := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	relayPublicKey, relayPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	runtime, err := NewAgentRuntime(build, cfg, store.LoadedState{
		Path: filepath.Join(dataDir, "agent-state.json"),
		State: store.State{
			AgentID:           "agent-test",
			CreatedAt:         time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			SessionSigningKey: signingKey,
			AdminToken:        adminToken,
			RelayKeyID:        "relay-key",
			RelayPrivateKey:   base64.RawStdEncoding.EncodeToString(relayPrivateKey),
			RelayPublicKey:    base64.RawStdEncoding.EncodeToString(relayPublicKey),
		},
	}, time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	return runtime
}
