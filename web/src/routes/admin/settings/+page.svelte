<script lang="ts">
    import { onMount } from "svelte";
    import { fade } from "svelte/transition";
    import { appConfig, type AppConfig, type TURNTestResponse } from "$lib/api/client";
    import ConfirmDialog from "$lib/components/ConfirmDialog.svelte";
    import CopyField from "$lib/components/CopyField.svelte";

    let config = $state<AppConfig | null>(null);
    let isLoading = $state(true);
    let isSaving = $state(false);
    let isUploadingLogo = $state(false);
    let isTesting = $state(false);
    let turnTestResults = $state<TURNTestResponse | null>(null);
    let error = $state("");
    let successMessage = $state("");
    let confirmDeleteLogoOpen = $state(false);

    // Form state
    let watermarkText = $state("");
    let turnUrl = $state("");
    let turnUsername = $state("");
    let turnCredential = $state("");
    let clearTurnCredential = $state(false);

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
            const normalizedTurnURL = turnUrl
                .split(",")
                .map((entry) => entry.trim())
                .filter(Boolean)
                .join(",");
            const turnCredentialPayload = clearTurnCredential
                ? ""
                : turnCredential || undefined;

            config = await appConfig.update({
                turnExternalUrl: normalizedTurnURL,
                turnExternalUsername: turnUsername.trim(),
                turnExternalCredential: turnCredentialPayload,
            });
            turnUrl = config.turnExternalUrl || "";
            turnUsername = config.turnExternalUsername || "";
            turnCredential = ""; // Clear credential field after save
            clearTurnCredential = false;
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
        confirmDeleteLogoOpen = false;
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

    function turnModeLabel(mode?: string) {
        switch (mode) {
            case "external":
                return "External TURN only";
            case "hybrid":
                return "Hybrid (self-hosted + external)";
            case "self-hosted":
                return "Self-hosted TURN only";
            default:
                return "Not configured";
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
        <div class="alert alert-error" transition:fade={{ duration: 150 }}>{error}</div>
    {/if}

    {#if successMessage}
        <div class="alert alert-success" transition:fade={{ duration: 150 }}>{successMessage}</div>
    {/if}

    {#if isLoading}
        <div class="settings-skeletons" aria-busy="true" aria-label="Loading settings">
            {#each [0, 1, 2] as i (i)}
                <div class="skeleton skeleton-card"></div>
            {/each}
        </div>
    {:else}
        <!-- Server Info -->
        <section class="card settings-section">
            <h2>Server Information</h2>
            <div class="info-row">
                <span class="field-label">Public URL</span>
                <code>{config?.publicUrl || "Not configured"}</code>
            </div>
            <div class="info-row">
                <span class="field-label">TURN mode</span>
                <code>{turnModeLabel(config?.turnMode)}</code>
            </div>
            <div class="info-row">
                <span class="field-label">Cloudflare TURN</span>
                <code
                    >{config?.turnCloudflareConfigured
                        ? "Configured via environment"
                        : "Not configured"}</code
                >
            </div>
            <div class="info-row">
                <CopyField
                    label="WHIP URL Format"
                    value={config?.whipFormat || "Not configured"}
                />
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
                        placeholder={"{{name}} - {{date}}"}
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
                    {#if isSaving}
                        <span class="btn-spinner" aria-hidden="true"></span>
                        Saving...
                    {:else}
                        Save Watermark Text
                    {/if}
                </button>
            </form>

            <hr />

            <div class="form-group">
                <span class="field-label">Default Watermark Logo</span>
                {#if config?.defaultWatermarkLogoUrl}
                    <div class="logo-preview">
                        <img
                            src={config.defaultWatermarkLogoUrl}
                            alt="Current logo"
                        />
                        <button
                            class="btn btn-secondary btn-sm btn-danger-text"
                            onclick={() => (confirmDeleteLogoOpen = true)}
                        >
                            Delete Logo
                        </button>
                    </div>
                {/if}
                <div class="file-upload">
                    <label class="btn btn-secondary file-btn" class:disabled={isUploadingLogo}>
                        {#if isUploadingLogo}
                            <span class="btn-spinner" aria-hidden="true"></span>
                            Uploading...
                        {:else}
                            Choose Logo File
                        {/if}
                        <input
                            type="file"
                            class="visually-hidden-input"
                            accept="image/png,image/jpeg,image/webp"
                            onchange={handleLogoUpload}
                            disabled={isUploadingLogo}
                        />
                    </label>
                </div>
                <p class="hint">
                    Recommended: PNG with transparency, max 500x500px, max 1MB
                </p>
            </div>
        </section>

        <!-- TURN Connectivity -->
        <section class="card settings-section">
            <h2>TURN Connectivity</h2>
            <p class="section-description">
                Cloudflare TURN is the recommended non-self-hosted option and
                is configured with environment variables. Use this form to add
                static external TURN URLs and credentials as fallback or for
                other providers.
            </p>

            {#if config?.turnCloudflareConfigured}
                <p class="hint">
                    Cloudflare TURN credentials are generated dynamically
                    server-side. Leave username/credential empty unless you are
                    adding a non-Cloudflare provider.
                </p>
            {/if}

            <form onsubmit={handleSaveTurn}>
                <div class="form-group">
                    <label for="turnUrl">TURN URL(s)</label>
                    <input
                        type="text"
                        id="turnUrl"
                        class="input"
                        bind:value={turnUrl}
                        placeholder="turn:turn.cloudflare.com:3478?transport=udp,turns:turn.cloudflare.com:443?transport=tcp"
                    />
                    <p class="hint">
                        Comma-separated URLs are supported.
                    </p>
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
                        oninput={() => (clearTurnCredential = false)}
                        placeholder={config?.hasTurnCredential
                            ? clearTurnCredential
                                ? "Will be cleared on save"
                                : "••••••••"
                            : "TURN credential"}
                    />
                    {#if config?.hasTurnCredential}
                        <p class="hint">
                            Credential is already set. Enter a new value to
                            rotate it or clear it explicitly.
                        </p>
                        <div class="button-row">
                            <button
                                type="button"
                                class="btn btn-secondary btn-sm"
                                onclick={() => {
                                    clearTurnCredential = !clearTurnCredential;
                                    if (clearTurnCredential) {
                                        turnCredential = "";
                                    }
                                }}
                            >
                                {clearTurnCredential
                                    ? "Credential Will Be Cleared"
                                    : "Clear Saved Credential"}
                            </button>
                        </div>
                    {/if}
                </div>

                <div class="button-row">
                    <button
                        type="submit"
                        class="btn btn-primary"
                        disabled={isSaving}
                    >
                        {#if isSaving}
                            <span class="btn-spinner" aria-hidden="true"></span>
                            Saving...
                        {:else}
                            Save TURN Settings
                        {/if}
                    </button>
                    <button
                        type="button"
                        class="btn btn-secondary"
                        onclick={handleTestTurn}
                        disabled={isTesting}
                    >
                        {#if isTesting}
                            <span class="btn-spinner" aria-hidden="true"></span>
                            Testing...
                        {:else}
                            Test TURN Connectivity
                        {/if}
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

<ConfirmDialog
    open={confirmDeleteLogoOpen}
    title="Delete the default logo?"
    body="Rooms using the default watermark logo will no longer display it."
    confirmLabel="Delete Logo"
    danger
    onConfirm={handleDeleteLogo}
    onCancel={() => (confirmDeleteLogoOpen = false)}
/>

<style>
    .settings-page {
        max-width: 800px;
        margin: 0 auto;
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

    .form-group .field-label {
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

    .info-row .field-label {
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

    .file-btn {
        position: relative;
        overflow: hidden;
    }

    .file-btn.disabled {
        opacity: 0.55;
        cursor: not-allowed;
    }

    .visually-hidden-input {
        position: absolute;
        width: 1px;
        height: 1px;
        padding: 0;
        margin: -1px;
        overflow: hidden;
        clip: rect(0, 0, 0, 0);
        white-space: nowrap;
        border: 0;
    }

    hr {
        border: none;
        border-top: 1px solid var(--color-border);
        margin: var(--space-lg) 0;
    }

    .settings-skeletons {
        display: flex;
        flex-direction: column;
        gap: var(--space-lg);
    }

    .skeleton-card {
        min-height: 180px;
    }

    .btn-danger-text {
        color: var(--color-error);
    }

    .btn-danger-text:hover {
        background: var(--color-error-bg);
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
        background: var(--color-success-bg);
        border: 1px solid var(--color-success);
        color: var(--color-success);
    }

    .test-summary.failure {
        background: var(--color-error-bg);
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

    .error-text {
        color: var(--color-error);
        cursor: help;
        text-decoration: underline dotted;
    }
</style>
