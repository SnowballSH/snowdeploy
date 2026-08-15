<script lang="ts">
  import { Button, Callout, Dialog } from "foundationui/svelte";
  import CommitLine from "./CommitLine.svelte";
  import { shortDigest, type Revision } from "./state";

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
    onConfirm: (service: string, digest: string) => Promise<void>;
  } = $props();

  let submitting = $state(false);
  let failure = $state<string | null>(null);

  $effect(() => {
    if (open) failure = null;
  });

  async function confirm() {
    if (submitting) return;
    submitting = true;
    try {
      await onConfirm(service, digest);
      open = false;
    } catch (err) {
      // The dialog is the surface the user is looking at; a failure must
      // land here, not only in a banner scrolled off the top of the page.
      failure = err instanceof Error ? err.message : String(err);
    } finally {
      submitting = false;
    }
  }
</script>

<Dialog
  bind:open
  size="sm"
  title="Deploy {service}?"
  description="This opens a pull request, merges it once checks pass, and restarts the service on the new image."
>
  <p class="font-mono text-sm" title={digest}>{shortDigest(digest)}</p>
  <CommitLine {revision} />
  {#if failure}
    <div class="mt-3">
      <Callout tone="warn">{failure}</Callout>
    </div>
  {/if}

  {#snippet footer()}
    <Button
      variant="secondary"
      size="sm"
      disabled={submitting}
      onclick={() => (open = false)}
    >
      Cancel
    </Button>
    <Button variant="primary" size="sm" disabled={submitting} onclick={confirm}>
      {submitting ? "Deploying…" : "Deploy"}
    </Button>
  {/snippet}
</Dialog>
