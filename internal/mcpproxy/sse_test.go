// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package mcpproxy

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
)

// helper to encode a jsonrpc message to use inside data: lines.
func mustEncode(t *testing.T, m jsonrpc.Message) []byte {
	t.Helper()
	b, err := jsonrpc.EncodeMessage(m)
	require.NoError(t, err)
	return b
}

func TestTryDecodeJSONRPCMessage(t *testing.T) {
	id, err := jsonrpc.MakeID("1")
	require.NoError(t, err)
	want := &jsonrpc.Response{ID: id, Result: []byte(`{"tools":[]}`)}
	raw, err := jsonrpc.EncodeMessage(want)
	require.NoError(t, err)

	t.Run("valid JSON-RPC", func(t *testing.T) {
		msg, ok := tryDecodeJSONRPCMessage(raw)
		require.True(t, ok)
		got, isResp := msg.(*jsonrpc.Response)
		require.True(t, isResp)
		require.Equal(t, want.ID, got.ID)
	})
	t.Run("BOM prefix stripped", func(t *testing.T) {
		bomBody := append(append([]byte{}, utf8BOM...), raw...)
		msg, ok := tryDecodeJSONRPCMessage(bomBody)
		require.True(t, ok)
		got, isResp := msg.(*jsonrpc.Response)
		require.True(t, isResp)
		require.Equal(t, want.ID, got.ID)
	})
	t.Run("leading whitespace stripped", func(t *testing.T) {
		msg, ok := tryDecodeJSONRPCMessage(append([]byte("  \n"), raw...))
		require.True(t, ok)
		require.NotNil(t, msg)
	})
	t.Run("SSE body returns false", func(t *testing.T) {
		sseBody := append(append([]byte("data: "), raw...), []byte("\n\n")...)
		_, ok := tryDecodeJSONRPCMessage(sseBody)
		require.False(t, ok)
	})
	t.Run("binary garbage returns false", func(t *testing.T) {
		_, ok := tryDecodeJSONRPCMessage([]byte{0x13, 0x65, 0x70, 0x8c})
		require.False(t, ok)
	})
}

func TestIsJSONContentType(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "plain JSON", value: "application/json", want: true},
		{name: "JSON with charset", value: "application/json;charset=utf-8", want: true},
		{name: "JSON with whitespace", value: " Application/JSON ; charset=utf-8 ", want: true},
		{name: "SSE", value: "text/event-stream", want: false},
		{name: "invalid media type", value: "not a media type", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isJSONContentType(tt.value))
		})
	}
}

func TestSSEEventParser_SingleEvent(t *testing.T) {
	id, err := jsonrpc.MakeID("1")
	require.NoError(t, err)
	req := &jsonrpc.Request{Method: "initialize", ID: id}
	encoded := mustEncode(t, req)

	tests := []struct {
		name string
		raw  []byte
	}{
		{
			"with LF separators",
			bytes.Join([][]byte{
				[]byte("event: message\n"),
				[]byte("id: 42\n"),
				append([]byte("data: "), encoded...),
				[]byte("\n\n"),
			}, nil),
		},
		{
			"with CR separators",
			bytes.Join([][]byte{
				[]byte("event: message\r"),
				[]byte("id: 42\r"),
				append([]byte("data: "), encoded...),
				[]byte("\r\r"),
			}, nil),
		},
		{
			"with CRLF separators",
			bytes.Join([][]byte{
				[]byte("event: message\r\n"),
				[]byte("id: 42\r\n"),
				append([]byte("data: "), encoded...),
				[]byte("\r\n\r\n"),
			}, nil),
		},
		{
			"with mixed separators",
			bytes.Join([][]byte{
				[]byte("event: message\r\n"),
				[]byte("id: 42\n"),
				append([]byte("data: "), encoded...),
				[]byte("\r\r"),
			}, nil),
		},
		{
			"with mixed separators",
			bytes.Join([][]byte{
				[]byte("event: message\r"),
				[]byte("id: 42\r\n"),
				append([]byte("data: "), encoded...),
				[]byte("\n\n"),
			}, nil),
		},
		{
			"without space after colon",
			bytes.Join([][]byte{
				[]byte("event:message\n"),
				[]byte("id:42\n"),
				append([]byte("data:"), encoded...),
				[]byte("\n\n"),
			}, nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newSSEEventParser(bytes.NewReader(tt.raw), "mybackend")
			ev, err := p.next()
			require.NoError(t, err)
			require.Equal(t, "mybackend", ev.backend)
			require.Equal(t, "message", ev.event)
			require.Equal(t, "42", ev.id)
			require.Len(t, ev.messages, 1)
			gotReq, ok := ev.messages[0].(*jsonrpc.Request)
			require.True(t, ok)
			require.Equal(t, req.Method, gotReq.Method)
			require.Equal(t, req.ID, gotReq.ID)
		})
	}
}

