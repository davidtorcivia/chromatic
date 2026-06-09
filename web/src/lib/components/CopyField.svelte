<script lang="ts">
    interface Props {
        /** The value copied to the clipboard. */
        value: string;
        /** Optional small label rendered above the field. */
        label?: string;
        /** Optional masked text shown instead of the value (value is still copied). */
        display?: string;
        copyLabel?: string;
        copiedLabel?: string;
    }

    let {
        value,
        label = "",
        display = "",
        copyLabel = "Copy",
        copiedLabel = "Copied",
    }: Props = $props();

    let copied = $state(false);
    let timer: ReturnType<typeof setTimeout> | undefined;

    async function copy() {
        try {
            await navigator.clipboard.writeText(value);
            copied = true;
            clearTimeout(timer);
            timer = setTimeout(() => (copied = false), 2000);
        } catch (e) {
            console.error("Failed to copy to clipboard", e);
        }
    }
</script>

<div class="copy-field-wrap">
    {#if label}
        <span class="copy-label">{label}</span>
    {/if}
    <div class="copy-row">
        <code>{display || value}</code>
        <button
            type="button"
            class="btn btn-secondary btn-sm"
            class:copied
            onclick={copy}
            aria-live="polite"
        >
            {copied ? copiedLabel : copyLabel}
        </button>
    </div>
</div>

<style>
    .copy-label {
        display: block;
        font-size: var(--text-meta);
        color: var(--color-text-muted);
        margin-bottom: var(--space-xs);
    }

    .copy-row {
        display: flex;
        gap: var(--space-sm);
        align-items: center;
    }

    .copy-row code {
        flex: 1;
        padding: var(--space-sm);
        background: var(--color-surface-elevated);
        border: 1px solid var(--color-border-subtle);
        border-radius: var(--radius-md);
        font-size: var(--text-meta);
        overflow-x: auto;
        white-space: nowrap;
    }

    .copy-row .btn.copied {
        color: var(--color-success);
        border-color: var(--color-success);
    }
</style>
