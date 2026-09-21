package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

type mirrorWindowFixture struct {
	now          time.Time
	assets       []immichAsset
	requests     []map[string]any
	pings        int
	afterPage    func(map[string]any)
	clockHeaders http.Header
}

func (f *mirrorWindowFixture) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("x-api-key") != "test-key" {
		return nil, fmt.Errorf("missing datasource credentials")
	}
	if r.URL.Path == "/api/server/ping" {
		f.pings++
		if r.Header.Get("Cache-Control") != "no-cache, no-store" {
			return nil, fmt.Errorf("clock request permits caching")
		}
		response := jsonResponse(`{"res":"pong"}`)
		response.Header.Set("Date", f.now.UTC().Format(http.TimeFormat))
		if f.clockHeaders != nil {
			response.Header = f.clockHeaders.Clone()
		}
		return response, nil
	}
	if r.URL.Path != "/api/search/metadata" {
		return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	f.requests = append(f.requests, body)
	if body["page"] != float64(1) {
		return nil, fmt.Errorf("offset paging was used: %v", body["page"])
	}
	var selected []immichAsset
	for _, asset := range f.assets {
		if asset.Visibility != body["visibility"] {
			continue
		}
		eligible := true
		for _, filter := range []struct {
			name  string
			value time.Time
			lower bool
		}{
			{"updatedAfter", asset.UpdatedAt.Time, true},
			{"updatedBefore", asset.UpdatedAt.Time, false},
			{"takenBefore", asset.FileCreatedAt.Time, false},
		} {
			if text, ok := body[filter.name].(string); ok {
				bound, err := time.Parse(time.RFC3339Nano, text)
				if err != nil {
					return nil, err
				}
				if filter.lower && filter.value.Before(bound) || !filter.lower && filter.value.After(bound) {
					eligible = false
				}
			}
		}
		if eligible {
			selected = append(selected, asset)
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].FileCreatedAt.Equal(selected[j].FileCreatedAt.Time) {
			return selected[i].ID > selected[j].ID
		}
		return selected[i].FileCreatedAt.After(selected[j].FileCreatedAt.Time)
	})
	size := int(body["size"].(float64))
	var next any
	if len(selected) > size {
		next = 2
		selected = selected[:size]
	}
	raw, err := json.Marshal(map[string]any{"assets": map[string]any{"items": selected, "nextPage": next, "total": len(selected)}})
	if f.afterPage != nil {
		f.afterPage(body)
	}
	return jsonResponse(string(raw)), err
}

func mirrorWindowAsset(id string, captured, updated time.Time) immichAsset {
	return immichAsset{ID: id, Type: "IMAGE", OriginalFileName: id + ".jpg", Visibility: "timeline",
		FileCreatedAt: flexibleTime{Time: captured}, UpdatedAt: &flexibleTime{Time: updated}}
}

func TestImmichMirrorWindowUsesUpstreamClock(t *testing.T) {
	for _, skew := range []time.Duration{-30 * time.Second, 30 * time.Second} {
		t.Run(skew.String(), func(t *testing.T) {
			s := visibilityTestService(t)
			upstream := time.Now().UTC().Truncate(time.Second).Add(skew)
			f := &mirrorWindowFixture{now: upstream, assets: []immichAsset{mirrorWindowAsset("stable", upstream.Add(-time.Hour), upstream.Add(-time.Hour))}}
			s.client = &http.Client{Transport: f}
			full, err := s.SyncMirror(context.Background(), MirrorSyncModeFull)
			if err != nil {
				t.Fatal(err)
			}
			if !full.SyncedThrough.Equal(upstream) || full.StartedAt.Equal(upstream) {
				t.Fatalf("clock domains mixed: %+v", full)
			}
			f.assets = append(f.assets, mirrorWindowAsset("fresh", upstream, upstream.Add(time.Second)))
			f.now = upstream.Add(2 * time.Second)
			if _, err := s.SyncMirror(context.Background(), MirrorSyncModeIncremental); err != nil {
				t.Fatal(err)
			}
			requireVisibilityGallery(t, s, "fresh", "stable")
			requireMirrorCursor(t, s.catalog, visibilityTestSource, f.now)
		})
	}
}

