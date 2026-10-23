// Copyright 2018-2020 opcua authors. All rights reserved.
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

package uacp

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopcua/opcua/debug"
	"github.com/gopcua/opcua/errors"
	"github.com/gopcua/opcua/ua"
)

const (
	KB = 1024
	MB = 1024 * KB

	DefaultReceiveBufSize = 0xffff
	DefaultSendBufSize    = 0xffff
	DefaultMaxChunkCount  = 512
	DefaultMaxMessageSize = 2 * MB
)

var (
	// DefaultClientACK is the ACK handshake message sent to the server
	// for client connections.
	DefaultClientACK = &Acknowledge{
		ReceiveBufSize: DefaultReceiveBufSize,
		SendBufSize:    DefaultSendBufSize,
		MaxChunkCount:  0, // use what the server wants
		MaxMessageSize: 0, // use what the server wants
	}

	// DefaultServerACK is the ACK handshake message sent to the client
	// for server connections.
	DefaultServerACK = &Acknowledge{
		ReceiveBufSize: DefaultReceiveBufSize,
		SendBufSize:    DefaultSendBufSize,
		MaxChunkCount:  DefaultMaxChunkCount,
		MaxMessageSize: DefaultMaxMessageSize,
	}
)

// connid stores the current connection id. updated with atomic.AddUint32
var connid uint32

// nextid returns the next connection id
func nextid() uint32 {
	return atomic.AddUint32(&connid, 1)
}

// Dialer establishes a connection to an endpoint.
type Dialer struct {
	// Dialer establishes the TCP connection. Defaults to net.Dialer.
	Dialer *net.Dialer

	// ClientACK defines the connection parameters requested by the client.
	// Defaults to DefaultClientACK.
	ClientACK *Acknowledge
}

