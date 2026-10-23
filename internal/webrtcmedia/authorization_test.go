package webrtcmedia

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/lifecycle"
)

func authorizedTestPeer(t *testing.T, m *Manager, device string, expires time.Time, authority string) *session {
	t.Helper()
	id, err := m.Prepare(Authorization{AgentID: "agent", DeviceID: device, KeyID: "key", ExpiresAt: expires, Authority: authority})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s.lease != nil {
		<-s.lease.ready
	}
	s.requestMu.Lock()
	s.active = true
	s.requestMu.Unlock()
	return s
}

func TestRevokeStopsBlockedBodyAndPreservesOtherDeviceAndHTTP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var loads atomic.Int32
		reader, writer := io.Pipe()
		defer writer.Close()
		entered := make(chan struct{})
		work := &lifecycle.Group{}
		m := NewManagerWithLifecycle(func(r *http.Request, _ string) (*catalog.UpstreamMediaResponse, error) {
			loads.Add(1)
			if r.Method == http.MethodGet {
				close(entered)
				return &catalog.UpstreamMediaResponse{StatusCode: 200, Header: http.Header{}, Body: reader}, nil
			}
			return &catalog.UpstreamMediaResponse{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
		}, work)
		defer m.Close()
		a := authorizedTestPeer(t, m, "A", time.Now().Add(time.Hour), "")
		b := authorizedTestPeer(t, m, "B", time.Now().Add(time.Hour), "")
		ctx, release, ok := work.Acquire()
		if !ok {
			t.Fatal("HTTP not admitted")
		}
		defer release()
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.handleMediaRequest(a, &fakeDataChannel{}, mediaRequest{RequestID: "a", AssetID: "photo"})
		}()
		<-entered
		synctest.Wait()
		m.RevokeDevice("A")
		synctest.Wait()
		<-done
		if ctx.Err() != nil {
			t.Fatal("revocation canceled unrelated HTTP")
		}
		m.handleMediaRequest(a, &fakeDataChannel{}, mediaRequest{RequestID: "late", AssetID: "photo"})
		m.handleMediaRequest(b, &fakeDataChannel{}, mediaRequest{RequestID: "b", AssetID: "photo", Method: http.MethodHead})
		if loads.Load() != 2 {
			t.Fatalf("loads=%d, revoked request reached original or B was stopped", loads.Load())
		}
		if err := m.Reauthorize(a.id, a.auth); !errors.Is(err, ErrAuthorization) {
			t.Fatal("closed session revived")
		}
	})
}

func TestExpiryAndReauthorizationKeepRequestsUntilNewDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil)
		defer m.Close()
		s := authorizedTestPeer(t, m, "A", time.Now().Add(10*time.Second), "")
		ctx, done := s.registerMediaRequest("video")
		defer done()
		updated := s.auth
		updated.IssuedAt = 1
		updated.ExpiresAt = time.Now().Add(time.Minute)
		wrong := updated
		wrong.DeviceID = "B"
		if m.Reauthorize(s.id, wrong) == nil {
			t.Fatal("cross-device update accepted")
		}
		wrong = updated
		wrong.Authority = "Reach"
		if m.Reauthorize(s.id, wrong) == nil {
			t.Fatal("cross-route update accepted")
		}
		if err := m.Reauthorize(s.id, updated); err != nil {
			t.Fatal(err)
		}
		if err := m.Reauthorize(s.id, updated); err != nil {
			t.Fatal("idempotent retry", err)
		}
		time.Sleep(11 * time.Second)
		synctest.Wait()
		if ctx.Err() != nil || !s.authorized() {
			t.Fatal("old timer interrupted valid update")
		}
		time.Sleep(50 * time.Second)
		synctest.Wait()
		if ctx.Err() == nil || s.authorized() {
			t.Fatal("token expiry not enforced")
		}
		if m.Reauthorize(s.id, updated) == nil {
			t.Fatal("expired session revived")
		}
	})
}

