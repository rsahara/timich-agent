package runtime

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestONNXShutdownReapsChildrenWithinSharedBudget(t *testing.T) {
	if os.Getenv("TIMICH_TEST_ONNX_STUB") == "1" {
		signal.Ignore(os.Interrupt)
		fmt.Fprintln(os.Stdout, "READY")
		for {
			time.Sleep(time.Hour)
		}
	}
	m := newSemanticONNXRuntimeManager(config.SemanticONNXRuntimeConfig{}, nil, nil)
	for n := range 2 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestONNXShutdownReapsChildrenWithinSharedBudget$")
		cmd.Env = append(os.Environ(), "TIMICH_TEST_ONNX_STUB=1")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		key := fmt.Sprint(n)
		process := &managedSemanticONNXRuntimeProcess{cmd: cmd, done: make(chan struct{}), status: semanticONNXRuntimeStatusRunning}
		m.mu.Lock()
		m.processes[key] = process
		m.mu.Unlock()
		go m.waitForProcess(key, process, cmd)
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() || scanner.Text() != "READY" {
			t.Fatal("child not ready")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := m.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(m.processes) != 0 || !m.stopping.Load() {
		t.Fatal("manager did not shut down")
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("children consumed entire budget: %v", err)
	}
}
