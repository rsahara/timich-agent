package catalog

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	semanticRuntimePackRuntimeDir     = "runtime"
	semanticRuntimePackLayoutFile     = "timich-runtime.json"
	semanticRuntimePackLayoutProduct  = "timich-semantic-runtime-pack"
	semanticRuntimePackLayoutVersion  = 1
	semanticRuntimePackMaxExtractSize = int64(8 << 30)
)

type semanticRuntimePackLayout struct {
	SchemaVersion int    `json:"schemaVersion"`
	Product       string `json:"product"`
	Runtime       string `json:"runtime"`
	ServerPath    string `json:"serverPath"`
	PythonPath    string `json:"pythonPath"`
}

func prepareSemanticRuntimePackLayout(installDir string, artifactPath string) (string, string, string, error) {
	runtimePath := filepath.Join(installDir, semanticRuntimePackRuntimeDir)
	tempRuntimePath, err := os.MkdirTemp(installDir, ".runtime-*")
	if err != nil {
		return "", "", "", fmt.Errorf("create semantic runtime temp layout: %w", err)
	}
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = os.RemoveAll(tempRuntimePath)
		}
	}()
	if err := os.Chmod(tempRuntimePath, 0o700); err != nil {
		return "", "", "", fmt.Errorf("chmod semantic runtime temp layout: %w", err)
	}
	if err := extractSemanticRuntimeZip(artifactPath, tempRuntimePath); err != nil {
		return "", "", "", fmt.Errorf("%w: extract runtime layout: %v", ErrSemanticRuntimePackInvalid, err)
	}
	tempServerPath, tempPythonPath, err := validateSemanticRuntimePackLayout(tempRuntimePath)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: %v", ErrSemanticRuntimePackInvalid, err)
	}
	serverRelative, err := filepath.Rel(tempRuntimePath, tempServerPath)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: resolve server path: %v", ErrSemanticRuntimePackInvalid, err)
	}
	pythonRelative, err := filepath.Rel(tempRuntimePath, tempPythonPath)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: resolve python path: %v", ErrSemanticRuntimePackInvalid, err)
	}
	if err := replaceSemanticRuntimePath(tempRuntimePath, runtimePath); err != nil {
		return "", "", "", fmt.Errorf("install semantic runtime layout: %w", err)
	}
	cleanupTemp = false
	serverPath := filepath.Join(runtimePath, serverRelative)
	pythonPath := filepath.Join(runtimePath, pythonRelative)
	return runtimePath, serverPath, pythonPath, nil
}

func replaceSemanticRuntimePath(tempRuntimePath string, runtimePath string) error {
	backupPath := runtimePath + ".previous"
	if err := os.RemoveAll(backupPath); err != nil {
		return err
	}
	hadExisting := false
	if _, err := os.Stat(runtimePath); err == nil {
		if err := os.Rename(runtimePath, backupPath); err != nil {
			return err
		}
		hadExisting = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tempRuntimePath, runtimePath); err != nil {
		if hadExisting {
			_ = os.Rename(backupPath, runtimePath)
		}
		return err
	}
	if hadExisting {
		_ = os.RemoveAll(backupPath)
	}
	return nil
}

func extractSemanticRuntimeZip(artifactPath string, destination string) error {
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
		if entry.UncompressedSize64 > uint64(semanticRuntimePackMaxExtractSize-extractedBytes) {
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
		err = writeSemanticRuntimeZipFile(target, source, entry.FileInfo().Mode())
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

func writeSemanticRuntimeZipFile(target string, source io.Reader, mode os.FileMode) error {
	permission := os.FileMode(0o600)
	if mode&0o111 != 0 {
		permission = 0o700
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, permission)
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

func validateSemanticRuntimePackLayout(runtimePath string) (string, string, error) {
	raw, err := os.ReadFile(filepath.Join(runtimePath, semanticRuntimePackLayoutFile))
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", semanticRuntimePackLayoutFile, err)
	}
	var layout semanticRuntimePackLayout
	if err := json.Unmarshal(raw, &layout); err != nil {
		return "", "", fmt.Errorf("decode %s: %w", semanticRuntimePackLayoutFile, err)
	}
	normalizeSemanticRuntimePackLayout(&layout)
	if layout.SchemaVersion != semanticRuntimePackLayoutVersion {
		return "", "", fmt.Errorf("%s schemaVersion must be %d", semanticRuntimePackLayoutFile, semanticRuntimePackLayoutVersion)
	}
	if layout.Product != semanticRuntimePackLayoutProduct {
		return "", "", fmt.Errorf("%s product is invalid", semanticRuntimePackLayoutFile)
	}
	if layout.Runtime != semanticRuntimeLoaderONNXRuntime {
		return "", "", fmt.Errorf("%s runtime %q is not supported", semanticRuntimePackLayoutFile, layout.Runtime)
	}
	serverRelative, ok := safeSemanticModelRelativePath(layout.ServerPath)
	if !ok {
		return "", "", fmt.Errorf("serverPath is invalid")
	}
	serverPath := filepath.Join(runtimePath, filepath.FromSlash(serverRelative))
	if info, err := os.Stat(serverPath); err != nil {
		return "", "", fmt.Errorf("serverPath file is missing: %w", err)
	} else if info.IsDir() {
		return "", "", fmt.Errorf("serverPath file is a directory")
	}
	pythonRelative, ok := safeSemanticModelRelativePath(layout.PythonPath)
	if !ok {
		return "", "", fmt.Errorf("pythonPath is invalid")
	}
	pythonPath := filepath.Join(runtimePath, filepath.FromSlash(pythonRelative))
	if info, err := os.Stat(pythonPath); err != nil {
		return "", "", fmt.Errorf("pythonPath file is missing: %w", err)
	} else if info.IsDir() {
		return "", "", fmt.Errorf("pythonPath file is a directory")
	}
	return serverPath, pythonPath, nil
}

func normalizeSemanticRuntimePackLayout(layout *semanticRuntimePackLayout) {
	layout.Product = strings.TrimSpace(layout.Product)
	layout.Runtime = strings.TrimSpace(layout.Runtime)
	layout.ServerPath = strings.TrimSpace(layout.ServerPath)
	layout.PythonPath = strings.TrimSpace(layout.PythonPath)
}
