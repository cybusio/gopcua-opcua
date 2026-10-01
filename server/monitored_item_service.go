package server

import (
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uasc"
)

// MonitoredItemService implements the MonitoredItem Service Set.
//
// https://reference.opcfoundation.org/Core/Part4/v105/docs/5.13
type MonitoredItemService struct {
	SubService *SubscriptionService
	Mu         sync.Mutex

	// items tracked by ID
	Items map[uint32]*MonitoredItem
	// items tracked by node
	Nodes map[string][]*MonitoredItem
	// items tracked by subscription
	Subs map[uint32][]*MonitoredItem

	id uint32
}

// function to get rid of all references to a specific Monitored Item (by ID number)
func (s *MonitoredItemService) DeleteMonitoredItem(id uint32) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.deleteMonitoredItemLocked(id)
}

// deleteMonitoredItemLocked is DeleteMonitoredItem for callers that hold s.Mu.
func (s *MonitoredItemService) deleteMonitoredItemLocked(id uint32) {
	item, ok := s.Items[id]
	if !ok {
		// id does not exist.
		return
	}

	if item == nil || item.Req == nil || item.Req.ItemToMonitor == nil || item.Req.ItemToMonitor.NodeID == nil {
		return
	}
	nodeid := item.Req.ItemToMonitor.NodeID.String()

	if s == nil || s.Nodes == nil || s.Nodes[nodeid] == nil {
		return
	}

	// delete the monitored item from all nodes
	// was using slices.DeleteFunc but that is from a newer go version so we'll do it manually with /exp/slices
	// we've got to go backwards because we're deleting from the slice as we go.
	// I'm guessing this loop is less efficient than slices.DeleteFunc but it's what we've got.
	delete(s.Items, id)
	if item.pendingTimer != nil {
		item.pendingTimer.Stop()
		item.pendingTimer = nil
	}
	if item.Sub != nil {
		item.Sub.removeQueue(id)
	}
	for i := len(s.Nodes[nodeid]) - 1; i >= 0; i-- {
		n := s.Nodes[nodeid][i]
		if n == nil {
			continue
		}
		if n.ID == id {
			s.Nodes[nodeid] = slices.Delete(s.Nodes[nodeid], i, i+1)
		}
	}
	//slices.DeleteFunc(s.Nodes[nodeid], func(i *MonitoredItem) bool { return i.ID == item.ID })
	if len(s.Nodes[nodeid]) == 0 {
		delete(s.Nodes, nodeid)
	}

	for i := len(s.Subs[item.Sub.ID]) - 1; i >= 0; i-- {
		n := s.Subs[item.Sub.ID][i]
		if n == nil {
			continue
		}
		if n.ID == id {
			s.Subs[item.Sub.ID] = slices.Delete(s.Subs[item.Sub.ID], i, i+1)
		}
	}
	//slices.DeleteFunc(s.Subs[item.Sub.ID], func(i *MonitoredItem) bool { return i.ID == item.ID })
	if len(s.Subs[item.Sub.ID]) == 0 {
		delete(s.Subs, item.Sub.ID)
	}
}

// function to delete all monitored items associated with a specific sub (as indicated by id number)
func (s *MonitoredItemService) DeleteSub(id uint32) {
	s.Mu.Lock()
	items, ok := s.Subs[id]
	delete(s.Subs, id)
	s.Mu.Unlock()
	if !ok {
		return
	}
	for i := range items {
		if items[i] != nil {
			s.DeleteMonitoredItem(items[i].ID)
		}
	}
}

func (s *MonitoredItemService) ChangeNotification(n *ua.NodeID) {

	s.Mu.Lock()
	defer s.Mu.Unlock()
	items, ok := s.Nodes[n.String()]

	if !ok {
		// this node isn't monitored - don't have to do anything.
		return
	}

	for i := range items {
		item := items[i]
		if item == nil {
			continue
		}
		s.queueNotificationLocked(item, n)
	}

}

