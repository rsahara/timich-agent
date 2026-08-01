//go:build !unix

package catalog

import (
	"errors"
	"os"
)

var errLocalRootChildUnavailable = errors.New("local root child is unavailable")

func openLocalRootFile(string, string) (*os.File, os.FileInfo, error) {
	return nil, nil, errLocalRootChildUnavailable
}

func openLocalRootFileFromPinnedRoot(*os.Root, string) (*os.File, os.FileInfo, error) {
	return nil, nil, errLocalRootChildUnavailable
}
