<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { page } from "$app/stores";
    import { rooms } from "$lib/api/client";

    const slug = $page.params.slug!;

    // Reconnection configuration
    const RECONNECT_MAX_ATTEMPTS = 10;
    const RECONNECT_BASE_DELAY = 1000; // 1 second
    const RECONNECT_MAX_DELAY = 30000; // 30 seconds

    let status = $state<"waiting" | "admitted" | "ended" | "error" | "connection_failed">("waiting");
    let error = $state("");
    let roomName = $state("");
    let eventSource: EventSource | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout>;
    let reconnectAttempts = $state(0);

    // Get session data from storage
    let sessionData: {
        participantId: string;
        token: string;
        color: string;
    } | null = null;

    onMount(async () => {
        // Get session data
        const stored = sessionStorage.getItem(`chromatic_session_${slug}`);
        if (!stored) {
            window.location.href = `/room/${slug}`;
            return;
        }

        sessionData = JSON.parse(stored);

        // Fetch the room name so the wait screen says what you're joining
        rooms
            .info(slug)
            .then((info) => {
                roomName = info.name;
            })
            .catch(() => {
                // Non-fatal: fall back to generic copy
            });

        // Connect to SSE endpoint for push notifications
        connectSSE();
    });

    onDestroy(() => {
        if (eventSource) {
            eventSource.close();
        }
        if (reconnectTimer) {
            clearTimeout(reconnectTimer);
        }
    });

    // Calculate delay with exponential backoff and jitter
    function getReconnectDelay(): number {
        const exponentialDelay = Math.min(
            RECONNECT_BASE_DELAY * Math.pow(2, reconnectAttempts),
            RECONNECT_MAX_DELAY
        );
        // Add jitter (±25% randomization to prevent thundering herd)
        const jitter = exponentialDelay * 0.25 * (Math.random() * 2 - 1);
        return Math.floor(exponentialDelay + jitter);
    }

    function connectSSE() {
        if (!sessionData) return;

        // Check if we've exceeded max retry attempts
        if (reconnectAttempts >= RECONNECT_MAX_ATTEMPTS) {
            status = "connection_failed";
            error = "Unable to connect to the waiting room. Please check your connection and try again.";
            return;
        }

        // Close existing connection if any
        if (eventSource) {
            eventSource.close();
        }

        const url = `/api/rooms/${slug}/waiting/events/${sessionData.participantId}?token=${encodeURIComponent(sessionData.token)}`;
        eventSource = new EventSource(url);

        eventSource.onopen = () => {
            // Reset retry count on successful connection
            reconnectAttempts = 0;
        };

        eventSource.onmessage = (event) => {
            try {
                const data = JSON.parse(event.data);

                if (data.event === "admitted") {
                    status = "admitted";
                    eventSource?.close();
                    // Redirect to session
                    window.location.href = `/room/${slug}/session`;
                } else if (data.event === "ended") {
                    status = "ended";
                    eventSource?.close();
                }
            } catch (e) {
                console.error("Failed to parse SSE message", e);
            }
        };

        eventSource.onerror = (e) => {
            console.error("SSE error", e);
            eventSource?.close();

            reconnectAttempts++;

            // Check if max retries exceeded
            if (reconnectAttempts >= RECONNECT_MAX_ATTEMPTS) {
                status = "connection_failed";
                error = "Unable to connect to the waiting room. Please check your connection and try again.";
                return;
            }

            // Reconnect with exponential backoff
            const delay = getReconnectDelay();
            console.log(`SSE reconnecting in ${delay}ms (attempt ${reconnectAttempts}/${RECONNECT_MAX_ATTEMPTS})`);

            reconnectTimer = setTimeout(() => {
                // Before reconnecting, check status via API
                checkStatusAndReconnect();
            }, delay);
        };
    }

    async function checkStatusAndReconnect() {
        if (!sessionData) return;

        try {
            const result = await rooms.checkStatus(slug, sessionData.participantId, sessionData.token);

            if (result.roomStatus === "ended") {
                status = "ended";
                return;
            }

            if (result.isAdmitted) {
                status = "admitted";
                // Redirect to session
                window.location.href = `/room/${slug}/session`;
                return;
            }

            // Still waiting, reconnect SSE
            connectSSE();
        } catch (e: any) {
            // If participant not found, may have been removed
            if (e.message.includes("not found")) {
                error = "You have been removed from the waiting room.";
                status = "error";
            } else {
                // Other error, increment attempts and try to reconnect with backoff
                reconnectAttempts++;

                if (reconnectAttempts >= RECONNECT_MAX_ATTEMPTS) {
                    status = "connection_failed";
                    error = "Unable to connect to the waiting room. Please check your connection and try again.";
                    return;
                }

                const delay = getReconnectDelay();
                reconnectTimer = setTimeout(() => {
                    connectSSE();
                }, delay);
            }
        }
    }

    function handleRetry() {
        // Reset retry state and attempt to connect again
        reconnectAttempts = 0;
        status = "waiting";
        error = "";
        connectSSE();
    }

    function handleLeave() {
        sessionStorage.removeItem(`chromatic_session_${slug}`);
        window.location.href = `/room/${slug}`;
    }
