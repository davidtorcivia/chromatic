<script lang="ts">
    import { onMount } from "svelte";
    import { fade } from "svelte/transition";
    import { rooms, type Room } from "$lib/api/client";
    import ConfirmDialog from "$lib/components/ConfirmDialog.svelte";
    import StatusBadge from "$lib/components/StatusBadge.svelte";

    let allRooms = $state<Room[]>([]);
    let isLoading = $state(true);
    let statusFilter = $state<"all" | "pending" | "live" | "ended">("all");

    // Multi-select state
    let selected = $state<string[]>([]);
    let bulkBusy = $state(false);
    let bulkError = $state("");
    let bulkSuccess = $state("");
    let confirmBulkDeleteOpen = $state(false);

    let filteredRooms = $derived(
        statusFilter === "all"
            ? allRooms
            : allRooms.filter((r) => r.status === statusFilter)
    );

    let allSelected = $derived(
        filteredRooms.length > 0 &&
            filteredRooms.every((r) => selected.includes(r.slug))
    );

    onMount(loadRooms);

    async function loadRooms() {
        try {
            allRooms = (await rooms.list()) ?? [];
        } catch (e) {
            console.error("Failed to load rooms", e);
        } finally {
            isLoading = false;
        }
    }

    function isSelected(slug: string): boolean {
        return selected.includes(slug);
    }

    function toggleSelect(slug: string) {
        selected = isSelected(slug)
            ? selected.filter((s) => s !== slug)
            : [...selected, slug];
    }

    function toggleSelectAll() {
        selected = allSelected ? [] : filteredRooms.map((r) => r.slug);
    }

    function clearSelection() {
        selected = [];
        bulkError = "";
        bulkSuccess = "";
    }

    async function handleBulkDelete() {
        confirmBulkDeleteOpen = false;
        bulkBusy = true;
        bulkError = "";
        bulkSuccess = "";

        const count = selected.length;
        const { failed } = await rooms.deleteMany(selected);
        await loadRooms();
        // Keep failures selected so the user can retry
        selected = failed;

        if (failed.length > 0) {
            bulkError = `Failed to delete ${failed.length} of ${count} room${count === 1 ? "" : "s"}: ${failed.join(", ")}`;
        } else {
            bulkSuccess = `Deleted ${count} room${count === 1 ? "" : "s"}`;
            setTimeout(() => (bulkSuccess = ""), 4000);
        }
        bulkBusy = false;
    }

    async function handleBulkEnd() {
        bulkBusy = true;
        bulkError = "";
        bulkSuccess = "";

        // Only live rooms have a session to end
        const liveSlugs = allRooms
            .filter((r) => selected.includes(r.slug) && r.status === "live")
            .map((r) => r.slug);

        if (liveSlugs.length === 0) {
            bulkError = "No live rooms selected.";
            bulkBusy = false;
            return;
        }

        const { failed } = await rooms.endMany(liveSlugs);
        await loadRooms();

        if (failed.length > 0) {
            bulkError = `Failed to end ${failed.length} of ${liveSlugs.length} session${liveSlugs.length === 1 ? "" : "s"}: ${failed.join(", ")}`;
        } else {
            bulkSuccess = `Ended ${liveSlugs.length} session${liveSlugs.length === 1 ? "" : "s"}`;
            setTimeout(() => (bulkSuccess = ""), 4000);
        }
        bulkBusy = false;
    }

    function formatDate(dateStr: string): string {
        return new Date(dateStr).toLocaleDateString(undefined, {
            month: "short",
            day: "numeric",
            year: "numeric",
            hour: "2-digit",
            minute: "2-digit",
        });
    }
</script>

<svelte:head>
    <title>Rooms | Chromatic</title>
</svelte:head>

