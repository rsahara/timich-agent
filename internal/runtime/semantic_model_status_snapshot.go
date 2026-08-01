package runtime

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
)

const (
	semanticModelRegistryStatusSnapshotKey = "semantic_model_registry"
	semanticModelRegistrySnapshotTimeout   = 500 * time.Millisecond
)

// CachedSemanticModelRegistryStatus returns the last successful Semantic Models
// read model from memory or the persisted Admin status snapshot table.
func (a *AgentRuntime) CachedSemanticModelRegistryStatus(ctx context.Context) (catalog.SemanticModelRegistryStatus, bool) {
	if a == nil {
		return catalog.SemanticModelRegistryStatus{}, false
	}
	a.semanticModelSnapshotMu.Lock()
	if a.semanticModelSnapshot != nil {
		snapshot := *a.semanticModelSnapshot
		a.semanticModelSnapshotMu.Unlock()
		return snapshot, true
	}
	a.semanticModelSnapshotMu.Unlock()

	catalogService := a.catalogService()
	if catalogService == nil {
		return catalog.SemanticModelRegistryStatus{}, false
	}
	loadCtx, cancel := context.WithTimeout(ctx, semanticModelRegistrySnapshotTimeout)
	defer cancel()
	stored, ok, err := catalogService.AdminStatusSnapshot(loadCtx, semanticModelRegistryStatusSnapshotKey)
	if err != nil || !ok || len(stored.Payload) == 0 {
		return catalog.SemanticModelRegistryStatus{}, false
	}
	var snapshot catalog.SemanticModelRegistryStatus
	if err := json.Unmarshal(stored.Payload, &snapshot); err != nil {
		return catalog.SemanticModelRegistryStatus{}, false
	}
	cloned, ok := cloneSemanticModelRegistryStatus(snapshot)
	if !ok {
		return catalog.SemanticModelRegistryStatus{}, false
	}
	a.semanticModelSnapshotMu.Lock()
	a.semanticModelSnapshot = &cloned
	a.semanticModelSnapshotAt = stored.UpdatedAt
	a.semanticModelSnapshotMu.Unlock()
	return snapshot, true
}

// RememberSemanticModelRegistryStatus stores a best-effort Semantic Models read
// model so the Admin UI can render immediately during later busy periods.
func (a *AgentRuntime) RememberSemanticModelRegistryStatus(status catalog.SemanticModelRegistryStatus) {
	if a == nil {
		return
	}
	snapshot, ok := cloneSemanticModelRegistryStatus(status)
	if !ok {
		return
	}
	snapshotAt := time.Now().UTC()
	a.semanticModelSnapshotMu.Lock()
	a.semanticModelSnapshot = &snapshot
	a.semanticModelSnapshotAt = snapshotAt
	a.semanticModelSnapshotMu.Unlock()

	catalogService := a.catalogService()
	if catalogService == nil {
		return
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	saveCtx, cancel := context.WithTimeout(context.Background(), semanticModelRegistrySnapshotTimeout)
	defer cancel()
	_ = catalogService.SaveAdminStatusSnapshot(saveCtx, semanticModelRegistryStatusSnapshotKey, payload, snapshotAt)
}

func cloneSemanticModelRegistryStatus(status catalog.SemanticModelRegistryStatus) (catalog.SemanticModelRegistryStatus, bool) {
	payload, err := json.Marshal(status)
	if err != nil {
		return catalog.SemanticModelRegistryStatus{}, false
	}
	var cloned catalog.SemanticModelRegistryStatus
	if err := json.Unmarshal(payload, &cloned); err != nil {
		return catalog.SemanticModelRegistryStatus{}, false
	}
	return cloned, true
}
