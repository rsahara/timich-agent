package mediaapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
	runtimestate "github.com/rsahara/timich-agent/internal/runtime"
	"github.com/rsahara/timich-agent/internal/store"
)

func TestMuxRootOnlyServesRouteIndex(t *testing.T) {
	t.Parallel()

	handler := NewMux(nil)
	tests := []struct {
		name        string
		path        string
		wantStatus  int
		wantKey     string
		wantValue   string
		wantMessage string
	}{
		{
			name:       "root route index",
			path:       "/",
			wantStatus: http.StatusOK,
			wantKey:    "service",
			wantValue:  "timich-agent-media",
		},
		{
			name:       "unknown route",
			path:       "/missing",
			wantStatus: http.StatusNotFound,
			wantKey:    "error",
			wantValue:  "route_not_found",
		},
		{
			name:        "removed catalog route",
			path:        "/v1/catalog",
			wantStatus:  http.StatusGone,
			wantKey:     "error",
			wantValue:   "catalog_endpoint_removed",
			wantMessage: "GET /v1/catalog has been removed. Use POST /v1/assets/search instead.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}

			var payload map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
			}
			if payload[tc.wantKey] != tc.wantValue {
				t.Fatalf("%s = %v, want %q payload=%v", tc.wantKey, payload[tc.wantKey], tc.wantValue, payload)
			}
			if tc.wantMessage != "" && payload["message"] != tc.wantMessage {
				t.Fatalf("message = %v, want %q payload=%v", payload["message"], tc.wantMessage, payload)
			}
		})
	}
}

func TestWriteCatalogErrorMapsDatasourceFailureToBadGateway(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writeCatalogError(recorder, fmt.Errorf("upstream auth failed: %w", catalog.ErrDatasourceUnavailable))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["error"] != "datasource_unavailable" {
		t.Fatalf("error = %#v, want datasource_unavailable", payload["error"])
	}
}

func TestParseAssetRequestSupportsMetadataAndMediaRoutes(t *testing.T) {
	t.Parallel()

	assetID, variant, ok := parseAssetRequest("/v1/assets/ta1_asset.signature")
	if !ok || assetID != "ta1_asset.signature" || variant != "" {
		t.Fatalf("metadata route = (%q, %q, %v)", assetID, variant, ok)
	}
	assetID, variant, ok = parseAssetRequest("/v1/assets/ta1_asset.signature/preview")
	if !ok || assetID != "ta1_asset.signature" || variant != "preview" {
		t.Fatalf("preview route = (%q, %q, %v)", assetID, variant, ok)
	}
	if _, _, ok := parseAssetRequest("/v1/assets/ta1_asset.signature/"); ok {
		t.Fatal("trailing-slash asset route should be rejected")
	}
}

func TestCopyProxyResponseCopiesStreamingHeaders(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	response := &catalog.UpstreamMediaResponse{
		StatusCode: http.StatusPartialContent,
		Header: http.Header{
			"Content-Type":   []string{"video/mp4"},
			"Cache-Control":  []string{"public, s-maxage=86400"},
			"Accept-Ranges":  []string{"bytes"},
			"Content-Range":  []string{"bytes 0-1/100"},
			"Content-Length": []string{"2"},
			"Server-Timing":  []string{"total;dur=12.0"},
		},
		Body: io.NopCloser(strings.NewReader("ok")),
	}

	copyProxyResponse(recorder, http.MethodGet, response)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("expected status 206, got %d", recorder.Code)
	}
	if recorder.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("expected Accept-Ranges header, got %q", recorder.Header().Get("Accept-Ranges"))
	}
	if recorder.Header().Get("Content-Range") != "bytes 0-1/100" {
		t.Fatalf("expected Content-Range header, got %q", recorder.Header().Get("Content-Range"))
	}
	if recorder.Header().Get("Server-Timing") != "total;dur=12.0" {
		t.Fatalf("expected Server-Timing header, got %q", recorder.Header().Get("Server-Timing"))
	}
	if recorder.Header().Get("Cache-Control") != "private, max-age=3600" {
		t.Fatalf("Cache-Control = %q, want private media policy", recorder.Header().Get("Cache-Control"))
	}
	if recorder.Body.String() != "ok" {
		t.Fatalf("expected body ok, got %q", recorder.Body.String())
	}
}

