package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	localContentVerificationFailureBackoff = 30 * time.Minute

	LocalContentVerificationStatusRunning   = "running"
	LocalContentVerificationStatusCompleted = "completed"
	LocalContentVerificationStatusSkipped   = "skipped"

	LocalContentVerificationSkipNoIdleWorker = "no_idle_worker"
	LocalContentVerificationSkipMissedWindow = "schedule_missed"
	LocalContentVerificationSkipRootChanged  = "root_identity_changed"
)

// LocalContentVerificationResult reports one cooperative verification slice.
type LocalContentVerificationResult struct {
	SourceKey      string    `json:"sourceKey"`
	ProcessedFiles int       `json:"processedFiles"`
	VerifiedFiles  int       `json:"verifiedFiles"`
	ChangedFiles   int       `json:"changedFiles"`
	FailedFiles    int       `json:"failedFiles"`
	ReadBytes      int64     `json:"readBytes"`
	StartedAt      time.Time `json:"startedAt"`
	CompletedAt    time.Time `json:"completedAt"`
	WindowExpired  bool      `json:"windowExpired,omitempty"`
	WindowComplete bool      `json:"windowComplete"`
}

type localContentVerificationCandidate struct {
	ID                int64
	SourceKey         string
	RootKey           string
	RootGeneration    int64
	RelativePath      string
	Status            string
	SizeBytes         int64
	MTime             string
	FastSignature     string
	FileIdentity      string
	SHA1Hex           string
	AttemptedAt       sql.NullString
	VerificationError sql.NullString
}

type localContentVerificationOutcome struct {
	found    bool
	verified bool
	changed  bool
	failed   bool
	bytes    int64
}

type localContentVerificationWindow struct {
	scheduledAt time.Time
	startedAt   time.Time
	deadlineAt  time.Time
	cutoffText  string
}

// StartLocalContentVerificationWindow admits one configured daily occurrence.
// The occurrence is durable so restarts cannot run or skip it twice.
func (s *Service) StartLocalContentVerificationWindow(ctx context.Context, sourceKey string, scheduledAt time.Time, deadlineAt time.Time) (bool, error) {
	if s == nil || s.catalog == nil {
		return false, ErrNoDatasourceConfigured
	}
	if !deadlineAt.After(scheduledAt) {
		return false, fmt.Errorf("start local content verification window: deadline must follow schedule")
	}
	if err := s.ensureStateWritesAvailable(); err != nil {
		return false, err
	}
	_, root, err := s.localDatasourceAndRoot(sourceKey)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	startedAtText := formatCatalogTime(now)
	result, err := s.catalog.db.ExecContext(ctx, `UPDATE local_scan_root_state
		SET content_verification_scheduled_at = ?,
			content_verification_window_started_at = ?,
			content_verification_window_deadline_at = ?,
			content_verification_status = ?,
			content_verification_skip_reason = NULL,
			content_verification_processed_files = 0,
			content_verification_verified_files = 0,
			content_verification_changed_files = 0,
			content_verification_failed_files = 0,
			content_verification_read_bytes = 0,
			updated_at = ?
		WHERE source_key = ?
			AND root_key = ?
			AND reconciliation_pending = 0
			AND content_verification_status <> ?
			AND (
				content_verification_scheduled_at IS NULL
				OR content_verification_scheduled_at < ?
			)`,
		formatCatalogTime(scheduledAt.UTC()),
		startedAtText,
		formatCatalogTime(deadlineAt.UTC()),
		LocalContentVerificationStatusRunning,
		startedAtText,
		sourceKey,
		root.Key,
		LocalContentVerificationStatusRunning,
		formatCatalogTime(scheduledAt.UTC()),
	)
	if err != nil {
		return false, fmt.Errorf("start local content verification window: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read started local content verification rows: %w", err)
	}
	return affected > 0, nil
}