</script>

<svelte:head>
    <title>Waiting Room | Chromatic</title>
</svelte:head>

<main class="waiting-page">
    <div class="waiting-content" aria-live="polite">
        {#if status === "waiting"}
            <div class="waiting-card">
                <div class="waiting-pulse" aria-hidden="true"></div>
                <h1>Waiting to join {roomName || "the session"}</h1>
                <p>The host will let you in soon.</p>
                <p class="muted">Please keep this page open.</p>
                <button class="btn btn-secondary" onclick={handleLeave}>
                    Leave Waiting Room
                </button>
            </div>
        {:else if status === "admitted"}
            <div class="waiting-card">
                <div class="success-icon">&#10003;</div>
                <h1>You've been admitted!</h1>
                <p>Redirecting to the session...</p>
            </div>
        {:else if status === "ended"}
            <div class="waiting-card">
                <h1>Session Ended</h1>
                <p>This session has ended before you could join.</p>
                <button class="btn btn-primary" onclick={handleLeave}>
                    Return Home
                </button>
            </div>
        {:else if status === "connection_failed"}
            <div class="waiting-card error">
                <div class="error-icon">!</div>
                <h1>Connection Lost</h1>
                <p>{error}</p>
                <div class="button-group">
                    <button class="btn btn-primary" onclick={handleRetry}>
                        Try Again
                    </button>
                    <button class="btn btn-secondary" onclick={handleLeave}>
                        Leave
                    </button>
                </div>
            </div>
        {:else if status === "error"}
            <div class="waiting-card error">
                <h1>Unable to Join</h1>
                <p>{error}</p>
                <button class="btn btn-primary" onclick={handleLeave}>
                    Return to Room
                </button>
            </div>
        {/if}
    </div>
</main>

<style>
    .waiting-page {
        min-height: 100vh;
        display: flex;
        align-items: center;
        justify-content: center;
        padding: var(--space-lg);
        background: var(--color-bg);
    }

    .waiting-content {
        max-width: 400px;
        width: 100%;
    }

    .waiting-card {
        background: var(--color-surface);
        border-radius: var(--radius-lg);
        padding: var(--space-xl);
        text-align: center;
        border: 1px solid var(--color-border);
    }

    .waiting-card h1 {
        font-size: 1.5rem;
        margin-bottom: var(--space-md);
        color: var(--color-text);
    }

    .waiting-card p {
        color: var(--color-text-muted);
        margin-bottom: var(--space-md);
    }

    .waiting-card p.muted {
        font-size: 0.875rem;
        margin-bottom: var(--space-lg);
    }

    /* Soft pulse instead of a generic spinner */
    .waiting-pulse {
        width: 16px;
        height: 16px;
        margin: 0 auto var(--space-lg);
        border-radius: 50%;
        background: var(--color-primary);
        animation: waiting-pulse 2s ease-in-out infinite;
    }

    @keyframes waiting-pulse {
        0%, 100% { opacity: 0.35; transform: scale(0.8); }
        50% { opacity: 1; transform: scale(1); }
    }

    .success-icon {
        width: 60px;
        height: 60px;
        margin: 0 auto var(--space-lg);
        background: var(--color-success);
        border-radius: 50%;
        display: flex;
        align-items: center;
        justify-content: center;
        font-size: 2rem;
        color: white;
    }

    .waiting-card.error h1 {
        color: var(--color-error);
    }

    .error-icon {
        width: 60px;
        height: 60px;
        margin: 0 auto var(--space-lg);
        background: var(--color-error);
        border-radius: 50%;
        display: flex;
        align-items: center;
        justify-content: center;
        font-size: 2rem;
        font-weight: bold;
        color: white;
    }

    .button-group {
        display: flex;
        gap: var(--space-sm);
        justify-content: center;
        margin-top: var(--space-lg);
    }
</style>
