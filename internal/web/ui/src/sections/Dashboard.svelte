<script>
  import { onMount, onDestroy } from "svelte";
  import { getJSON, stream } from "../lib/api.js";
  import { palette, themeState } from "../lib/theme.svelte.js";
  import { activityCalendar, topToolsBar, savingsTreemap, gauges, liveStreamOpts } from "../lib/charts.js";
  import { humanBytes, humanDuration, num } from "../lib/format.js";
  import Card from "../lib/Card.svelte";
  import Kpi from "../lib/Kpi.svelte";
  import Chart from "../lib/Chart.svelte";
  import Uplot from "../lib/Uplot.svelte";

  let data = $state(null);
  let metrics = $state({ cpuPercent: 0, heapInuseBytes: 0, goroutines: 0, rssBytes: 0 });
  let closeStream;
  let poll;

  // Live uPlot ring buffers.
  const N = 120;
  let t = [];
  let cpu = [];
  let heap = [];
  let streamData = $state([[], [], []]);

  const P = $derived(themeState.loaded ? palette() : palette());

  async function refresh() {
    try {
      data = await getJSON("/api/dashboard");
      if (data?.metrics) metrics = data.metrics;
    } catch {
      // transient — keep last good data
    }
  }

  function pushMetric(m) {
    metrics = m;
    const now = Math.floor(Date.now() / 1000);
    t.push(now);
    cpu.push(+(m.cpuPercent || 0).toFixed(2));
    heap.push(Math.round((m.heapInuseBytes || 0) / 1048576));
    if (t.length > N) {
      t.shift();
      cpu.shift();
      heap.shift();
    }
    streamData = [t.slice(), cpu.slice(), heap.slice()];
  }

  // Build a year of synthetic calendar cells anchored on the activity buckets so
  // the heatmap is populated even before a full year of history exists; the most
  // recent day reflects the live activity total.
  function calendarDays() {
    const days = [];
    const end = new Date();
    const total = data?.activity?.calls || 0;
    for (let i = 364; i >= 0; i--) {
      const d = new Date(end);
      d.setDate(d.getDate() - i);
      const iso = d.toISOString().slice(0, 10);
      const v = i === 0 ? total : 0;
      days.push([iso, v]);
    }
    return days;
  }

  const calRange = $derived(() => {
    const end = new Date();
    const start = new Date(end);
    start.setDate(start.getDate() - 364);
    return [start.toISOString().slice(0, 10), end.toISOString().slice(0, 10)];
  });

  onMount(async () => {
    await refresh();
    poll = setInterval(refresh, 5000);
    closeStream = stream("/api/stream/metrics", pushMetric);
  });
  onDestroy(() => {
    poll && clearInterval(poll);
    closeStream && closeStream();
  });
</script>

<h1 class="text-lg font-semibold mb-4" style="color:var(--text)">Dashboard</h1>

{#if data}
  <div class="grid grid-cols-2 lg:grid-cols-4 gap-4 mb-4">
    <Kpi label="Uptime" value={humanDuration(data.uptimeSeconds)} accent />
    <Kpi label="Sessions" value={num(data.sessions)} sub="active connections" />
    <Kpi label="Total calls" value={num(data.totalCalls)} sub="all tools" />
    <Kpi label="Memory (RSS)" value={metrics.rssAvailable ? humanBytes(metrics.rssBytes) : "—"} sub={`${metrics.goroutines} goroutines`} />
  </div>

  <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
    <Card span2 title="Live daemon metrics" badge="uPlot · streaming" desc="CPU % and heap, pushed over SSE">
      <div class="flex items-baseline gap-4 mb-1 text-[12px]" style="color:var(--soft)">
        <span><b style="color:var(--acc)">{(metrics.cpuPercent || 0).toFixed(1)}%</b> CPU</span>
        <span><b style="color:var(--grn)">{Math.round((metrics.heapInuseBytes || 0) / 1048576)}</b> MiB heap</span>
      </div>
      <Uplot makeOpts={liveStreamOpts(P)} data={streamData} height={210} />
    </Card>

    <Card span2 title="Daemon vitals" badge="gauges" desc="CPU, heap, goroutines">
      <Chart option={gauges(P, metrics)} height="190px" />
    </Card>

    <Card span2 title="Activity" badge="calendar heatmap" desc="tool calls per day">
      <Chart option={activityCalendar(P, calendarDays(), calRange())} height="200px" />
    </Card>

    <Card title="Top tools" badge="by calls" desc="busiest tools this daemon">
      {#if data.topTools?.length}
        <Chart option={topToolsBar(P, data.topTools)} height="320px" />
      {:else}
        <div class="text-[12px] py-8 text-center" style="color:var(--faint)">No tool calls recorded yet.</div>
      {/if}
    </Card>

    <Card title="Token savings" badge="treemap" desc="capability vs efficiency, by tool">
      {#if data.savings?.byTool?.length}
        <Chart option={savingsTreemap(P, data.savings)} height="320px" />
      {:else}
        <div class="text-[12px] py-8 text-center" style="color:var(--faint)">No savings recorded yet.</div>
      {/if}
    </Card>
  </div>
{/if}
