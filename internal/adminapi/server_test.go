package adminapi

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
	runtimestate "github.com/rsahara/timich-agent/internal/runtime"
	"github.com/rsahara/timich-agent/internal/store"
)

type agentBaseURLChoicePayload struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

func TestWriteDatasourceIndexingErrorUsesTaskStatusMessage(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writeDatasourceIndexingError(recorder, context.DeadlineExceeded)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "Datasource task status is still loading.") {
		t.Fatalf("body does not mention datasource task status: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "local datasource scan") {
		t.Fatalf("body still uses local scan wording: %s", recorder.Body.String())
	}
}

func TestWriteAdminCatalogErrorUsesTimeoutMessage(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	writeAdminCatalogError(recorder, context.DeadlineExceeded)

	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusGatewayTimeout, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "catalog request took too long") {
		t.Fatalf("body does not mention catalog timeout: %s", recorder.Body.String())
	}
}

func TestMuxDiagnosticsRoutes(t *testing.T) {
	t.Parallel()

	handler := NewMux(newTestRuntime(t, 5))
	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantKey    string
		wantValue  string
	}{
		{name: "health", path: "/healthz", wantStatus: http.StatusOK, wantKey: "status", wantValue: "ok"},
		{name: "ready", path: "/readyz", wantStatus: http.StatusOK, wantKey: "status", wantValue: "ready"},
		{name: "version", path: "/version", wantStatus: http.StatusOK, wantKey: "version", wantValue: "test-version"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if recorder.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", recorder.Header().Get("Content-Type"))
			}

			var payload map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
			}
			if payload[tc.wantKey] != tc.wantValue {
				t.Fatalf("%s = %v, want %q payload=%v", tc.wantKey, payload[tc.wantKey], tc.wantValue, payload)
			}
			if tc.path == "/version" && payload["releaseTag"] != "v0.4.0-rc.2" {
				t.Fatalf("releaseTag = %v, want published build identity", payload["releaseTag"])
			}
		})
	}
}

