package server

import (
	"encoding/xml"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/schema"
	"github.com/gopcua/opcua/ua"
	"github.com/stretchr/testify/require"
)

// The Server Object variables whose value the server computes at run time.
var liveServerNodeIDs = []uint32{
	id.Server_NamespaceArray,
	id.Server_ServerStatus,
	id.Server_ServerStatus_StartTime,
	id.Server_ServerStatus_CurrentTime,
	id.Server_ServerStatus_State,
	id.Server_ServerStatus_BuildInfo,
	id.Server_ServerStatus_BuildInfo_ProductName,
	id.Server_ServerStatus_BuildInfo_ProductURI,
	id.Server_ServerStatus_BuildInfo_ManufacturerName,
	id.Server_ServerStatus_BuildInfo_SoftwareVersion,
	id.Server_ServerStatus_BuildInfo_BuildNumber,
	id.Server_ServerStatus_BuildInfo_BuildDate,
	id.Server_ServerStatus_SecondsTillShutdown,
	id.Server_ServerStatus_ShutdownReason,
	id.Server_ServerCapabilities_OperationLimits_MaxNodesPerRead,
}

// nodeSetNode is the part of a UANodeSet node these tests compare against.
// The optional Variable attributes are pointers so that an attribute the
// nodeset leaves out can be told apart from one set to the zero value.
type nodeSetNode struct {
	NodeID                  string   `xml:"NodeId,attr"`
	BrowseName              string   `xml:"BrowseName,attr"`
	DisplayName             []string `xml:"DisplayName"`
	DataType                *string  `xml:"DataType,attr"`
	ValueRank               *int32   `xml:"ValueRank,attr"`
	ArrayDimensions         *string  `xml:"ArrayDimensions,attr"`
	AccessLevel             *uint8   `xml:"AccessLevel,attr"`
	UserAccessLevel         *uint8   `xml:"UserAccessLevel,attr"`
	MinimumSamplingInterval *float64 `xml:"MinimumSamplingInterval,attr"`
	Historizing             *bool    `xml:"Historizing,attr"`
	References              []struct {
		ReferenceType string `xml:"ReferenceType,attr"`
		IsForward     *bool  `xml:"IsForward,attr"`
		Target        string `xml:",chardata"`
	} `xml:"References>Reference"`
}

type nodeSetFile struct {
	Aliases []struct {
		Alias string `xml:"Alias,attr"`
		Value string `xml:",chardata"`
	} `xml:"Aliases>Alias"`
	Objects        []nodeSetNode `xml:"UAObject"`
	Variables      []nodeSetNode `xml:"UAVariable"`
	Methods        []nodeSetNode `xml:"UAMethod"`
	ObjectTypes    []nodeSetNode `xml:"UAObjectType"`
	VariableTypes  []nodeSetNode `xml:"UAVariableType"`
	DataTypes      []nodeSetNode `xml:"UADataType"`
	ReferenceTypes []nodeSetNode `xml:"UAReferenceType"`
}

type refKeyT struct {
	refType uint32
	forward bool
	target  uint32
}

type standardNodeSet struct {
	aliases   map[string]string
	variables map[uint32]nodeSetNode
	// refs holds, per node, the references the nodeset declares on the
	// node and the inverses of those other nodes declare to it.
	refs map[uint32]map[refKeyT]bool
}

func loadStandardNodeSet(t *testing.T) *standardNodeSet {
	t.Helper()
	var f nodeSetFile
	require.NoError(t, xml.Unmarshal(schema.OpcUaNodeSet2, &f))

	ns := &standardNodeSet{
		aliases:   map[string]string{},
		variables: map[uint32]nodeSetNode{},
		refs:      map[uint32]map[refKeyT]bool{},
	}
	for _, a := range f.Aliases {
		ns.aliases[a.Alias] = strings.TrimSpace(a.Value)
	}
	for _, v := range f.Variables {
		ns.variables[ns.numeric(t, v.NodeID)] = v
	}
	add := func(node uint32, k refKeyT) {
		if ns.refs[node] == nil {
			ns.refs[node] = map[refKeyT]bool{}
		}
		ns.refs[node][k] = true
	}
	for _, list := range [][]nodeSetNode{f.Objects, f.Variables, f.Methods, f.ObjectTypes, f.VariableTypes, f.DataTypes, f.ReferenceTypes} {
		for _, n := range list {
			src := ns.numeric(t, n.NodeID)
			for _, r := range n.References {
				fwd := r.IsForward == nil || *r.IsForward
				rt := ns.numeric(t, r.ReferenceType)
				dst := ns.numeric(t, strings.TrimSpace(r.Target))
				add(src, refKeyT{rt, fwd, dst})
				add(dst, refKeyT{rt, !fwd, src})
			}
		}
	}
	return ns
}

