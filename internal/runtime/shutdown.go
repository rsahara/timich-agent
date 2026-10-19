package runtime

import (
	"context"
	"errors"
	"net/http"
)

// RequestContext stays live during drain, independently of signal notification.
func (a *AgentRuntime) RequestContext() context.Context { return a.requests.Context() }
func (a *AgentRuntime) RequestHandler(next http.Handler) http.Handler {
	return a.requests.Handler(next)
}
func (a *AgentRuntime) AcquireRequest() (context.Context, func(), bool) { return a.requests.Acquire() }

// AcquireBackgroundWork registers work that outlives an API response. Acquire
// before launching or acknowledging it, and release only after all cleanup and
// result publication. Its context is canceled at shutdown start, not at HTTP return.
func (a *AgentRuntime) AcquireBackgroundWork() (context.Context, func(), bool) {
	return a.background.Acquire()
}

// BeginShutdown never waits for work to finish. Cancel every background owner
// before waiting for any one of them; admitted requests keep their grace period.
func (a *AgentRuntime) BeginShutdown() {
	a.requests.CloseAdmission()
	a.background.CloseAdmission()
	a.background.Cancel()
	if a.webrtc != nil {
		a.webrtc.BeginShutdown()
	}
	if a.semanticONNXRuntime != nil {
		a.semanticONNXRuntime.BeginShutdown()
	}
}

func (a *AgentRuntime) CancelRequests() error {
	a.requests.Cancel()
	if a.webrtc != nil {
		return a.webrtc.Cancel()
	}
	return nil
}
func (a *AgentRuntime) WaitRequests(ctx context.Context) error { return a.requests.Wait(ctx) }
func (a *AgentRuntime) PendingShutdownWork() int {
	count := a.requests.Pending() + a.background.Pending()
	if a.webrtc != nil {
		count += a.webrtc.PendingOffers()
	}
	if a.semanticONNXRuntime != nil {
		count += a.semanticONNXRuntime.callbacks.Pending() + a.semanticONNXRuntime.retryWork.Pending()
	}
	return count
}

func (a *AgentRuntime) WaitStopped(ctx context.Context) error {
	if err := a.requests.Wait(ctx); err != nil {
		return err
	}
	if err := a.background.Wait(ctx); err != nil {
		return err
	}
	if a.webrtc != nil {
		if err := a.webrtc.Wait(ctx); err != nil {
			return err
		}
	}
	if a.semanticONNXRuntime != nil {
		if err := a.semanticONNXRuntime.retryWork.Wait(ctx); err != nil {
			return err
		}
		return a.semanticONNXRuntime.callbacks.Wait(ctx)
	}
	return nil
}

// CloseResources must follow admission closure and completion of all callers,
// including an external control-plane Run. The CLI watchdog covers blocking I/O.
func (a *AgentRuntime) CloseResources(ctx context.Context) error {
	if err := a.WaitStopped(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.closeOnce.Do(func() {
		if a.webrtc != nil {
			a.closeErr = errors.Join(a.closeErr, a.webrtc.Close())
		}
		if a.semanticONNXRuntime != nil {
			if err := a.semanticONNXRuntime.CloseContext(ctx); err != nil {
				a.closeErr = errors.Join(a.closeErr, err)
				return
			}
		}
		if err := ctx.Err(); err != nil {
			a.closeErr = errors.Join(a.closeErr, err)
			return
		}
		if a.uploads != nil {
			a.closeErr = errors.Join(a.closeErr, a.uploads.Close())
		}
		if a.catalog != nil {
			a.closeErr = errors.Join(a.closeErr, a.catalog.Close())
		}
	})
	return a.closeErr
}
