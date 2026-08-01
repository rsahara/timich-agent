package semanticruntimehelper

import (
	"math"
	"testing"
)

func TestValidateEmbeddingResponseRejectsInvalidConsumerContract(t *testing.T) {
	expected := InspectResponse{
		ProtocolVersion: ProtocolVersion,
		Runtime:         "onnxruntime",
		ModelID:         "model",
		VectorSpaceID:   "space",
		EmbeddingDim:    2,
		InputKind:       "image",
	}
	valid := EmbeddingResponse{
		ProtocolVersion: ProtocolVersion,
		Runtime:         expected.Runtime,
		ModelID:         expected.ModelID,
		VectorSpaceID:   expected.VectorSpaceID,
		EmbeddingDim:    expected.EmbeddingDim,
		InputKind:       expected.InputKind,
		Vector:          []float32{1, 0},
	}
	if err := ValidateEmbeddingResponse(valid, expected); err != nil {
		t.Fatalf("ValidateEmbeddingResponse(valid) error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*EmbeddingResponse)
	}{
		{name: "missing protocol", mutate: func(response *EmbeddingResponse) { response.ProtocolVersion = 0 }},
		{name: "zero vector", mutate: func(response *EmbeddingResponse) { response.Vector = []float32{0, 0} }},
		{name: "NaN vector", mutate: func(response *EmbeddingResponse) { response.Vector = []float32{float32(math.NaN()), 1} }},
		{name: "infinite vector", mutate: func(response *EmbeddingResponse) { response.Vector = []float32{float32(math.Inf(1)), 1} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := valid
			response.Vector = append([]float32(nil), valid.Vector...)
			test.mutate(&response)
			if err := ValidateEmbeddingResponse(response, expected); err == nil {
				t.Fatal("ValidateEmbeddingResponse() error = nil")
			}
		})
	}
}
