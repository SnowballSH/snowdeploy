<script lang="ts">
  import { Badge } from "foundationui/svelte";
  import { isTerminal } from "./state";

  let { state }: { state: string } = $props();

  // Three visually distinct classes: success (aurora), a bad ending (accent,
  // with a glyph so it cannot be mistaken for progress), and still-moving
  // (neutral). Badge offers three tones, so the glyph carries the fourth bit.
  const tone = $derived(
    state === "healthy" ? "aurora" : isTerminal(state) ? "accent" : "neutral",
  );
  const label = $derived(
    state === "failed"
      ? "✕ failed"
      : state === "rolled-back"
        ? "↩ rolled back"
        : state,
  );
</script>

<Badge {tone}>{label}</Badge>
