// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package bodymutator

import (
	"fmt"
	"strings"

	"github.com/tidwall/sjson"

	"github.com/envoyproxy/ai-gateway/internal/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/json"
)

type BodyMutator struct {
	// originalBody is the original request body for retry scenarios
	originalBody []byte

	// bodyMutations is the body mutations to apply
	bodyMutations *filterapi.HTTPBodyMutation
}

func NewBodyMutator(bodyMutations *filterapi.HTTPBodyMutation, originalBody []byte) *BodyMutator {
	return &BodyMutator{
		originalBody:  originalBody,
		bodyMutations: bodyMutations,
	}
}

// HasMutations reports whether this BodyMutator was constructed with a
// non-nil HTTPBodyMutation config. When false, Mutate is a no-op that
// returns its input unchanged, so callers can short-circuit the
// upstream filter's body-replacement path entirely.
func (b *BodyMutator) HasMutations() bool {
	return b != nil && b.bodyMutations != nil
}

// isJSONValue checks if a string represents a JSON value (not a plain string)
func isJSONValue(value string) bool {
	value = strings.TrimSpace(value)

	// Check for quoted strings (JSON strings)
	if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") {
		return true
	}

	// Check for numbers (integers or floats)
	if value == "0" || value == "true" || value == "false" || value == "null" {
		return true
	}

	// Check for positive/negative numbers
	if len(value) > 0 {
		first := value[0]
		if (first >= '0' && first <= '9') || first == '-' || first == '+' {
			// Simple number check - contains only digits, dots, +, -, e, E
			isNumber := true
			for _, r := range value {
				if (r < '0' || r > '9') && r != '.' && r != '-' && r != '+' && r != 'e' && r != 'E' {
					isNumber = false
					break
				}
			}
			if isNumber {
				return true
			}
		}
	}

	// Check for objects or arrays
	if strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}") {
		return true
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		return true
	}

	// Default to plain string
	return false
}

// Mutate mutates the request body based on the body mutations.
func (b *BodyMutator) Mutate(requestBody []byte) ([]byte, error) {
	if b.bodyMutations == nil {
		return requestBody, nil
	}

	mutatedBody := requestBody
	var err error

	// Apply removals first
	if len(b.bodyMutations.Remove) > 0 {
		for _, fieldName := range b.bodyMutations.Remove {
			if fieldName != "" {
				// TODO optimize by using SetBytesOption to avoid underlying sjson copy.
				mutatedBody, err = sjson.DeleteBytes(mutatedBody, fieldName)
				if err != nil {
					return nil, fmt.Errorf("failed to remove field %s: %w", fieldName, err)
				}
			}
		}
	}

	// Apply sets
	if len(b.bodyMutations.Set) > 0 {
		for _, field := range b.bodyMutations.Set {
			if field.Path != "" {
				// Check value type to determine appropriate sjson method.
				// isJSONValue is a shallow prefix/suffix heuristic, so before embedding a
				// value as raw JSON via sjson.SetRawBytes (whose SetRaw* contract is that
				// the caller asserts the bytes are valid JSON), parse it to verify it really
				// is a JSON value. Without this, a misclassified value such as `{key: "v"}`
				// would be embedded verbatim and Mutate would return malformed JSON with
				// err == nil, which extproc would then forward upstream. Configuration-load
				// validation (filterapi.HTTPBodyMutation.Validate) rejects such values
				// before any request is processed; this is the request-path backstop that
				// guarantees no malformed JSON is ever emitted, regardless of how the
				// mutation was constructed.
				if isJSONValue(field.Value) {
					var jsonValue any
					if err = json.Unmarshal([]byte(field.Value), &jsonValue); err != nil {
						return nil, fmt.Errorf("failed to set field %s: value %q is not a valid JSON value: %w", field.Path, field.Value, err)
					}
					// Use SetRawBytes for JSON values (quoted strings, numbers, booleans, objects, arrays)
					mutatedBody, err = sjson.SetRawBytesOptions(mutatedBody, field.Path, []byte(field.Value), &sjson.Options{ReplaceInPlace: true})
				} else {
					// Use SetBytes for plain string values
					mutatedBody, err = sjson.SetBytesOptions(mutatedBody, field.Path, field.Value, &sjson.Options{ReplaceInPlace: true})
				}
				if err != nil {
					return nil, fmt.Errorf("failed to set field %s: %w", field.Path, err)
				}
			}
		}
	}

	return mutatedBody, nil
}
