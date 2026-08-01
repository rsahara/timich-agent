//go:build !linux

package runtime

func populatePlatformCPUResource(cpu *SystemCPUResource) {
	cpu.Message = "CPU load is unavailable on this platform."
}

func populatePlatformMemoryResource(memory *SystemMemoryResource) {
	memory.Message = "Memory usage is unavailable on this platform."
}
