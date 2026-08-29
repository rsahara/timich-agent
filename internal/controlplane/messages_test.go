package controlplane

import (
	"bytes"
	"reflect"
	"testing"

	contractscontrolplane "github.com/rsahara/timich-agent/packages/contracts/controlplane"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestControlPlaneDescriptorContract(t *testing.T) {
	t.Parallel()

	fileDescriptor, err := FileDescriptor()
	if err != nil {
		t.Fatalf("FileDescriptor() error = %v", err)
	}
	if got := string(fileDescriptor.Package()); got != "timich.controlplane.v1" {
		t.Fatalf("FileDescriptor().Package() = %q, want timich.controlplane.v1", got)
	}
	if got := ServiceFullName(); got != "timich.controlplane.v1.ControlPlane" {
		t.Fatalf("ServiceFullName() = %q, want control-plane service name", got)
	}
	if got := OpenControlStreamFullMethod(); got != "/timich.controlplane.v1.ControlPlane/OpenControlStream" {
		t.Fatalf("OpenControlStreamFullMethod() = %q, want full stream method", got)
	}
	if got := UploadFetchResultFullMethod(); got != "/timich.controlplane.v1.ControlPlane/UploadFetchResult" {
		t.Fatalf("UploadFetchResultFullMethod() = %q, want full upload method", got)
	}

	message, err := newMessage("ServerCommand")
	if err != nil {
		t.Fatalf("newMessage(ServerCommand) error = %v", err)
	}
	if fieldByName(message, "command_id") == nil {
		t.Fatal("fieldByName(command_id) = nil, want descriptor")
	}
	if fieldByName(message, "missing") != nil {
		t.Fatal("fieldByName(missing) != nil, want no descriptor")
	}
	if _, err := newMessage("MissingMessage"); err == nil {
		t.Fatal("newMessage(MissingMessage) error = nil, want unknown-message error")
	}
}

func TestNewHelloEventTrimsProtocolFields(t *testing.T) {
	t.Parallel()

	event, err := newHelloEvent("  agent-home  ", "  1.2.3  ")
	if err != nil {
		t.Fatalf("newHelloEvent() error = %v", err)
	}
	if got := stringField(event, "agent_id"); got != "agent-home" {
		t.Fatalf("agent_id = %q, want trimmed identity", got)
	}
	payloadField := event.WhichOneof(event.Descriptor().Oneofs().ByName("payload"))
	if payloadField == nil || payloadField.Name() != "hello" {
		t.Fatalf("payload field = %v, want hello", payloadField)
	}
	hello := event.Get(payloadField).Message()
	if got := stringFieldFromReflect(hello, "version"); got != "1.2.3" {
		t.Fatalf("version = %q, want trimmed version", got)
	}
}

func TestNewHeartbeatEventTrimsProtocolFields(t *testing.T) {
	t.Parallel()

	event, err := newHeartbeatEvent("  agent-home  ", "  online  ")
	if err != nil {
		t.Fatalf("newHeartbeatEvent() error = %v", err)
	}
	if got := stringField(event, "agent_id"); got != "agent-home" {
		t.Fatalf("agent_id = %q, want trimmed identity", got)
	}
	payloadField := event.WhichOneof(event.Descriptor().Oneofs().ByName("payload"))
	if payloadField == nil || payloadField.Name() != "heartbeat" {
		t.Fatalf("payload field = %v, want heartbeat", payloadField)
	}
	heartbeat := event.Get(payloadField).Message()
	if got := stringFieldFromReflect(heartbeat, "state"); got != "online" {
		t.Fatalf("state = %q, want trimmed state", got)
	}
}

func TestParseServerCommandWithoutPayload(t *testing.T) {
	t.Parallel()

	command := mustControlPlaneMessage(t, "ServerCommand")
	setControlPlaneString(command, "command_id", "  command-1  ")

	commandID, ackMessage, relayRequest, err := parseServerCommand(command)
	if err != nil {
		t.Fatalf("parseServerCommand() error = %v", err)
	}
	if commandID != "command-1" || ackMessage != "" || relayRequest != nil {
		t.Fatalf("parseServerCommand() = %q/%q/%+v, want trimmed ID and no payload", commandID, ackMessage, relayRequest)
	}
}

func TestParseServerCommandAck(t *testing.T) {
	t.Parallel()

	command := mustControlPlaneMessage(t, "ServerCommand")
	setControlPlaneString(command, "command_id", "command-2")
	ack := mustControlPlaneMessage(t, "ServerAck")
	setControlPlaneString(ack, "message", "accepted")
	setControlPlaneMessage(command, "ack", ack)

	commandID, ackMessage, relayRequest, err := parseServerCommand(command)
	if err != nil {
		t.Fatalf("parseServerCommand() error = %v", err)
	}
	if commandID != "command-2" || ackMessage != "accepted" || relayRequest != nil {
		t.Fatalf("parseServerCommand() = %q/%q/%+v, want ACK result", commandID, ackMessage, relayRequest)
	}
}

func TestParseServerCommandRelayFetch(t *testing.T) {
	t.Parallel()

	command := mustControlPlaneMessage(t, "ServerCommand")
	setControlPlaneString(command, "command_id", "  command-3  ")
	relay := mustControlPlaneMessage(t, "RelayFetchRequest")
	setControlPlaneString(relay, "fetch_id", "  fetch-1  ")
	setControlPlaneString(relay, "method", "  POST  ")
	setControlPlaneString(relay, "path", "  /v1/assets/search?page=2  ")
	setControlPlaneString(relay, "content_type", "  application/json  ")
	relay.Set(fieldByName(relay, "deadline_unix_millis"), protoreflect.ValueOfInt64(1_787_460_000_123))
	relay.Set(fieldByName(relay, "body"), protoreflect.ValueOfBytes([]byte(`{"page":2}`)))
	setHeaders(relay, []Header{
		{Name: "  X-Test  ", Value: " first "},
		{Name: "X-Test", Value: "second"},
	})
	setControlPlaneMessage(command, "relay_fetch", relay)

	commandID, ackMessage, request, err := parseServerCommand(command)
	if err != nil {
		t.Fatalf("parseServerCommand() error = %v", err)
	}
	want := &RelayRequest{
		FetchID: "fetch-1",
		Method:  "POST",
		Path:    "/v1/assets/search?page=2",
		Headers: []Header{
			{Name: "X-Test", Value: " first "},
			{Name: "X-Test", Value: "second"},
		},
		Body:               []byte(`{"page":2}`),
		ContentType:        "application/json",
		DeadlineUnixMillis: 1_787_460_000_123,
	}
	if commandID != "command-3" || ackMessage != "" || !reflect.DeepEqual(request, want) {
		t.Fatalf("parseServerCommand() = %q/%q/%+v, want %q/%q/%+v", commandID, ackMessage, request, "command-3", "", want)
	}
}

func TestParseServerCommandRejectsUnsupportedPayload(t *testing.T) {
	t.Parallel()

	command := unsupportedServerCommand(t)
	commandID, ackMessage, relayRequest, err := parseServerCommand(command)
	if err == nil {
		t.Fatal("parseServerCommand() error = nil, want unsupported-payload error")
	}
	if commandID != "future-command" || ackMessage != "" || relayRequest != nil {
		t.Fatalf("parseServerCommand() = %q/%q/%+v, want command ID and no decoded payload", commandID, ackMessage, relayRequest)
	}
}

func TestNewUploadRequestMapsRelayResponse(t *testing.T) {
	t.Parallel()

	request, err := newUploadRequest(RelayResponse{
		FetchID:    "  fetch-2  ",
		StatusCode: 206,
		Headers: []Header{
			{Name: "  Content-Type  ", Value: "application/octet-stream"},
			{Name: "Set-Cookie", Value: "one=1"},
			{Name: "Set-Cookie", Value: "two=2"},
		},
		Body:        []byte("response-body"),
		ContentType: "  application/octet-stream  ",
	})
	if err != nil {
		t.Fatalf("newUploadRequest() error = %v", err)
	}
	if got := stringField(request, "fetch_id"); got != "fetch-2" {
		t.Fatalf("fetch_id = %q, want trimmed ID", got)
	}
	if got := request.Get(fieldByName(request, "status_code")).Int(); got != 206 {
		t.Fatalf("status_code = %d, want 206", got)
	}
	if got := contractscontrolplane.ParseHeaders(request.ProtoReflect()); !reflect.DeepEqual(got, []Header{
		{Name: "Content-Type", Value: "application/octet-stream"},
		{Name: "Set-Cookie", Value: "one=1"},
		{Name: "Set-Cookie", Value: "two=2"},
	}) {
		t.Fatalf("headers = %+v, want trimmed names and repeated values", got)
	}
	if got := bytesFieldFromReflect(request.ProtoReflect(), "body"); !bytes.Equal(got, []byte("response-body")) {
		t.Fatalf("body = %q, want response-body", got)
	}
	if got := stringField(request, "content_type"); got != "application/octet-stream" {
		t.Fatalf("content_type = %q, want trimmed type", got)
	}
}

func TestSetHeadersIgnoresMessagesWithoutHeadersField(t *testing.T) {
	t.Parallel()

	header := mustControlPlaneMessage(t, "Header")
	setHeaders(header, []Header{{Name: "X-Test", Value: "value"}})
	if got := stringField(header, "name"); got != "" {
		t.Fatalf("header name = %q after setHeaders(), want unchanged message", got)
	}
}

func mustControlPlaneMessage(t *testing.T, name protoreflect.Name) *dynamicpb.Message {
	t.Helper()

	message, err := newMessage(name)
	if err != nil {
		t.Fatalf("newMessage(%s) error = %v", name, err)
	}
	return message
}

func setControlPlaneString(message *dynamicpb.Message, name protoreflect.Name, value string) {
	message.Set(fieldByName(message, name), protoreflect.ValueOfString(value))
}

func setControlPlaneMessage(parent *dynamicpb.Message, name protoreflect.Name, child *dynamicpb.Message) {
	parent.Set(fieldByName(parent, name), protoreflect.ValueOfMessage(child.ProtoReflect()))
}

func unsupportedServerCommand(t *testing.T) *dynamicpb.Message {
	t.Helper()

	fileDescriptor, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Syntax:  proto.String("proto3"),
		Name:    proto.String("future-controlplane.proto"),
		Package: proto.String("test.controlplane"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("FuturePayload")},
			{
				Name: proto.String("ServerCommand"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:   proto.String("command_id"),
						Number: proto.Int32(1),
						Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					},
					{
						Name:       proto.String("future"),
						Number:     proto.Int32(2),
						Label:      descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						Type:       descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName:   proto.String(".test.controlplane.FuturePayload"),
						OneofIndex: proto.Int32(0),
					},
				},
				OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("payload")}},
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile() error = %v", err)
	}
	commandDescriptor := fileDescriptor.Messages().ByName("ServerCommand")
	payloadDescriptor := fileDescriptor.Messages().ByName("FuturePayload")
	command := dynamicpb.NewMessage(commandDescriptor)
	payload := dynamicpb.NewMessage(payloadDescriptor)
	command.Set(commandDescriptor.Fields().ByName("command_id"), protoreflect.ValueOfString("future-command"))
	command.Set(commandDescriptor.Fields().ByName("future"), protoreflect.ValueOfMessage(payload.ProtoReflect()))
	return command
}
