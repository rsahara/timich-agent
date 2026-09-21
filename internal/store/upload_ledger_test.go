package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestUploadLedgerEpochMigrationResetAndMaintenance(t *testing.T) {
	s := newTestUploadStore(t)
	now := time.Now().UTC()
	createUploadedTestAsset(t, s, "upload-old", "old", now.Add(-time.Hour), now)
	createUploadedTestAsset(t, s, "upload-new", "new", now, now)
	createCommittedTestAsset(t, s, "upload-pending", "pending", now, now)
	identities := []UploadIdentity{{"old", "version-1"}, {"new", "version-1"}, {"pending", "version-1"}, {"missing", "version-1"}}
	before, states, err := s.CheckUploaded("device-1", identities)
	if err != nil || before == "" || fmt.Sprint(states) != "[true true false false]" {
		t.Fatalf("initial = %q %v %v", before, states, err)
	}
	other, otherStates, err := s.CheckUploaded("device-2", identities)
	if err != nil || fmt.Sprint(otherStates) != "[false false false false]" {
		t.Fatalf("cross-device = %v %v", otherStates, err)
	}
	proof, _, err := s.GetUploadedAssetBySourceIdentity("device-1", "new", "version-1")
	if err != nil || proof.LedgerEpoch != before {
		t.Fatalf("proof = %+v %v", proof, err)
	}
	if _, err := s.CleanupUploadState(UploadCleanupInput{Now: now.Add(365 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	epoch, _ := s.LedgerEpoch("device-1")
	if epoch != before {
		t.Fatal("maintenance rotated ledger")
	}
	afterDate := now.Add(-time.Minute)
	if _, err := s.ResetDeviceUploadState(UploadResetInput{DeviceID: "device-1", CapturedAfter: &afterDate, Now: now}); err != nil {
		t.Fatal(err)
	}
	after, states, err := s.CheckUploaded("device-1", identities)
	if err != nil || after == before || fmt.Sprint(states) != "[true false false false]" {
		t.Fatalf("reset = %q %v %v", after, states, err)
	}
	if proof.LedgerEpoch != before {
		t.Fatal("old proof changed generation")
	}
	unchanged, _ := s.LedgerEpoch("device-2")
	if unchanged != other {
		t.Fatal("other device epoch rotated")
	}
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := s.LedgerEpoch("device-1")
	if reloaded != after {
		t.Fatal("migration rotated existing epoch")
	}
	// A pre-sync database has the original ledger but no epoch table.
	if _, err := s.db.Exec(`DROP TABLE upload_ledger_epochs`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	migrated, migratedStates, err := s.CheckUploaded("device-1", identities)
	if err != nil || migrated == "" || migrated == after || fmt.Sprint(migratedStates) != "[true false false false]" {
		t.Fatalf("migration lost ledger: %q %v %v", migrated, migratedStates, err)
	}
}

func TestUploadLedgerResetRollsBackEpochWithDeletion(t *testing.T) {
	s := newTestUploadStore(t)
	now := time.Now().UTC()
	createUploadedTestAsset(t, s, "upload", "asset", now, now)
	before, _ := s.LedgerEpoch("device-1")
	if _, err := s.db.Exec(`CREATE TRIGGER prevent_reset BEFORE DELETE ON uploaded_assets BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetDeviceUploadState(UploadResetInput{DeviceID: "device-1"}); err == nil {
		t.Fatal("expected reset failure")
	}
	after, states, err := s.CheckUploaded("device-1", []UploadIdentity{{"asset", "version-1"}})
	if err != nil || after != before || !states[0] {
		t.Fatalf("partial reset: %q %v %v", after, states, err)
	}
}

func TestUploadLedgerCheckSnapshotDoesNotMixResetGenerations(t *testing.T) {
	s := newTestUploadStore(t)
	now := time.Now().UTC()
	createUploadedTestAsset(t, s, "upload", "asset", now, now)
	before, _ := s.LedgerEpoch("device-1")
	// Open the same WAL database from another connection to allow reset during reads.
	second, err := LoadOrCreateUploadStore(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	identities := make([]UploadIdentity, 200)
	for i := range identities {
		identities[i] = UploadIdentity{"asset", "version-1"}
	}
	var wg sync.WaitGroup
	errors := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			epoch, states, err := s.CheckUploaded("device-1", identities)
			if err != nil {
				errors <- err
				return
			}
			for _, state := range states {
				if state != (epoch == before) {
					errors <- fmt.Errorf("mixed generation")
					return
				}
			}
		}()
	}
	if _, err := second.ResetDeviceUploadState(UploadResetInput{DeviceID: "device-1"}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}
