package server

import (
	"math"
	"testing"

	"github.com/gopcua/opcua/ua"
)

func (qt *queueTest) modify(subID, itemID, clientHandle, queueSize uint32, discardOldest bool) ua.Response {
	qt.t.Helper()
	return qt.modifyWith(subID, itemID, &ua.MonitoringParameters{
		ClientHandle:  clientHandle,
		QueueSize:     queueSize,
		DiscardOldest: discardOldest,
	})
}

func (qt *queueTest) modifyWith(subID, itemID uint32, params *ua.MonitoringParameters) ua.Response {
	qt.t.Helper()
	resp, err := qt.srv.MonitoredItemService.ModifyMonitoredItems(nil, &ua.ModifyMonitoredItemsRequest{
		RequestHeader:  qt.header(),
		SubscriptionID: subID,
		ItemsToModify: []*ua.MonitoredItemModifyRequest{{
			MonitoredItemID:     itemID,
			RequestedParameters: params,
		}},
	}, 0)
	if err != nil {
		qt.t.Fatalf("ModifyMonitoredItems: %v", err)
	}
	return resp
}

func TestNotificationQueueResize(t *testing.T) {
	tests := []struct {
		name          string
		size          int
		discardOldest bool
		want          []int32
		overflow      []bool
	}{
		{"grow", 20, true, []int32{1, 2, 3, 4, 5, 6}, []bool{false, false, false, false, false, false}},
		{"shrink, discard oldest", 3, true, []int32{4, 5, 6}, []bool{true, false, false}},
		{"shrink, discard newest", 3, false, []int32{1, 2, 6}, []bool{false, false, true}},
		{"shrink to one", 1, false, []int32{6}, []bool{false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &notificationQueue{size: 10, discardOldest: true}
			pushValues(q, 1, 2, 3, 4, 5, 6)
			q.resize(tt.size, tt.discardOldest)
			checkQueue(t, q.entries, tt.want, tt.overflow)
		})
	}

	t.Run("size one clears the Overflow bit", func(t *testing.T) {
		q := &notificationQueue{size: 3, discardOldest: false}
		pushValues(q, 1, 2, 3, 4, 5)
		checkQueue(t, q.entries, []int32{1, 2, 5}, []bool{false, false, true})
		q.resize(1, false)
		checkQueue(t, q.entries, []int32{5}, []bool{false})
	})
}

// Part 4 §7.38.1, Table 176: a queue that is trimmed hands the StructureChanged
// and SemanticsChanged bits of the discarded values to the value that stands
// in for them.
func TestNotificationQueueResizeCarriesChangedBits(t *testing.T) {
	flags := map[int32]ua.StatusCode{2: structureChanged, 3: semanticsChanged}
	for _, tt := range []struct {
		name          string
		size          int
		discardOldest bool
		want          []int32
		status        []ua.StatusCode
	}{
		{"discard oldest", 3, true, []int32{4, 5, 6},
			[]ua.StatusCode{structureChanged | semanticsChanged | statusGoodOverflow, 0, 0}},
		{"discard newest", 3, false, []int32{1, 2, 6},
			[]ua.StatusCode{0, structureChanged, semanticsChanged | statusGoodOverflow}},
		{"to one", 1, false, []int32{6},
			[]ua.StatusCode{structureChanged | semanticsChanged}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := &notificationQueue{size: 10, discardOldest: true}
			for _, v := range flaggedValues(6, flags) {
				q.push(v)
			}
			q.resize(tt.size, tt.discardOldest)
			checkStatuses(t, q, tt.want, tt.status)
		})
	}
}

