package server

import (
	"testing"

	"github.com/gopcua/opcua/ua"
)

const createFaultRequestHandle = 11

// createFaultTest drives CreateMonitoredItems with two Sessions, each owning
// one Subscription. The Subscriptions are registered without starting their
// publish loop.
type createFaultTest struct {
	srv      *Server
	node     *ua.NodeID
	sess     *session
	other    *session
	sub      *Subscription // Subscription of sess
	otherSub *Subscription // Subscription of other
}

func newCreateFaultTest(t *testing.T) *createFaultTest {
	t.Helper()
	srv := New()
	srv.initHandlers()
	ns := NewNodeNameSpace(srv, "urn:test:create-monitored-items")
	node := ua.NewStringNodeID(ns.ID(), "value")
	ns.AddNode(NewVariableNode(node, "value", func() *ua.DataValue {
		return DataValueFromValue(int32(1))
	}))
	ct := &createFaultTest{
		srv:   srv,
		node:  node,
		sess:  srv.sb.NewSession(),
		other: srv.sb.NewSession(),
	}
	ct.sub = ct.addSubscription(1, ct.sess)
	ct.otherSub = ct.addSubscription(2, ct.other)
	return ct
}

func (ct *createFaultTest) addSubscription(id uint32, sess *session) *Subscription {
	sub := NewSubscription()
	sub.srv = ct.srv.SubscriptionService
	sub.Session = sess
	sub.ID = id
	sub.RevisedPublishingInterval = 100
	ct.srv.SubscriptionService.Mu.Lock()
	ct.srv.SubscriptionService.Subs[id] = sub
	ct.srv.SubscriptionService.Mu.Unlock()
	return sub
}

func (ct *createFaultTest) items(n int) []*ua.MonitoredItemCreateRequest {
	items := make([]*ua.MonitoredItemCreateRequest, n)
	for i := range items {
		items[i] = &ua.MonitoredItemCreateRequest{
			ItemToMonitor:       &ua.ReadValueID{NodeID: ct.node, AttributeID: ua.AttributeIDValue},
			MonitoringMode:      ua.MonitoringModeReporting,
			RequestedParameters: &ua.MonitoringParameters{ClientHandle: uint32(i + 1), QueueSize: 1},
		}
	}
	return items
}

func (ct *createFaultTest) itemCount() int {
	svc := ct.srv.MonitoredItemService
	svc.Mu.Lock()
	defer svc.Mu.Unlock()
	return len(svc.Items) + len(svc.Nodes) + len(svc.Subs)
}

// call invokes the handler registered for CreateMonitoredItems, so a panic
// fails the test instead of being hidden by a dispatch recover.
func (ct *createFaultTest) call(t *testing.T, req *ua.CreateMonitoredItemsRequest) ua.Response {
	t.Helper()
	h, ok := ct.srv.handlers[ua.ServiceTypeID(req)]
	if !ok {
		t.Fatal("no handler registered for CreateMonitoredItems")
	}
	resp, err := h(nil, req, 0)
	if err != nil {
		t.Fatalf("CreateMonitoredItems returned error %v, want a response", err)
	}
	return resp
}

