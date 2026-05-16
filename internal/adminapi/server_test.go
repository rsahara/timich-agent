package adminapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
	runtimestate "github.com/rsahara/timich-agent/internal/runtime"
	"github.com/rsahara/timich-agent/internal/store"
)

type agentBaseURLChoicePayload struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

func TestMuxDiagnosticsRoutes(t *testing.T) {
	t.Parallel()

	handler := NewMux(newTestRuntime(t, 5))
	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantKey    string
		wantValue  string
	}{
		{name: "health", path: "/healthz", wantStatus: http.StatusOK, wantKey: "status", wantValue: "ok"},
		{name: "ready", path: "/readyz", wantStatus: http.StatusOK, wantKey: "status", wantValue: "ready"},
		{name: "version", path: "/version", wantStatus: http.StatusOK, wantKey: "version", wantValue: "test-version"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if recorder.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", recorder.Header().Get("Content-Type"))
			}

			var payload map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
			}
			if payload[tc.wantKey] != tc.wantValue {
				t.Fatalf("%s = %v, want %q payload=%v", tc.wantKey, payload[tc.wantKey], tc.wantValue, payload)
			}
		})
	}
}

func TestIndexServesLoginWithoutCookie(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/html", got)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("Admin token")) {
		t.Fatalf("login page body = %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`value="timich-agent-admin"`)) {
		t.Fatalf("login page is missing password-manager username: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`autocomplete="username"`)) {
		t.Fatalf("login page is missing username autocomplete hint: %s", recorder.Body.String())
	}
}

func TestIndexServesSetupWhenAdminTokenMissing(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntimeWithoutAdminToken(t)).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("Create admin token")) {
		t.Fatalf("setup page body = %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("Admin UI sign-in, Admin API bearer auth, and CLI admin commands")) {
		t.Fatalf("setup page is missing admin token usage guidance: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("Save this as the password for timich-agent-admin")) {
		t.Fatalf("setup page is missing password-manager save guidance: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`value="timich-agent-admin"`)) {
		t.Fatalf("setup page is missing password-manager username: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`pattern="[A-Za-z0-9]{16,128}"`)) {
		t.Fatalf("setup page is missing cookie-safe token pattern: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`passwordrules="minlength: 16; maxlength: 128; allowed: upper, lower, digit;"`)) {
		t.Fatalf("setup page is missing password generator rules: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`id="generateAdminToken"`)) {
		t.Fatalf("setup page is missing token generator control: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`id="toggleAdminToken"`)) {
		t.Fatalf("setup page is missing token visibility control: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`stored in the local agent state file`)) {
		t.Fatalf("setup page is missing token storage note: %s", recorder.Body.String())
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte(`id="copyAdminToken"`)) {
		t.Fatalf("setup page should not include token copy control: %s", recorder.Body.String())
	}
}

func TestIndexServesDashboardWithCopyPairingControl(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.Bytes()
	if !bytes.Contains(body, []byte("Pair New Device")) {
		t.Fatalf("dashboard body is missing explicit device pairing section: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Create device pairing code")) {
		t.Fatalf("dashboard body is missing pairing action: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("pairing the Timich app on a phone or tablet")) {
		t.Fatalf("dashboard body is missing pairing explanation: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("copyPairingCode")) {
		t.Fatalf("dashboard body is missing copy pairing control: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("pairingQRCode")) {
		t.Fatalf("dashboard body is missing pairing QR image: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("copyPairingLink")) {
		t.Fatalf("dashboard body is missing copy pairing link control: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Agent Update")) {
		t.Fatalf("dashboard body is missing update section: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("/v1/update-check")) {
		t.Fatalf("dashboard body is missing update-check API call: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("http://immich_server:2283")) {
		t.Fatalf("dashboard body is missing Immich Docker datasource placeholder: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("/v1/datasource/primary/check")) {
		t.Fatalf("dashboard body is missing datasource check API call: %s", recorder.Body.String())
	}
}

func TestAdminRoutesRequireAuthentication(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/status"},
		{method: http.MethodGet, path: "/config"},
		{method: http.MethodGet, path: "/v1/datasource/primary"},
		{method: http.MethodPut, path: "/v1/datasource/primary"},
		{method: http.MethodPost, path: "/v1/datasource/primary/check"},
		{method: http.MethodPost, path: "/v1/pairing-sessions"},
		{method: http.MethodPost, path: "/v1/pairing-links"},
		{method: http.MethodPost, path: "/v1/compatibility-check"},
		{method: http.MethodGet, path: "/v1/update-check"},
		{method: http.MethodPost, path: "/v1/restart"},
		{method: http.MethodGet, path: "/v1/devices"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))

			assertErrorPayload(t, recorder, http.StatusUnauthorized, "unauthorized")
		})
	}
}

func TestAuthenticatedAdminRoutes(t *testing.T) {
	t.Parallel()

	handler := NewMux(newTestRuntime(t, 5))
	tests := []struct {
		name      string
		path      string
		wantKey   string
		wantValue string
	}{
		{name: "status", path: "/status", wantKey: "agentId", wantValue: "agent-test"},
		{name: "config", path: "/config", wantKey: "agentName", wantValue: "test-agent"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, authenticatedRequest(http.MethodGet, tc.path, nil))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
			}
			if payload[tc.wantKey] != tc.wantValue {
				t.Fatalf("%s = %v, want %q payload=%v", tc.wantKey, payload[tc.wantKey], tc.wantValue, payload)
			}
		})
	}
}

func TestLoginSetsAdminCookie(t *testing.T) {
	t.Parallel()

	adminToken := `unsafe;admin token"with\slash`
	form := url.Values{"token": {adminToken}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler := NewMux(newTestRuntimeWithAdminToken(t, 5, adminToken))

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 body=%s", recorder.Code, recorder.Body.String())
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != adminCookieName {
		t.Fatalf("cookies = %#v, want admin cookie", cookies)
	}
	if cookies[0].Value == adminToken || strings.ContainsAny(cookies[0].Value, " ;\"\\,") {
		t.Fatalf("cookie value = %q, want encoded cookie-safe token", cookies[0].Value)
	}
	if got := decodeAdminCookieValue(cookies[0].Value); got != adminToken {
		t.Fatalf("decoded cookie = %q, want %q", got, adminToken)
	}

	statusRequest := httptest.NewRequest(http.MethodGet, "/status", nil)
	statusRequest.AddCookie(cookies[0])
	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, statusRequest)
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("encoded cookie auth status = %d, want 200 body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
}

func TestSetupAdminTokenPersistsTokenAndSetsCookie(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(
		http.MethodPost,
		"/setup-admin-token",
		strings.NewReader("token=created-admin-token&confirmToken=created-admin-token"),
	)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	runtime := newTestRuntimeWithoutAdminToken(t)

	NewMux(runtime).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 body=%s", recorder.Code, recorder.Body.String())
	}
	if !runtime.AuthenticateAdminToken("created-admin-token") {
		t.Fatal("created admin token was not accepted")
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != adminCookieName {
		t.Fatalf("cookies = %#v, want admin cookie", cookies)
	}
	if got := decodeAdminCookieValue(cookies[0].Value); got != "created-admin-token" {
		t.Fatalf("decoded cookie = %q, want created admin token", got)
	}
}

func TestSetupAdminTokenRejectsShortToken(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodPost, "/setup-admin-token", strings.NewReader("token=short&confirmToken=short"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()

	NewMux(newTestRuntimeWithoutAdminToken(t)).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("at least 16 characters")) {
		t.Fatalf("short token response = %s", recorder.Body.String())
	}
}

func TestPrimaryDatasourceGetAndUpdate(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, 5)
	handler := NewMux(runtime)

	initialRecorder := httptest.NewRecorder()
	handler.ServeHTTP(initialRecorder, authenticatedRequest(http.MethodGet, "/v1/datasource/primary", nil))
	if initialRecorder.Code != http.StatusOK {
		t.Fatalf("initial status = %d, want 200 body=%s", initialRecorder.Code, initialRecorder.Body.String())
	}

	updateBody := bytes.NewReader([]byte(`{"name":"Home Immich","kind":"immich","url":"http://immich.local:2283","accessToken":"immich-api-key"}`))
	updateRecorder := httptest.NewRecorder()
	handler.ServeHTTP(updateRecorder, authenticatedRequest(http.MethodPut, "/v1/datasource/primary", updateBody))
	if updateRecorder.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 body=%s", updateRecorder.Code, updateRecorder.Body.String())
	}
	var payload struct {
		Configured     bool   `json:"configured"`
		Name           string `json:"name"`
		URL            string `json:"url"`
		HasAccessToken bool   `json:"hasAccessToken"`
	}
	if err := json.Unmarshal(updateRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("update response is not JSON: %v body=%s", err, updateRecorder.Body.String())
	}
	if !payload.Configured || payload.Name != "Home Immich" || payload.URL != "http://immich.local:2283" || !payload.HasAccessToken {
		t.Fatalf("datasource payload = %#v", payload)
	}
}

func TestPrimaryDatasourceCheckReturnsDatasourceStatus(t *testing.T) {
	t.Parallel()

	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search/metadata" {
			t.Fatalf("unexpected datasource path %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "immich-api-key" {
			t.Fatalf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assets":{"items":[],"total":0,"nextPage":null}}`))
	}))
	defer datasourceServer.Close()

	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.Datasources = []config.DatasourceConfig{{
			Name:        "Home Immich",
			Kind:        "immich",
			URL:         datasourceServer.URL,
			AccessToken: "immich-api-key",
		}}
	})
	recorder := httptest.NewRecorder()
	NewMux(runtime).ServeHTTP(
		recorder,
		authenticatedRequest(http.MethodPost, "/v1/datasource/primary/check", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.Name != "datasource" || payload.Status != "ok" {
		t.Fatalf("check payload = %#v, want datasource ok", payload)
	}
	if !strings.Contains(payload.Summary, "metadata request") {
		t.Fatalf("summary = %q, want metadata request detail", payload.Summary)
	}
}

func TestPrimaryDatasourceCheckReportsProbeFailure(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.Datasources = []config.DatasourceConfig{{
			Name:        "Home Immich",
			Kind:        "immich",
			URL:         "http://127.0.0.1:1",
			AccessToken: "immich-api-key",
		}}
	})
	recorder := httptest.NewRecorder()
	NewMux(runtime).ServeHTTP(
		recorder,
		authenticatedRequest(http.MethodPost, "/v1/datasource/primary/check", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Name        string         `json:"name"`
		Status      string         `json:"status"`
		Remediation string         `json:"remediation"`
		Details     map[string]any `json:"details"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.Name != "datasource" || payload.Status != "failed" {
		t.Fatalf("check payload = %#v, want datasource failed", payload)
	}
	if !strings.Contains(payload.Remediation, "agent runtime") {
		t.Fatalf("remediation = %q, want agent runtime hint", payload.Remediation)
	}
	if payload.Details["datasourceURL"] != "http://127.0.0.1:1" {
		t.Fatalf("details = %#v, want datasource URL", payload.Details)
	}
}

func TestRestartEndpointInvokesCallback(t *testing.T) {
	t.Parallel()

	called := make(chan struct{}, 1)
	handler := NewMuxWithOptions(newTestRuntime(t, 5), Options{
		Restart: func(context.Context) error {
			called <- struct{}{}
			return nil
		},
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/restart", nil))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 body=%s", recorder.Code, recorder.Body.String())
	}
	select {
	case <-called:
	default:
		t.Fatal("restart callback was not called")
	}
}

func TestUpdateCheckReportsAvailableRelease(t *testing.T) {
	t.Parallel()

	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"schemaVersion":           1,
			"product":                 "timich-agent",
			"version":                 "0.2.0",
			"minimumSupportedVersion": "0.1.0",
			"notesUrl":                "https://example.test/releases/v0.2.0",
			"artifacts": map[string]any{
				runtimePlatform(): map[string]string{
					"filename": "timich-agent_0.2.0_test.tar.gz",
					"url":      "https://example.test/timich-agent_0.2.0_test.tar.gz",
					"sha256":   strings.Repeat("a", 64),
				},
			},
			"updateGuide": map[string]any{
				"dockerCompose": []string{"Keep .local.", "Restart compose."},
			},
		})
	}))
	defer manifestServer.Close()

	handler := NewMuxWithOptions(newTestRuntimeWithBuildVersion(t, 5, "test-admin-token", "0.1.0"), Options{
		UpdateManifestURL: manifestServer.URL,
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/update-check", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		CurrentVersion string          `json:"currentVersion"`
		LatestVersion  string          `json:"latestVersion"`
		Status         string          `json:"status"`
		Artifact       *updateArtifact `json:"artifact"`
		Guide          updateGuide     `json:"guide"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.CurrentVersion != "0.1.0" || payload.LatestVersion != "0.2.0" || payload.Status != "update_available" {
		t.Fatalf("payload = %+v, want update_available from 0.1.0 to 0.2.0", payload)
	}
	if payload.Artifact == nil || payload.Artifact.Filename == "" {
		t.Fatalf("Artifact = %+v, want platform artifact", payload.Artifact)
	}
	if len(payload.Guide.DockerCompose) != 2 {
		t.Fatalf("Guide = %+v, want docker compose steps", payload.Guide)
	}
}

func TestUpdateCheckRejectsUnsafeManifestURL(t *testing.T) {
	t.Parallel()

	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"schemaVersion": 1,
			"product":       "timich-agent",
			"version":       "0.2.0",
			"notesUrl":      "javascript:alert(1)",
			"artifacts": map[string]any{
				runtimePlatform(): map[string]string{
					"filename": "timich-agent_0.2.0_test.tar.gz",
					"url":      "https://example.test/timich-agent_0.2.0_test.tar.gz",
					"sha256":   strings.Repeat("a", 64),
				},
			},
		})
	}))
	defer manifestServer.Close()

	handler := NewMuxWithOptions(newTestRuntimeWithBuildVersion(t, 5, "test-admin-token", "0.1.0"), Options{
		UpdateManifestURL: manifestServer.URL,
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/update-check", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Status   string          `json:"status"`
		Message  string          `json:"message"`
		NotesURL string          `json:"notesUrl"`
		Artifact *updateArtifact `json:"artifact"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.Status != "unavailable" {
		t.Fatalf("Status = %q, want unavailable", payload.Status)
	}
	if !strings.Contains(payload.Message, "notesUrl") {
		t.Fatalf("Message = %q, want unsafe notesUrl explanation", payload.Message)
	}
	if payload.NotesURL != "" || payload.Artifact != nil {
		t.Fatalf("payload = %+v, want unsafe manifest to expose no links", payload)
	}
}

func TestUpdateCheckDisabledWithoutManifestURL(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()

	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/update-check", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.Status != "disabled" {
		t.Fatalf("Status = %q, want disabled", payload.Status)
	}
}

func TestPairingSessionsRequiresPost(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/pairing-sessions", nil))

	assertErrorPayload(t, recorder, http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestPairingSessionsCreatesSession(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "http://agent.local:8081/v1/pairing-sessions", nil))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		PairingCode         string                      `json:"pairingCode"`
		ExpiresAt           string                      `json:"expiresAt"`
		PairingURL          string                      `json:"pairingURL"`
		PairingPayload      any                         `json:"pairingPayload"`
		AgentBaseURLChoices []agentBaseURLChoicePayload `json:"agentBaseURLChoices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.PairingCode == "" {
		t.Fatalf("pairingCode is empty payload=%v", payload)
	}
	if payload.ExpiresAt == "" {
		t.Fatalf("expiresAt is empty payload=%v", payload)
	}
	if payload.PairingURL != "" || payload.PairingPayload != nil {
		t.Fatalf("pairing session should be code-first without generated link fields payload=%#v", payload)
	}
	if !hasAgentBaseURLChoice(payload.AgentBaseURLChoices, "http://agent.local:8082") {
		t.Fatalf("agentBaseURLChoices = %#v, want current Admin UI host candidate", payload.AgentBaseURLChoices)
	}
}

