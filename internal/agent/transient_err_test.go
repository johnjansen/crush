package agent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsTransientConnError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("whatever"), false},
		{"raw EPIPE", syscall.EPIPE, true},
		{"raw ECONNRESET", syscall.ECONNRESET, true},
		{"raw EOF", io.EOF, true},
		{"raw ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"raw ErrClosedPipe", io.ErrClosedPipe, true},
		{"wrapped EPIPE", fmt.Errorf("write tcp: %w", syscall.EPIPE), true},
		{
			"net.OpError around EPIPE (matches Bedrock symptom)",
			&net.OpError{
				Op:  "write",
				Net: "tcp",
				Err: syscall.EPIPE,
			},
			true,
		},
		{
			"deeply nested ECONNRESET",
			fmt.Errorf("outer: %w", fmt.Errorf("middle: %w", syscall.ECONNRESET)),
			true,
		},
		{
			"net.OpError around random error stays untransient",
			&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("nope")},
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isTransientConnError(tc.err))
		})
	}
}
