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

    // duration can be Infinity (MediaRecorder ogg/webm clips in Chrome
    // report Infinity at loadedmetadata) — only finite durations are
    // seekable, and Infinity must never reach the currentTime setter.
    let seekable = $derived(isFinite(duration) && duration > 0);

    function seek(e: MouseEvent) {
        if (!audioEl || !seekable) return;
        const track = e.currentTarget as HTMLElement;
        const rect = track.getBoundingClientRect();
        const f = Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width));
        audioEl.currentTime = f * duration;
    }

    function seekKeydown(e: KeyboardEvent) {
        if (!audioEl || !seekable) return;
        const step = duration / 20;
        if (e.key === "ArrowRight" || e.key === "ArrowUp") {
            e.preventDefault();
            audioEl.currentTime = Math.min(duration, audioEl.currentTime + step);
        } else if (e.key === "ArrowLeft" || e.key === "ArrowDown") {
            e.preventDefault();
            audioEl.currentTime = Math.max(0, audioEl.currentTime - step);
        } else if (e.key === "Home") {
            e.preventDefault();
            audioEl.currentTime = 0;
        } else if (e.key === "End") {
            e.preventDefault();
            audioEl.currentTime = duration;
        }
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
        ondurationchange={() => (duration = audioEl?.duration ?? 0)}
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
        <div
            class="audio-track"
            role="slider"
            tabindex="0"
            aria-label="Seek"
            aria-valuemin="0"
            aria-valuemax={seekable ? Math.round(duration) : 0}
            aria-valuenow={Math.round(current)}
            aria-valuetext={fmt(current)}
            onclick={seek}
            onkeydown={seekKeydown}
        >
            <div class="audio-fill" style="width: {seekable ? (current / duration) * 100 : 0}%"></div>
        </div>
    </div>
    <span class="audio-time">{fmt(playing || current > 0 ? current : seekable ? duration : 0)}</span>
    <a
        class="audio-open"
        href={src}
        target="_blank"
        rel="noopener noreferrer"
        aria-label="Open audio file"
        title="Open original file"
    >
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="12" height="12" aria-hidden="true"><path d="M15 3h6v6"/><path d="M10 14 21 3"/><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/></svg>
    </a>
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

    .audio-track:focus-visible {
        outline: none;
        box-shadow: 0 0 0 2px rgba(255, 255, 255, 0.4);
    }

    .audio-open {
        display: flex;
        align-items: center;
        justify-content: center;
        width: 22px;
        height: 22px;
        flex-shrink: 0;
        border-radius: var(--radius-full);
        color: var(--color-text-subtle);
        transition: background 0.12s ease, color 0.12s ease;
    }
    .audio-open:hover {
        background: rgba(255, 255, 255, 0.08);
        color: var(--color-text);
    }
</style>
