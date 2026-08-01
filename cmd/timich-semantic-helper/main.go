package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/semanticmanifest"
	"github.com/rsahara/timich-agent/internal/semanticruntimehelper"
)

var (
	version = "dev"
	commit  = "unknown"
	builtAt = "unknown"
)

const (
	onnxRuntimeServerInspectTimeout = 4 * time.Second
	onnxRuntimeServerEmbedTimeout   = 30 * time.Second
)

func main() {
	if err := runCLI(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_ = json.NewEncoder(os.Stderr).Encode(semanticruntimehelper.ErrorResponse{
			Error:      err.Error(),
			ErrorClass: semanticruntimehelper.ErrorClass(err),
		})
		os.Exit(1)
	}
}

func runCLI(args []string, stdout io.Writer, stderr io.Writer) error {
	if len(args) == 0 {
		return usage(stderr)
	}

	switch args[0] {
	case "inspect":
		return inspect(args[1:], stdout, stderr)
	case "embed-image":
		return embedImage(args[1:], os.Stdin, stdout, stderr)
	case "embed-text":
		return embedText(args[1:], stdout, stderr)
	case "validate-manifest":
		return validateManifest(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "version-json":
		return writeVersionJSON(stdout)
	default:
		return usage(stderr)
	}
}

func validateManifest(args []string, stdout io.Writer, stderr io.Writer) error {
	flags := flag.NewFlagSet("timich-semantic-helper validate-manifest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "path to semantic-models.json")
	requiredPlatform := flags.String("required-platform", "", "platform that recommended artifacts must support")
	requireRecommendedModel := flags.Bool("require-recommended-model", false, "require an explicit recommended model")
	requireRecommendedRuntimePack := flags.Bool("require-recommended-runtime-pack", false, "require an explicit recommended runtime pack")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected validate-manifest argument %q", flags.Arg(0))
	}
	if strings.TrimSpace(*manifestPath) == "" {
		return errors.New("manifest path is required")
	}
	raw, err := os.ReadFile(*manifestPath)
	if err != nil {
		return fmt.Errorf("read semantic model manifest: %w", err)
	}
	var manifest semanticmanifest.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("decode semantic model manifest: %w", err)
	}
	if err := semanticmanifest.Validate(&manifest, semanticmanifest.ValidationOptions{
		RequiredPlatform:              *requiredPlatform,
		RequireRecommendedModel:       *requireRecommendedModel,
		RequireRecommendedRuntimePack: *requireRecommendedRuntimePack,
	}); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"status":                 "ok",
		"requiredPlatform":       strings.TrimSpace(*requiredPlatform),
		"recommended":            manifest.Recommended,
		"recommendedRuntimePack": manifest.RecommendedRuntimePack,
	})
}

func inspect(args []string, stdout io.Writer, stderr io.Writer) error {
	flags := flag.NewFlagSet("timich-semantic-helper inspect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	runtimeLayout := flags.String("runtime-layout", "", "path to an installed semantic model runtime layout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected inspect argument %q", flags.Arg(0))
	}

	response, err := inspectRuntime(*runtimeLayout)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	return encoder.Encode(response)
}

func embedImage(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	flags := flag.NewFlagSet("timich-semantic-helper embed-image", flag.ContinueOnError)
	flags.SetOutput(stderr)
	runtimeLayout := flags.String("runtime-layout", "", "path to an installed semantic model runtime layout")
	contentType := flags.String("content-type", "", "image content type")
	source := flags.String("source", "", "optional image input source label")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected embed-image argument %q", flags.Arg(0))
	}
	if strings.TrimSpace(*contentType) == "" {
		return fmt.Errorf("content type is required")
	}
	imageBytes, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read image input: %w", err)
	}
	response, err := embedImageWithRuntime(*runtimeLayout, *contentType, *source, imageBytes)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(response)
}

