package controlplane

import (
	"fmt"
	"strings"

	contractscontrolplane "github.com/rsahara/timich-agent/packages/contracts/controlplane"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

type Header = contractscontrolplane.Header
type RelayRequest = contractscontrolplane.RelayRequest
type RelayResponse = contractscontrolplane.RelayResponse

func newHelloEvent(agentID string, version string) (*dynamicpb.Message, error) {
	event, err := newMessage("AgentEvent")
	if err != nil {
		return nil, err
	}
	event.Set(fieldByName(event, "agent_id"), protoreflect.ValueOfString(strings.TrimSpace(agentID)))

	hello, err := newMessage("AgentHello")
	if err != nil {
		return nil, err
	}
	hello.Set(fieldByName(hello, "version"), protoreflect.ValueOfString(strings.TrimSpace(version)))
	event.Set(fieldByName(event, "hello"), protoreflect.ValueOfMessage(hello.ProtoReflect()))
	return event, nil
}

func newHeartbeatEvent(agentID string, state string) (*dynamicpb.Message, error) {
	event, err := newMessage("AgentEvent")
	if err != nil {
		return nil, err
	}
	event.Set(fieldByName(event, "agent_id"), protoreflect.ValueOfString(strings.TrimSpace(agentID)))

	heartbeat, err := newMessage("AgentHeartbeat")
	if err != nil {
		return nil, err
	}
	heartbeat.Set(fieldByName(heartbeat, "state"), protoreflect.ValueOfString(strings.TrimSpace(state)))
	event.Set(fieldByName(event, "heartbeat"), protoreflect.ValueOfMessage(heartbeat.ProtoReflect()))
	return event, nil
}

func parseServerCommand(command *dynamicpb.Message) (commandID string, ackMessage string, relayRequest *RelayRequest, err error) {
	commandID = strings.TrimSpace(stringField(command, "command_id"))
	payloadOneof := command.Descriptor().Oneofs().ByName("payload")
	payloadField := command.WhichOneof(payloadOneof)
	if payloadField == nil {
		return commandID, "", nil, nil
	}

	payload := command.Get(payloadField).Message()
	switch payloadField.Name() {
	case "ack":
		return commandID, stringFieldFromReflect(payload, "message"), nil, nil
	case "relay_fetch":
		request := &RelayRequest{
			FetchID:     strings.TrimSpace(stringFieldFromReflect(payload, "fetch_id")),
			Method:      strings.TrimSpace(stringFieldFromReflect(payload, "method")),
			Path:        strings.TrimSpace(stringFieldFromReflect(payload, "path")),
			Headers:     contractscontrolplane.ParseHeaders(payload),
			Body:        contractscontrolplane.BytesFieldFromReflect(payload, "body"),
			ContentType: strings.TrimSpace(contractscontrolplane.StringFieldFromReflect(payload, "content_type")),
		}
		return commandID, "", request, nil
	default:
		return commandID, "", nil, fmt.Errorf("unsupported server command payload %q", payloadField.Name())
	}
}

func newUploadRequest(result RelayResponse) (*dynamicpb.Message, error) {
	request, err := newMessage("FetchResultUploadRequest")
	if err != nil {
		return nil, err
	}
	request.Set(fieldByName(request, "fetch_id"), protoreflect.ValueOfString(strings.TrimSpace(result.FetchID)))
	request.Set(fieldByName(request, "status_code"), protoreflect.ValueOfInt32(int32(result.StatusCode)))
	request.Set(fieldByName(request, "body"), protoreflect.ValueOfBytes(result.Body))
	request.Set(fieldByName(request, "content_type"), protoreflect.ValueOfString(strings.TrimSpace(result.ContentType)))
	setHeaders(request, result.Headers)
	return request, nil
}

func setHeaders(message *dynamicpb.Message, headers []Header) {
	field := fieldByName(message, "headers")
	if field == nil {
		return
	}

	list := message.NewField(field).List()
	for _, header := range headers {
		headerMessage, err := newMessage("Header")
		if err != nil {
			continue
		}
		headerMessage.Set(fieldByName(headerMessage, "name"), protoreflect.ValueOfString(strings.TrimSpace(header.Name)))
		headerMessage.Set(fieldByName(headerMessage, "value"), protoreflect.ValueOfString(header.Value))
		list.Append(protoreflect.ValueOfMessage(headerMessage.ProtoReflect()))
	}
	message.Set(field, protoreflect.ValueOfList(list))
}

func stringField(message *dynamicpb.Message, fieldName protoreflect.Name) string {
	return contractscontrolplane.StringFieldFromReflect(message.ProtoReflect(), fieldName)
}

func stringFieldFromReflect(message protoreflect.Message, fieldName protoreflect.Name) string {
	return contractscontrolplane.StringFieldFromReflect(message, fieldName)
}

func bytesFieldFromReflect(message protoreflect.Message, fieldName protoreflect.Name) []byte {
	return contractscontrolplane.BytesFieldFromReflect(message, fieldName)
}
