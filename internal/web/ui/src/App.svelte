<script>
  import * as echarts from "echarts";
  import { onMount } from "svelte";
  import { loadTheme, themeState } from "./lib/theme.svelte.js";
  import { setGraphic } from "./lib/charts.js";
  import Dashboard from "./sections/Dashboard.svelte";
  import Sessions from "./sections/Sessions.svelte";
  import Memory from "./sections/Memory.svelte";
  import Logs from "./sections/Logs.svelte";
  import Settings from "./sections/Settings.svelte";

  const sections = [
    { id: "dashboard", label: "Dashboard", component: Dashboard },
    { id: "sessions", label: "Sessions", component: Sessions },
    { id: "memory", label: "Memory", component: Memory },
    { id: "logs", label: "Logs", component: Logs },
    { id: "settings", label: "Settings", component: Settings },
  ];

  let active = $state("dashboard");
  let ready = $state(false);
  let error = $state("");

  setGraphic(echarts.graphic);

  const Current = $derived(sections.find((s) => s.id === active)?.component ?? Dashboard);

  onMount(async () => {
    try {
      await loadTheme();
      ready = true;
    } catch (e) {
      error = e.message || String(e);
    }
  });
</script>

<div class="flex min-h-screen">
  <!-- Sidebar -->
  <nav
    class="w-52 shrink-0 border-r flex flex-col gap-1 p-4"
    style="border-color:var(--rule);background:color-mix(in srgb,var(--card) 55%,transparent)"
  >
    <div class="flex items-center gap-2.5 mb-5 px-1">
      <div
        class="w-8 h-8 rounded-[9px]"
        style="background:linear-gradient(135deg,var(--acc),var(--acc2));box-shadow:0 8px 26px color-mix(in srgb,var(--acc) 30%,transparent)"
      ></div>
      <div>
        <div class="font-semibold text-[15px] leading-tight" style="color:var(--text)">plumb</div>
        <div class="text-[11px]" style="color:var(--faint)">web</div>
      </div>
    </div>

    {#each sections as s (s.id)}
      <button
        class="text-left px-3 py-2 rounded-lg text-[13.5px] transition-colors"
        style={active === s.id
          ? "background:color-mix(in srgb,var(--acc) 14%,transparent);color:var(--acc);font-weight:600"
          : "color:var(--soft)"}
        onclick={() => (active = s.id)}
      >
        {s.label}
      </button>
    {/each}

    <div class="mt-auto text-[11px] px-1" style="color:var(--faint)">
      <span class="live-dot"></span> live · {themeState.name}
    </div>
  </nav>

  <!-- Content -->
  <main class="flex-1 min-w-0 p-6 lg:p-8 overflow-x-hidden">
    {#if error}
      <div class="rounded-xl border p-5" style="border-color:var(--rule);background:var(--card);color:var(--warn)">
        Failed to load: {error}
      </div>
    {:else if !ready}
      <div style="color:var(--faint)">Loading…</div>
    {:else}
      <Current />
    {/if}
  </main>
</div>
