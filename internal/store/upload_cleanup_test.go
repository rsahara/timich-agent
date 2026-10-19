package store

import (
	"testing"
	"time"
)

func TestCleanupUploadSessionRechecksCandidateState(t *testing.T) {
	for _, state := range []string{"extended expiry", "committing", "recent completion", "deleted"} {
		t.Run(state, func(t *testing.T) {
			s := newTestUploadStore(t)
			now := time.Now().UTC()
			expired := now.Add(-time.Hour)
			_, err := s.CreateSession(UploadSessionInput{UploadID: "upl_candidate", DeviceID: "device", SourceAssetID: "asset", SourceAssetVersion: "v1", MediaType: "image", OriginalFilename: "photo.jpg", SelectedRootKey: "photos", TempRelativePath: ".timich-upload-tmp/upl_candidate.part", ExpiresAt: &expired, Now: expired})
			if err != nil {
				t.Fatal(err)
			}
			input := UploadCleanupInput{Now: now, CompletedSessionRetention: 7 * 24 * time.Hour}
			candidates, err := s.UploadCleanupCandidates(input)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidates=%+v err=%v", candidates, err)
			}
			switch state {
			case "extended expiry":
				_, err = s.db.Exec(`UPDATE upload_sessions SET expires_at = ? WHERE upload_id = ?`, formatDBTime(now.Add(time.Hour)), "upl_candidate")
			case "committing":
				_, err = s.db.Exec(`UPDATE upload_sessions SET status = 'committing' WHERE upload_id = ?`, "upl_candidate")
			case "recent completion":
				_, err = s.db.Exec(`UPDATE upload_sessions SET status = 'completed', updated_at = ? WHERE upload_id = ?`, formatDBTime(now), "upl_candidate")
			case "deleted":
				_, err = s.db.Exec(`DELETE FROM upload_sessions WHERE upload_id = ?`, "upl_candidate")
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.CleanupUploadSession(candidates[0].UploadID, input)
			if err != nil || result.RemovedSessions() != 0 || len(result.TempFiles) != 0 {
				t.Fatalf("stale cleanup=%+v err=%v", result, err)
			}
			if state != "deleted" {
				if _, exists, err := s.GetSession("upl_candidate"); err != nil || !exists {
					t.Fatalf("session exists=%v err=%v", exists, err)
				}
			}
		})
	}
}
