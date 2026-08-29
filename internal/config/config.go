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
	DefaultDeviceLimit          = 32
	DatasourceKindImmich        = "immich"
	DatasourceKindImmichIndexed = "immich_indexed"
	DatasourceKindLocalFiles    = "local_filesystem"
	DatasourceKindStaticDemo    = "static_demo"
)

var ErrImmichPassthroughRequiresSingleDatasource = errors.New("immich passthrough requires exactly one datasource")

// IsImmichDatasourceKind reports whether kind uses the Immich HTTP connector.
func IsImmichDatasourceKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case DatasourceKindImmich, DatasourceKindImmichIndexed:
		return true
	default:
		return false
	}
}

// IsIndexedDatasourceKind reports whether kind contributes assets to the
// Agent-owned unified catalog.
func IsIndexedDatasourceKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case DatasourceKindImmichIndexed, DatasourceKindLocalFiles:
		return true
	default:
		return false
	}
}

// IsImmichPassthroughDatasourceKind reports whether Immich remains the
// query-time catalog authority for the datasource.
func IsImmichPassthroughDatasourceKind(kind string) bool {
	return strings.TrimSpace(kind) == DatasourceKindImmich
}

var currentExecutablePath = os.Executable

// DatasourceConfig describes one upstream datasource the local agent can manage.
type DatasourceConfig struct {
	SourceKey   string                     `json:"sourceKey,omitempty"`
	Name        string                     `json:"name"`
	Kind        string                     `json:"kind"`
	URL         string                     `json:"url,omitempty"`
	AccessToken string                     `json:"accessToken,omitempty"`
	RootKey     string                     `json:"rootKey,omitempty"`
	Indexing    *DatasourceIndexingConfig  `json:"indexing,omitempty"`
	Scan        *LocalDatasourceScanConfig `json:"scan,omitempty"`
}

// DatasourceIndexingConfig controls optional Immich ingestion tuning for
// immich_indexed datasources. Omitting this object uses runtime defaults.
type DatasourceIndexingConfig struct {
	Phase0SyncInterval   string `json:"phase0SyncInterval,omitempty"`
	DailyFullSweepWindow string `json:"dailyFullSweepWindow,omitempty"`
	LatestAssetLimit     int    `json:"latestAssetLimit,omitempty"`
	MetadataDetailLimit  int    `json:"metadataDetailLimit,omitempty"`
}

// LocalMediaRootConfig describes an administrator-configured read-only media root.
type LocalMediaRootConfig struct {
	Key  string `json:"key"`
	Path string `json:"path"`
}

// LocalDatasourceScanConfig controls local filesystem scan scheduling and behavior.
type LocalDatasourceScanConfig struct {
	FirstViewThumbnailCount       int                                           `json:"firstViewThumbnailCount,omitempty"`
	ImmichFallbackEnabled         *bool                                         `json:"immichFallbackEnabled,omitempty"`
	ImmichExternalLibraryMappings []LocalDatasourceImmichExternalLibraryMapping `json:"immichExternalLibraryMappings,omitempty"`
	QuickScanInterval             string                                        `json:"quickScanInterval,omitempty"`
	ReconciliationTime            string                                        `json:"reconciliationTime,omitempty"`
	ContentVerificationTime       string                                        `json:"contentVerificationTime,omitempty"`
	// ContentVerificationDuration accepts zero as an explicit disabled state.
	ContentVerificationDuration string `json:"contentVerificationDuration,omitempty"`
	SettlingDuration            string `json:"settlingDuration,omitempty"`
	IncludeHiddenDirs           bool   `json:"includeHiddenDirectories,omitempty"`
}

// LocalDatasourceImmichExternalLibraryMapping declares that one Immich
// external-library path prefix and one Local datasource root expose the same
// underlying files. The explicit relationship permits exact canonical
// identity reuse without filename, timestamp, or visual-similarity guessing.
type LocalDatasourceImmichExternalLibraryMapping struct {
	SourceKey          string `json:"sourceKey"`
	OriginalPathPrefix string `json:"originalPathPrefix"`
}

