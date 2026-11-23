package server

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
	"github.com/stretchr/testify/require"
)

// limitsTestServer returns a server with the given options and its handlers
// initialized. Every Subscription still present when the test ends is
// deleted.
func limitsTestServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	srv := New(opts...)
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

// createSubscription calls CreateSubscription for sess and returns the
// service result and, when it is Good, the new subscription id. A request
// that is refused must be answered with a ServiceFault that carries the
// request's RequestHandle (Part 4 §7.33, §7.34); a CreateSubscriptionResponse
// with a bad ServiceResult is an error. It does not use t, so that it can
// run on other goroutines.
func createSubscription(srv *Server, sess *session, lifetime uint32) (ua.StatusCode, uint32, error) {
	const handle = 7
	resp, err := srv.SubscriptionService.CreateSubscription(nil, &ua.CreateSubscriptionRequest{
		RequestHeader:               &ua.RequestHeader{AuthenticationToken: sess.AuthTokenID, RequestHandle: handle},
		RequestedPublishingInterval: 10,
		RequestedLifetimeCount:      lifetime,
		RequestedMaxKeepAliveCount:  0,
		PublishingEnabled:           true,
	}, 0)
	if err != nil {
		return 0, 0, err
	}
	switch resp := resp.(type) {
	case *ua.CreateSubscriptionResponse:
		if status := resp.ResponseHeader.ServiceResult; status != ua.StatusOK {
			return 0, 0, fmt.Errorf("refusal %s sent as CreateSubscriptionResponse, want ServiceFault", status)
		}
		return ua.StatusOK, resp.SubscriptionID, nil
	case *ua.ServiceFault:
		if resp.ResponseHeader.RequestHandle != handle {
			return 0, 0, fmt.Errorf("ServiceFault carries RequestHandle %d, want %d", resp.ResponseHeader.RequestHandle, handle)
		}
		if resp.ResponseHeader.ServiceResult == ua.StatusOK {
			return 0, 0, fmt.Errorf("ServiceFault with a Good ServiceResult")
		}
		return resp.ResponseHeader.ServiceResult, 0, nil
	default:
		return 0, 0, fmt.Errorf("unexpected response %T", resp)
	}
}

// longLifetime keeps a Subscription alive for the duration of a test.
const longLifetime = 100000

// create asserts that CreateSubscription for sess returns want and returns
// the new subscription id (0 when the request was refused).
func create(t *testing.T, srv *Server, sess *session, want ua.StatusCode) uint32 {
	t.Helper()
	status, subID, err := createSubscription(srv, sess, longLifetime)
	require.NoError(t, err)
	require.Equal(t, want, status, "CreateSubscription service result")
	return subID
}

func liveSubscriptions(srv *Server) map[uint32]*Subscription {
	svc := srv.SubscriptionService
	svc.Mu.Lock()
	defer svc.Mu.Unlock()
	subs := make(map[uint32]*Subscription, len(svc.Subs))
	for id, sub := range svc.Subs {
		subs[id] = sub
	}
	return subs
}

func deleteSubscriptions(t *testing.T, srv *Server, sess *session, ids ...uint32) {
	t.Helper()
	resp, err := srv.SubscriptionService.DeleteSubscriptions(nil, &ua.DeleteSubscriptionsRequest{
		RequestHeader:   &ua.RequestHeader{AuthenticationToken: sess.AuthTokenID},
		SubscriptionIDs: ids,
	}, 0)
	require.NoError(t, err)
	for i, status := range resp.(*ua.DeleteSubscriptionsResponse).Results {
		require.Equal(t, ua.StatusOK, status, "result of subscription %d", ids[i])
	}
}

func closeSession(t *testing.T, srv *Server, sess *session, deleteSubscriptions bool) {
	t.Helper()
	sessions := &SessionService{srv}
	resp, err := sessions.CloseSession(nil, &ua.CloseSessionRequest{
		RequestHeader:       &ua.RequestHeader{AuthenticationToken: sess.AuthTokenID},
		DeleteSubscriptions: deleteSubscriptions,
	}, 0)
	require.NoError(t, err)
	require.Equal(t, ua.StatusOK, resp.(*ua.CloseSessionResponse).ResponseHeader.ServiceResult)
}

// TestSubscriptionLimitOptions checks how the options map onto the reported
// capabilities: a non-positive value means no limit and is reported as 0, a
// value above the UInt32 range is reported as the largest UInt32.
func TestSubscriptionLimitOptions(t *testing.T) {
	tests := []struct {
		name string
		n    int
		want uint32
	}{
		{"unset", 0, 0},
		{"negative", -1, 0},
		{"one", 1, 1},
		{"max uint32", math.MaxUint32, math.MaxUint32},
		{"above uint32", math.MaxUint32 + 1, math.MaxUint32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := New(MaxSubscriptions(tt.n), MaxSubscriptionsPerSession(tt.n))
			require.Equal(t, tt.want, srv.cfg.cap.MaxSubscriptions)
			require.Equal(t, tt.want, srv.cfg.cap.MaxSubscriptionsPerSession)
		})
	}
}

