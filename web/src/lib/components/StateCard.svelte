<script lang="ts">
    import type { Snippet } from "svelte";

    /**
     * Shared empty/edge-state card: icon, title, body, optional actions.
     * Used by the join and waiting pages so every "ended / not found /
     * connection trouble" moment has the same calm anatomy.
     */
    interface Props {
        icon?: "error" | "ended" | "clock" | "check" | "leave";
        tone?: "neutral" | "error" | "success";
        title: string;
        body?: string;
        children?: Snippet;
    }

    let { icon, tone = "neutral", title, body, children }: Props = $props();
</script>

<div class="state-card {tone}">
    {#if icon}
        <div class="state-card-icon" aria-hidden="true">
            <svg viewBox="0 0 24 24" fill="currentColor" width="22" height="22">
                {#if icon === "error"}
                    <path d="M11 7h2v6h-2zM11 15h2v2h-2z" />
                    <path
                        d="M12 2C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm0 18c-4.41 0-8-3.59-8-8s3.59-8 8-8 8 3.59 8 8-3.59 8-8 8z"
                    />
                {:else if icon === "check"}
                    <path d="M9 16.17L4.83 12l-1.42 1.41L9 19 21 7l-1.41-1.41z" />
                {:else if icon === "clock"}
                    <path
                        d="M11.99 2C6.47 2 2 6.48 2 12s4.47 10 9.99 10C17.52 22 22 17.52 22 12S17.52 2 11.99 2zM12 20c-4.42 0-8-3.58-8-8s3.58-8 8-8 8 3.58 8 8-3.58 8-8 8zm.5-13H11v6l5.25 3.15.75-1.23-4.5-2.67z"
                    />
                {:else if icon === "leave"}
                    <path
                        d="M17 7l-1.41 1.41L18.17 11H8v2h10.17l-2.58 2.58L17 17l5-5zM4 5h8V3H4c-1.1 0-2 .9-2 2v14c0 1.1.9 2 2 2h8v-2H4V5z"
                    />
                {:else}
                    <!-- ended / invalid link -->
                    <path
                        d="M17 7h-4v1.9h4c1.71 0 3.1 1.39 3.1 3.1 0 1.43-.98 2.63-2.31 2.98l1.46 1.46C20.88 15.61 22 13.95 22 12c0-2.76-2.24-5-5-5zm-1 4h-2.19l2 2H16v-2zM2 4.27l3.11 3.11C3.29 8.12 2 9.91 2 12c0 2.76 2.24 5 5 5h4v-1.9H7c-1.71 0-3.1-1.39-3.1-3.1 0-1.59 1.21-2.9 2.76-3.08L8.73 11H8v2h2.73L13 15.27V17h1.73l4.01 4L20 19.74 3.27 3 2 4.27z"
                    />
                {/if}
            </svg>
        </div>
    {/if}
    <h1 class="state-card-title">{title}</h1>
    {#if body}
        <p class="state-card-body">{body}</p>
    {/if}
    {#if children}
        <div class="state-card-actions">
            {@render children()}
        </div>
    {/if}
</div>

<style>
    .state-card {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: var(--space-md);
        padding: var(--space-xl);
        text-align: center;
        background: var(--color-surface);
        border: 1px solid var(--color-border);
        border-radius: var(--radius-lg);
        box-shadow: var(--shadow-md);
    }

    .state-card-icon {
        width: 48px;
        height: 48px;
        border-radius: 50%;
        display: flex;
        align-items: center;
        justify-content: center;
        background: var(--color-neutral-bg);
        color: var(--color-text-muted);
    }

    .state-card.error .state-card-icon {
        background: var(--color-error-bg);
        color: var(--color-error);
    }

    .state-card.success .state-card-icon {
        background: var(--color-success-bg);
        color: var(--color-success);
    }

    .state-card-title {
        margin: 0;
        font-family: var(--font-display);
        font-size: 1.25rem;
        font-weight: 600;
        letter-spacing: -0.01em;
        color: var(--color-text);
    }

    .state-card-body {
        margin: 0;
        font-size: var(--text-body);
        line-height: 1.55;
        color: var(--color-text-muted);
        max-width: 34ch;
    }

    .state-card-actions {
        display: flex;
        flex-wrap: wrap;
        justify-content: center;
        gap: var(--space-sm);
        margin-top: var(--space-xs);
    }
</style>