func TestSSEEventParser_MultipleEvents(t *testing.T) {
	id1, err := jsonrpc.MakeID("10")
	require.NoError(t, err)
	id2, err := jsonrpc.MakeID("11")
	require.NoError(t, err)
	r1 := &jsonrpc.Request{Method: "foo", ID: id1}
	r2 := &jsonrpc.Request{Method: "bar", ID: id2}

	tests := []struct {
		name string
		raw  []byte
	}{
		{
			"with LF separators",
			bytes.Join([][]byte{
				[]byte("event: e1\n"), append([]byte("data: "), mustEncode(t, r1)...), []byte("\n\n"),
				[]byte("event: e2\n"), append([]byte("data: "), mustEncode(t, r2)...), []byte("\n\n"),
			}, nil),
		},
		{
			"with CR separators",
			bytes.Join([][]byte{
				[]byte("event: e1\r"), append([]byte("data: "), mustEncode(t, r1)...), []byte("\r\r"),
				[]byte("event: e2\r"), append([]byte("data: "), mustEncode(t, r2)...), []byte("\r\r"),
			}, nil),
		},
		{
			"with CRLF separators",
			bytes.Join([][]byte{
				[]byte("event: e1\r\n"), append([]byte("data: "), mustEncode(t, r1)...), []byte("\r\n\r\n"),
				[]byte("event: e2\r\n"), append([]byte("data: "), mustEncode(t, r2)...), []byte("\r\n\r\n"),
			}, nil),
		},
		{
			"with mixes separators",
			bytes.Join([][]byte{
				[]byte("event: e1\n"), append([]byte("data: "), mustEncode(t, r1)...), []byte("\n\n"),
				[]byte("event: e2\r\n"), append([]byte("data: "), mustEncode(t, r2)...), []byte("\r\r"),
			}, nil),
		},
		{
			"without space after colon",
			bytes.Join([][]byte{
				[]byte("event:e1\n"), append([]byte("data:"), mustEncode(t, r1)...), []byte("\n\n"),
				[]byte("event:e2\n"), append([]byte("data:"), mustEncode(t, r2)...), []byte("\n\n"),
			}, nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newSSEEventParser(bytes.NewReader(tt.raw), "mybackend")
			ev1, err := p.next()
			require.NoError(t, err)
			ev2, err := p.next()
			require.NoError(t, err)
			require.Equal(t, "e1", ev1.event)
			require.Equal(t, "e2", ev2.event)
			require.Equal(t, "mybackend", ev1.backend)
			require.Equal(t, "mybackend", ev2.backend)
			require.Len(t, ev1.messages, 1)
			require.Len(t, ev2.messages, 1)
		})
	}
}

// partialReader simulates an io.Reader that returns provided chunks sequentially.
type partialReader struct {
	chunks [][]byte
	idx    int
}

func (p *partialReader) Read(b []byte) (int, error) {
	if p.idx >= len(p.chunks) {
		return 0, io.EOF
	}
	n := copy(b, p.chunks[p.idx])
	p.idx++
	return n, nil
}

