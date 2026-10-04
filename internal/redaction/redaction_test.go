// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package redaction

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedactString(t *testing.T) {
	require.Empty(t, RedactString(""))
	out := RedactString("secret")
	require.Contains(t, out, "[REDACTED LENGTH=6 HASH=")
	// Deterministic for identical input.
	require.Equal(t, out, RedactString("secret"))
}

func TestRedactJSONTree(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		require.Equal(t, RedactString("hello"), RedactJSONTree("hello"))
		require.Empty(t, RedactJSONTree(""))
	})

	t.Run("non-string leaves unchanged", func(t *testing.T) {
		require.Equal(t, 42, RedactJSONTree(42))
		require.Equal(t, 3.14, RedactJSONTree(3.14))
		require.Equal(t, true, RedactJSONTree(true))
		require.Nil(t, RedactJSONTree(nil))
	})

	t.Run("slice of any", func(t *testing.T) {
		in := []any{"secret1", 7, []any{"nested", true}}
		out := RedactJSONTree(in)
		require.Equal(t, []any{RedactString("secret1"), 7, []any{RedactString("nested"), true}}, out)
		// Input not mutated.
		require.Equal(t, "secret1", in[0])
	})

	t.Run("map of any", func(t *testing.T) {
		in := map[string]any{"prompt": "secret", "n": 1, "nested": map[string]any{"text": "shh"}}
		out := RedactJSONTree(in)
		require.Equal(t, RedactString("secret"), out.(map[string]any)["prompt"])
		require.Equal(t, 1, out.(map[string]any)["n"])
		require.Equal(t, RedactString("shh"), out.(map[string]any)["nested"].(map[string]any)["text"])
		// Keys preserved.
		_, ok := out.(map[string]any)["prompt"]
		require.True(t, ok)
		// Input not mutated.
		require.Equal(t, "secret", in["prompt"])
	})

	t.Run("no string leaves", func(t *testing.T) {
		in := []any{1, true, nil, 2.5}
		require.Equal(t, in, RedactJSONTree(in))
	})

	t.Run("type discriminator preserved", func(t *testing.T) {
		// "type" values are JSON-union discriminators, not user data — they must be
		// preserved (when named in preserveKeys) so the redacted tree still round-trips
		// into typed unions.
		in := map[string]any{
			"type":    "message",
			"role":    "user",
			"content": "secret",
		}
		out := RedactJSONTree(in, "type").(map[string]any)
		require.Equal(t, "message", out["type"], "type discriminator preserved when named")
		require.Equal(t, RedactString("user"), out["role"], "unnamed non-type string redacted")
		require.Equal(t, RedactString("secret"), out["content"])
	})

	t.Run("no preserveKeys redacts every string leaf including type and role", func(t *testing.T) {
		// With no preserveKeys (the non-round-trip default) everything is over-redacted.
		in := map[string]any{
			"type":    "message",
			"role":    "user",
			"content": "secret",
		}
		out := RedactJSONTree(in).(map[string]any)
		require.Equal(t, RedactString("message"), out["type"], "type redacted when not preserved")
		require.Equal(t, RedactString("user"), out["role"], "role redacted when not preserved")
		require.Equal(t, RedactString("secret"), out["content"])
	})

	t.Run("preserveKeys type and role preserves both", func(t *testing.T) {
		// A round-tripping caller whose T discriminates on both "type" and "role"
		// (e.g. OpenAI Responses input items) must name both.
		in := map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": "secret",
		}
		out := RedactJSONTree(in, "type", "role").(map[string]any)
		require.Equal(t, "message", out["type"], "type preserved")
		require.Equal(t, "assistant", out["role"], "role preserved")
		require.Equal(t, RedactString("secret"), out["content"], "user content still redacted")
	})

	t.Run("preserveKeys role preserves role and redacts type", func(t *testing.T) {
		// preserveKeys is an exact-name set: a key not named is still redacted.
		in := map[string]any{"type": "message", "role": "user", "content": "secret"}
		out := RedactJSONTree(in, "role").(map[string]any)
		require.Equal(t, RedactString("message"), out["type"], "type redacted when not named")
		require.Equal(t, "user", out["role"], "role preserved when named")
		require.Equal(t, RedactString("secret"), out["content"])
	})

	t.Run("preserveKeys applies at every nesting level", func(t *testing.T) {
		// discriminators can appear at multiple depths (e.g. output_text content blocks);
		// preserveKeys must apply recursively, not only at the top level.
		in := map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": "secret"},
			},
		}
		out := RedactJSONTree(in, "type", "role").(map[string]any)
		require.Equal(t, "message", out["type"])
		require.Equal(t, "assistant", out["role"])
		block := out["content"].([]any)[0].(map[string]any)
		require.Equal(t, "output_text", block["type"], "nested type discriminator preserved")
		require.Equal(t, RedactString("secret"), block["text"], "nested user content redacted")
	})
}