func (d *Dialer) Dial(ctx context.Context, endpoint string) (*Conn, error) {
	debug.Printf("uacp: connecting to %s", endpoint)

	_, raddr, err := ResolveEndpoint(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	dl := d.Dialer
	if dl == nil {
		dl = &net.Dialer{}

	}

	c, err := dl.DialContext(ctx, "tcp", raddr.Host)
	if err != nil {
		return nil, err
	}

	conn, err := NewConn(c.(*net.TCPConn), d.ClientACK)
	if err != nil {
		c.Close()
		return nil, err
	}

	debug.Printf("uacp %d: start HEL/ACK handshake", conn.id)
	if err := conn.Handshake(ctx, endpoint); err != nil {
		debug.Printf("uacp %d: HEL/ACK handshake failed: %s", conn.id, err)
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Dial uses the default dialer to establish a connection to the endpoint
func Dial(ctx context.Context, endpoint string) (*Conn, error) {
	d := &Dialer{}
	return d.Dial(ctx, endpoint)
}

// Listener is a OPC UA Connection Protocol network listener.
type Listener struct {
	l        *net.TCPListener
	ack      *Acknowledge
	endpoint string
}

// Listen acts like net.Listen for OPC UA Connection Protocol networks.
//
// Currently the endpoint can only be specified in "opc.tcp://<addr[:port]>/path" format.
//
// If the IP field of laddr is nil or an unspecified IP address, Listen listens
// on all available unicast and anycast IP addresses of the local system.
// If the Port field of laddr is 0, a port number is automatically chosen.
func Listen(ctx context.Context, endpoint string, ack *Acknowledge) (*Listener, error) {
	if ack == nil {
		ack = DefaultServerACK
	}
	_, laddr, err := ResolveEndpoint(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", laddr.Host)
	if err != nil {
		return nil, err
	}
	return &Listener{
		l:        l.(*net.TCPListener),
		ack:      ack,
		endpoint: endpoint,
	}, nil
}

// Accept accepts the next incoming call and returns the new connection.
//
// The first param ctx is to be passed to monitor(), which monitors and handles
// incoming messages automatically in another goroutine.
//
// Accept runs the handshake before it returns, without a deadline, so a client
// that connects and never sends Hello blocks it. A server that accepts in a loop
// should use AcceptTCP and Handshake instead.
func (l *Listener) Accept(ctx context.Context) (*Conn, error) {
	c, err := l.l.AcceptTCP()
	if err != nil {
		return nil, err
	}
	return l.Handshake(c, 0)
}

// AcceptTCP waits for the next TCP connection and returns it without running
// the UACP handshake. Handshake completes it.
//
// Splitting the two lets a server handshake each connection in its own
// goroutine: a slow or silent client then cannot stall the accept loop, and a
// failed handshake is an error of that one connection, not of the listener. An
// error from AcceptTCP that wraps net.ErrClosed means the listener was closed.
func (l *Listener) AcceptTCP() (*net.TCPConn, error) {
	return l.l.AcceptTCP()
}

// Handshake runs the server side of the UACP handshake on c, which AcceptTCP
// returned, and returns the established connection. A timeout greater than zero
// bounds the handshake. c is closed when the handshake fails.
func (l *Listener) Handshake(c *net.TCPConn, timeout time.Duration) (*Conn, error) {
	return l.HandshakeContext(context.Background(), c, timeout)
}

// HandshakeContext is Handshake ended by ctx as well: once ctx is done, c is
// closed and the handshake fails. No Acknowledge is started once ctx is done;
// one already started is written in full before c is closed. A client whose
// Hello arrives after ctx is done therefore finds the connection closed. A
// server that is shutting down uses it to close the connections still waiting
// for Hello. ctx ends the handshake only, not the connection it returns.
func (l *Listener) HandshakeContext(ctx context.Context, c *net.TCPConn, timeout time.Duration) (*Conn, error) {
	if timeout > 0 {
		if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
			c.Close()
			return nil, err
		}
	}
	conn := &Conn{TCPConn: c, id: nextid(), ack: l.ack}

	// mu keeps the Acknowledge and the close on ctx apart: an Acknowledge is
	// started only while ctx is not done, and c is closed only after it.
	var mu sync.Mutex
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		defer mu.Unlock()
		c.Close()
	})
	ack := func() error {
		mu.Lock()
		defer mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		return conn.Send("ACKF", conn.ack)
	}
	err := conn.srvhandshake(ctx, ack)
	if !stop() && err == nil {
		// ctx was done after the Acknowledge; c is being closed.
		err = ctx.Err()
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	if timeout > 0 {
		if err := c.SetDeadline(time.Time{}); err != nil {
			c.Close()
			return nil, err
		}
	}
	return conn, nil
}

// Close closes the Listener.
func (l *Listener) Close() error {
	return l.l.Close()
}

// Addr returns the listener's network address.
func (l *Listener) Addr() net.Addr {
	return l.l.Addr()
}

// Endpoint returns the listener's EndpointURL.
func (l *Listener) Endpoint() string {
	return l.endpoint
}

type Conn struct {
	*net.TCPConn
	id  uint32
	ack *Acknowledge

	closeOnce sync.Once
}

func NewConn(c *net.TCPConn, ack *Acknowledge) (*Conn, error) {
	if c == nil {
		return nil, fmt.Errorf("no connection")
	}
	if ack == nil {
		ack = DefaultClientACK
	}
	return &Conn{TCPConn: c, id: nextid(), ack: ack}, nil
}

func (c *Conn) ID() uint32 {
	return c.id
}

func (c *Conn) ReceiveBufSize() uint32 {
	return c.ack.ReceiveBufSize
}

func (c *Conn) SendBufSize() uint32 {
	return c.ack.SendBufSize
}

func (c *Conn) MaxMessageSize() uint32 {
	return c.ack.MaxMessageSize
}

func (c *Conn) MaxChunkCount() uint32 {
	return c.ack.MaxChunkCount
}

func (c *Conn) Close() (err error) {
	err = io.EOF
	c.closeOnce.Do(func() { err = c.close() })
	return err
}

func (c *Conn) close() error {
	debug.Printf("uacp %d: close", c.id)
	return c.TCPConn.Close()
}

func (c *Conn) Handshake(ctx context.Context, endpoint string) error {
	hel := &Hello{
		Version:        c.ack.Version,
		ReceiveBufSize: c.ack.ReceiveBufSize,
		SendBufSize:    c.ack.SendBufSize,
		MaxMessageSize: c.ack.MaxMessageSize,
		MaxChunkCount:  c.ack.MaxChunkCount,
		EndpointURL:    endpoint,
	}

	// set a deadline if there is one
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}

	if err := c.Send("HELF", hel); err != nil {
		return err
	}

	b, err := c.Receive()
	if err != nil {
		return err
	}

	// clear the deadline
	c.SetDeadline(time.Time{})

	msgtyp := string(b[:4])
	switch msgtyp {
	case "ACKF":
		ack := new(Acknowledge)
		if _, err := ack.Decode(b[hdrlen:]); err != nil {
			return errors.Errorf("uacp: decode ACK failed: %s", err)
		}
		if ack.Version != 0 {
			return errors.Errorf("uacp: invalid version %d", ack.Version)
		}
		if ack.MaxChunkCount == 0 {
			ack.MaxChunkCount = DefaultMaxChunkCount
			debug.Printf("uacp %d: server has no chunk limit. Using %d", c.id, ack.MaxChunkCount)
		}
		if ack.MaxMessageSize == 0 {
			ack.MaxMessageSize = DefaultMaxMessageSize
			debug.Printf("uacp %d: server has no message size limit. Using %d", c.id, ack.MaxMessageSize)
		}
		c.ack = ack
		debug.Printf("uacp %d: recv %#v", c.id, ack)
		return nil

	case "ERRF":
		errf := new(Error)
		if _, err := errf.Decode(b[hdrlen:]); err != nil {
			return errors.Errorf("uacp: decode ERR failed: %s", err)
		}
		debug.Printf("uacp %d: recv %#v", c.id, errf)
		return errf

	default:
		c.SendError(ua.StatusBadTCPInternalError)
		return errors.Errorf("invalid handshake packet %q", msgtyp)
	}
}

// srvhandshake runs the server side of the handshake; ack writes the
// Acknowledge to a Hello. The handshake ends when ctx is done.
//
// A client opens a connection with Hello (Part 6 §7.1.3), so Hello is the only
// first message a server accepts. Any other type is answered with
// Bad_TcpMessageTypeInvalid (Part 6 §7.1.5) and ends the handshake. That
// includes ReverseHello: a server sends it to a client on a connection the
// server opened (Part 6 §7.1.2.6), and a server that receives one does not act
// on it.
func (c *Conn) srvhandshake(ctx context.Context, ack func() error) error {
	h, b, err := c.receive()
	if err != nil {
		if ctx.Err() == nil {
			c.SendError(rejectCode(h, c.ack.ReceiveBufSize))
		}
		return err
	}

	msgtyp := string(b[:4])
	msg := b[hdrlen:]
	switch msgtyp {
	case "HELF":
		hel := new(Hello)
		if _, err := hel.Decode(msg); err != nil {
			c.SendError(ua.StatusBadTCPInternalError)
			return err
		}
		if err := ack(); err != nil {
			if ctx.Err() == nil {
				c.SendError(ua.StatusBadTCPInternalError)
			}
			return err
		}
		debug.Printf("uacp %d: recv %#v", c.id, hel)
		return nil

	default:
		if ctx.Err() == nil {
			c.SendError(ua.StatusBadTCPMessageTypeInvalid)
		}
		return errors.Errorf("invalid handshake packet %q", msgtyp)
	}
}

// rejectCode returns the error code for a first message that receive failed
// on, with header h if it was read. A message other than Hello is rejected for
// its type, before its size.
func rejectCode(h Header, receiveBufSize uint32) ua.StatusCode {
	switch {
	case h.MessageType == "":
		// The header was not read.
		return ua.StatusBadTCPInternalError
	case h.MessageType != MessageTypeHello || h.ChunkType != ChunkTypeFinal:
		return ua.StatusBadTCPMessageTypeInvalid
	case h.MessageSize > receiveBufSize:
		return ua.StatusBadTCPMessageTooLarge
	default:
		return ua.StatusBadTCPInternalError
	}
}

// hdrlen is the size of the uacp header
const hdrlen = 8

// Receive reads a full UACP message from the underlying connection.
// The size of b must be at least ReceiveBufSize. Otherwise,
// the function returns an error.
func (c *Conn) Receive() ([]byte, error) {
	h, b, err := c.receive()
	if err != nil {
		return nil, err
	}
	if h.MessageType == MessageTypeError {
		errf := new(Error)
		if _, err := errf.Decode(b[hdrlen:]); err != nil {
			return nil, errors.Errorf("uacp: failed to decode ERRF message: %s", err)
		}
		return nil, errf
	}
	return b, nil
}

// receive reads a full UACP message, an Error message included, and returns
// its header and bytes. The header is also returned when the message is
// rejected after the header was read.
func (c *Conn) receive() (Header, []byte, error) {
	// TODO(kung-foo): allow user-specified buffer
	// TODO(kung-foo): sync.Pool
	b := make([]byte, c.ack.ReceiveBufSize)

	if _, err := io.ReadFull(c, b[:hdrlen]); err != nil {
		// todo(fs): do not wrap this error since it hides io.EOF
		// todo(fs): use golang.org/x/xerrors
		return Header{}, nil, err
	}

	var h Header
	if _, err := h.Decode(b[:hdrlen]); err != nil {
		return Header{}, nil, errors.Errorf("uacp: header decode failed: %s", err)
	}

	if h.MessageSize > c.ack.ReceiveBufSize {
		return h, nil, errors.Errorf("uacp: message too large: %d > %d bytes. MsgType=%s, ChunkType=%c", h.MessageSize, c.ack.ReceiveBufSize, h.MessageType, h.ChunkType)
	}
	if h.MessageSize < hdrlen {
		return h, nil, errors.Errorf("uacp: message too small: %d bytes. MsgType=%s, ChunkType=%c.", h.MessageSize, h.MessageType, h.ChunkType)
	}

	if _, err := io.ReadFull(c, b[hdrlen:h.MessageSize]); err != nil {
		// todo(fs): do not wrap this error since it hides io.EOF
		// todo(fs): use golang.org/x/xerrors
		return h, nil, err
	}

	debug.Printf("uacp %d: recv %s%c with %d bytes", c.id, h.MessageType, h.ChunkType, h.MessageSize)
	return h, b[:h.MessageSize], nil
}

func (c *Conn) Send(typ string, msg interface{}) error {
	if len(typ) != 4 {
		return errors.Errorf("invalid msg type: %s", typ)
	}

	body, err := ua.Encode(msg)
	if err != nil {
		return errors.Errorf("encode msg failed: %s", err)
	}

	h := Header{
		MessageType: typ[:3],
		ChunkType:   typ[3],
		MessageSize: uint32(len(body) + hdrlen),
	}

	if h.MessageSize > c.ack.SendBufSize {
		return errors.Errorf("send packet too large: %d > %d bytes", h.MessageSize, c.ack.SendBufSize)
	}

	hdr, err := h.Encode()
	if err != nil {
		return errors.Errorf("encode hdr failed: %s", err)
	}

	b := append(hdr, body...)
	if _, err := c.Write(b); err != nil {
		return errors.Errorf("write failed: %s", err)
	}
	debug.Printf("uacp %d: sent %s with %d bytes", c.id, typ, len(b))

	return nil
}

func (c *Conn) SendError(code ua.StatusCode) {
	// we swallow the error to silence complaints from the linter
	// since sending an error will close the connection and we
	// want to bubble a different error up.
	_ = c.Send("ERRF", &Error{ErrorCode: uint32(code)})
}
