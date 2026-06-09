package runtime

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
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

func TestStatusAndConfigUsePassiveUploadRootSummaries(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	runtime.mu.Lock()
	runtime.config.UploadRoots = []config.UploadRootConfig{{
		Key:  "nas-photos",
		Path: filepath.Join(t.TempDir(), "missing-root"),
	}}
	runtime.mu.Unlock()

	status := runtime.StatusResponse()
	if len(status.UploadRoots) != 1 {
		t.Fatalf("Status UploadRoots length = %d, want 1", len(status.UploadRoots))
	}
	if status.UploadRoots[0].Key != "nas-photos" || status.UploadRoots[0].Status != "" || status.UploadRoots[0].Message != "" || status.UploadRoots[0].Writable {
		t.Fatalf("Status UploadRoots[0] = %+v, want passive key/path summary", status.UploadRoots[0])
	}

	configResponse := runtime.ConfigResponse()
	if len(configResponse.UploadRoots) != 1 {
		t.Fatalf("Config UploadRoots length = %d, want 1", len(configResponse.UploadRoots))
	}
	if configResponse.UploadRoots[0].Key != "nas-photos" || configResponse.UploadRoots[0].Status != "" || configResponse.UploadRoots[0].Message != "" || configResponse.UploadRoots[0].Writable {
		t.Fatalf("Config UploadRoots[0] = %+v, want passive key/path summary", configResponse.UploadRoots[0])
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

func TestNewAgentRuntimeCreatesUploadStore(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	if runtime.uploads == nil {
		t.Fatal("upload store is nil")
	}
	if runtime.uploads.Path() != filepath.Join(runtime.config.DataDir, "uploads.db") {
		t.Fatalf("upload store path = %q, want uploads.db under data dir", runtime.uploads.Path())
	}
	if _, err := os.Stat(runtime.uploads.Path()); err != nil {
		t.Fatalf("Stat(upload store) error = %v", err)
	}
}

func TestRedeemPairingCreatesDisabledDeviceUploadProfile(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name: "Static demo",
			Kind: config.DatasourceKindStaticDemo,
			URL:  writeStaticDemoManifest(t),
		},
	})

	pairingSession, err := runtime.CreatePairingSession()
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}
	bundle, err := runtime.RedeemPairing(pairingSession.PairingCode, "Test iPhone", "http://127.0.0.1:8082")
	if err != nil {
		t.Fatalf("RedeemPairing() error = %v", err)
	}

	profiles, err := store.LoadOrCreateDeviceProfileStore(runtime.config.DataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceProfileStore() error = %v", err)
	}
	profile, err := profiles.LoadProfile(bundle.DeviceID)
	if err != nil {
		t.Fatalf("LoadProfile() error = %v", err)
	}
	if profile.DeviceName != "Test iPhone" {
		t.Fatalf("DeviceName = %q, want Test iPhone", profile.DeviceName)
	}
	if profile.Upload.Enabled {
		t.Fatal("Upload.Enabled = true, want disabled default")
	}
	if profile.Upload.PathPattern == "" {
		t.Fatal("PathPattern is empty, want default")
	}
}

func TestUpdateDeviceRenamesRegistryAndProfile(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name: "Static demo",
			Kind: config.DatasourceKindStaticDemo,
			URL:  writeStaticDemoManifest(t),
		},
	})
	pairingSession, err := runtime.CreatePairingSession()
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}
	bundle, err := runtime.RedeemPairing(pairingSession.PairingCode, "Old iPhone", "http://127.0.0.1:8082")
	if err != nil {
		t.Fatalf("RedeemPairing() error = %v", err)
	}
	if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled:     false,
		PathPattern: "{deviceName}/{filename}",
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy() error = %v", err)
	}

	renamed, err := runtime.UpdateDevice(bundle.DeviceID, DeviceUpdate{DeviceName: " Alice iPhone "})
	if err != nil {
		t.Fatalf("UpdateDevice() error = %v", err)
	}
	if renamed.DeviceName != "Alice iPhone" {
		t.Fatalf("DeviceName = %q, want Alice iPhone", renamed.DeviceName)
	}
	profiles, err := store.LoadOrCreateDeviceProfileStore(runtime.config.DataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceProfileStore() error = %v", err)
	}
	profile, err := profiles.LoadProfile(bundle.DeviceID)
	if err != nil {
		t.Fatalf("LoadProfile() error = %v", err)
	}
	if profile.DeviceName != "Alice iPhone" {
		t.Fatalf("profile.DeviceName = %q, want Alice iPhone", profile.DeviceName)
	}
	if profile.Upload.PathPattern != "{deviceName}/{filename}" {
		t.Fatalf("PathPattern = %q, want existing upload policy preserved", profile.Upload.PathPattern)
	}
	if _, err := runtime.UpdateDevice(bundle.DeviceID, DeviceUpdate{DeviceName: " "}); !errors.Is(err, store.ErrDeviceNameInvalid) {
		t.Fatalf("UpdateDevice(empty) error = %v, want %v", err, store.ErrDeviceNameInvalid)
	}
}

