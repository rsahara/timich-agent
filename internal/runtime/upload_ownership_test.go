//go:build unix

package runtime

import (
	"bytes"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/rsahara/timich-agent/internal/config"
)

func uploadOwnerTestRoot(t *testing.T) config.UploadRootConfig {
	t.Helper()
	account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		account, err = user.Lookup("nobody")
		if err != nil {
			t.Fatal(err)
		}
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	return config.UploadRootConfig{Key: "uploads", Path: t.TempDir(), TempPath: ".timich-upload-tmp", OwnerUser: account.Username, OwnerGroup: group.Name}
}

func assertUploadOwner(t *testing.T, filename string, owner uploadOwner) {
	t.Helper()
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != owner.uid || int(stat.Gid) != owner.gid {
		t.Fatalf("owner=%d:%d, want %d:%d", stat.Uid, stat.Gid, owner.uid, owner.gid)
	}
}

func TestUploadOwnershipTempResumeAndFinalize(t *testing.T) {
	root := uploadOwnerTestRoot(t)
	owner, err := resolveUploadOwner(root)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root.Path, "phone", "2026-10-11")
	untouched := filepath.Join(root.Path, "existing.jpg")
	if err := os.WriteFile(untouched, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(untouched)
	if err != nil {
		t.Fatal(err)
	}
	temp := ".timich-upload-tmp/upl_owner.part"
	if _, _, err := appendUploadChunkToTemp(root, temp, 0, bytes.NewBufferString("first"), 5); err != nil {
		t.Fatal(err)
	}
	assertUploadOwner(t, filepath.Join(root.Path, temp), owner)
	assertUploadOwner(t, filepath.Join(root.Path, root.TempPath), owner)
	if _, _, err := appendUploadChunkToTemp(root, temp, 5, bytes.NewBufferString("last"), 4); err != nil {
		t.Fatal(err)
	}
	final := "phone/2026-10-11/image.jpg"
	if err := finalizeUploadFile(root, temp, final); err != nil {
		t.Fatal(err)
	}
	assertUploadOwner(t, filepath.Join(root.Path, final), owner)
	assertUploadOwner(t, parent, owner)
	assertUploadOwner(t, filepath.Join(root.Path, "phone"), owner)
	content, err := os.ReadFile(filepath.Join(root.Path, final))
	if err != nil || string(content) != "firstlast" {
		t.Fatalf("final content=%q, err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(root.Path, temp)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp remains: %v", err)
	}
	after, err := os.Stat(untouched)
	if err != nil {
		t.Fatal(err)
	}
	if before.Sys().(*syscall.Stat_t).Uid != after.Sys().(*syscall.Stat_t).Uid || before.Mode() != after.Mode() {
		t.Fatal("unrelated existing media was changed")
	}
}

func TestUploadOwnershipPreservesExistingParents(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise parents with a different owner")
	}
	for _, test := range []struct {
		name         string
		mode         os.FileMode
		newFinalLeaf bool
	}{
		{name: "existing_private_parents", mode: 0o755},
		{name: "existing_shared_parents", mode: 0o777},
		{name: "new_child_of_existing_parent", mode: 0o755, newFinalLeaf: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := uploadOwnerTestRoot(t)
			root.TempPath = "working/.timich-upload-tmp"
			owner, err := resolveUploadOwner(root)
			if err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(root.Path, "phone", "date")
			tempDir := filepath.Join(root.Path, root.TempPath)
			for _, directory := range []string{parent, tempDir} {
				if err := os.MkdirAll(directory, test.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(directory, test.mode); err != nil {
					t.Fatal(err)
				}
			}
			existing := filepath.Join(parent, "existing.jpg")
			if err := os.WriteFile(existing, []byte("existing"), 0o644); err != nil {
				t.Fatal(err)
			}
			before := make(map[string]os.FileInfo)
			for _, name := range []string{root.Path, filepath.Join(root.Path, "working"), tempDir, filepath.Join(root.Path, "phone"), parent, existing} {
				info, err := os.Stat(name)
				if err != nil {
					t.Fatal(err)
				}
				if uploadOwnerMatches(info, owner) {
					t.Fatal("fixture must use an owner different from the configured owner")
				}
				before[name] = info
			}
			status := uploadRootStatus(root)
			if status.Status != "ready" || !status.Writable {
				t.Fatalf("existing parent owner blocked writable storage: %+v", status)
			}
			temp := root.TempPath + "/upl_existing.part"
			if _, _, err := appendUploadChunkToTemp(root, temp, 0, bytes.NewBufferString("new"), 3); err != nil {
				t.Fatal(err)
			}
			final := "phone/date/new.jpg"
			if test.newFinalLeaf {
				final = "phone/new-date/new.jpg"
			}
			if err := finalizeUploadFile(root, temp, final); err != nil {
				t.Fatal(err)
			}
			assertUploadOwner(t, filepath.Join(root.Path, final), owner)
			if test.newFinalLeaf {
				assertUploadOwner(t, filepath.Dir(filepath.Join(root.Path, final)), owner)
			}
			// Exercise the EEXIST branch used when another session creates a parent.
			if err := createUploadDirectory(root, parent); err != nil {
				t.Fatal(err)
			}
			for name, original := range before {
				assertUploadOwner(t, name, uploadOwner{uid: int(original.Sys().(*syscall.Stat_t).Uid), gid: int(original.Sys().(*syscall.Stat_t).Gid)})
				current, err := os.Stat(name)
				if err != nil || current.Mode() != original.Mode() {
					t.Fatalf("existing path mode changed: %s, %v", name, err)
				}
			}
			content, err := os.ReadFile(existing)
			if err != nil || string(content) != "existing" {
				t.Fatalf("existing media changed: %q, %v", content, err)
			}
		})
	}
}

