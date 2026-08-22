package controlplaneprobe

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	contracts "github.com/rsahara/timich-agent/packages/contracts/controlplane"
	"github.com/rsahara/timich-agent/internal/controlplanetransport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestProbeReturnsTrimmedAckAndSendsNormalizedHello(t *testing.T) {
	t.Parallel()

	type receivedHello struct {
		agentID       string
		version       string
		authorization []string
	}
	received := make(chan receivedHello, 1)
	dialContext := startProbeServer(t, func(event *dynamicpb.Message, stream grpc.ServerStream) error {
		hello := event.Get(contracts.FieldByName(event, "hello")).Message()
		incomingMetadata, _ := metadata.FromIncomingContext(stream.Context())
		received <- receivedHello{
			agentID:       contracts.StringFieldFromReflect(event, "agent_id"),
			version:       contracts.StringFieldFromReflect(hello, "version"),
			authorization: incomingMetadata.Get(contracts.ControlPlaneAuthorizationKey),
		}
		return sendProbeAck(stream, " ready ")
	})

	ack, err := probe(t.Context(), ProbeInput{
		AgentID: " agent-home ",
		Version: " 1.2.3 ",
		Target:  " control-plane ",
	}, dialContext)
	if err != nil {
		t.Fatalf("probe() error = %v", err)
	}
	if ack != "ready" {
		t.Fatalf("ack = %q, want ready", ack)
	}

	hello := <-received
	if hello.agentID != "agent-home" || hello.version != "1.2.3" {
		t.Fatalf("hello = %+v, want normalized agent ID and version", hello)
	}
	if len(hello.authorization) != 0 {
		t.Fatalf("authorization metadata = %q, want unsigned probe", hello.authorization)
	}
}

func TestProbeRejectsInvalidTransportBeforeDial(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     ProbeInput
		wantError string
	}{
		{
			name:      "unsupported scheme",
			input:     ProbeInput{Target: "ftp://relay.example"},
			wantError: "unsupported control-plane scheme",
		},
		{
			name: "signed raw target",
			input: ProbeInput{
				AgentID:    "agent-home",
				KeyID:      "key-1",
				PrivateKey: "private-key",
				Target:     "127.0.0.1:8080",
			},
			wantError: "signed control-plane tokens require https transport",
		},
		{
			name: "signed HTTP target",
			input: ProbeInput{
				AgentID:    "agent-home",
				KeyID:      "key-1",
				PrivateKey: "private-key",
				Target:     "http://relay.example",
			},
			wantError: "signed control-plane tokens require https transport",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dialCalled := false
			_, err := probe(t.Context(), test.input, func(context.Context, string, ...grpc.DialOption) (*grpc.ClientConn, error) {
				dialCalled = true
				return nil, errors.New("unexpected dial")
			})
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("probe() error = %v, want containing %q", err, test.wantError)
			}
			if dialCalled {
				t.Fatal("dial called for invalid transport")
			}
		})
	}
}

func TestProbeNormalizesDialTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      ProbeInput
		wantTarget string
	}{
		{
			name:       "unsigned raw target",
			input:      ProbeInput{Target: " relay.example:8080 "},
			wantTarget: "relay.example:8080",
		},
		{
			name:       "unsigned HTTP target",
			input:      ProbeInput{Target: " http://relay.example "},
			wantTarget: "relay.example:80",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			wantDialError := errors.New("stop after dial configuration")
			var gotTarget string
			_, err := probe(t.Context(), test.input, func(_ context.Context, target string, _ ...grpc.DialOption) (*grpc.ClientConn, error) {
				gotTarget = target
				return nil, wantDialError
			})
			if !errors.Is(err, wantDialError) {
				t.Fatalf("probe() error = %v, want %v", err, wantDialError)
			}
			if gotTarget != test.wantTarget {
				t.Fatalf("dial target = %q, want %q", gotTarget, test.wantTarget)
			}
		})
	}
}

