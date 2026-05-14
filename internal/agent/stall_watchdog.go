package agent

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// stallWatchdog monitors LLM-stream activity. Every stream callback that
// signals real progress (text/reasoning/tool delta, tool call, tool result,
// step finish, provider retry) is expected to call ping. If no ping arrives
// for idleTimeout, the watchdog cancels the supplied context so the in-flight
// fantasy.Stream returns context.Canceled and Run can surface a recoverable
// error instead of leaving the spinner spinning forever.
//
// The watchdog is safe to use from a single goroutine plus N stream callbacks
// concurrently; ping is lock-free.
type stallWatchdog struct {
	cancel       context.CancelFunc
	stalled      atomic.Bool
	lastActivity atomic.Int64 // unix nanos
	sessionID    string
	idleTimeout  time.Duration
	idleTick     time.Duration

	// nowFn lets tests substitute a deterministic clock. Production code
	// leaves this nil so the watchdog uses time.Now.
	nowFn func() time.Time

	doneCh chan struct{} // closed when the watchdog goroutine exits
	stopCh chan struct{} // signal to stop the watchdog
}

// newStallWatchdog spawns a watchdog goroutine bound to ctx. The returned
// watchdog must be stopped (via stop) once the stream completes. Passing a
// non-positive idleTimeout disables the watchdog entirely; ping becomes a
// no-op and the goroutine exits immediately. This keeps callers simple —
// no nil-check at the call sites.
func newStallWatchdog(ctx context.Context, cancel context.CancelFunc, sessionID string, idleTimeout, idleTick time.Duration) *stallWatchdog {
	w := &stallWatchdog{
		cancel:      cancel,
		sessionID:   sessionID,
		idleTimeout: idleTimeout,
		idleTick:    idleTick,
		doneCh:      make(chan struct{}),
		stopCh:      make(chan struct{}),
	}
	w.ping()
	if idleTimeout <= 0 || idleTick <= 0 {
		close(w.doneCh)
		return w
	}
	go w.run(ctx)
	return w
}

// ping resets the inactivity timer. Cheap enough to call from every stream
// callback without measurable overhead.
func (w *stallWatchdog) ping() {
	if w == nil {
		return
	}
	w.lastActivity.Store(w.now().UnixNano())
}

// didStall reports whether the watchdog has fired (i.e. cancelled the context
// because the stream went silent).
func (w *stallWatchdog) didStall() bool {
	if w == nil {
		return false
	}
	return w.stalled.Load()
}

// stop terminates the watchdog goroutine and blocks until it exits. Safe to
// call multiple times.
func (w *stallWatchdog) stop() {
	if w == nil {
		return
	}
	select {
	case <-w.stopCh:
		// Already stopped.
	default:
		close(w.stopCh)
	}
	<-w.doneCh
}

func (w *stallWatchdog) run(ctx context.Context) {
	defer close(w.doneCh)

	ticker := time.NewTicker(w.idleTick)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := time.Unix(0, w.lastActivity.Load())
			if w.now().Sub(last) < w.idleTimeout {
				continue
			}
			// Stream has been silent for longer than the timeout —
			// flip the flag first so the error block can distinguish
			// stall from a real user cancel, then cancel the context.
			w.stalled.Store(true)
			slog.Warn("LLM stream stalled; cancelling session context",
				"session_id", w.sessionID,
				"idle_for", w.now().Sub(last).String(),
				"timeout", w.idleTimeout.String(),
			)
			w.cancel()
			return
		}
	}
}

func (w *stallWatchdog) now() time.Time {
	if w.nowFn != nil {
		return w.nowFn()
	}
	return time.Now()
}
