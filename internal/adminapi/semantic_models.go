package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
	runtimestate "github.com/rsahara/timich-agent/internal/runtime"
	"github.com/rsahara/timich-agent/internal/semanticmanifest"
)

const (
	semanticModelManifestMaxBytes        = 1 << 20
	defaultRecommendedSemanticModelID    = "timich-siglip2-base-patch16-224-multilingual-v1"
	defaultRecommendedSemanticModelName  = "Timich SigLIP 2 Base Patch16 224 Multilingual"
	semanticSearchEnableDefaultInterval  = "30s"
	semanticSearchEnableDefaultBatch     = 100
	semanticSearchEnableDefaultInitial   = 100
	semanticModelStatusRefreshTimeout    = 12 * time.Second
	semanticModelStatusRefreshMinDelay   = 5 * time.Second
	semanticModelStatusRefreshStaleAfter = semanticModelStatusRefreshTimeout + 5*time.Second
)

type semanticModelManifest = semanticmanifest.Manifest
type semanticManifestModel = semanticmanifest.Model
type semanticModelArtifact = semanticmanifest.Artifact
type semanticManifestRuntimePack = semanticmanifest.RuntimePack

type semanticSearchEnableRequest struct {
	Interval               string `json:"interval,omitempty"`
	BatchSize              int    `json:"batchSize,omitempty"`
	TargetCompletedVectors int    `json:"targetCompletedVectors,omitempty"`
	InitialIndexingAssets  *int   `json:"initialIndexingAssets,omitempty"`
	InitialBackfillAssets  *int   `json:"initialBackfillAssets,omitempty"`
}

type semanticModelActionRequest struct {
	ModelID       string `json:"modelId"`
	VectorSpaceID string `json:"vectorSpaceId,omitempty"`
}

func (s *server) semanticModelLoadingStatus() catalog.SemanticModelRegistryStatus {
	status := catalog.SemanticModelRegistry()
	if s != nil && s.runtime != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		status = s.runtime.SemanticModelRegistryInstalledStatusWithContext(ctx)
		status = s.runtime.EnrichSemanticModelRegistryCachedIndexingStatusWithContext(ctx, status)
	}
	manifestURL := strings.TrimSpace(s.options.SemanticModelManifestURL)
	status.ManifestURL = manifestURL
	if manifestURL == "" {
		status.RegistryStatus = "disabled"
		status.RegistryMessage = "Semantic model registry is not configured for this build."
		return status
	}
	status.RegistryStatus = "loading"
	status.RegistryMessage = "Reading installed models and runtime pack state."
	return status
}

func (s *server) scheduleSemanticModelStatusRefresh() {
	if s == nil || s.runtime == nil {
		return
	}
	s.semanticModelsRefreshMu.Lock()
	if s.semanticModelsRefreshBusy {
		if s.semanticModelsRefreshStarted.IsZero() || time.Since(s.semanticModelsRefreshStarted) < semanticModelStatusRefreshStaleAfter {
			s.semanticModelsRefreshMu.Unlock()
			return
		}
		s.semanticModelsRefreshBusy = false
		s.semanticModelsRefreshStarted = time.Time{}
	}
	if !s.semanticModelsRefreshAt.IsZero() && time.Since(s.semanticModelsRefreshAt) < semanticModelStatusRefreshMinDelay {
		s.semanticModelsRefreshMu.Unlock()
		return
	}
	s.semanticModelsRefreshBusy = true
	s.semanticModelsRefreshStarted = time.Now().UTC()
	s.semanticModelsRefreshMu.Unlock()

	go func() {
		defer func() {
			s.semanticModelsRefreshMu.Lock()
			s.semanticModelsRefreshBusy = false
			s.semanticModelsRefreshStarted = time.Time{}
			s.semanticModelsRefreshMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), semanticModelStatusRefreshTimeout)
		defer cancel()
		_, _ = s.refreshSemanticModelStatusSnapshot(ctx)
	}()
}

func (s *server) refreshSemanticModelStatusSnapshot(ctx context.Context) (catalog.SemanticModelRegistryStatus, error) {
	status, err := s.buildSemanticModelsStatus(ctx)
	if err != nil {
		return status, err
	}
	s.runtime.RememberSemanticModelRegistryStatus(status)
	s.semanticModelsRefreshMu.Lock()
	s.semanticModelsRefreshAt = time.Now().UTC()
	s.semanticModelsRefreshMu.Unlock()
	return status, nil
}

