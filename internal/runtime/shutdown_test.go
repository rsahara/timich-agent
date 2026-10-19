package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRuntimeShutdownCancelsAllOwnersBeforeWaiting(t *testing.T) {
	a, _ := newConcurrentUploadRuntime(t)
	first, second, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a.background.Go(func(ctx context.Context) { <-ctx.Done(); close(first); <-release })
	a.background.Go(func(ctx context.Context) { <-ctx.Done(); close(second) })
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	a.StartUploadMaintenance()
	a.StartDatasourceIndexingStatusRefresh()
	a.BeginShutdown()
	for _, canceled := range []<-chan struct{}{first, second} {
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("owner was not canceled")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if a.CloseResources(ctx) == nil {
		t.Fatal("closed while owner was running")
	}
	// SQLite remains usable during the owner's cleanup.
	device := addConcurrentUploadDevice(t, a)
	if device == "" {
		t.Fatal("empty device")
	}
	releaseOnce()
	if err := a.WaitStopped(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.CloseResources(context.Background()); err != nil {
		t.Fatal(err)
	}
	var starts sync.WaitGroup
	for range 20 {
		starts.Go(func() {
			a.StartUploadMaintenance()
			a.StartDatasourceIndexingStatusRefresh()
			a.StartBackgroundWorkerScheduler()
			a.StartLocalDatasourceScan()
			a.triggerLocalPhase0Scan("after-shutdown")
			a.StartSemanticModelRuntime()
		})
	}
	starts.Wait()
	if a.background.Pending() != 0 {
		t.Fatal("work restarted after shutdown")
	}
}

func TestRuntimeShutdownTracksONNXCallbacks(t *testing.T) {
	a := newTestAgentRuntime(t, BuildInfo{}, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	t.Cleanup(finish)
	a.semanticONNXRuntime.SetStateChangeCallback(func() { close(entered); <-release })
	a.semanticONNXRuntime.mu.Lock()
	a.semanticONNXRuntime.notifyStateChangeLocked()
	a.semanticONNXRuntime.mu.Unlock()
	<-entered
	a.BeginShutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if a.WaitStopped(ctx) == nil {
		t.Fatal("callback was not tracked")
	}
	finish()
	if err := a.WaitStopped(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.semanticONNXRuntime.mu.Lock()
	a.semanticONNXRuntime.notifyStateChangeLocked()
	a.semanticONNXRuntime.mu.Unlock()
	if a.semanticONNXRuntime.callbacks.Pending() != 0 {
		t.Fatal("callback admitted after shutdown")
	}
}

func TestShutdownHTTPUploadRollsBackBeforeDatabaseCloseAndResumes(t *testing.T) {
	a, root := newConcurrentUploadRuntime(t)
	device := addConcurrentUploadDevice(t, a)
	body := []byte("abcdef")
	upload := startConcurrentUpload(t, a, device, "shutdown-upload", body)
	entered, finished := make(chan struct{}), make(chan struct{})
	var appendErr error
	server := httptest.NewUnstartedServer(a.RequestHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		close(entered)
		_, appendErr = a.AppendUploadChunk(device, upload.UploadID, concurrentChunk(body, r.Body))
	})))
	server.Config.BaseContext = func(net.Listener) context.Context { return a.RequestContext() }
	server.Start()
	defer server.Close()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "POST /chunk HTTP/1.1\r\nHost: agent\r\nContent-Length: 6\r\n\r\nab")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("upload not started")
	}
	a.BeginShutdown()
	grace, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := server.Config.Shutdown(grace); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain=%v", err)
	}
	a.CancelRequests()
	_ = server.Config.Close()
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := a.WaitStopped(ctx); err != nil {
		t.Fatal(err)
	}
	<-finished
	if appendErr == nil {
		t.Fatal("interrupted upload succeeded")
	}
	temp := filepath.Join(root, ".timich-upload-tmp", upload.UploadID+".part")
	info, err := os.Stat(temp)
	if err != nil || info.Size() != 0 {
		t.Fatalf("temp=%v err=%v", info, err)
	}
	if got := mustConcurrentSession(t, a, upload.UploadID).NextOffset; got != 0 {
		t.Fatalf("offset=%d", got)
	}
	if err := a.CloseResources(ctx); err != nil {
		t.Fatal(err)
	}
	// Reopen the same persistent state, then retry and finish the actual upload.
	reopened, err := NewAgentRuntime(BuildInfo{}, a.config, a.state, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.AppendUploadChunk(device, upload.UploadID, concurrentChunk(body, bytes.NewReader(body))); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.CompleteUploadSession(device, upload.UploadID, concurrentComplete(body)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "photo.jpg"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("final=%q error=%v", got, err)
	}
}
