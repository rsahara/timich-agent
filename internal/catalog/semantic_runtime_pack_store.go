package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rsahara/timich-agent/internal/atomicfile"
)

const (
	semanticRuntimePacksDirName       = "semantic-runtime-packs"
	semanticRuntimeInstallFile        = "install.json"
	semanticRuntimeActiveFilePrefix   = "active-"
	semanticRuntimeRollbackFilePrefix = "rollback-"
)

var (
	ErrSemanticRuntimePackInvalid          = errors.New("semantic runtime pack invalid")
	ErrSemanticRuntimePackChecksumMismatch = errors.New("semantic runtime pack checksum mismatch")
	ErrSemanticRuntimePackSizeMismatch     = errors.New("semantic runtime pack size mismatch")
)

type SemanticRuntimePackStatus struct {
	ID          string                       `json:"id,omitempty"`
	Name        string                       `json:"name,omitempty"`
	Version     string                       `json:"version,omitempty"`
	Runtime     string                       `json:"runtime,omitempty"`
	Status      string                       `json:"status,omitempty"`
	Source      string                       `json:"source,omitempty"`
	SizeBytes   int64                        `json:"sizeBytes,omitempty"`
	License     string                       `json:"license,omitempty"`
	Platform    string                       `json:"platform,omitempty"`
	Artifact    *SemanticModelArtifactStatus `json:"artifact,omitempty"`
	Installed   bool                         `json:"installed"`
	InstalledAt time.Time                    `json:"installedAt,omitempty"`
	RuntimePath string                       `json:"runtimePath,omitempty"`
	ServerPath  string                       `json:"serverPath,omitempty"`
	PythonPath  string                       `json:"pythonPath,omitempty"`
}

type SemanticRuntimePackInstallResult struct {
	Status       string                    `json:"status"`
	RuntimePack  SemanticRuntimePackStatus `json:"runtimePack"`
	InstalledAt  time.Time                 `json:"installedAt"`
	BytesWritten int64                     `json:"bytesWritten"`
	InstallID    string                    `json:"-"`
}

type SemanticRuntimePackStore struct {
	root string
	mu   sync.Mutex
}

type semanticRuntimePackInstallRecord struct {
	InstallID    string                    `json:"installId,omitempty"`
	RuntimePack  SemanticRuntimePackStatus `json:"runtimePack"`
	ArtifactPath string                    `json:"artifactPath"`
	RuntimePath  string                    `json:"runtimePath"`
	ServerPath   string                    `json:"serverPath"`
	PythonPath   string                    `json:"pythonPath,omitempty"`
	InstalledAt  time.Time                 `json:"installedAt"`
	BytesWritten int64                     `json:"bytesWritten"`
}

type semanticRuntimePackRollbackRecord struct {
	InstallID          string                            `json:"installId"`
	Runtime            string                            `json:"runtime"`
	ReplacementID      string                            `json:"replacementId"`
	ReplacementVersion string                            `json:"replacementVersion"`
	PreviousActive     *semanticRuntimePackInstallRecord `json:"previousActive,omitempty"`
	PreviousIDInstall  *semanticRuntimePackInstallRecord `json:"previousIdInstall,omitempty"`
	ReplacementDirName string                            `json:"replacementDirName"`
	CreatedAt          time.Time                         `json:"createdAt"`
}

func LoadOrCreateSemanticRuntimePackStore(dataDir string) (*SemanticRuntimePackStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("data directory must not be empty")
	}
	root := filepath.Join(dataDir, semanticRuntimePacksDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create semantic runtime pack directory: %w", err)
	}
	store := &SemanticRuntimePackStore{root: root}
	if err := store.recoverInterruptedInstalls(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *SemanticRuntimePackStore) recoverInterruptedInstalls() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("list semantic runtime packs for recovery: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), semanticRuntimeRollbackFilePrefix) || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		rollbackPath := filepath.Join(s.root, entry.Name())
		rollback, ok := readSemanticRuntimePackRollbackRecord(rollbackPath)
		if !ok {
			return fmt.Errorf("%w: unreadable semantic runtime rollback record %q", ErrSemanticRuntimePackInvalid, entry.Name())
		}
		if s.semanticRuntimeInstallCommittedLocked(rollback) {
			if err := removeSemanticModelRoleRecord(rollbackPath); err != nil {
				log.Printf("timich-agent committed semantic runtime marker cleanup deferred runtime=%s install_id=%s error=%v", rollback.Runtime, rollback.InstallID, err)
			}
			continue
		}
		if err := s.rollbackPackInstallLocked(rollbackPath, rollback); err != nil {
			return fmt.Errorf("recover interrupted semantic runtime install %q: %w", rollback.ReplacementID, err)
		}
	}
	if err := s.sweepOrphanedRuntimeInstallDirsLocked(); err != nil {
		return err
	}
	if err := s.garbageCollectUnreachableRuntimePacksLocked(); err != nil {
		log.Printf("timich-agent semantic runtime pack startup GC deferred error=%v", err)
	}
	return nil
}

