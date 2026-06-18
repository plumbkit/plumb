// charts.js — palette-aware ECharts/uPlot option builders, ported from the
// approved chart demos (~/plumb-web-chart-demo). Every builder takes the live
// palette so the charts track the selected plumb theme. Data shapes match the
// daemon API DTOs.
import uPlot from "uplot";

export function tip(P) {
  return {
    backgroundColor: P.card2,
    borderColor: P.rule,
    textStyle: { color: P.text },
    extraCssText: "border-radius:8px",
  };
}

function heatRamp(P) {
  return [P.card2, "#6b4a2e", P.acc2, P.acc, "#f0b487"];
}

// --- Dashboard ------------------------------------------------------------

// activityCalendar — GitHub-style contribution heatmap of daily call counts.
// `days` is an array of [isoDate, count].
export function activityCalendar(P, days, range) {
  return {
    tooltip: { ...tip(P), formatter: (p) => `${p.value[0]}<br/><b>${p.value[1]}</b> calls` },
    visualMap: {
      min: 0,
      max: Math.max(10, ...days.map((d) => d[1])),
      calculable: false,
      orient: "horizontal",
      right: 0,
      top: 0,
      inRange: { color: heatRamp(P) },
      textStyle: { color: P.soft },
    },
    calendar: {
      top: 46,
      left: 24,
      right: 14,
      cellSize: ["auto", 15],
      range,
      itemStyle: { color: P.card, borderColor: P.bg, borderWidth: 2 },
      splitLine: { show: false },
      yearLabel: { show: false },
      monthLabel: { color: P.faint, fontSize: 10 },
      dayLabel: { color: P.faint, fontSize: 10, firstDay: 1 },
    },
    series: [{ type: "heatmap", coordinateSystem: "calendar", data: days }],
  };
}

// topToolsBar — horizontal bar of the busiest tools.
export function topToolsBar(P, tools) {
  const names = tools.map((t) => t.tool).reverse();
  const calls = tools.map((t) => t.calls).reverse();
  return {
    tooltip: { ...tip(P), trigger: "axis", axisPointer: { type: "shadow" } },
    grid: { left: 8, right: 18, top: 8, bottom: 8, containLabel: true },
    xAxis: {
      type: "value",
      axisLabel: { color: P.faint, fontSize: 10 },
      splitLine: { lineStyle: { color: "color-mix(in srgb," + P.rule + " 60%, transparent)" } },
    },
    yAxis: {
      type: "category",
      data: names,
      axisLabel: { color: P.soft, fontSize: 11 },
      axisLine: { lineStyle: { color: P.rule } },
    },
    series: [
      {
        type: "bar",
        data: calls,
        itemStyle: {
          borderRadius: [0, 4, 4, 0],
          color: new (chartsGraphic()).LinearGradient(0, 0, 1, 0, [
            { offset: 0, color: P.acc2 },
            { offset: 1, color: P.acc },
          ]),
        },
        barWidth: "62%",
      },
    ],
  };
}

// savingsTreemap — token-savings split by axis then tool.
export function savingsTreemap(P, savings) {
  const cap = (savings.byTool || []).filter((t) => t.capability > 0);
  const eff = (savings.byTool || []).filter((t) => t.efficiency > 0);
  return {
    tooltip: { ...tip(P), formatter: (p) => `${p.name}<br/><b>${p.value}</b> tokens` },
    series: [
      {
        type: "treemap",
        roam: false,
        nodeClick: false,
        breadcrumb: { show: false },
        width: "100%",
        height: "100%",
        top: 6,
        bottom: 6,
        left: 0,
        right: 0,
        upperLabel: { show: true, height: 26, color: P.text, fontWeight: 700, fontSize: 12.5, padding: [0, 0, 0, 4] },
        itemStyle: { borderColor: P.bg, borderWidth: 2, gapWidth: 3 },
        levels: [
          { itemStyle: { borderColor: P.bg, borderWidth: 4, gapWidth: 4 }, upperLabel: { show: true } },
          { colorSaturation: [0.32, 0.6], itemStyle: { borderWidth: 2, gapWidth: 2 } },
        ],
        label: {
          show: true,
          position: "insideTopLeft",
          formatter: (p) => `{n|${p.name}}\n{v|${p.value}}`,
          rich: {
            n: { color: "rgba(18,19,16,.92)", fontSize: 11.5, fontWeight: 600, lineHeight: 16 },
            v: { color: "rgba(18,19,16,.7)", fontSize: 11, lineHeight: 14 },
          },
        },
        data: [
          {
            name: "Capability · enabled work",
            itemStyle: { color: P.grn },
            children: cap.map((t) => ({ name: t.tool, value: t.capability })),
          },
          {
            name: "Efficiency · cheaper",
            itemStyle: { color: P.acc },
            children: eff.map((t) => ({ name: t.tool, value: t.efficiency })),
          },
        ],
      },
    ],
  };
}

