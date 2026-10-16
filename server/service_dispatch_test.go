package server

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uacp"
	"github.com/gopcua/opcua/uasc"
	"github.com/stretchr/testify/require"
)

// promptly bounds every answer that must not wait for a blocked request of
// another secure channel, or for a blocked earlier request of the same one.
const promptly = time.Second

// dumpGoroutines logs the stacks of all goroutines, so that a test that
// waited in vain shows where the server is stuck.
func dumpGoroutines(t *testing.T) {
	t.Helper()
	buf := make([]byte, 1<<22)
	n := runtime.Stack(buf, true)
	t.Logf("goroutines:\n%s", buf[:n])
}

// dispGoroutines counts the goroutines that serve secure channels: the
// receive loop of each channel and the goroutine that handles its requests.
func dispGoroutines() int {
	buf := make([]byte, 1<<22)
	n := runtime.Stack(buf, true)
	count := 0
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(g, "server.(*channelBroker).") {
			count++
		}
	}
	return count
}

func dispChannelCount(s *Server) int {
	s.cb.mu.RLock()
	defer s.cb.mu.RUnlock()
	return len(s.cb.s)
}

// dispCalls is a Call handler whose requests block until the test releases
// them, selected by requestHandle. It records the order in which it handles
// requests.
type dispCalls struct {
	mu      sync.Mutex
	blocked map[uint32]chan struct{}
	order   []uint32
	entered chan uint32
}

func newDispCalls() *dispCalls {
	return &dispCalls{
		blocked: map[uint32]chan struct{}{},
		entered: make(chan uint32, 16),
	}
}

// block makes the Call request with the given handle wait in the handler
// until release is called.
func (c *dispCalls) block(handle uint32) (release func()) {
	ch := make(chan struct{})
	c.mu.Lock()
	c.blocked[handle] = ch
	c.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

func (c *dispCalls) handle(sc *uasc.SecureChannel, r ua.Request, reqID uint32) (ua.Response, error) {
	req, err := safeReq[*ua.CallRequest](r)
	if err != nil {
		return nil, err
	}
	h := req.RequestHeader.RequestHandle
	c.mu.Lock()
	c.order = append(c.order, h)
	ch := c.blocked[h]
	c.mu.Unlock()
	if ch != nil {
		c.entered <- h
		<-ch
	}
	return &ua.CallResponse{
		ResponseHeader:  responseHeader(h, ua.StatusOK),
		Results:         []*ua.CallMethodResult{},
		DiagnosticInfos: []*ua.DiagnosticInfo{},
	}, nil
}

// waitEntered waits until the blocked request with the given handle is in
// the handler.
func (c *dispCalls) waitEntered(t *testing.T, handle uint32) {
	t.Helper()
	select {
	case h := <-c.entered:
		require.Equal(t, handle, h, "unexpected request entered the handler")
	case <-time.After(5 * time.Second):
		dumpGoroutines(t)
		t.Fatalf("request %d did not reach the handler", handle)
	}
}

func (c *dispCalls) handled() []uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]uint32(nil), c.order...)
}

// startDispServer starts a SecurityPolicy#None server on a loopback port
// whose Call service is calls, after setup if it is not nil. Every request
// blocked in calls is released before the server is closed.
func startDispServer(t *testing.T, calls *dispCalls, setup func(*Server)) (*Server, string) {
	t.Helper()
	srv := New(
		EndPoint("127.0.0.1", 0),
		EnableSecurity("None", ua.MessageSecurityModeNone),
		EnableAuthMode(ua.UserTokenTypeAnonymous),
	)
	if calls != nil {
		srv.RegisterHandler(id.CallRequest_Encoding_DefaultBinary, calls.handle)
	}
	if setup != nil {
		setup(srv)
	}
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, srv.Start(ctx))
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})
	if calls != nil {
		t.Cleanup(func() {
			calls.mu.Lock()
			defer calls.mu.Unlock()
			for h, ch := range calls.blocked {
				select {
				case <-ch:
				default:
					close(ch)
				}
				delete(calls.blocked, h)
			}
		})
	}
	return srv, "opc.tcp://" + srv.l.Addr().String()
}

// dispChannel is the client end of a SecurityPolicy#None secure channel that
// writes each request at once, without waiting for earlier responses, so
// that a test decides the order of the requests on the wire. A receive loop
// decodes every message from the server in arrival order.
type dispChannel struct {
	t    *testing.T
	conn *uacp.Conn

	mu        sync.Mutex // serializes writes and guards the fields below
	channelID uint32
	tokenID   uint32
	seq       uint32

	msgs   chan any
	closed chan struct{}
	err    error // why the receive loop ended; read after closed
}

