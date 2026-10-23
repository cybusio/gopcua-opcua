package server

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHandshakeTimeoutOption(t *testing.T) {
	require.Equal(t, 10*time.Second, New().cfg.handshakeTimeout, "default")

	tests := []struct {
		name string
		opts []Option
		want time.Duration
	}{
		{"set", []Option{HandshakeTimeout(3 * time.Second)}, 3 * time.Second},
		{"zero ignored", []Option{HandshakeTimeout(0)}, defaultHandshakeTimeout},
		{"negative ignored", []Option{HandshakeTimeout(-time.Second)}, defaultHandshakeTimeout},
		{"zero keeps an earlier value", []Option{HandshakeTimeout(time.Second), HandshakeTimeout(0)}, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &serverConfig{handshakeTimeout: defaultHandshakeTimeout}
			for _, opt := range tt.opts {
				opt(cfg)
			}
			require.Equal(t, tt.want, cfg.handshakeTimeout)
		})
	}
}

// A client that does not complete its Hello within the handshake timeout is
// disconnected, whether it sends nothing or stalls part way through the Hello:
// the period runs from the accept and is not restarted by a partial read.
func TestServer_ClosesConnectionWithoutHelloAfterHandshakeTimeout(t *testing.T) {
	const timeout = 500 * time.Millisecond
	endpoint := startAcceptTestServer(t, HandshakeTimeout(timeout))

	tests := []struct {
		name string
		send []byte
	}{
		{"nothing", nil},
		{"Hello header only", []byte{'H', 'E', 'L', 'F', 64, 0, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := net.Dial("tcp", hostPort(endpoint))
			require.NoError(t, err)
			t.Cleanup(func() { _ = c.Close() })
			start := time.Now()
			if tt.send != nil {
				_, err := c.Write(tt.send)
				require.NoError(t, err)
			}

			// Still open before the timeout.
			require.NoError(t, c.SetReadDeadline(start.Add(timeout/2)))
			_, err = c.Read(make([]byte, 1))
			var ne net.Error
			require.ErrorAs(t, err, &ne, "the connection must stay open before the timeout")
			require.True(t, ne.Timeout(), "the connection must stay open before the timeout: %v", err)

			// Closed by the server after it, with at most an Error message.
			require.NoError(t, c.SetReadDeadline(start.Add(timeout+2*time.Second)))
			b := make([]byte, 1024)
			for {
				_, err = c.Read(b)
				if err != nil {
					break
				}
			}
			require.False(t, isTimeout(err), "the server did not close the connection after the handshake timeout")
			require.GreaterOrEqual(t, time.Since(start), timeout)
		})
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
