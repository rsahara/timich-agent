package runtime

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
)

func TestDatasourceRefreshShutdownCancelsForegroundWait(t *testing.T) {
	a := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{{
		SourceKey: "1111111111111111", Name: "Home Immich", Kind: config.DatasourceKindImmichIndexed,
		URL: "http://immich.local:2283", AccessToken: "test",
	}})
	if !a.datasourceIndexingReconciliationReady(context.Background()) {
		t.Fatal("fixture not ready for reconciliation")
	}
	a.foregroundCatalog.quietPeriod = time.Nanosecond
	unblock := a.foregroundCatalog.begin()
	entered, done := make(chan struct{}), make(chan struct{})
	if !a.background.Go(func(ctx context.Context) {
		defer close(done)
		close(entered)
		a.runDatasourceIndexingSnapshotRefresh(ctx, a.catalogService(), 0)
	}) {
		t.Fatal("background admission failed")
	}
	defer func() { unblock(); <-done }()
	<-entered
	select {
	case <-done:
		t.Fatal("refresh did not wait for foreground work")
	case <-time.After(20 * time.Millisecond):
	}
	a.BeginShutdown()
	wait, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.WaitStopped(wait); err != nil {
		t.Fatalf("refresh kept waiting for foreground work after shutdown: %v", err)
	}
	if a.foregroundCatalog.active.Load() != 1 {
		t.Fatal("foreground work must still be held when refresh returns")
	}
	a.datasourceTaskMu.Lock()
	busy := a.datasourceSnapshotBusy
	a.datasourceTaskMu.Unlock()
	if busy {
		t.Fatal("canceled refresh retained busy state")
	}
}

func TestDatasourceCanceledRefreshPreservesSnapshot(t *testing.T) {
	for _, test := range []struct{ name, kind, operation string }{
		{"indexed_refresh", config.DatasourceKindImmichIndexed, "refresh"},
		{"indexed_task_stats", config.DatasourceKindImmichIndexed, "task_stats"},
		{"passthrough_refresh", config.DatasourceKindImmich, "refresh"},
		{"passthrough_task_stats", config.DatasourceKindImmich, "task_stats"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := newTestAgentRuntime(t, BuildInfo{}, []config.DatasourceConfig{{
				SourceKey: "1111111111111111", Name: "Home Immich", Kind: test.kind,
				URL: "http://immich.local:2283", AccessToken: "test",
			}})
			service := a.catalogService()
			good := a.emptyDatasourceIndexingResponse()
			if !a.rememberDatasourceIndexingSnapshotAt(service, good, time.Now().Add(-time.Hour)) {
				t.Fatal("seed snapshot failed")
			}
			before, ok := a.CachedDatasourceIndexingStatus(context.Background())
			if !ok {
				t.Fatal("seed snapshot missing")
			}
			storedBefore, ok, err := service.AdminStatusSnapshot(context.Background(), datasourceIndexingStatusSnapshotKey)
			persisted := test.kind == config.DatasourceKindImmichIndexed
			if err != nil || ok != persisted {
				t.Fatalf("seed persisted snapshot: ok=%v err=%v", ok, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			switch test.operation {
			case "refresh":
				// A passthrough build can return nil error even after cancellation.
				if _, err := a.refreshDatasourceIndexingSnapshot(ctx, service); err != nil {
					t.Fatalf("last successful snapshot should remain available: %v", err)
				}
			case "task_stats":
				a.rememberDatasourceTaskStatsSnapshot(ctx, service, catalog.AssetProcessingStatsSnapshot{
					RefreshedAt: time.Now(), Stats: []catalog.AssetProcessingStat{{Stage: catalog.AssetProcessingStageMetadata}},
				})
			}
			after, ok := a.CachedDatasourceIndexingStatus(context.Background())
			if !ok || !reflect.DeepEqual(before, after) {
				t.Fatalf("cancellation replaced cached snapshot: before=%+v after=%+v", before, after)
			}
			storedAfter, ok, err := service.AdminStatusSnapshot(context.Background(), datasourceIndexingStatusSnapshotKey)
			if err != nil || ok != persisted || !reflect.DeepEqual(storedBefore, storedAfter) {
				t.Fatalf("cancellation changed persisted snapshot: ok=%v err=%v", ok, err)
			}
		})
	}
}
