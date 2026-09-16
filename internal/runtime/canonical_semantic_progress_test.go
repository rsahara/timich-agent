package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
)

func TestCanonicalSemanticProgressUpdatesTasksWithoutChangingDatasourceCounts(t *testing.T) {
	runtime := newTestAgentRuntimeWithAdminToken(t, BuildInfo{}, nil, "test-admin-token")
	runtime.rememberDatasourceIndexingSnapshot(nil, DatasourceIndexingResponse{
		Tasks: []DatasourceTaskStatus{
			{Phase: "metadata", TotalTasks: 200, QueuedTasks: 100},
			{Phase: "embeddings", TotalTasks: 100, CompletedTasks: 20, QueuedTasks: 80},
			{Phase: "search_index", TotalTasks: 20, CompletedTasks: 0, QueuedTasks: 20},
		},
		Datasources: []DatasourceIndexingStatus{{SourceKey: "1111111111111111", IngestionKind: datasourceIngestionRemoteAPI, EmbeddingCompleted: 20, EmbeddingIndexed: 0, EmbeddingEligible: 100}},
	})
	progress := catalog.SemanticModelBackfillStatus{Status: catalog.SemanticBackfillStatusBackfilling, ModelID: "model", VectorSpaceID: "model/d4", EligibleAssetCount: 100, CompletedVectorCount: 60, IndexedVectorCount: 50, RemainingVectorCount: 40}
	runtime.rememberSemanticIndexingProgressSnapshot(nil, []catalog.SemanticBackfillSource{{SourceKey: "__timich_canonical__", Status: progress}}, progress)
	snapshot, ok := runtime.datasourceIndexingSnapshot(context.Background(), nil)
	if !ok {
		t.Fatal("missing progress snapshot")
	}
	seen := map[string]bool{}
	for _, task := range snapshot.Tasks {
		seen[task.Phase] = true
		switch task.Phase {
		case "embeddings":
			if task.CompletedTasks != 60 || task.QueuedTasks != 40 {
				t.Fatalf("embedding progress = %+v", task)
			}
		case "search_index":
			if task.CompletedTasks != 50 || task.QueuedTasks != 10 {
				t.Fatalf("index progress = %+v", task)
			}
		case "metadata":
			if task.TotalTasks != 200 || task.QueuedTasks != 100 {
				t.Fatalf("metadata changed: %+v", task)
			}
		}
	}
	for _, phase := range []string{"metadata", "embeddings", "search_index"} {
		if !seen[phase] {
			t.Fatalf("missing task phase %s", phase)
		}
	}
	if len(snapshot.Datasources) != 1 || snapshot.Datasources[0].EmbeddingCompleted != 20 {
		t.Fatalf("canonical total assigned to a source: %+v", snapshot.Datasources)
	}
}

func TestCanonicalPublishKeepsCompletedTasksAfterSourceRemoval(t *testing.T) {
	for _, priority := range []bool{false, true} {
		t.Run(map[bool]string{false: "scheduled", true: "priority"}[priority], func(t *testing.T) {
			ctx := context.Background()
			helperPath := writeRuntimeCandidateSelectionHelper(t)
			jpeg := encodeRuntimeJPEGForTest(t, 32, 32)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/search/metadata":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"assets":{"total":1,"items":[{"id":"asset","type":"IMAGE","originalFileName":"asset.jpg","fileCreatedAt":"2026-06-01T10:00:00Z","updatedAt":"2026-06-01T10:05:00Z"}],"nextPage":null}}`)
				case "/api/assets/asset/thumbnail":
					w.Header().Set("Content-Type", "image/jpeg")
					_, _ = w.Write(jpeg)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			sources := []config.DatasourceConfig{
				{SourceKey: "1111111111111111", Kind: config.DatasourceKindImmichIndexed, URL: server.URL, AccessToken: "test"},
				{SourceKey: "2222222222222222", Kind: config.DatasourceKindImmichIndexed, URL: server.URL, AccessToken: "test"},
			}
			runtime := newTestAgentRuntimeWithConfig(t, BuildInfo{}, sources, "test-admin-token", func(cfg *config.ResolvedConfig) {
				cfg.SemanticRuntime.HelperPath = helperPath
				cfg.WorkerRuntime.HeavyTaskWorkers = runtimeTestIntPtr(1)
			})
			service := runtime.catalogService()
			for _, source := range sources {
				if _, err := service.SyncDatasourceMirror(ctx, source.SourceKey, catalog.MirrorSyncModeFull); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.ReconfigureDatasources(sources[:1]); err != nil {
				t.Fatal(err)
			}
			runtime.mu.Lock()
			runtime.config.Datasources = sources[:1]
			runtime.mu.Unlock()
			installRuntimeSemanticPackForTest(t, runtime, runtimeSemanticPackForTest("zzz-new-model"))
			registry := runtime.SemanticModelRegistryStatusWithContext(ctx)
			if registry.Candidate == nil {
				t.Fatal("missing candidate")
			}
			backfill, err := service.BackfillSemanticModelCandidateWithOptions(ctx, runtime.semanticModels, *registry.Candidate, catalog.SemanticModelBackfillOptions{MaxAssets: 10})
			if err != nil || backfill.Status.CompletedVectorCount != 1 || backfill.Status.RemainingVectorCount != 0 {
				t.Fatalf("backfill = %+v, %v", backfill, err)
			}
			runtime.rememberDatasourceIndexingSnapshot(service, DatasourceIndexingResponse{
				Tasks:       []DatasourceTaskStatus{{Phase: "embeddings", TotalTasks: 1, CompletedTasks: 1}, {Phase: "search_index", TotalTasks: 1, QueuedTasks: 1}},
				Datasources: []DatasourceIndexingStatus{{SourceKey: sources[0].SourceKey, IngestionKind: datasourceIngestionRemoteAPI, EmbeddingCompleted: 1, EmbeddingEligible: 1}},
			})
			publish := runtime.runScheduledSemanticIndexPublish
			if priority {
				publish = runtime.runPrioritySemanticIndexPublish
			}
			if !publish(ctx, semanticIndexingSchedule{Workers: 1}) {
				t.Fatal("index was not published")
			}
			// Read the notification snapshot directly, without the repair recount.
			snapshot, ok := runtime.datasourceIndexingSnapshot(ctx, service)
			if !ok {
				t.Fatal("missing publication progress snapshot")
			}
			seen := map[string]bool{}
			for _, task := range snapshot.Tasks {
				if task.Phase != "embeddings" && task.Phase != "search_index" {
					continue
				}
				seen[task.Phase] = true
				if task.TotalTasks != 1 || task.CompletedTasks != 1 || task.QueuedTasks != 0 {
					t.Fatalf("post-publication %s progress = %+v", task.Phase, task)
				}
			}
			if !seen["embeddings"] || !seen["search_index"] {
				t.Fatalf("missing semantic Tasks: %+v", snapshot.Tasks)
			}
		})
	}
}
