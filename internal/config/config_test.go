package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaultsWhenConfigFileIsMissing(t *testing.T) {
	t.Setenv("TIMICH_AGENT_CONFIG_PATH", "")
	path := filepath.Join(t.TempDir(), "missing.json")

	resolved, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if resolved.ConfigSource != "defaults" {
		t.Fatalf("ConfigSource = %q, want defaults", resolved.ConfigSource)
	}
	if resolved.AdminListenAddress != "0.0.0.0:8081" {
		t.Fatalf("AdminListenAddress = %q", resolved.AdminListenAddress)
	}
	if resolved.DeviceLimit != DefaultDeviceLimit {
		t.Fatalf("DeviceLimit = %d, want default %d", resolved.DeviceLimit, DefaultDeviceLimit)
	}
	if resolved.ControlPlaneAddress != DefaultRelayConnectionAddress {
		t.Fatalf("relay connection address = %q, want default %q", resolved.ControlPlaneAddress, DefaultRelayConnectionAddress)
	}
	if resolved.AppLinkBaseURL != DefaultAppLinkBaseURL {
		t.Fatalf("app link base URL = %q, want default %q", resolved.AppLinkBaseURL, DefaultAppLinkBaseURL)
	}
	if resolved.Hosted.ServerURL != DefaultRemoteBrowsingServerURL {
		t.Fatalf("remote browsing server URL = %q, want default %q", resolved.Hosted.ServerURL, DefaultRemoteBrowsingServerURL)
	}
	if !filepath.IsAbs(resolved.DataDir) {
		t.Fatalf("DataDir should be absolute, got %q", resolved.DataDir)
	}
}

func TestLoadResolvesRelativeDataDirFromConfigFileDirectory(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "agent.json")
	raw := []byte("{\n  \"dataDir\": \"state\"\n}\n")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := filepath.Join(tempDir, "state")
	if resolved.DataDir != want {
		t.Fatalf("DataDir = %q, want %q", resolved.DataDir, want)
	}
}

func TestLoadAppliesEnvironmentOverrides(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "agent.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("TIMICH_AGENT_NAME", "  kitchen-agent  ")
	t.Setenv("TIMICH_AGENT_ADMIN_LISTEN_ADDR", "127.0.0.1:19081")
	t.Setenv("TIMICH_AGENT_MEDIA_LISTEN_ADDR", "0.0.0.0:19082")
	t.Setenv("TIMICH_AGENT_MEDIA_PUBLISHED_ADDR", "18082")
	t.Setenv("TIMICH_AGENT_DATA_DIR", "env-state")
	t.Setenv("TIMICH_AGENT_TIMEZONE", "Asia/Tokyo")
	t.Setenv("TIMICH_AGENT_DEVICE_LIMIT", "9")
	t.Setenv("TIMICH_AGENT_APP_LINK_BASE_URL", "https://link.dev.timich.runo.jp")
	t.Setenv("TIMICH_AGENT_RELAY_CONNECTION_ADDR", "https://relay-connection.example")
	t.Setenv("TIMICH_AGENT_CONTROL_PLANE_SERVER_NAME", "control.example")
	t.Setenv("TIMICH_AGENT_REMOTE_BROWSING_SERVER_URL", "https://relay.example")
	t.Setenv("TIMICH_AGENT_REMOTE_BROWSING_ENABLED", "yes")

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if resolved.AgentName != "kitchen-agent" {
		t.Fatalf("AgentName = %q, want trimmed env override", resolved.AgentName)
	}
	if resolved.DeviceLimit != 9 {
		t.Fatalf("DeviceLimit = %d, want 9", resolved.DeviceLimit)
	}
	if resolved.DataDir != filepath.Join(tempDir, "env-state") {
		t.Fatalf("DataDir = %q, want env-state resolved from config directory", resolved.DataDir)
	}
	if resolved.Timezone != "Asia/Tokyo" {
		t.Fatalf("Timezone = %q, want env override", resolved.Timezone)
	}
	if resolved.ControlPlaneAddress != "https://relay-connection.example" {
		t.Fatalf("relay connection address = %q, want env override", resolved.ControlPlaneAddress)
	}
	if resolved.AppLinkBaseURL != "https://link.dev.timich.runo.jp" {
		t.Fatalf("app link base URL = %q, want env override", resolved.AppLinkBaseURL)
	}
	if resolved.MediaPublishedAddress != "18082" {
		t.Fatalf("MediaPublishedAddress = %q, want media published address env override", resolved.MediaPublishedAddress)
	}
	if resolved.ControlPlaneServerName != "control.example" {
		t.Fatalf("ControlPlaneServerName = %q, want env override", resolved.ControlPlaneServerName)
	}
	if !resolved.Hosted.Enabled {
		t.Fatal("remote browsing enabled = false, want true")
	}
	if resolved.Hosted.ServerURL != "https://relay.example" {
		t.Fatalf("relay server URL = %q, want remote browsing env override", resolved.Hosted.ServerURL)
	}
}

