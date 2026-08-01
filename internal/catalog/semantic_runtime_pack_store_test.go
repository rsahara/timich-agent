package catalog

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSemanticRuntimePackStoreReplacesOneCurrentVersionAtomically(t *testing.T) {
	t.Parallel()

	store, err := LoadOrCreateSemanticRuntimePackStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticRuntimePackStore() error = %v", err)
	}
	v1Artifact := semanticRuntimePackArtifactForTest(t, true)
	v1 := semanticRuntimePackStatusForTest("1.0.0", v1Artifact)
	v1Result, err := store.InstallPack(context.Background(), v1, bytes.NewReader(v1Artifact))
	if err != nil {
		t.Fatalf("InstallPack(v1) error = %v", err)
	}
	if err := store.FinalizePackInstall(v1.ID, v1.Version, v1Result.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(v1) error = %v", err)
	}
	oldRuntimePath := v1Result.RuntimePack.RuntimePath

	invalidArtifact := semanticRuntimePackArtifactForTest(t, false)
	v2Invalid := semanticRuntimePackStatusForTest("2.0.0", invalidArtifact)
	if _, err := store.InstallPack(context.Background(), v2Invalid, bytes.NewReader(invalidArtifact)); !errors.Is(err, ErrSemanticRuntimePackInvalid) {
		t.Fatalf("InstallPack(invalid v2) error = %v, want ErrSemanticRuntimePackInvalid", err)
	}
	if _, err := os.Stat(oldRuntimePath); err != nil {
		t.Fatalf("v1 runtime after rejected upgrade: %v", err)
	}
	if got := store.InstalledPackStatus(v1); !got.Installed || got.Version != "1.0.0" {
		t.Fatalf("InstalledPackStatus(v1) = %#v, want current v1", got)
	}
	if got := store.InstalledPackStatus(v2Invalid); got.Installed {
		t.Fatalf("InstalledPackStatus(v2 before install) = %#v, want not installed", got)
	}

	v2Artifact := semanticRuntimePackArtifactForTest(t, true)
	v2 := semanticRuntimePackStatusForTest("2.0.0", v2Artifact)
	v2Result, err := store.InstallPack(context.Background(), v2, bytes.NewReader(v2Artifact))
	if err != nil {
		t.Fatalf("InstallPack(v2) error = %v", err)
	}
	if _, err := os.Stat(v2Result.RuntimePack.RuntimePath); err != nil {
		t.Fatalf("v2 runtime path: %v", err)
	}
	if _, err := os.Stat(oldRuntimePath); err != nil {
		t.Fatalf("v1 runtime path before health finalization: %v", err)
	}
	failedReplacementPath := v2Result.RuntimePack.RuntimePath
	if err := store.RollbackPackInstall(v2.ID, v2.Version, v2Result.InstallID); err != nil {
		t.Fatalf("RollbackPackInstall(v2) error = %v", err)
	}
	if got := store.InstalledPackStatus(v1); !got.Installed || got.Version != v1.Version {
		t.Fatalf("InstalledPackStatus(v1 after rollback) = %#v, want restored v1", got)
	}
	if _, err := os.Stat(failedReplacementPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed v2 replacement path after rollback error = %v, want removed", err)
	}
	v2Result, err = store.InstallPack(context.Background(), v2, bytes.NewReader(v2Artifact))
	if err != nil {
		t.Fatalf("InstallPack(v2 retry) error = %v", err)
	}
	if err := store.FinalizePackInstall(v2.ID, v2.Version, v2Result.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(v2) error = %v", err)
	}
	if _, err := os.Stat(oldRuntimePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("v1 runtime path after activation error = %v, want removed", err)
	}
	if got := store.InstalledPackStatus(v1); got.Installed {
		t.Fatalf("InstalledPackStatus(v1 after v2) = %#v, want superseded", got)
	}
	if got := store.InstalledPackStatus(v2); !got.Installed || got.Version != "2.0.0" {
		t.Fatalf("InstalledPackStatus(v2) = %#v, want installed", got)
	}
	if packs := store.InstalledPacks(); len(packs) != 1 || packs[0].Version != "2.0.0" {
		t.Fatalf("InstalledPacks() = %#v, want one current v2", packs)
	}
}

