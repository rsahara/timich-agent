package runtime

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/store"
)

const (
	uploadMaintenanceInterval                  = 24 * time.Hour
	uploadMaintenanceCompletedSessionRetention = 7 * 24 * time.Hour
	uploadMaintenanceAbortedSessionRetention   = 7 * 24 * time.Hour
	uploadMaintenanceFailedSessionRetention    = 30 * 24 * time.Hour
	uploadMaintenanceCommitEventRetention      = 30 * 24 * time.Hour
	uploadMaintenanceResetEventRetention       = 90 * 24 * time.Hour
	uploadMaintenanceOrphanTempRetention       = 24 * time.Hour
)

type UploadMaintenanceResult struct {
	CleanedAt              time.Time `json:"cleanedAt"`
	RemovedExpiredSessions int64     `json:"removedExpiredSessions"`
	RemovedCompleted       int64     `json:"removedCompletedSessions"`
	RemovedAborted         int64     `json:"removedAbortedSessions"`
	RemovedFailed          int64     `json:"removedFailedSessions"`
	RemovedCommitEvents    int64     `json:"removedCommitEvents"`
	RemovedResetEvents     int64     `json:"removedResetEvents"`
	RemovedTempFiles       int64     `json:"removedTempFiles"`
	RemovedOrphanTempFiles int64     `json:"removedOrphanTempFiles"`
	TempCleanupErrors      []string  `json:"tempCleanupErrors,omitempty"`
	WALCheckpointed        bool      `json:"walCheckpointed"`
}

// StartUploadMaintenance starts routine upload DB/temp cleanup for the serving
// Agent process. Tests can call RunUploadMaintenanceOnce directly.
func (a *AgentRuntime) StartUploadMaintenance() {
	if a == nil || a.uploads == nil {
		return
	}
	a.maintenanceMu.Lock()
	defer a.maintenanceMu.Unlock()
	if a.maintenanceCancel != nil {
		return
	}
	parent, release, ok := a.background.Acquire()
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	a.maintenanceCancel = cancel
	a.maintenanceWG.Add(1)
	go func() { defer release(); defer cancel(); a.runUploadMaintenanceLoop(ctx) }()
}

func (a *AgentRuntime) stopUploadMaintenance() {
	a.maintenanceMu.Lock()
	cancel := a.maintenanceCancel
	a.maintenanceCancel = nil
	a.maintenanceMu.Unlock()
	if cancel != nil {
		cancel()
		a.maintenanceWG.Wait()
	}
}

