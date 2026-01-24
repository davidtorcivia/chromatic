<script lang="ts">
  let adminToken = $state('');
  let isLoading = $state(false);
  let error = $state('');

  async function handleLogin(e: SubmitEvent) {
    e.preventDefault();
    isLoading = true;
    error = '';

    try {
      // Validate token by making a test request
      const res = await fetch('/api/rooms', {
        headers: {
          'Authorization': `Bearer ${adminToken}`
        }
      });

      if (res.ok) {
        // Store token and redirect
        localStorage.setItem('chromatic_admin_token', adminToken);
        window.location.href = '/admin';
      } else {
        error = 'Invalid admin token';
      }
    } catch (err) {
      error = 'Connection error. Is the server running?';
    } finally {
      isLoading = false;
    }
  }
</script>

<main class="landing">
  <div class="landing-content">
    <div class="logo">
      <h1>Chromatic</h1>
      <p class="tagline">Self-hosted streaming for color-critical review</p>
    </div>

    <form class="login-form card" onsubmit={handleLogin}>
      <h2>Admin Login</h2>
      
      {#if error}
        <div class="error-message">{error}</div>
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
        {isLoading ? 'Verifying...' : 'Login'}
      </button>
    </form>

    <div class="features">
      <div class="feature">
        <h3>🎨 Color Fidelity</h3>
        <p>High-bitrate streaming optimized for XDR displays</p>
      </div>
      <div class="feature">
        <h3>⚡ Sub-second Latency</h3>
        <p>Real-time WebRTC streaming from DaVinci Resolve</p>
      </div>
      <div class="feature">
        <h3>👆 Interactive Review</h3>
        <p>Laser pointer annotations visible to all participants</p>
      </div>
    </div>
  </div>
</main>

<style>
  .landing {
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: var(--space-lg);
    background: radial-gradient(ellipse at top, #1a1a2e 0%, var(--color-bg) 70%);
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
    font-size: 3rem;
    font-weight: 700;
    background: linear-gradient(135deg, #3b82f6, #8b5cf6);
    -webkit-background-clip: text;
    -webkit-text-fill-color: transparent;
    background-clip: text;
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

  .error-message {
    padding: var(--space-md);
    background: rgba(239, 68, 68, 0.1);
    border: 1px solid var(--color-error);
    border-radius: var(--radius-md);
    color: var(--color-error);
    font-size: 0.875rem;
    margin-bottom: var(--space-lg);
  }

  .features {
    display: grid;
    gap: var(--space-lg);
    text-align: left;
  }

  .feature {
    padding: var(--space-md);
    background: var(--color-surface);
    border-radius: var(--radius-md);
    border: 1px solid var(--color-border);
  }

  .feature h3 {
    font-size: 1rem;
    margin-bottom: var(--space-xs);
  }

  .feature p {
    font-size: 0.875rem;
    color: var(--color-text-muted);
  }
</style>
