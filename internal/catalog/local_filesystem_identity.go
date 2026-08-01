package catalog

import (
	"os"
)

// reopenLocalRootFileAndMatch resolves the root-relative pathname again after
// long-running work. Comparing that new descriptor with the pinned descriptor
// detects atomic pathname replacement, which repeated Stat calls on the pinned
// descriptor cannot observe.
func reopenLocalRootFileAndMatch(rootPath string, relativePath string, pinned os.FileInfo) (*os.File, os.FileInfo, bool, error) {
	file, current, err := openLocalRootFile(rootPath, relativePath)
	if err != nil {
		return nil, nil, false, err
	}
	return file, current, localFileInfoUnchanged(pinned, current), nil
}

func reopenPinnedLocalRootFileAndMatch(root *os.Root, relativePath string, pinned os.FileInfo) (*os.File, os.FileInfo, bool, error) {
	file, current, err := openLocalRootFileFromPinnedRoot(root, relativePath)
	if err != nil {
		return nil, nil, false, err
	}
	return file, current, localFileInfoUnchanged(pinned, current), nil
}
