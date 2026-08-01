//go:build linux

package runtime

import (
	"path/filepath"
	"testing"
)

func TestReadLinuxCPUSample(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "stat")
	writeTestFile(t, path, "cpu  100 2 30 400 50 1 2 3 4 5\ncpu0 1 2 3 4 5 6 7 8 9 10\n")

	sample, err := readLinuxCPUSample(path)
	if err != nil {
		t.Fatalf("readLinuxCPUSample() error = %v", err)
	}
	if sample.total != 597 || sample.idle != 400 || sample.iowait != 50 {
		t.Fatalf("sample = %+v, want total 597 idle 400 iowait 50", sample)
	}
}

func TestLinuxCPUUsagePercentFromSamples(t *testing.T) {
	t.Parallel()

	usage, iowait, ok := linuxCPUUsagePercentFromSamples(
		linuxCPUSample{total: 1000, idle: 600, iowait: 100},
		linuxCPUSample{total: 1200, idle: 650, iowait: 150},
	)
	if !ok {
		t.Fatal("linuxCPUUsagePercentFromSamples() ok = false, want true")
	}
	if usage != 50 {
		t.Fatalf("usage = %v, want 50", usage)
	}
	if iowait != 25 {
		t.Fatalf("iowait = %v, want 25", iowait)
	}
}
