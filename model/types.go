package model

import "time"

// --- Requests ---

type Pattern struct {
	ID          string `json:"id"`
	Regex       string `json:"regex"`
	Description string `json:"description,omitempty"`
}

type StartRequest struct {
	JobID       string    `json:"job_id"`
	CallbackURL string    `json:"callback_url"`
	Patterns    []Pattern `json:"patterns"`
	Files       []string  `json:"files"`
}

type CompressedScanRequest struct {
	JobID       string    `json:"job_id"`
	CallbackURL string    `json:"callback_url"`
	Patterns    []Pattern `json:"patterns"`
	Files       []string  `json:"files"`
}

// --- Callback payload ---

type MatchDetail struct {
	File       string `json:"file"`
	LineNumber int64  `json:"line_number"`
	Line       string `json:"line"`
	PatternID  string `json:"pattern_id"`
	MatchedAt  string `json:"matched_at"`
}

type MatchEvent struct {
	JobID    string      `json:"job_id"`
	Hostname string      `json:"hostname"`
	Match    MatchDetail `json:"match"`
}

// --- Responses ---

type JobStatus struct {
	JobID      string   `json:"job_id"`
	Type       string   `json:"type"`  // "tail" or "compressed"
	State      string   `json:"state"` // "running", "completed", "stopped", "error"
	Files      []string `json:"files"`
	MatchCount int64    `json:"match_count"`
	StartedAt  string   `json:"started_at"`
	Errors     []string `json:"errors,omitempty"`
}

type StatusResponse struct {
	Jobs []JobStatus `json:"jobs"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type SuccessResponse struct {
	Message string `json:"message"`
	JobID   string `json:"job_id,omitempty"`
}

// --- Recovery callback payload ---

// RecoveryFileInfo describes the resume state of a single file after restart.
type RecoveryFileInfo struct {
	File           string `json:"file"`
	ResumeFromLine int64  `json:"resume_from_line"`
	ResumeFromByte int64  `json:"resume_from_byte"`
	FileRotated    bool   `json:"file_rotated"`
	Note           string `json:"note,omitempty"`
}

// RecoveryEvent is POSTed to the callback URL when a server restarts and
// resumes a previously running scan job.
type RecoveryEvent struct {
	EventType  string             `json:"event_type"` // "server_recovered"
	JobID      string             `json:"job_id"`
	Hostname   string             `json:"hostname"`
	DownSince  string             `json:"down_since"`  // last checkpoint time
	RecoveredAt string            `json:"recovered_at"`
	Files      []RecoveryFileInfo `json:"files"`
}

// --- Internal ---

// InternalMatch is sent from tailers/scanners to the notifier goroutine.
type InternalMatch struct {
	JobID       string
	CallbackURL string
	File        string
	LineNumber  int64
	Line        string
	PatternID   string
	MatchedAt   time.Time
}

// TailerCheckpoint is reported by each tailer so the manager can persist progress.
type TailerCheckpoint struct {
	JobID      string
	File       string
	ByteOffset int64
	LineNumber int64
	Inode      uint64
}
