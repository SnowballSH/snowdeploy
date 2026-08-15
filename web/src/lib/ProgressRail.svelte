<script lang="ts">
  import { Badge } from "foundationui/svelte";
  import { LIFECYCLE, lifecycleIndex, type Progress } from "./state";

  let { progress }: { progress: Progress } = $props();

  const reached = $derived(lifecycleIndex(progress.state));

  /**
   * Steps that have an artifact link to the outside record: the pull request,
   * its checks tab, and the merge commit. A step is only linked once the
   * daemon has told us the artifact exists.
   */
  function stepHref(step: string): string | undefined {
    switch (step) {
      case "pr-open":
        return progress.prUrl;
      case "checks":
        return progress.prUrl ? `${progress.prUrl}/checks` : undefined;
      case "merged":
        return progress.mergeUrl;
      default:
        return undefined;
    }
  }
</script>

<div class="flex flex-col gap-2">
  <ol class="flex flex-wrap items-center gap-1.5">
    {#each LIFECYCLE as step, i (step)}
      {@const href = stepHref(step)}
      <li class="flex items-center gap-1.5">
        {#if href}
          <a
            {href}
            target="_blank"
            rel="noopener noreferrer"
            class="rounded-full px-2 py-0.5 text-xs font-mono underline decoration-dotted underline-offset-2 transition-colors
              {i < reached ? 'bg-accent/15 text-accent' : ''}
              {i === reached ? 'bg-accent text-surface' : ''}
              {i > reached ? 'bg-glass-3 text-ink-secondary' : ''}"
          >
            {step}
          </a>
        {:else}
          <span
            class="rounded-full px-2 py-0.5 text-xs font-mono transition-colors
              {i < reached ? 'bg-accent/15 text-accent' : ''}
              {i === reached ? 'bg-accent text-surface' : ''}
              {i > reached ? 'bg-glass-3 text-ink-secondary' : ''}"
          >
            {step}
          </span>
        {/if}
        {#if i < LIFECYCLE.length - 1}
          <span class="text-ink-secondary" aria-hidden="true">›</span>
        {/if}
      </li>
    {/each}
  </ol>

  <p class="text-sm text-ink-secondary">
    <Badge tone="accent">{progress.state}</Badge>
    <span class="ml-2">{progress.detail}</span>
    {#if progress.prNumber && progress.prUrl}
      <a
        href={progress.prUrl}
        target="_blank"
        rel="noopener noreferrer"
        class="ml-2 text-accent hover:underline"
      >
        PR #{progress.prNumber}
      </a>
    {/if}
  </p>
</div>
