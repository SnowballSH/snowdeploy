<script lang="ts">
  import { Button, Panel } from "foundationui/svelte";
  import OutcomeBadge from "../lib/OutcomeBadge.svelte";
  import ProgressRail from "../lib/ProgressRail.svelte";
  import { href } from "../lib/router";
  import {
    shortDigest,
    type HistoryEntry,
    type Progress,
    type ServiceStatus,
  } from "../lib/state";

  let {
    name,
    service,
    progress,
    history,
    onRollback,
  }: {
    name: string;
    service: ServiceStatus | undefined;
    progress: Progress | undefined;
    history: HistoryEntry[];
    onRollback: (service: string, digest: string) => void;
  } = $props();

  function when(value: string): string {
    if (!value) return "—";
    const at = new Date(value);
    return Number.isNaN(at.getTime()) ? "—" : at.toLocaleString();
  }
</script>

<div class="flex flex-col gap-4">
  <a href={href({ name: "services" })} class="text-sm text-ink-secondary hover:text-accent">
    ← all services
  </a>

  <Panel tier="glass" padding="lg">
    <h2 class="text-xl font-medium text-ink">{name}</h2>
    {#if service}
      <p class="mt-1 font-mono text-xs text-ink-secondary">{service.repository}</p>
      <p class="mt-3 font-mono text-sm" title={service.runningDigest}>
        running {shortDigest(service.runningDigest)} · manifest {shortDigest(
          service.manifestDigest,
        )}
      </p>
    {/if}

    {#if progress}
      <div class="mt-4 border-t border-line pt-4">
        <ProgressRail {progress} />
      </div>
    {/if}
  </Panel>

  <Panel tier="glass" padding="lg">
    <h3 class="text-base font-medium text-ink">Deploy history</h3>

    <ul class="mt-3 flex flex-col divide-y divide-line">
      {#each history as entry (entry.ID)}
        <li class="flex flex-wrap items-center justify-between gap-3 py-3">
          <div class="flex flex-col gap-1">
            <div class="flex items-center gap-2">
              <OutcomeBadge state={entry.State} />
              <span class="font-mono text-sm" title={entry.NewDigest}>
                {shortDigest(entry.NewDigest)}
              </span>
            </div>
            <p class="text-xs text-ink-secondary">
              {entry.Action}
              {#if entry.Actor}· {entry.Actor}{/if}
              · {when(entry.StartedAt)}
              {#if entry.PRNumber}· PR #{entry.PRNumber}{/if}
            </p>
            {#if entry.Detail}
              <p class="text-xs text-ink-secondary">{entry.Detail}</p>
            {/if}
          </div>

          <Button
            variant="secondary"
            size="sm"
            disabled={!!progress ||
              !entry.NewDigest ||
              entry.NewDigest === service?.manifestDigest}
            onclick={() => onRollback(name, entry.NewDigest)}
          >
            Roll back to this
          </Button>
        </li>
      {:else}
        <li class="py-3 text-sm text-ink-secondary">
          Nothing deployed through snowdeploy yet.
        </li>
      {/each}
    </ul>
  </Panel>
</div>
