package handler

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/logdepot/server/model"
	"github.com/logdepot/server/scanner"
	"github.com/logdepot/server/state"
)

func testHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	store, _ := state.NewStore(filepath.Join(dir, "state"))
	mgr, err := scanner.NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.Shutdown() })
	return New(mgr), dir
}

func doRequest(t *testing.T, h *Handler, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)
	return rr
}

func TestHandler_Health(t *testing.T) {
	h, _ := testHandler(t)
	rr := doRequest(t, h, "GET", "/health", nil)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestHandler_StartScan(t *testing.T) {
	h, dir := testHandler(t)

	logPath := filepath.Join(dir, "app.log")
	os.WriteFile(logPath, []byte(""), 0644)

	req := model.StartRequest{
		JobID:       "job1",
		CallbackURL: "http://localhost:9090/cb",
		Patterns:    []model.Pattern{{ID: "p1", Regex: "ERROR"}},
		Files:       []string{logPath},
	}

	rr := doRequest(t, h, "POST", "/scan/start", req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp model.SuccessResponse
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp.JobID != "job1" {
		t.Errorf("expected job1, got %s", resp.JobID)
	}
}

func TestHandler_StartScan_MissingFields(t *testing.T) {
	h, _ := testHandler(t)

	tests := []struct {
		name string
		req  model.StartRequest
	}{
		{"no job_id", model.StartRequest{CallbackURL: "http://x", Patterns: []model.Pattern{{ID: "p1", Regex: "E"}}, Files: []string{"/tmp/x"}}},
		{"no callback", model.StartRequest{JobID: "j", Patterns: []model.Pattern{{ID: "p1", Regex: "E"}}, Files: []string{"/tmp/x"}}},
		{"no patterns", model.StartRequest{JobID: "j", CallbackURL: "http://x", Files: []string{"/tmp/x"}}},
		{"no files", model.StartRequest{JobID: "j", CallbackURL: "http://x", Patterns: []model.Pattern{{ID: "p1", Regex: "E"}}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := doRequest(t, h, "POST", "/scan/start", tc.req)
			if rr.Code != 400 {
				t.Errorf("expected 400, got %d", rr.Code)
			}
		})
	}
}

func TestHandler_StartScan_Conflict(t *testing.T) {
	h, dir := testHandler(t)

	logPath := filepath.Join(dir, "app.log")
	os.WriteFile(logPath, []byte(""), 0644)

	req := model.StartRequest{
		JobID:       "dup",
		CallbackURL: "http://localhost/cb",
		Patterns:    []model.Pattern{{ID: "p1", Regex: "E"}},
		Files:       []string{logPath},
	}

	doRequest(t, h, "POST", "/scan/start", req)
	rr := doRequest(t, h, "POST", "/scan/start", req)
	if rr.Code != 409 {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandler_StopScan(t *testing.T) {
	h, dir := testHandler(t)

	logPath := filepath.Join(dir, "app.log")
	os.WriteFile(logPath, []byte(""), 0644)

	doRequest(t, h, "POST", "/scan/start", model.StartRequest{
		JobID: "s1", CallbackURL: "http://localhost/cb",
		Patterns: []model.Pattern{{ID: "p1", Regex: "E"}},
		Files:    []string{logPath},
	})

	rr := doRequest(t, h, "POST", "/scan/stop/s1", nil)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestHandler_StopScan_NotFound(t *testing.T) {
	h, _ := testHandler(t)
	rr := doRequest(t, h, "POST", "/scan/stop/nonexistent", nil)
	if rr.Code != 404 {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestHandler_Status(t *testing.T) {
	h, dir := testHandler(t)

	logPath := filepath.Join(dir, "app.log")
	os.WriteFile(logPath, []byte(""), 0644)

	doRequest(t, h, "POST", "/scan/start", model.StartRequest{
		JobID: "st1", CallbackURL: "http://localhost/cb",
		Patterns: []model.Pattern{{ID: "p1", Regex: "E"}},
		Files:    []string{logPath},
	})

	rr := doRequest(t, h, "GET", "/scan/status", nil)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp model.StatusResponse
	json.NewDecoder(rr.Body).Decode(&resp)
	if len(resp.Jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(resp.Jobs))
	}
}

func TestHandler_StatusByID(t *testing.T) {
	h, dir := testHandler(t)

	logPath := filepath.Join(dir, "app.log")
	os.WriteFile(logPath, []byte(""), 0644)

	doRequest(t, h, "POST", "/scan/start", model.StartRequest{
		JobID: "sid1", CallbackURL: "http://localhost/cb",
		Patterns: []model.Pattern{{ID: "p1", Regex: "E"}},
		Files:    []string{logPath},
	})

	rr := doRequest(t, h, "GET", "/scan/status/sid1", nil)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var status model.JobStatus
	json.NewDecoder(rr.Body).Decode(&status)
	if status.JobID != "sid1" {
		t.Errorf("expected sid1, got %s", status.JobID)
	}
}

func TestHandler_StatusByID_NotFound(t *testing.T) {
	h, _ := testHandler(t)
	rr := doRequest(t, h, "GET", "/scan/status/nope", nil)
	if rr.Code != 404 {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestHandler_StopAll(t *testing.T) {
	h, dir := testHandler(t)

	log1 := filepath.Join(dir, "a.log")
	log2 := filepath.Join(dir, "b.log")
	os.WriteFile(log1, []byte(""), 0644)
	os.WriteFile(log2, []byte(""), 0644)

	doRequest(t, h, "POST", "/scan/start", model.StartRequest{
		JobID: "a1", CallbackURL: "http://localhost/cb",
		Patterns: []model.Pattern{{ID: "p1", Regex: "E"}}, Files: []string{log1},
	})
	doRequest(t, h, "POST", "/scan/start", model.StartRequest{
		JobID: "a2", CallbackURL: "http://localhost/cb",
		Patterns: []model.Pattern{{ID: "p1", Regex: "E"}}, Files: []string{log2},
	})

	rr := doRequest(t, h, "POST", "/scan/stop", nil)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestHandler_RemoveJob(t *testing.T) {
	h, dir := testHandler(t)

	logPath := filepath.Join(dir, "app.log")
	os.WriteFile(logPath, []byte(""), 0644)

	doRequest(t, h, "POST", "/scan/start", model.StartRequest{
		JobID: "rm1", CallbackURL: "http://localhost/cb",
		Patterns: []model.Pattern{{ID: "p1", Regex: "E"}}, Files: []string{logPath},
	})
	doRequest(t, h, "POST", "/scan/stop/rm1", nil)

	rr := doRequest(t, h, "DELETE", "/scan/jobs/rm1", nil)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandler_CompressedScan(t *testing.T) {
	h, dir := testHandler(t)

	// Write a .gz file by just creating the file — we need the import.
	// Since handler tests focus on HTTP layer, we just need the file to exist
	// with valid gzip content.
	logPath := filepath.Join(dir, "test.log.gz")
	// Create a minimal gzip file.
	createMinimalGzip(t, logPath)

	req := model.CompressedScanRequest{
		JobID:       "comp1",
		CallbackURL: "http://localhost/cb",
		Patterns:    []model.Pattern{{ID: "p1", Regex: "ERROR"}},
		Files:       []string{logPath},
	}

	rr := doRequest(t, h, "POST", "/scan/compressed", req)
	if rr.Code != 202 {
		t.Fatalf("expected 202, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandler_InvalidJSON(t *testing.T) {
	h, _ := testHandler(t)

	req := httptest.NewRequest("POST", "/scan/start", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)

	if rr.Code != 400 {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// createMinimalGzip creates a minimal valid gzip file for handler tests.
func createMinimalGzip(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gw, _ := gzip.NewWriter(f)
	gw.Write([]byte("INFO: test line\nERROR: test error\n"))
	gw.Close()
}