func embedText(args []string, stdout io.Writer, stderr io.Writer) error {
	flags := flag.NewFlagSet("timich-semantic-helper embed-text", flag.ContinueOnError)
	flags.SetOutput(stderr)
	runtimeLayout := flags.String("runtime-layout", "", "path to an installed semantic model runtime layout")
	text := flags.String("text", "", "text query to embed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected embed-text argument %q", flags.Arg(0))
	}
	if strings.TrimSpace(*text) == "" {
		return fmt.Errorf("text is required")
	}
	response, err := embedTextWithRuntime(*runtimeLayout, *text)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(response)
}

func inspectRuntime(runtimeLayout string) (semanticruntimehelper.InspectResponse, error) {
	response, err := semanticruntimehelper.InspectRuntimeLayout(runtimeLayout)
	if err != nil {
		return semanticruntimehelper.InspectResponse{}, err
	}
	if strings.TrimSpace(response.Runtime) != "onnxruntime" {
		return response, nil
	}
	serverURL := onnxRuntimeServerURL(response.ModelID, response.VectorSpaceID)
	if serverURL == "" {
		return response, nil
	}
	payload, err := runtimeServerPayload(runtimeLayout, map[string]any{})
	if err != nil {
		return semanticruntimehelper.InspectResponse{}, err
	}
	serverResponse, err := postRuntimeServerJSON[semanticruntimehelper.InspectResponse](serverURL, "/inspect", payload, onnxRuntimeServerInspectTimeout)
	if err != nil {
		response.MessageCode = semanticruntimehelper.MessageONNXServerUnavailable
		response.Loaded = false
		response.CanEmbed = false
		return response, nil
	}
	if err := semanticruntimehelper.ValidateInspectResponse(serverResponse, response, true); err != nil {
		return semanticruntimehelper.InspectResponse{}, err
	}
	return serverResponse, nil
}

type runtimeEmbeddingResponse = semanticruntimehelper.EmbeddingResponse

func embedTextWithRuntime(runtimeLayout string, text string) (runtimeEmbeddingResponse, error) {
	identity, err := semanticruntimehelper.InspectRuntimeLayout(runtimeLayout)
	if err != nil {
		return runtimeEmbeddingResponse{}, err
	}
	serverURL := onnxRuntimeServerURL(identity.ModelID, identity.VectorSpaceID)
	if serverURL == "" {
		return runtimeEmbeddingResponse{}, errors.New(semanticruntimehelper.MessageONNXRuntimeUnavailable)
	}
	payload, err := runtimeServerPayload(runtimeLayout, map[string]any{
		"text": strings.TrimSpace(text),
	})
	if err != nil {
		return runtimeEmbeddingResponse{}, err
	}
	response, err := postRuntimeServerJSON[runtimeEmbeddingResponse](serverURL, "/embed-text", payload, onnxRuntimeServerEmbedTimeout)
	if err != nil {
		return runtimeEmbeddingResponse{}, err
	}
	if err := semanticruntimehelper.ValidateEmbeddingResponse(response, identity); err != nil {
		return runtimeEmbeddingResponse{}, err
	}
	return response, nil
}

func embedImageWithRuntime(runtimeLayout string, contentType string, source string, imageBytes []byte) (runtimeEmbeddingResponse, error) {
	identity, err := semanticruntimehelper.InspectRuntimeLayout(runtimeLayout)
	if err != nil {
		return runtimeEmbeddingResponse{}, err
	}
	serverURL := onnxRuntimeServerURL(identity.ModelID, identity.VectorSpaceID)
	if serverURL == "" {
		return runtimeEmbeddingResponse{}, errors.New(semanticruntimehelper.MessageONNXRuntimeUnavailable)
	}
	payload, err := runtimeServerPayload(runtimeLayout, map[string]any{
		"contentType": strings.TrimSpace(contentType),
		"source":      strings.TrimSpace(source),
		"imageBase64": base64.StdEncoding.EncodeToString(imageBytes),
	})
	if err != nil {
		return runtimeEmbeddingResponse{}, err
	}
	response, err := postRuntimeServerJSON[runtimeEmbeddingResponse](serverURL, "/embed-image", payload, onnxRuntimeServerEmbedTimeout)
	if err != nil {
		return runtimeEmbeddingResponse{}, err
	}
	if err := semanticruntimehelper.ValidateEmbeddingResponse(response, identity); err != nil {
		return runtimeEmbeddingResponse{}, err
	}
	return response, nil
}

