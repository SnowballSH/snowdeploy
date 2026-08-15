<script lang="ts">
  import { Button, Panel, Skeleton } from "foundationui/svelte";
  import CommitLine from "../lib/CommitLine.svelte";
  import DeployConfirm from "../lib/DeployConfirm.svelte";
  import ProgressRail from "../lib/ProgressRail.svelte";
  import StatusBadge from "../lib/StatusBadge.svelte";
  import { href } from "../lib/router";
  import {
    shortDigest,
    updateAvailable,
    type Progress,
    type Revision,
    type ServiceStatus,
  } from "../lib/state";

  let {
    services,
    active,
    revisions,
    loaded,
    error,
    onDeploy,
  }: {
    services: ServiceStatus[];
    active: Record<string, Progress>;
    revisions: Record<string, Revision>;
    loaded: boolean;
    error: string | null;
    onDeploy: (service: string, digest: string) => Promise<void>;
  } = $props();

  let confirmOpen = $state(false);
  let confirmService = $state("");
  let confirmDigest = $state("");

  function askDeploy(service: string, digest: string) {
    confirmService = service;
    confirmDigest = digest;
    confirmOpen = true;
  }
</script>

<div class="flex flex-col gap-4">
  {#if !loaded}
    <Panel tier="glass" padding="lg">
      <div class="flex flex-col gap-3" aria-label="Loading services">
        <Skeleton class="h-6 w-40" />
        <Skeleton class="h-4 w-64" />
        <Skeleton class="h-4 w-full" />
      </div>
    </Panel>
  {:else}
    {#each services as service (service.name)}
      <Panel tier="glass" padding="lg">
        <div class="flex flex-wrap items-start justify-between gap-4">
          <div class="flex flex-col gap-1">
            <a
              href={href({ name: "service", service: service.name })}
              class="text-lg font-medium text-ink hover:text-accent"
            >
              {service.name}
            </a>
            <p class="font-mono text-xs text-ink-secondary">
              {service.repository}
            </p>
          </div>

          <div class="flex items-center gap-2">
            <StatusBadge {service} progress={active[service.name]} />
            <Button
              variant="primary"
              size="sm"
              disabled={!!active[service.name] || !updateAvailable(service)}
              onclick={() => askDeploy(service.name, service.latestAvailable)}
            >
              Deploy
            </Button>
          </div>
        </div>

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

        {#if active[service.name]}
          <div class="mt-4 border-t border-line pt-4">
            <ProgressRail progress={active[service.name]!} />
          </div>
        {/if}
      </Panel>
    {:else}
      <Panel tier="glass" padding="lg">
        {#if error}
          <p class="text-ink-secondary">
            The service catalog could not be read: {error}
          </p>
        {:else}
          <p class="text-ink-secondary">
            No managed services yet. Add a manifest to the configuration
            repository.
          </p>
        {/if}
      </Panel>
    {/each}
  {/if}
</div>

<DeployConfirm
  bind:open={confirmOpen}
  service={confirmService}
  digest={confirmDigest}
  revision={revisions[confirmDigest]}
  onConfirm={onDeploy}
/>
