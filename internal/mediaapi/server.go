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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeRouteNotFound(w, "Unknown media route.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "timich-agent-media",
			"routes": []string{
				"/healthz",
				"/version",
				"/v1/info",
				"/v1/assets/search",
				"/v1/assets/search/capabilities",
				"/v1/uploads/me",
				"/v1/uploads/sessions",
				"/v1/uploads/sessions/{uploadId}",
				"/v1/uploads/sessions/{uploadId}/chunk",
				"/v1/uploads/sessions/{uploadId}/complete",
				"/v1/uploads/sessions/{uploadId}/abort",
				"/v1/nearby-links",
				"/v1/nearby-links/{linkId}/cancel",
				"/v1/nearby-links/{linkId}/poll",
				"/v1/pairing/redeem",
				"/v1/session/refresh",
				"/v1/assets/{assetID}/preview",
				"/v1/assets/{assetID}/detail_preview",
				"/v1/assets/{assetID}/original",
				"/v1/webrtc/offer",
			},
		})
	})
	mux.HandleFunc("/v1/catalog", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusGone, map[string]string{
			"error":   "catalog_endpoint_removed",
			"message": "GET /v1/catalog has been removed. Use POST /v1/assets/search instead.",
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
	mux.HandleFunc("/v1/nearby-links", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, "Use POST to start a Nearby Link request.")
			return
		}

		var request struct {
			DeviceName string `json:"deviceName"`
			DeviceKind string `json:"deviceKind"`
		}
		if err := decodeJSONRequest(w, r, &request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid_request",
				"message": "Could not parse the Nearby Link request.",
			})
			return
		}

		response, err := runtime.CreateNearbyLink(request.DeviceName, request.DeviceKind)
		if err != nil {
			writeNearbyLinkError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, response)
	})
	mux.HandleFunc("/v1/nearby-links/", func(w http.ResponseWriter, r *http.Request) {
		linkID, action, ok := parseNearbyLinkRequest(r.URL.Path)
		if !ok || (action != "poll" && action != "cancel") {
			writeRouteNotFound(w, "Unknown Nearby Link route.")
			return
		}
		if action == "cancel" {
			if r.Method != http.MethodPost {
				writeMethodNotAllowed(w, "Use POST to cancel a Nearby Link request.")
				return
			}

			var request struct {
				PollToken string `json:"pollToken"`
			}
			if err := decodeJSONRequest(w, r, &request); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error":   "invalid_request",
					"message": "Could not parse the Nearby Link cancel request.",
				})
				return
			}

			response, err := runtime.CancelNearbyLink(linkID, request.PollToken)
			if err != nil {
				writeNearbyLinkError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, response)
			return
		}
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, "Use POST to poll a Nearby Link request.")
			return
		}

		var request struct {
			PollToken string `json:"pollToken"`
		}
		if err := decodeJSONRequest(w, r, &request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid_request",
				"message": "Could not parse the Nearby Link polling request.",
			})
			return
		}

		response, err := runtime.PollNearbyLink(linkID, request.PollToken, requestBaseURL(r))
		if err != nil {
			writeNearbyLinkError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
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
	mux.HandleFunc("/v1/assets/search/capabilities", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, "Use GET to read search capabilities.")
			return
		}
		if _, ok := authenticateRequest(w, runtime, r); !ok {
			return
		}
		writeJSON(w, http.StatusOK, runtime.SearchCapabilities())
	})
	mux.HandleFunc("/v1/assets/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, "Use POST to search assets.")
			return
		}
		if _, ok := authenticateRequest(w, runtime, r); !ok {
			return
		}

		var request catalog.AssetSearchRequest
		if err := decodeJSONRequest(w, r, &request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid_request",
				"message": "Could not parse the asset search request.",
			})
			return
		}

		page, err := runtime.SearchAssets(request)
		if err != nil {
			writeCatalogError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("/v1/uploads/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, "Use GET to read upload policy and status.")
			return
		}
		claims, ok := authenticateRequest(w, runtime, r)
		if !ok {
			return
		}
		response, err := runtime.AppUploadState(claims.AppDeviceID)
		if err != nil {
			writeUploadError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
	})
	mux.HandleFunc("/v1/uploads/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, "Use POST to create or resume an upload session.")
			return
		}
		claims, ok := authenticateRequest(w, runtime, r)
		if !ok {
			return
		}
		var request runtimestate.UploadSessionStartInput
		if err := decodeJSONRequest(w, r, &request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid_request",
				"message": "Could not parse the upload session request.",
			})
			return
		}
		response, err := runtime.StartUploadSession(claims.AppDeviceID, request)
		if err != nil {
			writeUploadError(w, err)
			return
		}
		status := http.StatusOK
		if response.State == "accepted" {
			status = http.StatusCreated
		}
		writeJSON(w, status, response)
	})
	mux.HandleFunc("/v1/uploads/sessions/", func(w http.ResponseWriter, r *http.Request) {
		claims, ok := authenticateRequest(w, runtime, r)
		if !ok {
			return
		}
		uploadID, action, ok := parseUploadSessionRequest(r.URL.Path)
		if !ok {
			writeRouteNotFound(w, "Unknown upload session route.")
			return
		}
		switch action {
		case "":
			if r.Method != http.MethodGet {
				writeMethodNotAllowed(w, "Use GET to read an upload session.")
				return
			}
			response, err := runtime.UploadSession(claims.AppDeviceID, uploadID)
			if err != nil {
				writeUploadError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, response)
		case "chunk":
			if r.Method != http.MethodPut {
				writeMethodNotAllowed(w, "Use PUT to append an upload chunk.")
				return
			}
			offset, err := parseRequiredInt64Header(r, "X-Timich-Offset")
			if err != nil {
				writeUploadError(w, runtimestate.ErrUploadRequestInvalid)
				return
			}
			response, err := runtime.AppendUploadChunk(claims.AppDeviceID, uploadID, runtimestate.UploadChunkInput{
				Offset:             offset,
				ChunkSHA1Hex:       r.Header.Get("X-Timich-Chunk-SHA1"),
				Body:               r.Body,
				ContentLengthBytes: r.ContentLength,
			})
			if err != nil {
				writeUploadError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, response)
		case "complete":
			if r.Method != http.MethodPost {
				writeMethodNotAllowed(w, "Use POST to complete an upload session.")
				return
			}
			var request runtimestate.UploadSessionCompleteInput
			if err := decodeJSONRequest(w, r, &request); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error":   "invalid_request",
					"message": "Could not parse the upload completion request.",
				})
				return
			}
			response, err := runtime.CompleteUploadSession(claims.AppDeviceID, uploadID, request)
			if err != nil {
				writeUploadError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, response)
		case "abort":
			if r.Method != http.MethodPost {
				writeMethodNotAllowed(w, "Use POST to abort an upload session.")
				return
			}
			response, err := runtime.AbortUploadSession(claims.AppDeviceID, uploadID)
			if err != nil {
				writeUploadError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, response)
		default:
			writeRouteNotFound(w, "Unknown upload session route.")
			return
		}
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
			writeRouteNotFound(w, "Unknown media route.")
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
	return mux
}