func TestRevokeDeviceDeletesDeviceUploadProfile(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name: "Static demo",
			Kind: config.DatasourceKindStaticDemo,
			URL:  writeStaticDemoManifest(t),
		},
	})
	pairingSession, err := runtime.CreatePairingSession()
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}
	bundle, err := runtime.RedeemPairing(pairingSession.PairingCode, "Test iPhone", "http://127.0.0.1:8082")
	if err != nil {
		t.Fatalf("RedeemPairing() error = %v", err)
	}
	capturedAt := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	uploaded, err := runtime.uploads.CreateSession(store.UploadSessionInput{
		UploadID:           "upload-revoke",
		DeviceID:           bundle.DeviceID,
		SourceAssetID:      "asset-revoke",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
		CapturedAt:         &capturedAt,
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/upload-revoke.part",
		Now:                capturedAt,
	})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	asset, err := runtime.uploads.ReserveUploadCommit(store.UploadCommitInput{
		UploadID:           uploaded.UploadID,
		DeviceID:           uploaded.DeviceID,
		SourceAssetID:      uploaded.SourceAssetID,
		SourceAssetVersion: uploaded.SourceAssetVersion,
		MediaType:          uploaded.MediaType,
		OriginalFilename:   uploaded.OriginalFilename,
		CapturedAt:         uploaded.CapturedAt,
		FinalRelativePath:  "Test iPhone/2026-05-31/IMG_0001.HEIC",
		Now:                capturedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ReserveUploadCommit() error = %v", err)
	}
	if _, err := runtime.uploads.MarkUploaded(asset.ID, capturedAt.Add(2*time.Minute)); err != nil {
		t.Fatalf("MarkUploaded() error = %v", err)
	}

	if err := runtime.RevokeDevice(bundle.DeviceID); err != nil {
		t.Fatalf("RevokeDevice() error = %v", err)
	}
	profiles, err := store.LoadOrCreateDeviceProfileStore(runtime.config.DataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceProfileStore() error = %v", err)
	}
	if _, err := profiles.LoadProfile(bundle.DeviceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadProfile() error = %v, want not exist", err)
	}
	if _, ok, err := runtime.uploads.GetUploadedAssetBySourceIdentity(bundle.DeviceID, "asset-revoke", "version-1"); err != nil || ok {
		t.Fatalf("GetUploadedAssetBySourceIdentity() ok=%v error=%v, want removed upload state", ok, err)
	}
	if _, ok, err := runtime.uploads.GetSession("upload-revoke"); err != nil || ok {
		t.Fatalf("GetSession() ok=%v error=%v, want removed upload session", ok, err)
	}
}

