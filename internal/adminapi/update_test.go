package adminapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpdateStatusUsesReleaseIdentityForPrereleaseChannel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		currentCommit     string
		currentReleaseTag string
		manifest          updateManifest
		want              string
	}{
		{
			name:              "same commit with newer prerelease tag",
			currentCommit:     "same-commit",
			currentReleaseTag: "v0.4.0-rc.1",
			manifest: updateManifest{
				Version:    "0.4.0",
				Channel:    "prerelease",
				Commit:     "same-commit",
				ReleaseTag: "v0.4.0-rc.2",
			},
			want: "update_available",
		},
		{
			name:              "same prerelease tag",
			currentCommit:     "same-commit",
			currentReleaseTag: "v0.4.0-rc.2",
			manifest: updateManifest{
				Version:    "0.4.0",
				Channel:    "prerelease",
				Commit:     "same-commit",
				ReleaseTag: "v0.4.0-rc.2",
			},
			want: "up_to_date",
		},
		{
			name:              "older prerelease tag does not replace newer candidate",
			currentCommit:     "newer-commit",
			currentReleaseTag: "v0.4.0-rc.10",
			manifest: updateManifest{
				Version:    "0.4.0",
				Channel:    "prerelease",
				Commit:     "older-commit",
				ReleaseTag: "v0.4.0-rc.2",
			},
			want: "up_to_date",
		},
		{
			name:              "release candidate numbers compare numerically",
			currentCommit:     "older-commit",
			currentReleaseTag: "v0.4.0-rc.2",
			manifest: updateManifest{
				Version:    "0.4.0",
				Channel:    "prerelease",
				Commit:     "newer-commit",
				ReleaseTag: "v0.4.0-rc.10",
			},
			want: "update_available",
		},
		{
			name:              "stable tag does not downgrade to release candidate",
			currentCommit:     "stable-commit",
			currentReleaseTag: "v0.4.0",
			manifest: updateManifest{
				Version:    "0.4.0",
				Channel:    "prerelease",
				Commit:     "prerelease-commit",
				ReleaseTag: "v0.4.0-rc.10",
			},
			want: "up_to_date",
		},
		{
			name:          "legacy prerelease build without tag updates to tagged release",
			currentCommit: "same-commit",
			manifest: updateManifest{
				Version:    "0.4.0",
				Channel:    "prerelease",
				Commit:     "same-commit",
				ReleaseTag: "v0.4.0-rc.2",
			},
			want: "update_available",
		},
		{
			name:          "untagged manifest falls back to new prerelease commit",
			currentCommit: "old-commit",
			manifest: updateManifest{
				Version: "0.4.0",
				Channel: "prerelease",
				Commit:  "new-commit",
			},
			want: "update_available",
		},
		{
			name:          "untagged manifest with same prerelease commit",
			currentCommit: "same-commit",
			manifest: updateManifest{
				Version: "0.4.0",
				Channel: "prerelease",
				Commit:  "same-commit",
			},
			want: "up_to_date",
		},
		{
			name:          "stable rebuild does not replace the version",
			currentCommit: "old-commit",
			manifest: updateManifest{
				Version: "0.4.0",
				Channel: "stable",
				Commit:  "new-commit",
			},
			want: "up_to_date",
		},
		{
			name:          "new semantic version",
			currentCommit: "old-commit",
			manifest: updateManifest{
				Version: "0.4.1",
				Channel: "stable",
				Commit:  "new-commit",
			},
			want: "update_available",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := updateStatus("0.4.0", test.currentCommit, test.currentReleaseTag, test.manifest); got != test.want {
				t.Fatalf("updateStatus() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFetchUpdateManifestSelectsLatestPublishedPrerelease(t *testing.T) {
	t.Parallel()

	manifestBody := []byte(`{
		"schemaVersion": 1,
		"product": "timich-agent",
		"version": "0.4.1",
		"channel": "prerelease",
		"releaseTag": "v0.4.1-rc.2",
		"commit": "new-prerelease",
		"artifacts": {
			"linux-amd64": {
				"filename": "timich-agent_0.4.1_linux_amd64.tar.gz",
				"url": "https://github.com/rsahara/timich-agent/releases/download/v0.4.1-rc.2/timich-agent_0.4.1_linux_amd64.tar.gz",
				"sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
			}
		}
	}`)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifestBody))
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/releases":
			if request.URL.Query().Get(updateReleaseIndexChannelQuery) != "" {
				t.Errorf("release index request leaked local channel selector: %s", request.URL.RawQuery)
			}
			writeJSON(response, http.StatusOK, []map[string]any{
				{
					"draft":        false,
					"prerelease":   true,
					"published_at": "2026-07-22T00:00:00Z",
					"assets":       []any{},
				},
				{
					"draft":        false,
					"prerelease":   false,
					"published_at": "2026-07-24T00:00:00Z",
					"assets":       []any{},
				},
				{
					"draft":        false,
					"prerelease":   true,
					"published_at": "2026-07-23T00:00:00Z",
					"assets": []map[string]any{{
						"name":                 "agent-update-manifest.json",
						"browser_download_url": server.URL + "/manifest",
						"size":                 len(manifestBody),
						"digest":               digest,
					}},
				},
				{
					"draft":        true,
					"prerelease":   true,
					"published_at": "2026-07-25T00:00:00Z",
					"assets":       []any{},
				},
			})
		case "/manifest":
			response.Header().Set("Content-Type", "application/json")
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write(manifestBody)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	manifest, err := fetchUpdateManifest(
		context.Background(),
		server.Client(),
		server.URL+"/releases?per_page=100&"+updateReleaseIndexChannelQuery+"=prerelease",
	)
	if err != nil {
		t.Fatalf("fetchUpdateManifest() error = %v", err)
	}
	if manifest.Version != "0.4.1" || manifest.Channel != "prerelease" || manifest.Commit != "new-prerelease" {
		t.Fatalf("fetchUpdateManifest() = %+v, want latest published prerelease manifest", manifest)
	}
}

func TestFetchUpdateManifestRejectsPrereleaseDigestMismatch(t *testing.T) {
	t.Parallel()

	manifestBody := []byte(`{"product":"timich-agent","version":"0.4.1","channel":"prerelease"}`)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/releases":
			writeJSON(response, http.StatusOK, []map[string]any{{
				"draft":        false,
				"prerelease":   true,
				"published_at": "2026-07-23T00:00:00Z",
				"assets": []map[string]any{{
					"name":                 "agent-update-manifest.json",
					"browser_download_url": server.URL + "/manifest",
					"size":                 len(manifestBody),
					"digest":               "sha256:" + strings.Repeat("0", sha256.Size*2),
				}},
			}})
		case "/manifest":
			response.WriteHeader(http.StatusOK)
			_, _ = response.Write(manifestBody)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	_, err := fetchUpdateManifest(
		context.Background(),
		server.Client(),
		server.URL+"/releases?"+updateReleaseIndexChannelQuery+"=prerelease",
	)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("fetchUpdateManifest() error = %v, want digest mismatch", err)
	}
}