func (s *SemanticRuntimePackStore) sweepOrphanedRuntimeInstallDirsLocked() error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("list semantic runtime packs for orphan sweep: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		packRoot := filepath.Join(s.root, entry.Name())
		keepDir := ""
		if record, ok := readSemanticRuntimePackInstallRecord(filepath.Join(packRoot, semanticRuntimeInstallFile)); ok {
			candidate := filepath.Dir(filepath.Dir(record.ArtifactPath))
			if validateSemanticRuntimeInstallRecord(packRoot, record, candidate) != nil {
				return fmt.Errorf("%w: semantic runtime install record for %q is invalid", ErrSemanticRuntimePackInvalid, entry.Name())
			}
			keepDir = candidate
		}
		if err := removeSemanticRuntimeInstallDirs(packRoot, keepDir); err != nil {
			return fmt.Errorf("sweep semantic runtime install directories for %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func (s *SemanticRuntimePackStore) InstalledPacks() []SemanticRuntimePackStatus {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil
	}
	packs := []SemanticRuntimePackStatus{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), semanticRuntimeActiveFilePrefix) || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record, ok := readSemanticRuntimePackInstallRecord(filepath.Join(s.root, entry.Name()))
		if !ok {
			continue
		}
		pack, ok := installedSemanticRuntimePackFromRecord(record)
		if ok {
			packs = append(packs, pack)
		}
	}
	sort.Slice(packs, func(left, right int) bool {
		if !packs[left].InstalledAt.Equal(packs[right].InstalledAt) {
			return packs[left].InstalledAt.After(packs[right].InstalledAt)
		}
		return packs[left].ID < packs[right].ID
	})
	return packs
}