func TestRemoveUploadTempFilesCountsOnlyDeletedFiles(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	rootPath := t.TempDir()
	runtime.mu.Lock()
	runtime.config.UploadRoots = []config.UploadRootConfig{{
		Key:  "nas-photos",
		Path: rootPath,
	}}
	runtime.mu.Unlock()

	removed, cleanupErrors := runtime.removeUploadTempFiles([]store.UploadTempFile{{
		RootKey:      "nas-photos",
		RelativePath: ".timich-upload-tmp/missing.part",
	}})
	if removed != 0 || len(cleanupErrors) != 0 {
		t.Fatalf("missing temp removed=%d errors=%v, want 0/no errors", removed, cleanupErrors)
	}

	tempDir := filepath.Join(rootPath, ".timich-upload-tmp")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	tempPath := filepath.Join(tempDir, "upload.part")
	if err := os.WriteFile(tempPath, []byte("partial"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	removed, cleanupErrors = runtime.removeUploadTempFiles([]store.UploadTempFile{{
		RootKey:      "nas-photos",
		RelativePath: ".timich-upload-tmp/upload.part",
	}})
	if removed != 1 || len(cleanupErrors) != 0 {
		t.Fatalf("existing temp removed=%d errors=%v, want 1/no errors", removed, cleanupErrors)
	}
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(tempPath) error = %v, want removed", err)
	}
}

func TestRunUploadMaintenanceOnceRemovesExpiredSessionAndOrphanTemps(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	rootPath := t.TempDir()
	runtime.mu.Lock()
	runtime.config.UploadRoots = []config.UploadRootConfig{{
		Key:  "nas-photos",
		Path: rootPath,
	}}
	runtime.mu.Unlock()

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	expiredAt := now.Add(-time.Hour)
	if _, err := runtime.uploads.CreateSession(store.UploadSessionInput{
		UploadID:           "upload-expired",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-expired",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "expired.HEIC",
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/upload-expired.part",
		ExpiresAt:          &expiredAt,
		Now:                now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateSession(expired) error = %v", err)
	}

	tempDir := filepath.Join(rootPath, ".timich-upload-tmp")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(tempDir) error = %v", err)
	}
	knownTemp := filepath.Join(tempDir, "upload-expired.part")
	if err := os.WriteFile(knownTemp, []byte("known"), 0o600); err != nil {
		t.Fatalf("WriteFile(knownTemp) error = %v", err)
	}
	orphanTemp := filepath.Join(tempDir, "upl_orphan.part")
	if err := os.WriteFile(orphanTemp, []byte("orphan"), 0o600); err != nil {
		t.Fatalf("WriteFile(orphanTemp) error = %v", err)
	}
	oldTime := now.Add(-25 * time.Hour)
	if err := os.Chtimes(orphanTemp, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes(orphanTemp) error = %v", err)
	}
	freshTemp := filepath.Join(tempDir, "upl_fresh.part")
	if err := os.WriteFile(freshTemp, []byte("fresh"), 0o600); err != nil {
		t.Fatalf("WriteFile(freshTemp) error = %v", err)
	}
	if err := os.Chtimes(freshTemp, now, now); err != nil {
		t.Fatalf("Chtimes(freshTemp) error = %v", err)
	}

	result, err := runtime.RunUploadMaintenanceOnce(now)
	if err != nil {
		t.Fatalf("RunUploadMaintenanceOnce() error = %v", err)
	}
	if result.RemovedExpiredSessions != 1 {
		t.Fatalf("RemovedExpiredSessions = %d, want 1", result.RemovedExpiredSessions)
	}
	if result.RemovedTempFiles != 1 {
		t.Fatalf("RemovedTempFiles = %d, want 1 known temp", result.RemovedTempFiles)
	}
	if result.RemovedOrphanTempFiles != 1 {
		t.Fatalf("RemovedOrphanTempFiles = %d, want 1 orphan temp", result.RemovedOrphanTempFiles)
	}
	if len(result.TempCleanupErrors) != 0 {
		t.Fatalf("TempCleanupErrors = %v, want none", result.TempCleanupErrors)
	}
	if _, ok, err := runtime.uploads.GetSession("upload-expired"); err != nil || ok {
		t.Fatalf("GetSession(expired) ok=%v err=%v, want removed", ok, err)
	}
	for _, removedPath := range []string{knownTemp, orphanTemp} {
		if _, err := os.Stat(removedPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Stat(%s) error = %v, want removed", removedPath, err)
		}
	}
	if _, err := os.Stat(freshTemp); err != nil {
		t.Fatalf("Stat(freshTemp) error = %v, want retained", err)
	}
}

func TestStartUploadSessionAcceptedResumableAndAlreadyUploaded(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	rootPath := t.TempDir()
	runtime.mu.Lock()
	runtime.config.UploadRoots = []config.UploadRootConfig{{Key: "nas-photos", Path: rootPath}}
	runtime.mu.Unlock()

	bundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled:     true,
		RootKey:     "nas-photos",
		PathPattern: store.DefaultUploadPathPattern(),
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy() error = %v", err)
	}
	capturedAt := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	expectedSize := int64(1024)
	input := UploadSessionStartInput{
		SourceAssetID:      "asset-1",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
		CapturedAt:         &capturedAt,
		ExpectedSizeBytes:  &expectedSize,
	}

	created, err := runtime.StartUploadSession(bundle.DeviceID, input)
	if err != nil {
		t.Fatalf("StartUploadSession() error = %v", err)
	}
	if created.State != "accepted" || created.Session == nil || created.Session.NextOffset != 0 {
		t.Fatalf("created response = %+v, want accepted session", created)
	}
	if created.Session.ChunkSizeBytes != defaultUploadChunkSizeBytes {
		t.Fatalf("ChunkSizeBytes = %d, want %d", created.Session.ChunkSizeBytes, defaultUploadChunkSizeBytes)
	}

	resumed, err := runtime.StartUploadSession(bundle.DeviceID, input)
	if err != nil {
		t.Fatalf("StartUploadSession(resume) error = %v", err)
	}
	if resumed.State != "resumable" || resumed.Session == nil || resumed.Session.UploadID != created.Session.UploadID {
		t.Fatalf("resumed response = %+v, want same resumable session", resumed)
	}

	asset, err := runtime.uploads.ReserveUploadCommit(store.UploadCommitInput{
		UploadID:           created.Session.UploadID,
		DeviceID:           bundle.DeviceID,
		SourceAssetID:      input.SourceAssetID,
		SourceAssetVersion: input.SourceAssetVersion,
		MediaType:          input.MediaType,
		OriginalFilename:   input.OriginalFilename,
		CapturedAt:         input.CapturedAt,
		ExpectedSizeBytes:  input.ExpectedSizeBytes,
		FinalRelativePath:  "Upload iPhone/2026-05-31/IMG_0001.HEIC",
		Now:                capturedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ReserveUploadCommit() error = %v", err)
	}
	commitBlocked, err := runtime.StartUploadSession(bundle.DeviceID, input)
	if err != nil {
		t.Fatalf("StartUploadSession(commit blocked) error = %v", err)
	}
	if commitBlocked.State != "blocked" || commitBlocked.UploadedAsset == nil || commitBlocked.UploadedAsset.Status != "committing" {
		t.Fatalf("commit blocked response = %+v, want committing blocked response", commitBlocked)
	}
	if commitBlocked.Status == nil || commitBlocked.Status.State != "blocked" || commitBlocked.Reason == "" {
		t.Fatalf("commit blocked status = %+v reason=%q, want blocked status with reason", commitBlocked.Status, commitBlocked.Reason)
	}
	committedAt := capturedAt.Add(2 * time.Minute)
	if _, err := runtime.uploads.MarkUploaded(asset.ID, committedAt); err != nil {
		t.Fatalf("MarkUploaded() error = %v", err)
	}
	alreadyUploaded, err := runtime.StartUploadSession(bundle.DeviceID, input)
	if err != nil {
		t.Fatalf("StartUploadSession(already uploaded) error = %v", err)
	}
	if alreadyUploaded.State != "already_uploaded" || alreadyUploaded.UploadedAsset == nil {
		t.Fatalf("already uploaded response = %+v, want uploaded asset", alreadyUploaded)
	}
	state, err := runtime.AppUploadState(bundle.DeviceID)
	if err != nil {
		t.Fatalf("AppUploadState() error = %v", err)
	}
	if state.LatestCommittedUploadAt == nil || !state.LatestCommittedUploadAt.Equal(committedAt) {
		t.Fatalf("LatestCommittedUploadAt = %v, want %v", state.LatestCommittedUploadAt, committedAt)
	}
}

func TestStartUploadSessionBlockedByPolicy(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	bundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	response, err := runtime.StartUploadSession(bundle.DeviceID, UploadSessionStartInput{
		SourceAssetID:      "asset-1",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
	})
	if err != nil {
		t.Fatalf("StartUploadSession() error = %v", err)
	}
	if response.State != "blocked" || response.Status == nil || response.Status.State != "disabled" {
		t.Fatalf("response = %+v, want disabled blocked response", response)
	}
}

func TestStartUploadSessionBlocksResumableWhenRootPolicyChanges(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	rootPath := t.TempDir()
	archivePath := t.TempDir()
	runtime.mu.Lock()
	runtime.config.UploadRoots = []config.UploadRootConfig{
		{Key: "nas-photos", Path: rootPath},
		{Key: "archive", Path: archivePath},
	}
	runtime.mu.Unlock()
	bundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled:     true,
		RootKey:     "nas-photos",
		PathPattern: store.DefaultUploadPathPattern(),
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy() error = %v", err)
	}
	capturedAt := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	body := []byte("root policy changed")
	expectedSize := int64(len(body))
	input := UploadSessionStartInput{
		SourceAssetID:      "asset-root-change",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
		CapturedAt:         &capturedAt,
		ExpectedSizeBytes:  &expectedSize,
	}
	created, err := runtime.StartUploadSession(bundle.DeviceID, input)
	if err != nil {
		t.Fatalf("StartUploadSession() error = %v", err)
	}
	if created.State != "accepted" || created.Session == nil {
		t.Fatalf("created response = %+v, want accepted session", created)
	}
	if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled:     true,
		RootKey:     "archive",
		PathPattern: store.DefaultUploadPathPattern(),
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy(root change) error = %v", err)
	}
	state, err := runtime.AppUploadState(bundle.DeviceID)
	if err != nil {
		t.Fatalf("AppUploadState(after root change) error = %v", err)
	}
	if state.ActiveSession != nil {
		t.Fatalf("ActiveSession = %+v, want hidden after root policy change", state.ActiveSession)
	}

	resumed, err := runtime.StartUploadSession(bundle.DeviceID, input)
	if err != nil {
		t.Fatalf("StartUploadSession(resume after root change) error = %v", err)
	}
	if resumed.State != "blocked" || resumed.Reason != uploadSessionPolicyReason {
		t.Fatalf("resumed response = %+v, want root-policy blocked", resumed)
	}
	if _, err := runtime.AppendUploadChunk(bundle.DeviceID, created.Session.UploadID, UploadChunkInput{
		Offset:             0,
		ChunkSHA1Hex:       sha1Hex(body),
		Body:               bytes.NewReader(body),
		ContentLengthBytes: int64(len(body)),
	}); !errors.Is(err, ErrUploadPolicyInvalid) {
		t.Fatalf("AppendUploadChunk(after root change) error = %v, want upload policy invalid", err)
	}
}

