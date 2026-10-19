package runtime

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/rsahara/timich-agent/internal/config"
)

type uploadOwner struct {
	uid int
	gid int
}

// Account mappings are stable for a serving process. Restart after changing them.
var uploadOwnerLookups sync.Map

func uploadOwnershipConfigured(root config.UploadRootConfig) bool {
	return strings.TrimSpace(root.OwnerUser) != "" || strings.TrimSpace(root.OwnerGroup) != ""
}

func resolveUploadOwner(root config.UploadRootConfig) (uploadOwner, error) {
	userName := strings.TrimSpace(root.OwnerUser)
	groupName := strings.TrimSpace(root.OwnerGroup)
	if userName == "" && groupName == "" {
		return uploadOwner{uid: -1, gid: -1}, nil
	}
	key := [2]string{userName, groupName}
	if cached, ok := uploadOwnerLookups.Load(key); ok {
		return cached.(func() (uploadOwner, error))()
	}
	lookup := sync.OnceValues(func() (uploadOwner, error) {
		owner := uploadOwner{uid: -1, gid: -1}
		if runtime.GOOS == "windows" {
			return owner, fmt.Errorf("upload ownership requires a Unix runtime")
		}
		if userName != "" {
			account, err := user.Lookup(userName)
			if err != nil {
				return owner, fmt.Errorf("resolve upload owner user %q: %w", userName, err)
			}
			owner.uid, err = strconv.Atoi(account.Uid)
			if err != nil || owner.uid < 0 {
				return owner, fmt.Errorf("upload owner user %q has an invalid UID", userName)
			}
		}
		if groupName != "" {
			group, err := user.LookupGroup(groupName)
			if err != nil {
				return owner, fmt.Errorf("resolve upload owner group %q: %w", groupName, err)
			}
			owner.gid, err = strconv.Atoi(group.Gid)
			if err != nil || owner.gid < 0 {
				return owner, fmt.Errorf("upload owner group %q has an invalid GID", groupName)
			}
		}
		return owner, nil
	})
	actual, _ := uploadOwnerLookups.LoadOrStore(key, lookup)
	return actual.(func() (uploadOwner, error))()
}

func applyUploadOwnership(file *os.File, root config.UploadRootConfig) error {
	owner, err := resolveUploadOwner(root)
	if err != nil {
		return err
	}
	if owner.uid == -1 && owner.gid == -1 {
		return nil
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if uploadOwnerMatches(info, owner) {
		return nil
	}
	if err := file.Chown(owner.uid, owner.gid); err != nil {
		return fmt.Errorf("apply upload ownership: %w", err)
	}
	return nil
}

// Pin the upload root and reject symlinks before changing a managed child.
func applyUploadChildOwnership(root config.UploadRootConfig, relativePath string, directory bool) error {
	if !uploadOwnershipConfigured(root) {
		return nil
	}
	file, err := openUploadChild(root, relativePath, os.O_RDONLY, directory)
	if err != nil {
		return err
	}
	defer file.Close()
	return applyUploadOwnership(file, root)
}

func openUploadChild(root config.UploadRootConfig, relativePath string, flags int, directory bool) (*os.File, error) {
	pinned, err := os.OpenRoot(root.Path)
	if err != nil {
		return nil, err
	}
	defer pinned.Close()
	info, err := pinned.Lstat(relativePath)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return nil, ErrUploadPolicyInvalid
	}
	file, err := pinned.OpenFile(relativePath, flags, 0)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !os.SameFile(info, opened) {
		file.Close()
		return nil, ErrUploadPolicyInvalid
	}
	return file, nil
}

func applyUploadDirectoryOwnership(root config.UploadRootConfig, directory string) error {
	if !uploadOwnershipConfigured(root) {
		return nil
	}
	relative, err := filepath.Rel(root.Path, directory)
	if err != nil {
		return err
	}
	return applyUploadChildOwnership(root, filepath.ToSlash(relative), true)
}