// queueNotificationLocked handles a change of the value monitored by item. It
// samples at once, or defers the sample to the end of the item's current
// sampling interval. The caller must hold s.Mu.
func (s *MonitoredItemService) queueNotificationLocked(item *MonitoredItem, nodeID *ua.NodeID) {
	if item == nil || item.Req == nil || item.Req.ItemToMonitor == nil || item.Req.ItemToMonitor.NodeID == nil {
		return
	}
	// A disabled item is not sampled (Part 4 §5.13.1.3).
	if item.Mode == ua.MonitoringModeDisabled {
		return
	}

	// The interval in use is always the one reported to the client.
	interval := samplingDuration(item.RevisedSamplingInterval)
	now := time.Now()
	if interval <= 0 || item.lastSampledAt.IsZero() || now.Sub(item.lastSampledAt) >= interval {
		s.dispatchNotificationLocked(item, nodeID, now)
		return
	}

	if item.pending {
		return
	}

	delay := interval - now.Sub(item.lastSampledAt)
	if delay < 0 {
		delay = 0
	}
	item.pending = true
	item.pendingTimer = time.AfterFunc(delay, func() {
		s.firePendingNotification(item.ID)
	})
}

func (s *MonitoredItemService) firePendingNotification(id uint32) {
	s.Mu.Lock()
	defer s.Mu.Unlock()

	item, ok := s.Items[id]
	if !ok || item == nil || !item.pending || item.Mode == ua.MonitoringModeDisabled || item.Req == nil || item.Req.ItemToMonitor == nil || item.Req.ItemToMonitor.NodeID == nil {
		return
	}

	s.dispatchNotificationLocked(item, item.Req.ItemToMonitor.NodeID, time.Now())
}

func (s *MonitoredItemService) dispatchNotificationLocked(item *MonitoredItem, nodeID *ua.NodeID, now time.Time) {
	if item.pendingTimer != nil {
		item.pendingTimer.Stop()
		item.pendingTimer = nil
	}
	item.pending = false
	item.lastSampledAt = now
	s.sampleLocked(item, nodeID)
}

// sampleLocked reads the monitored attribute of item and queues the value
// in the item's queue. The caller must hold s.Mu.
func (s *MonitoredItemService) sampleLocked(item *MonitoredItem, n *ua.NodeID) {
	ns, err := s.SubService.srv.Namespace(int(n.Namespace()))
	if err != nil {
		if s.SubService.srv.cfg.logger != nil {
			s.SubService.srv.cfg.logger.Warn("error getting namespace %d: %v", n.Namespace(), err)
		}
		item.Sub.enqueue(item.ID, &ua.DataValue{
			EncodingMask: ua.DataValueStatusCode,
			Status:       ua.StatusBad,
		})
		return
	}
	item.Sub.enqueue(item.ID, ns.Attribute(n, item.Req.ItemToMonitor.AttributeID))
}

func (s *MonitoredItemService) NextID() uint32 {
	i := atomic.AddUint32(&s.id, 1)
	if i == 0 {
		i = atomic.AddUint32(&s.id, 1)
	}
	return i
}

type MonitoredItem struct {
	ID  uint32
	Sub *Subscription
	Req *ua.MonitoredItemCreateRequest

	// Mode is the MonitoringMode of the item (Part 4 §5.13.1.3). A disabled
	// item is not sampled; a sampling item queues its values without
	// reporting them. Use SetMonitoringMode to change it.
	Mode ua.MonitoringMode

	// RevisedSamplingInterval is the sampling interval in use for the item,
	// in milliseconds.
	RevisedSamplingInterval float64

	lastSampledAt time.Time
	pending       bool
	pendingTimer  *time.Timer

	// RevisedQueueSize reports the queue size in use for the item. The
	// server sets it; changing it does not resize the queue.
	RevisedQueueSize uint32
}