func TestUploadSessionActionsRevalidateCapturedAfterPolicy(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	rootPath := t.TempDir()
	runtime.mu.Lock()
	runtime.config.UploadRoots = []config.UploadRootConfig{{Key: "nas-photos", Path: rootPath}}
	runtime.mu.Unlock()
	bundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled:     true,
		RootKey:     "nas-photos",
		PathPattern: store.DefaultUploadPathPattern(),
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy() error = %v", err)
	}
	capturedAt := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	appendBody := []byte("append blocked")
	appendSize := int64(len(appendBody))
	appendInput := UploadSessionStartInput{
		SourceAssetID:      "asset-append",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
		CapturedAt:         &capturedAt,
		ExpectedSizeBytes:  &appendSize,
	}
	appendSession, err := runtime.StartUploadSession(bundle.DeviceID, appendInput)
	if err != nil {
		t.Fatalf("StartUploadSession(append) error = %v", err)
	}
	if appendSession.State != "accepted" || appendSession.Session == nil {
		t.Fatalf("append session = %+v, want accepted session", appendSession)
	}
	completeBody := []byte("complete blocked")
	completeSize := int64(len(completeBody))
	completeSession, err := runtime.StartUploadSession(bundle.DeviceID, UploadSessionStartInput{
		SourceAssetID:      "asset-complete",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0002.HEIC",
		CapturedAt:         &capturedAt,
		ExpectedSizeBytes:  &completeSize,
	})
	if err != nil {
		t.Fatalf("StartUploadSession(complete) error = %v", err)
	}
	if completeSession.State != "accepted" || completeSession.Session == nil {
		t.Fatalf("complete session = %+v, want accepted session", completeSession)
	}
	if _, err := runtime.AppendUploadChunk(bundle.DeviceID, completeSession.Session.UploadID, UploadChunkInput{
		Offset:             0,
		ChunkSHA1Hex:       sha1Hex(completeBody),
		Body:               bytes.NewReader(completeBody),
		ContentLengthBytes: int64(len(completeBody)),
	}); err != nil {
		t.Fatalf("AppendUploadChunk(complete setup) error = %v", err)
	}
	abortSize := int64(1)
	abortSession, err := runtime.StartUploadSession(bundle.DeviceID, UploadSessionStartInput{
		SourceAssetID:      "asset-abort",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0003.HEIC",
		CapturedAt:         &capturedAt,
		ExpectedSizeBytes:  &abortSize,
	})
	if err != nil {
		t.Fatalf("StartUploadSession(abort) error = %v", err)
	}
	if abortSession.State != "accepted" || abortSession.Session == nil {
		t.Fatalf("abort session = %+v, want accepted session", abortSession)
	}
	capturedAfter := capturedAt.Add(time.Hour)
	if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled:       true,
		RootKey:       "nas-photos",
		PathPattern:   store.DefaultUploadPathPattern(),
		CapturedAfter: &capturedAfter,
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy(capturedAfter) error = %v", err)
	}
	state, err := runtime.AppUploadState(bundle.DeviceID)
	if err != nil {
		t.Fatalf("AppUploadState(after capturedAfter) error = %v", err)
	}
	if state.ActiveSession != nil {
		t.Fatalf("ActiveSession = %+v, want hidden after captured-after policy change", state.ActiveSession)
	}

	resumed, err := runtime.StartUploadSession(bundle.DeviceID, appendInput)
	if err != nil {
		t.Fatalf("StartUploadSession(resume after capturedAfter) error = %v", err)
	}
	if resumed.State != "blocked" || resumed.Reason != uploadCapturedBeforeReason {
		t.Fatalf("resumed response = %+v, want captured-after blocked", resumed)
	}
	if _, err := runtime.AppendUploadChunk(bundle.DeviceID, appendSession.Session.UploadID, UploadChunkInput{
		Offset:             0,
		ChunkSHA1Hex:       sha1Hex(appendBody),
		Body:               bytes.NewReader(appendBody),
		ContentLengthBytes: int64(len(appendBody)),
	}); !errors.Is(err, ErrUploadPolicyInvalid) {
		t.Fatalf("AppendUploadChunk(after capturedAfter) error = %v, want upload policy invalid", err)
	}
	if _, err := runtime.CompleteUploadSession(bundle.DeviceID, completeSession.Session.UploadID, UploadSessionCompleteInput{
		SourceAssetVersion: "version-1",
		Checksums: []UploadSessionChecksumInput{{
			Algorithm: "sha1",
			Digest:    sha1Hex(completeBody),
		}},
	}); !errors.Is(err, ErrUploadPolicyInvalid) {
		t.Fatalf("CompleteUploadSession(after capturedAfter) error = %v, want upload policy invalid", err)
	}
	if _, err := runtime.AbortUploadSession(bundle.DeviceID, abortSession.Session.UploadID); !errors.Is(err, ErrUploadPolicyInvalid) {
		t.Fatalf("AbortUploadSession(after capturedAfter) error = %v, want upload policy invalid", err)
	}
}

