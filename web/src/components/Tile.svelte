<script lang="ts">
  import type { Component } from 'svelte'
  import { link } from '../lib/router.svelte'

  interface Props {
    label: string
    value: string
    sub?: string
    color: string
    icon: Component
    href?: string
  }
  let { label, value, sub, color, icon: Icon, href }: Props = $props()
</script>

<svelte:element
  this={href ? 'a' : 'div'}
  {href}
  onclick={href ? link : undefined}
  class="tile"
  style="--tile: {color}"
  role={href ? 'link' : undefined}
>
  <div class="text">
    <div class="value num">{value}</div>
    <div class="label">{label}</div>
    {#if sub}<div class="sub">{sub}</div>{/if}
  </div>
  <div class="icon" aria-hidden="true"><Icon size={46} strokeWidth={1.6} /></div>
</svelte:element>

<style>
  .tile {
    position: relative;
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 0.5rem;
    padding: 1rem 1.1rem;
    border-radius: var(--radius);
    background: var(--tile);
    color: #fff;
    box-shadow: var(--shadow);
    overflow: hidden;
    min-width: 0;
    text-decoration: none;
  }
  a.tile:hover {
    filter: brightness(1.06);
    text-decoration: none;
  }
  .value {
    font-size: 1.85rem;
    font-weight: 700;
    line-height: 1.1;
    white-space: nowrap;
  }
  .label {
    font-weight: 600;
    opacity: 0.95;
  }
  .sub {
    font-size: 0.8rem;
    opacity: 0.85;
    margin-top: 0.15rem;
  }
  .icon {
    opacity: 0.35;
    flex: none;
  }
  @media (max-width: 560px) {
    .tile {
      padding: 0.75rem 0.85rem;
    }
    .value {
      font-size: 1.45rem;
    }
    .sub,
    .icon {
      display: none;
    }
  }
</style>