// gauges — CPU / heap / goroutines arc gauges.
export function gauges(P, m) {
  const g = chartsGraphic();
  const make = (val, max, c1, c2, name, unit, center) => ({
    type: "gauge",
    startAngle: 215,
    endAngle: -35,
    min: 0,
    max,
    center,
    radius: "74%",
    progress: {
      show: true,
      width: 13,
      roundCap: true,
      itemStyle: { color: new g.LinearGradient(0, 1, 1, 0, [{ offset: 0, color: c1 }, { offset: 1, color: c2 }]) },
    },
    axisLine: { roundCap: true, lineStyle: { width: 13, color: [[1, P.card2]] } },
    pointer: { show: false },
    axisTick: { show: false },
    splitLine: { show: false },
    axisLabel: { show: false },
    anchor: { show: false },
    title: { offsetCenter: [0, "74%"], color: P.soft, fontSize: 11, fontWeight: 500 },
    detail: { valueAnimation: true, offsetCenter: [0, "4%"], color: P.text, fontSize: 22, fontWeight: 700, formatter: (v) => v + unit },
    data: [{ value: val, name }],
  });
  return {
    series: [
      make(round1(m.cpuPercent), 100, P.acc2, P.acc, "CPU", "%", ["16.6%", "58%"]),
      make(Math.round((m.heapInuseBytes || 0) / 1048576), 512, P.forest, P.grn, "HEAP · MiB", "", ["50%", "58%"]),
      make(m.goroutines || 0, 200, "#4a3d63", P.cats?.[3] || "#6e5a7a", "GOROUTINES", "", ["83.3%", "58%"]),
    ],
  };
}

// liveStreamOpts — uPlot opts for the two synchronised CPU/heap series.
export function liveStreamOpts(P) {
  const splineFn = uPlot.paths.spline();
  const fillGrad = (c1, c2) => (u) => {
    const top = u.bbox?.top ?? 0;
    const h = u.bbox?.height;
    if (!isFinite(top) || !isFinite(h) || h <= 0) return c1;
    const grad = u.ctx.createLinearGradient(0, top, 0, top + h);
    grad.addColorStop(0, c1);
    grad.addColorStop(1, c2);
    return grad;
  };
  return (width, height) => ({
    width,
    height,
    cursor: { points: { size: 7 } },
    scales: { x: { time: true }, cpu: {}, heap: {} },
    axes: [
      { stroke: P.faint, grid: { stroke: "rgba(120,116,101,.18)" }, ticks: { stroke: P.rule, size: 4 }, space: 70 },
      { scale: "cpu", stroke: P.acc, grid: { stroke: "rgba(120,116,101,.12)" }, size: 42 },
      { scale: "heap", side: 1, stroke: P.grn, grid: { show: false }, size: 46 },
    ],
    series: [
      {},
      { label: "CPU %", scale: "cpu", stroke: P.acc, width: 2.5, paths: splineFn, fill: fillGrad(rgba(P.acc, 0.3), rgba(P.acc, 0)), points: { show: false } },
      { label: "Heap MiB", scale: "heap", stroke: P.grn, width: 2.5, paths: splineFn, fill: fillGrad(rgba(P.grn, 0.22), rgba(P.grn, 0)), points: { show: false } },
    ],
  });
}

// --- Sessions -------------------------------------------------------------

