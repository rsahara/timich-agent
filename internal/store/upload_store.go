package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	uploadStoreFileName = "uploads.db"
	dbTimeLayout        = "2006-01-02T15:04:05.000000000Z"
)

var (
	ErrUploadSessionNotFound       = errors.New("upload session not found")
	ErrUploadSessionOffsetConflict = errors.New("upload session offset conflict")
	ErrUploadedAssetNotFound       = errors.New("uploaded asset not found")
	ErrUploadedAssetExists         = errors.New("uploaded asset already exists")
)

// UploadStore owns durable upload metadata, active sessions, and once-only
// uploaded-asset identity rows.
type UploadStore struct {
	path string
	db   *sql.DB
}

// UploadChecksum stores one content checksum for an uploaded asset.
type UploadChecksum struct {
	Algorithm string
	Encoding  string
	Digest    string
}

// UploadSessionInput describes an app upload session before binary chunks are
// accepted.
type UploadSessionInput struct {
	UploadID           string
	DeviceID           string
	SourceAssetID      string
	SourceAssetVersion string
	MediaType          string
	OriginalFilename   string
	CapturedAt         *time.Time
	ExpectedSizeBytes  *int64
	SelectedRootKey    string
	TempRelativePath   string
	ExpiresAt          *time.Time
	Now                time.Time
}

// UploadSession is the persisted resumable upload session state.
type UploadSession struct {
	UploadID           string
	DeviceID           string
	SourceAssetID      string
	SourceAssetVersion string
	Status             string
	MediaType          string
	OriginalFilename   string
	CapturedAt         *time.Time
	ExpectedSizeBytes  *int64
	NextOffset         int64
	SelectedRootKey    string
	TempRelativePath   string
	FinalRelativePath  string
	LastError          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	ExpiresAt          *time.Time
}

// UploadCommitInput reserves the once-only uploaded-asset identity during final
// file placement.
type UploadCommitInput struct {
	UploadID           string
	DeviceID           string
	SourceAssetID      string
	SourceAssetVersion string
	MediaType          string
	OriginalFilename   string
	CapturedAt         *time.Time
	ExpectedSizeBytes  *int64
	FinalRelativePath  string
	Checksums          []UploadChecksum
	Now                time.Time
}

// UploadedAsset is one canonical uploaded media metadata row.
type UploadedAsset struct {
	ID                 int64
	DeviceID           string
	SourceAssetID      string
	SourceAssetVersion string
	UploadID           string
	Status             string
	MediaType          string
	OriginalFilename   string
	CapturedAt         *time.Time
	ExpectedSizeBytes  *int64
	SelectedRootKey    string
	FinalRelativePath  string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	CommittingAt       *time.Time
	UploadedAt         *time.Time
	Checksums          []UploadChecksum
}

// UploadTempFile identifies an upload session temp file that can be removed
// after its corresponding session state is reset.
type UploadTempFile struct {
	RootKey      string
	RelativePath string
}

// UploadResetInput describes an administrator-owned reset of upload state.
type UploadResetInput struct {
	DeviceID       string
	CapturedAfter  *time.Time
	CapturedBefore *time.Time
	Reason         string
	Now            time.Time
}

// UploadResetResult reports the upload state removed from SQLite.
type UploadResetResult struct {
	DeviceID              string
	CapturedAfter         *time.Time
	CapturedBefore        *time.Time
	ResetAt               time.Time
	RemovedUploadedAssets int64
	RemovedSessions       int64
	TempFiles             []UploadTempFile
}

// UploadCleanupInput describes routine maintenance for auxiliary upload state.
// It never deletes uploaded_assets or uploaded_asset_checksums because those
// tables are the once-only upload ledger.
type UploadCleanupInput struct {
	Now                       time.Time
	CompletedSessionRetention time.Duration
	AbortedSessionRetention   time.Duration
	FailedSessionRetention    time.Duration
	CommitEventRetention      time.Duration
	ResetEventRetention       time.Duration
}

// UploadCleanupResult reports auxiliary state removed during routine
// maintenance. TempFiles identifies session temp files the runtime can remove
// after SQLite state is committed.
type UploadCleanupResult struct {
	CleanedAt              time.Time
	RemovedExpiredSessions int64
	RemovedCompleted       int64
	RemovedAborted         int64
	RemovedFailed          int64
	RemovedCommitEvents    int64
	RemovedResetEvents     int64
	TempFiles              []UploadTempFile
	WALCheckpointed        bool
}

// RemovedSessions returns the total number of upload_session rows removed.
func (r UploadCleanupResult) RemovedSessions() int64 {
	return r.RemovedExpiredSessions + r.RemovedCompleted + r.RemovedAborted + r.RemovedFailed
}