func TestCopyProxyResponseOmitsHeadBody(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	response := &catalog.UpstreamMediaResponse{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"video/mp4"},
		},
		Body: io.NopCloser(strings.NewReader("should-not-be-written")),
	}

	copyProxyResponse(recorder, http.MethodHead, response)

	if recorder.Body.Len() != 0 {
		t.Fatalf("expected empty HEAD body, got %q", recorder.Body.String())
	}
}

func TestNearbyLinkFlowCreatesSessionAfterAdminApproval(t *testing.T) {
	t.Parallel()

	runtime := newUploadTestRuntime(t)
	handler := NewMux(runtime)

	createRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		createRecorder,
		httptest.NewRequest(http.MethodPost, "http://agent.local:8082/v1/nearby-links", bytes.NewReader([]byte(`{"deviceName":"Living Room TV","deviceKind":"android_tv"}`))),
	)
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 body=%s", createRecorder.Code, createRecorder.Body.String())
	}
	var created struct {
		LinkID    string `json:"linkId"`
		LinkCode  string `json:"linkCode"`
		PollToken string `json:"pollToken"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("create response is not JSON: %v body=%s", err, createRecorder.Body.String())
	}
	if created.LinkID == "" || created.LinkCode == "" || created.PollToken == "" || created.Status != store.NearbyLinkStatusPending {
		t.Fatalf("created = %+v, want pending Nearby Link with poll token", created)
	}

	pendingRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		pendingRecorder,
		httptest.NewRequest(http.MethodPost, "http://agent.local:8082/v1/nearby-links/"+created.LinkID+"/poll", bytes.NewReader([]byte(`{"pollToken":"`+created.PollToken+`"}`))),
	)
	if pendingRecorder.Code != http.StatusOK {
		t.Fatalf("pending status = %d, want 200 body=%s", pendingRecorder.Code, pendingRecorder.Body.String())
	}
	var pending struct {
		Status  string          `json:"status"`
		Session json.RawMessage `json:"session"`
	}
	if err := json.Unmarshal(pendingRecorder.Body.Bytes(), &pending); err != nil {
		t.Fatalf("pending response is not JSON: %v body=%s", err, pendingRecorder.Body.String())
	}
	if pending.Status != store.NearbyLinkStatusPending || len(pending.Session) != 0 {
		t.Fatalf("pending = %+v, want pending without session", pending)
	}

	if _, err := runtime.ApproveNearbyLink(created.LinkCode); err != nil {
		t.Fatalf("ApproveNearbyLink() error = %v", err)
	}

	approvedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		approvedRecorder,
		httptest.NewRequest(http.MethodPost, "http://agent.local:8082/v1/nearby-links/"+created.LinkID+"/poll", bytes.NewReader([]byte(`{"pollToken":"`+created.PollToken+`"}`))),
	)
	if approvedRecorder.Code != http.StatusOK {
		t.Fatalf("approved status = %d, want 200 body=%s", approvedRecorder.Code, approvedRecorder.Body.String())
	}
	var approved struct {
		Status  string `json:"status"`
		Session struct {
			DeviceID     string `json:"deviceId"`
			BaseURL      string `json:"baseURL"`
			RefreshToken string `json:"refreshToken"`
		} `json:"session"`
	}
	if err := json.Unmarshal(approvedRecorder.Body.Bytes(), &approved); err != nil {
		t.Fatalf("approved response is not JSON: %v body=%s", err, approvedRecorder.Body.String())
	}
	if approved.Status != store.NearbyLinkStatusApproved || approved.Session.DeviceID == "" || approved.Session.RefreshToken == "" {
		t.Fatalf("approved = %+v, want approved session", approved)
	}
	if approved.Session.BaseURL != "http://agent.local:8082" {
		t.Fatalf("session baseURL = %q, want request base URL", approved.Session.BaseURL)
	}
}

func TestNearbyLinkCancelDeniesRequest(t *testing.T) {
	t.Parallel()

	runtime := newUploadTestRuntime(t)
	handler := NewMux(runtime)

	createRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		createRecorder,
		httptest.NewRequest(http.MethodPost, "http://agent.local:8082/v1/nearby-links", bytes.NewReader([]byte(`{"deviceName":"Living Room TV","deviceKind":"android_tv"}`))),
	)
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 body=%s", createRecorder.Code, createRecorder.Body.String())
	}
	var created struct {
		LinkID    string `json:"linkId"`
		LinkCode  string `json:"linkCode"`
		PollToken string `json:"pollToken"`
	}
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("create response is not JSON: %v body=%s", err, createRecorder.Body.String())
	}

	invalidRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		invalidRecorder,
		httptest.NewRequest(http.MethodPost, "http://agent.local:8082/v1/nearby-links/"+created.LinkID+"/cancel", bytes.NewReader([]byte(`{"pollToken":"wrong"}`))),
	)
	if invalidRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("invalid cancel status = %d, want 401 body=%s", invalidRecorder.Code, invalidRecorder.Body.String())
	}

	cancelRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		cancelRecorder,
		httptest.NewRequest(http.MethodPost, "http://agent.local:8082/v1/nearby-links/"+created.LinkID+"/cancel", bytes.NewReader([]byte(`{"pollToken":"`+created.PollToken+`"}`))),
	)
	if cancelRecorder.Code != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200 body=%s", cancelRecorder.Code, cancelRecorder.Body.String())
	}
	var canceled struct {
		LinkID    string `json:"linkId"`
		PollToken string `json:"pollToken"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(cancelRecorder.Body.Bytes(), &canceled); err != nil {
		t.Fatalf("cancel response is not JSON: %v body=%s", err, cancelRecorder.Body.String())
	}
	if canceled.LinkID != created.LinkID || canceled.Status != store.NearbyLinkStatusDenied || canceled.PollToken != "" {
		t.Fatalf("canceled = %+v, want denied without poll token", canceled)
	}
	if _, err := runtime.ApproveNearbyLink(created.LinkCode); !errors.Is(err, store.ErrNearbyLinkDenied) {
		t.Fatalf("ApproveNearbyLink(canceled) error = %v, want %v", err, store.ErrNearbyLinkDenied)
	}
}

