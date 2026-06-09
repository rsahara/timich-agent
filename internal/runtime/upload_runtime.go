package runtime

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
	"github.com/rsahara/timich-agent/internal/store"
)

const (
	defaultUploadChunkSizeBytes = int64(8 * 1024 * 1024)
	defaultUploadSessionTTL     = 24 * time.Hour
	uploadCommitBlockedReason   = "Upload commit cannot be recovered automatically. Reset this upload state from the Agent admin UI."
	uploadCapturedBeforeReason  = "Asset was captured before this device upload policy allows."
	uploadSessionPolicyReason   = "The upload policy no longer allows this session to continue."
	uploadCreatedFileMode       = 0o666
	uploadCreatedDirMode        = 0o777
)

var ErrUploadRequestInvalid = errors.New("upload request invalid")

var (
	ErrUploadChecksumMismatch  = errors.New("upload checksum mismatch")
	ErrUploadFinalPathConflict = errors.New("upload final path conflict")
)

// AppUploadStateResponse is the app-facing upload policy and status for the
// authenticated paired device.
type AppUploadStateResponse struct {
	DeviceID                string                   `json:"deviceId"`
	Upload                  DeviceUploadPolicy       `json:"upload"`
	Status                  DeviceUploadPolicyStatus `json:"status"`
	ChunkSizeBytes          int64                    `json:"chunkSizeBytes"`
	LatestCommittedUploadAt *time.Time               `json:"latestCommittedUploadAt,omitempty"`
	ActiveSession           *UploadSessionSummary    `json:"activeSession,omitempty"`
}

// UploadSessionStartInput is the metadata required before binary chunks are
// accepted for one Photos asset resource.
type UploadSessionStartInput struct {
	SourceAssetID      string     `json:"sourceAssetId"`
	SourceAssetVersion string     `json:"sourceAssetVersion"`
	MediaType          string     `json:"mediaType"`
	OriginalFilename   string     `json:"originalFilename"`
	CapturedAt         *time.Time `json:"capturedAt,omitempty"`
	ExpectedSizeBytes  *int64     `json:"expectedSizeBytes,omitempty"`
	MimeType           string     `json:"mimeType,omitempty"`
	ResourceSignature  string     `json:"resourceSignature,omitempty"`
	ChecksumAlgorithms []string   `json:"checksumAlgorithms,omitempty"`
}

// UploadSessionStartResponse reports how the app should proceed after asking
// the Agent to upload one source asset version.
type UploadSessionStartResponse struct {
	State         string                    `json:"state"`
	Reason        string                    `json:"reason,omitempty"`
	Status        *DeviceUploadPolicyStatus `json:"status,omitempty"`
	Session       *UploadSessionSummary     `json:"session,omitempty"`
	UploadedAsset *UploadedAssetSummary     `json:"uploadedAsset,omitempty"`
}

