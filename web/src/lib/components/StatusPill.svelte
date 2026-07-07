<script lang="ts">
    let { tone = 'neutral', label }: {
        tone?: 'good' | 'warn' | 'bad' | 'neutral' | 'checking' | 'optional';
        label: string;
    } = $props();
</script>

<span class="status-pill {tone}">{label}</span>

<style>
    .status-pill {
        padding: 0.2rem 0.5rem;
        border-radius: var(--radius-full);
        font-size: 0.75rem;
        border: 1px solid transparent;
        background: var(--color-neutral-bg);
        color: var(--color-text-muted);
        white-space: nowrap;
    }

    .status-pill.good {
        background: var(--color-success-bg);
        border-color: rgba(47, 191, 113, 0.35);
        color: var(--color-success);
    }

    .status-pill.warn {
        background: var(--color-warning-bg);
        border-color: rgba(230, 162, 60, 0.35);
        color: var(--color-warning);
    }

    .status-pill.bad {
        background: var(--color-error-bg);
        border-color: rgba(239, 90, 90, 0.35);
        color: var(--color-error);
    }

    .status-pill.checking,
    .status-pill.neutral,
    .status-pill.optional {
        background: var(--color-neutral-bg);
        border-color: var(--color-border);
        color: var(--color-text-muted);
    }
</style>
