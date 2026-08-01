package compatibility

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	contractscontrolplane "github.com/rsahara/timich-agent/packages/contracts/controlplane"
	"github.com/rsahara/timich-agent/internal/catalog"
	agentconfig "github.com/rsahara/timich-agent/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestRunReturnsOKWhenDatasourceAndRemoteBrowsingChecksPass(t *testing.T) {
	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search/metadata":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"assets": map[string]any{
					"items":    []map[string]any{},
					"total":    0,
					"nextPage": nil,
				},
			})
		default:
			t.Fatalf("unexpected datasource path %s", r.URL.Path)
		}
	}))
	defer datasourceServer.Close()

	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			t.Fatalf("unexpected relay server path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service": "timich-server",
			"version": "test-server",
		})
	}))
	defer relayServer.Close()

	controlPlaneAddress, shutdown := startControlPlaneServer(t)
	defer shutdown()

	cfg := agentconfig.ResolvedConfig{
		Config: agentconfig.Config{
			Hosted: agentconfig.RemoteBrowsingConfig{
				Enabled:   true,
				ServerURL: relayServer.URL,
			},
			ControlPlaneAddress: controlPlaneAddress,
			Datasources: []agentconfig.DatasourceConfig{{
				Name:        "Home Immich",
				Kind:        "immich",
				URL:         datasourceServer.URL,
				AccessToken: "test-token",
			}},
		},
	}

	report := NewService("test-agent-version", "agent-home", "relay-key", "private-key", cfg, catalog.NewService(cfg.Datasources), RelayRegistrationState{
		CredentialSynced: true,
		Ready:            true,
	}).Run(context.Background())
	if report.Status != StatusOK {
		t.Fatalf("report status = %q, want %q", report.Status, StatusOK)
	}
	if len(report.Checks) != 4 {
		t.Fatalf("check count = %d, want 4", len(report.Checks))
	}
	if report.Checks[3].Details["ack"] == "" {
		t.Fatalf("relay connection ack missing from details: %#v", report.Checks[3].Details)
	}
}

func TestRunFailsWhenRemoteBrowsingDisabled(t *testing.T) {
	cfg := agentconfig.ResolvedConfig{
		Config: agentconfig.Config{
			Hosted: agentconfig.RemoteBrowsingConfig{
				Enabled:   false,
				ServerURL: "https://timich.runo.jp",
			},
		},
	}

	report := NewService("test-agent-version", "agent-home", "", "", cfg, catalog.NewService(nil), RelayRegistrationState{}).Run(context.Background())
	if report.Status != StatusFailed {
		t.Fatalf("report status = %q, want %q", report.Status, StatusFailed)
	}
	if report.Checks[0].Status != StatusFailed {
		t.Fatalf("agent_config status = %q, want %q", report.Checks[0].Status, StatusFailed)
	}
}

func TestAgentConfigCheckAllowsUnsyncedRelayCredentialWithSigningKey(t *testing.T) {
	cfg := agentconfig.ResolvedConfig{
		Config: agentconfig.Config{
			Hosted: agentconfig.RemoteBrowsingConfig{
				Enabled:   true,
				ServerURL: "https://timich.runo.jp",
			},
			ControlPlaneAddress: "127.0.0.1:0",
		},
	}

	check := NewService("test-agent-version", "agent-home", "relay-key", "private-key", cfg, catalog.NewService(nil), RelayRegistrationState{}).runAgentConfigCheck()
	if check.Status != StatusOK {
		t.Fatalf("agent_config status = %q, want %q", check.Status, StatusOK)
	}
	if check.Details["relayCredentialSynced"] != false {
		t.Fatalf("relayCredentialSynced = %#v, want false", check.Details["relayCredentialSynced"])
	}
}

