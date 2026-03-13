package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/logdepot/server/model"
	"github.com/logdepot/server/scanner"
)

// Handler holds the scanner manager and exposes HTTP endpoints.
type Handler struct {
	mgr *scanner.Manager
}

// New creates a Handler backed by the given manager.
func New(mgr *scanner.Manager) *Handler {
	return &Handler{mgr: mgr}
}

// Routes returns a Chi router with all scan endpoints registered.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()

	r.Post("/scan/start", h.StartScan)
	r.Post("/scan/stop/{jobID}", h.StopScan)
	r.Post("/scan/stop", h.StopAll)
	r.Get("/scan/status", h.AllStatus)
	r.Get("/scan/status/{jobID}", h.JobStatusByID)
	r.Post("/scan/compressed", h.CompressedScan)
	r.Delete("/scan/jobs/{jobID}", h.RemoveJob)
	r.Get("/health", h.Health)

	return r
}

// POST /scan/start
func (h *Handler) StartScan(w http.ResponseWriter, r *http.Request) {
	var req model.StartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.JobID == "" {
		writeError(w, http.StatusBadRequest, "job_id is required")
		return
	}
	if req.CallbackURL == "" {
		writeError(w, http.StatusBadRequest, "callback_url is required")
		return
	}
	if len(req.Patterns) == 0 {
		writeError(w, http.StatusBadRequest, "at least one pattern is required")
		return
	}
	if len(req.Files) == 0 {
		writeError(w, http.StatusBadRequest, "at least one file is required")
		return
	}

	if err := h.mgr.StartTailJob(req); err != nil {
		// Distinguish "already exists" from other errors.
		code := http.StatusInternalServerError
		if isConflict(err) {
			code = http.StatusConflict
		}
		writeError(w, code, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, model.SuccessResponse{
		Message: "scan started",
		JobID:   req.JobID,
	})
}

// POST /scan/stop/{jobID}
func (h *Handler) StopScan(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobID")
	if jobID == "" {
		writeError(w, http.StatusBadRequest, "job_id is required")
		return
	}

	if err := h.mgr.StopJob(jobID); err != nil {
		code := http.StatusNotFound
		writeError(w, code, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, model.SuccessResponse{
		Message: "scan stopped",
		JobID:   jobID,
	})
}

// POST /scan/stop — stops all jobs.
func (h *Handler) StopAll(w http.ResponseWriter, r *http.Request) {
	h.mgr.StopAll()
	writeJSON(w, http.StatusOK, model.SuccessResponse{
		Message: "all scans stopped",
	})
}

// GET /scan/status
func (h *Handler) AllStatus(w http.ResponseWriter, r *http.Request) {
	statuses := h.mgr.AllJobStatuses()
	writeJSON(w, http.StatusOK, model.StatusResponse{Jobs: statuses})
}

// GET /scan/status/{jobID}
func (h *Handler) JobStatusByID(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobID")

	status, err := h.mgr.JobStatus(jobID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, status)
}

// POST /scan/compressed
func (h *Handler) CompressedScan(w http.ResponseWriter, r *http.Request) {
	var req model.CompressedScanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.JobID == "" {
		writeError(w, http.StatusBadRequest, "job_id is required")
		return
	}
	if req.CallbackURL == "" {
		writeError(w, http.StatusBadRequest, "callback_url is required")
		return
	}
	if len(req.Patterns) == 0 {
		writeError(w, http.StatusBadRequest, "at least one pattern is required")
		return
	}
	if len(req.Files) == 0 {
		writeError(w, http.StatusBadRequest, "at least one file is required")
		return
	}

	if err := h.mgr.StartCompressedJob(req); err != nil {
		code := http.StatusInternalServerError
		if isConflict(err) {
			code = http.StatusConflict
		}
		writeError(w, code, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, model.SuccessResponse{
		Message: "compressed scan started",
		JobID:   req.JobID,
	})
}

// DELETE /scan/jobs/{jobID}
func (h *Handler) RemoveJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobID")

	if err := h.mgr.RemoveJob(jobID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, model.SuccessResponse{
		Message: "job removed",
		JobID:   jobID,
	})
}

// GET /health
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, model.ErrorResponse{Error: msg})
}

func isConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}
