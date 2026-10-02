package cli

// serve_resume_strip.go — keeping the resume credential away from the client.
//
// The daemon discloses a resume credential (docs/identity-resume-credential-design.md)
// under mcp.MetaResumeCredentialKey, in an initialize result, in a tools/call result
// (the successor after an accepted resume, or a late disclosure to a connection that
// converged on the C3 retry), or in any `_meta` it may one day grow. This proxy
// announced that it removes the key from every frame it forwards, and the daemon
// disclosed on the strength of that announcement, so the removal is the one thing in
// this feature that must never be skipped: Claude Code persists a tool result's `_meta`
// to its on-disk transcripts, where a model with file tools can read it, and a
// credential in a forwarded frame is a bearer secret in the model's reach.
//
// So the removal is not a feature of any one frame kind. stripResumeCredential is the
// last step of writeClient, the single place every byte bound for the client passes
// through, and it removes the key from EVERY `_meta` object in the frame at any depth,
// not only from `result._meta`. Only the key goes: every other string in a frame, tool
// text included, is left as the daemon wrote it. A file or a diff that merely names the
// key, or holds something shaped like a credential, is ordinary output, and redacting it
// would corrupt what the model reads.
//
// It fails closed on a frame it cannot trust. A frame that does not parse, or that
// repeats a member name (which a map would collapse and so hide), is not one the daemon
// wrote, and any token-shaped value in it is redacted rather than forwarded intact.
//
// Capturing the value is a separate act at the places a daemon frame is READ
// (observeResumeFrame), because a replayed initialize response is swallowed and never
// reaches writeClient, and because the value is needed before the frame is stripped.

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

var (
	resumeKeyBytes    = []byte(mcp.MetaResumeCredentialKey)
	resumePrefixBytes = []byte(sessionstate.ResumeSecretPrefix)
	// resumeTokenShape is a resume credential: the versioned prefix and 22 base64url
	// characters. It validates a value before it is stored (a daemon frame can carry
	// anything under the key) and drives the fail-closed redaction.
	resumeTokenShape = regexp.MustCompile(`^` + sessionstate.ResumeSecretPrefix + `[A-Za-z0-9_-]{22}$`)
	resumeTokenScan  = regexp.MustCompile(sessionstate.ResumeSecretPrefix + `[A-Za-z0-9_-]{22}`)
)

// resumeRedacted replaces a credential-shaped token the structured removal could not
// reach. It is the same placeholder internal/redact uses.
const resumeRedacted = "[REDACTED:resume-credential]"

// mayCarryResumeCredential is the fast path: a frame that names neither the key nor a
// token prefix cannot carry the credential, so the common frame (every tool result that
// has nothing to do with identity) costs two byte scans and no parse. The daemon
// encodes with encoding/json, which never escapes the characters of the key or of a
// token, so a literal scan is sufficient for a frame the daemon wrote.
func mayCarryResumeCredential(b []byte) bool {
	return bytes.Contains(b, resumeKeyBytes) || bytes.Contains(b, resumePrefixBytes)
}

// stripResumeCredential returns frame with mcp.MetaResumeCredentialKey removed from
// every `_meta` object in it. A frame that does not carry the key is returned
// unchanged, byte for byte.
func stripResumeCredential(frame []byte) []byte {
	clean, _ := scrubResumeCredential(frame)
	return clean
}

// scrubResumeCredential is stripResumeCredential that also returns the credentials it
// removed, in order, keeping only values that have the credential shape.
func scrubResumeCredential(frame []byte) (clean []byte, secrets []string) {
	if !mayCarryResumeCredential(frame) {
		return frame, nil
	}
	sc := &resumeScrub{}
	out, changed, err := sc.value(frame, false)
	if err != nil {
		// Fail closed. A frame the daemon wrote always parses and never repeats a member,
		// so this is a frame nothing here understands; the one thing it must not do is
		// forward a token. Only this path touches text, and only on a frame that is not
		// well-formed JSON the daemon produced.
		return redactResumeTokens(frame), nil
	}
	if !changed {
		out = frame
	}
	return out, sc.secrets
}

func redactResumeTokens(b []byte) []byte {
	return resumeTokenScan.ReplaceAll(b, []byte(resumeRedacted))
}

// errDuplicateMember marks an object that repeats a member name.
var errDuplicateMember = errors.New("serve: a JSON object repeats a member name")

// resumeScrub walks a JSON document removing the key and collecting its values.
type resumeScrub struct {
	secrets []string
}

// value processes one JSON value. inMeta says the value is a member of a `_meta` key,
// the only place the credential is removed from. changed reports whether the returned
// bytes differ from raw; an unchanged value is returned as the original slice.
func (sc *resumeScrub) value(raw []byte, inMeta bool) (out []byte, changed bool, err error) {
	if !mayCarryResumeCredential(raw) {
		return raw, false, nil
	}
	t := bytes.TrimLeft(raw, " \t\r\n")
	if len(t) == 0 {
		return raw, false, nil
	}
	switch t[0] {
	case '{':
		return sc.object(raw, inMeta)
	case '[':
		return sc.array(raw)
	}
	return raw, false, nil // a scalar: its text is not a member
}

func (sc *resumeScrub) object(raw []byte, inMeta bool) (out []byte, changed bool, err error) {
	obj, err := decodeObject(raw)
	if err != nil {
		return raw, false, err
	}
	if v, ok := obj[mcp.MetaResumeCredentialKey]; ok && inMeta {
		sc.capture(v)
		delete(obj, mcp.MetaResumeCredentialKey)
		changed = true
	}
	for k, v := range obj {
		nv, ch, err := sc.value(v, k == metaMember)
		if err != nil {
			return raw, false, err
		}
		if ch {
			obj[k] = nv
			changed = true
		}
	}
	if !changed {
		return raw, false, nil
	}
	out, err = marshalPlain(obj)
	return out, true, err
}

func (sc *resumeScrub) array(raw []byte) (out []byte, changed bool, err error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return raw, false, err
	}
	for i, v := range arr {
		nv, ch, err := sc.value(v, false)
		if err != nil {
			return raw, false, err
		}
		if ch {
			arr[i] = nv
			changed = true
		}
	}
	if !changed {
		return raw, false, nil
	}
	out, err = marshalPlain(arr)
	return out, true, err
}

// metaMember is the JSON-RPC member name of an object's metadata.
const metaMember = "_meta"

// decodeObject reads a JSON object into raw members, refusing one that repeats a member
// name: a map keeps the last and so would hide an earlier one, which could be the
// credential.
func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil { // the opening brace
		return nil, err
	}
	obj := map[string]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errors.New("serve: a JSON object member name is not a string")
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		if _, dup := obj[key]; dup {
			return nil, errDuplicateMember
		}
		obj[key] = val
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	// Anything after the object is not a JSON value this strip can vouch for.
	if dec.More() {
		return nil, errors.New("serve: trailing data after a JSON object")
	}
	return obj, nil
}

// capture records a removed value when it is a string of the credential shape.
// Anything else under the key is removed all the same and never stored.
func (sc *resumeScrub) capture(raw json.RawMessage) {
	var s string
	if json.Unmarshal(raw, &s) == nil && resumeTokenShape.MatchString(s) {
		sc.secrets = append(sc.secrets, s)
	}
}

// marshalPlain encodes v without HTML escaping, so a frame the strip rewrites does not
// also have its `<`, `>` and `&` turned into \u escapes.
func marshalPlain(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
