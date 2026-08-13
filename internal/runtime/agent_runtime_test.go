package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/pairing"
	"github.com/rsahara/timich-agent/internal/semanticruntimehelper"
	"github.com/rsahara/timich-agent/internal/store"
)

func TestResponsesUseRedactedConfigAndBuildDefaults(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name:        "home-immich",
			Kind:        "immich_indexed",
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

func TestStatusIncludesSemanticONNXRuntimeConfiguration(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.HelperPath = filepath.Join(t.TempDir(), "timich-semantic-helper")
		cfg.SemanticRuntime.ONNXRuntime.ServerPath = filepath.Join(t.TempDir(), "semantic-runtime", "siglip2-onnx", "server.py")
		cfg.SemanticRuntime.ONNXRuntime.PythonPath = "python3"
		cfg.SemanticRuntime.ONNXRuntime.Provider = "cpu"
	})

	status := runtime.StatusResponse()
	if status.SemanticRuntime.HelperPath == "" {
		t.Fatal("SemanticRuntime.HelperPath is empty, want configured helper path")
	}
	if !status.SemanticRuntime.ONNXRuntime.Managed ||
		status.SemanticRuntime.ONNXRuntime.Status != semanticONNXRuntimeStatusIdle ||
		status.SemanticRuntime.ONNXRuntime.ServerPath == "" ||
		status.SemanticRuntime.ONNXRuntime.PythonPath != "python3" ||
		status.SemanticRuntime.ONNXRuntime.Provider != "cpu" {
		t.Fatalf("SemanticRuntime.ONNXRuntime = %+v, want idle managed runtime config", status.SemanticRuntime.ONNXRuntime)
	}
}

func TestSystemResourcesStatusIncludesConfiguredPaths(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	localRoot := t.TempDir()
	unusedLocalRoot := t.TempDir()
	uploadRoot := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "Local",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "local",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.DataDir = dataDir
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{
			{Key: "local", Path: localRoot},
			{Key: "unused", Path: unusedLocalRoot},
		}
		cfg.UploadRoots = []config.UploadRootConfig{{Key: "uploads", Path: uploadRoot}}
	})

	status := runtime.SystemResourcesStatus()
	if status.CPU.LogicalCores <= 0 {
		t.Fatalf("CPU logical cores = %d, want positive", status.CPU.LogicalCores)
	}
	if len(status.Disks) != 3 {
		t.Fatalf("Disk resources = %+v, want data, local, and upload paths", status.Disks)
	}
	paths := map[string]bool{}
	for _, disk := range status.Disks {
		paths[disk.Path] = true
		if disk.TotalBytes <= 0 || disk.AvailableBytes <= 0 {
			t.Fatalf("Disk resource = %+v, want filesystem usage", disk)
		}
	}
	for _, path := range []string{dataDir, localRoot, uploadRoot} {
		if !paths[path] {
			t.Fatalf("Disk resources = %+v, missing %s", status.Disks, path)
		}
	}
	if paths[unusedLocalRoot] {
		t.Fatalf("Disk resources = %+v, included unused local root %s", status.Disks, unusedLocalRoot)
	}
}

func TestSemanticONNXRuntimeEnvKeyUsesModelAndVectorSpace(t *testing.T) {
	t.Parallel()

	got := semanticONNXRuntimeEnvKey("timich-siglip2-base-patch16-224", "timich-siglip2-base-patch16-224/d768")
	want := semanticruntimehelper.ONNXRuntimeServerEnvKey("timich-siglip2-base-patch16-224", "timich-siglip2-base-patch16-224/d768")
	if got != want {
		t.Fatalf("semanticONNXRuntimeEnvKey() = %q, want %q", got, want)
	}
	if got == semanticONNXRuntimeEnvKey("timich_siglip2_base_patch16_224", "timich-siglip2-base-patch16-224/d768") {
		t.Fatal("semanticONNXRuntimeEnvKey() collapsed distinct model identifiers")
	}
}

func TestSemanticONNXRuntimeServerURLBracketsIPv6Host(t *testing.T) {
	t.Parallel()

	if got := semanticONNXRuntimeServerURL("::1", 19188); got != "http://[::1]:19188" {
		t.Fatalf("semanticONNXRuntimeServerURL() = %q, want bracketed IPv6 authority", got)
	}
}

func TestSemanticRuntimeRecommendedVersionIsNotReplacedByInstalledOlderVersion(t *testing.T) {
	t.Parallel()

	store, err := catalog.LoadOrCreateSemanticRuntimePackStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticRuntimePackStore() error = %v", err)
	}
	artifact := semanticRuntimePackArtifactForRuntimeTest(t)
	sum := sha256.Sum256(artifact)
	installedV1 := catalog.SemanticRuntimePackStatus{
		ID:       "timich-onnx-runtime",
		Version:  "1.0.0",
		Runtime:  "onnxruntime",
		Platform: "linux-amd64",
		Artifact: &catalog.SemanticModelArtifactStatus{
			Filename:  "runtime.zip",
			SHA256:    hex.EncodeToString(sum[:]),
			SizeBytes: int64(len(artifact)),
		},
	}
	installedResult, err := store.InstallPack(context.Background(), installedV1, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack(v1) error = %v", err)
	}
	if err := store.FinalizePackInstall(installedV1.ID, installedV1.Version, installedResult.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(v1) error = %v", err)
	}
	recommendedV2 := installedV1
	recommendedV2.Version = "2.0.0"
	recommendedV2.Installed = false
	runtime := &AgentRuntime{semanticRuntimePacks: store}
	status := runtime.withInstalledSemanticRuntimePacks(catalog.SemanticModelRegistryStatus{
		RecommendedRuntimePack: &recommendedV2,
		RuntimePacks:           []catalog.SemanticRuntimePackStatus{recommendedV2},
	})
	if status.RecommendedRuntimePack == nil || status.RecommendedRuntimePack.Version != "2.0.0" || status.RecommendedRuntimePack.Installed {
		t.Fatalf("recommended runtime pack = %#v, want uninstalled v2", status.RecommendedRuntimePack)
	}
	versions := map[string]bool{}
	for _, pack := range status.RuntimePacks {
		versions[pack.Version] = pack.Installed
	}
	if versions["1.0.0"] != true || versions["2.0.0"] != false {
		t.Fatalf("runtime pack versions = %#v, want installed v1 plus recommended v2", versions)
	}
}

func TestSemanticONNXRuntimeCommandEnvSetsPythonHomeForBundledVenv(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), ".venv")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatalf("MkdirAll(bin) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o700); err != nil {
		t.Fatalf("MkdirAll(lib) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "lib", "python3.11", "encodings"), 0o700); err != nil {
		t.Fatalf("MkdirAll(encodings) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "lib", "python3.11", "encodings", "__init__.py"), []byte("# bundled stdlib marker\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(encodings) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "pyvenv.cfg"), []byte("home = /tmp/python\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(pyvenv.cfg) error = %v", err)
	}
	pythonPath := filepath.Join(root, "bin", "python")
	env := semanticONNXRuntimeCommandEnv("TIMICH_ONNX_SERVER_URL_TEST", "http://127.0.0.1:19188", pythonPath)
	if !envContains(env, "PYTHONHOME="+root) {
		t.Fatalf("semanticONNXRuntimeCommandEnv() missing PYTHONHOME=%s in %v", root, env)
	}
	if !envContains(env, "PYTHONNOUSERSITE=1") {
		t.Fatalf("semanticONNXRuntimeCommandEnv() missing PYTHONNOUSERSITE in %v", env)
	}
}

func TestSemanticONNXRuntimeCommandEnvDoesNotSetPythonHomeForOrdinaryVenv(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), ".venv")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatalf("MkdirAll(bin) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "lib", "python3.11", "site-packages"), 0o700); err != nil {
		t.Fatalf("MkdirAll(site-packages) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "pyvenv.cfg"), []byte("home = /tmp/python\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(pyvenv.cfg) error = %v", err)
	}
	pythonPath := filepath.Join(root, "bin", "python")
	env := semanticONNXRuntimeCommandEnv("TIMICH_ONNX_SERVER_URL_TEST", "http://127.0.0.1:19188", pythonPath)
	if envContainsPrefix(env, "PYTHONHOME=") {
		t.Fatalf("semanticONNXRuntimeCommandEnv() unexpectedly set PYTHONHOME for ordinary venv: %v", env)
	}
	if envContains(env, "PYTHONNOUSERSITE=1") {
		t.Fatalf("semanticONNXRuntimeCommandEnv() unexpectedly set PYTHONNOUSERSITE for ordinary venv: %v", env)
	}
}

func TestSemanticONNXRuntimeStartsInstalledPackProcessAndCleansEnv(t *testing.T) {
	serverPath, pythonPath := writeSemanticONNXRuntimeExecutables(t)
	pack := runtimeSemanticPackForTest("managed-onnx-test")
	envKey := semanticONNXRuntimeEnvKey(pack.ID, pack.VectorSpaceID)
	previousEnv, hadPreviousEnv := os.LookupEnv(envKey)
	if err := os.Unsetenv(envKey); err != nil {
		t.Fatalf("Unsetenv(%s) error = %v", envKey, err)
	}
	t.Cleanup(func() {
		if hadPreviousEnv {
			_ = os.Setenv(envKey, previousEnv)
		} else {
			_ = os.Unsetenv(envKey)
		}
	})

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.ONNXRuntime.ServerPath = serverPath
		cfg.SemanticRuntime.ONNXRuntime.PythonPath = pythonPath
		if err := config.WriteFile(cfg.ConfigPath, cfg.Config); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	})
	installRuntimeSemanticPackForTest(t, runtime, pack)

	status := runtime.StatusResponse().SemanticRuntime.ONNXRuntime
	if status.Status != semanticONNXRuntimeStatusRunning || status.ProcessCount != 1 || len(status.Runtimes) != 1 {
		t.Fatalf("ONNX runtime status = %+v, want one running process", status)
	}
	if got := os.Getenv(envKey); !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Fatalf("%s = %q, want managed localhost URL", envKey, got)
	}
	firstPID := status.Runtimes[0].PID
	replacementServerPath := filepath.Join(t.TempDir(), "replacement-server.py")
	serverBytes, err := os.ReadFile(serverPath)
	if err != nil {
		t.Fatalf("ReadFile(test server) error = %v", err)
	}
	if err := os.WriteFile(replacementServerPath, serverBytes, 0o600); err != nil {
		t.Fatalf("WriteFile(replacement server) error = %v", err)
	}
	runtime.semanticONNXRuntime.mu.Lock()
	runtime.semanticONNXRuntime.cfg.ServerPath = replacementServerPath
	runtime.semanticONNXRuntime.mu.Unlock()
	runtime.reconcileSemanticModelRuntime()
	status = runtime.StatusResponse().SemanticRuntime.ONNXRuntime
	if status.Status != semanticONNXRuntimeStatusRunning || status.ProcessCount != 1 || len(status.Runtimes) != 1 || status.Runtimes[0].PID == firstPID {
		t.Fatalf("ONNX runtime after execution config replacement = %+v, want one restarted process", status)
	}
	secondPID := status.Runtimes[0].PID
	runtime.semanticONNXRuntime.InvalidateLaunchIdentities()
	runtime.reconcileSemanticModelRuntime()
	status = runtime.StatusResponse().SemanticRuntime.ONNXRuntime
	if status.Status != semanticONNXRuntimeStatusRunning || status.ProcessCount != 1 || len(status.Runtimes) != 1 || status.Runtimes[0].PID == secondPID {
		t.Fatalf("ONNX runtime after runtime-pack identity invalidation = %+v, want one restarted process", status)
	}
	if _, err := runtime.UpdatePrimaryDatasource(config.DatasourceConfig{
		Name: "Home Immich",
		Kind: config.DatasourceKindImmich,
		URL:  "http://immich.local:2283",
	}); err != nil {
		t.Fatalf("UpdatePrimaryDatasource(passthrough) error = %v", err)
	}
	status = runtime.StatusResponse().SemanticRuntime.ONNXRuntime
	if status.Status != semanticONNXRuntimeStatusIdle || status.ProcessCount != 0 {
		t.Fatalf("ONNX runtime status after passthrough switch = %+v, want idle without processes", status)
	}
	if got := os.Getenv(envKey); got != "" {
		t.Fatalf("%s after passthrough switch = %q, want cleared", envKey, got)
	}
	// Simulate an Indexed reconcile that read topology before the switch but
	// reached the process manager after the newer Passthrough generation.
	runtime.semanticONNXRuntime.Reconcile(context.Background(), 0, true)
	status = runtime.StatusResponse().SemanticRuntime.ONNXRuntime
	if status.Status != semanticONNXRuntimeStatusIdle || status.ProcessCount != 0 {
		t.Fatalf("ONNX runtime status after stale Indexed reconcile = %+v, want newer Passthrough topology preserved", status)
	}
	if _, err := runtime.UpdatePrimaryDatasource(config.DatasourceConfig{
		Name: "Home Immich",
		Kind: config.DatasourceKindImmichIndexed,
		URL:  "http://immich.local:2283",
	}); err != nil {
		t.Fatalf("UpdatePrimaryDatasource(indexed) error = %v", err)
	}
	status = runtime.StatusResponse().SemanticRuntime.ONNXRuntime
	if status.Status != semanticONNXRuntimeStatusRunning || status.ProcessCount != 1 {
		t.Fatalf("ONNX runtime status after Indexed switch = %+v, want restarted process", status)
	}
	runtime.semanticONNXRuntime.Close()
	if got := os.Getenv(envKey); got != "" {
		t.Fatalf("%s after Close = %q, want cleared", envKey, got)
	}
}

func TestSemanticONNXRuntimeRestartsUnexpectedExitWithoutExternalReconcile(t *testing.T) {
	serverPath, pythonPath := writeSemanticONNXRuntimeExecutables(t)
	pack := runtimeSemanticPackForTest("managed-onnx-restart-test")
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.ONNXRuntime.ServerPath = serverPath
		cfg.SemanticRuntime.ONNXRuntime.PythonPath = pythonPath
	})
	defer runtime.Close()
	installRuntimeSemanticPackForTest(t, runtime, pack)

	key := semanticONNXRuntimeKey(catalog.SemanticModelRuntimeLayout{
		ModelID:       pack.ID,
		VectorSpaceID: pack.VectorSpaceID,
	})
	runtime.semanticONNXRuntime.mu.Lock()
	process := runtime.semanticONNXRuntime.processes[key]
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		runtime.semanticONNXRuntime.mu.Unlock()
		t.Fatal("managed semantic runtime process is missing")
	}
	firstPID := process.cmd.Process.Pid
	if err := process.cmd.Process.Kill(); err != nil {
		runtime.semanticONNXRuntime.mu.Unlock()
		t.Fatalf("Kill() error = %v", err)
	}
	runtime.semanticONNXRuntime.mu.Unlock()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		status := runtime.StatusResponse().SemanticRuntime.ONNXRuntime
		if len(status.Runtimes) == 1 &&
			status.Runtimes[0].Status == semanticONNXRuntimeStatusRunning &&
			status.Runtimes[0].PID != 0 &&
			status.Runtimes[0].PID != firstPID {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("semantic runtime did not restart after unexpected exit: %+v", runtime.StatusResponse().SemanticRuntime.ONNXRuntime)
}

func TestSemanticModelPackUpgradeRollsBackWhenExactRuntimeProbeFails(t *testing.T) {
	serverPath, pythonPath := writeSemanticONNXRuntimeExecutables(t)
	oldPack := runtimeSemanticPackForTest("managed-onnx-upgrade-test")
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.ONNXRuntime.ServerPath = serverPath
		cfg.SemanticRuntime.ONNXRuntime.PythonPath = pythonPath
	})
	defer runtime.Close()
	installRuntimeSemanticPackForTest(t, runtime, oldPack)
	if _, err := runtime.semanticModels.ActivatePack(oldPack.ID, oldPack.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack(old) error = %v", err)
	}
	runtime.reconcileSemanticModelRuntime()

	newPack := oldPack
	newPack.Version = "2026.07.19"
	artifact := runtimeSemanticPackArtifactForTest(t, newPack)
	sum := sha256.Sum256(artifact)
	newPack.Artifact.SHA256 = hex.EncodeToString(sum[:])
	newPack.Artifact.SizeBytes = int64(len(artifact))
	newPack.SizeBytes = int64(len(artifact))
	runtime.semanticONNXRuntime.mu.Lock()
	runtime.semanticONNXRuntime.cfg.ServerPath = filepath.Join(t.TempDir(), "missing-server.py")
	runtime.semanticONNXRuntime.mu.Unlock()
	installCtx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	if _, err := runtime.InstallSemanticModelPack(installCtx, newPack, bytes.NewReader(artifact)); err == nil {
		t.Fatal("InstallSemanticModelPack(upgrade) error = nil, want failed exact runtime probe")
	}
	active, ok := runtime.semanticModels.ActiveProfile()
	if !ok || active.ModelPack == nil || active.ModelPack.Version != oldPack.Version {
		t.Fatalf("active profile after failed upgrade = %#v ok=%v, want previous version %q", active, ok, oldPack.Version)
	}

	runtime.semanticONNXRuntime.mu.Lock()
	runtime.semanticONNXRuntime.cfg.ServerPath = serverPath
	runtime.semanticONNXRuntime.mu.Unlock()
	runtime.semanticONNXRuntime.InvalidateLaunchIdentities()
	runtime.reconcileSemanticModelRuntime()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := runtime.StatusResponse().SemanticRuntime.ONNXRuntime
		if status.Status == semanticONNXRuntimeStatusRunning && len(status.Runtimes) == 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("previous semantic runtime did not recover after rollback: %+v", runtime.StatusResponse().SemanticRuntime.ONNXRuntime)
}

func TestSemanticRuntimePackProbeUsesCandidateWhenIndexingIsDisabled(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "candidate-server.py")
	if err := os.WriteFile(serverPath, []byte("candidate server\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(candidate server) error = %v", err)
	}
	pythonPath := filepath.Join(tempDir, "candidate-python")
	probe := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = %q ] && [ \"$2\" = \"--help\" ]\n", serverPath)
	if err := os.WriteFile(pythonPath, []byte(probe), 0o700); err != nil {
		t.Fatalf("WriteFile(candidate python) error = %v", err)
	}

	manager := newSemanticONNXRuntimeManager(config.SemanticONNXRuntimeConfig{
		Disabled:   true,
		ServerPath: filepath.Join(tempDir, "missing-operator-override.py"),
		PythonPath: filepath.Join(tempDir, "missing-operator-python"),
	}, nil, nil)
	manager.indexingConfigured = false
	err := manager.ProbeRuntimePack(context.Background(), catalog.SemanticRuntimePackStatus{
		ID:         "candidate-runtime",
		Runtime:    "onnxruntime",
		ServerPath: serverPath,
		PythonPath: pythonPath,
	}, "candidate-install")
	if err != nil {
		t.Fatalf("ProbeRuntimePack(candidate) error = %v", err)
	}
}

func TestSemanticModelProbeRejectsAmbientServerWithoutDirectLaunchSpec(t *testing.T) {
	t.Setenv("TIMICH_SEMANTIC_ONNX_SERVER_URL", "http://127.0.0.1:65535")
	manager := newSemanticONNXRuntimeManager(config.SemanticONNXRuntimeConfig{}, nil, nil)
	err := manager.ProbeModelLayout(context.Background(), catalog.SemanticModelRuntimeLayout{
		ModelID:       "replacement",
		VectorSpaceID: "replacement/d4",
		Runtime:       "onnxruntime",
		RuntimePath:   t.TempDir(),
		EmbeddingDim:  4,
		InputKind:     "image",
	}, "replacement-install")
	if err == nil || !strings.Contains(err.Error(), "server path is not configured") {
		t.Fatalf("ProbeModelLayout() error = %v, want direct launch requirement", err)
	}
}

func TestSemanticONNXLaunchSpecSharesProviderArguments(t *testing.T) {
	t.Parallel()
	spec := semanticONNXRuntimeLaunchSpec{
		ServerPath:    "/runtime/server.py",
		PythonPath:    "/runtime/python",
		Provider:      "cpu",
		TextProvider:  "openvino:CPU",
		ImageProvider: "openvino:GPU",
		TextTemplate:  "query: {query}",
	}
	args := spec.commandArgs(catalog.SemanticModelRuntimeLayout{RuntimePath: "/models/staged"}, "127.0.0.1", 19188)
	want := []string{
		"/runtime/server.py", "--runtime-layout", "/models/staged", "--host", "127.0.0.1", "--port", "19188",
		"--provider", "cpu", "--text-provider", "openvino:CPU", "--image-provider", "openvino:GPU", "--text-template", "query: {query}",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("commandArgs() = %#v, want %#v", args, want)
	}
}

func TestSemanticONNXEffectiveConfigPreservesOneSidedOverride(t *testing.T) {
	t.Parallel()
	manager := newSemanticONNXRuntimeManager(config.SemanticONNXRuntimeConfig{
		ServerPath:         "/operator/server.py",
		PythonPath:         "/auto/python",
		PythonAutoDetected: true,
	}, nil, nil)
	manager.mu.Lock()
	effective := manager.effectiveConfigLocked()
	manager.mu.Unlock()
	if effective.ServerPath != "/operator/server.py" || effective.PythonPath != "/auto/python" {
		t.Fatalf("effective config lost one-sided override pair: %#v", effective)
	}
}

func TestSemanticONNXConfigResolverCombinesExplicitAndInstalledPaths(t *testing.T) {
	t.Parallel()
	pack := catalog.SemanticRuntimePackStatus{ServerPath: "/pack/server.py", PythonPath: "/pack/python"}
	tests := []struct {
		name       string
		cfg        config.SemanticONNXRuntimeConfig
		wantServer string
		wantPython string
	}{
		{
			name: "explicit server with installed Python",
			cfg: config.SemanticONNXRuntimeConfig{
				ServerPath: "/operator/server.py",
			},
			wantServer: "/operator/server.py",
			wantPython: "/pack/python",
		},
		{
			name: "installed server with explicit Python",
			cfg: config.SemanticONNXRuntimeConfig{
				PythonPath: "/operator/python",
			},
			wantServer: "/pack/server.py",
			wantPython: "/operator/python",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			resolved := resolveSemanticONNXRuntimeConfig(tt.cfg, &pack)
			if resolved.ServerPath != tt.wantServer || resolved.PythonPath != tt.wantPython {
				t.Fatalf("resolved config = %#v, want server=%q python=%q", resolved, tt.wantServer, tt.wantPython)
			}
		})
	}
}

func TestSemanticRuntimePackProbeCombinesCandidateWithOneSidedOperatorOverride(t *testing.T) {
	tempDir := t.TempDir()
	operatorServerPath := filepath.Join(tempDir, "operator-server.py")
	if err := os.WriteFile(operatorServerPath, []byte("operator server\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(operator server) error = %v", err)
	}
	serverPath := filepath.Join(tempDir, "candidate-server.py")
	if err := os.WriteFile(serverPath, []byte("candidate server\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(candidate server) error = %v", err)
	}
	pythonPath := filepath.Join(tempDir, "candidate-python")
	probe := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = %q ] && [ \"$2\" = \"--help\" ]\n", operatorServerPath)
	if err := os.WriteFile(pythonPath, []byte(probe), 0o700); err != nil {
		t.Fatalf("WriteFile(candidate python) error = %v", err)
	}
	manager := newSemanticONNXRuntimeManager(config.SemanticONNXRuntimeConfig{
		ServerPath:         operatorServerPath,
		PythonPath:         "/auto/python",
		PythonAutoDetected: true,
	}, nil, nil)
	err := manager.ProbeRuntimePack(context.Background(), catalog.SemanticRuntimePackStatus{
		ID:         "candidate-runtime",
		Runtime:    "onnxruntime",
		ServerPath: serverPath,
		PythonPath: pythonPath,
	}, "replacement-runtime")
	if err != nil {
		t.Fatalf("ProbeRuntimePack() error = %v", err)
	}
}

func TestSemanticRuntimePackProbeChecksEveryInstalledModelLayout(t *testing.T) {
	dataDir := t.TempDir()
	modelStore, err := catalog.LoadOrCreateSemanticModelPackStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStore() error = %v", err)
	}
	install := func(pack catalog.SemanticModelPackStatus) string {
		t.Helper()
		artifact := runtimeSemanticPackArtifactForTest(t, pack)
		sum := sha256.Sum256(artifact)
		pack.Artifact.SHA256 = hex.EncodeToString(sum[:])
		pack.Artifact.SizeBytes = int64(len(artifact))
		pack.SizeBytes = int64(len(artifact))
		result, err := modelStore.InstallPack(context.Background(), pack, bytes.NewReader(artifact))
		if err != nil {
			t.Fatalf("InstallPack(%s) error = %v", pack.ID, err)
		}
		if err := modelStore.FinalizePackInstall(pack.ID, pack.VectorSpaceID, result.InstallID); err != nil {
			t.Fatalf("FinalizePackInstall(%s) error = %v", pack.ID, err)
		}
		return result.InstallID
	}
	first := runtimeSemanticPackForTest("probe-first-model")
	install(first)
	if _, err := modelStore.ActivatePack(first.ID, first.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack(first) error = %v", err)
	}
	second := runtimeSemanticPackForTest("probe-second-model")
	install(second)

	probeLogPath := filepath.Join(t.TempDir(), "runtime-probes.log")
	serverPath, pythonPath := writeSemanticONNXRuntimeExecutablesWithProbeLog(t, probeLogPath)
	manager := newSemanticONNXRuntimeManager(config.SemanticONNXRuntimeConfig{}, modelStore, nil)
	if err := manager.ProbeRuntimePack(context.Background(), catalog.SemanticRuntimePackStatus{
		ID:         "candidate-runtime",
		Runtime:    "onnxruntime",
		ServerPath: serverPath,
		PythonPath: pythonPath,
	}, "candidate-install"); err != nil {
		t.Fatalf("ProbeRuntimePack() error = %v", err)
	}
	rawLog, err := os.ReadFile(probeLogPath)
	if err != nil {
		t.Fatalf("ReadFile(probe log) error = %v", err)
	}
	probed := strings.Fields(string(rawLog))
	slices.Sort(probed)
	if !slices.Equal(probed, []string{first.ID, second.ID}) {
		t.Fatalf("probed model layouts = %v, want both installed layouts", probed)
	}
}

func TestSemanticONNXRuntimeFixedPortsRemainUniqueAfterFirstModelRestarts(t *testing.T) {
	serverPath, pythonPath := writeSemanticONNXRuntimeExecutables(t)
	const basePort = 29188
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.ONNXRuntime.ServerPath = serverPath
		cfg.SemanticRuntime.ONNXRuntime.PythonPath = pythonPath
		cfg.SemanticRuntime.ONNXRuntime.Port = basePort
	})
	packA := runtimeSemanticPackForTest("fixed-port-model-a")
	packB := runtimeSemanticPackForTest("fixed-port-model-b")
	installRuntimeSemanticPackForTest(t, runtime, packA)
	if _, err := runtime.semanticModels.ActivatePack(packA.ID, packA.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack(A) error = %v", err)
	}
	runtime.reconcileSemanticModelRuntime()
	installRuntimeSemanticPackForTest(t, runtime, packB)

	assertPorts := func(wantA int, wantB int) {
		t.Helper()
		status := runtime.StatusResponse().SemanticRuntime.ONNXRuntime
		ports := map[string]int{}
		for _, process := range status.Runtimes {
			parts := strings.Split(process.ServerURL, ":")
			if len(parts) == 0 {
				t.Fatalf("runtime URL = %q", process.ServerURL)
			}
			var port int
			if _, err := fmt.Sscanf(parts[len(parts)-1], "%d", &port); err != nil {
				t.Fatalf("parse runtime URL %q: %v", process.ServerURL, err)
			}
			ports[process.ModelID] = port
		}
		if ports[packA.ID] != wantA || ports[packB.ID] != wantB || ports[packA.ID] == ports[packB.ID] {
			t.Fatalf("runtime ports = %#v, want A=%d B=%d", ports, wantA, wantB)
		}
	}
	assertPorts(basePort, basePort+1)

	keyA := semanticONNXRuntimeKey(catalog.SemanticModelRuntimeLayout{
		ModelID:       packA.ID,
		VectorSpaceID: packA.VectorSpaceID,
	})
	runtime.semanticONNXRuntime.mu.Lock()
	processA := runtime.semanticONNXRuntime.processes[keyA]
	runtime.semanticONNXRuntime.stopProcessLocked(processA)
	delete(runtime.semanticONNXRuntime.processes, keyA)
	runtime.semanticONNXRuntime.mu.Unlock()
	runtime.reconcileSemanticModelRuntime()
	assertPorts(basePort, basePort+1)
}

func TestSemanticONNXRuntimeStaysIdleForPassthroughOnly(t *testing.T) {
	serverPath, pythonPath := writeSemanticONNXRuntimeExecutables(t)
	pack := runtimeSemanticPackForTest("passthrough-onnx-test")
	envKey := semanticONNXRuntimeEnvKey(pack.ID, pack.VectorSpaceID)
	previousEnv, hadPreviousEnv := os.LookupEnv(envKey)
	if err := os.Unsetenv(envKey); err != nil {
		t.Fatalf("Unsetenv(%s) error = %v", envKey, err)
	}
	t.Cleanup(func() {
		if hadPreviousEnv {
			_ = os.Setenv(envKey, previousEnv)
		} else {
			_ = os.Unsetenv(envKey)
		}
	})

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmich,
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.ONNXRuntime.ServerPath = serverPath
		cfg.SemanticRuntime.ONNXRuntime.PythonPath = pythonPath
	})
	installRuntimeSemanticPackForTest(t, runtime, pack)
	runtime.StartSemanticModelRuntime()

	status := runtime.StatusResponse().SemanticRuntime.ONNXRuntime
	if status.Status != semanticONNXRuntimeStatusIdle || status.ProcessCount != 0 {
		t.Fatalf("Passthrough-only ONNX runtime status = %+v, want idle without processes", status)
	}
	if got := os.Getenv(envKey); got != "" {
		t.Fatalf("%s = %q, want no managed runtime environment for Passthrough", envKey, got)
	}
}

func writeSemanticONNXRuntimeExecutables(t *testing.T) (string, string) {
	return writeSemanticONNXRuntimeExecutablesWithProbeLog(t, "")
}

func writeSemanticONNXRuntimeExecutablesWithProbeLog(t *testing.T, probeLogPath string) (string, string) {
	t.Helper()

	tempDir := t.TempDir()
	serverPath := filepath.Join(tempDir, "server.py")
	server := `import argparse
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

parser = argparse.ArgumentParser(add_help=False)
parser.add_argument("--host", default="127.0.0.1")
parser.add_argument("--port", type=int, required=True)
parser.add_argument("--runtime-layout", required=True)
args, _ = parser.parse_known_args()

with open(args.runtime_layout + "/timich-model.json", "r", encoding="utf-8") as handle:
    layout = json.load(handle)
probe_log_path = ` + fmt.Sprintf("%q", probeLogPath) + `
if probe_log_path:
    with open(probe_log_path, "a", encoding="utf-8") as handle:
        handle.write(layout["modelId"] + "\n")

def embedding_response(input_name):
    vector = [0.0] * int(layout["embeddingDim"])
    vector[0] = 1.0
    return {
        "protocolVersion": 1,
        "runtime": layout["runtime"],
        "modelId": layout["modelId"],
        "vectorSpaceId": layout["vectorSpaceId"],
        "embeddingDim": layout["embeddingDim"],
        "inputKind": layout["inputKind"],
        "vector": vector,
        "input": input_name,
    }

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/healthz":
            self.send_error(404)
            return
        body = json.dumps({"status": "ok"}).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if self.path not in ("/embed-text", "/embed-image"):
            self.send_error(404)
            return
        length = int(self.headers.get("Content-Length", "0"))
        if length:
            self.rfile.read(length)
        body = json.dumps(embedding_response(self.path)).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):
        return

ThreadingHTTPServer((args.host, args.port), Handler).serve_forever()
`
	if err := os.WriteFile(serverPath, []byte(server), 0o600); err != nil {
		t.Fatalf("WriteFile(server) error = %v", err)
	}
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for semantic runtime process tests")
	}
	return serverPath, pythonPath
}

func envContains(env []string, want string) bool {
	for _, item := range env {
		if item == want {
			return true
		}
	}
	return false
}

func envContainsPrefix(env []string, prefix string) bool {
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
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

func TestStatusAndConfigExposeLocalMediaRootSummaries(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
	})

	status := runtime.StatusResponse()
	if len(status.LocalMediaRoots) != 1 || status.LocalMediaRoots[0].Key != "nas-photos" || !status.LocalMediaRoots[0].Readable {
		t.Fatalf("Status LocalMediaRoots = %+v, want readable local root", status.LocalMediaRoots)
	}

	configResponse := runtime.ConfigResponse()
	if len(configResponse.LocalMediaRoots) != 1 || configResponse.LocalMediaRoots[0].Path != rootPath || configResponse.LocalMediaRoots[0].Status != "ready" {
		t.Fatalf("Config LocalMediaRoots = %+v, want ready local root", configResponse.LocalMediaRoots)
	}
}

