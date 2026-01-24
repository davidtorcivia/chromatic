<script lang="ts">
  import '../app.css';
  
  let { children } = $props();
</script>

<svelte:head>
  <title>Chromatic</title>
  <meta name="description" content="Self-hosted streaming platform for professional colorists" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin="anonymous">
  <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&display=swap" rel="stylesheet">
</svelte:head>

{@render children()}
