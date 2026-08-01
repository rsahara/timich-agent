package runtime

import (
	"fmt"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

// SystemResourceResponse reports lightweight host resource status for the
// Admin UI task dashboard.
type SystemResourceResponse struct {
	CheckedAt time.Time            `json:"checkedAt"`
	CPU       SystemCPUResource    `json:"cpu"`
	Memory    SystemMemoryResource `json:"memory"`
	Disks     []SystemDiskResource `json:"disks"`
}

type SystemCPUResource struct {
	LogicalCores       int      `json:"logicalCores"`
	Load1              *float64 `json:"load1,omitempty"`
	Load5              *float64 `json:"load5,omitempty"`
	Load15             *float64 `json:"load15,omitempty"`
	Load1Percent       *float64 `json:"load1Percent,omitempty"`
	UsagePercent       *float64 `json:"usagePercent,omitempty"`
	IOWaitPercent      *float64 `json:"ioWaitPercent,omitempty"`
	TemperatureCelsius *float64 `json:"temperatureCelsius,omitempty"`
	TemperatureLabel   string   `json:"temperatureLabel,omitempty"`
	Message            string   `json:"message,omitempty"`
}

type SystemMemoryResource struct {
	TotalBytes     int64    `json:"totalBytes,omitempty"`
	AvailableBytes int64    `json:"availableBytes,omitempty"`
	UsedBytes      int64    `json:"usedBytes,omitempty"`
	UsedPercent    *float64 `json:"usedPercent,omitempty"`
	Message        string   `json:"message,omitempty"`
}

type SystemDiskResource struct {
	Label          string   `json:"label"`
	Path           string   `json:"path"`
	TotalBytes     int64    `json:"totalBytes,omitempty"`
	AvailableBytes int64    `json:"availableBytes,omitempty"`
	UsedBytes      int64    `json:"usedBytes,omitempty"`
	UsedPercent    *float64 `json:"usedPercent,omitempty"`
	Message        string   `json:"message,omitempty"`
}

func (a *AgentRuntime) SystemResourcesStatus() SystemResourceResponse {
	a.mu.RLock()
	dataDir := a.config.DataDir
	localRoots := activeSystemResourceLocalRoots(a.config.LocalMediaRoots, a.config.Datasources)
	uploadRoots := append([]config.UploadRootConfig(nil), a.config.UploadRoots...)
	a.mu.RUnlock()

	cpu := SystemCPUResource{LogicalCores: goruntime.NumCPU()}
	populatePlatformCPUResource(&cpu)
	memory := SystemMemoryResource{}
	populatePlatformMemoryResource(&memory)

	response := SystemResourceResponse{
		CheckedAt: time.Now().UTC(),
		CPU:       cpu,
		Memory:    memory,
		Disks:     make([]SystemDiskResource, 0, 1+len(localRoots)+len(uploadRoots)),
	}
	seenPaths := map[string]struct{}{}
	addDisk := func(label, path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		key := filepath.Clean(path)
		if _, exists := seenPaths[key]; exists {
			return
		}
		seenPaths[key] = struct{}{}
		response.Disks = append(response.Disks, systemDiskResource(label, path))
	}
	addDisk("Agent data", dataDir)
	for _, root := range localRoots {
		addDisk(resourceLabel("Local media", root.Key), root.Path)
	}
	for _, root := range uploadRoots {
		addDisk(resourceLabel("Upload root", root.Key), root.Path)
	}
	return response
}

func activeSystemResourceLocalRoots(roots []config.LocalMediaRootConfig, datasources []config.DatasourceConfig) []config.LocalMediaRootConfig {
	if len(roots) == 0 || len(datasources) == 0 {
		return nil
	}
	rootsByKey := make(map[string]config.LocalMediaRootConfig, len(roots))
	for _, root := range roots {
		key := strings.TrimSpace(root.Key)
		if key == "" {
			continue
		}
		rootsByKey[key] = root
	}
	active := make([]config.LocalMediaRootConfig, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, datasource := range datasources {
		if datasource.Kind != config.DatasourceKindLocalFiles {
			continue
		}
		rootKey := strings.TrimSpace(datasource.RootKey)
		if rootKey == "" {
			continue
		}
		if _, exists := seen[rootKey]; exists {
			continue
		}
		root, ok := rootsByKey[rootKey]
		if !ok {
			continue
		}
		seen[rootKey] = struct{}{}
		active = append(active, root)
	}
	return active
}

func resourceLabel(prefix, key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return prefix
	}
	return fmt.Sprintf("%s: %s", prefix, key)
}

func systemDiskResource(label, targetPath string) SystemDiskResource {
	status := SystemDiskResource{
		Label: label,
		Path:  targetPath,
	}
	if strings.TrimSpace(targetPath) == "" {
		status.Message = "Path is not configured."
		return status
	}
	total, available, err := filesystemUsage(existingFilesystemPath(targetPath))
	if err != nil {
		status.Message = "Filesystem usage could not be inspected."
		return status
	}
	status.TotalBytes = total
	status.AvailableBytes = available
	if total > 0 {
		status.UsedBytes = total - available
		status.UsedPercent = floatPointer((float64(status.UsedBytes) / float64(total)) * 100)
	}
	return status
}

func floatPointer(value float64) *float64 {
	return &value
}