// subscriptionForCreate returns the Subscription that a CreateMonitoredItems
// request adds its items to. If the request cannot be processed as a whole,
// it returns the service result for a ServiceFault instead (Part 4 §5.13.2.3
// Table 64 and §7.38.2 Table 178), checked in this order:
// Bad_SessionIdInvalid if the authenticationToken identifies no Session,
// Bad_SubscriptionIdInvalid if subscriptionId identifies no Subscription or
// a Subscription of another Session (§5.14.1: a Subscription belongs to the
// Session that created it), and Bad_NothingToDo for an empty itemsToCreate.
func (s *MonitoredItemService) subscriptionForCreate(req *ua.CreateMonitoredItemsRequest) (*Subscription, ua.StatusCode) {
	hdr := req.RequestHeader
	if hdr == nil || hdr.AuthenticationToken == nil {
		return nil, ua.StatusBadSessionIDInvalid
	}
	sess := s.SubService.srv.Session(hdr)
	if sess == nil {
		return nil, ua.StatusBadSessionIDInvalid
	}
	s.SubService.Mu.Lock()
	sub := s.SubService.Subs[req.SubscriptionID]
	s.SubService.Mu.Unlock()
	if sub == nil || sub.Session != sess {
		return nil, ua.StatusBadSubscriptionIDInvalid
	}
	if len(req.ItemsToCreate) == 0 {
		return nil, ua.StatusBadNothingToDo
	}
	return sub, ua.StatusOK
}

// createServiceFault answers a CreateMonitoredItems request that fails as a
// whole. Part 4 §7.34: the ServiceFault carries the requestHandle the client
// sent.
func createServiceFault(hdr *ua.RequestHeader, status ua.StatusCode) ua.Response {
	var handle uint32
	if hdr != nil {
		handle = hdr.RequestHandle
	}
	return &ua.ServiceFault{ResponseHeader: responseHeader(handle, status)}
}

