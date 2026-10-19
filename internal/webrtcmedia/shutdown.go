package webrtcmedia

import (
	"context"
	"errors"
)

func (m *Manager) BeginShutdown() {
	m.offers.CloseAdmission()
	m.work.CloseAdmission()
}

func (m *Manager) Wait(ctx context.Context) error {
	if err := m.offers.Wait(ctx); err != nil {
		return err
	}
	return m.work.Wait(ctx)
}

// Close cancels active requests and closes peers before waiting. Completion is
// tracked independently of the request map (cancel/replacement removes entries).
func (m *Manager) Cancel() error {
	m.cancelOnce.Do(func() { m.cancelErr = m.cancelPeers() })
	return m.cancelErr
}

func (m *Manager) PendingOffers() int { return m.offers.Pending() }

func (m *Manager) cancelPeers() error {
	m.BeginShutdown()
	m.work.Cancel()
	m.mu.Lock()
	m.canceled = true
	sessions := m.sessions
	m.sessions = make(map[string]*session)
	m.mu.Unlock()
	var err error
	for _, active := range sessions {
		active.cancelAllMediaRequests("shutdown")
		if active.peer != nil {
			err = errors.Join(err, active.peer.Close())
		}
	}
	return err
}

func (m *Manager) Close() error {
	err := m.Cancel()
	return errors.Join(err, m.Wait(context.Background()))
}