// TestCreateSubscriptionLimits checks that CreateSubscription answers
// Bad_TooManySubscriptions (Part 4 §5.14.2.4, Table 83) while the per-session
// or the server-wide limit is reached, and that a refused request leaves every
// existing Subscription in place.
func TestCreateSubscriptionLimits(t *testing.T) {
	t.Run("per session", func(t *testing.T) {
		srv := limitsTestServer(t, MaxSubscriptionsPerSession(3))
		a, b := srv.sb.NewSession(), srv.sb.NewSession()
		for range 3 {
			create(t, srv, a, ua.StatusOK)
		}
		before := liveSubscriptions(srv)
		create(t, srv, a, ua.StatusBadTooManySubscriptions)
		require.Equal(t, before, liveSubscriptions(srv), "a refused request must not change the subscriptions")
		for _, sub := range before {
			sub.Mu.Lock()
			running := sub.running
			sub.Mu.Unlock()
			require.True(t, running, "subscription %d stopped", sub.ID)
		}
		for range 3 {
			create(t, srv, b, ua.StatusOK)
		}
		create(t, srv, b, ua.StatusBadTooManySubscriptions)
	})

	t.Run("server wide", func(t *testing.T) {
		srv := limitsTestServer(t, MaxSubscriptions(4))
		a, b, c := srv.sb.NewSession(), srv.sb.NewSession(), srv.sb.NewSession()
		create(t, srv, a, ua.StatusOK)
		create(t, srv, a, ua.StatusOK)
		create(t, srv, b, ua.StatusOK)
		create(t, srv, b, ua.StatusOK)
		create(t, srv, a, ua.StatusBadTooManySubscriptions)
		create(t, srv, b, ua.StatusBadTooManySubscriptions)
		create(t, srv, c, ua.StatusBadTooManySubscriptions)
		require.Len(t, liveSubscriptions(srv), 4)
	})

	t.Run("both", func(t *testing.T) {
		srv := limitsTestServer(t, MaxSubscriptionsPerSession(3), MaxSubscriptions(4))
		a, b := srv.sb.NewSession(), srv.sb.NewSession()
		for range 3 {
			create(t, srv, a, ua.StatusOK)
		}
		create(t, srv, a, ua.StatusBadTooManySubscriptions)
		create(t, srv, b, ua.StatusOK)
		create(t, srv, b, ua.StatusBadTooManySubscriptions)
	})

	t.Run("no limit", func(t *testing.T) {
		srv := limitsTestServer(t)
		sessions := []*session{srv.sb.NewSession(), srv.sb.NewSession(), srv.sb.NewSession()}
		for _, sess := range sessions {
			for range 50 {
				create(t, srv, sess, ua.StatusOK)
			}
		}
		require.Len(t, liveSubscriptions(srv), 150)
	})
}

// TestSubscriptionLimitsConcurrentCreate checks that concurrent
// CreateSubscription requests never exceed the server-wide limit.
func TestSubscriptionLimitsConcurrentCreate(t *testing.T) {
	const limit = 10
	srv := limitsTestServer(t, MaxSubscriptions(limit))
	sessions := []*session{srv.sb.NewSession(), srv.sb.NewSession(), srv.sb.NewSession()}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = map[ua.StatusCode]int{}
		errs    []error
	)
	for i := range 60 {
		wg.Add(1)
		go func(sess *session) {
			defer wg.Done()
			status, _, err := createSubscription(srv, sess, longLifetime)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results[status]++
		}(sessions[i%len(sessions)])
	}
	wg.Wait()
	require.Empty(t, errs)
	require.Equal(t, map[ua.StatusCode]int{ua.StatusOK: limit, ua.StatusBadTooManySubscriptions: 50}, results)
	require.Len(t, liveSubscriptions(srv), limit)
}