func TestLoadAcceptsUploadConfiguration(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	rootPath := filepath.Join(t.TempDir(), "uploads")
	raw := []byte(`{
  "timezone": " Asia/Tokyo ",
  "uploadRoots": [
    {"key": " nas-photos ", "path": " ` + filepath.ToSlash(rootPath) + ` "}
  ]
}
`)
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.Timezone != "Asia/Tokyo" {
		t.Fatalf("Timezone = %q, want Asia/Tokyo", resolved.Timezone)
	}
	if len(resolved.UploadRoots) != 1 {
		t.Fatalf("UploadRoots length = %d, want 1", len(resolved.UploadRoots))
	}
	if resolved.UploadRoots[0].Key != "nas-photos" ||
		resolved.UploadRoots[0].Path != rootPath ||
		resolved.UploadRoots[0].TempPath != DefaultUploadRootTempPath {
		t.Fatalf("UploadRoots[0] = %+v, want configured root with default temp path", resolved.UploadRoots[0])
	}
}

func TestWriteFileNormalizesUploadConfiguration(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	rootPath := filepath.Join(t.TempDir(), "uploads")
	cfg := Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "state")
	cfg.Timezone = " Asia/Tokyo "
	cfg.UploadRoots = []UploadRootConfig{
		{Key: " nas-photos ", Path: " " + rootPath + " ", TempPath: " working/../working/.timich-upload-tmp "},
	}

	if err := WriteFile(configPath, cfg); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Timezone != "Asia/Tokyo" {
		t.Fatalf("Timezone = %q, want normalized timezone", loaded.Timezone)
	}
	if len(loaded.UploadRoots) != 1 {
		t.Fatalf("UploadRoots length = %d, want 1", len(loaded.UploadRoots))
	}
	if loaded.UploadRoots[0].Key != "nas-photos" ||
		loaded.UploadRoots[0].Path != rootPath ||
		loaded.UploadRoots[0].TempPath != "working/.timich-upload-tmp" {
		t.Fatalf("UploadRoots[0] = %+v, want normalized root", loaded.UploadRoots[0])
	}
}

func TestLoadRejectsInvalidTimezone(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte("{\"timezone\":\"Mars/Olympus\"}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatal("Load() error = nil, want invalid timezone error")
	}
}

func TestLoadRejectsInvalidUploadRoot(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{"uploadRoots":[{"key":"NAS Photos","path":"relative"}]}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatal("Load() error = nil, want invalid upload root error")
	}
}

func TestLoadRejectsInvalidUploadRootTempPath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	rootPath := filepath.Join(t.TempDir(), "uploads")
	raw := []byte(`{"uploadRoots":[{"key":"nas-photos","path":"` + filepath.ToSlash(rootPath) + `","tempPath":"../outside"}]}` + "\n")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatal("Load() error = nil, want invalid upload root temp path error")
	}
}

func TestLoadAcceptsRemoteBrowsingConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	raw := []byte("{\n  \"relayConnectionAddress\": \"https://relay.example\",\n  \"remoteBrowsing\": {\"enabled\": true, \"serverURL\": \"https://relay-server.example\"}\n}\n")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.ControlPlaneAddress != "https://relay.example" {
		t.Fatalf("relay connection address = %q, want config value", resolved.ControlPlaneAddress)
	}
	if !resolved.Hosted.Enabled {
		t.Fatal("remote browsing enabled = false, want true")
	}
	if resolved.Hosted.ServerURL != "https://relay-server.example" {
		t.Fatalf("relay server URL = %q, want config value", resolved.Hosted.ServerURL)
	}
}

