<script>
  import { onMount } from "svelte";
  import { getJSON } from "../lib/api.js";
  import { humanBytes } from "../lib/format.js";
  import Card from "../lib/Card.svelte";

  let model = $state({ workspace: "", workspaces: [], memories: [] });
  let selectedWs = $state("");
  let openMem = $state(null);
  let openBody = $state("");

  async function load(ws) {
    const q = ws ? `?workspace=${encodeURIComponent(ws)}` : "";
    model = await getJSON("/api/memory" + q);
    selectedWs = model.workspace;
    openMem = null;
    openBody = "";
  }

  async function openMemory(name) {
    const q = `?workspace=${encodeURIComponent(selectedWs)}`;
    const data = await getJSON(`/api/memory/${encodeURIComponent(name)}${q}`);
    openMem = name;
    openBody = data.content;
  }

  onMount(() => load(""));
</script>

<h1 class="text-lg font-semibold mb-4" style="color:var(--text)">Memory</h1>

<div class="flex items-center gap-3 mb-4">
  <span class="text-[12px]" style="color:var(--faint)">Workspace</span>
  <select
    class="rounded-lg border px-3 py-1.5 text-[13px]"
    style="border-color:var(--rule);background:var(--card);color:var(--text)"
    bind:value={selectedWs}
    onchange={() => load(selectedWs)}
  >
    {#if !model.workspaces?.length}
      <option value="">{model.workspace || "—"}</option>
    {/if}
    {#each model.workspaces as ws (ws.folder)}
      <option value={ws.folder}>{ws.name || ws.folder}</option>
    {/each}
  </select>
</div>

<div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
  <Card title="Memories" desc={selectedWs || "no workspace selected"}>
    {#if model.memories?.length}
      <div class="flex flex-col gap-1.5">
        {#each model.memories as m (m.name)}
          <button
            class="text-left rounded-lg border px-3 py-2 transition-colors"
            style={openMem === m.name
              ? "border-color:color-mix(in srgb,var(--acc) 40%,transparent);background:color-mix(in srgb,var(--acc) 8%,transparent)"
              : "border-color:var(--rule);background:transparent"}
            onclick={() => openMemory(m.name)}
          >
            <div class="flex items-center gap-2">
              <span class="font-medium text-[13px]" style="color:var(--text)">{m.name}</span>
              {#if !m.userAuthored}
                <span class="text-[10px] px-1.5 py-0.5 rounded-full" style="color:var(--faint);border:1px solid var(--rule)">{m.confidence || "generated"}</span>
              {/if}
              <span class="ml-auto text-[11px]" style="color:var(--faint)">{humanBytes(m.sizeBytes)}</span>
            </div>
            {#if m.description}
              <div class="text-[11.5px] mt-0.5" style="color:var(--soft)">{m.description}</div>
            {/if}
          </button>
        {/each}
      </div>
    {:else}
      <div class="text-[12px] py-8 text-center" style="color:var(--faint)">No memories for this workspace.</div>
    {/if}
  </Card>

  <Card title={openMem || "Reader"} desc={openMem ? "memory contents" : "select a memory to read"}>
    {#if openMem}
      <pre class="text-[12px] whitespace-pre-wrap font-mono leading-relaxed m-0" style="color:var(--soft)">{openBody}</pre>
    {:else}
      <div class="text-[12px] py-8 text-center" style="color:var(--faint)">Nothing open.</div>
    {/if}
  </Card>
</div>
