package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestImmichAssetDurationCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, field, want string
		absent, invalid   bool
	}{
		{name: "missing", absent: true},
		{name: "null", field: `,"duration":null`, absent: true},
		{name: "v2 string", field: `,"duration":"0:00:05.250000"`, want: "0:00:05.250000"},
		{name: "empty string", field: `,"duration":""`},
		{name: "v3 video", field: `,"duration":5250`, want: "0:00:05.250"},
		{name: "v3 zero", field: `,"duration":0`, want: "0:00:00.000"},
		{name: "v3 hours", field: `,"duration":3723456`, want: "1:02:03.456"},
		{name: "large integer", field: `,"duration":9223372036854775807`, want: "2562047788015:12:55.807"},
		{name: "negative", field: `,"duration":-1`, invalid: true},
		{name: "fractional", field: `,"duration":1.5`, invalid: true},
		{name: "overflow", field: `,"duration":9223372036854775808`, invalid: true},
		{name: "boolean", field: `,"duration":true`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := "stale"
			asset := immichAsset{Duration: &previous}
			err := json.Unmarshal([]byte(`{"id":"asset","type":"VIDEO","fileCreatedAt":"2026-09-01T00:00:00Z"`+tc.field+`}`), &asset)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid duration accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if asset.ID != "asset" || asset.Type != "VIDEO" || asset.FileCreatedAt.IsZero() {
				t.Fatalf("other metadata lost: %#v", asset)
			}
			if tc.absent {
				if asset.Duration != nil {
					t.Fatal("missing/null duration retained stale value")
				}
			} else if asset.Duration == nil || *asset.Duration != tc.want {
				t.Fatalf("duration = %v, want %q", asset.Duration, tc.want)
			}
		})
	}
}

func TestImmichV3PassthroughAndMirrorKeepClientContract(t *testing.T) {
	for _, kind := range []string{config.DatasourceKindImmich, config.DatasourceKindImmichIndexed} {
		t.Run(kind, func(t *testing.T) {
			service, err := NewServiceWithOptions([]config.DatasourceConfig{{
				SourceKey: "1111111111111111", Name: "Test", Kind: kind,
				URL: "http://immich.test", AccessToken: "test-key",
				Indexing: &config.DatasourceIndexingConfig{MetadataDetailLimit: 2},
			}}, ServiceOptions{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			const video = `{"id":"video","type":"VIDEO","originalFileName":"video.mp4","fileCreatedAt":"2026-09-02T00:00:00Z","updatedAt":"2026-09-02T00:01:00Z","duration":5250,"visibility":"timeline"}`
			const gif = `{"id":"gif","type":"IMAGE","originalFileName":"animation.gif","fileCreatedAt":"2026-09-01T00:00:00Z","updatedAt":"2026-09-01T00:01:00Z","duration":2500,"visibility":"timeline"}`
			service.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						return nil, err
					}
					if body["updatedAfter"] != nil && (body["visibility"] == "hidden" || body["visibility"] == "archive") {
						return jsonResponse(`{"assets":{"total":0,"nextPage":null,"items":[]}}`), nil
					}
					if body["visibility"] != "timeline" {
						t.Errorf("%s did not restrict visibility: %#v", r.URL.Path, body)
					}
				}
				switch r.URL.Path {
				case "/api/search/metadata", "/api/search/smart":
					return jsonResponse(`{"assets":{"total":2,"nextPage":null,"items":[` + video + `,` + gif + `]}}`), nil
				case "/api/search/statistics":
					return jsonResponse(`{"images":1,"videos":1}`), nil
				case "/api/assets/video":
					return jsonResponse(video), nil
				case "/api/assets/gif":
					return jsonResponse(gif), nil
				default:
					return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
				}
			})}
			if err := service.Probe(context.Background()); err != nil {
				t.Fatal(err)
			}
			if kind == config.DatasourceKindImmichIndexed {
				for _, mode := range []string{MirrorSyncModeFull, MirrorSyncModeIncremental} {
					if _, err := service.SyncMirror(context.Background(), mode); err != nil {
						t.Fatal(err)
					}
				}
			}
			page, err := service.CatalogPage(0, 60)
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != 2 || len(page.Items) != 2 {
				t.Fatalf("unexpected page: %#v", page)
			}
			for i, want := range []string{"0:00:05.250", "0:00:02.500"} {
				if page.Items[i].Duration == nil || *page.Items[i].Duration != want {
					t.Fatalf("item %d duration = %v, want %s", i, page.Items[i].Duration, want)
				}
				asset, err := service.Asset(page.Items[i].ID)
				if err != nil || asset.Duration == nil || *asset.Duration != want {
					t.Fatalf("detail: %#v, %v", asset, err)
				}
				encoded, err := json.Marshal(asset)
				if err != nil {
					t.Fatal(err)
				}
				var client struct{ Duration *string }
				if err := json.Unmarshal(encoded, &client); err != nil || client.Duration == nil || *client.Duration != want {
					t.Fatalf("public string contract broken: %s, %v", encoded, err)
				}
			}
			if page.Items[1].Type != "image" {
				t.Fatal("animated image became video")
			}
			if kind == config.DatasourceKindImmich {
				for _, mode := range []string{QueryModeFilename, QueryModeSemantic} {
					_, err := service.SearchAssets(AssetSearchRequest{
						Collection: AssetCollectionRequest{Kind: CollectionKindSearch, Query: &AssetSearchQuery{Text: "video", Mode: mode}},
						Page:       AssetSearchPageRequest{Size: 60},
					})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
