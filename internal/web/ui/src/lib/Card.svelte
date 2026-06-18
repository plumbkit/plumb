<script>
  let { title = "", desc = "", badge = "", span2 = false, children } = $props();
</script>

<div
  class="rounded-2xl border p-4 relative overflow-hidden {span2 ? 'col-span-1 md:col-span-2' : ''}"
  style="border-color:var(--rule);background:linear-gradient(180deg,rgba(255,255,255,.012),transparent),var(--card)"
>
  {#if title}
    <h3 class="m-0 text-[14px] font-semibold flex items-center gap-2" style="color:var(--text)">
      {title}
      {#if badge}
        <span
          class="text-[10px] font-semibold px-2 py-0.5 rounded-full ml-auto"
          style="color:var(--acc);border:1px solid color-mix(in srgb,var(--acc) 30%,transparent);background:color-mix(in srgb,var(--acc) 8%,transparent)"
        >{badge}</span>
      {/if}
    </h3>
  {/if}
  {#if desc}
    <div class="text-[11.5px] mt-0.5 mb-2.5" style="color:var(--faint)">{desc}</div>
  {/if}
  {@render children?.()}
</div>
