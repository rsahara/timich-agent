package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/rsahara/timich-agent/internal/semanticmanifest"
)

const (
	semanticPackStorageReserveBytes = int64(1 << 30)
	semanticPackMaxArtifactBytes    = semanticmanifest.MaxArtifactSizeBytes
)

var (
	errSemanticPackArtifactTooLarge = errors.New("semantic pack artifact exceeds declared size")
	semanticPackAvailableBytes      = semanticPackFilesystemAvailableBytes
)

func semanticPackExpectedArtifactSize(artifactSize int64) (int64, error) {
	if artifactSize <= 0 {
		return 0, errors.New("semantic pack artifact size must be positive")
	}
	if artifactSize > semanticPackMaxArtifactBytes {
		return 0, fmt.Errorf("semantic pack artifact size %d exceeds limit %d", artifactSize, semanticPackMaxArtifactBytes)
	}
	return artifactSize, nil
}

func ensureSemanticPackStorage(path string, requiredBytes int64) error {
	if requiredBytes < 0 || requiredBytes > math.MaxInt64-semanticPackStorageReserveBytes {
		return errors.New("semantic pack storage requirement is invalid")
	}
	available, err := semanticPackAvailableBytes(path)
	if err != nil {
		return fmt.Errorf("inspect semantic pack storage: %w", err)
	}
	want := requiredBytes + semanticPackStorageReserveBytes
	if available < want {
		return fmt.Errorf("semantic pack storage has %d bytes available, need %d including reserve", available, want)
	}
	return nil
}

func copySemanticPackArtifact(ctx context.Context, writer io.Writer, reader io.Reader, expectedSize int64) (int64, error) {
	if expectedSize <= 0 || expectedSize > semanticPackMaxArtifactBytes {
		return 0, errors.New("semantic pack artifact size must be positive and within the supported limit")
	}
	written, err := copyWithContext(ctx, writer, io.LimitReader(reader, expectedSize+1))
	if err != nil {
		return written, err
	}
	if written > expectedSize {
		return written, errSemanticPackArtifactTooLarge
	}
	return written, nil
}
