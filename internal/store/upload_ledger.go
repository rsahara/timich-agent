package store

import (
	"database/sql"
	"errors"
	"strings"
)

// UploadIdentity is scoped to the authenticated device by the caller.
type UploadIdentity struct {
	SourceAssetID      string
	SourceAssetVersion string
}

// LedgerEpoch creates a generation once, preserving it across normal operation.
func (s *UploadStore) LedgerEpoch(deviceID string) (string, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return "", ErrDeviceNotFound
	}
	// The common read path does not take a write lock.
	var epoch string
	err := s.db.QueryRow(`SELECT epoch FROM upload_ledger_epochs WHERE device_id = ?`, deviceID).Scan(&epoch)
	if err == nil {
		return epoch, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO upload_ledger_epochs(device_id, epoch)
        VALUES (?, lower(hex(randomblob(16))))`, deviceID); err != nil {
		return "", err
	}
	err = s.db.QueryRow(`SELECT epoch FROM upload_ledger_epochs WHERE device_id = ?`, deviceID).Scan(&epoch)
	return epoch, err
}

// CheckUploaded reads the generation and all decisions from one read snapshot.
// It never opens sessions, recovers commits, or probes the filesystem.
func (s *UploadStore) CheckUploaded(deviceID string, identities []UploadIdentity) (string, []bool, error) {
	deviceID = strings.TrimSpace(deviceID)
	if _, err := s.LedgerEpoch(deviceID); err != nil {
		return "", nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()
	var epoch string
	if err := tx.QueryRow(`SELECT epoch FROM upload_ledger_epochs WHERE device_id = ?`, deviceID).Scan(&epoch); err != nil {
		return "", nil, err
	}
	stmt, err := tx.Prepare(`SELECT EXISTS(SELECT 1 FROM uploaded_assets
        WHERE device_id = ? AND source_asset_id = ? AND source_asset_version = ? AND status = 'uploaded')`)
	if err != nil {
		return "", nil, err
	}
	defer stmt.Close()
	states := make([]bool, len(identities))
	for i, identity := range identities {
		if err := stmt.QueryRow(deviceID, identity.SourceAssetID, identity.SourceAssetVersion).Scan(&states[i]); err != nil {
			return "", nil, err
		}
	}
	return epoch, states, tx.Commit()
}
