package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/semanticruntimehelper"
)

const (
	semanticONNXRuntimeName              = "onnxruntime"
	defaultSemanticONNXRuntimeHost       = "127.0.0.1"
	defaultSemanticONNXRuntimePython     = "python3"
	defaultSemanticONNXRuntimeStopWait   = 5 * time.Second
	semanticONNXRuntimeRetryBase         = time.Second
	semanticONNXRuntimeRetryMax          = time.Minute
	semanticONNXRuntimeHealthStable      = 500 * time.Millisecond
	semanticONNXRuntimeStableRun         = 5 * time.Minute
	semanticONNXRuntimeStatusDisabled    = "disabled"
	semanticONNXRuntimeStatusUnavailable = "unavailable"
	semanticONNXRuntimeStatusIdle        = "idle"
	semanticONNXRuntimeStatusRunning     = "running"
	semanticONNXRuntimeStatusExited      = "exited"
	semanticONNXRuntimeStatusFailed      = "failed"
)

type SemanticRuntimeResponse struct {
	HelperPath  string                      `json:"helperPath,omitempty"`
	ONNXRuntime SemanticONNXRuntimeResponse `json:"onnxRuntime"`
}

type SemanticONNXRuntimeResponse struct {
	Managed       bool                         `json:"managed"`
	Status        string                       `json:"status"`
	Disabled      bool                         `json:"disabled,omitempty"`
	ServerPath    string                       `json:"serverPath,omitempty"`
	PythonPath    string                       `json:"pythonPath,omitempty"`
	Host          string                       `json:"host,omitempty"`
	Port          int                          `json:"port,omitempty"`
	Provider      string                       `json:"provider,omitempty"`
	TextProvider  string                       `json:"textProvider,omitempty"`
	ImageProvider string                       `json:"imageProvider,omitempty"`
	ProcessCount  int                          `json:"processCount"`
	MessageCode   string                       `json:"messageCode,omitempty"`
	Runtimes      []SemanticONNXRuntimeProcess `json:"runtimes,omitempty"`
}

type SemanticONNXRuntimeProcess struct {
	ModelID       string     `json:"modelId"`
	VectorSpaceID string     `json:"vectorSpaceId"`
	RuntimePath   string     `json:"runtimePath,omitempty"`
	ServerURL     string     `json:"serverURL,omitempty"`
	EnvKey        string     `json:"envKey,omitempty"`
	PID           int        `json:"pid,omitempty"`
	Status        string     `json:"status"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	ExitedAt      *time.Time `json:"exitedAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
}

type semanticONNXRuntimeManager struct {
	cfg                config.SemanticONNXRuntimeConfig
	modelStore         *catalog.SemanticModelPackStore
	runtimePackStore   *catalog.SemanticRuntimePackStore
	mu                 sync.Mutex
	topologyGen        uint64
	indexingConfigured bool
	closed             bool
	processes          map[string]*managedSemanticONNXRuntimeProcess
	retries            map[string]*semanticONNXRuntimeRetry
	onStateChange      func()
}

type semanticONNXRuntimeRetry struct {
	attempts int
	timer    *time.Timer
}

type managedSemanticONNXRuntimeProcess struct {
	layout         catalog.SemanticModelRuntimeLayout
	launchIdentity semanticONNXRuntimeLaunchIdentity
	key            string
	envKey         string
	legacyEnvKey   string
	serverURL      string
	port           int
	cmd            *exec.Cmd
	done           chan struct{}
	startedAt      time.Time
	exitedAt       time.Time
	status         string
	lastError      string
	envInstalled   bool
	legacyEnvUsed  bool
}

type semanticONNXRuntimeLaunchIdentity struct {
	RuntimePath   string
	ServerPath    string
	PythonPath    string
	Host          string
	Port          int
	Provider      string
	TextProvider  string
	ImageProvider string
	TextTemplate  string
}

type semanticONNXRuntimeLaunchSpec struct {
	ServerPath    string
	PythonPath    string
	Provider      string
	TextProvider  string
	ImageProvider string
	TextTemplate  string
}

// StartSemanticModelRuntime starts Agent-managed semantic model runtime servers
// for installed helper-backed model packs.
func (a *AgentRuntime) StartSemanticModelRuntime() {
	a.reconcileSemanticModelRuntime()
}

func (a *AgentRuntime) reconcileSemanticModelRuntime() {
	if a == nil || a.semanticONNXRuntime == nil {
		return
	}
	a.mu.RLock()
	topologyGeneration := a.semanticTopologyGeneration
	indexingConfigured := a.datasourceIndexingConfiguredLocked()
	a.mu.RUnlock()
	a.semanticONNXRuntime.Reconcile(context.Background(), topologyGeneration, indexingConfigured)
}

