<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { goto } from "$app/navigation";
    import { page } from "$app/stores";
    import { rooms, type RoomInfo } from "$lib/api/client";
    import StatusBadge from "$lib/components/StatusBadge.svelte";

    let roomInfo = $state<RoomInfo | null>(null);
    let error = $state("");
    let name = $state("");
    let password = $state("");
    let adminToken = $state("");
    let showHostSection = $state(false);
    let isLoading = $state(false);
    let now = $state(Date.now());
    let timer: ReturnType<typeof setInterval>;

    const EARLY_ACCESS_WINDOW_MS = 10 * 60 * 1000;

    const slug = $page.params.slug!;

    onMount(async () => {
        // Pre-fill the name from a previous session
        const storedName = localStorage.getItem("chromatic_name");
        if (storedName) {
            name = storedName;
        }

        // Reveal the host token field when arriving via a host link
        if ($page.url.searchParams.get("host") === "1") {
            showHostSection = true;
        }

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
    let scheduledLabel = $derived(
        scheduledAt?.toLocaleString(undefined, {
            month: "short",
            day: "numeric",
            hour: "2-digit",
            minute: "2-digit",
        }) ?? ""
    );

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
                adminToken.trim() || undefined,
            );

            // Store sanitized participant name for future sessions
            localStorage.setItem('chromatic_name', result.name || name.trim());

            // Store session info (includes the server-assigned role, so the
            // session page knows whether this participant is an admin)
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
            <div class="invite-header">
                <div class="wordmark">Chromatic</div>
                <p class="invite-line">You've been invited to a live color review session.</p>
            </div>

            <div class="room-info">
                <h1>{roomInfo.name}</h1>
                <StatusBadge status={roomInfo.status} />
            </div>

            {#if roomInfo.status === "ended"}
                <div class="card">
                    <p>This session has ended.</p>
                </div>
            {:else if !canJoin}
                <div class="card">
                    <p>
                        This session is scheduled for {scheduledLabel}.
                        The room opens 10 minutes before.
                    </p>
                </div>
            {:else}
                <form class="card join-form" onsubmit={handleJoin}>
                    {#if error}
                        <div class="alert alert-error">{error}</div>
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

                    {#if showHostSection}
                        <div class="form-group">
                            <label for="adminToken">Admin Token</label>
                            <input
                                type="password"
                                id="adminToken"
                                class="input"
                                bind:value={adminToken}
                                placeholder="Enter admin token"
                            />
                        </div>
                    {/if}

                    <button
                        type="submit"
                        class="btn btn-primary btn-large"
                        disabled={isLoading}
                    >
                        {#if isLoading}
                            <span class="btn-spinner" aria-hidden="true"></span>
                            Joining...
                        {:else}
                            Join Session
                        {/if}
                    </button>

                    <p class="form-hint mic-hint">
                        Chromatic uses your microphone for voice chat — your
                        browser will ask for permission.
                    </p>

                    {#if roomInfo.waitingRoomEnabled}
                        <p class="waiting-note">
                            You'll be placed in a waiting room until the host
                            admits you.
                        </p>
                    {/if}
                </form>

                {#if !showHostSection}
                    <p class="host-link-row">
                        <button
                            type="button"
                            class="host-link"
                            onclick={() => (showHostSection = true)}
                        >
                            Joining as host?
                        </button>
                    </p>
                {/if}
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

    .invite-header {
        text-align: center;
        margin-bottom: var(--space-xl);
    }

    .wordmark {
        font-size: 0.875rem;
        font-weight: 700;
        text-transform: uppercase;
        letter-spacing: 0.18em;
        color: var(--color-text-muted);
        margin-bottom: var(--space-xs);
    }

    .invite-line {
        font-size: var(--text-body);
        color: var(--color-text-muted);
        margin: 0;
    }

    .room-info {
        text-align: center;
        margin-bottom: var(--space-lg);
    }

    .room-info h1 {
        margin-bottom: var(--space-sm);
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

    .mic-hint {
        margin-top: var(--space-md);
        text-align: center;
    }

    .waiting-note {
        margin-top: var(--space-lg);
        font-size: 0.875rem;
        color: var(--color-text-muted);
        text-align: center;
    }

    .host-link-row {
        text-align: center;
        margin-top: var(--space-lg);
    }

    .host-link {
        background: none;
        border: none;
        padding: 0;
        font-size: var(--text-meta);
        color: var(--color-text-subtle);
        text-decoration: underline;
        cursor: pointer;
    }

    .host-link:hover {
        color: var(--color-text);
    }

    .alert {
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
