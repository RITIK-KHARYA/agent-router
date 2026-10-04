// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package mainlib

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/json"
	internaltesting "github.com/envoyproxy/ai-gateway/internal/testing"
)

func Test_newLogHandler(t *testing.T) {
	t.Run("json emits parseable records", func(t *testing.T) {
		var buf bytes.Buffer
		slog.New(newLogHandler(&buf, slog.LevelInfo, internalapi.LogFormatJSON)).
			Info("starting external processor", slog.String("address", ":1063"))

		var record map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
		require.Equal(t, "starting external processor", record["msg"])
		require.Equal(t, ":1063", record["address"])
		require.Equal(t, "INFO", record["level"])
	})

	t.Run("text is unchanged and is the fallback", func(t *testing.T) {
		for _, format := range []string{internalapi.LogFormatText, ""} {
			var buf bytes.Buffer
			slog.New(newLogHandler(&buf, slog.LevelInfo, format)).
				Info("starting external processor", slog.String("address", ":1063"))

			require.Contains(t, buf.String(), `msg="starting external processor" address=:1063`)
			var record map[string]any
			require.Error(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record), "text output must not be JSON")
		}
	})

	t.Run("level is honored", func(t *testing.T) {
		var buf bytes.Buffer
		slog.New(newLogHandler(&buf, slog.LevelWarn, internalapi.LogFormatJSON)).Info("filtered out")
		require.Empty(t, buf.String())
	})
}

