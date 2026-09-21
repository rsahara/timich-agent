package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const immichMirrorReconciliationRequired = "Immich mirror requires one full reconciliation to initialize the upstream-clock checkpoint; run datasource indexing in full mode"

// Preserve catalog identities on upgrade. Legacy timestamps were based on the
// Agent's clock, so reusing them as upstream checkpoints could retain omissions.
func (s *CatalogStore) ensureImmichMirrorCursorSchema() error {
	columns, err := s.tableColumns("immich_mirror_state")
	if err != nil {
		return fmt.Errorf("inspect immich mirror cursor schema: %w", err)
	}
	for _, column := range []struct{ name, definition string }{
		{"synced_through", "TEXT"},
		{"sync_clock", "TEXT NOT NULL DEFAULT ''"},
	} {
		if !columns[column.name] {
			if _, err := s.db.Exec(`ALTER TABLE immich_mirror_state ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
				return fmt.Errorf("add immich mirror cursor: %w", err)
			}
		}
	}
	return nil
}

func (s *CatalogStore) immichMirrorUpdatedAfter(ctx context.Context, sourceKey string) (*time.Time, error) {
	if s == nil || s.db == nil {
		return nil, ErrCatalogNotConfigured
	}
	var cursor sql.NullString
	var clock string
	err := s.queryDB().QueryRowContext(ctx, `SELECT synced_through, sync_clock FROM immich_mirror_state WHERE source_key = ?`, strings.TrimSpace(sourceKey)).Scan(&cursor, &clock)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read immich mirror cursor: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if !cursor.Valid || clock != immichMirrorClock {
		return nil, errors.New(immichMirrorReconciliationRequired)
	}
	value, err := time.Parse(time.RFC3339Nano, cursor.String)
	if err != nil {
		return nil, fmt.Errorf("parse immich mirror cursor: %w", err)
	}
	return &value, nil
}
