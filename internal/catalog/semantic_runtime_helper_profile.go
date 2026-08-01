package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/semanticruntimehelper"
)

const semanticRuntimeHelperEmbedTimeout = 30 * time.Second

type semanticRuntimeHelperProfile struct {
	pack        SemanticModelPackStatus
	runtimePath string
	helperPath  string
}

type semanticRuntimeHelperEmbeddingResponse = semanticruntimehelper.EmbeddingResponse

func (p semanticRuntimeHelperProfile) ModelID() string {
	return strings.TrimSpace(p.pack.ID)
}

func (p semanticRuntimeHelperProfile) VectorSpaceID() string {
	return strings.TrimSpace(p.pack.VectorSpaceID)
}

func (p semanticRuntimeHelperProfile) EmbeddingDim() int {
	return p.pack.EmbeddingDim
}

func (p semanticRuntimeHelperProfile) ProfileKind() string {
	return semanticProfileKindModelPack
}

func (p semanticRuntimeHelperProfile) InputKind() string {
	return strings.TrimSpace(p.pack.InputKind)
}

func (p semanticRuntimeHelperProfile) ModelPackStatus() *SemanticModelPackStatus {
	pack := p.pack
	return &pack
}

func (p semanticRuntimeHelperProfile) EmbedSemanticAsset(ctx context.Context, input semanticAssetEmbeddingInput) (semanticEmbeddingResult, error) {
	if p.InputKind() != semanticInputKindImage {
		return semanticEmbeddingResult{}, fmt.Errorf("semantic runtime helper asset embedding input kind %q is not supported", p.InputKind())
	}
	if input.Image == nil || len(input.Image.Bytes) == 0 {
		return semanticEmbeddingResult{}, fmt.Errorf("%w: semantic runtime helper image input is required", ErrSemanticAssetInput)
	}
	args := []string{
		"embed-image",
		"--runtime-layout", p.runtimePath,
		"--content-type", strings.TrimSpace(input.Image.ContentType),
	}
	if source := strings.TrimSpace(input.Image.Source); source != "" {
		args = append(args, "--source", source)
	}
	response, err := p.runEmbeddingCommand(ctx, args, input.Image.Bytes)
	if err != nil {
		return semanticEmbeddingResult{}, err
	}
	inputLabel := strings.TrimSpace(response.Input)
	if inputLabel == "" {
		inputLabel = strings.TrimSpace(input.Image.Source)
	}
	return semanticEmbeddingResult{
		Vector: response.Vector,
		Input:  inputLabel,
	}, nil
}

func (p semanticRuntimeHelperProfile) EmbedText(ctx context.Context, text string) ([]float32, error) {
	response, err := p.runEmbeddingCommand(ctx, []string{"embed-text", "--runtime-layout", p.runtimePath, "--text", text}, nil)
	if err != nil {
		return nil, err
	}
	return response.Vector, nil
}

func (p semanticRuntimeHelperProfile) runEmbeddingCommand(ctx context.Context, args []string, stdin []byte) (semanticRuntimeHelperEmbeddingResponse, error) {
	helperPath := strings.TrimSpace(p.helperPath)
	if helperPath == "" {
		return semanticRuntimeHelperEmbeddingResponse{}, errors.New("semantic runtime helper path is required")
	}
	ctx, cancel := context.WithTimeout(ctx, semanticRuntimeHelperEmbedTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, helperPath, args...)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	raw, err := command.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if response, ok := semanticruntimehelper.DecodeErrorResponse(exitErr.Stderr); ok {
				switch response.ErrorClass {
				case semanticruntimehelper.ErrorClassAssetInput:
					return semanticRuntimeHelperEmbeddingResponse{}, fmt.Errorf("%w: %s", ErrSemanticAssetInput, response.Error)
				case semanticruntimehelper.ErrorClassRuntimeUnavailable:
					return semanticRuntimeHelperEmbeddingResponse{}, fmt.Errorf("%w: %s", ErrSemanticRuntimeUnavailable, response.Error)
				}
			}
		}
		return semanticRuntimeHelperEmbeddingResponse{}, fmt.Errorf("semantic runtime helper %s: %w", args[0], err)
	}
	var response semanticRuntimeHelperEmbeddingResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return semanticRuntimeHelperEmbeddingResponse{}, fmt.Errorf("decode semantic runtime helper %s response: %w", args[0], err)
	}
	if err := p.validateEmbeddingResponse(response); err != nil {
		return semanticRuntimeHelperEmbeddingResponse{}, err
	}
	response.Vector = normalizeSemanticVector(response.Vector)
	return response, nil
}

func (p semanticRuntimeHelperProfile) validateEmbeddingResponse(response semanticRuntimeHelperEmbeddingResponse) error {
	return semanticruntimehelper.ValidateEmbeddingResponse(response, semanticruntimehelper.InspectResponse{
		ProtocolVersion: semanticruntimehelper.ProtocolVersion,
		Runtime:         strings.TrimSpace(p.pack.Runtime),
		ModelID:         p.ModelID(),
		VectorSpaceID:   p.VectorSpaceID(),
		EmbeddingDim:    p.EmbeddingDim(),
		InputKind:       p.InputKind(),
	})
}
