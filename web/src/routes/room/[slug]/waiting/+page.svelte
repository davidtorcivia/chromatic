<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { fade, fly } from "svelte/transition";
    import { quintOut } from "svelte/easing";
    import { goto } from "$app/navigation";
    import { page } from "$app/stores";
    import { rooms, type LobbyInfo } from "$lib/api/client";
    import { countdownParts, formatScheduleLabel, serverClockOffset } from "$lib/lobby";
    import StateCard from "$lib/components/StateCard.svelte";
    import DeviceSetup from "$lib/components/DeviceSetup.svelte";
    import { parseStoredSession, type StoredSessionData } from "$lib/session/storedSession";
    import { getStorageItem, removeStorageItem } from "$lib/storage/safeStorage";

    const slug = $page.params.slug!;

    // Reconnection configuration
    const RECONNECT_MAX_ATTEMPTS = 10;
    const RECONNECT_BASE_DELAY = 1000; // 1 second
    const RECONNECT_MAX_DELAY = 30000; // 30 seconds

    let status = $state<"lobby" | "waiting" | "admitted" | "ended" | "error" | "connection_failed">("waiting");
    let error = $state("");
    let roomName = $state("");
    let eventSource: EventSource | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let reconnectAttempts = $state(0);
    let destroyed = false;
    let sseGeneration = 0;

    // After a minute of waiting, acknowledge the wait with a second line.
    const LONG_WAIT_MS = 60000;
    let waitedLong = $state(false);
    let longWaitTimer: ReturnType<typeof setTimeout> | null = null;

    // Smooth exit: fade the page out before navigating into the session.
    const EXIT_FADE_MS = 400;
    let isLeaving = $state(false);
    let exitTimer: ReturnType<typeof setTimeout> | null = null;

    // Countdown lobby (scheduled room joined before it opened)
    let lobby = $state<LobbyInfo | null>(null);
    let now = $state(Date.now());
    // Offset to the server clock — countdowns must not trust the local clock.
    let serverOffset = 0;
    let countdownTimer: ReturnType<typeof setInterval> | undefined;

    const prefersReducedMotion =
        typeof window !== "undefined" &&
        window.matchMedia("(prefers-reduced-motion: reduce)").matches;

    // Get session data from storage
    let sessionData: StoredSessionData | null = null;

    onMount(async () => {
        // Get session data
        const stored = getStorageItem("session", `chromatic_session_${slug}`);
        if (!stored) {
            void goto(`/room/${slug}`);
            return;
        }

        const parsedSession = parseStoredSession(stored);
        if (!parsedSession) {
            removeStorageItem("session", `chromatic_session_${slug}`);
            void goto(`/room/${slug}`);
            return;
        }
        sessionData = parsedSession;

        // Countdown lobby: anchor to the server clock captured at join time,
        // then refine with the fresher timestamp from /info below.
        serverOffset = serverClockOffset(sessionData?.serverTime, Date.now());
        if (sessionData?.lobby) {
            lobby = sessionData.lobby;
            if (Date.parse(lobby.scheduledAt) > Date.now() + serverOffset) {
                status = "lobby";
            } else if (lobby.waitingRoomEnabled) {
                status = "waiting";
            } else {
                status = "lobby"; // T-0 already passed; shows "starting" state
            }
            countdownTimer = setInterval(() => {
                now = Date.now();
            }, 250);
        }

        // Fetch the room name so the wait screen says what you're joining
        rooms
            .info(slug)
            .then((info) => {
                if (destroyed) return;
                roomName = info.name;
                if (info.serverTime) {
                    serverOffset = serverClockOffset(info.serverTime, Date.now());
                }
            })
            .catch(() => {
                // Non-fatal: fall back to generic copy
            });

        longWaitTimer = setTimeout(() => {
            if (!destroyed) {
                waitedLong = true;
            }
        }, LONG_WAIT_MS);

        // Connect to SSE endpoint for push notifications
        connectSSE();
    });

    onDestroy(() => {
        destroyed = true;
        closeSSE();
        clearReconnectTimer();
        if (longWaitTimer) {
            clearTimeout(longWaitTimer);
            longWaitTimer = null;
        }
        if (exitTimer) {
            clearTimeout(exitTimer);
            exitTimer = null;
        }
        if (countdownTimer) {
            clearInterval(countdownTimer);
            countdownTimer = undefined;
        }
    });

    // Server-anchored countdown state
    let serverNow = $derived(now + serverOffset);
    let scheduledDate = $derived(lobby ? new Date(lobby.scheduledAt) : null);
    let countdown = $derived(
        scheduledDate ? countdownParts(scheduledDate.getTime(), serverNow) : null
    );
    let scheduleLabel = $derived(
        scheduledDate ? formatScheduleLabel(scheduledDate, new Date(serverNow)) : ""
    );
    let countdownDone = $derived(countdown !== null && countdown.totalMs <= 0);

    // T-0 transition: waiting-room-enabled lobbies crossfade into the
    // approval-waiting state; auto-admit lobbies hold a "starting" state
    // until the server's SSE "admitted" event lands (enterSession).
    $effect(() => {
        if (status === "lobby" && countdownDone && lobby?.waitingRoomEnabled) {
            status = "waiting";
        }
    });

    // Presentational exit choreography: show the "You're in" card for a
    // beat, fade the page out, then navigate — the session page fades in on
    // top of the same dark background, so the handoff reads as one motion.
    function enterSession() {
        if (destroyed || isLeaving) return;
        status = "admitted";
        closeSSE();
        clearReconnectTimer();
        if (prefersReducedMotion) {
            void goto(`/room/${slug}/session`);
            return;
        }
        exitTimer = setTimeout(() => {
            if (destroyed) return;
            isLeaving = true;
            exitTimer = setTimeout(() => {
                if (!destroyed) void goto(`/room/${slug}/session`);
            }, EXIT_FADE_MS);
        }, 600);
    }

    function clearReconnectTimer() {
        if (!reconnectTimer) return;
        clearTimeout(reconnectTimer);
        reconnectTimer = null;
    }

    function closeSSE() {
        sseGeneration++;
        if (!eventSource) return;
        eventSource.onopen = null;
        eventSource.onmessage = null;
        eventSource.onerror = null;
        eventSource.close();
        eventSource = null;
    }

    function isTerminalStatus() {
        return status === "admitted" || status === "ended" || status === "error";
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
        if (destroyed || !sessionData || isLeaving || isTerminalStatus()) return;
        clearReconnectTimer();

        // Check if we've exceeded max retry attempts
        if (reconnectAttempts >= RECONNECT_MAX_ATTEMPTS) {
            status = "connection_failed";
            error = "Unable to connect to the waiting room. Please check your connection and try again.";
            return;
        }

        // Close existing connection if any
        closeSSE();

        const url = `/api/rooms/${slug}/waiting/events/${sessionData.participantId}?token=${encodeURIComponent(sessionData.token)}`;
        const generation = ++sseGeneration;
        const source = new EventSource(url);
        eventSource = source;

        const isCurrentConnection = () =>
            !destroyed && generation === sseGeneration && eventSource === source;

        source.onopen = () => {
            if (!isCurrentConnection()) return;
            // Reset retry count on successful connection
            reconnectAttempts = 0;
        };

        source.onmessage = (event) => {
            if (!isCurrentConnection()) return;
            try {
                const data = JSON.parse(event.data);

                if (data.event === "admitted") {
                    // Redirect to session (with exit choreography)
                    enterSession();
                } else if (data.event === "ended") {
                    status = "ended";
                    closeSSE();
                } else if (data.event === "denied") {
                    status = "error";
                    error = "The host declined your request to join this session.";
                    closeSSE();
                } else if (data.event === "open") {
                    // The room opened (waiting-room enabled): countdown
                    // crossfades into the approval-waiting state. The server
                    // closes the SSE stream after any event, so reconnect.
                    if (status === "lobby") {
                        status = "waiting";
                    }
                    connectSSE();
                }
            } catch (e) {
                console.error("Failed to parse SSE message", e);
            }
        };

        source.onerror = (e) => {
            if (!isCurrentConnection()) return;
            console.error("SSE error", e);
            source.close();
            if (eventSource === source) eventSource = null;

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

            const reconnectGeneration = generation;
            reconnectTimer = setTimeout(() => {
                reconnectTimer = null;
                if (destroyed || reconnectGeneration !== sseGeneration || isTerminalStatus()) return;
                // Before reconnecting, check status via API
                checkStatusAndReconnect();
            }, delay);
        };
    }

    async function checkStatusAndReconnect() {
        if (destroyed || !sessionData || isLeaving || isTerminalStatus()) return;
        const generation = sseGeneration;

        try {
            const result = await rooms.checkStatus(slug, sessionData.participantId, sessionData.token);
            if (destroyed || generation !== sseGeneration || isTerminalStatus()) return;

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
        } catch (e: unknown) {
            if (destroyed || generation !== sseGeneration || isTerminalStatus()) return;
            // If participant not found, may have been removed
            const message = e instanceof Error ? e.message : String(e);
            if (message.includes("not found")) {
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
                const reconnectGeneration = generation;
                reconnectTimer = setTimeout(() => {
                    reconnectTimer = null;
                    if (destroyed || reconnectGeneration !== sseGeneration || isTerminalStatus()) return;
                    connectSSE();
                }, delay);
            }
        }
    }

    function handleRetry() {
        // Reset retry state and attempt to connect again
        closeSSE();
        clearReconnectTimer();
        reconnectAttempts = 0;
        status = "waiting";
        error = "";
        connectSSE();
    }

    function handleLeave() {
        closeSSE();
        clearReconnectTimer();
        removeStorageItem("session", `chromatic_session_${slug}`);
        void goto(`/room/${slug}`);
    }
