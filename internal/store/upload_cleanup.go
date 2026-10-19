package store

import (
	"database/sql"
	"errors"
	"strings"
)

// UploadCleanupCandidate is an identity to lock before rechecking eligibility.
type UploadCleanupCandidate struct {
	UploadID string
	DeviceID string
}

func uploadCleanupPredicate(input UploadCleanupInput) (string, []any) {
	input = normalizeUploadCleanupInput(input)
	return `((status = 'active' AND expires_at IS NOT NULL AND expires_at <= ?)
		OR (status = 'completed' AND updated_at < ?)
		OR (status = 'aborted' AND updated_at < ?)
		OR (status = 'failed' AND updated_at < ?))`, []any{
			formatDBTime(input.Now),
			formatDBTime(input.Now.Add(-input.CompletedSessionRetention)),
			formatDBTime(input.Now.Add(-input.AbortedSessionRetention)),
			formatDBTime(input.Now.Add(-input.FailedSessionRetention)),
		}
}

// UploadCleanupCandidates takes a snapshot; it does not authorize deletion.
func (s *UploadStore) UploadCleanupCandidates(input UploadCleanupInput) ([]UploadCleanupCandidate, error) {
	where, args := uploadCleanupPredicate(input)
	rows, err := s.db.Query(`SELECT upload_id, device_id FROM upload_sessions WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []UploadCleanupCandidate
	for rows.Next() {
		var candidate UploadCleanupCandidate
		if err := rows.Scan(&candidate.UploadID, &candidate.DeviceID); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// CleanupUploadSession rechecks status and retention in the deletion transaction.
// The caller must hold the device gate and session lock through temp-file cleanup.
func (s *UploadStore) CleanupUploadSession(uploadID string, input UploadCleanupInput) (UploadCleanupResult, error) {
	input = normalizeUploadCleanupInput(input)
	result := UploadCleanupResult{CleanedAt: input.Now}
	where, args := uploadCleanupPredicate(input)
	args = append([]any{strings.TrimSpace(uploadID)}, args...)
	tx, err := s.db.Begin()
	if err != nil {
		return UploadCleanupResult{}, err
	}
	defer tx.Rollback()
	session, err := scanUploadSession(tx.QueryRow(uploadSessionSelectSQL()+` WHERE upload_id = ? AND `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return UploadCleanupResult{}, err
	}
	if _, err := tx.Exec(`DELETE FROM upload_sessions WHERE upload_id = ?`, session.UploadID); err != nil {
		return UploadCleanupResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return UploadCleanupResult{}, err
	}
	switch session.Status {
	case "active":
		result.RemovedExpiredSessions = 1
	case "completed":
		result.RemovedCompleted = 1
	case "aborted":
		result.RemovedAborted = 1
	case "failed":
		result.RemovedFailed = 1
	}
	if session.TempRelativePath != "" {
		result.TempFiles = []UploadTempFile{{RootKey: session.SelectedRootKey, RelativePath: session.TempRelativePath}}
	}
	return result, nil
}