// https://reference.opcfoundation.org/Core/Part4/v105/docs/5.13.2
// TODO: per-item results are never validated; every item is accepted with
// StatusOK whether or not the node exists. Part 4 §5.13.2.4 (Table 65) defines
// operation-level result codes such as Bad_NodeIdUnknown. Once implemented,
// client failure-mode tests (e.g. a rejected item during subscription
// recreation) could run against this server instead of an integration fixture.
func (s *MonitoredItemService) CreateMonitoredItems(sc *uasc.SecureChannel, r ua.Request, reqID uint32) (ua.Response, error) {
	if s.SubService.srv.cfg.logger != nil {
		s.SubService.srv.cfg.logger.Debug("Handling %T", r)
	}

	req, err := safeReq[*ua.CreateMonitoredItemsRequest](r)
	if err != nil {
		return nil, err
	}
	if req.TimestampsToReturn > ua.TimestampsToReturnNeither {
		return &ua.ServiceFault{ResponseHeader: responseHeader(req.RequestHeader.RequestHandle, ua.StatusBadTimestampsToReturnInvalid)}, nil
	}
	s.Mu.Lock()
	defer s.Mu.Unlock()

	count := len(req.ItemsToCreate)

	res := make([]*ua.MonitoredItemCreateResult, count)

	subID := req.SubscriptionID
	if s.SubService.srv.cfg.logger != nil {
		s.SubService.srv.cfg.logger.Debug("Creating monitored items for sub #%d", subID)
	}
	sub, status := s.subscriptionForCreate(req)
	if status != ua.StatusOK {
		return createServiceFault(req.RequestHeader, status), nil
	}

	maxQueueSize := s.SubService.srv.cfg.cap.MaxMonitoredItemsQueueSize
	for i := range req.ItemsToCreate {
		// The item keeps a shallow copy of the request, so that filling in
		// defaults does not write to the caller's request.
		reqCopy := *req.ItemsToCreate[i]
		itemreq := &reqCopy
		nodeid := itemreq.ItemToMonitor.NodeID
		if itemreq.RequestedParameters == nil {
			// Treat missing parameters as all defaults.
			itemreq.RequestedParameters = &ua.MonitoringParameters{}
		}
		params := itemreq.RequestedParameters
		item := MonitoredItem{
			ID:                      s.NextID(),
			Sub:                     sub,
			Req:                     itemreq,
			Mode:                    itemreq.MonitoringMode,
			RevisedSamplingInterval: revisedSamplingInterval(params, sub.RevisedPublishingInterval),
			RevisedQueueSize:        revisedQueueSize(params.QueueSize, maxQueueSize),
		}
		sub.createQueue(item.ID, params.ClientHandle, item.RevisedQueueSize, params.DiscardOldest, item.Mode)

		// book keeping of the new item
		s.Items[item.ID] = &item
		list, ok := s.Nodes[item.Req.ItemToMonitor.NodeID.String()]
		if !ok {
			list = make([]*MonitoredItem, 0, 1)
		}
		s.Nodes[item.Req.ItemToMonitor.NodeID.String()] = append(list, &item)

		list, ok = s.Subs[item.Sub.ID]
		if !ok {
			list = make([]*MonitoredItem, 0, 1)
		}
		s.Subs[item.Sub.ID] = append(list, &item)

		if s.SubService.srv.cfg.logger != nil {
			s.SubService.srv.cfg.logger.Debug("Adding monitored item '%s' to sub #%d as %d->%d",
				nodeid.String(),
				subID,
				item.ID,
				itemreq.RequestedParameters.ClientHandle)
		}
		res[i] = &ua.MonitoredItemCreateResult{
			StatusCode:              ua.StatusOK,
			MonitoredItemID:         item.ID,
			RevisedSamplingInterval: item.RevisedSamplingInterval,
			RevisedQueueSize:        item.RevisedQueueSize,
			FilterResult:            ua.NewExtensionObject(nil),
		}
		// Queue the initial value of the new item (Part 4 §7.25.2) before
		// responding, which §5.13.2.1 permits. Only this item is sampled:
		// other items on the same node keep their queues as they are.
		s.queueNotificationLocked(&item, nodeid)

	}

	resp := &ua.CreateMonitoredItemsResponse{
		ResponseHeader: &ua.ResponseHeader{
			Timestamp:          time.Now(),
			RequestHandle:      req.RequestHeader.RequestHandle,
			ServiceResult:      ua.StatusOK,
			ServiceDiagnostics: &ua.DiagnosticInfo{},
			StringTable:        []string{},
			AdditionalHeader:   ua.NewExtensionObject(nil),
		},
		Results:         res,                    //                  []StatusCode
		DiagnosticInfos: []*ua.DiagnosticInfo{}, //          []*DiagnosticInfo
	}

	return resp, nil

}

// revisedSamplingInterval returns the sampling interval the server uses for
// a MonitoredItem. Per Part 4 §5.13.1.2 a negative interval requests the
// publishing interval of the Subscription, and an interval of 0 is kept: the
// item is exception-based and every change is sampled.
func revisedSamplingInterval(params *ua.MonitoringParameters, publishingInterval float64) float64 {
	if params == nil {
		return defaultSamplingInterval(publishingInterval)
	}
	requested := params.SamplingInterval
	if requested < 0 || math.IsNaN(requested) {
		return defaultSamplingInterval(publishingInterval)
	}
	return requested
}

func defaultSamplingInterval(publishingInterval float64) float64 {
	if publishingInterval > 0 {
		return publishingInterval
	}
	return 0
}

func samplingDuration(interval float64) time.Duration {
	if interval <= 0 {
		return 0
	}
	return time.Duration(interval * float64(time.Millisecond))
}

