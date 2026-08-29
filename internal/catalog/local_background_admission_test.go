package catalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalMetadataBatchLeavesUnadmittedJobQueued(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "photo.jpg"), []byte("image"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "photo-2.jpg"), []byte("image-2"), 0o600); err != nil {
		t.Fatalf("WriteFile(second) error = %v", err)
	}
	service := newLocalPhase0TestService(t, rootPath)
	if _, err := service.RunLocalReconciliationScan(context.Background(), "1111111111111111"); err != nil {
		t.Fatalf("RunLocalReconciliationScan() error = %v", err)
	}

	foregroundActive := errors.New("foreground request active")
	result, err := service.RunLocalMetadataBatchWithOptions(context.Background(), 2, 1, LocalBackgroundBatchOptions{
		BeforeJob: func(context.Context) error { return foregroundActive },
	})
	if !errors.Is(err, foregroundActive) {
		t.Fatalf("RunLocalMetadataBatchWithOptions() error = %v, want %v", err, foregroundActive)
	}
	if result.ProcessedJobs != 0 {
		t.Fatalf("processed jobs = %d, want 0", result.ProcessedJobs)
	}
	assertLocalScanCount(t, service, `SELECT COUNT(*) FROM local_scan_jobs WHERE job_kind = 'metadata' AND status = 'queued'`, 2)

	resumed, err := service.RunLocalMetadataBatch(context.Background(), 2)
	if err != nil {
		t.Fatalf("RunLocalMetadataBatch(resumed) error = %v", err)
	}
	if resumed.ProcessedJobs != 2 || resumed.RegisteredAssets != 2 {
		t.Fatalf("resumed batch = %+v, want two registered assets", resumed)
	}
}
