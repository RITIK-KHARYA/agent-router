// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package backendauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/envoyproxy/ai-gateway/internal/filterapi"
)

func TestNewGCPHandler(t *testing.T) {
	type wantError struct {
		check bool
		msg   string
	}
	testCases := []struct {
		name        string
		gcpAuth     *filterapi.GCPAuth
		wantHandler *gcpHandler
		wantError   wantError
	}{
		{
			name: "valid config with token",
			gcpAuth: &filterapi.GCPAuth{
				AccessToken: "test-token",
				Region:      "us-central1",
				ProjectName: "test-project",
			},
			wantHandler: &gcpHandler{
				gcpAccessToken: "test-token",
				region:         "us-central1",
				projectName:    "test-project",
			},
		},
		{
			name: "empty token uses ADC",
			gcpAuth: &filterapi.GCPAuth{
				Region:      "us-central1",
				ProjectName: "test-project",
			},
			wantError: wantError{check: true, msg: "failed to find GCP default credentials"},
		},
		{
			name:      "nil config",
			gcpAuth:   nil,
			wantError: wantError{check: true, msg: "GCP auth configuration cannot be nil"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := newGCPHandler(tc.gcpAuth)
			if err != nil {
				require.True(t, tc.wantError.check, "unexpected error: %v", err)
				require.ErrorContains(t, err, tc.wantError.msg)
			} else {
				require.NotNil(t, handler)
				if tc.wantHandler != nil {
					if d := cmp.Diff(tc.wantHandler, handler, cmp.AllowUnexported(gcpHandler{})); d != "" {
						t.Errorf("Handler mismatch (-want +got):\n%s", d)
					}
				}
			}
		})
	}
}

func TestGCPHandler_Do(t *testing.T) {
	handler := &gcpHandler{
		gcpAccessToken: "test-token",
		region:         "us-central1",
		projectName:    "test-project",
	}
	testCases := []struct {
		name             string
		handler          *gcpHandler
		requestHeaders   map[string]string
		wantPathValue    string
		wantPathRawValue []byte
	}{
		{
			name:    "basic headers update",
			handler: handler,
			requestHeaders: map[string]string{
				":path": "publishers/google/models/gemini-pro:generateContent",
			},
			wantPathValue: "/v1/projects/test-project/locations/us-central1/publishers/google/models/gemini-pro:generateContent",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			hdrs, err := tc.handler.Do(ctx, tc.requestHeaders, nil)
			require.NoError(t, err)

			expectedAuthHeader := fmt.Sprintf("Bearer %s", tc.handler.gcpAccessToken)

			hdrsMap := stringPairsToMap(hdrs)
			authValue, ok := hdrsMap["Authorization"]
			require.True(t, ok, "Authorization header not found in returned headers")
			require.Equal(t, expectedAuthHeader, authValue, "Authorization header value mismatch")

			pathValue, ok := hdrsMap[":path"]
			require.True(t, ok, ":path header not found in returned headers")
			require.Equal(t, tc.wantPathValue, pathValue, ":path header value mismatch")
		})
	}
}

type mockTokenSource struct {
	token string
	err   error
}

func (m *mockTokenSource) Token() (*oauth2.Token, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &oauth2.Token{
		AccessToken: m.token,
		Expiry:      time.Now().Add(time.Hour),
	}, nil
}

func TestGCPHandler_Do_WithTokenSource(t *testing.T) {
	handler := &gcpHandler{
		tokenSource: &mockTokenSource{token: "adc-token"},
		region:      "us-central1",
		projectName: "test-project",
	}

	requestHeaders := map[string]string{
		":path": "publishers/google/models/gemini-pro:generateContent",
	}

	hdrs, err := handler.Do(context.Background(), requestHeaders, nil)
	require.NoError(t, err)

	hdrsMap := stringPairsToMap(hdrs)
	require.Equal(t, "Bearer adc-token", hdrsMap["Authorization"])
	require.Equal(t, "/v1/projects/test-project/locations/us-central1/publishers/google/models/gemini-pro:generateContent", hdrsMap[":path"])
}