func TestCompleteUploadSessionRecoversCommittingAssetAfterPolicyChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		updatePolicy func(t *testing.T, runtime *AgentRuntime, deviceID string, capturedAt time.Time)
	}{
		{
			name: "captured_after",
			updatePolicy: func(t *testing.T, runtime *AgentRuntime, deviceID string, capturedAt time.Time) {
				t.Helper()
				capturedAfter := capturedAt.Add(time.Hour)
				if _, err := runtime.UpdateDeviceUploadPolicy(deviceID, DeviceUploadPolicyUpdate{
					Enabled:       true,
					RootKey:       "nas-photos",
					PathPattern:   store.DefaultUploadPathPattern(),
					CapturedAfter: &capturedAfter,
				}); err != nil {
					t.Fatalf("UpdateDeviceUploadPolicy(capturedAfter) error = %v", err)
				}
			},
		},
		{
			name: "root_change",
			updatePolicy: func(t *testing.T, runtime *AgentRuntime, deviceID string, _ time.Time) {
				t.Helper()
				if _, err := runtime.UpdateDeviceUploadPolicy(deviceID, DeviceUploadPolicyUpdate{
					Enabled:     true,
					RootKey:     "archive",
					PathPattern: store.DefaultUploadPathPattern(),
				}); err != nil {
					t.Fatalf("UpdateDeviceUploadPolicy(root change) error = %v", err)
				}
			},
		},
		{
			name: "disabled",
			updatePolicy: func(t *testing.T, runtime *AgentRuntime, deviceID string, _ time.Time) {
				t.Helper()
				if _, err := runtime.UpdateDeviceUploadPolicy(deviceID, DeviceUploadPolicyUpdate{
					Enabled:     false,
					RootKey:     "nas-photos",
					PathPattern: store.DefaultUploadPathPattern(),
				}); err != nil {
					t.Fatalf("UpdateDeviceUploadPolicy(disabled) error = %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
			rootPath := t.TempDir()
			archivePath := t.TempDir()
			runtime.mu.Lock()
			runtime.config.UploadRoots = []config.UploadRootConfig{
				{Key: "nas-photos", Path: rootPath},
				{Key: "archive", Path: archivePath},
			}
			runtime.mu.Unlock()
			bundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
			if err != nil {
				t.Fatalf("CreateHostedSession() error = %v", err)
			}
			if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
				Enabled:     true,
				RootKey:     "nas-photos",
				PathPattern: store.DefaultUploadPathPattern(),
			}); err != nil {
				t.Fatalf("UpdateDeviceUploadPolicy() error = %v", err)
			}
			capturedAt := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
			body := []byte("recover after " + tt.name)
			expectedSize := int64(len(body))
			input := UploadSessionStartInput{
				SourceAssetID:      "asset-" + tt.name + "-recovery",
				SourceAssetVersion: "version-1",
				MediaType:          "image",
				OriginalFilename:   "IMG_0004.HEIC",
				CapturedAt:         &capturedAt,
				ExpectedSizeBytes:  &expectedSize,
			}
			session, err := runtime.StartUploadSession(bundle.DeviceID, input)
			if err != nil {
				t.Fatalf("StartUploadSession() error = %v", err)
			}
			if session.State != "accepted" || session.Session == nil {
				t.Fatalf("session = %+v, want accepted session", session)
			}
			if _, err := runtime.AppendUploadChunk(bundle.DeviceID, session.Session.UploadID, UploadChunkInput{
				Offset:             0,
				ChunkSHA1Hex:       sha1Hex(body),
				Body:               bytes.NewReader(body),
				ContentLengthBytes: int64(len(body)),
			}); err != nil {
				t.Fatalf("AppendUploadChunk() error = %v", err)
			}
			if _, err := runtime.uploads.ReserveUploadCommit(store.UploadCommitInput{
				UploadID:           session.Session.UploadID,
				DeviceID:           bundle.DeviceID,
				SourceAssetID:      input.SourceAssetID,
				SourceAssetVersion: input.SourceAssetVersion,
				MediaType:          input.MediaType,
				OriginalFilename:   input.OriginalFilename,
				CapturedAt:         input.CapturedAt,
				ExpectedSizeBytes:  input.ExpectedSizeBytes,
				FinalRelativePath:  "Upload iPhone/2026-05-31/IMG_0004.HEIC",
				Checksums: []store.UploadChecksum{{
					Algorithm: "sha1",
					Encoding:  "hex",
					Digest:    sha1Hex(body),
				}},
				Now: capturedAt.Add(time.Minute),
			}); err != nil {
				t.Fatalf("ReserveUploadCommit() error = %v", err)
			}

			tt.updatePolicy(t, runtime, bundle.DeviceID, capturedAt)

			completed, err := runtime.CompleteUploadSession(bundle.DeviceID, session.Session.UploadID, UploadSessionCompleteInput{
				SourceAssetVersion: "version-1",
				Checksums: []UploadSessionChecksumInput{{
					Algorithm: "sha1",
					Digest:    sha1Hex(body),
				}},
			})
			if err != nil {
				t.Fatalf("CompleteUploadSession(recover after policy change) error = %v", err)
			}
			if completed.State != "completed" || completed.UploadedAsset == nil {
				t.Fatalf("completed response = %+v, want recovered uploaded asset", completed)
			}
			finalPath := filepath.Join(rootPath, "Upload iPhone", "2026-05-31", "IMG_0004.HEIC")
			if raw, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(raw, body) {
				t.Fatalf("final raw=%q error=%v, want recovered body", raw, err)
			}
			if _, err := os.Stat(filepath.Join(archivePath, "Upload iPhone", "2026-05-31", "IMG_0004.HEIC")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("archive final stat error = %v, want no file in replacement root", err)
			}
		})
	}
}

func TestAppUploadStateReportsActiveSession(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	rootPath := t.TempDir()
	runtime.mu.Lock()
	runtime.config.UploadRoots = []config.UploadRootConfig{{Key: "nas-photos", Path: rootPath}}
	runtime.mu.Unlock()
	bundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled:     true,
		RootKey:     "nas-photos",
		PathPattern: store.DefaultUploadPathPattern(),
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy() error = %v", err)
	}
	session, err := runtime.StartUploadSession(bundle.DeviceID, UploadSessionStartInput{
		SourceAssetID:      "asset-1",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
	})
	if err != nil {
		t.Fatalf("StartUploadSession() error = %v", err)
	}
	state, err := runtime.AppUploadState(bundle.DeviceID)
	if err != nil {
		t.Fatalf("AppUploadState() error = %v", err)
	}
	if state.Status.State != "ready" || state.ActiveSession == nil || state.ActiveSession.UploadID != session.Session.UploadID {
		t.Fatalf("state = %+v, want ready state with active session", state)
	}
}

