package server

import (
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"
)

// The tests in this file cover the MonitoringMode of data MonitoredItems:
// Part 4 §5.13.1.3 (Monitoring mode), §5.13.4.1 (SetMonitoringMode) and §7.23
// (MonitoringMode).
//
//	Mode       sampled  queued  reported
//	Disabled   no       no      no
//	Sampling   yes      yes     no
//	Reporting  yes      yes     yes

// createInMode creates a data MonitoredItem in the given mode. A sampling
// interval of 0 samples every change.
func (qt *queueTest) createInMode(nodeID *ua.NodeID, clientHandle, queueSize uint32, mode ua.MonitoringMode) uint32 {
	qt.t.Helper()
	return qt.createInModeWith(nodeID, mode, &ua.MonitoringParameters{
		ClientHandle:  clientHandle,
		QueueSize:     queueSize,
		DiscardOldest: true,
	})
}

func (qt *queueTest) createInModeWith(nodeID *ua.NodeID, mode ua.MonitoringMode, params *ua.MonitoringParameters) uint32 {
	qt.t.Helper()
	resp, err := qt.srv.MonitoredItemService.CreateMonitoredItems(nil, &ua.CreateMonitoredItemsRequest{
		RequestHeader:  qt.header(),
		SubscriptionID: qt.sub.ID,
		ItemsToCreate: []*ua.MonitoredItemCreateRequest{{
			ItemToMonitor:       &ua.ReadValueID{NodeID: nodeID, AttributeID: ua.AttributeIDValue},
			MonitoringMode:      mode,
			RequestedParameters: params,
		}},
	}, 0)
	if err != nil {
		qt.t.Fatalf("CreateMonitoredItems: %v", err)
	}
	res := resp.(*ua.CreateMonitoredItemsResponse).Results[0]
	if res.StatusCode != ua.StatusOK {
		qt.t.Fatalf("CreateMonitoredItems: status %v", res.StatusCode)
	}
	return res.MonitoredItemID
}

// setMode sends SetMonitoringMode for the items and requires Good for each.
func (qt *queueTest) setMode(mode ua.MonitoringMode, ids ...uint32) {
	qt.t.Helper()
	resp, err := qt.srv.MonitoredItemService.SetMonitoringMode(nil, &ua.SetMonitoringModeRequest{
		RequestHeader:    qt.header(),
		SubscriptionID:   qt.sub.ID,
		MonitoringMode:   mode,
		MonitoredItemIDs: ids,
	}, 0)
	if err != nil {
		qt.t.Fatalf("SetMonitoringMode: %v", err)
	}
	r, ok := resp.(*ua.SetMonitoringModeResponse)
	if !ok {
		qt.t.Fatalf("SetMonitoringMode: got %T", resp)
	}
	if len(r.Results) != len(ids) {
		qt.t.Fatalf("SetMonitoringMode: got %d results, want %d", len(r.Results), len(ids))
	}
	for i, st := range r.Results {
		if st != ua.StatusOK {
			qt.t.Fatalf("SetMonitoringMode: result %d is %v", i, st)
		}
	}
}

// queued returns the values held in the queue of the item, whether or not
// the item reports them.
func (qt *queueTest) queued(id uint32) []int32 {
	qt.t.Helper()
	qt.sub.queueMu.Lock()
	defer qt.sub.queueMu.Unlock()
	q, ok := qt.sub.queues[id]
	if !ok {
		qt.t.Fatalf("item %d has no queue", id)
	}
	var vals []int32
	for _, e := range q.entries {
		v, _ := e.value.Value.Value().(int32)
		vals = append(vals, v)
	}
	return vals
}

func (qt *queueTest) expectQueued(id uint32, want ...int32) {
	qt.t.Helper()
	got := qt.queued(id)
	if len(got) != len(want) {
		qt.t.Fatalf("item %d queues %v, want %v", id, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			qt.t.Fatalf("item %d queues %v, want %v", id, got, want)
		}
	}
}

