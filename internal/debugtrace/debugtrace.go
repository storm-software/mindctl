// Package debugtrace creates the opt-in, content-safe router trace sink.
package debugtrace

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Trace owns one JSONL log file for a gateway process.
type Trace struct {
	file   *os.File
	logger *slog.Logger
	path   string
}

// Open creates a private per-process trace file in the user cache directory.
func Open() (*Trace, error) {
	cacheHome, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user cache directory: %w", err)
	}
	directory := filepath.Join(cacheHome, "mindctl", "logs")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, fmt.Errorf("create debug log directory: %w", err)
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, fmt.Errorf("secure debug log directory: %w", err)
	}
	name := fmt.Sprintf("mindctl-%s-%d.jsonl", time.Now().UTC().Format("20060102T150405.000000000Z"), os.Getpid())
	path := filepath.Join(directory, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("create debug log: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &Trace{file: file, logger: logger, path: path}, nil
}

// Logger returns the structured debug logger backed by the trace file.
func (t *Trace) Logger() *slog.Logger { return t.logger }

// Path returns the trace file path.
func (t *Trace) Path() string { return t.path }

// Close flushes and closes the trace file.
func (t *Trace) Close() error { return t.file.Close() }
