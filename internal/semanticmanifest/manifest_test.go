package semanticmanifest

import (
	"strings"
	"testing"
)

const testSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestValidateMatchesConsumerFields(t *testing.T) {
	manifest := validReleaseManifest()
	manifest.Models[0].Name = ""
	err := Validate(&manifest, ValidationOptions{})
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("Validate() error = %v, want consumer-required model name", err)
	}

	manifest = validReleaseManifest()
	manifest.Models[0].VectorSpaceID = ""
	err = Validate(&manifest, ValidationOptions{})
	if err == nil || !strings.Contains(err.Error(), "vectorSpaceId is required") {
		t.Fatalf("Validate() error = %v, want consumer-required vector space", err)
	}

	manifest = validReleaseManifest()
	manifest.Models[0].Version = ""
	err = Validate(&manifest, ValidationOptions{})
	if err == nil || !strings.Contains(err.Error(), "version is required") {
		t.Fatalf("Validate() error = %v, want consumer-required model version", err)
	}

	manifest = validReleaseManifest()
	manifest.RuntimePacks[0].Version = "unsafe/version"
	err = Validate(&manifest, ValidationOptions{})
	if err == nil || !strings.Contains(err.Error(), "version is required") {
		t.Fatalf("Validate() error = %v, want safe runtime version", err)
	}
}

func TestValidateStrictReleaseRequiresRecommendedLinuxArtifacts(t *testing.T) {
	options := ValidationOptions{
		RequiredPlatform:              "linux-amd64",
		RequireRecommendedModel:       true,
		RequireRecommendedRuntimePack: true,
	}
	manifest := validReleaseManifest()
	manifest.Recommended = ""
	if err := Validate(&manifest, options); err == nil || !strings.Contains(err.Error(), "recommended model is required") {
		t.Fatalf("Validate(missing recommended) error = %v", err)
	}

	manifest = validReleaseManifest()
	manifest.RuntimePacks[0].Artifacts = map[string]Artifact{"linux_amd64": manifest.RuntimePacks[0].Artifacts["linux-amd64"]}
	if err := Validate(&manifest, options); err == nil || !strings.Contains(err.Error(), "no artifact for linux-amd64") {
		t.Fatalf("Validate(wrong platform spelling) error = %v", err)
	}

	manifest = validReleaseManifest()
	manifest.RuntimePacks[0].Runtime = "different-runtime"
	if err := Validate(&manifest, options); err == nil || !strings.Contains(err.Error(), "runtime must be \"onnxruntime\"") {
		t.Fatalf("Validate(unsupported runtime) error = %v", err)
	}

	manifest = validReleaseManifest()
	if err := Validate(&manifest, options); err != nil {
		t.Fatalf("Validate(valid release) error = %v", err)
	}
}

func TestValidateRequiresBoundedArtifactSizes(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		size int64
	}{
		{name: "missing", size: 0},
		{name: "negative", size: -1},
		{name: "oversized", size: MaxArtifactSizeBytes + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := validReleaseManifest()
			artifact := manifest.Models[0].Artifacts["default"]
			artifact.SizeBytes = test.size
			manifest.Models[0].Artifacts["default"] = artifact
			if err := Validate(&manifest, ValidationOptions{}); err == nil || !strings.Contains(err.Error(), "sizeBytes") {
				t.Fatalf("Validate(sizeBytes=%d) error = %v, want bounded-size error", test.size, err)
			}
		})
	}

	manifest := validReleaseManifest()
	artifact := manifest.RuntimePacks[0].Artifacts["linux-amd64"]
	artifact.SizeBytes = MaxArtifactSizeBytes
	manifest.RuntimePacks[0].Artifacts["linux-amd64"] = artifact
	if err := Validate(&manifest, ValidationOptions{}); err != nil {
		t.Fatalf("Validate(maximum sizeBytes) error = %v", err)
	}
}

func TestValidateRejectsArtifactPlatformCollisionsAfterNormalization(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		set  func(*Manifest)
	}{
		{
			name: "model",
			set: func(manifest *Manifest) {
				artifact := manifest.Models[0].Artifacts["default"]
				manifest.Models[0].Artifacts = map[string]Artifact{
					"linux-amd64":   artifact,
					" linux-amd64 ": artifact,
				}
			},
		},
		{
			name: "runtime pack",
			set: func(manifest *Manifest) {
				artifact := manifest.RuntimePacks[0].Artifacts["linux-amd64"]
				manifest.RuntimePacks[0].Artifacts = map[string]Artifact{
					"linux-amd64":   artifact,
					" linux-amd64 ": artifact,
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := validReleaseManifest()
			test.set(&manifest)
			err := Validate(&manifest, ValidationOptions{})
			if err == nil || !strings.Contains(err.Error(), `artifact platform "linux-amd64" is duplicated after normalization`) {
				t.Fatalf("Validate(platform collision) error = %v, want normalized duplicate error", err)
			}
		})
	}
}

func validReleaseManifest() Manifest {
	modelArtifact := Artifact{
		Filename:  "model.zip",
		URL:       "https://example.invalid/model.zip",
		SHA256:    testSHA256,
		SizeBytes: 1,
	}
	runtimeArtifact := Artifact{
		Filename:  "runtime.zip",
		URL:       "https://example.invalid/runtime.zip",
		SHA256:    testSHA256,
		SizeBytes: 1,
	}
	return Manifest{
		SchemaVersion:          1,
		Product:                Product,
		Recommended:            "model",
		RecommendedRuntimePack: "runtime",
		Models: []Model{{
			ID:            "model",
			Name:          "Model",
			Version:       "2026.07",
			VectorSpaceID: "model/d4",
			EmbeddingDim:  4,
			InputKind:     "image",
			Runtime:       "onnxruntime",
			Artifacts:     map[string]Artifact{"default": modelArtifact},
		}},
		RuntimePacks: []RuntimePack{{
			ID:        "runtime",
			Name:      "Runtime",
			Version:   "2026.07",
			Runtime:   "onnxruntime",
			Artifacts: map[string]Artifact{"linux-amd64": runtimeArtifact},
		}},
	}
}
