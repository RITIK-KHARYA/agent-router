// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package openai

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	"github.com/envoyproxy/ai-gateway/internal/json"
	"github.com/envoyproxy/ai-gateway/internal/testing/testotel"
	"github.com/envoyproxy/ai-gateway/internal/tracing/openinference"
)

func TestChatCompletionRecorder_StartParams(t *testing.T) {
	tests := []struct {
		name             string
		req              *openai.ChatCompletionRequest
		reqBody          []byte
		expectedSpanName string
	}{
		{
			name:             "basic request",
			req:              basicReq,
			reqBody:          basicReqBody,
			expectedSpanName: "ChatCompletion",
		},
		{
			name:             "streaming request",
			req:              streamingReq,
			reqBody:          streamingReqBody,
			expectedSpanName: "ChatCompletion",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := NewChatCompletionRecorderFromEnv()

			spanName, opts := recorder.StartParams(tt.req, tt.reqBody)
			actualSpan := testotel.RecordNewSpan(t, spanName, opts...)

			require.Equal(t, tt.expectedSpanName, actualSpan.Name)
			require.Equal(t, oteltrace.SpanKindInternal, actualSpan.SpanKind)
		})
	}
}

func TestChatCompletionRecorder_RecordRequest(t *testing.T) {
	tests := []struct {
		name          string
		req           *openai.ChatCompletionRequest
		reqBody       []byte
		expectedAttrs []attribute.KeyValue
	}{
		{
			name:    "basic request",
			req:     basicReq,
			reqBody: basicReqBody,
			expectedAttrs: []attribute.KeyValue{
				attribute.String(openinference.SpanKind, openinference.SpanKindLLM),
				attribute.String(openinference.LLMSystem, openinference.LLMSystemOpenAI),
				attribute.String(openinference.LLMModelName, openai.ModelGPT5Nano),
				attribute.String(openinference.InputValue, string(basicReqBody)),
				attribute.String(openinference.InputMimeType, openinference.MimeTypeJSON),
				attribute.String(openinference.LLMInvocationParameters, `{"model":"gpt-5-nano"}`),
				attribute.String(openinference.InputMessageAttribute(0, openinference.MessageRole), openai.ChatMessageRoleUser),
				attribute.String(openinference.InputMessageAttribute(0, openinference.MessageContent), "Hello!"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := NewChatCompletionRecorderFromEnv()

			actualSpan := testotel.RecordWithSpan(t, func(span oteltrace.Span) bool {
				recorder.RecordRequest(span, tt.req, tt.reqBody)
				return false
			})

			openinference.RequireAttributesEqual(t, tt.expectedAttrs, actualSpan.Attributes)
			require.Empty(t, actualSpan.Events)
			require.Equal(t, trace.Status{Code: codes.Unset, Description: ""}, actualSpan.Status)
		})
	}
}

func TestChatCompletionRecorder_RecordResponse(t *testing.T) {
	tests := []struct {
		name           string
		respBody       []byte
		expectedAttrs  []attribute.KeyValue
		expectedEvents []trace.Event
		expectedStatus trace.Status
	}{
		{
			name:     "successful response",
			respBody: basicRespBody,
			expectedAttrs: []attribute.KeyValue{
				attribute.String(openinference.LLMModelName, openai.ModelGPT5Nano),
				attribute.String(openinference.OutputMimeType, openinference.MimeTypeJSON),
				attribute.String(openinference.OutputMessageAttribute(0, openinference.MessageRole), "assistant"),
				attribute.String(openinference.OutputMessageAttribute(0, openinference.MessageContent), "Hello! How can I help you today?"),
				attribute.Int(openinference.LLMTokenCountPrompt, 20),
				attribute.Int(openinference.LLMTokenCountCompletion, 10),
				attribute.Int(openinference.LLMTokenCountTotal, 30),
				attribute.String(openinference.OutputValue, string(basicRespBody)),
			},
			expectedEvents: nil,
			expectedStatus: trace.Status{Code: codes.Ok, Description: ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := NewChatCompletionRecorderFromEnv()

			resp := &openai.ChatCompletionResponse{}
			err := json.Unmarshal(tt.respBody, resp)
			require.NoError(t, err)

			actualSpan := testotel.RecordWithSpan(t, func(span oteltrace.Span) bool {
				recorder.RecordResponse(span, resp)
				return false
			})

			openinference.RequireAttributesEqual(t, tt.expectedAttrs, actualSpan.Attributes)
			openinference.RequireEventsEqual(t, tt.expectedEvents, actualSpan.Events)
			require.Equal(t, tt.expectedStatus, actualSpan.Status)
		})
	}
}

func TestChatCompletionRecorder_RecordResponseOnError(t *testing.T) {
	recorder := NewChatCompletionRecorderFromEnv()

	actualSpan := testotel.RecordWithSpan(t, func(span oteltrace.Span) bool {
		recorder.RecordResponseOnError(span, 400, []byte(`{"error":{"message":"Invalid request","type":"invalid_request_error"}}`))
		return false
	})

	openinference.RequireAttributesEqual(t, []attribute.KeyValue{}, actualSpan.Attributes)
	openinference.RequireEventsEqual(t, []trace.Event{{
		Name: "exception",
		Attributes: []attribute.KeyValue{
			attribute.String("exception.type", "BadRequestError"),
			attribute.String("exception.message", `Error code: 400 - {"error":{"message":"Invalid request","type":"invalid_request_error"}}`),
		},
		Time: time.Time{},
	}}, actualSpan.Events)
	require.Equal(t, trace.Status{
		Code:        codes.Error,
		Description: `Error code: 400 - {"error":{"message":"Invalid request","type":"invalid_request_error"}}`,
	}, actualSpan.Status)
}

func TestChatCompletionRecorder_RecordResponseChunks(t *testing.T) {
	recorder := NewChatCompletionRecorderFromEnv()

	actualSpan := testotel.RecordWithSpan(t, func(span oteltrace.Span) bool {
		recorder.RecordResponseChunks(span, []*openai.ChatCompletionResponseChunk{{}, {}})
		return false
	})

	openinference.RequireEventsEqual(t, []trace.Event{
		{
			Name: "First Token Stream Event",
			Time: time.Time{},
		},
	}, actualSpan.Events)
}

// TestChatCompletionRecorder_RecordResponseChunks_ToolCalls pins that a
// streaming function-calling response records the same tool-call span
// attributes as the equivalent non-streaming response: the call id, function
// name, and concatenated arguments must survive the SSE-to-JSON folding and
// appear both as llm.output_messages.*.message.tool_calls.* attributes and
// inside the synthesized output.value JSON. This is the regression test for
// the bug where convertSSEToJSON dropped Delta.ToolCalls.
func TestChatCompletionRecorder_RecordResponseChunks_ToolCalls(t *testing.T) {
	id := "call_abc"
	chunks := []*openai.ChatCompletionResponseChunk{
		{ID: "chatcmpl-1", Model: "gpt-5-nano", Choices: []openai.ChatCompletionResponseChunkChoice{{
			Index: 0,
			Delta: &openai.ChatCompletionResponseChunkChoiceDelta{
				Role: "assistant",
				ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{{
					Index: 0, ID: &id, Type: openai.ChatCompletionMessageToolCallTypeFunction,
					Function: openai.ChatCompletionMessageToolCallFunctionParam{Name: "get_weather"},
				}},
			},
		}}},
		{ID: "chatcmpl-1", Model: "gpt-5-nano", Choices: []openai.ChatCompletionResponseChunkChoice{{
			Index: 0,
			Delta: &openai.ChatCompletionResponseChunkChoiceDelta{
				ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{{
					Index:    0,
					Function: openai.ChatCompletionMessageToolCallFunctionParam{Arguments: `{"location":"NYC"}`},
				}},
			},
		}}},
		{ID: "chatcmpl-1", Model: "gpt-5-nano", Choices: []openai.ChatCompletionResponseChunkChoice{{
			Index:        0,
			Delta:        &openai.ChatCompletionResponseChunkChoiceDelta{},
			FinishReason: openai.ChatCompletionChoicesFinishReasonToolCalls,
		}}},
	}

	recorder := NewChatCompletionRecorderFromEnv()
	actualSpan := testotel.RecordWithSpan(t, func(span oteltrace.Span) bool {
		recorder.RecordResponseChunks(span, chunks)
		return false
	})

	// The streamed response must fold into the same message the non-streaming
	// path would record, so output.value carries the reassembled tool_calls.
	expectedOutputValue := mustJSON(&openai.ChatCompletionResponse{
		ID:     "chatcmpl-1",
		Object: "chat.completion.chunk",
		Model:  "gpt-5-nano",
		Choices: []openai.ChatCompletionResponseChoice{{
			Index: 0,
			Message: openai.ChatCompletionResponseChoiceMessage{
				Role:    "assistant",
				Content: ptr(""),
				ToolCalls: []openai.ChatCompletionMessageToolCallParam{{
					ID:   &id,
					Type: openai.ChatCompletionMessageToolCallTypeFunction,
					Function: openai.ChatCompletionMessageToolCallFunctionParam{
						Name:      "get_weather",
						Arguments: `{"location":"NYC"}`,
					},
				}},
			},
			FinishReason: openai.ChatCompletionChoicesFinishReasonToolCalls,
		}},
	})

	expectedAttrs := []attribute.KeyValue{
		attribute.String(openinference.LLMModelName, "gpt-5-nano"),
		attribute.String(openinference.OutputMimeType, openinference.MimeTypeJSON),
		attribute.String(openinference.OutputMessageAttribute(0, openinference.MessageRole), "assistant"),
		attribute.String(openinference.OutputMessageToolCallAttribute(0, 0, openinference.ToolCallID), "call_abc"),
		attribute.String(openinference.OutputMessageToolCallAttribute(0, 0, openinference.ToolCallFunctionName), "get_weather"),
		attribute.String(openinference.OutputMessageToolCallAttribute(0, 0, openinference.ToolCallFunctionArguments), `{"location":"NYC"}`),
		attribute.String(openinference.OutputValue, string(expectedOutputValue)),
	}

	openinference.RequireAttributesEqual(t, expectedAttrs, actualSpan.Attributes)
	require.Equal(t, trace.Status{Code: codes.Ok, Description: ""}, actualSpan.Status)
	// The streaming path still emits the First Token Stream Event.
	openinference.RequireEventsEqual(t, []trace.Event{{
		Name: "First Token Stream Event",
		Time: time.Time{},
	}}, actualSpan.Events)
}

// TestChatCompletionRecorder_StreamingToolCallsMatchUnary pins that the
// OpenInference attributes recorded for a streaming tool-call response match
// those of the equivalent non-streaming response, ignoring output.value (which
// differs only in the synthesized object type). The non-streaming tool-call
// path is already covered by response_attrs_test.go; this guarantees the
// streaming converter does not regress it.
func TestChatCompletionRecorder_StreamingToolCallsMatchUnary(t *testing.T) {
	id := "call_abc"
	unary := &openai.ChatCompletionResponse{
		ID:    "chatcmpl-1",
		Model: "gpt-5-nano",
		Choices: []openai.ChatCompletionResponseChoice{{
			Index: 0,
			Message: openai.ChatCompletionResponseChoiceMessage{
				Role:    "assistant",
				Content: ptr(""),
				ToolCalls: []openai.ChatCompletionMessageToolCallParam{{
					ID:   &id,
					Type: openai.ChatCompletionMessageToolCallTypeFunction,
					Function: openai.ChatCompletionMessageToolCallFunctionParam{
						Name:      "get_weather",
						Arguments: `{"location":"NYC"}`,
					},
				}},
			},
			FinishReason: openai.ChatCompletionChoicesFinishReasonToolCalls,
		}},
	}

	streamed := []*openai.ChatCompletionResponseChunk{
		{ID: "chatcmpl-1", Model: "gpt-5-nano", Choices: []openai.ChatCompletionResponseChunkChoice{{
			Index: 0,
			Delta: &openai.ChatCompletionResponseChunkChoiceDelta{
				Role: "assistant",
				ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{{
					Index: 0, ID: &id, Type: openai.ChatCompletionMessageToolCallTypeFunction,
					Function: openai.ChatCompletionMessageToolCallFunctionParam{Name: "get_weather"},
				}},
			},
		}}},
		{ID: "chatcmpl-1", Model: "gpt-5-nano", Choices: []openai.ChatCompletionResponseChunkChoice{{
			Index: 0,
			Delta: &openai.ChatCompletionResponseChunkChoiceDelta{
				ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{{
					Index:    0,
					Function: openai.ChatCompletionMessageToolCallFunctionParam{Arguments: `{"location":"NYC"}`},
				}},
			},
		}}},
		{ID: "chatcmpl-1", Model: "gpt-5-nano", Choices: []openai.ChatCompletionResponseChunkChoice{{
			Index:        0,
			Delta:        &openai.ChatCompletionResponseChunkChoiceDelta{},
			FinishReason: openai.ChatCompletionChoicesFinishReasonToolCalls,
		}}},
	}

	recorder := NewChatCompletionRecorderFromEnv()

	unarySpan := testotel.RecordWithSpan(t, func(span oteltrace.Span) bool {
		recorder.RecordResponse(span, unary)
		return false
	})
	streamSpan := testotel.RecordWithSpan(t, func(span oteltrace.Span) bool {
		recorder.RecordResponseChunks(span, streamed)
		return false
	})

	// Everything except output.value must match: the streaming converter keeps
	// object="chat.completion.chunk" while the unary response carries no
	// object field, so output.value legitimately differs.
	unaryAttrs := dropAttr(unarySpan.Attributes, openinference.OutputValue)
	streamAttrs := dropAttr(streamSpan.Attributes, openinference.OutputValue)
	openinference.RequireAttributesEqual(t, unaryAttrs, streamAttrs)

	// Sanity-check the streaming output.value still carries the tool call.
	streamOutput := attrValue(streamSpan.Attributes, openinference.OutputValue)
	require.JSONEq(t, string(mustJSON(&openai.ChatCompletionResponse{
		ID:     "chatcmpl-1",
		Object: "chat.completion.chunk",
		Model:  "gpt-5-nano",
		Choices: []openai.ChatCompletionResponseChoice{{
			Index: 0,
			Message: openai.ChatCompletionResponseChoiceMessage{
				Role:    "assistant",
				Content: ptr(""),
				ToolCalls: []openai.ChatCompletionMessageToolCallParam{{
					ID:   &id,
					Type: openai.ChatCompletionMessageToolCallTypeFunction,
					Function: openai.ChatCompletionMessageToolCallFunctionParam{
						Name:      "get_weather",
						Arguments: `{"location":"NYC"}`,
					},
				}},
			},
			FinishReason: openai.ChatCompletionChoicesFinishReasonToolCalls,
		}},
	})), streamOutput)
}

func dropAttr(attrs []attribute.KeyValue, key attribute.Key) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, kv := range attrs {
		if kv.Key == key {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func attrValue(attrs []attribute.KeyValue, key attribute.Key) string {
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.Value.AsString()
		}
	}
	return ""
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func ptr[T any](v T) *T {
	return &v
}