func dialDispChannel(t *testing.T, endpoint string) *dispChannel {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := uacp.Dial(ctx, endpoint)
	require.NoError(t, err)
	c := &dispChannel{
		t:      t,
		conn:   conn,
		msgs:   make(chan any, 1024),
		closed: make(chan struct{}),
	}
	t.Cleanup(func() { _ = conn.Close() })
	go c.receive()

	c.openSecureChannel(ua.SecurityTokenRequestTypeIssue, 5*time.Second)
	return c
}

func (c *dispChannel) receive() {
	defer close(c.closed)
	for {
		b, err := c.conn.Receive()
		if err != nil {
			c.err = err
			return
		}
		m := new(uasc.Message)
		if _, err := m.Decode(b); err != nil {
			c.msgs <- fmt.Errorf("decode %s: %w", b[:3], err)
			continue
		}
		if _, ok := m.Service.(*ua.CloseSecureChannelRequest); ok {
			// The server closed the channel; drop the connection as a
			// client does.
			_ = c.conn.Close()
			return
		}
		c.msgs <- m.Service
	}
}

func (c *dispChannel) write(msgType string, svc any, typeID uint16, handle uint32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	hdr := &uasc.MessageHeader{
		Header:         uasc.NewHeader(msgType, uasc.ChunkTypeFinal, c.channelID),
		SequenceHeader: uasc.NewSequenceHeader(c.seq, handle),
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
	if err != nil {
		return err
	}
	_, err = c.conn.Write(b)
	return err
}

func requestHeader(handle uint32, authToken *ua.NodeID) *ua.RequestHeader {
	if authToken == nil {
		authToken = ua.NewTwoByteNodeID(0)
	}
	return &ua.RequestHeader{
		AuthenticationToken: authToken,
		Timestamp:           time.Now(),
		RequestHandle:       handle,
		TimeoutHint:         10000,
		AdditionalHeader:    ua.NewExtensionObject(nil),
	}
}

// send writes req with the given requestHandle, which is also its
// requestId, and does not wait for the response.
func (c *dispChannel) send(req ua.Request, handle uint32, authToken *ua.NodeID) {
	c.t.Helper()
	req.SetHeader(requestHeader(handle, authToken))
	require.NoError(c.t, c.write(uasc.MessageTypeMessage, req, ua.ServiceTypeID(req), handle))
}

// recv returns the next message from the server, or an error when none
// arrives within d.
func (c *dispChannel) recv(d time.Duration) (any, error) {
	select {
	case m := <-c.msgs:
		if err, ok := m.(error); ok {
			return nil, err
		}
		return m, nil
	case <-time.After(d):
		select {
		case <-c.closed:
			return nil, fmt.Errorf("no message from the server within %s; connection ended: %v", d, c.err)
		default:
			return nil, fmt.Errorf("no message from the server within %s", d)
		}
	}
}

// next returns the next message from the server and fails the test, after
// a goroutine dump, when none arrives within d.
func (c *dispChannel) next(d time.Duration) any {
	c.t.Helper()
	m, err := c.recv(d)
	if err != nil {
		dumpGoroutines(c.t)
		c.t.Fatal(err)
	}
	return m
}

// call sends req and waits up to d for its response.
func (c *dispChannel) call(req ua.Request, handle uint32, authToken *ua.NodeID, d time.Duration) ua.Response {
	c.t.Helper()
	c.send(req, handle, authToken)
	return requireAnswer(c.t, c.next(d), handle, ua.StatusOK)
}

func (c *dispChannel) openSecureChannel(typ ua.SecurityTokenRequestType, d time.Duration) {
	c.t.Helper()
	const handle = 0xfffffff0
	req := &ua.OpenSecureChannelRequest{
		RequestHeader:     requestHeader(handle, nil),
		RequestType:       typ,
		SecurityMode:      ua.MessageSecurityModeNone,
		ClientNonce:       []byte{},
		RequestedLifetime: 60000,
	}
	require.NoError(c.t, c.write(uasc.MessageTypeOpenSecureChannel, req, id.OpenSecureChannelRequest_Encoding_DefaultBinary, handle))
	resp, ok := c.next(d).(*ua.OpenSecureChannelResponse)
	require.True(c.t, ok, "want an OpenSecureChannelResponse")
	require.Equal(c.t, ua.StatusOK, resp.ResponseHeader.ServiceResult)
	c.mu.Lock()
	c.channelID = resp.SecurityToken.ChannelID
	c.tokenID = resp.SecurityToken.TokenID
	c.mu.Unlock()
}

func (c *dispChannel) closeSecureChannel() {
	c.t.Helper()
	const handle = 0xfffffff1
	req := &ua.CloseSecureChannelRequest{RequestHeader: requestHeader(handle, nil)}
	require.NoError(c.t, c.write(uasc.MessageTypeCloseSecureChannel, req, id.CloseSecureChannelRequest_Encoding_DefaultBinary, handle))
}

// requireAnswer checks that m is a response to the request with the given
// handle and service result.
func requireAnswer(t *testing.T, m any, handle uint32, status ua.StatusCode) ua.Response {
	t.Helper()
	resp, ok := m.(ua.Response)
	require.True(t, ok, "want a response, got %T", m)
	require.Equal(t, handle, resp.Header().RequestHandle, "requestHandle of %T", resp)
	require.Equal(t, status, resp.Header().ServiceResult, "service result of %T", resp)
	return resp
}

func callRequest() *ua.CallRequest { return &ua.CallRequest{} }

func readCurrentTimeRequest() *ua.ReadRequest {
	return &ua.ReadRequest{
		TimestampsToReturn: ua.TimestampsToReturnBoth,
		NodesToRead: []*ua.ReadValueID{{
			NodeID:       ua.NewNumericNodeID(0, id.Server_ServerStatus_CurrentTime),
			AttributeID:  ua.AttributeIDValue,
			DataEncoding: &ua.QualifiedName{},
		}},
	}
}

// openSession runs CreateSession and ActivateSession on c, each answered
// within d, and returns the authentication token.
func openSession(t *testing.T, c *dispChannel, endpoint string, handle uint32, d time.Duration) *ua.NodeID {
	t.Helper()
	cs := c.call(&ua.CreateSessionRequest{
		ClientDescription: &ua.ApplicationDescription{
			ApplicationURI:  "urn:dispatch-test",
			ApplicationName: &ua.LocalizedText{EncodingMask: ua.LocalizedTextText, Text: "dispatch-test"},
			ApplicationType: ua.ApplicationTypeClient,
		},
		EndpointURL:             endpoint,
		SessionName:             "dispatch-test",
		ClientNonce:             make([]byte, 32),
		RequestedSessionTimeout: 60000,
	}, handle, nil, d).(*ua.CreateSessionResponse)
	token := cs.AuthenticationToken
	c.call(activateSessionRequest(), handle+1, token, d)
	return token
}

func activateSessionRequest() *ua.ActivateSessionRequest {
	return &ua.ActivateSessionRequest{
		ClientSignature:    &ua.SignatureData{},
		UserIdentityToken:  ua.NewExtensionObject(&ua.AnonymousIdentityToken{PolicyID: "anonymous"}),
		UserTokenSignature: &ua.SignatureData{},
	}
}

// A request whose handler has not returned delays no request of another
// secure channel. Opening a channel, and creating, activating and using a
// Session on it, are answered at once.
func TestDispatchSlowRequestDoesNotDelayOtherChannels(t *testing.T) {
	calls := newDispCalls()
	_, endpoint := startDispServer(t, calls, nil)

	a := dialDispChannel(t, endpoint)
	release := calls.block(1)
	a.send(callRequest(), 1, nil)
	calls.waitEntered(t, 1)
	a.send(callRequest(), 2, nil)

	b := dialDispChannel(t, endpoint)
	token := openSession(t, b, endpoint, 10, promptly)
	b.call(readCurrentTimeRequest(), 12, token, promptly)

	release()
	requireAnswer(t, a.next(5*time.Second), 1, ua.StatusOK)
	requireAnswer(t, a.next(5*time.Second), 2, ua.StatusOK)
}

// The requests of one secure channel are handled one at a time, in the
// order they arrive, and answered in that order.
func TestDispatchAnswersInRequestOrder(t *testing.T) {
	const n = 20
	calls := newDispCalls()
	_, endpoint := startDispServer(t, calls, nil)

	a := dialDispChannel(t, endpoint)
	release := calls.block(1)
	for h := uint32(1); h <= n; h++ {
		a.send(callRequest(), h, nil)
	}
	calls.waitEntered(t, 1)
	release()

	for h := uint32(1); h <= n; h++ {
		requireAnswer(t, a.next(5*time.Second), h, ua.StatusOK)
	}
	want := make([]uint32, 0, n)
	for h := uint32(1); h <= n; h++ {
		want = append(want, h)
	}
	require.Equal(t, want, calls.handled())
}

// While a request of a secure channel is handled, at most maxQueuedRequests
// further requests of that channel wait. A request beyond that is answered
// at once with Bad_ResourceUnavailable ("An operating system resource is not
// available.", Part 4 §7.38.2 Table 178) and its own requestHandle in the
// ResponseHeader (Part 4 §7.33), and is not handled. The waiting requests
// are handled after the blocked one, and other channels are served
// throughout.
func TestDispatchBoundedBacklog(t *testing.T) {
	const extra = 3
	calls := newDispCalls()
	_, endpoint := startDispServer(t, calls, nil)

	b := dialDispChannel(t, endpoint)
	token := openSession(t, b, endpoint, 1000, promptly)

	a := dialDispChannel(t, endpoint)
	release := calls.block(1)
	a.send(callRequest(), 1, nil)
	calls.waitEntered(t, 1)

	last := uint32(1 + maxQueuedRequests)
	for h := uint32(2); h <= last; h++ {
		a.send(callRequest(), h, nil)
	}
	for h := last + 1; h <= last+extra; h++ {
		a.send(callRequest(), h, nil)
		f, ok := a.next(promptly).(*ua.ServiceFault)
		require.True(t, ok, "request %d beyond the backlog: want a ServiceFault", h)
		require.Equal(t, h, f.ResponseHeader.RequestHandle)
		require.Equal(t, ua.StatusBadResourceUnavailable, f.ResponseHeader.ServiceResult)

		b.call(readCurrentTimeRequest(), 1000+h, token, promptly)
	}

	release()
	for h := uint32(1); h <= last; h++ {
		requireAnswer(t, a.next(5*time.Second), h, ua.StatusOK)
	}
	for _, h := range calls.handled() {
		require.LessOrEqual(t, h, last, "request %d beyond the backlog was handled", h)
	}
}

// When a secure channel ends while one of its requests is handled, the
// channel's goroutines end once the handler returns; requests still waiting
// are dropped without a response. The channel keeps reading while the
// handler runs, so CloseSecureChannel and OpenSecureChannel renewals (Part 4
// §5.5.3, §5.5.2) are processed, and the server closes within its bound.
func TestDispatchChannelEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, srv *Server, a *dispChannel)
	}{
		{
			name: "client sends CloseSecureChannel",
			end: func(t *testing.T, srv *Server, a *dispChannel) {
				a.closeSecureChannel()
			},
		},
		{
			name: "client closes the connection",
			end: func(t *testing.T, srv *Server, a *dispChannel) {
				require.NoError(t, a.conn.Close())
			},
		},
		{
			name: "server closes",
			end: func(t *testing.T, srv *Server, a *dispChannel) {
				// Close returns once the receive loops of the channels have
				// ended, after at most 10 s (channelBroker.Close); the
				// handler that has not returned does not hold it. The
				// client drops the connection as the server goes away.
				done := make(chan struct{})
				go func() {
					defer close(done)
					_ = srv.Close()
				}()
				require.NoError(t, a.conn.Close())
				select {
				case <-done:
				case <-time.After(10*time.Second + promptly):
					dumpGoroutines(t)
					t.Fatal("server Close did not return within its bound")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := dispGoroutines()
			calls := newDispCalls()
			srv, endpoint := startDispServer(t, calls, nil)

			a := dialDispChannel(t, endpoint)
			release := calls.block(1)
			a.send(callRequest(), 1, nil)
			calls.waitEntered(t, 1)
			a.send(callRequest(), 2, nil)
			a.send(callRequest(), 3, nil)

			tc.end(t, srv, a)
			require.Eventually(t, func() bool { return dispChannelCount(srv) == 0 }, promptly, 10*time.Millisecond,
				"the channel did not end while its request was handled")

			release()
			require.Eventually(t, func() bool { return dispGoroutines() <= before }, 5*time.Second, 10*time.Millisecond,
				"goroutines of the ended channel remain")
			require.Equal(t, []uint32{1}, calls.handled(), "waiting requests of the ended channel were handled")
			select {
			case m := <-a.msgs:
				t.Fatalf("the ended channel was answered: %#v", m)
			default:
			}
		})
	}

	t.Run("renewal", func(t *testing.T) {
		calls := newDispCalls()
		_, endpoint := startDispServer(t, calls, nil)

		a := dialDispChannel(t, endpoint)
		release := calls.block(1)
		a.send(callRequest(), 1, nil)
		calls.waitEntered(t, 1)
		a.send(callRequest(), 2, nil)

		a.openSecureChannel(ua.SecurityTokenRequestTypeRenew, promptly)

		release()
		requireAnswer(t, a.next(5*time.Second), 1, ua.StatusOK)
		requireAnswer(t, a.next(5*time.Second), 2, ua.StatusOK)
		a.call(callRequest(), 3, nil, promptly)
	})
}