func (s *server) buildSemanticModelsStatus(ctx context.Context) (catalog.SemanticModelRegistryStatus, error) {
	status := s.runtime.SemanticModelRegistryInstalledStatusWithContext(ctx)
	manifestURL := strings.TrimSpace(s.options.SemanticModelManifestURL)
	if manifestURL == "" {
		status.RegistryStatus = "disabled"
		status.RegistryMessage = "Semantic model registry is not configured for this build."
		return s.runtime.EnrichSemanticModelRegistryCachedIndexingStatusWithContext(ctx, status), nil
	}
	status.ManifestURL = manifestURL
	client := s.options.SemanticModelHTTPClient
	if client == nil {
		client = updateHTTPClient()
	}
	manifest, err := fetchSemanticModelManifest(ctx, client, manifestURL)
	if err != nil {
		status.RegistryStatus = "unavailable"
		status.RegistryMessage = err.Error()
		enriched := s.runtime.EnrichSemanticModelRegistryCachedIndexingStatusWithContext(ctx, status)
		return enriched, nil
	}
	enriched := s.runtime.EnrichSemanticModelRegistryCachedIndexingStatusWithContext(ctx, buildSemanticModelRegistryResponse(status, manifestURL, runtimePlatform(), manifest))
	return enriched, nil
}

type semanticModelInstallResponse struct {
	catalog.SemanticModelPackInstallResult
	WarningCode    string `json:"warningCode,omitempty"`
	WarningMessage string `json:"warningMessage,omitempty"`
}

type semanticSearchEnableResponse struct {
	Status               string                                    `json:"status"`
	InstalledRecommended bool                                      `json:"installedRecommended"`
	InstalledRuntimePack bool                                      `json:"installedRuntimePack"`
	RuntimePackInstall   *catalog.SemanticRuntimePackInstallResult `json:"runtimePackInstall,omitempty"`
	Install              *catalog.SemanticModelPackInstallResult   `json:"install,omitempty"`
	Indexing             *catalog.SemanticBackfillResult           `json:"indexing,omitempty"`
	IndexingWorker       *catalog.SemanticIndexingWorkerStatus     `json:"indexingWorker,omitempty"`
	Semantic             catalog.SemanticModelRegistryStatus       `json:"semantic"`
}

func fetchSemanticModelManifest(ctx context.Context, client *http.Client, url string) (semanticModelManifest, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return semanticModelManifest{}, fmt.Errorf("create semantic model manifest request: %w", err)
	}
	request.Header.Set("Accept", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return semanticModelManifest{}, fmt.Errorf("fetch semantic model manifest: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return semanticModelManifest{}, fmt.Errorf("semantic model manifest URL returned HTTP %d; check the registry URL or release tag", response.StatusCode)
	}

	var manifest semanticModelManifest
	decoder := json.NewDecoder(io.LimitReader(response.Body, semanticModelManifestMaxBytes))
	if err := decoder.Decode(&manifest); err != nil {
		return semanticModelManifest{}, fmt.Errorf("decode semantic model manifest: %w", err)
	}
	if err := validateSemanticModelManifest(&manifest); err != nil {
		return semanticModelManifest{}, err
	}
	return manifest, nil
}

func fetchSemanticModelArtifact(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create semantic model artifact request: %w", err)
	}
	request.Header.Set("Accept", "application/octet-stream")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch semantic model artifact: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("fetch semantic model artifact: HTTP %d", response.StatusCode)
	}
	return response, nil
}