// SkipLocalContentVerificationWindow records a daily occurrence that could not
// start. It intentionally creates no file-level queue.
func (s *Service) SkipLocalContentVerificationWindow(ctx context.Context, sourceKey string, scheduledAt time.Time, reason string) (bool, error) {
	if s == nil || s.catalog == nil {
		return false, ErrNoDatasourceConfigured
	}
	if err := s.ensureStateWritesAvailable(); err != nil {
		return false, err
	}
	_, root, err := s.localDatasourceAndRoot(sourceKey)
	if err != nil {
		return false, err
	}
	nowText := formatCatalogTime(time.Now().UTC())
	result, err := s.catalog.db.ExecContext(ctx, `UPDATE local_scan_root_state
		SET content_verification_scheduled_at = ?,
			content_verification_window_started_at = NULL,
			content_verification_window_deadline_at = NULL,
			last_content_verification_at = ?,
			content_verification_status = ?,
			content_verification_skip_reason = ?,
			content_verification_processed_files = 0,
			content_verification_verified_files = 0,
			content_verification_changed_files = 0,
			content_verification_failed_files = 0,
			content_verification_read_bytes = 0,
			updated_at = ?
		WHERE source_key = ?
			AND root_key = ?
			AND content_verification_status <> ?
			AND (
				content_verification_scheduled_at IS NULL
				OR content_verification_scheduled_at < ?
			)`,
		formatCatalogTime(scheduledAt.UTC()),
		nowText,
		LocalContentVerificationStatusSkipped,
		nullableCatalogText(strings.TrimSpace(reason)),
		nowText,
		sourceKey,
		root.Key,
		LocalContentVerificationStatusRunning,
		formatCatalogTime(scheduledAt.UTC()),
	)
	if err != nil {
		return false, fmt.Errorf("skip local content verification window: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read skipped local content verification rows: %w", err)
	}
	return affected > 0, nil
}

// LocalContentVerificationRunnableSourceKeys returns admitted windows that may
// start another file. Expired windows are finalized separately so an active
// file may finish cleanly after its deadline.
func (s *Service) LocalContentVerificationRunnableSourceKeys(ctx context.Context, now time.Time) ([]string, error) {
	if s == nil || s.catalog == nil {
		return nil, ErrNoDatasourceConfigured
	}
	sourceKeys := s.LocalDatasourceSourceKeys()
	runnable := make([]string, 0, len(sourceKeys))
	nowText := formatCatalogTime(now.UTC())
	for _, sourceKey := range sourceKeys {
		_, duration, err := s.LocalDatasourceContentVerificationSchedule(sourceKey)
		if err != nil {
			return nil, err
		}
		if duration <= 0 {
			continue
		}
		_, root, err := s.localDatasourceAndRoot(sourceKey)
		if err != nil {
			return nil, err
		}
		var found int
		err = s.catalog.queryDB().QueryRowContext(ctx, `SELECT COUNT(*)
			FROM local_scan_root_state
			WHERE source_key = ?
				AND root_key = ?
				AND content_verification_status = ?
				AND content_verification_window_started_at IS NOT NULL
				AND content_verification_window_deadline_at > ?
				AND reconciliation_pending = 0`,
			sourceKey,
			root.Key,
			LocalContentVerificationStatusRunning,
			nowText,
		).Scan(&found)
		if err != nil {
			return nil, fmt.Errorf("read runnable local content verification window: %w", err)
		}
		if found > 0 {
			runnable = append(runnable, sourceKey)
		}
	}
	return runnable, nil
}

// LocalContentVerificationNextDeadline returns the earliest active daily
// deadline so the schedule loop can finalize an idle window promptly.
func (s *Service) LocalContentVerificationNextDeadline(ctx context.Context) (*time.Time, error) {
	if s == nil || s.catalog == nil {
		return nil, ErrNoDatasourceConfigured
	}
	var raw sql.NullString
	if err := s.catalog.queryDB().QueryRowContext(ctx, `SELECT MIN(content_verification_window_deadline_at)
		FROM local_scan_root_state
		WHERE content_verification_status = ?
			AND content_verification_window_deadline_at IS NOT NULL`,
		LocalContentVerificationStatusRunning,
	).Scan(&raw); err != nil {
		return nil, fmt.Errorf("read next local content verification deadline: %w", err)
	}
	return parseLocalOptionalTime(raw), nil
}

