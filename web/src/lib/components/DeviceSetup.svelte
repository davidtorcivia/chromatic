<script lang="ts">
    // Pre-join "green room": lets a participant grant mic/camera permission,
    // pick devices, see a live mic level + camera preview, and choose whether to
    // join with their camera on — all while they wait, so the session is ready
    // the instant they're admitted. Device choices persist to the same keys the
    // session reads (chromatic_mic_device / chromatic_camera_device) and the
    // camera toggle to the join-with-camera preference.
    //
    // Self-contained and fully torn down on destroy (streams stopped, meter RAF
    // cancelled, AudioContext closed) so it never holds the camera/mic light on
    // after the user leaves the page.
    import { onDestroy } from "svelte";
    import {
        getStoredMicDeviceId,
        storeMicDeviceId,
        getStoredCameraDeviceId,
        storeCameraDeviceId,
    } from "$lib/webrtc/manager";
    import { describeGumError, gumErrorName } from "$lib/webrtc/gum-error";
    import { getJoinWithCamera, setJoinWithCamera } from "$lib/audio/audio-mode";

    let micStream = $state<MediaStream | null>(null);
    let camStream = $state<MediaStream | null>(null);
    let micOn = $state(false);
    let camOn = $state(false);
    let micPending = $state(false);
    let camPending = $state(false);
    let micDenied = $state(false);
    let micErrorMsg = $state("Microphone blocked — allow access in your browser.");
    let camDenied = $state(false);
    let camErrorMsg = $state("Camera unavailable. Check the browser and OS permissions.");
    let micLevel = $state(0); // 0..1 smoothed
    let audioInputs = $state<MediaDeviceInfo[]>([]);
    let videoInputs = $state<MediaDeviceInfo[]>([]);
    let activeMicId = $state<string | null>(getStoredMicDeviceId());
    let activeCamId = $state<string | null>(getStoredCameraDeviceId());

    let audioCtx: AudioContext | null = null;
    let analyser: AnalyserNode | null = null;
    let meterSource: MediaStreamAudioSourceNode | null = null;
    let meterRaf: number | null = null;
    let meterBuf: Uint8Array<ArrayBuffer> | null = null;
    let videoEl = $state<HTMLVideoElement | null>(null);
    let destroyed = false;

    async function refreshDevices() {
        try {
            const devices = await navigator.mediaDevices.enumerateDevices();
            if (destroyed) return;
            audioInputs = devices.filter((d) => d.kind === "audioinput" && d.deviceId);
            videoInputs = devices.filter((d) => d.kind === "videoinput" && d.deviceId);
        } catch {
            // labels appear after permission; non-fatal
        }
    }

    function stopMic() {
        if (meterRaf !== null) {
            cancelAnimationFrame(meterRaf);
            meterRaf = null;
        }
        try {
            meterSource?.disconnect();
        } catch {
            /* gone */
        }
        meterSource = null;
        analyser = null;
        if (micStream) {
            micStream.getTracks().forEach((t) => t.stop());
            micStream = null;
        }
        micLevel = 0;
        micOn = false;
    }

    async function enableMic(deviceId?: string | null, allowDefaultRetry = true) {
        if (micPending) return;
        micPending = true;
        micDenied = false;
        try {
            const wanted = deviceId ?? activeMicId;
            const stream = await navigator.mediaDevices.getUserMedia({
                audio: wanted ? { deviceId: { ideal: wanted } } : true,
                video: false,
            });
            if (destroyed) {
                stream.getTracks().forEach((t) => t.stop());
                return;
            }
            stopMic();
            micStream = stream;
            micOn = true;
            micDenied = false;
            activeMicId = stream.getAudioTracks()[0]?.getSettings().deviceId ?? activeMicId;
            if (activeMicId) storeMicDeviceId(activeMicId);
            startMeter(stream);
            await refreshDevices();
        } catch (err) {
            console.error("Mic enable failed:", err);
            const name = gumErrorName(err);
            // A saved mic that has since vanished (very common with hot-plugged
            // capture/Blackmagic audio) fails NotFound/Overconstrained. Drop the
            // stored device and retry once with the OS default before giving up.
            if (
                allowDefaultRetry &&
                (name === "NotFoundError" || name === "OverconstrainedError") &&
                (deviceId ?? activeMicId)
            ) {
                storeMicDeviceId("");
                activeMicId = null;
                micPending = false;
                await enableMic(null, false);
                return;
            }
            micDenied = true;
            micErrorMsg = describeGumError(err, "Microphone");
        } finally {
            if (!destroyed) micPending = false;
        }
    }

    function startMeter(stream: MediaStream) {
        try {
            if (!audioCtx) audioCtx = new AudioContext();
            void audioCtx.resume().catch(() => {});
            meterSource = audioCtx.createMediaStreamSource(stream);
            analyser = audioCtx.createAnalyser();
            analyser.fftSize = 256;
            meterSource.connect(analyser);
            meterBuf = new Uint8Array(analyser.frequencyBinCount) as Uint8Array<ArrayBuffer>;
            const tick = () => {
                if (!analyser || !meterBuf) return;
                analyser.getByteFrequencyData(meterBuf);
                let sum = 0;
                for (let i = 0; i < meterBuf.length; i++) sum += meterBuf[i];
                const avg = sum / meterBuf.length / 255; // 0..1
                // Smooth + lift the low end so normal speech reads as motion.
                micLevel = micLevel * 0.6 + Math.min(1, avg * 2.2) * 0.4;
                meterRaf = requestAnimationFrame(tick);
            };
            meterRaf = requestAnimationFrame(tick);
        } catch {
            // meter is non-essential
        }
    }

    function stopCam() {
        if (camStream) {
            camStream.getTracks().forEach((t) => t.stop());
            camStream = null;
        }
        camOn = false;
    }

    async function enableCam(deviceId?: string | null, allowDefaultRetry = true) {
        if (camPending) return;
        camPending = true;
        camDenied = false;
        try {
            const wanted = deviceId ?? activeCamId;
            const stream = await navigator.mediaDevices.getUserMedia({
                video: {
                    width: { ideal: 320 },
                    height: { ideal: 240 },
                    facingMode: "user",
                    ...(wanted ? { deviceId: { ideal: wanted } } : {}),
                },
                audio: false,
            });
            if (destroyed) {
                stream.getTracks().forEach((t) => t.stop());
                return;
            }
            stopCam();
            camStream = stream;
            camOn = true;
            activeCamId = stream.getVideoTracks()[0]?.getSettings().deviceId ?? activeCamId;
            if (activeCamId) storeCameraDeviceId(activeCamId);
            setJoinWithCamera(true);
            await refreshDevices();
        } catch (err) {
            console.error("Camera enable failed:", err);
            const name = gumErrorName(err);
            // Same hot-plug reality as the mic: a remembered camera that has
            // since vanished fails NotFound/Overconstrained. Forget it and retry
            // once with the default before telling the user anything is wrong.
            if (
                allowDefaultRetry &&
                (name === "NotFoundError" || name === "OverconstrainedError") &&
                (deviceId ?? activeCamId)
            ) {
                storeCameraDeviceId(null);
                activeCamId = null;
                camPending = false;
                await enableCam(null, false);
                return;
            }
            camDenied = true;
            camErrorMsg = describeGumError(err, "Camera");
            setJoinWithCamera(false);
        } finally {
            if (!destroyed) camPending = false;
        }
    }

    function disableCam() {
        stopCam();
        setJoinWithCamera(false);
    }

    function onSelectMic(e: Event) {
        const id = (e.target as HTMLSelectElement).value;
        activeMicId = id;
        storeMicDeviceId(id);
        if (micOn) void enableMic(id);
    }

    function onSelectCam(e: Event) {
        const id = (e.target as HTMLSelectElement).value;
        activeCamId = id;
        storeCameraDeviceId(id);
        if (camOn) void enableCam(id);
    }

    // Bind the preview stream to the <video> whenever either changes.
    $effect(() => {
        const el = videoEl;
        const stream = camStream;
        if (el && stream) {
            el.srcObject = stream;
            void el.play().catch(() => {});
        } else if (el) {
            el.srcObject = null;
        }
    });

    // Default the camera toggle to the saved preference, but don't auto-prompt:
    // the user explicitly enables each device (that IS the pre-join ask).

    onDestroy(() => {
        destroyed = true;
        stopMic();
        stopCam();
        if (audioCtx) {
            void audioCtx.close().catch(() => {});
            audioCtx = null;
        }
    });
