<script lang="ts">
    import { onMount } from "svelte";
    import {
        rooms,
        streamKeys,
        type Room,
        type StreamKey,
    } from "$lib/api/client";

    let recentRooms = $state<Room[]>([]);
    let keys = $state<StreamKey[]>([]);
    let isLoading = $state(true);

    onMount(async () => {
        try {
            const [roomsData, keysData] = await Promise.all([
                rooms.list(),
                streamKeys.list(),
            ]);
            recentRooms = roomsData.slice(0, 5);
            keys = keysData;
        } catch (e) {
            console.error("Failed to load dashboard data", e);
        } finally {
            isLoading = false;
        }
    });

    function formatDate(dateStr: string): string {
        return new Date(dateStr).toLocaleDateString(undefined, {
            month: "short",
            day: "numeric",
            hour: "2-digit",
            minute: "2-digit",
        });
    }
</script>

<svelte:head>
    <title>Dashboard | Chromatic</title>
</svelte:head>

<div class="dashboard">
    <header class="page-header">
        <h1>Dashboard</h1>
        <a href="/admin/rooms/new" class="btn btn-primary">Create Room</a>
    </header>

    {#if isLoading}
        <div class="loading">
            <div class="waiting-spinner"></div>
        </div>
    {:else}
        <div class="dashboard-grid">
            <!-- Quick Stats -->
            <div class="card stats-card">
                <h3>Quick Stats</h3>
                <div class="stats">
                    <div class="stat">
                        <span class="stat-value"
                            >{recentRooms.filter((r) => r.status === "live")
                                .length}</span
                        >
                        <span class="stat-label">Live Now</span>
                    </div>
                    <div class="stat">
                        <span class="stat-value"
                            >{recentRooms.filter((r) => r.status === "pending")
                                .length}</span
                        >
                        <span class="stat-label">Scheduled</span>
                    </div>
                    <div class="stat">
                        <span class="stat-value">{keys.length}</span>
                        <span class="stat-label">Stream Keys</span>
                    </div>
                </div>
            </div>

            <!-- Recent Rooms -->
            <div class="card rooms-card">
                <div class="card-header">
                    <h3>Recent Rooms</h3>
                    <a href="/admin/rooms" class="btn btn-ghost btn-sm"
                        >View All</a
                    >
                </div>

                {#if recentRooms.length === 0}
                    <p class="empty-state">
                        No rooms yet. Create your first room to get started.
                    </p>
                {:else}
                    <ul class="room-list">
                        {#each recentRooms as room (room.id)}
                            <li class="room-item">
                                <a href="/admin/rooms/{room.slug}">
                                    <div class="room-info">
                                        <span class="room-name"
                                            >{room.name}</span
                                        >
                                        <span class="room-date"
                                            >{formatDate(room.createdAt)}</span
                                        >
                                    </div>
                                    <span class="status-badge {room.status}"
                                        >{room.status}</span
                                    >
                                </a>
                            </li>
                        {/each}
                    </ul>
                {/if}
            </div>

            <!-- Stream Keys -->
            <div class="card keys-card">
                <div class="card-header">
                    <h3>Stream Keys</h3>
                    <a href="/admin/stream-keys" class="btn btn-ghost btn-sm"
                        >Manage</a
                    >
                </div>

                {#if keys.length === 0}
                    <p class="empty-state">
                        No stream keys configured. Add one to start streaming.
                    </p>
                {:else}
                    <ul class="key-list">
                        {#each keys as key (key.id)}
                            <li class="key-item">
                                <span class="key-name">{key.name}</span>
                                <code class="key-token"
                                    >{key.keyToken.slice(0, 8)}...</code
                                >
                            </li>
                        {/each}
                    </ul>
                {/if}
            </div>

            <!-- OBS Setup Help -->
            <div class="card help-card">
                <h3>OBS Setup</h3>
                <ol class="setup-steps">
                    <li>Open OBS Studio (30.0+)</li>
                    <li>Go to Settings → Stream</li>
                    <li>Set Service to <strong>WHIP</strong></li>
                    <li>Enter your stream URL</li>
                    <li>
                        Set <strong>B-Frames to 0</strong> in encoder settings
                    </li>
                </ol>
                <a href="/docs/obs-setup" class="btn btn-secondary btn-sm"
                    >Full Guide</a
                >
            </div>
        </div>
    {/if}
</div>

<style>
    .dashboard {
        max-width: 1200px;
        margin: 0 auto;
    }

    .page-header {
        display: flex;
        justify-content: space-between;
        align-items: center;
        margin-bottom: var(--space-xl);
    }

    .page-header h1 {
        margin: 0;
    }

    .dashboard-grid {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(300px, 1fr));
        gap: var(--space-lg);
    }

    .card-header {
        display: flex;
        justify-content: space-between;
        align-items: center;
        margin-bottom: var(--space-md);
    }

    .card-header h3 {
        margin: 0;
    }

    .stats-card .stats {
        display: flex;
        gap: var(--space-xl);
        margin-top: var(--space-md);
    }

    .stat {
        text-align: center;
    }

    .stat-value {
        display: block;
        font-size: 2rem;
        font-weight: 700;
        color: var(--color-primary);
    }

    .stat-label {
        font-size: 0.875rem;
        color: var(--color-text-muted);
    }

    .room-list,
    .key-list {
        list-style: none;
    }

    .room-item a {
        display: flex;
        justify-content: space-between;
        align-items: center;
        padding: var(--space-sm) 0;
        border-bottom: 1px solid var(--color-border-subtle);
        color: inherit;
    }

    .room-item:last-child a {
        border-bottom: none;
    }

    .room-name {
        font-weight: 500;
    }

    .room-date {
        font-size: 0.75rem;
        color: var(--color-text-subtle);
    }

    .status-badge {
        font-size: 0.75rem;
        padding: var(--space-xs) var(--space-sm);
        border-radius: var(--radius-full);
    }

    .status-badge.live {
        background: rgba(34, 197, 94, 0.2);
        color: var(--color-success);
    }

    .status-badge.pending {
        background: rgba(245, 158, 11, 0.2);
        color: var(--color-warning);
    }

    .status-badge.ended {
        background: rgba(107, 114, 128, 0.2);
        color: var(--color-text-muted);
    }

    .key-item {
        display: flex;
        justify-content: space-between;
        align-items: center;
        padding: var(--space-sm) 0;
    }

    .key-token {
        font-size: 0.75rem;
        background: var(--color-surface-elevated);
        padding: var(--space-xs) var(--space-sm);
        border-radius: var(--radius-sm);
    }

    .setup-steps {
        margin: var(--space-md) 0;
        padding-left: var(--space-lg);
        font-size: 0.875rem;
        color: var(--color-text-muted);
    }

    .setup-steps li {
        margin-bottom: var(--space-sm);
    }

    .empty-state {
        color: var(--color-text-muted);
        font-size: 0.875rem;
        padding: var(--space-lg) 0;
        text-align: center;
    }

    .loading {
        display: flex;
        justify-content: center;
        padding: var(--space-2xl);
    }

    .btn-sm {
        padding: var(--space-xs) var(--space-sm);
        font-size: 0.75rem;
    }
</style>