func newSemanticONNXRuntimeManager(cfg config.SemanticONNXRuntimeConfig, modelStore *catalog.SemanticModelPackStore, runtimePackStore *catalog.SemanticRuntimePackStore) *semanticONNXRuntimeManager {
	return &semanticONNXRuntimeManager{
		cfg:              cfg,
		modelStore:       modelStore,
		runtimePackStore: runtimePackStore,
		processes:        make(map[string]*managedSemanticONNXRuntimeProcess),
		retries:          make(map[string]*semanticONNXRuntimeRetry),
	}
}

func (m *semanticONNXRuntimeManager) SetStateChangeCallback(callback func()) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.onStateChange = callback
	m.mu.Unlock()
}

func (m *semanticONNXRuntimeManager) Reconcile(ctx context.Context, topologyGeneration uint64, indexingConfigured bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}

	if topologyGeneration < m.topologyGen {
		return
	}
	if topologyGeneration > m.topologyGen {
		m.cancelAllRetriesLocked()
	}
	m.topologyGen = topologyGeneration
	m.indexingConfigured = indexingConfigured

	if !indexingConfigured {
		m.stopAllLocked()
		return
	}

	cfg := m.effectiveConfigLocked()
	if cfg.Disabled {
		m.stopAllLocked()
		return
	}

	layouts := m.desiredLayoutsLocked()
	desired := make(map[string]catalog.SemanticModelRuntimeLayout, len(layouts))
	for _, layout := range layouts {
		desired[semanticONNXRuntimeKey(layout)] = layout
	}
	for key, process := range m.processes {
		if _, ok := desired[key]; !ok || process.launchIdentityChanged(cfg, desired[key]) {
			m.stopProcessLocked(process)
			delete(m.processes, key)
			m.cancelRetryLocked(key)
		}
	}
	for key := range m.retries {
		if _, ok := desired[key]; !ok {
			m.cancelRetryLocked(key)
		}
	}
	for _, layout := range layouts {
		key := semanticONNXRuntimeKey(layout)
		if process, ok := m.processes[key]; ok && process.status == semanticONNXRuntimeStatusRunning {
			continue
		}
		if retry := m.retries[key]; retry != nil && retry.timer != nil {
			continue
		}
		if process, ok := m.processes[key]; ok && (process.status == semanticONNXRuntimeStatusExited || process.status == semanticONNXRuntimeStatusFailed) {
			delete(m.processes, key)
		}
		offset := m.nextAvailablePortOffsetLocked(cfg)
		process, err := m.startProcessLocked(ctx, cfg, layout, offset)
		if err != nil {
			log.Printf("timich-agent semantic ONNX runtime start failed model=%s vector_space=%s error=%v", layout.ModelID, layout.VectorSpaceID, err)
			m.processes[key] = failedSemanticONNXRuntimeProcess(layout, err)
			m.scheduleRetryLocked(key)
			continue
		}
		m.processes[key] = process
		m.notifyStateChangeLocked()
	}
}

func (m *semanticONNXRuntimeManager) nextAvailablePortOffsetLocked(cfg config.SemanticONNXRuntimeConfig) int {
	if cfg.Port <= 0 {
		return 0
	}
	used := make(map[int]struct{}, len(m.processes))
	for _, process := range m.processes {
		if process == nil || process.port < cfg.Port {
			continue
		}
		used[process.port-cfg.Port] = struct{}{}
	}
	for offset := 0; cfg.Port+offset <= 65535; offset++ {
		if _, exists := used[offset]; !exists {
			return offset
		}
	}
	return 65536
}

func (m *semanticONNXRuntimeManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.cancelAllRetriesLocked()
	m.stopAllLocked()
}

func (m *semanticONNXRuntimeManager) InvalidateLaunchIdentities() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, process := range m.processes {
		process.launchIdentity = semanticONNXRuntimeLaunchIdentity{}
	}
}

