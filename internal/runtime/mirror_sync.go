package runtime

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
)

const (
	defaultDatasourceMirrorSyncInterval       = 30 * time.Minute
	defaultDatasourceMirrorReconciliationTime = "02:00"
	datasourceMirrorIncrementalSyncTimeout    = 5 * time.Minute
	datasourceMirrorFullSyncTimeout           = 12 * time.Hour
)

type datasourceMirrorSchedule struct {
	Interval             time.Duration
	DailyFullSweepWindow string
}

// StartDatasourceMirrorSync starts automatic Immich mirror sync for the serving
// Agent process. Tests can call SyncPrimaryDatasourceMirror directly.
func (a *AgentRuntime) StartDatasourceMirrorSync() {
	if a == nil {
		return
	}
	if _, ok := a.datasourceMirrorSchedule(); !ok {
		return
	}
	a.mirrorSchedulerMu.Lock()
	defer a.mirrorSchedulerMu.Unlock()
	if a.mirrorCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.mirrorCancel = cancel
	a.mirrorWG.Add(1)
	go a.runDatasourceMirrorSyncLoop(ctx)
}

func (a *AgentRuntime) stopDatasourceMirrorSync() {
	a.mirrorSchedulerMu.Lock()
	cancel := a.mirrorCancel
	a.mirrorCancel = nil
	a.mirrorSchedulerMu.Unlock()
	if cancel != nil {
		cancel()
		a.mirrorWG.Wait()
	}
}

func (a *AgentRuntime) runDatasourceMirrorSyncLoop(ctx context.Context) {
	defer a.mirrorWG.Done()
	a.runScheduledDatasourceMirrorSync(ctx, "startup")

	var intervalTicker *time.Ticker
	var intervalC <-chan time.Time
	var dailyTimer *time.Timer
	var dailyC <-chan time.Time
	defer func() {
		if intervalTicker != nil {
			intervalTicker.Stop()
		}
		if dailyTimer != nil {
			dailyTimer.Stop()
		}
	}()

	resetSchedule := func() bool {
		schedule, ok := a.datasourceMirrorSchedule()
		if !ok {
			return false
		}
		if intervalTicker != nil {
			intervalTicker.Stop()
		}
		intervalTicker = time.NewTicker(schedule.Interval)
		intervalC = intervalTicker.C

		if dailyTimer != nil {
			dailyTimer.Stop()
			dailyTimer = nil
			dailyC = nil
		}
		if next, ok := nextDatasourceMirrorDailyFullSweep(time.Now(), a.uploadLocation(), schedule.DailyFullSweepWindow); ok {
			dailyTimer = time.NewTimer(time.Until(next))
			dailyC = dailyTimer.C
		}
		return true
	}
	if !resetSchedule() {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-intervalC:
			a.runScheduledDatasourceMirrorSync(ctx, "interval")
			if !resetSchedule() {
				return
			}
		case <-dailyC:
			a.runScheduledDatasourceMirrorSync(ctx, "daily_full_sweep")
			if !resetSchedule() {
				return
			}
		}
	}
}

func (a *AgentRuntime) runScheduledDatasourceMirrorSync(ctx context.Context, reason string) {
	if _, ok := a.datasourceMirrorSchedule(); !ok {
		return
	}
	a.syncConfiguredDatasourceMirrors(ctx, reason)
}

func (a *AgentRuntime) syncConfiguredDatasourceMirrors(ctx context.Context, reason string) {
	a.mirrorSyncMu.Lock()
	a.datasourceMirrorSyncActive.Add(1)
	defer func() {
		a.mirrorSyncMu.Unlock()
		a.datasourceMirrorSyncActive.Add(-1)
	}()

	catalogService := a.catalogService()
	if catalogService == nil {
		return
	}
	sourceKeys := catalogService.MirrorDatasourceSourceKeys()
	if len(sourceKeys) == 0 {
		return
	}
	for _, sourceKey := range sourceKeys {
		if ctx.Err() != nil {
			return
		}
		mode := a.datasourceMirrorSyncModeForSource(ctx, sourceKey, time.Now())
		syncCtx, cancel := context.WithTimeout(ctx, DatasourceMirrorSyncTimeoutForMode(mode))
		result, err := catalogService.SyncDatasourceMirror(syncCtx, sourceKey, mode)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("timich-agent datasource mirror sync failed reason=%s source_key=%s error=%v", reason, sourceKey, err)
			continue
		}
		log.Printf(
			"timich-agent datasource mirror sync completed reason=%s source_key=%s mode=%s fetched=%d active=%d out_of_scope=%d missing=%d",
			reason,
			sourceKey,
			result.Mode,
			result.FetchedCount,
			result.ActiveCount,
			result.OutOfScopeCount,
			result.MissingCount,
		)
		a.rememberRemoteDatasourceSyncSnapshot(catalogService, sourceKey, result)
		a.notifyDatasourceMirrorSyncCompleted()
	}
}

func (a *AgentRuntime) notifyDatasourceMirrorSyncCompleted() {
	if a == nil {
		return
	}
	a.schedulerWorkStateMarkDirty()
	a.wakeBackgroundWorkerScheduler()
}

