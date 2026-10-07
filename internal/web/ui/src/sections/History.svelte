<script>
  import { onMount } from "svelte";
  import { getJSON } from "../lib/api.js";
  import { humanBytes } from "../lib/format.js";
  import { palette } from "../lib/theme.svelte.js";
  import { historyTimeline } from "../lib/charts.js";
  import Card from "../lib/Card.svelte";
  import Chart from "../lib/Chart.svelte";

  let model = $state({ workspace: "", workspaces: [], changes: [] });
  let selectedWs = $state("__all__");
  let sessionFilter = $state("");
  let agentFilter = $state("");
  let toolFilter = $state("");
  let fileFilter = $state("");
  let limit = $state(50);

  let selectedSeq = $state(null);
  let selectedCallId = $state(null);
  let detail = $state(null);
  let loadingDetail = $state(false);
  let loadingList = $state(false);

  const P = $derived(palette());
  const timelineOption = $derived(historyTimeline(P, model.changes));

  // Group changes by call_id while preserving overall order
  const callGroups = $derived.by(() => {
    const groups = [];
    let current = null;

    for (const c of model.changes) {
      const cid = c.callId || `single_${c.seq}`;
      if (!current || current.callId !== c.callId || !c.callId) {
        current = {
          id: `${cid}_${groups.length}`,
          callId: c.callId,
          tool: c.tool,
          call: c.call,
          changes: [c],
        };
        groups.push(current);
      } else {
        current.changes.push(c);
      }
    }
    return groups;
  });

  let detailReqId = 0;
  let listReqId = 0;

  async function load() {
    loadingList = true;
    const reqId = ++listReqId;
    // Invalidate pending detail requests when changing scope
    detailReqId++;
    try {
      const params = new URLSearchParams();
      if (selectedWs && selectedWs !== "__all__") {
        params.set("workspace", selectedWs);
      } else if (selectedWs === "__all__") {
        params.set("all", "true");
      }
      if (sessionFilter.trim()) params.set("session", sessionFilter.trim());
      if (agentFilter.trim()) params.set("agent", agentFilter.trim());
      if (toolFilter.trim()) params.set("tool", toolFilter.trim());
      if (fileFilter.trim()) params.set("file", fileFilter.trim());
      if (limit) params.set("limit", String(limit));

      const q = params.toString() ? "?" + params.toString() : "";
      const res = await getJSON("/api/history" + q);
      if (reqId !== listReqId) return;
      model = res;
      if (selectedWs === "" && model.workspace) {
        selectedWs = model.workspace;
      }

      // Reconcile selection: retain only if it belongs to the accepted result set
      const changes = model.changes || [];
      const hasSelectedSeq = selectedSeq != null && changes.some((c) => c.seq === selectedSeq);
      const hasSelectedCall = selectedCallId != null && changes.some((c) => c.callId === selectedCallId);

      if (hasSelectedSeq || hasSelectedCall) {
        // Selection remains valid in the current result set
      } else if (changes.length > 0) {
        // Selection is no longer present; select first matching change
        selectedSeq = null;
        selectedCallId = null;
        await selectSeq(changes[0].seq);
      } else {
        // No matching changes; clear selection and detail
        selectedSeq = null;
        selectedCallId = null;
        detail = null;
        loadingDetail = false;
      }
    } catch (err) {
      if (reqId === listReqId) {
        console.error("load history error:", err);
      }
    } finally {
      if (reqId === listReqId) {
        loadingList = false;
      }
    }
  }

  async function selectSeq(seq) {
    selectedSeq = seq;
    selectedCallId = null;
    const reqId = ++detailReqId;
    loadingDetail = true;
    try {
      const res = await getJSON(`/api/history/${seq}`);
      if (reqId !== detailReqId) return;
      detail = res;
    } catch (err) {
      if (reqId !== detailReqId) return;
      console.error("load seq detail error:", err);
      detail = null;
    } finally {
      if (reqId === detailReqId) {
        loadingDetail = false;
      }
    }
  }

  async function selectCall(callId) {
    if (!callId) return;
    selectedCallId = callId;
    selectedSeq = null;
    const reqId = ++detailReqId;
    loadingDetail = true;
    try {
      const res = await getJSON(`/api/history/${encodeURIComponent(callId)}`);
      if (reqId !== detailReqId) return;
      detail = res;
    } catch (err) {
      if (reqId !== detailReqId) return;
      console.error("load call detail error:", err);
      detail = null;
    } finally {
      if (reqId === detailReqId) {
        loadingDetail = false;
      }
    }
  }

  function formatTime(iso) {
    if (!iso) return "";
    const d = new Date(iso);
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  }

  function opClass(op) {
    switch (op) {
      case "create": return "op-create";
      case "delete": return "op-delete";
      case "revert": return "op-revert";
      default: return "op-update";
    }
  }

  onMount(() => {
    load();
  });
