package server

import (
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
			RequestedParameters: &ua.MonitoringParameters{
				ClientHandle:     1,
				SamplingInterval: 120,
				QueueSize:        1,
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

	if _, err := waitNotification(sub.NotifyChannel, time.Second); err != nil {
		t.Fatalf("expected initial notification: %v", err)
	}

	current = 1.0
	srv.ChangeNotification(nodeID)
	if _, err := waitNotification(sub.NotifyChannel, 60*time.Millisecond); err == nil {
		t.Fatal("expected sampling interval to delay notification")
	}

	current = 2.0
	srv.ChangeNotification(nodeID)

	msg, err := waitNotification(sub.NotifyChannel, 250*time.Millisecond)
	if err != nil {
		t.Fatalf("expected sampled notification: %v", err)
	}
	if got := msg.Value.Value.Value(); got != float64(2) {
		t.Fatalf("expected latest sampled value 2, got %#v", got)
	}

	if _, err := waitNotification(sub.NotifyChannel, 80*time.Millisecond); err == nil {
		t.Fatal("expected coalesced sampled notification without duplicates")
	}
}

func waitNotification(ch <-chan *ua.MonitoredItemNotification, timeout time.Duration) (*ua.MonitoredItemNotification, error) {
	select {
	case msg := <-ch:
		return msg, nil
	case <-time.After(timeout):
		return nil, contextDeadlineExceeded{}
	}
}

type contextDeadlineExceeded struct{}

func (contextDeadlineExceeded) Error() string {
	return "deadline exceeded"
}