func TestPairingSessionsUsesPublishedMediaPortForAdminHostCandidate(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.MediaPublishedAddress = "18082"
	})
	NewMux(runtime).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "http://agent.local:8081/v1/pairing-sessions", nil))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		AgentBaseURLChoices []agentBaseURLChoicePayload `json:"agentBaseURLChoices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if !hasAgentBaseURLChoice(payload.AgentBaseURLChoices, "http://agent.local:18082") {
		t.Fatalf("agentBaseURLChoices = %#v, want published media port candidate", payload.AgentBaseURLChoices)
	}
	if hasAgentBaseURLChoice(payload.AgentBaseURLChoices, "http://agent.local:8082") {
		t.Fatalf("agentBaseURLChoices = %#v, should not use container media port when published port is configured", payload.AgentBaseURLChoices)
	}
}

func TestPairingSessionsSkipsAdminHostCandidateForLoopbackPublishedMedia(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.MediaPublishedAddress = "127.0.0.1:18082"
	})
	NewMux(runtime).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "http://agent.local:8081/v1/pairing-sessions", nil))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		AgentBaseURLChoices []agentBaseURLChoicePayload `json:"agentBaseURLChoices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if len(payload.AgentBaseURLChoices) != 0 {
		t.Fatalf("agentBaseURLChoices = %#v, want no phone-reachable candidate for loopback host media publishing", payload.AgentBaseURLChoices)
	}
}

