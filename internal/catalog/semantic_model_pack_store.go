package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
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
	semanticModelPacksDirName  = "semantic-model-packs"
	semanticModelInstallFile   = "install.json"
	semanticModelIdentityFile  = "identity.json"
	semanticModelActiveFile    = "active.json"
	semanticModelCandidateFile = "candidate.json"
	semanticModelRollbackFile  = "install.rollback.json"
)

var (
	ErrSemanticModelPackInvalid          = errors.New("semantic model pack invalid")
	ErrSemanticModelPackActive           = errors.New("semantic model pack active")
	ErrSemanticModelPackChecksumMismatch = errors.New("semantic model pack checksum mismatch")
	ErrSemanticModelPackSizeMismatch     = errors.New("semantic model pack size mismatch")
)

type SemanticModelPackInstallResult struct {
	Status       string                  `json:"status"`
	ModelPack    SemanticModelPackStatus `json:"modelPack"`
	InstalledAt  time.Time               `json:"installedAt"`
	BytesWritten int64                   `json:"bytesWritten"`
	InstallID    string                  `json:"-"`
}

type SemanticModelActivationResult struct {
	Status        string                     `json:"status"`
	ModelID       string                     `json:"modelId"`
	VectorSpaceID string                     `json:"vectorSpaceId"`
	ActivatedAt   time.Time                  `json:"activatedAt"`
	Profile       SemanticModelProfileStatus `json:"profile"`
}

type SemanticModelUninstallResult struct {
	Status        string    `json:"status"`
	ModelID       string    `json:"modelId"`
	VectorSpaceID string    `json:"vectorSpaceId"`
	UninstalledAt time.Time `json:"uninstalledAt"`
}

type SemanticModelRuntimeLayout struct {
	ModelID       string `json:"modelId"`
	VectorSpaceID string `json:"vectorSpaceId"`
	Runtime       string `json:"runtime"`
	RuntimePath   string `json:"runtimePath"`
	EmbeddingDim  int    `json:"embeddingDim"`
	InputKind     string `json:"inputKind"`
}

type SemanticModelPackStore struct {
	root              string
	runtimeHelperPath string
	mu                sync.Mutex
}

type SemanticModelPackStoreOptions struct {
	RuntimeHelperPath string
}

// SemanticModelCorpusIdentity identifies durable semantic search data owned by
// one installed model role. Only active and candidate identities are reachable.
type SemanticModelCorpusIdentity struct {
	ModelID       string
	VectorSpaceID string
}

type semanticModelPackInstallRecord struct {
	InstallID    string                  `json:"installId,omitempty"`
	ModelPack    SemanticModelPackStatus `json:"modelPack"`
	ArtifactPath string                  `json:"artifactPath"`
	RuntimePath  string                  `json:"runtimePath,omitempty"`
	InstalledAt  time.Time               `json:"installedAt"`
	BytesWritten int64                   `json:"bytesWritten"`
}

type semanticModelPackRollbackRecord struct {
	InstallID                string                          `json:"installId"`
	ReplacementModelID       string                          `json:"replacementModelId"`
	ReplacementVectorSpaceID string                          `json:"replacementVectorSpaceId"`
	Previous                 *semanticModelPackInstallRecord `json:"previous,omitempty"`
	Replacement              *semanticModelPackInstallRecord `json:"replacement"`
	PreviousCandidate        *semanticModelCandidateRecord   `json:"previousCandidate,omitempty"`
	ReplacementDirName       string                          `json:"replacementDirName"`
	ReplacementCandidate     bool                            `json:"replacementCandidate"`
	CreatedAt                time.Time                       `json:"createdAt"`
}

type semanticModelIdentityRecord struct {
	ModelID       string `json:"modelId"`
	VectorSpaceID string `json:"vectorSpaceId"`
	EmbeddingDim  int    `json:"embeddingDim"`
	InputKind     string `json:"inputKind"`
	Runtime       string `json:"runtime"`
}

type semanticModelActiveRecord struct {
	ModelID       string    `json:"modelId"`
	VectorSpaceID string    `json:"vectorSpaceId"`
	ActivatedAt   time.Time `json:"activatedAt"`
}

type semanticModelCandidateRecord struct {
	ModelID       string    `json:"modelId"`
	VectorSpaceID string    `json:"vectorSpaceId"`
	SelectedAt    time.Time `json:"selectedAt"`
}

func LoadOrCreateSemanticModelPackStore(dataDir string) (*SemanticModelPackStore, error) {
	return LoadOrCreateSemanticModelPackStoreWithOptions(dataDir, SemanticModelPackStoreOptions{})
}

func LoadOrCreateSemanticModelPackStoreWithOptions(dataDir string, options SemanticModelPackStoreOptions) (*SemanticModelPackStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("data directory must not be empty")
	}
	root := filepath.Join(dataDir, semanticModelPacksDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create semantic model pack directory: %w", err)
	}
	store := &SemanticModelPackStore{
		root:              root,
		runtimeHelperPath: strings.TrimSpace(options.RuntimeHelperPath),
	}
	if err := store.recoverInterruptedInstalls(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *SemanticModelPackStore) recoverInterruptedInstalls() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("list semantic model packs for recovery: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		modelRoot := filepath.Join(s.root, entry.Name())
		rollback, ok := readSemanticModelPackRollbackRecord(filepath.Join(modelRoot, semanticModelRollbackFile))
		if !ok {
			if _, err := os.Stat(filepath.Join(modelRoot, semanticModelRollbackFile)); err == nil {
				return fmt.Errorf("%w: unreadable semantic model rollback record for %q", ErrSemanticModelPackInvalid, entry.Name())
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect semantic model rollback record for %q: %w", entry.Name(), err)
			}
			continue
		}
		if s.semanticModelInstallCommittedLocked(rollback) {
			if err := removeSemanticModelRoleRecord(filepath.Join(modelRoot, semanticModelRollbackFile)); err != nil {
				log.Printf("timich-agent committed semantic model marker cleanup deferred model=%s install_id=%s error=%v", rollback.ReplacementModelID, rollback.InstallID, err)
			}
			continue
		}
		if err := s.rollbackPackInstallLocked(modelRoot, rollback.ReplacementModelID, rollback.ReplacementVectorSpaceID, rollback.InstallID); err != nil {
			return fmt.Errorf("recover interrupted semantic model install %q: %w", entry.Name(), err)
		}
	}
	if err := s.sweepOrphanedModelInstallDirsLocked(); err != nil {
		return err
	}
	if err := s.garbageCollectUnreachableModelPacksLocked(); err != nil {
		log.Printf("timich-agent semantic model pack startup GC deferred error=%v", err)
	}
	return nil
}

