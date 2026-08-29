package runtime

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
)

func TestForegroundActivityGateDefersBackgroundWorkUntilRequestCompletes(t *testing.T) {
	gate := foregroundActivityGate{quietPeriod: time.Nanosecond}
	finish := gate.begin()

	blockedCtx, cancelBlocked := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelBlocked()
	if err := gate.waitUntilIdle(blockedCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitUntilIdle(active) error = %v, want deadline exceeded", err)
	}

	finish()
	finish()
	readyCtx, cancelReady := context.WithTimeout(context.Background(), time.Second)
	defer cancelReady()
	if err := gate.waitUntilIdle(readyCtx); err != nil {
		t.Fatalf("waitUntilIdle(completed) error = %v", err)
	}
}

func TestForegroundActivityGateCancelsMaintenanceReadWhenRequestStarts(t *testing.T) {
	gate := foregroundActivityGate{quietPeriod: time.Nanosecond}
	backgroundCtx, release, err := gate.beginCancelableBackground(context.Background())
	if err != nil {
		t.Fatalf("beginCancelableBackground() error = %v", err)
	}
	defer release()

	finishForeground := gate.begin()
	defer finishForeground()
	select {
	case <-backgroundCtx.Done():
		if !errors.Is(backgroundCtx.Err(), context.Canceled) {
			t.Fatalf("background context error = %v, want canceled", backgroundCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("foreground request did not cancel maintenance context")
	}
}

func TestForegroundMediaResponseKeepsGateActiveUntilBodyCloses(t *testing.T) {
	runtime := &AgentRuntime{}
	runtime.foregroundCatalog.quietPeriod = time.Nanosecond
	finish := runtime.foregroundCatalog.begin()
	response, err := runtime.foregroundMediaResponse(&catalog.UpstreamMediaResponse{
		Body: io.NopCloser(strings.NewReader("preview")),
	}, nil, finish)
	if err != nil {
		t.Fatalf("foregroundMediaResponse() error = %v", err)
	}

	blockedCtx, cancelBlocked := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelBlocked()
	if err := runtime.foregroundCatalog.waitUntilIdle(blockedCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitUntilIdle(open media body) error = %v, want deadline exceeded", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	readyCtx, cancelReady := context.WithTimeout(context.Background(), time.Second)
	defer cancelReady()
	if err := runtime.foregroundCatalog.waitUntilIdle(readyCtx); err != nil {
		t.Fatalf("waitUntilIdle(closed media body) error = %v", err)
	}
}

func TestForegroundRequestCancelsSemanticBackgroundAssignment(t *testing.T) {
	runtime := &AgentRuntime{}
	runtime.foregroundCatalog.quietPeriod = time.Nanosecond
	runtime.schedulerWorkState = schedulerWorkState{
		ConfigHash: "test",
		UpdatedAt:  time.Now().UTC(),
	}
	started := make(chan struct{})
	finished := make(chan bool, 1)
	go func() {
		finished <- runtime.runForegroundCancelableBackgroundAssignment(context.Background(), func(ctx context.Context) bool {
			close(started)
			<-ctx.Done()
			return false
		})
	}()
	<-started

	finishForeground := runtime.foregroundCatalog.begin()
	defer finishForeground()
	select {
	case completed := <-finished:
		if completed {
			t.Fatal("background assignment completed = true after foreground cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("foreground request did not cancel semantic background assignment")
	}
	runtime.schedulerWorkStateMu.Lock()
	dirty := runtime.schedulerWorkState.Dirty
	runtime.schedulerWorkStateMu.Unlock()
	if !dirty {
		t.Fatal("scheduler work state was not marked dirty after partial background cancellation")
	}
}