func TestImmichMirrorWindowConcurrentUpdateKeepsNextPageBoundary(t *testing.T) {
	s := visibilityTestService(t)
	upstream := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	f := &mirrorWindowFixture{now: upstream, assets: []immichAsset{mirrorWindowAsset("stable", upstream.Add(-time.Hour), upstream.Add(-time.Hour))}}
	s.client = &http.Client{Transport: f}
	if _, err := s.SyncMirror(context.Background(), MirrorSyncModeFull); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxPageSize+1; i++ {
		f.assets = append(f.assets, mirrorWindowAsset(fmt.Sprintf("new-%03d", i), upstream.Add(-time.Duration(i)*time.Second), upstream.Add(time.Second)))
	}
	f.now = upstream.Add(2 * time.Second)
	mutated := false
	f.afterPage = func(body map[string]any) {
		if !mutated && body["updatedAfter"] != nil && body["visibility"] == "timeline" {
			f.assets[1].UpdatedAt = &flexibleTime{Time: f.now.Add(time.Second)}
			mutated = true
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := s.SyncMirror(context.Background(), MirrorSyncModeIncremental); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(2 * time.Second)
	}
	page, err := s.CatalogPage(0, maxPageSize)
	if err != nil || page.Total != maxPageSize+2 {
		t.Fatalf("gallery has %d entries, want %d: %v", page.Total, maxPageSize+2, err)
	}
}

func TestImmichMirrorWindowInvalidClockPreservesCatalog(t *testing.T) {
	for _, bad := range []string{"missing", "invalid", "cached", "backward"} {
		t.Run(bad, func(t *testing.T) {
			s := visibilityTestService(t)
			now := time.Now().UTC().Truncate(time.Second)
			f := &mirrorWindowFixture{now: now, assets: []immichAsset{mirrorWindowAsset("stable", now, now)}}
			s.client = &http.Client{Transport: f}
			if _, err := s.SyncMirror(context.Background(), MirrorSyncModeFull); err != nil {
				t.Fatal(err)
			}
			f.requests = nil
			switch bad {
			case "missing":
				f.clockHeaders = http.Header{}
			case "invalid":
				f.clockHeaders = http.Header{"Date": []string{"not-a-date"}}
			case "cached":
				f.clockHeaders = http.Header{"Date": []string{now.Format(http.TimeFormat)}, "Age": []string{"60"}}
			case "backward":
				f.now = now.Add(-time.Second)
			}
			if _, err := s.SyncMirror(context.Background(), MirrorSyncModeIncremental); err == nil {
				t.Fatal("invalid clock accepted")
			}
			if len(f.requests) != 0 {
				t.Fatal("queried assets despite invalid clock")
			}
			requireMirrorCursor(t, s.catalog, visibilityTestSource, now)
			requireVisibilityGallery(t, s, "stable")
		})
	}
}

func TestImmichMirrorPagingLargeBatchesAndCaptureTies(t *testing.T) {
	for _, group := range []int{1, 20, 600} {
		t.Run(fmt.Sprint(group), func(t *testing.T) {
			s := visibilityTestService(t)
			now := time.Now().UTC().Truncate(time.Second)
			f := &mirrorWindowFixture{now: now}
			for i := 0; i < 5000; i++ {
				f.assets = append(f.assets, mirrorWindowAsset(fmt.Sprintf("asset-%05d", i), now.Add(-time.Duration(i/group)*time.Second), now))
			}
			s.client = &http.Client{Transport: f}
			datasource, err := s.mirrorDatasource("")
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			assets, err := s.fetchImmichMirrorAssets(context.Background(), datasource, immichMirrorFetchOptions{})
			if err != nil || len(assets) != 5000 {
				t.Fatalf("fetched %d assets: %v", len(assets), err)
			}
			if len(f.requests) > 30 {
				t.Fatalf("too many requests: %d", len(f.requests))
			}
			t.Logf("5000 assets, capture group %d: %d requests, %v", group, len(f.requests), time.Since(start))
		})
	}
}

func TestImmichMirrorDenseTimestampDoesNotAdvanceCheckpoint(t *testing.T) {
	s := visibilityTestService(t)
	now := time.Now().UTC().Truncate(time.Second)
	f := &mirrorWindowFixture{now: now}
	s.client = &http.Client{Transport: f}
	if _, err := s.SyncMirror(context.Background(), MirrorSyncModeFull); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < immichMirrorMaxPageSize+1; i++ {
		f.assets = append(f.assets, mirrorWindowAsset(fmt.Sprintf("asset-%04d", i), now, now.Add(time.Second)))
	}
	f.now = now.Add(2 * time.Second)
	f.requests = nil
	if _, err := s.SyncMirror(context.Background(), MirrorSyncModeIncremental); err == nil || !strings.Contains(err.Error(), "checkpoint was not advanced") {
		t.Fatalf("dense window: %v", err)
	}
	if len(f.requests) != 3 {
		t.Fatalf("dense window retried excessively: %d requests", len(f.requests))
	}
	requireMirrorCursor(t, s.catalog, visibilityTestSource, now)
	requireVisibilityGallery(t, s)
}

func TestImmichMirrorUnchangedWindowCost(t *testing.T) {
	s := visibilityTestService(t)
	now := time.Now().UTC().Truncate(time.Second)
	f := &mirrorWindowFixture{now: now, assets: []immichAsset{mirrorWindowAsset("stable", now.Add(-time.Hour), now.Add(-time.Hour))}}
	s.client = &http.Client{Transport: f}
	if _, err := s.SyncMirror(context.Background(), MirrorSyncModeFull); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TRIGGER reject_unchanged_gallery BEFORE INSERT ON catalog_gallery_timeline BEGIN SELECT RAISE(ABORT, 'unexpected gallery rebuild'); END`,
		`CREATE TRIGGER reject_unchanged_generation BEFORE UPDATE OF generation ON catalog_canonical_state BEGIN SELECT RAISE(ABORT, 'unexpected canonical generation'); END`,
		`CREATE TRIGGER reject_unchanged_semantics BEFORE UPDATE OF asset_generation ON semantic_state BEGIN SELECT RAISE(ABORT, 'unexpected semantic invalidation'); END`,
	} {
		if _, err := s.catalog.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	f.requests, f.pings = nil, 0
	f.now = now.Add(time.Minute)
	result, err := s.SyncMirror(context.Background(), MirrorSyncModeIncremental)
	if err != nil || result.FetchedCount != 0 || f.pings != 1 || len(f.requests) != 3 {
		t.Fatalf("unchanged delta: %+v, %v, pings=%d, metadata=%d", result, err, f.pings, len(f.requests))
	}
	requireMirrorCursor(t, s.catalog, visibilityTestSource, f.now)
}

func TestImmichMirrorLegacyClockRequiresExplicitFull(t *testing.T) {
	s := visibilityTestService(t)
	now := time.Now().UTC().Truncate(time.Second)
	f := &mirrorWindowFixture{now: now, assets: []immichAsset{mirrorWindowAsset("stable", now, now)}}
	s.client = &http.Client{Transport: f}
	if _, err := s.SyncMirror(context.Background(), MirrorSyncModeFull); err != nil {
		t.Fatal(err)
	}
	var originalID string
	if err := s.catalog.db.QueryRow(`SELECT canonical_asset_id FROM catalog_assets`).Scan(&originalID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.catalog.db.Exec(`UPDATE immich_mirror_state SET sync_clock = ''`); err != nil {
		t.Fatal(err)
	}
	status, err := s.MirrorStatus(context.Background())
	if err != nil || status.Status != "error" || status.LastError != immichMirrorReconciliationRequired {
		t.Fatalf("migration must be visible: %+v, %v", status, err)
	}
	f.requests, f.pings = nil, 0
	if _, err := s.SyncMirror(context.Background(), MirrorSyncModeIncremental); err == nil || err.Error() != immichMirrorReconciliationRequired {
		t.Fatalf("legacy delta: %v", err)
	}
	if f.pings != 0 || len(f.requests) != 0 {
		t.Fatal("migration initiated unexpected upstream work")
	}
	if _, err := s.SyncMirror(context.Background(), MirrorSyncModeFull); err != nil {
		t.Fatal(err)
	}
	status, err = s.MirrorStatus(context.Background())
	if err != nil || status.LastError != "" {
		t.Fatalf("full sync did not clear migration notice: %+v, %v", status, err)
	}
	var preservedID string
	if err := s.catalog.db.QueryRow(`SELECT canonical_asset_id FROM catalog_assets`).Scan(&preservedID); err != nil || preservedID != originalID {
		t.Fatalf("migration changed identity: %q, %v", preservedID, err)
	}
	requireMirrorCursor(t, s.catalog, visibilityTestSource, now)
}

func TestImmichMirrorLimitedFullAcceptsDensePrefix(t *testing.T) {
	s := visibilityTestService(t)
	now := time.Now().UTC().Truncate(time.Second)
	f := &mirrorWindowFixture{now: now}
	for i := 0; i < immichMirrorMaxPageSize+1; i++ {
		f.assets = append(f.assets, mirrorWindowAsset(fmt.Sprintf("asset-%04d", i), now, now))
	}
	s.client = &http.Client{Transport: f}
	datasource, err := s.mirrorDatasource("")
	if err != nil {
		t.Fatal(err)
	}
	assets, err := s.fetchImmichMirrorAssets(context.Background(), datasource, immichMirrorFetchOptions{LatestAssetLimit: 900})
	if err != nil || len(assets) != 900 || len(f.requests) != 3 {
		t.Fatalf("limited dense full fetched %d, requests=%d: %v", len(assets), len(f.requests), err)
	}
}

func TestImmichMirrorPagerSubmillisecondBoundaryAndBoundedMemory(t *testing.T) {
	s := visibilityTestService(t)
	now := time.Now().UTC().Truncate(time.Second)
	f := &mirrorWindowFixture{now: now}
	for i := 0; i < 1500; i++ {
		f.assets = append(f.assets, mirrorWindowAsset(fmt.Sprintf("asset-%04d", i), now.Add(-time.Duration(i)*10*time.Microsecond), now))
	}
	s.client = &http.Client{Transport: f}
	datasource, err := s.mirrorDatasource("")
	if err != nil {
		t.Fatal(err)
	}
	pager := immichMirrorPager{service: s, datasource: datasource}
	seen := make(map[string]bool)
	for !pager.done {
		items, err := pager.next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, asset := range items {
			if seen[asset.ID] {
				t.Fatalf("duplicate asset %s", asset.ID)
			}
			seen[asset.ID] = true
		}
		if len(pager.seen) > immichMirrorMaxPageSize {
			t.Fatalf("deduplication retained %d entries", len(pager.seen))
		}
	}
	if len(seen) != len(f.assets) {
		t.Fatalf("sub-ms boundary lost assets: %d of %d", len(seen), len(f.assets))
	}
}
