package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	localMediaHelperCapabilityTimeout = 15 * time.Second
	localMediaFFmpegCapabilityTimeout = 10 * time.Second
)

type localMediaHelperCapabilityStatus struct {
	Status            string
	Usable            bool
	Version           string
	Platform          string
	RenderImage       bool
	RenderVideoPoster bool
	InspectImage      bool
	InspectVideo      bool
	LastError         string
}

type localFFmpegCapabilityStatus struct {
	Status    string
	Usable    bool
	Version   string
	Decoders  []string
	LastError string
}

func (s *Service) localMediaHelperCapabilityStatus() localMediaHelperCapabilityStatus {
	return s.localMediaHelperCapabilityStatusWithContext(context.Background())
}

func (s *Service) localMediaHelperCapabilityStatusWithContext(ctx context.Context) localMediaHelperCapabilityStatus {
	helperPath := strings.TrimSpace(s.mediaHelperPath)
	if helperPath == "" {
		return localMediaHelperCapabilityStatus{Status: "unavailable"}
	}

	s.mu.Lock()
	cached := s.mediaHelperCheck
	s.mu.Unlock()
	if cached.Status != "" {
		return cached
	}

	checked := inspectLocalMediaHelperCapabilityWithContext(ctx, helperPath, s.mediaVipsPath, s.mediaFFmpegPath)
	s.mu.Lock()
	if s.mediaHelperCheck.Status == "" && (ctx == nil || ctx.Err() == nil) {
		s.mediaHelperCheck = checked
	}
	cached = s.mediaHelperCheck
	s.mu.Unlock()
	if cached.Status == "" {
		return checked
	}
	return cached
}

type mediaHelperHealthResponse struct {
	SchemaVersion int  `json:"schemaVersion"`
	OK            bool `json:"ok"`
	Helper        struct {
		Version  string `json:"version"`
		Platform string `json:"platform"`
	} `json:"helper"`
	Capabilities struct {
		RenderImage       bool `json:"renderImage"`
		RenderVideoPoster bool `json:"renderVideoPoster"`
		InspectImage      bool `json:"inspectImage"`
		InspectVideo      bool `json:"inspectVideo"`
	} `json:"capabilities"`
}

func inspectLocalMediaHelperCapability(helperPath string, vipsPath string, ffmpegPath string) localMediaHelperCapabilityStatus {
	return inspectLocalMediaHelperCapabilityWithContext(context.Background(), helperPath, vipsPath, ffmpegPath)
}

func inspectLocalMediaHelperCapabilityWithContext(ctx context.Context, helperPath string, vipsPath string, ffmpegPath string) localMediaHelperCapabilityStatus {
	output, err := runLocalMediaHelperCommandWithContext(ctx, helperPath, vipsPath, ffmpegPath, "health", "--json")
	if err != nil {
		return localMediaHelperCapabilityStatus{
			Status:    "failed",
			LastError: err.Error(),
		}
	}

	var response mediaHelperHealthResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return localMediaHelperCapabilityStatus{
			Status:    "failed",
			LastError: fmt.Sprintf("media helper health JSON parse failed: %v", err),
		}
	}
	if response.SchemaVersion != 1 {
		return localMediaHelperCapabilityStatus{
			Status:    "failed",
			Version:   response.Helper.Version,
			Platform:  response.Helper.Platform,
			LastError: fmt.Sprintf("media helper health schema version %d is not supported", response.SchemaVersion),
		}
	}
	status := localMediaHelperCapabilityStatus{
		Status:            "ready",
		Usable:            response.OK,
		Version:           response.Helper.Version,
		Platform:          response.Helper.Platform,
		RenderImage:       response.Capabilities.RenderImage,
		RenderVideoPoster: response.Capabilities.RenderVideoPoster,
		InspectImage:      response.Capabilities.InspectImage,
		InspectVideo:      response.Capabilities.InspectVideo,
	}
	if !response.OK {
		status.Status = "failed"
		status.LastError = "media helper health returned ok=false"
	}
	return status
}

func (s *Service) localFFmpegCapabilityStatus() localFFmpegCapabilityStatus {
	return s.localFFmpegCapabilityStatusWithContext(context.Background())
}

