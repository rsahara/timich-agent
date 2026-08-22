package adminapi

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
	runtimestate "github.com/rsahara/timich-agent/internal/runtime"
	"github.com/rsahara/timich-agent/internal/store"
)

const (
	adminCookieName   = "timich_admin_token"
	adminCookiePrefix = "v1."
)

const (
	maxFormBodyBytes          = 64 << 10
	maxJSONBodyBytes          = 1 << 20
	assetSearchPreviewTimeout = 25 * time.Second
)

const initialDatasourceIndexingStatusScript = `<script id="initialDatasourceIndexingStatus" type="application/json">null</script>`

// Options configures optional admin actions that need process ownership.
type Options struct {
	Restart                  func(context.Context) error
	UpdateManifestURL        string
	UpdateHTTPClient         *http.Client
	SemanticModelManifestURL string
	SemanticModelHTTPClient  *http.Client
}

// NewMux returns the local admin API surface for setup and diagnostics.
func NewMux(runtime *runtimestate.AgentRuntime) http.Handler {
	return NewMuxWithOptions(runtime, Options{})
}

// NewMuxWithOptions returns the local admin API surface with optional lifecycle hooks.
func NewMuxWithOptions(runtime *runtimestate.AgentRuntime, options Options) http.Handler {
	api := &server{runtime: runtime, options: options, semanticInstallJobs: newSemanticInstallJobStore()}
	api.scheduleSemanticModelStatusRefresh()
	mux := http.NewServeMux()
	mux.HandleFunc("/", api.index)
	mux.HandleFunc("/login", api.login)
	mux.HandleFunc("/logout", api.logout)
	mux.HandleFunc("/setup-admin-token", api.setupAdminToken)
	mux.HandleFunc("/healthz", health("ok"))
	mux.HandleFunc("/readyz", health("ready"))
	mux.HandleFunc("/version", api.version)
	mux.HandleFunc("/status", api.requireAdmin(api.status))
	mux.HandleFunc("/config", api.requireAdmin(api.config))
	mux.HandleFunc("/v1/datasource/primary", api.requireAdmin(api.primaryDatasource))
	mux.HandleFunc("/v1/datasource/primary/check", api.requireAdmin(api.primaryDatasourceCheck))
	mux.HandleFunc("/v1/datasources", api.requireAdmin(api.datasources))
	mux.HandleFunc("/v1/datasources/indexing", api.requireAdmin(api.datasourceIndexing))
	mux.HandleFunc("/v1/datasources/indexing/run", api.requireAdmin(api.datasourceIndexingRun))
	mux.HandleFunc("/v1/catalog/dedup/status", api.requireAdmin(api.catalogDedupStatus))
	mux.HandleFunc("/v1/catalog/dedup/repair", api.requireAdmin(api.catalogDedupRepair))
	mux.HandleFunc("/v1/datasources/local/scan", api.requireAdmin(api.localDatasourceScan))
	mux.HandleFunc("/v1/datasources/local/root/accept", api.requireAdmin(api.localMediaRootAccept))
	mux.HandleFunc("/v1/datasources/local/immich-fallback", api.requireAdmin(api.localDatasourceImmichFallback))
	mux.HandleFunc("/v1/datasources/local/phase0-diagnostics.csv", api.requireAdmin(api.localDatasourcePhase0DiagnosticsCSV))
	mux.HandleFunc("/v1/datasources/local/failure-diagnostics.csv", api.requireAdmin(api.localDatasourceFailureDiagnosticsCSV))
	mux.HandleFunc("/v1/datasources/embeddings/failures.csv", api.requireAdmin(api.semanticEmbeddingFailureDiagnosticsCSV))
	mux.HandleFunc("/v1/datasources/embeddings/retry-failed", api.requireAdmin(api.semanticEmbeddingFailureRetry))
	mux.HandleFunc("/v1/datasources/local/metadata/repair", api.requireAdmin(api.localDatasourceMetadataRequeue))
	mux.HandleFunc("/v1/datasources/local/thumbnails/repair", api.requireAdmin(api.localDatasourceThumbnailRepair))
	mux.HandleFunc("/v1/datasources/local/embeddings/repair", api.requireAdmin(api.localDatasourceEmbeddingRepair))
	mux.HandleFunc("/v1/workers", api.requireAdmin(api.workers))
	mux.HandleFunc("/v1/system/resources", api.requireAdmin(api.systemResources))
	mux.HandleFunc("/v1/semantic-models", api.requireAdmin(api.semanticModels))
	mux.HandleFunc("/v1/semantic-install-job", api.requireAdmin(api.semanticInstallJob))
	mux.HandleFunc("/v1/semantic-models/install", api.requireAdmin(api.semanticModelInstall))
	mux.HandleFunc("/v1/semantic-models/activate", api.requireAdmin(api.semanticModelActivate))
	mux.HandleFunc("/v1/semantic-models/uninstall", api.requireAdmin(api.semanticModelUninstall))
	mux.HandleFunc("/v1/semantic-models/recommended/install", api.requireAdmin(api.semanticModelRecommendedInstall))
	mux.HandleFunc("/v1/semantic-runtime-packs/recommended/install", api.requireAdmin(api.semanticRuntimePackRecommendedInstall))
	mux.HandleFunc("/v1/semantic-models/search/enable", api.requireAdmin(api.semanticModelSearchEnable))
	mux.HandleFunc("/v1/semantic-indexing/run", api.requireAdmin(api.semanticIndexingRun))
	mux.HandleFunc("/v1/assets/search-preview", api.requireAdmin(api.assetSearchPreview))
	mux.HandleFunc("/v1/assets/", api.requireAdmin(api.assetPreview))
	mux.HandleFunc("/v1/nearby-links", api.requireAdmin(api.nearbyLinks))
	mux.HandleFunc("/v1/nearby-links/approve", api.requireAdmin(api.approveNearbyLink))
	mux.HandleFunc("/v1/nearby-links/", api.requireAdmin(api.nearbyLink))
	mux.HandleFunc("/v1/pairing-sessions", api.requireAdmin(api.pairingSessions))
	mux.HandleFunc("/v1/pairing-links", api.requireAdmin(api.pairingLinks))
	mux.HandleFunc("/v1/compatibility-check", api.requireAdmin(api.compatibilityCheck))
	mux.HandleFunc("/v1/update-check", api.requireAdmin(api.updateCheck))
	mux.HandleFunc("/v1/restart", api.requireAdmin(api.restart))
	mux.HandleFunc("/v1/uploads/roots", api.requireAdmin(api.uploadRoots))
	mux.HandleFunc("/v1/devices", api.requireAdmin(api.devices))
	mux.HandleFunc("/v1/devices/", api.requireAdmin(api.device))
	return mux
}

