package store

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeviceRegistryStoreCreateRedeemAndRotate(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	registry, err := LoadOrCreateDeviceRegistry(dataDir, 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	pairing, err := registry.CreatePairingSession(now, now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}
	if pairing.Code == "" {
		t.Fatal("CreatePairingSession() returned an empty code")
	}
	if len(pairing.Code) != 32 {
		t.Fatalf("CreatePairingSession() code length = %d, want 32", len(pairing.Code))
	}
	if _, err := hex.DecodeString(pairing.Code); err != nil {
		t.Fatalf("CreatePairingSession() code is not hex: %v", err)
	}

	device, err := registry.RedeemPairingSession(
		pairing.Code,
		"Test iPhone",
		now.Add(time.Minute),
		now.Add(24*time.Hour),
	)
	if err != nil {
		t.Fatalf("RedeemPairingSession() error = %v", err)
	}
	if device.DeviceID == "" {
		t.Fatal("RedeemPairingSession() returned an empty device id")
	}
	if device.CurrentRefreshToken == "" {
		t.Fatal("RedeemPairingSession() returned an empty refresh token")
	}
	if !registry.HasDevice(device.DeviceID) {
		t.Fatal("HasDevice() = false, want true")
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, deviceRegistryFileName))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(raw), device.CurrentRefreshToken) {
		t.Fatal("device registry persisted the raw refresh token")
	}
	if !strings.Contains(string(raw), "\"currentRefreshTokenHash\"") {
		t.Fatal("device registry is missing the hashed refresh token field")
	}

	rotated, err := registry.RotateRefreshToken(
		device.CurrentRefreshToken,
		now.Add(2*time.Minute),
		now.Add(48*time.Hour),
	)
	if err != nil {
		t.Fatalf("RotateRefreshToken() error = %v", err)
	}
	if rotated.CurrentRefreshToken == device.CurrentRefreshToken {
		t.Fatal("RotateRefreshToken() did not rotate the refresh token")
	}
}

func TestDeviceRegistryStoreRevokeDevice(t *testing.T) {
	t.Parallel()

	registry, err := LoadOrCreateDeviceRegistry(t.TempDir(), 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	device, err := registry.CreateHostedDevice("Test iPhone", now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("CreateHostedDevice() error = %v", err)
	}
	if err := registry.RevokeDevice(device.DeviceID); err != nil {
		t.Fatalf("RevokeDevice() error = %v", err)
	}
	if registry.HasDevice(device.DeviceID) {
		t.Fatal("HasDevice() = true after revoke, want false")
	}
	if err := registry.RevokeDevice(device.DeviceID); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("RevokeDevice() error = %v, want %v", err, ErrDeviceNotFound)
	}
}

