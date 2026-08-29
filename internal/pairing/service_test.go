package pairing

import (
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/security"
	"github.com/rsahara/timich-agent/internal/store"
)

func TestPairingSessionLifecycle(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t, 5)
	if got := service.PairingStatus(); got != PairingStatusReadyForFirstLocalPairing {
		t.Fatalf("PairingStatus() = %q, want %q", got, PairingStatusReadyForFirstLocalPairing)
	}

	pairingSession, err := service.CreatePairingSession()
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}
	active, err := service.ActivePairingSession("  " + pairingSession.PairingCode + "  ")
	if err != nil {
		t.Fatalf("ActivePairingSession() error = %v", err)
	}
	if active != pairingSession {
		t.Fatalf("ActivePairingSession() = %+v, want %+v", active, pairingSession)
	}
	if got := service.PairingStatus(); got != PairingStatusReadyForFirstLocalPairing {
		t.Fatalf("PairingStatus() with no devices = %q, want %q", got, PairingStatusReadyForFirstLocalPairing)
	}

	startedAt := time.Now().UTC()
	bundle, err := service.RedeemPairing(
		pairingSession.PairingCode,
		"  Test iPhone  ",
		"  http://127.0.0.1:8082///  ",
	)
	if err != nil {
		t.Fatalf("RedeemPairing() error = %v", err)
	}
	completedAt := time.Now().UTC()
	if bundle.AccessToken == "" || bundle.RefreshToken == "" || bundle.DeviceID == "" {
		t.Fatal("RedeemPairing() returned an empty access token, refresh token, or device ID")
	}
	if bundle.AgentID != "agent-home" || bundle.AgentName != "Home NAS" {
		t.Fatalf("RedeemPairing() agent = %q/%q, want agent-home/Home NAS", bundle.AgentID, bundle.AgentName)
	}
	if bundle.BaseURL != "http://127.0.0.1:8082" {
		t.Fatalf("RedeemPairing() BaseURL = %q, want normalized URL", bundle.BaseURL)
	}
	if bundle.AccessMode != AccessModeFull {
		t.Fatalf("RedeemPairing() AccessMode = %q, want %q", bundle.AccessMode, AccessModeFull)
	}
	assertExpiryNear(t, "access token", bundle.AccessTokenExpiresAt, startedAt, completedAt, defaultAccessTokenTTL)
	assertExpiryNear(t, "refresh token", bundle.RefreshTokenExpiresAt, startedAt, completedAt, defaultRefreshTokenTTL)

	if _, err := service.ActivePairingSession(pairingSession.PairingCode); !errors.Is(err, store.ErrPairingSessionNotFound) {
		t.Fatalf("ActivePairingSession(redeemed) error = %v, want %v", err, store.ErrPairingSessionNotFound)
	}
	if _, err := service.RedeemPairing(pairingSession.PairingCode, "Other iPhone", "http://127.0.0.1:8082"); err == nil {
		t.Fatal("RedeemPairing(reused code) error = nil, want one-time code rejection")
	}
	if got := service.PairingStatus(); got != PairingStatusReadyForAdditionalLocalPairing {
		t.Fatalf("PairingStatus() after first device = %q, want %q", got, PairingStatusReadyForAdditionalLocalPairing)
	}
	if _, err := service.CreatePairingSession(); err != nil {
		t.Fatalf("CreatePairingSession() for additional device error = %v", err)
	}
	if got := service.PairingStatus(); got != PairingStatusPairingSessionAvailable {
		t.Fatalf("PairingStatus() with active session = %q, want %q", got, PairingStatusPairingSessionAvailable)
	}
}

func TestActivePairingSessionRejectsMissingAndExpiredCodes(t *testing.T) {
	t.Parallel()

	service, registry := newTestService(t, 5)
	for _, code := range []string{"", "   ", "missing"} {
		if _, err := service.ActivePairingSession(code); !errors.Is(err, store.ErrPairingSessionNotFound) {
			t.Fatalf("ActivePairingSession(%q) error = %v, want %v", code, err, store.ErrPairingSessionNotFound)
		}
	}

	now := time.Now().UTC()
	expired, err := registry.CreatePairingSession(now.Add(-2*time.Minute), now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("CreatePairingSession(expired) error = %v", err)
	}
	if _, err := service.ActivePairingSession(expired.Code); !errors.Is(err, store.ErrPairingSessionNotFound) {
		t.Fatalf("ActivePairingSession(expired) error = %v, want %v", err, store.ErrPairingSessionNotFound)
	}
}

