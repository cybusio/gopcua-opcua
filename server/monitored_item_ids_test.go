package server

import (
	"testing"

	"github.com/gopcua/opcua/ua"
)

const itemTestRequestHandle = 7

// itemTest drives SetMonitoringMode and DeleteMonitoredItems with two
// Sessions. The first Session owns two Subscriptions, the second one owns
// one. The Subscriptions are registered without starting their publish loop.
type itemTest struct {
	t     *testing.T
	srv   *Server
	node  *ua.NodeID
	sess  *session
	other *session

	sub      *Subscription // first Subscription of sess
	sibling  *Subscription // second Subscription of sess
	otherSub *Subscription // Subscription of other
}

func newItemTest(t *testing.T) *itemTest {
	t.Helper()
	srv := New()
	srv.initHandlers()
	ns := NewNodeNameSpace(srv, "urn:test:monitoreditem-ids")
	node := ua.NewStringNodeID(ns.ID(), "value")
	ns.AddNode(NewVariableNode(node, "value", func() *ua.DataValue {
		return DataValueFromValue(int32(1))
	}))
	it := &itemTest{
		t:     t,
		srv:   srv,
		node:  node,
		sess:  srv.sb.NewSession(),
		other: srv.sb.NewSession(),
	}
	it.sub = it.addSubscription(1, it.sess)
	it.sibling = it.addSubscription(2, it.sess)
	it.otherSub = it.addSubscription(3, it.other)
	return it
}

func (it *itemTest) addSubscription(id uint32, sess *session) *Subscription {
	sub := NewSubscription()
	sub.srv = it.srv.SubscriptionService
	sub.Session = sess
	sub.ID = id
	sub.RevisedPublishingInterval = 100
	it.srv.SubscriptionService.Mu.Lock()
	it.srv.SubscriptionService.Subs[id] = sub
	it.srv.SubscriptionService.Mu.Unlock()
	return sub
}

func itemTestHeader(sess *session) *ua.RequestHeader {
	return &ua.RequestHeader{AuthenticationToken: sess.AuthTokenID, RequestHandle: itemTestRequestHandle}
}

// create adds a MonitoredItem in reporting mode to sub on behalf of sess.
func (it *itemTest) create(sess *session, sub *Subscription) uint32 {
	it.t.Helper()
	resp, err := it.srv.MonitoredItemService.CreateMonitoredItems(nil, &ua.CreateMonitoredItemsRequest{
		RequestHeader:  itemTestHeader(sess),
		SubscriptionID: sub.ID,
		ItemsToCreate: []*ua.MonitoredItemCreateRequest{{
			ItemToMonitor:       &ua.ReadValueID{NodeID: it.node, AttributeID: ua.AttributeIDValue},
			MonitoringMode:      ua.MonitoringModeReporting,
			RequestedParameters: &ua.MonitoringParameters{ClientHandle: 1, QueueSize: 1},
		}},
	}, 0)
	if err != nil {
		it.t.Fatalf("CreateMonitoredItems: %v", err)
	}
	res := resp.(*ua.CreateMonitoredItemsResponse).Results[0]
	if res.StatusCode != ua.StatusOK {
		it.t.Fatalf("CreateMonitoredItems: status %v", res.StatusCode)
	}
	it.setMode(res.MonitoredItemID, ua.MonitoringModeReporting)
	return res.MonitoredItemID
}

// setMode sets the mode of an item directly so that the tests do not depend
// on how CreateMonitoredItems records the requested mode.
func (it *itemTest) setMode(id uint32, mode ua.MonitoringMode) {
	svc := it.srv.MonitoredItemService
	svc.Mu.Lock()
	defer svc.Mu.Unlock()
	svc.Items[id].Mode = mode
}

func (it *itemTest) exists(id uint32) bool {
	svc := it.srv.MonitoredItemService
	svc.Mu.Lock()
	defer svc.Mu.Unlock()
	_, ok := svc.Items[id]
	return ok
}

func (it *itemTest) mode(id uint32) ua.MonitoringMode {
	svc := it.srv.MonitoredItemService
	svc.Mu.Lock()
	defer svc.Mu.Unlock()
	return svc.Items[id].Mode
}

