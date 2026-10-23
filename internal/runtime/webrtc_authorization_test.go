package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/webrtcmedia"
)

func TestRevokeDeviceRejectsPendingAndPreviouslyAuthenticatedWebRTCOffer(t *testing.T) {
	a := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{{
		Name: "Static demo", Kind: config.DatasourceKindStaticDemo, URL: writeStaticDemoManifest(t),
	}})
	pairing, err := a.CreatePairingSession()
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := a.RedeemPairing(pairing.PairingCode, "iPhone", "http://127.0.0.1:8082")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := a.AuthenticateAccessToken(bundle.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	checking := make(chan struct{})
	a.SetWebRTCLeaseChecker(func(ctx context.Context, _, _ string, _ bool) (time.Duration, error) {
		close(checking)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = webrtcmedia.WithHostedAuthority(ctx, "configured-Reach")
	offer := webrtcmedia.OfferRequest{SDP: "pending authorization precedes SDP parsing"}
	finished := make(chan error, 1)
	go func() {
		_, err := a.AnswerWebRTCOffer(ctx, offer, claims)
		finished <- err
	}()
	select {
	case <-checking:
	case <-ctx.Done():
		t.Fatal("offer did not reach initial authorization")
	}
	if err := a.RevokeDevice(bundle.DeviceID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("revoked pending offer succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("revocation did not stop pending offer")
	}
	if a.requests.Context().Err() != nil {
		t.Fatal("device revocation canceled unrelated HTTP work")
	}
	if _, err := a.AnswerWebRTCOffer(ctx, offer, claims); !errors.Is(err, webrtcmedia.ErrAuthorization) {
		t.Fatalf("claims authenticated before revoke bypassed registry gate: %v", err)
	}
}
