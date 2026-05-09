package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCLIVersion(t *testing.T) {
	originalVersion := version
	version = "test-version"
	t.Cleanup(func() { version = originalVersion })

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"version"}, &stdout, &stderr); err != nil {
		t.Fatalf("runCLI() error = %v stderr=%s", err, stderr.String())
	}
	if stdout.String() != "test-version\n" {
		t.Fatalf("stdout = %q, want version line", stdout.String())
	}
}

func TestRunCLIVersionJSON(t *testing.T) {
	originalVersion := version
	originalCommit := commit
	originalBuiltAt := builtAt
	version = "test-version"
	commit = "test-commit"
	builtAt = "2026-04-25T00:00:00Z"
	t.Cleanup(func() {
		version = originalVersion
		commit = originalCommit
		builtAt = originalBuiltAt
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"version-json"}, &stdout, &stderr); err != nil {
		t.Fatalf("runCLI() error = %v stderr=%s", err, stderr.String())
	}

	var payload map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v output=%s", err, stdout.String())
	}
	if payload["version"] != "test-version" || payload["commit"] != "test-commit" {
		t.Fatalf("payload = %v, want test build info", payload)
	}

	want := "{\"version\":\"test-version\",\"commit\":\"test-commit\",\"builtAt\":\"2026-04-25T00:00:00Z\"}\n"
	if stdout.String() != want {
		t.Fatalf("version-json output = %q, want %q", stdout.String(), want)
	}
}

func TestRunCLIInitWritesConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "agent.json")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{"init", "-config", configPath, "-data-dir", "state"}, &stdout, &stderr); err != nil {
		t.Fatalf("runCLI() error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Wrote starter config") {
		t.Fatalf("stdout missing confirmation: %s", stdout.String())
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
}

func TestLoadConfigForServeUsesProvidedFlagOutput(t *testing.T) {
	var stderr bytes.Buffer
	_, err := loadConfigForServe([]string{"-bad-flag"}, &stderr)
	if err == nil {
		t.Fatal("loadConfigForServe() error = nil, want flag parse error")
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("stderr = %q, want flag parse output", stderr.String())
	}
}

func TestAdminSetupURLUsesLocalhostForWildcardListenAddress(t *testing.T) {
	got := adminSetupURL("0.0.0.0:8081")
	if got != "http://localhost:8081/" {
		t.Fatalf("adminSetupURL() = %q, want localhost URL", got)
	}
}

func TestAdminSetupURLKeepsSpecificListenAddress(t *testing.T) {
	got := adminSetupURL("127.0.0.1:18081")
	if got != "http://127.0.0.1:18081/" {
		t.Fatalf("adminSetupURL() = %q, want specific URL", got)
	}
}
