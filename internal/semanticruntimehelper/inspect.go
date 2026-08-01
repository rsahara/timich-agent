package semanticruntimehelper

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	ProtocolVersion = 1

	layoutFile    = "timich-model.json"
	layoutProduct = "timich-semantic-model-pack"
	layoutVersion = 1

	runtimeONNXRuntime = "onnxruntime"

	MessageONNXRuntimeUnavailable = "semantic_runtime_onnxruntime_unavailable"
	MessageONNXServerUnavailable  = "semantic_runtime_onnx_server_unavailable"
	MessageRuntimeUnsupported     = "semantic_runtime_unsupported"
)

type InspectResponse struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Runtime         string `json:"runtime"`
	ModelID         string `json:"modelId"`
	VectorSpaceID   string `json:"vectorSpaceId"`
	EmbeddingDim    int    `json:"embeddingDim"`
	InputKind       string `json:"inputKind"`
	Loaded          bool   `json:"loaded"`
	CanEmbed        bool   `json:"canEmbed"`
	MessageCode     string `json:"messageCode,omitempty"`
}

type modelPackLayout struct {
	SchemaVersion int    `json:"schemaVersion"`
	Product       string `json:"product"`
	ModelID       string `json:"modelId"`
	VectorSpaceID string `json:"vectorSpaceId"`
	EmbeddingDim  int    `json:"embeddingDim"`
	InputKind     string `json:"inputKind"`
	Runtime       string `json:"runtime"`
	ImageModel    string `json:"imageModel"`
	TextModel     string `json:"textModel"`
	Tokenizer     string `json:"tokenizer"`
}

func InspectRuntimeLayout(runtimePath string) (InspectResponse, error) {
	runtimePath = strings.TrimSpace(runtimePath)
	if runtimePath == "" {
		return InspectResponse{}, errors.New("runtime layout path is required")
	}
	raw, err := os.ReadFile(filepath.Join(runtimePath, layoutFile))
	if err != nil {
		return InspectResponse{}, fmt.Errorf("read %s: %w", layoutFile, err)
	}
	var layout modelPackLayout
	if err := json.Unmarshal(raw, &layout); err != nil {
		return InspectResponse{}, fmt.Errorf("decode %s: %w", layoutFile, err)
	}
	normalizeLayout(&layout)
	if err := validateLayout(runtimePath, layout); err != nil {
		return InspectResponse{}, err
	}

	response := InspectResponse{
		ProtocolVersion: ProtocolVersion,
		Runtime:         layout.Runtime,
		ModelID:         layout.ModelID,
		VectorSpaceID:   layout.VectorSpaceID,
		EmbeddingDim:    layout.EmbeddingDim,
		InputKind:       layout.InputKind,
	}
	switch layout.Runtime {
	case runtimeONNXRuntime:
		response.MessageCode = MessageONNXRuntimeUnavailable
	default:
		response.MessageCode = MessageRuntimeUnsupported
	}
	return response, nil
}

func validateLayout(runtimePath string, layout modelPackLayout) error {
	if layout.SchemaVersion != layoutVersion {
		return fmt.Errorf("%s schemaVersion must be %d", layoutFile, layoutVersion)
	}
	if layout.Product != layoutProduct {
		return fmt.Errorf("%s product is invalid", layoutFile)
	}
	if layout.ModelID == "" {
		return fmt.Errorf("%s modelId is required", layoutFile)
	}
	if layout.VectorSpaceID == "" {
		return fmt.Errorf("%s vectorSpaceId is required", layoutFile)
	}
	if layout.EmbeddingDim <= 0 {
		return fmt.Errorf("%s embeddingDim must be positive", layoutFile)
	}
	if layout.InputKind == "" {
		return fmt.Errorf("%s inputKind is required", layoutFile)
	}
	if layout.Runtime == "" {
		return fmt.Errorf("%s runtime is required", layoutFile)
	}
	for label, value := range map[string]string{
		"imageModel": layout.ImageModel,
		"textModel":  layout.TextModel,
		"tokenizer":  layout.Tokenizer,
	} {
		if err := requireRuntimeFile(runtimePath, label, value); err != nil {
			return err
		}
	}
	return nil
}

func requireRuntimeFile(runtimePath string, label string, value string) error {
	relative, ok := safeRelativePath(value)
	if !ok {
		return fmt.Errorf("%s path is invalid", label)
	}
	info, err := os.Stat(filepath.Join(runtimePath, filepath.FromSlash(relative)))
	if err != nil {
		return fmt.Errorf("%s file is missing: %w", label, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s file is a directory", label)
	}
	return nil
}

func normalizeLayout(layout *modelPackLayout) {
	layout.Product = strings.TrimSpace(layout.Product)
	layout.ModelID = strings.TrimSpace(layout.ModelID)
	layout.VectorSpaceID = strings.TrimSpace(layout.VectorSpaceID)
	layout.InputKind = strings.TrimSpace(layout.InputKind)
	layout.Runtime = strings.TrimSpace(layout.Runtime)
	layout.ImageModel = strings.TrimSpace(layout.ImageModel)
	layout.TextModel = strings.TrimSpace(layout.TextModel)
	layout.Tokenizer = strings.TrimSpace(layout.Tokenizer)
}

func safeRelativePath(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.Contains(trimmed, "\\") {
		return "", false
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || path.IsAbs(cleaned) || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}