func TestPairingSessionsUsesPublishedMediaHostCandidate(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.MediaPublishedAddress = "10.0.111.128:18082"
	})
	NewMux(runtime).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "http://127.0.0.1:8081/v1/pairing-sessions", nil))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		AgentBaseURLChoices []agentBaseURLChoicePayload `json:"agentBaseURLChoices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if !hasAgentBaseURLChoice(payload.AgentBaseURLChoices, "http://10.0.111.128:18082") {
		t.Fatalf("agentBaseURLChoices = %#v, want published media host candidate", payload.AgentBaseURLChoices)
	}
}

func TestPairingSessionsCreatesCodeFirstResponseForLoopbackAdminHost(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "http://127.0.0.1:8081/v1/pairing-sessions", nil))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		PairingCode          string                      `json:"pairingCode"`
		PairingURL           string                      `json:"pairingURL"`
		PairingQRCodeDataURL string                      `json:"pairingQRCodeDataURL"`
		PairingPayload       any                         `json:"pairingPayload"`
		AgentBaseURLChoices  []agentBaseURLChoicePayload `json:"agentBaseURLChoices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.PairingCode == "" {
		t.Fatalf("pairingCode is empty payload=%v", payload)
	}
	if payload.PairingPayload != nil || payload.PairingURL != "" || payload.PairingQRCodeDataURL != "" {
		t.Fatalf("link fields should be omitted for code-only pairing payload=%#v", payload)
	}
	if len(payload.AgentBaseURLChoices) != 0 {
		t.Fatalf("agentBaseURLChoices = %#v, want no Docker/internal interface candidates from loopback Admin UI", payload.AgentBaseURLChoices)
	}
}

