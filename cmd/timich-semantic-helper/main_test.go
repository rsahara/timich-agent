package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsahara/timich-agent/internal/semanticruntimehelper"
)

func TestRunCLIVersion(t *testing.T) {
	originalVersion := version
	version = "test-version"
	t.Cleanup(func() { version = originalVersion })

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"version"}, &stdout, &stderr); err != nil {
		t.Fatalf("runCLI() error = %v stderr=%s", err, stderr.String())
	}
	if stdout.String() != "test-version\n" {
		t.Fatalf("stdout = %q, want version line", stdout.String())
	}
}

func TestRunCLIVersionJSON(t *testing.T) {
	originalVersion := version
	originalCommit := commit
	originalBuiltAt := builtAt
	version = "test-version"
	commit = "test-commit"
	builtAt = "2026-06-11T00:00:00Z"
	t.Cleanup(func() {
		version = originalVersion
		commit = originalCommit
		builtAt = originalBuiltAt
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"version-json"}, &stdout, &stderr); err != nil {
		t.Fatalf("runCLI() error = %v stderr=%s", err, stderr.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v output=%s", err, stdout.String())
	}
	if payload["version"] != "test-version" || payload["commit"] != "test-commit" || payload["builtAt"] != "2026-06-11T00:00:00Z" {
		t.Fatalf("payload = %#v, want build info", payload)
	}
}

func TestRunCLIInspectWritesRuntimeStatus(t *testing.T) {
	runtimePath := writeRuntimeLayout(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"inspect", "--runtime-layout", runtimePath}, &stdout, &stderr); err != nil {
		t.Fatalf("runCLI() error = %v stderr=%s", err, stderr.String())
	}
	var payload struct {
		ProtocolVersion int    `json:"protocolVersion"`
		Runtime         string `json:"runtime"`
		ModelID         string `json:"modelId"`
		VectorSpaceID   string `json:"vectorSpaceId"`
		EmbeddingDim    int    `json:"embeddingDim"`
		InputKind       string `json:"inputKind"`
		Loaded          bool   `json:"loaded"`
		CanEmbed        bool   `json:"canEmbed"`
		MessageCode     string `json:"messageCode"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v output=%s", err, stdout.String())
	}
	if payload.ProtocolVersion != 1 ||
		payload.Runtime != "onnxruntime" ||
		payload.ModelID != "timich-multilingual-clip-small" ||
		payload.VectorSpaceID != "timich-multilingual-clip-small/2026.06/d512" ||
		payload.EmbeddingDim != 512 ||
		payload.InputKind != "image" ||
		payload.Loaded ||
		payload.CanEmbed ||
		payload.MessageCode != "semantic_runtime_onnxruntime_unavailable" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestRunCLIInspectRequiresRuntimeLayout(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"inspect"}, &stdout, &stderr); err == nil {
		t.Fatal("runCLI() error = nil, want missing layout error")
	}
}

func TestEmbedImageReportsUnavailableONNXRuntime(t *testing.T) {
	runtimePath := writeRuntimeLayout(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := embedImage([]string{"--runtime-layout", runtimePath, "--content-type", "image/jpeg"}, strings.NewReader("image"), &stdout, &stderr)
	if err == nil || err.Error() != "semantic_runtime_onnxruntime_unavailable" {
		t.Fatalf("embedImage() error = %v, want ONNX unavailable", err)
	}
}

func TestEmbedTextReportsUnavailableONNXRuntime(t *testing.T) {
	runtimePath := writeRuntimeLayout(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := embedText([]string{"--runtime-layout", runtimePath, "--text", "beach"}, &stdout, &stderr)
	if err == nil || err.Error() != "semantic_runtime_onnxruntime_unavailable" {
		t.Fatalf("embedText() error = %v, want ONNX unavailable", err)
	}
}

func TestRunCLIInspectUsesONNXRuntimeServer(t *testing.T) {
	runtimePath := writeRuntimeLayout(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inspect" {
			t.Fatalf("runtime path = %s, want /inspect", r.URL.Path)
		}
		var payload struct {
			RuntimeLayout string         `json:"runtimeLayout"`
			Layout        map[string]any `json:"layout"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode runtime request: %v", err)
		}
		if payload.RuntimeLayout != runtimePath || payload.Layout["modelId"] != "timich-multilingual-clip-small" {
			t.Fatalf("runtime payload = %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocolVersion":1,"runtime":"onnxruntime","modelId":"timich-multilingual-clip-small","vectorSpaceId":"timich-multilingual-clip-small/2026.06/d512","embeddingDim":512,"inputKind":"image","loaded":true,"canEmbed":true}`))
	}))
	defer server.Close()
	t.Setenv("TIMICH_SEMANTIC_ONNX_SERVER_URL", server.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"inspect", "--runtime-layout", runtimePath}, &stdout, &stderr); err != nil {
		t.Fatalf("runCLI() error = %v stderr=%s", err, stderr.String())
	}
	var payload struct {
		Loaded   bool `json:"loaded"`
		CanEmbed bool `json:"canEmbed"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v output=%s", err, stdout.String())
	}
	if !payload.Loaded || !payload.CanEmbed {
		t.Fatalf("payload = %#v, want loaded runtime", payload)
	}
}

