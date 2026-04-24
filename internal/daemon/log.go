// Package daemon hosts the aichronicles ingest HTTP service and its
// append-only JSONL event log.
package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Logger appends one JSON value per line to a file. Safe for concurrent
// use: the mutex guarantees each event lands as one uninterrupted line
// even across goroutines.
type Logger struct {
	mu sync.Mutex
	f  *os.File
}

// OpenLogger opens or creates path in append mode with 0600 perms.
func OpenLogger(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", path, err)
	}
	return &Logger{f: f}, nil
}

// AppendJSON marshals v and writes a single line to the log. If marshal
// fails the log is untouched.
func (l *Logger) AppendJSON(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal log entry: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(line); err != nil {
		return fmt.Errorf("write log line: %w", err)
	}
	return nil
}

// Close releases the underlying file handle.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
