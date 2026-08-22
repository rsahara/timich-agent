package mediaapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
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

func TestCopyProxyResponseUsesNoStoreForErrors(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	response := &catalog.UpstreamMediaResponse{
		StatusCode: http.StatusNotFound,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(strings.NewReader(`{"error":"not_found"}`)),
	}

	copyProxyResponse(recorder, http.MethodGet, response)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q, want private, no-store", got)
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

func TestRequestBaseURLUsesRequestHostWithForwardedProto(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodPost, "/v1/session/refresh", nil)
	request.Host = "agent.local:8082"
	request.Header.Set("X-Forwarded-Proto", " https ")

	if got := requestBaseURL(request); got != "https://agent.local:8082" {
		t.Fatalf("requestBaseURL() = %q, want %q", got, "https://agent.local:8082")
	}
}

func TestRequestBaseURLUsesTransportSchemeForPathOnlyRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		useTLS  bool
		wantURL string
	}{
		{name: "HTTP", wantURL: "http://agent.local:8082"},
		{name: "HTTPS", useTLS: true, wantURL: "https://agent.local:8082"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/session/refresh", nil)
			request.Host = "agent.local:8082"
			if test.useTLS {
				request.TLS = &tls.ConnectionState{}
			}

			if got := requestBaseURL(request); got != test.wantURL {
				t.Fatalf("requestBaseURL() = %q, want %q", got, test.wantURL)
			}
		})
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

func TestProtectedRoutesRejectInvalidAuthorization(t *testing.T) {
	t.Parallel()

	runtime := newUploadTestRuntime(t)
	handler := NewMux(runtime)
	routes := []struct {
		name   string
		method string
		path   string
	}{
		{name: "search capabilities", method: http.MethodGet, path: "/v1/assets/search/capabilities"},
		{name: "asset search", method: http.MethodPost, path: "/v1/assets/search"},
		{name: "asset metadata", method: http.MethodGet, path: "/v1/assets/asset-1"},
		{name: "upload policy", method: http.MethodGet, path: "/v1/uploads/me"},
		{name: "create upload", method: http.MethodPost, path: "/v1/uploads/sessions"},
		{name: "upload session", method: http.MethodGet, path: "/v1/uploads/sessions/upload-1"},
		{name: "WebRTC offer", method: http.MethodPost, path: "/v1/webrtc/offer"},
	}
	authorizations := []struct {
		name  string
		value string
	}{
		{name: "missing"},
		{name: "wrong scheme", value: "Basic credentials"},
		{name: "empty bearer", value: "Bearer   "},
		{name: "invalid bearer", value: "Bearer invalid-token"},
	}

	for _, route := range routes {
		for _, authorization := range authorizations {
			t.Run(route.name+"/"+authorization.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(route.method, route.path, nil)
				if authorization.value != "" {
					request.Header.Set("Authorization", authorization.value)
				}

				handler.ServeHTTP(recorder, request)

				assertJSONErrorResponse(t, recorder, http.StatusUnauthorized, "unauthorized")
			})
		}
	}
}

