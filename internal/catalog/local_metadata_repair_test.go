package catalog

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func seedLocalMetadataRepairLibrary(t testing.TB, count int) *Service {
	t.Helper()
	s := newLocalVideoMetadataTestService(t, t.TempDir(), writeCaptureMetadataHelper(t, nil))
	ctx := context.Background()
	if _, err := s.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
		t.Fatal(err)
	}
	now := formatCatalogTime(time.Now().UTC())
	if _, err := s.catalog.db.ExecContext(ctx, `WITH RECURSIVE assets(n) AS (
        SELECT 1 UNION ALL SELECT n+1 FROM assets WHERE n < ?
    ) INSERT INTO local_assets (
        source_key,asset_id,sha1_hex,content_size_bytes,media_type,filename,
        captured_at,captured_at_source,visibility_status,thumbnail_status,first_seen_at,updated_at
    ) SELECT '1111111111111111',printf('asset-%06d',n),printf('%040x',n),n,'image',
        printf('%06d.png',n),?,'file_mtime','active','ready',?,? FROM assets`, count, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.catalog.db.ExecContext(ctx, `INSERT INTO local_asset_locations (
        source_key,asset_id,root_key,relative_path,size_bytes,mtime,fast_signature,
        status,first_seen_at,last_seen_at,updated_at
    ) SELECT source_key,asset_id,'nas-photos',filename,content_size_bytes,captured_at,'sig',
        'active',first_seen_at,first_seen_at,updated_at FROM local_assets;
    INSERT INTO local_scan_jobs (
        source_key,job_kind,priority,root_key,root_generation,location_id,status,scheduled_at,sort_at
    ) SELECT l.source_key,'metadata',0,l.root_key,r.root_generation,l.id,'queued',l.mtime,l.mtime
        FROM local_asset_locations l JOIN local_scan_root_state r
        ON r.source_key=l.source_key AND r.root_key=l.root_key;`); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLocalMetadataRepairQueuedLibrary(t *testing.T) {
	const count = 16000
	s := seedLocalMetadataRepairLibrary(t, count)
	// Test the production predicate, including the queue planner before and
	// after ANALYZE. Neither lookup may walk all jobs in the datasource.
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			if _, err := s.catalog.db.Exec(`ANALYZE`); err != nil {
				t.Fatal(err)
			}
		}
		plan := localScanQueryPlan(t, s.catalog.db, `SELECT target.asset_id FROM local_assets target
            WHERE EXISTS (`+localMetadataRepairActiveJobQuery+`)`, localMetadataJobKind)
		for _, lookup := range []string{
			"idx_local_asset_locations_asset_status (source_key=? AND asset_id=?)",
			"idx_local_scan_jobs_location_pending (source_key=? AND job_kind=? AND location_id=? AND status=?",
		} {
			if !strings.Contains(plan, lookup) {
				t.Fatalf("missing asset-scoped lookup %s:\n%s", lookup, plan)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		started := time.Now()
		result, err := s.RepairLocalMetadata(ctx)
		cancel()
		if err != nil || result.Queued != 0 {
			t.Fatalf("repeat repair: %+v, %v", result, err)
		}
		t.Logf("repeat repair of %d queued assets (analyzed=%t): %s", count, analyzed, time.Since(started))
	}
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind='metadata'`, count)
}

func TestLocalMetadataRepairOutstandingJobsAcrossLocations(t *testing.T) {
	s := seedLocalMetadataRepairLibrary(t, 6)
	// Asset 1/2 have pending work on another copy; asset 3 has a stale
	// generation; 4 has only completed work; 5/6 have failed jobs to reuse.
	if _, err := s.catalog.db.Exec(`INSERT INTO local_asset_locations (
        source_key,asset_id,root_key,relative_path,size_bytes,mtime,fast_signature,
        status,first_seen_at,last_seen_at,updated_at
    ) SELECT source_key,asset_id,root_key,'copy-'||relative_path,size_bytes,mtime,fast_signature,
        status,first_seen_at,last_seen_at,updated_at FROM local_asset_locations WHERE id IN (1,2);
    UPDATE local_scan_jobs SET location_id=7 WHERE location_id=1;
    UPDATE local_scan_jobs SET location_id=8,status='running' WHERE location_id=2;
    UPDATE local_scan_jobs SET root_generation=root_generation+1 WHERE location_id=3;
    UPDATE local_scan_jobs SET status='completed' WHERE location_id=4;
    UPDATE local_scan_jobs SET status='failed' WHERE location_id IN (5,6);
    INSERT INTO local_scan_jobs (source_key,job_kind,priority,root_key,root_generation,location_id,status,scheduled_at,sort_at)
    SELECT source_key,'metadata',priority,root_key,root_generation,1,'failed',scheduled_at,sort_at
        FROM local_scan_jobs WHERE location_id=7;`); err != nil {
		t.Fatal(err)
	}
	result, err := s.RepairLocalMetadata(context.Background())
	if err != nil || result.CaptureDateQueued != 4 || result.Queued != 4 {
		t.Fatalf("repair across copies: %+v, %v", result, err)
	}
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_scan_jobs WHERE status='failed'`, 0)
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_scan_jobs WHERE location_id IN (1,2)`, 0)
	assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_scan_jobs WHERE location_id IN (5,6)`, 2)
	repeated, err := s.RepairLocalMetadata(context.Background())
	if err != nil || repeated.Queued != 0 {
		t.Fatalf("repeat: %+v, %v", repeated, err)
	}
}

func BenchmarkLocalMetadataRepairQueuedLibrary(b *testing.B) {
	for _, count := range []int{4000, 8000, 16000, 300000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := seedLocalMetadataRepairLibrary(b, count)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, err := s.RepairLocalMetadata(context.Background())
				if err != nil || result.Queued != 0 {
					b.Fatalf("repeat repair: %+v, %v", result, err)
				}
			}
		})
	}
}
