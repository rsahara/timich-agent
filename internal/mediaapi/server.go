package mediaapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/rsahara/timich-agent/internal/catalog"
	runtimestate "github.com/rsahara/timich-agent/internal/runtime"
	"github.com/rsahara/timich-agent/internal/security"
	"github.com/rsahara/timich-agent/internal/store"
	"github.com/rsahara/timich-agent/internal/webrtcmedia"
)

const maxJSONBodyBytes = 1 << 20

// NewMux returns the local LAN-facing media API scaffold.
func NewMux(runtime *runtimestate.AgentRuntime) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "timich-agent-media",
			"routes": []string{
				"/healthz",
				"/version",
				"/v1/info",
				"/v1/catalog",
				"/v1/pairing/redeem",
				"/v1/session/refresh",
				"/v1/assets/{assetID}/preview",
				"/v1/assets/{assetID}/detail_preview",
				"/v1/assets/{assetID}/original",
				"/v1/webrtc/offer",
			},
		})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"service": "timich-agent-media",
			"status":  "ok",
		})
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		info := runtime.InfoResponse()
		payload := map[string]any{
			"service": "timich-agent-media",
			"version": info.Version,
		}
		if info.Commit != "" {
			payload["commit"] = info.Commit
		}
		if info.BuiltAt != "" {
			payload["builtAt"] = info.BuiltAt
		}
		writeJSON(w, http.StatusOK, payload)
	})
	mux.HandleFunc("/v1/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, runtime.InfoResponse())
	})
	mux.HandleFunc("/v1/pairing/redeem", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, "Use POST to redeem a pairing session.")
			return
		}

		var request struct {
			PairingCode string `json:"pairingCode"`
			DeviceName  string `json:"deviceName"`
		}
		if err := decodeJSONRequest(w, r, &request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid_request",
				"message": "Could not parse the pairing redemption request.",
			})
			return
		}

		sessionBundle, err := runtime.RedeemPairing(
			request.PairingCode,
			request.DeviceName,
			requestBaseURL(r),
		)
		if err != nil {
			writePairingError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, sessionBundle)
	})
	mux.HandleFunc("/v1/session/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, "Use POST to refresh an app session.")
			return
		}

		var request struct {
			RefreshToken string `json:"refreshToken"`
		}
		if err := decodeJSONRequest(w, r, &request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid_request",
				"message": "Could not parse the session refresh request.",
			})
			return
		}

		sessionBundle, err := runtime.RefreshAppSession(
			request.RefreshToken,
			requestBaseURL(r),
		)
		if err != nil {
			writeRefreshError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, sessionBundle)
	})
	mux.HandleFunc("/v1/catalog", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, "Use GET to read the local catalog.")
			return
		}
		if _, ok := authenticateRequest(w, runtime, r); !ok {
			return
		}

		pageIndex := parsePositiveInt(r.URL.Query().Get("page"), 0)
		pageSize := parsePositiveInt(r.URL.Query().Get("size"), 60)
		if pageSize < 1 {
			pageSize = 1
		}
		if pageSize > 200 {
			pageSize = 200
		}

		page, err := runtime.CatalogPage(pageIndex, pageSize)
		if err != nil {
			writeCatalogError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("/v1/webrtc/offer", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, "Use POST to answer a WebRTC media offer.")
			return
		}
		if _, ok := authenticateRequest(w, runtime, r); !ok {
			return
		}

		var request webrtcmedia.OfferRequest
		if err := decodeJSONRequest(w, r, &request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid_request",
				"message": "Could not parse the WebRTC offer request.",
			})
			return
		}

		response, err := runtime.AnswerWebRTCOffer(r.Context(), request)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error":   "webrtc_offer_failed",
				"message": "Could not answer the WebRTC media offer.",
			})
			return
		}
		writeJSON(w, http.StatusOK, response)
	})
	mux.HandleFunc("/v1/assets/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeMethodNotAllowed(w, "Use GET or HEAD to read media content.")
			return
		}
		if _, ok := authenticateRequest(w, runtime, r); !ok {
			return
		}

		assetID, variant, ok := parseAssetRequest(r.URL.Path)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "route_not_found",
				"message": "Unknown media route.",
			})
			return
		}

		var (
			response *catalog.UpstreamMediaResponse
			err      error
		)
		switch variant {
		case "preview":
			response, err = runtime.Preview(r, assetID)
		case "detail_preview":
			response, err = runtime.DetailPreview(r, assetID)
		case "original":
			response, err = runtime.Original(r, assetID)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "route_not_found",
				"message": "Unknown media variant.",
			})
			return
		}
		if err != nil {
			writeCatalogError(w, err)
			return
		}
		defer response.Body.Close()
		copyProxyResponse(w, r.Method, response)
	})
	mux.HandleFunc("/v1/catalog-placeholder", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error":   "catalog_not_implemented",
			"message": "This route is deprecated; use /v1/catalog instead.",
		})
	})
	return mux
}

