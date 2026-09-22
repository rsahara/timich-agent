package catalog

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCatalogSemanticMetadataCandidateBudgetFallsThroughToSemanticSearch(t *testing.T) {
	parent := context.Background()
	candidates, timedOut, err := catalogSemanticMetadataCandidatesWithinBudget(
		parent,
		10*time.Millisecond,
		func(ctx context.Context) ([]semanticScoredAsset, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)
	if err != nil {
		t.Fatalf("catalogSemanticMetadataCandidatesWithinBudget() error = %v", err)
	}
	if !timedOut {
		t.Fatal("catalogSemanticMetadataCandidatesWithinBudget() timedOut = false, want true")
	}
	if len(candidates) != 0 {
		t.Fatalf("catalogSemanticMetadataCandidatesWithinBudget() candidates = %#v, want none", candidates)
	}
	if err := parent.Err(); err != nil {
		t.Fatalf("metadata child deadline canceled parent context: %v", err)
	}
}

func TestCatalogSemanticMetadataCandidateBudgetPreservesParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	_, timedOut, err := catalogSemanticMetadataCandidatesWithinBudget(
		parent,
		time.Second,
		func(ctx context.Context) ([]semanticScoredAsset, error) {
			return nil, ctx.Err()
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("catalogSemanticMetadataCandidatesWithinBudget() error = %v, want context canceled", err)
	}
	if timedOut {
		t.Fatal("catalogSemanticMetadataCandidatesWithinBudget() timedOut = true for parent cancellation")
	}
}

func TestCatalogSemanticMetadataPromotionBudgetFallsThroughToSemanticSearch(t *testing.T) {
	parent := context.Background()
	matches, timedOut, err := catalogSemanticMetadataMatchKeysWithinBudget(
		parent,
		10*time.Millisecond,
		func(ctx context.Context) (map[string]struct{}, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)
	if err != nil {
		t.Fatalf("catalogSemanticMetadataMatchKeysWithinBudget() error = %v", err)
	}
	if !timedOut {
		t.Fatal("catalogSemanticMetadataMatchKeysWithinBudget() timedOut = false, want true")
	}
	if len(matches) != 0 {
		t.Fatalf("catalogSemanticMetadataMatchKeysWithinBudget() matches = %#v, want none", matches)
	}
	if err := parent.Err(); err != nil {
		t.Fatalf("metadata promotion child deadline canceled parent context: %v", err)
	}
}

func TestCatalogSemanticMetadataPromotionBudgetPreservesParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	_, timedOut, err := catalogSemanticMetadataMatchKeysWithinBudget(
		parent,
		time.Second,
		func(ctx context.Context) (map[string]struct{}, error) {
			return nil, ctx.Err()
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("catalogSemanticMetadataMatchKeysWithinBudget() error = %v, want context canceled", err)
	}
	if timedOut {
		t.Fatal("catalogSemanticMetadataMatchKeysWithinBudget() timedOut = true for parent cancellation")
	}
}

func TestCatalogSemanticMetadataPromotionExhaustedBudgetSkipsLoad(t *testing.T) {
	called := false
	matches, timedOut, err := catalogSemanticMetadataMatchKeysWithinBudget(
		context.Background(),
		0,
		func(context.Context) (map[string]struct{}, error) {
			called = true
			return map[string]struct{}{"unexpected": {}}, nil
		},
	)
	if err != nil {
		t.Fatalf("catalogSemanticMetadataMatchKeysWithinBudget() error = %v", err)
	}
	if !timedOut || called || len(matches) != 0 {
		t.Fatalf("exhausted promotion budget = matches:%#v timedOut:%t called:%t, want no load", matches, timedOut, called)
	}
}

func TestCatalogSemanticAutoMetadataRequestedRequiresThreeRunes(t *testing.T) {
	tests := []struct {
		name  string
		query string
		mode  string
		want  bool
	}{
		{name: "one rune place", query: "津", mode: QueryModeAuto, want: false},
		{name: "two rune concept", query: "泣く", mode: QueryModeAuto, want: false},
		{name: "two ascii runes", query: "ab", mode: QueryModeAuto, want: false},
		{name: "three rune place", query: "東京駅", mode: QueryModeAuto, want: true},
		{name: "three ascii runes", query: "abc", mode: QueryModeAuto, want: true},
		{name: "semantic mode", query: "Kyoto", mode: QueryModeSemantic, want: false},
		{name: "filename mode", query: "Kyoto", mode: QueryModeFilename, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, err := normalizeAssetSearchRequest(AssetSearchRequest{
				Collection: AssetCollectionRequest{
					Kind:  CollectionKindSearch,
					Query: &AssetSearchQuery{Text: test.query, Mode: test.mode},
				},
				Page: AssetSearchPageRequest{Index: 0, Size: 1},
			})
			if err != nil {
				t.Fatalf("normalizeAssetSearchRequest() error = %v", err)
			}
			if got := catalogSemanticAutoMetadataRequested(normalized); got != test.want {
				t.Fatalf("catalogSemanticAutoMetadataRequested() = %t, want %t", got, test.want)
			}
		})
	}
}
