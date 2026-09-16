package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGalleryV5InitialSchemaIncludesClusteredStorage(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	store := &CatalogStore{db: db}
	if err := store.ensureCatalogSchema(); err != nil {
		t.Fatal(err)
	}
	// Model stopping after the base-schema commit, before the later Gallery
	// index/trigger setup. The next startup must still recognize valid V5.
	if err := validateGalleryStorageSchema(context.Background(), db, true); err != nil {
		t.Fatal(err)
	}
	if err := store.ensureCatalogSchema(); err != nil {
		t.Fatalf("base-schema restart: %v", err)
	}
}

func TestGalleryClusteredStoragePreservesDensePages(t *testing.T) {
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	assertGallerySeekIndex(t, store.db)
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO catalog_gallery_projection(canonical_asset_id,` + galleryProjectionSelectColumns + `)
		VALUES(?, 'source', ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	const ties = 32768
	const total = ties + 64
	at := formatCatalogTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	older := formatCatalogTime(time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	for i := total - 1; i >= 0; i-- {
		id := fmt.Sprintf("asset-%06d", i)
		date := at
		if i >= ties {
			date = older
		}
		kind := "image"
		var duration any
		if i%3 == 0 {
			kind = "video"
			duration = "1.25"
		}
		if _, err := stmt.Exec(id, id, kind, "長い写真名 "+id+".jpg", date, duration); err != nil {
			t.Fatal(err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, position := range []int{0, 256, 26000, 32750, 32768, 32820, 32832} {
		t.Run(fmt.Sprint(position), func(t *testing.T) {
			read, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer read.Rollback()
			date, id, found, err := galleryProjectionAnchorAtOffset(ctx, read, position)
			if err != nil {
				t.Fatal(err)
			}
			if position == total {
				if found {
					t.Fatal("found beyond end")
				}
				return
			}
			if !found {
				t.Fatal("anchor absent")
			}
			rows, err := read.QueryContext(ctx, galleryProjectionAfterAnchorSQL, date, id, 61)
			if err != nil {
				t.Fatal(err)
			}
			var got [][]sql.NullString
			for rows.Next() {
				v := make([]sql.NullString, 6)
				if err := rows.Scan(&v[0], &v[1], &v[2], &v[3], &v[4], &v[5]); err != nil {
					t.Fatal(err)
				}
				got = append(got, v)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()
			rows, err = read.Query(`SELECT `+galleryProjectionSelectColumns+` FROM catalog_gallery_projection
				ORDER BY captured_at DESC, canonical_asset_id ASC LIMIT 61 OFFSET ?`, position)
			if err != nil {
				t.Fatal(err)
			}
			var want [][]sql.NullString
			for rows.Next() {
				v := make([]sql.NullString, 6)
				if err := rows.Scan(&v[0], &v[1], &v[2], &v[3], &v[4], &v[5]); err != nil {
					t.Fatal(err)
				}
				want = append(want, v)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()
			if !reflect.DeepEqual(got, want) {
				t.Fatal("bounded page differs from exact sorted page")
			}
		})
	}
	plan := explainGalleryProjectionQueryPlan(t, store.db, galleryProjectionAfterAnchorSQL, at, "asset-026000", 61)
	if !strings.Contains(plan, "USING PRIMARY KEY (captured_at=? AND canonical_asset_id>?)") ||
		!strings.Contains(plan, "USING PRIMARY KEY (captured_at<?)") || strings.Contains(plan, "SCAN catalog_gallery_projection") {
		t.Fatalf("unbounded Gallery access:\n%s", plan)
	}
	if _, err := store.db.Exec(`INSERT INTO catalog_gallery_projection(canonical_asset_id,` + galleryProjectionSelectColumns + `)
		SELECT canonical_asset_id,source_key,upstream_asset_id,media_type,filename,'2026-09-04T00:00:00Z',duration
		FROM catalog_gallery_projection LIMIT 1`); err == nil {
		t.Fatal("same ID accepted at another date")
	}
	assertGalleryProjectionDayIndexMatches(t, store.db)
	// Compare the bounded key-first path to the prior filtered query at deep
	// offsets, including both types, date bounds, partial pages and empty pages.
	for _, where := range []string{
		"WHERE media_type='video'",
		"WHERE media_type='image'",
		"WHERE media_type IN ('video','image')",
		"WHERE captured_at >= '2026-09-01'",
		"WHERE media_type='video' AND captured_at >= '2026-08-31' AND captured_at < '2026-09-02'",
	} {
		for _, offset := range []int{0, 10000, 10930, 32768, total} {
			queryRows := func(query string) [][]sql.NullString {
				t.Helper()
				rows, err := store.db.Query(query, 61, offset)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var values [][]sql.NullString
				for rows.Next() {
					v := make([]sql.NullString, 6)
					if err := rows.Scan(&v[0], &v[1], &v[2], &v[3], &v[4], &v[5]); err != nil {
						t.Fatal(err)
					}
					values = append(values, v)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				return values
			}
			want := queryRows(`SELECT ` + galleryProjectionSelectColumns + ` FROM catalog_gallery_projection ` + where + `
				ORDER BY captured_at DESC, canonical_asset_id ASC LIMIT ? OFFSET ?`)
			got := queryRows(galleryProjectionFilteredPageSQL(where))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("filtered page changed: %s offset %d", where, offset)
			}
		}
	}
	filteredPlan := explainGalleryProjectionQueryPlan(t, store.db, galleryProjectionFilteredPageSQL("WHERE media_type=?"), "video", 61, 10000)
	if !strings.Contains(filteredPlan, "MATERIALIZE page_keys") ||
		!strings.Contains(filteredPlan, "USING COVERING INDEX idx_catalog_gallery_projection_media_captured") ||
		!strings.Contains(filteredPlan, "USING PRIMARY KEY (captured_at=? AND canonical_asset_id=?)") {
		t.Fatalf("filtered OFFSET must stay index-only before fetching the bounded page:\n%s", filteredPlan)
	}
}

func assertGallerySeekIndex(t *testing.T, db *sql.DB) {
	t.Helper()
	var columns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_index_xinfo('idx_catalog_gallery_projection_captured')
		WHERE key = 1 AND coll = 'BINARY' AND ((seqno = 0 AND name = 'captured_at' AND desc = 1)
		OR (seqno = 1 AND name = 'canonical_asset_id' AND desc = 0))`).Scan(&columns); err != nil || columns != 2 {
		t.Fatalf("date/ID seek index: columns=%d error=%v", columns, err)
	}
	plan := explainGalleryProjectionQueryPlan(t, db, galleryProjectionAnchorWithinDaySQL,
		"2026-09-01T00:00:00Z", "2026-09-02T00:00:00Z", 3000)
	if !strings.Contains(plan, "USING COVERING INDEX idx_catalog_gallery_projection_captured") {
		t.Fatalf("day offset must skip narrow keys, not payload rows:\n%s", plan)
	}
}