func TestAuthenticateRequestAcceptsValidBearerAndRejectsRevokedDevice(t *testing.T) {
	t.Parallel()

	runtime := newUploadTestRuntime(t)
	session, err := runtime.CreateHostedSession("Authenticated iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}

	validRecorder := httptest.NewRecorder()
	validRequest := httptest.NewRequest(http.MethodGet, "/v1/uploads/me", nil)
	validRequest.Header.Set("Authorization", "  bEaReR   "+session.AccessToken+"  ")
	claims, ok := authenticateRequest(validRecorder, runtime, validRequest)
	if !ok {
		t.Fatalf("authenticateRequest(valid token) rejected request: status=%d body=%s", validRecorder.Code, validRecorder.Body.String())
	}
	if claims.AppDeviceID != session.DeviceID {
		t.Fatalf("claims.AppDeviceID = %q, want %q", claims.AppDeviceID, session.DeviceID)
	}

	if err := runtime.RevokeDevice(session.DeviceID); err != nil {
		t.Fatalf("RevokeDevice() error = %v", err)
	}
	revokedRecorder := httptest.NewRecorder()
	revokedRequest := httptest.NewRequest(http.MethodGet, "/v1/uploads/me", nil)
	revokedRequest.Header.Set("Authorization", "Bearer "+session.AccessToken)
	if _, ok := authenticateRequest(revokedRecorder, runtime, revokedRequest); ok {
		t.Fatal("authenticateRequest(revoked device) accepted request")
	}
	assertJSONErrorResponse(t, revokedRecorder, http.StatusUnauthorized, "unauthorized")
}

func TestMuxRejectsUnsupportedMethods(t *testing.T) {
	t.Parallel()

	runtime := newUploadTestRuntime(t)
	session, err := runtime.CreateHostedSession("Method Test iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	handler := NewMux(runtime)
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "create nearby link", method: http.MethodGet, path: "/v1/nearby-links"},
		{name: "poll nearby link", method: http.MethodGet, path: "/v1/nearby-links/link-1/poll"},
		{name: "cancel nearby link", method: http.MethodGet, path: "/v1/nearby-links/link-1/cancel"},
		{name: "redeem pairing", method: http.MethodGet, path: "/v1/pairing/redeem"},
		{name: "refresh session", method: http.MethodGet, path: "/v1/session/refresh"},
		{name: "search capabilities", method: http.MethodPost, path: "/v1/assets/search/capabilities"},
		{name: "search assets", method: http.MethodGet, path: "/v1/assets/search"},
		{name: "upload policy", method: http.MethodPost, path: "/v1/uploads/me"},
		{name: "create upload", method: http.MethodGet, path: "/v1/uploads/sessions"},
		{name: "read upload", method: http.MethodPost, path: "/v1/uploads/sessions/upload-1"},
		{name: "append upload chunk", method: http.MethodPost, path: "/v1/uploads/sessions/upload-1/chunk"},
		{name: "complete upload", method: http.MethodGet, path: "/v1/uploads/sessions/upload-1/complete"},
		{name: "abort upload", method: http.MethodGet, path: "/v1/uploads/sessions/upload-1/abort"},
		{name: "answer WebRTC offer", method: http.MethodGet, path: "/v1/webrtc/offer"},
		{name: "asset metadata", method: http.MethodPost, path: "/v1/assets/asset-1"},
		{name: "asset media", method: http.MethodPost, path: "/v1/assets/asset-1/preview"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, nil)
			request.Header.Set("Authorization", "Bearer "+session.AccessToken)

			handler.ServeHTTP(recorder, request)

			assertJSONErrorResponse(t, recorder, http.StatusMethodNotAllowed, "method_not_allowed")
		})
	}
}

func TestMuxRejectsMalformedNestedRoutes(t *testing.T) {
	t.Parallel()

	runtime := newUploadTestRuntime(t)
	session, err := runtime.CreateHostedSession("Route Test iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	handler := NewMux(runtime)
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "empty upload ID", method: http.MethodGet, path: "/v1/uploads/sessions/"},
		{name: "unknown upload action", method: http.MethodGet, path: "/v1/uploads/sessions/upload-1/unknown"},
		{name: "deep upload path", method: http.MethodGet, path: "/v1/uploads/sessions/upload-1/chunk/extra"},
		{name: "missing nearby action", method: http.MethodPost, path: "/v1/nearby-links/link-1"},
		{name: "unknown nearby action", method: http.MethodPost, path: "/v1/nearby-links/link-1/unknown"},
		{name: "empty asset ID", method: http.MethodGet, path: "/v1/assets/"},
		{name: "deep asset path", method: http.MethodGet, path: "/v1/assets/asset-1/preview/extra"},
		{name: "unknown media variant", method: http.MethodGet, path: "/v1/assets/asset-1/unknown"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, nil)
			request.Header.Set("Authorization", "Bearer "+session.AccessToken)

			handler.ServeHTTP(recorder, request)

			assertJSONErrorResponse(t, recorder, http.StatusNotFound, "route_not_found")
		})
	}
}

