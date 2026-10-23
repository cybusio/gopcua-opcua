// Copyright 2018-2020 opcua authors. All rights reserved.
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

package uacp

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gopcua/opcua/ua"
)

func TestListener_HandshakeTimesOutOnASilentClient(t *testing.T) {
	ctx := context.Background()
	l, err := Listen(ctx, "opc.tcp://127.0.0.1:0", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	c, err := net.Dial("tcp", l.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	tc, err := l.AcceptTCP()
	require.NoError(t, err)

	start := time.Now()
	_, err = l.Handshake(tc, 200*time.Millisecond)
	require.Error(t, err, "a client that never sends Hello must not complete the handshake")
	require.Less(t, time.Since(start), 5*time.Second, "the timeout must end the handshake")
}

func TestListener_HandshakeClearsTheDeadline(t *testing.T) {
	ctx := context.Background()
	l, err := Listen(ctx, "opc.tcp://127.0.0.1:0", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	dialed := make(chan *Conn, 1)
	go func() {
		c, err := Dial(ctx, "opc.tcp://"+l.Addr().String())
		if err != nil {
			t.Errorf("dial: %v", err)
		}
		dialed <- c
	}()

	tc, err := l.AcceptTCP()
	require.NoError(t, err)
	srv, err := l.Handshake(tc, 200*time.Millisecond)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	cli := <-dialed
	require.NotNil(t, cli)
	t.Cleanup(func() { _ = cli.Close() })

	// Past the handshake timeout, the established connection must still read.
	time.Sleep(400 * time.Millisecond)
	go func() { _, _ = cli.Write([]byte("ping")) }()
	b := make([]byte, 4)
	_, err = srv.Read(b)
	require.NoError(t, err, "the handshake deadline must not outlive the handshake")
	require.Equal(t, "ping", string(b))
}

// helloBytes returns an encoded Hello message.
func helloBytes(t *testing.T, endpoint string) []byte {
	t.Helper()
	body, err := ua.Encode(&Hello{ReceiveBufSize: DefaultReceiveBufSize, SendBufSize: DefaultSendBufSize, EndpointURL: endpoint})
	require.NoError(t, err)
	hdr, err := (&Header{MessageType: "HEL", ChunkType: 'F', MessageSize: uint32(len(body) + hdrlen)}).Encode()
	require.NoError(t, err)
	return append(hdr, body...)
}

// requireClosedWithoutAck reads c until the server closes it and fails if an
// Acknowledge arrived first.
func requireClosedWithoutAck(t *testing.T, c net.Conn, deadline time.Time) {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(deadline))
	b, err := io.ReadAll(c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the connection is still open")
	}
	if len(b) >= 4 {
		require.NotEqual(t, "ACKF", string(b[:4]), "an Acknowledge was sent after the context was done")
	}
}

func TestListener_HandshakeContextClosesTheConnectionWhenDone(t *testing.T) {
	l, err := Listen(context.Background(), "opc.tcp://127.0.0.1:0", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	c, err := net.Dial("tcp", l.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	tc, err := l.AcceptTCP()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := l.HandshakeContext(ctx, tc, time.Minute)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the handshake wait for Hello
	cancel()

	select {
	case err := <-done:
		require.Error(t, err, "the handshake must fail once the context is done")
	case <-time.After(2 * time.Second):
		t.Fatal("the handshake did not end when the context was done")
	}
	requireClosedWithoutAck(t, c, time.Now().Add(2*time.Second))
}

// A Hello already received when the context is done is not acknowledged.
func TestListener_HandshakeContextSendsNoAcknowledgeOnceDone(t *testing.T) {
	l, err := Listen(context.Background(), "opc.tcp://127.0.0.1:0", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	for i := 0; i < 20; i++ {
		c, err := net.Dial("tcp", l.Addr().String())
		require.NoError(t, err)
		_, err = c.Write(helloBytes(t, l.Endpoint()))
		require.NoError(t, err)
		tc, err := l.AcceptTCP()
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = l.HandshakeContext(ctx, tc, time.Minute)
		require.Error(t, err, "the handshake must fail when the context is done")
		requireClosedWithoutAck(t, c, time.Now().Add(2*time.Second))
		_ = c.Close()
	}
}

func TestListener_HandshakeContextCompletesBeforeDone(t *testing.T) {
	l, err := Listen(context.Background(), "opc.tcp://127.0.0.1:0", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	dialed := make(chan *Conn, 1)
	go func() {
		c, err := Dial(context.Background(), "opc.tcp://"+l.Addr().String())
		if err != nil {
			t.Errorf("dial: %v", err)
		}
		dialed <- c
	}()
	tc, err := l.AcceptTCP()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	srv, err := l.HandshakeContext(ctx, tc, time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	cli := <-dialed
	require.NotNil(t, cli)
	t.Cleanup(func() { _ = cli.Close() })

	// The context bounds the handshake only, not the connection it returned.
	cancel()
	time.Sleep(50 * time.Millisecond)
	go func() { _, _ = cli.Write([]byte("ping")) }()
	b := make([]byte, 4)
	_, err = srv.Read(b)
	require.NoError(t, err, "a context done after the handshake must not close the connection")
	require.Equal(t, "ping", string(b))
}

// message returns a message of type typ and chunk type F with body.
func message(t *testing.T, typ string, body []byte) []byte {
	t.Helper()
	hdr, err := (&Header{MessageType: typ, ChunkType: 'F', MessageSize: uint32(len(body) + hdrlen)}).Encode()
	require.NoError(t, err)
	return append(hdr, body...)
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	b, err := ua.Encode(v)
	require.NoError(t, err)
	return b
}

// requireRejected reads c until the server closes it and requires exactly one
// Error message with code before the close, and the close before deadline.
func requireRejected(t *testing.T, c net.Conn, code ua.StatusCode, deadline time.Time) {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(deadline))
	b, err := io.ReadAll(c) // a reset after the Error counts as the close
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the connection is still open (read %q)", b)
	}
	require.GreaterOrEqual(t, len(b), hdrlen, "no Error message before the close")
	var h Header
	_, err = h.Decode(b)
	require.NoError(t, err)
	require.Equal(t, "ERRF", h.MessageType+string(h.ChunkType))
	require.EqualValues(t, len(b), h.MessageSize, "one Error message and nothing else")
	var e Error
	_, err = e.Decode(b[hdrlen:])
	require.NoError(t, err)
	require.Equal(t, code, ua.StatusCode(e.ErrorCode))
}

// handshakeAsync accepts the next connection to l and runs the handshake on
// it with a timeout far beyond the test, so that the timeout cannot be what
// ends it.
func handshakeAsync(t *testing.T, l *Listener) <-chan error {
	t.Helper()
	tc, err := l.AcceptTCP()
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		conn, err := l.HandshakeContext(context.Background(), tc, time.Minute)
		if conn != nil {
			_ = conn.Close()
		}
		done <- err
	}()
	return done
}

// A server accepts only Hello as the first message (Part 6 §7.1.3). Any other
// message type, ReverseHello included, is answered with an Error
// Bad_TcpMessageTypeInvalid (Part 6 §7.1.5) and the connection is closed at
// once. A ReverseHello is sent by a server to a client on a connection the
// server opened (Part 6 §7.1.2.6), so the listener does not connect to the
// ServerUri it names, whatever its EndpointUrl.
func TestListener_HandshakeRejectsEveryFirstMessageButHello(t *testing.T) {
	l, err := Listen(context.Background(), "opc.tcp://127.0.0.1:0", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	back, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = back.Close() })
	serverURI := back.Addr().String()

	tests := []struct {
		name string
		msg  []byte
	}{
		{"ReverseHello for the endpoint", message(t, "RHE", encode(t, &ReverseHello{ServerURI: serverURI, EndpointURL: l.Endpoint()}))},
		{"ReverseHello for another endpoint", message(t, "RHE", encode(t, &ReverseHello{ServerURI: serverURI, EndpointURL: "opc.tcp://192.0.2.1:4840"}))},
		{"ReverseHello that does not decode", message(t, "RHE", []byte{0xff, 0xff, 0xff, 0x7f})},
		{"MSG", message(t, "MSG", nil)},
		{"OPN", message(t, "OPN", nil)},
		{"CLO", message(t, "CLO", nil)},
		{"ACK", message(t, "ACK", encode(t, DefaultClientACK))},
		{"ERR", message(t, "ERR", encode(t, &Error{ErrorCode: uint32(ua.StatusBadTCPInternalError)}))},
		{"unknown type", message(t, "XYZ", nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := net.Dial("tcp", l.Addr().String())
			require.NoError(t, err)
			t.Cleanup(func() { _ = c.Close() })
			_, err = c.Write(tt.msg)
			require.NoError(t, err)
			start := time.Now()
			done := handshakeAsync(t, l)

			requireRejected(t, c, ua.StatusBadTCPMessageTypeInvalid, start.Add(time.Second))
			select {
			case err := <-done:
				require.Error(t, err, "the handshake must fail")
			case <-time.After(time.Second):
				t.Fatal("the handshake did not end")
			}
		})
	}

	// A connect in response would have been made by now; allow for a late one.
	require.NoError(t, back.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second)))
	if peer, err := back.Accept(); err == nil {
		_ = peer.Close()
		t.Fatal("the listener connected to the ServerUri of a ReverseHello")
	}
}

// A first message larger than the receive buffer is rejected on its header,
// without waiting for its body: a Hello with Bad_TcpMessageTooLarge, any other
// type with Bad_TcpMessageTypeInvalid.
func TestListener_HandshakeRejectsAnOversizedFirstMessage(t *testing.T) {
	l, err := Listen(context.Background(), "opc.tcp://127.0.0.1:0", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	tests := []struct {
		typ  string
		code ua.StatusCode
	}{
		{"RHE", ua.StatusBadTCPMessageTypeInvalid},
		{"MSG", ua.StatusBadTCPMessageTypeInvalid},
		{"HEL", ua.StatusBadTCPMessageTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.typ, func(t *testing.T) {
			c, err := net.Dial("tcp", l.Addr().String())
			require.NoError(t, err)
			t.Cleanup(func() { _ = c.Close() })
			hdr, err := (&Header{MessageType: tt.typ, ChunkType: 'F', MessageSize: DefaultReceiveBufSize + 1}).Encode()
			require.NoError(t, err)
			_, err = c.Write(hdr)
			require.NoError(t, err)
			start := time.Now()
			done := handshakeAsync(t, l)

			requireRejected(t, c, tt.code, start.Add(time.Second))
			require.Error(t, <-done)
		})
	}
}

// A Hello is still acknowledged after rejected first messages on the same
// listener.
func TestListener_HandshakeAcknowledgesHelloAfterARejection(t *testing.T) {
	l, err := Listen(context.Background(), "opc.tcp://127.0.0.1:0", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	c, err := net.Dial("tcp", l.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	_, err = c.Write(message(t, "RHE", encode(t, &ReverseHello{ServerURI: "127.0.0.1:1", EndpointURL: l.Endpoint()})))
	require.NoError(t, err)
	done := handshakeAsync(t, l)
	requireRejected(t, c, ua.StatusBadTCPMessageTypeInvalid, time.Now().Add(time.Second))
	require.Error(t, <-done)

	dialed := make(chan *Conn, 1)
	go func() {
		c, err := Dial(context.Background(), "opc.tcp://"+l.Addr().String())
		if err != nil {
			t.Errorf("dial: %v", err)
		}
		dialed <- c
	}()
	require.NoError(t, <-handshakeAsync(t, l))
	cli := <-dialed
	require.NotNil(t, cli, "the Hello was not acknowledged")
	_ = cli.Close()
}
