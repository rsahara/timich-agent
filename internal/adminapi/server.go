package adminapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
	runtimestate "github.com/rsahara/timich-agent/internal/runtime"
	"github.com/rsahara/timich-agent/internal/store"
)

const (
	adminCookieName   = "timich_admin_token"
	adminCookiePrefix = "v1."
)

const (
	maxFormBodyBytes = 64 << 10
	maxJSONBodyBytes = 1 << 20
)

// Options configures optional admin actions that need process ownership.
type Options struct {
	Restart           func(context.Context) error
	UpdateManifestURL string
	UpdateHTTPClient  *http.Client
}

// NewMux returns the local admin API surface for setup and diagnostics.
func NewMux(runtime *runtimestate.AgentRuntime) http.Handler {
	return NewMuxWithOptions(runtime, Options{})
}

// NewMuxWithOptions returns the local admin API surface with optional lifecycle hooks.
func NewMuxWithOptions(runtime *runtimestate.AgentRuntime, options Options) http.Handler {
	api := &server{runtime: runtime, options: options}
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
	runtime *runtimestate.AgentRuntime
	options Options
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
		writeHTML(w, http.StatusOK, dashboardHTML)
		return
	}
	if !s.runtime.AdminAuthReady() {
		writeHTML(w, http.StatusOK, setupHTML(""))
		return
	}
	writeHTML(w, http.StatusOK, loginHTML(""))
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
	status := s.runtime.StatusResponse()
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
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.runtime.StatusResponse())
}

func (s *server) updateCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "Use GET to check for agent updates.")
		return
	}
	manifestURL := strings.TrimSpace(s.options.UpdateManifestURL)
	currentVersion := s.runtime.StatusResponse().Version
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
	writeJSON(w, http.StatusOK, buildUpdateCheckResponse(currentVersion, manifestURL, manifest))
}

func (s *server) config(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.runtime.ConfigResponse())
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

func (s *server) primaryDatasourceCheck(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r, "Use POST to check the primary datasource.") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	check := s.runtime.DatasourceCheck(ctx)
	writeJSON(w, http.StatusOK, check)
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
	default:
		writeError(w, http.StatusBadRequest, "datasource_invalid", "Could not save the datasource configuration.")
	}
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