func (s *Service) localFFmpegCapabilityStatusWithContext(ctx context.Context) localFFmpegCapabilityStatus {
	ffmpegPath := strings.TrimSpace(s.mediaFFmpegPath)
	if ffmpegPath == "" {
		return localFFmpegCapabilityStatus{Status: "unavailable"}
	}

	s.mu.Lock()
	cached := s.mediaFFmpegCheck
	s.mu.Unlock()
	if cached.Status != "" {
		return cached
	}

	checked := inspectLocalFFmpegCapabilityWithContext(ctx, ffmpegPath)
	s.mu.Lock()
	if s.mediaFFmpegCheck.Status == "" && (ctx == nil || ctx.Err() == nil) {
		s.mediaFFmpegCheck = checked
	}
	cached = s.mediaFFmpegCheck
	s.mu.Unlock()
	if cached.Status == "" {
		return checked
	}
	return cached
}

func inspectLocalFFmpegCapability(ffmpegPath string) localFFmpegCapabilityStatus {
	return inspectLocalFFmpegCapabilityWithContext(context.Background(), ffmpegPath)
}

func inspectLocalFFmpegCapabilityWithContext(ctx context.Context, ffmpegPath string) localFFmpegCapabilityStatus {
	version, err := localFFmpegVersionWithContext(ctx, ffmpegPath)
	if err != nil {
		return localFFmpegCapabilityStatus{
			Status:    "failed",
			LastError: err.Error(),
		}
	}

	decoders, decoderErr := localFFmpegCommonVideoDecodersWithContext(ctx, ffmpegPath)
	if err := localFFmpegPosterSmokeWithContext(ctx, ffmpegPath); err != nil {
		status := localFFmpegCapabilityStatus{
			Status:    "failed",
			Version:   version,
			Decoders:  decoders,
			LastError: err.Error(),
		}
		if decoderErr != nil {
			status.LastError = decoderErr.Error() + "; " + status.LastError
		}
		return status
	}

	status := localFFmpegCapabilityStatus{
		Status:   "ready",
		Usable:   true,
		Version:  version,
		Decoders: decoders,
	}
	if decoderErr != nil {
		status.Status = "warning"
		status.LastError = decoderErr.Error()
	} else if len(decoders) == 0 {
		status.Status = "warning"
		status.LastError = "ffmpeg -decoders did not report common MP4/MOV video decoders"
	}
	return status
}

func localFFmpegVersion(ffmpegPath string) (string, error) {
	return localFFmpegVersionWithContext(context.Background(), ffmpegPath)
}

func localFFmpegVersionWithContext(ctx context.Context, ffmpegPath string) (string, error) {
	output, err := runLocalFFmpegCommandWithContext(ctx, ffmpegPath, "-hide_banner", "-version")
	if err != nil {
		return "", fmt.Errorf("ffmpeg version check failed: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "ffmpeg" && fields[1] == "version" {
			return fields[2], nil
		}
		return trimLocalMediaRuntimeMessage(line), nil
	}
	return "", fmt.Errorf("ffmpeg version check failed: no version output")
}

func localFFmpegCommonVideoDecoders(ffmpegPath string) ([]string, error) {
	return localFFmpegCommonVideoDecodersWithContext(context.Background(), ffmpegPath)
}

func localFFmpegCommonVideoDecodersWithContext(ctx context.Context, ffmpegPath string) ([]string, error) {
	output, err := runLocalFFmpegCommandWithContext(ctx, ffmpegPath, "-hide_banner", "-decoders")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg decoder check failed: %w", err)
	}

	found := map[string]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.HasPrefix(fields[0], "V") {
			found[fields[1]] = true
		}
	}

	order := []string{"h264", "hevc", "mpeg4", "prores", "mjpeg", "vp9", "av1"}
	decoders := make([]string, 0, len(order))
	for _, decoder := range order {
		if found[decoder] {
			decoders = append(decoders, decoder)
		}
	}
	return decoders, nil
}

func localFFmpegPosterSmoke(ffmpegPath string) error {
	return localFFmpegPosterSmokeWithContext(context.Background(), ffmpegPath)
}

