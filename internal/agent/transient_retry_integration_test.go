package agent

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// flakyEPIPEModel is a fantasy.LanguageModel whose Stream fails with a
// wrapped syscall.EPIPE on the first call and succeeds with a clean,
// finished stream on subsequent calls. Mirrors the Bedrock broken-pipe
// symptom users hit on flaky network transitions.
type flakyEPIPEModel struct {
	calls atomic.Int32
}

func (f *flakyEPIPEModel) Provider() string { return "test" }
func (f *flakyEPIPEModel) Model() string    { return "flaky-epipe" }

func (f *flakyEPIPEModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not used")
}

func (f *flakyEPIPEModel) Stream(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	n := f.calls.Add(1)
	if n == 1 {
		// Wrap EPIPE in net.OpError to match the actual production
		// shape (net/http surfaces broken-pipe writes this way).
		return nil, &net.OpError{
			Op:  "write",
			Net: "tcp",
			Err: syscall.EPIPE,
		}
	}
	// Subsequent calls return a clean one-shot stream that produces a
	// short text response and finishes.
	seq := func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "txt-1"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "txt-1", Delta: "ok recovered"})
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "txt-1"})
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
		})
	}
	return seq, nil
}

func (f *flakyEPIPEModel) GenerateObject(_ context.Context, _ fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not used")
}

func (f *flakyEPIPEModel) StreamObject(_ context.Context, _ fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not used")
}

// TestSessionAgent_TransientErrorAutoRetries verifies that a broken-pipe
// from the provider on the first attempt triggers exactly one auto-retry
// (RetryCount=1), the second attempt succeeds, and the user message is
// not duplicated. This regression-pins the Bedrock broken-pipe symptom.
func TestSessionAgent_TransientErrorAutoRetries(t *testing.T) {
	env := testEnv(t)
	model := &flakyEPIPEModel{}
	agent := testSessionAgent(env, model, model, "test system prompt")

	sess, err := env.sessions.Create(t.Context(), "transient-retry-test")
	require.NoError(t, err)

	// Pre-populate a message so Run doesn't kick off async title
	// generation against the same flaky small model (which would
	// produce a third Stream call and skew the assertion below).
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "previous turn"}},
	})
	require.NoError(t, err)

	done := make(chan struct{})
	var (
		runResult *fantasy.AgentResult
		runErr    error
	)
	go func() {
		defer close(done)
		runResult, runErr = agent.Run(t.Context(), SessionAgentCall{
			SessionID: sess.ID,
			Prompt:    "this prompt will EPIPE once",
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not complete after auto-retry")
	}

	require.NoError(t, runErr, "auto-retry should have produced a successful result")
	require.NotNil(t, runResult, "Run should return a result on successful retry")
	require.Equal(t, int32(2), model.calls.Load(), "Stream should be called exactly twice (1 fail, 1 success)")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	// Exactly one user message — RetryCount must suppress duplicate.
	var userCount int
	var assistantFinishes []string
	for _, m := range msgs {
		switch m.Role {
		case message.User:
			userCount++
		case message.Assistant:
			if fin := m.FinishPart(); fin != nil {
				assistantFinishes = append(assistantFinishes, fin.Message)
			}
		}
	}
	require.Equal(t, 2, userCount, "expected the seeded message + the test prompt; auto-retry must not duplicate the test prompt")

	// First assistant turn must record the dropped connection; second
	// must end normally.
	require.GreaterOrEqual(t, len(assistantFinishes), 2, "expected at least two assistant finishes (failed + retried)")
	require.Equal(t, "Connection dropped — retrying", assistantFinishes[0],
		"first turn should record the auto-retry; got finishes=%v", assistantFinishes)

	// Session should not be busy after Run returns.
	require.False(t, agent.IsSessionBusy(sess.ID), "session must release busy state after auto-retry success")
}
