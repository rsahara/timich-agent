//go:build windows

package runtime

import (
	"math"

	"golang.org/x/sys/windows"
)

func filesystemAvailableBytes(path string) (int64, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var freeBytesAvailable uint64
	if err := windows.GetDiskFreeSpaceEx(path16, &freeBytesAvailable, nil, nil); err != nil {
		return 0, err
	}
	if freeBytesAvailable > uint64(math.MaxInt64) {
		return math.MaxInt64, nil
	}
	return int64(freeBytesAvailable), nil
}

func filesystemUsage(path string) (int64, int64, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeBytesAvailable uint64
	var totalBytes uint64
	if err := windows.GetDiskFreeSpaceEx(path16, &freeBytesAvailable, &totalBytes, nil); err != nil {
		return 0, 0, err
	}
	total := clampUint64ToInt64(totalBytes)
	available := clampUint64ToInt64(freeBytesAvailable)
	return total, available, nil
}

func clampUint64ToInt64(value uint64) int64 {
	if value > uint64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(value)
}