func TestRequestBaseURLPrefersForwardedHeaders(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/session/refresh", nil)
	request.Host = "127.0.0.1"
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "timich.runo.jp")

	if got := requestBaseURL(request); got != "https://timich.runo.jp" {
		t.Fatalf("requestBaseURL() = %q, want %q", got, "https://timich.runo.jp")
	}
}

func TestRequestBaseURLFallsBackToRequestURL(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodPost, "https://timich.runo.jp/v1/session/refresh", nil)
	request.Host = "timich.runo.jp"

	if got := requestBaseURL(request); got != "https://timich.runo.jp" {
		t.Fatalf("requestBaseURL() = %q, want %q", got, "https://timich.runo.jp")
	}
}

func TestUploadPolicyAndSessionRoutes(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newUploadTestRuntime(t, config.UploadRootConfig{Key: "nas-photos", Path: rootPath})
	sessionBundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	if _, err := runtime.UpdateDeviceUploadPolicy(sessionBundle.DeviceID, runtimestate.DeviceUploadPolicyUpdate{
		Enabled:     true,
		RootKey:     "nas-photos",
		PathPattern: store.DefaultUploadPathPattern(),
	}); err != nil {
		t.Fatalf("UpdateDeviceUploadPolicy() error = %v", err)
	}
	handler := NewMux(runtime)

	meRecorder := httptest.NewRecorder()
	handler.ServeHTTP(meRecorder, authenticatedUploadRequest(http.MethodGet, "/v1/uploads/me", nil, sessionBundle.AccessToken))
	if meRecorder.Code != http.StatusOK {
		t.Fatalf("me status = %d, want 200 body=%s", meRecorder.Code, meRecorder.Body.String())
	}
	var mePayload struct {
		DeviceID string `json:"deviceId"`
		Status   struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(meRecorder.Body.Bytes(), &mePayload); err != nil {
		t.Fatalf("me response is not JSON: %v body=%s", err, meRecorder.Body.String())
	}
	if mePayload.DeviceID != sessionBundle.DeviceID || mePayload.Status.State != "ready" {
		t.Fatalf("me payload = %+v, want ready device upload state", mePayload)
	}

	createBody := bytes.NewReader([]byte(`{"sourceAssetId":"asset-1","sourceAssetVersion":"version-1","mediaType":"image","originalFilename":"IMG_0001.HEIC","capturedAt":"2026-05-31T12:00:00Z","expectedSizeBytes":1024}`))
	createRecorder := httptest.NewRecorder()
	handler.ServeHTTP(createRecorder, authenticatedUploadRequest(http.MethodPost, "/v1/uploads/sessions", createBody, sessionBundle.AccessToken))
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 body=%s", createRecorder.Code, createRecorder.Body.String())
	}
	var createPayload struct {
		State   string `json:"state"`
		Session struct {
			UploadID       string `json:"uploadId"`
			NextOffset     int64  `json:"nextOffset"`
			ChunkSizeBytes int64  `json:"chunkSizeBytes"`
		} `json:"session"`
	}
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &createPayload); err != nil {
		t.Fatalf("create response is not JSON: %v body=%s", err, createRecorder.Body.String())
	}
	if createPayload.State != "accepted" || createPayload.Session.UploadID == "" || createPayload.Session.NextOffset != 0 || createPayload.Session.ChunkSizeBytes == 0 {
		t.Fatalf("create payload = %+v, want accepted upload session", createPayload)
	}

	getRecorder := httptest.NewRecorder()
	handler.ServeHTTP(getRecorder, authenticatedUploadRequest(http.MethodGet, "/v1/uploads/sessions/"+createPayload.Session.UploadID, nil, sessionBundle.AccessToken))
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("get session status = %d, want 200 body=%s", getRecorder.Code, getRecorder.Body.String())
	}

	chunkBody := []byte(strings.Repeat("a", 1024))
	chunkRecorder := httptest.NewRecorder()
	chunkRequest := authenticatedUploadRequest(http.MethodPut, "/v1/uploads/sessions/"+createPayload.Session.UploadID+"/chunk", bytes.NewReader(chunkBody), sessionBundle.AccessToken)
	chunkRequest.Header.Set("X-Timich-Offset", "0")
	chunkRequest.Header.Set("X-Timich-Chunk-SHA1", mediaAPISHA1Hex(chunkBody))
	handler.ServeHTTP(chunkRecorder, chunkRequest)
	if chunkRecorder.Code != http.StatusOK {
		t.Fatalf("chunk status = %d, want 200 body=%s", chunkRecorder.Code, chunkRecorder.Body.String())
	}
	var chunkPayload struct {
		State   string `json:"state"`
		Session struct {
			NextOffset int64 `json:"nextOffset"`
		} `json:"session"`
	}
	if err := json.Unmarshal(chunkRecorder.Body.Bytes(), &chunkPayload); err != nil {
		t.Fatalf("chunk response is not JSON: %v body=%s", err, chunkRecorder.Body.String())
	}
	if chunkPayload.State != "accepted" || chunkPayload.Session.NextOffset != int64(len(chunkBody)) {
		t.Fatalf("chunk payload = %+v, want accepted next offset", chunkPayload)
	}

	staleRecorder := httptest.NewRecorder()
	staleRequest := authenticatedUploadRequest(http.MethodPut, "/v1/uploads/sessions/"+createPayload.Session.UploadID+"/chunk", bytes.NewReader(chunkBody), sessionBundle.AccessToken)
	staleRequest.Header.Set("X-Timich-Offset", "0")
	staleRequest.Header.Set("X-Timich-Chunk-SHA1", mediaAPISHA1Hex(chunkBody))
	handler.ServeHTTP(staleRecorder, staleRequest)
	if staleRecorder.Code != http.StatusConflict {
		t.Fatalf("stale chunk status = %d, want 409 body=%s", staleRecorder.Code, staleRecorder.Body.String())
	}

	completeBody := bytes.NewReader([]byte(`{"sourceAssetVersion":"version-1","checksums":[{"algorithm":"sha1","digest":"` + mediaAPISHA1Hex(chunkBody) + `"}]}`))
	completeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(completeRecorder, authenticatedUploadRequest(http.MethodPost, "/v1/uploads/sessions/"+createPayload.Session.UploadID+"/complete", completeBody, sessionBundle.AccessToken))
	if completeRecorder.Code != http.StatusOK {
		t.Fatalf("complete status = %d, want 200 body=%s", completeRecorder.Code, completeRecorder.Body.String())
	}
	var completePayload struct {
		State         string `json:"state"`
		UploadedAsset struct {
			Status            string `json:"status"`
			FinalRelativePath string `json:"finalRelativePath"`
		} `json:"uploadedAsset"`
	}
	if err := json.Unmarshal(completeRecorder.Body.Bytes(), &completePayload); err != nil {
		t.Fatalf("complete response is not JSON: %v body=%s", err, completeRecorder.Body.String())
	}
	if completePayload.State != "completed" || completePayload.UploadedAsset.Status != "uploaded" || completePayload.UploadedAsset.FinalRelativePath == "" {
		t.Fatalf("complete payload = %+v, want uploaded asset", completePayload)
	}
}