// latencyBoxplot — per-tool p0–p100 spread on a log scale. We approximate the
// box from avg/p95 since the DB exposes those.
export function latencyBoxplot(P, tools) {
  const rows = tools.filter((t) => t.calls > 0).slice(0, 10);
  const cats = rows.map((t) => t.tool);
  const boxes = rows.map((t) => {
    const avg = Math.max(0.5, t.avgMs);
    const p95 = Math.max(avg, t.p95Ms);
    return [avg * 0.3, avg * 0.7, avg, (avg + p95) / 2, p95];
  });
  return {
    tooltip: { ...tip(P), trigger: "item" },
    grid: { left: 8, right: 14, top: 14, bottom: 60, containLabel: true },
    xAxis: { type: "category", data: cats, axisLabel: { color: P.faint, fontSize: 10, rotate: 34 }, axisLine: { lineStyle: { color: P.rule } } },
    yAxis: { type: "log", axisLabel: { color: P.faint, fontSize: 10 }, splitLine: { lineStyle: { color: "rgba(120,116,101,.15)" } } },
    series: [
      {
        type: "boxplot",
        data: boxes,
        itemStyle: { color: rgba(P.acc, 0.18), borderColor: P.acc, borderWidth: 1.5 },
        emphasis: { itemStyle: { borderColor: P.grn } },
      },
    ],
  };
}

// bubbleScatter — x=calls, y=p95, bubble=tokens saved.
export function bubbleScatter(P, tools) {
  const data = tools
    .filter((t) => t.calls > 0)
    .map((t) => ({ value: [t.calls, Math.max(1, t.p95Ms), t.tokensSaved], name: t.tool }));
  return {
    tooltip: {
      ...tip(P),
      formatter: (p) => `<b>${p.data.name}</b><br/>${p.value[0]} calls · ${p.value[1]} ms p95<br/>${p.value[2]} tokens saved`,
    },
    grid: { left: 8, right: 20, top: 16, bottom: 40, containLabel: true },
    xAxis: { name: "calls", type: "log", nameTextStyle: { color: P.faint }, axisLabel: { color: P.faint, fontSize: 10 }, splitLine: { lineStyle: { color: "rgba(120,116,101,.12)" } } },
    yAxis: { name: "p95 ms", type: "log", nameTextStyle: { color: P.faint }, axisLabel: { color: P.faint, fontSize: 10 }, splitLine: { lineStyle: { color: "rgba(120,116,101,.12)" } } },
    series: [
      {
        type: "scatter",
        data,
        symbolSize: (v) => 8 + Math.sqrt(Math.max(0, v[2])) * 0.6,
        itemStyle: { color: P.acc, opacity: 0.8, borderColor: "rgba(0,0,0,.3)" },
        emphasis: { itemStyle: { opacity: 1, borderColor: P.text } },
      },
    ],
  };
}

// --- Topology -------------------------------------------------------------

// topologyGraph — force-directed map. `langs`/`nodes`/`links` derived from the
// status (we render a synthesised language-bucket graph when no live graph data
// is available, matching the demo's shape).
export function topologyForce(P, langs, nodes, links) {
  const cats = langs.map((name, i) => ({ name, itemStyle: { color: (P.cats || [])[i % (P.cats || [P.acc]).length] } }));
  return {
    tooltip: { ...tip(P), formatter: (p) => (p.dataType === "node" ? `<b>${p.data.name}</b>` : "") },
    legend: [{ data: langs, textStyle: { color: P.soft }, top: 0, icon: "circle" }],
    series: [
      {
        type: "graph",
        layout: "force",
        roam: true,
        draggable: true,
        categories: cats,
        data: nodes,
        links,
        force: { repulsion: 130, edgeLength: [40, 120], gravity: 0.08, friction: 0.12 },
        lineStyle: { color: "rgba(170,165,149,.22)", width: 1, curveness: 0.08 },
        emphasis: { focus: "adjacency", lineStyle: { width: 2, color: P.acc }, label: { show: true } },
        label: { position: "right", color: P.soft, fontSize: 11 },
      },
    ],
    animationDuration: 900,
  };
}

// --- helpers --------------------------------------------------------------

function rgba(hex, a) {
  const h = hex.replace("#", "");
  const n = parseInt(h.length === 3 ? h.replace(/(.)/g, "$1$1") : h, 16);
  return `rgba(${(n >> 16) & 255},${(n >> 8) & 255},${n & 255},${a})`;
}

function round1(v) {
  return Math.round((v || 0) * 10) / 10;
}

// chartsGraphic lazily resolves echarts.graphic so this module stays free of a
// static echarts import (the Chart component owns the import). It is set by the
// app at startup via setGraphic.
let _graphic = null;
export function setGraphic(g) {
  _graphic = g;
}
function chartsGraphic() {
  return _graphic || { LinearGradient: function () { return undefined; } };
}