func localFFmpegPosterSmokeWithContext(ctx context.Context, ffmpegPath string) error {
	tempDir, err := os.MkdirTemp("", "timich-ffmpeg-check-*")
	if err != nil {
		return fmt.Errorf("create ffmpeg smoke temp directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	inputPath := filepath.Join(tempDir, "input.jpg")
	outputPath := filepath.Join(tempDir, "poster.jpg")
	if err := writeLocalFFmpegPosterSmokeInput(inputPath); err != nil {
		return err
	}
	if _, err := runLocalFFmpegCommandWithContext(ctx, ffmpegPath,
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-y",
		"-i", inputPath,
		"-map", "0:v:0",
		"-frames:v", "1",
		"-an",
		"-sn",
		"-dn",
		outputPath,
	); err != nil {
		return fmt.Errorf("ffmpeg poster smoke failed: %w", err)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("ffmpeg poster smoke did not create output: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("ffmpeg poster smoke created an empty output")
	}
	return nil
}

func writeLocalFFmpegPosterSmokeInput(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create ffmpeg poster smoke input: %w", err)
	}
	defer file.Close()

	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	if err := jpeg.Encode(file, img, &jpeg.Options{Quality: 85}); err != nil {
		return fmt.Errorf("write ffmpeg poster smoke input: %w", err)
	}
	return nil
}

func runLocalMediaHelperCommand(helperPath string, vipsPath string, ffmpegPath string, args ...string) ([]byte, error) {
	return runLocalMediaHelperCommandWithContext(context.Background(), helperPath, vipsPath, ffmpegPath, args...)
}

func runLocalMediaHelperCommandWithContext(ctx context.Context, helperPath string, vipsPath string, ffmpegPath string, args ...string) ([]byte, error) {
	output, err := runLocalMediaHelperCommandContext(ctx, localMediaHelperCapabilityTimeout, helperPath, vipsPath, ffmpegPath, args...)
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return output, err
		}
		return output, fmt.Errorf("%s", trimLocalMediaRuntimeMessage(message))
	}
	return output, nil
}

func runLocalFFmpegCommand(ffmpegPath string, args ...string) ([]byte, error) {
	return runLocalFFmpegCommandWithContext(context.Background(), ffmpegPath, args...)
}

func runLocalFFmpegCommandWithContext(ctx context.Context, ffmpegPath string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, localMediaFFmpegCapabilityTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, ffmpegPath, args...)
	command.Env = localMediaFFmpegCommandEnv(os.Environ(), ffmpegPath)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return output, err
		}
		return output, fmt.Errorf("%s", trimLocalMediaRuntimeMessage(message))
	}
	return output, nil
}

func resolveMediaHelperPath(configuredPath string) (string, bool) {
	configuredPath = strings.TrimSpace(configuredPath)
	if configuredPath != "" {
		return configuredPath, false
	}
	path, err := exec.LookPath(mediaHelperBinaryName())
	if err != nil {
		return "", false
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return path, true
	}
	return absolutePath, true
}

func localMediaHelperCommandEnv(base []string, vipsPath string, ffmpegPath string) []string {
	env := append([]string{}, base...)
	if vipsPath = strings.TrimSpace(vipsPath); vipsPath != "" {
		env = setEnvValue(env, "TIMICH_AGENT_VIPS_PATH", vipsPath)
	}
	if ffmpegPath = strings.TrimSpace(ffmpegPath); ffmpegPath != "" {
		env = setEnvValue(env, "TIMICH_AGENT_FFMPEG_PATH", ffmpegPath)
	}
	return env
}

func setEnvValue(env []string, key string, value string) []string {
	if strings.TrimSpace(key) == "" {
		return env
	}
	prefix := key + "="
	for index, item := range env {
		if strings.HasPrefix(item, prefix) {
			env[index] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func mediaHelperBinaryName() string {
	if os.PathSeparator == '\\' {
		return "timich-media-helper.exe"
	}
	return "timich-media-helper"
}

func trimLocalMediaRuntimeMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= 400 {
		return message
	}
	return strings.TrimSpace(message[:400]) + "..."
}
