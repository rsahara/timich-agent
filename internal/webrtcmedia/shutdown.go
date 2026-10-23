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
	if err := m.work.Wait(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	canceled := m.canceled
	m.mu.Unlock()
	if canceled {
		return m.cleanup.Wait(ctx)
	}
	return nil
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
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.closeSession(id, false)
	}
	m.cleanup.CloseAdmission()
	return nil
}

func (m *Manager) Close() error {
	err := m.Cancel()
	return errors.Join(err, m.Wait(context.Background()))
}
