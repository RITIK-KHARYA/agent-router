// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/envoyproxy/ai-gateway/internal/autoconfig"
)

// proxyStdioMCPServers runs the configured stdio MCP servers and starts a Streamable HTTP proxy
// for each, updating the MCPServers in place.
func proxyStdioMCPServers(ctx context.Context, logger *slog.Logger, mcpServers *autoconfig.MCPServers) error {
	if mcpServers == nil {
		return nil
	}
	for name, mcpServer := range mcpServers.McpServers {
		if mcpServer.Command != "" {
			address, err := runStdio2HTTPProxy(ctx, logger, name, mcpServer.Command, mcpServer.Args...)
			if err != nil {
				return err
			}
			mcpServers.McpServers[name] = autoconfig.MCPServer{
				Type:         "http",
				URL:          address,
				Headers:      mcpServer.Headers,
				IncludeTools: mcpServer.IncludeTools,
			}
		}
	}
	return nil
}

// runStdio2HTTPProxy runs a Streamable HTTP MCP proxy that connects to a stdio MCP server.
// It starts the command, connects to its stdio as an MCP transport, and
// exposes a Streamable HTTP server that proxies requests to the stdio MCP session.
func runStdio2HTTPProxy(ctx context.Context, logger *slog.Logger, name, command string, args ...string) (string, error) {
	// Initialize the command to run the stdio MCP server.
	cmd := exec.Command(command, args...)
	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio2http-" + name}, nil)
	// This will start the configured command in the background and connect to its
	// stdio as an MCP transport.
	logger.Info("starting stdio2http MCP proxy command", "name", name, "command", command, "args", args)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return "", fmt.Errorf("running the %s stdio2http proxy command: %w", name, err)
	}

	// Create an HTTP server that proxies requests to the MCP session.
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "stdio2http-" + name}, nil)
	if err = errors.Join(
		proxyTools(ctx, cs, mcpServer),
		proxyResources(ctx, cs, mcpServer),
		proxyPrompts(ctx, cs, mcpServer),
	); err != nil {
		// Tear down the stdio session we opened; otherwise the spawned command
		// would outlive this failed startup.
		_ = cs.Close()
		return "", fmt.Errorf("proxying features for the %s stdio2http proxy: %w", name, err)
	}

	// Find a free port to listen on.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("getting a free port for the %s stdio2http proxy: %w", name, err)
	}
	mcpAddress := fmt.Sprintf("http://localhost:%d/mcp", listener.Addr().(*net.TCPAddr).Port)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer }, nil)
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 120 * time.Second,
		WriteTimeout:      120 * time.Second,
	}

	go func() {
		logger.Info("starting stdio2http MCP proxy", "name", name, "address", listener.Addr().String())
		if serverErr := server.Serve(listener); serverErr != nil && !errors.Is(serverErr, http.ErrServerClosed) {
			logger.Error("stdio2http MCP proxy error", "name", name, "error", serverErr)
		}
	}()

	go func() {
		<-ctx.Done()
		logger.Info("shutting down stdio2http MCP proxy", "name", name)
		// Terminate the command process.
		if err = cs.Close(); err != nil {
			logger.Error("stdio2http MCP proxy command shutdown error", "name", name, "error", err)
		}
		// Shutdown the HTTP server.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdownCtx); err != nil {
			logger.Error("stdio2http MCP proxy server shutdown error", "name", name, "error", err)
		}
	}()

	return mcpAddress, nil
}

// proxyTools proxies tool calls to the stdio MCP client session.
func proxyTools(ctx context.Context, cs *mcp.ClientSession, server *mcp.Server) error {
	if cs.InitializeResult().Capabilities.Tools == nil {
		return nil
	}

	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return err
		}
		handler := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return cs.CallTool(ctx, &mcp.CallToolParams{
				Meta:      req.Params.Meta,
				Name:      req.Params.Name,
				Arguments: req.Params.Arguments,
			})
		}
		if err := safeAddTool(server, tool, handler); err != nil {
			return err
		}
	}
	return nil
}

// proxyResources proxies resource requests to the stdio MCP client session.
func proxyResources(ctx context.Context, cs *mcp.ClientSession, server *mcp.Server) error {
	if cs.InitializeResult().Capabilities.Resources == nil {
		return nil
	}

	for resource, err := range cs.Resources(ctx, nil) {
		if err != nil {
			return err
		}
		handler := func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return cs.ReadResource(ctx, &mcp.ReadResourceParams{
				Meta: req.Params.Meta,
				URI:  req.Params.URI,
			})
		}
		if err := safeAddResource(server, resource, handler); err != nil {
			return err
		}
	}
	for template, err := range cs.ResourceTemplates(ctx, nil) {
		if err != nil {
			return err
		}
		handler := func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return cs.ReadResource(ctx, &mcp.ReadResourceParams{
				Meta: req.Params.Meta,
				URI:  req.Params.URI,
			})
		}
		if err := safeAddResourceTemplate(server, template, handler); err != nil {
			return err
		}
	}
	return nil
}

// proxyPrompts proxies prompt requests to the stdio MCP client session.
func proxyPrompts(ctx context.Context, cs *mcp.ClientSession, server *mcp.Server) error {
	if cs.InitializeResult().Capabilities.Prompts == nil {
		return nil
	}

	for prompt, err := range cs.Prompts(ctx, nil) {
		if err != nil {
			return err
		}
		server.AddPrompt(prompt, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return cs.GetPrompt(ctx, &mcp.GetPromptParams{
				Meta:      req.Params.Meta,
				Name:      req.Params.Name,
				Arguments: req.Params.Arguments,
			})
		})
	}
	return nil
}

// safeAddTool wraps (*mcp.Server).AddTool, which panics by design when tool
// invariants are violated (for example a missing or non-object inputSchema).
// The panic is recovered and returned as an error so that a spec-violating
// upstream stdio MCP server fails startup through the project's normal
// error-return path instead of crashing the aigw process with an unrecovered
// panic.
func safeAddTool(server *mcp.Server, tool *mcp.Tool, handler mcp.ToolHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("adding tool %q: %w", tool.Name, recoveredError(r))
		}
	}()
	server.AddTool(tool, handler)
	return nil
}

// safeAddResource wraps (*mcp.Server).AddResource, which panics by design when
// the resource URI is malformed. The panic is recovered and returned as an
// error; see [safeAddTool] for the rationale.
func safeAddResource(server *mcp.Server, resource *mcp.Resource, handler mcp.ResourceHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("adding resource %q: %w", resource.URI, recoveredError(r))
		}
	}()
	server.AddResource(resource, handler)
	return nil
}

// safeAddResourceTemplate wraps (*mcp.Server).AddResourceTemplate, which panics
// by design when the URI template is malformed. The panic is recovered and
// returned as an error; see [safeAddTool] for the rationale.
func safeAddResourceTemplate(server *mcp.Server, template *mcp.ResourceTemplate, handler mcp.ResourceHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("adding resource template %q: %w", template.URITemplate, recoveredError(r))
		}
	}()
	server.AddResourceTemplate(template, handler)
	return nil
}

// recoveredError converts a recover()'d panic value into an error, preserving
// the original error type when the panic value implements error so that
// errors.Is/errors.As still work across the recovered boundary.
func recoveredError(r any) error {
	if e, ok := r.(error); ok {
		return e
	}
	return fmt.Errorf("%v", r)
}