func TestUploadSessionRoutesRequireValidRequest(t *testing.T) {
	t.Parallel()

	runtime := newUploadTestRuntime(t)
	sessionBundle, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	handler := NewMux(runtime)

	unauthorizedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedRecorder, httptest.NewRequest(http.MethodGet, "/v1/uploads/me", nil))
	if unauthorizedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401 body=%s", unauthorizedRecorder.Code, unauthorizedRecorder.Body.String())
	}

	invalidRecorder := httptest.NewRecorder()
	handler.ServeHTTP(invalidRecorder, authenticatedUploadRequest(http.MethodPost, "/v1/uploads/sessions", bytes.NewReader([]byte(`{"sourceAssetId":""}`)), sessionBundle.AccessToken))
	if invalidRecorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid status = %d, want 400 body=%s", invalidRecorder.Code, invalidRecorder.Body.String())
	}
}

func TestWriteUploadErrorMapsFinalPathConflict(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writeUploadError(recorder, runtimestate.ErrUploadFinalPathConflict)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload["error"] != "upload_final_path_conflict" {
		t.Fatalf("error = %q, want upload_final_path_conflict", payload["error"])
	}
}

func TestWriteUploadErrorMapsStorageWriteBlocked(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writeUploadError(recorder, runtimestate.ErrStorageWriteBlocked)
	if recorder.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload["error"] != "storage_write_blocked" {
		t.Fatalf("error = %q, want storage_write_blocked", payload["error"])
	}
}