func TestIndexServesLoginWithoutCookie(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/html", got)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("Admin token")) {
		t.Fatalf("login page body = %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`value="timich-agent-admin"`)) {
		t.Fatalf("login page is missing password-manager username: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`autocomplete="username"`)) {
		t.Fatalf("login page is missing username autocomplete hint: %s", recorder.Body.String())
	}
}

func TestIndexServesSetupWhenAdminTokenMissing(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntimeWithoutAdminToken(t)).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("Create admin token")) {
		t.Fatalf("setup page body = %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("Admin UI sign-in, Admin API bearer auth, and CLI admin commands")) {
		t.Fatalf("setup page is missing admin token usage guidance: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("Save this as the password for timich-agent-admin")) {
		t.Fatalf("setup page is missing password-manager save guidance: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`value="timich-agent-admin"`)) {
		t.Fatalf("setup page is missing password-manager username: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`pattern="[A-Za-z0-9]{16,128}"`)) {
		t.Fatalf("setup page is missing cookie-safe token pattern: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`passwordrules="minlength: 16; maxlength: 128; allowed: upper, lower, digit;"`)) {
		t.Fatalf("setup page is missing password generator rules: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`id="generateAdminToken"`)) {
		t.Fatalf("setup page is missing token generator control: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`id="toggleAdminToken"`)) {
		t.Fatalf("setup page is missing token visibility control: %s", recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`stored in the local agent state file`)) {
		t.Fatalf("setup page is missing token storage note: %s", recorder.Body.String())
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte(`id="copyAdminToken"`)) {
		t.Fatalf("setup page should not include token copy control: %s", recorder.Body.String())
	}
}

func TestIndexServesDashboardWithCopyPairingControl(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.Bytes()
	if !bytes.Contains(body, []byte("Pair New Device")) {
		t.Fatalf("dashboard body is missing explicit device pairing section: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("data-datasource-task-action=\"go-to-search\"")) ||
		!bytes.Contains(body, []byte("Go to Search tab")) ||
		!bytes.Contains(body, []byte("setupRequired === 'search_model'")) ||
		!bytes.Contains(body, []byte("return 'not enabled';")) ||
		bytes.Contains(body, []byte("status-warn\">not enabled")) {
		t.Fatalf("dashboard body is missing neutral semantic-model task guidance: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("datasource-task-table-wrap")) ||
		!bytes.Contains(body, []byte("width: 104px")) ||
		!bytes.Contains(body, []byte("width: 330px")) ||
		!bytes.Contains(body, []byte("join(' · ')")) {
		t.Fatalf("dashboard body is missing fixed single-line datasource task columns: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("data-datasource-task-note-trigger")) ||
		!bytes.Contains(body, []byte("Show task note")) ||
		!bytes.Contains(body, []byte(">?</button>")) ||
		!bytes.Contains(body, []byte("role=\"tooltip\"")) ||
		!bytes.Contains(body, []byte("pendingDatasourceTaskRender")) ||
		!bytes.Contains(body, []byte("datasource-task-label")) ||
		!bytes.Contains(body, []byte("toggleDatasourceTaskNote")) {
		t.Fatalf("dashboard body is missing on-demand datasource task notes: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Create device pairing code")) {
		t.Fatalf("dashboard body is missing pairing action: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Approve Nearby Link")) {
		t.Fatalf("dashboard body is missing Nearby Link approval action: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Manual pairing code")) {
		t.Fatalf("dashboard body is missing separated manual pairing method: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("pairing-methods")) {
		t.Fatalf("dashboard body is missing pairing method layout: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("nearbyLinkCode")) {
		t.Fatalf("dashboard body is missing Nearby Link code input: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("/v1/nearby-links/approve")) {
		t.Fatalf("dashboard body is missing Nearby Link approval API call: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("manual one-time code for fallback pairing")) {
		t.Fatalf("dashboard body is missing pairing explanation: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("copyPairingCode")) {
		t.Fatalf("dashboard body is missing copy pairing control: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("pairingQRCode")) {
		t.Fatalf("dashboard body is missing pairing QR image: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("copyPairingLink")) {
		t.Fatalf("dashboard body is missing copy pairing link control: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("data-tab-link=\"overview\"")) ||
		!bytes.Contains(body, []byte("data-tab-link=\"datasources\"")) ||
		!bytes.Contains(body, []byte("data-tab-link=\"tasks\"")) ||
		!bytes.Contains(body, []byte("data-tab-link=\"search\"")) ||
		!bytes.Contains(body, []byte("data-tab-link=\"devices\"")) ||
		!bytes.Contains(body, []byte("data-tab-link=\"system\"")) ||
		!bytes.Contains(body, []byte("data-tab-panel=\"search\"")) ||
		!bytes.Contains(body, []byte("setActiveTab")) {
		t.Fatalf("dashboard body is missing admin tab navigation: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("return normalizeTab(location.hash) || 'overview';")) ||
		bytes.Contains(body, []byte("timichAgentAdminTab")) {
		t.Fatalf("dashboard body should default to overview instead of restoring the last tab: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Agent Update")) {
		t.Fatalf("dashboard body is missing update section: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("/v1/update-check")) {
		t.Fatalf("dashboard body is missing update-check API call: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Background Workers")) ||
		!bytes.Contains(body, []byte("workerSettingsForm")) ||
		!bytes.Contains(body, []byte("heavyTaskWorkersMode")) ||
		!bytes.Contains(body, []byte("heavyTaskWorkersCustom")) ||
		!bytes.Contains(body, []byte("/v1/workers")) {
		t.Fatalf("dashboard body is missing worker settings controls: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Max workers")) ||
		!bytes.Contains(body, []byte("<div class=\"section-note worker-note\">")) ||
		!bytes.Contains(body, []byte("Limits concurrent metadata, thumbnail, video preview, content verification, semantic embedding, and search-index publishing jobs")) ||
		!bytes.Contains(body, []byte("Content verification, semantic embedding, and publishing use at most 1 worker")) ||
		!bytes.Contains(body, []byte("Media discovery and status checks run outside this limit")) ||
		!bytes.Contains(body, []byte("After pausing, in-flight work continues until its current batch completes")) {
		t.Fatalf("dashboard body is missing simplified background worker copy: %s", recorder.Body.String())
	}
	if bytes.Contains(body, []byte("Active workers")) ||
		bytes.Contains(body, []byte("Custom workers")) ||
		bytes.Contains(body, []byte("Auto (1)")) {
		t.Fatalf("dashboard body contains stale worker labels: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Math.floor(hostCPUCount / 2)")) ||
		!bytes.Contains(body, []byte("Auto (' + autoLabel + ')")) ||
		!bytes.Contains(body, []byte("Paused (0 workers)")) ||
		!bytes.Contains(body, []byte("Number of workers")) ||
		!bytes.Contains(body, []byte("String(workers) + ' worker'")) {
		t.Fatalf("dashboard body is missing updated worker labels: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("System Resources")) ||
		!bytes.Contains(body, []byte("systemResourcesStatus")) ||
		!bytes.Contains(body, []byte("/v1/system/resources")) ||
		!bytes.Contains(body, []byte("Current host load and storage headroom")) ||
		!bytes.Contains(body, []byte("Memory usage")) {
		t.Fatalf("dashboard body is missing system resource status controls: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("http://immich_server:2283")) {
		t.Fatalf("dashboard body is missing Immich Docker datasource placeholder: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Datasources")) || !bytes.Contains(body, []byte("datasourceList")) || !bytes.Contains(body, []byte("datasourceKind")) {
		t.Fatalf("dashboard body is missing datasource list/add controls: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte(`<option value="immich">Immich (Passthrough)</option>`)) ||
		!bytes.Contains(body, []byte(`<option value="immich_indexed">Immich (Indexed)</option>`)) ||
		!bytes.Contains(body, []byte("Immich passthrough must remain the only datasource")) {
		t.Fatalf("dashboard body is missing Immich datasource mode and topology guidance: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("[hidden] { display: none !important; }")) {
		t.Fatalf("dashboard body is missing explicit hidden-field styling: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("details.disclosure { margin-top: 12px;")) {
		t.Fatalf("dashboard body is missing add-datasource disclosure spacing: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("datasourceAddPanel.open = items.length === 0")) ||
		!bytes.Contains(body, []byte("datasourceAddPanelTouched")) {
		t.Fatalf("dashboard body is missing empty-datasource add form auto-expand logic: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte(`data-datasource-kind-field="local_filesystem" hidden>Local media root`)) ||
		!bytes.Contains(body, []byte(`id="datasourceRootHint"`)) ||
		!bytes.Contains(body, []byte("No local media roots are configured for this Agent")) {
		t.Fatalf("dashboard body is missing local media root availability guidance: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("/v1/datasources")) {
		t.Fatalf("dashboard body is missing datasources API call: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Immich fallback")) ||
		!bytes.Contains(body, []byte("data-local-immich-fallback")) ||
		!bytes.Contains(body, []byte("/v1/datasources/local/immich-fallback")) {
		t.Fatalf("dashboard body is missing local Immich fallback toggle: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Root identity changed")) ||
		!bytes.Contains(body, []byte("data-accept-local-root")) ||
		!bytes.Contains(body, []byte("/v1/datasources/local/root/accept")) ||
		!bytes.Contains(body, []byte("Media that exists only in the previous root may become missing")) ||
		!bytes.Contains(body, []byte("result?.scanStatus === 'completed'")) ||
		!bytes.Contains(body, []byte("Current root accepted, but reconciliation did not complete")) {
		t.Fatalf("dashboard body is missing explicit changed-root acceptance controls: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("/v1/datasource/primary/check")) {
		t.Fatalf("dashboard body is missing datasource check API call: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("function shouldCheckDatasourceReachability(datasource)")) || !bytes.Contains(body, []byte("shouldCheckDatasourceReachability(primaryDatasource)")) {
		t.Fatalf("dashboard body is missing kind-aware datasource reachability guard: %s", recorder.Body.String())
	}
	if bytes.Contains(body, []byte("Datasource Indexing")) {
		t.Fatalf("dashboard body still has the old datasource indexing section: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Run reconciliation now")) ||
		!bytes.Contains(body, []byte("Quick discovery finds likely filesystem changes with low NAS load")) ||
		!bytes.Contains(body, []byte(`data-datasource-task-action="media-discovery"`)) {
		t.Fatalf("dashboard body is missing explicit reconciliation controls: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Datasource Tasks")) ||
		!bytes.Contains(body, []byte("datasourceTaskStatus")) ||
		!bytes.Contains(body, []byte("<th>Task</th><th class=\"datasource-task-activity\">Activity</th><th class=\"datasource-task-status\">Status</th><th>Action</th>")) ||
		!bytes.Contains(body, []byte(`id="initialDatasourceIndexingStatus"`)) ||
		!bytes.Contains(body, []byte("hydrateDatasourceIndexingStatusFromCache")) ||
		!bytes.Contains(body, []byte("data-datasource-task-action")) ||
		!bytes.Contains(body, []byte("'running (' + formatCount(activeTasks) + ')'")) ||
		!bytes.Contains(body, []byte("status-running")) ||
		!bytes.Contains(body, []byte("'waiting ' + formatCount(target) + ' queued items'")) ||
		!bytes.Contains(body, []byte("activeTasks")) ||
		!bytes.Contains(body, []byte("queuedTasks")) ||
		!bytes.Contains(body, []byte("settlingTasks")) ||
		!bytes.Contains(body, []byte("settling: ")) ||
		!bytes.Contains(body, []byte("remain settling before metadata processing")) ||
		!bytes.Contains(body, []byte("quick discovery: ")) ||
		!bytes.Contains(body, []byte("reconciliation: ")) ||
		!bytes.Contains(body, []byte("content_verification")) ||
		!bytes.Contains(body, []byte("result: skipped (")) ||
		!bytes.Contains(body, []byte("root_identity_changed")) ||
		!bytes.Contains(body, []byte("root changed")) ||
		!bytes.Contains(body, []byte("not applicable (no local datasource)")) ||
		!bytes.Contains(body, []byte("content verification is disabled")) ||
		!bytes.Contains(body, []byte("task?.phase !== 'content_verification'")) ||
		!bytes.Contains(body, []byte("datasourceContentVerificationEventAt(task)")) ||
		!bytes.Contains(body, []byte("datasourceContentVerificationEventAt(previous)")) ||
		!bytes.Contains(body, []byte("latestDatasourceTaskTimestamp(task?.lastCompletedAt, task?.lastRunStartedAt)")) ||
		!bytes.Contains(body, []byte("lastQuickScanAt")) ||
		!bytes.Contains(body, []byte("lastReconciliationAt")) {
		t.Fatalf("dashboard body is missing datasource task status controls: %s", recorder.Body.String())
	}
	if bytes.Contains(body, []byte("task?.lastCompletedAt || task?.lastRunStartedAt")) {
		t.Fatalf("dashboard body still prefers an older content verification completion timestamp: %s", recorder.Body.String())
	}
	assertSnippetOrder(t, string(body), "<h2>Datasource Tasks</h2>", "<h2>System Resources</h2>")
	assertSnippetOrder(t, string(body), "<h2>System Resources</h2>", "<h2>Background Workers</h2>")
	assertSnippetOrder(t, string(body), "if (activeTasks > 0)", "if (task?.waitingReason === 'paused'")
	if bytes.Contains(body, []byte(`'<td>' + escapeHTML(task.label || task.phase || '') + '<div class="muted">'`)) {
		t.Fatalf("dashboard body still renders internal datasource task phase subtitles: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Datasource Health")) ||
		!bytes.Contains(body, []byte(`id="datasourceStatus"`)) ||
		!bytes.Contains(body, []byte("<th>Datasource</th><th>Found medias</th><th>Browsable medias</th><th>Searchable medias</th><th>Issues</th>")) {
		t.Fatalf("dashboard body is missing datasource health table: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("function isImmichPassthroughDatasource(datasource)")) ||
		!bytes.Contains(body, []byte("status: isImmichPassthroughDatasource(datasource) ? 'immich-managed' : 'updating'")) ||
		!bytes.Contains(body, []byte("if (metric.status === 'immich-managed') return 'Immich-managed';")) {
		t.Fatalf("dashboard body is missing Immich-managed passthrough coverage formatting: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Search Coverage")) ||
		!bytes.Contains(body, []byte(`id="searchCoverageStatus"`)) ||
		!bytes.Contains(body, []byte("Search results are partial")) ||
		!bytes.Contains(body, []byte("Media analyzed")) ||
		!bytes.Contains(body, []byte("Search index")) {
		t.Fatalf("dashboard body is missing search coverage summary: %s", recorder.Body.String())
	}
	searchCoverageLineStart := bytes.Index(body, []byte("function searchCoverageLine"))
	searchCoverageNoticeStart := bytes.Index(body, []byte("function searchCoverageNoticeHTML"))
	if searchCoverageLineStart < 0 || searchCoverageNoticeStart <= searchCoverageLineStart {
		t.Fatalf("dashboard body is missing the search coverage formatter: %s", recorder.Body.String())
	}
	if bytes.Contains(body[searchCoverageLineStart:searchCoverageNoticeStart], []byte("queued")) {
		t.Fatalf("search coverage should show completed progress without queued work counts: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Catalog Integrity")) ||
		!bytes.Contains(body, []byte(`id="catalogDedupStatus"`)) ||
		!bytes.Contains(body, []byte("/v1/catalog/dedup/status")) ||
		!bytes.Contains(body, []byte("/v1/catalog/dedup/repair")) ||
		!bytes.Contains(body, []byte("Duplicate source rows")) ||
		!bytes.Contains(body, []byte("Check catalog integrity")) {
		t.Fatalf("dashboard body is missing catalog integrity status and repair controls: %s", recorder.Body.String())
	}
	assertSnippetOrder(t, string(body), "<h2>Remote Browsing Readiness</h2>", "<h2>Catalog Integrity</h2>")
	assertSnippetOrder(t, string(body), "<h2>Catalog Integrity</h2>", "<h2>Agent Controls</h2>")
	if bytes.Contains(body, []byte("<th>Adapter</th>")) ||
		bytes.Contains(body, []byte("<th>Health</th>")) ||
		bytes.Contains(body, []byte("<th>Catalog</th>")) ||
		bytes.Contains(body, []byte("<th>Work</th>")) ||
		bytes.Contains(body, []byte("<th>Last run</th>")) {
		t.Fatalf("dashboard body still exposes low-value datasource status columns: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("No active search model")) ||
		!bytes.Contains(body, []byte("Search coverage unavailable")) ||
		!bytes.Contains(body, []byte("Datasource coverage unavailable")) ||
		!bytes.Contains(body, []byte("Loading datasource coverage")) ||
		!bytes.Contains(body, []byte("Loading search coverage")) ||
		!bytes.Contains(body, []byte("Catalog integrity not checked")) ||
		!bytes.Contains(body, []byte("Loading semantic models")) {
		t.Fatalf("dashboard body is missing clarified datasource health/search copy: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Download discovery CSV")) ||
		!bytes.Contains(body, []byte("/v1/datasources/local/phase0-diagnostics.csv")) ||
		!bytes.Contains(body, []byte("Download failures CSV")) ||
		!bytes.Contains(body, []byte("task-action-link")) ||
		!bytes.Contains(body, []byte("/v1/datasources/local/failure-diagnostics.csv")) {
		t.Fatalf("dashboard body is missing local datasource diagnostic CSV link: %s", recorder.Body.String())
	}
	if healthIndex := bytes.Index(body, []byte("Datasource Health")); healthIndex < 0 || bytes.Index(body, []byte(`data-tab-panel="datasources"`)) > healthIndex {
		t.Fatalf("dashboard body should render datasource health in the Datasources tab: %s", recorder.Body.String())
	}
	if coverageIndex := bytes.Index(body, []byte("Search Coverage")); coverageIndex < 0 || bytes.Index(body, []byte(`data-tab-panel="search"`)) > coverageIndex {
		t.Fatalf("dashboard body should render search coverage in the Search tab: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("/v1/datasources/indexing")) || !bytes.Contains(body, []byte("/v1/datasources/indexing/run")) {
		t.Fatalf("dashboard body is missing datasource indexing API calls: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte(`data-datasource-task-action="requeue-metadata"`)) ||
		!bytes.Contains(body, []byte("moves failed metadata jobs back to the queue")) ||
		!bytes.Contains(body, []byte("Processing starts after settling when a worker is available")) ||
		!bytes.Contains(body, []byte("/v1/datasources/local/metadata/repair")) {
		t.Fatalf("dashboard body is missing failed metadata requeue control and explanation: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Requeue failed")) ||
		!bytes.Contains(body, []byte("moves failed thumbnails back to the queue")) ||
		!bytes.Contains(body, []byte("/v1/datasources/local/thumbnails/repair")) {
		t.Fatalf("dashboard body is missing failed thumbnail requeue control and explanation: %s", recorder.Body.String())
	}
	if bytes.Contains(body, []byte("Running media discovery...")) ||
		bytes.Contains(body, []byte("Thumbnail retry requested. Background workers will pick it up.")) ||
		bytes.Contains(body, []byte("Metadata retry requested. Background workers will pick it up.")) {
		t.Fatalf("dashboard body still renders transient datasource task action copy: %s", recorder.Body.String())
	}
	if bytes.Contains(body, []byte("generated ' + String")) ||
		bytes.Contains(body, []byte("registered ' + String")) {
		t.Fatalf("dashboard body still reports synchronous retry processing results: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("if (completed > 0)")) ||
		bytes.Contains(body, []byte("completed > 0 || remaining > 0")) {
		t.Fatalf("dashboard body should omit done progress until completed or total counts are known: %s", recorder.Body.String())
	}
	if bytes.Contains(body, []byte("repairLocalEmbeddings")) {
		t.Fatalf("dashboard body still exposes embedding repair as a manual datasource task action: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Semantic Models")) || !bytes.Contains(body, []byte("/v1/semantic-models")) {
		t.Fatalf("dashboard body is missing semantic model status: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("/v1/semantic-install-job")) || !bytes.Contains(body, []byte("startSemanticInstallJobPolling")) {
		t.Fatalf("dashboard body is missing semantic install job polling: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("model-list")) || !bytes.Contains(body, []byte("data-semantic-action=\"install-model\"")) || !bytes.Contains(body, []byte("/v1/semantic-models/install")) {
		t.Fatalf("dashboard body is missing semantic model list install controls: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("data-semantic-action=\"install-runtime\"")) || !bytes.Contains(body, []byte("/v1/semantic-runtime-packs/recommended/install")) {
		t.Fatalf("dashboard body is missing semantic runtime pack install control: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("data-semantic-action=\"activate-model\"")) || !bytes.Contains(body, []byte("/v1/semantic-models/activate")) {
		t.Fatalf("dashboard body is missing semantic model activation control: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("data-semantic-action=\"uninstall-model\"")) || !bytes.Contains(body, []byte("/v1/semantic-models/uninstall")) {
		t.Fatalf("dashboard body is missing semantic model uninstall control: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("pack.installed && !semanticModelIsActive(model, status) && !semanticModelIsIndexing(model, status)")) {
		t.Fatalf("dashboard body allows semantic model uninstall during semantic indexing: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("['pending', 'backfilling', 'indexing'].includes")) ||
		bytes.Contains(body, []byte("['pending', 'backfilling', 'indexing', 'ready'].includes")) {
		t.Fatalf("dashboard body should allow uninstall for ready unactivated models: %s", recorder.Body.String())
	}
	if bytes.Contains(body, []byte("enableSemanticSearch")) || bytes.Contains(body, []byte("indexingSemanticModel")) || bytes.Contains(body, []byte("Activate candidate")) {
		t.Fatalf("dashboard body still exposes old semantic search/candidate controls: %s", recorder.Body.String())
	}
	if bytes.Contains(body, []byte("Candidate indexing")) || bytes.Contains(body, []byte("Candidate runtime")) || bytes.Contains(body, []byte("Semantic indexing workers")) || bytes.Contains(body, []byte("Candidate worker count")) {
		t.Fatalf("dashboard body still exposes detailed worker counts: %s", recorder.Body.String())
	}
	if bytes.Contains(body, []byte("Candidate model")) || bytes.Contains(body, []byte("candidate model")) {
		t.Fatalf("dashboard body still exposes candidate model wording: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Checking datasource reachability from this Agent")) {
		t.Fatalf("dashboard body is missing datasource reachability loading state: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Datasource is configured and reachable from this Agent")) {
		t.Fatalf("dashboard body is missing datasource reachable setup summary: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Device Uploads")) {
		t.Fatalf("dashboard body is missing device upload section: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("/v1/uploads/roots")) {
		t.Fatalf("dashboard body is missing upload roots API call: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("upload-policy")) {
		t.Fatalf("dashboard body is missing upload policy API call: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("Device List")) || !bytes.Contains(body, []byte("Device name (display)")) || !bytes.Contains(body, []byte("device-summary")) {
		t.Fatalf("dashboard body is missing device list summary layout: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("data-device-rename")) || !bytes.Contains(body, []byte("Device name settings")) || !bytes.Contains(body, []byte("Save device name")) {
		t.Fatalf("dashboard body is missing device name settings control: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("device-subsection device-upload-section")) || !bytes.Contains(body, []byte("Upload settings")) {
		t.Fatalf("dashboard body is missing upload settings subsection: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte(`input[type="checkbox"]`)) || !bytes.Contains(body, []byte("checkbox-label")) {
		t.Fatalf("dashboard body is missing compact checkbox styling: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("effectiveDeviceUploadStatus")) {
		t.Fatalf("dashboard body is missing effective upload status helper: %s", recorder.Body.String())
	}
	if !bytes.Contains(body, []byte("tempCleanupErrors")) {
		t.Fatalf("dashboard body is missing temp cleanup warning handling: %s", recorder.Body.String())
	}
}

func TestDashboardSeparatesDatasourceTaskFailureUnits(t *testing.T) {
	t.Parallel()

	failureText := dashboardSnippet(t, "function datasourceTaskFailureText", "function searchCoverageModelLabel")
	for _, marker := range []string{
		"publish jobs failed: ",
		"items failed: ",
		"unit === 'publish_jobs'",
		"unit === 'items'",
	} {
		if !strings.Contains(failureText, marker) {
			t.Fatalf("failure formatter is missing %q:\n%s", marker, failureText)
		}
	}

	searchCoverage := dashboardSnippet(t, "function searchCoverageLine", "function searchCoverageNoticeHTML")
	if !strings.Contains(searchCoverage, "datasourceTaskFailureText(task, formatCount(failed))") ||
		strings.Contains(searchCoverage, "parts.push('failed: '") {
		t.Fatalf("search coverage still renders an unscoped failure count:\n%s", searchCoverage)
	}

	taskStatus := dashboardSnippet(t, "function datasourceTaskStatusHTML", "function datasourceTaskNoteHTML")
	if !strings.Contains(taskStatus, "datasourceTaskFailureText(task, 'unknown')") ||
		!strings.Contains(taskStatus, "datasourceTaskFailureText(task, formatCount(task.failedTasks))") ||
		!strings.Contains(taskStatus, "'failed files: ' + formatCount(task.lastFailedFiles)") {
		t.Fatalf("datasource task status is missing scoped failure labels:\n%s", taskStatus)
	}

	failureLink := dashboardSnippet(t, "function datasourceTaskFailureLinkHTML", "function datasourceTaskStatusClass")
	if !strings.Contains(failureLink, "task?.phase === 'metadata' || task?.phase === 'thumbnails'") ||
		strings.Contains(failureLink, "search_index") ||
		strings.Contains(failureLink, "embeddings") {
		t.Fatalf("local failure CSV link is not limited to local item-only task phases:\n%s", failureLink)
	}

	taskNotes := dashboardSnippet(t, "const datasourceTaskNotes", "function datasourceTaskNote")
	if !strings.Contains(taskNotes, "Publishing can take several hours for a large library.") ||
		!strings.Contains(taskNotes, "An existing published index remains searchable while publishing runs.") ||
		!strings.Contains(taskNotes, "Failed publish jobs are retried automatically on the next eligible run.") {
		t.Fatalf("search index task note does not explain automatic publish retry:\n%s", taskNotes)
	}
}

func TestDashboardDatasourceSaveFailureRestoresStatusBeforeError(t *testing.T) {
	t.Parallel()

	restoreSnippet := dashboardSnippet(t,
		"async function restoreDatasourceSaveFailureState(attemptedDatasource)",
		"async function loadDevices()",
	)
	assertSnippetOrder(t, restoreSnippet,
		"await loadStatus();",
		"datasourceName.value = attemptedDatasource.name;",
		"datasourceKind.value = attemptedDatasource.kind || 'immich';",
		"datasourceURL.value = attemptedDatasource.url;",
		"datasourceAccessToken.value = attemptedDatasource.accessToken;",
		"datasourceRootKey.value = attemptedDatasource.rootKey || '';",
		"updateDatasourceFormMode();",
	)

	submitFailureSnippet := dashboardSnippet(t,
		"datasourceForm.addEventListener('submit'",
		"async function checkDatasource",
	)
	assertSnippetOrder(t, submitFailureSnippet,
		"const attemptedDatasource = {",
		"await api('/v1/datasources', {",
		"} catch (error) {",
		"await restoreDatasourceSaveFailureState(attemptedDatasource);",
		"datasourceMessage.textContent = error.message;",
		"datasourceMessage.className = 'status-failed';",
	)
}

func TestDashboardWorkerSaveRefreshesSetupTasks(t *testing.T) {
	t.Parallel()

	apiSnippet := dashboardSnippet(t,
		"async function api(path, options = {})",
		"function rows(items)",
	)
	if !strings.Contains(apiSnippet, "fetch(path, { credentials: 'same-origin', cache: 'no-store', ...options })") {
		t.Fatalf("admin API requests must bypass the browser cache:\n%s", apiSnippet)
	}

	submitSnippet := dashboardSnippet(t,
		"workerSettingsForm.addEventListener('submit'",
		"datasourceTaskStatus.addEventListener('click'",
	)
	assertSnippetOrder(t, submitSnippet,
		"await api('/v1/workers', {",
		"renderWorkerRuntime(status);",
		"await Promise.all([",
		"loadStatus(),",
		"loadSemanticModels({ forceRefresh: true }),",
		"workerRuntimeMessage.textContent = 'Saved worker settings';",
	)
}

func TestDashboardContentVerificationNotApplicableDiscardsCachedResult(t *testing.T) {
	t.Parallel()

	mergeSnippet := dashboardSnippet(t,
		"function mergeDatasourceTaskRow(task, previous)",
		"function latestDatasourceTaskTimestamp(first, second)",
	)
	assertSnippetOrder(t, mergeSnippet,
		"if (task?.phase === 'content_verification' && task?.status === 'not_applicable') return task;",
		"if (!previous) return task;",
		"const previousContentResultAt = Date.parse(datasourceContentVerificationEventAt(previous));",
	)

	statusSnippet := dashboardSnippet(t,
		"function datasourceTaskStatusHTML(task)",
		"function datasourceTaskNoteHTML(task)",
	)
	assertSnippetOrder(t, statusSnippet,
		"if (task?.phase === 'content_verification' && rawStatus === 'not_applicable')",
		"const lastAt = datasourceContentVerificationEventAt(task);",
		"if (task?.lastRunStatus === 'skipped')",
	)
}

func dashboardSnippet(t *testing.T, startMarker, endMarker string) string {
	t.Helper()

	start := strings.Index(dashboardHTML, startMarker)
	if start < 0 {
		t.Fatalf("dashboard HTML is missing start marker %q", startMarker)
	}
	end := strings.Index(dashboardHTML[start:], endMarker)
	if end < 0 {
		t.Fatalf("dashboard HTML is missing end marker %q after %q", endMarker, startMarker)
	}
	return dashboardHTML[start : start+end]
}

func assertSnippetOrder(t *testing.T, snippet string, orderedMarkers ...string) {
	t.Helper()

	offset := 0
	for _, marker := range orderedMarkers {
		index := strings.Index(snippet[offset:], marker)
		if index < 0 {
			t.Fatalf("snippet is missing marker %q after offset %d:\n%s", marker, offset, snippet)
		}
		offset += index + len(marker)
	}
}

func TestAdminRoutesRequireAuthentication(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/status"},
		{method: http.MethodGet, path: "/config"},
		{method: http.MethodGet, path: "/v1/datasource/primary"},
		{method: http.MethodPut, path: "/v1/datasource/primary"},
		{method: http.MethodGet, path: "/v1/datasources"},
		{method: http.MethodPost, path: "/v1/datasources"},
		{method: http.MethodPut, path: "/v1/datasources/local/immich-fallback"},
		{method: http.MethodGet, path: "/v1/datasources/indexing"},
		{method: http.MethodPost, path: "/v1/datasources/indexing/run"},
		{method: http.MethodGet, path: "/v1/catalog/dedup/status"},
		{method: http.MethodPost, path: "/v1/catalog/dedup/repair"},
		{method: http.MethodPost, path: "/v1/datasource/primary/check"},
		{method: http.MethodGet, path: "/v1/datasources/local/scan"},
		{method: http.MethodPost, path: "/v1/datasources/local/scan"},
		{method: http.MethodPost, path: "/v1/datasources/local/root/accept"},
		{method: http.MethodGet, path: "/v1/datasources/local/phase0-diagnostics.csv"},
		{method: http.MethodGet, path: "/v1/datasources/local/failure-diagnostics.csv"},
		{method: http.MethodPost, path: "/v1/datasources/local/metadata/repair"},
		{method: http.MethodPost, path: "/v1/datasources/local/thumbnails/repair"},
		{method: http.MethodPost, path: "/v1/datasources/local/embeddings/repair"},
		{method: http.MethodGet, path: "/v1/workers"},
		{method: http.MethodPut, path: "/v1/workers"},
		{method: http.MethodGet, path: "/v1/system/resources"},
		{method: http.MethodGet, path: "/v1/semantic-models"},
		{method: http.MethodGet, path: "/v1/semantic-install-job"},
		{method: http.MethodPost, path: "/v1/semantic-models/install"},
		{method: http.MethodPost, path: "/v1/semantic-models/activate"},
		{method: http.MethodPost, path: "/v1/semantic-models/uninstall"},
		{method: http.MethodPost, path: "/v1/semantic-models/recommended/install"},
		{method: http.MethodPost, path: "/v1/semantic-runtime-packs/recommended/install"},
		{method: http.MethodPost, path: "/v1/semantic-models/search/enable"},
		{method: http.MethodPost, path: "/v1/semantic-indexing/run"},
		{method: http.MethodPost, path: "/v1/assets/search-preview"},
		{method: http.MethodGet, path: "/v1/assets/demo-0001/preview"},
		{method: http.MethodGet, path: "/v1/nearby-links"},
		{method: http.MethodPost, path: "/v1/nearby-links/approve"},
		{method: http.MethodPost, path: "/v1/nearby-links/link-1/deny"},
		{method: http.MethodPost, path: "/v1/pairing-sessions"},
		{method: http.MethodPost, path: "/v1/pairing-links"},
		{method: http.MethodPost, path: "/v1/compatibility-check"},
		{method: http.MethodGet, path: "/v1/update-check"},
		{method: http.MethodPost, path: "/v1/restart"},
		{method: http.MethodGet, path: "/v1/uploads/roots"},
		{method: http.MethodGet, path: "/v1/devices"},
		{method: http.MethodPut, path: "/v1/devices/device-1"},
		{method: http.MethodGet, path: "/v1/devices/device-1/upload-policy"},
		{method: http.MethodPut, path: "/v1/devices/device-1/upload-policy"},
		{method: http.MethodPost, path: "/v1/devices/device-1/upload-reset"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))

			assertErrorPayload(t, recorder, http.StatusUnauthorized, "unauthorized")
		})
	}
}

func TestAuthenticatedAdminRoutes(t *testing.T) {
	t.Parallel()

	handler := NewMux(newTestRuntime(t, 5))
	tests := []struct {
		name      string
		path      string
		wantKey   string
		wantValue string
	}{
		{name: "status", path: "/status", wantKey: "agentId", wantValue: "agent-test"},
		{name: "config", path: "/config", wantKey: "agentName", wantValue: "test-agent"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, authenticatedRequest(http.MethodGet, tc.path, nil))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
			}
			if payload[tc.wantKey] != tc.wantValue {
				t.Fatalf("%s = %v, want %q payload=%v", tc.wantKey, payload[tc.wantKey], tc.wantValue, payload)
			}
		})
	}
}

func TestWorkersGetAndUpdatePersistsConfig(t *testing.T) {
	t.Parallel()

	datasource := config.DatasourceConfig{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        "immich_indexed",
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
		Indexing:    &config.DatasourceIndexingConfig{},
	}
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.Indexing = config.SemanticIndexingConfig{
			Enabled:   true,
			Interval:  "45s",
			BatchSize: 7,
		}
		cfg.Datasources = []config.DatasourceConfig{datasource}
	})
	defer runtime.Close()
	fileConfig := config.Default()
	fileConfig.DataDir = runtime.ConfigResponse().DataDir
	fileConfig.SemanticRuntime.Indexing = config.SemanticIndexingConfig{
		Enabled:   true,
		Interval:  "45s",
		BatchSize: 7,
	}
	fileConfig.Datasources = []config.DatasourceConfig{datasource}
	if err := config.WriteFile(runtime.ConfigResponse().ConfigPath, fileConfig); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	handler := NewMux(runtime)

	updateRecorder := httptest.NewRecorder()
	handler.ServeHTTP(updateRecorder, authenticatedRequest(http.MethodPut, "/v1/workers", bytes.NewReader([]byte(`{"heavyTaskWorkers":3}`))))
	if updateRecorder.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 body=%s", updateRecorder.Code, updateRecorder.Body.String())
	}
	var payload struct {
		HeavyTaskWorkers           int  `json:"heavyTaskWorkers"`
		ConfiguredHeavyTaskWorkers *int `json:"configuredHeavyTaskWorkers"`
		AutoHeavyTaskWorkers       bool `json:"autoHeavyTaskWorkers"`
		PausedHeavyTaskWorkers     bool `json:"pausedHeavyTaskWorkers"`
		LocalDatasourceWorkers     int  `json:"localDatasourceWorkers"`
		LocalMetadataBatchSize     int  `json:"localMetadataBatchSize"`
		LocalThumbnailBatchSize    int  `json:"localThumbnailBatchSize"`
		SemanticIndexingWorkers    int  `json:"semanticIndexingWorkers"`
	}
	if err := json.Unmarshal(updateRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("update response is not JSON: %v body=%s", err, updateRecorder.Body.String())
	}
	if payload.HeavyTaskWorkers != 3 ||
		payload.ConfiguredHeavyTaskWorkers == nil ||
		*payload.ConfiguredHeavyTaskWorkers != 3 ||
		payload.AutoHeavyTaskWorkers ||
		payload.PausedHeavyTaskWorkers ||
		payload.LocalDatasourceWorkers != 0 ||
		payload.LocalMetadataBatchSize != 0 ||
		payload.LocalThumbnailBatchSize != 0 ||
		payload.SemanticIndexingWorkers != 1 {
		t.Fatalf("worker payload = %#v, want configured 3 with semantic max 1", payload)
	}

	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("semantic status = %d, want 200 body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var semanticPayload struct {
		IndexingWorker struct {
			WorkerCount int `json:"workerCount"`
		} `json:"indexingWorker"`
	}
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &semanticPayload); err != nil {
		t.Fatalf("semantic response is not JSON: %v body=%s", err, statusRecorder.Body.String())
	}
	if semanticPayload.IndexingWorker.WorkerCount != 1 {
		t.Fatalf("semantic worker count = %d, want semantic max 1", semanticPayload.IndexingWorker.WorkerCount)
	}

	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.WorkerRuntime.HeavyTaskWorkers == nil || *loaded.WorkerRuntime.HeavyTaskWorkers != 3 {
		t.Fatalf("persisted HeavyTaskWorkers = %v, want 3", loaded.WorkerRuntime.HeavyTaskWorkers)
	}
	if loaded.Datasources[0].AccessToken != "immich-api-key" {
		t.Fatalf("persisted datasource token = %q, want preserved token", loaded.Datasources[0].AccessToken)
	}
}

func TestWorkersAutoAndPausedModes(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, 5)
	handler := NewMux(runtime)

	autoRecorder := httptest.NewRecorder()
	handler.ServeHTTP(autoRecorder, authenticatedRequest(http.MethodPut, "/v1/workers", bytes.NewReader([]byte(`{"heavyTaskWorkers":null}`))))
	if autoRecorder.Code != http.StatusOK {
		t.Fatalf("auto status = %d, want 200 body=%s", autoRecorder.Code, autoRecorder.Body.String())
	}
	var autoPayload struct {
		HeavyTaskWorkers           int  `json:"heavyTaskWorkers"`
		ConfiguredHeavyTaskWorkers *int `json:"configuredHeavyTaskWorkers"`
		AutoHeavyTaskWorkers       bool `json:"autoHeavyTaskWorkers"`
		PausedHeavyTaskWorkers     bool `json:"pausedHeavyTaskWorkers"`
		HostCPUCount               int  `json:"hostCpuCount"`
	}
	if err := json.Unmarshal(autoRecorder.Body.Bytes(), &autoPayload); err != nil {
		t.Fatalf("auto response is not JSON: %v body=%s", err, autoRecorder.Body.String())
	}
	wantAutoWorkers := max(1, autoPayload.HostCPUCount/2)
	if !autoPayload.AutoHeavyTaskWorkers ||
		autoPayload.PausedHeavyTaskWorkers ||
		autoPayload.ConfiguredHeavyTaskWorkers != nil ||
		autoPayload.HostCPUCount <= 0 ||
		autoPayload.HeavyTaskWorkers != wantAutoWorkers {
		t.Fatalf("auto payload = %#v, want %d auto workers", autoPayload, wantAutoWorkers)
	}

	pausedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(pausedRecorder, authenticatedRequest(http.MethodPut, "/v1/workers", bytes.NewReader([]byte(`{"heavyTaskWorkers":0}`))))
	if pausedRecorder.Code != http.StatusOK {
		t.Fatalf("paused status = %d, want 200 body=%s", pausedRecorder.Code, pausedRecorder.Body.String())
	}
	var pausedPayload struct {
		HeavyTaskWorkers           int  `json:"heavyTaskWorkers"`
		ConfiguredHeavyTaskWorkers *int `json:"configuredHeavyTaskWorkers"`
		AutoHeavyTaskWorkers       bool `json:"autoHeavyTaskWorkers"`
		PausedHeavyTaskWorkers     bool `json:"pausedHeavyTaskWorkers"`
	}
	if err := json.Unmarshal(pausedRecorder.Body.Bytes(), &pausedPayload); err != nil {
		t.Fatalf("paused response is not JSON: %v body=%s", err, pausedRecorder.Body.String())
	}
	if pausedPayload.HeavyTaskWorkers != 0 ||
		pausedPayload.ConfiguredHeavyTaskWorkers == nil ||
		*pausedPayload.ConfiguredHeavyTaskWorkers != 0 ||
		pausedPayload.AutoHeavyTaskWorkers ||
		!pausedPayload.PausedHeavyTaskWorkers {
		t.Fatalf("paused payload = %#v, want explicit paused workers", pausedPayload)
	}
}

func TestAssetSearchPreviewReturnsAdminSearchPage(t *testing.T) {
	t.Parallel()

	handler := NewMux(newStaticDemoAdminTestRuntime(t))
	body := bytes.NewReader([]byte(`{"collection":{"kind":"timeline"},"page":{"index":0,"size":5}}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/assets/search-preview", body))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var page catalog.AssetSearchPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("response is not AssetSearchPage JSON: %v body=%s", err, recorder.Body.String())
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Filename != "demo.jpg" {
		t.Fatalf("page = %+v, want demo asset", page)
	}
	if page.Items[0].ID == "demo-0001" {
		t.Fatal("search preview returned an unsigned upstream asset id")
	}
}

func TestAssetPreviewProxiesPreviewForAdmin(t *testing.T) {
	t.Parallel()

	runtime := newStaticDemoAdminTestRuntime(t)
	page, err := runtime.SearchAssets(catalog.AssetSearchRequest{
		Collection: catalog.AssetCollectionRequest{Kind: catalog.CollectionKindTimeline},
		Page:       catalog.AssetSearchPageRequest{Index: 0, Size: 1},
	})
	if err != nil {
		t.Fatalf("SearchAssets() error = %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}

	recorder := httptest.NewRecorder()
	request := authenticatedRequest(http.MethodGet, "/v1/assets/"+url.PathEscape(page.Items[0].ID)+"/preview", nil)
	NewMux(runtime).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", got)
	}
	if recorder.Body.Len() == 0 {
		t.Fatal("preview response body is empty")
	}
}

func TestSemanticModelsReturnsNoSyntheticProfileBeforeInstall(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload catalog.SemanticModelRegistryStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.Active.ModelID != "" || payload.Active.ModelPack != nil || payload.Active.Runtime != nil || len(payload.Profiles) != 0 || payload.RegistryStatus != "disabled" {
		t.Fatalf("semantic model registry payload = %#v", payload)
	}
	if payload.IndexingWorker == nil || payload.IndexingWorker.Enabled || payload.IndexingWorker.Status != "disabled" || payload.IndexingWorker.MessageCode != "semantic_indexing_worker_disabled" {
		t.Fatalf("candidate indexing worker = %#v", payload.IndexingWorker)
	}
}

func TestSemanticModelsCachedReturnsLastKnownStatus(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, 5)
	runtime.RememberSemanticModelRegistryStatus(catalog.SemanticModelRegistryStatus{
		RegistryStatus: "available",
		Profiles: []catalog.SemanticModelProfileStatus{{
			ModelID:       "cached-model",
			VectorSpaceID: "cached-model/d768",
			EmbeddingDim:  768,
			ProfileKind:   "modelPack",
			InputKind:     "image",
		}},
	})
	recorder := httptest.NewRecorder()
	api := &server{runtime: runtime, options: Options{}}
	api.semanticModels(recorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models?cached=1", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload catalog.SemanticModelRegistryStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.RegistryStatus != "available" ||
		len(payload.Profiles) != 1 ||
		payload.Profiles[0].ModelID != "cached-model" {
		t.Fatalf("semantic models cached payload = %+v, want cached model status", payload)
	}
}

func TestSemanticModelsReportsIndexingWorkerSchedule(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.Indexing = config.SemanticIndexingConfig{
			Enabled:                true,
			Interval:               "45s",
			BatchSize:              7,
			TargetCompletedVectors: 10000,
		}
		cfg.WorkerRuntime.HeavyTaskWorkers = adminTestIntPtr(3)
		cfg.Datasources = []config.DatasourceConfig{{
			SourceKey: "1111111111111111",
			Name:      "Home Immich",
			Kind:      "immich_indexed",
			URL:       "http://immich.local:2283",
			Indexing:  &config.DatasourceIndexingConfig{},
		}}
	})
	defer runtime.Close()

	recorder := httptest.NewRecorder()
	NewMux(runtime).ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		IndexingWorker struct {
			Enabled                bool   `json:"enabled"`
			Status                 string `json:"status"`
			IntervalSeconds        int64  `json:"intervalSeconds"`
			BatchSize              int    `json:"batchSize"`
			WorkerCount            int    `json:"workerCount"`
			TargetCompletedVectors int    `json:"targetCompletedVectors"`
			MessageCode            string `json:"messageCode"`
		} `json:"indexingWorker"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if !payload.IndexingWorker.Enabled ||
		payload.IndexingWorker.Status != "scheduled" ||
		payload.IndexingWorker.IntervalSeconds != 45 ||
		payload.IndexingWorker.BatchSize != 7 ||
		payload.IndexingWorker.WorkerCount != 1 ||
		payload.IndexingWorker.TargetCompletedVectors != 10000 ||
		payload.IndexingWorker.MessageCode != "semantic_indexing_worker_scheduled" {
		t.Fatalf("candidate indexing worker = %#v", payload.IndexingWorker)
	}
}

func TestSemanticModelsIncludesRecommendedManifestModel(t *testing.T) {
	t.Parallel()

	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"schemaVersion": 1,
			"product":       "timich-semantic-models",
			"version":       "2026.06.01",
			"recommended":   "timich-multilingual-clip-small",
			"models": []map[string]any{{
				"id":             "timich-multilingual-clip-small",
				"name":           "Timich Multilingual CLIP Small",
				"version":        "2026.06.01",
				"vectorSpaceId":  "timich-multilingual-clip-small/2026.06/d512",
				"embeddingDim":   512,
				"inputKind":      "image",
				"queryLanguages": []string{"en", "ja", "multilingual"},
				"runtime":        "onnxruntime",
				"quantization":   "int8",
				"license":        "Apache-2.0",
				"artifacts": map[string]any{
					"default": map[string]any{
						"filename":  "timich-multilingual-clip-small.tar.zst",
						"url":       "https://example.test/models/timich-multilingual-clip-small.tar.zst",
						"sha256":    strings.Repeat("b", 64),
						"sizeBytes": 123456789,
					},
				},
			}},
		})
	}))
	defer manifestServer.Close()

	handler := NewMuxWithOptions(newTestRuntime(t, 5), Options{
		SemanticModelManifestURL: manifestServer.URL,
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		RegistryStatus string `json:"registryStatus"`
		Recommended    struct {
			ModelID       string `json:"modelId"`
			VectorSpaceID string `json:"vectorSpaceId"`
			EmbeddingDim  int    `json:"embeddingDim"`
			Role          string `json:"role"`
			ProfileKind   string `json:"profileKind"`
			InputKind     string `json:"inputKind"`
			ModelPack     struct {
				Version        string   `json:"version"`
				Role           string   `json:"role"`
				Status         string   `json:"status"`
				QueryLanguages []string `json:"queryLanguages"`
				Runtime        string   `json:"runtime"`
				Quantization   string   `json:"quantization"`
				SizeBytes      int64    `json:"sizeBytes"`
				License        string   `json:"license"`
				Installed      bool     `json:"installed"`
				Artifact       struct {
					Filename  string `json:"filename"`
					URL       string `json:"url"`
					SHA256    string `json:"sha256"`
					SizeBytes int64  `json:"sizeBytes"`
				} `json:"artifact"`
			} `json:"modelPack"`
		} `json:"recommended"`
		Profiles []map[string]any `json:"profiles"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.RegistryStatus != "available" {
		t.Fatalf("registryStatus = %q, want available payload=%#v", payload.RegistryStatus, payload)
	}
	if payload.Recommended.ModelID != "timich-multilingual-clip-small" ||
		payload.Recommended.VectorSpaceID != "timich-multilingual-clip-small/2026.06/d512" ||
		payload.Recommended.EmbeddingDim != 512 ||
		payload.Recommended.Role != "recommended" ||
		payload.Recommended.ProfileKind != "modelPack" ||
		payload.Recommended.InputKind != "image" {
		t.Fatalf("recommended = %#v", payload.Recommended)
	}
	if payload.Recommended.ModelPack.Status != "available" || payload.Recommended.ModelPack.Role != "recommended" || payload.Recommended.ModelPack.Installed {
		t.Fatalf("recommended model pack status = %#v", payload.Recommended.ModelPack)
	}
	if payload.Recommended.ModelPack.Artifact.URL != "https://example.test/models/timich-multilingual-clip-small.tar.zst" ||
		payload.Recommended.ModelPack.Artifact.SHA256 != strings.Repeat("b", 64) ||
		payload.Recommended.ModelPack.Artifact.SizeBytes != 123456789 {
		t.Fatalf("recommended artifact = %#v", payload.Recommended.ModelPack.Artifact)
	}
	if len(payload.Recommended.ModelPack.QueryLanguages) != 3 ||
		payload.Recommended.ModelPack.Runtime != "onnxruntime" ||
		payload.Recommended.ModelPack.Quantization != "int8" ||
		payload.Recommended.ModelPack.SizeBytes != 123456789 ||
		payload.Recommended.ModelPack.License != "Apache-2.0" {
		t.Fatalf("recommended pack metadata = %#v", payload.Recommended.ModelPack)
	}
	if len(payload.Profiles) != 1 {
		t.Fatalf("profiles = %#v, want only the recommended installable model", payload.Profiles)
	}
}

func TestSemanticModelsInstallsRecommendedRuntimePack(t *testing.T) {
	t.Parallel()

	artifact := semanticRuntimePackZipArtifact(t)
	sum := sha256.Sum256(artifact)
	artifactSHA := hex.EncodeToString(sum[:])
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			writeJSON(w, http.StatusOK, map[string]any{
				"schemaVersion":          1,
				"product":                "timich-semantic-models",
				"recommendedRuntimePack": "timich-onnxruntime-bundle",
				"runtimePacks": []map[string]any{{
					"id":      "timich-onnxruntime-bundle",
					"name":    "Timich ONNX Runtime Bundle",
					"version": "2026.06.01",
					"runtime": "onnxruntime",
					"license": "Apache-2.0",
					"artifacts": map[string]any{
						"default": map[string]any{
							"filename":  "timich-onnxruntime-bundle.zip",
							"url":       "http://" + r.Host + "/runtime.zip",
							"sha256":    artifactSHA,
							"sizeBytes": len(artifact),
						},
					},
				}},
			})
		case "/runtime.zip":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	defer manifestServer.Close()

	runtime := newTestRuntime(t, 5)
	handler := NewMuxWithOptions(runtime, Options{
		SemanticModelManifestURL: manifestServer.URL + "/manifest.json",
	})
	installRecorder := httptest.NewRecorder()

	handler.ServeHTTP(installRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-runtime-packs/recommended/install", nil))

	if installRecorder.Code != http.StatusAccepted {
		t.Fatalf("install status = %d, want 202 body=%s", installRecorder.Code, installRecorder.Body.String())
	}
	job := waitSemanticInstallJob(t, handler)
	if job.Status != semanticInstallJobStatusComplete {
		t.Fatalf("semantic install job = %+v, want complete", job)
	}
	var installPayload struct {
		Status       string `json:"status"`
		BytesWritten int64  `json:"bytesWritten"`
		RuntimePack  struct {
			ID         string `json:"id"`
			Runtime    string `json:"runtime"`
			Status     string `json:"status"`
			Installed  bool   `json:"installed"`
			ServerPath string `json:"serverPath"`
			PythonPath string `json:"pythonPath"`
			Artifact   struct {
				Filename  string `json:"filename"`
				SHA256    string `json:"sha256"`
				SizeBytes int64  `json:"sizeBytes"`
			} `json:"artifact"`
		} `json:"runtimePack"`
	}
	if err := json.Unmarshal(job.Result, &installPayload); err != nil {
		t.Fatalf("install result is not JSON: %v result=%s job=%+v", err, string(job.Result), job)
	}
	if installPayload.Status != "installed" ||
		installPayload.RuntimePack.ID != "timich-onnxruntime-bundle" ||
		installPayload.RuntimePack.Runtime != "onnxruntime" ||
		installPayload.RuntimePack.Status != "installed" ||
		!installPayload.RuntimePack.Installed {
		t.Fatalf("install payload = %#v", installPayload)
	}
	if installPayload.BytesWritten != int64(len(artifact)) ||
		installPayload.RuntimePack.Artifact.SizeBytes != int64(len(artifact)) ||
		installPayload.RuntimePack.Artifact.SHA256 != artifactSHA {
		t.Fatalf("install artifact payload = %#v", installPayload)
	}
	if installPayload.RuntimePack.ServerPath == "" || installPayload.RuntimePack.PythonPath == "" {
		t.Fatalf("runtime pack paths = %#v, want server and python paths", installPayload.RuntimePack)
	}
	if _, err := os.Stat(installPayload.RuntimePack.ServerPath); err != nil {
		t.Fatalf("Stat(serverPath) error = %v", err)
	}
	if _, err := os.Stat(installPayload.RuntimePack.PythonPath); err != nil {
		t.Fatalf("Stat(pythonPath) error = %v", err)
	}

	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))

	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var statusPayload struct {
		RecommendedRuntimePack struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			Installed bool   `json:"installed"`
		} `json:"recommendedRuntimePack"`
		RuntimePacks []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			Installed bool   `json:"installed"`
		} `json:"runtimePacks"`
	}
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &statusPayload); err != nil {
		t.Fatalf("status response is not JSON: %v body=%s", err, statusRecorder.Body.String())
	}
	if statusPayload.RecommendedRuntimePack.ID != "timich-onnxruntime-bundle" ||
		statusPayload.RecommendedRuntimePack.Status != "installed" ||
		!statusPayload.RecommendedRuntimePack.Installed {
		t.Fatalf("recommended runtime pack = %#v", statusPayload.RecommendedRuntimePack)
	}
	if len(statusPayload.RuntimePacks) != 1 ||
		statusPayload.RuntimePacks[0].ID != "timich-onnxruntime-bundle" ||
		!statusPayload.RuntimePacks[0].Installed {
		t.Fatalf("runtime packs = %#v", statusPayload.RuntimePacks)
	}
}

func TestSemanticManifestDefaultsToStandardSigLIP2Model(t *testing.T) {
	t.Parallel()

	model, ok := semanticManifestRecommendedModel(semanticModelManifest{
		Models: []semanticManifestModel{
			{
				ID:            "timich-multilingual-clip-small",
				Name:          "Older CLIP",
				VectorSpaceID: "timich-multilingual-clip-small/d512",
				EmbeddingDim:  512,
				InputKind:     "image",
			},
			{
				ID:            defaultRecommendedSemanticModelID,
				Name:          "SigLIP2",
				VectorSpaceID: defaultRecommendedSemanticModelID + "/d768",
				EmbeddingDim:  768,
				InputKind:     "image",
			},
		},
	})
	if !ok || model.ID != defaultRecommendedSemanticModelID {
		t.Fatalf("recommended model = %#v ok=%v", model, ok)
	}
}

func TestSemanticModelsInstallsRecommendedModelPack(t *testing.T) {
	t.Parallel()

	artifact := semanticModelZipArtifact(t)
	sum := sha256.Sum256(artifact)
	artifactSHA := hex.EncodeToString(sum[:])
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			writeJSON(w, http.StatusOK, map[string]any{
				"schemaVersion": 1,
				"product":       "timich-semantic-models",
				"recommended":   "timich-multilingual-clip-small",
				"models": []map[string]any{{
					"id":            "timich-multilingual-clip-small",
					"name":          "Timich Multilingual CLIP Small",
					"version":       "2026.06.01",
					"vectorSpaceId": "timich-multilingual-clip-small/2026.06/d512",
					"embeddingDim":  512,
					"inputKind":     "image",
					"runtime":       "onnxruntime",
					"artifacts": map[string]any{
						"default": map[string]any{
							"filename":  "timich-multilingual-clip-small.zip",
							"url":       "http://" + r.Host + "/model.zip",
							"sha256":    artifactSHA,
							"sizeBytes": len(artifact),
						},
					},
				}},
			})
		case "/model.zip":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	defer manifestServer.Close()

	runtime := newTestRuntime(t, 5)
	handler := NewMuxWithOptions(runtime, Options{
		SemanticModelManifestURL: manifestServer.URL + "/manifest.json",
	})
	installRecorder := httptest.NewRecorder()

	handler.ServeHTTP(installRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/recommended/install", nil))

	if installRecorder.Code != http.StatusAccepted {
		t.Fatalf("install status = %d, want 202 body=%s", installRecorder.Code, installRecorder.Body.String())
	}
	job := waitSemanticInstallJob(t, handler)
	if job.Status != semanticInstallJobStatusComplete {
		t.Fatalf("semantic install job = %+v, want complete", job)
	}
	var installPayload struct {
		Status       string `json:"status"`
		BytesWritten int64  `json:"bytesWritten"`
		ModelPack    struct {
			ID        string `json:"id"`
			Role      string `json:"role"`
			Status    string `json:"status"`
			Installed bool   `json:"installed"`
			Artifact  struct {
				Filename  string `json:"filename"`
				SHA256    string `json:"sha256"`
				SizeBytes int64  `json:"sizeBytes"`
			} `json:"artifact"`
		} `json:"modelPack"`
	}
	if err := json.Unmarshal(job.Result, &installPayload); err != nil {
		t.Fatalf("install result is not JSON: %v result=%s job=%+v", err, string(job.Result), job)
	}
	if installPayload.Status != "installed" || installPayload.ModelPack.ID != "timich-multilingual-clip-small" || installPayload.ModelPack.Role != "candidate" || !installPayload.ModelPack.Installed {
		t.Fatalf("install payload = %#v", installPayload)
	}
	if installPayload.BytesWritten != int64(len(artifact)) || installPayload.ModelPack.Artifact.SizeBytes != int64(len(artifact)) || installPayload.ModelPack.Artifact.SHA256 != artifactSHA {
		t.Fatalf("install artifact payload = %#v", installPayload)
	}

	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))

	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var statusPayload struct {
		Recommended struct {
			Role      string `json:"role"`
			ModelPack struct {
				Role      string `json:"role"`
				Status    string `json:"status"`
				Installed bool   `json:"installed"`
			} `json:"modelPack"`
		} `json:"recommended"`
		Candidate struct {
			ModelID   string `json:"modelId"`
			Role      string `json:"role"`
			ModelPack struct {
				Role      string `json:"role"`
				Status    string `json:"status"`
				Installed bool   `json:"installed"`
			} `json:"modelPack"`
			Runtime struct {
				Status         string `json:"status"`
				Runtime        string `json:"runtime"`
				Loader         string `json:"loader"`
				ArtifactStatus string `json:"artifactStatus"`
				ArtifactFormat string `json:"artifactFormat"`
				LayoutStatus   string `json:"layoutStatus"`
				Loaded         bool   `json:"loaded"`
				CanEmbed       bool   `json:"canEmbed"`
				MessageCode    string `json:"messageCode"`
			} `json:"runtime"`
		} `json:"candidate"`
	}
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &statusPayload); err != nil {
		t.Fatalf("status response is not JSON: %v body=%s", err, statusRecorder.Body.String())
	}
	if statusPayload.Recommended.Role != "candidate" || statusPayload.Recommended.ModelPack.Role != "candidate" || statusPayload.Recommended.ModelPack.Status != "installed" || !statusPayload.Recommended.ModelPack.Installed {
		t.Fatalf("recommended after install = %#v", statusPayload.Recommended.ModelPack)
	}
	if statusPayload.Candidate.ModelID != "timich-multilingual-clip-small" || statusPayload.Candidate.Role != "candidate" || statusPayload.Candidate.ModelPack.Status != "installed" || !statusPayload.Candidate.ModelPack.Installed {
		t.Fatalf("candidate after install = %#v", statusPayload.Candidate)
	}
	if statusPayload.Candidate.Runtime.Status != "loaded" ||
		statusPayload.Candidate.Runtime.Runtime != "onnxruntime" ||
		statusPayload.Candidate.Runtime.Loader != "onnxruntime" ||
		statusPayload.Candidate.Runtime.ArtifactStatus != "available" ||
		statusPayload.Candidate.Runtime.ArtifactFormat != "zip" ||
		statusPayload.Candidate.Runtime.LayoutStatus != "ready" ||
		!statusPayload.Candidate.Runtime.Loaded ||
		!statusPayload.Candidate.Runtime.CanEmbed ||
		statusPayload.Candidate.Runtime.MessageCode != "semantic_runtime_helper_loaded" {
		t.Fatalf("candidate runtime after install = %#v", statusPayload.Candidate.Runtime)
	}

	offlineRecorder := httptest.NewRecorder()
	NewMux(runtime).ServeHTTP(offlineRecorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))

	if offlineRecorder.Code != http.StatusOK {
		t.Fatalf("offline status = %d, want 200 body=%s", offlineRecorder.Code, offlineRecorder.Body.String())
	}
	var offlinePayload struct {
		RegistryStatus string `json:"registryStatus"`
		Candidate      struct {
			ModelID   string `json:"modelId"`
			Role      string `json:"role"`
			ModelPack struct {
				Status    string `json:"status"`
				Installed bool   `json:"installed"`
			} `json:"modelPack"`
		} `json:"candidate"`
	}
	if err := json.Unmarshal(offlineRecorder.Body.Bytes(), &offlinePayload); err != nil {
		t.Fatalf("offline status response is not JSON: %v body=%s", err, offlineRecorder.Body.String())
	}
	if offlinePayload.RegistryStatus != "disabled" {
		t.Fatalf("offline registryStatus = %q, want disabled", offlinePayload.RegistryStatus)
	}
	if offlinePayload.Candidate.ModelID != "timich-multilingual-clip-small" || offlinePayload.Candidate.Role != "candidate" || offlinePayload.Candidate.ModelPack.Status != "installed" || !offlinePayload.Candidate.ModelPack.Installed {
		t.Fatalf("offline candidate = %#v", offlinePayload.Candidate)
	}
}

func TestSemanticModelsInstallsModelPackByIdentity(t *testing.T) {
	t.Parallel()

	artifact := semanticModelZipArtifactForIdentity(t, "timich-low-memory-siglip", "timich-low-memory-siglip/2026.06/d384", 384)
	sum := sha256.Sum256(artifact)
	artifactSHA := hex.EncodeToString(sum[:])
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			writeJSON(w, http.StatusOK, map[string]any{
				"schemaVersion": 1,
				"product":       "timich-semantic-models",
				"recommended":   "timich-standard-siglip",
				"models": []map[string]any{
					{
						"id":             "timich-standard-siglip",
						"name":           "Timich Standard SigLIP",
						"version":        "2026.06.01",
						"vectorSpaceId":  "timich-standard-siglip/2026.06/d768",
						"embeddingDim":   768,
						"inputKind":      "image",
						"runtime":        "onnxruntime",
						"queryLanguages": []string{"en", "ja"},
						"artifacts": map[string]any{
							"default": map[string]any{
								"filename":  "timich-standard-siglip.tar.zst",
								"url":       "http://" + r.Host + "/standard.tar.zst",
								"sha256":    strings.Repeat("a", 64),
								"sizeBytes": 1,
							},
						},
					},
					{
						"id":            "timich-low-memory-siglip",
						"name":          "Timich Low Memory SigLIP",
						"version":       "2026.06.02",
						"vectorSpaceId": "timich-low-memory-siglip/2026.06/d384",
						"embeddingDim":  384,
						"inputKind":     "image",
						"runtime":       "onnxruntime",
						"quantization":  "int8",
						"artifacts": map[string]any{
							"default": map[string]any{
								"filename":  "timich-low-memory-siglip.zip",
								"url":       "http://" + r.Host + "/low-memory.zip",
								"sha256":    artifactSHA,
								"sizeBytes": len(artifact),
							},
						},
					},
				},
			})
		case "/low-memory.zip":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	defer manifestServer.Close()

	runtime := newTestRuntime(t, 5)
	handler := NewMuxWithOptions(runtime, Options{
		SemanticModelManifestURL: manifestServer.URL + "/manifest.json",
	})
	installRecorder := httptest.NewRecorder()
	handler.ServeHTTP(installRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/install", bytes.NewReader([]byte(`{
		"modelId": "timich-low-memory-siglip",
		"vectorSpaceId": "timich-low-memory-siglip/2026.06/d384"
	}`))))

	if installRecorder.Code != http.StatusAccepted {
		t.Fatalf("install status = %d, want 202 body=%s", installRecorder.Code, installRecorder.Body.String())
	}
	job := waitSemanticInstallJob(t, handler)
	if job.Status != semanticInstallJobStatusComplete {
		t.Fatalf("semantic install job = %+v, want complete", job)
	}
	var installPayload struct {
		Status       string `json:"status"`
		BytesWritten int64  `json:"bytesWritten"`
		ModelPack    struct {
			ID            string `json:"id"`
			VectorSpaceID string `json:"vectorSpaceId"`
			Role          string `json:"role"`
			Status        string `json:"status"`
			Installed     bool   `json:"installed"`
			Quantization  string `json:"quantization"`
			Artifact      struct {
				Filename string `json:"filename"`
				SHA256   string `json:"sha256"`
			} `json:"artifact"`
		} `json:"modelPack"`
	}
	if err := json.Unmarshal(job.Result, &installPayload); err != nil {
		t.Fatalf("install result is not JSON: %v result=%s job=%+v", err, string(job.Result), job)
	}
	if installPayload.Status != "installed" ||
		installPayload.ModelPack.ID != "timich-low-memory-siglip" ||
		installPayload.ModelPack.VectorSpaceID != "timich-low-memory-siglip/2026.06/d384" ||
		installPayload.ModelPack.Role != "candidate" ||
		installPayload.ModelPack.Status != "installed" ||
		!installPayload.ModelPack.Installed ||
		installPayload.ModelPack.Quantization != "int8" ||
		installPayload.BytesWritten != int64(len(artifact)) ||
		installPayload.ModelPack.Artifact.Filename != "timich-low-memory-siglip.zip" ||
		installPayload.ModelPack.Artifact.SHA256 != artifactSHA {
		t.Fatalf("install payload = %#v", installPayload)
	}

	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var statusPayload struct {
		Recommended struct {
			ModelID   string `json:"modelId"`
			Role      string `json:"role"`
			ModelPack struct {
				Installed bool `json:"installed"`
			} `json:"modelPack"`
		} `json:"recommended"`
		Candidate struct {
			ModelID       string `json:"modelId"`
			VectorSpaceID string `json:"vectorSpaceId"`
			Role          string `json:"role"`
			ModelPack     struct {
				Status    string `json:"status"`
				Installed bool   `json:"installed"`
			} `json:"modelPack"`
		} `json:"candidate"`
		Profiles []struct {
			ModelID       string `json:"modelId"`
			VectorSpaceID string `json:"vectorSpaceId"`
			Role          string `json:"role"`
			ModelPack     struct {
				Installed bool `json:"installed"`
			} `json:"modelPack"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &statusPayload); err != nil {
		t.Fatalf("status response is not JSON: %v body=%s", err, statusRecorder.Body.String())
	}
	if statusPayload.Recommended.ModelID != "timich-standard-siglip" || statusPayload.Recommended.Role != "recommended" || statusPayload.Recommended.ModelPack.Installed {
		t.Fatalf("recommended status = %#v", statusPayload.Recommended)
	}
	if statusPayload.Candidate.ModelID != "timich-low-memory-siglip" ||
		statusPayload.Candidate.VectorSpaceID != "timich-low-memory-siglip/2026.06/d384" ||
		statusPayload.Candidate.Role != "candidate" ||
		statusPayload.Candidate.ModelPack.Status != "installed" ||
		!statusPayload.Candidate.ModelPack.Installed {
		t.Fatalf("candidate status = %#v", statusPayload.Candidate)
	}
	foundRecommended := false
	foundInstalled := false
	for _, profile := range statusPayload.Profiles {
		if profile.ModelID == "timich-standard-siglip" && profile.Role == "recommended" && !profile.ModelPack.Installed {
			foundRecommended = true
		}
		if profile.ModelID == "timich-low-memory-siglip" &&
			profile.VectorSpaceID == "timich-low-memory-siglip/2026.06/d384" &&
			profile.Role == "candidate" &&
			profile.ModelPack.Installed {
			foundInstalled = true
		}
	}
	if !foundRecommended || !foundInstalled {
		t.Fatalf("profiles = %#v, want recommended registry model and installed requested model", statusPayload.Profiles)
	}
}

func TestSemanticModelInstallRunsAsBackgroundJobAndBlocksActivation(t *testing.T) {
	t.Parallel()

	artifact := semanticModelZipArtifactForIdentity(t, "timich-standard-siglip", "timich-standard-siglip/2026.06/d4", 4)
	sum := sha256.Sum256(artifact)
	artifactSHA := hex.EncodeToString(sum[:])
	releaseArtifact := make(chan struct{})
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			writeJSON(w, http.StatusOK, map[string]any{
				"schemaVersion": 1,
				"product":       "timich-semantic-models",
				"recommended":   "timich-standard-siglip",
				"models": []map[string]any{{
					"id":            "timich-standard-siglip",
					"name":          "Timich Standard SigLIP",
					"version":       "2026.06.01",
					"vectorSpaceId": "timich-standard-siglip/2026.06/d4",
					"embeddingDim":  4,
					"inputKind":     "image",
					"runtime":       "onnxruntime",
					"artifacts": map[string]any{
						"default": map[string]any{
							"filename":  "timich-standard-siglip.zip",
							"url":       "http://" + r.Host + "/standard.zip",
							"sha256":    artifactSHA,
							"sizeBytes": len(artifact),
						},
					},
				}},
			})
		case "/standard.zip":
			<-releaseArtifact
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	defer manifestServer.Close()

	handler := NewMuxWithOptions(newTestRuntime(t, 5), Options{
		SemanticModelManifestURL: manifestServer.URL + "/manifest.json",
	})
	installRecorder := httptest.NewRecorder()
	handler.ServeHTTP(installRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/install", bytes.NewReader([]byte(`{
		"modelId": "timich-standard-siglip",
		"vectorSpaceId": "timich-standard-siglip/2026.06/d4"
	}`))))
	if installRecorder.Code != http.StatusAccepted {
		t.Fatalf("install status = %d, want 202 body=%s", installRecorder.Code, installRecorder.Body.String())
	}
	var started semanticInstallJobStatus
	if err := json.Unmarshal(installRecorder.Body.Bytes(), &started); err != nil {
		t.Fatalf("install response is not JSON: %v body=%s", err, installRecorder.Body.String())
	}
	if started.Status != semanticInstallJobStatusRunning {
		t.Fatalf("started job = %+v, want running", started)
	}

	activateRecorder := httptest.NewRecorder()
	handler.ServeHTTP(activateRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/activate", bytes.NewReader([]byte(`{
		"modelId": "timich-standard-siglip",
		"vectorSpaceId": "timich-standard-siglip/2026.06/d4"
	}`))))
	assertErrorPayload(t, activateRecorder, http.StatusConflict, "semantic_install_running")

	close(releaseArtifact)
	job := waitSemanticInstallJob(t, handler)
	if job.Status != semanticInstallJobStatusComplete {
		t.Fatalf("semantic install job = %+v, want complete", job)
	}
}

func TestSemanticModelsInstallKeepsPackWhenIndexingScheduleFails(t *testing.T) {
	t.Parallel()

	artifact := semanticModelZipArtifactForIdentity(t, "timich-standard-siglip", "timich-standard-siglip/2026.06/d4", 4)
	sum := sha256.Sum256(artifact)
	artifactSHA := hex.EncodeToString(sum[:])
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			writeJSON(w, http.StatusOK, map[string]any{
				"schemaVersion": 1,
				"product":       "timich-semantic-models",
				"recommended":   "timich-standard-siglip",
				"models": []map[string]any{{
					"id":            "timich-standard-siglip",
					"name":          "Timich Standard SigLIP",
					"version":       "2026.06.01",
					"vectorSpaceId": "timich-standard-siglip/2026.06/d4",
					"embeddingDim":  4,
					"inputKind":     "image",
					"runtime":       "onnxruntime",
					"artifacts": map[string]any{
						"default": map[string]any{
							"filename":  "timich-standard-siglip.zip",
							"url":       "http://" + r.Host + "/standard.zip",
							"sha256":    artifactSHA,
							"sizeBytes": len(artifact),
						},
					},
				}},
			})
		case "/standard.zip":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	defer manifestServer.Close()

	configPath := filepath.Join(t.TempDir(), "agent-config-dir")
	if err := os.Mkdir(configPath, 0o700); err != nil {
		t.Fatalf("Mkdir(configPath) error = %v", err)
	}
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.ConfigPath = configPath
	})
	handler := NewMuxWithOptions(runtime, Options{
		SemanticModelManifestURL: manifestServer.URL + "/manifest.json",
	})
	installRecorder := httptest.NewRecorder()
	handler.ServeHTTP(installRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/install", bytes.NewReader([]byte(`{
		"modelId": "timich-standard-siglip",
		"vectorSpaceId": "timich-standard-siglip/2026.06/d4"
	}`))))

	if installRecorder.Code != http.StatusAccepted {
		t.Fatalf("install status = %d, want 202 body=%s", installRecorder.Code, installRecorder.Body.String())
	}
	job := waitSemanticInstallJob(t, handler)
	if job.Status != semanticInstallJobStatusComplete {
		t.Fatalf("semantic install job = %+v, want complete", job)
	}
	var installPayload struct {
		Status         string `json:"status"`
		WarningCode    string `json:"warningCode"`
		WarningMessage string `json:"warningMessage"`
		ModelPack      struct {
			ID        string `json:"id"`
			Installed bool   `json:"installed"`
			Role      string `json:"role"`
		} `json:"modelPack"`
	}
	if err := json.Unmarshal(job.Result, &installPayload); err != nil {
		t.Fatalf("install result is not JSON: %v result=%s job=%+v", err, string(job.Result), job)
	}
	if installPayload.Status != "installed" ||
		installPayload.WarningCode != "semantic_indexing_schedule_failed" ||
		installPayload.WarningMessage == "" ||
		installPayload.ModelPack.ID != "timich-standard-siglip" ||
		!installPayload.ModelPack.Installed ||
		installPayload.ModelPack.Role != "candidate" {
		t.Fatalf("install payload = %#v", installPayload)
	}

	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var statusPayload struct {
		Candidate struct {
			ModelID   string `json:"modelId"`
			ModelPack struct {
				Installed bool `json:"installed"`
			} `json:"modelPack"`
		} `json:"candidate"`
	}
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &statusPayload); err != nil {
		t.Fatalf("status response is not JSON: %v body=%s", err, statusRecorder.Body.String())
	}
	if statusPayload.Candidate.ModelID != "timich-standard-siglip" || !statusPayload.Candidate.ModelPack.Installed {
		t.Fatalf("candidate status after schedule failure = %#v", statusPayload.Candidate)
	}
}

func TestSemanticModelsUninstallRejectsActiveAndRemovesInactive(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.DataDir = dataDir
	})
	handler := NewMux(runtime)
	activeArtifact := semanticModelZipArtifactForIdentity(t, "api-active-siglip", "api-active-siglip/2026.06/d4", 4)
	inactiveArtifact := semanticModelZipArtifactForIdentity(t, "api-inactive-siglip", "api-inactive-siglip/2026.06/d4", 4)
	activePack := semanticModelArchivePackForTest("api-active-siglip", "api-active-siglip/2026.06/d4", "2026.06-active", activeArtifact)
	inactivePack := semanticModelArchivePackForTest("api-inactive-siglip", "api-inactive-siglip/2026.06/d4", "2026.06-inactive", inactiveArtifact)
	if _, err := runtime.InstallSemanticModelPack(context.Background(), activePack, bytes.NewReader(activeArtifact)); err != nil {
		t.Fatalf("InstallSemanticModelPack(active) error = %v", err)
	}
	modelStore, err := catalog.LoadOrCreateSemanticModelPackStore(dataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateSemanticModelPackStore() error = %v", err)
	}
	if _, err := modelStore.ActivatePack(activePack.ID, activePack.VectorSpaceID); err != nil {
		t.Fatalf("ActivatePack(active) error = %v", err)
	}
	if _, err := runtime.InstallSemanticModelPack(context.Background(), inactivePack, bytes.NewReader(inactiveArtifact)); err != nil {
		t.Fatalf("InstallSemanticModelPack(inactive) error = %v", err)
	}

	activeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(activeRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/uninstall", bytes.NewReader([]byte(`{
		"modelId": "api-active-siglip",
		"vectorSpaceId": "api-active-siglip/2026.06/d4"
	}`))))
	if activeRecorder.Code != http.StatusConflict {
		t.Fatalf("active uninstall status = %d, want 409 body=%s", activeRecorder.Code, activeRecorder.Body.String())
	}
	var activeError struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(activeRecorder.Body.Bytes(), &activeError); err != nil {
		t.Fatalf("active uninstall response is not JSON: %v body=%s", err, activeRecorder.Body.String())
	}
	if activeError.Error != "semantic_model_active" {
		t.Fatalf("active uninstall error = %#v, want semantic_model_active", activeError)
	}

	inactiveRecorder := httptest.NewRecorder()
	handler.ServeHTTP(inactiveRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/uninstall", bytes.NewReader([]byte(`{
		"modelId": "api-inactive-siglip",
		"vectorSpaceId": "api-inactive-siglip/2026.06/d4"
	}`))))
	if inactiveRecorder.Code != http.StatusOK {
		t.Fatalf("inactive uninstall status = %d, want 200 body=%s", inactiveRecorder.Code, inactiveRecorder.Body.String())
	}
	var inactivePayload struct {
		Status        string    `json:"status"`
		ModelID       string    `json:"modelId"`
		VectorSpaceID string    `json:"vectorSpaceId"`
		UninstalledAt time.Time `json:"uninstalledAt"`
	}
	if err := json.Unmarshal(inactiveRecorder.Body.Bytes(), &inactivePayload); err != nil {
		t.Fatalf("inactive uninstall response is not JSON: %v body=%s", err, inactiveRecorder.Body.String())
	}
	if inactivePayload.Status != "uninstalled" ||
		inactivePayload.ModelID != "api-inactive-siglip" ||
		inactivePayload.VectorSpaceID != "api-inactive-siglip/2026.06/d4" ||
		inactivePayload.UninstalledAt.IsZero() {
		t.Fatalf("inactive uninstall payload = %#v", inactivePayload)
	}

	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var statusPayload struct {
		Active struct {
			ModelID       string `json:"modelId"`
			VectorSpaceID string `json:"vectorSpaceId"`
			ModelPack     struct {
				Installed bool `json:"installed"`
			} `json:"modelPack"`
		} `json:"active"`
		Profiles []struct {
			ModelID       string `json:"modelId"`
			VectorSpaceID string `json:"vectorSpaceId"`
			ModelPack     struct {
				Installed bool `json:"installed"`
			} `json:"modelPack"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &statusPayload); err != nil {
		t.Fatalf("status response is not JSON: %v body=%s", err, statusRecorder.Body.String())
	}
	if statusPayload.Active.ModelID != "api-active-siglip" ||
		statusPayload.Active.VectorSpaceID != "api-active-siglip/2026.06/d4" ||
		!statusPayload.Active.ModelPack.Installed {
		t.Fatalf("active status after inactive uninstall = %#v", statusPayload.Active)
	}
	for _, profile := range statusPayload.Profiles {
		if profile.ModelID == "api-inactive-siglip" && profile.VectorSpaceID == "api-inactive-siglip/2026.06/d4" && profile.ModelPack.Installed {
			t.Fatalf("inactive profile is still installed after uninstall: %#v", profile)
		}
	}
}

func TestSemanticModelsInstallsZipModelPackWithReadyRuntimeLayout(t *testing.T) {
	t.Parallel()

	artifact := semanticModelZipArtifact(t)
	sum := sha256.Sum256(artifact)
	artifactSHA := hex.EncodeToString(sum[:])
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			writeJSON(w, http.StatusOK, map[string]any{
				"schemaVersion": 1,
				"product":       "timich-semantic-models",
				"recommended":   "timich-multilingual-clip-small",
				"models": []map[string]any{{
					"id":            "timich-multilingual-clip-small",
					"name":          "Timich Multilingual CLIP Small",
					"version":       "2026.06.01",
					"vectorSpaceId": "timich-multilingual-clip-small/2026.06/d512",
					"embeddingDim":  512,
					"inputKind":     "image",
					"runtime":       "onnxruntime",
					"artifacts": map[string]any{
						"default": map[string]any{
							"filename":  "timich-multilingual-clip-small.zip",
							"url":       "http://" + r.Host + "/model.zip",
							"sha256":    artifactSHA,
							"sizeBytes": len(artifact),
						},
					},
				}},
			})
		case "/model.zip":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	defer manifestServer.Close()

	handler := NewMuxWithOptions(newTestRuntime(t, 5), Options{
		SemanticModelManifestURL: manifestServer.URL + "/manifest.json",
	})
	installRecorder := httptest.NewRecorder()
	handler.ServeHTTP(installRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/recommended/install", nil))

	if installRecorder.Code != http.StatusAccepted {
		t.Fatalf("install status = %d, want 202 body=%s", installRecorder.Code, installRecorder.Body.String())
	}
	if job := waitSemanticInstallJob(t, handler); job.Status != semanticInstallJobStatusComplete {
		t.Fatalf("semantic install job = %+v, want complete", job)
	}
	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))

	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var payload struct {
		Candidate struct {
			Runtime struct {
				Status         string `json:"status"`
				ArtifactStatus string `json:"artifactStatus"`
				ArtifactFormat string `json:"artifactFormat"`
				LayoutStatus   string `json:"layoutStatus"`
				HelperStatus   string `json:"helperStatus"`
				Loaded         bool   `json:"loaded"`
				CanEmbed       bool   `json:"canEmbed"`
				MessageCode    string `json:"messageCode"`
			} `json:"runtime"`
		} `json:"candidate"`
	}
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("status response is not JSON: %v body=%s", err, statusRecorder.Body.String())
	}
	if payload.Candidate.Runtime.Status != "loaded" ||
		payload.Candidate.Runtime.ArtifactStatus != "available" ||
		payload.Candidate.Runtime.ArtifactFormat != "zip" ||
		payload.Candidate.Runtime.LayoutStatus != "ready" ||
		payload.Candidate.Runtime.HelperStatus != "ready" ||
		!payload.Candidate.Runtime.Loaded ||
		!payload.Candidate.Runtime.CanEmbed ||
		payload.Candidate.Runtime.MessageCode != "semantic_runtime_helper_loaded" {
		t.Fatalf("candidate runtime = %#v", payload.Candidate.Runtime)
	}
}

func TestSemanticModelsLoadsZipModelPackThroughRuntimeHelper(t *testing.T) {
	t.Parallel()

	artifact := semanticModelZipArtifact(t)
	sum := sha256.Sum256(artifact)
	artifactSHA := hex.EncodeToString(sum[:])
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			writeJSON(w, http.StatusOK, map[string]any{
				"schemaVersion": 1,
				"product":       "timich-semantic-models",
				"recommended":   "timich-multilingual-clip-small",
				"models": []map[string]any{{
					"id":            "timich-multilingual-clip-small",
					"name":          "Timich Multilingual CLIP Small",
					"version":       "2026.06.01",
					"vectorSpaceId": "timich-multilingual-clip-small/2026.06/d512",
					"embeddingDim":  512,
					"inputKind":     "image",
					"runtime":       "onnxruntime",
					"artifacts": map[string]any{
						"default": map[string]any{
							"filename":  "timich-multilingual-clip-small.zip",
							"url":       "http://" + r.Host + "/model.zip",
							"sha256":    artifactSHA,
							"sizeBytes": len(artifact),
						},
					},
				}},
			})
		case "/model.zip":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	defer manifestServer.Close()

	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.HelperPath = semanticRuntimeGenericEmbeddingHelperScript(t)
	})
	handler := NewMuxWithOptions(runtime, Options{
		SemanticModelManifestURL: manifestServer.URL + "/manifest.json",
	})
	installRecorder := httptest.NewRecorder()
	handler.ServeHTTP(installRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/recommended/install", nil))

	if installRecorder.Code != http.StatusAccepted {
		t.Fatalf("install status = %d, want 202 body=%s", installRecorder.Code, installRecorder.Body.String())
	}
	if job := waitSemanticInstallJob(t, handler); job.Status != semanticInstallJobStatusComplete {
		t.Fatalf("semantic install job = %+v, want complete", job)
	}
	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))

	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var payload struct {
		Candidate struct {
			Runtime struct {
				Status         string `json:"status"`
				Runtime        string `json:"runtime"`
				Loader         string `json:"loader"`
				ArtifactStatus string `json:"artifactStatus"`
				ArtifactFormat string `json:"artifactFormat"`
				LayoutStatus   string `json:"layoutStatus"`
				HelperStatus   string `json:"helperStatus"`
				HelperProtocol int    `json:"helperProtocol"`
				Loaded         bool   `json:"loaded"`
				CanEmbed       bool   `json:"canEmbed"`
				MessageCode    string `json:"messageCode"`
			} `json:"runtime"`
		} `json:"candidate"`
	}
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("status response is not JSON: %v body=%s", err, statusRecorder.Body.String())
	}
	if payload.Candidate.Runtime.Status != "loaded" ||
		payload.Candidate.Runtime.Runtime != "onnxruntime" ||
		payload.Candidate.Runtime.Loader != "onnxruntime" ||
		payload.Candidate.Runtime.ArtifactStatus != "available" ||
		payload.Candidate.Runtime.ArtifactFormat != "zip" ||
		payload.Candidate.Runtime.LayoutStatus != "ready" ||
		payload.Candidate.Runtime.HelperStatus != "ready" ||
		payload.Candidate.Runtime.HelperProtocol != 1 ||
		!payload.Candidate.Runtime.Loaded ||
		!payload.Candidate.Runtime.CanEmbed ||
		payload.Candidate.Runtime.MessageCode != "semantic_runtime_helper_loaded" {
		t.Fatalf("candidate runtime = %#v", payload.Candidate.Runtime)
	}
}

func TestSemanticModelsRunsSemanticIndexingBatch(t *testing.T) {
	t.Parallel()

	artifact := semanticModelZipArtifactWithDim(t, 4)
	sum := sha256.Sum256(artifact)
	artifactSHA := hex.EncodeToString(sum[:])
	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			writeJSON(w, http.StatusOK, map[string]any{
				"schemaVersion": 1,
				"product":       "timich-semantic-models",
				"recommended":   "timich-multilingual-clip-small",
				"models": []map[string]any{{
					"id":            "timich-multilingual-clip-small",
					"name":          "Timich Multilingual CLIP Small",
					"version":       "2026.06.01",
					"vectorSpaceId": "timich-multilingual-clip-small/2026.06/d4",
					"embeddingDim":  4,
					"inputKind":     "image",
					"runtime":       "onnxruntime",
					"artifacts": map[string]any{
						"default": map[string]any{
							"filename":  "timich-multilingual-clip-small.zip",
							"url":       "http://" + r.Host + "/model.zip",
							"sha256":    artifactSHA,
							"sizeBytes": len(artifact),
						},
					},
				}},
			})
		case "/model.zip":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(artifact)
		case "/api/search/metadata":
			if r.Header.Get("x-api-key") != "immich-api-key" {
				t.Fatalf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"assets": {
					"total": 2,
					"items": [
						{
							"id": "asset-new",
							"type": "IMAGE",
							"originalFileName": "new.jpg",
							"fileCreatedAt": "2026-06-01T10:00:00Z",
							"updatedAt": "2026-06-01T10:05:00Z"
						},
						{
							"id": "asset-old",
							"type": "IMAGE",
							"originalFileName": "old.jpg",
							"fileCreatedAt": "2025-06-01T10:00:00Z",
							"updatedAt": "2025-06-01T10:05:00Z"
						}
					],
					"nextPage": null
				}
			}`))
		case "/api/assets/asset-new/thumbnail", "/api/assets/asset-old/thumbnail":
			if r.Header.Get("x-api-key") != "immich-api-key" {
				t.Fatalf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
			}
			if r.URL.Query().Get("size") != "preview" {
				t.Fatalf("thumbnail size = %q, want preview", r.URL.Query().Get("size"))
			}
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(semanticModelTestJPEG(t))
		default:
			http.NotFound(w, r)
		}
	}))
	defer datasourceServer.Close()

	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.HelperPath = semanticRuntimeEmbeddingHelperScript(t)
		cfg.Datasources = []config.DatasourceConfig{{
			SourceKey:   "1111111111111111",
			Name:        "Home Immich",
			Kind:        "immich_indexed",
			URL:         datasourceServer.URL,
			AccessToken: "immich-api-key",
			Indexing: &config.DatasourceIndexingConfig{
				LatestAssetLimit: 2,
			},
		}}
	})
	defer runtime.Close()
	handler := NewMuxWithOptions(runtime, Options{
		SemanticModelManifestURL: datasourceServer.URL + "/manifest.json",
	})

	installRecorder := httptest.NewRecorder()
	handler.ServeHTTP(installRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/recommended/install", nil))
	if installRecorder.Code != http.StatusAccepted {
		t.Fatalf("install status = %d, want 202 body=%s", installRecorder.Code, installRecorder.Body.String())
	}
	if job := waitSemanticInstallJob(t, handler); job.Status != semanticInstallJobStatusComplete {
		t.Fatalf("semantic install job = %+v, want complete", job)
	}

	syncRecorder := httptest.NewRecorder()
	handler.ServeHTTP(syncRecorder, authenticatedRequest(http.MethodPost, "/v1/datasources/indexing/run", bytes.NewReader([]byte(`{"mode":"full"}`))))
	if syncRecorder.Code != http.StatusOK {
		t.Fatalf("sync status = %d, want 200 body=%s", syncRecorder.Code, syncRecorder.Body.String())
	}

	preIndexSearch, err := runtime.SearchAssets(catalog.AssetSearchRequest{
		Collection: catalog.AssetCollectionRequest{
			Kind: catalog.CollectionKindSearch,
			Query: &catalog.AssetSearchQuery{
				Text: "new",
				Mode: catalog.QueryModeAuto,
			},
		},
		Page: catalog.AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() before candidate indexing error = %v", err)
	}
	if preIndexSearch.Total != 1 ||
		len(preIndexSearch.Items) != 1 ||
		preIndexSearch.Items[0].Filename != "new.jpg" ||
		preIndexSearch.Resolved.QueryMode != catalog.QueryModeFilename ||
		preIndexSearch.Resolved.Semantic == nil ||
		preIndexSearch.Resolved.Semantic.FallbackQueryMode != catalog.QueryModeFilename {
		t.Fatalf("pre-index candidate auto search = %#v, want filename fallback", preIndexSearch)
	}

	indexingRecorder := httptest.NewRecorder()
	handler.ServeHTTP(indexingRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-indexing/run", bytes.NewReader([]byte(`{"maxAssets":1}`))))
	if indexingRecorder.Code != http.StatusOK {
		t.Fatalf("indexing status = %d, want 200 body=%s", indexingRecorder.Code, indexingRecorder.Body.String())
	}
	var payload struct {
		ProcessedVectorCount int `json:"processedVectorCount"`
		IndexedVectorCount   int `json:"indexedVectorCount"`
		Status               struct {
			Status               string `json:"status"`
			ModelID              string `json:"modelId"`
			EligibleAssetCount   int    `json:"eligibleAssetCount"`
			CompletedVectorCount int    `json:"completedVectorCount"`
			IndexedVectorCount   int    `json:"indexedVectorCount"`
			RemainingVectorCount int    `json:"remainingVectorCount"`
		} `json:"status"`
	}
	if err := json.Unmarshal(indexingRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("indexing response is not JSON: %v body=%s", err, indexingRecorder.Body.String())
	}
	if payload.ProcessedVectorCount != 1 || payload.IndexedVectorCount != 0 ||
		payload.Status.Status != "backfilling" ||
		payload.Status.ModelID != "timich-multilingual-clip-small" ||
		payload.Status.EligibleAssetCount != 2 ||
		payload.Status.CompletedVectorCount != 1 ||
		payload.Status.IndexedVectorCount != 0 ||
		payload.Status.RemainingVectorCount != 1 {
		t.Fatalf("indexing payload = %#v", payload)
	}

	publishRecorder := httptest.NewRecorder()
	handler.ServeHTTP(publishRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-indexing/run", bytes.NewReader([]byte(`{"maxAssets":0}`))))
	if publishRecorder.Code != http.StatusAccepted {
		t.Fatalf("publish status = %d, want 202 body=%s", publishRecorder.Code, publishRecorder.Body.String())
	}
	var publishPayload struct {
		ProcessedVectorCount int `json:"processedVectorCount"`
		IndexedVectorCount   int `json:"indexedVectorCount"`
		Status               struct {
			IndexedVectorCount   int `json:"indexedVectorCount"`
			PendingIndexJobCount int `json:"pendingIndexJobCount"`
		} `json:"status"`
	}
	if err := json.Unmarshal(publishRecorder.Body.Bytes(), &publishPayload); err != nil {
		t.Fatalf("publish response is not JSON: %v body=%s", err, publishRecorder.Body.String())
	}
	if publishPayload.ProcessedVectorCount != 0 ||
		publishPayload.IndexedVectorCount != 0 ||
		publishPayload.Status.IndexedVectorCount != 0 ||
		publishPayload.Status.PendingIndexJobCount != 1 {
		t.Fatalf("publish payload = %#v, want one durable queued index job", publishPayload)
	}
	waitSemanticIndexPublished(t, runtime, 1)

	searchPage, err := runtime.SearchAssets(catalog.AssetSearchRequest{
		Collection: catalog.AssetCollectionRequest{
			Kind: catalog.CollectionKindSearch,
			Query: &catalog.AssetSearchQuery{
				Text: "beach",
				Mode: catalog.QueryModeSemantic,
			},
		},
		Page: catalog.AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() after candidate indexing error = %v", err)
	}
	if len(searchPage.Items) != 1 || searchPage.Total != 1 {
		t.Fatalf("search page after candidate indexing = %#v", searchPage)
	}
	if searchPage.Resolved.Semantic == nil ||
		searchPage.Resolved.Semantic.ModelID != "timich-multilingual-clip-small" ||
		searchPage.Resolved.Semantic.ProfileKind != "modelPack" ||
		searchPage.Resolved.Semantic.InputKind != "image" ||
		searchPage.Resolved.Semantic.IndexedVectorCount != 1 {
		t.Fatalf("search semantic resolution after candidate indexing = %#v", searchPage.Resolved.Semantic)
	}
	capabilities := runtime.SearchCapabilities()
	if capabilities.Semantic == nil ||
		capabilities.Semantic.ModelID != "timich-multilingual-clip-small" ||
		capabilities.Semantic.ProfileKind != "modelPack" ||
		capabilities.Semantic.InputKind != "image" ||
		capabilities.Semantic.IndexedVectorCount != 1 {
		t.Fatalf("search capabilities after candidate indexing = %#v", capabilities.Semantic)
	}

	incompleteActivateByIDRecorder := httptest.NewRecorder()
	handler.ServeHTTP(incompleteActivateByIDRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/activate", bytes.NewReader([]byte(`{
		"modelId": "timich-multilingual-clip-small",
		"vectorSpaceId": "timich-multilingual-clip-small/2026.06/d4"
	}`))))
	if incompleteActivateByIDRecorder.Code != http.StatusConflict {
		t.Fatalf("incomplete activate-by-id status = %d, want 409 body=%s", incompleteActivateByIDRecorder.Code, incompleteActivateByIDRecorder.Body.String())
	}

	indexingUninstallRecorder := httptest.NewRecorder()
	handler.ServeHTTP(indexingUninstallRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/uninstall", bytes.NewReader([]byte(`{
		"modelId": "timich-multilingual-clip-small",
		"vectorSpaceId": "timich-multilingual-clip-small/2026.06/d4"
	}`))))
	if indexingUninstallRecorder.Code != http.StatusConflict {
		t.Fatalf("indexing uninstall status = %d, want 409 body=%s", indexingUninstallRecorder.Code, indexingUninstallRecorder.Body.String())
	}
	var indexingUninstallError struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(indexingUninstallRecorder.Body.Bytes(), &indexingUninstallError); err != nil {
		t.Fatalf("indexing uninstall response is not JSON: %v body=%s", err, indexingUninstallRecorder.Body.String())
	}
	if indexingUninstallError.Error != "semantic_model_indexing" {
		t.Fatalf("indexing uninstall error = %#v, want semantic_model_indexing", indexingUninstallError)
	}

	completeIndexingRecorder := httptest.NewRecorder()
	handler.ServeHTTP(completeIndexingRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-indexing/run", bytes.NewReader([]byte(`{"maxAssets":10}`))))
	if completeIndexingRecorder.Code != http.StatusOK {
		t.Fatalf("complete indexing status = %d, want 200 body=%s", completeIndexingRecorder.Code, completeIndexingRecorder.Body.String())
	}
	completePublishRecorder := httptest.NewRecorder()
	handler.ServeHTTP(completePublishRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-indexing/run", bytes.NewReader([]byte(`{"maxAssets":0}`))))
	if completePublishRecorder.Code != http.StatusAccepted {
		t.Fatalf("complete publish status = %d, want 202 body=%s", completePublishRecorder.Code, completePublishRecorder.Body.String())
	}
	waitSemanticIndexPublished(t, runtime, 2)

	activateRecorder := httptest.NewRecorder()
	handler.ServeHTTP(activateRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/activate", bytes.NewReader([]byte(`{
		"modelId": "timich-multilingual-clip-small",
		"vectorSpaceId": "timich-multilingual-clip-small/2026.06/d4"
	}`))))
	if activateRecorder.Code != http.StatusOK {
		t.Fatalf("activate status = %d, want 200 body=%s", activateRecorder.Code, activateRecorder.Body.String())
	}
	var activation struct {
		Status        string    `json:"status"`
		ModelID       string    `json:"modelId"`
		VectorSpaceID string    `json:"vectorSpaceId"`
		ActivatedAt   time.Time `json:"activatedAt"`
		Profile       struct {
			ModelID     string `json:"modelId"`
			Role        string `json:"role"`
			ProfileKind string `json:"profileKind"`
			InputKind   string `json:"inputKind"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(activateRecorder.Body.Bytes(), &activation); err != nil {
		t.Fatalf("activation response is not JSON: %v body=%s", err, activateRecorder.Body.String())
	}
	if activation.Status != "activated" ||
		activation.ModelID != "timich-multilingual-clip-small" ||
		activation.Profile.Role != "active" ||
		activation.Profile.ProfileKind != "modelPack" ||
		activation.Profile.InputKind != "image" ||
		activation.ActivatedAt.IsZero() {
		t.Fatalf("activation payload = %#v", activation)
	}

	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("semantic status = %d, want 200 body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var activeStatus struct {
		Active struct {
			ModelID     string `json:"modelId"`
			Role        string `json:"role"`
			ProfileKind string `json:"profileKind"`
			InputKind   string `json:"inputKind"`
		} `json:"active"`
	}
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &activeStatus); err != nil {
		t.Fatalf("semantic status response is not JSON: %v body=%s", err, statusRecorder.Body.String())
	}
	if activeStatus.Active.ModelID != "timich-multilingual-clip-small" ||
		activeStatus.Active.Role != "active" ||
		activeStatus.Active.ProfileKind != "modelPack" ||
		activeStatus.Active.InputKind != "image" {
		t.Fatalf("active semantic status = %#v", activeStatus.Active)
	}
}

func TestSemanticModelsSearchEnableInstallsIndexesAndSchedules(t *testing.T) {
	t.Parallel()

	artifact := semanticModelZipArtifactWithDim(t, 4)
	sum := sha256.Sum256(artifact)
	artifactSHA := hex.EncodeToString(sum[:])
	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			writeJSON(w, http.StatusOK, map[string]any{
				"schemaVersion": 1,
				"product":       "timich-semantic-models",
				"recommended":   "timich-multilingual-clip-small",
				"models": []map[string]any{{
					"id":            "timich-multilingual-clip-small",
					"name":          "Timich Multilingual CLIP Small",
					"version":       "2026.06.01",
					"vectorSpaceId": "timich-multilingual-clip-small/2026.06/d4",
					"embeddingDim":  4,
					"inputKind":     "image",
					"runtime":       "onnxruntime",
					"artifacts": map[string]any{
						"default": map[string]any{
							"filename":  "timich-multilingual-clip-small.zip",
							"url":       "http://" + r.Host + "/model.zip",
							"sha256":    artifactSHA,
							"sizeBytes": len(artifact),
						},
					},
				}},
			})
		case "/model.zip":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(artifact)
		case "/api/search/metadata":
			if r.Header.Get("x-api-key") != "immich-api-key" {
				t.Fatalf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"assets": {
					"total": 1,
					"items": [
						{
							"id": "asset-search-enable",
							"type": "IMAGE",
							"originalFileName": "search-enable.jpg",
							"fileCreatedAt": "2026-06-01T10:00:00Z",
							"updatedAt": "2026-06-01T10:05:00Z"
						}
					],
					"nextPage": null
				}
			}`))
		case "/api/assets/asset-search-enable/thumbnail":
			if r.Header.Get("x-api-key") != "immich-api-key" {
				t.Fatalf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
			}
			if r.URL.Query().Get("size") != "preview" {
				t.Fatalf("thumbnail size = %q, want preview", r.URL.Query().Get("size"))
			}
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(semanticModelTestJPEG(t))
		default:
			http.NotFound(w, r)
		}
	}))
	defer datasourceServer.Close()

	helperPath := semanticRuntimeEmbeddingHelperScript(t)
	datasource := config.DatasourceConfig{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        "immich_indexed",
		URL:         datasourceServer.URL,
		AccessToken: "immich-api-key",
		Indexing: &config.DatasourceIndexingConfig{
			LatestAssetLimit: 1,
		},
	}
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.HelperPath = helperPath
		cfg.Datasources = []config.DatasourceConfig{datasource}
	})
	defer runtime.Close()
	persistedConfig := config.Default()
	persistedConfig.DataDir = runtime.ConfigResponse().DataDir
	persistedConfig.SemanticRuntime.HelperPath = helperPath
	persistedConfig.Datasources = []config.DatasourceConfig{datasource}
	if err := config.WriteFile(runtime.ConfigResponse().ConfigPath, persistedConfig); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	handler := NewMuxWithOptions(runtime, Options{
		SemanticModelManifestURL: datasourceServer.URL + "/manifest.json",
	})

	syncRecorder := httptest.NewRecorder()
	handler.ServeHTTP(syncRecorder, authenticatedRequest(http.MethodPost, "/v1/datasources/indexing/run", bytes.NewReader([]byte(`{"mode":"full"}`))))
	if syncRecorder.Code != http.StatusOK {
		t.Fatalf("sync status = %d, want 200 body=%s", syncRecorder.Code, syncRecorder.Body.String())
	}

	enableRecorder := httptest.NewRecorder()
	handler.ServeHTTP(enableRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/search/enable", bytes.NewReader([]byte(`{
		"interval": "45s",
		"batchSize": 7,
		"targetCompletedVectors": 10000,
		"initialIndexingAssets": 1
	}`))))
	if enableRecorder.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200 body=%s", enableRecorder.Code, enableRecorder.Body.String())
	}
	var payload struct {
		Status               string                                `json:"status"`
		InstalledRecommended bool                                  `json:"installedRecommended"`
		Indexing             *catalog.SemanticBackfillResult       `json:"indexing"`
		IndexingWorker       *catalog.SemanticIndexingWorkerStatus `json:"indexingWorker"`
		Semantic             catalog.SemanticModelRegistryStatus   `json:"semantic"`
	}
	if err := json.Unmarshal(enableRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("enable response is not JSON: %v body=%s", err, enableRecorder.Body.String())
	}
	if payload.Status != "enabled" || !payload.InstalledRecommended {
		t.Fatalf("enable payload status = %#v, want installed enabled response", payload)
	}
	if payload.Indexing == nil || payload.Indexing.ProcessedVectorCount != 1 || payload.Indexing.IndexedVectorCount != 0 {
		t.Fatalf("enable indexing = %+v, want one completed candidate vector awaiting index publish", payload.Indexing)
	}
	if payload.IndexingWorker == nil ||
		!payload.IndexingWorker.Enabled ||
		payload.IndexingWorker.Status != "scheduled" ||
		payload.IndexingWorker.IntervalSeconds != 45 ||
		payload.IndexingWorker.BatchSize != 7 ||
		payload.IndexingWorker.TargetCompletedVectors != 10000 {
		t.Fatalf("IndexingWorker = %+v, want scheduled 45s worker", payload.IndexingWorker)
	}
	if payload.Semantic.Candidate == nil ||
		payload.Semantic.Candidate.ModelID != "timich-multilingual-clip-small" {
		t.Fatalf("semantic status = %#v, want installed candidate", payload.Semantic)
	}

	publishRecorder := httptest.NewRecorder()
	handler.ServeHTTP(publishRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-indexing/run", bytes.NewReader([]byte(`{"maxAssets":0}`))))
	if publishRecorder.Code != http.StatusAccepted {
		t.Fatalf("publish status = %d, want 202 body=%s", publishRecorder.Code, publishRecorder.Body.String())
	}
	waitSemanticIndexPublished(t, runtime, 1)

	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	indexing := loaded.SemanticRuntime.Indexing
	if !indexing.Enabled || indexing.Interval != "45s" || indexing.BatchSize != 7 || indexing.TargetCompletedVectors != 10000 {
		t.Fatalf("persisted Indexing = %+v, want enabled settings", indexing)
	}
	if loaded.Datasources[0].AccessToken != "immich-api-key" {
		t.Fatalf("persisted datasource token = %q, want preserved token", loaded.Datasources[0].AccessToken)
	}

	searchPage, err := runtime.SearchAssets(catalog.AssetSearchRequest{
		Collection: catalog.AssetCollectionRequest{
			Kind: catalog.CollectionKindSearch,
			Query: &catalog.AssetSearchQuery{
				Text: "beach",
				Mode: catalog.QueryModeSemantic,
			},
		},
		Page: catalog.AssetSearchPageRequest{Index: 0, Size: 10},
	})
	if err != nil {
		t.Fatalf("SearchAssets() after enable error = %v", err)
	}
	if len(searchPage.Items) != 1 || searchPage.Total != 1 {
		t.Fatalf("search page after enable = %#v", searchPage)
	}
}

func TestSemanticModelsSearchEnableCanSkipInitialIndexing(t *testing.T) {
	t.Parallel()

	artifact := semanticModelZipArtifactWithDim(t, 4)
	sum := sha256.Sum256(artifact)
	artifactSHA := hex.EncodeToString(sum[:])
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			writeJSON(w, http.StatusOK, map[string]any{
				"schemaVersion": 1,
				"product":       "timich-semantic-models",
				"recommended":   "timich-multilingual-clip-small",
				"models": []map[string]any{{
					"id":            "timich-multilingual-clip-small",
					"name":          "Timich Multilingual CLIP Small",
					"version":       "2026.06.01",
					"vectorSpaceId": "timich-multilingual-clip-small/2026.06/d4",
					"embeddingDim":  4,
					"inputKind":     "image",
					"runtime":       "onnxruntime",
					"artifacts": map[string]any{
						"default": map[string]any{
							"filename":  "timich-multilingual-clip-small.zip",
							"url":       "http://" + r.Host + "/model.zip",
							"sha256":    artifactSHA,
							"sizeBytes": len(artifact),
						},
					},
				}},
			})
		case "/model.zip":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	defer manifestServer.Close()

	helperPath := semanticRuntimeEmbeddingHelperScript(t)
	datasource := config.DatasourceConfig{
		SourceKey:   "1111111111111111",
		Name:        "Home Immich",
		Kind:        "immich_indexed",
		URL:         "http://immich.local:2283",
		AccessToken: "immich-api-key",
		Indexing:    &config.DatasourceIndexingConfig{},
	}
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.SemanticRuntime.HelperPath = helperPath
		cfg.Datasources = []config.DatasourceConfig{datasource}
	})
	defer runtime.Close()
	persistedConfig := config.Default()
	persistedConfig.DataDir = runtime.ConfigResponse().DataDir
	persistedConfig.SemanticRuntime.HelperPath = helperPath
	persistedConfig.Datasources = []config.DatasourceConfig{datasource}
	if err := config.WriteFile(runtime.ConfigResponse().ConfigPath, persistedConfig); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	handler := NewMuxWithOptions(runtime, Options{
		SemanticModelManifestURL: manifestServer.URL + "/manifest.json",
	})

	enableRecorder := httptest.NewRecorder()
	handler.ServeHTTP(enableRecorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/search/enable", bytes.NewReader([]byte(`{
		"interval": "45s",
		"batchSize": 7,
		"targetCompletedVectors": 10000,
		"initialIndexingAssets": 0
	}`))))
	if enableRecorder.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200 body=%s", enableRecorder.Code, enableRecorder.Body.String())
	}
	var payload struct {
		Status               string                                `json:"status"`
		InstalledRecommended bool                                  `json:"installedRecommended"`
		Indexing             *catalog.SemanticBackfillResult       `json:"indexing"`
		IndexingWorker       *catalog.SemanticIndexingWorkerStatus `json:"indexingWorker"`
	}
	if err := json.Unmarshal(enableRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("enable response is not JSON: %v body=%s", err, enableRecorder.Body.String())
	}
	if payload.Status != "enabled" || !payload.InstalledRecommended {
		t.Fatalf("enable payload status = %#v, want installed enabled response", payload)
	}
	if payload.Indexing != nil {
		t.Fatalf("enable indexing = %+v, want skipped initial indexing", payload.Indexing)
	}
	if payload.IndexingWorker == nil ||
		!payload.IndexingWorker.Enabled ||
		payload.IndexingWorker.Status != "scheduled" ||
		payload.IndexingWorker.IntervalSeconds != 45 ||
		payload.IndexingWorker.BatchSize != 7 ||
		payload.IndexingWorker.TargetCompletedVectors != 10000 {
		t.Fatalf("IndexingWorker = %+v, want scheduled 45s worker", payload.IndexingWorker)
	}

	loaded, err := config.Load(runtime.ConfigResponse().ConfigPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	indexing := loaded.SemanticRuntime.Indexing
	if !indexing.Enabled || indexing.Interval != "45s" || indexing.BatchSize != 7 || indexing.TargetCompletedVectors != 10000 {
		t.Fatalf("persisted Indexing = %+v, want enabled settings", indexing)
	}
}

func semanticRuntimeHelperScript(t *testing.T) string {
	t.Helper()

	helperPath := filepath.Join(t.TempDir(), "timich-semantic-helper")
	script := `#!/bin/sh
if [ "$1" != "inspect" ]; then
  exit 2
fi
printf '%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"timich-multilingual-clip-small","vectorSpaceId":"timich-multilingual-clip-small/2026.06/d512","embeddingDim":512,"inputKind":"image","loaded":true,"canEmbed":true}'
`
	if err := os.WriteFile(helperPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write semantic runtime helper: %v", err)
	}
	return helperPath
}

func semanticModelZipArtifact(t *testing.T) []byte {
	t.Helper()

	return semanticModelZipArtifactWithDim(t, 512)
}

func semanticModelArchivePackForTest(id string, vectorSpaceID string, version string, artifact []byte) catalog.SemanticModelPackStatus {
	sum := sha256.Sum256(artifact)
	return catalog.SemanticModelPackStatus{
		ID:            id,
		Version:       version,
		VectorSpaceID: vectorSpaceID,
		EmbeddingDim:  4,
		InputKind:     "image",
		Runtime:       "onnxruntime",
		Artifact: &catalog.SemanticModelArtifactStatus{
			Filename:  id + ".zip",
			SHA256:    hex.EncodeToString(sum[:]),
			SizeBytes: int64(len(artifact)),
		},
	}
}

func semanticRuntimePackZipArtifact(t *testing.T) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	writeZipEntry := func(name string, payload []byte, mode os.FileMode) {
		t.Helper()
		header := &zip.FileHeader{
			Name:   name,
			Method: zip.Deflate,
		}
		header.SetMode(mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("create zip entry %q: %v", name, err)
		}
		if _, err := entry.Write(payload); err != nil {
			t.Fatalf("write zip entry %q: %v", name, err)
		}
	}
	layout := map[string]any{
		"schemaVersion": 1,
		"product":       "timich-semantic-runtime-pack",
		"runtime":       "onnxruntime",
		"serverPath":    "semantic-runtime/siglip2-onnx/server.py",
		"pythonPath":    ".venv/bin/python",
	}
	rawLayout, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("marshal semantic runtime layout: %v", err)
	}
	writeZipEntry("timich-runtime.json", rawLayout, 0o600)
	writeZipEntry("semantic-runtime/siglip2-onnx/server.py", []byte("#!/usr/bin/env python3\n"), 0o700)
	writeZipEntry(".venv/bin/python", []byte("#!/bin/sh\n"), 0o700)
	if err := writer.Close(); err != nil {
		t.Fatalf("close semantic runtime zip: %v", err)
	}
	return buffer.Bytes()
}

func semanticRuntimeEmbeddingHelperScript(t *testing.T) string {
	t.Helper()

	helperPath := filepath.Join(t.TempDir(), "timich-semantic-helper")
	script := `#!/bin/sh
case "$1" in
  inspect)
    printf '%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"timich-multilingual-clip-small","vectorSpaceId":"timich-multilingual-clip-small/2026.06/d4","embeddingDim":4,"inputKind":"image","loaded":true,"canEmbed":true}'
    ;;
  embed-image)
    cat >/dev/null
    printf '%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"timich-multilingual-clip-small","vectorSpaceId":"timich-multilingual-clip-small/2026.06/d4","embeddingDim":4,"inputKind":"image","vector":[1,0,0,0],"input":"test-preview"}'
    ;;
  embed-text)
    printf '%s\n' '{"protocolVersion":1,"runtime":"onnxruntime","modelId":"timich-multilingual-clip-small","vectorSpaceId":"timich-multilingual-clip-small/2026.06/d4","embeddingDim":4,"inputKind":"image","vector":[1,0,0,0],"input":"test-query"}'
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(helperPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write semantic runtime embedding helper: %v", err)
	}
	return helperPath
}

func semanticModelZipArtifactWithDim(t *testing.T, dim int) []byte {
	t.Helper()
	return semanticModelZipArtifactForIdentity(
		t,
		"timich-multilingual-clip-small",
		"timich-multilingual-clip-small/2026.06/d"+strconv.Itoa(dim),
		dim,
	)
}

func semanticModelZipArtifactForIdentity(t *testing.T, modelID string, vectorSpaceID string, dim int) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	writeZipEntry := func(name string, payload []byte) {
		t.Helper()
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %q: %v", name, err)
		}
		if _, err := entry.Write(payload); err != nil {
			t.Fatalf("write zip entry %q: %v", name, err)
		}
	}
	layout := map[string]any{
		"schemaVersion": 1,
		"product":       "timich-semantic-model-pack",
		"modelId":       modelID,
		"vectorSpaceId": vectorSpaceID,
		"embeddingDim":  dim,
		"inputKind":     "image",
		"runtime":       "onnxruntime",
		"imageModel":    "models/image.onnx",
		"textModel":     "models/text.onnx",
		"tokenizer":     "tokenizer/tokenizer.json",
	}
	rawLayout, err := json.Marshal(layout)
	if err != nil {
		t.Fatalf("marshal semantic model layout: %v", err)
	}
	writeZipEntry("timich-model.json", rawLayout)
	writeZipEntry("models/image.onnx", []byte("test image onnx"))
	writeZipEntry("models/text.onnx", []byte("test text onnx"))
	writeZipEntry("tokenizer/tokenizer.json", []byte(`{"model":"test"}`))
	if err := writer.Close(); err != nil {
		t.Fatalf("close semantic model zip: %v", err)
	}
	return buffer.Bytes()
}

func semanticModelTestJPEG(t *testing.T) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			img.Set(x, y, color.RGBA{R: uint8(16 * x), G: uint8(16 * y), B: 128, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("encode semantic model test jpeg: %v", err)
	}
	return buffer.Bytes()
}

func TestSemanticModelsRejectsRecommendedModelChecksumMismatch(t *testing.T) {
	t.Parallel()

	artifact := []byte("test semantic model artifact")
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			writeJSON(w, http.StatusOK, map[string]any{
				"schemaVersion": 1,
				"product":       "timich-semantic-models",
				"models": []map[string]any{{
					"id":            "timich-multilingual-clip-small",
					"name":          "Timich Multilingual CLIP Small",
					"version":       "2026.06.01",
					"vectorSpaceId": "timich-multilingual-clip-small/2026.06/d512",
					"embeddingDim":  512,
					"inputKind":     "image",
					"artifacts": map[string]any{
						"default": map[string]any{
							"filename":  "timich-multilingual-clip-small.tar.zst",
							"url":       "http://" + r.Host + "/model.tar.zst",
							"sha256":    strings.Repeat("d", 64),
							"sizeBytes": len(artifact),
						},
					},
				}},
			})
		case "/model.tar.zst":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(artifact)
		default:
			http.NotFound(w, r)
		}
	}))
	defer manifestServer.Close()

	handler := NewMuxWithOptions(newTestRuntime(t, 5), Options{
		SemanticModelManifestURL: manifestServer.URL + "/manifest.json",
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/semantic-models/recommended/install", nil))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("install status = %d, want 202 body=%s", recorder.Code, recorder.Body.String())
	}
	job := waitSemanticInstallJob(t, handler)
	if job.Status != semanticInstallJobStatusFailed || job.ErrorCode != "semantic_model_checksum_mismatch" {
		t.Fatalf("semantic install job = %+v, want checksum mismatch failure", job)
	}
}

func TestSemanticModelsRejectsUnsafeRecommendedManifest(t *testing.T) {
	t.Parallel()

	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"schemaVersion": 1,
			"product":       "timich-semantic-models",
			"models": []map[string]any{{
				"id":            "bad-model",
				"name":          "Bad Model",
				"version":       "1.0.0",
				"vectorSpaceId": "bad/d4",
				"embeddingDim":  4,
				"inputKind":     "image",
				"artifacts": map[string]any{
					"default": map[string]string{
						"filename": "bad.tar.zst",
						"url":      "javascript:alert(1)",
						"sha256":   strings.Repeat("c", 64),
					},
				},
			}},
		})
	}))
	defer manifestServer.Close()

	handler := NewMuxWithOptions(newTestRuntime(t, 5), Options{
		SemanticModelManifestURL: manifestServer.URL,
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/semantic-models", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		RegistryStatus  string `json:"registryStatus"`
		RegistryMessage string `json:"registryMessage"`
		Recommended     any    `json:"recommended"`
		Profiles        []any  `json:"profiles"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.RegistryStatus != "unavailable" {
		t.Fatalf("registryStatus = %q, want unavailable", payload.RegistryStatus)
	}
	if !strings.Contains(payload.RegistryMessage, "artifact URL") {
		t.Fatalf("registryMessage = %q, want artifact URL explanation", payload.RegistryMessage)
	}
	if payload.Recommended != nil || len(payload.Profiles) != 0 {
		t.Fatalf("payload = %#v, want no synthetic profile when the manifest is unsafe", payload)
	}
}

func TestLoginSetsAdminCookie(t *testing.T) {
	t.Parallel()

	adminToken := `unsafe;admin token"with\slash`
	form := url.Values{"token": {adminToken}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler := NewMux(newTestRuntimeWithAdminToken(t, 5, adminToken))

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 body=%s", recorder.Code, recorder.Body.String())
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != adminCookieName {
		t.Fatalf("cookies = %#v, want admin cookie", cookies)
	}
	if cookies[0].Value == adminToken || strings.ContainsAny(cookies[0].Value, " ;\"\\,") {
		t.Fatalf("cookie value = %q, want encoded cookie-safe token", cookies[0].Value)
	}
	if got := decodeAdminCookieValue(cookies[0].Value); got != adminToken {
		t.Fatalf("decoded cookie = %q, want %q", got, adminToken)
	}

	statusRequest := httptest.NewRequest(http.MethodGet, "/status", nil)
	statusRequest.AddCookie(cookies[0])
	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, statusRequest)
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("encoded cookie auth status = %d, want 200 body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
}

func TestSetupAdminTokenPersistsTokenAndSetsCookie(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(
		http.MethodPost,
		"/setup-admin-token",
		strings.NewReader("token=created-admin-token&confirmToken=created-admin-token"),
	)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	runtime := newTestRuntimeWithoutAdminToken(t)

	NewMux(runtime).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 body=%s", recorder.Code, recorder.Body.String())
	}
	if !runtime.AuthenticateAdminToken("created-admin-token") {
		t.Fatal("created admin token was not accepted")
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != adminCookieName {
		t.Fatalf("cookies = %#v, want admin cookie", cookies)
	}
	if got := decodeAdminCookieValue(cookies[0].Value); got != "created-admin-token" {
		t.Fatalf("decoded cookie = %q, want created admin token", got)
	}
}

func TestSetupAdminTokenRejectsShortToken(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodPost, "/setup-admin-token", strings.NewReader("token=short&confirmToken=short"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()

	NewMux(newTestRuntimeWithoutAdminToken(t)).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("at least 16 characters")) {
		t.Fatalf("short token response = %s", recorder.Body.String())
	}
}

func TestPrimaryDatasourceGetAndUpdate(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, 5)
	handler := NewMux(runtime)

	initialRecorder := httptest.NewRecorder()
	handler.ServeHTTP(initialRecorder, authenticatedRequest(http.MethodGet, "/v1/datasource/primary", nil))
	if initialRecorder.Code != http.StatusOK {
		t.Fatalf("initial status = %d, want 200 body=%s", initialRecorder.Code, initialRecorder.Body.String())
	}

	updateBody := bytes.NewReader([]byte(`{"name":"Home Immich","kind":"immich_indexed","url":"http://immich.local:2283","accessToken":"immich-api-key"}`))
	updateRecorder := httptest.NewRecorder()
	handler.ServeHTTP(updateRecorder, authenticatedRequest(http.MethodPut, "/v1/datasource/primary", updateBody))
	if updateRecorder.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 body=%s", updateRecorder.Code, updateRecorder.Body.String())
	}
	var payload struct {
		Configured     bool   `json:"configured"`
		Name           string `json:"name"`
		URL            string `json:"url"`
		HasAccessToken bool   `json:"hasAccessToken"`
	}
	if err := json.Unmarshal(updateRecorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("update response is not JSON: %v body=%s", err, updateRecorder.Body.String())
	}
	if !payload.Configured || payload.Name != "Home Immich" || payload.URL != "http://immich.local:2283" || !payload.HasAccessToken {
		t.Fatalf("datasource payload = %#v", payload)
	}
}

func TestDatasourcesListAndAdd(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, 5)
	handler := NewMux(runtime)

	initialRecorder := httptest.NewRecorder()
	handler.ServeHTTP(initialRecorder, authenticatedRequest(http.MethodGet, "/v1/datasources", nil))
	if initialRecorder.Code != http.StatusOK {
		t.Fatalf("initial status = %d, want 200 body=%s", initialRecorder.Code, initialRecorder.Body.String())
	}
	if strings.TrimSpace(initialRecorder.Body.String()) != "[]" {
		t.Fatalf("initial datasources = %s, want []", initialRecorder.Body.String())
	}

	addBody := bytes.NewReader([]byte(`{"name":"Home Immich","kind":"immich_indexed","url":"http://immich.local:2283","accessToken":"immich-api-key"}`))
	addRecorder := httptest.NewRecorder()
	handler.ServeHTTP(addRecorder, authenticatedRequest(http.MethodPost, "/v1/datasources", addBody))
	if addRecorder.Code != http.StatusCreated {
		t.Fatalf("add status = %d, want 201 body=%s", addRecorder.Code, addRecorder.Body.String())
	}
	var added runtimestate.DatasourceSummary
	if err := json.Unmarshal(addRecorder.Body.Bytes(), &added); err != nil {
		t.Fatalf("add response is not JSON: %v body=%s", err, addRecorder.Body.String())
	}
	if added.SourceKey == "" || added.Name != "Home Immich" || added.Kind != config.DatasourceKindImmichIndexed || added.URL != "http://immich.local:2283" || !added.HasAccessToken || !added.IndexingEnabled {
		t.Fatalf("added datasource = %+v", added)
	}

	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, authenticatedRequest(http.MethodGet, "/v1/datasources", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var listed []runtimestate.DatasourceSummary
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list response is not JSON: %v body=%s", err, listRecorder.Body.String())
	}
	if len(listed) != 1 || listed[0].SourceKey != added.SourceKey {
		t.Fatalf("listed datasources = %+v, want added datasource", listed)
	}

	duplicateRecorder := httptest.NewRecorder()
	handler.ServeHTTP(duplicateRecorder, authenticatedRequest(http.MethodPost, "/v1/datasources", bytes.NewReader([]byte(`{"name":"Duplicate","kind":"immich_indexed","url":"http://immich.local:2283","accessToken":"immich-api-key"}`))))
	assertErrorPayload(t, duplicateRecorder, http.StatusConflict, "datasource_already_configured")
}

func TestDatasourcesRejectAdditionalSourceAfterImmichPassthrough(t *testing.T) {
	t.Parallel()

	handler := NewMux(newTestRuntime(t, 5))
	passthroughRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		passthroughRecorder,
		authenticatedRequest(http.MethodPost, "/v1/datasources", bytes.NewReader([]byte(`{"name":"Home Immich","kind":"immich","url":"http://immich.local:2283","accessToken":"immich-api-key"}`))),
	)
	if passthroughRecorder.Code != http.StatusCreated {
		t.Fatalf("passthrough add status = %d, want 201 body=%s", passthroughRecorder.Code, passthroughRecorder.Body.String())
	}

	additionalRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		additionalRecorder,
		authenticatedRequest(http.MethodPost, "/v1/datasources", bytes.NewReader([]byte(`{"name":"Other Immich","kind":"immich_indexed","url":"http://other-immich.local:2283","accessToken":"other-key"}`))),
	)
	assertErrorPayload(t, additionalRecorder, http.StatusConflict, "immich_passthrough_requires_single_datasource")
}

func TestDatasourcesAddLocalRequiresConfiguredRoot(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(
		recorder,
		authenticatedRequest(http.MethodPost, "/v1/datasources", bytes.NewReader([]byte(`{"name":"NAS","kind":"local_filesystem","rootKey":"nas-photos"}`))),
	)

	assertErrorPayload(t, recorder, http.StatusBadRequest, "local_media_root_required")
}

func TestLocalDatasourceImmichFallbackUpdateRejectsUnknownSource(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(
		recorder,
		authenticatedRequest(http.MethodPut, "/v1/datasources/local/immich-fallback", bytes.NewReader([]byte(`{"sourceKey":"1111111111111111","enabled":false}`))),
	)

	assertErrorPayload(t, recorder, http.StatusNotFound, "datasource_not_found")
}

func TestPrimaryDatasourceCheckReturnsDatasourceStatus(t *testing.T) {
	t.Parallel()

	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search/metadata" {
			t.Fatalf("unexpected datasource path %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "immich-api-key" {
			t.Fatalf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assets":{"items":[],"total":0,"nextPage":null}}`))
	}))
	defer datasourceServer.Close()

	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.Datasources = []config.DatasourceConfig{{
			Name:        "Home Immich",
			Kind:        "immich_indexed",
			URL:         datasourceServer.URL,
			AccessToken: "immich-api-key",
		}}
	})
	recorder := httptest.NewRecorder()
	NewMux(runtime).ServeHTTP(
		recorder,
		authenticatedRequest(http.MethodPost, "/v1/datasource/primary/check", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.Name != "datasource" || payload.Status != "ok" {
		t.Fatalf("check payload = %#v, want datasource ok", payload)
	}
	if !strings.Contains(payload.Summary, "metadata request") {
		t.Fatalf("summary = %q, want metadata request detail", payload.Summary)
	}
}

func TestPrimaryDatasourceCheckAllowsLocalDatasourceWithoutProbe(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.Datasources = []config.DatasourceConfig{{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
		}}
	})
	recorder := httptest.NewRecorder()
	NewMux(runtime).ServeHTTP(
		recorder,
		authenticatedRequest(http.MethodPost, "/v1/datasource/primary/check", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Name    string         `json:"name"`
		Status  string         `json:"status"`
		Summary string         `json:"summary"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.Name != "datasource" || payload.Status != "ok" {
		t.Fatalf("check payload = %#v, want datasource ok", payload)
	}
	if !strings.Contains(payload.Summary, "No upstream HTTP") {
		t.Fatalf("summary = %q, want local datasource check detail", payload.Summary)
	}
	if payload.Details["datasourceKind"] != config.DatasourceKindLocalFiles || payload.Details["rootKey"] != "nas-photos" {
		t.Fatalf("details = %#v, want local datasource kind/root", payload.Details)
	}
}

func TestPrimaryDatasourceCheckReportsProbeFailure(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.Datasources = []config.DatasourceConfig{{
			Name:        "Home Immich",
			Kind:        "immich_indexed",
			URL:         "http://127.0.0.1:1",
			AccessToken: "immich-api-key",
		}}
	})
	recorder := httptest.NewRecorder()
	NewMux(runtime).ServeHTTP(
		recorder,
		authenticatedRequest(http.MethodPost, "/v1/datasource/primary/check", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Name        string         `json:"name"`
		Status      string         `json:"status"`
		Remediation string         `json:"remediation"`
		Details     map[string]any `json:"details"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.Name != "datasource" || payload.Status != "failed" {
		t.Fatalf("check payload = %#v, want datasource failed", payload)
	}
	if !strings.Contains(payload.Remediation, "agent runtime") {
		t.Fatalf("remediation = %q, want agent runtime hint", payload.Remediation)
	}
	if payload.Details["datasourceURL"] != "http://127.0.0.1:1" {
		t.Fatalf("details = %#v, want datasource URL", payload.Details)
	}
}

func TestDatasourceIndexingRunReturnsRemoteCounts(t *testing.T) {
	t.Parallel()

	datasourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search/metadata" {
			t.Fatalf("unexpected datasource path %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "immich-api-key" {
			t.Fatalf("x-api-key = %q, want configured key", r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"assets": {
				"total": 2,
				"items": [
					{
						"id": "asset-new",
						"type": "IMAGE",
						"originalFileName": "new.jpg",
						"fileCreatedAt": "2026-06-01T10:00:00Z",
						"updatedAt": "2026-06-01T10:05:00Z"
					},
					{
						"id": "asset-old",
						"type": "IMAGE",
						"originalFileName": "old.jpg",
						"fileCreatedAt": "2025-06-01T10:00:00Z",
						"updatedAt": "2025-06-01T10:05:00Z"
					}
				],
				"nextPage": null
			}
		}`))
	}))
	defer datasourceServer.Close()

	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.Datasources = []config.DatasourceConfig{{
			SourceKey:   "1111111111111111",
			Name:        "Home Immich",
			Kind:        "immich_indexed",
			URL:         datasourceServer.URL,
			AccessToken: "immich-api-key",
			Indexing: &config.DatasourceIndexingConfig{
				LatestAssetLimit: 1,
			},
		}}
	})
	defer runtime.Close()
	handler := NewMux(runtime)

	syncRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		syncRecorder,
		authenticatedRequest(http.MethodPost, "/v1/datasources/indexing/run", bytes.NewReader([]byte(`{"mode":"full"}`))),
	)
	if syncRecorder.Code != http.StatusOK {
		t.Fatalf("sync status = %d, want 200 body=%s", syncRecorder.Code, syncRecorder.Body.String())
	}
	var syncPayload struct {
		Results []struct {
			Mode             string `json:"mode"`
			Status           string `json:"status"`
			FetchedAssets    int    `json:"fetchedAssets"`
			ActiveAssets     int    `json:"activeAssets"`
			OutOfScopeAssets int    `json:"outOfScopeAssets"`
		} `json:"results"`
	}
	if err := json.Unmarshal(syncRecorder.Body.Bytes(), &syncPayload); err != nil {
		t.Fatalf("sync response is not JSON: %v body=%s", err, syncRecorder.Body.String())
	}
	if len(syncPayload.Results) != 1 ||
		syncPayload.Results[0].Mode != "full" ||
		syncPayload.Results[0].FetchedAssets != 1 ||
		syncPayload.Results[0].ActiveAssets != 1 ||
		syncPayload.Results[0].OutOfScopeAssets != 0 {
		t.Fatalf("sync payload = %#v", syncPayload)
	}

	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		statusRecorder,
		authenticatedRequest(http.MethodGet, "/v1/datasources/indexing?refresh=1", nil),
	)
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var statusPayload runtimestate.DatasourceIndexingResponse
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &statusPayload); err != nil {
		t.Fatalf("status response is not JSON: %v body=%s", err, statusRecorder.Body.String())
	}
	if len(statusPayload.Datasources) != 1 ||
		!statusPayload.Datasources[0].IndexingEnabled ||
		statusPayload.Datasources[0].LatestAssetLimit != 1 ||
		statusPayload.Datasources[0].ActiveAssets != 1 {
		t.Fatalf("status payload = %#v", statusPayload)
	}

	catalogRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		catalogRecorder,
		authenticatedRequest(http.MethodGet, "/v1/catalog/dedup/status", nil),
	)
	if catalogRecorder.Code != http.StatusOK {
		t.Fatalf("catalog dedup status = %d, want 200 body=%s", catalogRecorder.Code, catalogRecorder.Body.String())
	}
	var catalogPayload struct {
		SourceRows      int `json:"sourceRows"`
		CanonicalAssets int `json:"canonicalAssets"`
		ActiveAssets    int `json:"activeAssets"`
	}
	if err := json.Unmarshal(catalogRecorder.Body.Bytes(), &catalogPayload); err != nil {
		t.Fatalf("catalog dedup status is not JSON: %v body=%s", err, catalogRecorder.Body.String())
	}
	if catalogPayload.SourceRows != 1 || catalogPayload.CanonicalAssets != 1 || catalogPayload.ActiveAssets != 1 {
		t.Fatalf("catalog dedup status = %#v, want one active canonical asset", catalogPayload)
	}
}

func TestLocalDatasourceScanStatusAndManualPhase0(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), []byte("image"), 0o644); err != nil {
		t.Fatalf("WriteFile(local image) error = %v", err)
	}
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{
			Key:  "nas-photos",
			Path: rootPath,
		}}
		cfg.Datasources = []config.DatasourceConfig{{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
			Scan: &config.LocalDatasourceScanConfig{
				QuickScanInterval: "15m",
			},
		}}
	})
	defer runtime.Close()
	handler := NewMux(runtime)

	beforeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(beforeRecorder, authenticatedRequest(http.MethodGet, "/v1/datasources/local/scan", nil))
	if beforeRecorder.Code != http.StatusOK {
		t.Fatalf("initial status = %d, want 200 body=%s", beforeRecorder.Code, beforeRecorder.Body.String())
	}
	var before runtimestate.LocalDatasourceScanResponse
	if err := json.Unmarshal(beforeRecorder.Body.Bytes(), &before); err != nil {
		t.Fatalf("initial response is not JSON: %v body=%s", err, beforeRecorder.Body.String())
	}
	if len(before.Roots) != 1 || before.Roots[0].Status != "ready" || len(before.Datasources) != 1 {
		t.Fatalf("initial local scan status = %#v", before)
	}
	if before.Datasources[0].RootStatus != "not_scanned" || before.Datasources[0].Phase0Status != "idle" {
		t.Fatalf("initial datasource status = %#v, want not_scanned idle", before.Datasources[0])
	}

	scanRecorder := httptest.NewRecorder()
	handler.ServeHTTP(scanRecorder, authenticatedRequest(http.MethodPost, "/v1/datasources/local/scan", nil))
	if scanRecorder.Code != http.StatusOK {
		t.Fatalf("scan status = %d, want 200 body=%s", scanRecorder.Code, scanRecorder.Body.String())
	}
	var scanResult runtimestate.LocalDatasourcePhase0ScanResponse
	if err := json.Unmarshal(scanRecorder.Body.Bytes(), &scanResult); err != nil {
		t.Fatalf("scan response is not JSON: %v body=%s", err, scanRecorder.Body.String())
	}
	if len(scanResult.Phase0) != 1 ||
		scanResult.Phase0[0].Status != "completed" ||
		scanResult.Phase0[0].DiscoveredPaths != 1 ||
		scanResult.Phase0[0].QueuedMetadata != 1 ||
		scanResult.Metadata.ProcessedJobs != 0 {
		t.Fatalf("scan result = %#v, want one discovered path queued for metadata", scanResult)
	}

	afterRecorder := httptest.NewRecorder()
	handler.ServeHTTP(afterRecorder, authenticatedRequest(http.MethodGet, "/v1/datasources/local/scan", nil))
	if afterRecorder.Code != http.StatusOK {
		t.Fatalf("after status = %d, want 200 body=%s", afterRecorder.Code, afterRecorder.Body.String())
	}
	var after runtimestate.LocalDatasourceScanResponse
	if err := json.Unmarshal(afterRecorder.Body.Bytes(), &after); err != nil {
		t.Fatalf("after response is not JSON: %v body=%s", err, afterRecorder.Body.String())
	}
	got := after.Datasources[0]
	if got.RootStatus != "ready" || got.Phase0Status != "completed" || got.DiscoveredLocations != 1 || got.ActiveLocations != 0 || got.ActiveAssets != 0 || got.QueuedMetadataJobs != 0 || got.SettlingMetadataJobs != 1 {
		t.Fatalf("after datasource status = %#v, want ready completed with one settling metadata job", got)
	}
	if got.LastRun == nil || got.LastRun.Status != "completed" || got.LastRun.QueuedMetadata != 1 {
		t.Fatalf("last run = %#v, want completed run with queued metadata", got.LastRun)
	}

	csvRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		csvRecorder,
		authenticatedRequest(http.MethodGet, "/v1/datasources/local/phase0-diagnostics.csv?sourceKey=1111111111111111", nil),
	)
	if csvRecorder.Code != http.StatusOK {
		t.Fatalf("diagnostic csv status = %d, want 200 body=%s", csvRecorder.Code, csvRecorder.Body.String())
	}
	if contentType := csvRecorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/csv") {
		t.Fatalf("diagnostic csv content-type = %q, want text/csv", contentType)
	}
	csvBody := csvRecorder.Body.String()
	if !strings.Contains(csvBody, "source_key,root_key,scan_run_id") ||
		!strings.Contains(csvBody, "family.jpg") ||
		!strings.Contains(csvBody, "metadata_pending") ||
		!strings.Contains(csvBody, "discovered") ||
		!strings.Contains(csvBody, "ready") {
		t.Fatalf("diagnostic csv body missing expected data: %s", csvBody)
	}

	failureCSVRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		failureCSVRecorder,
		authenticatedRequest(http.MethodGet, "/v1/datasources/local/failure-diagnostics.csv?sourceKey=1111111111111111", nil),
	)
	if failureCSVRecorder.Code != http.StatusOK {
		t.Fatalf("failure diagnostic csv status = %d, want 200 body=%s", failureCSVRecorder.Code, failureCSVRecorder.Body.String())
	}
	if contentType := failureCSVRecorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/csv") {
		t.Fatalf("failure diagnostic csv content-type = %q, want text/csv", contentType)
	}
	if failureCSVBody := failureCSVRecorder.Body.String(); !strings.Contains(failureCSVBody, "source_key,root_key,relative_path,asset_id,media_type,failure_kind,component,status,attempts,last_error,updated_at") {
		t.Fatalf("failure diagnostic csv body missing header: %s", failureCSVBody)
	}

	indexingStatusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(indexingStatusRecorder, authenticatedRequest(http.MethodGet, "/v1/datasources/indexing?refresh=1", nil))
	if indexingStatusRecorder.Code != http.StatusOK {
		t.Fatalf("indexing status = %d, want 200 body=%s", indexingStatusRecorder.Code, indexingStatusRecorder.Body.String())
	}
	var indexingStatus runtimestate.DatasourceIndexingResponse
	if err := json.Unmarshal(indexingStatusRecorder.Body.Bytes(), &indexingStatus); err != nil {
		t.Fatalf("indexing status response is not JSON: %v body=%s", err, indexingStatusRecorder.Body.String())
	}
	if len(indexingStatus.Roots) != 1 || indexingStatus.Roots[0].Status != "ready" {
		t.Fatalf("indexing roots = %#v, want one ready root", indexingStatus.Roots)
	}
	if len(indexingStatus.Datasources) != 1 ||
		indexingStatus.Datasources[0].Kind != config.DatasourceKindLocalFiles ||
		indexingStatus.Datasources[0].IngestionKind != "filesystem" ||
		!indexingStatus.Datasources[0].IndexingEnabled ||
		indexingStatus.Datasources[0].ActiveAssets != 0 ||
		indexingStatus.Datasources[0].QueuedMetadataJobs != 0 ||
		indexingStatus.Datasources[0].SettlingMetadataJobs != 1 {
		t.Fatalf("indexing datasource status = %#v, want one settling filesystem datasource", indexingStatus.Datasources)
	}

	indexingRunRecorder := httptest.NewRecorder()
	handler.ServeHTTP(
		indexingRunRecorder,
		authenticatedRequest(http.MethodPost, "/v1/datasources/indexing/run", bytes.NewReader([]byte(`{"kind":"local_filesystem","sourceKey":"1111111111111111"}`))),
	)
	if indexingRunRecorder.Code != http.StatusOK {
		t.Fatalf("indexing run = %d, want 200 body=%s", indexingRunRecorder.Code, indexingRunRecorder.Body.String())
	}
	var indexingRun runtimestate.DatasourceIndexingRunResponse
	if err := json.Unmarshal(indexingRunRecorder.Body.Bytes(), &indexingRun); err != nil {
		t.Fatalf("indexing run response is not JSON: %v body=%s", err, indexingRunRecorder.Body.String())
	}
	if len(indexingRun.Results) != 1 ||
		indexingRun.Results[0].Kind != config.DatasourceKindLocalFiles ||
		indexingRun.Results[0].IngestionKind != "filesystem" ||
		indexingRun.Results[0].Status != "completed" {
		t.Fatalf("indexing run = %#v, want one completed filesystem result", indexingRun.Results)
	}
}

func TestLocalMediaRootAcceptanceRequiresCurrentObservedIdentityAndRunsFullScan(t *testing.T) {
	parentPath := t.TempDir()
	rootPath := filepath.Join(parentPath, "photos")
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(root) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "family.jpg"), []byte("family"), 0o644); err != nil {
		t.Fatalf("WriteFile(family) error = %v", err)
	}
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.LocalMediaRoots = []config.LocalMediaRootConfig{{Key: "nas-photos", Path: rootPath}}
		cfg.Datasources = []config.DatasourceConfig{{
			SourceKey: "1111111111111111",
			Name:      "NAS Photos",
			Kind:      config.DatasourceKindLocalFiles,
			RootKey:   "nas-photos",
		}}
	})
	defer runtime.Close()
	handler := NewMux(runtime)

	initialScan := httptest.NewRecorder()
	handler.ServeHTTP(initialScan, authenticatedRequest(http.MethodPost, "/v1/datasources/local/scan", nil))
	if initialScan.Code != http.StatusOK {
		t.Fatalf("initial scan = %d body=%s", initialScan.Code, initialScan.Body.String())
	}
	if err := os.Rename(rootPath, filepath.Join(parentPath, "mounted")); err != nil {
		t.Fatalf("Rename(root) error = %v", err)
	}
	if err := os.Mkdir(rootPath, 0o755); err != nil {
		t.Fatalf("Mkdir(replacement) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "placeholder.jpg"), []byte("placeholder"), 0o644); err != nil {
		t.Fatalf("WriteFile(placeholder) error = %v", err)
	}

	statusRecorder := httptest.NewRecorder()
	handler.ServeHTTP(statusRecorder, authenticatedRequest(http.MethodGet, "/status", nil))
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var status runtimestate.StatusResponse
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(status.LocalMediaRoots) != 1 || !status.LocalMediaRoots[0].RootAcceptanceRequired || status.LocalMediaRoots[0].ObservedRootIdentity == "" {
		t.Fatalf("status roots = %+v, want acceptance candidate", status.LocalMediaRoots)
	}
	candidateIdentity := status.LocalMediaRoots[0].ObservedRootIdentity

	staleRecorder := httptest.NewRecorder()
	handler.ServeHTTP(staleRecorder, authenticatedRequest(http.MethodPost, "/v1/datasources/local/root/accept", bytes.NewReader([]byte(`{"sourceKey":"1111111111111111","rootKey":"nas-photos","observedIdentity":"stale"}`))))
	if staleRecorder.Code != http.StatusConflict || !strings.Contains(staleRecorder.Body.String(), "local_root_acceptance_stale") {
		t.Fatalf("stale acceptance = %d body=%s, want 409 stale", staleRecorder.Code, staleRecorder.Body.String())
	}

	requestBody, err := json.Marshal(map[string]string{
		"sourceKey":        "1111111111111111",
		"rootKey":          "nas-photos",
		"observedIdentity": candidateIdentity,
	})
	if err != nil {
		t.Fatalf("marshal acceptance request: %v", err)
	}
	acceptRecorder := httptest.NewRecorder()
	handler.ServeHTTP(acceptRecorder, authenticatedRequest(http.MethodPost, "/v1/datasources/local/root/accept", bytes.NewReader(requestBody)))
	if acceptRecorder.Code != http.StatusOK {
		t.Fatalf("acceptance = %d body=%s", acceptRecorder.Code, acceptRecorder.Body.String())
	}
	var accepted runtimestate.LocalMediaRootAcceptanceResponse
	if err := json.Unmarshal(acceptRecorder.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode acceptance: %v", err)
	}
	if !accepted.Acceptance.Accepted || accepted.ScanStatus != "completed" || accepted.ScanError != "" || accepted.Phase0.Status != "completed" || accepted.Phase0.ScanMode != "reconciliation" || accepted.Phase0.DiscoveredPaths != 1 || accepted.Phase0.MissingPaths != 1 {
		t.Fatalf("acceptance response = %+v, want accepted reconciliation", accepted)
	}

	afterRecorder := httptest.NewRecorder()
	handler.ServeHTTP(afterRecorder, authenticatedRequest(http.MethodGet, "/status", nil))
	var after runtimestate.StatusResponse
	if err := json.Unmarshal(afterRecorder.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode status after acceptance: %v", err)
	}
	if len(after.LocalMediaRoots) != 1 || after.LocalMediaRoots[0].Status != "ready" || after.LocalMediaRoots[0].RootAcceptanceRequired || after.LocalMediaRoots[0].ObservedRootIdentity != "" {
		t.Fatalf("status after acceptance = %+v, want ready root", after.LocalMediaRoots)
	}
}

func TestRestartEndpointInvokesCallback(t *testing.T) {
	t.Parallel()

	called := make(chan struct{}, 1)
	handler := NewMuxWithOptions(newTestRuntime(t, 5), Options{
		Restart: func(context.Context) error {
			called <- struct{}{}
			return nil
		},
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/restart", nil))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 body=%s", recorder.Code, recorder.Body.String())
	}
	select {
	case <-called:
	default:
		t.Fatal("restart callback was not called")
	}
}

func TestUpdateCheckReportsAvailableRelease(t *testing.T) {
	t.Parallel()

	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"schemaVersion":           1,
			"product":                 "timich-agent",
			"version":                 "0.2.1",
			"minimumSupportedVersion": "0.1.0",
			"notesUrl":                "https://example.test/releases/v0.2.1",
			"artifacts": map[string]any{
				runtimePlatform(): map[string]string{
					"filename": "timich-agent_0.2.1_test.tar.gz",
					"url":      "https://example.test/timich-agent_0.2.1_test.tar.gz",
					"sha256":   strings.Repeat("a", 64),
				},
			},
			"updateGuide": map[string]any{
				"dockerCompose": []string{"Keep .local.", "Restart compose."},
			},
		})
	}))
	defer manifestServer.Close()

	handler := NewMuxWithOptions(newTestRuntimeWithBuildVersion(t, 5, "test-admin-token", "0.1.0"), Options{
		UpdateManifestURL: manifestServer.URL,
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/update-check", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		CurrentVersion string          `json:"currentVersion"`
		LatestVersion  string          `json:"latestVersion"`
		Status         string          `json:"status"`
		Artifact       *updateArtifact `json:"artifact"`
		Guide          updateGuide     `json:"guide"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.CurrentVersion != "0.1.0" || payload.LatestVersion != "0.2.1" || payload.Status != "update_available" {
		t.Fatalf("payload = %+v, want update_available from 0.1.0 to 0.2.1", payload)
	}
	if payload.Artifact == nil || payload.Artifact.Filename == "" {
		t.Fatalf("Artifact = %+v, want platform artifact", payload.Artifact)
	}
	if len(payload.Guide.DockerCompose) != 2 {
		t.Fatalf("Guide = %+v, want docker compose steps", payload.Guide)
	}
}

func TestUpdateCheckRejectsUnsafeManifestURL(t *testing.T) {
	t.Parallel()

	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"schemaVersion": 1,
			"product":       "timich-agent",
			"version":       "0.2.1",
			"notesUrl":      "javascript:alert(1)",
			"artifacts": map[string]any{
				runtimePlatform(): map[string]string{
					"filename": "timich-agent_0.2.1_test.tar.gz",
					"url":      "https://example.test/timich-agent_0.2.1_test.tar.gz",
					"sha256":   strings.Repeat("a", 64),
				},
			},
		})
	}))
	defer manifestServer.Close()

	handler := NewMuxWithOptions(newTestRuntimeWithBuildVersion(t, 5, "test-admin-token", "0.1.0"), Options{
		UpdateManifestURL: manifestServer.URL,
	})
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/update-check", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Status   string          `json:"status"`
		Message  string          `json:"message"`
		NotesURL string          `json:"notesUrl"`
		Artifact *updateArtifact `json:"artifact"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.Status != "unavailable" {
		t.Fatalf("Status = %q, want unavailable", payload.Status)
	}
	if !strings.Contains(payload.Message, "notesUrl") {
		t.Fatalf("Message = %q, want unsafe notesUrl explanation", payload.Message)
	}
	if payload.NotesURL != "" || payload.Artifact != nil {
		t.Fatalf("payload = %+v, want unsafe manifest to expose no links", payload)
	}
}

func TestUpdateCheckDisabledWithoutManifestURL(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()

	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/update-check", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.Status != "disabled" {
		t.Fatalf("Status = %q, want disabled", payload.Status)
	}
}

func TestPairingSessionsRequiresPost(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/pairing-sessions", nil))

	assertErrorPayload(t, recorder, http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestPairingSessionsCreatesSession(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "http://agent.local:8081/v1/pairing-sessions", nil))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		PairingCode         string                      `json:"pairingCode"`
		ExpiresAt           string                      `json:"expiresAt"`
		PairingURL          string                      `json:"pairingURL"`
		PairingPayload      any                         `json:"pairingPayload"`
		AgentBaseURLChoices []agentBaseURLChoicePayload `json:"agentBaseURLChoices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.PairingCode == "" {
		t.Fatalf("pairingCode is empty payload=%v", payload)
	}
	if payload.ExpiresAt == "" {
		t.Fatalf("expiresAt is empty payload=%v", payload)
	}
	if payload.PairingURL != "" || payload.PairingPayload != nil {
		t.Fatalf("pairing session should be code-first without generated link fields payload=%#v", payload)
	}
	if !hasAgentBaseURLChoice(payload.AgentBaseURLChoices, "http://agent.local:8082") {
		t.Fatalf("agentBaseURLChoices = %#v, want current Admin UI host candidate", payload.AgentBaseURLChoices)
	}
}

func TestNearbyLinksListApproveAndDeny(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, 5)
	handler := NewMux(runtime)
	created, err := runtime.CreateNearbyLink("Living Room TV", "android_tv")
	if err != nil {
		t.Fatalf("CreateNearbyLink() error = %v", err)
	}

	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, authenticatedRequest(http.MethodGet, "/v1/nearby-links", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var listPayload struct {
		NearbyLinks []struct {
			LinkID    string `json:"linkId"`
			LinkCode  string `json:"linkCode"`
			PollToken string `json:"pollToken"`
			Status    string `json:"status"`
		} `json:"nearbyLinks"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listPayload); err != nil {
		t.Fatalf("list response is not JSON: %v body=%s", err, listRecorder.Body.String())
	}
	if len(listPayload.NearbyLinks) != 1 || listPayload.NearbyLinks[0].LinkID != created.LinkID || listPayload.NearbyLinks[0].Status != "pending" {
		t.Fatalf("nearbyLinks = %+v, want created pending link", listPayload.NearbyLinks)
	}
	if listPayload.NearbyLinks[0].PollToken != "" {
		t.Fatalf("admin list exposed poll token %q", listPayload.NearbyLinks[0].PollToken)
	}

	approveBody := bytes.NewReader([]byte(`{"linkCode":"` + created.LinkCode + `"}`))
	approveRecorder := httptest.NewRecorder()
	handler.ServeHTTP(approveRecorder, authenticatedRequest(http.MethodPost, "/v1/nearby-links/approve", approveBody))
	if approveRecorder.Code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200 body=%s", approveRecorder.Code, approveRecorder.Body.String())
	}
	var approved struct {
		LinkID    string `json:"linkId"`
		PollToken string `json:"pollToken"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(approveRecorder.Body.Bytes(), &approved); err != nil {
		t.Fatalf("approve response is not JSON: %v body=%s", err, approveRecorder.Body.String())
	}
	if approved.LinkID != created.LinkID || approved.Status != "approved" || approved.PollToken != "" {
		t.Fatalf("approved = %+v, want approved link without poll token", approved)
	}

	denyCandidate, err := runtime.CreateNearbyLink("Bedroom TV", "android_tv")
	if err != nil {
		t.Fatalf("CreateNearbyLink() deny candidate error = %v", err)
	}
	denyRecorder := httptest.NewRecorder()
	handler.ServeHTTP(denyRecorder, authenticatedRequest(http.MethodPost, "/v1/nearby-links/"+denyCandidate.LinkID+"/deny", nil))
	if denyRecorder.Code != http.StatusOK {
		t.Fatalf("deny status = %d, want 200 body=%s", denyRecorder.Code, denyRecorder.Body.String())
	}
	var denied struct {
		LinkID string `json:"linkId"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(denyRecorder.Body.Bytes(), &denied); err != nil {
		t.Fatalf("deny response is not JSON: %v body=%s", err, denyRecorder.Body.String())
	}
	if denied.LinkID != denyCandidate.LinkID || denied.Status != "denied" {
		t.Fatalf("denied = %+v, want denied candidate", denied)
	}
}

func TestPairingSessionsUsesPublishedMediaPortForAdminHostCandidate(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.MediaPublishedAddress = "18082"
	})
	NewMux(runtime).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "http://agent.local:8081/v1/pairing-sessions", nil))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		AgentBaseURLChoices []agentBaseURLChoicePayload `json:"agentBaseURLChoices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if !hasAgentBaseURLChoice(payload.AgentBaseURLChoices, "http://agent.local:18082") {
		t.Fatalf("agentBaseURLChoices = %#v, want published media port candidate", payload.AgentBaseURLChoices)
	}
	if hasAgentBaseURLChoice(payload.AgentBaseURLChoices, "http://agent.local:8082") {
		t.Fatalf("agentBaseURLChoices = %#v, should not use container media port when published port is configured", payload.AgentBaseURLChoices)
	}
}

func TestPairingSessionsSkipsAdminHostCandidateForLoopbackPublishedMedia(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.MediaPublishedAddress = "127.0.0.1:18082"
	})
	NewMux(runtime).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "http://agent.local:8081/v1/pairing-sessions", nil))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		AgentBaseURLChoices []agentBaseURLChoicePayload `json:"agentBaseURLChoices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if len(payload.AgentBaseURLChoices) != 0 {
		t.Fatalf("agentBaseURLChoices = %#v, want no phone-reachable candidate for loopback host media publishing", payload.AgentBaseURLChoices)
	}
}

func TestPairingSessionsUsesPublishedMediaHostCandidate(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.MediaPublishedAddress = "10.0.111.128:18082"
	})
	NewMux(runtime).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "http://127.0.0.1:8081/v1/pairing-sessions", nil))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		AgentBaseURLChoices []agentBaseURLChoicePayload `json:"agentBaseURLChoices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if !hasAgentBaseURLChoice(payload.AgentBaseURLChoices, "http://10.0.111.128:18082") {
		t.Fatalf("agentBaseURLChoices = %#v, want published media host candidate", payload.AgentBaseURLChoices)
	}
}

func TestPairingSessionsCreatesCodeFirstResponseForLoopbackAdminHost(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "http://127.0.0.1:8081/v1/pairing-sessions", nil))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		PairingCode          string                      `json:"pairingCode"`
		PairingURL           string                      `json:"pairingURL"`
		PairingQRCodeDataURL string                      `json:"pairingQRCodeDataURL"`
		PairingPayload       any                         `json:"pairingPayload"`
		AgentBaseURLChoices  []agentBaseURLChoicePayload `json:"agentBaseURLChoices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.PairingCode == "" {
		t.Fatalf("pairingCode is empty payload=%v", payload)
	}
	if payload.PairingPayload != nil || payload.PairingURL != "" || payload.PairingQRCodeDataURL != "" {
		t.Fatalf("link fields should be omitted for code-only pairing payload=%#v", payload)
	}
	if len(payload.AgentBaseURLChoices) != 0 {
		t.Fatalf("agentBaseURLChoices = %#v, want no Docker/internal interface candidates from loopback Admin UI", payload.AgentBaseURLChoices)
	}
}

func TestPairingSessionsSkipsAdminHostChoiceForProxiedRequests(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	request := authenticatedRequest(http.MethodPost, "http://agent.local:8081/v1/pairing-sessions", nil)
	request.Header.Set("X-Forwarded-Host", "admin.example")
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		PairingCode         string                      `json:"pairingCode"`
		AgentBaseURLChoices []agentBaseURLChoicePayload `json:"agentBaseURLChoices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.PairingCode == "" {
		t.Fatal("pairingCode is empty")
	}
	if len(payload.AgentBaseURLChoices) != 0 {
		t.Fatalf("agentBaseURLChoices = %#v, should not trust proxied Admin UI host or detected interfaces", payload.AgentBaseURLChoices)
	}
}

func TestPairingLinksCreatesQRCodeForSelectedAgentBaseURL(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, 5)
	pairingSession, err := runtime.CreatePairingSession()
	if err != nil {
		t.Fatalf("CreatePairingSession() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"agentBaseURL":"http://10.0.1.4:8082/","pairingCode":"` + pairingSession.PairingCode + `"}`))
	NewMux(runtime).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/pairing-links", body))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		PairingPayload struct {
			Version      int    `json:"version"`
			Kind         string `json:"kind"`
			AgentBaseURL string `json:"agentBaseURL"`
			PairingCode  string `json:"pairingCode"`
		} `json:"pairingPayload"`
		PairingURL           string `json:"pairingURL"`
		PairingQRCodeDataURL string `json:"pairingQRCodeDataURL"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload.PairingPayload.AgentBaseURL != "http://10.0.1.4:8082" {
		t.Fatalf("agent base URL = %q, want selected media URL", payload.PairingPayload.AgentBaseURL)
	}
	if payload.PairingPayload.Version != 1 || payload.PairingPayload.Kind != "timich.agent.pairing" {
		t.Fatalf("pairing payload = %#v, want Timich pairing payload", payload.PairingPayload)
	}
	if payload.PairingPayload.PairingCode != pairingSession.PairingCode {
		t.Fatalf("pairing code = %q, want active pairing code", payload.PairingPayload.PairingCode)
	}
	if !strings.HasPrefix(payload.PairingURL, "https://link.timich.runo.jp/pair?payload=") {
		t.Fatalf("pairing URL = %q, want production Universal Link", payload.PairingURL)
	}
	if !strings.HasPrefix(payload.PairingQRCodeDataURL, "data:image/png;base64,") {
		t.Fatalf("pairing QR data URL prefix = %q", payload.PairingQRCodeDataURL[:min(len(payload.PairingQRCodeDataURL), 32)])
	}

	parsedPairingURL, err := url.Parse(payload.PairingURL)
	if err != nil {
		t.Fatalf("pairing URL did not parse: %v", err)
	}
	encodedPayload := parsedPairingURL.Query().Get("payload")
	if encodedPayload == "" {
		t.Fatalf("pairing URL missing payload query: %q", payload.PairingURL)
	}
	decodedPayload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		t.Fatalf("pairing URL payload did not decode: %v", err)
	}
	var linkPayload struct {
		AgentBaseURL string `json:"agentBaseURL"`
		PairingCode  string `json:"pairingCode"`
	}
	if err := json.Unmarshal(decodedPayload, &linkPayload); err != nil {
		t.Fatalf("pairing URL payload is not JSON: %v payload=%s", err, string(decodedPayload))
	}
	if linkPayload.AgentBaseURL != payload.PairingPayload.AgentBaseURL || linkPayload.PairingCode != payload.PairingPayload.PairingCode {
		t.Fatalf("link payload = %#v, want response payload", linkPayload)
	}
}

func TestPairingLinksRejectsUnreachableAgentBaseURL(t *testing.T) {
	t.Parallel()

	tests := []string{
		"http://localhost:8082",
		"http://127.0.0.1:8082",
		"http://0.0.0.0:8082",
		"http://[::]:8082",
		"http://[::1]:8082",
	}
	for _, agentBaseURL := range tests {
		t.Run(agentBaseURL, func(t *testing.T) {
			t.Parallel()

			runtime := newTestRuntime(t, 5)
			pairingSession, err := runtime.CreatePairingSession()
			if err != nil {
				t.Fatalf("CreatePairingSession() error = %v", err)
			}

			recorder := httptest.NewRecorder()
			body := bytes.NewReader([]byte(`{"agentBaseURL":"` + agentBaseURL + `","pairingCode":"` + pairingSession.PairingCode + `"}`))
			NewMux(runtime).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/pairing-links", body))

			assertErrorPayload(t, recorder, http.StatusBadRequest, "agent_base_url_invalid")
		})
	}
}

func TestPairingLinksRejectsInactivePairingCode(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"agentBaseURL":"http://10.0.1.4:8082","pairingCode":"PAIRING-CODE"}`))
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/pairing-links", body))

	assertErrorPayload(t, recorder, http.StatusBadRequest, "pairing_session_invalid")
}

func TestPairingLinksRejectsReplacedPairingCode(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, 5)
	firstSession, err := runtime.CreatePairingSession()
	if err != nil {
		t.Fatalf("CreatePairingSession() first error = %v", err)
	}
	if _, err := runtime.CreatePairingSession(); err != nil {
		t.Fatalf("CreatePairingSession() second error = %v", err)
	}

	recorder := httptest.NewRecorder()
	body := bytes.NewReader([]byte(`{"agentBaseURL":"http://10.0.1.4:8082","pairingCode":"` + firstSession.PairingCode + `"}`))
	NewMux(runtime).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/pairing-links", body))

	assertErrorPayload(t, recorder, http.StatusBadRequest, "pairing_session_invalid")
}

func TestPairingSessionsMapsDeviceLimitError(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 0)).ServeHTTP(recorder, authenticatedRequest(http.MethodPost, "/v1/pairing-sessions", nil))

	assertErrorPayload(t, recorder, http.StatusConflict, "device_limit_reached")
}

