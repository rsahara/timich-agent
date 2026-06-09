package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeviceProfileStoreEnsureProfileCreatesDisabledDefault(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	store, err := LoadOrCreateDeviceProfileStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceProfileStore() error = %v", err)
	}

	profile, err := store.EnsureProfile(DeviceRecord{
		DeviceID:   "device123",
		DeviceName: "Test iPhone",
		CreatedAt:  now.Add(-time.Hour),
	}, now)
	if err != nil {
		t.Fatalf("EnsureProfile() error = %v", err)
	}

	if profile.DeviceID != "device123" {
		t.Fatalf("DeviceID = %q, want device123", profile.DeviceID)
	}
	if profile.Upload.Enabled {
		t.Fatal("Upload.Enabled = true, want disabled default")
	}
	if profile.Upload.PathPattern != defaultUploadPathPattern {
		t.Fatalf("PathPattern = %q, want default %q", profile.Upload.PathPattern, defaultUploadPathPattern)
	}
	if profile.Upload.RootKey != "" {
		t.Fatalf("RootKey = %q, want empty default", profile.Upload.RootKey)
	}
	if profile.Upload.CapturedAfter != nil {
		t.Fatalf("CapturedAfter = %v, want nil default", profile.Upload.CapturedAfter)
	}
}

func TestDeviceProfileStoreEnsureProfilePreservesUploadPolicy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	store, err := LoadOrCreateDeviceProfileStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceProfileStore() error = %v", err)
	}
	profile, err := store.EnsureProfile(DeviceRecord{
		DeviceID:   "device123",
		DeviceName: "Old Name",
		CreatedAt:  now,
	}, now)
	if err != nil {
		t.Fatalf("EnsureProfile() error = %v", err)
	}
	profile.Upload.Enabled = true
	profile.Upload.RootKey = "nas-photos"
	profile.Upload.PathPattern = "{deviceName}/{filename}"
	if err := writeDeviceProfileFile(filepath.Join(store.dir, "device123.json"), profile); err != nil {
		t.Fatalf("writeDeviceProfileFile() error = %v", err)
	}

	updated, err := store.EnsureProfile(DeviceRecord{
		DeviceID:   "device123",
		DeviceName: "New Name",
		CreatedAt:  now,
	}, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("EnsureProfile() error = %v", err)
	}

	if updated.DeviceName != "New Name" {
		t.Fatalf("DeviceName = %q, want refreshed device metadata", updated.DeviceName)
	}
	if !updated.Upload.Enabled || updated.Upload.RootKey != "nas-photos" || updated.Upload.PathPattern != "{deviceName}/{filename}" {
		t.Fatalf("Upload policy was not preserved: %+v", updated.Upload)
	}
}

func TestDeviceProfileStoreUpdateUploadProfile(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	capturedAfter := now.Add(-24 * time.Hour)
	store, err := LoadOrCreateDeviceProfileStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceProfileStore() error = %v", err)
	}
	if _, err := store.EnsureProfile(DeviceRecord{
		DeviceID:   "device123",
		DeviceName: "Test iPhone",
		CreatedAt:  now,
	}, now); err != nil {
		t.Fatalf("EnsureProfile() error = %v", err)
	}

	updated, err := store.UpdateUploadProfile(" device123 ", DeviceUploadProfile{
		Enabled:       true,
		RootKey:       " nas-photos ",
		PathPattern:   " {deviceName}/{filename} ",
		CapturedAfter: &capturedAfter,
	}, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("UpdateUploadProfile() error = %v", err)
	}
	if !updated.Upload.Enabled || updated.Upload.RootKey != "nas-photos" || updated.Upload.PathPattern != "{deviceName}/{filename}" {
		t.Fatalf("Upload = %+v, want normalized enabled policy", updated.Upload)
	}
	if updated.Upload.CapturedAfter == nil || !updated.Upload.CapturedAfter.Equal(capturedAfter) {
		t.Fatalf("CapturedAfter = %v, want %v", updated.Upload.CapturedAfter, capturedAfter)
	}
	if !updated.Upload.UpdatedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("UpdatedAt = %v, want %v", updated.Upload.UpdatedAt, now.Add(time.Hour))
	}
}

func TestDeviceProfileStoreDeleteProfileTreatsMissingAsDeleted(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	store, err := LoadOrCreateDeviceProfileStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceProfileStore() error = %v", err)
	}
	if _, err := store.EnsureProfile(DeviceRecord{
		DeviceID:   "device123",
		DeviceName: "Test iPhone",
		CreatedAt:  time.Now().UTC(),
	}, time.Now().UTC()); err != nil {
		t.Fatalf("EnsureProfile() error = %v", err)
	}

	profilePath := filepath.Join(dataDir, deviceProfilesDirName, "device123.json")
	if _, err := os.Stat(profilePath); err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if err := store.DeleteProfile("device123"); err != nil {
		t.Fatalf("DeleteProfile() error = %v", err)
	}
	if err := store.DeleteProfile("device123"); err != nil {
		t.Fatalf("DeleteProfile() missing error = %v", err)
	}
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Fatalf("Stat() error = %v, want not exist", err)
	}
}
