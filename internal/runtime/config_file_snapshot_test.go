package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigFileSnapshotRestoresExistingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	original := []byte("{\"datasources\":[{\"name\":\"original\"}]}\n")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatalf("WriteFile(original) error = %v", err)
	}
	snapshot, err := snapshotConfigFile(path)
	if err != nil {
		t.Fatalf("snapshotConfigFile() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("{\"datasources\":[]}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(mutated) error = %v", err)
	}
	if err := snapshot.restore(); err != nil {
		t.Fatalf("restore() error = %v", err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(restored) error = %v", err)
	}
	if string(restored) != string(original) {
		t.Fatalf("restored contents = %q, want %q", restored, original)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(restored) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("restored mode = %03o, want 640", got)
	}
}

func TestConfigFileSnapshotRemovesFileCreatedByFailedMutation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	snapshot, err := snapshotConfigFile(path)
	if err != nil {
		t.Fatalf("snapshotConfigFile() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(created) error = %v", err)
	}
	if err := snapshot.restore(); err != nil {
		t.Fatalf("restore() error = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(restored absent file) error = %v, want not exist", err)
	}
}

func TestApplyConfigFileMutationRestoresFileAfterPostReplacementError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	before := []byte("before\n")
	if err := os.WriteFile(path, before, 0o640); err != nil {
		t.Fatalf("WriteFile(before) error = %v", err)
	}
	snapshot, err := snapshotConfigFile(path)
	if err != nil {
		t.Fatalf("snapshotConfigFile() error = %v", err)
	}

	writeErr := errors.New("sync parent directory after rename")
	err = applyConfigFileMutation(snapshot, func() error {
		if err := os.WriteFile(path, []byte("after\n"), 0o600); err != nil {
			return err
		}
		return writeErr
	})
	if !errors.Is(err, writeErr) {
		t.Fatalf("applyConfigFileMutation() error = %v, want %v", err, writeErr)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(restored) error = %v", err)
	}
	if string(raw) != string(before) {
		t.Fatalf("restored content = %q, want %q", raw, before)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(restored) error = %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("restored mode = %o, want 640", info.Mode().Perm())
	}
}

func TestApplyConfigFileMutationRemovesFileCreatedBeforeError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent.json")
	snapshot, err := snapshotConfigFile(path)
	if err != nil {
		t.Fatalf("snapshotConfigFile() error = %v", err)
	}

	writeErr := errors.New("sync parent directory after rename")
	err = applyConfigFileMutation(snapshot, func() error {
		if err := os.WriteFile(path, []byte("created\n"), 0o600); err != nil {
			return err
		}
		return writeErr
	})
	if !errors.Is(err, writeErr) {
		t.Fatalf("applyConfigFileMutation() error = %v, want %v", err, writeErr)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(restored missing file) error = %v, want not exist", err)
	}
}
