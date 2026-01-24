<script lang="ts">
    import { onMount } from "svelte";
    import { streamKeys, type StreamKey } from "$lib/api/client";

    let keys = $state<StreamKey[]>([]);
    let isLoading = $state(true);
    let isCreating = $state(false);
    let showCreateForm = $state(false);
    let newKeyName = $state("");
    let error = $state("");
    let copiedId = $state<string | null>(null);

    onMount(async () => {
        await loadKeys();
    });

    async function loadKeys() {
        try {
            keys = await streamKeys.list();
        } catch (e) {
            console.error("Failed to load stream keys", e);
        } finally {
            isLoading = false;
        }
    }

    async function handleCreate(e: SubmitEvent) {
        e.preventDefault();
        if (!newKeyName.trim()) return;

        isCreating = true;
        error = "";

        try {
            const newKey = await streamKeys.create(newKeyName.trim());
            keys = [...keys, newKey];
            newKeyName = "";
            showCreateForm = false;
        } catch (e: any) {
            error = e.message || "Failed to create stream key";
        } finally {
            isCreating = false;
        }
    }

    async function handleDelete(id: string) {
        if (!confirm("Are you sure you want to delete this stream key?")) {
            return;
        }

        try {
            await streamKeys.delete(id);
            keys = keys.filter((k) => k.id !== id);
        } catch (e) {
            console.error("Failed to delete stream key", e);
        }
    }

    function copyToClipboard(key: StreamKey) {
        const whipUrl = `${window.location.origin}/whip/${key.keyToken}`;
        navigator.clipboard.writeText(whipUrl);
        copiedId = key.id;
        setTimeout(() => (copiedId = null), 2000);
    }

    function formatDate(dateStr: string): string {
        return new Date(dateStr).toLocaleDateString(undefined, {
            month: "short",
            day: "numeric",
            year: "numeric",
        });
    }
</script>

<svelte:head>
    <title>Stream Keys | Chromatic</title>
</svelte:head>

<div class="keys-page">
    <header class="page-header">
        <div class="header-left">
            <a href="/admin" class="back-link">&larr; Dashboard</a>
            <h1>Stream Keys</h1>
        </div>
        <button class="btn btn-primary" onclick={() => (showCreateForm = true)}>
            New Stream Key
        </button>
    </header>

    {#if showCreateForm}
        <div class="card create-card">
            <h3>Create Stream Key</h3>
            <form onsubmit={handleCreate}>
                {#if error}
                    <div class="error-message">{error}</div>
                {/if}
                <div class="form-row">
                    <input
                        type="text"
                        class="input"
                        bind:value={newKeyName}
                        placeholder="Key name (e.g., Main Studio)"
                        required
                    />
                    <button
                        type="submit"
                        class="btn btn-primary"
                        disabled={isCreating}
                    >
                        {isCreating ? "Creating..." : "Create"}
                    </button>
                    <button
                        type="button"
                        class="btn btn-secondary"
                        onclick={() => {
                            showCreateForm = false;
                            newKeyName = "";
                            error = "";
                        }}
                    >
                        Cancel
                    </button>
                </div>
            </form>
        </div>
    {/if}

    {#if isLoading}
        <div class="loading">
            <div class="waiting-spinner"></div>
        </div>
    {:else if keys.length === 0}
        <div class="empty-state card">
            <h3>No Stream Keys</h3>
            <p>
                Create a stream key to connect OBS and start streaming. Each key
                generates a unique WHIP URL.
            </p>
            {#if !showCreateForm}
                <button
                    class="btn btn-primary"
                    onclick={() => (showCreateForm = true)}
                >
                    Create Your First Key
                </button>
            {/if}
        </div>
    {:else}
        <div class="keys-list">
            {#each keys as key (key.id)}
                <div class="key-card card">
                    <div class="key-header">
                        <div class="key-info">
                            <h3>{key.name}</h3>
                            <span class="key-date">Created {formatDate(key.createdAt)}</span>
                        </div>
                        <button
                            class="btn btn-ghost btn-sm btn-danger-text"
                            onclick={() => handleDelete(key.id)}
                        >
                            Delete
                        </button>
                    </div>

                    <div class="key-url">
                        <label>WHIP URL</label>
                        <div class="copy-row">
                            <code>{window.location.origin}/whip/{key.keyToken}</code>
                            <button
                                class="btn btn-secondary btn-sm"
                                onclick={() => copyToClipboard(key)}
                            >
                                {copiedId === key.id ? "Copied!" : "Copy"}
                            </button>
                        </div>
                    </div>

                    <div class="key-instructions">
                        <p>
                            <strong>OBS Setup:</strong> Settings &rarr; Stream &rarr;
                            Service: WHIP &rarr; Paste URL above
                        </p>
                    </div>
                </div>
            {/each}
        </div>
    {/if}
</div>

<style>
    .keys-page {
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

    .create-card {
        margin-bottom: var(--space-lg);
        padding: var(--space-lg);
    }

    .create-card h3 {
        margin: 0 0 var(--space-md);
    }

    .form-row {
        display: flex;
        gap: var(--space-sm);
    }

    .form-row .input {
        flex: 1;
    }

    .keys-list {
        display: flex;
        flex-direction: column;
        gap: var(--space-md);
    }

    .key-card {
        padding: var(--space-lg);
    }

    .key-header {
        display: flex;
        justify-content: space-between;
        align-items: flex-start;
        margin-bottom: var(--space-md);
    }

    .key-info h3 {
        margin: 0 0 var(--space-xs);
        font-size: 1rem;
    }

    .key-date {
        font-size: 0.75rem;
        color: var(--color-text-subtle);
    }

    .key-url {
        margin-bottom: var(--space-md);
    }

    .key-url label {
        display: block;
        font-size: 0.75rem;
        color: var(--color-text-muted);
        margin-bottom: var(--space-xs);
    }

    .copy-row {
        display: flex;
        gap: var(--space-sm);
        align-items: center;
    }

    .copy-row code {
        flex: 1;
        padding: var(--space-sm);
        background: var(--color-surface-elevated);
        border-radius: var(--radius-md);
        font-size: 0.75rem;
        overflow-x: auto;
        white-space: nowrap;
    }

    .key-instructions {
        padding: var(--space-sm);
        background: var(--color-surface-elevated);
        border-radius: var(--radius-md);
        font-size: 0.75rem;
        color: var(--color-text-muted);
    }

    .key-instructions p {
        margin: 0;
    }

    .empty-state {
        text-align: center;
        padding: var(--space-2xl);
    }

    .empty-state h3 {
        margin: 0 0 var(--space-sm);
    }

    .empty-state p {
        color: var(--color-text-muted);
        margin-bottom: var(--space-lg);
        max-width: 400px;
        margin-left: auto;
        margin-right: auto;
    }

    .error-message {
        padding: var(--space-sm);
        background: rgba(239, 68, 68, 0.1);
        border: 1px solid var(--color-error);
        border-radius: var(--radius-md);
        color: var(--color-error);
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
</style>
