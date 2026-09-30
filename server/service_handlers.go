// Copyright 2018-2019 opcua authors. All rights reserved.
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

package server

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uasc"
)

type Handler func(*uasc.SecureChannel, ua.Request, uint32) (ua.Response, error)

func (s *Server) initHandlers() {
	// s.registerHandlerFunc(id.ServiceFault_Encoding_DefaultBinary, handleServiceFault)

	discovery := &DiscoveryService{s}
	s.RegisterHandler(id.FindServersRequest_Encoding_DefaultBinary, discovery.FindServers)
	s.RegisterHandler(id.FindServersOnNetworkRequest_Encoding_DefaultBinary, discovery.FindServersOnNetwork)
	s.RegisterHandler(id.GetEndpointsRequest_Encoding_DefaultBinary, discovery.GetEndpoints)
	s.RegisterHandler(id.RegisterServerRequest_Encoding_DefaultBinary, discovery.RegisterServer)
	s.RegisterHandler(id.RegisterServer2Request_Encoding_DefaultBinary, discovery.RegisterServer2)

	// SecureChannel service (handled in the uasc stack)
	// s.registerHandlerFunc(id.OpenSecureChannelRequest_Encoding_DefaultBinary, handleOpenSecureChannel)
	// s.registerHandlerFunc(id.CloseSecureChannelRequest_Encoding_DefaultBinary, handleCloseSecureChannel)

	session := &SessionService{s}
	s.RegisterHandler(id.CreateSessionRequest_Encoding_DefaultBinary, session.CreateSession)
	s.RegisterHandler(id.ActivateSessionRequest_Encoding_DefaultBinary, session.ActivateSession)
	s.RegisterHandler(id.CloseSessionRequest_Encoding_DefaultBinary, session.CloseSession)
	s.RegisterHandler(id.CancelRequest_Encoding_DefaultBinary, session.Cancel)

	node := &NodeManagementService{s}
	s.RegisterHandler(id.AddNodesRequest_Encoding_DefaultBinary, node.AddNodes)
	s.RegisterHandler(id.AddReferencesRequest_Encoding_DefaultBinary, node.AddReferences)
	s.RegisterHandler(id.DeleteNodesRequest_Encoding_DefaultBinary, node.DeleteNodes)
	s.RegisterHandler(id.DeleteReferencesRequest_Encoding_DefaultBinary, node.DeleteReferences)

	view := &ViewService{s}
	s.RegisterHandler(id.BrowseRequest_Encoding_DefaultBinary, view.Browse)
	s.RegisterHandler(id.BrowseNextRequest_Encoding_DefaultBinary, view.BrowseNext)
	s.RegisterHandler(id.TranslateBrowsePathsToNodeIDsRequest_Encoding_DefaultBinary, view.TranslateBrowsePathsToNodeIDs)
	s.RegisterHandler(id.RegisterNodesRequest_Encoding_DefaultBinary, view.RegisterNodes)
	s.RegisterHandler(id.UnregisterNodesRequest_Encoding_DefaultBinary, view.UnregisterNodes)

	query := &QueryService{s}
	s.RegisterHandler(id.QueryFirstRequest_Encoding_DefaultBinary, query.QueryFirst)
	s.RegisterHandler(id.QueryNextRequest_Encoding_DefaultBinary, query.QueryNext)

	attr := &AttributeService{s}
	s.RegisterHandler(id.ReadRequest_Encoding_DefaultBinary, attr.Read)
	s.RegisterHandler(id.HistoryReadRequest_Encoding_DefaultBinary, attr.HistoryRead)
	s.RegisterHandler(id.WriteRequest_Encoding_DefaultBinary, attr.Write)
	s.RegisterHandler(id.HistoryUpdateRequest_Encoding_DefaultBinary, attr.HistoryUpdate)

	method := &MethodService{s}
	// s.registerHandler(id.CallMethodRequest_Encoding_DefaultBinary, method.CallMethod) // todo(fs): I think this is bogus
	s.RegisterHandler(id.CallRequest_Encoding_DefaultBinary, method.Call)

	sub := &SubscriptionService{
		srv:  s,
		Subs: make(map[uint32]*Subscription),
	}
	s.SubscriptionService = sub
	s.RegisterHandler(id.CreateSubscriptionRequest_Encoding_DefaultBinary, sub.CreateSubscription)
	s.RegisterHandler(id.ModifySubscriptionRequest_Encoding_DefaultBinary, sub.ModifySubscription)
	s.RegisterHandler(id.SetPublishingModeRequest_Encoding_DefaultBinary, sub.SetPublishingMode)
	s.RegisterHandler(id.PublishRequest_Encoding_DefaultBinary, sub.Publish)
	s.RegisterHandler(id.RepublishRequest_Encoding_DefaultBinary, sub.Republish)
	s.RegisterHandler(id.TransferSubscriptionsRequest_Encoding_DefaultBinary, sub.TransferSubscriptions)
	s.RegisterHandler(id.DeleteSubscriptionsRequest_Encoding_DefaultBinary, sub.DeleteSubscriptions)

	item := &MonitoredItemService{
		SubService: sub,
		Items:      make(map[uint32]*MonitoredItem),
		Nodes:      make(map[string][]*MonitoredItem),
		Subs:       make(map[uint32][]*MonitoredItem),
	}
	s.MonitoredItemService = item
	// s.registerHandler(id.MonitoredItemCreateRequest_Encoding_DefaultBinary, item.MonitoredItemCreate)
	s.RegisterHandler(id.CreateMonitoredItemsRequest_Encoding_DefaultBinary, item.CreateMonitoredItems)
	//s.RegisterHandler(id.CreateMonitoredItemsRequest_Encoding_DefaultBinary, s.CreateMonitoredItems)
	// s.registerHandler(id.MonitoredItemModifyRequest_Encoding_DefaultBinary, item.MonitoredItemModify)
	s.RegisterHandler(id.ModifyMonitoredItemsRequest_Encoding_DefaultBinary, item.ModifyMonitoredItems)
	s.RegisterHandler(id.SetMonitoringModeRequest_Encoding_DefaultBinary, item.SetMonitoringMode)
	s.RegisterHandler(id.SetTriggeringRequest_Encoding_DefaultBinary, item.SetTriggering)
	s.RegisterHandler(id.DeleteMonitoredItemsRequest_Encoding_DefaultBinary, item.DeleteMonitoredItems)
}

