package server

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"
	"github.com/stretchr/testify/require"
)

const sessionCheckRequestHandle = 23

// sessionCheckServer returns a server with its handlers initialized. Every
// Subscription still present when the test ends is deleted, so that no
// publish loop outlives the test.
func sessionCheckServer(t *testing.T) *Server {
	t.Helper()
	srv := New()
	srv.initHandlers()
	t.Cleanup(func() {
		svc := srv.SubscriptionService
		svc.Mu.Lock()
		ids := make([]uint32, 0, len(svc.Subs))
		for id := range svc.Subs {
			ids = append(ids, id)
		}
		svc.Mu.Unlock()
		for _, id := range ids {
			svc.DeleteSubscription(id)
		}
	})
	return srv
}

// callService invokes the handler registered for req. A panic is reported as
// an error, so that it fails the test instead of the test binary.
func callService(srv *Server, req ua.Request) (resp ua.Response, err error) {
	h, ok := srv.handlers[ua.ServiceTypeID(req)]
	if !ok {
		return nil, ua.StatusBadServiceUnsupported
	}
	defer func() {
		if r := recover(); r != nil {
			resp, err = nil, panicError{r}
		}
	}()
	return h(nil, req, 0)
}

type panicError struct{ v any }

func (e panicError) Error() string { return fmt.Sprintf("handler panicked: %v", e.v) }

func createSubscriptionRequest(token *ua.NodeID) *ua.CreateSubscriptionRequest {
	return &ua.CreateSubscriptionRequest{
		RequestHeader: &ua.RequestHeader{
			AuthenticationToken: token,
			RequestHandle:       sessionCheckRequestHandle,
		},
		RequestedPublishingInterval: 100,
		RequestedLifetimeCount:      100000,
		RequestedMaxKeepAliveCount:  0,
		PublishingEnabled:           true,
	}
}

func subscriptionsOf(srv *Server) map[uint32]*Subscription {
	svc := srv.SubscriptionService
	svc.Mu.Lock()
	defer svc.Mu.Unlock()
	subs := make(map[uint32]*Subscription, len(svc.Subs))
	for id, sub := range svc.Subs {
		subs[id] = sub
	}
	return subs
}

// requireSessionIDInvalid asserts that resp is a ServiceFault with
// Bad_SessionIdInvalid that echoes the request's RequestHandle
// (Part 4 v1.05 §7.34).
func requireSessionIDInvalid(t *testing.T, resp ua.Response) {
	t.Helper()
	fault, ok := resp.(*ua.ServiceFault)
	require.True(t, ok, "want *ua.ServiceFault, got %T", resp)
	require.Equal(t, ua.StatusBadSessionIDInvalid, fault.ResponseHeader.ServiceResult)
	require.Equal(t, uint32(sessionCheckRequestHandle), fault.ResponseHeader.RequestHandle)
}

// noSessionTokens returns authentication tokens that identify no Session of
// srv: none, one never issued, and one of a Session that was closed.
func noSessionTokens(t *testing.T, srv *Server) map[string]*ua.NodeID {
	t.Helper()
	closed := srv.sb.NewSession()
	resp, err := callService(srv, &ua.CloseSessionRequest{
		RequestHeader: &ua.RequestHeader{AuthenticationToken: closed.AuthTokenID},
	})
	require.NoError(t, err)
	require.Equal(t, ua.StatusOK, resp.(*ua.CloseSessionResponse).ResponseHeader.ServiceResult)
	return map[string]*ua.NodeID{
		"no token":     nil,
		"null token":   ua.NewTwoByteNodeID(0),
		"never issued": ua.NewStringNodeID(1, "never-issued"),
		"closed":       closed.AuthTokenID,
	}
}

// TestCreateSubscriptionWithoutSession checks that CreateSubscription with an
// authenticationToken that identifies no Session is answered with a
// ServiceFault Bad_SessionIdInvalid and creates and starts no Subscription.
func TestCreateSubscriptionWithoutSession(t *testing.T) {
	srv := sessionCheckServer(t)
	for name, token := range noSessionTokens(t, srv) {
		t.Run(name, func(t *testing.T) {
			before := runtime.NumGoroutine()
			resp, err := callService(srv, createSubscriptionRequest(token))
			require.NoError(t, err)
			requireSessionIDInvalid(t, resp)
			require.Empty(t, subscriptionsOf(srv), "no Subscription may be created")
			require.LessOrEqual(t, runtime.NumGoroutine(), before, "no publish loop may be started")
		})
	}
}

// TestDeleteSubscriptionsWithoutSession checks that DeleteSubscriptions with
// an authenticationToken that identifies no Session is answered with a
// ServiceFault Bad_SessionIdInvalid, whatever subscriptionIds holds, and
// deletes nothing.
func TestDeleteSubscriptionsWithoutSession(t *testing.T) {
	srv := sessionCheckServer(t)
	owner := srv.sb.NewSession()
	resp, err := callService(srv, createSubscriptionRequest(owner.AuthTokenID))
	require.NoError(t, err)
	created, ok := resp.(*ua.CreateSubscriptionResponse)
	require.True(t, ok, "want *ua.CreateSubscriptionResponse, got %T", resp)
	subID := created.SubscriptionID
	sub := subscriptionsOf(srv)[subID]
	require.NotNil(t, sub)

	for name, token := range noSessionTokens(t, srv) {
		for _, ids := range [][]uint32{{subID, subID + 1000}, {subID}, {}} {
			t.Run(fmt.Sprintf("%s/ids=%v", name, ids), func(t *testing.T) {
				resp, err := callService(srv, &ua.DeleteSubscriptionsRequest{
					RequestHeader: &ua.RequestHeader{
						AuthenticationToken: token,
						RequestHandle:       sessionCheckRequestHandle,
					},
					SubscriptionIDs: ids,
				})
				require.NoError(t, err)
				requireSessionIDInvalid(t, resp)

				require.Same(t, sub, subscriptionsOf(srv)[subID], "the Subscription must not be deleted")
				sub.Mu.Lock()
				running := sub.running
				sub.Mu.Unlock()
				require.True(t, running, "the Subscription must keep running")
			})
		}
	}
}

