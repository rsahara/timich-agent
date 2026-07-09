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

func TestDeviceRegistryStoreNearbyLinkApproveAndRedeem(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	registry, err := LoadOrCreateDeviceRegistry(dataDir, 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	link, pollToken, err := registry.CreateNearbyLink("Living Room TV", "android_tv", now, now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("CreateNearbyLink() error = %v", err)
	}
	if len(link.LinkCode) != 6 {
		t.Fatalf("LinkCode length = %d, want 6", len(link.LinkCode))
	}
	for _, char := range link.LinkCode {
		if char < '0' || char > '9' {
			t.Fatalf("LinkCode = %q, want decimal digits only", link.LinkCode)
		}
	}
	if pollToken == "" {
		t.Fatal("CreateNearbyLink() returned an empty poll token")
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, deviceRegistryFileName))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if strings.Contains(string(raw), pollToken) {
		t.Fatal("device registry persisted the raw Nearby Link poll token")
	}
	if !strings.Contains(string(raw), "\"pollTokenHash\"") {
		t.Fatal("device registry is missing the Nearby Link poll token hash")
	}

	pending, err := registry.PollNearbyLink(link.LinkID, pollToken, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("PollNearbyLink() pending error = %v", err)
	}
	if pending.Status() != NearbyLinkStatusPending {
		t.Fatalf("NearbyLink status = %q, want %q", pending.Status(), NearbyLinkStatusPending)
	}

	approved, err := registry.ApproveNearbyLink(link.LinkCode[:3]+" "+link.LinkCode[3:], now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("ApproveNearbyLink() error = %v", err)
	}
	if approved.Status() != NearbyLinkStatusApproved {
		t.Fatalf("approved status = %q, want %q", approved.Status(), NearbyLinkStatusApproved)
	}

	device, redeemed, err := registry.RedeemNearbyLink(
		link.LinkID,
		pollToken,
		now.Add(3*time.Minute),
		now.Add(24*time.Hour),
	)
	if err != nil {
		t.Fatalf("RedeemNearbyLink() error = %v", err)
	}
	if device.DeviceName != "Living Room TV" {
		t.Fatalf("device name = %q, want Living Room TV", device.DeviceName)
	}
	if redeemed.ConsumedAt == nil {
		t.Fatal("RedeemNearbyLink() did not mark the link consumed")
	}
	snapshot := registry.Snapshot()
	if len(snapshot.NearbyLinks) != 0 {
		t.Fatalf("Snapshot().NearbyLinks length = %d, want 0 after redeem", len(snapshot.NearbyLinks))
	}
	if len(snapshot.Devices) != 1 || snapshot.Devices[0].DeviceID != device.DeviceID {
		t.Fatalf("Snapshot().Devices = %+v, want redeemed device", snapshot.Devices)
	}
}