// Part 4 §5.13.3.
func TestModifyMonitoredItems(t *testing.T) {
	qt := newQueueTest(t)
	nodeID := qt.node("value")
	item := qt.create(nodeID, 1, 10, true)
	qt.set(nodeID, 1, 2, 3, 4, 5, 6)

	resp := qt.modify(qt.sub.ID, item.MonitoredItemID, 2, 3, true).(*ua.ModifyMonitoredItemsResponse)
	if resp.ResponseHeader.ServiceResult != ua.StatusOK {
		t.Fatalf("service result %v", resp.ResponseHeader.ServiceResult)
	}
	if res := resp.Results[0]; res.StatusCode != ua.StatusOK || res.RevisedQueueSize != 3 {
		t.Fatalf("got %+v", res)
	}
	// The queue is trimmed to the newest values and the queued values carry
	// the new ClientHandle. The oldest kept value reports the discarded ones.
	got, _ := qt.sub.drainQueues(0)
	if len(got) != 3 || got[0].Value.Status != statusGoodOverflow || got[1].Value.Status != ua.StatusOK {
		t.Fatalf("got %d notifications: %+v", len(got), got)
	}
	for i, n := range got {
		if n.ClientHandle != 2 || n.Value.Value.Value() != int32(4+i) {
			t.Fatalf("notification %d: handle %d value %v", i, n.ClientHandle, n.Value.Value.Value())
		}
	}

	for _, tt := range []struct{ requested, want uint32 }{{5000, 5000}, {5001, 5000}, {0, 1}} {
		resp := qt.modify(qt.sub.ID, item.MonitoredItemID, 2, tt.requested, true).(*ua.ModifyMonitoredItemsResponse)
		if got := resp.Results[0].RevisedQueueSize; got != tt.want {
			t.Errorf("queueSize %d: got revisedQueueSize %d, want %d", tt.requested, got, tt.want)
		}
	}

	resp = qt.modify(qt.sub.ID, item.MonitoredItemID+100, 2, 3, true).(*ua.ModifyMonitoredItemsResponse)
	if resp.ResponseHeader.ServiceResult != ua.StatusOK || resp.Results[0].StatusCode != ua.StatusBadMonitoredItemIDInvalid {
		t.Fatalf("unknown item: service result %v, result %v", resp.ResponseHeader.ServiceResult, resp.Results[0].StatusCode)
	}

	fault, ok := qt.modify(qt.sub.ID+100, item.MonitoredItemID, 2, 3, true).(*ua.ServiceFault)
	if !ok || fault.ResponseHeader.ServiceResult != ua.StatusBadSubscriptionIDInvalid {
		t.Fatalf("unknown subscription: got %+v", fault)
	}
}

// ModifyMonitoredItems revises the sampling interval by the same rule as
// CreateMonitoredItems (see TestRevisedSamplingInterval), so it is never less
// than requested (Part 4 §5.13.3.2, Table 66), and the item records it.
func TestModifyMonitoredItemsRevisesSamplingIntervalLikeCreate(t *testing.T) {
	qt := newQueueTest(t) // publishing interval 100
	nodeID := qt.node("value")
	for _, tt := range []struct{ requested, want float64 }{
		{-1, 100}, {-5, 100}, {math.NaN(), 100}, {0, 0}, {50, 50}, {100, 100}, {120, 120}, {5000, 5000},
	} {
		created := qt.createWith(nodeID, &ua.MonitoringParameters{ClientHandle: 1, SamplingInterval: tt.requested})
		if created.RevisedSamplingInterval != tt.want {
			t.Errorf("create %v: got %v, want %v", tt.requested, created.RevisedSamplingInterval, tt.want)
		}

		other := qt.createWith(nodeID, &ua.MonitoringParameters{ClientHandle: 2, SamplingInterval: 700})
		resp := qt.modifyWith(qt.sub.ID, other.MonitoredItemID, &ua.MonitoringParameters{ClientHandle: 2, SamplingInterval: tt.requested}).(*ua.ModifyMonitoredItemsResponse)
		res := resp.Results[0]
		if res.StatusCode != ua.StatusOK || res.RevisedSamplingInterval != tt.want {
			t.Errorf("modify %v: got %v (status %v), want %v", tt.requested, res.RevisedSamplingInterval, res.StatusCode, tt.want)
		}
		qt.srv.MonitoredItemService.Mu.Lock()
		got := qt.srv.MonitoredItemService.Items[other.MonitoredItemID].RevisedSamplingInterval
		qt.srv.MonitoredItemService.Mu.Unlock()
		if got != res.RevisedSamplingInterval {
			t.Errorf("modify %v: item records %v, Modify returned %v", tt.requested, got, res.RevisedSamplingInterval)
		}
	}
}

// Missing MonitoringParameters in a modify request keep the item's
// parameters.
func TestModifyMonitoredItemsWithoutParameters(t *testing.T) {
	qt := newQueueTest(t)
	nodeID := qt.node("value")
	item := qt.createWith(nodeID, nil)
	qt.drain()

	resp := qt.modifyWith(qt.sub.ID, item.MonitoredItemID, nil).(*ua.ModifyMonitoredItemsResponse)
	if res := resp.Results[0]; res.StatusCode != ua.StatusOK || res.RevisedQueueSize != 1 {
		t.Fatalf("got %+v", res)
	}
	qt.set(nodeID, 1)
	qt.expect(qt.drain(), handleValue{0, 1})
}

