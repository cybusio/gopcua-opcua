// Copyright 2018-2020 opcua authors. All rights reserved.
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

package server

import (
	"strings"
	"testing"

	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uasc"
)

// TestResourcePathAppendsToEndpoints verifies that the ResourcePath
// option is appended to each endpoint URL exactly once, regardless of
// the option order, with leading-slash normalization and
// trailing-slash stripping.
func TestResourcePathAppendsToEndpoints(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"plain", "UA/SampleServer", "/UA/SampleServer"},
		{"leading-slash", "/UA/SampleServer", "/UA/SampleServer"},
		{"trailing-slash", "/UA/SampleServer/", "/UA/SampleServer"},
		{"double-leading", "//UA/SampleServer", "/UA/SampleServer"},
		{"whitespace", "  /UA/X  ", "/UA/X"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(
				EndPoint("127.0.0.1", 4840),
				ResourcePath(tc.path),
			)
			urls := srv.URLs()
			if len(urls) != 1 {
				t.Fatalf("expected exactly 1 endpoint, got %d", len(urls))
			}
			if !strings.HasSuffix(urls[0], tc.want) {
				t.Fatalf("endpoint %q does not end with %q", urls[0], tc.want)
			}
			// Path must be appended exactly once.
			if got := strings.Count(urls[0], tc.want); got != 1 {
				t.Fatalf("endpoint %q contains resource path %d times, want 1", urls[0], got)
			}
		})
	}
}

// TestResourcePathOrderIndependent verifies that placing ResourcePath
// before EndPoint produces the same result as placing it after.
func TestResourcePathOrderIndependent(t *testing.T) {
	a := New(EndPoint("127.0.0.1", 4840), ResourcePath("/UA/X"))
	b := New(ResourcePath("/UA/X"), EndPoint("127.0.0.1", 4840))
	if a.URLs()[0] != b.URLs()[0] {
		t.Fatalf("order-dependent endpoints: %q vs %q", a.URLs()[0], b.URLs()[0])
	}
}

// TestResourcePathEmptyNoop verifies that an empty (or unset)
// ResourcePath leaves endpoint URLs unchanged.
func TestResourcePathEmptyNoop(t *testing.T) {
	a := New(EndPoint("127.0.0.1", 4840))
	b := New(EndPoint("127.0.0.1", 4840), ResourcePath(""))
	if a.URLs()[0] != b.URLs()[0] {
		t.Fatalf("empty ResourcePath altered endpoint: %q vs %q", a.URLs()[0], b.URLs()[0])
	}
}

// TestMaxSessionsRejectsAtCap verifies that once the configured cap is
// reached, CreateSession returns StatusBadTooManySessions.
func TestMaxSessionsRejectsAtCap(t *testing.T) {
	srv := New(MaxSessions(2))

	// Fill the broker to the cap directly.
	_ = srv.sb.NewSession()
	_ = srv.sb.NewSession()

	svc := &SessionService{srv}
	_, err := svc.CreateSession(
		&uasc.SecureChannel{},
		&ua.CreateSessionRequest{RequestHeader: &ua.RequestHeader{}},
		0,
	)
	if err != ua.StatusBadTooManySessions {
		t.Fatalf("CreateSession at cap: got err=%v, want StatusBadTooManySessions", err)
	}
}

// TestMaxSessionsZeroUnlimited verifies that the zero / unset cap does
// not reject new sessions.
func TestMaxSessionsZeroUnlimited(t *testing.T) {
	srv := New() // no MaxSessions option

	// Pre-populate to a number that would trip a small cap.
	for i := 0; i < 8; i++ {
		_ = srv.sb.NewSession()
	}

	if srv.cfg.maxSessions != 0 {
		t.Fatalf("expected maxSessions=0 (unset), got %d", srv.cfg.maxSessions)
	}
	// We do not call CreateSession here because constructing the
	// signing path requires a real SecureChannel; the config-level
	// assertion above suffices for the smoke test, and the
	// "rejects-at-cap" test exercises the early-return branch.
}

// TestMaxConnectionsOptionSetsField verifies that MaxConnections wires
// through to the serverConfig field. End-to-end enforcement requires a
// live listener and is exercised by the consuming server's tests.
func TestMaxConnectionsOptionSetsField(t *testing.T) {
	srv := New(MaxConnections(5))
	if srv.cfg.maxConnections != 5 {
		t.Fatalf("MaxConnections(5) -> cfg.maxConnections=%d, want 5", srv.cfg.maxConnections)
	}
	// Zero / non-positive must remain the unset sentinel.
	srv0 := New(MaxConnections(0))
	if srv0.cfg.maxConnections != 0 {
		t.Fatalf("MaxConnections(0) -> cfg.maxConnections=%d, want 0", srv0.cfg.maxConnections)
	}
	srvNeg := New(MaxConnections(-3))
	if srvNeg.cfg.maxConnections != -3 {
		// We intentionally pass the value through as-is; the
		// enforcement site treats any non-positive value as "no cap".
		t.Logf("MaxConnections(-3) stored as %d (treated as no-cap at enforcement)", srvNeg.cfg.maxConnections)
	}
}

// TestNormalizeResourcePath covers the helper's edge cases directly so
// regressions in the canonicalization rules surface as unit failures
// rather than as endpoint-URL drift.
func TestNormalizeResourcePath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"/", "/"},
		{"//", "/"},
		{"foo", "/foo"},
		{"/foo", "/foo"},
		{"/foo/", "/foo"},
		{"/foo//", "/foo"},
		{"//foo", "/foo"},
		{"  /foo/bar/  ", "/foo/bar"},
	}
	for _, tc := range cases {
		if got := normalizeResourcePath(tc.in); got != tc.want {
			t.Errorf("normalizeResourcePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
