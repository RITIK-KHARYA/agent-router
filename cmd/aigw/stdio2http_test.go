// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package main

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/json"
)

const (
	runMCPTestServer           = "__RUN_MCP_TEST_SERVER__"
	runMalformedStdioServerEnv = "__RUN_MALFORMED_MCP_SERVER__"
)

func TestMain(m *testing.M) {
	// If the runMalformedStdioServerEnv variable is set, run the hand-rolled
	// malformed MCP stdio server (selected by the env var's value) instead of
	// running tests. This is the fork-exec trick used to feed unvalidated wire
	// data into the stdio2http proxy; see runStdio2HTTPProxy.
	if mode := os.Getenv(runMalformedStdioServerEnv); mode != "" {
		_ = os.Unsetenv(runMalformedStdioServerEnv)
		runMalformedStdioServer(mode)
		return
	}

	// If the runMCPTestServer variable is set, run the mcp test server
	// instead of running tests (aka the fork and exec trick).
	if os.Getenv(runMCPTestServer) == "true" {
		_ = os.Unsetenv(runMCPTestServer)
		runTestStdioServer()
		return
	}

	os.Exit(m.Run())
}

func TestStdio2HTTP(t *testing.T) {
	// create a command to fork and exec the test binary as an MCP server.
	t.Setenv(runMCPTestServer, "true")
	cmd, err := os.Executable()
	require.NoError(t, err)

	// Run the stdio2http proxy; this will run the test binary as a subprocess in a separate goroutine.
	// Since it is bound to the test context, when the test is completed the command will be aborted.
	logger := slog.New(slog.DiscardHandler)
	addr, err := runStdio2HTTPProxy(t.Context(), logger, "test-stdio", cmd)
	require.NoError(t, err)

	// run a streamable HTTP client against the proxy.
	client := mcp.NewClient(&mcp.Implementation{Name: t.Name()}, nil)
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: addr}, nil)
	require.NoError(t, err)

	// Verify that the tool call is properly proxied.
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": "test stdio proxy"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Len(t, res.Content, 1)
	require.IsType(t, &mcp.TextContent{}, res.Content[0])
	require.Equal(t, "test stdio proxy", res.Content[0].(*mcp.TextContent).Text)
}

// runTestStdioServer runs a simple MCP stdio server that implements an "echo" tool.
// This method will be run in a subprocess via TestMain, which will be executed by the
// stdio2http proxy.
func runTestStdioServer() {
	type echoArgs struct {
		Text string `json:"text"`
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test-stdio"}, nil)
	mcp.AddTool(server,
		&mcp.Tool{Name: "echo", Description: "echo tool"},
		func(_ context.Context, _ *mcp.CallToolRequest, args echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: args.Text},
				},
			}, nil, nil
		})

	_ = server.Run(context.Background(), &mcp.StdioTransport{})
}

// The following modes select which malformed feature the hand-rolled stdio MCP
// server advertises, one per panic branch in the Go MCP SDK.
const (
	malformedToolMode             = "bad-tool"
	malformedResourceURIMode      = "bad-resource-uri"
	malformedResourceTemplateMode = "bad-resource-template"
)

// TestProxyToolsPanicsOnMalformedTool ensures the stdio2http proxy converts the
// SDK's AddTool panic (triggered by a non-object inputSchema) into a clean
// error returned from runStdio2HTTPProxy, rather than crashing the process
// with an unrecovered panic.
func TestProxyToolsPanicsOnMalformedTool(t *testing.T) {
	malformedStdioProxyTest(t, malformedToolMode, `input schema must have type "object"`)
}

// TestProxyResourcesPanicsOnBadURI ensures the proxy converts the AddResource
// panic (triggered by a resource URI that net/url rejects) into a clean error.
func TestProxyResourcesPanicsOnBadURI(t *testing.T) {
	malformedStdioProxyTest(t, malformedResourceURIMode, "invalid control character in URL")
}

// TestProxyResourceTemplatePanicsOnBadTemplate ensures the proxy converts the
// AddResourceTemplate panic (triggered by an unbalanced URI template brace)
// into a clean error.
func TestProxyResourceTemplatePanicsOnBadTemplate(t *testing.T) {
	malformedStdioProxyTest(t, malformedResourceTemplateMode, "incomplete expression")
}