func TestSSEEventParser_PartialReads(t *testing.T) {
	id, err := jsonrpc.MakeID("abc")
	require.NoError(t, err)
	req := &jsonrpc.Request{Method: "stream", ID: id}
	encoded := mustEncode(t, req)

	tests := []struct {
		name string
		raw  []byte
	}{
		{
			"with LF separators",
			bytes.Join([][]byte{
				[]byte("event:"), []byte(" message\nid"), []byte(": 99\n"), []byte("data: "), encoded, []byte("\n\n"),
			}, nil),
		},
		{
			"with CR separators",
			bytes.Join([][]byte{
				[]byte("event:"), []byte(" message\rid"), []byte(": 99\r"), []byte("data: "), encoded, []byte("\r\r"),
			}, nil),
		},
		{
			"with CRLF separators",
			bytes.Join([][]byte{
				[]byte("event:"), []byte(" message\r\nid"), []byte(": 99\r\n"), []byte("data: "), encoded, []byte("\r\n\r\n"),
			}, nil),
		},
		{
			"with mixed separators",
			bytes.Join([][]byte{
				[]byte("event:"), []byte(" message\nid"), []byte(": 99\r"), []byte("data: "), encoded, []byte("\r\r"),
			}, nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pr := &partialReader{chunks: [][]byte{tt.raw[:5], tt.raw[5:12], tt.raw[12:20], tt.raw[20:30], tt.raw[30:]}}
			p := newSSEEventParser(pr, "mybackend")
			ev, err := p.next()
			require.NoError(t, err)
			require.Equal(t, "mybackend", ev.backend)
			require.Equal(t, "message", ev.event)
			require.Equal(t, "99", ev.id)
			require.Len(t, ev.messages, 1)
		})
	}
}

func TestSSEEventParser_IncompleteEvent(t *testing.T) {
	raw := []byte("event: foo\ndat")
	p := newSSEEventParser(bytes.NewReader(raw), "mybackend")
	ev, err := p.next()
	require.NotNil(t, ev)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, "mybackend", ev.backend)
	require.Equal(t, "foo", ev.event)
	require.Empty(t, ev.id)
	require.Nil(t, ev.messages)
}

func TestSSEEventParser_InvalidJSONRPCMessage(t *testing.T) {
	// Malformed JSON (not a jsonrpc message).
	for _, prefix := range []string{"data:", "data: "} {
		t.Run(prefix, func(t *testing.T) {
			raw := []byte(prefix + " {invalid json}\n\n")
			p := newSSEEventParser(bytes.NewReader(raw), "mybackend")
			ev, err := p.next()
			require.Nil(t, ev)
			require.Error(t, err)
		})
	}
}

func TestSSEEvent_WriteAndMaybeFlush(t *testing.T) {
	// Build an event with a request and a response to test multi message writing.
	id, err := jsonrpc.MakeID("1")
	require.NoError(t, err)
	req := &jsonrpc.Request{Method: "foo", ID: id}
	resp := &jsonrpc.Response{ID: id}

	ev := &sseEvent{event: "custom", id: "7", messages: []jsonrpc.Message{req, resp}}
	rr := httptest.NewRecorder()
	ev.writeAndMaybeFlush(rr)
	output := rr.Body.String()
	// Basic structural assertions.
	require.Contains(t, output, "event: custom\n")
	require.Contains(t, output, "id: 7\n")
	require.Contains(t, output, "data: ")
	// It ends with the separator (blank line).
	require.True(t, bytes.HasSuffix([]byte(output), []byte("\n\n")))
}

func TestSSEEventParser_EndOfStream(t *testing.T) {
	// One good event then EOF.
	req := &jsonrpc.Request{Method: "last"}

	tests := []struct {
		name string
		raw  []byte
	}{
		{
			"with LF separators",
			bytes.Join([][]byte{append([]byte("data: "), mustEncode(t, req)...), []byte("\n\n")}, nil),
		},
		{
			"with CR separators",
			bytes.Join([][]byte{append([]byte("data: "), mustEncode(t, req)...), []byte("\r\r")}, nil),
		},
		{
			"with CRLF separators",
			bytes.Join([][]byte{append([]byte("data: "), mustEncode(t, req)...), []byte("\r\n\r\n")}, nil),
		},
		{
			"with mixed separators",
			bytes.Join([][]byte{append([]byte("id: 12\rdata: "), mustEncode(t, req)...), []byte("\r\n\r\n")}, nil),
		},
		{
			"without space after colon",
			bytes.Join([][]byte{append([]byte("id:12\ndata:"), mustEncode(t, req)...), []byte("\n\n")}, nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newSSEEventParser(bytes.NewReader(tt.raw), "mybackend")
			ev, err := p.next()
			require.NoError(t, err)
			require.NotNil(t, ev)
			require.Equal(t, "mybackend", ev.backend)
			// Next call should return EOF.
			_, err = p.next()
			require.Error(t, err)
			if !errors.Is(err, io.EOF) { // Allow other final errors but usually EOF.
				t.Logf("received terminal error: %v", err)
			}
		})
	}
}

