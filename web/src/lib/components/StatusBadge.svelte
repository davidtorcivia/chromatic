<script lang="ts">
    interface Props {
        /** Raw room status from the API: "pending" | "live" | "ended". */
        status: string;
    }

    let { status }: Props = $props();

    const labels: Record<string, string> = {
        pending: "Scheduled",
        live: "Live",
        ended: "Ended",
    };

    const label = $derived(
        labels[status] ?? status.charAt(0).toUpperCase() + status.slice(1),
    );
</script>

<span class="status-badge {status}">{label}</span>