func TestMuxRejectsMalformedJSONRequests(t *testing.T) {
	t.Parallel()

	runtime := newUploadTestRuntime(t)
	session, err := runtime.CreateHostedSession("JSON Test iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	handler := NewMux(runtime)
	tests := []struct {
		name string
		path string
	}{
		{name: "create nearby link", path: "/v1/nearby-links"},
		{name: "poll nearby link", path: "/v1/nearby-links/link-1/poll"},
		{name: "cancel nearby link", path: "/v1/nearby-links/link-1/cancel"},
		{name: "redeem pairing", path: "/v1/pairing/redeem"},
		{name: "refresh session", path: "/v1/session/refresh"},
		{name: "search assets", path: "/v1/assets/search"},
		{name: "create upload", path: "/v1/uploads/sessions"},
		{name: "complete upload", path: "/v1/uploads/sessions/upload-1/complete"},
		{name: "answer WebRTC offer", path: "/v1/webrtc/offer"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader("{"))
			request.Header.Set("Authorization", "Bearer "+session.AccessToken)

			handler.ServeHTTP(recorder, request)

			assertJSONErrorResponse(t, recorder, http.StatusBadRequest, "invalid_request")
		})
	}
}

func TestRouteParserContracts(t *testing.T) {
	t.Parallel()

	t.Run("upload sessions", func(t *testing.T) {
		tests := []struct {
			path       string
			wantID     string
			wantAction string
			wantOK     bool
		}{
			{path: "/v1/uploads/sessions/upload-1", wantID: "upload-1", wantOK: true},
			{path: "/v1/uploads/sessions/upload-1/chunk", wantID: "upload-1", wantAction: "chunk", wantOK: true},
			{path: "/v1/uploads/sessions/upload-1/complete/", wantID: "upload-1", wantAction: "complete", wantOK: true},
			{path: "/v1/uploads/sessions/"},
			{path: "/v1/uploads/sessions/upload-1/chunk/extra"},
		}
		for _, test := range tests {
			gotID, gotAction, gotOK := parseUploadSessionRequest(test.path)
			if gotID != test.wantID || gotAction != test.wantAction || gotOK != test.wantOK {
				t.Errorf("parseUploadSessionRequest(%q) = %q/%q/%t, want %q/%q/%t", test.path, gotID, gotAction, gotOK, test.wantID, test.wantAction, test.wantOK)
			}
		}
	})

	t.Run("nearby links", func(t *testing.T) {
		tests := []struct {
			path       string
			wantID     string
			wantAction string
			wantOK     bool
		}{
			{path: "/v1/nearby-links/link-1/poll", wantID: "link-1", wantAction: "poll", wantOK: true},
			{path: "/v1/nearby-links/link-1/cancel/", wantID: "link-1", wantAction: "cancel", wantOK: true},
			{path: "/v1/nearby-links/"},
			{path: "/v1/nearby-links/link-1"},
			{path: "/v1/nearby-links/link-1/poll/extra"},
		}
		for _, test := range tests {
			gotID, gotAction, gotOK := parseNearbyLinkRequest(test.path)
			if gotID != test.wantID || gotAction != test.wantAction || gotOK != test.wantOK {
				t.Errorf("parseNearbyLinkRequest(%q) = %q/%q/%t, want %q/%q/%t", test.path, gotID, gotAction, gotOK, test.wantID, test.wantAction, test.wantOK)
			}
		}
	})
}

