package server

import (
	"testing"

	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uacp"
	"github.com/gopcua/opcua/uasc"
	"github.com/stretchr/testify/require"
)

// noneChannel returns a SecureChannel with SecurityPolicy#None, for which
// CreateSession issues no server signature.
func noneChannel(t *testing.T) *uasc.SecureChannel {
	t.Helper()
	sc, err := uasc.NewSecureChannel("", &uacp.Conn{}, &uasc.Config{
		SecurityPolicyURI: ua.SecurityPolicyURINone,
		SecurityMode:      ua.MessageSecurityModeNone,
	}, make(chan error))
	require.NoError(t, err)
	return sc
}

// endpointTestServer builds a server with a None and a Basic256Sha256
// endpoint, Anonymous and UserName authentication, and its service handlers
// and endpoint descriptions initialized as Start would.
func endpointTestServer(t *testing.T, register func(s *Server)) *Server {
	t.Helper()
	s := New(
		EndPoint("localhost", 4840),
		EnableSecurity("None", ua.MessageSecurityModeNone),
		EnableSecurity("Basic256Sha256", ua.MessageSecurityModeSignAndEncrypt),
		EnableAuthMode(ua.UserTokenTypeAnonymous),
		EnableAuthMode(ua.UserTokenTypeUserName),
	)
	if register != nil {
		register(s)
	}
	s.initHandlers()
	s.initEndpoints()
	return s
}

func callGetEndpoints(t *testing.T, s *Server, url string) []*ua.EndpointDescription {
	t.Helper()
	resp, err := s.handlers[id.GetEndpointsRequest_Encoding_DefaultBinary](noneChannel(t), &ua.GetEndpointsRequest{
		RequestHeader: &ua.RequestHeader{RequestHandle: 1},
		EndpointURL:   url,
	}, 1)
	require.NoError(t, err)
	return resp.(*ua.GetEndpointsResponse).Endpoints
}

func callCreateSession(t *testing.T, s *Server, url string, handle uint32) (ua.Response, error) {
	t.Helper()
	return s.handlers[id.CreateSessionRequest_Encoding_DefaultBinary](noneChannel(t), &ua.CreateSessionRequest{
		RequestHeader:           &ua.RequestHeader{RequestHandle: handle},
		EndpointURL:             url,
		RequestedSessionTimeout: 60000,
	}, handle)
}

func sessionCount(s *Server) int {
	s.sb.mu.Lock()
	defer s.sb.mu.Unlock()
	return len(s.sb.s)
}

// TestCreateSessionServerEndpointsMatchGetEndpoints checks that the
// serverEndpoints of a CreateSession response equal the endpoints a
// GetEndpoints request with the same EndpointUrl returns. Part 4 v1.05.07
// §5.7.2.2 has the Client verify the one list against the other.
func TestCreateSessionServerEndpointsMatchGetEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name      string
		url       string
		wantCount int
	}{
		{name: "registered url", url: "opc.tcp://localhost:4840", wantCount: 2},
		{name: "host in other case", url: "opc.tcp://LOCALHOST:4840", wantCount: 2},
		{name: "trailing slash", url: "opc.tcp://localhost:4840/", wantCount: 2},
		{name: "unregistered url", url: "opc.tcp://otherhost:4840", wantCount: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := endpointTestServer(t, nil)

			want := callGetEndpoints(t, s, tc.url)
			require.Len(t, want, tc.wantCount)

			resp, err := callCreateSession(t, s, tc.url, 2)
			require.NoError(t, err)
			got := resp.(*ua.CreateSessionResponse).ServerEndpoints
			require.Equal(t, want, got)
		})
	}
}

