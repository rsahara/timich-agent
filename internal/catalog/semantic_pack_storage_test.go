package catalog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestCopySemanticPackArtifactStopsAfterDeclaredSize(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	written, err := copySemanticPackArtifact(context.Background(), &output, bytes.NewReader([]byte("abcdef")), 3)
	if !errors.Is(err, errSemanticPackArtifactTooLarge) {
		t.Fatalf("copySemanticPackArtifact() error = %v, want size limit", err)
	}
	if written != 4 || output.Len() != 4 {
		t.Fatalf("copySemanticPackArtifact() wrote %d bytes (buffer %d), want expected size plus one", written, output.Len())
	}
}

func TestSemanticPackExpectedArtifactSizeRejectsOversizeDeclaration(t *testing.T) {
	t.Parallel()

	if _, err := semanticPackExpectedArtifactSize(semanticPackMaxArtifactBytes + 1); err == nil {
		t.Fatal("semanticPackExpectedArtifactSize() accepted oversized artifact")
	}
}

func TestSemanticPackExpectedArtifactSizeRequiresPositiveDeclaration(t *testing.T) {
	t.Parallel()

	if _, err := semanticPackExpectedArtifactSize(0); err == nil {
		t.Fatal("semanticPackExpectedArtifactSize() accepted unknown artifact size")
	}
	if got, err := semanticPackExpectedArtifactSize(123); err != nil || got != 123 {
		t.Fatalf("semanticPackExpectedArtifactSize() = %d, %v, want 123", got, err)
	}
	if _, err := copySemanticPackArtifact(context.Background(), io.Discard, bytes.NewReader(nil), 0); err == nil {
		t.Fatal("copySemanticPackArtifact() accepted unknown artifact size")
	}
}