// FinalizeExpiredLocalContentVerificationWindows completes idle windows after
// their soft deadline. Callers must not invoke it while a verification file is
// still active.
func (s *Service) FinalizeExpiredLocalContentVerificationWindows(ctx context.Context, now time.Time) (int, error) {
	if s == nil || s.catalog == nil {
		return 0, ErrNoDatasourceConfigured
	}
	if err := s.ensureStateWritesAvailable(); err != nil {
		return 0, err
	}
	nowText := formatCatalogTime(now.UTC())
	result, err := s.catalog.db.ExecContext(ctx, `UPDATE local_scan_root_state
		SET last_content_verification_at = ?,
			content_verification_window_started_at = NULL,
			content_verification_window_deadline_at = NULL,
			content_verification_status = ?,
			content_verification_skip_reason = NULL,
			updated_at = ?
		WHERE content_verification_status = ?
			AND content_verification_window_deadline_at IS NOT NULL
			AND content_verification_window_deadline_at <= ?`,
		nowText,
		LocalContentVerificationStatusCompleted,
		nowText,
		LocalContentVerificationStatusRunning,
		nowText,
	)
	if err != nil {
		return 0, fmt.Errorf("finalize expired local content verification windows: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read finalized local content verification rows: %w", err)
	}
	return int(affected), nil
}

// RunLocalContentVerification verifies at most one active Location for an
// admitted datasource window. The deadline is a soft boundary: no new file
// starts after it, but a file already being hashed is allowed to finish.
func (s *Service) RunLocalContentVerification(ctx context.Context, sourceKey string) (LocalContentVerificationResult, error) {
	if s == nil || s.catalog == nil {
		return LocalContentVerificationResult{}, ErrNoDatasourceConfigured
	}
	if err := s.ensureStateWritesAvailable(); err != nil {
		return LocalContentVerificationResult{}, err
	}
	startedAt := time.Now().UTC()
	result := LocalContentVerificationResult{
		SourceKey: sourceKey,
		StartedAt: startedAt,
	}
	window, active, err := s.localContentVerificationWindow(ctx, sourceKey)
	if err != nil {
		return result, err
	}
	if !active {
		result.CompletedAt = time.Now().UTC()
		result.WindowComplete = true
		return result, nil
	}
	if !time.Now().UTC().Before(window.deadlineAt) {
		result.WindowExpired = true
		result.CompletedAt = time.Now().UTC()
		if err := s.completeLocalContentVerificationWindow(sourceKey, window, result.CompletedAt); err != nil {
			return result, err
		}
		result.WindowComplete = true
		return result, nil
	}

	outcome, err := s.verifyNextLocalContent(ctx, sourceKey, window.cutoffText)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		// A root-wide trust/readiness failure cannot be repaired by
		// retrying the same verification window. Finish this daily attempt;
		// a future configured occurrence may run after root readiness is
		// restored, while the root problem remains visible in its own status.
		if errors.Is(err, ErrLocalMediaRootNotTrusted) {
			result.FailedFiles = 1
			result.CompletedAt = time.Now().UTC()
			if progressErr := s.recordLocalContentVerificationProgress(ctx, sourceKey, window, result); progressErr != nil {
				return result, progressErr
			}
			if finishErr := s.completeLocalContentVerificationWindow(sourceKey, window, result.CompletedAt); finishErr != nil {
				return result, finishErr
			}
			result.WindowComplete = true
			return result, nil
		}
		return result, err
	}
	if outcome.found {
		result.ProcessedFiles++
		result.ReadBytes += outcome.bytes
		if outcome.verified {
			result.VerifiedFiles++
		}
		if outcome.changed {
			result.ChangedFiles++
		}
		if outcome.failed {
			result.FailedFiles++
		}
	}
	result.CompletedAt = time.Now().UTC()
	if ctxErr := ctx.Err(); ctxErr != nil {
		if outcome.found {
			finishCtx, finishCancel := context.WithTimeout(context.Background(), localPhase0FinishTimeout)
			progressErr := s.recordLocalContentVerificationProgress(finishCtx, sourceKey, window, result)
			finishCancel()
			if progressErr != nil {
				return result, errors.Join(ctxErr, progressErr)
			}
		}
		return result, ctxErr
	}
	if err := s.recordLocalContentVerificationProgress(ctx, sourceKey, window, result); err != nil {
		return result, err
	}
	if !result.CompletedAt.Before(window.deadlineAt) {
		result.WindowExpired = true
	}
	if !result.WindowExpired && outcome.found {
		_, nextFound, nextErr := s.nextLocalContentVerificationCandidate(ctx, sourceKey, window.cutoffText)
		if nextErr != nil {
			return result, nextErr
		}
		if nextFound {
			return result, nil
		}
	}
	if err := s.completeLocalContentVerificationWindow(sourceKey, window, result.CompletedAt); err != nil {
		return result, err
	}
	result.WindowComplete = true
	return result, nil
}

