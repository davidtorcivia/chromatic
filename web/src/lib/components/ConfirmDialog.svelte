<script lang="ts">
    import { fade, fly } from "svelte/transition";

    interface Props {
        open: boolean;
        title: string;
        body?: string;
        confirmLabel?: string;
        cancelLabel?: string;
        danger?: boolean;
        onConfirm: () => void;
        onCancel: () => void;
    }

    let {
        open,
        title,
        body = "",
        confirmLabel = "Confirm",
        cancelLabel = "Cancel",
        danger = false,
        onConfirm,
        onCancel,
    }: Props = $props();

    let dialogEl = $state<HTMLDivElement | null>(null);
    let previouslyFocused: HTMLElement | null = null;

    $effect(() => {
        if (open && dialogEl) {
            previouslyFocused = document.activeElement as HTMLElement | null;
            const target = dialogEl.querySelector<HTMLElement>(
                "[data-autofocus]",
            );
            (target ?? dialogEl).focus();
            return () => {
                previouslyFocused?.focus?.();
                previouslyFocused = null;
            };
        }
    });

    function handleKeydown(e: KeyboardEvent) {
        if (e.key === "Escape") {
            e.stopPropagation();
            onCancel();
            return;
        }
        // Simple focus trap
        if (e.key === "Tab" && dialogEl) {
            const focusables = Array.from(
                dialogEl.querySelectorAll<HTMLElement>(
                    'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
                ),
            );
            if (focusables.length === 0) return;
            const first = focusables[0];
            const last = focusables[focusables.length - 1];
            if (e.shiftKey && document.activeElement === first) {
                e.preventDefault();
                last.focus();
            } else if (!e.shiftKey && document.activeElement === last) {
                e.preventDefault();
                first.focus();
            }
        }
    }
</script>

{#if open}
    <div
        class="dialog-backdrop"
        transition:fade={{ duration: 150 }}
        onclick={onCancel}
        onkeydown={handleKeydown}
        role="presentation"
    >
        <div
            class="dialog card"
            class:danger
            role="dialog"
            aria-modal="true"
            aria-labelledby="confirm-dialog-title"
            tabindex="-1"
            bind:this={dialogEl}
            transition:fly={{ y: 8, duration: 200 }}
            onclick={(e) => e.stopPropagation()}
            onkeydown={handleKeydown}
        >
            <h3 id="confirm-dialog-title">{title}</h3>
            {#if body}
                <p class="dialog-body">{body}</p>
            {/if}
            <div class="dialog-actions">
                <button type="button" class="btn btn-secondary" onclick={onCancel}>
                    {cancelLabel}
                </button>
                <button
                    type="button"
                    class="btn"
                    class:btn-primary={!danger}
                    class:btn-danger={danger}
                    data-autofocus
                    onclick={onConfirm}
                >
                    {confirmLabel}
                </button>
            </div>
        </div>
    </div>
{/if}

<style>
    .dialog-backdrop {
        position: fixed;
        inset: 0;
        background: rgba(0, 0, 0, 0.6);
        display: flex;
        align-items: center;
        justify-content: center;
        padding: var(--space-lg);
        z-index: 2000;
    }

    .dialog {
        width: 100%;
        max-width: 420px;
        background: var(--color-surface-elevated);
        box-shadow: var(--shadow-lg);
        outline: none;
    }

    .dialog.danger {
        border-color: var(--color-error);
    }

    .dialog h3 {
        margin: 0 0 var(--space-sm);
        font-size: var(--text-card-title);
        font-weight: 600;
    }

    .dialog.danger h3 {
        color: var(--color-error);
    }

    .dialog-body {
        margin: 0 0 var(--space-lg);
        font-size: var(--text-body);
        color: var(--color-text-muted);
    }

    .dialog-actions {
        display: flex;
        justify-content: flex-end;
        gap: var(--space-sm);
        margin-top: var(--space-md);
    }
</style>
