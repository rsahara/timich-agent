package adminapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
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

type pairingSessionAPIResponse struct {
	PairingCode         string                    `json:"pairingCode"`
	ExpiresAt           time.Time                 `json:"expiresAt"`
	AgentBaseURLChoices []pairingAgentBaseURLItem `json:"agentBaseURLChoices,omitempty"`
}

type pairingLinkAPIRequest struct {
	AgentBaseURL string    `json:"agentBaseURL"`
	PairingCode  string    `json:"pairingCode"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

type pairingLinkAPIResponse struct {
	PairingPayload       pairingLinkPayload `json:"pairingPayload"`
	PairingURL           string             `json:"pairingURL"`
	PairingQRCodeDataURL string             `json:"pairingQRCodeDataURL"`
}

type pairingAgentBaseURLItem struct {
	Label       string `json:"label"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
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
) pairingSessionAPIResponse {
	cfg := s.runtime.ConfigResponse()
	return pairingSessionAPIResponse{
		PairingCode:         session.PairingCode,
		ExpiresAt:           session.ExpiresAt,
		AgentBaseURLChoices: pairingAgentBaseURLChoices(r, cfg.MediaListenAddress, cfg.MediaPublishedAddress),
	}
}

func (s *server) buildPairingLinkAPIResponse(request pairingLinkAPIRequest) (pairingLinkAPIResponse, error) {
	cfg := s.runtime.ConfigResponse()
	agentBaseURL, err := normalizePairingAgentBaseURL(request.AgentBaseURL)
	if err != nil {
		return pairingLinkAPIResponse{}, err
	}
	payload := pairingLinkPayload{
		Version:      pairingLinkPayloadVersion,
		Kind:         pairingLinkPayloadKind,
		AgentBaseURL: agentBaseURL,
		PairingCode:  strings.TrimSpace(request.PairingCode),
		ExpiresAt:    request.ExpiresAt,
	}
	pairingURL, err := buildPairingURL(cfg.AppLinkBaseURL, payload)
	if err != nil {
		return pairingLinkAPIResponse{}, err
	}
	qrCodeDataURL, err := pairingQRCodeDataURL(pairingURL)
	if err != nil {
		return pairingLinkAPIResponse{}, err
	}

	return pairingLinkAPIResponse{
		PairingPayload:       payload,
		PairingURL:           pairingURL,
		PairingQRCodeDataURL: qrCodeDataURL,
	}, nil
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

func pairingAgentBaseURLChoices(r *http.Request, mediaListenAddress string, mediaPublishedAddress string) []pairingAgentBaseURLItem {
	mediaHost, mediaPort, _ := net.SplitHostPort(mediaListenAddress)
	if mediaPort == "" {
		return nil
	}
	mediaHost = strings.Trim(mediaHost, "[]")
	publishedHost, publishedPort, publishedReachable := pairingPublishedMediaEndpoint(mediaPublishedAddress, mediaPort)

	choices := []pairingAgentBaseURLItem{}
	seen := map[string]struct{}{}
	add := func(label string, host string, port string, description string) {
		if !isPairingReachableHost(host) {
			return
		}
		candidate := (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}).String()
		if _, ok := seen[candidate]; ok {
			return
		}
		seen[candidate] = struct{}{}
		choices = append(choices, pairingAgentBaseURLItem{
			Label:       label,
			URL:         candidate,
			Description: description,
		})
	}

	if isWildcardHost(mediaHost) && !requestLooksProxied(r) {
		requestHost := hostNameOnly(r.Host)
		if publishedReachable {
			add("Current Admin UI host", requestHost, publishedPort, "Use when the phone or tablet can reach this Admin UI host on the local network.")
		}
	}
	if isPairingReachableHost(mediaHost) && publishedReachable {
		add("Configured media API host", mediaHost, publishedPort, "Use when this media listen host is reachable from the phone or tablet.")
	}
	if publishedHost != "" && publishedReachable {
		add("Published media API host", publishedHost, publishedPort, "Use when this published media address is reachable from the phone or tablet.")
	}

	return choices
}

func pairingPublishedMediaEndpoint(raw string, fallbackPort string) (string, string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fallbackPort, true
	}
	if validPort(value) {
		return "", value, true
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil || !validPort(port) {
		return "", "", false
	}
	host = strings.Trim(host, "[]")
	if isWildcardHost(host) {
		return "", port, true
	}
	if !isPairingReachableHost(host) {
		return "", "", false
	}
	return host, port, true
}

func validPort(value string) bool {
	port, err := strconv.Atoi(strings.TrimSpace(value))
	return err == nil && port > 0 && port <= 65535
}

func normalizePairingAgentBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid agent base URL %q", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("agent base URL must use http or https URL %q", raw)
	}
	if !isPairingReachableHost(hostNameOnly(parsed.Host)) {
		return "", fmt.Errorf("agent base URL host is not reachable from app devices %q", raw)
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
