package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rsahara/timich-agent/internal/atomicfile"
)

const (
	deviceProfilesDirName       = "devices"
	defaultUploadPathPattern    = "{deviceName}/{yyyy}-{MM}-{dd}/{filename}"
	defaultDeviceProfileVersion = 1
)

// DefaultUploadPathPattern returns the default per-device upload destination pattern.
func DefaultUploadPathPattern() string {
	return defaultUploadPathPattern
}

// DeviceProfile stores low-churn per-device policy and status state that should
// not live in the auth-oriented device registry.
type DeviceProfile struct {
	Version    int                 `json:"version"`
	DeviceID   string              `json:"deviceId"`
	DeviceName string              `json:"deviceName"`
	CreatedAt  time.Time           `json:"createdAt"`
	Upload     DeviceUploadProfile `json:"upload"`
}

// DeviceUploadProfile stores administrator-owned upload policy for one paired
// device. High-cardinality upload ledger/session state belongs in SQLite later.
type DeviceUploadProfile struct {
	Enabled       bool       `json:"enabled"`
	RootKey       string     `json:"rootKey,omitempty"`
	PathPattern   string     `json:"pathPattern"`
	CapturedAfter *time.Time `json:"capturedAfter,omitempty"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// DeviceProfileStore provides concurrency-safe access to per-device profile
// files under dataDir/devices.
type DeviceProfileStore struct {
	dir string
	mu  sync.Mutex
}

// LoadOrCreateDeviceProfileStore ensures the per-device profile directory
// exists and returns a profile store rooted at dataDir/devices.
func LoadOrCreateDeviceProfileStore(dataDir string) (*DeviceProfileStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("data directory must not be empty")
	}
	dir := filepath.Join(dataDir, deviceProfilesDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create device profile directory: %w", err)
	}
	return &DeviceProfileStore{dir: dir}, nil
}

// EnsureProfile creates the default disabled upload profile for a device, or
// refreshes stable device metadata while preserving existing upload policy.
func (s *DeviceProfileStore) EnsureProfile(device DeviceRecord, now time.Time) (DeviceProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalizedDeviceID := strings.TrimSpace(device.DeviceID)
	if normalizedDeviceID == "" {
		return DeviceProfile{}, ErrDeviceNotFound
	}
	path := s.profilePathLocked(normalizedDeviceID)
	if profile, err := readDeviceProfileFile(path); err == nil {
		updated := profile
		updated.Version = defaultDeviceProfileVersion
		updated.DeviceID = normalizedDeviceID
		updated.DeviceName = device.DeviceName
		if updated.CreatedAt.IsZero() {
			updated.CreatedAt = device.CreatedAt.UTC()
		}
		if updated.Upload.PathPattern == "" {
			updated.Upload.PathPattern = defaultUploadPathPattern
		}
		if updated.Upload.UpdatedAt.IsZero() {
			updated.Upload.UpdatedAt = now.UTC()
		}
		if updated == profile {
			return profile, nil
		}
		if err := writeDeviceProfileFile(path, updated); err != nil {
			return DeviceProfile{}, err
		}
		return updated, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return DeviceProfile{}, err
	}

	profile := DeviceProfile{
		Version:    defaultDeviceProfileVersion,
		DeviceID:   normalizedDeviceID,
		DeviceName: device.DeviceName,
		CreatedAt:  device.CreatedAt.UTC(),
		Upload: DeviceUploadProfile{
			Enabled:     false,
			PathPattern: defaultUploadPathPattern,
			UpdatedAt:   now.UTC(),
		},
	}
	if err := writeDeviceProfileFile(path, profile); err != nil {
		return DeviceProfile{}, err
	}
	return profile, nil
}

// DeleteProfile removes a per-device profile. Missing profile files are treated
// as already deleted so revocation can clean up legacy devices.
func (s *DeviceProfileStore) DeleteProfile(deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalizedDeviceID := strings.TrimSpace(deviceID)
	if normalizedDeviceID == "" {
		return ErrDeviceNotFound
	}
	path := s.profilePathLocked(normalizedDeviceID)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete device profile %s: %w", normalizedDeviceID, err)
	}
	return nil
}

// LoadProfile reads a per-device profile by device ID.
func (s *DeviceProfileStore) LoadProfile(deviceID string) (DeviceProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalizedDeviceID := strings.TrimSpace(deviceID)
	if normalizedDeviceID == "" {
		return DeviceProfile{}, ErrDeviceNotFound
	}
	return readDeviceProfileFile(s.profilePathLocked(normalizedDeviceID))
}

// UpdateUploadProfile replaces administrator-owned upload policy for a paired
// device profile.
func (s *DeviceProfileStore) UpdateUploadProfile(deviceID string, upload DeviceUploadProfile, now time.Time) (DeviceProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalizedDeviceID := strings.TrimSpace(deviceID)
	if normalizedDeviceID == "" {
		return DeviceProfile{}, ErrDeviceNotFound
	}
	path := s.profilePathLocked(normalizedDeviceID)
	profile, err := readDeviceProfileFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DeviceProfile{}, ErrDeviceNotFound
		}
		return DeviceProfile{}, err
	}
	upload.RootKey = strings.TrimSpace(upload.RootKey)
	upload.PathPattern = strings.TrimSpace(upload.PathPattern)
	if upload.PathPattern == "" {
		upload.PathPattern = defaultUploadPathPattern
	}
	if upload.CapturedAfter != nil {
		capturedAfter := upload.CapturedAfter.UTC()
		upload.CapturedAfter = &capturedAfter
	}
	upload.UpdatedAt = now.UTC()
	profile.Version = defaultDeviceProfileVersion
	profile.Upload = upload
	if err := writeDeviceProfileFile(path, profile); err != nil {
		return DeviceProfile{}, err
	}
	return profile, nil
}

func (s *DeviceProfileStore) profilePathLocked(deviceID string) string {
	return filepath.Join(s.dir, deviceID+".json")
}

func readDeviceProfileFile(path string) (DeviceProfile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return DeviceProfile{}, err
	}
	var profile DeviceProfile
	if err := json.Unmarshal(raw, &profile); err != nil {
		return DeviceProfile{}, fmt.Errorf("parse device profile %s: %w", path, err)
	}
	return profile, nil
}

func writeDeviceProfileFile(path string, profile DeviceProfile) error {
	raw, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal device profile: %w", err)
	}
	raw = append(raw, '\n')
	if err := atomicfile.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write device profile %s: %w", path, err)
	}
	return nil
}