func TestHostedLeaseSharedAndOutageCannotExtendDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil)
		defer m.Close()
		var calls atomic.Int32
		m.SetLeaseChecker(func(context.Context, string, string, bool) (time.Duration, error) {
			if calls.Add(1) == 1 {
				return time.Minute, nil
			}
			return 0, errors.New("offline")
		})
		a := authorizedTestPeer(t, m, "A", time.Now().Add(time.Hour), "Reach")
		b := authorizedTestPeer(t, m, "A", time.Now().Add(45*time.Second), "Reach")
		if a.lease != b.lease || calls.Load() != 1 {
			t.Fatal("duplicated lease lookup")
		}
		time.Sleep(35 * time.Second)
		synctest.Wait()
		if !a.authorized() || calls.Load() < 2 {
			t.Fatal("outage caused immediate loss instead of using residual lease")
		}
		time.Sleep(11 * time.Second)
		synctest.Wait()
		if b.authorized() || !a.authorized() {
			t.Fatal("sharing lease extended another peer token")
		}
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if a.authorized() {
			t.Fatal("unavailable extended lease")
		}
		if managerSessionCount(m) != 0 {
			t.Fatal("expired sessions retained")
		}
		before := calls.Load()
		time.Sleep(time.Minute)
		synctest.Wait()
		if calls.Load() != before {
			t.Fatal("polling continued without peers")
		}
	})
}

func TestShortHostedLeaseRenewsBeforeExpiryWithoutInterruptingMedia(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil)
		defer m.Close()
		started := time.Now()
		var calls atomic.Int32
		m.SetLeaseChecker(func(context.Context, string, string, bool) (time.Duration, error) {
			calls.Add(1)
			// The subscription is extended on Reach while the initial short
			// lease is still valid; playback should adopt that extension.
			if time.Since(started) < 2*time.Second {
				return time.Until(started.Add(10 * time.Second)), nil
			}
			return time.Minute, nil
		})
		s := authorizedTestPeer(t, m, "A", started.Add(time.Hour), "Reach")
		ctx, done := s.registerMediaRequest("video")
		defer done()
		time.Sleep(11 * time.Second)
		synctest.Wait()
		if calls.Load() < 2 || !s.authorized() || ctx.Err() != nil {
			t.Fatalf("valid extension interrupted playback: calls=%d authorized=%v err=%v", calls.Load(), s.authorized(), ctx.Err())
		}
		// Once a full lease is available, use the ordinary renewal cadence.
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("short-lease polling continued after extension: calls=%d", calls.Load())
		}
	})
}

func TestShortHostedLeaseStillExpiresWithoutRapidPolling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil)
		defer m.Close()
		deadline := time.Now().Add(10 * time.Second)
		var calls atomic.Int32
		m.SetLeaseChecker(func(context.Context, string, string, bool) (time.Duration, error) {
			calls.Add(1)
			return time.Until(deadline), nil
		})
		s := authorizedTestPeer(t, m, "A", time.Now().Add(time.Hour), "Reach")
		ctx, done := s.registerMediaRequest("video")
		defer done()
		time.Sleep(11 * time.Second)
		synctest.Wait()
		if s.authorized() || ctx.Err() == nil || managerSessionCount(m) != 0 {
			t.Fatal("renewal extended the fixed entitlement deadline")
		}
		before := calls.Load()
		if before < 2 || before > 10 {
			t.Fatalf("expected bounded renewal attempts before expiry: calls=%d", before)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if calls.Load() != before {
			t.Fatal("polling continued after short lease expired")
		}
	})
}

func TestInitialHostedLeaseWaitsForBusyRPCSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil)
		defer m.Close()
		entered := make(chan struct{}, 5)
		release := make(chan struct{})
		var active, peak atomic.Int32
		m.SetLeaseChecker(func(ctx context.Context, _, device string, _ bool) (time.Duration, error) {
			count := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); count > old; old = peak.Load() {
				if peak.CompareAndSwap(old, count) {
					break
				}
			}
			entered <- struct{}{}
			if device != "fifth" {
				select {
				case <-release:
				case <-ctx.Done():
					return 0, ctx.Err()
				}
			}
			return 2 * time.Second, nil
		})
		prepare := func(device string) *session {
			t.Helper()
			id, err := m.Prepare(Authorization{AgentID: "agent", DeviceID: device, KeyID: "key", Authority: "Reach", ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.sessions[id]
		}
		for i := range 4 {
			prepare(fmt.Sprint(i))
			<-entered
		}
		fifth := prepare("fifth")
		synctest.Wait()
		select {
		case <-fifth.lease.ready:
			t.Fatal("initial lease rejected before an RPC slot became available")
		default:
		}
		// Queue time must not consume the lease returned by a later RPC.
		time.Sleep(3 * time.Second)
		close(release)
		synctest.Wait()
		select {
		case <-fifth.lease.ready:
		default:
			t.Fatal("initial lease did not complete after a slot became available")
		}
		if peak.Load() > 4 {
			t.Fatalf("exceeded RPC concurrency limit: %d", peak.Load())
		}
		deadline := fifth.lease.deadline.Load()
		if deadline == nil || !leaseDeadlineValid(*deadline, time.Now()) || fifth.ctx.Err() != nil {
			t.Fatal("queued initial lease was not granted its valid lifetime")
		}
	})
}

