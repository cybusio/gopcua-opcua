package server

import (
	"context"
	"crypto/rsa"
	"io"
	mrand "math/rand"
	"sync"
	"time"

	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uacp"
	"github.com/gopcua/opcua/uasc"
)

// maxQueuedRequests is the number of requests of one secure channel that wait
// while an earlier request of that channel is handled. A request that arrives
// when that many are waiting is answered at once with Bad_ResourceUnavailable
// and not handled.
const maxQueuedRequests = 64

// requestHandler handles one service request of a secure channel and sends
// its response.
type requestHandler func(ctx context.Context, sc *uasc.SecureChannel, reqID uint32, req ua.Request)

type channelBroker struct {
	endpoints map[string]*ua.EndpointDescription

	wg sync.WaitGroup

	// mu protects concurrent modification of s, secureChannelID, and secureTokenID
	mu sync.RWMutex
	// s is a slice of all SecureChannels watched by the channelBroker
	s map[uint32]*uasc.SecureChannel

	// Next Secure Channel ID to issue to a client
	secureChannelID uint32

	// Next Token ID to issue to a client
	secureTokenID uint32

	logger Logger
}

func newChannelBroker(logger Logger) *channelBroker {
	rng := mrand.New(mrand.NewSource(time.Now().UnixNano()))
	return &channelBroker{
		endpoints:       make(map[string]*ua.EndpointDescription),
		s:               make(map[uint32]*uasc.SecureChannel),
		secureChannelID: uint32(rng.Int31()),
		secureTokenID:   uint32(rng.Int31()),
		logger:          logger,
	}
}

// RegisterConn connects a new UACP connection to the channel broker's list
// of connections and starts waiting for data on it. Each service request is
// passed to handle.
//
// The requests of the channel are handled one at a time, in the order they
// arrive, on a goroutine of the channel, so that a request that takes long
// delays no other channel. The channel keeps reading meanwhile, so that
// OpenSecureChannel and CloseSecureChannel are processed, and queues further
// requests, up to maxQueuedRequests.
//
// Blocks until the context is done, the connection closes, or a critical error
func (c *channelBroker) RegisterConn(ctx context.Context, conn *uacp.Conn, localCert []byte, localKey *rsa.PrivateKey, handle requestHandler) error {
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

	c.mu.Lock()
	c.s[secureChannelID] = sc
	if c.logger != nil {
		c.logger.Info("Registered new channel (id %d) now at %d channels", secureChannelID, len(c.s))
	}
	c.mu.Unlock()
	c.wg.Add(1)

	// The requests of the channel wait in queue for serveRequests. Ending ctx
	// ends serveRequests once the request it handles, if any, returns; the
	// requests still waiting are dropped and the response of that request is
	// not sent, as the channel is gone.
	ctx, cancel := context.WithCancel(ctx)
	queue := make(chan *uasc.MessageBody, maxQueuedRequests)
	go c.serveRequests(ctx, sc, queue, handle)

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
			c.queueRequest(ctx, sc, queue, msg)
		}
	}
	cancel()

	c.mu.Lock()
	delete(c.s, secureChannelID)
	c.mu.Unlock()
	c.wg.Done()

	return nil
}

// queueRequest queues the service request in msg for serveRequests without
// waiting. When maxQueuedRequests requests of the channel are already
// waiting, the request is answered with Bad_ResourceUnavailable, "An
// operating system resource is not available." (Part 4 §7.38.2 Table 178),
// with its requestHandle in the ResponseHeader (Part 4 §7.33), and not
// handled. The answer is sent
// from the receive loop of the channel, so it can only wait for that
// channel's own connection.
func (c *channelBroker) queueRequest(ctx context.Context, sc *uasc.SecureChannel, queue chan<- *uasc.MessageBody, msg *uasc.MessageBody) {
	req := msg.Request()
	if req == nil {
		// OpenSecureChannel is handled by the secure channel itself and
		// leaves an empty message.
		if resp := msg.Response(); resp != nil && c.logger != nil {
			c.logger.Error("Secure Channel %d: server received response %T", msg.SecureChannelID, resp)
		}
		return
	}

	select {
	case queue <- msg:
		return
	default:
	}

	if c.logger != nil {
		c.logger.Warn("Secure Channel %d: %d requests waiting, rejecting %T", msg.SecureChannelID, maxQueuedRequests, req)
	}
	var handle uint32
	if hdr := req.Header(); hdr != nil {
		handle = hdr.RequestHandle
	}
	fault := &ua.ServiceFault{ResponseHeader: responseHeader(handle, ua.StatusBadResourceUnavailable)}
	if err := sc.SendResponseWithContext(ctx, msg.RequestID, fault); err != nil && c.logger != nil {
		c.logger.Warn("Error sending response: %s\n", err)
	}
}

// serveRequests passes the requests from queue to handle, one at a time, in
// order, until ctx is done.
func (c *channelBroker) serveRequests(ctx context.Context, sc *uasc.SecureChannel, queue <-chan *uasc.MessageBody, handle requestHandler) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-queue:
			if ctx.Err() != nil {
				return
			}
			handle(ctx, sc, msg.RequestID, msg.Request())
		}
	}
}

// Close gracefully closes all secure channels
// todo(fs): use ctx
func (c *channelBroker) Close() error {
	var err error
	c.mu.Lock()
	for _, s := range c.s {
		s.Close()
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