func (it *itemTest) delete(sess *session, sub *Subscription, ids ...uint32) ua.Response {
	it.t.Helper()
	resp, err := it.srv.MonitoredItemService.DeleteMonitoredItems(nil, &ua.DeleteMonitoredItemsRequest{
		RequestHeader:    itemTestHeader(sess),
		SubscriptionID:   sub.ID,
		MonitoredItemIDs: ids,
	}, 0)
	if err != nil {
		it.t.Fatalf("DeleteMonitoredItems: %v", err)
	}
	return resp
}

func (it *itemTest) setMonitoringMode(sess *session, sub *Subscription, mode ua.MonitoringMode, ids ...uint32) ua.Response {
	it.t.Helper()
	resp, err := it.srv.MonitoredItemService.SetMonitoringMode(nil, &ua.SetMonitoringModeRequest{
		RequestHeader:    itemTestHeader(sess),
		SubscriptionID:   sub.ID,
		MonitoringMode:   mode,
		MonitoredItemIDs: ids,
	}, 0)
	if err != nil {
		it.t.Fatalf("SetMonitoringMode: %v", err)
	}
	return resp
}

func checkItemResults(t *testing.T, hdr *ua.ResponseHeader, got, want []ua.StatusCode) {
	t.Helper()
	if hdr.ServiceResult != ua.StatusOK {
		t.Fatalf("service result: got %v, want Good", hdr.ServiceResult)
	}
	if hdr.RequestHandle != itemTestRequestHandle {
		t.Fatalf("request handle: got %d, want %d", hdr.RequestHandle, itemTestRequestHandle)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

// itemSet is the set of MonitoredItems the per-item tests address.
type itemSet struct {
	own, sibling, foreign, deleted, unknown uint32
}

func (it *itemTest) items() itemSet {
	it.t.Helper()
	s := itemSet{
		own:     it.create(it.sess, it.sub),
		sibling: it.create(it.sess, it.sibling),
		foreign: it.create(it.other, it.otherSub),
		deleted: it.create(it.sess, it.sub),
	}
	it.delete(it.sess, it.sub, s.deleted)
	s.unknown = s.deleted + 1000
	return s
}

// Part 4 §5.13.6.4 Table 77: an id that is not a MonitoredItem of the
// Subscription is Bad_MonitoredItemIdInvalid, and only that entry fails.
func TestDeleteMonitoredItemsResults(t *testing.T) {
	bad := ua.StatusBadMonitoredItemIDInvalid
	tests := []struct {
		name string
		ids  func(itemSet) []uint32
		want []ua.StatusCode
		gone func(itemSet) []uint32
		kept func(itemSet) []uint32
	}{
		{
			name: "unknown id",
			ids:  func(s itemSet) []uint32 { return []uint32{s.unknown} },
			want: []ua.StatusCode{bad},
			kept: func(s itemSet) []uint32 { return []uint32{s.own, s.sibling, s.foreign} },
		},
		{
			name: "item of another subscription of the session",
			ids:  func(s itemSet) []uint32 { return []uint32{s.sibling} },
			want: []ua.StatusCode{bad},
			kept: func(s itemSet) []uint32 { return []uint32{s.own, s.sibling, s.foreign} },
		},
		{
			name: "item of another session",
			ids:  func(s itemSet) []uint32 { return []uint32{s.foreign} },
			want: []ua.StatusCode{bad},
			kept: func(s itemSet) []uint32 { return []uint32{s.own, s.sibling, s.foreign} },
		},
		{
			name: "already deleted",
			ids:  func(s itemSet) []uint32 { return []uint32{s.deleted} },
			want: []ua.StatusCode{bad},
			kept: func(s itemSet) []uint32 { return []uint32{s.own, s.sibling, s.foreign} },
		},
		{
			name: "mixed",
			ids: func(s itemSet) []uint32 {
				return []uint32{s.unknown, s.own, s.foreign, s.deleted, s.sibling}
			},
			want: []ua.StatusCode{bad, ua.StatusOK, bad, bad, bad},
			gone: func(s itemSet) []uint32 { return []uint32{s.own} },
			kept: func(s itemSet) []uint32 { return []uint32{s.sibling, s.foreign} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := newItemTest(t)
			s := it.items()
			resp, ok := it.delete(it.sess, it.sub, tt.ids(s)...).(*ua.DeleteMonitoredItemsResponse)
			if !ok {
				t.Fatalf("got %T, want *ua.DeleteMonitoredItemsResponse", resp)
			}
			checkItemResults(t, resp.ResponseHeader, resp.Results, tt.want)
			if tt.gone != nil {
				for _, id := range tt.gone(s) {
					if it.exists(id) {
						t.Errorf("item %d still exists after a Good result", id)
					}
				}
			}
			for _, id := range tt.kept(s) {
				if !it.exists(id) {
					t.Errorf("item %d was deleted", id)
				}
			}
		})
	}
}

// A deleted item is gone when the response is sent: the id is no longer
// listed for its node or its Subscription, and deleting it again fails.
func TestDeleteMonitoredItemsDeletesBeforeResponding(t *testing.T) {
	it := newItemTest(t)
	id := it.create(it.sess, it.sub)

	resp := it.delete(it.sess, it.sub, id).(*ua.DeleteMonitoredItemsResponse)
	checkItemResults(t, resp.ResponseHeader, resp.Results, []ua.StatusCode{ua.StatusOK})

	svc := it.srv.MonitoredItemService
	svc.Mu.Lock()
	for _, item := range svc.Nodes[it.node.String()] {
		if item.ID == id {
			t.Errorf("item %d still listed for its node", id)
		}
	}
	for _, item := range svc.Subs[it.sub.ID] {
		if item.ID == id {
			t.Errorf("item %d still listed for its subscription", id)
		}
	}
	svc.Mu.Unlock()

	resp = it.delete(it.sess, it.sub, id).(*ua.DeleteMonitoredItemsResponse)
	checkItemResults(t, resp.ResponseHeader, resp.Results, []ua.StatusCode{ua.StatusBadMonitoredItemIDInvalid})
}

// Part 4 §5.13.4.4 Table 71: the same per-item rule for SetMonitoringMode.
// Only the items with a Good result change their mode.
func TestSetMonitoringModeResults(t *testing.T) {
	bad := ua.StatusBadMonitoredItemIDInvalid
	tests := []struct {
		name     string
		ids      func(itemSet) []uint32
		want     []ua.StatusCode
		disabled func(itemSet) []uint32
	}{
		{
			name: "unknown id",
			ids:  func(s itemSet) []uint32 { return []uint32{s.unknown} },
			want: []ua.StatusCode{bad},
		},
		{
			name: "item of another subscription of the session",
			ids:  func(s itemSet) []uint32 { return []uint32{s.sibling} },
			want: []ua.StatusCode{bad},
		},
		{
			name: "item of another session",
			ids:  func(s itemSet) []uint32 { return []uint32{s.foreign} },
			want: []ua.StatusCode{bad},
		},
		{
			name: "deleted item",
			ids:  func(s itemSet) []uint32 { return []uint32{s.deleted} },
			want: []ua.StatusCode{bad},
		},
		{
			name: "mixed",
			ids: func(s itemSet) []uint32 {
				return []uint32{s.unknown, s.own, s.foreign, s.deleted, s.sibling}
			},
			want:     []ua.StatusCode{bad, ua.StatusOK, bad, bad, bad},
			disabled: func(s itemSet) []uint32 { return []uint32{s.own} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := newItemTest(t)
			s := it.items()
			resp, ok := it.setMonitoringMode(it.sess, it.sub, ua.MonitoringModeDisabled, tt.ids(s)...).(*ua.SetMonitoringModeResponse)
			if !ok {
				t.Fatalf("got %T, want *ua.SetMonitoringModeResponse", resp)
			}
			checkItemResults(t, resp.ResponseHeader, resp.Results, tt.want)

			disabled := map[uint32]bool{}
			if tt.disabled != nil {
				for _, id := range tt.disabled(s) {
					disabled[id] = true
				}
			}
			for _, id := range []uint32{s.own, s.sibling, s.foreign} {
				want := ua.MonitoringModeReporting
				if disabled[id] {
					want = ua.MonitoringModeDisabled
				}
				if got := it.mode(id); got != want {
					t.Errorf("item %d: mode %v, want %v", id, got, want)
				}
			}
		})
	}
}

// Part 4 §5.13.4.3 Table 70, §5.13.6.3 Table 76, §7.34: a request that cannot
// be processed as a whole is answered with a ServiceFault that carries the
// client's requestHandle, and no item changes.
func TestMonitoredItemIDServiceFaults(t *testing.T) {
	type call func(it *itemTest, hdr *ua.RequestHeader, subID uint32, ids []uint32) (ua.Response, error)
	deleteCall := func(it *itemTest, hdr *ua.RequestHeader, subID uint32, ids []uint32) (ua.Response, error) {
		return it.srv.MonitoredItemService.DeleteMonitoredItems(nil, &ua.DeleteMonitoredItemsRequest{
			RequestHeader:    hdr,
			SubscriptionID:   subID,
			MonitoredItemIDs: ids,
		}, 0)
	}
	setModeCall := func(mode ua.MonitoringMode) call {
		return func(it *itemTest, hdr *ua.RequestHeader, subID uint32, ids []uint32) (ua.Response, error) {
			return it.srv.MonitoredItemService.SetMonitoringMode(nil, &ua.SetMonitoringModeRequest{
				RequestHeader:    hdr,
				SubscriptionID:   subID,
				MonitoringMode:   mode,
				MonitoredItemIDs: ids,
			}, 0)
		}
	}
	services := []struct {
		name string
		call call
	}{
		{"DeleteMonitoredItems", deleteCall},
		{"SetMonitoringMode", setModeCall(ua.MonitoringModeDisabled)},
	}

	type request struct {
		hdr   *ua.RequestHeader
		subID uint32
		ids   []uint32
	}
	cases := []struct {
		name string
		req  func(it *itemTest, own uint32) request
		want ua.StatusCode
	}{
		{
			name: "no authentication token",
			req: func(it *itemTest, own uint32) request {
				return request{&ua.RequestHeader{RequestHandle: itemTestRequestHandle}, it.sub.ID, []uint32{own}}
			},
			want: ua.StatusBadSessionIDInvalid,
		},
		{
			name: "unknown authentication token",
			req: func(it *itemTest, own uint32) request {
				hdr := &ua.RequestHeader{
					AuthenticationToken: ua.NewStringNodeID(0, "no-such-session"),
					RequestHandle:       itemTestRequestHandle,
				}
				return request{hdr, it.sub.ID, []uint32{own}}
			},
			want: ua.StatusBadSessionIDInvalid,
		},
		{
			name: "empty id list",
			req: func(it *itemTest, own uint32) request {
				return request{itemTestHeader(it.sess), it.sub.ID, nil}
			},
			want: ua.StatusBadNothingToDo,
		},
		{
			name: "unknown subscription",
			req: func(it *itemTest, own uint32) request {
				return request{itemTestHeader(it.sess), 99, []uint32{own}}
			},
			want: ua.StatusBadSubscriptionIDInvalid,
		},
		{
			name: "subscription of another session",
			req: func(it *itemTest, own uint32) request {
				return request{itemTestHeader(it.other), it.sub.ID, []uint32{own}}
			},
			want: ua.StatusBadSubscriptionIDInvalid,
		},
	}

	for _, svc := range services {
		for _, tc := range cases {
			t.Run(svc.name+"/"+tc.name, func(t *testing.T) {
				it := newItemTest(t)
				own := it.create(it.sess, it.sub)
				r := tc.req(it, own)
				resp, err := svc.call(it, r.hdr, r.subID, r.ids)
				checkServiceFault(t, resp, err, tc.want)
				if !it.exists(own) || it.mode(own) != ua.MonitoringModeReporting {
					t.Fatalf("item %d changed", own)
				}
			})
		}
	}

	t.Run("SetMonitoringMode/invalid monitoring mode", func(t *testing.T) {
		it := newItemTest(t)
		own := it.create(it.sess, it.sub)
		resp, err := setModeCall(ua.MonitoringMode(3))(it, itemTestHeader(it.sess), it.sub.ID, []uint32{own})
		checkServiceFault(t, resp, err, ua.StatusBadMonitoringModeInvalid)
		if it.mode(own) != ua.MonitoringModeReporting {
			t.Fatalf("item %d changed its mode", own)
		}
	})
}

func checkServiceFault(t *testing.T, resp ua.Response, err error, want ua.StatusCode) {
	t.Helper()
	if err != nil {
		t.Fatalf("got error %v, want a ServiceFault", err)
	}
	fault, ok := resp.(*ua.ServiceFault)
	if !ok {
		t.Fatalf("got %T, want *ua.ServiceFault", resp)
	}
	if fault.ResponseHeader.ServiceResult != want {
		t.Fatalf("service result: got %v, want %v", fault.ResponseHeader.ServiceResult, want)
	}
	if fault.ResponseHeader.RequestHandle != itemTestRequestHandle {
		t.Fatalf("request handle: got %d, want %d", fault.ResponseHeader.RequestHandle, itemTestRequestHandle)
	}
}