func (a *AgentRuntime) runUploadMaintenanceLoop(ctx context.Context) {
	defer a.maintenanceWG.Done()
	run := func() {
		result, err := a.RunUploadMaintenanceOnce(time.Now().UTC())
		if err != nil {
			log.Printf("timich-agent upload maintenance failed: %v", err)
			return
		}
		if result.RemovedExpiredSessions > 0 ||
			result.RemovedCompleted > 0 ||
			result.RemovedAborted > 0 ||
			result.RemovedFailed > 0 ||
			result.RemovedCommitEvents > 0 ||
			result.RemovedResetEvents > 0 ||
			result.RemovedTempFiles > 0 ||
			result.RemovedOrphanTempFiles > 0 ||
			len(result.TempCleanupErrors) > 0 {
			log.Printf(
				"timich-agent upload maintenance cleaned expired_sessions=%d completed_sessions=%d aborted_sessions=%d failed_sessions=%d commit_events=%d reset_events=%d temp_files=%d orphan_temp_files=%d temp_errors=%d",
				result.RemovedExpiredSessions,
				result.RemovedCompleted,
				result.RemovedAborted,
				result.RemovedFailed,
				result.RemovedCommitEvents,
				result.RemovedResetEvents,
				result.RemovedTempFiles,
				result.RemovedOrphanTempFiles,
				len(result.TempCleanupErrors),
			)
		}
	}
	run()

	ticker := time.NewTicker(uploadMaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// RunUploadMaintenanceOnce prunes auxiliary upload state. It keeps uploaded
// asset ledger rows intact and only removes expired/old session and audit rows.
func (a *AgentRuntime) RunUploadMaintenanceOnce(now time.Time) (UploadMaintenanceResult, error) {
	if a == nil || a.uploads == nil {
		return UploadMaintenanceResult{}, nil
	}
	now = now.UTC()
	input := store.UploadCleanupInput{
		Now:                       now,
		CompletedSessionRetention: uploadMaintenanceCompletedSessionRetention,
		AbortedSessionRetention:   uploadMaintenanceAbortedSessionRetention,
		FailedSessionRetention:    uploadMaintenanceFailedSessionRetention,
		CommitEventRetention:      uploadMaintenanceCommitEventRetention,
		ResetEventRetention:       uploadMaintenanceResetEventRetention,
	}
	candidates, err := a.uploads.UploadCleanupCandidates(input)
	if err != nil {
		return UploadMaintenanceResult{}, err
	}
	var result store.UploadCleanupResult
	var removedTempFiles int64
	var tempErrors []string
	for _, candidate := range candidates {
		release := a.tryLockUploadSession(candidate.DeviceID, candidate.UploadID)
		if release == nil {
			continue
		}
		removed, err := a.uploads.CleanupUploadSession(candidate.UploadID, input)
		if err != nil {
			release()
			return UploadMaintenanceResult{}, err
		}
		count, cleanupErrors := a.removeUploadTempFiles(removed.TempFiles)
		release()
		result.RemovedExpiredSessions += removed.RemovedExpiredSessions
		result.RemovedCompleted += removed.RemovedCompleted
		result.RemovedAborted += removed.RemovedAborted
		result.RemovedFailed += removed.RemovedFailed
		removedTempFiles += count
		tempErrors = append(tempErrors, cleanupErrors...)
	}
	events, err := a.uploads.CleanupUploadEvents(input)
	if err != nil {
		return UploadMaintenanceResult{}, err
	}
	removedOrphans, orphanErrors := a.removeOrphanUploadTempFiles(
		now.Add(-uploadMaintenanceOrphanTempRetention),
	)
	tempErrors = append(tempErrors, orphanErrors...)
	return UploadMaintenanceResult{
		CleanedAt:              events.CleanedAt,
		RemovedExpiredSessions: result.RemovedExpiredSessions,
		RemovedCompleted:       result.RemovedCompleted,
		RemovedAborted:         result.RemovedAborted,
		RemovedFailed:          result.RemovedFailed,
		RemovedCommitEvents:    events.RemovedCommitEvents,
		RemovedResetEvents:     events.RemovedResetEvents,
		RemovedTempFiles:       removedTempFiles,
		RemovedOrphanTempFiles: removedOrphans,
		TempCleanupErrors:      tempErrors,
		WALCheckpointed:        events.WALCheckpointed,
	}, nil
}

func (a *AgentRuntime) removeOrphanUploadTempFiles(cutoff time.Time) (int64, []string) {
	a.mu.RLock()
	roots := make([]config.UploadRootConfig, 0, len(a.config.UploadRoots))
	for _, root := range a.config.UploadRoots {
		roots = append(roots, normalizedUploadRootConfig(root))
	}
	a.mu.RUnlock()

	var removed int64
	var cleanupErrors []string
	for _, root := range roots {
		tempDirExists, err := uploadTempDirExists(root)
		if err != nil {
			cleanupErrors = append(cleanupErrors, err.Error())
			continue
		}
		if !tempDirExists {
			continue
		}
		tempDir, err := uploadTempAbsoluteDir(root)
		if err != nil {
			cleanupErrors = append(cleanupErrors, err.Error())
			continue
		}
		entries, err := os.ReadDir(tempDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			cleanupErrors = append(cleanupErrors, err.Error())
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, "upl_") || !strings.HasSuffix(name, ".part") {
				continue
			}
			didRemove, err := a.removeOrphanUploadTempFile(filepath.Join(tempDir, name), strings.TrimSuffix(name, ".part"), cutoff)
			if err != nil {
				cleanupErrors = append(cleanupErrors, err.Error())
				continue
			}
			if didRemove {
				removed++
			}
		}
	}
	return removed, cleanupErrors
}

func (a *AgentRuntime) removeOrphanUploadTempFile(fullPath, uploadID string, cutoff time.Time) (bool, error) {
	session, exists, err := a.uploads.GetSession(uploadID)
	if err != nil {
		return false, err
	}
	var release func()
	ownerKnown := exists
	if exists {
		release = a.tryLockUploadSession(session.DeviceID, uploadID)
	} else {
		// No device gate is known. Acquire only the session lock, then skip if
		// a session has appeared; never acquire a device gate after this lock.
		release = a.uploadSessions.tryLock(uploadID)
	}
	if release == nil {
		return false, nil
	}
	defer release()
	session, exists, err = a.uploads.GetSession(uploadID)
	if err != nil {
		return false, err
	}
	if exists && !ownerKnown {
		return false, nil // A new owner needs its device gate on a later pass.
	}
	info, err := os.Lstat(fullPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeType != 0 || info.ModTime().After(cutoff) {
		return false, nil
	}
	if exists {
		ownerRoot, configured := a.uploadRootConfig(session.SelectedRootKey)
		if !configured {
			return false, nil
		}
		ownerPath, err := uploadTempAbsolutePath(ownerRoot, session.TempRelativePath)
		if err != nil {
			return false, err
		}
		ownerInfo, err := os.Lstat(ownerPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err == nil && os.SameFile(info, ownerInfo) {
			// Different configured root keys/paths can refer to the same file.
			return false, nil
		}
	}
	if err := os.Remove(fullPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
