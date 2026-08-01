package catalog

import (
	"context"
	"math"
	"time"
)

const (
	semanticProfileKindModelPack     = "modelPack"
	semanticInputKindImage           = "image"
	semanticModelRoleActive          = "active"
	semanticModelRoleCandidate       = "candidate"
	semanticModelRoleRecommended     = "recommended"
	semanticModelPackStatusAvailable = "available"

	semanticRuntimeStatusLoaded              = "loaded"
	semanticRuntimeStatusBlocked             = "blocked"
	semanticRuntimeArtifactAvailable         = "available"
	semanticRuntimeArtifactMissing           = "missing"
	semanticRuntimeLayoutReady               = "ready"
	semanticRuntimeLayoutMissing             = "missing"
	semanticRuntimeLayoutInvalid             = "invalid"
	semanticRuntimeLayoutUnsupported         = "unsupported"
	semanticRuntimeHelperReady               = "ready"
	semanticRuntimeHelperMissing             = "missing"
	semanticRuntimeHelperBlocked             = "blocked"
	semanticRuntimeHelperFailed              = "failed"
	semanticRuntimeHelperRejected            = "rejected"
	semanticRuntimeLoaderONNXRuntime         = "onnxruntime"
	semanticRuntimeLoaderSentenceCLIP        = "sentence-transformers-clip"
	semanticRuntimeLoaderTransformersSigLIP2 = "transformers-siglip2"
	semanticRuntimeMessageArtifactMissing    = "semantic_runtime_artifact_missing"
	semanticRuntimeMessageLayoutMissing      = "semantic_runtime_layout_missing"
	semanticRuntimeMessageLayoutInvalid      = "semantic_runtime_layout_invalid"
	semanticRuntimeMessageLayoutUnsupported  = "semantic_runtime_layout_unsupported"
	semanticRuntimeMessageLoaderMissing      = "semantic_runtime_loader_missing"
	semanticRuntimeMessageHelperLoaded       = "semantic_runtime_helper_loaded"
	semanticRuntimeMessageHelperMissing      = "semantic_runtime_helper_missing"
	semanticRuntimeMessageHelperBlocked      = "semantic_runtime_helper_blocked"
	semanticRuntimeMessageHelperFailed       = "semantic_runtime_helper_failed"
	semanticRuntimeMessageHelperRejected     = "semantic_runtime_helper_rejected"
	semanticRuntimeMessageUnsupported        = "semantic_runtime_unsupported"

	semanticBackfillStatusPending     = "pending"
	semanticBackfillStatusBackfilling = "backfilling"
	semanticBackfillStatusIndexing    = "indexing"
	semanticBackfillStatusReady       = "ready"
	semanticBackfillStatusUnavailable = "unavailable"

	semanticBackfillMessagePending     = "semantic_indexing_pending"
	semanticBackfillMessageIncomplete  = "semantic_indexing_incomplete"
	semanticBackfillMessageIndexing    = "semantic_indexing_index_incomplete"
	semanticBackfillMessageReady       = "semantic_indexing_ready"
	semanticBackfillMessageUnavailable = "semantic_indexing_unavailable"

	semanticIndexJobStatusQueued    = "queued"
	semanticIndexJobStatusRunning   = "running"
	semanticIndexJobStatusFailed    = "failed"
	semanticIndexJobStatusCompleted = "completed"
)

const (
	SemanticProfileKindModelPack     = semanticProfileKindModelPack
	SemanticInputKindImage           = semanticInputKindImage
	SemanticModelRoleActive          = semanticModelRoleActive
	SemanticModelRoleCandidate       = semanticModelRoleCandidate
	SemanticModelRoleRecommended     = semanticModelRoleRecommended
	SemanticModelPackStatusAvailable = semanticModelPackStatusAvailable

	SemanticBackfillStatusPending     = semanticBackfillStatusPending
	SemanticBackfillStatusBackfilling = semanticBackfillStatusBackfilling
	SemanticBackfillStatusIndexing    = semanticBackfillStatusIndexing
	SemanticBackfillStatusReady       = semanticBackfillStatusReady
	SemanticBackfillStatusUnavailable = semanticBackfillStatusUnavailable

	SemanticBackfillMessageUnavailable = semanticBackfillMessageUnavailable
)

type semanticEmbeddingProfile interface {
	ModelID() string
	VectorSpaceID() string
	EmbeddingDim() int
	ProfileKind() string
	InputKind() string
	ModelPackStatus() *SemanticModelPackStatus
	EmbedSemanticAsset(ctx context.Context, input semanticAssetEmbeddingInput) (semanticEmbeddingResult, error)
	EmbedText(ctx context.Context, text string) ([]float32, error)
}

type semanticAssetEmbeddingInput struct {
	Asset semanticAsset
	Image *semanticImageEmbeddingInput
}

type semanticImageEmbeddingInput struct {
	Bytes       []byte
	ContentType string
	Source      string
}

type semanticEmbeddingResult struct {
	Vector []float32
	Input  string
}

