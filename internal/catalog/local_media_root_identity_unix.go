//go:build unix

package catalog

import "os"

func localMediaRootIdentity(_ string, info os.FileInfo) (string, error) {
	identity := localFileIdentity(info)
	if identity == "" {
		return "", ErrLocalMediaRootIdentityUnavailable
	}
	return identity, nil
}
