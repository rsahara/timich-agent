package runtime

import (
	"bytes"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/store"
)

type heldUploadReader struct {
	entered chan struct{}
	release chan struct{}
	body    io.Reader
	once    sync.Once
}

func (r *heldUploadReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.entered)
		<-r.release
	})
	return r.body.Read(p)
}

type uploadAction struct {
	done chan struct{}
	err  error
}

func runUploadAction(fn func() error) *uploadAction {
	action := &uploadAction{done: make(chan struct{})}
	go func() { action.err = fn(); close(action.done) }()
	return action
}

func (action *uploadAction) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-action.done:
		return action.err
	case <-time.After(5 * time.Second):
		t.Fatal("upload action did not finish")
		return nil
	}
}

func holdUploadChunk(t *testing.T, a *AgentRuntime, deviceID, uploadID string, body []byte) (func(), *uploadAction) {
	t.Helper()
	reader := &heldUploadReader{entered: make(chan struct{}), release: make(chan struct{}), body: bytes.NewReader(body)}
	action := runUploadAction(func() error {
		_, err := a.AppendUploadChunk(deviceID, uploadID, concurrentChunk(body, reader))
		return err
	})
	var once sync.Once
	release := func() { once.Do(func() { close(reader.release) }) }
	t.Cleanup(func() { release(); _ = action.wait(t) })
	select {
	case <-reader.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow body did not start")
	}
	return release, action
}

func waitUploadCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("upload synchronization did not arrive")
		}
	}
}

func uploadLockRefs(registry *uploadLockRegistry, key string) int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if entry := registry.entries[strings.TrimSpace(key)]; entry != nil {
		return entry.refs
	}
	return 0
}

func TestUploadResetAndRevocationWaitOnlyForTargetDevice(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		name := "reset"
		if revoke {
			name = "revocation"
		}
		t.Run(name, func(t *testing.T) {
			a, root := newConcurrentUploadRuntime(t)
			deviceA, deviceB := addConcurrentUploadDevice(t, a), addConcurrentUploadDevice(t, a)
			body := []byte("slow upload")
			session := startConcurrentUpload(t, a, deviceA, "slow", body)
			release, appendAction := holdUploadChunk(t, a, deviceA, session.UploadID, body)
			before, after := session.CapturedAt.Add(time.Hour), session.CapturedAt.Add(-time.Hour)
			mutation := runUploadAction(func() error {
				if revoke {
					return a.RevokeDevice(" " + deviceA + " ")
				}
				_, err := a.ResetDeviceUploadState(UploadResetInput{DeviceID: " " + deviceA + " ", CapturedAfter: &after, CapturedBefore: &before})
				return err
			})
			// TryRLock fails only once the exclusive writer is actually pending.
			waitUploadCondition(t, func() bool {
				unlock := a.uploadDevices.tryReadLock(deviceA)
				if unlock == nil {
					return true
				}
				unlock()
				return false
			})
			unrelated := runUploadAction(func() error {
				size := int64(len(body))
				started, err := a.StartUploadSession(deviceB, UploadSessionStartInput{SourceAssetID: "other", SourceAssetVersion: "v1", MediaType: "image", OriginalFilename: "photo.jpg", ExpectedSizeBytes: &size})
				if err != nil {
					return err
				}
				if started.Session == nil {
					return errors.New("other device start did not create session")
				}
				if _, err := a.AppendUploadChunk(deviceB, started.Session.UploadID, concurrentChunk(body, bytes.NewReader(body))); err != nil {
					return err
				}
				_, err = a.CompleteUploadSession(deviceB, started.Session.UploadID, concurrentComplete(body))
				return err
			})
			// Ensure mutations are drained after releasing the body even on failure.
			defer func() { release(); _ = mutation.wait(t); _ = unrelated.wait(t) }()
			if err := unrelated.wait(t); err != nil {
				t.Fatal(err)
			}
			select {
			case <-mutation.done:
				t.Fatal("reset/revocation passed active upload")
			default:
			}
			// A stale completion queues behind the writer, and must reload after deletion.
			stale := runUploadAction(func() error {
				_, err := a.CompleteUploadSession(deviceA, session.UploadID, concurrentComplete(body))
				return err
			})
			defer func() { release(); _ = stale.wait(t) }()
			waitUploadCondition(t, func() bool { return uploadLockRefs(&a.uploadDevices, deviceA) >= 3 })
			release()
			if err := appendAction.wait(t); err != nil {
				t.Fatal(err)
			}
			if err := mutation.wait(t); err != nil {
				t.Fatal(err)
			}
			if err := stale.wait(t); !errors.Is(err, store.ErrUploadSessionNotFound) && !errors.Is(err, store.ErrDeviceNotFound) {
				t.Fatalf("stale completion = %v", err)
			}
			if _, exists, err := a.uploads.GetSession(session.UploadID); err != nil || exists {
				t.Fatalf("deleted session resurrected: exists=%v err=%v", exists, err)
			}
			if _, exists, err := a.uploads.GetUploadedAssetByUploadID(session.UploadID); err != nil || exists {
				t.Fatalf("deleted ledger resurrected: exists=%v err=%v", exists, err)
			}
			if _, err := os.Stat(filepath.Join(root, ".timich-upload-tmp", session.UploadID+".part")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temp after deletion = %v", err)
			}
			if revoke {
				if _, err := a.profiles.LoadProfile(deviceA); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("revoked profile = %v", err)
				}
				if _, err := a.StartUploadSession(deviceA, UploadSessionStartInput{SourceAssetID: "new", SourceAssetVersion: "v1", MediaType: "image", OriginalFilename: "photo.jpg"}); !errors.Is(err, store.ErrDeviceNotFound) {
					t.Fatalf("revoked start = %v", err)
				}
			}
		})
	}
}

