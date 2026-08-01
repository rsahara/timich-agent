package semanticruntimehelper

import (
	"encoding/json"
	"errors"
	"strings"
)

const (
	ErrorClassAssetInput         = "asset_input"
	ErrorClassRuntimeUnavailable = "runtime_unavailable"
)

type ErrorResponse struct {
	Error      string `json:"error"`
	ErrorClass string `json:"errorClass"`
}

type ClassifiedError struct {
	Class   string
	Message string
}

func (e *ClassifiedError) Error() string {
	if e == nil {
		return ""
	}
	return strings.TrimSpace(e.Message)
}

func NewClassifiedError(class string, message string) error {
	return &ClassifiedError{Class: strings.TrimSpace(class), Message: strings.TrimSpace(message)}
}

func ErrorClass(err error) string {
	var classified *ClassifiedError
	if errors.As(err, &classified) {
		return strings.TrimSpace(classified.Class)
	}
	return ""
}

func DecodeErrorResponse(raw []byte) (ErrorResponse, bool) {
	var response ErrorResponse
	if json.Unmarshal(raw, &response) != nil || strings.TrimSpace(response.Error) == "" {
		return ErrorResponse{}, false
	}
	response.Error = strings.TrimSpace(response.Error)
	response.ErrorClass = strings.TrimSpace(response.ErrorClass)
	return response, true
}