func TestPairingSessionsSkipsAdminHostChoiceForProxiedRequests(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	request := authenticatedRequest(http.MethodPost, "http://agent.local:8081/v1/pairing-sessions", nil)
	request.Header.Set("X-Forwarded-Host", "admin.example")
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		PairingCode         string                      `json:"pairingCode"`
		AgentBaseURLChoices []agentBaseURLChoicePayload `json:"agentBaseURLChoices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.PairingCode == "" {
		t.Fatal("pairingCode is empty")
	}
	if len(payload.AgentBaseURLChoices) != 0 {
		t.Fatalf("agentBaseURLChoices = %#v, should not trust proxied Admin UI host or detected interfaces", payload.AgentBaseURLChoices)
	}
}

func TestPairingLinksCreatesQRCodeForSelectedAgentBaseURL(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, 5)
	pairingSession, err := runtime.CreatePairingSession()
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"agentBaseURL":"http://10.0.1.4:8082/","pairingCode":"` + pairingSession.PairingCode + `"}`))
	NewMux(runtime).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/pairing-links", body))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		PairingPayload struct {
			Version      int    `json:"version"`
			Kind         string `json:"kind"`
			AgentBaseURL string `json:"agentBaseURL"`
			PairingCode  string `json:"pairingCode"`
		} `json:"pairingPayload"`
		PairingURL           string `json:"pairingURL"`
		PairingQRCodeDataURL string `json:"pairingQRCodeDataURL"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.PairingPayload.AgentBaseURL != "http://10.0.1.4:8082" {
		t.Fatalf("agent base URL = %q, want selected media URL", payload.PairingPayload.AgentBaseURL)
	}
	if payload.PairingPayload.Version != 1 || payload.PairingPayload.Kind != "timich.agent.pairing" {
		t.Fatalf("pairing payload = %#v, want Timich pairing payload", payload.PairingPayload)
	}
	if payload.PairingPayload.PairingCode != pairingSession.PairingCode {
		t.Fatalf("pairing code = %q, want active pairing code", payload.PairingPayload.PairingCode)
	}
	if !strings.HasPrefix(payload.PairingURL, "https://link.timich.runo.jp/pair?payload=") {
		t.Fatalf("pairing URL = %q, want production Universal Link", payload.PairingURL)
	}
	if !strings.HasPrefix(payload.PairingQRCodeDataURL, "data:image/png;base64,") {
		t.Fatalf("pairing QR data URL prefix = %q", payload.PairingQRCodeDataURL[:min(len(payload.PairingQRCodeDataURL), 32)])
	}

	parsedPairingURL, err := url.Parse(payload.PairingURL)
	if err != nil {
		t.Fatalf("pairing URL did not parse: %v", err)
	}
	encodedPayload := parsedPairingURL.Query().Get("payload")
	if encodedPayload == "" {
		t.Fatalf("pairing URL missing payload query: %q", payload.PairingURL)
	}
	decodedPayload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		t.Fatalf("pairing URL payload did not decode: %v", err)
	}
	var linkPayload struct {
		AgentBaseURL string `json:"agentBaseURL"`
		PairingCode  string `json:"pairingCode"`
	}
	if err := json.Unmarshal(decodedPayload, &linkPayload); err != nil {
		t.Fatalf("pairing URL payload is not JSON: %v payload=%s", err, string(decodedPayload))
	}
	if linkPayload.AgentBaseURL != payload.PairingPayload.AgentBaseURL || linkPayload.PairingCode != payload.PairingPayload.PairingCode {
		t.Fatalf("link payload = %#v, want response payload", linkPayload)
	}
}

func TestPairingLinksRejectsUnreachableAgentBaseURL(t *testing.T) {
	t.Parallel()

	tests := []string{
		"http://localhost:8082",
		"http://127.0.0.1:8082",
		"http://0.0.0.0:8082",
		"http://[::]:8082",
		"http://[::1]:8082",
	}
	for _, agentBaseURL := range tests {
		t.Run(agentBaseURL, func(t *testing.T) {
			t.Parallel()

			runtime := newTestRuntime(t, 5)
			pairingSession, err := runtime.CreatePairingSession()
			if err != nil {
				t.Fatalf("CreatePairingSession() error = %v", err)
			}

			recorder := httptest.NewRecorder()
			body := bytes.NewReader([]byte(`{"agentBaseURL":"` + agentBaseURL + `","pairingCode":"` + pairingSession.PairingCode + `"}`))
			NewMux(runtime).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/pairing-links", body))

			assertErrorPayload(t, recorder, http.StatusBadRequest, "agent_base_url_invalid")
		})
	}
}

func TestPairingLinksRejectsInactivePairingCode(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"agentBaseURL":"http://10.0.1.4:8082","pairingCode":"PAIRING-CODE"}`))
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/pairing-links", body))

	assertErrorPayload(t, recorder, http.StatusBadRequest, "pairing_session_invalid")
}

