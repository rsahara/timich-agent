package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rsahara/timich-agent/internal/atomicfile"
)

type configFileSnapshot struct {
	path    string
	raw     []byte
	mode    os.FileMode
	existed bool
}

func snapshotConfigFile(configPath string) (configFileSnapshot, error) {
	path := strings.TrimSpace(configPath)
	if path == "" {
		return configFileSnapshot{}, errors.New("config path must not be empty")
	}
	resolvedPath, err := filepath.Abs(path)
	if err != nil {
		return configFileSnapshot{}, fmt.Errorf("resolve config snapshot path: %w", err)
	}
	snapshot := configFileSnapshot{path: resolvedPath, mode: 0o600}
	info, err := os.Stat(resolvedPath)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return configFileSnapshot{}, fmt.Errorf("inspect config snapshot: %w", err)
	}
	raw, err := os.ReadFile(resolvedPath)
	if err != nil {
		return configFileSnapshot{}, fmt.Errorf("read config snapshot: %w", err)
	}
	snapshot.raw = raw
	snapshot.mode = info.Mode().Perm()
	snapshot.existed = true
	return snapshot, nil
}

func (snapshot configFileSnapshot) restore() error {
	if strings.TrimSpace(snapshot.path) == "" {
		return errors.New("config snapshot path must not be empty")
	}
	if snapshot.existed {
		if err := atomicfile.WriteFile(snapshot.path, snapshot.raw, snapshot.mode); err != nil {
			return fmt.Errorf("restore config snapshot: %w", err)
		}
		return nil
	}
	if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove config created by failed mutation: %w", err)
	}
	return nil
}

func applyConfigFileMutation(snapshot configFileSnapshot, mutate func() error) error {
	if err := mutate(); err != nil {
		return errors.Join(err, snapshot.restore())
	}
	return nil
}