// TestCreateMonitoredItemsServiceFaults checks the service results of
// CreateMonitoredItems when the request cannot be processed as a whole
// (Part 4 §5.13.2.3 Table 64, §7.38.2 Table 178): a ServiceFault carrying the
// requestHandle of the request (§7.34) and no MonitoredItem created. The
// checks apply in the order Session, Subscription, item list.
func TestCreateMonitoredItemsServiceFaults(t *testing.T) {
	unknownToken := ua.NewNumericNodeID(0, 4242)

	tests := []struct {
		name   string
		token  func(ct *createFaultTest) *ua.NodeID
		header bool
		subID  func(ct *createFaultTest) uint32
		items  int
		want   ua.StatusCode
	}{
		{
			name:   "unknown authenticationToken",
			token:  func(*createFaultTest) *ua.NodeID { return unknownToken },
			header: true,
			subID:  func(ct *createFaultTest) uint32 { return ct.sub.ID },
			items:  1,
			want:   ua.StatusBadSessionIDInvalid,
		},
		{
			name:   "missing authenticationToken",
			token:  func(*createFaultTest) *ua.NodeID { return nil },
			header: true,
			subID:  func(ct *createFaultTest) uint32 { return ct.sub.ID },
			items:  1,
			want:   ua.StatusBadSessionIDInvalid,
		},
		{
			name:  "missing request header",
			subID: func(ct *createFaultTest) uint32 { return ct.sub.ID },
			items: 1,
			want:  ua.StatusBadSessionIDInvalid,
		},
		{
			name:   "unknown authenticationToken before unknown subscriptionId",
			token:  func(*createFaultTest) *ua.NodeID { return unknownToken },
			header: true,
			subID:  func(*createFaultTest) uint32 { return 99 },
			items:  0,
			want:   ua.StatusBadSessionIDInvalid,
		},
		{
			name:   "unknown subscriptionId",
			token:  func(ct *createFaultTest) *ua.NodeID { return ct.sess.AuthTokenID },
			header: true,
			subID:  func(*createFaultTest) uint32 { return 99 },
			items:  1,
			want:   ua.StatusBadSubscriptionIDInvalid,
		},
		{
			name:   "Subscription of another Session",
			token:  func(ct *createFaultTest) *ua.NodeID { return ct.sess.AuthTokenID },
			header: true,
			subID:  func(ct *createFaultTest) uint32 { return ct.otherSub.ID },
			items:  1,
			want:   ua.StatusBadSubscriptionIDInvalid,
		},
		{
			name:   "unknown subscriptionId before empty itemsToCreate",
			token:  func(ct *createFaultTest) *ua.NodeID { return ct.sess.AuthTokenID },
			header: true,
			subID:  func(*createFaultTest) uint32 { return 99 },
			items:  0,
			want:   ua.StatusBadSubscriptionIDInvalid,
		},
		{
			name:   "empty itemsToCreate",
			token:  func(ct *createFaultTest) *ua.NodeID { return ct.sess.AuthTokenID },
			header: true,
			subID:  func(ct *createFaultTest) uint32 { return ct.sub.ID },
			items:  0,
			want:   ua.StatusBadNothingToDo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := newCreateFaultTest(t)
			req := &ua.CreateMonitoredItemsRequest{
				SubscriptionID:     tt.subID(ct),
				TimestampsToReturn: ua.TimestampsToReturnBoth,
				ItemsToCreate:      ct.items(tt.items),
			}
			wantHandle := uint32(0)
			if tt.header {
				req.RequestHeader = &ua.RequestHeader{
					AuthenticationToken: tt.token(ct),
					RequestHandle:       createFaultRequestHandle,
				}
				wantHandle = createFaultRequestHandle
			}

			resp := ct.call(t, req)
			fault, ok := resp.(*ua.ServiceFault)
			if !ok {
				t.Fatalf("response is %T, want *ua.ServiceFault", resp)
			}
			if got := fault.ResponseHeader.ServiceResult; got != tt.want {
				t.Errorf("ServiceResult = %v, want %v", got, tt.want)
			}
			if got := fault.ResponseHeader.RequestHandle; got != wantHandle {
				t.Errorf("RequestHandle = %d, want %d", got, wantHandle)
			}
			if got := ct.itemCount(); got != 0 {
				t.Errorf("%d MonitoredItem entries after the fault, want none", got)
			}
		})
	}
}

// TestCreateMonitoredItemsOwnSubscription checks that a request of the Session
// that owns the Subscription still creates one MonitoredItem per item and
// echoes the requestHandle.
func TestCreateMonitoredItemsOwnSubscription(t *testing.T) {
	ct := newCreateFaultTest(t)
	resp := ct.call(t, &ua.CreateMonitoredItemsRequest{
		RequestHeader: &ua.RequestHeader{
			AuthenticationToken: ct.sess.AuthTokenID,
			RequestHandle:       createFaultRequestHandle,
		},
		SubscriptionID:     ct.sub.ID,
		TimestampsToReturn: ua.TimestampsToReturnBoth,
		ItemsToCreate:      ct.items(2),
	})
	res, ok := resp.(*ua.CreateMonitoredItemsResponse)
	if !ok {
		t.Fatalf("response is %T, want *ua.CreateMonitoredItemsResponse", resp)
	}
	if got := res.ResponseHeader.ServiceResult; got != ua.StatusOK {
		t.Errorf("ServiceResult = %v, want Good", got)
	}
	if got := res.ResponseHeader.RequestHandle; got != createFaultRequestHandle {
		t.Errorf("RequestHandle = %d, want %d", got, createFaultRequestHandle)
	}
	if len(res.Results) != 2 {
		t.Fatalf("%d results, want 2", len(res.Results))
	}
	for i, r := range res.Results {
		if r.StatusCode != ua.StatusOK {
			t.Errorf("results[%d] = %v, want Good", i, r.StatusCode)
		}
	}
	svc := ct.srv.MonitoredItemService
	svc.Mu.Lock()
	n := len(svc.Items)
	svc.Mu.Unlock()
	if n != 2 {
		t.Errorf("%d MonitoredItems, want 2", n)
	}
}