func TestStatusConfigAndIndexingBlockSymlinkLocalMediaRoot(t *testing.T) {
	t.Parallel()

	parentPath := t.TempDir()
	realRootPath := filepath.Join(parentPath, "photos-real")
	if err := os.Mkdir(realRootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(real root) error = %v", err)
	}
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Symlink(realRootPath, rootPath); err != nil {
		t.Skipf("symbolic-link creation is unavailable: %v", err)
	}

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
	})

	assertBlocked := func(label string, roots []LocalMediaRootSummary) {
		t.Helper()
		if len(roots) != 1 || roots[0].Status != "blocked" || roots[0].Readable || !strings.Contains(roots[0].Message, "symbolic link") {
			t.Fatalf("%s LocalMediaRoots = %+v, want blocked symlink root", label, roots)
		}
	}
	assertBlocked("Status", runtime.StatusResponse().LocalMediaRoots)
	assertBlocked("Config", runtime.ConfigResponse().LocalMediaRoots)
	assertBlocked("Indexing", runtime.emptyDatasourceIndexingResponse().Roots)
}

func TestStatusConfigAndIndexingRequireAcceptanceForChangedLocalMediaRoot(t *testing.T) {
	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), []byte("family"), 0o644); err != nil {
		t.Fatalf("WriteFile(family) error = %v", err)
	}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
	})
	if _, err := runtime.catalogService().RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if err := os.Rename(rootPath, filepath.Join(parentPath, "mounted")); err != nil {
		t.Fatalf("Rename(root) error = %v", err)
	}
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "placeholder.jpg"), []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("WriteFile(placeholder) error = %v", err)
	}

	assertBlocked := func(label string, roots []LocalMediaRootSummary) string {
		t.Helper()
		if len(roots) != 1 || roots[0].Status != "blocked" || roots[0].Readable || !roots[0].RootAcceptanceRequired || roots[0].ObservedRootIdentity == "" || !strings.Contains(roots[0].Message, "administrator acceptance") {
			t.Fatalf("%s LocalMediaRoots = %+v, want explicit root acceptance", label, roots)
		}
		return roots[0].ObservedRootIdentity
	}
	statusResponse := runtime.StatusResponse()
	statusIdentity := assertBlocked("Status", statusResponse.LocalMediaRoots)
	var datasourceTask *SetupTask
	for index := range statusResponse.SetupTasks {
		if statusResponse.SetupTasks[index].ID == "datasource" {
			datasourceTask = &statusResponse.SetupTasks[index]
			break
		}
	}
	if datasourceTask == nil || datasourceTask.Status != "pending" || !strings.Contains(datasourceTask.Summary, "root warning") {
		t.Fatalf("datasource setup task = %+v, want pending root warning", datasourceTask)
	}
	configIdentity := assertBlocked("Config", runtime.ConfigResponse().LocalMediaRoots)
	indexingIdentity := assertBlocked("Indexing", runtime.emptyDatasourceIndexingResponse().Roots)
	if statusIdentity != configIdentity || statusIdentity != indexingIdentity {
		t.Fatalf("observed identities differ status=%q config=%q indexing=%q", statusIdentity, configIdentity, indexingIdentity)
	}
	scanStatus, err := runtime.LocalDatasourceScanStatus(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatus() error = %v", err)
	}
	if len(scanStatus.Datasources) != 1 || scanStatus.Datasources[0].RootStatus != catalog.LocalMediaRootStatusIdentityChanged || scanStatus.Datasources[0].Phase0Status != "blocked" || !scanStatus.Datasources[0].RootAcceptanceRequired || scanStatus.Datasources[0].ObservedRootIdentity != statusIdentity {
		t.Fatalf("LocalDatasourceScanStatus() = %+v, want blocked acceptance candidate", scanStatus)
	}
}

func TestLocalMediaRootAcceptanceResponsePreservesCommittedAcceptanceWhenScanFails(t *testing.T) {
	acceptance := catalog.LocalMediaRootAcceptanceResult{
		SourceKey: "1111111111111111",
		RootKey:   "nas-photos",
		Accepted:  true,
	}
	phase0 := catalog.LocalPhase0ScanResult{
		SourceKey: "1111111111111111",
		RootKey:   "nas-photos",
		ScanMode:  "reconciliation",
		Status:    "failed",
	}

	response := localMediaRootAcceptanceResponse(acceptance, phase0, context.DeadlineExceeded)

	if !response.Acceptance.Accepted || response.ScanStatus != "failed" || response.ScanError != context.DeadlineExceeded.Error() || response.Phase0.SourceKey != phase0.SourceKey {
		t.Fatalf("acceptance response = %+v, want committed acceptance and separate failed scan outcome", response)
	}
}

func TestAcceptLocalMediaRootDiscoveryWaitHonorsContext(t *testing.T) {
	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	releaseDiscovery, err := runtime.localDiscovery.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire local discovery single-flight: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		_, acceptErr := runtime.AcceptLocalMediaRoot(ctx, "source", "root", "identity")
		returned <- acceptErr
	}()

	select {
	case acceptErr := <-returned:
		releaseDiscovery()
		if !errors.Is(acceptErr, context.DeadlineExceeded) {
			t.Fatalf("AcceptLocalMediaRoot() error = %v, want context deadline", acceptErr)
		}
	case <-time.After(250 * time.Millisecond):
		releaseDiscovery()
		select {
		case <-returned:
		case <-time.After(time.Second):
		}
		t.Fatal("AcceptLocalMediaRoot() remained blocked after its context deadline")
	}
}

func TestFailedLocalMediaRootAcceptanceInvalidatesSchedulerWorkState(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), []byte("family"), 0o644); err != nil {
		t.Fatalf("WriteFile(family.jpg) error = %v", err)
	}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
	})
	if result, err := runtime.catalogService().RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil || result.QueuedMetadata != 1 {
		t.Fatalf("RunLocalReconciliationScan() result=%+v error=%v, want one metadata job", result, err)
	}

	wake := make(chan struct{}, 1)
	runtime.semanticBackfillMu.Lock()
	runtime.backgroundWorkerWake = wake
	runtime.semanticBackfillMu.Unlock()
	schedule := semanticIndexingSchedule{}
	runtime.schedulerWorkStateMu.Lock()
	runtime.schedulerWorkState = schedulerWorkState{
		ConfigHash: runtime.schedulerWorkStateConfigHash(schedule, false),
		UpdatedAt:  time.Now().UTC(),
	}
	runtime.schedulerWorkStateSeq = 41
	runtime.schedulerWorkStateMu.Unlock()

	if response, err := runtime.AcceptLocalMediaRoot(context.Background(), "1111111111111111", "nas-photos", "stale-identity"); !errors.Is(err, catalog.ErrLocalMediaRootAcceptanceStale) || response.Acceptance.Accepted {
		t.Fatalf("AcceptLocalMediaRoot(stale) response=%+v error=%v", response, err)
	}
	runtime.schedulerWorkStateMu.Lock()
	dirty := runtime.schedulerWorkState.Dirty
	sequence := runtime.schedulerWorkStateSeq
	runtime.schedulerWorkStateMu.Unlock()
	if !dirty || sequence <= 41 {
		t.Fatalf("scheduler work state dirty=%t sequence=%d, want invalidated after failed transition", dirty, sequence)
	}
	select {
	case <-wake:
	default:
		t.Fatal("failed root transition did not wake the background scheduler")
	}

	state, ok := runtime.schedulerWorkStateSnapshot(context.Background(), schedule, false)
	if !ok || state.MetadataQueued+state.MetadataSettling != 1 {
		t.Fatalf("recomputed scheduler state ok=%t state=%+v, want retained metadata job", ok, state)
	}
}

func TestSuccessfulLocalMediaRootAcceptanceInvalidatesSchedulerBeforeAndAfterScan(t *testing.T) {
	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), []byte("family"), 0o644); err != nil {
		t.Fatalf("WriteFile(family.jpg) error = %v", err)
	}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
	})
	if _, err := runtime.catalogService().RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("initial RunLocalReconciliationScan() error = %v", err)
	}
	if err := os.Rename(rootPath, filepath.Join(parentPath, "trusted")); err != nil {
		t.Fatalf("Rename(root) error = %v", err)
	}
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement root) error = %v", err)
	}
	statuses, err := runtime.catalogService().LocalMediaRootContinuityStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalMediaRootContinuityStatuses() error = %v", err)
	}
	if len(statuses) != 1 || !statuses[0].AcceptanceRequired || statuses[0].ObservedRootIdentity == "" {
		t.Fatalf("root continuity statuses = %+v, want replacement candidate", statuses)
	}

	wake := make(chan struct{}, 1)
	runtime.semanticBackfillMu.Lock()
	runtime.backgroundWorkerWake = wake
	runtime.semanticBackfillMu.Unlock()
	runtime.schedulerWorkStateMu.Lock()
	runtime.schedulerWorkState = schedulerWorkState{UpdatedAt: time.Now().UTC()}
	runtime.schedulerWorkStateSeq = 80
	runtime.schedulerWorkStateMu.Unlock()

	response, err := runtime.AcceptLocalMediaRoot(context.Background(), "1111111111111111", "nas-photos", statuses[0].ObservedRootIdentity)
	if err != nil {
		t.Fatalf("AcceptLocalMediaRoot() error = %v", err)
	}
	if !response.Acceptance.Accepted || response.ScanStatus != "completed" {
		t.Fatalf("acceptance response = %+v, want accepted and completed", response)
	}
	runtime.schedulerWorkStateMu.Lock()
	dirty := runtime.schedulerWorkState.Dirty
	sequence := runtime.schedulerWorkStateSeq
	runtime.schedulerWorkStateMu.Unlock()
	if !dirty || sequence != 82 {
		t.Fatalf("scheduler work state dirty=%t sequence=%d, want immediate and final invalidation", dirty, sequence)
	}
	select {
	case <-wake:
	default:
		t.Fatal("successful root acceptance did not wake the background scheduler")
	}
}

func TestInfoResponseReportsNoDatasourcePairingStatus(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{
		Version:    "test-version",
		Commit:     "test-commit",
		BuiltAt:    "2026-04-25T00:00:00Z",
		ReleaseTag: "v0.4.0-rc.2",
	}, nil)

	info := runtime.InfoResponse()
	if info.Version != "test-version" || info.Commit != "test-commit" || info.ReleaseTag != "v0.4.0-rc.2" {
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

func TestInfoResponseDoesNotExposeDatasourceConfiguration(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
	})

	payload, err := json.Marshal(runtime.InfoResponse())
	if err != nil {
		t.Fatalf("json.Marshal(InfoResponse()) error = %v", err)
	}
	for _, forbidden := range [][]byte{[]byte(rootPath), []byte(`"rootPath"`), []byte(`"rootKey"`), []byte(`"sourceKey"`), []byte(`"datasources"`), []byte(`"hasAccessToken"`)} {
		if bytes.Contains(payload, forbidden) {
			t.Fatalf("InfoResponse JSON leaked datasource configuration %q: %s", forbidden, payload)
		}
	}

	admin := runtime.Datasources()
	if len(admin) != 1 || admin[0].RootKey != "nas-photos" || admin[0].RootPath != rootPath {
		t.Fatalf("Datasources() = %+v, want authenticated root details", admin)
	}
}

func TestInfoResponseHidesAgentIDUntilRelayCredentialRegistered(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name:        "Home Immich",
			Kind:        "immich_indexed",
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
			Kind:        "immich_indexed",
			URL:         "http://immich.local:2283",
			AccessToken: "immich-token",
		},
	})

	ready, reason := runtime.RemoteRegistrationReady()
	if ready || !strings.Contains(reason, "paired device") {
		t.Fatalf("RemoteRegistrationReady() = %v/%q, want paired device blocker", ready, reason)
	}
	status := runtime.StatusResponse()
	if len(status.SetupTasks) != 5 {
		t.Fatalf("SetupTasks length = %d, want 5", len(status.SetupTasks))
	}
	gotSetupTaskIDs := make([]string, 0, len(status.SetupTasks))
	for _, task := range status.SetupTasks {
		gotSetupTaskIDs = append(gotSetupTaskIDs, task.ID)
	}
	wantSetupTaskIDs := []string{"admin_token", "datasource", "paired_device", "search_model", "remote_registration"}
	if !reflect.DeepEqual(gotSetupTaskIDs, wantSetupTaskIDs) {
		t.Fatalf("SetupTasks IDs = %v, want %v", gotSetupTaskIDs, wantSetupTaskIDs)
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

func TestSetupTasksIncludeSearchModelStatus(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{{
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
	}})
	searchTask := setupTaskByID(runtime.StatusResponse().SetupTasks, "search_model")
	if searchTask == nil || searchTask.Status != "pending" || !strings.Contains(searchTask.Summary, "Install") {
		t.Fatalf("search model setup task = %+v, want pending install guidance", searchTask)
	}

	pack := runtimeSemanticPackForTest("setup-search-model")
	installRuntimeSemanticPackForTest(t, runtime, pack)
	searchTask = setupTaskByID(runtime.StatusResponse().SetupTasks, "search_model")
	if searchTask == nil || searchTask.Status != "complete" || !strings.Contains(searchTask.Summary, "Indexing runs in the background") {
		t.Fatalf("search model setup task = %+v, want completed setup with background indexing guidance", searchTask)
	}

	pausedWorkers := 0
	runtime.mu.Lock()
	runtime.config.WorkerRuntime.HeavyTaskWorkers = &pausedWorkers
	runtime.mu.Unlock()
	searchTask = setupTaskByID(runtime.StatusResponse().SetupTasks, "search_model")
	if searchTask == nil || searchTask.Status != "complete" || !strings.Contains(searchTask.Summary, "Indexing is paused") {
		t.Fatalf("search model setup task = %+v, want completed setup with paused indexing guidance", searchTask)
	}

	runtime.semanticONNXRuntime.mu.Lock()
	runtime.semanticONNXRuntime.cfg.Disabled = true
	runtime.semanticONNXRuntime.mu.Unlock()
	searchTask = setupTaskByID(runtime.StatusResponse().SetupTasks, "search_model")
	if searchTask == nil || searchTask.Status != "pending" || !strings.Contains(searchTask.Summary, "runtime is unavailable") {
		t.Fatalf("search model setup task = %+v, want pending runtime guidance", searchTask)
	}
	runtime.semanticONNXRuntime.mu.Lock()
	runtime.semanticONNXRuntime.cfg.Disabled = false
	runtime.semanticONNXRuntime.mu.Unlock()

	if _, err := runtime.semanticModels.ActivatePack(pack.ID, pack.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack() error = %v", err)
	}

	searchTask = setupTaskByID(runtime.StatusResponse().SetupTasks, "search_model")
	if searchTask == nil || searchTask.Status != "complete" {
		t.Fatalf("search model setup task = %+v, want complete", searchTask)
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

func TestRemoteRegistrationReadyAllowsLocalDatasourceWithoutURL(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{
		{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
		},
	}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
	})

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

	var datasourceTask *SetupTask
	status := runtime.StatusResponse()
	for index := range status.SetupTasks {
		if status.SetupTasks[index].ID == "datasource" {
			datasourceTask = &status.SetupTasks[index]
			break
		}
	}
	if datasourceTask == nil || datasourceTask.Status != "complete" {
		t.Fatalf("datasource setup task = %+v, want complete", datasourceTask)
	}
}

func setupTaskByID(tasks []SetupTask, id string) *SetupTask {
	for index := range tasks {
		if tasks[index].ID == id {
			return &tasks[index]
		}
	}
	return nil
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
	secondID, err := encodeTimichAssetID(runtime.assetIDKey, sourceKey, upstreamAssetID)
	if err != nil {
		t.Fatalf("encodeTimichAssetID() error = %v", err)
	}
	if secondID != page.Items[0].ID {
		t.Fatalf("stable asset ID changed: first=%q second=%q", page.Items[0].ID, secondID)
	}
	rawID, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(page.Items[0].ID, timichAssetIDPrefix))
	if err != nil {
		t.Fatalf("decode opaque asset ID bytes: %v", err)
	}
	if bytes.Contains(rawID, []byte(sourceKey)) || bytes.Contains(rawID, []byte(upstreamAssetID)) {
		t.Fatalf("opaque asset ID bytes expose datasource or upstream identity: %x", rawID)
	}
	tamperedSuffix := "A"
	if strings.HasSuffix(page.Items[0].ID, tamperedSuffix) {
		tamperedSuffix = "B"
	}
	tampered := page.Items[0].ID[:len(page.Items[0].ID)-1] + tamperedSuffix
	if _, _, err := decodeTimichAssetID(runtime.assetIDKey, tampered); err == nil {
		t.Fatal("decodeTimichAssetID(tampered) error = nil")
	}
}

func TestSignAssetSearchPageUsesItemSourceKeys(t *testing.T) {
	t.Parallel()

	assetIDKey := assetIDKeys{current: bytes.Repeat([]byte{7}, 32)}
	primarySource := "1111111111111111"
	secondarySource := "2222222222222222"
	page, err := signAssetSearchPage(catalog.AssetSearchPage{
		Items: []catalog.Asset{
			{ID: "primary-asset"},
			{SourceKey: secondarySource, ID: "secondary-asset"},
		},
	}, assetIDKey, primarySource)
	if err != nil {
		t.Fatalf("signAssetSearchPage() error = %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("Items length = %d, want 2", len(page.Items))
	}

	sourceKey, upstreamAssetID, err := decodeTimichAssetID(assetIDKey, page.Items[0].ID)
	if err != nil {
		t.Fatalf("decode first asset ID: %v", err)
	}
	if sourceKey != primarySource || upstreamAssetID != "primary-asset" {
		t.Fatalf("first decoded source=%q upstream=%q, want primary source", sourceKey, upstreamAssetID)
	}
	if page.Items[0].SourceKey != "" {
		t.Fatalf("first SourceKey = %q, want hidden after signing", page.Items[0].SourceKey)
	}

	sourceKey, upstreamAssetID, err = decodeTimichAssetID(assetIDKey, page.Items[1].ID)
	if err != nil {
		t.Fatalf("decode second asset ID: %v", err)
	}
	if sourceKey != secondarySource || upstreamAssetID != "secondary-asset" {
		t.Fatalf("second decoded source=%q upstream=%q, want item source", sourceKey, upstreamAssetID)
	}
	if page.Items[1].SourceKey != "" {
		t.Fatalf("second SourceKey = %q, want hidden after signing", page.Items[1].SourceKey)
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
			Kind:        "immich_indexed",
			URL:         "http://old-immich.local:2283",
			AccessToken: "existing-token",
		},
	})
	catalogService := runtime.catalogService()

	updated, err := runtime.UpdatePrimaryDatasource(config.DatasourceConfig{
		Name: "Home Immich",
		Kind: "immich_indexed",
		URL:  "http://immich.local:2283",
	})
	if err != nil {
		t.Fatalf("UpdatePrimaryDatasource() error = %v", err)
	}
	if !updated.Configured || updated.Name != "Home Immich" || updated.URL != "http://immich.local:2283" || !updated.HasAccessToken {
		t.Fatalf("updated datasource = %+v", updated)
	}
	if got := runtime.catalogService(); got != catalogService {
		t.Fatalf("catalog service changed from %p to %p, want stable service", catalogService, got)
	}

	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Datasources[0].AccessToken != "existing-token" {
		t.Fatalf("AccessToken = %q, want preserved token", loaded.Datasources[0].AccessToken)
	}
}

func TestUpdatePrimaryDatasourceCopiesInheritedIndexingBeforePersistence(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{{
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.local:2283",
		AccessToken: "existing-token",
	}})
	originalIndexing := &config.DatasourceIndexingConfig{
		Phase0SyncInterval:   " 30m ",
		DailyFullSweepWindow: " 03:00 ",
	}
	runtime.mu.Lock()
	runtime.config.Datasources[0].Indexing = originalIndexing
	runtime.mu.Unlock()

	if _, err := runtime.UpdatePrimaryDatasource(config.DatasourceConfig{
		Name: "Home Immich",
		Kind: config.DatasourceKindImmichIndexed,
		URL:  "http://immich.local:2283",
	}); err != nil {
		t.Fatalf("UpdatePrimaryDatasource() error = %v", err)
	}

	if originalIndexing.Phase0SyncInterval != " 30m " || originalIndexing.DailyFullSweepWindow != " 03:00 " {
		t.Fatalf("inherited live indexing was mutated during persistence: %+v", originalIndexing)
	}
	runtime.mu.RLock()
	updatedIndexing := runtime.config.Datasources[0].Indexing
	runtime.mu.RUnlock()
	if updatedIndexing == originalIndexing {
		t.Fatal("updated indexing reuses the previous live config pointer")
	}
	if updatedIndexing == nil || updatedIndexing.Phase0SyncInterval != "30m" || updatedIndexing.DailyFullSweepWindow != "03:00" {
		t.Fatalf("updated indexing = %+v, want normalized detached copy", updatedIndexing)
	}
}

func TestUpdatePrimaryDatasourceSwitchesIndexedImmichToPassthrough(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.local:2283",
		AccessToken: "existing-token",
		Indexing:    &config.DatasourceIndexingConfig{LatestAssetLimit: 600},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		if err := config.WriteFile(cfg.ConfigPath, cfg.Config); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	})

	updated, err := runtime.UpdatePrimaryDatasource(config.DatasourceConfig{
		Name: "Home Immich",
		Kind: config.DatasourceKindImmich,
		URL:  "http://immich.local:2283",
	})
	if err != nil {
		t.Fatalf("UpdatePrimaryDatasource() error = %v", err)
	}
	if updated.Kind != config.DatasourceKindImmich || !updated.HasAccessToken {
		t.Fatalf("updated datasource = %+v, want passthrough with preserved API key", updated)
	}
	if summaries := runtime.StatusResponse().Datasources; len(summaries) != 1 || summaries[0].IndexingEnabled {
		t.Fatalf("datasource summaries = %+v, want passthrough without indexing", summaries)
	}

	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Datasources[0].Kind != config.DatasourceKindImmich || loaded.Datasources[0].Indexing != nil {
		t.Fatalf("persisted datasource = %+v, want passthrough without indexing tuning", loaded.Datasources[0])
	}
}

func TestUpdatePrimaryDatasourceAllowsStaticDemoWithoutAccessToken(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name:        "Old",
			Kind:        "immich_indexed",
			URL:         "http://old-immich.local:2283",
			AccessToken: "existing-token",
		},
	})
	catalogService := runtime.catalogService()

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
	if got := runtime.catalogService(); got != catalogService {
		t.Fatalf("catalog service changed from %p to %p, want stable service", catalogService, got)
	}
	page, err := runtime.CatalogPage(0, 1)
	if err != nil {
		t.Fatalf("CatalogPage() after static demo reconfigure error = %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("CatalogPage() items = %d, want 1", len(page.Items))
	}

	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Datasources[0].AccessToken != "" {
		t.Fatalf("AccessToken = %q, want empty token for static demo", loaded.Datasources[0].AccessToken)
	}
}

func TestAddDatasourcePersistsImmich(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)

	added, err := runtime.AddDatasource(config.DatasourceConfig{
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
	})
	if err != nil {
		t.Fatalf("AddDatasource() error = %v", err)
	}
	if added.SourceKey == "" || added.Name != "Home Immich" || added.Kind != config.DatasourceKindImmichIndexed || added.URL != "http://immich.local:2283" || !added.HasAccessToken || !added.IndexingEnabled {
		t.Fatalf("added datasource = %+v", added)
	}
	status := runtime.StatusResponse()
	if len(status.Datasources) != 1 || status.Datasources[0].SourceKey != added.SourceKey {
		t.Fatalf("StatusResponse Datasources = %+v, want added datasource", status.Datasources)
	}

	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded.Datasources) != 1 || loaded.Datasources[0].AccessToken != "immich-api-key" {
		t.Fatalf("persisted datasources = %+v", loaded.Datasources)
	}
}

func TestAddDatasourceDefaultsImmichToPassthrough(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	added, err := runtime.AddDatasource(config.DatasourceConfig{
		Name:        "Home Immich",
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
	})
	if err != nil {
		t.Fatalf("AddDatasource() error = %v", err)
	}
	if added.Kind != config.DatasourceKindImmich || added.IndexingEnabled {
		t.Fatalf("added datasource = %+v, want default Immich passthrough", added)
	}
}

func TestImmichPassthroughRelaysSearchWithoutIndexing(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "immich-api-key" {
			t.Fatalf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/search/metadata":
			_, _ = io.WriteString(w, `{"assets":{"total":1,"items":[{"id":"asset-123","type":"IMAGE","originalFileName":"photo.jpg","fileCreatedAt":"2026-04-07T09:57:15.053Z"}]}}`)
		case "/api/search/statistics":
			_, _ = io.WriteString(w, `{"images":1,"videos":0}`)
		default:
			t.Fatalf("unexpected Immich path %q", r.URL.Path)
		}
	}))
	defer upstream.Close()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{{
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmich,
		URL:         upstream.URL,
		AccessToken: "immich-api-key",
	}})

	page, err := runtime.CatalogPage(0, 60)
	if err != nil {
		t.Fatalf("CatalogPage() error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Filename != "photo.jpg" {
		t.Fatalf("CatalogPage() = %#v, want direct Immich asset", page)
	}
	_, assetIDKey, sourceKey := runtime.catalogSnapshot()
	decodedSourceKey, upstreamAssetID, err := decodeTimichAssetID(assetIDKey, page.Items[0].ID)
	if err != nil {
		t.Fatalf("decodeTimichAssetID() error = %v", err)
	}
	if decodedSourceKey != sourceKey || upstreamAssetID != "asset-123" {
		t.Fatalf("decoded asset = %q/%q, want %q/asset-123", decodedSourceKey, upstreamAssetID, sourceKey)
	}

	status, err := runtime.DatasourceIndexingStatus(context.Background())
	if err != nil {
		t.Fatalf("DatasourceIndexingStatus() error = %v", err)
	}
	if len(status.Datasources) != 1 || status.Datasources[0].Kind != config.DatasourceKindImmich || status.Datasources[0].IndexingEnabled || status.Datasources[0].Status != "passthrough" {
		t.Fatalf("DatasourceIndexingStatus() = %+v, want passthrough without indexing", status.Datasources)
	}
	if len(status.Tasks) != 0 {
		t.Fatalf("DatasourceIndexingStatus() tasks = %+v, want none for passthrough", status.Tasks)
	}
	if task := setupTaskByID(runtime.StatusResponse().SetupTasks, "search_model"); task != nil {
		t.Fatalf("search model setup task = %+v, want passthrough to use Immich search", task)
	}
	if _, err := runtime.SyncPrimaryDatasourceMirror(context.Background(), catalog.MirrorSyncModeFull); !errors.Is(err, catalog.ErrCatalogNotConfigured) {
		t.Fatalf("SyncPrimaryDatasourceMirror() error = %v, want ErrCatalogNotConfigured", err)
	}
}

func TestAddDatasourceRejectsAdditionalSourceWhenImmichPassthroughExists(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{{
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmich,
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
	}})

	if _, err := runtime.AddDatasource(config.DatasourceConfig{
		Name:        "Other Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://other-immich.local:2283",
		AccessToken: "other-key",
	}); !errors.Is(err, config.ErrImmichPassthroughRequiresSingleDatasource) {
		t.Fatalf("AddDatasource() error = %v, want passthrough topology error", err)
	}
	if got := runtime.StatusResponse().Datasources; len(got) != 1 || got[0].Kind != config.DatasourceKindImmich {
		t.Fatalf("live datasources = %+v, want unchanged passthrough datasource", got)
	}
}

func TestAddDatasourceRejectsDuplicateTarget(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			Name:        "Home Immich",
			Kind:        config.DatasourceKindImmichIndexed,
			URL:         "http://immich.local:2283",
			AccessToken: "existing-token",
		},
	})

	if _, err := runtime.AddDatasource(config.DatasourceConfig{
		Name:        "Duplicate Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.local:2283",
		AccessToken: "new-token",
	}); !errors.Is(err, ErrDatasourceAlreadyConfigured) {
		t.Fatalf("AddDatasource() error = %v, want ErrDatasourceAlreadyConfigured", err)
	}
	status := runtime.StatusResponse()
	if len(status.Datasources) != 1 {
		t.Fatalf("Datasources length = %d, want 1", len(status.Datasources))
	}
}

func TestAddDatasourcePersistsLocalFilesystem(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		if err := config.WriteFile(cfg.ConfigPath, cfg.Config); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	})
	catalogService := runtime.catalogService()

	added, err := runtime.AddDatasource(config.DatasourceConfig{
		Name:    "NAS Photos",
		Kind:    config.DatasourceKindLocalFiles,
		RootKey: "nas-photos",
	})
	if err != nil {
		t.Fatalf("AddDatasource() error = %v", err)
	}
	if added.SourceKey == "" || added.Name != "NAS Photos" || added.Kind != config.DatasourceKindLocalFiles || added.RootKey != "nas-photos" || added.RootPath != rootPath || !added.ImmichFallbackEnabled {
		t.Fatalf("added datasource = %+v", added)
	}
	if got := runtime.catalogService(); got != catalogService {
		t.Fatalf("catalog service after add changed from %p to %p, want stable service", catalogService, got)
	}
	updated, err := runtime.UpdateLocalDatasourceImmichFallback(added.SourceKey, false)
	if err != nil {
		t.Fatalf("UpdateLocalDatasourceImmichFallback() error = %v", err)
	}
	if updated.SourceKey != added.SourceKey || updated.ImmichFallbackEnabled {
		t.Fatalf("updated datasource = %+v, want fallback disabled", updated)
	}
	if got := runtime.catalogService(); got != catalogService {
		t.Fatalf("catalog service after fallback changed from %p to %p, want stable service", catalogService, got)
	}
	if _, err := catalogService.SearchAssets(catalog.AssetSearchRequest{
		Collection: catalog.AssetCollectionRequest{Kind: catalog.CollectionKindTimeline},
		Page:       catalog.AssetSearchPageRequest{Index: 0, Size: 10},
	}); err != nil {
		t.Fatalf("SearchAssets() through pre-update catalog error = %v", err)
	}

	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(loaded.LocalMediaRoots) != 1 || loaded.LocalMediaRoots[0].Key != "nas-photos" {
		t.Fatalf("persisted local roots = %+v", loaded.LocalMediaRoots)
	}
	if len(loaded.Datasources) != 1 || loaded.Datasources[0].Kind != config.DatasourceKindLocalFiles || loaded.Datasources[0].RootKey != "nas-photos" {
		t.Fatalf("persisted datasources = %+v", loaded.Datasources)
	}
	if config.LocalDatasourceImmichFallbackEnabled(loaded.Datasources[0]) {
		t.Fatalf("persisted datasource fallback = enabled, want disabled: %+v", loaded.Datasources[0])
	}
}