func TestCompatibilityCheckRequiresPost(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	NewMux(newTestRuntime(t, 5)).ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/compatibility-check", nil))

	assertErrorPayload(t, recorder, http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestDevicesListAndRevoke(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, 5)
	created, err := runtime.CreateHostedSession("Old iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	handler := NewMux(runtime)

	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, authenticatedRequest(http.MethodGet, "/v1/devices", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var listPayload struct {
		Devices []struct {
			DeviceID   string `json:"deviceId"`
			DeviceName string `json:"deviceName"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listPayload); err != nil {
		t.Fatalf("list response is not JSON: %v body=%s", err, listRecorder.Body.String())
	}
	if len(listPayload.Devices) != 1 || listPayload.Devices[0].DeviceID != created.DeviceID {
		t.Fatalf("devices = %#v, want created device", listPayload.Devices)
	}

	renameRecorder := httptest.NewRecorder()
	renameBody := bytes.NewReader([]byte(`{"deviceName":"Alice iPhone"}`))
	handler.ServeHTTP(renameRecorder, authenticatedRequest(http.MethodPut, "/v1/devices/"+created.DeviceID, renameBody))
	if renameRecorder.Code != http.StatusOK {
		t.Fatalf("rename status = %d, want 200 body=%s", renameRecorder.Code, renameRecorder.Body.String())
	}
	var renamePayload struct {
		DeviceID   string `json:"deviceId"`
		DeviceName string `json:"deviceName"`
	}
	if err := json.Unmarshal(renameRecorder.Body.Bytes(), &renamePayload); err != nil {
		t.Fatalf("rename response is not JSON: %v body=%s", err, renameRecorder.Body.String())
	}
	if renamePayload.DeviceID != created.DeviceID || renamePayload.DeviceName != "Alice iPhone" {
		t.Fatalf("rename payload = %+v, want renamed device", renamePayload)
	}
	invalidRenameRecorder := httptest.NewRecorder()
	handler.ServeHTTP(invalidRenameRecorder, authenticatedRequest(http.MethodPut, "/v1/devices/"+created.DeviceID, bytes.NewReader([]byte(`{"deviceName":" "}`))))
	assertErrorPayload(t, invalidRenameRecorder, http.StatusBadRequest, "device_name_invalid")

	renamedListRecorder := httptest.NewRecorder()
	handler.ServeHTTP(renamedListRecorder, authenticatedRequest(http.MethodGet, "/v1/devices", nil))
	if renamedListRecorder.Code != http.StatusOK {
		t.Fatalf("renamed list status = %d, want 200 body=%s", renamedListRecorder.Code, renamedListRecorder.Body.String())
	}
	if err := json.Unmarshal(renamedListRecorder.Body.Bytes(), &listPayload); err != nil {
		t.Fatalf("renamed list response is not JSON: %v body=%s", err, renamedListRecorder.Body.String())
	}
	if len(listPayload.Devices) != 1 || listPayload.Devices[0].DeviceName != "Alice iPhone" {
		t.Fatalf("renamed devices = %#v, want Alice iPhone", listPayload.Devices)
	}

	revokeRecorder := httptest.NewRecorder()
	handler.ServeHTTP(revokeRecorder, authenticatedRequest(http.MethodDelete, "/v1/devices/"+created.DeviceID, nil))
	if revokeRecorder.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204 body=%s", revokeRecorder.Code, revokeRecorder.Body.String())
	}
	if _, err := runtime.RefreshAppSession(created.RefreshToken, "https://timich.example"); !errors.Is(err, store.ErrRefreshTokenNotFound) {
		t.Fatalf("RefreshAppSession() error = %v, want %v", err, store.ErrRefreshTokenNotFound)
	}
}

func TestUploadRootsAndDevicePolicyEndpoints(t *testing.T) {
	t.Parallel()

	rootPath := t.TempDir()
	runtime := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.UploadRoots = []config.UploadRootConfig{{Key: "nas-photos", Path: rootPath}}
	})
	created, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	profiles, err := store.LoadOrCreateDeviceProfileStore(runtime.ConfigResponse().DataDir)
	if err != nil {
		t.Fatalf("LoadOrCreateDeviceProfileStore() error = %v", err)
	}
	if err := profiles.DeleteProfile(created.DeviceID); err != nil {
		t.Fatalf("DeleteProfile() error = %v", err)
	}
	handler := NewMux(runtime)

	rootsRecorder := httptest.NewRecorder()
	handler.ServeHTTP(rootsRecorder, authenticatedRequest(http.MethodGet, "/v1/uploads/roots", nil))
	if rootsRecorder.Code != http.StatusOK {
		t.Fatalf("roots status = %d, want 200 body=%s", rootsRecorder.Code, rootsRecorder.Body.String())
	}
	var rootsPayload struct {
		Roots []struct {
			Key      string `json:"key"`
			TempPath string `json:"tempPath"`
			Status   string `json:"status"`
			Writable bool   `json:"writable"`
		} `json:"roots"`
	}
	if err := json.Unmarshal(rootsRecorder.Body.Bytes(), &rootsPayload); err != nil {
		t.Fatalf("roots response is not JSON: %v body=%s", err, rootsRecorder.Body.String())
	}
	if len(rootsPayload.Roots) != 1 ||
		rootsPayload.Roots[0].Key != "nas-photos" ||
		rootsPayload.Roots[0].TempPath != config.DefaultUploadRootTempPath ||
		rootsPayload.Roots[0].Status != "ready" ||
		!rootsPayload.Roots[0].Writable {
		t.Fatalf("roots payload = %+v, want writable nas-photos", rootsPayload.Roots)
	}

	updateBody := bytes.NewReader([]byte(`{"enabled":true,"rootKey":"nas-photos","pathPattern":"{deviceName}/{yyyy}-{MM}-{dd}/{filename}","capturedAfter":"2026-05-01T00:00:00Z"}`))
	updateRecorder := httptest.NewRecorder()
	handler.ServeHTTP(updateRecorder, authenticatedRequest(http.MethodPut, "/v1/devices/"+created.DeviceID+"/upload-policy", updateBody))
	if updateRecorder.Code != http.StatusOK {
		t.Fatalf("policy update status = %d, want 200 body=%s", updateRecorder.Code, updateRecorder.Body.String())
	}
	var policyPayload struct {
		DeviceID string `json:"deviceId"`
		Upload   struct {
			Enabled bool   `json:"enabled"`
			RootKey string `json:"rootKey"`
		} `json:"upload"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(updateRecorder.Body.Bytes(), &policyPayload); err != nil {
		t.Fatalf("policy response is not JSON: %v body=%s", err, updateRecorder.Body.String())
	}
	if policyPayload.DeviceID != created.DeviceID || !policyPayload.Upload.Enabled || policyPayload.Upload.RootKey != "nas-photos" || policyPayload.Status.State != "ready" {
		t.Fatalf("policy payload = %+v, want enabled ready policy", policyPayload)
	}

	resetBody := bytes.NewReader([]byte(`{"capturedAfter":"2026-05-01T00:00:00Z","capturedBefore":"2026-06-01T00:00:00Z","reason":"test"}`))
	resetRecorder := httptest.NewRecorder()
	handler.ServeHTTP(resetRecorder, authenticatedRequest(http.MethodPost, "/v1/devices/"+created.DeviceID+"/upload-reset", resetBody))
	if resetRecorder.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 body=%s", resetRecorder.Code, resetRecorder.Body.String())
	}
	var resetPayload struct {
		DeviceID string `json:"deviceId"`
		ResetAt  string `json:"resetAt"`
	}
	if err := json.Unmarshal(resetRecorder.Body.Bytes(), &resetPayload); err != nil {
		t.Fatalf("reset response is not JSON: %v body=%s", err, resetRecorder.Body.String())
	}
	if resetPayload.DeviceID != created.DeviceID || resetPayload.ResetAt == "" {
		t.Fatalf("reset payload = %+v, want device reset response", resetPayload)
	}
}

func TestUploadPolicyRejectsUnknownRootAndResetRequiresRange(t *testing.T) {
	t.Parallel()

	runtime := newTestRuntime(t, 5)
	created, err := runtime.CreateHostedSession("Upload iPhone", "https://timich.example")
	if err != nil {
		t.Fatalf("CreateHostedSession() error = %v", err)
	}
	handler := NewMux(runtime)

	updateRecorder := httptest.NewRecorder()
	updateBody := bytes.NewReader([]byte(`{"enabled":true,"rootKey":"missing-root","pathPattern":"{deviceName}/{filename}"}`))
	handler.ServeHTTP(updateRecorder, authenticatedRequest(http.MethodPut, "/v1/devices/"+created.DeviceID+"/upload-policy", updateBody))
	assertErrorPayload(t, updateRecorder, http.StatusBadRequest, "upload_root_not_found")

	resetRecorder := httptest.NewRecorder()
	handler.ServeHTTP(resetRecorder, authenticatedRequest(http.MethodPost, "/v1/devices/"+created.DeviceID+"/upload-reset", bytes.NewReader([]byte(`{}`))))
	assertErrorPayload(t, resetRecorder, http.StatusBadRequest, "upload_reset_range_required")

	invalidRangeRecorder := httptest.NewRecorder()
	invalidRangeBody := bytes.NewReader([]byte(`{"capturedAfter":"2026-06-01T00:00:00Z","capturedBefore":"2026-05-01T00:00:00Z"}`))
	handler.ServeHTTP(invalidRangeRecorder, authenticatedRequest(http.MethodPost, "/v1/devices/"+created.DeviceID+"/upload-reset", invalidRangeBody))
	assertErrorPayload(t, invalidRangeRecorder, http.StatusBadRequest, "upload_reset_invalid")
}

func authenticatedRequest(method string, path string, body *bytes.Reader) *http.Request {
	var reader io.Reader
	if body != nil {
		reader = body
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Authorization", "Bearer test-admin-token")
	return request
}

func waitSemanticInstallJob(t *testing.T, handler http.Handler) semanticInstallJobStatus {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var payload semanticInstallJobStatus
	for time.Now().Before(deadline) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, authenticatedRequest(http.MethodGet, "/v1/semantic-install-job", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("semantic install job status = %d, want 200 body=%s", recorder.Code, recorder.Body.String())
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("semantic install job response is not JSON: %v body=%s", err, recorder.Body.String())
		}
		if payload.Status != semanticInstallJobStatusRunning {
			return payload
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("semantic install job did not finish before timeout; latest=%+v", payload)
	return semanticInstallJobStatus{}
}

func waitSemanticIndexPublished(t *testing.T, runtime *runtimestate.AgentRuntime, wantIndexed int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var latest *catalog.SemanticModelBackfillStatus
	for time.Now().Before(deadline) {
		status := runtime.SemanticModelRegistryStatusWithContext(context.Background())
		latest = status.Indexing
		if latest != nil &&
			latest.IndexedVectorCount == wantIndexed &&
			latest.PendingIndexJobCount == 0 &&
			latest.FailedIndexJobCount == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("semantic index did not publish before timeout; latest=%+v", latest)
}

func assertErrorPayload(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()

	if recorder.Code != wantStatus {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, wantStatus, recorder.Body.String())
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not JSON: %v body=%s", err, recorder.Body.String())
	}
	if payload["error"] != wantCode {
		t.Fatalf("error = %q, want %q payload=%v", payload["error"], wantCode, payload)
	}
}

func hasAgentBaseURLChoice(choices []agentBaseURLChoicePayload, target string) bool {
	for _, choice := range choices {
		if choice.URL == target {
			return true
		}
	}
	return false
}

func newTestRuntime(t *testing.T, deviceLimit int) *runtimestate.AgentRuntime {
	t.Helper()

	return newTestRuntimeWithAdminToken(t, deviceLimit, "test-admin-token")
}

func newTestRuntimeWithoutAdminToken(t *testing.T) *runtimestate.AgentRuntime {
	t.Helper()

	return newTestRuntimeWithAdminToken(t, 5, "")
}

func newTestRuntimeWithAdminToken(t *testing.T, deviceLimit int, adminToken string) *runtimestate.AgentRuntime {
	t.Helper()

	return newTestRuntimeWithBuildVersion(t, deviceLimit, adminToken, "test-version")
}

func newTestRuntimeWithBuildVersion(t *testing.T, deviceLimit int, adminToken string, version string) *runtimestate.AgentRuntime {
	return newTestRuntimeWithBuildVersionAndConfig(t, deviceLimit, adminToken, version, nil)
}

func newTestRuntimeWithConfig(
	t *testing.T,
	deviceLimit int,
	adminToken string,
	configure func(*config.ResolvedConfig),
) *runtimestate.AgentRuntime {
	t.Helper()

	return newTestRuntimeWithBuildVersionAndConfig(t, deviceLimit, adminToken, "test-version", configure)
}

func newStaticDemoAdminTestRuntime(t *testing.T) *runtimestate.AgentRuntime {
	t.Helper()

	root := t.TempDir()
	jpegBytes := semanticModelTestJPEG(t)
	for _, name := range []string{"preview.jpg", "detail_preview.jpg", "original.jpg"} {
		if err := os.WriteFile(filepath.Join(root, name), jpegBytes, 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	manifestPath := filepath.Join(root, "manifest.json")
	rawManifest := []byte(`{"version":1,"assets":[{"id":"demo-0001","type":"IMAGE","originalFileName":"demo.jpg","fileCreatedAt":"2026-01-01T00:00:00Z","previewPath":"preview.jpg","detailPreviewPath":"detail_preview.jpg","originalPath":"original.jpg"}]}`)
	if err := os.WriteFile(manifestPath, rawManifest, 0o600); err != nil {
		t.Fatalf("WriteFile(manifest.json) error = %v", err)
	}

	return newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		cfg.Datasources = []config.DatasourceConfig{{
			Name: "Demo",
			Kind: config.DatasourceKindStaticDemo,
			URL:  manifestPath,
		}}
	})
}

func newTestRuntimeWithBuildVersionAndConfig(
	t *testing.T,
	deviceLimit int,
	adminToken string,
	version string,
	configure func(*config.ResolvedConfig),
) *runtimestate.AgentRuntime {
	t.Helper()

	dataDir := t.TempDir()
	cfg := config.ResolvedConfig{
		Config: config.Default(),
	}
	cfg.AgentName = "test-agent"
	cfg.DataDir = dataDir
	cfg.DeviceLimit = max(deviceLimit, 1)
	cfg.ConfigSource = "test"
	cfg.ConfigPath = filepath.Join(dataDir, "agent.json")
	if configure != nil {
		configure(&cfg)
	}
	if strings.TrimSpace(cfg.SemanticRuntime.HelperPath) == "" {
		cfg.SemanticRuntime.HelperPath = semanticRuntimeGenericEmbeddingHelperScript(t)
	}
	if strings.TrimSpace(cfg.SemanticRuntime.ONNXRuntime.ServerPath) == "" {
		serverPath, pythonPath := semanticONNXRuntimeExecutablesForAdminTest(t)
		cfg.SemanticRuntime.ONNXRuntime.ServerPath = serverPath
		cfg.SemanticRuntime.ONNXRuntime.PythonPath = pythonPath
	}

	signingKey := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	runtime, err := runtimestate.NewAgentRuntime(runtimestate.BuildInfo{
		Version:    version,
		Commit:     "test-commit",
		BuiltAt:    "2026-04-25T00:00:00Z",
		ReleaseTag: "v0.4.0-rc.2",
	}, cfg, store.LoadedState{
		Path: filepath.Join(dataDir, "agent-state.json"),
		State: store.State{
			AgentID:           "agent-test",
			CreatedAt:         time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC),
			SessionSigningKey: signingKey,
			AdminToken:        adminToken,
		},
	}, time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewAgentRuntime() error = %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("AgentRuntime.Close() error = %v", err)
		}
	})
	if deviceLimit == 0 {
		_, err := runtime.CreateHostedSession("already paired", "https://timich.example")
		if err != nil {
			t.Fatalf("CreateHostedSession() error = %v", err)
		}
	}
	return runtime
}

func semanticRuntimeGenericEmbeddingHelperScript(t *testing.T) string {
	t.Helper()

	helperPath := filepath.Join(t.TempDir(), "timich-semantic-helper")
	script := `#!/usr/bin/env python3
import json
import sys

command = sys.argv[1]
args = sys.argv[2:]
runtime_layout = args[args.index("--runtime-layout") + 1]
with open(runtime_layout + "/timich-model.json", "r", encoding="utf-8") as handle:
    layout = json.load(handle)
response = {
    "protocolVersion": 1,
    "runtime": layout["runtime"],
    "modelId": layout["modelId"],
    "vectorSpaceId": layout["vectorSpaceId"],
    "embeddingDim": layout["embeddingDim"],
    "inputKind": layout["inputKind"],
}
if command == "inspect":
    response.update({"loaded": True, "canEmbed": True})
elif command in ("embed-text", "embed-image"):
    if command == "embed-image":
        sys.stdin.buffer.read()
    response["vector"] = [1.0] + [0.0] * (int(layout["embeddingDim"]) - 1)
    response["input"] = "install-probe"
else:
    raise SystemExit(2)
print(json.dumps(response, separators=(",", ":")))
`
	if err := os.WriteFile(helperPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write generic semantic runtime helper: %v", err)
	}
	return helperPath
}

func semanticONNXRuntimeExecutablesForAdminTest(t *testing.T) (string, string) {
	t.Helper()
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for semantic runtime tests")
	}
	serverPath := filepath.Join(t.TempDir(), "semantic-server.py")
	script := `import argparse
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

parser = argparse.ArgumentParser()
parser.add_argument("--host", default="127.0.0.1")
parser.add_argument("--port", type=int, required=True)
parser.add_argument("--runtime-layout", required=True)
args, _ = parser.parse_known_args()
with open(args.runtime_layout + "/timich-model.json", "r", encoding="utf-8") as handle:
    layout = json.load(handle)

def embedding_response(input_name):
    vector = [0.0] * int(layout["embeddingDim"])
    vector[0] = 1.0
    return {
        "protocolVersion": 1,
        "runtime": layout["runtime"],
        "modelId": layout["modelId"],
        "vectorSpaceId": layout["vectorSpaceId"],
        "embeddingDim": layout["embeddingDim"],
        "inputKind": layout["inputKind"],
        "vector": vector,
        "input": input_name,
    }

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/healthz":
            self.send_error(404)
            return
        self.respond({"status": "ok"})

    def do_POST(self):
        if self.path not in ("/embed-text", "/embed-image"):
            self.send_error(404)
            return
        length = int(self.headers.get("Content-Length", "0"))
        if length:
            self.rfile.read(length)
        self.respond(embedding_response(self.path))

    def respond(self, payload):
        body = json.dumps(payload).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):
        return

ThreadingHTTPServer((args.host, args.port), Handler).serve_forever()
`
	if err := os.WriteFile(serverPath, []byte(script), 0o600); err != nil {
		t.Fatalf("write semantic ONNX test server: %v", err)
	}
	return serverPath, pythonPath
}

func adminTestIntPtr(value int) *int {
	return &value
}