func TestNotificationQueueSetMode(t *testing.T) {
	tests := []struct {
		name     string
		mode     ua.MonitoringMode
		held     bool
		remained []int32
	}{
		// §5.13.4.1: Disabled deletes all queued values.
		{"disabled", ua.MonitoringModeDisabled, true, nil},
		// §5.13.1.3: Sampling keeps the values queued without reporting.
		{"sampling", ua.MonitoringModeSampling, true, []int32{1, 2, 3}},
		{"reporting", ua.MonitoringModeReporting, false, []int32{1, 2, 3}},
		// A value outside the enumeration (§7.23) reports.
		{"outside the enumeration", ua.MonitoringMode(3), false, []int32{1, 2, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &notificationQueue{size: 5, discardOldest: true}
			pushValues(q, 1, 2, 3)
			q.setMode(tt.mode)
			if q.held != tt.held {
				t.Fatalf("held %v, want %v", q.held, tt.held)
			}
			checkQueue(t, q.entries, tt.remained, make([]bool, len(tt.remained)))
		})
	}
}

// Table row Disabled, transition Reporting -> Disabled: a disabled item is
// not sampled and reports nothing; another reporting item of the same
// Subscription is not affected.
func TestMonitoringModeDisabledStopsSampling(t *testing.T) {
	qt := newQueueTest(t)
	a, b := qt.node("a"), qt.node("b")
	itemA := qt.createInMode(a, 1, 10, ua.MonitoringModeReporting)
	qt.createInMode(b, 2, 10, ua.MonitoringModeReporting)
	qt.expect(qt.drain(), handleValue{1, 0}, handleValue{2, 0})

	qt.setMode(ua.MonitoringModeDisabled, itemA)
	for i := int32(1); i <= 3; i++ {
		qt.set(a, 100+i)
		qt.set(b, 200+i)
		qt.expect(qt.drain(), handleValue{2, 200 + i})
	}
	qt.expectQueued(itemA)
	if qt.sub.hasQueued() {
		t.Fatal("hasQueued reports values for a disabled item")
	}
}

// §5.13.4.1: setting the mode to Disabled deletes the queued values, from a
// reporting and from a sampling item. On a later enable, only the
// then-current value is reported (§5.13.1.3).
func TestMonitoringModeDisabledDeletesQueuedValues(t *testing.T) {
	for _, from := range []ua.MonitoringMode{ua.MonitoringModeReporting, ua.MonitoringModeSampling} {
		t.Run(from.String(), func(t *testing.T) {
			qt := newQueueTest(t)
			nodeID := qt.node("value")
			item := qt.createInMode(nodeID, 1, 5, from)
			qt.set(nodeID, 1, 2, 3)
			qt.expectQueued(item, 0, 1, 2, 3)

			qt.setMode(ua.MonitoringModeDisabled, item)
			qt.expectQueued(item)

			qt.setMode(ua.MonitoringModeReporting, item)
			qt.expect(qt.drain(), handleValue{1, 3})
			qt.expect(qt.drain())
		})
	}
}

// Table row Sampling, transition Sampling -> Reporting: a sampling item
// queues under its queue size and discard policy (§5.13.1.5) without
// reporting; once reporting, the next drain carries what the queue holds,
// oldest first, with the Overflow bit on the value next to the discarded
// ones.
func TestMonitoringModeSamplingQueuesWithoutReporting(t *testing.T) {
	qt := newQueueTest(t)
	a, b := qt.node("a"), qt.node("b")
	itemA := qt.createInMode(a, 1, 3, ua.MonitoringModeReporting)
	qt.createInMode(b, 2, 10, ua.MonitoringModeReporting)
	qt.drain()

	qt.setMode(ua.MonitoringModeSampling, itemA)
	for i := int32(1); i <= 5; i++ {
		qt.set(a, 100+i)
		qt.set(b, 200+i)
		qt.expect(qt.drain(), handleValue{2, 200 + i})
	}
	qt.expectQueued(itemA, 103, 104, 105)
	if qt.sub.hasQueued() {
		t.Fatal("hasQueued reports values held by a sampling item")
	}

	qt.setMode(ua.MonitoringModeReporting, itemA)
	items, more := qt.sub.drainQueues(0)
	if more {
		t.Fatal("more notifications reported without a limit")
	}
	entries := make([]queuedValue, len(items))
	for i, n := range items {
		if n.ClientHandle != 1 {
			t.Fatalf("notification %d: client handle %d, want 1", i, n.ClientHandle)
		}
		entries[i] = queuedValue{value: n.Value}
	}
	checkQueue(t, entries, []int32{103, 104, 105}, []bool{true, false, false})

	// Later changes are reported as they come.
	qt.set(a, 106)
	qt.expect(qt.drain(), handleValue{1, 106})
}