func runtimeServerPayload(runtimeLayout string, extra map[string]any) (map[string]any, error) {
	raw, err := os.ReadFile(filepath.Join(strings.TrimSpace(runtimeLayout), "timich-model.json"))
	if err != nil {
		return nil, fmt.Errorf("read timich-model.json: %w", err)
	}
	var layout map[string]any
	if err := json.Unmarshal(raw, &layout); err != nil {
		return nil, fmt.Errorf("decode timich-model.json: %w", err)
	}
	payload := map[string]any{
		"layout":        layout,
		"runtimeLayout": strings.TrimSpace(runtimeLayout),
	}
	for key, value := range extra {
		payload[key] = value
	}
	return payload, nil
}

func postRuntimeServerJSON[T any](serverURL string, path string, payload map[string]any, timeout time.Duration) (T, error) {
	var zero T
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(serverURL), "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return zero, fmt.Errorf("invalid ONNX runtime server URL")
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	raw, err := json.Marshal(payload)
	if err != nil {
		return zero, fmt.Errorf("marshal ONNX runtime request: %w", err)
	}
	client := http.Client{Timeout: timeout}
	response, err := client.Post(base.String(), "application/json", bytes.NewReader(raw))
	if err != nil {
		return zero, fmt.Errorf("post ONNX runtime %s: %w", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 64*1024))
		if readErr != nil {
			return zero, semanticruntimehelper.NewClassifiedError(
				semanticruntimehelper.ErrorClassRuntimeUnavailable,
				fmt.Sprintf("read ONNX runtime %s error response: %v", path, readErr),
			)
		}
		if runtimeError, ok := semanticruntimehelper.DecodeErrorResponse(body); ok {
			class := runtimeError.ErrorClass
			if class == "" {
				class = semanticruntimehelper.ErrorClassRuntimeUnavailable
			}
			return zero, semanticruntimehelper.NewClassifiedError(class, runtimeError.Error)
		}
		return zero, semanticruntimehelper.NewClassifiedError(
			semanticruntimehelper.ErrorClassRuntimeUnavailable,
			fmt.Sprintf("ONNX runtime %s returned status %d", path, response.StatusCode),
		)
	}
	var decoded T
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return zero, fmt.Errorf("decode ONNX runtime %s response: %w", path, err)
	}
	return decoded, nil
}

func onnxRuntimeServerURL(modelID string, vectorSpaceID string) string {
	if modelID = strings.TrimSpace(modelID); modelID != "" && strings.TrimSpace(vectorSpaceID) != "" {
		key := semanticruntimehelper.ONNXRuntimeServerEnvKey(modelID, vectorSpaceID)
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	if modelID = strings.TrimSpace(modelID); modelID != "" {
		key := semanticruntimehelper.ONNXRuntimeServerEnvKey(modelID, "")
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	if value := strings.TrimSpace(os.Getenv("TIMICH_SEMANTIC_ONNX_SERVER_URL")); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("TIMICH_ONNX_SERVER_URL"))
}

func writeVersionJSON(stdout io.Writer) error {
	payload := struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		BuiltAt string `json:"builtAt"`
	}{
		Version: version,
		Commit:  commit,
		BuiltAt: builtAt,
	}
	encoder := json.NewEncoder(stdout)
	return encoder.Encode(payload)
}

func usage(stderr io.Writer) error {
	fmt.Fprintln(stderr, strings.TrimSpace(`
Usage:
  timich-semantic-helper inspect --runtime-layout PATH
  timich-semantic-helper embed-image --runtime-layout PATH --content-type TYPE [--source LABEL] < image
  timich-semantic-helper embed-text --runtime-layout PATH --text TEXT
  timich-semantic-helper validate-manifest --manifest PATH [--required-platform PLATFORM] [--require-recommended-model] [--require-recommended-runtime-pack]
  timich-semantic-helper version
  timich-semantic-helper version-json
`))
	return fmt.Errorf("unknown or missing command")
}