func TestDeviceRegistryStoreRenameDevice(t *testing.T) {
	t.Parallel()

	registry, err := LoadOrCreateDeviceRegistry(t.TempDir(), 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	device, err := registry.CreateHostedDevice("Old iPhone", now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("CreateHostedDevice() error = %v", err)
	}

	renamed, err := registry.RenameDevice(device.DeviceID, " New iPhone ")
	if err != nil {
		t.Fatalf("RenameDevice() error = %v", err)
	}
	if renamed.DeviceID != device.DeviceID || renamed.DeviceName != "New iPhone" {
		t.Fatalf("RenameDevice() = %+v, want same device id with new name", renamed)
	}
	snapshot := registry.Snapshot()
	if len(snapshot.Devices) != 1 || snapshot.Devices[0].DeviceName != "New iPhone" {
		t.Fatalf("Snapshot().Devices = %+v, want renamed device", snapshot.Devices)
	}
	if _, err := registry.RenameDevice(device.DeviceID, " "); !errors.Is(err, ErrDeviceNameInvalid) {
		t.Fatalf("RenameDevice(empty) error = %v, want %v", err, ErrDeviceNameInvalid)
	}
	if _, err := registry.RenameDevice("missing", "Other"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("RenameDevice(missing) error = %v, want %v", err, ErrDeviceNotFound)
	}
}

func TestDeviceRegistryStoreRevokeDeviceKeepsDeviceOnPersistError(t *testing.T) {
	t.Parallel()

	registry, err := LoadOrCreateDeviceRegistry(t.TempDir(), 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	device, err := registry.CreateHostedDevice("Test iPhone", now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("CreateHostedDevice() error = %v", err)
	}

	registry.path = filepath.Join(t.TempDir(), "missing", deviceRegistryFileName)
	if err := registry.RevokeDevice(device.DeviceID); err == nil {
		t.Fatal("RevokeDevice() error = nil, want persist error")
	}
	if !registry.HasDevice(device.DeviceID) {
		t.Fatal("HasDevice() = false after failed revoke, want true")
	}
}

func TestDeviceRegistryStoreLoadsLegacyPlaintextRefreshToken(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	registryPath := filepath.Join(dataDir, deviceRegistryFileName)
	now := time.Now().UTC().Truncate(time.Second)
	expiresAt := now.Add(24 * time.Hour)
	raw := []byte("{\n  \"pairingSessions\": [],\n  \"devices\": [\n    {\n      \"deviceId\": \"device-123\",\n      \"deviceName\": \"Legacy iPhone\",\n      \"createdAt\": \"2026-04-11T00:00:00Z\",\n      \"lastRefreshedAt\": \"2026-04-11T00:00:00Z\",\n      \"currentRefreshToken\": \"legacy-refresh-token\",\n      \"refreshTokenExpiresAt\": \"" + expiresAt.Format(time.RFC3339) + "\"\n    }\n  ]\n}\n")
	if err := os.WriteFile(registryPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	registry, err := LoadOrCreateDeviceRegistry(dataDir, 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	rotated, err := registry.RotateRefreshToken(
		"legacy-refresh-token",
		expiresAt,
		now.Add(48*time.Hour),
	)
	if err != nil {
		t.Fatalf("RotateRefreshToken() error = %v", err)
	}
	if rotated.CurrentRefreshToken == "" {
		t.Fatal("RotateRefreshToken() returned an empty replacement refresh token")
	}

	persisted, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(persisted), "legacy-refresh-token") {
		t.Fatal("legacy plaintext refresh token was not migrated away")
	}
}

func TestDeviceRegistryStoreRejectsExpiredPairingSession(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	registry, err := LoadOrCreateDeviceRegistry(dataDir, 1)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	pairing, err := registry.CreatePairingSession(now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}

	_, err = registry.RedeemPairingSession(
		pairing.Code,
		"Expired Device",
		now.Add(2*time.Minute),
		now.Add(24*time.Hour),
	)
	if !errors.Is(err, ErrPairingSessionNotFound) && !errors.Is(err, ErrPairingSessionExpired) {
		t.Fatalf("RedeemPairingSession() error = %v, want expired/not found", err)
	}
}

func TestDeviceRegistryStoreEnforcesDeviceLimit(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	registry, err := LoadOrCreateDeviceRegistry(dataDir, 1)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	first, err := registry.CreatePairingSession(now, now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}
	if _, err := registry.RedeemPairingSession(first.Code, "First Device", now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("RedeemPairingSession() error = %v", err)
	}

	_, err = registry.CreatePairingSession(now.Add(time.Minute), now.Add(11*time.Minute))
	if !errors.Is(err, ErrDeviceLimitReached) {
		t.Fatalf("CreatePairingSession() error = %v, want %v", err, ErrDeviceLimitReached)
	}
}

func TestDeviceRegistryStoreReplacesExistingPairingSession(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	registry, err := LoadOrCreateDeviceRegistry(dataDir, 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	first, err := registry.CreatePairingSession(now, now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}

	second, err := registry.CreatePairingSession(now.Add(time.Minute), now.Add(11*time.Minute))
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}
	if second.Code == first.Code {
		t.Fatal("CreatePairingSession() reused the previous pairing code")
	}

	snapshot := registry.Snapshot()
	if len(snapshot.PairingSessions) != 1 {
		t.Fatalf("Snapshot() pairing count = %d, want 1", len(snapshot.PairingSessions))
	}
	if snapshot.PairingSessions[0].Code != second.Code {
		t.Fatalf("Snapshot() pairing code = %q, want %q", snapshot.PairingSessions[0].Code, second.Code)
	}
}

func TestDeviceRegistryStoreInvalidatesPairingAfterFailedAttempts(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	registry, err := LoadOrCreateDeviceRegistry(dataDir, 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	pairing, err := registry.CreatePairingSession(now, now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}

	for attempt := 1; attempt <= maxPairingFailedAttempts; attempt++ {
		_, err := registry.RedeemPairingSession(
			"wrong-code",
			"Attacker",
			now.Add(time.Duration(attempt)*time.Minute),
			now.Add(24*time.Hour),
		)
		if !errors.Is(err, ErrPairingSessionNotFound) {
			t.Fatalf("RedeemPairingSession() error = %v, want %v", err, ErrPairingSessionNotFound)
		}
	}

	snapshot := registry.Snapshot()
	if len(snapshot.PairingSessions) != 0 {
		t.Fatalf("Snapshot() pairing count = %d, want 0 after failed attempts", len(snapshot.PairingSessions))
	}

	_, err = registry.RedeemPairingSession(
		pairing.Code,
		"Test iPhone",
		now.Add(6*time.Minute),
		now.Add(24*time.Hour),
	)
	if !errors.Is(err, ErrPairingSessionNotFound) {
		t.Fatalf("RedeemPairingSession() error = %v, want %v", err, ErrPairingSessionNotFound)
	}
}