func (s *Service) localContentVerificationWindow(ctx context.Context, sourceKey string) (localContentVerificationWindow, bool, error) {
	_, root, err := s.localDatasourceAndRoot(sourceKey)
	if err != nil {
		return localContentVerificationWindow{}, false, err
	}
	var scheduled sql.NullString
	var windowStarted sql.NullString
	var windowDeadline sql.NullString
	var status string
	err = s.catalog.queryDB().QueryRowContext(ctx, `SELECT content_verification_scheduled_at,
			content_verification_window_started_at, content_verification_window_deadline_at,
			content_verification_status
		FROM local_scan_root_state
		WHERE source_key = ? AND root_key = ?`,
		sourceKey,
		root.Key,
	).Scan(&scheduled, &windowStarted, &windowDeadline, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return localContentVerificationWindow{}, false, nil
	}
	if err != nil {
		return localContentVerificationWindow{}, false, fmt.Errorf("read local content verification window: %w", err)
	}
	if status != LocalContentVerificationStatusRunning ||
		!scheduled.Valid ||
		!windowStarted.Valid ||
		!windowDeadline.Valid {
		return localContentVerificationWindow{}, false, nil
	}
	scheduledAt, err := time.Parse(time.RFC3339Nano, scheduled.String)
	if err != nil {
		return localContentVerificationWindow{}, false, fmt.Errorf("parse local content verification schedule: %w", err)
	}
	startedAt, err := time.Parse(time.RFC3339Nano, windowStarted.String)
	if err != nil {
		return localContentVerificationWindow{}, false, fmt.Errorf("parse local content verification start: %w", err)
	}
	deadlineAt, err := time.Parse(time.RFC3339Nano, windowDeadline.String)
	if err != nil {
		return localContentVerificationWindow{}, false, fmt.Errorf("parse local content verification deadline: %w", err)
	}
	return localContentVerificationWindow{
		scheduledAt: scheduledAt,
		startedAt:   startedAt,
		deadlineAt:  deadlineAt,
		cutoffText:  formatCatalogTime(startedAt),
	}, true, nil
}

func (s *Service) recordLocalContentVerificationProgress(ctx context.Context, sourceKey string, window localContentVerificationWindow, result LocalContentVerificationResult) error {
	_, root, err := s.localDatasourceAndRoot(sourceKey)
	if err != nil {
		return err
	}
	update, err := s.catalog.db.ExecContext(ctx, `UPDATE local_scan_root_state
		SET content_verification_processed_files = content_verification_processed_files + ?,
			content_verification_verified_files = content_verification_verified_files + ?,
			content_verification_changed_files = content_verification_changed_files + ?,
			content_verification_failed_files = content_verification_failed_files + ?,
			content_verification_read_bytes = content_verification_read_bytes + ?,
			updated_at = ?
		WHERE source_key = ?
			AND root_key = ?
			AND content_verification_status = ?
			AND content_verification_scheduled_at = ?
			AND content_verification_window_started_at = ?`,
		max(result.ProcessedFiles, 0),
		max(result.VerifiedFiles, 0),
		max(result.ChangedFiles, 0),
		max(result.FailedFiles, 0),
		max(result.ReadBytes, 0),
		formatCatalogTime(time.Now().UTC()),
		sourceKey,
		root.Key,
		LocalContentVerificationStatusRunning,
		formatCatalogTime(window.scheduledAt),
		formatCatalogTime(window.startedAt),
	)
	if err != nil {
		return fmt.Errorf("record local content verification progress: %w", err)
	}
	affected, err := update.RowsAffected()
	if err != nil {
		return fmt.Errorf("read local content verification progress rows: %w", err)
	}
	if affected == 0 {
		return ErrLocalMediaRootNotTrusted
	}
	return nil
}