// LoadOrCreateUploadStore opens the local upload metadata database and applies
// idempotent schema migrations.
func LoadOrCreateUploadStore(dataDir string) (*UploadStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("data directory must not be empty")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dataDir, uploadStoreFileName)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open upload store: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &UploadStore{path: path, db: db}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Path returns the SQLite database path for diagnostics.
func (s *UploadStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Close releases the database handle.
func (s *UploadStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *UploadStore) migrate() error {
	statements := []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS uploaded_assets (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			device_id TEXT NOT NULL,
			source_asset_id TEXT NOT NULL,
			source_asset_version TEXT NOT NULL,
			upload_id TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('committing', 'uploaded')),
			media_type TEXT NOT NULL,
			original_filename TEXT NOT NULL,
			captured_at TEXT,
			expected_size_bytes INTEGER,
			selected_root_key TEXT NOT NULL,
			final_relative_path TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			committing_at TEXT,
			uploaded_at TEXT,
			UNIQUE(device_id, source_asset_id, source_asset_version),
			UNIQUE(upload_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_uploaded_assets_device_captured
			ON uploaded_assets(device_id, captured_at)`,
		`CREATE TABLE IF NOT EXISTS uploaded_asset_checksums (
			asset_id INTEGER NOT NULL REFERENCES uploaded_assets(id) ON DELETE CASCADE,
			algorithm TEXT NOT NULL,
			encoding TEXT NOT NULL,
			digest TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(asset_id, algorithm)
		)`,
		`CREATE TABLE IF NOT EXISTS upload_sessions (
			upload_id TEXT PRIMARY KEY,
			device_id TEXT NOT NULL,
			source_asset_id TEXT NOT NULL,
			source_asset_version TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('active', 'committing', 'completed', 'failed', 'aborted')),
			media_type TEXT NOT NULL,
			original_filename TEXT NOT NULL,
			captured_at TEXT,
			expected_size_bytes INTEGER,
			next_offset INTEGER NOT NULL DEFAULT 0,
			selected_root_key TEXT NOT NULL,
			temp_relative_path TEXT NOT NULL,
			final_relative_path TEXT,
			last_error TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			expires_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_upload_sessions_device_status
			ON upload_sessions(device_id, status, updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_upload_sessions_source_identity
			ON upload_sessions(device_id, source_asset_id, source_asset_version)`,
		`CREATE TABLE IF NOT EXISTS upload_commit_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			upload_id TEXT NOT NULL,
			asset_id INTEGER,
			event_type TEXT NOT NULL,
			message TEXT,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_upload_commit_events_upload
			ON upload_commit_events(upload_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS upload_reset_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			device_id TEXT NOT NULL,
			captured_after TEXT,
			captured_before TEXT,
			reason TEXT,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_upload_reset_events_device
			ON upload_reset_events(device_id, created_at)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate upload store: %w", err)
		}
	}
	if err := s.ensureUploadedAssetsSelectedRootKey(); err != nil {
		return err
	}
	return nil
}

func (s *UploadStore) ensureUploadedAssetsSelectedRootKey() error {
	exists, err := s.tableColumnExists("uploaded_assets", "selected_root_key")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := s.db.Exec(`ALTER TABLE uploaded_assets ADD COLUMN selected_root_key TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add uploaded asset selected root key column: %w", err)
	}
	if _, err := s.db.Exec(
		`UPDATE uploaded_assets
			SET selected_root_key = COALESCE((
				SELECT selected_root_key
				FROM upload_sessions
				WHERE upload_sessions.upload_id = uploaded_assets.upload_id
			), '')
			WHERE selected_root_key = ''`,
	); err != nil {
		return fmt.Errorf("backfill uploaded asset selected root key: %w", err)
	}
	return nil
}

func (s *UploadStore) tableColumnExists(tableName string, columnName string) (bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(` + tableName + `)`)
	if err != nil {
		return false, fmt.Errorf("inspect table %s: %w", tableName, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var columnType string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == columnName {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// CreateSession persists a new active upload session.
func (s *UploadStore) CreateSession(input UploadSessionInput) (UploadSession, error) {
	normalized, err := normalizeSessionInput(input)
	if err != nil {
		return UploadSession{}, err
	}
	_, err = s.db.Exec(
		`INSERT INTO upload_sessions (
			upload_id, device_id, source_asset_id, source_asset_version, status,
			media_type, original_filename, captured_at, expected_size_bytes,
			next_offset, selected_root_key, temp_relative_path, final_relative_path,
			last_error, created_at, updated_at, expires_at
		) VALUES (?, ?, ?, ?, 'active', ?, ?, ?, ?, 0, ?, ?, NULL, '', ?, ?, ?)`,
		normalized.UploadID,
		normalized.DeviceID,
		normalized.SourceAssetID,
		normalized.SourceAssetVersion,
		normalized.MediaType,
		normalized.OriginalFilename,
		nullableTime(normalized.CapturedAt),
		nullableInt64(normalized.ExpectedSizeBytes),
		normalized.SelectedRootKey,
		normalized.TempRelativePath,
		formatDBTime(normalized.Now),
		formatDBTime(normalized.Now),
		nullableTime(normalized.ExpiresAt),
	)
	if err != nil {
		return UploadSession{}, fmt.Errorf("create upload session: %w", err)
	}
	session, ok, err := s.GetSession(normalized.UploadID)
	if err != nil {
		return UploadSession{}, err
	}
	if !ok {
		return UploadSession{}, ErrUploadSessionNotFound
	}
	return session, nil
}

// GetSession returns one upload session by upload id.
func (s *UploadStore) GetSession(uploadID string) (UploadSession, bool, error) {
	row := s.db.QueryRow(
		uploadSessionSelectSQL()+` WHERE upload_id = ?`,
		strings.TrimSpace(uploadID),
	)
	session, err := scanUploadSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UploadSession{}, false, nil
		}
		return UploadSession{}, false, err
	}
	return session, true, nil
}

// GetActiveSessionBySourceIdentity returns the most recently updated active
// upload session for one device-local source identity.
func (s *UploadStore) GetActiveSessionBySourceIdentity(
	deviceID string,
	sourceAssetID string,
	sourceAssetVersion string,
) (UploadSession, bool, error) {
	row := s.db.QueryRow(
		uploadSessionSelectSQL()+`
			WHERE device_id = ?
				AND source_asset_id = ?
				AND source_asset_version = ?
				AND status = 'active'
			ORDER BY updated_at DESC
			LIMIT 1`,
		strings.TrimSpace(deviceID),
		strings.TrimSpace(sourceAssetID),
		strings.TrimSpace(sourceAssetVersion),
	)
	session, err := scanUploadSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UploadSession{}, false, nil
		}
		return UploadSession{}, false, err
	}
	return session, true, nil
}

// GetLatestActiveSession returns the most recently updated active upload
// session for a paired device.
func (s *UploadStore) GetLatestActiveSession(deviceID string) (UploadSession, bool, error) {
	row := s.db.QueryRow(
		uploadSessionSelectSQL()+`
			WHERE device_id = ?
				AND status = 'active'
			ORDER BY updated_at DESC
			LIMIT 1`,
		strings.TrimSpace(deviceID),
	)
	session, err := scanUploadSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UploadSession{}, false, nil
		}
		return UploadSession{}, false, err
	}
	return session, true, nil
}