type server struct {
	runtime                      *runtimestate.AgentRuntime
	options                      Options
	semanticInstallJobs          *semanticInstallJobStore
	semanticModelsRefreshMu      sync.Mutex
	semanticModelsRefreshBusy    bool
	semanticModelsRefreshStarted time.Time
	semanticModelsRefreshAt      time.Time
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "route_not_found",
			"message": "Unknown admin route.",
		})
		return
	}
	if s.authenticated(r) {
		writeHTML(w, http.StatusOK, s.dashboardHTML(r.Context()))
		return
	}
	if !s.runtime.AdminAuthReady() {
		writeHTML(w, http.StatusOK, setupHTML(""))
		return
	}
	writeHTML(w, http.StatusOK, loginHTML(""))
}

func (s *server) dashboardHTML(ctx context.Context) string {
	if s == nil || s.runtime == nil {
		return dashboardHTML
	}
	snapshotCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	snapshot, ok := s.runtime.CachedDatasourceIndexingStatus(snapshotCtx)
	if !ok {
		return dashboardHTML
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return dashboardHTML
	}
	replacement := strings.Replace(initialDatasourceIndexingStatusScript, "null", string(payload), 1)
	return strings.Replace(dashboardHTML, initialDatasourceIndexingStatusScript, replacement, 1)
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !s.runtime.AdminAuthReady() {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if s.authenticated(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		writeHTML(w, http.StatusOK, loginHTML(""))
	case http.MethodPost:
		if !s.runtime.AdminAuthReady() {
			writeHTML(w, http.StatusBadRequest, setupHTML("Create an admin token before signing in."))
			return
		}
		if err := parseLimitedForm(w, r); err != nil {
			writeHTML(w, http.StatusBadRequest, loginHTML("Could not read the submitted token."))
			return
		}
		token := strings.TrimSpace(r.FormValue("token"))
		if !s.runtime.AuthenticateAdminToken(token) {
			writeHTML(w, http.StatusUnauthorized, loginHTML("The admin token was not accepted."))
			return
		}
		setAdminCookie(w, r, token)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	default:
		writeMethodNotAllowed(w, "Use GET or POST for admin login.")
	}
}

func setAdminCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    encodeAdminCookieValue(token),
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Now().Add(12 * time.Hour),
		MaxAge:   int((12 * time.Hour).Seconds()),
	})
}

func encodeAdminCookieValue(token string) string {
	return adminCookiePrefix + base64.RawURLEncoding.EncodeToString([]byte(token))
}

func decodeAdminCookieValue(value string) string {
	if !strings.HasPrefix(value, adminCookiePrefix) {
		return value
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, adminCookiePrefix))
	if err != nil {
		return ""
	}
	return string(decoded)
}

func (s *server) setupAdminToken(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to create the initial admin token.") {
		return
	}
	if s.runtime.AdminAuthReady() {
		writeError(w, http.StatusConflict, "admin_token_already_configured", "The admin token is already configured.")
		return
	}
	if err := parseLimitedForm(w, r); err != nil {
		writeHTML(w, http.StatusBadRequest, setupHTML("Could not read the submitted token."))
		return
	}
	token := strings.TrimSpace(r.FormValue("token"))
	confirmation := strings.TrimSpace(r.FormValue("confirmToken"))
	if token != confirmation {
		writeHTML(w, http.StatusBadRequest, setupHTML("The admin tokens did not match."))
		return
	}
	if err := s.runtime.SetAdminToken(token); err != nil {
		if errors.Is(err, runtimestate.ErrAdminTokenTooShort) {
			writeHTML(w, http.StatusBadRequest, setupHTML("Use at least 16 characters for the admin token."))
			return
		}
		if errors.Is(err, runtimestate.ErrAdminTokenAlreadyConfigured) {
			writeError(w, http.StatusConflict, "admin_token_already_configured", "The admin token is already configured.")
			return
		}
		writeHTML(w, http.StatusInternalServerError, setupHTML("Could not save the admin token."))
		return
	}
	setAdminCookie(w, r, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to sign out.") {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func health(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"service": "timich-agent",
			"status":  status,
		})
	}
}

func (s *server) version(w http.ResponseWriter, _ *http.Request) {
	status := s.runtime.InfoResponse()
	payload := map[string]any{
		"service": "timich-agent",
		"version": status.Version,
		"mode":    status.Mode,
	}
	if status.Commit != "" {
		payload["commit"] = status.Commit
	}
	if status.BuiltAt != "" {
		payload["builtAt"] = status.BuiltAt
	}
	if status.ReleaseTag != "" {
		payload["releaseTag"] = status.ReleaseTag
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.runtime.StatusResponseWithContext(r.Context()))
}

func (s *server) updateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "Use GET to check for agent updates.")
		return
	}
	manifestURL := strings.TrimSpace(s.options.UpdateManifestURL)
	currentInfo := s.runtime.InfoResponse()
	currentVersion := currentInfo.Version
	platform := runtimePlatform()
	if manifestURL == "" {
		writeJSON(w, http.StatusOK, updateCheckResponse{
			CurrentVersion: currentVersion,
			Status:         "disabled",
			Platform:       platform,
			Message:        "Update checks are not configured for this build.",
		})
		return
	}
	client := s.options.UpdateHTTPClient
	if client == nil {
		client = updateHTTPClient()
	}
	manifest, err := fetchUpdateManifest(r.Context(), client, manifestURL)
	if err != nil {
		writeJSON(w, http.StatusOK, updateCheckResponse{
			CurrentVersion: currentVersion,
			Status:         "unavailable",
			ManifestURL:    manifestURL,
			Platform:       platform,
			Message:        err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, buildUpdateCheckResponse(
		currentVersion,
		currentInfo.Commit,
		currentInfo.ReleaseTag,
		manifestURL,
		manifest,
	))
}

func (s *server) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.runtime.ConfigResponseWithContext(r.Context()))
}

