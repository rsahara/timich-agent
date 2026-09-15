//go:build linux

package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Opt-in rehearsal on disposable, already-copied catalogs only. Never point this
// tool at a live state directory. The directory prefix and snapshot filename are
// deliberate guardrails; no service is started and no live catalog is replaced.
func galleryScratchPaths(t *testing.T) (string, string, string) {
	t.Helper()
	root := os.Getenv("TIMICH_GALLERY_SCRATCH_DIR")
	if root == "" {
		t.Skip("requires an approved disposable catalog copy")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil || !strings.HasPrefix(filepath.Base(root), "gallery-layout-check.") {
		t.Fatalf("invalid scratch directory: %v", err)
	}
	return root, filepath.Join(root, "benchmark-v4.db"), filepath.Join(root, "benchmark-v5.db")
}

func openGalleryScratch(t *testing.T, path string, writable bool) *sql.DB {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("scratch database must be an existing regular file: %v", err)
	}
	mode := "ro"
	if writable {
		mode = "rw"
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=" + mode}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA cache_size=-16384; PRAGMA mmap_size=0; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return db
}

func TestGalleryScratchMigrate(t *testing.T) {
	_, before, after := galleryScratchPaths(t)
	started := time.Now()
	t.Log("starting offline migration of disposable V4 snapshot")
	result, err := MigratePreReleaseCatalogV4ToV5(context.Background(), before, after)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("migration elapsed=%s rows=%d", time.Since(started), result.GalleryRows)
	old := openGalleryScratch(t, before, false)
	defer old.Close()
	current := openGalleryScratch(t, after, false)
	defer current.Close()
	for _, table := range []string{"catalog_assets", "catalog_canonical_assets", "local_assets", "local_asset_locations", "local_renditions", "local_scan_jobs", "semantic_vectors", "semantic_index_membership", "canonical_semantic_vector_inputs"} {
		var a, b int64
		if err := old.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&a); err != nil {
			t.Fatal(err)
		}
		if err := current.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&b); err != nil || a != b {
			t.Fatalf("preserved table %s rows: %d != %d: %v", table, a, b, err)
		}
		t.Logf("preserved table=%s rows=%d", table, a)
	}
	assertGalleryProjectionDayIndexMatches(t, old)
	assertGalleryProjectionDayIndexMatches(t, current)
	for label, db := range map[string]*sql.DB{"v4": old, "v5": current} {
		var pages, free, size int64
		for q, dest := range map[string]*int64{"PRAGMA page_count": &pages, "PRAGMA freelist_count": &free, "PRAGMA page_size": &size} {
			if err := db.QueryRow(q).Scan(dest); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("storage %s file_bytes=%d free_bytes=%d", label, pages*size, free*size)
	}
}

type galleryScratchSample struct {
	Layout    string  `json:"layout"`
	Kind      string  `json:"kind"`
	Position  int     `json:"position"`
	Warm      bool    `json:"warm"`
	TotalMS   float64 `json:"total_ms"`
	AnchorMS  float64 `json:"anchor_ms"`
	PayloadMS float64 `json:"payload_ms"`
	ReadBytes int64   `json:"read_bytes"`
	Rows      int     `json:"rows"`
	Digest    string  `json:"digest"`
}

func galleryScratchReadBytes(t *testing.T) int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "read_bytes:") {
			v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "read_bytes:")), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatal("read_bytes is unavailable")
	return 0
}

// Per-file advice only, with no connections left open or dirty WAL pages.
// DONTNEED is advisory: physical read_bytes is reported to verify cache misses.
// Controller/drive caches and the rest of the host cache are not flushed.
func galleryScratchEvict(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED); err != nil {
		t.Fatal(err)
	}
}

