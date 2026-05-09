package mediaapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rsahara/timich-agent/internal/catalog"
)

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