func (s *server) primaryDatasource(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.runtime.PrimaryDatasource())
	case http.MethodPut:
		var request struct {
			Name        string `json:"name"`
			Kind        string `json:"kind"`
			URL         string `json:"url"`
			AccessToken string `json:"accessToken"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the datasource request.")
			return
		}
		datasource, err := s.runtime.UpdatePrimaryDatasource(config.DatasourceConfig{
			Name:        request.Name,
			Kind:        request.Kind,
			URL:         request.URL,
			AccessToken: request.AccessToken,
		})
		if err != nil {
			writeDatasourceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, datasource)
	default:
		writeMethodNotAllowed(w, "Use GET or PUT for the primary datasource.")
	}
}

func (s *server) datasources(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.runtime.Datasources())
	case http.MethodPost:
		var request struct {
			Name        string `json:"name"`
			Kind        string `json:"kind"`
			URL         string `json:"url"`
			AccessToken string `json:"accessToken"`
			RootKey     string `json:"rootKey"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the datasource request.")
			return
		}
		summary, err := s.runtime.AddDatasource(config.DatasourceConfig{
			Name:        request.Name,
			Kind:        request.Kind,
			URL:         request.URL,
			AccessToken: request.AccessToken,
			RootKey:     request.RootKey,
		})
		if err != nil {
			writeDatasourceError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, summary)
	default:
		writeMethodNotAllowed(w, "Use GET to list datasources or POST to add one.")
	}
}

func (s *server) localDatasourceImmichFallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeMethodNotAllowed(w, "Use PUT to update local datasource Immich fallback.")
		return
	}
	var request struct {
		SourceKey string `json:"sourceKey"`
		Enabled   bool   `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the fallback setting request.")
		return
	}
	summary, err := s.runtime.UpdateLocalDatasourceImmichFallback(request.SourceKey, request.Enabled)
	if err != nil {
		writeDatasourceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *server) primaryDatasourceCheck(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to check the primary datasource.") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	check := s.runtime.DatasourceCheck(ctx)
	writeJSON(w, http.StatusOK, check)
}

func (s *server) datasourceIndexing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "Use GET to inspect datasource indexing.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var (
		status runtimestate.DatasourceIndexingResponse
		err    error
	)
	if r.URL.Query().Get("refresh") == "1" {
		status, err = s.runtime.RefreshDatasourceIndexingStatus(ctx)
	} else {
		status, err = s.runtime.DatasourceIndexingStatus(ctx)
	}
	if err != nil && !errors.Is(err, catalog.ErrNoDatasourceConfigured) {
		writeDatasourceIndexingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *server) datasourceIndexingRun(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to run datasource indexing.") {
		return
	}
	var request struct {
		SourceKey string `json:"sourceKey"`
		Kind      string `json:"kind"`
		Mode      string `json:"mode"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the datasource indexing request.")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Hour)
	defer cancel()
	result, err := s.runtime.RunDatasourceIndexing(ctx, runtimestate.DatasourceIndexingRunOptions{
		SourceKey: request.SourceKey,
		Kind:      request.Kind,
		Mode:      request.Mode,
	})
	if err != nil {
		writeLocalDatasourceScanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) localDatasourceScan(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		status, err := s.runtime.LocalDatasourceScanStatus(ctx)
		if err != nil && !errors.Is(err, catalog.ErrNoDatasourceConfigured) {
			writeLocalDatasourceScanError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodPost:
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Hour)
		defer cancel()
		result, err := s.runtime.RunLocalDatasourcePhase0Scans(ctx)
		if err != nil {
			writeLocalDatasourceScanError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	default:
		writeMethodNotAllowed(w, "Use GET to inspect local datasource scans or POST to run reconciliation.")
	}
}

func (s *server) localMediaRootAccept(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, "Use POST to accept the currently observed local media root identity.")
		return
	}
	var request struct {
		SourceKey        string `json:"sourceKey"`
		RootKey          string `json:"rootKey"`
		ObservedIdentity string `json:"observedIdentity"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the local media root acceptance request.")
		return
	}
	if strings.TrimSpace(request.SourceKey) == "" || strings.TrimSpace(request.RootKey) == "" || strings.TrimSpace(request.ObservedIdentity) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "Datasource, root, and observed identity are required.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Hour)
	defer cancel()
	result, err := s.runtime.AcceptLocalMediaRoot(ctx, request.SourceKey, request.RootKey, request.ObservedIdentity)
	if err != nil {
		writeLocalMediaRootAcceptanceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) localDatasourcePhase0DiagnosticsCSV(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "Use GET to export local datasource Phase 0 diagnostics.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	sourceKey := strings.TrimSpace(r.URL.Query().Get("sourceKey"))
	rows, err := s.runtime.LocalPhase0DiagnosticRows(ctx, sourceKey)
	if err != nil {
		writeLocalDatasourceScanError(w, err)
		return
	}
	filename := "timich-local-phase0-diagnostics.csv"
	if sourceKey != "" {
		filename = "timich-local-phase0-" + sourceKey + ".csv"
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	writer := csv.NewWriter(w)
	if err := writer.Write(catalog.LocalPhase0DiagnosticCSVHeader()); err != nil {
		return
	}
	for _, row := range rows {
		if err := writer.Write(row.CSVRecord()); err != nil {
			return
		}
	}
	writer.Flush()
}

func (s *server) localDatasourceFailureDiagnosticsCSV(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "Use GET to export local datasource failure diagnostics.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	sourceKey := strings.TrimSpace(r.URL.Query().Get("sourceKey"))
	rows, err := s.runtime.LocalFailureDiagnosticRows(ctx, sourceKey)
	if err != nil {
		writeLocalDatasourceScanError(w, err)
		return
	}
	filename := "timich-local-failure-diagnostics.csv"
	if sourceKey != "" {
		filename = "timich-local-failures-" + sourceKey + ".csv"
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	writer := csv.NewWriter(w)
	if err := writer.Write(catalog.LocalFailureDiagnosticCSVHeader()); err != nil {
		return
	}
	for _, row := range rows {
		if err := writer.Write(row.CSVRecord()); err != nil {
			return
		}
	}
	writer.Flush()
}

func (s *server) semanticEmbeddingFailureDiagnosticsCSV(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "Use GET to export embedding failure diagnostics.")
		return
	}
	diagnostics, err := s.runtime.OpenSemanticEmbeddingFailureDiagnostics(r.Context())
	if err != nil {
		writeSemanticIndexingError(w, err)
		return
	}
	defer diagnostics.Close()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="timich-embedding-failures.csv"`)
	writer := csv.NewWriter(w)
	if err := writer.Write(catalog.SemanticEmbeddingFailureDiagnosticCSVHeader()); err != nil {
		return
	}
	written := 0
	for {
		row, ok, err := diagnostics.Next()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("timich-agent stream semantic embedding failure diagnostics failed error=%v", err)
			}
			return
		}
		if !ok {
			break
		}
		if err := writer.Write(row.CSVRecord()); err != nil {
			return
		}
		written++
		if written%256 == 0 {
			writer.Flush()
			if writer.Error() != nil {
				return
			}
		}
	}
	writer.Flush()
}

