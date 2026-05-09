package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOrCreatePersistsAgentIdentity(t *testing.T) {
	dataDir := t.TempDir()

	first, err := LoadOrCreate(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreate() first error = %v", err)
	}
	second, err := LoadOrCreate(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreate() second error = %v", err)
	}

	if first.State.AgentID == "" {
		t.Fatal("AgentID should not be empty")
	}
	if first.State.AdminToken != "" {
		t.Fatal("AdminToken should start empty for first-run browser setup")
	}
	if first.State.AgentID != second.State.AgentID {
		t.Fatalf("AgentID changed across loads: %q != %q", first.State.AgentID, second.State.AgentID)
	}
	if first.State.SessionSigningKey != second.State.SessionSigningKey {
		t.Fatal("SessionSigningKey changed across loads")
	}
	if first.State.RelayKeyID == "" || first.State.RelayPrivateKey == "" || first.State.RelayPublicKey == "" {
		t.Fatalf("relay credential should be generated on first run: %#v", first.State)
	}
	if first.State.RelayKeyID != second.State.RelayKeyID {
		t.Fatalf("RelayKeyID changed across loads: %q != %q", first.State.RelayKeyID, second.State.RelayKeyID)
	}
	if first.State.RelayPrivateKey != second.State.RelayPrivateKey || first.State.RelayPublicKey != second.State.RelayPublicKey {
		t.Fatal("relay signing key changed across loads")
	}
	if first.State.AdminToken != second.State.AdminToken {
		t.Fatal("AdminToken changed across loads")
	}
}

func TestLoadOrCreateKeepsLegacyAdminTokenEmpty(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, stateFileName)
	legacyState := State{
		AgentID:           "agent-legacy",
		CreatedAt:         time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC),
		SessionSigningKey: "legacy-signing-key",
	}
	raw, err := json.Marshal(legacyState)
	if err != nil {
		t.Fatalf("marshal legacy state: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}

	loaded, err := LoadOrCreate(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	if loaded.State.AdminToken != "" {
		t.Fatal("AdminToken should remain empty for browser setup")
	}
	if loaded.State.RelayKeyID == "" || loaded.State.RelayPrivateKey == "" || loaded.State.RelayPublicKey == "" {
		t.Fatalf("legacy state should be migrated with relay credential: %#v", loaded.State)
	}
	if loaded.State.AgentID != legacyState.AgentID || loaded.State.SessionSigningKey != legacyState.SessionSigningKey {
		t.Fatalf("legacy identity changed: %#v", loaded.State)
	}
}

func TestSaveLoadedStatePersistsAdminToken(t *testing.T) {
	dataDir := t.TempDir()
	loaded, err := LoadOrCreate(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	loaded.State.AdminToken = "admin-token-long-enough"
	if err := SaveLoadedState(loaded); err != nil {
		t.Fatalf("SaveLoadedState() error = %v", err)
	}

	reloaded, err := LoadOrCreate(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreate() reload error = %v", err)
	}
	if reloaded.State.AdminToken != "admin-token-long-enough" {
		t.Fatalf("AdminToken = %q, want saved token", reloaded.State.AdminToken)
	}
}