func (s *SemanticModelPackStore) sweepOrphanedModelInstallDirsLocked() error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("list semantic model packs for orphan sweep: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		modelRoot := filepath.Join(s.root, entry.Name())
		keepDir := ""
		if record, ok := readSemanticModelInstallRecord(filepath.Join(modelRoot, semanticModelInstallFile)); ok {
			candidate := filepath.Dir(filepath.Dir(record.ArtifactPath))
			if validateSemanticModelInstallRecord(modelRoot, record, candidate) != nil {
				return fmt.Errorf("%w: semantic model install record for %q is invalid", ErrSemanticModelPackInvalid, entry.Name())
			}
			keepDir = candidate
		}
		if err := removeSemanticModelInstallDirs(modelRoot, keepDir); err != nil {
			return fmt.Errorf("sweep semantic model install directories for %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func (s *SemanticModelPackStore) InstalledProfile(profile SemanticModelProfileStatus) SemanticModelProfileStatus {
	return s.InstalledProfileWithContext(context.Background(), profile)
}

func (s *SemanticModelPackStore) InstalledProfileWithContext(ctx context.Context, profile SemanticModelProfileStatus) SemanticModelProfileStatus {
	if s == nil || profile.ModelID == "" {
		return profile
	}
	var installed SemanticModelPackStatus
	var artifactPath, runtimePath string
	var ok bool
	if s.activeIdentityMatches(profile.ModelID, profile.VectorSpaceID) {
		installed, artifactPath, runtimePath, ok = s.activeInstalledPack(profile.ModelID, profile.VectorSpaceID)
	} else {
		installed, artifactPath, runtimePath, ok = s.installedPack(profile.ModelID, profile.VectorSpaceID)
	}
	if !ok {
		return profile
	}
	role := ""
	if s.activeIdentityMatches(profile.ModelID, installed.VectorSpaceID) {
		role = semanticModelRoleActive
	} else if s.candidateIdentityMatches(profile.ModelID, installed.VectorSpaceID) {
		role = semanticModelRoleCandidate
	}
	if role == "" {
		return profile
	}
	installed.Role = role
	profile.ModelPack = &installed
	profile.VectorSpaceID = installed.VectorSpaceID
	profile.EmbeddingDim = installed.EmbeddingDim
	profile.InputKind = installed.InputKind
	profile.Role = role
	profile.ProfileKind = semanticProfileKindModelPack
	profile.Runtime = semanticInstalledModelRuntimeStatusWithContext(ctx, installed, artifactPath, runtimePath, s.runtimeHelperPath)
	return profile
}

func (s *SemanticModelPackStore) InstalledProfiles() []SemanticModelProfileStatus {
	return s.InstalledProfilesWithContext(context.Background())
}

// ReachableCorpusIdentities returns the model identities referenced by the
// durable active and candidate pointers. Derived vectors and indexes for any
// other identity can be garbage-collected.
func (s *SemanticModelPackStore) ReachableCorpusIdentities() []SemanticModelCorpusIdentity {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	active := s.activeRecordLocked()
	candidate := s.candidateRecordLocked()
	identities := make([]SemanticModelCorpusIdentity, 0, 2)
	seen := map[string]struct{}{}
	appendIdentity := func(modelID string, vectorSpaceID string) {
		modelID = strings.TrimSpace(modelID)
		vectorSpaceID = strings.TrimSpace(vectorSpaceID)
		key := modelID + "\x00" + vectorSpaceID
		if modelID == "" || vectorSpaceID == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		identities = append(identities, SemanticModelCorpusIdentity{
			ModelID:       modelID,
			VectorSpaceID: vectorSpaceID,
		})
	}
	appendIdentity(active.ModelID, active.VectorSpaceID)
	appendIdentity(candidate.ModelID, candidate.VectorSpaceID)
	return identities
}

func (s *SemanticModelPackStore) InstalledProfilesWithContext(ctx context.Context) []SemanticModelProfileStatus {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	active := s.activeRecordLocked()
	candidate := s.candidateRecordLocked()
	modelEntries, err := os.ReadDir(s.root)
	if err != nil {
		s.mu.Unlock()
		return nil
	}
	type installedProfileSnapshot struct {
		profile      SemanticModelProfileStatus
		pack         SemanticModelPackStatus
		artifactPath string
		runtimePath  string
	}
	snapshots := []installedProfileSnapshot{}
	for _, modelEntry := range modelEntries {
		if !modelEntry.IsDir() {
			continue
		}
		modelRoot := filepath.Join(s.root, modelEntry.Name())
		record, ok := readSemanticModelInstallRecord(filepath.Join(modelRoot, semanticModelInstallFile))
		if !ok {
			continue
		}
		role := ""
		if activeMatches(active, record.ModelPack.ID, record.ModelPack.VectorSpaceID) {
			role = semanticModelRoleActive
			if guarded, guardedOK := s.activeInstallRecordLocked(active); guardedOK {
				record = guarded
			}
		} else if candidateMatches(candidate, record.ModelPack.ID, record.ModelPack.VectorSpaceID) {
			role = semanticModelRoleCandidate
		}
		if role == "" {
			continue
		}
		pack, artifactPath, runtimePath, ok := installedSemanticModelPackFromRecord(record)
		if !ok {
			continue
		}
		pack.Role = role
		packCopy := pack
		snapshots = append(snapshots, installedProfileSnapshot{
			profile: SemanticModelProfileStatus{
				ModelID:       pack.ID,
				VectorSpaceID: pack.VectorSpaceID,
				EmbeddingDim:  pack.EmbeddingDim,
				Role:          role,
				ProfileKind:   semanticProfileKindModelPack,
				InputKind:     pack.InputKind,
				ModelPack:     &packCopy,
			},
			pack:         pack,
			artifactPath: artifactPath,
			runtimePath:  runtimePath,
		})
	}
	s.mu.Unlock()

	profiles := make([]SemanticModelProfileStatus, 0, len(snapshots))
	for _, snapshot := range snapshots {
		profile := snapshot.profile
		profile.Runtime = semanticInstalledModelRuntimeStatusWithContext(ctx, snapshot.pack, snapshot.artifactPath, snapshot.runtimePath, s.runtimeHelperPath)
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(left, right int) bool {
		if profiles[left].ModelID != profiles[right].ModelID {
			return profiles[left].ModelID < profiles[right].ModelID
		}
		return profiles[left].VectorSpaceID < profiles[right].VectorSpaceID
	})
	return profiles
}

func (s *SemanticModelPackStore) InstalledCandidateProfile() (SemanticModelProfileStatus, bool) {
	if s == nil {
		return SemanticModelProfileStatus{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	candidate := s.candidateRecordLocked()
	active := s.activeRecordLocked()
	if candidate.ModelID == "" || candidate.VectorSpaceID == "" || activeMatches(active, candidate.ModelID, candidate.VectorSpaceID) {
		return SemanticModelProfileStatus{}, false
	}
	record, ok := readSemanticModelInstallRecord(filepath.Join(s.root, candidate.ModelID, semanticModelInstallFile))
	if !ok {
		return SemanticModelProfileStatus{}, false
	}
	pack, _, _, ok := installedSemanticModelPackFromRecord(record)
	if !ok || !candidateMatches(candidate, pack.ID, pack.VectorSpaceID) {
		return SemanticModelProfileStatus{}, false
	}
	pack.Role = semanticModelRoleCandidate
	packCopy := pack
	return SemanticModelProfileStatus{
		ModelID:       pack.ID,
		VectorSpaceID: pack.VectorSpaceID,
		EmbeddingDim:  pack.EmbeddingDim,
		Role:          semanticModelRoleCandidate,
		ProfileKind:   semanticProfileKindModelPack,
		InputKind:     pack.InputKind,
		ModelPack:     &packCopy,
	}, true
}

func (s *SemanticModelPackStore) InstalledActiveProfile() (SemanticModelProfileStatus, bool) {
	if s == nil {
		return SemanticModelProfileStatus{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	active := s.activeRecordLocked()
	if active.ModelID == "" || active.VectorSpaceID == "" {
		return SemanticModelProfileStatus{}, false
	}
	record, ok := s.activeInstallRecordLocked(active)
	if !ok {
		return SemanticModelProfileStatus{}, false
	}
	pack, _, _, ok := installedSemanticModelPackFromRecord(record)
	if !ok || !activeMatches(active, pack.ID, pack.VectorSpaceID) {
		return SemanticModelProfileStatus{}, false
	}
	pack.Role = semanticModelRoleActive
	packCopy := pack
	return SemanticModelProfileStatus{
		ModelID:       pack.ID,
		VectorSpaceID: pack.VectorSpaceID,
		EmbeddingDim:  pack.EmbeddingDim,
		Role:          semanticModelRoleActive,
		ProfileKind:   semanticProfileKindModelPack,
		InputKind:     pack.InputKind,
		ModelPack:     &packCopy,
	}, true
}

func (s *SemanticModelPackStore) InstalledRuntimeLayouts() []SemanticModelRuntimeLayout {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	active := s.activeRecordLocked()
	candidate := s.candidateRecordLocked()
	identities := [][2]string{}
	if active.ModelID != "" && active.VectorSpaceID != "" {
		identities = append(identities, [2]string{active.ModelID, active.VectorSpaceID})
	}
	if candidate.ModelID != "" && candidate.VectorSpaceID != "" && !activeMatches(active, candidate.ModelID, candidate.VectorSpaceID) {
		identities = append(identities, [2]string{candidate.ModelID, candidate.VectorSpaceID})
	}
	layouts := []SemanticModelRuntimeLayout{}
	for _, identity := range identities {
		var record semanticModelPackInstallRecord
		var ok bool
		if activeMatches(active, identity[0], identity[1]) {
			record, ok = s.activeInstallRecordLocked(active)
		} else {
			record, ok = readSemanticModelInstallRecord(filepath.Join(s.root, identity[0], semanticModelInstallFile))
		}
		if !ok {
			continue
		}
		pack, _, runtimePath, ok := installedSemanticModelPackFromRecord(record)
		if !ok || pack.VectorSpaceID != identity[1] || strings.TrimSpace(runtimePath) == "" {
			continue
		}
		if semanticModelRuntimeLayoutStatus(pack, runtimePath) != semanticRuntimeLayoutReady {
			continue
		}
		layouts = append(layouts, SemanticModelRuntimeLayout{
			ModelID:       pack.ID,
			VectorSpaceID: pack.VectorSpaceID,
			Runtime:       pack.Runtime,
			RuntimePath:   runtimePath,
			EmbeddingDim:  pack.EmbeddingDim,
			InputKind:     pack.InputKind,
		})
	}
	sort.Slice(layouts, func(left, right int) bool {
		if layouts[left].ModelID != layouts[right].ModelID {
			return layouts[left].ModelID < layouts[right].ModelID
		}
		return layouts[left].VectorSpaceID < layouts[right].VectorSpaceID
	})
	return layouts
}

func (s *SemanticModelPackStore) ActiveProfile() (SemanticModelProfileStatus, bool) {
	return s.ActiveProfileWithContext(context.Background())
}

func (s *SemanticModelPackStore) ActiveProfileWithContext(ctx context.Context) (SemanticModelProfileStatus, bool) {
	if s == nil {
		return SemanticModelProfileStatus{}, false
	}
	active := s.activeRecord()
	if active.ModelID == "" || active.VectorSpaceID == "" {
		return SemanticModelProfileStatus{}, false
	}
	installed, artifactPath, runtimePath, ok := s.activeInstalledPack(active.ModelID, active.VectorSpaceID)
	if !ok {
		return SemanticModelProfileStatus{}, false
	}
	installed.Role = semanticModelRoleActive
	packCopy := installed
	return SemanticModelProfileStatus{
		ModelID:       installed.ID,
		VectorSpaceID: installed.VectorSpaceID,
		EmbeddingDim:  installed.EmbeddingDim,
		Role:          semanticModelRoleActive,
		ProfileKind:   semanticProfileKindModelPack,
		InputKind:     installed.InputKind,
		ModelPack:     &packCopy,
		Runtime:       semanticInstalledModelRuntimeStatusWithContext(ctx, installed, artifactPath, runtimePath, s.runtimeHelperPath),
	}, true
}

func (s *SemanticModelPackStore) ActivePackInstalled() bool {
	if s == nil {
		return false
	}
	active := s.activeRecord()
	if active.ModelID == "" || active.VectorSpaceID == "" {
		return false
	}
	_, _, _, ok := s.activeInstalledPack(active.ModelID, active.VectorSpaceID)
	return ok
}

func (s *SemanticModelPackStore) ActiveEmbeddingProfile() (semanticEmbeddingProfile, bool) {
	return s.ActiveEmbeddingProfileWithContext(context.Background())
}

func (s *SemanticModelPackStore) ActiveEmbeddingProfileWithContext(ctx context.Context) (semanticEmbeddingProfile, bool) {
	if s == nil {
		return nil, false
	}
	active := s.activeRecord()
	if active.ModelID == "" || active.VectorSpaceID == "" {
		return nil, false
	}
	installed, artifactPath, runtimePath, ok := s.activeInstalledPack(active.ModelID, active.VectorSpaceID)
	if !ok {
		return nil, false
	}
	runtime := semanticInstalledModelRuntimeStatusWithContext(ctx, installed, artifactPath, runtimePath, s.runtimeHelperPath)
	if runtime == nil || !runtime.Loaded || !runtime.CanEmbed {
		return nil, false
	}
	return semanticRuntimeHelperProfile{pack: installed, runtimePath: runtimePath, helperPath: s.runtimeHelperPath}, true
}

func (s *SemanticModelPackStore) CandidateEmbeddingProfile(modelID string, vectorSpaceID string) (semanticEmbeddingProfile, bool) {
	return s.CandidateEmbeddingProfileWithContext(context.Background(), modelID, vectorSpaceID)
}

func (s *SemanticModelPackStore) CandidateEmbeddingProfileWithContext(ctx context.Context, modelID string, vectorSpaceID string) (semanticEmbeddingProfile, bool) {
	if s == nil {
		return nil, false
	}
	if !s.candidateIdentityMatches(modelID, vectorSpaceID) {
		return nil, false
	}
	return s.installedEmbeddingProfileWithContext(ctx, modelID, vectorSpaceID)
}

func (s *SemanticModelPackStore) installedEmbeddingProfileWithContext(ctx context.Context, modelID string, vectorSpaceID string) (semanticEmbeddingProfile, bool) {
	installed, artifactPath, runtimePath, ok := s.installedPack(modelID, vectorSpaceID)
	if !ok {
		return nil, false
	}
	runtime := semanticInstalledModelRuntimeStatusWithContext(ctx, installed, artifactPath, runtimePath, s.runtimeHelperPath)
	if runtime == nil || !runtime.Loaded || !runtime.CanEmbed {
		return nil, false
	}
	return semanticRuntimeHelperProfile{
		pack:        installed,
		runtimePath: runtimePath,
		helperPath:  s.runtimeHelperPath,
	}, true
}

// InstallRuntimeLayout resolves the exact immutable replacement directory
// identified by installID. It is intentionally independent of active/candidate
// role pointers so health checks cannot accidentally approve older bytes.
func (s *SemanticModelPackStore) InstallRuntimeLayout(modelID string, vectorSpaceID string, installID string) (SemanticModelRuntimeLayout, bool) {
	if s == nil {
		return SemanticModelRuntimeLayout{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	modelRoot, err := s.modelRoot(modelID)
	if err != nil {
		return SemanticModelRuntimeLayout{}, false
	}
	rollback, ok := readSemanticModelPackRollbackRecord(filepath.Join(modelRoot, semanticModelRollbackFile))
	if !ok || rollback.InstallID != strings.TrimSpace(installID) || rollback.Replacement == nil {
		return SemanticModelRuntimeLayout{}, false
	}
	record := *rollback.Replacement
	if record.InstallID != rollback.InstallID || record.ModelPack.VectorSpaceID != strings.TrimSpace(vectorSpaceID) {
		return SemanticModelRuntimeLayout{}, false
	}
	installDir, err := modelReplacementDir(modelRoot, rollback.ReplacementDirName)
	if err != nil || validateSemanticModelInstallRecord(modelRoot, record, installDir) != nil ||
		semanticModelRuntimeLayoutStatus(record.ModelPack, record.RuntimePath) != semanticRuntimeLayoutReady {
		return SemanticModelRuntimeLayout{}, false
	}
	return SemanticModelRuntimeLayout{
		ModelID:       record.ModelPack.ID,
		VectorSpaceID: record.ModelPack.VectorSpaceID,
		Runtime:       record.ModelPack.Runtime,
		RuntimePath:   record.RuntimePath,
		EmbeddingDim:  record.ModelPack.EmbeddingDim,
		InputKind:     record.ModelPack.InputKind,
	}, true
}

// ProbeInstallRuntime executes both text and image helper contracts for a
// non-managed runtime replacement. ONNX model packs use ProbeModelLayout so
// their exact runtime-pack server is also part of the proof.
func (s *SemanticModelPackStore) ProbeInstallRuntime(ctx context.Context, modelID string, vectorSpaceID string, installID string) error {
	layout, ok := s.InstallRuntimeLayout(modelID, vectorSpaceID, installID)
	if !ok {
		return ErrSemanticModelPackInvalid
	}
	s.mu.Lock()
	modelRoot, err := s.modelRoot(modelID)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	rollback, ok := readSemanticModelPackRollbackRecord(filepath.Join(modelRoot, semanticModelRollbackFile))
	s.mu.Unlock()
	if !ok || rollback.InstallID != strings.TrimSpace(installID) || rollback.Replacement == nil {
		return ErrSemanticModelPackInvalid
	}
	record := *rollback.Replacement
	profile := semanticRuntimeHelperProfile{pack: record.ModelPack, runtimePath: layout.RuntimePath, helperPath: s.runtimeHelperPath}
	if _, err := profile.EmbedText(ctx, "timich semantic runtime health"); err != nil {
		return err
	}
	_, err = profile.EmbedSemanticAsset(ctx, semanticAssetEmbeddingInput{Image: &semanticImageEmbeddingInput{
		Bytes:       semanticModelProbePNG(),
		ContentType: "image/png",
		Source:      "install-probe.png",
	}})
	return err
}

func semanticModelProbePNG() []byte {
	imageValue := image.NewRGBA(image.Rect(0, 0, 1, 1))
	imageValue.Set(0, 0, color.RGBA{R: 127, G: 191, B: 255, A: 255})
	var output bytes.Buffer
	_ = png.Encode(&output, imageValue)
	return output.Bytes()
}

func (s *SemanticModelPackStore) ActivatePack(modelID string, vectorSpaceID string) (SemanticModelActivationResult, error) {
	if s == nil {
		return SemanticModelActivationResult{}, ErrSemanticModelPackInvalid
	}
	if !s.candidateIdentityMatches(modelID, vectorSpaceID) {
		return SemanticModelActivationResult{}, ErrSemanticModelPackInvalid
	}
	installed, _, _, ok := s.installedPack(modelID, vectorSpaceID)
	if !ok {
		return SemanticModelActivationResult{}, ErrSemanticModelPackInvalid
	}
	activatedAt := time.Now().UTC()
	record := semanticModelActiveRecord{
		ModelID:       installed.ID,
		VectorSpaceID: installed.VectorSpaceID,
		ActivatedAt:   activatedAt,
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return SemanticModelActivationResult{}, fmt.Errorf("marshal semantic active model record: %w", err)
	}
	raw = append(raw, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if !candidateMatches(s.candidateRecordLocked(), installed.ID, installed.VectorSpaceID) {
		return SemanticModelActivationResult{}, ErrSemanticModelPackInvalid
	}
	previousActive := s.activeRecordLocked()
	profile, ok := s.activeProfileLocked(record)
	if !ok {
		return SemanticModelActivationResult{}, ErrSemanticModelPackInvalid
	}
	if err := atomicfile.WriteFile(filepath.Join(s.root, semanticModelActiveFile), raw, 0o600); err != nil {
		return SemanticModelActivationResult{}, fmt.Errorf("write semantic active model record: %w", err)
	}
	if err := removeSemanticModelRoleRecord(filepath.Join(s.root, semanticModelCandidateFile)); err != nil {
		log.Printf("timich-agent committed semantic candidate marker cleanup deferred model=%s error=%v", installed.ID, err)
	}
	if err := s.garbageCollectUnreachableModelPacksLocked(); err != nil {
		log.Printf("timich-agent committed semantic model activation GC deferred previous_model=%s error=%v", previousActive.ModelID, err)
	}
	return SemanticModelActivationResult{
		Status:        "activated",
		ModelID:       profile.ModelID,
		VectorSpaceID: profile.VectorSpaceID,
		ActivatedAt:   activatedAt,
		Profile:       profile,
	}, nil
}

func (s *SemanticModelPackStore) UninstallPack(modelID string, vectorSpaceID string) (SemanticModelUninstallResult, error) {
	if s == nil {
		return SemanticModelUninstallResult{}, ErrSemanticModelPackInvalid
	}
	modelID = strings.TrimSpace(modelID)
	vectorSpaceID = strings.TrimSpace(vectorSpaceID)
	if modelID == "" || vectorSpaceID == "" {
		return SemanticModelUninstallResult{}, ErrSemanticModelPackInvalid
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	active := s.activeRecordLocked()
	candidate := s.candidateRecordLocked()
	if activeMatches(active, modelID, vectorSpaceID) || !candidateMatches(candidate, modelID, vectorSpaceID) {
		return SemanticModelUninstallResult{}, ErrSemanticModelPackActive
	}

	installRoot, err := s.modelRoot(modelID)
	if err != nil {
		return SemanticModelUninstallResult{}, ErrSemanticModelPackInvalid
	}
	record, ok := readSemanticModelInstallRecord(filepath.Join(installRoot, semanticModelInstallFile))
	if !ok || record.ModelPack.VectorSpaceID != vectorSpaceID {
		return SemanticModelUninstallResult{}, ErrSemanticModelPackInvalid
	}
	installed, _, _, ok := installedSemanticModelPackFromRecord(record)
	if !ok {
		return SemanticModelUninstallResult{}, ErrSemanticModelPackInvalid
	}
	if err := removeSemanticModelRoleRecord(filepath.Join(s.root, semanticModelCandidateFile)); err != nil {
		return SemanticModelUninstallResult{}, fmt.Errorf("clear semantic candidate model record: %w", err)
	}
	if err := s.garbageCollectUnreachableModelPacksLocked(); err != nil {
		log.Printf("timich-agent committed semantic model uninstall GC deferred model=%s error=%v", installed.ID, err)
	}
	return SemanticModelUninstallResult{
		Status:        "uninstalled",
		ModelID:       installed.ID,
		VectorSpaceID: installed.VectorSpaceID,
		UninstalledAt: time.Now().UTC(),
	}, nil
}

func (s *SemanticModelPackStore) InstallPack(ctx context.Context, pack SemanticModelPackStatus, reader io.Reader) (SemanticModelPackInstallResult, error) {
	return s.installPack(ctx, pack, reader, nil)
}

// InstallPackWithCommitHook stages and verifies bytes first, then invokes
// beforeCommit immediately before the durable candidate-pointer transition.
func (s *SemanticModelPackStore) InstallPackWithCommitHook(ctx context.Context, pack SemanticModelPackStatus, reader io.Reader, beforeCommit func()) (SemanticModelPackInstallResult, error) {
	return s.installPack(ctx, pack, reader, beforeCommit)
}

func (s *SemanticModelPackStore) installPack(ctx context.Context, pack SemanticModelPackStatus, reader io.Reader, beforeCommit func()) (_ SemanticModelPackInstallResult, resultErr error) {
	if s == nil {
		return SemanticModelPackInstallResult{}, ErrSemanticModelPackInvalid
	}
	if reader == nil {
		return SemanticModelPackInstallResult{}, fmt.Errorf("%w: artifact body is required", ErrSemanticModelPackInvalid)
	}
	if err := validateSemanticModelPackForInstall(pack); err != nil {
		return SemanticModelPackInstallResult{}, err
	}

	modelRoot, err := s.modelRoot(pack.ID)
	if err != nil {
		return SemanticModelPackInstallResult{}, err
	}
	if err := os.MkdirAll(modelRoot, 0o700); err != nil {
		return SemanticModelPackInstallResult{}, fmt.Errorf("create semantic model pack install directory: %w", err)
	}
	installDir, err := os.MkdirTemp(modelRoot, ".install-*")
	if err != nil {
		return SemanticModelPackInstallResult{}, fmt.Errorf("create semantic model pack staging directory: %w", err)
	}
	cleanupInstallDir := true
	defer func() {
		if cleanupInstallDir {
			resultErr = errors.Join(resultErr, removeSemanticModelInstallPath(installDir))
		}
	}()
	artifactDir := filepath.Join(installDir, "artifact")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return SemanticModelPackInstallResult{}, fmt.Errorf("create semantic model pack artifact directory: %w", err)
	}
	expectedSize, err := semanticPackExpectedArtifactSize(pack.Artifact.SizeBytes)
	if err != nil {
		return SemanticModelPackInstallResult{}, fmt.Errorf("%w: %v", ErrSemanticModelPackInvalid, err)
	}
	if err := ensureSemanticPackStorage(artifactDir, expectedSize); err != nil {
		return SemanticModelPackInstallResult{}, err
	}
	artifactPath := filepath.Join(artifactDir, pack.Artifact.Filename)
	tempFile, err := os.CreateTemp(artifactDir, pack.Artifact.Filename+".tmp-*")
	if err != nil {
		return SemanticModelPackInstallResult{}, fmt.Errorf("create semantic model artifact temp file: %w", err)
	}
	tempPath := tempFile.Name()
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := tempFile.Chmod(0o600); err != nil {
		_ = tempFile.Close()
		return SemanticModelPackInstallResult{}, fmt.Errorf("chmod semantic model artifact temp file: %w", err)
	}

	hasher := sha256.New()
	written, err := copySemanticPackArtifact(ctx, io.MultiWriter(tempFile, hasher), reader, expectedSize)
	if err != nil {
		_ = tempFile.Close()
		if errors.Is(err, errSemanticPackArtifactTooLarge) {
			return SemanticModelPackInstallResult{}, fmt.Errorf("%w: exceeded %d bytes", ErrSemanticModelPackSizeMismatch, expectedSize)
		}
		return SemanticModelPackInstallResult{}, fmt.Errorf("write semantic model artifact: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return SemanticModelPackInstallResult{}, fmt.Errorf("sync semantic model artifact: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return SemanticModelPackInstallResult{}, fmt.Errorf("close semantic model artifact: %w", err)
	}

	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	if actualSHA != strings.ToLower(pack.Artifact.SHA256) {
		return SemanticModelPackInstallResult{}, fmt.Errorf("%w: got %s", ErrSemanticModelPackChecksumMismatch, actualSHA)
	}
	if written != expectedSize {
		return SemanticModelPackInstallResult{}, fmt.Errorf("%w: got %d want %d", ErrSemanticModelPackSizeMismatch, written, expectedSize)
	}
	if err := os.Rename(tempPath, artifactPath); err != nil {
		return SemanticModelPackInstallResult{}, fmt.Errorf("install semantic model artifact: %w", err)
	}
	cleanupTemp = false
	runtimePath, err := s.prepareRuntimeLayout(pack, installDir, artifactPath)
	if err != nil {
		return SemanticModelPackInstallResult{}, err
	}
	installedAt := time.Now().UTC()
	installID := fmt.Sprintf("%d-%s", installedAt.UnixNano(), actualSHA[:16])
	installedPack := pack
	installedPack.Role = semanticModelRoleCandidate
	installedPack.Status = "installed"
	installedPack.Installed = true
	installedPack.InstalledAt = installedAt
	installedPack.Artifact.SizeBytes = written
	installedPack.SizeBytes = written
	finalDir := filepath.Join(modelRoot, fmt.Sprintf("%s-%d", actualSHA[:16], installedAt.UnixNano()))

	// Keep multi-gigabyte download, fsync, and extraction outside the store
	// mutex. Only compatibility revalidation and durable pointer transitions
	// need to exclude read-side profile resolution.
	if beforeCommit != nil {
		beforeCommit()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rollbackPath := filepath.Join(modelRoot, semanticModelRollbackFile)
	if _, statErr := os.Stat(rollbackPath); statErr == nil {
		rollback, ok := readSemanticModelPackRollbackRecord(rollbackPath)
		if !ok || !s.semanticModelInstallCommittedLocked(rollback) {
			return SemanticModelPackInstallResult{}, fmt.Errorf("%w: model %q already has an install awaiting executable proof", ErrSemanticModelPackInvalid, pack.ID)
		}
		if err := removeSemanticModelRoleRecord(rollbackPath); err != nil {
			log.Printf("timich-agent stale committed semantic model marker will be replaced model=%s install_id=%s error=%v", rollback.ReplacementModelID, rollback.InstallID, err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return SemanticModelPackInstallResult{}, fmt.Errorf("inspect semantic model install transaction: %w", statErr)
	}
	previousCandidate := s.candidateRecordLocked()
	active := s.activeRecordLocked()
	identityPath := filepath.Join(modelRoot, semanticModelIdentityFile)
	if _, statErr := os.Stat(identityPath); statErr == nil {
		identity, ok := readSemanticModelIdentityRecord(identityPath)
		if !ok {
			return SemanticModelPackInstallResult{}, fmt.Errorf("%w: model id %q has an unreadable compatibility identity", ErrSemanticModelPackInvalid, pack.ID)
		}
		if !semanticModelIdentityCompatible(identity, pack) {
			return SemanticModelPackInstallResult{}, fmt.Errorf(
				"%w: model id %q is reserved for a different vector space, dimension, input kind, or runtime",
				ErrSemanticModelPackInvalid,
				pack.ID,
			)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return SemanticModelPackInstallResult{}, fmt.Errorf("inspect semantic model identity: %w", statErr)
	}
	var previous *semanticModelPackInstallRecord
	if current, ok := readSemanticModelInstallRecord(filepath.Join(modelRoot, semanticModelInstallFile)); ok &&
		!semanticModelPackIdentityCompatible(current.ModelPack, pack) {
		return SemanticModelPackInstallResult{}, fmt.Errorf(
			"%w: model id %q is already installed with a different vector space, dimension, input kind, or runtime",
			ErrSemanticModelPackInvalid,
			pack.ID,
		)
	} else if ok {
		value := current
		previous = &value
	}
	if err := os.Rename(installDir, finalDir); err != nil {
		return SemanticModelPackInstallResult{}, fmt.Errorf("activate semantic model pack directory: %w", err)
	}
	cleanupInstallDir = false
	artifactPath = filepath.Join(finalDir, "artifact", pack.Artifact.Filename)
	if strings.TrimSpace(runtimePath) != "" {
		relativeRuntimePath, relativeErr := filepath.Rel(installDir, runtimePath)
		if relativeErr != nil {
			return SemanticModelPackInstallResult{}, errors.Join(fmt.Errorf("resolve semantic model runtime path: %w", relativeErr), removeSemanticModelInstallPath(finalDir))
		}
		if relativeRuntimePath == ".." || strings.HasPrefix(relativeRuntimePath, ".."+string(filepath.Separator)) {
			return SemanticModelPackInstallResult{}, errors.Join(errors.New("semantic model runtime path escaped the staged install"), removeSemanticModelInstallPath(finalDir))
		}
		runtimePath = filepath.Join(finalDir, relativeRuntimePath)
	}
	record := semanticModelPackInstallRecord{
		InstallID:    installID,
		ModelPack:    installedPack,
		ArtifactPath: artifactPath,
		RuntimePath:  runtimePath,
		InstalledAt:  installedAt,
		BytesWritten: written,
	}
	replacementCandidate := !activeMatches(active, installedPack.ID, installedPack.VectorSpaceID)
	rollback := semanticModelPackRollbackRecord{
		InstallID:                installID,
		ReplacementModelID:       installedPack.ID,
		ReplacementVectorSpaceID: installedPack.VectorSpaceID,
		Previous:                 previous,
		Replacement:              &record,
		ReplacementDirName:       filepath.Base(finalDir),
		ReplacementCandidate:     replacementCandidate,
		CreatedAt:                installedAt,
	}
	if previousCandidate.ModelID != "" {
		value := previousCandidate
		rollback.PreviousCandidate = &value
	}
	rollbackRaw, err := json.MarshalIndent(rollback, "", "  ")
	if err != nil {
		return SemanticModelPackInstallResult{}, errors.Join(fmt.Errorf("marshal semantic model rollback record: %w", err), removeSemanticModelInstallPath(finalDir))
	}
	rollbackRaw = append(rollbackRaw, '\n')
	if err := atomicfile.WriteFile(filepath.Join(modelRoot, semanticModelRollbackFile), rollbackRaw, 0o600); err != nil {
		return SemanticModelPackInstallResult{}, errors.Join(fmt.Errorf("write semantic model rollback record: %w", err), removeSemanticModelInstallPath(finalDir))
	}
	return SemanticModelPackInstallResult{
		Status:       "installed",
		ModelPack:    installedPack,
		InstalledAt:  installedAt,
		BytesWritten: written,
		InstallID:    installID,
	}, nil
}

// FinalizePackInstall retires the rollback copy only after the installed model
// has completed a runtime health handshake.
func (s *SemanticModelPackStore) FinalizePackInstall(modelID string, vectorSpaceID string, installID string) error {
	if s == nil {
		return ErrSemanticModelPackInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	modelRoot, err := s.modelRoot(modelID)
	if err != nil {
		return err
	}
	rollback, ok := readSemanticModelPackRollbackRecord(filepath.Join(modelRoot, semanticModelRollbackFile))
	if !ok || rollback.Replacement == nil {
		return ErrSemanticModelPackInvalid
	}
	current := *rollback.Replacement
	if current.ModelPack.VectorSpaceID != strings.TrimSpace(vectorSpaceID) || current.InstallID != strings.TrimSpace(installID) {
		return ErrSemanticModelPackInvalid
	}
	activeDir, containmentErr := modelReplacementDir(modelRoot, rollback.ReplacementDirName)
	if rollback.InstallID != current.InstallID || containmentErr != nil || validateSemanticModelInstallRecord(modelRoot, current, activeDir) != nil {
		return fmt.Errorf("%w: semantic model install changed before finalize", ErrSemanticModelPackInvalid)
	}
	if err := writeSemanticModelIdentityRecord(filepath.Join(modelRoot, semanticModelIdentityFile), current.ModelPack); err != nil {
		return err
	}
	installRaw, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal semantic model install record: %w", err)
	}
	installRaw = append(installRaw, '\n')
	if err := atomicfile.WriteFile(filepath.Join(modelRoot, semanticModelInstallFile), installRaw, 0o600); err != nil {
		return fmt.Errorf("activate semantic model install record: %w", err)
	}
	if rollback.ReplacementCandidate {
		candidate := semanticModelCandidateRecord{ModelID: current.ModelPack.ID, VectorSpaceID: current.ModelPack.VectorSpaceID, SelectedAt: current.InstalledAt}
		candidateRaw, marshalErr := json.MarshalIndent(candidate, "", "  ")
		if marshalErr != nil {
			rollbackErr := s.rollbackPackInstallLocked(modelRoot, modelID, vectorSpaceID, installID)
			return errors.Join(fmt.Errorf("marshal semantic candidate model record: %w", marshalErr), rollbackErr)
		}
		candidateRaw = append(candidateRaw, '\n')
		if err := atomicfile.WriteFile(filepath.Join(s.root, semanticModelCandidateFile), candidateRaw, 0o600); err != nil {
			rollbackErr := s.rollbackPackInstallLocked(modelRoot, modelID, vectorSpaceID, installID)
			return errors.Join(fmt.Errorf("activate semantic candidate model record: %w", err), rollbackErr)
		}
	}
	if err := removeSemanticModelRoleRecord(filepath.Join(modelRoot, semanticModelRollbackFile)); err != nil {
		log.Printf("timich-agent committed semantic model pending marker cleanup deferred model=%s install_id=%s error=%v", current.ModelPack.ID, current.InstallID, err)
	}
	if err := removeSemanticModelInstallDirs(modelRoot, activeDir); err != nil {
		log.Printf("timich-agent semantic model pack cleanup deferred model=%s install_id=%s error=%v", current.ModelPack.ID, current.InstallID, err)
	}
	if err := s.garbageCollectUnreachableModelPacksLocked(); err != nil {
		log.Printf("timich-agent semantic model role GC deferred model=%s install_id=%s error=%v", current.ModelPack.ID, current.InstallID, err)
	}
	return nil
}

// RollbackPackInstall restores the previous install and candidate pointers
// before deleting replacement bytes.
func (s *SemanticModelPackStore) RollbackPackInstall(modelID string, vectorSpaceID string, installID string) error {
	if s == nil {
		return ErrSemanticModelPackInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	modelRoot, err := s.modelRoot(modelID)
	if err != nil {
		return err
	}
	return s.rollbackPackInstallLocked(modelRoot, modelID, vectorSpaceID, installID)
}

func (s *SemanticModelPackStore) rollbackPackInstallLocked(modelRoot string, modelID string, vectorSpaceID string, installID string) error {
	rollbackPath := filepath.Join(modelRoot, semanticModelRollbackFile)
	rollback, ok := readSemanticModelPackRollbackRecord(rollbackPath)
	if !ok {
		return fmt.Errorf("%w: unreadable semantic model rollback record", ErrSemanticModelPackInvalid)
	}
	if rollback.InstallID == "" || rollback.InstallID != strings.TrimSpace(installID) ||
		rollback.ReplacementModelID != strings.TrimSpace(modelID) ||
		rollback.ReplacementVectorSpaceID != strings.TrimSpace(vectorSpaceID) {
		return fmt.Errorf("%w: semantic model install changed before rollback", ErrSemanticModelPackInvalid)
	}
	current, currentOK := readSemanticModelInstallRecord(filepath.Join(modelRoot, semanticModelInstallFile))
	replacementDir, containmentErr := modelReplacementDir(modelRoot, rollback.ReplacementDirName)
	if containmentErr != nil {
		return fmt.Errorf("%w: semantic model replacement escaped its pack root", ErrSemanticModelPackInvalid)
	}
	replacementCurrent := currentOK && current.InstallID == rollback.InstallID &&
		validateSemanticModelInstallRecord(modelRoot, current, replacementDir) == nil
	previousCurrent := currentOK && rollback.Previous != nil && current.ArtifactPath == rollback.Previous.ArtifactPath
	if !replacementCurrent && !previousCurrent && (currentOK || rollback.Previous != nil) {
		return fmt.Errorf("%w: semantic model install pointer changed before rollback", ErrSemanticModelPackInvalid)
	}
	if rollback.Previous != nil {
		previousDir := filepath.Dir(filepath.Dir(rollback.Previous.ArtifactPath))
		if validateSemanticModelInstallRecord(modelRoot, *rollback.Previous, previousDir) != nil {
			return fmt.Errorf("%w: previous semantic model install escaped its pack root", ErrSemanticModelPackInvalid)
		}
		previousRaw, err := json.MarshalIndent(rollback.Previous, "", "  ")
		if err != nil {
			return err
		}
		previousRaw = append(previousRaw, '\n')
		if err := atomicfile.WriteFile(filepath.Join(modelRoot, semanticModelInstallFile), previousRaw, 0o600); err != nil {
			return fmt.Errorf("restore semantic model install record: %w", err)
		}
	} else if err := removeSemanticModelRoleRecord(filepath.Join(modelRoot, semanticModelInstallFile)); err != nil {
		return fmt.Errorf("clear semantic model install record: %w", err)
	}
	if rollback.ReplacementCandidate {
		if rollback.PreviousCandidate != nil {
			candidateRaw, err := json.MarshalIndent(rollback.PreviousCandidate, "", "  ")
			if err != nil {
				return err
			}
			candidateRaw = append(candidateRaw, '\n')
			if err := atomicfile.WriteFile(filepath.Join(s.root, semanticModelCandidateFile), candidateRaw, 0o600); err != nil {
				return fmt.Errorf("restore semantic candidate model record: %w", err)
			}
		} else if err := removeSemanticModelRoleRecord(filepath.Join(s.root, semanticModelCandidateFile)); err != nil {
			return fmt.Errorf("clear semantic candidate model record: %w", err)
		}
	}
	if err := removeSemanticModelRoleRecord(rollbackPath); err != nil {
		return fmt.Errorf("clear semantic model rollback record: %w", err)
	}
	if err := os.RemoveAll(replacementDir); err != nil {
		return fmt.Errorf("remove failed semantic model replacement: %w", err)
	}
	keepDir := ""
	if rollback.Previous != nil {
		keepDir = filepath.Dir(filepath.Dir(rollback.Previous.ArtifactPath))
	}
	if err := removeSemanticModelInstallDirs(modelRoot, keepDir); err != nil {
		return fmt.Errorf("remove superseded semantic model installs after rollback: %w", err)
	}
	return nil
}

func (s *SemanticModelPackStore) semanticModelInstallCommittedLocked(rollback semanticModelPackRollbackRecord) bool {
	if rollback.Replacement == nil {
		return false
	}
	modelRoot, err := s.modelRoot(rollback.ReplacementModelID)
	if err != nil {
		return false
	}
	current, ok := readSemanticModelInstallRecord(filepath.Join(modelRoot, semanticModelInstallFile))
	if !ok || current.InstallID != rollback.InstallID {
		return false
	}
	if !rollback.ReplacementCandidate {
		return true
	}
	return candidateMatches(s.candidateRecordLocked(), rollback.ReplacementModelID, rollback.ReplacementVectorSpaceID)
}

func semanticModelPackIdentityCompatible(current SemanticModelPackStatus, next SemanticModelPackStatus) bool {
	return strings.TrimSpace(current.ID) == strings.TrimSpace(next.ID) &&
		strings.TrimSpace(current.VectorSpaceID) == strings.TrimSpace(next.VectorSpaceID) &&
		current.EmbeddingDim == next.EmbeddingDim &&
		strings.TrimSpace(current.InputKind) == strings.TrimSpace(next.InputKind) &&
		strings.TrimSpace(current.Runtime) == strings.TrimSpace(next.Runtime)
}

func semanticModelIdentityCompatible(identity semanticModelIdentityRecord, pack SemanticModelPackStatus) bool {
	return strings.TrimSpace(identity.ModelID) == strings.TrimSpace(pack.ID) &&
		strings.TrimSpace(identity.VectorSpaceID) == strings.TrimSpace(pack.VectorSpaceID) &&
		identity.EmbeddingDim == pack.EmbeddingDim &&
		strings.TrimSpace(identity.InputKind) == strings.TrimSpace(pack.InputKind) &&
		strings.TrimSpace(identity.Runtime) == strings.TrimSpace(pack.Runtime)
}

func readSemanticModelIdentityRecord(path string) (semanticModelIdentityRecord, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return semanticModelIdentityRecord{}, false
	}
	var identity semanticModelIdentityRecord
	if err := json.Unmarshal(raw, &identity); err != nil ||
		identity.ModelID == "" || identity.VectorSpaceID == "" || identity.EmbeddingDim <= 0 ||
		identity.InputKind == "" || identity.Runtime == "" {
		return semanticModelIdentityRecord{}, false
	}
	return identity, true
}

func writeSemanticModelIdentityRecord(path string, pack SemanticModelPackStatus) error {
	if current, ok := readSemanticModelIdentityRecord(path); ok {
		if semanticModelIdentityCompatible(current, pack) {
			return nil
		}
		return fmt.Errorf("%w: semantic model identity changed", ErrSemanticModelPackInvalid)
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w: semantic model identity is unreadable", ErrSemanticModelPackInvalid)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect semantic model identity: %w", err)
	}
	identity := semanticModelIdentityRecord{
		ModelID:       strings.TrimSpace(pack.ID),
		VectorSpaceID: strings.TrimSpace(pack.VectorSpaceID),
		EmbeddingDim:  pack.EmbeddingDim,
		InputKind:     strings.TrimSpace(pack.InputKind),
		Runtime:       strings.TrimSpace(pack.Runtime),
	}
	raw, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal semantic model identity: %w", err)
	}
	raw = append(raw, '\n')
	if err := atomicfile.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write semantic model identity: %w", err)
	}
	return nil
}

func removeSemanticModelInstallDirs(modelRoot string, activeDir string) error {
	entries, err := os.ReadDir(modelRoot)
	if err != nil {
		return err
	}
	activeDir = filepath.Clean(activeDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(modelRoot, entry.Name())
		if filepath.Clean(path) == activeDir {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func removeSemanticModelInstallPath(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove semantic model install directory %q: %w", filepath.Base(path), err)
	}
	return nil
}

func (s *SemanticModelPackStore) installedPack(modelID string, vectorSpaceID string) (SemanticModelPackStatus, string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	installRoot, err := s.modelRoot(modelID)
	if err != nil {
		return SemanticModelPackStatus{}, "", "", false
	}
	record, ok := readSemanticModelInstallRecord(filepath.Join(installRoot, semanticModelInstallFile))
	if !ok {
		return SemanticModelPackStatus{}, "", "", false
	}
	if strings.TrimSpace(vectorSpaceID) != "" && record.ModelPack.VectorSpaceID != vectorSpaceID {
		return SemanticModelPackStatus{}, "", "", false
	}
	pack, artifactPath, runtimePath, ok := installedSemanticModelPackFromRecord(record)
	return pack, artifactPath, runtimePath, ok
}

func (s *SemanticModelPackStore) activeInstalledPack(modelID string, vectorSpaceID string) (SemanticModelPackStatus, string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := s.activeRecordLocked()
	if !activeMatches(active, strings.TrimSpace(modelID), strings.TrimSpace(vectorSpaceID)) {
		return SemanticModelPackStatus{}, "", "", false
	}
	record, ok := s.activeInstallRecordLocked(active)
	if !ok {
		return SemanticModelPackStatus{}, "", "", false
	}
	return installedSemanticModelPackFromRecord(record)
}

func (s *SemanticModelPackStore) activeInstallRecordLocked(active semanticModelActiveRecord) (semanticModelPackInstallRecord, bool) {
	modelRoot, err := s.modelRoot(active.ModelID)
	if err != nil {
		return semanticModelPackInstallRecord{}, false
	}
	current, ok := readSemanticModelInstallRecord(filepath.Join(modelRoot, semanticModelInstallFile))
	if !ok || current.ModelPack.VectorSpaceID != active.VectorSpaceID {
		return semanticModelPackInstallRecord{}, false
	}
	currentDir := filepath.Dir(filepath.Dir(current.ArtifactPath))
	if validateSemanticModelInstallRecord(modelRoot, current, currentDir) != nil {
		return semanticModelPackInstallRecord{}, false
	}
	// install.json is the durable commit pointer. A rollback record is only
	// authority before that pointer changes; stale marker cleanup must never
	// make readers select superseded model bytes.
	return current, true
}

func installedSemanticModelPackFromRecord(record semanticModelPackInstallRecord) (SemanticModelPackStatus, string, string, bool) {
	pack := record.ModelPack
	pack.Role = semanticModelRoleCandidate
	pack.Status = "installed"
	pack.Installed = true
	pack.InstalledAt = record.InstalledAt
	return pack, record.ArtifactPath, record.RuntimePath, true
}

func readSemanticModelInstallRecord(path string) (semanticModelPackInstallRecord, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return semanticModelPackInstallRecord{}, false
	}
	var record semanticModelPackInstallRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return semanticModelPackInstallRecord{}, false
	}
	if record.ModelPack.ID == "" || record.ArtifactPath == "" {
		return semanticModelPackInstallRecord{}, false
	}
	return record, true
}

func readSemanticModelPackRollbackRecord(path string) (semanticModelPackRollbackRecord, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return semanticModelPackRollbackRecord{}, false
	}
	var record semanticModelPackRollbackRecord
	if err := json.Unmarshal(raw, &record); err != nil ||
		strings.TrimSpace(record.InstallID) == "" ||
		strings.TrimSpace(record.ReplacementModelID) == "" ||
		strings.TrimSpace(record.ReplacementVectorSpaceID) == "" ||
		strings.TrimSpace(record.ReplacementDirName) == "" ||
		record.Replacement == nil || record.Replacement.InstallID != record.InstallID {
		return semanticModelPackRollbackRecord{}, false
	}
	return record, true
}

func (s *SemanticModelPackStore) activeRecord() semanticModelActiveRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeRecordLocked()
}

func (s *SemanticModelPackStore) activeRecordLocked() semanticModelActiveRecord {
	record, ok := readSemanticModelActiveRecord(filepath.Join(s.root, semanticModelActiveFile))
	if !ok {
		return semanticModelActiveRecord{}
	}
	return record
}

func (s *SemanticModelPackStore) candidateRecord() semanticModelCandidateRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.candidateRecordLocked()
}

func (s *SemanticModelPackStore) candidateRecordLocked() semanticModelCandidateRecord {
	record, ok := readSemanticModelCandidateRecord(filepath.Join(s.root, semanticModelCandidateFile))
	if !ok {
		return semanticModelCandidateRecord{}
	}
	return record
}

func (s *SemanticModelPackStore) activeIdentityMatches(modelID string, vectorSpaceID string) bool {
	if s == nil {
		return false
	}
	return activeMatches(s.activeRecord(), modelID, vectorSpaceID)
}

func (s *SemanticModelPackStore) candidateIdentityMatches(modelID string, vectorSpaceID string) bool {
	if s == nil {
		return false
	}
	return candidateMatches(s.candidateRecord(), modelID, vectorSpaceID)
}

func activeMatches(active semanticModelActiveRecord, modelID string, vectorSpaceID string) bool {
	return active.ModelID == modelID && active.VectorSpaceID == vectorSpaceID
}

func candidateMatches(candidate semanticModelCandidateRecord, modelID string, vectorSpaceID string) bool {
	return candidate.ModelID == modelID && candidate.VectorSpaceID == vectorSpaceID
}

func (s *SemanticModelPackStore) removeUnownedModelPackLocked(modelID string, protectedModelIDs ...string) error {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	for _, protected := range protectedModelIDs {
		if modelID == strings.TrimSpace(protected) {
			return nil
		}
	}
	modelRoot, err := s.modelRoot(modelID)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(modelRoot, semanticModelInstallFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return removeSemanticModelInstallDirs(modelRoot, "")
}

func (s *SemanticModelPackStore) garbageCollectUnreachableModelPacksLocked() error {
	active := s.activeRecordLocked()
	candidate := s.candidateRecordLocked()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("list semantic model packs for role GC: %w", err)
	}
	var cleanupErr error
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == active.ModelID || entry.Name() == candidate.ModelID {
			continue
		}
		if err := s.removeUnownedModelPackLocked(entry.Name(), active.ModelID, candidate.ModelID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove unreachable semantic model pack %q: %w", entry.Name(), err))
		}
	}
	return cleanupErr
}

func (s *SemanticModelPackStore) activeProfileLocked(active semanticModelActiveRecord) (SemanticModelProfileStatus, bool) {
	record, ok := s.activeInstallRecordLocked(active)
	if !ok {
		return SemanticModelProfileStatus{}, false
	}
	pack, artifactPath, runtimePath, ok := installedSemanticModelPackFromRecord(record)
	if !ok {
		return SemanticModelProfileStatus{}, false
	}
	pack.Role = semanticModelRoleActive
	packCopy := pack
	return SemanticModelProfileStatus{
		ModelID:       pack.ID,
		VectorSpaceID: pack.VectorSpaceID,
		EmbeddingDim:  pack.EmbeddingDim,
		Role:          semanticModelRoleActive,
		ProfileKind:   semanticProfileKindModelPack,
		InputKind:     pack.InputKind,
		ModelPack:     &packCopy,
		Runtime:       semanticInstalledModelRuntimeStatus(pack, artifactPath, runtimePath, s.runtimeHelperPath),
	}, true
}

func readSemanticModelActiveRecord(path string) (semanticModelActiveRecord, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return semanticModelActiveRecord{}, false
	}
	var record semanticModelActiveRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return semanticModelActiveRecord{}, false
	}
	if record.ModelID == "" || record.VectorSpaceID == "" {
		return semanticModelActiveRecord{}, false
	}
	return record, true
}

func readSemanticModelCandidateRecord(path string) (semanticModelCandidateRecord, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return semanticModelCandidateRecord{}, false
	}
	var record semanticModelCandidateRecord
	if err := json.Unmarshal(raw, &record); err != nil || record.ModelID == "" || record.VectorSpaceID == "" {
		return semanticModelCandidateRecord{}, false
	}
	return record, true
}

func removeSemanticModelRoleRecord(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validateSemanticModelPackForInstall(pack SemanticModelPackStatus) error {
	if safeSemanticModelPathPart(pack.ID) == "" {
		return fmt.Errorf("%w: id is required", ErrSemanticModelPackInvalid)
	}
	if strings.TrimSpace(pack.Version) == "" || safeSemanticModelPathPart(pack.Version) == "" {
		return fmt.Errorf("%w: version is invalid", ErrSemanticModelPackInvalid)
	}
	if pack.VectorSpaceID == "" || pack.EmbeddingDim <= 0 || pack.InputKind == "" {
		return fmt.Errorf("%w: profile metadata is incomplete", ErrSemanticModelPackInvalid)
	}
	if pack.Artifact == nil {
		return fmt.Errorf("%w: artifact is required", ErrSemanticModelPackInvalid)
	}
	if safeSemanticArtifactFilename(pack.Artifact.Filename) == "" {
		return fmt.Errorf("%w: artifact filename is invalid", ErrSemanticModelPackInvalid)
	}
	if !validSHA256Hex(pack.Artifact.SHA256) {
		return fmt.Errorf("%w: artifact SHA-256 is required", ErrSemanticModelPackInvalid)
	}
	return nil
}

func (s *SemanticModelPackStore) modelRoot(modelID string) (string, error) {
	modelPart := safeSemanticModelPathPart(modelID)
	if modelPart == "" {
		return "", fmt.Errorf("%w: id is invalid", ErrSemanticModelPackInvalid)
	}
	return filepath.Join(s.root, modelPart), nil
}

func modelReplacementDir(modelRoot string, dirName string) (string, error) {
	dirName = strings.TrimSpace(dirName)
	if dirName == "" || filepath.IsAbs(dirName) || filepath.Base(dirName) != dirName || dirName == "." || dirName == ".." {
		return "", ErrSemanticModelPackInvalid
	}
	candidate := filepath.Join(modelRoot, dirName)
	relative, err := filepath.Rel(modelRoot, candidate)
	if err != nil || relative != dirName || strings.Contains(relative, string(filepath.Separator)) {
		return "", ErrSemanticModelPackInvalid
	}
	return candidate, nil
}

func validateSemanticModelInstallRecord(modelRoot string, record semanticModelPackInstallRecord, installDir string) error {
	expectedRoot, err := modelReplacementDir(modelRoot, filepath.Base(installDir))
	if err != nil || filepath.Clean(expectedRoot) != filepath.Clean(installDir) {
		return ErrSemanticModelPackInvalid
	}
	for _, path := range []string{record.ArtifactPath, record.RuntimePath} {
		if strings.TrimSpace(path) == "" {
			continue
		}
		relative, relErr := filepath.Rel(installDir, path)
		if relErr != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return ErrSemanticModelPackInvalid
		}
	}
	return nil
}

func safeSemanticArtifactFilename(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "." || trimmed == ".." {
		return ""
	}
	if strings.ContainsAny(trimmed, `/\`) || filepath.Base(trimmed) != trimmed {
		return ""
	}
	return trimmed
}

func safeSemanticModelPathPart(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "." || trimmed == ".." {
		return ""
	}
	for _, r := range trimmed {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return ""
	}
	return trimmed
}

func validSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return true
}

func copyWithContext(ctx context.Context, writer io.Writer, reader io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := reader.Read(buffer)
		if n > 0 {
			writeN, writeErr := writer.Write(buffer[:n])
			written += int64(writeN)
			if writeErr != nil {
				return written, writeErr
			}
			if writeN != n {
				return written, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}
