package server

import (
	"sync"
	"testing"
	"time"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
	"github.com/stretchr/testify/require"
)

// TestCreateSubscriptionUniqueIDs checks that CreateSubscription hands out
// subscription ids that are unique for the entire server across sessions
// (Part 4 §5.14.2.2, Table 82), never 0 (Part 4 §7.19), and that the id of a
// subscription that was deleted or expired is not handed out again, so that
// the late teardown of a removed subscription leaves every live subscription
// and its MonitoredItems in place.
func TestCreateSubscriptionUniqueIDs(t *testing.T) {
	srv := New()
	srv.initHandlers()
	svc := srv.SubscriptionService
	items := srv.MonitoredItemService

	s1 := srv.sb.NewSession()
	s2 := srv.sb.NewSession()

	t.Cleanup(func() {
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

	var (
		handedOut = map[uint32]bool{}          // every id handed out so far
		live      = map[uint32]*session{}      // expected live ids and their owners
		created   = map[uint32]*Subscription{} // subscription created under each id
	)

	// createReq calls CreateSubscription and returns the new id. It does not
	// use t, so that it can run on other goroutines.
	createReq := func(sess *session, interval float64, keepAlive, lifetime uint32) (uint32, error) {
		resp, err := svc.CreateSubscription(nil, &ua.CreateSubscriptionRequest{
			RequestHeader:               &ua.RequestHeader{AuthenticationToken: sess.AuthTokenID},
			RequestedPublishingInterval: interval,
			RequestedLifetimeCount:      lifetime,
			RequestedMaxKeepAliveCount:  keepAlive,
			PublishingEnabled:           true,
		}, 0)
		if err != nil {
			return 0, err
		}
		return resp.(*ua.CreateSubscriptionResponse).SubscriptionID, nil
	}

	// record checks a newly handed out id and records it as live.
	record := func(sess *session, id uint32) {
		t.Helper()
		require.NotZero(t, id, "subscription id must not be 0")
		require.False(t, handedOut[id], "subscription id %d handed out twice", id)
		handedOut[id] = true
		live[id] = sess

		svc.Mu.Lock()
		defer svc.Mu.Unlock()
		require.NotNil(t, svc.Subs[id], "subscription %d not stored", id)
		created[id] = svc.Subs[id]
	}

	create := func(sess *session, interval float64, keepAlive, lifetime uint32) uint32 {
		t.Helper()
		id, err := createReq(sess, interval, keepAlive, lifetime)
		require.NoError(t, err)
		record(sess, id)
		return id
	}
	createLong := func(sess *session) uint32 {
		t.Helper()
		return create(sess, 1000, 1000, 3000)
	}

	// gone waits until the subscription with the given id has been removed.
	gone := func(id uint32) {
		t.Helper()
		require.Eventually(t, func() bool {
			svc.Mu.Lock()
			defer svc.Mu.Unlock()
			_, ok := svc.Subs[id]
			return !ok
		}, 10*time.Second, time.Millisecond, "subscription %d not removed", id)
		delete(live, id)
	}

	remove := func(sess *session, id uint32) {
		t.Helper()
		resp, err := svc.DeleteSubscriptions(nil, &ua.DeleteSubscriptionsRequest{
			RequestHeader:   &ua.RequestHeader{AuthenticationToken: sess.AuthTokenID},
			SubscriptionIDs: []uint32{id},
		}, 0)
		require.NoError(t, err)
		r, ok := resp.(*ua.DeleteSubscriptionsResponse)
		require.True(t, ok, "expected *ua.DeleteSubscriptionsResponse, got %T", resp)
		require.Equal(t, []ua.StatusCode{ua.StatusOK}, r.Results)
		// DeleteSubscriptions removes the subscription in the background.
		gone(id)
	}

	// checkLive checks that exactly the expected subscriptions are live, each
	// stored under its own id, owned by the session that created it, the
	// subscription created under that id, and still running.
	checkLive := func() {
		t.Helper()
		svc.Mu.Lock()
		defer svc.Mu.Unlock()
		got := map[uint32]*session{}
		for k, sub := range svc.Subs {
			require.NotZero(t, k, "subscription stored under id 0")
			require.Equal(t, k, sub.ID, "subscription stored under id %d has id %d", k, sub.ID)
			require.Same(t, created[k], sub, "subscription %d was replaced", k)
			got[k] = sub.Session

			sub.Mu.Lock()
			running := sub.running
			sub.Mu.Unlock()
			require.True(t, running, "subscription %d not running", k)
			select {
			case <-sub.shutdown:
				require.FailNow(t, "subscription shut down", "subscription %d", k)
			default:
			}
		}
		require.Equal(t, len(live), len(got), "number of live subscriptions")
		for k, sess := range live {
			require.Contains(t, got, k, "subscription %d missing", k)
			require.Same(t, sess, got[k], "subscription %d not owned by the creating session", k)
		}
	}

	createItem := func(sess *session, subID uint32) uint32 {
		t.Helper()
		resp, err := items.CreateMonitoredItems(nil, &ua.CreateMonitoredItemsRequest{
			RequestHeader:      &ua.RequestHeader{AuthenticationToken: sess.AuthTokenID},
			SubscriptionID:     subID,
			TimestampsToReturn: ua.TimestampsToReturnBoth,
			ItemsToCreate: []*ua.MonitoredItemCreateRequest{{
				ItemToMonitor: &ua.ReadValueID{
					NodeID:      ua.NewNumericNodeID(0, id.Server_ServerStatus_CurrentTime),
					AttributeID: ua.AttributeIDValue,
				},
				MonitoringMode: ua.MonitoringModeReporting,
				RequestedParameters: &ua.MonitoringParameters{
					ClientHandle:     subID,
					SamplingInterval: 1000,
					QueueSize:        1,
					DiscardOldest:    true,
				},
			}},
		}, 0)
		require.NoError(t, err)
		r, ok := resp.(*ua.CreateMonitoredItemsResponse)
		require.True(t, ok, "expected *ua.CreateMonitoredItemsResponse, got %T", resp)
		require.Len(t, r.Results, 1)
		require.Equal(t, ua.StatusOK, r.Results[0].StatusCode)
		return r.Results[0].MonitoredItemID
	}

	// checkItem checks that the MonitoredItem is still attached to the live
	// subscription it was created on.
	checkItem := func(subID, itemID uint32) {
		t.Helper()
		svc.Mu.Lock()
		sub := svc.Subs[subID]
		svc.Mu.Unlock()
		require.NotNil(t, sub, "subscription %d missing", subID)

		items.Mu.Lock()
		defer items.Mu.Unlock()
		item := items.Items[itemID]
		require.NotNil(t, item, "monitored item %d missing", itemID)
		require.Same(t, sub, item.Sub, "monitored item %d moved to another subscription", itemID)
		require.Contains(t, items.Subs[subID], item, "monitored item %d not listed for subscription %d", itemID, subID)
	}
	// checkNoItems checks that a removed subscription left no MonitoredItems.
	checkNoItems := func(subID uint32, itemIDs ...uint32) {
		t.Helper()
		items.Mu.Lock()
		defer items.Mu.Unlock()
		require.NotContains(t, items.Subs, subID, "removed subscription %d still has monitored items", subID)
		for _, itemID := range itemIDs {
			require.NotContains(t, items.Items, itemID, "monitored item %d of removed subscription %d still exists", itemID, subID)
		}
	}

	a := createLong(s1)
	checkLive()
	b := createLong(s2)
	checkLive()
	c := createLong(s1)
	checkLive()

	// MonitoredItems are created one after the other, never while a
	// subscription is being torn down.
	itemA := createItem(s1, a)
	itemB := createItem(s2, b)
	checkItem(a, itemA)
	checkItem(b, itemB)

	// A subscription that receives no Publish requests expires after its
	// lifetime and removes itself.
	e := create(s2, 20, 1, 3)
	checkLive()
	gone(e)
	checkLive()
	checkNoItems(e)

	// Deleting a subscription must not let the next one take the id of a
	// subscription that is still live, whichever session creates it.
	remove(s1, a)
	checkLive()
	checkNoItems(a, itemA)
	d := createLong(s2)
	checkLive()

	// The removed subscriptions tear down again when their run loops end;
	// replay that here. It must not touch the live subscriptions.
	svc.DeleteSubscription(a)
	svc.DeleteSubscription(e)
	checkLive()
	for _, k := range []uint32{b, c, d} {
		require.Contains(t, live, k)
	}
	checkItem(b, itemB)
	checkNoItems(a, itemA)
	checkNoItems(e)

	// Concurrent CreateSubscription calls from both sessions get distinct ids.
	const n = 16
	sessions := []*session{s1, s2}
	ids := make([]uint32, 2*n)
	errs := make([]error, 2*n)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = createReq(sessions[i%2], 1000, 1000, 3000)
		}(i)
	}
	wg.Wait()
	for i := range ids {
		require.NoError(t, errs[i])
		record(sessions[i%2], ids[i])
	}
	checkLive()
	checkItem(b, itemB)

	// Once every subscription is gone, no earlier id comes back.
	for k, sess := range live {
		remove(sess, k)
		checkLive()
	}
	require.Empty(t, live)
	createLong(s1)
	checkLive()
	createLong(s2)
	checkLive()
}
