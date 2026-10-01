package server

import (
	"time"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/server/attrs"
	"github.com/gopcua/opcua/ua"
)

// serverVariableAttributes returns the attributes of one of the namespace 0
// Server Object variables whose value the server computes at run time: the
// Variable attributes of Part 3 5.6.2 Table 13 as the standard nodeset
// defines them for that node (Part 5 6.3.1 ServerType, 6.3.11
// OperationLimitsType, 7.6 ServerStatusType, 7.7 BuildInfoType). Values the
// nodeset leaves out take the defaults of the UANodeSet schema
// (schema/UANodeSet.xsd): AccessLevel and UserAccessLevel CurrentRead,
// Historizing false. arrayDims is nil for a node without ArrayDimensions.
func serverVariableAttributes(name string, dataType uint32, valueRank int32, arrayDims []uint32, minSamplingInterval float64) Attributes {
	a := Attributes{
		ua.AttributeIDNodeClass:               DataValueFromValue(uint32(ua.NodeClassVariable)),
		ua.AttributeIDBrowseName:              DataValueFromValue(attrs.BrowseName(name)),
		ua.AttributeIDDisplayName:             DataValueFromValue(attrs.DisplayName(name, "")),
		ua.AttributeIDDataType:                DataValueFromValue(ua.NewNumericNodeID(0, dataType)),
		ua.AttributeIDValueRank:               DataValueFromValue(valueRank),
		ua.AttributeIDAccessLevel:             DataValueFromValue(uint8(ua.AccessLevelTypeCurrentRead)),
		ua.AttributeIDUserAccessLevel:         DataValueFromValue(uint8(ua.AccessLevelTypeCurrentRead)),
		ua.AttributeIDHistorizing:             DataValueFromValue(false),
		ua.AttributeIDMinimumSamplingInterval: DataValueFromValue(minSamplingInterval),
	}
	if arrayDims != nil {
		a[ua.AttributeIDArrayDimensions] = DataValueFromValue(arrayDims)
	}
	return a
}

func serverVariable(nodeID uint32, name string, dataType uint32, valueRank int32, arrayDims []uint32, minSamplingInterval float64, val ValueFunc) *Node {
	return NewNode(ua.NewNumericNodeID(0, nodeID), serverVariableAttributes(name, dataType, valueRank, arrayDims, minSamplingInterval), nil, val)
}

// CurrentTimeNode returns the Server.ServerStatus.CurrentTime variable.
func CurrentTimeNode() *Node {
	return serverVariable(id.Server_ServerStatus_CurrentTime, "CurrentTime", id.UtcTime, -1, nil, 0,
		func() *ua.DataValue { return DataValueFromValue(time.Now()) })
}

// NamespacesNode returns the Server.NamespaceArray property, whose value
// lists the URIs of the server's namespaces in index order.
func NamespacesNode(s *Server) *Node {
	return serverVariable(id.Server_NamespaceArray, "NamespaceArray", id.String, 1, []uint32{0}, 1000,
		func() *ua.DataValue {
			n := s.Namespaces()
			ns := make([]string, len(n))
			for i := range ns {
				ns[i] = n[i].Name()
			}
			return DataValueFromValue(ns)
		})
}

func ServerCapabilitiesNodes(s *Server) []*Node {
	var nodes []*Node
	nodes = append(nodes, NewNode(
		ua.NewNumericNodeID(0, id.Server_ServerCapabilities_OperationLimits_MaxNodesPerRead),
		serverVariableAttributes("MaxNodesPerRead", id.UInt32, -1, nil, 0),
		nil,
		func() *ua.DataValue { return DataValueFromValue(s.cfg.cap.OperationalLimits.MaxNodesPerRead) },
	))
	return nodes
}

func RootNode() *Node {
	return NewNode(
		ua.NewNumericNodeID(0, id.RootFolder),
		map[ua.AttributeID]*ua.DataValue{
			ua.AttributeIDNodeClass:  DataValueFromValue(attrs.NodeClass(ua.NodeClassObject)),
			ua.AttributeIDBrowseName: DataValueFromValue(attrs.BrowseName("Root")),
			ua.AttributeIDDataType:   DataValueFromValue(ua.NewNumericExpandedNodeID(0, id.DataTypesFolder)),
		},
		nil,
		nil,
	)
}

