package pairing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/security"
	"github.com/rsahara/timich-agent/internal/store"
)

const (
	defaultPairingTTL      = 10 * time.Minute
	defaultNearbyLinkTTL   = 5 * time.Minute
	defaultAccessTokenTTL  = 12 * time.Hour
	defaultRefreshTokenTTL = 30 * 24 * time.Hour
	AccessModeFull         = "full"
)

// SessionBundle is the app-facing local agent session response.
type SessionBundle struct {
	AccessToken            string    `json:"accessToken"`
	RefreshToken           string    `json:"refreshToken"`
	AgentID                string    `json:"agentId"`
	AgentName              string    `json:"agentName"`
	DeviceID               string    `json:"deviceId"`
	BaseURL                string    `json:"baseURL"`
	AccessMode             string    `json:"accessMode"`
	AccessTokenExpiresAt   time.Time `json:"accessTokenExpiresAt"`
	RefreshTokenExpiresAt  time.Time `json:"refreshTokenExpiresAt"`
	CertificateFingerprint string    `json:"certificateFingerprint,omitempty"`
}

// PairingSessionResponse summarizes a pending local pairing session.
type PairingSessionResponse struct {
	PairingCode string    `json:"pairingCode"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// PairingStatus is the coarse local readiness signal exposed to app clients.
type PairingStatus string

const (
	PairingStatusNoDatasourceConfigured         PairingStatus = "no datasource configured"
	PairingStatusReadyForFirstLocalPairing      PairingStatus = "ready for first local pairing"
	PairingStatusPairingSessionAvailable        PairingStatus = "pairing session available"
	PairingStatusReadyForAdditionalLocalPairing PairingStatus = "ready for additional local pairing"
)

// NearbyLinkResponse is the app-facing response for a pending LAN approval request.
type NearbyLinkResponse struct {
	LinkID     string    `json:"linkId"`
	LinkCode   string    `json:"linkCode"`
	PollToken  string    `json:"pollToken,omitempty"`
	DeviceName string    `json:"deviceName"`
	DeviceKind string    `json:"deviceKind"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// NearbyLinkPollResponse is returned to an app polling for local admin approval.
type NearbyLinkPollResponse struct {
	LinkID    string         `json:"linkId"`
	Status    string         `json:"status"`
	ExpiresAt time.Time      `json:"expiresAt"`
	Session   *SessionBundle `json:"session,omitempty"`
}

// Service owns local pairing, refresh, and access-token verification.
type Service struct {
	agentID            string
	agentName          string
	registry           *store.DeviceRegistryStore
	tokens             *security.TokenManager
	refreshRotationKey [sha256.Size]byte
}

// NewService builds the local pairing/session service.
func NewService(
	agentID string,
	agentName string,
	encodedSigningKey string,
	registry *store.DeviceRegistryStore,
) (*Service, error) {
	tokenManager, err := security.NewTokenManager(agentID, encodedSigningKey, defaultAccessTokenTTL)
	if err != nil {
		return nil, err
	}
	return &Service{
		agentID:            agentID,
		agentName:          agentName,
		registry:           registry,
		tokens:             tokenManager,
		refreshRotationKey: sha256.Sum256([]byte("timich-refresh-rotation-v1\x00" + encodedSigningKey)),
	}, nil
}

// CreatePairingSession issues a one-time local pairing code.
func (s *Service) CreatePairingSession() (PairingSessionResponse, error) {
	now := time.Now().UTC()
	session, err := s.registry.CreatePairingSession(now, now.Add(defaultPairingTTL))
	if err != nil {
		return PairingSessionResponse{}, err
	}
	return PairingSessionResponse{
		PairingCode: session.Code,
		ExpiresAt:   session.ExpiresAt,
	}, nil
}

// CreateNearbyLink starts a LAN-local admin approval request for a nearby app device.
func (s *Service) CreateNearbyLink(deviceName string, deviceKind string) (NearbyLinkResponse, error) {
	now := time.Now().UTC()
	link, pollToken, err := s.registry.CreateNearbyLink(
		strings.TrimSpace(deviceName),
		strings.TrimSpace(deviceKind),
		now,
		now.Add(defaultNearbyLinkTTL),
	)
	if err != nil {
		return NearbyLinkResponse{}, err
	}
	return nearbyLinkResponse(link, pollToken), nil
}

// NearbyLinks returns active local approval requests for the admin surface.
func (s *Service) NearbyLinks() ([]NearbyLinkResponse, error) {
	links := s.registry.NearbyLinks(time.Now().UTC())
	responses := make([]NearbyLinkResponse, 0, len(links))
	for _, link := range links {
		responses = append(responses, nearbyLinkResponse(link, ""))
	}
	return responses, nil
}

// ApproveNearbyLink marks a Link Code as locally approved by an administrator.
func (s *Service) ApproveNearbyLink(linkCode string) (NearbyLinkResponse, error) {
	link, err := s.registry.ApproveNearbyLink(linkCode, time.Now().UTC())
	if err != nil {
		return NearbyLinkResponse{}, err
	}
	return nearbyLinkResponse(link, ""), nil
}

// DenyNearbyLink rejects a pending local approval request.
func (s *Service) DenyNearbyLink(linkID string) (NearbyLinkResponse, error) {
	link, err := s.registry.DenyNearbyLink(linkID, time.Now().UTC())
	if err != nil {
		return NearbyLinkResponse{}, err
	}
	return nearbyLinkResponse(link, ""), nil
}

// CancelNearbyLink lets the requesting app cancel its own pending local approval request.
func (s *Service) CancelNearbyLink(linkID string, pollToken string) (NearbyLinkResponse, error) {
	link, err := s.registry.CancelNearbyLink(linkID, pollToken, time.Now().UTC())
	if err != nil {
		return NearbyLinkResponse{}, err
	}
	return nearbyLinkResponse(link, ""), nil
}

// PollNearbyLink returns pending/denied state or consumes an approved link into an app session.
func (s *Service) PollNearbyLink(linkID string, pollToken string, baseURL string) (NearbyLinkPollResponse, error) {
	now := time.Now().UTC()
	link, err := s.registry.PollNearbyLink(linkID, pollToken, now)
	if err != nil {
		return NearbyLinkPollResponse{}, err
	}
	response := NearbyLinkPollResponse{
		LinkID:    link.LinkID,
		Status:    link.Status(),
		ExpiresAt: link.ExpiresAt,
	}
	if response.Status != store.NearbyLinkStatusApproved {
		return response, nil
	}

	device, link, err := s.registry.RedeemNearbyLink(
		linkID,
		pollToken,
		now,
		now.Add(defaultRefreshTokenTTL),
	)
	if err != nil {
		return NearbyLinkPollResponse{}, err
	}
	bundle, err := s.buildSessionBundle(device, baseURL, now)
	if err != nil {
		return NearbyLinkPollResponse{}, err
	}
	response.Status = link.Status()
	response.ExpiresAt = link.ExpiresAt
	response.Session = &bundle
	return response, nil
}

// ActivePairingSession returns the current unredeemed, unexpired pairing session for a code.
func (s *Service) ActivePairingSession(code string) (PairingSessionResponse, error) {
	normalizedCode := strings.TrimSpace(code)
	if normalizedCode == "" {
		return PairingSessionResponse{}, store.ErrPairingSessionNotFound
	}
	snapshot := s.registry.Snapshot()
	for _, session := range snapshot.PairingSessions {
		if session.Code == normalizedCode {
			return PairingSessionResponse{
				PairingCode: session.Code,
				ExpiresAt:   session.ExpiresAt,
			}, nil
		}
	}
	return PairingSessionResponse{}, store.ErrPairingSessionNotFound
}

// RedeemPairing exchanges a one-time code for an agent-owned app session.
func (s *Service) RedeemPairing(code string, deviceName string, baseURL string) (SessionBundle, error) {
	now := time.Now().UTC()
	device, err := s.registry.RedeemPairingSession(
		code,
		strings.TrimSpace(deviceName),
		now,
		now.Add(defaultRefreshTokenTTL),
	)
	if err != nil {
		return SessionBundle{}, err
	}
	return s.buildSessionBundle(device, baseURL, now)
}

// CreateHostedSession creates a Remote Browsing app session without a prior LAN
// pairing code. This keeps the local pairing boundary intact for LAN usage while
// letting the relay control plane provision a remote session during preview.
func (s *Service) CreateHostedSession(deviceName string, baseURL string) (SessionBundle, error) {
	now := time.Now().UTC()
	device, err := s.registry.CreateHostedDevice(
		strings.TrimSpace(deviceName),
		now,
		now.Add(defaultRefreshTokenTTL),
	)
	if err != nil {
		return SessionBundle{}, err
	}
	return s.buildSessionBundle(device, baseURL, now)
}

// RefreshSession rotates the refresh-token family and mints a fresh access token.
func (s *Service) RefreshSession(refreshToken string, baseURL string) (SessionBundle, error) {
	now := time.Now().UTC()
	normalizedRefreshToken := strings.TrimSpace(refreshToken)
	replacementRefreshToken := s.nextRefreshToken(normalizedRefreshToken)
	device, err := s.registry.RotateRefreshToken(
		normalizedRefreshToken,
		replacementRefreshToken,
		now,
		now.Add(defaultRefreshTokenTTL),
	)
	if err != nil {
		return SessionBundle{}, err
	}
	return s.buildSessionBundle(device, baseURL, now)
}

func (s *Service) nextRefreshToken(refreshToken string) string {
	digest := hmac.New(sha256.New, s.refreshRotationKey[:])
	_, _ = digest.Write([]byte(strings.TrimSpace(refreshToken)))
	return base64.RawURLEncoding.EncodeToString(digest.Sum(nil))
}

// AuthenticateAccessToken verifies the bearer token and confirms the device still exists.
func (s *Service) AuthenticateAccessToken(token string) (security.AccessTokenClaims, error) {
	claims, err := s.tokens.VerifyAccessToken(
		strings.TrimSpace(token),
		security.AudienceTimichAgent,
		time.Now().UTC(),
	)
	if err != nil {
		return security.AccessTokenClaims{}, err
	}
	if !s.registry.HasDevice(claims.AppDeviceID) {
		return security.AccessTokenClaims{}, security.ErrAccessTokenInvalid
	}
	return claims, nil
}

// PairingStatus returns a public summary of pairing readiness.
func (s *Service) PairingStatus() PairingStatus {
	snapshot := s.registry.Snapshot()
	switch {
	case len(snapshot.Devices) == 0:
		return PairingStatusReadyForFirstLocalPairing
	case len(snapshot.PairingSessions) > 0:
		return PairingStatusPairingSessionAvailable
	default:
		return PairingStatusReadyForAdditionalLocalPairing
	}
}

func (s *Service) buildSessionBundle(
	device store.DeviceRecord,
	baseURL string,
	now time.Time,
) (SessionBundle, error) {
	accessToken, claims, err := s.tokens.MintAccessToken(
		device.DeviceID,
		security.AudienceTimichAgent,
		now,
	)
	if err != nil {
		return SessionBundle{}, err
	}
	return SessionBundle{
		AccessToken:           accessToken,
		RefreshToken:          device.CurrentRefreshToken,
		AgentID:               s.agentID,
		AgentName:             s.agentName,
		DeviceID:              device.DeviceID,
		BaseURL:               strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		AccessMode:            AccessModeFull,
		AccessTokenExpiresAt:  time.Unix(claims.ExpiresAt, 0).UTC(),
		RefreshTokenExpiresAt: device.RefreshTokenExpiresAt.UTC(),
	}, nil
}

// IsClientError reports whether the error should map to a 4xx response.
func IsClientError(err error) bool {
	return errors.Is(err, store.ErrPairingSessionNotFound) ||
		errors.Is(err, store.ErrPairingSessionExpired) ||
		errors.Is(err, store.ErrPairingSessionUsed) ||
		errors.Is(err, store.ErrDeviceLimitReached) ||
		errors.Is(err, store.ErrRefreshTokenNotFound) ||
		errors.Is(err, store.ErrRefreshTokenExpired) ||
		errors.Is(err, store.ErrNearbyLinkNotFound) ||
		errors.Is(err, store.ErrNearbyLinkDenied) ||
		errors.Is(err, store.ErrNearbyLinkNotApproved) ||
		errors.Is(err, store.ErrNearbyLinkConsumed) ||
		errors.Is(err, store.ErrNearbyLinkPollTokenInvalid) ||
		errors.Is(err, store.ErrNearbyLinkLimitReached)
}

func nearbyLinkResponse(link store.NearbyLink, pollToken string) NearbyLinkResponse {
	return NearbyLinkResponse{
		LinkID:     link.LinkID,
		LinkCode:   link.LinkCode,
		PollToken:  pollToken,
		DeviceName: link.DeviceName,
		DeviceKind: link.DeviceKind,
		Status:     link.Status(),
		CreatedAt:  link.CreatedAt,
		ExpiresAt:  link.ExpiresAt,
	}
}