func TestAppendCompleteAndAbortUploadSession(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	rootPath := t.TempDir()
	runtime.mu.Lock()
	runtime.config.UploadRoots = []config.UploadRootConfig{{Key: "nas-photos", Path: rootPath}}
	runtime.mu.Unlock()
	bundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled:     true,
		RootKey:     "nas-photos",
		PathPattern: store.DefaultUploadPathPattern(),
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy() error = %v", err)
	}
	capturedAt := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	body := []byte("hello upload")
	expectedSize := int64(len(body))
	session, err := runtime.StartUploadSession(bundle.DeviceID, UploadSessionStartInput{
		SourceAssetID:      "asset-1",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG:0001.HEIC",
		CapturedAt:         &capturedAt,
		ExpectedSizeBytes:  &expectedSize,
	})
	if err != nil {
		t.Fatalf("StartUploadSession() error = %v", err)
	}
	chunk, err := runtime.AppendUploadChunk(bundle.DeviceID, session.Session.UploadID, UploadChunkInput{
		Offset:             0,
		ChunkSHA1Hex:       sha1Hex(body),
		Body:               bytes.NewReader(body),
		ContentLengthBytes: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("AppendUploadChunk() error = %v", err)
	}
	if chunk.State != "accepted" || chunk.Session == nil || chunk.Session.NextOffset != int64(len(body)) {
		t.Fatalf("chunk response = %+v, want accepted next offset", chunk)
	}
	if _, err := runtime.AppendUploadChunk(bundle.DeviceID, session.Session.UploadID, UploadChunkInput{
		Offset:             0,
		ChunkSHA1Hex:       sha1Hex(body),
		Body:               bytes.NewReader(body),
		ContentLengthBytes: int64(len(body)),
	}); !errors.Is(err, store.ErrUploadSessionOffsetConflict) {
		t.Fatalf("AppendUploadChunk(stale) error = %v, want offset conflict", err)
	}
	completed, err := runtime.CompleteUploadSession(bundle.DeviceID, session.Session.UploadID, UploadSessionCompleteInput{
		SourceAssetVersion: "version-1",
		Checksums: []UploadSessionChecksumInput{{
			Algorithm: "sha1",
			Digest:    sha1Hex(body),
		}},
	})
	if err != nil {
		t.Fatalf("CompleteUploadSession() error = %v", err)
	}
	if completed.State != "completed" || completed.UploadedAsset == nil {
		t.Fatalf("completed response = %+v, want uploaded asset", completed)
	}
	if completed.UploadedAsset.FinalRelativePath != "Upload iPhone/2026-05-31/IMG_0001.HEIC" {
		t.Fatalf("FinalRelativePath = %q, want sanitized default path", completed.UploadedAsset.FinalRelativePath)
	}
	finalPath := filepath.Join(rootPath, "Upload iPhone", "2026-05-31", "IMG_0001.HEIC")
	if raw, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(raw, body) {
		t.Fatalf("final file raw=%q error=%v, want uploaded body", raw, err)
	}
	tempPath := filepath.Join(rootPath, ".timich-upload-tmp", session.Session.UploadID+".part")
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file stat error = %v, want removed temp", err)
	}
	alreadyUploaded, err := runtime.StartUploadSession(bundle.DeviceID, UploadSessionStartInput{
		SourceAssetID:      "asset-1",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG:0001.HEIC",
		CapturedAt:         &capturedAt,
		ExpectedSizeBytes:  &expectedSize,
	})
	if err != nil {
		t.Fatalf("StartUploadSession(already uploaded) error = %v", err)
	}
	if alreadyUploaded.State != "already_uploaded" {
		t.Fatalf("already uploaded response = %+v", alreadyUploaded)
	}

	abortBody := []byte("abort me")
	abortSize := int64(len(abortBody))
	abortSession, err := runtime.StartUploadSession(bundle.DeviceID, UploadSessionStartInput{
		SourceAssetID:      "asset-abort",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0002.HEIC",
		ExpectedSizeBytes:  &abortSize,
	})
	if err != nil {
		t.Fatalf("StartUploadSession(abort) error = %v", err)
	}
	if _, err := runtime.AppendUploadChunk(bundle.DeviceID, abortSession.Session.UploadID, UploadChunkInput{
		Offset:             0,
		ChunkSHA1Hex:       sha1Hex(abortBody),
		Body:               bytes.NewReader(abortBody),
		ContentLengthBytes: int64(len(abortBody)),
	}); err != nil {
		t.Fatalf("AppendUploadChunk(abort) error = %v", err)
	}
	aborted, err := runtime.AbortUploadSession(bundle.DeviceID, abortSession.Session.UploadID)
	if err != nil {
		t.Fatalf("AbortUploadSession() error = %v", err)
	}
	if aborted.State != "aborted" || aborted.Session == nil || aborted.Session.Status != "aborted" {
		t.Fatalf("aborted response = %+v, want aborted session", aborted)
	}
}

func TestUploadSessionUsesDefaultFilesystemPermissions(t *testing.T) {
	previousUmask := syscall.Umask(0o027)
	defer syscall.Umask(previousUmask)

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	rootPath := t.TempDir()
	runtime.mu.Lock()
	runtime.config.UploadRoots = []config.UploadRootConfig{{Key: "nas-photos", Path: rootPath}}
	runtime.mu.Unlock()
	bundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled:     true,
		RootKey:     "nas-photos",
		PathPattern: store.DefaultUploadPathPattern(),
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy() error = %v", err)
	}

	capturedAt := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	body := []byte("permission check")
	expectedSize := int64(len(body))
	session, err := runtime.StartUploadSession(bundle.DeviceID, UploadSessionStartInput{
		SourceAssetID:      "asset-permissions",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
		CapturedAt:         &capturedAt,
		ExpectedSizeBytes:  &expectedSize,
	})
	if err != nil {
		t.Fatalf("StartUploadSession() error = %v", err)
	}
	if _, err := runtime.AppendUploadChunk(bundle.DeviceID, session.Session.UploadID, UploadChunkInput{
		Offset:             0,
		ChunkSHA1Hex:       sha1Hex(body),
		Body:               bytes.NewReader(body),
		ContentLengthBytes: int64(len(body)),
	}); err != nil {
		t.Fatalf("AppendUploadChunk() error = %v", err)
	}

	tempDir := filepath.Join(rootPath, ".timich-upload-tmp")
	tempPath := filepath.Join(tempDir, session.Session.UploadID+".part")
	assertPathPerm(t, tempDir, 0o750)
	assertPathPerm(t, tempPath, 0o640)

	if _, err := runtime.CompleteUploadSession(bundle.DeviceID, session.Session.UploadID, UploadSessionCompleteInput{
		SourceAssetVersion: "version-1",
		Checksums: []UploadSessionChecksumInput{{
			Algorithm: "sha1",
			Digest:    sha1Hex(body),
		}},
	}); err != nil {
		t.Fatalf("CompleteUploadSession() error = %v", err)
	}

	assertPathPerm(t, filepath.Join(rootPath, "Upload iPhone"), 0o750)
	assertPathPerm(t, filepath.Join(rootPath, "Upload iPhone", "2026-05-31"), 0o750)
	assertPathPerm(t, filepath.Join(rootPath, "Upload iPhone", "2026-05-31", "IMG_0001.HEIC"), 0o640)
}

