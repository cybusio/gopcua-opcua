package server

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uacp"
	"github.com/gopcua/opcua/uasc"
	"github.com/stretchr/testify/require"
)

// startLoopbackServer starts a SecurityPolicy#None server on an ephemeral
// loopback port and returns it together with the endpoint URL to dial.
func startLoopbackServer(t *testing.T) (*Server, string) {
	t.Helper()

	s := New(
		EndPoint("127.0.0.1", 0),
		EnableSecurity("None", ua.MessageSecurityModeNone),
		EnableAuthMode(ua.UserTokenTypeAnonymous),
	)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, s.Start(ctx))
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
	})
	return s, "opc.tcp://" + s.l.Addr().String()
}

func channelCount(s *Server) int {
	s.cb.mu.RLock()
	defer s.cb.mu.RUnlock()
	return len(s.cb.s)
}

// TestRegisterConnClosesTransportWhenChannelEnds checks that the server
// closes the transport connection as soon as a secure channel ends, whether
// the client sent CloseSecureChannel or simply closed its end of the TCP
// connection. Part 6 §7.1.5 requires the server to close the transport
// connection gracefully when it receives CloseSecureChannel, and §7.1.4 has it
// release all resources allocated for the channel once the client has closed
// its socket gracefully; the server's half of the connection is one of them. A
// server that leaves the socket open until the runtime finalizer reclaims it
// keeps the client waiting for the FIN and leaks a descriptor per connection
// in the meantime.
func TestRegisterConnClosesTransportWhenChannelEnds(t *testing.T) {
	// Well under the runtime's forced-GC period, which is what used to close
	// the socket eventually.
	const within = time.Second

	for _, tc := range []struct {
		name string
		// end terminates the channel from the client side and returns a
		// function that blocks until the server's FIN has been observed, or
		// fails the test after the deadline.
		end func(t *testing.T, endpoint string, conn *uacp.Conn) (waitForServerClose func())
	}{
		{
			name: "client sends CloseSecureChannel",
			end: func(t *testing.T, endpoint string, conn *uacp.Conn) func() {
				errch := make(chan error, 8)
				cfg := &uasc.Config{
					SecurityPolicyURI: ua.SecurityPolicyURINone,
					SecurityMode:      ua.MessageSecurityModeNone,
					Lifetime:          uint32(time.Minute / time.Millisecond),
					RequestTimeout:    within,
				}
				sc, err := uasc.NewSecureChannel(endpoint, conn, cfg, errch)
				require.NoError(t, err)

				ctx, cancel := context.WithTimeout(context.Background(), within)
				defer cancel()
				require.NoError(t, sc.Open(ctx))

				// Send CloseSecureChannel without tearing the channel down
				// locally: the channel's dispatcher keeps reading, so the
				// server's FIN surfaces as io.EOF on errch.
				require.NoError(t, sc.SendRequest(ctx, &ua.CloseSecureChannelRequest{}, nil, nil))

				return func() {
					select {
					case err := <-errch:
						require.ErrorIs(t, err, io.EOF)
					case <-time.After(within):
						t.Fatalf("server did not close the connection within %s of CloseSecureChannel", within)
					}
				}
			},
		},
		{
			name: "client half-closes the connection",
			end: func(t *testing.T, _ string, conn *uacp.Conn) func() {
				require.NoError(t, conn.TCPConn.CloseWrite())

				return func() {
					require.NoError(t, conn.TCPConn.SetReadDeadline(time.Now().Add(within)))
					_, err := conn.TCPConn.Read(make([]byte, 1))
					require.ErrorIs(t, err, io.EOF, "server did not close the connection within %s of the client's FIN", within)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, endpoint := startLoopbackServer(t)

			ctx, cancel := context.WithTimeout(context.Background(), within)
			defer cancel()
			conn, err := uacp.Dial(ctx, endpoint)
			require.NoError(t, err)
			defer conn.Close()

			require.Eventually(t, func() bool { return channelCount(s) == 1 }, within, 5*time.Millisecond,
				"server did not register the connection")

			waitForServerClose := tc.end(t, endpoint, conn)
			waitForServerClose()

			require.Eventually(t, func() bool { return channelCount(s) == 0 }, within, 5*time.Millisecond,
				"server did not drop the channel from the broker")
		})
	}
}
