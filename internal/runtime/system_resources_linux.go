//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

var linuxCPUSampleState struct {
	sync.Mutex
	previous linuxCPUSample
	ok       bool
}

func populatePlatformCPUResource(cpu *SystemCPUResource) {
	load1, load5, load15, err := readLinuxLoadAverage("/proc/loadavg")
	if err != nil {
		cpu.Message = "CPU load is unavailable."
	} else {
		cpu.Load1 = floatPointer(load1)
		cpu.Load5 = floatPointer(load5)
		cpu.Load15 = floatPointer(load15)
		if cpu.LogicalCores > 0 {
			cpu.Load1Percent = floatPointer((load1 / float64(cpu.LogicalCores)) * 100)
		}
	}
	if usage, iowait, ok := readLinuxCPUUsagePercent("/proc/stat"); ok {
		cpu.UsagePercent = floatPointer(usage)
		cpu.IOWaitPercent = floatPointer(iowait)
	}
	if temperature, label, ok := readLinuxCPUTemperature(); ok {
		cpu.TemperatureCelsius = floatPointer(temperature)
		cpu.TemperatureLabel = label
	}
}

func populatePlatformMemoryResource(memory *SystemMemoryResource) {
	total, available, err := readLinuxMemory("/proc/meminfo")
	if err != nil {
		memory.Message = "Memory usage is unavailable."
		return
	}
	memory.TotalBytes = total
	memory.AvailableBytes = available
	if total > 0 {
		memory.UsedBytes = total - available
		memory.UsedPercent = floatPointer((float64(memory.UsedBytes) / float64(total)) * 100)
	}
}

func readLinuxLoadAverage(path string) (float64, float64, float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return 0, 0, 0, strconv.ErrSyntax
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, 0, err
	}
	load5, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, 0, err
	}
	load15, err := strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return 0, 0, 0, err
	}
	return load1, load5, load15, nil
}

type linuxCPUSample struct {
	total  uint64
	idle   uint64
	iowait uint64
}

func readLinuxCPUUsagePercent(path string) (float64, float64, bool) {
	sample, err := readLinuxCPUSample(path)
	if err != nil {
		return 0, 0, false
	}
	linuxCPUSampleState.Lock()
	defer linuxCPUSampleState.Unlock()
	if !linuxCPUSampleState.ok {
		linuxCPUSampleState.previous = sample
		linuxCPUSampleState.ok = true
		return 0, 0, false
	}
	previous := linuxCPUSampleState.previous
	linuxCPUSampleState.previous = sample
	return linuxCPUUsagePercentFromSamples(previous, sample)
}

func linuxCPUUsagePercentFromSamples(previous, sample linuxCPUSample) (float64, float64, bool) {
	if sample.total <= previous.total {
		return 0, 0, false
	}
	totalDelta := sample.total - previous.total
	idleDelta := boundedUint64Delta(sample.idle, previous.idle)
	iowaitDelta := boundedUint64Delta(sample.iowait, previous.iowait)
	if totalDelta == 0 {
		return 0, 0, false
	}
	busyDelta := totalDelta
	if idleDelta+iowaitDelta < busyDelta {
		busyDelta -= idleDelta + iowaitDelta
	} else {
		busyDelta = 0
	}
	return clampPercent((float64(busyDelta) / float64(totalDelta)) * 100),
		clampPercent((float64(iowaitDelta) / float64(totalDelta)) * 100),
		true
}

func readLinuxCPUSample(path string) (linuxCPUSample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return linuxCPUSample{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[0] != "cpu" {
			continue
		}
		values := make([]uint64, 0, len(fields)-1)
		for _, field := range fields[1:] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return linuxCPUSample{}, err
			}
			values = append(values, value)
		}
		var total uint64
		for _, value := range values {
			total += value
		}
		return linuxCPUSample{
			total:  total,
			idle:   values[3],
			iowait: values[4],
		}, nil
	}
	return linuxCPUSample{}, strconv.ErrSyntax
}