// TestCreateSessionServerEndpointsFromReplacedGetEndpoints checks that a
// GetEndpoints handler installed with RegisterHandler also answers for the
// serverEndpoints of CreateSession, so an application that customizes its
// endpoint descriptions does not have to keep a second copy in sync.
func TestCreateSessionServerEndpointsFromReplacedGetEndpoints(t *testing.T) {
	const url = "opc.tcp://gateway.example:48400/path"

	var gotReq *ua.GetEndpointsRequest
	var gotHandle uint32
	custom := func(sc *uasc.SecureChannel, r ua.Request, reqID uint32) (ua.Response, error) {
		req := r.(*ua.GetEndpointsRequest)
		gotReq = req
		gotHandle = req.RequestHeader.RequestHandle
		// A handler that writes to the header must not change the
		// CreateSession response.
		req.RequestHeader.RequestHandle = 99
		return &ua.GetEndpointsResponse{
			ResponseHeader: responseHeader(gotHandle, ua.StatusOK),
			Endpoints: []*ua.EndpointDescription{{
				EndpointURL:       req.EndpointURL,
				SecurityMode:      ua.MessageSecurityModeNone,
				SecurityPolicyURI: ua.SecurityPolicyURINone,
				UserIdentityTokens: []*ua.UserTokenPolicy{{
					PolicyID:          "username",
					TokenType:         ua.UserTokenTypeUserName,
					SecurityPolicyURI: "http://opcfoundation.org/UA/SecurityPolicy#Basic256Sha256",
				}},
			}},
		}, nil
	}
	s := endpointTestServer(t, func(s *Server) {
		s.RegisterHandler(id.GetEndpointsRequest_Encoding_DefaultBinary, custom)
	})

	want := callGetEndpoints(t, s, url)
	require.Len(t, want, 1)

	resp, err := callCreateSession(t, s, url, 7)
	require.NoError(t, err)
	cs := resp.(*ua.CreateSessionResponse)
	require.Equal(t, want, cs.ServerEndpoints)
	require.Equal(t, uint32(7), cs.ResponseHeader.RequestHandle)

	require.NotNil(t, gotReq)
	require.Equal(t, url, gotReq.EndpointURL)
	require.Equal(t, uint32(7), gotHandle)
}

// TestCreateSessionGetEndpointsFailure checks that CreateSession fails, and
// leaves no Session behind, when the endpoint descriptions cannot be
// obtained.
func TestCreateSessionGetEndpointsFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler Handler
		wantErr error
	}{
		{
			name: "handler error",
			handler: func(*uasc.SecureChannel, ua.Request, uint32) (ua.Response, error) {
				return nil, ua.StatusBadTooManyOperations
			},
			wantErr: ua.StatusBadTooManyOperations,
		},
		{
			name: "wrong response type",
			handler: func(_ *uasc.SecureChannel, r ua.Request, _ uint32) (ua.Response, error) {
				return &ua.ServiceFault{ResponseHeader: responseHeader(0, ua.StatusBadInternalError)}, nil
			},
			wantErr: ua.StatusBadInternalError,
		},
		{
			name: "bad service result",
			handler: func(_ *uasc.SecureChannel, r ua.Request, _ uint32) (ua.Response, error) {
				return &ua.GetEndpointsResponse{
					ResponseHeader: responseHeader(0, ua.StatusBadResourceUnavailable),
				}, nil
			},
			wantErr: ua.StatusBadResourceUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := endpointTestServer(t, func(s *Server) {
				s.RegisterHandler(id.GetEndpointsRequest_Encoding_DefaultBinary, tc.handler)
			})

			resp, err := callCreateSession(t, s, "opc.tcp://localhost:4840", 3)
			require.Nil(t, resp)
			require.ErrorIs(t, err, tc.wantErr)
			require.Zero(t, sessionCount(s))
		})
	}

	t.Run("handler panics", func(t *testing.T) {
		s := endpointTestServer(t, func(s *Server) {
			s.RegisterHandler(id.GetEndpointsRequest_Encoding_DefaultBinary, func(*uasc.SecureChannel, ua.Request, uint32) (ua.Response, error) {
				panic("GetEndpoints failed")
			})
		})

		func() {
			defer func() {
				require.NotNil(t, recover())
			}()
			_, _ = callCreateSession(t, s, "opc.tcp://localhost:4840", 3)
		}()
		require.Zero(t, sessionCount(s))
	})
}

// TestCreateSessionWithoutRegisteredHandlers checks that CreateSession falls
// back to the default GetEndpoints behavior when it is called on a Server
// whose handlers were not initialized.
func TestCreateSessionWithoutRegisteredHandlers(t *testing.T) {
	s := New(
		EndPoint("localhost", 4840),
		EnableSecurity("None", ua.MessageSecurityModeNone),
		EnableAuthMode(ua.UserTokenTypeAnonymous),
	)
	s.initEndpoints()

	resp, err := (&SessionService{s}).CreateSession(noneChannel(t), &ua.CreateSessionRequest{
		RequestHeader:           &ua.RequestHeader{RequestHandle: 4},
		EndpointURL:             "opc.tcp://localhost:4840",
		RequestedSessionTimeout: 60000,
	}, 4)
	require.NoError(t, err)
	require.Equal(t, s.Endpoints(), resp.(*ua.CreateSessionResponse).ServerEndpoints)
}
