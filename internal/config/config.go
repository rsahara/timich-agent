package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	// Embed IANA timezone data so validation works in minimal container images.
	_ "time/tzdata"

	"github.com/rsahara/timich-agent/internal/atomicfile"
)

const (
	// DefaultConfigPath is the repo-local config path used for local agent development.
	DefaultConfigPath = ".local/agent.json"
	defaultDataDir    = ".local/state"
	// DefaultRemoteBrowsingServerURL is the public Timich Reach HTTP endpoint.
	DefaultRemoteBrowsingServerURL = "https://timich.runo.jp"
	// DefaultAppLinkBaseURL is the production Timich Universal Link origin.
	DefaultAppLinkBaseURL = "https://link.timich.runo.jp"
	// DefaultRelayConnectionAddress is the production Timich Reach control-plane endpoint.
	DefaultRelayConnectionAddress       = "https://control.timich.runo.jp:18090"
	legacyDefaultRelayConnectionAddress = "https://timich.runo.jp"
	// DefaultDeviceLimit is high enough for typical household device churn while still bounding registry growth.
	DefaultDeviceLimit       = 32
	DatasourceKindImmich     = "immich"
	DatasourceKindStaticDemo = "static_demo"
)

// DatasourceConfig describes one upstream datasource the local agent can manage.
type DatasourceConfig struct {
	SourceKey   string `json:"sourceKey,omitempty"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	URL         string `json:"url"`
	AccessToken string `json:"accessToken,omitempty"`
}

// UploadRootConfig describes an administrator-configured writable destination
// root for device media uploads. Paths are runtime/container paths.
type UploadRootConfig struct {
	Key      string `json:"key"`
	Path     string `json:"path"`
	TempPath string `json:"tempPath,omitempty"`
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
	MediaPublishedAddress  string               `json:"mediaPublishedAddress,omitempty"`
	DataDir                string               `json:"dataDir"`
	Timezone               string               `json:"timezone,omitempty"`
	DeviceLimit            int                  `json:"deviceLimit"`
	AppLinkBaseURL         string               `json:"appLinkBaseURL"`
	ControlPlaneAddress    string               `json:"controlPlaneAddress"`
	ControlPlaneServerName string               `json:"controlPlaneServerName,omitempty"`
	Hosted                 RemoteBrowsingConfig `json:"-"`
	Datasources            []DatasourceConfig   `json:"datasources"`
	UploadRoots            []UploadRootConfig   `json:"uploadRoots,omitempty"`
}

type configJSON struct {
	AgentName              string               `json:"agentName"`
	AdminListenAddress     string               `json:"adminListenAddress"`
	MediaListenAddress     string               `json:"mediaListenAddress"`
	MediaPublishedAddress  string               `json:"mediaPublishedAddress,omitempty"`
	DataDir                string               `json:"dataDir"`
	Timezone               string               `json:"timezone,omitempty"`
	DeviceLimit            int                  `json:"deviceLimit"`
	AppLinkBaseURL         string               `json:"appLinkBaseURL"`
	RelayConnectionAddress string               `json:"relayConnectionAddress"`
	ControlPlaneServerName string               `json:"controlPlaneServerName,omitempty"`
	RemoteBrowsing         RemoteBrowsingConfig `json:"remoteBrowsing"`
	Datasources            []DatasourceConfig   `json:"datasources"`
	UploadRoots            []UploadRootConfig   `json:"uploadRoots,omitempty"`
}

type configOverrideJSON struct {
	AgentName              *string             `json:"agentName"`
	AdminListenAddress     *string             `json:"adminListenAddress"`
	MediaListenAddress     *string             `json:"mediaListenAddress"`
	MediaPublishedAddress  *string             `json:"mediaPublishedAddress"`
	DataDir                *string             `json:"dataDir"`
	Timezone               *string             `json:"timezone"`
	DeviceLimit            *int                `json:"deviceLimit"`
	AppLinkBaseURL         *string             `json:"appLinkBaseURL"`
	RelayConnectionAddress *string             `json:"relayConnectionAddress"`
	ControlPlaneAddress    *string             `json:"controlPlaneAddress"`
	ControlPlaneServerName *string             `json:"controlPlaneServerName"`
	RemoteBrowsing         *hostedConfigJSON   `json:"remoteBrowsing"`
	Hosted                 *hostedConfigJSON   `json:"hosted"`
	Datasources            *[]DatasourceConfig `json:"datasources"`
	UploadRoots            *[]UploadRootConfig `json:"uploadRoots"`
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
		MediaPublishedAddress:  c.MediaPublishedAddress,
		DataDir:                c.DataDir,
		Timezone:               c.Timezone,
		DeviceLimit:            c.DeviceLimit,
		AppLinkBaseURL:         c.AppLinkBaseURL,
		RelayConnectionAddress: c.ControlPlaneAddress,
		ControlPlaneServerName: c.ControlPlaneServerName,
		RemoteBrowsing:         c.Hosted,
		Datasources:            c.Datasources,
		UploadRoots:            c.UploadRoots,
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
	if payload.MediaPublishedAddress != nil {
		c.MediaPublishedAddress = *payload.MediaPublishedAddress
	}
	if payload.DataDir != nil {
		c.DataDir = *payload.DataDir
	}
	if payload.Timezone != nil {
		c.Timezone = *payload.Timezone
	}
	if payload.DeviceLimit != nil {
		c.DeviceLimit = *payload.DeviceLimit
	}
	if payload.AppLinkBaseURL != nil {
		c.AppLinkBaseURL = *payload.AppLinkBaseURL
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
	if payload.UploadRoots != nil {
		c.UploadRoots = *payload.UploadRoots
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
		Timezone:               "",
		DeviceLimit:            DefaultDeviceLimit,
		AppLinkBaseURL:         DefaultAppLinkBaseURL,
		ControlPlaneAddress:    DefaultRelayConnectionAddress,
		ControlPlaneServerName: "",
		Hosted: RemoteBrowsingConfig{
			Enabled:   false,
			ServerURL: DefaultRemoteBrowsingServerURL,
		},
		Datasources: []DatasourceConfig{},
		UploadRoots: []UploadRootConfig{},
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
		if changed, err := EnsureDatasourceSourceKeys(&cfg); err != nil {
			return ResolvedConfig{}, err
		} else if changed {
			if err := WriteFile(resolvedPath, cfg); err != nil {
				return ResolvedConfig{}, fmt.Errorf("persist datasource source keys: %w", err)
			}
		}
		source = "file"
		baseDir = filepath.Dir(resolvedPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ResolvedConfig{}, fmt.Errorf("read config file %s: %w", resolvedPath, err)
	}

	relayConnectionEnvProvided := relayConnectionAddressEnvProvided()
	applyEnvOverrides(&cfg)
	upgradeLegacyRelayConnectionAddress(&cfg, relayConnectionEnvProvided)
	cfg.DataDir = resolveDataDir(baseDir, cfg.DataDir)
	normalizeConfig(&cfg)

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
	normalizeConfig(&cfg)

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
	normalizeConfig(&cfg)
	if _, err := EnsureDatasourceSourceKeys(&cfg); err != nil {
		return err
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
		if strings.TrimSpace(datasource.SourceKey) == "" {
			sourceKey, err := GenerateDatasourceSourceKey()
			if err != nil {
				return Config{}, err
			}
			datasource.SourceKey = sourceKey
		}
		cfg.Datasources = []DatasourceConfig{datasource}
	} else {
		if strings.TrimSpace(datasource.SourceKey) == "" {
			datasource.SourceKey = cfg.Datasources[0].SourceKey
		}
		if strings.TrimSpace(datasource.SourceKey) == "" {
			sourceKey, err := GenerateDatasourceSourceKey()
			if err != nil {
				return Config{}, err
			}
			datasource.SourceKey = sourceKey
		}
		if strings.TrimSpace(datasource.AccessToken) == "" {
			datasource.AccessToken = cfg.Datasources[0].AccessToken
		}
		cfg.Datasources[0] = datasource
	}
	upgradeLegacyRelayConnectionAddress(&cfg, false)
	if err := WriteFile(resolvedPath, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnvOverrides(cfg *Config) {
	applyStringEnv("TIMICH_AGENT_NAME", &cfg.AgentName)
	applyStringEnv("TIMICH_AGENT_ADMIN_LISTEN_ADDR", &cfg.AdminListenAddress)
	applyStringEnv("TIMICH_AGENT_MEDIA_LISTEN_ADDR", &cfg.MediaListenAddress)
	applyStringEnv("TIMICH_AGENT_MEDIA_PUBLISHED_ADDR", &cfg.MediaPublishedAddress)
	applyStringEnv("TIMICH_AGENT_DATA_DIR", &cfg.DataDir)
	applyStringEnv("TIMICH_AGENT_TIMEZONE", &cfg.Timezone)
	applyIntEnv("TIMICH_AGENT_DEVICE_LIMIT", &cfg.DeviceLimit)
	applyStringEnv("TIMICH_AGENT_APP_LINK_BASE_URL", &cfg.AppLinkBaseURL)
	applyStringEnv("TIMICH_AGENT_CONTROL_PLANE_ADDR", &cfg.ControlPlaneAddress)
	applyStringEnv("TIMICH_AGENT_RELAY_CONNECTION_ADDR", &cfg.ControlPlaneAddress)
	applyStringEnv("TIMICH_AGENT_CONTROL_PLANE_SERVER_NAME", &cfg.ControlPlaneServerName)
	applyStringEnv("TIMICH_AGENT_HOSTED_SERVER_URL", &cfg.Hosted.ServerURL)
	applyBoolEnv("TIMICH_AGENT_HOSTED_ENABLED", &cfg.Hosted.Enabled)
	applyStringEnv("TIMICH_AGENT_REMOTE_BROWSING_SERVER_URL", &cfg.Hosted.ServerURL)
	applyBoolEnv("TIMICH_AGENT_REMOTE_BROWSING_ENABLED", &cfg.Hosted.Enabled)
}

func normalizeConfig(cfg *Config) {
	cfg.Timezone = strings.TrimSpace(cfg.Timezone)
	for index := range cfg.UploadRoots {
		cfg.UploadRoots[index].Key = strings.TrimSpace(cfg.UploadRoots[index].Key)
		cfg.UploadRoots[index].Path = strings.TrimSpace(cfg.UploadRoots[index].Path)
		cfg.UploadRoots[index].TempPath = normalizeUploadRootTempPath(cfg.UploadRoots[index].TempPath)
	}
}

func relayConnectionAddressEnvProvided() bool {
	return strings.TrimSpace(os.Getenv("TIMICH_AGENT_CONTROL_PLANE_ADDR")) != "" ||
		strings.TrimSpace(os.Getenv("TIMICH_AGENT_RELAY_CONNECTION_ADDR")) != ""
}

func upgradeLegacyRelayConnectionAddress(cfg *Config, relayConnectionEnvProvided bool) {
	if relayConnectionEnvProvided {
		return
	}
	if strings.TrimRight(strings.TrimSpace(cfg.Hosted.ServerURL), "/") != DefaultRemoteBrowsingServerURL {
		return
	}
	if strings.TrimRight(strings.TrimSpace(cfg.ControlPlaneAddress), "/") == legacyDefaultRelayConnectionAddress {
		cfg.ControlPlaneAddress = DefaultRelayConnectionAddress
	}
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
	if timezone := strings.TrimSpace(cfg.Timezone); timezone != "" {
		if _, err := time.LoadLocation(timezone); err != nil {
			return fmt.Errorf("timezone: %w", err)
		}
	}
	if cfg.DeviceLimit < 1 {
		return errors.New("device limit must be at least 1")
	}
	if _, err := parseHTTPSURL(cfg.AppLinkBaseURL); err != nil {
		return fmt.Errorf("app link base URL: %w", err)
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
		if err := ValidateDatasourceSourceKey(datasource.SourceKey); err != nil {
			return fmt.Errorf("datasource %d: source key: %w", index, err)
		}
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

	if err := validateUploadRoots(cfg.UploadRoots); err != nil {
		return err
	}

	return nil
}

func validateUploadRoots(roots []UploadRootConfig) error {
	seen := map[string]struct{}{}
	for index, root := range roots {
		key := strings.TrimSpace(root.Key)
		if err := ValidateUploadRootKey(key); err != nil {
			return fmt.Errorf("upload root %d: key: %w", index, err)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("upload root %d: duplicate key", index)
		}
		seen[key] = struct{}{}
		path := strings.TrimSpace(root.Path)
		if path == "" {
			return fmt.Errorf("upload root %d: path must not be empty", index)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("upload root %d: path must be absolute", index)
		}
		if _, err := ValidateUploadRootTempPath(root.TempPath); err != nil {
			return fmt.Errorf("upload root %d: tempPath: %w", index, err)
		}
	}
	return nil
}

const DefaultUploadRootTempPath = ".timich-upload-tmp"

func normalizeUploadRootTempPath(value string) string {
	cleaned, err := ValidateUploadRootTempPath(value)
	if err != nil {
		return strings.TrimSpace(value)
	}
	return cleaned
}

// ValidateUploadRootTempPath verifies the root-relative temporary directory for
// upload session part files.
func ValidateUploadRootTempPath(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return DefaultUploadRootTempPath, nil
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("must be a relative path inside the upload root")
	}
	return cleaned, nil
}

// EnsureDatasourceSourceKeys assigns stable 64-bit source keys to configured datasources.
func EnsureDatasourceSourceKeys(cfg *Config) (bool, error) {
	if cfg == nil {
		return false, errors.New("config must not be nil")
	}
	changed := false
	seen := map[string]struct{}{}
	for index := range cfg.Datasources {
		key := strings.ToLower(strings.TrimSpace(cfg.Datasources[index].SourceKey))
		if key == "" {
			generated, err := GenerateDatasourceSourceKey()
			if err != nil {
				return false, err
			}
			key = generated
			cfg.Datasources[index].SourceKey = key
			changed = true
		} else if key != cfg.Datasources[index].SourceKey {
			cfg.Datasources[index].SourceKey = key
			changed = true
		}
		if err := ValidateDatasourceSourceKey(key); err != nil {
			return false, fmt.Errorf("datasource %d: %w", index, err)
		}
		if _, ok := seen[key]; ok {
			return false, fmt.Errorf("datasource %d: duplicate source key", index)
		}
		seen[key] = struct{}{}
	}
	return changed, nil
}

// GenerateDatasourceSourceKey returns a non-zero random 64-bit source key.
func GenerateDatasourceSourceKey() (string, error) {
	for {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("generate datasource source key: %w", err)
		}
		if raw == [8]byte{} {
			continue
		}
		return hex.EncodeToString(raw[:]), nil
	}
}

// ValidateDatasourceSourceKey verifies the persisted 64-bit lowercase hex key.
func ValidateDatasourceSourceKey(value string) error {
	key := strings.TrimSpace(value)
	if len(key) != 16 {
		return errors.New("must be 16 lowercase hex characters")
	}
	if key == "0000000000000000" {
		return errors.New("must not be zero")
	}
	for _, char := range key {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return errors.New("must be 16 lowercase hex characters")
		}
	}
	return nil
}

// ValidateUploadRootKey verifies the stable operator-facing upload root key.
func ValidateUploadRootKey(value string) error {
	key := strings.TrimSpace(value)
	if key == "" {
		return errors.New("must not be empty")
	}
	if key == "." || key == ".." {
		return errors.New("must not be a path segment")
	}
	if len(key) > 64 {
		return errors.New("must be 64 characters or shorter")
	}
	for _, char := range key {
		if (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') ||
			char == '-' ||
			char == '_' ||
			char == '.' {
			continue
		}
		return errors.New("must use lowercase letters, digits, '.', '_', or '-'")
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

	normalizeConfig(&resolved.Config)
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

func parseHTTPSURL(raw string) (*url.URL, error) {
	parsed, err := parseURL(raw)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("must use https URL %q", raw)
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
