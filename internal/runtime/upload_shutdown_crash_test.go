package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/store"
)

type exitDuringUploadReader struct{}

func (exitDuringUploadReader) Read([]byte) (int, error) { os.Exit(23); return 0, io.EOF }

func TestUploadShutdownCrashPreservesUnconfirmedTailBoundary(t *testing.T) {
	dir := os.Getenv("TIMICH_TEST_UPLOAD_CRASH_DIR")
	child := dir != ""
	if !child {
		dir = t.TempDir()
	}
	cfg := config.ResolvedConfig{Config: config.Default()}
	cfg.DataDir = filepath.Join(dir, "state")
	cfg.ConfigPath = filepath.Join(dir, "agent.json")
	cfg.Hosted.Enabled = true
	cfg.Hosted.ServerURL = "https://timich.example"
	cfg.UploadRoots = []config.UploadRootConfig{{Key: "photos", Path: filepath.Join(dir, "photos")}}
	if err := os.MkdirAll(cfg.UploadRoots[0].Path, 0700); err != nil {
		t.Fatal(err)
	}
	body := []byte("abcdef")
	if child {
		state, err := store.LoadOrCreate(cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		a, err := NewAgentRuntime(BuildInfo{}, cfg, state, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		device := addConcurrentUploadDevice(t, a)
		upload := startConcurrentUpload(t, a, device, "crash", body)
		meta, _ := json.Marshal([]string{device, upload.UploadID})
		if err := os.WriteFile(filepath.Join(dir, "fixture.json"), meta, 0600); err != nil {
			t.Fatal(err)
		}
		_, _ = a.AppendUploadChunk(device, upload.UploadID, concurrentChunk(body, io.MultiReader(bytes.NewReader(body[:2]), exitDuringUploadReader{})))
		os.Exit(24)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUploadShutdownCrashPreservesUnconfirmedTailBoundary$")
	cmd.Env = append(os.Environ(), "TIMICH_TEST_UPLOAD_CRASH_DIR="+dir)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("child=%v output=%s", err, output)
	}
	meta, err := os.ReadFile(filepath.Join(dir, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := json.Unmarshal(meta, &ids); err != nil {
		t.Fatal(err)
	}
	state, err := store.LoadOrCreate(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAgentRuntime(BuildInfo{}, cfg, state, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if got := mustConcurrentSession(t, a, ids[1]).NextOffset; got != 0 {
		t.Fatalf("saved offset=%d", got)
	}
	temp := filepath.Join(cfg.UploadRoots[0].Path, ".timich-upload-tmp", ids[1]+".part")
	before, err := os.ReadFile(temp)
	if err != nil || !bytes.Equal(before, body[:2]) {
		t.Fatalf("crash tail=%q err=%v", before, err)
	}
	_, err = a.AppendUploadChunk(ids[0], ids[1], concurrentChunk(body, bytes.NewReader(body)))
	if !errors.Is(err, store.ErrUploadSessionOffsetConflict) {
		t.Fatalf("retry=%v, want explicit offset conflict", err)
	}
	after, err := os.ReadFile(temp)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("retry modified unconfirmed file: %q %v", after, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.UploadRoots[0].Path, "photo.jpg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected final: %v", err)
	}
}