// Transition Reporting -> Sampling -> Reporting: reporting stops, sampling
// and queueing go on, and the queued values are reported after the switch
// back.
func TestMonitoringModeReportingToSampling(t *testing.T) {
	qt := newQueueTest(t)
	nodeID := qt.node("value")
	item := qt.createInMode(nodeID, 1, 10, ua.MonitoringModeReporting)
	qt.set(nodeID, 1)
	qt.setMode(ua.MonitoringModeSampling, item)
	qt.expect(qt.drain())
	qt.expectQueued(item, 0, 1)

	qt.set(nodeID, 2, 3)
	qt.expect(qt.drain())
	qt.expectQueued(item, 0, 1, 2, 3)

	qt.setMode(ua.MonitoringModeReporting, item)
	qt.expect(qt.drain(), handleValue{1, 0}, handleValue{1, 1}, handleValue{1, 2}, handleValue{1, 3})
}

// Transitions Disabled -> Reporting and Disabled -> Sampling: the first
// sample is taken at once, whether or not the value changed while the item
// was disabled, and even inside the item's sampling interval (§5.13.1.3).
func TestMonitoringModeEnableSamplesAtOnce(t *testing.T) {
	t.Run("reporting", func(t *testing.T) {
		qt := newQueueTest(t)
		nodeID := qt.node("value")
		item := qt.createInModeWith(nodeID, ua.MonitoringModeReporting, &ua.MonitoringParameters{
			ClientHandle:     1,
			SamplingInterval: 60000,
			QueueSize:        10,
		})
		qt.expect(qt.drain(), handleValue{1, 0})

		qt.setMode(ua.MonitoringModeDisabled, item)
		qt.setMode(ua.MonitoringModeReporting, item)
		qt.expect(qt.drain(), handleValue{1, 0})
		qt.expect(qt.drain())
	})
	t.Run("sampling", func(t *testing.T) {
		qt := newQueueTest(t)
		nodeID := qt.node("value")
		item := qt.createInMode(nodeID, 1, 10, ua.MonitoringModeDisabled)
		qt.set(nodeID, 7)

		qt.setMode(ua.MonitoringModeSampling, item)
		qt.expect(qt.drain())
		qt.expectQueued(item, 7)

		qt.setMode(ua.MonitoringModeReporting, item)
		qt.expect(qt.drain(), handleValue{1, 7})
		qt.expect(qt.drain())
	})
}

// A sample deferred to the end of the sampling interval is dropped when the
// item is disabled before it is taken.
func TestMonitoringModeDisabledDropsDeferredSample(t *testing.T) {
	const interval = 50 * time.Millisecond
	qt := newQueueTest(t)
	nodeID := qt.node("value")
	item := qt.createInModeWith(nodeID, ua.MonitoringModeReporting, &ua.MonitoringParameters{
		ClientHandle:     1,
		SamplingInterval: float64(interval / time.Millisecond),
		QueueSize:        10,
	})
	qt.expect(qt.drain(), handleValue{1, 0})

	// Within the interval of the initial sample: deferred.
	qt.set(nodeID, 1)
	qt.setMode(ua.MonitoringModeDisabled, item)
	time.Sleep(3 * interval)
	qt.expectQueued(item)

	qt.srv.MonitoredItemService.Mu.Lock()
	pending := qt.srv.MonitoredItemService.Items[item].pending
	qt.srv.MonitoredItemService.Mu.Unlock()
	if pending {
		t.Fatal("disabled item still has a deferred sample")
	}
}

