<script lang="ts">
  import '@fontsource/work-sans/400.css';
  import '@fontsource/work-sans/500.css';
  import '@fontsource/work-sans/600.css';
  import '@fontsource/space-grotesk/400.css';
  import '@fontsource/space-grotesk/500.css';
  import '@fontsource/space-grotesk/600.css';
  import '@fontsource/space-grotesk/700.css';
  import '../app.css';

  let { children } = $props();
</script>

<svelte:head>
  <title>Chromatic</title>
  <meta name="description" content="Self-hosted streaming platform for professional colorists" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
</svelte:head>

{@render children()}
