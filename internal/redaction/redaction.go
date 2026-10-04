// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Package redaction provides utilities for redacting sensitive information
// from requests and responses for safe debug logging.
package redaction

import (
	"crypto/sha256"
	"fmt"
)

// ComputeContentHash computes a cryptographic hash for content uniqueness tracking.
// This hash is used for debugging purposes and unique content detection, particularly for:
// - Tracking cache hits/misses by correlating identical content across requests
// - Identifying duplicate or similar requests without exposing actual content
// - Debugging issues by matching redacted logs to specific content patterns
// - Unique detection of content for deduplication and analysis
//
// We use SHA256 for:
// - Strong collision resistance for reliable unique detection
// - Cryptographic properties suitable for content identification
// - Standard hash function widely used for content addressing
//
// Returns a 16-character hex string (first 64 bits of SHA256 hash) for compact representation
// while maintaining strong collision resistance.
func ComputeContentHash(s string) string {
	hash := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", hash)[:16]
}

// RedactString replaces sensitive string content with a placeholder containing length and hash.
// The hash allows correlating logs with specific content for debugging without exposing
// the actual sensitive data.
//
// Format: [REDACTED LENGTH=n HASH=xxxxxxxx]
//
// Example: "secret API key 12345" becomes "[REDACTED LENGTH=19 HASH=a3f5e8c2]"
func RedactString(s string) string {
	if s == "" {
		return ""
	}
	hash := ComputeContentHash(s)
	return fmt.Sprintf("[REDACTED LENGTH=%d HASH=%s]", len(s), hash)
}

// RedactJSONTree redacts every string leaf in a JSON-compatible value, returning a new value.
//
// It walks the standard JSON tree shapes produced by encoding/json's default unmarshaler into
// `any`: string, []any, and map[string]any, replacing each string leaf with the [REDACTED …]
// placeholder from [RedactString]. Numbers, bools, and nil are kept as-is.
//
// preserveKeys names JSON object keys whose values are preserved verbatim instead of redacted.
// In the request schemas redacted here, such keys hold JSON-union discriminator or other
// non-sensitive structural-enum values (e.g. "type" with values like "message", "text",
// "image", "tool_use", or "role" with values like "user"/"assistant"), not user data. Redacting
// a discriminator value would make the redacted tree fail to round-trip back into the typed
// union during redaction, and these values carry no sensitive content. Which keys are
// discriminators depends on the typed union the caller round-trips into, so the preserved set
// is caller-supplied: a caller whose T discriminates only on "type" passes just "type"; a caller
// whose T also discriminates on "role" (e.g. the OpenAI Responses `input` union, whose items
// route assistant turns on role) passes both "type" and "role"; a caller that does not
// round-trip the result into a typed union passes nothing and over-redacts.
//
// It is intended for endpoint request fields whose values are untyped unions populated by custom
// unmarshalers (e.g. prompt/input content), so that no sensitive nested string is missed when
// redacting for debug logs. Non-sensitive structural strings not named in preserveKeys may be
// redacted — this over-redaction is acceptable for debug logging when the caller does not need
// to round-trip the result into an enum-discriminated typed union; a round-tripping caller must
// name every discriminator of T in preserveKeys, or the re-unmarshal will drop the field from
// the debug log entirely (worse for the redactor's stated purpose than over-redacting it).
//
// RedactJSONTree does not mutate its input; it returns a new tree (slices/maps are copied as it
// descends).
func RedactJSONTree(v any, preserveKeys ...string) any {
	return redactJSONTree(v, preserveKeySet(preserveKeys))
}

// redactJSONTree is the recursive core of [RedactJSONTree]. The preserve set is built once at
// the top level and threaded down so the set is not re-allocated at every level of the tree.
func redactJSONTree(v any, preserved map[string]struct{}) any {
	switch val := v.(type) {
	case string:
		return RedactString(val)
	case []any:
		out := make([]any, len(val))
		for i, e := range val {
			out[i] = redactJSONTree(e, preserved)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, e := range val {
			// Keys are structural (field/type names), not user data — keep them.
			// A key named in `preserved` holds a non-sensitive discriminator or
			// structural-enum value that must survive verbatim so the redacted tree
			// still round-trips into the caller's typed union — keep its value too.
			if _, keep := preserved[k]; keep {
				out[k] = e
				continue
			}
			out[k] = redactJSONTree(e, preserved)
		}
		return out
	default:
		// Numbers, bools, nil, and any other concrete type are left unchanged.
		return v
	}
}

// preserveKeySet builds a set for O(1) membership checks during redaction.
func preserveKeySet(keys []string) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}
