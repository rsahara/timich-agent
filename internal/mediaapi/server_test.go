package mediaapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rsahara/timich-agent/internal/catalog"
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

func TestCopyProxyResponseCopiesStreamingHeaders(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	response := &catalog.UpstreamMediaResponse{
		StatusCode: http.StatusPartialContent,
		Header: http.Header{
			"Content-Type":   []string{"video/mp4"},
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
