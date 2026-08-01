//go:build unix

package catalog

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestKillHelperProcessGroupPreservesCompletedProcessState(t *testing.T) {
	command := exec.Command("sleep", "30")
	configureHelperProcessGroup(command)
	if err := command.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	if err := killHelperProcessGroup(command); err != nil {
		t.Fatalf("kill active helper process group: %v", err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("wait for killed helper process succeeded, want signal failure")
	}
	if err := killHelperProcessGroup(command); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill completed helper process group error = %v, want os.ErrProcessDone", err)
	}
}

func TestRunLocalMediaHelperCommandContextKillsChildProcessGroupOnTimeout(t *testing.T) {
	tempDir := t.TempDir()
	pidPath := filepath.Join(tempDir, "child.pid")
	scriptPath := filepath.Join(tempDir, "helper.sh")
	script := `#!/bin/sh
sleep 30 &
echo "$!" > "$TIMICH_TEST_CHILD_PID_FILE"
wait
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	t.Setenv("TIMICH_TEST_CHILD_PID_FILE", pidPath)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runLocalMediaHelperCommandContext(ctx, 30*time.Second, scriptPath, "", "")
		done <- err
	}()

	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		body, readErr := os.ReadFile(pidPath)
		if readErr == nil {
			parsed, parseErr := strconv.Atoi(strings.TrimSpace(string(body)))
			if parseErr != nil {
				t.Fatalf("parse child pid: %v", parseErr)
			}
			pid = parsed
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		cancel()
		t.Fatalf("helper did not write child pid")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context canceled, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("helper command did not stop after context cancellation")
	}

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("child process %d survived helper timeout", pid)
}

func TestRunLocalMediaHelperCommandWithContextRespectsCallerContext(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "helper.sh")
	script := "#!/bin/sh\nsleep 2\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runLocalMediaHelperCommandWithContext(ctx, scriptPath, "", "")
	elapsed := time.Since(started)

	if elapsed >= time.Second {
		t.Fatalf("helper command took %s, want caller context to stop it quickly", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("helper command error = %v, want context deadline exceeded", err)
	}
}
