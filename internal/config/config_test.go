package config

import (
	"errors"
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
	helperPath := filepath.Join(tempDir, "timich-semantic-helper")
	t.Setenv("TIMICH_AGENT_SEMANTIC_RUNTIME_HELPER", helperPath)
	onnxServerPath := filepath.Join(tempDir, "semantic-runtime", "siglip2-onnx", "server.py")
	t.Setenv("TIMICH_AGENT_SEMANTIC_ONNX_SERVER_PATH", onnxServerPath)
	t.Setenv("TIMICH_AGENT_SEMANTIC_ONNX_PYTHON", "python3")
	t.Setenv("TIMICH_AGENT_SEMANTIC_ONNX_HOST", "127.0.0.1")
	t.Setenv("TIMICH_AGENT_SEMANTIC_ONNX_PORT", "19188")
	t.Setenv("TIMICH_AGENT_SEMANTIC_ONNX_PROVIDER", "cpu")
	t.Setenv("TIMICH_AGENT_SEMANTIC_ONNX_TEXT_PROVIDER", "openvino:CPU")
	t.Setenv("TIMICH_AGENT_SEMANTIC_ONNX_IMAGE_PROVIDER", "openvino:GPU")
	t.Setenv("TIMICH_AGENT_SEMANTIC_ONNX_TEXT_TEMPLATE", "query: {query}")
	mediaHelperPath := filepath.Join(tempDir, "timich-media-helper")
	t.Setenv("TIMICH_AGENT_MEDIA_HELPER_PATH", mediaHelperPath)
	vipsPath := filepath.Join(tempDir, "vips")
	t.Setenv("TIMICH_AGENT_VIPS_PATH", vipsPath)
	ffmpegPath := filepath.Join(tempDir, "ffmpeg")
	t.Setenv("TIMICH_AGENT_FFMPEG_PATH", ffmpegPath)

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
	if resolved.SemanticRuntime.HelperPath != helperPath {
		t.Fatalf("SemanticRuntime.HelperPath = %q, want env override", resolved.SemanticRuntime.HelperPath)
	}
	if resolved.SemanticRuntime.ONNXRuntime.ServerPath != onnxServerPath {
		t.Fatalf("SemanticRuntime.ONNXRuntime.ServerPath = %q, want env override", resolved.SemanticRuntime.ONNXRuntime.ServerPath)
	}
	if resolved.SemanticRuntime.ONNXRuntime.PythonPath != "python3" ||
		resolved.SemanticRuntime.ONNXRuntime.Host != "127.0.0.1" ||
		resolved.SemanticRuntime.ONNXRuntime.Port != 19188 ||
		resolved.SemanticRuntime.ONNXRuntime.Provider != "cpu" ||
		resolved.SemanticRuntime.ONNXRuntime.TextProvider != "openvino:CPU" ||
		resolved.SemanticRuntime.ONNXRuntime.ImageProvider != "openvino:GPU" ||
		resolved.SemanticRuntime.ONNXRuntime.TextTemplate != "query: {query}" {
		t.Fatalf("SemanticRuntime.ONNXRuntime = %+v, want env overrides", resolved.SemanticRuntime.ONNXRuntime)
	}
	if resolved.MediaRuntime.VipsPath != vipsPath {
		t.Fatalf("MediaRuntime.VipsPath = %q, want env override", resolved.MediaRuntime.VipsPath)
	}
	if resolved.MediaRuntime.HelperPath != mediaHelperPath {
		t.Fatalf("MediaRuntime.HelperPath = %q, want env override", resolved.MediaRuntime.HelperPath)
	}
	if resolved.MediaRuntime.FFmpegPath != ffmpegPath {
		t.Fatalf("MediaRuntime.FFmpegPath = %q, want env override", resolved.MediaRuntime.FFmpegPath)
	}
}

func TestLoadAutoDetectsBundledSemanticRuntimeHelper(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := filepath.Join(tempDir, "timich-agent")
	helperPath := filepath.Join(tempDir, "timich-semantic-helper")
	if err := os.WriteFile(executablePath, []byte("agent"), 0o700); err != nil {
		t.Fatalf("WriteFile(agent) error = %v", err)
	}
	if err := os.WriteFile(helperPath, []byte("helper"), 0o700); err != nil {
		t.Fatalf("WriteFile(helper) error = %v", err)
	}
	previousExecutablePath := currentExecutablePath
	currentExecutablePath = func() (string, error) {
		return executablePath, nil
	}
	t.Cleanup(func() {
		currentExecutablePath = previousExecutablePath
	})

	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.SemanticRuntime.HelperPath != helperPath {
		t.Fatalf("SemanticRuntime.HelperPath = %q, want bundled helper %q", resolved.SemanticRuntime.HelperPath, helperPath)
	}
}

