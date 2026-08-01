package semanticmanifest

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const Product = "timich-semantic-models"

const RuntimeONNX = "onnxruntime"

const MaxArtifactSizeBytes = int64(8 << 30)

type Manifest struct {
	SchemaVersion          int           `json:"schemaVersion"`
	Product                string        `json:"product"`
	Version                string        `json:"version,omitempty"`
	Recommended            string        `json:"recommended,omitempty"`
	RecommendedRuntimePack string        `json:"recommendedRuntimePack,omitempty"`
	Models                 []Model       `json:"models,omitempty"`
	RuntimePacks           []RuntimePack `json:"runtimePacks,omitempty"`
}

type Model struct {
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	Version        string              `json:"version,omitempty"`
	VectorSpaceID  string              `json:"vectorSpaceId"`
	EmbeddingDim   int                 `json:"embeddingDim"`
	InputKind      string              `json:"inputKind"`
	QueryLanguages []string            `json:"queryLanguages,omitempty"`
	Runtime        string              `json:"runtime,omitempty"`
	Quantization   string              `json:"quantization,omitempty"`
	License        string              `json:"license,omitempty"`
	Artifacts      map[string]Artifact `json:"artifacts,omitempty"`
}

type Artifact struct {
	Filename  string `json:"filename"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

type RuntimePack struct {
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	Version   string              `json:"version,omitempty"`
	Runtime   string              `json:"runtime"`
	License   string              `json:"license,omitempty"`
	Artifacts map[string]Artifact `json:"artifacts,omitempty"`
}

type ValidationOptions struct {
	RequiredPlatform              string
	RequireRecommendedModel       bool
	RequireRecommendedRuntimePack bool
}

func Validate(manifest *Manifest, options ValidationOptions) error {
	if manifest == nil {
		return errors.New("semantic model manifest is required")
	}
	manifest.Product = strings.TrimSpace(manifest.Product)
	manifest.Version = strings.TrimSpace(manifest.Version)
	manifest.Recommended = strings.TrimSpace(manifest.Recommended)
	manifest.RecommendedRuntimePack = strings.TrimSpace(manifest.RecommendedRuntimePack)
	if manifest.SchemaVersion != 1 {
		return fmt.Errorf("unsupported semantic model manifest schemaVersion %d", manifest.SchemaVersion)
	}
	if manifest.Product != Product {
		return errors.New("manifest is not for timich semantic models")
	}
	if len(manifest.Models) == 0 && len(manifest.RuntimePacks) == 0 {
		return errors.New("semantic model manifest has no models or runtime packs")
	}

	modelIDs := make(map[string]struct{}, len(manifest.Models))
	for index := range manifest.Models {
		model := &manifest.Models[index]
		normalizeModel(model)
		if model.ID == "" {
			return fmt.Errorf("semantic model %d: id is required", index)
		}
		if _, exists := modelIDs[model.ID]; exists {
			return fmt.Errorf("semantic model %q is duplicated", model.ID)
		}
		modelIDs[model.ID] = struct{}{}
		if model.Name == "" {
			return fmt.Errorf("semantic model %q: name is required", model.ID)
		}
		if !validIdentityVersion(model.Version) {
			return fmt.Errorf("semantic model %q: version is required and must contain only letters, digits, '.', '-', or '_'", model.ID)
		}
		if model.VectorSpaceID == "" {
			return fmt.Errorf("semantic model %q: vectorSpaceId is required", model.ID)
		}
		if model.EmbeddingDim <= 0 {
			return fmt.Errorf("semantic model %q: embeddingDim must be positive", model.ID)
		}
		if model.InputKind != "image" {
			return fmt.Errorf("semantic model %q: inputKind must be image", model.ID)
		}
		if err := validateArtifacts("semantic model", model.ID, model.Artifacts); err != nil {
			return err
		}
	}

	runtimePackIDs := make(map[string]struct{}, len(manifest.RuntimePacks))
	for index := range manifest.RuntimePacks {
		pack := &manifest.RuntimePacks[index]
		normalizeRuntimePack(pack)
		if pack.ID == "" {
			return fmt.Errorf("semantic runtime pack %d: id is required", index)
		}
		if _, exists := runtimePackIDs[pack.ID]; exists {
			return fmt.Errorf("semantic runtime pack %q is duplicated", pack.ID)
		}
		runtimePackIDs[pack.ID] = struct{}{}
		if pack.Name == "" {
			return fmt.Errorf("semantic runtime pack %q: name is required", pack.ID)
		}
		if !validIdentityVersion(pack.Version) {
			return fmt.Errorf("semantic runtime pack %q: version is required and must contain only letters, digits, '.', '-', or '_'", pack.ID)
		}
		if pack.Runtime != RuntimeONNX {
			return fmt.Errorf("semantic runtime pack %q: runtime must be %q", pack.ID, RuntimeONNX)
		}
		if err := validateArtifacts("semantic runtime pack", pack.ID, pack.Artifacts); err != nil {
			return err
		}
	}

	if options.RequireRecommendedModel && manifest.Recommended == "" {
		return errors.New("semantic model manifest recommended model is required")
	}
	if manifest.Recommended != "" {
		if _, ok := modelIDs[manifest.Recommended]; !ok {
			return fmt.Errorf("semantic model manifest recommended model %q was not found", manifest.Recommended)
		}
	}
	if options.RequireRecommendedRuntimePack && manifest.RecommendedRuntimePack == "" {
		return errors.New("semantic model manifest recommended runtime pack is required")
	}
	if manifest.RecommendedRuntimePack != "" {
		if _, ok := runtimePackIDs[manifest.RecommendedRuntimePack]; !ok {
			return fmt.Errorf("semantic model manifest recommended runtime pack %q was not found", manifest.RecommendedRuntimePack)
		}
	}

	platform := strings.TrimSpace(options.RequiredPlatform)
	if platform == "" {
		return nil
	}
	if options.RequireRecommendedModel {
		model, _ := RecommendedModel(*manifest)
		if _, ok := ArtifactForPlatform(model.Artifacts, platform); !ok {
			return fmt.Errorf("semantic model %q has no artifact for %s", model.ID, platform)
		}
	}
	if options.RequireRecommendedRuntimePack {
		pack, _ := RecommendedRuntimePack(*manifest)
		if _, ok := ArtifactForPlatform(pack.Artifacts, platform); !ok {
			return fmt.Errorf("semantic runtime pack %q has no artifact for %s", pack.ID, platform)
		}
	}
	if options.RequireRecommendedModel && options.RequireRecommendedRuntimePack {
		model, _ := RecommendedModel(*manifest)
		pack, _ := RecommendedRuntimePack(*manifest)
		if model.Runtime == "" {
			return fmt.Errorf("semantic model %q: runtime is required for a recommended release model", model.ID)
		}
		if model.Runtime != pack.Runtime {
			return fmt.Errorf("semantic model %q runtime %q does not match recommended runtime pack %q runtime %q", model.ID, model.Runtime, pack.ID, pack.Runtime)
		}
	}
	return nil
}

func validIdentityVersion(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func RecommendedModel(manifest Manifest) (Model, bool) {
	id := strings.TrimSpace(manifest.Recommended)
	for _, model := range manifest.Models {
		if strings.TrimSpace(model.ID) == id && id != "" {
			return model, true
		}
	}
	return Model{}, false
}

func RecommendedRuntimePack(manifest Manifest) (RuntimePack, bool) {
	id := strings.TrimSpace(manifest.RecommendedRuntimePack)
	for _, pack := range manifest.RuntimePacks {
		if strings.TrimSpace(pack.ID) == id && id != "" {
			return pack, true
		}
	}
	return RuntimePack{}, false
}

func ArtifactForPlatform(artifacts map[string]Artifact, platform string) (Artifact, bool) {
	if artifact, ok := artifacts[strings.TrimSpace(platform)]; ok {
		return artifact, true
	}
	if artifact, ok := artifacts["default"]; ok {
		return artifact, true
	}
	return Artifact{}, false
}

func validateArtifacts(label string, ownerID string, artifacts map[string]Artifact) error {
	if len(artifacts) == 0 {
		return fmt.Errorf("%s %q: at least one artifact is required", label, ownerID)
	}
	normalized := make(map[string]Artifact, len(artifacts))
	for rawPlatform, artifact := range artifacts {
		platform := strings.TrimSpace(rawPlatform)
		if platform == "" {
			return fmt.Errorf("%s %q: artifact platform is required", label, ownerID)
		}
		if _, exists := normalized[platform]; exists {
			return fmt.Errorf("%s %q: artifact platform %q is duplicated after normalization", label, ownerID, platform)
		}
		artifact.Filename = strings.TrimSpace(artifact.Filename)
		artifact.URL = strings.TrimSpace(artifact.URL)
		artifact.SHA256 = strings.ToLower(strings.TrimSpace(artifact.SHA256))
		if artifact.Filename == "" {
			return fmt.Errorf("%s %q: artifact filename is required", label, ownerID)
		}
		if _, ok := safeURL(artifact.URL); !ok {
			return fmt.Errorf("%s %q: artifact URL for %s is not an http or https URL", label, ownerID, platform)
		}
		if !validSHA256Hex(artifact.SHA256) {
			return fmt.Errorf("%s %q: artifact SHA-256 for %s is required", label, ownerID, platform)
		}
		if artifact.SizeBytes <= 0 || artifact.SizeBytes > MaxArtifactSizeBytes {
			return fmt.Errorf("%s %q: artifact sizeBytes for %s must be between 1 and %d", label, ownerID, platform, MaxArtifactSizeBytes)
		}
		normalized[platform] = artifact
	}
	clear(artifacts)
	for platform, artifact := range normalized {
		artifacts[platform] = artifact
	}
	return nil
}

func normalizeModel(model *Model) {
	model.ID = strings.TrimSpace(model.ID)
	model.Name = strings.TrimSpace(model.Name)
	model.Version = strings.TrimSpace(model.Version)
	model.VectorSpaceID = strings.TrimSpace(model.VectorSpaceID)
	model.InputKind = strings.TrimSpace(model.InputKind)
	model.Runtime = strings.TrimSpace(model.Runtime)
	model.Quantization = strings.TrimSpace(model.Quantization)
	model.License = strings.TrimSpace(model.License)
	languages := make([]string, 0, len(model.QueryLanguages))
	for _, language := range model.QueryLanguages {
		if trimmed := strings.TrimSpace(language); trimmed != "" {
			languages = append(languages, trimmed)
		}
	}
	model.QueryLanguages = languages
}

func normalizeRuntimePack(pack *RuntimePack) {
	pack.ID = strings.TrimSpace(pack.ID)
	pack.Name = strings.TrimSpace(pack.Name)
	pack.Version = strings.TrimSpace(pack.Version)
	pack.Runtime = strings.TrimSpace(pack.Runtime)
	pack.License = strings.TrimSpace(pack.License)
}

func safeURL(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", false
	}
	return trimmed, true
}

func validSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return true
}