func TestLoadUpgradesLegacyProductionRelayConnectionAddress(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	raw := []byte("{\n  \"relayConnectionAddress\": \"https://timich.runo.jp\",\n  \"remoteBrowsing\": {\"enabled\": true, \"serverURL\": \"https://timich.runo.jp\"}\n}\n")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.ControlPlaneAddress != DefaultRelayConnectionAddress {
		t.Fatalf("relay connection address = %q, want upgraded default %q", resolved.ControlPlaneAddress, DefaultRelayConnectionAddress)
	}
}

func TestLoadPreservesExplicitRelayConnectionEnvironmentOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	raw := []byte("{\n  \"relayConnectionAddress\": \"https://timich.runo.jp\",\n  \"remoteBrowsing\": {\"enabled\": true, \"serverURL\": \"https://timich.runo.jp\"}\n}\n")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("TIMICH_AGENT_RELAY_CONNECTION_ADDR", "https://timich.runo.jp")

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.ControlPlaneAddress != "https://timich.runo.jp" {
		t.Fatalf("relay connection address = %q, want explicit env override preserved", resolved.ControlPlaneAddress)
	}
}

func TestLoadAcceptsLegacyHostedConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	raw := []byte("{\n  \"hosted\": {\"enabled\": true, \"serverURL\": \"https://legacy.example\"}\n}\n")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !resolved.Hosted.Enabled {
		t.Fatal("remote browsing enabled = false, want true")
	}
	if resolved.Hosted.ServerURL != "https://legacy.example" {
		t.Fatalf("relay server URL = %q, want legacy config value", resolved.Hosted.ServerURL)
	}
}

func TestLoadIgnoresInvalidDeviceLimitEnvironmentOverride(t *testing.T) {
	t.Setenv("TIMICH_AGENT_DEVICE_LIMIT", "not-a-number")

	resolved, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.DeviceLimit != Default().DeviceLimit {
		t.Fatalf("DeviceLimit = %d, want default %d", resolved.DeviceLimit, Default().DeviceLimit)
	}
}

func TestWriteDefaultFileCreatesConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")

	writtenPath, err := WriteDefaultFile(configPath, "state")
	if err != nil {
		t.Fatalf("WriteDefaultFile() error = %v", err)
	}
	if writtenPath != configPath {
		t.Fatalf("writtenPath = %q, want %q", writtenPath, configPath)
	}

	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(raw), `"remoteBrowsing"`) {
		t.Fatalf("config missing remoteBrowsing key: %s", string(raw))
	}
	if !strings.Contains(string(raw), `"relayConnectionAddress"`) {
		t.Fatalf("config missing relayConnectionAddress key: %s", string(raw))
	}
	if !strings.Contains(string(raw), DefaultRelayConnectionAddress) {
		t.Fatalf("config missing default relay connection address %q: %s", DefaultRelayConnectionAddress, string(raw))
	}
	if strings.Contains(string(raw), `"hosted"`) {
		t.Fatalf("config should not write legacy hosted key: %s", string(raw))
	}
	if strings.Contains(string(raw), `"controlPlaneAddress"`) {
		t.Fatalf("config should not write legacy controlPlaneAddress key: %s", string(raw))
	}
}

func TestWriteFilePersistsPrimaryDatasource(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	cfg := Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "state")
	cfg.Datasources = []DatasourceConfig{
		{
			Name:        "Immich",
			Kind:        "immich",
			URL:         "http://immich.local:2283",
			AccessToken: "secret-api-key",
		},
	}

	if err := WriteFile(configPath, cfg); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded.Datasources) != 1 {
		t.Fatalf("Datasources length = %d, want 1", len(loaded.Datasources))
	}
	if loaded.Datasources[0].AccessToken != "secret-api-key" {
		t.Fatal("datasource access token was not persisted")
	}
}