</script>

<div class="flex items-center justify-between mb-4">
  <h1 class="text-lg font-semibold" style="color:var(--text)">Write-Diff History</h1>
  <div class="text-[12px]" style="color:var(--faint)">
    {model.changes.length} recorded change{model.changes.length === 1 ? "" : "s"}
  </div>
</div>

<!-- Filter bar -->
<div
  class="rounded-xl border p-3 mb-4 flex flex-wrap gap-3 items-center"
  style="border-color:var(--rule);background:var(--card)"
>
  <div class="flex items-center gap-1.5 text-[12px]">
    <span style="color:var(--faint)">Workspace:</span>
    <select
      class="rounded-lg border px-2 py-1 text-[12px]"
      style="border-color:var(--rule);background:var(--card2);color:var(--text)"
      bind:value={selectedWs}
      onchange={load}
    >
      <option value="__all__">All workspaces</option>
      {#each model.workspaces as ws (ws.folder)}
        <option value={ws.folder}>{ws.name || ws.folder}</option>
      {/each}
    </select>
  </div>

  <div class="flex items-center gap-1.5 text-[12px]">
    <span style="color:var(--faint)">Tool:</span>
    <select
      class="rounded-lg border px-2 py-1 text-[12px]"
      style="border-color:var(--rule);background:var(--card2);color:var(--text)"
      bind:value={toolFilter}
      onchange={load}
    >
      <option value="">All tools</option>
      <option value="edit_file">edit_file</option>
      <option value="write_file">write_file</option>
      <option value="delete_file">delete_file</option>
      <option value="rename_file">rename_file</option>
      <option value="copy_file">copy_file</option>
      <option value="undo_edit">undo_edit</option>
      <option value="transaction_apply">transaction_apply</option>
    </select>
  </div>

  <div class="flex items-center gap-1.5 text-[12px]">
    <span style="color:var(--faint)">Session:</span>
    <input
      type="text"
      placeholder="filter..."
      class="rounded-lg border px-2 py-1 text-[12px] w-24"
      style="border-color:var(--rule);background:var(--card2);color:var(--text)"
      bind:value={sessionFilter}
      onkeydown={(e) => e.key === "Enter" && load()}
    />
  </div>

  <div class="flex items-center gap-1.5 text-[12px]">
    <span style="color:var(--faint)">Agent:</span>
    <input
      type="text"
      placeholder="filter..."
      class="rounded-lg border px-2 py-1 text-[12px] w-24"
      style="border-color:var(--rule);background:var(--card2);color:var(--text)"
      bind:value={agentFilter}
      onkeydown={(e) => e.key === "Enter" && load()}
    />
  </div>

  <div class="flex items-center gap-1.5 text-[12px]">
    <span style="color:var(--faint)">File:</span>
    <input
      type="text"
      placeholder="file path..."
      class="rounded-lg border px-2 py-1 text-[12px] w-36"
      style="border-color:var(--rule);background:var(--card2);color:var(--text)"
      bind:value={fileFilter}
      onkeydown={(e) => e.key === "Enter" && load()}
    />
  </div>

  <button
    class="ml-auto rounded-lg px-3 py-1 text-[12px] font-medium transition-colors"
    style="background:color-mix(in srgb,var(--acc) 14%,transparent);color:var(--acc);border:1px solid color-mix(in srgb,var(--acc) 30%,transparent)"
    onclick={load}
  >
    Apply
  </button>
</div>

<!-- Timeline Chart -->
<div class="mb-4">
  <Card title="Activity Timeline" desc="Write changes over time (point size reflects lines changed)">
    {#if model.changes.length > 0}
      <Chart option={timelineOption} height="180px" />
    {:else}
      <div class="text-[12px] py-8 text-center" style="color:var(--faint)">
        No write history found matching filter.
      </div>
    {/if}
  </Card>
</div>

<!-- Master-Detail Grid -->
<div class="grid grid-cols-1 lg:grid-cols-12 gap-4 items-start">
  <!-- Left: Changes List (5 cols) -->
  <div class="lg:col-span-5 flex flex-col gap-3">
    {#if loadingList}
      <div class="text-[12px] p-4 text-center" style="color:var(--faint)">Loading changes…</div>
    {:else if model.changes.length === 0}
      <Card title="Changes" desc="No records found">
        <div class="text-[12px] py-8 text-center" style="color:var(--faint)">
          No file changes recorded yet in history.db.
        </div>
      </Card>
    {:else}
      {#each callGroups as grp (grp.id)}
        <div
          class="rounded-xl border overflow-hidden transition-colors"
          style="border-color:var(--rule);background:var(--card)"
        >
          <!-- Call Header -->
          <div
            class="px-3 py-2 border-b flex items-center gap-2 text-[12px] transition-colors"
            style={selectedCallId === grp.callId && grp.callId
              ? "border-color:var(--acc);background:color-mix(in srgb,var(--acc) 10%,var(--card2))"
              : "border-color:var(--rule);background:var(--card2)"}
          >
            <span class="font-semibold font-mono" style="color:var(--acc)">{grp.tool}</span>
            {#if grp.call}
              <span
                class="text-[10px] px-1.5 py-0.5 rounded font-mono font-medium"
                style={grp.call.success
                  ? "background:color-mix(in srgb,var(--grn) 12%,transparent);color:var(--grn)"
                  : "background:color-mix(in srgb,var(--warn) 12%,transparent);color:var(--warn)"}
              >
                {grp.call.durationMs}ms
              </span>
            {/if}

            {#if grp.callId}
              <button
                class="ml-auto text-[11px] font-mono hover:underline cursor-pointer"
                style="color:var(--faint)"
                onclick={() => selectCall(grp.callId)}
                title="View all diffs for this tool call"
              >
                {grp.callId}
              </button>
            {/if}
          </div>

          <!-- Changes inside this call -->
          <div class="divide-y" style="border-color:color-mix(in srgb,var(--rule) 50%,transparent)">
            {#each grp.changes as c (c.seq)}
              <!-- Gap marker if gapBefore is true -->
              {#if c.gapBefore}
                <div
                  class="px-3 py-1.5 text-[11px] flex items-center gap-1.5 font-mono"
                  style="background:color-mix(in srgb,var(--warn) 8%,transparent);color:var(--warn)"
                >
                  <span>⚠</span>
                  <span>
                    {c.gapDropped ? "⋯ unrecorded change (dropped by queue overflow)" : "⋯ unrecorded change (outside plumb)"}
                  </span>
                </div>
              {/if}

              <button
                class="w-full text-left px-3 py-2 flex items-center gap-2 text-[12px] transition-colors cursor-pointer"
                style={selectedSeq === c.seq
                  ? "background:color-mix(in srgb,var(--acc) 12%,transparent)"
                  : "background:transparent"}
                onclick={() => selectSeq(c.seq)}
              >
                <!-- Operation badge -->
                <span
                  class="text-[9.5px] px-1.5 py-0.5 rounded font-bold uppercase tracking-wider {opClass(c.op)}"
                >
                  {c.op}
                </span>

                <!-- File path -->
                <span class="font-mono text-[11.5px] truncate flex-1" style="color:var(--text)" title={c.path}>
                  {c.path}
                </span>

                <!-- Lines or withheld indicator -->
                {#if c.content !== "diff" && c.content !== ""}
                  <span class="text-[10.5px] italic font-mono" style="color:var(--faint)">
                    [{c.content}]
                  </span>
                {:else if c.added > 0 || c.removed > 0}
                  <span class="text-[11px] font-mono shrink-0">
                    {#if c.added > 0}<span style="color:var(--grn)">+{c.added}</span>{/if}
                    {#if c.removed > 0} <span style="color:var(--warn)">-{c.removed}</span>{/if}
                  </span>
                {/if}

                <span class="text-[10px] shrink-0 font-mono" style="color:var(--faint)">
                  {formatTime(c.at)}
                </span>
              </button>
            {/each}
          </div>
        </div>
      {/each}
    {/if}
  </div>

  <!-- Right: Unified Diff Viewer (7 cols) -->
  <div class="lg:col-span-7">
    <Card
      title={detail ? (detail.callId && !detail.entry ? `Call ${detail.callId}` : `Diff #${detail.seq || detail.entry?.seq}`) : "Diff Viewer"}
      desc={detail?.entry ? detail.entry.path : (detail?.entries?.length > 1 ? `${detail.entries.length} files changed in tool call` : "Select a change row or tool call to inspect diff")}
    >
      {#if loadingDetail}
        <div class="text-[12px] py-12 text-center" style="color:var(--faint)">Loading diff…</div>
      {:else if !detail}
        <div class="text-[12px] py-12 text-center" style="color:var(--faint)">Nothing selected.</div>
      {:else}
        <!-- Metadata bar -->
        <div
          class="rounded-lg border p-2.5 mb-3 text-[11.5px] flex flex-wrap gap-x-4 gap-y-1 items-center"
          style="border-color:var(--rule);background:var(--card2)"
        >
          {#if detail.entry}
            <div><span style="color:var(--faint)">Seq:</span> <b class="font-mono">{detail.entry.seq}</b></div>
            <div><span style="color:var(--faint)">Tool:</span> <b class="font-mono" style="color:var(--acc)">{detail.entry.tool}</b></div>
            {#if detail.entry.logicalAgent}
              <div><span style="color:var(--faint)">Agent:</span> <b class="font-mono">{detail.entry.logicalAgent}</b></div>
            {/if}
            {#if detail.entry.sessionName}
              <div><span style="color:var(--faint)">Session:</span> <b class="font-mono">{detail.entry.sessionName}</b></div>
            {/if}
            {#if detail.entry.revertsSeq}
              <div><span style="color:var(--faint)">Reverts:</span> <b class="font-mono" style="color:var(--warn)">#{detail.entry.revertsSeq}</b></div>
            {/if}
            {#if detail.entry.beforeSize || detail.entry.afterSize}
              <div>
                <span style="color:var(--faint)">Size:</span>
                <span class="font-mono">{humanBytes(detail.entry.beforeSize)} → {humanBytes(detail.entry.afterSize)}</span>
              </div>
            {/if}
          {/if}

          {#if detail.call}
            <div>
              <span style="color:var(--faint)">Duration:</span>
              <span class="font-mono">{detail.call.durationMs}ms</span>
            </div>
            {#if detail.call.errorMsg}
              <div style="color:var(--warn)">
                <span style="color:var(--faint)">Error:</span>
                <span>{detail.call.errorMsg}</span>
              </div>
            {/if}
          {/if}
        </div>

        <!-- Render diff(s) -->
        {#if detail.entries && detail.entries.length > 1}
          <!-- Multiple files in call view -->
          <div class="flex flex-col gap-4">
            {#each detail.entries as entry, i (entry.seq)}
              <div class="rounded-lg border overflow-hidden" style="border-color:var(--rule)">
                <div class="px-3 py-1.5 border-b font-mono text-[11.5px] flex items-center justify-between" style="border-color:var(--rule);background:var(--card2)">
                  <span>{entry.path}</span>
                  <span class="text-[9.5px] px-1.5 py-0.5 rounded font-bold uppercase {opClass(entry.op)}">{entry.op}</span>
                </div>
                {#if entry.content !== "diff" && entry.content !== ""}
                  <div class="p-4 text-[12px] italic text-center" style="color:var(--faint)">
                    [Content: {entry.content}]
                  </div>
                {:else if detail.diffs && detail.diffs[i]}
                  <pre class="diff-block font-mono text-[11.5px] leading-relaxed p-3 overflow-x-auto m-0">{#each detail.diffs[i].split("\n") as line}<span class="diff-line {line.startsWith('+') && !line.startsWith('+++') ? 'diff-add' : line.startsWith('-') && !line.startsWith('---') ? 'diff-del' : line.startsWith('@@') ? 'diff-hunk' : 'diff-ctx'}">{line}
</span>{/each}</pre>
                {:else}
                  <div class="p-4 text-[12px] text-center" style="color:var(--faint)">No diff content recorded.</div>
                {/if}
              </div>
            {/each}
          </div>
        {:else}
          <!-- Single entry view -->
          {#if detail.entry && detail.entry.content !== "diff" && detail.entry.content !== ""}
            <div
              class="rounded-lg border p-6 text-center"
              style="border-color:var(--rule);background:var(--card2)"
            >
              <div class="font-mono text-[13px] font-semibold mb-1" style="color:var(--warn)">
                [{detail.entry.content}]
              </div>
              <div class="text-[12px]" style="color:var(--faint)">
                {detail.entry.content.startsWith("withheld:")
                  ? "Content withheld from history database per project safety policy."
                  : "No content change (metadata only)."}
              </div>
            </div>
          {:else if detail.diff}
            <pre class="diff-block font-mono text-[11.5px] leading-relaxed p-3 rounded-lg border overflow-x-auto m-0" style="border-color:var(--rule);background:var(--card2)">{#each detail.diff.split("\n") as line}<span class="diff-line {line.startsWith('+') && !line.startsWith('+++') ? 'diff-add' : line.startsWith('-') && !line.startsWith('---') ? 'diff-del' : line.startsWith('@@') ? 'diff-hunk' : 'diff-ctx'}">{line}
</span>{/each}</pre>
          {:else}
            <div class="rounded-lg border p-6 text-center text-[12px]" style="border-color:var(--rule);color:var(--faint)">
              No diff recorded for this operation.
            </div>
          {/if}
        {/if}
      {/if}
    </Card>
  </div>
</div>

<style>
  .op-create { background: color-mix(in srgb, var(--grn) 15%, transparent); color: var(--grn); }
  .op-update { background: color-mix(in srgb, var(--acc) 15%, transparent); color: var(--acc); }
  .op-delete { background: color-mix(in srgb, var(--warn) 15%, transparent); color: var(--warn); }
  .op-revert { background: color-mix(in srgb, var(--acc2) 20%, transparent); color: var(--acc2); }

  .diff-line {
    display: block;
    white-space: pre;
  }
  .diff-add {
    background: color-mix(in srgb, var(--grn) 12%, transparent);
    color: var(--grn);
  }
  .diff-del {
    background: color-mix(in srgb, var(--warn) 12%, transparent);
    color: var(--warn);
  }
  .diff-hunk {
    background: color-mix(in srgb, var(--acc) 8%, transparent);
    color: var(--acc);
  }
  .diff-ctx {
    color: var(--soft);
  }
</style>
