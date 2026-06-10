<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { fade } from "svelte/transition";
    import { page } from "$app/stores";
    import { rooms, streamKeys, appConfig, type Room, type StreamKey, type AppConfig } from "$lib/api/client";
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

    // Form fields (populated from room)
    let name = $state("");
    let password = $state("");
    let waitingRoomEnabled = $state(false);
    let streamKeyId = $state<string>("");
    let watermarkMode = $state<"none" | "text" | "logo" | "both">("text");
    let watermarkText = $state("");
    let watermarkLogoPosition = $state<"top-left" | "top-right" | "bottom-left" | "bottom-right">("bottom-right");
    let watermarkOpacity = $state(0.3);

    // Polling interval for waiting room
    let pollInterval: ReturnType<typeof setInterval>;

    onMount(async () => {
        try {
            const [roomData, keysData, configData] = await Promise.all([
                rooms.get(slug),
                streamKeys.list(),
                appConfig.get().catch(() => null)
            ]);

            room = roomData;
            keys = keysData;
            config = configData;

            // Populate form fields
            name = room.name;
            waitingRoomEnabled = room.waitingRoomEnabled;
            streamKeyId = room.streamKeyId || "";
            watermarkMode = (room.watermarkMode as "none" | "text" | "logo" | "both") || "text";
            watermarkText = room.watermarkText || "{{ name }} - {{ date }}";
            watermarkLogoPosition = (room.watermarkLogoPosition as typeof watermarkLogoPosition) || "bottom-right";
            watermarkOpacity = room.watermarkOpacity ?? 0.3;

            // Start polling waiting room if enabled
            if (room.waitingRoomEnabled && room.status !== "ended") {
                await loadWaitingRoom();
                pollInterval = setInterval(loadWaitingRoom, 5000);
            }
        } catch (e) {
            error = "Failed to load room";
            console.error(e);
        } finally {
            isLoading = false;
        }
    });

    onDestroy(() => {
        if (pollInterval) {
            clearInterval(pollInterval);
        }
    });

    async function loadWaitingRoom() {
        try {
            waitingParticipants = (await rooms.listWaiting(slug)) ?? [];
        } catch (e) {
            console.error("Failed to load waiting room", e);
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

            room = await rooms.update(slug, updateData);
            successMessage = "Room updated successfully";
            password = ""; // Clear password field after save
        } catch (e: any) {
            error = e.message || "Failed to update room";
        } finally {
            isSaving = false;
        }
    }

    async function handleAdmit(participantId: string) {
        try {
            await rooms.admit(slug, participantId);
            waitingParticipants = waitingParticipants.filter(p => p.id !== participantId);
        } catch (e) {
            console.error("Failed to admit participant", e);
        }
    }

    async function handleAdmitAll() {
        try {
            await rooms.admitAll(slug);
            waitingParticipants = [];
        } catch (e) {
            console.error("Failed to admit all", e);
        }
    }

    async function handleEndSession() {
        confirmEndOpen = false;
        try {
            await rooms.end(slug);
            if (room) {
                room.status = "ended";
            }
        } catch (e) {
            console.error("Failed to end session", e);
        }
    }

    async function handleDelete() {
        confirmDeleteOpen = false;
        try {
            await rooms.delete(slug);
            window.location.href = "/admin/rooms";
        } catch (e: any) {
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
                                    <button
                                        class="btn btn-primary btn-sm"
                                        onclick={() => handleAdmit(participant.id)}
                                    >
                                        Admit
                                    </button>
                                </li>
                            {/each}
                        </ul>
                    {/if}
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
                                    Bitrate <strong>8000&ndash;10000 Kbps</strong>,
                                    Keyframe Interval <strong>1 s</strong> (2 s max &mdash; viewers join on the next keyframe),
                                    Profile <strong>baseline</strong> (required &mdash; Main/High are rejected),
                                    Tune <strong>zerolatency</strong>, B-frames <strong>0</strong>.
                                </li>
                                <li>
                                    <strong>Settings &rarr; Video:</strong>
                                    Output 1920x1080 at 24 or 30 fps to match your footage.
                                </li>
                                <li>
                                    <strong>Settings &rarr; Advanced &rarr; Video:</strong>
                                    Color Format <strong>NV12</strong>,
                                    Color Space <strong>Rec. 709</strong>,
                                    Color Range <strong>Limited</strong>
                                    &mdash; matches browser rendering for accurate review color.
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
                        <span class="form-hint">Preview</span>
                        <div class="watermark-preview">
                            {#key `${watermarkMode}-${watermarkLogoPosition}-${config?.defaultWatermarkLogoUrl ?? ""}`}
                                <WatermarkOverlay
                                    mode={watermarkMode}
                                    text={watermarkText}
                                    logoUrl={config?.defaultWatermarkLogoUrl}
                                    logoPosition={watermarkLogoPosition}
                                    opacity={watermarkOpacity}
                                    participantName="Jane Colorist"
                                    roomName={name}
                                />
                            {/key}
                        </div>
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
    body="The room and all its data will be permanently deleted. This cannot be undone."
    confirmLabel="Delete Room"
    danger
    onConfirm={handleDelete}
    onCancel={() => (confirmDeleteOpen = false)}
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

    .waiting-card .participant-list {
        list-style: none;
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