func TestWriteFileAcceptsStaticDemoDatasource(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	cfg := Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "state")
	cfg.Datasources = []DatasourceConfig{
		{
			Name: "Review Demo",
			Kind: DatasourceKindStaticDemo,
			URL:  "file:///var/lib/timich-agent/static-demo/manifest.json",
		},
	}

	if err := WriteFile(configPath, cfg); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Datasources[0].Kind != DatasourceKindStaticDemo {
		t.Fatalf("Kind = %q, want %q", loaded.Datasources[0].Kind, DatasourceKindStaticDemo)
	}
}

func TestUpdatePrimaryDatasourceFilePreservesFileBackedSettingsOnly(t *testing.T) {
	t.Setenv("TIMICH_AGENT_NAME", "env-agent")

	configPath := filepath.Join(t.TempDir(), "agent.json")
	cfg := Default()
	cfg.AdminListenAddress = "127.0.0.1:8081"
	cfg.DataDir = "state"
	cfg.Datasources = []DatasourceConfig{
		{
			Name:        "Old",
			Kind:        "immich",
			URL:         "http://old-immich.local:2283",
			AccessToken: "existing-api-key",
		},
		{
			Name:        "Extra",
			Kind:        "immich",
			URL:         "http://extra-immich.local:2283",
			AccessToken: "extra-api-key",
		},
	}
	if err := WriteFile(configPath, cfg); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.AgentName != "env-agent" {
		t.Fatal("test setup did not apply env override")
	}

	updated, err := UpdatePrimaryDatasourceFile(configPath, DatasourceConfig{
		Name: "Home Immich",
		Kind: "immich",
		URL:  "http://immich.local:2283",
	})
	if err != nil {
		t.Fatalf("UpdatePrimaryDatasourceFile() error = %v", err)
	}
	if updated.AgentName != cfg.AgentName {
		t.Fatalf("AgentName = %q, want file-backed value %q", updated.AgentName, cfg.AgentName)
	}
	if updated.AdminListenAddress != "127.0.0.1:8081" {
		t.Fatalf("AdminListenAddress = %q, want file-backed value", updated.AdminListenAddress)
	}
	if updated.ControlPlaneAddress != DefaultRelayConnectionAddress {
		t.Fatalf("relay connection address = %q, want current default %q", updated.ControlPlaneAddress, DefaultRelayConnectionAddress)
	}
	if len(updated.Datasources) != 2 {
		t.Fatalf("Datasources length = %d, want preserved additional datasource", len(updated.Datasources))
	}
	if updated.Datasources[0].AccessToken != "existing-api-key" {
		t.Fatalf("AccessToken = %q, want preserved primary API key", updated.Datasources[0].AccessToken)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(raw), "env-agent") {
		t.Fatalf("config file persisted env override: %s", string(raw))
	}
}

func TestApplyRuntimeOverridesResolvesRelativeDataDirFromWorkingDirectory(t *testing.T) {
	workingDir := t.TempDir()
	t.Chdir(workingDir)

	resolved := ResolvedConfig{
		Config: Config{
			AgentName:           "agent",
			AdminListenAddress:  "127.0.0.1:8081",
			MediaListenAddress:  "0.0.0.0:8082",
			DataDir:             filepath.Join(t.TempDir(), "state"),
			DeviceLimit:         5,
			AppLinkBaseURL:      DefaultAppLinkBaseURL,
			ControlPlaneAddress: "https://timich.runo.jp",
			Hosted: RemoteBrowsingConfig{
				ServerURL: "https://timich.runo.jp",
			},
		},
	}

	overridden, err := ApplyRuntimeOverrides(resolved, "", "", "runtime-state")
	if err != nil {
		t.Fatalf("ApplyRuntimeOverrides() error = %v", err)
	}

	want := filepath.Join(workingDir, "runtime-state")
	if overridden.DataDir != want {
		t.Fatalf("DataDir = %q, want %q", overridden.DataDir, want)
	}
}

func TestLoadAllowsLoopbackAdminListenAddressOptOut(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	raw := []byte("{\n  \"adminListenAddress\": \"127.0.0.1:8081\"\n}\n")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.AdminListenAddress != "127.0.0.1:8081" {
		t.Fatalf("AdminListenAddress = %q, want loopback opt-out", resolved.AdminListenAddress)
	}
}