// https://reference.opcfoundation.org/Core/Part4/v105/docs/5.13.3
func (s *MonitoredItemService) ModifyMonitoredItems(sc *uasc.SecureChannel, r ua.Request, reqID uint32) (ua.Response, error) {
	if s.SubService.srv.cfg.logger != nil {
		s.SubService.srv.cfg.logger.Debug("Handling %T", r)
	}

	req, err := safeReq[*ua.ModifyMonitoredItemsRequest](r)
	if err != nil {
		return nil, err
	}
	if len(req.ItemsToModify) == 0 {
		return &ua.ServiceFault{ResponseHeader: responseHeader(req.RequestHeader.RequestHandle, ua.StatusBadNothingToDo)}, nil
	}
	if req.TimestampsToReturn > ua.TimestampsToReturnNeither {
		return &ua.ServiceFault{ResponseHeader: responseHeader(req.RequestHeader.RequestHandle, ua.StatusBadTimestampsToReturnInvalid)}, nil
	}

	s.SubService.Mu.Lock()
	sub, ok := s.SubService.Subs[req.SubscriptionID]
	s.SubService.Mu.Unlock()
	sess := s.SubService.srv.Session(req.RequestHeader)
	if !ok || sess == nil || sub.Session == nil || sub.Session.AuthTokenID.String() != sess.AuthTokenID.String() {
		return &ua.ServiceFault{ResponseHeader: responseHeader(req.RequestHeader.RequestHandle, ua.StatusBadSubscriptionIDInvalid)}, nil
	}

	s.Mu.Lock()
	defer s.Mu.Unlock()

	maxQueueSize := s.SubService.srv.cfg.cap.MaxMonitoredItemsQueueSize
	results := make([]*ua.MonitoredItemModifyResult, len(req.ItemsToModify))
	for i, itemreq := range req.ItemsToModify {
		item, ok := s.Items[itemreq.MonitoredItemID]
		if !ok || item.Sub != sub {
			results[i] = &ua.MonitoredItemModifyResult{
				StatusCode:   ua.StatusBadMonitoredItemIDInvalid,
				FilterResult: ua.NewExtensionObject(nil),
			}
			continue
		}
		params := itemreq.RequestedParameters
		if params == nil {
			params = item.Req.RequestedParameters
		}

		// Revise the parameters by the same rules as CreateMonitoredItems and
		// apply them at once, including to the values already queued. An item
		// whose deletion is still being completed has no queue any more and
		// is treated as unknown.
		queueSize := revisedQueueSize(params.QueueSize, maxQueueSize)
		if !sub.updateQueue(item.ID, params.ClientHandle, queueSize, params.DiscardOldest) {
			results[i] = &ua.MonitoredItemModifyResult{
				StatusCode:   ua.StatusBadMonitoredItemIDInvalid,
				FilterResult: ua.NewExtensionObject(nil),
			}
			continue
		}
		item.Req.RequestedParameters = params
		item.RevisedSamplingInterval = revisedSamplingInterval(params, sub.RevisedPublishingInterval)
		item.RevisedQueueSize = queueSize

		results[i] = &ua.MonitoredItemModifyResult{
			StatusCode:              ua.StatusOK,
			RevisedSamplingInterval: item.RevisedSamplingInterval,
			RevisedQueueSize:        item.RevisedQueueSize,
			FilterResult:            ua.NewExtensionObject(nil),
		}
	}

	return &ua.ModifyMonitoredItemsResponse{
		ResponseHeader:  responseHeader(req.RequestHeader.RequestHandle, ua.StatusOK),
		Results:         results,
		DiagnosticInfos: []*ua.DiagnosticInfo{},
	}, nil
}

