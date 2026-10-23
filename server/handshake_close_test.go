package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uacp"
)

// startCloseTestServer starts a server on a free port and returns it with its
// endpoint once it answers. The test closes it.
func startCloseTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	srv := New(EndPoint("127.0.0.1", port), EnableSecurity("None", ua.MessageSecurityModeNone))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, srv.Start(ctx))
	endpoint := srv.URLs()[0]
	requireAnswers(t, endpoint, 5*time.Second)
	return srv, endpoint
}

// closeWithin closes srv and fails unless Close returns within budget.
func closeWithin(t *testing.T, srv *Server, budget time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Close()
	}()
	select {
	case <-done:
	case <-time.After(budget):
		t.Fatalf("Close did not return within %s", budget)
	}
}

// requireClosedByServer reads c until the server has closed it and fails if
// that does not happen by deadline or if an Acknowledge arrives.
func requireClosedByServer(t *testing.T, c net.Conn, deadline time.Time) {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(deadline))
	b, err := io.ReadAll(c)
	if len(b) >= 4 {
		require.NotEqual(t, "ACKF", string(b[:4]), "the server acknowledged a Hello")
	}
	require.False(t, isTimeout(err), "the connection is still open")
}

// Close closes the connections that have not completed the handshake, without
// waiting for their handshake timeout, whether the client sent nothing or part
// of a Hello.
func TestServer_CloseClosesConnectionsInHandshake(t *testing.T) {
	srv, endpoint := startCloseTestServer(t)

	var conns []net.Conn
	for i := 0; i < 8; i++ {
		c, err := net.Dial("tcp", hostPort(endpoint))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		if i%2 == 1 {
			_, err = c.Write([]byte{'H', 'E', 'L', 'F', 64, 0, 0, 0})
			require.NoError(t, err)
		}
		conns = append(conns, c)
	}
	time.Sleep(100 * time.Millisecond) // let the server accept them first

	start := time.Now()
	closeWithin(t, srv, 2*time.Second)
	for _, c := range conns {
		requireClosedByServer(t, c, start.Add(2*time.Second))
	}
}

// A Hello that arrives after Close is not acknowledged.
func TestServer_NoAcknowledgeAfterClose(t *testing.T) {
	srv, endpoint := startCloseTestServer(t)

	tcp, err := net.Dial("tcp", hostPort(endpoint))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tcp.Close() })
	time.Sleep(100 * time.Millisecond) // let the server accept it first

	closeWithin(t, srv, 2*time.Second)

	c, err := uacp.NewConn(tcp.(*net.TCPConn), nil)
	require.NoError(t, err)
	if err := c.Send("HELF", &uacp.Hello{ReceiveBufSize: uacp.DefaultReceiveBufSize, SendBufSize: uacp.DefaultSendBufSize, EndpointURL: endpoint}); err != nil {
		return // the server closed the connection before the Hello
	}
	requireClosedByServer(t, tcp, time.Now().Add(2*time.Second))
}

// recordingLogger keeps the Warn messages.
type recordingLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *recordingLogger) Debug(string, ...any) {}
func (l *recordingLogger) Error(string, ...any) {}
func (l *recordingLogger) Info(string, ...any)  {}
func (l *recordingLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, fmt.Sprintf(msg, args...))
}

func (l *recordingLogger) warnings() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.warns...)
}

// startVerifierServer starts a server with opts on a free port and returns it,
// its endpoint and the cancel of the context passed to Start.
func startVerifierServer(t *testing.T, opts ...Option) (*Server, string, context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	srv := New(append([]Option{EndPoint("127.0.0.1", port), EnableSecurity("None", ua.MessageSecurityModeNone)}, opts...)...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, srv.Start(ctx))
	endpoint := srv.URLs()[0]
	requireAnswers(t, endpoint, 5*time.Second)
	return srv, endpoint, cancel
}

// handshakesOf returns the stacks of the goroutines inside a handshake of srv.
func handshakesOf(srv *Server) []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	frame := fmt.Sprintf("server.(*Server).handshakeAndRegister(%p", srv)
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, frame) && strings.Contains(g, "uacp.(*Listener).HandshakeContext") {
			out = append(out, g)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, budget time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(budget); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, budget)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func encodeMessage(t *testing.T, typ string, body any) []byte {
	t.Helper()
	b, err := ua.Encode(body)
	require.NoError(t, err)
	hdr, err := (&uacp.Header{MessageType: typ, ChunkType: 'F', MessageSize: uint32(len(b) + 8)}).Encode()
	require.NoError(t, err)
	return append(hdr, b...)
}

// Close returns only once the handshakes it ended are over: no goroutine of
// the server is still inside a handshake when it returns, and a stop logs no
// handshake failure for the connections it ended.
func TestServer_CloseWaitsForHandshakesAndLogsNoFailure(t *testing.T) {
	log := &recordingLogger{}
	srv, endpoint, _ := startVerifierServer(t, SetLogger(log))

	const n = 16
	var conns []net.Conn
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", hostPort(endpoint))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		conns = append(conns, c)
	}
	waitFor(t, "handshakes running", 2*time.Second, func() bool { return len(handshakesOf(srv)) >= n })

	closeWithin(t, srv, 2*time.Second)
	// Each connection is closed by the time Close returns: the read sees the
	// close at once instead of waiting.
	for i, c := range conns {
		require.NoError(t, c.SetReadDeadline(time.Now().Add(time.Millisecond)))
		_, err := c.Read(make([]byte, 1))
		require.False(t, isTimeout(err), "connection %d still open when Close returned", i)
	}
	require.Empty(t, handshakesOf(srv), "a handshake goroutine outlived Close")
	for _, w := range log.warnings() {
		require.NotContains(t, w, "handshake with", "Close logged a handshake it ended as failed")
	}
}

// The handshakes end with the context passed to Start as well, without Close.
func TestServer_StartContextEndsHandshakes(t *testing.T) {
	srv, endpoint, cancel := startVerifierServer(t)
	t.Cleanup(func() { _ = srv.Close() })

	c, err := net.Dial("tcp", hostPort(endpoint))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	waitFor(t, "handshake running", 2*time.Second, func() bool { return len(handshakesOf(srv)) == 1 })

	cancel()
	requireClosedByServer(t, c, time.Now().Add(2*time.Second))
}

// Close racing connections that arrive and send Hello: Close returns in time,
// and the race detector sees no unsynchronized use of the handshake WaitGroup.
func TestServer_CloseRacingNewConnections(t *testing.T) {
	for round := 0; round < 10; round++ {
		srv, endpoint, _ := startVerifierServer(t)
		hello := encodeMessage(t, "HEL", &uacp.Hello{ReceiveBufSize: uacp.DefaultReceiveBufSize, SendBufSize: uacp.DefaultSendBufSize, EndpointURL: endpoint})
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					c, err := net.DialTimeout("tcp", hostPort(endpoint), time.Second)
					if err != nil {
						continue
					}
					_, _ = c.Write(hello)
					_ = c.Close()
				}
			}()
		}
		time.Sleep(time.Duration(round%5) * time.Millisecond)
		closeWithin(t, srv, 2*time.Second)
		close(stop)
		wg.Wait()
		require.Empty(t, handshakesOf(srv), "round %d: a handshake goroutine outlived Close", round)
	}
}
