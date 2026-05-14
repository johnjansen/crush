package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStallWatchdog_FiresAfterIdleTimeout verifies the watchdog cancels the
// supplied context and flips the stalled flag after the configured idle
// window elapses without a ping.
func TestStallWatchdog_FiresAfterIdleTimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	var cancelled atomic.Bool
	wrapped := func() {
		cancelled.Store(true)
		cancel()
	}

	w := newStallWatchdog(ctx, wrapped, "test-session", 50*time.Millisecond, 5*time.Millisecond)
	t.Cleanup(w.stop)

	// Wait a bit longer than the timeout for the watchdog to fire.
	require.Eventually(t, func() bool {
		return w.didStall() && cancelled.Load()
	}, 500*time.Millisecond, 5*time.Millisecond, "watchdog should fire after idle timeout")

	require.ErrorIs(t, ctx.Err(), context.Canceled, "context should be cancelled by watchdog")
}

// TestStallWatchdog_DoesNotFireWhilePinged verifies that frequent pings keep
// the watchdog from cancelling the context.
func TestStallWatchdog_DoesNotFireWhilePinged(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var cancelled atomic.Bool
	wrapped := func() {
		cancelled.Store(true)
		cancel()
	}

	w := newStallWatchdog(ctx, wrapped, "test-session", 100*time.Millisecond, 10*time.Millisecond)
	t.Cleanup(w.stop)

	// Ping every 20ms for 250ms — well below the 100ms idle window.
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		w.ping()
		time.Sleep(20 * time.Millisecond)
	}

	require.False(t, w.didStall(), "watchdog should not fire while pings are arriving")
	require.False(t, cancelled.Load(), "cancel should not have been called")
}

// TestStallWatchdog_StopIsIdempotent verifies stop can be called multiple
// times safely and returns promptly.
func TestStallWatchdog_StopIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	w := newStallWatchdog(ctx, cancel, "test-session", 1*time.Second, 100*time.Millisecond)

	done := make(chan struct{})
	go func() {
		w.stop()
		w.stop()
		w.stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stop() did not return promptly across multiple calls")
	}
}

// TestStallWatchdog_DisabledWhenTimeoutZero verifies the watchdog is a
// no-op when configured with a non-positive timeout, so callers don't need
// to special-case the disabled path.
func TestStallWatchdog_DisabledWhenTimeoutZero(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var cancelled atomic.Bool
	wrapped := func() {
		cancelled.Store(true)
		cancel()
	}

	w := newStallWatchdog(ctx, wrapped, "test-session", 0, 10*time.Millisecond)
	t.Cleanup(w.stop)

	time.Sleep(100 * time.Millisecond)

	require.False(t, w.didStall(), "disabled watchdog must never fire")
	require.False(t, cancelled.Load(), "disabled watchdog must not cancel the context")
}

// TestStallWatchdog_NilSafePing verifies that ping is safe on a nil
// watchdog so callers can avoid nil-checks at every callback site.
func TestStallWatchdog_NilSafePing(t *testing.T) {
	t.Parallel()

	var w *stallWatchdog
	require.NotPanics(t, func() {
		w.ping()
		_ = w.didStall()
		w.stop()
	})
}
