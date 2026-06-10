<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { fade, fly } from "svelte/transition";
    import { page } from "$app/stores";
    import { rooms } from "$lib/api/client";
    import StateCard from "$lib/components/StateCard.svelte";

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

    // After a minute of waiting, acknowledge the wait with a second line.
    const LONG_WAIT_MS = 60000;
    let waitedLong = $state(false);
    let longWaitTimer: ReturnType<typeof setTimeout>;

    // Smooth exit: fade the page out before navigating into the session.
    const EXIT_FADE_MS = 400;
    let isLeaving = $state(false);
    let exitTimer: ReturnType<typeof setTimeout>;

    const prefersReducedMotion =
        typeof window !== "undefined" &&
        window.matchMedia("(prefers-reduced-motion: reduce)").matches;

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

        longWaitTimer = setTimeout(() => {
            waitedLong = true;
        }, LONG_WAIT_MS);

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
        if (longWaitTimer) {
            clearTimeout(longWaitTimer);
        }
        if (exitTimer) {
            clearTimeout(exitTimer);
        }
    });

    // Presentational exit choreography: show the "You're in" card for a
    // beat, fade the page out, then navigate — the session page fades in on
    // top of the same dark background, so the handoff reads as one motion.
    function enterSession() {
        if (isLeaving) return;
        status = "admitted";
        eventSource?.close();
        if (prefersReducedMotion) {
            window.location.href = `/room/${slug}/session`;
            return;
        }
        exitTimer = setTimeout(() => {
            isLeaving = true;
            exitTimer = setTimeout(() => {
                window.location.href = `/room/${slug}/session`;
            }, EXIT_FADE_MS);
        }, 600);
    }

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
                    // Redirect to session (with exit choreography)
                    enterSession();
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
                // Redirect to session (with exit choreography)
                enterSession();
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

<main class="waiting-page" class:leaving={isLeaving}>
    <div class="waiting-content" aria-live="polite">
        {#if status === "waiting"}
            <div class="waiting-card" in:fly={{ y: 8, duration: prefersReducedMotion ? 0 : 200 }}>
                <div class="wordmark">Chromatic</div>
                <div class="waiting-pulse" aria-hidden="true">
                    <span class="pulse-core"></span>
                    <span class="pulse-ring"></span>
                </div>
                <h1>{roomName || "Your session"}</h1>
                <p class="waiting-line">The host will let you in shortly.</p>
                <div class="waiting-secondary-slot">
                    {#key waitedLong}
                        <p
                            class="waiting-secondary"
                            transition:fade={{ duration: prefersReducedMotion ? 0 : 200 }}
                        >
                            {waitedLong
                                ? "Still waiting — the host has been notified."
                                : "Keep this page open."}
                        </p>
                    {/key}
                </div>
                <button class="btn btn-secondary waiting-leave" onclick={handleLeave}>
                    Leave waiting room
                </button>
            </div>
        {:else if status === "admitted"}
            <div in:fade={{ duration: prefersReducedMotion ? 0 : 200 }}>
                <StateCard
                    icon="check"
                    tone="success"
                    title="You're in"
                    body="Taking you to the session…"
                />
            </div>
        {:else if status === "ended"}
            <div in:fade={{ duration: prefersReducedMotion ? 0 : 200 }}>
                <StateCard
                    icon="ended"
                    title="This session has ended"
                    body="The review wrapped up before you could join. Check with your host if you were expecting to be let in."
                >
                    <button class="btn btn-primary" onclick={handleLeave}>
                        Back to room page
                    </button>
                </StateCard>
            </div>
        {:else if status === "connection_failed"}
            <div in:fade={{ duration: prefersReducedMotion ? 0 : 200 }}>
                <StateCard icon="error" tone="error" title="Connection lost" body={error}>
                    <button class="btn btn-primary" onclick={handleRetry}>
                        Try again
                    </button>
                    <button class="btn btn-secondary" onclick={handleLeave}>
                        Leave
                    </button>
                </StateCard>
            </div>
        {:else if status === "error"}
            <div in:fade={{ duration: prefersReducedMotion ? 0 : 200 }}>
                <StateCard icon="error" tone="error" title="Unable to join" body={error}>
                    <button class="btn btn-primary" onclick={handleLeave}>
                        Back to room page
                    </button>
                </StateCard>
            </div>
        {/if}
    </div>
</main>

<style>
    .waiting-page {
        min-height: 100vh;
        min-height: 100dvh;
        display: flex;
        align-items: center;
        justify-content: center;
        padding: var(--space-lg);
        padding-top: calc(var(--space-lg) + env(safe-area-inset-top, 0px));
        padding-bottom: calc(var(--space-lg) + env(safe-area-inset-bottom, 0px));
        background:
            radial-gradient(
                ellipse 70% 45% at 50% 0%,
                rgba(72, 182, 166, 0.05),
                transparent 70%
            ),
            var(--color-bg);
        opacity: 1;
        transition: opacity 400ms ease;
    }

    /* Exit choreography into the session */
    .waiting-page.leaving {
        opacity: 0;
        pointer-events: none;
    }

    .waiting-content {
        max-width: 400px;
        width: 100%;
    }

    .waiting-card {
        display: flex;
        flex-direction: column;
        align-items: center;
        background: var(--color-surface);
        border-radius: var(--radius-lg);
        padding: var(--space-xl);
        text-align: center;
        border: 1px solid var(--color-border);
        box-shadow: var(--shadow-md);
    }

    .waiting-card .wordmark {
        margin-bottom: var(--space-xl);
    }

    .waiting-card h1 {
        font-size: clamp(1.375rem, 5vw, 1.625rem);
        letter-spacing: -0.015em;
        text-wrap: balance;
        margin-bottom: var(--space-sm);
        color: var(--color-text);
    }

    .waiting-line {
        color: var(--color-text-muted);
        font-size: var(--text-body);
        margin: 0;
    }

    /* Grid-stacked slot so the two secondary lines crossfade in place
       without the card height jumping. */
    .waiting-secondary-slot {
        display: grid;
        place-items: center;
        min-height: 1.5rem;
        margin-top: var(--space-xs);
        margin-bottom: var(--space-lg);
        width: 100%;
    }

    .waiting-secondary-slot > * {
        grid-area: 1 / 1;
    }

    .waiting-secondary {
        margin: 0;
        font-size: var(--text-meta);
        color: var(--color-text-subtle);
    }

    .waiting-leave {
        min-height: 40px;
    }

    /* Soft pulse: a calm core with a slow expanding halo */
    .waiting-pulse {
        position: relative;
        width: 14px;
        height: 14px;
        margin-bottom: var(--space-xl);
    }

    .pulse-core {
        position: absolute;
        inset: 0;
        border-radius: 50%;
        background: var(--color-primary);
        animation: waiting-pulse 2.4s ease-in-out infinite;
    }

    .pulse-ring {
        position: absolute;
        inset: 0;
        border-radius: 50%;
        border: 1px solid var(--color-primary);
        opacity: 0;
        animation: waiting-ring 2.4s ease-out infinite;
    }

    @keyframes waiting-pulse {
        0%, 100% { opacity: 0.45; transform: scale(0.85); }
        50% { opacity: 1; transform: scale(1); }
    }

    @keyframes waiting-ring {
        0% { opacity: 0.5; transform: scale(1); }
        70%, 100% { opacity: 0; transform: scale(2.6); }
    }

    @media (max-width: 480px) {
        .waiting-page {
            padding-left: var(--space-md);
            padding-right: var(--space-md);
        }

        .waiting-card {
            padding: var(--space-xl) var(--space-lg);
        }
    }
</style>
