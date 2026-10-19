package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/adminapi"
	"github.com/rsahara/timich-agent/internal/config"
	runtimestate "github.com/rsahara/timich-agent/internal/runtime"
	"github.com/rsahara/timich-agent/internal/store"
)

func TestAdminRestartResponsePrecedesShutdown(t *testing.T) {
	cfg := config.ResolvedConfig{Config: config.Default()}
	cfg.DataDir = t.TempDir()
	cfg.ConfigPath = filepath.Join(cfg.DataDir, "agent.json")
	state, err := store.LoadOrCreate(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	state.State.AdminToken = "test-admin-token"
	a, err := runtimestate.NewAgentRuntime(runtimestate.BuildInfo{}, cfg, state, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	restart := make(chan struct{}, 1)
	admin := httptest.NewUnstartedServer(a.RequestHandler(adminapi.NewMuxWithOptions(a, adminapi.Options{Restart: func(context.Context) error { scheduleRestart(restart); return nil }})))
	admin.Config.BaseContext = func(net.Listener) context.Context { return a.RequestContext() }
	admin.Start()
	defer admin.Close()
	media := httptest.NewServer(a.RequestHandler(http.NotFoundHandler()))
	defer media.Close()
	c := newShutdownCoordinator(a, admin.Config, media.Config)
	done := make(chan error, 1)
	go func() { done <- waitForShutdown(nil, restart, nil, c) }()
	request, err := http.NewRequest("POST", admin.URL+"/v1/restart", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer test-admin-token")
	response, err := admin.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 202 {
		t.Fatalf("restart status=%d", response.StatusCode)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restart did not stop")
	}
	if _, _, ok := a.AcquireRequest(); ok {
		t.Fatal("runtime still accepting work")
	}
}

func TestSIGTERMUsesProductionServeLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM is a Unix process signal")
	}
	if os.Getenv("TIMICH_TEST_SHUTDOWN_SIGNAL") == "1" {
		r := &shutdownTestRuntime{closeHook: func(context.Context) error { fmt.Fprintln(os.Stdout, "CLOSED"); return nil }}
		admin := &http.Server{Addr: os.Getenv("TIMICH_TEST_ADMIN_ADDR"), Handler: r.work.Handler(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { w.WriteHeader(204) }))}
		media := &http.Server{Addr: "127.0.0.1:0", Handler: r.work.Handler(http.NotFoundHandler())}
		go func() {
			deadline := time.After(3 * time.Second)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-deadline:
					os.Exit(7)
				case <-ticker.C:
					conn, err := net.DialTimeout("tcp", admin.Addr, 50*time.Millisecond)
					if err == nil {
						conn.Close()
						fmt.Fprintln(os.Stdout, "READY")
						return
					}
				}
			}
		}()
		if err := serveUntilShutdown(admin, media, make(chan struct{}), newShutdownCoordinator(r, admin, media)); err != nil {
			os.Exit(8)
		}
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSIGTERMUsesProductionServeLoop$")
	cmd.Env = append(os.Environ(), "TIMICH_TEST_SHUTDOWN_SIGNAL=1", "TIMICH_TEST_ADMIN_ADDR="+addr)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "READY" {
		t.Fatal("child did not start")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for scanner.Scan() {
		output.WriteString(scanner.Text())
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("signal shutdown failed: %v %s", err, output.String())
	}
	if !strings.Contains(output.String(), "CLOSED") {
		t.Fatalf("resource cleanup missing: %s", output.String())
	}
}
