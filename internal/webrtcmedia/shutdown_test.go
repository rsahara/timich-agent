package webrtcmedia

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
)

func TestShutdownTracksCanceledRequestAfterMapRemoval(t *testing.T) {
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	t.Cleanup(finish)
	m := NewManager(func(r *http.Request, _ string) (*catalog.UpstreamMediaResponse, error) {
		close(entered)
		<-r.Context().Done()
		close(canceled)
		<-release
		return nil, r.Context().Err()
	})
	active := newTestSession("shutdown")
	channel := &fakeDataChannel{}
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		m.handleMediaRequest(active, channel, mediaRequest{RequestID: "r", AssetID: "photo"})
	}()
	<-entered
	if !active.cancelMediaRequest("r", "test") {
		t.Fatal("request not found")
	}
	<-canceled
	m.BeginShutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if m.Wait(ctx) == nil {
		t.Fatal("removing the request map entry was mistaken for completion")
	}
	if m.work.Go(func(context.Context) { t.Error("post-stop request ran") }) {
		t.Fatal("new request accepted")
	}
	if _, err := m.AnswerOffer(context.Background(), OfferRequest{SDP: "new"}, "missing"); err == nil {
		t.Fatal("new offer accepted")
	}
	finish()
	<-requestDone
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownCancellationReachesWebRTCRequest(t *testing.T) {
	entered := make(chan struct{})
	m := NewManager(func(r *http.Request, _ string) (*catalog.UpstreamMediaResponse, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.handleMediaRequest(newTestSession("s"), &fakeDataChannel{}, mediaRequest{RequestID: "r", AssetID: "photo"})
	}()
	<-entered
	if err := m.Cancel(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
}