// numeric resolves an alias or an "i=N" NodeId of namespace 0.
func (ns *standardNodeSet) numeric(t *testing.T, s string) uint32 {
	t.Helper()
	if v, ok := ns.aliases[s]; ok {
		s = v
	}
	nid, err := ua.ParseNodeID(s)
	require.NoError(t, err, s)
	require.Equal(t, uint16(0), nid.Namespace(), s)
	return nid.IntID()
}

func browseAll(t *testing.T, s *Server, nodeID uint32) []*ua.ReferenceDescription {
	t.Helper()
	res := s.namespaces[0].Browse(&ua.BrowseDescription{
		NodeID:          ua.NewNumericNodeID(0, nodeID),
		BrowseDirection: ua.BrowseDirectionBoth,
		ReferenceTypeID: ua.NewTwoByteNodeID(0),
		IncludeSubtypes: true,
		ResultMask:      uint32(ua.BrowseResultMaskAll),
	})
	require.Equal(t, ua.StatusGood, res.StatusCode)
	return res.References
}

func readAttr(s *Server, nodeID uint32, attr ua.AttributeID) *ua.DataValue {
	return s.namespaces[0].Attribute(ua.NewNumericNodeID(0, nodeID), attr)
}

// TestServerNodesKeepNodeSetAttributes checks that the Server Object
// variables with a run-time value answer every attribute of the Variable
// NodeClass (Part 3 5.6.2 Table 13) as the standard nodeset defines it, with
// the defaults of schema/UANodeSet.xsd where the nodeset leaves one out.
func TestServerNodesKeepNodeSetAttributes(t *testing.T) {
	s := New()
	std := loadStandardNodeSet(t)

	for _, nid := range liveServerNodeIDs {
		def, ok := std.variables[nid]
		require.True(t, ok, "i=%d is not a Variable of the nodeset", nid)

		t.Run(def.BrowseName, func(t *testing.T) {
			dv := readAttr(s, nid, ua.AttributeIDNodeClass)
			require.Equal(t, ua.StatusOK, dv.Status)
			require.Equal(t, int32(ua.NodeClassVariable), dv.Value.Value())

			dv = readAttr(s, nid, ua.AttributeIDBrowseName)
			require.Equal(t, ua.StatusOK, dv.Status)
			require.Equal(t, &ua.QualifiedName{NamespaceIndex: 0, Name: def.BrowseName}, dv.Value.Value())

			dv = readAttr(s, nid, ua.AttributeIDDisplayName)
			require.Equal(t, ua.StatusOK, dv.Status)
			require.Len(t, def.DisplayName, 1)
			require.Equal(t, def.DisplayName[0], dv.Value.Value().(*ua.LocalizedText).Text)

			require.NotNil(t, def.DataType)
			dv = readAttr(s, nid, ua.AttributeIDDataType)
			require.Equal(t, ua.StatusOK, dv.Status)
			require.Equal(t, std.numeric(t, *def.DataType), dv.Value.NodeID().IntID())
			require.Equal(t, uint16(0), dv.Value.NodeID().Namespace())

			wantRank := int32(-1)
			if def.ValueRank != nil {
				wantRank = *def.ValueRank
			}
			dv = readAttr(s, nid, ua.AttributeIDValueRank)
			require.Equal(t, ua.StatusOK, dv.Status)
			require.Equal(t, wantRank, dv.Value.Value())

			dv = readAttr(s, nid, ua.AttributeIDArrayDimensions)
			if def.ArrayDimensions == nil {
				require.Equal(t, ua.StatusBadAttributeIDInvalid, dv.Status)
			} else {
				var want []uint32
				for _, d := range strings.Split(*def.ArrayDimensions, ",") {
					n, err := strconv.ParseUint(strings.TrimSpace(d), 10, 32)
					require.NoError(t, err)
					want = append(want, uint32(n))
				}
				require.Equal(t, ua.StatusOK, dv.Status)
				require.Equal(t, want, dv.Value.Value())
			}

			wantAccess := uint8(ua.AccessLevelTypeCurrentRead)
			if def.AccessLevel != nil {
				wantAccess = *def.AccessLevel
			}
			dv = readAttr(s, nid, ua.AttributeIDAccessLevel)
			require.Equal(t, ua.StatusOK, dv.Status)
			require.Equal(t, wantAccess, dv.Value.Value())

			wantUserAccess := uint8(ua.AccessLevelTypeCurrentRead)
			if def.UserAccessLevel != nil {
				wantUserAccess = *def.UserAccessLevel
			}
			dv = readAttr(s, nid, ua.AttributeIDUserAccessLevel)
			require.Equal(t, ua.StatusOK, dv.Status)
			require.Equal(t, wantUserAccess, dv.Value.Value())

			wantHistorizing := false
			if def.Historizing != nil {
				wantHistorizing = *def.Historizing
			}
			dv = readAttr(s, nid, ua.AttributeIDHistorizing)
			require.Equal(t, ua.StatusOK, dv.Status)
			require.Equal(t, wantHistorizing, dv.Value.Value())

			wantInterval := 0.0
			if def.MinimumSamplingInterval != nil {
				wantInterval = *def.MinimumSamplingInterval
			}
			dv = readAttr(s, nid, ua.AttributeIDMinimumSamplingInterval)
			require.Equal(t, ua.StatusOK, dv.Status)
			require.Equal(t, wantInterval, dv.Value.Value())
		})
	}
}

