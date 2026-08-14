<script lang="ts">
  import { Badge, Button, Panel } from "foundationui/svelte";
  import ProgressRail from "../lib/ProgressRail.svelte";
  import { href } from "../lib/router";
  import {
    shortDigest,
    updateAvailable,
    type Progress,
    type ServiceStatus,
  } from "../lib/state";

  let {
    services,
    active,
    onDeploy,
  }: {
    services: ServiceStatus[];
    active: Record<string, Progress>;
    onDeploy: (service: string, digest: string) => void;
  } = $props();
</script>

<div class="flex flex-col gap-4">
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
          {#if service.drifted}
            <Badge tone="accent">drifted</Badge>
          {:else if updateAvailable(service)}
            <Badge tone="aurora">update available</Badge>
          {:else}
            <Badge tone="neutral">in sync</Badge>
          {/if}

          <Button
            variant="primary"
            size="sm"
            disabled={!!active[service.name] || !updateAvailable(service)}
            onclick={() => onDeploy(service.name, service.latestAvailable)}
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
        </div>
        <div>
          <dt class="text-ink-secondary">manifest</dt>
          <dd class="font-mono" title={service.manifestDigest}>
            {shortDigest(service.manifestDigest)}
          </dd>
        </div>
        <div>
          <dt class="text-ink-secondary">latest available</dt>
          <dd class="font-mono" title={service.latestAvailable}>
            {shortDigest(service.latestAvailable)}
          </dd>
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
      <p class="text-ink-secondary">
        No managed services yet. Add a manifest to the configuration repository.
      </p>
    </Panel>
  {/each}
</div>