// This function allows you to overwrite a handler before you call start.
func (s *Server) RegisterHandler(typeID uint16, h Handler) {
	_, ok := s.handlers[typeID]
	if !ok {
		s.handlers[typeID] = h
	}
}

func (s *Server) handleService(ctx context.Context, sc *uasc.SecureChannel, reqID uint32, req ua.Request) {
	// All requests of all secure channels are dispatched from one goroutine,
	// so a panic in a handler, or while its response is encoded, would stop
	// the server. Answer that request with Bad_InternalError instead.
	defer func() {
		if v := recover(); v != nil {
			s.answerServicePanic(ctx, sc, reqID, req, v)
		}
	}()

	if s.cfg.logger != nil {
		s.cfg.logger.Debug("handleService: Got: %T\n", req)
	}

	var resp ua.Response
	var err error

	typeID := ua.ServiceTypeID(req)
	h, ok := s.handlers[typeID]
	if ok {
		resp, err = h(sc, req, reqID)
	} else {
		if typeID == 0 {
			if s.cfg.logger != nil {
				s.cfg.logger.Warn("unknown service %T. Did you call register?", req)
			}
		}
		err = ua.StatusBadServiceUnsupported
	}

	if err != nil {
		if statusCode, ok := err.(ua.StatusCode); ok {
			resp = &ua.ServiceFault{ResponseHeader: responseHeader(0, statusCode)}
		} else {
			resp = &ua.ServiceFault{ResponseHeader: responseHeader(0, ua.StatusBadUnexpectedError)}
		}
	}

	if resp == nil {
		return
	}

	err = sc.SendResponseWithContext(ctx, reqID, resp)
	if err != nil {
		if s.cfg.logger != nil {
			s.cfg.logger.Warn("Error sending response: %s\n", err)
		}
	}
}

// answerServicePanic logs the panic v raised while handling req, with the
// stack, and answers req with internalErrorFault.
func (s *Server) answerServicePanic(ctx context.Context, sc *uasc.SecureChannel, reqID uint32, req ua.Request, v any) {
	defer func() {
		// Never let the recovery itself stop the dispatch loop.
		if v := recover(); v != nil && s.cfg.logger != nil {
			s.cfg.logger.Error("handleService: cannot answer %T after a panic: %v", req, v)
		}
	}()

	if s.cfg.logger != nil {
		s.cfg.logger.Error("handleService: panic while handling %T: %v\n%s", req, v, debug.Stack())
	}
	if err := sc.SendResponseWithContext(ctx, reqID, internalErrorFault(req)); err != nil && s.cfg.logger != nil {
		s.cfg.logger.Warn("Error sending response: %s\n", err)
	}
}

// internalErrorFault is the ServiceFault for a request that failed with a
// panic: Bad_InternalError, "an internal error occurred as a result of a
// programming or configuration error" (Part 4 §7.38.2 Table 178), with the
// requestHandle of the request (Part 4 §7.34).
func internalErrorFault(req ua.Request) *ua.ServiceFault {
	var handle uint32
	if req != nil {
		if hdr := req.Header(); hdr != nil {
			handle = hdr.RequestHandle
		}
	}
	return &ua.ServiceFault{ResponseHeader: responseHeader(handle, ua.StatusBadInternalError)}
}

func responseHeader(reqID uint32, statusCode ua.StatusCode) *ua.ResponseHeader {
	return &ua.ResponseHeader{
		Timestamp:          time.Now(),
		RequestHandle:      reqID,
		ServiceResult:      statusCode,
		ServiceDiagnostics: &ua.DiagnosticInfo{},
		StringTable:        []string{},
		AdditionalHeader:   ua.NewExtensionObject(nil),
	}
}

func serviceUnsupported(hdr *ua.RequestHeader) ua.Response {
	return &ua.ServiceFault{
		ResponseHeader: responseHeader(hdr.RequestHandle, ua.StatusBadServiceUnsupported),
	}
}

func safeReq[T ua.Request](r ua.Request) (T, error) {
	var t T
	req, ok := r.(T)
	if !ok {
		//debug.Printf("expected %T, got %T", t, r)
		return t, ua.StatusBadRequestTypeInvalid
	}
	return req, nil
}

// func handleServiceFault(s *Server, sc *uasc.SecureChannel, r ua.Request) (ua.Response, error) {
// 	debug.Printf("Handling %T", r)

// 	req, ok := r.(*ua.ServiceFault)
// 	if !ok {
// 		debug.Printf("handleServiceFault: Expected *ua.ServiceFault, got %T", r)
// 		return nil, ua.StatusBadRequestTypeInvalid
// 	}
// 	debug.Printf("Got ServiceFault: %s", req.ResponseHeader.ServiceResult)

// 	// No response required
// 	return nil, nil
// }
