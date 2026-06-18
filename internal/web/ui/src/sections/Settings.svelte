<script>
  import { onMount } from "svelte";
  import { getJSON, postJSON } from "../lib/api.js";
  import { themeState, setTheme } from "../lib/theme.svelte.js";
  import Card from "../lib/Card.svelte";

  let scopes = $state([]);
  let activeScope = $state("global");
  let status = $state("");

  const current = $derived(scopes.find((s) => s.scope === activeScope) ?? scopes[0]);

  // Group rows by their top-level section for display.
  const grouped = $derived.by(() => {
    const rows = current?.rows ?? [];
    const map = new Map();
    for (const r of rows) {
      if (!map.has(r.group)) map.set(r.group, []);
      map.get(r.group).push(r);
    }
    return [...map.entries()];
  });

  async function load() {
    const data = await getJSON("/api/settings");
    scopes = data.scopes;
    if (!scopes.find((s) => s.scope === activeScope)) activeScope = "global";
  }

  function tierBadge(tier) {
    if (tier === "live") return { sym: "¹", label: "live", color: "var(--grn)" };
    if (tier === "next-session") return { sym: "²", label: "next session", color: "var(--acc)" };
    return { sym: "³", label: "restart", color: "var(--faint)" };
  }

  async function save(row, value) {
    try {
      await postJSON("/api/settings", { scope: activeScope, key: row.key, value });
      status = `${row.key} saved`;
      await load();
      if (row.key === "ui.theme") await setTheme(value);
    } catch (e) {
      status = `error: ${e.message || e}`;
    }
    setTimeout(() => (status = ""), 3000);
  }

  function coerce(row, raw) {
    if (row.type === "int") return parseInt(raw, 10);
    if (row.type === "bool") return raw === true || raw === "true";
    if (row.type === "list") return raw.split(/[\n,]/).map((s) => s.trim()).filter(Boolean);
    return raw;
  }

  onMount(load);
</script>

<h1 class="text-lg font-semibold mb-4" style="color:var(--text)">Settings</h1>

{#if status}
  <div class="mb-3 text-[12.5px]" style="color:var(--soft)">{status}</div>
{/if}

<div class="flex gap-5">
  <!-- Scope column -->
  <div class="w-44 shrink-0 flex flex-col gap-1">
    <div class="text-[11px] uppercase tracking-wide mb-1" style="color:var(--faint)">Scope</div>
    {#each scopes as s (s.scope)}
      <button
        class="text-left px-3 py-2 rounded-lg text-[13px] truncate transition-colors"
        style={activeScope === s.scope
          ? "background:color-mix(in srgb,var(--acc) 14%,transparent);color:var(--acc);font-weight:600"
          : "color:var(--soft)"}
        onclick={() => (activeScope = s.scope)}
        title={s.scope}
      >
        {s.name}
      </button>
    {/each}
  </div>

  <!-- Rows -->
  <div class="flex-1 min-w-0 flex flex-col gap-4">
    <!-- Theme picker (global scope only) -->
    {#if current?.global}
      <Card title="Theme" desc="palette for the web UI and TUI">
        <div class="flex flex-wrap gap-2">
          {#each themeState.names as name (name)}
            <button
              class="px-3 py-1.5 rounded-lg border text-[12.5px]"
              style={themeState.name === name
                ? "border-color:var(--acc);color:var(--acc);background:color-mix(in srgb,var(--acc) 8%,transparent)"
                : "border-color:var(--rule);color:var(--soft)"}
              onclick={() => save({ key: "ui.theme" }, name)}
            >
              {name}
            </button>
          {/each}
        </div>
      </Card>
    {/if}

    {#each grouped as [group, rows] (group)}
      <Card title={group}>
        <div class="flex flex-col divide-y" style="--tw-divide-opacity:1">
          {#each rows as row (row.key)}
            {@const tier = tierBadge(row.reloadTier)}
            <div class="py-2.5 flex items-center gap-3" style="border-top:1px solid var(--rule)">
              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-2">
                  <span class="font-mono text-[12.5px]" style="color:var(--text)">{row.key}</span>
                  <span class="text-[10px]" style="color:{tier.color}" title={tier.label}>{tier.sym}</span>
                  {#if row.overridden}
                    <span class="text-[10px] px-1.5 rounded-full" style="color:var(--acc);border:1px solid color-mix(in srgb,var(--acc) 30%,transparent)">override</span>
                  {/if}
                </div>
                <div class="text-[11.5px] mt-0.5" style="color:var(--faint)">{row.help}</div>
              </div>

              <div class="shrink-0">
                {#if row.type === "bool"}
                  <button
                    class="px-3 py-1 rounded-lg border text-[12px]"
                    style="border-color:var(--rule);color:{row.value ? 'var(--grn)' : 'var(--faint)'};background:var(--card2)"
                    onclick={() => save(row, !row.value)}
                  >
                    {row.value ? "on" : "off"}
                  </button>
                {:else if row.type === "enum" && row.options?.length}
                  <select
                    class="rounded-lg border px-2.5 py-1 text-[12.5px]"
                    style="border-color:var(--rule);background:var(--card2);color:var(--text)"
                    value={row.value}
                    onchange={(e) => save(row, e.currentTarget.value)}
                  >
                    {#each row.options as opt (opt)}
                      <option value={opt}>{opt}</option>
                    {/each}
                  </select>
                {:else if row.type === "list"}
                  <input
                    class="rounded-lg border px-2.5 py-1 text-[12.5px] w-48 font-mono"
                    style="border-color:var(--rule);background:var(--card2);color:var(--text)"
                    value={(row.value || []).join(", ")}
                    onchange={(e) => save(row, coerce(row, e.currentTarget.value))}
                  />
                {:else}
                  <input
                    class="rounded-lg border px-2.5 py-1 text-[12.5px] w-32"
                    style="border-color:var(--rule);background:var(--card2);color:var(--text)"
                    value={row.value ?? ""}
                    onchange={(e) => save(row, coerce(row, e.currentTarget.value))}
                  />
                {/if}
              </div>
            </div>
          {/each}
        </div>
      </Card>
    {/each}
  </div>
</div>