<div class="rooms-page">
    <header class="page-header">
        <div class="header-left">
            <a href="/admin" class="back-link">&larr; Dashboard</a>
            <h1>Rooms</h1>
        </div>
        <a href="/admin/rooms/new" class="btn btn-primary">Create Room</a>
    </header>

    <div class="filters">
        <button
            class="filter-btn"
            class:active={statusFilter === "all"}
            onclick={() => (statusFilter = "all")}
        >
            All ({allRooms.length})
        </button>
        <button
            class="filter-btn"
            class:active={statusFilter === "live"}
            onclick={() => (statusFilter = "live")}
        >
            Live ({allRooms.filter((r) => r.status === "live").length})
        </button>
        <button
            class="filter-btn"
            class:active={statusFilter === "pending"}
            onclick={() => (statusFilter = "pending")}
        >
            Pending ({allRooms.filter((r) => r.status === "pending").length})
        </button>
        <button
            class="filter-btn"
            class:active={statusFilter === "ended"}
            onclick={() => (statusFilter = "ended")}
        >
            Ended ({allRooms.filter((r) => r.status === "ended").length})
        </button>

        {#if filteredRooms.length > 0}
            <label class="select-all">
                <input
                    type="checkbox"
                    checked={allSelected}
                    onchange={toggleSelectAll}
                    disabled={bulkBusy}
                />
                <span>Select all</span>
            </label>
        {/if}
    </div>

    {#if bulkError}
        <div class="alert alert-error" transition:fade={{ duration: 150 }}>
            {bulkError}
        </div>
    {/if}

    {#if bulkSuccess}
        <div class="alert alert-success" transition:fade={{ duration: 150 }}>
            {bulkSuccess}
        </div>
    {/if}

    {#if isLoading}
        <div class="rooms-grid" aria-busy="true" aria-label="Loading rooms">
            {#each [0, 1, 2] as i (i)}
                <div class="skeleton skeleton-card"></div>
            {/each}
        </div>
    {:else if filteredRooms.length === 0}
        <div class="empty-state card">
            <p>
                {#if statusFilter === "all"}
                    No rooms yet. Create your first room to get started.
                {:else}
                    No {statusFilter} rooms.
                {/if}
            </p>
            {#if statusFilter === "all"}
                <a href="/admin/rooms/new" class="btn btn-primary">Create Room</a>
            {/if}
        </div>
    {:else}
        <div class="rooms-grid" class:selecting={selected.length > 0}>
            {#each filteredRooms as room (room.id)}
                <a
                    href="/admin/rooms/{room.slug}"
                    class="room-card card"
                    class:selected={isSelected(room.slug)}
                >
                    <div class="room-header">
                        <input
                            type="checkbox"
                            class="room-select"
                            aria-label="Select {room.name}"
                            checked={isSelected(room.slug)}
                            disabled={bulkBusy}
                            onclick={(e) => {
                                e.preventDefault();
                                e.stopPropagation();
                                toggleSelect(room.slug);
                            }}
                        />
                        <h3>{room.name}</h3>
                        <StatusBadge status={room.status} />
                    </div>
                    <div class="room-meta">
                        <span class="room-slug">/room/{room.slug}</span>
                        <span class="room-date">{formatDate(room.createdAt)}</span>
                    </div>
                    <div class="room-features">
                        {#if room.hasPassword}
                            <span class="feature-badge">Password</span>
                        {/if}
                        {#if room.waitingRoomEnabled}
                            <span class="feature-badge">Waiting Room</span>
                        {/if}
                        {#if room.watermarkMode !== "none"}
                            <span class="feature-badge">Watermark</span>
                        {/if}
                    </div>
                </a>
            {/each}
        </div>
    {/if}

    {#if selected.length > 0}
        <div class="bulk-bar" transition:fade={{ duration: 150 }}>
            <span class="bulk-count">
                {#if bulkBusy}
                    <span class="btn-spinner" aria-hidden="true"></span>
                    Working...
                {:else}
                    {selected.length} selected
                {/if}
            </span>
            <div class="bulk-actions">
                <button
                    class="btn btn-danger btn-sm"
                    disabled={bulkBusy}
                    onclick={() => (confirmBulkDeleteOpen = true)}
                >
                    Delete
                </button>
                <button
                    class="btn btn-secondary btn-sm"
                    disabled={bulkBusy}
                    onclick={handleBulkEnd}
                >
                    End Session
                </button>
                <button
                    class="btn btn-ghost btn-sm"
                    disabled={bulkBusy}
                    onclick={clearSelection}
                >
                    Clear
                </button>
            </div>
        </div>
    {/if}
</div>

<ConfirmDialog
    open={confirmBulkDeleteOpen}
    title="Delete {selected.length} room{selected.length === 1 ? '' : 's'}?"
    body="This removes their chat history and files. This cannot be undone."
    confirmLabel="Delete {selected.length === 1 ? 'Room' : `${selected.length} Rooms`}"
    danger
    onConfirm={handleBulkDelete}
    onCancel={() => (confirmBulkDeleteOpen = false)}
/>

<style>
    .rooms-page {
        max-width: 1000px;
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

    .filters {
        display: flex;
        gap: var(--space-sm);
        margin-bottom: var(--space-lg);
        flex-wrap: wrap;
    }

    .filter-btn {
        padding: var(--space-sm) var(--space-md);
        background: var(--color-surface);
        border: 1px solid var(--color-border);
        border-radius: var(--radius-md);
        color: var(--color-text-muted);
        font-size: 0.875rem;
        cursor: pointer;
        transition: all var(--transition-fast);
    }

    .filter-btn:hover {
        background: var(--color-surface-hover);
    }

    .filter-btn.active {
        background: var(--color-primary);
        border-color: var(--color-primary);
        color: white;
    }

    .select-all {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        margin-left: auto;
        font-size: 0.875rem;
        color: var(--color-text-muted);
        cursor: pointer;
        user-select: none;
    }

    .select-all input {
        cursor: pointer;
    }

    .rooms-grid {
        display: grid;
        grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
        gap: var(--space-md);
    }

    .room-card {
        display: block;
        padding: var(--space-lg);
        color: inherit;
        transition: transform var(--transition-fast), box-shadow var(--transition-fast);
    }

    .room-card:hover {
        transform: translateY(-2px);
        box-shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
    }

    .room-card.selected {
        border-color: var(--color-primary);
    }

    .room-header {
        display: flex;
        align-items: flex-start;
        gap: var(--space-sm);
        margin-bottom: var(--space-sm);
    }

    .room-header h3 {
        flex: 1;
        margin: 0;
        font-size: 1rem;
        font-weight: 600;
    }

    /* Selection checkbox: subtle until the card is hovered, the checkbox is
       focused, it's checked, or a selection is in progress */
    .room-select {
        margin-top: 3px;
        opacity: 0;
        cursor: pointer;
        transition: opacity var(--transition-fast);
    }

    .room-card:hover .room-select,
    .room-select:focus-visible,
    .room-select:checked,
    .rooms-grid.selecting .room-select {
        opacity: 1;
    }

    .bulk-bar {
        position: sticky;
        bottom: var(--space-md);
        z-index: 10;
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: var(--space-md);
        margin-top: var(--space-lg);
        padding: var(--space-sm) var(--space-md);
        background: var(--color-surface-elevated);
        border: 1px solid var(--color-border);
        border-radius: var(--radius-md);
        box-shadow: 0 4px 16px rgba(0, 0, 0, 0.35);
    }

    .bulk-count {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        font-size: 0.875rem;
        color: var(--color-text-muted);
    }

    .bulk-actions {
        display: flex;
        gap: var(--space-sm);
        flex-wrap: wrap;
    }

    .room-meta {
        display: flex;
        flex-direction: column;
        gap: var(--space-xs);
        margin-bottom: var(--space-md);
    }

    .room-slug {
        font-size: 0.75rem;
        color: var(--color-text-subtle);
        font-family: monospace;
    }

    .room-date {
        font-size: 0.75rem;
        color: var(--color-text-subtle);
    }

    .room-features {
        display: flex;
        gap: var(--space-xs);
        flex-wrap: wrap;
    }

    .feature-badge {
        font-size: var(--text-min);
        padding: 2px var(--space-xs);
        background: var(--color-surface-elevated);
        border-radius: var(--radius-sm);
        color: var(--color-text-muted);
    }

    .empty-state {
        text-align: center;
        padding: var(--space-2xl);
    }

    .empty-state p {
        color: var(--color-text-muted);
        margin-bottom: var(--space-lg);
    }

    .skeleton-card {
        min-height: 140px;
    }
</style>