func TestSemanticRuntimePackStoreRejectsNonONNXRuntime(t *testing.T) {
	t.Parallel()
	store, err := LoadOrCreateSemanticRuntimePackStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticRuntimePackStore() error = %v", err)
	}
	artifact := semanticRuntimePackArtifactForTest(t, true)
	pack := semanticRuntimePackStatusForTest("1.0.0", artifact)
	pack.Runtime = "custom-runtime"
	if _, err := store.InstallPack(context.Background(), pack, bytes.NewReader(artifact)); !errors.Is(err, ErrSemanticRuntimePackInvalid) {
		t.Fatalf("InstallPack(custom runtime) error = %v, want ErrSemanticRuntimePackInvalid", err)
	}
}

func TestSemanticRuntimePackStoreRecoversInterruptedReplacement(t *testing.T) {
	dataDir := t.TempDir()
	store, err := LoadOrCreateSemanticRuntimePackStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticRuntimePackStore() error = %v", err)
	}
	v1Artifact := semanticRuntimePackArtifactForTest(t, true)
	v1 := semanticRuntimePackStatusForTest("1.0.0", v1Artifact)
	v1Result, err := store.InstallPack(context.Background(), v1, bytes.NewReader(v1Artifact))
	if err != nil {
		t.Fatalf("InstallPack(v1) error = %v", err)
	}
	if err := store.FinalizePackInstall(v1.ID, v1.Version, v1Result.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(v1) error = %v", err)
	}

	v2Artifact := semanticRuntimePackArtifactForTest(t, true)
	v2 := semanticRuntimePackStatusForTest("2.0.0", v2Artifact)
	v2Result, err := store.InstallPack(context.Background(), v2, bytes.NewReader(v2Artifact))
	if err != nil {
		t.Fatalf("InstallPack(v2) error = %v", err)
	}
	if err := store.RollbackPackInstall(v2.ID, v2.Version, "stale-install-id"); !errors.Is(err, ErrSemanticRuntimePackInvalid) {
		t.Fatalf("RollbackPackInstall(stale) error = %v, want ErrSemanticRuntimePackInvalid", err)
	}
	v2Path := v2Result.RuntimePack.RuntimePath

	reopened, err := LoadOrCreateSemanticRuntimePackStore(dataDir)
	if err != nil {
		t.Fatalf("reopen runtime pack store: %v", err)
	}
	if got := reopened.InstalledPackStatus(v1); !got.Installed || got.Version != v1.Version {
		t.Fatalf("runtime after recovery = %#v, want v1", got)
	}
	if got := reopened.InstalledPackStatus(v2); got.Installed {
		t.Fatalf("interrupted runtime after recovery = %#v, want not installed", got)
	}
	if _, err := os.Stat(v2Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted runtime directory after recovery error = %v, want removed", err)
	}
}

func TestSemanticRuntimePackStoreKeepsCommittedActivePointerDuringRecovery(t *testing.T) {
	dataDir := t.TempDir()
	store, err := LoadOrCreateSemanticRuntimePackStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticRuntimePackStore() error = %v", err)
	}
	artifact := semanticRuntimePackArtifactForTest(t, true)
	v1 := semanticRuntimePackStatusForTest("1.0.0", artifact)
	v1Result, err := store.InstallPack(context.Background(), v1, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack(v1) error = %v", err)
	}
	if err := store.FinalizePackInstall(v1.ID, v1.Version, v1Result.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(v1) error = %v", err)
	}
	v2 := semanticRuntimePackStatusForTest("2.0.0", artifact)
	v2Result, err := store.InstallPack(context.Background(), v2, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack(v2) error = %v", err)
	}
	packRoot, err := store.packRoot(v2.ID)
	if err != nil {
		t.Fatalf("packRoot(v2) error = %v", err)
	}
	replacement, ok := readSemanticRuntimePackInstallRecord(filepath.Join(packRoot, semanticRuntimeInstallFile))
	if !ok || replacement.InstallID != v2Result.InstallID {
		t.Fatal("read v2 replacement record")
	}
	activeRaw, err := json.MarshalIndent(replacement, "", "  ")
	if err != nil {
		t.Fatalf("Marshal(active replacement) error = %v", err)
	}
	activeRaw = append(activeRaw, '\n')
	if err := os.WriteFile(store.activeRecordPath(v2.Runtime), activeRaw, 0o600); err != nil {
		t.Fatalf("WriteFile(active replacement) error = %v", err)
	}

	reopened, err := LoadOrCreateSemanticRuntimePackStore(dataDir)
	if err != nil {
		t.Fatalf("reopen committed runtime store: %v", err)
	}
	active, ok := reopened.InstalledPack(v2.Runtime)
	if !ok || active.Version != v2.Version || active.ID != v2.ID {
		t.Fatalf("InstalledPack() after recovery = %#v ok=%v, want v2", active, ok)
	}
	if _, err := os.Stat(store.rollbackRecordPath(v2.Runtime)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed rollback marker error = %v, want removed", err)
	}
}