func TestConcurrentConfigMutationsPreserveAllUpdates(t *testing.T) {
	t.Parallel()

	firstRootPath := t.TempDir()
	secondRootPath := t.TempDir()
	datasources := []config.DatasourceConfig{
		{
			SourceKey: "1111111111111111",
			Name:      "First local source",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "first-local-root",
		},
		{
			SourceKey: "2222222222222222",
			Name:      "Second local source",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "second-local-root",
		},
	}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, datasources, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{
			{Key: "first-local-root", Path: firstRootPath},
			{Key: "second-local-root", Path: secondRootPath},
		}
		if err := config.WriteFile(cfg.ConfigPath, cfg.Config); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	})
	catalogService := runtime.catalogService()
	searchRequest := catalog.AssetSearchRequest{
		Collection: catalog.AssetCollectionRequest{Kind: catalog.CollectionKindTimeline},
		Page:       catalog.AssetSearchPageRequest{Index: 0, Size: 10},
	}

	type mutationResult struct {
		name string
		err  error
	}
	start := make(chan struct{})
	results := make(chan mutationResult, 3)
	searchDone := make(chan error, 1)
	go func() {
		<-start
		for range 64 {
			if _, err := catalogService.SearchAssets(searchRequest); err != nil {
				searchDone <- err
				return
			}
		}
		searchDone <- nil
	}()
	go func() {
		<-start
		_, err := runtime.UpdateLocalDatasourceImmichFallback("1111111111111111", false)
		results <- mutationResult{name: "first fallback", err: err}
	}()
	go func() {
		<-start
		_, err := runtime.UpdateLocalDatasourceImmichFallback("2222222222222222", false)
		results <- mutationResult{name: "second fallback", err: err}
	}()
	go func() {
		<-start
		_, err := runtime.UpdateWorkerRuntime(config.WorkerRuntimeConfig{HeavyTaskWorkers: runtimeTestIntPtr(0)})
		results <- mutationResult{name: "worker runtime", err: err}
	}()
	close(start)

	for range 3 {
		result := <-results
		if result.err != nil {
			t.Fatalf("%s update error = %v", result.name, result.err)
		}
	}
	if err := <-searchDone; err != nil {
		t.Fatalf("concurrent SearchAssets() error = %v", err)
	}
	if got := runtime.catalogService(); got != catalogService {
		t.Fatalf("catalog service changed from %p to %p, want stable service", catalogService, got)
	}
	if _, err := catalogService.SearchAssets(searchRequest); err != nil {
		t.Fatalf("SearchAssets() through retained catalog error = %v", err)
	}

	fallbackBySourceKey := map[string]bool{}
	for _, datasource := range runtime.Datasources() {
		fallbackBySourceKey[datasource.SourceKey] = datasource.ImmichFallbackEnabled
	}
	for _, sourceKey := range []string{"1111111111111111", "2222222222222222"} {
		fallbackEnabled, ok := fallbackBySourceKey[sourceKey]
		if !ok || fallbackEnabled {
			t.Fatalf("runtime fallback for %s = %t, present = %t; want disabled and present", sourceKey, fallbackEnabled, ok)
		}
	}
	workerStatus := runtime.WorkerRuntimeStatus()
	if workerStatus.ConfiguredHeavyTaskWorkers == nil || *workerStatus.ConfiguredHeavyTaskWorkers != 0 {
		t.Fatalf("runtime configured workers = %v, want 0", workerStatus.ConfiguredHeavyTaskWorkers)
	}

	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	persistedFallbackBySourceKey := map[string]bool{}
	for _, datasource := range loaded.Datasources {
		if datasource.Kind == config.DatasourceKindLocalFiles {
			persistedFallbackBySourceKey[datasource.SourceKey] = config.LocalDatasourceImmichFallbackEnabled(datasource)
		}
	}
	for _, sourceKey := range []string{"1111111111111111", "2222222222222222"} {
		fallbackEnabled, ok := persistedFallbackBySourceKey[sourceKey]
		if !ok || fallbackEnabled {
			t.Fatalf("persisted fallback for %s = %t, present = %t; want disabled and present", sourceKey, fallbackEnabled, ok)
		}
	}
	if loaded.WorkerRuntime.HeavyTaskWorkers == nil || *loaded.WorkerRuntime.HeavyTaskWorkers != 0 {
		t.Fatalf("persisted configured workers = %v, want 0", loaded.WorkerRuntime.HeavyTaskWorkers)
	}
}

func TestAddDatasourceDoesNotReplaceRuntimeOverrides(t *testing.T) {
	t.Setenv("TIMICH_AGENT_NAME", "env-agent")

	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "agent.json")
	cfg := config.Default()
	cfg.AdminListenAddress = "127.0.0.1:8081"
	cfg.DataDir = "state"
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
	runtime, err := NewAgentRuntime(BuildInfo{}, resolved, store.LoadedState{
		Path: filepath.Join(dataDir, "agent-state.json"),
		State: store.State{
			AgentID:           "agent-test",
			CreatedAt:         time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			SessionSigningKey: signingKey,
			AdminToken:        "test-admin-token",
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

	if _, err := runtime.AddDatasource(config.DatasourceConfig{
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
	}); err != nil {
		t.Fatalf("AddDatasource() error = %v", err)
	}

	response := runtime.ConfigResponse()
	if response.AgentName != "env-agent" || response.AdminListenAddress != "0.0.0.0:18081" || response.DataDir != filepath.Join(dataDir, "runtime-state") {
		t.Fatalf("runtime config = %+v, want runtime overrides preserved", response)
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
			Kind:        "immich_indexed",
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
		Kind: "immich_indexed",
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
			Kind:        "immich_indexed",
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
	if response.Datasources[0].Name != "Old" || response.Datasources[0].Kind != config.DatasourceKindImmichIndexed || response.Datasources[0].URL != "http://old-immich.local:2283" {
		t.Fatalf("live datasource mutated after failed update: %+v", response.Datasources[0])
	}
}

func TestStartDatasourceMirrorSyncRunsStartupFullSync(t *testing.T) {
	t.Parallel()

	requests := make(chan struct{}, 1)
	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search/metadata" {
			t.Fatalf("unexpected datasource path %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "immich-api-key" {
			t.Fatalf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
		}
		select {
		case requests <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"assets": {
				"total": 1,
				"items": [
					{
						"id": "asset-startup",
						"type": "IMAGE",
						"originalFileName": "startup.jpg",
						"fileCreatedAt": "2026-06-01T10:00:00Z",
						"updatedAt": "2026-06-01T10:05:00Z"
					}
				],
				"nextPage": null
			}
		}`)
	}))
	defer datasourceServer.Close()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        "immich_indexed",
		URL:         datasourceServer.URL,
		AccessToken: "immich-api-key",
	}})
	runtime.StartDatasourceMirrorSync()

	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("mirror startup sync did not call datasource")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		status, err := runtime.PrimaryDatasourceMirrorStatus(ctx)
		cancel()
		if err != nil {
			t.Fatalf("PrimaryDatasourceMirrorStatus() error = %v", err)
		}
		if status.ActiveCount == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ActiveCount = %d, want 1", status.ActiveCount)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDatasourceMirrorSyncTimeoutForMode(t *testing.T) {
	t.Parallel()

	if got := DatasourceMirrorSyncTimeoutForMode(""); got != datasourceMirrorFullSyncTimeout {
		t.Fatalf("empty mode timeout = %s, want %s", got, datasourceMirrorFullSyncTimeout)
	}
	if got := DatasourceMirrorSyncTimeoutForMode(catalog.MirrorSyncModeFull); got != datasourceMirrorFullSyncTimeout {
		t.Fatalf("full mode timeout = %s, want %s", got, datasourceMirrorFullSyncTimeout)
	}
	if got := DatasourceMirrorSyncTimeoutForMode(catalog.MirrorSyncModeIncremental); got != datasourceMirrorIncrementalSyncTimeout {
		t.Fatalf("incremental mode timeout = %s, want %s", got, datasourceMirrorIncrementalSyncTimeout)
	}
	if datasourceMirrorFullSyncTimeout <= datasourceMirrorIncrementalSyncTimeout {
		t.Fatalf("full sync timeout = %s, want greater than incremental timeout %s", datasourceMirrorFullSyncTimeout, datasourceMirrorIncrementalSyncTimeout)
	}
}

func TestScheduledDatasourceMirrorIntervalRunsIncrementalSync(t *testing.T) {
	t.Parallel()

	requestBodies := []map[string]any{}
	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search/metadata" {
			t.Fatalf("unexpected datasource path %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requestBodies = append(requestBodies, body)
		w.Header().Set("Content-Type", "application/json")
		if len(requestBodies) == 1 {
			_, _ = io.WriteString(w, `{
				"assets": {
					"total": 1,
					"items": [
						{
							"id": "asset-start",
							"type": "IMAGE",
							"originalFileName": "start.jpg",
							"fileCreatedAt": "2026-06-01T10:00:00Z",
							"updatedAt": "2026-06-01T10:05:00Z"
						}
					],
					"nextPage": null
				}
			}`)
			return
		}
		_, _ = io.WriteString(w, `{
			"assets": {
				"total": 1,
				"items": [
					{
						"id": "asset-interval",
						"type": "IMAGE",
						"originalFileName": "interval.jpg",
						"fileCreatedAt": "2026-06-02T10:00:00Z",
						"updatedAt": "2026-06-02T10:05:00Z"
					}
				],
				"nextPage": null
			}
		}`)
	}))
	defer datasourceServer.Close()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        "immich_indexed",
		URL:         datasourceServer.URL,
		AccessToken: "immich-api-key",
		Indexing: &config.DatasourceIndexingConfig{
			Phase0SyncInterval: "1h",
		},
	}})
	defer runtime.Close()

	if _, err := runtime.SyncPrimaryDatasourceMirror(context.Background(), catalog.MirrorSyncModeFull); err != nil {
		t.Fatalf("full SyncPrimaryDatasourceMirror() error = %v", err)
	}
	runtime.runScheduledDatasourceMirrorSync(context.Background(), "interval")

	if len(requestBodies) != 2 {
		t.Fatalf("request count = %d, want 2", len(requestBodies))
	}
	if _, ok := requestBodies[0]["updatedAfter"]; ok {
		t.Fatalf("full request unexpectedly had updatedAfter: %#v", requestBodies[0])
	}
	if got := requestBodies[1]["updatedAfter"]; got != "2026-06-01T10:05:00Z" {
		t.Fatalf("interval updatedAfter = %#v, want incremental cursor", got)
	}
	status, err := runtime.PrimaryDatasourceMirrorStatus(context.Background())
	if err != nil {
		t.Fatalf("PrimaryDatasourceMirrorStatus() error = %v", err)
	}
	if status.ActiveCount != 2 || status.LastIncrementalSyncAt == nil {
		t.Fatalf("mirror status after interval = %#v", status)
	}
}

func TestScheduledDatasourceMirrorStartupUsesIncrementalAfterExistingFullSync(t *testing.T) {
	t.Parallel()

	requestBodies := []map[string]any{}
	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search/metadata" {
			t.Fatalf("unexpected datasource path %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requestBodies = append(requestBodies, body)
		w.Header().Set("Content-Type", "application/json")
		if len(requestBodies) == 1 {
			_, _ = io.WriteString(w, `{
				"assets": {
					"total": 1,
					"items": [
						{
							"id": "asset-start",
							"type": "IMAGE",
							"originalFileName": "start.jpg",
							"fileCreatedAt": "2026-06-01T10:00:00Z",
							"updatedAt": "2026-06-01T10:05:00Z"
						}
					],
					"nextPage": null
				}
			}`)
			return
		}
		_, _ = io.WriteString(w, `{
			"assets": {
				"total": 1,
				"items": [
					{
						"id": "asset-startup-incremental",
						"type": "IMAGE",
						"originalFileName": "startup-incremental.jpg",
						"fileCreatedAt": "2026-06-02T10:00:00Z",
						"updatedAt": "2026-06-02T10:05:00Z"
					}
				],
				"nextPage": null
			}
		}`)
	}))
	defer datasourceServer.Close()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        "immich_indexed",
		URL:         datasourceServer.URL,
		AccessToken: "immich-api-key",
		Indexing: &config.DatasourceIndexingConfig{
			Phase0SyncInterval: "1h",
		},
	}})
	defer runtime.Close()

	if _, err := runtime.SyncPrimaryDatasourceMirror(context.Background(), catalog.MirrorSyncModeFull); err != nil {
		t.Fatalf("full SyncPrimaryDatasourceMirror() error = %v", err)
	}
	runtime.schedulerWorkStateMu.Lock()
	runtime.schedulerWorkState = schedulerWorkState{
		ConfigHash: "cached-before-mirror",
		UpdatedAt:  time.Now().UTC(),
	}
	runtime.schedulerWorkStateMu.Unlock()
	wake := make(chan struct{}, 1)
	runtime.semanticBackfillMu.Lock()
	runtime.backgroundWorkerWake = wake
	runtime.semanticBackfillMu.Unlock()
	runtime.runScheduledDatasourceMirrorSync(context.Background(), "startup")

	runtime.schedulerWorkStateMu.Lock()
	dirty := runtime.schedulerWorkState.Dirty
	runtime.schedulerWorkStateMu.Unlock()
	if !dirty {
		t.Fatal("scheduler work state remained clean after successful mirror sync")
	}
	select {
	case <-wake:
	default:
		t.Fatal("background worker scheduler was not woken after successful mirror sync")
	}

	if len(requestBodies) != 2 {
		t.Fatalf("request count = %d, want 2", len(requestBodies))
	}
	if _, ok := requestBodies[0]["updatedAfter"]; ok {
		t.Fatalf("full request unexpectedly had updatedAfter: %#v", requestBodies[0])
	}
	if got := requestBodies[1]["updatedAfter"]; got != "2026-06-01T10:05:00Z" {
		t.Fatalf("startup updatedAfter = %#v, want incremental cursor", got)
	}
}

func TestScheduledDatasourceMirrorModeIsSourceSpecific(t *testing.T) {
	t.Parallel()

	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search/metadata" {
			t.Fatalf("unexpected datasource path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"assets": {
				"total": 1,
				"items": [
					{
						"id": "asset-start",
						"type": "IMAGE",
						"originalFileName": "start.jpg",
						"fileCreatedAt": "2026-06-01T10:00:00Z",
						"updatedAt": "2026-06-01T10:05:00Z"
					}
				],
				"nextPage": null
			}
		}`)
	}))
	defer datasourceServer.Close()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			SourceKey:   "1111111111111111",
			Name:        "Home Immich",
			Kind:        "immich_indexed",
			URL:         datasourceServer.URL,
			AccessToken: "immich-api-key",
			Indexing: &config.DatasourceIndexingConfig{
				Phase0SyncInterval: "1h",
			},
		},
		{
			SourceKey:   "2222222222222222",
			Name:        "Archive Immich",
			Kind:        "immich_indexed",
			URL:         datasourceServer.URL,
			AccessToken: "immich-api-key",
			Indexing: &config.DatasourceIndexingConfig{
				Phase0SyncInterval: "1h",
			},
		},
	})
	defer runtime.Close()

	if _, err := runtime.SyncPrimaryDatasourceMirror(context.Background(), catalog.MirrorSyncModeFull); err != nil {
		t.Fatalf("full SyncPrimaryDatasourceMirror() error = %v", err)
	}
	if got := runtime.datasourceMirrorSyncModeForSource(context.Background(), "1111111111111111", "startup"); got != catalog.MirrorSyncModeIncremental {
		t.Fatalf("primary startup mode = %q, want incremental", got)
	}
	if got := runtime.datasourceMirrorSyncModeForSource(context.Background(), "2222222222222222", "startup"); got != catalog.MirrorSyncModeFull {
		t.Fatalf("secondary startup mode = %q, want full because it has no full sync", got)
	}
	if got := runtime.datasourceMirrorSyncModeForSource(context.Background(), "2222222222222222", "interval"); got != catalog.MirrorSyncModeFull {
		t.Fatalf("secondary interval mode = %q, want full because it has no full sync", got)
	}
	if got := runtime.datasourceMirrorSyncModeForSource(context.Background(), "1111111111111111", "daily_full_sweep"); got != catalog.MirrorSyncModeFull {
		t.Fatalf("daily mode = %q, want full", got)
	}
}

func TestScheduledDatasourceMirrorSyncsAllConfiguredMirrors(t *testing.T) {
	t.Parallel()

	sourceRequests := map[string]int{}
	newMirrorServer := func(sourceName string, assetID string, filename string, capturedAt string) *httptest.Server {
		t.Helper()
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/search/metadata" {
				t.Fatalf("%s unexpected datasource path %s", sourceName, r.URL.Path)
			}
			sourceRequests[sourceName]++
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"assets": {
					"total": 1,
					"items": [
						{
							"id": %q,
							"type": "IMAGE",
							"originalFileName": %q,
							"fileCreatedAt": %q,
							"updatedAt": %q
						}
					],
					"nextPage": null
				}
			}`, assetID, filename, capturedAt, capturedAt)
		}))
	}
	sourceA := newMirrorServer("source-a", "asset-a", "source-a.jpg", "2026-06-01T10:00:00Z")
	defer sourceA.Close()
	sourceB := newMirrorServer("source-b", "asset-b", "source-b.jpg", "2026-06-02T10:00:00Z")
	defer sourceB.Close()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{
		{
			SourceKey:   "1111111111111111",
			Name:        "Home Immich",
			Kind:        "immich_indexed",
			URL:         sourceA.URL,
			AccessToken: "immich-api-key",
			Indexing: &config.DatasourceIndexingConfig{
				Phase0SyncInterval: "1h",
			},
		},
		{
			SourceKey:   "2222222222222222",
			Name:        "Archive Immich",
			Kind:        "immich_indexed",
			URL:         sourceB.URL,
			AccessToken: "immich-api-key",
			Indexing: &config.DatasourceIndexingConfig{
				Phase0SyncInterval: "30m",
			},
		},
	})

	schedule, ok := runtime.datasourceMirrorSchedule()
	if !ok {
		t.Fatal("datasourceMirrorSchedule() ok = false, want enabled schedule")
	}
	if schedule.Interval != 30*time.Minute {
		t.Fatalf("schedule interval = %s, want shortest configured interval", schedule.Interval)
	}

	runtime.runScheduledDatasourceMirrorSync(context.Background(), "startup")
	if sourceRequests["source-a"] != 1 || sourceRequests["source-b"] != 1 {
		t.Fatalf("source requests = %#v, want one request per mirrored datasource", sourceRequests)
	}

	page, err := runtime.SearchAssets(catalog.AssetSearchRequest{
		Collection: catalog.AssetCollectionRequest{Kind: catalog.CollectionKindTimeline},
		Page:       catalog.AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() error = %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("timeline page total=%d items=%d, want 2", page.Total, len(page.Items))
	}
	if page.Items[0].Filename != "source-b.jpg" || page.Items[1].Filename != "source-a.jpg" {
		t.Fatalf("timeline items = %#v, want newest across mirrored datasources", page.Items)
	}
}

func TestLocalDatasourceScanScheduleUsesShortestConfiguredInterval(t *testing.T) {
	t.Parallel()

	rootA := t.TempDir()
	rootB := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{
		{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
			Scan: &config.LocalDatasourceScanConfig{
				QuickScanInterval: "20m",
			},
		},
		{
			SourceKey: "2222222222222222",
			Name:      "Archive Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "archive-photos",
			Scan: &config.LocalDatasourceScanConfig{
				QuickScanInterval: "5m",
			},
		},
	}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{
			{Key: "nas-photos", Path: rootA},
			{Key: "archive-photos", Path: rootB},
		}
	})

	schedule, ok := runtime.localDatasourceScanScheduleAt(time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("localDatasourceScanSchedule() ok = false, want enabled schedule")
	}
	if schedule.Interval != 5*time.Minute {
		t.Fatalf("schedule interval = %s, want shortest configured interval", schedule.Interval)
	}
}

func TestLocalDatasourceScanScheduleWakesAtDailyReconciliationTime(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			QuickScanInterval:  "30m",
			ReconciliationTime: "04:00",
		},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.Timezone = "Asia/Tokyo"
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
	})

	// 03:55 in the configured Agent timezone must wake at 04:00 rather than
	// waiting for the longer quick-discovery interval.
	now := time.Date(2026, 7, 28, 18, 55, 0, 0, time.UTC)
	schedule, ok := runtime.localDatasourceScanScheduleAt(now)
	if !ok {
		t.Fatal("localDatasourceScanScheduleAt() ok = false, want enabled schedule")
	}
	if schedule.Interval != 5*time.Minute {
		t.Fatalf("schedule interval = %s, want 5m until 04:00 Asia/Tokyo reconciliation", schedule.Interval)
	}
}

func TestLocalDailyScheduleUsesAgentTimezone(t *testing.T) {
	t.Parallel()

	location, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("LoadLocation(Asia/Tokyo) error = %v", err)
	}
	now := time.Date(2026, 7, 28, 19, 30, 0, 0, time.UTC) // 04:30 JST
	latest := latestLocalDailySchedule(now, location, "04:00")
	if want := time.Date(2026, 7, 28, 19, 0, 0, 0, time.UTC); !latest.Equal(want) {
		t.Fatalf("latest schedule = %s, want %s", latest, want)
	}
	next := nextLocalDailySchedule(now, location, "04:00")
	if want := time.Date(2026, 7, 29, 19, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next schedule = %s, want %s", next, want)
	}
}

func TestLocalContentVerificationScheduleAdmitsOnlyWithIdleWorker(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name       string
		active     int
		wantStatus string
		wantReason string
	}{
		{
			name:       "idle worker starts daily window",
			wantStatus: catalog.LocalContentVerificationStatusRunning,
		},
		{
			name:       "busy worker skips daily window",
			active:     1,
			wantStatus: catalog.LocalContentVerificationStatusSkipped,
			wantReason: catalog.LocalContentVerificationSkipNoIdleWorker,
		},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			rootPath := t.TempDir()
			workers := 1
			runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
				SourceKey: "1111111111111111",
				Name:      "NAS Photos",
				Kind:      config.DatasourceKindLocalFiles,
				RootKey:   "nas-photos",
				Scan: &config.LocalDatasourceScanConfig{
					ContentVerificationTime:     "04:00",
					ContentVerificationDuration: "30m",
				},
			}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
				cfg.Timezone = "UTC"
				cfg.WorkerRuntime.HeavyTaskWorkers = &workers
				cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
					Key:  "nas-photos",
					Path: rootPath,
				}}
			})
			catalogService := runtime.catalogService()
			if _, err := catalogService.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
				t.Fatalf("RunLocalReconciliationScan() error = %v", err)
			}
			if testCase.active > 0 {
				runtime.backgroundWorkerMu.Lock()
				if runtime.backgroundWorkerActive == nil {
					runtime.backgroundWorkerActive = make(map[string]int)
				}
				runtime.backgroundWorkerActive["metadata"] = testCase.active
				runtime.backgroundWorkerMu.Unlock()
				defer func() {
					runtime.backgroundWorkerMu.Lock()
					delete(runtime.backgroundWorkerActive, "metadata")
					runtime.backgroundWorkerMu.Unlock()
				}()
			}

			runtime.reconcileLocalContentVerificationSchedule(
				context.Background(),
				time.Date(2026, 7, 30, 4, 0, 10, 0, time.UTC),
			)
			statuses, err := catalogService.LocalDatasourceScanStatuses(context.Background())
			if err != nil {
				t.Fatalf("LocalDatasourceScanStatuses() error = %v", err)
			}
			if len(statuses) != 1 ||
				statuses[0].ContentVerificationStatus != testCase.wantStatus ||
				statuses[0].ContentVerificationSkipReason != testCase.wantReason {
				t.Fatalf("content verification status = %+v, want status=%q reason=%q", statuses, testCase.wantStatus, testCase.wantReason)
			}
		})
	}
}

func TestLocalContentVerificationZeroDurationHasNoDailyEvent(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			ContentVerificationTime:     "04:00",
			ContentVerificationDuration: "0",
		},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.Timezone = "UTC"
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
	})
	schedules := runtime.localContentVerificationSchedules()
	if len(schedules) != 1 || schedules[0].Duration != 0 {
		t.Fatalf("content verification schedules = %+v, want explicit disabled schedule", schedules)
	}
	if next := runtime.nextLocalContentVerificationScheduleEvent(context.Background(), time.Now().UTC()); next != nil {
		t.Fatalf("next content verification event = %s, want nil for disabled duration", next)
	}
}

func TestLocalContentVerificationScheduleKeepsAdmissionGraceWake(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			ContentVerificationTime:     "04:00",
			ContentVerificationDuration: "30m",
		},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.Timezone = "UTC"
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
	})
	now := time.Date(2026, 7, 30, 4, 0, 10, 0, time.UTC)
	next := runtime.nextLocalContentVerificationScheduleEvent(context.Background(), now)
	want := time.Date(2026, 7, 30, 4, 1, 0, 0, time.UTC)
	if next == nil || !next.Equal(want) {
		t.Fatalf("next content verification event = %v, want admission retry boundary %s", next, want)
	}
}

func TestCompletedLocalContentVerificationWindowResetsDailySchedule(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), []byte("photo"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	workers := 1
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			ContentVerificationTime:     "04:00",
			ContentVerificationDuration: "48h",
			SettlingDuration:            "1ns",
		},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.Timezone = "UTC"
		cfg.WorkerRuntime.HeavyTaskWorkers = &workers
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
	})
	catalogService := runtime.catalogService()
	if _, err := catalogService.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := catalogService.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v, want one registered asset", result, err)
	}
	scheduledAt := time.Now().UTC().Add(-24 * time.Hour)
	started, err := catalogService.StartLocalContentVerificationWindow(
		context.Background(),
		"1111111111111111",
		scheduledAt,
		scheduledAt.Add(48*time.Hour),
	)
	if err != nil {
		t.Fatalf("StartLocalContentVerificationWindow() error = %v", err)
	}
	if !started {
		t.Fatal("StartLocalContentVerificationWindow() = false, want running window")
	}
	select {
	case <-runtime.localVerifyScheduleReset:
	default:
	}
	runtime.backgroundWorkerMu.Lock()
	if runtime.backgroundWorkerActive == nil {
		runtime.backgroundWorkerActive = make(map[string]int)
	}
	runtime.backgroundWorkerActive["content_verification"] = 1
	runtime.backgroundWorkerMu.Unlock()

	if processed := runtime.runLocalContentVerificationSource(
		context.Background(),
		catalogService,
		"1111111111111111",
		"test",
	); !processed {
		t.Fatal("runLocalContentVerificationSource() = false, want one verified file")
	}
	select {
	case <-runtime.localVerifyScheduleReset:
		t.Fatal("content verification schedule reset before worker assignment finished")
	default:
	}
	runtime.finishBackgroundWorkerAssignment("content_verification", 1)
	select {
	case <-runtime.localVerifyScheduleReset:
	case <-time.After(time.Second):
		t.Fatal("completed content verification window did not reset daily schedule")
	}
}

func TestLocalContentVerificationAssignmentProcessesOneLocationAndRotatesSources(t *testing.T) {
	t.Parallel()

	rootA := t.TempDir()
	rootB := t.TempDir()
	for _, path := range []string{
		filepath.Join(rootA, "a-1.jpg"),
		filepath.Join(rootA, "a-2.jpg"),
		filepath.Join(rootB, "b-1.jpg"),
		filepath.Join(rootB, "b-2.jpg"),
	} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, err)
		}
	}
	workers := 1
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{
		{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos A",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos-a",
			Scan: &config.LocalDatasourceScanConfig{
				ContentVerificationTime:     "04:00",
				ContentVerificationDuration: "30m",
				SettlingDuration:            "1ns",
			},
		},
		{
			SourceKey: "2222222222222222",
			Name:      "NAS Photos B",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos-b",
			Scan: &config.LocalDatasourceScanConfig{
				ContentVerificationTime:     "04:00",
				ContentVerificationDuration: "30m",
				SettlingDuration:            "1ns",
			},
		},
	}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.Timezone = "UTC"
		cfg.WorkerRuntime.HeavyTaskWorkers = &workers
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{
			{Key: "nas-photos-a", Path: rootA},
			{Key: "nas-photos-b", Path: rootB},
		}
	})
	catalogService := runtime.catalogService()
	for _, sourceKey := range []string{"1111111111111111", "2222222222222222"} {
		if _, err := catalogService.RunLocalReconciliationScan(context.Background(), sourceKey); err != nil {
			t.Fatalf("RunLocalReconciliationScan(%s) error = %v", sourceKey, err)
		}
	}
	if result, err := catalogService.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 4 {
		t.Fatalf("RunLocalMetadataBatch() result=%+v error=%v, want four registered assets", result, err)
	}
	scheduledAt := time.Now().UTC()
	for _, sourceKey := range []string{"1111111111111111", "2222222222222222"} {
		started, err := catalogService.StartLocalContentVerificationWindow(
			context.Background(),
			sourceKey,
			scheduledAt,
			scheduledAt.Add(30*time.Minute),
		)
		if err != nil {
			t.Fatalf("StartLocalContentVerificationWindow(%s) error = %v", sourceKey, err)
		}
		if !started {
			t.Fatalf("StartLocalContentVerificationWindow(%s) = false, want running window", sourceKey)
		}
	}

	processedBySource := func() map[string]int {
		t.Helper()
		statuses, err := catalogService.LocalDatasourceScanStatuses(context.Background())
		if err != nil {
			t.Fatalf("LocalDatasourceScanStatuses() error = %v", err)
		}
		result := make(map[string]int, len(statuses))
		for _, status := range statuses {
			result[status.SourceKey] = status.ContentVerificationProcessedFiles
		}
		return result
	}
	for iteration, want := range []map[string]int{
		{"1111111111111111": 1, "2222222222222222": 0},
		{"1111111111111111": 1, "2222222222222222": 1},
		{"1111111111111111": 2, "2222222222222222": 1},
	} {
		if !runtime.runScheduledLocalContentVerification(context.Background()) {
			t.Fatalf("runScheduledLocalContentVerification(iteration %d) = false, want one processed Location", iteration+1)
		}
		if got := processedBySource(); !reflect.DeepEqual(got, want) {
			t.Fatalf("processed files after iteration %d = %#v, want %#v", iteration+1, got, want)
		}
	}
}

func TestLocalContentVerificationSystemErrorDefersOnlyVerification(t *testing.T) {
	rootPath := t.TempDir()
	workers := 1
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan: &config.LocalDatasourceScanConfig{
			ContentVerificationTime:     "04:00",
			ContentVerificationDuration: "30m",
		},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.Timezone = "UTC"
		cfg.WorkerRuntime.HeavyTaskWorkers = &workers
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
	})
	catalogService := runtime.catalogService()
	if _, err := catalogService.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	scheduledAt := time.Now().UTC()
	started, err := catalogService.StartLocalContentVerificationWindow(
		context.Background(),
		"1111111111111111",
		scheduledAt,
		scheduledAt.Add(30*time.Minute),
	)
	if err != nil {
		t.Fatalf("StartLocalContentVerificationWindow() error = %v", err)
	}
	if !started {
		t.Fatal("StartLocalContentVerificationWindow() = false, want running window")
	}

	restoreStorageAvailableBytes := stubStorageAvailableBytes(t, 512*1024*1024, nil)
	if runtime.runScheduledLocalContentVerification(context.Background()) {
		restoreStorageAvailableBytes()
		t.Fatal("runScheduledLocalContentVerification() = true under storage guard, want deferred error")
	}
	restoreStorageAvailableBytes()
	retryAt := runtime.localBackgroundWorkerRetryNotBeforeAt("content_verification")
	if retryAt == nil || !retryAt.After(time.Now().UTC()) {
		t.Fatalf("content verification retry deadline = %v, want future retry", retryAt)
	}

	state := schedulerWorkState{ContentVerificationReady: true}
	if assignment, ok := runtime.nextBackgroundWorkerAssignment(
		context.Background(),
		1,
		false,
		semanticIndexingSchedule{},
		false,
		state,
		true,
	); ok {
		t.Fatalf("deferred assignment = %+v, want no content verification before retry deadline", assignment)
	}
	state.MetadataQueued = 1
	if assignment, ok := runtime.nextBackgroundWorkerAssignment(
		context.Background(),
		1,
		false,
		semanticIndexingSchedule{},
		false,
		state,
		true,
	); !ok || assignment.phase != "metadata" {
		t.Fatalf("higher-priority assignment = %+v, %t, want metadata during verification backoff", assignment, ok)
	}

	expired := time.Now().UTC().Add(-time.Second)
	runtime.setLocalBackgroundWorkerRetryNotBefore("content_verification", &expired)
	state.MetadataQueued = 0
	if assignment, ok := runtime.nextBackgroundWorkerAssignment(
		context.Background(),
		1,
		false,
		semanticIndexingSchedule{},
		false,
		state,
		true,
	); !ok || assignment.phase != "content_verification" {
		t.Fatalf("expired retry assignment = %+v, %t, want content verification", assignment, ok)
	}
}

func TestLocalMetadataAndThumbnailSystemErrorsUseIndependentRetries(t *testing.T) {
	rootPath := t.TempDir()
	workers := 1
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.WorkerRuntime.HeavyTaskWorkers = &workers
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
	})
	runtime.backgroundWorkerRandom = func() float64 { return 0 }

	restoreStorageAvailableBytes := stubStorageAvailableBytes(t, 512*1024*1024, nil)
	if runtime.runScheduledLocalMetadataBatchWithWorkers(context.Background(), "test", 1, 1) {
		restoreStorageAvailableBytes()
		t.Fatal("metadata batch = true under storage guard, want deferred error")
	}
	metadataRetryAt := runtime.localBackgroundWorkerRetryNotBeforeAt("metadata")
	if metadataRetryAt == nil || !metadataRetryAt.After(time.Now().UTC()) {
		restoreStorageAvailableBytes()
		t.Fatalf("metadata retry deadline = %v, want future retry", metadataRetryAt)
	}

	state := schedulerWorkState{MetadataQueued: 1, ThumbnailQueued: 1}
	assignment, ok := runtime.nextBackgroundWorkerAssignment(
		context.Background(),
		1,
		false,
		semanticIndexingSchedule{},
		false,
		state,
		true,
	)
	if !ok || assignment.phase != "thumbnails" {
		restoreStorageAvailableBytes()
		t.Fatalf("assignment during metadata retry = %+v, %t, want thumbnails", assignment, ok)
	}

	if runtime.runScheduledLocalThumbnailBatchWithWorkers(context.Background(), "test", 1, 1) {
		restoreStorageAvailableBytes()
		t.Fatal("thumbnail batch = true under storage guard, want deferred error")
	}
	restoreStorageAvailableBytes()
	thumbnailRetryAt := runtime.localBackgroundWorkerRetryNotBeforeAt("thumbnails")
	if thumbnailRetryAt == nil || !thumbnailRetryAt.After(time.Now().UTC()) {
		t.Fatalf("thumbnail retry deadline = %v, want future retry", thumbnailRetryAt)
	}
	if assignment, ok := runtime.nextBackgroundWorkerAssignment(
		context.Background(),
		1,
		false,
		semanticIndexingSchedule{},
		false,
		state,
		true,
	); ok {
		t.Fatalf("assignment with both local phases deferred = %+v, want none", assignment)
	}
	semanticState := state
	semanticState.SemanticEmbeddingReady = true
	semanticState.SemanticEmbeddingBatchSize = 1
	if assignment, ok := runtime.nextBackgroundWorkerAssignment(
		context.Background(),
		1,
		false,
		semanticIndexingSchedule{Workers: 1, BatchSize: 1},
		true,
		semanticState,
		true,
	); !ok || assignment.phase != "embeddings" {
		t.Fatalf("healthy semantic assignment during local retries = %+v, %t, want embeddings", assignment, ok)
	}
	runtime.schedulerWorkStateMu.Lock()
	runtime.schedulerWorkState = schedulerWorkState{
		UpdatedAt:       time.Now().UTC(),
		MetadataQueued:  1,
		ThumbnailQueued: 1,
	}
	runtime.schedulerWorkStateMu.Unlock()
	if delay := runtime.backgroundWorkerIdleInterval(); delay <= localMetadataBatchInterval || delay > localBackgroundWorkerRetryDelay {
		t.Fatalf("idle interval with deferred local phases = %s, want retry deadline instead of batch polling", delay)
	}

	expired := time.Now().UTC().Add(-time.Second)
	runtime.setLocalBackgroundWorkerRetryNotBefore("metadata", &expired)
	if assignment, ok := runtime.nextBackgroundWorkerAssignment(
		context.Background(),
		1,
		false,
		semanticIndexingSchedule{},
		false,
		state,
		true,
	); !ok || assignment.phase != "metadata" {
		t.Fatalf("assignment after metadata retry = %+v, %t, want metadata", assignment, ok)
	}
	if got := runtime.localBackgroundWorkerRetryNotBeforeAt("thumbnails"); got == nil || !got.After(time.Now().UTC()) {
		t.Fatalf("thumbnail retry after metadata expiry = %v, want independent future deadline", got)
	}
}

func TestAutomaticLocalPhase0ScanUsesQuickModeUntilNextReconciliationSchedule(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
	})
	catalogService := runtime.catalogService()
	if catalogService == nil {
		t.Fatal("catalog service is nil")
	}

	first, err := runAutomaticLocalPhase0Scan(context.Background(), catalogService, "1111111111111111", time.Now().UTC(), time.UTC)
	if err != nil {
		t.Fatalf("first runAutomaticLocalPhase0Scan() error = %v", err)
	}
	if first.ScanMode != "reconciliation" {
		t.Fatalf("first automatic scan mode = %q, want reconciliation", first.ScanMode)
	}
	second, err := runAutomaticLocalPhase0Scan(context.Background(), catalogService, "1111111111111111", first.CompletedAt.Add(time.Minute), time.UTC)
	if err != nil {
		t.Fatalf("second runAutomaticLocalPhase0Scan() error = %v", err)
	}
	if second.ScanMode != "quick" {
		t.Fatalf("second automatic scan mode = %q, want quick", second.ScanMode)
	}
	nextReconciliation := nextLocalDailySchedule(first.CompletedAt, time.UTC, defaultLocalReconciliationTime).Add(time.Minute)
	third, err := runAutomaticLocalPhase0Scan(context.Background(), catalogService, "1111111111111111", nextReconciliation, time.UTC)
	if err != nil {
		t.Fatalf("third runAutomaticLocalPhase0Scan() error = %v", err)
	}
	if third.ScanMode != "reconciliation" {
		t.Fatalf("third automatic scan mode = %q, want reconciliation", third.ScanMode)
	}
	status, err := runtime.RefreshDatasourceIndexingStatus(context.Background())
	if err != nil {
		t.Fatalf("RefreshDatasourceIndexingStatus() error = %v", err)
	}
	var discovery DatasourceTaskStatus
	for _, task := range status.Tasks {
		if task.Phase == "phase0" {
			discovery = task
			break
		}
	}
	if discovery.LastQuickScanAt == nil || !discovery.LastQuickScanAt.Equal(second.CompletedAt) ||
		discovery.LastReconciliationAt == nil || !discovery.LastReconciliationAt.Equal(third.CompletedAt) {
		t.Fatalf("discovery scan times = %+v, want quick=%v reconciliation=%v", discovery, second.CompletedAt, third.CompletedAt)
	}
}

func TestManualLocalDiscoveryResetsPeriodicSchedule(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
	})
	if _, err := runtime.RunLocalDatasourceDiscoveryScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalDatasourceDiscoveryScan() error = %v", err)
	}
	select {
	case <-runtime.localScanScheduleReset:
	default:
		t.Fatal("manual discovery did not request periodic schedule reset")
	}
}

func TestLocalUploadTargetsMatchDeepestLocalRoot(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	localRoot := filepath.Join(tempDir, "media")
	uploadRoot := filepath.Join(localRoot, "uploads")
	archiveRoot := filepath.Join(tempDir, "archive")
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{
		{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
			Scan:      &config.LocalDatasourceScanConfig{},
		},
		{
			SourceKey: "2222222222222222",
			Name:      "Archive",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "archive",
			Scan:      &config.LocalDatasourceScanConfig{},
		},
		{
			SourceKey: "3333333333333333",
			Name:      "Parent",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "parent",
		},
	}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.UploadRoots = []config.UploadRootConfig{{
			Key:  "device-upload",
			Path: uploadRoot,
		}}
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{
			{Key: "nas-photos", Path: localRoot},
			{Key: "archive", Path: archiveRoot},
			{Key: "parent", Path: tempDir},
		}
	})

	targets := runtime.localUploadTargets(store.UploadedAsset{
		SelectedRootKey:   "device-upload",
		FinalRelativePath: "2026/06/photo.jpg",
	})
	if len(targets) != 1 {
		t.Fatalf("targets length = %d, want 1: %#v", len(targets), targets)
	}
	if targets[0].SourceKey != "1111111111111111" {
		t.Fatalf("target = %+v, want NAS Photos", targets[0])
	}
}

func TestCommittedAgentUploadQueuesMetadataWithoutIdleScan(t *testing.T) {
	rootPath := t.TempDir()
	finalRelativePath := "2026/uploaded.jpg"
	finalPath := filepath.Join(rootPath, filepath.FromSlash(finalRelativePath))
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(final parent) error = %v", err)
	}
	if err := os.WriteFile(finalPath, []byte("verified-upload"), 0o644); err != nil {
		t.Fatalf("WriteFile(final upload) error = %v", err)
	}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "2m"},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
		cfg.UploadRoots = []config.UploadRootConfig{{Key: "nas-photos", Path: rootPath}}
	})
	if _, err := runtime.catalogService().RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	runtime.schedulerWorkStateMu.Lock()
	runtime.schedulerWorkState = schedulerWorkState{
		ConfigHash:       "cached-before-upload",
		UpdatedAt:        time.Now().UTC(),
		MetadataSettling: 1,
	}
	runtime.schedulerWorkStateMu.Unlock()

	runtime.scheduleLocalDiscoveryAfterUpload(store.UploadedAsset{
		SelectedRootKey:   "nas-photos",
		FinalRelativePath: finalRelativePath,
	})
	state, err := runtime.catalogService().LocalMetadataQueueState(context.Background())
	if err != nil {
		t.Fatalf("LocalMetadataQueueState() error = %v", err)
	}
	if state.Queued != 1 || state.Settling != 0 {
		t.Fatalf("metadata queue state = %+v, want immediate Agent upload queue", state)
	}
	runtime.schedulerWorkStateMu.Lock()
	workState := runtime.schedulerWorkState
	runtime.schedulerWorkStateMu.Unlock()
	if !workState.Dirty || workState.MetadataQueued != 0 || workState.MetadataSettling != 1 {
		t.Fatalf("scheduler work state = %+v, want dirty cached state without guessed queue counts", workState)
	}
}

func TestLocalDatasourceWorkerRuntimeUsesSharedHeavyTaskBudget(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(3)
	})

	status := runtime.WorkerRuntimeStatus()
	if status.HeavyTaskWorkers != 3 ||
		status.LocalDatasourceWorkers != 3 ||
		status.LocalMetadataBatchSize != 144 ||
		status.LocalThumbnailBatchSize != 48 ||
		status.SemanticIndexingWorkers != 0 {
		t.Fatalf("WorkerRuntimeStatus = %+v, want local batch sizes derived from 3 heavy workers", status)
	}
}

func TestLocalDatasourceWorkerRuntimeCanPauseHeavyWork(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "paused.jpg"), encodeRuntimeJPEGForTest(t, 48, 32), 0o644); err != nil {
		t.Fatalf("WriteFile(paused.jpg) error = %v", err)
	}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(0)
	})

	status := runtime.WorkerRuntimeStatus()
	if status.HeavyTaskWorkers != 0 ||
		status.ConfiguredHeavyTaskWorkers == nil ||
		*status.ConfiguredHeavyTaskWorkers != 0 ||
		status.AutoHeavyTaskWorkers ||
		!status.PausedHeavyTaskWorkers ||
		status.LocalDatasourceWorkers != 0 ||
		status.LocalMetadataBatchSize != 0 ||
		status.LocalThumbnailBatchSize != 0 {
		t.Fatalf("WorkerRuntimeStatus = %+v, want paused local heavy work", status)
	}

	result, err := runtime.RunLocalDatasourcePhase0Scans(context.Background())
	if err != nil {
		t.Fatalf("RunLocalDatasourcePhase0Scans() error = %v", err)
	}
	if len(result.Phase0) != 1 || result.Phase0[0].QueuedMetadata != 1 {
		t.Fatalf("phase0 result = %+v, want one queued metadata job", result.Phase0)
	}
	if result.Metadata.ProcessedJobs != 0 || result.Thumbnail.ProcessedJobs != 0 {
		t.Fatalf("paused phase0 follow-up = metadata %+v thumbnails %+v, want no heavy processing", result.Metadata, result.Thumbnail)
	}
}

func TestRequeueFailedLocalDatasourceMetadataWhileWorkersPaused(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	photoPath := filepath.Join(rootPath, "failed.jpg")
	if err := os.WriteFile(photoPath, []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(failed.jpg) error = %v", err)
	}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(0)
	})
	catalogService := runtime.catalogService()
	if _, err := catalogService.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if err := os.Remove(photoPath); err != nil {
		t.Fatalf("Remove(failed.jpg) error = %v", err)
	}
	if result, err := catalogService.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.FailedJobs != 1 {
		t.Fatalf("RunLocalMetadataBatch() = %+v, %v, want one failed metadata job", result, err)
	}
	before, err := catalogService.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses(before requeue) error = %v", err)
	}
	if len(before) != 1 || before[0].FailedMetadataJobs != 1 || before[0].QueuedMetadataJobs != 0 {
		t.Fatalf("status before requeue = %+v, want one failed metadata job", before)
	}

	result, err := runtime.RequeueFailedLocalDatasourceMetadata(context.Background())
	if err != nil {
		t.Fatalf("RequeueFailedLocalDatasourceMetadata() error = %v", err)
	}
	if result.Queued != 1 {
		t.Fatalf("RequeueFailedLocalDatasourceMetadata() = %+v, want one queued metadata job", result)
	}
	after, err := catalogService.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses(after requeue) error = %v", err)
	}
	if len(after) != 1 || after[0].QueuedMetadataJobs != 1 || after[0].FailedMetadataJobs != 0 {
		t.Fatalf("status after paused requeue = %+v, want queued work without inline processing", after)
	}
}

func TestRequeueFailedLocalDatasourceThumbnailsWhileWorkersPaused(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "failed.jpg"), encodeRuntimeJPEGForTest(t, 48, 32), 0o644); err != nil {
		t.Fatalf("WriteFile(failed.jpg) error = %v", err)
	}
	helperPath := filepath.Join(t.TempDir(), "failing-media-helper")
	helperScript := `#!/bin/sh
