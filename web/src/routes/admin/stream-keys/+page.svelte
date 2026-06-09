<script lang="ts">
    import { onMount } from "svelte";
    import { fade, fly } from "svelte/transition";
    import { streamKeys, type StreamKey } from "$lib/api/client";
    import ConfirmDialog from "$lib/components/ConfirmDialog.svelte";
    import CopyField from "$lib/components/CopyField.svelte";

    let keys = $state<StreamKey[]>([]);
    let isLoading = $state(true);
    let isCreating = $state(false);
    let showCreateForm = $state(false);
    let newKeyName = $state("");
    let error = $state("");
    let revealedIds = $state<Set<string>>(new Set());
    let pendingDeleteId = $state<string | null>(null);

    onMount(async () => {
        await loadKeys();
    });

    async function loadKeys() {
        try {
            keys = (await streamKeys.list()) ?? [];
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

    async function handleDelete() {
        const id = pendingDeleteId;
        pendingDeleteId = null;
        if (!id) return;

        try {
            await streamKeys.delete(id);
            keys = keys.filter((k) => k.id !== id);
        } catch (e) {
            console.error("Failed to delete stream key", e);
        }
    }

    function toggleReveal(id: string) {
        const next = new Set(revealedIds);
        if (next.has(id)) {
            next.delete(id);
        } else {
            next.add(id);
        }
        revealedIds = next;
    }

    function whipUrl(key: StreamKey): string {
        return `${window.location.origin}/whip/${key.keyToken}`;
    }

    function maskedWhipUrl(key: StreamKey): string {
        const last4 = key.keyToken.slice(-4);
        return `${window.location.origin}/whip/…${last4}`;
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
        <div class="card create-card" transition:fly={{ y: 8, duration: 200 }}>
            <h3>Create Stream Key</h3>
            <form onsubmit={handleCreate}>
                {#if error}
                    <div class="alert alert-error" transition:fade={{ duration: 150 }}>{error}</div>
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
                        {#if isCreating}
                            <span class="btn-spinner" aria-hidden="true"></span>
                            Creating...
                        {:else}
                            Create
                        {/if}
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
        <div class="keys-list" aria-busy="true" aria-label="Loading stream keys">
            {#each [0, 1] as i (i)}
                <div class="skeleton skeleton-card"></div>
            {/each}
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
                            onclick={() => (pendingDeleteId = key.id)}
                        >
                            Delete
                        </button>
                    </div>

                    <div class="key-url">
                        <CopyField
                            label="WHIP URL"
                            value={whipUrl(key)}
                            display={revealedIds.has(key.id) ? "" : maskedWhipUrl(key)}
                        />
                        <button
                            class="btn btn-ghost btn-sm reveal-btn"
                            onclick={() => toggleReveal(key.id)}
                        >
                            {revealedIds.has(key.id) ? "Hide token" : "Reveal token"}
                        </button>
                    </div>

                    <div class="key-instructions">
                        <p>
                            <strong>OBS Setup:</strong> Settings &rarr; Stream &rarr;
                            Service: WHIP &rarr; paste the URL above into
                            <strong>Server</strong> and leave
                            <strong>Bearer Token empty</strong>.
                        </p>
                    </div>
                </div>
            {/each}
        </div>
    {/if}
</div>

<ConfirmDialog
    open={pendingDeleteId !== null}
    title="Delete this stream key?"
    body="Any rooms linked to this key will no longer receive a stream until you assign them a new key. This cannot be undone."
    confirmLabel="Delete Key"
    danger
    onConfirm={handleDelete}
    onCancel={() => (pendingDeleteId = null)}
/>

<style>
    .keys-page {
        max-width: 800px;
        margin: 0 auto;
    }

    .header-left {
        display: flex;
        flex-direction: column;
        gap: var(--space-xs);
    }

    .back-link {
        font-size: var(--text-body);
        color: var(--color-text-muted);
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

    .skeleton-card {
        min-height: 160px;
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
        font-size: var(--text-card-title);
    }

    .key-date {
        font-size: var(--text-meta);
        color: var(--color-text-subtle);
    }

    .key-url {
        margin-bottom: var(--space-md);
    }

    .reveal-btn {
        margin-top: var(--space-xs);
        color: var(--color-text-subtle);
    }

    .key-instructions {
        padding: var(--space-sm);
        background: var(--color-surface-elevated);
        border-radius: var(--radius-md);
        font-size: var(--text-meta);
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

    .btn-danger-text {
        color: var(--color-error);
    }

    .btn-danger-text:hover {
        background: var(--color-error-bg);
    }
</style>