func TestProbeSendsSignedAuthorizationMetadata(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	encodedPublicKey := base64.RawStdEncoding.EncodeToString(publicKey)
	encodedPrivateKey := base64.RawStdEncoding.EncodeToString(privateKey)
	serverCredentials, clientCredentials := newProbeTLSCredentials(t)
	authorization := make(chan string, 1)
	dialContext := startProbeServer(t, func(_ *dynamicpb.Message, stream grpc.ServerStream) error {
		incomingMetadata, ok := metadata.FromIncomingContext(stream.Context())
		if !ok {
			return status.Error(codes.Unauthenticated, "authorization metadata is missing")
		}
		values := incomingMetadata.Get(contracts.ControlPlaneAuthorizationKey)
		if len(values) != 1 {
			return status.Errorf(codes.Unauthenticated, "authorization value count = %d", len(values))
		}
		authorization <- values[0]
		return sendProbeAck(stream, "signed-ready")
	}, grpc.Creds(serverCredentials))
	secureDialContext := func(ctx context.Context, target string, options ...grpc.DialOption) (*grpc.ClientConn, error) {
		options = append(options, grpc.WithTransportCredentials(clientCredentials))
		return dialContext(ctx, target, options...)
	}

	ack, err := probe(t.Context(), ProbeInput{
		AgentID:    " agent-home ",
		Version:    " 1.2.3 ",
		KeyID:      " key-1 ",
		PrivateKey: encodedPrivateKey,
		Target:     " https://relay.test ",
		TLS:        TLSConfig{ServerName: "relay.test"},
	}, secureDialContext)
	if err != nil {
		t.Fatalf("probe() error = %v", err)
	}
	if ack != "signed-ready" {
		t.Fatalf("ack = %q, want signed-ready", ack)
	}
	authorizationValue := <-authorization
	token, ok := strings.CutPrefix(authorizationValue, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		t.Fatalf("authorization metadata = %q, want Bearer token", authorizationValue)
	}
	claims, err := contracts.VerifyControlPlaneToken(encodedPublicKey, token, time.Now().UTC())
	if err != nil {
		t.Fatalf("VerifyControlPlaneToken() error = %v", err)
	}
	if claims.AgentID != "agent-home" || claims.KeyID != "key-1" || claims.Audience != contracts.ControlPlaneAudience {
		t.Fatalf("token claims = %+v, want signed probe identity", claims)
	}
}

func TestProbeRejectsInvalidServerCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		send      func(grpc.ServerStream) error
		wantError string
	}{
		{
			name: "missing payload",
			send: func(stream grpc.ServerStream) error {
				command, err := contracts.NewMessage("ServerCommand")
				if err != nil {
					return err
				}
				return stream.SendMsg(command)
			},
			wantError: "server command payload is missing",
		},
		{
			name:      "empty acknowledgement",
			send:      func(stream grpc.ServerStream) error { return sendProbeAck(stream, " ") },
			wantError: "server ack message is empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dialContext := startProbeServer(t, func(_ *dynamicpb.Message, stream grpc.ServerStream) error {
				return test.send(stream)
			})
			_, err := probe(t.Context(), ProbeInput{Target: "control-plane"}, dialContext)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("probe() error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func TestProbeReportsReceiveFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler func(*dynamicpb.Message, grpc.ServerStream) error
		check   func(error) bool
	}{
		{
			name:    "server closes stream",
			handler: func(*dynamicpb.Message, grpc.ServerStream) error { return nil },
			check:   func(err error) bool { return errors.Is(err, io.EOF) },
		},
		{
			name: "server returns status",
			handler: func(*dynamicpb.Message, grpc.ServerStream) error {
				return status.Error(codes.PermissionDenied, "probe denied")
			},
			check: func(err error) bool { return status.Code(err) == codes.PermissionDenied },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dialContext := startProbeServer(t, test.handler)
			_, err := probe(t.Context(), ProbeInput{Target: "control-plane"}, dialContext)
			if err == nil || !test.check(err) {
				t.Fatalf("probe() error = %v, want expected receive failure", err)
			}
		})
	}
}

func TestProbeStopsWhenContextIsCanceled(t *testing.T) {
	t.Parallel()

	receivedHello := make(chan struct{}, 1)
	dialContext := startProbeServer(t, func(_ *dynamicpb.Message, stream grpc.ServerStream) error {
		receivedHello <- struct{}{}
		<-stream.Context().Done()
		return stream.Context().Err()
	})
	waitContext, cancelWait := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelWait()
	probeContext, cancelProbe := context.WithCancel(waitContext)
	defer cancelProbe()
	result := make(chan error, 1)
	go func() {
		_, err := probe(probeContext, ProbeInput{Target: "control-plane"}, dialContext)
		result <- err
	}()

	select {
	case <-receivedHello:
	case err := <-result:
		t.Fatalf("probe() returned before sending hello: %v", err)
	case <-waitContext.Done():
		t.Fatalf("waiting for probe hello: %v", waitContext.Err())
	}
	cancelProbe()

	select {
	case err := <-result:
		if status.Code(err) != codes.Canceled && !errors.Is(err, context.Canceled) {
			t.Fatalf("probe() error = %v, want context cancellation", err)
		}
	case <-waitContext.Done():
		t.Fatalf("waiting for probe cancellation: %v", waitContext.Err())
	}
}