func writeRouteNotFound(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusNotFound, map[string]string{
		"error":   "route_not_found",
		"message": message,
	})
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

func parseAssetRequest(path string) (assetID string, variant string, ok bool) {
	trimmedPath := strings.TrimPrefix(path, "/v1/assets/")
	parts := strings.Split(trimmedPath, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func parseUploadSessionRequest(path string) (uploadID string, action string, ok bool) {
	trimmedPath := strings.Trim(strings.TrimPrefix(path, "/v1/uploads/sessions/"), "/")
	if trimmedPath == "" {
		return "", "", false
	}
	parts := strings.Split(trimmedPath, "/")
	if len(parts) == 1 && parts[0] != "" {
		return parts[0], "", true
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1], true
	}
	return "", "", false
}

func parseNearbyLinkRequest(path string) (linkID string, action string, ok bool) {
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

func parseRequiredInt64Header(r *http.Request, name string) (int64, error) {
	rawValue := strings.TrimSpace(r.Header.Get(name))
	if rawValue == "" {
		return 0, runtimestate.ErrUploadRequestInvalid
	}
	value, err := strconv.ParseInt(rawValue, 10, 64)
	if err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, runtimestate.ErrUploadRequestInvalid
	}
	return value, nil
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

func writeNearbyLinkError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	errorCode := "nearby_link_failed"
	message := "Could not complete the Nearby Link request."
	switch {
	case errors.Is(err, store.ErrNearbyLinkNotFound):
		status = http.StatusNotFound
		errorCode = "nearby_link_not_found"
		message = "The Nearby Link request was not found."
	case errors.Is(err, store.ErrNearbyLinkDenied):
		status = http.StatusGone
		errorCode = "nearby_link_denied"
		message = "The Nearby Link request was denied."
	case errors.Is(err, store.ErrNearbyLinkNotApproved):
		status = http.StatusConflict
		errorCode = "nearby_link_pending"
		message = "The Nearby Link request is still waiting for approval."
	case errors.Is(err, store.ErrNearbyLinkConsumed):
		status = http.StatusGone
		errorCode = "nearby_link_used"
		message = "The Nearby Link request has already been used."
	case errors.Is(err, store.ErrNearbyLinkPollTokenInvalid):
		status = http.StatusUnauthorized
		errorCode = "nearby_link_poll_token_invalid"
		message = "The Nearby Link polling token is not valid."
	case errors.Is(err, store.ErrNearbyLinkLimitReached):
		status = http.StatusTooManyRequests
		errorCode = "nearby_link_limit_reached"
		message = "Too many Nearby Link requests are active. Try again shortly."
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
	} else if errors.Is(err, catalog.ErrInvalidSearchRequest) {
		status = http.StatusBadRequest
		errorCode = "invalid_search_request"
		message = "The asset search request is not valid."
	} else if errors.Is(err, catalog.ErrUnsupportedSearch) {
		status = http.StatusBadRequest
		errorCode = "unsupported_search"
		message = "The requested asset search is not supported by this datasource."
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

func writeUploadError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	errorCode := "upload_request_failed"
	message := "Could not process the upload request."
	switch {
	case errors.Is(err, runtimestate.ErrUploadRequestInvalid):
		status = http.StatusBadRequest
		errorCode = "upload_request_invalid"
		message = "The upload request is invalid."
	case errors.Is(err, runtimestate.ErrUploadChecksumMismatch):
		status = http.StatusBadRequest
		errorCode = "upload_checksum_mismatch"
		message = "The upload checksum did not match the received bytes."
	case errors.Is(err, store.ErrUploadSessionOffsetConflict):
		status = http.StatusConflict
		errorCode = "upload_offset_conflict"
		message = "The upload chunk offset does not match the current session offset."
	case errors.Is(err, store.ErrUploadedAssetExists):
		status = http.StatusConflict
		errorCode = "uploaded_asset_exists"
		message = "The source asset version is already reserved by another upload."
	case errors.Is(err, runtimestate.ErrUploadFinalPathConflict):
		status = http.StatusConflict
		errorCode = "upload_final_path_conflict"
		message = "The upload destination path could not be reserved."
	case errors.Is(err, runtimestate.ErrUploadPolicyInvalid):
		status = http.StatusConflict
		errorCode = "upload_policy_blocked"
		message = "The upload policy no longer allows this session to continue."
	case errors.Is(err, store.ErrUploadSessionNotFound):
		status = http.StatusNotFound
		errorCode = "upload_session_not_found"
		message = "The upload session was not found."
	case errors.Is(err, store.ErrDeviceNotFound):
		status = http.StatusNotFound
		errorCode = "device_not_found"
		message = "The paired device was not found."
	}
	writeJSON(w, status, map[string]string{
		"error":   errorCode,
		"message": message,
	})
}
