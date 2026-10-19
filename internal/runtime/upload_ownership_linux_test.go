//go:build linux

package runtime

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/rsahara/timich-agent/internal/config"
)

func TestUploadOwnershipReadinessAfterChown(t *testing.T) {
	const childFlag = "TIMICH_UPLOAD_PROBE_TEST_CHILD"
	if os.Getenv(childFlag) != "1" {
		if os.Geteuid() != 0 {
			t.Skip("requires root to launch a child with only CAP_CHOWN")
		}
		required := uint64(1<<unix.CAP_CHOWN | 1<<unix.CAP_SETUID | 1<<unix.CAP_SETGID | 1<<unix.CAP_SETPCAP)
		if effectiveUploadTestCapabilities(t)&required != required {
			t.Skip("requires CHOWN, SETUID, SETGID and SETPCAP to launch the child")
		}
		agent, err := user.Lookup("daemon")
		if err != nil {
			t.Fatal(err)
		}
		uid, err := strconv.ParseUint(agent.Uid, 10, 32)
		if err != nil || uid == 0 {
			t.Fatalf("invalid non-root test UID: %q, %v", agent.Uid, err)
		}
		gid, err := strconv.ParseUint(agent.Gid, 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(executable, "-test.run=^TestUploadOwnershipReadinessAfterChown$", "-test.v")
		child.Env = append(os.Environ(), childFlag+"=1")
		child.SysProcAttr = &syscall.SysProcAttr{
			Credential:  &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
			AmbientCaps: []uintptr{unix.CAP_CHOWN},
		}
		output, err := child.CombinedOutput()
		if err != nil {
			t.Fatalf("limited-capability child failed: %v\n%s", err, output)
		}
		t.Logf("%s", output)
		return
	}

	capabilities := effectiveUploadTestCapabilities(t)
	if os.Geteuid() == 0 || capabilities != 1<<unix.CAP_CHOWN {
		t.Fatalf("expected non-root child with only CAP_CHOWN, uid=%d capabilities=%x", os.Geteuid(), capabilities)
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(strconv.Itoa(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		umask      int
		blocked    bool
		readDenied bool
	}{
		{name: "owner_only_write_is_blocked", umask: 0o022, blocked: true},
		{name: "shared_group_write_without_read_is_blocked", umask: 0o046, blocked: true, readDenied: true},
		{name: "shared_group_read_write_remains_ready", umask: 0o002},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := unix.Umask(test.umask)
			defer unix.Umask(previous)
			root := config.UploadRootConfig{Key: "uploads", Path: t.TempDir(), TempPath: ".timich-upload-tmp", OwnerUser: account.Username, OwnerGroup: group.Name}
			tempDir := filepath.Join(root.Path, root.TempPath)
			if err := os.Mkdir(tempDir, 0o777); err != nil {
				t.Fatal(err)
			}
			// Keep directory access available after chown to isolate file access.
			if err := os.Chmod(tempDir, 0o777); err != nil {
				t.Fatal(err)
			}
			if test.blocked {
				// Establish the failure that readiness must catch before uploads start.
				temp := root.TempPath + "/upl_denied.part"
				if _, _, err := appendUploadChunkToTemp(root, temp, 0, bytes.NewBufferString("first"), 5); err != nil {
					t.Fatal(err)
				}
				_, _, err := appendUploadChunkToTemp(root, temp, 5, bytes.NewBufferString("last"), 4)
				if test.readDenied {
					if err != nil {
						t.Fatalf("write-only group could not append: %v", err)
					}
					if _, _, err := sha1File(root, temp); !errors.Is(err, os.ErrPermission) {
						t.Fatalf("expected checksum read permission failure, got %v", err)
					}
				} else if !errors.Is(err, os.ErrPermission) {
					t.Fatalf("expected write-reopen permission failure, got %v", err)
				}
				if err := os.Remove(filepath.Join(root.Path, temp)); err != nil {
					t.Fatal(err)
				}
			}
			status := uploadRootStatus(root)
			if test.blocked {
				if status.Status != "blocked" || status.Writable || !strings.Contains(status.Message, "reopen upload file") {
					t.Fatalf("unexpected status: %+v", status)
				}
			} else {
				if status.Status != "ready" || !status.Writable {
					t.Fatalf("shared-group write was blocked: %+v", status)
				}
				temp := root.TempPath + "/upl_group.part"
				position := int64(0)
				for _, chunk := range []string{"first", "last"} {
					written, _, err := appendUploadChunkToTemp(root, temp, position, bytes.NewBufferString(chunk), int64(len(chunk)))
					if err != nil {
						t.Fatalf("append at %d: %v", position, err)
					}
					position += written
				}
				if size, _, err := sha1File(root, temp); err != nil || size != position {
					t.Fatalf("checksum read size=%d, want %d, err=%v", size, position, err)
				}
				final := "phone/date/image.jpg"
				if err := finalizeUploadFile(root, temp, final); err != nil {
					t.Fatal(err)
				}
				content, err := os.ReadFile(filepath.Join(root.Path, final))
				if err != nil || string(content) != "firstlast" {
					t.Fatalf("final content=%q, err=%v", content, err)
				}
				owner, err := resolveUploadOwner(root)
				if err != nil {
					t.Fatal(err)
				}
				assertUploadOwner(t, filepath.Join(root.Path, final), owner)
			}
			for _, directory := range []string{root.Path, tempDir} {
				probes, err := filepath.Glob(filepath.Join(directory, ".timich-upload-*-check-*"))
				if err != nil || len(probes) != 0 {
					t.Fatalf("probe files remain: %v, %v", probes, err)
				}
			}
		})
	}
	testUploadOwnershipStickyDirectories(t, account.Username, group.Name)
}

func testUploadOwnershipStickyDirectories(t *testing.T, ownerUser, ownerGroup string) {
	t.Helper()
	previous := unix.Umask(0o002)
	defer unix.Umask(previous)
	for _, test := range []struct {
		name       string
		stickyTemp bool
	}{
		{name: "sticky_root_remains_ready"},
		{name: "sticky_temp_is_blocked", stickyTemp: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := config.UploadRootConfig{Key: "uploads", Path: t.TempDir(), TempPath: ".timich-upload-tmp", OwnerUser: ownerUser, OwnerGroup: ownerGroup}
			tempDir := filepath.Join(root.Path, root.TempPath)
			if err := os.Mkdir(tempDir, 0o777); err != nil {
				t.Fatal(err)
			}
			stickyDir := root.Path
			if test.stickyTemp {
				stickyDir = tempDir
			}
			if err := os.Chmod(stickyDir, os.ModeSticky|0o777); err != nil {
				t.Fatal(err)
			}
			// A different directory owner makes unlink depend on file ownership.
			if err := os.Chown(stickyDir, 0, -1); err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Let TempDir cleanup remove files owned by the configured user.
				if err := os.Chown(stickyDir, os.Geteuid(), -1); err != nil {
					t.Error(err)
				}
			}()
			for attempt := 0; attempt < 2; attempt++ {
				status := uploadRootStatus(root)
				if test.stickyTemp {
					if status.Status != "blocked" || status.Writable || !strings.Contains(status.Message, "remove") {
						t.Errorf("expected temp removal failure: %+v", status)
					}
				} else if status.Status != "ready" || !status.Writable {
					t.Errorf("sticky root blocked usable storage: %+v", status)
				}
				for _, directory := range []string{root.Path, tempDir} {
					probes, err := filepath.Glob(filepath.Join(directory, ".timich-upload-*-check-*"))
					if err != nil || len(probes) != 0 {
						t.Errorf("probe files remain: %v, %v", probes, err)
					}
				}
			}
			info, err := os.Stat(stickyDir)
			if err != nil {
				t.Fatal(err)
			}
			if !uploadOwnerMatches(info, uploadOwner{uid: 0, gid: -1}) || info.Mode()&(os.ModeSticky|os.ModePerm) != os.ModeSticky|0o777 {
				t.Fatal("probe changed existing directory ownership or mode")
			}
			// Exercise the real operations independently of the readiness gate.
			temp := root.TempPath + "/upl_sticky.part"
			if _, _, err := appendUploadChunkToTemp(root, temp, 0, bytes.NewBufferString("media"), 5); err != nil {
				t.Fatal(err)
			}
			if size, _, err := sha1File(root, temp); err != nil || size != 5 {
				t.Fatalf("checksum size=%d, err=%v", size, err)
			}
			final := "image.jpg"
			err = finalizeUploadFile(root, temp, final)
			if test.stickyTemp {
				if !errors.Is(err, os.ErrPermission) {
					t.Fatalf("expected actual temp removal failure, got %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("finalize into sticky root: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root.Path, temp)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("temporary media remains: %v", err)
				}
			}
			content, err := os.ReadFile(filepath.Join(root.Path, final))
			if err != nil || string(content) != "media" {
				t.Fatalf("final content=%q, err=%v", content, err)
			}
			owner, err := resolveUploadOwner(root)
			if err != nil {
				t.Fatal(err)
			}
			assertUploadOwner(t, filepath.Join(root.Path, final), owner)
		})
	}
}

func effectiveUploadTestCapabilities(t *testing.T) uint64 {
	t.Helper()
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&header, &data[0]); err != nil {
		t.Fatal(err)
	}
	return uint64(data[0].Effective) | uint64(data[1].Effective)<<32
}
