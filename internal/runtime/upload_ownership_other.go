//go:build !unix

package runtime

import "os"

func uploadOwnerMatches(info os.FileInfo, owner uploadOwner) bool { return false }
