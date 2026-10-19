//go:build unix

package runtime

import (
	"os"
	"syscall"
)

func uploadOwnerMatches(info os.FileInfo, owner uploadOwner) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (owner.uid == -1 || uint64(stat.Uid) == uint64(owner.uid)) &&
		(owner.gid == -1 || uint64(stat.Gid) == uint64(owner.gid))
}
