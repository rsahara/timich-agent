package controlplane

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

func TestMintAndVerifyControlPlaneToken(t *testing.T) {
	t.Parallel()

	publicKey, privateKey := testKeyPair(t)
	now := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	token, err := MintControlPlaneToken(privateKey, "key-1", "agent-home", now, 5*time.Minute)
	if err != nil {
		t.Fatalf("MintControlPlaneToken() error = %v", err)
	}

	unverified, err := ParseUnverifiedControlPlaneToken(token)
	if err != nil {
		t.Fatalf("ParseUnverifiedControlPlaneToken() error = %v", err)
	}
	if unverified.AgentID != "agent-home" || unverified.KeyID != "key-1" {
		t.Fatalf("unverified claims = %+v, want agent/key identity", unverified)
	}

	claims, err := VerifyControlPlaneToken(publicKey, token, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("VerifyControlPlaneToken() error = %v", err)
	}
	if claims.AgentID != "agent-home" || claims.KeyID != "key-1" {
		t.Fatalf("claims = %+v, want agent/key identity", claims)
	}
}

func TestVerifyControlPlaneTokenRejectsExpiredToken(t *testing.T) {
	t.Parallel()

	publicKey, privateKey := testKeyPair(t)
	now := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	token, err := MintControlPlaneToken(privateKey, "key-1", "agent-home", now, time.Minute)
	if err != nil {
		t.Fatalf("MintControlPlaneToken() error = %v", err)
	}

	_, err = VerifyControlPlaneToken(publicKey, token, now.Add(2*time.Minute))
	if !errors.Is(err, ErrExpiredControlPlaneToken) {
		t.Fatalf("VerifyControlPlaneToken() err = %v, want expired token", err)
	}
}

func TestVerifyControlPlaneTokenRejectsWrongPublicKey(t *testing.T) {
	t.Parallel()

	_, privateKey := testKeyPair(t)
	wrongPublicKey, _ := testKeyPair(t)
	now := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	token, err := MintControlPlaneToken(privateKey, "key-1", "agent-home", now, time.Minute)
	if err != nil {
		t.Fatalf("MintControlPlaneToken() error = %v", err)
	}

	_, err = VerifyControlPlaneToken(wrongPublicKey, token, now)
	if !errors.Is(err, ErrInvalidControlPlaneToken) {
		t.Fatalf("VerifyControlPlaneToken() err = %v, want invalid token", err)
	}
}

func testKeyPair(t *testing.T) (string, string) {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	return base64.RawStdEncoding.EncodeToString(publicKey), base64.RawStdEncoding.EncodeToString(privateKey)
}