// TestCreateSubscriptionRacesCloseSession runs CreateSubscription against a
// concurrent CloseSession of the same Session, followed by a deletion of the
// Session's Subscriptions. CreateSubscription either creates a Subscription
// of the Session, which the deletion finds, or answers Bad_SessionIdInvalid.
// No Subscription is ever stored without a Session, and a CreateSubscription
// after the close always answers Bad_SessionIdInvalid.
func TestCreateSubscriptionRacesCloseSession(t *testing.T) {
	srv := sessionCheckServer(t)
	svc := srv.SubscriptionService

	for i := 0; i < 200; i++ {
		sess := srv.sb.NewSession()
		token := sess.AuthTokenID

		var (
			wg         sync.WaitGroup
			start      = make(chan struct{})
			createResp ua.Response
			createErr  error
			closeResp  ua.Response
			closeErr   error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			createResp, createErr = callService(srv, createSubscriptionRequest(token))
		}()
		go func() {
			defer wg.Done()
			<-start
			closeResp, closeErr = callService(srv, &ua.CloseSessionRequest{
				RequestHeader:       &ua.RequestHeader{AuthenticationToken: token},
				DeleteSubscriptions: true,
			})
			if closeErr != nil {
				return
			}
			// Delete what is left of the Session's Subscriptions, as a
			// server that cleans up on close does.
			svc.Mu.Lock()
			var ids []uint32
			for id, sub := range svc.Subs {
				if sub.Session == sess {
					ids = append(ids, id)
				}
			}
			svc.Mu.Unlock()
			for _, id := range ids {
				svc.DeleteSubscription(id)
			}
		}()
		close(start)
		wg.Wait()

		require.NoError(t, closeErr, "iteration %d: CloseSession", i)
		require.Equal(t, ua.StatusOK, closeResp.(*ua.CloseSessionResponse).ResponseHeader.ServiceResult)
		require.NoError(t, createErr, "iteration %d: CreateSubscription", i)
		switch resp := createResp.(type) {
		case *ua.CreateSubscriptionResponse:
			require.Equal(t, ua.StatusOK, resp.ResponseHeader.ServiceResult, "iteration %d", i)
		case *ua.ServiceFault:
			requireSessionIDInvalid(t, resp)
		default:
			t.Fatalf("iteration %d: unexpected response %T", i, createResp)
		}

		for id, sub := range subscriptionsOf(srv) {
			require.NotNil(t, sub.Session, "iteration %d: Subscription %d has no Session", i, id)
			require.NotSame(t, sess, sub.Session, "iteration %d: Subscription %d outlived its Session", i, id)
		}

		resp, err := callService(srv, createSubscriptionRequest(token))
		require.NoError(t, err, "iteration %d: CreateSubscription after close", i)
		requireSessionIDInvalid(t, resp)
	}
	require.Empty(t, subscriptionsOf(srv))
}

// TestCreateSubscriptionQueuedBehindSessionClose checks that a
// CreateSubscription waiting for SubscriptionService.Mu while its Session is
// closed answers Bad_SessionIdInvalid and creates nothing: the Session is
// looked up while Mu is held, not before.
func TestCreateSubscriptionQueuedBehindSessionClose(t *testing.T) {
	srv := sessionCheckServer(t)
	svc := srv.SubscriptionService
	sess := srv.sb.NewSession()

	type result struct {
		resp ua.Response
		err  error
	}
	done := make(chan result, 1)

	svc.Mu.Lock()
	go func() {
		resp, err := callService(srv, createSubscriptionRequest(sess.AuthTokenID))
		done <- result{resp, err}
	}()
	if !waitBlockedOnLock(fmt.Sprintf("(*SubscriptionService).CreateSubscription(%p", svc), 5*time.Second) {
		svc.Mu.Unlock()
		t.Fatal("CreateSubscription never blocked on SubscriptionService.Mu")
	}
	require.NoError(t, srv.sb.Close(sess.AuthTokenID))
	svc.Mu.Unlock()

	r := <-done
	require.NoError(t, r.err)
	requireSessionIDInvalid(t, r.resp)
	require.Empty(t, subscriptionsOf(srv), "no Subscription may be created")
}

// waitBlockedOnLock reports whether, within timeout, a goroutine whose stack
// contains frame waits to acquire a sync.Mutex.
func waitBlockedOnLock(frame string, timeout time.Duration) bool {
	buf := make([]byte, 1<<20)
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, frame) && strings.Contains(g, "lockSlow") {
				return true
			}
		}
	}
	return false
}