// malformedStdioProxyTest forks the test binary as a hand-rolled stdio MCP
// server (selected by mode) and verifies runStdio2HTTPProxy returns an error
// whose message names the offending feature (wantSubstr) instead of panicking.
func malformedStdioProxyTest(t *testing.T, mode, wantSubstr string) {
	t.Setenv(runMalformedStdioServerEnv, mode)
	cmd, err := os.Executable()
	require.NoError(t, err)

	logger := slog.New(slog.DiscardHandler)
	_, err = runStdio2HTTPProxy(t.Context(), logger, "malformed-stdio", cmd)
	require.Error(t, err)
	require.Contains(t, err.Error(), wantSubstr)
}

// TestSafeAddResourcePreservesURLError verifies safeAddResource returns the
// AddResource panic as a wrapped error whose chain still matches *url.Error,
// so callers can use errors.As across the recovered boundary.
func TestSafeAddResourcePreservesURLError(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	resource := &mcp.Resource{Name: "bad", URI: "file:///bad\nuri"}
	handler := func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return nil, nil
	}
	err := safeAddResource(server, resource, handler)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid control character in URL")
	var urlErr *url.Error
	require.ErrorAs(t, err, &urlErr)
}

// runMalformedStdioServer is a hand-rolled JSON-RPC 2.0 stdio MCP server that
// advertises a single malformed feature (selected by mode) so the stdio2http
// proxy exercises the SDK's panic path. It cannot use the SDK's helpers because
// they validate (and would reject) the malformed payload server-side before it
// reaches the wire. This runs in a subprocess via TestMain.
func runMalformedStdioServer(mode string) {
	serveMalformedStdio(os.Stdin, os.Stdout, mode)
}

// jsonrpcRequest is the subset of the JSON-RPC 2.0 wire format needed to
// distinguish requests (id present) from notifications (id absent).
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonrpcResponse is the JSON-RPC 2.0 response shape written by the malformed
// stdio server.
type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// serveMalformedStdio reads JSON-RPC messages from r and writes responses to w,
// advertising the malformed feature indicated by mode.
func serveMalformedStdio(r io.Reader, w io.Writer, mode string) {
	dec := json.NewDecoder(r)
	enc := json.NewEncoder(w)
	for {
		var req jsonrpcRequest
		if err := dec.Decode(&req); err != nil {
			return
		}
		// Notifications carry no id and expect no response.
		if len(req.ID) == 0 {
			continue
		}
		resp := malformedStdioResponse(req.Method, mode)
		resp.ID = req.ID
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

// malformedStdioResponse builds the JSON-RPC response for the given method:
//
//   - server/discover is rejected with method-not-found so the client falls
//     back to the legacy initialize handshake (which is simpler to satisfy).
//   - initialize advertises only the capability relevant to mode.
//   - the listing RPC (tools/list, resources/list, resources/templates/list)
//     returns the single malformed feature for mode (or an empty list where
//     needed to advance to the next listing RPC).
func malformedStdioResponse(method, mode string) jsonrpcResponse {
	resp := jsonrpcResponse{JSONRPC: "2.0"}
	switch method {
	case "server/discover":
		resp.Error = &jsonrpcError{Code: -32601, Message: "method not found"}
	case "initialize":
		caps := map[string]any{}
		switch mode {
		case malformedToolMode:
			caps["tools"] = map[string]any{}
		case malformedResourceURIMode, malformedResourceTemplateMode:
			caps["resources"] = map[string]any{}
		}
		resp.Result = map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    caps,
			"serverInfo":      map[string]any{"name": "malformed-stdio", "version": "test"},
		}
	case "tools/list":
		resp.Result = map[string]any{
			"tools": []map[string]any{
				{"name": "bad", "inputSchema": map[string]any{"type": "string"}},
			},
		}
	case "resources/list":
		resources := []map[string]any{}
		if mode == malformedResourceURIMode {
			resources = append(resources, map[string]any{"name": "bad", "uri": "file:///bad\nuri"})
		}
		resp.Result = map[string]any{"resources": resources}
	case "resources/templates/list":
		resp.Result = map[string]any{
			"resourceTemplates": []map[string]any{
				{"name": "bad", "uriTemplate": "{path"},
			},
		}
	case "ping":
		resp.Result = map[string]any{}
	default:
		resp.Error = &jsonrpcError{Code: -32601, Message: "method not found"}
	}
	return resp
}