func Test_parseAndValidateFlags(t *testing.T) {
	t.Run("ok extProcFlags", func(t *testing.T) {
		for _, tc := range []struct {
			name             string
			args             []string
			configBundlePath string
			addr             string
			rootPrefix       string
			logLevel         slog.Level
			logFormat        string
			enableRedaction  bool
		}{
			{
				name:             "minimal extProcFlags",
				args:             []string{"-configBundlePath", "/path/to/config-bundle"},
				configBundlePath: "/path/to/config-bundle",
				addr:             ":1063",
				rootPrefix:       "/",
				logLevel:         slog.LevelInfo,
				logFormat:        "text",
				enableRedaction:  false,
			},
			{
				name:             "log format json",
				args:             []string{"-configBundlePath", "/path/to/config-bundle", "-logFormat", "json"},
				configBundlePath: "/path/to/config-bundle",
				addr:             ":1063",
				rootPrefix:       "/",
				logLevel:         slog.LevelInfo,
				logFormat:        "json",
				enableRedaction:  false,
			},
			{
				name:             "custom addr",
				args:             []string{"-configBundlePath", "/path/to/config-bundle", "-extProcAddr", "unix:///tmp/ext_proc.sock"},
				configBundlePath: "/path/to/config-bundle",
				addr:             "unix:///tmp/ext_proc.sock",
				rootPrefix:       "/",
				logLevel:         slog.LevelInfo,
				enableRedaction:  false,
			},
			{
				name:             "log level debug",
				args:             []string{"-configBundlePath", "/path/to/config-bundle", "-logLevel", "debug"},
				configBundlePath: "/path/to/config-bundle",
				addr:             ":1063",
				rootPrefix:       "/",
				logLevel:         slog.LevelDebug,
				enableRedaction:  false,
			},
			{
				name:             "log level debug with redaction enabled",
				args:             []string{"-configBundlePath", "/path/to/config-bundle", "-logLevel", "debug", "-enableRedaction"},
				configBundlePath: "/path/to/config-bundle",
				addr:             ":1063",
				rootPrefix:       "/",
				logLevel:         slog.LevelDebug,
				enableRedaction:  true,
			},
			{
				name:             "log level warn",
				args:             []string{"-configBundlePath", "/path/to/config-bundle", "-logLevel", "warn"},
				configBundlePath: "/path/to/config-bundle",
				addr:             ":1063",
				rootPrefix:       "/",
				logLevel:         slog.LevelWarn,
				enableRedaction:  false,
			},
			{
				name:             "log level error",
				args:             []string{"-configBundlePath", "/path/to/config-bundle", "-logLevel", "error"},
				configBundlePath: "/path/to/config-bundle",
				addr:             ":1063",
				rootPrefix:       "/",
				logLevel:         slog.LevelError,
				enableRedaction:  false,
			},
			{
				name: "all extProcFlags",
				args: []string{
					"-configBundlePath", "/path/to/config-bundle",
					"-extProcAddr", "unix:///tmp/ext_proc.sock",
					"-logLevel", "debug",
					"-rootPrefix", "/foo/bar/",
				},
				configBundlePath: "/path/to/config-bundle",
				addr:             "unix:///tmp/ext_proc.sock",
				rootPrefix:       "/foo/bar/",
				logLevel:         slog.LevelDebug,
				enableRedaction:  false,
			},
			{
				name:             "with endpoint prefixes",
				args:             []string{"-configBundlePath", "/path/to/config-bundle", "-endpointPrefixes", "openai:/,cohere:/cohere,anthropic:/anthropic"},
				configBundlePath: "/path/to/config-bundle",
				addr:             ":1063",
				rootPrefix:       "/",
				logLevel:         slog.LevelInfo,
				enableRedaction:  false,
			},
			{
				name: "with metrics header mapping",
				args: []string{
					"-configBundlePath", "/path/to/config-bundle",
					"-metricsRequestHeaderAttributes", "x-tenant-id:tenant.id,x-tenant-id:tenant.id",
				},
				configBundlePath: "/path/to/config-bundle",
				rootPrefix:       "/",
				addr:             ":1063",
				logLevel:         slog.LevelInfo,
				enableRedaction:  false,
			},
			{
				name: "with base header mapping",
				args: []string{
					"-configBundlePath", "/path/to/config-bundle",
					"-metricsRequestHeaderAttributes", "x-team-id:team.id,x-user-id:user.id",
				},
				configBundlePath: "/path/to/config-bundle",
				rootPrefix:       "/",
				addr:             ":1063",
				logLevel:         slog.LevelInfo,
				enableRedaction:  false,
			},
			{
				name: "with tracing header attributes",
				args: []string{
					"-configBundlePath", "/path/to/config-bundle",
					"-spanRequestHeaderAttributes", "x-session-id:session.id,x-user-id:user.id",
				},
				configBundlePath: "/path/to/config-bundle",
				rootPrefix:       "/",
				addr:             ":1063",
				logLevel:         slog.LevelInfo,
				enableRedaction:  false,
			},
			{
				name: "with both metrics and tracing headers",
				args: []string{
					"-configBundlePath", "/path/to/config-bundle",
					"-metricsRequestHeaderAttributes", "x-user-id:user.id",
					"-spanRequestHeaderAttributes", "x-session-id:session.id",
				},
				configBundlePath: "/path/to/config-bundle",
				rootPrefix:       "/",
				addr:             ":1063",
				logLevel:         slog.LevelInfo,
				enableRedaction:  false,
			},
			{
				name:             "bundle path only",
				args:             []string{"-configBundlePath", "/path/to/config-bundle"},
				configBundlePath: "/path/to/config-bundle",
				rootPrefix:       "/",
				addr:             ":1063",
				logLevel:         slog.LevelInfo,
				enableRedaction:  false,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				flags, err := parseAndValidateFlags(tc.args)
				require.NoError(t, err)
				require.Equal(t, tc.configBundlePath, flags.configBundlePath)
				require.Equal(t, tc.addr, flags.extProcAddr)
				require.Equal(t, tc.logLevel, flags.logLevel)
				// Cases that do not care about the format expect the default.
				wantLogFormat := tc.logFormat
				if wantLogFormat == "" {
					wantLogFormat = "text"
				}
				require.Equal(t, wantLogFormat, flags.logFormat)
				require.Equal(t, tc.enableRedaction, flags.enableRedaction)
				require.Equal(t, tc.rootPrefix, flags.rootPrefix)
			})
		}
	})

	t.Run("invalid extProcFlags", func(t *testing.T) {
		tests := []struct {
			name          string
			args          []string
			expectedError string
		}{
			{
				name:          "invalid log level",
				args:          []string{"-logLevel", "invalid"},
				expectedError: "configBundlePath must be provided\nfailed to unmarshal log level: slog: level string \"invalid\": unknown name",
			},
			{
				name:          "invalid log format",
				args:          []string{"-configBundlePath", "/path/to/config-bundle", "-logFormat", "yaml"},
				expectedError: `invalid log format: "yaml", must be "text" or "json"`,
			},
			{
				name:          "invalid endpoint prefixes - unknown key",
				args:          []string{"-configBundlePath", "/path/to/config-bundle", "-endpointPrefixes", "foo:/x"},
				expectedError: "failed to parse endpoint prefixes: unknown endpointPrefixes key \"foo\" at position 1 (allowed: openai, cohere, anthropic, typesafe)",
			},
			{
				name:          "invalid endpoint prefixes - missing colon",
				args:          []string{"-configBundlePath", "/path/to/config-bundle", "-endpointPrefixes", "openai"},
				expectedError: "failed to parse endpoint prefixes: invalid endpointPrefixes pair at position 1: \"openai\" (expected format: key:value)",
			},
			{
				name:          "invalid tracing header attributes - missing colon",
				args:          []string{"-configBundlePath", "/path/to/config-bundle", "-spanRequestHeaderAttributes", "x-session-id"},
				expectedError: "failed to parse tracing header mapping: invalid header-attribute pair at position 1: \"x-session-id\" (expected format: header:attribute)",
			},
			{
				name:          "invalid tracing header attributes - empty header",
				args:          []string{"-configBundlePath", "/path/to/config-bundle", "-spanRequestHeaderAttributes", ":session.id"},
				expectedError: "failed to parse tracing header mapping: empty header or attribute at position 1: \":session.id\"",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, err := parseAndValidateFlags(tt.args)
				require.EqualError(t, err, tt.expectedError)
			})
		}
	})

	t.Run("legacy config path is rejected", func(t *testing.T) {
		_, err := parseAndValidateFlags([]string{"-configPath", "/path/to/config.yaml"})
		require.EqualError(t, err, "failed to parse extProcFlags: flag provided but not defined: -configPath")
	})
}

