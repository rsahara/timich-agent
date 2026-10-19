package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Exercises both places where a remote stream rejection may be observed.
type rejectionServer struct {
	rejectAfterHello bool
	attempts         atomic.Int32
	command          *dynamicpb.Message
	uploads          chan *dynamicpb.Message
	rejected         chan struct{}
}

func (s *rejectionServer) OpenControlStream(stream grpc.ServerStream) error {
	attempt := s.attempts.Add(1)
	if attempt == 1 && !s.rejectAfterHello {
		close(s.rejected)
		return status.Error(codes.Unavailable, "agent control stream already connected")
	}
	hello, err := newMessage("AgentEvent")
	if err != nil {
		return err
	}
	if err := stream.RecvMsg(hello); err != nil {
		return err
	}
	if attempt == 1 {
		close(s.rejected)
		return status.Error(codes.Unavailable, "agent control stream already connected")
	}
	if err := stream.SendMsg(s.command); err != nil {
		return err
	}
	for {
		event, err := newMessage("AgentEvent")
		if err != nil {
			return err
		}
		if err := stream.RecvMsg(event); err != nil {
			return err
		}
	}
}

func (s *rejectionServer) UploadFetchResult(ctx context.Context, request *dynamicpb.Message) (*dynamicpb.Message, error) {
	select {
	case s.uploads <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	response, err := newMessage("FetchResultUploadResponse")
	if err != nil {
		return nil, err
	}
	response.Set(fieldByName(response, "accepted"), protoreflect.ValueOfBool(true))
	return response, nil
}

func TestRunRetriesUnavailableAndRelays(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		afterHello, cancelOnRejection bool
	}{
		{name: "before_hello"},
		{name: "after_hello", afterHello: true},
		{name: "cancel_on_rejection", cancelOnRejection: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var registrations atomic.Int32
			registrationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				registrations.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer registrationServer.Close()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			service := &rejectionServer{rejectAfterHello: tc.afterHello, command: relayCommandForClientTest(t, "retry-test", "/v1/assets/search"), uploads: make(chan *dynamicpb.Message, 1), rejected: make(chan struct{})}
			server := grpc.NewServer()
			registerRelayIsolationServer(server, service)
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			defer listener.Close()
			publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			client := &Client{
				target: listener.Addr().String(), agentID: "retry-test-agent", version: "retry-test",
				hostedBaseURL: registrationServer.URL, keyID: "retry-test-key",
				publicKey: base64.RawStdEncoding.EncodeToString(publicKey), privateKey: base64.RawStdEncoding.EncodeToString(privateKey),
				mediaHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("relay-after-retry")) }),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			finished := make(chan error, 1)
			start := time.Now()
			go func() { finished <- client.Run(ctx) }()
			if tc.cancelOnRejection {
				select {
				case <-service.rejected:
				case <-ctx.Done():
					t.Fatal("first stream was not rejected")
				}
				cancel()
				select {
				case err := <-finished:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Run did not stop after rejection and cancellation")
				}
				if registrations.Load() != 1 || service.attempts.Load() != 1 {
					t.Fatal("Run started another attempt after cancellation")
				}
				return
			}
			select {
			case upload := <-service.uploads:
				if got := upload.Get(fieldByName(upload, "status_code")).Int(); got != 200 {
					t.Fatalf("relayed status = %d", got)
				}
				if got := string(upload.Get(fieldByName(upload, "body")).Bytes()); got != "relay-after-retry" {
					t.Fatalf("relayed body = %q", got)
				}
			case <-ctx.Done():
				t.Fatal("Agent did not reconnect and return a relay response")
			}
			if registrations.Load() != 2 || service.attempts.Load() != 2 {
				t.Fatalf("registrations=%d streams=%d; expected two application-level attempts", registrations.Load(), service.attempts.Load())
			}
			t.Logf("Agent retried Unavailable and returned HTTP 200: registrations=%d streams=%d elapsed=%s", registrations.Load(), service.attempts.Load(), time.Since(start))
			cancel()
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not finish after parent cancellation")
			}
		})
	}
}
