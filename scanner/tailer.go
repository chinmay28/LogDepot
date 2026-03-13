package scanner

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/logdepot/server/model"
)

const (
	pollInterval       = 250 * time.Millisecond
	readBufSize        = 32 * 1024
	reopenBackoff      = 1 * time.Second
	maxReopenWait      = 30 * time.Second
	checkpointInterval = 5 * time.Second
)

// ResumeState tells the tailer where to pick up from a previous run.
// If nil, the tailer seeks to the end of the file (fresh start).
type ResumeState struct {
	ByteOffset int64
	LineNumber int64
	Inode      uint64
}

// Tailer watches a single file for new lines, matches them against patterns,
// and sends hits to the notifier channel. It periodically reports its position
// via checkpointCh for persistence.
type Tailer struct {
	path         string
	jobID        string
	callbackURL  string
	matcher      *Matcher
	matchCh      chan<- model.InternalMatch
	matchCount   *atomic.Int64
	errCh        chan<- string
	checkpointCh chan<- model.TailerCheckpoint
	resume       *ResumeState
}

// NewTailer creates a Tailer for the given file path.
func NewTailer(
	path string,
	jobID string,
	callbackURL string,
	matcher *Matcher,
	matchCh chan<- model.InternalMatch,
	matchCount *atomic.Int64,
	errCh chan<- string,
	checkpointCh chan<- model.TailerCheckpoint,
	resume *ResumeState,
) *Tailer {
	return &Tailer{
		path:         path,
		jobID:        jobID,
		callbackURL:  callbackURL,
		matcher:      matcher,
		matchCh:      matchCh,
		matchCount:   matchCount,
		errCh:        errCh,
		checkpointCh: checkpointCh,
		resume:       resume,
	}
}

// Run opens the file, positions to the correct offset, and begins tailing.
// It runs until the context is cancelled. Handles log rotation and truncation.
func (t *Tailer) Run(ctx context.Context) {
	log := slog.With("file", t.path, "job_id", t.jobID)

	file, err := os.Open(t.path)
	if err != nil {
		log.Error("failed to open file", "error", err)
		t.reportError("open failed: " + err.Error())
		return
	}
	defer file.Close()

	var lineNum int64

	if t.resume != nil {
		lineNum = t.initResume(file, log)
	} else {
		lineNum = t.initFresh(file, log)
	}
	if lineNum < 0 {
		return // error already reported
	}

	log.Info("tailing started", "line_number", lineNum)

	readBuf := make([]byte, readBufSize)
	var partial []byte
	lastCheckpoint := time.Now()

	for {
		select {
		case <-ctx.Done():
			// Final checkpoint before exit.
			t.sendCheckpoint(file, lineNum)
			log.Info("tailing stopped")
			return
		default:
		}

		n, readErr := file.Read(readBuf)
		if n > 0 {
			data := readBuf[:n]
			if len(partial) > 0 {
				data = append(partial, data...)
				partial = nil
			}

			for {
				idx := bytes.IndexByte(data, '\n')
				if idx < 0 {
					partial = make([]byte, len(data))
					copy(partial, data)
					break
				}
				lineNum++
				line := string(data[:idx])
				data = data[idx+1:]
				t.processLine(line, lineNum)
			}
		}

		// Periodic checkpoint.
		if time.Since(lastCheckpoint) >= checkpointInterval {
			t.sendCheckpoint(file, lineNum)
			lastCheckpoint = time.Now()
		}

		if readErr != nil && readErr != io.EOF {
			log.Error("read error", "error", readErr)
			t.reportError("read error: " + readErr.Error())
			t.sendCheckpoint(file, lineNum)
			return
		}

		if readErr == io.EOF {
			rotated, newFile := t.checkRotation(ctx, file, log)
			if rotated && newFile != nil {
				file.Close()
				file = newFile
				partial = nil
				lineNum = 0
				t.sendCheckpoint(file, lineNum)
				log.Info("file rotated, reopened")
				continue
			}
			if rotated && newFile == nil {
				return // context cancelled while waiting
			}
		}

		select {
		case <-ctx.Done():
			t.sendCheckpoint(file, lineNum)
			return
		case <-time.After(pollInterval):
		}
	}
}

// initFresh positions at end-of-file for a brand new scan.
// Returns the existing line count, or -1 on error.
func (t *Tailer) initFresh(file *os.File, log *slog.Logger) int64 {
	lineNum, err := countLines(file)
	if err != nil {
		log.Error("failed to count existing lines", "error", err)
		t.reportError("line count failed: " + err.Error())
		return -1
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		log.Error("failed to seek to end", "error", err)
		t.reportError("seek failed: " + err.Error())
		return -1
	}
	return lineNum
}

