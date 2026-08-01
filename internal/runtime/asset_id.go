package runtime

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	timichAssetIDPrefix              = "ta2_"
	timichAssetIDKeyPurpose          = "timich-asset-id-v2"
	timichAssetIDNoncePurpose        = "timich-asset-id-nonce-v2"
	timichLegacyAssetIDPrefix        = "ta1_"
	timichLegacyAssetIDKeyPurpose    = "timich-asset-id-v1"
	timichLegacyAssetIDSignatureSize = 16
	timichAssetIDSourceKeySize       = 8
	timichAssetIDMaxUpstreamID       = 512
)

var errInvalidAssetID = errors.New("invalid asset id")

type assetIDKeys struct {
	current []byte
	legacy  []byte
}

func deriveAssetIDKeys(encodedSessionSigningKey string) (assetIDKeys, error) {
	key, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(encodedSessionSigningKey))
	if err != nil {
		return assetIDKeys{}, fmt.Errorf("decode asset id signing key: %w", err)
	}
	if len(key) == 0 {
		return assetIDKeys{}, errors.New("asset id signing key is empty")
	}
	return assetIDKeys{
		current: deriveAssetIDPurposeKey(key, timichAssetIDKeyPurpose),
		legacy:  deriveAssetIDPurposeKey(key, timichLegacyAssetIDKeyPurpose),
	}, nil
}

func deriveAssetIDPurposeKey(key []byte, purpose string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(purpose))
	return mac.Sum(nil)
}

func (keys assetIDKeys) clone() assetIDKeys {
	return assetIDKeys{
		current: append([]byte(nil), keys.current...),
		legacy:  append([]byte(nil), keys.legacy...),
	}
}

func encodeTimichAssetID(keys assetIDKeys, sourceKey string, upstreamAssetID string) (string, error) {
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
	aead, err := assetIDAEAD(keys.current)
	if err != nil {
		return "", err
	}
	nonce := assetIDNonce(keys.current, payload, aead.NonceSize())
	sealed := aead.Seal(nil, nonce, payload, []byte(timichAssetIDPrefix))
	encoded := append(append(make([]byte, 0, len(nonce)+len(sealed)), nonce...), sealed...)
	return timichAssetIDPrefix + base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeTimichAssetID(keys assetIDKeys, value string) (sourceKey string, upstreamAssetID string, err error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, timichLegacyAssetIDPrefix) {
		return decodeLegacyTimichAssetID(keys.legacy, value)
	}
	if !strings.HasPrefix(value, timichAssetIDPrefix) {
		return "", "", errInvalidAssetID
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, timichAssetIDPrefix))
	if err != nil {
		return "", "", errInvalidAssetID
	}
	aead, err := assetIDAEAD(keys.current)
	if err != nil {
		return "", "", err
	}
	if len(raw) <= aead.NonceSize()+aead.Overhead()+timichAssetIDSourceKeySize {
		return "", "", errInvalidAssetID
	}
	nonce := raw[:aead.NonceSize()]
	payload, err := aead.Open(nil, nonce, raw[aead.NonceSize():], []byte(timichAssetIDPrefix))
	if err != nil || len(payload) <= timichAssetIDSourceKeySize || len(payload)-timichAssetIDSourceKeySize > timichAssetIDMaxUpstreamID {
		return "", "", errInvalidAssetID
	}
	sourceKey = hex.EncodeToString(payload[:timichAssetIDSourceKeySize])
	upstreamAssetID = string(payload[timichAssetIDSourceKeySize:])
	if strings.TrimSpace(upstreamAssetID) == "" {
		return "", "", errInvalidAssetID
	}
	return sourceKey, upstreamAssetID, nil
}

func decodeLegacyTimichAssetID(assetIDKey []byte, value string) (sourceKey string, upstreamAssetID string, err error) {
	parts := strings.Split(strings.TrimPrefix(value, timichLegacyAssetIDPrefix), ".")
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
		len(signature) != timichLegacyAssetIDSignatureSize {
		return "", "", errInvalidAssetID
	}
	mac := hmac.New(sha256.New, assetIDKey)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)[:timichLegacyAssetIDSignatureSize]) {
		return "", "", errInvalidAssetID
	}
	sourceKey = hex.EncodeToString(payload[:timichAssetIDSourceKeySize])
	upstreamAssetID = string(payload[timichAssetIDSourceKeySize:])
	if strings.TrimSpace(upstreamAssetID) == "" {
		return "", "", errInvalidAssetID
	}
	return sourceKey, upstreamAssetID, nil
}

func assetIDAEAD(assetIDKey []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(assetIDKey)
	if err != nil {
		return nil, fmt.Errorf("create asset id cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create asset id AEAD: %w", err)
	}
	return aead, nil
}

func assetIDNonce(assetIDKey []byte, payload []byte, size int) []byte {
	mac := hmac.New(sha256.New, assetIDKey)
	_, _ = mac.Write([]byte(timichAssetIDNoncePurpose))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(payload)
	sum := mac.Sum(nil)
	return sum[:size]
}