if [ "$1" = "health" ]; then
  printf '%s\n' '{"schemaVersion":1,"ok":true,"helper":{"version":"0.1.0-test","platform":"test-platform"},"capabilities":{"renderImage":true,"renderVideoPoster":false,"inspectImage":false,"inspectVideo":false}}'
  exit 0
fi
echo "media helper image failure" >&2
exit 42
`
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o700); err != nil {
		t.Fatalf("WriteFile(failing media helper) error = %v", err)
	}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.MediaRuntime.HelperPath = helperPath
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(0)
	})
	catalogService := runtime.catalogService()
	if _, err := catalogService.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}
	if result, err := catalogService.RunLocalMetadataBatch(context.Background(), 10); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("RunLocalMetadataBatch() = %+v, %v, want one registered asset", result, err)
	}
	if result, err := catalogService.RunLocalThumbnailBatch(context.Background(), 10); err != nil || result.FailedJobs != 1 {
		t.Fatalf("RunLocalThumbnailBatch() = %+v, %v, want one failed thumbnail", result, err)
	}
	before, err := catalogService.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses(before requeue) error = %v", err)
	}
	if len(before) != 1 || before[0].FailedThumbnailJobs != 1 || before[0].PendingThumbnailJobs != 0 {
		t.Fatalf("status before requeue = %+v, want one failed thumbnail", before)
	}

	result, err := runtime.RequeueFailedLocalDatasourceThumbnails(context.Background())
	if err != nil {
		t.Fatalf("RequeueFailedLocalDatasourceThumbnails() error = %v", err)
	}
	if result.Queued != 1 {
		t.Fatalf("RequeueFailedLocalDatasourceThumbnails() = %+v, want one queued thumbnail", result)
	}
	after, err := catalogService.LocalDatasourceScanStatuses(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatuses(after requeue) error = %v", err)
	}
	if len(after) != 1 || after[0].PendingThumbnailJobs != 1 || after[0].QueuedThumbnailJobs != 1 || after[0].FailedThumbnailJobs != 0 {
		t.Fatalf("status after paused requeue = %+v, want queued work without inline processing", after)
	}
}

func TestUpdateWorkerRuntimeWakesBackgroundWorkerScheduler(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "wake.jpg"), encodeRuntimeJPEGForTest(t, 48, 32), 0o644); err != nil {
		t.Fatalf("WriteFile(wake.jpg) error = %v", err)
	}
	datasources := []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}
	localRoots := []config.LocalMediaRootConfig{{
		Key:  "nas-photos",
		Path: rootPath,
	}}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, datasources, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = localRoots
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(0)
	})

	fileConfig := config.Default()
	fileConfig.AgentName = "file-agent"
	fileConfig.DataDir = runtime.ConfigResponse().DataDir
	fileConfig.Datasources = datasources
	fileConfig.LocalMediaRoots = localRoots
	fileConfig.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(0)
	if err := config.WriteFile(runtime.ConfigResponse().ConfigPath, fileConfig); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	result, err := runtime.RunLocalDatasourcePhase0Scans(context.Background())
	if err != nil {
		t.Fatalf("RunLocalDatasourcePhase0Scans() error = %v", err)
	}
	if len(result.Phase0) != 1 || result.Phase0[0].QueuedMetadata != 1 {
		t.Fatalf("phase0 result = %+v, want one queued metadata job", result.Phase0)
	}
	if !runtime.StartBackgroundWorkerScheduler() {
		t.Fatal("StartBackgroundWorkerScheduler() = false, want running scheduler")
	}
	time.Sleep(50 * time.Millisecond)
	paused, err := runtime.LocalDatasourceScanStatus(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatus(paused) error = %v", err)
	}
	if len(paused.Datasources) != 1 || paused.Datasources[0].QueuedMetadataJobs != 1 {
		t.Fatalf("paused local status = %+v, want queued metadata preserved", paused.Datasources)
	}

	if _, err := runtime.UpdateWorkerRuntime(config.WorkerRuntimeConfig{HeavyTaskWorkers: runtimeTestIntPtr(1)}); err != nil {
		t.Fatalf("UpdateWorkerRuntime() error = %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		status, err := runtime.LocalDatasourceScanStatus(context.Background())
		if err != nil {
			t.Fatalf("LocalDatasourceScanStatus(after wake) error = %v", err)
		}
		if len(status.Datasources) == 1 &&
			status.Datasources[0].QueuedMetadataJobs == 0 &&
			status.Datasources[0].ActiveAssets == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("local status after worker wake = %+v, want metadata processed without waiting for normal tick", status.Datasources)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestUpdateWorkerRuntimeLeavesActiveBackgroundAssignmentRunningAfterReduction(t *testing.T) {
	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(8)
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
	})
	fileConfig := config.Default()
	fileConfig.AgentName = "file-agent"
	fileConfig.DataDir = runtime.ConfigResponse().DataDir
	fileConfig.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(8)
	if err := config.WriteFile(runtime.ConfigResponse().ConfigPath, fileConfig); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	assignment := backgroundWorkerAssignment{
		phase:   "metadata",
		workers: 8,
		run: func(ctx context.Context) bool {
			close(started)
			select {
			case <-release:
				close(finished)
				return true
			case <-ctx.Done():
				t.Errorf("active assignment canceled by worker reduction: %v", ctx.Err())
				close(finished)
				return false
			}
		},
	}
	if !runtime.launchBackgroundWorkerAssignment(context.Background(), assignment) {
		t.Fatal("launchBackgroundWorkerAssignment() = false, want active assignment")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background assignment did not start")
	}

	if _, err := runtime.UpdateWorkerRuntime(config.WorkerRuntimeConfig{HeavyTaskWorkers: runtimeTestIntPtr(1)}); err != nil {
		t.Fatalf("UpdateWorkerRuntime() error = %v", err)
	}
	select {
	case <-finished:
		t.Fatal("worker reduction interrupted the active assignment")
	case <-time.After(50 * time.Millisecond):
	}
	if activeWorkers, _ := runtime.backgroundWorkerActiveState(); activeWorkers != 8 {
		t.Fatalf("active workers after reduction = %d, want existing 8-worker assignment preserved", activeWorkers)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("active assignment did not finish after release")
	}
}

func TestUpdateWorkerRuntimePauseLeavesActiveAssignmentRunningAndBlocksNewAssignment(t *testing.T) {
	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
	})
	fileConfig := config.Default()
	fileConfig.AgentName = "file-agent"
	fileConfig.DataDir = runtime.ConfigResponse().DataDir
	fileConfig.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	if err := config.WriteFile(runtime.ConfigResponse().ConfigPath, fileConfig); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	if !runtime.launchBackgroundWorkerAssignment(context.Background(), backgroundWorkerAssignment{
		phase:   "embeddings",
		workers: 1,
		run: func(ctx context.Context) bool {
			close(started)
			select {
			case <-release:
				close(finished)
				return true
			case <-ctx.Done():
				t.Errorf("active assignment canceled by pause: %v", ctx.Err())
				close(finished)
				return false
			}
		},
	}) {
		t.Fatal("launchBackgroundWorkerAssignment() = false, want active semantic assignment")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("semantic assignment did not start")
	}

	if _, err := runtime.UpdateWorkerRuntime(config.WorkerRuntimeConfig{HeavyTaskWorkers: runtimeTestIntPtr(0)}); err != nil {
		t.Fatalf("UpdateWorkerRuntime() error = %v", err)
	}
	select {
	case <-finished:
		t.Fatal("pause interrupted the active assignment")
	case <-time.After(50 * time.Millisecond):
	}

	newAssignmentRan := make(chan struct{})
	if runtime.launchBackgroundWorkerAssignment(context.Background(), backgroundWorkerAssignment{
		phase:   "metadata",
		workers: 1,
		run: func(context.Context) bool {
			close(newAssignmentRan)
			return true
		},
	}) {
		t.Fatal("launchBackgroundWorkerAssignment() = true after pause, want new assignment rejected")
	}
	select {
	case <-newAssignmentRan:
		t.Fatal("new assignment ran after pause")
	default:
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("active assignment did not finish after release")
	}
}

func TestBackgroundWorkerMetadataDoesNotWaitForDiscoveryLock(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "queued.jpg"), encodeRuntimeJPEGForTest(t, 48, 32), 0o644); err != nil {
		t.Fatalf("WriteFile(queued.jpg) error = %v", err)
	}
	datasources := []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}
	localRoots := []config.LocalMediaRootConfig{{
		Key:  "nas-photos",
		Path: rootPath,
	}}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, datasources, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = localRoots
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	})

	result, err := runtime.RunLocalDatasourcePhase0Scans(context.Background())
	if err != nil {
		t.Fatalf("RunLocalDatasourcePhase0Scans() error = %v", err)
	}
	if len(result.Phase0) != 1 || result.Phase0[0].QueuedMetadata != 1 {
		t.Fatalf("phase0 result = %+v, want one queued metadata job", result.Phase0)
	}

	releaseDiscovery, err := runtime.localDiscovery.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire local discovery single-flight: %v", err)
	}
	defer releaseDiscovery()
	if processed := runtime.runNextBackgroundWorkerTask(context.Background()); !processed {
		t.Fatal("runNextBackgroundWorkerTask() = false, want metadata processed while discovery lock is held")
	}
	status, err := runtime.LocalDatasourceScanStatus(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatus() error = %v", err)
	}
	if len(status.Datasources) != 1 ||
		status.Datasources[0].QueuedMetadataJobs != 0 ||
		status.Datasources[0].ActiveAssets != 1 {
		t.Fatalf("local status = %+v, want metadata processed", status.Datasources)
	}
}

func TestBackgroundWorkerSchedulerCanSelectLocalMetadataBeforeThumbnails(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	for index := 0; index < 50; index++ {
		name := fmt.Sprintf("local-%02d.jpg", index)
		if err := os.WriteFile(filepath.Join(rootPath, name), encodeRuntimeJPEGForTest(t, 48+index, 32), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	mediaHelperPath := writeRuntimeFakeMediaHelperScript(t, "")
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
		cfg.MediaRuntime.HelperPath = mediaHelperPath
	})
	runtime.backgroundWorkerRandom = func() float64 { return 0 }

	result, err := runtime.RunLocalDatasourcePhase0Scans(context.Background())
	if err != nil {
		t.Fatalf("RunLocalDatasourcePhase0Scans() error = %v", err)
	}
	if len(result.Phase0) != 1 || result.Phase0[0].QueuedMetadata != 50 {
		t.Fatalf("phase0 result = %+v, want 50 queued metadata jobs", result.Phase0)
	}
	if result.Metadata.ProcessedJobs != 0 || result.Thumbnail.ProcessedJobs != 0 {
		t.Fatalf("phase0 follow-up = metadata %+v thumbnails %+v, want discovery-only scan", result.Metadata, result.Thumbnail)
	}
	if processed := runtime.runNextBackgroundWorkerTask(context.Background()); !processed {
		t.Fatal("first runNextBackgroundWorkerTask() = false, want metadata batch")
	}
	status, err := runtime.LocalDatasourceScanStatus(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatus(after first metadata) error = %v", err)
	}
	if len(status.Datasources) != 1 || status.Datasources[0].QueuedMetadataJobs != 2 {
		t.Fatalf("local status after first metadata = %+v, want 2 queued metadata jobs", status.Datasources)
	}
	if status.Datasources[0].PendingThumbnailJobs != 48 {
		t.Fatalf("local status after first metadata = %+v, want 48 pending thumbnails", status.Datasources)
	}
	if processed := runtime.runNextBackgroundWorkerTask(context.Background()); !processed {
		t.Fatal("second runNextBackgroundWorkerTask() = false, want remaining metadata batch")
	}

	status, err = runtime.LocalDatasourceScanStatus(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatus(after metadata drained) error = %v", err)
	}
	if len(status.Datasources) != 1 || status.Datasources[0].QueuedMetadataJobs != 0 {
		t.Fatalf("local status after metadata drain = %+v, want empty metadata queue", status.Datasources)
	}
	if status.Datasources[0].PendingThumbnailJobs != 50 {
		t.Fatalf("local status after metadata drain = %+v, want all thumbnails pending", status.Datasources)
	}
	if processed := runtime.runNextBackgroundWorkerTask(context.Background()); !processed {
		t.Fatal("third runNextBackgroundWorkerTask() = false, want thumbnail batch")
	}
	status, err = runtime.LocalDatasourceScanStatus(context.Background())
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatus(after thumbnail) error = %v", err)
	}
	if len(status.Datasources) != 1 || status.Datasources[0].PendingThumbnailJobs != 34 {
		t.Fatalf("local status after first thumbnail batch = %+v, want 34 pending thumbnails", status.Datasources)
	}
}

func TestChooseMixedBackgroundWorkerPhaseWeightsQueueSizes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		metadataQueued  int
		thumbnailQueued int
		embeddingQueued int
		embeddingBatch  int
		randomValue     float64
		want            string
	}{
		{name: "empty", metadataQueued: 0, thumbnailQueued: 0, randomValue: 0, want: ""},
		{name: "metadata only", metadataQueued: 5, thumbnailQueued: 0, randomValue: 0.9, want: "metadata"},
		{name: "thumbnail only", metadataQueued: 0, thumbnailQueued: 5, randomValue: 0, want: "thumbnails"},
		{name: "embedding below batch skipped", metadataQueued: 0, thumbnailQueued: 0, embeddingQueued: 9, embeddingBatch: 10, randomValue: 0, want: ""},
		{name: "embedding only", metadataQueued: 0, thumbnailQueued: 0, embeddingQueued: 10, embeddingBatch: 10, randomValue: 0.9, want: "embeddings"},
		{name: "weighted metadata", metadataQueued: 8, thumbnailQueued: 2, embeddingQueued: 100, embeddingBatch: 10, randomValue: 0.1, want: "metadata"},
		{name: "weighted thumbnail", metadataQueued: 8, thumbnailQueued: 2, embeddingQueued: 100, embeddingBatch: 10, randomValue: 0.2, want: "thumbnails"},
		{name: "weighted embedding", metadataQueued: 8, thumbnailQueued: 2, embeddingQueued: 100, embeddingBatch: 10, randomValue: 0.3, want: "embeddings"},
		{name: "capped queues retain each phase", metadataQueued: 300000, thumbnailQueued: 300000, embeddingQueued: 300000, embeddingBatch: 10, randomValue: 0.29, want: "metadata"},
		{name: "capped queues retain thumbnail", metadataQueued: 300000, thumbnailQueued: 300000, embeddingQueued: 300000, embeddingBatch: 10, randomValue: 0.32, want: "thumbnails"},
		{name: "capped queues retain embedding", metadataQueued: 300000, thumbnailQueued: 300000, embeddingQueued: 300000, embeddingBatch: 10, randomValue: 0.9, want: "embeddings"},
		{name: "reported backlog favors local work", metadataQueued: 310641, thumbnailQueued: 14574, embeddingQueued: 168, embeddingBatch: 10, randomValue: 0.93, want: "thumbnails"},
		{name: "reported backlog still permits embeddings", metadataQueued: 310641, thumbnailQueued: 14574, embeddingQueued: 168, embeddingBatch: 10, randomValue: 0.95, want: "embeddings"},
		{name: "large metadata backlog does not starve thumbnails", metadataQueued: 300000, thumbnailQueued: 48, randomValue: 0.9, want: "thumbnails"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := chooseMixedBackgroundWorkerPhase(tt.metadataQueued, tt.thumbnailQueued, tt.embeddingQueued, tt.embeddingBatch, tt.randomValue); got != tt.want {
				t.Fatalf("chooseMixedBackgroundWorkerPhase(%d, %d, %d, %d, %f) = %q, want %q", tt.metadataQueued, tt.thumbnailQueued, tt.embeddingQueued, tt.embeddingBatch, tt.randomValue, got, tt.want)
			}
		})
	}
}

func TestDatasourceTaskStatusesIncludeFailedEmbeddings(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:           "1111111111111111",
		Name:                "NAS Photos",
		IngestionKind:       datasourceIngestionFilesystem,
		FailedEmbeddingJobs: 1,
	}})
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	if byPhase["embeddings"].FailedTasks != 1 || byPhase["embeddings"].Status != "attention" {
		t.Fatalf("embedding task status = %+v, want failed attention", byPhase["embeddings"])
	}
	if note := byPhase["embeddings"].Note; !strings.Contains(note, "Within each datasource, first-attempt work is prioritized ahead of eligible retries, so the failed count may remain unchanged while new embeddings complete.") {
		t.Fatalf("embedding task note = %q, want datasource-scoped deferred retry guidance", note)
	}
}

func TestDatasourceTaskStatusesMarkSemanticTasksNotEnabledWithoutModel(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:       "1111111111111111",
		Name:            "NAS Photos",
		IngestionKind:   datasourceIngestionFilesystem,
		IndexingEnabled: true,
	}})
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	for _, phase := range []string{"embeddings", "search_index"} {
		task := byPhase[phase]
		roleNote := datasourceTaskNoteEmbeddings
		if phase == "search_index" {
			roleNote = datasourceTaskNoteSearchIndex
		}
		wantNote := roleNote + " " + datasourceTaskNoteSearchModelRequired
		if task.Status != "idle" || task.SetupRequired != datasourceTaskSetupSearchModel || task.Note != wantNote {
			t.Fatalf("%s task = %+v, want idle with task role and search-model setup guidance", phase, task)
		}
	}
}

func TestAutoHeavyTaskWorkersForCPU(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		cpuCount int
		want     int
	}{
		{name: "zero", cpuCount: 0, want: 1},
		{name: "one", cpuCount: 1, want: 1},
		{name: "two", cpuCount: 2, want: 1},
		{name: "three", cpuCount: 3, want: 1},
		{name: "four", cpuCount: 4, want: 2},
		{name: "fifteen", cpuCount: 15, want: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := autoHeavyTaskWorkersForCPU(test.cpuCount); got != test.want {
				t.Fatalf("autoHeavyTaskWorkersForCPU(%d) = %d, want %d", test.cpuCount, got, test.want)
			}
		})
	}
}

func TestDatasourceIndexingAdminActiveWindow(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	now := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	if runtime.datasourceIndexingAdminActive(now) {
		t.Fatal("datasourceIndexingAdminActive() = true before any Admin access")
	}
	runtime.markDatasourceIndexingAdminAccess(now)
	if !runtime.datasourceIndexingAdminActive(now.Add(datasourceIndexingAdminActiveWindow)) {
		t.Fatal("datasourceIndexingAdminActive() = false at the active window boundary")
	}
	if runtime.datasourceIndexingAdminActive(now.Add(datasourceIndexingAdminActiveWindow + time.Second)) {
		t.Fatal("datasourceIndexingAdminActive() = true after the active window")
	}
}

func TestDatasourceTaskStatusesUseDatasourceEmbeddingCoverage(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:          "1111111111111111",
		Name:               "NAS Photos",
		IngestionKind:      datasourceIngestionRemoteAPI,
		EmbeddingStatus:    catalog.SemanticBackfillStatusBackfilling,
		EmbeddingModelID:   "timich-siglip2-base-patch16-224-onnx-multilingual-v1",
		EmbeddingEligible:  100,
		EmbeddingCompleted: 40,
		EmbeddingIndexed:   30,
		EmbeddingRemaining: 60,
	}})
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["embeddings"]; got.QueuedTasks != 60 || got.CompletedTasks != 40 || got.TotalTasks != 100 || got.Status != "queued" || got.QueuedTasksUnknown {
		t.Fatalf("embedding task status = %+v, want datasource coverage progress", got)
	}
	if got := byPhase["search_index"]; got.QueuedTasks != 10 || got.CompletedTasks != 30 || got.TotalTasks != 40 || got.Status != "queued" || got.QueuedTasksUnknown {
		t.Fatalf("search index task status = %+v, want unindexed vector progress", got)
	}
}

func TestDatasourceTaskStatusesShowEmbeddingScheduledWaitBetweenBatches(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.SemanticRuntime.Indexing = config.SemanticIndexingConfig{
			Enabled: true,
		}
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	})
	nextRunAt := time.Now().UTC().Add(30 * time.Second)
	runtime.setSemanticIndexingNextRunAt(&nextRunAt)

	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:          "1111111111111111",
		Name:               "NAS Photos",
		IngestionKind:      datasourceIngestionFilesystem,
		EmbeddingStatus:    catalog.SemanticBackfillStatusBackfilling,
		EmbeddingModelID:   "timich-siglip2-base-patch16-224-onnx-multilingual-v1",
		EmbeddingEligible:  100,
		EmbeddingCompleted: 40,
		EmbeddingIndexed:   40,
		EmbeddingRemaining: 60,
	}})
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	embeddings := byPhase["embeddings"]
	if embeddings.Status != "waiting" ||
		embeddings.WaitingReason != datasourceTaskWaitingScheduled ||
		embeddings.ActiveTasks != 0 ||
		embeddings.QueuedTasks != 60 ||
		embeddings.NextRunAt == nil ||
		!embeddings.NextRunAt.Equal(nextRunAt) {
		t.Fatalf("embedding task status = %+v, want scheduled wait between batches", embeddings)
	}
}

func TestDatasourceTaskStatusesShowQueuedHeavyWorkPausedWhenWorkersArePaused(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(0)
	})
	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:                 "1111111111111111",
		Name:                      "NAS Photos",
		IngestionKind:             datasourceIngestionFilesystem,
		QueuedMetadataJobs:        1,
		PendingThumbnailJobs:      2,
		EmbeddingStatus:           catalog.SemanticBackfillStatusBackfilling,
		EmbeddingModelID:          "timich-siglip2-base-patch16-224-onnx-multilingual-v1",
		EmbeddingEligible:         100,
		EmbeddingCompleted:        40,
		EmbeddingIndexed:          30,
		EmbeddingRemaining:        60,
		FailedEmbeddingJobs:       0,
		EmbeddingPendingIndexJobs: 0,
	}})
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	for _, phase := range []string{"metadata", "thumbnails", "embeddings", "search_index"} {
		if got := byPhase[phase]; got.Status != "paused" || got.WaitingReason != datasourceTaskWaitingPaused || got.ActiveTasks != 0 {
			t.Fatalf("%s task = %+v, want paused worker state", phase, got)
		}
	}
}

func TestDatasourceTaskStatusesKeepInFlightHeavyWorkRunningWhileWorkersArePaused(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(0)
	})
	runtime.rememberDatasourceTaskActivitySnapshot(nil, "embeddings", 1)
	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:          "1111111111111111",
		Name:               "NAS Photos",
		IngestionKind:      datasourceIngestionFilesystem,
		EmbeddingStatus:    catalog.SemanticBackfillStatusBackfilling,
		EmbeddingModelID:   "timich-siglip2-base-patch16-224-onnx-multilingual-v1",
		EmbeddingEligible:  100,
		EmbeddingCompleted: 40,
		EmbeddingIndexed:   40,
		EmbeddingRemaining: 60,
	}})
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["embeddings"]; got.Status != "running" || got.WaitingReason != "" || got.ActiveTasks != 1 {
		t.Fatalf("embeddings task = %+v, want in-flight work to remain running until the current batch completes", got)
	}
	if got := byPhase["metadata"]; got.Status != "idle" || got.ActiveTasks != 0 {
		t.Fatalf("metadata task = %+v, want unrelated empty work to remain idle", got)
	}
}

func TestRefreshDatasourceIndexingStatusShowsPausedInstalledSemanticWork(t *testing.T) {
	ctx := context.Background()
	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "immich-api-key" {
			t.Fatalf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
		}
		switch r.URL.Path {
		case "/api/search/metadata":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"assets": {
					"total": 1,
					"items": [
						{
							"id": "asset-paused-semantic",
							"type": "IMAGE",
							"originalFileName": "paused-semantic.jpg",
							"fileCreatedAt": "2026-06-01T10:00:00Z",
							"updatedAt": "2026-06-01T10:05:00Z"
						}
					],
					"nextPage": null
				}
			}`)
		default:
			t.Fatalf("unexpected datasource path %s", r.URL.RequestURI())
		}
	}))
	defer datasourceServer.Close()

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        "immich_indexed",
		URL:         datasourceServer.URL,
		AccessToken: "immich-api-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 1,
		},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(0)
	})

	if _, err := runtime.SyncPrimaryDatasourceMirror(ctx, catalog.MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncPrimaryDatasourceMirror() error = %v", err)
	}
	pack := runtimeSemanticPackForTest("timich-siglip2-base-patch16-224-onnx-multilingual-v1")
	installRuntimeSemanticPackForTest(t, runtime, pack)

	status, err := runtime.RefreshDatasourceIndexingStatus(ctx)
	if err != nil {
		t.Fatalf("RefreshDatasourceIndexingStatus() error = %v", err)
	}
	if len(status.Datasources) != 1 ||
		status.Datasources[0].EmbeddingModelID != pack.ID ||
		status.Datasources[0].EmbeddingEligible != 1 ||
		status.Datasources[0].EmbeddingRemaining != 1 {
		t.Fatalf("datasource semantic coverage = %+v, want installed model coverage for one queued asset", status.Datasources)
	}
	tasks := map[string]DatasourceTaskStatus{}
	for _, task := range status.Tasks {
		tasks[task.Phase] = task
	}
	embeddings := tasks["embeddings"]
	if embeddings.Status != "paused" ||
		embeddings.WaitingReason != datasourceTaskWaitingPaused ||
		embeddings.QueuedTasks != 1 ||
		embeddings.CompletedTasks != 0 ||
		embeddings.TotalTasks != 1 {
		t.Fatalf("embeddings task = %+v, want paused queued semantic work", embeddings)
	}
}

