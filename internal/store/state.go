package store

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rsahara/timich-agent/internal/atomicfile"
)

const stateFileName = "agent-state.json"

// State holds the persistent local-only identity and secrets for the agent.
type State struct {
	AgentID                 string     `json:"agentId"`
	CreatedAt               time.Time  `json:"createdAt"`
	SessionSigningKey       string     `json:"sessionSigningKey"`
	AdminToken              string     `json:"adminToken"`
	RelayKeyID              string     `json:"relayKeyId"`
	RelayPrivateKey         string     `json:"relayPrivateKey"`
	RelayPublicKey          string     `json:"relayPublicKey"`
	RelayCredentialSyncedAt *time.Time `json:"relayCredentialSyncedAt,omitempty"`
}

// LoadedState bundles the persisted state and the file path it came from.
type LoadedState struct {
	Path  string
	State State
}

// LoadOrCreate loads the persistent agent state or creates it on first run.
func LoadOrCreate(dataDir string) (LoadedState, error) {
	if dataDir == "" {
		return LoadedState{}, errors.New("data directory must not be empty")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return LoadedState{}, fmt.Errorf("create data directory: %w", err)
	}

	path := filepath.Join(dataDir, stateFileName)
	if raw, err := os.ReadFile(path); err == nil {
		var state State
		if err := json.Unmarshal(raw, &state); err != nil {
			return LoadedState{}, fmt.Errorf("parse state file %s: %w", path, err)
		}
		if state.AgentID == "" || state.SessionSigningKey == "" {
			return LoadedState{}, fmt.Errorf("state file %s is missing required fields", path)
		}
		if state.RelayKeyID == "" || state.RelayPrivateKey == "" || state.RelayPublicKey == "" {
			if err := ensureRelayCredential(&state); err != nil {
				return LoadedState{}, err
			}
			if err := writeStateFile(path, state); err != nil {
				return LoadedState{}, err
			}
		}
		return LoadedState{Path: path, State: state}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return LoadedState{}, fmt.Errorf("read state file %s: %w", path, err)
	}

	state, err := generateState()
	if err != nil {
		return LoadedState{}, err
	}
	if err := writeStateFile(path, state); err != nil {
		return LoadedState{}, err
	}
	return LoadedState{Path: path, State: state}, nil
}

func generateState() (State, error) {
	agentID, err := randomHex(16)
	if err != nil {
		return State{}, fmt.Errorf("generate agent id: %w", err)
	}
	signingKey, err := randomBase64(32)
	if err != nil {
		return State{}, fmt.Errorf("generate session signing key: %w", err)
	}
	state := State{
		AgentID:           agentID,
		CreatedAt:         time.Now().UTC(),
		SessionSigningKey: signingKey,
	}
	if err := ensureRelayCredential(&state); err != nil {
		return State{}, err
	}
	return state, nil
}

func ensureRelayCredential(state *State) error {
	keyID, err := randomHex(16)
	if err != nil {
		return fmt.Errorf("generate relay key id: %w", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate relay signing key: %w", err)
	}
	state.RelayKeyID = keyID
	state.RelayPrivateKey = base64.RawStdEncoding.EncodeToString(privateKey)
	state.RelayPublicKey = base64.RawStdEncoding.EncodeToString(publicKey)
	state.RelayCredentialSyncedAt = nil
	return nil
}

// SaveLoadedState persists an updated loaded state back to its original path.
func SaveLoadedState(loaded LoadedState) error {
	if loaded.Path == "" {
		return errors.New("state path must not be empty")
	}
	return writeStateFile(loaded.Path, loaded.State)
}

func writeStateFile(path string, state State) error {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	raw = append(raw, '\n')

	if err := atomicfile.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write state file %s: %w", path, err)
	}
	return nil
}

func randomHex(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func randomBase64(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(bytes), nil
}
