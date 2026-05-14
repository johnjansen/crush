package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// stallingLanguageModel is a fantasy.LanguageModel whose Stream returns a
// sequence that blocks forever (until ctx cancellation). Used to verify the
// stall watchdog cancels genCtx and Run surfaces ErrStreamStalled.
type stallingLanguageModel struct{}

func (stallingLanguageModel) Provider() string { return "test" }
func (stallingLanguageModel) Model() string    { return "stalling" }

func (stallingLanguageModel) Generate(ctx context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (stallingLanguageModel) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	// Return a seq that blocks on ctx and emits a StreamPartTypeError
	// containing ctx.Err() once cancelled. This mirrors how real HTTP
	// providers surface a mid-stream abort: fantasy converts the error
	// part into a Stream return value, and Run sees context.Canceled.
	seq := func(yield func(fantasy.StreamPart) bool) {
		<-ctx.Done()
		yield(fantasy.StreamPart{
			Type:  fantasy.StreamPartTypeError,
			Error: ctx.Err(),
		})
	}
	return seq, nil
}

func (stallingLanguageModel) GenerateObject(ctx context.Context, _ fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (stallingLanguageModel) StreamObject(ctx context.Context, _ fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	seq := func(yield func(fantasy.ObjectStreamPart) bool) {
		<-ctx.Done()
	}
	return seq, nil
}

// TestSessionAgent_StallDetection verifies that when the provider stream
// goes silent (the "spinner of doom" symptom from issues #2804/#770),
// the watchdog cancels the in-flight request and Run returns
// ErrStreamStalled instead of hanging forever.
func TestSessionAgent_StallDetection(t *testing.T) {
	// Use tiny timeouts so the test runs in well under a second.
	t.Cleanup(swapStreamIdleConstants(50*time.Millisecond, 5*time.Millisecond))

	env := testEnv(t)
	model := stallingLanguageModel{}
	agent := testSessionAgent(env, model, model, "test system prompt")

	sess, err := env.sessions.Create(t.Context(), "stall-test")
	require.NoError(t, err)

	// Pre-populate a message so Run doesn't kick off async title
	// generation against the same stalling small model (which would
	// also block on the parent ctx and hold up wg.Wait()).
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "previous turn"}},
	})
	require.NoError(t, err)

	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = agent.Run(t.Context(), SessionAgentCall{
			SessionID: sess.ID,
			Prompt:    "this prompt will stall",
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after stall watchdog should have fired")
	}

	require.ErrorIs(t, runErr, ErrStreamStalled, "stalled stream should surface as ErrStreamStalled")

	// And the assistant message must record the failure so the UI clears
	// the spinner — otherwise we've just renamed the bug.
	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var foundStallFinish bool
	for _, m := range msgs {
		if m.Role != message.Assistant {
			continue
		}
		fin := m.FinishPart()
		if fin == nil {
			continue
		}
		if fin.Reason == message.FinishReasonError && fin.Message == "Stream stalled" {
			foundStallFinish = true
			break
		}
	}
	require.True(t, foundStallFinish, "expected assistant message with stall finish reason; got %+v", msgs)

	// The session must not remain marked busy after Run returns.
	require.False(t, agent.IsSessionBusy(sess.ID), "session must release busy state after stall")
}

// TestSessionAgent_UserCancelStillReportsCancellation verifies that an
// explicit user cancel (not a stall) keeps reporting as cancelled rather
// than getting promoted to a stall error.
func TestSessionAgent_UserCancelStillReportsCancellation(t *testing.T) {
	// Long enough that the watchdog never fires during this test.
	t.Cleanup(swapStreamIdleConstants(10*time.Second, 1*time.Second))

	env := testEnv(t)
	model := stallingLanguageModel{}
	agent := testSessionAgent(env, model, model, "test system prompt")

	sess, err := env.sessions.Create(t.Context(), "cancel-test")
	require.NoError(t, err)

	// Pre-populate a message so Run doesn't kick off async title
	// generation against the same stalling small model.
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "previous turn"}},
	})
	require.NoError(t, err)

	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = agent.Run(t.Context(), SessionAgentCall{
			SessionID: sess.ID,
			Prompt:    "this prompt will be cancelled by the user",
		})
	}()

	// Give the stream a moment to start, then cancel as if the user
	// pressed ESC. Cancel ID's the active request via the agent's
	// cancel-by-session API rather than the parent ctx, mirroring the
	// real Cancel() code path.
	time.Sleep(50 * time.Millisecond)
	agent.Cancel(sess.ID)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Cancel")
	}

	require.True(t, errors.Is(runErr, context.Canceled), "user cancel should report context.Canceled, got: %v", runErr)
	require.NotErrorIs(t, runErr, ErrStreamStalled, "user cancel must not be reported as a stall")
}

// swapStreamIdleConstants temporarily replaces the package-level stall
// watchdog tuning so tests can drive it deterministically. The returned
// closure restores the originals — register it with t.Cleanup.
func swapStreamIdleConstants(timeout, tick time.Duration) func() {
	origTimeout, origTick := streamIdleTimeout, streamIdleTick
	streamIdleTimeout = timeout
	streamIdleTick = tick
	return func() {
		streamIdleTimeout = origTimeout
		streamIdleTick = origTick
	}
}
