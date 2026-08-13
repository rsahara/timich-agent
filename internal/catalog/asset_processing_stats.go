package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

const (
	AssetProcessingScopeAll = ""

	AssetProcessingStageMetadata    = "metadata"
	AssetProcessingStageThumbnails  = "thumbnails"
	AssetProcessingStageEmbeddings  = "embeddings"
	AssetProcessingStageSearchIndex = "search_index"
	AssetProcessingStageFoundMedias = "found_medias"
	AssetProcessingStageBrowsable   = "browsable_medias"
	AssetProcessingStageSearchable  = "searchable_medias"
	AssetProcessingStageIssues      = "issues"

	AssetProcessingStatusPending     = "pending"
	AssetProcessingStatusSettling    = "settling"
	AssetProcessingStatusRunning     = "running"
	AssetProcessingStatusReady       = "ready"
	AssetProcessingStatusFailed      = "failed"
	AssetProcessingStatusUpdating    = "updating"
	AssetProcessingStatusUnavailable = "unavailable"

	assetProcessingSemanticStatsRefreshMinAge = 30 * time.Second
	assetProcessingSemanticVariantPrefix      = "semantic-profile-v1:"
)

// AssetProcessingStat is a low-cost Admin read model row. Counts are refreshed
// from absolute catalog state so missed task transitions cannot drift forever.
type AssetProcessingStat struct {
	ScopeKey    string
	Stage       string
	Variant     string
	Status      string
	Count       int
	TotalCount  int
	RefreshedAt time.Time
}

// AssetProcessingStatsSnapshot is the current Admin read model for task-sized
// work. ScopeKey is empty for the aggregate across all configured datasources.
type AssetProcessingStatsSnapshot struct {
	RefreshedAt time.Time
	Stats       []AssetProcessingStat
}

type processingStatsQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type semanticProcessingCounts struct {
	EligibleCount        int
	CompletedVectorCount int
	IndexedVectorCount   int
	FailedVectorCount    int
	FailedIndexJobCount  int
}

type processingStatsDatasource struct {
	SourceKey string
	Kind      string
}

func (s AssetProcessingStatsSnapshot) Empty() bool {
	return len(s.Stats) == 0 || s.RefreshedAt.IsZero()
}

func (s AssetProcessingStatsSnapshot) HasStage(stage string) bool {
	return s.HasStageForScope(AssetProcessingScopeAll, stage)
}

func (s AssetProcessingStatsSnapshot) HasStageForScope(scopeKey string, stage string) bool {
	scopeKey = strings.TrimSpace(scopeKey)
	stage = strings.TrimSpace(stage)
	for _, stat := range s.Stats {
		if stat.ScopeKey == scopeKey && stat.Stage == stage {
			return true
		}
	}
	return false
}

func (s AssetProcessingStatsSnapshot) Count(stage string, status string) int {
	return s.CountForScope(AssetProcessingScopeAll, stage, status)
}

func (s AssetProcessingStatsSnapshot) CountForScope(scopeKey string, stage string, status string) int {
	scopeKey = strings.TrimSpace(scopeKey)
	stage = strings.TrimSpace(stage)
	status = strings.TrimSpace(status)
	total := 0
	for _, stat := range s.Stats {
		if stat.ScopeKey != scopeKey || stat.Stage != stage || stat.Status != status {
			continue
		}
		total += stat.Count
	}
	return total
}

func (s AssetProcessingStatsSnapshot) Running(stage string) int {
	return s.Count(stage, AssetProcessingStatusRunning)
}

func (s AssetProcessingStatsSnapshot) Pending(stage string) int {
	return s.Count(stage, AssetProcessingStatusPending)
}

func (s AssetProcessingStatsSnapshot) Settling(stage string) int {
	return s.Count(stage, AssetProcessingStatusSettling)
}

func (s AssetProcessingStatsSnapshot) Ready(stage string) int {
	return s.Count(stage, AssetProcessingStatusReady)
}

func (s AssetProcessingStatsSnapshot) Failed(stage string) int {
	return s.Count(stage, AssetProcessingStatusFailed)
}

func (s AssetProcessingStatsSnapshot) Total(stage string) int {
	return s.TotalForScope(AssetProcessingScopeAll, stage)
}

func (s AssetProcessingStatsSnapshot) TotalForScope(scopeKey string, stage string) int {
	scopeKey = strings.TrimSpace(scopeKey)
	stage = strings.TrimSpace(stage)
	total := 0
	for _, stat := range s.Stats {
		if stat.ScopeKey != scopeKey || stat.Stage != stage {
			continue
		}
		if stat.TotalCount > total {
			total = stat.TotalCount
		}
	}
	return total
}

func (s AssetProcessingStatsSnapshot) StatForScope(scopeKey string, stage string) (AssetProcessingStat, bool) {
	return s.statForScopeAndVariant(scopeKey, stage, "", false)
}

