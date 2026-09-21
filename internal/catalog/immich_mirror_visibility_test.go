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

	"github.com/rsahara/timich-agent/internal/config"
)

const visibilityTestSource = "1111111111111111"

func visibilityTestService(t *testing.T) *Service {
	t.Helper()
	return visibilityTestServiceIn(t, t.TempDir())
}

func visibilityTestServiceIn(t *testing.T, dataDir string) *Service {
	t.Helper()
	s, err := NewServiceWithOptions([]config.DatasourceConfig{{
		SourceKey: visibilityTestSource, Name: "Test", Kind: config.DatasourceKindImmichIndexed,
		URL: "http://immich.test", AccessToken: "test-key", Indexing: &config.DatasourceIndexingConfig{},
	}}, ServiceOptions{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func visibilityTestAsset(id, visibility string, hour int) map[string]any {
	return map[string]any{
		"id": id, "type": "IMAGE", "originalFileName": id + ".jpg", "visibility": visibility,
		"fileCreatedAt": "2026-09-01T00:00:00Z", "updatedAt": fmt.Sprintf("2026-09-01T%02d:00:00Z", hour),
	}
}

// Model the real visibility/update/capture filters without a listening socket.
type mirrorTestRoundTripFunc func(*http.Request) (*http.Response, error)

func (f mirrorTestRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func visibilityTestClient(t *testing.T, assets *[]map[string]any, requests *[]map[string]any) *http.Client {
	t.Helper()
	clock := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Second)
	return &http.Client{Transport: mirrorTestRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/server/ping" {
			// Retain the synthetic clock across pings, including unchanged deltas
			// and removal of the asset that last advanced it.
			for _, asset := range *assets {
				updated, _ := time.Parse(time.RFC3339Nano, asset["updatedAt"].(string))
				if updated.After(clock) {
					clock = updated.Add(time.Second).Truncate(time.Second)
				}
			}
			response := jsonResponse(`{"res":"pong"}`)
			response.Header.Set("Date", clock.Format(http.TimeFormat))
			return response, nil
		}
		if r.URL.Path != "/api/search/metadata" {
			return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, err
		}
		*requests = append(*requests, body)
		var selected []map[string]any
		for _, asset := range *assets {
			if (asset["isTrashed"] == true || asset["deletedAt"] != nil || asset["trashedAt"] != nil) && body["withDeleted"] != true {
				continue
			}
			if asset["visibility"] != body["visibility"] {
				continue
			}
			updated, _ := time.Parse(time.RFC3339Nano, asset["updatedAt"].(string))
			if value, ok := body["updatedAfter"].(string); ok {
				after, _ := time.Parse(time.RFC3339Nano, value)
				if updated.Before(after) {
					continue
				}
			}
			if value, ok := body["updatedBefore"].(string); ok {
				before, _ := time.Parse(time.RFC3339Nano, value)
				if updated.After(before) {
					continue
				}
			}
			if value, ok := body["takenBefore"].(string); ok {
				before, _ := time.Parse(time.RFC3339Nano, value)
				captured, _ := time.Parse(time.RFC3339Nano, asset["fileCreatedAt"].(string))
				if captured.After(before) {
					continue
				}
			}
			selected = append(selected, asset)
		}
		sort.Slice(selected, func(i, j int) bool {
			left, right := selected[i]["fileCreatedAt"].(string), selected[j]["fileCreatedAt"].(string)
			if left == right {
				return selected[i]["id"].(string) > selected[j]["id"].(string)
			}
			return left > right
		})
		if body["page"] != float64(1) {
			t.Fatalf("offset paging: %#v", body)
		}
		size := int(body["size"].(float64))
		items := selected[:min(size, len(selected))]
		var next any
		if len(selected) > size {
			next = 2
		}
		data, err := json.Marshal(map[string]any{"assets": map[string]any{"items": items, "total": len(selected), "nextPage": next}})
		return jsonResponse(string(data)), err
	})}
}