func TestListenAddress(t *testing.T) {
	unixPath := t.TempDir() + "/extproc.sock"
	// Create a stale file to ensure that removing the file works correctly.
	require.NoError(t, os.WriteFile(unixPath, []byte("stale socket"), 0o600))

	lis, err := listen(t.Context(), t.Name(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer lis.Close() //nolint:errcheck

	tests := []struct {
		addr        string
		wantNetwork string
		wantAddress string
	}{
		{lis.Addr().String(), "tcp", lis.Addr().String()},
		{"unix://" + unixPath, "unix", unixPath},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			network, address := listenAddress(tt.addr)
			require.Equal(t, tt.wantNetwork, network)
			require.Equal(t, tt.wantAddress, address)
		})
	}
	_, err = os.Stat(unixPath)
	require.ErrorIs(t, err, os.ErrNotExist, "expected the stale socket file to be removed")
}

// TestExtProcStartupMessage ensures other programs can rely on the startup message to STDERR.
func TestExtProcStartupMessage(t *testing.T) {
	// Create a temporary config bundle.
	tmpDir := t.TempDir()
	configRaw := []byte(`
version: dev
backends:
- name: openai
  schema:
    name: OpenAI
    version: v1
`)
	configBundlePath := filepath.Join(tmpDir, "config-bundle")
	part := filterapi.ConfigBundlePart{Name: "config", Path: filterapi.ConfigBundlePartPath(0), SizeBytes: len(configRaw)}
	partPath := filepath.Join(configBundlePath, filepath.FromSlash(part.Path))
	require.NoError(t, os.MkdirAll(filepath.Dir(partPath), 0o700))
	require.NoError(t, os.WriteFile(partPath, configRaw, 0o600))
	indexRaw, err := filterapi.MarshalConfigBundleIndex(&filterapi.ConfigBundleIndex{
		Checksum: filterapi.ConfigBundleChecksum(configRaw),
		Parts:    []filterapi.ConfigBundlePart{part},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(configBundlePath, filterapi.ConfigBundleIndexFileName), indexRaw, 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Create a pipe for stderr.
	stderrR, stderrW := io.Pipe()

	// Start a goroutine to scan stderr until it reaches "AI Gateway External Processor is ready" written by envoy.
	go func() {
		scanner := bufio.NewScanner(stderrR)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "AI Gateway External Processor is ready") {
				cancel() // interrupts extproc.
				return
			}
		}
	}()

	// UNIX doesn't like the long socket paths, so create a temp directory for the socket instead of t.TempDir.
	socketTempDir := "/tmp/" + uuid.NewString()
	t.Cleanup(func() { _ = os.RemoveAll(socketTempDir) })
	require.NoError(t, os.MkdirAll(socketTempDir, 0o700))
	socketPath := filepath.Join(socketTempDir, "mcp.sock")

	// Run ExtProc in a goroutine on ephemeral ports.
	errCh := make(chan error, 1)
	go func() {
		args := []string{
			"-configBundlePath", configBundlePath,
			"-extProcAddr", ":0",
			"-adminPort", "0",
			"-mcpAddr", "unix://" + socketPath,
			"-logLevel", "info",
		}
		errCh <- Main(ctx, args, stderrW)
	}()

	timeout, cancelTimeout := context.WithTimeout(t.Context(), time.Second*3)
	defer cancelTimeout()
	select {
	case <-ctx.Done():
	case <-timeout.Done():
		t.Fatal("timeout waiting for startup message")
	case err := <-errCh:
		require.NoError(t, err, "extproc exited with error before startup message")
	}
}