func TestParseRequiredInt64HeaderContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setHeader   bool
		raw         string
		want        int64
		wantError   bool
		wantInvalid bool
	}{
		{name: "missing", wantError: true, wantInvalid: true},
		{name: "blank", setHeader: true, raw: "   ", wantError: true, wantInvalid: true},
		{name: "not a number", setHeader: true, raw: "offset", wantError: true},
		{name: "negative", setHeader: true, raw: "-1", wantError: true, wantInvalid: true},
		{name: "zero", setHeader: true, raw: "0"},
		{name: "trimmed positive", setHeader: true, raw: " 42 ", want: 42},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/v1/uploads/sessions/upload-1/chunk", nil)
			if test.setHeader {
				request.Header.Set("X-Timich-Offset", test.raw)
			}
			got, err := parseRequiredInt64Header(request, "X-Timich-Offset")
			if (err != nil) != test.wantError {
				t.Fatalf("parseRequiredInt64Header() error = %v, wantError %t", err, test.wantError)
			}
			if test.wantInvalid && !errors.Is(err, runtimestate.ErrUploadRequestInvalid) {
				t.Fatalf("parseRequiredInt64Header() error = %v, want %v", err, runtimestate.ErrUploadRequestInvalid)
			}
			if err == nil && got != test.want {
				t.Fatalf("parseRequiredInt64Header() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestDecodeJSONRequestEnforcesBodyContract(t *testing.T) {
	t.Parallel()

	t.Run("valid body", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"value":"ok"}`))
		var payload struct {
			Value string `json:"value"`
		}
		if err := decodeJSONRequest(recorder, request, &payload); err != nil {
			t.Fatalf("decodeJSONRequest(valid) error = %v", err)
		}
		if payload.Value != "ok" {
			t.Fatalf("decoded value = %q, want ok", payload.Value)
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{"))
		var payload map[string]any
		if err := decodeJSONRequest(recorder, request, &payload); err == nil {
			t.Fatal("decodeJSONRequest(malformed) error = nil")
		}
	})

	t.Run("oversized body", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		body := `{"value":"` + strings.Repeat("x", maxJSONBodyBytes) + `"}`
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		var payload map[string]any
		err := decodeJSONRequest(recorder, request, &payload)
		if err == nil || !strings.Contains(err.Error(), "request body too large") {
			t.Fatalf("decodeJSONRequest(oversized) error = %v, want body-too-large error", err)
		}
	})
}

func TestPublicErrorResponseMappings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		write      func(http.ResponseWriter, error)
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "pairing not found", write: writePairingError, err: store.ErrPairingSessionNotFound, wantStatus: http.StatusNotFound, wantCode: "pairing_not_found"},
		{name: "pairing expired", write: writePairingError, err: store.ErrPairingSessionExpired, wantStatus: http.StatusGone, wantCode: "pairing_expired"},
		{name: "pairing used", write: writePairingError, err: store.ErrPairingSessionUsed, wantStatus: http.StatusGone, wantCode: "pairing_used"},
		{name: "pairing device limit", write: writePairingError, err: store.ErrDeviceLimitReached, wantStatus: http.StatusConflict, wantCode: "device_limit_reached"},
		{name: "pairing fallback", write: writePairingError, err: errors.New("pairing failure"), wantStatus: http.StatusInternalServerError, wantCode: "pairing_redeem_failed"},
		{name: "nearby not found", write: writeNearbyLinkError, err: store.ErrNearbyLinkNotFound, wantStatus: http.StatusNotFound, wantCode: "nearby_link_not_found"},
		{name: "nearby denied", write: writeNearbyLinkError, err: store.ErrNearbyLinkDenied, wantStatus: http.StatusGone, wantCode: "nearby_link_denied"},
		{name: "nearby pending", write: writeNearbyLinkError, err: store.ErrNearbyLinkNotApproved, wantStatus: http.StatusConflict, wantCode: "nearby_link_pending"},
		{name: "nearby consumed", write: writeNearbyLinkError, err: store.ErrNearbyLinkConsumed, wantStatus: http.StatusGone, wantCode: "nearby_link_used"},
		{name: "nearby poll token", write: writeNearbyLinkError, err: store.ErrNearbyLinkPollTokenInvalid, wantStatus: http.StatusUnauthorized, wantCode: "nearby_link_poll_token_invalid"},
		{name: "nearby limit", write: writeNearbyLinkError, err: store.ErrNearbyLinkLimitReached, wantStatus: http.StatusTooManyRequests, wantCode: "nearby_link_limit_reached"},
		{name: "nearby device limit", write: writeNearbyLinkError, err: store.ErrDeviceLimitReached, wantStatus: http.StatusConflict, wantCode: "device_limit_reached"},
		{name: "nearby fallback", write: writeNearbyLinkError, err: errors.New("nearby failure"), wantStatus: http.StatusInternalServerError, wantCode: "nearby_link_failed"},
		{name: "refresh token not found", write: writeRefreshError, err: store.ErrRefreshTokenNotFound, wantStatus: http.StatusUnauthorized, wantCode: "refresh_token_invalid"},
		{name: "refresh token expired", write: writeRefreshError, err: store.ErrRefreshTokenExpired, wantStatus: http.StatusUnauthorized, wantCode: "refresh_token_invalid"},
		{name: "refresh fallback", write: writeRefreshError, err: errors.New("refresh failure"), wantStatus: http.StatusInternalServerError, wantCode: "session_refresh_failed"},
		{name: "catalog missing datasource", write: writeCatalogError, err: catalog.ErrNoDatasourceConfigured, wantStatus: http.StatusServiceUnavailable, wantCode: "datasource_not_configured"},
		{name: "catalog invalid search", write: writeCatalogError, err: catalog.ErrInvalidSearchRequest, wantStatus: http.StatusBadRequest, wantCode: "invalid_search_request"},
		{name: "catalog unsupported search", write: writeCatalogError, err: catalog.ErrUnsupportedSearch, wantStatus: http.StatusBadRequest, wantCode: "unsupported_search"},
		{name: "catalog unavailable", write: writeCatalogError, err: catalog.ErrDatasourceUnavailable, wantStatus: http.StatusBadGateway, wantCode: "datasource_unavailable"},
		{name: "catalog asset not found", write: writeCatalogError, err: catalog.ErrAssetNotFound, wantStatus: http.StatusNotFound, wantCode: "asset_not_found"},
		{name: "catalog media too large", write: writeCatalogError, err: catalog.ErrMediaTooLarge, wantStatus: http.StatusRequestEntityTooLarge, wantCode: "media_too_large"},
		{name: "catalog fallback", write: writeCatalogError, err: errors.New("catalog failure"), wantStatus: http.StatusBadGateway, wantCode: "catalog_proxy_failed"},
		{name: "upload invalid request", write: writeUploadError, err: runtimestate.ErrUploadRequestInvalid, wantStatus: http.StatusBadRequest, wantCode: "upload_request_invalid"},
		{name: "upload checksum mismatch", write: writeUploadError, err: runtimestate.ErrUploadChecksumMismatch, wantStatus: http.StatusBadRequest, wantCode: "upload_checksum_mismatch"},
		{name: "upload offset conflict", write: writeUploadError, err: store.ErrUploadSessionOffsetConflict, wantStatus: http.StatusConflict, wantCode: "upload_offset_conflict"},
		{name: "upload asset exists", write: writeUploadError, err: store.ErrUploadedAssetExists, wantStatus: http.StatusConflict, wantCode: "uploaded_asset_exists"},
		{name: "upload final path conflict", write: writeUploadError, err: runtimestate.ErrUploadFinalPathConflict, wantStatus: http.StatusConflict, wantCode: "upload_final_path_conflict"},
		{name: "upload policy blocked", write: writeUploadError, err: runtimestate.ErrUploadPolicyInvalid, wantStatus: http.StatusConflict, wantCode: "upload_policy_blocked"},
		{name: "upload storage blocked", write: writeUploadError, err: runtimestate.ErrStorageWriteBlocked, wantStatus: http.StatusInsufficientStorage, wantCode: "storage_write_blocked"},
		{name: "upload session not found", write: writeUploadError, err: store.ErrUploadSessionNotFound, wantStatus: http.StatusNotFound, wantCode: "upload_session_not_found"},
		{name: "upload device not found", write: writeUploadError, err: store.ErrDeviceNotFound, wantStatus: http.StatusNotFound, wantCode: "device_not_found"},
		{name: "upload fallback", write: writeUploadError, err: errors.New("upload failure"), wantStatus: http.StatusInternalServerError, wantCode: "upload_request_failed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			test.write(recorder, fmt.Errorf("wrapped response error: %w", test.err))
			assertJSONErrorResponse(t, recorder, test.wantStatus, test.wantCode)
		})
	}
}

func assertJSONErrorResponse(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()

	if recorder.Code != wantStatus {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, wantStatus, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload["error"] != wantCode {
		t.Fatalf("error = %q, want %q payload=%v", payload["error"], wantCode, payload)
	}
	if strings.TrimSpace(payload["message"]) == "" {
		t.Fatalf("message is empty payload=%v", payload)
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