func TestVisibilityTestClientClockDoesNotRegress(t *testing.T) {
	assets := []map[string]any{visibilityTestAsset("latest", "timeline", 0)}
	assets[0]["updatedAt"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	var requests []map[string]any
	client := visibilityTestClient(t, &assets, &requests)
	ping := func() string {
		t.Helper()
		response, err := client.Get("http://immich.test/api/server/ping")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.Header.Get("Date")
	}
	want := ping()
	assets = nil
	if got := ping(); got != want {
		t.Fatalf("clock regressed after removing the latest asset: %s -> %s", want, got)
	}
}

func requireVisibilityGallery(t *testing.T, s *Service, want ...string) {
	t.Helper()
	page, err := s.CatalogPage(0, 60)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, item := range page.Items {
		got = append(got, item.ID)
	}
	sort.Strings(got)
	sort.Strings(want)
	if page.Total != len(want) || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("gallery = %v (total %d), want %v", got, page.Total, want)
	}
}

func TestImmichMirrorIncrementalVisibilityTransitions(t *testing.T) {
	for _, visibility := range []string{"hidden", "archive"} {
		t.Run(visibility, func(t *testing.T) {
			s := visibilityTestService(t)
			ctx := context.Background()
			assets := []map[string]any{visibilityTestAsset("changing", "timeline", 1), visibilityTestAsset("stable", "timeline", 0)}
			var requests []map[string]any
			s.client = visibilityTestClient(t, &assets, &requests)
			full, err := s.SyncMirror(ctx, MirrorSyncModeFull)
			if err != nil {
				t.Fatal(err)
			}
			requireVisibilityGallery(t, s, "changing", "stable")
			var canonicalBefore string
			if err := s.catalog.db.QueryRow(`SELECT canonical_asset_id FROM catalog_assets WHERE upstream_asset_id = 'changing'`).Scan(&canonicalBefore); err != nil {
				t.Fatal(err)
			}
			assets[0] = visibilityTestAsset("changing", visibility, 2)
			assets = append(assets, visibilityTestAsset("never-visible", visibility, 2))
			assets[0]["updatedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
			assets[2]["updatedAt"] = assets[0]["updatedAt"]
			requests = nil
			result, err := s.SyncMirror(ctx, MirrorSyncModeIncremental)
			if err != nil {
				t.Fatal(err)
			}
			if result.ActiveCount != 1 || result.OutOfScopeCount != 1 {
				t.Fatalf("unexpected result: %+v", result)
			}
			requireVisibilityGallery(t, s, "stable")
			var count int
			if err := s.catalog.db.QueryRow(`SELECT COUNT(*) FROM catalog_assets`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("catalog count=%d error=%v; unseen exclusions must not be inserted", count, err)
			}
			cursor, err := s.catalog.LatestSourceUpdatedAt(ctx, visibilityTestSource)
			if err != nil || cursor == nil || cursor.Format(time.RFC3339Nano) != assets[0]["updatedAt"] {
				t.Fatalf("excluded update did not advance cursor: %v, %v", cursor, err)
			}
			window := requests[0]["updatedBefore"]
			for _, request := range requests {
				if request["updatedAfter"] != full.SyncedThrough.Format(time.RFC3339Nano) || request["updatedBefore"] != window || window == nil {
					t.Fatalf("inconsistent incremental window: %#v", requests)
				}
			}
			assets[0] = visibilityTestAsset("changing", "timeline", 3)
			assets[0]["updatedAt"] = result.SyncedThrough.Add(time.Second).Format(time.RFC3339Nano)
			if _, err := s.SyncMirror(ctx, MirrorSyncModeIncremental); err != nil {
				t.Fatal(err)
			}
			requireVisibilityGallery(t, s, "changing", "stable")
			var canonicalAfter string
			if err := s.catalog.db.QueryRow(`SELECT canonical_asset_id FROM catalog_assets WHERE upstream_asset_id = 'changing'`).Scan(&canonicalAfter); err != nil || canonicalBefore != canonicalAfter {
				t.Fatalf("canonical identity changed: %q -> %q, %v", canonicalBefore, canonicalAfter, err)
			}
		})
	}
}

func TestImmichMirrorIncrementalVisibilityFailureIsAtomic(t *testing.T) {
	for _, failure := range []string{"hidden-page", "archive-query", "invalid-next-page", "database", "cursor-write", "commit"} {
		t.Run(failure, func(t *testing.T) {
			s := visibilityTestService(t)
			ctx := context.Background()
			assets := []map[string]any{visibilityTestAsset("changing", "timeline", 1)}
			var requests []map[string]any
			client := visibilityTestClient(t, &assets, &requests)
			s.client = client
			full, err := s.SyncMirror(ctx, MirrorSyncModeFull)
			if err != nil {
				t.Fatal(err)
			}
			assets = []map[string]any{visibilityTestAsset("new", "timeline", 3), visibilityTestAsset("changing", "hidden", 2), visibilityTestAsset("unseen", "hidden", 2)}
			for _, asset := range assets {
				asset["updatedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
			}
			if failure == "hidden-page" {
				for i := 0; i < maxPageSize; i++ {
					asset := visibilityTestAsset(fmt.Sprintf("unseen-page-%d", i), "hidden", 2)
					asset["updatedAt"] = assets[0]["updatedAt"]
					asset["fileCreatedAt"] = time.Date(2026, 8, 1, 0, i, 0, 0, time.UTC).Format(time.RFC3339Nano)
					assets = append(assets, asset)
				}
			}
			if failure == "database" {
				if _, err := s.catalog.db.Exec(`CREATE TRIGGER fail_exclusion BEFORE UPDATE OF visibility_status ON catalog_assets WHEN NEW.visibility_status='out_of_scope' BEGIN SELECT RAISE(ABORT, 'injected exclusion failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "cursor-write" {
				if _, err := s.catalog.db.Exec(`CREATE TRIGGER fail_cursor BEFORE UPDATE OF synced_through ON immich_mirror_state BEGIN SELECT RAISE(ABORT, 'injected cursor failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "commit" {
				for _, statement := range []string{
					`CREATE TABLE commit_guard (source_key TEXT REFERENCES immich_mirror_state(source_key) DEFERRABLE INITIALLY DEFERRED)`,
					`CREATE TRIGGER fail_commit AFTER UPDATE OF synced_through ON immich_mirror_state BEGIN INSERT INTO commit_guard VALUES ('missing'); END`,
				} {
					if _, err := s.catalog.db.Exec(statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			s.client = &http.Client{Transport: mirrorTestRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/server/ping" {
					return client.Transport.RoundTrip(r)
				}
				response, err := client.Transport.RoundTrip(r)
				body := requests[len(requests)-1]
				fail := failure == "hidden-page" && body["visibility"] == "hidden" && body["takenBefore"] != nil || failure == "archive-query" && body["visibility"] == "archive"
				if fail {
					response.StatusCode = http.StatusServiceUnavailable
				}
				if failure == "invalid-next-page" && body["visibility"] == "hidden" {
					_ = response.Body.Close()
					return jsonResponse(`{"assets":{"items":[],"nextPage":3}}`), nil
				}
				return response, err
			})}
			if _, err := s.SyncMirror(ctx, MirrorSyncModeIncremental); err == nil {
				t.Fatal("expected injected sync failure")
			}
			requireVisibilityGallery(t, s, "changing")
			requireMirrorCursor(t, s.catalog, visibilityTestSource, full.SyncedThrough)
			cursor, err := s.catalog.LatestSourceUpdatedAt(ctx, visibilityTestSource)
			if err != nil || cursor == nil || cursor.Hour() != 1 {
				t.Fatalf("failed sync advanced cursor: %v, %v", cursor, err)
			}
			status, err := s.catalog.Status(ctx, visibilityTestSource)
			if err != nil || status.LastIncrementalSyncAt != nil {
				t.Fatalf("failed sync recorded completion: %+v, %v", status, err)
			}
			for _, name := range []string{"fail_exclusion", "fail_cursor", "fail_commit"} {
				if _, err := s.catalog.db.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
					t.Fatal(err)
				}
			}
			s.client = client
			requests = nil
			retry, err := s.SyncMirror(ctx, MirrorSyncModeIncremental)
			if err != nil {
				t.Fatal(err)
			}
			if requests[0]["updatedAfter"] != full.SyncedThrough.Format(time.RFC3339Nano) {
				t.Fatalf("retry skipped failed window: %#v", requests[0])
			}
			requireMirrorCursor(t, s.catalog, visibilityTestSource, retry.SyncedThrough)
			requireVisibilityGallery(t, s, "new")
		})
	}
}

func TestImmichMirrorIncrementalDetailVisibilityAndCursor(t *testing.T) {
	for _, visibility := range []string{"timeline", "hidden"} {
		t.Run(visibility, func(t *testing.T) {
			s := visibilityTestService(t)
			ctx := context.Background()
			assets := []map[string]any{visibilityTestAsset("changing", "timeline", 1)}
			var requests []map[string]any
			client := visibilityTestClient(t, &assets, &requests)
			s.client = client
			if _, err := s.SyncMirror(ctx, MirrorSyncModeFull); err != nil {
				t.Fatal(err)
			}
			assets[0] = visibilityTestAsset("changing", "timeline", 2)
			assets[0]["updatedAt"] = time.Now().UTC().Format(time.RFC3339Nano)
			s.client = &http.Client{Transport: mirrorTestRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/assets/changing" {
					detail := visibilityTestAsset("changing", visibility, 3)
					detail["updatedAt"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
					raw, _ := json.Marshal(detail)
					return jsonResponse(string(raw)), nil
				}
				return client.Transport.RoundTrip(r)
			})}
			datasource, err := s.mirrorDatasource("")
			if err != nil {
				t.Fatal(err)
			}
			datasource.Indexing.MetadataDetailLimit = 1
			result, err := s.syncMirrorDatasource(ctx, datasource, MirrorSyncModeIncremental)
			if err != nil {
				t.Fatal(err)
			}
			requireMirrorCursor(t, s.catalog, visibilityTestSource, result.SyncedThrough)
			if visibility == "hidden" {
				requireVisibilityGallery(t, s)
			} else {
				requireVisibilityGallery(t, s, "changing")
			}
			cursor, err := s.catalog.LatestSourceUpdatedAt(ctx, visibilityTestSource)
			if err != nil || cursor == nil || cursor.Format(time.RFC3339Nano) != assets[0]["updatedAt"] {
				t.Fatalf("detail advanced cursor outside metadata window: %v, %v", cursor, err)
			}
		})
	}
}

func TestImmichMirrorExclusionPreservesActiveDuplicateSource(t *testing.T) {
	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	const otherSource = "2222222222222222"
	asset := ImmichMirrorAsset{
		UpstreamAssetID: "shared", MediaType: "image", Filename: "shared.jpg", CapturedAt: now,
		ContentSHA1Hex: "0123456789abcdef0123456789abcdef01234567", ContentSizeBytes: 1234,
		CanonicalContentSHA1Hex: "0123456789abcdef0123456789abcdef01234567", CanonicalContentSizeBytes: 1234,
		SourceUpdatedAt: &now,
	}
	for _, source := range []string{visibilityTestSource, otherSource} {
		if _, err := store.ReplaceFull(ctx, source, []ImmichMirrorAsset{asset}, 0, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, source := range []string{visibilityTestSource, otherSource, canonicalSemanticCorpusSourceKey} {
		if _, err := store.db.Exec(`INSERT INTO semantic_state (source_key, model_id, vector_space_id, status, embedding_dim, updated_at) VALUES (?, 'test', 'test', 'ready', 1, ?)`, source, formatCatalogTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	updated := now.Add(time.Hour)
	if _, err := store.MergeIncremental(ctx, visibilityTestSource, []ImmichMirrorAsset{{UpstreamAssetID: "shared", Excluded: true, SourceUpdatedAt: &updated}}, now); err != nil {
		t.Fatal(err)
	}
	var visibility, primary string
	if err := store.db.QueryRow(`SELECT visibility_status, primary_source_key FROM catalog_canonical_assets`).Scan(&visibility, &primary); err != nil || visibility != "active" || primary != otherSource {
		t.Fatalf("active duplicate lost: %s / %s, %v", visibility, primary, err)
	}
	for _, source := range []string{visibilityTestSource, otherSource, canonicalSemanticCorpusSourceKey} {
		var generation int
		if err := store.db.QueryRow(`SELECT asset_generation FROM semantic_state WHERE source_key = ?`, source).Scan(&generation); err != nil {
			t.Fatal(err)
		}
		if source == otherSource && generation != 0 || source != otherSource && generation == 0 {
			t.Fatalf("unexpected semantic invalidation for %s: %d", source, generation)
		}
	}
}
