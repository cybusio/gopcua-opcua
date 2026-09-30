package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"
)

func TestMonitoredItemSamplingIntervalCoalescesRapidChanges(t *testing.T) {
	srv := New()
	srv.initHandlers()
	ns := NewNodeNameSpace(srv, "urn:test:sampling")

	var current float64
	nodeID := ua.NewStringNodeID(ns.ID(), "value")
	ns.AddNode(NewVariableNode(nodeID, "Value", func() *ua.DataValue {
		return DataValueFromValue(current)
	}))

	session := srv.sb.NewSession()

	// Register the subscription directly instead of via CreateSubscription.
	// CreateSubscription starts the subscription's run loop, which reads from
	// NotifyChannel; this test reads that channel itself and must be its only
	// consumer, otherwise the two race for every notification.
	sub := NewSubscription()
	sub.srv = srv.SubscriptionService
	sub.Session = session
	sub.ID = 1
	sub.RevisedPublishingInterval = 25
	srv.SubscriptionService.Mu.Lock()
	srv.SubscriptionService.Subs[sub.ID] = sub
	srv.SubscriptionService.Mu.Unlock()
	defer srv.SubscriptionService.DeleteSubscription(sub.ID)

	itemResp, err := srv.MonitoredItemService.CreateMonitoredItems(nil, &ua.CreateMonitoredItemsRequest{
		RequestHeader:  &ua.RequestHeader{AuthenticationToken: session.AuthTokenID},
		SubscriptionID: sub.ID,
		ItemsToCreate: []*ua.MonitoredItemCreateRequest{{
			ItemToMonitor:  &ua.ReadValueID{NodeID: nodeID, AttributeID: ua.AttributeIDValue},
			MonitoringMode: ua.MonitoringModeReporting,
			// A queue larger than one keeps a duplicate sample next to
			// the first one instead of replacing it.
			RequestedParameters: &ua.MonitoringParameters{
				ClientHandle:     1,
				SamplingInterval: 120,
				QueueSize:        10,
				DiscardOldest:    true,
			},
		}},
	}, 0)
	if err != nil {
		t.Fatalf("CreateMonitoredItems returned error: %v", err)
	}

	createItemResp := itemResp.(*ua.CreateMonitoredItemsResponse)
	if got := createItemResp.Results[0].RevisedSamplingInterval; got != 120 {
		t.Fatalf("unexpected revised sampling interval %v", got)
	}

	if _, err := waitNotification(sub, time.Second); err != nil {
		t.Fatalf("expected initial notification: %v", err)
	}

	current = 1.0
	srv.ChangeNotification(nodeID)
	if _, err := waitNotification(sub, 60*time.Millisecond); err == nil {
		t.Fatal("expected sampling interval to delay notification")
	}

	current = 2.0
	srv.ChangeNotification(nodeID)

	msg, err := waitNotification(sub, 250*time.Millisecond)
	if err != nil {
		t.Fatalf("expected sampled notification: %v", err)
	}
	if got := msg.Value.Value.Value(); got != float64(2) {
		t.Fatalf("expected latest sampled value 2, got %#v", got)
	}

	if _, err := waitNotification(sub, 80*time.Millisecond); err == nil {
		t.Fatal("expected coalesced sampled notification without duplicates")
	}
}

// waitNotification waits for the next notification sampled for an item of
// sub.
func waitNotification(sub *Subscription, timeout time.Duration) (*ua.MonitoredItemNotification, error) {
	deadline := time.Now().Add(timeout)
	for {
		switch items, _ := sub.drainQueues(0); len(items) {
		case 0:
		case 1:
			return items[0], nil
		default:
			return nil, fmt.Errorf("got %d queued notifications, want 1", len(items))
		}
		if time.Now().After(deadline) {
			return nil, contextDeadlineExceeded{}
		}
		time.Sleep(time.Millisecond)
	}
}

type contextDeadlineExceeded struct{}

func (contextDeadlineExceeded) Error() string {
	return "deadline exceeded"
}

