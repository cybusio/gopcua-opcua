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

// The connection a ReverseHello dialed is the one the handshake returns, and
// closing it closes that connection.
func TestListener_HandshakeReverseHelloConnectionCloses(t *testing.T) {
	l, err := Listen(context.Background(), "opc.tcp://127.0.0.1:0", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	back, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = back.Close() })

	c, err := net.Dial("tcp", l.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	body, err := ua.Encode(&ReverseHello{ServerURI: back.Addr().String(), EndpointURL: l.Endpoint()})
	require.NoError(t, err)
	hdr, err := (&Header{MessageType: "RHE", ChunkType: 'F', MessageSize: uint32(len(body) + hdrlen)}).Encode()
	require.NoError(t, err)
	_, err = c.Write(append(hdr, body...))
	require.NoError(t, err)

	tc, err := l.AcceptTCP()
	require.NoError(t, err)
	srv, err := l.HandshakeContext(context.Background(), tc, time.Minute)
	require.NoError(t, err)
	peer, err := back.Accept()
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })

	_ = srv.Close()
	requireClosedWithoutAck(t, peer, time.Now().Add(2*time.Second))
}

// The handshake timeout bounds the dial a ReverseHello asks for, too.
func TestListener_HandshakeTimeoutBoundsReverseHelloDial(t *testing.T) {
	const blackhole = "10.255.255.1:4840"
	if c, err := net.DialTimeout("tcp", blackhole, 300*time.Millisecond); err == nil {
		_ = c.Close()
		t.Skipf("%s answers here", blackhole)
	} else {
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Skipf("connect to %s fails fast here (%v)", blackhole, err)
		}
	}
	l, err := Listen(context.Background(), "opc.tcp://127.0.0.1:0", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	c, err := net.Dial("tcp", l.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	body, err := ua.Encode(&ReverseHello{ServerURI: blackhole, EndpointURL: l.Endpoint()})
	require.NoError(t, err)
	hdr, err := (&Header{MessageType: "RHE", ChunkType: 'F', MessageSize: uint32(len(body) + hdrlen)}).Encode()
	require.NoError(t, err)
	_, err = c.Write(append(hdr, body...))
	require.NoError(t, err)

	tc, err := l.AcceptTCP()
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, err := l.HandshakeContext(context.Background(), tc, 500*time.Millisecond)
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err, "the dial cannot succeed")
	case <-time.After(3 * time.Second):
		t.Fatal("the ReverseHello dial outlived the handshake timeout")
	}
}
