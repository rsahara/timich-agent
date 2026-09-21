package runtime

import (
	"github.com/rsahara/timich-agent/internal/store"
	"strings"
)

const maxUploadCheckItems = 200

// UploadSyncCapability allows clients to retain reset-aware completion evidence.
type UploadSyncCapability struct {
	Version       int    `json:"version"`
	LedgerEpoch   string `json:"ledgerEpoch"`
	MaxCheckItems int    `json:"maxCheckItems"`
}

type UploadCheckIdentity struct {
	SourceAssetID      string `json:"sourceAssetId"`
	SourceAssetVersion string `json:"sourceAssetVersion"`
}

type UploadCheckInput struct {
	Items []UploadCheckIdentity `json:"items"`
}
type UploadCheckResult struct {
	UploadCheckIdentity
	State string `json:"state"`
}
type UploadCheckResponse struct {
	LedgerEpoch string              `json:"ledgerEpoch"`
	Items       []UploadCheckResult `json:"items"`
}

// CheckUploads returns evidence for only the caller's identities, in request order.
func (a *AgentRuntime) CheckUploads(deviceID string, input UploadCheckInput) (UploadCheckResponse, error) {
	_, profile, err := a.deviceProfile(deviceID)
	if err != nil {
		return UploadCheckResponse{}, err
	}
	if len(input.Items) < 1 || len(input.Items) > maxUploadCheckItems {
		return UploadCheckResponse{}, ErrUploadRequestInvalid
	}
	identities := make([]store.UploadIdentity, len(input.Items))
	seen := make(map[store.UploadIdentity]bool, len(input.Items))
	for i, item := range input.Items {
		// Reject noncanonical identities instead of returning different IDs than requested.
		if item.SourceAssetID == "" || item.SourceAssetVersion == "" ||
			strings.TrimSpace(item.SourceAssetID) != item.SourceAssetID || strings.TrimSpace(item.SourceAssetVersion) != item.SourceAssetVersion ||
			len(item.SourceAssetID) > 4096 || len(item.SourceAssetVersion) > 4096 {
			return UploadCheckResponse{}, ErrUploadRequestInvalid
		}
		identity := store.UploadIdentity{SourceAssetID: item.SourceAssetID, SourceAssetVersion: item.SourceAssetVersion}
		if seen[identity] {
			return UploadCheckResponse{}, ErrUploadRequestInvalid
		}
		seen[identity] = true
		identities[i] = identity
	}
	epoch, states, err := a.uploads.CheckUploaded(profile.DeviceID, identities)
	if err != nil {
		return UploadCheckResponse{}, err
	}
	response := UploadCheckResponse{LedgerEpoch: epoch, Items: make([]UploadCheckResult, len(states))}
	for i, uploaded := range states {
		state := "needs_start"
		if uploaded {
			state = "uploaded"
		}
		response.Items[i] = UploadCheckResult{UploadCheckIdentity: input.Items[i], State: state}
	}
	return response, nil
}
