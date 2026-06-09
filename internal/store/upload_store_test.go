package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUploadStoreCreateSessionAndUpdateProgress(t *testing.T) {
	t.Parallel()

	store := newTestUploadStore(t)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	capturedAt := now.Add(-time.Hour)
	expectedSize := int64(4096)

	session, err := store.CreateSession(UploadSessionInput{
		UploadID:           "upload-1",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-1",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
		CapturedAt:         &capturedAt,
		ExpectedSizeBytes:  &expectedSize,
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/upload-1.part",
		Now:                now,
	})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if session.Status != "active" {
		t.Fatalf("Status = %q, want active", session.Status)
	}
	if session.NextOffset != 0 {
		t.Fatalf("NextOffset = %d, want 0", session.NextOffset)
	}
	if session.CapturedAt == nil || !session.CapturedAt.Equal(capturedAt) {
		t.Fatalf("CapturedAt = %v, want %v", session.CapturedAt, capturedAt)
	}

	updated, err := store.UpdateSessionProgress("upload-1", 0, 2048, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("UpdateSessionProgress() error = %v", err)
	}
	if updated.NextOffset != 2048 {
		t.Fatalf("NextOffset = %d, want 2048", updated.NextOffset)
	}
	current, err := store.UpdateSessionProgress("upload-1", 0, 1024, now.Add(2*time.Minute))
	if !errors.Is(err, ErrUploadSessionOffsetConflict) {
		t.Fatalf("UpdateSessionProgress() stale error = %v, want %v", err, ErrUploadSessionOffsetConflict)
	}
	if current.NextOffset != 2048 {
		t.Fatalf("current NextOffset = %d, want 2048 after stale retry", current.NextOffset)
	}
	updated, err = store.UpdateSessionProgress("upload-1", 2048, 4096, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("UpdateSessionProgress() second advance error = %v", err)
	}
	if updated.NextOffset != 4096 {
		t.Fatalf("NextOffset = %d, want 4096", updated.NextOffset)
	}

	reloaded, ok, err := store.GetSession("upload-1")
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if !ok {
		t.Fatal("GetSession() ok = false, want true")
	}
	if reloaded.NextOffset != 4096 {
		t.Fatalf("reloaded NextOffset = %d, want 4096", reloaded.NextOffset)
	}
}

