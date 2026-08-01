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
	"time"
)

func TestInstalledModelRuntimeStatusMarksHelperBlocked(t *testing.T) {
	root := t.TempDir()
	artifactPath := filepath.Join(root, "model.zip")
	if err := os.WriteFile(artifactPath, []byte("model artifact"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	runtimePath := writeRuntimeLayoutForStatusTest(t)
	helperPath := writeRuntimeHelperForStatusTest(t, map[string]any{
		"protocolVersion": 1,
		"runtime":         "onnxruntime",
		"modelId":         "timich-multilingual-clip-small",
		"vectorSpaceId":   "timich-multilingual-clip-small/2026.06/d512",
		"embeddingDim":    512,
		"inputKind":       "image",
		"loaded":          false,
		"canEmbed":        false,
		"messageCode":     "semantic_runtime_onnxruntime_unavailable",
	})

	status := semanticInstalledModelRuntimeStatus(semanticRuntimeStatusTestPack(), artifactPath, runtimePath, helperPath)

	if status.Status != semanticRuntimeStatusBlocked ||
		status.Runtime != semanticRuntimeLoaderONNXRuntime ||
		status.Loader != semanticRuntimeLoaderONNXRuntime ||
		status.ArtifactStatus != semanticRuntimeArtifactAvailable ||
		status.ArtifactFormat != "zip" ||
		status.LayoutStatus != semanticRuntimeLayoutReady ||
		status.HelperStatus != semanticRuntimeHelperBlocked ||
		status.HelperProtocol != 1 ||
		status.Loaded ||
		status.CanEmbed ||
		status.MessageCode != "semantic_runtime_onnxruntime_unavailable" {
		t.Fatalf("runtime status = %#v", status)
	}
}

func TestInstalledModelRuntimeStatusLoadsSentenceCLIPRuntime(t *testing.T) {
	root := t.TempDir()
	artifactPath := filepath.Join(root, "model.zip")
	if err := os.WriteFile(artifactPath, []byte("model artifact"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	pack := semanticRuntimeStatusTestPack()
	pack.Runtime = semanticRuntimeLoaderSentenceCLIP
	runtimePath := writeRuntimeLayoutForStatusTestWithRuntime(t, semanticRuntimeLoaderSentenceCLIP)
	helperPath := writeRuntimeHelperForStatusTest(t, map[string]any{
		"protocolVersion": 1,
		"runtime":         "sentence-transformers-clip",
		"modelId":         "timich-multilingual-clip-small",
		"vectorSpaceId":   "timich-multilingual-clip-small/2026.06/d512",
		"embeddingDim":    512,
		"inputKind":       "image",
		"loaded":          true,
		"canEmbed":        true,
	})

	status := semanticInstalledModelRuntimeStatus(pack, artifactPath, runtimePath, helperPath)

	if status.Status != semanticRuntimeStatusLoaded ||
		status.Runtime != semanticRuntimeLoaderSentenceCLIP ||
		status.Loader != semanticRuntimeLoaderSentenceCLIP ||
		status.ArtifactStatus != semanticRuntimeArtifactAvailable ||
		status.LayoutStatus != semanticRuntimeLayoutReady ||
		status.HelperStatus != semanticRuntimeHelperReady ||
		status.HelperProtocol != 1 ||
		!status.Loaded ||
		!status.CanEmbed ||
		status.MessageCode != semanticRuntimeMessageHelperLoaded {
		t.Fatalf("runtime status = %#v", status)
	}
}

func TestInstalledModelRuntimeStatusLoadsTransformersSigLIP2Runtime(t *testing.T) {
	root := t.TempDir()
	artifactPath := filepath.Join(root, "model.zip")
	if err := os.WriteFile(artifactPath, []byte("model artifact"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	pack := semanticRuntimeStatusTestPack()
	pack.Runtime = semanticRuntimeLoaderTransformersSigLIP2
	runtimePath := writeRuntimeLayoutForStatusTestWithRuntime(t, semanticRuntimeLoaderTransformersSigLIP2)
	helperPath := writeRuntimeHelperForStatusTest(t, map[string]any{
		"protocolVersion": 1,
		"runtime":         "transformers-siglip2",
		"modelId":         "timich-multilingual-clip-small",
		"vectorSpaceId":   "timich-multilingual-clip-small/2026.06/d512",
		"embeddingDim":    512,
		"inputKind":       "image",
		"loaded":          true,
		"canEmbed":        true,
	})

	status := semanticInstalledModelRuntimeStatus(pack, artifactPath, runtimePath, helperPath)

	if status.Status != semanticRuntimeStatusLoaded ||
		status.Runtime != semanticRuntimeLoaderTransformersSigLIP2 ||
		status.Loader != semanticRuntimeLoaderTransformersSigLIP2 ||
		status.ArtifactStatus != semanticRuntimeArtifactAvailable ||
		status.LayoutStatus != semanticRuntimeLayoutReady ||
		status.HelperStatus != semanticRuntimeHelperReady ||
		status.HelperProtocol != 1 ||
		!status.Loaded ||
		!status.CanEmbed ||
		status.MessageCode != semanticRuntimeMessageHelperLoaded {
		t.Fatalf("runtime status = %#v", status)
	}
}

func TestInstalledModelRuntimeStatusRespectsCallerContext(t *testing.T) {
	root := t.TempDir()
	artifactPath := filepath.Join(root, "model.zip")
	if err := os.WriteFile(artifactPath, []byte("model artifact"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	runtimePath := writeRuntimeLayoutForStatusTest(t)
	helperPath := filepath.Join(t.TempDir(), "timich-semantic-helper")
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\nsleep 2\n"), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	status := semanticInstalledModelRuntimeStatusWithContext(ctx, semanticRuntimeStatusTestPack(), artifactPath, runtimePath, helperPath)
	elapsed := time.Since(started)

	if elapsed >= time.Second {
		t.Fatalf("runtime status helper took %s, want caller context to stop it quickly", elapsed)
	}
	if status.HelperStatus != semanticRuntimeHelperFailed ||
		status.MessageCode != semanticRuntimeMessageHelperFailed ||
		status.Loaded ||
		status.CanEmbed {
		t.Fatalf("runtime status = %#v, want helper failure after caller context timeout", status)
	}
}

func TestSemanticModelPackStoreReleasesLockDuringHelperInspection(t *testing.T) {
	dataDir := t.TempDir()
	markerPath := filepath.Join(t.TempDir(), "helper-started")
	helperPath := filepath.Join(t.TempDir(), "timich-semantic-helper")
	helperScript := "#!/bin/sh\n: > '" + markerPath + "'\nsleep 1\nexit 1\n"
	if err := os.WriteFile(helperPath, []byte(helperScript), 0o700); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	store, err := LoadOrCreateSemanticModelPackStoreWithOptions(dataDir, SemanticModelPackStoreOptions{RuntimeHelperPath: helperPath})
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStoreWithOptions() error = %v", err)
	}
	artifact := semanticRuntimeStatusTestZipArtifact(t)
	pack := semanticRuntimeStatusTestPack()
	pack.EmbeddingDim = 4
	sum := sha256.Sum256(artifact)
	pack.Artifact.SHA256 = hex.EncodeToString(sum[:])
	pack.Artifact.SizeBytes = int64(len(artifact))
	result, err := store.InstallPack(context.Background(), pack, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack() error = %v", err)
	}
	if err := store.FinalizePackInstall(pack.ID, pack.VectorSpaceID, result.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall() error = %v", err)
	}

	inspectionDone := make(chan struct{})
	go func() {
		defer close(inspectionDone)
		store.InstalledProfilesWithContext(context.Background())
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(markerPath); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect helper marker: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("helper inspection did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}

	started := time.Now()
	if _, ok := store.InstalledCandidateProfile(); !ok {
		t.Fatal("InstalledCandidateProfile() ok = false")
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("InstalledCandidateProfile() waited %s for helper inspection lock", elapsed)
	}
	<-inspectionDone
}

func TestSemanticModelPackStoreReturnsCandidateHelperEmbeddingProfile(t *testing.T) {
	ctx := context.Background()
	helperPath := writeEmbeddingRuntimeHelperForStatusTest(t)
	dataDir := t.TempDir()
	store, err := LoadOrCreateSemanticModelPackStoreWithOptions(dataDir, SemanticModelPackStoreOptions{
		RuntimeHelperPath: helperPath,
	})
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStoreWithOptions() error = %v", err)
	}

	artifact := semanticRuntimeStatusTestZipArtifact(t)
	sum := sha256.Sum256(artifact)
	pack := semanticRuntimeStatusTestPack()
	pack.EmbeddingDim = 4
	pack.Artifact.SHA256 = hex.EncodeToString(sum[:])
	pack.Artifact.SizeBytes = int64(len(artifact))
	installResult, err := store.InstallPack(ctx, pack, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack() error = %v", err)
	}
	if err := store.FinalizePackInstall(pack.ID, pack.VectorSpaceID, installResult.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall() error = %v", err)
	}

	profile, ok := store.CandidateEmbeddingProfile(pack.ID, pack.VectorSpaceID)
	if !ok {
		t.Fatal("CandidateEmbeddingProfile() ok = false, want helper-backed profile")
	}
	imageEmbedding, err := profile.EmbedSemanticAsset(ctx, semanticAssetEmbeddingInput{
		Image: &semanticImageEmbeddingInput{
			Bytes:       []byte("image bytes"),
			ContentType: "image/jpeg",
			Source:      "preview",
		},
	})
	if err != nil {
		t.Fatalf("EmbedSemanticAsset() error = %v", err)
	}
	if imageEmbedding.Input != "image-helper" ||
		len(imageEmbedding.Vector) != 4 ||
		imageEmbedding.Vector[0] != 1 ||
		imageEmbedding.Vector[1] != 0 {
		t.Fatalf("image embedding = %#v", imageEmbedding)
	}

	textVector, err := profile.EmbedText(ctx, "beach")
	if err != nil {
		t.Fatalf("EmbedText() error = %v", err)
	}
	if len(textVector) != 4 || textVector[0] != 0 || textVector[1] != 1 {
		t.Fatalf("text vector = %#v", textVector)
	}

	activation, err := store.ActivatePack(pack.ID, pack.VectorSpaceID)
	if err != nil {
		t.Fatalf("ActivatePack() error = %v", err)
	}
	if activation.Status != "activated" || activation.Profile.Role != semanticModelRoleActive || activation.Profile.ModelID != pack.ID {
		t.Fatalf("activation = %#v", activation)
	}
	if active, ok := store.ActiveProfile(); !ok || active.Role != semanticModelRoleActive || active.ModelID != pack.ID {
		t.Fatalf("ActiveProfile() = %#v ok=%v", active, ok)
	}
	if active, ok := store.InstalledActiveProfile(); !ok ||
		active.Role != semanticModelRoleActive ||
		active.ModelID != pack.ID ||
		active.Runtime != nil {
		t.Fatalf("InstalledActiveProfile() = %#v ok=%v, want active identity without runtime inspection", active, ok)
	}
	if candidate, ok := store.InstalledCandidateProfile(); ok {
		t.Fatalf("InstalledCandidateProfile() after activation = %#v, want none", candidate)
	}

	reloaded, err := LoadOrCreateSemanticModelPackStoreWithOptions(dataDir, SemanticModelPackStoreOptions{
		RuntimeHelperPath: helperPath,
	})
	if err != nil {
		t.Fatalf("reload semantic model pack store: %v", err)
	}
	if active, ok := reloaded.ActiveProfile(); !ok || active.Role != semanticModelRoleActive || active.ModelID != pack.ID {
		t.Fatalf("reloaded ActiveProfile() = %#v ok=%v", active, ok)
	}
	if active, ok := reloaded.InstalledActiveProfile(); !ok || active.Role != semanticModelRoleActive || active.ModelID != pack.ID {
		t.Fatalf("reloaded InstalledActiveProfile() = %#v ok=%v", active, ok)
	}
}

func TestSemanticModelPackStoreAtomicallyReplacesCompatibleArtifactVersion(t *testing.T) {
	ctx := context.Background()
	store, err := LoadOrCreateSemanticModelPackStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStore() error = %v", err)
	}

	oldPack := semanticRuntimeStatusTestArchivePack("replaceable-model", "replaceable-model/d4", "2026.06", []byte("old artifact"))
	newPack := semanticRuntimeStatusTestArchivePack("replaceable-model", "replaceable-model/d4", "2026.07", []byte("new artifact"))
	oldResult, err := store.InstallPack(ctx, oldPack, bytes.NewReader([]byte("old artifact")))
	if err != nil {
		t.Fatalf("InstallPack(old) error = %v", err)
	}
	if err := store.FinalizePackInstall(oldPack.ID, oldPack.VectorSpaceID, oldResult.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(old) error = %v", err)
	}
	if _, err := store.ActivatePack(oldPack.ID, oldPack.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack(old) error = %v", err)
	}
	newResult, err := store.InstallPack(ctx, newPack, bytes.NewReader([]byte("new artifact")))
	if err != nil {
		t.Fatalf("InstallPack(new) error = %v", err)
	}
	if err := store.RollbackPackInstall(newPack.ID, newPack.VectorSpaceID, newResult.InstallID); err != nil {
		t.Fatalf("RollbackPackInstall(new) error = %v", err)
	}
	if active, ok := store.ActiveProfile(); !ok || active.ModelPack == nil || active.ModelPack.Version != oldPack.Version {
		t.Fatalf("ActiveProfile() after rollback = %#v ok=%v, want previous version", active, ok)
	}
	newResult, err = store.InstallPack(ctx, newPack, bytes.NewReader([]byte("new artifact")))
	if err != nil {
		t.Fatalf("InstallPack(new retry) error = %v", err)
	}

	profiles := store.InstalledProfiles()
	if len(profiles) != 1 || profiles[0].ModelPack == nil || profiles[0].ModelPack.Version != "2026.06" {
		t.Fatalf("InstalledProfiles() = %#v, want active version until finalize", profiles)
	}
	active, ok := store.ActiveProfile()
	if !ok || active.ModelPack == nil || active.ModelPack.Version != "2026.06" || active.Role != semanticModelRoleActive {
		t.Fatalf("ActiveProfile() = %#v ok=%v, want previous active bytes until finalize", active, ok)
	}
	if err := store.FinalizePackInstall(newPack.ID, newPack.VectorSpaceID, newResult.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(new) error = %v", err)
	}
	if active, ok := store.ActiveProfile(); !ok || active.ModelPack == nil || active.ModelPack.Version != "2026.07" {
		t.Fatalf("ActiveProfile() after finalize = %#v ok=%v, want replacement", active, ok)
	}
	modelRoot, err := store.modelRoot(newPack.ID)
	if err != nil {
		t.Fatalf("modelRoot() error = %v", err)
	}
	directoryCount := 0
	entries, err := os.ReadDir(modelRoot)
	if err != nil {
		t.Fatalf("ReadDir(model root) error = %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			directoryCount++
		}
	}
	if directoryCount != 1 {
		t.Fatalf("installed artifact directories = %d, want one current install", directoryCount)
	}
}

func TestSemanticModelPackRollbackRejectsReplacementOutsidePackRoot(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	store, err := LoadOrCreateSemanticModelPackStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStore() error = %v", err)
	}
	artifact := []byte("model artifact")
	pack := semanticRuntimeStatusTestArchivePack("contained-model", "contained-model/d4", "2026.07", artifact)
	result, err := store.InstallPack(context.Background(), pack, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack() error = %v", err)
	}
	modelRoot, err := store.modelRoot(pack.ID)
	if err != nil {
		t.Fatalf("modelRoot() error = %v", err)
	}
	sentinel := filepath.Join(dataDir, "outside-model-sentinel")
	if err := os.MkdirAll(sentinel, 0o700); err != nil {
		t.Fatalf("MkdirAll(sentinel) error = %v", err)
	}
	rollbackPath := filepath.Join(modelRoot, semanticModelRollbackFile)
	rollback, ok := readSemanticModelPackRollbackRecord(rollbackPath)
	if !ok {
		t.Fatal("readSemanticModelPackRollbackRecord() ok = false")
	}
	rollback.ReplacementDirName = filepath.Join("..", "..", filepath.Base(sentinel))
	raw, err := json.Marshal(rollback)
	if err != nil {
		t.Fatalf("json.Marshal(rollback) error = %v", err)
	}
	if err := os.WriteFile(rollbackPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile(rollback) error = %v", err)
	}
	if err := store.RollbackPackInstall(pack.ID, pack.VectorSpaceID, result.InstallID); !errors.Is(err, ErrSemanticModelPackInvalid) {
		t.Fatalf("RollbackPackInstall() error = %v, want ErrSemanticModelPackInvalid", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("outside sentinel was removed: %v", err)
	}
}

func TestSemanticModelPackStoreRejectsIncompatibleIdentityReuse(t *testing.T) {
	ctx := context.Background()
	store, err := LoadOrCreateSemanticModelPackStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStore() error = %v", err)
	}

	current := semanticRuntimeStatusTestArchivePack("stable-model-id", "stable-model-id/d4", "2026.06", []byte("current artifact"))
	currentResult, err := store.InstallPack(ctx, current, bytes.NewReader([]byte("current artifact")))
	if err != nil {
		t.Fatalf("InstallPack(current) error = %v", err)
	}
	if err := store.FinalizePackInstall(current.ID, current.VectorSpaceID, currentResult.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(current) error = %v", err)
	}
	incompatible := semanticRuntimeStatusTestArchivePack("stable-model-id", "stable-model-id-v2/d4", "2026.07", []byte("incompatible artifact"))
	if _, err := store.InstallPack(ctx, incompatible, bytes.NewReader([]byte("incompatible artifact"))); !errors.Is(err, ErrSemanticModelPackInvalid) {
		t.Fatalf("InstallPack(incompatible) error = %v, want ErrSemanticModelPackInvalid", err)
	}

	profiles := store.InstalledProfiles()
	if len(profiles) != 1 || profiles[0].VectorSpaceID != current.VectorSpaceID || profiles[0].ModelPack == nil || profiles[0].ModelPack.Version != current.Version {
		t.Fatalf("InstalledProfiles() = %#v, want original compatible identity unchanged", profiles)
	}
}

func TestSemanticModelPackStorePreservesIdentityAcrossUninstall(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := LoadOrCreateSemanticModelPackStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStore() error = %v", err)
	}
	originalArtifact := []byte("original artifact")
	original := semanticRuntimeStatusTestArchivePack("reserved-model-id", "reserved-model-id/d4", "2026.06", originalArtifact)
	originalResult, err := store.InstallPack(ctx, original, bytes.NewReader(originalArtifact))
	if err != nil {
		t.Fatalf("InstallPack(original) error = %v", err)
	}
	if err := store.FinalizePackInstall(original.ID, original.VectorSpaceID, originalResult.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(original) error = %v", err)
	}
	if _, err := store.UninstallPack(original.ID, original.VectorSpaceID); err != nil {
		t.Fatalf("UninstallPack(original) error = %v", err)
	}
	modelRoot, err := store.modelRoot(original.ID)
	if err != nil {
		t.Fatalf("modelRoot() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(modelRoot, semanticModelIdentityFile)); err != nil {
		t.Fatalf("identity record after uninstall: %v", err)
	}

	compatibleArtifact := []byte("compatible replacement")
	compatible := semanticRuntimeStatusTestArchivePack(original.ID, original.VectorSpaceID, "2026.07", compatibleArtifact)
	compatibleResult, err := store.InstallPack(ctx, compatible, bytes.NewReader(compatibleArtifact))
	if err != nil {
		t.Fatalf("InstallPack(compatible) error = %v", err)
	}
	if err := store.FinalizePackInstall(compatible.ID, compatible.VectorSpaceID, compatibleResult.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(compatible) error = %v", err)
	}
	if _, err := store.UninstallPack(compatible.ID, compatible.VectorSpaceID); err != nil {
		t.Fatalf("UninstallPack(compatible) error = %v", err)
	}

	incompatibleArtifact := []byte("incompatible replacement")
	incompatible := semanticRuntimeStatusTestArchivePack(original.ID, "reserved-model-id-v2/d4", "2026.08", incompatibleArtifact)
	if _, err := store.InstallPack(ctx, incompatible, bytes.NewReader(incompatibleArtifact)); !errors.Is(err, ErrSemanticModelPackInvalid) {
		t.Fatalf("InstallPack(incompatible after uninstall) error = %v, want ErrSemanticModelPackInvalid", err)
	}
	if profiles := store.InstalledProfiles(); len(profiles) != 0 {
		t.Fatalf("InstalledProfiles() = %#v, want no incompatible replacement", profiles)
	}
}

func TestSemanticRuntimeHelperProfileClassifiesAssetInputFailure(t *testing.T) {
	helperPath := filepath.Join(t.TempDir(), "timich-semantic-helper")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"error":"invalid image input","errorClass":"asset_input"}' >&2
exit 1
`
	if err := os.WriteFile(helperPath, []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	profile := semanticRuntimeHelperProfile{
		helperPath:  helperPath,
		runtimePath: t.TempDir(),
		pack: SemanticModelPackStatus{
			ID:            "test-model",
			VectorSpaceID: "test-model/d4",
			EmbeddingDim:  4,
			InputKind:     semanticInputKindImage,
			Runtime:       "onnxruntime",
		},
	}
	_, err := profile.EmbedSemanticAsset(context.Background(), semanticAssetEmbeddingInput{
		Image: &semanticImageEmbeddingInput{Bytes: []byte("broken"), ContentType: "image/jpeg"},
	})
	if !errors.Is(err, ErrSemanticAssetInput) {
		t.Fatalf("EmbedSemanticAsset() error = %v, want ErrSemanticAssetInput", err)
	}
}

func TestSemanticModelPackStoreUninstallProtectsActiveAndRemovesOnlyTarget(t *testing.T) {
	ctx := context.Background()
	store, err := LoadOrCreateSemanticModelPackStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStore() error = %v", err)
	}

	activePack := semanticRuntimeStatusTestArchivePack("active-model", "active-model/v1/d4", "2026.06-active", []byte("active artifact"))
	inactivePack := semanticRuntimeStatusTestArchivePack("inactive-model", "inactive-model/v1/d4", "2026.06-inactive", []byte("inactive artifact"))
	activeResult, err := store.InstallPack(ctx, activePack, bytes.NewReader([]byte("active artifact")))
	if err != nil {
		t.Fatalf("InstallPack(active) error = %v", err)
	}
	if err := store.FinalizePackInstall(activePack.ID, activePack.VectorSpaceID, activeResult.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(active) error = %v", err)
	}
	installedDir := func(pack SemanticModelPackStatus) string {
		t.Helper()
		modelRoot, err := store.modelRoot(pack.ID)
		if err != nil {
			t.Fatalf("modelRoot(%s) error = %v", pack.ID, err)
		}
		record, ok := readSemanticModelInstallRecord(filepath.Join(modelRoot, semanticModelInstallFile))
		if !ok {
			t.Fatalf("read install record for %s", pack.ID)
		}
		return filepath.Dir(filepath.Dir(record.ArtifactPath))
	}
	activeDir := installedDir(activePack)
	if _, err := store.ActivatePack(activePack.ID, activePack.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack(active) error = %v", err)
	}
	inactiveResult, err := store.InstallPack(ctx, inactivePack, bytes.NewReader([]byte("inactive artifact")))
	if err != nil {
		t.Fatalf("InstallPack(inactive) error = %v", err)
	}
	if err := store.FinalizePackInstall(inactivePack.ID, inactivePack.VectorSpaceID, inactiveResult.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(inactive) error = %v", err)
	}
	inactiveDir := installedDir(inactivePack)

	if _, err := store.UninstallPack(activePack.ID, activePack.VectorSpaceID); !errors.Is(err, ErrSemanticModelPackActive) {
		t.Fatalf("UninstallPack(active) error = %v, want ErrSemanticModelPackActive", err)
	}
	if _, err := os.Stat(activeDir); err != nil {
		t.Fatalf("active install dir after blocked uninstall Stat() error = %v", err)
	}
	if _, err := os.Stat(inactiveDir); err != nil {
		t.Fatalf("inactive install dir before uninstall Stat() error = %v", err)
	}

	result, err := store.UninstallPack(inactivePack.ID, inactivePack.VectorSpaceID)
	if err != nil {
		t.Fatalf("UninstallPack(inactive) error = %v", err)
	}
	if result.Status != "uninstalled" || result.ModelID != inactivePack.ID || result.VectorSpaceID != inactivePack.VectorSpaceID || result.UninstalledAt.IsZero() {
		t.Fatalf("uninstall result = %#v", result)
	}
	if _, err := os.Stat(inactiveDir); !os.IsNotExist(err) {
		t.Fatalf("inactive install dir Stat() error = %v, want not exist", err)
	}
	if _, err := os.Stat(activeDir); err != nil {
		t.Fatalf("active install dir after inactive uninstall Stat() error = %v", err)
	}
	if active, ok := store.ActiveProfile(); !ok || active.ModelID != activePack.ID {
		t.Fatalf("ActiveProfile() after inactive uninstall = %#v ok=%v", active, ok)
	}
	if candidate, ok := store.InstalledCandidateProfile(); ok {
		t.Fatalf("InstalledCandidateProfile() after inactive uninstall = %#v, want none", candidate)
	}
}

func TestSemanticModelPackStoreKeepsOneExplicitCandidateSlot(t *testing.T) {
	ctx := context.Background()
	store, err := LoadOrCreateSemanticModelPackStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStore() error = %v", err)
	}
	install := func(pack SemanticModelPackStatus, artifact []byte) (string, string) {
		t.Helper()
		result, err := store.InstallPack(ctx, pack, bytes.NewReader(artifact))
		if err != nil {
			t.Fatalf("InstallPack(%s) error = %v", pack.ID, err)
		}
		modelRoot, err := store.modelRoot(pack.ID)
		if err != nil {
			t.Fatalf("modelRoot(%s) error = %v", pack.ID, err)
		}
		pending, ok := readSemanticModelPackRollbackRecord(filepath.Join(modelRoot, semanticModelRollbackFile))
		if !ok || pending.Replacement == nil {
			t.Fatalf("read pending install record for %s", pack.ID)
		}
		return filepath.Dir(filepath.Dir(pending.Replacement.ArtifactPath)), result.InstallID
	}

	activeBytes := []byte("active artifact")
	activePack := semanticRuntimeStatusTestArchivePack("active-slot-model", "active-slot-model/d4", "2026.06", activeBytes)
	_, activeInstallID := install(activePack, activeBytes)
	if err := store.FinalizePackInstall(activePack.ID, activePack.VectorSpaceID, activeInstallID); err != nil {
		t.Fatalf("FinalizePackInstall(active) error = %v", err)
	}
	if _, err := store.ActivatePack(activePack.ID, activePack.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack(active) error = %v", err)
	}

	firstBytes := []byte("first candidate artifact")
	first := semanticRuntimeStatusTestArchivePack("first-candidate", "first-candidate/d4", "2026.06", firstBytes)
	firstDir, firstInstallID := install(first, firstBytes)
	if err := store.FinalizePackInstall(first.ID, first.VectorSpaceID, firstInstallID); err != nil {
		t.Fatalf("FinalizePackInstall(first candidate) error = %v", err)
	}
	secondBytes := []byte("second candidate artifact")
	second := semanticRuntimeStatusTestArchivePack("second-candidate", "second-candidate/d4", "2026.07", secondBytes)
	_, secondInstallID := install(second, secondBytes)
	if candidate, ok := store.InstalledCandidateProfile(); !ok || candidate.ModelID != first.ID {
		t.Fatalf("InstalledCandidateProfile() before proof = %#v ok=%v, want first candidate", candidate, ok)
	}
	if profiles := store.InstalledProfiles(); len(profiles) != 2 {
		t.Fatalf("InstalledProfiles() = %#v, want active plus one candidate", profiles)
	}
	if err := store.FinalizePackInstall(second.ID, second.VectorSpaceID, secondInstallID); err != nil {
		t.Fatalf("FinalizePackInstall(second candidate) error = %v", err)
	}
	if candidate, ok := store.InstalledCandidateProfile(); !ok || candidate.ModelID != second.ID {
		t.Fatalf("InstalledCandidateProfile() after proof = %#v ok=%v, want second candidate", candidate, ok)
	}
	if _, err := os.Stat(firstDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first candidate directory after replacement error = %v, want removed", err)
	}
}

func TestSemanticModelPackStoreRecoversInterruptedCandidateReplacement(t *testing.T) {
	dataDir := t.TempDir()
	store, err := LoadOrCreateSemanticModelPackStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStore() error = %v", err)
	}
	ctx := context.Background()
	firstArtifact := []byte("first candidate artifact")
	first := semanticRuntimeStatusTestArchivePack("first-recovery-candidate", "first-recovery-candidate/d4", "2026.06", firstArtifact)
	firstResult, err := store.InstallPack(ctx, first, bytes.NewReader(firstArtifact))
	if err != nil {
		t.Fatalf("InstallPack(first) error = %v", err)
	}
	if err := store.FinalizePackInstall(first.ID, first.VectorSpaceID, firstResult.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(first) error = %v", err)
	}

	secondArtifact := []byte("second candidate artifact")
	second := semanticRuntimeStatusTestArchivePack("second-recovery-candidate", "second-recovery-candidate/d4", "2026.07", secondArtifact)
	_, err = store.InstallPack(ctx, second, bytes.NewReader(secondArtifact))
	if err != nil {
		t.Fatalf("InstallPack(second) error = %v", err)
	}
	if err := store.FinalizePackInstall(second.ID, second.VectorSpaceID, "stale-install-id"); !errors.Is(err, ErrSemanticModelPackInvalid) {
		t.Fatalf("FinalizePackInstall(stale) error = %v, want ErrSemanticModelPackInvalid", err)
	}
	secondRoot, err := store.modelRoot(second.ID)
	if err != nil {
		t.Fatalf("modelRoot(second) error = %v", err)
	}
	pending, ok := readSemanticModelPackRollbackRecord(filepath.Join(secondRoot, semanticModelRollbackFile))
	if !ok || pending.Replacement == nil {
		t.Fatal("read second pending install record")
	}
	secondDir := filepath.Dir(filepath.Dir(pending.Replacement.ArtifactPath))

	reopened, err := LoadOrCreateSemanticModelPackStore(dataDir)
	if err != nil {
		t.Fatalf("reopen model pack store: %v", err)
	}
	if candidate, ok := reopened.InstalledCandidateProfile(); !ok || candidate.ModelID != first.ID {
		t.Fatalf("candidate after recovery = %#v ok=%v, want first candidate", candidate, ok)
	}
	if _, err := os.Stat(secondDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted candidate directory after recovery error = %v, want removed", err)
	}
	if _, err := os.Stat(filepath.Join(secondRoot, semanticModelRollbackFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback marker after recovery error = %v, want removed", err)
	}
}

func TestSemanticModelPackStoreTreatsCurrentInstallPointerAsCommitted(t *testing.T) {
	dataDir := t.TempDir()
	store, err := LoadOrCreateSemanticModelPackStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStore() error = %v", err)
	}
	ctx := context.Background()
	oldArtifact := []byte("old active artifact")
	oldPack := semanticRuntimeStatusTestArchivePack("committed-model", "committed-model/d4", "2026.06", oldArtifact)
	oldResult, err := store.InstallPack(ctx, oldPack, bytes.NewReader(oldArtifact))
	if err != nil {
		t.Fatalf("InstallPack(old) error = %v", err)
	}
	if err := store.FinalizePackInstall(oldPack.ID, oldPack.VectorSpaceID, oldResult.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall(old) error = %v", err)
	}
	if _, err := store.ActivatePack(oldPack.ID, oldPack.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack(old) error = %v", err)
	}

	newArtifact := []byte("new active artifact")
	newPack := semanticRuntimeStatusTestArchivePack(oldPack.ID, oldPack.VectorSpaceID, "2026.07", newArtifact)
	newResult, err := store.InstallPack(ctx, newPack, bytes.NewReader(newArtifact))
	if err != nil {
		t.Fatalf("InstallPack(new) error = %v", err)
	}
	modelRoot, err := store.modelRoot(newPack.ID)
	if err != nil {
		t.Fatalf("modelRoot() error = %v", err)
	}
	rollback, ok := readSemanticModelPackRollbackRecord(filepath.Join(modelRoot, semanticModelRollbackFile))
	if !ok || rollback.Replacement == nil || rollback.InstallID != newResult.InstallID {
		t.Fatal("read pending replacement")
	}
	committedRaw, err := json.MarshalIndent(rollback.Replacement, "", "  ")
	if err != nil {
		t.Fatalf("Marshal(replacement) error = %v", err)
	}
	committedRaw = append(committedRaw, '\n')
	if err := os.WriteFile(filepath.Join(modelRoot, semanticModelInstallFile), committedRaw, 0o600); err != nil {
		t.Fatalf("WriteFile(committed install) error = %v", err)
	}

	active, ok := store.ActiveProfile()
	if !ok || active.ModelPack == nil || active.ModelPack.Version != newPack.Version {
		t.Fatalf("ActiveProfile() with committed marker = %#v ok=%v, want new version", active, ok)
	}
	reopened, err := LoadOrCreateSemanticModelPackStore(dataDir)
	if err != nil {
		t.Fatalf("reopen committed model store: %v", err)
	}
	active, ok = reopened.ActiveProfile()
	if !ok || active.ModelPack == nil || active.ModelPack.Version != newPack.Version {
		t.Fatalf("ActiveProfile() after recovery = %#v ok=%v, want new version", active, ok)
	}
	if _, err := os.Stat(filepath.Join(modelRoot, semanticModelRollbackFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed rollback marker error = %v, want removed", err)
	}
}

func TestSemanticModelPackStoreRejectsEmptyVersionWithoutBreakingReopen(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	store, err := LoadOrCreateSemanticModelPackStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStore() error = %v", err)
	}
	artifact := []byte("model artifact")
	pack := semanticRuntimeStatusTestArchivePack("empty-version-model", "empty-version-model/d4", "", artifact)
	if _, err := store.InstallPack(context.Background(), pack, bytes.NewReader(artifact)); !errors.Is(err, ErrSemanticModelPackInvalid) {
		t.Fatalf("InstallPack(empty version) error = %v, want ErrSemanticModelPackInvalid", err)
	}
	if _, err := LoadOrCreateSemanticModelPackStore(dataDir); err != nil {
		t.Fatalf("reopen after rejected empty version: %v", err)
	}
}

func TestSemanticModelPackStoreSweepsOrphanedInstallDirectoriesOnStartup(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	store, err := LoadOrCreateSemanticModelPackStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStore() error = %v", err)
	}
	artifact := []byte("model artifact")
	pack := semanticRuntimeStatusTestArchivePack("orphan-sweep-model", "orphan-sweep-model/d4", "2026.07", artifact)
	result, err := store.InstallPack(context.Background(), pack, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("InstallPack() error = %v", err)
	}
	if err := store.FinalizePackInstall(pack.ID, pack.VectorSpaceID, result.InstallID); err != nil {
		t.Fatalf("FinalizePackInstall() error = %v", err)
	}
	modelRoot, err := store.modelRoot(pack.ID)
	if err != nil {
		t.Fatalf("modelRoot() error = %v", err)
	}
	installRecord, ok := readSemanticModelInstallRecord(filepath.Join(modelRoot, semanticModelInstallFile))
	if !ok {
		t.Fatal("read active model install record")
	}
	activeArtifactPath := installRecord.ArtifactPath
	orphanPath := filepath.Join(modelRoot, ".install-orphan")
	if err := os.MkdirAll(orphanPath, 0o700); err != nil {
		t.Fatalf("MkdirAll(orphan) error = %v", err)
	}
	if _, err := LoadOrCreateSemanticModelPackStore(dataDir); err != nil {
		t.Fatalf("reopen model pack store: %v", err)
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan model directory Stat() error = %v, want not exist", err)
	}
	if _, err := os.Stat(activeArtifactPath); err != nil {
		t.Fatalf("active model artifact removed during sweep: %v", err)
	}
}

func semanticRuntimeStatusTestPack() SemanticModelPackStatus {
	return SemanticModelPackStatus{
		ID:            "timich-multilingual-clip-small",
		Version:       "2026.06.01",
		VectorSpaceID: "timich-multilingual-clip-small/2026.06/d512",
		EmbeddingDim:  512,
		InputKind:     "image",
		Runtime:       "onnxruntime",
		Artifact: &SemanticModelArtifactStatus{
			Filename: "timich-multilingual-clip-small.zip",
		},
	}
}

func semanticRuntimeStatusTestArchivePack(id string, vectorSpaceID string, version string, artifact []byte) SemanticModelPackStatus {
	sum := sha256.Sum256(artifact)
	return SemanticModelPackStatus{
		ID:            id,
		Version:       version,
		VectorSpaceID: vectorSpaceID,
		EmbeddingDim:  4,
		InputKind:     "image",
		Runtime:       "onnxruntime",
		Artifact: &SemanticModelArtifactStatus{
			Filename:  id + ".tar.zst",
			SHA256:    hex.EncodeToString(sum[:]),
			SizeBytes: int64(len(artifact)),
		},
	}
}

func writeRuntimeLayoutForStatusTest(t *testing.T) string {
	t.Helper()

	return writeRuntimeLayoutForStatusTestWithRuntime(t, semanticRuntimeLoaderONNXRuntime)
}

func writeRuntimeLayoutForStatusTestWithRuntime(t *testing.T, runtime string) string {
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
		"runtime":       runtime,
		"imageModel":    "models/image.onnx",
		"textModel":     "models/text.onnx",
		"tokenizer":     "tokenizer/tokenizer.json",
	}
	raw, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(runtimePath, semanticModelPackLayoutFile), raw, 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", semanticModelPackLayoutFile, err)
	}
	return runtimePath
}

func semanticRuntimeStatusTestZipArtifact(t *testing.T) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	writeZipEntry := func(name string, payload []byte) {
		t.Helper()
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("Create(%q) error = %v", name, err)
		}
		if _, err := entry.Write(payload); err != nil {
			t.Fatalf("Write(%q) error = %v", name, err)
		}
	}
	layout := map[string]any{
		"schemaVersion": 1,
		"product":       "timich-semantic-model-pack",
		"modelId":       "timich-multilingual-clip-small",
		"vectorSpaceId": "timich-multilingual-clip-small/2026.06/d512",
		"embeddingDim":  4,
		"inputKind":     "image",
		"runtime":       "onnxruntime",
		"imageModel":    "models/image.onnx",
		"textModel":     "models/text.onnx",
		"tokenizer":     "tokenizer/tokenizer.json",
	}
	raw, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	writeZipEntry("timich-model.json", raw)
	writeZipEntry("models/image.onnx", []byte("image model"))
	writeZipEntry("models/text.onnx", []byte("text model"))
	writeZipEntry("tokenizer/tokenizer.json", []byte(`{"model":"test"}`))
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	return buffer.Bytes()
}

func writeRuntimeHelperForStatusTest(t *testing.T, response map[string]any) string {
	t.Helper()

	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	helperPath := filepath.Join(t.TempDir(), "timich-semantic-helper")
	script := "#!/bin/sh\nprintf '%s\\n' '" + string(raw) + "'\n"
	if err := os.WriteFile(helperPath, []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return helperPath
}

func writeEmbeddingRuntimeHelperForStatusTest(t *testing.T) string {
	t.Helper()

	helperPath := filepath.Join(t.TempDir(), "timich-semantic-helper")
	script := `#!/bin/sh
case "$1" in
  inspect)
    printf '%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"timich-multilingual-clip-small","vectorSpaceId":"timich-multilingual-clip-small/2026.06/d512","embeddingDim":4,"inputKind":"image","loaded":true,"canEmbed":true}'
    ;;
  embed-image)
    cat >/dev/null
    printf '%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"timich-multilingual-clip-small","vectorSpaceId":"timich-multilingual-clip-small/2026.06/d512","embeddingDim":4,"inputKind":"image","vector":[2,0,0,0],"input":"image-helper"}'
    ;;
  embed-text)
    printf '%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"timich-multilingual-clip-small","vectorSpaceId":"timich-multilingual-clip-small/2026.06/d512","embeddingDim":4,"inputKind":"image","vector":[0,3,0,0],"input":"text-helper"}'
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(helperPath, []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return helperPath
}
