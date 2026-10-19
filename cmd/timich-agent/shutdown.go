package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	shutdownDrainTimeout    = 5 * time.Second
	shutdownTotalTimeout    = 8 * time.Second
	shutdownLogFlushTimeout = 50 * time.Millisecond
)

var errShutdownDeadline = errors.New("agent shutdown exceeded total deadline")

// shutdownError has already had its diagnostic attempted within the shutdown
// budget. main must not try a synchronous log.Fatal after that budget ends.
type shutdownError struct{ err error }

func (e *shutdownError) Error() string { return e.err.Error() }
func (e *shutdownError) Unwrap() error { return e.err }

type shutdownRuntime interface {
	BeginShutdown()
	CancelRequests() error
	WaitRequests(context.Context) error
	WaitStopped(context.Context) error
	CloseResources(context.Context) error
	PendingShutdownWork() int
}

type shutdownServer struct {
	name   string
	server *http.Server
}

type shutdownCoordinator struct {
	runtime                    shutdownRuntime
	servers                    []shutdownServer
	stopControlPlane           func()
	controlPlaneDone           <-chan struct{}
	drainTimeout, totalTimeout time.Duration
	forceExit                  func(int)
	once                       sync.Once
	done                       chan struct{}
	err                        error
	phase                      atomic.Int32
}

func newShutdownCoordinator(runtime shutdownRuntime, admin, media *http.Server) *shutdownCoordinator {
	return &shutdownCoordinator{
		runtime:      runtime,
		servers:      []shutdownServer{{"admin", admin}, {"media", media}},
		drainTimeout: shutdownDrainTimeout, totalTimeout: shutdownTotalTimeout,
		forceExit: os.Exit, done: make(chan struct{}),
	}
}

// All causes share one clock and one result. The outer watchdog remains alive
// during cleanup, including non-context-aware database/file/child-process calls.
func (c *shutdownCoordinator) shutdown() error {
	return c.shutdownWithCause("", nil)
}

func (c *shutdownCoordinator) shutdownWithCause(reason string, cause error) error {
	c.once.Do(func() {
		defer close(c.done)
		started := time.Now()
		ctx, cancel := context.WithDeadline(context.Background(), started.Add(c.totalTimeout))
		defer cancel()
		var logs []<-chan struct{}
		if reason != "" {
			logs = append(logs, startShutdownLog(func() { log.Print(reason) }))
		}
		result := make(chan error, 1)
		go func() { result <- c.stop(ctx, started.Add(c.drainTimeout)) }()
		select {
		case err := <-result:
			if ctx.Err() == nil {
				c.err = errors.Join(cause, err)
				if c.err != nil {
					failure := c.err
					logs = append(logs, startShutdownLog(func() { log.Print(failure) }))
					c.err = &shutdownError{err: failure}
				}
				// Give a healthy sink a short chance to preserve diagnostics, but
				// never extend the original deadline or delay resource cleanup.
				flush, stopFlush := context.WithTimeout(ctx, shutdownLogFlushTimeout)
				waitShutdownLogs(flush, logs)
				stopFlush()
				// Cleanup already finished on time. Missing diagnostics must
				// neither change its result nor replace an existing failure.
				return
			}
		case <-ctx.Done():
		}
		c.err = &shutdownError{err: errors.Join(cause, errShutdownDeadline)}
		// Even collecting diagnostics can block. Keep both logging and runtime
		// inspection off the path between the timer and exit.
		startShutdownLog(func() {
			phases := [...]string{"starting", "admission", "drain", "cancel", "control-plane", "owners", "resources"}
			log.Printf("timich-agent shutdown failed: total deadline exceeded phase=%s pending=%d; forcing process exit", phases[c.phase.Load()], c.runtime.PendingShutdownWork())
		})
		c.forceExit(1)
	})
	<-c.done
	return c.err
}

func startShutdownLog(write func()) <-chan struct{} {
	done := make(chan struct{})
	go func() { defer close(done); write() }()
	return done
}

func waitShutdownLogs(ctx context.Context, logs []<-chan struct{}) {
	for _, done := range logs {
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
}

func (c *shutdownCoordinator) stop(ctx context.Context, drainDeadline time.Time) error {
	c.phase.Store(1)
	c.runtime.BeginShutdown()
	c.phase.Store(2)
	drain, cancelDrain := context.WithDeadline(ctx, drainDeadline)
	defer cancelDrain()
	results := make(chan error, len(c.servers))
	for _, entry := range c.servers {
		go func() {
			err := entry.server.Shutdown(drain)
			if err != nil {
				err = fmt.Errorf("%s shutdown: %w", entry.name, err)
			}
			results <- err
		}()
	}
	// Includes admitted relay/WebRTC requests as well as HTTP handlers. The
	// control stream itself is stopped after drain, then joined before cleanup.
	requestErr := c.runtime.WaitRequests(drain)
	var result error
	for range c.servers {
		result = errors.Join(result, <-results)
	}
	if requestErr != nil || result != nil {
		c.phase.Store(3)
		result = errors.Join(result, requestErr)
		forced := make(chan error, len(c.servers)+1)
		go func() { forced <- c.runtime.CancelRequests() }()
		for _, entry := range c.servers {
			go func() {
				err := entry.server.Close()
				if err != nil {
					err = fmt.Errorf("%s close: %w", entry.name, err)
				}
				forced <- err
			}()
		}
		for range len(c.servers) + 1 {
			select {
			case err := <-forced:
				result = errors.Join(result, err)
			case <-ctx.Done():
				return errors.Join(result, ctx.Err())
			}
		}
	}
	c.phase.Store(4)
	if c.stopControlPlane != nil {
		c.stopControlPlane()
	}
	if c.controlPlaneDone != nil {
		select {
		case <-c.controlPlaneDone:
		case <-ctx.Done():
			return errors.Join(result, ctx.Err())
		}
	}
	c.phase.Store(5)
	if err := c.runtime.WaitStopped(ctx); err != nil {
		return errors.Join(result, err)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(result, err)
	}
	c.phase.Store(6)
	return errors.Join(result, c.runtime.CloseResources(ctx))
}
