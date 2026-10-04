// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package mcpproxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/internalapi"
	"github.com/envoyproxy/ai-gateway/internal/metrics"
	"github.com/envoyproxy/ai-gateway/internal/testing/testotel"
)

// TestResolveFailureRecordsErrorMetrics verifies that route/backend resolution
// failures (403 access-denied and 404 route-not-found) record exactly one sample
// in mcp.request.duration (tagged with error.type) and one increment in
// mcp.method.count (tagged status=error), mirroring the contract pinned by
// TestServePOST_EarlyReturnErrorMetrics for the other early-return guards.
//
// Before the fix, handlers set perBackendMetricsRecorded = true *before* calling
// resolve (newSession / resolveModernRouteBackends). When resolve failed, the
// deferred recordPOSTCompletion took its early-return branch and skipped
// RecordMethodErrorCount / RecordRequestErrorDuration, leaving the security-
// relevant 403/404 traffic invisible to error-rate dashboards.
func TestResolveFailureRecordsErrorMetrics(t *testing.T) {
	const initializeBody = `{"jsonrpc":"2.0","method":"initialize","id":1,` +
		`"params":{"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"test","version":"1.0"}}}`

	type tc struct {
		name        string
		method      string
		wantStatus  int
		wantErrType metrics.MCPErrorType
		// run drives the proxy; the recorder captures the HTTP response.
		run func(t *testing.T, proxy *mcpRequestContext, rr *httptest.ResponseRecorder)
	}

	// Modern fan-out methods that flow through resolveModernRouteBackends.
	modernMethods := []string{"server/discover", "tools/list", "resources/list", "resources/templates/list", "prompts/list"}

	var tests []tc
	// 403 access-denied on every modern handler.
	for _, method := range modernMethods {
		tests = append(tests, tc{
			name:        "modern/" + method + "/403_access_denied",
			method:      method,
			wantStatus:  http.StatusForbidden,
			wantErrType: metrics.MCPErrorInternal,
			run: func(t *testing.T, proxy *mcpRequestContext, rr *httptest.ResponseRecorder) {
				proxy.routes["test-route"].backendSelector = mustCompileBackendSelector(t, &filterapi.MCPRouteAuthorization{
					DefaultAction: filterapi.AuthorizationActionDeny,
				})
				r := newModernRequest(method)
				req := modernReq(t, method, nil)
				proxy.serveModernPOST(rr, r, req, time.Now())
			},
		})
	}
	// 404 route-not-found on every modern handler.
	for _, method := range modernMethods {
		tests = append(tests, tc{
			name:        "modern/" + method + "/404_route_not_found",
			method:      method,
			wantStatus:  http.StatusNotFound,
			wantErrType: metrics.MCPErrorInvalidParam,
			run: func(t *testing.T, proxy *mcpRequestContext, rr *httptest.ResponseRecorder) {
				r := newModernRequest(method)
				r.Header.Set(internalapi.MCPRouteHeader, "does-not-exist")
				req := modernReq(t, method, nil)
				proxy.serveModernPOST(rr, r, req, time.Now())
			},
		})
	}
	// Legacy initialize: 403 access-denied and 500 unknown-route (route absence
	// surfaces as 500, not 404, on the legacy path).
	tests = append(tests,
		tc{
			name:        "legacy/initialize/403_access_denied",
			method:      "initialize",
			wantStatus:  http.StatusForbidden,
			wantErrType: metrics.MCPErrorInternal,
			run: func(t *testing.T, proxy *mcpRequestContext, rr *httptest.ResponseRecorder) {
				proxy.routes["test-route"].backendSelector = mustCompileBackendSelector(t, &filterapi.MCPRouteAuthorization{
					DefaultAction: filterapi.AuthorizationActionDeny,
				})
				r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initializeBody))
				r.Header.Set(internalapi.MCPRouteHeader, "test-route")
				proxy.servePOST(rr, r)
			},
		},
		tc{
			name:        "legacy/initialize/500_unknown_route",
			method:      "initialize",
			wantStatus:  http.StatusInternalServerError,
			wantErrType: metrics.MCPErrorInternal,
			run: func(_ *testing.T, proxy *mcpRequestContext, rr *httptest.ResponseRecorder) {
				r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initializeBody))
				r.Header.Set(internalapi.MCPRouteHeader, "does-not-exist")
				proxy.servePOST(rr, r)
			},
		},
	)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := sdkmetric.NewManualReader()
			proxy := newTestMCPProxyWithOTEL(mr, noopTracer)
			t.Cleanup(func() {
				if err := mr.Shutdown(t.Context()); err != nil {
					t.Logf("failed to shutdown manual reader: %v", err)
				}
			})

			rr := httptest.NewRecorder()
			tt.run(t, proxy, rr)

			require.Equal(t, tt.wantStatus, rr.Code, "body: %s", rr.Body.String())

			// mcp.request.duration must have exactly one sample tagged with the
			// expected error.type.
			count, sum := testotel.GetHistogramValues(t, mr, "mcp.request.duration", attribute.NewSet(
				attribute.String("error.type", string(tt.wantErrType))))
			require.Equal(t, 1, int(count), // nolint: gosec
				"expected 1 sample in mcp.request.duration for error.type=%s, got %d", tt.wantErrType, count)
			require.Greater(t, sum, 0.0)

			// mcp.method.count must have exactly one increment tagged status=error
			// for the resolved method.
			methodCount := testotel.GetCounterValue(t, mr, "mcp.method.count", attribute.NewSet(
				attribute.String("mcp.method.name", tt.method),
				attribute.String("status", string(metrics.MCPStatusError))))
			require.Equal(t, 1, int(methodCount), // nolint: gosec
				"expected mcp.method.count=1 for %s error, got %d", tt.method, methodCount)

			// The resolve-failure path must NOT have set perBackendMetricsRecorded;
			// the generic completion branch handled the metrics.
			require.False(t, proxy.perBackendMetricsRecorded,
				"perBackendMetricsRecorded must be false on resolve failure so gateway-level error metrics are recorded")
		})
	}
}

// TestInitializeSuccessStillSetsPerBackendMetricsFlag verifies the legacy
// initialize success path still sets perBackendMetricsRecorded after the fix
// moved it past newSession.
func TestInitializeSuccessStillSetsPerBackendMetricsFlag(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(sessionIDHeader) == "" {
			w.Header().Set(sessionIDHeader, "test-session-123")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(validInitializeResponse))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()

	proxy := newTestMCPProxy()
	proxy.backendListenerAddr = backend.URL

	id, err := jsonrpc.MakeID("1")
	require.NoError(t, err)
	initReq := &jsonrpc.Request{Method: "initialize", ID: id, Params: []byte(`{"protocolVersion":"2024-11-05"}`)}
	body, err := jsonrpc.EncodeMessage(initReq)
	require.NoError(t, err)

	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	r.Header.Set(internalapi.MCPRouteHeader, "test-route")
	rr := httptest.NewRecorder()

	proxy.servePOST(rr, r)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.True(t, proxy.perBackendMetricsRecorded,
		"perBackendMetricsRecorded must be set after a successful initialize so per-backend metrics are not double-counted")
}