func (s *server) semanticModelSearchEnable(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to enable semantic search.") {
		return
	}
	var request semanticSearchEnableRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the semantic search enable request.")
			return
		}
	}
	if request.BatchSize < 0 ||
		request.TargetCompletedVectors < 0 ||
		(request.InitialIndexingAssets != nil && *request.InitialIndexingAssets < 0) ||
		(request.InitialBackfillAssets != nil && *request.InitialBackfillAssets < 0) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Semantic search enable numeric settings must not be negative.")
		return
	}
	interval := strings.TrimSpace(request.Interval)
	if interval == "" {
		interval = semanticSearchEnableDefaultInterval
	}
	parsedInterval, err := time.ParseDuration(interval)
	if err != nil || parsedInterval <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "Semantic search enable interval must be a positive duration.")
		return
	}
	batchSize := request.BatchSize
	if batchSize == 0 {
		batchSize = semanticSearchEnableDefaultBatch
	}
	initialIndexingAssets := semanticSearchEnableDefaultInitial
	if request.InitialIndexingAssets != nil {
		initialIndexingAssets = *request.InitialIndexingAssets
	} else if request.InitialBackfillAssets != nil {
		initialIndexingAssets = *request.InitialBackfillAssets
	}

	ctx := r.Context()
	status := s.runtime.SemanticModelRegistryStatusWithContext(ctx)
	manifestURL := strings.TrimSpace(s.options.SemanticModelManifestURL)
	client := s.options.SemanticModelHTTPClient
	if client == nil {
		client = semanticModelHTTPClient()
	}
	var manifest *semanticModelManifest
	if manifestURL != "" {
		fetched, err := fetchSemanticModelManifest(ctx, client, manifestURL)
		if err != nil {
			if status.Candidate == nil || strings.TrimSpace(status.Candidate.ModelID) == "" {
				writeError(w, http.StatusBadGateway, "semantic_model_manifest_unavailable", "Could not read the semantic model registry.")
				return
			}
		} else {
			manifest = &fetched
			status = s.runtime.EnrichSemanticModelRegistryStatusWithContext(ctx, buildSemanticModelRegistryResponse(status, manifestURL, runtimePlatform(), fetched))
		}
	}

	var runtimePackInstallResult *catalog.SemanticRuntimePackInstallResult
	if manifest != nil {
		if recommended, ok := semanticManifestRecommendedRuntimePack(*manifest); ok && !semanticRuntimePackInstalled(status.RuntimePacks, recommended.ID, recommended.Version) {
			runtimePack := semanticManifestRuntimePackStatus(recommended, manifestURL, runtimePlatform())
			if runtimePack.Artifact == nil {
				writeError(w, http.StatusBadRequest, "semantic_runtime_pack_platform_unsupported", "The recommended semantic runtime pack does not support this platform.")
				return
			}
			response, err := fetchSemanticModelArtifact(ctx, client, runtimePack.Artifact.URL)
			if err != nil {
				writeError(w, http.StatusBadGateway, "semantic_runtime_pack_download_failed", "Could not download the recommended semantic runtime pack.")
				return
			}
			result, err := s.runtime.InstallSemanticRuntimePack(ctx, runtimePack, response.Body)
			response.Body.Close()
			if err != nil {
				writeSemanticRuntimePackInstallError(w, err)
				return
			}
			runtimePackInstallResult = &result
			status = s.runtime.SemanticModelRegistryStatusWithContext(ctx)
			if manifest != nil {
				status = s.runtime.EnrichSemanticModelRegistryStatusWithContext(ctx, buildSemanticModelRegistryResponse(status, manifestURL, runtimePlatform(), *manifest))
			}
		}
	}

	var installResult *catalog.SemanticModelPackInstallResult
	if status.Candidate == nil || strings.TrimSpace(status.Candidate.ModelID) == "" {
		if manifest == nil {
			writeError(w, http.StatusBadRequest, "semantic_model_registry_disabled", "Semantic model registry is not configured for this build.")
			return
		}
		recommended, ok := semanticManifestRecommendedModel(*manifest)
		if !ok {
			writeError(w, http.StatusBadGateway, "semantic_model_recommended_missing", "The semantic model registry did not include a recommended model.")
			return
		}
		profile := semanticManifestModelProfile(recommended, manifestURL, runtimePlatform())
		if profile.ModelPack == nil || profile.ModelPack.Artifact == nil {
			writeError(w, http.StatusBadRequest, "semantic_model_platform_unsupported", "The recommended semantic model does not support this platform.")
			return
		}
		response, err := fetchSemanticModelArtifact(ctx, client, profile.ModelPack.Artifact.URL)
		if err != nil {
			writeError(w, http.StatusBadGateway, "semantic_model_download_failed", "Could not download the recommended semantic model.")
			return
		}
		defer response.Body.Close()
		result, err := s.runtime.InstallSemanticModelPack(ctx, *profile.ModelPack, response.Body)
		if err != nil {
			writeSemanticModelInstallError(w, err)
			return
		}
		installResult = &result
	}

	var indexingResult *catalog.SemanticBackfillResult
	if initialIndexingAssets > 0 {
		result, err := s.runtime.RunSemanticIndexing(ctx, initialIndexingAssets)
		if err != nil {
			writeSemanticIndexingError(w, err)
			return
		}
		indexingResult = &result
	}

	_, err = s.runtime.UpdateSemanticIndexing(config.SemanticIndexingConfig{
		Enabled:                true,
		Interval:               interval,
		BatchSize:              batchSize,
		TargetCompletedVectors: request.TargetCompletedVectors,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "semantic_search_enable_failed", "Could not enable semantic search indexing.")
		return
	}
	status = s.runtime.SemanticModelRegistryStatusWithContext(ctx)
	if manifest != nil {
		status = s.runtime.EnrichSemanticModelRegistryStatusWithContext(ctx, buildSemanticModelRegistryResponse(status, manifestURL, runtimePlatform(), *manifest))
	}
	writeJSON(w, http.StatusOK, semanticSearchEnableResponse{
		Status:               "enabled",
		InstalledRecommended: installResult != nil,
		InstalledRuntimePack: runtimePackInstallResult != nil,
		RuntimePackInstall:   runtimePackInstallResult,
		Install:              installResult,
		Indexing:             indexingResult,
		IndexingWorker:       status.IndexingWorker,
		Semantic:             status,
	})
}

