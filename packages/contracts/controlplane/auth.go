package controlplane

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	ControlPlaneAudience         = "timich-server-control-plane"
	ControlPlaneAuthorizationKey = "authorization"
)

var (
	ErrInvalidControlPlaneToken = errors.New("invalid control-plane token")
	ErrExpiredControlPlaneToken = errors.New("expired control-plane token")
)

type ControlPlaneTokenClaims struct {
	AgentID   string    `json:"agentId"`
	KeyID     string    `json:"keyId"`
	Audience  string    `json:"aud"`
	IssuedAt  time.Time `json:"iat"`
	ExpiresAt time.Time `json:"exp"`
}

type controlPlaneTokenHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

type controlPlaneTokenPayload struct {
	AgentID   string `json:"agentId"`
	KeyID     string `json:"keyId"`
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

func MintControlPlaneToken(privateKey string, keyID string, agentID string, now time.Time, ttl time.Duration) (string, error) {
	agentID = strings.TrimSpace(agentID)
	keyID = strings.TrimSpace(keyID)
	privateKeyBytes, err := decodeEd25519PrivateKey(privateKey)
	if err != nil || agentID == "" || keyID == "" || ttl <= 0 {
		return "", ErrInvalidControlPlaneToken
	}

	headerJSON, err := json.Marshal(controlPlaneTokenHeader{
		Algorithm: "EdDSA",
		Type:      "JWT",
		KeyID:     keyID,
	})
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(controlPlaneTokenPayload{
		AgentID:   agentID,
		KeyID:     keyID,
		Audience:  ControlPlaneAudience,
		IssuedAt:  now.UTC().Unix(),
		ExpiresAt: now.UTC().Add(ttl).Unix(),
	})
	if err != nil {
		return "", err
	}

	header := base64.RawURLEncoding.EncodeToString(headerJSON)
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := header + "." + payload
	signature := ed25519.Sign(privateKeyBytes, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func ParseUnverifiedControlPlaneToken(token string) (ControlPlaneTokenClaims, error) {
	header, payload, _, err := splitControlPlaneToken(token)
	if err != nil {
		return ControlPlaneTokenClaims{}, err
	}
	if header.Algorithm != "EdDSA" || header.Type != "JWT" {
		return ControlPlaneTokenClaims{}, ErrInvalidControlPlaneToken
	}
	return claimsFromPayload(header, payload)
}

func VerifyControlPlaneToken(publicKey string, token string, now time.Time) (ControlPlaneTokenClaims, error) {
	publicKeyBytes, err := decodeEd25519PublicKey(publicKey)
	if err != nil {
		return ControlPlaneTokenClaims{}, ErrInvalidControlPlaneToken
	}

	header, payload, parts, err := splitControlPlaneToken(token)
	if err != nil {
		return ControlPlaneTokenClaims{}, err
	}
	if header.Algorithm != "EdDSA" || header.Type != "JWT" {
		return ControlPlaneTokenClaims{}, ErrInvalidControlPlaneToken
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return ControlPlaneTokenClaims{}, ErrInvalidControlPlaneToken
	}
	signingInput := parts[0] + "." + parts[1]
	if !ed25519.Verify(publicKeyBytes, []byte(signingInput), signature) {
		return ControlPlaneTokenClaims{}, ErrInvalidControlPlaneToken
	}

	claims, err := claimsFromPayload(header, payload)
	if err != nil {
		return ControlPlaneTokenClaims{}, err
	}
	if !claims.ExpiresAt.After(now.UTC()) {
		return ControlPlaneTokenClaims{}, ErrExpiredControlPlaneToken
	}
	return claims, nil
}

func splitControlPlaneToken(token string) (controlPlaneTokenHeader, controlPlaneTokenPayload, []string, error) {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return controlPlaneTokenHeader{}, controlPlaneTokenPayload{}, nil, ErrInvalidControlPlaneToken
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return controlPlaneTokenHeader{}, controlPlaneTokenPayload{}, nil, ErrInvalidControlPlaneToken
	}
	var header controlPlaneTokenHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return controlPlaneTokenHeader{}, controlPlaneTokenPayload{}, nil, ErrInvalidControlPlaneToken
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return controlPlaneTokenHeader{}, controlPlaneTokenPayload{}, nil, ErrInvalidControlPlaneToken
	}
	var payload controlPlaneTokenPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return controlPlaneTokenHeader{}, controlPlaneTokenPayload{}, nil, ErrInvalidControlPlaneToken
	}
	return header, payload, parts, nil
}

func claimsFromPayload(header controlPlaneTokenHeader, payload controlPlaneTokenPayload) (ControlPlaneTokenClaims, error) {
	agentID := strings.TrimSpace(payload.AgentID)
	keyID := strings.TrimSpace(payload.KeyID)
	headerKeyID := strings.TrimSpace(header.KeyID)
	if agentID == "" || keyID == "" || headerKeyID == "" || keyID != headerKeyID || payload.Audience != ControlPlaneAudience {
		return ControlPlaneTokenClaims{}, ErrInvalidControlPlaneToken
	}
	return ControlPlaneTokenClaims{
		AgentID:   agentID,
		KeyID:     keyID,
		Audience:  payload.Audience,
		IssuedAt:  time.Unix(payload.IssuedAt, 0).UTC(),
		ExpiresAt: time.Unix(payload.ExpiresAt, 0).UTC(),
	}, nil
}

func decodeEd25519PrivateKey(encoded string) (ed25519.PrivateKey, error) {
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, ErrInvalidControlPlaneToken
	}
	return ed25519.PrivateKey(raw), nil
}

func decodeEd25519PublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, ErrInvalidControlPlaneToken
	}
	return ed25519.PublicKey(raw), nil
}
