package catalog

import (
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/tphakala/simd/cpu"
)

var semanticDotBenchmarkSink float32

func TestSemanticDotMatchesScalarReference(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		left  int
		right int
	}{
		{name: "empty", left: 0, right: 0},
		{name: "single", left: 1, right: 1},
		{name: "short SIMD tail", left: 7, right: 7},
		{name: "unequal left", left: 13, right: 9},
		{name: "unequal right", left: 9, right: 13},
		{name: "semantic embedding", left: 768, right: 768},
		{name: "semantic embedding with tail", left: 769, right: 769},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			left := semanticDotTestVector(test.left, 0x12345678)
			right := semanticDotTestVector(test.right, 0x9abcdef0)
			got := semanticDot(left, right)
			want := semanticDotScalarReference(left, right)
			tolerance := float32(1e-5) * max(float32(1), float32(math.Abs(float64(want))))
			if difference := float32(math.Abs(float64(got - want))); difference > tolerance {
				t.Fatalf("semanticDot() = %.9g, scalar = %.9g, difference %.9g exceeds tolerance %.9g", got, want, difference, tolerance)
			}
		})
	}
}

func TestSemanticDotDoesNotAllocate(t *testing.T) {
	left := semanticDotTestVector(768, 0x12345678)
	right := semanticDotTestVector(768, 0x9abcdef0)
	if allocations := testing.AllocsPerRun(1000, func() {
		_ = semanticDot(left, right)
	}); allocations != 0 {
		t.Fatalf("semanticDot() allocations = %v, want 0", allocations)
	}
}

func TestSemanticDotBackendIsReported(t *testing.T) {
	if backend := semanticDotBackend(768); backend == "" {
		t.Fatal("semanticDotBackend() is empty")
	}
	if got, want := semanticDotScoringFingerprint(768), semanticDotScoringVersion+"/"+semanticDotBackend(768); got != want {
		t.Fatalf("semanticDotScoringFingerprint() = %q, want %q", got, want)
	}
}

func TestSemanticDotBackendMatchesSIMDDispatch(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name         string
		goarch       string
		vectorLength int
		x86          cpu.Features
		arm64        cpu.Features
		want         string
	}{
		{
			name: "amd64 AVX-512", goarch: "amd64", vectorLength: 768,
			x86:  cpu.Features{AVX512F: true, AVX512VL: true, AVX: true, FMA: true, SSE2: true},
			want: "amd64-avx512",
		},
		{
			name: "amd64 AVX and FMA", goarch: "amd64", vectorLength: 768,
			x86:  cpu.Features{AVX: true, FMA: true, SSE2: true},
			want: "amd64-avx-fma",
		},
		{
			name: "amd64 AVX without FMA falls back to SSE2", goarch: "amd64", vectorLength: 768,
			x86:  cpu.Features{AVX: true, SSE2: true},
			want: "amd64-sse2",
		},
		{
			name: "amd64 scalar", goarch: "amd64", vectorLength: 768,
			want: "go-amd64",
		},
		{
			name: "arm64 NEON", goarch: "arm64", vectorLength: 768,
			arm64: cpu.Features{NEON: true},
			want:  "arm64-neon",
		},
		{
			name: "arm64 short vector falls back to Go", goarch: "arm64", vectorLength: 3,
			arm64: cpu.Features{NEON: true},
			want:  "go-arm64",
		},
		{
			name: "other architecture", goarch: "riscv64", vectorLength: 768,
			want: "go-riscv64",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := semanticDotBackendFor(test.goarch, test.vectorLength, test.x86, test.arm64); got != test.want {
				t.Fatalf("semanticDotBackendFor() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSemanticDotBackendHonorsSIMDDisable(t *testing.T) {
	const (
		helperEnv   = "TIMICH_TEST_SEMANTIC_DOT_DISABLE_HELPER"
		expectedEnv = "TIMICH_TEST_SEMANTIC_DOT_EXPECTED_BACKEND"
	)
	if os.Getenv(helperEnv) == "1" {
		if got, want := semanticDotBackend(768), os.Getenv(expectedEnv); got != want {
			t.Fatalf("semanticDotBackend() = %q, want %q", got, want)
		}
		return
	}

	type disableTest struct {
		name     string
		disable  string
		expected string
	}
	tests := []disableTest{{name: "all", disable: "all", expected: "go-" + runtime.GOARCH}}
	switch runtime.GOARCH {
	case "amd64":
		tests = append(tests, disableTest{name: "AVX-512 and FMA", disable: "avx512,fma", expected: "amd64-sse2"})
	case "arm64":
		tests = append(tests, disableTest{name: "NEON", disable: "neon", expected: "go-arm64"})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestSemanticDotBackendHonorsSIMDDisable$")
			command.Env = make([]string, 0, len(os.Environ())+3)
			for _, value := range os.Environ() {
				if !strings.HasPrefix(value, "SIMD_DISABLE=") &&
					!strings.HasPrefix(value, helperEnv+"=") &&
					!strings.HasPrefix(value, expectedEnv+"=") {
					command.Env = append(command.Env, value)
				}
			}
			command.Env = append(command.Env,
				helperEnv+"=1",
				expectedEnv+"="+test.expected,
				"SIMD_DISABLE="+test.disable,
			)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("SIMD_DISABLE=%s helper error = %v\n%s", test.disable, err, output)
			}
		})
	}
}

func BenchmarkSemanticDot768(b *testing.B) {
	left := semanticDotTestVector(768, 0x12345678)
	right := semanticDotTestVector(768, 0x9abcdef0)
	bytesPerDot := int64((len(left) + len(right)) * 4)

	b.Run("simd", func(b *testing.B) {
		var result float32
		b.ReportAllocs()
		b.SetBytes(bytesPerDot)
		b.ResetTimer()
		for range b.N {
			result = semanticDot(left, right)
		}
		semanticDotBenchmarkSink = result
	})
	b.Run("scalar", func(b *testing.B) {
		var result float32
		b.ReportAllocs()
		b.SetBytes(bytesPerDot)
		b.ResetTimer()
		for range b.N {
			result = semanticDotScalarReference(left, right)
		}
		semanticDotBenchmarkSink = result
	})
}

func semanticDotScalarReference(left []float32, right []float32) float32 {
	limit := min(len(left), len(right))
	var sum float32
	for index := 0; index < limit; index++ {
		sum += left[index] * right[index]
	}
	return sum
}

func semanticDotTestVector(length int, seed uint32) []float32 {
	result := make([]float32, length)
	state := seed
	for index := range result {
		state = state*1664525 + 1013904223
		result[index] = float32(int32(state>>8)%2001-1000) / 1000
	}
	return result
}
