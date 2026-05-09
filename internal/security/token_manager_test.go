package security

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

func TestTokenManagerMintAndVerifyTimichAgentAudience(t *testing.T) {
	t.Parallel()

	manager, err := NewTokenManager(
		"agent-123",
		base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		time.Hour,
	)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}

	now := time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC)
	token, claims, err := manager.MintAccessToken("device-456", AudienceTimichAgent, now)
	if err != nil {
		t.Fatalf("MintAccessToken() error = %v", err)
	}
	if claims.AgentID != "agent-123" {
		t.Fatalf("claims.AgentID = %q, want %q", claims.AgentID, "agent-123")
	}
	if claims.AppDeviceID != "device-456" {
		t.Fatalf("claims.AppDeviceID = %q, want %q", claims.AppDeviceID, "device-456")
	}
	if claims.Audience != AudienceTimichAgent {
		t.Fatalf("claims.Audience = %q, want %q", claims.Audience, AudienceTimichAgent)
	}
	if claims.Scope != ScopeMedia {
		t.Fatalf("claims.Scope = %q, want %q", claims.Scope, ScopeMedia)
	}
	if claims.TokenID == "" {
		t.Fatal("claims.TokenID is empty")
	}
	if claims.KeyID == "" {
		t.Fatal("claims.KeyID is empty")
	}

	verifiedClaims, err := manager.VerifyAccessToken(token, AudienceTimichAgent, now.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("VerifyAccessToken() error = %v", err)
	}
	if verifiedClaims != claims {
		t.Fatalf("VerifyAccessToken() claims = %+v, want %+v", verifiedClaims, claims)
	}
}

func TestTokenManagerRejectsWrongAudience(t *testing.T) {
	t.Parallel()

	manager, err := NewTokenManager(
		"agent-123",
		base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		time.Hour,
	)
	if err != nil {
		t.Fatalf("NewTokenManager() error = %v", err)
	}

	now := time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC)
	token, _, err := manager.MintAccessToken("device-456", AudienceTimichAgent, now)
	if err != nil {
		t.Fatalf("MintAccessToken() error = %v", err)
	}

	_, err = manager.VerifyAccessToken(token, "timich-server", now.Add(30*time.Minute))
	if !errors.Is(err, ErrAccessTokenInvalid) {
		t.Fatalf("VerifyAccessToken() error = %v, want %v", err, ErrAccessTokenInvalid)
	}
}