func (s *server) semanticEmbeddingFailureRetry(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to retry failed embeddings.") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result, err := s.runtime.RetryFailedSemanticEmbeddings(ctx)
	if err != nil {
		writeSemanticIndexingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) localDatasourceThumbnailRepair(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to requeue failed local datasource thumbnails.") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	result, err := s.runtime.RequeueFailedLocalDatasourceThumbnails(ctx)
	if err != nil {
		writeLocalDatasourceScanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) localDatasourceMetadataRequeue(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to requeue failed local datasource metadata.") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	result, err := s.runtime.RequeueFailedLocalDatasourceMetadata(ctx)
	if err != nil {
		writeLocalDatasourceScanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) localDatasourceEmbeddingRepair(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to repair local datasource embeddings.") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	result, err := s.runtime.RepairLocalDatasourceEmbeddings(ctx)
	if err != nil {
		writeSemanticIndexingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) workers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.runtime.WorkerRuntimeStatus())
	case http.MethodPut:
		var request config.WorkerRuntimeConfig
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the worker settings request.")
				return
			}
		}
		if request.HeavyTaskWorkers != nil && *request.HeavyTaskWorkers < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "heavyTaskWorkers must not be negative.")
			return
		}
		status, err := s.runtime.UpdateWorkerRuntime(request)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "worker_settings_update_failed", "Could not update worker settings.")
			return
		}
		writeJSON(w, http.StatusOK, status)
	default:
		writeMethodNotAllowed(w, "Use GET or PUT for worker settings.")
	}
}

func (s *server) systemResources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "Use GET for system resources.")
		return
	}
	writeJSON(w, http.StatusOK, s.runtime.SystemResourcesStatus())
}

func (s *server) catalogDedupRepair(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to repair catalog links.") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	status, err := s.runtime.RepairCatalogDeduplication(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "catalog_dedup_repair_failed", "Could not repair catalog links.")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *server) catalogDedupStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "Use GET to check catalog links.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	status, err := s.runtime.CatalogDeduplicationStatus(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "catalog_dedup_status_failed", "Could not check catalog links.")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *server) semanticModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "Use GET to inspect semantic models.")
		return
	}
	switch {
	case r.URL.Query().Get("cached") == "1":
		if status, ok := s.runtime.CachedSemanticModelRegistryStatus(r.Context()); ok {
			s.scheduleSemanticModelStatusRefresh()
			writeJSON(w, http.StatusOK, status)
			return
		}
		s.scheduleSemanticModelStatusRefresh()
		writeJSON(w, http.StatusOK, s.semanticModelLoadingStatus())
		return
	case r.URL.Query().Get("refresh") == "1":
		ctx, cancel := context.WithTimeout(r.Context(), semanticModelStatusRefreshTimeout)
		defer cancel()
		status, err := s.refreshSemanticModelStatusSnapshot(ctx)
		if err != nil {
			if cached, ok := s.runtime.CachedSemanticModelRegistryStatus(r.Context()); ok {
				writeJSON(w, http.StatusOK, cached)
				return
			}
			writeJSON(w, http.StatusOK, s.semanticModelLoadingStatus())
			return
		}
		writeJSON(w, http.StatusOK, status)
		return
	}

	status, err := s.buildSemanticModelsStatus(r.Context())
	if err == nil {
		s.runtime.RememberSemanticModelRegistryStatus(status)
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *server) semanticModelRecommendedInstall(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to install the recommended semantic model.") {
		return
	}
	s.startSemanticInstallJob(w, semanticInstallJobStart{
		Action: "install_model",
		Label:  "Installing recommended semantic model",
	}, func(ctx context.Context) (any, string, error) {
		result, err := s.installRecommendedSemanticModel(ctx)
		if err != nil {
			return nil, "", err
		}
		return result, "Installed " + result.ModelPack.Name + ".", nil
	})
}

func (s *server) installRecommendedSemanticModel(ctx context.Context) (catalog.SemanticModelPackInstallResult, error) {
	manifestURL := strings.TrimSpace(s.options.SemanticModelManifestURL)
	if manifestURL == "" {
		return catalog.SemanticModelPackInstallResult{}, errors.New("semantic model registry is not configured for this build")
	}
	client := s.options.SemanticModelHTTPClient
	if client == nil {
		client = semanticModelHTTPClient()
	}
	manifest, err := fetchSemanticModelManifest(ctx, client, manifestURL)
	if err != nil {
		return catalog.SemanticModelPackInstallResult{}, err
	}
	recommended, ok := semanticManifestRecommendedModel(manifest)
	if !ok {
		return catalog.SemanticModelPackInstallResult{}, errors.New("semantic model registry did not include a recommended model")
	}
	profile := semanticManifestModelProfile(recommended, manifestURL, runtimePlatform())
	if profile.ModelPack == nil || profile.ModelPack.Artifact == nil {
		return catalog.SemanticModelPackInstallResult{}, catalog.ErrSemanticModelPackInvalid
	}
	response, err := fetchSemanticModelArtifact(ctx, client, profile.ModelPack.Artifact.URL)
	if err != nil {
		return catalog.SemanticModelPackInstallResult{}, err
	}
	defer response.Body.Close()
	return s.runtime.InstallSemanticModelPack(ctx, *profile.ModelPack, response.Body)
}

