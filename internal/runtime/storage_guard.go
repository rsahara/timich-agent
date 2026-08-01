package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultStorageMinFreeBytes int64 = 1 << 30

var (
	ErrStorageWriteBlocked = errors.New("storage write blocked")
	storageAvailableBytes  = filesystemAvailableBytes
)

// StorageGuardrailResponse reports whether the agent should pause
// write-producing work on a filesystem.
type StorageGuardrailResponse struct {
	State          string    `json:"state"`
	Path           string    `json:"path"`
	MinFreeBytes   int64     `json:"minFreeBytes"`
	AvailableBytes int64     `json:"availableBytes,omitempty"`
	WriteBlocked   bool      `json:"writeBlocked"`
	Message        string    `json:"message,omitempty"`
	CheckedAt      time.Time `json:"checkedAt"`
}

func (a *AgentRuntime) StorageGuardrailStatus() StorageGuardrailResponse {
	a.mu.RLock()
	dataDir := a.config.DataDir
	a.mu.RUnlock()

	return storageGuardrailStatus(dataDir, defaultStorageMinFreeBytes)
}

func (a *AgentRuntime) storageGuardrailStatusLocked() StorageGuardrailResponse {
	return storageGuardrailStatus(a.config.DataDir, defaultStorageMinFreeBytes)
}

func (a *AgentRuntime) ensureStateWritesAvailable() error {
	a.mu.RLock()
	dataDir := a.config.DataDir
	a.mu.RUnlock()
	return ensureWritesAvailableForPath(dataDir)
}

func ensurePathWritesAvailable(targetPath string) error {
	return ensureWritesAvailableForPath(targetPath)
}

func ensureWritesAvailableForPath(targetPath string) error {
	status := storageGuardrailStatus(targetPath, defaultStorageMinFreeBytes)
	if status.WriteBlocked {
		return storageWriteBlockedError(status)
	}
	return nil
}

func storageGuardrailStatus(targetPath string, minFreeBytes int64) StorageGuardrailResponse {
	if minFreeBytes <= 0 {
		minFreeBytes = defaultStorageMinFreeBytes
	}
	targetPath = strings.TrimSpace(targetPath)
	status := StorageGuardrailResponse{
		State:        "unknown",
		Path:         targetPath,
		MinFreeBytes: minFreeBytes,
		CheckedAt:    time.Now().UTC(),
	}
	if targetPath == "" {
		status.WriteBlocked = true
		status.Message = "Storage path is not configured."
		return status
	}
	checkPath := existingFilesystemPath(targetPath)
	available, err := storageAvailableBytes(checkPath)
	if err != nil {
		status.WriteBlocked = true
		status.Message = "Storage free space could not be inspected."
		return status
	}
	status.AvailableBytes = available
	if available < minFreeBytes {
		status.State = "blocked"
		status.WriteBlocked = true
		status.Message = fmt.Sprintf("Storage free space is below the %d byte guardrail.", minFreeBytes)
		return status
	}
	status.State = "ready"
	status.Message = "Storage has enough free space for write-producing work."
	return status
}

func storageWriteBlockedError(status StorageGuardrailResponse) error {
	message := strings.TrimSpace(status.Message)
	if message == "" {
		message = "Storage free space is below the configured guardrail."
	}
	return fmt.Errorf("%w: %s", ErrStorageWriteBlocked, message)
}

func existingFilesystemPath(targetPath string) string {
	path := filepath.Clean(targetPath)
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}
