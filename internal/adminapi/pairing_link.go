package adminapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/pairing"
	qrcode "github.com/skip2/go-qrcode"
)

const (
	pairingLinkPayloadKind    = "timich.agent.pairing"
	pairingLinkPayloadVersion = 1
	pairingQRCodeSize         = 280
)

var errPairingAgentBaseURLUnavailable = errors.New("pairing agent media API base URL unavailable")

type pairingSessionAPIResponse struct {
	PairingCode          string              `json:"pairingCode"`
	ExpiresAt            time.Time           `json:"expiresAt"`
	PairingPayload       *pairingLinkPayload `json:"pairingPayload,omitempty"`
	PairingURL           string              `json:"pairingURL,omitempty"`
	PairingQRCodeDataURL string              `json:"pairingQRCodeDataURL,omitempty"`
	PairingLinkWarning   *pairingLinkWarning `json:"pairingLinkWarning,omitempty"`
}

type pairingLinkWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type pairingLinkPayload struct {
	Version      int       `json:"version"`
	Kind         string    `json:"kind"`
	AgentBaseURL string    `json:"agentBaseURL"`
	PairingCode  string    `json:"pairingCode"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

func (s *server) buildPairingSessionAPIResponse(
	r *http.Request,
	session pairing.PairingSessionResponse,
) (pairingSessionAPIResponse, error) {
	response := pairingSessionAPIResponse{
		PairingCode: session.PairingCode,
		ExpiresAt:   session.ExpiresAt,
	}

	cfg := s.runtime.ConfigResponse()
	agentBaseURL, err := pairingAgentBaseURL(r, cfg.AdvertisedMediaBaseURL, cfg.MediaListenAddress)
	if err != nil {
		if errors.Is(err, errPairingAgentBaseURLUnavailable) {
			response.PairingLinkWarning = &pairingLinkWarning{
				Code:    "pairing_agent_base_url_unavailable",
				Message: "QR/link pairing is unavailable because the Media API URL is not reachable from the app device. Use the manual code, open the Admin UI with the agent LAN hostname/IP, or set advertisedMediaBaseURL.",
			}
			return response, nil
		}
		return pairingSessionAPIResponse{}, err
	}
	payload := pairingLinkPayload{
		Version:      pairingLinkPayloadVersion,
		Kind:         pairingLinkPayloadKind,
		AgentBaseURL: agentBaseURL,
		PairingCode:  session.PairingCode,
		ExpiresAt:    session.ExpiresAt,
	}
	pairingURL, err := buildPairingURL(cfg.AppLinkBaseURL, payload)
	if err != nil {
		return pairingSessionAPIResponse{}, err
	}
	qrCodeDataURL, err := pairingQRCodeDataURL(pairingURL)
	if err != nil {
		return pairingSessionAPIResponse{}, err
	}

	response.PairingPayload = &payload
	response.PairingURL = pairingURL
	response.PairingQRCodeDataURL = qrCodeDataURL
	return response, nil
}

func buildPairingURL(appLinkBaseURL string, payload pairingLinkPayload) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(appLinkBaseURL))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid app link base URL %q", appLinkBaseURL)
	}

	encodedPayload, err := encodePairingPayload(payload)
	if err != nil {
		return "", err
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	if basePath == "" {
		parsed.Path = "/pair"
	} else {
		parsed.Path = basePath + "/pair"
	}
	query := parsed.Query()
	query.Set("payload", encodedPayload)
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	return parsed.String(), nil
}

func encodePairingPayload(payload pairingLinkPayload) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func pairingQRCodeDataURL(content string) (string, error) {
	png, err := qrcode.Encode(content, qrcode.Medium, pairingQRCodeSize)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

func pairingAgentBaseURL(r *http.Request, advertisedMediaBaseURL string, mediaListenAddress string) (string, error) {
	if strings.TrimSpace(advertisedMediaBaseURL) != "" {
		return normalizeAdvertisedMediaBaseURL(advertisedMediaBaseURL)
	}

	mediaHost, mediaPort, _ := net.SplitHostPort(mediaListenAddress)
	if mediaPort == "" {
		return "", fmt.Errorf("%w: media listen address is missing a port", errPairingAgentBaseURLUnavailable)
	}
	mediaHost = strings.Trim(mediaHost, "[]")

	host := ""
	if isPairingReachableHost(mediaHost) {
		host = mediaHost
	} else if isWildcardHost(mediaHost) && !requestLooksProxied(r) {
		requestHost := hostNameOnly(r.Host)
		if isPairingReachableHost(requestHost) {
			host = requestHost
		}
	}
	if host == "" {
		return "", fmt.Errorf("%w: open the Admin UI with the agent LAN hostname/IP or set advertisedMediaBaseURL", errPairingAgentBaseURLUnavailable)
	}

	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, mediaPort)}).String(), nil
}

func normalizeAdvertisedMediaBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid advertised media base URL %q", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("advertised media base URL must use http or https URL %q", raw)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func hostNameOnly(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return strings.Trim(host, "[]")
	}
	if strings.HasPrefix(value, "[") {
		if end := strings.Index(value, "]"); end > 1 {
			return value[1:end]
		}
	}
	if strings.Count(value, ":") > 1 {
		return strings.Trim(value, "[]")
	}
	if host, _, found := strings.Cut(value, ":"); found {
		return host
	}
	return value
}

func isWildcardHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	return host == "" || host == "0.0.0.0" || host == "::"
}

func isPairingReachableHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" || isWildcardHost(host) || strings.EqualFold(host, "localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback() && !ip.IsUnspecified()
	}
	return true
}

func requestLooksProxied(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.TrimSpace(r.Header.Get("Forwarded")) != "" ||
		strings.TrimSpace(r.Header.Get("X-Forwarded-Host")) != "" ||
		strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")) != ""
}
