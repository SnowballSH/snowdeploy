<script lang="ts">
  import { Button, Panel, Skeleton } from "foundationui/svelte";
  import CommitLine from "../lib/CommitLine.svelte";
  import DeployConfirm from "../lib/DeployConfirm.svelte";
  import OutcomeBadge from "../lib/OutcomeBadge.svelte";
  import ProgressRail from "../lib/ProgressRail.svelte";
  import StatusBadge from "../lib/StatusBadge.svelte";
  import { href } from "../lib/router";
  import {
    shortDigest,
    updateAvailable,
    prLink,
    type HistoryEntry,
    type Progress,
    type Revision,
    type ServiceStatus,
  } from "../lib/state";

  let {
    name,
    service,
    progress,
    history,
    revisions,
    repoWebUrl,
    loaded,
    onDeploy,
    onRollback,
  }: {
    name: string;
    service: ServiceStatus | undefined;
    progress: Progress | undefined;
    /** undefined = not fetched yet; [] = fetched and genuinely empty. */
    history: HistoryEntry[] | undefined;
    revisions: Record<string, Revision>;
    repoWebUrl: string;
    loaded: boolean;
    onDeploy: (service: string, digest: string) => Promise<void>;
    onRollback: (service: string, digest: string) => Promise<void>;
  } = $props();

  let confirmOpen = $state(false);

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
    {#if !service && !loaded}
      <div class="flex flex-col gap-3" aria-label="Loading service">
        <Skeleton class="h-6 w-40" />
        <Skeleton class="h-4 w-64" />
      </div>
    {:else}
      <div class="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h2 class="text-xl font-medium text-ink">{name}</h2>
          {#if service}
            <p class="mt-1 font-mono text-xs text-ink-secondary">
              {service.repository}
            </p>
          {:else}
            <p class="mt-1 text-sm text-ink-secondary">
              This service is not in the catalog.
            </p>
          {/if}
        </div>
        {#if service}
          <div class="flex items-center gap-2">
            <StatusBadge {service} {progress} />
            <Button
              variant="primary"
              size="sm"
              disabled={!!progress || !updateAvailable(service)}
              onclick={() => (confirmOpen = true)}
            >
              Deploy
            </Button>
          </div>
        {/if}
      </div>
      {#if service}
        <dl class="mt-4 grid grid-cols-1 gap-3 text-sm sm:grid-cols-3">
          <div>
            <dt class="text-ink-secondary">running</dt>
            <dd class="font-mono" title={service.runningDigest}>
              {shortDigest(service.runningDigest)}
            </dd>
            <CommitLine revision={revisions[service.runningDigest]} />
          </div>
          <div>
            <dt class="text-ink-secondary">manifest</dt>
            <dd class="font-mono" title={service.manifestDigest}>
              {shortDigest(service.manifestDigest)}
            </dd>
            <CommitLine revision={revisions[service.manifestDigest]} />
          </div>
          <div>
            <dt class="text-ink-secondary">latest available</dt>
            <dd class="font-mono" title={service.latestAvailable}>
              {shortDigest(service.latestAvailable)}
            </dd>
            <CommitLine revision={revisions[service.latestAvailable]} />
          </div>
        </dl>
      {/if}

      {#if progress}
        <div class="mt-4 border-t border-line pt-4">
          <ProgressRail {progress} />
        </div>
      {/if}
    {/if}
  </Panel>

  <Panel tier="glass" padding="lg">
    <h3 class="text-base font-medium text-ink">Deploy history</h3>

    {#if history === undefined}
      <div class="mt-3 flex flex-col gap-2" aria-label="Loading history">
        <Skeleton class="h-4 w-full" />
        <Skeleton class="h-4 w-3/4" />
      </div>
    {:else}
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
              <CommitLine revision={revisions[entry.NewDigest]} />
              <p class="text-xs text-ink-secondary">
                {entry.Action}
                {#if entry.Actor}· {entry.Actor}{/if}
                · {when(entry.StartedAt)}
                {#if entry.PRNumber}
                  ·
                  {#if prLink(repoWebUrl, entry.PRNumber)}
                    <a
                      href={prLink(repoWebUrl, entry.PRNumber)}
                      target="_blank"
                      rel="noopener noreferrer"
                      class="text-accent hover:underline"
                    >
                      PR #{entry.PRNumber}
                    </a>
                  {:else}
                    PR #{entry.PRNumber}
                  {/if}
                {/if}
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
              onclick={() => void onRollback(name, entry.NewDigest).catch(() => {})}
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
    {/if}
  </Panel>
</div>

{#if service}
  <DeployConfirm
    bind:open={confirmOpen}
    service={name}
    digest={service.latestAvailable}
    revision={revisions[service.latestAvailable]}
    onConfirm={onDeploy}
  />
{/if}
