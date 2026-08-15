<script lang="ts">
  import type { Revision } from "./state";

  let { revision }: { revision: Revision | undefined } = $props();
</script>

{#if revision}
  <p class="mt-0.5 max-w-full truncate text-xs text-ink-secondary">
    <a
      href={revision.url}
      target="_blank"
      rel="noopener noreferrer"
      class="font-mono text-accent hover:underline"
    >
      {revision.sha.slice(0, 7)}
    </a>
    {#if revision.subject}
      <span title={revision.subject}>· {revision.subject}</span>
    {/if}
  </p>
{/if}