// UpdateSessionProgress advances a resumable sequential upload session only
// when the caller's expected offset still matches the store's current offset.
func (s *UploadStore) UpdateSessionProgress(
	uploadID string,
	expectedOffset int64,
	nextOffset int64,
	now time.Time,
) (UploadSession, error) {
	if expectedOffset < 0 {
		return UploadSession{}, errors.New("expected offset must not be negative")
	}
	if nextOffset < 0 {
		return UploadSession{}, errors.New("next offset must not be negative")
	}
	if nextOffset < expectedOffset {
		return UploadSession{}, errors.New("next offset must not be before expected offset")
	}
	normalizedUploadID := strings.TrimSpace(uploadID)
	result, err := s.db.Exec(
		`UPDATE upload_sessions
			SET next_offset = ?, updated_at = ?
			WHERE upload_id = ? AND status = 'active' AND next_offset = ?`,
		nextOffset,
		formatDBTime(nonZeroTime(now)),
		normalizedUploadID,
		expectedOffset,
	)
	if err != nil {
		return UploadSession{}, fmt.Errorf("update upload session progress: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return UploadSession{}, err
	}
	if changed == 0 {
		session, ok, err := s.GetSession(normalizedUploadID)
		if err != nil {
			return UploadSession{}, err
		}
		if !ok || session.Status != "active" {
			return UploadSession{}, ErrUploadSessionNotFound
		}
		return session, ErrUploadSessionOffsetConflict
	}
	session, ok, err := s.GetSession(normalizedUploadID)
	if err != nil {
		return UploadSession{}, err
	}
	if !ok {
		return UploadSession{}, ErrUploadSessionNotFound
	}
	return session, nil
}

// AbortSession marks an active upload session as aborted.
func (s *UploadStore) AbortSession(uploadID string, deviceID string, now time.Time) (UploadSession, error) {
	normalizedUploadID := strings.TrimSpace(uploadID)
	normalizedDeviceID := strings.TrimSpace(deviceID)
	if normalizedUploadID == "" {
		return UploadSession{}, ErrUploadSessionNotFound
	}
	if normalizedDeviceID == "" {
		return UploadSession{}, ErrDeviceNotFound
	}
	result, err := s.db.Exec(
		`UPDATE upload_sessions
			SET status = 'aborted', updated_at = ?
			WHERE upload_id = ? AND device_id = ? AND status = 'active'`,
		formatDBTime(nonZeroTime(now)),
		normalizedUploadID,
		normalizedDeviceID,
	)
	if err != nil {
		return UploadSession{}, fmt.Errorf("abort upload session: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return UploadSession{}, err
	}
	if changed == 0 {
		return UploadSession{}, ErrUploadSessionNotFound
	}
	session, ok, err := s.GetSession(normalizedUploadID)
	if err != nil {
		return UploadSession{}, err
	}
	if !ok {
		return UploadSession{}, ErrUploadSessionNotFound
	}
	return session, nil
}

// ReserveUploadCommit acquires the once-only source identity before final file
// placement. Existing source identity rows are returned with ErrUploadedAssetExists.
func (s *UploadStore) ReserveUploadCommit(input UploadCommitInput) (UploadedAsset, error) {
	normalized, err := normalizeCommitInput(input)
	if err != nil {
		return UploadedAsset{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return UploadedAsset{}, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	now := formatDBTime(normalized.Now)
	result, err := tx.Exec(
		`UPDATE upload_sessions
			SET status = 'committing', final_relative_path = ?, updated_at = ?
			WHERE upload_id = ?
				AND device_id = ?
				AND source_asset_id = ?
				AND source_asset_version = ?
				AND status IN ('active', 'committing')`,
		normalized.FinalRelativePath,
		now,
		normalized.UploadID,
		normalized.DeviceID,
		normalized.SourceAssetID,
		normalized.SourceAssetVersion,
	)
	if err != nil {
		return UploadedAsset{}, fmt.Errorf("mark upload session committing: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return UploadedAsset{}, err
	}
	if changed == 0 {
		return UploadedAsset{}, ErrUploadSessionNotFound
	}
	selectedRootKey, err := selectedRootKeyForUploadSession(tx, normalized.UploadID)
	if err != nil {
		return UploadedAsset{}, err
	}
	result, err = tx.Exec(
		`INSERT OR IGNORE INTO uploaded_assets (
			device_id, source_asset_id, source_asset_version, upload_id, status,
			media_type, original_filename, captured_at, expected_size_bytes,
			selected_root_key, final_relative_path, created_at, updated_at, committing_at, uploaded_at
		) VALUES (?, ?, ?, ?, 'committing', ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		normalized.DeviceID,
		normalized.SourceAssetID,
		normalized.SourceAssetVersion,
		normalized.UploadID,
		normalized.MediaType,
		normalized.OriginalFilename,
		nullableTime(normalized.CapturedAt),
		nullableInt64(normalized.ExpectedSizeBytes),
		selectedRootKey,
		normalized.FinalRelativePath,
		now,
		now,
		now,
	)
	if err != nil {
		return UploadedAsset{}, fmt.Errorf("reserve uploaded asset: %w", err)
	}
	changed, err = result.RowsAffected()
	if err != nil {
		return UploadedAsset{}, err
	}
	if changed == 0 {
		existing, ok, err := uploadedAssetBySourceIdentityTx(
			tx,
			normalized.DeviceID,
			normalized.SourceAssetID,
			normalized.SourceAssetVersion,
		)
		if err != nil {
			return UploadedAsset{}, err
		}
		if !ok {
			return UploadedAsset{}, ErrUploadedAssetExists
		}
		if existing.UploadID != normalized.UploadID || existing.Status != "committing" {
			return existing, ErrUploadedAssetExists
		}
		result, err = tx.Exec(
			`UPDATE uploaded_assets
				SET media_type = ?, original_filename = ?, captured_at = ?,
					expected_size_bytes = ?, selected_root_key = ?,
					final_relative_path = ?, updated_at = ?
				WHERE id = ? AND status = 'committing'`,
			normalized.MediaType,
			normalized.OriginalFilename,
			nullableTime(normalized.CapturedAt),
			nullableInt64(normalized.ExpectedSizeBytes),
			selectedRootKey,
			normalized.FinalRelativePath,
			now,
			existing.ID,
		)
		if err != nil {
			return UploadedAsset{}, fmt.Errorf("update reserved uploaded asset: %w", err)
		}
		changed, err = result.RowsAffected()
		if err != nil {
			return UploadedAsset{}, err
		}
		if changed == 0 {
			return existing, ErrUploadedAssetExists
		}
		if err := upsertAssetChecksumsTx(tx, existing.ID, normalized.Checksums, now); err != nil {
			return UploadedAsset{}, err
		}
		if _, err := tx.Exec(
			`INSERT INTO upload_commit_events (upload_id, asset_id, event_type, message, created_at)
				VALUES (?, ?, 'reservation_updated', 'updated final path', ?)`,
			normalized.UploadID,
			existing.ID,
			now,
		); err != nil {
			return UploadedAsset{}, fmt.Errorf("insert upload commit event: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return UploadedAsset{}, err
		}
		tx = nil
		asset, ok, err := s.getUploadedAssetByID(existing.ID)
		if err != nil {
			return UploadedAsset{}, err
		}
		if !ok {
			return UploadedAsset{}, ErrUploadedAssetNotFound
		}
		return asset, nil
	}
	assetID, err := result.LastInsertId()
	if err != nil {
		return UploadedAsset{}, err
	}
	if err := upsertAssetChecksumsTx(tx, assetID, normalized.Checksums, now); err != nil {
		return UploadedAsset{}, err
	}
	if _, err := tx.Exec(
		`INSERT INTO upload_commit_events (upload_id, asset_id, event_type, message, created_at)
			VALUES (?, ?, 'reserved', '', ?)`,
		normalized.UploadID,
		assetID,
		now,
	); err != nil {
		return UploadedAsset{}, fmt.Errorf("insert upload commit event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return UploadedAsset{}, err
	}
	tx = nil
	asset, ok, err := s.GetUploadedAssetBySourceIdentity(
		normalized.DeviceID,
		normalized.SourceAssetID,
		normalized.SourceAssetVersion,
	)
	if err != nil {
		return UploadedAsset{}, err
	}
	if !ok {
		return UploadedAsset{}, ErrUploadedAssetNotFound
	}
	return asset, nil
}

// MarkUploaded marks a reserved uploaded asset as fully committed.
func (s *UploadStore) MarkUploaded(assetID int64, now time.Time) (UploadedAsset, error) {
	normalizedNow := formatDBTime(nonZeroTime(now))
	tx, err := s.db.Begin()
	if err != nil {
		return UploadedAsset{}, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	var uploadID string
	row := tx.QueryRow(`SELECT upload_id FROM uploaded_assets WHERE id = ?`, assetID)
	if err := row.Scan(&uploadID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UploadedAsset{}, ErrUploadedAssetNotFound
		}
		return UploadedAsset{}, err
	}
	result, err := tx.Exec(
		`UPDATE uploaded_assets
			SET status = 'uploaded', uploaded_at = COALESCE(uploaded_at, ?), updated_at = ?
			WHERE id = ?`,
		normalizedNow,
		normalizedNow,
		assetID,
	)
	if err != nil {
		return UploadedAsset{}, fmt.Errorf("mark uploaded asset uploaded: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return UploadedAsset{}, err
	}
	if changed == 0 {
		return UploadedAsset{}, ErrUploadedAssetNotFound
	}
	result, err = tx.Exec(
		`UPDATE upload_sessions
			SET status = 'completed', updated_at = ?
			WHERE upload_id = ?`,
		normalizedNow,
		uploadID,
	)
	if err != nil {
		return UploadedAsset{}, fmt.Errorf("mark upload session completed: %w", err)
	}
	changed, err = result.RowsAffected()
	if err != nil {
		return UploadedAsset{}, err
	}
	if changed == 0 {
		return UploadedAsset{}, ErrUploadSessionNotFound
	}
	if _, err := tx.Exec(
		`INSERT INTO upload_commit_events (upload_id, asset_id, event_type, message, created_at)
			VALUES (?, ?, 'completed', '', ?)`,
		uploadID,
		assetID,
		normalizedNow,
	); err != nil {
		return UploadedAsset{}, fmt.Errorf("insert upload commit event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return UploadedAsset{}, err
	}
	tx = nil
	asset, ok, err := s.getUploadedAssetByID(assetID)
	if err != nil {
		return UploadedAsset{}, err
	}
	if !ok {
		return UploadedAsset{}, ErrUploadedAssetNotFound
	}
	return asset, nil
}

// ResetDeviceUploadState removes upload ledger and session rows for one device
// and optional captured-at range. Final uploaded media files are never removed.
func (s *UploadStore) ResetDeviceUploadState(input UploadResetInput) (UploadResetResult, error) {
	normalized, err := normalizeUploadResetInput(input)
	if err != nil {
		return UploadResetResult{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return UploadResetResult{}, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	tempFiles, err := uploadSessionTempFilesForResetTx(tx, normalized)
	if err != nil {
		return UploadResetResult{}, err
	}
	assetQuery, assetArgs := resetWhereClause("device_id", "captured_at", normalized)
	assetResult, err := tx.Exec(`DELETE FROM uploaded_assets WHERE `+assetQuery, assetArgs...)
	if err != nil {
		return UploadResetResult{}, fmt.Errorf("reset uploaded assets: %w", err)
	}
	removedAssets, err := assetResult.RowsAffected()
	if err != nil {
		return UploadResetResult{}, err
	}
	sessionQuery, sessionArgs := resetWhereClause("device_id", "captured_at", normalized)
	sessionResult, err := tx.Exec(`DELETE FROM upload_sessions WHERE `+sessionQuery, sessionArgs...)
	if err != nil {
		return UploadResetResult{}, fmt.Errorf("reset upload sessions: %w", err)
	}
	removedSessions, err := sessionResult.RowsAffected()
	if err != nil {
		return UploadResetResult{}, err
	}
	if _, err := tx.Exec(
		`INSERT INTO upload_reset_events (device_id, captured_after, captured_before, reason, created_at)
			VALUES (?, ?, ?, ?, ?)`,
		normalized.DeviceID,
		nullableTime(normalized.CapturedAfter),
		nullableTime(normalized.CapturedBefore),
		normalized.Reason,
		formatDBTime(normalized.Now),
	); err != nil {
		return UploadResetResult{}, fmt.Errorf("insert upload reset event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return UploadResetResult{}, err
	}
	tx = nil
	return UploadResetResult{
		DeviceID:              normalized.DeviceID,
		CapturedAfter:         normalized.CapturedAfter,
		CapturedBefore:        normalized.CapturedBefore,
		ResetAt:               normalized.Now,
		RemovedUploadedAssets: removedAssets,
		RemovedSessions:       removedSessions,
		TempFiles:             tempFiles,
	}, nil
}

// CleanupUploadState removes expired or old auxiliary upload state. It keeps
// the uploaded asset ledger and checksum rows intact so once-only semantics are
// preserved even when final media files are deleted outside Timich.
func (s *UploadStore) CleanupUploadState(input UploadCleanupInput) (UploadCleanupResult, error) {
	normalized := normalizeUploadCleanupInput(input)
	tx, err := s.db.Begin()
	if err != nil {
		return UploadCleanupResult{}, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	tempFiles, err := uploadSessionTempFilesForCleanupTx(tx, normalized)
	if err != nil {
		return UploadCleanupResult{}, err
	}
	removedExpired, err := deleteUploadSessionsTx(
		tx,
		`status = 'active' AND expires_at IS NOT NULL AND expires_at <= ?`,
		formatDBTime(normalized.Now),
	)
	if err != nil {
		return UploadCleanupResult{}, err
	}
	removedCompleted, err := deleteUploadSessionsTx(
		tx,
		`status = 'completed' AND updated_at < ?`,
		formatDBTime(normalized.Now.Add(-normalized.CompletedSessionRetention)),
	)
	if err != nil {
		return UploadCleanupResult{}, err
	}
	removedAborted, err := deleteUploadSessionsTx(
		tx,
		`status = 'aborted' AND updated_at < ?`,
		formatDBTime(normalized.Now.Add(-normalized.AbortedSessionRetention)),
	)
	if err != nil {
		return UploadCleanupResult{}, err
	}
	removedFailed, err := deleteUploadSessionsTx(
		tx,
		`status = 'failed' AND updated_at < ?`,
		formatDBTime(normalized.Now.Add(-normalized.FailedSessionRetention)),
	)
	if err != nil {
		return UploadCleanupResult{}, err
	}
	commitEventsResult, err := tx.Exec(
		`DELETE FROM upload_commit_events
			WHERE created_at < ?
				AND upload_id NOT IN (
					SELECT upload_id FROM uploaded_assets WHERE status = 'committing'
				)
				AND upload_id NOT IN (
					SELECT upload_id FROM upload_sessions WHERE status IN ('active', 'committing')
				)`,
		formatDBTime(normalized.Now.Add(-normalized.CommitEventRetention)),
	)
	if err != nil {
		return UploadCleanupResult{}, fmt.Errorf("cleanup upload commit events: %w", err)
	}
	removedCommitEvents, err := commitEventsResult.RowsAffected()
	if err != nil {
		return UploadCleanupResult{}, err
	}
	resetEventsResult, err := tx.Exec(
		`DELETE FROM upload_reset_events WHERE created_at < ?`,
		formatDBTime(normalized.Now.Add(-normalized.ResetEventRetention)),
	)
	if err != nil {
		return UploadCleanupResult{}, fmt.Errorf("cleanup upload reset events: %w", err)
	}
	removedResetEvents, err := resetEventsResult.RowsAffected()
	if err != nil {
		return UploadCleanupResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return UploadCleanupResult{}, err
	}
	tx = nil

	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return UploadCleanupResult{}, fmt.Errorf("checkpoint upload store wal: %w", err)
	}
	return UploadCleanupResult{
		CleanedAt:              normalized.Now,
		RemovedExpiredSessions: removedExpired,
		RemovedCompleted:       removedCompleted,
		RemovedAborted:         removedAborted,
		RemovedFailed:          removedFailed,
		RemovedCommitEvents:    removedCommitEvents,
		RemovedResetEvents:     removedResetEvents,
		TempFiles:              tempFiles,
		WALCheckpointed:        true,
	}, nil
}

func selectedRootKeyForUploadSession(tx *sql.Tx, uploadID string) (string, error) {
	var selectedRootKey string
	row := tx.QueryRow(`SELECT selected_root_key FROM upload_sessions WHERE upload_id = ?`, uploadID)
	if err := row.Scan(&selectedRootKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrUploadSessionNotFound
		}
		return "", err
	}
	if strings.TrimSpace(selectedRootKey) == "" {
		return "", errors.New("selected root key must not be empty")
	}
	return selectedRootKey, nil
}

func uploadedAssetBySourceIdentityTx(
	tx *sql.Tx,
	deviceID string,
	sourceAssetID string,
	sourceAssetVersion string,
) (UploadedAsset, bool, error) {
	row := tx.QueryRow(
		uploadedAssetSelectSQL()+` WHERE device_id = ? AND source_asset_id = ? AND source_asset_version = ?`,
		strings.TrimSpace(deviceID),
		strings.TrimSpace(sourceAssetID),
		strings.TrimSpace(sourceAssetVersion),
	)
	asset, err := scanUploadedAsset(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UploadedAsset{}, false, nil
		}
		return UploadedAsset{}, false, err
	}
	checksums, err := checksumsForAssetTx(tx, asset.ID)
	if err != nil {
		return UploadedAsset{}, false, err
	}
	asset.Checksums = checksums
	return asset, true, nil
}

func upsertAssetChecksumsTx(tx *sql.Tx, assetID int64, checksums []UploadChecksum, now string) error {
	for _, checksum := range checksums {
		if _, err := tx.Exec(
			`INSERT INTO uploaded_asset_checksums (asset_id, algorithm, encoding, digest, created_at)
				VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(asset_id, algorithm) DO UPDATE SET
					encoding = excluded.encoding,
					digest = excluded.digest`,
			assetID,
			checksum.Algorithm,
			checksum.Encoding,
			checksum.Digest,
			now,
		); err != nil {
			return fmt.Errorf("upsert uploaded asset checksum: %w", err)
		}
	}
	return nil
}

func checksumsForAssetTx(tx *sql.Tx, assetID int64) ([]UploadChecksum, error) {
	rows, err := tx.Query(
		`SELECT algorithm, encoding, digest
			FROM uploaded_asset_checksums
			WHERE asset_id = ?
			ORDER BY algorithm`,
		assetID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var checksums []UploadChecksum
	for rows.Next() {
		var checksum UploadChecksum
		if err := rows.Scan(&checksum.Algorithm, &checksum.Encoding, &checksum.Digest); err != nil {
			return nil, err
		}
		checksums = append(checksums, checksum)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return checksums, nil
}

func uploadSessionTempFilesForResetTx(tx *sql.Tx, input UploadResetInput) ([]UploadTempFile, error) {
	where, args := resetWhereClause("device_id", "captured_at", input)
	rows, err := tx.Query(
		`SELECT selected_root_key, temp_relative_path
			FROM upload_sessions
			WHERE `+where+`
				AND temp_relative_path <> ''`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []UploadTempFile
	for rows.Next() {
		var file UploadTempFile
		if err := rows.Scan(&file.RootKey, &file.RelativePath); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return files, nil
}

func uploadSessionTempFilesForCleanupTx(tx *sql.Tx, input UploadCleanupInput) ([]UploadTempFile, error) {
	completedCutoff := formatDBTime(input.Now.Add(-input.CompletedSessionRetention))
	abortedCutoff := formatDBTime(input.Now.Add(-input.AbortedSessionRetention))
	failedCutoff := formatDBTime(input.Now.Add(-input.FailedSessionRetention))
	rows, err := tx.Query(
		`SELECT selected_root_key, temp_relative_path
			FROM upload_sessions
			WHERE temp_relative_path <> ''
				AND (
					(status = 'active' AND expires_at IS NOT NULL AND expires_at <= ?)
					OR (status = 'completed' AND updated_at < ?)
					OR (status = 'aborted' AND updated_at < ?)
					OR (status = 'failed' AND updated_at < ?)
				)`,
		formatDBTime(input.Now),
		completedCutoff,
		abortedCutoff,
		failedCutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []UploadTempFile
	for rows.Next() {
		var file UploadTempFile
		if err := rows.Scan(&file.RootKey, &file.RelativePath); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return files, nil
}

func deleteUploadSessionsTx(tx *sql.Tx, where string, args ...any) (int64, error) {
	result, err := tx.Exec(`DELETE FROM upload_sessions WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("cleanup upload sessions: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return removed, nil
}

func resetWhereClause(deviceColumn string, capturedColumn string, input UploadResetInput) (string, []any) {
	clauses := []string{deviceColumn + " = ?"}
	args := []any{input.DeviceID}
	if input.CapturedAfter != nil {
		clauses = append(clauses, capturedColumn+" IS NOT NULL", capturedColumn+" >= ?")
		args = append(args, formatDBTime(*input.CapturedAfter))
	}
	if input.CapturedBefore != nil {
		clauses = append(clauses, capturedColumn+" IS NOT NULL", capturedColumn+" < ?")
		args = append(args, formatDBTime(*input.CapturedBefore))
	}
	return strings.Join(clauses, " AND "), args
}

// GetUploadedAssetBySourceIdentity returns an uploaded or committing asset row
// for the device-local once-only identity.
func (s *UploadStore) GetUploadedAssetBySourceIdentity(
	deviceID string,
	sourceAssetID string,
	sourceAssetVersion string,
) (UploadedAsset, bool, error) {
	row := s.db.QueryRow(
		uploadedAssetSelectSQL()+` WHERE device_id = ? AND source_asset_id = ? AND source_asset_version = ?`,
		strings.TrimSpace(deviceID),
		strings.TrimSpace(sourceAssetID),
		strings.TrimSpace(sourceAssetVersion),
	)
	asset, err := scanUploadedAsset(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UploadedAsset{}, false, nil
		}
		return UploadedAsset{}, false, err
	}
	checksums, err := s.checksumsForAsset(asset.ID)
	if err != nil {
		return UploadedAsset{}, false, err
	}
	asset.Checksums = checksums
	return asset, true, nil
}

// LatestUploadedAt returns the newest completed upload timestamp for one
// paired device.
func (s *UploadStore) LatestUploadedAt(deviceID string) (*time.Time, error) {
	row := s.db.QueryRow(
		`SELECT uploaded_at
			FROM uploaded_assets
			WHERE device_id = ?
				AND status = 'uploaded'
				AND uploaded_at IS NOT NULL
			ORDER BY uploaded_at DESC
			LIMIT 1`,
		strings.TrimSpace(deviceID),
	)
	var uploadedAt string
	if err := row.Scan(&uploadedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	parsed, err := parseDBTime(uploadedAt)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// GetUploadedAssetByUploadID returns an uploaded or committing asset row for
// one upload session id.
func (s *UploadStore) GetUploadedAssetByUploadID(uploadID string) (UploadedAsset, bool, error) {
	row := s.db.QueryRow(
		uploadedAssetSelectSQL()+` WHERE upload_id = ?`,
		strings.TrimSpace(uploadID),
	)
	asset, err := scanUploadedAsset(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UploadedAsset{}, false, nil
		}
		return UploadedAsset{}, false, err
	}
	checksums, err := s.checksumsForAsset(asset.ID)
	if err != nil {
		return UploadedAsset{}, false, err
	}
	asset.Checksums = checksums
	return asset, true, nil
}

func (s *UploadStore) getUploadedAssetByID(assetID int64) (UploadedAsset, bool, error) {
	row := s.db.QueryRow(uploadedAssetSelectSQL()+` WHERE id = ?`, assetID)
	asset, err := scanUploadedAsset(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UploadedAsset{}, false, nil
		}
		return UploadedAsset{}, false, err
	}
	checksums, err := s.checksumsForAsset(asset.ID)
	if err != nil {
		return UploadedAsset{}, false, err
	}
	asset.Checksums = checksums
	return asset, true, nil
}

func (s *UploadStore) checksumsForAsset(assetID int64) ([]UploadChecksum, error) {
	rows, err := s.db.Query(
		`SELECT algorithm, encoding, digest
			FROM uploaded_asset_checksums
			WHERE asset_id = ?
			ORDER BY algorithm`,
		assetID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var checksums []UploadChecksum
	for rows.Next() {
		var checksum UploadChecksum
		if err := rows.Scan(&checksum.Algorithm, &checksum.Encoding, &checksum.Digest); err != nil {
			return nil, err
		}
		checksums = append(checksums, checksum)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return checksums, nil
}

func uploadedAssetSelectSQL() string {
	return `SELECT id, device_id, source_asset_id, source_asset_version, upload_id,
		status, media_type, original_filename, captured_at, expected_size_bytes,
		selected_root_key, final_relative_path, created_at, updated_at, committing_at, uploaded_at
		FROM uploaded_assets`
}

func uploadSessionSelectSQL() string {
	return `SELECT upload_id, device_id, source_asset_id, source_asset_version, status,
		media_type, original_filename, captured_at, expected_size_bytes,
		next_offset, selected_root_key, temp_relative_path, final_relative_path,
		last_error, created_at, updated_at, expires_at
		FROM upload_sessions`
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUploadSession(scanner rowScanner) (UploadSession, error) {
	var session UploadSession
	var capturedAt sql.NullString
	var expectedSize sql.NullInt64
	var finalRelativePath sql.NullString
	var lastError sql.NullString
	var createdAt string
	var updatedAt string
	var expiresAt sql.NullString
	if err := scanner.Scan(
		&session.UploadID,
		&session.DeviceID,
		&session.SourceAssetID,
		&session.SourceAssetVersion,
		&session.Status,
		&session.MediaType,
		&session.OriginalFilename,
		&capturedAt,
		&expectedSize,
		&session.NextOffset,
		&session.SelectedRootKey,
		&session.TempRelativePath,
		&finalRelativePath,
		&lastError,
		&createdAt,
		&updatedAt,
		&expiresAt,
	); err != nil {
		return UploadSession{}, err
	}
	parsedCreatedAt, err := parseDBTime(createdAt)
	if err != nil {
		return UploadSession{}, err
	}
	parsedUpdatedAt, err := parseDBTime(updatedAt)
	if err != nil {
		return UploadSession{}, err
	}
	session.CreatedAt = parsedCreatedAt
	session.UpdatedAt = parsedUpdatedAt
	session.CapturedAt, err = parseNullableDBTime(capturedAt)
	if err != nil {
		return UploadSession{}, err
	}
	session.ExpiresAt, err = parseNullableDBTime(expiresAt)
	if err != nil {
		return UploadSession{}, err
	}
	if expectedSize.Valid {
		value := expectedSize.Int64
		session.ExpectedSizeBytes = &value
	}
	if finalRelativePath.Valid {
		session.FinalRelativePath = finalRelativePath.String
	}
	if lastError.Valid {
		session.LastError = lastError.String
	}
	return session, nil
}

func scanUploadedAsset(scanner rowScanner) (UploadedAsset, error) {
	var asset UploadedAsset
	var capturedAt sql.NullString
	var expectedSize sql.NullInt64
	var createdAt string
	var updatedAt string
	var committingAt sql.NullString
	var uploadedAt sql.NullString
	if err := scanner.Scan(
		&asset.ID,
		&asset.DeviceID,
		&asset.SourceAssetID,
		&asset.SourceAssetVersion,
		&asset.UploadID,
		&asset.Status,
		&asset.MediaType,
		&asset.OriginalFilename,
		&capturedAt,
		&expectedSize,
		&asset.SelectedRootKey,
		&asset.FinalRelativePath,
		&createdAt,
		&updatedAt,
		&committingAt,
		&uploadedAt,
	); err != nil {
		return UploadedAsset{}, err
	}
	parsedCreatedAt, err := parseDBTime(createdAt)
	if err != nil {
		return UploadedAsset{}, err
	}
	parsedUpdatedAt, err := parseDBTime(updatedAt)
	if err != nil {
		return UploadedAsset{}, err
	}
	asset.CreatedAt = parsedCreatedAt
	asset.UpdatedAt = parsedUpdatedAt
	asset.CapturedAt, err = parseNullableDBTime(capturedAt)
	if err != nil {
		return UploadedAsset{}, err
	}
	asset.CommittingAt, err = parseNullableDBTime(committingAt)
	if err != nil {
		return UploadedAsset{}, err
	}
	asset.UploadedAt, err = parseNullableDBTime(uploadedAt)
	if err != nil {
		return UploadedAsset{}, err
	}
	if expectedSize.Valid {
		value := expectedSize.Int64
		asset.ExpectedSizeBytes = &value
	}
	return asset, nil
}

func normalizeSessionInput(input UploadSessionInput) (UploadSessionInput, error) {
	input.UploadID = strings.TrimSpace(input.UploadID)
	input.DeviceID = strings.TrimSpace(input.DeviceID)
	input.SourceAssetID = strings.TrimSpace(input.SourceAssetID)
	input.SourceAssetVersion = strings.TrimSpace(input.SourceAssetVersion)
	input.MediaType = strings.TrimSpace(input.MediaType)
	input.OriginalFilename = strings.TrimSpace(input.OriginalFilename)
	input.SelectedRootKey = strings.TrimSpace(input.SelectedRootKey)
	input.TempRelativePath = strings.TrimSpace(input.TempRelativePath)
	input.Now = nonZeroTime(input.Now)
	if input.UploadID == "" {
		return UploadSessionInput{}, errors.New("upload id must not be empty")
	}
	if input.DeviceID == "" {
		return UploadSessionInput{}, errors.New("device id must not be empty")
	}
	if input.SourceAssetID == "" {
		return UploadSessionInput{}, errors.New("source asset id must not be empty")
	}
	if input.SourceAssetVersion == "" {
		return UploadSessionInput{}, errors.New("source asset version must not be empty")
	}
	if input.MediaType == "" {
		return UploadSessionInput{}, errors.New("media type must not be empty")
	}
	if input.OriginalFilename == "" {
		return UploadSessionInput{}, errors.New("original filename must not be empty")
	}
	if input.SelectedRootKey == "" {
		return UploadSessionInput{}, errors.New("selected root key must not be empty")
	}
	if input.TempRelativePath == "" {
		return UploadSessionInput{}, errors.New("temp relative path must not be empty")
	}
	if input.ExpectedSizeBytes != nil && *input.ExpectedSizeBytes < 0 {
		return UploadSessionInput{}, errors.New("expected size bytes must not be negative")
	}
	return input, nil
}

func normalizeCommitInput(input UploadCommitInput) (UploadCommitInput, error) {
	input.UploadID = strings.TrimSpace(input.UploadID)
	input.DeviceID = strings.TrimSpace(input.DeviceID)
	input.SourceAssetID = strings.TrimSpace(input.SourceAssetID)
	input.SourceAssetVersion = strings.TrimSpace(input.SourceAssetVersion)
	input.MediaType = strings.TrimSpace(input.MediaType)
	input.OriginalFilename = strings.TrimSpace(input.OriginalFilename)
	input.FinalRelativePath = strings.TrimSpace(input.FinalRelativePath)
	input.Now = nonZeroTime(input.Now)
	if input.UploadID == "" {
		return UploadCommitInput{}, errors.New("upload id must not be empty")
	}
	if input.DeviceID == "" {
		return UploadCommitInput{}, errors.New("device id must not be empty")
	}
	if input.SourceAssetID == "" {
		return UploadCommitInput{}, errors.New("source asset id must not be empty")
	}
	if input.SourceAssetVersion == "" {
		return UploadCommitInput{}, errors.New("source asset version must not be empty")
	}
	if input.MediaType == "" {
		return UploadCommitInput{}, errors.New("media type must not be empty")
	}
	if input.OriginalFilename == "" {
		return UploadCommitInput{}, errors.New("original filename must not be empty")
	}
	if input.FinalRelativePath == "" {
		return UploadCommitInput{}, errors.New("final relative path must not be empty")
	}
	if input.ExpectedSizeBytes != nil && *input.ExpectedSizeBytes < 0 {
		return UploadCommitInput{}, errors.New("expected size bytes must not be negative")
	}
	seenChecksums := map[string]struct{}{}
	for index := range input.Checksums {
		input.Checksums[index].Algorithm = strings.ToLower(strings.TrimSpace(input.Checksums[index].Algorithm))
		input.Checksums[index].Encoding = strings.ToLower(strings.TrimSpace(input.Checksums[index].Encoding))
		input.Checksums[index].Digest = strings.TrimSpace(input.Checksums[index].Digest)
		if input.Checksums[index].Encoding == "" {
			input.Checksums[index].Encoding = "hex"
		}
		if input.Checksums[index].Algorithm == "" {
			return UploadCommitInput{}, errors.New("checksum algorithm must not be empty")
		}
		if input.Checksums[index].Digest == "" {
			return UploadCommitInput{}, errors.New("checksum digest must not be empty")
		}
		if _, ok := seenChecksums[input.Checksums[index].Algorithm]; ok {
			return UploadCommitInput{}, errors.New("checksum algorithms must be unique")
		}
		seenChecksums[input.Checksums[index].Algorithm] = struct{}{}
	}
	return input, nil
}

func normalizeUploadResetInput(input UploadResetInput) (UploadResetInput, error) {
	input.DeviceID = strings.TrimSpace(input.DeviceID)
	input.Reason = strings.TrimSpace(input.Reason)
	input.Now = nonZeroTime(input.Now)
	if input.DeviceID == "" {
		return UploadResetInput{}, errors.New("device id must not be empty")
	}
	if input.CapturedAfter != nil {
		capturedAfter := input.CapturedAfter.UTC()
		input.CapturedAfter = &capturedAfter
	}
	if input.CapturedBefore != nil {
		capturedBefore := input.CapturedBefore.UTC()
		input.CapturedBefore = &capturedBefore
	}
	if input.CapturedAfter != nil && input.CapturedBefore != nil && !input.CapturedAfter.Before(*input.CapturedBefore) {
		return UploadResetInput{}, errors.New("captured after must be before captured before")
	}
	return input, nil
}

func normalizeUploadCleanupInput(input UploadCleanupInput) UploadCleanupInput {
	input.Now = nonZeroTime(input.Now)
	input.CompletedSessionRetention = nonNegativeDuration(input.CompletedSessionRetention)
	input.AbortedSessionRetention = nonNegativeDuration(input.AbortedSessionRetention)
	input.FailedSessionRetention = nonNegativeDuration(input.FailedSessionRetention)
	input.CommitEventRetention = nonNegativeDuration(input.CommitEventRetention)
	input.ResetEventRetention = nonNegativeDuration(input.ResetEventRetention)
	return input
}

func nonNegativeDuration(value time.Duration) time.Duration {
	if value < 0 {
		return 0
	}
	return value
}

func nullableTime(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return formatDBTime(*value)
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nonZeroTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func formatDBTime(value time.Time) string {
	return value.UTC().Format(dbTimeLayout)
}

func parseDBTime(value string) (time.Time, error) {
	parsed, err := time.Parse(dbTimeLayout, value)
	if err == nil {
		return parsed.UTC(), nil
	}
	parsed, err = time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func parseNullableDBTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil, nil
	}
	parsed, err := parseDBTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}
