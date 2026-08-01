package semanticruntimehelper

import (
	"encoding/hex"
	"strings"
)

const onnxRuntimeServerURLPrefix = "TIMICH_ONNX_SERVER_URL_"

// ONNXRuntimeServerEnvKey encodes the complete model/vector-space tuple without
// collapsing punctuation, so distinct runtime identities cannot share a
// process-global endpoint variable.
func ONNXRuntimeServerEnvKey(modelID string, vectorSpaceID string) string {
	modelHex := strings.ToUpper(hex.EncodeToString([]byte(strings.TrimSpace(modelID))))
	vectorHex := strings.ToUpper(hex.EncodeToString([]byte(strings.TrimSpace(vectorSpaceID))))
	return onnxRuntimeServerURLPrefix + "M_" + modelHex + "_V_" + vectorHex
}
