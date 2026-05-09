package pairing

import (
	"encoding/base64"
	"testing"

	"github.com/rsahara/timich-agent/internal/store"
)

func TestRedeemPairingSessionBundleIncludesAccessMode(t *testing.T) {
	registry, err := store.LoadOrCreateDeviceRegistry(t.TempDir(), 5)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}
	service, err := NewService(
		"agent-home",
		"Home NAS",
		base64.RawStdEncoding.EncodeToString([]byte("test-signing-key")),
		registry,
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	pairingSession, err := service.CreatePairingSession()
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}

	bundle, err := service.RedeemPairing(
		pairingSession.PairingCode,
		"Test iPhone",
		"http://127.0.0.1:8082",
	)
	if err != nil {
		t.Fatalf("RedeemPairing() error = %v", err)
	}
	if bundle.AccessMode != AccessModeFull {
		t.Fatalf("AccessMode = %q, want %q", bundle.AccessMode, AccessModeFull)
	}
}