func TestRunCLIInspectPrefersVectorSpaceSpecificONNXRuntimeServer(t *testing.T) {
	runtimePath := writeRuntimeLayout(t)
	globalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("global runtime server should not be used")
	}))
	defer globalServer.Close()
	vectorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inspect" {
			t.Fatalf("runtime path = %s, want /inspect", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocolVersion":1,"runtime":"onnxruntime","modelId":"timich-multilingual-clip-small","vectorSpaceId":"timich-multilingual-clip-small/2026.06/d512","embeddingDim":512,"inputKind":"image","loaded":true,"canEmbed":true}`))
	}))
	defer vectorServer.Close()
	t.Setenv("TIMICH_SEMANTIC_ONNX_SERVER_URL", globalServer.URL)
	t.Setenv(semanticruntimehelper.ONNXRuntimeServerEnvKey("timich-multilingual-clip-small", "timich-multilingual-clip-small/2026.06/d512"), vectorServer.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"inspect", "--runtime-layout", runtimePath}, &stdout, &stderr); err != nil {
		t.Fatalf("runCLI() error = %v stderr=%s", err, stderr.String())
	}
	var payload struct {
		Loaded   bool `json:"loaded"`
		CanEmbed bool `json:"canEmbed"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v output=%s", err, stdout.String())
	}
	if !payload.Loaded || !payload.CanEmbed {
		t.Fatalf("payload = %#v, want loaded runtime", payload)
	}
}

func TestRunCLIEmbedTextUsesONNXRuntimeServer(t *testing.T) {
	runtimePath := writeRuntimeLayout(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed-text" {
			t.Fatalf("runtime path = %s, want /embed-text", r.URL.Path)
		}
		var payload struct {
			Text   string         `json:"text"`
			Layout map[string]any `json:"layout"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode runtime request: %v", err)
		}
		if payload.Text != "beach" || payload.Layout["modelId"] != "timich-multilingual-clip-small" {
			t.Fatalf("runtime payload = %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		vector := "[1," + strings.Repeat("0,", 510) + "0]"
		_, _ = w.Write([]byte(`{"protocolVersion":1,"runtime":"onnxruntime","modelId":"timich-multilingual-clip-small","vectorSpaceId":"timich-multilingual-clip-small/2026.06/d512","embeddingDim":512,"inputKind":"image","vector":` + vector + `,"input":"text"}`))
	}))
	defer server.Close()
	t.Setenv("TIMICH_SEMANTIC_ONNX_SERVER_URL", server.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"embed-text", "--runtime-layout", runtimePath, "--text", "beach"}, &stdout, &stderr); err != nil {
		t.Fatalf("runCLI() error = %v stderr=%s", err, stderr.String())
	}
	var payload struct {
		Vector []float32 `json:"vector"`
		Input  string    `json:"input"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v output=%s", err, stdout.String())
	}
	if len(payload.Vector) != 512 || payload.Vector[0] != 1 || payload.Input != "text" {
		t.Fatalf("payload = %#v, want runtime vector", payload)
	}
}

func TestRunCLIEmbedTextRejectsInvalidRuntimeContract(t *testing.T) {
	runtimePath := writeRuntimeLayout(t)
	tests := []struct {
		name     string
		response string
	}{
		{
			name:     "missing protocol",
			response: `{"runtime":"onnxruntime","modelId":"timich-multilingual-clip-small","vectorSpaceId":"timich-multilingual-clip-small/2026.06/d512","embeddingDim":512,"inputKind":"image","vector":[1]}`,
		},
		{
			name:     "zero vector",
			response: `{"protocolVersion":1,"runtime":"onnxruntime","modelId":"timich-multilingual-clip-small","vectorSpaceId":"timich-multilingual-clip-small/2026.06/d512","embeddingDim":512,"inputKind":"image","vector":[` + strings.Repeat("0,", 511) + `0]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.response))
			}))
			defer server.Close()
			t.Setenv("TIMICH_SEMANTIC_ONNX_SERVER_URL", server.URL)

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if err := runCLI([]string{"embed-text", "--runtime-layout", runtimePath, "--text", "beach"}, &stdout, &stderr); err == nil {
				t.Fatal("runCLI() error = nil, want contract rejection")
			}
		})
	}
}

