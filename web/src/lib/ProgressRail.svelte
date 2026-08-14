<script lang="ts">
  import { Badge } from "foundationui/svelte";
  import { LIFECYCLE, lifecycleIndex, type Progress } from "./state";

  let { progress }: { progress: Progress } = $props();

  const reached = $derived(lifecycleIndex(progress.state));
</script>

<div class="flex flex-col gap-2">
  <ol class="flex flex-wrap items-center gap-1.5">
    {#each LIFECYCLE as step, i (step)}
      <li class="flex items-center gap-1.5">
        <span
          class="rounded-full px-2 py-0.5 text-xs font-mono transition-colors
            {i < reached ? 'bg-accent/15 text-accent' : ''}
            {i === reached ? 'bg-accent text-surface' : ''}
            {i > reached ? 'bg-glass-3 text-ink-secondary' : ''}"
        >
          {step}
        </span>
        {#if i < LIFECYCLE.length - 1}
          <span class="text-ink-secondary" aria-hidden="true">›</span>
        {/if}
      </li>
    {/each}
  </ol>

  <p class="text-sm text-ink-secondary">
    <Badge tone="accent">{progress.state}</Badge>
    <span class="ml-2">{progress.detail}</span>
  </p>
</div>
