package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uasc"
	"github.com/stretchr/testify/require"
)

// errorLogger records the Error calls and ignores the other levels.
type errorLogger struct {
	mu     sync.Mutex
	errors []string
}

func (l *errorLogger) Debug(string, ...any) {}
func (l *errorLogger) Info(string, ...any)  {}
func (l *errorLogger) Warn(string, ...any)  {}

func (l *errorLogger) Error(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, fmt.Sprintf(msg, args...))
}

func (l *errorLogger) records() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.errors...)
}

// panicEncoder fails while the response that carries it is encoded.
type panicEncoder struct{}

func (panicEncoder) Encode() ([]byte, error) { panic("encode failed") }

// connectPanicTestClient opens a session on endpoint.
func connectPanicTestClient(t *testing.T, endpoint string) *opcua.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := opcua.NewClient(endpoint,
		opcua.SecurityMode(ua.MessageSecurityModeNone),
		opcua.AuthAnonymous(),
		opcua.RequestTimeout(5*time.Second),
	)
	require.NoError(t, err)
	require.NoError(t, c.Connect(ctx))
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func readCurrentTime(c *opcua.Client) (*ua.ReadResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Read(ctx, &ua.ReadRequest{
		NodesToRead: []*ua.ReadValueID{{
			NodeID:      ua.NewNumericNodeID(0, id.Server_ServerStatus_CurrentTime),
			AttributeID: ua.AttributeIDValue,
		}},
	})
}

// A panic while a request is handled, or while its response is encoded, is
// answered with a ServiceFault Bad_InternalError (Part 4 §7.38.2 Table 178)
// that carries the requestHandle of the request (Part 4 §7.34). It is logged
// once with the stack, and the server goes on serving that channel and others.
func TestHandleServiceRecoversFromPanic(t *testing.T) {
	tests := []struct {
		name string
		// fail is called by the Read handler instead of reading once.
		fail func(req *ua.ReadRequest) (ua.Response, error)
	}{
		{
			name: "handler panics",
			fail: func(*ua.ReadRequest) (ua.Response, error) { panic("handler failed") },
		},
		{
			name: "response encoding panics",
			fail: func(req *ua.ReadRequest) (ua.Response, error) {
				hdr := responseHeader(req.RequestHeader.RequestHandle, ua.StatusOK)
				hdr.AdditionalHeader = &ua.ExtensionObject{
					TypeID:       ua.NewFourByteExpandedNodeID(0, id.ReadResponse_Encoding_DefaultBinary),
					EncodingMask: ua.ExtensionObjectBinary,
					Value:        panicEncoder{},
				}
				return &ua.ReadResponse{ResponseHeader: hdr, DiagnosticInfos: []*ua.DiagnosticInfo{}}, nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := &errorLogger{}
			srv := New(
				EndPoint("127.0.0.1", 0),
				EnableSecurity("None", ua.MessageSecurityModeNone),
				EnableAuthMode(ua.UserTokenTypeAnonymous),
				SetLogger(logger),
			)

			// The handler holds a lock the way the service handlers do, so
			// the test also shows that a panic does not leave it held.
			var mu sync.Mutex
			var armed atomic.Bool
			attrs := &AttributeService{srv}
			srv.RegisterHandler(id.ReadRequest_Encoding_DefaultBinary, func(sc *uasc.SecureChannel, r ua.Request, reqID uint32) (ua.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				if armed.Swap(false) {
					return tt.fail(r.(*ua.ReadRequest))
				}
				return attrs.Read(sc, r, reqID)
			})

			ctx, cancel := context.WithCancel(context.Background())
			require.NoError(t, srv.Start(ctx))
			t.Cleanup(func() {
				cancel()
				_ = srv.Close()
			})
			endpoint := "opc.tcp://" + srv.l.Addr().String()

			first := connectPanicTestClient(t, endpoint)
			second := connectPanicTestClient(t, endpoint)
			before := len(logger.records())

			armed.Store(true)
			_, err := readCurrentTime(first)
			require.ErrorIs(t, err, ua.StatusBadInternalError)

			records := logger.records()[before:]
			require.Len(t, records, 1)
			require.Contains(t, records[0], "*ua.ReadRequest")
			require.True(t, strings.Contains(records[0], "goroutine ") && strings.Contains(records[0], "service_handlers.go"),
				"error record carries no stack: %q", records[0])

			// The other session is served at once.
			resp, err := readCurrentTime(second)
			require.NoError(t, err)
			require.Equal(t, ua.StatusOK, resp.ResponseHeader.ServiceResult)
			require.Len(t, resp.Results, 1)

			// The client treats a Bad service result as a channel error and
			// reconnects its session on a new secure channel first.
			require.Eventually(t, func() bool {
				resp, err := readCurrentTime(first)
				return err == nil && resp.ResponseHeader.ServiceResult == ua.StatusOK && len(resp.Results) == 1
			}, 10*time.Second, 50*time.Millisecond, "the session is not served after the panic")
			require.Len(t, logger.records()[before:], 1)
		})
	}
}

// The fault echoes the requestHandle of the request (Part 4 §7.34), also when
// the request carries no header.
func TestInternalErrorFault(t *testing.T) {
	for _, tt := range []struct {
		name string
		req  ua.Request
		want uint32
	}{
		{"request handle", &ua.ReadRequest{RequestHeader: &ua.RequestHeader{RequestHandle: 42}}, 42},
		{"no header", &ua.ReadRequest{}, 0},
		{"no request", nil, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fault := internalErrorFault(tt.req)
			require.Equal(t, ua.StatusBadInternalError, fault.ResponseHeader.ServiceResult)
			require.Equal(t, tt.want, fault.ResponseHeader.RequestHandle)
		})
	}
}
