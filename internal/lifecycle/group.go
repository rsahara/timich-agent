// Package lifecycle coordinates admission and the actual completion of owned work.
package lifecycle

import (
	"context"
	"net/http"
	"sync"
)

// Group's zero value accepts work. Closing admission and registering work use the
// same lock, so Wait cannot miss an acquisition racing with shutdown. Cancellation
// is deliberately separate: canceled work still owns its resources until release.
type Group struct {
	mu     sync.Mutex
	closed bool
	active int
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
}

func (g *Group) initLocked() {
	if g.done == nil {
		g.done = make(chan struct{})
		g.ctx, g.cancel = context.WithCancel(context.Background())
	}
}

func (g *Group) Context() context.Context {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	return g.ctx
}

func (g *Group) Acquire() (context.Context, func(), bool) {
	g.mu.Lock()
	g.initLocked()
	if g.closed {
		g.mu.Unlock()
		return nil, nil, false
	}
	g.active++
	ctx := g.ctx
	g.mu.Unlock()
	return ctx, sync.OnceFunc(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.active--
		if g.closed && g.active == 0 {
			close(g.done)
		}
	}), true
}

func (g *Group) CloseAdmission() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	if !g.closed {
		g.closed = true
		if g.active == 0 {
			close(g.done)
		}
	}
}

func (g *Group) Cancel() {
	g.mu.Lock()
	g.initLocked()
	cancel := g.cancel
	g.mu.Unlock()
	cancel()
}

func (g *Group) Wait(ctx context.Context) error {
	g.mu.Lock()
	g.initLocked()
	done := g.done
	g.mu.Unlock()
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *Group) Pending() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
}

// Go registers before launching, including time spent waiting to be scheduled.
func (g *Group) Go(fn func(context.Context)) bool {
	ctx, release, ok := g.Acquire()
	if !ok {
		return false
	}
	go func() { defer release(); fn(ctx) }()
	return true
}

func (g *Group) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, release, ok := g.Acquire()
		if !ok {
			http.Error(w, "agent is shutting down", http.StatusServiceUnavailable)
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}
