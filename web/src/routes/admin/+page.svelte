<script lang="ts">
    import { onDestroy, onMount } from "svelte";
    import { goto } from "$app/navigation";
    import {
        rooms,
        streamKeys,
        type Room,
        type StreamKey,
    } from "$lib/api/client";
    import StatusBadge from "$lib/components/StatusBadge.svelte";

    let allRooms = $state<Room[]>([]);
    let keys = $state<StreamKey[]>([]);
    let isLoading = $state(true);
    let showSetupBanner = $state(false);
    let destroyed = false;
    let loadRequestId = 0;

    let recentRooms = $derived(allRooms.slice(0, 5));
    let liveCount = $derived(
        allRooms.filter((r) => r.status === "live").length,
    );
    let scheduledCount = $derived(
        allRooms.filter((r) => r.status === "pending").length,
    );

    onDestroy(() => {
        destroyed = true;
    });

    onMount(async () => {
        const requestId = ++loadRequestId;
        try {
            const [roomsData, keysData] = await Promise.all([
                rooms.list(),
                streamKeys.list(),
            ]);
            if (destroyed || requestId !== loadRequestId) return;

            allRooms = roomsData ?? [];
            keys = keysData ?? [];
            if (typeof localStorage !== "undefined") {
                const setupComplete =
                    localStorage.getItem("chromatic-setup-complete") === "true";
                const setupDismissed =
                    localStorage.getItem("chromatic-setup-dismissed") === "true";
                const isFirstRun =
                    allRooms.length === 0 && keys.length === 0;
                showSetupBanner = !setupComplete && isFirstRun;
                if (!setupComplete && !setupDismissed && isFirstRun) {
                    goto("/admin/setup");
                }
            }
        } catch (e) {
            if (destroyed || requestId !== loadRequestId) return;
            console.error("Failed to load dashboard data", e);
        } finally {
            if (!destroyed && requestId === loadRequestId) {
                isLoading = false;
            }
        }
    });

    function dismissSetup() {
        if (typeof localStorage !== "undefined") {
            localStorage.setItem("chromatic-setup-dismissed", "true");
        }
        showSetupBanner = false;
    }

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

    {#if showSetupBanner}
        <section class="setup-banner card">
            <div class="setup-banner-content">
                <h2>First-run setup</h2>
                <p>
                    Finish the setup wizard to configure TURN, branding, stream
                    keys, and your first room.
                </p>
            </div>
            <div class="setup-banner-actions">
                <a href="/admin/setup" class="btn btn-primary"
                    >Launch Wizard</a
                >
                <button class="btn btn-ghost" onclick={dismissSetup}>
                    Dismiss
                </button>
            </div>
        </section>
    {/if}

    {#if isLoading}
        <div class="dashboard-grid" aria-busy="true" aria-label="Loading dashboard">
            {#each [0, 1, 2] as i (i)}
                <div class="skeleton skeleton-card"></div>
            {/each}
        </div>
    {:else}
        <div class="dashboard-grid">
            <!-- Quick Stats -->
            <div class="card stats-card">
                <h3>Quick Stats</h3>
                <div class="stats">
                    <div class="stat">
                        <span class="stat-value">{liveCount}</span>
                        <span class="stat-label">Live Now</span>
                    </div>
                    <div class="stat">
                        <span class="stat-value">{scheduledCount}</span>
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
                                    <StatusBadge status={room.status} />
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
                <a href="/admin/setup" class="btn btn-secondary btn-sm"
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

    .setup-banner {
        display: flex;
        justify-content: space-between;
        align-items: center;
        gap: var(--space-lg);
        margin-bottom: var(--space-xl);
        background: var(--color-surface-elevated);
        border-left: 2px solid var(--color-primary);
    }

    .setup-banner-content h2 {
        margin: 0 0 var(--space-xs);
        font-size: 1.125rem;
    }

    .setup-banner-content p {
        margin: 0;
        color: var(--color-text-muted);
        font-size: var(--text-body);
    }

    .setup-banner-actions {
        display: flex;
        gap: var(--space-sm);
        flex-wrap: wrap;
    }

    .dashboard-grid {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(300px, 1fr));
        gap: var(--space-lg);
    }

    .skeleton-card {
        min-height: 180px;
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
        font-family: var(--font-display);
        color: var(--color-primary);
    }

    .stat-label {
        font-size: var(--text-body);
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
        font-size: var(--text-meta);
        color: var(--color-text-subtle);
    }

    .key-item {
        display: flex;
        justify-content: space-between;
        align-items: center;
        padding: var(--space-sm) 0;
    }

    .key-token {
        font-size: var(--text-meta);
        background: var(--color-surface-elevated);
        padding: var(--space-xs) var(--space-sm);
        border-radius: var(--radius-sm);
    }

    .setup-steps {
        margin: var(--space-md) 0;
        padding-left: var(--space-lg);
        font-size: var(--text-body);
        color: var(--color-text-muted);
    }

    .setup-steps li {
        margin-bottom: var(--space-sm);
    }

    .empty-state {
        color: var(--color-text-muted);
        font-size: var(--text-body);
        padding: var(--space-lg) 0;
        text-align: center;
    }

    @media (max-width: 768px) {
        .setup-banner {
            flex-direction: column;
            align-items: flex-start;
        }
    }
</style>
