<script lang="ts">
    import { onMount, onDestroy } from "svelte";
    import { page } from "$app/stores";
    import { rooms, streamKeys, type Room, type StreamKey } from "$lib/api/client";

    const slug = $page.params.slug!;

    let room = $state<Room | null>(null);
    let keys = $state<StreamKey[]>([]);
    let waitingParticipants = $state<{ id: string; name: string; joinedAt: string }[]>([]);
    let isLoading = $state(true);
    let isSaving = $state(false);
    let error = $state("");
    let successMessage = $state("");

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
            const [roomData, keysData] = await Promise.all([
                rooms.get(slug),
                streamKeys.list()
            ]);

            room = roomData;
            keys = keysData;

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
        if (!confirm("Are you sure you want to end this session? This cannot be undone.")) {
            return;
        }

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
        if (!confirm("Are you sure you want to delete this room? This cannot be undone.")) {
            return;
        }

        try {
            await rooms.delete(slug);
            window.location.href = "/admin";
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

    function copyToClipboard(text: string) {
        navigator.clipboard.writeText(text);
        successMessage = "Copied to clipboard!";
        setTimeout(() => successMessage = "", 2000);
    }
</script>

<svelte:head>
    <title>{room?.name || "Room"} | Chromatic</title>
</svelte:head>

<div class="room-manage">
    <header class="page-header">
        <div class="header-left">
            <a href="/admin" class="back-link">&larr; Back to Dashboard</a>
            <h1>{room?.name || "Loading..."}</h1>
            {#if room}
                <span class="status-badge {room.status}">{room.status}</span>
            {/if}
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
        <div class="loading">
            <div class="waiting-spinner"></div>
        </div>
    {:else if room}
        <div class="room-content">
            <!-- Live Controls -->
            {#if room.status === "live"}
                <div class="card live-card">
                    <div class="card-header">
                        <h3><span class="live-dot"></span> Session Live</h3>
                        <button class="btn btn-danger" onclick={handleEndSession}>
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
                            <div class="stream-field">
                                <span class="field-label">WHIP URL</span>
                                <div class="copy-field">
                                    <code>{getWhipUrl()}</code>
                                    <button
                                        class="btn btn-ghost btn-sm"
                                        onclick={() => copyToClipboard(getWhipUrl())}
                                    >
                                        Copy
                                    </button>
                                </div>
                            </div>
                            <p class="stream-hint">
                                In OBS: Settings &rarr; Stream &rarr; Service: WHIP &rarr;
                                paste the URL above into <strong>Server</strong> and leave <strong>Bearer Token empty</strong>.
                            </p>
                        </div>
                    {:else}
                        <p class="empty-state">
                            No stream key assigned. Select one below to enable streaming.
                        </p>
                    {/if}
                </div>
            {/if}

            <!-- Room Settings -->
            <form class="card settings-card" onsubmit={handleSave}>
                <h3>Room Settings</h3>

                {#if error}
                    <div class="error-message">{error}</div>
                {/if}

                {#if successMessage}
                    <div class="success-message">{successMessage}</div>
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
                        <p class="hint">Variables: {"{{name}}"}, {"{{room}}"}, {"{{date}}"}, {"{{time}}"}</p>
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
                        <p class="hint">Logo is configured in Settings. Uses the default watermark logo.</p>
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
                            step="0.1"
                            bind:value={watermarkOpacity}
                        />
                    </div>
                {/if}

                <div class="form-actions">
                    <button type="submit" class="btn btn-primary" disabled={isSaving}>
                        {isSaving ? "Saving..." : "Save Changes"}
                    </button>
                </div>
            </form>

            <!-- Danger Zone -->
            {#if room.status !== "live"}
                <div class="card danger-card">
                    <h3>Danger Zone</h3>
                    <p>Permanently delete this room and all its data.</p>
                    <button class="btn btn-danger" onclick={handleDelete}>
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

<style>
    .room-manage {
        max-width: 800px;
        margin: 0 auto;
    }

    .page-header {
        display: flex;
        justify-content: space-between;
        align-items: flex-start;
        margin-bottom: var(--space-xl);
    }

    .header-left {
        display: flex;
        flex-direction: column;
        gap: var(--space-xs);
    }

    .back-link {
        font-size: 0.875rem;
        color: var(--color-text-muted);
    }

    .page-header h1 {
        margin: 0;
        display: inline;
    }

    .status-badge {
        display: inline-block;
        font-size: 0.75rem;
        padding: var(--space-xs) var(--space-sm);
        border-radius: var(--radius-full);
        margin-left: var(--space-sm);
    }

    .status-badge.live {
        background: rgba(34, 197, 94, 0.2);
        color: var(--color-success);
    }

    .status-badge.pending {
        background: rgba(245, 158, 11, 0.2);
        color: var(--color-warning);
    }

    .status-badge.ended {
        background: rgba(107, 114, 128, 0.2);
        color: var(--color-text-muted);
    }

    .room-content {
        display: flex;
        flex-direction: column;
        gap: var(--space-lg);
    }

    .card {
        padding: var(--space-lg);
    }

    .card h3 {
        font-size: 1rem;
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
        background: rgba(34, 197, 94, 0.05);
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
        font-size: 0.875rem;
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
        font-size: 0.75rem;
        color: var(--color-text-subtle);
        display: block;
    }

    .stream-field {
        margin-bottom: var(--space-md);
    }

    .stream-field .field-label {
        display: block;
        font-size: 0.875rem;
        color: var(--color-text-muted);
        margin-bottom: var(--space-xs);
    }

    .copy-field {
        display: flex;
        gap: var(--space-sm);
        align-items: center;
    }

    .copy-field code {
        flex: 1;
        padding: var(--space-sm);
        background: var(--color-surface-elevated);
        border-radius: var(--radius-md);
        font-size: 0.75rem;
        overflow-x: auto;
    }

    .stream-hint {
        font-size: 0.75rem;
        color: var(--color-text-subtle);
        margin: 0;
    }

    .form-group {
        margin-bottom: var(--space-lg);
    }

    .form-group label {
        display: block;
        font-size: 0.875rem;
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
    }

    .form-actions {
        display: flex;
        justify-content: flex-end;
        gap: var(--space-md);
        margin-top: var(--space-lg);
    }

    .error-message {
        padding: var(--space-md);
        background: rgba(239, 68, 68, 0.1);
        border: 1px solid var(--color-error);
        border-radius: var(--radius-md);
        color: var(--color-error);
        font-size: 0.875rem;
        margin-bottom: var(--space-lg);
    }

    .success-message {
        padding: var(--space-md);
        background: rgba(34, 197, 94, 0.1);
        border: 1px solid var(--color-success);
        border-radius: var(--radius-md);
        color: var(--color-success);
        font-size: 0.875rem;
        margin-bottom: var(--space-lg);
    }

    .empty-state {
        color: var(--color-text-muted);
        font-size: 0.875rem;
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
        font-size: 0.875rem;
        margin-bottom: var(--space-md);
    }

    .btn-danger {
        background: var(--color-error);
        color: white;
    }

    .btn-danger:hover {
        background: #dc2626;
    }

    .btn-secondary {
        background: var(--color-surface-hover);
        color: var(--color-text);
        border: 1px solid var(--color-border);
    }

    .btn-secondary:hover {
        background: var(--color-border);
    }

    .btn-sm {
        padding: var(--space-xs) var(--space-sm);
        font-size: 0.75rem;
    }

    .loading {
        display: flex;
        justify-content: center;
        padding: var(--space-2xl);
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

    .hint {
        font-size: 0.75rem;
        color: var(--color-text-subtle);
        margin-top: var(--space-xs);
    }

    .range-input {
        width: 100%;
        height: 6px;
        background: var(--color-surface-elevated);
        border-radius: var(--radius-full);
        appearance: none;
        cursor: pointer;
    }

    .range-input::-webkit-slider-thumb {
        appearance: none;
        width: 16px;
        height: 16px;
        background: var(--color-primary);
        border-radius: 50%;
        cursor: pointer;
    }

    .range-input::-moz-range-thumb {
        width: 16px;
        height: 16px;
        background: var(--color-primary);
        border-radius: 50%;
        cursor: pointer;
        border: none;
    }
</style>
