package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rsahara/timich-agent/internal/catalog"
)

func TestRunCLIGalleryMigrationRequiresExplicitPathsAndStop(t *testing.T) {
	for _, args := range [][]string{
		{}, {"--source", "/source"}, {"--output", "/output"},
		{"--source", "/source", "--output", "/output"},
		{"--source", "/source", "--output", "/output", "--confirm-agent-stopped", "unexpected"},
	} {
		var out, errOut bytes.Buffer
		if err := runCLI(append([]string{"pre-release-migrate-catalog-v4-v5"}, args...), &out, &errOut); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestRunCLIGalleryMigrationWritesVersionedJSON(t *testing.T) {
	original := migrateGalleryCatalog
	t.Cleanup(func() { migrateGalleryCatalog = original })
	migrateGalleryCatalog = func(ctx context.Context, source, output string) (catalog.CatalogGalleryMigrationResult, error) {
		if ctx == nil || source != "/source" || output != "/output" {
			t.Fatalf("bad args %s %s", source, output)
		}
		return catalog.CatalogGalleryMigrationResult{FromVersion: 4, ToVersion: 5, OutputPath: output, GalleryRows: 123}, nil
	}
	var out, errOut bytes.Buffer
	if err := runCLI([]string{"pre-release-migrate-catalog-v4-v5", "--source", "/source", "--output", "/output", "--confirm-agent-stopped"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	var result struct {
		catalog.CatalogGalleryMigrationResult
		AgentVersion string
		AgentCommit  string
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ToVersion != 5 || result.OutputPath != "/output" || result.GalleryRows != 123 || result.AgentVersion != version || result.AgentCommit != commit {
		t.Fatalf("%+v", result)
	}
}