func TestGCPHandler_Do_TokenSourceError(t *testing.T) {
	handler := &gcpHandler{
		tokenSource: &mockTokenSource{err: fmt.Errorf("token refresh failed")},
		region:      "us-central1",
		projectName: "test-project",
	}

	requestHeaders := map[string]string{
		":path": "publishers/google/models/gemini-pro:generateContent",
	}

	_, err := handler.Do(context.Background(), requestHeaders, nil)
	require.ErrorContains(t, err, "failed to get GCP access token")
}

// TestNewGCPHandler_ADCSurvivesCanceledContext is a regression test for the
// config-reload ADC bug.
//
// On reload, newGCPHandler was previously invoked under the watcher's per-tick
// context, which is canceled the instant loadConfig returns
// (internal/filterapi/watcher.go). google.FindDefaultCredentials captured that
// context and bound it to every future token-refresh HTTP request, so before
// the fix every post-reload ADC refresh failed with "context canceled" and
// Do() could not obtain a token.
//
// The fix removed the ctx parameter entirely and derives a process-scoped
// context.Background() for the credentials lookup, so the token source is not
// bound to any short-lived caller context. This structurally prevents the bug
// (there is no caller ctx to capture) and this test exercises the real ADC code
// path end-to-end to confirm the STS token exchange still works.
func TestNewGCPHandler_ADCSurvivesCanceledContext(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fake-access-token","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer sts.Close()

	tmp := t.TempDir()
	subjectTokenFile := filepath.Join(tmp, "subject-token")
	if err := os.WriteFile(subjectTokenFile, []byte("dummy-subject-token"), 0o600); err != nil {
		t.Fatalf("write subject token: %v", err)
	}
	credsFile := filepath.Join(tmp, "adc.json")
	credsJSON := fmt.Sprintf(`{`+
		`"type":"external_account",`+
		`"audience":"//iam.googleapis.com/projects/x/locations/global/workloadIdentityPools/y/providers/z",`+
		`"subject_token_type":"urn:ietf:params:oauth:token-type:jwt",`+
		`"token_url":%q,`+
		`"credential_source":{"file":%q}`+
		`}`, sts.URL, subjectTokenFile)
	if err := os.WriteFile(credsFile, []byte(credsJSON), 0o600); err != nil {
		t.Fatalf("write credentials file: %v", err)
	}

	// Drive ADC resolution at our fake external_account credentials file. The
	// package-level gcpHTTPClient (initialized at package load) reaches 127.0.0.1
	// directly because http.ProxyFromEnvironment never proxies the loopback.
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credsFile)

	// Build the handler with no AccessToken so it takes the ADC branch. Even
	// though the watcher would have previously passed a per-tick context that
	// is canceled right after construction, the handler ignores any caller
	// context and the ADC token source refreshes against context.Background().
	handler, err := newGCPHandler(&filterapi.GCPAuth{
		Region:      "us-central1",
		ProjectName: "test-project",
	})
	require.NoError(t, err)

	// Simulate a per-request context arriving long after the (now-canceled)
	// watcher per-tick context. Do() must mint a fresh ADC access token.
	perTickCtx, cancel := context.WithCancel(context.Background())
	cancel()
	requestHeaders := map[string]string{":path": "publishers/google/models/gemini-pro:generateContent"}
	hdrs, err := handler.Do(perTickCtx, requestHeaders, nil)
	require.NoError(t, err)

	hdrsMap := stringPairsToMap(hdrs)
	require.Equal(t, "Bearer fake-access-token", hdrsMap["Authorization"])
	require.Equal(t,
		"/v1/projects/test-project/locations/us-central1/publishers/google/models/gemini-pro:generateContent",
		hdrsMap[":path"])
}
