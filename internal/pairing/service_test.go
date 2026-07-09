package pairing

import (
	"encoding/base64"
	"errors"
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

func TestNearbyLinkPollCreatesSessionAfterApproval(t *testing.T) {
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

	link, err := service.CreateNearbyLink("Living Room TV", "android_tv")
	if err != nil {
		t.Fatalf("CreateNearbyLink() error = %v", err)
	}
	if link.LinkCode == "" || link.PollToken == "" {
		t.Fatalf("CreateNearbyLink() = %+v, want code and poll token", link)
	}

	pending, err := service.PollNearbyLink(link.LinkID, link.PollToken, "http://127.0.0.1:8082")
	if err != nil {
		t.Fatalf("PollNearbyLink() pending error = %v", err)
	}
	if pending.Status != store.NearbyLinkStatusPending || pending.Session != nil {
		t.Fatalf("pending response = %+v, want pending without session", pending)
	}

	if _, err := service.ApproveNearbyLink(link.LinkCode); err != nil {
		t.Fatalf("ApproveNearbyLink() error = %v", err)
	}
	approved, err := service.PollNearbyLink(link.LinkID, link.PollToken, "http://127.0.0.1:8082")
	if err != nil {
		t.Fatalf("PollNearbyLink() approved error = %v", err)
	}
	if approved.Status != store.NearbyLinkStatusApproved {
		t.Fatalf("approved status = %q, want %q", approved.Status, store.NearbyLinkStatusApproved)
	}
	if approved.Session == nil {
		t.Fatal("approved response missing session")
	}
	if approved.Session.DeviceID == "" || approved.Session.RefreshToken == "" {
		t.Fatalf("session = %+v, want device id and refresh token", approved.Session)
	}
	if approved.Session.AccessMode != AccessModeFull {
		t.Fatalf("AccessMode = %q, want %q", approved.Session.AccessMode, AccessModeFull)
	}
}

func TestNearbyLinkCancelPreventsApproval(t *testing.T) {
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

	link, err := service.CreateNearbyLink("Living Room TV", "android_tv")
	if err != nil {
		t.Fatalf("CreateNearbyLink() error = %v", err)
	}
	canceled, err := service.CancelNearbyLink(link.LinkID, link.PollToken)
	if err != nil {
		t.Fatalf("CancelNearbyLink() error = %v", err)
	}
	if canceled.Status != store.NearbyLinkStatusDenied || canceled.PollToken != "" {
		t.Fatalf("canceled = %+v, want denied without poll token", canceled)
	}
	if _, err := service.ApproveNearbyLink(link.LinkCode); !errors.Is(err, store.ErrNearbyLinkDenied) {
		t.Fatalf("ApproveNearbyLink(canceled) error = %v, want %v", err, store.ErrNearbyLinkDenied)
	}
}
