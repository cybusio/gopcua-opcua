package server

import (
	"sync"
	"testing"

	"github.com/gopcua/opcua/ua"
)

// TestReadNonValueAttribute_AccessLevelValueOnly verifies that a Variable whose
// AccessLevel denies CurrentRead still exposes its non-Value attributes
// (BrowseName, NodeClass, DisplayName, …) on Read, while the Value attribute
// itself remains access-gated.
//
// OPC UA Part 3 §5.6.2 defines AccessLevel/UserAccessLevel as Variable-class
// attributes describing how the *Value* attribute may be accessed. Access to
// the other attributes is not governed by AccessLevel; a Read of them returns
// its own operation-level StatusCode per the Read Service (Part 4 §5.10.2).
// Gating every attribute on CurrentRead therefore violates the spec and makes a
// read-protected node un-browsable (a client cannot even resolve its
// BrowseName or NodeClass).
//
// This is the read-side counterpart to the non-Value *write* behavior already
// covered by tests/go/write_nonvalue_attribute_test.go, whose documentation
// states: "AccessLevel governs the Value attribute only."
func TestReadNonValueAttribute_AccessLevelValueOnly(t *testing.T) {
	srv := New()
	srv.initHandlers()
	ns := NewNodeNameSpace(srv, "urn:test:accesslevel")

	nodeID := ua.NewStringNodeID(ns.ID(), "no-read")
	n := NewVariableNode(nodeID, "NoRead", func() *ua.DataValue {
		return DataValueFromValue(int32(42))
	})
	// AccessLevel = CurrentWrite only. CurrentRead is not set, so
	// Node.Access(CurrentRead) is false. Stored as uint8 to match the type
	// Node.Access asserts when it reads the attribute back.
	n.SetAttribute(ua.AttributeIDAccessLevel, DataValueFromValue(uint8(ua.AccessLevelTypeCurrentWrite)))
	ns.AddNode(n)

	// Non-Value attributes must remain readable regardless of AccessLevel.
	for _, attr := range []struct {
		name string
		id   ua.AttributeID
	}{
		{"BrowseName", ua.AttributeIDBrowseName},
		{"NodeClass", ua.AttributeIDNodeClass},
		{"DisplayName", ua.AttributeIDDisplayName},
	} {
		dv := ns.Attribute(nodeID, attr.id)
		if dv.Status == ua.StatusBadUserAccessDenied {
			t.Errorf("read of non-Value attribute %s returned Bad_UserAccessDenied; "+
				"AccessLevel must gate the Value attribute only", attr.name)
		}
	}

	// The Value attribute stays access-gated.
	if dv := ns.Attribute(nodeID, ua.AttributeIDValue); dv.Status != ua.StatusBadUserAccessDenied {
		t.Errorf("read of Value attribute returned status %v; want Bad_UserAccessDenied", dv.Status)
	}
}

// TestAttributeNodeClassLeavesNodeUnchanged verifies that reading the
// NodeClass attribute returns it as an Int32 without rewriting the value
// stored on the Node.
//
// OPC UA Part 3 §8.29 defines NodeClass as an Enumeration, which Part 6 §5.2.4
// encodes as an Int32. The Node constructors store it as a UInt32, so Attribute
// converts it on the way out. Node.Attribute hands out the stored
// *ua.DataValue, so the conversion must work on a copy: otherwise the first
// read changes the type Node.Attribute reports, and reads that run at the same
// time race on the stored value.
func TestAttributeNodeClassLeavesNodeUnchanged(t *testing.T) {
	srv := New()
	ns := NewNodeNameSpace(srv, "urn:test:nodeclass")

	nodeID := ua.NewStringNodeID(ns.ID(), "var")
	n := NewVariableNode(nodeID, "Var", int32(42))
	ns.AddNode(n)

	stored, err := n.Attribute(ua.AttributeIDNodeClass)
	if err != nil {
		t.Fatalf("Node.Attribute(NodeClass): %v", err)
	}
	want := stored.Value.Value.Value()

	dv := ns.Attribute(nodeID, ua.AttributeIDNodeClass)
	if got := dv.Value.Value(); got != int32(ua.NodeClassVariable) {
		t.Fatalf("read NodeClass = %#v, want %#v", got, int32(ua.NodeClassVariable))
	}

	after, err := n.Attribute(ua.AttributeIDNodeClass)
	if err != nil {
		t.Fatalf("Node.Attribute(NodeClass): %v", err)
	}
	if got := after.Value.Value.Value(); got != want {
		t.Errorf("stored NodeClass after a read = %v (%T), want %v (%T)", got, got, want, want)
	}
	if after.Value == dv {
		t.Errorf("Attribute returned the stored DataValue")
	}
}

// TestAttributeNodeClassConcurrentReads reads the NodeClass attribute of one
// Node from several goroutines. Run with -race.
func TestAttributeNodeClassConcurrentReads(t *testing.T) {
	srv := New()
	ns := NewNodeNameSpace(srv, "urn:test:nodeclass")

	nodeID := ua.NewStringNodeID(ns.ID(), "var")
	ns.AddNode(NewVariableNode(nodeID, "Var", int32(42)))

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				dv := ns.Attribute(nodeID, ua.AttributeIDNodeClass)
				if got := dv.Value.Value(); got != int32(ua.NodeClassVariable) {
					t.Errorf("NodeClass = %#v, want %#v", got, int32(ua.NodeClassVariable))
					return
				}
			}
		}()
	}
	wg.Wait()
}