// TestSubscriptionLimitsReleasedWhenSubscriptionEnds checks that every way a
// Subscription ends frees its place before the request that ends it is
// answered: DeleteSubscriptions (Part 4 §5.14.8), CloseSession with
// deleteSubscriptions TRUE (Part 4 §5.7.4.2) and lifetime expiry
// (Part 4 §5.14.1.1). With deleteSubscriptions FALSE the Subscriptions are
// kept until their lifetime expires.
//
// CreateSubscription derives a new id from len(Subs), so a new Subscription
// can be given the id of one still in Subs once an earlier one has ended.
// The subtests therefore end the Subscriptions created last.
func TestSubscriptionLimitsReleasedWhenSubscriptionEnds(t *testing.T) {
	t.Run("DeleteSubscriptions per session", func(t *testing.T) {
		srv := limitsTestServer(t, MaxSubscriptionsPerSession(3))
		a := srv.sb.NewSession()
		ids := []uint32{create(t, srv, a, ua.StatusOK), create(t, srv, a, ua.StatusOK), create(t, srv, a, ua.StatusOK)}
		create(t, srv, a, ua.StatusBadTooManySubscriptions)

		deleteSubscriptions(t, srv, a, ids[2])
		require.NotContains(t, liveSubscriptions(srv), ids[2], "deleted before the response")
		create(t, srv, a, ua.StatusOK)
		create(t, srv, a, ua.StatusBadTooManySubscriptions)
	})

	t.Run("DeleteSubscriptions server wide", func(t *testing.T) {
		srv := limitsTestServer(t, MaxSubscriptions(4))
		a, b := srv.sb.NewSession(), srv.sb.NewSession()
		create(t, srv, a, ua.StatusOK)
		create(t, srv, a, ua.StatusOK)
		b1 := create(t, srv, b, ua.StatusOK)
		b2 := create(t, srv, b, ua.StatusOK)
		create(t, srv, a, ua.StatusBadTooManySubscriptions)

		deleteSubscriptions(t, srv, b, b1, b2)
		create(t, srv, a, ua.StatusOK)
		create(t, srv, a, ua.StatusOK)
		create(t, srv, a, ua.StatusBadTooManySubscriptions)
	})

	t.Run("CloseSession deleteSubscriptions TRUE", func(t *testing.T) {
		srv := limitsTestServer(t, MaxSubscriptions(4))
		a, b := srv.sb.NewSession(), srv.sb.NewSession()
		create(t, srv, a, ua.StatusOK)
		create(t, srv, a, ua.StatusOK)
		b1 := create(t, srv, b, ua.StatusOK)
		b2 := create(t, srv, b, ua.StatusOK)

		closeSession(t, srv, b, true)
		live := liveSubscriptions(srv)
		require.NotContains(t, live, b1)
		require.NotContains(t, live, b2)
		require.Len(t, live, 2, "the other session's subscriptions stay")

		c := srv.sb.NewSession()
		create(t, srv, c, ua.StatusOK)
		create(t, srv, c, ua.StatusOK)
		create(t, srv, c, ua.StatusBadTooManySubscriptions)
	})

	t.Run("CloseSession deleteSubscriptions FALSE", func(t *testing.T) {
		srv := limitsTestServer(t, MaxSubscriptions(2))
		a := srv.sb.NewSession()
		create(t, srv, a, ua.StatusOK)
		// Expires a few publishing intervals after the session is closed.
		status, short, err := createSubscription(srv, a, 3)
		require.NoError(t, err)
		require.Equal(t, ua.StatusOK, status)

		closeSession(t, srv, a, false)
		require.Len(t, liveSubscriptions(srv), 2, "kept until the lifetime expires")
		c := srv.sb.NewSession()
		create(t, srv, c, ua.StatusBadTooManySubscriptions)

		require.Eventually(t, func() bool {
			_, ok := liveSubscriptions(srv)[short]
			return !ok
		}, 10*time.Second, 5*time.Millisecond)
		create(t, srv, c, ua.StatusOK)
		create(t, srv, c, ua.StatusBadTooManySubscriptions)
	})

	t.Run("lifetime expiry", func(t *testing.T) {
		srv := limitsTestServer(t, MaxSubscriptionsPerSession(2))
		a := srv.sb.NewSession()
		create(t, srv, a, ua.StatusOK)
		status, short, err := createSubscription(srv, a, 3)
		require.NoError(t, err)
		require.Equal(t, ua.StatusOK, status)
		create(t, srv, a, ua.StatusBadTooManySubscriptions)

		require.Eventually(t, func() bool {
			_, ok := liveSubscriptions(srv)[short]
			return !ok
		}, 10*time.Second, 5*time.Millisecond)
		create(t, srv, a, ua.StatusOK)
		create(t, srv, a, ua.StatusBadTooManySubscriptions)
	})

	t.Run("repeated create and delete", func(t *testing.T) {
		srv := limitsTestServer(t, MaxSubscriptionsPerSession(3), MaxSubscriptions(4))
		a, b := srv.sb.NewSession(), srv.sb.NewSession()
		create(t, srv, a, ua.StatusOK)
		create(t, srv, a, ua.StatusOK)
		create(t, srv, b, ua.StatusOK)
		for range 200 {
			subID := create(t, srv, a, ua.StatusOK)
			create(t, srv, a, ua.StatusBadTooManySubscriptions)
			create(t, srv, b, ua.StatusBadTooManySubscriptions)
			deleteSubscriptions(t, srv, a, subID)
		}
		create(t, srv, b, ua.StatusOK)
		create(t, srv, b, ua.StatusBadTooManySubscriptions)
	})
}

