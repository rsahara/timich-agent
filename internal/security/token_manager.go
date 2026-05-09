package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrAccessTokenInvalid = errors.New("access token invalid")
	ErrAccessTokenExpired = errors.New("access token expired")
)

const (
	AudienceTimichAgent = "timich-agent"
	ScopeMedia          = "media"
)

// AccessTokenClaims describes the agent-issued LAN access token.
type AccessTokenClaims struct {
	AgentID     string `json:"agentId"`
	AppDeviceID string `json:"appDeviceId"`
	Audience    string `json:"aud"`
	Scope       string `json:"scope"`
	TokenID     string `json:"jti"`
	KeyID       string `json:"kid"`
	IssuedAt    int64  `json:"iat"`
	ExpiresAt   int64  `json:"exp"`
}

// TokenManager signs and verifies agent-issued access tokens.
type TokenManager struct {
	agentID string
	key     []byte
	keyID   string
	ttl     time.Duration
}

// NewTokenManager creates a new HMAC-based access-token manager.
func NewTokenManager(agentID string, encodedKey string, ttl time.Duration) (*TokenManager, error) {
	if strings.TrimSpace(agentID) == "" {
		return nil, errors.New("agent id must not be empty")
	}
	if ttl <= 0 {
		return nil, errors.New("token ttl must be positive")
	}
	key, err := base64.RawStdEncoding.DecodeString(encodedKey)
	if err != nil {
		return nil, fmt.Errorf("decode signing key: %w", err)
	}
	if len(key) == 0 {
		return nil, errors.New("decoded signing key must not be empty")
	}
	return &TokenManager{
		agentID: agentID,
		key:     key,
		keyID:   deriveKeyID(key),
		ttl:     ttl,
	}, nil
}

// MintAccessToken signs a short-lived LAN access token for a paired app device.
func (m *TokenManager) MintAccessToken(deviceID string, audience string, now time.Time) (string, AccessTokenClaims, error) {
	normalizedDeviceID := strings.TrimSpace(deviceID)
	if normalizedDeviceID == "" {
		return "", AccessTokenClaims{}, errors.New("device id must not be empty")
	}
	normalizedAudience := strings.TrimSpace(audience)
	if normalizedAudience == "" {
		return "", AccessTokenClaims{}, errors.New("token audience must not be empty")
	}
	tokenID, err := randomTokenID()
	if err != nil {
		return "", AccessTokenClaims{}, fmt.Errorf("generate token id: %w", err)
	}

	claims := AccessTokenClaims{
		AgentID:     m.agentID,
		AppDeviceID: normalizedDeviceID,
		Audience:    normalizedAudience,
		Scope:       ScopeMedia,
		TokenID:     tokenID,
		KeyID:       m.keyID,
		IssuedAt:    now.UTC().Unix(),
		ExpiresAt:   now.UTC().Add(m.ttl).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", AccessTokenClaims{}, fmt.Errorf("marshal access token claims: %w", err)
	}

	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signature := m.sign(payload)
	encodedSignature := base64.RawURLEncoding.EncodeToString(signature)
	return encodedPayload + "." + encodedSignature, claims, nil
}

// VerifyAccessToken verifies the HMAC signature and expiry of a LAN access token.
func (m *TokenManager) VerifyAccessToken(token string, expectedAudience string, now time.Time) (AccessTokenClaims, error) {
	normalizedAudience := strings.TrimSpace(expectedAudience)
	if normalizedAudience == "" {
		return AccessTokenClaims{}, ErrAccessTokenInvalid
	}

	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return AccessTokenClaims{}, ErrAccessTokenInvalid
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return AccessTokenClaims{}, ErrAccessTokenInvalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return AccessTokenClaims{}, ErrAccessTokenInvalid
	}
	expectedSignature := m.sign(payload)
	if !hmac.Equal(signature, expectedSignature) {
		return AccessTokenClaims{}, ErrAccessTokenInvalid
	}

	var claims AccessTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return AccessTokenClaims{}, ErrAccessTokenInvalid
	}
	if claims.AgentID != m.agentID ||
		claims.AppDeviceID == "" ||
		claims.Audience != normalizedAudience ||
		claims.Scope != ScopeMedia ||
		claims.TokenID == "" ||
		claims.KeyID != m.keyID {
		return AccessTokenClaims{}, ErrAccessTokenInvalid
	}
	if now.UTC().Unix() >= claims.ExpiresAt {
		return AccessTokenClaims{}, ErrAccessTokenExpired
	}
	return claims, nil
}

func (m *TokenManager) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, m.key)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

func deriveKeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return base64.RawURLEncoding.EncodeToString(sum[:8])
}

func randomTokenID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
