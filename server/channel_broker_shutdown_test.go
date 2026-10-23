package server

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uacp"
	"github.com/gopcua/opcua/uasc"
	"github.com/stretchr/testify/require"
)

// shutdownBound is how long the tests allow the server to end a secure
// channel it closes itself. Far below the ten seconds channelBroker.Close
// waits for channels that do not end.
const shutdownBound = 2 * time.Second

// startShutdownServer starts a SecurityPolicy#None server on an ephemeral
// loopback port. It returns the server, the endpoint URL to dial and the
// cancel function of the context the server was started with.
func startShutdownServer(t *testing.T) (*Server, string, context.CancelFunc) {
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
	return s, "opc.tcp://" + s.l.Addr().String(), cancel
}

func registeredChannels(s *Server) int {
	s.cb.mu.RLock()
	defer s.cb.mu.RUnlock()
	return len(s.cb.s)
}

// idleClient is a client connection that reads nothing until told to. It
// frames its messages itself, so that whatever the server writes stays
// unread in the socket and the test sees exactly that.
type idleClient struct {
	t    *testing.T
	conn *uacp.Conn
	seq  uint32
	// channelID and tokenID are zero until openChannel has run.
	channelID uint32
	tokenID   uint32
}

// dialIdle completes the Hello/Acknowledge handshake.
func dialIdle(t *testing.T, endpoint string) *idleClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownBound)
	defer cancel()
	conn, err := uacp.Dial(ctx, endpoint)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return &idleClient{t: t, conn: conn}
}

func (c *idleClient) send(msgType string, typeID uint16, svc any) error {
	c.seq++
	hdr := &uasc.MessageHeader{
		Header:         uasc.NewHeader(msgType, uasc.ChunkTypeFinal, c.channelID),
		SequenceHeader: uasc.NewSequenceHeader(c.seq, c.seq),
	}
	if msgType == uasc.MessageTypeOpenSecureChannel {
		hdr.AsymmetricSecurityHeader = uasc.NewAsymmetricSecurityHeader(ua.SecurityPolicyURINone, nil, nil)
	} else {
		hdr.SymmetricSecurityHeader = uasc.NewSymmetricSecurityHeader(c.tokenID)
	}
	b, err := (&uasc.Message{
		MessageHeader: hdr,
		TypeID:        ua.NewFourByteExpandedNodeID(0, typeID),
		Service:       svc,
	}).Encode()
	require.NoError(c.t, err)
	require.NoError(c.t, c.conn.SetWriteDeadline(time.Now().Add(shutdownBound)))
	_, err = c.conn.Write(b)
	return err
}

// receive reads the next message within d and returns its service body,
// or the read error unchanged.
func (c *idleClient) receive(d time.Duration) (any, error) {
	require.NoError(c.t, c.conn.SetReadDeadline(time.Now().Add(d)))
	b, err := c.conn.Receive()
	if err != nil {
		return nil, err
	}
	m := new(uasc.Message)
	if _, err := m.Decode(b); err != nil {
		return nil, err
	}
	return m.Service, nil
}

func (c *idleClient) header() *ua.RequestHeader {
	return &ua.RequestHeader{
		AuthenticationToken: ua.NewTwoByteNodeID(0),
		Timestamp:           time.Now(),
		RequestHandle:       c.seq + 1,
		TimeoutHint:         uint32(shutdownBound / time.Millisecond),
	}
}

func (c *idleClient) openChannel() {
	c.t.Helper()
	require.NoError(c.t, c.send(uasc.MessageTypeOpenSecureChannel, id.OpenSecureChannelRequest_Encoding_DefaultBinary, &ua.OpenSecureChannelRequest{
		RequestHeader:     c.header(),
		RequestType:       ua.SecurityTokenRequestTypeIssue,
		SecurityMode:      ua.MessageSecurityModeNone,
		RequestedLifetime: uint32(time.Minute / time.Millisecond),
	}))
	svc, err := c.receive(shutdownBound)
	require.NoError(c.t, err)
	resp, ok := svc.(*ua.OpenSecureChannelResponse)
	require.True(c.t, ok, "got %T, want *ua.OpenSecureChannelResponse", svc)
	c.channelID = resp.SecurityToken.ChannelID
	c.tokenID = resp.SecurityToken.TokenID
}