func TestUploadSessionUsesConfiguredTempPath(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	rootPath := t.TempDir()
	tempRelativePath := "working/.timich-upload-tmp"
	runtime.mu.Lock()
	runtime.config.UploadRoots = []config.UploadRootConfig{{
		Key:      "nas-photos",
		Path:     rootPath,
		TempPath: tempRelativePath,
	}}
	runtime.mu.Unlock()
	bundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled:     true,
		RootKey:     "nas-photos",
		PathPattern: store.DefaultUploadPathPattern(),
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy() error = %v", err)
	}
	body := []byte("custom temp path")
	expectedSize := int64(len(body))
	session, err := runtime.StartUploadSession(bundle.DeviceID, UploadSessionStartInput{
		SourceAssetID:      "asset-custom-temp",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0003.HEIC",
		ExpectedSizeBytes:  &expectedSize,
	})
	if err != nil {
		t.Fatalf("StartUploadSession() error = %v", err)
	}
	if _, err := runtime.AppendUploadChunk(bundle.DeviceID, session.Session.UploadID, UploadChunkInput{
		Offset:             0,
		ChunkSHA1Hex:       sha1Hex(body),
		Body:               bytes.NewReader(body),
		ContentLengthBytes: int64(len(body)),
	}); err != nil {
		t.Fatalf("AppendUploadChunk() error = %v", err)
	}
	tempPath := filepath.Join(rootPath, "working", ".timich-upload-tmp", session.Session.UploadID+".part")
	if raw, err := os.ReadFile(tempPath); err != nil || !bytes.Equal(raw, body) {
		t.Fatalf("custom temp raw=%q error=%v, want uploaded body", raw, err)
	}
	defaultTempPath := filepath.Join(rootPath, ".timich-upload-tmp", session.Session.UploadID+".part")
	if _, err := os.Stat(defaultTempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default temp stat error = %v, want no default temp", err)
	}
	completed, err := runtime.CompleteUploadSession(bundle.DeviceID, session.Session.UploadID, UploadSessionCompleteInput{
		SourceAssetVersion: "version-1",
		Checksums: []UploadSessionChecksumInput{{
			Algorithm: "sha1",
			Digest:    sha1Hex(body),
		}},
	})
	if err != nil {
		t.Fatalf("CompleteUploadSession() error = %v", err)
	}
	if completed.State != "completed" {
		t.Fatalf("completed response = %+v, want completed", completed)
	}
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("custom temp stat error = %v, want removed temp", err)
	}
}

func TestUploadRootStatusBlocksUnusableConfiguredTempPath(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "working"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	runtime.mu.Lock()
	runtime.config.UploadRoots = []config.UploadRootConfig{{
		Key:      "nas-photos",
		Path:     rootPath,
		TempPath: "working/.timich-upload-tmp",
	}}
	runtime.mu.Unlock()

	roots := runtime.UploadRootStatuses()
	if len(roots) != 1 || roots[0].Status != "blocked" || roots[0].Writable || roots[0].Message == "" {
		t.Fatalf("UploadRootStatuses() = %+v, want blocked unusable temp path", roots)
	}
	bundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled:     true,
		RootKey:     "nas-photos",
		PathPattern: store.DefaultUploadPathPattern(),
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy() error = %v", err)
	}
	state, err := runtime.AppUploadState(bundle.DeviceID)
	if err != nil {
		t.Fatalf("AppUploadState() error = %v", err)
	}
	if state.Status.State != "blocked" || state.Status.Reason == "" {
		t.Fatalf("AppUploadState().Status = %+v, want blocked status", state.Status)
	}
	policy, err := runtime.DeviceUploadPolicy(bundle.DeviceID)
	if err != nil {
		t.Fatalf("DeviceUploadPolicy() error = %v", err)
	}
	if policy.Status.State != "blocked" || policy.Status.Reason == "" || policy.Status.Root == nil || policy.Status.Root.Writable {
		t.Fatalf("DeviceUploadPolicy().Status = %+v, want blocked unwritable root status", policy.Status)
	}
	if policy.Status.Root.Path != rootPath || policy.Status.Root.TempPath != "working/.timich-upload-tmp" {
		t.Fatalf("DeviceUploadPolicy().Status.Root = %+v, want configured root details", policy.Status.Root)
	}
	started, err := runtime.StartUploadSession(bundle.DeviceID, UploadSessionStartInput{
		SourceAssetID:      "asset-unusable-temp",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0004.HEIC",
	})
	if err != nil {
		t.Fatalf("StartUploadSession() error = %v", err)
	}
	if started.State != "blocked" || started.Session != nil {
		t.Fatalf("StartUploadSession() = %+v, want blocked without session", started)
	}
}

