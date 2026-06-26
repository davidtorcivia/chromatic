<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { fade } from "svelte/transition";
    import { goto } from "$app/navigation";
    import { page } from "$app/stores";
    import { rooms, streamKeys, appConfig, roomFiles, type Room, type StreamKey, type AppConfig, type RoomFile } from "$lib/api/client";
    import ConfirmDialog from "$lib/components/ConfirmDialog.svelte";
    import CopyField from "$lib/components/CopyField.svelte";
    import StatusBadge from "$lib/components/StatusBadge.svelte";
    import WatermarkOverlay from "$lib/components/WatermarkOverlay.svelte";

    const slug = $page.params.slug!;

    let room = $state<Room | null>(null);
    let keys = $state<StreamKey[]>([]);
    let config = $state<AppConfig | null>(null);
    let waitingParticipants = $state<{ id: string; name: string; joinedAt: string }[]>([]);
    let isLoading = $state(true);
    let isSaving = $state(false);
    let error = $state("");
    let successMessage = $state("");
    let confirmEndOpen = $state(false);
    let confirmDeleteOpen = $state(false);
    // Uploaded-files review (admin moderation)
    let files = $state<RoomFile[]>([]);
    let deleteRoomFiles = $state(true);
    let fileToDelete = $state<RoomFile | null>(null);

    // Form fields (populated from room)
    let name = $state("");
    let password = $state("");
    let waitingRoomEnabled = $state(false);
    // Scheduling: local datetime-local string ("" = unscheduled) + lobby window
    let scheduledAtLocal = $state("");
    let earlyOpenMinutes = $state(10);
    let isOpeningRoom = $state(false);
    let streamKeyId = $state<string>("");
    let watermarkMode = $state<"none" | "text" | "logo" | "both">("text");
    let watermarkText = $state("");
    let watermarkLogoPosition = $state<"top-left" | "top-right" | "bottom-left" | "bottom-right">("bottom-right");
    let watermarkOpacity = $state(0.3);
    // Fractional center of the watermark (null = built-in placement)
    let watermarkPosX = $state<number | null>(null);
    let watermarkPosY = $state<number | null>(null);
    let watermarkScale = $state(1);
    // Per-room participant limit (null = use the global default)
    let maxParticipants = $state<number | null>(null);

    // Watermark drag state for the preview
    type Bounds = { x: number; y: number; width: number; height: number };
    let previewEl = $state<HTMLDivElement | null>(null);
    let watermarkBounds = $state<Bounds | null>(null);
    let isPreviewHovered = $state(false);
    let isOverWatermark = $state(false);
    let isDraggingWatermark = $state(false);
    let dragOffsetX = 0;
    let dragOffsetY = 0;
    let destroyed = false;
    let successTimer: ReturnType<typeof setTimeout> | null = null;

    function previewPoint(e: PointerEvent) {
        const rect = previewEl!.getBoundingClientRect();
        return {
            x: e.clientX - rect.left,
            y: e.clientY - rect.top,
            w: rect.width,
            h: rect.height,
        };
    }

    function pointInWatermark(x: number, y: number): boolean {
        if (!watermarkBounds) return false;
        const pad = 6; // forgiving hit area
        return (
            x >= watermarkBounds.x - pad &&
            x <= watermarkBounds.x + watermarkBounds.width + pad &&
            y >= watermarkBounds.y - pad &&
            y <= watermarkBounds.y + watermarkBounds.height + pad
        );
    }

    function handlePreviewPointerDown(e: PointerEvent) {
        if (!previewEl || !watermarkBounds) return;
        const p = previewPoint(e);
        if (!pointInWatermark(p.x, p.y)) return;
        isDraggingWatermark = true;
        // Keep the grab point stable relative to the watermark center
        dragOffsetX = p.x - (watermarkBounds.x + watermarkBounds.width / 2);
        dragOffsetY = p.y - (watermarkBounds.y + watermarkBounds.height / 2);
        previewEl.setPointerCapture(e.pointerId);
        e.preventDefault();
    }

    function handlePreviewPointerMove(e: PointerEvent) {
        if (!previewEl) return;
        const p = previewPoint(e);
        if (!isDraggingWatermark) {
            isOverWatermark = pointInWatermark(p.x, p.y);
            return;
        }
        // Clamp the fractional center so the watermark stays inside the frame
        const halfW = (watermarkBounds?.width ?? 0) / 2;
        const halfH = (watermarkBounds?.height ?? 0) / 2;
        const loX = Math.min(0.5, halfW / p.w);
        const hiX = Math.max(0.5, 1 - halfW / p.w);
        const loY = Math.min(0.5, halfH / p.h);
        const hiY = Math.max(0.5, 1 - halfH / p.h);
        watermarkPosX = Math.min(hiX, Math.max(loX, (p.x - dragOffsetX) / p.w));
        watermarkPosY = Math.min(hiY, Math.max(loY, (p.y - dragOffsetY) / p.h));
    }

    function handlePreviewPointerUp(e: PointerEvent) {
        if (!isDraggingWatermark) return;
        isDraggingWatermark = false;
        previewEl?.releasePointerCapture?.(e.pointerId);
    }

    // Polling interval for waiting room
    let pollInterval: ReturnType<typeof setInterval> | null = null;

    onMount(async () => {
        try {
            const [roomData, keysData, configData] = await Promise.all([
                rooms.get(slug),
                streamKeys.list(),
                appConfig.get().catch(() => null)
            ]);

            if (destroyed) return;
            room = roomData;
            keys = keysData;
            config = configData;

            // Populate form fields
            name = room.name;
            waitingRoomEnabled = room.waitingRoomEnabled;
            scheduledAtLocal = isoToLocalInput(room.scheduledAt);
            earlyOpenMinutes = room.earlyOpenMinutes ?? 10;
            streamKeyId = room.streamKeyId || "";
            watermarkMode = (room.watermarkMode as "none" | "text" | "logo" | "both") || "text";
            watermarkText = room.watermarkText || "{{ name }} - {{ date }}";
            watermarkLogoPosition = (room.watermarkLogoPosition as typeof watermarkLogoPosition) || "bottom-right";
            watermarkOpacity = room.watermarkOpacity ?? 0.3;
            watermarkPosX = room.watermarkPosX ?? null;
            watermarkPosY = room.watermarkPosY ?? null;
            watermarkScale = room.watermarkScale ?? 1;
            maxParticipants = room.maxParticipants ?? null;

            // Start polling waiting room if enabled
            if (room.waitingRoomEnabled && room.status !== "ended") {
                startWaitingRoomPolling();
            }

            void loadFiles();
        } catch (e) {
            if (destroyed) return;
            error = "Failed to load room";
            console.error(e);
        } finally {
            if (!destroyed) isLoading = false;
        }
    });

    onDestroy(() => {
        destroyed = true;
        stopWaitingRoomPolling();
        clearSuccessTimer();
    });

    function clearSuccessTimer() {
        if (!successTimer) return;
        clearTimeout(successTimer);
        successTimer = null;
    }

    function showSuccess(message: string) {
        clearSuccessTimer();
        successMessage = message;
        successTimer = setTimeout(() => {
            successMessage = "";
            successTimer = null;
        }, 4000);
    }

    function stopWaitingRoomPolling() {
        if (!pollInterval) return;
        clearInterval(pollInterval);
        pollInterval = null;
    }

    function startWaitingRoomPolling() {
        if (pollInterval || destroyed) return;
        void loadWaitingRoom();
        pollInterval = setInterval(() => {
            void loadWaitingRoom();
        }, 5000);
    }

    function reconcileWaitingRoomPolling() {
        if (room?.waitingRoomEnabled && room.status !== "ended") {
            startWaitingRoomPolling();
        } else {
            stopWaitingRoomPolling();
            waitingParticipants = [];
        }
    }

    async function loadWaitingRoom() {
        try {
            const participants = (await rooms.listWaiting(slug)) ?? [];
            if (destroyed) return;
            waitingParticipants = participants;
        } catch (e) {
            if (destroyed) return;
            console.error("Failed to load waiting room", e);
        }
    }

    async function loadFiles() {
        try {
            const result = (await roomFiles.list(slug))?.files ?? [];
            if (destroyed) return;
            files = result;
        } catch (e) {
            if (destroyed) return;
            console.error("Failed to load room files", e);
        }
    }

    async function handleDeleteFile() {
        if (!fileToDelete) return;
        const target = fileToDelete;
        fileToDelete = null;
        try {
            await roomFiles.delete(target.id);
            files = files.filter((f) => f.id !== target.id);
        } catch (e: any) {
            error = e.message || "Failed to delete file";
        }
    }

    function formatBytes(bytes: number): string {
        if (bytes < 1024) return `${bytes} B`;
        if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
        return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
    }

    // datetime-local <-> ISO conversion (datetime-local has no timezone; it
    // represents the admin's local wall-clock time)
    function isoToLocalInput(iso?: string): string {
        if (!iso) return "";
        const d = new Date(iso);
        if (Number.isNaN(d.getTime())) return "";
        const pad = (n: number) => String(n).padStart(2, "0");
        return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
    }

    function localInputToIso(local: string): string | null {
        if (!local) return null;
        const d = new Date(local);
        return Number.isNaN(d.getTime()) ? null : d.toISOString();
    }

    async function handleOpenRoom() {
        if (isOpeningRoom) return;
        isOpeningRoom = true;
        error = "";
        try {
            const result = await rooms.open(slug);
            if (destroyed) return;
            if (room) {
                room.openedAt = result.openedAt;
            }
            waitingParticipants = [];
            showSuccess("Room opened. Waiting guests have been let in.");
        } catch (e: any) {
            if (destroyed) return;
            error = e.message || "Failed to open the room";
        } finally {
            if (!destroyed) isOpeningRoom = false;
        }
    }

    async function handleDeny(participantId: string) {
        try {
            await rooms.deny(slug, participantId);
            if (destroyed) return;
            waitingParticipants = waitingParticipants.filter(p => p.id !== participantId);
        } catch (e) {
            if (destroyed) return;
            console.error("Failed to deny participant", e);
        }
    }

    async function handleSave(e: SubmitEvent) {
        e.preventDefault();
        if (!name.trim()) return;

        isSaving = true;
        error = "";
        successMessage = "";

        try {
            const updateData: Record<string, unknown> = {
                name: name.trim(),
                waitingRoomEnabled,
                watermarkMode,
                scheduledAt: localInputToIso(scheduledAtLocal),
                earlyOpenMinutes:
                    typeof earlyOpenMinutes === "number" && !Number.isNaN(earlyOpenMinutes)
                        ? Math.min(120, Math.max(0, earlyOpenMinutes))
                        : 10,
            };

            if (password) {
                updateData.password = password;
            }

            if (streamKeyId) {
                updateData.streamKeyId = streamKeyId;
            } else {
                updateData.streamKeyId = null;
            }

            if (watermarkMode === "text" || watermarkMode === "both") {
                updateData.watermarkText = watermarkText;
            }

            if (watermarkMode === "logo" || watermarkMode === "both") {
                updateData.watermarkLogoPosition = watermarkLogoPosition;
            }

            updateData.watermarkOpacity = watermarkOpacity;
            updateData.watermarkScale = watermarkScale;
            // null clears the custom position (back to built-in placement)
            updateData.watermarkPosX = watermarkPosX;
            updateData.watermarkPosY = watermarkPosY;

            updateData.maxParticipants =
                typeof maxParticipants === "number" && !Number.isNaN(maxParticipants)
                    ? maxParticipants
                    : null;

            const updatedRoom = await rooms.update(slug, updateData);
            if (destroyed) return;
            room = updatedRoom;
            reconcileWaitingRoomPolling();
            showSuccess("Room updated successfully");
            password = ""; // Clear password field after save
        } catch (e: any) {
            if (destroyed) return;
            error = e.message || "Failed to update room";
        } finally {
            if (!destroyed) isSaving = false;
        }
    }

    async function handleAdmit(participantId: string) {
        try {
            await rooms.admit(slug, participantId);
            if (destroyed) return;
            waitingParticipants = waitingParticipants.filter(p => p.id !== participantId);
        } catch (e) {
            if (destroyed) return;
            console.error("Failed to admit participant", e);
        }
    }

    async function handleAdmitAll() {
        try {
            await rooms.admitAll(slug);
            if (destroyed) return;
            waitingParticipants = [];
        } catch (e) {
            if (destroyed) return;
            console.error("Failed to admit all", e);
        }
    }

    async function handleEndSession() {
        confirmEndOpen = false;
        try {
            await rooms.end(slug);
            if (destroyed) return;
            if (room) {
                room.status = "ended";
            }
            reconcileWaitingRoomPolling();
        } catch (e) {
            if (destroyed) return;
            console.error("Failed to end session", e);
        }
    }

    async function handleDelete() {
        confirmDeleteOpen = false;
        try {
            await rooms.delete(slug, deleteRoomFiles);
            if (!destroyed) void goto("/admin/rooms");
        } catch (e: any) {
            if (destroyed) return;
            error = e.message || "Failed to delete room";
        }
    }

    function formatDate(dateStr: string): string {
        return new Date(dateStr).toLocaleString(undefined, {
            month: "short",
            day: "numeric",
            hour: "2-digit",
            minute: "2-digit"
        });
    }

    function getWhipUrl(): string {
        if (!room || !room.streamKeyId) return "";
        const streamKeyId = room.streamKeyId;
        const key = keys.find(k => k.id === streamKeyId);
        if (!key) return "";
        return `${window.location.origin}/whip/${key.keyToken}`;
    }

    function getInviteUrl(): string {
        return `${window.location.origin}/room/${slug}`;
    }
