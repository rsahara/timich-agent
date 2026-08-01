//go:build unix

package catalog

import (
	"fmt"
	"os"
	"syscall"
)

func localFileIdentity(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return ""
	}
	return fmt.Sprintf("%x:%x", uint64(stat.Dev), uint64(stat.Ino))
}