func TestUploadOwnershipUnknownAccountsBlockWithoutPreparingTemp(t *testing.T) {
	for _, root := range []config.UploadRootConfig{
		{Key: "user", Path: t.TempDir(), OwnerUser: "timich-no-such-upload-user"},
		{Key: "group", Path: t.TempDir(), OwnerGroup: "timich-no-such-upload-group"},
	} {
		status := uploadRootStatus(root)
		if status.Status != "blocked" || status.Writable || !strings.Contains(status.Message, "ownership is unavailable") {
			t.Fatalf("unexpected status: %+v", status)
		}
		entries, err := os.ReadDir(root.Path)
		if err != nil || len(entries) != 0 {
			t.Fatalf("invalid ownership wrote files: %v, %v", entries, err)
		}
	}
}

func TestUploadOwnershipPermissionFailureBlocksRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged process")
	}
	account, err := user.LookupId("0")
	if err != nil {
		t.Fatal(err)
	}
	root := config.UploadRootConfig{Key: "denied", Path: t.TempDir(), TempPath: ".timich-upload-tmp", OwnerUser: account.Username}
	tempDir := filepath.Join(root.Path, root.TempPath)
	if err := os.Mkdir(tempDir, 0o755); err != nil {
		t.Fatal(err)
	}
	status := uploadRootStatus(root)
	if status.Status != "blocked" || status.Writable || !strings.Contains(status.Message, "ownership check failed") {
		t.Fatalf("unexpected status: %+v", status)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed probe leaked files: %v, %v", entries, err)
	}
}

func TestUploadOwnershipRequiresReadableDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged process to enforce directory read permissions")
	}
	for _, test := range []struct {
		name            string
		relativeDir     string
		readinessBlocks bool
	}{
		{name: "root", readinessBlocks: true},
		{name: "temp", relativeDir: ".timich-upload-tmp", readinessBlocks: true},
		{name: "final_parent", relativeDir: "phone/date"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := uploadOwnerTestRoot(t)
			for _, relative := range []string{root.TempPath, "phone/date"} {
				if err := os.MkdirAll(filepath.Join(root.Path, relative), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			restricted := filepath.Join(root.Path, test.relativeDir)
			restoreAccess := func() {
				if err := os.Chmod(restricted, 0o755); err != nil {
					t.Error(err)
				}
			}
			defer restoreAccess()
			if err := os.Chmod(restricted, 0o333); err != nil {
				t.Fatal(err)
			}
			// This is writable storage, but it cannot be opened for directory reads.
			ordinary := filepath.Join(restricted, "ordinary-file")
			if err := os.WriteFile(ordinary, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(ordinary); err != nil {
				t.Fatal(err)
			}
			status := uploadRootStatus(root)
			if test.readinessBlocks {
				if status.Status != "blocked" || status.Writable || !strings.Contains(status.Message, "ownership check failed") {
					t.Fatalf("unexpected unreadable-directory status: %+v", status)
				}
				restoreAccess()
				status = uploadRootStatus(root)
			}
			if status.Status != "ready" || !status.Writable {
				t.Fatalf("readable root/temp should be ready: %+v", status)
			}
			temp := root.TempPath + "/upl_read_access.part"
			if _, _, err := appendUploadChunkToTemp(root, temp, 0, bytes.NewBufferString("media"), 5); err != nil {
				t.Fatal(err)
			}
			final := "phone/date/image.jpg"
			if !test.readinessBlocks {
				if err := finalizeUploadFile(root, temp, final); !errors.Is(err, os.ErrPermission) {
					t.Fatalf("expected final-parent read failure, got %v", err)
				}
				restoreAccess()
			}
			if err := finalizeUploadFile(root, temp, final); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(root.Path, final))
			if err != nil || string(content) != "media" {
				t.Fatalf("final content=%q, err=%v", content, err)
			}
			for _, directory := range []string{root.Path, filepath.Join(root.Path, root.TempPath)} {
				probes, err := filepath.Glob(filepath.Join(directory, ".timich-upload-*-check-*"))
				if err != nil || len(probes) != 0 {
					t.Fatalf("probe files remain: %v, %v", probes, err)
				}
			}
		})
	}
}

func TestUploadOwnershipRejectsSymlinkAndTraversal(t *testing.T) {
	root := uploadOwnerTestRoot(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root.Path, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := applyUploadChildOwnership(root, "linked", true); !errors.Is(err, ErrUploadPolicyInvalid) {
		t.Fatalf("symlink accepted: %v", err)
	}
	if err := applyUploadChildOwnership(root, "../", true); err == nil {
		t.Fatal("root traversal accepted")
	}
}

func TestUploadOwnershipDefaultsAndPartialNames(t *testing.T) {
	owner, err := resolveUploadOwner(config.UploadRootConfig{})
	if err != nil || owner.uid != -1 || owner.gid != -1 {
		t.Fatalf("default=%+v, %v", owner, err)
	}
	root := uploadOwnerTestRoot(t)
	root.OwnerGroup = ""
	owner, err = resolveUploadOwner(root)
	if err != nil || owner.uid < 0 || owner.gid != -1 {
		t.Fatalf("user-only=%+v, %v", owner, err)
	}
	group, err := user.LookupGroupId(strconv.Itoa(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	root.OwnerGroup, root.OwnerUser = " "+group.Name+" ", ""
	owner, err = resolveUploadOwner(root)
	if err != nil || owner.uid != -1 || owner.gid != os.Getegid() {
		t.Fatalf("group-only=%+v, %v", owner, err)
	}
}