// TestDeleteSessionSubscriptions checks that DeleteSessionSubscriptions ends
// the Subscriptions of one Session, and only those.
func TestDeleteSessionSubscriptions(t *testing.T) {
	srv := limitsTestServer(t)
	a, b := srv.sb.NewSession(), srv.sb.NewSession()
	a1 := create(t, srv, a, ua.StatusOK)
	a2 := create(t, srv, a, ua.StatusOK)
	b1 := create(t, srv, b, ua.StatusOK)

	deleted := srv.SubscriptionService.DeleteSessionSubscriptions(a.AuthTokenID)
	require.ElementsMatch(t, []uint32{a1, a2}, deleted)
	live := liveSubscriptions(srv)
	require.Len(t, live, 1)
	require.Contains(t, live, b1)

	require.Empty(t, srv.SubscriptionService.DeleteSessionSubscriptions(a.AuthTokenID))
	require.Empty(t, srv.SubscriptionService.DeleteSessionSubscriptions(nil))
}

// TestSubscriptionEndsOnce checks that a Subscription whose background task
// ends after the Subscription was deleted does not remove a later
// Subscription that was given the same id.
func TestSubscriptionEndsOnce(t *testing.T) {
	srv := limitsTestServer(t)
	svc := srv.SubscriptionService
	a := srv.sb.NewSession()
	first := create(t, srv, a, ua.StatusOK)

	svc.Mu.Lock()
	old := svc.Subs[first]
	svc.Mu.Unlock()
	deleteSubscriptions(t, srv, a, first)

	// Put another Subscription under the same id, then let the old
	// Subscription's task end it once more.
	replacement := NewSubscription()
	replacement.srv = svc
	replacement.ID = first
	svc.Mu.Lock()
	svc.Subs[first] = replacement
	svc.Mu.Unlock()

	svc.endSubscription(old)
	require.Same(t, replacement, liveSubscriptions(srv)[first])

	svc.Mu.Lock()
	delete(svc.Subs, first)
	svc.Mu.Unlock()
}

// TestServerCapabilitiesSubscriptionLimits checks that the limits are
// reported as ServerCapabilities.MaxSubscriptions and
// MaxSubscriptionsPerSession (Part 5 §6.3.2), UInt32, 0 when no limit is set,
// and that both are found by browsing ServerCapabilities.
func TestServerCapabilitiesSubscriptionLimits(t *testing.T) {
	tests := []struct {
		name                  string
		opts                  []Option
		perSession, wholeSize uint32
	}{
		{"set", []Option{MaxSubscriptionsPerSession(7), MaxSubscriptions(70)}, 7, 70},
		{"unset", nil, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := limitsTestServer(t, tt.opts...)
			ns := srv.namespaces[0].(*NodeNameSpace)

			for nodeID, want := range map[uint32]uint32{
				id.Server_ServerCapabilities_MaxSubscriptionsPerSession: tt.perSession,
				id.Server_ServerCapabilities_MaxSubscriptions:           tt.wholeSize,
			} {
				dv := ns.Attribute(ua.NewNumericNodeID(0, nodeID), ua.AttributeIDValue)
				require.Equal(t, ua.StatusOK, dv.Status, "i=%d", nodeID)
				require.Equal(t, want, dv.Value.Value(), "i=%d", nodeID)

				dt := ns.Attribute(ua.NewNumericNodeID(0, nodeID), ua.AttributeIDDataType)
				require.Equal(t, ua.StatusOK, dt.Status, "i=%d DataType", nodeID)
				require.Equal(t, uint32(id.UInt32), dt.Value.NodeID().IntID(), "i=%d DataType", nodeID)
			}

			view := &ViewService{srv: srv}
			resp, err := view.Browse(nil, &ua.BrowseRequest{
				RequestHeader: &ua.RequestHeader{},
				NodesToBrowse: []*ua.BrowseDescription{{
					NodeID:          ua.NewNumericNodeID(0, id.Server_ServerCapabilities),
					BrowseDirection: ua.BrowseDirectionForward,
					ReferenceTypeID: ua.NewNumericNodeID(0, id.HasProperty),
					IncludeSubtypes: true,
					ResultMask:      uint32(ua.BrowseResultMaskAll),
				}},
			}, 0)
			require.NoError(t, err)
			found := map[uint32]bool{}
			for _, ref := range resp.(*ua.BrowseResponse).Results[0].References {
				found[ref.NodeID.NodeID.IntID()] = true
			}
			require.True(t, found[id.Server_ServerCapabilities_MaxSubscriptionsPerSession], "MaxSubscriptionsPerSession not browsable")
			require.True(t, found[id.Server_ServerCapabilities_MaxSubscriptions], "MaxSubscriptions not browsable")
		})
	}
}
