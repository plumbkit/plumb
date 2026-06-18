<script>
  import { onMount, onDestroy } from "svelte";
  import { getJSON } from "../lib/api.js";
  import { palette, themeState } from "../lib/theme.svelte.js";
  import { latencyBoxplot, bubbleScatter, topologyForce } from "../lib/charts.js";
  import { num } from "../lib/format.js";
  import Card from "../lib/Card.svelte";
  import Chart from "../lib/Chart.svelte";

  let sessions = $state([]);
  let stats = $state({ tools: [] });
  let topo = $state(null);
  let poll;

  const P = $derived(themeState.loaded ? palette() : palette());

  async function refresh() {
    try {
      [sessions, stats, topo] = await Promise.all([
        getJSON("/api/sessions"),
        getJSON("/api/stats"),
        getJSON("/api/topology"),
      ]);
    } catch {
      // keep last good
    }
  }

  function healthColour(h) {
    if (h === "blocked") return "var(--warn)";
    if (h === "idle") return "var(--faint)";
    return "var(--grn)";
  }

  // Build a small force-graph from the topology language list: a node per
  // language plus a hub, edges to the hub — a legible whole-repo shape that
  // works without per-edge graph data (the GL-scale variant degrades to this 2D
  // force layout, which is also our headless fallback).
  function graphData() {
    const langs = topo?.languages?.length ? topo.languages : ["code"];
    const nodes = [
      { id: "repo", name: topo?.workspace?.split("/").pop() || "workspace", category: 0, symbolSize: 34, label: { show: true } },
    ];
    const links = [];
    langs.forEach((l, i) => {
      nodes.push({ id: "l" + i, name: l, category: i, symbolSize: 14 + (i % 3) * 6, label: { show: true } });
      links.push({ source: "l" + i, target: "repo" });
      for (let j = 0; j < 3; j++) {
        const id = `n${i}_${j}`;
        nodes.push({ id, name: l + "." + ["pkg", "type", "fn"][j], category: i, symbolSize: 7, label: { show: false } });
        links.push({ source: id, target: "l" + i });
      }
    });
    return { langs, nodes, links };
  }

  onMount(async () => {
    await refresh();
    poll = setInterval(refresh, 5000);
  });
  onDestroy(() => poll && clearInterval(poll));
</script>

<h1 class="text-lg font-semibold mb-4" style="color:var(--text)">Sessions</h1>

<Card title="Active sessions" desc="connections served by this daemon">
  {#if sessions.length}
    <div class="overflow-x-auto">
      <table class="w-full text-[12.5px] border-collapse">
        <thead>
          <tr style="color:var(--faint)" class="text-left">
            <th class="py-2 pr-3 font-medium">Name</th>
            <th class="py-2 pr-3 font-medium">Client</th>
            <th class="py-2 pr-3 font-medium">Language</th>
            <th class="py-2 pr-3 font-medium">Workspace</th>
            <th class="py-2 pr-3 font-medium text-right">Calls</th>
            <th class="py-2 pr-3 font-medium">Last seen</th>
            <th class="py-2 pr-3 font-medium">Health</th>
          </tr>
        </thead>
        <tbody>
          {#each sessions as s (s.id)}
            <tr style="border-top:1px solid var(--rule)">
              <td class="py-2 pr-3" style="color:var(--text)">{s.name || "—"}</td>
              <td class="py-2 pr-3" style="color:var(--soft)">{s.client || "—"}{s.clientVersion ? " " + s.clientVersion : ""}</td>
              <td class="py-2 pr-3" style="color:var(--soft)">{s.language || "none"}</td>
              <td class="py-2 pr-3 font-mono text-[11.5px]" style="color:var(--faint)">{s.folderShort}</td>
              <td class="py-2 pr-3 text-right" style="color:var(--text)">{num(s.calls)}</td>
              <td class="py-2 pr-3" style="color:var(--faint)">{s.lastSeen || "—"}</td>
              <td class="py-2 pr-3">
                <span style="color:{healthColour(s.health)}">● {s.health || "active"}</span>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {:else}
    <div class="text-[12px] py-8 text-center" style="color:var(--faint)">No active sessions.</div>
  {/if}
</Card>

<div class="grid grid-cols-1 md:grid-cols-2 gap-4 mt-4">
  <Card title="Latency distribution" badge="boxplot" desc="per-tool spread, ms · log scale">
    {#if stats.tools?.length}
      <Chart option={latencyBoxplot(P, stats.tools)} height="320px" />
    {:else}
      <div class="text-[12px] py-8 text-center" style="color:var(--faint)">No data yet.</div>
    {/if}
  </Card>

  <Card title="Cost vs frequency" badge="bubble scatter" desc="x=calls · y=p95 · size=tokens saved">
    {#if stats.tools?.length}
      <Chart option={bubbleScatter(P, stats.tools)} height="320px" />
    {:else}
      <div class="text-[12px] py-8 text-center" style="color:var(--faint)">No data yet.</div>
    {/if}
  </Card>

  <Card span2 title="Topology" badge="force graph · drag · zoom" desc={topo?.available ? `${num(topo.totalNodes)} nodes · ${num(topo.totalEdges)} edges · ${(topo.languages || []).join(", ")}` : "no index for the active workspace"}>
    {#if topo?.available}
      {@const g = graphData()}
      <Chart option={topologyForce(P, g.langs, g.nodes, g.links)} height="420px" />
    {:else}
      <div class="text-[12px] py-12 text-center" style="color:var(--faint)">Topology index not available for the active workspace.</div>
    {/if}
  </Card>
</div>
