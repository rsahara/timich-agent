package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/lifecycle"
)

type shutdownTestRuntime struct {
	work      lifecycle.Group
	started   chan struct{}
	startOnce sync.Once
	closed    atomic.Bool
	closeHook func(context.Context) error
}

func (r *shutdownTestRuntime) BeginShutdown() {
	r.work.CloseAdmission()
	r.startOnce.Do(func() {
		if r.started != nil {
			close(r.started)
		}
	})
}
func (r *shutdownTestRuntime) CancelRequests() error                  { r.work.Cancel(); return nil }
func (r *shutdownTestRuntime) WaitRequests(ctx context.Context) error { return r.work.Wait(ctx) }
func (r *shutdownTestRuntime) WaitStopped(ctx context.Context) error  { return r.work.Wait(ctx) }
func (r *shutdownTestRuntime) PendingShutdownWork() int               { return r.work.Pending() }
func (r *shutdownTestRuntime) CloseResources(ctx context.Context) error {
	if r.work.Pending() != 0 {
		return errors.New("closed with active work")
	}
	if r.closeHook != nil {
		if err := r.closeHook(ctx); err != nil {
			return err
		}
	}
	r.closed.Store(true)
	return nil
}
func shutdownTestServer(t *testing.T, r *shutdownTestRuntime, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewUnstartedServer(r.work.Handler(handler))
	s.Config.BaseContext = func(net.Listener) context.Context { return r.work.Context() }
	s.Start()
	t.Cleanup(s.Close)
	return s
}
func shutdownAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown event not observed")
	}
}

func TestShutdownParallelDrainAndCleanupOrdering(t *testing.T) {
	r := &shutdownTestRuntime{started: make(chan struct{})}
	adminEntered, mediaEntered, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseAdmin, releaseMedia := make(chan struct{}), make(chan struct{})
	finishAdmin := sync.OnceFunc(func() { close(releaseAdmin) })
	finishMedia := sync.OnceFunc(func() { close(releaseMedia) })
	admin := shutdownTestServer(t, r, func(w http.ResponseWriter, q *http.Request) {
		close(adminEntered)
		<-q.Context().Done()
		close(canceled)
		<-releaseAdmin
	})
	media := shutdownTestServer(t, r, func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/warm" {
			w.WriteHeader(204)
			return
		}
		if q.URL.Path == "/new" {
			t.Error("new request reached business handler")
			return
		}
		close(mediaEntered)
		<-releaseMedia
		w.WriteHeader(204)
	})
	t.Cleanup(finishAdmin)
	t.Cleanup(finishMedia)
	keepAlive := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 2}, Timeout: 3 * time.Second}
	defer keepAlive.CloseIdleConnections()
	response, err := keepAlive.Get(media.URL + "/warm")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	adminDone := make(chan struct{})
	go func() {
		defer close(adminDone)
		response, _ := http.Get(admin.URL)
		if response != nil {
			response.Body.Close()
		}
	}()
	mediaDone := make(chan struct{})
	go func() {
		defer close(mediaDone)
		response, err := http.Get(media.URL + "/active")
		if err != nil {
			t.Error(err)
			return
		}
		response.Body.Close()
		if response.StatusCode != 204 {
			t.Errorf("media status=%d", response.StatusCode)
		}
	}()
	shutdownAwait(t, adminEntered)
	shutdownAwait(t, mediaEntered)
	c := newShutdownCoordinator(r, admin.Config, media.Config)
	c.drainTimeout = 500 * time.Millisecond
	c.totalTimeout = 4 * time.Second
	c.forceExit = func(int) { t.Error("unexpected watchdog exit") }
	done := make(chan error, 1)
	go func() { done <- c.shutdown() }()
	shutdownAwait(t, r.started)
	if r.work.Context().Err() != nil {
		t.Fatal("drain canceled requests immediately")
	}
	response, err = keepAlive.Get(media.URL + "/new")
	if err == nil {
		response.Body.Close()
		if response.StatusCode != 503 {
			t.Errorf("post-stop keep-alive status=%d", response.StatusCode)
		}
	}
	finishMedia()
	shutdownAwait(t, mediaDone)
	shutdownAwait(t, canceled)
	if r.closed.Load() {
		t.Fatal("resources closed while handler cleanup was held")
	}
	finishAdmin()
	shutdownAwait(t, adminDone)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error=%v", err)
	}
	if !r.closed.Load() {
		t.Fatal("resources not closed after work finished")
	}
}