func TestQueuedHostedLeaseStopsOnRevocationOrTimeout(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(fmt.Sprintf("revoke=%v", revoke), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := NewManager(nil)
				defer m.Close()
				entered := make(chan struct{}, 5)
				release := make(chan struct{})
				defer close(release) // release blocked checkers before closing the manager
				var fifthCalls atomic.Int32
				m.SetLeaseChecker(func(ctx context.Context, _, device string, _ bool) (time.Duration, error) {
					if device == "4" {
						fifthCalls.Add(1)
					}
					entered <- struct{}{}
					// Hold capacity independently of RPC deadlines so this test
					// isolates cancellation/timeout while the fifth scope is queued.
					<-release
					return 0, ctx.Err()
				})
				var fifth *session
				for i := range 5 {
					id, err := m.Prepare(Authorization{AgentID: "agent", DeviceID: fmt.Sprint(i), KeyID: "key", Authority: "Reach", ExpiresAt: time.Now().Add(time.Hour)})
					if err != nil {
						t.Fatal(err)
					}
					if i < 4 {
						<-entered
					} else {
						m.mu.Lock()
						fifth = m.sessions[id]
						m.mu.Unlock()
					}
				}
				synctest.Wait()
				if revoke {
					m.RevokeDevice("4")
				} else {
					time.Sleep(6 * time.Second)
				}
				synctest.Wait()
				if fifth.ctx.Err() == nil || fifthCalls.Load() != 0 {
					t.Fatalf("queued scope outlived revocation/timeout: err=%v RPC calls=%d", fifth.ctx.Err(), fifthCalls.Load())
				}
				select {
				case <-fifth.lease.ready:
				default:
					t.Fatal("queued worker did not exit")
				}
			})
		})
	}
}

func TestPendingRevocationRejectsLateLeaseAndMediaAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(nil)
		defer m.Close()
		entered, finish := make(chan struct{}), make(chan struct{})
		m.SetLeaseChecker(func(context.Context, string, string, bool) (time.Duration, error) {
			close(entered)
			<-finish
			return time.Minute, nil
		})
		id, err := m.Prepare(Authorization{AgentID: "agent", DeviceID: "A", KeyID: "key", ExpiresAt: time.Now().Add(time.Hour), Authority: "Reach"})
		if err != nil {
			t.Fatal(err)
		}
		<-entered
		m.mu.Lock()
		s := m.sessions[id]
		m.mu.Unlock()
		m.RevokeDevice("A")
		close(finish)
		synctest.Wait()
		ctx, done := s.registerMediaRequest("late")
		defer done()
		if ctx.Err() == nil || managerSessionCount(m) != 0 || s.authorized() {
			t.Fatal("late result revived revoked pending peer")
		}
	})
}

func BenchmarkLocalMediaAuthorization(b *testing.B) {
	s := newTestSession("benchmark")
	b.ReportAllocs()
	for b.Loop() {
		if !s.authorized() {
			b.Fatal("unexpected expiry")
		}
	}
}

func TestLateLeaseResponseCannotReviveExpiredDeadlineBeforeTimerRuns(t *testing.T) {
	m := NewManager(nil)
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	expired := time.Now().Add(-time.Second)
	scope := &leaseScope{key: "expired", ctx: ctx, cancel: cancel}
	scope.deadline.Store(&expired)
	m.leases[scope.key] = scope
	m.applyLease(scope, time.Now().Add(time.Minute))
	if scope.deadline.Load() != &expired {
		t.Fatal("late successful renewal revived an expired scope")
	}
}

func TestExpiredAuthorizationCannotSendBufferedMedia(t *testing.T) {
	s := newTestSession("expired")
	s.auth.ExpiresAt = time.Now().Add(-time.Second)
	channel := &fakeDataChannel{}
	s.sendJSON(channel, mediaComplete{Type: "complete"})
	if len(channel.snapshotEvents()) != 0 {
		t.Fatal("sent metadata after expiry")
	}
	if _, err := s.sendBinary(context.Background(), channel, []byte("secret")); err == nil {
		t.Fatal("sent body after expiry")
	}
}
