//go:build windows

package ipc

import (
	"io"
	"os"
	"time"
)

// SyncSpinFromEnv is a no-op on Windows (no poll on pipe handles).
func SyncSpinFromEnv() time.Duration { return 0 }

// NewSpinReadCloser returns f unchanged on Windows.
func NewSpinReadCloser(f *os.File, _ time.Duration) io.ReadCloser { return f }
