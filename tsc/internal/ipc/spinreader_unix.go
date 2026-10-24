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
// adaptive spin state in either direction, and adaptiveSlowWait the wait beyond
// which the peer counts as idle rather than merely paying wake-up latency (a
// blocked read on a host with deep idle states returns after ~200us even when
// the peer answered immediately, so the budget itself cannot be the criterion).
const (
	adaptiveWindow   = 3
	adaptiveSlowWait = time.Millisecond
)

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
		// peer is back to back-to-back messages, so resume spinning.
		start := time.Now()
		n, err := r.f.Read(p)
		if time.Since(start) < adaptiveSlowWait {
			r.hits++
			if r.hits >= adaptiveWindow {
				r.spinning, r.hits = true, 0
			}
		} else {
			r.hits = 0
		}
		return n, err
	}
	start := time.Now()
	fds := []unix.PollFd{{Fd: r.fd, Events: unix.POLLIN}}
	deadline := start.Add(r.spin)
	for {
		n, err := unix.Poll(fds, 0)
		if n > 0 || (err != nil && err != unix.EINTR) || time.Now().After(deadline) {
			break
		}
	}
	n, err := r.f.Read(p)
	if r.adaptive {
		// A spin that ran out AND was followed by a long wait means the peer is
		// idle between messages: spinning only burns CPU, so suspend it.
		if time.Since(start) > adaptiveSlowWait {
			r.misses++
			if r.misses >= adaptiveWindow {
				r.spinning, r.misses = false, 0
			}
		} else {
			r.misses = 0
		}
	}
	return n, err
}

func (r *spinReadCloser) Close() error {
	return r.f.Close()
}