// readyWriter closes readyCh the first time it observes the extproc startup
// marker and discards all output. It avoids the io.Pipe + scanner goroutine used
// elsewhere so there is no reader goroutine to leak or drain.
type readyWriter struct {
	readyCh  chan struct{}
	once     sync.Once
	captureM sync.Mutex
	captured []byte
}

func (w *readyWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("AI Gateway External Processor is ready")) {
		w.once.Do(func() { close(w.readyCh) })
	}
	w.captureM.Lock()
	w.captured = append(w.captured, p...)
	w.captureM.Unlock()
	return len(p), nil
}

// capturedLines returns a snapshot of everything written so far.
func (w *readyWriter) capturedLines() []byte {
	w.captureM.Lock()
	defer w.captureM.Unlock()
	out := make([]byte, len(w.captured))
	copy(out, w.captured)
	return out
}

// TestMain_WaitsForShutdownFlush verifies that Main blocks until the shutdown
// goroutine finishes flushing the tracing/metrics exporters and draining the
// admin/MCP HTTP servers. Before the fix, Main returned the instant s.Serve
// returned (right as GracefulStop completed), so the process could exit and
// kill the detached shutdown goroutine mid-flush, dropping any buffered
// OTLP/console spans and metrics accumulated since the last periodic export.
//
// The test holds a connection to the MCP server open with a partial HTTP
// request, so the last step of the shutdown goroutine (mcpServer.Shutdown)
// blocks until the connection is released. If Main is synchronized on
// shutdownDone it must remain blocked; if not (the bug) it returns within a
// few ms of cancel(), before the goroutine reaches mcpServer.Shutdown.
func TestMain_WaitsForShutdownFlush(t *testing.T) {
	internaltesting.ClearTestEnv(t)

	// Build a minimal config bundle on disk.
	tmpDir := t.TempDir()
	configRaw := []byte(`
version: dev
backends:
- name: openai
  schema:
    name: OpenAI
    version: v1
`)
	configBundlePath := filepath.Join(tmpDir, "config-bundle")
	part := filterapi.ConfigBundlePart{Name: "config", Path: filterapi.ConfigBundlePartPath(0), SizeBytes: len(configRaw)}
	partPath := filepath.Join(configBundlePath, filepath.FromSlash(part.Path))
	require.NoError(t, os.MkdirAll(filepath.Dir(partPath), 0o700))
	require.NoError(t, os.WriteFile(partPath, configRaw, 0o600))
	indexRaw, err := filterapi.MarshalConfigBundleIndex(&filterapi.ConfigBundleIndex{
		Checksum: filterapi.ConfigBundleChecksum(configRaw),
		Parts:    []filterapi.ConfigBundlePart{part},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(configBundlePath, filterapi.ConfigBundleIndexFileName), indexRaw, 0o600))

	// Long UNIX socket paths fail on some platforms, so use /tmp/<uuid> as in
	// TestExtProcStartupMessage.
	socketTempDir := "/tmp/" + uuid.NewString()
	t.Cleanup(func() { _ = os.RemoveAll(socketTempDir) })
	require.NoError(t, os.MkdirAll(socketTempDir, 0o700))
	mcpSocketPath := filepath.Join(socketTempDir, "mcp.sock")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stderr := &readyWriter{readyCh: make(chan struct{})}

	errCh := make(chan error, 1)
	go func() {
		args := []string{
			"-configBundlePath", configBundlePath,
			"-extProcAddr", ":0",
			"-adminPort", "0",
			"-mcpAddr", "unix://" + mcpSocketPath,
			"-logLevel", "info",
		}
		errCh <- Main(ctx, args, stderr)
	}()

	select {
	case <-stderr.readyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for extproc startup message")
	}

	// Hold a connection to the MCP server open with a partial HTTP/1.1 request
	// (no terminating blank line). The server blocks reading headers, keeping the
	// connection in its active set, so mcpServer.Shutdown — the last step of the
	// shutdown goroutine — blocks until we release it. That keeps shutdownDone
	// from closing: a correctly synchronized Main must block, the buggy one does
	// not.
	mcpHold, err := net.DialTimeout("unix", mcpSocketPath, 2*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mcpHold.Close() })
	_, err = mcpHold.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n"))
	require.NoError(t, err)

	// Give the http.Server's accept loop time to register the held connection in
	// its active set before shutdown begins; otherwise Shutdown may not see it
	// and complete immediately, making the test flaky under -race.
	time.Sleep(100 * time.Millisecond)

	// Trigger graceful shutdown, mirroring a SIGTERM handler.
	cancel()

	// With the fix, Main is blocked on <-shutdownDone while the goroutine is
	// parked in mcpServer.Shutdown waiting on mcpHold. With the bug, Main has
	// already returned (right as GracefulStop completed, before the trailing
	// flush/drain work even started).
	select {
	case err := <-errCh:
		t.Fatalf("Main returned before shutdown flush/drain completed (synchronization missing): %v", err)
	case <-time.After(500 * time.Millisecond):
		// Expected: Main is still blocked, proving it waits for shutdown.
	}

	// Release the held MCP connection so mcpServer.Shutdown can finish; the rest
	// of the shutdown goroutine then completes and shutdownDone closes, letting
	// Main return.
	_ = mcpHold.Close()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for Main to return after releasing the MCP hold")
	}
}