// Issue #2219: a leading keep-alive event with an empty data line (emitted by
// some FastMCP backends, e.g. firecrawl with protocolVersion 2025-11-25, before
// the real response) must be skipped, not treated as a JSON decode error.
func TestSSEEventParser_EmptyDataLineSkipped(t *testing.T) {
	raw := []byte("id: keepalive_0000\ndata:\n\n" +
		"event: message\nid: msg_0001\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{}}\n\n")
	p := newSSEEventParser(bytes.NewReader(raw), "mybackend")

	// First event carries only an empty data line: no messages, no error.
	// It has a non-empty id, so it is surfaced (regression guard for the #2219 fix).
	ev1, err := p.next()
	require.NoError(t, err)
	require.NotNil(t, ev1)
	require.Empty(t, ev1.messages)
	require.Equal(t, "keepalive_0000", ev1.id)

	// Second event is the real JSON-RPC response.
	ev2, err := p.next()
	require.NoError(t, err)
	require.Len(t, ev2.messages, 1)
	_, ok := ev2.messages[0].(*jsonrpc.Response)
	require.True(t, ok)
}

// realRequest builds a minimal jsonrpc.Request suitable for SSE data lines.
func realRequest(t *testing.T, method string, idStr string) *jsonrpc.Request {
	t.Helper()
	id, err := jsonrpc.MakeID(idStr)
	require.NoError(t, err)
	return &jsonrpc.Request{Method: method, ID: id}
}

// drainEvents calls next() until an error is returned, collecting all non-nil
// events. It returns the events and the terminal error (io.EOF on a clean end).
func drainEvents(t *testing.T, p *sseEventParser) ([]*sseEvent, error) {
	t.Helper()
	var events []*sseEvent
	for {
		ev, err := p.next()
		if ev != nil {
			events = append(events, ev)
		}
		if err != nil {
			return events, err
		}
	}
}

// SSE comment-only heartbeats (":\n\n", the spec-recommended keep-alive) must
// not be surfaced as events: they carry no event:, id:, or data: content.
// Surfacing them would clobber the stored per-backend last event ID with "".
// This is the core regression test for the resume-clobber bug.
func TestSSEEventParser_CommentOnlyHeartbeatSkipped(t *testing.T) {
	encoded := mustEncode(t, realRequest(t, "ping", "1"))

	tests := []struct {
		name string
		// The heartbeat line terminator variant to use inside the heartbeat event.
		heartbeat []byte
		// The real event separator to use.
		separator []byte
	}{
		{"LF heartbeat", []byte(":\n\n"), []byte("\n\n")},
		{"CR heartbeat", []byte(":\r\r"), []byte("\n\n")},
		{"CRLF heartbeat", []byte(":\r\n\r\n"), []byte("\n\n")},
		{"heartbeat with comment text", []byte(": this is a keepalive comment\n\n"), []byte("\n\n")},
		{"heartbeat at offset 0", []byte(":\n\n\n\n"), []byte("\n\n")}, // double trailing separator
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			realEvent := bytes.Join([][]byte{
				[]byte("event: message\nid: real\ndata: "), encoded, tt.separator,
			}, nil)
			raw := bytes.Join([][]byte{tt.heartbeat, realEvent}, nil)
			p := newSSEEventParser(bytes.NewReader(raw), "b")

			// The first next() must skip the heartbeat and return the real event
			// directly — not a spurious empty event before it.
			ev, err := p.next()
			require.NoError(t, err)
			require.NotNil(t, ev, "heartbeat must be skipped, real event returned first")
			require.Equal(t, "b", ev.backend)
			require.Equal(t, "message", ev.event)
			require.Equal(t, "real", ev.id)
			require.Len(t, ev.messages, 1)

			// Stream then ends cleanly with EOF, no trailing spurious event.
			ev2, err := p.next()
			require.Nil(t, ev2, "no spurious event after the real event")
			require.ErrorIs(t, err, io.EOF)
		})
	}
}