func TestHostedSessionRefreshAndAuthenticationLifecycle(t *testing.T) {
	t.Parallel()

	service, registry := newTestService(t, 5)
	bundle, err := service.CreateHostedSession("  Remote iPhone  ", "  https://timich.example///  ")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	if bundle.BaseURL != "https://timich.example" {
		t.Fatalf("CreateHostedSession() BaseURL = %q, want normalized URL", bundle.BaseURL)
	}
	snapshot := registry.Snapshot()
	if len(snapshot.Devices) != 1 || snapshot.Devices[0].DeviceName != "Remote iPhone" {
		t.Fatalf("Snapshot().Devices = %+v, want trimmed hosted device", snapshot.Devices)
	}

	claims, err := service.AuthenticateAccessToken("  " + bundle.AccessToken + "  ")
	if err != nil {
		t.Fatalf("AuthenticateAccessToken() error = %v", err)
	}
	if claims.AgentID != bundle.AgentID || claims.AppDeviceID != bundle.DeviceID ||
		claims.Audience != security.AudienceTimichAgent || claims.Scope != security.ScopeMedia {
		t.Fatalf("AuthenticateAccessToken() claims = %+v, want bundle device and Timich Agent media claims", claims)
	}
	if _, err := service.AuthenticateAccessToken(bundle.AccessToken + "tampered"); !errors.Is(err, security.ErrAccessTokenInvalid) {
		t.Fatalf("AuthenticateAccessToken(tampered) error = %v, want %v", err, security.ErrAccessTokenInvalid)
	}

	refreshed, err := service.RefreshSession("  "+bundle.RefreshToken+"  ", "  https://timich.example/v1///  ")
	if err != nil {
		t.Fatalf("RefreshSession() error = %v", err)
	}
	if refreshed.DeviceID != bundle.DeviceID {
		t.Fatalf("RefreshSession() DeviceID = %q, want %q", refreshed.DeviceID, bundle.DeviceID)
	}
	if refreshed.RefreshToken == bundle.RefreshToken || refreshed.AccessToken == bundle.AccessToken {
		t.Fatal("RefreshSession() did not rotate both credentials")
	}
	if refreshed.BaseURL != "https://timich.example/v1" {
		t.Fatalf("RefreshSession() BaseURL = %q, want normalized URL", refreshed.BaseURL)
	}
	recovered, err := service.RefreshSession(bundle.RefreshToken, bundle.BaseURL)
	if err != nil {
		t.Fatalf("RefreshSession(lost-response retry) error = %v", err)
	}
	if recovered.RefreshToken != refreshed.RefreshToken || recovered.DeviceID != refreshed.DeviceID {
		t.Fatalf("RefreshSession(lost-response retry) = device %q token %q, want same replacement", recovered.DeviceID, recovered.RefreshToken)
	}
	advanced, err := service.RefreshSession(refreshed.RefreshToken, refreshed.BaseURL)
	if err != nil {
		t.Fatalf("RefreshSession(current token) error = %v", err)
	}
	if advanced.RefreshToken == refreshed.RefreshToken {
		t.Fatal("RefreshSession(current token) did not advance rotation")
	}
	if _, err := service.RefreshSession(bundle.RefreshToken, bundle.BaseURL); !errors.Is(err, store.ErrRefreshTokenNotFound) {
		t.Fatalf("RefreshSession(two generations old token) error = %v, want %v", err, store.ErrRefreshTokenNotFound)
	}
	if _, err := service.AuthenticateAccessToken(refreshed.AccessToken); err != nil {
		t.Fatalf("AuthenticateAccessToken(refreshed) error = %v", err)
	}

	if err := registry.RevokeDevice(bundle.DeviceID); err != nil {
		t.Fatalf("RevokeDevice() error = %v", err)
	}
	if _, err := service.AuthenticateAccessToken(refreshed.AccessToken); !errors.Is(err, security.ErrAccessTokenInvalid) {
		t.Fatalf("AuthenticateAccessToken(revoked device) error = %v, want %v", err, security.ErrAccessTokenInvalid)
	}
	if _, err := service.RefreshSession(advanced.RefreshToken, advanced.BaseURL); !errors.Is(err, store.ErrRefreshTokenNotFound) {
		t.Fatalf("RefreshSession(revoked device) error = %v, want %v", err, store.ErrRefreshTokenNotFound)
	}
}