func galleryScratchPage(t *testing.T, db *sql.DB, layout, kind string, position int, warm bool) galleryScratchSample {
	t.Helper()
	s := galleryScratchSample{Layout: layout, Kind: kind, Position: position, Warm: warm}
	ioBefore := galleryScratchReadBytes(t)
	started := time.Now()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var rows *sql.Rows
	var payloadStarted time.Time
	if kind == "video" {
		query := `SELECT ` + galleryProjectionSelectColumns + ` FROM catalog_gallery_projection
			WHERE media_type='video' ORDER BY captured_at DESC,canonical_asset_id ASC LIMIT ? OFFSET ?`
		if layout == "v5" {
			query = galleryProjectionFilteredPageSQL("WHERE media_type='video'")
		}
		payloadStarted = time.Now()
		rows, err = tx.Query(query, 61, position)
	} else {
		anchorStarted := time.Now()
		at, id, found, e := galleryProjectionAnchorAtOffset(ctx, tx, position)
		if e != nil || !found {
			t.Fatalf("anchor position %d found=%t: %v", position, found, e)
		}
		s.AnchorMS = float64(time.Since(anchorStarted).Microseconds()) / 1000
		query := galleryProjectionAfterAnchorSQL
		if layout == "v4" {
			query = `SELECT ` + galleryProjectionSelectColumns + ` FROM catalog_gallery_projection
			WHERE captured_at<=?1 AND (captured_at<?1 OR canonical_asset_id>=?2)
			ORDER BY captured_at DESC,canonical_asset_id ASC LIMIT ?3`
		}
		payloadStarted = time.Now()
		rows, err = tx.Query(query, at, id, 61)
	}
	if err != nil {
		t.Fatal(err)
	}
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
	_ = rows.Close()
	s.PayloadMS = float64(time.Since(payloadStarted).Microseconds()) / 1000
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.TotalMS = float64(time.Since(started).Microseconds()) / 1000
	s.ReadBytes = galleryScratchReadBytes(t) - ioBefore
	s.Rows = len(values)
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	s.Digest = fmt.Sprintf("%x", sha256.Sum256(raw))
	return s
}

func galleryScratchPercentile(values []float64, p float64) float64 {
	values = append([]float64(nil), values...)
	sort.Float64s(values)
	return values[int(math.Ceil(float64(len(values))*p))-1]
}