func decodeJSONRequest(w http.ResponseWriter, r *http.Request, payload any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(payload)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeMethodNotAllowed(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
		"error":   "method_not_allowed",
		"message": message,
	})
}

func authenticateRequest(
	w http.ResponseWriter,
	runtime *runtimestate.AgentRuntime,
	r *http.Request,
) (security.AccessTokenClaims, bool) {
	token := bearerTokenFromHeader(r.Header.Get("Authorization"))
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   "unauthorized",
			"message": "Missing bearer token.",
		})
		return security.AccessTokenClaims{}, false
	}

	claims, err := runtime.AuthenticateAccessToken(token)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   "unauthorized",
			"message": "The app session is not valid for this request.",
		})
		return security.AccessTokenClaims{}, false
	}
	return claims, true
}

func bearerTokenFromHeader(value string) string {
	trimmedValue := strings.TrimSpace(value)
	if !strings.HasPrefix(strings.ToLower(trimmedValue), "bearer ") {
		return ""
	}
	return strings.TrimSpace(trimmedValue[len("Bearer "):])
}

func parsePositiveInt(rawValue string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(rawValue))
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func parseAssetRequest(path string) (assetID string, variant string, ok bool) {
	trimmedPath := strings.TrimPrefix(path, "/v1/assets/")
	parts := strings.Split(trimmedPath, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func requestBaseURL(r *http.Request) string {
	if scheme := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); scheme != "" {
		host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
		if host == "" {
			host = r.Host
		}
		if host != "" {
			return scheme + "://" + host
		}
	}
	if r.URL != nil && strings.TrimSpace(r.URL.Scheme) != "" && strings.TrimSpace(r.URL.Host) != "" {
		return strings.TrimSpace(r.URL.Scheme) + "://" + strings.TrimSpace(r.URL.Host)
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func copyProxyResponse(w http.ResponseWriter, requestMethod string, response *catalog.UpstreamMediaResponse) {
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

func writePairingError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	errorCode := "pairing_redeem_failed"
	message := "Could not redeem the pairing session."
	switch {
	case errors.Is(err, store.ErrPairingSessionNotFound):
		status = http.StatusNotFound
		errorCode = "pairing_not_found"
		message = "The pairing code was not found."
	case errors.Is(err, store.ErrPairingSessionExpired):
		status = http.StatusGone
		errorCode = "pairing_expired"
		message = "The pairing code has expired."
	case errors.Is(err, store.ErrPairingSessionUsed):
		status = http.StatusGone
		errorCode = "pairing_used"
		message = "The pairing code has already been redeemed."
	case errors.Is(err, store.ErrDeviceLimitReached):
		status = http.StatusConflict
		errorCode = "device_limit_reached"
		message = "The local agent has reached its paired-device limit."
	}
	writeJSON(w, status, map[string]string{
		"error":   errorCode,
		"message": message,
	})
}

func writeRefreshError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	errorCode := "session_refresh_failed"
	message := "Could not refresh the app session."
	switch {
	case errors.Is(err, store.ErrRefreshTokenNotFound), errors.Is(err, store.ErrRefreshTokenExpired):
		status = http.StatusUnauthorized
		errorCode = "refresh_token_invalid"
		message = "The refresh token is not valid anymore."
	}
	writeJSON(w, status, map[string]string{
		"error":   errorCode,
		"message": message,
	})
}

func writeCatalogError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	errorCode := "catalog_proxy_failed"
	message := "Could not fetch data from the configured datasource."
	if errors.Is(err, catalog.ErrNoDatasourceConfigured) {
		status = http.StatusServiceUnavailable
		errorCode = "datasource_not_configured"
		message = "No datasource is configured on this agent."
	} else if errors.Is(err, catalog.ErrAssetNotFound) {
		status = http.StatusNotFound
		errorCode = "asset_not_found"
		message = "The requested asset could not be found."
	} else if errors.Is(err, catalog.ErrMediaTooLarge) {
		status = http.StatusRequestEntityTooLarge
		errorCode = "media_too_large"
		message = "The requested media response is too large for remote browsing relay."
	}
	writeJSON(w, status, map[string]string{
		"error":   errorCode,
		"message": message,
	})
}