func (s *SemanticRuntimePackStore) InstalledPack(runtimeName string) (SemanticRuntimePackStatus, bool) {
	if s == nil {
		return SemanticRuntimePackStatus{}, false
	}
	runtimeName = strings.TrimSpace(runtimeName)
	if runtimeName == "" {
		packs := s.InstalledPacks()
		if len(packs) > 0 {
			return packs[0], true
		}
		return SemanticRuntimePackStatus{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := readSemanticRuntimePackInstallRecord(s.activeRecordPath(runtimeName))
	if !ok || strings.TrimSpace(record.RuntimePack.Runtime) != runtimeName {
		return SemanticRuntimePackStatus{}, false
	}
	return installedSemanticRuntimePackFromRecord(record)
}

func (s *SemanticRuntimePackStore) InstalledPackStatus(pack SemanticRuntimePackStatus) SemanticRuntimePackStatus {
	if s == nil || strings.TrimSpace(pack.ID) == "" {
		return pack
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := readSemanticRuntimePackInstallRecord(s.activeRecordPath(pack.Runtime))
	if !ok || record.RuntimePack.ID != strings.TrimSpace(pack.ID) || record.RuntimePack.Version != strings.TrimSpace(pack.Version) {
		return pack
	}
	installed, ok := installedSemanticRuntimePackFromRecord(record)
	if !ok {
		return pack
	}
	installed.Source = pack.Source
	installed.Artifact = pack.Artifact
	return installed
}

func (s *SemanticRuntimePackStore) InstallPack(ctx context.Context, pack SemanticRuntimePackStatus, reader io.Reader) (SemanticRuntimePackInstallResult, error) {
	return s.installPack(ctx, pack, reader, nil)
}

// InstallPackWithCommitHook stages and verifies bytes first, then invokes
// beforeCommit immediately before the durable runtime transition begins.
func (s *SemanticRuntimePackStore) InstallPackWithCommitHook(ctx context.Context, pack SemanticRuntimePackStatus, reader io.Reader, beforeCommit func()) (SemanticRuntimePackInstallResult, error) {
	return s.installPack(ctx, pack, reader, beforeCommit)
}

func (s *SemanticRuntimePackStore) installPack(ctx context.Context, pack SemanticRuntimePackStatus, reader io.Reader, beforeCommit func()) (_ SemanticRuntimePackInstallResult, resultErr error) {
	if s == nil {
		return SemanticRuntimePackInstallResult{}, ErrSemanticRuntimePackInvalid
	}
	if reader == nil {
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("%w: artifact body is required", ErrSemanticRuntimePackInvalid)
	}
	if err := validateSemanticRuntimePackForInstall(pack); err != nil {
		return SemanticRuntimePackInstallResult{}, err
	}

	packRoot, err := s.packRoot(pack.ID)
	if err != nil {
		return SemanticRuntimePackInstallResult{}, err
	}
	if err := os.MkdirAll(packRoot, 0o700); err != nil {
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("create semantic runtime pack install directory: %w", err)
	}
	installDir, err := os.MkdirTemp(packRoot, ".install-*")
	if err != nil {
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("create semantic runtime pack staging directory: %w", err)
	}
	cleanupInstallDir := true
	defer func() {
		if cleanupInstallDir {
			resultErr = errors.Join(resultErr, removeSemanticRuntimeInstallPath(installDir))
		}
	}()
	artifactDir := filepath.Join(installDir, "artifact")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("create semantic runtime artifact directory: %w", err)
	}
	expectedSize, err := semanticPackExpectedArtifactSize(pack.Artifact.SizeBytes)
	if err != nil {
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("%w: %v", ErrSemanticRuntimePackInvalid, err)
	}
	if err := ensureSemanticPackStorage(artifactDir, expectedSize); err != nil {
		return SemanticRuntimePackInstallResult{}, err
	}
	artifactPath := filepath.Join(artifactDir, pack.Artifact.Filename)
	tempFile, err := os.CreateTemp(artifactDir, pack.Artifact.Filename+".tmp-*")
	if err != nil {
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("create semantic runtime artifact temp file: %w", err)
	}
	tempPath := tempFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()
	if err := tempFile.Chmod(0o600); err != nil {
		_ = tempFile.Close()
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("chmod semantic runtime artifact temp file: %w", err)
	}
	hasher := sha256.New()
	written, err := copySemanticPackArtifact(ctx, io.MultiWriter(tempFile, hasher), reader, expectedSize)
	if err != nil {
		_ = tempFile.Close()
		if errors.Is(err, errSemanticPackArtifactTooLarge) {
			return SemanticRuntimePackInstallResult{}, fmt.Errorf("%w: exceeded %d bytes", ErrSemanticRuntimePackSizeMismatch, expectedSize)
		}
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("write semantic runtime artifact: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("sync semantic runtime artifact: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("close semantic runtime artifact: %w", err)
	}
	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	if actualSHA != strings.ToLower(pack.Artifact.SHA256) {
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("%w: got %s", ErrSemanticRuntimePackChecksumMismatch, actualSHA)
	}
	if written != expectedSize {
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("%w: got %d want %d", ErrSemanticRuntimePackSizeMismatch, written, expectedSize)
	}
	if err := os.Rename(tempPath, artifactPath); err != nil {
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("install semantic runtime artifact: %w", err)
	}
	cleanup = false

	runtimePath, serverPath, pythonPath, err := prepareSemanticRuntimePackLayout(installDir, artifactPath)
	if err != nil {
		return SemanticRuntimePackInstallResult{}, err
	}
	installedAt := time.Now().UTC()
	installID := fmt.Sprintf("%d-%s", installedAt.UnixNano(), actualSHA[:16])
	installedPack := pack
	installedPack.Status = "installed"
	installedPack.Installed = true
	installedPack.InstalledAt = installedAt
	installedPack.RuntimePath = runtimePath
	installedPack.ServerPath = serverPath
	installedPack.PythonPath = pythonPath
	installedPack.Artifact.SizeBytes = written
	installedPack.SizeBytes = written
	finalDir := filepath.Join(packRoot, fmt.Sprintf("%s-%d", actualSHA[:16], installedAt.UnixNano()))
	artifactPath, err = remapSemanticRuntimeInstallPath(installDir, finalDir, artifactPath)
	if err != nil {
		return SemanticRuntimePackInstallResult{}, err
	}
	runtimePath, err = remapSemanticRuntimeInstallPath(installDir, finalDir, runtimePath)
	if err != nil {
		return SemanticRuntimePackInstallResult{}, err
	}
	serverPath, err = remapSemanticRuntimeInstallPath(installDir, finalDir, serverPath)
	if err != nil {
		return SemanticRuntimePackInstallResult{}, err
	}
	if strings.TrimSpace(pythonPath) != "" {
		pythonPath, err = remapSemanticRuntimeInstallPath(installDir, finalDir, pythonPath)
		if err != nil {
			return SemanticRuntimePackInstallResult{}, err
		}
	}
	installedPack.RuntimePath = runtimePath
	installedPack.ServerPath = serverPath
	installedPack.PythonPath = pythonPath
	record := semanticRuntimePackInstallRecord{
		InstallID:    installID,
		RuntimePack:  installedPack,
		ArtifactPath: artifactPath,
		RuntimePath:  runtimePath,
		ServerPath:   serverPath,
		PythonPath:   pythonPath,
		InstalledAt:  installedAt,
		BytesWritten: written,
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("marshal semantic runtime install record: %w", err)
	}
	raw = append(raw, '\n')

	// Artifact download, fsync, and extraction deliberately happen before this
	// short pointer-transition lock so normal search and status reads remain live.
	if beforeCommit != nil {
		beforeCommit()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rollbackPath := s.rollbackRecordPath(installedPack.Runtime)
	if _, err := os.Stat(rollbackPath); err == nil {
		rollback, ok := readSemanticRuntimePackRollbackRecord(rollbackPath)
		if !ok || !s.semanticRuntimeInstallCommittedLocked(rollback) {
			return SemanticRuntimePackInstallResult{}, fmt.Errorf("%w: another %s runtime install is awaiting health finalization", ErrSemanticRuntimePackInvalid, installedPack.Runtime)
		}
		if err := removeSemanticModelRoleRecord(rollbackPath); err != nil {
			log.Printf("timich-agent stale committed semantic runtime marker will be replaced runtime=%s install_id=%s error=%v", rollback.Runtime, rollback.InstallID, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("inspect semantic runtime rollback record: %w", err)
	}
	var previousIDInstall *semanticRuntimePackInstallRecord
	if current, ok := readSemanticRuntimePackInstallRecord(filepath.Join(packRoot, semanticRuntimeInstallFile)); ok {
		if !semanticRuntimePackIdentityCompatible(current.RuntimePack, pack) {
			return SemanticRuntimePackInstallResult{}, fmt.Errorf(
				"%w: runtime pack id %q is already installed with a different runtime or platform",
				ErrSemanticRuntimePackInvalid,
				pack.ID,
			)
		}
		value := current
		previousIDInstall = &value
	}
	var previousActive *semanticRuntimePackInstallRecord
	if current, ok := readSemanticRuntimePackInstallRecord(s.activeRecordPath(installedPack.Runtime)); ok {
		value := current
		previousActive = &value
	}
	if err := os.Rename(installDir, finalDir); err != nil {
		return SemanticRuntimePackInstallResult{}, fmt.Errorf("activate semantic runtime pack directory: %w", err)
	}
	cleanupInstallDir = false
	rollback := semanticRuntimePackRollbackRecord{
		InstallID:          installID,
		Runtime:            installedPack.Runtime,
		ReplacementID:      installedPack.ID,
		ReplacementVersion: installedPack.Version,
		PreviousActive:     previousActive,
		PreviousIDInstall:  previousIDInstall,
		ReplacementDirName: filepath.Base(finalDir),
		CreatedAt:          installedAt,
	}
	rollbackRaw, err := json.MarshalIndent(rollback, "", "  ")
	if err != nil {
		return SemanticRuntimePackInstallResult{}, errors.Join(fmt.Errorf("marshal semantic runtime rollback record: %w", err), removeSemanticRuntimeInstallPath(finalDir))
	}
	rollbackRaw = append(rollbackRaw, '\n')
	if err := atomicfile.WriteFile(rollbackPath, rollbackRaw, 0o600); err != nil {
		return SemanticRuntimePackInstallResult{}, errors.Join(fmt.Errorf("write semantic runtime rollback record: %w", err), removeSemanticRuntimeInstallPath(finalDir))
	}
	if err := atomicfile.WriteFile(filepath.Join(packRoot, semanticRuntimeInstallFile), raw, 0o600); err != nil {
		rollbackErr := s.rollbackPackInstallLocked(rollbackPath, rollback)
		return SemanticRuntimePackInstallResult{}, errors.Join(fmt.Errorf("write semantic runtime install record: %w", err), rollbackErr)
	}
	return SemanticRuntimePackInstallResult{
		Status:       "installed",
		RuntimePack:  installedPack,
		InstalledAt:  installedAt,
		BytesWritten: written,
		InstallID:    installID,
	}, nil
}

// FinalizePackInstall retires the rollback copy after the replacement runtime
// has completed its health handshake.
func (s *SemanticRuntimePackStore) FinalizePackInstall(id string, version string, installID string) error {
	if s == nil {
		return ErrSemanticRuntimePackInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	packRoot, err := s.packRoot(id)
	if err != nil {
		return err
	}
	record, ok := readSemanticRuntimePackInstallRecord(filepath.Join(packRoot, semanticRuntimeInstallFile))
	if !ok || record.RuntimePack.Version != strings.TrimSpace(version) || record.InstallID != strings.TrimSpace(installID) {
		return ErrSemanticRuntimePackInvalid
	}
	rollbackPath := s.rollbackRecordPath(record.RuntimePack.Runtime)
	rollback, ok := readSemanticRuntimePackRollbackRecord(rollbackPath)
	if !ok || rollback.InstallID != record.InstallID {
		return fmt.Errorf("%w: semantic runtime install changed before finalize", ErrSemanticRuntimePackInvalid)
	}
	activeDir, err := runtimeReplacementDir(packRoot, rollback.ReplacementDirName)
	if err != nil || validateSemanticRuntimeInstallRecord(packRoot, record, activeDir) != nil {
		return fmt.Errorf("%w: semantic runtime replacement escaped its pack root", ErrSemanticRuntimePackInvalid)
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal semantic runtime active record: %w", err)
	}
	raw = append(raw, '\n')
	if err := atomicfile.WriteFile(s.activeRecordPath(record.RuntimePack.Runtime), raw, 0o600); err != nil {
		return fmt.Errorf("activate semantic runtime pack: %w", err)
	}
	if err := removeSemanticModelRoleRecord(rollbackPath); err != nil {
		log.Printf("timich-agent committed semantic runtime marker cleanup deferred runtime=%s install_id=%s error=%v", record.RuntimePack.Runtime, record.InstallID, err)
	}
	if err := s.garbageCollectUnreachableRuntimePacksLocked(); err != nil {
		log.Printf("timich-agent semantic runtime pack cleanup deferred runtime=%s install_id=%s error=%v", record.RuntimePack.Runtime, record.InstallID, err)
	}
	return nil
}

func (s *SemanticRuntimePackStore) semanticRuntimeInstallCommittedLocked(rollback semanticRuntimePackRollbackRecord) bool {
	active, ok := readSemanticRuntimePackInstallRecord(s.activeRecordPath(rollback.Runtime))
	if !ok || active.InstallID != rollback.InstallID ||
		active.RuntimePack.ID != rollback.ReplacementID ||
		active.RuntimePack.Version != rollback.ReplacementVersion {
		return false
	}
	packRoot, err := s.packRoot(rollback.ReplacementID)
	if err != nil {
		return false
	}
	activeDir, err := runtimeReplacementDir(packRoot, rollback.ReplacementDirName)
	return err == nil && validateSemanticRuntimeInstallRecord(packRoot, active, activeDir) == nil
}

// RollbackPackInstall restores the previous immutable install pointer and only
// then removes the failed replacement directory.
func (s *SemanticRuntimePackStore) RollbackPackInstall(id string, version string, installID string) error {
	if s == nil {
		return ErrSemanticRuntimePackInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	packRoot, err := s.packRoot(id)
	if err != nil {
		return err
	}
	record, ok := readSemanticRuntimePackInstallRecord(filepath.Join(packRoot, semanticRuntimeInstallFile))
	if !ok {
		return ErrSemanticRuntimePackInvalid
	}
	rollbackPath := s.rollbackRecordPath(record.RuntimePack.Runtime)
	rollback, ok := readSemanticRuntimePackRollbackRecord(rollbackPath)
	if !ok || rollback.InstallID != strings.TrimSpace(installID) || rollback.ReplacementID != strings.TrimSpace(id) || rollback.ReplacementVersion != strings.TrimSpace(version) {
		return fmt.Errorf("%w: semantic runtime install changed before rollback", ErrSemanticRuntimePackInvalid)
	}
	return s.rollbackPackInstallLocked(rollbackPath, rollback)
}

func (s *SemanticRuntimePackStore) rollbackPackInstallLocked(rollbackPath string, rollback semanticRuntimePackRollbackRecord) error {
	packRoot, err := s.packRoot(rollback.ReplacementID)
	if err != nil {
		return err
	}
	replacementDir, err := runtimeReplacementDir(packRoot, rollback.ReplacementDirName)
	if err != nil {
		return fmt.Errorf("%w: semantic runtime replacement escaped its pack root", ErrSemanticRuntimePackInvalid)
	}
	current, currentOK := readSemanticRuntimePackInstallRecord(filepath.Join(packRoot, semanticRuntimeInstallFile))
	replacementCurrent := currentOK && current.InstallID == rollback.InstallID &&
		validateSemanticRuntimeInstallRecord(packRoot, current, replacementDir) == nil
	previousCurrent := currentOK && rollback.PreviousIDInstall != nil && current.ArtifactPath == rollback.PreviousIDInstall.ArtifactPath
	if !replacementCurrent && !previousCurrent && (currentOK || rollback.PreviousIDInstall != nil) {
		return fmt.Errorf("%w: semantic runtime install pointer changed before rollback", ErrSemanticRuntimePackInvalid)
	}
	activePath := s.activeRecordPath(rollback.Runtime)
	active, activeOK := readSemanticRuntimePackInstallRecord(activePath)
	activeIsReplacement := activeOK && active.InstallID == rollback.InstallID
	activeIsPrevious := activeOK && rollback.PreviousActive != nil && active.InstallID == rollback.PreviousActive.InstallID
	if activeOK && !activeIsReplacement && !activeIsPrevious {
		return fmt.Errorf("%w: semantic runtime active pointer changed before rollback", ErrSemanticRuntimePackInvalid)
	}
	if rollback.PreviousActive != nil {
		previousRoot, rootErr := s.packRoot(rollback.PreviousActive.RuntimePack.ID)
		previousDir := filepath.Dir(filepath.Dir(rollback.PreviousActive.ArtifactPath))
		if rootErr != nil || validateSemanticRuntimeInstallRecord(previousRoot, *rollback.PreviousActive, previousDir) != nil {
			return fmt.Errorf("%w: previous semantic runtime active record escaped its pack root", ErrSemanticRuntimePackInvalid)
		}
		previousRaw, marshalErr := json.MarshalIndent(rollback.PreviousActive, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		previousRaw = append(previousRaw, '\n')
		if err := atomicfile.WriteFile(activePath, previousRaw, 0o600); err != nil {
			return fmt.Errorf("restore semantic runtime active record: %w", err)
		}
	} else if activeIsReplacement {
		if err := removeSemanticModelRoleRecord(activePath); err != nil {
			return fmt.Errorf("clear semantic runtime active record: %w", err)
		}
	}
	if rollback.PreviousIDInstall != nil {
		previousDir := filepath.Dir(filepath.Dir(rollback.PreviousIDInstall.ArtifactPath))
		if validateSemanticRuntimeInstallRecord(packRoot, *rollback.PreviousIDInstall, previousDir) != nil {
			return fmt.Errorf("%w: previous semantic runtime install escaped its pack root", ErrSemanticRuntimePackInvalid)
		}
		previousRaw, marshalErr := json.MarshalIndent(rollback.PreviousIDInstall, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		previousRaw = append(previousRaw, '\n')
		if err := atomicfile.WriteFile(filepath.Join(packRoot, semanticRuntimeInstallFile), previousRaw, 0o600); err != nil {
			return fmt.Errorf("restore semantic runtime install record: %w", err)
		}
	} else if err := removeSemanticModelRoleRecord(filepath.Join(packRoot, semanticRuntimeInstallFile)); err != nil {
		return fmt.Errorf("clear semantic runtime install record: %w", err)
	}
	if err := removeSemanticModelRoleRecord(rollbackPath); err != nil {
		return fmt.Errorf("clear semantic runtime rollback record: %w", err)
	}
	if err := os.RemoveAll(replacementDir); err != nil {
		return fmt.Errorf("remove failed semantic runtime replacement: %w", err)
	}
	keepDir := ""
	if rollback.PreviousIDInstall != nil {
		keepDir = filepath.Dir(filepath.Dir(rollback.PreviousIDInstall.ArtifactPath))
	}
	if err := removeSemanticRuntimeInstallDirs(packRoot, keepDir); err != nil {
		return fmt.Errorf("remove superseded semantic runtime installs after rollback: %w", err)
	}
	return nil
}

func (s *SemanticRuntimePackStore) installedPack(id string, version string) (SemanticRuntimePackStatus, bool) {
	installRoot, err := s.packRoot(id)
	if err != nil {
		return SemanticRuntimePackStatus{}, false
	}
	record, ok := readSemanticRuntimePackInstallRecord(filepath.Join(installRoot, semanticRuntimeInstallFile))
	if !ok {
		return SemanticRuntimePackStatus{}, false
	}
	pack, ok := installedSemanticRuntimePackFromRecord(record)
	if !ok {
		return SemanticRuntimePackStatus{}, false
	}
	if version = strings.TrimSpace(version); version != "" && pack.Version != version {
		return SemanticRuntimePackStatus{}, false
	}
	return pack, true
}

func installedSemanticRuntimePackFromRecord(record semanticRuntimePackInstallRecord) (SemanticRuntimePackStatus, bool) {
	pack := record.RuntimePack
	pack.Status = "installed"
	pack.Installed = true
	pack.InstalledAt = record.InstalledAt
	pack.RuntimePath = record.RuntimePath
	pack.ServerPath = record.ServerPath
	pack.PythonPath = record.PythonPath
	return pack, true
}

func readSemanticRuntimePackInstallRecord(path string) (semanticRuntimePackInstallRecord, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return semanticRuntimePackInstallRecord{}, false
	}
	var record semanticRuntimePackInstallRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return semanticRuntimePackInstallRecord{}, false
	}
	if strings.TrimSpace(record.RuntimePack.ID) == "" ||
		strings.TrimSpace(record.RuntimePack.Version) == "" ||
		strings.TrimSpace(record.ArtifactPath) == "" ||
		strings.TrimSpace(record.RuntimePath) == "" ||
		strings.TrimSpace(record.ServerPath) == "" ||
		strings.TrimSpace(record.PythonPath) == "" {
		return semanticRuntimePackInstallRecord{}, false
	}
	return record, true
}

func readSemanticRuntimePackRollbackRecord(path string) (semanticRuntimePackRollbackRecord, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return semanticRuntimePackRollbackRecord{}, false
	}
	var record semanticRuntimePackRollbackRecord
	if err := json.Unmarshal(raw, &record); err != nil ||
		strings.TrimSpace(record.InstallID) == "" ||
		strings.TrimSpace(record.Runtime) == "" ||
		strings.TrimSpace(record.ReplacementID) == "" ||
		strings.TrimSpace(record.ReplacementVersion) == "" ||
		strings.TrimSpace(record.ReplacementDirName) == "" {
		return semanticRuntimePackRollbackRecord{}, false
	}
	return record, true
}

func validateSemanticRuntimePackForInstall(pack SemanticRuntimePackStatus) error {
	if safeSemanticModelPathPart(pack.ID) == "" {
		return fmt.Errorf("%w: id is required", ErrSemanticRuntimePackInvalid)
	}
	if strings.TrimSpace(pack.Version) == "" || safeSemanticModelPathPart(pack.Version) == "" {
		return fmt.Errorf("%w: version is invalid", ErrSemanticRuntimePackInvalid)
	}
	if strings.TrimSpace(pack.Runtime) != semanticRuntimeLoaderONNXRuntime {
		return fmt.Errorf("%w: runtime must be %q", ErrSemanticRuntimePackInvalid, semanticRuntimeLoaderONNXRuntime)
	}
	if pack.Artifact == nil {
		return fmt.Errorf("%w: artifact is required", ErrSemanticRuntimePackInvalid)
	}
	if safeSemanticArtifactFilename(pack.Artifact.Filename) == "" {
		return fmt.Errorf("%w: artifact filename is invalid", ErrSemanticRuntimePackInvalid)
	}
	if !validSHA256Hex(pack.Artifact.SHA256) {
		return fmt.Errorf("%w: artifact SHA-256 is required", ErrSemanticRuntimePackInvalid)
	}
	return nil
}

func (s *SemanticRuntimePackStore) packRoot(id string) (string, error) {
	idPart := safeSemanticModelPathPart(id)
	if idPart == "" {
		return "", fmt.Errorf("%w: id is invalid", ErrSemanticRuntimePackInvalid)
	}
	return filepath.Join(s.root, idPart), nil
}

func (s *SemanticRuntimePackStore) activeRecordPath(runtimeName string) string {
	return filepath.Join(s.root, semanticRuntimeActiveFilePrefix+safeSemanticModelPathPart(runtimeName)+".json")
}

func (s *SemanticRuntimePackStore) rollbackRecordPath(runtimeName string) string {
	return filepath.Join(s.root, semanticRuntimeRollbackFilePrefix+safeSemanticModelPathPart(runtimeName)+".json")
}

func runtimeReplacementDir(packRoot string, dirName string) (string, error) {
	dirName = strings.TrimSpace(dirName)
	if dirName == "" || filepath.IsAbs(dirName) || filepath.Base(dirName) != dirName || dirName == "." || dirName == ".." {
		return "", ErrSemanticRuntimePackInvalid
	}
	candidate := filepath.Join(packRoot, dirName)
	relative, err := filepath.Rel(packRoot, candidate)
	if err != nil || relative != dirName || strings.Contains(relative, string(filepath.Separator)) {
		return "", ErrSemanticRuntimePackInvalid
	}
	return candidate, nil
}

func validateSemanticRuntimeInstallRecord(packRoot string, record semanticRuntimePackInstallRecord, installDir string) error {
	expectedRoot, err := runtimeReplacementDir(packRoot, filepath.Base(installDir))
	if err != nil || filepath.Clean(expectedRoot) != filepath.Clean(installDir) {
		return ErrSemanticRuntimePackInvalid
	}
	for _, path := range []string{record.ArtifactPath, record.RuntimePath, record.ServerPath, record.PythonPath} {
		if strings.TrimSpace(path) == "" {
			continue
		}
		relative, relErr := filepath.Rel(installDir, path)
		if relErr != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return ErrSemanticRuntimePackInvalid
		}
	}
	return nil
}

func semanticRuntimePackIdentityCompatible(current SemanticRuntimePackStatus, next SemanticRuntimePackStatus) bool {
	return strings.TrimSpace(current.ID) == strings.TrimSpace(next.ID) &&
		strings.TrimSpace(current.Runtime) == strings.TrimSpace(next.Runtime) &&
		strings.TrimSpace(current.Platform) == strings.TrimSpace(next.Platform)
}

func remapSemanticRuntimeInstallPath(stagingDir string, finalDir string, path string) (string, error) {
	relative, err := filepath.Rel(stagingDir, path)
	if err != nil {
		return "", fmt.Errorf("resolve semantic runtime install path: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("semantic runtime install path escaped the staged install")
	}
	return filepath.Join(finalDir, relative), nil
}

func removeSemanticRuntimeInstallDirs(packRoot string, activeDir string) error {
	entries, err := os.ReadDir(packRoot)
	if err != nil {
		return err
	}
	activeDir = filepath.Clean(activeDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(packRoot, entry.Name())
		if filepath.Clean(path) == activeDir {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func removeSemanticRuntimeInstallPath(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove semantic runtime install directory %q: %w", filepath.Base(path), err)
	}
	return nil
}

func (s *SemanticRuntimePackStore) garbageCollectUnreachableRuntimePacksLocked() error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("list semantic runtime packs for role GC: %w", err)
	}
	var cleanupErr error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		packRoot := filepath.Join(s.root, entry.Name())
		record, ok := readSemanticRuntimePackInstallRecord(filepath.Join(packRoot, semanticRuntimeInstallFile))
		if !ok {
			continue
		}
		active, activeOK := readSemanticRuntimePackInstallRecord(s.activeRecordPath(record.RuntimePack.Runtime))
		if activeOK && active.RuntimePack.ID == record.RuntimePack.ID && active.InstallID == record.InstallID {
			activeDir := filepath.Dir(filepath.Dir(active.ArtifactPath))
			if err := removeSemanticRuntimeInstallDirs(packRoot, activeDir); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("clean active semantic runtime pack %q: %w", entry.Name(), err))
			}
			continue
		}
		if err := removeSemanticModelRoleRecord(filepath.Join(packRoot, semanticRuntimeInstallFile)); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove unreachable semantic runtime pointer %q: %w", entry.Name(), err))
			continue
		}
		if err := removeSemanticRuntimeInstallDirs(packRoot, ""); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove unreachable semantic runtime pack %q: %w", entry.Name(), err))
		}
	}
	return cleanupErr
}