func semanticRuntimePackInstalled(packs []catalog.SemanticRuntimePackStatus, id string, version string) bool {
	for _, pack := range packs {
		if pack.ID != strings.TrimSpace(id) || !pack.Installed {
			continue
		}
		if strings.TrimSpace(version) == "" || pack.Version == strings.TrimSpace(version) {
			return true
		}
	}
	return false
}

func semanticModelHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Minute}
}

func writeSemanticModelInstallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrSemanticModelPackChecksumMismatch):
		writeError(w, http.StatusBadGateway, "semantic_model_checksum_mismatch", "Downloaded semantic model did not match its SHA-256 checksum.")
	case errors.Is(err, catalog.ErrSemanticModelPackSizeMismatch):
		writeError(w, http.StatusBadGateway, "semantic_model_size_mismatch", "Downloaded semantic model did not match its expected size.")
	case errors.Is(err, catalog.ErrSemanticModelPackInvalid):
		writeError(w, http.StatusBadRequest, "semantic_model_invalid", "Semantic model pack metadata is invalid.")
	default:
		writeError(w, http.StatusInternalServerError, "semantic_model_install_failed", "Could not install the semantic model pack.")
	}
}

func writeSemanticRuntimePackInstallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrSemanticRuntimePackChecksumMismatch):
		writeError(w, http.StatusBadGateway, "semantic_runtime_pack_checksum_mismatch", "Downloaded semantic runtime pack did not match its SHA-256 checksum.")
	case errors.Is(err, catalog.ErrSemanticRuntimePackSizeMismatch):
		writeError(w, http.StatusBadGateway, "semantic_runtime_pack_size_mismatch", "Downloaded semantic runtime pack did not match its expected size.")
	case errors.Is(err, catalog.ErrSemanticRuntimePackInvalid):
		writeError(w, http.StatusBadRequest, "semantic_runtime_pack_invalid", "Semantic runtime pack metadata is invalid.")
	default:
		writeError(w, http.StatusInternalServerError, "semantic_runtime_pack_install_failed", "Could not install the semantic runtime pack.")
	}
}

func writeSemanticIndexingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtimestate.ErrSemanticCandidateUnavailable):
		writeError(w, http.StatusBadRequest, "semantic_candidate_unavailable", "Install a semantic model before starting indexing.")
	case errors.Is(err, runtimestate.ErrSemanticCandidateRuntimeUnavailable):
		writeError(w, http.StatusConflict, "semantic_candidate_runtime_unavailable", "The semantic model candidate runtime is not ready to embed.")
	case errors.Is(err, catalog.ErrCatalogNotConfigured):
		writeError(w, http.StatusBadRequest, "datasource_not_configured", "No indexed datasource is configured.")
	case errors.Is(err, catalog.ErrSemanticModelPackInvalid):
		writeError(w, http.StatusBadRequest, "semantic_model_invalid", "Semantic model pack metadata is invalid.")
	default:
		writeError(w, http.StatusBadGateway, "semantic_indexing_failed", "Could not run semantic indexing.")
	}
}

func writeSemanticModelActivationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtimestate.ErrSemanticCandidateUnavailable):
		writeError(w, http.StatusBadRequest, "semantic_candidate_unavailable", "Install a semantic model candidate before activating search.")
	case errors.Is(err, runtimestate.ErrSemanticCandidateRuntimeUnavailable):
		writeError(w, http.StatusConflict, "semantic_candidate_runtime_unavailable", "The semantic model candidate runtime is not ready to embed.")
	case errors.Is(err, runtimestate.ErrSemanticIndexingIncomplete):
		writeError(w, http.StatusConflict, "semantic_indexing_incomplete", "Finish semantic indexing for this model before activating it.")
	case errors.Is(err, catalog.ErrSemanticModelPackInvalid):
		writeError(w, http.StatusBadRequest, "semantic_model_invalid", "Semantic model pack metadata is invalid.")
	default:
		writeError(w, http.StatusInternalServerError, "semantic_candidate_activation_failed", "Could not activate the semantic model candidate.")
	}
}

func writeSemanticModelUninstallError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrSemanticModelPackActive):
		writeError(w, http.StatusConflict, "semantic_model_active", "The active semantic model cannot be uninstalled.")
	case errors.Is(err, runtimestate.ErrSemanticModelPackMigrating):
		writeError(w, http.StatusConflict, "semantic_model_indexing", "Finish or replace semantic indexing before uninstalling this model.")
	case errors.Is(err, catalog.ErrSemanticModelPackInvalid):
		writeError(w, http.StatusBadRequest, "semantic_model_invalid", "Semantic model pack metadata is invalid.")
	default:
		writeError(w, http.StatusInternalServerError, "semantic_model_uninstall_failed", "Could not uninstall the semantic model pack.")
	}
}

func validateSemanticModelManifest(manifest *semanticModelManifest) error {
	return semanticmanifest.Validate(manifest, semanticmanifest.ValidationOptions{})
}

func buildSemanticModelRegistryResponse(
	status catalog.SemanticModelRegistryStatus,
	manifestURL string,
	platform string,
	manifest semanticModelManifest,
) catalog.SemanticModelRegistryStatus {
	status.RegistryStatus = "available"
	status.RegistryMessage = "Recommended semantic model registry is available."
	status.ManifestURL = strings.TrimSpace(manifestURL)
	recommendedID := semanticManifestRecommendedModelID(manifest)
	for _, model := range manifest.Models {
		role := ""
		if model.ID == recommendedID {
			role = catalog.SemanticModelRoleRecommended
		}
		profile := semanticManifestModelProfileWithRole(model, manifestURL, platform, role)
		if role == catalog.SemanticModelRoleRecommended {
			recommended := profile
			status.Recommended = &recommended
		}
		if !semanticProfileAlreadyListed(status.Profiles, profile) {
			status.Profiles = append(status.Profiles, profile)
		}
	}
	if recommended, ok := semanticManifestRecommendedRuntimePack(manifest); ok {
		runtimePack := semanticManifestRuntimePackStatus(recommended, manifestURL, platform)
		status.RecommendedRuntimePack = &runtimePack
		if !semanticRuntimePackAlreadyListed(status.RuntimePacks, runtimePack) {
			status.RuntimePacks = append(status.RuntimePacks, runtimePack)
		}
	}
	return status
}

func semanticManifestRecommendedModelID(manifest semanticModelManifest) string {
	recommendedID := strings.TrimSpace(manifest.Recommended)
	if recommendedID == "" {
		recommendedID = defaultRecommendedSemanticModelID
	}
	for _, model := range manifest.Models {
		if model.ID == recommendedID {
			return model.ID
		}
	}
	if strings.TrimSpace(manifest.Recommended) == "" && len(manifest.Models) > 0 {
		return manifest.Models[0].ID
	}
	return recommendedID
}

func semanticManifestRecommendedModel(manifest semanticModelManifest) (semanticManifestModel, bool) {
	recommendedID := semanticManifestRecommendedModelID(manifest)
	for _, model := range manifest.Models {
		if model.ID == recommendedID {
			return model, true
		}
	}
	return semanticManifestModel{}, false
}

func semanticManifestModelByIdentity(manifest semanticModelManifest, modelID string, vectorSpaceID string) (semanticManifestModel, bool) {
	modelID = strings.TrimSpace(modelID)
	vectorSpaceID = strings.TrimSpace(vectorSpaceID)
	for _, model := range manifest.Models {
		if model.ID != modelID {
			continue
		}
		if vectorSpaceID != "" && model.VectorSpaceID != vectorSpaceID {
			continue
		}
		return model, true
	}
	return semanticManifestModel{}, false
}

