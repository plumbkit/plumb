package mcp

import (
	"bytes"
	"encoding/json"
	"sort"
)

// argidentity.go — the argument-carried form of the logical-agent identity.
//
// A client RUNTIME that can rewrite a tool call's input but not its `_meta`
// (Claude Code's PreToolUse hook is the concrete case: `updatedInput` replaces
// `tool_input`, and nothing a hook returns reaches the JSON-RPC envelope) has
// exactly one place to put a per-agent identity: inside `arguments`. This file
// lifts it back out before the argument guard sees it, so every tool keeps its
// closed schema and every consumer downstream reads ONE identity channel.
//
// The split is byte-preserving on purpose. Sibling values are carried as
// json.RawMessage and re-emitted verbatim, so `1.10` stays `1.10` and a big
// integer keeps its digits; a tool that decodes a number as a string — or a
// stats row that records the input — sees what the client sent. When the key
// is absent the original bytes are returned untouched, which is the common
// path for every client that does not stamp.

// splitLogicalAgentArg removes ArgLogicalAgentKey from a top-level arguments
// object and returns its string value. Non-object arguments (an array, a
// scalar, null, empty, malformed) pass through unchanged with no identity. A
// non-string value under the key is removed but never becomes an identity: a
// malformed stamp must neither attribute the call nor reach validation as an
// unknown parameter.
func splitLogicalAgentArg(raw json.RawMessage) (string, json.RawMessage) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", raw
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
		return "", raw
	}
	stamp, ok := obj[ArgLogicalAgentKey]
	if !ok {
		return "", raw
	}
	delete(obj, ArgLogicalAgentKey)
	var id string
	if err := json.Unmarshal(stamp, &id); err != nil {
		id = ""
	}
	return id, marshalRawObject(obj)
}

// marshalRawObject re-encodes a map of raw values with sorted keys, matching
// encoding/json's own map ordering, without decoding any value.
func marshalRawObject(obj map[string]json.RawMessage) json.RawMessage {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(bytes.TrimSpace(obj[k]))
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// resolveLogicalAgent picks the identity a tools/call declares. The per-call
// `_meta` key is the stronger channel — it is asserted by the client's
// transport layer on every frame — so it outranks the argument-carried stamp;
// the stamp outranks nothing but its own absence. session_start's `session_id`
// is the third rung and needs no code here: declaredAgentCtx leaves a ctx that
// already carries an identity alone.
func resolveLogicalAgent(meta map[string]json.RawMessage, argID string) string {
	if id := logicalAgentFromMeta(meta); id != "" {
		return id
	}
	return argID
}
