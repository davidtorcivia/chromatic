<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { fade, fly } from "svelte/transition";
    import { goto } from "$app/navigation";
    import { page } from "$app/stores";
    import { rooms, type RoomInfo, type JoinResult } from "$lib/api/client";
    import {
        countdownParts,
        formatOpensIn,
        formatScheduleLabel,
        serverClockOffset,
    } from "$lib/lobby";
    import StatusBadge from "$lib/components/StatusBadge.svelte";
    import StateCard from "$lib/components/StateCard.svelte";

    let roomInfo = $state<RoomInfo | null>(null);
    let error = $state("");
    let name = $state("");
    let password = $state("");
    let adminToken = $state("");
    let showHostSection = $state(false);
    let isLoading = $state(false);
    let now = $state(Date.now());
    // Offset to the server clock (computed from /info's serverTime) so the
    // "opens in" countdown doesn't trust the local clock.
    let serverOffset = $state(0);
    let timer: ReturnType<typeof setInterval> | undefined;
    // ?host=1 auto-join: 'joining' shows the branded hold state instead of
    // flashing the form; 'failed' falls back to the form with the token field.
    let hostJoinState = $state<"none" | "joining" | "failed">("none");

    const slug = $page.params.slug!;

    const prefersReducedMotion =
        typeof window !== "undefined" &&
        window.matchMedia("(prefers-reduced-motion: reduce)").matches;

    // Staged entrance: each block rises in ~80ms after the previous one.
    const enter = (step: number) => ({
        y: 8,
        duration: prefersReducedMotion ? 0 : 200,
        delay: prefersReducedMotion ? 0 : step * 80,
    });

    function startTicker(intervalMs: number) {
        if (timer) clearInterval(timer);
        timer = setInterval(() => {
            now = Date.now();
        }, intervalMs);
    }

    onMount(async () => {
        // Pre-fill the name from a previous session
        const storedName = localStorage.getItem("chromatic_name");
        if (storedName) {
            name = storedName;
        }

        const isHostLink = $page.url.searchParams.get("host") === "1";
        if (isHostLink) {
            // Hold the form back while the automatic admin join runs.
            hostJoinState = "joining";
        }

        try {
            roomInfo = await rooms.info(slug);
            serverOffset = serverClockOffset(roomInfo.serverTime, Date.now());
        } catch (e) {
            error = "Room not found";
            hostJoinState = "none";
        }

        if (isHostLink && roomInfo && roomInfo.status !== "ended") {
            await attemptHostJoin();
        } else if (hostJoinState === "joining") {
            hostJoinState = roomInfo ? "failed" : "none";
            showHostSection = true;
        }

        startTicker(1000);
    });

    onDestroy(() => {
        if (timer) {
            clearInterval(timer);
        }
    });

    // Automatic admin join for dashboard "Join as host" links: the admin
    // session cookie travels with the request, so no password or token is
    // needed. If the server doesn't grant the admin role (no/expired
    // session), fall back to the normal form with the token field revealed.
    async function attemptHostJoin() {
        const hostName = localStorage.getItem("chromatic_name")?.trim() || "Host";
        try {
            const result = await rooms.join(slug, hostName);
            if (result.role === "admin") {
                finishJoin(result, hostName);
                return;
            }
        } catch {
            // 401 / network — fall through to the manual form
        }
        hostJoinState = "failed";
        showHostSection = true;
    }

    function finishJoin(result: JoinResult, joinedName: string) {
        // Store sanitized participant name for future sessions
        localStorage.setItem("chromatic_name", result.name || joinedName);

        // Store session info (includes the server-assigned role and any
        // countdown-lobby payload for the waiting page)
        sessionStorage.setItem(`chromatic_session_${slug}`, JSON.stringify(result));

        // Navigate based on waiting room / lobby status
        if (result.waitingRoom) {
            goto(`/room/${slug}/waiting`);
        } else {
            goto(`/room/${slug}/session`);
        }
    }

    let scheduledAt = $derived(roomInfo?.scheduledAt ? new Date(roomInfo.scheduledAt) : null);
    let earlyOpenMs = $derived((roomInfo?.earlyOpenMinutes ?? 10) * 60 * 1000);
    let lobbyOpensAt = $derived(
        scheduledAt ? new Date(scheduledAt.getTime() - earlyOpenMs) : null
    );
    let serverNow = $derived(now + serverOffset);
    // The lobby window opens earlyOpenMinutes before start — or immediately
    // if the host opened the room early / the stream is already live.
    let canJoin = $derived(
        !lobbyOpensAt ||
            serverNow >= lobbyOpensAt.getTime() ||
            Boolean(roomInfo?.openedAt) ||
            roomInfo?.status === "live"
    );
    let scheduledLabel = $derived(
        scheduledAt ? formatScheduleLabel(scheduledAt, new Date(serverNow)) : ""
    );
    let opensInLabel = $derived(
        lobbyOpensAt && !canJoin
            ? formatOpensIn(countdownParts(lobbyOpensAt.getTime(), serverNow))
            : ""
    );
    let hasSessionDetails = $derived(
        Boolean(roomInfo && (scheduledAt || roomInfo.hasPassword || roomInfo.waitingRoomEnabled))
    );
    // Hosts bypass scheduling server-side, so a filled-in host token always
    // unlocks the Join button — even before the lobby window opens.
    let canSubmit = $derived(canJoin || adminToken.trim().length > 0);

    async function handleJoin(e: SubmitEvent) {
        e.preventDefault();
        if (!name.trim()) return;
        if (!canSubmit) {
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

            finishJoin(result, name.trim());
        } catch (e: any) {
            error = e.message || "We couldn't join the session. Please try again.";
        } finally {
            isLoading = false;
        }
    }
