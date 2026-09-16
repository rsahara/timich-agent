package runtime

import (
	"context"
	"testing"
	"time"
)

func TestSemanticAssignmentsWaitForMetadataThenResumeAlongsideThumbnails(t *testing.T) {
	for _, phase := range []string{"mixed_embedding", "scheduled_embedding", "priority_publish", "normal_publish"} {
		t.Run(phase, func(t *testing.T) {
			runtime := &AgentRuntime{backgroundWorkerRandom: func() float64 { return 0.99 }}
			schedule := semanticIndexingSchedule{Workers: 1, BatchSize: 10}
			state := schedulerWorkState{SemanticScheduled: true}
			want := "embeddings"
			switch phase {
			case "mixed_embedding":
				state.SemanticMixedEmbeddingQueued, state.SemanticMixedEmbeddingBatch = 300000, 10
			case "scheduled_embedding":
				state.SemanticEmbeddingReady, state.SemanticEmbeddingBatchSize = true, 10
			case "priority_publish":
				state.SemanticPriorityPublishReady = true
				want = "search_index"
			case "normal_publish":
				state.SemanticPublishReady = true
				want = "search_index"
			}
			selectPhase := func() string {
				assignment, ok := runtime.nextBackgroundWorkerAssignment(context.Background(), 1, false, schedule, true, state, true)
				if !ok {
					return ""
				}
				return assignment.phase
			}
			state.MetadataQueued = 1
			if got := selectPhase(); got != "metadata" {
				t.Fatalf("metadata pending: selected %q", got)
			}
			state.MetadataQueued = 0
			state.MetadataSettling = 1
			if got := selectPhase(); got != "" {
				t.Fatalf("metadata settling: selected %q", got)
			}
			state.MetadataSettling = 0
			runtime.backgroundWorkerActive = map[string]int{"metadata": 1}
			if got := selectPhase(); got != "" {
				t.Fatalf("last metadata batch active: selected %q", got)
			}
			runtime.backgroundWorkerActive = nil
			runtime.datasourceTaskActive = map[string]int{"metadata": 1}
			if got := selectPhase(); got != "" {
				t.Fatalf("manual metadata batch active: selected %q", got)
			}
			runtime.datasourceTaskActive = nil
			runtime.datasourceDiscoveryActive = 1
			if got := selectPhase(); got != "" {
				t.Fatalf("discovery active: selected %q", got)
			}
			runtime.datasourceDiscoveryActive = 0
			runtime.backgroundWorkerActive = map[string]int{"thumbnails": 1}
			if phase == "mixed_embedding" {
				state.ThumbnailQueued = 100
			}
			if got := selectPhase(); got != want {
				t.Fatalf("metadata drained with thumbnails active: selected %q, want %q", got, want)
			}
		})
	}
}

func TestSemanticTaskWaitPreservesProgressAndClearsAfterMetadata(t *testing.T) {
	tasks := []DatasourceTaskStatus{
		{Phase: "metadata", QueuedTasks: 1},
		{Phase: "thumbnails", QueuedTasks: 100},
		{Phase: "embeddings", QueuedTasks: 300000, CompletedTasks: 60000, TotalTasks: 360000},
		{Phase: "search_index", QueuedTasks: 1000, CompletedTasks: 59000, TotalTasks: 60000, WaitingReason: datasourceTaskWaitingQueuedTarget, WaitingQueuedTarget: 11800},
	}
	tasks = normalizeDatasourceTaskDependencies(tasks)
	for _, task := range tasks[2:] {
		if task.Status != "waiting" || task.WaitingReason != datasourceTaskWaitingMetadata || task.WaitingQueuedTarget != 0 {
			t.Fatalf("waiting task = %+v", task)
		}
	}
	if tasks[2].CompletedTasks != 60000 || tasks[2].QueuedTasks != 300000 || tasks[3].CompletedTasks != 59000 {
		t.Fatalf("progress lost: %+v", tasks)
	}
	tasks[0].QueuedTasks = 0
	tasks[0].FailedTasks = 1
	tasks = normalizeDatasourceTaskDependencies(tasks)
	for _, task := range tasks[2:] {
		if task.Status != "queued" || task.WaitingReason != "" {
			t.Fatalf("drained metadata with failure: %+v", task)
		}
	}
	tasks[0].SettlingTasks = 1
	tasks[2].ActiveTasks = 1
	tasks[3].WaitingReason = datasourceTaskWaitingPaused
	tasks = normalizeDatasourceTaskDependencies(tasks)
	if tasks[2].Status != "running" || tasks[3].Status != "paused" {
		t.Fatalf("active/paused states lost: %+v", tasks)
	}
}

func TestSemanticMetadataBackoffDoesNotPermitExpensiveWork(t *testing.T) {
	runtime := &AgentRuntime{backgroundWorkerRandom: func() float64 { return 0.99 }}
	next := time.Now().UTC().Add(time.Minute)
	runtime.setLocalBackgroundWorkerRetryNotBefore("metadata", &next)
	state := schedulerWorkState{MetadataQueued: 1, ThumbnailQueued: 1, SemanticMixedEmbeddingQueued: 300000, SemanticMixedEmbeddingBatch: 10, SemanticPriorityPublishReady: true}
	assignment, ok := runtime.nextBackgroundWorkerAssignment(context.Background(), 1, false, semanticIndexingSchedule{Workers: 1, BatchSize: 10}, true, state, true)
	if !ok || assignment.phase != "thumbnails" {
		t.Fatalf("backoff assignment = %+v, %t", assignment, ok)
	}
}