func (s *Service) completeLocalContentVerificationWindow(sourceKey string, window localContentVerificationWindow, completedAt time.Time) error {
	finishCtx, finishCancel := context.WithTimeout(context.Background(), localPhase0FinishTimeout)
	defer finishCancel()
	_, root, err := s.localDatasourceAndRoot(sourceKey)
	if err != nil {
		return err
	}
	completedAtText := formatCatalogTime(completedAt)
	result, err := s.catalog.db.ExecContext(finishCtx, `UPDATE local_scan_root_state
		SET last_content_verification_at = ?,
			content_verification_window_started_at = NULL,
			content_verification_window_deadline_at = NULL,
			content_verification_status = ?,
			content_verification_skip_reason = NULL,
			updated_at = ?
		WHERE source_key = ?
			AND root_key = ?
			AND content_verification_scheduled_at = ?
			AND content_verification_window_started_at = ?`,
		completedAtText,
		LocalContentVerificationStatusCompleted,
		completedAtText,
		sourceKey,
		root.Key,
		formatCatalogTime(window.scheduledAt),
		formatCatalogTime(window.startedAt),
	)
	if err != nil {
		return fmt.Errorf("complete local content verification window: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read completed local content verification rows: %w", err)
	}
	if affected == 0 {
		return ErrLocalMediaRootNotTrusted
	}
	return nil
}

func (s *Service) verifyNextLocalContent(ctx context.Context, sourceKey string, cutoffText string) (localContentVerificationOutcome, error) {
	candidate, found, err := s.nextLocalContentVerificationCandidate(ctx, sourceKey, cutoffText)
	if err != nil || !found {
		return localContentVerificationOutcome{found: found}, err
	}
	outcome := localContentVerificationOutcome{found: true}
	trustedRoot, err := s.acquireTrustedLocalMediaRoot(ctx, sourceKey)
	if err != nil {
		return outcome, err
	}
	defer trustedRoot.Close()
	if trustedRoot.root.Key != candidate.RootKey ||
		trustedRoot.rootGeneration != candidate.RootGeneration ||
		trustedRoot.reconciliationPending {
		return outcome, ErrLocalMediaRootNotTrusted
	}

	file, info, err := openLocalRootFileFromPinnedRoot(trustedRoot.handle, candidate.RelativePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			nowText := formatCatalogTime(time.Now().UTC())
			if markErr := s.markLocalLocationMissing(
				ctx,
				trustedRoot.externalContentIdentityMappings,
				trustedRoot.externalContentIdentityScopeKey,
				candidate.ID,
				nowText,
				"content_verification_absent",
			); markErr != nil {
				return outcome, markErr
			}
			outcome.changed = true
			return outcome, nil
		}
		if recordErr := s.recordLocalContentVerificationFailure(ctx, candidate, err); recordErr != nil {
			return outcome, errors.Join(err, recordErr)
		}
		outcome.failed = true
		return outcome, nil
	}
	defer file.Close()
	location := localLocationForMetadata{
		ID:            candidate.ID,
		SourceKey:     candidate.SourceKey,
		RootKey:       candidate.RootKey,
		RelativePath:  candidate.RelativePath,
		Status:        candidate.Status,
		SizeBytes:     candidate.SizeBytes,
		MTime:         candidate.MTime,
		FastSignature: candidate.FastSignature,
		FileIdentity:  candidate.FileIdentity,
	}
	if !localLocationMatchesFileInfo(location, info) {
		if err := s.resettleLocalLocationAfterContentVerification(ctx, trustedRoot, candidate, info, "content_verification_stat_changed"); err != nil {
			return outcome, err
		}
		outcome.changed = true
		return outcome, nil
	}

	attemptCtx, attemptCancel := context.WithTimeout(context.Background(), localPhase0FinishTimeout)
	attemptedAt, err := s.recordLocalContentVerificationAttempt(attemptCtx, candidate)
	attemptCancel()
	if err != nil {
		return outcome, err
	}
	hashFile := sha1OpenFile
	if s.localContentVerificationHash != nil {
		hashFile = s.localContentVerificationHash
	}
	actualSHA1, readBytes, infoAfter, err := hashFile(ctx, file)
	outcome.bytes = readBytes
	if err != nil {
		if ctx.Err() != nil {
			finishCtx, finishCancel := context.WithTimeout(context.Background(), localPhase0FinishTimeout)
			restoreErr := s.restoreLocalContentVerificationAttempt(finishCtx, candidate, attemptedAt)
			finishCancel()
			if restoreErr != nil {
				return outcome, errors.Join(ctx.Err(), restoreErr)
			}
			return outcome, ctx.Err()
		}
		if recordErr := s.recordLocalContentVerificationFailure(ctx, candidate, err); recordErr != nil {
			return outcome, errors.Join(err, recordErr)
		}
		outcome.failed = true
		return outcome, nil
	}
	finishCtx, finishCancel := context.WithTimeout(context.Background(), localPhase0FinishTimeout)
	defer finishCancel()
	if readBytes != info.Size() || !localFileInfoUnchanged(info, infoAfter) {
		if err := s.resettleLocalLocationAfterContentVerification(finishCtx, trustedRoot, candidate, infoAfter, "content_verification_source_changed"); err != nil {
			return outcome, err
		}
		outcome.changed = true
		return outcome, nil
	}
	pathFile, pathInfo, pathnameMatches, err := reopenPinnedLocalRootFileAndMatch(trustedRoot.handle, candidate.RelativePath, infoAfter)
	if err != nil {
		if recordErr := s.recordLocalContentVerificationFailure(finishCtx, candidate, err); recordErr != nil {
			return outcome, errors.Join(err, recordErr)
		}
		outcome.failed = true
		return outcome, nil
	}
	_ = pathFile.Close()
	if !pathnameMatches {
		if err := s.resettleLocalLocationAfterContentVerification(finishCtx, trustedRoot, candidate, pathInfo, "content_verification_path_replaced"); err != nil {
			return outcome, err
		}
		outcome.changed = true
		return outcome, nil
	}
	if !strings.EqualFold(actualSHA1, candidate.SHA1Hex) {
		if err := s.resettleLocalLocationAfterContentVerification(finishCtx, trustedRoot, candidate, infoAfter, "content_hash_changed"); err != nil {
			return outcome, err
		}
		outcome.changed = true
		return outcome, nil
	}
	updated, err := s.recordLocalContentVerificationSuccess(finishCtx, candidate)
	if err != nil {
		return outcome, err
	}
	outcome.verified = updated
	return outcome, nil
}

