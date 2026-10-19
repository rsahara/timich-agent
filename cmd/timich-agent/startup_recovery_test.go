package main

import (
	"context"
	"database/sql"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/store"
)

// Exercise main's real exit behavior without building a second executable.
func TestStartupRecoveryAgentProcess(t *testing.T) {
	if os.Getenv("TIMICH_TEST_STARTUP_RECOVERY") != "1" {
		return
	}
	os.Args = []string{"timich-agent", "serve", "--config", os.Getenv("TIMICH_TEST_STARTUP_CONFIG")}
	main()
}

func TestCLIStartupRecoveryFailureAndRelaunch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("media helper fixture requires a POSIX shell")
	}
	cfg := config.ResolvedConfig{Config: config.Default()}
	cfg.DataDir = t.TempDir()
	cfg.ConfigPath = filepath.Join(cfg.DataDir, "agent.json")
	cfg.Hosted.Enabled = false
	workers := 1
	cfg.WorkerRuntime.HeavyTaskWorkers = &workers
	root := t.TempDir()
	cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "photos", Path: root}}
	cfg.Datasources = []config.DatasourceConfig{{
		SourceKey: "1111111111111111", Name: "Photos", Kind: config.DatasourceKindLocalFiles,
		RootKey: "photos", Scan: &config.LocalDatasourceScanConfig{SettlingDuration: "1ns"},
	}}
	for index, name := range []string{"first.jpg", "second.jpg"} {
		file, err := os.Create(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		photo := image.NewRGBA(image.Rect(0, 0, 96, 64))
		draw.Draw(photo, photo.Bounds(), image.NewUniform(color.RGBA{R: uint8(80 + index*80), A: 255}), image.Point{}, draw.Src)
		err = jpeg.Encode(file, photo, nil)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("write JPEG: %v, %v", err, closeErr)
		}
	}
	cfg.MediaRuntime.HelperPath = writeStartupRecoveryMediaHelper(t)
	state, err := store.LoadOrCreate(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	state.State.AdminToken = "startup-recovery-test-token"
	if err := store.SaveLoadedState(state); err != nil {
		t.Fatal(err)
	}
	seed, err := catalog.NewServiceWithOptions(cfg.Datasources, catalog.ServiceOptions{
		DataDir: cfg.DataDir, LocalRoots: cfg.LocalMediaRoots, MediaHelperPath: cfg.MediaRuntime.HelperPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { seed.Close() })
	if _, err := seed.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatal(err)
	}
	if result, err := seed.RunLocalMetadataBatch(context.Background(), 1); err != nil || result.RegisteredAssets != 1 {
		t.Fatalf("seed one registered photo and one pending metadata job: %+v, %v", result, err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, "catalog-state-v1", "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO local_scan_jobs (
		source_key, job_kind, priority, root_key, root_generation, asset_id, status, scheduled_at, sort_at
	) SELECT a.source_key, 'thumbnail', 1, rs.root_key, rs.root_generation, a.asset_id,
		'queued', '2026-10-18T00:00:00Z', a.captured_at
		FROM local_assets a JOIN local_scan_root_state rs ON rs.source_key = a.source_key`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE local_scan_jobs SET status = 'running', attempts = 2,
		locked_at = '2026-10-18T00:00:00Z' WHERE job_kind IN ('metadata', 'thumbnail')`); err != nil {
		t.Fatal(err)
	}
	var running int
	if err := db.QueryRow(`SELECT COUNT(*) FROM local_scan_jobs WHERE status = 'running'`).Scan(&running); err != nil || running != 2 {
		t.Fatalf("seeded running jobs = %d, %v; want 2", running, err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_startup_recovery BEFORE UPDATE OF status ON local_scan_jobs
		WHEN OLD.status = 'running' AND NEW.status = 'queued'
		BEGIN SELECT RAISE(ABORT, 'injected startup recovery failure'); END`); err != nil {
		t.Fatal(err)
	}
	adminAddress := reserveStartupRecoveryAddress(t)
	cfg.AdminListenAddress = adminAddress
	cfg.MediaListenAddress = "127.0.0.1:0"
	if err := config.WriteFile(cfg.ConfigPath, cfg.Config); err != nil {
		t.Fatal(err)
	}
	newProcess := func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStartupRecoveryAgentProcess$")
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "TIMICH_") && !strings.HasPrefix(value, "TMPDIR=") && !strings.HasPrefix(value, "SQLITE_TMPDIR=") {
				cmd.Env = append(cmd.Env, value)
			}
		}
		cmd.Env = append(cmd.Env, "TIMICH_TEST_STARTUP_RECOVERY=1", "TIMICH_TEST_STARTUP_CONFIG="+cfg.ConfigPath)
		return cmd
	}
	// A supervisor retry while the fault persists must also fail, not admit work.
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		output, err := newProcess(ctx).CombinedOutput()
		cancel()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("failed startup exit = %v; output=%s", err, output)
		}
		if !strings.Contains(string(output), "recover local scan jobs during startup") ||
			!strings.Contains(string(output), "injected startup recovery failure") {
			t.Fatalf("missing recovery cause: %s", output)
		}
		for _, started := range []string{"timich-agent started mode=", "background worker scheduler started"} {
			if strings.Contains(string(output), started) {
				t.Fatalf("failed startup admitted work: %s", output)
			}
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM local_scan_jobs WHERE status = 'running' AND attempts = 2`).Scan(&running); err != nil || running != 2 {
			t.Fatalf("failed startup changed interrupted jobs: count=%d, err=%v", running, err)
		}
	}
	if _, err := db.Exec(`DROP TRIGGER fail_startup_recovery`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := newProcess(ctx)
	logFile, err := os.Create(filepath.Join(t.TempDir(), "agent.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	defer func() {
		cancel()
		<-done
		if t.Failed() {
			output, _ := os.ReadFile(logFile.Name())
			t.Logf("Agent output: %s", output)
		}
	}()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(15 * time.Second)
	ready := false
	processed := false
	var remaining, completed int
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("Agent exited before processing recovered work: %v", waitErr)
		default:
		}
		response, err := client.Get("http://" + adminAddress + "/healthz")
		if err == nil {
			ready = response.StatusCode == http.StatusOK
			response.Body.Close()
		}
		err = db.QueryRow(`SELECT
			(SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind IN ('metadata', 'thumbnail')),
			(SELECT COUNT(*) FROM local_assets WHERE thumbnail_status = 'ready')`).Scan(&remaining, &completed)
		if ready && err == nil && remaining == 0 && completed == 2 {
			processed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !processed {
		t.Fatalf("relaunch did not resume automatic processing: API=%v remaining=%d ready thumbnails=%d", ready, remaining, completed)
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+adminAddress+"/v1/restart", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+state.State.AdminToken)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("restart status = %d", response.StatusCode)
	}
	select {
	case <-done:
		if waitErr != nil {
			t.Fatalf("normal shutdown after recovery: %v", waitErr)
		}
	case <-ctx.Done():
		t.Fatal("recovered Agent did not shut down")
	}
}

func reserveStartupRecoveryAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

func writeStartupRecoveryMediaHelper(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "timich-media-helper")
	script := `#!/bin/sh
case "$1" in
health)
  printf '%s\n' '{"schemaVersion":1,"ok":true,"helper":{"version":"test","platform":"test"},"capabilities":{"renderImage":true,"renderVideoPoster":false,"inspectImage":false,"inspectVideo":false}}'
  ;;
render-image)
  shift
  while [ "$#" -gt 0 ]; do
    case "$1" in
    --input) input="$2"; shift 2 ;;
    --output) output="$2"; shift 2 ;;
    *) shift ;;
    esac
  done
  cp "$input" "$output" || exit 1
  printf '%s\n' '{"schemaVersion":1,"ok":true,"operation":"render-image","backend":"libvips-cli","outputPath":"rendition.jpg"}'
  ;;
*) exit 2 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
