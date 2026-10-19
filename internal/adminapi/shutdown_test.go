package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
	"github.com/rsahara/timich-agent/internal/config"
)

func awaitAdminShutdownEvent(t *testing.T, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown event not observed")
	}
}

func TestSemanticInstallShutdownWaitsForCleanupAfterHTTPReturn(t *testing.T) {
	a := newTestRuntime(t, 5)
	s := &server{runtime: a, semanticInstallJobs: newSemanticInstallJobStore()}
	entered, cleanup := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(cleanup) })
	t.Cleanup(finish)
	var jobCtx context.Context
	run := func(ctx context.Context) (any, string, error) {
		jobCtx = ctx
		close(entered)
		<-ctx.Done()
		<-cleanup
		return nil, "", ctx.Err()
	}
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	response := httptest.NewRecorder()
	a.RequestHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.startSemanticInstallJob(w, semanticInstallJobStart{Action: "install_model"}, run)
	})).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/install", nil).WithContext(requestCtx))
	cancelRequest() // net/http cancels this context when the handler returns.
	awaitAdminShutdownEvent(t, entered)
	if response.Code != http.StatusAccepted || jobCtx.Err() != nil {
		t.Fatalf("202 job did not outlive HTTP: status=%d context=%v", response.Code, jobCtx.Err())
	}
	duplicate := httptest.NewRecorder()
	s.startSemanticInstallJob(duplicate, semanticInstallJobStart{Action: "install_model"}, func(context.Context) (any, string, error) {
		t.Error("duplicate install ran")
		return nil, "", nil
	})
	if duplicate.Code != http.StatusAccepted || a.PendingShutdownWork() != 1 {
		t.Fatalf("duplicate admission leaked ownership: status=%d pending=%d", duplicate.Code, a.PendingShutdownWork())
	}
	a.BeginShutdown()
	awaitAdminShutdownEvent(t, jobCtx.Done())
	blocked, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := a.CloseResources(blocked); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resources closed before job cleanup: %v", err)
	}
	finish()
	wait, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := a.WaitStopped(wait); err != nil {
		t.Fatal(err)
	}
	job := s.semanticInstallJobs.snapshot()
	if job.Status != semanticInstallJobStatusFailed || job.FinishedAt == nil {
		t.Fatalf("ownership released before result publication: %+v", job)
	}
	if err := a.CloseResources(wait); err != nil {
		t.Fatal(err)
	}
}

func TestSemanticAsyncAdmissionRacesShutdown(t *testing.T) {
	a := newTestRuntime(t, 5)
	s := &server{runtime: a, semanticInstallJobs: newSemanticInstallJobStore()}
	start := make(chan struct{})
	var callers sync.WaitGroup
	for range 40 {
		callers.Go(func() {
			<-start
			response := httptest.NewRecorder()
			s.startSemanticInstallJob(response, semanticInstallJobStart{Action: "install_model"}, func(ctx context.Context) (any, string, error) {
				<-ctx.Done()
				return nil, "", ctx.Err()
			})
			if response.Code != http.StatusAccepted && response.Code != http.StatusServiceUnavailable {
				t.Errorf("admission status=%d", response.Code)
			}
			s.scheduleSemanticModelStatusRefresh()
		})
	}
	callers.Go(func() { <-start; a.BeginShutdown() })
	close(start)
	callers.Wait()
	wait, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.WaitStopped(wait); err != nil {
		t.Fatal(err)
	}
	before := s.semanticInstallJobs.snapshot()
	late := httptest.NewRecorder()
	s.startSemanticInstallJob(late, semanticInstallJobStart{Action: "install_model"}, func(context.Context) (any, string, error) {
		t.Error("post-stop install ran")
		return nil, "", nil
	})
	s.scheduleSemanticModelStatusRefresh()
	if late.Code != http.StatusServiceUnavailable || !reflect.DeepEqual(before, s.semanticInstallJobs.snapshot()) {
		t.Fatal("post-stop admission acknowledged or changed a job")
	}
	s.semanticModelsRefreshMu.Lock()
	busy := s.semanticModelsRefreshBusy
	s.semanticModelsRefreshMu.Unlock()
	if busy || a.PendingShutdownWork() != 0 {
		t.Fatalf("async work leaked: busy=%v pending=%d", busy, a.PendingShutdownWork())
	}
}

type shutdownRoundTripper func(*http.Request) (*http.Response, error)

func (f shutdownRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSemanticModelRefreshShutdownPreservesPersistedSnapshot(t *testing.T) {
	var dataDir string
	a := newTestRuntimeWithConfig(t, 5, "test-admin-token", func(cfg *config.ResolvedConfig) {
		dataDir = cfg.DataDir
		cfg.Datasources = []config.DatasourceConfig{{
			SourceKey: "1111111111111111", Name: "Home Immich", Kind: config.DatasourceKindImmichIndexed,
			URL: "http://immich.local:2283", AccessToken: "test",
		}}
	})
	good := catalog.SemanticModelRegistryStatus{RegistryStatus: "available", RegistryMessage: "last successful refresh"}
	a.RememberSemanticModelRegistryStatus(good)
	entered, canceled, cleanup := make(chan struct{}), make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(cleanup) })
	t.Cleanup(finish)
	s := &server{runtime: a, options: Options{
		SemanticModelManifestURL: "https://registry.example/manifest.json",
		SemanticModelHTTPClient: &http.Client{Transport: shutdownRoundTripper(func(r *http.Request) (*http.Response, error) {
			close(entered)
			<-r.Context().Done()
			close(canceled)
			<-cleanup
			return nil, r.Context().Err()
		})},
	}}
	s.scheduleSemanticModelStatusRefresh()
	awaitAdminShutdownEvent(t, entered)
	s.scheduleSemanticModelStatusRefresh() // A coalesced refresh must not leak an owner.
	if a.PendingShutdownWork() != 1 {
		t.Fatalf("refresh ownership=%d, want 1", a.PendingShutdownWork())
	}
	a.BeginShutdown()
	awaitAdminShutdownEvent(t, canceled)
	blocked, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := a.CloseResources(blocked); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resources closed during refresh cleanup: %v", err)
	}
	finish()
	wait, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := a.WaitStopped(wait); err != nil {
		t.Fatal(err)
	}
	if cached, ok := a.CachedSemanticModelRegistryStatus(wait); !ok || !reflect.DeepEqual(cached, good) {
		t.Fatalf("cancellation replaced good cache: %+v ok=%v", cached, ok)
	}
	s.semanticModelsRefreshMu.Lock()
	busy, updated := s.semanticModelsRefreshBusy, s.semanticModelsRefreshAt
	s.semanticModelsRefreshMu.Unlock()
	if busy || !updated.IsZero() {
		t.Fatalf("canceled refresh retained busy state or marked success: busy=%v updated=%v", busy, updated)
	}
	if err := a.CloseResources(wait); err != nil {
		t.Fatal(err)
	}
	reopened, err := catalog.LoadOrCreateCatalogStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, ok, err := reopened.AdminStatusSnapshot(wait, "semantic_model_registry")
	if err != nil || !ok {
		t.Fatalf("persisted snapshot: ok=%v err=%v", ok, err)
	}
	var saved catalog.SemanticModelRegistryStatus
	if err := json.Unmarshal(stored.Payload, &saved); err != nil || !reflect.DeepEqual(saved, good) {
		t.Fatalf("persisted cancellation result: %+v err=%v", saved, err)
	}
}