</script>

<svelte:head>
    <title>Waiting Room | Chromatic</title>
</svelte:head>

<main class="waiting-page stage" class:leaving={isLeaving}>
    <div class="waiting-content" aria-live="polite">
        {#if status === "lobby" && countdown}
            <div class="lobby-card stage-panel" in:fade={{ duration: prefersReducedMotion ? 0 : 200 }}>
                <div class="wordmark">Chromatic</div>
                <h1 class="lobby-room">{roomName || "Your session"}</h1>
                <p class="lobby-schedule">{scheduleLabel}</p>

                {#if !countdownDone}
                    <p class="lobby-lead">You're early. The session begins in</p>
                    <div class="lobby-countdown" role="timer" aria-label="Time until the session begins">
                        {#if countdown.days > 0}
                            <div class="count-unit">
                                <span class="count-value-slot">
                                    {#key countdown.days}
                                        <span class="count-value" in:fly={{ y: -10, duration: prefersReducedMotion ? 0 : 260, easing: quintOut }}>{countdown.days}</span>
                                    {/key}
                                </span>
                                <span class="count-label">{countdown.days === 1 ? "day" : "days"}</span>
                            </div>
                        {/if}
                        <div class="count-unit">
                            <span class="count-value-slot">
                                {#key countdown.hours}
                                    <span class="count-value" in:fly={{ y: -10, duration: prefersReducedMotion ? 0 : 260, easing: quintOut }}>{String(countdown.hours).padStart(2, "0")}</span>
                                {/key}
                            </span>
                            <span class="count-label">hours</span>
                        </div>
                        <div class="count-unit">
                            <span class="count-value-slot">
                                {#key countdown.minutes}
                                    <span class="count-value" in:fly={{ y: -10, duration: prefersReducedMotion ? 0 : 260, easing: quintOut }}>{String(countdown.minutes).padStart(2, "0")}</span>
                                {/key}
                            </span>
                            <span class="count-label">min</span>
                        </div>
                        <div class="count-unit">
                            <span class="count-value-slot">
                                {#key countdown.seconds}
                                    <span class="count-value" in:fly={{ y: -10, duration: prefersReducedMotion ? 0 : 260, easing: quintOut }}>{String(countdown.seconds).padStart(2, "0")}</span>
                                {/key}
                            </span>
                            <span class="count-label">sec</span>
                        </div>
                    </div>
                    <p class="lobby-hint">Keep this page open and you'll be taken in automatically.</p>
                {:else}
                    <div class="waiting-pulse lobby-pulse" aria-hidden="true">
                        <span class="pulse-core"></span>
                        <span class="pulse-ring"></span>
                    </div>
                    <p class="lobby-lead">Starting now, opening the room…</p>
                    <p class="lobby-hint">You'll be taken in automatically.</p>
                {/if}

                <DeviceSetup />
                <button class="btn btn-secondary waiting-leave" onclick={handleLeave}>
                    Leave
                </button>
            </div>
        {:else if status === "waiting"}
            <div class="waiting-card stage-panel" in:fly={{ y: 8, duration: prefersReducedMotion ? 0 : 200 }}>
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
                                ? "Still waiting. The host has been notified."
                                : "Keep this page open."}
                        </p>
                    {/key}
                </div>
                <DeviceSetup />
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
        padding: var(--space-xl);
        text-align: center;
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

    /* Quiet ghost capsule: leaving should never compete with waiting */
    .waiting-leave {
        min-height: 40px;
        border-radius: var(--radius-full);
        background: rgba(255, 255, 255, 0.06);
        border: 1px solid rgba(255, 255, 255, 0.1);
        color: var(--color-text-muted);
        transition:
            background var(--transition-fast),
            color var(--transition-fast),
            transform 0.2s var(--ease-spring);
    }

    .waiting-leave:hover {
        background: rgba(255, 255, 255, 0.1);
        color: var(--color-text);
    }

    .waiting-leave:active {
        transform: scale(0.97);
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

    /* Countdown lobby: centered composition with a large live countdown */
    .lobby-card {
        display: flex;
        flex-direction: column;
        align-items: center;
        padding: var(--space-2xl, 3rem) var(--space-xl);
        text-align: center;
    }

    .lobby-card .wordmark {
        margin-bottom: var(--space-xl);
    }

    .lobby-room {
        font-family: var(--font-display);
        font-size: clamp(1.5rem, 6vw, 1.875rem);
        letter-spacing: -0.015em;
        text-wrap: balance;
        margin: 0 0 var(--space-xs);
        color: var(--color-text);
    }

    .lobby-schedule {
        margin: 0 0 var(--space-xl);
        font-size: var(--text-body);
        color: var(--color-primary);
        font-weight: 500;
    }

    .lobby-lead {
        margin: 0 0 var(--space-md);
        font-size: var(--text-body);
        color: var(--color-text-muted);
    }

    .lobby-countdown {
        display: flex;
        align-items: flex-start;
        justify-content: center;
        gap: var(--space-sm);
        margin-bottom: var(--space-lg);
    }

    /* Glass clock tiles; digits tick in from above on change */
    .count-unit {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: 6px;
    }

    .count-value-slot {
        display: grid;
        place-items: center;
        overflow: hidden;
        min-width: 4rem;
        padding: 10px 8px;
        background: rgba(255, 255, 255, 0.05);
        border: 1px solid rgba(255, 255, 255, 0.06);
        border-radius: 14px;
        box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.06);
    }

    .count-value-slot > * {
        grid-area: 1 / 1;
    }

    .count-value {
        font-family: var(--font-display);
        font-size: clamp(2rem, 9vw, 2.75rem);
        font-weight: 600;
        line-height: 1.1;
        letter-spacing: 0.01em;
        font-variant-numeric: tabular-nums;
        color: var(--color-text);
    }

    .count-label {
        font-size: var(--text-min);
        text-transform: uppercase;
        letter-spacing: 0.08em;
        color: var(--color-text-subtle);
    }

    .lobby-hint {
        margin: 0 0 var(--space-lg);
        font-size: var(--text-meta);
        color: var(--color-text-subtle);
    }

    .lobby-pulse {
        margin-bottom: var(--space-lg);
    }

    @media (max-width: 480px) {
        .waiting-page {
            padding-left: var(--space-md);
            padding-right: var(--space-md);
        }

        .waiting-card {
            padding: var(--space-xl) var(--space-lg);
        }

        .lobby-card {
            padding: var(--space-xl) var(--space-lg);
        }
    }
</style>
