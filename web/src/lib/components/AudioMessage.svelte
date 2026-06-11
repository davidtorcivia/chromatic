<script lang="ts">
    /** Minimal custom audio player for chat: the native <audio> controls
     *  were the last visibly-browser element in the room. */
    interface Props {
        src: string;
        name: string;
    }

    let { src, name }: Props = $props();

    let audioEl = $state<HTMLAudioElement | null>(null);
    let playing = $state(false);
    let duration = $state(0);
    let current = $state(0);

    function toggle() {
        if (!audioEl) return;
        if (playing) audioEl.pause();
        else void audioEl.play().catch(() => {});
    }

    function seek(e: MouseEvent) {
        if (!audioEl || !duration) return;
        const track = e.currentTarget as HTMLElement;
        const rect = track.getBoundingClientRect();
        const f = Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width));
        audioEl.currentTime = f * duration;
    }

    function fmt(s: number): string {
        if (!isFinite(s)) return "0:00";
        const m = Math.floor(s / 60);
        const sec = Math.floor(s % 60);
        return `${m}:${sec.toString().padStart(2, "0")}`;
    }
</script>

<div class="audio-msg">
    <audio
        bind:this={audioEl}
        {src}
        preload="metadata"
        onplay={() => (playing = true)}
        onpause={() => (playing = false)}
        onended={() => {
            playing = false;
            current = 0;
        }}
        onloadedmetadata={() => (duration = audioEl?.duration ?? 0)}
        ontimeupdate={() => (current = audioEl?.currentTime ?? 0)}
    ></audio>
    <button class="audio-play" onclick={toggle} aria-label={playing ? "Pause" : "Play"} title={name}>
        {#if playing}
            <svg viewBox="0 0 24 24" fill="currentColor" width="13" height="13" aria-hidden="true"><rect x="6" y="4" width="4" height="16" rx="1"/><rect x="14" y="4" width="4" height="16" rx="1"/></svg>
        {:else}
            <svg viewBox="0 0 24 24" fill="currentColor" width="13" height="13" aria-hidden="true"><path d="M8 5v14l11-7z"/></svg>
        {/if}
    </button>
    <div class="audio-body">
        <span class="audio-name">{name}</span>
        <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
        <div class="audio-track" onclick={seek}>
            <div class="audio-fill" style="width: {duration ? (current / duration) * 100 : 0}%"></div>
        </div>
    </div>
    <span class="audio-time">{fmt(playing || current > 0 ? current : duration)}</span>
</div>

<style>
    .audio-msg {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        padding: var(--space-sm) 10px;
        background: var(--color-bg);
        border: 1px solid var(--color-border-subtle);
        border-radius: 10px;
    }

    .audio-play {
        display: flex;
        align-items: center;
        justify-content: center;
        width: 30px;
        height: 30px;
        flex-shrink: 0;
        background: rgba(255, 255, 255, 0.1);
        border: none;
        border-radius: var(--radius-full);
        color: var(--color-text);
        cursor: pointer;
        transition: background 0.12s ease, transform 0.15s var(--ease-spring);
    }
    .audio-play:hover {
        background: rgba(255, 255, 255, 0.18);
    }
    .audio-play:active {
        transform: scale(0.9);
    }

    .audio-body {
        flex: 1;
        min-width: 0;
        display: flex;
        flex-direction: column;
        gap: 5px;
    }

    .audio-name {
        font-size: 0.6875rem;
        color: var(--color-text-muted);
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
    }

    .audio-track {
        height: 4px;
        border-radius: 2px;
        background: rgba(255, 255, 255, 0.1);
        cursor: pointer;
        overflow: hidden;
    }

    .audio-fill {
        height: 100%;
        border-radius: 2px;
        background: var(--color-text-muted);
        transition: width 0.1s linear;
    }

    .audio-time {
        font-size: 0.625rem;
        font-variant-numeric: tabular-nums;
        color: var(--color-text-subtle);
        flex-shrink: 0;
    }
</style>