func TestLoadKeepsExplicitSemanticRuntimeHelperOverBundledHelper(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := filepath.Join(tempDir, "timich-agent")
	bundledHelperPath := filepath.Join(tempDir, "timich-semantic-helper")
	explicitHelperPath := filepath.Join(tempDir, "explicit-helper")
	for _, path := range []string{executablePath, bundledHelperPath, explicitHelperPath} {
		if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	previousExecutablePath := currentExecutablePath
	currentExecutablePath = func() (string, error) {
		return executablePath, nil
	}
	t.Cleanup(func() {
		currentExecutablePath = previousExecutablePath
	})

	configPath := filepath.Join(t.TempDir(), "agent.json")
	raw := []byte(`{"semanticRuntime":{"helperPath":"` + filepath.ToSlash(explicitHelperPath) + `"}}` + "\n")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.SemanticRuntime.HelperPath != explicitHelperPath {
		t.Fatalf("SemanticRuntime.HelperPath = %q, want explicit helper %q", resolved.SemanticRuntime.HelperPath, explicitHelperPath)
	}
}

func TestLoadAutoDetectsBundledSemanticONNXRuntime(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := filepath.Join(tempDir, "timich-agent")
	serverPath := filepath.Join(tempDir, "semantic-runtime", "siglip2-onnx", "server.py")
	pythonPath := filepath.Join(tempDir, "semantic-runtime", "siglip2-onnx", ".venv", "bin", "python")
	if err := os.WriteFile(executablePath, []byte("agent"), 0o700); err != nil {
		t.Fatalf("WriteFile(agent) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(serverPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(server dir) error = %v", err)
	}
	if err := os.WriteFile(serverPath, []byte("server"), 0o600); err != nil {
		t.Fatalf("WriteFile(server) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(pythonPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(python dir) error = %v", err)
	}
	if err := os.WriteFile(pythonPath, []byte("python"), 0o700); err != nil {
		t.Fatalf("WriteFile(python) error = %v", err)
	}
	previousExecutablePath := currentExecutablePath
	currentExecutablePath = func() (string, error) {
		return executablePath, nil
	}
	t.Cleanup(func() {
		currentExecutablePath = previousExecutablePath
	})

	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.SemanticRuntime.ONNXRuntime.ServerPath != serverPath {
		t.Fatalf("SemanticRuntime.ONNXRuntime.ServerPath = %q, want bundled server %q", resolved.SemanticRuntime.ONNXRuntime.ServerPath, serverPath)
	}
	if resolved.SemanticRuntime.ONNXRuntime.PythonPath != pythonPath {
		t.Fatalf("SemanticRuntime.ONNXRuntime.PythonPath = %q, want bundled python %q", resolved.SemanticRuntime.ONNXRuntime.PythonPath, pythonPath)
	}
}

func TestLoadAutoDetectsBundledMediaRuntimeVips(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := filepath.Join(tempDir, "timich-agent")
	vipsPath := filepath.Join(tempDir, "media-runtime", "libvips", "bin", mediaRuntimeVipsBinaryName())
	if err := os.WriteFile(executablePath, []byte("agent"), 0o700); err != nil {
		t.Fatalf("WriteFile(agent) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(vipsPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(vips dir) error = %v", err)
	}
	if err := os.WriteFile(vipsPath, []byte("vips"), 0o700); err != nil {
		t.Fatalf("WriteFile(vips) error = %v", err)
	}
	previousExecutablePath := currentExecutablePath
	currentExecutablePath = func() (string, error) {
		return executablePath, nil
	}
	t.Cleanup(func() {
		currentExecutablePath = previousExecutablePath
	})

	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.MediaRuntime.VipsPath != vipsPath {
		t.Fatalf("MediaRuntime.VipsPath = %q, want bundled vips %q", resolved.MediaRuntime.VipsPath, vipsPath)
	}
}

func TestLoadAutoDetectsBundledMediaRuntimeHelper(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := filepath.Join(tempDir, "timich-agent")
	helperPath := filepath.Join(tempDir, mediaRuntimeHelperBinaryName())
	if err := os.WriteFile(executablePath, []byte("agent"), 0o700); err != nil {
		t.Fatalf("WriteFile(agent) error = %v", err)
	}
	if err := os.WriteFile(helperPath, []byte("helper"), 0o700); err != nil {
		t.Fatalf("WriteFile(helper) error = %v", err)
	}
	previousExecutablePath := currentExecutablePath
	currentExecutablePath = func() (string, error) {
		return executablePath, nil
	}
	t.Cleanup(func() {
		currentExecutablePath = previousExecutablePath
	})

	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.MediaRuntime.HelperPath != helperPath {
		t.Fatalf("MediaRuntime.HelperPath = %q, want bundled helper %q", resolved.MediaRuntime.HelperPath, helperPath)
	}
}

func TestLoadAutoDetectsBundledMediaRuntimeFFmpeg(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := filepath.Join(tempDir, "timich-agent")
	ffmpegPath := filepath.Join(tempDir, "media-runtime", "ffmpeg", "bin", mediaRuntimeFFmpegBinaryName())
	if err := os.WriteFile(executablePath, []byte("agent"), 0o700); err != nil {
		t.Fatalf("WriteFile(agent) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(ffmpegPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(ffmpeg dir) error = %v", err)
	}
	if err := os.WriteFile(ffmpegPath, []byte("ffmpeg"), 0o700); err != nil {
		t.Fatalf("WriteFile(ffmpeg) error = %v", err)
	}
	previousExecutablePath := currentExecutablePath
	currentExecutablePath = func() (string, error) {
		return executablePath, nil
	}
	t.Cleanup(func() {
		currentExecutablePath = previousExecutablePath
	})

	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.MediaRuntime.FFmpegPath != ffmpegPath {
		t.Fatalf("MediaRuntime.FFmpegPath = %q, want bundled ffmpeg %q", resolved.MediaRuntime.FFmpegPath, ffmpegPath)
	}
}

func TestLoadKeepsExplicitMediaRuntimeHelperOverBundledHelper(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := filepath.Join(tempDir, "timich-agent")
	bundledHelperPath := filepath.Join(tempDir, mediaRuntimeHelperBinaryName())
	explicitHelperPath := filepath.Join(tempDir, "custom-media-helper")
	if err := os.WriteFile(executablePath, []byte("agent"), 0o700); err != nil {
		t.Fatalf("WriteFile(agent) error = %v", err)
	}
	for _, path := range []string{bundledHelperPath, explicitHelperPath} {
		if err := os.WriteFile(path, []byte("helper"), 0o700); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	previousExecutablePath := currentExecutablePath
	currentExecutablePath = func() (string, error) {
		return executablePath, nil
	}
	t.Cleanup(func() {
		currentExecutablePath = previousExecutablePath
	})

	configPath := filepath.Join(t.TempDir(), "agent.json")
	raw := []byte(`{"mediaRuntime":{"helperPath":"` + filepath.ToSlash(explicitHelperPath) + `"}}` + "\n")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.MediaRuntime.HelperPath != explicitHelperPath {
		t.Fatalf("MediaRuntime.HelperPath = %q, want explicit helper %q", resolved.MediaRuntime.HelperPath, explicitHelperPath)
	}
}

func TestLoadKeepsExplicitMediaRuntimeVipsOverBundledVips(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := filepath.Join(tempDir, "timich-agent")
	bundledVipsPath := filepath.Join(tempDir, "media-runtime", "libvips", "bin", mediaRuntimeVipsBinaryName())
	explicitVipsPath := filepath.Join(tempDir, "custom-vips")
	if err := os.WriteFile(executablePath, []byte("agent"), 0o700); err != nil {
		t.Fatalf("WriteFile(agent) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(bundledVipsPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(bundled vips dir) error = %v", err)
	}
	for _, path := range []string{bundledVipsPath, explicitVipsPath} {
		if err := os.WriteFile(path, []byte("vips"), 0o700); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	previousExecutablePath := currentExecutablePath
	currentExecutablePath = func() (string, error) {
		return executablePath, nil
	}
	t.Cleanup(func() {
		currentExecutablePath = previousExecutablePath
	})

	configPath := filepath.Join(t.TempDir(), "agent.json")
	raw := []byte(`{"mediaRuntime":{"vipsPath":"` + filepath.ToSlash(explicitVipsPath) + `"}}` + "\n")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.MediaRuntime.VipsPath != explicitVipsPath {
		t.Fatalf("MediaRuntime.VipsPath = %q, want explicit vips %q", resolved.MediaRuntime.VipsPath, explicitVipsPath)
	}
}

func TestLoadKeepsExplicitMediaRuntimeFFmpegOverBundledFFmpeg(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := filepath.Join(tempDir, "timich-agent")
	bundledFFmpegPath := filepath.Join(tempDir, "media-runtime", "ffmpeg", "bin", mediaRuntimeFFmpegBinaryName())
	explicitFFmpegPath := filepath.Join(tempDir, "custom-ffmpeg")
	if err := os.WriteFile(executablePath, []byte("agent"), 0o700); err != nil {
		t.Fatalf("WriteFile(agent) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(bundledFFmpegPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(bundled ffmpeg dir) error = %v", err)
	}
	for _, path := range []string{bundledFFmpegPath, explicitFFmpegPath} {
		if err := os.WriteFile(path, []byte("ffmpeg"), 0o700); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	previousExecutablePath := currentExecutablePath
	currentExecutablePath = func() (string, error) {
		return executablePath, nil
	}
	t.Cleanup(func() {
		currentExecutablePath = previousExecutablePath
	})

	configPath := filepath.Join(t.TempDir(), "agent.json")
	raw := []byte(`{"mediaRuntime":{"ffmpegPath":"` + filepath.ToSlash(explicitFFmpegPath) + `"}}` + "\n")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.MediaRuntime.FFmpegPath != explicitFFmpegPath {
		t.Fatalf("MediaRuntime.FFmpegPath = %q, want explicit ffmpeg %q", resolved.MediaRuntime.FFmpegPath, explicitFFmpegPath)
	}
}

func TestLoadRejectsRelativeSemanticRuntimeHelperPath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{"semanticRuntime":{"helperPath":"bin/timich-semantic-helper"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatal("Load() error = nil, want invalid semantic runtime helper path error")
	}
}

func TestLoadRejectsRelativeMediaRuntimeVipsPath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{"mediaRuntime":{"vipsPath":"bin/vips"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatal("Load() error = nil, want invalid media runtime vips path error")
	}
}

func TestLoadRejectsRelativeMediaRuntimeHelperPath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{"mediaRuntime":{"helperPath":"bin/timich-media-helper"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatal("Load() error = nil, want invalid media runtime helper path error")
	}
}

func TestLoadRejectsRelativeMediaRuntimeFFmpegPath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{"mediaRuntime":{"ffmpegPath":"bin/ffmpeg"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatal("Load() error = nil, want invalid media runtime ffmpeg path error")
	}
}

func TestLoadAcceptsSemanticIndexingConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{
  "semanticRuntime": {
    "indexing": {
      "enabled": true,
      "interval": "45s",
      "batchSize": 25,
	  "targetCompletedVectors": 10000
    }
  }
}
`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	backfill := resolved.SemanticRuntime.Indexing
	if !backfill.Enabled || backfill.Interval != "45s" || backfill.BatchSize != 25 || backfill.TargetCompletedVectors != 10000 {
		t.Fatalf("Indexing = %+v, want enabled interval/batch/target", backfill)
	}
}

func TestLoadRejectsInvalidSemanticIndexingConfig(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "interval",
			raw:  `{"semanticRuntime":{"indexing":{"interval":"soon"}}}`,
		},
		{
			name: "batchSize",
			raw:  `{"semanticRuntime":{"indexing":{"batchSize":-1}}}`,
		},
		{
			name: "targetCompletedVectors",
			raw:  `{"semanticRuntime":{"indexing":{"targetCompletedVectors":-1}}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "agent.json")
			if err := os.WriteFile(configPath, []byte(test.raw+"\n"), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			if _, err := Load(configPath); err == nil {
				t.Fatal("Load() error = nil, want invalid semantic indexing config error")
			}
		})
	}
}

func TestLoadAcceptsWorkerRuntimeConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{"workerRuntime":{"heavyTaskWorkers":3}}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.WorkerRuntime.HeavyTaskWorkers == nil || *resolved.WorkerRuntime.HeavyTaskWorkers != 3 {
		t.Fatalf("WorkerRuntime.HeavyTaskWorkers = %v, want 3", resolved.WorkerRuntime.HeavyTaskWorkers)
	}
}