// TestServerNodesKeepNodeSetReferences checks that binding the run-time
// values leaves each node with the references of the standard nodeset: no
// reference is lost, none is added, and the BuildInfo components stay below
// BuildInfo (Part 5 7.6, 7.7).
func TestServerNodesKeepNodeSetReferences(t *testing.T) {
	s := New()
	std := loadStandardNodeSet(t)

	for _, nid := range liveServerNodeIDs {
		t.Run(fmt.Sprintf("i=%d", nid), func(t *testing.T) {
			got := map[refKeyT]bool{}
			for _, r := range browseAll(t, s, nid) {
				require.Equal(t, uint16(0), r.NodeID.NodeID.Namespace())
				got[refKeyT{r.ReferenceTypeID.IntID(), r.IsForward, r.NodeID.NodeID.IntID()}] = true
			}
			require.Equal(t, std.refs[nid], got)
		})
	}

	children := func(nid uint32) []uint32 {
		var ids []uint32
		for _, r := range browseAll(t, s, nid) {
			if r.IsForward && r.ReferenceTypeID.IntID() == id.HasComponent && !slices.Contains(ids, r.NodeID.NodeID.IntID()) {
				ids = append(ids, r.NodeID.NodeID.IntID())
			}
		}
		return ids
	}
	buildInfoComponents := []uint32{
		id.Server_ServerStatus_BuildInfo_ProductName,
		id.Server_ServerStatus_BuildInfo_ProductURI,
		id.Server_ServerStatus_BuildInfo_ManufacturerName,
		id.Server_ServerStatus_BuildInfo_SoftwareVersion,
		id.Server_ServerStatus_BuildInfo_BuildNumber,
		id.Server_ServerStatus_BuildInfo_BuildDate,
	}
	status := children(id.Server_ServerStatus)
	require.Contains(t, status, uint32(id.Server_ServerStatus_BuildInfo))
	for _, c := range buildInfoComponents {
		require.NotContains(t, status, c)
	}
	require.ElementsMatch(t, buildInfoComponents, children(id.Server_ServerStatus_BuildInfo))
}

// TestServerNodesBrowseMatchesRead checks that the reference descriptions
// Browse returns for the children of Server and ServerStatus describe the
// nodes a Read of the children answers, and that they report the target's
// type definition (Part 4 7.29).
func TestServerNodesBrowseMatchesRead(t *testing.T) {
	s := New()
	std := loadStandardNodeSet(t)

	for _, parent := range []uint32{id.Server, id.Server_ServerStatus} {
		for _, r := range browseAll(t, s, parent) {
			if !r.IsForward || r.ReferenceTypeID.IntID() == id.HasTypeDefinition {
				continue
			}
			target := r.NodeID.NodeID.IntID()
			t.Run(fmt.Sprintf("i=%d/i=%d", parent, target), func(t *testing.T) {
				dv := readAttr(s, target, ua.AttributeIDNodeClass)
				require.Equal(t, ua.StatusOK, dv.Status)
				require.Equal(t, int32(r.NodeClass), dv.Value.Value())

				dv = readAttr(s, target, ua.AttributeIDBrowseName)
				require.Equal(t, ua.StatusOK, dv.Status)
				require.Equal(t, r.BrowseName, dv.Value.Value())

				dv = readAttr(s, target, ua.AttributeIDDisplayName)
				require.Equal(t, ua.StatusOK, dv.Status)
				require.Equal(t, r.DisplayName.Text, dv.Value.Value().(*ua.LocalizedText).Text)

				var wantTypeDef uint32
				for k := range std.refs[target] {
					if k.forward && k.refType == id.HasTypeDefinition {
						wantTypeDef = k.target
					}
				}
				if r.NodeClass == ua.NodeClassObject || r.NodeClass == ua.NodeClassVariable {
					require.Equal(t, wantTypeDef, r.TypeDefinition.NodeID.IntID())
				}
			})
		}
	}
}

