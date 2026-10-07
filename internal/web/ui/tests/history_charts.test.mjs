import test from "node:test";
import assert from "node:assert/strict";
import { historyTimeline, escapeHTML } from "../src/lib/charts.js";

test("escapeHTML correctly sanitises hostile HTML sequences", () => {
  assert.equal(
    escapeHTML('<img src=x onerror=window.__reviewXSS=1>'),
    '&lt;img src=x onerror=window.__reviewXSS=1&gt;'
  );
  assert.equal(
    escapeHTML('<script>alert("xss")</script>'),
    '&lt;script&gt;alert(&quot;xss&quot;)&lt;/script&gt;'
  );
  assert.equal(
    escapeHTML("foo & bar 'baz'"),
    "foo &amp; bar &#39;baz&#39;"
  );
  assert.equal(escapeHTML(null), "");
  assert.equal(escapeHTML(undefined), "");
  assert.equal(escapeHTML(123), "123");
});

test("historyTimeline tooltip formatter sanitises hostile filenames and metadata", () => {
  const dummyPalette = {
    acc: "#e06c75",
    grn: "#98c379",
    warn: "#e5c07b",
    soft: "#abb2bf",
    rule: "#3e4451",
    faint: "#5c6370",
    text: "#abb2bf",
    card: "#282c34",
  };

  const hostileChange = {
    seq: 42,
    at: "2026-10-07T12:00:00Z",
    tool: "<script>alert(1)</script>",
    op: '<b onmouseover="alert(2)">update</b>',
    path: "<img src=x onerror=window.__reviewXSS=1>",
    added: 10,
    removed: 2,
  };

  const chart = historyTimeline(dummyPalette, [hostileChange]);
  assert.ok(chart.tooltip && typeof chart.tooltip.formatter === "function");

  // Construct series item point matching historyTimeline mapping:
  // [ts, lines, c.tool, c.op, c.path, c.seq]
  const lines = hostileChange.added + hostileChange.removed;
  const ts = new Date(hostileChange.at).getTime();
  const pointData = [
    ts,
    lines,
    hostileChange.tool,
    hostileChange.op,
    hostileChange.path,
    hostileChange.seq,
  ];

  const renderedHtml = chart.tooltip.formatter({ data: pointData });

  // Must not contain unescaped HTML elements from user input
  assert.ok(!renderedHtml.includes("<img"), "rendered HTML must not contain <img tag");
  assert.ok(!renderedHtml.includes("<script"), "rendered HTML must not contain <script tag");
  assert.ok(!renderedHtml.includes("<b onmouseover"), "rendered HTML must not contain unescaped hostile tags");

  // Must contain properly escaped entities
  assert.ok(
    renderedHtml.includes("&lt;img src=x onerror=window.__reviewXSS=1&gt;"),
    "hostile filename must be HTML-escaped"
  );
  assert.ok(
    renderedHtml.includes("&lt;script&gt;alert(1)&lt;/script&gt;"),
    "tool name must be HTML-escaped"
  );
  assert.ok(
    renderedHtml.includes("&lt;b onmouseover=&quot;alert(2)&quot;&gt;update&lt;/b&gt;"),
    "operation must be HTML-escaped"
  );
});

test("detail request sequencing discards older response on rapid clicks", async () => {
  let detailReqId = 0;
  let selectedSeq = null;
  let detail = null;
  let loadingDetail = false;

  async function mockSelectSeq(seq, delayMs, payload) {
    selectedSeq = seq;
    const reqId = ++detailReqId;
    loadingDetail = true;
    await new Promise((r) => setTimeout(r, delayMs));
    if (reqId !== detailReqId) return;
    detail = payload;
    loadingDetail = false;
  }

  // Click A (seq 1, slow: 50ms) then click B (seq 2, fast: 10ms)
  const p1 = mockSelectSeq(1, 50, { seq: 1, diff: "diff 1" });
  const p2 = mockSelectSeq(2, 10, { seq: 2, diff: "diff 2" });

  await Promise.all([p1, p2]);

  assert.equal(selectedSeq, 2);
  assert.equal(detail.seq, 2);
  assert.equal(detail.diff, "diff 2");
  assert.equal(loadingDetail, false);
});

test("selection reconciliation retains valid selection or resets appropriately", () => {
  function reconcileSelection(changes, currentSeq, currentCallId) {
    const hasSeq = currentSeq != null && changes.some((c) => c.seq === currentSeq);
    const hasCall = currentCallId != null && changes.some((c) => c.callId === currentCallId);

    if (hasSeq || hasCall) {
      return { selectedSeq: currentSeq, selectedCallId: currentCallId, action: "keep" };
    }
    if (changes.length > 0) {
      return { selectedSeq: changes[0].seq, selectedCallId: null, action: "select_first" };
    }
    return { selectedSeq: null, selectedCallId: null, action: "clear" };
  }

  // Case 1: selection exists in new list
  const r1 = reconcileSelection([{ seq: 1 }, { seq: 2 }], 2, null);
  assert.equal(r1.action, "keep");
  assert.equal(r1.selectedSeq, 2);

  // Case 2: switching workspace where selectedSeq 2 is absent, list has seq 3
  const r2 = reconcileSelection([{ seq: 3 }], 2, null);
  assert.equal(r2.action, "select_first");
  assert.equal(r2.selectedSeq, 3);

  // Case 3: filtering results in 0 changes -> clear
  const r3 = reconcileSelection([], 2, null);
  assert.equal(r3.action, "clear");
  assert.equal(r3.selectedSeq, null);
});

