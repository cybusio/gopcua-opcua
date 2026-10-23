package server

import (
	"context"
	"crypto/rsa"
	"errors"
	"io"
	mrand "math/rand"
	"sync"
	"time"

	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uacp"
	"github.com/gopcua/opcua/uasc"
)

type channelBroker struct {
	endpoints map[string]*ua.EndpointDescription

	wg sync.WaitGroup

	// mu protects concurrent modification of s, end, closed, secureChannelID,
	// and secureTokenID
	mu sync.RWMutex
	// s is a slice of all SecureChannels watched by the channelBroker
	s map[uint32]*uasc.SecureChannel
	// end ends the secure channel with the same id in s and closes its
	// transport connection.
	end map[uint32]func()
	// closed is set by Close. No channel is registered after it.
	closed bool

	// Next Secure Channel ID to issue to a client
	secureChannelID uint32

	// Next Token ID to issue to a client
	secureTokenID uint32

	// msgChan is the common channel that all messages from all channels
	// get funneled into for handling
	msgChan chan *uasc.MessageBody
	logger  Logger
}

func newChannelBroker(logger Logger) *channelBroker {
	rng := mrand.New(mrand.NewSource(time.Now().UnixNano()))
	return &channelBroker{
		endpoints:       make(map[string]*ua.EndpointDescription),
		s:               make(map[uint32]*uasc.SecureChannel),
		end:             make(map[uint32]func()),
		msgChan:         make(chan *uasc.MessageBody),
		secureChannelID: uint32(rng.Int31()),
		secureTokenID:   uint32(rng.Int31()),
		logger:          logger,
	}
}

// RegisterConn connects a new UACP connection to the channel broker's list
// of connections and starts waiting for data on it.  Data is pushed onto the broker's
// Response channel
// Blocks until the context is done, the connection closes, or a critical error
func (c *channelBroker) RegisterConn(ctx context.Context, conn *uacp.Conn, localCert []byte, localKey *rsa.PrivateKey) error {
	cfg := defaultChannelConfig()
	cfg.Certificate = localCert
	cfg.LocalKey = localKey

	c.mu.Lock()
	c.secureChannelID++
	c.secureTokenID++
	secureChannelID := c.secureChannelID
	secureTokenID := c.secureTokenID
	sequenceNumber := uint32(mrand.Int31n(1023) + 1)
	c.mu.Unlock()

	errch := make(chan error, 1)
	sc, err := uasc.NewServerSecureChannel(
		"", // todo(fs): this is most likely wrong
		conn,
		cfg,
		errch,
		secureChannelID,
		sequenceNumber,
		secureTokenID,
	)
	if err != nil {
		if c.logger != nil {
			c.logger.Error("Error creating secure channel for new connection: %s", err)
		}
		return err
	}

	// The channel ends when ctx is done, because the caller cancelled it or
	// Close stops the server. Closing the transport connection then ends a
	// read that waits for the client, so the receive loop returns without
	// the client's help.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	end := func() {
		cancel()
		conn.Close()
	}
	stop := context.AfterFunc(ctx, end)
	defer stop()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		end()
		return errors.New("server: secure channel not registered: server closed")
	}
	c.s[secureChannelID] = sc
	c.end[secureChannelID] = end
	c.wg.Add(1)
	if c.logger != nil {
		c.logger.Info("Registered new channel (id %d) now at %d channels", secureChannelID, len(c.s))
	}
	c.mu.Unlock()
outer:
	for {
		select {
		case <-ctx.Done():
			// todo(fs): return error?
			if c.logger != nil {
				c.logger.Warn("Context done, closing Secure Channel %d", secureChannelID)
			}
			break outer

		default:
			msg := sc.Receive(ctx)
			if ctx.Err() != nil {
				// The connection was closed because ctx is done. A request
				// read meanwhile is not handled.
				if c.logger != nil {
					c.logger.Warn("Context done, closing Secure Channel %d", secureChannelID)
				}
				break outer
			}
			if msg.Err == io.EOF {
				if c.logger != nil {
					c.logger.Warn("Secure Channel %d closed", secureChannelID)
				}
				break outer
			} else if msg.Err != nil {
				if c.logger != nil {
					c.logger.Error("Secure Channel %d error: %s", secureChannelID, msg.Err)
				}
				break outer
			}
			select {
			case c.msgChan <- msg:
			case <-ctx.Done():
				break outer
			}
		}
	}
	if ctx.Err() != nil {
		// Make sure the connection is closed before Close learns that the
		// channel has ended; end may still be running in the AfterFunc.
		end()
	}

	c.mu.Lock()
	delete(c.s, secureChannelID)
	delete(c.end, secureChannelID)
	c.mu.Unlock()
	c.wg.Done()

	return nil
}

// Close ends all secure channels and closes their transport connections,
// and registers no channel afterwards. It returns once the receive loop of
// every channel has returned.
//
// The server is the side that ends the channels here, so it cannot wait for
// the clients: Part 6 §7.1.4 has it release all resources allocated for a
// channel and §7.1.5 has it close the TransportConnection gracefully. A
// client that stays connected and idle would otherwise keep its channel,
// and Close, waiting until the timeout below.
//
// Nothing is sent before the close. CloseSecureChannel is a request of the
// client (Part 4 §5.6.3, Part 6 §7.1.4); the Error message is for protocol
// errors the server cannot recover from (Part 6 §7.1.5), and a server that
// stops has none.
// todo(fs): use ctx
func (c *channelBroker) Close() error {
	var err error
	c.mu.Lock()
	c.closed = true
	for _, end := range c.end {
		end()
	}
	c.mu.Unlock()

	// Wait for all goroutines to finish or timeout
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second): // todo(fs): magic number
		if c.logger != nil {
			c.logger.Error("CloseAll: timed out waiting for channels to exit")
		}
	}

	return err
}

func (c *channelBroker) ReadMessage(ctx context.Context) *uasc.MessageBody {
	select {
	case <-ctx.Done():
		return nil
	case msg := <-c.msgChan:
		return msg
	}
}
