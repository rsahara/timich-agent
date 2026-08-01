package catalog

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	semanticModelPackRuntimeDir     = "runtime"
	semanticModelPackLayoutFile     = "timich-model.json"
	semanticModelPackLayoutProduct  = "timich-semantic-model-pack"
	semanticModelPackLayoutVersion  = 1
	semanticModelPackMaxExtractSize = int64(8 << 30)
)

type semanticModelPackLayout struct {
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

func (s *SemanticModelPackStore) prepareRuntimeLayout(pack SemanticModelPackStatus, installDir string, artifactPath string) (string, error) {
	format := semanticModelArtifactFormat(pack)
	if format != "zip" {
		return "", nil
	}
	runtimePath := filepath.Join(installDir, semanticModelPackRuntimeDir)
	if err := os.RemoveAll(runtimePath); err != nil {
		return "", fmt.Errorf("clear semantic model runtime layout: %w", err)
	}
	if err := os.MkdirAll(runtimePath, 0o700); err != nil {
		return "", fmt.Errorf("create semantic model runtime layout: %w", err)
	}
	if err := extractSemanticModelZip(artifactPath, runtimePath); err != nil {
		return "", fmt.Errorf("%w: extract runtime layout: %v", ErrSemanticModelPackInvalid, err)
	}
	if err := validateSemanticModelRuntimeLayout(pack, runtimePath); err != nil {
		return "", fmt.Errorf("%w: %v", ErrSemanticModelPackInvalid, err)
	}
	return runtimePath, nil
}

func extractSemanticModelZip(artifactPath string, destination string) error {
	reader, err := zip.OpenReader(artifactPath)
	if err != nil {
		return err
	}
	defer reader.Close()

	var extractedBytes int64
	for _, entry := range reader.File {
		_, ok := safeSemanticModelRelativePath(entry.Name)
		if !ok {
			return fmt.Errorf("unsafe zip entry %q", entry.Name)
		}
		if entry.FileInfo().Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("zip entry %q is a symlink", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		if entry.UncompressedSize64 > uint64(semanticModelPackMaxExtractSize-extractedBytes) {
			return fmt.Errorf("zip uncompressed size exceeds limit")
		}
		extractedBytes += int64(entry.UncompressedSize64)
	}
	if err := ensureSemanticPackStorage(destination, extractedBytes); err != nil {
		return err
	}
	for _, entry := range reader.File {
		relative, _ := safeSemanticModelRelativePath(entry.Name)
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		err = writeSemanticModelZipFile(target, source)
		closeErr := source.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func writeSemanticModelZipFile(target string, source io.Reader) error {
	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, source); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return nil
}

func validateSemanticModelRuntimeLayout(pack SemanticModelPackStatus, runtimePath string) error {
	raw, err := os.ReadFile(filepath.Join(runtimePath, semanticModelPackLayoutFile))
	if err != nil {
		return fmt.Errorf("read %s: %w", semanticModelPackLayoutFile, err)
	}
	var layout semanticModelPackLayout
	if err := json.Unmarshal(raw, &layout); err != nil {
		return fmt.Errorf("decode %s: %w", semanticModelPackLayoutFile, err)
	}
	normalizeSemanticModelPackLayout(&layout)
	if layout.SchemaVersion != semanticModelPackLayoutVersion {
		return fmt.Errorf("%s schemaVersion must be %d", semanticModelPackLayoutFile, semanticModelPackLayoutVersion)
	}
	if layout.Product != semanticModelPackLayoutProduct {
		return fmt.Errorf("%s product is invalid", semanticModelPackLayoutFile)
	}
	if layout.ModelID != pack.ID || layout.VectorSpaceID != pack.VectorSpaceID || layout.EmbeddingDim != pack.EmbeddingDim {
		return fmt.Errorf("%s model identity does not match installed pack", semanticModelPackLayoutFile)
	}
	if layout.InputKind != pack.InputKind || layout.Runtime != pack.Runtime {
		return fmt.Errorf("%s runtime metadata does not match installed pack", semanticModelPackLayoutFile)
	}
	for label, value := range map[string]string{
		"imageModel": layout.ImageModel,
		"textModel":  layout.TextModel,
		"tokenizer":  layout.Tokenizer,
	} {
		relative, ok := safeSemanticModelRelativePath(value)
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
	}
	return nil
}

func semanticModelRuntimeLayoutStatus(pack SemanticModelPackStatus, runtimePath string) string {
	if strings.TrimSpace(runtimePath) == "" {
		if semanticModelArtifactFormat(pack) == "zip" {
			return semanticRuntimeLayoutMissing
		}
		return semanticRuntimeLayoutUnsupported
	}
	if err := validateSemanticModelRuntimeLayout(pack, runtimePath); err != nil {
		return semanticRuntimeLayoutInvalid
	}
	return semanticRuntimeLayoutReady
}

func normalizeSemanticModelPackLayout(layout *semanticModelPackLayout) {
	layout.Product = strings.TrimSpace(layout.Product)
	layout.ModelID = strings.TrimSpace(layout.ModelID)
	layout.VectorSpaceID = strings.TrimSpace(layout.VectorSpaceID)
	layout.InputKind = strings.TrimSpace(layout.InputKind)
	layout.Runtime = strings.TrimSpace(layout.Runtime)
	layout.ImageModel = strings.TrimSpace(layout.ImageModel)
	layout.TextModel = strings.TrimSpace(layout.TextModel)
	layout.Tokenizer = strings.TrimSpace(layout.Tokenizer)
}

func safeSemanticModelRelativePath(value string) (string, bool) {
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