func TestLoadTreatsEmptyWorkerRuntimeEnvAsAuto(t *testing.T) {
	t.Setenv("TIMICH_AGENT_HEAVY_TASK_WORKERS", "")

	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if resolved.WorkerRuntime.HeavyTaskWorkers != nil {
		t.Fatalf("WorkerRuntime.HeavyTaskWorkers = %v, want nil auto", resolved.WorkerRuntime.HeavyTaskWorkers)
	}
}

func TestLoadRejectsInvalidWorkerRuntimeConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(configPath, []byte(`{"workerRuntime":{"heavyTaskWorkers":-1}}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := Load(configPath); err == nil {
		t.Fatal("Load() error = nil, want invalid worker runtime config error")
	}
}

func TestDatasourceKindPolicies(t *testing.T) {
	t.Parallel()

	if !IsImmichDatasourceKind(DatasourceKindImmich) || !IsImmichDatasourceKind(DatasourceKindImmichIndexed) {
		t.Fatal("both Immich datasource kinds must share the Immich connector family")
	}
	if IsIndexedDatasourceKind(DatasourceKindImmich) {
		t.Fatal("passthrough Immich must not be indexed")
	}
	if !IsIndexedDatasourceKind(DatasourceKindImmichIndexed) || !IsIndexedDatasourceKind(DatasourceKindLocalFiles) {
		t.Fatal("indexed Immich and local filesystem must contribute to the catalog")
	}
}

func TestValidateDoesNotMutateSharedConfiguration(t *testing.T) {
	t.Parallel()

	fallbackEnabled := true
	heavyTaskWorkers := 2
	cfg := Default()
	cfg.WorkerRuntime.HeavyTaskWorkers = &heavyTaskWorkers
	cfg.LocalMediaRoots = []LocalMediaRootConfig{{Key: " nas-photos ", Path: " /photos "}}
	cfg.UploadRoots = []UploadRootConfig{{Key: " uploads ", Path: " /uploads ", TempPath: " staging "}}
	cfg.Datasources = []DatasourceConfig{
		{
			SourceKey:   "1111111111111111",
			Name:        "Home Immich",
			Kind:        DatasourceKindImmichIndexed,
			URL:         "http://immich.local:2283",
			AccessToken: "token",
			Indexing: &DatasourceIndexingConfig{
				Phase0SyncInterval:   " 30m ",
				DailyFullSweepWindow: " 02:00 ",
			},
		},
		{
			SourceKey: "2222222222222222",
			Name:      "NAS Photos",
			Kind:      DatasourceKindLocalFiles,
			RootKey:   " nas-photos ",
			Scan: &LocalDatasourceScanConfig{
				ImmichFallbackEnabled: &fallbackEnabled,
				QuickScanInterval:     " 15m ",
			},
		},
	}

	if err := Validate(cfg); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if cfg.LocalMediaRoots[0].Key != " nas-photos " || cfg.LocalMediaRoots[0].Path != " /photos " {
		t.Fatalf("Validate() mutated LocalMediaRoots: %+v", cfg.LocalMediaRoots)
	}
	if cfg.UploadRoots[0].Key != " uploads " || cfg.UploadRoots[0].TempPath != " staging " {
		t.Fatalf("Validate() mutated UploadRoots: %+v", cfg.UploadRoots)
	}
	if cfg.Datasources[0].Indexing.Phase0SyncInterval != " 30m " || cfg.Datasources[0].Indexing.DailyFullSweepWindow != " 02:00 " {
		t.Fatalf("Validate() mutated datasource Indexing: %+v", cfg.Datasources[0].Indexing)
	}
	if cfg.Datasources[1].RootKey != " nas-photos " || cfg.Datasources[1].Scan.QuickScanInterval != " 15m " {
		t.Fatalf("Validate() mutated datasource Scan: %+v", cfg.Datasources[1])
	}
}

func TestWriteFileRejectsPassthroughImmichWithAdditionalDatasource(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.LocalMediaRoots = []LocalMediaRootConfig{{Key: "nas-photos", Path: t.TempDir()}}
	cfg.Datasources = []DatasourceConfig{
		{
			SourceKey:   "1111111111111111",
			Name:        "Home Immich",
			Kind:        DatasourceKindImmich,
			URL:         "http://immich.local:2283",
			AccessToken: "token",
		},
		{
			SourceKey: "2222222222222222",
			Name:      "NAS Photos",
			Kind:      DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
		},
	}

	err := WriteFile(filepath.Join(t.TempDir(), "agent.json"), cfg)
	if !errors.Is(err, ErrImmichPassthroughRequiresSingleDatasource) {
		t.Fatalf("WriteFile() error = %v, want passthrough topology error", err)
	}
}

func TestWriteFileAcceptsIndexedImmichWithAdditionalDatasource(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.LocalMediaRoots = []LocalMediaRootConfig{{Key: "nas-photos", Path: t.TempDir()}}
	cfg.Datasources = []DatasourceConfig{
		{
			SourceKey:   "1111111111111111",
			Name:        "Home Immich",
			Kind:        DatasourceKindImmichIndexed,
			URL:         "http://immich.local:2283",
			AccessToken: "token",
		},
		{
			SourceKey: "2222222222222222",
			Name:      "NAS Photos",
			Kind:      DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
		},
	}

	if err := WriteFile(filepath.Join(t.TempDir(), "agent.json"), cfg); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func TestWriteFileAcceptsUploadRootsWithPassthroughImmich(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.UploadRoots = []UploadRootConfig{{Key: "camera-uploads", Path: t.TempDir()}}
	cfg.Datasources = []DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        DatasourceKindImmich,
		URL:         "http://immich.local:2283",
		AccessToken: "token",
	}}

	if err := WriteFile(filepath.Join(t.TempDir(), "agent.json"), cfg); err != nil {
		t.Fatalf("WriteFile() error = %v, want uploads independent of passthrough mode", err)
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

func TestLoadAcceptsLocalMediaRootAndDatasource(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")
	rootPath := filepath.Join(t.TempDir(), "photos")
	raw := []byte(`{
  "localMediaRoots": [
    {"key": " nas-photos ", "path": " ` + filepath.ToSlash(rootPath) + ` "}
  ],
  "datasources": [
    {
      "name": "NAS Photos",
      "kind": "local_filesystem",
      "rootKey": " nas-photos ",
      "scan": {
        "firstViewThumbnailCount": 60,
        "quickScanInterval": " 5m ",
        "reconciliationTime": " 04:00 ",
        "contentVerificationTime": " 04:30 ",
        "contentVerificationDuration": " 30m ",
        "settlingDuration": " 2m ",
        "includeHiddenDirectories": true
      }
    }
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
	if len(resolved.LocalMediaRoots) != 1 {
		t.Fatalf("LocalMediaRoots length = %d, want 1", len(resolved.LocalMediaRoots))
	}
	if resolved.LocalMediaRoots[0].Key != "nas-photos" || resolved.LocalMediaRoots[0].Path != rootPath {
		t.Fatalf("LocalMediaRoots[0] = %+v, want normalized local root", resolved.LocalMediaRoots[0])
	}
	if len(resolved.Datasources) != 1 {
		t.Fatalf("Datasources length = %d, want 1", len(resolved.Datasources))
	}
	datasource := resolved.Datasources[0]
	if datasource.Kind != DatasourceKindLocalFiles || datasource.RootKey != "nas-photos" {
		t.Fatalf("Datasource = %+v, want local filesystem datasource", datasource)
	}
	if datasource.Scan == nil ||
		datasource.Scan.QuickScanInterval != "5m" ||
		datasource.Scan.ReconciliationTime != "04:00" ||
		datasource.Scan.ContentVerificationTime != "04:30" ||
		datasource.Scan.ContentVerificationDuration != "30m" ||
		datasource.Scan.SettlingDuration != "2m" {
		t.Fatalf("Datasource.Scan = %+v, want normalized quick/reconciliation/verification/settling settings", datasource.Scan)
	}
	if err := ValidateDatasourceSourceKey(datasource.SourceKey); err != nil {
		t.Fatalf("generated SourceKey = %q: %v", datasource.SourceKey, err)
	}
	if datasource.Scan == nil ||
		datasource.Scan.FirstViewThumbnailCount != 60 ||
		datasource.Scan.QuickScanInterval != "5m" ||
		datasource.Scan.ReconciliationTime != "04:00" ||
		datasource.Scan.ContentVerificationTime != "04:30" ||
		datasource.Scan.ContentVerificationDuration != "30m" ||
		datasource.Scan.SettlingDuration != "2m" ||
		!datasource.Scan.IncludeHiddenDirs {
		t.Fatalf("Scan = %+v, want normalized scan config", datasource.Scan)
	}
}

func TestLoadAcceptsZeroContentVerificationDurationAsDisabled(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "agent.json")
	rootPath := t.TempDir()
	raw := []byte(`{
  "localMediaRoots": [
    {"key": "nas-photos", "path": "` + filepath.ToSlash(rootPath) + `"}
  ],
  "datasources": [
    {
      "name": "NAS Photos",
      "kind": "local_filesystem",
      "rootKey": "nas-photos",
      "scan": {"contentVerificationDuration": "0"}
    }
  ]
}`)
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	resolved, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v, want zero duration accepted", err)
	}
	if len(resolved.Datasources) != 1 ||
		resolved.Datasources[0].Scan == nil ||
		resolved.Datasources[0].Scan.ContentVerificationDuration != "0" {
		t.Fatalf("loaded datasource = %+v, want explicit disabled duration retained", resolved.Datasources)
	}
}

func TestLocalDatasourceImmichFallbackDefaultsEnabledAndPersistsDisabled(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "agent.json")
	rootPath := t.TempDir()
	cfg := Default()
	cfg.LocalMediaRoots = []LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
	cfg.Datasources = []DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}
	if err := WriteFile(configPath, cfg); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !LocalDatasourceImmichFallbackEnabled(loaded.Datasources[0]) {
		t.Fatal("LocalDatasourceImmichFallbackEnabled() = false for omitted setting, want true")
	}

	if _, err := UpdateLocalDatasourceImmichFallbackFile(configPath, "1111111111111111", false); err != nil {
		t.Fatalf("UpdateLocalDatasourceImmichFallbackFile() error = %v", err)
	}
	loaded, err = Load(configPath)
	if err != nil {
		t.Fatalf("Load() after update error = %v", err)
	}
	if LocalDatasourceImmichFallbackEnabled(loaded.Datasources[0]) {
		t.Fatal("LocalDatasourceImmichFallbackEnabled() = true after disabling, want false")
	}
	if loaded.Datasources[0].Scan == nil || loaded.Datasources[0].Scan.ImmichFallbackEnabled == nil || *loaded.Datasources[0].Scan.ImmichFallbackEnabled {
		t.Fatalf("persisted scan = %#v, want explicit disabled fallback", loaded.Datasources[0].Scan)
	}
}

