package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
)

func TestDatasourceMirrorDailyScheduleDefaultsAndConfiguredClocks(t *testing.T) {
	t.Parallel()
	location := time.FixedZone("JST", 9*60*60)
	now := time.Date(2026, 9, 21, 1, 0, 0, 0, location)
	datasource := config.DatasourceConfig{Kind: config.DatasourceKindImmichIndexed}
	runtime := &AgentRuntime{config: config.ResolvedConfig{Config: config.Config{
		Timezone: "Asia/Tokyo", Datasources: []config.DatasourceConfig{datasource},
	}}}
	for _, indexing := range []*config.DatasourceIndexingConfig{nil, {}} {
		runtime.config.Datasources[0].Indexing = indexing
		schedule, ok := runtime.datasourceMirrorScheduleAt(now)
		if !ok || schedule.Interval != 30*time.Minute || schedule.DailyFullSweepWindow != "02:00" {
			t.Fatalf("default schedule = %+v, enabled=%v", schedule, ok)
		}
	}
	runtime.config.Datasources = []config.DatasourceConfig{
		{Kind: config.DatasourceKindImmichIndexed, Indexing: &config.DatasourceIndexingConfig{Phase0SyncInterval: "1h", DailyFullSweepWindow: "05:00"}},
		{Kind: config.DatasourceKindImmichIndexed, Indexing: &config.DatasourceIndexingConfig{Phase0SyncInterval: "45m", DailyFullSweepWindow: "02:00"}},
	}
	for _, test := range []struct {
		hour int
		want string
	}{{1, "02:00"}, {3, "05:00"}, {6, "02:00"}} {
		schedule, ok := runtime.datasourceMirrorScheduleAt(time.Date(2026, 9, 21, test.hour, 0, 0, 0, location))
		if !ok || schedule.Interval != 45*time.Minute || schedule.DailyFullSweepWindow != test.want {
			t.Fatalf("schedule at %d:00 = %+v, enabled=%v; want next clock %s", test.hour, schedule, ok, test.want)
		}
	}
}

func TestDatasourceMirrorReconciliationCatchesUpAndRetriesWithoutRepeating(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const sourceKey = "1111111111111111"
	var removed, fail atomic.Bool
	var fullRequests, incrementalRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/server/ping" {
			fmt.Fprint(w, `{"res":"pong"}`)
			return
		}
		if r.URL.Path != "/api/search/metadata" {
			http.NotFound(w, r)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var items []map[string]any
		if request["updatedAfter"] == nil {
			fullRequests.Add(1)
			ids := []string{"stable"}
			if !removed.Load() {
				ids = append(ids, "no-longer-listed")
			}
			for _, id := range ids {
				items = append(items, map[string]any{"id": id, "type": "IMAGE", "originalFileName": id + ".jpg", "visibility": "timeline", "fileCreatedAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z"})
			}
		} else {
			incrementalRequests.Add(1)
		}
		if fail.Load() {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"assets": map[string]any{"items": items, "total": len(items), "nextPage": nil}})
	}))
	defer upstream.Close()
	runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: sourceKey, Kind: config.DatasourceKindImmichIndexed, URL: upstream.URL, AccessToken: "test-key",
	}}, "test-admin-token", func(cfg *config.ResolvedConfig) { cfg.Timezone = "Asia/Tokyo" })
	if _, err := runtime.RunDatasourceIndexing(ctx, DatasourceIndexingRunOptions{Mode: "full"}); err != nil {
		t.Fatal(err)
	}
	if got := runtime.datasourceMirrorSyncModeForSource(ctx, sourceKey, time.Now()); got != catalog.MirrorSyncModeIncremental {
		t.Fatalf("mode after manual reconciliation = %q, want incremental", got)
	}
	// Persist a missed successful occurrence, as after the Agent was offline.
	db, err := sql.Open("sqlite", filepath.Join(runtime.config.DataDir, "catalog-state-v1", "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := db.Exec(`UPDATE immich_mirror_state SET last_full_sync_at = ? WHERE source_key = ?`, old.Format(time.RFC3339Nano), sourceKey); err != nil {
		t.Fatal(err)
	}
	removed.Store(true)
	fail.Store(true)
	runtime.runScheduledDatasourceMirrorSync(ctx, "startup")
	status, err := runtime.PrimaryDatasourceMirrorStatus(ctx)
	if err != nil || status.ActiveCount != 2 || status.LastFullSyncAt == nil || !status.LastFullSyncAt.Equal(old) {
		t.Fatalf("failed reconciliation altered catalog/completion: %+v, error=%v", status, err)
	}
	fail.Store(false)
	runtime.runScheduledDatasourceMirrorSync(ctx, "interval")
	status, err = runtime.PrimaryDatasourceMirrorStatus(ctx)
	if err != nil || status.ActiveCount != 1 || status.LastFullSyncAt == nil || !status.LastFullSyncAt.After(old) {
		t.Fatalf("retry did not reconcile missing ID: %+v, error=%v", status, err)
	}
	completed := *status.LastFullSyncAt
	fullCount := fullRequests.Load()
	for _, reason := range []string{"startup", "interval", "daily_full_sweep"} {
		runtime.runScheduledDatasourceMirrorSync(ctx, reason)
	}
	if fullCount != 3 || fullRequests.Load() != fullCount || incrementalRequests.Load() != 9 {
		t.Fatalf("requests full=%d incremental=%d; want three full attempts, then ordinary deltas", fullRequests.Load(), incrementalRequests.Load())
	}
	admin, err := runtime.DatasourceIndexingStatus(ctx)
	if err != nil || len(admin.Datasources) != 1 || admin.Datasources[0].LastReconciliationAt == nil || !admin.Datasources[0].LastReconciliationAt.Equal(completed) {
		t.Fatalf("reconciliation status not retained after incremental sync: %+v, error=%v", admin.Datasources, err)
	}
	if got := runtime.datasourceMirrorSyncModeForSource(ctx, sourceKey, completed.Add(48*time.Hour)); got != catalog.MirrorSyncModeFull {
		t.Fatalf("next missed day mode = %q, want full", got)
	}
	// Due checks use this source's clock in the Agent timezone, including equality.
	location := time.FixedZone("JST", 9*60*60)
	completed = time.Date(2026, 9, 21, 1, 0, 0, 0, location)
	if _, err := db.Exec(`UPDATE immich_mirror_state SET last_full_sync_at = ? WHERE source_key = ?`, completed.UTC().Format(time.RFC3339Nano), sourceKey); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		clock string
		hour  int
		want  string
	}{{"", 1, catalog.MirrorSyncModeIncremental}, {"", 2, catalog.MirrorSyncModeFull}, {"05:00", 2, catalog.MirrorSyncModeIncremental}, {"05:00", 5, catalog.MirrorSyncModeFull}} {
		runtime.mu.Lock()
		runtime.config.Datasources[0].Indexing = &config.DatasourceIndexingConfig{DailyFullSweepWindow: test.clock}
		runtime.mu.Unlock()
		now := time.Date(2026, 9, 21, test.hour, 0, 0, 0, location)
		if got := runtime.datasourceMirrorSyncModeForSource(ctx, sourceKey, now); got != test.want {
			t.Fatalf("clock %q at %d:00 JST: mode=%q, want %q", test.clock, test.hour, got, test.want)
		}
	}
}