// https://reference.opcfoundation.org/Core/Part4/v105/docs/5.13.4
func (s *MonitoredItemService) SetMonitoringMode(sc *uasc.SecureChannel, r ua.Request, reqID uint32) (ua.Response, error) {
	if s.SubService.srv.cfg.logger != nil {
		s.SubService.srv.cfg.logger.Debug("Handling %T", r)
	}

	req, err := safeReq[*ua.SetMonitoringModeRequest](r)
	if err != nil {
		return nil, err
	}
	sub, status := s.subscriptionForItems(req.RequestHeader, req.SubscriptionID, len(req.MonitoredItemIDs))
	if status != ua.StatusOK {
		return itemServiceFault(req.RequestHeader, status), nil
	}
	// Table 70: the monitoring mode applies to every item of the request.
	if req.MonitoringMode > ua.MonitoringModeReporting {
		return itemServiceFault(req.RequestHeader, ua.StatusBadMonitoringModeInvalid), nil
	}

	s.Mu.Lock()
	defer s.Mu.Unlock()

	results := make([]ua.StatusCode, len(req.MonitoredItemIDs))
	for i, id := range req.MonitoredItemIDs {
		item, ok := s.itemOf(sub, id)
		if !ok {
			results[i] = ua.StatusBadMonitoredItemIDInvalid
			continue
		}
		s.setMonitoringModeLocked(item, req.MonitoringMode)
		results[i] = ua.StatusOK
	}

	return &ua.SetMonitoringModeResponse{
		ResponseHeader: &ua.ResponseHeader{
			Timestamp:          time.Now(),
			RequestHandle:      req.RequestHeader.RequestHandle,
			ServiceResult:      ua.StatusOK,
			ServiceDiagnostics: &ua.DiagnosticInfo{},
			StringTable:        []string{},
			AdditionalHeader:   ua.NewExtensionObject(nil),
		},
		Results:         results,
		DiagnosticInfos: []*ua.DiagnosticInfo{},
	}, nil

}

// setMonitoringModeLocked changes the MonitoringMode of item. The caller must
// hold s.Mu.
//
// Part 4 §5.13.1.3: a disabled item is neither sampled nor reported, a
// sampling item is sampled and its values are queued but not reported, and a
// reporting item is sampled and reported. When an item is enabled, its first
// sample is taken as soon as possible, whether or not the value changed while
// it was disabled. §5.13.4.1: setting the mode to Disabled deletes all queued
// values of the item. Setting the mode the item already has changes nothing.
// §7.23 defines the three modes; any other value is treated as Reporting.
func (s *MonitoredItemService) setMonitoringModeLocked(item *MonitoredItem, mode ua.MonitoringMode) {
	prev := item.Mode
	if mode == prev {
		return
	}
	item.Mode = mode
	if item.Sub != nil {
		item.Sub.setQueueMode(item.ID, mode)
	}

	switch {
	case mode == ua.MonitoringModeDisabled:
		// Stop sampling: drop a sample deferred to the end of the
		// sampling interval.
		if item.pendingTimer != nil {
			item.pendingTimer.Stop()
			item.pendingTimer = nil
		}
		item.pending = false
	case prev == ua.MonitoringModeDisabled:
		if item.Sub == nil || item.Req == nil || item.Req.ItemToMonitor == nil || item.Req.ItemToMonitor.NodeID == nil {
			return
		}
		s.dispatchNotificationLocked(item, item.Req.ItemToMonitor.NodeID, time.Now())
	}
}

// https://reference.opcfoundation.org/Core/Part4/v105/docs/5.13.5
func (s *MonitoredItemService) SetTriggering(sc *uasc.SecureChannel, r ua.Request, reqID uint32) (ua.Response, error) {
	if s.SubService.srv.cfg.logger != nil {
		s.SubService.srv.cfg.logger.Debug("Handling %T", r)
	}

	req, err := safeReq[*ua.SetTriggeringRequest](r)
	if err != nil {
		return nil, err
	}
	return serviceUnsupported(req.RequestHeader), nil
}