func TestAddDatasourceFileAppendsDatasource(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "agent.json")
	rootPath := t.TempDir()
	cfg := Default()
	cfg.LocalMediaRoots = []LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
	cfg.Datasources = []DatasourceConfig{{
		Name:        "Home Immich",
		Kind:        DatasourceKindImmichIndexed,
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
		SourceKey:   "1111111111111111",
	}}
	if err := WriteFile(configPath, cfg); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	added := DatasourceConfig{
		Name:    "NAS Photos",
		Kind:    DatasourceKindLocalFiles,
		RootKey: "nas-photos",
	}
	persisted, err := AddDatasourceFile(configPath, added)
	if err != nil {
		t.Fatalf("AddDatasourceFile() error = %v", err)
	}
	if len(persisted.Datasources) != 2 {
		t.Fatalf("persisted datasource length = %d, want 2", len(persisted.Datasources))
	}
	if persisted.Datasources[0].SourceKey != "1111111111111111" || persisted.Datasources[0].AccessToken != "immich-api-key" {
		t.Fatalf("first datasource changed: %+v", persisted.Datasources[0])
	}
	if persisted.Datasources[1].Kind != DatasourceKindLocalFiles ||
		persisted.Datasources[1].RootKey != "nas-photos" ||
		strings.TrimSpace(persisted.Datasources[1].SourceKey) == "" {
		t.Fatalf("added datasource = %+v", persisted.Datasources[1])
	}
}

