<script lang="ts">
  import { goto } from '$app/navigation';
  import { auth } from '$lib/api/client';

  let adminToken = $state('');
  let isLoading = $state(false);
  let error = $state('');
  let destroyed = false;

  $effect(() => {
    return () => {
      destroyed = true;
    };
  });

  async function handleLogin(e: SubmitEvent) {
    e.preventDefault();
    isLoading = true;
    error = '';

    try {
      // Login via secure httpOnly cookie
      await auth.login(adminToken);
      if (destroyed) return;
      // Redirect on success
      void goto('/admin');
    } catch (err) {
      if (destroyed) return;
      if (err instanceof Error) {
        error = err.message === 'Invalid token'
          ? 'Invalid admin token'
          : err.message;
      } else {
        error = 'Connection error. Is the server running?';
      }
    } finally {
      if (!destroyed) isLoading = false;
    }
  }
</script>

<main class="landing">
  <div class="landing-content">
    <div class="logo">
      <h1>Chromatic<span class="logo-mark">.</span></h1>
      <p class="tagline">Self-hosted streaming for color-critical review</p>
    </div>

    <form class="login-form card" onsubmit={handleLogin}>
      <h2>Admin Login</h2>

      {#if error}
        <div class="alert alert-error">{error}</div>
      {/if}

      <div class="form-group">
        <label for="token">Admin Token</label>
        <input
          type="password"
          id="token"
          class="input"
          bind:value={adminToken}
          placeholder="Enter your admin token"
          required
        />
      </div>

      <button type="submit" class="btn btn-primary" disabled={isLoading}>
        {#if isLoading}
          <span class="btn-spinner" aria-hidden="true"></span>
          Verifying...
        {:else}
          Login
        {/if}
      </button>
    </form>
  </div>
</main>

<style>
  .landing {
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: var(--space-lg);
    background:
      radial-gradient(ellipse at top, rgba(72, 182, 166, 0.06) 0%, transparent 70%),
      var(--color-bg);
  }

  .landing-content {
    max-width: 400px;
    width: 100%;
    text-align: center;
  }

  .logo {
    margin-bottom: var(--space-2xl);
  }

  .logo h1 {
    font-size: 2.5rem;
    font-weight: 700;
    letter-spacing: -0.02em;
    color: var(--color-text);
  }

  .logo-mark {
    color: var(--color-primary);
  }

  .tagline {
    color: var(--color-text-muted);
    margin-top: var(--space-sm);
  }

  .login-form {
    text-align: left;
    margin-bottom: var(--space-2xl);
  }

  .login-form h2 {
    margin-bottom: var(--space-lg);
    text-align: center;
    font-size: 1.25rem;
  }

  .form-group {
    margin-bottom: var(--space-lg);
  }

  .form-group label {
    display: block;
    font-size: 0.875rem;
    font-weight: 500;
    margin-bottom: var(--space-sm);
    color: var(--color-text-muted);
  }

  .login-form .btn {
    width: 100%;
  }
</style>
