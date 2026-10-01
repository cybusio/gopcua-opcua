package server

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"
)

// TestCreateMonitoredItemsConcurrentWithSubscriptionEnd runs
// CreateMonitoredItems while the subscription it adds items to ends, either
// by DeleteSubscriptions or by lifetime expiry. CreateMonitoredItems used to
// take MonitoredItemService.Mu and then SubscriptionService.Mu, while
// DeleteSubscription takes them in the opposite order, so the two could
// deadlock. The test fails if the calls do not finish in time, and also if a
// monitored item outlives its subscription.
func TestCreateMonitoredItemsConcurrentWithSubscriptionEnd(t *testing.T) {
	const (
		workers       = 8
		subsPerWorker = 25
		// Each item is created on its own node, so it queues one
		// notification. Staying well below the 100 notifications a
		// subscription buffers keeps an ended subscription from blocking
		// ChangeNotification.
		createsPerSub = 40
		watchdog      = 30 * time.Second
		drainTimeout  = 10 * time.Second
	)

	srv := New()
	srv.initHandlers()
	sess := srv.sb.NewSession()
	hdr := func() *ua.RequestHeader {
		return &ua.RequestHeader{AuthenticationToken: sess.AuthTokenID}
	}

	var subID, nodeID atomic.Uint32
	nodeID.Store(1 << 30)

	startSub := func(lifetimeCount uint32) uint32 {
		id := subID.Add(1)
		sub := NewSubscription()
		sub.srv = srv.SubscriptionService
		sub.Session = sess
		sub.ID = id
		sub.RevisedPublishingInterval = 1
		sub.RevisedLifetimeCount = lifetimeCount
		sub.RevisedMaxKeepAliveCount = 0
		srv.SubscriptionService.Mu.Lock()
		srv.SubscriptionService.Subs[id] = sub
		sub.running = true
		sub.Start()
		srv.SubscriptionService.Mu.Unlock()
		return id
	}

	// Every answer is counted. Anything but a created item or the answer
	// for a subscription that has ended fails the test.
	var created, ended, unexpected atomic.Int64
	var samplesMu sync.Mutex
	var samples []string
	createItem := func(id uint32) bool {
		resp, err := srv.MonitoredItemService.CreateMonitoredItems(nil, &ua.CreateMonitoredItemsRequest{
			RequestHeader:      hdr(),
			SubscriptionID:     id,
			TimestampsToReturn: ua.TimestampsToReturnBoth,
			ItemsToCreate: []*ua.MonitoredItemCreateRequest{{
				ItemToMonitor:       &ua.ReadValueID{NodeID: ua.NewNumericNodeID(0, nodeID.Add(1)), AttributeID: ua.AttributeIDValue},
				MonitoringMode:      ua.MonitoringModeReporting,
				RequestedParameters: &ua.MonitoringParameters{ClientHandle: 1, QueueSize: 1},
			}},
		}, 0)
		switch outcome, detail := classifyCreate(resp, err); outcome {
		case createOK:
			created.Add(1)
			return true
		case createSubscriptionEnded:
			ended.Add(1)
		default:
			unexpected.Add(1)
			samplesMu.Lock()
			if len(samples) < 5 {
				samples = append(samples, detail)
			}
			samplesMu.Unlock()
		}
		return false
	}

	deleteSub := func(id uint32) {
		_, _ = srv.SubscriptionService.DeleteSubscriptions(nil, &ua.DeleteSubscriptionsRequest{
			RequestHeader:   hdr(),
			SubscriptionIDs: []uint32{id},
		}, 0)
	}

	// subsAndItems reports the number of subscriptions and the number of
	// entries the MonitoredItemService still tracks. It takes one mutex at a
	// time.
	subsAndItems := func() (subs, items int) {
		srv.SubscriptionService.Mu.Lock()
		subs = len(srv.SubscriptionService.Subs)
		srv.SubscriptionService.Mu.Unlock()
		mi := srv.MonitoredItemService
		mi.Mu.Lock()
		items = len(mi.Items) + len(mi.Nodes) + len(mi.Subs)
		mi.Mu.Unlock()
		return subs, items
	}

	done := make(chan struct{})
	var drained bool
	var leftSubs, leftItems int
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range subsPerWorker {
					// Half of the subscriptions are deleted by the client
					// while items are still being created, the others expire
					// after one to four publishing intervals.
					byClient := (w+i)%2 == 0
					id := startSub(uint32(i % 4))
					for k := range createsPerSub {
						if byClient && k == createsPerSub/4 {
							deleteSub(id)
						}
						if !createItem(id) {
							break
						}
					}
				}
			}()
		}
		wg.Wait()

		// Every subscription ends by now or shortly after. None of its items
		// may be left behind.
		deadline := time.Now().Add(drainTimeout)
		for time.Now().Before(deadline) {
			leftSubs, leftItems = subsAndItems()
			if leftSubs == 0 && leftItems == 0 {
				drained = true
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	select {
	case <-done:
	case <-time.After(watchdog):
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("CreateMonitoredItems and subscription deletion did not finish within %v (deadlock?)\n%s", watchdog, buf)
	}
	if n := unexpected.Load(); n > 0 {
		t.Errorf("CreateMonitoredItems: %d unexpected answers (%d created, %d for an ended subscription), e.g. %v",
			n, created.Load(), ended.Load(), samples)
	}
	if created.Load() == 0 || ended.Load() == 0 {
		t.Errorf("CreateMonitoredItems: %d created, %d for an ended subscription; want both > 0", created.Load(), ended.Load())
	}
	if !drained {
		t.Fatalf("after all subscriptions ended: %d subscriptions and %d monitored item entries left", leftSubs, leftItems)
	}
}

type createOutcome int

const (
	createUnexpected createOutcome = iota
	createOK
	createSubscriptionEnded
)

// classifyCreate classifies the answer to a CreateMonitoredItems request for
// one item. The item is created, or the request is rejected because the
// subscription has ended, which the server answers with an error. Anything
// else is unexpected; detail then describes the answer.
func classifyCreate(resp ua.Response, err error) (outcome createOutcome, detail string) {
	if err != nil {
		return createSubscriptionEnded, ""
	}
	if r, ok := resp.(*ua.CreateMonitoredItemsResponse); ok && len(r.Results) == 1 && r.Results[0] != nil &&
		r.Results[0].StatusCode == ua.StatusOK && r.Results[0].MonitoredItemID != 0 {
		return createOK, ""
	}
	return createUnexpected, fmt.Sprintf("%T %+v", resp, resp)
}