// SemanticModelRegistryStatus exposes the local semantic profile registry.
type SemanticModelRegistryStatus struct {
	Active                 SemanticModelProfileStatus    `json:"active"`
	Recommended            *SemanticModelProfileStatus   `json:"recommended,omitempty"`
	RecommendedRuntimePack *SemanticRuntimePackStatus    `json:"recommendedRuntimePack,omitempty"`
	Candidate              *SemanticModelProfileStatus   `json:"candidate,omitempty"`
	Indexing               *SemanticModelBackfillStatus  `json:"indexing,omitempty"`
	IndexingWorker         *SemanticIndexingWorkerStatus `json:"indexingWorker,omitempty"`
	Profiles               []SemanticModelProfileStatus  `json:"profiles"`
	RuntimePacks           []SemanticRuntimePackStatus   `json:"runtimePacks,omitempty"`
	RegistryStatus         string                        `json:"registryStatus,omitempty"`
	RegistryMessage        string                        `json:"registryMessage,omitempty"`
	ManifestURL            string                        `json:"manifestUrl,omitempty"`
	MessageCode            string                        `json:"messageCode,omitempty"`
}

// SemanticModelProfileStatus summarizes one semantic embedding profile.
type SemanticModelProfileStatus struct {
	ModelID       string                      `json:"modelId"`
	VectorSpaceID string                      `json:"vectorSpaceId"`
	EmbeddingDim  int                         `json:"embeddingDim"`
	Role          string                      `json:"role,omitempty"`
	ProfileKind   string                      `json:"profileKind"`
	InputKind     string                      `json:"inputKind"`
	ModelPack     *SemanticModelPackStatus    `json:"modelPack,omitempty"`
	Runtime       *SemanticModelRuntimeStatus `json:"runtime,omitempty"`
}

// SemanticModelRuntimeStatus reports whether a semantic profile can currently embed.
type SemanticModelRuntimeStatus struct {
	Status         string `json:"status"`
	Runtime        string `json:"runtime,omitempty"`
	Loader         string `json:"loader,omitempty"`
	ArtifactStatus string `json:"artifactStatus,omitempty"`
	ArtifactFormat string `json:"artifactFormat,omitempty"`
	LayoutStatus   string `json:"layoutStatus,omitempty"`
	HelperStatus   string `json:"helperStatus,omitempty"`
	HelperProtocol int    `json:"helperProtocol,omitempty"`
	Loaded         bool   `json:"loaded"`
	CanEmbed       bool   `json:"canEmbed"`
	MessageCode    string `json:"messageCode,omitempty"`
}

// SemanticModelBackfillStatus summarizes semantic vector and search-index progress.
type SemanticModelBackfillStatus struct {
	Status                        string     `json:"status"`
	SourceKind                    string     `json:"sourceKind,omitempty"`
	ModelID                       string     `json:"modelId,omitempty"`
	VectorSpaceID                 string     `json:"vectorSpaceId,omitempty"`
	EmbeddingDim                  int        `json:"embeddingDim,omitempty"`
	EligibleAssetCount            int        `json:"eligibleAssetCount"`
	CompletedVectorCount          int        `json:"completedVectorCount"`
	IndexedVectorCount            int        `json:"indexedVectorCount"`
	RemainingVectorCount          int        `json:"remainingVectorCount"`
	PendingIndexJobCount          int        `json:"pendingIndexJobCount,omitempty"`
	FailedIndexJobCount           int        `json:"failedIndexJobCount,omitempty"`
	LastPublishedAt               *time.Time `json:"lastPublishedAt,omitempty"`
	MessageCode                   string     `json:"messageCode,omitempty"`
	EligibleNowVectorCount        int        `json:"-"`
	EligibleIndexJobCount         int        `json:"-"`
	NextEligibleAt                *time.Time `json:"-"`
	AssetGeneration               int64      `json:"-"`
	IndexedGeneration             int64      `json:"-"`
	GenerationMismatchSourceCount int        `json:"-"`
}

// SemanticIndexingWorkerStatus reports whether background semantic indexing is
// configured for the running Agent process.
type SemanticIndexingWorkerStatus struct {
	Enabled                bool   `json:"enabled"`
	Status                 string `json:"status"`
	IntervalSeconds        int64  `json:"intervalSeconds,omitempty"`
	BatchSize              int    `json:"batchSize,omitempty"`
	WorkerCount            int    `json:"workerCount,omitempty"`
	TargetCompletedVectors int    `json:"targetCompletedVectors,omitempty"`
	MessageCode            string `json:"messageCode,omitempty"`
}

type SemanticCandidateBackfillWorkerStatus = SemanticIndexingWorkerStatus

// SemanticModelRegistry returns the semantic profile registry visible to admin surfaces.
func SemanticModelRegistry() SemanticModelRegistryStatus {
	return SemanticModelRegistryStatus{Profiles: []SemanticModelProfileStatus{}}
}

func normalizeSemanticVector(vector []float32) []float32 {
	var sum float64
	for _, value := range vector {
		sum += float64(value * value)
	}
	if sum == 0 {
		return vector
	}
	scale := float32(1 / math.Sqrt(sum))
	for index := range vector {
		vector[index] *= scale
	}
	return vector
}
