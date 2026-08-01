//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadLinuxCPUTemperatureFromRootsPrefersCPUSensors(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	thermalRoot := filepath.Join(root, "thermal")
	hwmonRoot := filepath.Join(root, "hwmon")
	writeTestFile(t, filepath.Join(thermalRoot, "thermal_zone0", "type"), "nvme\n")
	writeTestFile(t, filepath.Join(thermalRoot, "thermal_zone0", "temp"), "72000\n")
	writeTestFile(t, filepath.Join(thermalRoot, "thermal_zone1", "type"), "x86_pkg_temp\n")
	writeTestFile(t, filepath.Join(thermalRoot, "thermal_zone1", "temp"), "61000\n")
	writeTestFile(t, filepath.Join(hwmonRoot, "hwmon0", "name"), "coretemp\n")
	writeTestFile(t, filepath.Join(hwmonRoot, "hwmon0", "temp1_label"), "Package id 0\n")
	writeTestFile(t, filepath.Join(hwmonRoot, "hwmon0", "temp1_input"), "64000\n")
	writeTestFile(t, filepath.Join(hwmonRoot, "hwmon0", "temp2_label"), "Core 0\n")
	writeTestFile(t, filepath.Join(hwmonRoot, "hwmon0", "temp2_input"), "85000\n")
	writeTestFile(t, filepath.Join(hwmonRoot, "hwmon1", "name"), "nvme\n")
	writeTestFile(t, filepath.Join(hwmonRoot, "hwmon1", "temp1_input"), "79000\n")

	temperature, label, ok := readLinuxCPUTemperatureFromRoots(thermalRoot, hwmonRoot)
	if !ok {
		t.Fatal("readLinuxCPUTemperatureFromRoots() ok = false, want true")
	}
	if temperature != 64 {
		t.Fatalf("temperature = %v, want 64", temperature)
	}
	if label != "coretemp Package id 0" {
		t.Fatalf("label = %q, want coretemp Package id 0", label)
	}
}

func TestReadLinuxCPUTemperatureFromRootsIgnoresNonCPUSensors(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	thermalRoot := filepath.Join(root, "thermal")
	writeTestFile(t, filepath.Join(thermalRoot, "thermal_zone0", "type"), "nvme\n")
	writeTestFile(t, filepath.Join(thermalRoot, "thermal_zone0", "temp"), "72000\n")

	_, _, ok := readLinuxCPUTemperatureFromRoots(thermalRoot, "")
	if ok {
		t.Fatal("readLinuxCPUTemperatureFromRoots() ok = true, want false")
	}
}

func writeTestFile(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}
