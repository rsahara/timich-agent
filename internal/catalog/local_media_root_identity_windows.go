//go:build windows

package catalog

import (
	"fmt"
	"os"
	"syscall"
)

func localMediaRootIdentity(rootPath string, expected os.FileInfo) (string, error) {
	directory, err := os.Open(rootPath)
	if err != nil {
		return "", err
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return "", err
	}
	if expected == nil || !os.SameFile(expected, info) {
		return "", errLocalMediaRootChanged
	}
	var identity syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(directory.Fd()), &identity); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x:%x:%x", identity.VolumeSerialNumber, identity.FileIndexHigh, identity.FileIndexLow), nil
}
