package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rsahara/timich-agent/internal/atomicfile"
)

const (
	// DefaultConfigPath is the repo-local config path used for local agent development.
	DefaultConfigPath = ".local/agent.json"
	defaultDataDir    = ".local/state"
	// DefaultDeviceLimit is high enough for typical household device churn while still bounding registry growth.
	DefaultDeviceLimit       = 32
	DatasourceKindImmich     = "immich"
	DatasourceKindStaticDemo = "static_demo"
)

// DatasourceConfig describes one upstream datasource the local agent can manage.
type DatasourceConfig struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	URL         string `json:"url"`
	AccessToken string `json:"accessToken,omitempty"`
}

// RemoteBrowsingConfig controls whether the local agent should connect to the relay service.
type RemoteBrowsingConfig struct {
	Enabled   bool   `json:"enabled"`
	ServerURL string `json:"serverURL"`
}

// Config holds the first-pass runtime settings for timich-agent.
type Config struct {
	AgentName              string               `json:"agentName"`
	AdminListenAddress     string               `json:"adminListenAddress"`
	MediaListenAddress     string               `json:"mediaListenAddress"`
	DataDir                string               `json:"dataDir"`
	DeviceLimit            int                  `json:"deviceLimit"`
	ControlPlaneAddress    string               `json:"controlPlaneAddress"`
	ControlPlaneServerName string               `json:"controlPlaneServerName,omitempty"`
	Hosted                 RemoteBrowsingConfig `json:"-"`
	Datasources            []DatasourceConfig   `json:"datasources"`
}

type configJSON struct {
	AgentName              string               `json:"agentName"`
	AdminListenAddress     string               `json:"adminListenAddress"`
	MediaListenAddress     string               `json:"mediaListenAddress"`
	DataDir                string               `json:"dataDir"`
	DeviceLimit            int                  `json:"deviceLimit"`
	RelayConnectionAddress string               `json:"relayConnectionAddress"`
	ControlPlaneServerName string               `json:"controlPlaneServerName,omitempty"`
	RemoteBrowsing         RemoteBrowsingConfig `json:"remoteBrowsing"`
	Datasources            []DatasourceConfig   `json:"datasources"`
}

type configOverrideJSON struct {
	AgentName              *string             `json:"agentName"`
	AdminListenAddress     *string             `json:"adminListenAddress"`
	MediaListenAddress     *string             `json:"mediaListenAddress"`
	DataDir                *string             `json:"dataDir"`
	DeviceLimit            *int                `json:"deviceLimit"`
	RelayConnectionAddress *string             `json:"relayConnectionAddress"`
	ControlPlaneAddress    *string             `json:"controlPlaneAddress"`
	ControlPlaneServerName *string             `json:"controlPlaneServerName"`
	RemoteBrowsing         *hostedConfigJSON   `json:"remoteBrowsing"`
	Hosted                 *hostedConfigJSON   `json:"hosted"`
	Datasources            *[]DatasourceConfig `json:"datasources"`
}

type hostedConfigJSON struct {
	Enabled   *bool   `json:"enabled"`
	ServerURL *string `json:"serverURL"`
}

// MarshalJSON writes the current operator-facing config shape. Older config
// files that use "hosted" are still accepted by UnmarshalJSON.
func (c Config) MarshalJSON() ([]byte, error) {
	return json.Marshal(configJSON{
		AgentName:              c.AgentName,
		AdminListenAddress:     c.AdminListenAddress,
		MediaListenAddress:     c.MediaListenAddress,
		DataDir:                c.DataDir,
		DeviceLimit:            c.DeviceLimit,
		RelayConnectionAddress: c.ControlPlaneAddress,
		ControlPlaneServerName: c.ControlPlaneServerName,
		RemoteBrowsing:         c.Hosted,
		Datasources:            c.Datasources,
	})
}

