package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Default rotation parameters
const (
	DefaultMaxBytes   = 1024 * 1024 // 1 MB
	DefaultMaxBackups = 3           // keep 3 old backup files (.1, .2, .3)
)

// RotateWriter is a log file writer with size-based rotation; it implements io.WriteCloser.
type RotateWriter struct {
	mu         sync.Mutex
	filename   string
	maxBytes   int64
	maxBackups int
	file       *os.File
	currSize   int64
}

var _ io.WriteCloser = (*RotateWriter)(nil)

// NewRotateWriter creates a rotating file writer.
// maxBytes <= 0 uses the default 1MB; maxBackups <= 0 uses the default 3 files.
func NewRotateWriter(filename string, maxBytes int64, maxBackups int) (*RotateWriter, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if maxBackups <= 0 {
		maxBackups = DefaultMaxBackups
	}

	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	w := &RotateWriter{
		filename:   filename,
		maxBytes:   maxBytes,
		maxBackups: maxBackups,
	}

	if err := w.openFile(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotateWriter) openFile() error {
	f, err := os.OpenFile(w.filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat log file: %w", err)
	}
	w.file = f
	w.currSize = info.Size()
	return nil
}

// Write implements io.Writer and checks whether rotation is needed first.
func (w *RotateWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	writeLen := int64(len(p))
	if w.currSize+writeLen > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}

	if w.file == nil {
		if err := w.openFile(); err != nil {
			return 0, err
		}
	}

	n, err = w.file.Write(p)
	w.currSize += int64(n)
	return n, err
}

// rotate performs the file split:
// file.3 <- file.2 <- file.1 <- file
func (w *RotateWriter) rotate() error {
	if w.file != nil {
		_ = w.file.Sync()
		_ = w.file.Close()
		w.file = nil
	}

	for i := w.maxBackups - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.filename, i)
		dst := fmt.Sprintf("%s.%d", w.filename, i+1)
		if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rotate log backup %s -> %s: %w", src, dst, err)
		}
	}

	if err := os.Rename(w.filename, fmt.Sprintf("%s.1", w.filename)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rotate log file %s: %w", w.filename, err)
	}
	w.currSize = 0
	return w.openFile()
}

// Sync flushes the buffer to disk.
func (w *RotateWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Sync()
	}
	return nil
}

// Close closes the file.
func (w *RotateWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}
