package server

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"
)

// drainN takes at most limit values from the subscription's queues as a
// publish response would.
func (qt *queueTest) drainN(limit uint32) ([]handleValue, bool) {
	items, more := qt.sub.drainQueues(limit)
	return qt.handleValues(items), more
}

// A publish response carries at most maxNotificationsPerPublish values
// (Part 4 §5.14.2.2). The rest stay queued in order and are reported as more
// notifications (§5.14.1.2). The spec only requires the order per item
// (§7.25.1); that the oldest values go first across items is how drainQueues
// orders them.
func TestDrainQueuesHonorsMaxNotificationsPerPublish(t *testing.T) {
	qt := newQueueTest(t)
	a, b := qt.node("a"), qt.node("b")
	qt.create(a, 1, 10, true)
	qt.create(b, 2, 10, true)
	qt.drain()

	for i := int32(1); i <= 3; i++ {
		qt.set(a, 100+i)
		qt.set(b, 200+i)
	}

	got, more := qt.drainN(4)
	qt.expect(got, handleValue{1, 101}, handleValue{2, 201}, handleValue{1, 102}, handleValue{2, 202})
	if !more {
		t.Fatal("more notifications not reported")
	}

	// A value sampled in the meantime queues behind the ones still waiting.
	qt.set(a, 104)
	got, more = qt.drainN(4)
	qt.expect(got, handleValue{1, 103}, handleValue{2, 203}, handleValue{1, 104})
	if more {
		t.Fatal("more notifications reported for an empty queue")
	}

	// A limit that exactly fits the queued values takes all of them.
	qt.set(b, 204)
	got, more = qt.drainN(1)
	qt.expect(got, handleValue{2, 204})
	if more {
		t.Fatal("more notifications reported for an empty queue")
	}
}

// A limit that does not fit in an int32 takes every value. On 32-bit
// platforms a signed comparison would slice out of range and panic.
func TestDrainQueuesLargeLimit(t *testing.T) {
	qt := newQueueTest(t)
	nodeID := qt.node("value")
	qt.create(nodeID, 1, 10, true)
	qt.drain()
	for _, limit := range []uint32{math.MaxInt32, math.MaxInt32 + 1, math.MaxUint32} {
		qt.set(nodeID, 1, 2, 3)
		got, more := qt.drainN(limit)
		if len(got) != 3 || more {
			t.Fatalf("limit %d: got %v more %v, want 3 values", limit, got, more)
		}
	}
}

// The limit for a publish response is the smaller of the client's and the
// server's, where zero means no limit.
func TestNotificationLimit(t *testing.T) {
	qt := newQueueTest(t)
	saved := qt.srv.cfg.cap
	t.Cleanup(func() { qt.srv.cfg.cap = saved })
	if got := qt.srv.cfg.cap.MaxNotificationsPerPublish; got != defaultMaxNotificationsPerPublish {
		t.Errorf("default: got %d, want %d", got, defaultMaxNotificationsPerPublish)
	}
	for _, tt := range []struct{ server, client, want uint32 }{
		{1000, 0, 1000},
		{1000, 10, 10},
		{1000, 5000, 1000},
		{0, 0, 0},
		{0, 10, 10},
	} {
		MaxNotificationsPerPublish(tt.server)(qt.srv.cfg)
		qt.sub.MaxNotificationsPerPublish = tt.client
		if got := qt.sub.notificationLimit(); got != tt.want {
			t.Errorf("server %d, client %d: got %d, want %d", tt.server, tt.client, got, tt.want)
		}
	}
}

// The publish loop sends at most the smaller of the client's and the
// server's notification limit per response, sets moreNotifications while
// values are left (Part 4 §5.14.1.2, §5.14.5.2) and answers the next Publish
// request without waiting for the publishing interval (§5.14.1.2, Table 79).
func TestSubscriptionPublishesMoreNotifications(t *testing.T) {
	const interval = time.Second
	qt := newQueueTest(t, MaxNotificationsPerPublish(2))
	nodeID := qt.node("value")
	qt.create(nodeID, 1, 10, true) // queues the initial value 0
	qt.set(nodeID, 1, 2, 3, 4)

	type sent struct {
		reqID uint32
		resp  *ua.PublishResponse
		at    time.Time
	}
	responses := make(chan sent, 10)
	sub := qt.sub
	sub.RevisedPublishingInterval = float64(interval / time.Millisecond)
	sub.RevisedLifetimeCount = 100
	sub.RevisedMaxKeepAliveCount = 100
	// The client allows three notifications per response, the server two.
	sub.MaxNotificationsPerPublish = 3
	sub.send = func(reqID uint32, resp ua.Response) error {
		responses <- sent{reqID, resp.(*ua.PublishResponse), time.Now()}
		return nil
	}
	for i := uint32(1); i <= 3; i++ {
		qt.sess.PublishRequests <- PubReq{
			Req: &ua.PublishRequest{RequestHeader: &ua.RequestHeader{RequestHandle: i}},
			ID:  i,
		}
	}
	sub.Mu.Lock()
	sub.running = true
	sub.Mu.Unlock()
	sub.Start()
	defer qt.srv.SubscriptionService.DeleteSubscription(sub.ID)

	want := []struct {
		values []int32
		more   bool
	}{
		{[]int32{0, 1}, true},
		{[]int32{2, 3}, true},
		{[]int32{4}, false},
	}
	var first time.Time
	for i, w := range want {
		var s sent
		select {
		case s = <-responses:
		case <-time.After(5 * interval):
			t.Fatalf("response %d: timeout", i+1)
		}
		if first.IsZero() {
			first = s.at
		}
		if s.reqID != uint32(i+1) || s.resp.MoreNotifications != w.more {
			t.Fatalf("response %d: request %d moreNotifications %v, want request %d moreNotifications %v",
				i+1, s.reqID, s.resp.MoreNotifications, i+1, w.more)
		}
		dcn := s.resp.NotificationMessage.NotificationData[0].Value.(*ua.DataChangeNotification)
		var got []int32
		for _, n := range dcn.MonitoredItems {
			got = append(got, n.Value.Value.Value().(int32))
		}
		if !slices.Equal(got, w.values) {
			t.Fatalf("response %d: got values %v, want %v", i+1, got, w.values)
		}
	}
	if d := time.Since(first); d >= interval/2 {
		t.Fatalf("remaining notifications took %v, want less than half the publishing interval %v", d, interval)
	}
}

// BenchmarkDrainQueues takes 100 values from 100 queues of 5000 values each,
// as a publish response with a limit of 100 would, and queues 100 new ones.
func BenchmarkDrainQueues(b *testing.B) {
	const items, depth, limit = 100, 5000, 100
	s := NewSubscription()
	for id := uint32(1); id <= items; id++ {
		s.queues[id] = &notificationQueue{clientHandle: id, size: depth}
	}
	for i := 0; i < depth; i++ {
		for id := uint32(1); id <= items; id++ {
			s.enqueue(id, int32Value(int32(i)))
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got, _ := s.drainQueues(limit); len(got) != limit {
			b.Fatalf("got %d values, want %d", len(got), limit)
		}
		for id := uint32(1); id <= items; id++ {
			s.enqueue(id, int32Value(int32(i)))
		}
	}
}
