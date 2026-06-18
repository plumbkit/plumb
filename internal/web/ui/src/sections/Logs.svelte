<script>
  import { onMount, onDestroy } from "svelte";
  import { stream } from "../lib/api.js";
  import Card from "../lib/Card.svelte";

  let lines = $state([]);
  let filter = $state("");
  let paused = $state(false);
  let closeStream;
  const MAX = 2000;

  const shown = $derived(filter ? lines.filter((l) => l.toLowerCase().includes(filter.toLowerCase())) : lines);

  function lineColour(l) {
    if (/\b(ERROR|level=ERROR|"level":"error")\b/i.test(l)) return "var(--warn)";
    if (/\b(WARN|level=WARN|"level":"warn")\b/i.test(l)) return "var(--acc)";
    if (/\b(DEBUG|level=DEBUG)\b/i.test(l)) return "var(--faint)";
    return "var(--soft)";
  }

  onMount(() => {
    closeStream = stream("/api/stream/logs", (line) => {
      if (paused) return;
      lines.push(typeof line === "string" ? line.replace(/\n$/, "") : JSON.stringify(line));
      if (lines.length > MAX) lines.splice(0, lines.length - MAX);
    });
  });
  onDestroy(() => closeStream && closeStream());
</script>

<h1 class="text-lg font-semibold mb-4" style="color:var(--text)">Logs</h1>

<div class="flex items-center gap-3 mb-4">
  <input
    class="rounded-lg border px-3 py-1.5 text-[13px] flex-1 max-w-md"
    style="border-color:var(--rule);background:var(--card);color:var(--text)"
    placeholder="filter…"
    bind:value={filter}
  />
  <button
    class="rounded-lg border px-3 py-1.5 text-[12.5px]"
    style="border-color:var(--rule);background:var(--card);color:{paused ? 'var(--acc)' : 'var(--soft)'}"
    onclick={() => (paused = !paused)}
  >
    {paused ? "Resume" : "Pause"}
  </button>
  <button
    class="rounded-lg border px-3 py-1.5 text-[12.5px]"
    style="border-color:var(--rule);background:var(--card);color:var(--soft)"
    onclick={() => (lines = [])}
  >
    Clear
  </button>
  <span class="text-[11px] ml-auto" style="color:var(--faint)"><span class="live-dot"></span> {shown.length} lines</span>
</div>

<Card>
  <div class="font-mono text-[11.5px] leading-relaxed overflow-auto" style="max-height:65vh">
    {#if shown.length}
      {#each shown as l, i (i)}
        <div style="color:{lineColour(l)}" class="whitespace-pre-wrap break-all">{l}</div>
      {/each}
    {:else}
      <div class="text-center py-8" style="color:var(--faint)">Waiting for log output…</div>
    {/if}
  </div>
</Card>