func (s AssetProcessingStatsSnapshot) statForScopeVariant(scopeKey string, stage string, variant string) (AssetProcessingStat, bool) {
	return s.statForScopeAndVariant(scopeKey, stage, variant, true)
}

func (s AssetProcessingStatsSnapshot) statForScopeAndVariant(scopeKey string, stage string, variant string, matchVariant bool) (AssetProcessingStat, bool) {
	scopeKey = strings.TrimSpace(scopeKey)
	stage = strings.TrimSpace(stage)
	variant = strings.TrimSpace(variant)
	var fallback AssetProcessingStat
	for _, stat := range s.Stats {
		if stat.ScopeKey != scopeKey || stat.Stage != stage {
			continue
		}
		if matchVariant && strings.TrimSpace(stat.Variant) != variant {
			continue
		}
		if stat.Status == AssetProcessingStatusReady {
			return stat, true
		}
		if fallback.Stage == "" {
			fallback = stat
		}
	}
	if fallback.Stage != "" {
		return fallback, true
	}
	return AssetProcessingStat{}, false
}

func (s AssetProcessingStatsSnapshot) statsForScopesStagesVariant(scopeKeys []string, stages []string, variant string) []AssetProcessingStat {
	scopeSet := make(map[string]struct{}, len(scopeKeys))
	for _, scopeKey := range scopeKeys {
		scopeSet[strings.TrimSpace(scopeKey)] = struct{}{}
	}
	stageSet := make(map[string]struct{}, len(stages))
	for _, stage := range stages {
		stageSet[strings.TrimSpace(stage)] = struct{}{}
	}
	stats := make([]AssetProcessingStat, 0)
	variant = strings.TrimSpace(variant)
	for _, stat := range s.Stats {
		if _, ok := scopeSet[strings.TrimSpace(stat.ScopeKey)]; !ok {
			continue
		}
		if _, ok := stageSet[strings.TrimSpace(stat.Stage)]; !ok {
			continue
		}
		if strings.TrimSpace(stat.Variant) != variant {
			continue
		}
		stats = append(stats, stat)
	}
	return stats
}

func assetProcessingSemanticVariant(profile *SemanticModelProfileStatus) string {
	if profile == nil {
		return ""
	}
	modelID := strings.TrimSpace(profile.ModelID)
	vectorSpaceID := strings.TrimSpace(profile.VectorSpaceID)
	if modelID == "" || vectorSpaceID == "" {
		return ""
	}
	identity, _ := json.Marshal([2]string{modelID, vectorSpaceID})
	return assetProcessingSemanticVariantPrefix + string(identity)
}

// MatchesSemanticProfile reports whether aggregate semantic rows belong to the
// requested profile. Legacy rows with an empty variant intentionally do not
// match an installed profile and are recounted once after upgrade.
func (s AssetProcessingStatsSnapshot) MatchesSemanticProfile(profile *SemanticModelProfileStatus) bool {
	variant := assetProcessingSemanticVariant(profile)
	if variant == "" {
		return !s.HasStage(AssetProcessingStageEmbeddings) && !s.HasStage(AssetProcessingStageSearchIndex)
	}
	for _, stage := range []string{AssetProcessingStageEmbeddings, AssetProcessingStageSearchIndex} {
		if _, ok := s.statForScopeVariant(AssetProcessingScopeAll, stage, variant); !ok {
			return false
		}
	}
	return true
}

// SemanticBackfillStatusFromAssetProcessingStats converts the Admin read model
// into the semantic progress shape used by Admin UI surfaces. It intentionally
// avoids live catalog joins so installed model status can render even while the
// main catalog is busy.
func SemanticBackfillStatusFromAssetProcessingStats(snapshot AssetProcessingStatsSnapshot, profile SemanticModelProfileStatus) *SemanticModelBackfillStatus {
	if snapshot.Empty() || !snapshot.MatchesSemanticProfile(&profile) {
		return nil
	}
	modelID := strings.TrimSpace(profile.ModelID)
	vectorSpaceID := strings.TrimSpace(profile.VectorSpaceID)
	if modelID == "" || vectorSpaceID == "" {
		return nil
	}

	status := &SemanticModelBackfillStatus{
		Status:               semanticBackfillStatusReady,
		SourceKind:           "catalog",
		ModelID:              modelID,
		VectorSpaceID:        vectorSpaceID,
		EmbeddingDim:         profile.EmbeddingDim,
		EligibleAssetCount:   snapshot.Total(AssetProcessingStageEmbeddings),
		CompletedVectorCount: snapshot.Ready(AssetProcessingStageEmbeddings),
		FailedVectorCount:    snapshot.Failed(AssetProcessingStageEmbeddings),
		IndexedVectorCount:   snapshot.Ready(AssetProcessingStageSearchIndex),
		FailedIndexJobCount:  snapshot.Failed(AssetProcessingStageSearchIndex),
		MessageCode:          semanticBackfillMessageReady,
	}
	status.RemainingVectorCount = max(status.EligibleAssetCount-status.CompletedVectorCount, 0)
	switch {
	case status.CompletedVectorCount == 0 && status.EligibleAssetCount > 0:
		status.Status = semanticBackfillStatusPending
		status.MessageCode = semanticBackfillMessagePending
	case status.CompletedVectorCount < status.EligibleAssetCount:
		status.Status = semanticBackfillStatusBackfilling
		status.MessageCode = semanticBackfillMessageIncomplete
	case status.IndexedVectorCount < status.CompletedVectorCount || status.PendingIndexJobCount > 0 || status.FailedIndexJobCount > 0:
		status.Status = semanticBackfillStatusIndexing
		status.MessageCode = semanticBackfillMessageIndexing
	}
	return status
}