func TestEmbedImagePreservesRuntimeErrorClass(t *testing.T) {
	runtimePath := writeRuntimeLayout(t)
	tests := []struct {
		name       string
		statusCode int
		response   string
		wantClass  string
	}{
		{
			name:       "asset input",
			statusCode: http.StatusUnprocessableEntity,
			response:   `{"error":"invalid image input","errorClass":"asset_input"}`,
			wantClass:  semanticruntimehelper.ErrorClassAssetInput,
		},
		{
			name:       "runtime unavailable",
			statusCode: http.StatusInternalServerError,
			response:   `{"error":"onnx execution failed","errorClass":"runtime_unavailable"}`,
			wantClass:  semanticruntimehelper.ErrorClassRuntimeUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.response))
			}))
			defer server.Close()
			t.Setenv("TIMICH_SEMANTIC_ONNX_SERVER_URL", server.URL)

			_, err := embedImageWithRuntime(runtimePath, "image/jpeg", "preview", []byte("image"))
			if err == nil || semanticruntimehelper.ErrorClass(err) != test.wantClass {
				t.Fatalf("embedImageWithRuntime() error = %v class=%q, want %q", err, semanticruntimehelper.ErrorClass(err), test.wantClass)
			}
		})
	}
}

func TestRunCLIUnknownCommandShowsUsage(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"bogus"}, &stdout, &stderr); err == nil {
		t.Fatal("runCLI() error = nil, want usage error")
	}
	if !strings.Contains(stderr.String(), "timich-semantic-helper inspect") {
		t.Fatalf("stderr = %q, want usage", stderr.String())
	}
}

func writeRuntimeLayout(t *testing.T) string {
	t.Helper()

	runtimePath := t.TempDir()
	for _, dir := range []string{"models", "tokenizer"} {
		if err := os.MkdirAll(filepath.Join(runtimePath, dir), 0o700); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", dir, err)
		}
	}
	files := map[string]string{
		"models/image.onnx":        "image model",
		"models/text.onnx":         "text model",
		"tokenizer/tokenizer.json": `{"model":"test"}`,
		"timich-model.json": `{
  "schemaVersion": 1,
  "product": "timich-semantic-model-pack",
  "modelId": "timich-multilingual-clip-small",
  "vectorSpaceId": "timich-multilingual-clip-small/2026.06/d512",
  "embeddingDim": 512,
  "inputKind": "image",
  "runtime": "onnxruntime",
  "imageModel": "models/image.onnx",
  "textModel": "models/text.onnx",
  "tokenizer": "tokenizer/tokenizer.json"
}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(runtimePath, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}
	return runtimePath
}