func TestShutdownWatchdogCoversCleanupAndRunsOnce(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	r := &shutdownTestRuntime{closeHook: func(ctx context.Context) error { close(entered); <-release; return ctx.Err() }}
	c := newShutdownCoordinator(r, &http.Server{}, &http.Server{})
	c.drainTimeout = 20 * time.Millisecond
	c.totalTimeout = 100 * time.Millisecond
	var exits atomic.Int32
	c.forceExit = func(code int) {
		if code != 1 {
			t.Errorf("exit=%d", code)
		}
		exits.Add(1)
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- c.shutdown() }()
	shutdownAwait(t, entered)
	go func() { second <- c.shutdown() }()
	if !errors.Is(<-first, errShutdownDeadline) || !errors.Is(<-second, errShutdownDeadline) {
		t.Fatal("missing watchdog result")
	}
	if exits.Load() != 1 {
		t.Fatalf("watchdog ran %d times", exits.Load())
	}
	close(release)
}

func TestShutdownNonCooperativeProcess(t *testing.T) {
	if os.Getenv("TIMICH_TEST_SHUTDOWN_STUCK") == "1" {
		r := &shutdownTestRuntime{}
		_, _, _ = r.work.Acquire()
		c := newShutdownCoordinator(r, &http.Server{}, &http.Server{})
		c.drainTimeout = 10 * time.Millisecond
		c.totalTimeout = 60 * time.Millisecond
		_ = c.shutdown()
		os.Exit(9)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShutdownNonCooperativeProcess$")
	cmd.Env = append(os.Environ(), "TIMICH_TEST_SHUTDOWN_STUCK=1")
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("exit=%v output=%s", err, output)
	}
	// Deadline diagnostics are best effort; process exit must not wait for them.
}

// A real listener failure must still stop the other server and release resources.
func TestServeFailureUsesShutdownCoordinator(t *testing.T) {
	r := &shutdownTestRuntime{}
	admin := &http.Server{Addr: "invalid-address"}
	media := &http.Server{Addr: "127.0.0.1:0", Handler: r.work.Handler(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { io.WriteString(w, "ok") }))}
	c := newShutdownCoordinator(r, admin, media)
	c.totalTimeout = 2 * time.Second
	err := serveUntilShutdown(admin, media, make(chan struct{}), c)
	if err == nil || !strings.Contains(err.Error(), "admin API server") {
		t.Fatalf("listen error=%v", err)
	}
	if !r.closed.Load() {
		t.Fatal("runtime not closed")
	}
}

type shutdownFailingListener struct {
	net.Listener
	failure error
}

func (l shutdownFailingListener) Close() error { _ = l.Listener.Close(); return l.failure }

func TestShutdownReportsBothAPIErrors(t *testing.T) {
	r := &shutdownTestRuntime{}
	failures := []error{errors.New("admin listener close"), errors.New("media listener close")}
	servers := make([]*http.Server, 2)
	for i := range servers {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ready := make(chan struct{})
		servers[i] = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { w.WriteHeader(204) }), BaseContext: func(net.Listener) context.Context { close(ready); return context.Background() }}
		go servers[i].Serve(shutdownFailingListener{listener, failures[i]})
		shutdownAwait(t, ready)
	}
	err := newShutdownCoordinator(r, servers[0], servers[1]).shutdown()
	for _, failure := range failures {
		if !errors.Is(err, failure) {
			t.Fatalf("lost %v from %v", failure, err)
		}
	}
	if !r.closed.Load() {
		t.Fatal("listener failure prevented resource cleanup")
	}
}