func galleryScratchDigest(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT canonical_asset_id,` + galleryProjectionSelectColumns + ` FROM catalog_gallery_projection ORDER BY captured_at DESC,canonical_asset_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for rows.Next() {
		var v [7]sql.NullString
		if err := rows.Scan(&v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6]); err != nil {
			t.Fatal(err)
		}
		if err := encoder.Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func TestGalleryScratchRead(t *testing.T) {
	_, before, after := galleryScratchPaths(t)
	db := openGalleryScratch(t, before, false)
	var total, videos, denseStart, denseCount int
	if err := db.QueryRow(galleryProjectionUnfilteredTotalSQL).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM catalog_gallery_projection WHERE media_type='video'`).Scan(&videos); err != nil {
		t.Fatal(err)
	}
	var day string
	if err := db.QueryRow(`SELECT captured_day,item_count FROM catalog_gallery_projection_days ORDER BY item_count DESC LIMIT 1`).Scan(&day, &denseCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COALESCE(SUM(item_count),0) FROM catalog_gallery_projection_days WHERE captured_day>?`, day).Scan(&denseStart); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if total < 61 || videos < 61 {
		t.Fatal("not enough Gallery rows for the configured workload")
	}
	type position struct {
		kind   string
		offset int
	}
	positions := []position{{"dense_day", denseStart + max(0, denseCount-61)}}
	rng := rand.New(rand.NewSource(20260906))
	for i := 0; i < 64; i++ {
		positions = append(positions, position{"timeline", rng.Intn((total-61)/60+1) * 60})
	}
	for i := 0; i < 20; i++ {
		positions = append(positions, position{"video", rng.Intn(videos - 60)})
	}
	var samples []galleryScratchSample
	for i, pos := range positions {
		layouts := []string{"v4", "v5"}
		if i%2 == 1 {
			layouts[0], layouts[1] = layouts[1], layouts[0]
		}
		var want string
		for _, layout := range layouts {
			path := before
			if layout == "v5" {
				path = after
			}
			galleryScratchEvict(t, path)
			db := openGalleryScratch(t, path, false)
			for _, warm := range []bool{false, true} {
				s := galleryScratchPage(t, db, layout, pos.kind, pos.offset, warm)
				if s.Rows != 61 {
					t.Fatalf("page size %d", s.Rows)
				}
				if want == "" {
					want = s.Digest
				} else if want != s.Digest {
					t.Fatalf("page mismatch kind=%s offset=%d", pos.kind, pos.offset)
				}
				samples = append(samples, s)
				raw, _ := json.Marshal(s)
				t.Log("GALLERY_SAMPLE", string(raw))
			}
			_ = db.Close()
		}
	}
	for _, kind := range []string{"timeline", "video", "dense_day"} {
		for _, layout := range []string{"v4", "v5"} {
			for _, warm := range []bool{false, true} {
				var elapsed, anchors, payloads, bytes []float64
				for _, s := range samples {
					if s.Kind == kind && s.Layout == layout && s.Warm == warm {
						elapsed = append(elapsed, s.TotalMS)
						anchors = append(anchors, s.AnchorMS)
						payloads = append(payloads, s.PayloadMS)
						bytes = append(bytes, float64(s.ReadBytes))
					}
				}
				raw, _ := json.Marshal(map[string]any{"kind": kind, "layout": layout, "warm": warm, "samples": len(elapsed), "p50_ms": galleryScratchPercentile(elapsed, .5), "p95_ms": galleryScratchPercentile(elapsed, .95), "anchor_p95_ms": galleryScratchPercentile(anchors, .95), "payload_p95_ms": galleryScratchPercentile(payloads, .95), "read_bytes_p50": galleryScratchPercentile(bytes, .5), "read_bytes_p95": galleryScratchPercentile(bytes, .95)})
				t.Log("GALLERY_SUMMARY", string(raw))
			}
		}
	}
}

func TestGalleryScratchAnchorPhases(t *testing.T) {
	_, before, after := galleryScratchPaths(t)
	positions := []int{163860, 250620, 218760, 139080, 17520, 184260, 291360, 273300}
	for _, layout := range []string{"v4", "v5"} {
		path := before
		if layout == "v5" {
			path = after
		}
		for _, position := range positions {
			galleryScratchEvict(t, path)
			db := openGalleryScratch(t, path, false)
			var day, at, id string
			var start int64
			ioStart := galleryScratchReadBytes(t)
			timer := time.Now()
			if err := db.QueryRow(galleryProjectionDayAtOffsetSQL, position, position).Scan(&day, &start); err != nil {
				t.Fatal(err)
			}
			dayMS := float64(time.Since(timer).Microseconds()) / 1000
			ioDay := galleryScratchReadBytes(t)
			date, err := time.Parse("2006-01-02", day)
			if err != nil {
				t.Fatal(err)
			}
			timer = time.Now()
			if err := db.QueryRow(galleryProjectionAnchorWithinDaySQL, formatCatalogTime(date), formatCatalogTime(date.AddDate(0, 0, 1)), int64(position)-start).Scan(&at, &id); err != nil {
				t.Fatal(err)
			}
			withinMS := float64(time.Since(timer).Microseconds()) / 1000
			ioWithin := galleryScratchReadBytes(t)
			var count int
			if err := db.QueryRow(`SELECT item_count FROM catalog_gallery_projection_days WHERE captured_day=?`, day).Scan(&count); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			raw, _ := json.Marshal(map[string]any{"layout": layout, "position": position, "day_ms": dayMS, "within_day_ms": withinMS, "day_read_bytes": ioDay - ioStart, "within_read_bytes": ioWithin - ioDay, "day_count": count, "day_offset": int64(position) - start})
			t.Log("GALLERY_ANCHOR", string(raw))
		}
	}
}

