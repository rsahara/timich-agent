package controlplane

import "google.golang.org/protobuf/reflect/protoreflect"

type Header struct {
	Name  string
	Value string
}

type RelayRequest struct {
	FetchID     string
	Method      string
	Path        string
	Headers     []Header
	Body        []byte
	ContentType string
}

type RelayResponse struct {
	FetchID     string
	StatusCode  int
	Headers     []Header
	Body        []byte
	ContentType string
}

func ParseHeaders(message protoreflect.Message) []Header {
	field := message.Descriptor().Fields().ByName("headers")
	if field == nil || !message.Has(field) {
		return nil
	}

	list := message.Get(field).List()
	headers := make([]Header, 0, list.Len())
	for index := 0; index < list.Len(); index++ {
		entry := list.Get(index).Message()
		headers = append(headers, Header{
			Name:  StringFieldFromReflect(entry, "name"),
			Value: StringFieldFromReflect(entry, "value"),
		})
	}
	return headers
}

func StringFieldFromReflect(message protoreflect.Message, fieldName protoreflect.Name) string {
	field := message.Descriptor().Fields().ByName(fieldName)
	if field == nil || !message.Has(field) {
		return ""
	}
	return message.Get(field).String()
}

func BytesFieldFromReflect(message protoreflect.Message, fieldName protoreflect.Name) []byte {
	field := message.Descriptor().Fields().ByName(fieldName)
	if field == nil || !message.Has(field) {
		return nil
	}
	return append([]byte(nil), message.Get(field).Bytes()...)
}
