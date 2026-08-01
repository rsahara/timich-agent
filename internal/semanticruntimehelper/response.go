package semanticruntimehelper

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// EmbeddingResponse is the protocol shared by the runtime server, the bundled
// helper, release smoke tests, and the catalog consumer.
type EmbeddingResponse struct {
	ProtocolVersion int       `json:"protocolVersion"`
	Runtime         string    `json:"runtime"`
	ModelID         string    `json:"modelId"`
	VectorSpaceID   string    `json:"vectorSpaceId"`
	EmbeddingDim    int       `json:"embeddingDim"`
	InputKind       string    `json:"inputKind"`
	Vector          []float32 `json:"vector"`
	Input           string    `json:"input,omitempty"`
}

func ValidateInspectResponse(actual InspectResponse, expected InspectResponse, requireReady bool) error {
	if err := validateResponseIdentity(
		actual.ProtocolVersion,
		actual.Runtime,
		actual.ModelID,
		actual.VectorSpaceID,
		actual.EmbeddingDim,
		actual.InputKind,
		expected,
	); err != nil {
		return fmt.Errorf("semantic runtime inspect response: %w", err)
	}
	if requireReady && (!actual.Loaded || !actual.CanEmbed) {
		return errors.New("semantic runtime inspect response is not loaded and embeddable")
	}
	return nil
}

func ValidateEmbeddingResponse(actual EmbeddingResponse, expected InspectResponse) error {
	if err := validateResponseIdentity(
		actual.ProtocolVersion,
		actual.Runtime,
		actual.ModelID,
		actual.VectorSpaceID,
		actual.EmbeddingDim,
		actual.InputKind,
		expected,
	); err != nil {
		return fmt.Errorf("semantic runtime embedding response: %w", err)
	}
	if len(actual.Vector) != expected.EmbeddingDim {
		return fmt.Errorf("semantic runtime embedding dimension = %d, want %d", len(actual.Vector), expected.EmbeddingDim)
	}
	nonZero := false
	for _, value := range actual.Vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return errors.New("semantic runtime embedding contains a non-finite value")
		}
		if value != 0 {
			nonZero = true
		}
	}
	if !nonZero {
		return errors.New("semantic runtime embedding is a zero vector")
	}
	return nil
}

func validateResponseIdentity(protocolVersion int, runtime string, modelID string, vectorSpaceID string, embeddingDim int, inputKind string, expected InspectResponse) error {
	if protocolVersion != ProtocolVersion {
		return fmt.Errorf("protocolVersion = %d, want %d", protocolVersion, ProtocolVersion)
	}
	if strings.TrimSpace(runtime) != strings.TrimSpace(expected.Runtime) ||
		strings.TrimSpace(modelID) != strings.TrimSpace(expected.ModelID) ||
		strings.TrimSpace(vectorSpaceID) != strings.TrimSpace(expected.VectorSpaceID) ||
		embeddingDim != expected.EmbeddingDim ||
		strings.TrimSpace(inputKind) != strings.TrimSpace(expected.InputKind) {
		return errors.New("identity mismatch")
	}
	return nil
}