// AssetProcessingStats returns the latest persisted task read model without
// performing any expensive live catalog aggregation.
func (s *Service) AssetProcessingStats(ctx context.Context) (AssetProcessingStatsSnapshot, error) {
	if !s.catalogStoreEnabled() {
		return AssetProcessingStatsSnapshot{}, nil
	}
	return s.catalog.AssetProcessingStats(ctx)
}

// RefreshAssetProcessingStats refreshes the task read model when it is stale.
// It performs expensive COUNTs outside the write transaction and then replaces
// the small stats table in one short write.
func (s *Service) RefreshAssetProcessingStats(ctx context.Context, profile *SemanticModelProfileStatus, minAge time.Duration) (AssetProcessingStatsSnapshot, error) {
	if !s.catalogStoreEnabled() {
		return AssetProcessingStatsSnapshot{}, nil
	}
	current, err := s.AssetProcessingStats(ctx)
	if err != nil {
		return AssetProcessingStatsSnapshot{}, fmt.Errorf("load asset processing stats: %w", err)
	}
	if minAge > 0 && !current.RefreshedAt.IsZero() && time.Since(current.RefreshedAt) < minAge && current.MatchesSemanticProfile(profile) {
		return current, nil
	}

	refreshedAt := time.Now().UTC()
	stats, err := s.collectAssetProcessingStats(ctx, profile, refreshedAt, current)
	if err != nil {
		return current, fmt.Errorf("collect asset processing stats: %w", err)
	}
	if err := s.catalog.ReplaceAssetProcessingStats(ctx, stats, refreshedAt); err != nil {
		return current, fmt.Errorf("replace asset processing stats: %w", err)
	}
	return AssetProcessingStatsSnapshot{RefreshedAt: refreshedAt, Stats: stats}, nil
}

