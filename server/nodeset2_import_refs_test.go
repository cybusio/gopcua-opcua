package server

import (
	"encoding/xml"
	"fmt"
	"testing"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/schema"
	"github.com/gopcua/opcua/ua"
	"github.com/stretchr/testify/require"
)

func requireUniqueRefs(t *testing.T, n *Node) {
	t.Helper()
	seen := map[string]bool{}
	for _, r := range n.refs {
		key := fmt.Sprintf("%s|%t|%s", r.ReferenceTypeID, r.IsForward, r.NodeID.NodeID)
		require.False(t, seen[key], "node %s holds reference %s twice", n.ID(), key)
		seen[key] = true
	}
}

// TestImportedReferencesAreUnique checks that importing the standard
// nodeset, which declares most references on both of their nodes, leaves
// every node with each reference once (Part 3 4.3.4).
func TestImportedReferencesAreUnique(t *testing.T) {
	s := New()
	n0 := s.namespaces[0].(*NodeNameSpace)

	// Import the nodeset again so that every node of namespace 0 holds only
	// the references the import made: New adds nodes after its import.
	var nodes schema.UANodeSet
	require.NoError(t, xml.Unmarshal(schema.OpcUaNodeSet2, &nodes))
	require.NoError(t, s.ImportNodeSet(&nodes))
	for _, n := range n0.m {
		requireUniqueRefs(t, n)
	}

	// Server declares HasProperty to ServerArray, and ServerArray declares
	// the inverse HasProperty to Server: one reference on each end.
	browse := func(nodeID uint32, dir ua.BrowseDirection) []*ua.ReferenceDescription {
		res := n0.Browse(&ua.BrowseDescription{
			NodeID:          ua.NewNumericNodeID(0, nodeID),
			BrowseDirection: dir,
			ReferenceTypeID: ua.NewNumericNodeID(0, id.HasProperty),
			ResultMask:      uint32(ua.BrowseResultMaskAll),
		})
		require.Equal(t, ua.StatusGood, res.StatusCode)
		return res.References
	}
	inv := browse(id.Server_ServerArray, ua.BrowseDirectionInverse)
	require.Len(t, inv, 1)
	require.Equal(t, uint32(id.Server), inv[0].NodeID.NodeID.IntID())

	var toServerArray int
	for _, r := range browse(id.Server, ua.BrowseDirectionForward) {
		if r.NodeID.NodeID.IntID() == id.Server_ServerArray {
			toServerArray++
		}
	}
	require.Equal(t, 1, toServerArray)
}

// TestImportNodeSetStringNodeIDReferencesOnce imports a nodeset with string
// NodeIds whose reference is declared on both nodes, and checks that each
// node holds it once while distinct references stay.
func TestImportNodeSetStringNodeIDReferencesOnce(t *testing.T) {
	const doc = `<UANodeSet xmlns="http://opcfoundation.org/UA/2011/03/UANodeSet.xsd">
  <NamespaceUris><Uri>urn:test:refs</Uri></NamespaceUris>
  <Aliases>
    <Alias Alias="HasComponent">i=47</Alias>
    <Alias Alias="Organizes">i=35</Alias>
  </Aliases>
  <UAObject NodeId="ns=1;s=parent" BrowseName="1:parent">
    <DisplayName>parent</DisplayName>
    <References>
      <Reference ReferenceType="HasComponent">ns=1;s=child</Reference>
      <Reference ReferenceType="Organizes">ns=1;s=child</Reference>
    </References>
  </UAObject>
  <UAVariable NodeId="ns=1;s=child" BrowseName="1:child" DataType="i=6">
    <DisplayName>child</DisplayName>
    <References>
      <Reference ReferenceType="HasComponent" IsForward="false">ns=1;s=parent</Reference>
    </References>
  </UAVariable>
</UANodeSet>`
	var nodes schema.UANodeSet
	require.NoError(t, xml.Unmarshal([]byte(doc), &nodes))

	s := New()
	require.NoError(t, s.ImportNodeSet(&nodes))

	parent := s.Node(ua.NewStringNodeID(1, "parent"))
	child := s.Node(ua.NewStringNodeID(1, "child"))
	require.NotNil(t, parent)
	require.NotNil(t, child)
	requireUniqueRefs(t, parent)
	requireUniqueRefs(t, child)
	// HasComponent and Organizes forward on the parent, both inverse on the
	// child.
	require.Len(t, parent.refs, 2)
	require.Len(t, child.refs, 2)
}

// TestAddRefAppends checks that AddRef stays a plain append: deduplication
// is the importer's, so adding references does not cost more as a node
// grows.
func TestAddRefAppends(t *testing.T) {
	parent := NewFolderNode(ua.NewNumericNodeID(1, 1), "parent")
	child := NewVariableNode(ua.NewStringNodeID(1, "child"), "child", int32(1))
	base := len(parent.refs)
	parent.AddRef(child, id.HasComponent, true)
	parent.AddRef(child, id.HasComponent, true)
	require.Len(t, parent.refs, base+2)
}