func TestUploadStoreFindsActiveSessions(t *testing.T) {
	t.Parallel()

	store := newTestUploadStore(t)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateSession(UploadSessionInput{
		UploadID:           "upload-active",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-1",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/upload-active.part",
		Now:                now,
	}); err != nil {
		t.Fatalf("CreateSession(active) error = %v", err)
	}
	if _, err := store.CreateSession(UploadSessionInput{
		UploadID:           "upload-other",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-2",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0002.HEIC",
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/upload-other.part",
		Now:                now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("CreateSession(other) error = %v", err)
	}

	session, ok, err := store.GetActiveSessionBySourceIdentity("device-1", "asset-1", "version-1")
	if err != nil {
		t.Fatalf("GetActiveSessionBySourceIdentity() error = %v", err)
	}
	if !ok || session.UploadID != "upload-active" {
		t.Fatalf("active session = %+v ok=%v, want upload-active", session, ok)
	}
	latest, ok, err := store.GetLatestActiveSession("device-1")
	if err != nil {
		t.Fatalf("GetLatestActiveSession() error = %v", err)
	}
	if !ok || latest.UploadID != "upload-other" {
		t.Fatalf("latest session = %+v ok=%v, want upload-other", latest, ok)
	}
}

func TestUploadStoreReserveCommitAndMarkUploaded(t *testing.T) {
	t.Parallel()

	store := newTestUploadStore(t)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	expectedSize := int64(8192)
	if _, err := store.CreateSession(UploadSessionInput{
		UploadID:           "upload-1",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-1",
		SourceAssetVersion: "version-1",
		MediaType:          "video",
		OriginalFilename:   "IMG_0001.MOV",
		ExpectedSizeBytes:  &expectedSize,
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/upload-1.part",
		Now:                now,
	}); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	asset, err := store.ReserveUploadCommit(UploadCommitInput{
		UploadID:           "upload-1",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-1",
		SourceAssetVersion: "version-1",
		MediaType:          "video",
		OriginalFilename:   "IMG_0001.MOV",
		ExpectedSizeBytes:  &expectedSize,
		FinalRelativePath:  "Test iPhone/2026-05-31/IMG_0001.MOV",
		Checksums: []UploadChecksum{
			{Algorithm: " SHA1 ", Encoding: " base64 ", Digest: "abc123"},
			{Algorithm: "blake3", Digest: "def456"},
		},
		Now: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ReserveUploadCommit() error = %v", err)
	}
	if asset.Status != "committing" {
		t.Fatalf("Status = %q, want committing", asset.Status)
	}
	if asset.SelectedRootKey != "nas-photos" {
		t.Fatalf("SelectedRootKey = %q, want nas-photos", asset.SelectedRootKey)
	}
	if len(asset.Checksums) != 2 {
		t.Fatalf("Checksums length = %d, want 2", len(asset.Checksums))
	}
	if asset.Checksums[0].Algorithm != "blake3" || asset.Checksums[1].Algorithm != "sha1" {
		t.Fatalf("Checksums = %+v, want normalized sorted algorithms", asset.Checksums)
	}
	reservedAgain, err := store.ReserveUploadCommit(UploadCommitInput{
		UploadID:           "upload-1",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-1",
		SourceAssetVersion: "version-1",
		MediaType:          "video",
		OriginalFilename:   "IMG_0001.MOV",
		ExpectedSizeBytes:  &expectedSize,
		FinalRelativePath:  "Test iPhone/2026-05-31/IMG_0001.MOV",
		Now:                now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ReserveUploadCommit() idempotent retry error = %v", err)
	}
	if reservedAgain.ID != asset.ID {
		t.Fatalf("reservedAgain ID = %d, want %d", reservedAgain.ID, asset.ID)
	}
	reservedWithSuffix, err := store.ReserveUploadCommit(UploadCommitInput{
		UploadID:           "upload-1",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-1",
		SourceAssetVersion: "version-1",
		MediaType:          "video",
		OriginalFilename:   "IMG_0001.MOV",
		ExpectedSizeBytes:  &expectedSize,
		FinalRelativePath:  "Test iPhone/2026-05-31/IMG_0001-2.MOV",
		Checksums: []UploadChecksum{
			{Algorithm: "sha1", Encoding: "base64", Digest: "updated-sha1"},
		},
		Now: now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("ReserveUploadCommit() suffixed retry error = %v", err)
	}
	if reservedWithSuffix.ID != asset.ID {
		t.Fatalf("reservedWithSuffix ID = %d, want %d", reservedWithSuffix.ID, asset.ID)
	}
	if reservedWithSuffix.FinalRelativePath != "Test iPhone/2026-05-31/IMG_0001-2.MOV" {
		t.Fatalf("FinalRelativePath = %q, want suffixed path", reservedWithSuffix.FinalRelativePath)
	}
	if len(reservedWithSuffix.Checksums) != 2 || reservedWithSuffix.Checksums[1].Digest != "updated-sha1" {
		t.Fatalf("Checksums after retry = %+v, want updated sha1 and retained blake3", reservedWithSuffix.Checksums)
	}

	session, ok, err := store.GetSession("upload-1")
	if err != nil {
		t.Fatalf("GetSession() error = %v", err)
	}
	if !ok || session.Status != "committing" {
		t.Fatalf("session = %+v ok=%v, want committing", session, ok)
	}
	if session.FinalRelativePath != "Test iPhone/2026-05-31/IMG_0001-2.MOV" {
		t.Fatalf("session FinalRelativePath = %q, want suffixed path", session.FinalRelativePath)
	}

	uploaded, err := store.MarkUploaded(asset.ID, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("MarkUploaded() error = %v", err)
	}
	if uploaded.Status != "uploaded" {
		t.Fatalf("Status = %q, want uploaded", uploaded.Status)
	}
	if uploaded.UploadedAt == nil {
		t.Fatal("UploadedAt = nil, want timestamp")
	}
	if uploaded.SelectedRootKey != "nas-photos" || uploaded.FinalRelativePath != "Test iPhone/2026-05-31/IMG_0001-2.MOV" {
		t.Fatalf("uploaded root/path = %q/%q, want nas-photos suffixed path", uploaded.SelectedRootKey, uploaded.FinalRelativePath)
	}
	latestUploadedAt, err := store.LatestUploadedAt("device-1")
	if err != nil {
		t.Fatalf("LatestUploadedAt() error = %v", err)
	}
	if latestUploadedAt == nil || !latestUploadedAt.Equal(now.Add(3*time.Minute)) {
		t.Fatalf("LatestUploadedAt() = %v, want mark-upload timestamp", latestUploadedAt)
	}
	session, ok, err = store.GetSession("upload-1")
	if err != nil {
		t.Fatalf("GetSession() after upload error = %v", err)
	}
	if !ok || session.Status != "completed" {
		t.Fatalf("session = %+v ok=%v, want completed", session, ok)
	}
	byUploadID, ok, err := store.GetUploadedAssetByUploadID("upload-1")
	if err != nil {
		t.Fatalf("GetUploadedAssetByUploadID() error = %v", err)
	}
	if !ok || byUploadID.ID != uploaded.ID || byUploadID.Status != "uploaded" {
		t.Fatalf("GetUploadedAssetByUploadID() = %+v ok=%v, want uploaded asset", byUploadID, ok)
	}
}

func TestUploadStoreReserveCommitRejectsDuplicateSourceIdentity(t *testing.T) {
	t.Parallel()

	store := newTestUploadStore(t)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	input := UploadCommitInput{
		UploadID:           "upload-1",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-1",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
		FinalRelativePath:  "Test iPhone/2026-05-31/IMG_0001.HEIC",
		Checksums: []UploadChecksum{
			{Algorithm: "sha1", Digest: "existing-sha1"},
		},
		Now: now,
	}
	if _, err := store.CreateSession(UploadSessionInput{
		UploadID:           input.UploadID,
		DeviceID:           input.DeviceID,
		SourceAssetID:      input.SourceAssetID,
		SourceAssetVersion: input.SourceAssetVersion,
		MediaType:          input.MediaType,
		OriginalFilename:   input.OriginalFilename,
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/upload-1.part",
		Now:                now,
	}); err != nil {
		t.Fatalf("CreateSession() first error = %v", err)
	}
	first, err := store.ReserveUploadCommit(input)
	if err != nil {
		t.Fatalf("ReserveUploadCommit() first error = %v", err)
	}
	input.UploadID = "upload-2"
	input.FinalRelativePath = "Test iPhone/2026-05-31/IMG_0001-2.HEIC"
	if _, err := store.CreateSession(UploadSessionInput{
		UploadID:           input.UploadID,
		DeviceID:           input.DeviceID,
		SourceAssetID:      input.SourceAssetID,
		SourceAssetVersion: input.SourceAssetVersion,
		MediaType:          input.MediaType,
		OriginalFilename:   input.OriginalFilename,
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/upload-2.part",
		Now:                now,
	}); err != nil {
		t.Fatalf("CreateSession() second error = %v", err)
	}
	existing, err := store.ReserveUploadCommit(input)
	if !errors.Is(err, ErrUploadedAssetExists) {
		t.Fatalf("ReserveUploadCommit() second error = %v, want %v", err, ErrUploadedAssetExists)
	}
	if existing.ID != first.ID {
		t.Fatalf("existing ID = %d, want first ID %d", existing.ID, first.ID)
	}
	if len(existing.Checksums) != 1 || existing.Checksums[0].Digest != "existing-sha1" {
		t.Fatalf("existing Checksums = %+v, want checksum from first asset", existing.Checksums)
	}
}

func TestUploadStoreAbortSession(t *testing.T) {
	t.Parallel()

	store := newTestUploadStore(t)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateSession(UploadSessionInput{
		UploadID:           "upload-abort",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-abort",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/upload-abort.part",
		Now:                now,
	}); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	aborted, err := store.AbortSession(" upload-abort ", " device-1 ", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("AbortSession() error = %v", err)
	}
	if aborted.Status != "aborted" {
		t.Fatalf("Status = %q, want aborted", aborted.Status)
	}
	if _, err := store.AbortSession("upload-abort", "device-1", now.Add(2*time.Minute)); !errors.Is(err, ErrUploadSessionNotFound) {
		t.Fatalf("AbortSession(second) error = %v, want not found", err)
	}
}

func TestUploadStoreReserveCommitRequiresSession(t *testing.T) {
	t.Parallel()

	store := newTestUploadStore(t)
	_, err := store.ReserveUploadCommit(UploadCommitInput{
		UploadID:           "upload-1",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-1",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "IMG_0001.HEIC",
		FinalRelativePath:  "Test iPhone/2026-05-31/IMG_0001.HEIC",
		Now:                time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC),
	})
	if !errors.Is(err, ErrUploadSessionNotFound) {
		t.Fatalf("ReserveUploadCommit() error = %v, want %v", err, ErrUploadSessionNotFound)
	}
}

func TestUploadStoreResetDeviceUploadState(t *testing.T) {
	t.Parallel()

	store := newTestUploadStore(t)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	insideCapturedAt := now
	outsideCapturedAt := now.Add(-48 * time.Hour)
	createCommittedTestAsset(t, store, "upload-1", "asset-1", insideCapturedAt, now)
	createCommittedTestAsset(t, store, "upload-2", "asset-2", outsideCapturedAt, now)
	after := now.Add(-time.Hour)
	before := now.Add(time.Hour)

	result, err := store.ResetDeviceUploadState(UploadResetInput{
		DeviceID:       "device-1",
		CapturedAfter:  &after,
		CapturedBefore: &before,
		Reason:         "test reset",
		Now:            now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ResetDeviceUploadState() error = %v", err)
	}
	if result.RemovedUploadedAssets != 1 || result.RemovedSessions != 1 {
		t.Fatalf("removed assets/sessions = %d/%d, want 1/1", result.RemovedUploadedAssets, result.RemovedSessions)
	}
	if len(result.TempFiles) != 1 || result.TempFiles[0].RelativePath != ".timich-upload-tmp/upload-1.part" {
		t.Fatalf("TempFiles = %+v, want upload-1 temp file", result.TempFiles)
	}
	if _, ok, err := store.GetUploadedAssetBySourceIdentity("device-1", "asset-1", "version-1"); err != nil || ok {
		t.Fatalf("inside asset ok=%v err=%v, want removed", ok, err)
	}
	if _, ok, err := store.GetUploadedAssetBySourceIdentity("device-1", "asset-2", "version-1"); err != nil || !ok {
		t.Fatalf("outside asset ok=%v err=%v, want retained", ok, err)
	}
	if _, ok, err := store.GetSession("upload-1"); err != nil || ok {
		t.Fatalf("inside session ok=%v err=%v, want removed", ok, err)
	}
	if _, ok, err := store.GetSession("upload-2"); err != nil || !ok {
		t.Fatalf("outside session ok=%v err=%v, want retained", ok, err)
	}
}

func TestUploadStoreCleanupUploadStatePrunesAuxiliaryRows(t *testing.T) {
	t.Parallel()

	store := newTestUploadStore(t)
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-10 * 24 * time.Hour)
	recent := now.Add(-2 * 24 * time.Hour)
	expiredAt := now.Add(-time.Hour)
	futureExpiresAt := now.Add(time.Hour)

	createUploadedTestAsset(t, store, "upload-completed-old", "asset-completed-old", old, old)
	createUploadedTestAsset(t, store, "upload-completed-recent", "asset-completed-recent", recent, recent)
	if _, err := store.CreateSession(UploadSessionInput{
		UploadID:           "upload-expired-active",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-expired-active",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "expired-active.HEIC",
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/upload-expired-active.part",
		ExpiresAt:          &expiredAt,
		Now:                old,
	}); err != nil {
		t.Fatalf("CreateSession(expired active) error = %v", err)
	}
	if _, err := store.CreateSession(UploadSessionInput{
		UploadID:           "upload-active",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-active",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "active.HEIC",
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/upload-active.part",
		ExpiresAt:          &futureExpiresAt,
		Now:                recent,
	}); err != nil {
		t.Fatalf("CreateSession(active) error = %v", err)
	}
	if _, err := store.CreateSession(UploadSessionInput{
		UploadID:           "upload-aborted-old",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-aborted-old",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "aborted.HEIC",
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/upload-aborted-old.part",
		Now:                old,
	}); err != nil {
		t.Fatalf("CreateSession(aborted) error = %v", err)
	}
	if _, err := store.AbortSession("upload-aborted-old", "device-1", old.Add(time.Minute)); err != nil {
		t.Fatalf("AbortSession() error = %v", err)
	}
	if _, err := store.CreateSession(UploadSessionInput{
		UploadID:           "upload-failed-old",
		DeviceID:           "device-1",
		SourceAssetID:      "asset-failed-old",
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   "failed.HEIC",
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/upload-failed-old.part",
		Now:                old,
	}); err != nil {
		t.Fatalf("CreateSession(failed) error = %v", err)
	}
	if _, err := store.db.Exec(
		`UPDATE upload_sessions SET status = 'failed', updated_at = ? WHERE upload_id = 'upload-failed-old'`,
		formatDBTime(old.Add(time.Minute)),
	); err != nil {
		t.Fatalf("mark failed session: %v", err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO upload_reset_events (device_id, captured_after, captured_before, reason, created_at)
			VALUES ('device-1', NULL, NULL, 'old reset', ?)`,
		formatDBTime(now.Add(-120*24*time.Hour)),
	); err != nil {
		t.Fatalf("insert old reset event: %v", err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO upload_reset_events (device_id, captured_after, captured_before, reason, created_at)
			VALUES ('device-1', NULL, NULL, 'recent reset', ?)`,
		formatDBTime(now.Add(-10*24*time.Hour)),
	); err != nil {
		t.Fatalf("insert recent reset event: %v", err)
	}

	result, err := store.CleanupUploadState(UploadCleanupInput{
		Now:                       now,
		CompletedSessionRetention: 7 * 24 * time.Hour,
		AbortedSessionRetention:   7 * 24 * time.Hour,
		FailedSessionRetention:    7 * 24 * time.Hour,
		CommitEventRetention:      7 * 24 * time.Hour,
		ResetEventRetention:       90 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("CleanupUploadState() error = %v", err)
	}
	if result.RemovedExpiredSessions != 1 || result.RemovedCompleted != 1 || result.RemovedAborted != 1 || result.RemovedFailed != 1 {
		t.Fatalf("removed sessions = expired:%d completed:%d aborted:%d failed:%d, want 1 each",
			result.RemovedExpiredSessions,
			result.RemovedCompleted,
			result.RemovedAborted,
			result.RemovedFailed,
		)
	}
	if result.RemovedSessions() != 4 {
		t.Fatalf("RemovedSessions() = %d, want 4", result.RemovedSessions())
	}
	if result.RemovedCommitEvents == 0 {
		t.Fatal("RemovedCommitEvents = 0, want old completed commit events removed")
	}
	if result.RemovedResetEvents != 1 {
		t.Fatalf("RemovedResetEvents = %d, want 1", result.RemovedResetEvents)
	}
	if !result.WALCheckpointed {
		t.Fatal("WALCheckpointed = false, want true")
	}
	if len(result.TempFiles) != 4 {
		t.Fatalf("TempFiles length = %d, want 4: %+v", len(result.TempFiles), result.TempFiles)
	}
	if countRows(t, store, "upload_sessions") != 2 {
		t.Fatalf("remaining upload_sessions = %d, want 2", countRows(t, store, "upload_sessions"))
	}
	if countRows(t, store, "uploaded_assets") != 2 {
		t.Fatalf("uploaded_assets = %d, want once-only rows retained", countRows(t, store, "uploaded_assets"))
	}
	if countRows(t, store, "uploaded_asset_checksums") != 2 {
		t.Fatalf("uploaded_asset_checksums = %d, want checksums retained", countRows(t, store, "uploaded_asset_checksums"))
	}
	if _, ok, err := store.GetUploadedAssetBySourceIdentity("device-1", "asset-completed-old", "version-1"); err != nil || !ok {
		t.Fatalf("old completed asset ok=%v err=%v, want retained once-only row", ok, err)
	}
	if _, ok, err := store.GetSession("upload-completed-recent"); err != nil || !ok {
		t.Fatalf("recent completed session ok=%v err=%v, want retained", ok, err)
	}
	if _, ok, err := store.GetSession("upload-active"); err != nil || !ok {
		t.Fatalf("active session ok=%v err=%v, want retained", ok, err)
	}
	if countRows(t, store, "upload_reset_events") != 1 {
		t.Fatalf("upload_reset_events = %d, want recent reset retained", countRows(t, store, "upload_reset_events"))
	}
}

func TestUploadStoreLoadOrCreatePersistsDatabase(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	first, err := LoadOrCreateUploadStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateUploadStore() first error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, uploadStoreFileName)); err != nil {
		t.Fatalf("Stat(upload store) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() first error = %v", err)
	}

	second, err := LoadOrCreateUploadStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateUploadStore() second error = %v", err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Fatalf("Close() second error = %v", err)
		}
	}()
}

func TestDBTimeFormatSortsLexicographically(t *testing.T) {
	t.Parallel()

	wholeSecond := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	oneNanosecondLater := wholeSecond.Add(time.Nanosecond)
	wholeFormatted := formatDBTime(wholeSecond)
	laterFormatted := formatDBTime(oneNanosecondLater)
	if !(wholeFormatted < laterFormatted) {
		t.Fatalf("formatDBTime order = %q then %q, want lexicographic chronological order", wholeFormatted, laterFormatted)
	}
	if len(wholeFormatted) != len(laterFormatted) {
		t.Fatalf("format lengths = %d/%d, want fixed width", len(wholeFormatted), len(laterFormatted))
	}
}

func newTestUploadStore(t *testing.T) *UploadStore {
	t.Helper()

	store, err := LoadOrCreateUploadStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateUploadStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return store
}

func createCommittedTestAsset(t *testing.T, store *UploadStore, uploadID string, assetID string, capturedAt time.Time, now time.Time) {
	t.Helper()

	if _, err := store.CreateSession(UploadSessionInput{
		UploadID:           uploadID,
		DeviceID:           "device-1",
		SourceAssetID:      assetID,
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   assetID + ".HEIC",
		CapturedAt:         &capturedAt,
		SelectedRootKey:    "nas-photos",
		TempRelativePath:   ".timich-upload-tmp/" + uploadID + ".part",
		Now:                now,
	}); err != nil {
		t.Fatalf("CreateSession(%s) error = %v", uploadID, err)
	}
	if _, err := store.ReserveUploadCommit(UploadCommitInput{
		UploadID:           uploadID,
		DeviceID:           "device-1",
		SourceAssetID:      assetID,
		SourceAssetVersion: "version-1",
		MediaType:          "image",
		OriginalFilename:   assetID + ".HEIC",
		CapturedAt:         &capturedAt,
		FinalRelativePath:  "Test iPhone/2026-05-31/" + assetID + ".HEIC",
		Now:                now.Add(time.Second),
	}); err != nil {
		t.Fatalf("ReserveUploadCommit(%s) error = %v", uploadID, err)
	}
}

func createUploadedTestAsset(t *testing.T, store *UploadStore, uploadID string, assetID string, capturedAt time.Time, now time.Time) {
	t.Helper()

	createCommittedTestAsset(t, store, uploadID, assetID, capturedAt, now)
	asset, ok, err := store.GetUploadedAssetByUploadID(uploadID)
	if err != nil {
		t.Fatalf("GetUploadedAssetByUploadID(%s) error = %v", uploadID, err)
	}
	if !ok {
		t.Fatalf("GetUploadedAssetByUploadID(%s) ok=false", uploadID)
	}
	if _, err := store.MarkUploaded(asset.ID, now.Add(2*time.Second)); err != nil {
		t.Fatalf("MarkUploaded(%s) error = %v", uploadID, err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO uploaded_asset_checksums (asset_id, algorithm, encoding, digest, created_at)
			VALUES (?, 'sha1', 'hex', ?, ?)
			ON CONFLICT(asset_id, algorithm) DO UPDATE SET digest = excluded.digest`,
		asset.ID,
		assetID+"-sha1",
		formatDBTime(now.Add(time.Second)),
	); err != nil {
		t.Fatalf("insert checksum(%s): %v", uploadID, err)
	}
}

func countRows(t *testing.T, store *UploadStore, tableName string) int64 {
	t.Helper()

	var count int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM ` + tableName).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", tableName, err)
	}
	return count
}