// TestServerNodesValues checks that the bound values are live and of the
// DataType each node declares.
func TestServerNodesValues(t *testing.T) {
	s := New()
	NewNodeNameSpace(s, "urn:test:one")

	value := func(nid uint32) any {
		t.Helper()
		dv := readAttr(s, nid, ua.AttributeIDValue)
		require.Equal(t, ua.StatusOK, dv.Status, "i=%d", nid)
		require.NotNil(t, dv.Value, "i=%d", nid)
		return dv.Value.Value()
	}

	var want []string
	for _, n := range s.Namespaces() {
		want = append(want, n.Name())
	}
	require.Equal(t, want, value(id.Server_NamespaceArray))
	require.Equal(t, "urn:test:one", want[len(want)-1])

	status, ok := value(id.Server_ServerStatus).(*ua.ExtensionObject)
	require.True(t, ok)
	require.IsType(t, &ua.ServerStatusDataType{}, status.Value)

	info, ok := value(id.Server_ServerStatus_BuildInfo).(*ua.ExtensionObject)
	require.True(t, ok)
	buildInfo, ok := info.Value.(*ua.BuildInfo)
	require.True(t, ok)
	require.Equal(t, value(id.Server_ServerStatus_BuildInfo_ProductName), buildInfo.ProductName)
	require.Equal(t, value(id.Server_ServerStatus_BuildInfo_ProductURI), buildInfo.ProductURI)
	require.Equal(t, value(id.Server_ServerStatus_BuildInfo_ManufacturerName), buildInfo.ManufacturerName)
	require.Equal(t, value(id.Server_ServerStatus_BuildInfo_SoftwareVersion), buildInfo.SoftwareVersion)
	require.Equal(t, value(id.Server_ServerStatus_BuildInfo_BuildNumber), buildInfo.BuildNumber)
	require.Equal(t, value(id.Server_ServerStatus_BuildInfo_BuildDate), buildInfo.BuildDate)

	require.IsType(t, int32(0), value(id.Server_ServerStatus_State))
	require.Equal(t, int32(ua.ServerStateSuspended), value(id.Server_ServerStatus_State))
	require.IsType(t, uint32(0), value(id.Server_ServerStatus_SecondsTillShutdown))
	require.IsType(t, &ua.LocalizedText{}, value(id.Server_ServerStatus_ShutdownReason))
	require.IsType(t, uint32(0), value(id.Server_ServerCapabilities_OperationLimits_MaxNodesPerRead))

	start := value(id.Server_ServerStatus_StartTime)
	first := value(id.Server_ServerStatus_CurrentTime).(time.Time)
	time.Sleep(5 * time.Millisecond)
	require.True(t, value(id.Server_ServerStatus_CurrentTime).(time.Time).After(first))
	require.Equal(t, start, value(id.Server_ServerStatus_StartTime))
}

// TestServerNodesNotWritable checks that the AccessLevel the nodes now carry
// keeps a client from writing their values.
func TestServerNodesNotWritable(t *testing.T) {
	s := New()
	for _, nid := range liveServerNodeIDs {
		st := s.namespaces[0].SetAttribute(ua.NewNumericNodeID(0, nid), ua.AttributeIDValue, DataValueFromValue(int32(1)))
		require.Equal(t, ua.StatusBadUserAccessDenied, st, "i=%d", nid)
	}
	require.Len(t, readAttr(s, id.Server_NamespaceArray, ua.AttributeIDValue).Value.Value(), 1)
}

// TestTypeDefinitionPrefersHasTypeDefinition checks that a reference
// description reports the target's HasTypeDefinition target even when the
// target also has a DataType, and falls back to DataType for a target
// without one.
func TestTypeDefinitionPrefersHasTypeDefinition(t *testing.T) {
	typed := NewNode(ua.NewNumericNodeID(1, 1), Attributes{
		ua.AttributeIDDataType: DataValueFromValue(ua.NewNumericNodeID(0, id.Double)),
	}, []*ua.ReferenceDescription{{
		ReferenceTypeID: ua.NewNumericNodeID(0, id.HasTypeDefinition),
		IsForward:       true,
		NodeID:          ua.NewNumericExpandedNodeID(0, id.BaseDataVariableType),
	}}, nil)
	require.Equal(t, uint32(id.BaseDataVariableType), typed.typeDefinition().NodeID.IntID())

	untyped := NewNode(ua.NewNumericNodeID(1, 2), Attributes{
		ua.AttributeIDDataType: DataValueFromValue(ua.NewNumericExpandedNodeID(0, id.FolderType)),
	}, nil, nil)
	require.Equal(t, uint32(id.FolderType), untyped.typeDefinition().NodeID.IntID())

	var missing *Node
	require.Equal(t, uint32(0), missing.typeDefinition().NodeID.IntID())
}
