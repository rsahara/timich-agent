package runtime

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	timichAssetIDPrefix        = "ta1_"
	timichAssetIDKeyPurpose    = "timich-asset-id-v1"
	timichAssetIDSignatureSize = 16
	timichAssetIDSourceKeySize = 8
	timichAssetIDMaxUpstreamID = 512
)

var errInvalidAssetID = errors.New("invalid asset id")

func deriveAssetIDKey(encodedSessionSigningKey string) ([]byte, error) {
	key, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(encodedSessionSigningKey))
	if err != nil {
		return nil, fmt.Errorf("decode asset id signing key: %w", err)
	}
	if len(key) == 0 {
		return nil, errors.New("asset id signing key is empty")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(timichAssetIDKeyPurpose))
	return mac.Sum(nil), nil
}

func encodeTimichAssetID(assetIDKey []byte, sourceKey string, upstreamAssetID string) (string, error) {
	sourceKeyBytes, err := hex.DecodeString(strings.TrimSpace(sourceKey))
	if err != nil || len(sourceKeyBytes) != timichAssetIDSourceKeySize {
		return "", fmt.Errorf("%w: invalid source key", errInvalidAssetID)
	}
	upstreamAssetID = strings.TrimSpace(upstreamAssetID)
	if upstreamAssetID == "" || len([]byte(upstreamAssetID)) > timichAssetIDMaxUpstreamID {
		return "", fmt.Errorf("%w: invalid upstream asset id", errInvalidAssetID)
	}

	payload := make([]byte, 0, timichAssetIDSourceKeySize+len(upstreamAssetID))
	payload = append(payload, sourceKeyBytes...)
	payload = append(payload, []byte(upstreamAssetID)...)
	signature := signAssetIDPayload(assetIDKey, payload)
	return timichAssetIDPrefix +
		base64.RawURLEncoding.EncodeToString(payload) +
		"." +
		base64.RawURLEncoding.EncodeToString(signature), nil
}

func decodeTimichAssetID(assetIDKey []byte, value string) (sourceKey string, upstreamAssetID string, err error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, timichAssetIDPrefix) {
		return "", "", errInvalidAssetID
	}
	parts := strings.Split(strings.TrimPrefix(value, timichAssetIDPrefix), ".")
	if len(parts) != 2 {
		return "", "", errInvalidAssetID
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", errInvalidAssetID
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", errInvalidAssetID
	}
	if len(payload) <= timichAssetIDSourceKeySize ||
		len(payload)-timichAssetIDSourceKeySize > timichAssetIDMaxUpstreamID ||
		len(signature) != timichAssetIDSignatureSize {
		return "", "", errInvalidAssetID
	}
	expectedSignature := signAssetIDPayload(assetIDKey, payload)
	if !hmac.Equal(signature, expectedSignature) {
		return "", "", errInvalidAssetID
	}
	sourceKey = hex.EncodeToString(payload[:timichAssetIDSourceKeySize])
	upstreamAssetID = string(payload[timichAssetIDSourceKeySize:])
	if strings.TrimSpace(upstreamAssetID) == "" {
		return "", "", errInvalidAssetID
	}
	return sourceKey, upstreamAssetID, nil
}

func signAssetIDPayload(assetIDKey []byte, payload []byte) []byte {
	mac := hmac.New(sha256.New, assetIDKey)
	_, _ = mac.Write(payload)
	sum := mac.Sum(nil)
	return sum[:timichAssetIDSignatureSize]
}
