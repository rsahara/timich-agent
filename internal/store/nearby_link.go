package store

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const (
	NearbyLinkStatusPending  = "pending"
	NearbyLinkStatusApproved = "approved"
	NearbyLinkStatusDenied   = "denied"

	maxActiveNearbyLinks          = 8
	maxNearbyLinkFailedAttempts   = 5
	nearbyLinkCodeLength          = 6
	nearbyLinkCodeGenerationTries = 16
)

var (
	ErrNearbyLinkNotFound         = errors.New("nearby link not found")
	ErrNearbyLinkDenied           = errors.New("nearby link denied")
	ErrNearbyLinkNotApproved      = errors.New("nearby link not approved")
	ErrNearbyLinkConsumed         = errors.New("nearby link already consumed")
	ErrNearbyLinkPollTokenInvalid = errors.New("nearby link poll token invalid")
	ErrNearbyLinkLimitReached     = errors.New("nearby link limit reached")
)

// NearbyLink stores a short-lived LAN approval request from an app device.
type NearbyLink struct {
	LinkID         string     `json:"linkId"`
	LinkCode       string     `json:"linkCode"`
	DeviceName     string     `json:"deviceName"`
	DeviceKind     string     `json:"deviceKind,omitempty"`
	PollTokenHash  string     `json:"pollTokenHash,omitempty"`
	PollTokenSalt  string     `json:"pollTokenSalt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	ExpiresAt      time.Time  `json:"expiresAt"`
	ApprovedAt     *time.Time `json:"approvedAt,omitempty"`
	DeniedAt       *time.Time `json:"deniedAt,omitempty"`
	ConsumedAt     *time.Time `json:"consumedAt,omitempty"`
	FailedAttempts int        `json:"failedAttempts,omitempty"`
}

// CreateNearbyLink creates a pending LAN approval request and returns the
// high-entropy poll token exactly once to the requesting app.
func (s *DeviceRegistryStore) CreateNearbyLink(
	deviceName string,
	deviceKind string,
	now time.Time,
	expiresAt time.Time,
) (NearbyLink, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(now)
	if len(s.registry.Devices) >= s.deviceLimit {
		return NearbyLink{}, "", ErrDeviceLimitReached
	}
	if activeNearbyLinkCount(s.registry.NearbyLinks) >= maxActiveNearbyLinks {
		return NearbyLink{}, "", ErrNearbyLinkLimitReached
	}

	linkID, err := randomHex(16)
	if err != nil {
		return NearbyLink{}, "", fmt.Errorf("generate nearby link id: %w", err)
	}
	linkCode, err := s.randomNearbyLinkCodeLocked()
	if err != nil {
		return NearbyLink{}, "", fmt.Errorf("generate nearby link code: %w", err)
	}
	pollToken, err := randomBase64(32)
	if err != nil {
		return NearbyLink{}, "", fmt.Errorf("generate nearby link poll token: %w", err)
	}
	salt, err := randomBase64(16)
	if err != nil {
		return NearbyLink{}, "", fmt.Errorf("generate nearby link poll token salt: %w", err)
	}

	link := NearbyLink{
		LinkID:        linkID,
		LinkCode:      linkCode,
		DeviceName:    strings.TrimSpace(deviceName),
		DeviceKind:    normalizeNearbyLinkDeviceKind(deviceKind),
		PollTokenHash: hashRefreshToken(pollToken, salt),
		PollTokenSalt: salt,
		CreatedAt:     now.UTC(),
		ExpiresAt:     expiresAt.UTC(),
	}
	if err := s.appendNearbyLinkLocked(link); err != nil {
		return NearbyLink{}, "", err
	}
	return link, pollToken, nil
}

// NearbyLinks returns a snapshot of active pending or recently denied links.
func (s *DeviceRegistryStore) NearbyLinks(now time.Time) []NearbyLink {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(now)
	return append([]NearbyLink(nil), s.registry.NearbyLinks...)
}

// ApproveNearbyLink marks the active link matching a human-entered Link Code
// as approved by a local administrator.
func (s *DeviceRegistryStore) ApproveNearbyLink(linkCode string, now time.Time) (NearbyLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(now)
	normalizedCode := normalizeNearbyLinkCode(linkCode)
	if normalizedCode == "" {
		return NearbyLink{}, ErrNearbyLinkNotFound
	}
	for index, link := range s.registry.NearbyLinks {
		if link.LinkCode != normalizedCode {
			continue
		}
		if link.ConsumedAt != nil {
			return NearbyLink{}, ErrNearbyLinkConsumed
		}
		if link.DeniedAt != nil {
			return NearbyLink{}, ErrNearbyLinkDenied
		}
		if link.ApprovedAt == nil {
			approvedAt := now.UTC()
			link.ApprovedAt = &approvedAt
			updatedLink, err := s.updateNearbyLinkLocked(index, link)
			if err != nil {
				return NearbyLink{}, err
			}
			return updatedLink, nil
		}
		return s.registry.NearbyLinks[index], nil
	}
	return NearbyLink{}, ErrNearbyLinkNotFound
}

// DenyNearbyLink marks a pending link as denied.
func (s *DeviceRegistryStore) DenyNearbyLink(linkID string, now time.Time) (NearbyLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(now)
	normalizedLinkID := strings.TrimSpace(linkID)
	if normalizedLinkID == "" {
		return NearbyLink{}, ErrNearbyLinkNotFound
	}
	for index, link := range s.registry.NearbyLinks {
		if link.LinkID != normalizedLinkID {
			continue
		}
		if link.ConsumedAt != nil {
			return NearbyLink{}, ErrNearbyLinkConsumed
		}
		if link.DeniedAt == nil {
			deniedAt := now.UTC()
			link.DeniedAt = &deniedAt
			updatedLink, err := s.updateNearbyLinkLocked(index, link)
			if err != nil {
				return NearbyLink{}, err
			}
			return updatedLink, nil
		}
		return s.registry.NearbyLinks[index], nil
	}
	return NearbyLink{}, ErrNearbyLinkNotFound
}

// CancelNearbyLink verifies the app poll token and marks the link as denied.
func (s *DeviceRegistryStore) CancelNearbyLink(linkID string, pollToken string, now time.Time) (NearbyLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(now)
	index, link, err := s.nearbyLinkByIDLocked(linkID)
	if err != nil {
		return NearbyLink{}, err
	}
	if !link.matchesPollToken(pollToken) {
		if err := s.recordFailedNearbyLinkAttemptLocked(index, now); err != nil {
			return NearbyLink{}, err
		}
		return NearbyLink{}, ErrNearbyLinkPollTokenInvalid
	}
	if link.ConsumedAt != nil {
		return NearbyLink{}, ErrNearbyLinkConsumed
	}
	if link.DeniedAt == nil {
		deniedAt := now.UTC()
		link.DeniedAt = &deniedAt
		updatedLink, err := s.updateNearbyLinkLocked(index, link)
		if err != nil {
			return NearbyLink{}, err
		}
		return updatedLink, nil
	}
	return s.registry.NearbyLinks[index], nil
}

// PollNearbyLink verifies the app poll token and returns the current link state.
func (s *DeviceRegistryStore) PollNearbyLink(linkID string, pollToken string, now time.Time) (NearbyLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(now)
	index, link, err := s.nearbyLinkByIDLocked(linkID)
	if err != nil {
		return NearbyLink{}, err
	}
	if !link.matchesPollToken(pollToken) {
		if err := s.recordFailedNearbyLinkAttemptLocked(index, now); err != nil {
			return NearbyLink{}, err
		}
		return NearbyLink{}, ErrNearbyLinkPollTokenInvalid
	}
	return link, nil
}

// RedeemNearbyLink consumes an admin-approved Nearby Link and creates the
// paired app device in the same critical section.
func (s *DeviceRegistryStore) RedeemNearbyLink(
	linkID string,
	pollToken string,
	now time.Time,
	refreshTokenExpiresAt time.Time,
) (DeviceRecord, NearbyLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(now)
	if len(s.registry.Devices) >= s.deviceLimit {
		return DeviceRecord{}, NearbyLink{}, ErrDeviceLimitReached
	}
	index, link, err := s.nearbyLinkByIDLocked(linkID)
	if err != nil {
		return DeviceRecord{}, NearbyLink{}, err
	}
	if !link.matchesPollToken(pollToken) {
		if err := s.recordFailedNearbyLinkAttemptLocked(index, now); err != nil {
			return DeviceRecord{}, NearbyLink{}, err
		}
		return DeviceRecord{}, NearbyLink{}, ErrNearbyLinkPollTokenInvalid
	}
	if link.ConsumedAt != nil {
		return DeviceRecord{}, NearbyLink{}, ErrNearbyLinkConsumed
	}
	if link.DeniedAt != nil {
		return DeviceRecord{}, NearbyLink{}, ErrNearbyLinkDenied
	}
	if link.ApprovedAt == nil {
		return DeviceRecord{}, NearbyLink{}, ErrNearbyLinkNotApproved
	}

	device, err := newDeviceRecord(link.DeviceName, now, refreshTokenExpiresAt)
	if err != nil {
		return DeviceRecord{}, NearbyLink{}, err
	}
	consumedAt := now.UTC()
	link.ConsumedAt = &consumedAt

	updated := cloneRegistry(s.registry)
	updated.NearbyLinks = append(updated.NearbyLinks[:index], updated.NearbyLinks[index+1:]...)
	updated.Devices = append(updated.Devices, device)
	if err := writeDeviceRegistryFile(s.path, updated); err != nil {
		return DeviceRecord{}, NearbyLink{}, err
	}
	s.registry = updated
	return device, link, nil
}

func (s *DeviceRegistryStore) nearbyLinkByIDLocked(linkID string) (int, NearbyLink, error) {
	normalizedLinkID := strings.TrimSpace(linkID)
	if normalizedLinkID == "" {
		return -1, NearbyLink{}, ErrNearbyLinkNotFound
	}
	for index, link := range s.registry.NearbyLinks {
		if link.LinkID == normalizedLinkID {
			return index, link, nil
		}
	}
	return -1, NearbyLink{}, ErrNearbyLinkNotFound
}

func (s *DeviceRegistryStore) appendNearbyLinkLocked(link NearbyLink) error {
	updated := cloneRegistry(s.registry)
	updated.NearbyLinks = append(updated.NearbyLinks, link)
	if err := writeDeviceRegistryFile(s.path, updated); err != nil {
		return err
	}
	s.registry = updated
	return nil
}

func (s *DeviceRegistryStore) updateNearbyLinkLocked(index int, link NearbyLink) (NearbyLink, error) {
	if index < 0 || index >= len(s.registry.NearbyLinks) {
		return NearbyLink{}, ErrNearbyLinkNotFound
	}
	updated := cloneRegistry(s.registry)
	updated.NearbyLinks[index] = link
	if err := writeDeviceRegistryFile(s.path, updated); err != nil {
		return NearbyLink{}, err
	}
	s.registry = updated
	return s.registry.NearbyLinks[index], nil
}

func (s *DeviceRegistryStore) recordFailedNearbyLinkAttemptLocked(index int, now time.Time) error {
	if index < 0 || index >= len(s.registry.NearbyLinks) {
		return ErrNearbyLinkNotFound
	}
	link := s.registry.NearbyLinks[index]
	link.FailedAttempts++
	if link.FailedAttempts >= maxNearbyLinkFailedAttempts {
		deniedAt := now.UTC()
		link.DeniedAt = &deniedAt
	}
	_, err := s.updateNearbyLinkLocked(index, link)
	return err
}

func (s *DeviceRegistryStore) randomNearbyLinkCodeLocked() (string, error) {
	for range nearbyLinkCodeGenerationTries {
		code, err := randomNearbyLinkCode()
		if err != nil {
			return "", err
		}
		if !nearbyLinkCodeExists(s.registry.NearbyLinks, code) {
			return code, nil
		}
	}
	return "", ErrNearbyLinkLimitReached
}

func randomNearbyLinkCode() (string, error) {
	maxValue := new(big.Int).Exp(big.NewInt(10), big.NewInt(nearbyLinkCodeLength), nil)
	value, err := rand.Int(rand.Reader, maxValue)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", nearbyLinkCodeLength, value.Int64()), nil
}

func nearbyLinkCodeExists(links []NearbyLink, code string) bool {
	for _, link := range links {
		if link.ConsumedAt == nil && link.LinkCode == code {
			return true
		}
	}
	return false
}

func activeNearbyLinkCount(links []NearbyLink) int {
	count := 0
	for _, link := range links {
		if link.ConsumedAt == nil && link.DeniedAt == nil {
			count++
		}
	}
	return count
}

func normalizeNearbyLinkCode(value string) string {
	var builder strings.Builder
	for _, char := range strings.TrimSpace(value) {
		switch {
		case char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == ' ' || char == '-':
			continue
		default:
			return ""
		}
	}
	if builder.Len() != nearbyLinkCodeLength {
		return ""
	}
	return builder.String()
}

func normalizeNearbyLinkDeviceKind(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "android_tv", "ios", "android", "mcp":
		return normalized
	default:
		return "unknown"
	}
}

func (l NearbyLink) Status() string {
	switch {
	case l.DeniedAt != nil:
		return NearbyLinkStatusDenied
	case l.ApprovedAt != nil:
		return NearbyLinkStatusApproved
	default:
		return NearbyLinkStatusPending
	}
}

func (l NearbyLink) matchesPollToken(token string) bool {
	normalizedToken := strings.TrimSpace(token)
	if normalizedToken == "" || l.PollTokenHash == "" || l.PollTokenSalt == "" {
		return false
	}
	expectedHash := hashRefreshToken(normalizedToken, l.PollTokenSalt)
	return subtle.ConstantTimeCompare([]byte(expectedHash), []byte(l.PollTokenHash)) == 1
}
