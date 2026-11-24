<script lang="ts">
  import "../styles/colors.css";
  import type { Snippet } from "svelte";

  let { children }: { children: Snippet } = $props();
</script>

{@render children()}
