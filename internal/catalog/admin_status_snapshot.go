package catalog

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// AdminStatusSnapshot stores a small, UI-oriented status payload that can be
// served when live catalog aggregation is busy.
type AdminStatusSnapshot struct {
	Key       string
	Payload   []byte
	UpdatedAt time.Time
}

// SaveAdminStatusSnapshot persists a best-effort Admin UI status snapshot.
func (s *Service) SaveAdminStatusSnapshot(ctx context.Context, key string, payload []byte, updatedAt time.Time) error {
	if !s.catalogStoreEnabled() {
		return nil
	}
	return s.catalog.SaveAdminStatusSnapshot(ctx, key, payload, updatedAt)
}

// AdminStatusSnapshot returns a previously persisted Admin UI status snapshot.
func (s *Service) AdminStatusSnapshot(ctx context.Context, key string) (AdminStatusSnapshot, bool, error) {
	if !s.catalogStoreEnabled() {
		return AdminStatusSnapshot{}, false, nil
	}
	return s.catalog.AdminStatusSnapshot(ctx, key)
}

// DeleteAdminStatusSnapshot removes a persisted Admin UI status snapshot.
func (s *Service) DeleteAdminStatusSnapshot(ctx context.Context, key string) error {
	if !s.catalogStoreEnabled() {
		return nil
	}
	return s.catalog.DeleteAdminStatusSnapshot(ctx, key)
}

func (s *CatalogStore) SaveAdminStatusSnapshot(ctx context.Context, key string, payload []byte, updatedAt time.Time) error {
	if s == nil || s.db == nil {
		return nil
	}
	key = strings.TrimSpace(key)
	if key == "" || len(payload) == 0 {
		return nil
	}
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	writeDB, err := s.openAdminWriteDB(ctx)
	if err != nil {
		return err
	}
	defer writeDB.Close()

	_, err = writeDB.ExecContext(ctx, `INSERT INTO admin_status_snapshots (
		snapshot_key,
		payload_json,
		updated_at
	) VALUES (?, ?, ?)
	ON CONFLICT(snapshot_key) DO UPDATE SET
		payload_json = excluded.payload_json,
		updated_at = excluded.updated_at`,
		key,
		string(payload),
		updatedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (s *CatalogStore) AdminStatusSnapshot(ctx context.Context, key string) (AdminStatusSnapshot, bool, error) {
	if s == nil || s.db == nil {
		return AdminStatusSnapshot{}, false, nil
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return AdminStatusSnapshot{}, false, nil
	}
	readDB, ok, err := s.openAdminReadDB(ctx)
	if err != nil {
		return AdminStatusSnapshot{}, false, err
	}
	if !ok {
		return AdminStatusSnapshot{}, false, nil
	}
	defer readDB.Close()

	var payload string
	var updatedAtRaw string
	err = readDB.QueryRowContext(ctx, `SELECT payload_json, updated_at
		FROM admin_status_snapshots WHERE snapshot_key = ?`, key).Scan(&payload, &updatedAtRaw)
	if err != nil {
		if err == sql.ErrNoRows {
			return AdminStatusSnapshot{}, false, nil
		}
		return AdminStatusSnapshot{}, false, err
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, updatedAtRaw)
	if err != nil {
		updatedAt = time.Time{}
	}
	return AdminStatusSnapshot{
		Key:       key,
		Payload:   []byte(payload),
		UpdatedAt: updatedAt.UTC(),
	}, true, nil
}

func (s *CatalogStore) DeleteAdminStatusSnapshot(ctx context.Context, key string) error {
	if s == nil || s.db == nil {
		return nil
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	writeDB, err := s.openAdminWriteDB(ctx)
	if err != nil {
		return err
	}
	defer writeDB.Close()

	_, err = writeDB.ExecContext(ctx, `DELETE FROM admin_status_snapshots WHERE snapshot_key = ?`, key)
	return err
}
