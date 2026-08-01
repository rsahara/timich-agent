//go:build unix

package catalog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var errLocalRootChildUnavailable = errors.New("local root child is unavailable")

// openLocalRootFile opens a regular file relative to a configured Local root
// without following symlinks in any path component. Keeping the returned file
// open pins the object that was validated, so callers must use this descriptor
// for hashing, rendering, or serving instead of reopening the pathname.
func openLocalRootFile(rootPath string, relativePath string) (*os.File, os.FileInfo, error) {
	components, err := localRootRelativeComponents(relativePath)
	if err != nil {
		return nil, nil, err
	}
	rootAbs, err := filepath.Abs(strings.TrimSpace(rootPath))
	if err != nil {
		return nil, nil, fmt.Errorf("resolve local root path: %w", err)
	}
	rootFD, err := unix.Open(rootAbs, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open local root: %w", err)
	}
	file, info, openErr := openLocalRootFileAt(rootFD, components, relativePath)
	_ = unix.Close(rootFD)
	return file, info, openErr
}

func openLocalRootFileFromPinnedRoot(root *os.Root, relativePath string) (*os.File, os.FileInfo, error) {
	if root == nil {
		return nil, nil, fmt.Errorf("open pinned local root child: root is nil")
	}
	components, err := localRootRelativeComponents(relativePath)
	if err != nil {
		return nil, nil, err
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, nil, fmt.Errorf("open pinned local root: %w", err)
	}
	defer directory.Close()
	return openLocalRootFileAt(int(directory.Fd()), components, relativePath)
}

func openLocalRootFileAt(rootFD int, components []string, relativePath string) (*os.File, os.FileInfo, error) {
	currentFD := rootFD
	for index, component := range components {
		last := index == len(components)-1
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if !last {
			flags |= unix.O_DIRECTORY
		}
		nextFD, openErr := unix.Openat(currentFD, component, flags, 0)
		if currentFD != rootFD {
			_ = unix.Close(currentFD)
		}
		if openErr != nil {
			return nil, nil, fmt.Errorf("%w: open without symlinks: %w", errLocalRootChildUnavailable, openErr)
		}
		currentFD = nextFD
	}
	file := os.NewFile(uintptr(currentFD), filepath.Base(relativePath))
	if file == nil {
		_ = unix.Close(currentFD)
		return nil, nil, fmt.Errorf("open local root child: invalid file descriptor")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("inspect local root child: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w: not a regular file", errLocalRootChildUnavailable)
	}
	return file, info, nil
}

func localRootRelativeComponents(relativePath string) ([]string, error) {
	relativePath = filepath.Clean(filepath.FromSlash(strings.TrimSpace(relativePath)))
	if relativePath == "." || relativePath == "" || filepath.IsAbs(relativePath) {
		return nil, fmt.Errorf("local child path is invalid")
	}
	components := strings.Split(relativePath, string(os.PathSeparator))
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, fmt.Errorf("local child path escapes root")
		}
	}
	return components, nil
}