func TestPairingLinksRejectsReplacedPairingCode(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, 5)
	firstSession, err := runtime.CreatePairingSession()
	if err != nil {
		t.Fatalf("CreatePairingSession() first error = %v", err)
	}
	if _, err := runtime.CreatePairingSession(); err != nil {
		t.Fatalf("CreatePairingSession() second error = %v", err)
	}

	recorder := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"agentBaseURL":"http://10.0.1.4:8082","pairingCode":"` + firstSession.PairingCode + `"}`))
	NewMux(runtime).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/pairing-links", body))

	assertErrorPayload(t, recorder, http.StatusBadRequest, "pairing_session_invalid")
}

func TestPairingSessionsMapsDeviceLimitError(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 0)).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/pairing-sessions", nil))

	assertErrorPayload(t, recorder, http.StatusConflict, "device_limit_reached")
}

func TestCompatibilityCheckRequiresPost(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/compatibility-check", nil))

	assertErrorPayload(t, recorder, http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestDevicesListAndRevoke(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, 5)
	created, err := runtime.CreateHostedSession("Old iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	handler := NewMux(runtime)

	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, authenticatedRequest(http.MethodGet, "/v1/devices", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var listPayload struct {
		Devices []struct {
			DeviceID string `json:"deviceId"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listPayload); err != nil {
		t.Fatalf("list response is not JSON: %v body=%s", err, listRecorder.Body.String())
	}
	if len(listPayload.Devices) != 1 || listPayload.Devices[0].DeviceID != created.DeviceID {
		t.Fatalf("devices = %#v, want created device", listPayload.Devices)
	}

	revokeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(revokeRecorder, authenticatedRequest(http.MethodDelete, "/v1/devices/"+created.DeviceID, nil))
	if revokeRecorder.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204 body=%s", revokeRecorder.Code, revokeRecorder.Body.String())
	}
	if _, err := runtime.RefreshAppSession(created.RefreshToken, "https://timich.example"); !errors.Is(err, store.ErrRefreshTokenNotFound) {
		t.Fatalf("RefreshAppSession() error = %v, want %v", err, store.ErrRefreshTokenNotFound)
	}
}