func TestDeviceRegistryStoreRedeemNearbyLinkKeepsLinkOnPersistError(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	registry, err := LoadOrCreateDeviceRegistry(dataDir, 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	link, pollToken, err := registry.CreateNearbyLink("Living Room TV", "android_tv", now, now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("CreateNearbyLink() error = %v", err)
	}
	if _, err := registry.ApproveNearbyLink(link.LinkCode, now.Add(time.Minute)); err != nil {
		t.Fatalf("ApproveNearbyLink() error = %v", err)
	}

	originalPath := registry.path
	registry.path = filepath.Join(t.TempDir(), "missing", deviceRegistryFileName)
	if _, _, err := registry.RedeemNearbyLink(
		link.LinkID,
		pollToken,
		now.Add(2*time.Minute),
		now.Add(24*time.Hour),
	); err == nil {
		t.Fatal("RedeemNearbyLink() error = nil, want persist error")
	}

	snapshot := registry.Snapshot()
	if len(snapshot.Devices) != 0 {
		t.Fatalf("Snapshot().Devices length = %d after failed redeem, want 0", len(snapshot.Devices))
	}
	if len(snapshot.NearbyLinks) != 1 {
		t.Fatalf("Snapshot().NearbyLinks length = %d after failed redeem, want 1", len(snapshot.NearbyLinks))
	}
	if snapshot.NearbyLinks[0].ConsumedAt != nil {
		t.Fatal("Nearby Link was consumed after failed redeem")
	}
	if snapshot.NearbyLinks[0].Status() != NearbyLinkStatusApproved {
		t.Fatalf("Nearby Link status = %q after failed redeem, want %q", snapshot.NearbyLinks[0].Status(), NearbyLinkStatusApproved)
	}

	registry.path = originalPath
	device, redeemed, err := registry.RedeemNearbyLink(
		link.LinkID,
		pollToken,
		now.Add(3*time.Minute),
		now.Add(24*time.Hour),
	)
	if err != nil {
		t.Fatalf("RedeemNearbyLink() retry error = %v", err)
	}
	if device.DeviceID == "" {
		t.Fatal("RedeemNearbyLink() retry returned an empty device id")
	}
	if redeemed.ConsumedAt == nil {
		t.Fatal("RedeemNearbyLink() retry did not mark the link consumed")
	}
}

func TestDeviceRegistryStoreCreateNearbyLinkKeepsMemoryOnPersistError(t *testing.T) {
	t.Parallel()

	registry, err := LoadOrCreateDeviceRegistry(t.TempDir(), 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	registry.path = filepath.Join(t.TempDir(), "missing", deviceRegistryFileName)
	if link, pollToken, err := registry.CreateNearbyLink("Living Room TV", "android_tv", now, now.Add(5*time.Minute)); err == nil {
		t.Fatalf("CreateNearbyLink() error = nil, want persist error; link=%+v pollToken=%q", link, pollToken)
	} else if pollToken != "" {
		t.Fatalf("CreateNearbyLink() pollToken = %q after persist error, want empty", pollToken)
	}

	snapshot := registry.Snapshot()
	if len(snapshot.NearbyLinks) != 0 {
		t.Fatalf("Snapshot().NearbyLinks length = %d after failed create, want 0", len(snapshot.NearbyLinks))
	}
}

func TestDeviceRegistryStoreApproveNearbyLinkKeepsPendingOnPersistError(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	registry, err := LoadOrCreateDeviceRegistry(dataDir, 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	link, _, err := registry.CreateNearbyLink("Living Room TV", "android_tv", now, now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("CreateNearbyLink() error = %v", err)
	}

	originalPath := registry.path
	registry.path = filepath.Join(t.TempDir(), "missing", deviceRegistryFileName)
	if _, err := registry.ApproveNearbyLink(link.LinkCode, now.Add(time.Minute)); err == nil {
		t.Fatal("ApproveNearbyLink() error = nil, want persist error")
	}

	snapshot := registry.Snapshot()
	if len(snapshot.NearbyLinks) != 1 {
		t.Fatalf("Snapshot().NearbyLinks length = %d after failed approve, want 1", len(snapshot.NearbyLinks))
	}
	if snapshot.NearbyLinks[0].Status() != NearbyLinkStatusPending {
		t.Fatalf("Nearby Link status = %q after failed approve, want %q", snapshot.NearbyLinks[0].Status(), NearbyLinkStatusPending)
	}

	registry.path = originalPath
	approved, err := registry.ApproveNearbyLink(link.LinkCode, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("ApproveNearbyLink() retry error = %v", err)
	}
	if approved.Status() != NearbyLinkStatusApproved {
		t.Fatalf("ApproveNearbyLink() retry status = %q, want %q", approved.Status(), NearbyLinkStatusApproved)
	}
}

func TestDeviceRegistryStoreDenyNearbyLinkKeepsPendingOnPersistError(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	registry, err := LoadOrCreateDeviceRegistry(dataDir, 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	link, _, err := registry.CreateNearbyLink("Living Room TV", "android_tv", now, now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("CreateNearbyLink() error = %v", err)
	}

	originalPath := registry.path
	registry.path = filepath.Join(t.TempDir(), "missing", deviceRegistryFileName)
	if _, err := registry.DenyNearbyLink(link.LinkID, now.Add(time.Minute)); err == nil {
		t.Fatal("DenyNearbyLink() error = nil, want persist error")
	}

	snapshot := registry.Snapshot()
	if len(snapshot.NearbyLinks) != 1 {
		t.Fatalf("Snapshot().NearbyLinks length = %d after failed deny, want 1", len(snapshot.NearbyLinks))
	}
	if snapshot.NearbyLinks[0].Status() != NearbyLinkStatusPending {
		t.Fatalf("Nearby Link status = %q after failed deny, want %q", snapshot.NearbyLinks[0].Status(), NearbyLinkStatusPending)
	}

	registry.path = originalPath
	denied, err := registry.DenyNearbyLink(link.LinkID, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("DenyNearbyLink() retry error = %v", err)
	}
	if denied.Status() != NearbyLinkStatusDenied {
		t.Fatalf("DenyNearbyLink() retry status = %q, want %q", denied.Status(), NearbyLinkStatusDenied)
	}
}

func TestDeviceRegistryStoreCancelNearbyLinkKeepsPendingOnPersistError(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	registry, err := LoadOrCreateDeviceRegistry(dataDir, 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	link, pollToken, err := registry.CreateNearbyLink("Living Room TV", "android_tv", now, now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("CreateNearbyLink() error = %v", err)
	}

	originalPath := registry.path
	registry.path = filepath.Join(t.TempDir(), "missing", deviceRegistryFileName)
	if _, err := registry.CancelNearbyLink(link.LinkID, pollToken, now.Add(time.Minute)); err == nil {
		t.Fatal("CancelNearbyLink() error = nil, want persist error")
	}

	snapshot := registry.Snapshot()
	if len(snapshot.NearbyLinks) != 1 {
		t.Fatalf("Snapshot().NearbyLinks length = %d after failed cancel, want 1", len(snapshot.NearbyLinks))
	}
	if snapshot.NearbyLinks[0].Status() != NearbyLinkStatusPending {
		t.Fatalf("Nearby Link status = %q after failed cancel, want %q", snapshot.NearbyLinks[0].Status(), NearbyLinkStatusPending)
	}

	registry.path = originalPath
	canceled, err := registry.CancelNearbyLink(link.LinkID, pollToken, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("CancelNearbyLink() retry error = %v", err)
	}
	if canceled.Status() != NearbyLinkStatusDenied {
		t.Fatalf("CancelNearbyLink() retry status = %q, want %q", canceled.Status(), NearbyLinkStatusDenied)
	}
}

func TestDeviceRegistryStoreNearbyLinkFailedAttemptKeepsStateOnPersistError(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	registry, err := LoadOrCreateDeviceRegistry(dataDir, 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	link, _, err := registry.CreateNearbyLink("Living Room TV", "android_tv", now, now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("CreateNearbyLink() error = %v", err)
	}

	originalPath := registry.path
	registry.path = filepath.Join(t.TempDir(), "missing", deviceRegistryFileName)
	if _, err := registry.PollNearbyLink(link.LinkID, "wrong-token", now.Add(time.Minute)); err == nil {
		t.Fatal("PollNearbyLink(wrong token) error = nil, want persist error")
	}

	snapshot := registry.Snapshot()
	if len(snapshot.NearbyLinks) != 1 {
		t.Fatalf("Snapshot().NearbyLinks length = %d after failed poll attempt, want 1", len(snapshot.NearbyLinks))
	}
	if snapshot.NearbyLinks[0].FailedAttempts != 0 {
		t.Fatalf("Nearby Link failed attempts = %d after failed persist, want 0", snapshot.NearbyLinks[0].FailedAttempts)
	}
	if snapshot.NearbyLinks[0].Status() != NearbyLinkStatusPending {
		t.Fatalf("Nearby Link status = %q after failed poll attempt, want %q", snapshot.NearbyLinks[0].Status(), NearbyLinkStatusPending)
	}

	registry.path = originalPath
	if _, err := registry.PollNearbyLink(link.LinkID, "wrong-token", now.Add(2*time.Minute)); !errors.Is(err, ErrNearbyLinkPollTokenInvalid) {
		t.Fatalf("PollNearbyLink(wrong token) retry error = %v, want %v", err, ErrNearbyLinkPollTokenInvalid)
	}
	snapshot = registry.Snapshot()
	if snapshot.NearbyLinks[0].FailedAttempts != 1 {
		t.Fatalf("Nearby Link failed attempts = %d after retry, want 1", snapshot.NearbyLinks[0].FailedAttempts)
	}
}

func TestDeviceRegistryStoreNearbyLinkCancelRequiresPollToken(t *testing.T) {
	t.Parallel()

	registry, err := LoadOrCreateDeviceRegistry(t.TempDir(), 2)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	link, pollToken, err := registry.CreateNearbyLink("Living Room TV", "android_tv", now, now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("CreateNearbyLink() error = %v", err)
	}
	if _, err := registry.CancelNearbyLink(link.LinkID, "wrong-token", now.Add(time.Minute)); !errors.Is(err, ErrNearbyLinkPollTokenInvalid) {
		t.Fatalf("CancelNearbyLink(wrong token) error = %v, want %v", err, ErrNearbyLinkPollTokenInvalid)
	}

	canceled, err := registry.CancelNearbyLink(link.LinkID, pollToken, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("CancelNearbyLink() error = %v", err)
	}
	if canceled.Status() != NearbyLinkStatusDenied {
		t.Fatalf("canceled status = %q, want %q", canceled.Status(), NearbyLinkStatusDenied)
	}
	if _, err := registry.ApproveNearbyLink(link.LinkCode, now.Add(3*time.Minute)); !errors.Is(err, ErrNearbyLinkDenied) {
		t.Fatalf("ApproveNearbyLink(canceled) error = %v, want %v", err, ErrNearbyLinkDenied)
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
