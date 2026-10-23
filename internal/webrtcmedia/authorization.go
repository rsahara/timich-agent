package webrtcmedia

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	contracts "github.com/rsahara/timich-agent/packages/contracts/controlplane"
)

var ErrAuthorization = errors.New("WebRTC authorization is no longer valid")

// Authorization is supplied only by the authenticated HTTP/relay boundary.
type Authorization struct {
	AgentID, DeviceID, KeyID, Authority string
	IssuedAt                            int64
	ExpiresAt                           time.Time
}

type hostedRouteKey struct{}

func WithHostedAuthority(ctx context.Context, authority string) context.Context {
	return context.WithValue(ctx, hostedRouteKey{}, authority)
}
func HostedAuthority(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(hostedRouteKey{}).(string)
	return v, ok
}

// A nil error and zero age is a definitive denial. Errors never renew a lease.
type LeaseChecker func(context.Context, string, string, bool) (time.Duration, error)

type leaseScope struct {
	key, authority, device string
	ctx                    context.Context
	cancel                 context.CancelFunc
	ready                  chan struct{}
	readyOnce              sync.Once
	deadline               atomic.Pointer[time.Time]
	used                   atomic.Bool
	refs                   int         // manager.mu
	timer                  *time.Timer // manager.mu
}

func (m *Manager) SetLeaseChecker(check LeaseChecker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkLease = check
}

// Prepare registers PENDING before SDP work. The caller holds its device gate
// only for this operation and its registry check, never for network I/O.
func (m *Manager) Prepare(auth Authorization) (string, error) {
	if auth.AgentID == "" || auth.DeviceID == "" || auth.KeyID == "" || !time.Now().Before(auth.ExpiresAt) {
		return "", ErrAuthorization
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.canceled {
		return "", context.Canceled
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{id: "rtc-" + uuid.NewString(), requests: make(map[string]*activeMediaRequest), auth: auth, ctx: ctx, cancel: cancel}
	m.sessions[s.id] = s
	if auth.Authority != "" {
		key := auth.Authority + "\x00" + auth.AgentID + "\x00" + auth.DeviceID
		scope := m.leases[key]
		if scope == nil {
			leaseCtx, leaseCancel := context.WithCancel(context.Background())
			scope = &leaseScope{key: key, authority: auth.Authority, device: auth.DeviceID, ctx: leaseCtx, cancel: leaseCancel, ready: make(chan struct{})}
			m.leases[key] = scope
			m.cleanup.Go(func(context.Context) { m.renewLease(scope) })
		}
		scope.refs++
		s.lease = scope
	}
	s.tokenTimer = time.AfterFunc(time.Until(auth.ExpiresAt), func() { m.closeSession(s.id, true) })
	return s.id, nil
}

func (m *Manager) Reauthorize(id string, auth Authorization) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		return ErrAuthorization
	}
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	if !s.authorizedLocked() || !s.active || auth.AgentID != s.auth.AgentID || auth.DeviceID != s.auth.DeviceID || auth.KeyID != s.auth.KeyID || auth.Authority != s.auth.Authority || !time.Now().Before(auth.ExpiresAt) {
		return ErrAuthorization
	}
	// Repeating the same or an older valid update is harmless; it cannot shorten
	// the current token lifetime or revive a closed/expired session.
	if auth.IssuedAt < s.auth.IssuedAt || !auth.ExpiresAt.After(s.auth.ExpiresAt) {
		return nil
	}
	s.auth = auth
	s.tokenTimer.Stop()
	s.tokenTimer = time.AfterFunc(time.Until(auth.ExpiresAt), func() { m.closeSession(id, true) })
	return nil
}

func (s *session) authorizedLocked() bool {
	now := time.Now()
	if s.closed || !now.Before(s.auth.ExpiresAt) {
		return false
	}
	if s.lease != nil {
		deadline := s.lease.deadline.Load()
		return s.lease.ctx.Err() == nil && deadline != nil && leaseDeadlineValid(*deadline, now)
	}
	return true
}

// Check both clocks: monotonic time bounds a lease across wall-clock rollback,
// while wall time catches suspend/resume on systems whose monotonic clock stops.
func leaseDeadlineValid(deadline, now time.Time) bool {
	return now.Before(deadline) && now.UnixNano() < deadline.UnixNano()
}

