package controlplane

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/controlplanetransport"
)

// The integration runner builds both modules' test binaries, avoiding a Server
// dependency in the public Agent module. Run this test alone so its temporary CA
// is installed before Go loads the process-wide system root pool.
func TestRunRecoversAfterRealServerRejectsDuplicate(t *testing.T) {
	binary := os.Getenv("TIMICH_CONTROLPLANE_TEST_SERVER")
	if binary == "" {
		t.Skip("run tools/e2e/controlplane_reconnect.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	child := exec.CommandContext(ctx, binary, "-test.run=^TestControlPlaneRecoveryFixture$")
	child.Env = append(os.Environ(), "TIMICH_CONTROLPLANE_FIXTURE=1")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	child.Stderr = &stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if err := child.Wait(); err != nil {
			t.Errorf("Server fixture: %v\n%s", err, stderr.String())
		}
	})
	config := map[string]string{}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "CONTROLPLANE_FIXTURE ") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "CONTROLPLANE_FIXTURE ")), &config); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if config["target"] == "" {
		t.Fatal("Server fixture did not report its address")
	}
	// Drain the test runner's final PASS output before waiting for the child.
	go io.Copy(io.Discard, stdout)
	certPath := filepath.Join(t.TempDir(), "fixture-ca.pem")
	if err := os.WriteFile(certPath, []byte(config["root_pem"]), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", certPath)
	client := &Client{
		target: config["target"], hostedBaseURL: config["http_url"], agentID: "agent-home", version: "test",
		keyID: "key-1", publicKey: config["public_key"], privateKey: config["private_key"],
		tokenSigner: controlplanetransport.NewTokenSigner("agent-home", "key-1", config["private_key"], time.Minute),
		mediaHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil || r.Method != http.MethodPost || r.URL.Path != "/v1/assets/search" || string(body) != `{"page":1}` {
				http.Error(w, "unexpected relay request", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte("real-agent-relay"))
		}),
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- client.Run(runCtx) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Agent Run: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Agent did not stop")
		}
	})
	request := func(path string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, config["http_url"]+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, body
	}
	// The fixture keeps A alive until it observes the real Service reject B.
	if code, _ := request("/release-after-rejection"); code != http.StatusNoContent {
		t.Fatalf("release status=%d", code)
	}
	if code, body := request("/relay"); code != http.StatusOK || string(body) != "real-agent-relay" {
		t.Fatalf("recovered relay=%d %q", code, body)
	}
	_, body := request("/stats")
	var stats map[string]int
	if err := json.Unmarshal(body, &stats); err != nil {
		t.Fatal(err)
	}
	if stats["streams"] != 3 || stats["registrations"] != 2 {
		t.Fatalf("attempts=%v, want A + rejected B + accepted C", stats)
	}
}