func (s *server) semanticModelInstall(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to install a semantic model.") {
		return
	}
	var request semanticModelActionRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the semantic model install request.")
			return
		}
	}
	modelID := strings.TrimSpace(request.ModelID)
	vectorSpaceID := strings.TrimSpace(request.VectorSpaceID)
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "modelId is required.")
		return
	}
	s.startSemanticInstallJob(w, semanticInstallJobStart{
		Action:        "install_model",
		Label:         "Installing semantic model",
		ModelID:       modelID,
		VectorSpaceID: vectorSpaceID,
	}, func(ctx context.Context) (any, string, error) {
		result, err := s.installSemanticModelByIdentity(ctx, modelID, vectorSpaceID)
		if err != nil {
			return nil, "", err
		}
		return result, "Installed " + result.ModelPack.Name + ". Indexing will continue in Datasource Tasks.", nil
	})
}

func (s *server) installSemanticModelByIdentity(ctx context.Context, modelID string, vectorSpaceID string) (semanticModelInstallResponse, error) {
	manifestURL := strings.TrimSpace(s.options.SemanticModelManifestURL)
	if manifestURL == "" {
		return semanticModelInstallResponse{}, errors.New("semantic model registry is not configured for this build")
	}
	client := s.options.SemanticModelHTTPClient
	if client == nil {
		client = semanticModelHTTPClient()
	}
	manifest, err := fetchSemanticModelManifest(ctx, client, manifestURL)
	if err != nil {
		return semanticModelInstallResponse{}, err
	}
	model, ok := semanticManifestModelByIdentity(manifest, modelID, vectorSpaceID)
	if !ok {
		return semanticModelInstallResponse{}, catalog.ErrSemanticModelPackInvalid
	}
	profile := semanticManifestModelProfileWithRole(model, manifestURL, runtimePlatform(), "")
	if profile.ModelPack == nil || profile.ModelPack.Artifact == nil {
		return semanticModelInstallResponse{}, catalog.ErrSemanticModelPackInvalid
	}
	response, err := fetchSemanticModelArtifact(ctx, client, profile.ModelPack.Artifact.URL)
	if err != nil {
		return semanticModelInstallResponse{}, err
	}
	defer response.Body.Close()
	result, err := s.runtime.InstallSemanticModelPack(ctx, *profile.ModelPack, response.Body)
	if err != nil {
		return semanticModelInstallResponse{}, err
	}
	installResponse := semanticModelInstallResponse{SemanticModelPackInstallResult: result}
	if _, err := s.runtime.UpdateSemanticIndexing(config.SemanticIndexingConfig{
		Enabled:   true,
		Interval:  semanticSearchEnableDefaultInterval,
		BatchSize: semanticSearchEnableDefaultBatch,
	}); err != nil {
		installResponse.WarningCode = "semantic_indexing_schedule_failed"
		installResponse.WarningMessage = "Semantic model installed, but background indexing could not be scheduled."
		return installResponse, nil
	}
	return installResponse, nil
}

func (s *server) semanticModelActivate(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to activate a semantic model.") {
		return
	}
	if s.semanticInstallJobRunning() {
		writeError(w, http.StatusConflict, "semantic_install_running", "Wait for the current semantic model install to finish before activating a model.")
		return
	}
	var request semanticModelActionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the semantic model activation request.")
		return
	}
	result, err := s.runtime.ActivateSemanticModel(r.Context(), request.ModelID, request.VectorSpaceID)
	if err != nil {
		writeSemanticModelActivationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) semanticModelUninstall(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to uninstall a semantic model.") {
		return
	}
	if s.semanticInstallJobRunning() {
		writeError(w, http.StatusConflict, "semantic_install_running", "Wait for the current semantic model install to finish before uninstalling a model.")
		return
	}
	var request semanticModelActionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the semantic model uninstall request.")
		return
	}
	result, err := s.runtime.UninstallSemanticModelPack(r.Context(), request.ModelID, request.VectorSpaceID)
	if err != nil {
		writeSemanticModelUninstallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *server) semanticRuntimePackRecommendedInstall(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to install the recommended semantic runtime pack.") {
		return
	}
	s.startSemanticInstallJob(w, semanticInstallJobStart{
		Action: "install_runtime",
		Label:  "Installing semantic runtime pack",
	}, func(ctx context.Context) (any, string, error) {
		result, err := s.installRecommendedSemanticRuntimePack(ctx)
		if err != nil {
			return nil, "", err
		}
		return result, "Installed " + result.RuntimePack.Name + ".", nil
	})
}

func (s *server) installRecommendedSemanticRuntimePack(ctx context.Context) (catalog.SemanticRuntimePackInstallResult, error) {
	manifestURL := strings.TrimSpace(s.options.SemanticModelManifestURL)
	if manifestURL == "" {
		return catalog.SemanticRuntimePackInstallResult{}, errors.New("semantic model registry is not configured for this build")
	}
	client := s.options.SemanticModelHTTPClient
	if client == nil {
		client = semanticModelHTTPClient()
	}
	manifest, err := fetchSemanticModelManifest(ctx, client, manifestURL)
	if err != nil {
		return catalog.SemanticRuntimePackInstallResult{}, err
	}
	recommended, ok := semanticManifestRecommendedRuntimePack(manifest)
	if !ok {
		return catalog.SemanticRuntimePackInstallResult{}, errors.New("semantic model registry did not include a recommended runtime pack")
	}
	runtimePack := semanticManifestRuntimePackStatus(recommended, manifestURL, runtimePlatform())
	if runtimePack.Artifact == nil {
		return catalog.SemanticRuntimePackInstallResult{}, catalog.ErrSemanticRuntimePackInvalid
	}
	response, err := fetchSemanticModelArtifact(ctx, client, runtimePack.Artifact.URL)
	if err != nil {
		return catalog.SemanticRuntimePackInstallResult{}, err
	}
	defer response.Body.Close()
	return s.runtime.InstallSemanticRuntimePack(ctx, runtimePack, response.Body)
}