// TestMain_WaitsForAdminDrain isolates G4 (admin HTTP server drain) by running
// without -mcpAddr (so mcpServer is nil) and holding a connection to the admin
// server with a partial HTTP request. The shutdown goroutine runs
// adminServer.Shutdown as its second step, so a correctly synchronized Main
// blocks until the held admin connection is released. The admin port is
// discovered by parsing the "starting admin server" slog line from stderr.
//
// This also doubles as a regression guard for G7 (graceful shutdown returns
// nil): the received err is asserted to be nil.
func TestMain_WaitsForAdminDrain(t *testing.T) {
	internaltesting.ClearTestEnv(t)

	tmpDir := t.TempDir()
	configRaw := []byte(`
version: dev
backends:
- name: openai
  schema:
    name: OpenAI
    version: v1
`)
	configBundlePath := filepath.Join(tmpDir, "config-bundle")
	part := filterapi.ConfigBundlePart{Name: "config", Path: filterapi.ConfigBundlePartPath(0), SizeBytes: len(configRaw)}
	partPath := filepath.Join(configBundlePath, filepath.FromSlash(part.Path))
	require.NoError(t, os.MkdirAll(filepath.Dir(partPath), 0o700))
	require.NoError(t, os.WriteFile(partPath, configRaw, 0o600))
	indexRaw, err := filterapi.MarshalConfigBundleIndex(&filterapi.ConfigBundleIndex{
		Checksum: filterapi.ConfigBundleChecksum(configRaw),
		Parts:    []filterapi.ConfigBundlePart{part},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(configBundlePath, filterapi.ConfigBundleIndexFileName), indexRaw, 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stderr := &readyWriter{readyCh: make(chan struct{})}

	errCh := make(chan error, 1)
	go func() {
		// No -mcpAddr: mcpServer is nil, so adminServer.Shutdown is the latest
		// controllable step the shutdown goroutine blocks on.
		args := []string{
			"-configBundlePath", configBundlePath,
			"-extProcAddr", ":0",
			"-adminPort", "0",
			"-logLevel", "info",
		}
		errCh <- Main(ctx, args, stderr)
	}()

	select {
	case <-stderr.readyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for extproc startup message")
	}

	// Discover the admin port from the "starting admin server" slog line:
	//   msg="starting admin server" address=127.0.0.1:PORT
	adminAddr := func() string {
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
			for _, line := range bytes.Split(stderr.capturedLines(), []byte("\n")) {
				if bytes.Contains(line, []byte("starting admin server")) {
					if idx := bytes.Index(line, []byte("address=")); idx >= 0 {
						return string(bytes.TrimSpace(line[idx+len("address="):]))
					}
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("could not find admin server address in stderr")
		return ""
	}()
	require.NotEmpty(t, adminAddr)

	// Hold a connection to the admin server with a partial HTTP/1.1 request so
	// adminServer.Shutdown blocks until we release it.
	adminHold, err := net.DialTimeout("tcp", adminAddr, 2*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = adminHold.Close() })
	_, err = adminHold.Write([]byte("GET /metrics HTTP/1.1\r\nHost: localhost\r\n"))
	require.NoError(t, err)

	// Give the http.Server's accept loop time to register the held connection in
	// its active set before shutdown begins; otherwise Shutdown may not see it
	// and complete immediately, making the test flaky under -race.
	time.Sleep(100 * time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		t.Fatalf("Main returned before admin server drain completed (synchronization missing): %v", err)
	case <-time.After(500 * time.Millisecond):
		// Expected: Main is still blocked on adminServer.Shutdown.
	}

	_ = adminHold.Close()

	select {
	case err := <-errCh:
		require.NoError(t, err, "graceful shutdown must return nil (G7)")
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for Main to return after releasing the admin hold")
	}
}
