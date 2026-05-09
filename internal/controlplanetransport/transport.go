package controlplanetransport

import (
	"context"
	"crypto/tls"
	"errors"
	"net/url"
	"strings"
	"time"

	contracts "github.com/rsahara/timich-agent/packages/contracts/controlplane"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	MaxRelayMessageBytes = 4 << 20
	DefaultTokenTTL      = 5 * time.Minute
)

var ControlStreamDesc = &grpc.StreamDesc{
	StreamName:    "OpenControlStream",
	ServerStreams: true,
	ClientStreams: true,
}

type TLSConfig struct {
	ServerName string
}

type TokenSigner struct {
	agentID    string
	keyID      string
	privateKey string
	tokenTTL   time.Duration
}

func NewHelloEvent(agentID string, version string) (*dynamicpb.Message, error) {
	event, err := contracts.NewMessage("AgentEvent")
	if err != nil {
		return nil, err
	}
	event.Set(contracts.FieldByName(event, "agent_id"), protoreflect.ValueOfString(strings.TrimSpace(agentID)))

	hello, err := contracts.NewMessage("AgentHello")
	if err != nil {
		return nil, err
	}
	hello.Set(contracts.FieldByName(hello, "version"), protoreflect.ValueOfString(strings.TrimSpace(version)))
	event.Set(contracts.FieldByName(event, "hello"), protoreflect.ValueOfMessage(hello.ProtoReflect()))
	return event, nil
}

func ParseAck(command *dynamicpb.Message) (string, error) {
	payloadOneof := command.Descriptor().Oneofs().ByName("payload")
	payloadField := command.WhichOneof(payloadOneof)
	if payloadField == nil {
		return "", errors.New("server command payload is missing")
	}
	if payloadField.Name() != "ack" {
		return "", errors.New("server command payload is not ack")
	}

	payload := command.Get(payloadField).Message()
	message := strings.TrimSpace(contracts.StringFieldFromReflect(payload, "message"))
	if message == "" {
		return "", errors.New("server ack message is empty")
	}
	return message, nil
}

func DialOptionForTarget(rawTarget string, tlsSettings TLSConfig, requireTransportSecurity bool) (string, grpc.DialOption, error) {
	target := strings.TrimSpace(rawTarget)
	// gRPC targets may be plain host:port values. Treat every no-"://" value as
	// a raw target, including scheme-only URI forms that are invalid here anyway.
	if !strings.Contains(target, "://") {
		if requireTransportSecurity {
			return "", nil, errors.New("signed control-plane tokens require https transport")
		}
		return target, grpc.WithTransportCredentials(insecure.NewCredentials()), nil
	}

	parsed, err := url.Parse(target)
	if err != nil {
		return "", nil, err
	}

	switch parsed.Scheme {
	case "https":
		host := parsed.Host
		if !strings.Contains(host, ":") {
			host += ":443"
		}
		tlsConfig, err := LoadClientTLSConfig(parsed.Hostname(), tlsSettings)
		if err != nil {
			return "", nil, err
		}
		return host, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), nil
	case "http":
		if requireTransportSecurity {
			return "", nil, errors.New("signed control-plane tokens require https transport")
		}
		host := parsed.Host
		if !strings.Contains(host, ":") {
			host += ":80"
		}
		return host, grpc.WithTransportCredentials(insecure.NewCredentials()), nil
	default:
		return "", nil, errors.New("unsupported control-plane scheme")
	}
}

func LoadClientTLSConfig(defaultServerName string, settings TLSConfig) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: defaultServerName,
	}
	if strings.TrimSpace(settings.ServerName) != "" {
		tlsConfig.ServerName = strings.TrimSpace(settings.ServerName)
	}
	return tlsConfig, nil
}

func NewTokenSigner(agentID string, keyID string, privateKey string, tokenTTL time.Duration) *TokenSigner {
	agentID = strings.TrimSpace(agentID)
	keyID = strings.TrimSpace(keyID)
	privateKey = strings.TrimSpace(privateKey)
	if agentID == "" || keyID == "" || privateKey == "" || tokenTTL <= 0 {
		return nil
	}
	return &TokenSigner{
		agentID:    agentID,
		keyID:      keyID,
		privateKey: privateKey,
		tokenTTL:   tokenTTL,
	}
}

func (s *TokenSigner) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	token, err := contracts.MintControlPlaneToken(s.privateKey, s.keyID, s.agentID, time.Now().UTC(), s.tokenTTL)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		contracts.ControlPlaneAuthorizationKey: "Bearer " + token,
	}, nil
}

func (s *TokenSigner) RequireTransportSecurity() bool {
	return true
}