func (s *server) semanticIndexingRun(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to run semantic indexing.") {
		return
	}
	var request struct {
		MaxAssets int `json:"maxAssets"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the semantic indexing request.")
			return
		}
	}
	if request.MaxAssets < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "maxAssets must not be negative.")
		return
	}
	ctx := r.Context()
	cancel := func() {}
	if request.MaxAssets > 0 {
		ctx, cancel = context.WithTimeout(ctx, 10*time.Minute)
	}
	defer cancel()
	result, err := s.runtime.RunSemanticIndexing(ctx, request.MaxAssets)
	if err != nil {
		writeSemanticIndexingError(w, err)
		return
	}
	status := http.StatusOK
	if request.MaxAssets == 0 {
		status = http.StatusAccepted
	}
	writeJSON(w, status, result)
}

func (s *server) assetSearchPreview(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to run an asset search preview.") {
		return
	}
	var request adminAssetSearchPreviewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the asset search request.")
		return
	}
	searchCtx, cancelSearch := context.WithTimeout(r.Context(), assetSearchPreviewTimeout)
	defer cancelSearch()
	started := time.Now()
	page, err := s.runtime.SearchAssetsForAdminPreview(searchCtx, request.AssetSearchRequest, catalog.AssetSearchOptions{
		SemanticModelID:       request.SemanticModelID,
		SemanticVectorSpaceID: request.SemanticVectorSpaceID,
	})
	if err != nil {
		writeAdminCatalogError(w, err)
		return
	}
	page.ElapsedMs = max(0, time.Since(started).Milliseconds())
	writeJSON(w, http.StatusOK, page)
}

type adminAssetSearchPreviewRequest struct {
	catalog.AssetSearchRequest
	SemanticModelID       string `json:"semanticModelId,omitempty"`
	SemanticVectorSpaceID string `json:"semanticVectorSpaceId,omitempty"`
}

func (s *server) assetPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeMethodNotAllowed(w, "Use GET or HEAD to read asset preview content.")
		return
	}
	assetID, variant, ok := parseAdminAssetRequest(r.URL.Path)
	if !ok || variant != "preview" {
		writeError(w, http.StatusNotFound, "route_not_found", "Unknown asset preview route.")
		return
	}
	response, err := s.runtime.Preview(r, assetID)
	if err != nil {
		writeAdminCatalogError(w, err)
		return
	}
	defer response.Body.Close()
	copyAdminProxyResponse(w, r.Method, response)
}

func (s *server) pairingSessions(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to create a pairing session.") {
		return
	}

	pairingSession, err := s.runtime.CreatePairingSession()
	if err != nil {
		writePairingError(w, err)
		return
	}
	response := s.buildPairingSessionAPIResponse(r, pairingSession)
	writeJSON(w, http.StatusCreated, response)
}

func (s *server) nearbyLinks(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/nearby-links" {
		writeError(w, http.StatusNotFound, "route_not_found", "The Nearby Link route was not found.")
		return
	}
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "Use GET to list Nearby Link requests.")
		return
	}
	links, err := s.runtime.NearbyLinks()
	if err != nil {
		writeNearbyLinkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nearbyLinks": links,
	})
}

func (s *server) approveNearbyLink(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to approve a Nearby Link request.") {
		return
	}
	var request struct {
		LinkCode string `json:"linkCode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the Nearby Link approval request.")
		return
	}
	response, err := s.runtime.ApproveNearbyLink(request.LinkCode)
	if err != nil {
		writeNearbyLinkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) nearbyLink(w http.ResponseWriter, r *http.Request) {
	linkID, action, ok := parseNearbyLinkAdminRoute(r.URL.Path)
	if !ok || action != "deny" {
		writeError(w, http.StatusNotFound, "route_not_found", "The Nearby Link route was not found.")
		return
	}
	if !requirePost(w, r, "Use POST to deny a Nearby Link request.") {
		return
	}
	response, err := s.runtime.DenyNearbyLink(linkID)
	if err != nil {
		writeNearbyLinkError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) pairingLinks(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to create a pairing link.") {
		return
	}

	var request pairingLinkAPIRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the pairing link request.")
		return
	}
	if strings.TrimSpace(request.PairingCode) == "" {
		writeError(w, http.StatusBadRequest, "pairing_code_required", "Pairing code is required.")
		return
	}
	activeSession, err := s.runtime.ActivePairingSession(request.PairingCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, "pairing_session_invalid", "Create a fresh pairing code before generating a QR code.")
		return
	}
	request.PairingCode = activeSession.PairingCode
	request.ExpiresAt = activeSession.ExpiresAt
	response, err := s.buildPairingLinkAPIResponse(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "agent_base_url_invalid", "Use an http or https Media API URL that the app device can reach.")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) compatibilityCheck(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to run a compatibility check.") {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.runtime.CompatibilityCheck(ctx))
}

func (s *server) restart(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to restart the agent.") {
		return
	}
	if s.options.Restart == nil {
		writeError(w, http.StatusServiceUnavailable, "restart_unavailable", "Agent restart is not available in this runtime.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.options.Restart(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, "restart_failed", "Could not request an agent restart.")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "restarting",
		"message": "The agent restart was requested.",
	})
}

func (s *server) devices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "Use GET to list paired devices.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"devices": s.runtime.DeviceSummaries(),
	})
}

func (s *server) uploadRoots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "Use GET to list upload roots.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"roots": s.runtime.UploadRootStatuses(),
	})
}

func (s *server) device(w http.ResponseWriter, r *http.Request) {
	devicePath := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/devices/"), "/")
	segments := strings.Split(devicePath, "/")
	if len(segments) == 0 || strings.TrimSpace(segments[0]) == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "device_not_found",
			"message": "The paired device was not found.",
		})
		return
	}
	deviceID := strings.TrimSpace(segments[0])
	if len(segments) == 1 {
		s.deviceMetadata(w, r, deviceID)
		return
	}
	if len(segments) != 2 {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "device_route_not_found",
			"message": "The paired device route was not found.",
		})
		return
	}
	switch segments[1] {
	case "upload-policy":
		s.deviceUploadPolicy(w, r, deviceID)
	case "upload-reset":
		s.deviceUploadReset(w, r, deviceID)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "device_route_not_found",
			"message": "The paired device route was not found.",
		})
	}
}

