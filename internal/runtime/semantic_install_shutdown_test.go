package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestSemanticInstallShutdownRollsBackProbeAndAllowsRetry(t *testing.T) {
	serverPath, pythonPath := writeSemanticONNXRuntimeExecutables(t)
	probeStarted := make(chan struct{})
	notify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(probeStarted)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer notify.Close()
	blockedProbePath := filepath.Join(t.TempDir(), "blocked-probe.py")
	// The candidate is durably staged before this real child starts. It cannot
	// become healthy, so shutdown must cancel the probe, reap it, and roll back.
	script := fmt.Sprintf("import urllib.request, time\nurllib.request.urlopen(%q).close()\ntime.sleep(60)\n", notify.URL)
	if err := os.WriteFile(blockedProbePath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newTestAgentRuntimeWithConfig(t, BuildInfo{}, nil, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.ONNXRuntime.ServerPath = blockedProbePath
		cfg.SemanticRuntime.ONNXRuntime.PythonPath = pythonPath
	})
	oldPack := runtimeSemanticPackForTest("shutdown-upgrade")
	installStoredRuntimeSemanticPackForTest(t, a, oldPack)
	if _, err := a.semanticModels.ActivatePack(oldPack.ID, oldPack.VectorSpaceID); err != nil {
		t.Fatal(err)
	}
	newPack := runtimeSemanticPackForTest(oldPack.ID)
	newPack.Version = "2026.07.19"
	artifact := runtimeSemanticPackArtifactForTest(t, newPack)
	sum := sha256.Sum256(artifact)
	newPack.Artifact.SHA256 = hex.EncodeToString(sum[:])
	newPack.Artifact.SizeBytes = int64(len(artifact))
	newPack.SizeBytes = int64(len(artifact))
	ctx, release, ok := a.AcquireBackgroundWork()
	if !ok {
		t.Fatal("install admission failed")
	}
	result := make(chan error, 1)
	go func() {
		defer release()
		_, err := a.InstallSemanticModelPack(ctx, newPack, bytes.NewReader(artifact))
		result <- err
	}()
	select {
	case <-probeStarted:
	case err := <-result:
		t.Fatalf("install returned before probe: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("candidate probe never started")
	}
	a.BeginShutdown()
	wait, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	if err := a.WaitStopped(wait); err != nil {
		t.Fatalf("probe cleanup did not finish: %v", err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("install error=%v, want cancellation", err)
	}
	active, ok := a.semanticModels.InstalledActiveProfile()
	if !ok || active.ModelPack == nil || active.ModelPack.Version != oldPack.Version {
		t.Fatalf("canceled replacement lost committed model: %+v", active)
	}
	if err := a.CloseResources(wait); err != nil {
		t.Fatal(err)
	}

	// Reopen the same durable state and retry the replacement with a healthy
	// probe. A subsequent shutdown/reopen must keep the committed replacement.
	cfg := a.config
	cfg.SemanticRuntime.ONNXRuntime.ServerPath = serverPath
	reopen := func() *AgentRuntime {
		runtime, err := NewAgentRuntime(BuildInfo{}, cfg, a.state, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := runtime.Close(); err != nil {
				t.Error(err)
			}
		})
		return runtime
	}
	retry := reopen()
	retryCtx, stopRetry := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopRetry()
	if _, err := retry.InstallSemanticModelPack(retryCtx, newPack, bytes.NewReader(artifact)); err != nil {
		t.Fatalf("retry after interrupted install: %v", err)
	}
	if err := retry.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := reopen()
	active, ok = restarted.semanticModels.InstalledActiveProfile()
	if !ok || active.ModelPack == nil || active.ModelPack.Version != newPack.Version {
		t.Fatalf("shutdown lost committed replacement: %+v", active)
	}
}
