package catalog

import (
	"context"
	"errors"
	"testing"
)

func TestCachedSemanticTextProfileCachesByVectorSpaceAndText(t *testing.T) {
	t.Parallel()

	cache := newSemanticTextEmbeddingCache(4)
	base := &countingSemanticTextProfile{
		modelID:       "model-a",
		vectorSpaceID: "space-a",
		vector:        []float32{0.1, 0.2},
	}
	profile := cachedSemanticTextProfileFor(base, cache)

	first, err := profile.EmbedText(context.Background(), "flower")
	if err != nil {
		t.Fatalf("first EmbedText() error = %v", err)
	}
	first[0] = 9
	second, err := profile.EmbedText(context.Background(), "flower")
	if err != nil {
		t.Fatalf("second EmbedText() error = %v", err)
	}
	if base.calls != 1 {
		t.Fatalf("base calls = %d, want 1", base.calls)
	}
	if second[0] != 0.1 {
		t.Fatalf("cached vector was mutated: %v", second)
	}

	otherBase := &countingSemanticTextProfile{
		modelID:       "model-b",
		vectorSpaceID: "space-b",
		vector:        []float32{0.3, 0.4},
	}
	otherProfile := cachedSemanticTextProfileFor(otherBase, cache)
	other, err := otherProfile.EmbedText(context.Background(), "flower")
	if err != nil {
		t.Fatalf("other EmbedText() error = %v", err)
	}
	if otherBase.calls != 1 || other[0] != 0.3 {
		t.Fatalf("other profile calls=%d vector=%v, want independent cache entry", otherBase.calls, other)
	}
}

func TestCachedSemanticTextProfileDoesNotCacheErrors(t *testing.T) {
	t.Parallel()

	base := &countingSemanticTextProfile{
		modelID:       "model-a",
		vectorSpaceID: "space-a",
		err:           errors.New("embed failed"),
	}
	profile := cachedSemanticTextProfileFor(base, newSemanticTextEmbeddingCache(4))

	if _, err := profile.EmbedText(context.Background(), "flower"); err == nil {
		t.Fatal("first EmbedText() error = nil, want error")
	}
	base.err = nil
	base.vector = []float32{0.5, 0.6}
	vector, err := profile.EmbedText(context.Background(), "flower")
	if err != nil {
		t.Fatalf("second EmbedText() error = %v", err)
	}
	if base.calls != 2 || vector[0] != 0.5 {
		t.Fatalf("calls=%d vector=%v, want retry after error", base.calls, vector)
	}
}

type countingSemanticTextProfile struct {
	modelID       string
	vectorSpaceID string
	vector        []float32
	calls         int
	err           error
}

func (p *countingSemanticTextProfile) ModelID() string {
	return p.modelID
}

func (p *countingSemanticTextProfile) VectorSpaceID() string {
	return p.vectorSpaceID
}

func (p *countingSemanticTextProfile) EmbeddingDim() int {
	return len(p.vector)
}

func (p *countingSemanticTextProfile) ProfileKind() string {
	return semanticProfileKindModelPack
}

func (p *countingSemanticTextProfile) InputKind() string {
	return semanticInputKindImage
}

func (*countingSemanticTextProfile) ModelPackStatus() *SemanticModelPackStatus {
	return nil
}

func (p *countingSemanticTextProfile) EmbedSemanticAsset(context.Context, semanticAssetEmbeddingInput) (semanticEmbeddingResult, error) {
	return semanticEmbeddingResult{Vector: append([]float32(nil), p.vector...)}, p.err
}

func (p *countingSemanticTextProfile) EmbedText(context.Context, string) ([]float32, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return append([]float32(nil), p.vector...), nil
}
