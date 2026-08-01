package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rsahara/timich-agent/internal/config"
	"golang.org/x/sync/semaphore"
)

// Local media root inspection statuses are persisted by discovery and mapped
// to administrator-facing readiness summaries by the runtime.
const (
	LocalMediaRootStatusReady        = "ready"
	LocalMediaRootStatusMissing      = "missing"
	LocalMediaRootStatusNotDirectory = "not_directory"
	LocalMediaRootStatusUnreadable   = "unreadable"
)

// ErrLocalMediaRootSymlink identifies a configured root that is itself a symlink.
var ErrLocalMediaRootSymlink = errors.New("local media root must not be a symbolic link")

// ErrLocalMediaRootIdentityUnavailable identifies a platform or filesystem
// where the Agent cannot safely compare a root with the last successful scan.
var ErrLocalMediaRootIdentityUnavailable = errors.New("local media root identity is unavailable")

// ErrLocalMediaRootNotTrusted identifies a configured root that cannot be
// used until discovery establishes, or an administrator accepts, its identity.
var ErrLocalMediaRootNotTrusted = errors.New("local media root is not trusted")

var errLocalMediaRootChanged = errors.New("local media root changed during scan")

const localMediaRootTransitionCapacity int64 = 1 << 30
const initialLocalMediaRootGeneration int64 = 1

type localMediaRootTransitionKey struct {
	sourceKey string
	rootKey   string
}

type localMediaRootTransitionGate struct {
	semaphore *semaphore.Weighted
}

type localMediaRootTransitionLease struct {
	gate   *localMediaRootTransitionGate
	weight int64
}

func (lease *localMediaRootTransitionLease) release() {
	if lease == nil || lease.gate == nil || lease.weight == 0 {
		return
	}
	lease.gate.semaphore.Release(lease.weight)
	lease.gate = nil
	lease.weight = 0
}

type localMediaRootInspection struct {
	status   string
	info     os.FileInfo
	identity string
}

type trustedLocalMediaRoot struct {
	transition            *localMediaRootTransitionLease
	datasource            config.DatasourceConfig
	root                  config.LocalMediaRootConfig
	handle                *os.Root
	rootGeneration        int64
	reconciliationPending bool
}

type localMediaRootWorkState struct {
	identity              string
	generation            int64
	reconciliationPending bool
}

type trustedLocalMediaRootReference struct {
	sourceKey             string
	rootKey               string
	rootGeneration        int64
	reconciliationPending bool
}

func (root *trustedLocalMediaRoot) matchesJobRoot(rootKey string, rootGeneration int64) bool {
	return root != nil &&
		strings.TrimSpace(root.root.Key) == strings.TrimSpace(rootKey) &&
		root.rootGeneration == normalizeLocalMediaRootGeneration(rootGeneration)
}

func normalizeLocalMediaRootGeneration(generation int64) int64 {
	if generation <= 0 {
		return initialLocalMediaRootGeneration
	}
	return generation
}

func (root *trustedLocalMediaRoot) Close() error {
	if root == nil {
		return nil
	}
	var err error
	if root.handle != nil {
		err = root.handle.Close()
		root.handle = nil
	}
	root.transition.release()
	root.transition = nil
	return err
}

func (s *Service) localRootTransitionGate(sourceKey string, rootKey string) *localMediaRootTransitionGate {
	key := localMediaRootTransitionKey{
		sourceKey: strings.TrimSpace(sourceKey),
		rootKey:   strings.TrimSpace(rootKey),
	}
	s.localRootTransitionGatesMu.Lock()
	defer s.localRootTransitionGatesMu.Unlock()
	if s.localRootTransitionGates == nil {
		s.localRootTransitionGates = make(map[localMediaRootTransitionKey]*localMediaRootTransitionGate)
	}
	gate := s.localRootTransitionGates[key]
	if gate == nil {
		gate = &localMediaRootTransitionGate{semaphore: semaphore.NewWeighted(localMediaRootTransitionCapacity)}
		s.localRootTransitionGates[key] = gate
	}
	return gate
}