func TestCompleteUploadSessionUsesSuffixedPathOnCollision(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	rootPath := t.TempDir()
	runtime.mu.Lock()
	runtime.config.UploadRoots = []config.UploadRootConfig{{Key: "nas-photos", Path: rootPath}}
	runtime.mu.Unlock()
	bundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled:     true,
		RootKey:     "nas-photos",
		PathPattern: store.DefaultUploadPathPattern(),
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy() error = %v", err)
	}
	capturedAt := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	collisionDir := filepath.Join(rootPath, "Upload iPhone", "2026-05-31")
	if err := os.MkdirAll(collisionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(collisionDir, "IMG_0001.HEIC"), []byte("existing"), 0o644); err != nil {
		t.Fatalf("WriteFile(collision) error = %v", err)
	}
	body := []byte("new content")
	expectedSize := int64(len(body))
	session, err := runtime.StartUploadSession(bundle.DeviceID, UploadSessionStartInput{
		SourceAssetID:      "asset-2",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
		CapturedAt:         &capturedAt,
		ExpectedSizeBytes:  &expectedSize,
	})
	if err != nil {
		t.Fatalf("StartUploadSession() error = %v", err)
	}
	if _, err := runtime.AppendUploadChunk(bundle.DeviceID, session.Session.UploadID, UploadChunkInput{
		Offset:             0,
		ChunkSHA1Hex:       sha1Hex(body),
		Body:               bytes.NewReader(body),
		ContentLengthBytes: int64(len(body)),
	}); err != nil {
		t.Fatalf("AppendUploadChunk() error = %v", err)
	}
	completed, err := runtime.CompleteUploadSession(bundle.DeviceID, session.Session.UploadID, UploadSessionCompleteInput{
		Checksums: []UploadSessionChecksumInput{{Algorithm: "sha1", Digest: sha1Hex(body)}},
	})
	if err != nil {
		t.Fatalf("CompleteUploadSession() error = %v", err)
	}
	if completed.UploadedAsset == nil || completed.UploadedAsset.FinalRelativePath != "Upload iPhone/2026-05-31/IMG_0001-2.HEIC" {
		t.Fatalf("completed = %+v, want suffixed final path", completed)
	}
	if raw, err := os.ReadFile(filepath.Join(collisionDir, "IMG_0001-2.HEIC")); err != nil || !bytes.Equal(raw, body) {
		t.Fatalf("suffixed final raw=%q error=%v, want uploaded body", raw, err)
	}
}

func TestStartUploadSessionRecoversCommittingTempWithSuffixedPath(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	rootPath := t.TempDir()
	runtime.mu.Lock()
	runtime.config.UploadRoots = []config.UploadRootConfig{{Key: "nas-photos", Path: rootPath}}
	runtime.mu.Unlock()
	bundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	if _, err := runtime.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled:     true,
		RootKey:     "nas-photos",
		PathPattern: store.DefaultUploadPathPattern(),
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy() error = %v", err)
	}
	capturedAt := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	body := []byte("recoverable content")
	expectedSize := int64(len(body))
	input := UploadSessionStartInput{
		SourceAssetID:      "asset-recover",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
		CapturedAt:         &capturedAt,
		ExpectedSizeBytes:  &expectedSize,
	}
	session, err := runtime.StartUploadSession(bundle.DeviceID, input)
	if err != nil {
		t.Fatalf("StartUploadSession() error = %v", err)
	}
	if _, err := runtime.AppendUploadChunk(bundle.DeviceID, session.Session.UploadID, UploadChunkInput{
		Offset:             0,
		ChunkSHA1Hex:       sha1Hex(body),
		Body:               bytes.NewReader(body),
		ContentLengthBytes: int64(len(body)),
	}); err != nil {
		t.Fatalf("AppendUploadChunk() error = %v", err)
	}
	collisionDir := filepath.Join(rootPath, "Upload iPhone", "2026-05-31")
	if err := os.MkdirAll(collisionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(collisionDir, "IMG_0001.HEIC"), []byte("different existing file"), 0o644); err != nil {
		t.Fatalf("WriteFile(collision) error = %v", err)
	}
	if _, err := runtime.uploads.ReserveUploadCommit(store.UploadCommitInput{
		UploadID:           session.Session.UploadID,
		DeviceID:           bundle.DeviceID,
		SourceAssetID:      input.SourceAssetID,
		SourceAssetVersion: input.SourceAssetVersion,
		MediaType:          input.MediaType,
		OriginalFilename:   input.OriginalFilename,
		CapturedAt:         input.CapturedAt,
		ExpectedSizeBytes:  input.ExpectedSizeBytes,
		FinalRelativePath:  "Upload iPhone/2026-05-31/IMG_0001.HEIC",
		Checksums: []store.UploadChecksum{{
			Algorithm: "sha1",
			Encoding:  "hex",
			Digest:    sha1Hex(body),
		}},
		Now: capturedAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("ReserveUploadCommit() error = %v", err)
	}

	recovered, err := runtime.StartUploadSession(bundle.DeviceID, input)
	if err != nil {
		t.Fatalf("StartUploadSession(recover) error = %v", err)
	}
	if recovered.State != "already_uploaded" || recovered.UploadedAsset == nil {
		t.Fatalf("recovered response = %+v, want already uploaded", recovered)
	}
	if recovered.UploadedAsset.FinalRelativePath != "Upload iPhone/2026-05-31/IMG_0001-2.HEIC" {
		t.Fatalf("FinalRelativePath = %q, want suffixed recovery path", recovered.UploadedAsset.FinalRelativePath)
	}
	if raw, err := os.ReadFile(filepath.Join(collisionDir, "IMG_0001-2.HEIC")); err != nil || !bytes.Equal(raw, body) {
		t.Fatalf("recovered final raw=%q error=%v, want uploaded body", raw, err)
	}
	tempPath := filepath.Join(rootPath, ".timich-upload-tmp", session.Session.UploadID+".part")
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file stat error = %v, want removed temp", err)
	}
}

func TestRevokeDeviceCleansProfileWhenRegistryDeviceAlreadyMissing(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name: "Static demo",
			Kind: config.DatasourceKindStaticDemo,
			URL:  writeStaticDemoManifest(t),
		},
	})
	pairingSession, err := runtime.CreatePairingSession()
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}
	bundle, err := runtime.RedeemPairing(pairingSession.PairingCode, "Test iPhone", "http://127.0.0.1:8082")
	if err != nil {
		t.Fatalf("RedeemPairing() error = %v", err)
	}
	if err := runtime.registry.RevokeDevice(bundle.DeviceID); err != nil {
		t.Fatalf("registry.RevokeDevice() error = %v", err)
	}

	if err := runtime.RevokeDevice(bundle.DeviceID); !errors.Is(err, store.ErrDeviceNotFound) {
		t.Fatalf("RevokeDevice() error = %v, want device not found after orphan cleanup", err)
	}
	profiles, err := store.LoadOrCreateDeviceProfileStore(runtime.config.DataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceProfileStore() error = %v", err)
	}
	if _, err := profiles.LoadProfile(bundle.DeviceID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadProfile() error = %v, want profile cleaned up", err)
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

func sha1Hex(raw []byte) string {
	sum := sha1.Sum(raw)
	return hex.EncodeToString(sum[:])
}

func assertPathPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("Mode(%s) = %03o, want %03o", path, got, want)
	}
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
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return runtime
}