// A ModifyMonitoredItems request for an item whose deletion is still being
// completed reports the item as unknown and does not bring its queue back, so
// no later publish response carries its values.
func TestModifyMonitoredItemsAfterDelete(t *testing.T) {
	qt := newQueueTest(t)
	nodeID := qt.node("value")
	item := qt.create(nodeID, 1, 10, true)
	qt.drain()

	// DeleteMonitoredItems finishes the deletion in the background; the
	// modify request may come before or after that.
	_, err := qt.srv.MonitoredItemService.DeleteMonitoredItems(nil, &ua.DeleteMonitoredItemsRequest{
		RequestHeader:    qt.header(),
		SubscriptionID:   qt.sub.ID,
		MonitoredItemIDs: []uint32{item.MonitoredItemID},
	}, 0)
	if err != nil {
		t.Fatalf("DeleteMonitoredItems: %v", err)
	}
	resp := qt.modify(qt.sub.ID, item.MonitoredItemID, 1, 10, true).(*ua.ModifyMonitoredItemsResponse)
	if got := resp.Results[0].StatusCode; got != ua.StatusBadMonitoredItemIDInvalid {
		t.Fatalf("modify after delete: got %v, want %v", got, ua.StatusBadMonitoredItemIDInvalid)
	}
	qt.set(nodeID, 5)
	qt.expect(qt.drain())
}

// Part 4 §5.13.2.3, Table 64.
func TestCreateMonitoredItemsTimestampsToReturnInvalid(t *testing.T) {
	qt := newQueueTest(t)
	resp, err := qt.srv.MonitoredItemService.CreateMonitoredItems(nil, &ua.CreateMonitoredItemsRequest{
		RequestHeader:      qt.header(),
		SubscriptionID:     qt.sub.ID,
		TimestampsToReturn: ua.TimestampsToReturnInvalid,
		ItemsToCreate: []*ua.MonitoredItemCreateRequest{{
			ItemToMonitor:       &ua.ReadValueID{NodeID: qt.node("value"), AttributeID: ua.AttributeIDValue},
			RequestedParameters: &ua.MonitoringParameters{ClientHandle: 1, QueueSize: 10},
		}},
	}, 0)
	if err != nil {
		t.Fatalf("CreateMonitoredItems: %v", err)
	}
	if fault, ok := resp.(*ua.ServiceFault); !ok || fault.ResponseHeader.ServiceResult != ua.StatusBadTimestampsToReturnInvalid {
		t.Fatalf("got %T %+v", resp, resp)
	}
}

// Part 4 §5.13.3.3, Table 67.
func TestModifyMonitoredItemsTimestampsToReturnInvalid(t *testing.T) {
	qt := newQueueTest(t)
	item := qt.create(qt.node("value"), 1, 10, true)
	resp, err := qt.srv.MonitoredItemService.ModifyMonitoredItems(nil, &ua.ModifyMonitoredItemsRequest{
		RequestHeader:      qt.header(),
		SubscriptionID:     qt.sub.ID,
		TimestampsToReturn: ua.TimestampsToReturnInvalid,
		ItemsToModify: []*ua.MonitoredItemModifyRequest{{
			MonitoredItemID:     item.MonitoredItemID,
			RequestedParameters: &ua.MonitoringParameters{ClientHandle: 1, QueueSize: 10},
		}},
	}, 0)
	if err != nil {
		t.Fatalf("ModifyMonitoredItems: %v", err)
	}
	if fault, ok := resp.(*ua.ServiceFault); !ok || fault.ResponseHeader.ServiceResult != ua.StatusBadTimestampsToReturnInvalid {
		t.Fatalf("got %T %+v", resp, resp)
	}
}

// ModifyMonitoredItems updates the server's copy of the create request, not
// the caller's.
func TestModifyMonitoredItemsKeepsCallerRequest(t *testing.T) {
	qt := newQueueTest(t)
	params := &ua.MonitoringParameters{ClientHandle: 1, QueueSize: 10}
	req := &ua.MonitoredItemCreateRequest{
		ItemToMonitor:       &ua.ReadValueID{NodeID: qt.node("value"), AttributeID: ua.AttributeIDValue},
		MonitoringMode:      ua.MonitoringModeReporting,
		RequestedParameters: params,
	}
	resp, err := qt.srv.MonitoredItemService.CreateMonitoredItems(nil, &ua.CreateMonitoredItemsRequest{
		RequestHeader:  qt.header(),
		SubscriptionID: qt.sub.ID,
		ItemsToCreate:  []*ua.MonitoredItemCreateRequest{req},
	}, 0)
	if err != nil {
		t.Fatalf("CreateMonitoredItems: %v", err)
	}
	qt.modify(qt.sub.ID, resp.(*ua.CreateMonitoredItemsResponse).Results[0].MonitoredItemID, 2, 3, false)
	if req.RequestedParameters != params || params.ClientHandle != 1 || params.QueueSize != 10 {
		t.Fatalf("modify changed the caller's create request: %+v", req.RequestedParameters)
	}
}
