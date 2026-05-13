package compatibility

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/controlplaneprobe"
)

type Status string

const (
	StatusOK      Status = "ok"
	StatusWarning Status = "warning"
	StatusFailed  Status = "failed"
)

type Check struct {
	Name        string         `json:"name"`
	Status      Status         `json:"status"`
	Summary     string         `json:"summary"`
	Remediation string         `json:"remediation,omitempty"`
	Details     map[string]any `json:"details,omitempty"`
}

type Report struct {
	Service   string    `json:"service"`
	AgentID   string    `json:"agentId"`
	CheckedAt time.Time `json:"checkedAt"`
	Status    Status    `json:"status"`
	Checks    []Check   `json:"checks"`
}

// RelayRegistrationState summarizes whether the agent can authenticate to the relay.
type RelayRegistrationState struct {
	CredentialSynced bool
	Ready            bool
	BlockedBy        []string
}

type Service struct {
	version           string
	agentID           string
	relayKeyID        string
	privateKey        string
	relayRegistration RelayRegistrationState
	cfg               config.ResolvedConfig
	catalog           *catalog.Service
	client            *http.Client
}

func NewService(version string, agentID string, relayKeyID string, privateKey string, cfg config.ResolvedConfig, catalogService *catalog.Service, relayRegistration RelayRegistrationState) *Service {
	return &Service{
		version:    strings.TrimSpace(version),
		agentID:    strings.TrimSpace(agentID),
		relayKeyID: strings.TrimSpace(relayKeyID),
		privateKey: strings.TrimSpace(privateKey),
		relayRegistration: RelayRegistrationState{
			CredentialSynced: relayRegistration.CredentialSynced,
			Ready:            relayRegistration.Ready,
			BlockedBy:        append([]string(nil), relayRegistration.BlockedBy...),
		},
		cfg:     cfg,
		catalog: catalogService,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (s *Service) Run(ctx context.Context) Report {
	checks := []Check{
		s.runAgentConfigCheck(),
		s.runDatasourceCheck(),
		s.runRelayServerCheck(ctx),
		s.runRelayConnectionCheck(ctx),
	}

	reportStatus := StatusOK
	for _, check := range checks {
		if check.Status == StatusFailed {
			reportStatus = StatusFailed
			break
		}
		if check.Status == StatusWarning {
			reportStatus = StatusWarning
		}
	}

	return Report{
		Service:   "timich-agent",
		AgentID:   s.agentID,
		CheckedAt: time.Now().UTC(),
		Status:    reportStatus,
		Checks:    checks,
	}
}

func (s *Service) runAgentConfigCheck() Check {
	details := map[string]any{
		"remoteBrowsingEnabled":  s.cfg.Hosted.Enabled,
		"relayServerURL":         s.cfg.Hosted.ServerURL,
		"relayConnectionAddress": s.cfg.ControlPlaneAddress,
		"relayKeyID":             s.relayKeyID,
		"hasRelaySigningKey":     s.relayKeyID != "" && s.privateKey != "",
		"relayCredentialSynced":  s.relayRegistration.CredentialSynced,
		"relayRegistrationReady": s.relayRegistration.Ready,
	}
	if len(s.relayRegistration.BlockedBy) > 0 {
		details["relayRegistrationBlockedBy"] = append([]string(nil), s.relayRegistration.BlockedBy...)
	}

	if !s.cfg.Hosted.Enabled {
		return Check{
			Name:        "agent_config",
			Status:      StatusFailed,
			Summary:     "Remote browsing is disabled in the agent config.",
			Remediation: "Enable remote browsing in the agent config before retrying.",
			Details:     details,
		}
	}
	if strings.TrimSpace(s.cfg.ControlPlaneAddress) == "" {
		return Check{
			Name:        "agent_config",
			Status:      StatusFailed,
			Summary:     "The relay connection address is missing from the agent config.",
			Remediation: "Set relayConnectionAddress to the relay connection URL and retry.",
			Details:     details,
		}
	}
	if s.relayKeyID == "" || s.privateKey == "" {
		return Check{
			Name:        "agent_config",
			Status:      StatusFailed,
			Summary:     "The agent relay signing key is missing.",
			Remediation: "Restart the agent so it can repair its local relay credential state, then rerun the check.",
			Details:     details,
		}
	}
	return Check{
		Name:    "agent_config",
		Status:  StatusOK,
		Summary: "Remote browsing and relay connection settings are present.",
		Details: details,
	}
}

func (s *Service) runDatasourceCheck() Check {
	details := map[string]any{
		"datasourceCount": len(s.cfg.Datasources),
	}

	if !s.catalog.Ready() {
		return Check{
			Name:        "datasource",
			Status:      StatusFailed,
			Summary:     "No datasource is configured on this agent.",
			Remediation: "Add an Immich datasource and API key, then rerun the remote browsing check.",
			Details:     details,
		}
	}

	page, err := s.catalog.CatalogPage(0, 1)
	if err != nil {
		details["error"] = err.Error()
		details["datasourceURL"] = s.cfg.Datasources[0].URL
		return Check{
			Name:        "datasource",
			Status:      StatusFailed,
			Summary:     "The configured datasource could not be queried.",
			Remediation: "Confirm the Immich URL, API key, and local network reachability.",
			Details:     details,
		}
	}

	details["datasourceURL"] = s.cfg.Datasources[0].URL
	details["returnedItems"] = len(page.Items)
	details["reportedTotal"] = page.Total
	return Check{
		Name:    "datasource",
		Status:  StatusOK,
		Summary: "The configured datasource returned metadata successfully.",
		Details: details,
	}
}

func (s *Service) runRelayServerCheck(ctx context.Context) Check {
	serverURL := strings.TrimRight(strings.TrimSpace(s.cfg.Hosted.ServerURL), "/")
	details := map[string]any{
		"relayServerURL": serverURL,
	}
	if serverURL == "" {
		return Check{
			Name:        "relay_server",
			Status:      StatusFailed,
			Summary:     "The relay server URL is missing from the agent config.",
			Remediation: "Set the relay server URL in the agent config and retry.",
			Details:     details,
		}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/version", nil)
	if err != nil {
		details["error"] = err.Error()
		return Check{
			Name:        "relay_server",
			Status:      StatusFailed,
			Summary:     "The relay server version request could not be created.",
			Remediation: "Confirm the configured relay server URL is valid.",
			Details:     details,
		}
	}

	response, err := s.client.Do(request)
	if err != nil {
		details["error"] = err.Error()
		return Check{
			Name:        "relay_server",
			Status:      StatusFailed,
			Summary:     "The relay server public URL could not be reached.",
			Remediation: "Confirm outbound HTTPS reachability and the relay server URL setting.",
			Details:     details,
		}
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		details["statusCode"] = response.StatusCode
		return Check{
			Name:        "relay_server",
			Status:      StatusFailed,
			Summary:     "The relay server responded with a non-success status.",
			Remediation: "Confirm the Timich relay server is deployed and the public hostname routes to it.",
			Details:     details,
		}
	}

	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err == nil {
		if version, ok := payload["version"]; ok {
			details["serverVersion"] = version
		}
	}

	return Check{
		Name:    "relay_server",
		Status:  StatusOK,
		Summary: "The relay server public version endpoint is reachable.",
		Details: details,
	}
}

func (s *Service) runRelayConnectionCheck(ctx context.Context) Check {
	details := map[string]any{
		"relayConnectionAddress": s.cfg.ControlPlaneAddress,
		"relayCredentialSynced":  s.relayRegistration.CredentialSynced,
		"relayRegistrationReady": s.relayRegistration.Ready,
	}
	if len(s.relayRegistration.BlockedBy) > 0 {
		details["relayRegistrationBlockedBy"] = append([]string(nil), s.relayRegistration.BlockedBy...)
	}

	if !s.cfg.Hosted.Enabled {
		return Check{
			Name:        "relay_connection",
			Status:      StatusWarning,
			Summary:     "Remote browsing is disabled, so no relay connection round trip was run.",
			Remediation: "Enable remote browsing in the agent config before checking relay connectivity.",
			Details:     details,
		}
	}
	if strings.TrimSpace(s.cfg.ControlPlaneAddress) == "" {
		return Check{
			Name:        "relay_connection",
			Status:      StatusWarning,
			Summary:     "The relay connection address is missing, so no relay connection round trip was run.",
			Remediation: "Set relayConnectionAddress to the relay connection URL and rerun the check.",
			Details:     details,
		}
	}
	if s.relayKeyID == "" || s.privateKey == "" {
		return Check{
			Name:        "relay_connection",
			Status:      StatusWarning,
			Summary:     "The agent relay signing key is missing, so no relay connection round trip was run.",
			Remediation: "Restart the agent so it can repair its local relay credential state, then rerun the check.",
			Details:     details,
		}
	}
	if !s.relayRegistration.CredentialSynced {
		summary := "The relay credential is not registered yet, so the relay connection round trip has not run yet."
		remediation := "Wait for the agent to register its relay credential, then rerun the check."
		if len(s.relayRegistration.BlockedBy) > 0 {
			summary = "Remote browsing setup is not complete, so the relay connection round trip has not run yet."
			remediation = "Finish setup (" + strings.Join(s.relayRegistration.BlockedBy, ", ") + "), then wait for relay credential registration and rerun the check."
		}
		return Check{
			Name:        "relay_connection",
			Status:      StatusWarning,
			Summary:     summary,
			Remediation: remediation,
			Details:     details,
		}
	}

	keyID := ""
	privateKey := ""
	if shouldSignRelayConnectionProbe(s.cfg.ControlPlaneAddress) {
		keyID = s.relayKeyID
		privateKey = s.privateKey
	}

	ack, err := controlplaneprobe.Probe(ctx, controlplaneprobe.ProbeInput{
		Version:    s.version,
		AgentID:    s.agentID,
		KeyID:      keyID,
		PrivateKey: privateKey,
		Target:     s.cfg.ControlPlaneAddress,
		TLS: controlplaneprobe.TLSConfig{
			ServerName: s.cfg.ControlPlaneServerName,
		},
	})
	if err != nil {
		details["error"] = err.Error()
		return Check{
			Name:        "relay_connection",
			Status:      StatusFailed,
			Summary:     "The relay connection could not complete a hello/ack round trip.",
			Remediation: s.relayConnectionFailureRemediation(),
			Details:     details,
		}
	}

	details["ack"] = ack
	return Check{
		Name:    "relay_connection",
		Status:  StatusOK,
		Summary: "The relay connection accepted the agent hello and returned an acknowledgement.",
		Details: details,
	}
}

func shouldSignRelayConnectionProbe(target string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(target)), "https://")
}

func (s *Service) relayConnectionFailureRemediation() string {
	if strings.TrimRight(strings.TrimSpace(s.cfg.ControlPlaneAddress), "/") == config.DefaultRemoteBrowsingServerURL {
		return "The relay connection target is the public Timich Reach web/API URL. Set relayConnectionAddress or TIMICH_AGENT_RELAY_CONNECTION_ADDR to " + config.DefaultRelayConnectionAddress + "."
	}
	return "Confirm relay TLS, relay credential registration, and outbound reachability to the relay connection host."
}
