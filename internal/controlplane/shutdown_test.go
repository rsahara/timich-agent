package controlplane

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	runtimestate "github.com/rsahara/timich-agent/internal/runtime"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestControlPlaneJoinsCanceledDispatchBeforeReturning(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	registerRelayIsolationServer(server, &relayIsolationServer{commands: []*dynamicpb.Message{relayCommandForClientTest(t, "held", "/v1/assets/search")}})
	go server.Serve(listener)
	defer server.Stop()
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	r := &runtimestate.AgentRuntime{}
	client := &Client{target: listener.Addr().String(), agentID: "test", hostedBaseURL: "https://relay.example", runtime: r, mediaHandler: http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		close(entered)
		<-q.Context().Done()
		close(canceled)
		<-release
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.runOnce(ctx) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch not entered")
	}
	r.BeginShutdown()
	response := client.handleRelayRequestContext(context.Background(), &RelayRequest{FetchID: "late", Method: "GET", Path: "/v1/assets/search"})
	if response.StatusCode != 503 {
		t.Fatalf("late response=%d", response.StatusCode)
	}
	r.CancelRequests()
	<-canceled
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Run returned before cleanup: %v", err)
	default:
	}
	finish()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch not joined")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}