// Every automatic wake checks the last successful reconciliation. This catches
// missed daily runs after downtime, retries failures, and lets a manual run
// satisfy the current daily occurrence without repeating it on restart.
func (a *AgentRuntime) datasourceMirrorSyncModeForSource(ctx context.Context, sourceKey string, now time.Time) string {
	catalogService := a.catalogService()
	if catalogService == nil {
		return catalog.MirrorSyncModeFull
	}
	status, err := catalogService.MirrorStatusForDatasource(ctx, sourceKey)
	if err != nil || status.LastFullSyncAt == nil {
		return catalog.MirrorSyncModeFull
	}
	a.mu.RLock()
	location := localScheduleLocation(a.config.Timezone)
	clock := defaultDatasourceMirrorReconciliationTime
	for _, datasource := range a.config.Datasources {
		if datasource.SourceKey == sourceKey {
			clock = datasourceMirrorReconciliationTime(datasource)
			break
		}
	}
	a.mu.RUnlock()
	if status.LastFullSyncAt.Before(latestLocalDailySchedule(now, location, clock)) {
		return catalog.MirrorSyncModeFull
	}
	return catalog.MirrorSyncModeIncremental
}

// DatasourceMirrorSyncTimeoutForMode returns the request budget for a mirror sync mode.
func DatasourceMirrorSyncTimeoutForMode(mode string) time.Duration {
	switch strings.TrimSpace(mode) {
	case "", catalog.MirrorSyncModeFull:
		return datasourceMirrorFullSyncTimeout
	default:
		return datasourceMirrorIncrementalSyncTimeout
	}
}

func (a *AgentRuntime) datasourceMirrorSchedule() (datasourceMirrorSchedule, bool) {
	return a.datasourceMirrorScheduleAt(time.Now())
}

func (a *AgentRuntime) datasourceMirrorScheduleAt(now time.Time) (datasourceMirrorSchedule, bool) {
	if a == nil {
		return datasourceMirrorSchedule{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.config.Datasources) == 0 {
		return datasourceMirrorSchedule{}, false
	}
	interval := time.Duration(0)
	dailyFullSweepWindow := ""
	var earliestReconciliation time.Time
	location := localScheduleLocation(a.config.Timezone)
	for _, datasource := range a.config.Datasources {
		if datasource.Kind != config.DatasourceKindImmichIndexed {
			continue
		}
		datasourceInterval := defaultDatasourceMirrorSyncInterval
		if datasource.Indexing != nil {
			if rawInterval := strings.TrimSpace(datasource.Indexing.Phase0SyncInterval); rawInterval != "" {
				parsed, err := time.ParseDuration(rawInterval)
				if err != nil || parsed <= 0 {
					return datasourceMirrorSchedule{}, false
				}
				datasourceInterval = parsed
			}
		}
		if interval == 0 || datasourceInterval < interval {
			interval = datasourceInterval
		}
		clock := datasourceMirrorReconciliationTime(datasource)
		next, ok := nextDatasourceMirrorDailyFullSweep(now, location, clock)
		if !ok {
			return datasourceMirrorSchedule{}, false
		}
		if earliestReconciliation.IsZero() || next.Before(earliestReconciliation) {
			earliestReconciliation = next
			dailyFullSweepWindow = clock
		}
	}
	if interval == 0 {
		return datasourceMirrorSchedule{}, false
	}
	return datasourceMirrorSchedule{
		Interval:             interval,
		DailyFullSweepWindow: dailyFullSweepWindow,
	}, true
}

func datasourceMirrorReconciliationTime(datasource config.DatasourceConfig) string {
	if datasource.Indexing != nil {
		if clock := strings.TrimSpace(datasource.Indexing.DailyFullSweepWindow); clock != "" {
			return clock
		}
	}
	return defaultDatasourceMirrorReconciliationTime
}

func nextDatasourceMirrorDailyFullSweep(now time.Time, location *time.Location, window string) (time.Time, bool) {
	hour, minute, ok := parseDatasourceMirrorDailyFullSweepWindow(window)
	if !ok {
		return time.Time{}, false
	}
	if location == nil {
		location = time.Local
	}
	localNow := now.In(location)
	next := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), hour, minute, 0, 0, location)
	if !next.After(localNow) {
		next = next.AddDate(0, 0, 1)
	}
	return next, true
}

func parseDatasourceMirrorDailyFullSweepWindow(window string) (int, int, bool) {
	window = strings.TrimSpace(window)
	if window == "" || len(window) != len("00:00") || window[2] != ':' {
		return 0, 0, false
	}
	hour := int(window[0]-'0')*10 + int(window[1]-'0')
	minute := int(window[3]-'0')*10 + int(window[4]-'0')
	if window[0] < '0' || window[0] > '9' ||
		window[1] < '0' || window[1] > '9' ||
		window[3] < '0' || window[3] > '9' ||
		window[4] < '0' || window[4] > '9' ||
		hour > 23 ||
		minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}
