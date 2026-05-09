package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const updateManifestMaxBytes = 1 << 20

type updateManifest struct {
	SchemaVersion           int                       `json:"schemaVersion"`
	Product                 string                    `json:"product"`
	Version                 string                    `json:"version"`
	Channel                 string                    `json:"channel,omitempty"`
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

type updateCheckResponse struct {
	CurrentVersion string          `json:"currentVersion"`
	LatestVersion  string          `json:"latestVersion,omitempty"`
	Status         string          `json:"status"`
	ManifestURL    string          `json:"manifestUrl,omitempty"`
	Platform       string          `json:"platform"`
	Message        string          `json:"message,omitempty"`
	NotesURL       string          `json:"notesUrl,omitempty"`
	Artifact       *updateArtifact `json:"artifact,omitempty"`
	Guide          updateGuide     `json:"guide,omitempty"`
	Manifest       *updateManifest `json:"manifest,omitempty"`
}

func fetchUpdateManifest(ctx context.Context, client *http.Client, url string) (updateManifest, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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

	var manifest updateManifest
	decoder := json.NewDecoder(io.LimitReader(response.Body, updateManifestMaxBytes))
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
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", false
	}
	return trimmed, true
}

func buildUpdateCheckResponse(currentVersion string, manifestURL string, manifest updateManifest) updateCheckResponse {
	platform := runtimePlatform()
	artifact, hasArtifact := manifest.Artifacts[platform]
	status := updateStatus(currentVersion, manifest.Version)
	message := updateMessage(status, hasArtifact, platform)
	if !hasArtifact && status == "update_available" {
		status = "unsupported_platform"
	}

	response := updateCheckResponse{
		CurrentVersion: currentVersion,
		LatestVersion:  manifest.Version,
		Status:         status,
		ManifestURL:    manifestURL,
		Platform:       platform,
		Message:        message,
		NotesURL:       manifest.NotesURL,
		Guide:          manifest.UpdateGuide,
		Manifest:       &manifest,
	}
	if hasArtifact {
		response.Artifact = &artifact
	}
	return response
}

func runtimePlatform() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

func updateStatus(currentVersion string, latestVersion string) string {
	comparison, ok := compareVersions(currentVersion, latestVersion)
	if !ok {
		if strings.TrimSpace(currentVersion) == strings.TrimSpace(latestVersion) {
			return "up_to_date"
		}
		return "unknown"
	}
	if comparison < 0 {
		return "update_available"
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
	for index := 0; index < len(current); index++ {
		if current[index] < latest[index] {
			return -1, true
		}
		if current[index] > latest[index] {
			return 1, true
		}
	}
	return 0, true
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
