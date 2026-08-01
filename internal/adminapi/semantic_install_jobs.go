package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rsahara/timich-agent/internal/catalog"
)

const (
	semanticInstallJobStatusIdle     = "idle"
	semanticInstallJobStatusRunning  = "running"
	semanticInstallJobStatusComplete = "complete"
	semanticInstallJobStatusFailed   = "failed"

	semanticInstallJobTimeout = 30 * time.Minute
)

type semanticInstallJobStart struct {
	Action        string
	Label         string
	ModelID       string
	VectorSpaceID string
}

type semanticInstallJobStatus struct {
	ID            string          `json:"id,omitempty"`
	Action        string          `json:"action,omitempty"`
	Label         string          `json:"label,omitempty"`
	Status        string          `json:"status"`
	ModelID       string          `json:"modelId,omitempty"`
	VectorSpaceID string          `json:"vectorSpaceId,omitempty"`
	StartedAt     *time.Time      `json:"startedAt,omitempty"`
	FinishedAt    *time.Time      `json:"finishedAt,omitempty"`
	Message       string          `json:"message,omitempty"`
	ErrorCode     string          `json:"errorCode,omitempty"`
	ErrorMessage  string          `json:"errorMessage,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
}

type semanticInstallJobStore struct {
	mu     sync.Mutex
	nextID int64
	latest semanticInstallJobStatus
}

func newSemanticInstallJobStore() *semanticInstallJobStore {
	return &semanticInstallJobStore{
		latest: semanticInstallJobStatus{Status: semanticInstallJobStatusIdle},
	}
}

func (s *semanticInstallJobStore) snapshot() semanticInstallJobStatus {
	if s == nil {
		return semanticInstallJobStatus{Status: semanticInstallJobStatusIdle}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSemanticInstallJobStatus(s.latest)
}

func (s *semanticInstallJobStore) start(input semanticInstallJobStart) (semanticInstallJobStatus, bool) {
	if s == nil {
		return semanticInstallJobStatus{Status: semanticInstallJobStatusIdle}, false
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest.Status == semanticInstallJobStatusRunning {
		return cloneSemanticInstallJobStatus(s.latest), false
	}
	s.nextID++
	s.latest = semanticInstallJobStatus{
		ID:            fmt.Sprintf("semantic-install-%d", s.nextID),
		Action:        strings.TrimSpace(input.Action),
		Label:         strings.TrimSpace(input.Label),
		Status:        semanticInstallJobStatusRunning,
		ModelID:       strings.TrimSpace(input.ModelID),
		VectorSpaceID: strings.TrimSpace(input.VectorSpaceID),
		StartedAt:     &now,
		Message:       strings.TrimSpace(input.Label) + " started.",
	}
	return cloneSemanticInstallJobStatus(s.latest), true
}

func (s *semanticInstallJobStore) complete(id string, result any, message string) {
	if s == nil {
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest.ID != id || s.latest.Status != semanticInstallJobStatusRunning {
		return
	}
	s.latest.Status = semanticInstallJobStatusComplete
	s.latest.FinishedAt = &now
	s.latest.Message = strings.TrimSpace(message)
	if s.latest.Message == "" {
		s.latest.Message = "Semantic install completed."
	}
	if result != nil {
		if encoded, err := json.Marshal(result); err == nil {
			s.latest.Result = encoded
		}
	}
}

func (s *semanticInstallJobStore) fail(id string, code string, message string) {
	if s == nil {
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest.ID != id || s.latest.Status != semanticInstallJobStatusRunning {
		return
	}
	s.latest.Status = semanticInstallJobStatusFailed
	s.latest.FinishedAt = &now
	s.latest.ErrorCode = strings.TrimSpace(code)
	s.latest.ErrorMessage = strings.TrimSpace(message)
	if s.latest.ErrorCode == "" {
		s.latest.ErrorCode = "semantic_install_failed"
	}
	if s.latest.ErrorMessage == "" {
		s.latest.ErrorMessage = "Semantic install failed."
	}
	s.latest.Message = s.latest.ErrorMessage
}

func cloneSemanticInstallJobStatus(status semanticInstallJobStatus) semanticInstallJobStatus {
	if status.StartedAt != nil {
		startedAt := *status.StartedAt
		status.StartedAt = &startedAt
	}
	if status.FinishedAt != nil {
		finishedAt := *status.FinishedAt
		status.FinishedAt = &finishedAt
	}
	if status.Result != nil {
		status.Result = append(json.RawMessage(nil), status.Result...)
	}
	return status
}

func (s *server) semanticInstallJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, "Use GET to inspect the semantic install job.")
		return
	}
	writeJSON(w, http.StatusOK, s.semanticInstallJobs.snapshot())
}

func (s *server) startSemanticInstallJob(w http.ResponseWriter, input semanticInstallJobStart, run func(context.Context) (any, string, error)) {
	job, started := s.semanticInstallJobs.start(input)
	writeJSON(w, http.StatusAccepted, job)
	if !started {
		return
	}
	go func(jobID string, action string) {
		ctx, cancel := context.WithTimeout(context.Background(), semanticInstallJobTimeout)
		defer cancel()
		result, message, err := run(ctx)
		if err != nil {
			code, friendly := semanticInstallJobError(action, err)
			s.semanticInstallJobs.fail(jobID, code, friendly)
			return
		}
		s.semanticInstallJobs.complete(jobID, result, message)
	}(job.ID, job.Action)
}

func (s *server) semanticInstallJobRunning() bool {
	return s != nil && s.semanticInstallJobs.snapshot().Status == semanticInstallJobStatusRunning
}

func semanticInstallJobError(action string, err error) (string, string) {
	switch {
	case errors.Is(err, catalog.ErrSemanticModelPackChecksumMismatch):
		return "semantic_model_checksum_mismatch", "Downloaded semantic model did not match its SHA-256 checksum."
	case errors.Is(err, catalog.ErrSemanticModelPackSizeMismatch):
		return "semantic_model_size_mismatch", "Downloaded semantic model did not match its expected size."
	case errors.Is(err, catalog.ErrSemanticModelPackInvalid):
		return "semantic_model_invalid", "Semantic model pack metadata is invalid."
	case errors.Is(err, catalog.ErrSemanticRuntimePackChecksumMismatch):
		return "semantic_runtime_pack_checksum_mismatch", "Downloaded semantic runtime pack did not match its SHA-256 checksum."
	case errors.Is(err, catalog.ErrSemanticRuntimePackSizeMismatch):
		return "semantic_runtime_pack_size_mismatch", "Downloaded semantic runtime pack did not match its expected size."
	case errors.Is(err, catalog.ErrSemanticRuntimePackInvalid):
		return "semantic_runtime_pack_invalid", "Semantic runtime pack metadata is invalid."
	}
	switch action {
	case "install_runtime":
		return "semantic_runtime_pack_install_failed", "Could not install the semantic runtime pack. " + err.Error()
	case "install_model":
		return "semantic_model_install_failed", "Could not install the semantic model pack. " + err.Error()
	default:
		return "semantic_install_failed", err.Error()
	}
}