func TestNewServiceRejectsInvalidSigningConfiguration(t *testing.T) {
	t.Parallel()

	registry, err := store.LoadOrCreateDeviceRegistry(t.TempDir(), 5)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceRegistry() error = %v", err)
	}
	for _, test := range []struct {
		name    string
		agentID string
		key     string
	}{
		{name: "empty agent id", key: base64.RawStdEncoding.EncodeToString([]byte("test-signing-key"))},
		{name: "invalid base64 key", agentID: "agent-home", key: "%%%"},
		{name: "empty decoded key", agentID: "agent-home", key: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewService(test.agentID, "Home NAS", test.key, registry); err == nil {
				t.Fatal("NewService() error = nil, want invalid signing configuration rejection")
			}
		})
	}
}

func TestNearbyLinkPollCreatesSessionAfterApproval(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t, 5)

	link, err := service.CreateNearbyLink("Living Room TV", "android_tv")
	if err != nil {
		t.Fatalf("CreateNearbyLink() error = %v", err)
	}
	if link.LinkCode == "" || link.PollToken == "" {
		t.Fatal("CreateNearbyLink() returned an empty link code or poll token")
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
		t.Fatal("approved session returned an empty device ID or refresh token")
	}
	if approved.Session.AccessMode != AccessModeFull {
		t.Fatalf("AccessMode = %q, want %q", approved.Session.AccessMode, AccessModeFull)
	}
	if _, err := service.PollNearbyLink(link.LinkID, link.PollToken, "http://127.0.0.1:8082"); !errors.Is(err, store.ErrNearbyLinkNotFound) {
		t.Fatalf("PollNearbyLink(consumed) error = %v, want %v", err, store.ErrNearbyLinkNotFound)
	}
}

func TestNearbyLinkCancelPreventsApproval(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t, 5)

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

func TestNearbyLinkListingDenyAndInvalidPollToken(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t, 5)
	link, err := service.CreateNearbyLink("  Living Room TV  ", "  unsupported  ")
	if err != nil {
		t.Fatalf("CreateNearbyLink() error = %v", err)
	}
	if link.DeviceName != "Living Room TV" || link.DeviceKind != "unknown" {
		t.Fatalf("CreateNearbyLink() device = %q/%q, want trimmed name and unknown kind", link.DeviceName, link.DeviceKind)
	}

	links, err := service.NearbyLinks()
	if err != nil {
		t.Fatalf("NearbyLinks() error = %v", err)
	}
	if len(links) != 1 || links[0].LinkID != link.LinkID || links[0].PollToken != "" {
		t.Fatalf("NearbyLinks() = %+v, want one link without secret poll token", links)
	}
	if _, err := service.CancelNearbyLink(link.LinkID, "wrong-token"); !errors.Is(err, store.ErrNearbyLinkPollTokenInvalid) {
		t.Fatalf("CancelNearbyLink(wrong token) error = %v, want %v", err, store.ErrNearbyLinkPollTokenInvalid)
	}

	denied, err := service.DenyNearbyLink(link.LinkID)
	if err != nil {
		t.Fatalf("DenyNearbyLink() error = %v", err)
	}
	if denied.Status != store.NearbyLinkStatusDenied || denied.PollToken != "" {
		t.Fatalf("DenyNearbyLink() = %+v, want denied response without poll token", denied)
	}
	poll, err := service.PollNearbyLink(link.LinkID, link.PollToken, "http://127.0.0.1:8082")
	if err != nil {
		t.Fatalf("PollNearbyLink(denied) error = %v", err)
	}
	if poll.Status != store.NearbyLinkStatusDenied || poll.Session != nil {
		t.Fatalf("PollNearbyLink(denied) = %+v, want denied without session", poll)
	}
	if _, err := service.ApproveNearbyLink(link.LinkCode); !errors.Is(err, store.ErrNearbyLinkDenied) {
		t.Fatalf("ApproveNearbyLink(denied) error = %v, want %v", err, store.ErrNearbyLinkDenied)
	}
	if _, err := service.DenyNearbyLink("missing"); !errors.Is(err, store.ErrNearbyLinkNotFound) {
		t.Fatalf("DenyNearbyLink(missing) error = %v, want %v", err, store.ErrNearbyLinkNotFound)
	}
}