</script>

<svelte:head>
    <title>{room?.name || "Room"} | Chromatic</title>
</svelte:head>

<div class="room-manage">
    <header class="page-header">
        <div class="header-left">
            <a href="/admin" class="back-link">&larr; Back to Dashboard</a>
            <div class="title-row">
                <h1>{room?.name || "Loading..."}</h1>
                {#if room}
                    <StatusBadge status={room.status} />
                {/if}
            </div>
        </div>
        {#if room && room.status !== "ended"}
            <div class="header-actions">
                <a href="/room/{slug}?host=1" target="_blank" class="btn btn-primary">
                    Join as host
                </a>
                <a href="/room/{slug}" target="_blank" class="btn btn-secondary">
                    View Room
                </a>
            </div>
        {/if}
    </header>

    {#if isLoading}
        <div class="room-content" aria-busy="true" aria-label="Loading room">
            {#each [0, 1, 2] as i (i)}
                <div class="skeleton skeleton-card"></div>
            {/each}
        </div>
    {:else if room}
        <div class="room-content">
            <!-- Live Controls -->
            {#if room.status === "live"}
                <div class="card live-card">
                    <div class="card-header">
                        <h3><span class="live-dot"></span> Session Live</h3>
                        <button class="btn btn-danger" onclick={() => (confirmEndOpen = true)}>
                            End Session
                        </button>
                    </div>
                    <p class="live-info">
                        Started {room.startedAt ? formatDate(room.startedAt) : "recently"}.
                        Viewers can now join and watch the stream.
                    </p>
                </div>
                <div class="card health-card">
                    <div class="card-header">
                        <h3>Room Health</h3>
                        <a href="/room/{slug}?host=1" target="_blank" class="btn btn-secondary btn-sm">
                            Open Host Diagnostics
                        </a>
                    </div>
                    <div class="health-grid">
                        <div>
                            <span>Target latency</span>
                            <strong>&lt; 250 ms glass-to-glass</strong>
                        </div>
                        <div>
                            <span>Browser buffer</span>
                            <strong>&lt; 100 ms preferred</strong>
                        </div>
                        <div>
                            <span>Network RTT</span>
                            <strong>&lt; 50 ms</strong>
                        </div>
                        <div>
                            <span>Review tools</span>
                            <strong>Use Performance mode if load appears</strong>
                        </div>
                    </div>
                    <p class="health-note">
                        Host diagnostics show live video buffer, post-receive delay, long frames,
                        reconnects, media rebuilds, and review-tool pressure during the session.
                    </p>
                </div>
            {/if}

            <!-- Scheduled session: open ahead of the countdown -->
            {#if room.scheduledAt && !room.openedAt && room.status === "pending"}
                <div class="card scheduled-card">
                    <div class="card-header">
                        <h3>Scheduled Session</h3>
                        <button class="btn btn-primary btn-sm" onclick={handleOpenRoom} disabled={isOpeningRoom}>
                            {isOpeningRoom ? "Opening…" : "Open room now"}
                        </button>
                    </div>
                    <p class="scheduled-info">
                        Starts {formatDate(room.scheduledAt)}. Guests can enter the lobby
                        {room.earlyOpenMinutes ?? 10} minutes before. Opening the room now lets
                        them in immediately{room.waitingRoomEnabled ? " (waiting room approval still applies)" : ""}.
                    </p>
                </div>
            {/if}

            <!-- Waiting Room -->
            {#if room.waitingRoomEnabled && room.status !== "ended"}
                <div class="card waiting-card">
                    <div class="card-header">
                        <h3>Waiting Room ({waitingParticipants.length})</h3>
                        {#if waitingParticipants.length > 0}
                            <button class="btn btn-primary btn-sm" onclick={handleAdmitAll}>
                                Admit All
                            </button>
                        {/if}
                    </div>

                    {#if waitingParticipants.length === 0}
                        <p class="empty-state">No one waiting to join.</p>
                    {:else}
                        <ul class="participant-list">
                            {#each waitingParticipants as participant (participant.id)}
                                <li class="participant-item">
                                    <div class="participant-info">
                                        <span class="participant-name">{participant.name}</span>
                                        <span class="participant-time">
                                            Joined {formatDate(participant.joinedAt)}
                                        </span>
                                    </div>
                                    <div class="participant-actions">
                                        <button
                                            class="btn btn-primary btn-sm"
                                            onclick={() => handleAdmit(participant.id)}
                                        >
                                            Admit
                                        </button>
                                        <button
                                            class="btn btn-secondary btn-sm"
                                            onclick={() => handleDeny(participant.id)}
                                        >
                                            Deny
                                        </button>
                                    </div>
                                </li>
                            {/each}
                        </ul>
                    {/if}
                </div>
            {/if}

            <!-- Uploaded files (review/moderation) -->
            {#if files.length > 0}
                <div class="card files-card">
                    <div class="card-header">
                        <h3>Uploaded Files ({files.length})</h3>
                    </div>
                    <ul class="file-list">
                        {#each files as file (file.id)}
                            <li class="file-item">
                                {#if file.thumbnailUrl}
                                    <img class="file-thumb" src={file.thumbnailUrl} alt="" loading="lazy" />
                                {:else}
                                    <span class="file-thumb file-thumb-placeholder" aria-hidden="true">
                                        {file.mimeType.startsWith("audio/") ? "♪" : "📄"}
                                    </span>
                                {/if}
                                <div class="file-info">
                                    <a class="file-name" href={file.url} target="_blank" rel="noopener">{file.originalName}</a>
                                    <span class="file-meta">
                                        {formatBytes(file.sizeBytes)} · {file.uploaderName} · {formatDate(new Date(file.createdAt).toISOString())}
                                    </span>
                                </div>
                                <button
                                    class="btn btn-secondary btn-sm file-delete"
                                    onclick={() => (fileToDelete = file)}
                                >
                                    Delete
                                </button>
                            </li>
                        {/each}
                    </ul>
                </div>
            {/if}

            <!-- Stream Setup -->
            {#if room.status === "pending"}
                <div class="card stream-card">
                    <h3>Stream Setup</h3>
                    {#if room.streamKeyId}
                        <div class="stream-info">
                            <CopyField label="WHIP URL" value={getWhipUrl()} />
                            <p class="stream-hint">How to set up OBS (30+):</p>
                            <ol class="obs-steps">
                                <li>
                                    <strong>Settings &rarr; Stream:</strong>
                                    Service <strong>WHIP</strong>, Server = the WHIP URL above,
                                    Bearer Token <strong>empty</strong> (the stream key is in the URL).
                                </li>
                                <li>
                                    <strong>Settings &rarr; Output</strong> (Output Mode: Advanced, Streaming tab):
                                    Encoder <strong>x264</strong> or hardware H.264 (NVENC/AMF/QSV),
                                    Rate Control <strong>CBR</strong>,
                                    Bitrate <strong>8000&ndash;12000 Kbps for 48 fps</strong> or <strong>10000&ndash;15000 Kbps for 60 fps</strong>,
                                    Keyframe Interval <strong>1 s</strong> (2 s max &mdash; viewers join on the next keyframe),
                                    Profile <strong>baseline</strong> (required &mdash; Main/High are rejected),
                                    Tune <strong>zerolatency</strong>, B-frames <strong>0</strong>.
                                </li>
                                <li>
                                    <strong>Settings &rarr; Video:</strong>
                                    Use <strong>48/47.952 fps</strong> for clean 24p cadence, or
                                    <strong>60/59.94 fps</strong> for lowest latency. 60 fps can add
                                    uneven cadence to true 24p footage.
                                </li>
                                <li>
                                    <strong>Settings &rarr; Advanced &rarr; Video:</strong>
                                    Color Format <strong>NV12</strong>,
                                    Color Space <strong>sRGB</strong>,
                                    Color Range <strong>Limited</strong>
                                    &mdash; sRGB matches browser rendering on every platform
                                    (Rec. 709 looks washed out on macOS, which applies a
                                    1.96 gamma to 709-tagged video).
                                </li>
                            </ol>
                        </div>
                    {:else}
                        <p class="empty-state">
                            No stream key assigned. Select one below to enable streaming.
                        </p>
                    {/if}
                </div>
            {/if}

            <!-- Invite Link -->
            {#if room.status !== "ended"}
                <div class="card">
                    <h3>Invite Link</h3>
                    <p class="invite-hint">Share this link with participants to join the room.</p>
                    <CopyField value={getInviteUrl()} />
                </div>
            {/if}

            <!-- Room Settings -->
            <form class="card settings-card" onsubmit={handleSave}>
                <h3>Room Settings</h3>

                {#if error}
                    <div class="alert alert-error" transition:fade={{ duration: 150 }}>{error}</div>
                {/if}

                {#if successMessage}
                    <div class="alert alert-success" transition:fade={{ duration: 150 }}>{successMessage}</div>
                {/if}

                <div class="form-group">
                    <label for="name">Room Name</label>
                    <input
                        type="text"
                        id="name"
                        class="input"
                        bind:value={name}
                        maxlength="100"
                        required
                    />
                </div>

                <div class="form-group">
                    <label for="streamKey">Stream Key</label>
                    <select id="streamKey" class="input" bind:value={streamKeyId}>
                        <option value="">None</option>
                        {#each keys as key (key.id)}
                            <option value={key.id}>{key.name}</option>
                        {/each}
                    </select>
                </div>

                <div class="form-group">
                    <label for="password">New Password (leave empty to keep current)</label>
                    <input
                        type="password"
                        id="password"
                        class="input"
                        bind:value={password}
                        placeholder={room.hasPassword ? "Room has password" : "No password set"}
                        minlength="8"
                    />
                </div>

                <div class="form-group checkbox-group">
                    <label class="checkbox-label">
                        <input type="checkbox" bind:checked={waitingRoomEnabled} />
                        <span>Enable Waiting Room</span>
                    </label>
                </div>

                <div class="form-group">
                    <label for="scheduledAt">Scheduled Start (optional)</label>
                    <input
                        type="datetime-local"
                        id="scheduledAt"
                        class="input"
                        bind:value={scheduledAtLocal}
                    />
                    <p class="form-hint">Guests see a countdown lobby until the session starts. Leave blank for an instant room.</p>
                </div>

                {#if scheduledAtLocal}
                    <div class="form-group">
                        <label for="earlyOpenMinutes">Lobby opens (minutes before start)</label>
                        <input
                            type="number"
                            id="earlyOpenMinutes"
                            class="input"
                            bind:value={earlyOpenMinutes}
                            min="0"
                            max="120"
                            placeholder="10"
                        />
                        <p class="form-hint">How early guests may enter the countdown lobby (0 to 120 minutes).</p>
                    </div>
                {/if}

                <div class="form-group">
                    <label for="maxParticipants">Participant Limit</label>
                    <input
                        type="number"
                        id="maxParticipants"
                        class="input"
                        bind:value={maxParticipants}
                        min="1"
                        max="100"
                        placeholder="20"
                    />
                    <p class="form-hint">Maximum participants in this room. Leave blank to use the default (20).</p>
                </div>

                <div class="form-group">
                    <label for="watermarkMode-none">Watermark Mode</label>
                    <div class="radio-group">
                        <label class="radio-label">
                            <input id="watermarkMode-none" type="radio" name="watermarkMode" value="none" bind:group={watermarkMode} />
                            <span>None</span>
                        </label>
                        <label class="radio-label">
                            <input type="radio" name="watermarkMode" value="text" bind:group={watermarkMode} />
                            <span>Text</span>
                        </label>
                        <label class="radio-label">
                            <input type="radio" name="watermarkMode" value="logo" bind:group={watermarkMode} />
                            <span>Logo</span>
                        </label>
                        <label class="radio-label">
                            <input type="radio" name="watermarkMode" value="both" bind:group={watermarkMode} />
                            <span>Both</span>
                        </label>
                    </div>
                </div>

                {#if watermarkMode === "text" || watermarkMode === "both"}
                    <div class="form-group">
                        <label for="watermarkText">Watermark Text</label>
                        <input
                            type="text"
                            id="watermarkText"
                            class="input"
                            bind:value={watermarkText}
                            placeholder={"{{name}} - {{date}}"}
                        />
                        <p class="form-hint">Variables: {"{{name}}"}, {"{{room}}"}, {"{{date}}"}, {"{{time}}"}</p>
                    </div>
                {/if}

                {#if watermarkMode === "logo" || watermarkMode === "both"}
                    <div class="form-group">
                        <label for="logoPosition">Logo Position</label>
                        <select id="logoPosition" class="input" bind:value={watermarkLogoPosition}>
                            <option value="top-left">Top Left</option>
                            <option value="top-right">Top Right</option>
                            <option value="bottom-left">Bottom Left</option>
                            <option value="bottom-right">Bottom Right</option>
                        </select>
                        <p class="form-hint">Logo is configured in Settings. Uses the default watermark logo.</p>
                    </div>
                {/if}

                {#if watermarkMode !== "none"}
                    <div class="form-group">
                        <label for="opacity">Watermark Opacity: {Math.round(watermarkOpacity * 100)}%</label>
                        <input
                            type="range"
                            id="opacity"
                            class="range-input"
                            min="0.1"
                            max="1"
                            step="0.01"
                            bind:value={watermarkOpacity}
                        />
                    </div>

                    <div class="form-group">
                        <span class="form-hint">Preview &mdash; drag the watermark to position it</span>
                        <!-- svelte-ignore a11y_no_static_element_interactions -->
                        <div
                            class="watermark-preview"
                            class:grabbable={isOverWatermark && !isDraggingWatermark}
                            class:grabbing={isDraggingWatermark}
                            bind:this={previewEl}
                            onpointerenter={() => (isPreviewHovered = true)}
                            onpointerleave={() => {
                                isPreviewHovered = false;
                                isOverWatermark = false;
                            }}
                            onpointerdown={handlePreviewPointerDown}
                            onpointermove={handlePreviewPointerMove}
                            onpointerup={handlePreviewPointerUp}
                            onpointercancel={handlePreviewPointerUp}
                        >
                            {#key `${watermarkMode}-${watermarkLogoPosition}-${config?.defaultWatermarkLogoUrl ?? ""}`}
                                <WatermarkOverlay
                                    mode={watermarkMode}
                                    text={watermarkText}
                                    logoUrl={config?.defaultWatermarkLogoUrl}
                                    logoPosition={watermarkLogoPosition}
                                    opacity={watermarkOpacity}
                                    posX={watermarkPosX}
                                    posY={watermarkPosY}
                                    scale={watermarkScale}
                                    onBoundsChange={(b) => (watermarkBounds = b)}
                                    participantName="Jane Colorist"
                                    roomName={name}
                                />
                            {/key}
                            {#if watermarkBounds && (isPreviewHovered || isDraggingWatermark)}
                                <div
                                    class="watermark-outline"
                                    style="left: {watermarkBounds.x - 4}px; top: {watermarkBounds.y - 4}px; width: {watermarkBounds.width + 8}px; height: {watermarkBounds.height + 8}px"
                                ></div>
                            {/if}
                        </div>
                    </div>

                    <div class="form-group">
                        <label for="wmSize">Size: {watermarkScale.toFixed(2)}x</label>
                        <input
                            type="range"
                            id="wmSize"
                            class="range-input"
                            min="0.25"
                            max="3"
                            step="0.05"
                            bind:value={watermarkScale}
                        />
                    </div>
                {/if}

                <div class="form-actions">
                    <button type="submit" class="btn btn-primary" disabled={isSaving}>
                        {#if isSaving}
                            <span class="btn-spinner" aria-hidden="true"></span>
                            Saving...
                        {:else}
                            Save Changes
                        {/if}
                    </button>
                </div>
            </form>

            <!-- Danger Zone -->
            {#if room.status !== "live"}
                <div class="card danger-card">
                    <h3>Danger Zone</h3>
                    <p>Permanently delete this room and all its data.</p>
                    <label class="delete-files-option">
                        <input type="checkbox" bind:checked={deleteRoomFiles} />
                        Also delete uploaded files and images from disk
                    </label>
                    <button class="btn btn-danger" onclick={() => (confirmDeleteOpen = true)}>
                        Delete Room
                    </button>
                </div>
            {/if}
        </div>
    {:else}
        <div class="error-card card">
            <h2>Room Not Found</h2>
            <p>The room you're looking for doesn't exist.</p>
            <a href="/admin" class="btn btn-primary">Back to Dashboard</a>
        </div>
    {/if}
</div>

<ConfirmDialog
    open={confirmEndOpen}
    title="End this session?"
    body="All participants will be disconnected and the room will be marked as ended. This cannot be undone."
    confirmLabel="End Session"
    danger
    onConfirm={handleEndSession}
    onCancel={() => (confirmEndOpen = false)}
/>

<ConfirmDialog
    open={confirmDeleteOpen}
    title="Delete this room?"
    body={deleteRoomFiles
        ? "The room, all its data, and every uploaded file will be permanently deleted. This cannot be undone."
        : "The room and all its data will be permanently deleted (uploaded files stay on disk). This cannot be undone."}
    confirmLabel="Delete Room"
    danger
    onConfirm={handleDelete}
    onCancel={() => (confirmDeleteOpen = false)}
/>

<ConfirmDialog
    open={fileToDelete !== null}
    title="Delete this file?"
    body={`"${fileToDelete?.originalName ?? ""}" will be removed from the room, including any chat messages that shared it. This cannot be undone.`}
    confirmLabel="Delete File"
    danger
    onConfirm={handleDeleteFile}
    onCancel={() => (fileToDelete = null)}
/>

<style>
    .room-manage {
        max-width: 800px;
        margin: 0 auto;
    }

    .header-left {
        display: flex;
        flex-direction: column;
        gap: var(--space-xs);
    }

    .title-row {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
    }

    .back-link {
        font-size: var(--text-body);
        color: var(--color-text-muted);
    }

    .room-content {
        display: flex;
        flex-direction: column;
        gap: var(--space-lg);
    }

    .skeleton-card {
        min-height: 120px;
    }

    .card {
        padding: var(--space-lg);
    }

    .card h3 {
        font-size: var(--text-card-title);
        font-weight: 600;
        margin: 0 0 var(--space-md);
    }

    .card-header {
        display: flex;
        justify-content: space-between;
        align-items: center;
        margin-bottom: var(--space-md);
    }

    .card-header h3 {
        margin: 0;
        display: flex;
        align-items: center;
        gap: var(--space-sm);
    }

    .live-card {
        border: 1px solid var(--color-success);
        background: var(--color-success-bg);
    }

    .live-dot {
        display: inline-block;
        width: 8px;
        height: 8px;
        background: var(--color-success);
        border-radius: 50%;
        animation: pulse 1.5s infinite;
    }

    @keyframes pulse {
        0%, 100% { opacity: 1; }
        50% { opacity: 0.5; }
    }

    .live-info {
        color: var(--color-text-muted);
        font-size: var(--text-body);
        margin: 0;
    }

    .health-card {
        border: 1px solid rgba(72, 182, 166, 0.35);
    }

    .health-grid {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
        gap: var(--space-sm);
    }

    .health-grid div {
        display: flex;
        flex-direction: column;
        gap: 3px;
        padding: var(--space-sm);
        border-radius: var(--radius-md);
        background: var(--color-surface-elevated);
    }

    .health-grid span {
        font-size: 0.72rem;
        text-transform: uppercase;
        letter-spacing: 0.06em;
        color: var(--color-text-subtle);
    }

    .health-grid strong {
        font-size: 0.875rem;
        font-weight: 600;
    }

    .health-note {
        margin: var(--space-md) 0 0;
        color: var(--color-text-muted);
        font-size: var(--text-meta);
    }

    .scheduled-card {
        border: 1px solid var(--color-primary);
    }

    .scheduled-info {
        color: var(--color-text-muted);
        font-size: var(--text-body);
        margin: 0;
    }

    .waiting-card .participant-list {
        list-style: none;
    }

    .participant-actions {
        display: flex;
        gap: var(--space-xs);
    }

    .participant-item {
        display: flex;
        justify-content: space-between;
        align-items: center;
        padding: var(--space-sm) 0;
        border-bottom: 1px solid var(--color-border-subtle);
    }

    .participant-item:last-child {
        border-bottom: none;
    }

    .participant-name {
        font-weight: 500;
    }

    .participant-time {
        font-size: var(--text-meta);
        color: var(--color-text-subtle);
        display: block;
    }

    .invite-hint {
        font-size: var(--text-body);
        color: var(--color-text-muted);
        margin-bottom: var(--space-md);
    }

    .stream-hint {
        font-size: var(--text-meta);
        color: var(--color-text-subtle);
        margin: var(--space-md) 0 0;
    }

    .obs-steps {
        list-style: decimal;
        padding-left: var(--space-lg);
        margin: var(--space-sm) 0 0;
        display: grid;
        gap: var(--space-sm);
        font-size: var(--text-meta);
        color: var(--color-text-muted);
    }

    .obs-steps strong {
        color: var(--color-text);
        font-weight: 600;
    }

    .form-group {
        margin-bottom: var(--space-lg);
    }

    .form-group label {
        display: block;
        font-size: var(--text-body);
        font-weight: 500;
        margin-bottom: var(--space-sm);
        color: var(--color-text-muted);
    }

    .checkbox-group {
        display: flex;
        align-items: center;
    }

    .checkbox-label,
    .radio-label {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        cursor: pointer;
        font-weight: normal;
    }

    .radio-group {
        display: flex;
        gap: var(--space-lg);
        flex-wrap: wrap;
    }

    .watermark-preview {
        position: relative;
        width: 100%;
        aspect-ratio: 16 / 9;
        background: #000;
        border: 1px solid var(--color-border);
        border-radius: var(--radius-md);
        overflow: hidden;
        margin-top: var(--space-xs);
        touch-action: none;
    }

    .watermark-preview.grabbable {
        cursor: grab;
    }

    .watermark-preview.grabbing {
        cursor: grabbing;
    }

    .watermark-outline {
        position: absolute;
        border: 1px dashed rgba(255, 255, 255, 0.55);
        border-radius: 2px;
        pointer-events: none;
        z-index: 11;
    }

    .form-actions {
        display: flex;
        justify-content: flex-end;
        gap: var(--space-md);
        margin-top: var(--space-lg);
    }

    .empty-state {
        color: var(--color-text-muted);
        font-size: var(--text-body);
        text-align: center;
        padding: var(--space-lg) 0;
    }

    .danger-card {
        border: 1px solid var(--color-error);
    }

    .delete-files-option {
        display: flex;
        align-items: center;
        gap: var(--space-xs);
        margin-bottom: var(--space-md);
        font-size: 0.875rem;
        color: var(--color-text-muted);
        cursor: pointer;
    }

    .file-list {
        list-style: none;
        margin: 0;
        padding: 0;
        display: flex;
        flex-direction: column;
        gap: var(--space-xs);
    }

    .file-item {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        padding: var(--space-xs) 0;
        border-bottom: 1px solid var(--color-border-subtle);
    }
    .file-item:last-child {
        border-bottom: none;
    }

    .file-thumb {
        width: 40px;
        height: 40px;
        object-fit: cover;
        border-radius: var(--radius-sm);
        flex-shrink: 0;
        background: var(--color-surface);
    }
    .file-thumb-placeholder {
        display: flex;
        align-items: center;
        justify-content: center;
        font-size: 1.1rem;
        color: var(--color-text-subtle);
    }

    .file-info {
        flex: 1;
        min-width: 0;
        display: flex;
        flex-direction: column;
    }
    .file-name {
        color: var(--color-text);
        text-decoration: none;
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
    }
    .file-name:hover {
        text-decoration: underline;
    }
    .file-meta {
        font-size: 0.75rem;
        color: var(--color-text-subtle);
    }

    .file-delete {
        flex-shrink: 0;
    }

    .danger-card h3 {
        color: var(--color-error);
    }

    .danger-card p {
        color: var(--color-text-muted);
        font-size: var(--text-body);
        margin-bottom: var(--space-md);
    }

    .error-card {
        text-align: center;
    }

    .error-card h2 {
        margin-bottom: var(--space-md);
    }

    .error-card p {
        margin-bottom: var(--space-lg);
        color: var(--color-text-muted);
    }
</style>