func (m *semanticONNXRuntimeManager) Status() SemanticONNXRuntimeResponse {
	if m == nil {
		return SemanticONNXRuntimeResponse{
			Managed:     false,
			Status:      semanticONNXRuntimeStatusUnavailable,
			MessageCode: "semantic_onnx_runtime_manager_missing",
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg := m.effectiveConfigLocked()
	response := SemanticONNXRuntimeResponse{
		Managed:       !cfg.Disabled,
		Disabled:      cfg.Disabled,
		ServerPath:    strings.TrimSpace(cfg.ServerPath),
		PythonPath:    strings.TrimSpace(cfg.PythonPath),
		Host:          semanticONNXRuntimeHost(cfg),
		Port:          cfg.Port,
		Provider:      strings.TrimSpace(cfg.Provider),
		TextProvider:  strings.TrimSpace(cfg.TextProvider),
		ImageProvider: strings.TrimSpace(cfg.ImageProvider),
		ProcessCount:  len(m.processes),
	}
	switch {
	case cfg.Disabled:
		response.Status = semanticONNXRuntimeStatusDisabled
		response.MessageCode = "semantic_onnx_runtime_disabled"
	case strings.TrimSpace(cfg.ServerPath) == "":
		response.Status = semanticONNXRuntimeStatusUnavailable
		response.MessageCode = "semantic_onnx_runtime_server_missing"
	case len(m.processes) == 0:
		response.Status = semanticONNXRuntimeStatusIdle
		response.MessageCode = "semantic_onnx_runtime_idle"
	default:
		response.Status = semanticONNXRuntimeStatusRunning
		for _, process := range m.processes {
			if process.status == semanticONNXRuntimeStatusFailed || process.status == semanticONNXRuntimeStatusExited {
				response.Status = process.status
			}
			response.Runtimes = append(response.Runtimes, process.summary())
		}
	}
	return response
}

// ProbeRuntimePack executes the replacement artifact directly. It never uses
// the ambient managed process or configured ServerPath/PythonPath, so disabled
// indexing and explicit operator overrides cannot approve unrelated bytes.
func (m *semanticONNXRuntimeManager) ProbeRuntimePack(ctx context.Context, pack catalog.SemanticRuntimePackStatus, installID string) error {
	if m == nil {
		return errors.New("semantic runtime manager is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	cfg = resolveSemanticONNXRuntimeConfig(cfg, &pack)
	spec, err := semanticONNXRuntimeLaunchSpecForConfig(cfg)
	if err != nil {
		return fmt.Errorf("semantic runtime pack %s: %w", installID, err)
	}
	for label, path := range map[string]string{"server": spec.ServerPath, "python": spec.PythonPath} {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("semantic runtime pack %s %s executable: %w", installID, label, err)
		}
		if info.IsDir() {
			return fmt.Errorf("semantic runtime pack %s %s executable is a directory", installID, label)
		}
	}

	layouts := []catalog.SemanticModelRuntimeLayout{}
	if m.modelStore != nil {
		for _, candidate := range m.modelStore.InstalledRuntimeLayouts() {
			if strings.TrimSpace(candidate.Runtime) == strings.TrimSpace(pack.Runtime) {
				layouts = append(layouts, candidate)
			}
		}
	}
	if len(layouts) == 0 {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		command := exec.CommandContext(probeCtx, spec.PythonPath, spec.ServerPath, "--help")
		command.Env = semanticONNXRuntimeCommandEnv("TIMICH_ONNX_SERVER_URL_PROBE", "http://127.0.0.1:0", spec.PythonPath)
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Run(); err != nil {
			return fmt.Errorf("semantic runtime pack %s executable probe failed: %w", installID, err)
		}
		return nil
	}
	for _, layout := range layouts {
		if err := probeSemanticONNXRuntimeLayout(ctx, "runtime pack "+installID, spec, layout); err != nil {
			return err
		}
	}
	return nil
}

// ProbeModelLayout executes both embedding graphs for the exact staged model
// layout using the active runtime-pack bytes or explicit operator runtime.
func (m *semanticONNXRuntimeManager) ProbeModelLayout(ctx context.Context, layout catalog.SemanticModelRuntimeLayout, installID string) error {
	if m == nil {
		return errors.New("semantic runtime manager is unavailable")
	}
	m.mu.Lock()
	cfg := m.effectiveConfigLocked()
	m.mu.Unlock()
	spec, err := semanticONNXRuntimeLaunchSpecForConfig(cfg)
	if err != nil {
		return err
	}
	return probeSemanticONNXRuntimeLayout(ctx, "model pack "+installID, spec, layout)
}

func probeSemanticONNXRuntimeLayout(ctx context.Context, label string, spec semanticONNXRuntimeLaunchSpec, layout catalog.SemanticModelRuntimeLayout) error {
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("allocate semantic runtime probe port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	serverURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	command := exec.Command(spec.PythonPath, spec.commandArgs(layout, "127.0.0.1", port)...)
	command.Env = semanticONNXRuntimeCommandEnv("TIMICH_ONNX_SERVER_URL_PROBE", serverURL, spec.PythonPath)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return fmt.Errorf("start semantic %s probe: %w", label, err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	exited := false
	defer func() {
		if exited || command.Process == nil {
			return
		}
		_ = command.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(defaultSemanticONNXRuntimeStopWait):
			_ = command.Process.Kill()
			<-done
		}
	}()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var healthySince time.Time
	for {
		if semanticONNXRuntimeEndpointHealthy(serverURL) {
			if healthySince.IsZero() {
				healthySince = time.Now()
			} else if time.Since(healthySince) >= semanticONNXRuntimeHealthStable {
				if err := probeSemanticONNXRuntimeEmbeddingContract(probeCtx, serverURL, layout); err != nil {
					return fmt.Errorf("semantic %s embedding probe: %w", label, err)
				}
				return nil
			}
		} else {
			healthySince = time.Time{}
		}
		select {
		case err := <-done:
			exited = true
			if err == nil {
				err = errors.New("process exited before health was stable")
			}
			return fmt.Errorf("semantic %s probe failed: %w", label, err)
		case <-probeCtx.Done():
			return fmt.Errorf("semantic %s health probe: %w", label, probeCtx.Err())
		case <-ticker.C:
		}
	}
}

func probeSemanticONNXRuntimeEmbeddingContract(ctx context.Context, serverURL string, layout catalog.SemanticModelRuntimeLayout) error {
	expected, err := semanticruntimehelper.InspectRuntimeLayout(layout.RuntimePath)
	if err != nil {
		return err
	}
	rawLayout, err := os.ReadFile(filepath.Join(layout.RuntimePath, "timich-model.json"))
	if err != nil {
		return fmt.Errorf("read model runtime layout: %w", err)
	}
	var layoutPayload map[string]any
	if err := json.Unmarshal(rawLayout, &layoutPayload); err != nil {
		return fmt.Errorf("decode model runtime layout: %w", err)
	}
	basePayload := map[string]any{
		"layout":        layoutPayload,
		"runtimeLayout": layout.RuntimePath,
	}
	textPayload := cloneSemanticRuntimeProbePayload(basePayload)
	textPayload["text"] = "timich semantic runtime health"
	textResponse, err := postSemanticRuntimeProbe[semanticruntimehelper.EmbeddingResponse](ctx, serverURL, "/embed-text", textPayload)
	if err != nil {
		return err
	}
	if err := semanticruntimehelper.ValidateEmbeddingResponse(textResponse, expected); err != nil {
		return fmt.Errorf("validate text embedding: %w", err)
	}
	imagePayload := cloneSemanticRuntimeProbePayload(basePayload)
	imagePayload["contentType"] = "image/png"
	imagePayload["source"] = "install-probe.png"
	imagePayload["imageBase64"] = base64.StdEncoding.EncodeToString(semanticRuntimeProbePNG())
	imageResponse, err := postSemanticRuntimeProbe[semanticruntimehelper.EmbeddingResponse](ctx, serverURL, "/embed-image", imagePayload)
	if err != nil {
		return err
	}
	if err := semanticruntimehelper.ValidateEmbeddingResponse(imageResponse, expected); err != nil {
		return fmt.Errorf("validate image embedding: %w", err)
	}
	return nil
}

func cloneSemanticRuntimeProbePayload(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source)+3)
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func postSemanticRuntimeProbe[T any](ctx context.Context, serverURL string, path string, payload map[string]any) (T, error) {
	var responsePayload T
	raw, err := json.Marshal(payload)
	if err != nil {
		return responsePayload, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(serverURL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return responsePayload, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return responsePayload, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return responsePayload, fmt.Errorf("%s returned %s: %s", path, response.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&responsePayload); err != nil {
		return responsePayload, err
	}
	return responsePayload, nil
}

func semanticRuntimeProbePNG() []byte {
	imageValue := image.NewRGBA(image.Rect(0, 0, 1, 1))
	imageValue.Set(0, 0, color.RGBA{R: 127, G: 191, B: 255, A: 255})
	var output bytes.Buffer
	_ = png.Encode(&output, imageValue)
	return output.Bytes()
}

func semanticONNXRuntimeEndpointHealthy(serverURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(serverURL, "/")+"/healthz", nil)
	if err != nil {
		return false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&payload); err != nil {
		return false
	}
	return strings.TrimSpace(payload.Status) == "ok"
}

func (m *semanticONNXRuntimeManager) desiredLayoutsLocked() []catalog.SemanticModelRuntimeLayout {
	if m.modelStore == nil {
		return nil
	}
	layouts := m.modelStore.InstalledRuntimeLayouts()
	desired := make([]catalog.SemanticModelRuntimeLayout, 0, len(layouts))
	for _, layout := range layouts {
		if strings.TrimSpace(layout.Runtime) != semanticONNXRuntimeName {
			continue
		}
		if strings.TrimSpace(layout.RuntimePath) == "" {
			continue
		}
		desired = append(desired, layout)
	}
	return desired
}

func (m *semanticONNXRuntimeManager) effectiveConfigLocked() config.SemanticONNXRuntimeConfig {
	cfg := m.cfg
	if m.runtimePackStore == nil {
		return cfg
	}
	pack, ok := m.runtimePackStore.InstalledPack(semanticONNXRuntimeName)
	if !ok {
		return cfg
	}
	return resolveSemanticONNXRuntimeConfig(cfg, &pack)
}

// resolveSemanticONNXRuntimeConfig selects one launch pair for production and
// probes. Explicit operator values win independently while the operator
// runtime is enabled. Installed packs replace missing, auto-detected, or
// disabled-runtime halves so a candidate can still prove its own executables.
// An empty Python path remains a documented request to resolve python3 on PATH.
func resolveSemanticONNXRuntimeConfig(cfg config.SemanticONNXRuntimeConfig, pack *catalog.SemanticRuntimePackStatus) config.SemanticONNXRuntimeConfig {
	if pack == nil {
		return cfg
	}
	explicitServer := !cfg.Disabled && strings.TrimSpace(cfg.ServerPath) != "" && !cfg.ServerAutoDetected
	explicitPython := !cfg.Disabled && strings.TrimSpace(cfg.PythonPath) != "" && !cfg.PythonAutoDetected
	if !explicitServer {
		if serverPath := strings.TrimSpace(pack.ServerPath); serverPath != "" {
			cfg.ServerPath = serverPath
			cfg.ServerAutoDetected = false
		}
	}
	if !explicitPython {
		if pythonPath := strings.TrimSpace(pack.PythonPath); pythonPath != "" {
			cfg.PythonPath = pythonPath
			cfg.PythonAutoDetected = false
		}
	}
	return cfg
}

func (m *semanticONNXRuntimeManager) startProcessLocked(ctx context.Context, cfg config.SemanticONNXRuntimeConfig, layout catalog.SemanticModelRuntimeLayout, offset int) (*managedSemanticONNXRuntimeProcess, error) {
	_ = ctx
	spec, err := semanticONNXRuntimeLaunchSpecForConfig(cfg)
	if err != nil {
		return nil, err
	}
	host := semanticONNXRuntimeHost(cfg)
	port, err := semanticONNXRuntimePort(cfg, offset)
	if err != nil {
		return nil, err
	}
	serverURL := semanticONNXRuntimeServerURL(host, port)
	key := semanticONNXRuntimeKey(layout)
	envKey := semanticONNXRuntimeEnvKey(layout.ModelID, layout.VectorSpaceID)
	legacyEnvKey := semanticONNXRuntimeEnvKey(layout.ModelID, "")

	command := exec.Command(spec.PythonPath, spec.commandArgs(layout, host, port)...)
	command.Env = semanticONNXRuntimeCommandEnv(envKey, serverURL, spec.PythonPath)
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start semantic ONNX runtime server: %w", err)
	}
	process := &managedSemanticONNXRuntimeProcess{
		layout:         layout,
		launchIdentity: semanticONNXRuntimeLaunchIdentityFor(cfg, layout),
		key:            key,
		envKey:         envKey,
		legacyEnvKey:   legacyEnvKey,
		serverURL:      serverURL,
		port:           port,
		cmd:            command,
		done:           make(chan struct{}),
		startedAt:      time.Now().UTC(),
		status:         semanticONNXRuntimeStatusRunning,
	}
	process.installEnvLocked()
	go m.waitForProcess(key, process, command)
	log.Printf("timich-agent semantic ONNX runtime started model=%s vector_space=%s url=%s pid=%d", layout.ModelID, layout.VectorSpaceID, serverURL, command.Process.Pid)
	return process, nil
}

func (m *semanticONNXRuntimeManager) waitForProcess(key string, processRef *managedSemanticONNXRuntimeProcess, command *exec.Cmd) {
	err := command.Wait()
	close(processRef.done)
	m.mu.Lock()
	process := m.processes[key]
	if process == nil || process.cmd != command {
		m.mu.Unlock()
		return
	}
	process.exitedAt = time.Now().UTC()
	process.status = semanticONNXRuntimeStatusExited
	if err != nil {
		process.status = semanticONNXRuntimeStatusFailed
		process.lastError = err.Error()
	}
	process.uninstallEnvLocked()
	if time.Since(process.startedAt) >= semanticONNXRuntimeStableRun {
		m.cancelRetryLocked(key)
	}
	m.scheduleRetryLocked(key)
	m.notifyStateChangeLocked()
	m.mu.Unlock()
}

func (m *semanticONNXRuntimeManager) stopAllLocked() {
	m.cancelAllRetriesLocked()
	for key, process := range m.processes {
		m.stopProcessLocked(process)
		delete(m.processes, key)
	}
}

func (m *semanticONNXRuntimeManager) scheduleRetryLocked(key string) {
	if m.closed || !m.indexingConfigured || strings.TrimSpace(key) == "" {
		return
	}
	retry := m.retries[key]
	if retry == nil {
		retry = &semanticONNXRuntimeRetry{}
		m.retries[key] = retry
	}
	if retry.timer != nil {
		return
	}
	retry.attempts++
	delay := semanticONNXRuntimeRetryBase
	for attempt := 1; attempt < retry.attempts && delay < semanticONNXRuntimeRetryMax; attempt++ {
		delay *= 2
		if delay > semanticONNXRuntimeRetryMax {
			delay = semanticONNXRuntimeRetryMax
		}
	}
	generation := m.topologyGen
	retry.timer = time.AfterFunc(delay, func() {
		m.retryProcess(key, generation)
	})
}

func (m *semanticONNXRuntimeManager) retryProcess(key string, generation uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	retry := m.retries[key]
	if retry != nil {
		retry.timer = nil
	}
	if m.closed || !m.indexingConfigured || generation != m.topologyGen {
		m.mu.Unlock()
		return
	}
	cfg := m.effectiveConfigLocked()
	if cfg.Disabled {
		m.mu.Unlock()
		return
	}
	var desired *catalog.SemanticModelRuntimeLayout
	for _, layout := range m.desiredLayoutsLocked() {
		if semanticONNXRuntimeKey(layout) == key {
			value := layout
			desired = &value
			break
		}
	}
	if desired == nil {
		m.cancelRetryLocked(key)
		m.mu.Unlock()
		return
	}
	if process := m.processes[key]; process != nil && process.status == semanticONNXRuntimeStatusRunning {
		m.mu.Unlock()
		return
	}
	delete(m.processes, key)
	process, err := m.startProcessLocked(context.Background(), cfg, *desired, m.nextAvailablePortOffsetLocked(cfg))
	if err != nil {
		log.Printf("timich-agent semantic ONNX runtime restart failed model=%s vector_space=%s error=%v", desired.ModelID, desired.VectorSpaceID, err)
		m.processes[key] = failedSemanticONNXRuntimeProcess(*desired, err)
		m.scheduleRetryLocked(key)
		m.notifyStateChangeLocked()
		m.mu.Unlock()
		return
	}
	m.processes[key] = process
	m.notifyStateChangeLocked()
	m.mu.Unlock()
}

func (m *semanticONNXRuntimeManager) cancelRetryLocked(key string) {
	retry := m.retries[key]
	if retry != nil && retry.timer != nil {
		retry.timer.Stop()
	}
	delete(m.retries, key)
}

func (m *semanticONNXRuntimeManager) cancelAllRetriesLocked() {
	for key := range m.retries {
		m.cancelRetryLocked(key)
	}
}

func (m *semanticONNXRuntimeManager) notifyStateChangeLocked() {
	callback := m.onStateChange
	if callback != nil {
		go callback()
	}
}

func (m *semanticONNXRuntimeManager) stopProcessLocked(process *managedSemanticONNXRuntimeProcess) {
	if process == nil {
		return
	}
	process.uninstallEnvLocked()
	if process.cmd == nil || process.cmd.Process == nil || process.status != semanticONNXRuntimeStatusRunning {
		return
	}
	_ = process.cmd.Process.Signal(os.Interrupt)
	select {
	case <-process.done:
	case <-time.After(defaultSemanticONNXRuntimeStopWait):
		_ = process.cmd.Process.Kill()
		<-process.done
	}
	process.status = semanticONNXRuntimeStatusExited
	now := time.Now().UTC()
	process.exitedAt = now
}

func (p *managedSemanticONNXRuntimeProcess) launchIdentityChanged(cfg config.SemanticONNXRuntimeConfig, layout catalog.SemanticModelRuntimeLayout) bool {
	if p == nil {
		return true
	}
	return p.launchIdentity != semanticONNXRuntimeLaunchIdentityFor(cfg, layout)
}

func semanticONNXRuntimeLaunchIdentityFor(cfg config.SemanticONNXRuntimeConfig, layout catalog.SemanticModelRuntimeLayout) semanticONNXRuntimeLaunchIdentity {
	pythonPath := strings.TrimSpace(cfg.PythonPath)
	if pythonPath == "" {
		pythonPath = defaultSemanticONNXRuntimePython
	}
	return semanticONNXRuntimeLaunchIdentity{
		RuntimePath:   strings.TrimSpace(layout.RuntimePath),
		ServerPath:    strings.TrimSpace(cfg.ServerPath),
		PythonPath:    pythonPath,
		Host:          semanticONNXRuntimeHost(cfg),
		Port:          cfg.Port,
		Provider:      strings.TrimSpace(cfg.Provider),
		TextProvider:  strings.TrimSpace(cfg.TextProvider),
		ImageProvider: strings.TrimSpace(cfg.ImageProvider),
		TextTemplate:  strings.TrimSpace(cfg.TextTemplate),
	}
}

func (p *managedSemanticONNXRuntimeProcess) installEnvLocked() {
	if p == nil || p.envInstalled {
		return
	}
	_ = os.Setenv(p.envKey, p.serverURL)
	if p.legacyEnvKey != "" && strings.TrimSpace(os.Getenv(p.legacyEnvKey)) == "" {
		_ = os.Setenv(p.legacyEnvKey, p.serverURL)
		p.legacyEnvUsed = true
	}
	p.envInstalled = true
}

func (p *managedSemanticONNXRuntimeProcess) uninstallEnvLocked() {
	if p == nil || !p.envInstalled {
		return
	}
	if strings.TrimSpace(os.Getenv(p.envKey)) == p.serverURL {
		_ = os.Unsetenv(p.envKey)
	}
	if p.legacyEnvUsed && strings.TrimSpace(os.Getenv(p.legacyEnvKey)) == p.serverURL {
		_ = os.Unsetenv(p.legacyEnvKey)
	}
	p.envInstalled = false
}

func (p *managedSemanticONNXRuntimeProcess) summary() SemanticONNXRuntimeProcess {
	if p == nil {
		return SemanticONNXRuntimeProcess{}
	}
	startedAt := p.startedAt
	var exitedAt *time.Time
	if !p.exitedAt.IsZero() {
		exited := p.exitedAt
		exitedAt = &exited
	}
	pid := 0
	if p.cmd != nil && p.cmd.Process != nil {
		pid = p.cmd.Process.Pid
	}
	return SemanticONNXRuntimeProcess{
		ModelID:       p.layout.ModelID,
		VectorSpaceID: p.layout.VectorSpaceID,
		RuntimePath:   p.layout.RuntimePath,
		ServerURL:     p.serverURL,
		EnvKey:        p.envKey,
		PID:           pid,
		Status:        p.status,
		StartedAt:     &startedAt,
		ExitedAt:      exitedAt,
		LastError:     p.lastError,
	}
}

func failedSemanticONNXRuntimeProcess(layout catalog.SemanticModelRuntimeLayout, err error) *managedSemanticONNXRuntimeProcess {
	return &managedSemanticONNXRuntimeProcess{
		layout:    layout,
		key:       semanticONNXRuntimeKey(layout),
		envKey:    semanticONNXRuntimeEnvKey(layout.ModelID, layout.VectorSpaceID),
		status:    semanticONNXRuntimeStatusFailed,
		lastError: err.Error(),
	}
}

func semanticONNXRuntimeCommandEnv(envKey string, serverURL string, pythonPath string) []string {
	env := os.Environ()
	if pythonHome := semanticONNXRuntimePythonHome(pythonPath); pythonHome != "" {
		env = append(env, "PYTHONHOME="+pythonHome, "PYTHONNOUSERSITE=1")
		if key, value := semanticONNXRuntimeLibraryEnv(pythonHome); key != "" && value != "" {
			env = append(env, prependPathEnv(key, value))
		}
	}
	env = append(env, envKey+"="+serverURL)
	return env
}

func semanticONNXRuntimePythonHome(pythonPath string) string {
	pythonPath = strings.TrimSpace(pythonPath)
	if pythonPath == "" || !filepath.IsAbs(pythonPath) {
		return ""
	}
	root := filepath.Dir(filepath.Dir(pythonPath))
	if _, err := os.Stat(filepath.Join(root, "pyvenv.cfg")); err != nil {
		return ""
	}
	if info, err := os.Stat(filepath.Join(root, "lib")); err != nil || !info.IsDir() {
		return ""
	}
	if !semanticONNXRuntimeHasBundledStdlib(root) {
		return ""
	}
	return root
}

func semanticONNXRuntimeHasBundledStdlib(root string) bool {
	if semanticONNXRuntimeFileExists(filepath.Join(root, "Lib", "encodings", "__init__.py")) {
		return true
	}
	entries, err := os.ReadDir(filepath.Join(root, "lib"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "python") {
			continue
		}
		if semanticONNXRuntimeFileExists(filepath.Join(root, "lib", entry.Name(), "encodings", "__init__.py")) {
			return true
		}
	}
	return false
}

func semanticONNXRuntimeFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func semanticONNXRuntimeLibraryEnv(pythonHome string) (string, string) {
	if pythonHome == "" || goruntime.GOOS != "linux" {
		return "", ""
	}
	libraryPath := filepath.Join(pythonHome, "lib")
	if info, err := os.Stat(libraryPath); err != nil || !info.IsDir() {
		return "", ""
	}
	return "LD_LIBRARY_PATH", libraryPath
}

func prependPathEnv(key string, value string) string {
	if previous := strings.TrimSpace(os.Getenv(key)); previous != "" {
		value += string(os.PathListSeparator) + previous
	}
	return key + "=" + value
}

func semanticONNXRuntimeLaunchSpecForConfig(cfg config.SemanticONNXRuntimeConfig) (semanticONNXRuntimeLaunchSpec, error) {
	serverPath := strings.TrimSpace(cfg.ServerPath)
	if serverPath == "" {
		return semanticONNXRuntimeLaunchSpec{}, errors.New("semantic ONNX runtime server path is not configured")
	}
	if info, err := os.Stat(serverPath); err != nil {
		return semanticONNXRuntimeLaunchSpec{}, fmt.Errorf("semantic ONNX runtime server missing: %w", err)
	} else if info.IsDir() {
		return semanticONNXRuntimeLaunchSpec{}, errors.New("semantic ONNX runtime server path is a directory")
	}
	pythonPath, err := semanticONNXRuntimePythonPath(cfg)
	if err != nil {
		return semanticONNXRuntimeLaunchSpec{}, err
	}
	return semanticONNXRuntimeLaunchSpec{
		ServerPath:    serverPath,
		PythonPath:    pythonPath,
		Provider:      strings.TrimSpace(cfg.Provider),
		TextProvider:  strings.TrimSpace(cfg.TextProvider),
		ImageProvider: strings.TrimSpace(cfg.ImageProvider),
		TextTemplate:  strings.TrimSpace(cfg.TextTemplate),
	}, nil
}

func (s semanticONNXRuntimeLaunchSpec) commandArgs(layout catalog.SemanticModelRuntimeLayout, host string, port int) []string {
	args := []string{
		s.ServerPath,
		"--runtime-layout", layout.RuntimePath,
		"--host", host,
		"--port", fmt.Sprintf("%d", port),
	}
	if s.Provider != "" {
		args = append(args, "--provider", s.Provider)
	}
	if s.TextProvider != "" {
		args = append(args, "--text-provider", s.TextProvider)
	}
	if s.ImageProvider != "" {
		args = append(args, "--image-provider", s.ImageProvider)
	}
	if s.TextTemplate != "" {
		args = append(args, "--text-template", s.TextTemplate)
	}
	return args
}

func semanticONNXRuntimePythonPath(cfg config.SemanticONNXRuntimeConfig) (string, error) {
	pythonPath := strings.TrimSpace(cfg.PythonPath)
	if pythonPath == "" {
		pythonPath = defaultSemanticONNXRuntimePython
	}
	if strings.ContainsAny(pythonPath, `/\`) || filepath.IsAbs(pythonPath) {
		if info, err := os.Stat(pythonPath); err != nil {
			return "", fmt.Errorf("semantic ONNX runtime python missing: %w", err)
		} else if info.IsDir() {
			return "", errors.New("semantic ONNX runtime python path is a directory")
		}
		return pythonPath, nil
	}
	resolved, err := exec.LookPath(pythonPath)
	if err != nil {
		return "", fmt.Errorf("semantic ONNX runtime python not found: %w", err)
	}
	return resolved, nil
}

func semanticONNXRuntimeHost(cfg config.SemanticONNXRuntimeConfig) string {
	if host := strings.TrimSpace(cfg.Host); host != "" {
		return host
	}
	return defaultSemanticONNXRuntimeHost
}

func semanticONNXRuntimePort(cfg config.SemanticONNXRuntimeConfig, offset int) (int, error) {
	if cfg.Port > 0 {
		port := cfg.Port + offset
		if port <= 0 || port > 65535 {
			return 0, errors.New("semantic ONNX runtime port range is exhausted")
		}
		return port, nil
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(semanticONNXRuntimeHost(cfg), "0"))
	if err != nil {
		return 0, fmt.Errorf("allocate semantic ONNX runtime port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func semanticONNXRuntimeServerURL(host string, port int) string {
	return "http://" + net.JoinHostPort(strings.TrimSpace(host), strconv.Itoa(port))
}

func semanticONNXRuntimeKey(layout catalog.SemanticModelRuntimeLayout) string {
	return strings.TrimSpace(layout.ModelID) + "\x00" + strings.TrimSpace(layout.VectorSpaceID)
}

func semanticONNXRuntimeEnvKey(modelID string, vectorSpaceID string) string {
	return semanticruntimehelper.ONNXRuntimeServerEnvKey(modelID, vectorSpaceID)
}