func authenticatedRequest(method string, path string, body *bytes.Reader) *http.Request {
	var reader io.Reader
	if body != nil {
		reader = body
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer test-admin-token")
	return request
}

func assertErrorPayload(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()

	if recorder.Code != wantStatus {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, wantStatus, recorder.Body.String())
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload["error"] != wantCode {
		t.Fatalf("error = %q, want %q payload=%v", payload["error"], wantCode, payload)
	}
}

func hasAgentBaseURLChoice(choices []agentBaseURLChoicePayload, target string) bool {
	for _, choice := range choices {
		if choice.URL == target {
			return true
		}
	}
	return false
}

func newTestRuntime(t *testing.T, deviceLimit int) *runtimestate.AgentRuntime {
	t.Helper()

	return newTestRuntimeWithAdminToken(t, deviceLimit, "test-admin-token")
}

func newTestRuntimeWithoutAdminToken(t *testing.T) *runtimestate.AgentRuntime {
	t.Helper()

	return newTestRuntimeWithAdminToken(t, 5, "")
}

func newTestRuntimeWithAdminToken(t *testing.T, deviceLimit int, adminToken string) *runtimestate.AgentRuntime {
	t.Helper()

	return newTestRuntimeWithBuildVersion(t, deviceLimit, adminToken, "test-version")
}

func newTestRuntimeWithBuildVersion(t *testing.T, deviceLimit int, adminToken string, version string) *runtimestate.AgentRuntime {
	return newTestRuntimeWithBuildVersionAndConfig(t, deviceLimit, adminToken, version, nil)
}

func newTestRuntimeWithConfig(
	t *testing.T,
	deviceLimit int,
	adminToken string,
	configure func(*config.ResolvedConfig),
) *runtimestate.AgentRuntime {
	t.Helper()

	return newTestRuntimeWithBuildVersionAndConfig(t, deviceLimit, adminToken, "test-version", configure)
}

func newTestRuntimeWithBuildVersionAndConfig(
	t *testing.T,
	deviceLimit int,
	adminToken string,
	version string,
	configure func(*config.ResolvedConfig),
) *runtimestate.AgentRuntime {
	t.Helper()

	dataDir := t.TempDir()
	cfg := config.ResolvedConfig{
		Config: config.Default(),
	}
	cfg.AgentName = "test-agent"
	cfg.DataDir = dataDir
	cfg.DeviceLimit = max(deviceLimit, 1)
	cfg.ConfigSource = "test"
	cfg.ConfigPath = filepath.Join(dataDir, "agent.json")
	if configure != nil {
		configure(&cfg)
	}

	signingKey := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	runtime, err := runtimestate.NewAgentRuntime(runtimestate.BuildInfo{
		Version: version,
		Commit:  "test-commit",
		BuiltAt: "2026-04-25T00:00:00Z",
	}, cfg, store.LoadedState{
		Path: filepath.Join(dataDir, "agent-state.json"),
		State: store.State{
			AgentID:           "agent-test",
			CreatedAt:         time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			SessionSigningKey: signingKey,
			AdminToken:        adminToken,
		},
	}, time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	if deviceLimit == 0 {
		_, err := runtime.CreateHostedSession("already paired", "https://timich.example")
		if err != nil {
			t.Fatalf("CreateHostedSession() error = %v", err)
		}
	}
	return runtime
}
