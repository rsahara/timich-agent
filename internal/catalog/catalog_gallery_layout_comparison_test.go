package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"
)

// Opt-in structural comparison with the exact Agent SQLite driver. It only
// creates disposable synthetic databases; it cannot open an existing catalog.
// Run with TIMICH_GALLERY_LAYOUT_ROWS=363570 go test ... -run TestGalleryLayoutComparison -v.
func TestGalleryLayoutComparison(t *testing.T) {
	value := os.Getenv("TIMICH_GALLERY_LAYOUT_ROWS")
	if value == "" {
		t.Skip("opt-in synthetic layout comparison")
	}
	count, err := strconv.Atoi(value)
	if err != nil || count < 4096 {
		t.Fatal("TIMICH_GALLERY_LAYOUT_ROWS must be >=4096")
	}
	type row struct {
		id, source, upstream, kind, name, at string
		duration                             sql.NullString
	}
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	original := make([]row, count)
	for i := range original {
		id := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("canonical-%d", i))))
		upstream := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("source-%d", i))))
		date := base.AddDate(0, 0, -1-(i-3319)/64).Add(-time.Duration(i%64/4) * time.Second)
		if i < 3319 {
			date = base.Add(-time.Duration(i/4) * time.Second)
		}
		r := row{id: id[:32], source: "1111111111111111", upstream: upstream[:36], kind: "image", name: fmt.Sprintf("IMG_%07d_sample.jpg", i), at: formatCatalogTime(date)}
		if i%8 == 0 {
			r.kind = "video"
			r.duration = sql.NullString{String: "00:00:12.345", Valid: true}
		}
		original[i] = r
	}
	order := func(rows []row) {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].at != rows[j].at {
				return rows[i].at > rows[j].at
			}
			return rows[i].id < rows[j].id
		})
	}
	p95 := func(values []float64) float64 { sort.Float64s(values); return values[int(float64(len(values)-1)*.95)] }
	for _, layout := range []string{"current", "covering", "clustered", "clustered_seek"} {
		t.Run(layout, func(t *testing.T) {
			clustered := layout == "clustered" || layout == "clustered_seek"
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "gallery.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := db.Exec(q, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA wal_autocheckpoint=0; PRAGMA cache_size=-16384`)
			table := galleryV4TableForTest
			if clustered {
				table = galleryProjectionTableSQL
			}
			exec(table)
			exec(`CREATE TABLE catalog_gallery_projection_days(captured_day TEXT PRIMARY KEY, item_count INTEGER NOT NULL CHECK(item_count>0))`)
			exec(galleryProjectionDayInsertTriggerSQL())
			exec(galleryProjectionDayDeleteTriggerSQL())
			exec(`CREATE INDEX idx_catalog_gallery_projection_media_captured ON catalog_gallery_projection(media_type,captured_at DESC,canonical_asset_id)`)
			if !clustered || layout == "clustered_seek" {
				trailing := ""
				if layout == "covering" {
					trailing = ",source_key,upstream_asset_id,media_type,filename,duration"
				}
				exec(`CREATE INDEX idx_catalog_gallery_projection_captured ON catalog_gallery_projection(captured_at DESC,canonical_asset_id` + trailing + `)`)
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			const insert = `INSERT INTO catalog_gallery_projection(canonical_asset_id,` + galleryProjectionSelectColumns + `) VALUES(?,?,?,?,?,?,?)`
			stmt, err := tx.Prepare(insert)
			if err != nil {
				t.Fatal(err)
			}
			seedStarted := time.Now()
			for _, i := range rand.New(rand.NewSource(23)).Perm(count) {
				r := original[i]
				if _, err := stmt.Exec(r.id, r.source, r.upstream, r.kind, r.name, r.at, r.duration); err != nil {
					t.Fatal(err)
				}
			}
			_ = stmt.Close()
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
			seedMS := float64(time.Since(seedStarted).Microseconds()) / 1000
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			query := galleryProjectionAfterAnchorSQL
			if !clustered {
				query = `SELECT ` + galleryProjectionSelectColumns + ` FROM catalog_gallery_projection
				WHERE captured_at<=?1 AND (captured_at<?1 OR canonical_asset_id>=?2)
				ORDER BY captured_at DESC,canonical_asset_id ASC LIMIT ?3`
			}
			current := append([]row(nil), original...)
			order(current)
			var videos []row
			for _, r := range current {
				if r.kind == "video" {
					videos = append(videos, r)
				}
			}
			filteredQuery := `SELECT ` + galleryProjectionSelectColumns + ` FROM catalog_gallery_projection
				WHERE media_type='video' ORDER BY captured_at DESC,canonical_asset_id ASC LIMIT ? OFFSET ?`
			if clustered {
				filteredQuery = galleryProjectionFilteredPageSQL("WHERE media_type='video'")
			}
			t.Log("filtered plan:", explainGalleryProjectionQueryPlan(t, db, filteredQuery, 61, count/16))
			readPage := func(position int, filtered bool) float64 {
				t.Helper()
				start := time.Now()
				tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				var rows *sql.Rows
				var expected []row
				if filtered {
					expected = videos
					rows, err = tx.Query(filteredQuery, 61, position)
				} else {
					expected = current
					at, id, found, e := galleryProjectionAnchorAtOffset(ctx, tx, position)
					if e != nil || !found {
						t.Fatalf("anchor %d: %v %t", position, e, found)
					}
					rows, err = tx.Query(query, at, id, 61)
				}
				if err != nil {
					t.Fatal(err)
				}
				got := 0
				for rows.Next() {
					var r row
					if err := rows.Scan(&r.source, &r.upstream, &r.kind, &r.name, &r.at, &r.duration); err != nil {
						t.Fatal(err)
					}
					want := expected[position+got]
					want.id = ""
					if r != want {
						t.Fatalf("page mismatch at %d", position+got)
					}
					got++
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				_ = rows.Close()
				_ = tx.Commit()
				if got != 61 {
					t.Fatalf("page length = %d", got)
				}
				return float64(time.Since(start).Microseconds()) / 1000
			}
			var cacheCleared, warm, filtered []float64
			rng := rand.New(rand.NewSource(42))
			positions := make([]int, 64)
			for i := range positions {
				positions[i] = rng.Intn(count - 61)
				exec(`PRAGMA shrink_memory`)
				cacheCleared = append(cacheCleared, readPage(positions[i], false))
				warm = append(warm, readPage(positions[i], false))
			}
			exec(`PRAGMA shrink_memory`)
			denseMS := readPage(3250, false)
			// Actual filtered OFFSET path, including deep filtered positions.
			for i := 0; i < 16; i++ {
				exec(`PRAGMA shrink_memory`)
				filtered = append(filtered, readPage(rng.Intn(count/8-61), true))
			}
			exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
			var writes []float64
			updateIndices := rand.New(rand.NewSource(43)).Perm(count)[:2400]
			for batch := 0; batch < 50; batch++ {
				start := time.Now()
				tx, err := db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				del, err := tx.Prepare(`DELETE FROM catalog_gallery_projection WHERE canonical_asset_id=?`)
				if err != nil {
					t.Fatal(err)
				}
				ins, err := tx.Prepare(insert)
				if err != nil {
					t.Fatal(err)
				}
				for k := 0; k < 48; k++ {
					i := updateIndices[batch*48+k]
					r := current[i]
					if i%10 == 0 {
						r.at = "2026-09-05T00:00:00Z"
					}
					r.name += ".updated"
					if _, err := del.Exec(r.id); err != nil {
						t.Fatal(err)
					}
					if _, err := ins.Exec(r.id, r.source, r.upstream, r.kind, r.name, r.at, r.duration); err != nil {
						t.Fatal(err)
					}
					current[i] = r
				}
				_ = del.Close()
				_ = ins.Close()
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				writes = append(writes, float64(time.Since(start).Microseconds())/1000)
			}
			wal, err := os.Stat(path + "-wal")
			if err != nil {
				t.Fatal(err)
			}
			order(current)
			var after []float64
			for _, position := range positions {
				exec(`PRAGMA shrink_memory`)
				after = append(after, readPage(position, false))
			}
			assertGalleryProjectionDayIndexMatches(t, db)
			if err := checkGalleryMigrationIntegrity(ctx, db); err != nil {
				t.Fatal(err)
			}
			var version string
			if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			payload, _ := json.Marshal(map[string]any{
				"layout": layout, "sqlite": version, "rows": count, "environment": "synthetic; SQLite cache cleared per sample; OS cache NOT evicted; not NAS/API/Metadata throughput",
				"database_mib": float64(info.Size()) / (1024 * 1024), "seed_ms": seedMS, "page_p95_ms": p95(cacheCleared), "warm_page_p95_ms": p95(warm),
				"dense_day_page_ms": denseMS, "filtered_page_p95_ms": p95(filtered), "refresh_48_p95_ms": p95(writes),
				"wal_mib": float64(wal.Size()) / (1024 * 1024), "after_update_page_p95_ms": p95(after),
			})
			t.Log(string(payload))
		})
	}
}
