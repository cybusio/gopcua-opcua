// Copyright 2018-2020 opcua authors. All rights reserved.
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

package uasc

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/gopcua/opcua/ua"
	"github.com/stretchr/testify/require"
)

// unencodableBody is the body of an ExtensionObject that cannot be encoded:
// the encoder has no encoding for a channel.
type unencodableBody struct {
	C chan int
}

// panickingBody is the body of an ExtensionObject whose encoder panics.
type panickingBody struct{}

func (panickingBody) Encode() ([]byte, error) {
	panic("panickingBody: Encode")
}

// TestUnsentMessageKeepsSequenceNumber sends a message, then one that fails
// before a chunk of it is written, then another message, and reads the
// SequenceNumbers that reach the connection. A message draws its number when
// it is built, before it is encoded; if the failed one kept it, the message
// after it would skip a number. OPC 10000-6 §6.7.2.4 requires the number to be
// "incremented by exactly one for each MessageChunk sent", and a receiver that
// checks it fails the channel on the gap.
func TestUnsentMessageKeepsSequenceNumber(t *testing.T) {
	value := func(body any) *ua.DataValue {
		v := ua.MustVariant(int32(1))
		if body != nil {
			v = ua.MustVariant(&ua.ExtensionObject{
				TypeID:       ua.NewFourByteExpandedNodeID(1, 1),
				EncodingMask: ua.ExtensionObjectBinary,
				Value:        body,
			})
		}
		return &ua.DataValue{EncodingMask: ua.DataValueValue, Value: v}
	}
	request := func(body any) *ua.WriteRequest {
		return &ua.WriteRequest{NodesToWrite: []*ua.WriteValue{{
			NodeID:      ua.NewNumericNodeID(1, 1),
			AttributeID: ua.AttributeIDValue,
			Value:       value(body),
		}}}
	}
	response := func(body any) *ua.ReadResponse {
		return &ua.ReadResponse{
			ResponseHeader: &ua.ResponseHeader{},
			Results:        []*ua.DataValue{value(body)},
		}
	}

	// send sends one message whose value has the given ExtensionObject body,
	// or a plain value if body is nil.
	type send func(ctx context.Context, s *SecureChannel, instance *channelInstance, reqID uint32, body any) error
	paths := []struct {
		name string
		send send
	}{
		{
			name: "request",
			send: func(ctx context.Context, s *SecureChannel, instance *channelInstance, reqID uint32, body any) error {
				_, err := s.sendAsyncWithTimeout(ctx, request(body), reqID, instance, nil, false, chunkedResponseTestTimeout)
				return err
			},
		},
		{
			name: "response",
			send: func(ctx context.Context, s *SecureChannel, instance *channelInstance, reqID uint32, body any) error {
				return s.SendResponseWithContext(ctx, reqID, response(body))
			},
		},
		{
			name: "message on an instance",
			send: func(ctx context.Context, s *SecureChannel, instance *channelInstance, reqID uint32, body any) error {
				return s.SendMsgWithContext(ctx, instance, reqID, response(body))
			},
		},
	}

	done, cancel := context.WithCancel(context.Background())
	cancel()

	failures := []struct {
		name   string
		ctx    context.Context
		body   any
		panics bool
	}{
		{name: "body cannot be encoded", ctx: context.Background(), body: unencodableBody{}},
		{name: "context already done", ctx: done},
		{name: "encoder panics", ctx: context.Background(), body: panickingBody{}, panics: true},
	}

	for _, path := range paths {
		for _, failure := range failures {
			t.Run(path.name+"/"+failure.name, func(t *testing.T) {
				s, peer := newSequenceNumberTestChannel(t)
				instance := s.activeInstance

				require.NoError(t, path.send(context.Background(), s, instance, 1, nil))

				failed := func() error { return path.send(failure.ctx, s, instance, 2, failure.body) }
				if failure.panics {
					require.Panics(t, func() { _ = failed() })
				} else {
					require.Error(t, failed())
				}

				require.NoError(t, path.send(context.Background(), s, instance, 3, nil))

				conn, ok := peer.conn.(net.Conn)
				require.True(t, ok, "the test reads from the peer's end of the connection")
				require.Equal(t, []uint32{1, 2}, readSequenceNumbers(t, conn, 2),
					"the message after one that was not sent must carry the successor of the last SequenceNumber on the connection")
			})
		}
	}
}

// readSequenceNumbers reads n chunks from conn and returns their
// SequenceNumbers in the order they arrived.
func readSequenceNumbers(t *testing.T, conn net.Conn, n int) []uint32 {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(chunkedResponseTestTimeout)))

	var numbers []uint32
	for len(numbers) < n {
		hdr := make([]byte, 8)
		_, err := io.ReadFull(conn, hdr)
		require.NoError(t, err, "reading the header of chunk %d of %d", len(numbers)+1, n)

		size := binary.LittleEndian.Uint32(hdr[4:8])
		require.GreaterOrEqual(t, size, uint32(24), "chunk %d is too short for a SequenceHeader", len(numbers)+1)

		rest := make([]byte, int(size)-8)
		_, err = io.ReadFull(conn, rest)
		require.NoError(t, err, "reading chunk %d of %d", len(numbers)+1, n)

		// 8 bytes of the message header plus 4 each of channel id and
		// token id precede the SequenceNumber (OPC 10000-6 §6.7.2)
		numbers = append(numbers, binary.LittleEndian.Uint32(rest[8:12]))
	}
	return numbers
}