</script>

<div class="device-setup">
    <p class="device-setup-title">Set up while you wait</p>

    <!-- Microphone -->
    <div class="device-row">
        <div class="device-row-head">
            <span class="device-label">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="15" height="15"><path d="M12 2a3 3 0 0 0-3 3v7a3 3 0 0 0 6 0V5a3 3 0 0 0-3-3Z"/><path d="M19 10v2a7 7 0 0 1-14 0v-2"/><line x1="12" x2="12" y1="19" y2="22"/></svg>
                Microphone
            </span>
            {#if micOn}
                <button class="device-toggle on" onclick={stopMic}>On</button>
            {:else}
                <button class="device-toggle" onclick={() => enableMic()} disabled={micPending}>
                    {micPending ? "…" : "Enable"}
                </button>
            {/if}
        </div>
        {#if micOn}
            <div class="mic-meter" aria-hidden="true">
                <div class="mic-meter-fill" style="width: {Math.round(micLevel * 100)}%"></div>
            </div>
            {#if audioInputs.length > 0}
                <select class="device-select" value={activeMicId} onchange={onSelectMic}>
                    {#each audioInputs as d (d.deviceId)}
                        <option value={d.deviceId}>{d.label || "Microphone"}</option>
                    {/each}
                </select>
            {/if}
        {:else if micDenied}
            <p class="device-hint error">{micErrorMsg}</p>
        {/if}
    </div>

    <!-- Camera -->
    <div class="device-row">
        <div class="device-row-head">
            <span class="device-label">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="15" height="15"><path d="M23 7l-7 5 7 5V7z"/><rect x="1" y="5" width="15" height="14" rx="2" ry="2"/></svg>
                Camera
            </span>
            {#if camOn}
                <button class="device-toggle on" onclick={disableCam}>On</button>
            {:else}
                <button class="device-toggle" onclick={() => enableCam()} disabled={camPending}>
                    {camPending ? "…" : "Enable"}
                </button>
            {/if}
        </div>
        <div class="cam-preview-box" class:active={camOn}>
            {#if camOn}
                <!-- svelte-ignore a11y_media_has_caption -->
                <video bind:this={videoEl} class="cam-preview-video" muted autoplay playsinline></video>
            {:else if camDenied}
                <span class="device-hint error">{camErrorMsg}</span>
            {:else}
                <span class="device-hint">Join with your camera on for a face-to-face feel.</span>
            {/if}
        </div>
        {#if camOn && videoInputs.length > 0}
            <select class="device-select" value={activeCamId} onchange={onSelectCam}>
                {#each videoInputs as d (d.deviceId)}
                    <option value={d.deviceId}>{d.label || "Camera"}</option>
                {/each}
            </select>
        {/if}
    </div>
</div>

<style>
    .device-setup {
        width: 100%;
        display: flex;
        flex-direction: column;
        gap: var(--space-md);
        padding: var(--space-md);
        border: 1px solid rgba(255, 255, 255, 0.08);
        border-radius: var(--radius-md);
        background: rgba(255, 255, 255, 0.03);
        text-align: left;
    }
    .device-setup-title {
        margin: 0;
        font-size: var(--text-min);
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.05em;
        color: var(--color-text-muted);
    }
    .device-row {
        display: flex;
        flex-direction: column;
        gap: 8px;
    }
    .device-row-head {
        display: flex;
        align-items: center;
        justify-content: space-between;
    }
    .device-label {
        display: inline-flex;
        align-items: center;
        gap: 8px;
        font-size: 0.875rem;
        color: var(--color-text);
    }
    .device-toggle {
        min-width: 56px;
        border: 1px solid rgba(255, 255, 255, 0.16);
        border-radius: var(--radius-full);
        background: rgba(255, 255, 255, 0.06);
        color: var(--color-text);
        font-size: 0.75rem;
        font-weight: 600;
        padding: 5px 12px;
        cursor: pointer;
        transition: background 0.12s ease, border-color 0.12s ease, color 0.12s ease;
    }
    .device-toggle:hover {
        background: rgba(255, 255, 255, 0.12);
    }
    .device-toggle.on {
        background: var(--color-primary);
        border-color: transparent;
        color: #041014;
    }
    .device-toggle:disabled {
        opacity: 0.6;
        cursor: wait;
    }
    .mic-meter {
        height: 8px;
        border-radius: var(--radius-full);
        background: rgba(255, 255, 255, 0.08);
        overflow: hidden;
    }
    .mic-meter-fill {
        height: 100%;
        background: linear-gradient(90deg, var(--color-success), #f0c040 80%, #e0556a);
        border-radius: var(--radius-full);
        transition: width 0.05s linear;
    }
    .device-select {
        width: 100%;
        background: rgba(255, 255, 255, 0.05);
        border: 1px solid rgba(255, 255, 255, 0.12);
        border-radius: var(--radius-sm);
        color: var(--color-text);
        font-size: 0.8125rem;
        padding: 7px 9px;
        cursor: pointer;
    }
    /* Touch only: 44px targets + 16px text (no iOS focus-zoom). Desktop keeps
       the compact pre-join controls. */
    @media (pointer: coarse) {
        .device-toggle {
            min-width: 64px;
            min-height: 44px;
            font-size: 0.8125rem;
            padding: 7px 14px;
        }
        .device-select {
            min-height: 44px;
            font-size: 16px;
            padding: 9px 10px;
        }
    }
    .cam-preview-box {
        width: 100%;
        aspect-ratio: 4 / 3;
        border-radius: var(--radius-sm);
        overflow: hidden;
        background: #000;
        border: 1px solid rgba(255, 255, 255, 0.08);
        display: flex;
        align-items: center;
        justify-content: center;
        padding: var(--space-md);
        text-align: center;
    }
    .cam-preview-box.active {
        padding: 0;
    }
    .cam-preview-video {
        width: 100%;
        height: 100%;
        object-fit: cover;
        transform: scaleX(-1); /* self-view mirror */
    }
    .device-hint {
        font-size: 0.75rem;
        color: var(--color-text-subtle);
    }
    .device-hint.error {
        color: var(--color-error);
    }
</style>
