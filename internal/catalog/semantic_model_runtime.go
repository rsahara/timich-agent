package catalog

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/semanticruntimehelper"
)

const semanticRuntimeHelperTimeout = 15 * time.Second

func semanticInstalledModelRuntimeStatus(pack SemanticModelPackStatus, artifactPath string, runtimePath string, helperPath string) *SemanticModelRuntimeStatus {
	return semanticInstalledModelRuntimeStatusWithContext(context.Background(), pack, artifactPath, runtimePath, helperPath)
}

func semanticInstalledModelRuntimeStatusWithContext(ctx context.Context, pack SemanticModelPackStatus, artifactPath string, runtimePath string, helperPath string) *SemanticModelRuntimeStatus {
	runtime := strings.TrimSpace(pack.Runtime)
	status := &SemanticModelRuntimeStatus{
		Status:         semanticRuntimeStatusBlocked,
		Runtime:        runtime,
		ArtifactFormat: semanticModelArtifactFormat(pack),
		Loaded:         false,
		CanEmbed:       false,
	}
	if semanticRuntimeUsesHelper(runtime) {
		status.Loader = runtime
	}
	if strings.TrimSpace(artifactPath) == "" {
		status.ArtifactStatus = semanticRuntimeArtifactMissing
		status.MessageCode = semanticRuntimeMessageArtifactMissing
		return status
	}
	if _, err := os.Stat(artifactPath); err != nil {
		status.ArtifactStatus = semanticRuntimeArtifactMissing
		status.MessageCode = semanticRuntimeMessageArtifactMissing
		return status
	}
	status.ArtifactStatus = semanticRuntimeArtifactAvailable
	status.LayoutStatus = semanticModelRuntimeLayoutStatus(pack, runtimePath)
	switch status.LayoutStatus {
	case semanticRuntimeLayoutReady:
	case semanticRuntimeLayoutMissing:
		status.MessageCode = semanticRuntimeMessageLayoutMissing
		return status
	case semanticRuntimeLayoutInvalid:
		status.MessageCode = semanticRuntimeMessageLayoutInvalid
		return status
	case semanticRuntimeLayoutUnsupported:
		status.MessageCode = semanticRuntimeMessageLayoutUnsupported
		return status
	}

	switch runtime {
	case semanticRuntimeLoaderONNXRuntime, semanticRuntimeLoaderSentenceCLIP, semanticRuntimeLoaderTransformersSigLIP2:
		applySemanticRuntimeHelperStatusWithContext(ctx, status, pack, runtimePath, helperPath)
	default:
		status.MessageCode = semanticRuntimeMessageUnsupported
	}
	return status
}

func semanticRuntimeUsesHelper(runtime string) bool {
	switch strings.TrimSpace(runtime) {
	case semanticRuntimeLoaderONNXRuntime, semanticRuntimeLoaderSentenceCLIP, semanticRuntimeLoaderTransformersSigLIP2:
		return true
	default:
		return false
	}
}

type semanticRuntimeHelperResponse = semanticruntimehelper.InspectResponse

func applySemanticRuntimeHelperStatus(status *SemanticModelRuntimeStatus, pack SemanticModelPackStatus, runtimePath string, helperPath string) {
	applySemanticRuntimeHelperStatusWithContext(context.Background(), status, pack, runtimePath, helperPath)
}

func applySemanticRuntimeHelperStatusWithContext(ctx context.Context, status *SemanticModelRuntimeStatus, pack SemanticModelPackStatus, runtimePath string, helperPath string) {
	helperPath = strings.TrimSpace(helperPath)
	if helperPath == "" {
		status.HelperStatus = semanticRuntimeHelperMissing
		status.MessageCode = semanticRuntimeMessageHelperMissing
		return
	}
	if info, err := os.Stat(helperPath); err != nil || info.IsDir() {
		status.HelperStatus = semanticRuntimeHelperMissing
		status.MessageCode = semanticRuntimeMessageHelperMissing
		return
	}

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, semanticRuntimeHelperTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, helperPath, "inspect", "--runtime-layout", runtimePath)
	configureHelperProcessGroup(command)
	command.Cancel = func() error {
		return killHelperProcessGroup(command)
	}
	raw, err := command.Output()
	if err != nil {
		status.HelperStatus = semanticRuntimeHelperFailed
		status.MessageCode = semanticRuntimeMessageHelperFailed
		return
	}
	var response semanticRuntimeHelperResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		status.HelperStatus = semanticRuntimeHelperFailed
		status.MessageCode = semanticRuntimeMessageHelperFailed
		return
	}
	status.HelperProtocol = response.ProtocolVersion
	if err := semanticruntimehelper.ValidateInspectResponse(response, semanticruntimehelper.InspectResponse{
		ProtocolVersion: semanticruntimehelper.ProtocolVersion,
		Runtime:         strings.TrimSpace(pack.Runtime),
		ModelID:         strings.TrimSpace(pack.ID),
		VectorSpaceID:   strings.TrimSpace(pack.VectorSpaceID),
		EmbeddingDim:    pack.EmbeddingDim,
		InputKind:       strings.TrimSpace(pack.InputKind),
	}, false); err != nil {
		status.HelperStatus = semanticRuntimeHelperRejected
		status.MessageCode = semanticRuntimeMessageHelperRejected
		return
	}
	if !response.Loaded || !response.CanEmbed {
		status.HelperStatus = semanticRuntimeHelperBlocked
		status.MessageCode = semanticRuntimeMessageHelperBlocked
		if strings.TrimSpace(response.MessageCode) != "" {
			status.MessageCode = strings.TrimSpace(response.MessageCode)
		}
		return
	}
	status.Status = semanticRuntimeStatusLoaded
	status.HelperStatus = semanticRuntimeHelperReady
	status.Loaded = true
	status.CanEmbed = true
	status.MessageCode = semanticRuntimeMessageHelperLoaded
	if strings.TrimSpace(response.MessageCode) != "" {
		status.MessageCode = strings.TrimSpace(response.MessageCode)
	}
}

func semanticModelArtifactFormat(pack SemanticModelPackStatus) string {
	if pack.Artifact == nil {
		return ""
	}
	filename := strings.ToLower(strings.TrimSpace(pack.Artifact.Filename))
	switch {
	case strings.HasSuffix(filename, ".tar.zst"):
		return "tar.zst"
	case strings.HasSuffix(filename, ".onnx"):
		return "onnx"
	case strings.HasSuffix(filename, ".zip"):
		return "zip"
	case strings.HasSuffix(filename, ".tar"):
		return "tar"
	default:
		ext := strings.TrimPrefix(filepath.Ext(filename), ".")
		return ext
	}
}