func TestRunWarnsWhenRelayCredentialIsNotRegisteredYet(t *testing.T) {
	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/search/metadata":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"assets": map[string]any{
					"items":    []map[string]any{},
					"total":    0,
					"nextPage": nil,
				},
			})
		default:
			t.Fatalf("unexpected datasource path %s", r.URL.Path)
		}
	}))
	defer datasourceServer.Close()

	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			t.Fatalf("unexpected relay server path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service": "timich-server",
			"version": "test-server",
		})
	}))
	defer relayServer.Close()

	cfg := agentconfig.ResolvedConfig{
		Config: agentconfig.Config{
			Hosted: agentconfig.RemoteBrowsingConfig{
				Enabled:   true,
				ServerURL: relayServer.URL,
			},
			ControlPlaneAddress: "http://127.0.0.1:1",
			Datasources: []agentconfig.DatasourceConfig{{
				Name:        "Home Immich",
				Kind:        "immich",
				URL:         datasourceServer.URL,
				AccessToken: "test-token",
			}},
		},
	}

	report := NewService("test-agent-version", "agent-home", "relay-key", "private-key", cfg, catalog.NewService(cfg.Datasources), RelayRegistrationState{
		CredentialSynced: false,
		Ready:            false,
		BlockedBy:        []string{"paired device"},
	}).Run(context.Background())
	if report.Status != StatusWarning {
		t.Fatalf("report status = %q, want %q", report.Status, StatusWarning)
	}
	relayConnection := report.Checks[3]
	if relayConnection.Status != StatusWarning {
		t.Fatalf("relay_connection status = %q, want %q", relayConnection.Status, StatusWarning)
	}
	if !strings.Contains(relayConnection.Summary, "setup is not complete") {
		t.Fatalf("relay_connection summary = %q, want setup-incomplete message", relayConnection.Summary)
	}
	if !strings.Contains(relayConnection.Remediation, "paired device") {
		t.Fatalf("relay_connection remediation = %q, want paired device blocker", relayConnection.Remediation)
	}
	if _, ok := relayConnection.Details["ack"]; ok {
		t.Fatalf("relay_connection details included ack even though the probe should not run: %#v", relayConnection.Details)
	}
}

func TestRelayConnectionCheckExplainsLegacyProductionTarget(t *testing.T) {
	cfg := agentconfig.ResolvedConfig{
		Config: agentconfig.Config{
			Hosted: agentconfig.RemoteBrowsingConfig{
				Enabled:   true,
				ServerURL: agentconfig.DefaultRemoteBrowsingServerURL,
			},
			ControlPlaneAddress: agentconfig.DefaultRemoteBrowsingServerURL,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	check := NewService("test-agent-version", "agent-home", "relay-key", "private-key", cfg, catalog.NewService(nil), RelayRegistrationState{
		CredentialSynced: true,
		Ready:            true,
	}).runRelayConnectionCheck(ctx)
	if check.Status != StatusFailed {
		t.Fatalf("relay_connection status = %q, want %q", check.Status, StatusFailed)
	}
	if !strings.Contains(check.Remediation, agentconfig.DefaultRelayConnectionAddress) {
		t.Fatalf("relay_connection remediation = %q, want production control-plane address", check.Remediation)
	}
}

func startControlPlaneServer(t *testing.T) (string, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&grpc.ServiceDesc{
		ServiceName: contractscontrolplane.ServiceFullName(),
		HandlerType: (*compatibilityProbeControlPlaneAPI)(nil),
		Streams: []grpc.StreamDesc{
			{
				StreamName:    "OpenControlStream",
				Handler:       openCompatibilityProbeControlStream,
				ServerStreams: true,
				ClientStreams: true,
			},
		},
	}, compatibilityProbeControlPlaneServer{})
	go func() {
		_ = grpcServer.Serve(listener)
	}()

	return "http://" + listener.Addr().String(), func() {
		done := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			grpcServer.Stop()
		}
		_ = listener.Close()
	}
}

type compatibilityProbeControlPlaneAPI interface {
	OpenControlStream(grpc.ServerStream) error
}

type compatibilityProbeControlPlaneServer struct{}

func (compatibilityProbeControlPlaneServer) OpenControlStream(stream grpc.ServerStream) error {
	message, err := contractscontrolplane.NewMessage("AgentEvent")
	if err != nil {
		return err
	}
	if err := stream.RecvMsg(message); err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}

	command, err := contractscontrolplane.NewMessage("ServerCommand")
	if err != nil {
		return err
	}
	command.Set(contractscontrolplane.FieldByName(command, "command_id"), protoreflect.ValueOfString("cmd-1"))
	ack, err := contractscontrolplane.NewMessage("ServerAck")
	if err != nil {
		return err
	}
	ack.Set(contractscontrolplane.FieldByName(ack, "message"), protoreflect.ValueOfString("hello:test:no_preview_service"))
	command.Set(contractscontrolplane.FieldByName(command, "ack"), protoreflect.ValueOfMessage(ack.ProtoReflect()))
	return stream.SendMsg(command)
}

func openCompatibilityProbeControlStream(_ any, stream grpc.ServerStream) error {
	return compatibilityProbeControlPlaneServer{}.OpenControlStream(stream)
}
