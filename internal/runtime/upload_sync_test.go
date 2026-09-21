package runtime

import (
	"errors"
	"testing"
)

func TestUploadCheckValidatesRequestsAndScopesDevices(t *testing.T) {
	r := newTestAgentRuntime(t, BuildInfo{}, nil)
	bundle, err := r.CreateHostedSession("Check device", "https://timich.example")
	if err != nil {
		t.Fatal(err)
	}
	state, err := r.AppUploadState(bundle.DeviceID)
	if err != nil || state.Sync.Version != 1 || state.Sync.LedgerEpoch == "" || state.Sync.MaxCheckItems != 200 {
		t.Fatalf("sync = %+v %v", state.Sync, err)
	}
	identity := UploadCheckIdentity{SourceAssetID: "asset", SourceAssetVersion: "version"}
	response, err := r.CheckUploads(bundle.DeviceID, UploadCheckInput{Items: []UploadCheckIdentity{identity}})
	if err != nil || response.LedgerEpoch != state.Sync.LedgerEpoch || response.Items[0].State != "needs_start" {
		t.Fatalf("check = %+v %v", response, err)
	}
	if _, err := r.CheckUploads("unknown", UploadCheckInput{Items: []UploadCheckIdentity{identity}}); err == nil {
		t.Fatal("unknown device was accepted")
	}
	tooMany := make([]UploadCheckIdentity, 201)
	for _, items := range [][]UploadCheckIdentity{nil, tooMany, {identity, identity}, {{SourceAssetID: ""}}, {{SourceAssetID: " asset", SourceAssetVersion: "version"}}} {
		if _, err := r.CheckUploads(bundle.DeviceID, UploadCheckInput{Items: items}); !errors.Is(err, ErrUploadRequestInvalid) {
			t.Fatalf("invalid request returned %v", err)
		}
	}
}