// Requests of different secure channels are handled concurrently. Reads,
// Writes and Browses of the same nodes and repeated activation of one Session
// from several channels (Part 4 §5.6.3) share server state; run under -race.
func TestDispatchConcurrentHandlers(t *testing.T) {
	const (
		channels = 4
		rounds   = 50
	)
	var node, mapNode *ua.NodeID
	_, endpoint := startDispServer(t, nil, func(srv *Server) {
		ns := NewNodeNameSpace(srv, "dispatch")
		v := ns.AddNewVariableStringNode("v", int32(0))
		ns.Objects().AddRef(v, RefTypeIDHasComponent, true)
		node = v.ID()
		m := NewMapNamespace(srv, "dispatch-map")
		m.Data["k"] = int32(0)
		mapNode = ua.NewStringNodeID(m.ID(), "k")
	})

	owner := dialDispChannel(t, endpoint)
	token := openSession(t, owner, endpoint, 1, 5*time.Second)

	var wg sync.WaitGroup
	errs := make(chan error, channels)
	for i := 0; i < channels; i++ {
		c := dialDispChannel(t, endpoint)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- concurrentRounds(c, i, rounds, token, node, mapNode)
		}(i)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		dumpGoroutines(t)
		t.Fatal("concurrent requests did not complete")
	}
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func concurrentRounds(c *dispChannel, i, rounds int, token, node, mapNode *ua.NodeID) error {
	handle := uint32(i*10000 + 100)
	call := func(req ua.Request, authToken *ua.NodeID) error {
		handle++
		req.SetHeader(requestHeader(handle, authToken))
		if err := c.write(uasc.MessageTypeMessage, req, ua.ServiceTypeID(req), handle); err != nil {
			return err
		}
		m, err := c.recv(10 * time.Second)
		if err != nil {
			return fmt.Errorf("%T: %w", req, err)
		}
		resp, ok := m.(ua.Response)
		if !ok {
			return fmt.Errorf("%T: got %T", req, m)
		}
		if got := resp.Header().RequestHandle; got != handle {
			return fmt.Errorf("%T: requestHandle %d, want %d", req, got, handle)
		}
		if f, ok := resp.(*ua.ServiceFault); ok {
			return fmt.Errorf("%T: %s", req, f.ResponseHeader.ServiceResult)
		}
		return nil
	}
	for r := 0; r < rounds; r++ {
		value := int32(i*1000 + r)
		writes := &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{
			{NodeID: node, AttributeID: ua.AttributeIDValue, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(value)}},
			{NodeID: node, AttributeID: ua.AttributeIDDisplayName, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(&ua.LocalizedText{EncodingMask: ua.LocalizedTextText, Text: fmt.Sprintf("v%d", value)})}},
			{NodeID: mapNode, AttributeID: ua.AttributeIDValue, Value: &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(value)}},
		}}
		reads := &ua.ReadRequest{TimestampsToReturn: ua.TimestampsToReturnBoth, NodesToRead: []*ua.ReadValueID{
			{NodeID: node, AttributeID: ua.AttributeIDValue, DataEncoding: &ua.QualifiedName{}},
			{NodeID: node, AttributeID: ua.AttributeIDDisplayName, DataEncoding: &ua.QualifiedName{}},
			{NodeID: mapNode, AttributeID: ua.AttributeIDValue, DataEncoding: &ua.QualifiedName{}},
		}}
		browse := &ua.BrowseRequest{View: &ua.ViewDescription{ViewID: ua.NewTwoByteNodeID(0)}, NodesToBrowse: []*ua.BrowseDescription{{
			NodeID:          ua.NewNumericNodeID(node.Namespace(), id.ObjectsFolder),
			BrowseDirection: ua.BrowseDirectionBoth,
			ReferenceTypeID: ua.NewNumericNodeID(0, id.References),
			IncludeSubtypes: true,
			ResultMask:      uint32(ua.BrowseResultMaskAll),
		}}}
		for _, step := range []struct {
			req   ua.Request
			token *ua.NodeID
		}{
			{writes, nil},
			{reads, nil},
			{browse, nil},
			{activateSessionRequest(), token},
		} {
			if err := call(step.req, step.token); err != nil {
				return fmt.Errorf("channel %d round %d: %w", i, r, err)
			}
		}
	}
	return nil
}