func (s *session) authorized() bool {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	return s.active && s.authorizedLocked()
}

func (m *Manager) RevokeDevice(device string) {
	m.mu.Lock()
	ids := make([]string, 0)
	for id, s := range m.sessions {
		if s.auth.DeviceID == device {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.closeSession(id, false)
	}
}

func (m *Manager) closeSession(id string, expiredOnly bool) {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return
	}
	s.requestMu.Lock()
	if expiredOnly && s.authorizedLocked() {
		s.requestMu.Unlock()
		m.mu.Unlock()
		return
	}
	s.closed = true
	peer := s.peer
	var releasePeer func()
	if peer != nil {
		_, releasePeer, _ = m.cleanup.Acquire()
	}
	if s.tokenTimer != nil {
		s.tokenTimer.Stop()
	}
	s.requestMu.Unlock()
	delete(m.sessions, id)
	if scope := s.lease; scope != nil {
		scope.refs--
		if scope.refs == 0 {
			if m.leases[scope.key] == scope {
				delete(m.leases, scope.key)
			}
			scope.cancel()
			if scope.timer != nil {
				scope.timer.Stop()
			}
		}
	}
	m.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	s.cancelAllMediaRequests("authorization_closed")
	if peer != nil {
		go func() {
			defer releasePeer()
			_ = peer.Close()
		}()
	}
}

func (m *Manager) expireLease(scope *leaseScope, force bool) {
	m.mu.Lock()
	deadline := scope.deadline.Load()
	if m.leases[scope.key] != scope || (!force && deadline != nil && leaseDeadlineValid(*deadline, time.Now())) {
		m.mu.Unlock()
		return
	}
	scope.cancel() // terminal before releasing the lock; late results cannot renew
	delete(m.leases, scope.key)
	ids := make([]string, 0)
	for id, s := range m.sessions {
		if s.lease == scope {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.closeSession(id, false)
	}
}

func (m *Manager) renewLease(scope *leaseScope) {
	defer scope.readyOnce.Do(func() { close(scope.ready) })
	for {
		if scope.ctx.Err() != nil {
			return
		}
		ctx, cancel := context.WithTimeout(scope.ctx, 5*time.Second)
		m.mu.Lock()
		check := m.checkLease
		m.mu.Unlock()
		started := time.Now()
		age, err := time.Duration(0), errors.New("Reach unavailable")
		select {
		case m.leaseSlots <- struct{}{}:
			if check != nil && ctx.Err() == nil {
				// Lease age starts at the RPC, not while waiting for capacity.
				started = time.Now()
				used := scope.used.Swap(false)
				age, err = check(ctx, scope.authority, scope.device, used)
				if used && err != nil {
					scope.used.Store(true)
				}
			}
			<-m.leaseSlots
		case <-ctx.Done(): // queueing and the RPC share the bounded timeout
		}
		cancel()
		if scope.ctx.Err() != nil {
			return
		}
		if err == nil && age <= 0 {
			m.expireLease(scope, true)
			return
		}
		if err == nil && age > 0 && age <= contracts.MediaAuthorizationMaxAge {
			m.applyLease(scope, started.Add(age))
		}
		scope.readyOnce.Do(func() { close(scope.ready) })
		deadline := scope.deadline.Load()
		if deadline == nil {
			m.expireLease(scope, true)
			return
		}
		delay := time.Duration(20+rand.IntN(11)) * time.Second
		// A subscription/Family expiry can grant less than the usual minute.
		// Recheck before that deadline so an extension can preserve playback.
		// Bound retries near an unchanged expiry; the expiry timer still wins.
		delay = min(delay, max(time.Second, time.Until(*deadline)/2))
		timer := time.NewTimer(delay)
		select {
		case <-scope.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (m *Manager) applyLease(scope *leaseScope, deadline time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	previous := scope.deadline.Load()
	// Even if the expiry timer has not run yet, an expired scope is terminal.
	if m.leases[scope.key] != scope || scope.ctx.Err() != nil || !leaseDeadlineValid(deadline, now) || (previous != nil && !leaseDeadlineValid(*previous, now)) {
		return
	}
	scope.deadline.Store(&deadline)
	if scope.timer != nil {
		scope.timer.Stop()
	}
	scope.timer = time.AfterFunc(time.Until(deadline), func() { m.expireLease(scope, false) })
}