// expectClosed asserts that the server has closed the connection: the next
// read returns io.EOF or a reset within d, and no message comes first.
func (c *idleClient) expectClosed(d time.Duration, what string) {
	c.t.Helper()
	svc, err := c.receive(d)
	if err == nil {
		c.t.Fatalf("%s: server sent %T instead of closing the connection", what, svc)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		c.t.Fatalf("%s: connection still open %s after the server ended the channel", what, d)
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !isConnReset(err) {
		c.t.Fatalf("%s: read failed with %v, want io.EOF or a reset", what, err)
	}
}

func isConnReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET)
}

// waitChannels waits until the broker holds n channels.
func waitChannels(t *testing.T, s *Server, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return registeredChannels(s) == n },
		shutdownBound, 5*time.Millisecond, "broker holds %d channels, want %d", registeredChannels(s), n)
}

// TestCloseEndsIdleChannels checks that Close ends the transport connection
// of every secure channel itself instead of waiting for the clients to hang
// up. Part 6 §7.1.4 has the server release all resources allocated for a
// channel once the channel is closed, and §7.1.5 has it close the
// TransportConnection gracefully; a server that stops is the one ending the
// channels, so it cannot wait for a client that has no reason to act. The
// clients here stay idle: one after Hello/Acknowledge only, one with an open
// secure channel.
func TestCloseEndsIdleChannels(t *testing.T) {
	s, endpoint, _ := startShutdownServer(t)

	helloOnly := dialIdle(t, endpoint)
	open := dialIdle(t, endpoint)
	open.openChannel()
	waitChannels(t, s, 2)

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(shutdownBound):
		t.Fatalf("Close did not return within %s with two idle clients connected", shutdownBound)
	}
	t.Logf("Close returned after %s", time.Since(start))

	// The server wrote nothing before closing and the connections are
	// already closed when Close returns.
	helloOnly.expectClosed(shutdownBound, "after Hello only")
	open.expectClosed(shutdownBound, "with an open secure channel")
	require.Equal(t, 0, registeredChannels(s), "channels left in the broker after Close")
}

// TestCloseAnswersNoRequestAfterwards checks that a request a client sends
// after Close returned is not answered.
func TestCloseAnswersNoRequestAfterwards(t *testing.T) {
	s, endpoint, _ := startShutdownServer(t)

	c := dialIdle(t, endpoint)
	c.openChannel()
	waitChannels(t, s, 1)
	require.NoError(t, s.Close())

	// The write may still succeed locally; the server must not answer it.
	if err := c.send(uasc.MessageTypeMessage, id.GetEndpointsRequest_Encoding_DefaultBinary, &ua.GetEndpointsRequest{
		RequestHeader: c.header(),
		EndpointURL:   endpoint,
	}); err != nil {
		return
	}
	c.expectClosed(shutdownBound, "request after Close")
}

// TestContextCancelEndsChannels checks that ending the context the server
// was started with ends its secure channels as well; RegisterConn is
// documented to return when its context is done.
func TestContextCancelEndsChannels(t *testing.T) {
	s, endpoint, cancel := startShutdownServer(t)

	c := dialIdle(t, endpoint)
	c.openChannel()
	waitChannels(t, s, 1)

	cancel()
	c.expectClosed(shutdownBound, "after the server context was cancelled")
	waitChannels(t, s, 0)
}

// TestRegisterConnAfterClose checks that a connection the listener accepted
// just before Close, and that reaches the broker only after it, is closed at
// once instead of being served by a server that has stopped.
func TestRegisterConnAfterClose(t *testing.T) {
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer l.Close()

	client, err := net.DialTCP("tcp", nil, l.Addr().(*net.TCPAddr))
	require.NoError(t, err)
	defer client.Close()
	server, err := l.AcceptTCP()
	require.NoError(t, err)
	conn, err := uacp.NewConn(server, nil)
	require.NoError(t, err)

	cb := newChannelBroker(nil)
	require.NoError(t, cb.Close())

	returned := make(chan error, 1)
	go func() { returned <- cb.RegisterConn(context.Background(), conn, nil, nil) }()
	select {
	case err := <-returned:
		require.Error(t, err)
	case <-time.After(shutdownBound):
		t.Fatal("RegisterConn served a connection after Close")
	}

	require.NoError(t, client.SetReadDeadline(time.Now().Add(shutdownBound)))
	_, err = client.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF, "connection registered after Close was not closed")
	require.Empty(t, cb.s)
}
