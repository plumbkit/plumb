package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// context_body_test.go — bodies, guards and the stale-span rule (PLAN-462 A2).

var (
	gutterRe   = regexp.MustCompile(`^ *\d+\t`)
	bodyHeadRe = regexp.MustCompile(`^    body lines (\d+)–(\d+) \((\d+) B\) content_sha256=([0-9a-f]{64})$`)
	guardRe    = regexp.MustCompile(`^    guard for edit_file on (\S+) \(expected_mtime, expected_sha\): mtime=(\S+) sha256=([0-9a-f]{64})$`)
)

// deliveredBody is one body block as a reader of the output sees it.
type deliveredBody struct {
	start, end, size int
	contentSHA, text string
	guard            *deliveredGuard // nil when an earlier body of the file carried it
}

type deliveredGuard struct{ path, mtime, sha string }

// splitRelated cuts a rendered pack at the related section: what comes before it
// is the seeds, with the bodies the caller asked for by name; what follows is the
// expansion (A3), whose bodies are a different thing and are counted apart.
func splitRelated(out string) (seeds, rest string) {
	seeds, rest, found := strings.Cut(out, "\nrelated (")
	if found {
		rest = "related (" + rest
	}
	return seeds, rest
}

// parseBodies reads the body blocks of a rendered pack's SEEDS, stripping the
// display gutter exactly as a client would before using a line as old_string.
func parseBodies(t *testing.T, out string) []deliveredBody {
	t.Helper()
	seeds, _ := splitRelated(out)
	return parseBodyBlocks(t, seeds)
}

// parseRelatedBodies reads the body blocks of a rendered pack's related section.
func parseRelatedBodies(t *testing.T, out string) []deliveredBody {
	t.Helper()
	_, rest := splitRelated(out)
	return parseBodyBlocks(t, rest)
}

func parseBodyBlocks(t *testing.T, text string) []deliveredBody {
	t.Helper()
	lines := strings.Split(text, "\n")
	var bodies []deliveredBody
	for i := 0; i < len(lines); i++ {
		m := bodyHeadRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		b := deliveredBody{contentSHA: m[4]}
		b.start, _ = strconv.Atoi(m[1])
		b.end, _ = strconv.Atoi(m[2])
		b.size, _ = strconv.Atoi(m[3])
		if g := guardRe.FindStringSubmatch(lines[i+1]); g != nil {
			b.guard = &deliveredGuard{path: g[1], mtime: g[2], sha: g[3]}
			i++
		}
		var sb strings.Builder
		for i+1 < len(lines) && gutterRe.MatchString(lines[i+1]) {
			i++
			sb.WriteString(gutterRe.ReplaceAllString(lines[i], ""))
			sb.WriteByte('\n')
		}
		b.text = sb.String()
		bodies = append(bodies, b)
	}
	return bodies
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return hashText(string(data))
}

// declLines finds a declaration in a file by the prefix of its first line and
// returns its 1-based span and text, scanning for the closing "}" at column 0
// (or the same line for a one-line declaration). It is independent of the index.
func declLines(t *testing.T, path, prefix string) (start, end int, text string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, prefix) {
			continue
		}
		j := i
		if !strings.HasSuffix(l, "}") { // a multi-line declaration ends at the next "}" in column 0
			for j = i + 1; j < len(lines) && lines[j] != "}"; j++ {
			}
			if j >= len(lines) {
				t.Fatalf("no end found for %q in %s", prefix, path)
			}
		}
		return i + 1, j + 1, strings.Join(lines[i:j+1], "\n") + "\n"
	}
	t.Fatalf("no line starts with %q in %s", prefix, path)
	return 0, 0, ""
}

