package semanticruntimehelper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectRuntimeLayoutReportsUnavailableONNXRuntime(t *testing.T) {
	runtimePath := writeTestRuntimeLayout(t, nil)

	response, err := InspectRuntimeLayout(runtimePath)
	if err != nil {
		t.Fatalf("InspectRuntimeLayout() error = %v", err)
	}

	if response.ProtocolVersion != ProtocolVersion ||
		response.Runtime != "onnxruntime" ||
		response.ModelID != "timich-multilingual-clip-small" ||
		response.VectorSpaceID != "timich-multilingual-clip-small/2026.06/d512" ||
		response.EmbeddingDim != 512 ||
		response.InputKind != "image" {
		t.Fatalf("response identity = %#v", response)
	}
	if response.Loaded || response.CanEmbed || response.MessageCode != MessageONNXRuntimeUnavailable {
		t.Fatalf("response runtime status = %#v", response)
	}
}

func TestInspectRuntimeLayoutRejectsUnsafeModelPath(t *testing.T) {
	runtimePath := writeTestRuntimeLayout(t, func(layout map[string]any) {
		layout["imageModel"] = "../image.onnx"
	})

	if _, err := InspectRuntimeLayout(runtimePath); err == nil {
		t.Fatal("InspectRuntimeLayout() error = nil, want unsafe path error")
	}
}

func TestInspectRuntimeLayoutRejectsMissingModelFile(t *testing.T) {
	runtimePath := writeTestRuntimeLayout(t, nil)
	if err := os.Remove(filepath.Join(runtimePath, "models", "image.onnx")); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	if _, err := InspectRuntimeLayout(runtimePath); err == nil {
		t.Fatal("InspectRuntimeLayout() error = nil, want missing model file error")
	}
}

func writeTestRuntimeLayout(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()

	runtimePath := t.TempDir()
	for _, dir := range []string{"models", "tokenizer"} {
		if err := os.MkdirAll(filepath.Join(runtimePath, dir), 0o700); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", dir, err)
		}
	}
	for name, content := range map[string]string{
		"models/image.onnx":        "image model",
		"models/text.onnx":         "text model",
		"tokenizer/tokenizer.json": `{"model":"test"}`,
	} {
		if err := os.WriteFile(filepath.Join(runtimePath, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}
	layout := map[string]any{
		"schemaVersion": 1,
		"product":       "timich-semantic-model-pack",
		"modelId":       "timich-multilingual-clip-small",
		"vectorSpaceId": "timich-multilingual-clip-small/2026.06/d512",
		"embeddingDim":  512,
		"inputKind":     "image",
		"runtime":       "onnxruntime",
		"imageModel":    "models/image.onnx",
		"textModel":     "models/text.onnx",
		"tokenizer":     "tokenizer/tokenizer.json",
	}
	if mutate != nil {
		mutate(layout)
	}
	raw, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(runtimePath, layoutFile), raw, 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", layoutFile, err)
	}
	return runtimePath
}
