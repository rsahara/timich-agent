package catalog

import (
	"context"
	"errors"
	"testing"
)

func TestSemanticBackfillChecksAdmissionBeforeEachAsset(t *testing.T) {
	stop := errors.New("foreground request active")
	admissions := 0
	assets := []semanticAsset{
		{SourceKey: "source-a", ID: "asset-a", Filename: "a.jpg"},
		{SourceKey: "source-a", ID: "asset-b", Filename: "b.jpg"},
		{SourceKey: "source-a", ID: "asset-c", Filename: "c.jpg"},
	}

	embedded, failures, err := (&CatalogStore{}).embedSemanticBackfillAssets(
		context.Background(),
		testImageSemanticProfile{},
		assets,
		SemanticBackfillOptions{
			ImageLoader: staticSemanticImageLoader{},
			Workers:     1,
			BeforeEmbed: func(context.Context) error {
				admissions++
				if admissions == 2 {
					return stop
				}
				return nil
			},
		},
	)
	if !errors.Is(err, stop) {
		t.Fatalf("embedSemanticBackfillAssets() error = %v, want %v", err, stop)
	}
	if admissions != 2 {
		t.Fatalf("admission calls = %d, want 2", admissions)
	}
	if embedded != nil || failures != nil {
		t.Fatalf("partial result = embedded %#v failures %#v, want nil on admission error", embedded, failures)
	}
}