func semanticManifestRecommendedRuntimePack(manifest semanticModelManifest) (semanticManifestRuntimePack, bool) {
	recommendedID := strings.TrimSpace(manifest.RecommendedRuntimePack)
	if recommendedID == "" && len(manifest.RuntimePacks) == 1 {
		recommendedID = manifest.RuntimePacks[0].ID
	}
	if recommendedID == "" {
		return semanticManifestRuntimePack{}, false
	}
	for _, pack := range manifest.RuntimePacks {
		if pack.ID == recommendedID {
			return pack, true
		}
	}
	return semanticManifestRuntimePack{}, false
}

func semanticManifestModelProfile(model semanticManifestModel, manifestURL string, platform string) catalog.SemanticModelProfileStatus {
	return semanticManifestModelProfileWithRole(model, manifestURL, platform, catalog.SemanticModelRoleRecommended)
}

func semanticManifestModelProfileWithRole(model semanticManifestModel, manifestURL string, platform string, role string) catalog.SemanticModelProfileStatus {
	artifact, hasArtifact := semanticModelArtifactForPlatform(model.Artifacts, platform)
	packStatus := catalog.SemanticModelPackStatusAvailable
	var artifactStatus *catalog.SemanticModelArtifactStatus
	if hasArtifact {
		artifactStatus = &catalog.SemanticModelArtifactStatus{
			Filename:  artifact.Filename,
			URL:       artifact.URL,
			SHA256:    artifact.SHA256,
			SizeBytes: artifact.SizeBytes,
		}
	} else {
		packStatus = "unsupported_platform"
	}
	return catalog.SemanticModelProfileStatus{
		ModelID:       model.ID,
		VectorSpaceID: model.VectorSpaceID,
		EmbeddingDim:  model.EmbeddingDim,
		Role:          strings.TrimSpace(role),
		ProfileKind:   catalog.SemanticProfileKindModelPack,
		InputKind:     model.InputKind,
		ModelPack: &catalog.SemanticModelPackStatus{
			ID:             model.ID,
			Name:           model.Name,
			Version:        model.Version,
			Role:           strings.TrimSpace(role),
			Status:         packStatus,
			Source:         strings.TrimSpace(manifestURL),
			InputKind:      model.InputKind,
			VectorSpaceID:  model.VectorSpaceID,
			EmbeddingDim:   model.EmbeddingDim,
			QueryLanguages: model.QueryLanguages,
			Runtime:        model.Runtime,
			Quantization:   model.Quantization,
			SizeBytes:      artifact.SizeBytes,
			License:        model.License,
			Artifact:       artifactStatus,
			Installed:      false,
		},
	}
}

func semanticManifestRuntimePackStatus(pack semanticManifestRuntimePack, manifestURL string, platform string) catalog.SemanticRuntimePackStatus {
	artifact, hasArtifact := semanticModelArtifactForPlatform(pack.Artifacts, platform)
	packStatus := catalog.SemanticModelPackStatusAvailable
	var artifactStatus *catalog.SemanticModelArtifactStatus
	if hasArtifact {
		artifactStatus = &catalog.SemanticModelArtifactStatus{
			Filename:  artifact.Filename,
			URL:       artifact.URL,
			SHA256:    artifact.SHA256,
			SizeBytes: artifact.SizeBytes,
		}
	} else {
		packStatus = "unsupported_platform"
	}
	return catalog.SemanticRuntimePackStatus{
		ID:        pack.ID,
		Name:      pack.Name,
		Version:   pack.Version,
		Runtime:   pack.Runtime,
		Status:    packStatus,
		Source:    strings.TrimSpace(manifestURL),
		SizeBytes: artifact.SizeBytes,
		License:   pack.License,
		Platform:  platform,
		Artifact:  artifactStatus,
		Installed: false,
	}
}

func semanticModelArtifactForPlatform(artifacts map[string]semanticModelArtifact, platform string) (semanticModelArtifact, bool) {
	return semanticmanifest.ArtifactForPlatform(artifacts, platform)
}

func semanticRuntimePackAlreadyListed(packs []catalog.SemanticRuntimePackStatus, pack catalog.SemanticRuntimePackStatus) bool {
	for _, existing := range packs {
		if existing.ID == pack.ID && existing.Version == pack.Version {
			return true
		}
	}
	return false
}

func semanticProfileAlreadyListed(profiles []catalog.SemanticModelProfileStatus, profile catalog.SemanticModelProfileStatus) bool {
	for _, existing := range profiles {
		if existing.ModelID == profile.ModelID && existing.VectorSpaceID == profile.VectorSpaceID {
			return true
		}
	}
	return false
}