// A trailing newline after the final event separator must not be parsed as a
// spurious empty event. This exercises the EOF-leftover path in next() that
// was introduced by the d13651709 refactor.
func TestSSEEventParser_TrailingNewlineNotSpurious(t *testing.T) {
	encoded := mustEncode(t, realRequest(t, "ping", "1"))

	tests := []struct {
		name    string
		trailer []byte
	}{
		{"single trailing LF", []byte("\n")},
		{"trailing LF LF", []byte("\n\n")},
		{"trailing CR", []byte("\r")},
		{"trailing CRLF", []byte("\r\n")},
		{"trailing comment fragment with LF", []byte(":\n")},
		{"trailing blanks then comment", []byte("\n:\n")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := bytes.Join([][]byte{
				[]byte("event: message\nid: real\ndata: "), encoded, []byte("\n\n"),
				tt.trailer,
			}, nil)
			p := newSSEEventParser(bytes.NewReader(raw), "b")

			ev, err := p.next()
			require.NoError(t, err)
			require.NotNil(t, ev)
			require.Equal(t, "real", ev.id)
			require.Len(t, ev.messages, 1)

			// The trailing chunk must not surface as a spurious event.
			ev2, err := p.next()
			require.Nil(t, ev2, "trailing chunk %q must not produce a spurious event", tt.trailer)
			require.ErrorIs(t, err, io.EOF)
		})
	}
}

// A trailing comment-only heartbeat (the last event before the connection
// drops) must not clobber the stored last event ID. This is the canonical SSE
// reconnection scenario: a long-lived stream goes idle, the backend sends a
// heartbeat, then the connection drops. The client reconnects and the gateway
// must still be able to request resumption for that backend.
func TestSSEEventParser_TrailingHeartbeatDoesNotClobber(t *testing.T) {
	encoded := mustEncode(t, realRequest(t, "ping", "1"))

	streams := []struct {
		name string
		raw  []byte
	}{
		{
			"trailing heartbeat",
			bytes.Join([][]byte{
				[]byte("event: message\nid: real_1\ndata: "), encoded, []byte("\n\n"),
				[]byte(":\n\n"), // trailing heartbeat — last event before disconnect
			}, nil),
		},
		{
			"trailing newline",
			bytes.Join([][]byte{
				[]byte("event: message\nid: real_1\ndata: "), encoded, []byte("\n\n"),
				[]byte("\n"), // trailing stray newline
			}, nil),
		},
	}

	for _, tt := range streams {
		t.Run(tt.name, func(t *testing.T) {
			p := newSSEEventParser(bytes.NewReader(tt.raw), "b")

			// Simulate the streamNotifications caller: store the last event id
			// per backend, only when an event is returned.
			store := map[string]string{}
			events, err := drainEvents(t, &p)
			require.ErrorIs(t, err, io.EOF)

			// Exactly one real event must be surfaced — the trailing spurious
			// event must not appear in the stream.
			require.Len(t, events, 1, "trailing spurious event must not be surfaced")
			require.Equal(t, "real_1", events[0].id)
			for _, ev := range events {
				store[ev.backend] = ev.id
			}

			// The stored last event ID must remain "real_1", not "" — this is
			// what the gateway forwards upstream on reconnect.
			require.Equal(t, "real_1", store["b"],
				"trailing spurious event must not clobber the real last event ID")
		})
	}
}

// Self-healing: a heartbeat strictly between two real id-bearing events must
// not persist as the stored last event ID. With the fix, the heartbeat is not
// surfaced at all, so self-healing is automatic.
func TestSSEEventParser_HeartbeatBetweenRealEventsSkipped(t *testing.T) {
	encoded := mustEncode(t, realRequest(t, "ping", "7"))
	raw := bytes.Join([][]byte{
		[]byte("event: message\nid: real_1\ndata: "), encoded, []byte("\n\n"),
		[]byte(":\n\n"), // heartbeat BETWEEN two real events
		[]byte("event: message\nid: real_2\ndata: "), encoded, []byte("\n\n"),
	}, nil)
	p := newSSEEventParser(bytes.NewReader(raw), "b")

	store := map[string]string{}
	events, err := drainEvents(t, &p)
	require.ErrorIs(t, err, io.EOF)

	// With the fix, the heartbeat is skipped entirely — only two real events.
	require.Len(t, events, 2, "heartbeat between real events must be skipped")
	for _, ev := range events {
		store[ev.backend] = ev.id
	}
	require.Equal(t, "real_2", store["b"])

	// Ordering must be preserved.
	require.Equal(t, "real_1", events[0].id)
	require.Equal(t, "real_2", events[1].id)
}

