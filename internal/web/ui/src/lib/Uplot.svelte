<script>
  import uPlot from "uplot";
  import "uplot/dist/uPlot.min.css";
  import { onMount, onDestroy } from "svelte";

  // uPlot wrapper for dense live streams. `makeOpts(width)` returns a uPlot opts
  // object; `data` is the [x, ...series] arrays, updated reactively.
  let { makeOpts, data = [[]], height = 230 } = $props();

  let el;
  let plot;
  let ro;

  onMount(() => {
    const width = el.clientWidth || 600;
    plot = new uPlot(makeOpts(width, height), data, el);
    ro = new ResizeObserver(() => {
      if (plot) plot.setSize({ width: el.clientWidth || width, height });
    });
    ro.observe(el);
  });

  onDestroy(() => {
    ro && ro.disconnect();
    plot && plot.destroy();
  });

  $effect(() => {
    if (plot && data) plot.setData(data);
  });
</script>

<div bind:this={el} style="width:100%"></div>