func TestProbeRejectsOversizedHello(t *testing.T) {
	t.Parallel()

	dialContext := startProbeServer(t, func(_ *dynamicpb.Message, _ grpc.ServerStream) error {
		return nil
	})
	_, err := probe(t.Context(), ProbeInput{
		Version: strings.Repeat("x", controlplanetransport.MaxRelayMessageBytes),
		Target:  "control-plane",
	}, dialContext)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("probe() error = %v, want resource exhausted", err)
	}
}

func TestProbeRejectsOversizedServerCommand(t *testing.T) {
	t.Parallel()

	dialContext := startProbeServer(t, func(_ *dynamicpb.Message, stream grpc.ServerStream) error {
		return sendProbeAck(stream, strings.Repeat("x", controlplanetransport.MaxRelayMessageBytes))
	})
	_, err := probe(t.Context(), ProbeInput{Target: "control-plane"}, dialContext)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("probe() error = %v, want resource exhausted", err)
	}
}

func TestProbeReportsClosedConnection(t *testing.T) {
	t.Parallel()

	dialContext := startProbeServer(t, func(_ *dynamicpb.Message, stream grpc.ServerStream) error {
		return sendProbeAck(stream, "ready")
	})
	closedDialContext := func(ctx context.Context, target string, options ...grpc.DialOption) (*grpc.ClientConn, error) {
		conn, err := dialContext(ctx, target, options...)
		if err != nil {
			return nil, err
		}
		if err := conn.Close(); err != nil {
			return nil, err
		}
		return conn, nil
	}
	if _, err := probe(t.Context(), ProbeInput{Target: "control-plane"}, closedDialContext); err == nil {
		t.Fatal("probe() error = nil, want closed connection error")
	}
}

func TestProbeUsesCallerContextForDial(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Probe(ctx, ProbeInput{Target: "127.0.0.1:1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Probe() error = %v, want context canceled", err)
	}
}

type probeControlPlaneAPI interface {
	OpenControlStream(grpc.ServerStream) error
}

type probeControlPlaneServer struct {
	handle func(*dynamicpb.Message, grpc.ServerStream) error
}

func (s probeControlPlaneServer) OpenControlStream(stream grpc.ServerStream) error {
	event, err := contracts.NewMessage("AgentEvent")
	if err != nil {
		return err
	}
	if err := stream.RecvMsg(event); err != nil {
		return err
	}
	return s.handle(event, stream)
}

func startProbeServer(t *testing.T, handler func(*dynamicpb.Message, grpc.ServerStream) error, options ...grpc.ServerOption) dialContextFunc {
	t.Helper()

	listener := bufconn.Listen(8 << 20)
	server := grpc.NewServer(options...)
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: contracts.ServiceFullName(),
		HandlerType: (*probeControlPlaneAPI)(nil),
		Streams: []grpc.StreamDesc{
			{
				StreamName:    "OpenControlStream",
				ServerStreams: true,
				ClientStreams: true,
				Handler: func(server any, stream grpc.ServerStream) error {
					return server.(probeControlPlaneAPI).OpenControlStream(stream)
				},
			},
		},
	}, probeControlPlaneServer{handle: handler})
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serveDone)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
		<-serveDone
	})

	return func(ctx context.Context, target string, options ...grpc.DialOption) (*grpc.ClientConn, error) {
		options = append(options, grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}))
		return grpc.DialContext(ctx, target, options...)
	}
}

func newProbeTLSCredentials(t *testing.T) (credentials.TransportCredentials, credentials.TransportCredentials) {
	t.Helper()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "relay.test"},
		DNSNames:              []string{"relay.test"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatalf("CreateCertificate() error = %v", err)
	}
	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		t.Fatalf("ParseCertificate() error = %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	serverCredentials := credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{certificateDER},
			PrivateKey:  privateKey,
			Leaf:        certificate,
		}},
	})
	clientCredentials := credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: "relay.test",
	})
	return serverCredentials, clientCredentials
}

func sendProbeAck(stream grpc.ServerStream, message string) error {
	command, err := contracts.NewMessage("ServerCommand")
	if err != nil {
		return err
	}
	ack, err := contracts.NewMessage("ServerAck")
	if err != nil {
		return err
	}
	ack.Set(contracts.FieldByName(ack, "message"), protoreflect.ValueOfString(message))
	command.Set(contracts.FieldByName(command, "ack"), protoreflect.ValueOfMessage(ack.ProtoReflect()))
	return stream.SendMsg(command)
}
