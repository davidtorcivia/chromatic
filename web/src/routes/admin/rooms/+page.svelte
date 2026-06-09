<script lang="ts">
    import { onMount } from "svelte";
    import { rooms, type Room } from "$lib/api/client";
    import StatusBadge from "$lib/components/StatusBadge.svelte";

    let allRooms = $state<Room[]>([]);
    let isLoading = $state(true);
    let statusFilter = $state<"all" | "pending" | "live" | "ended">("all");

    let filteredRooms = $derived(
        statusFilter === "all"
            ? allRooms
            : allRooms.filter((r) => r.status === statusFilter)
    );

    onMount(async () => {
        try {
            allRooms = (await rooms.list()) ?? [];
        } catch (e) {
            console.error("Failed to load rooms", e);
        } finally {
            isLoading = false;
        }
    });

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
    </div>

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
        <div class="rooms-grid">
            {#each filteredRooms as room (room.id)}
                <a href="/admin/rooms/{room.slug}" class="room-card card">
                    <div class="room-header">
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
</div>

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

    .room-header {
        display: flex;
        justify-content: space-between;
        align-items: flex-start;
        gap: var(--space-md);
        margin-bottom: var(--space-sm);
    }

    .room-header h3 {
        margin: 0;
        font-size: 1rem;
        font-weight: 600;
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
