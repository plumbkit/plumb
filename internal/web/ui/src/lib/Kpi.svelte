<script>
  let { label = "", value = "", sub = "", accent = false } = $props();
</script>

<div
  class="rounded-2xl border p-4"
  style="border-color:var(--rule);background:linear-gradient(180deg,rgba(255,255,255,.012),transparent),var(--card)"
>
  <div class="text-[11px] uppercase tracking-wide" style="color:var(--faint)">{label}</div>
  <div class="text-[26px] font-bold mt-1 leading-none" style="color:{accent ? 'var(--acc)' : 'var(--text)'}">
    {value}
  </div>
  {#if sub}
    <div class="text-[11.5px] mt-1" style="color:var(--soft)">{sub}</div>
  {/if}
</div>
