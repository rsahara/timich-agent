//go:build windows

package catalog

import (
	"math"

	"golang.org/x/sys/windows"
)

func semanticPackFilesystemAvailableBytes(path string) (int64, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(path16, &available, nil, nil); err != nil {
		return 0, err
	}
	if available > uint64(math.MaxInt64) {
		return math.MaxInt64, nil
	}
	return int64(available), nil
}