func (c *Config) UnmarshalJSON(data []byte) error {
	var payload configOverrideJSON
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	if payload.AgentName != nil {
		c.AgentName = *payload.AgentName
	}
	if payload.AdminListenAddress != nil {
		c.AdminListenAddress = *payload.AdminListenAddress
	}
	if payload.MediaListenAddress != nil {
		c.MediaListenAddress = *payload.MediaListenAddress
	}
	if payload.DataDir != nil {
		c.DataDir = *payload.DataDir
	}
	if payload.DeviceLimit != nil {
		c.DeviceLimit = *payload.DeviceLimit
	}
	if payload.ControlPlaneAddress != nil {
		c.ControlPlaneAddress = *payload.ControlPlaneAddress
	}
	if payload.RelayConnectionAddress != nil {
		c.ControlPlaneAddress = *payload.RelayConnectionAddress
	}
	if payload.ControlPlaneServerName != nil {
		c.ControlPlaneServerName = *payload.ControlPlaneServerName
	}
	if payload.Hosted != nil {
		applyHostedConfigJSON(&c.Hosted, *payload.Hosted)
	}
	if payload.RemoteBrowsing != nil {
		applyHostedConfigJSON(&c.Hosted, *payload.RemoteBrowsing)
	}
	if payload.Datasources != nil {
		c.Datasources = *payload.Datasources
	}
	return nil
}

func applyHostedConfigJSON(target *RemoteBrowsingConfig, payload hostedConfigJSON) {
	if payload.Enabled != nil {
		target.Enabled = *payload.Enabled
	}
	if payload.ServerURL != nil {
		target.ServerURL = *payload.ServerURL
	}
}

// ResolvedConfig includes the config source metadata after loading defaults, file data, and env overrides.
type ResolvedConfig struct {
	Config
	ConfigPath   string
	ConfigSource string
}

// Default returns the first-pass local agent configuration.
func Default() Config {
	hostName, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostName) == "" {
		hostName = "timich-agent"
	}

	return Config{
		AgentName:              hostName,
		AdminListenAddress:     "0.0.0.0:8081",
		MediaListenAddress:     "0.0.0.0:8082",
		DataDir:                defaultDataDir,
		DeviceLimit:            DefaultDeviceLimit,
		ControlPlaneAddress:    "https://timich.runo.jp",
		ControlPlaneServerName: "",
		Hosted: RemoteBrowsingConfig{
			Enabled:   false,
			ServerURL: "https://timich.runo.jp",
		},
		Datasources: []DatasourceConfig{},
	}
}

// Load resolves config from defaults, an optional JSON config file, and env overrides.
func Load(configPath string) (ResolvedConfig, error) {
	path := strings.TrimSpace(configPath)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("TIMICH_AGENT_CONFIG_PATH"))
	}
	if path == "" {
		path = DefaultConfigPath
	}

	resolvedPath, err := filepath.Abs(path)
	if err != nil {
		return ResolvedConfig{}, fmt.Errorf("resolve config path: %w", err)
	}

	cfg := Default()
	source := "defaults"
	baseDir, err := os.Getwd()
	if err != nil {
		return ResolvedConfig{}, fmt.Errorf("resolve working directory: %w", err)
	}

	if raw, err := os.ReadFile(resolvedPath); err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return ResolvedConfig{}, fmt.Errorf("parse config file %s: %w", resolvedPath, err)
		}
		source = "file"
		baseDir = filepath.Dir(resolvedPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ResolvedConfig{}, fmt.Errorf("read config file %s: %w", resolvedPath, err)
	}

	applyEnvOverrides(&cfg)
	cfg.DataDir = resolveDataDir(baseDir, cfg.DataDir)

	if err := validate(cfg); err != nil {
		return ResolvedConfig{}, err
	}

	return ResolvedConfig{
		Config:       cfg,
		ConfigPath:   resolvedPath,
		ConfigSource: source,
	}, nil
}

