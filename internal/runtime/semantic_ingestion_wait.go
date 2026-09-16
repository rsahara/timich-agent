package runtime

const (
	datasourceTaskWaitingDiscovery = "media_discovery"
	datasourceTaskWaitingMetadata  = "local_metadata"
)

// Reuse ingestion counts and current worker activity. Checking this dependency
// must not scan media, hash files, or recount the semantic corpus.
func semanticIngestionWaitReason(discovery, metadata, settling int) string {
	switch {
	case discovery > 0:
		return datasourceTaskWaitingDiscovery
	case metadata > 0 || settling > 0:
		return datasourceTaskWaitingMetadata
	default:
		return ""
	}
}

func (a *AgentRuntime) semanticIngestionWaitReason(state schedulerWorkState) string {
	a.backgroundWorkerMu.Lock()
	activeMetadata := a.backgroundWorkerActive["metadata"]
	a.backgroundWorkerMu.Unlock()
	a.datasourceTaskMu.Lock()
	activeMetadata = max(activeMetadata, a.datasourceTaskActive["metadata"])
	discovery := a.datasourceDiscoveryActive
	a.datasourceTaskMu.Unlock()
	return semanticIngestionWaitReason(discovery, state.MetadataQueued+activeMetadata, state.MetadataSettling)
}

func applySemanticIngestionWaitToDatasourceTasks(tasks []DatasourceTaskStatus) {
	var discovery, metadata, settling int
	for _, task := range tasks {
		switch task.Phase {
		case "phase0":
			discovery += task.ActiveTasks
		case "metadata":
			metadata += task.QueuedTasks + task.ActiveTasks
			settling += task.SettlingTasks
		}
	}
	reason := semanticIngestionWaitReason(discovery, metadata, settling)
	for index := range tasks {
		task := &tasks[index]
		if task.Phase != "embeddings" && task.Phase != "search_index" {
			continue
		}
		switch task.WaitingReason {
		case datasourceTaskWaitingDiscovery, datasourceTaskWaitingMetadata:
			task.WaitingReason = ""
		}
		if reason == "" || task.ActiveTasks > 0 || task.SetupRequired != "" ||
			task.WaitingReason == datasourceTaskWaitingPaused {
			continue
		}
		if task.QueuedTasks > 0 || task.QueuedTasksUnknown {
			task.WaitingReason = reason
			task.WaitingQueuedTarget = 0
			task.NextRunAt = nil
		}
	}
}