// Sampling uses the item's RevisedSamplingInterval, so a revised interval
// takes effect for the next change.
func TestMonitoredItemSamplingFollowsRevisedSamplingInterval(t *testing.T) {
	srv := New()
	srv.initHandlers()
	ns := NewNodeNameSpace(srv, "urn:test:sampling-revised")

	var current float64
	nodeID := ua.NewStringNodeID(ns.ID(), "value")
	ns.AddNode(NewVariableNode(nodeID, "Value", func() *ua.DataValue {
		return DataValueFromValue(current)
	}))

	session := srv.sb.NewSession()

	// Registered without Start, see TestMonitoredItemSamplingIntervalCoalescesRapidChanges.
	sub := NewSubscription()
	sub.srv = srv.SubscriptionService
	sub.Session = session
	sub.ID = 1
	sub.RevisedPublishingInterval = 1000
	srv.SubscriptionService.Mu.Lock()
	srv.SubscriptionService.Subs[sub.ID] = sub
	srv.SubscriptionService.Mu.Unlock()
	defer srv.SubscriptionService.DeleteSubscription(sub.ID)

	itemResp, err := srv.MonitoredItemService.CreateMonitoredItems(nil, &ua.CreateMonitoredItemsRequest{
		RequestHeader:  &ua.RequestHeader{AuthenticationToken: session.AuthTokenID},
		SubscriptionID: sub.ID,
		ItemsToCreate: []*ua.MonitoredItemCreateRequest{{
			ItemToMonitor:  &ua.ReadValueID{NodeID: nodeID, AttributeID: ua.AttributeIDValue},
			MonitoringMode: ua.MonitoringModeReporting,
			RequestedParameters: &ua.MonitoringParameters{
				ClientHandle:     1,
				SamplingInterval: 10000,
				QueueSize:        10,
				DiscardOldest:    true,
			},
		}},
	}, 0)
	if err != nil {
		t.Fatalf("CreateMonitoredItems returned error: %v", err)
	}
	if _, err := waitNotification(sub, time.Second); err != nil {
		t.Fatalf("expected initial notification: %v", err)
	}

	id := itemResp.(*ua.CreateMonitoredItemsResponse).Results[0].MonitoredItemID
	srv.MonitoredItemService.Mu.Lock()
	srv.MonitoredItemService.Items[id].RevisedSamplingInterval = 0
	srv.MonitoredItemService.Mu.Unlock()

	for _, v := range []float64{1, 2} {
		current = v
		srv.ChangeNotification(nodeID)
		msg, err := waitNotification(sub, 50*time.Millisecond)
		if err != nil {
			t.Fatalf("value %v: expected an immediate notification: %v", v, err)
		}
		if got := msg.Value.Value.Value(); got != v {
			t.Fatalf("got %#v, want %v", got, v)
		}
	}
}

func TestRevisedSamplingInterval(t *testing.T) {
	tests := []struct {
		name               string
		params             *ua.MonitoringParameters
		publishingInterval float64
		want               float64
	}{
		{"negative uses the publishing interval", &ua.MonitoringParameters{SamplingInterval: -1}, 250, 250},
		{"any negative value", &ua.MonitoringParameters{SamplingInterval: -40}, 250, 250},
		{"zero is exception-based", &ua.MonitoringParameters{SamplingInterval: 0}, 250, 0},
		{"positive is kept", &ua.MonitoringParameters{SamplingInterval: 120}, 250, 120},
		{"no parameters", nil, 250, 250},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := revisedSamplingInterval(tt.params, tt.publishingInterval); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// A MonitoredItem with a sampling interval of 0 is exception-based: every
// change is sampled at once, none is coalesced (Part 4 §5.13.1.2).
func TestMonitoredItemZeroSamplingIntervalSamplesEveryChange(t *testing.T) {
	srv := New()
	srv.initHandlers()
	ns := NewNodeNameSpace(srv, "urn:test:sampling-zero")

	var current float64
	nodeID := ua.NewStringNodeID(ns.ID(), "value")
	ns.AddNode(NewVariableNode(nodeID, "Value", func() *ua.DataValue {
		return DataValueFromValue(current)
	}))

	session := srv.sb.NewSession()

	// Registered without Start, see TestMonitoredItemSamplingIntervalCoalescesRapidChanges.
	sub := NewSubscription()
	sub.srv = srv.SubscriptionService
	sub.Session = session
	sub.ID = 1
	sub.RevisedPublishingInterval = 1000
	srv.SubscriptionService.Mu.Lock()
	srv.SubscriptionService.Subs[sub.ID] = sub
	srv.SubscriptionService.Mu.Unlock()
	defer srv.SubscriptionService.DeleteSubscription(sub.ID)

	itemResp, err := srv.MonitoredItemService.CreateMonitoredItems(nil, &ua.CreateMonitoredItemsRequest{
		RequestHeader:  &ua.RequestHeader{AuthenticationToken: session.AuthTokenID},
		SubscriptionID: sub.ID,
		ItemsToCreate: []*ua.MonitoredItemCreateRequest{{
			ItemToMonitor:  &ua.ReadValueID{NodeID: nodeID, AttributeID: ua.AttributeIDValue},
			MonitoringMode: ua.MonitoringModeReporting,
			RequestedParameters: &ua.MonitoringParameters{
				ClientHandle:     1,
				SamplingInterval: 0,
				QueueSize:        1,
				DiscardOldest:    true,
			},
		}},
	}, 0)
	if err != nil {
		t.Fatalf("CreateMonitoredItems returned error: %v", err)
	}
	if got := itemResp.(*ua.CreateMonitoredItemsResponse).Results[0].RevisedSamplingInterval; got != 0 {
		t.Fatalf("unexpected revised sampling interval %v", got)
	}
	if _, err := waitNotification(sub, time.Second); err != nil {
		t.Fatalf("expected initial notification: %v", err)
	}

	for _, v := range []float64{1, 2, 3} {
		current = v
		srv.ChangeNotification(nodeID)
		msg, err := waitNotification(sub, 50*time.Millisecond)
		if err != nil {
			t.Fatalf("value %v: expected an immediate notification: %v", v, err)
		}
		if got := msg.Value.Value.Value(); got != v {
			t.Fatalf("got %#v, want %v", got, v)
		}
	}
}
