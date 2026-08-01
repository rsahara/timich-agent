package adminapi

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAgentAdminOpenAPICoversJSONRoutes(t *testing.T) {
	t.Parallel()

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	var contractPath string
	var content []byte
	for _, relativePath := range []string{
		"../.././packages/contracts/openapi/agent-admin.yaml",
		"./packages/contracts/openapi/agent-admin.yaml",
	} {
		candidate := filepath.Clean(filepath.Join(filepath.Dir(currentFile), relativePath))
		var err error
		content, err = os.ReadFile(candidate)
		if err == nil {
			contractPath = candidate
			break
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read Agent Admin OpenAPI contract %s: %v", candidate, err)
		}
	}
	if contractPath == "" {
		t.Fatal("locate Agent Admin OpenAPI contract")
	}
	contract := string(content)
	if !strings.Contains(contract, "DatasourceListJSONResponse") {
		t.Fatalf("OpenAPI contract %s does not contain DatasourceListJSONResponse", contractPath)
	}
	required := map[string][]string{
		"/v1/datasources":                               {"get", "post"},
		"/v1/datasources/indexing":                      {"get"},
		"/v1/datasources/indexing/run":                  {"post"},
		"/v1/catalog/dedup/status":                      {"get"},
		"/v1/catalog/dedup/repair":                      {"post"},
		"/v1/datasources/local/scan":                    {"get", "post"},
		"/v1/datasources/local/root/accept":             {"post"},
		"/v1/datasources/local/immich-fallback":         {"put"},
		"/v1/datasources/local/phase0-diagnostics.csv":  {"get"},
		"/v1/datasources/local/failure-diagnostics.csv": {"get"},
		"/v1/datasources/local/metadata/repair":         {"post"},
		"/v1/datasources/local/thumbnails/repair":       {"post"},
		"/v1/datasources/local/embeddings/repair":       {"post"},
		"/v1/workers":                                    {"get", "put"},
		"/v1/system/resources":                           {"get"},
		"/v1/semantic-models":                            {"get"},
		"/v1/semantic-install-job":                       {"get"},
		"/v1/semantic-models/install":                    {"post"},
		"/v1/semantic-models/activate":                   {"post"},
		"/v1/semantic-models/uninstall":                  {"post"},
		"/v1/semantic-models/recommended/install":        {"post"},
		"/v1/semantic-runtime-packs/recommended/install": {"post"},
		"/v1/semantic-models/search/enable":              {"post"},
		"/v1/semantic-indexing/run":                      {"post"},
		"/v1/assets/search-preview":                      {"post"},
		"/v1/assets/{assetId}/preview":                   {"get", "head"},
		"/v1/nearby-links":                               {"get"},
		"/v1/nearby-links/approve":                       {"post"},
		"/v1/nearby-links/{linkId}/deny":                 {"post"},
		"/v1/update-check":                               {"get"},
		"/v1/uploads/roots":                              {"get"},
		"/v1/devices/{deviceId}":                         {"put", "delete"},
		"/v1/devices/{deviceId}/upload-policy":           {"get", "put"},
		"/v1/devices/{deviceId}/upload-reset":            {"post"},
	}
	for path, methods := range required {
		block, found := openAPIPathBlock(contract, path)
		if !found {
			t.Errorf("OpenAPI contract is missing path %s", path)
			continue
		}
		for _, method := range methods {
			if !strings.Contains(block, "\n    "+method+":") {
				t.Errorf("OpenAPI path %s is missing method %s", path, method)
			}
		}
	}
	datasources, found := openAPIPathBlock(contract, "/v1/datasources")
	if !found || !strings.Contains(datasources, `$ref: "#/components/responses/DatasourceListJSONResponse"`) {
		t.Error("GET /v1/datasources must use the dedicated list response")
	}
	listResponse, found := openAPIComponentBlock(contract, "DatasourceListJSONResponse")
	if !found || !strings.Contains(listResponse, "type: array") ||
		!strings.Contains(listResponse, `$ref: "#/components/schemas/DatasourceSummary"`) {
		t.Errorf("DatasourceListJSONResponse must be an array of DatasourceSummary: %q", listResponse)
	}
	createRequest, found := openAPIComponentBlock(contract, "DatasourceCreateRequest")
	if !found {
		t.Fatal("OpenAPI contract is missing DatasourceCreateRequest")
	}
	if strings.Contains(createRequest, "static_demo") || strings.Contains(createRequest, "required: [name, kind]") {
		t.Errorf("DatasourceCreateRequest exposes unsupported or artificially required fields: %q", createRequest)
	}
	if !strings.Contains(createRequest, "default: immich") {
		t.Errorf("DatasourceCreateRequest kind must document the runtime default: %q", createRequest)
	}
	binaryResponse, found := openAPIComponentBlock(contract, "AdminBinaryResponse")
	if !found || !strings.Contains(binaryResponse, `"image/*":`) {
		t.Errorf("AdminBinaryResponse must accept proxied image media types: %q", binaryResponse)
	}
	resetRequest, found := openAPIComponentBlock(contract, "DeviceUploadResetRequest")
	if !found || !strings.Contains(resetRequest, "required: [capturedAfter, capturedBefore]") ||
		!strings.Contains(resetRequest, "capturedAfter is earlier than capturedBefore") {
		t.Errorf("DeviceUploadResetRequest must require and order both capture bounds: %q", resetRequest)
	}
	resetOperation, found := openAPIPathBlock(contract, "/v1/devices/{deviceId}/upload-reset")
	if !found || !strings.Contains(resetOperation, "required half-open capture-date range") ||
		strings.Contains(resetOperation, "optional capture-date range") {
		t.Errorf("upload-reset operation must document its required half-open range: %q", resetOperation)
	}
	rootAcceptance, found := openAPIComponentBlock(contract, "LocalMediaRootAcceptanceRequest")
	if !found || !strings.Contains(rootAcceptance, "required: [sourceKey, rootKey, observedIdentity]") || !strings.Contains(rootAcceptance, "additionalProperties: false") {
		t.Errorf("LocalMediaRootAcceptanceRequest must bind the exact observed candidate: %q", rootAcceptance)
	}
	rootAcceptanceResponse, found := openAPIComponentBlock(contract, "LocalMediaRootAcceptanceResponse")
	if !found || !strings.Contains(rootAcceptanceResponse, "required: [acceptance, phase0, scanStatus]") ||
		!strings.Contains(rootAcceptanceResponse, "enum: [completed, failed, blocked]") ||
		!strings.Contains(rootAcceptanceResponse, "scanError:") {
		t.Errorf("LocalMediaRootAcceptanceResponse must separate committed acceptance from the scan outcome: %q", rootAcceptanceResponse)
	}
	rootAcceptanceOperation, found := openAPIPathBlock(contract, "/v1/datasources/local/root/accept")
	if !found || !strings.Contains(rootAcceptanceOperation, "#/components/schemas/LocalMediaRootAcceptanceResponse") ||
		!strings.Contains(rootAcceptanceOperation, "acceptance was committed") {
		t.Errorf("local root acceptance operation must return its committed transition and scan outcome: %q", rootAcceptanceOperation)
	}
}