</script>

<svelte:head>
    <title>{roomInfo?.name || "Join Room"} | Chromatic</title>
</svelte:head>

<main class="join-page">
    {#if hostJoinState === "joining"}
        <!-- Branded hold state while the automatic admin join runs (no form flash) -->
        <div class="join-content host-joining" in:fade={{ duration: prefersReducedMotion ? 0 : 150 }}>
            <div class="invite-header">
                <div class="wordmark">Chromatic</div>
            </div>
            <div class="card host-joining-card">
                <div class="host-spinner" aria-hidden="true"></div>
                <h1>Joining as host…</h1>
                <p>Setting up your session with host controls.</p>
            </div>
        </div>
    {:else if error && !roomInfo}
        <div class="join-content" in:fly={enter(0)}>
            <div class="invite-header">
                <div class="wordmark">Chromatic</div>
            </div>
            <StateCard
                icon="ended"
                title="Room not found"
                body="This link is invalid or the session has ended. Check the link with your host."
            />
        </div>
    {:else if roomInfo}
        <div class="join-content">
            <div class="invite-header" in:fly={enter(0)}>
                <div class="wordmark">Chromatic</div>
                <p class="invite-line">You've been invited to a live color review.</p>
            </div>

            <div class="room-hero" in:fly={enter(1)}>
                <h1>{roomInfo.name}</h1>
                <StatusBadge status={roomInfo.status} />
            </div>

            {#if hasSessionDetails}
                <div class="session-details" in:fly={enter(2)}>
                    {#if scheduledAt}
                        <span class="session-detail">
                            <svg viewBox="0 0 24 24" fill="currentColor" width="13" height="13" aria-hidden="true"><path d="M11.99 2C6.47 2 2 6.48 2 12s4.47 10 9.99 10C17.52 22 22 17.52 22 12S17.52 2 11.99 2zM12 20c-4.42 0-8-3.58-8-8s3.58-8 8-8 8 3.58 8 8-3.58 8-8 8zm.5-13H11v6l5.25 3.15.75-1.23-4.5-2.67z"/></svg>
                            {scheduledLabel}
                        </span>
                    {/if}
                    {#if roomInfo.hasPassword}
                        <span class="session-detail">
                            <svg viewBox="0 0 24 24" fill="currentColor" width="13" height="13" aria-hidden="true"><path d="M18 8h-1V6c0-2.76-2.24-5-5-5S7 3.24 7 6v2H6c-1.1 0-2 .9-2 2v10c0 1.1.9 2 2 2h12c1.1 0 2-.9 2-2V10c0-1.1-.9-2-2-2zm-6 9c-1.1 0-2-.9-2-2s.9-2 2-2 2 .9 2 2-.9 2-2 2zM9 8V6c0-1.66 1.34-3 3-3s3 1.34 3 3v2H9z"/></svg>
                            Private session
                        </span>
                    {/if}
                    {#if roomInfo.waitingRoomEnabled}
                        <span class="session-detail">
                            <svg viewBox="0 0 24 24" fill="currentColor" width="13" height="13" aria-hidden="true"><path d="M12 1L3 5v6c0 5.55 3.84 10.74 9 12 5.16-1.26 9-6.45 9-12V5l-9-4zm0 10.99h7c-.53 4.12-3.28 7.79-7 8.94V12H5V6.3l7-3.11v8.8z"/></svg>
                            Host admits each guest
                        </span>
                    {/if}
                </div>
            {/if}

            {#if roomInfo.status === "ended"}
                <div in:fly={enter(2)}>
                    <StateCard
                        icon="ended"
                        title="This session has ended"
                        body="The review is over. If you were expecting to join, check with your host for a new link."
                    />
                </div>
            {:else}
                {#if !canJoin}
                    <div class="opens-soon" in:fly={enter(3)} aria-live="polite">
                        <svg viewBox="0 0 24 24" fill="currentColor" width="16" height="16" aria-hidden="true"><path d="M11.99 2C6.47 2 2 6.48 2 12s4.47 10 9.99 10C17.52 22 22 17.52 22 12S17.52 2 11.99 2zM12 20c-4.42 0-8-3.58-8-8s3.58-8 8-8 8 3.58 8 8-3.58 8-8 8zm.5-13H11v6l5.25 3.15.75-1.23-4.5-2.67z"/></svg>
                        <div class="opens-soon-copy">
                            <span class="opens-soon-title">Scheduled for {scheduledLabel}</span>
                            <span class="opens-soon-line">The room opens in <strong class="opens-soon-count">{opensInLabel}</strong> — the Join button will unlock automatically.</span>
                        </div>
                    </div>
                {/if}
                <form class="card join-form" in:fly={enter(3)} onsubmit={handleJoin}>
                    {#if error}
                        <div class="alert alert-error" role="alert" in:fade={{ duration: prefersReducedMotion ? 0 : 150 }}>
                            {error}
                        </div>
                    {/if}

                    <div class="form-group">
                        <label for="name">Your name</label>
                        <input
                            type="text"
                            id="name"
                            class="input input-lg"
                            bind:value={name}
                            placeholder="How should we introduce you?"
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
                                class="input input-lg"
                                bind:value={password}
                                placeholder="From your invitation"
                            />
                        </div>
                    {/if}

                    {#if showHostSection}
                        <div class="form-group" in:fly={{ y: 8, duration: prefersReducedMotion ? 0 : 200 }}>
                            <label for="adminToken">Host token</label>
                            <input
                                type="password"
                                id="adminToken"
                                class="input input-lg"
                                bind:value={adminToken}
                                placeholder="Paste your host token"
                            />
                        </div>
                    {/if}

                    <button
                        type="submit"
                        class="btn btn-primary btn-join"
                        disabled={isLoading || !canSubmit}
                    >
                        {#if isLoading}
                            <span class="btn-spinner" aria-hidden="true"></span>
                            Joining…
                        {:else if !canSubmit}
                            Opens in {opensInLabel}
                        {:else}
                            Join session
                        {/if}
                    </button>

                    <p class="form-hint mic-hint">
                        Voice chat uses your microphone — your browser will ask
                        for permission once you're in.
                    </p>
                </form>

                {#if !showHostSection}
                    <p class="host-link-row" in:fade={{ duration: prefersReducedMotion ? 0 : 200, delay: prefersReducedMotion ? 0 : 320 }}>
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
        <div class="loading-state">
            <div class="waiting-spinner"></div>
        </div>
    {/if}
</main>

<style>
    .join-page {
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
    }

    .join-content {
        max-width: 400px;
        width: 100%;
    }

    .invite-header {
        text-align: center;
        margin-bottom: var(--space-xl);
    }

    .invite-header .wordmark {
        margin-bottom: var(--space-sm);
    }

    .invite-line {
        font-size: var(--text-body);
        color: var(--color-text-muted);
        margin: 0;
    }

    .room-hero {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: var(--space-sm);
        text-align: center;
        margin-bottom: var(--space-lg);
    }

    .room-hero h1 {
        margin: 0;
        font-size: clamp(1.625rem, 6vw, 2rem);
        letter-spacing: -0.015em;
        text-wrap: balance;
    }

    /* Quiet icon+text rows for session particulars */
    .session-details {
        display: flex;
        flex-wrap: wrap;
        justify-content: center;
        column-gap: var(--space-lg);
        row-gap: var(--space-xs);
        margin-bottom: var(--space-lg);
    }

    .session-detail {
        display: inline-flex;
        align-items: center;
        gap: 6px;
        font-size: var(--text-meta);
        color: var(--color-text-subtle);
        white-space: nowrap;
    }

    .session-detail svg {
        flex-shrink: 0;
        opacity: 0.8;
    }

    .join-form {
        display: flex;
        flex-direction: column;
        padding: var(--space-xl);
        box-shadow: var(--shadow-md);
    }

    .form-group {
        margin-bottom: var(--space-lg);
    }

    .form-group label {
        display: block;
        font-size: var(--text-meta);
        font-weight: 500;
        text-transform: uppercase;
        letter-spacing: 0.06em;
        margin-bottom: var(--space-sm);
        color: var(--color-text-muted);
    }

    /* Comfortable, confident fields */
    .input.input-lg {
        min-height: 48px;
        padding: 0 var(--space-md);
        font-size: 1rem;
        background: var(--color-surface-elevated);
    }

    .btn-join {
        width: 100%;
        min-height: 48px;
        padding: var(--space-md) var(--space-lg);
        font-size: 1rem;
        font-weight: 600;
        transition:
            background var(--transition-fast),
            transform var(--transition-fast),
            box-shadow var(--transition-fast);
    }

    .btn-join:not(:disabled):active {
        transform: translateY(1px) scale(0.99);
        background: var(--color-primary-hover);
    }

    .mic-hint {
        margin-top: var(--space-md);
        text-align: center;
        line-height: 1.5;
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
        text-underline-offset: 2px;
        cursor: pointer;
        transition: color var(--transition-fast);
    }

    .host-link:hover {
        color: var(--color-text);
    }

    .alert {
        margin-bottom: var(--space-lg);
    }

    .loading-state {
        display: flex;
        justify-content: center;
    }

    /* Branded hold state while the automatic admin join runs */
    .host-joining-card {
        display: flex;
        flex-direction: column;
        align-items: center;
        text-align: center;
        padding: var(--space-xl);
        box-shadow: var(--shadow-md);
    }

    .host-joining-card h1 {
        margin: 0 0 var(--space-xs);
        font-size: clamp(1.25rem, 5vw, 1.5rem);
        letter-spacing: -0.015em;
    }

    .host-joining-card p {
        margin: 0;
        font-size: var(--text-body);
        color: var(--color-text-muted);
    }

    .host-spinner {
        width: 22px;
        height: 22px;
        margin-bottom: var(--space-lg);
        border-radius: 50%;
        border: 2px solid var(--color-border);
        border-top-color: var(--color-primary);
        animation: host-spin 0.8s linear infinite;
    }

    @keyframes host-spin {
        to { transform: rotate(360deg); }
    }

    @media (prefers-reduced-motion: reduce) {
        .host-spinner {
            animation-duration: 1.6s;
        }
    }

    /* Pre-window schedule banner with the live "opens in" countdown */
    .opens-soon {
        display: flex;
        align-items: flex-start;
        gap: var(--space-sm);
        padding: var(--space-md);
        margin-bottom: var(--space-md);
        background: var(--color-surface);
        border: 1px solid var(--color-border);
        border-radius: var(--radius-md);
        color: var(--color-text-muted);
    }

    .opens-soon svg {
        flex-shrink: 0;
        margin-top: 2px;
        color: var(--color-primary);
        opacity: 0.9;
    }

    .opens-soon-copy {
        display: flex;
        flex-direction: column;
        gap: 2px;
    }

    .opens-soon-title {
        font-size: var(--text-body);
        font-weight: 500;
        color: var(--color-text);
    }

    .opens-soon-line {
        font-size: var(--text-meta);
        color: var(--color-text-subtle);
    }

    .opens-soon-count {
        color: var(--color-primary);
        font-variant-numeric: tabular-nums;
        font-weight: 600;
    }

    @media (max-width: 480px) {
        .join-page {
            padding-left: var(--space-md);
            padding-right: var(--space-md);
            align-items: flex-start;
            padding-top: calc(var(--space-2xl) + env(safe-area-inset-top, 0px));
        }

        .join-form {
            padding: var(--space-lg);
        }

        .session-details {
            column-gap: var(--space-md);
        }
    }
</style>
