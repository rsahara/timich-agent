package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// validateGalleryStorageSchema rejects a version marker pasted onto an old or
// incomplete layout. It does not repair or rebuild an existing database.
func validateGalleryStorageSchema(ctx context.Context, db *sql.DB, clustered bool) error {
	var withoutRowID int
	if err := db.QueryRowContext(ctx, "SELECT wr FROM pragma_table_list WHERE schema = 'main' AND name = 'catalog_gallery_projection'").Scan(&withoutRowID); err != nil {
		return fmt.Errorf("inspect Gallery table: %w", err)
	}
	if (withoutRowID == 1) != clustered {
		return fmt.Errorf("Gallery table WITHOUT ROWID = %d, want %t", withoutRowID, clustered)
	}
	rows, err := db.QueryContext(ctx, `SELECT name, type, "notnull", pk, hidden
		FROM pragma_table_xinfo('catalog_gallery_projection') ORDER BY cid`)
	if err != nil {
		return err
	}
	type column struct {
		name, kind          string
		notNull, pk, hidden int
	}
	want := []column{
		{"canonical_asset_id", "TEXT", 0, 1, 0},
		{"source_key", "TEXT", 1, 0, 0},
		{"upstream_asset_id", "TEXT", 1, 0, 0},
		{"media_type", "TEXT", 1, 0, 0},
		{"filename", "TEXT", 1, 0, 0},
		{"captured_at", "TEXT", 1, 0, 0},
		{"duration", "TEXT", 0, 0, 0},
	}
	if clustered {
		want[0].notNull, want[0].pk, want[5].pk = 1, 2, 1
	}
	var got []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.name, &c.kind, &c.notNull, &c.pk, &c.hidden); err != nil {
			_ = rows.Close()
			return err
		}
		got = append(got, c)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(got) != len(want) {
		return fmt.Errorf("Gallery column count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("unexpected Gallery column %q definition", want[i].name)
		}
	}
	var uniqueID int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_index_list('catalog_gallery_projection') i
		WHERE i."unique" = 1 AND i.partial = 0
		AND (SELECT COUNT(*) FROM pragma_index_xinfo(i.name) WHERE key = 1) = 1
		AND EXISTS (SELECT 1 FROM pragma_index_xinfo(i.name) WHERE key = 1 AND name = 'canonical_asset_id')`).Scan(&uniqueID); err != nil {
		return err
	}
	if uniqueID != 1 {
		return fmt.Errorf("Gallery requires one non-partial canonical ID uniqueness constraint")
	}
	if clustered {
		var orderedKey int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_index_list('catalog_gallery_projection') i
			JOIN pragma_index_xinfo(i.name) x WHERE i.origin = 'pk' AND x.key = 1
			AND ((x.seqno = 0 AND x.name = 'captured_at' AND x.desc = 1 AND x.coll = 'BINARY')
			OR (x.seqno = 1 AND x.name = 'canonical_asset_id' AND x.desc = 0 AND x.coll = 'BINARY'))`).Scan(&orderedKey); err != nil {
			return err
		}
		if orderedKey != 2 {
			return fmt.Errorf("Gallery primary key must order captured_at DESC, canonical_asset_id ASC")
		}
	}
	return nil
}