func TestDatasourceTaskStatusesShowQueuedEmbeddingWaitingWhenWorkersAreBusy(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	})
	runtime.rememberDatasourceTaskActivitySnapshot(nil, "thumbnails", 1)
	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:          "1111111111111111",
		Name:               "NAS Photos",
		IngestionKind:      datasourceIngestionFilesystem,
		EmbeddingStatus:    catalog.SemanticBackfillStatusBackfilling,
		EmbeddingModelID:   "timich-siglip2-base-patch16-224-onnx-multilingual-v1",
		EmbeddingEligible:  100,
		EmbeddingCompleted: 40,
		EmbeddingIndexed:   40,
		EmbeddingRemaining: 60,
	}})
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["thumbnails"]; got.Status != "running" || got.ActiveTasks != 1 {
		t.Fatalf("thumbnail task = %+v, want running", got)
	}
	if got := byPhase["embeddings"]; got.Status != "queued" || got.WaitingReason != "" || got.ActiveTasks != 0 || got.QueuedTasks != 60 {
		t.Fatalf("embedding task = %+v, want queued while workers are busy", got)
	}
}

func TestLocalActiveWorkersForBatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		workers       int
		queued        int
		jobsPerWorker int
		want          int
	}{
		{name: "paused", workers: 0, queued: 64, jobsPerWorker: 32, want: 0},
		{name: "empty", workers: 2, queued: 0, jobsPerWorker: 32, want: 0},
		{name: "partial first worker", workers: 2, queued: 1, jobsPerWorker: 32, want: 1},
		{name: "full first worker", workers: 2, queued: 32, jobsPerWorker: 32, want: 1},
		{name: "second worker", workers: 2, queued: 33, jobsPerWorker: 32, want: 2},
		{name: "capped at configured workers", workers: 2, queued: 200, jobsPerWorker: 32, want: 2},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := localActiveWorkersForBatch(tt.workers, tt.queued, tt.jobsPerWorker); got != tt.want {
				t.Fatalf("localActiveWorkersForBatch(%d, %d, %d) = %d, want %d", tt.workers, tt.queued, tt.jobsPerWorker, got, tt.want)
			}
		})
	}
}

func TestNormalizeDatasourceTaskWorkerStateCapsStaleActiveWorkers(t *testing.T) {
	t.Parallel()

	paused := normalizeDatasourceTaskDependencies(normalizeDatasourceTaskWorkerState([]DatasourceTaskStatus{
		{
			Phase:       "embeddings",
			Label:       "Embeddings",
			ActiveTasks: 1,
			QueuedTasks: 60,
		},
		{
			Phase:       "thumbnails",
			Label:       "Thumbnails",
			QueuedTasks: 20,
		},
	}, 0))
	pausedByPhase := map[string]DatasourceTaskStatus{}
	for _, task := range paused {
		pausedByPhase[task.Phase] = task
	}
	if got := pausedByPhase["embeddings"]; got.ActiveTasks != 1 || got.Status != "running" {
		t.Fatalf("paused embedding task = %+v, want active assignment preserved", got)
	}
	if got := pausedByPhase["thumbnails"]; got.ActiveTasks != 0 || got.Status != "paused" {
		t.Fatalf("paused thumbnail task = %+v, want queued inactive work paused", got)
	}

	localOnly := normalizeDatasourceTaskWorkerState([]DatasourceTaskStatus{{
		Phase:       "metadata",
		Label:       "Metadata",
		ActiveTasks: 8,
		QueuedTasks: 100,
	}}, 1)
	if len(localOnly) != 1 || localOnly[0].ActiveTasks != 1 {
		t.Fatalf("local-only normalized tasks = %+v, want one active metadata worker", localOnly)
	}

	tasks := normalizeDatasourceTaskDependencies(normalizeDatasourceTaskWorkerState([]DatasourceTaskStatus{
		{
			Phase:       "metadata",
			Label:       "Metadata",
			ActiveTasks: 8,
			QueuedTasks: 100,
		},
		{
			Phase:       "embeddings",
			Label:       "Embeddings",
			ActiveTasks: 1,
			QueuedTasks: 60,
		},
	}, 1))
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["embeddings"]; got.ActiveTasks != 1 || got.Status != "running" {
		t.Fatalf("embedding task = %+v, want one active worker preserved", got)
	}
	if got := byPhase["metadata"]; got.ActiveTasks != 0 || got.Status != "queued" {
		t.Fatalf("metadata task = %+v, want stale active workers removed", got)
	}
}

func TestNormalizeDatasourceIndexingSnapshotRestoresLiveActivityAfterPositiveReduction(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	})
	runtime.setDatasourceTaskActive("embeddings", 1)
	runtime.setDatasourceTaskActive("metadata", 7)

	response := runtime.normalizeDatasourceIndexingSnapshot(DatasourceIndexingResponse{Tasks: []DatasourceTaskStatus{
		{
			Phase:       "metadata",
			Label:       "Metadata",
			ActiveTasks: 7,
			QueuedTasks: 100,
		},
		{
			Phase:       "embeddings",
			Label:       "Embeddings",
			ActiveTasks: 1,
			QueuedTasks: 60,
		},
	}})
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range response.Tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["embeddings"]; got.ActiveTasks != 1 || got.Status != "running" {
		t.Fatalf("embedding task after reduction = %+v, want live assignment running", got)
	}
	if got := byPhase["metadata"]; got.ActiveTasks != 7 || got.Status != "running" {
		t.Fatalf("metadata task after reduction = %+v, want all live workers running until completion", got)
	}
}

func TestDatasourceTaskStatusesShowPriorityIndexPublishQueued(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.SemanticRuntime.Indexing = config.SemanticIndexingConfig{
			Enabled: true,
		}
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	})
	lastPublishedAt := time.Now().UTC()
	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:                "1111111111111111",
		Name:                     "NAS Photos",
		IngestionKind:            datasourceIngestionFilesystem,
		EmbeddingStatus:          catalog.SemanticBackfillStatusIndexing,
		EmbeddingModelID:         "timich-siglip2-base-patch16-224-onnx-multilingual-v1",
		EmbeddingEligible:        100,
		EmbeddingCompleted:       60,
		EmbeddingIndexed:         50,
		EmbeddingRemaining:       40,
		EmbeddingLastPublishedAt: &lastPublishedAt,
	}})
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	searchIndex := byPhase["search_index"]
	if searchIndex.Status != "queued" ||
		searchIndex.WaitingReason != "" ||
		searchIndex.QueuedTasks != 10 ||
		searchIndex.CompletedTasks != 50 ||
		searchIndex.TotalTasks != 60 ||
		searchIndex.NextRunAt != nil {
		t.Fatalf("search index task status = %+v, want priority publish queued without scheduled timer", searchIndex)
	}
}

func TestDatasourceTaskStatusesShowIndexPublishQueuedTargetWait(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.SemanticRuntime.Indexing = config.SemanticIndexingConfig{
			Enabled: true,
		}
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	})
	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:          "1111111111111111",
		Name:               "NAS Photos",
		IngestionKind:      datasourceIngestionFilesystem,
		EmbeddingStatus:    catalog.SemanticBackfillStatusBackfilling,
		EmbeddingModelID:   "timich-siglip2-base-patch16-224-onnx-multilingual-v1",
		EmbeddingEligible:  1500,
		EmbeddingCompleted: 1050,
		EmbeddingIndexed:   1000,
		EmbeddingRemaining: 450,
		// A queued durable publish job should not hide the priority-threshold wait.
		EmbeddingPendingIndexJobs: 1,
	}})
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	searchIndex := byPhase["search_index"]
	if searchIndex.Status != "waiting" ||
		searchIndex.WaitingReason != datasourceTaskWaitingQueuedTarget ||
		searchIndex.WaitingQueuedTarget != 200 ||
		searchIndex.QueuedTasks != 50 ||
		searchIndex.CompletedTasks != 1000 ||
		searchIndex.TotalTasks != 1050 {
		t.Fatalf("search index task status = %+v, want queued target wait", searchIndex)
	}
}

func TestDatasourceTaskStatusesShowIndexPublishQueuedTargetWaitFromStats(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.SemanticRuntime.Indexing = config.SemanticIndexingConfig{
			Enabled: true,
		}
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	})
	stats := catalog.AssetProcessingStatsSnapshot{
		RefreshedAt: time.Now().UTC(),
		Stats: []catalog.AssetProcessingStat{
			{Stage: catalog.AssetProcessingStageEmbeddings, Status: catalog.AssetProcessingStatusReady, Count: 1050, TotalCount: 1500},
			{Stage: catalog.AssetProcessingStageEmbeddings, Status: catalog.AssetProcessingStatusPending, Count: 450, TotalCount: 1500},
			{Stage: catalog.AssetProcessingStageSearchIndex, Status: catalog.AssetProcessingStatusReady, Count: 1000, TotalCount: 1050},
			{Stage: catalog.AssetProcessingStageSearchIndex, Status: catalog.AssetProcessingStatusPending, Count: 50, TotalCount: 1050},
		},
	}

	tasks := runtime.datasourceTaskStatuses(context.Background(), nil, stats)
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	searchIndex := byPhase["search_index"]
	if searchIndex.Status != "waiting" ||
		searchIndex.WaitingReason != datasourceTaskWaitingQueuedTarget ||
		searchIndex.WaitingQueuedTarget != 200 ||
		searchIndex.QueuedTasks != 50 ||
		searchIndex.CompletedTasks != 1000 ||
		searchIndex.TotalTasks != 1050 {
		t.Fatalf("search index task status from stats = %+v, want queued target wait", searchIndex)
	}
}

func TestDatasourceTaskStatusesKeepFailureUnitsOutOfProgressTotals(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	stats := catalog.AssetProcessingStatsSnapshot{
		RefreshedAt: time.Now().UTC(),
		Stats: []catalog.AssetProcessingStat{
			{Stage: catalog.AssetProcessingStageEmbeddings, Status: catalog.AssetProcessingStatusReady, Count: 80, TotalCount: 100},
			{Stage: catalog.AssetProcessingStageEmbeddings, Status: catalog.AssetProcessingStatusPending, Count: 19, TotalCount: 100},
			{Stage: catalog.AssetProcessingStageEmbeddings, Status: catalog.AssetProcessingStatusFailed, Count: 1, TotalCount: 100},
			{Stage: catalog.AssetProcessingStageSearchIndex, Status: catalog.AssetProcessingStatusReady, Count: 70, TotalCount: 80},
			{Stage: catalog.AssetProcessingStageSearchIndex, Status: catalog.AssetProcessingStatusPending, Count: 10, TotalCount: 80},
			{Stage: catalog.AssetProcessingStageSearchIndex, Status: catalog.AssetProcessingStatusFailed, Count: 1, TotalCount: 80},
		},
	}

	tasks := runtime.datasourceTaskStatuses(context.Background(), nil, stats)
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	embeddings := byPhase["embeddings"]
	if embeddings.QueuedTasks != 19 ||
		embeddings.CompletedTasks != 80 ||
		embeddings.TotalTasks != 100 ||
		embeddings.FailedTasks != 1 ||
		embeddings.FailureUnit != datasourceTaskFailureUnitItems {
		t.Fatalf("embeddings task = %+v, want disjoint item counts", embeddings)
	}
	searchIndex := byPhase["search_index"]
	if searchIndex.QueuedTasks != 10 ||
		searchIndex.CompletedTasks != 70 ||
		searchIndex.TotalTasks != 80 ||
		searchIndex.FailedTasks != 1 ||
		searchIndex.FailureUnit != datasourceTaskFailureUnitPublishJobs ||
		!strings.Contains(searchIndex.Note, "Publishing can take several hours for a large library.") ||
		!strings.Contains(searchIndex.Note, "An existing published index remains searchable while publishing runs.") ||
		!strings.Contains(searchIndex.Note, "Failed publish jobs are retried automatically on the next eligible run.") {
		t.Fatalf("search index task = %+v, want vector progress plus a separate failed publish job", searchIndex)
	}
}

func TestNormalizeDatasourceIndexingSnapshotPrefersSearchIndexWait(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.Indexing = config.SemanticIndexingConfig{
			Enabled: true,
		}
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	})
	snapshot := runtime.normalizeDatasourceIndexingSnapshot(DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:          "embeddings",
			Label:          "Embeddings",
			QueuedTasks:    450,
			CompletedTasks: 1050,
			TotalTasks:     1500,
			Status:         "queued",
		}, {
			Phase:          "search_index",
			Label:          "Search index",
			QueuedTasks:    50,
			CompletedTasks: 1000,
			TotalTasks:     1050,
			Status:         "queued",
		}},
	})

	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	searchIndex := byPhase["search_index"]
	if searchIndex.Status != "waiting" ||
		searchIndex.WaitingReason != datasourceTaskWaitingQueuedTarget ||
		searchIndex.WaitingQueuedTarget != 200 ||
		searchIndex.QueuedTasks != 50 {
		t.Fatalf("search index task from stale snapshot = %+v, want queued target wait", searchIndex)
	}
}

func TestApplySemanticAggregateClearsStaleSearchIndexWait(t *testing.T) {
	t.Parallel()

	nextRunAt := time.Now().UTC().Add(10 * time.Minute)
	tasks := []DatasourceTaskStatus{{
		Phase:               "search_index",
		Label:               "Search index",
		QueuedTasks:         50,
		CompletedTasks:      1000,
		TotalTasks:          1050,
		WaitingReason:       datasourceTaskWaitingQueuedTarget,
		WaitingQueuedTarget: 200,
		NextRunAt:           &nextRunAt,
		Status:              "waiting",
	}}

	updated := applySemanticAggregateToDatasourceTasks(tasks, catalog.SemanticModelBackfillStatus{
		EligibleAssetCount:   1050,
		CompletedVectorCount: 1050,
		IndexedVectorCount:   1050,
		Status:               catalog.SemanticBackfillStatusReady,
	}, 1)
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range updated {
		byPhase[task.Phase] = task
	}
	searchIndex := byPhase["search_index"]
	if searchIndex.Status != "idle" ||
		searchIndex.WaitingReason != "" ||
		searchIndex.WaitingQueuedTarget != 0 ||
		searchIndex.NextRunAt != nil ||
		searchIndex.QueuedTasks != 0 ||
		searchIndex.CompletedTasks != 1050 ||
		searchIndex.TotalTasks != 1050 {
		t.Fatalf("search index task status = %+v, want stale wait cleared", searchIndex)
	}
}

func TestApplySemanticAggregateReplacesStaleEmbeddingFailureCount(t *testing.T) {
	t.Parallel()

	tasks := []DatasourceTaskStatus{{
		Phase:       "embeddings",
		Label:       "Embeddings",
		FailedTasks: 2,
		Status:      "attention",
	}}
	updated := applySemanticAggregateToDatasourceTasks(tasks, catalog.SemanticModelBackfillStatus{
		ModelID:              "model-current",
		VectorSpaceID:        "model-current/d4",
		EligibleAssetCount:   10,
		FailedVectorCount:    1,
		RemainingVectorCount: 10,
		Status:               catalog.SemanticBackfillStatusBackfilling,
	}, 1)
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range updated {
		byPhase[task.Phase] = task
	}
	embeddings := byPhase["embeddings"]
	if embeddings.QueuedTasks != 9 || embeddings.FailedTasks != 1 || embeddings.TotalTasks != 10 {
		t.Fatalf("embeddings task = %+v, want current model queued/failed/total 9/1/10", embeddings)
	}
}

func TestDatasourceSemanticTaskCountsIgnoreUnscopedLocalEmbeddingFailures(t *testing.T) {
	t.Parallel()

	var status DatasourceIndexingStatus
	status.applyLocalStatus(catalog.LocalDatasourceScanStatus{
		SourceKey:           "1111111111111111",
		FailedEmbeddingJobs: 2,
	})
	if status.FailedEmbeddingJobs != 0 {
		t.Fatalf("raw local FailedEmbeddingJobs = %d, want unscoped failures ignored", status.FailedEmbeddingJobs)
	}
	status.applySemanticBackfillStatus(catalog.SemanticModelBackfillStatus{
		ModelID:              "model-current",
		VectorSpaceID:        "model-current/d4",
		EligibleAssetCount:   10,
		FailedVectorCount:    1,
		RemainingVectorCount: 10,
		Status:               catalog.SemanticBackfillStatusBackfilling,
	})

	summary := datasourceSemanticTaskCounts([]DatasourceIndexingStatus{status})
	if summary.embeddingQueued != 9 || summary.failedEmbeddingJobs != 1 || summary.eligible != 10 {
		t.Fatalf("semantic task counts = %+v, want current model queued/failed/eligible 9/1/10", summary)
	}
}

func TestDatasourceTaskStatusesShowEmbeddingWaitingForSearchIndex(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	clearActive := runtime.setSemanticIndexPublishActive(1)
	defer clearActive()
	clearBackfillActive := runtime.setSemanticBackfillActiveWorkers(1)
	defer clearBackfillActive()
	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:          "1111111111111111",
		Name:               "NAS Photos",
		IngestionKind:      datasourceIngestionRemoteAPI,
		EmbeddingStatus:    catalog.SemanticBackfillStatusBackfilling,
		EmbeddingModelID:   "timich-siglip2-base-patch16-224-onnx-multilingual-v1",
		EmbeddingEligible:  100,
		EmbeddingCompleted: 40,
		EmbeddingIndexed:   30,
		EmbeddingRemaining: 60,
	}})
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["embeddings"]; got.Status != "waiting" || got.WaitingReason != "search_index" || got.ActiveTasks != 0 || got.QueuedTasks != 60 {
		t.Fatalf("embedding task status = %+v, want waiting for search index", got)
	}
	if got := byPhase["search_index"]; got.Status != "running" || got.ActiveTasks != 1 {
		t.Fatalf("search index task status = %+v, want running publish", got)
	}
}

func TestDatasourceTaskStatusPrefersWaitingReasonOverActiveCount(t *testing.T) {
	t.Parallel()

	task := DatasourceTaskStatus{
		Phase:         "embeddings",
		Label:         "Embeddings",
		ActiveTasks:   1,
		QueuedTasks:   60,
		WaitingReason: "search_index",
	}
	if got := datasourceTaskStatus(task); got != "waiting" {
		t.Fatalf("datasourceTaskStatus(%+v) = %q, want waiting", task, got)
	}
}

func TestDatasourceTaskSnapshotNormalizesEmbeddingDuringSearchIndexPublish(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	runtime.rememberDatasourceIndexingSnapshot(nil, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:          "embeddings",
			Label:          "Embeddings",
			ActiveTasks:    1,
			QueuedTasks:    60,
			CompletedTasks: 40,
			TotalTasks:     100,
			Status:         "running",
		}, {
			Phase:          "search_index",
			Label:          "Search index",
			ActiveTasks:    1,
			QueuedTasks:    10,
			CompletedTasks: 30,
			TotalTasks:     40,
			Status:         "running",
		}},
	})

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["embeddings"]; got.Status != "waiting" || got.WaitingReason != "search_index" || got.ActiveTasks != 0 || got.QueuedTasks != 60 {
		t.Fatalf("embedding task = %+v, want waiting for running search index", got)
	}
	if got := byPhase["search_index"]; got.Status != "running" || got.ActiveTasks != 1 {
		t.Fatalf("search index task = %+v, want running", got)
	}
}

func TestDatasourceIndexingSnapshotReturnsLastKnownStatus(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	runtime.rememberDatasourceIndexingSnapshot(nil, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:          "embeddings",
			Label:          "Embeddings",
			QueuedTasks:    42,
			CompletedTasks: 8,
			TotalTasks:     50,
			Status:         "queued",
		}},
		Datasources: []DatasourceIndexingStatus{{
			SourceKey:          "1111111111111111",
			Name:               "NAS Photos",
			IngestionKind:      datasourceIngestionFilesystem,
			Status:             "idle",
			EmbeddingStatus:    catalog.SemanticBackfillStatusBackfilling,
			EmbeddingCompleted: 8,
			EmbeddingEligible:  50,
			EmbeddingRemaining: 42,
		}},
	})

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	if !snapshot.StatusSnapshotUsed || snapshot.StatusSnapshotAt == nil {
		t.Fatalf("snapshot metadata = used:%v at:%v, want cached marker", snapshot.StatusSnapshotUsed, snapshot.StatusSnapshotAt)
	}
	if len(snapshot.Tasks) != 1 ||
		snapshot.Tasks[0].QueuedTasks != 42 ||
		snapshot.Tasks[0].QueuedTasksUnknown {
		t.Fatalf("snapshot tasks = %+v, want last known queued count", snapshot.Tasks)
	}
}

func TestDatasourceTaskStatsSnapshotUpdatesTasksWithoutLiveStatus(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	oldSnapshotAt := time.Date(2026, 7, 5, 9, 51, 23, 0, time.UTC)
	runtime.rememberDatasourceIndexingSnapshotInMemory(DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:       "phase0",
			Label:       "Media discovery",
			ActiveTasks: 1,
			Status:      "running",
		}, {
			Phase:          "embeddings",
			Label:          "Embeddings",
			QueuedTasks:    10,
			CompletedTasks: 5,
			TotalTasks:     15,
			Status:         "queued",
		}},
		Datasources: []DatasourceIndexingStatus{{
			SourceKey:     "1111111111111111",
			Name:          "Immich",
			IngestionKind: datasourceIngestionRemoteAPI,
			Status:        "busy",
		}},
	}, oldSnapshotAt)

	refreshedAt := oldSnapshotAt.Add(15 * time.Second)
	runtime.rememberDatasourceTaskStatsSnapshot(context.Background(), nil, catalog.AssetProcessingStatsSnapshot{
		RefreshedAt: refreshedAt,
		Stats: []catalog.AssetProcessingStat{{
			ScopeKey:   catalog.AssetProcessingScopeAll,
			Stage:      catalog.AssetProcessingStageEmbeddings,
			Status:     catalog.AssetProcessingStatusPending,
			Count:      30,
			TotalCount: 100,
		}, {
			ScopeKey:   catalog.AssetProcessingScopeAll,
			Stage:      catalog.AssetProcessingStageEmbeddings,
			Status:     catalog.AssetProcessingStatusReady,
			Count:      70,
			TotalCount: 100,
		}, {
			ScopeKey:   catalog.AssetProcessingScopeAll,
			Stage:      catalog.AssetProcessingStageSearchIndex,
			Status:     catalog.AssetProcessingStatusPending,
			Count:      20,
			TotalCount: 70,
		}, {
			ScopeKey:   catalog.AssetProcessingScopeAll,
			Stage:      catalog.AssetProcessingStageSearchIndex,
			Status:     catalog.AssetProcessingStatusReady,
			Count:      50,
			TotalCount: 70,
		}, {
			ScopeKey:   "1111111111111111",
			Stage:      catalog.AssetProcessingStageFoundMedias,
			Status:     catalog.AssetProcessingStatusReady,
			Count:      120,
			TotalCount: 120,
		}, {
			ScopeKey:   "1111111111111111",
			Stage:      catalog.AssetProcessingStageBrowsable,
			Status:     catalog.AssetProcessingStatusReady,
			Count:      100,
			TotalCount: 120,
		}, {
			ScopeKey:   "1111111111111111",
			Stage:      catalog.AssetProcessingStageSearchable,
			Status:     catalog.AssetProcessingStatusReady,
			Count:      50,
			TotalCount: 100,
		}, {
			ScopeKey:   "1111111111111111",
			Stage:      catalog.AssetProcessingStageIssues,
			Status:     catalog.AssetProcessingStatusReady,
			Count:      3,
			TotalCount: 120,
		}},
	})

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	if snapshot.StatusSnapshotAt == nil || !snapshot.StatusSnapshotAt.Equal(refreshedAt) {
		t.Fatalf("snapshot time = %v, want %v", snapshot.StatusSnapshotAt, refreshedAt)
	}
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["phase0"]; got.ActiveTasks != 0 || got.Status != "idle" {
		t.Fatalf("phase0 task = %+v, want stale running state cleared", got)
	}
	if got := byPhase["embeddings"]; got.QueuedTasks != 30 || got.CompletedTasks != 70 || got.TotalTasks != 100 {
		t.Fatalf("embeddings task = %+v, want stats-derived counts", got)
	}
	if got := byPhase["search_index"]; got.QueuedTasks != 20 || got.CompletedTasks != 50 || got.TotalTasks != 70 {
		t.Fatalf("search index task = %+v, want stats-derived counts", got)
	}
	if len(snapshot.Datasources) != 1 || snapshot.Datasources[0].Coverage == nil {
		t.Fatalf("datasources = %+v, want coverage from stats snapshot", snapshot.Datasources)
	}
	coverage := snapshot.Datasources[0].Coverage
	if coverage.FoundMedias.Count != 120 ||
		coverage.BrowsableMedias.Count != 100 ||
		coverage.SearchableMedias.Count != 50 ||
		coverage.Issues.Count != 3 {
		t.Fatalf("coverage = %+v, want stats-derived datasource coverage", coverage)
	}
}

func TestDatasourceTaskStatsSnapshotRejectsDifferentSemanticProfile(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
	})
	currentPack := runtimeSemanticPackForTest("current-stats-model")
	installStoredRuntimeSemanticPackForTest(t, runtime, currentPack)
	catalogService := runtime.catalogService()
	if catalogService == nil {
		t.Fatal("catalogService() = nil")
	}
	runtime.rememberDatasourceIndexingSnapshot(catalogService, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:  "embeddings",
			Label:  "Embeddings",
			Status: "idle",
		}},
		Datasources: []DatasourceIndexingStatus{{
			SourceKey:     "1111111111111111",
			Name:          "NAS Photos",
			IngestionKind: datasourceIngestionFilesystem,
			Status:        "idle",
			Coverage: &DatasourceCoverage{
				SearchableMedias: DatasourceCoverageMetric{Status: catalog.AssetProcessingStatusReady},
			},
		}},
	})

	oldVariant := assetProcessingSemanticVariantForRuntimeTest("old-stats-model", "old-stats-model/d4")
	oldStats := catalog.AssetProcessingStatsSnapshot{
		RefreshedAt: time.Now().UTC(),
		Stats: []catalog.AssetProcessingStat{{
			ScopeKey:   catalog.AssetProcessingScopeAll,
			Stage:      catalog.AssetProcessingStageEmbeddings,
			Variant:    oldVariant,
			Status:     catalog.AssetProcessingStatusFailed,
			Count:      1,
			TotalCount: 1,
		}, {
			ScopeKey: catalog.AssetProcessingScopeAll,
			Stage:    catalog.AssetProcessingStageSearchIndex,
			Variant:  oldVariant,
			Status:   catalog.AssetProcessingStatusReady,
		}, {
			ScopeKey:   "1111111111111111",
			Stage:      catalog.AssetProcessingStageSearchable,
			Variant:    oldVariant,
			Status:     catalog.AssetProcessingStatusReady,
			Count:      1,
			TotalCount: 1,
		}},
	}
	if runtime.assetProcessingStatsMatchCurrentSemanticProfile(oldStats) {
		t.Fatal("assetProcessingStatsMatchCurrentSemanticProfile(old stats) = true, want false")
	}
	runtime.rememberDatasourceTaskStatsSnapshot(context.Background(), catalogService, oldStats)

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), catalogService)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want baseline snapshot")
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].FailedTasks != 0 || snapshot.Tasks[0].Status != "idle" {
		t.Fatalf("tasks = %+v, want old-profile failures ignored", snapshot.Tasks)
	}
	if len(snapshot.Datasources) != 1 || snapshot.Datasources[0].Coverage == nil || snapshot.Datasources[0].Coverage.SearchableMedias.Count != 0 {
		t.Fatalf("datasources = %+v, want old-profile searchable coverage ignored", snapshot.Datasources)
	}
}

func TestCachedAssetProcessingStatsRejectsDifferentSemanticProfile(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
	})
	catalogService := runtime.catalogService()
	if catalogService == nil {
		t.Fatal("catalogService() = nil")
	}
	oldProfile := catalog.SemanticModelProfileStatus{
		ModelID:       "cached-old-model",
		VectorSpaceID: "cached-old-model/d4",
		EmbeddingDim:  4,
		ProfileKind:   catalog.SemanticProfileKindModelPack,
		InputKind:     catalog.SemanticInputKindImage,
	}
	oldStats, err := catalogService.RefreshAssetProcessingStats(context.Background(), &oldProfile, 0)
	if err != nil {
		t.Fatalf("RefreshAssetProcessingStats(old profile) error = %v", err)
	}
	if oldStats.Empty() || !oldStats.MatchesSemanticProfile(&oldProfile) {
		t.Fatalf("old stats = %+v, want persisted old-profile semantic rows", oldStats)
	}

	currentPack := runtimeSemanticPackForTest("cached-current-model")
	installStoredRuntimeSemanticPackForTest(t, runtime, currentPack)
	if got := runtime.cachedAssetProcessingStatsForAdmin(context.Background(), catalogService); !got.Empty() {
		t.Fatalf("cachedAssetProcessingStatsForAdmin() = %+v, want old-profile snapshot rejected", got)
	}
}

