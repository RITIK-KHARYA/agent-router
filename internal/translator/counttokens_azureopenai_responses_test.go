// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	"github.com/envoyproxy/ai-gateway/internal/json"
)

func TestResponsesInputTokensOpenAIToAzure_RequestBody(t *testing.T) {
	for _, tc := range []struct {
		name              string
		apiVersion        string
		original          []byte
		body              openai.ResponseRequest
		forceBodyMutation bool
		modelNameOverride string
		expPathContains   []string
	}{
		{
			name:       "basic path with api-version",
			apiVersion: "2025-01-01-preview",
			original:   []byte(`{"model":"gpt-4.1","input":"hello"}`),
			body:       openai.ResponseRequest{Model: "gpt-4.1"},
			expPathContains: []string{
				"responses/input_tokens",
				"api-version=2025-01-01-preview",
			},
		},
		{
			name:              "model override",
			apiVersion:        "2025-04-01-preview",
			original:          []byte(`{"model":"gpt-4.1","input":"hello"}`),
			body:              openai.ResponseRequest{Model: "gpt-4.1"},
			modelNameOverride: "gpt-4.1-2025-04-14",
			expPathContains: []string{
				"responses/input_tokens",
				"api-version=2025-04-01-preview",
			},
		},
		{
			name:              "force body mutation without model override",
			apiVersion:        "2025-01-01-preview",
			original:          []byte(`{"model":"gpt-4.1","input":"hello"}`),
			body:              openai.ResponseRequest{Model: "gpt-4.1"},
			forceBodyMutation: true,
			expPathContains: []string{
				"responses/input_tokens",
				"api-version=2025-01-01-preview",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewResponsesInputTokensOpenAIToAzureOpenAITranslator(tc.apiVersion, tc.modelNameOverride)
			require.NotNil(t, tr)

			headerMutation, bodyMutation, err := tr.RequestBody(tc.original, &tc.body, tc.forceBodyMutation)
			require.NoError(t, err)
			require.NotNil(t, headerMutation)

			pathHeader := headerMutation[0]
			require.Equal(t, pathHeaderName, pathHeader.Key())
			for _, s := range tc.expPathContains {
				assert.Contains(t, pathHeader.Value(), s)
			}

			if tc.modelNameOverride != "" {
				require.NotNil(t, bodyMutation)
				var parsed map[string]any
				require.NoError(t, json.Unmarshal(bodyMutation, &parsed))
				assert.Equal(t, tc.modelNameOverride, parsed["model"])
			}

			if tc.forceBodyMutation && tc.modelNameOverride == "" {
				require.Equal(t, tc.original, bodyMutation)
			}
		})
	}
}

// TestResponsesInputTokensOpenAIToAzure_EmptyAPIVersion verifies that the Azure OpenAI
// input-tokens translator fails fast when constructed without an api-version, rather
// than silently emitting a malformed "?api-version=" query parameter.
func TestResponsesInputTokensOpenAIToAzure_EmptyAPIVersion(t *testing.T) {
	tr := NewResponsesInputTokensOpenAIToAzureOpenAITranslator("", "")
	require.NotNil(t, tr)
	hm, bm, err := tr.RequestBody([]byte(`{"model":"gpt-4.1","input":"hi"}`), &openai.ResponseRequest{Model: "gpt-4.1"}, false)
	require.ErrorIs(t, err, errAzureOpenAIAPIVersionRequired)
	require.ErrorContains(t, err, "api version")
	require.Nil(t, hm)
	require.Nil(t, bm)
}
