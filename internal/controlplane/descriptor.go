package controlplane

import (
	contractscontrolplane "github.com/rsahara/timich-agent/packages/contracts/controlplane"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func FileDescriptor() (protoreflect.FileDescriptor, error) {
	return contractscontrolplane.FileDescriptor()
}

func ServiceFullName() string {
	return contractscontrolplane.ServiceFullName()
}

func OpenControlStreamFullMethod() string {
	return contractscontrolplane.OpenControlStreamFullMethod()
}

func UploadFetchResultFullMethod() string {
	return contractscontrolplane.UploadFetchResultFullMethod()
}

func newMessage(messageName protoreflect.Name) (*dynamicpb.Message, error) {
	return contractscontrolplane.NewMessage(messageName)
}

func fieldByName(message protoreflect.Message, fieldName protoreflect.Name) protoreflect.FieldDescriptor {
	return contractscontrolplane.FieldByName(message, fieldName)
}
