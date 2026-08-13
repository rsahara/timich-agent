package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
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
	originalReleaseTag := releaseTag
	version = "test-version"
	commit = "test-commit"
	builtAt = "2026-04-25T00:00:00Z"
	releaseTag = "v0.4.0-rc.2"
	t.Cleanup(func() {
		version = originalVersion
		commit = originalCommit
		builtAt = originalBuiltAt
		releaseTag = originalReleaseTag
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

	want := "{\"version\":\"test-version\",\"commit\":\"test-commit\",\"builtAt\":\"2026-04-25T00:00:00Z\",\"releaseTag\":\"v0.4.0-rc.2\"}\n"
	if stdout.String() != want {
		t.Fatalf("version-json output = %q, want %q", stdout.String(), want)
	}
}

func TestRunCLIPreReleaseCatalogMigrationRequiresOfflineConfirmation(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{
		"pre-release-migrate-catalog-v2-v3",
		"--data-dir", "/data",
	}, &stdout, &stderr); err == nil {
		t.Fatal("runCLI() error = nil, want offline confirmation")
	}
}

func TestRunCLIPreReleaseCatalogMigrationWritesVersionedJSON(t *testing.T) {
	originalMigrate := migratePreReleaseCatalog
	originalVersion := version
	originalCommit := commit
	migratePreReleaseCatalog = func(_ context.Context, dataDir string, backupPath string) (catalog.CatalogPreReleaseMigrationResult, error) {
		if dataDir != "/data" || backupPath != "/backup/catalog-v2.db" {
			t.Fatalf("migration args = %q %q", dataDir, backupPath)
		}
		return catalog.CatalogPreReleaseMigrationResult{
			FromVersion:                 2,
			ToVersion:                   3,
			BackupPath:                  backupPath,
			CatalogAssetCount:           300000,
			ActiveSemanticManifestCount: 1,
			SemanticMembershipCount:     96300,
		}, nil
	}
	version = "0.4.0"
	commit = "test-migration-commit"
	t.Cleanup(func() {
		migratePreReleaseCatalog = originalMigrate
		version = originalVersion
		commit = originalCommit
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runCLI([]string{
		"pre-release-migrate-catalog-v2-v3",
		"--data-dir", "/data",
		"--backup", "/backup/catalog-v2.db",
		"--confirm-agent-stopped",
	}, &stdout, &stderr); err != nil {
		t.Fatalf("runCLI() error = %v stderr=%s", err, stderr.String())
	}
	var result struct {
		catalog.CatalogPreReleaseMigrationResult
		AgentVersion string `json:"agentVersion"`
		AgentCommit  string `json:"agentCommit"`
	}
	if err := json.NewDecoder(bytes.NewReader(stdout.Bytes())).Decode(&result); err != nil {
		t.Fatalf("decode migration result: %v", err)
	}
	if result.ToVersion != 3 || result.SemanticMembershipCount != 96300 ||
		result.AgentVersion != "0.4.0" || result.AgentCommit != "test-migration-commit" {
		t.Fatalf("migration result = %#v", result)
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

func TestConfigureRuntimeTempDirSetsSQLiteAndOSTempWhenUnset(t *testing.T) {
	t.Setenv("SQLITE_TMPDIR", "")
	t.Setenv("TMPDIR", "")

	dataDir := t.TempDir()
	got, err := configureRuntimeTempDir(dataDir)
	if err != nil {
		t.Fatalf("configureRuntimeTempDir() error = %v", err)
	}

	want := filepath.Join(dataDir, "tmp")
	if got != want {
		t.Fatalf("configureRuntimeTempDir() = %q, want %q", got, want)
	}
	if os.Getenv("SQLITE_TMPDIR") != want {
		t.Fatalf("SQLITE_TMPDIR = %q, want %q", os.Getenv("SQLITE_TMPDIR"), want)
	}
	if os.Getenv("TMPDIR") != want {
		t.Fatalf("TMPDIR = %q, want %q", os.Getenv("TMPDIR"), want)
	}
	if info, err := os.Stat(want); err != nil {
		t.Fatalf("Stat(temp dir) error = %v", err)
	} else if !info.IsDir() {
		t.Fatalf("runtime temp path is not a directory")
	}
}

func TestConfigureRuntimeTempDirPreservesExplicitEnv(t *testing.T) {
	sqliteTemp := filepath.Join(t.TempDir(), "sqlite-temp")
	osTemp := filepath.Join(t.TempDir(), "os-temp")
	t.Setenv("SQLITE_TMPDIR", sqliteTemp)
	t.Setenv("TMPDIR", osTemp)

	if _, err := configureRuntimeTempDir(t.TempDir()); err != nil {
		t.Fatalf("configureRuntimeTempDir() error = %v", err)
	}
	if os.Getenv("SQLITE_TMPDIR") != sqliteTemp {
		t.Fatalf("SQLITE_TMPDIR = %q, want explicit %q", os.Getenv("SQLITE_TMPDIR"), sqliteTemp)
	}
	if os.Getenv("TMPDIR") != osTemp {
		t.Fatalf("TMPDIR = %q, want explicit %q", os.Getenv("TMPDIR"), osTemp)
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

func TestLoadConfigForServeRejectsImmichPassthroughWithAdditionalDatasource(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "agent.json")
	raw := `{
  "datasources": [
    {"sourceKey":"1111111111111111","name":"Home Immich","kind":"immich","url":"http://immich.local:2283","accessToken":"key"},
    {"sourceKey":"2222222222222222","name":"Other Immich","kind":"immich_indexed","url":"http://other-immich.local:2283","accessToken":"key"}
  ]
}`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	var stderr bytes.Buffer
	_, err := loadConfigForServe([]string{"-config", configPath}, &stderr)
	if !errors.Is(err, config.ErrImmichPassthroughRequiresSingleDatasource) {
		t.Fatalf("loadConfigForServe() error = %v, want passthrough topology error", err)
	}
	if !strings.Contains(err.Error(), config.DatasourceKindImmichIndexed) {
		t.Fatalf("loadConfigForServe() error = %q, want actionable conversion guidance", err)
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