func (s *Service) nextLocalContentVerificationCandidate(ctx context.Context, sourceKey string, cutoffText string) (localContentVerificationCandidate, bool, error) {
	retryBefore := formatCatalogTime(time.Now().UTC().Add(-localContentVerificationFailureBackoff))
	var candidate localContentVerificationCandidate
	err := s.catalog.queryDB().QueryRowContext(ctx, `SELECT l.id, l.source_key, l.root_key,
			rs.root_generation, l.relative_path, l.status, l.size_bytes, l.mtime,
			l.fast_signature, l.file_identity, l.sha1_hex,
			l.content_verification_attempted_at, l.content_verification_error
		FROM local_asset_locations l INDEXED BY idx_local_asset_locations_content_verification
		JOIN local_scan_root_state rs
			ON rs.source_key = l.source_key
			AND rs.root_key = l.root_key
		WHERE l.source_key = ?
			AND l.status = 'active'
			AND l.asset_id IS NOT NULL
			AND l.sha1_hex IS NOT NULL
			AND COALESCE(l.content_verified_at, '') < ?
			AND COALESCE(l.content_verification_attempted_at, '') < ?
			AND (
				l.content_verification_error IS NULL
				OR l.content_verification_attempted_at IS NULL
				OR l.content_verification_attempted_at <= ?
			)
			AND rs.reconciliation_pending = 0
		ORDER BY l.content_verification_attempted_at ASC, l.id ASC
		LIMIT 1`,
		sourceKey,
		cutoffText,
		cutoffText,
		retryBefore,
	).Scan(
		&candidate.ID,
		&candidate.SourceKey,
		&candidate.RootKey,
		&candidate.RootGeneration,
		&candidate.RelativePath,
		&candidate.Status,
		&candidate.SizeBytes,
		&candidate.MTime,
		&candidate.FastSignature,
		&candidate.FileIdentity,
		&candidate.SHA1Hex,
		&candidate.AttemptedAt,
		&candidate.VerificationError,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return localContentVerificationCandidate{}, false, nil
	}
	if err != nil {
		return localContentVerificationCandidate{}, false, fmt.Errorf("select local content verification candidate: %w", err)
	}
	return candidate, true, nil
}

