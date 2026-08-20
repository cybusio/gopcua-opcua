package server

import (
	"testing"

	"github.com/gopcua/opcua/ua"
	"github.com/stretchr/testify/require"
)

// userTokenPolicies returns the token policies advertised for the endpoint
// with the given security policy URI, or nil when no such endpoint exists.
func userTokenPolicies(eps []*ua.EndpointDescription, secPolicy string) []*ua.UserTokenPolicy {
	for _, ep := range eps {
		if ep.SecurityPolicyURI == secPolicy {
			return ep.UserIdentityTokens
		}
	}
	return nil
}

func hasTokenType(policies []*ua.UserTokenPolicy, t ua.UserTokenType, secPolicy string) bool {
	for _, p := range policies {
		if p.TokenType == t && p.SecurityPolicyURI == secPolicy {
			return true
		}
	}
	return false
}

// TestUserNameTokenPolicyOnNoneOnlyServer checks which SecurityPolicy a
// non-Anonymous UserTokenPolicy is advertised under. Part 4 §7.36.4 protects
// the token's secret with the policy named in its UserTokenPolicy, not with
// the channel's, so a SecurityPolicy#None variant must be withheld when a
// protected alternative exists — and must be offered when it is the only
// policy configured, since Part 4 §5.6.3 has the client select from this
// list and an empty list leaves UserName authentication unreachable.
func TestUserNameTokenPolicyOnNoneOnlyServer(t *testing.T) {
	const none = ua.SecurityPolicyURINone
	const basic256sha256 = "http://opcfoundation.org/UA/SecurityPolicy#Basic256Sha256"

	for _, tc := range []struct {
		name         string
		opts         []Option
		wantOnNone   []string // policy URIs expected for the UserName token on the None endpoint
		unwantOnNone []string
	}{
		{
			name: "none is the only configured policy: offer it",
			opts: []Option{
				EnableSecurity("None", ua.MessageSecurityModeNone),
				EnableAuthMode(ua.UserTokenTypeUserName),
			},
			wantOnNone: []string{none},
		},
		{
			name: "a protected policy exists: withhold the None variant",
			opts: []Option{
				EnableSecurity("None", ua.MessageSecurityModeNone),
				EnableSecurity("Basic256Sha256", ua.MessageSecurityModeSignAndEncrypt),
				EnableAuthMode(ua.UserTokenTypeUserName),
			},
			wantOnNone:   []string{basic256sha256},
			unwantOnNone: []string{none},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(append(tc.opts, EndPoint("localhost", 4840))...)
			s.initEndpoints()
			eps := s.Endpoints()
			require.NotEmpty(t, eps)

			policies := userTokenPolicies(eps, none)
			require.NotEmpty(t, policies, "None endpoint advertised no UserTokenPolicy")

			for _, want := range tc.wantOnNone {
				require.True(t, hasTokenType(policies, ua.UserTokenTypeUserName, want),
					"expected a UserName policy under %q, got %+v", want, policies)
			}
			for _, unwant := range tc.unwantOnNone {
				require.False(t, hasTokenType(policies, ua.UserTokenTypeUserName, unwant),
					"UserName policy under %q must be withheld when a protected policy exists", unwant)
			}
		})
	}
}

// TestAnonymousTokenPolicyUnaffected checks the Anonymous token keeps its
// SecurityPolicy#None policy in both configurations: it carries no secret,
// so §7.36.4 does not apply to it.
func TestAnonymousTokenPolicyUnaffected(t *testing.T) {
	s := New(
		EnableSecurity("None", ua.MessageSecurityModeNone),
		EnableSecurity("Basic256Sha256", ua.MessageSecurityModeSignAndEncrypt),
		EnableAuthMode(ua.UserTokenTypeAnonymous),
		EndPoint("localhost", 4840),
	)
	s.initEndpoints()

	policies := userTokenPolicies(s.Endpoints(), ua.SecurityPolicyURINone)
	require.True(t, hasTokenType(policies, ua.UserTokenTypeAnonymous, ua.SecurityPolicyURINone),
		"Anonymous policy missing, got %+v", policies)
}