// This mutates only the two disposable copies. They must never be installed as
// production catalogs after the benchmark. It exercises canonical-update/FTS/
// Gallery triggers and fsyncs, not the entire filesystem Metadata pipeline.
func TestGalleryScratchWrite(t *testing.T) {
	root, before, after := galleryScratchPaths(t)
	marker, err := os.OpenFile(filepath.Join(root, "write-benchmark-started"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_ = marker.Close()
	old := openGalleryScratch(t, before, false)
	type item struct{ id, at string }
	const batches = 20
	const batchSize = 48
	const sampleSize = batches * batchSize
	rng := rand.New(rand.NewSource(43))
	seen := 0
	var candidates []item
	rows, err := old.Query(`SELECT canonical_asset_id,captured_at FROM catalog_gallery_projection ORDER BY captured_at DESC,canonical_asset_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.id, &v.at); err != nil {
			t.Fatal(err)
		}
		if seen < sampleSize {
			candidates = append(candidates, v)
		} else if j := rng.Intn(seen + 1); j < sampleSize {
			candidates[j] = v
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if len(candidates) < batches*batchSize {
		t.Fatal("insufficient rows")
	}
	rng.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	_ = old.Close()
	// Sampling must not leave one layout with a warm cache advantage. Start
	// both write workloads from equivalent fresh connection/file-cache states.
	galleryScratchEvict(t, before)
	galleryScratchEvict(t, after)
	old = openGalleryScratch(t, before, true)
	defer old.Close()
	current := openGalleryScratch(t, after, true)
	defer current.Close()
	dbs := map[string]*sql.DB{"v4": old, "v5": current}
	for _, db := range dbs {
		if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA wal_autocheckpoint=0; PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			t.Fatal(err)
		}
	}
	timings := map[string][]float64{}
	for batch := 0; batch < batches; batch++ {
		layouts := []string{"v4", "v5"}
		if batch%2 == 1 {
			layouts[0], layouts[1] = layouts[1], layouts[0]
		}
		for _, layout := range layouts {
			started := time.Now()
			tx, err := dbs[layout].Begin()
			if err != nil {
				t.Fatal(err)
			}
			stmt, err := tx.Prepare(`UPDATE catalog_canonical_assets SET filename=filename||'.validation',captured_at=? WHERE canonical_asset_id=?`)
			if err != nil {
				t.Fatal(err)
			}
			for j := 0; j < batchSize; j++ {
				v := candidates[batch*batchSize+j]
				if j%10 == 0 {
					at, err := time.Parse(time.RFC3339Nano, v.at)
					if err != nil {
						t.Fatal(err)
					}
					v.at = formatCatalogTime(at.AddDate(0, 0, 1))
				}
				if _, err := stmt.Exec(v.at, v.id); err != nil {
					t.Fatal(err)
				}
			}
			_ = stmt.Close()
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			elapsed := float64(time.Since(started).Microseconds()) / 1000
			timings[layout] = append(timings[layout], elapsed)
			t.Logf("GALLERY_WRITE_SAMPLE layout=%s batch=%d rows=%d ms=%.3f", layout, batch, batchSize, elapsed)
		}
	}
	for label, db := range dbs {
		path := before
		if label == "v5" {
			path = after
		}
		info, err := os.Stat(path + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"layout": label, "batches": batches, "rows_per_batch": batchSize, "p50_ms": galleryScratchPercentile(timings[label], .5), "p95_ms": galleryScratchPercentile(timings[label], .95), "wal_bytes": info.Size()})
		t.Log("GALLERY_WRITE_SUMMARY", string(raw))
		assertGalleryProjectionDayIndexMatches(t, db)
		if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE); PRAGMA journal_mode=DELETE`); err != nil {
			t.Fatal(err)
		}
	}
	if galleryScratchDigest(t, old) != galleryScratchDigest(t, current) {
		t.Fatal("updated Gallery rows differ")
	}
}
