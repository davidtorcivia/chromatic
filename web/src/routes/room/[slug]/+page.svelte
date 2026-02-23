<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { goto } from "$app/navigation";
    import { page } from "$app/stores";
    import { rooms, type RoomInfo } from "$lib/api/client";

    let roomInfo = $state<RoomInfo | null>(null);
    let error = $state("");
    let name = $state("");
    let password = $state("");
    let isLoading = $state(false);
    let now = $state(Date.now());
    let timer: ReturnType<typeof setInterval>;

    const EARLY_ACCESS_WINDOW_MS = 10 * 60 * 1000;

    const slug = $page.params.slug!;

    onMount(async () => {
        try {
            roomInfo = await rooms.info(slug);
        } catch (e) {
            error = "Room not found";
        }

        timer = setInterval(() => {
            now = Date.now();
        }, 30000);
    });

    onDestroy(() => {
        if (timer) {
            clearInterval(timer);
        }
    });

    let scheduledAt = $derived(roomInfo?.scheduledAt ? new Date(roomInfo.scheduledAt) : null);
    let earlyAccessAt = $derived(
        scheduledAt ? new Date(scheduledAt.getTime() - EARLY_ACCESS_WINDOW_MS) : null
    );
    let canJoin = $derived(!earlyAccessAt || now >= earlyAccessAt.getTime());

    async function handleJoin(e: SubmitEvent) {
        e.preventDefault();
        if (!name.trim()) return;
        if (!canJoin) {
            error = "This session is not open yet.";
            return;
        }

        isLoading = true;
        error = "";

        try {
            const result = await rooms.join(
                slug,
                name.trim(),
                password || undefined,
            );

            // Store sanitized participant name for future sessions
            localStorage.setItem('chromatic_name', result.name || name.trim());

            // Store session info
            sessionStorage.setItem(
                `chromatic_session_${slug}`,
                JSON.stringify(result),
            );

            // Navigate based on waiting room status
            if (result.waitingRoom) {
                goto(`/room/${slug}/waiting`);
            } else {
                goto(`/room/${slug}/session`);
            }
        } catch (e: any) {
            error = e.message || "Failed to join room";
        } finally {
            isLoading = false;
        }
    }
</script>

<svelte:head>
    <title>{roomInfo?.name || "Join Room"} | Chromatic</title>
</svelte:head>

<main class="join-page">
    {#if error && !roomInfo}
        <div class="card error-card">
            <h2>Room Not Found</h2>
            <p>The room you're looking for doesn't exist or has ended.</p>
        </div>
    {:else if roomInfo}
        <div class="join-content">
            <div class="room-info">
                <h1>{roomInfo.name}</h1>
                {#if roomInfo.status === "live"}
                    <span class="status-badge live">● Live</span>
                {:else if roomInfo.status === "pending"}
                    <span class="status-badge pending">Waiting for host</span>
                {:else}
                    <span class="status-badge ended">Session ended</span>
                {/if}
            </div>

            {#if roomInfo.status === "ended"}
                <div class="card">
                    <p>This session has ended.</p>
                </div>
            {:else if !canJoin}
                <div class="card">
                    <p>
                        This session opens at
                        {earlyAccessAt?.toLocaleString(undefined, {
                            month: "short",
                            day: "numeric",
                            hour: "2-digit",
                            minute: "2-digit"
                        })}
                        .
                    </p>
                </div>
            {:else}
                <form class="card join-form" onsubmit={handleJoin}>
                    {#if error}
                        <div class="error-message">{error}</div>
                    {/if}

                    <div class="form-group">
                        <label for="name">Your Name</label>
                        <input
                            type="text"
                            id="name"
                            class="input"
                            bind:value={name}
                            placeholder="Enter your name"
                            maxlength="50"
                            required
                        />
                    </div>

                    {#if roomInfo.hasPassword}
                        <div class="form-group">
                            <label for="password">Password</label>
                            <input
                                type="password"
                                id="password"
                                class="input"
                                bind:value={password}
                                placeholder="Enter room password"
                            />
                        </div>
                    {/if}

                    <button
                        type="submit"
                        class="btn btn-primary btn-large"
                        disabled={isLoading}
                    >
                        {isLoading ? "Joining..." : "Join Session"}
                    </button>

                    {#if roomInfo.waitingRoomEnabled}
                        <p class="waiting-note">
                            You'll be placed in a waiting room until the host
                            admits you.
                        </p>
                    {/if}
                </form>
            {/if}
        </div>
    {:else}
        <div class="loading">
            <div class="waiting-spinner"></div>
            <p>Loading...</p>
        </div>
    {/if}
</main>

<style>
    .join-page {
        min-height: 100vh;
        display: flex;
        align-items: center;
        justify-content: center;
        padding: var(--space-lg);
    }

    .join-content {
        max-width: 400px;
        width: 100%;
    }

    .room-info {
        text-align: center;
        margin-bottom: var(--space-lg);
    }

    .room-info h1 {
        margin-bottom: var(--space-sm);
    }

    .status-badge {
        display: inline-block;
        padding: var(--space-xs) var(--space-md);
        border-radius: var(--radius-full);
        font-size: 0.75rem;
        font-weight: 500;
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
        background: rgba(239, 68, 68, 0.2);
        color: var(--color-error);
    }

    .join-form {
        display: flex;
        flex-direction: column;
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

    .btn-large {
        padding: var(--space-md) var(--space-lg);
        font-size: 1rem;
    }

    .waiting-note {
        margin-top: var(--space-lg);
        font-size: 0.875rem;
        color: var(--color-text-muted);
        text-align: center;
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

    .error-card {
        text-align: center;
    }

    .error-card h2 {
        margin-bottom: var(--space-md);
    }

    .loading {
        text-align: center;
        color: var(--color-text-muted);
    }

    .loading p {
        margin-top: var(--space-md);
    }
</style>
