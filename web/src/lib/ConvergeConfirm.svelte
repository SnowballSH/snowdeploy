<script lang="ts">
  import { Button, Callout, Dialog } from "foundationui/svelte";
  import CommitLine from "./CommitLine.svelte";
  import {
    joinedNotice,
    shortDigest,
    type Accepted,
    type Revision,
  } from "./state";

  let {
    open = $bindable(false),
    service,
    digest,
    revision,
    onConfirm,
  }: {
    open?: boolean;
    service: string;
    digest: string;
    revision: Revision | undefined;
    onConfirm: (service: string) => Promise<Accepted>;
  } = $props();

  let submitting = $state(false);
  let failure = $state<string | null>(null);
  let joined = $state<string | null>(null);

  $effect(() => {
    if (open) {
      failure = null;
      joined = null;
    }
  });

  async function confirm() {
    if (submitting) return;
    submitting = true;
    try {
      const accepted = await onConfirm(service);
      // A joined click started nothing; say so before the dialog goes away,
      // or the operator reads the pending run as the one they just asked for.
      joined = joinedNotice(service, "converge", accepted);
      if (!joined) open = false;
    } catch (err) {
      failure = err instanceof Error ? err.message : String(err);
    } finally {
      submitting = false;
    }
  }
</script>

<Dialog
  bind:open
  size="sm"
  title="Converge {service}?"
  description="This re-renders the unit from the merged manifest and restarts the service on the digest main pins. There is no automatic rollback: if the health probe fails, the run ends failed and the unit stays as rendered."
>
  <p class="text-sm text-ink-secondary">
    main pins
    <span class="font-mono text-ink" title={digest}>{shortDigest(digest)}</span>
    now; the daemon syncs again and applies whatever main pins when this run
    reaches the front of the queue.
  </p>
  <CommitLine {revision} />
  {#if joined}
    <div class="mt-3">
      <Callout tone="info">{joined}</Callout>
    </div>
  {/if}
  {#if failure}
    <div class="mt-3">
      <Callout tone="warn">{failure}</Callout>
    </div>
  {/if}

  {#snippet footer()}
    {#if joined}
      <Button variant="primary" size="sm" onclick={() => (open = false)}>
        Close
      </Button>
    {:else}
      <Button
        variant="secondary"
        size="sm"
        disabled={submitting}
        onclick={() => (open = false)}
      >
        Cancel
      </Button>
      <Button
        variant="primary"
        size="sm"
        disabled={submitting}
        onclick={confirm}
      >
        {submitting ? "Converging…" : "Converge"}
      </Button>
    {/if}
  {/snippet}
</Dialog>
