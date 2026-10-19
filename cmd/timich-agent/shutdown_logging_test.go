package main

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type shutdownLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *shutdownLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *shutdownLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestShutdownPreservesDiagnosticsWhenLoggerIsHealthy(t *testing.T) {
	var output shutdownLogBuffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	failure := errors.New("listener failed")
	r := &shutdownTestRuntime{}
	c := newShutdownCoordinator(r, &http.Server{}, &http.Server{})
	err := c.shutdownWithCause("shutdown requested", failure)
	var stopped *shutdownError
	if !errors.Is(err, failure) || !errors.As(err, &stopped) {
		t.Fatalf("missing reported shutdown cause: %v", err)
	}
	if !r.closed.Load() {
		t.Fatal("resources not closed")
	}
	if got := output.String(); !strings.Contains(got, "shutdown requested") || !strings.Contains(got, failure.Error()) {
		t.Fatalf("missing shutdown diagnostics: %s", got)
	}
}

type shutdownBlockedDiagnosticsRuntime struct {
	shutdownTestRuntime
	entered, release chan struct{}
}

func (r *shutdownBlockedDiagnosticsRuntime) PendingShutdownWork() int {
	close(r.entered)
	<-r.release
	return 1
}

func TestShutdownDeadlineDoesNotWaitForDiagnosticCollection(t *testing.T) {
	r := &shutdownBlockedDiagnosticsRuntime{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(r.release)
	_, releaseWork, _ := r.work.Acquire()
	defer releaseWork()
	c := newShutdownCoordinator(r, &http.Server{}, &http.Server{})
	c.drainTimeout, c.totalTimeout = 10*time.Millisecond, 50*time.Millisecond
	var exits atomic.Int32
	c.forceExit = func(code int) {
		if code != 1 {
			t.Errorf("exit code=%d", code)
		}
		exits.Add(1)
	}
	done := make(chan error, 1)
	go func() { done <- c.shutdown() }()
	select {
	case err := <-done:
		if !errors.Is(err, errShutdownDeadline) || exits.Load() != 1 {
			t.Fatalf("deadline result=%v exits=%d", err, exits.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("blocked diagnostics prevented forced exit")
	}
	shutdownAwait(t, r.entered)
}

type shutdownDiagnosticWriter struct {
	once sync.Once
	done chan string
}

func (w *shutdownDiagnosticWriter) Write(p []byte) (int, error) {
	message := string(p)
	if strings.Contains(message, "total deadline exceeded") {
		w.once.Do(func() { w.done <- message })
	}
	return len(p), nil
}

func TestShutdownDeadlineAttemptsPhaseAndPendingDiagnostic(t *testing.T) {
	w := &shutdownDiagnosticWriter{done: make(chan string, 1)}
	previous := log.Writer()
	log.SetOutput(w)
	defer log.SetOutput(previous)
	r := &shutdownTestRuntime{}
	_, release, _ := r.work.Acquire()
	defer release()
	c := newShutdownCoordinator(r, &http.Server{}, &http.Server{})
	c.drainTimeout, c.totalTimeout = 10*time.Millisecond, 50*time.Millisecond
	c.forceExit = func(int) {}
	if err := c.shutdown(); !errors.Is(err, errShutdownDeadline) {
		t.Fatal(err)
	}
	select {
	case message := <-w.done:
		if !strings.Contains(message, "phase=") || !strings.Contains(message, "pending=1") {
			t.Fatalf("missing diagnostic context: %s", message)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline diagnostic was not attempted")
	}
}