func TestAgentMediaOpenAPIIsExportedWithAdminContract(t *testing.T) {
	t.Parallel()

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	for _, relativePath := range []string{
		"../.././packages/contracts/openapi/agent-media.yaml",
		"./packages/contracts/openapi/agent-media.yaml",
	} {
		candidate := filepath.Clean(filepath.Join(filepath.Dir(currentFile), relativePath))
		content, err := os.ReadFile(candidate)
		if err == nil {
			if !strings.Contains(string(content), "openapi:") || !strings.Contains(string(content), "/v1/assets") {
				t.Fatalf("Agent Media OpenAPI contract %s is incomplete", candidate)
			}
			return
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read Agent Media OpenAPI contract %s: %v", candidate, err)
		}
	}
	t.Fatal("locate Agent Media OpenAPI contract")
}

func openAPIPathBlock(contract string, path string) (string, bool) {
	marker := "\n  " + path + ":"
	start := strings.Index(contract, marker)
	if start < 0 {
		return "", false
	}
	remainder := contract[start+len(marker):]
	end := strings.Index(remainder, "\n  /")
	components := strings.Index(remainder, "\ncomponents:")
	if end < 0 || (components >= 0 && components < end) {
		end = components
	}
	if end < 0 {
		end = len(remainder)
	}
	return remainder[:end], true
}

func openAPIComponentBlock(contract string, name string) (string, bool) {
	marker := name + ":"
	start := strings.Index(contract, marker)
	if start < 0 {
		return "", false
	}
	remainder := contract[start+len(marker):]
	end := len(remainder)
	searchFrom := 0
	for {
		next := strings.Index(remainder[searchFrom:], "\n    ")
		if next < 0 {
			break
		}
		next += searchFrom
		lineStart := next + len("\n    ")
		if lineStart < len(remainder) && remainder[lineStart] != ' ' {
			end = next
			break
		}
		searchFrom = lineStart
	}
	return remainder[:end], true
}