// CreateMonitoredItems in each mode. Disabled: no initial value, and no
// sample until enabled. Sampling: the initial value is queued, not reported.
// Reporting: the initial value is reported.
func TestCreateMonitoredItemsInMode(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		qt := newQueueTest(t)
		nodeID := qt.node("value")
		item := qt.createInMode(nodeID, 1, 10, ua.MonitoringModeDisabled)
		qt.set(nodeID, 1, 2)
		qt.expect(qt.drain())
		qt.expectQueued(item)

		qt.setMode(ua.MonitoringModeReporting, item)
		qt.expect(qt.drain(), handleValue{1, 2})
	})
	t.Run("sampling", func(t *testing.T) {
		qt := newQueueTest(t)
		nodeID := qt.node("value")
		item := qt.createInMode(nodeID, 1, 10, ua.MonitoringModeSampling)
		qt.expect(qt.drain())
		qt.expectQueued(item, 0)

		qt.setMode(ua.MonitoringModeReporting, item)
		qt.expect(qt.drain(), handleValue{1, 0})
	})
	t.Run("reporting", func(t *testing.T) {
		qt := newQueueTest(t)
		nodeID := qt.node("value")
		qt.createInMode(nodeID, 1, 10, ua.MonitoringModeReporting)
		qt.expect(qt.drain(), handleValue{1, 0})
	})
}

// Setting the mode an item already has answers Good and changes nothing: the
// queue keeps its values and no extra sample is taken.
func TestSetMonitoringModeUnchanged(t *testing.T) {
	for _, mode := range []ua.MonitoringMode{ua.MonitoringModeDisabled, ua.MonitoringModeSampling, ua.MonitoringModeReporting} {
		t.Run(mode.String(), func(t *testing.T) {
			qt := newQueueTest(t)
			nodeID := qt.node("value")
			item := qt.createInMode(nodeID, 1, 10, ua.MonitoringModeReporting)
			qt.set(nodeID, 1, 2)
			qt.setMode(mode, item)
			before := qt.queued(item)

			qt.setMode(mode, item)
			qt.expectQueued(item, before...)

			qt.setMode(ua.MonitoringModeReporting, item)
			var want []handleValue
			if mode == ua.MonitoringModeDisabled {
				// Enabling samples the current value once.
				want = []handleValue{{1, 2}}
			} else {
				want = []handleValue{{1, 0}, {1, 1}, {1, 2}}
			}
			qt.expect(qt.drain(), want...)
		})
	}
}

// The publish loop: values held by a sampling item neither go into a Publish
// response nor hold back the keep-alive (Part 4 §5.14.1.1); after the switch
// to Reporting they are published.
func TestSubscriptionPublishesOnlyReportingItems(t *testing.T) {
	const interval = 50 * time.Millisecond
	pt := newPublishTest(t, interval, 3)
	a, b := pt.node("a"), pt.node("b")
	pt.createInMode(a, 1, 10, ua.MonitoringModeReporting)
	itemB := pt.createInMode(b, 2, 10, ua.MonitoringModeSampling)
	pt.publish(1)
	pt.start()

	handles := func(s publishSent) []uint32 {
		var got []uint32
		for _, eo := range s.resp.NotificationMessage.NotificationData {
			for _, n := range eo.Value.(*ua.DataChangeNotification).MonitoredItems {
				got = append(got, n.ClientHandle)
			}
		}
		return got
	}

	if got := handles(pt.next(20 * interval)); len(got) != 1 || got[0] != 1 {
		t.Fatalf("first response carries handles %v, want [1]", got)
	}

	pt.set(b, 1, 2)
	pt.publish(2)
	if got := handles(pt.next(20 * interval)); len(got) != 0 {
		t.Fatalf("response carries handles %v, want a keep-alive", got)
	}

	pt.setMode(ua.MonitoringModeReporting, itemB)
	pt.publish(3)
	if got := handles(pt.next(20 * interval)); len(got) != 3 || got[0] != 2 || got[1] != 2 || got[2] != 2 {
		t.Fatalf("response carries handles %v, want [2 2 2]", got)
	}
}
