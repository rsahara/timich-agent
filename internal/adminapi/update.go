package adminapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const updateManifestMaxBytes = 1 << 20
const updateReleaseIndexMaxBytes = 4 << 20
const updateReleaseIndexChannelQuery = "timich_channel"

type updateManifest struct {
	SchemaVersion           int                       `json:"schemaVersion"`
	Product                 string                    `json:"product"`
	Version                 string                    `json:"version"`
	Channel                 string                    `json:"channel,omitempty"`
	ReleaseTag              string                    `json:"releaseTag,omitempty"`
	ReleasedAt              string                    `json:"releasedAt,omitempty"`
	Commit                  string                    `json:"commit,omitempty"`
	MinimumSupportedVersion string                    `json:"minimumSupportedVersion,omitempty"`
	NotesURL                string                    `json:"notesUrl,omitempty"`
	Artifacts               map[string]updateArtifact `json:"artifacts,omitempty"`
	UpdateGuide             updateGuide               `json:"updateGuide,omitempty"`
}

type updateArtifact struct {
	Filename string `json:"filename"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
}

type updateGuide struct {
	DockerCompose []string `json:"dockerCompose,omitempty"`
	ManualBinary  []string `json:"manualBinary,omitempty"`
}

type updateReleaseIndexEntry struct {
	Draft       bool                      `json:"draft"`
	Prerelease  bool                      `json:"prerelease"`
	PublishedAt string                    `json:"published_at"`
	Assets      []updateReleaseIndexAsset `json:"assets"`
}

type updateReleaseIndexAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
}

type updateCheckResponse struct {
	CurrentVersion    string          `json:"currentVersion"`
	CurrentReleaseTag string          `json:"currentReleaseTag,omitempty"`
	LatestVersion     string          `json:"latestVersion,omitempty"`
	LatestReleaseTag  string          `json:"latestReleaseTag,omitempty"`
	Status            string          `json:"status"`
	ManifestURL       string          `json:"manifestUrl,omitempty"`
	Platform          string          `json:"platform"`
	Message           string          `json:"message,omitempty"`
	NotesURL          string          `json:"notesUrl,omitempty"`
	Artifact          *updateArtifact `json:"artifact,omitempty"`
	Guide             updateGuide     `json:"guide,omitempty"`
	Manifest          *updateManifest `json:"manifest,omitempty"`
}

func fetchUpdateManifest(ctx context.Context, client *http.Client, url string) (updateManifest, error) {
	parsedURL, err := neturl.Parse(url)
	if err != nil {
		return updateManifest{}, fmt.Errorf("parse manifest URL: %w", err)
	}
	query := parsedURL.Query()
	channel := strings.TrimSpace(query.Get(updateReleaseIndexChannelQuery))
	if channel == "" {
		return fetchDirectUpdateManifest(ctx, client, url)
	}
	if channel != "prerelease" {
		return updateManifest{}, fmt.Errorf("unsupported update release channel %q", channel)
	}
	query.Del(updateReleaseIndexChannelQuery)
	parsedURL.RawQuery = query.Encode()
	return fetchUpdateManifestFromReleaseIndex(ctx, client, parsedURL.String(), channel)
}

func fetchDirectUpdateManifest(ctx context.Context, client *http.Client, manifestURL string) (updateManifest, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return updateManifest{}, fmt.Errorf("create manifest request: %w", err)
	}
	request.Header.Set("Accept", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return updateManifest{}, fmt.Errorf("fetch manifest: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return updateManifest{}, fmt.Errorf("fetch manifest: HTTP %d", response.StatusCode)
	}

	return decodeAndValidateUpdateManifest(io.LimitReader(response.Body, updateManifestMaxBytes))
}

func fetchUpdateManifestFromReleaseIndex(ctx context.Context, client *http.Client, indexURL string, channel string) (updateManifest, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, indexURL, nil)
	if err != nil {
		return updateManifest{}, fmt.Errorf("create release index request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := client.Do(request)
	if err != nil {
		return updateManifest{}, fmt.Errorf("fetch release index: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return updateManifest{}, fmt.Errorf("fetch release index: HTTP %d", response.StatusCode)
	}

	var releases []updateReleaseIndexEntry
	decoder := json.NewDecoder(io.LimitReader(response.Body, updateReleaseIndexMaxBytes))
	if err := decoder.Decode(&releases); err != nil {
		return updateManifest{}, fmt.Errorf("decode release index: %w", err)
	}
	var latest *updateReleaseIndexEntry
	for index := range releases {
		release := &releases[index]
		if release.Draft || !release.Prerelease || strings.TrimSpace(release.PublishedAt) == "" {
			continue
		}
		if latest == nil || release.PublishedAt > latest.PublishedAt {
			latest = release
		}
	}
	if latest == nil {
		return updateManifest{}, errors.New("release index has no published prerelease")
	}

	var manifestAsset *updateReleaseIndexAsset
	for index := range latest.Assets {
		asset := &latest.Assets[index]
		if asset.Name != "agent-update-manifest.json" {
			continue
		}
		if manifestAsset != nil {
			return updateManifest{}, errors.New("latest prerelease has duplicate update manifests")
		}
		manifestAsset = asset
	}
	if manifestAsset == nil {
		return updateManifest{}, errors.New("latest prerelease has no update manifest")
	}
	if manifestAsset.Size <= 0 || manifestAsset.Size > updateManifestMaxBytes {
		return updateManifest{}, errors.New("latest prerelease update manifest has invalid size")
	}
	assetURL, ok := safeManifestURL(manifestAsset.BrowserDownloadURL)
	if !ok {
		return updateManifest{}, errors.New("latest prerelease update manifest URL is unsafe")
	}
	expectedDigest := strings.TrimPrefix(strings.TrimSpace(manifestAsset.Digest), "sha256:")
	if len(expectedDigest) != sha256.Size*2 {
		return updateManifest{}, errors.New("latest prerelease update manifest has no SHA-256 digest")
	}

	assetRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return updateManifest{}, fmt.Errorf("create release manifest request: %w", err)
	}
	assetRequest.Header.Set("Accept", "application/json")
	assetResponse, err := client.Do(assetRequest)
	if err != nil {
		return updateManifest{}, fmt.Errorf("fetch release manifest: %w", err)
	}
	defer assetResponse.Body.Close()
	if assetResponse.StatusCode != http.StatusOK {
		return updateManifest{}, fmt.Errorf("fetch release manifest: HTTP %d", assetResponse.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(assetResponse.Body, updateManifestMaxBytes+1))
	if err != nil {
		return updateManifest{}, fmt.Errorf("read release manifest: %w", err)
	}
	if int64(len(body)) != manifestAsset.Size {
		return updateManifest{}, errors.New("latest prerelease update manifest size mismatch")
	}
	actualDigest := fmt.Sprintf("%x", sha256.Sum256(body))
	if !strings.EqualFold(actualDigest, expectedDigest) {
		return updateManifest{}, errors.New("latest prerelease update manifest digest mismatch")
	}
	manifest, err := decodeAndValidateUpdateManifest(bytes.NewReader(body))
	if err != nil {
		return updateManifest{}, err
	}
	if strings.TrimSpace(manifest.Channel) != channel {
		return updateManifest{}, fmt.Errorf("latest prerelease manifest channel is %q", manifest.Channel)
	}
	return manifest, nil
}

func decodeAndValidateUpdateManifest(reader io.Reader) (updateManifest, error) {
	var manifest updateManifest
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&manifest); err != nil {
		return updateManifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if strings.TrimSpace(manifest.Product) != "timich-agent" {
		return updateManifest{}, errors.New("manifest is not for timich-agent")
	}
	if strings.TrimSpace(manifest.Version) == "" {
		return updateManifest{}, errors.New("manifest is missing version")
	}
	if err := validateUpdateManifestURLs(&manifest); err != nil {
		return updateManifest{}, err
	}
	return manifest, nil
}

func validateUpdateManifestURLs(manifest *updateManifest) error {
	if manifest.NotesURL != "" {
		trimmed, ok := safeManifestURL(manifest.NotesURL)
		if !ok {
			return errors.New("manifest notesUrl is not an http or https URL")
		}
		manifest.NotesURL = trimmed
	}
	for platform, artifact := range manifest.Artifacts {
		if artifact.URL == "" {
			continue
		}
		trimmed, ok := safeManifestURL(artifact.URL)
		if !ok {
			return fmt.Errorf("manifest artifact URL for %s is not an http or https URL", platform)
		}
		artifact.URL = trimmed
		manifest.Artifacts[platform] = artifact
	}
	return nil
}

func safeManifestURL(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	parsed, err := neturl.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", false
	}
	return trimmed, true
}

func buildUpdateCheckResponse(currentVersion string, currentCommit string, currentReleaseTag string, manifestURL string, manifest updateManifest) updateCheckResponse {
	platform := runtimePlatform()
	artifact, hasArtifact := manifest.Artifacts[platform]
	status := updateStatus(currentVersion, currentCommit, currentReleaseTag, manifest)
	message := updateMessage(status, hasArtifact, platform)
	if !hasArtifact && status == "update_available" {
		status = "unsupported_platform"
	}

	response := updateCheckResponse{
		CurrentVersion:    currentVersion,
		CurrentReleaseTag: strings.TrimSpace(currentReleaseTag),
		LatestVersion:     manifest.Version,
		LatestReleaseTag:  strings.TrimSpace(manifest.ReleaseTag),
		Status:            status,
		ManifestURL:       manifestURL,
		Platform:          platform,
		Message:           message,
		NotesURL:          manifest.NotesURL,
		Guide:             manifest.UpdateGuide,
		Manifest:          &manifest,
	}
	if hasArtifact {
		response.Artifact = &artifact
	}
	return response
}

func runtimePlatform() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

func updateStatus(currentVersion string, currentCommit string, currentReleaseTag string, manifest updateManifest) string {
	comparison, ok := compareVersions(currentVersion, manifest.Version)
	if !ok {
		if strings.TrimSpace(currentVersion) == strings.TrimSpace(manifest.Version) {
			return "up_to_date"
		}
		return "unknown"
	}
	if comparison < 0 {
		return "update_available"
	}
	if comparison == 0 && strings.TrimSpace(manifest.Channel) == "prerelease" {
		currentTag := strings.TrimSpace(currentReleaseTag)
		latestTag := strings.TrimSpace(manifest.ReleaseTag)
		if latestTag != "" && currentTag != latestTag {
			if currentTag == "" {
				return "update_available"
			}
			tagComparison, comparable := compareReleaseTags(currentTag, latestTag)
			if !comparable {
				return "unknown"
			}
			if tagComparison < 0 {
				return "update_available"
			}
		}
		if latestTag == "" &&
			strings.TrimSpace(currentCommit) != "" &&
			strings.TrimSpace(manifest.Commit) != "" &&
			strings.TrimSpace(currentCommit) != strings.TrimSpace(manifest.Commit) {
			return "update_available"
		}
	}
	return "up_to_date"
}

func updateMessage(status string, hasArtifact bool, platform string) string {
	switch {
	case status == "update_available" && hasArtifact:
		return "A newer Timich Agent release is available for this platform."
	case status == "update_available":
		return "A newer Timich Agent release is available, but no artifact was listed for this platform."
	case status == "up_to_date":
		return "This Timich Agent is up to date."
	default:
		return "Could not compare these version strings."
	}
}

func compareVersions(currentVersion string, latestVersion string) (int, bool) {
	current, ok := parseVersion(currentVersion)
	if !ok {
		return 0, false
	}
	latest, ok := parseVersion(latestVersion)
	if !ok {
		return 0, false
	}
	return compareVersionParts(current, latest), true
}

func compareVersionParts(current [3]int, latest [3]int) int {
	for index := 0; index < len(current); index++ {
		if current[index] < latest[index] {
			return -1
		}
		if current[index] > latest[index] {
			return 1
		}
	}
	return 0
}

type releaseTagVersion struct {
	version          [3]int
	releaseCandidate int
	stable           bool
}

func compareReleaseTags(currentTag string, latestTag string) (int, bool) {
	current, ok := parseReleaseTag(currentTag)
	if !ok {
		return 0, false
	}
	latest, ok := parseReleaseTag(latestTag)
	if !ok {
		return 0, false
	}
	if comparison := compareVersionParts(current.version, latest.version); comparison != 0 {
		return comparison, true
	}
	if current.stable != latest.stable {
		if current.stable {
			return 1, true
		}
		return -1, true
	}
	if current.releaseCandidate < latest.releaseCandidate {
		return -1, true
	}
	if current.releaseCandidate > latest.releaseCandidate {
		return 1, true
	}
	return 0, true
}

func parseReleaseTag(value string) (releaseTagVersion, bool) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "v") {
		return releaseTagVersion{}, false
	}
	versionText, releaseCandidateText, hasReleaseCandidate := strings.Cut(strings.TrimPrefix(trimmed, "v"), "-rc.")
	version, ok := parseVersion(versionText)
	if !ok {
		return releaseTagVersion{}, false
	}
	parsed := releaseTagVersion{version: version, stable: !hasReleaseCandidate}
	if !hasReleaseCandidate {
		return parsed, true
	}
	if releaseCandidateText == "" || (len(releaseCandidateText) > 1 && strings.HasPrefix(releaseCandidateText, "0")) {
		return releaseTagVersion{}, false
	}
	releaseCandidate, err := strconv.Atoi(releaseCandidateText)
	if err != nil || releaseCandidate <= 0 {
		return releaseTagVersion{}, false
	}
	parsed.releaseCandidate = releaseCandidate
	return parsed, true
}

func parseVersion(value string) ([3]int, bool) {
	var version [3]int
	trimmed := strings.TrimPrefix(strings.TrimSpace(value), "v")
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return version, false
	}
	for index, part := range parts {
		if part == "" {
			return version, false
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return version, false
		}
		version[index] = number
	}
	return version, true
}

func updateHTTPClient() *http.Client {
	return &http.Client{Timeout: 6 * time.Second}
}
