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

<div class="state-card stage-panel {tone}">
    {#if icon}
        <div class="state-card-icon" aria-hidden="true">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" width="22" height="22">
                {#if icon === "error"}
                    <circle cx="12" cy="12" r="10" />
                    <line x1="12" x2="12" y1="8" y2="12" />
                    <line x1="12" x2="12.01" y1="16" y2="16" />
                {:else if icon === "check"}
                    <path d="M20 6 9 17l-5-5" />
                {:else if icon === "clock"}
                    <circle cx="12" cy="12" r="10" />
                    <polyline points="12 6 12 12 16 14" />
                {:else if icon === "leave"}
                    <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" />
                    <polyline points="16 17 21 12 16 7" />
                    <line x1="21" x2="9" y1="12" y2="12" />
                {:else}
                    <!-- ended / invalid link -->
                    <path d="m2 2 20 20" />
                    <path d="M8.35 2.69A10 10 0 0 1 21.3 15.65" />
                    <path d="M19.08 19.08A10 10 0 1 1 4.92 4.92" />
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
    /* Material comes from the global .stage-panel */
    .state-card {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: var(--space-md);
        padding: var(--space-xl);
        text-align: center;
    }

    .state-card-icon {
        width: 48px;
        height: 48px;
        border-radius: 50%;
        display: flex;
        align-items: center;
        justify-content: center;
        background: rgba(255, 255, 255, 0.06);
        box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.08);
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
