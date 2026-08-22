package catalog

import (
	"runtime"

	"github.com/tphakala/simd/cpu"
	"github.com/tphakala/simd/f32"
)

const semanticDotScoringVersion = "tphakala-simd-v1.8.0-f32-dot-v1"

// semanticDot keeps semantic scoring portable while allowing the SIMD package
// to select the best safe implementation for the current CPU. The backend uses
// SSE2 or newer instructions on amd64, NEON on arm64, and a pure-Go fallback on
// unsupported architectures. DotProduct preserves the existing min-length
// behavior for mismatched slices.
func semanticDot(left []float32, right []float32) float32 {
	return f32.DotProduct(left, right)
}

// semanticDotBackend mirrors tphakala/simd v1.8.0's f32 DotProduct dispatch.
// Keep semanticDotScoringVersion and these predicates in sync when upgrading
// the dependency so an interrupted graph never resumes with mixed scoring.
func semanticDotBackend(vectorLength int) string {
	return semanticDotBackendFor(runtime.GOARCH, vectorLength, cpu.X86, cpu.ARM64)
}

func semanticDotBackendFor(goarch string, vectorLength int, x86 cpu.Features, arm64 cpu.Features) string {
	switch goarch {
	case "amd64":
		switch {
		case x86.AVX512F && x86.AVX512VL:
			return "amd64-avx512"
		case x86.AVX && x86.FMA:
			return "amd64-avx-fma"
		case x86.SSE2:
			return "amd64-sse2"
		}
	case "arm64":
		if arm64.NEON && vectorLength >= 4 {
			return "arm64-neon"
		}
	}
	return "go-" + goarch
}

func semanticDotScoringFingerprint(vectorLength int) string {
	return semanticDotScoringVersion + "/" + semanticDotBackend(vectorLength)
}