func TestSemanticRuntimePackStoreRejectsEmptyVersionWithoutBreakingReopen(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	store, err := LoadOrCreateSemanticRuntimePackStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticRuntimePackStore() error = %v", err)
	}
	artifact := semanticRuntimePackArtifactForTest(t, true)
	pack := semanticRuntimePackStatusForTest("", artifact)
	if _, err := store.InstallPack(context.Background(), pack, bytes.NewReader(artifact)); !errors.Is(err, ErrSemanticRuntimePackInvalid) {
		t.Fatalf("InstallPack(empty version) error = %v, want ErrSemanticRuntimePackInvalid", err)
	}
	if _, err := LoadOrCreateSemanticRuntimePackStore(dataDir); err != nil {
		t.Fatalf("reopen after rejected empty version: %v", err)
	}
}

func TestSemanticRuntimePackStoreSweepsOrphanedInstallDirectoriesOnStartup(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	store, err := LoadOrCreateSemanticRuntimePackStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticRuntimePackStore() error = %v", err)
	}
	artifact := semanticRuntimePackArtifactForTest(t, true)
	pack := semanticRuntimePackStatusForTest("1.0.0", artifact)
	result, err := store.InstallPack(context.Background(), pack, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack() error = %v", err)
	}
	if err := store.FinalizePackInstall(pack.ID, pack.Version, result.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall() error = %v", err)
	}
	packRoot, err := store.packRoot(pack.ID)
	if err != nil {
		t.Fatalf("packRoot() error = %v", err)
	}
	orphanPath := filepath.Join(packRoot, ".install-orphan")
	if err := os.MkdirAll(orphanPath, 0o700); err != nil {
		t.Fatalf("MkdirAll(orphan) error = %v", err)
	}
	if _, err := LoadOrCreateSemanticRuntimePackStore(dataDir); err != nil {
		t.Fatalf("reopen runtime pack store: %v", err)
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan runtime directory Stat() error = %v, want not exist", err)
	}
	if _, err := os.Stat(result.RuntimePack.RuntimePath); err != nil {
		t.Fatalf("active runtime directory removed during sweep: %v", err)
	}
}

func TestSemanticRuntimePackStoreKeepsOneCrossIDActiveRuntimeUntilFinalize(t *testing.T) {
	t.Parallel()

	store, err := LoadOrCreateSemanticRuntimePackStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticRuntimePackStore() error = %v", err)
	}
	artifact := semanticRuntimePackArtifactForTest(t, true)
	packA := semanticRuntimePackStatusForTest("1.0.0", artifact)
	packA.ID = "runtime-a"
	resultA, err := store.InstallPack(context.Background(), packA, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack(A) error = %v", err)
	}
	if err := store.FinalizePackInstall(packA.ID, packA.Version, resultA.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(A) error = %v", err)
	}

	packB := semanticRuntimePackStatusForTest("2.0.0", artifact)
	packB.ID = "runtime-b"
	resultB, err := store.InstallPack(context.Background(), packB, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack(B) error = %v", err)
	}
	if active, ok := store.InstalledPack(packA.Runtime); !ok || active.ID != packA.ID {
		t.Fatalf("InstalledPack() before finalize = %#v ok=%v, want A", active, ok)
	}
	if err := store.RollbackPackInstall(packB.ID, packB.Version, resultB.InstallID); err != nil {
		t.Fatalf("RollbackPackInstall(B) error = %v", err)
	}
	if active, ok := store.InstalledPack(packA.Runtime); !ok || active.ID != packA.ID {
		t.Fatalf("InstalledPack() after rollback = %#v ok=%v, want A", active, ok)
	}

	resultB, err = store.InstallPack(context.Background(), packB, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack(B retry) error = %v", err)
	}
	if err := store.FinalizePackInstall(packB.ID, packB.Version, resultB.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(B) error = %v", err)
	}
	if active, ok := store.InstalledPack(packA.Runtime); !ok || active.ID != packB.ID {
		t.Fatalf("InstalledPack() after finalize = %#v ok=%v, want B", active, ok)
	}
	if _, err := os.Stat(resultA.RuntimePack.RuntimePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime A path after B finalize error = %v, want removed", err)
	}
}

