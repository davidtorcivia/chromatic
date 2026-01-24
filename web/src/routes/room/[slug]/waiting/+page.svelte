<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { page } from "$app/stores";
    import { rooms } from "$lib/api/client";

    const slug = $page.params.slug!;

    let status = $state<"waiting" | "admitted" | "ended" | "error">("waiting");
    let error = $state("");
    let pollInterval: ReturnType<typeof setInterval>;

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

        // Start polling for admission status
        checkStatus();
        pollInterval = setInterval(checkStatus, 3000);
    });

    onDestroy(() => {
        if (pollInterval) {
            clearInterval(pollInterval);
        }
    });

    async function checkStatus() {
        if (!sessionData) return;

        try {
            const result = await rooms.checkStatus(slug, sessionData.participantId);

            if (result.roomStatus === "ended") {
                status = "ended";
                clearInterval(pollInterval);
                return;
            }

            if (result.isAdmitted) {
                status = "admitted";
                clearInterval(pollInterval);
                // Redirect to session
                window.location.href = `/room/${slug}/session`;
            }
        } catch (e: any) {
            // If participant not found, may have been removed
            if (e.message.includes("not found")) {
                error = "You have been removed from the waiting room.";
                status = "error";
                clearInterval(pollInterval);
            }
        }
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
    <div class="waiting-content">
        {#if status === "waiting"}
            <div class="waiting-card">
                <div class="waiting-spinner large"></div>
                <h1>Waiting to be admitted</h1>
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

    .waiting-spinner {
        margin: 0 auto var(--space-lg);
    }

    .waiting-spinner.large {
        width: 60px;
        height: 60px;
        border-width: 4px;
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

    .btn-secondary {
        background: var(--color-surface-hover);
        color: var(--color-text);
        border: 1px solid var(--color-border);
    }

    .btn-secondary:hover {
        background: var(--color-border);
    }
</style>
