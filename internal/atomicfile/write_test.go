package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileWritesContentAndPermissions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteFile(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(raw) != "hello\n" {
		t.Fatalf("content = %q, want hello", string(raw))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestWriteFileReturnsCreateError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing", "state.json")
	if err := WriteFile(path, []byte("hello\n"), 0o600); err == nil {
		t.Fatal("WriteFile() error = nil, want create error")
	}
}