func TestDatasourceTaskStatsSnapshotMarksUncountedDatasourceIndexing(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "Local",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "local",
	}}, "test-admin-token")
	refreshedAt := time.Date(2026, 7, 6, 10, 30, 0, 0, time.UTC)
	runtime.rememberDatasourceTaskStatsSnapshot(context.Background(), nil, catalog.AssetProcessingStatsSnapshot{
		RefreshedAt: refreshedAt,
		Stats: []catalog.AssetProcessingStat{{
			ScopeKey:   catalog.AssetProcessingScopeAll,
			Stage:      catalog.AssetProcessingStageMetadata,
			Status:     catalog.AssetProcessingStatusPending,
			Count:      343714,
			TotalCount: 343714,
		}},
	})

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	if len(snapshot.Datasources) != 1 {
		t.Fatalf("datasources = %d, want 1", len(snapshot.Datasources))
	}
	datasource := snapshot.Datasources[0]
	if datasource.Status != "indexing" {
		t.Fatalf("datasource status = %q, want indexing", datasource.Status)
	}
	if datasource.ActiveAssets != 0 || datasource.ActiveLocations != 0 {
		t.Fatalf("datasource counts = assets:%d locations:%d, want unassigned aggregate counts", datasource.ActiveAssets, datasource.ActiveLocations)
	}
}

func TestAggregateProcessingStatsMarkIdleDatasourceIndexing(t *testing.T) {
	t.Parallel()

	refreshedAt := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	datasources := []DatasourceIndexingStatus{{
		SourceKey:       "source",
		Kind:            config.DatasourceKindImmichIndexed,
		Status:          "idle",
		IndexingEnabled: true,
	}}
	got := applyAggregateProcessingStatsToDatasourceStatus(datasources, catalog.AssetProcessingStatsSnapshot{
		RefreshedAt: refreshedAt,
		Stats: []catalog.AssetProcessingStat{{
			ScopeKey:    catalog.AssetProcessingScopeAll,
			Stage:       catalog.AssetProcessingStageMetadata,
			Status:      catalog.AssetProcessingStatusPending,
			Count:       343714,
			TotalCount:  343714,
			RefreshedAt: refreshedAt,
		}},
	})

	if got[0].Status != "indexing" {
		t.Fatalf("datasource status = %q, want indexing", got[0].Status)
	}
}

func TestDatasourceIndexingSnapshotAppliesPausedWorkerState(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(0)
	})
	runtime.rememberDatasourceIndexingSnapshot(nil, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:          "embeddings",
			Label:          "Embeddings",
			QueuedTasks:    42,
			CompletedTasks: 8,
			TotalTasks:     50,
			Status:         "queued",
		}},
	})

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	if len(snapshot.Tasks) != 1 {
		t.Fatalf("snapshot tasks = %+v, want one task", snapshot.Tasks)
	}
	if got := snapshot.Tasks[0]; got.Status != "paused" || got.WaitingReason != datasourceTaskWaitingPaused || got.ActiveTasks != 0 {
		t.Fatalf("snapshot task = %+v, want paused worker state", got)
	}
}

func TestDatasourceIndexingSnapshotStoresBestEffortBusyStatus(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	runtime.rememberDatasourceIndexingSnapshot(nil, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:              "embeddings",
			Label:              "Embeddings",
			QueuedTasksUnknown: true,
			Status:             "busy",
		}},
		Datasources: []DatasourceIndexingStatus{{
			SourceKey:     "1111111111111111",
			Name:          "Immich",
			IngestionKind: datasourceIngestionRemoteAPI,
			Status:        "busy",
			LastError:     "context deadline exceeded",
		}},
	})

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	if len(snapshot.Datasources) != 1 || snapshot.Datasources[0].Status != "busy" {
		t.Fatalf("datasources = %+v, want best-effort busy datasource status", snapshot.Datasources)
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].Status != "busy" || !snapshot.Tasks[0].QueuedTasksUnknown {
		t.Fatalf("tasks = %+v, want best-effort busy task status", snapshot.Tasks)
	}
}

func TestSemanticModelRegistrySnapshotReturnsLastKnownStatus(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	runtime.RememberSemanticModelRegistryStatus(catalog.SemanticModelRegistryStatus{
		RegistryStatus: "available",
		Profiles: []catalog.SemanticModelProfileStatus{{
			ModelID:       "cached-model",
			VectorSpaceID: "cached-model/d768",
			EmbeddingDim:  768,
			ProfileKind:   "modelPack",
			InputKind:     "image",
		}},
	})

	snapshot, ok := runtime.CachedSemanticModelRegistryStatus(context.Background())
	if !ok {
		t.Fatal("CachedSemanticModelRegistryStatus() ok = false, want true")
	}
	if snapshot.RegistryStatus != "available" ||
		len(snapshot.Profiles) != 1 ||
		snapshot.Profiles[0].ModelID != "cached-model" {
		t.Fatalf("semantic model snapshot = %+v, want cached model status", snapshot)
	}
}

func TestSemanticProgressUpdatesDatasourceIndexingSnapshot(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	runtime.rememberDatasourceIndexingSnapshot(nil, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:          "embeddings",
			Label:          "Embeddings",
			QueuedTasks:    80,
			CompletedTasks: 20,
			TotalTasks:     100,
			Status:         "queued",
		}, {
			Phase:          "search_index",
			Label:          "Search index",
			QueuedTasks:    20,
			CompletedTasks: 0,
			TotalTasks:     20,
			Status:         "queued",
		}},
		Datasources: []DatasourceIndexingStatus{{
			SourceKey:          "1111111111111111",
			Name:               "NAS Photos",
			IngestionKind:      datasourceIngestionRemoteAPI,
			Status:             "idle",
			EmbeddingStatus:    catalog.SemanticBackfillStatusBackfilling,
			EmbeddingCompleted: 20,
			EmbeddingIndexed:   0,
			EmbeddingEligible:  100,
			EmbeddingRemaining: 80,
		}},
	})

	progress := catalog.SemanticModelBackfillStatus{
		Status:               catalog.SemanticBackfillStatusBackfilling,
		ModelID:              "timich-siglip2-base-patch16-224-onnx-multilingual-v1",
		VectorSpaceID:        "timich-siglip2-base-patch16-224-onnx-multilingual-v1/d768",
		EligibleAssetCount:   100,
		CompletedVectorCount: 60,
		IndexedVectorCount:   50,
		RemainingVectorCount: 40,
	}
	runtime.rememberSemanticIndexingProgressSnapshot(nil, []catalog.SemanticBackfillSource{{
		SourceKey: "1111111111111111",
		Status:    progress,
	}}, progress)

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["embeddings"]; got.QueuedTasks != 40 || got.CompletedTasks != 60 || got.TotalTasks != 100 || got.Status != "queued" {
		t.Fatalf("embedding task = %+v, want updated semantic progress", got)
	}
	if got := byPhase["search_index"]; got.QueuedTasks != 10 || got.CompletedTasks != 50 || got.TotalTasks != 60 || got.Status != "queued" {
		t.Fatalf("search index task = %+v, want updated semantic progress", got)
	}
	if len(snapshot.Datasources) != 1 ||
		snapshot.Datasources[0].EmbeddingCompleted != 60 ||
		snapshot.Datasources[0].EmbeddingIndexed != 50 {
		t.Fatalf("datasources = %+v, want updated semantic datasource coverage", snapshot.Datasources)
	}
}

func TestSemanticProgressSnapshotAggregatesAllDatasources(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	runtime.rememberDatasourceIndexingSnapshot(nil, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{
			{Phase: "embeddings", Label: "Embeddings", CompletedTasks: 60000, TotalTasks: 344000, Status: "queued"},
			{Phase: "search_index", Label: "Search index", CompletedTasks: 58000, TotalTasks: 60000, Status: "queued"},
		},
		Datasources: []DatasourceIndexingStatus{{
			SourceKey:          "1111111111111111",
			Name:               "Immich",
			IngestionKind:      datasourceIngestionRemoteAPI,
			Status:             "idle",
			EmbeddingStatus:    catalog.SemanticBackfillStatusBackfilling,
			EmbeddingCompleted: 60000,
			EmbeddingIndexed:   58000,
			EmbeddingEligible:  344000,
			EmbeddingRemaining: 284000,
		}, {
			SourceKey:          "2222222222222222",
			Name:               "Local smoke",
			IngestionKind:      datasourceIngestionFilesystem,
			Status:             "idle",
			EmbeddingStatus:    catalog.SemanticBackfillStatusBackfilling,
			EmbeddingCompleted: 10,
			EmbeddingIndexed:   10,
			EmbeddingEligible:  12,
			EmbeddingRemaining: 2,
		}},
	})

	localProgress := catalog.SemanticModelBackfillStatus{
		Status:               catalog.SemanticBackfillStatusReady,
		ModelID:              "timich-siglip2-base-patch16-224-onnx-multilingual-v1",
		VectorSpaceID:        "timich-siglip2-base-patch16-224-onnx-multilingual-v1/d768",
		EligibleAssetCount:   12,
		CompletedVectorCount: 12,
		IndexedVectorCount:   12,
	}
	runtime.rememberSemanticIndexingProgressSnapshot(nil, []catalog.SemanticBackfillSource{{
		SourceKey: "2222222222222222",
		Status:    localProgress,
	}}, localProgress)

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["embeddings"]; got.CompletedTasks != 60012 || got.TotalTasks != 344012 || got.QueuedTasks != 284000 {
		t.Fatalf("embedding task = %+v, want aggregate progress across Immich and local datasources", got)
	}
	if got := byPhase["search_index"]; got.CompletedTasks != 58012 || got.TotalTasks != 60012 || got.QueuedTasks != 2000 {
		t.Fatalf("search index task = %+v, want aggregate index progress across Immich and local datasources", got)
	}
	if len(snapshot.Datasources) != 2 || snapshot.Datasources[1].EmbeddingCompleted != 12 || snapshot.Datasources[1].EmbeddingIndexed != 12 {
		t.Fatalf("datasources = %+v, want local datasource row updated", snapshot.Datasources)
	}
}

func TestDatasourceTaskActivitySnapshotMarksEmbeddingStart(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	runtime.rememberDatasourceIndexingSnapshot(nil, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:          "embeddings",
			Label:          "Embeddings",
			QueuedTasks:    20,
			CompletedTasks: 80,
			TotalTasks:     100,
			Status:         "queued",
		}},
	})

	runtime.rememberDatasourceTaskActivitySnapshot(nil, "embeddings", 1)
	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["embeddings"]; got.ActiveTasks != 1 || got.Status != "running" || got.QueuedTasks != 20 {
		t.Fatalf("embedding task after start = %+v, want running with preserved queue", got)
	}

	runtime.rememberDatasourceTaskActivitySnapshot(nil, "embeddings", 0)
	snapshot, ok = runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() after stop ok = false, want true")
	}
	byPhase = map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["embeddings"]; got.ActiveTasks != 0 || got.Status != "queued" || got.QueuedTasks != 20 {
		t.Fatalf("embedding task after stop = %+v, want queued with preserved queue", got)
	}
}

func TestSemanticProgressSnapshotPreservesActiveEmbedding(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	runtime.rememberDatasourceIndexingSnapshot(nil, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:          "embeddings",
			Label:          "Embeddings",
			ActiveTasks:    1,
			QueuedTasks:    20,
			CompletedTasks: 80,
			TotalTasks:     100,
			Status:         "running",
		}, {
			Phase:          "search_index",
			Label:          "Search index",
			QueuedTasks:    10,
			CompletedTasks: 70,
			TotalTasks:     80,
			Status:         "queued",
		}},
	})

	progress := catalog.SemanticModelBackfillStatus{
		Status:               catalog.SemanticBackfillStatusBackfilling,
		ModelID:              "timich-siglip2-base-patch16-224-onnx-multilingual-v1",
		VectorSpaceID:        "timich-siglip2-base-patch16-224-onnx-multilingual-v1/d768",
		EligibleAssetCount:   100,
		CompletedVectorCount: 80,
		IndexedVectorCount:   70,
		RemainingVectorCount: 20,
	}
	runtime.rememberSemanticIndexingProgressSnapshot(nil, nil, progress)

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["embeddings"]; got.ActiveTasks != 1 || got.Status != "running" || got.QueuedTasks != 20 {
		t.Fatalf("embedding task = %+v, want progress update to preserve running activity", got)
	}
	if got := byPhase["search_index"]; got.ActiveTasks != 0 ||
		got.Status != "waiting" ||
		got.WaitingReason != datasourceTaskWaitingQueuedTarget ||
		got.WaitingQueuedTarget != 14 ||
		got.QueuedTasks != 10 {
		t.Fatalf("search index task = %+v, want progress update to expose 20%% publish wait", got)
	}
}

func TestDatasourceTaskActivitySnapshotMarksEmbeddingWaitingDuringSearchIndex(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	runtime.rememberDatasourceIndexingSnapshot(nil, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:          "embeddings",
			Label:          "Embeddings",
			QueuedTasks:    20,
			CompletedTasks: 80,
			TotalTasks:     100,
			Status:         "queued",
		}, {
			Phase:          "search_index",
			Label:          "Search index",
			QueuedTasks:    10,
			CompletedTasks: 70,
			TotalTasks:     80,
			Status:         "queued",
		}},
	})

	runtime.rememberDatasourceTaskActivitySnapshot(nil, "search_index", 1)
	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["search_index"]; got.ActiveTasks != 1 || got.Status != "running" {
		t.Fatalf("search index task = %+v, want running", got)
	}
	if got := byPhase["embeddings"]; got.ActiveTasks != 0 || got.Status != "waiting" || got.WaitingReason != datasourceTaskWaitingSearchIndex {
		t.Fatalf("embedding task = %+v, want waiting for search index", got)
	}
}

func TestSemanticBackfillActivitySnapshotUsesSearchIndexForPublishOnly(t *testing.T) {
	t.Parallel()

	phase, active := semanticBackfillActivitySnapshot(0, catalog.SemanticModelBackfillOptions{
		DrainIndexJobs: true,
		Workers:        3,
	})
	if phase != "search_index" || active != 1 {
		t.Fatalf("semanticBackfillActivitySnapshot(publish-only) = %q/%d, want search_index/1", phase, active)
	}

	phase, active = semanticBackfillActivitySnapshot(2, catalog.SemanticModelBackfillOptions{
		DrainIndexJobs: true,
		Workers:        3,
	})
	if phase != "embeddings" || active != 2 {
		t.Fatalf("semanticBackfillActivitySnapshot(vector batch) = %q/%d, want embeddings/2", phase, active)
	}
}

func TestDatasourceIndexingSnapshotDoesNotHideNonTimeoutErrors(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	runtime.rememberDatasourceIndexingSnapshot(nil, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:       "embeddings",
			Label:       "Embeddings",
			QueuedTasks: 42,
			Status:      "queued",
		}},
	})

	_, err := runtime.refreshDatasourceIndexingSnapshot(context.Background(), nil)
	if !errors.Is(err, catalog.ErrNoDatasourceConfigured) {
		t.Fatalf("refreshDatasourceIndexingSnapshot() error = %v, want ErrNoDatasourceConfigured", err)
	}
	if snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil); ok {
		t.Fatalf("datasourceIndexingSnapshot() = %+v, want invalidated after non-timeout error", snapshot)
	}
}

func TestDatasourceIndexingSnapshotInvalidationDeletesPersistedSnapshot(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.Datasources = []config.DatasourceConfig{{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
		}}
	})
	catalogService := runtime.catalogService()
	if catalogService == nil {
		t.Fatal("catalogService() = nil")
	}
	runtime.rememberDatasourceIndexingSnapshot(catalogService, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:       "embeddings",
			Label:       "Embeddings",
			QueuedTasks: 42,
			Status:      "queued",
		}},
	})

	ctx := context.Background()
	if _, ok, err := catalogService.AdminStatusSnapshot(ctx, datasourceIndexingStatusSnapshotKey); err != nil || !ok {
		t.Fatalf("AdminStatusSnapshot() before invalidate ok=%v err=%v, want persisted snapshot", ok, err)
	}
	runtime.invalidateDatasourceIndexingSnapshot(catalogService)
	if snapshot, ok, err := catalogService.AdminStatusSnapshot(ctx, datasourceIndexingStatusSnapshotKey); err != nil || ok {
		t.Fatalf("AdminStatusSnapshot() after invalidate = snapshot:%+v ok:%v err:%v, want deleted", snapshot, ok, err)
	}
}

func TestDatasourceIndexingSnapshotClearsPersistedProcessActivity(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.Datasources = []config.DatasourceConfig{{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
		}}
	})
	catalogService := runtime.catalogService()
	if catalogService == nil {
		t.Fatal("catalogService() = nil")
	}
	runtime.rememberDatasourceIndexingSnapshot(catalogService, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:       "phase0",
			Label:       "Media discovery",
			ActiveTasks: 1,
			Status:      "running",
		}, {
			Phase:          "embeddings",
			Label:          "Embeddings",
			ActiveTasks:    1,
			QueuedTasks:    42,
			CompletedTasks: 8,
			TotalTasks:     50,
			Status:         "running",
		}},
		Datasources: []DatasourceIndexingStatus{{
			SourceKey:            "1111111111111111",
			Name:                 "NAS Photos",
			IngestionKind:        datasourceIngestionFilesystem,
			Status:               "running",
			RunningPhase0Scans:   1,
			RunningMetadataJobs:  1,
			RunningThumbnailJobs: 1,
		}},
	})

	runtime.datasourceTaskMu.Lock()
	runtime.datasourceSnapshot = nil
	runtime.datasourceSnapshotAt = time.Time{}
	runtime.datasourceSnapshotHash = ""
	runtime.datasourceSnapshotInvalid = false
	runtime.datasourceTaskMu.Unlock()

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), catalogService)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want persisted snapshot")
	}
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["phase0"]; got.ActiveTasks != 0 || got.Status != "idle" {
		t.Fatalf("phase0 task = %+v, want persisted process activity cleared", got)
	}
	if got := byPhase["embeddings"]; got.ActiveTasks != 0 || got.Status != "queued" || got.QueuedTasks != 42 {
		t.Fatalf("embedding task = %+v, want queued remaining work without stale active worker", got)
	}
	if len(snapshot.Datasources) != 1 ||
		snapshot.Datasources[0].Status == "running" ||
		snapshot.Datasources[0].RunningPhase0Scans != 0 ||
		snapshot.Datasources[0].RunningMetadataJobs != 0 ||
		snapshot.Datasources[0].RunningThumbnailJobs != 0 {
		t.Fatalf("datasources = %+v, want persisted process activity cleared", snapshot.Datasources)
	}
}

func TestDatasourceIndexingSnapshotPersistsOnlyDurableTaskState(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.Datasources = []config.DatasourceConfig{{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
		}}
	})
	catalogService := runtime.catalogService()
	if catalogService == nil {
		t.Fatal("catalogService() = nil")
	}
	runtime.rememberDatasourceIndexingSnapshot(catalogService, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:       "phase0",
			Label:       "Media discovery",
			ActiveTasks: 1,
			Status:      "running",
		}, {
			Phase:          "embeddings",
			Label:          "Embeddings",
			ActiveTasks:    1,
			QueuedTasks:    42,
			CompletedTasks: 8,
			TotalTasks:     50,
			Status:         "running",
		}},
		Datasources: []DatasourceIndexingStatus{{
			SourceKey:            "1111111111111111",
			Name:                 "NAS Photos",
			IngestionKind:        datasourceIngestionFilesystem,
			Status:               "running",
			RunningPhase0Scans:   1,
			RunningMetadataJobs:  1,
			RunningThumbnailJobs: 1,
		}},
	})

	liveSnapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), catalogService)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want in-memory snapshot")
	}
	liveByPhase := map[string]DatasourceTaskStatus{}
	for _, task := range liveSnapshot.Tasks {
		liveByPhase[task.Phase] = task
	}
	if got := liveByPhase["phase0"]; got.ActiveTasks != 1 || got.Status != "running" {
		t.Fatalf("live phase0 task = %+v, want in-memory process activity preserved", got)
	}

	stored, ok, err := catalogService.AdminStatusSnapshot(context.Background(), datasourceIndexingStatusSnapshotKey)
	if err != nil || !ok {
		t.Fatalf("AdminStatusSnapshot() ok=%v err=%v, want persisted snapshot", ok, err)
	}
	var payload datasourceIndexingSnapshotPayload
	if err := json.Unmarshal(stored.Payload, &payload); err != nil {
		t.Fatalf("unmarshal persisted payload: %v", err)
	}
	if payload.Response.StatusSnapshotAt != nil || payload.Response.StatusSnapshotUsed {
		t.Fatalf("persisted response snapshot markers = at:%v used:%v, want unset", payload.Response.StatusSnapshotAt, payload.Response.StatusSnapshotUsed)
	}
	persistedByPhase := map[string]DatasourceTaskStatus{}
	for _, task := range payload.Response.Tasks {
		persistedByPhase[task.Phase] = task
	}
	if got := persistedByPhase["phase0"]; got.ActiveTasks != 0 || got.Status != "idle" {
		t.Fatalf("persisted phase0 task = %+v, want process activity omitted", got)
	}
	if got := persistedByPhase["embeddings"]; got.ActiveTasks != 0 || got.Status != "queued" || got.QueuedTasks != 42 {
		t.Fatalf("persisted embedding task = %+v, want queued durable work without active worker", got)
	}
	if len(payload.Response.Datasources) != 1 ||
		payload.Response.Datasources[0].Status == "running" ||
		payload.Response.Datasources[0].RunningPhase0Scans != 0 ||
		payload.Response.Datasources[0].RunningMetadataJobs != 0 ||
		payload.Response.Datasources[0].RunningThumbnailJobs != 0 {
		t.Fatalf("persisted datasources = %+v, want process activity omitted", payload.Response.Datasources)
	}
}

func TestDatasourceIndexingSnapshotRejectsPersistedConfigMismatch(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.Datasources = []config.DatasourceConfig{{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
		}}
	})
	catalogService := runtime.catalogService()
	if catalogService == nil {
		t.Fatal("catalogService() = nil")
	}
	runtime.rememberDatasourceIndexingSnapshot(catalogService, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:       "embeddings",
			Label:       "Embeddings",
			QueuedTasks: 42,
			Status:      "queued",
		}},
	})

	runtime.datasourceTaskMu.Lock()
	runtime.datasourceSnapshot = nil
	runtime.datasourceSnapshotAt = time.Time{}
	runtime.datasourceSnapshotHash = ""
	runtime.datasourceSnapshotInvalid = false
	runtime.datasourceTaskMu.Unlock()

	runtime.mu.Lock()
	runtime.config.LocalMediaRoots = []config.LocalMediaRootConfig{{
		Key:  "other-root",
		Path: t.TempDir(),
	}}
	runtime.config.Datasources[0].RootKey = "other-root"
	runtime.mu.Unlock()

	ctx := context.Background()
	if snapshot, ok := runtime.datasourceIndexingSnapshot(ctx, catalogService); ok {
		t.Fatalf("datasourceIndexingSnapshot() = %+v, want no stale snapshot after config mismatch", snapshot)
	}
	if snapshot, ok, err := catalogService.AdminStatusSnapshot(ctx, datasourceIndexingStatusSnapshotKey); err != nil || ok {
		t.Fatalf("AdminStatusSnapshot() after mismatch = snapshot:%+v ok:%v err:%v, want deleted", snapshot, ok, err)
	}
}

func TestDatasourceIndexingSnapshotRejectsPersistedSemanticProfileMismatch(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.Datasources = []config.DatasourceConfig{{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
		}}
	})
	catalogService := runtime.catalogService()
	if catalogService == nil {
		t.Fatal("catalogService() = nil")
	}
	oldPack := runtimeSemanticPackForTest("snapshot-old-model")
	installStoredRuntimeSemanticPackForTest(t, runtime, oldPack)
	if _, err := runtime.semanticModels.ActivatePack(oldPack.ID, oldPack.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack(old) error = %v", err)
	}
	oldHash := runtime.datasourceIndexingConfigHash()
	runtime.rememberDatasourceIndexingSnapshot(catalogService, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{{
			Phase:       "embeddings",
			Label:       "Embeddings",
			FailedTasks: 1,
			Status:      "attention",
		}},
	})

	newPack := runtimeSemanticPackForTest("snapshot-new-model")
	installStoredRuntimeSemanticPackForTest(t, runtime, newPack)
	newHash := runtime.datasourceIndexingConfigHash()
	if oldHash == newHash {
		t.Fatalf("datasourceIndexingConfigHash() did not change across profiles: %q", oldHash)
	}
	runtime.datasourceTaskMu.Lock()
	runtime.datasourceSnapshot = nil
	runtime.datasourceSnapshotAt = time.Time{}
	runtime.datasourceSnapshotHash = ""
	runtime.datasourceSnapshotInvalid = false
	runtime.datasourceTaskMu.Unlock()

	ctx := context.Background()
	if snapshot, ok := runtime.datasourceIndexingSnapshot(ctx, catalogService); ok {
		t.Fatalf("datasourceIndexingSnapshot() = %+v, want old-profile snapshot rejected", snapshot)
	}
	if snapshot, ok, err := catalogService.AdminStatusSnapshot(ctx, datasourceIndexingStatusSnapshotKey); err != nil || ok {
		t.Fatalf("AdminStatusSnapshot() after profile mismatch = snapshot:%+v ok:%v err:%v, want deleted", snapshot, ok, err)
	}
}

func TestDatasourceIndexingSnapshotDoesNotRetagResponseAfterSemanticProfileChange(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.Datasources = []config.DatasourceConfig{{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
		}}
	})
	catalogService := runtime.catalogService()
	if catalogService == nil {
		t.Fatal("catalogService() = nil")
	}
	oldPack := runtimeSemanticPackForTest("snapshot-build-old-model")
	installStoredRuntimeSemanticPackForTest(t, runtime, oldPack)
	if _, err := runtime.semanticModels.ActivatePack(oldPack.ID, oldPack.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack(old) error = %v", err)
	}

	response, err := runtime.buildDatasourceIndexingStatus(context.Background(), catalogService)
	if err != nil {
		t.Fatalf("buildDatasourceIndexingStatus(old profile) error = %v", err)
	}
	response.Tasks = []DatasourceTaskStatus{{
		Phase:       "embeddings",
		Label:       "Embeddings",
		FailedTasks: 1,
		Status:      "attention",
	}}
	buildHash := response.snapshotConfigHash
	if buildHash == "" {
		t.Fatal("response snapshotConfigHash is empty")
	}

	newPack := runtimeSemanticPackForTest("snapshot-build-new-model")
	installStoredRuntimeSemanticPackForTest(t, runtime, newPack)
	currentHash := runtime.datasourceIndexingConfigHash()
	if currentHash == buildHash {
		t.Fatalf("datasourceIndexingConfigHash() = build hash %q after profile change", buildHash)
	}
	if runtime.rememberDatasourceIndexingSnapshot(catalogService, response) {
		t.Fatal("rememberDatasourceIndexingSnapshot(old response) = true, want rejected")
	}
	if snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), catalogService); ok {
		t.Fatalf("datasourceIndexingSnapshot() = %+v, want no retagged old-profile snapshot", snapshot)
	}
	if snapshot, ok, err := catalogService.AdminStatusSnapshot(context.Background(), datasourceIndexingStatusSnapshotKey); err != nil || ok {
		t.Fatalf("AdminStatusSnapshot() = snapshot:%+v ok:%v err:%v, want no persisted old response", snapshot, ok, err)
	}
}

func TestDatasourceIndexingSnapshotFallbackOnlyAllowsTimeouts(t *testing.T) {
	t.Parallel()

	if !datasourceIndexingSnapshotFallbackAllowed(context.DeadlineExceeded) {
		t.Fatal("datasourceIndexingSnapshotFallbackAllowed(deadline) = false, want true")
	}
	if !datasourceIndexingSnapshotFallbackAllowed(context.Canceled) {
		t.Fatal("datasourceIndexingSnapshotFallbackAllowed(canceled) = false, want true")
	}
	if datasourceIndexingSnapshotFallbackAllowed(catalog.ErrNoDatasourceConfigured) {
		t.Fatal("datasourceIndexingSnapshotFallbackAllowed(non-timeout) = true, want false")
	}
}

func TestDatasourceTaskStatusTreatsUnknownRemainingAsBusy(t *testing.T) {
	t.Parallel()

	tests := []DatasourceTaskStatus{
		{
			Phase:              "embeddings",
			Label:              "Embeddings",
			QueuedTasksUnknown: true,
		},
		{
			Phase:              "search_index",
			Label:              "Search index",
			FailedTasksUnknown: true,
		},
	}
	for _, task := range tests {
		if status := datasourceTaskStatus(task); status != "busy" {
			t.Fatalf("datasourceTaskStatus(%+v) = %q, want busy", task, status)
		}
	}
}

func TestDatasourceTaskStatusesIncludeRemoteDiscoveryActive(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	clearActive := runtime.setDatasourceDiscoveryActive(1)
	defer clearActive()

	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:       "1111111111111111",
		Name:            "Immich",
		IngestionKind:   datasourceIngestionRemoteAPI,
		IndexingEnabled: true,
		Status:          "idle",
	}})
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	if byPhase["phase0"].ActiveTasks != 1 || byPhase["phase0"].Status != "running" {
		t.Fatalf("media discovery task = %+v, want one running remote discovery", byPhase["phase0"])
	}
}

func TestDatasourceTaskStatusesShowLatestContentVerificationResultWithoutQueue(t *testing.T) {
	t.Parallel()

	lastRun := time.Now().UTC().Add(-time.Hour)
	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
	}}, "test-admin-token")
	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:                         "1111111111111111",
		Name:                              "NAS Photos",
		IngestionKind:                     datasourceIngestionFilesystem,
		LastContentVerificationAt:         &lastRun,
		ContentVerificationStatus:         catalog.LocalContentVerificationStatusCompleted,
		ContentVerificationProcessedFiles: 7,
		ContentVerificationVerifiedFiles:  6,
		ContentVerificationChangedFiles:   1,
		ContentVerificationReadBytes:      12345,
	}})
	for index, task := range tasks {
		if task.Phase != "content_verification" {
			continue
		}
		if index != len(tasks)-1 {
			t.Fatalf("content verification task index = %d, want last task", index)
		}
		if task.QueuedTasks != 0 ||
			task.Status != "idle" ||
			task.LastRunStatus != catalog.LocalContentVerificationStatusCompleted ||
			task.LastProcessedFiles != 7 ||
			task.LastVerifiedFiles != 6 ||
			task.LastChangedFiles != 1 ||
			task.LastReadBytes != 12345 {
			t.Fatalf("content verification task = %+v, want latest result without queue count", task)
		}
		return
	}
	t.Fatal("content verification task is missing")
}

func TestDatasourceTaskStatusesShowZeroDurationContentVerificationDisabled(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		Scan: &config.LocalDatasourceScanConfig{
			ContentVerificationDuration: "0",
		},
	}}, "test-admin-token")
	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:     "1111111111111111",
		Name:          "NAS Photos",
		IngestionKind: datasourceIngestionFilesystem,
	}})
	for _, task := range tasks {
		if task.Phase == "content_verification" {
			if task.Status != "disabled" || task.QueuedTasks != 0 {
				t.Fatalf("content verification task = %+v, want disabled without queued work", task)
			}
			return
		}
	}
	t.Fatal("content verification task is missing")
}

func TestDatasourceTaskStatusesShowContentVerificationNotApplicableWithoutLocalDatasource(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "Immich",
		Kind:      config.DatasourceKindImmichIndexed,
	}}, "test-admin-token")
	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:       "1111111111111111",
		Name:            "Immich",
		IngestionKind:   datasourceIngestionRemoteAPI,
		IndexingEnabled: true,
	}})
	for _, task := range tasks {
		if task.Phase == "content_verification" {
			if task.Status != "not_applicable" || task.QueuedTasks != 0 || task.LastRunStatus != "" {
				t.Fatalf("content verification task = %+v, want not applicable without queued work or a synthetic run result", task)
			}
			return
		}
	}
	t.Fatal("content verification task is missing")
}

