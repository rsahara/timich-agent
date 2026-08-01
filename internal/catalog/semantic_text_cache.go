package catalog

import (
	"context"
	"strings"
	"sync"
)

const semanticTextEmbeddingCacheSize = 64

type semanticTextEmbeddingCache struct {
	mu      sync.Mutex
	maxSize int
	order   []string
	values  map[string][]float32
}

type cachedSemanticTextProfile struct {
	base  semanticEmbeddingProfile
	cache *semanticTextEmbeddingCache
}

func newSemanticTextEmbeddingCache(maxSize int) *semanticTextEmbeddingCache {
	if maxSize < 1 {
		maxSize = semanticTextEmbeddingCacheSize
	}
	return &semanticTextEmbeddingCache{
		maxSize: maxSize,
		values:  make(map[string][]float32, maxSize),
	}
}

func (c *semanticTextEmbeddingCache) get(key string) ([]float32, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.values[key]
	if !ok {
		return nil, false
	}
	return append([]float32(nil), value...), true
}

func (c *semanticTextEmbeddingCache) put(key string, value []float32) {
	if c == nil || key == "" || len(value) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.values[key]; !exists {
		c.order = append(c.order, key)
	}
	c.values[key] = append([]float32(nil), value...)
	for len(c.order) > c.maxSize {
		evicted := c.order[0]
		c.order = c.order[1:]
		delete(c.values, evicted)
	}
}

func cachedSemanticTextProfileFor(base semanticEmbeddingProfile, cache *semanticTextEmbeddingCache) semanticEmbeddingProfile {
	if base == nil || cache == nil {
		return base
	}
	return cachedSemanticTextProfile{base: base, cache: cache}
}

func (p cachedSemanticTextProfile) ModelID() string {
	return p.base.ModelID()
}

func (p cachedSemanticTextProfile) VectorSpaceID() string {
	return p.base.VectorSpaceID()
}

func (p cachedSemanticTextProfile) EmbeddingDim() int {
	return p.base.EmbeddingDim()
}

func (p cachedSemanticTextProfile) ProfileKind() string {
	return p.base.ProfileKind()
}

func (p cachedSemanticTextProfile) InputKind() string {
	return p.base.InputKind()
}

func (p cachedSemanticTextProfile) ModelPackStatus() *SemanticModelPackStatus {
	return p.base.ModelPackStatus()
}

func (p cachedSemanticTextProfile) EmbedSemanticAsset(ctx context.Context, input semanticAssetEmbeddingInput) (semanticEmbeddingResult, error) {
	return p.base.EmbedSemanticAsset(ctx, input)
}

func (p cachedSemanticTextProfile) EmbedText(ctx context.Context, text string) ([]float32, error) {
	key := semanticTextEmbeddingCacheKey(p.base, text)
	if cached, ok := p.cache.get(key); ok {
		return cached, nil
	}
	vector, err := p.base.EmbedText(ctx, text)
	if err != nil {
		return nil, err
	}
	p.cache.put(key, vector)
	return append([]float32(nil), vector...), nil
}

func semanticTextEmbeddingCacheKey(profile semanticEmbeddingProfile, text string) string {
	if profile == nil {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(profile.VectorSpaceID()) + "\x00" + strings.TrimSpace(text)
}
