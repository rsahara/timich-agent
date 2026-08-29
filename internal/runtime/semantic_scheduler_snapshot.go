package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/rsahara/timich-agent/internal/catalog"
)

type semanticSchedulerSnapshotLoader func(context.Context, catalog.SemanticModelProfileStatus) (*catalog.SemanticModelBackfillSnapshot, error)

type semanticSchedulerSnapshotEntry struct {
	snapshot *catalog.SemanticModelBackfillSnapshot
	err      error
}

type semanticSchedulerSnapshot struct {
	profiles []catalog.SemanticModelProfileStatus
	entries  map[string]semanticSchedulerSnapshotEntry
}

func loadSemanticSchedulerSnapshot(ctx context.Context, profiles []catalog.SemanticModelProfileStatus, loader semanticSchedulerSnapshotLoader) semanticSchedulerSnapshot {
	snapshot := semanticSchedulerSnapshot{
		profiles: append([]catalog.SemanticModelProfileStatus(nil), profiles...),
		entries:  make(map[string]semanticSchedulerSnapshotEntry, len(profiles)),
	}
	if loader == nil {
		return snapshot
	}
	for _, profile := range profiles {
		if semanticBackfillRolePriority(profile) == 0 ||
			profile.Runtime == nil ||
			!profile.Runtime.Loaded ||
			!profile.Runtime.CanEmbed {
			continue
		}
		status, err := loader(ctx, profile)
		snapshot.entries[semanticSchedulerProfileKey(profile)] = semanticSchedulerSnapshotEntry{snapshot: status, err: err}
	}
	return snapshot
}

func (s semanticSchedulerSnapshot) backfillStatus(profile catalog.SemanticModelProfileStatus) (*catalog.SemanticModelBackfillStatus, error) {
	entry, ok := s.entries[semanticSchedulerProfileKey(profile)]
	if !ok || entry.snapshot == nil {
		return nil, entry.err
	}
	status := entry.snapshot.Status
	return &status, entry.err
}

func (s semanticSchedulerSnapshot) indexPublishNeed(ctx context.Context, catalogService *catalog.Service, modelStore *catalog.SemanticModelPackStore, profile catalog.SemanticModelProfileStatus) (bool, int, error) {
	entry, ok := s.entries[semanticSchedulerProfileKey(profile)]
	if !ok || entry.err != nil || entry.snapshot == nil {
		return false, 0, entry.err
	}
	return catalogService.SemanticModelIndexPublishNeededFromSnapshot(ctx, modelStore, profile, entry.snapshot, false)
}

func semanticSchedulerProfileKey(profile catalog.SemanticModelProfileStatus) string {
	return strings.Join([]string{
		strings.TrimSpace(profile.ModelID),
		strings.TrimSpace(profile.VectorSpaceID),
		fmt.Sprintf("%d", profile.EmbeddingDim),
	}, "\x1f")
}