func TestDatasourceTaskStatusesIgnoreStaleLocalRunningCounts(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	tasks := runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:            "1111111111111111",
		Name:                 "NAS Photos",
		IngestionKind:        datasourceIngestionFilesystem,
		RunningPhase0Scans:   1,
		RunningMetadataJobs:  1,
		RunningThumbnailJobs: 1,
		QueuedMetadataJobs:   4,
		PendingThumbnailJobs: 5,
	}})
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["phase0"]; got.ActiveTasks != 0 || got.Status != "idle" {
		t.Fatalf("phase0 task = %+v, want stale DB running scan ignored", got)
	}
	if got := byPhase["metadata"]; got.ActiveTasks != 0 || got.Status != "queued" || got.QueuedTasks != 4 {
		t.Fatalf("metadata task = %+v, want queued work without stale DB running job", got)
	}
	if got := byPhase["thumbnails"]; got.ActiveTasks != 0 || got.Status != "queued" || got.QueuedTasks != 5 {
		t.Fatalf("thumbnail task = %+v, want queued work without stale DB running job", got)
	}

	runtime.rememberDatasourceTaskActivitySnapshot(nil, "metadata", 1)
	tasks = runtime.datasourceTaskStatuses(context.Background(), []DatasourceIndexingStatus{{
		SourceKey:          "1111111111111111",
		Name:               "NAS Photos",
		IngestionKind:      datasourceIngestionFilesystem,
		QueuedMetadataJobs: 4,
	}})
	byPhase = map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["metadata"]; got.ActiveTasks != 1 || got.Status != "running" || got.QueuedTasks != 4 {
		t.Fatalf("metadata task = %+v, want runtime activity to mark active work", got)
	}
}

func TestDatasourceDiscoveryActiveIsSingleFlight(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	clearActive, ok := runtime.trySetDatasourceDiscoveryActive(nil)
	if !ok {
		t.Fatal("trySetDatasourceDiscoveryActive() ok = false, want true")
	}
	if _, ok := runtime.trySetDatasourceDiscoveryActive(nil); ok {
		t.Fatal("trySetDatasourceDiscoveryActive() second ok = true, want false")
	}
	tasks := runtime.datasourceTaskStatuses(context.Background(), nil)
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	if byPhase["phase0"].ActiveTasks != 1 || byPhase["phase0"].Status != "running" {
		t.Fatalf("media discovery task = %+v, want one running task", byPhase["phase0"])
	}
	clearActive(nil)
	if active := runtime.datasourceDiscoveryActiveCount(); active != 0 {
		t.Fatalf("datasourceDiscoveryActiveCount() = %d, want 0 after clear", active)
	}
}

func TestNestedDatasourceDiscoveryActivityPreservesRunningStatus(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	clearOuter := runtime.setDatasourceDiscoveryActive(1)
	defer clearOuter()
	clearInner := runtime.setDatasourceDiscoveryActive(1)
	clearInner()

	tasks := runtime.datasourceTaskStatuses(context.Background(), nil)
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range tasks {
		byPhase[task.Phase] = task
	}
	if got := byPhase["phase0"]; got.ActiveTasks != 1 || got.Status != "running" {
		t.Fatalf("media discovery task = %+v, want running while outer activity remains", got)
	}
}

func TestDatasourceDiscoveryTaskSnapshotRecordsLastCompletedAt(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	clearActive, ok := runtime.trySetDatasourceDiscoveryActive(nil)
	if !ok {
		t.Fatal("trySetDatasourceDiscoveryActive() ok = false, want true")
	}
	completedAt := time.Date(2026, 6, 30, 4, 44, 58, 0, time.UTC)
	clearActive(&completedAt)

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	phase0 := byPhase["phase0"]
	if phase0.Status != "idle" || phase0.ActiveTasks != 0 {
		t.Fatalf("media discovery task = %+v, want idle after clear", phase0)
	}
	if phase0.LastCompletedAt == nil || !phase0.LastCompletedAt.Equal(completedAt) {
		t.Fatalf("media discovery last completed = %v, want %v", phase0.LastCompletedAt, completedAt)
	}
}

func TestDatasourceDiscoveryTaskSnapshotRecordsQuickAndReconciliationCompletion(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	reconciliationCompletedAt := time.Date(2026, 7, 11, 2, 0, 0, 0, time.UTC)
	quickCompletedAt := reconciliationCompletedAt.Add(time.Hour)
	runtime.rememberDatasourceDiscoveryTaskSnapshot(nil, false, &reconciliationCompletedAt, catalog.LocalPhase0ScanResult{
		ScanMode:    datasourceLocalScanModeReconciliation,
		CompletedAt: reconciliationCompletedAt,
	})
	runtime.rememberDatasourceDiscoveryTaskSnapshot(nil, false, &quickCompletedAt, catalog.LocalPhase0ScanResult{
		ScanMode:    datasourceLocalScanModeQuick,
		CompletedAt: quickCompletedAt,
	})

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	var phase0 DatasourceTaskStatus
	for _, task := range snapshot.Tasks {
		if task.Phase == "phase0" {
			phase0 = task
			break
		}
	}
	if phase0.LastQuickScanAt == nil || !phase0.LastQuickScanAt.Equal(quickCompletedAt) ||
		phase0.LastReconciliationAt == nil || !phase0.LastReconciliationAt.Equal(reconciliationCompletedAt) {
		t.Fatalf("media discovery scan mode timestamps = %+v, want quick=%v reconciliation=%v", phase0, quickCompletedAt, reconciliationCompletedAt)
	}
}

func TestDatasourceDiscoveryTaskSnapshotUpdatesUnsafeInMemorySnapshot(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	oldCompletedAt := time.Date(2026, 6, 29, 19, 32, 41, 0, time.UTC)
	runtime.datasourceTaskMu.Lock()
	runtime.datasourceSnapshot = &DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{
			{
				Phase:           "phase0",
				Label:           "Media discovery",
				Status:          "idle",
				LastCompletedAt: &oldCompletedAt,
			},
			{
				Phase:              "embeddings",
				Label:              "Embeddings",
				Status:             "busy",
				QueuedTasksUnknown: true,
			},
		},
	}
	runtime.datasourceSnapshotAt = oldCompletedAt
	runtime.datasourceTaskMu.Unlock()

	completedAt := time.Date(2026, 6, 30, 4, 44, 58, 0, time.UTC)
	runtime.rememberDatasourceDiscoveryTaskSnapshot(nil, false, &completedAt)

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	phase0 := byPhase["phase0"]
	if phase0.Status != "idle" || phase0.ActiveTasks != 0 {
		t.Fatalf("media discovery task = %+v, want idle after clear", phase0)
	}
	if phase0.LastCompletedAt == nil || !phase0.LastCompletedAt.Equal(completedAt) {
		t.Fatalf("media discovery last completed = %v, want %v", phase0.LastCompletedAt, completedAt)
	}
}

func TestDatasourceDiscoveryLastCompletedAtDoesNotRewindOnLiveRefresh(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	newCompletedAt := time.Date(2026, 6, 30, 4, 44, 58, 0, time.UTC)
	oldCompletedAt := time.Date(2026, 6, 29, 19, 32, 41, 0, time.UTC)
	runtime.rememberDatasourceDiscoveryTaskSnapshot(nil, false, &newCompletedAt)

	liveResponse := DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{
			{
				Phase:           "phase0",
				Label:           "Media discovery",
				Status:          "idle",
				LastCompletedAt: &oldCompletedAt,
			},
			{
				Phase:  "metadata",
				Label:  "Metadata",
				Status: "idle",
			},
		},
	}
	runtime.rememberDatasourceIndexingSnapshot(nil, liveResponse)

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	phase0 := byPhase["phase0"]
	if phase0.LastCompletedAt == nil || !phase0.LastCompletedAt.Equal(newCompletedAt) {
		t.Fatalf("media discovery last completed = %v, want preserved newer %v", phase0.LastCompletedAt, newCompletedAt)
	}
}

func TestRunDatasourceIndexingWithLocalSourceKeyDoesNotProcessOtherLocalDatasources(t *testing.T) {
	t.Parallel()

	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(firstRoot, "first.jpg"), encodeRuntimeJPEGForTest(t, 48, 32), 0o644); err != nil {
		t.Fatalf("WriteFile(first image) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(secondRoot, "second.jpg"), encodeRuntimeJPEGForTest(t, 48, 32), 0o644); err != nil {
		t.Fatalf("WriteFile(second image) error = %v", err)
	}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{
		{
			SourceKey: "1111111111111111",
			Name:      "First Root",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "first-root",
		},
		{
			SourceKey: "2222222222222222",
			Name:      "Second Root",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "second-root",
		},
	}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{
			{Key: "first-root", Path: firstRoot},
			{Key: "second-root", Path: secondRoot},
		}
	})

	catalogService := runtime.catalogService()
	if catalogService == nil {
		t.Fatal("catalog service is nil")
	}
	if _, err := catalogService.RunLocalReconciliationScan(context.Background(), "2222222222222222"); err != nil {
		t.Fatalf("RunLocalReconciliationScan(second) error = %v", err)
	}

	result, err := runtime.RunDatasourceIndexing(context.Background(), DatasourceIndexingRunOptions{
		Kind:      config.DatasourceKindLocalFiles,
		SourceKey: "1111111111111111",
	})
	if err != nil {
		t.Fatalf("RunDatasourceIndexing(first) error = %v", err)
	}
	if len(result.Results) != 1 || result.Results[0].SourceKey != "1111111111111111" {
		t.Fatalf("indexing results = %+v, want only first datasource", result.Results)
	}
	if result.Metadata != nil || result.Thumbnail != nil {
		t.Fatalf("indexing result = %+v, want media discovery only without immediate metadata/thumbnail work", result)
	}

	status, err := runtime.RefreshDatasourceIndexingStatus(context.Background())
	if err != nil {
		t.Fatalf("RefreshDatasourceIndexingStatus() error = %v", err)
	}
	bySource := map[string]DatasourceIndexingStatus{}
	for _, datasource := range status.Datasources {
		bySource[datasource.SourceKey] = datasource
	}
	first := bySource["1111111111111111"]
	second := bySource["2222222222222222"]
	if first.ActiveAssets != 0 || first.QueuedMetadataJobs != 0 || first.SettlingMetadataJobs != 1 || first.DiscoveredLocations != 1 {
		t.Fatalf("first datasource status = %+v, want discovered location settling for metadata only", first)
	}
	if second.ActiveAssets != 0 || second.QueuedMetadataJobs != 0 || second.SettlingMetadataJobs != 1 || second.DiscoveredLocations != 1 {
		t.Fatalf("second datasource status = %+v, want pre-scanned discovered location with settling metadata untouched", second)
	}
}

func TestScheduledLocalPhase0ScanUpdatesDiscoveryLastCompletedAt(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeRuntimeJPEGForTest(t, 48, 32), 0o644); err != nil {
		t.Fatalf("WriteFile(image) error = %v", err)
	}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
	})

	runtime.syncLocalPhase0Scans(context.Background(), "test")

	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), runtime.catalogService())
	if !ok {
		t.Fatal("datasourceIndexingSnapshot() ok = false, want true")
	}
	byPhase := map[string]DatasourceTaskStatus{}
	for _, task := range snapshot.Tasks {
		byPhase[task.Phase] = task
	}
	phase0 := byPhase["phase0"]
	if phase0.Status != "idle" || phase0.ActiveTasks != 0 {
		t.Fatalf("media discovery task = %+v, want idle after scheduled scan", phase0)
	}
	if phase0.LastCompletedAt == nil || phase0.LastCompletedAt.IsZero() {
		t.Fatalf("media discovery last completed = %v, want scheduled scan completion", phase0.LastCompletedAt)
	}
	if time.Since(phase0.LastCompletedAt.UTC()) > time.Minute {
		t.Fatalf("media discovery last completed = %v, want recent scheduled scan completion", phase0.LastCompletedAt)
	}
}

func TestSemanticIndexingScheduleSupportsAlwaysIndexedImmich(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        "immich_indexed",
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
	}})

	if _, ok := runtime.semanticIndexingSchedule(); ok {
		t.Fatal("semanticIndexingSchedule() ok = true with disabled config")
	}

	runtime.mu.Lock()
	runtime.config.SemanticRuntime.Indexing = config.SemanticIndexingConfig{
		Enabled:                true,
		Interval:               "250ms",
		BatchSize:              250,
		TargetCompletedVectors: 10000,
	}
	runtime.config.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(3)
	runtime.mu.Unlock()

	schedule, ok := runtime.semanticIndexingSchedule()
	if !ok {
		t.Fatal("semanticIndexingSchedule() ok = false, want enabled schedule")
	}
	if schedule.Interval != 250*time.Millisecond {
		t.Fatalf("Interval = %s, want 250ms", schedule.Interval)
	}
	if schedule.BatchSize != semanticIndexingMaxBatchSize {
		t.Fatalf("BatchSize = %d, want cap %d", schedule.BatchSize, semanticIndexingMaxBatchSize)
	}
	if schedule.Workers != 1 {
		t.Fatalf("Workers = %d, want semantic max 1", schedule.Workers)
	}
	if schedule.TargetCompletedVectors != 10000 {
		t.Fatalf("TargetCompletedVectors = %d, want 10000", schedule.TargetCompletedVectors)
	}

}

func TestSemanticIndexingScheduleAllowsLocalDatasource(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.SemanticRuntime.Indexing = config.SemanticIndexingConfig{
			Enabled:                true,
			Interval:               "500ms",
			BatchSize:              12,
			TargetCompletedVectors: 100,
		}
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(2)
	})

	schedule, ok := runtime.semanticIndexingSchedule()
	if !ok {
		t.Fatal("semanticIndexingSchedule() ok = false, want enabled schedule for local datasource")
	}
	if schedule.Interval != 500*time.Millisecond ||
		schedule.BatchSize != 12 ||
		schedule.Workers != 1 ||
		schedule.TargetCompletedVectors != 100 {
		t.Fatalf("schedule = %+v, want local datasource semantic worker config", schedule)
	}

	status := runtime.SemanticModelRegistryStatus()
	if status.IndexingWorker == nil ||
		!status.IndexingWorker.Enabled ||
		status.IndexingWorker.Status != "scheduled" ||
		status.IndexingWorker.WorkerCount != 1 {
		t.Fatalf("IndexingWorker = %+v, want scheduled local datasource worker", status.IndexingWorker)
	}
}

func TestUpdateSemanticIndexingPersistsAndStartsSchedule(t *testing.T) {
	t.Parallel()

	datasource := config.DatasourceConfig{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        "immich_indexed",
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
		Indexing:    &config.DatasourceIndexingConfig{},
	}
	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{datasource})
	fileConfig := config.Default()
	fileConfig.AgentName = "file-agent"
	fileConfig.DataDir = runtime.ConfigResponse().DataDir
	fileConfig.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(3)
	fileConfig.Datasources = []config.DatasourceConfig{datasource}
	if err := config.WriteFile(runtime.ConfigResponse().ConfigPath, fileConfig); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	updated, err := runtime.UpdateSemanticIndexing(config.SemanticIndexingConfig{
		Enabled:                true,
		Interval:               "45s",
		BatchSize:              25,
		TargetCompletedVectors: 10000,
	})
	if err != nil {
		t.Fatalf("UpdateSemanticIndexing() error = %v", err)
	}
	if !updated.Enabled || updated.Interval != "45s" || updated.BatchSize != 25 || updated.TargetCompletedVectors != 10000 {
		t.Fatalf("updated backfill = %+v, want requested settings", updated)
	}

	schedule, ok := runtime.semanticIndexingSchedule()
	if !ok {
		t.Fatal("semanticIndexingSchedule() ok = false, want enabled schedule")
	}
	if schedule.Interval != 45*time.Second || schedule.BatchSize != 25 || schedule.Workers != 1 || schedule.TargetCompletedVectors != 10000 {
		t.Fatalf("schedule = %+v, want requested settings", schedule)
	}

	status := runtime.SemanticModelRegistryStatus()
	if status.IndexingWorker == nil ||
		!status.IndexingWorker.Enabled ||
		status.IndexingWorker.Status != "scheduled" ||
		status.IndexingWorker.BatchSize != 25 ||
		status.IndexingWorker.WorkerCount != 1 ||
		status.IndexingWorker.TargetCompletedVectors != 10000 {
		t.Fatalf("IndexingWorker = %+v, want scheduled worker", status.IndexingWorker)
	}

	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	backfill := loaded.SemanticRuntime.Indexing
	if !backfill.Enabled || backfill.Interval != "45s" || backfill.BatchSize != 25 || backfill.TargetCompletedVectors != 10000 {
		t.Fatalf("persisted Indexing = %+v, want requested settings", backfill)
	}
	if loaded.Datasources[0].AccessToken != "immich-api-key" {
		t.Fatalf("persisted datasource token = %q, want preserved token", loaded.Datasources[0].AccessToken)
	}
}

func TestUpdateSemanticIndexingPreservesEffectiveWorkerPause(t *testing.T) {
	datasource := config.DatasourceConfig{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        config.DatasourceKindImmichIndexed,
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
		Indexing:    &config.DatasourceIndexingConfig{},
	}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{datasource}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(0)
	})
	fileConfig := config.Default()
	fileConfig.AgentName = "file-agent"
	fileConfig.DataDir = runtime.ConfigResponse().DataDir
	fileConfig.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(3)
	fileConfig.Datasources = []config.DatasourceConfig{datasource}
	if err := config.WriteFile(runtime.ConfigResponse().ConfigPath, fileConfig); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := runtime.UpdateSemanticIndexing(config.SemanticIndexingConfig{
		Enabled:   true,
		Interval:  "45s",
		BatchSize: 25,
	}); err != nil {
		t.Fatalf("UpdateSemanticIndexing() error = %v", err)
	}
	status := runtime.WorkerRuntimeStatus()
	if status.HeavyTaskWorkers != 0 || !status.PausedHeavyTaskWorkers || status.ConfiguredHeavyTaskWorkers == nil || *status.ConfiguredHeavyTaskWorkers != 0 {
		t.Fatalf("WorkerRuntimeStatus() = %+v, want effective worker pause preserved", status)
	}
	schedule, ok := runtime.semanticIndexingSchedule()
	if !ok || schedule.Workers != 0 {
		t.Fatalf("semanticIndexingSchedule() = %+v, %t, want paused zero-worker schedule", schedule, ok)
	}
	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.WorkerRuntime.HeavyTaskWorkers == nil || *loaded.WorkerRuntime.HeavyTaskWorkers != 3 {
		t.Fatalf("persisted HeavyTaskWorkers = %v, want unrelated file value 3 preserved", loaded.WorkerRuntime.HeavyTaskWorkers)
	}
}

func TestUpdateWorkerRuntimePersistsAndUpdatesSemanticWorkerStatus(t *testing.T) {
	t.Parallel()

	datasource := config.DatasourceConfig{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        "immich_indexed",
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
		Indexing:    &config.DatasourceIndexingConfig{},
	}
	runtime := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{datasource})
	fileConfig := config.Default()
	fileConfig.AgentName = "file-agent"
	fileConfig.DataDir = runtime.ConfigResponse().DataDir
	fileConfig.SemanticRuntime.Indexing = config.SemanticIndexingConfig{
		Enabled:   true,
		Interval:  "45s",
		BatchSize: 25,
	}
	fileConfig.Datasources = []config.DatasourceConfig{datasource}
	if err := config.WriteFile(runtime.ConfigResponse().ConfigPath, fileConfig); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	runtime.mu.Lock()
	runtime.config.SemanticRuntime.Indexing = fileConfig.SemanticRuntime.Indexing
	runtime.mu.Unlock()

	status, err := runtime.UpdateWorkerRuntime(config.WorkerRuntimeConfig{HeavyTaskWorkers: runtimeTestIntPtr(4)})
	if err != nil {
		t.Fatalf("UpdateWorkerRuntime() error = %v", err)
	}
	if status.HeavyTaskWorkers != 4 || status.ConfiguredHeavyTaskWorkers == nil || *status.ConfiguredHeavyTaskWorkers != 4 || status.AutoHeavyTaskWorkers || status.SemanticIndexingWorkers != 1 {
		t.Fatalf("WorkerRuntimeStatus = %+v, want configured 4 workers with semantic max 1", status)
	}
	semanticStatus := runtime.SemanticModelRegistryStatus()
	if semanticStatus.IndexingWorker == nil || semanticStatus.IndexingWorker.WorkerCount != 1 {
		t.Fatalf("IndexingWorker = %+v, want semantic worker count 1", semanticStatus.IndexingWorker)
	}

	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.WorkerRuntime.HeavyTaskWorkers == nil || *loaded.WorkerRuntime.HeavyTaskWorkers != 4 {
		t.Fatalf("persisted HeavyTaskWorkers = %v, want 4", loaded.WorkerRuntime.HeavyTaskWorkers)
	}
	if loaded.Datasources[0].AccessToken != "immich-api-key" {
		t.Fatalf("persisted datasource token = %q, want preserved token", loaded.Datasources[0].AccessToken)
	}
}

func TestSemanticIndexingBatchSizeForTarget(t *testing.T) {
	t.Parallel()

	schedule := semanticIndexingSchedule{
		BatchSize:              100,
		TargetCompletedVectors: 10000,
	}
	if got, ok := semanticIndexingBatchSizeForStatus(schedule, 9900); !ok || got != 100 {
		t.Fatalf("batch at 9900 = %d, %t; want 100, true", got, ok)
	}
	if got, ok := semanticIndexingBatchSizeForStatus(schedule, 9950); !ok || got != 50 {
		t.Fatalf("batch at 9950 = %d, %t; want 50, true", got, ok)
	}
	if got, ok := semanticIndexingBatchSizeForStatus(schedule, 10000); ok || got != 0 {
		t.Fatalf("batch at target = %d, %t; want 0, false", got, ok)
	}
	if got, ok := semanticIndexingBatchSizeForStatus(semanticIndexingSchedule{BatchSize: 100}, 10000); !ok || got != 100 {
		t.Fatalf("unbounded batch = %d, %t; want 100, true", got, ok)
	}
}

func TestSemanticBackfillWorkSeparatesVectorAndPublishWork(t *testing.T) {
	t.Parallel()

	if got := semanticCandidateVectorBackfillWorkCount(&catalog.SemanticModelBackfillStatus{
		CompletedVectorCount: 10,
		IndexedVectorCount:   7,
	}); got != 0 {
		t.Fatalf("semanticCandidateVectorBackfillWorkCount(index gap) = %d, want no vector work", got)
	}
	if got := semanticCandidateIndexPublishWorkCount(&catalog.SemanticModelBackfillStatus{
		EligibleAssetCount:   10,
		CompletedVectorCount: 10,
		IndexedVectorCount:   7,
	}); got != 3 {
		t.Fatalf("semanticCandidateIndexPublishWorkCount(index gap) = %d, want unindexed vector count", got)
	}
	if got := semanticCandidateIndexPublishWorkCount(&catalog.SemanticModelBackfillStatus{
		PendingIndexJobCount:  2,
		EligibleIndexJobCount: 2,
	}); got != 2 {
		t.Fatalf("semanticCandidateIndexPublishWorkCount(pending jobs) = %d, want 2", got)
	}
	if got := semanticCandidateVectorBackfillWorkCount(&catalog.SemanticModelBackfillStatus{
		EligibleNowVectorCount: 4,
		RemainingVectorCount:   4,
		CompletedVectorCount:   10,
		IndexedVectorCount:     7,
	}); got != 4 {
		t.Fatalf("semanticCandidateVectorBackfillWorkCount(remaining) = %d, want 4", got)
	}
	if got := semanticBackfillRolePriority(catalog.SemanticModelProfileStatus{
		Role:        catalog.SemanticModelRoleCandidate,
		ProfileKind: catalog.SemanticProfileKindModelPack,
	}); got != 2 {
		t.Fatalf("candidate role priority = %d, want 2", got)
	}
	if got := semanticBackfillRolePriority(catalog.SemanticModelProfileStatus{
		Role:        catalog.SemanticModelRoleActive,
		ProfileKind: catalog.SemanticProfileKindModelPack,
	}); got != 1 {
		t.Fatalf("active role priority = %d, want 1", got)
	}
	if semanticPriorityIndexPublishDue(catalog.SemanticModelBackfillStatus{
		CompletedVectorCount: 119,
		IndexedVectorCount:   100,
	}) {
		t.Fatal("semanticPriorityIndexPublishDue(19 queued over 100 indexed) = true, want false")
	}
	if !semanticPriorityIndexPublishDue(catalog.SemanticModelBackfillStatus{
		CompletedVectorCount: 120,
		IndexedVectorCount:   100,
	}) {
		t.Fatal("semanticPriorityIndexPublishDue(20 queued over 100 indexed) = false, want true")
	}
	mixedSchedule := semanticIndexingSchedule{BatchSize: 10}
	if got, ok := semanticMixedIndexingBatchSizeForStatus(mixedSchedule, catalog.SemanticModelBackfillStatus{
		EligibleAssetCount:     109,
		EligibleNowVectorCount: 9,
		CompletedVectorCount:   100,
		RemainingVectorCount:   9,
	}); ok || got != 0 {
		t.Fatalf("semanticMixedIndexingBatchSizeForStatus(9 queued) = %d, %t; want 0, false", got, ok)
	}
	if got, ok := semanticMixedIndexingBatchSizeForStatus(mixedSchedule, catalog.SemanticModelBackfillStatus{
		EligibleAssetCount:     110,
		EligibleNowVectorCount: 10,
		CompletedVectorCount:   100,
		RemainingVectorCount:   10,
	}); !ok || got != 10 {
		t.Fatalf("semanticMixedIndexingBatchSizeForStatus(10 queued) = %d, %t; want 10, true", got, ok)
	}
	smallerBatchSchedule := semanticIndexingSchedule{BatchSize: 5}
	if got, ok := semanticMixedIndexingBatchSizeForStatus(smallerBatchSchedule, catalog.SemanticModelBackfillStatus{
		EligibleAssetCount:     105,
		EligibleNowVectorCount: 5,
		CompletedVectorCount:   100,
		RemainingVectorCount:   5,
	}); !ok || got != 5 {
		t.Fatalf("semanticMixedIndexingBatchSizeForStatus(5 queued, 5 batch) = %d, %t; want 5, true", got, ok)
	}
}

func TestSemanticIndexingPrefersRuntimeReadyInstalledCandidate(t *testing.T) {
	ctx := context.Background()
	helperPath := writeRuntimeCandidateSelectionHelper(t)
	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "immich-api-key" {
			t.Fatalf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
		}
		switch r.URL.Path {
		case "/api/search/metadata":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"assets": {
					"total": 1,
					"items": [
						{
							"id": "asset-runtime-ready",
							"type": "IMAGE",
							"originalFileName": "runtime-ready.jpg",
							"fileCreatedAt": "2026-06-01T10:00:00Z",
							"updatedAt": "2026-06-01T10:05:00Z"
						}
					],
					"nextPage": null
				}
			}`)
		case "/api/assets/asset-runtime-ready/thumbnail":
			if r.URL.Query().Get("size") != "preview" {
				t.Fatalf("thumbnail size = %q, want preview", r.URL.Query().Get("size"))
			}
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(encodeRuntimeJPEGForTest(t, 32, 32))
		default:
			t.Fatalf("unexpected datasource path %s", r.URL.RequestURI())
		}
	}))
	defer datasourceServer.Close()

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        "immich_indexed",
		URL:         datasourceServer.URL,
		AccessToken: "immich-api-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 1,
		},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.HelperPath = helperPath
	})

	if _, err := runtime.SyncPrimaryDatasourceMirror(ctx, catalog.MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncPrimaryDatasourceMirror() error = %v", err)
	}
	installRuntimeSemanticPackForTest(t, runtime, runtimeSemanticPackForTest("aaa-old-model"))
	installRuntimeSemanticPackForTest(t, runtime, runtimeSemanticPackForTest("zzz-new-model"))

	result, err := runtime.RunSemanticIndexing(ctx, 1)
	if err != nil {
		t.Fatalf("RunSemanticIndexing() error = %v", err)
	}
	if result.Status.ModelID != "zzz-new-model" ||
		result.ProcessedVectorCount != 1 ||
		result.Status.Status != catalog.SemanticBackfillStatusIndexing ||
		result.Status.CompletedVectorCount != 1 ||
		result.Status.IndexedVectorCount != 0 {
		t.Fatalf("backfill result = %#v", result)
	}
	if published := runtime.runScheduledSemanticIndexPublish(ctx, semanticIndexingSchedule{Workers: 1}); !published {
		t.Fatal("runScheduledSemanticIndexPublish() published = false, want candidate index publish")
	}

	indexing, err := runtime.DatasourceIndexingStatus(ctx)
	if err != nil {
		t.Fatalf("DatasourceIndexingStatus() error = %v", err)
	}
	if len(indexing.Datasources) != 1 ||
		indexing.Datasources[0].EmbeddingStatus != catalog.SemanticBackfillStatusReady ||
		indexing.Datasources[0].EmbeddingModelID != "zzz-new-model" ||
		indexing.Datasources[0].EmbeddingEligible != 1 ||
		indexing.Datasources[0].EmbeddingCompleted != 1 ||
		indexing.Datasources[0].EmbeddingIndexed != 1 ||
		indexing.Datasources[0].EmbeddingRemaining != 0 {
		t.Fatalf("remote datasource embedding coverage = %+v, want ready candidate coverage", indexing.Datasources)
	}

	status := runtime.SemanticModelRegistryStatusWithContext(ctx)
	if status.Candidate == nil || status.Candidate.ModelID != "zzz-new-model" {
		t.Fatalf("status candidate = %#v, want runtime-ready candidate", status.Candidate)
	}
	if status.Indexing == nil || status.Indexing.ModelID != "zzz-new-model" || status.Indexing.Status != catalog.SemanticBackfillStatusReady {
		t.Fatalf("semantic indexing status = %#v", status.Indexing)
	}

	activation, err := runtime.ActivateSemanticModel(ctx, status.Candidate.ModelID, status.Candidate.VectorSpaceID)
	if err != nil {
		t.Fatalf("ActivateSemanticModel() error = %v", err)
	}
	if activation.ModelID != "zzz-new-model" || activation.Profile.Role != catalog.SemanticModelRoleActive {
		t.Fatalf("activation = %#v", activation)
	}

	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()
	coverage := []DatasourceIndexingStatus{{
		SourceKey:       "1111111111111111",
		Name:            "Home Immich",
		IngestionKind:   datasourceIngestionRemoteAPI,
		IndexingEnabled: true,
		Status:          "idle",
	}}
	if err := runtime.enrichDatasourceEmbeddingCoverage(cancelledContext, runtime.catalogService(), coverage); err != nil {
		t.Fatalf("enrichDatasourceEmbeddingCoverage(cancelled) error = %v", err)
	}
	if coverage[0].EmbeddingStatus != datasourceEmbeddingUnavailable ||
		coverage[0].EmbeddingModelID == "" ||
		coverage[0].EmbeddingLastError == "" {
		t.Fatalf("coverage after cancelled enrich = %+v, want unavailable with model and error", coverage[0])
	}
}

func TestSemanticIndexingSchedulerSeesMissingBinaryIndexPublishWork(t *testing.T) {
	ctx := context.Background()
	helperPath := writeRuntimeCandidateSelectionHelper(t)
	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "immich-api-key" {
			t.Fatalf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
		}
		switch r.URL.Path {
		case "/api/search/metadata":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"assets": {
					"total": 1,
					"items": [
						{
							"id": "asset-missing-binary",
							"type": "IMAGE",
							"originalFileName": "missing-binary.jpg",
							"fileCreatedAt": "2026-06-01T10:00:00Z",
							"updatedAt": "2026-06-01T10:05:00Z"
						}
					],
					"nextPage": null
				}
			}`)
		case "/api/assets/asset-missing-binary/thumbnail":
			if r.URL.Query().Get("size") != "preview" {
				t.Fatalf("thumbnail size = %q, want preview", r.URL.Query().Get("size"))
			}
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(encodeRuntimeJPEGForTest(t, 32, 32))
		default:
			t.Fatalf("unexpected datasource path %s", r.URL.RequestURI())
		}
	}))
	defer datasourceServer.Close()

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        "immich_indexed",
		URL:         datasourceServer.URL,
		AccessToken: "immich-api-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 1,
		},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.HelperPath = helperPath
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	})

	if _, err := runtime.SyncPrimaryDatasourceMirror(ctx, catalog.MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncPrimaryDatasourceMirror() error = %v", err)
	}
	installRuntimeSemanticPackForTest(t, runtime, runtimeSemanticPackForTest("zzz-new-model"))
	backfill, err := runtime.RunSemanticIndexing(ctx, 1)
	if err != nil {
		t.Fatalf("RunSemanticIndexing(seed) error = %v", err)
	}
	if backfill.ProcessedVectorCount != 1 || backfill.Status.IndexedVectorCount != 0 {
		t.Fatalf("seed backfill = %#v, want one vector before publish", backfill)
	}
	if published := runtime.runScheduledSemanticIndexPublish(ctx, semanticIndexingSchedule{Workers: 1}); !published {
		t.Fatal("runScheduledSemanticIndexPublish(initial) = false, want initial publish")
	}
	files := semanticBinaryIndexFilesForRuntimeTest(t, runtime.config.DataDir)
	if len(files) == 0 {
		t.Fatal("semantic binary index files = 0 after publish, want one file to remove")
	}
	for _, path := range files {
		if err := os.Remove(path); err != nil {
			t.Fatalf("Remove(%s) error = %v", path, err)
		}
	}

	status := runtime.SemanticModelRegistryStatusWithContext(ctx)
	candidate := semanticCandidateNeedingIndexPublish(ctx, runtime.catalogService(), status, runtime.semanticModels)
	if candidate == nil {
		t.Fatal("semanticCandidateNeedingIndexPublish() = nil, want missing binary repair candidate")
	}
	if candidate.ModelID != "zzz-new-model" {
		t.Fatalf("semanticCandidateNeedingIndexPublish() = %s, want zzz-new-model", candidate.ModelID)
	}
	state, ok := runtime.schedulerWorkStateSnapshot(ctx, semanticIndexingSchedule{
		Workers:   1,
		BatchSize: 100,
	}, true)
	if !ok {
		t.Fatal("schedulerWorkStateSnapshot() ok = false, want scheduler state")
	}
	if !state.SemanticPublishReady {
		t.Fatalf("SemanticPublishReady = false in state %#v, want missing binary repair to be publish-ready", state)
	}
	if published := runtime.runScheduledSemanticIndexPublish(ctx, semanticIndexingSchedule{Workers: 1}); !published {
		t.Fatal("runScheduledSemanticIndexPublish(repair) = false, want missing binary repair publish")
	}
	if files := semanticBinaryIndexFilesForRuntimeTest(t, runtime.config.DataDir); len(files) == 0 {
		t.Fatal("semantic binary index files = 0 after repair, want regenerated index")
	}
}

