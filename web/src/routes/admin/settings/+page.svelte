<script lang="ts">
    import { onMount } from "svelte";
    import { appConfig, type AppConfig, type TURNTestResponse } from "$lib/api/client";

    let config = $state<AppConfig | null>(null);
    let isLoading = $state(true);
    let isSaving = $state(false);
    let isUploadingLogo = $state(false);
    let isTesting = $state(false);
    let turnTestResults = $state<TURNTestResponse | null>(null);
    let error = $state("");
    let successMessage = $state("");

    // Form state
    let watermarkText = $state("");
    let turnUrl = $state("");
    let turnUsername = $state("");
    let turnCredential = $state("");
    let logoFile: File | null = null;

    onMount(async () => {
        await loadConfig();
    });

    async function loadConfig() {
        try {
            config = await appConfig.get();
            watermarkText = config.defaultWatermarkText || "";
            turnUrl = config.turnExternalUrl || "";
            turnUsername = config.turnExternalUsername || "";
        } catch (e) {
            console.error("Failed to load config", e);
            error = "Failed to load settings";
        } finally {
            isLoading = false;
        }
    }

    async function handleSaveWatermark(e: SubmitEvent) {
        e.preventDefault();
        isSaving = true;
        error = "";
        successMessage = "";

        try {
            config = await appConfig.update({
                defaultWatermarkText: watermarkText || undefined,
            });
            successMessage = "Watermark settings saved";
            setTimeout(() => (successMessage = ""), 3000);
        } catch (e: any) {
            error = e.message || "Failed to save settings";
        } finally {
            isSaving = false;
        }
    }

    async function handleSaveTurn(e: SubmitEvent) {
        e.preventDefault();
        isSaving = true;
        error = "";
        successMessage = "";

        try {
            config = await appConfig.update({
                turnExternalUrl: turnUrl || undefined,
                turnExternalUsername: turnUsername || undefined,
                turnExternalCredential: turnCredential || undefined,
            });
            turnCredential = ""; // Clear credential field after save
            successMessage = "TURN settings saved";
            setTimeout(() => (successMessage = ""), 3000);
        } catch (e: any) {
            error = e.message || "Failed to save settings";
        } finally {
            isSaving = false;
        }
    }

    async function handleLogoUpload(e: Event) {
        const input = e.target as HTMLInputElement;
        const file = input.files?.[0];
        if (!file) return;

        // Validate file type
        const allowedTypes = ["image/png", "image/jpeg", "image/webp"];
        if (!allowedTypes.includes(file.type)) {
            error = "Invalid file type. Use PNG, JPEG, or WebP.";
            return;
        }

        // Validate file size (1MB max)
        if (file.size > 1024 * 1024) {
            error = "File too large. Maximum size is 1MB.";
            return;
        }

        isUploadingLogo = true;
        error = "";
        successMessage = "";

        try {
            await appConfig.uploadLogo(file);
            // Reload config to get new logo URL
            config = await appConfig.get();
            successMessage = "Logo uploaded successfully";
            setTimeout(() => (successMessage = ""), 3000);
        } catch (e: any) {
            error = e.message || "Failed to upload logo";
        } finally {
            isUploadingLogo = false;
            input.value = "";
        }
    }

    async function handleDeleteLogo() {
        if (!confirm("Are you sure you want to delete the default logo?")) {
            return;
        }

        error = "";
        successMessage = "";

        try {
            await appConfig.deleteLogo();
            config = await appConfig.get();
            successMessage = "Logo deleted";
            setTimeout(() => (successMessage = ""), 3000);
        } catch (e: any) {
            error = e.message || "Failed to delete logo";
        }
    }

    function copyWhipFormat() {
        if (config?.whipFormat) {
            navigator.clipboard.writeText(config.whipFormat);
            successMessage = "WHIP URL format copied to clipboard";
            setTimeout(() => (successMessage = ""), 3000);
        }
    }

    async function handleTestTurn() {
        isTesting = true;
        turnTestResults = null;
        error = "";

        try {
            turnTestResults = await appConfig.testTurn();
            if (turnTestResults.success) {
                successMessage = "TURN connectivity test passed!";
                setTimeout(() => (successMessage = ""), 5000);
            }
        } catch (e: any) {
            error = e.message || "Failed to test TURN connectivity";
        } finally {
            isTesting = false;
        }
    }
