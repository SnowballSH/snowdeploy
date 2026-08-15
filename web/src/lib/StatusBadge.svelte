<script lang="ts">
  import { Badge } from "foundationui/svelte";
  import { updateAvailable, type Progress, type ServiceStatus } from "./state";

  let {
    service,
    progress,
  }: { service: ServiceStatus; progress: Progress | undefined } = $props();
</script>

{#if progress}
  <Badge tone="accent">deploying</Badge>
{:else if !service.manifestDigest}
  <!-- An unreadable manifest is unknown state, not agreement. -->
  <Badge tone="accent">unknown</Badge>
{:else if !service.runningDigest}
  <Badge tone="accent">not running</Badge>
{:else if service.drifted}
  <Badge tone="accent">drifted</Badge>
{:else if updateAvailable(service)}
  <Badge tone="aurora">update available</Badge>
{:else if !service.latestAvailable}
  <Badge tone="neutral">registry unreachable</Badge>
{:else}
  <Badge tone="neutral">in sync</Badge>
{/if}
