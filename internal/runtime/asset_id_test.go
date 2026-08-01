package runtime

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestAssetIDKeysIssueV2AndDecodeReleasedV1(t *testing.T) {
	t.Parallel()

	signingKey := bytes.Repeat([]byte{0x42}, 32)
	keys, err := deriveAssetIDKeys(base64.RawStdEncoding.EncodeToString(signingKey))
	if err != nil {
		t.Fatalf("deriveAssetIDKeys() error = %v", err)
	}
	sourceKey := "1122334455667788"
	upstreamAssetID := "released-client-asset"

	current, err := encodeTimichAssetID(keys, sourceKey, upstreamAssetID)
	if err != nil {
		t.Fatalf("encodeTimichAssetID() error = %v", err)
	}
	if !strings.HasPrefix(current, timichAssetIDPrefix) {
		t.Fatalf("current asset ID = %q, want %s prefix", current, timichAssetIDPrefix)
	}

	payloadSource, err := hex.DecodeString(sourceKey)
	if err != nil {
		t.Fatalf("decode source key: %v", err)
	}
	payload := append(append([]byte(nil), payloadSource...), []byte(upstreamAssetID)...)
	mac := hmac.New(sha256.New, keys.legacy)
	_, _ = mac.Write(payload)
	legacy := timichLegacyAssetIDPrefix +
		base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:timichLegacyAssetIDSignatureSize])

	decodedSource, decodedUpstream, err := decodeTimichAssetID(keys, legacy)
	if err != nil {
		t.Fatalf("decodeTimichAssetID(legacy) error = %v", err)
	}
	if decodedSource != sourceKey || decodedUpstream != upstreamAssetID {
		t.Fatalf("legacy decode = %q/%q, want %q/%q", decodedSource, decodedUpstream, sourceKey, upstreamAssetID)
	}
}

func TestLegacyAssetIDRejectsCurrentKeySignature(t *testing.T) {
	t.Parallel()

	keys := assetIDKeys{
		current: bytes.Repeat([]byte{1}, 32),
		legacy:  bytes.Repeat([]byte{2}, 32),
	}
	payload := append(bytes.Repeat([]byte{3}, timichAssetIDSourceKeySize), []byte("asset")...)
	mac := hmac.New(sha256.New, keys.current)
	_, _ = mac.Write(payload)
	value := timichLegacyAssetIDPrefix +
		base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:timichLegacyAssetIDSignatureSize])

	if _, _, err := decodeTimichAssetID(keys, value); err == nil {
		t.Fatal("decodeTimichAssetID() error = nil, want v1 key rejection")
	}
}