func (s *server) deviceMetadata(w http.ResponseWriter, r *http.Request, deviceID string) {
	switch r.Method {
	case http.MethodPut:
		var request runtimestate.DeviceUpdate
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the device update request.")
			return
		}
		device, err := s.runtime.UpdateDevice(deviceID, request)
		if err != nil {
			writeDeviceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, device)
	case http.MethodDelete:
		s.revokeDevice(w, r, deviceID)
	default:
		writeMethodNotAllowed(w, "Use PUT to rename or DELETE to revoke a paired device.")
	}
}

func (s *server) revokeDevice(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodDelete {
		writeMethodNotAllowed(w, "Use DELETE to revoke a paired device.")
		return
	}
	if err := s.runtime.RevokeDevice(deviceID); err != nil {
		if errors.Is(err, store.ErrDeviceNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "device_not_found",
				"message": "The paired device was not found.",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":   "device_revoke_failed",
			"message": "Could not revoke the paired device.",
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) deviceUploadPolicy(w http.ResponseWriter, r *http.Request, deviceID string) {
	switch r.Method {
	case http.MethodGet:
		policy, err := s.runtime.DeviceUploadPolicy(deviceID)
		if err != nil {
			writeUploadError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, policy)
	case http.MethodPut:
		var request runtimestate.DeviceUploadPolicyUpdate
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the upload policy request.")
			return
		}
		policy, err := s.runtime.UpdateDeviceUploadPolicy(deviceID, request)
		if err != nil {
			writeUploadError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, policy)
	default:
		writeMethodNotAllowed(w, "Use GET or PUT for device upload policy.")
	}
}

func (s *server) deviceUploadReset(w http.ResponseWriter, r *http.Request, deviceID string) {
	if !requirePost(w, r, "Use POST to reset device upload state.") {
		return
	}
	var request runtimestate.UploadResetInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Could not parse the upload reset request.")
		return
	}
	request.DeviceID = deviceID
	response, err := s.runtime.ResetDeviceUploadState(request)
	if err != nil {
		writeUploadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error":   "unauthorized",
				"message": "Admin authentication is required.",
			})
			return
		}
		next(w, r)
	}
}

func (s *server) authenticated(r *http.Request) bool {
	if s.runtime == nil {
		return false
	}
	token := bearerTokenFromHeader(r.Header.Get("Authorization"))
	if token == "" {
		if cookie, err := r.Cookie(adminCookieName); err == nil {
			token = decodeAdminCookieValue(cookie.Value)
		}
	}
	return s.runtime.AuthenticateAdminToken(token)
}

func bearerTokenFromHeader(value string) string {
	trimmedValue := strings.TrimSpace(value)
	if !strings.HasPrefix(strings.ToLower(trimmedValue), "bearer ") {
		return ""
	}
	return strings.TrimSpace(trimmedValue[len("Bearer "):])
}

func requirePost(w http.ResponseWriter, r *http.Request, message string) bool {
	if r.Method == http.MethodPost {
		return true
	}
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", message)
	return false
}

func parseLimitedForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBodyBytes)
	return r.ParseForm()
}

func parseNearbyLinkAdminRoute(path string) (linkID string, action string, ok bool) {
	trimmedPath := strings.Trim(strings.TrimPrefix(path, "/v1/nearby-links/"), "/")
	if trimmedPath == "" {
		return "", "", false
	}
	parts := strings.Split(trimmedPath, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func writePairingError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrDeviceLimitReached) {
		writeError(w, http.StatusConflict, "device_limit_reached", "The local agent has reached its paired-device limit.")
		return
	}
	writeError(w, http.StatusInternalServerError, "pairing_session_create_failed", "Could not create a pairing session.")
}

func writeNearbyLinkError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNearbyLinkNotFound):
		writeError(w, http.StatusNotFound, "nearby_link_not_found", "The Nearby Link request was not found.")
	case errors.Is(err, store.ErrNearbyLinkDenied):
		writeError(w, http.StatusGone, "nearby_link_denied", "The Nearby Link request was denied.")
	case errors.Is(err, store.ErrNearbyLinkConsumed):
		writeError(w, http.StatusGone, "nearby_link_used", "The Nearby Link request has already been used.")
	case errors.Is(err, store.ErrNearbyLinkLimitReached):
		writeError(w, http.StatusTooManyRequests, "nearby_link_limit_reached", "Too many Nearby Link requests are active.")
	case errors.Is(err, store.ErrDeviceLimitReached):
		writeError(w, http.StatusConflict, "device_limit_reached", "The local agent has reached its paired-device limit.")
	default:
		writeError(w, http.StatusInternalServerError, "nearby_link_failed", "Could not complete the Nearby Link request.")
	}
}

func writeDatasourceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtimestate.ErrPrimaryDatasourceRequired):
		writeError(w, http.StatusBadRequest, "datasource_url_required", "Datasource URL is required.")
	case errors.Is(err, runtimestate.ErrDatasourceAccessTokenNeeded):
		writeError(w, http.StatusBadRequest, "datasource_access_token_required", "Immich API key is required for the datasource.")
	case errors.Is(err, runtimestate.ErrDatasourceAlreadyConfigured):
		writeError(w, http.StatusConflict, "datasource_already_configured", "A datasource with the same target is already configured.")
	case errors.Is(err, runtimestate.ErrDatasourceNotFound):
		writeError(w, http.StatusNotFound, "datasource_not_found", "The local datasource was not found.")
	case errors.Is(err, runtimestate.ErrUploadRootNotFound):
		writeError(w, http.StatusBadRequest, "local_media_root_required", "Select a configured local media root.")
	case errors.Is(err, config.ErrImmichPassthroughRequiresSingleDatasource):
		writeError(w, http.StatusConflict, "immich_passthrough_requires_single_datasource", "Immich passthrough must be the only datasource. Convert it to Immich indexed before adding another datasource.")
	default:
		writeError(w, http.StatusBadRequest, "datasource_invalid", "Could not save the datasource configuration.")
	}
}