func sameLocalRootConfiguration(
	datasource *config.DatasourceConfig,
	root *config.LocalMediaRootConfig,
	expectedDatasource config.DatasourceConfig,
	expectedRoot config.LocalMediaRootConfig,
) bool {
	return datasource != nil && root != nil &&
		strings.TrimSpace(datasource.SourceKey) == strings.TrimSpace(expectedDatasource.SourceKey) &&
		strings.TrimSpace(datasource.RootKey) == strings.TrimSpace(expectedDatasource.RootKey) &&
		strings.TrimSpace(root.Key) == strings.TrimSpace(expectedRoot.Key) &&
		strings.TrimSpace(root.Path) == strings.TrimSpace(expectedRoot.Path)
}

func (s *Service) acquireLocalRootTransition(
	ctx context.Context,
	datasource config.DatasourceConfig,
	root config.LocalMediaRootConfig,
	exclusive bool,
	wait bool,
) (*localMediaRootTransitionLease, bool, error) {
	gate := s.localRootTransitionGate(datasource.SourceKey, root.Key)
	weight := int64(1)
	if exclusive {
		weight = localMediaRootTransitionCapacity
	}
	if wait {
		if err := gate.semaphore.Acquire(ctx, weight); err != nil {
			return nil, false, err
		}
	} else if !gate.semaphore.TryAcquire(weight) {
		return nil, false, nil
	}
	lease := &localMediaRootTransitionLease{gate: gate, weight: weight}
	currentDatasource, currentRoot, err := s.localDatasourceAndRoot(datasource.SourceKey)
	if err != nil {
		lease.release()
		return nil, false, err
	}
	if !sameLocalRootConfiguration(currentDatasource, currentRoot, datasource, root) {
		lease.release()
		return nil, false, ErrNoDatasourceConfigured
	}
	return lease, true, nil
}

// acquireTrustedLocalMediaRoot pins the exact root whose identity was last
// established by successful reconciliation or explicit administrator acceptance.
// The returned guard must remain open until all root-derived durable work is
// complete so identity promotion cannot race that work.
func (s *Service) acquireTrustedLocalMediaRoot(ctx context.Context, sourceKey string) (*trustedLocalMediaRoot, error) {
	root, _, err := s.acquireTrustedLocalMediaRootWithMode(ctx, sourceKey, true)
	return root, err
}

func (s *Service) acquireTrustedLocalMediaRootWithMode(ctx context.Context, sourceKey string, wait bool) (*trustedLocalMediaRoot, bool, error) {
	if s == nil || s.catalog == nil {
		return nil, false, ErrNoDatasourceConfigured
	}
	datasource, rootConfig, err := s.localDatasourceAndRoot(sourceKey)
	if err != nil {
		return nil, false, err
	}
	transition, acquired, err := s.acquireLocalRootTransition(ctx, *datasource, *rootConfig, false, wait)
	if err != nil || !acquired {
		return nil, acquired, err
	}
	release := true
	defer func() {
		if release {
			transition.release()
		}
	}()
	workState, err := s.localMediaRootWorkState(ctx, datasource.SourceKey, rootConfig.Key)
	if err != nil {
		return nil, false, err
	}
	if workState.identity == "" {
		return nil, false, fmt.Errorf("%w: no successful root scan", ErrLocalMediaRootNotTrusted)
	}
	inspection, handle, err := openLocalMediaRoot(rootConfig.Path)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrLocalMediaRootNotTrusted, err)
	}
	if inspection.identity != workState.identity {
		_ = handle.Close()
		return nil, false, fmt.Errorf("%w: %w", ErrLocalMediaRootNotTrusted, ErrLocalMediaRootIdentityChanged)
	}
	release = false
	return &trustedLocalMediaRoot{
		transition:            transition,
		datasource:            *datasource,
		root:                  *rootConfig,
		handle:                handle,
		rootGeneration:        normalizeLocalMediaRootGeneration(workState.generation),
		reconciliationPending: workState.reconciliationPending,
	}, true, nil
}

func (s *Service) trustedLocalMediaRootReferences(ctx context.Context, requestedSourceKey string) ([]trustedLocalMediaRootReference, error) {
	requestedSourceKey = strings.TrimSpace(requestedSourceKey)
	sourceKeys := s.LocalDatasourceSourceKeys()
	if requestedSourceKey != "" {
		sourceKeys = []string{requestedSourceKey}
	}
	trusted := make([]trustedLocalMediaRootReference, 0, len(sourceKeys))
	for _, sourceKey := range sourceKeys {
		root, acquired, err := s.acquireTrustedLocalMediaRootWithMode(ctx, sourceKey, false)
		if err != nil {
			if errors.Is(err, ErrLocalMediaRootNotTrusted) || errors.Is(err, ErrNoDatasourceConfigured) {
				continue
			}
			return nil, err
		}
		if !acquired {
			continue
		}
		trusted = append(trusted, trustedLocalMediaRootReference{
			sourceKey:             root.datasource.SourceKey,
			rootKey:               root.root.Key,
			rootGeneration:        root.rootGeneration,
			reconciliationPending: root.reconciliationPending,
		})
		_ = root.Close()
	}
	return trusted, nil
}

