<script lang="ts">
  // Donut chart of parts of a whole, with a legend and the total inside.
  interface Part {
    label: string
    value: number
    color: string
  }
  interface Props {
    parts: Part[]
    center?: string
  }
  let { parts, center = 'calls' }: Props = $props()

  const total = $derived(parts.reduce((s, p) => s + p.value, 0))
  const R = 42
  const C = 2 * Math.PI * R
  const arcs = $derived.by(() => {
    let offset = 0
    return parts
      .filter((p) => p.value > 0)
      .map((p) => {
        const len = (p.value / total) * C
        const a = { ...p, dash: `${len} ${C - len}`, offset: -offset }
        offset += len
        return a
      })
  })
</script>

<div class="donut">
  <svg viewBox="0 0 100 100" role="img" aria-label="Call outcomes">
    <circle cx="50" cy="50" r={R} class="track" />
    {#each arcs as a (a.label)}
      <circle
        cx="50"
        cy="50"
        r={R}
        stroke={a.color}
        stroke-dasharray={a.dash}
        stroke-dashoffset={a.offset}
        transform="rotate(-90 50 50)"
      >
        <title>{a.label}: {a.value}</title>
      </circle>
    {/each}
    <text x="50" y="49" class="total">{total}</text>
    <text x="50" y="61" class="caption">{center}</text>
  </svg>
  <ul>
    {#each parts as p (p.label)}
      <li>
        <i style="background: {p.color}"></i>
        <span>{p.label}</span>
        <b class="num">{p.value}</b>
        <span class="muted num">{total ? Math.round((p.value / total) * 100) : 0}%</span>
      </li>
    {/each}
  </ul>
</div>

<style>
  .donut {
    display: grid;
    grid-template-columns: minmax(110px, 150px) 1fr;
    gap: 1rem;
    align-items: center;
  }
  svg {
    width: 100%;
  }
  circle {
    fill: none;
    stroke-width: 13;
  }
  .track {
    stroke: var(--panel-2);
  }
  text {
    text-anchor: middle;
    fill: var(--text);
  }
  .total {
    font-size: 17px;
    font-weight: 700;
  }
  .caption {
    font-size: 7.5px;
    fill: var(--muted);
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    gap: 0.35rem;
  }
  li {
    display: grid;
    grid-template-columns: auto 1fr auto 3.2rem;
    gap: 0.5rem;
    align-items: center;
  }
  li .muted {
    text-align: right;
  }
  i {
    width: 0.7rem;
    height: 0.7rem;
    border-radius: 3px;
  }
  @media (max-width: 420px) {
    .donut {
      grid-template-columns: 1fr;
    }
    svg {
      max-width: 160px;
      margin: 0 auto;
    }
  }
</style>