// WriteDefaultFile writes a starter config file if one does not already exist.
func WriteDefaultFile(configPath string, dataDir string) (string, error) {
	path := strings.TrimSpace(configPath)
	if path == "" {
		path = DefaultConfigPath
	}

	resolvedPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	if _, err := os.Stat(resolvedPath); err == nil {
		return "", fmt.Errorf("config file already exists at %s", resolvedPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect config path: %w", err)
	}

	cfg := Default()
	if strings.TrimSpace(dataDir) != "" {
		cfg.DataDir = dataDir
	} else {
		cfg.DataDir = "state"
	}

	if err := os.MkdirAll(filepath.Dir(resolvedPath), 0o755); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	raw = append(raw, '\n')

	if err := atomicfile.WriteFile(resolvedPath, raw, 0o600); err != nil {
		return "", fmt.Errorf("write config file: %w", err)
	}
	return resolvedPath, nil
}

// WriteFile persists a validated config file to path.
func WriteFile(configPath string, cfg Config) error {
	path := strings.TrimSpace(configPath)
	if path == "" {
		return errors.New("config path must not be empty")
	}
	resolvedPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	if err := validate(cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolvedPath), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	raw = append(raw, '\n')
	if err := atomicfile.WriteFile(resolvedPath, raw, 0o600); err != nil {
		return fmt.Errorf("write config file %s: %w", resolvedPath, err)
	}
	return nil
}

// UpdatePrimaryDatasourceFile patches only the file-backed primary datasource.
// Environment and serve-time overrides are intentionally not included in the
// write path so temporary runtime values do not become persistent config.
func UpdatePrimaryDatasourceFile(configPath string, datasource DatasourceConfig) (Config, error) {
	path := strings.TrimSpace(configPath)
	if path == "" {
		return Config{}, errors.New("config path must not be empty")
	}
	resolvedPath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("resolve config path: %w", err)
	}

	cfg := Default()
	if raw, err := os.ReadFile(resolvedPath); err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config file %s: %w", resolvedPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read config file %s: %w", resolvedPath, err)
	}

	if len(cfg.Datasources) == 0 {
		cfg.Datasources = []DatasourceConfig{datasource}
	} else {
		if strings.TrimSpace(datasource.AccessToken) == "" {
			datasource.AccessToken = cfg.Datasources[0].AccessToken
		}
		cfg.Datasources[0] = datasource
	}
	if err := WriteFile(resolvedPath, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnvOverrides(cfg *Config) {
	applyStringEnv("TIMICH_AGENT_NAME", &cfg.AgentName)
	applyStringEnv("TIMICH_AGENT_ADMIN_LISTEN_ADDR", &cfg.AdminListenAddress)
	applyStringEnv("TIMICH_AGENT_MEDIA_LISTEN_ADDR", &cfg.MediaListenAddress)
	applyStringEnv("TIMICH_AGENT_DATA_DIR", &cfg.DataDir)
	applyIntEnv("TIMICH_AGENT_DEVICE_LIMIT", &cfg.DeviceLimit)
	applyStringEnv("TIMICH_AGENT_CONTROL_PLANE_ADDR", &cfg.ControlPlaneAddress)
	applyStringEnv("TIMICH_AGENT_RELAY_CONNECTION_ADDR", &cfg.ControlPlaneAddress)
	applyStringEnv("TIMICH_AGENT_CONTROL_PLANE_SERVER_NAME", &cfg.ControlPlaneServerName)
	applyStringEnv("TIMICH_AGENT_HOSTED_SERVER_URL", &cfg.Hosted.ServerURL)
	applyBoolEnv("TIMICH_AGENT_HOSTED_ENABLED", &cfg.Hosted.Enabled)
	applyStringEnv("TIMICH_AGENT_REMOTE_BROWSING_SERVER_URL", &cfg.Hosted.ServerURL)
	applyBoolEnv("TIMICH_AGENT_REMOTE_BROWSING_ENABLED", &cfg.Hosted.Enabled)
}

func applyStringEnv(key string, target *string) {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		*target = value
	}
}

func applyIntEnv(key string, target *int) {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			*target = parsed
		}
	}
}

func applyBoolEnv(key string, target *bool) {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		*target = truthyEnv(value)
	}
}

func truthyEnv(value string) bool {
	return strings.EqualFold(value, "1") ||
		strings.EqualFold(value, "true") ||
		strings.EqualFold(value, "yes")
}