func boundedUint64Delta(next, previous uint64) uint64 {
	if next <= previous {
		return 0
	}
	return next - previous
}

func clampPercent(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 100:
		return 100
	default:
		return value
	}
}

func readLinuxMemory(path string) (int64, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var total int64
	var available int64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		valueKiB, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			total = valueKiB * 1024
		case "MemAvailable":
			available = valueKiB * 1024
		}
	}
	if total <= 0 {
		return 0, 0, strconv.ErrSyntax
	}
	if available < 0 {
		available = 0
	}
	return total, available, nil
}

func readLinuxCPUTemperature() (float64, string, bool) {
	return readLinuxCPUTemperatureFromRoots("/sys/class/thermal", "/sys/class/hwmon")
}

func readLinuxCPUTemperatureFromRoots(thermalRoot, hwmonRoot string) (float64, string, bool) {
	type sensor struct {
		celsius float64
		label   string
		score   int
	}
	best := sensor{}
	consider := func(celsius float64, label string, score int) {
		if score <= 0 || celsius < -50 || celsius > 150 {
			return
		}
		if best.score == 0 || score > best.score || (score == best.score && celsius > best.celsius) {
			best = sensor{celsius: celsius, label: label, score: score}
		}
	}

	if thermalRoot != "" {
		if zones, err := filepath.Glob(filepath.Join(thermalRoot, "thermal_zone*")); err == nil {
			for _, zone := range zones {
				label := readTrimmedFile(filepath.Join(zone, "type"))
				celsius, err := readLinuxTemperatureFile(filepath.Join(zone, "temp"))
				if err != nil {
					continue
				}
				consider(celsius, label, cpuTemperatureScore(label))
			}
		}
	}

	if hwmonRoot != "" {
		if devices, err := filepath.Glob(filepath.Join(hwmonRoot, "hwmon*")); err == nil {
			for _, device := range devices {
				name := readTrimmedFile(filepath.Join(device, "name"))
				inputs, err := filepath.Glob(filepath.Join(device, "temp*_input"))
				if err != nil {
					continue
				}
				for _, input := range inputs {
					labelPath := strings.TrimSuffix(input, "_input") + "_label"
					label := strings.TrimSpace(strings.Join([]string{name, readTrimmedFile(labelPath)}, " "))
					celsius, err := readLinuxTemperatureFile(input)
					if err != nil {
						continue
					}
					consider(celsius, label, cpuTemperatureScore(label))
				}
			}
		}
	}

	if best.score == 0 {
		return 0, "", false
	}
	return best.celsius, strings.TrimSpace(best.label), true
}

func readLinuxTemperatureFile(path string) (float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	raw, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		return 0, err
	}
	switch {
	case raw > 1000:
		raw = raw / 1000
	case raw > 150:
		raw = raw / 10
	}
	return raw, nil
}

func readTrimmedFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func cpuTemperatureScore(label string) int {
	normalized := strings.ToLower(strings.TrimSpace(label))
	if normalized == "" {
		return 0
	}
	for _, blocked := range []string{"battery", "bat", "disk", "drive", "hdd", "nvme", "ssd", "wifi", "wireless"} {
		if strings.Contains(normalized, blocked) {
			return 0
		}
	}
	switch {
	case strings.Contains(normalized, "x86_pkg_temp") || strings.Contains(normalized, "package") || strings.Contains(normalized, "pkg"):
		return 100
	case strings.Contains(normalized, "k10temp") || strings.Contains(normalized, "zenpower"):
		return 90
	case strings.Contains(normalized, "cpu") || strings.Contains(normalized, "soc"):
		return 80
	case strings.Contains(normalized, "coretemp"):
		return 70
	case strings.Contains(normalized, "core"):
		return 60
	case strings.Contains(normalized, "acpitz"):
		return 10
	}
	if strings.Contains(normalized, "thermal") || strings.Contains(normalized, "temp") {
		return 1
	}
	return 0
}
