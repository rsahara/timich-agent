package controlplane

import (
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

var (
	initDescriptorsOnce sync.Once
	fileDescriptor      protoreflect.FileDescriptor
	initDescriptorsErr  error
)

func FileDescriptor() (protoreflect.FileDescriptor, error) {
	initDescriptorsOnce.Do(func() {
		fileDescriptor, initDescriptorsErr = buildFileDescriptor()
	})
	return fileDescriptor, initDescriptorsErr
}

func ServiceFullName() string {
	return "timich.controlplane.v1.ControlPlane"
}

func OpenControlStreamFullMethod() string {
	return "/" + ServiceFullName() + "/OpenControlStream"
}

func UploadFetchResultFullMethod() string {
	return "/" + ServiceFullName() + "/UploadFetchResult"
}

func NewMessage(messageName protoreflect.Name) (*dynamicpb.Message, error) {
	fileDescriptor, err := FileDescriptor()
	if err != nil {
		return nil, err
	}

	messageDescriptor := fileDescriptor.Messages().ByName(messageName)
	if messageDescriptor == nil {
		return nil, fmt.Errorf("message %q not found", messageName)
	}

	return dynamicpb.NewMessage(messageDescriptor), nil
}

func FieldByName(message protoreflect.Message, fieldName protoreflect.Name) protoreflect.FieldDescriptor {
	return message.Descriptor().Fields().ByName(fieldName)
}

func buildFileDescriptor() (protoreflect.FileDescriptor, error) {
	fileProto := &descriptorpb.FileDescriptorProto{
		Syntax:  proto.String("proto3"),
		Name:    proto.String("controlplane.proto"),
		Package: proto.String("timich.controlplane.v1"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Header"),
				Field: []*descriptorpb.FieldDescriptorProto{
					fieldString("name", 1),
					fieldString("value", 2),
				},
			},
			{
				Name: proto.String("AgentEvent"),
				Field: []*descriptorpb.FieldDescriptorProto{
					fieldString("agent_id", 1),
					fieldMessage("hello", 2, "AgentHello", true, 0),
					fieldMessage("heartbeat", 3, "AgentHeartbeat", true, 0),
				},
				OneofDecl: []*descriptorpb.OneofDescriptorProto{
					{Name: proto.String("payload")},
				},
			},
			{
				Name: proto.String("ServerCommand"),
				Field: []*descriptorpb.FieldDescriptorProto{
					fieldString("command_id", 1),
					fieldMessage("ack", 2, "ServerAck", true, 0),
					fieldMessage("relay_fetch", 3, "RelayFetchRequest", true, 0),
				},
				OneofDecl: []*descriptorpb.OneofDescriptorProto{
					{Name: proto.String("payload")},
				},
			},
			{
				Name: proto.String("AgentHello"),
				Field: []*descriptorpb.FieldDescriptorProto{
					fieldString("version", 1),
				},
			},
			{
				Name: proto.String("AgentHeartbeat"),
				Field: []*descriptorpb.FieldDescriptorProto{
					fieldString("state", 1),
				},
			},
			{
				Name: proto.String("ServerAck"),
				Field: []*descriptorpb.FieldDescriptorProto{
					fieldString("message", 1),
				},
			},
			{
				Name: proto.String("RelayFetchRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					fieldString("fetch_id", 1),
					fieldString("method", 2),
					fieldString("path", 3),
					fieldRepeatedMessage("headers", 4, "Header"),
					fieldBytes("body", 5),
					fieldString("content_type", 6),
					fieldInt64("deadline_unix_millis", 7),
				},
			},
			{
				Name: proto.String("FetchResultUploadRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					fieldString("fetch_id", 1),
					fieldInt32("status_code", 2),
					fieldRepeatedMessage("headers", 3, "Header"),
					fieldBytes("body", 4),
					fieldString("content_type", 5),
				},
			},
			{
				Name: proto.String("FetchResultUploadResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					fieldBool("accepted", 1),
				},
			},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name: proto.String("ControlPlane"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{
						Name:            proto.String("OpenControlStream"),
						InputType:       proto.String(".timich.controlplane.v1.AgentEvent"),
						OutputType:      proto.String(".timich.controlplane.v1.ServerCommand"),
						ClientStreaming: proto.Bool(true),
						ServerStreaming: proto.Bool(true),
					},
					{
						Name:       proto.String("UploadFetchResult"),
						InputType:  proto.String(".timich.controlplane.v1.FetchResultUploadRequest"),
						OutputType: proto.String(".timich.controlplane.v1.FetchResultUploadResponse"),
					},
				},
			},
		},
	}

	return protodesc.NewFile(fileProto, nil)
}

func fieldString(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(number),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
	}
}

func fieldBytes(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(number),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_BYTES.Enum(),
	}
}

func fieldBool(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(number),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
	}
}

func fieldInt32(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(number),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
	}
}

func fieldInt64(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(number),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
	}
}

func fieldMessage(
	name string,
	number int32,
	messageName string,
	inOneof bool,
	oneofIndex int32,
) *descriptorpb.FieldDescriptorProto {
	field := &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(name),
		Number:   proto.Int32(number),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String(".timich.controlplane.v1." + messageName),
	}
	if inOneof {
		field.OneofIndex = proto.Int32(oneofIndex)
	}
	return field
}

func fieldRepeatedMessage(name string, number int32, messageName string) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(name),
		Number:   proto.Int32(number),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String(".timich.controlplane.v1." + messageName),
	}
}
