package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rsahara/timich-agent/internal/adminapi"
	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/controlplane"
	"github.com/rsahara/timich-agent/internal/mediaapi"
	runtimestate "github.com/rsahara/timich-agent/internal/runtime"
	"github.com/rsahara/timich-agent/internal/store"
)

var (
	version           = "dev"
	commit            = "unknown"
	builtAt           = "unknown"
	updateManifestURL = ""
)

func main() {
	if err := runCLI(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		log.Fatal(err)
	}
}

func runCLI(args []string, stdout io.Writer, stderr io.Writer) error {
	if len(args) == 0 {
		return serve(nil, stderr)
	}

	switch args[0] {
	case "serve":
		return serve(args[1:], stderr)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "version-json":
		return writeVersionJSON(stdout)
	case "init":
		return initConfig(args[1:], stdout, stderr)
	default:
		return serve(args, stderr)
	}
}

func initConfig(args []string, stdout io.Writer, stderr io.Writer) error {
	flags := flag.NewFlagSet("timich-agent init", flag.ContinueOnError)
	flags.SetOutput(stderr)

	configPath := flags.String("config", config.DefaultConfigPath, "path to the JSON config file to create")
	dataDir := flags.String("data-dir", "", "optional data directory override to write into the starter config")
	if err := flags.Parse(args); err != nil {
		return err
	}

	writtenPath, err := config.WriteDefaultFile(*configPath, *dataDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Wrote starter config to %s\n", writtenPath)
	return nil
}

func writeVersionJSON(stdout io.Writer) error {
	payload := struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
		BuiltAt string `json:"builtAt"`
	}{
		Version: version,
		Commit:  commit,
		BuiltAt: builtAt,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode version: %w", err)
	}
	_, err = fmt.Fprintln(stdout, string(raw))
	return err
}

func serve(args []string, stderr io.Writer) error {
	cfg, err := loadConfigForServe(args, stderr)
	if err != nil {
		return err
	}

	loadedState, err := store.LoadOrCreate(cfg.DataDir)
	if err != nil {
		return err
	}

	startedAt := time.Now().UTC()
	runtime, err := runtimestate.NewAgentRuntime(runtimestate.BuildInfo{
		Version: version,
		Commit:  commit,
		BuiltAt: builtAt,
	}, cfg, loadedState, startedAt)
	if err != nil {
		return err
	}
	defer func() {
		if err := runtime.Close(); err != nil {
			log.Printf("timich-agent runtime close failed: %v", err)
		}
	}()
	runtime.StartUploadMaintenance()

	restartCh := make(chan struct{}, 1)
	adminServer := &http.Server{
		Addr: cfg.AdminListenAddress,
		Handler: adminapi.NewMuxWithOptions(runtime, adminapi.Options{
			UpdateManifestURL: resolveUpdateManifestURL(),
			Restart: func(context.Context) error {
				scheduleRestart(restartCh)
				return nil
			},
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	mediaServer := &http.Server{
		Addr:              cfg.MediaListenAddress,
		Handler:           mediaapi.NewMux(runtime),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf(
		"timich-agent started mode=%s agent_id=%s admin=%s media=%s data_dir=%s config_source=%s",
		runtime.StatusResponse().Mode,
		runtime.StatusResponse().AgentID,
		cfg.AdminListenAddress,
		cfg.MediaListenAddress,
		cfg.DataDir,
		cfg.ConfigSource,
	)
	logFirstRunAdminSetupPrompt(runtime, cfg, loadedState.Path)

	controlPlaneCtx, cancelControlPlane := context.WithCancel(context.Background())
	defer cancelControlPlane()
	if cfg.Hosted.Enabled && strings.TrimSpace(cfg.ControlPlaneAddress) != "" {
		client := controlplane.NewClient(version, cfg.ControlPlaneAddress, cfg.Hosted.ServerURL, runtime, controlplane.ClientOptions{
			TLS: controlplane.TLSConfig{
				ServerName: cfg.ControlPlaneServerName,
			},
			KeyID:                      loadedState.State.RelayKeyID,
			PublicKey:                  loadedState.State.RelayPublicKey,
			PrivateKey:                 loadedState.State.RelayPrivateKey,
			RelayRegistrationReadyFunc: runtime.RemoteRegistrationReady,
			RelayCredentialRegisteredFunc: func(syncedAt time.Time) {
				if err := runtime.UpdateRelayCredentialSyncedAt(syncedAt); err != nil {
					log.Printf("timich-agent relay credential sync timestamp save failed: %v", err)
				}
			},
		})
		go func() {
			if err := client.Run(controlPlaneCtx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("timich-agent control plane stopped: %v", err)
			}
		}()
	}

	return serveUntilShutdown(adminServer, mediaServer, restartCh)
}

func resolveUpdateManifestURL() string {
	if value := strings.TrimSpace(os.Getenv("TIMICH_AGENT_UPDATE_MANIFEST_URL")); value != "" {
		return value
	}
	return strings.TrimSpace(updateManifestURL)
}

func logFirstRunAdminSetupPrompt(runtime *runtimestate.AgentRuntime, cfg config.ResolvedConfig, statePath string) {
	if runtime.AdminAuthReady() {
		return
	}
	log.Printf(
		"timich-agent admin setup required: open %s from a trusted LAN or agent host to create the admin token",
		adminSetupURL(cfg.AdminListenAddress),
	)
	log.Printf("timich-agent admin token will be stored in %s after setup", statePath)
}

func adminSetupURL(listenAddress string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddress))
	if err != nil {
		return "http://localhost:8081/"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

func loadConfigForServe(args []string, stderr io.Writer) (config.ResolvedConfig, error) {
	flags := flag.NewFlagSet("timich-agent serve", flag.ContinueOnError)
	flags.SetOutput(stderr)

	configPath := flags.String("config", "", "path to the JSON config file")
	adminListenAddress := flags.String("admin-listen-addr", "", "override the admin API listen address")
	mediaListenAddress := flags.String("media-listen-addr", "", "override the media API listen address")
	dataDir := flags.String("data-dir", "", "override the agent data directory")
	if err := flags.Parse(args); err != nil {
		return config.ResolvedConfig{}, err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return config.ResolvedConfig{}, err
	}
	return config.ApplyRuntimeOverrides(cfg, *adminListenAddress, *mediaListenAddress, *dataDir)
}

func serveUntilShutdown(adminServer *http.Server, mediaServer *http.Server, restartCh <-chan struct{}) error {
	errCh := make(chan error, 2)
	go serveHTTP("admin", adminServer, errCh)
	go serveHTTP("media", mediaServer, errCh)

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signalCh)

	select {
	case sig := <-signalCh:
		log.Printf("timich-agent shutting down after signal=%s", sig)
		return shutdownServers(adminServer, mediaServer)
	case <-restartCh:
		log.Printf("timich-agent restarting after admin request")
		return shutdownServers(adminServer, mediaServer)
	case err := <-errCh:
		if err == nil {
			return nil
		}
		_ = shutdownServers(adminServer, mediaServer)
		return err
	}
}

func scheduleRestart(restartCh chan<- struct{}) {
	go func() {
		time.Sleep(250 * time.Millisecond)
		select {
		case restartCh <- struct{}{}:
		default:
		}
	}()
}

func serveHTTP(name string, server *http.Server, errCh chan<- error) {
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- fmt.Errorf("%s API server: %w", name, err)
	}
}

func shutdownServers(servers ...*http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var shutdownErr error
	for _, server := range servers {
		if err := server.Shutdown(ctx); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
	}
	return shutdownErr
}