// initResume validates the saved state and positions accordingly.
// Returns the starting line number, or -1 on error.
func (t *Tailer) initResume(file *os.File, log *slog.Logger) int64 {
	currentIno, err := inode(file)
	if err != nil {
		log.Error("failed to get inode", "error", err)
		t.reportError("inode check failed: " + err.Error())
		return -1
	}

	// If inode doesn't match, the file was rotated while we were down.
	// Start from the beginning of the new file.
	if currentIno != t.resume.Inode {
		log.Warn("inode mismatch on resume, file was rotated while down",
			"saved_inode", t.resume.Inode,
			"current_inode", currentIno,
		)
		return 0 // start from beginning, file position is already at 0
	}

	// Inode matches — check if file was truncated.
	info, err := file.Stat()
	if err != nil {
		log.Error("failed to stat file for resume", "error", err)
		t.reportError("stat failed: " + err.Error())
		return -1
	}

	if info.Size() < t.resume.ByteOffset {
		log.Warn("file smaller than saved offset, was truncated while down",
			"saved_offset", t.resume.ByteOffset,
			"current_size", info.Size(),
		)
		return 0 // start from beginning
	}

	// Seek to saved position.
	if _, err := file.Seek(t.resume.ByteOffset, io.SeekStart); err != nil {
		log.Error("failed to seek to resume offset", "error", err)
		t.reportError("resume seek failed: " + err.Error())
		return -1
	}

	log.Info("resuming from saved checkpoint",
		"byte_offset", t.resume.ByteOffset,
		"line_number", t.resume.LineNumber,
	)
	return t.resume.LineNumber
}

func (t *Tailer) processLine(line string, lineNum int64) {
	hits := t.matcher.Match(line)
	now := time.Now()
	for _, hit := range hits {
		t.matchCount.Add(1)
		t.matchCh <- model.InternalMatch{
			JobID:       t.jobID,
			CallbackURL: t.callbackURL,
			File:        t.path,
			LineNumber:  lineNum,
			Line:        line,
			PatternID:   hit.ID,
			MatchedAt:   now,
		}
	}
}

func (t *Tailer) sendCheckpoint(file *os.File, lineNum int64) {
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}
	ino, err := inode(file)
	if err != nil {
		return
	}
	select {
	case t.checkpointCh <- model.TailerCheckpoint{
		JobID:      t.jobID,
		File:       t.path,
		ByteOffset: offset,
		LineNumber: lineNum,
		Inode:      ino,
	}:
	default:
		// Don't block if the channel is full.
	}
}

func (t *Tailer) checkRotation(ctx context.Context, current *os.File, log *slog.Logger) (rotated bool, newFile *os.File) {
	currentIno, err := inode(current)
	if err != nil {
		return false, nil
	}

	pathIno, err := inodeByPath(t.path)
	if err != nil {
		log.Info("file disappeared, waiting for it to reappear")
		return true, t.waitForFile(ctx, log)
	}

	if currentIno != pathIno {
		return true, t.waitForFile(ctx, log)
	}

	// Check for truncation.
	info, err := current.Stat()
	if err != nil {
		return false, nil
	}
	offset, err := current.Seek(0, io.SeekCurrent)
	if err != nil {
		return false, nil
	}
	if info.Size() < offset {
		log.Info("file truncated, resetting to beginning")
		current.Seek(0, io.SeekStart)
	}

	return false, nil
}

func (t *Tailer) waitForFile(ctx context.Context, log *slog.Logger) *os.File {
	wait := reopenBackoff
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}

		f, err := os.Open(t.path)
		if err == nil {
			return f
		}

		log.Debug("waiting for file to reappear", "backoff", wait)
		wait *= 2
		if wait > maxReopenWait {
			wait = maxReopenWait
		}
	}
}

func (t *Tailer) reportError(msg string) {
	select {
	case t.errCh <- t.path + ": " + msg:
	default:
	}
}

// --- Helpers ---

func countLines(f *os.File) (int64, error) {
	orig, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}

	var count int64
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] == '\n' {
				count++
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}

	if _, err := f.Seek(orig, io.SeekStart); err != nil {
		return 0, err
	}
	return count, nil
}

func inode(f *os.File) (uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &stat); err != nil {
		return 0, err
	}
	return stat.Ino, nil
}

func inodeByPath(path string) (uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return 0, err
	}
	return stat.Ino, nil
}