// InspectLocalMediaRoot applies the filesystem-readiness policy shared by
// discovery and administrator-facing status responses.
func InspectLocalMediaRoot(rootPath string) (string, error) {
	inspection, root, err := openLocalMediaRoot(rootPath)
	if root != nil {
		_ = root.Close()
	}
	return inspection.status, err
}

func openLocalMediaRoot(rootPath string) (localMediaRootInspection, *os.Root, error) {
	inspection, err := lstatLocalMediaRoot(rootPath)
	if err != nil {
		return inspection, nil, err
	}
	root, err := os.OpenRoot(strings.TrimSpace(rootPath))
	if err != nil {
		inspection.status = LocalMediaRootStatusUnreadable
		return inspection, nil, err
	}
	closeWithError := func(result localMediaRootInspection, resultErr error) (localMediaRootInspection, *os.Root, error) {
		_ = root.Close()
		return result, nil, resultErr
	}
	pinnedInfo, err := root.Stat(".")
	if err != nil {
		inspection.status = LocalMediaRootStatusUnreadable
		return closeWithError(inspection, err)
	}
	if !pinnedInfo.IsDir() || inspection.info == nil || !os.SameFile(inspection.info, pinnedInfo) {
		inspection.status = LocalMediaRootStatusUnreadable
		return closeWithError(inspection, errLocalMediaRootChanged)
	}
	inspection.info = pinnedInfo
	directory, err := root.Open(".")
	if err != nil {
		inspection.status = LocalMediaRootStatusUnreadable
		return closeWithError(inspection, err)
	}
	_, readErr := directory.Readdirnames(1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		inspection.status = LocalMediaRootStatusUnreadable
		return closeWithError(inspection, readErr)
	}
	if closeErr != nil {
		inspection.status = LocalMediaRootStatusUnreadable
		return closeWithError(inspection, closeErr)
	}
	if err := validateLocalMediaRootIdentity(rootPath, pinnedInfo); err != nil {
		inspection.status = LocalMediaRootStatusUnreadable
		return closeWithError(inspection, err)
	}
	identity, err := localMediaRootIdentity(rootPath, pinnedInfo)
	if err != nil {
		inspection.status = LocalMediaRootStatusUnreadable
		return closeWithError(inspection, err)
	}
	if strings.TrimSpace(identity) == "" {
		inspection.status = LocalMediaRootStatusUnreadable
		return closeWithError(inspection, ErrLocalMediaRootIdentityUnavailable)
	}
	inspection.identity = identity
	return inspection, root, nil
}

func lstatLocalMediaRoot(rootPath string) (localMediaRootInspection, error) {
	rootPath = strings.TrimSpace(rootPath)
	if rootPath == "" {
		return localMediaRootInspection{status: LocalMediaRootStatusMissing}, errors.New("local media root path is empty")
	}
	info, err := os.Lstat(rootPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return localMediaRootInspection{status: LocalMediaRootStatusMissing}, err
		}
		return localMediaRootInspection{status: LocalMediaRootStatusUnreadable}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return localMediaRootInspection{status: LocalMediaRootStatusUnreadable, info: info}, ErrLocalMediaRootSymlink
	}
	if !info.IsDir() {
		return localMediaRootInspection{status: LocalMediaRootStatusNotDirectory, info: info}, errors.New("local media root is not a directory")
	}
	return localMediaRootInspection{status: LocalMediaRootStatusReady, info: info}, nil
}

func validateLocalMediaRootIdentity(rootPath string, expected os.FileInfo) error {
	current, err := lstatLocalMediaRoot(rootPath)
	if err != nil {
		return err
	}
	if expected == nil || !os.SameFile(expected, current.info) {
		return errLocalMediaRootChanged
	}
	return nil
}