func (s *Service) collectAssetProcessingStats(ctx context.Context, profile *SemanticModelProfileStatus, refreshedAt time.Time, current AssetProcessingStatsSnapshot) ([]AssetProcessingStat, error) {
	readDB, err := s.catalog.openReadOnlyDB(ctx)
	if err != nil {
		return nil, err
	}
	defer readDB.Close()

	datasources := s.processingStatsDatasources()
	stats := make([]AssetProcessingStat, 0, 16)
	addVariantAt := func(scopeKey string, stage string, variant string, status string, count int, total int, statRefreshedAt time.Time) {
		if statRefreshedAt.IsZero() {
			statRefreshedAt = refreshedAt
		}
		stats = append(stats, AssetProcessingStat{
			ScopeKey:    scopeKey,
			Stage:       stage,
			Variant:     variant,
			Status:      status,
			Count:       max(count, 0),
			TotalCount:  max(total, 0),
			RefreshedAt: statRefreshedAt,
		})
	}
	addAt := func(scopeKey string, stage string, status string, count int, total int, statRefreshedAt time.Time) {
		addVariantAt(scopeKey, stage, "", status, count, total, statRefreshedAt)
	}
	add := func(stage string, status string, count int, total int) {
		addAt(AssetProcessingScopeAll, stage, status, count, total, refreshedAt)
	}

	metadataReady, err := countProcessingRows(ctx, readDB, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'active'`)
	if err != nil {
		return nil, fmt.Errorf("count metadata ready assets: %w", err)
	}
	nowText := formatCatalogTime(refreshedAt)
	metadataPending, err := countProcessingRows(ctx, readDB, `SELECT COUNT(*)
		FROM local_scan_jobs j
		LEFT JOIN local_asset_locations l ON l.id = j.location_id
		JOIN local_scan_root_state rs
			ON rs.source_key = j.source_key
			AND rs.root_key = j.root_key
			AND rs.root_generation = j.root_generation
		WHERE j.job_kind = ? AND j.status = 'queued'
			AND MAX(j.scheduled_at, COALESCE(l.metadata_not_before, j.scheduled_at)) <= ?`, localMetadataJobKind, nowText)
	if err != nil {
		return nil, fmt.Errorf("count metadata pending jobs: %w", err)
	}
	metadataSettling, err := countProcessingRows(ctx, readDB, `SELECT COUNT(*)
		FROM local_scan_jobs j
		LEFT JOIN local_asset_locations l ON l.id = j.location_id
		JOIN local_scan_root_state rs
			ON rs.source_key = j.source_key
			AND rs.root_key = j.root_key
			AND rs.root_generation = j.root_generation
		WHERE j.job_kind = ? AND j.status = 'queued'
			AND MAX(j.scheduled_at, COALESCE(l.metadata_not_before, j.scheduled_at)) > ?`, localMetadataJobKind, nowText)
	if err != nil {
		return nil, fmt.Errorf("count metadata settling jobs: %w", err)
	}
	metadataRunning, err := countProcessingRows(ctx, readDB, `SELECT COUNT(*)
		FROM local_scan_jobs j
		JOIN local_scan_root_state rs
			ON rs.source_key = j.source_key
			AND rs.root_key = j.root_key
			AND rs.root_generation = j.root_generation
		WHERE j.job_kind = ? AND j.status = 'running'`, localMetadataJobKind)
	if err != nil {
		return nil, fmt.Errorf("count metadata running jobs: %w", err)
	}
	metadataFailed, err := countProcessingRows(ctx, readDB, `SELECT COUNT(*)
		FROM local_scan_jobs j
		JOIN local_scan_root_state rs
			ON rs.source_key = j.source_key
			AND rs.root_key = j.root_key
			AND rs.root_generation = j.root_generation
		WHERE j.job_kind = ? AND j.status = 'failed'`, localMetadataJobKind)
	if err != nil {
		return nil, fmt.Errorf("count metadata failed jobs: %w", err)
	}
	metadataTotal := metadataReady + metadataPending + metadataSettling + metadataRunning + metadataFailed
	add(AssetProcessingStageMetadata, AssetProcessingStatusPending, metadataPending, metadataTotal)
	add(AssetProcessingStageMetadata, AssetProcessingStatusSettling, metadataSettling, metadataTotal)
	add(AssetProcessingStageMetadata, AssetProcessingStatusRunning, metadataRunning, metadataTotal)
	add(AssetProcessingStageMetadata, AssetProcessingStatusReady, metadataReady, metadataTotal)
	add(AssetProcessingStageMetadata, AssetProcessingStatusFailed, metadataFailed, metadataTotal)

	thumbnailReady, err := countProcessingRows(ctx, readDB, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'active' AND media_type IN ('image', 'video') AND thumbnail_status = 'ready'`)
	if err != nil {
		return nil, fmt.Errorf("count thumbnail ready assets: %w", err)
	}
	thumbnailPendingAssets, err := countProcessingRows(ctx, readDB, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'active' AND media_type IN ('image', 'video') AND thumbnail_status = 'pending'`)
	if err != nil {
		return nil, fmt.Errorf("count thumbnail pending assets: %w", err)
	}
	thumbnailRunning, err := countProcessingRows(ctx, readDB, `SELECT COUNT(*)
		FROM local_scan_jobs j
		JOIN local_scan_root_state rs
			ON rs.source_key = j.source_key
			AND rs.root_key = j.root_key
			AND rs.root_generation = j.root_generation
		WHERE j.job_kind = ? AND j.status = 'running'`, localThumbnailJobKind)
	if err != nil {
		return nil, fmt.Errorf("count thumbnail running jobs: %w", err)
	}
	thumbnailFailed, err := countProcessingRows(ctx, readDB, `SELECT COUNT(*) FROM local_assets WHERE visibility_status = 'active' AND media_type IN ('image', 'video') AND thumbnail_status = 'failed'`)
	if err != nil {
		return nil, fmt.Errorf("count thumbnail failed assets: %w", err)
	}
	thumbnailPending := max(thumbnailPendingAssets-thumbnailRunning, 0)
	thumbnailTotal := thumbnailReady + thumbnailPendingAssets + thumbnailFailed
	add(AssetProcessingStageThumbnails, AssetProcessingStatusPending, thumbnailPending, thumbnailTotal)
	add(AssetProcessingStageThumbnails, AssetProcessingStatusRunning, thumbnailRunning, thumbnailTotal)
	add(AssetProcessingStageThumbnails, AssetProcessingStatusReady, thumbnailReady, thumbnailTotal)
	add(AssetProcessingStageThumbnails, AssetProcessingStatusFailed, thumbnailFailed, thumbnailTotal)

	semanticCountsBySource := map[string]semanticProcessingCounts{}
	reuseSemanticStats := false
	semanticVariant := assetProcessingSemanticVariant(profile)
	if semanticVariant != "" {
		reuseSemanticStats = assetProcessingSemanticStatsFresh(current, datasources, semanticVariant, refreshedAt)
	}
	if reuseSemanticStats {
		stats = append(stats, current.statsForScopesStagesVariant(
			[]string{AssetProcessingScopeAll},
			[]string{AssetProcessingStageEmbeddings, AssetProcessingStageSearchIndex},
			semanticVariant,
		)...)
	} else if semanticVariant != "" {
		countsBySource, status, err := s.semanticProcessingCounts(ctx, readDB, *profile)
		if err != nil {
			return nil, fmt.Errorf("count semantic processing stats: %w", err)
		}
		semanticCountsBySource = countsBySource
		if status != nil {
			embeddingPending := max(status.EligibleCount-status.CompletedVectorCount-status.FailedVectorCount, 0)
			addVariantAt(AssetProcessingScopeAll, AssetProcessingStageEmbeddings, semanticVariant, AssetProcessingStatusPending, embeddingPending, status.EligibleCount, refreshedAt)
			addVariantAt(AssetProcessingScopeAll, AssetProcessingStageEmbeddings, semanticVariant, AssetProcessingStatusReady, status.CompletedVectorCount, status.EligibleCount, refreshedAt)
			addVariantAt(AssetProcessingScopeAll, AssetProcessingStageEmbeddings, semanticVariant, AssetProcessingStatusFailed, status.FailedVectorCount, status.EligibleCount, refreshedAt)

			searchPending := max(status.CompletedVectorCount-status.IndexedVectorCount, 0)
			addVariantAt(AssetProcessingScopeAll, AssetProcessingStageSearchIndex, semanticVariant, AssetProcessingStatusPending, searchPending, status.CompletedVectorCount, refreshedAt)
			addVariantAt(AssetProcessingScopeAll, AssetProcessingStageSearchIndex, semanticVariant, AssetProcessingStatusReady, status.IndexedVectorCount, status.CompletedVectorCount, refreshedAt)
			addVariantAt(AssetProcessingScopeAll, AssetProcessingStageSearchIndex, semanticVariant, AssetProcessingStatusFailed, status.FailedIndexJobCount, status.CompletedVectorCount, refreshedAt)
		}
	}

	datasourceStats, err := s.collectDatasourceCoverageStats(ctx, readDB, profile, refreshedAt, datasources, semanticCountsBySource, reuseSemanticStats, current)
	if err != nil {
		return nil, err
	}
	stats = append(stats, datasourceStats...)

	return stats, nil
}

func (s *Service) collectDatasourceCoverageStats(ctx context.Context, db processingStatsQueryer, profile *SemanticModelProfileStatus, refreshedAt time.Time, datasources []processingStatsDatasource, semanticCountsBySource map[string]semanticProcessingCounts, reuseSemanticStats bool, current AssetProcessingStatsSnapshot) ([]AssetProcessingStat, error) {
	stats := make([]AssetProcessingStat, 0, len(datasources)*4)
	semanticVariant := assetProcessingSemanticVariant(profile)
	addVariantAt := func(scopeKey string, stage string, variant string, status string, count int, total int, statRefreshedAt time.Time) {
		if statRefreshedAt.IsZero() {
			statRefreshedAt = refreshedAt
		}
		stats = append(stats, AssetProcessingStat{
			ScopeKey:    scopeKey,
			Stage:       stage,
			Variant:     variant,
			Status:      status,
			Count:       max(count, 0),
			TotalCount:  max(total, 0),
			RefreshedAt: statRefreshedAt,
		})
	}
	addAt := func(scopeKey string, stage string, status string, count int, total int, statRefreshedAt time.Time) {
		addVariantAt(scopeKey, stage, "", status, count, total, statRefreshedAt)
	}
	add := func(scopeKey string, stage string, status string, count int, total int) {
		addAt(scopeKey, stage, status, count, total, refreshedAt)
	}
	for _, datasource := range datasources {
		found, err := s.countDatasourceFoundMedia(ctx, db, datasource)
		if err != nil {
			return nil, fmt.Errorf("count datasource found media for %s: %w", datasource.SourceKey, err)
		}
		browsable, err := s.countDatasourceBrowsableMedia(ctx, db, datasource)
		if err != nil {
			return nil, fmt.Errorf("count datasource browsable media for %s: %w", datasource.SourceKey, err)
		}
		searchableStatus := AssetProcessingStatusUnavailable
		searchable := 0
		searchableRefreshedAt := refreshedAt
		if semanticVariant != "" {
			searchableStatus = AssetProcessingStatusReady
			if reuseSemanticStats {
				if stat, ok := current.statForScopeVariant(datasource.SourceKey, AssetProcessingStageSearchable, semanticVariant); ok {
					searchableStatus = stat.Status
					searchable = stat.Count
					searchableRefreshedAt = stat.RefreshedAt
				} else {
					searchable, err = countDatasourceIndexedSemanticVectors(ctx, db, datasource.SourceKey, *profile)
					if err != nil {
						return nil, fmt.Errorf("count datasource searchable media for %s: %w", datasource.SourceKey, err)
					}
				}
			} else if sourceCounts, ok := semanticCountsBySource[strings.TrimSpace(datasource.SourceKey)]; ok {
				searchable = sourceCounts.IndexedVectorCount
			} else {
				searchable, err = countDatasourceIndexedSemanticVectors(ctx, db, datasource.SourceKey, *profile)
				if err != nil {
					return nil, fmt.Errorf("count datasource searchable media for %s: %w", datasource.SourceKey, err)
				}
			}
		}
		issues, err := s.countDatasourceIssues(ctx, db, datasource, profile)
		if err != nil {
			return nil, fmt.Errorf("count datasource issues for %s: %w", datasource.SourceKey, err)
		}
		coverageTotal := max(found, browsable)
		add(datasource.SourceKey, AssetProcessingStageFoundMedias, AssetProcessingStatusReady, found, found)
		add(datasource.SourceKey, AssetProcessingStageBrowsable, AssetProcessingStatusReady, browsable, coverageTotal)
		addVariantAt(datasource.SourceKey, AssetProcessingStageSearchable, semanticVariant, searchableStatus, searchable, browsable, searchableRefreshedAt)
		add(datasource.SourceKey, AssetProcessingStageIssues, AssetProcessingStatusReady, issues, coverageTotal)
	}
	return stats, nil
}

func (s *Service) processingStatsDatasources() []processingStatsDatasource {
	if s == nil {
		return nil
	}
	state := s.datasourceStateSnapshot()
	sourceKeys := append([]string{}, s.MirrorDatasourceSourceKeys()...)
	sourceKeys = append(sourceKeys, s.LocalDatasourceSourceKeys()...)
	sort.Strings(sourceKeys)
	datasources := make([]processingStatsDatasource, 0, len(sourceKeys))
	for _, sourceKey := range sourceKeys {
		datasource, ok := state.datasources[sourceKey]
		if !ok {
			continue
		}
		datasources = append(datasources, processingStatsDatasource{
			SourceKey: sourceKey,
			Kind:      datasource.Kind,
		})
	}
	return datasources
}

func (s *Service) countDatasourceFoundMedia(ctx context.Context, db processingStatsQueryer, datasource processingStatsDatasource) (int, error) {
	sourceKey := strings.TrimSpace(datasource.SourceKey)
	catalogRows, err := countProcessingRows(ctx, db, `SELECT COUNT(*)
		FROM catalog_assets
		WHERE source_key = ?`, sourceKey)
	if err != nil {
		return 0, err
	}
	if datasource.Kind != config.DatasourceKindLocalFiles {
		return catalogRows, nil
	}
	locationRows, err := countProcessingRows(ctx, db, `SELECT COUNT(*)
		FROM local_asset_locations
		WHERE source_key = ?
			AND status IN ('discovered', 'active', 'missing', 'permission_blocked', 'failed')`, sourceKey)
	if err != nil {
		return 0, err
	}
	return max(locationRows, catalogRows), nil
}

func (s *Service) countDatasourceBrowsableMedia(ctx context.Context, db processingStatsQueryer, datasource processingStatsDatasource) (int, error) {
	sourceKey := strings.TrimSpace(datasource.SourceKey)
	if datasource.Kind != config.DatasourceKindLocalFiles {
		return countProcessingRows(ctx, db, `SELECT COUNT(*)
			FROM catalog_assets
			WHERE source_key = ? AND visibility_status = 'active'`, sourceKey)
	}
	return countProcessingRows(ctx, db, `SELECT COUNT(*)
		FROM local_assets AS la
		INNER JOIN catalog_assets AS ca
			ON ca.source_key = la.source_key
			AND ca.upstream_asset_id = la.asset_id
		WHERE la.source_key = ?
			AND la.visibility_status = 'active'
			AND la.media_type IN ('image', 'video')
			AND la.thumbnail_status = 'ready'
			AND ca.visibility_status = 'active'`, sourceKey)
}

func countDatasourceIndexedSemanticVectors(ctx context.Context, db processingStatsQueryer, sourceKey string, profile SemanticModelProfileStatus) (int, error) {
	return countProcessingRows(ctx, db, `SELECT COALESCE((SELECT indexed_vector_count
		FROM semantic_state
		WHERE source_key = ?
			AND model_id = ?
			AND vector_space_id = ?), 0)`,
		strings.TrimSpace(sourceKey),
		strings.TrimSpace(profile.ModelID),
		strings.TrimSpace(profile.VectorSpaceID),
	)
}

func (s *Service) countDatasourceIssues(ctx context.Context, db processingStatsQueryer, datasource processingStatsDatasource, profile *SemanticModelProfileStatus) (int, error) {
	sourceKey := strings.TrimSpace(datasource.SourceKey)
	total, err := countProcessingRows(ctx, db, `SELECT COUNT(*)
		FROM catalog_assets
		WHERE source_key = ?
			AND visibility_status IN ('missing', 'permission_blocked')`, sourceKey)
	if err != nil {
		return 0, err
	}
	if datasource.Kind == config.DatasourceKindLocalFiles {
		locationIssues, err := countProcessingRows(ctx, db, `SELECT COUNT(*)
			FROM local_asset_locations
			WHERE source_key = ?
				AND status IN ('missing', 'permission_blocked', 'failed')`, sourceKey)
		if err != nil {
			return 0, err
		}
		total += locationIssues
		failedJobs, err := countProcessingRows(ctx, db, `SELECT COUNT(*)
				FROM local_scan_jobs j
				JOIN local_scan_root_state rs
					ON rs.source_key = j.source_key
					AND rs.root_key = j.root_key
					AND rs.root_generation = j.root_generation
				WHERE j.source_key = ?
					AND j.status = 'failed'`, sourceKey)
		if err != nil {
			return 0, err
		}
		total += failedJobs
		failedThumbnails, err := countProcessingRows(ctx, db, `SELECT COUNT(*)
			FROM local_assets
			WHERE source_key = ?
				AND visibility_status = 'active'
				AND media_type IN ('image', 'video')
				AND thumbnail_status = 'failed'`, sourceKey)
		if err != nil {
			return 0, err
		}
		total += failedThumbnails
	}
	if profile != nil && strings.TrimSpace(profile.ModelID) != "" && strings.TrimSpace(profile.VectorSpaceID) != "" {
		failedVectors, err := countProcessingRows(ctx, db, `SELECT COUNT(*)
			FROM semantic_vectors
			WHERE source_key = ?
				AND model_id = ?
				AND vector_space_id = ?
				AND status = 'failed'`, sourceKey, strings.TrimSpace(profile.ModelID), strings.TrimSpace(profile.VectorSpaceID))
		if err != nil {
			return 0, err
		}
		total += failedVectors
		failedIndexJobs, err := countProcessingRows(ctx, db, `SELECT COUNT(*)
			FROM semantic_index_jobs
			WHERE source_key = ?
				AND model_id = ?
				AND vector_space_id = ?
				AND status = 'failed'`, sourceKey, strings.TrimSpace(profile.ModelID), strings.TrimSpace(profile.VectorSpaceID))
		if err != nil {
			return 0, err
		}
		total += failedIndexJobs
	}
	return total, nil
}

func countProcessingRows(ctx context.Context, db processingStatsQueryer, query string, args ...any) (int, error) {
	var count int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func assetProcessingSemanticStatsFresh(current AssetProcessingStatsSnapshot, datasources []processingStatsDatasource, variant string, now time.Time) bool {
	variant = strings.TrimSpace(variant)
	if current.Empty() || variant == "" {
		return false
	}
	for _, stage := range []string{AssetProcessingStageEmbeddings, AssetProcessingStageSearchIndex} {
		if !assetProcessingStatFresh(current, AssetProcessingScopeAll, stage, variant, now, assetProcessingSemanticStatsRefreshMinAge) {
			return false
		}
	}
	for _, datasource := range datasources {
		if !assetProcessingStatFresh(current, datasource.SourceKey, AssetProcessingStageSearchable, variant, now, assetProcessingSemanticStatsRefreshMinAge) {
			return false
		}
	}
	return true
}

func assetProcessingStatFresh(current AssetProcessingStatsSnapshot, scopeKey string, stage string, variant string, now time.Time, maxAge time.Duration) bool {
	stat, ok := current.statForScopeVariant(scopeKey, stage, variant)
	if !ok || stat.RefreshedAt.IsZero() {
		return false
	}
	if stage == AssetProcessingStageSearchable && stat.Status == AssetProcessingStatusUnavailable {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.Sub(stat.RefreshedAt) < maxAge
}

func (s *Service) semanticProcessingCounts(ctx context.Context, db processingStatsQueryer, profile SemanticModelProfileStatus) (map[string]semanticProcessingCounts, *semanticProcessingCounts, error) {
	sourceKeys := s.semanticDatasourceSourceKeysFor(nil)
	if len(sourceKeys) == 0 {
		return nil, nil, nil
	}
	modelID := strings.TrimSpace(profile.ModelID)
	vectorSpaceID := strings.TrimSpace(profile.VectorSpaceID)
	if modelID == "" || vectorSpaceID == "" {
		return nil, nil, nil
	}

	total := semanticProcessingCounts{}
	bySource := make(map[string]semanticProcessingCounts, len(sourceKeys))
	for _, sourceKey := range sourceKeys {
		where, args := semanticCatalogEligibilityWhere(sourceKey, profile.InputKind, "a")

		eligible, err := countProcessingRows(ctx, db, `SELECT COUNT(*)
			FROM catalog_assets a `+where, args...)
		if err != nil {
			return nil, nil, fmt.Errorf("count semantic eligible assets: %w", err)
		}
		sourceCounts := semanticProcessingCounts{EligibleCount: eligible}

		progressWhere, progressArgs := semanticCatalogEligibilityWhere(sourceKey, profile.InputKind, "a")
		progressArgs = append(progressArgs, modelID, vectorSpaceID)
		var completed int
		var failedVectors int
		err = db.QueryRowContext(ctx, `SELECT
				COALESCE(SUM(CASE WHEN v.status = 'ready' THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN v.status = 'failed' THEN 1 ELSE 0 END), 0)
			FROM catalog_assets a
			JOIN semantic_vectors v
				ON v.source_key = a.source_key
				AND v.upstream_asset_id = a.upstream_asset_id
			`+progressWhere+`
				AND v.model_id = ?
				AND v.vector_space_id = ?`, progressArgs...).Scan(&completed, &failedVectors)
		if err != nil {
			return nil, nil, fmt.Errorf("count semantic ready and failed vectors: %w", err)
		}
		sourceCounts.CompletedVectorCount = completed
		sourceCounts.FailedVectorCount = failedVectors

		indexed, err := countProcessingRows(ctx, db, `SELECT COALESCE((SELECT indexed_vector_count
			FROM semantic_state
			WHERE source_key = ?
				AND model_id = ?
				AND vector_space_id = ?), 0)`, sourceKey, modelID, vectorSpaceID)
		if err != nil {
			return nil, nil, fmt.Errorf("count indexed semantic vectors: %w", err)
		}
		sourceCounts.IndexedVectorCount = indexed

		failedJobs, err := countProcessingRows(ctx, db, `SELECT COUNT(*)
			FROM semantic_index_jobs
			WHERE source_key = ?
				AND model_id = ?
				AND vector_space_id = ?
				AND status = 'failed'`, sourceKey, modelID, vectorSpaceID)
		if err != nil {
			return nil, nil, fmt.Errorf("count semantic failed index jobs: %w", err)
		}
		sourceCounts.FailedIndexJobCount = failedJobs

		bySource[sourceKey] = sourceCounts
		total.EligibleCount += sourceCounts.EligibleCount
		total.CompletedVectorCount += sourceCounts.CompletedVectorCount
		total.IndexedVectorCount += sourceCounts.IndexedVectorCount
		total.FailedVectorCount += sourceCounts.FailedVectorCount
		total.FailedIndexJobCount += sourceCounts.FailedIndexJobCount
	}
	return bySource, &total, nil
}

func (s *CatalogStore) AssetProcessingStats(ctx context.Context) (AssetProcessingStatsSnapshot, error) {
	if s == nil || s.db == nil {
		return AssetProcessingStatsSnapshot{}, nil
	}
	readDB, ok, err := s.openAdminReadDB(ctx)
	if err != nil {
		return AssetProcessingStatsSnapshot{}, err
	}
	if !ok {
		return AssetProcessingStatsSnapshot{}, nil
	}
	defer readDB.Close()

	rows, err := readDB.QueryContext(ctx, `SELECT scope_key, stage, variant, status, count, total_count, refreshed_at
		FROM asset_processing_stats
		ORDER BY scope_key, stage, variant, status`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return AssetProcessingStatsSnapshot{}, nil
		}
		return AssetProcessingStatsSnapshot{}, err
	}
	defer rows.Close()

	var snapshot AssetProcessingStatsSnapshot
	for rows.Next() {
		var stat AssetProcessingStat
		var refreshedAtRaw string
		if err := rows.Scan(
			&stat.ScopeKey,
			&stat.Stage,
			&stat.Variant,
			&stat.Status,
			&stat.Count,
			&stat.TotalCount,
			&refreshedAtRaw,
		); err != nil {
			return AssetProcessingStatsSnapshot{}, err
		}
		refreshedAt, err := time.Parse(time.RFC3339Nano, refreshedAtRaw)
		if err != nil {
			refreshedAt = time.Time{}
		}
		stat.RefreshedAt = refreshedAt.UTC()
		if snapshot.RefreshedAt.IsZero() || stat.RefreshedAt.After(snapshot.RefreshedAt) {
			snapshot.RefreshedAt = stat.RefreshedAt
		}
		snapshot.Stats = append(snapshot.Stats, stat)
	}
	if err := rows.Err(); err != nil {
		return AssetProcessingStatsSnapshot{}, err
	}
	return snapshot, nil
}

func (s *CatalogStore) ReplaceAssetProcessingStats(ctx context.Context, stats []AssetProcessingStat, refreshedAt time.Time) error {
	if s == nil || s.db == nil {
		return nil
	}
	if refreshedAt.IsZero() {
		refreshedAt = time.Now().UTC()
	}
	writeDB, err := s.openStatsWriteDB(ctx)
	if err != nil {
		return err
	}
	defer writeDB.Close()

	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_processing_stats`); err != nil {
		return err
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO asset_processing_stats (
		scope_key,
		stage,
		variant,
		status,
		count,
		total_count,
		refreshed_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, stat := range stats {
		statRefreshedAt := refreshedAt
		if !stat.RefreshedAt.IsZero() {
			statRefreshedAt = stat.RefreshedAt
		}
		if _, err := statement.ExecContext(ctx,
			strings.TrimSpace(stat.ScopeKey),
			strings.TrimSpace(stat.Stage),
			strings.TrimSpace(stat.Variant),
			strings.TrimSpace(stat.Status),
			max(stat.Count, 0),
			max(stat.TotalCount, 0),
			statRefreshedAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}
