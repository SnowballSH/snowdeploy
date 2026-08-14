<script lang="ts">
  import { Badge } from "foundationui/svelte";

  let { state }: { state: string } = $props();

  // Only "healthy" is a success; a rollback and a failure both need to read as
  // something that happened, never as a quiet neutral.
  const tone = $derived(state === "healthy" ? "aurora" : "accent");
</script>

<Badge {tone}>{state}</Badge>