// A stream consisting entirely of comment-only heartbeats must surface no
// events at all and terminate with EOF (no spurious events, no clobbering).
func TestSSEEventParser_OnlyHeartbeats(t *testing.T) {
	raw := bytes.Join([][]byte{
		[]byte(":\n\n"),
		[]byte(": keepalive\n\n"),
		[]byte(":\r\n\r\n"),
		[]byte(":\n\n"),
	}, nil)
	p := newSSEEventParser(bytes.NewReader(raw), "b")

	events, err := drainEvents(t, &p)
	require.Empty(t, events, "comment-only stream must produce no events")
	require.ErrorIs(t, err, io.EOF)
}

// An event carrying only an id: line (no event:, no data:) must still be
// surfaced: it has a non-empty id, so the empty-event guard must not drop it.
// This guards against over-aggressive skipping.
func TestSSEEventParser_IDOnlyEventNotSkipped(t *testing.T) {
	raw := []byte("id: only_id\n\n" +
		"event: message\nid: real\ndata: " + string(mustEncode(t, realRequest(t, "ping", "1"))) + "\n\n")
	p := newSSEEventParser(bytes.NewReader(raw), "b")

	// The id-only event is surfaced (it carries a real id, used for resume tracking).
	ev1, err := p.next()
	require.NoError(t, err)
	require.NotNil(t, ev1)
	require.Equal(t, "only_id", ev1.id)
	require.Empty(t, ev1.event)
	require.Empty(t, ev1.messages)

	// The real event follows.
	ev2, err := p.next()
	require.NoError(t, err)
	require.Equal(t, "real", ev2.id)
	require.Len(t, ev2.messages, 1)
}

// An event carrying only an event: field (no id:, no data:) must still be
// surfaced: it has a non-empty event, so the empty-event guard must not drop it.
func TestSSEEventParser_EventOnlyNotSkipped(t *testing.T) {
	raw := []byte("event: notification\n\n" +
		"event: message\nid: real\ndata: " + string(mustEncode(t, realRequest(t, "ping", "1"))) + "\n\n")
	p := newSSEEventParser(bytes.NewReader(raw), "b")

	ev1, err := p.next()
	require.NoError(t, err)
	require.NotNil(t, ev1)
	require.Equal(t, "notification", ev1.event)
	require.Empty(t, ev1.id)
	require.Empty(t, ev1.messages)

	ev2, err := p.next()
	require.NoError(t, err)
	require.Equal(t, "real", ev2.id)
}

// Partial reads that split a comment-only heartbeat across chunks must still
// be skipped once the separator arrives.
func TestSSEEventParser_CommentHeartbeatSplitAcrossReads(t *testing.T) {
	encoded := mustEncode(t, realRequest(t, "ping", "1"))
	full := bytes.Join([][]byte{
		[]byte(":\n\n"),
		[]byte("event: message\nid: real\ndata: "), encoded, []byte("\n\n"),
	}, nil)

	// Split the stream into one-byte chunks so the heartbeat separator is
	// assembled only after several reads.
	chunks := make([][]byte, 0, len(full))
	for i := range full {
		chunks = append(chunks, full[i:i+1])
	}
	pr := &partialReader{chunks: chunks}
	p := newSSEEventParser(pr, "b")

	ev, err := p.next()
	require.NoError(t, err)
	require.NotNil(t, ev)
	require.Equal(t, "real", ev.id)
	require.Len(t, ev.messages, 1)

	ev2, err := p.next()
	require.Nil(t, ev2)
	require.ErrorIs(t, err, io.EOF)
}

// A leading blank separator ("\n\nevent: ...") must be skipped, not surfaced as
// a spurious empty event before the real one.
func TestSSEEventParser_LeadingBlankSeparatorSkipped(t *testing.T) {
	encoded := mustEncode(t, realRequest(t, "ping", "1"))
	raw := bytes.Join([][]byte{
		[]byte("\n\n"), // leading blank event
		[]byte("event: message\nid: real\ndata: "), encoded, []byte("\n\n"),
	}, nil)
	p := newSSEEventParser(bytes.NewReader(raw), "b")

	ev, err := p.next()
	require.NoError(t, err)
	require.NotNil(t, ev)
	require.Equal(t, "real", ev.id)
	require.Len(t, ev.messages, 1)

	ev2, err := p.next()
	require.Nil(t, ev2)
	require.ErrorIs(t, err, io.EOF)
}
