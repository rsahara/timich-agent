package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
)

const (
	defaultAssetCount     = 120
	defaultStartDate      = "2026-01-01T12:00:00Z"
	defaultImageStepHours = 1
)

type sourceFile struct {
	Path string
	Ext  string
}

type manifest struct {
	Version     int             `json:"version"`
	GeneratedAt time.Time       `json:"generatedAt"`
	Assets      []manifestAsset `json:"assets"`
}

type manifestAsset struct {
	ID                string    `json:"id"`
	Type              string    `json:"type"`
	OriginalFileName  string    `json:"originalFileName"`
	FileCreatedAt     time.Time `json:"fileCreatedAt"`
	Duration          *string   `json:"duration,omitempty"`
	PreviewPath       string    `json:"previewPath"`
	DetailPreviewPath string    `json:"detailPreviewPath"`
	OriginalPath      string    `json:"originalPath"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "timich-static-demo-bundle: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	inputDir := flag.String("input", "", "directory containing source images and videos")
	outputDir := flag.String("output", "", "directory to write the static demo bundle")
	assetCount := flag.Int("count", defaultAssetCount, "total asset records to generate")
	videoCount := flag.Int("video-count", -1, "video assets to include; -1 includes each source video once, up to 5")
	startDateRaw := flag.String("start-date", defaultStartDate, "RFC3339 timestamp for the newest generated asset")
	flag.Parse()

	if strings.TrimSpace(*inputDir) == "" {
		return errors.New("--input is required")
	}
	if strings.TrimSpace(*outputDir) == "" {
		return errors.New("--output is required")
	}
	if *assetCount < 1 {
		return errors.New("--count must be at least 1")
	}
	startDate, err := time.Parse(time.RFC3339, strings.TrimSpace(*startDateRaw))
	if err != nil {
		return fmt.Errorf("parse --start-date: %w", err)
	}

	images, videos, err := scanSources(*inputDir)
	if err != nil {
		return err
	}
	if len(images) == 0 {
		return errors.New("input directory does not contain supported image files")
	}
	resolvedVideoCount := *videoCount
	if resolvedVideoCount < 0 {
		resolvedVideoCount = min(len(videos), 5, *assetCount)
	}
	if resolvedVideoCount > len(videos) {
		return fmt.Errorf("--video-count=%d exceeds source video count %d", resolvedVideoCount, len(videos))
	}
	if resolvedVideoCount > *assetCount {
		return fmt.Errorf("--video-count=%d exceeds total count %d", resolvedVideoCount, *assetCount)
	}

	if err := os.RemoveAll(*outputDir); err != nil {
		return fmt.Errorf("clear output directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(*outputDir, "assets"), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	tempDir, err := os.MkdirTemp("", "timich-static-demo-*")
	if err != nil {
		return fmt.Errorf("create temp directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	manifest := manifest{
		Version:     1,
		GeneratedAt: time.Now().UTC(),
		Assets:      make([]manifestAsset, 0, *assetCount),
	}
	imageCount := *assetCount - resolvedVideoCount
	for index := 0; index < imageCount; index++ {
		source := images[index%len(images)]
		asset, err := generateImageAsset(*outputDir, source, index, startDate)
		if err != nil {
			return err
		}
		manifest.Assets = append(manifest.Assets, asset)
	}
	for index := 0; index < resolvedVideoCount; index++ {
		source := videos[index]
		assetIndex := imageCount + index
		asset, err := generateVideoAsset(*outputDir, tempDir, source, assetIndex, startDate)
		if err != nil {
			return err
		}
		manifest.Assets = append(manifest.Assets, asset)
	}

	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(*outputDir, "manifest.json"), raw, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	fmt.Printf("wrote %d static demo assets to %s\n", len(manifest.Assets), *outputDir)
	return nil
}

func scanSources(inputDir string) ([]sourceFile, []sourceFile, error) {
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read input directory: %w", err)
	}
	var images []sourceFile
	var videos []sourceFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(inputDir, entry.Name())
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		switch ext {
		case ".jpg", ".jpeg", ".png":
			images = append(images, sourceFile{Path: path, Ext: ext})
		case ".mov", ".mp4", ".m4v":
			videos = append(videos, sourceFile{Path: path, Ext: ext})
		}
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Path < images[j].Path })
	sort.Slice(videos, func(i, j int) bool { return videos[i].Path < videos[j].Path })
	return images, videos, nil
}

func generateImageAsset(outputDir string, source sourceFile, index int, startDate time.Time) (manifestAsset, error) {
	id := fmt.Sprintf("static-demo-%04d", index+1)
	assetDir := filepath.Join(outputDir, "assets", id)
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		return manifestAsset{}, fmt.Errorf("create asset directory %s: %w", id, err)
	}

	body, err := os.ReadFile(source.Path)
	if err != nil {
		return manifestAsset{}, fmt.Errorf("read source image %s: %w", source.Path, err)
	}
	if err := writeImageVariants(assetDir, body); err != nil {
		return manifestAsset{}, fmt.Errorf("generate image asset %s: %w", id, err)
	}

	return manifestAsset{
		ID:                id,
		Type:              "IMAGE",
		OriginalFileName:  id + ".jpg",
		FileCreatedAt:     generatedAssetDate(startDate, index),
		PreviewPath:       filepath.ToSlash(filepath.Join("assets", id, "preview.jpg")),
		DetailPreviewPath: filepath.ToSlash(filepath.Join("assets", id, "detail_preview.jpg")),
		OriginalPath:      filepath.ToSlash(filepath.Join("assets", id, "original.jpg")),
	}, nil
}

func generateVideoAsset(outputDir string, tempDir string, source sourceFile, index int, startDate time.Time) (manifestAsset, error) {
	id := fmt.Sprintf("static-demo-%04d", index+1)
	assetDir := filepath.Join(outputDir, "assets", id)
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		return manifestAsset{}, fmt.Errorf("create asset directory %s: %w", id, err)
	}

	posterPath := filepath.Join(tempDir, id+"-poster.jpg")
	if err := extractVideoPoster(source.Path, posterPath); err != nil {
		return manifestAsset{}, err
	}
	posterBody, err := os.ReadFile(posterPath)
	if err != nil {
		return manifestAsset{}, fmt.Errorf("read video poster: %w", err)
	}
	if err := writeImageVariants(assetDir, posterBody); err != nil {
		return manifestAsset{}, fmt.Errorf("generate video poster asset %s: %w", id, err)
	}

	originalName := id + source.Ext
	if err := copyFile(source.Path, filepath.Join(assetDir, originalName)); err != nil {
		return manifestAsset{}, fmt.Errorf("copy video original %s: %w", id, err)
	}
	duration := probeVideoDuration(source.Path)

	return manifestAsset{
		ID:                id,
		Type:              "VIDEO",
		OriginalFileName:  originalName,
		FileCreatedAt:     generatedAssetDate(startDate, index),
		Duration:          duration,
		PreviewPath:       filepath.ToSlash(filepath.Join("assets", id, "preview.jpg")),
		DetailPreviewPath: filepath.ToSlash(filepath.Join("assets", id, "detail_preview.jpg")),
		OriginalPath:      filepath.ToSlash(filepath.Join("assets", id, originalName)),
	}, nil
}

func writeImageVariants(assetDir string, body []byte) error {
	original, err := catalog.RenderStaticDemoOriginal(body)
	if err != nil {
		return fmt.Errorf("render original: %w", err)
	}
	preview, err := catalog.RenderStaticDemoPreview(body)
	if err != nil {
		return fmt.Errorf("render preview: %w", err)
	}
	detailPreview, err := catalog.RenderStaticDemoDetailPreview(body)
	if err != nil {
		return fmt.Errorf("render detail preview: %w", err)
	}
	for path, payload := range map[string][]byte{
		"original.jpg":       original,
		"preview.jpg":        preview,
		"detail_preview.jpg": detailPreview,
	} {
		if err := os.WriteFile(filepath.Join(assetDir, path), payload, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

func extractVideoPoster(sourcePath string, outputPath string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return errors.New("ffmpeg is required to extract video posters")
	}
	command := exec.Command(
		"ffmpeg",
		"-v", "error",
		"-y",
		"-i", sourcePath,
		"-frames:v", "1",
		outputPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("extract video poster from %s: %w: %s", sourcePath, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func probeVideoDuration(sourcePath string) *string {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return nil
	}
	command := exec.Command(
		"ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		sourcePath,
	)
	output, err := command.Output()
	if err != nil {
		return nil
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil || seconds <= 0 {
		return nil
	}
	formatted := formatImmichDuration(seconds)
	return &formatted
}

func formatImmichDuration(seconds float64) string {
	totalSeconds := int(seconds + 0.5)
	hours := totalSeconds / 3600
	minutes := (totalSeconds % 3600) / 60
	remainingSeconds := totalSeconds % 60
	return fmt.Sprintf("%d:%02d:%02d.000000", hours, minutes, remainingSeconds)
}

func generatedAssetDate(startDate time.Time, index int) time.Time {
	return startDate.Add(-time.Duration(index*defaultImageStepHours) * time.Hour).UTC()
}

func copyFile(sourcePath string, targetPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.Create(targetPath)
	if err != nil {
		return err
	}
	defer target.Close()
	_, err = io.Copy(target, source)
	return err
}
