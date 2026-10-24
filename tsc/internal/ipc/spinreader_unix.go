//go:build unix

package ipc

import (
	"io"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// SyncSpinFromEnv returns the spin budget configured with TSGO_SYNC_SPIN_US
// (microseconds), or 0 when spinning is disabled.
func SyncSpinFromEnv() time.Duration {
	v, err := strconv.Atoi(os.Getenv("TSGO_SYNC_SPIN_US"))
	if err != nil || v <= 0 {
		return 0
	}
	return time.Duration(v) * time.Microsecond
}

// spinReadCloser wraps the sync connection's input: before each blocking Read
// it polls the descriptor for readability for up to `spin`. Between the
// back-to-back requests of a synchronous client the wait is typically a few
// tens of microseconds; blocking immediately lets the CPU drop into a deep idle
// state whose exit latency (~190us for C6 on current Intel hosts) then dominates
// the round trip. Spinning briefly keeps the core in a shallow idle state at the
// cost of one otherwise idle core. Disabled unless TSGO_SYNC_SPIN_US is set.
type spinReadCloser struct {
	f    *os.File
	fd   int32
	spin time.Duration
}

// NewSpinReadCloser wraps f; spin <= 0 returns f unchanged.
func NewSpinReadCloser(f *os.File, spin time.Duration) io.ReadCloser {
	if spin <= 0 {
		return f
	}
	return &spinReadCloser{f: f, fd: int32(f.Fd()), spin: spin}
}

func (r *spinReadCloser) Read(p []byte) (int, error) {
	fds := []unix.PollFd{{Fd: r.fd, Events: unix.POLLIN}}
	deadline := time.Now().Add(r.spin)
	for {
		n, err := unix.Poll(fds, 0)
		if n > 0 || (err != nil && err != unix.EINTR) || time.Now().After(deadline) {
			break
		}
	}
	return r.f.Read(p)
}

func (r *spinReadCloser) Close() error {
	return r.f.Close()
}