func authenticatedUploadRequest(method string, path string, body *bytes.Reader, token string) *http.Request {
	var reader io.Reader
	if body != nil {
		reader = body
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func mediaAPISHA1Hex(raw []byte) string {
	sum := sha1.Sum(raw)
	return hex.EncodeToString(sum[:])
}

func newUploadTestRuntime(t *testing.T, uploadRoots ...config.UploadRootConfig) *runtimestate.AgentRuntime {
	t.Helper()

	dataDir := t.TempDir()
	cfg := config.ResolvedConfig{
		Config: config.Default(),
	}
	cfg.AgentName = "test-agent"
	cfg.DataDir = dataDir
	cfg.ConfigSource = "test"
	cfg.ConfigPath = filepath.Join(dataDir, "agent.json")
	cfg.UploadRoots = uploadRoots
	signingKey := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	relayPublicKey, relayPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	runtime, err := runtimestate.NewAgentRuntime(runtimestate.BuildInfo{}, cfg, store.LoadedState{
		Path: filepath.Join(dataDir, "agent-state.json"),
		State: store.State{
			AgentID:           "agent-test",
			CreatedAt:         time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			SessionSigningKey: signingKey,
			AdminToken:        "test-admin-token",
			RelayKeyID:        "relay-key",
			RelayPrivateKey:   base64.RawStdEncoding.EncodeToString(relayPrivateKey),
			RelayPublicKey:    base64.RawStdEncoding.EncodeToString(relayPublicKey),
		},
	}, time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return runtime
}
