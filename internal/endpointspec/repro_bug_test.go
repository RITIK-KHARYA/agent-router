// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package endpointspec

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/internal/json"
	"github.com/envoyproxy/ai-gateway/internal/redaction"
)

// TestRedactResponsesInput_AssistantOutputTextSurvives guards the round-trip redaction of a
// /v1/responses `input` carrying an assistant turn with output_text array content — the shape
// that triggered the drop bug. The OpenAI Responses input union
// (ResponseInputItemUnionParam.UnmarshalJSON) routes assistant turns on the "role" discriminator,
// so "role" must be preserved by the redactor or the round-trip unmarshal in redactUnionField
// fails and the fail-safe drops the entire `input` from the redacted debug log.
func TestRedactResponsesInput_AssistantOutputTextSurvives(t *testing.T) {
	const marker = "assistant-output-text-marker"
	body := `{"model":"gpt-4o","input":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + marker + `"}]}]}`
	_, req, _, _, err := ResponsesEndpointSpec{}.ParseBody([]byte(body), false)
	require.NoError(t, err)

	redacted, err := ResponsesEndpointSpec{}.RedactSensitiveInfoFromRequest(req)
	require.NoError(t, err)

	// The whole input union must survive (not be dropped): the item list round-trips.
	require.NotNil(t, redacted.Input.OfInputItemList)
	require.Len(t, redacted.Input.OfInputItemList, 1)
	require.NotNil(t, redacted.Input.OfInputItemList[0].OfOutputMessage, "assistant turn routes to OfOutputMessage")

	out := mustMarshal(t, redacted)
	require.Contains(t, out, `"input"`, "input field present in debug log (not dropped)")
	require.Contains(t, out, `"role":"assistant"`, "role discriminator preserved")
	require.Contains(t, out, `"type":"message"`, "type discriminator preserved")
	require.Contains(t, out, `"type":"output_text"`, "nested content-block discriminator preserved")
	require.NotContains(t, out, marker, "user text must be redacted")
	require.Contains(t, out, "[REDACTED", "user text replaced with redaction placeholder")
	require.Contains(t, mustMarshal(t, req), marker, "original must not be mutated")
}

// TestRedactResponsesInput_MixedTurnsAllSurvive guards the collateral-drop symptom: when an
// assistant output_text turn sits alongside unrelated user turns, all turns must survive
// redaction. Before the fix the single failing assistant turn caused redactUnionField to drop
// the *entire* input slice (including the unrelated user turns) from the debug log. This body
// omits the explicit "type" field on each item, exercising arm A of the unmarshaler (the
// no-type compatibility branch that still routes on role).
func TestRedactResponsesInput_MixedTurnsAllSurvive(t *testing.T) {
	const m1, m2, m3 = "first-user-marker", "assistant-mid-marker", "third-user-marker"
	body := `{"model":"gpt-4o","input":[{"role":"user","content":"` + m1 + `"},{"role":"assistant","content":[{"type":"output_text","text":"` + m2 + `"}]},{"role":"user","content":"` + m3 + `"}]}`
	_, req, _, _, err := ResponsesEndpointSpec{}.ParseBody([]byte(body), false)
	require.NoError(t, err)

	redacted, err := ResponsesEndpointSpec{}.RedactSensitiveInfoFromRequest(req)
	require.NoError(t, err)

	require.NotNil(t, redacted.Input.OfInputItemList)
	require.Len(t, redacted.Input.OfInputItemList, 3, "all three turns survive redaction")

	out := mustMarshal(t, redacted)
	require.Contains(t, out, `"input"`)
	require.Contains(t, out, `"role":"user"`, "user role preserved")
	require.Contains(t, out, `"role":"assistant"`, "assistant role preserved")
	require.Contains(t, out, `"type":"output_text"`)
	require.NotContains(t, out, m1)
	require.NotContains(t, out, m2)
	require.NotContains(t, out, m3)
	require.Contains(t, out, "[REDACTED")
	require.Contains(t, mustMarshal(t, req), m1, "original must not be mutated")
}

// TestRedactResponsesInput_RoleIsLoadBearer proves "role" is the load-bearing discriminator for
// the round-trip: with {"type","role"} preserved the final unmarshal succeeds; with only "type"
// it fails. This prevents a future maintainer from "simplifying" the call site to drop "role"
// and silently reintroducing the drop bug.
func TestRedactResponsesInput_RoleIsLoadBearer(t *testing.T) {
	const marker = "roundtrip-output-text-marker"
	body := `{"model":"gpt-4o","input":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + marker + `"}]}]}`
	_, req, _, _, err := ResponsesEndpointSpec{}.ParseBody([]byte(body), false)
	require.NoError(t, err)

	t.Run("type and role preserved round-trips", func(t *testing.T) {
		out, finalErr := redactUnionFieldWithError(req.Input, "type", "role")
		require.NoError(t, finalErr, "final unmarshal must succeed when both type and role are preserved")
		require.NotNil(t, out.OfInputItemList)
		require.Len(t, out.OfInputItemList, 1)
		require.NotNil(t, out.OfInputItemList[0].OfOutputMessage, "assistant turn routed to OfOutputMessage")
		require.Equal(t, "assistant", out.OfInputItemList[0].OfOutputMessage.Role)
		require.Equal(t, "message", out.OfInputItemList[0].OfOutputMessage.Type)
	})

	t.Run("type only (role omitted) fails the round-trip", func(t *testing.T) {
		_, finalErr := redactUnionFieldWithError(req.Input, "type")
		require.Error(t, finalErr, "omitting role must fail the round-trip; this is the bug being fixed")
	})
}

// redactUnionFieldWithError mirrors redactUnionField but returns the final unmarshal error
// instead of discarding it, so tests can assert on the round-trip's success/failure directly.
func redactUnionFieldWithError[T any](in T, preserveKeys ...string) (out T, finalErr error) {
	b, err := json.Marshal(&in)
	if err != nil {
		return out, err
	}
	var generic any
	_ = json.Unmarshal(b, &generic)
	redacted, _ := json.Marshal(redaction.RedactJSONTree(generic, preserveKeys...))
	finalErr = json.Unmarshal(redacted, &out)
	return out, finalErr
}
