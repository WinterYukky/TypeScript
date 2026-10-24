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

// SyncSpinAdaptiveFromEnv reports whether the spin should adapt to the observed
// request cadence (TSGO_SYNC_SPIN_ADAPTIVE=1): spinning is suspended after
// consecutive budgets that elapsed without data (a client that is slow between
// requests) and resumed once blocking reads are again served within the budget.
func SyncSpinAdaptiveFromEnv() bool {
	return os.Getenv("TSGO_SYNC_SPIN_ADAPTIVE") == "1"
}

// adaptiveWindow is the number of consecutive observations that flips the
// adaptive spin state in either direction.
const adaptiveWindow = 3

// spinReadCloser wraps the sync connection's input: before each blocking Read
// it polls the descriptor for readability for up to `spin`. Between the
// back-to-back requests of a synchronous client the wait is typically a few
// tens of microseconds; blocking immediately lets the CPU drop into a deep idle
// state whose exit latency (~190us for C6 on current Intel hosts) then dominates
// the round trip. Spinning briefly keeps the core in a shallow idle state at the
// cost of one otherwise idle core. Disabled unless TSGO_SYNC_SPIN_US is set.
type spinReadCloser struct {
	f        *os.File
	fd       int32
	spin     time.Duration
	adaptive bool
	// adaptive state: spinning is the current mode; misses counts consecutive
	// spins that timed out, hits counts consecutive blocking reads that were
	// served within the budget.
	spinning bool
	misses   int
	hits     int
}

// NewSpinReadCloser wraps f; spin <= 0 returns f unchanged.
func NewSpinReadCloser(f *os.File, spin time.Duration, adaptive bool) io.ReadCloser {
	if spin <= 0 {
		return f
	}
	return &spinReadCloser{f: f, fd: int32(f.Fd()), spin: spin, adaptive: adaptive, spinning: true}
}

func (r *spinReadCloser) Read(p []byte) (int, error) {
	if r.adaptive && !r.spinning {
		// Blocking mode: time the wait; a few short waits in a row mean the
		// client is back to back-to-back requests, so resume spinning.
		start := time.Now()
		n, err := r.f.Read(p)
		if time.Since(start) < r.spin {
			r.hits++
			if r.hits >= adaptiveWindow {
				r.spinning, r.hits = true, 0
			}
		} else {
			r.hits = 0
		}
		return n, err
	}
	fds := []unix.PollFd{{Fd: r.fd, Events: unix.POLLIN}}
	deadline := time.Now().Add(r.spin)
	ready := false
	for {
		n, err := unix.Poll(fds, 0)
		if n > 0 {
			ready = true
			break
		}
		if (err != nil && err != unix.EINTR) || time.Now().After(deadline) {
			break
		}
	}
	if r.adaptive {
		if ready {
			r.misses = 0
		} else {
			r.misses++
			if r.misses >= adaptiveWindow {
				r.spinning, r.misses = false, 0
			}
		}
	}
	return r.f.Read(p)
}

func (r *spinReadCloser) Close() error {
	return r.f.Close()
}
