package server

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uacp"
)

// requireErrorThenClose reads c until the server closes it and requires
// exactly one Error message with code before the close, and the close before
// deadline.
func requireErrorThenClose(t *testing.T, c net.Conn, code ua.StatusCode, deadline time.Time) {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(deadline))
	b, err := io.ReadAll(c) // a reset after the Error counts as the close
	require.False(t, isTimeout(err), "the connection is still open (read %q)", b)
	require.GreaterOrEqual(t, len(b), 8, "no Error message before the close")
	var h uacp.Header
	_, err = h.Decode(b)
	require.NoError(t, err)
	require.Equal(t, "ERRF", h.MessageType+string(h.ChunkType))
	require.EqualValues(t, len(b), h.MessageSize, "one Error message and nothing else")
	var e uacp.Error
	_, err = e.Decode(b[8:])
	require.NoError(t, err)
	require.Equal(t, code, ua.StatusCode(e.ErrorCode))
}

// A ReverseHello is sent by a server to a client (Part 6 §7.1.2.6); a server
// that receives one as the first message answers Bad_TcpMessageTypeInvalid
// and closes the connection (Part 6 §7.1.5) at once, without waiting for the
// handshake timeout and without connecting to the ServerUri it names. Other
// clients are served meanwhile, and no handshake is left behind.
func TestServer_RejectsReverseHello(t *testing.T) {
	srv, endpoint, _ := startVerifierServer(t, HandshakeTimeout(time.Minute))
	back, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = back.Close() })

	for _, ep := range []string{endpoint, "opc.tcp://192.0.2.1:4840"} {
		c, err := net.Dial("tcp", hostPort(endpoint))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		_, err = c.Write(encodeMessage(t, "RHE", &uacp.ReverseHello{ServerURI: back.Addr().String(), EndpointURL: ep}))
		require.NoError(t, err)
		requireErrorThenClose(t, c, ua.StatusBadTCPMessageTypeInvalid, time.Now().Add(time.Second))
	}

	requireAnswers(t, endpoint, 3*time.Second)
	waitFor(t, "rejected handshakes over", time.Second, func() bool { return len(handshakesOf(srv)) == 0 })
	require.NoError(t, back.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second)))
	if peer, err := back.Accept(); err == nil {
		_ = peer.Close()
		t.Fatal("the server connected to the ServerUri of a ReverseHello")
	}
}
