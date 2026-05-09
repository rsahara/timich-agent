package controlplanetransport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	contracts "github.com/rsahara/timich-agent/packages/contracts/controlplane"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestNewHelloEventTrimsIdentity(t *testing.T) {
	t.Parallel()

	event, err := NewHelloEvent(" agent-home ", " 1.2.3 ")
	if err != nil {
		t.Fatalf("NewHelloEvent() error = %v", err)
	}

	if got := contracts.StringFieldFromReflect(event, "agent_id"); got != "agent-home" {
		t.Fatalf("agent_id = %q, want trimmed identity", got)
	}

	helloField := contracts.FieldByName(event, "hello")
	hello := event.Get(helloField).Message()
	if got := contracts.StringFieldFromReflect(hello, "version"); got != "1.2.3" {
		t.Fatalf("version = %q, want trimmed version", got)
	}
}

func TestParseAck(t *testing.T) {
	t.Parallel()

	command, err := contracts.NewMessage("ServerCommand")
	if err != nil {
		t.Fatalf("NewMessage(ServerCommand) error = %v", err)
	}
	ack, err := contracts.NewMessage("ServerAck")
	if err != nil {
		t.Fatalf("NewMessage(ServerAck) error = %v", err)
	}
	ack.Set(contracts.FieldByName(ack, "message"), protoreflect.ValueOfString(" ready "))
	command.Set(contracts.FieldByName(command, "ack"), protoreflect.ValueOfMessage(ack.ProtoReflect()))

	message, err := ParseAck(command)
	if err != nil {
		t.Fatalf("ParseAck() error = %v", err)
	}
	if message != "ready" {
		t.Fatalf("message = %q, want trimmed ack", message)
	}
}

func TestParseAckRejectsMissingPayload(t *testing.T) {
	t.Parallel()

	command, err := contracts.NewMessage("ServerCommand")
	if err != nil {
		t.Fatalf("NewMessage(ServerCommand) error = %v", err)
	}
	if _, err := ParseAck(command); err == nil {
		t.Fatal("ParseAck() error = nil, want missing payload error")
	}
}

func TestDialOptionForTargetNormalizesTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		target     string
		requireTLS bool
		wantTarget string
		wantError  string
	}{
		{name: "https default port", target: "https://control.example", wantTarget: "control.example:443"},
		{name: "http default port", target: "http://127.0.0.1", wantTarget: "127.0.0.1:80"},
		{name: "raw target", target: "127.0.0.1:8080", wantTarget: "127.0.0.1:8080"},
		{name: "signed token requires https", target: "http://127.0.0.1", requireTLS: true, wantError: "require https"},
		{name: "unsupported scheme", target: "ftp://control.example", wantError: "unsupported control-plane scheme"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			target, _, err := DialOptionForTarget(tc.target, TLSConfig{}, tc.requireTLS)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("DialOptionForTarget() error = %v", err)
			}
			if target != tc.wantTarget {
				t.Fatalf("target = %q, want %q", target, tc.wantTarget)
			}
		})
	}
}

func TestLoadClientTLSConfigAppliesServerName(t *testing.T) {
	t.Parallel()

	cfg, err := LoadClientTLSConfig("default.example", TLSConfig{ServerName: "override.example"})
	if err != nil {
		t.Fatalf("LoadClientTLSConfig() error = %v", err)
	}
	if cfg.ServerName != "override.example" {
		t.Fatalf("ServerName = %q, want override", cfg.ServerName)
	}
}

func TestNewTokenSignerValidatesInputAndProducesMetadata(t *testing.T) {
	t.Parallel()

	if signer := NewTokenSigner(" ", "key-1", "secret", time.Minute); signer != nil {
		t.Fatalf("NewTokenSigner() = %+v, want nil for blank agent ID", signer)
	}

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	signer := NewTokenSigner("agent-home", "key-1", base64.RawStdEncoding.EncodeToString(privateKey), time.Minute)
	if signer == nil {
		t.Fatal("NewTokenSigner() = nil, want signer")
	}
	if !signer.RequireTransportSecurity() {
		t.Fatal("RequireTransportSecurity() = false, want true")
	}

	metadata, err := signer.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata() error = %v", err)
	}
	if value := metadata[contracts.ControlPlaneAuthorizationKey]; !strings.HasPrefix(value, "Bearer ") {
		t.Fatalf("authorization metadata = %q, want bearer token", value)
	}
}
