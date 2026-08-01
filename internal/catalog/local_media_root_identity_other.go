//go:build !unix && !windows

package catalog

import "os"

func localMediaRootIdentity(_ string, _ os.FileInfo) (string, error) {
	return "", ErrLocalMediaRootIdentityUnavailable
}