func writeMirrorError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrCatalogNotConfigured):
		writeError(w, http.StatusBadRequest, "mirror_not_configured", "Datasource mirror is not configured.")
	case errors.Is(err, catalog.ErrUnsupportedSearch):
		writeError(w, http.StatusBadRequest, "mirror_mode_unsupported", "Mirror sync mode is not supported.")
	default:
		writeError(w, http.StatusBadGateway, "mirror_sync_failed", "Could not sync the datasource mirror.")
	}
}

func writeLocalDatasourceScanError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrNoDatasourceConfigured):
		writeError(w, http.StatusBadRequest, "local_datasource_not_configured", "No local filesystem datasource is configured.")
	case errors.Is(err, runtimestate.ErrDatasourceDiscoveryAlreadyRunning):
		writeError(w, http.StatusConflict, "datasource_discovery_running", "Media discovery is already running.")
	case errors.Is(err, runtimestate.ErrStorageWriteBlocked):
		writeError(w, http.StatusInsufficientStorage, "storage_write_blocked", "Agent storage free space is below the write guardrail.")
	default:
		writeError(w, http.StatusInternalServerError, "local_datasource_scan_failed", "Could not inspect or run the local datasource scan.")
	}
}

func writeLocalMediaRootAcceptanceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrNoDatasourceConfigured):
		writeError(w, http.StatusBadRequest, "local_datasource_not_configured", "The local datasource or root was not found.")
	case errors.Is(err, catalog.ErrLocalMediaRootAcceptanceStale):
		writeError(w, http.StatusConflict, "local_root_acceptance_stale", "The local media root changed after it was inspected. Refresh status and review it again.")
	case errors.Is(err, catalog.ErrLocalMediaRootAcceptanceNotRequired):
		writeError(w, http.StatusConflict, "local_root_acceptance_not_required", "The local media root does not currently require acceptance.")
	case errors.Is(err, runtimestate.ErrStorageWriteBlocked):
		writeError(w, http.StatusInsufficientStorage, "storage_write_blocked", "Agent storage free space is below the write guardrail.")
	default:
		writeError(w, http.StatusConflict, "local_root_acceptance_failed", "Could not safely accept the local media root.")
	}
}

func writeDatasourceIndexingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrNoDatasourceConfigured):
		writeError(w, http.StatusServiceUnavailable, "datasource_not_configured", "No datasource is configured on this agent.")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeError(w, http.StatusServiceUnavailable, "datasource_status_busy", "Datasource task status is still loading.")
	default:
		writeError(w, http.StatusServiceUnavailable, "datasource_status_unavailable", "Could not refresh datasource task status.")
	}
}

func writeAdminCatalogError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrNoDatasourceConfigured):
		writeError(w, http.StatusServiceUnavailable, "datasource_not_configured", "No datasource is configured on this agent.")
	case errors.Is(err, catalog.ErrInvalidSearchRequest):
		writeError(w, http.StatusBadRequest, "invalid_search_request", "The asset search request is not valid.")
	case errors.Is(err, catalog.ErrUnsupportedSearch):
		writeError(w, http.StatusBadRequest, "unsupported_search", "The requested asset search is not supported by this datasource.")
	case errors.Is(err, catalog.ErrAssetNotFound):
		writeError(w, http.StatusNotFound, "asset_not_found", "The requested asset could not be found.")
	case errors.Is(err, catalog.ErrMediaTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "media_too_large", "The requested media response is too large.")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeError(w, http.StatusGatewayTimeout, "catalog_request_timeout", "The catalog request took too long. Try again when the datasource is less busy.")
	default:
		writeError(w, http.StatusBadGateway, "catalog_proxy_failed", "Could not fetch data from the configured datasource.")
	}
}

func parseAdminAssetRequest(path string) (assetID string, variant string, ok bool) {
	trimmedPath := strings.TrimPrefix(path, "/v1/assets/")
	parts := strings.Split(trimmedPath, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func copyAdminProxyResponse(w http.ResponseWriter, requestMethod string, response *catalog.UpstreamMediaResponse) {
	for _, headerName := range []string{
		"Content-Type",
		"Content-Length",
		"Cache-Control",
		"ETag",
		"Accept-Ranges",
		"Content-Range",
		"Content-Disposition",
		"Last-Modified",
		"Server-Timing",
	} {
		if value := response.Header.Get(headerName); value != "" {
			w.Header().Set(headerName, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if requestMethod == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, response.Body)
}

func writeDeviceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrDeviceNotFound):
		writeError(w, http.StatusNotFound, "device_not_found", "The paired device was not found.")
	case errors.Is(err, store.ErrDeviceNameInvalid):
		writeError(w, http.StatusBadRequest, "device_name_invalid", "Device name is required.")
	default:
		writeError(w, http.StatusInternalServerError, "device_update_failed", "Could not update the paired device.")
	}
}

func writeUploadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrDeviceNotFound):
		writeError(w, http.StatusNotFound, "device_not_found", "The paired device was not found.")
	case errors.Is(err, runtimestate.ErrUploadRootNotFound):
		writeError(w, http.StatusBadRequest, "upload_root_not_found", "Select a configured upload root.")
	case errors.Is(err, runtimestate.ErrUploadResetRangeRequired):
		writeError(w, http.StatusBadRequest, "upload_reset_range_required", "Select a captured-at date range for upload reset.")
	case errors.Is(err, runtimestate.ErrUploadResetInvalid):
		writeError(w, http.StatusBadRequest, "upload_reset_invalid", "Select a valid captured-at date range for upload reset.")
	case errors.Is(err, runtimestate.ErrUploadPolicyInvalid):
		writeError(w, http.StatusBadRequest, "upload_policy_invalid", "Upload policy is invalid.")
	default:
		writeError(w, http.StatusInternalServerError, "upload_request_failed", "Could not update upload configuration.")
	}
}

func writeMethodNotAllowed(w http.ResponseWriter, message string) {
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", message)
}

func writeError(w http.ResponseWriter, status int, code string, message string) {
	writeJSON(w, status, map[string]string{
		"error":   code,
		"message": message,
	})
}

func writeHTML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