func validate(cfg Config) error {
	if strings.TrimSpace(cfg.AgentName) == "" {
		return errors.New("agent name must not be empty")
	}
	if strings.TrimSpace(cfg.AdminListenAddress) == "" {
		return errors.New("admin listen address must not be empty")
	}
	if strings.TrimSpace(cfg.MediaListenAddress) == "" {
		return errors.New("media listen address must not be empty")
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		return errors.New("data directory must not be empty")
	}
	if cfg.DeviceLimit < 1 {
		return errors.New("device limit must be at least 1")
	}

	if err := validateListenAddress(cfg.AdminListenAddress); err != nil {
		return fmt.Errorf("admin listen address: %w", err)
	}
	if err := validateListenAddress(cfg.MediaListenAddress); err != nil {
		return fmt.Errorf("media listen address: %w", err)
	}

	if _, err := parseURL(cfg.ControlPlaneAddress); err != nil {
		return fmt.Errorf("relay connection address: %w", err)
	}
	if _, err := parseURL(cfg.Hosted.ServerURL); err != nil {
		return fmt.Errorf("remote browsing relay server URL: %w", err)
	}

	for index, datasource := range cfg.Datasources {
		if strings.TrimSpace(datasource.Name) == "" {
			return fmt.Errorf("datasource %d: name must not be empty", index)
		}
		if strings.TrimSpace(datasource.Kind) == "" {
			return fmt.Errorf("datasource %d: kind must not be empty", index)
		}
		if datasource.Kind != DatasourceKindImmich && datasource.Kind != DatasourceKindStaticDemo {
			return fmt.Errorf("datasource %d: unsupported kind %q", index, datasource.Kind)
		}
		switch datasource.Kind {
		case DatasourceKindStaticDemo:
			if err := validateStaticDemoURL(datasource.URL); err != nil {
				return fmt.Errorf("datasource %d: %w", index, err)
			}
		default:
			if _, err := parseURL(datasource.URL); err != nil {
				return fmt.Errorf("datasource %d: %w", index, err)
			}
		}
	}

	return nil
}

func validateStaticDemoURL(raw string) error {
	value := strings.TrimSpace(raw)
	if value == "" {
		return errors.New("static_demo datasource URL must not be empty")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Scheme == "" {
		return nil
	}
	if parsed.Scheme == "file" && parsed.Path != "" {
		return nil
	}
	return fmt.Errorf("invalid static_demo URL %q", raw)
}

// ApplyRuntimeOverrides reapplies validation after serve-time flag overrides.
func ApplyRuntimeOverrides(
	resolved ResolvedConfig,
	adminListenAddress string,
	mediaListenAddress string,
	dataDir string,
) (ResolvedConfig, error) {
	if adminListenAddress != "" {
		resolved.AdminListenAddress = adminListenAddress
	}
	if mediaListenAddress != "" {
		resolved.MediaListenAddress = mediaListenAddress
	}
	if dataDir != "" {
		if filepath.IsAbs(dataDir) {
			resolved.DataDir = dataDir
		} else {
			cwd, err := os.Getwd()
			if err != nil {
				return ResolvedConfig{}, fmt.Errorf("resolve working directory: %w", err)
			}
			resolved.DataDir = filepath.Join(cwd, dataDir)
		}
	}

	if err := validate(resolved.Config); err != nil {
		return ResolvedConfig{}, err
	}
	return resolved, nil
}

func validateListenAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("must be in host:port form: %w", err)
	}
	if port == "" {
		return errors.New("must include a port")
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return nil
	}
	return errors.New("must use localhost or a literal IP address")
}

func parseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid URL %q", raw)
	}
	return parsed, nil
}

func resolveDataDir(baseDir string, value string) string {
	if value == "" {
		return resolveOptionalPath(baseDir, defaultDataDir)
	}
	return resolveOptionalPath(baseDir, value)
}

func resolveOptionalPath(baseDir string, value string) string {
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(baseDir, value)
}