func TestSemanticRuntimePackRollbackRejectsReplacementOutsidePackRoot(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	store, err := LoadOrCreateSemanticRuntimePackStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticRuntimePackStore() error = %v", err)
	}
	artifact := semanticRuntimePackArtifactForTest(t, true)
	pack := semanticRuntimePackStatusForTest("1.0.0", artifact)
	result, err := store.InstallPack(context.Background(), pack, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack() error = %v", err)
	}
	sentinel := filepath.Join(dataDir, "outside-sentinel")
	if err := os.MkdirAll(sentinel, 0o700); err != nil {
		t.Fatalf("MkdirAll(sentinel) error = %v", err)
	}
	rollbackPath := store.rollbackRecordPath(pack.Runtime)
	rollback, ok := readSemanticRuntimePackRollbackRecord(rollbackPath)
	if !ok {
		t.Fatal("readSemanticRuntimePackRollbackRecord() ok = false")
	}
	rollback.ReplacementDirName = filepath.Join("..", "..", filepath.Base(sentinel))
	raw, err := json.Marshal(rollback)
	if err != nil {
		t.Fatalf("json.Marshal(rollback) error = %v", err)
	}
	if err := os.WriteFile(rollbackPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile(rollback) error = %v", err)
	}
	if err := store.RollbackPackInstall(pack.ID, pack.Version, result.InstallID); !errors.Is(err, ErrSemanticRuntimePackInvalid) {
		t.Fatalf("RollbackPackInstall() error = %v, want ErrSemanticRuntimePackInvalid", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("outside sentinel was removed: %v", err)
	}
}

func semanticRuntimePackStatusForTest(version string, artifact []byte) SemanticRuntimePackStatus {
	sum := sha256.Sum256(artifact)
	return SemanticRuntimePackStatus{
		ID:       "test-onnx-runtime",
		Name:     "Test ONNX runtime",
		Version:  version,
		Runtime:  semanticRuntimeLoaderONNXRuntime,
		Platform: "linux-amd64",
		Artifact: &SemanticModelArtifactStatus{
			Filename:  "test-runtime.zip",
			SHA256:    hex.EncodeToString(sum[:]),
			SizeBytes: int64(len(artifact)),
		},
	}
}

func semanticRuntimePackArtifactForTest(t *testing.T, valid bool) []byte {
	t.Helper()

	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	write := func(name string, contents []byte, mode os.FileMode) {
		t.Helper()
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("CreateHeader(%q) error = %v", name, err)
		}
		if _, err := entry.Write(contents); err != nil {
			t.Fatalf("write zip entry %q error = %v", name, err)
		}
	}
	layout := semanticRuntimePackLayout{
		SchemaVersion: semanticRuntimePackLayoutVersion,
		Product:       semanticRuntimePackLayoutProduct,
		Runtime:       semanticRuntimeLoaderONNXRuntime,
		ServerPath:    "server.py",
		PythonPath:    "python/bin/python",
	}
	rawLayout, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("json.Marshal(layout) error = %v", err)
	}
	write(semanticRuntimePackLayoutFile, rawLayout, 0o600)
	if valid {
		write("server.py", []byte("print('ready')\n"), 0o700)
	}
	write("python/bin/python", []byte("#!/bin/sh\n"), 0o700)
	if err := writer.Close(); err != nil {
		t.Fatalf("zip.Close() error = %v", err)
	}
	return output.Bytes()
}
