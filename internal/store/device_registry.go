package store

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
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

const deviceRegistryFileName = "device-registry.json"
const maxPairingFailedAttempts = 5

var (
	ErrPairingSessionNotFound = errors.New("pairing session not found")
	ErrPairingSessionExpired  = errors.New("pairing session expired")
	ErrPairingSessionUsed     = errors.New("pairing session already redeemed")
	ErrDeviceLimitReached     = errors.New("device limit reached")
	ErrDeviceNotFound         = errors.New("device not found")
	ErrDeviceNameInvalid      = errors.New("device name invalid")
	ErrRefreshTokenNotFound   = errors.New("refresh token not found")
	ErrRefreshTokenExpired    = errors.New("refresh token expired")
)

// PairingSession stores a short-lived local pairing code.
type PairingSession struct {
	Code           string     `json:"code"`
	CreatedAt      time.Time  `json:"createdAt"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	FailedAttempts int        `json:"failedAttempts,omitempty"`
	RedeemedAt     *time.Time `json:"redeemedAt,omitempty"`
}

// DeviceRecord stores one paired app device and its refresh-token family.
type DeviceRecord struct {
	DeviceID                string    `json:"deviceId"`
	DeviceName              string    `json:"deviceName"`
	CreatedAt               time.Time `json:"createdAt"`
	LastRefreshedAt         time.Time `json:"lastRefreshedAt"`
	CurrentRefreshToken     string    `json:"-"`
	CurrentRefreshTokenHash string    `json:"currentRefreshTokenHash,omitempty"`
	RefreshTokenSalt        string    `json:"refreshTokenSalt,omitempty"`
	RefreshTokenExpiresAt   time.Time `json:"refreshTokenExpiresAt"`
}

// DeviceRegistry holds the persisted local pairing/device state.
type DeviceRegistry struct {
	PairingSessions []PairingSession `json:"pairingSessions"`
	Devices         []DeviceRecord   `json:"devices"`
}

// DeviceRegistryStore provides concurrency-safe access to the device registry.
type DeviceRegistryStore struct {
	path        string
	deviceLimit int

	mu       sync.Mutex
	registry DeviceRegistry
}

type deviceRecordJSON struct {
	DeviceID                string    `json:"deviceId"`
	DeviceName              string    `json:"deviceName"`
	CreatedAt               time.Time `json:"createdAt"`
	LastRefreshedAt         time.Time `json:"lastRefreshedAt"`
	CurrentRefreshToken     string    `json:"currentRefreshToken,omitempty"`
	CurrentRefreshTokenHash string    `json:"currentRefreshTokenHash,omitempty"`
	RefreshTokenSalt        string    `json:"refreshTokenSalt,omitempty"`
	RefreshTokenExpiresAt   time.Time `json:"refreshTokenExpiresAt"`
}

// LoadOrCreateDeviceRegistry loads or creates the local paired-device registry.
func LoadOrCreateDeviceRegistry(dataDir string, deviceLimit int) (*DeviceRegistryStore, error) {
	if dataDir == "" {
		return nil, errors.New("data directory must not be empty")
	}
	if deviceLimit < 1 {
		return nil, errors.New("device limit must be at least 1")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	path := filepath.Join(dataDir, deviceRegistryFileName)
	registry := DeviceRegistry{
		PairingSessions: []PairingSession{},
		Devices:         []DeviceRecord{},
	}

	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &registry); err != nil {
			return nil, fmt.Errorf("parse device registry %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read device registry %s: %w", path, err)
	} else if err := writeDeviceRegistryFile(path, registry); err != nil {
		return nil, err
	}

	store := &DeviceRegistryStore{
		path:        path,
		deviceLimit: deviceLimit,
		registry:    registry,
	}
	store.cleanupExpiredLocked(time.Now().UTC())
	if err := store.persistLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

// Snapshot returns a copy of the current registry.
func (s *DeviceRegistryStore) Snapshot() DeviceRegistry {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(time.Now().UTC())
	return cloneRegistry(s.registry)
}

// CreatePairingSession creates and persists a one-time pairing session.
func (s *DeviceRegistryStore) CreatePairingSession(now time.Time, expiresAt time.Time) (PairingSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(now)
	if len(s.registry.Devices) >= s.deviceLimit {
		return PairingSession{}, ErrDeviceLimitReached
	}

	code, err := randomPairingCode()
	if err != nil {
		return PairingSession{}, err
	}
	session := PairingSession{
		Code:      code,
		CreatedAt: now.UTC(),
		ExpiresAt: expiresAt.UTC(),
	}
	s.registry.PairingSessions = []PairingSession{session}
	if err := s.persistLocked(); err != nil {
		return PairingSession{}, err
	}
	return session, nil
}

// RedeemPairingSession consumes a one-time pairing session and creates a device record.
func (s *DeviceRegistryStore) RedeemPairingSession(
	code string,
	deviceName string,
	now time.Time,
	refreshTokenExpiresAt time.Time,
) (DeviceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(now)
	if len(s.registry.Devices) >= s.deviceLimit {
		return DeviceRecord{}, ErrDeviceLimitReached
	}

	normalizedCode := strings.TrimSpace(code)
	if normalizedCode == "" {
		if err := s.recordFailedPairingAttemptLocked(now); err != nil {
			return DeviceRecord{}, err
		}
		return DeviceRecord{}, ErrPairingSessionNotFound
	}

	sessionIndex := -1
	for index, session := range s.registry.PairingSessions {
		if session.Code != normalizedCode {
			continue
		}
		sessionIndex = index
		if session.RedeemedAt != nil {
			return DeviceRecord{}, ErrPairingSessionUsed
		}
		if now.After(session.ExpiresAt) {
			return DeviceRecord{}, ErrPairingSessionExpired
		}
		break
	}
	if sessionIndex == -1 {
		if err := s.recordFailedPairingAttemptLocked(now); err != nil {
			return DeviceRecord{}, err
		}
		return DeviceRecord{}, ErrPairingSessionNotFound
	}

	device, err := newDeviceRecord(deviceName, now, refreshTokenExpiresAt)
	if err != nil {
		return DeviceRecord{}, err
	}
	s.registry.Devices = append(s.registry.Devices, device)
	redeemedAt := now.UTC()
	s.registry.PairingSessions[sessionIndex].RedeemedAt = &redeemedAt
	if err := s.persistLocked(); err != nil {
		return DeviceRecord{}, err
	}
	return device, nil
}

// RotateRefreshToken rotates the refresh-token family for an existing device.
func (s *DeviceRegistryStore) RotateRefreshToken(
	refreshToken string,
	now time.Time,
	refreshTokenExpiresAt time.Time,
) (DeviceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(now)
	normalizedToken := strings.TrimSpace(refreshToken)
	if normalizedToken == "" {
		return DeviceRecord{}, ErrRefreshTokenNotFound
	}

	for index, device := range s.registry.Devices {
		if !device.matchesRefreshToken(normalizedToken) {
			continue
		}
		if now.After(device.RefreshTokenExpiresAt) {
			return DeviceRecord{}, ErrRefreshTokenExpired
		}

		rotatedToken, err := randomBase64(32)
		if err != nil {
			return DeviceRecord{}, fmt.Errorf("generate refresh token: %w", err)
		}
		if err := device.setRefreshToken(rotatedToken); err != nil {
			return DeviceRecord{}, fmt.Errorf("persist refresh token: %w", err)
		}
		device.LastRefreshedAt = now.UTC()
		device.RefreshTokenExpiresAt = refreshTokenExpiresAt.UTC()
		s.registry.Devices[index] = device
		if err := s.persistLocked(); err != nil {
			return DeviceRecord{}, err
		}
		return device, nil
	}

	return DeviceRecord{}, ErrRefreshTokenNotFound
}

// CreateHostedDevice creates a paired device directly for Timich Reach routing.
func (s *DeviceRegistryStore) CreateHostedDevice(
	deviceName string,
	now time.Time,
	refreshTokenExpiresAt time.Time,
) (DeviceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(now)
	if len(s.registry.Devices) >= s.deviceLimit {
		return DeviceRecord{}, ErrDeviceLimitReached
	}

	device, err := newDeviceRecord(deviceName, now, refreshTokenExpiresAt)
	if err != nil {
		return DeviceRecord{}, err
	}
	s.registry.Devices = append(s.registry.Devices, device)
	if err := s.persistLocked(); err != nil {
		return DeviceRecord{}, err
	}
	return device, nil
}

// HasDevice reports whether a paired device still exists in the registry.
func (s *DeviceRegistryStore) HasDevice(deviceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(time.Now().UTC())
	for _, device := range s.registry.Devices {
		if device.DeviceID == deviceID {
			return true
		}
	}
	return false
}

// RenameDevice updates administrator-visible paired-device metadata without
// changing device identity, tokens, or upload state.
func (s *DeviceRegistryStore) RenameDevice(deviceID string, deviceName string) (DeviceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalizedDeviceID := strings.TrimSpace(deviceID)
	if normalizedDeviceID == "" {
		return DeviceRecord{}, ErrDeviceNotFound
	}
	normalizedDeviceName := strings.TrimSpace(deviceName)
	if normalizedDeviceName == "" {
		return DeviceRecord{}, ErrDeviceNameInvalid
	}

	s.cleanupExpiredLocked(time.Now().UTC())
	for index, device := range s.registry.Devices {
		if device.DeviceID != normalizedDeviceID {
			continue
		}
		if device.DeviceName == normalizedDeviceName {
			return device, nil
		}
		updated := cloneRegistry(s.registry)
		updated.Devices[index].DeviceName = normalizedDeviceName
		if err := writeDeviceRegistryFile(s.path, updated); err != nil {
			return DeviceRecord{}, err
		}
		s.registry = updated
		return s.registry.Devices[index], nil
	}
	return DeviceRecord{}, ErrDeviceNotFound
}

// RevokeDevice removes a paired app device and invalidates its refresh-token family.
func (s *DeviceRegistryStore) RevokeDevice(deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	normalizedDeviceID := strings.TrimSpace(deviceID)
	if normalizedDeviceID == "" {
		return ErrDeviceNotFound
	}
	s.cleanupExpiredLocked(time.Now().UTC())
	for index, device := range s.registry.Devices {
		if device.DeviceID != normalizedDeviceID {
			continue
		}
		updated := cloneRegistry(s.registry)
		updated.Devices = append(updated.Devices[:index], updated.Devices[index+1:]...)
		if err := writeDeviceRegistryFile(s.path, updated); err != nil {
			return err
		}
		s.registry = updated
		return nil
	}
	return ErrDeviceNotFound
}

func (s *DeviceRegistryStore) cleanupExpiredLocked(now time.Time) {
	activePairings := s.registry.PairingSessions[:0]
	for _, session := range s.registry.PairingSessions {
		if session.RedeemedAt != nil {
			continue
		}
		if now.After(session.ExpiresAt) {
			continue
		}
		activePairings = append(activePairings, session)
	}
	s.registry.PairingSessions = activePairings
}

func (s *DeviceRegistryStore) recordFailedPairingAttemptLocked(now time.Time) error {
	if len(s.registry.PairingSessions) == 0 {
		return nil
	}

	activeSession := s.registry.PairingSessions[0]
	activeSession.FailedAttempts++
	if activeSession.FailedAttempts >= maxPairingFailedAttempts {
		s.registry.PairingSessions = s.registry.PairingSessions[:0]
	} else {
		s.registry.PairingSessions[0] = activeSession
	}
	return s.persistLocked()
}

func (s *DeviceRegistryStore) persistLocked() error {
	return writeDeviceRegistryFile(s.path, s.registry)
}

func writeDeviceRegistryFile(path string, registry DeviceRegistry) error {
	raw, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal device registry: %w", err)
	}
	raw = append(raw, '\n')
	if err := atomicfile.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write device registry %s: %w", path, err)
	}
	return nil
}

func newDeviceRecord(
	deviceName string,
	now time.Time,
	refreshTokenExpiresAt time.Time,
) (DeviceRecord, error) {
	deviceID, err := randomHex(16)
	if err != nil {
		return DeviceRecord{}, fmt.Errorf("generate device id: %w", err)
	}
	refreshToken, err := randomBase64(32)
	if err != nil {
		return DeviceRecord{}, fmt.Errorf("generate refresh token: %w", err)
	}

	device := DeviceRecord{
		DeviceID:              deviceID,
		DeviceName:            strings.TrimSpace(deviceName),
		CreatedAt:             now.UTC(),
		LastRefreshedAt:       now.UTC(),
		RefreshTokenExpiresAt: refreshTokenExpiresAt.UTC(),
	}
	if err := device.setRefreshToken(refreshToken); err != nil {
		return DeviceRecord{}, fmt.Errorf("persist refresh token: %w", err)
	}
	return device, nil
}

func cloneRegistry(registry DeviceRegistry) DeviceRegistry {
	pairings := append([]PairingSession(nil), registry.PairingSessions...)
	devices := append([]DeviceRecord(nil), registry.Devices...)
	return DeviceRegistry{
		PairingSessions: pairings,
		Devices:         devices,
	}
}

func randomPairingCode() (string, error) {
	return randomHex(16)
}

func (d *DeviceRecord) UnmarshalJSON(data []byte) error {
	var payload deviceRecordJSON
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}

	*d = DeviceRecord{
		DeviceID:                payload.DeviceID,
		DeviceName:              payload.DeviceName,
		CreatedAt:               payload.CreatedAt,
		LastRefreshedAt:         payload.LastRefreshedAt,
		CurrentRefreshTokenHash: strings.TrimSpace(payload.CurrentRefreshTokenHash),
		RefreshTokenSalt:        strings.TrimSpace(payload.RefreshTokenSalt),
		RefreshTokenExpiresAt:   payload.RefreshTokenExpiresAt,
	}

	switch {
	case d.CurrentRefreshTokenHash != "" && d.RefreshTokenSalt != "":
		return nil
	case strings.TrimSpace(payload.CurrentRefreshToken) != "":
		return d.setRefreshToken(payload.CurrentRefreshToken)
	default:
		return errors.New("device record missing refresh token material")
	}
}

func (d *DeviceRecord) setRefreshToken(token string) error {
	normalizedToken := strings.TrimSpace(token)
	if normalizedToken == "" {
		return errors.New("refresh token must not be empty")
	}

	salt, err := randomBase64(16)
	if err != nil {
		return fmt.Errorf("generate refresh token salt: %w", err)
	}

	d.CurrentRefreshToken = normalizedToken
	d.RefreshTokenSalt = salt
	d.CurrentRefreshTokenHash = hashRefreshToken(normalizedToken, salt)
	return nil
}

func (d DeviceRecord) matchesRefreshToken(token string) bool {
	normalizedToken := strings.TrimSpace(token)
	if normalizedToken == "" || d.CurrentRefreshTokenHash == "" || d.RefreshTokenSalt == "" {
		return false
	}

	expectedHash := hashRefreshToken(normalizedToken, d.RefreshTokenSalt)
	return subtle.ConstantTimeCompare([]byte(expectedHash), []byte(d.CurrentRefreshTokenHash)) == 1
}

func hashRefreshToken(token string, salt string) string {
	sum := sha256.Sum256([]byte(salt + ":" + token))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}
