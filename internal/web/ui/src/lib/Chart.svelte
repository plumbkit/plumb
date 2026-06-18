<script>
  import * as echarts from "echarts";
  import { onMount, onDestroy } from "svelte";

  // ECharts wrapper. `option` is reactive — when it changes the chart updates.
  // `height` is a CSS size. The instance is disposed on unmount and resizes with
  // its container.
  let { option = {}, height = "320px" } = $props();

  let el;
  let chart;
  let ro;

  onMount(() => {
    chart = echarts.init(el, null, { renderer: "canvas" });
    chart.setOption(option ?? {});
    ro = new ResizeObserver(() => chart && chart.resize());
    ro.observe(el);
  });

  onDestroy(() => {
    ro && ro.disconnect();
    chart && chart.dispose();
  });

  $effect(() => {
    if (chart && option) {
      chart.setOption(option, { notMerge: true });
    }
  });
</script>

<div bind:this={el} style="width:100%;height:{height}"></div>