</script>

<svelte:head>
    <title>Settings | Chromatic</title>
</svelte:head>

<div class="settings-page">
    <header class="page-header">
        <div class="header-left">
            <a href="/admin" class="back-link">&larr; Dashboard</a>
            <h1>Settings</h1>
        </div>
    </header>

    {#if error}
        <div class="error-message">{error}</div>
    {/if}

    {#if successMessage}
        <div class="success-message">{successMessage}</div>
    {/if}

    {#if isLoading}
        <div class="loading">
            <div class="waiting-spinner"></div>
        </div>
    {:else}
        <!-- Server Info -->
        <section class="card settings-section">
            <h2>Server Information</h2>
            <div class="info-row">
                <label>Public URL</label>
                <code>{config?.publicUrl || "Not configured"}</code>
            </div>
            <div class="info-row">
                <label>WHIP URL Format</label>
                <div class="copy-row">
                    <code>{config?.whipFormat || "Not configured"}</code>
                    <button
                        class="btn btn-secondary btn-sm"
                        onclick={copyWhipFormat}
                    >
                        Copy
                    </button>
                </div>
                <p class="hint">
                    Replace <code>{"{stream_key_token}"}</code> with your actual
                    stream key token from the Stream Keys page.
                </p>
            </div>
        </section>

        <!-- Default Watermark -->
        <section class="card settings-section">
            <h2>Default Watermark</h2>
            <p class="section-description">
                Set default watermark settings for new rooms. Individual rooms
                can override these.
            </p>

            <form onsubmit={handleSaveWatermark}>
                <div class="form-group">
                    <label for="watermarkText">Watermark Text Template</label>
                    <input
                        type="text"
                        id="watermarkText"
                        class="input"
                        bind:value={watermarkText}
                        placeholder="{{name}} - {{date}}"
                    />
                    <p class="hint">
                        Available variables: <code>{"{{name}}"}</code>,
                        <code>{"{{room}}"}</code>, <code>{"{{date}}"}</code>,
                        <code>{"{{time}}"}</code>
                    </p>
                </div>

                <button
                    type="submit"
                    class="btn btn-primary"
                    disabled={isSaving}
                >
                    {isSaving ? "Saving..." : "Save Watermark Text"}
                </button>
            </form>

            <hr />

            <div class="form-group">
                <label>Default Watermark Logo</label>
                {#if config?.defaultWatermarkLogoUrl}
                    <div class="logo-preview">
                        <img
                            src={config.defaultWatermarkLogoUrl}
                            alt="Current logo"
                        />
                        <button
                            class="btn btn-secondary btn-sm btn-danger-text"
                            onclick={handleDeleteLogo}
                        >
                            Delete Logo
                        </button>
                    </div>
                {/if}
                <div class="file-upload">
                    <input
                        type="file"
                        accept="image/png,image/jpeg,image/webp"
                        onchange={handleLogoUpload}
                        disabled={isUploadingLogo}
                    />
                    {#if isUploadingLogo}
                        <span class="upload-status">Uploading...</span>
                    {/if}
                </div>
                <p class="hint">
                    Recommended: PNG with transparency, max 500x500px, max 1MB
                </p>
            </div>
        </section>

        <!-- External TURN Server -->
        <section class="card settings-section">
            <h2>External TURN Server</h2>
            <p class="section-description">
                Configure a fallback TURN server (e.g., Twilio) for better NAT
                traversal. Leave empty to use only the built-in Coturn server.
            </p>

            <form onsubmit={handleSaveTurn}>
                <div class="form-group">
                    <label for="turnUrl">TURN Server URL</label>
                    <input
                        type="text"
                        id="turnUrl"
                        class="input"
                        bind:value={turnUrl}
                        placeholder="turn:global.turn.twilio.com:3478?transport=udp"
                    />
                </div>

                <div class="form-group">
                    <label for="turnUsername">Username</label>
                    <input
                        type="text"
                        id="turnUsername"
                        class="input"
                        bind:value={turnUsername}
                        placeholder="TURN username"
                    />
                </div>

                <div class="form-group">
                    <label for="turnCredential">Credential</label>
                    <input
                        type="password"
                        id="turnCredential"
                        class="input"
                        bind:value={turnCredential}
                        placeholder={config?.hasTurnCredential
                            ? "••••••••"
                            : "TURN credential"}
                    />
                    {#if config?.hasTurnCredential}
                        <p class="hint">
                            Credential is already set. Enter a new value to
                            change it.
                        </p>
                    {/if}
                </div>

                <div class="button-row">
                    <button
                        type="submit"
                        class="btn btn-primary"
                        disabled={isSaving}
                    >
                        {isSaving ? "Saving..." : "Save TURN Settings"}
                    </button>
                    <button
                        type="button"
                        class="btn btn-secondary"
                        onclick={handleTestTurn}
                        disabled={isTesting}
                    >
                        {isTesting ? "Testing..." : "Test TURN Connectivity"}
                    </button>
                </div>
            </form>

            {#if turnTestResults}
                <div class="turn-test-results">
                    <h3>Connectivity Test Results</h3>
                    <div class="test-summary" class:success={turnTestResults.success} class:failure={!turnTestResults.success}>
                        {turnTestResults.message}
                    </div>
                    {#if turnTestResults.results.length > 0}
                        <table class="test-results-table">
                            <thead>
                                <tr>
                                    <th>Server</th>
                                    <th>Type</th>
                                    <th>Protocol</th>
                                    <th>Status</th>
                                    <th>Latency</th>
                                </tr>
                            </thead>
                            <tbody>
                                {#each turnTestResults.results as result}
                                    <tr>
                                        <td><code>{result.server}</code></td>
                                        <td>{result.testType}</td>
                                        <td>{result.protocol || 'n/a'}</td>
                                        <td>
                                            {#if result.reachable}
                                                <span class="status-badge success">Reachable</span>
                                            {:else}
                                                <span class="status-badge failure">Failed</span>
                                            {/if}
                                        </td>
                                        <td>
                                            {#if result.latency}
                                                {result.latency}ms
                                            {:else if result.error}
                                                <span class="error-text" title={result.error}>Error</span>
                                            {:else}
                                                -
                                            {/if}
                                        </td>
                                    </tr>
                                {/each}
                            </tbody>
                        </table>
                    {/if}
                </div>
            {/if}
        </section>
    {/if}
</div>

<style>
    .settings-page {
        max-width: 800px;
        margin: 0 auto;
    }

    .page-header {
        display: flex;
        justify-content: space-between;
        align-items: flex-start;
        margin-bottom: var(--space-lg);
    }

    .header-left {
        display: flex;
        flex-direction: column;
        gap: var(--space-xs);
    }

    .back-link {
        font-size: 0.875rem;
        color: var(--color-text-muted);
    }

    .page-header h1 {
        margin: 0;
    }

    .settings-section {
        padding: var(--space-lg);
        margin-bottom: var(--space-lg);
    }

    .settings-section h2 {
        margin: 0 0 var(--space-sm);
        font-size: 1.125rem;
    }

    .section-description {
        color: var(--color-text-muted);
        font-size: 0.875rem;
        margin-bottom: var(--space-lg);
    }

    .form-group {
        margin-bottom: var(--space-md);
    }

    .form-group label {
        display: block;
        font-size: 0.875rem;
        font-weight: 500;
        margin-bottom: var(--space-xs);
    }

    .hint {
        font-size: 0.75rem;
        color: var(--color-text-subtle);
        margin-top: var(--space-xs);
    }

    .hint code {
        background: var(--color-surface-elevated);
        padding: 0.1em 0.3em;
        border-radius: var(--radius-sm);
    }

    .info-row {
        margin-bottom: var(--space-md);
    }

    .info-row label {
        display: block;
        font-size: 0.75rem;
        color: var(--color-text-muted);
        margin-bottom: var(--space-xs);
    }

    .info-row code {
        display: block;
        padding: var(--space-sm);
        background: var(--color-surface-elevated);
        border-radius: var(--radius-md);
        font-size: 0.875rem;
        overflow-x: auto;
        white-space: nowrap;
    }

    .copy-row {
        display: flex;
        gap: var(--space-sm);
        align-items: center;
    }

    .copy-row code {
        flex: 1;
    }

    .logo-preview {
        display: flex;
        align-items: center;
        gap: var(--space-md);
        margin-bottom: var(--space-md);
        padding: var(--space-md);
        background: var(--color-surface-elevated);
        border-radius: var(--radius-md);
    }

    .logo-preview img {
        max-width: 100px;
        max-height: 100px;
        object-fit: contain;
    }

    .file-upload {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
    }

    .upload-status {
        font-size: 0.875rem;
        color: var(--color-text-muted);
    }

    hr {
        border: none;
        border-top: 1px solid var(--color-border);
        margin: var(--space-lg) 0;
    }

    .error-message {
        padding: var(--space-sm) var(--space-md);
        background: rgba(239, 68, 68, 0.1);
        border: 1px solid var(--color-error);
        border-radius: var(--radius-md);
        color: var(--color-error);
        font-size: 0.875rem;
        margin-bottom: var(--space-md);
    }

    .success-message {
        padding: var(--space-sm) var(--space-md);
        background: rgba(34, 197, 94, 0.1);
        border: 1px solid var(--color-success);
        border-radius: var(--radius-md);
        color: var(--color-success);
        font-size: 0.875rem;
        margin-bottom: var(--space-md);
    }

    .loading {
        display: flex;
        justify-content: center;
        padding: var(--space-2xl);
    }

    .btn-secondary {
        background: var(--color-surface-hover);
        color: var(--color-text);
        border: 1px solid var(--color-border);
    }

    .btn-secondary:hover {
        background: var(--color-border);
    }

    .btn-sm {
        padding: var(--space-xs) var(--space-sm);
        font-size: 0.75rem;
    }

    .btn-danger-text {
        color: var(--color-error);
    }

    .btn-danger-text:hover {
        background: rgba(239, 68, 68, 0.1);
    }

    .button-row {
        display: flex;
        gap: var(--space-sm);
        flex-wrap: wrap;
    }

    .turn-test-results {
        margin-top: var(--space-lg);
        padding-top: var(--space-lg);
        border-top: 1px solid var(--color-border);
    }

    .turn-test-results h3 {
        font-size: 0.875rem;
        font-weight: 600;
        margin: 0 0 var(--space-sm);
    }

    .test-summary {
        padding: var(--space-sm) var(--space-md);
        border-radius: var(--radius-md);
        font-size: 0.875rem;
        margin-bottom: var(--space-md);
    }

    .test-summary.success {
        background: rgba(34, 197, 94, 0.1);
        border: 1px solid var(--color-success);
        color: var(--color-success);
    }

    .test-summary.failure {
        background: rgba(239, 68, 68, 0.1);
        border: 1px solid var(--color-error);
        color: var(--color-error);
    }

    .test-results-table {
        width: 100%;
        border-collapse: collapse;
        font-size: 0.875rem;
    }

    .test-results-table th,
    .test-results-table td {
        padding: var(--space-sm);
        text-align: left;
        border-bottom: 1px solid var(--color-border);
    }

    .test-results-table th {
        font-weight: 500;
        color: var(--color-text-muted);
        font-size: 0.75rem;
        text-transform: uppercase;
    }

    .test-results-table code {
        font-size: 0.75rem;
        background: var(--color-surface-elevated);
        padding: 0.1em 0.3em;
        border-radius: var(--radius-sm);
    }

    .status-badge {
        display: inline-block;
        padding: 0.15em 0.5em;
        border-radius: var(--radius-sm);
        font-size: 0.75rem;
        font-weight: 500;
    }

    .status-badge.success {
        background: rgba(34, 197, 94, 0.1);
        color: var(--color-success);
    }

    .status-badge.failure {
        background: rgba(239, 68, 68, 0.1);
        color: var(--color-error);
    }

    .error-text {
        color: var(--color-error);
        cursor: help;
        text-decoration: underline dotted;
    }
</style>