// https://reference.opcfoundation.org/Core/Part4/v105/docs/5.13.6
func (s *MonitoredItemService) DeleteMonitoredItems(sc *uasc.SecureChannel, r ua.Request, reqID uint32) (ua.Response, error) {
	if s.SubService.srv.cfg.logger != nil {
		s.SubService.srv.cfg.logger.Debug("Handling %T", r)
	}

	req, err := safeReq[*ua.DeleteMonitoredItemsRequest](r)
	if err != nil {
		return nil, err
	}

	sub, status := s.subscriptionForItems(req.RequestHeader, req.SubscriptionID, len(req.MonitoredItemIDs))
	if status != ua.StatusOK {
		return itemServiceFault(req.RequestHeader, status), nil
	}

	s.Mu.Lock()
	defer s.Mu.Unlock()

	results := make([]ua.StatusCode, len(req.MonitoredItemIDs))
	for i, id := range req.MonitoredItemIDs {
		if _, ok := s.itemOf(sub, id); !ok {
			results[i] = ua.StatusBadMonitoredItemIDInvalid
			continue
		}
		// Delete before responding, so that a later request for the same
		// id sees Bad_MonitoredItemIdInvalid.
		s.deleteMonitoredItemLocked(id)
		results[i] = ua.StatusOK
	}

	response := &ua.DeleteMonitoredItemsResponse{
		ResponseHeader: &ua.ResponseHeader{
			Timestamp:          time.Now(),
			RequestHandle:      req.RequestHeader.RequestHandle,
			ServiceResult:      ua.StatusOK,
			ServiceDiagnostics: &ua.DiagnosticInfo{},
			StringTable:        []string{},
			AdditionalHeader:   ua.NewExtensionObject(nil),
		},
		Results:         results,
		DiagnosticInfos: []*ua.DiagnosticInfo{},
	}
	return response, nil

}

// subscriptionForItems returns the Subscription that qualifies the
// monitoredItemIds of a SetMonitoringMode or DeleteMonitoredItems request
// (Part 4 §5.13.4.2, §5.13.6.2). If the request cannot be processed, it
// returns the service result for the ServiceFault instead (Tables 70, 76 and
// 178): Bad_SessionIdInvalid if the authenticationToken identifies no Session,
// Bad_NothingToDo for an empty list, and Bad_SubscriptionIdInvalid if the
// Subscription does not exist or belongs to another Session.
func (s *MonitoredItemService) subscriptionForItems(hdr *ua.RequestHeader, subID uint32, count int) (*Subscription, ua.StatusCode) {
	if hdr == nil || hdr.AuthenticationToken == nil {
		return nil, ua.StatusBadSessionIDInvalid
	}
	sess := s.SubService.srv.Session(hdr)
	if sess == nil {
		return nil, ua.StatusBadSessionIDInvalid
	}
	if count == 0 {
		return nil, ua.StatusBadNothingToDo
	}
	s.SubService.Mu.Lock()
	sub := s.SubService.Subs[subID]
	s.SubService.Mu.Unlock()
	if sub == nil || sub.Session != sess {
		return nil, ua.StatusBadSubscriptionIDInvalid
	}
	return sub, ua.StatusOK
}

// itemOf returns the MonitoredItem id if it is one of the items of sub. An id
// that was never created, was deleted, or belongs to another Subscription is
// Bad_MonitoredItemIdInvalid for that operation (Part 4 §5.13.4.4 Table 71,
// §5.13.6.4 Table 77). The caller must hold s.Mu.
func (s *MonitoredItemService) itemOf(sub *Subscription, id uint32) (*MonitoredItem, bool) {
	item, ok := s.Items[id]
	if !ok || item == nil || item.Sub != sub {
		return nil, false
	}
	return item, true
}

// itemServiceFault answers a request that fails as a whole. Part 4 §7.34: the
// requestHandle is the one the client sent.
func itemServiceFault(hdr *ua.RequestHeader, status ua.StatusCode) ua.Response {
	var handle uint32
	if hdr != nil {
		handle = hdr.RequestHandle
	}
	return &ua.ServiceFault{ResponseHeader: responseHeader(handle, status)}
}