func TestNearbyLinkApprovalDoesNotBypassDeviceLimit(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t, 1)
	link, err := service.CreateNearbyLink("Living Room TV", "android_tv")
	if err != nil {
		t.Fatalf("CreateNearbyLink() error = %v", err)
	}
	if _, err := service.ApproveNearbyLink(link.LinkCode); err != nil {
		t.Fatalf("ApproveNearbyLink() error = %v", err)
	}
	if _, err := service.CreateHostedSession("Existing device", "https://timich.example"); err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}

	if _, err := service.PollNearbyLink(link.LinkID, link.PollToken, "http://127.0.0.1:8082"); !errors.Is(err, store.ErrDeviceLimitReached) {
		t.Fatalf("PollNearbyLink() error = %v, want %v", err, store.ErrDeviceLimitReached)
	}
	links, err := service.NearbyLinks()
	if err != nil {
		t.Fatalf("NearbyLinks() error = %v", err)
	}
	if len(links) != 1 || links[0].Status != store.NearbyLinkStatusApproved {
		t.Fatalf("NearbyLinks() = %+v, want the approved link to remain available", links)
	}
}

func TestDeviceLimitErrorsPropagateThroughPairingService(t *testing.T) {
	t.Parallel()

	service, _ := newTestService(t, 1)
	if _, err := service.CreateHostedSession("First device", "https://timich.example"); err != nil {
		t.Fatalf("CreateHostedSession(first device) error = %v", err)
	}
	for _, test := range []struct {
		name string
		run  func() error
	}{
		{
			name: "pairing session",
			run: func() error {
				_, err := service.CreatePairingSession()
				return err
			},
		},
		{
			name: "nearby link",
			run: func() error {
				_, err := service.CreateNearbyLink("Second device", "ios")
				return err
			},
		},
		{
			name: "hosted session",
			run: func() error {
				_, err := service.CreateHostedSession("Second device", "https://timich.example")
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, store.ErrDeviceLimitReached) {
				t.Fatalf("operation error = %v, want %v", err, store.ErrDeviceLimitReached)
			}
		})
	}
}

func TestIsClientError(t *testing.T) {
	t.Parallel()

	clientErrors := []error{
		store.ErrPairingSessionNotFound,
		store.ErrPairingSessionExpired,
		store.ErrPairingSessionUsed,
		store.ErrDeviceLimitReached,
		store.ErrRefreshTokenNotFound,
		store.ErrRefreshTokenExpired,
		store.ErrNearbyLinkNotFound,
		store.ErrNearbyLinkDenied,
		store.ErrNearbyLinkNotApproved,
		store.ErrNearbyLinkConsumed,
		store.ErrNearbyLinkPollTokenInvalid,
		store.ErrNearbyLinkLimitReached,
	}
	for _, clientErr := range clientErrors {
		clientErr := clientErr
		t.Run(clientErr.Error(), func(t *testing.T) {
			if !IsClientError(fmt.Errorf("wrapped: %w", clientErr)) {
				t.Fatalf("IsClientError(%v) = false, want true", clientErr)
			}
		})
	}
	for _, serverErr := range []error{nil, errors.New("internal failure"), store.ErrDeviceNotFound, store.ErrDeviceNameInvalid} {
		if IsClientError(serverErr) {
			t.Fatalf("IsClientError(%v) = true, want false", serverErr)
		}
	}
}

func newTestService(t *testing.T, deviceLimit int) (*Service, *store.DeviceRegistryStore) {
	t.Helper()

	registry, err := store.LoadOrCreateDeviceRegistry(t.TempDir(), deviceLimit)
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
	return service, registry
}

func assertExpiryNear(
	t *testing.T,
	name string,
	got time.Time,
	startedAt time.Time,
	completedAt time.Time,
	ttl time.Duration,
) {
	t.Helper()

	lowerBound := startedAt.Add(ttl - time.Second)
	upperBound := completedAt.Add(ttl)
	if got.Before(lowerBound) || got.After(upperBound) {
		t.Fatalf("%s expiry = %s, want between %s and %s", name, got, lowerBound, upperBound)
	}
}