func TestUploadSameSessionSerializesActions(t *testing.T) {
	for _, kind := range []string{"duplicate offset", "complete", "abort"} {
		t.Run(kind, func(t *testing.T) {
			a, root := newConcurrentUploadRuntime(t)
			device := addConcurrentUploadDevice(t, a)
			body := []byte("one append")
			session := startConcurrentUpload(t, a, device, "one", body)
			release, appendAction := holdUploadChunk(t, a, device, session.UploadID, body)
			action := runUploadAction(func() error {
				switch kind {
				case "duplicate offset":
					_, err := a.AppendUploadChunk(device, " "+session.UploadID+" ", concurrentChunk(body, bytes.NewReader(body)))
					return err
				case "complete":
					_, err := a.CompleteUploadSession(device, " "+session.UploadID+" ", concurrentComplete(body))
					return err
				default:
					_, err := a.AbortUploadSession(device, " "+session.UploadID+" ")
					return err
				}
			})
			defer func() { release(); _ = action.wait(t) }()
			waitUploadCondition(t, func() bool { return uploadLockRefs(&a.uploadSessions, session.UploadID) == 2 })
			select {
			case <-action.done:
				t.Fatal("same session action passed active append")
			default:
			}
			release()
			if err := appendAction.wait(t); err != nil {
				t.Fatal(err)
			}
			err := action.wait(t)
			if kind == "duplicate offset" {
				if !errors.Is(err, store.ErrUploadSessionOffsetConflict) {
					t.Fatalf("duplicate = %v", err)
				}
				state, _, err := a.uploads.GetSession(session.UploadID)
				if err != nil || state.NextOffset != int64(len(body)) {
					t.Fatalf("offset = %d err=%v", state.NextOffset, err)
				}
				raw, err := os.ReadFile(filepath.Join(root, ".timich-upload-tmp", session.UploadID+".part"))
				if err != nil || !bytes.Equal(raw, body) {
					t.Fatalf("temp = %q err=%v", raw, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				state, _, err := a.uploads.GetSession(session.UploadID)
				want := "completed"
				if kind == "abort" {
					want = "aborted"
				}
				if err != nil || state.Status != want {
					t.Fatalf("state = %s err=%v", state.Status, err)
				}
				if _, err := os.Stat(filepath.Join(root, ".timich-upload-tmp", session.UploadID+".part")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("temp = %v", err)
				}
			}
		})
	}
}

func TestUploadMaintenanceSkipsActiveFilesAndRetries(t *testing.T) {
	a, root := newConcurrentUploadRuntime(t)
	device := addConcurrentUploadDevice(t, a)
	body := []byte("held content")
	session := startConcurrentUpload(t, a, device, "held", body)
	release, action := holdUploadChunk(t, a, device, session.UploadID, body)
	temp := filepath.Join(root, ".timich-upload-tmp", session.UploadID+".part")
	now := session.ExpiresAt.Add(2 * time.Hour)
	old := now.Add(-48 * time.Hour)
	if err := os.Chtimes(temp, old, old); err != nil {
		t.Fatal(err)
	}
	maintenance := runUploadAction(func() error {
		result, err := a.RunUploadMaintenanceOnce(now)
		if err == nil && (result.RemovedExpiredSessions != 0 || result.RemovedTempFiles != 0 || result.RemovedOrphanTempFiles != 0) {
			return errors.New("maintenance removed busy session/file")
		}
		return err
	})
	defer func() { release(); _ = maintenance.wait(t) }()
	if err := maintenance.wait(t); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := a.uploads.GetSession(session.UploadID); err != nil || !exists {
		t.Fatalf("held session exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(temp); err != nil {
		t.Fatal(err)
	}
	release()
	if err := action.wait(t); err != nil {
		t.Fatal(err)
	}
	result, err := a.RunUploadMaintenanceOnce(now)
	if err != nil || result.RemovedExpiredSessions != 1 || result.RemovedTempFiles != 1 {
		t.Fatalf("retry = %+v err=%v", result, err)
	}
}

func TestUploadMaintenanceRetainsOwnedOldTemps(t *testing.T) {
	a, root := newConcurrentUploadRuntime(t)
	// Two configured keys may expose the same physical upload root.
	a.config.UploadRoots = append(a.config.UploadRoots, config.UploadRootConfig{Key: "alias", Path: root})
	device := addConcurrentUploadDevice(t, a)
	body := []byte("owned content")
	session := startConcurrentUpload(t, a, device, "owned", body)
	if _, err := a.AppendUploadChunk(device, session.UploadID, concurrentChunk(body, bytes.NewReader(body))); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	temp := filepath.Join(root, ".timich-upload-tmp", session.UploadID+".part")
	if err := os.Chtimes(temp, old, old); err != nil {
		t.Fatal(err)
	}
	for _, committing := range []bool{false, true} {
		if committing {
			reserveConcurrentUpload(t, a, session, body, "photo.jpg")
		}
		result, err := a.RunUploadMaintenanceOnce(now)
		if err != nil || result.RemovedOrphanTempFiles != 0 {
			t.Fatalf("maintenance = %+v err=%v", result, err)
		}
		if _, err := os.Stat(temp); err != nil {
			t.Fatalf("owned temp removed (committing=%v): %v", committing, err)
		}
	}
}

func reserveConcurrentUpload(t *testing.T, a *AgentRuntime, session UploadSessionSummary, body []byte, finalPath string) store.UploadedAsset {
	t.Helper()
	asset, err := a.uploads.ReserveUploadCommit(store.UploadCommitInput{
		UploadID: session.UploadID, DeviceID: mustConcurrentSession(t, a, session.UploadID).DeviceID,
		SourceAssetID: session.SourceAssetID, SourceAssetVersion: session.SourceAssetVersion,
		MediaType: session.MediaType, OriginalFilename: session.OriginalFilename, CapturedAt: session.CapturedAt,
		ExpectedSizeBytes: session.ExpectedSizeBytes, FinalRelativePath: finalPath,
		Checksums: []store.UploadChecksum{{Algorithm: "sha1", Encoding: "hex", Digest: sha1Hex(body)}}, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return asset
}

func mustConcurrentSession(t *testing.T, a *AgentRuntime, uploadID string) store.UploadSession {
	t.Helper()
	session, exists, err := a.uploads.GetSession(uploadID)
	if err != nil || !exists {
		t.Fatalf("session exists=%v err=%v", exists, err)
	}
	return session
}

func TestUploadConcurrentCompletionAndRecoveryPreserveFiles(t *testing.T) {
	a, root := newConcurrentUploadRuntime(t)
	device := addConcurrentUploadDevice(t, a)
	if err := os.WriteFile(filepath.Join(root, "photo.jpg"), []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	bodyA, bodyB := []byte("content a"), []byte("content b")
	sessionA := startConcurrentUpload(t, a, device, "a", bodyA)
	sessionB := startConcurrentUpload(t, a, device, "b", bodyB)
	for i, session := range []UploadSessionSummary{sessionA, sessionB} {
		body := [][]byte{bodyA, bodyB}[i]
		if _, err := a.AppendUploadChunk(device, session.UploadID, concurrentChunk(body, bytes.NewReader(body))); err != nil {
			t.Fatal(err)
		}
	}
	// A reservation simulates an interrupted completion, with a colliding path.
	stale := reserveConcurrentUpload(t, a, sessionA, bodyA, "photo.jpg")
	barrier := make(chan struct{})
	actions := []*uploadAction{}
	for _, request := range []struct {
		session UploadSessionSummary
		body    []byte
		recover bool
	}{
		{sessionA, bodyA, false}, {sessionA, bodyA, true}, {sessionB, bodyB, false}, {sessionB, bodyB, false},
	} {
		request := request
		actions = append(actions, runUploadAction(func() error {
			<-barrier
			if request.recover {
				_, err := a.StartUploadSession(device, UploadSessionStartInput{SourceAssetID: request.session.SourceAssetID, SourceAssetVersion: "v1", MediaType: "image", OriginalFilename: "photo.jpg"})
				return err
			}
			_, err := a.CompleteUploadSession(device, request.session.UploadID, concurrentComplete(request.body))
			return err
		}))
	}
	close(barrier)
	for _, action := range actions {
		if err := action.wait(t); err != nil {
			t.Fatal(err)
		}
	}
	assetA, _, err := a.uploads.GetUploadedAssetByUploadID(sessionA.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	assetB, _, err := a.uploads.GetUploadedAssetByUploadID(sessionB.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	if assetA.Status != "uploaded" || assetB.Status != "uploaded" || assetA.FinalRelativePath == assetB.FinalRelativePath {
		t.Fatalf("assets A=%+v B=%+v", assetA, assetB)
	}
	// Recovery must reload rather than acting on this stale photo.jpg reservation.
	unlock := a.uploadDevices.readLock(device)
	recovered, ok, err := a.recoverPendingUploadAsset(stale)
	unlock()
	if err != nil || !ok || recovered.FinalRelativePath != assetA.FinalRelativePath {
		t.Fatalf("stale recovery = %+v ok=%v err=%v", recovered, ok, err)
	}
	for file, body := range map[string][]byte{"photo.jpg": []byte("existing"), assetA.FinalRelativePath: bodyA, assetB.FinalRelativePath: bodyB} {
		raw, err := os.ReadFile(filepath.Join(root, file))
		if err != nil || !bytes.Equal(raw, body) {
			t.Fatalf("file %s = %q err=%v", file, raw, err)
		}
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 4 {
		t.Fatalf("root entries = %d err=%v, want 3 final files and temp directory", len(files), err)
	}
}

func TestUploadFailedAppendPreservesOffsetAndAllowsRetry(t *testing.T) {
	for _, kind := range []string{"body read", "checksum", "progress save"} {
		t.Run(kind, func(t *testing.T) {
			a, root := newConcurrentUploadRuntime(t)
			device := addConcurrentUploadDevice(t, a)
			prefix, suffix := []byte("prefix "), []byte("suffix")
			session := startConcurrentUpload(t, a, device, "retry", append(append([]byte{}, prefix...), suffix...))
			if _, err := a.AppendUploadChunk(device, session.UploadID, concurrentChunk(prefix, bytes.NewReader(prefix))); err != nil {
				t.Fatal(err)
			}
			input := concurrentChunk(suffix, bytes.NewReader(suffix))
			input.Offset = int64(len(prefix))
			var db *sql.DB
			switch kind {
			case "body read":
				input.Body = io.MultiReader(bytes.NewReader(suffix[:2]), failingUploadReader{})
			case "checksum":
				input.ChunkSHA1Hex = sha1Hex([]byte("wrong"))
			case "progress save":
				var err error
				db, err = sql.Open("sqlite", a.uploads.Path())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(`CREATE TRIGGER reject_progress BEFORE UPDATE OF next_offset ON upload_sessions BEGIN SELECT RAISE(FAIL, 'injected progress failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.AppendUploadChunk(device, session.UploadID, input); err == nil {
				t.Fatal("failed append succeeded")
			}
			state := mustConcurrentSession(t, a, session.UploadID)
			raw, err := os.ReadFile(filepath.Join(root, ".timich-upload-tmp", session.UploadID+".part"))
			if state.NextOffset != int64(len(prefix)) || err != nil || !bytes.Equal(raw, prefix) {
				t.Fatalf("after failure: offset=%d raw=%q err=%v", state.NextOffset, raw, err)
			}
			if db != nil {
				if _, err := db.Exec(`DROP TRIGGER reject_progress`); err != nil {
					t.Fatal(err)
				}
			}
			input = concurrentChunk(suffix, bytes.NewReader(suffix))
			input.Offset = int64(len(prefix))
			if _, err := a.AppendUploadChunk(device, session.UploadID, input); err != nil {
				t.Fatal(err)
			}
			full := append(append([]byte{}, prefix...), suffix...)
			if _, err := a.CompleteUploadSession(device, session.UploadID, concurrentComplete([]byte("wrong"))); !errors.Is(err, ErrUploadChecksumMismatch) {
				t.Fatalf("final checksum mismatch = %v", err)
			}
			if _, err := a.CompleteUploadSession(device, session.UploadID, concurrentComplete(full)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type failingUploadReader struct{}

func (failingUploadReader) Read([]byte) (int, error) {
	return 0, errors.New("injected body read failure")
}

func TestUploadConcurrentFinalParentCreation(t *testing.T) {
	a, root := newConcurrentUploadRuntime(t)
	device := addConcurrentUploadDevice(t, a)
	if _, err := a.UpdateDeviceUploadPolicy(device, DeviceUploadPolicyUpdate{Enabled: true, RootKey: "photos", PathPattern: "shared/date/{filename}"}); err != nil {
		t.Fatal(err)
	}
	body := []byte("shared parent content")
	var sessions []UploadSessionSummary
	for _, id := range []string{"one", "two", "three", "four"} {
		session := startConcurrentUpload(t, a, device, id, body)
		if _, err := a.AppendUploadChunk(device, session.UploadID, concurrentChunk(body, bytes.NewReader(body))); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, session)
	}
	barrier := make(chan struct{})
	var actions []*uploadAction
	for _, session := range sessions {
		session := session
		actions = append(actions, runUploadAction(func() error {
			<-barrier
			_, err := a.CompleteUploadSession(device, session.UploadID, concurrentComplete(body))
			return err
		}))
	}
	close(barrier)
	defer func() {
		for _, action := range actions {
			_ = action.wait(t)
		}
	}()
	for _, action := range actions {
		if err := action.wait(t); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "shared", "date"))
	if err != nil || len(entries) != len(sessions) {
		t.Fatalf("final files = %d err=%v", len(entries), err)
	}
}

func TestUploadCompleteAndAbortRace(t *testing.T) {
	a, root := newConcurrentUploadRuntime(t)
	device := addConcurrentUploadDevice(t, a)
	body := []byte("complete or abort")
	session := startConcurrentUpload(t, a, device, "race", body)
	if _, err := a.AppendUploadChunk(device, session.UploadID, concurrentChunk(body, bytes.NewReader(body))); err != nil {
		t.Fatal(err)
	}
	barrier := make(chan struct{})
	complete := runUploadAction(func() error {
		<-barrier
		_, err := a.CompleteUploadSession(device, session.UploadID, concurrentComplete(body))
		return err
	})
	abort := runUploadAction(func() error { <-barrier; _, err := a.AbortUploadSession(device, session.UploadID); return err })
	close(barrier)
	defer func() { _ = complete.wait(t); _ = abort.wait(t) }()
	completeErr, abortErr := complete.wait(t), abort.wait(t)
	state := mustConcurrentSession(t, a, session.UploadID)
	asset, exists, err := a.uploads.GetUploadedAssetByUploadID(session.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status == "completed" {
		if completeErr != nil || !errors.Is(abortErr, store.ErrUploadSessionNotFound) || !exists || asset.Status != "uploaded" {
			t.Fatalf("completed race: complete=%v abort=%v asset=%+v exists=%v", completeErr, abortErr, asset, exists)
		}
		raw, err := os.ReadFile(filepath.Join(root, asset.FinalRelativePath))
		if err != nil || !bytes.Equal(raw, body) {
			t.Fatalf("completed file=%q err=%v", raw, err)
		}
	} else if state.Status == "aborted" {
		if abortErr != nil || !errors.Is(completeErr, store.ErrUploadSessionNotFound) || exists {
			t.Fatalf("aborted race: complete=%v abort=%v exists=%v", completeErr, abortErr, exists)
		}
		if _, err := os.Stat(filepath.Join(root, "photo.jpg")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("aborted final file=%v", err)
		}
	} else {
		t.Fatalf("race state=%s", state.Status)
	}
	if _, err := os.Stat(filepath.Join(root, ".timich-upload-tmp", session.UploadID+".part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("race temp=%v", err)
	}
}

func newConcurrentUploadRuntime(t *testing.T) (*AgentRuntime, string) {
	t.Helper()
	a := newTestAgentRuntime(t, BuildInfo{}, nil)
	root := t.TempDir()
	a.config.UploadRoots = []config.UploadRootConfig{{Key: "photos", Path: root}}
	return a, root
}

func addConcurrentUploadDevice(t *testing.T, a *AgentRuntime) string {
	t.Helper()
	bundle, err := a.CreateHostedSession("Upload device", "https://timich.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpdateDeviceUploadPolicy(bundle.DeviceID, DeviceUploadPolicyUpdate{
		Enabled: true, RootKey: "photos", PathPattern: "{filename}",
	}); err != nil {
		t.Fatal(err)
	}
	return bundle.DeviceID
}

func startConcurrentUpload(t *testing.T, a *AgentRuntime, deviceID, assetID string, body []byte) UploadSessionSummary {
	t.Helper()
	size := int64(len(body))
	captured := time.Now().UTC()
	response, err := a.StartUploadSession(deviceID, UploadSessionStartInput{
		SourceAssetID: assetID, SourceAssetVersion: "v1", MediaType: "image",
		OriginalFilename: "photo.jpg", ExpectedSizeBytes: &size, CapturedAt: &captured,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Session == nil {
		t.Fatalf("start = %+v", response)
	}
	return *response.Session
}

func concurrentChunk(body []byte, reader io.Reader) UploadChunkInput {
	digest := sha1.Sum(body)
	return UploadChunkInput{Body: reader, ChunkSHA1Hex: hex.EncodeToString(digest[:]), ContentLengthBytes: int64(len(body))}
}

func concurrentComplete(body []byte) UploadSessionCompleteInput {
	digest := sha1.Sum(body)
	return UploadSessionCompleteInput{Checksums: []UploadSessionChecksumInput{{Algorithm: "sha1", Digest: hex.EncodeToString(digest[:])}}}
}

func awaitUploadResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("upload operation did not finish")
		return nil
	}
}

func TestUploadSlowBodyDoesNotBlockOtherSessions(t *testing.T) {
	for _, sameDevice := range []bool{false, true} {
		name := "different devices"
		if sameDevice {
			name = "same device"
		}
		t.Run(name, func(t *testing.T) {
			a, _ := newConcurrentUploadRuntime(t)
			deviceA := addConcurrentUploadDevice(t, a)
			deviceB := deviceA
			if !sameDevice {
				deviceB = addConcurrentUploadDevice(t, a)
			}
			body := []byte("photo content")
			sessionA := startConcurrentUpload(t, a, deviceA, "a", body)
			sessionB := startConcurrentUpload(t, a, deviceB, "b", body)
			reader := &heldUploadReader{entered: make(chan struct{}), release: make(chan struct{}), body: bytes.NewReader(body)}
			resultA := make(chan error, 1)
			resultB := make(chan error, 1)
			pendingB := false
			go func() {
				_, err := a.AppendUploadChunk(deviceA, sessionA.UploadID, concurrentChunk(body, reader))
				resultA <- err
			}()
			defer func() {
				close(reader.release)
				if err := awaitUploadResult(t, resultA); err != nil {
					t.Error(err)
				}
				if pendingB {
					if err := awaitUploadResult(t, resultB); err != nil {
						t.Error(err)
					}
				}
			}()
			select {
			case <-reader.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("slow reader did not start")
			}
			pendingB = true
			go func() {
				_, err := a.AppendUploadChunk(deviceB, sessionB.UploadID, concurrentChunk(body, bytes.NewReader(body)))
				if err == nil {
					_, err = a.CompleteUploadSession(deviceB, sessionB.UploadID, concurrentComplete(body))
				}
				resultB <- err
			}()
			select {
			case err := <-resultB:
				pendingB = false
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				// Release A before draining B, including on the broken implementation.
				t.Error("unrelated append/complete blocked by slow body")
			}
		})
	}
}