// LocalDatasourceImmichFallbackEnabled preserves the historical enabled
// behavior when the setting is omitted from an existing configuration.
func LocalDatasourceImmichFallbackEnabled(datasource DatasourceConfig) bool {
	if datasource.Kind != DatasourceKindLocalFiles || datasource.Scan == nil || datasource.Scan.ImmichFallbackEnabled == nil {
		return datasource.Kind == DatasourceKindLocalFiles
	}
	return *datasource.Scan.ImmichFallbackEnabled
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

// SemanticRuntimeConfig controls optional local semantic model execution helpers.
type SemanticRuntimeConfig struct {
	HelperPath  string                    `json:"helperPath,omitempty"`
	ONNXRuntime SemanticONNXRuntimeConfig `json:"onnxRuntime,omitempty"`
	Indexing    SemanticIndexingConfig    `json:"indexing,omitempty"`
}

type semanticRuntimeConfigJSON struct {
	HelperPath  string                    `json:"helperPath,omitempty"`
	ONNXRuntime SemanticONNXRuntimeConfig `json:"onnxRuntime,omitempty"`
	Indexing    *SemanticIndexingConfig   `json:"indexing,omitempty"`
}

func (c SemanticRuntimeConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(semanticRuntimeConfigJSON{
		HelperPath:  c.HelperPath,
		ONNXRuntime: c.ONNXRuntime,
		Indexing:    &c.Indexing,
	})
}

func (c *SemanticRuntimeConfig) UnmarshalJSON(data []byte) error {
	var payload semanticRuntimeConfigJSON
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	c.HelperPath = payload.HelperPath
	c.ONNXRuntime = payload.ONNXRuntime
	if payload.Indexing != nil {
		c.Indexing = *payload.Indexing
	}
	return nil
}

// SemanticONNXRuntimeConfig controls the Agent-managed long-lived ONNX runtime
// server used by helper-backed SigLIP 2 model packs.
type SemanticONNXRuntimeConfig struct {
	Disabled           bool   `json:"disabled,omitempty"`
	ServerPath         string `json:"serverPath,omitempty"`
	PythonPath         string `json:"pythonPath,omitempty"`
	Host               string `json:"host,omitempty"`
	Port               int    `json:"port,omitempty"`
	Provider           string `json:"provider,omitempty"`
	TextProvider       string `json:"textProvider,omitempty"`
	ImageProvider      string `json:"imageProvider,omitempty"`
	TextTemplate       string `json:"textTemplate,omitempty"`
	ServerAutoDetected bool   `json:"-"`
	PythonAutoDetected bool   `json:"-"`
}

// SemanticIndexingConfig controls optional low-priority semantic vector
// generation and search-index publication.
type SemanticIndexingConfig struct {
	Enabled                bool   `json:"enabled"`
	Interval               string `json:"interval,omitempty"`
	BatchSize              int    `json:"batchSize,omitempty"`
	TargetCompletedVectors int    `json:"targetCompletedVectors,omitempty"`
}

// MediaRuntimeConfig controls optional local media processing helpers.
type MediaRuntimeConfig struct {
	HelperPath string `json:"helperPath,omitempty"`
	VipsPath   string `json:"vipsPath,omitempty"`
	FFmpegPath string `json:"ffmpegPath,omitempty"`
}

// WorkerRuntimeConfig controls shared background work capacity. A nil
// HeavyTaskWorkers value means the automatic max(1, CPU count / 2) default;
// zero pauses heavyweight background work.
type WorkerRuntimeConfig struct {
	HeavyTaskWorkers *int `json:"heavyTaskWorkers,omitempty"`
}

// Config holds the first-pass runtime settings for timich-agent.
type Config struct {
	AgentName              string                 `json:"agentName"`
	AdminListenAddress     string                 `json:"adminListenAddress"`
	MediaListenAddress     string                 `json:"mediaListenAddress"`
	MediaPublishedAddress  string                 `json:"mediaPublishedAddress,omitempty"`
	DataDir                string                 `json:"dataDir"`
	Timezone               string                 `json:"timezone,omitempty"`
	DeviceLimit            int                    `json:"deviceLimit"`
	AppLinkBaseURL         string                 `json:"appLinkBaseURL"`
	ControlPlaneAddress    string                 `json:"controlPlaneAddress"`
	ControlPlaneServerName string                 `json:"controlPlaneServerName,omitempty"`
	Hosted                 RemoteBrowsingConfig   `json:"-"`
	SemanticRuntime        SemanticRuntimeConfig  `json:"semanticRuntime,omitempty"`
	MediaRuntime           MediaRuntimeConfig     `json:"mediaRuntime,omitempty"`
	WorkerRuntime          WorkerRuntimeConfig    `json:"workerRuntime,omitempty"`
	Datasources            []DatasourceConfig     `json:"datasources"`
	LocalMediaRoots        []LocalMediaRootConfig `json:"localMediaRoots,omitempty"`
	UploadRoots            []UploadRootConfig     `json:"uploadRoots,omitempty"`
}

type configJSON struct {
	AgentName              string                 `json:"agentName"`
	AdminListenAddress     string                 `json:"adminListenAddress"`
	MediaListenAddress     string                 `json:"mediaListenAddress"`
	MediaPublishedAddress  string                 `json:"mediaPublishedAddress,omitempty"`
	DataDir                string                 `json:"dataDir"`
	Timezone               string                 `json:"timezone,omitempty"`
	DeviceLimit            int                    `json:"deviceLimit"`
	AppLinkBaseURL         string                 `json:"appLinkBaseURL"`
	RelayConnectionAddress string                 `json:"relayConnectionAddress"`
	ControlPlaneServerName string                 `json:"controlPlaneServerName,omitempty"`
	RemoteBrowsing         RemoteBrowsingConfig   `json:"remoteBrowsing"`
	SemanticRuntime        SemanticRuntimeConfig  `json:"semanticRuntime,omitempty"`
	MediaRuntime           MediaRuntimeConfig     `json:"mediaRuntime,omitempty"`
	WorkerRuntime          WorkerRuntimeConfig    `json:"workerRuntime,omitempty"`
	Datasources            []DatasourceConfig     `json:"datasources"`
	LocalMediaRoots        []LocalMediaRootConfig `json:"localMediaRoots,omitempty"`
	UploadRoots            []UploadRootConfig     `json:"uploadRoots,omitempty"`
}

type configOverrideJSON struct {
	AgentName              *string                 `json:"agentName"`
	AdminListenAddress     *string                 `json:"adminListenAddress"`
	MediaListenAddress     *string                 `json:"mediaListenAddress"`
	MediaPublishedAddress  *string                 `json:"mediaPublishedAddress"`
	DataDir                *string                 `json:"dataDir"`
	Timezone               *string                 `json:"timezone"`
	DeviceLimit            *int                    `json:"deviceLimit"`
	AppLinkBaseURL         *string                 `json:"appLinkBaseURL"`
	RelayConnectionAddress *string                 `json:"relayConnectionAddress"`
	ControlPlaneAddress    *string                 `json:"controlPlaneAddress"`
	ControlPlaneServerName *string                 `json:"controlPlaneServerName"`
	RemoteBrowsing         *hostedConfigJSON       `json:"remoteBrowsing"`
	Hosted                 *hostedConfigJSON       `json:"hosted"`
	SemanticRuntime        *SemanticRuntimeConfig  `json:"semanticRuntime"`
	MediaRuntime           *MediaRuntimeConfig     `json:"mediaRuntime"`
	WorkerRuntime          *WorkerRuntimeConfig    `json:"workerRuntime"`
	Datasources            *[]DatasourceConfig     `json:"datasources"`
	LocalMediaRoots        *[]LocalMediaRootConfig `json:"localMediaRoots"`
	UploadRoots            *[]UploadRootConfig     `json:"uploadRoots"`
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
		SemanticRuntime:        c.SemanticRuntime,
		MediaRuntime:           c.MediaRuntime,
		WorkerRuntime:          c.WorkerRuntime,
		Datasources:            c.Datasources,
		LocalMediaRoots:        c.LocalMediaRoots,
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
	if payload.SemanticRuntime != nil {
		c.SemanticRuntime = *payload.SemanticRuntime
	}
	if payload.MediaRuntime != nil {
		c.MediaRuntime = *payload.MediaRuntime
	}
	if payload.WorkerRuntime != nil {
		c.WorkerRuntime = *payload.WorkerRuntime
	}
	if payload.Datasources != nil {
		c.Datasources = *payload.Datasources
	}
	if payload.LocalMediaRoots != nil {
		c.LocalMediaRoots = *payload.LocalMediaRoots
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
		SemanticRuntime: SemanticRuntimeConfig{},
		MediaRuntime:    MediaRuntimeConfig{},
		WorkerRuntime:   WorkerRuntimeConfig{},
		Datasources:     []DatasourceConfig{},
		LocalMediaRoots: []LocalMediaRootConfig{},
		UploadRoots:     []UploadRootConfig{},
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
	applyBundledSemanticRuntimeHelper(&cfg)
	applyBundledSemanticONNXRuntime(&cfg)
	applyBundledMediaRuntimeHelper(&cfg)
	applyBundledMediaRuntimeVips(&cfg)
	applyBundledMediaRuntimeFFmpeg(&cfg)

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

// Validate checks a complete in-memory configuration without persisting it.
// Runtime mutations use this before patching the file-backed representation so
// topology rules also cover effective configuration supplied by other sources.
func Validate(cfg Config) error {
	cfg = cloneConfigForValidation(cfg)
	normalizeConfig(&cfg)
	if _, err := EnsureDatasourceSourceKeys(&cfg); err != nil {
		return err
	}
	return validate(cfg)
}

func cloneConfigForValidation(cfg Config) Config {
	if cfg.Datasources != nil {
		cfg.Datasources = append([]DatasourceConfig(nil), cfg.Datasources...)
		for index := range cfg.Datasources {
			if cfg.Datasources[index].Indexing != nil {
				indexing := *cfg.Datasources[index].Indexing
				cfg.Datasources[index].Indexing = &indexing
			}
			if cfg.Datasources[index].Scan != nil {
				scan := *cfg.Datasources[index].Scan
				if scan.ImmichExternalLibraryMappings != nil {
					scan.ImmichExternalLibraryMappings = append([]LocalDatasourceImmichExternalLibraryMapping(nil), scan.ImmichExternalLibraryMappings...)
				}
				if scan.ImmichFallbackEnabled != nil {
					enabled := *scan.ImmichFallbackEnabled
					scan.ImmichFallbackEnabled = &enabled
				}
				cfg.Datasources[index].Scan = &scan
			}
		}
	}
	if cfg.LocalMediaRoots != nil {
		cfg.LocalMediaRoots = append([]LocalMediaRootConfig(nil), cfg.LocalMediaRoots...)
	}
	if cfg.UploadRoots != nil {
		cfg.UploadRoots = append([]UploadRootConfig(nil), cfg.UploadRoots...)
	}
	if cfg.WorkerRuntime.HeavyTaskWorkers != nil {
		workers := *cfg.WorkerRuntime.HeavyTaskWorkers
		cfg.WorkerRuntime.HeavyTaskWorkers = &workers
	}
	return cfg
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
		if datasource.Kind == DatasourceKindImmichIndexed && datasource.Indexing == nil {
			datasource.Indexing = cfg.Datasources[0].Indexing
		}
		if datasource.Kind != DatasourceKindImmichIndexed {
			datasource.Indexing = nil
		}
		cfg.Datasources[0] = datasource
	}
	upgradeLegacyRelayConnectionAddress(&cfg, false)
	if err := WriteFile(resolvedPath, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// AddDatasourceFile appends one datasource to the file-backed configuration.
// Environment and serve-time overrides are intentionally not included in the
// write path so temporary runtime values do not become persistent config.
func AddDatasourceFile(configPath string, datasource DatasourceConfig) (Config, error) {
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

	if strings.TrimSpace(datasource.SourceKey) == "" {
		sourceKey, err := GenerateDatasourceSourceKey()
		if err != nil {
			return Config{}, err
		}
		datasource.SourceKey = sourceKey
	}
	cfg.Datasources = append(cfg.Datasources, datasource)
	upgradeLegacyRelayConnectionAddress(&cfg, false)
	if err := WriteFile(resolvedPath, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// UpdateLocalDatasourceImmichFallbackFile patches one file-backed local
// datasource without persisting environment or serve-time overrides.
func UpdateLocalDatasourceImmichFallbackFile(configPath string, sourceKey string, enabled bool) (Config, error) {
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

	sourceKey = strings.TrimSpace(sourceKey)
	found := false
	for index := range cfg.Datasources {
		datasource := &cfg.Datasources[index]
		if datasource.SourceKey != sourceKey || datasource.Kind != DatasourceKindLocalFiles {
			continue
		}
		scan := LocalDatasourceScanConfig{}
		if datasource.Scan != nil {
			scan = *datasource.Scan
		}
		enabledCopy := enabled
		scan.ImmichFallbackEnabled = &enabledCopy
		datasource.Scan = &scan
		found = true
		break
	}
	if !found {
		return Config{}, fmt.Errorf("local datasource %q is not configured", sourceKey)
	}

	upgradeLegacyRelayConnectionAddress(&cfg, false)
	if err := WriteFile(resolvedPath, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// UpdateSemanticIndexingFile patches only the file-backed semantic indexing
// settings. Environment and serve-time overrides are
// intentionally not included in the write path.
func UpdateSemanticIndexingFile(configPath string, indexing SemanticIndexingConfig) (Config, error) {
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

	cfg.SemanticRuntime.Indexing = indexing
	upgradeLegacyRelayConnectionAddress(&cfg, false)
	if err := WriteFile(resolvedPath, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// UpdateWorkerRuntimeFile patches only the file-backed background worker
// settings without persisting environment or serve-time overrides.
func UpdateWorkerRuntimeFile(configPath string, workers WorkerRuntimeConfig) (Config, error) {
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

	cfg.WorkerRuntime = workers
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
	applyStringEnv("TIMICH_AGENT_SEMANTIC_RUNTIME_HELPER", &cfg.SemanticRuntime.HelperPath)
	applyBoolEnv("TIMICH_AGENT_SEMANTIC_ONNX_DISABLED", &cfg.SemanticRuntime.ONNXRuntime.Disabled)
	applyStringEnv("TIMICH_AGENT_SEMANTIC_ONNX_SERVER_PATH", &cfg.SemanticRuntime.ONNXRuntime.ServerPath)
	applyStringEnv("TIMICH_AGENT_SEMANTIC_ONNX_PYTHON", &cfg.SemanticRuntime.ONNXRuntime.PythonPath)
	applyStringEnv("TIMICH_AGENT_SEMANTIC_ONNX_HOST", &cfg.SemanticRuntime.ONNXRuntime.Host)
	applyIntEnv("TIMICH_AGENT_SEMANTIC_ONNX_PORT", &cfg.SemanticRuntime.ONNXRuntime.Port)
	applyStringEnv("TIMICH_AGENT_SEMANTIC_ONNX_PROVIDER", &cfg.SemanticRuntime.ONNXRuntime.Provider)
	applyStringEnv("TIMICH_AGENT_SEMANTIC_ONNX_TEXT_PROVIDER", &cfg.SemanticRuntime.ONNXRuntime.TextProvider)
	applyStringEnv("TIMICH_AGENT_SEMANTIC_ONNX_IMAGE_PROVIDER", &cfg.SemanticRuntime.ONNXRuntime.ImageProvider)
	applyStringEnv("TIMICH_AGENT_SEMANTIC_ONNX_TEXT_TEMPLATE", &cfg.SemanticRuntime.ONNXRuntime.TextTemplate)
	applyStringEnv("TIMICH_AGENT_MEDIA_HELPER_PATH", &cfg.MediaRuntime.HelperPath)
	applyStringEnv("TIMICH_AGENT_VIPS_PATH", &cfg.MediaRuntime.VipsPath)
	applyStringEnv("TIMICH_AGENT_FFMPEG_PATH", &cfg.MediaRuntime.FFmpegPath)
	applyIntPtrEnv("TIMICH_AGENT_HEAVY_TASK_WORKERS", &cfg.WorkerRuntime.HeavyTaskWorkers)
}

func normalizeConfig(cfg *Config) {
	cfg.Timezone = strings.TrimSpace(cfg.Timezone)
	cfg.SemanticRuntime.HelperPath = strings.TrimSpace(cfg.SemanticRuntime.HelperPath)
	cfg.SemanticRuntime.ONNXRuntime.ServerPath = strings.TrimSpace(cfg.SemanticRuntime.ONNXRuntime.ServerPath)
	cfg.SemanticRuntime.ONNXRuntime.PythonPath = strings.TrimSpace(cfg.SemanticRuntime.ONNXRuntime.PythonPath)
	cfg.SemanticRuntime.ONNXRuntime.Host = strings.TrimSpace(cfg.SemanticRuntime.ONNXRuntime.Host)
	cfg.SemanticRuntime.ONNXRuntime.Provider = strings.TrimSpace(cfg.SemanticRuntime.ONNXRuntime.Provider)
	cfg.SemanticRuntime.ONNXRuntime.TextProvider = strings.TrimSpace(cfg.SemanticRuntime.ONNXRuntime.TextProvider)
	cfg.SemanticRuntime.ONNXRuntime.ImageProvider = strings.TrimSpace(cfg.SemanticRuntime.ONNXRuntime.ImageProvider)
	cfg.SemanticRuntime.ONNXRuntime.TextTemplate = strings.TrimSpace(cfg.SemanticRuntime.ONNXRuntime.TextTemplate)
	cfg.MediaRuntime.HelperPath = strings.TrimSpace(cfg.MediaRuntime.HelperPath)
	cfg.MediaRuntime.VipsPath = strings.TrimSpace(cfg.MediaRuntime.VipsPath)
	cfg.MediaRuntime.FFmpegPath = strings.TrimSpace(cfg.MediaRuntime.FFmpegPath)
	for index := range cfg.LocalMediaRoots {
		cfg.LocalMediaRoots[index].Key = strings.TrimSpace(cfg.LocalMediaRoots[index].Key)
		cfg.LocalMediaRoots[index].Path = strings.TrimSpace(cfg.LocalMediaRoots[index].Path)
	}
	for index := range cfg.Datasources {
		cfg.Datasources[index].RootKey = strings.TrimSpace(cfg.Datasources[index].RootKey)
		if cfg.Datasources[index].Indexing != nil {
			cfg.Datasources[index].Indexing.Phase0SyncInterval = strings.TrimSpace(cfg.Datasources[index].Indexing.Phase0SyncInterval)
			cfg.Datasources[index].Indexing.DailyFullSweepWindow = strings.TrimSpace(cfg.Datasources[index].Indexing.DailyFullSweepWindow)
		}
		if cfg.Datasources[index].Scan != nil {
			for mappingIndex := range cfg.Datasources[index].Scan.ImmichExternalLibraryMappings {
				mapping := &cfg.Datasources[index].Scan.ImmichExternalLibraryMappings[mappingIndex]
				mapping.SourceKey = strings.TrimSpace(mapping.SourceKey)
				mapping.OriginalPathPrefix = normalizeImmichExternalOriginalPathPrefix(mapping.OriginalPathPrefix)
			}
			cfg.Datasources[index].Scan.QuickScanInterval = strings.TrimSpace(cfg.Datasources[index].Scan.QuickScanInterval)
			cfg.Datasources[index].Scan.ReconciliationTime = strings.TrimSpace(cfg.Datasources[index].Scan.ReconciliationTime)
			cfg.Datasources[index].Scan.ContentVerificationTime = strings.TrimSpace(cfg.Datasources[index].Scan.ContentVerificationTime)
			cfg.Datasources[index].Scan.ContentVerificationDuration = strings.TrimSpace(cfg.Datasources[index].Scan.ContentVerificationDuration)
			cfg.Datasources[index].Scan.SettlingDuration = strings.TrimSpace(cfg.Datasources[index].Scan.SettlingDuration)
		}
	}
	for index := range cfg.UploadRoots {
		cfg.UploadRoots[index].Key = strings.TrimSpace(cfg.UploadRoots[index].Key)
		cfg.UploadRoots[index].Path = strings.TrimSpace(cfg.UploadRoots[index].Path)
		cfg.UploadRoots[index].TempPath = normalizeUploadRootTempPath(cfg.UploadRoots[index].TempPath)
	}
}

func applyBundledSemanticRuntimeHelper(cfg *Config) {
	if strings.TrimSpace(cfg.SemanticRuntime.HelperPath) != "" {
		return
	}
	helperPath, ok := bundledSemanticRuntimeHelperPath()
	if !ok {
		return
	}
	cfg.SemanticRuntime.HelperPath = helperPath
}

func applyBundledSemanticONNXRuntime(cfg *Config) {
	if strings.TrimSpace(cfg.SemanticRuntime.ONNXRuntime.ServerPath) != "" {
		if strings.TrimSpace(cfg.SemanticRuntime.ONNXRuntime.PythonPath) == "" {
			if pythonPath, ok := bundledSemanticONNXRuntimePythonPath(filepath.Dir(cfg.SemanticRuntime.ONNXRuntime.ServerPath)); ok {
				cfg.SemanticRuntime.ONNXRuntime.PythonPath = pythonPath
				cfg.SemanticRuntime.ONNXRuntime.PythonAutoDetected = true
			}
		}
		return
	}
	serverPath, ok := bundledSemanticONNXRuntimeServerPath()
	if !ok {
		return
	}
	cfg.SemanticRuntime.ONNXRuntime.ServerPath = serverPath
	cfg.SemanticRuntime.ONNXRuntime.ServerAutoDetected = true
	if strings.TrimSpace(cfg.SemanticRuntime.ONNXRuntime.PythonPath) == "" {
		if pythonPath, ok := bundledSemanticONNXRuntimePythonPath(filepath.Dir(serverPath)); ok {
			cfg.SemanticRuntime.ONNXRuntime.PythonPath = pythonPath
			cfg.SemanticRuntime.ONNXRuntime.PythonAutoDetected = true
		}
	}
}

func applyBundledMediaRuntimeVips(cfg *Config) {
	if strings.TrimSpace(cfg.MediaRuntime.VipsPath) != "" {
		return
	}
	vipsPath, ok := bundledMediaRuntimeVipsPath()
	if !ok {
		return
	}
	cfg.MediaRuntime.VipsPath = vipsPath
}

func applyBundledMediaRuntimeHelper(cfg *Config) {
	if strings.TrimSpace(cfg.MediaRuntime.HelperPath) != "" {
		return
	}
	helperPath, ok := bundledMediaRuntimeHelperPath()
	if !ok {
		return
	}
	cfg.MediaRuntime.HelperPath = helperPath
}

func applyBundledMediaRuntimeFFmpeg(cfg *Config) {
	if strings.TrimSpace(cfg.MediaRuntime.FFmpegPath) != "" {
		return
	}
	ffmpegPath, ok := bundledMediaRuntimeFFmpegPath()
	if !ok {
		return
	}
	cfg.MediaRuntime.FFmpegPath = ffmpegPath
}

func bundledSemanticRuntimeHelperPath() (string, bool) {
	executablePath, err := currentExecutablePath()
	if err != nil {
		return "", false
	}
	executablePath = strings.TrimSpace(executablePath)
	if executablePath == "" {
		return "", false
	}
	candidates := []string{
		filepath.Join(filepath.Dir(executablePath), semanticRuntimeHelperBinaryName()),
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

func bundledSemanticONNXRuntimeServerPath() (string, bool) {
	executablePath, err := currentExecutablePath()
	if err != nil {
		return "", false
	}
	executablePath = strings.TrimSpace(executablePath)
	if executablePath == "" {
		return "", false
	}
	candidates := []string{
		filepath.Join(filepath.Dir(executablePath), "semantic-runtime", "siglip2-onnx", "server.py"),
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

func bundledSemanticONNXRuntimePythonPath(runtimeDir string) (string, bool) {
	runtimeDir = strings.TrimSpace(runtimeDir)
	if runtimeDir == "" {
		return "", false
	}
	candidates := []string{
		filepath.Join(runtimeDir, ".venv", "bin", "python"),
		filepath.Join(runtimeDir, "venv", "bin", "python"),
		filepath.Join(runtimeDir, ".venv", "Scripts", "python.exe"),
		filepath.Join(runtimeDir, "venv", "Scripts", "python.exe"),
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

func bundledMediaRuntimeHelperPath() (string, bool) {
	executablePath, err := currentExecutablePath()
	if err != nil {
		return "", false
	}
	executablePath = strings.TrimSpace(executablePath)
	if executablePath == "" {
		return "", false
	}
	candidates := []string{
		filepath.Join(filepath.Dir(executablePath), mediaRuntimeHelperBinaryName()),
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

func bundledMediaRuntimeVipsPath() (string, bool) {
	executablePath, err := currentExecutablePath()
	if err != nil {
		return "", false
	}
	executablePath = strings.TrimSpace(executablePath)
	if executablePath == "" {
		return "", false
	}
	candidates := []string{
		filepath.Join(filepath.Dir(executablePath), "media-runtime", "libvips", "bin", mediaRuntimeVipsBinaryName()),
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

func bundledMediaRuntimeFFmpegPath() (string, bool) {
	executablePath, err := currentExecutablePath()
	if err != nil {
		return "", false
	}
	executablePath = strings.TrimSpace(executablePath)
	if executablePath == "" {
		return "", false
	}
	candidates := []string{
		filepath.Join(filepath.Dir(executablePath), "media-runtime", "ffmpeg", "bin", mediaRuntimeFFmpegBinaryName()),
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

func semanticRuntimeHelperBinaryName() string {
	if os.PathSeparator == '\\' {
		return "timich-semantic-helper.exe"
	}
	return "timich-semantic-helper"
}

func mediaRuntimeHelperBinaryName() string {
	if os.PathSeparator == '\\' {
		return "timich-media-helper.exe"
	}
	return "timich-media-helper"
}

func mediaRuntimeVipsBinaryName() string {
	if os.PathSeparator == '\\' {
		return "vips.exe"
	}
	return "vips"
}

func mediaRuntimeFFmpegBinaryName() string {
	if os.PathSeparator == '\\' {
		return "ffmpeg.exe"
	}
	return "ffmpeg"
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

func applyIntPtrEnv(key string, target **int) {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			parsedCopy := parsed
			*target = &parsedCopy
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
	if helperPath := strings.TrimSpace(cfg.SemanticRuntime.HelperPath); helperPath != "" && !filepath.IsAbs(helperPath) {
		return errors.New("semantic runtime helper path must be absolute")
	}
	if serverPath := strings.TrimSpace(cfg.SemanticRuntime.ONNXRuntime.ServerPath); serverPath != "" && !filepath.IsAbs(serverPath) {
		return errors.New("semantic ONNX runtime server path must be absolute")
	}
	if pythonPath := strings.TrimSpace(cfg.SemanticRuntime.ONNXRuntime.PythonPath); pythonPath != "" && strings.ContainsAny(pythonPath, `/\`) && !filepath.IsAbs(pythonPath) {
		return errors.New("semantic ONNX runtime python path must be absolute or a command name")
	}
	if cfg.SemanticRuntime.ONNXRuntime.Port < 0 {
		return errors.New("semantic ONNX runtime port must not be negative")
	}
	if vipsPath := strings.TrimSpace(cfg.MediaRuntime.VipsPath); vipsPath != "" && !filepath.IsAbs(vipsPath) {
		return errors.New("media runtime vips path must be absolute")
	}
	if ffmpegPath := strings.TrimSpace(cfg.MediaRuntime.FFmpegPath); ffmpegPath != "" && !filepath.IsAbs(ffmpegPath) {
		return errors.New("media runtime ffmpeg path must be absolute")
	}
	if helperPath := strings.TrimSpace(cfg.MediaRuntime.HelperPath); helperPath != "" && !filepath.IsAbs(helperPath) {
		return errors.New("media runtime helper path must be absolute")
	}
	if interval := strings.TrimSpace(cfg.SemanticRuntime.Indexing.Interval); interval != "" {
		parsed, err := time.ParseDuration(interval)
		if err != nil || parsed <= 0 {
			return errors.New("semantic runtime indexing interval must be a positive duration")
		}
	}
	if cfg.SemanticRuntime.Indexing.BatchSize < 0 {
		return errors.New("semantic runtime indexing batchSize must not be negative")
	}
	if cfg.SemanticRuntime.Indexing.TargetCompletedVectors < 0 {
		return errors.New("semantic runtime indexing targetCompletedVectors must not be negative")
	}
	if cfg.WorkerRuntime.HeavyTaskWorkers != nil && *cfg.WorkerRuntime.HeavyTaskWorkers < 0 {
		return errors.New("worker runtime heavyTaskWorkers must not be negative")
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

	localRootKeys := map[string]struct{}{}
	for _, root := range cfg.LocalMediaRoots {
		localRootKeys[root.Key] = struct{}{}
	}
	datasourceKindsBySourceKey := make(map[string]string, len(cfg.Datasources))
	for _, datasource := range cfg.Datasources {
		datasourceKindsBySourceKey[strings.TrimSpace(datasource.SourceKey)] = strings.TrimSpace(datasource.Kind)
	}
	type configuredExternalPrefix struct {
		localSourceKey string
		prefix         string
	}
	externalPrefixesByImmichSource := map[string][]configuredExternalPrefix{}
	passthroughImmichCount := 0
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
		if datasource.Kind != DatasourceKindImmich && datasource.Kind != DatasourceKindImmichIndexed && datasource.Kind != DatasourceKindLocalFiles && datasource.Kind != DatasourceKindStaticDemo {
			return fmt.Errorf("datasource %d: unsupported kind %q", index, datasource.Kind)
		}
		if IsImmichPassthroughDatasourceKind(datasource.Kind) {
			passthroughImmichCount++
		}
		if datasource.Indexing != nil {
			if datasource.Kind != DatasourceKindImmichIndexed {
				return fmt.Errorf("datasource %d: indexing tuning is only supported for immich_indexed datasources", index)
			}
			if datasource.Indexing.LatestAssetLimit < 0 {
				return fmt.Errorf("datasource %d: indexing latestAssetLimit must be non-negative", index)
			}
			if datasource.Indexing.MetadataDetailLimit < 0 {
				return fmt.Errorf("datasource %d: indexing metadataDetailLimit must be non-negative", index)
			}
			if interval := strings.TrimSpace(datasource.Indexing.Phase0SyncInterval); interval != "" {
				parsed, err := time.ParseDuration(interval)
				if err != nil || parsed <= 0 {
					return fmt.Errorf("datasource %d: indexing phase0SyncInterval must be a positive duration", index)
				}
			}
			if window := strings.TrimSpace(datasource.Indexing.DailyFullSweepWindow); window != "" && !validDailyFullSweepWindow(window) {
				return fmt.Errorf("datasource %d: indexing dailyFullSweepWindow must use HH:MM", index)
			}
		}
		switch datasource.Kind {
		case DatasourceKindLocalFiles:
			rootKey := strings.TrimSpace(datasource.RootKey)
			if err := ValidateLocalMediaRootKey(rootKey); err != nil {
				return fmt.Errorf("datasource %d: rootKey: %w", index, err)
			}
			if _, ok := localRootKeys[rootKey]; !ok {
				return fmt.Errorf("datasource %d: rootKey %q is not configured", index, rootKey)
			}
			if datasource.Scan != nil {
				for mappingIndex, mapping := range datasource.Scan.ImmichExternalLibraryMappings {
					immichSourceKey := strings.TrimSpace(mapping.SourceKey)
					if err := ValidateDatasourceSourceKey(immichSourceKey); err != nil {
						return fmt.Errorf("datasource %d: scan immichExternalLibraryMappings[%d] sourceKey: %w", index, mappingIndex, err)
					}
					if datasourceKindsBySourceKey[immichSourceKey] != DatasourceKindImmichIndexed {
						return fmt.Errorf("datasource %d: scan immichExternalLibraryMappings[%d] sourceKey %q is not an immich_indexed datasource", index, mappingIndex, immichSourceKey)
					}
					prefix := normalizeImmichExternalOriginalPathPrefix(mapping.OriginalPathPrefix)
					if prefix == "" || !path.IsAbs(prefix) || prefix == "/" {
						return fmt.Errorf("datasource %d: scan immichExternalLibraryMappings[%d] originalPathPrefix must be an absolute non-root path", index, mappingIndex)
					}
					for _, existing := range externalPrefixesByImmichSource[immichSourceKey] {
						if immichExternalPrefixesOverlap(existing.prefix, prefix) {
							return fmt.Errorf("datasource %d: scan immichExternalLibraryMappings[%d] originalPathPrefix %q overlaps mapping for local datasource %q", index, mappingIndex, prefix, existing.localSourceKey)
						}
					}
					externalPrefixesByImmichSource[immichSourceKey] = append(externalPrefixesByImmichSource[immichSourceKey], configuredExternalPrefix{
						localSourceKey: datasource.SourceKey,
						prefix:         prefix,
					})
				}
				if datasource.Scan.FirstViewThumbnailCount < 0 {
					return fmt.Errorf("datasource %d: scan firstViewThumbnailCount must be non-negative", index)
				}
				if interval := strings.TrimSpace(datasource.Scan.QuickScanInterval); interval != "" {
					parsed, err := time.ParseDuration(interval)
					if err != nil || parsed <= 0 {
						return fmt.Errorf("datasource %d: scan quickScanInterval must be a positive duration", index)
					}
				}
				if reconciliationTime := strings.TrimSpace(datasource.Scan.ReconciliationTime); reconciliationTime != "" {
					if _, err := time.Parse("15:04", reconciliationTime); err != nil {
						return fmt.Errorf("datasource %d: scan reconciliationTime must use 24-hour HH:MM format", index)
					}
				}
				if verificationTime := strings.TrimSpace(datasource.Scan.ContentVerificationTime); verificationTime != "" {
					if _, err := time.Parse("15:04", verificationTime); err != nil {
						return fmt.Errorf("datasource %d: scan contentVerificationTime must use 24-hour HH:MM format", index)
					}
				}
				if duration := strings.TrimSpace(datasource.Scan.ContentVerificationDuration); duration != "" {
					parsed, err := time.ParseDuration(duration)
					if err != nil || parsed < 0 {
						return fmt.Errorf("datasource %d: scan contentVerificationDuration must be a non-negative duration", index)
					}
				}
				if duration := strings.TrimSpace(datasource.Scan.SettlingDuration); duration != "" {
					parsed, err := time.ParseDuration(duration)
					if err != nil || parsed <= 0 {
						return fmt.Errorf("datasource %d: scan settlingDuration must be a positive duration", index)
					}
				}
			}
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
	if passthroughImmichCount > 0 && (passthroughImmichCount != 1 || len(cfg.Datasources) != 1) {
		return fmt.Errorf(
			"%w: kind %q requires exactly 1 datasource; found %d; change it to %q or remove the other datasource",
			ErrImmichPassthroughRequiresSingleDatasource,
			DatasourceKindImmich,
			len(cfg.Datasources),
			DatasourceKindImmichIndexed,
		)
	}

	if err := validateLocalMediaRoots(cfg.LocalMediaRoots); err != nil {
		return err
	}
	if err := validateUploadRoots(cfg.UploadRoots); err != nil {
		return err
	}

	return nil
}

func normalizeImmichExternalOriginalPathPrefix(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return path.Clean(value)
}

func immichExternalPrefixesOverlap(left string, right string) bool {
	left = normalizeImmichExternalOriginalPathPrefix(left)
	right = normalizeImmichExternalOriginalPathPrefix(right)
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func validDailyFullSweepWindow(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != len("00:00") || value[2] != ':' {
		return false
	}
	for _, index := range []int{0, 1, 3, 4} {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	hour := int(value[0]-'0')*10 + int(value[1]-'0')
	minute := int(value[3]-'0')*10 + int(value[4]-'0')
	return hour <= 23 && minute <= 59
}

func validateLocalMediaRoots(roots []LocalMediaRootConfig) error {
	seen := map[string]struct{}{}
	for index, root := range roots {
		key := strings.TrimSpace(root.Key)
		if err := ValidateLocalMediaRootKey(key); err != nil {
			return fmt.Errorf("local media root %d: key: %w", index, err)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("local media root %d: duplicate key", index)
		}
		seen[key] = struct{}{}
		path := strings.TrimSpace(root.Path)
		if path == "" {
			return fmt.Errorf("local media root %d: path must not be empty", index)
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("local media root %d: path must be absolute", index)
		}
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
	if strings.Contains(trimmed, `\`) {
		return "", errors.New("must use forward slashes and remain inside the upload root")
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

// ValidateLocalMediaRootKey verifies the stable operator-facing read-only media root key.
func ValidateLocalMediaRootKey(value string) error {
	return validateOperatorRootKey(value)
}

// ValidateUploadRootKey verifies the stable operator-facing upload root key.
func ValidateUploadRootKey(value string) error {
	return validateOperatorRootKey(value)
}

func validateOperatorRootKey(value string) error {
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