// UploadSessionSummary is the resumable upload session state exposed to the app.
type UploadSessionSummary struct {
	UploadID           string     `json:"uploadId"`
	Status             string     `json:"status"`
	SourceAssetID      string     `json:"sourceAssetId"`
	SourceAssetVersion string     `json:"sourceAssetVersion"`
	MediaType          string     `json:"mediaType"`
	OriginalFilename   string     `json:"originalFilename"`
	CapturedAt         *time.Time `json:"capturedAt,omitempty"`
	ExpectedSizeBytes  *int64     `json:"expectedSizeBytes,omitempty"`
	RootKey            string     `json:"rootKey"`
	NextOffset         int64      `json:"nextOffset"`
	ChunkSizeBytes     int64      `json:"chunkSizeBytes"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
	ExpiresAt          *time.Time `json:"expiresAt,omitempty"`
}

// UploadedAssetSummary identifies a device-local source asset version that is
// already committed in the once-only ledger.
type UploadedAssetSummary struct {
	UploadedAssetID   int64      `json:"uploadedAssetId"`
	Status            string     `json:"status"`
	RootKey           string     `json:"rootKey"`
	FinalRelativePath string     `json:"finalRelativePath"`
	UploadedAt        *time.Time `json:"uploadedAt,omitempty"`
}

// UploadChunkInput describes one sequential raw chunk append request.
type UploadChunkInput struct {
	Offset             int64
	ChunkSHA1Hex       string
	Body               io.Reader
	ContentLengthBytes int64
}

// UploadSessionChecksumInput describes a final checksum supplied by the app.
type UploadSessionChecksumInput struct {
	Algorithm string `json:"algorithm"`
	Encoding  string `json:"encoding,omitempty"`
	Digest    string `json:"digest"`
}

// UploadSessionCompleteInput confirms the source version and final checksums
// before the Agent commits the temp file into the selected upload root.
type UploadSessionCompleteInput struct {
	SourceAssetVersion string                       `json:"sourceAssetVersion,omitempty"`
	Checksums          []UploadSessionChecksumInput `json:"checksums,omitempty"`
}

// UploadSessionActionResponse reports the result of chunk, complete, or abort
// actions against an upload session.
type UploadSessionActionResponse struct {
	State         string                `json:"state"`
	Session       *UploadSessionSummary `json:"session,omitempty"`
	UploadedAsset *UploadedAssetSummary `json:"uploadedAsset,omitempty"`
}

// AppUploadState returns the effective upload state for a paired app device.
func (a *AgentRuntime) AppUploadState(deviceID string) (AppUploadStateResponse, error) {
	_, profile, err := a.deviceProfile(deviceID)
	if err != nil {
		return AppUploadStateResponse{}, err
	}
	status := a.deviceUploadEffectiveStatus(profile.Upload)
	var activeSession *UploadSessionSummary
	if session, ok, err := a.uploads.GetLatestActiveSession(profile.DeviceID); err != nil {
		return AppUploadStateResponse{}, err
	} else if ok &&
		!uploadSessionExpired(session, time.Now().UTC()) &&
		status.State == "ready" &&
		a.uploadSessionPolicyStatus(profile.Upload, session, true).State == "ready" {
		summary := uploadSessionSummary(session)
		activeSession = &summary
	}
	latestCommittedUploadAt, err := a.uploads.LatestUploadedAt(profile.DeviceID)
	if err != nil {
		return AppUploadStateResponse{}, err
	}
	return AppUploadStateResponse{
		DeviceID:                profile.DeviceID,
		Upload:                  deviceUploadPolicy(profile.Upload),
		Status:                  status,
		ChunkSizeBytes:          defaultUploadChunkSizeBytes,
		LatestCommittedUploadAt: latestCommittedUploadAt,
		ActiveSession:           activeSession,
	}, nil
}

// StartUploadSession creates or resumes an upload session for one source asset
// version after enforcing the current Agent-owned upload policy.
func (a *AgentRuntime) StartUploadSession(deviceID string, input UploadSessionStartInput) (UploadSessionStartResponse, error) {
	normalized, err := normalizeUploadSessionStartInput(input)
	if err != nil {
		return UploadSessionStartResponse{}, err
	}
	device, profile, err := a.deviceProfile(deviceID)
	if err != nil {
		return UploadSessionStartResponse{}, err
	}
	if asset, ok, err := a.uploads.GetUploadedAssetBySourceIdentity(
		device.DeviceID,
		normalized.SourceAssetID,
		normalized.SourceAssetVersion,
	); err != nil {
		return UploadSessionStartResponse{}, err
	} else if ok {
		if asset.Status != "uploaded" {
			if recovered, ok, err := a.recoverPendingUploadAsset(asset); err != nil {
				return UploadSessionStartResponse{}, err
			} else if ok {
				return UploadSessionStartResponse{
					State:         "already_uploaded",
					UploadedAsset: uploadedAssetSummary(recovered),
				}, nil
			}
			return blockedPendingUploadSessionResponse(asset), nil
		}
		return UploadSessionStartResponse{
			State:         "already_uploaded",
			UploadedAsset: uploadedAssetSummary(asset),
		}, nil
	}

	status := a.deviceUploadEffectiveStatus(profile.Upload)
	if status.State != "ready" {
		return blockedUploadSessionResponse(status), nil
	}
	root, ok := a.uploadRootConfig(profile.Upload.RootKey)
	if !ok {
		return blockedUploadSessionResponse(DeviceUploadPolicyStatus{
			State:  "blocked",
			Reason: "Upload root is not configured.",
		}), nil
	}
	now := time.Now().UTC()
	if session, ok, err := a.uploads.GetActiveSessionBySourceIdentity(
		device.DeviceID,
		normalized.SourceAssetID,
		normalized.SourceAssetVersion,
	); err != nil {
		return UploadSessionStartResponse{}, err
	} else if ok && !uploadSessionExpired(session, now) {
		if sessionStatus := a.uploadSessionPolicyStatus(profile.Upload, session, true); sessionStatus.State != "ready" {
			return blockedUploadSessionResponse(sessionStatus), nil
		}
		summary := uploadSessionSummary(session)
		return UploadSessionStartResponse{
			State:   "resumable",
			Session: &summary,
		}, nil
	}
	if uploadCapturedAtBlocked(profile.Upload, normalized.CapturedAt) {
		return blockedUploadSessionResponse(uploadCapturedBeforePolicyStatus()), nil
	}

	uploadID, err := randomUploadID()
	if err != nil {
		return UploadSessionStartResponse{}, err
	}
	expiresAt := now.Add(defaultUploadSessionTTL)
	session, err := a.uploads.CreateSession(store.UploadSessionInput{
		UploadID:           uploadID,
		DeviceID:           device.DeviceID,
		SourceAssetID:      normalized.SourceAssetID,
		SourceAssetVersion: normalized.SourceAssetVersion,
		MediaType:          normalized.MediaType,
		OriginalFilename:   normalized.OriginalFilename,
		CapturedAt:         normalized.CapturedAt,
		ExpectedSizeBytes:  normalized.ExpectedSizeBytes,
		SelectedRootKey:    profile.Upload.RootKey,
		TempRelativePath:   path.Join(root.TempPath, uploadID+".part"),
		ExpiresAt:          &expiresAt,
		Now:                now,
	})
	if err != nil {
		return UploadSessionStartResponse{}, err
	}
	summary := uploadSessionSummary(session)
	return UploadSessionStartResponse{
		State:   "accepted",
		Session: &summary,
	}, nil
}

// UploadSession returns one app-visible upload session when it belongs to the
// authenticated paired device.
func (a *AgentRuntime) UploadSession(deviceID string, uploadID string) (UploadSessionSummary, error) {
	normalizedDeviceID := strings.TrimSpace(deviceID)
	session, ok, err := a.uploads.GetSession(uploadID)
	if err != nil {
		return UploadSessionSummary{}, err
	}
	if !ok || session.DeviceID != normalizedDeviceID {
		return UploadSessionSummary{}, store.ErrUploadSessionNotFound
	}
	return uploadSessionSummary(session), nil
}

// AppendUploadChunk appends one sequential raw chunk to the session temp file.
func (a *AgentRuntime) AppendUploadChunk(deviceID string, uploadID string, input UploadChunkInput) (UploadSessionActionResponse, error) {
	if input.Body == nil {
		return UploadSessionActionResponse{}, ErrUploadRequestInvalid
	}
	chunkSHA1, err := normalizeSHA1Hex(input.ChunkSHA1Hex)
	if err != nil {
		return UploadSessionActionResponse{}, ErrUploadRequestInvalid
	}
	if input.Offset < 0 || input.ContentLengthBytes == 0 || input.ContentLengthBytes > defaultUploadChunkSizeBytes {
		return UploadSessionActionResponse{}, ErrUploadRequestInvalid
	}

	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()

	session, root, _, err := a.activeUploadSessionContext(deviceID, uploadID)
	if err != nil {
		return UploadSessionActionResponse{}, err
	}
	if session.NextOffset != input.Offset {
		return UploadSessionActionResponse{}, store.ErrUploadSessionOffsetConflict
	}
	if session.ExpectedSizeBytes != nil && input.Offset+input.ContentLengthBytes > *session.ExpectedSizeBytes {
		return UploadSessionActionResponse{}, ErrUploadRequestInvalid
	}
	written, digest, err := appendUploadChunkToTemp(root, session.TempRelativePath, input.Offset, input.Body, input.ContentLengthBytes)
	if err != nil {
		return UploadSessionActionResponse{}, err
	}
	if digest != chunkSHA1 {
		_ = truncateUploadTempFile(root, session.TempRelativePath, input.Offset)
		return UploadSessionActionResponse{}, ErrUploadChecksumMismatch
	}
	nextOffset := input.Offset + written
	if session.ExpectedSizeBytes != nil && nextOffset > *session.ExpectedSizeBytes {
		_ = truncateUploadTempFile(root, session.TempRelativePath, input.Offset)
		return UploadSessionActionResponse{}, ErrUploadRequestInvalid
	}
	updated, err := a.uploads.UpdateSessionProgress(session.UploadID, input.Offset, nextOffset, time.Now().UTC())
	if err != nil {
		_ = truncateUploadTempFile(root, session.TempRelativePath, input.Offset)
		return UploadSessionActionResponse{}, err
	}
	summary := uploadSessionSummary(updated)
	return UploadSessionActionResponse{
		State:   "accepted",
		Session: &summary,
	}, nil
}

// CompleteUploadSession verifies the temp file and final checksum, then commits
// it into the selected upload root without overwriting existing files.
func (a *AgentRuntime) CompleteUploadSession(deviceID string, uploadID string, input UploadSessionCompleteInput) (UploadSessionActionResponse, error) {
	finalSHA1, err := finalSHA1Checksum(input.Checksums)
	if err != nil {
		return UploadSessionActionResponse{}, ErrUploadRequestInvalid
	}

	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()

	session, err := a.uploadSessionForComplete(deviceID, uploadID)
	if err != nil {
		return UploadSessionActionResponse{}, err
	}
	if input.SourceAssetVersion = strings.TrimSpace(input.SourceAssetVersion); input.SourceAssetVersion != "" && input.SourceAssetVersion != session.SourceAssetVersion {
		return UploadSessionActionResponse{}, ErrUploadRequestInvalid
	}
	if asset, ok, err := a.uploads.GetUploadedAssetByUploadID(session.UploadID); err != nil {
		return UploadSessionActionResponse{}, err
	} else if ok {
		if asset.Status == "uploaded" {
			return completedUploadResponse(asset), nil
		}
		if recovered, ok, err := a.recoverPendingUploadAssetLocked(asset); err != nil {
			return UploadSessionActionResponse{}, err
		} else if ok {
			return completedUploadResponse(recovered), nil
		}
		return UploadSessionActionResponse{}, ErrUploadRequestInvalid
	}
	if session.Status == "completed" {
		return UploadSessionActionResponse{}, ErrUploadRequestInvalid
	}

	_, profile, err := a.deviceProfile(strings.TrimSpace(deviceID))
	if err != nil {
		return UploadSessionActionResponse{}, err
	}
	if sessionStatus := a.uploadSessionPolicyStatus(profile.Upload, session, false); sessionStatus.State != "ready" {
		return UploadSessionActionResponse{}, ErrUploadPolicyInvalid
	}
	root, ok := a.uploadRootConfig(session.SelectedRootKey)
	if !ok {
		return UploadSessionActionResponse{}, ErrUploadPolicyInvalid
	}
	if uploadCapturedAtBlocked(profile.Upload, session.CapturedAt) {
		return UploadSessionActionResponse{}, ErrUploadPolicyInvalid
	}

	tempSize, tempSHA1, err := sha1File(root, session.TempRelativePath)
	if err != nil {
		return UploadSessionActionResponse{}, err
	}
	if session.ExpectedSizeBytes != nil && tempSize != *session.ExpectedSizeBytes {
		return UploadSessionActionResponse{}, ErrUploadRequestInvalid
	}
	if tempSize != session.NextOffset {
		return UploadSessionActionResponse{}, store.ErrUploadSessionOffsetConflict
	}
	if tempSHA1 != finalSHA1 {
		return UploadSessionActionResponse{}, ErrUploadChecksumMismatch
	}

	now := time.Now().UTC()
	baseRelativePath, err := renderUploadFinalRelativePath(profile, session, now, a.uploadLocation())
	if err != nil {
		return UploadSessionActionResponse{}, err
	}
	checksums := []store.UploadChecksum{{
		Algorithm: "sha1",
		Encoding:  "hex",
		Digest:    finalSHA1,
	}}
	uploaded, err := a.reserveAndFinalizeUploadWithRetry(root, session, baseRelativePath, checksums, now)
	if err != nil {
		return UploadSessionActionResponse{}, err
	}
	return completedUploadResponse(uploaded), nil
}

// AbortUploadSession stops an active upload session and removes its temp file
// when the temp path still belongs to the selected root.
func (a *AgentRuntime) AbortUploadSession(deviceID string, uploadID string) (UploadSessionActionResponse, error) {
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()

	session, root, _, err := a.activeUploadSessionContext(deviceID, uploadID)
	if err != nil {
		return UploadSessionActionResponse{}, err
	}
	aborted, err := a.uploads.AbortSession(session.UploadID, session.DeviceID, time.Now().UTC())
	if err != nil {
		return UploadSessionActionResponse{}, err
	}
	if _, err := removeUploadTempFile(root, session.TempRelativePath); err != nil {
		return UploadSessionActionResponse{}, err
	}
	summary := uploadSessionSummary(aborted)
	return UploadSessionActionResponse{
		State:   "aborted",
		Session: &summary,
	}, nil
}

func (a *AgentRuntime) deviceProfile(deviceID string) (store.DeviceRecord, store.DeviceProfile, error) {
	device, err := a.deviceRecord(deviceID)
	if err != nil {
		return store.DeviceRecord{}, store.DeviceProfile{}, err
	}
	profile, err := a.profiles.EnsureProfile(device, time.Now().UTC())
	if err != nil {
		return store.DeviceRecord{}, store.DeviceProfile{}, err
	}
	return device, profile, nil
}

func (a *AgentRuntime) deviceUploadEffectiveStatus(upload store.DeviceUploadProfile) DeviceUploadPolicyStatus {
	if !upload.Enabled {
		return DeviceUploadPolicyStatus{
			State:  "disabled",
			Reason: "Upload is disabled for this device.",
		}
	}
	if strings.TrimSpace(upload.RootKey) == "" {
		return DeviceUploadPolicyStatus{
			State:  "blocked",
			Reason: "Upload root is not selected.",
		}
	}
	root, ok := a.uploadRootConfig(upload.RootKey)
	if !ok {
		return DeviceUploadPolicyStatus{
			State:  "blocked",
			Reason: "Upload root is not configured.",
		}
	}
	rootStatus := uploadRootStatus(root)
	appRootStatus := UploadRootSummary{
		Key:      rootStatus.Key,
		Status:   rootStatus.Status,
		Writable: rootStatus.Writable,
		Message:  rootStatus.Message,
	}
	if !rootStatus.Writable {
		return DeviceUploadPolicyStatus{
			State:  "blocked",
			Reason: rootStatus.Message,
			Root:   &appRootStatus,
		}
	}
	return DeviceUploadPolicyStatus{
		State: "ready",
		Root:  &appRootStatus,
	}
}

func (a *AgentRuntime) uploadRootConfig(rootKey string) (config.UploadRootConfig, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	normalizedRootKey := strings.TrimSpace(rootKey)
	for _, root := range a.config.UploadRoots {
		if root.Key == normalizedRootKey {
			return normalizedUploadRootConfig(root), true
		}
	}
	return config.UploadRootConfig{}, false
}

func deviceUploadPolicy(upload store.DeviceUploadProfile) DeviceUploadPolicy {
	return DeviceUploadPolicy{
		Enabled:       upload.Enabled,
		RootKey:       upload.RootKey,
		PathPattern:   upload.PathPattern,
		CapturedAfter: upload.CapturedAfter,
		UpdatedAt:     upload.UpdatedAt,
	}
}

func normalizeUploadSessionStartInput(input UploadSessionStartInput) (UploadSessionStartInput, error) {
	input.SourceAssetID = strings.TrimSpace(input.SourceAssetID)
	input.SourceAssetVersion = strings.TrimSpace(input.SourceAssetVersion)
	input.MediaType = strings.TrimSpace(input.MediaType)
	input.OriginalFilename = strings.TrimSpace(input.OriginalFilename)
	input.MimeType = strings.TrimSpace(input.MimeType)
	input.ResourceSignature = strings.TrimSpace(input.ResourceSignature)
	for index := range input.ChecksumAlgorithms {
		input.ChecksumAlgorithms[index] = strings.ToLower(strings.TrimSpace(input.ChecksumAlgorithms[index]))
	}
	if input.CapturedAt != nil {
		capturedAt := input.CapturedAt.UTC()
		input.CapturedAt = &capturedAt
	}
	if input.SourceAssetID == "" ||
		input.SourceAssetVersion == "" ||
		input.MediaType == "" ||
		input.OriginalFilename == "" {
		return UploadSessionStartInput{}, ErrUploadRequestInvalid
	}
	if input.ExpectedSizeBytes != nil && *input.ExpectedSizeBytes < 0 {
		return UploadSessionStartInput{}, ErrUploadRequestInvalid
	}
	return input, nil
}

func blockedUploadSessionResponse(status DeviceUploadPolicyStatus) UploadSessionStartResponse {
	return UploadSessionStartResponse{
		State:  "blocked",
		Reason: status.Reason,
		Status: &status,
	}
}

func blockedPendingUploadSessionResponse(asset store.UploadedAsset) UploadSessionStartResponse {
	status := DeviceUploadPolicyStatus{
		State:  "blocked",
		Reason: uploadCommitBlockedReason,
	}
	return UploadSessionStartResponse{
		State:         "blocked",
		Reason:        status.Reason,
		Status:        &status,
		UploadedAsset: uploadedAssetSummary(asset),
	}
}

func uploadSessionSummary(session store.UploadSession) UploadSessionSummary {
	return UploadSessionSummary{
		UploadID:           session.UploadID,
		Status:             session.Status,
		SourceAssetID:      session.SourceAssetID,
		SourceAssetVersion: session.SourceAssetVersion,
		MediaType:          session.MediaType,
		OriginalFilename:   session.OriginalFilename,
		CapturedAt:         session.CapturedAt,
		ExpectedSizeBytes:  session.ExpectedSizeBytes,
		RootKey:            session.SelectedRootKey,
		NextOffset:         session.NextOffset,
		ChunkSizeBytes:     defaultUploadChunkSizeBytes,
		CreatedAt:          session.CreatedAt,
		UpdatedAt:          session.UpdatedAt,
		ExpiresAt:          session.ExpiresAt,
	}
}

func uploadedAssetSummary(asset store.UploadedAsset) *UploadedAssetSummary {
	return &UploadedAssetSummary{
		UploadedAssetID:   asset.ID,
		Status:            asset.Status,
		RootKey:           asset.SelectedRootKey,
		FinalRelativePath: asset.FinalRelativePath,
		UploadedAt:        asset.UploadedAt,
	}
}

func uploadSessionExpired(session store.UploadSession, now time.Time) bool {
	return session.ExpiresAt != nil && !session.ExpiresAt.After(now)
}

func (a *AgentRuntime) activeUploadSessionContext(
	deviceID string,
	uploadID string,
) (store.UploadSession, config.UploadRootConfig, store.DeviceProfile, error) {
	session, root, profile, err := a.uploadSessionContext(deviceID, uploadID, false)
	if err != nil {
		return store.UploadSession{}, config.UploadRootConfig{}, store.DeviceProfile{}, err
	}
	if session.Status != "active" || uploadSessionExpired(session, time.Now().UTC()) {
		return store.UploadSession{}, config.UploadRootConfig{}, store.DeviceProfile{}, store.ErrUploadSessionNotFound
	}
	return session, root, profile, nil
}

func (a *AgentRuntime) uploadSessionContext(
	deviceID string,
	uploadID string,
	allowCommitting bool,
) (store.UploadSession, config.UploadRootConfig, store.DeviceProfile, error) {
	return a.uploadSessionContextWithCapturedAfter(deviceID, uploadID, allowCommitting, true)
}

func (a *AgentRuntime) uploadSessionContextWithCapturedAfter(
	deviceID string,
	uploadID string,
	allowCommitting bool,
	checkCapturedAfter bool,
) (store.UploadSession, config.UploadRootConfig, store.DeviceProfile, error) {
	normalizedDeviceID := strings.TrimSpace(deviceID)
	session, ok, err := a.uploads.GetSession(uploadID)
	if err != nil {
		return store.UploadSession{}, config.UploadRootConfig{}, store.DeviceProfile{}, err
	}
	if !ok || session.DeviceID != normalizedDeviceID {
		return store.UploadSession{}, config.UploadRootConfig{}, store.DeviceProfile{}, store.ErrUploadSessionNotFound
	}
	if session.Status != "active" && !(allowCommitting && session.Status == "committing") {
		if session.Status == "completed" {
			return session, config.UploadRootConfig{}, store.DeviceProfile{}, nil
		}
		return store.UploadSession{}, config.UploadRootConfig{}, store.DeviceProfile{}, store.ErrUploadSessionNotFound
	}
	_, profile, err := a.deviceProfile(normalizedDeviceID)
	if err != nil {
		return store.UploadSession{}, config.UploadRootConfig{}, store.DeviceProfile{}, err
	}
	if sessionStatus := a.uploadSessionPolicyStatus(profile.Upload, session, checkCapturedAfter); sessionStatus.State != "ready" {
		return store.UploadSession{}, config.UploadRootConfig{}, store.DeviceProfile{}, ErrUploadPolicyInvalid
	}
	root, ok := a.uploadRootConfig(session.SelectedRootKey)
	if !ok {
		return store.UploadSession{}, config.UploadRootConfig{}, store.DeviceProfile{}, ErrUploadPolicyInvalid
	}
	return session, root, profile, nil
}

func (a *AgentRuntime) uploadSessionPolicyStatus(
	upload store.DeviceUploadProfile,
	session store.UploadSession,
	checkCapturedAfter bool,
) DeviceUploadPolicyStatus {
	if !upload.Enabled {
		return DeviceUploadPolicyStatus{
			State:  "disabled",
			Reason: "Upload is disabled for this device.",
		}
	}
	rootKey := strings.TrimSpace(upload.RootKey)
	if rootKey == "" {
		return DeviceUploadPolicyStatus{
			State:  "blocked",
			Reason: "Upload root is not selected.",
		}
	}
	if rootKey != session.SelectedRootKey {
		return DeviceUploadPolicyStatus{
			State:  "blocked",
			Reason: uploadSessionPolicyReason,
		}
	}
	if _, ok := a.uploadRootConfig(session.SelectedRootKey); !ok {
		return DeviceUploadPolicyStatus{
			State:  "blocked",
			Reason: "Upload root is not configured.",
		}
	}
	if checkCapturedAfter && uploadCapturedAtBlocked(upload, session.CapturedAt) {
		return uploadCapturedBeforePolicyStatus()
	}
	return DeviceUploadPolicyStatus{
		State: "ready",
	}
}

func uploadCapturedAtBlocked(upload store.DeviceUploadProfile, capturedAt *time.Time) bool {
	return upload.CapturedAfter != nil &&
		(capturedAt == nil || capturedAt.Before(*upload.CapturedAfter))
}

func uploadCapturedBeforePolicyStatus() DeviceUploadPolicyStatus {
	return DeviceUploadPolicyStatus{
		State:  "blocked",
		Reason: uploadCapturedBeforeReason,
	}
}

func completedUploadResponse(asset store.UploadedAsset) UploadSessionActionResponse {
	return UploadSessionActionResponse{
		State:         "completed",
		UploadedAsset: uploadedAssetSummary(asset),
	}
}

func (a *AgentRuntime) uploadSessionForComplete(deviceID string, uploadID string) (store.UploadSession, error) {
	normalizedDeviceID := strings.TrimSpace(deviceID)
	session, ok, err := a.uploads.GetSession(uploadID)
	if err != nil {
		return store.UploadSession{}, err
	}
	if !ok || session.DeviceID != normalizedDeviceID {
		return store.UploadSession{}, store.ErrUploadSessionNotFound
	}
	if session.Status != "active" && session.Status != "committing" && session.Status != "completed" {
		return store.UploadSession{}, store.ErrUploadSessionNotFound
	}
	return session, nil
}

func normalizeSHA1Hex(value string) (string, error) {
	digest := strings.ToLower(strings.TrimSpace(value))
	if len(digest) != sha1.Size*2 {
		return "", ErrUploadRequestInvalid
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", ErrUploadRequestInvalid
	}
	return digest, nil
}

func finalSHA1Checksum(checksums []UploadSessionChecksumInput) (string, error) {
	for _, checksum := range checksums {
		algorithm := strings.ToLower(strings.TrimSpace(checksum.Algorithm))
		encoding := strings.ToLower(strings.TrimSpace(checksum.Encoding))
		if encoding == "" {
			encoding = "hex"
		}
		if algorithm != "sha1" {
			continue
		}
		if encoding != "hex" {
			return "", ErrUploadRequestInvalid
		}
		return normalizeSHA1Hex(checksum.Digest)
	}
	return "", ErrUploadRequestInvalid
}

func appendUploadChunkToTemp(
	root config.UploadRootConfig,
	tempRelativePath string,
	offset int64,
	body io.Reader,
	contentLength int64,
) (int64, string, error) {
	tempPath, err := prepareUploadTempFile(root, tempRelativePath, offset)
	if err != nil {
		return 0, "", err
	}
	file, err := os.OpenFile(tempPath, os.O_WRONLY, 0)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return 0, "", err
	}
	hasher := sha1.New()
	limited := io.LimitReader(body, contentLength+1)
	written, err := io.Copy(file, io.TeeReader(limited, hasher))
	if err != nil {
		_ = file.Truncate(offset)
		return 0, "", err
	}
	if written != contentLength {
		_ = file.Truncate(offset)
		return 0, "", ErrUploadRequestInvalid
	}
	if err := file.Sync(); err != nil {
		_ = file.Truncate(offset)
		return 0, "", err
	}
	return written, hex.EncodeToString(hasher.Sum(nil)), nil
}

func prepareUploadTempFile(root config.UploadRootConfig, tempRelativePath string, offset int64) (string, error) {
	tempPath, err := uploadTempAbsolutePath(root, tempRelativePath)
	if err != nil {
		return "", err
	}
	if err := ensureUploadTempDir(root); err != nil {
		return "", err
	}
	if info, err := os.Lstat(tempPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		file, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, uploadCreatedFileMode)
		if err != nil {
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
	} else if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
		return "", ErrUploadPolicyInvalid
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		return "", err
	}
	if info.Size() != offset {
		return "", store.ErrUploadSessionOffsetConflict
	}
	return tempPath, nil
}

func ensureUploadTempDir(root config.UploadRootConfig) error {
	cleanTempDir, err := uploadTempRelativeDir(root)
	if err != nil {
		return err
	}
	current := root.Path
	for _, segment := range strings.Split(cleanTempDir, "/") {
		current = filepath.Join(current, filepath.FromSlash(segment))
		if info, err := os.Lstat(current); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return ErrUploadPolicyInvalid
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Mkdir(current, uploadCreatedDirMode); err != nil {
			return err
		}
	}
	return nil
}

func uploadTempDirExists(root config.UploadRootConfig) (bool, error) {
	cleanTempDir, err := uploadTempRelativeDir(root)
	if err != nil {
		return false, err
	}
	current := root.Path
	for _, segment := range strings.Split(cleanTempDir, "/") {
		current = filepath.Join(current, filepath.FromSlash(segment))
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, ErrUploadPolicyInvalid
		}
	}
	return true, nil
}

func uploadTempAbsoluteDir(root config.UploadRootConfig) (string, error) {
	cleanTempDir, err := uploadTempRelativeDir(root)
	if err != nil {
		return "", err
	}
	return uploadRootChildPath(root.Path, cleanTempDir)
}

func uploadTempRelativeDir(root config.UploadRootConfig) (string, error) {
	cleanTempDir, err := config.ValidateUploadRootTempPath(root.TempPath)
	if err != nil {
		return "", ErrUploadPolicyInvalid
	}
	return cleanTempDir, nil
}

func truncateUploadTempFile(root config.UploadRootConfig, tempRelativePath string, offset int64) error {
	tempPath, err := uploadTempAbsolutePath(root, tempRelativePath)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(tempPath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Truncate(offset)
}

func uploadTempAbsolutePath(root config.UploadRootConfig, tempRelativePath string) (string, error) {
	cleanRelative := path.Clean(strings.TrimSpace(tempRelativePath))
	cleanTempDir, err := uploadTempRelativeDir(root)
	if err != nil {
		return "", err
	}
	if cleanRelative == "." || path.IsAbs(cleanRelative) || path.Dir(cleanRelative) != cleanTempDir {
		return "", ErrUploadPolicyInvalid
	}
	if !strings.HasPrefix(path.Base(cleanRelative), "upl_") || !strings.HasSuffix(path.Base(cleanRelative), ".part") {
		return "", ErrUploadPolicyInvalid
	}
	return uploadRootChildPath(root.Path, cleanRelative)
}

func uploadRootChildPath(rootPath string, relativePath string) (string, error) {
	cleanRelative := path.Clean(strings.TrimSpace(relativePath))
	if cleanRelative == "." || path.IsAbs(cleanRelative) || strings.HasPrefix(cleanRelative, "../") || cleanRelative == ".." {
		return "", ErrUploadPolicyInvalid
	}
	fullPath := filepath.Join(rootPath, filepath.FromSlash(cleanRelative))
	relativeToRoot, err := filepath.Rel(rootPath, fullPath)
	if err != nil {
		return "", err
	}
	if relativeToRoot == ".." || strings.HasPrefix(relativeToRoot, ".."+string(os.PathSeparator)) {
		return "", ErrUploadPolicyInvalid
	}
	return fullPath, nil
}

func sha1File(root config.UploadRootConfig, tempRelativePath string) (int64, string, error) {
	exists, err := uploadTempDirExists(root)
	if err != nil {
		return 0, "", err
	}
	if !exists {
		return 0, "", os.ErrNotExist
	}
	fullPath, err := uploadTempAbsolutePath(root, tempRelativePath)
	if err != nil {
		return 0, "", err
	}
	return sha1RegularFile(fullPath)
}

func sha1RegularFile(fullPath string) (int64, string, error) {
	info, err := os.Lstat(fullPath)
	if err != nil {
		return 0, "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, "", ErrUploadPolicyInvalid
	}
	file, err := os.Open(fullPath)
	if err != nil {
		return 0, "", err
	}
	defer file.Close()
	hasher := sha1.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return 0, "", err
	}
	return info.Size(), hex.EncodeToString(hasher.Sum(nil)), nil
}

func finalizeUploadFile(root config.UploadRootConfig, tempRelativePath string, finalRelativePath string) error {
	exists, err := uploadTempDirExists(root)
	if err != nil {
		return err
	}
	if !exists {
		return os.ErrNotExist
	}
	tempPath, err := uploadTempAbsolutePath(root, tempRelativePath)
	if err != nil {
		return err
	}
	finalPath, err := uploadRootChildPath(root.Path, finalRelativePath)
	if err != nil {
		return err
	}
	if err := ensureUploadFinalParent(root.Path, path.Dir(path.Clean(finalRelativePath))); err != nil {
		return err
	}
	if err := os.Link(tempPath, finalPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrUploadFinalPathConflict
		}
		return err
	}
	if err := os.Remove(tempPath); err != nil {
		return err
	}
	return nil
}

func ensureUploadFinalParent(rootPath string, parentRelativePath string) error {
	cleanParent := path.Clean(strings.TrimSpace(parentRelativePath))
	if cleanParent == "." {
		return nil
	}
	if path.IsAbs(cleanParent) || cleanParent == ".." || strings.HasPrefix(cleanParent, "../") {
		return ErrUploadPolicyInvalid
	}
	current := rootPath
	for _, segment := range strings.Split(cleanParent, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return ErrUploadPolicyInvalid
		}
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return ErrUploadPolicyInvalid
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Mkdir(current, uploadCreatedDirMode); err != nil {
			return err
		}
	}
	return nil
}

func renderUploadFinalRelativePath(profile store.DeviceProfile, session store.UploadSession, now time.Time, location *time.Location) (string, error) {
	pattern := strings.TrimSpace(profile.Upload.PathPattern)
	if pattern == "" {
		pattern = store.DefaultUploadPathPattern()
	}
	if err := validateUploadPathPattern(pattern); err != nil {
		return "", err
	}
	capturedAt := now
	if session.CapturedAt != nil {
		capturedAt = *session.CapturedAt
	}
	localCapturedAt := capturedAt.In(location)
	filename := sanitizeUploadPathSegment(session.OriginalFilename)
	ext := strings.TrimPrefix(path.Ext(filename), ".")
	name := strings.TrimSuffix(filename, path.Ext(filename))
	values := map[string]string{
		"deviceName": profile.DeviceName,
		"deviceId":   profile.DeviceID,
		"yyyy":       strconv.Itoa(localCapturedAt.Year()),
		"MM":         fmt.Sprintf("%02d", int(localCapturedAt.Month())),
		"dd":         fmt.Sprintf("%02d", localCapturedAt.Day()),
		"assetId":    session.SourceAssetID,
		"filename":   filename,
		"name":       name,
		"ext":        ext,
		"mediaType":  session.MediaType,
	}
	rendered := pattern
	for token, value := range values {
		rendered = strings.ReplaceAll(rendered, "{"+token+"}", sanitizeUploadPathSegment(value))
	}
	segments := strings.Split(rendered, "/")
	for index, segment := range segments {
		segments[index] = sanitizeUploadPathSegment(segment)
		if segments[index] == "." || segments[index] == ".." {
			return "", ErrUploadPolicyInvalid
		}
	}
	relativePath := path.Clean(strings.Join(segments, "/"))
	if relativePath == "." || path.IsAbs(relativePath) || strings.HasPrefix(relativePath, "../") || relativePath == ".." {
		return "", ErrUploadPolicyInvalid
	}
	return relativePath, nil
}

func sanitizeUploadPathSegment(value string) string {
	trimmed := strings.TrimSpace(value)
	var builder strings.Builder
	lastUnderscore := false
	for _, char := range trimmed {
		replace := char < 0x20 || char == 0x7f
		switch char {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			replace = true
		}
		if replace {
			if !lastUnderscore {
				builder.WriteByte('_')
				lastUnderscore = true
			}
			continue
		}
		builder.WriteRune(char)
		lastUnderscore = false
	}
	sanitized := strings.Trim(builder.String(), " .")
	if sanitized == "" || sanitized == "." || sanitized == ".." {
		return "_"
	}
	return sanitized
}

func suffixedUploadRelativePath(relativePath string, attempt int) string {
	if attempt == 0 {
		return relativePath
	}
	ext := path.Ext(relativePath)
	base := strings.TrimSuffix(relativePath, ext)
	return fmt.Sprintf("%s-%d%s", base, attempt+1, ext)
}

func (a *AgentRuntime) uploadLocation() *time.Location {
	a.mu.RLock()
	timezone := strings.TrimSpace(a.config.Timezone)
	a.mu.RUnlock()
	if timezone == "" {
		return time.Local
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Local
	}
	return location
}

func (a *AgentRuntime) recoverPendingUploadAsset(asset store.UploadedAsset) (store.UploadedAsset, bool, error) {
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()

	return a.recoverPendingUploadAssetLocked(asset)
}

func (a *AgentRuntime) recoverPendingUploadAssetLocked(asset store.UploadedAsset) (store.UploadedAsset, bool, error) {
	if asset.Status == "uploaded" {
		return asset, true, nil
	}
	root, ok := a.uploadRootConfig(asset.SelectedRootKey)
	if !ok {
		return asset, false, nil
	}
	finalPath, err := uploadRootChildPath(root.Path, asset.FinalRelativePath)
	if err != nil {
		return store.UploadedAsset{}, false, err
	}
	if finalOK, err := uploadedFileMatches(finalPath, asset.ExpectedSizeBytes, asset.Checksums); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return store.UploadedAsset{}, false, err
		}
	} else if finalOK {
		uploaded, err := a.uploads.MarkUploaded(asset.ID, time.Now().UTC())
		if err != nil {
			return store.UploadedAsset{}, false, err
		}
		if session, ok, err := a.uploads.GetSession(asset.UploadID); err != nil {
			return store.UploadedAsset{}, false, err
		} else if ok {
			_, _ = removeUploadTempFile(root, session.TempRelativePath)
		}
		return uploaded, true, nil
	}
	session, ok, err := a.uploads.GetSession(asset.UploadID)
	if err != nil {
		return store.UploadedAsset{}, false, err
	}
	if !ok {
		return asset, false, nil
	}
	tempPath, err := uploadTempAbsolutePath(root, session.TempRelativePath)
	if err != nil {
		return store.UploadedAsset{}, false, err
	}
	if tempOK, err := uploadedFileMatches(tempPath, asset.ExpectedSizeBytes, asset.Checksums); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return asset, false, nil
		}
		return store.UploadedAsset{}, false, err
	} else if !tempOK {
		return asset, false, nil
	}
	uploaded, err := a.reserveAndFinalizeUploadWithRetry(root, session, asset.FinalRelativePath, asset.Checksums, time.Now().UTC())
	if err != nil {
		if errors.Is(err, ErrUploadFinalPathConflict) {
			return asset, false, nil
		}
		return store.UploadedAsset{}, false, err
	}
	return uploaded, true, nil
}

func (a *AgentRuntime) reserveAndFinalizeUploadWithRetry(
	root config.UploadRootConfig,
	session store.UploadSession,
	baseRelativePath string,
	checksums []store.UploadChecksum,
	now time.Time,
) (store.UploadedAsset, error) {
	for attempt := 0; attempt < 1000; attempt++ {
		finalRelativePath := suffixedUploadRelativePath(baseRelativePath, attempt)
		asset, err := a.uploads.ReserveUploadCommit(store.UploadCommitInput{
			UploadID:           session.UploadID,
			DeviceID:           session.DeviceID,
			SourceAssetID:      session.SourceAssetID,
			SourceAssetVersion: session.SourceAssetVersion,
			MediaType:          session.MediaType,
			OriginalFilename:   session.OriginalFilename,
			CapturedAt:         session.CapturedAt,
			ExpectedSizeBytes:  session.ExpectedSizeBytes,
			FinalRelativePath:  finalRelativePath,
			Checksums:          checksums,
			Now:                now,
		})
		if err != nil {
			if errors.Is(err, store.ErrUploadedAssetExists) && asset.Status == "uploaded" {
				return asset, nil
			}
			return store.UploadedAsset{}, err
		}
		if err := finalizeUploadFile(root, session.TempRelativePath, finalRelativePath); err != nil {
			if errors.Is(err, ErrUploadFinalPathConflict) {
				continue
			}
			return store.UploadedAsset{}, err
		}
		return a.uploads.MarkUploaded(asset.ID, time.Now().UTC())
	}
	return store.UploadedAsset{}, ErrUploadFinalPathConflict
}

func uploadedFileMatches(fullPath string, expectedSize *int64, checksums []store.UploadChecksum) (bool, error) {
	size, digest, err := sha1RegularFile(fullPath)
	if err != nil {
		return false, err
	}
	if expectedSize != nil && size != *expectedSize {
		return false, nil
	}
	for _, checksum := range checksums {
		if strings.ToLower(strings.TrimSpace(checksum.Algorithm)) != "sha1" {
			continue
		}
		if strings.ToLower(strings.TrimSpace(checksum.Encoding)) != "hex" {
			return false, nil
		}
		expected, err := normalizeSHA1Hex(checksum.Digest)
		if err != nil {
			return false, nil
		}
		return digest == expected, nil
	}
	return false, nil
}

func randomUploadID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "upl_" + hex.EncodeToString(raw[:]), nil
}
