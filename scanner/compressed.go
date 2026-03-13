package scanner

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/logdepot/server/model"
)

// ScanCompressedFile decompresses a .gz or .zst file and scans it line by line,
// sending any pattern matches to the match channel.
func ScanCompressedFile(
	ctx context.Context,
	path string,
	jobID string,
	callbackURL string,
	matcher *Matcher,
	matchCh chan<- model.InternalMatch,
	matchCount *atomic.Int64,
) error {
	log := slog.With("file", path, "job_id", jobID)

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	reader, cleanup, err := decompressReader(f, path)
	if err != nil {
		return err
	}
	defer cleanup()

	sc := bufio.NewScanner(reader)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lineNum int64
	for sc.Scan() {
		if lineNum%1000 == 0 {
			select {
			case <-ctx.Done():
				log.Info("compressed scan cancelled", "lines_scanned", lineNum)
				return ctx.Err()
			default:
			}
		}

		lineNum++
		line := sc.Text()

		hits := matcher.Match(line)
		now := time.Now()
		for _, hit := range hits {
			matchCount.Add(1)
			matchCh <- model.InternalMatch{
				JobID:       jobID,
				CallbackURL: callbackURL,
				File:        path,
				LineNumber:  lineNum,
				Line:        line,
				PatternID:   hit.ID,
				MatchedAt:   now,
			}
		}
	}

	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", path, err)
	}

	log.Info("compressed scan complete", "lines_scanned", lineNum)
	return nil
}

func decompressReader(f *os.File, path string) (io.Reader, func(), error) {
	switch {
	case strings.HasSuffix(path, ".gz"):
		gr, err := gzip.NewReader(f)
		if err != nil {
			return nil, nil, fmt.Errorf("gzip open %s: %w", path, err)
		}
		return gr, func() { gr.Close() }, nil

	case strings.HasSuffix(path, ".zst"):
		zr, err := zstd.NewReader(f)
		if err != nil {
			return nil, nil, fmt.Errorf("zstd open %s: %w", path, err)
		}
		return zr, func() { zr.Close() }, nil

	default:
		return nil, nil, fmt.Errorf("unsupported compressed format: %s", path)
	}
}