func (s *Service) recordLocalContentVerificationAttempt(ctx context.Context, candidate localContentVerificationCandidate) (string, error) {
	nowText := formatCatalogTime(time.Now().UTC())
	result, err := s.catalog.db.ExecContext(ctx, `UPDATE local_asset_locations
		SET content_verification_attempted_at = ?,
			content_verification_error = NULL
		WHERE id = ?
			AND source_key = ?
			AND root_key = ?
			AND status = 'active'
			AND sha1_hex = ?
			AND size_bytes = ?
			AND mtime = ?
			AND fast_signature = ?
			AND file_identity = ?
			AND EXISTS (
				SELECT 1
				FROM local_scan_root_state rs
				WHERE rs.source_key = local_asset_locations.source_key
					AND rs.root_key = local_asset_locations.root_key
					AND rs.root_generation = ?
					AND rs.reconciliation_pending = 0
			)`,
		nowText,
		candidate.ID,
		candidate.SourceKey,
		candidate.RootKey,
		candidate.SHA1Hex,
		candidate.SizeBytes,
		candidate.MTime,
		candidate.FastSignature,
		candidate.FileIdentity,
		candidate.RootGeneration,
	)
	if err != nil {
		return "", fmt.Errorf("record local content verification attempt: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("read local content verification attempt rows: %w", err)
	}
	if affected == 0 {
		return "", ErrLocalMediaRootNotTrusted
	}
	return nowText, nil
}

func (s *Service) restoreLocalContentVerificationAttempt(ctx context.Context, candidate localContentVerificationCandidate, attemptedAt string) error {
	var previousAttemptedAt any
	if candidate.AttemptedAt.Valid {
		previousAttemptedAt = candidate.AttemptedAt.String
	}
	var previousError any
	if candidate.VerificationError.Valid {
		previousError = candidate.VerificationError.String
	}
	_, err := s.catalog.db.ExecContext(ctx, `UPDATE local_asset_locations
		SET content_verification_attempted_at = ?,
			content_verification_error = ?
		WHERE id = ?
			AND source_key = ?
			AND root_key = ?
			AND status = 'active'
			AND sha1_hex = ?
			AND content_verification_attempted_at = ?`,
		previousAttemptedAt,
		previousError,
		candidate.ID,
		candidate.SourceKey,
		candidate.RootKey,
		candidate.SHA1Hex,
		attemptedAt,
	)
	if err != nil {
		return fmt.Errorf("restore canceled local content verification attempt: %w", err)
	}
	return nil
}

func (s *Service) recordLocalContentVerificationSuccess(ctx context.Context, candidate localContentVerificationCandidate) (bool, error) {
	nowText := formatCatalogTime(time.Now().UTC())
	result, err := s.catalog.db.ExecContext(ctx, `UPDATE local_asset_locations
		SET content_verified_at = ?,
			content_verification_attempted_at = ?,
			content_verification_error = NULL
		WHERE id = ?
			AND source_key = ?
			AND root_key = ?
			AND status = 'active'
			AND sha1_hex = ?
			AND size_bytes = ?
			AND mtime = ?
			AND fast_signature = ?
			AND file_identity = ?
			AND EXISTS (
				SELECT 1
				FROM local_scan_root_state rs
				WHERE rs.source_key = local_asset_locations.source_key
					AND rs.root_key = local_asset_locations.root_key
					AND rs.root_generation = ?
					AND rs.reconciliation_pending = 0
			)`,
		nowText,
		nowText,
		candidate.ID,
		candidate.SourceKey,
		candidate.RootKey,
		candidate.SHA1Hex,
		candidate.SizeBytes,
		candidate.MTime,
		candidate.FastSignature,
		candidate.FileIdentity,
		candidate.RootGeneration,
	)
	if err != nil {
		return false, fmt.Errorf("record local content verification success: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read local content verification success rows: %w", err)
	}
	return affected > 0, nil
}

func (s *Service) recordLocalContentVerificationFailure(ctx context.Context, candidate localContentVerificationCandidate, verificationErr error) error {
	nowText := formatCatalogTime(time.Now().UTC())
	message := ""
	if verificationErr != nil {
		message = verificationErr.Error()
	}
	_, err := s.catalog.db.ExecContext(ctx, `UPDATE local_asset_locations
		SET content_verification_attempted_at = ?,
			content_verification_error = ?
		WHERE id = ?
			AND source_key = ?
			AND root_key = ?
			AND status = 'active'
			AND sha1_hex = ?`,
		nowText,
		nullableCatalogText(message),
		candidate.ID,
		candidate.SourceKey,
		candidate.RootKey,
		candidate.SHA1Hex,
	)
	if err != nil {
		return fmt.Errorf("record local content verification failure: %w", err)
	}
	return nil
}

func (s *Service) resettleLocalLocationAfterContentVerification(ctx context.Context, trustedRoot *trustedLocalMediaRoot, candidate localContentVerificationCandidate, info os.FileInfo, reason string) error {
	if info == nil || !info.Mode().IsRegular() {
		return fmt.Errorf("resettle content verification source: path is not a regular file")
	}
	if trustedRoot == nil || trustedRoot.datasource.SourceKey != candidate.SourceKey || trustedRoot.root.Key != candidate.RootKey {
		return ErrLocalMediaRootNotTrusted
	}
	now := time.Now().UTC()
	nowText := formatCatalogTime(now)
	mtimeText := formatCatalogTime(info.ModTime().UTC())
	fastSignature := fmt.Sprintf("%d:%s", info.Size(), mtimeText)
	fileIdentity := localFileIdentity(info)
	notBeforeText := formatCatalogTime(now.Add(localDatasourceSettlingDuration(trustedRoot.datasource)))
	tx, err := s.catalog.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin local content verification resettle: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE local_asset_locations
		SET asset_id = NULL,
			size_bytes = ?,
			mtime = ?,
			fast_signature = ?,
			file_identity = ?,
			sha1_hex = NULL,
			status = 'discovered',
			status_reason = ?,
			last_seen_at = ?,
			verified_at = NULL,
			content_verified_at = NULL,
			content_verification_attempted_at = ?,
			content_verification_error = NULL,
			metadata_not_before = ?,
			superseded_at = NULL,
			updated_at = ?
		WHERE id = ?
			AND source_key = ?
			AND root_key = ?
			AND status = 'active'
			AND sha1_hex = ?
			AND EXISTS (
				SELECT 1
				FROM local_scan_root_state rs
				WHERE rs.source_key = local_asset_locations.source_key
					AND rs.root_key = local_asset_locations.root_key
					AND rs.root_generation = ?
					AND rs.reconciliation_pending = 0
			)`,
		info.Size(),
		mtimeText,
		fastSignature,
		fileIdentity,
		reason,
		nowText,
		nowText,
		notBeforeText,
		nowText,
		candidate.ID,
		candidate.SourceKey,
		candidate.RootKey,
		candidate.SHA1Hex,
		candidate.RootGeneration,
	)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("resettle local content verification source: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("read local content verification resettle rows: %w", err)
	}
	if affected == 0 {
		_ = tx.Rollback()
		return nil
	}
	scanner := localPhase0Scanner{
		service:        s,
		datasource:     trustedRoot.datasource,
		root:           trustedRoot.root,
		nowText:        nowText,
		rootGeneration: candidate.RootGeneration,
	}
	if _, err := scanner.queueMetadataJobInTx(ctx, tx, candidate.ID, mtimeText); err != nil {
		_ = tx.Rollback()
		return err
	}
	externalIdentityChanges, err := s.catalog.reconcileImmichExternalIdentitiesForLocalIdentityLossInTx(
		ctx,
		tx,
		trustedRoot.externalContentIdentityMappings,
		trustedRoot.externalContentIdentityScopeKey,
		candidate.SourceKey,
		candidate.RootKey,
		[]string{candidate.RelativePath},
		nowText,
	)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	changedCanonicalIDs, err := refreshLocalAssetVisibilityInTx(ctx, tx, candidate.SourceKey, candidate.RootKey, nowText)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.catalog.rebuildCatalogCanonicalIDsInTx(ctx, tx, changedCanonicalIDs, nowText); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := s.catalog.commitCatalogAssetChanges(ctx, tx, len(changedCanonicalIDs) > 0 || externalIdentityChanges > 0); err != nil {
		return fmt.Errorf("commit local content verification resettle: %w", err)
	}
	s.notifyLocalWorkQueued()
	return nil
}
