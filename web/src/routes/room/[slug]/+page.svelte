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

<main class="join-page stage">
    {#if hostJoinState === "joining"}
        <!-- Branded hold state while the automatic admin join runs (no form flash) -->
        <div class="join-content host-joining" in:fade={{ duration: prefersReducedMotion ? 0 : 150 }}>
            <div class="invite-header">
                <div class="wordmark">Chromatic</div>
            </div>
            <div class="stage-panel host-joining-card">
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
            {#if roomInfo.status === "ended"}
                <div class="invite-header" in:fly={enter(0)}>
                    <div class="wordmark">Chromatic</div>
                </div>
                <div in:fly={enter(1)}>
                    <StateCard
                        icon="ended"
                        title="This session has ended"
                        body="The review is over. If you were expecting to join, check with your host for a new link."
                    />
                </div>
            {:else}
            <div class="stage-panel join-panel" in:fly={enter(0)}>
                <div class="wordmark">Chromatic</div>
                <p class="invite-line" in:fly={enter(1)}>You've been invited to a live color review.</p>
                <h1 class="join-room-name" in:fly={enter(1)}>{roomInfo.name}</h1>

                <div class="session-details" in:fly={enter(2)}>
                    {#if roomInfo.status === "live"}
                        <span class="session-detail live-detail">
                            <span class="live-dot-mini" aria-hidden="true"></span>
                            Live now
                        </span>
                    {/if}
                    {#if scheduledAt}
                        <span class="session-detail">
                            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="13" height="13" aria-hidden="true"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>
                            {scheduledLabel}
                        </span>
                    {/if}
                    {#if roomInfo.hasPassword}
                        <span class="session-detail">
                            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="13" height="13" aria-hidden="true"><rect width="18" height="11" x="3" y="11" rx="2"/><path d="M7 11V7a5 5 0 0 1 10 0v4"/></svg>
                            Private session
                        </span>
                    {/if}
                    {#if roomInfo.waitingRoomEnabled}
                        <span class="session-detail">
                            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="13" height="13" aria-hidden="true"><path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1 1 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"/></svg>
                            Host admits each guest
                        </span>
                    {/if}
                </div>

                <div class="join-divider" aria-hidden="true"></div>

                {#if !canJoin}
                    <div class="opens-soon" in:fly={enter(3)} aria-live="polite">
                        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="16" height="16" aria-hidden="true"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>
                        <div class="opens-soon-copy">
                            <span class="opens-soon-title">Scheduled for {scheduledLabel}</span>
                            <span class="opens-soon-line">The room opens in <strong class="opens-soon-count">{opensInLabel}</strong>. The Join button will unlock automatically.</span>
                        </div>
                    </div>
                {/if}
                <form class="join-form" onsubmit={handleJoin}>
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
                        Voice chat uses your microphone, so your browser will ask
                        for permission once you're in.
                    </p>
                </form>
            </div>

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
    /* Background comes from the shared .stage (aurora + drift + grain) */
    .join-page {
        min-height: 100vh;
        min-height: 100dvh;
        display: flex;
        align-items: center;
        justify-content: center;
        padding: var(--space-lg);
        padding-top: calc(var(--space-lg) + env(safe-area-inset-top, 0px));
        padding-bottom: calc(var(--space-lg) + env(safe-area-inset-bottom, 0px));
    }

    .join-content {
        max-width: 420px;
        width: 100%;
    }

    /* One centered panel carries the whole invitation, like the lobby */
    .join-panel {
        display: flex;
        flex-direction: column;
        padding: var(--space-xl);
        text-align: center;
    }

    .join-panel .wordmark {
        margin-bottom: var(--space-md);
    }

    .join-room-name {
        margin: var(--space-xs) 0 var(--space-md);
        font-size: clamp(1.5rem, 6vw, 1.875rem);
        letter-spacing: -0.015em;
        text-wrap: balance;
    }

    .join-divider {
        height: 1px;
        margin: var(--space-lg) calc(-1 * var(--space-xl)) var(--space-lg);
        background: linear-gradient(
            to right,
            transparent,
            rgba(255, 255, 255, 0.07) 20%,
            rgba(255, 255, 255, 0.07) 80%,
            transparent
        );
    }

    .live-detail {
        color: var(--color-success);
    }

    .live-dot-mini {
        width: 7px;
        height: 7px;
        border-radius: 50%;
        background: var(--color-success);
        animation: live-mini-pulse 1.6s ease-in-out infinite;
    }

    @keyframes live-mini-pulse {
        0%, 100% { opacity: 1; }
        50% { opacity: 0.45; }
    }

    .invite-header {
        text-align: center;
        margin-bottom: var(--space-xl);
    }

    .invite-line {
        font-size: var(--text-body);
        color: var(--color-text-muted);
        margin: 0;
    }

    /* Quiet icon+text rows for session particulars */
    .session-details {
        display: flex;
        flex-wrap: wrap;
        justify-content: center;
        column-gap: var(--space-lg);
        row-gap: var(--space-xs);
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
        text-align: left;
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

    /* Filled, borderless fields: the border exists only to show focus */
    .input.input-lg {
        min-height: 48px;
        padding: 0 var(--space-md);
        font-size: 1rem;
        background: rgba(255, 255, 255, 0.06);
        border-color: transparent;
        border-radius: 12px;
        transition:
            border-color var(--transition-fast),
            background var(--transition-fast),
            box-shadow var(--transition-fast);
    }

    .input.input-lg:hover {
        background: rgba(255, 255, 255, 0.08);
    }

    .input.input-lg:focus,
    .input.input-lg:focus-visible {
        background: rgba(255, 255, 255, 0.07);
        border-color: rgba(72, 182, 166, 0.55);
        box-shadow: 0 0 0 3px rgba(72, 182, 166, 0.14);
    }

    /* The one saturated moment on the page: a lit teal capsule */
    .btn-join {
        width: 100%;
        min-height: 48px;
        padding: var(--space-md) var(--space-lg);
        font-size: 1rem;
        font-weight: 600;
        border-radius: var(--radius-full);
        background: linear-gradient(to bottom, #54c2b2, #3da394);
        color: #04201c;
        box-shadow:
            inset 0 1px 0 rgba(255, 255, 255, 0.28),
            0 6px 18px rgba(61, 163, 148, 0.22);
        transition:
            filter var(--transition-fast),
            transform 0.2s var(--ease-spring),
            box-shadow var(--transition-fast);
    }

    .btn-join:not(:disabled):hover {
        background: linear-gradient(to bottom, #5ccaba, #45ab9c);
        transform: translateY(-1px);
        box-shadow:
            inset 0 1px 0 rgba(255, 255, 255, 0.32),
            0 10px 28px rgba(72, 182, 166, 0.32);
    }

    .btn-join:not(:disabled):active {
        transform: translateY(0) scale(0.985);
        box-shadow:
            inset 0 1px 0 rgba(255, 255, 255, 0.22),
            0 4px 12px rgba(61, 163, 148, 0.2);
    }

    .btn-join:disabled {
        background: rgba(255, 255, 255, 0.06);
        color: var(--color-text-subtle);
        box-shadow: none;
        opacity: 1;
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

    /* Pre-window schedule note: a quiet inset row inside the panel */
    .opens-soon {
        display: flex;
        align-items: flex-start;
        gap: var(--space-sm);
        padding: var(--space-md);
        margin-bottom: var(--space-lg);
        text-align: left;
        background: rgba(255, 255, 255, 0.035);
        border-radius: 12px;
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

        .join-panel {
            padding: var(--space-lg);
        }

        .join-divider {
            margin-left: calc(-1 * var(--space-lg));
            margin-right: calc(-1 * var(--space-lg));
        }

        .session-details {
            column-gap: var(--space-md);
        }
    }
</style>