// The body is the declaration's exact lines, its content hash is the SHA-256 of
// exactly that text, and the file guard is the real mtime and SHA-256 of the file
// the body was sliced from: a different thing from the content hash.
func TestContextForTask_DeliversTheBodyWithItsHashAndGuard(t *testing.T) {
	s := newShop(t)
	file := filepath.Join(s.root, "cart", "cart.go")
	wantStart, wantEnd, wantText := declLines(t, file, "func (c *Cart) Total()")

	out, err := s.run(t, map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}})
	if err != nil {
		t.Fatal(err)
	}
	bodies := parseBodies(t, out)
	if len(bodies) != 1 {
		t.Fatalf("want one delivered body, got %d:\n%s", len(bodies), out)
	}
	b := bodies[0]
	if b.text != wantText || b.start != wantStart || b.end != wantEnd {
		t.Errorf("body = lines %d-%d %q, want lines %d-%d %q", b.start, b.end, b.text, wantStart, wantEnd, wantText)
	}
	if b.size != len(wantText) || b.contentSHA != hashText(wantText) {
		t.Errorf("content_sha256 = %s over %d B, want the SHA-256 of exactly the delivered text (%s over %d B)",
			b.contentSHA, b.size, hashText(wantText), len(wantText))
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if b.guard == nil {
		t.Fatalf("the first body of a file must carry the file guard:\n%s", out)
	}
	if b.guard.sha != fileSHA(t, file) || b.guard.mtime != info.ModTime().Format(time.RFC3339Nano) || b.guard.path != "cart/cart.go" {
		t.Errorf("guard = %+v, want sha256 %s and mtime %s of the file itself", *b.guard, fileSHA(t, file), info.ModTime().Format(time.RFC3339Nano))
	}
	if b.guard.sha == b.contentSHA {
		t.Error("the guard and the content hash are the same value; a content hash is not an edit guard")
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "content_sha256") && (strings.Contains(l, "mtime=") || strings.Contains(l, "expected_sha")) {
			t.Errorf("the content hash shares a line with an edit guard: %q", l)
		}
	}
}

// Two bodies from one file print the guard once, with the first, and a body
// whose predecessor was dropped carries it instead.
func TestContextForTask_OneGuardPerFile(t *testing.T) {
	s := newShop(t)
	out, err := s.run(t, map[string]any{"symbols": []string{"cart/cart.go#Cart.Add", "cart/cart.go#Cart.Remove", "reports/reports.go#Summarise"}})
	if err != nil {
		t.Fatal(err)
	}
	bodies := parseBodies(t, out)
	if len(bodies) < 3 {
		t.Fatalf("want three delivered bodies, got %d:\n%s", len(bodies), out)
	}
	guards := map[string]int{}
	for _, b := range bodies {
		if b.guard != nil {
			guards[b.guard.path]++
		}
	}
	if guards["cart/cart.go"] != 1 || guards["reports/reports.go"] != 1 || bodies[1].guard != nil {
		t.Errorf("guards per file = %v (second cart body guarded: %v); want exactly one each, on the first body of each file", guards, bodies[1].guard != nil)
	}
}

// C09: after indexing, a method is inserted above Total. The index still holds
// Total's old lines. The body must be exactly the current Total, never the lines
// the stale span now points at, and the guard must be the changed file's.
func TestContextForTask_StaleIndexSpanNeverSlicesTheBody(t *testing.T) {
	s := newShop(t)
	file := filepath.Join(s.root, "cart", "cart.go")
	indexed := s.collect(t, map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}})
	if len(indexed.Seeds) != 1 {
		t.Fatalf("the seed did not resolve before the mutation: %+v", indexed.Misses)
	}
	staleStart, staleEnd := indexed.Seeds[0].Line, indexed.Seeds[0].EndLine

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	inserted := "// Count returns how many lines the cart holds.\nfunc (c *Cart) Count() int { return len(c.items) }\n\n// Total returns"
	mutated := strings.Replace(string(data), "// Total returns", inserted, 1)
	if mutated == string(data) {
		t.Fatal("the fixture no longer has the anchor the mutation inserts above")
	}
	if err := os.WriteFile(file, []byte(mutated), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := s.run(t, map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}})
	if err != nil {
		t.Fatal(err)
	}
	wantStart, wantEnd, wantText := declLines(t, file, "func (c *Cart) Total()")
	bodies := parseBodies(t, out)
	if len(bodies) != 1 {
		t.Fatalf("want one delivered body, got %d:\n%s", len(bodies), out)
	}
	b := bodies[0]
	if b.text != wantText || b.start != wantStart || b.end != wantEnd {
		t.Errorf("body = lines %d-%d, want the current Total at lines %d-%d:\n%s", b.start, b.end, wantStart, wantEnd, b.text)
	}
	// Control: the stale span really does point somewhere else, so a body sliced
	// from it would have differed; without this the assertions above could pass
	// for a fixture whose mutation moved nothing.
	lines := strings.Split(mutated, "\n")
	staleSlice := strings.Join(lines[staleStart-1:staleEnd], "\n") + "\n"
	if staleSlice == wantText || staleStart == wantStart {
		t.Fatalf("control: the stale span [%d-%d] slices %q, the same as the current Total", staleStart, staleEnd, staleSlice)
	}
	if b.guard == nil || b.guard.sha != fileSHA(t, file) {
		t.Errorf("guard = %+v, want the sha256 %s of the changed file, not anything from the index", b.guard, fileSHA(t, file))
	}
	if !strings.Contains(out, "cart/cart.go changed since it was indexed") {
		t.Errorf("the pack does not say the file changed since indexing:\n%s", out)
	}
}

