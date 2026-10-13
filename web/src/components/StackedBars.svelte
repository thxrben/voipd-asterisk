<script lang="ts" generics="T extends { start: string }">
  // Stacked bar chart in plain SVG: one bar per time bucket, one stacked
  // segment per series, hover tooltip with the bucket's numbers.
  import { clock, day } from '../lib/format'

  interface Series {
    key: keyof T & string
    label: string
    color: string
  }
  interface Props {
    data: T[]
    series: Series[]
    bucketSeconds: number
    height?: number
  }
  let { data, series, bucketSeconds, height = 220 }: Props = $props()

  const W = 1000 // viewBox width; the SVG scales to its container
  const padL = 34
  const padB = 22
  const padT = 8
  let hover = $state<number | null>(null)
  const hovered = $derived(hover !== null ? data[hover] : undefined)

  const value = (b: T, k: string) => Number((b as Record<string, unknown>)[k] ?? 0)
  const totals = $derived(data.map((b) => series.reduce((s, x) => s + value(b, x.key), 0)))
  const max = $derived(niceMax(Math.max(1, ...totals)))
  const plotW = W - padL
  const plotH = $derived(height - padB - padT)
  const bw = $derived(data.length ? plotW / data.length : plotW)

  // Call counts are integers: the axis max is a "nice" even number so the
  // middle tick is an integer too (1 call max -> ticks 0, 1, 2).
  function niceMax(v: number): number {
    if (v <= 2) return 2
    const p = 10 ** Math.floor(Math.log10(v))
    for (const m of [1, 2, 4, 5, 10]) if (m * p >= v && (m * p) % 2 === 0) return m * p
    return 10 * p
  }
  const ticks = $derived([0, max / 2, max])

  function y(v: number): number {
    return padT + plotH - (v / max) * plotH
  }

  // Label every n-th bucket so labels don't overlap (~8 labels).
  const labelEvery = $derived(Math.max(1, Math.ceil(data.length / 8)))
  function label(b: T): string {
    const d = new Date(b.start)
    return bucketSeconds >= 86400 ? day(d) : clock(d)
  }

  function onMove(e: PointerEvent) {
    const svg = e.currentTarget as SVGSVGElement
    const r = svg.getBoundingClientRect()
    const x = ((e.clientX - r.left) / r.width) * W - padL
    const i = Math.floor(x / bw)
    hover = i >= 0 && i < data.length ? i : null
  }
</script>

<div class="chart">
  <svg
    viewBox="0 0 {W} {height}"
    preserveAspectRatio="none"
    style="height: {height}px"
    role="img"
    aria-label="Calls over time"
    onpointermove={onMove}
    onpointerleave={() => (hover = null)}
  >
    {#each ticks as t (t)}
      <line class="grid" x1={padL} x2={W} y1={y(t)} y2={y(t)} />
    {/each}
    {#each data as b, i (b.start)}
      {@const x = padL + i * bw + bw * 0.12}
      {#if hover === i}
        <rect class="hover" x={padL + i * bw} y={padT} width={bw} height={plotH} />
      {/if}
      {#each series as s, si (s.key)}
        {@const below = series.slice(0, si).reduce((acc, x2) => acc + value(b, x2.key), 0)}
        {@const v = value(b, s.key)}
        {#if v > 0}
          <rect x={x} width={bw * 0.76} y={y(below + v)} height={y(below) - y(below + v)} fill={s.color} />
        {/if}
      {/each}
    {/each}
  </svg>
  <div class="yaxis" style="height: {height - padB}px">
    {#each ticks as t (t)}
      <span style="top: {y(t)}px">{Math.round(t)}</span>
    {/each}
  </div>
  <div class="xaxis">
    {#each data as b, i (b.start)}
      {#if i % labelEvery === 0}
        <span style="left: {((padL + i * bw + bw / 2) / W) * 100}%">{label(b)}</span>
      {/if}
    {/each}
  </div>
  {#if hovered && hover !== null}
    <div class="tip" style="left: {((padL + hover * bw + bw / 2) / W) * 100}%">
      <strong>{label(hovered)}</strong>
      {#each series as s (s.key)}
        <div><i style="background: {s.color}"></i>{s.label}: <b class="num">{value(hovered, s.key)}</b></div>
      {/each}
    </div>
  {/if}
  <div class="legend">
    {#each series as s (s.key)}
      <span><i style="background: {s.color}"></i>{s.label}</span>
    {/each}
  </div>
</div>

<style>
  .chart {
    position: relative;
  }
  svg {
    /* Fixed pixel height, stretched horizontally: y coordinates are pixels,
       which keeps the HTML axis labels aligned. */
    display: block;
    width: 100%;
  }
  .grid {
    stroke: var(--border);
    stroke-width: 1;
    vector-effect: non-scaling-stroke;
  }
  .hover {
    fill: var(--panel-2);
  }
  .yaxis {
    position: absolute;
    left: 0;
    top: 0;
    width: 30px;
    pointer-events: none;
  }
  .yaxis span {
    position: absolute;
    right: 4px;
    transform: translateY(-50%);
    font-size: 0.72rem;
    color: var(--muted);
  }
  .xaxis {
    position: relative;
    height: 1.2rem;
    margin-top: -1.3rem;
    pointer-events: none;
  }
  .xaxis span {
    position: absolute;
    transform: translateX(-50%);
    font-size: 0.72rem;
    color: var(--muted);
    white-space: nowrap;
  }
  .tip {
    position: absolute;
    top: 0.25rem;
    transform: translateX(-50%);
    background: var(--panel);
    border: 1px solid var(--border);
    box-shadow: var(--shadow);
    border-radius: 8px;
    padding: 0.45rem 0.6rem;
    font-size: 0.8rem;
    pointer-events: none;
    white-space: nowrap;
    z-index: 2;
  }
  i {
    display: inline-block;
    width: 0.65rem;
    height: 0.65rem;
    border-radius: 2px;
    margin-right: 0.35rem;
    vertical-align: -0.05rem;
  }
  .legend {
    display: flex;
    flex-wrap: wrap;
    gap: 0.4rem 1rem;
    margin-top: 0.75rem;
    font-size: 0.82rem;
    color: var(--muted);
  }
</style>
