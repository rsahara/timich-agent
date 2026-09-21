package mediaapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rsahara/timich-agent/internal/config"
	runtimestate "github.com/rsahara/timich-agent/internal/runtime"
)

func TestUploadCheckRouteAuthenticationLimitsAndResponse(t *testing.T) {
	runtime := newUploadTestRuntime(t, config.UploadRootConfig{Key: "photos", Path: t.TempDir()})
	bundle, err := runtime.CreateHostedSession("Check device", "https://timich.example")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewMux(runtime)
	body := []byte(`{"items":[{"sourceAssetId":"asset","sourceAssetVersion":"version"}]}`)
	for _, test := range []struct {
		method, token string
		body          []byte
		status        int
	}{
		{http.MethodPost, "", body, http.StatusUnauthorized},
		{http.MethodGet, bundle.AccessToken, body, http.StatusMethodNotAllowed},
		{http.MethodPost, bundle.AccessToken, []byte(`{"items":[]}`), http.StatusBadRequest},
		{http.MethodPost, bundle.AccessToken, []byte(`{"items":[{"sourceAssetId":"` + strings.Repeat("x", maxJSONBodyBytes) + `","sourceAssetVersion":"v"}]}`), http.StatusBadRequest},
		{http.MethodPost, bundle.AccessToken, body, http.StatusOK},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authenticatedUploadRequest(test.method, "/v1/uploads/check", bytes.NewReader(test.body), test.token))
		if recorder.Code != test.status {
			t.Fatalf("status = %d, want %d", recorder.Code, test.status)
		}
		if recorder.Code == http.StatusOK {
			var response runtimestate.UploadCheckResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.LedgerEpoch == "" || len(response.Items) != 1 || response.Items[0].State != "needs_start" {
				t.Fatalf("response = %+v", response)
			}
		}
	}
}