// ServerStatusNodes returns the Server.ServerStatus variable and its
// components (Part 5 7.6 ServerStatusType, 7.7 BuildInfoType).
//
// The returned nodes carry no references: New binds their values onto the
// nodes with the same NodeIds that the standard nodeset defines, and those
// hold the references. serverNode is not used.
func ServerStatusNodes(s *Server, serverNode *Node) []*Node {

	/*
		Server_ServerArray                                                                                                                                                    = 2254
		Server_NamespaceArray                                                                                                                                                 = 2255
		Server_ServerStatus_BuildInfo                                                                                                                                         = 2260
		Server_ServerStatus_BuildInfo_ProductName                                                                                                                             = 2261
		Server_ServerStatus_BuildInfo_ProductURI                                                                                                                              = 2262
		Server_ServerStatus_BuildInfo_ManufacturerName                                                                                                                        = 2263
		Server_ServerStatus_BuildInfo_SoftwareVersion                                                                                                                         = 2264
		Server_ServerStatus_BuildInfo_BuildNumber                                                                                                                             = 2265
		Server_ServerStatus_BuildInfo_BuildDate                                                                                                                               = 2266
		Server_ServiceLevel                                                                                                                                                   = 2267
		Server_ServerCapabilities                                                                                                                                             = 2268
		Server_ServerCapabilities_ServerProfileArray                                                                                                                          = 2269
		Server_ServerCapabilities_LocaleIDArray                                                                                                                               = 2271
		Server_ServerCapabilities_MinSupportedSampleRate                                                                                                                      = 2272
		Server_ServerDiagnostics                                                                                                                                              = 2274
		Server_ServerDiagnostics_ServerDiagnosticsSummary                                                                                                                     = 2275
		Server_ServerDiagnostics_ServerDiagnosticsSummary_ServerViewCount                                                                                                     = 2276
		Server_ServerDiagnostics_ServerDiagnosticsSummary_CurrentSessionCount                                                                                                 = 2277
		Server_ServerDiagnostics_ServerDiagnosticsSummary_CumulatedSessionCount                                                                                               = 2278
		Server_ServerDiagnostics_ServerDiagnosticsSummary_SecurityRejectedSessionCount                                                                                        = 2279
		Server_ServerDiagnostics_ServerDiagnosticsSummary_SessionTimeoutCount                                                                                                 = 2281
		Server_ServerDiagnostics_ServerDiagnosticsSummary_SessionAbortCount                                                                                                   = 2282
		Server_ServerDiagnostics_ServerDiagnosticsSummary_PublishingIntervalCount                                                                                             = 2284
		Server_ServerDiagnostics_ServerDiagnosticsSummary_CurrentSubscriptionCount                                                                                            = 2285
		Server_ServerDiagnostics_ServerDiagnosticsSummary_CumulatedSubscriptionCount                                                                                          = 2286
		Server_ServerDiagnostics_ServerDiagnosticsSummary_SecurityRejectedRequestsCount                                                                                       = 2287
		Server_ServerDiagnostics_ServerDiagnosticsSummary_RejectedRequestsCount                                                                                               = 2288
		Server_ServerDiagnostics_SamplingIntervalDiagnosticsArray                                                                                                             = 2289
		Server_ServerDiagnostics_SubscriptionDiagnosticsArray                                                                                                                 = 2290
		Server_ServerDiagnostics_EnabledFlag                                                                                                                                  = 2294
		Server_VendorServerInfo                                                                                                                                               = 2295
		Server_ServerRedundancy                                                                                                                                               = 2296
	*/

	sStatus := serverVariable(id.Server_ServerStatus, "ServerStatus", id.ServerStatusDataType, -1, nil, 1000,
		func() *ua.DataValue { return DataValueFromValue(ua.NewExtensionObject(s.Status())) })

	sState := serverVariable(id.Server_ServerStatus_State, "State", id.ServerState, -1, nil, 0,
		func() *ua.DataValue { return DataValueFromValue(int32(s.Status().State)) })

	mName := serverVariable(id.Server_ServerStatus_BuildInfo_ManufacturerName, "ManufacturerName", id.String, -1, nil, 1000,
		func() *ua.DataValue { return DataValueFromValue(s.cfg.manufacturerName) })

	pName := serverVariable(id.Server_ServerStatus_BuildInfo_ProductName, "ProductName", id.String, -1, nil, 1000,
		func() *ua.DataValue { return DataValueFromValue(s.cfg.productName) })

	pURI := serverVariable(id.Server_ServerStatus_BuildInfo_ProductURI, "ProductUri", id.String, -1, nil, 1000,
		func() *ua.DataValue { return DataValueFromValue(s.cfg.applicationURI) })

	sVersion := serverVariable(id.Server_ServerStatus_BuildInfo_SoftwareVersion, "SoftwareVersion", id.String, -1, nil, 1000,
		func() *ua.DataValue { return DataValueFromValue(s.cfg.softwareVersion) })

	bNumber := serverVariable(id.Server_ServerStatus_BuildInfo_BuildNumber, "BuildNumber", id.String, -1, nil, 1000,
		func() *ua.DataValue { return DataValueFromValue(s.cfg.softwareVersion) })

	ts := time.Now()
	bDate := serverVariable(id.Server_ServerStatus_BuildInfo_BuildDate, "BuildDate", id.UtcTime, -1, nil, 1000,
		func() *ua.DataValue { return DataValueFromValue(ts) })

	// The BuildInfo value is the BuildInfo structure (Part 5 12.4) whose
	// fields the components above expose.
	bInfo := serverVariable(id.Server_ServerStatus_BuildInfo, "BuildInfo", id.BuildInfo, -1, nil, 0,
		func() *ua.DataValue {
			return DataValueFromValue(ua.NewExtensionObject(&ua.BuildInfo{
				ProductURI:       s.cfg.applicationURI,
				ManufacturerName: s.cfg.manufacturerName,
				ProductName:      s.cfg.productName,
				SoftwareVersion:  s.cfg.softwareVersion,
				BuildNumber:      s.cfg.softwareVersion,
				BuildDate:        ts,
			}))
		})

	timeStart := serverVariable(id.Server_ServerStatus_StartTime, "StartTime", id.UtcTime, -1, nil, 0,
		func() *ua.DataValue { return DataValueFromValue(ts) })

	timeCurrent := CurrentTimeNode()

	sTillShutdown := serverVariable(id.Server_ServerStatus_SecondsTillShutdown, "SecondsTillShutdown", id.UInt32, -1, nil, 0,
		func() *ua.DataValue { return DataValueFromValue(s.Status().SecondsTillShutdown) })

	sReason := serverVariable(id.Server_ServerStatus_ShutdownReason, "ShutdownReason", id.LocalizedText, -1, nil, 0,
		func() *ua.DataValue {
			if r := s.Status().ShutdownReason; r != nil {
				return DataValueFromValue(r)
			}
			return DataValueFromValue(&ua.LocalizedText{})
		})

	return []*Node{sState, mName, pName, pURI, sVersion, bNumber, bDate, timeStart, timeCurrent, bInfo, sTillShutdown, sReason, sStatus}
}

// addServerNode serves the value of n on the namespace 0 node with n's
// NodeId. When the standard nodeset defines that node, the imported node
// keeps its NodeClass, attributes, and references, and takes n's value and
// the attributes of n it does not have. Replacing it instead would drop the
// nodeset's references and leave the references of its neighbours
// describing a node that is no longer there.
func (s *Server) addServerNode(n *Node) {
	ns := s.namespaces[0]
	existing := ns.Node(n.ID())
	if existing == nil {
		ns.AddNode(n)
		return
	}
	for id, v := range n.attr {
		if _, ok := existing.attr[id]; !ok {
			existing.attr[id] = v
		}
	}
	existing.val = n.val
}
