package lifecycle

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestShutdownKeepsCanceledWorkOwnedUntilRelease(t *testing.T) {
	var group Group
	root, release, ok := group.Acquire()
	if !ok {
		t.Fatal("initial acquisition rejected")
	}
	group.CloseAdmission()
	if root.Err() != nil {
		t.Fatal("closing admission canceled admitted work")
	}
	group.Cancel()
	if root.Err() == nil {
		t.Fatal("root was not canceled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if group.Wait(ctx) == nil {
		t.Fatal("cancellation was mistaken for completion")
	}
	release()
	release()
	if err := group.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	group.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("work admitted after shutdown") })).ServeHTTP(recorder, httptest.NewRequest("POST", "/", nil))
	if recorder.Code != 503 {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestAdmissionRacesShutdownWithoutLosingWork(t *testing.T) {
	var group Group
	_, release, _ := group.Acquire()
	start := make(chan struct{})
	var racers sync.WaitGroup
	for range 100 {
		racers.Go(func() {
			<-start
			if _, done, ok := group.Acquire(); ok {
				done()
			}
		})
	}
	close(start)
	group.CloseAdmission()
	racers.Wait()
	if group.Pending() != 1 {
		t.Fatalf("pending=%d", group.Pending())
	}
	if _, _, ok := group.Acquire(); ok {
		t.Fatal("admitted after closure")
	}
	release()
	if err := group.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
