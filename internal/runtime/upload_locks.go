package runtime

import (
	"strings"
	"sync"
)

// uploadLockRegistry keeps one lock per normalized identity while any holder or
// waiter references it. The registry mutex is held only for map bookkeeping.
type uploadLockRegistry struct {
	mu      sync.Mutex
	entries map[string]*uploadLockEntry
}

type uploadLockEntry struct {
	mu   sync.RWMutex
	refs int
}

func (r *uploadLockRegistry) acquire(key string, shared, try bool) func() {
	key = strings.TrimSpace(key)
	r.mu.Lock()
	if r.entries == nil {
		r.entries = make(map[string]*uploadLockEntry)
	}
	entry := r.entries[key]
	if entry == nil {
		entry = &uploadLockEntry{}
		r.entries[key] = entry
	}
	entry.refs++ // Count waiters before attempting the per-identity lock.
	r.mu.Unlock()
	releaseRef := func() {
		r.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(r.entries, key)
		}
		r.mu.Unlock()
	}
	if try {
		var acquired bool
		if shared {
			acquired = entry.mu.TryRLock()
		} else {
			acquired = entry.mu.TryLock()
		}
		if !acquired {
			releaseRef()
			return nil
		}
	} else if shared {
		entry.mu.RLock()
	} else {
		entry.mu.Lock()
	}
	return func() {
		if shared {
			entry.mu.RUnlock()
		} else {
			entry.mu.Unlock()
		}
		releaseRef()
	}
}

func (r *uploadLockRegistry) lock(key string) func()        { return r.acquire(key, false, false) }
func (r *uploadLockRegistry) readLock(key string) func()    { return r.acquire(key, true, false) }
func (r *uploadLockRegistry) tryLock(key string) func()     { return r.acquire(key, false, true) }
func (r *uploadLockRegistry) tryReadLock(key string) func() { return r.acquire(key, true, true) }

// Every operation takes the device gate before the session lock. Reset and
// revocation exclusively take only the affected device's gate.
func (a *AgentRuntime) lockUploadSession(deviceID, uploadID string) func() {
	releaseDevice := a.uploadDevices.readLock(deviceID)
	releaseSession := a.uploadSessions.lock(uploadID)
	return func() { releaseSession(); releaseDevice() }
}

// Maintenance skips busy sessions and devices with a reset/revocation pending.
func (a *AgentRuntime) tryLockUploadSession(deviceID, uploadID string) func() {
	releaseDevice := a.uploadDevices.tryReadLock(deviceID)
	if releaseDevice == nil {
		return nil
	}
	releaseSession := a.uploadSessions.tryLock(uploadID)
	if releaseSession == nil {
		releaseDevice()
		return nil
	}
	return func() { releaseSession(); releaseDevice() }
}