func TestSemanticIndexingZeroQueuesReadyVectorsAndAllowsReadyUninstallAfterPublish(t *testing.T) {
	ctx := context.Background()
	helperPath := writeRuntimeCandidateSelectionHelper(t)
	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "immich-api-key" {
			t.Fatalf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
		}
		switch r.URL.Path {
		case "/api/search/metadata":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"assets": {
					"total": 1,
					"items": [
						{
							"id": "asset-publish-only",
							"type": "IMAGE",
							"originalFileName": "publish-only.jpg",
							"fileCreatedAt": "2026-06-01T10:00:00Z",
							"updatedAt": "2026-06-01T10:05:00Z"
						}
					],
					"nextPage": null
				}
			}`)
		case "/api/assets/asset-publish-only/thumbnail":
			if r.URL.Query().Get("size") != "preview" {
				t.Fatalf("thumbnail size = %q, want preview", r.URL.Query().Get("size"))
			}
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(encodeRuntimeJPEGForTest(t, 32, 32))
		default:
			t.Fatalf("unexpected datasource path %s", r.URL.RequestURI())
		}
	}))
	defer datasourceServer.Close()

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        "immich_indexed",
		URL:         datasourceServer.URL,
		AccessToken: "immich-api-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 1,
		},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.HelperPath = helperPath
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	})

	if _, err := runtime.SyncPrimaryDatasourceMirror(ctx, catalog.MirrorSyncModeFull); err != nil {
		t.Fatalf("SyncPrimaryDatasourceMirror() error = %v", err)
	}
	installRuntimeSemanticPackForTest(t, runtime, runtimeSemanticPackForTest("zzz-new-model"))
	status := runtime.SemanticModelRegistryStatusWithContext(ctx)
	if status.Candidate == nil {
		t.Fatal("semantic status candidate = nil, want installed candidate")
	}
	candidate := *status.Candidate
	catalogService := runtime.catalogService()
	if catalogService == nil {
		t.Fatal("catalog service is nil")
	}
	seed, err := catalogService.BackfillSemanticModelCandidateWithOptions(ctx, runtime.semanticModels, candidate, catalog.SemanticModelBackfillOptions{
		MaxAssets: 1,
		Workers:   1,
	})
	if err != nil {
		t.Fatalf("BackfillSemanticModelCandidateWithOptions(seed) error = %v", err)
	}
	if seed.ProcessedVectorCount != 1 ||
		seed.Status.CompletedVectorCount != 1 ||
		seed.Status.IndexedVectorCount != 0 {
		t.Fatalf("seed backfill = %#v, want one ready vector without published index", seed)
	}

	publish, err := runtime.RunSemanticIndexing(ctx, 0)
	if err != nil {
		t.Fatalf("RunSemanticIndexing(publish-only) error = %v", err)
	}
	if publish.ProcessedVectorCount != 0 ||
		publish.Status.CompletedVectorCount != 1 ||
		publish.Status.IndexedVectorCount != 0 ||
		publish.IndexedVectorCount != 0 ||
		publish.Status.PendingIndexJobCount != 1 ||
		publish.Status.Status != catalog.SemanticBackfillStatusIndexing {
		t.Fatalf("publish-only backfill = %#v, want durable HNSW job without new vectors", publish)
	}
	if got := runtime.semanticBackfillActiveWorkers(); got != 0 {
		t.Fatalf("semanticBackfillActiveWorkers() = %d after publish-only backfill, want 0", got)
	}
	if got := runtime.semanticIndexPublishActive(); got != 0 {
		t.Fatalf("semanticIndexPublishActive() = %d after publish-only backfill, want 0", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	var published *catalog.SemanticModelBackfillStatus
	for time.Now().Before(deadline) {
		published, err = catalogService.SemanticModelBackfillStatus(ctx, candidate)
		if err != nil {
			t.Fatalf("SemanticModelBackfillStatus() error = %v", err)
		}
		if published != nil &&
			published.Status == catalog.SemanticBackfillStatusReady &&
			published.IndexedVectorCount == 1 &&
			published.PendingIndexJobCount == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if published == nil || published.Status != catalog.SemanticBackfillStatusReady || published.IndexedVectorCount != 1 {
		t.Fatalf("published semantic status = %#v, want async HNSW publish", published)
	}
	if runtime.semanticModelPackMigrating(ctx, candidate.ModelID, candidate.VectorSpaceID) {
		t.Fatalf("semanticModelPackMigrating(%s) = true after ready publish, want false", candidate.ModelID)
	}
	uninstall, err := runtime.UninstallSemanticModelPack(ctx, candidate.ModelID, candidate.VectorSpaceID)
	if err != nil {
		t.Fatalf("UninstallSemanticModelPack(ready candidate) error = %v", err)
	}
	if uninstall.Status != "uninstalled" || uninstall.ModelID != candidate.ModelID || uninstall.VectorSpaceID != candidate.VectorSpaceID {
		t.Fatalf("uninstall result = %#v, want ready candidate removed", uninstall)
	}
}

func TestSemanticIndexingIndexesLocalDatasourceImages(t *testing.T) {
	ctx := context.Background()
	semanticHelperPath := writeRuntimeCandidateSelectionHelper(t)
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "local-family.jpg"), encodeRuntimeJPEGForTest(t, 96, 64), 0o644); err != nil {
		t.Fatalf("WriteFile(local image) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "local-clip.mov"), []byte("video"), 0o644); err != nil {
		t.Fatalf("WriteFile(local video) error = %v", err)
	}
	posterPath := filepath.Join(t.TempDir(), "poster.jpg")
	if err := os.WriteFile(posterPath, encodeRuntimeJPEGForTest(t, 64, 64), 0o644); err != nil {
		t.Fatalf("WriteFile(video poster) error = %v", err)
	}
	ffmpegPath := writeRuntimeFakeFFmpegPosterScript(t, posterPath)
	mediaHelperPath := writeRuntimeFakeMediaHelperScript(t, posterPath)

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.SemanticRuntime.HelperPath = semanticHelperPath
		cfg.MediaRuntime.HelperPath = mediaHelperPath
		cfg.MediaRuntime.FFmpegPath = ffmpegPath
	})

	scan, err := runtime.RunLocalDatasourcePhase0Scans(ctx)
	if err != nil {
		t.Fatalf("RunLocalDatasourcePhase0Scans() error = %v", err)
	}
	if len(scan.Phase0) != 1 || scan.Phase0[0].QueuedMetadata != 2 ||
		scan.Metadata.ProcessedJobs != 0 || scan.Thumbnail.ProcessedJobs != 0 {
		t.Fatalf("local scan result = %+v, want discovery-only queueing for image+video", scan)
	}
	if processed := runtime.runNextBackgroundWorkerTask(ctx); !processed {
		t.Fatal("runNextBackgroundWorkerTask(metadata) = false, want image+video registration")
	}
	if processed := runtime.runNextBackgroundWorkerTask(ctx); !processed {
		t.Fatal("runNextBackgroundWorkerTask(thumbnail) = false, want image+video poster thumbnails")
	}
	installRuntimeSemanticPackForTest(t, runtime, runtimeSemanticPackForTest("zzz-new-model"))

	result, err := runtime.RunSemanticIndexing(ctx, 10)
	if err != nil {
		t.Fatalf("RunSemanticIndexing() error = %v", err)
	}
	if result.ProcessedVectorCount != 1 ||
		result.Status.EligibleAssetCount != 1 ||
		result.Status.CompletedVectorCount != 1 ||
		result.Status.IndexedVectorCount != 0 ||
		result.Status.Status != catalog.SemanticBackfillStatusIndexing {
		t.Fatalf("local semantic indexing result = %#v, want one completed image vector awaiting index publish", result)
	}
	if published := runtime.runScheduledSemanticIndexPublish(ctx, semanticIndexingSchedule{Workers: 1}); !published {
		t.Fatal("runScheduledSemanticIndexPublish() published = false, want local image index")
	}

	page, err := runtime.SearchAssets(catalog.AssetSearchRequest{
		Collection: catalog.AssetCollectionRequest{
			Kind: catalog.CollectionKindSearch,
			Query: &catalog.AssetSearchQuery{
				Text: "family",
				Mode: catalog.QueryModeSemantic,
			},
		},
		Page: catalog.AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets(semantic local) error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Filename != "local-family.jpg" {
		t.Fatalf("semantic local page = %#v, want local image result only", page)
	}
}

func TestLocalPhase3EmbeddingBatchIndexesReadyLocalImages(t *testing.T) {
	ctx := context.Background()
	semanticHelperPath := writeRuntimeCandidateSelectionHelper(t)
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "phase3-family.jpg"), encodeRuntimeJPEGForTest(t, 96, 64), 0o644); err != nil {
		t.Fatalf("WriteFile(local image) error = %v", err)
	}
	mediaHelperPath := writeRuntimeFakeMediaHelperScript(t, "")

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.SemanticRuntime.HelperPath = semanticHelperPath
		cfg.MediaRuntime.HelperPath = mediaHelperPath
		cfg.SemanticRuntime.Indexing = config.SemanticIndexingConfig{
			Enabled:   true,
			Interval:  "1m",
			BatchSize: 10,
		}
	})
	installRuntimeSemanticPackForTest(t, runtime, runtimeSemanticPackForTest("zzz-new-model"))
	if result, err := runtime.RunLocalDatasourcePhase0Scans(ctx); err != nil {
		t.Fatalf("RunLocalDatasourcePhase0Scans() error = %v", err)
	} else if len(result.Phase0) != 1 || result.Phase0[0].QueuedMetadata != 1 ||
		result.Metadata.ProcessedJobs != 0 || result.Thumbnail.ProcessedJobs != 0 {
		t.Fatalf("local scan result = %+v, want discovery-only queueing", result)
	}
	if processed := runtime.runScheduledLocalMetadataBatch(ctx, "test"); !processed {
		t.Fatal("runScheduledLocalMetadataBatch() = false, want local image registration")
	}
	if processed := runtime.runScheduledLocalThumbnailBatch(ctx, "test"); !processed {
		t.Fatal("runScheduledLocalThumbnailBatch() = false, want local image thumbnail")
	}

	if processed := runtime.runScheduledLocalEmbeddingBatch(ctx, "test"); !processed {
		t.Fatal("runScheduledLocalEmbeddingBatch() processed = false, want local image vector")
	}
	schedule, ok := runtime.semanticIndexingSchedule()
	if !ok {
		t.Fatal("semanticIndexingSchedule() ok = false, want schedule")
	}
	if published := runtime.runScheduledSemanticIndexPublish(ctx, schedule); !published {
		t.Fatal("runScheduledSemanticIndexPublish() published = false, want local image index")
	}
	page, err := runtime.SearchAssets(catalog.AssetSearchRequest{
		Collection: catalog.AssetCollectionRequest{
			Kind: catalog.CollectionKindSearch,
			Query: &catalog.AssetSearchQuery{
				Text: "family",
				Mode: catalog.QueryModeSemantic,
			},
		},
		Page: catalog.AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets(phase3 semantic local) error = %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Filename != "phase3-family.jpg" {
		t.Fatalf("phase3 semantic local page = %#v, want local image result", page)
	}
}

func TestRepairLocalDatasourceEmbeddingsReportsCoverage(t *testing.T) {
	ctx := context.Background()
	semanticHelperPath := writeRuntimeCandidateSelectionHelper(t)
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "repair-family.jpg"), encodeRuntimeJPEGForTest(t, 96, 64), 0o644); err != nil {
		t.Fatalf("WriteFile(local image) error = %v", err)
	}
	mediaHelperPath := writeRuntimeFakeMediaHelperScript(t, "")

	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
		Scan:      &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.SemanticRuntime.HelperPath = semanticHelperPath
		cfg.MediaRuntime.HelperPath = mediaHelperPath
		cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
	})
	installRuntimeSemanticPackForTest(t, runtime, runtimeSemanticPackForTest("zzz-new-model"))
	if result, err := runtime.RunLocalDatasourcePhase0Scans(ctx); err != nil {
		t.Fatalf("RunLocalDatasourcePhase0Scans() error = %v", err)
	} else if len(result.Phase0) != 1 || result.Phase0[0].QueuedMetadata != 1 ||
		result.Metadata.ProcessedJobs != 0 || result.Thumbnail.ProcessedJobs != 0 {
		t.Fatalf("local scan result = %+v, want discovery-only queueing", result)
	}
	if processed := runtime.runScheduledLocalMetadataBatch(ctx, "test"); !processed {
		t.Fatal("runScheduledLocalMetadataBatch() = false, want local image registration")
	}
	if processed := runtime.runScheduledLocalThumbnailBatch(ctx, "test"); !processed {
		t.Fatal("runScheduledLocalThumbnailBatch() = false, want local image thumbnail")
	}

	before, err := runtime.LocalDatasourceScanStatus(ctx)
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatus(before) error = %v", err)
	}
	if len(before.Datasources) != 1 ||
		before.Datasources[0].EmbeddingEligible != 1 ||
		before.Datasources[0].EmbeddingCompleted != 0 ||
		before.Datasources[0].EmbeddingRemaining != 1 ||
		before.Datasources[0].EmbeddingStatus != catalog.SemanticBackfillStatusPending {
		t.Fatalf("embedding coverage before repair = %+v, want one pending vector", before.Datasources)
	}

	repair, err := runtime.RepairLocalDatasourceEmbeddings(ctx)
	if err != nil {
		t.Fatalf("RepairLocalDatasourceEmbeddings() error = %v", err)
	}
	if repair.ProcessedVectorCount != 1 ||
		repair.Status.EligibleAssetCount != 1 ||
		repair.Status.CompletedVectorCount != 1 ||
		repair.Status.IndexedVectorCount != 0 {
		t.Fatalf("embedding repair = %#v, want one completed local vector awaiting index publish", repair)
	}
	if published := runtime.runScheduledSemanticIndexPublish(ctx, semanticIndexingSchedule{Workers: 1}); !published {
		t.Fatal("runScheduledSemanticIndexPublish() published = false, want repaired local index")
	}

	after, err := runtime.LocalDatasourceScanStatus(ctx)
	if err != nil {
		t.Fatalf("LocalDatasourceScanStatus(after) error = %v", err)
	}
	if len(after.Datasources) != 1 ||
		after.Datasources[0].EmbeddingCompleted != 1 ||
		after.Datasources[0].EmbeddingIndexed != 1 ||
		after.Datasources[0].EmbeddingRemaining != 0 ||
		after.Datasources[0].EmbeddingStatus != catalog.SemanticBackfillStatusReady {
		t.Fatalf("embedding coverage after repair = %+v, want ready vector", after.Datasources)
	}
}

func TestLocalPhase3KindAlternatesThumbnailAndEmbedding(t *testing.T) {
	t.Parallel()

	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
	if got := runtime.nextLocalPhase3Kind(); got != localPhase3KindThumbnail {
		t.Fatalf("first phase3 kind = %q, want thumbnail", got)
	}
	if got := runtime.nextLocalPhase3Kind(); got != localPhase3KindEmbedding {
		t.Fatalf("second phase3 kind = %q, want embedding", got)
	}
	if got := runtime.nextLocalPhase3Kind(); got != localPhase3KindThumbnail {
		t.Fatalf("third phase3 kind = %q, want thumbnail", got)
	}
}

func TestNextDatasourceMirrorDailyFullSweep(t *testing.T) {
	t.Parallel()

	location, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	beforeWindow := time.Date(2026, 6, 10, 1, 30, 0, 0, location)
	next, ok := nextDatasourceMirrorDailyFullSweep(beforeWindow, location, "02:00")
	if !ok {
		t.Fatal("nextDatasourceMirrorDailyFullSweep() ok = false, want true")
	}
	if want := time.Date(2026, 6, 10, 2, 0, 0, 0, location); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}

	afterWindow := time.Date(2026, 6, 10, 3, 0, 0, 0, location)
	next, ok = nextDatasourceMirrorDailyFullSweep(afterWindow, location, "02:00")
	if !ok {
		t.Fatal("nextDatasourceMirrorDailyFullSweep() ok = false after window, want true")
	}
	if want := time.Date(2026, 6, 11, 2, 0, 0, 0, location); !next.Equal(want) {
		t.Fatalf("next after window = %v, want %v", next, want)
	}

	if _, ok := nextDatasourceMirrorDailyFullSweep(beforeWindow, location, "25:00"); ok {
		t.Fatal("invalid daily full sweep window ok = true, want false")
	}
}

func TestStorageGuardrailBlocksUploadStart(t *testing.T) {
	rootPath := t.TempDir()
	runtime := newTestAgentRuntime(t, BuildInfo{}, nil)
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
	restoreStorageAvailableBytes := stubStorageAvailableBytes(t, 512*1024*1024, nil)
	defer restoreStorageAvailableBytes()

	state, err := runtime.AppUploadState(bundle.DeviceID)
	if err != nil {
		t.Fatalf("AppUploadState() error = %v", err)
	}
	if state.Status.State != "blocked" || !strings.Contains(state.Status.Reason, "guardrail") {
		t.Fatalf("upload status = %+v, want storage guardrail block", state.Status)
	}
	response, err := runtime.StartUploadSession(bundle.DeviceID, UploadSessionStartInput{
		SourceAssetID:      "asset-storage",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
	})
	if err != nil {
		t.Fatalf("StartUploadSession() error = %v", err)
	}
	if response.State != "blocked" || response.Status == nil || !strings.Contains(response.Reason, "guardrail") {
		t.Fatalf("StartUploadSession() = %+v, want storage guardrail block", response)
	}
}

func TestStorageGuardrailBlocksLocalPhase0Writes(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), encodeRuntimeJPEGForTest(t, 64, 64), 0o644); err != nil {
		t.Fatalf("WriteFile(local image) error = %v", err)
	}
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111",
		Name:      "NAS Photos",
		Kind:      config.DatasourceKindLocalFiles,
		RootKey:   "nas-photos",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
	})
	restoreStorageAvailableBytes := stubStorageAvailableBytes(t, 512*1024*1024, nil)
	defer restoreStorageAvailableBytes()

	if _, err := runtime.RunLocalDatasourcePhase0Scans(context.Background()); !errors.Is(err, ErrStorageWriteBlocked) {
		t.Fatalf("RunLocalDatasourcePhase0Scans() error = %v, want ErrStorageWriteBlocked", err)
	}
	status := runtime.StatusResponse().StorageGuardrail
	if status.State != "blocked" || !status.WriteBlocked || status.AvailableBytes != 512*1024*1024 {
		t.Fatalf("StorageGuardrail = %+v, want blocked low-space status", status)
	}
}

func stubStorageAvailableBytes(t *testing.T, available int64, err error) func() {
	t.Helper()

	previous := storageAvailableBytes
	storageAvailableBytes = func(string) (int64, error) {
		return available, err
	}
	return func() {
		storageAvailableBytes = previous
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

func runtimeSemanticPackForTest(modelID string) catalog.SemanticModelPackStatus {
	return catalog.SemanticModelPackStatus{
		ID:            modelID,
		Name:          modelID,
		Version:       "2026.06.12",
		VectorSpaceID: modelID + "/d4",
		EmbeddingDim:  4,
		InputKind:     catalog.SemanticInputKindImage,
		Runtime:       "onnxruntime",
		Artifact: &catalog.SemanticModelArtifactStatus{
			Filename: modelID + ".zip",
		},
	}
}

func installStoredRuntimeSemanticPackForTest(t *testing.T, runtime *AgentRuntime, pack catalog.SemanticModelPackStatus) {
	t.Helper()

	artifact := runtimeSemanticPackArtifactForTest(t, pack)
	sum := sha256.Sum256(artifact)
	pack.Artifact.SHA256 = hex.EncodeToString(sum[:])
	pack.Artifact.SizeBytes = int64(len(artifact))
	pack.SizeBytes = int64(len(artifact))
	result, err := runtime.semanticModels.InstallPack(context.Background(), pack, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack(%q) error = %v", pack.ID, err)
	}
	if err := runtime.semanticModels.FinalizePackInstall(pack.ID, pack.VectorSpaceID, result.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(%q) error = %v", pack.ID, err)
	}
}

func assetProcessingSemanticVariantForRuntimeTest(modelID string, vectorSpaceID string) string {
	identity, _ := json.Marshal([2]string{modelID, vectorSpaceID})
	return "semantic-profile-v1:" + string(identity)
}

func semanticRuntimePackArtifactForRuntimeTest(t *testing.T) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	writeEntry := func(name string, payload []byte, mode os.FileMode) {
		t.Helper()
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("CreateHeader(%q) error = %v", name, err)
		}
		if _, err := entry.Write(payload); err != nil {
			t.Fatalf("write zip entry %q: %v", name, err)
		}
	}
	layout, err := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"product":       "timich-semantic-runtime-pack",
		"runtime":       "onnxruntime",
		"serverPath":    "server.py",
		"pythonPath":    "bin/python",
	})
	if err != nil {
		t.Fatalf("json.Marshal(runtime layout) error = %v", err)
	}
	writeEntry("timich-runtime.json", layout, 0o600)
	writeEntry("server.py", []byte("print('ready')\n"), 0o700)
	writeEntry("bin/python", []byte("#!/bin/sh\n"), 0o700)
	if err := writer.Close(); err != nil {
		t.Fatalf("zip.Close() error = %v", err)
	}
	return buffer.Bytes()
}

func installRuntimeSemanticPackForTest(t *testing.T, runtime *AgentRuntime, pack catalog.SemanticModelPackStatus) {
	t.Helper()
	runtime.semanticONNXRuntime.mu.Lock()
	if strings.TrimSpace(runtime.semanticONNXRuntime.cfg.ServerPath) == "" {
		serverPath, pythonPath := writeSemanticONNXRuntimeExecutables(t)
		runtime.semanticONNXRuntime.cfg.ServerPath = serverPath
		runtime.semanticONNXRuntime.cfg.PythonPath = pythonPath
	}
	runtime.semanticONNXRuntime.mu.Unlock()

	artifact := runtimeSemanticPackArtifactForTest(t, pack)
	sum := sha256.Sum256(artifact)
	pack.Artifact.SHA256 = hex.EncodeToString(sum[:])
	pack.Artifact.SizeBytes = int64(len(artifact))
	pack.SizeBytes = int64(len(artifact))
	if _, err := runtime.InstallSemanticModelPack(context.Background(), pack, bytes.NewReader(artifact)); err != nil {
		t.Fatalf("InstallSemanticModelPack(%q) error = %v", pack.ID, err)
	}
}

func semanticBinaryIndexFilesForRuntimeTest(t *testing.T, root string) []string {
	t.Helper()

	var files []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".tidx" {
			return nil
		}
		files = append(files, path)
		return nil
	}); err != nil {
		t.Fatalf("WalkDir(%s) error = %v", root, err)
	}
	return files
}

func runtimeSemanticPackArtifactForTest(t *testing.T, pack catalog.SemanticModelPackStatus) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	writeZipEntry := func(name string, payload []byte) {
		t.Helper()
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("Create(%q) error = %v", name, err)
		}
		if _, err := entry.Write(payload); err != nil {
			t.Fatalf("Write(%q) error = %v", name, err)
		}
	}
	layout := map[string]any{
		"schemaVersion": 1,
		"product":       "timich-semantic-model-pack",
		"modelId":       pack.ID,
		"vectorSpaceId": pack.VectorSpaceID,
		"embeddingDim":  pack.EmbeddingDim,
		"inputKind":     pack.InputKind,
		"runtime":       pack.Runtime,
		"imageModel":    "models/image.onnx",
		"textModel":     "models/text.onnx",
		"tokenizer":     "tokenizer/tokenizer.json",
	}
	raw, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	writeZipEntry("timich-model.json", raw)
	writeZipEntry("models/image.onnx", []byte("image model"))
	writeZipEntry("models/text.onnx", []byte("text model"))
	writeZipEntry("tokenizer/tokenizer.json", []byte(`{"model":"test"}`))
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return buffer.Bytes()
}

func writeRuntimeCandidateSelectionHelper(t *testing.T) string {
	t.Helper()

	helperPath := filepath.Join(t.TempDir(), "timich-semantic-helper")
	script := `#!/bin/sh
command="$1"
shift
layout=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --runtime-layout)
      layout="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
if grep -q 'zzz-new-model' "$layout/timich-model.json"; then
  case "$command" in
    inspect)
      printf '%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"zzz-new-model","vectorSpaceId":"zzz-new-model/d4","embeddingDim":4,"inputKind":"image","loaded":true,"canEmbed":true}'
      ;;
    embed-image)
      cat >/dev/null
      printf '%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"zzz-new-model","vectorSpaceId":"zzz-new-model/d4","embeddingDim":4,"inputKind":"image","vector":[1,0,0,0],"input":"image-helper"}'
      ;;
    embed-text)
      printf '%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"zzz-new-model","vectorSpaceId":"zzz-new-model/d4","embeddingDim":4,"inputKind":"image","vector":[0,1,0,0],"input":"text-helper"}'
      ;;
    *)
      exit 2
      ;;
  esac
else
  printf '%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"aaa-old-model","vectorSpaceId":"aaa-old-model/d4","embeddingDim":4,"inputKind":"image","loaded":false,"canEmbed":false,"messageCode":"semantic_runtime_test_unavailable"}'
fi
`
	if err := os.WriteFile(helperPath, []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return helperPath
}

func encodeRuntimeJPEGForTest(t *testing.T, width int, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 220, G: 180, B: 120, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("jpeg.Encode() error = %v", err)
	}
	return buffer.Bytes()
}

func writeRuntimeFakeFFmpegPosterScript(t *testing.T, posterPath string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "lavfi" ] || [ "$arg" = "color=c=black:s=16x16:d=0.1" ]; then
    echo "lavfi smoke is not supported by this fake runtime" >&2
    exit 12
  fi
  if [ "$arg" = "-version" ]; then
    echo "ffmpeg version test-ffmpeg"
    exit 0
  fi
  if [ "$arg" = "-decoders" ]; then
    echo " VFS..D h264 H.264"
    echo " VFS..D hevc HEVC"
    exit 0
  fi
done
output=""
for arg in "$@"; do
  output="$arg"
done
if [ -z "$output" ]; then
  exit 8
fi
cp ` + shellQuoteRuntimeTest(posterPath) + ` "$output"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake ffmpeg script: %v", err)
	}
	return path
}

func writeRuntimeFakeMediaHelperScript(t *testing.T, posterPath string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "timich-media-helper")
	quotedPosterPath := shellQuoteRuntimeTest(posterPath)
	script := `#!/bin/sh
case "$1" in
health)
  printf '%s\n' '{"schemaVersion":1,"ok":true,"helper":{"version":"0.1.0-test","platform":"test-platform"},"capabilities":{"renderImage":true,"renderVideoPoster":true,"inspectImage":false,"inspectVideo":false}}'
  exit 0
  ;;
render-image)
  shift
  input=""
  output=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
    --input)
      input="$2"
      shift 2
      ;;
    --output)
      output="$2"
      shift 2
      ;;
    *)
      shift
      ;;
    esac
  done
  if [ -z "$input" ] || [ -z "$output" ]; then
    echo "missing image input or output" >&2
    exit 8
  fi
  cp "$input" "$output"
  printf '%s\n' '{"schemaVersion":1,"ok":true,"operation":"render-image","backend":"libvips-cli","outputPath":"rendition.jpg"}'
  exit 0
  ;;
render-video-poster)
  shift
  output=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
    --output)
      output="$2"
      shift 2
      ;;
    *)
      shift
      ;;
    esac
  done
  if [ -z "$output" ]; then
    echo "missing poster output" >&2
    exit 8
  fi
  if [ -z ` + quotedPosterPath + ` ]; then
    echo "missing poster fixture" >&2
    exit 9
  fi
  cp ` + quotedPosterPath + ` "$output"
  printf '%s\n' '{"schemaVersion":1,"ok":true,"operation":"render-video-poster","backend":"ffmpeg-cli","outputPath":"poster.jpg"}'
  exit 0
  ;;
*)
  echo "unexpected command: $1" >&2
  exit 2
  ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake media helper script: %v", err)
	}
	return path
}

func shellQuoteRuntimeTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
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

	return newTestAgentRuntimeWithConfig(t, build, datasources, adminToken, nil)
}

func newTestAgentRuntimeWithConfig(
	t *testing.T,
	build BuildInfo,
	datasources []config.DatasourceConfig,
	adminToken string,
	configure func(*config.ResolvedConfig),
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
	if configure != nil {
		configure(&cfg)
	}

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

func runtimeTestIntPtr(value int) *int {
	return &value
}

func TestSemanticPriorityIndexPublishDue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status catalog.SemanticModelBackfillStatus
		want   bool
	}{
		{
			name: "no completed vectors",
			status: catalog.SemanticModelBackfillStatus{
				CompletedVectorCount: 0,
			},
		},
		{
			name: "first publish",
			status: catalog.SemanticModelBackfillStatus{
				CompletedVectorCount: 10,
				IndexedVectorCount:   4,
			},
			want: true,
		},
		{
			name: "below threshold waits",
			status: catalog.SemanticModelBackfillStatus{
				CompletedVectorCount: 119,
				IndexedVectorCount:   100,
				RemainingVectorCount: 0,
			},
		},
		{
			name: "ongoing embeddings wait below threshold",
			status: catalog.SemanticModelBackfillStatus{
				EligibleAssetCount:   100,
				CompletedVectorCount: 60,
				IndexedVectorCount:   55,
				RemainingVectorCount: 40,
			},
		},
		{
			name: "ongoing embeddings publish at threshold",
			status: catalog.SemanticModelBackfillStatus{
				EligibleAssetCount:   100,
				CompletedVectorCount: 66,
				IndexedVectorCount:   55,
				RemainingVectorCount: 34,
			},
			want: true,
		},
		{
			name: "existing publish job waits",
			status: catalog.SemanticModelBackfillStatus{
				CompletedVectorCount: 10,
				IndexedVectorCount:   4,
				PendingIndexJobCount: 1,
			},
			want: true,
		},
		{
			name: "failed publish job still counts as publish pressure",
			status: catalog.SemanticModelBackfillStatus{
				CompletedVectorCount: 10,
				IndexedVectorCount:   4,
				FailedIndexJobCount:  1,
			},
			want: true,
		},
		{
			name: "already indexed",
			status: catalog.SemanticModelBackfillStatus{
				CompletedVectorCount: 10,
				IndexedVectorCount:   10,
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := semanticPriorityIndexPublishDue(test.status); got != test.want {
				t.Fatalf("semanticPriorityIndexPublishDue() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSemanticIndexPartialPublishQueuedThreshold(t *testing.T) {
	t.Parallel()

	maxInt := int(^uint(0) >> 1)
	maxIntThreshold := maxInt / semanticIndexPartialPublishDivisor
	if maxInt%semanticIndexPartialPublishDivisor != 0 {
		maxIntThreshold++
	}
	tests := []struct {
		indexed int
		want    int
	}{
		{indexed: 0, want: 1},
		{indexed: 1, want: 1},
		{indexed: 5, want: 1},
		{indexed: 6, want: 2},
		{indexed: 100, want: 20},
		{indexed: 101, want: 21},
		{indexed: maxInt, want: maxIntThreshold},
	}
	for _, test := range tests {
		if got := semanticIndexPartialPublishQueuedThreshold(test.indexed); got != test.want {
			t.Fatalf("semanticIndexPartialPublishQueuedThreshold(%d) = %d, want %d", test.indexed, got, test.want)
		}
	}

	if target, wait := semanticIndexPartialPublishWaitTarget(1000, 50, 450, 0); !wait || target != 200 {
		t.Fatalf("semanticIndexPartialPublishWaitTarget(backfill) = %d, %t; want 200, true", target, wait)
	}
	if target, wait := semanticIndexPartialPublishWaitTarget(1000, 50, 0, 0); wait || target != 0 {
		t.Fatalf("semanticIndexPartialPublishWaitTarget(final flush) = %d, %t; want 0, false", target, wait)
	}
}