// With nothing changed since indexing the same call carries no such label: the
// stale-file label is evidence, not boilerplate.
func TestContextForTask_UnchangedFileCarriesNoStaleLabel(t *testing.T) {
	s := newShop(t)
	out, err := s.run(t, map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "changed since it was indexed") {
		t.Errorf("an unchanged file was labelled stale:\n%s", out)
	}
}

// When the declaration is gone from the changed file, the body is withheld with
// that reason and a read_symbol handoff; nothing is sliced from the old span.
func TestContextForTask_DeclarationGoneFromAChangedFileHasNoBody(t *testing.T) {
	s := newShop(t)
	file := filepath.Join(s.root, "cart", "cart.go")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	start, end, text := declLines(t, file, "func (c *Cart) Total()")
	gone := strings.Replace(string(data), text, strings.Repeat("// removed\n", end-start+1), 1)
	if err := os.WriteFile(file, []byte(gone), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := s.run(t, map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}})
	if err != nil {
		t.Fatal(err)
	}
	if bodies := parseBodies(t, out); len(bodies) != 0 {
		t.Fatalf("a body was delivered for a declaration that is no longer in the file: %+v", bodies)
	}
	for _, want := range []string{"stale span: the declaration is no longer in the current file", `read_symbol {"path"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// If the changed file now declares the selector twice, which one the caller meant
// cannot be told from the snapshot, so no body is chosen for them.
func TestContextForTask_DeclarationNowAppearingTwiceHasNoBody(t *testing.T) {
	s := newGoWorkspace(t, map[string]string{"x.go": "package p\n\nfunc F() int { return 1 }\n"})
	writeFiles(t, s.root, map[string]string{"x.go": "package p\n\nfunc F() int { return 1 }\n\nfunc F() int { return 2 }\n"})
	out, err := s.run(t, map[string]any{"symbols": []string{"x.go#F"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(parseBodies(t, out)) != 0 || !strings.Contains(out, "appears more than once in the current file") {
		t.Errorf("an ambiguous redeclaration must withhold the body with that reason:\n%s", out)
	}
}

func TestContextForTask_UnreadableFileHasNoBody(t *testing.T) {
	s := newShop(t)
	if err := os.Remove(filepath.Join(s.root, "cart", "cart.go")); err != nil {
		t.Fatal(err)
	}
	out, err := s.run(t, map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(parseBodies(t, out)) != 0 || !strings.Contains(out, "body unavailable (the file cannot be read)") {
		t.Errorf("a vanished file must give a labelled absence, not a body:\n%s", out)
	}
}

func TestContextForTask_NonUTF8BodyIsWithheld(t *testing.T) {
	root := t.TempDir()
	writeFileT(t, root, "bad.go", "package bad\n\n// Bad holds bytes no JSON client could receive intact.\nfunc Bad() string { return \"\xff\xfe\" }\n")
	s := newShopTool(t, openContextStore(t, root, 1), root)
	out, err := s.run(t, map[string]any{"symbols": []string{"bad.go#Bad"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(parseBodies(t, out)) != 0 || !strings.Contains(out, "not valid UTF-8") || !utf8.ValidString(out) {
		t.Errorf("a body that is not UTF-8 must be withheld, with the output itself valid:\n%s", out)
	}
}

// C11: a body larger than anything the budget can hold is a handoff naming its
// size, its content hash and the snapshot it was taken from. It is never a partial
// body, never exceeds the budget, and records no read.
func TestContextForTask_OversizedBodyIsAGuardedHandoffNotAPartialBody(t *testing.T) {
	s := newShop(t)
	var recorded atomic.Int32
	tracker := NewReadTracker()
	tracker.SetPersistSink(func(string, time.Time, string) { recorded.Add(1) })
	s.tool.WithReads(tracker)
	file := filepath.Join(s.root, "reports", "reports.go")
	start, end, text := declLines(t, file, "func StatusLabel(")

	out, err := s.run(t, map[string]any{"symbols": []string{"reports/reports.go#StatusLabel"}, "max_bytes": 16000})
	if err != nil {
		t.Fatal(err)
	}
	if len(text) < 16000 {
		t.Fatalf("control: the fixture body is %d B, not larger than the budget", len(text))
	}
	if len(parseBodies(t, out)) != 0 || strings.Contains(out, "code-000") || strings.Contains(out, "guard for edit_file") {
		t.Errorf("an oversized body leaked into the pack, or was presented with an edit guard:\n%.600s", out)
	}
	call := `read_symbol {"path":` + string(quotedJSON(t, filepath.Join(canonicalRoot(s.root), "reports", "reports.go"))) + `,"name":"StatusLabel"}`
	for _, want := range []string{
		"body larger than the budget (" + strconv.Itoa(len(text)) + " B, lines " + strconv.Itoa(start) + "–" + strconv.Itoa(end),
		"content_sha256=" + hashText(text),
		"snapshot file sha256=" + fileSHA(t, file),
		call,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("handoff lacks %q:\n%s", want, out)
		}
	}
	if len(out) > 16000-contextReserveBytes {
		t.Errorf("handoff output is %d B, over the %d B pack budget", len(out), 16000-contextReserveBytes)
	}
	if n := recorded.Load(); n != 0 {
		t.Errorf("a handoff recorded %d read(s); the agent has not seen the body", n)
	}
}

// The source read is capped at four times max_bytes: a file larger than that is
// not read for a body, and the pack says why.
func TestContextForTask_SourceReadCapWithholdsBodiesOfHugeFiles(t *testing.T) {
	s := newShop(t)
	out, err := s.run(t, map[string]any{"symbols": []string{"reports/reports.go#StatusLabel"}, "max_bytes": 4000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "source-read cap reached") || strings.Contains(out, "body larger than the budget") {
		t.Errorf("a file over 4x max_bytes must be withheld for the read cap, not slice-and-measured:\n%s", out)
	}
}

// ctxKeyAgent names the agent a test call runs as, for the per-agent tracker
// resolver.
type ctxKeyAgent struct{}

func asAgent(agent string) context.Context {
	return context.WithValue(context.Background(), ctxKeyAgent{}, agent)
}

// agentTrackers resolves a tracker per agent, the way the daemon's shard
// resolver does for a shared connection.
type agentTrackers map[string]*ReadTracker

func newAgentTrackers(agents ...string) agentTrackers {
	m := agentTrackers{}
	for _, a := range agents {
		m[a] = NewReadTracker()
	}
	return m
}

func (m agentTrackers) resolve(ctx context.Context) *ReadTracker {
	agent, _ := ctx.Value(ctxKeyAgent{}).(string)
	return m[agent]
}

// strictEdit makes a str_replace edit of path under strict mode with trackers as
// the agent's read record, optionally guarded.
func strictEdit(t *testing.T, ctx context.Context, trackers agentTrackers, path string, extra map[string]any) (string, error) {
	t.Helper()
	args := map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "c.items = append(c.items, it)", "new_string": "c.items = append(c.items, it) // edited"}},
	}
	for k, v := range extra {
		args[k] = v
	}
	deps := WriteDeps{ReadsFor: trackers.resolve, Strict: func() bool { return true }}
	return NewEditFile(deps).Execute(ctx, mustJSON(args))
}

// Read recording (Invariant 4), with strict mode explicitly on. Positive and
// negative controls in one: a pack whose body was omitted leaves a strict edit
// refused as unread; a pack that delivered the body lets a guarded edit with the
// printed values through.
func TestContextForTask_OnlyADeliveredBodyIsARead(t *testing.T) {
	file := func(s shopTool) string { return filepath.Join(s.root, "cart", "cart.go") }
	t.Run("omitted body leaves the strict edit refused", func(t *testing.T) {
		s := newShop(t)
		trackers := newAgentTrackers("a")
		s.tool.WithReadsFor(trackers.resolve)
		out, err := s.runAs(t, asAgent("a"), map[string]any{"symbols": []string{"cart/cart.go#Cart.Add"}, "max_bytes": contextMinMaxBytes})
		if err != nil {
			t.Fatal(err)
		}
		if len(parseBodies(t, out)) != 0 {
			t.Fatalf("control: the tiny budget still delivered a body:\n%s", out)
		}
		_, err = strictEdit(t, asAgent("a"), trackers, file(s), nil)
		requireErr(t, err, "has not been read")
	})
	t.Run("delivered body satisfies a guarded strict edit", func(t *testing.T) {
		s := newShop(t)
		trackers := newAgentTrackers("a")
		s.tool.WithReadsFor(trackers.resolve)
		out, err := s.runAs(t, asAgent("a"), map[string]any{"symbols": []string{"cart/cart.go#Cart.Add"}})
		if err != nil {
			t.Fatal(err)
		}
		bodies := parseBodies(t, out)
		if len(bodies) != 1 || bodies[0].guard == nil {
			t.Fatalf("want one delivered, guarded body:\n%s", out)
		}
		g := bodies[0].guard
		if _, err := strictEdit(t, asAgent("a"), trackers, file(s), map[string]any{"expected_sha": g.sha, "expected_mtime": g.mtime}); err != nil {
			t.Fatalf("a strict edit with the printed guard was refused: %v", err)
		}
	})
	t.Run("a content hash is not an edit guard", func(t *testing.T) {
		s := newShop(t)
		trackers := newAgentTrackers("a")
		s.tool.WithReadsFor(trackers.resolve)
		out, err := s.runAs(t, asAgent("a"), map[string]any{"symbols": []string{"cart/cart.go#Cart.Add"}})
		if err != nil {
			t.Fatal(err)
		}
		bodies := parseBodies(t, out)
		if len(bodies) != 1 {
			t.Fatalf("want one body:\n%s", out)
		}
		_, err = strictEdit(t, asAgent("a"), trackers, file(s), map[string]any{"expected_sha": bodies[0].contentSHA})
		if err == nil {
			t.Fatal("edit_file accepted a body's content_sha256 as the file's expected_sha")
		}
	})
}

// A pack delivered to agent A is not a read for agent B on the same connection.
func TestContextForTask_ABodyDeliveredToOneAgentIsNotAReadForAnother(t *testing.T) {
	s := newShop(t)
	trackers := newAgentTrackers("a", "b")
	s.tool.WithReadsFor(trackers.resolve)
	file := filepath.Join(s.root, "cart", "cart.go")
	if _, err := s.runAs(t, asAgent("a"), map[string]any{"symbols": []string{"cart/cart.go#Cart.Add"}}); err != nil {
		t.Fatal(err)
	}
	// The delivery is agent A's read: A's own strict edit goes through.
	if _, err := strictEdit(t, asAgent("a"), trackers, file, nil); err != nil {
		t.Fatalf("agent A's strict edit after its own delivery was refused: %v", err)
	}
	if _, err := strictEdit(t, asAgent("b"), trackers, file, nil); err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("agent B's strict edit must be refused as unread, got %v", err)
	}
	if !trackers["b"].Mtime(file).IsZero() {
		t.Error("agent A's delivered body was recorded on agent B's tracker")
	}
	// Control: B's own delivery makes B's edit succeed, so the refusal above was
	// isolation and not a broken edit.
	if _, err := s.runAs(t, asAgent("b"), map[string]any{"symbols": []string{"cart/cart.go#Cart.Add"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := strictEdit(t, asAgent("b"), trackers, file, nil); err != nil {
		t.Fatalf("control: agent B's edit after its own delivery was refused: %v", err)
	}
}

// Recording is once per file, with the snapshot's own version, and only for what
// was delivered: two bodies from one file record once; an ambiguous selector, an
// unresolved one and a hint-free omission record nothing.
func TestContextForTask_RecordsOncePerDeliveredFileWithTheSnapshotVersion(t *testing.T) {
	s := newShop(t)
	var calls []string
	tracker := NewReadTracker()
	tracker.SetPersistSink(func(path string, mtime time.Time, sha string) {
		calls = append(calls, path+"|"+mtime.Format(time.RFC3339Nano)+"|"+sha)
	})
	s.tool.WithReads(tracker)
	cart := filepath.Join(canonicalRoot(s.root), "cart", "cart.go")

	if _, err := s.run(t, map[string]any{"symbols": []string{"Total", "cart/cart.go#Cart.Coupon"}, "max_bytes": contextMinMaxBytes}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("an ambiguous selector, an unresolved one and an omitted body recorded reads: %v", calls)
	}
	if _, err := s.run(t, map[string]any{"symbols": []string{"cart/cart.go#Cart.Add", "cart/cart.go#Cart.Remove"}}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("two bodies from one file recorded %d times, want once: %v", len(calls), calls)
	}
	info, err := os.Stat(cart)
	if err != nil {
		t.Fatal(err)
	}
	if want := lockPathKey(cart) + "|" + info.ModTime().Format(time.RFC3339Nano) + "|" + fileSHA(t, cart); calls[0] != want {
		t.Errorf("recorded %q, want the snapshot's own version %q", calls[0], want)
	}
}
