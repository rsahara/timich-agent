package runtime

import (
	"context"
	"errors"
	"os"
	"time"
)

// The atomic flag prevents timers waiting for the manager lock from restarting
// a child. Closing callback admission is non-blocking even during reconciliation.
func (m *semanticONNXRuntimeManager) BeginShutdown() {
	m.stopping.Store(true)
	m.callbacks.CloseAdmission()
	m.retryWork.CloseAdmission()
	m.retryWork.Cancel()
}

func (m *semanticONNXRuntimeManager) CloseContext(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.BeginShutdown()
	if err := m.retryWork.Wait(ctx); err != nil {
		return err
	}
	if err := m.callbacks.Wait(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.closed = true
	m.cancelAllRetriesLocked()
	processes := m.processes
	m.processes = make(map[string]*managedSemanticONNXRuntimeProcess)
	for _, process := range processes {
		process.uninstallEnvLocked()
	}
	m.mu.Unlock()

	// Signal all children first; they share one grace period and one final deadline.
	var pending []*managedSemanticONNXRuntimeProcess
	var result error
	for _, process := range processes {
		if process.cmd == nil || process.cmd.Process == nil {
			continue
		}
		select {
		case <-process.done:
			continue
		default:
		}
		pending = append(pending, process)
		if err := process.cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
			// Windows does not support Interrupt; kill is the supported fallback.
			if killErr := process.cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				result = errors.Join(result, killErr)
			}
		}
	}
	grace := defaultSemanticONNXRuntimeStopWait
	if deadline, ok := ctx.Deadline(); ok {
		grace = min(grace, max(time.Until(deadline)/2, 0))
	}
	soft, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	for _, process := range pending {
		select {
		case <-process.done:
		case <-soft.Done():
		}
	}
	for _, process := range pending {
		select {
		case <-process.done:
			continue
		default:
		}
		if err := process.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			result = errors.Join(result, err)
		}
	}
	for _, process := range pending {
		select {
		case <-process.done:
		case <-ctx.Done():
			return errors.Join(result, ctx.Err())
		}
	}
	return result
}