func TestWriteFileRejectsInvalidLocalDatasourceConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		configure func(*Config, string)
	}{
		{
			name: "relative root path",
			configure: func(cfg *Config, rootPath string) {
				cfg.LocalMediaRoots = []LocalMediaRootConfig{{Key: "nas-photos", Path: "relative"}}
			},
		},
		{
			name: "unknown datasource root",
			configure: func(cfg *Config, rootPath string) {
				cfg.LocalMediaRoots = []LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
				cfg.Datasources = []DatasourceConfig{{
					Name:    "NAS Photos",
					Kind:    DatasourceKindLocalFiles,
					RootKey: "other-root",
				}}
			},
		},
		{
			name: "negative first view thumbnails",
			configure: func(cfg *Config, rootPath string) {
				cfg.LocalMediaRoots = []LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
				cfg.Datasources = []DatasourceConfig{{
					Name:    "NAS Photos",
					Kind:    DatasourceKindLocalFiles,
					RootKey: "nas-photos",
					Scan: &LocalDatasourceScanConfig{
						FirstViewThumbnailCount: -1,
					},
				}}
			},
		},
		{
			name: "invalid scan interval",
			configure: func(cfg *Config, rootPath string) {
				cfg.LocalMediaRoots = []LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
				cfg.Datasources = []DatasourceConfig{{
					Name:    "NAS Photos",
					Kind:    DatasourceKindLocalFiles,
					RootKey: "nas-photos",
					Scan: &LocalDatasourceScanConfig{
						QuickScanInterval: "soon",
					},
				}}
			},
		},
		{
			name: "invalid reconciliation time",
			configure: func(cfg *Config, rootPath string) {
				cfg.LocalMediaRoots = []LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
				cfg.Datasources = []DatasourceConfig{{
					Name:    "NAS Photos",
					Kind:    DatasourceKindLocalFiles,
					RootKey: "nas-photos",
					Scan: &LocalDatasourceScanConfig{
						ReconciliationTime: "25:00",
					},
				}}
			},
		},
		{
			name: "negative content verification duration",
			configure: func(cfg *Config, rootPath string) {
				cfg.LocalMediaRoots = []LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
				cfg.Datasources = []DatasourceConfig{{
					Name:    "NAS Photos",
					Kind:    DatasourceKindLocalFiles,
					RootKey: "nas-photos",
					Scan: &LocalDatasourceScanConfig{
						ContentVerificationDuration: "-1s",
					},
				}}
			},
		},
		{
			name: "invalid content verification time",
			configure: func(cfg *Config, rootPath string) {
				cfg.LocalMediaRoots = []LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
				cfg.Datasources = []DatasourceConfig{{
					Name:    "NAS Photos",
					Kind:    DatasourceKindLocalFiles,
					RootKey: "nas-photos",
					Scan: &LocalDatasourceScanConfig{
						ContentVerificationTime: "25:00",
					},
				}}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := Default()
			cfg.DataDir = filepath.Join(t.TempDir(), "state")
			tc.configure(&cfg, filepath.Join(t.TempDir(), "photos"))
			if err := WriteFile(filepath.Join(t.TempDir(), "agent.json"), cfg); err == nil {
				t.Fatal("WriteFile() error = nil, want invalid local datasource config")
			}
		})
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

func TestValidateUploadRootTempPathRejectsBackslashTraversal(t *testing.T) {
	t.Parallel()

	for _, value := range []string{`..\outside`, `working\..\outside`, `working\temp`} {
		if _, err := ValidateUploadRootTempPath(value); err == nil {
			t.Fatalf("ValidateUploadRootTempPath(%q) error = nil, want rejection", value)
		}
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

func TestWriteFileRejectsInvalidDatasourceIndexingSchedule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		indexing DatasourceIndexingConfig
	}{
		{
			name: "invalid interval",
			indexing: DatasourceIndexingConfig{
				Phase0SyncInterval: "soon",
			},
		},
		{
			name: "invalid daily window",
			indexing: DatasourceIndexingConfig{
				DailyFullSweepWindow: "25:00",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := Default()
			cfg.DataDir = filepath.Join(t.TempDir(), "state")
			cfg.Datasources = []DatasourceConfig{{
				Name:     "Immich",
				Kind:     DatasourceKindImmichIndexed,
				URL:      "http://immich.local:2283",
				Indexing: &tc.indexing,
			}}
			if err := WriteFile(filepath.Join(t.TempDir(), "agent.json"), cfg); err == nil {
				t.Fatal("WriteFile() error = nil, want invalid datasource indexing schedule")
			}
		})
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
			Kind:        "immich_indexed",
			URL:         "http://old-immich.local:2283",
			AccessToken: "existing-api-key",
			Indexing: &DatasourceIndexingConfig{
				LatestAssetLimit: 600,
			},
		},
		{
			Name:        "Extra",
			Kind:        "immich_indexed",
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
		Kind: "immich_indexed",
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
	if updated.Datasources[0].Indexing == nil || updated.Datasources[0].Indexing.LatestAssetLimit != 600 {
		t.Fatalf("Indexing = %#v, want preserved datasource indexing settings", updated.Datasources[0].Indexing)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(raw), "env-agent") {
		t.Fatalf("config file persisted env override: %s", string(raw))
	}
}

func TestUpdatePrimaryDatasourceFileClearsIndexingWhenSwitchingToPassthrough(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "agent.json")
	cfg := Default()
	cfg.Datasources = []DatasourceConfig{{
		Name:        "Home Immich",
		Kind:        DatasourceKindImmichIndexed,
		URL:         "http://immich.local:2283",
		AccessToken: "existing-api-key",
		Indexing:    &DatasourceIndexingConfig{LatestAssetLimit: 600},
	}}
	if err := WriteFile(configPath, cfg); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	updated, err := UpdatePrimaryDatasourceFile(configPath, DatasourceConfig{
		Name: "Home Immich",
		Kind: DatasourceKindImmich,
		URL:  "http://immich.local:2283",
	})
	if err != nil {
		t.Fatalf("UpdatePrimaryDatasourceFile() error = %v", err)
	}
	if updated.Datasources[0].Kind != DatasourceKindImmich || updated.Datasources[0].Indexing != nil {
		t.Fatalf("updated datasource = %+v, want passthrough without indexing tuning", updated.Datasources[0])
	}
	if updated.Datasources[0].AccessToken != "existing-api-key" {
		t.Fatalf("AccessToken = %q, want preserved API key", updated.Datasources[0].AccessToken)
	}
}

func TestUpdateSemanticIndexingFilePreservesFileBackedSettingsOnly(t *testing.T) {
	t.Setenv("TIMICH_AGENT_NAME", "env-agent")

	configPath := filepath.Join(t.TempDir(), "agent.json")
	cfg := Default()
	cfg.AgentName = "file-agent"
	cfg.AdminListenAddress = "127.0.0.1:8081"
	cfg.DataDir = "state"
	cfg.Datasources = []DatasourceConfig{{
		Name:        "Home Immich",
		Kind:        "immich",
		URL:         "http://immich.local:2283",
		AccessToken: "existing-api-key",
	}}
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

	updated, err := UpdateSemanticIndexingFile(configPath, SemanticIndexingConfig{
		Enabled:                true,
		Interval:               "30s",
		BatchSize:              100,
		TargetCompletedVectors: 10000,
	})
	if err != nil {
		t.Fatalf("UpdateSemanticIndexingFile() error = %v", err)
	}
	if updated.AgentName != "file-agent" {
		t.Fatalf("AgentName = %q, want file-backed value", updated.AgentName)
	}
	if len(updated.Datasources) != 1 || updated.Datasources[0].AccessToken != "existing-api-key" {
		t.Fatalf("Datasources = %#v, want preserved datasource", updated.Datasources)
	}
	backfill := updated.SemanticRuntime.Indexing
	if !backfill.Enabled || backfill.Interval != "30s" || backfill.BatchSize != 100 || backfill.TargetCompletedVectors != 10000 {
		t.Fatalf("Indexing = %+v, want enabled settings", backfill)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(raw), "env-agent") {
		t.Fatalf("config file persisted env override: %s", string(raw))
	}
}

func TestUpdateWorkerRuntimeFilePreservesFileBackedSettingsOnly(t *testing.T) {
	t.Setenv("TIMICH_AGENT_HEAVY_TASK_WORKERS", "9")

	configPath := filepath.Join(t.TempDir(), "agent.json")
	cfg := Default()
	cfg.AgentName = "file-agent"
	cfg.AdminListenAddress = "127.0.0.1:8081"
	cfg.DataDir = "state"
	cfg.WorkerRuntime.HeavyTaskWorkers = testIntPtr(2)
	cfg.Datasources = []DatasourceConfig{{
		Name:        "Home Immich",
		Kind:        "immich",
		URL:         "http://immich.local:2283",
		AccessToken: "existing-api-key",
	}}
	if err := WriteFile(configPath, cfg); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.WorkerRuntime.HeavyTaskWorkers == nil || *loaded.WorkerRuntime.HeavyTaskWorkers != 9 {
		t.Fatal("test setup did not apply worker env override")
	}

	updated, err := UpdateWorkerRuntimeFile(configPath, WorkerRuntimeConfig{HeavyTaskWorkers: testIntPtr(3)})
	if err != nil {
		t.Fatalf("UpdateWorkerRuntimeFile() error = %v", err)
	}
	if updated.WorkerRuntime.HeavyTaskWorkers == nil || *updated.WorkerRuntime.HeavyTaskWorkers != 3 {
		t.Fatalf("HeavyTaskWorkers = %v, want 3", updated.WorkerRuntime.HeavyTaskWorkers)
	}
	if len(updated.Datasources) != 1 || updated.Datasources[0].AccessToken != "existing-api-key" {
		t.Fatalf("Datasources = %#v, want preserved datasource", updated.Datasources)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(raw), `"heavyTaskWorkers": 9`) {
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

func testIntPtr(value int) *int {
	return &value
}
