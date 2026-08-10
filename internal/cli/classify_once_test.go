package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/toolerror"
)

// failingTool returns whatever error it is given, so a test can drive a real
// tools/call down the failure path with an error of a chosen classification.
type failingTool struct{ err error }

func (t *failingTool) Name() string        { return "boom" }
func (t *failingTool) Description() string { return "always fails, for classification tests" }
func (t *failingTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *failingTool) Execute(context.Context, json.RawMessage) (string, error) { return "", t.err }

// wireEnvelope is the `_meta` envelope as the client actually receives it.
type wireEnvelope struct {
	Kind        string `json:"kind"`
	Retryable   bool   `json:"retryable"`
	Remediation struct {
		Class string `json:"class"`
	} `json:"remediation"`
}

// callFailingTool runs one real tools/call against a server whose only tool
// fails with toolErr, and returns both halves of what plumb claimed about that
// failure: the `_meta` envelope decoded off the wire, and the stats row the
// daemon's OnAfterTool wiring would have written for the same call.
//
// The two are produced by ONE dispatch — not by two separate constructions — so
// a disagreement between them can only come from the code under test.
func callFailingTool(t *testing.T, toolErr error) (env *wireEnvelope, row stats.Call, metaPresent bool) {
	t.Helper()

	srv := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	srv.Register(&failingTool{err: toolErr})
	// The same stamping the daemon does in onAfterTool: take the classification
	// the dispatch boundary derived, never re-derive one here.
	srv.OnAfterTool = func(_ context.Context, name string, _ json.RawMessage, _, errMsg string, _ time.Duration, _ bool, failure *toolerror.Error) {
		row = withFailure(stats.Call{
			SessionID: "s", Workspace: "/w", Tool: name, ErrorMsg: errMsg,
		}, failure)
	}

	var out bytes.Buffer
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom","arguments":{}}}` + "\n"
	if err := srv.Serve(context.Background(), strings.NewReader(req), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var resp struct {
		Result struct {
			IsError bool                    `json:"isError"`
			Meta    map[string]wireEnvelope `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", out.String(), err)
	}
	if !resp.Result.IsError {
		t.Fatalf("tools/call did not report isError; response was %q", out.String())
	}
	e, ok := resp.Result.Meta[mcp.MetaToolErrorKey]
	if !ok {
		return nil, row, false
	}
	return &e, row, true
}

// TestClassifyOnce_WireAndTelemetryAgree is the drift guard for the two
// consumers of a failure's classification: the `_meta` envelope a client reads
// and the stats row plumb records. It asserts them against EACH OTHER over a
// single real tools/call, not against two hand-written expectations — two
// independent unit tests can both encode the same wrong answer, whereas this
// fails the moment the paths stop sharing one derivation.
func TestClassifyOnce_WireAndTelemetryAgree(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"retryable refusal", toolerror.Wrap(errors.New("uncommitted changes"), toolerror.KindDirtyFile, toolerror.ClassPassDirtyOk)},
		{"non-retryable refusal", toolerror.Wrap(errors.New("disabled"), toolerror.KindGitPolicy, toolerror.ClassEnablePolicy)},
		{"transient", toolerror.Wrap(errors.New("still warming"), toolerror.KindLSPUnavailable, toolerror.ClassRetryWhenReady)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, row, present := callFailingTool(t, tc.err)
			if !present {
				t.Fatal("classified failure emitted no _meta envelope")
			}
			if string(row.ErrorKind) != env.Kind {
				t.Errorf("stats row kind %q, wire kind %q — the two paths have stopped sharing one classification",
					row.ErrorKind, env.Kind)
			}
			if string(row.RemediationClass) != env.Remediation.Class {
				t.Errorf("stats row remediation class %q, wire class %q", row.RemediationClass, env.Remediation.Class)
			}
			if row.ErrorRetryable != env.Retryable {
				t.Errorf("stats row retryable %v, wire retryable %v", row.ErrorRetryable, env.Retryable)
			}
			if row.ErrorKind == "" {
				t.Error("classified failure recorded a blank kind")
			}
		})
	}
}

// TestClassifyOnce_UnclassifiedStaysBlank pins the honest answer for a failure
// plumb has no classification for: the envelope is absent, and the stats row
// stays blank. Recording KindInternal here would be a claim plumb never made to
// the client — and would then be indistinguishable from a genuinely internal
// fault in every failure report.
func TestClassifyOnce_UnclassifiedStaysBlank(t *testing.T) {
	env, row, present := callFailingTool(t, errors.New("something went wrong"))
	if present {
		t.Fatalf("unclassified failure emitted an envelope: %+v", env)
	}
	if row.ErrorKind != "" || row.RemediationClass != "" || row.ErrorRetryable {
		t.Errorf("unclassified failure recorded (%q, %q, %v), want blank/blank/false",
			row.ErrorKind, row.RemediationClass, row.ErrorRetryable)
	}
	if row.ErrorMsg == "" {
		t.Error("the prose error message was dropped; only the classification is absent")
	}
}
