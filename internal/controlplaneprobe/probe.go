package controlplaneprobe

import (
	"context"
	"strings"

	contracts "github.com/rsahara/timich-agent/packages/contracts/controlplane"
	"github.com/rsahara/timich-agent/internal/controlplanetransport"
	"google.golang.org/grpc"
)

type TLSConfig = controlplanetransport.TLSConfig

type ProbeInput struct {
	Version    string
	AgentID    string
	KeyID      string
	PrivateKey string
	Target     string
	TLS        TLSConfig
}

type dialContextFunc func(context.Context, string, ...grpc.DialOption) (*grpc.ClientConn, error)

func Probe(ctx context.Context, input ProbeInput) (string, error) {
	return probe(ctx, input, grpc.DialContext)
}

func probe(ctx context.Context, input ProbeInput, dialContext dialContextFunc) (string, error) {
	tokenSigner := controlplanetransport.NewTokenSigner(input.AgentID, input.KeyID, input.PrivateKey, controlplanetransport.DefaultTokenTTL)

	target, dialOption, err := controlplanetransport.DialOptionForTarget(strings.TrimSpace(input.Target), input.TLS, tokenSigner != nil)
	if err != nil {
		return "", err
	}

	dialOptions := []grpc.DialOption{
		grpc.WithBlock(),
		dialOption,
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(controlplanetransport.MaxRelayMessageBytes),
			grpc.MaxCallSendMsgSize(controlplanetransport.MaxRelayMessageBytes),
		),
	}
	if tokenSigner != nil {
		dialOptions = append(dialOptions, grpc.WithPerRPCCredentials(tokenSigner))
	}

	conn, err := dialContext(ctx, target, dialOptions...)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	stream, err := conn.NewStream(ctx, controlplanetransport.ControlStreamDesc, contracts.OpenControlStreamFullMethod())
	if err != nil {
		return "", err
	}

	hello, err := controlplanetransport.NewHelloEvent(strings.TrimSpace(input.AgentID), strings.TrimSpace(input.Version))
	if err != nil {
		return "", err
	}
	if err := stream.SendMsg(hello); err != nil {
		return "", err
	}

	command, err := contracts.NewMessage("ServerCommand")
	if err != nil {
		return "", err
	}
	if err := stream.RecvMsg(command); err != nil {
		return "", err
	}
	ack, err := controlplanetransport.ParseAck(command)
	if err != nil {
		return "", err
	}
	return ack, nil
}
