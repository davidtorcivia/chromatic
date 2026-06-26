<script lang="ts">
    import { goto } from "$app/navigation";
    import { rooms, streamKeys, appConfig, type StreamKey, type AppConfig } from "$lib/api/client";
    import { onMount, onDestroy } from "svelte";
    import WatermarkOverlay from "$lib/components/WatermarkOverlay.svelte";

    let keys = $state<StreamKey[]>([]);
    let config = $state<AppConfig | null>(null);
    let isLoading = $state(true);
    let isSaving = $state(false);
    let error = $state("");

    // Form fields
    let name = $state("");
    let slug = $state("");
    let password = $state("");
    let waitingRoomEnabled = $state(false);
    // Scheduling: local datetime-local string ("" = instant room) + lobby window
    let scheduledAtLocal = $state("");
    let earlyOpenMinutes = $state(10);
    let streamKeyId = $state<string>("");
    let watermarkMode = $state<"none" | "text" | "logo" | "both">("text");
    let watermarkText = $state("{{ name }} - {{ date }}");
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

    onMount(async () => {
        try {
            const [keysData, configData] = await Promise.all([
                streamKeys.list(),
                appConfig.get().catch(() => null),
            ]);
            if (destroyed) return;
            keys = keysData ?? [];
            config = configData;
            if (keys.length > 0) {
                streamKeyId = keys[0].id;
            }
        } catch (e) {
            if (destroyed) return;
            console.error("Failed to load stream keys", e);
        } finally {
            if (!destroyed) isLoading = false;
        }
    });

    onDestroy(() => {
        destroyed = true;
    });

    function getErrorMessage(e: unknown, fallback: string) {
        return e instanceof Error ? e.message : fallback;
    }

    function generateSlug(text: string): string {
        return text
            .toLowerCase()
            .replace(/[^a-z0-9\s-]/g, "")
            .replace(/\s+/g, "-")
            .replace(/-+/g, "-")
            .slice(0, 64);
    }

    function handleNameChange(e: Event) {
        const target = e.target as HTMLInputElement;
        name = target.value;
        // Auto-generate slug from name if slug is empty or was auto-generated
        if (!slug || slug === generateSlug(name.slice(0, -1))) {
            slug = generateSlug(name);
        }
    }

    async function handleSubmit(e: SubmitEvent) {
        e.preventDefault();
        if (!name.trim() || !slug.trim()) return;

        isSaving = true;
        error = "";

        try {
            const roomData: Record<string, unknown> = {
                name: name.trim(),
                slug: slug.trim(),
                waitingRoomEnabled,
                watermarkMode,
            };

            if (scheduledAtLocal) {
                const scheduled = new Date(scheduledAtLocal);
                if (!Number.isNaN(scheduled.getTime())) {
                    roomData.scheduledAt = scheduled.toISOString();
                    roomData.earlyOpenMinutes =
                        typeof earlyOpenMinutes === "number" && !Number.isNaN(earlyOpenMinutes)
                            ? Math.min(120, Math.max(0, earlyOpenMinutes))
                            : 10;
                }
            }

            if (password) {
                roomData.password = password;
            }

            if (streamKeyId) {
                roomData.streamKeyId = streamKeyId;
            }

            if (watermarkMode === "text" || watermarkMode === "both") {
                roomData.watermarkText = watermarkText;
            }

            if (watermarkMode === "logo" || watermarkMode === "both") {
                roomData.watermarkLogoPosition = watermarkLogoPosition;
            }

            if (watermarkMode !== "none") {
                roomData.watermarkOpacity = watermarkOpacity;
                roomData.watermarkScale = watermarkScale;
                if (watermarkPosX != null && watermarkPosY != null) {
                    roomData.watermarkPosX = watermarkPosX;
                    roomData.watermarkPosY = watermarkPosY;
                }
            }

            if (typeof maxParticipants === "number" && !Number.isNaN(maxParticipants)) {
                roomData.maxParticipants = maxParticipants;
            }

            const created = await rooms.create(roomData);
            if (destroyed) return;
            void goto(`/admin/rooms/${created.slug || slug}`);
        } catch (e) {
            if (destroyed) return;
            error = getErrorMessage(e, "Failed to create room");
        } finally {
            if (!destroyed) isSaving = false;
        }
    }
</script>

<svelte:head>
    <title>Create Room | Chromatic</title>
</svelte:head>

<div class="create-room">
    <header class="page-header">
        <div class="header-left">
            <a href="/admin" class="back-link">&larr; Back</a>
            <h1>Create Room</h1>
        </div>
    </header>

    {#if isLoading}
        <div class="skeleton skeleton-form" aria-busy="true" aria-label="Loading form"></div>
    {:else}
        <form class="room-form card" onsubmit={handleSubmit}>
            {#if error}
                <div class="alert alert-error">{error}</div>
            {/if}

            <div class="form-section">
                <h3>Basic Info</h3>

                <div class="form-group">
                    <label for="name">Room Name</label>
                    <input
                        type="text"
                        id="name"
                        class="input"
                        value={name}
                        oninput={handleNameChange}
                        placeholder="My Color Session"
                        maxlength="100"
                        required
                    />
                </div>

                <div class="form-group">
                    <label for="slug">URL Slug</label>
                    <div class="slug-preview">
                        <span class="slug-prefix">/room/</span>
                        <input
                            type="text"
                            id="slug"
                            class="input slug-input"
                            bind:value={slug}
                            placeholder="my-color-session"
                            pattern={"[a-z0-9-]{3,64}"}
                            minlength="3"
                            maxlength="64"
                            required
                            title="3-64 characters: lowercase letters, numbers, and hyphens only"
                        />
                    </div>
                    <span class="form-hint"
                        >3-64 characters: lowercase letters, numbers, and hyphens only</span
                    >
                </div>
            </div>

            <div class="form-section">
                <h3>Schedule</h3>

                <div class="form-group">
                    <label for="scheduledAt">Scheduled Start (optional)</label>
                    <input
                        type="datetime-local"
                        id="scheduledAt"
                        class="input"
                        bind:value={scheduledAtLocal}
                    />
                    <span class="form-hint"
                        >Guests see a countdown lobby until the session starts.
                        Leave blank for an instant room.</span
                    >
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
                        <span class="form-hint"
                            >How early guests may enter the countdown lobby (0&ndash;120 minutes).</span
                        >
                    </div>
                {/if}
            </div>

            <div class="form-section">
                <h3>Stream Settings</h3>

                <div class="form-group">
                    <label for="streamKey">Stream Key</label>
                    {#if keys.length === 0}
                        <p class="empty-note">
                            No stream keys available.
                            <a href="/admin/stream-keys">Create one first</a>.
                        </p>
                    {:else}
                        <select
                            id="streamKey"
                            class="input"
                            bind:value={streamKeyId}
                        >
                            <option value="">None (can set later)</option>
                            {#each keys as key (key.id)}
                                <option value={key.id}>{key.name}</option>
                            {/each}
                        </select>
                    {/if}
                </div>
            </div>

            <div class="form-section">
                <h3>Security</h3>

                <div class="form-group">
                    <label for="password">Room Password (Optional)</label>
                    <input
                        type="password"
                        id="password"
                        class="input"
                        bind:value={password}
                        placeholder="Leave empty for no password"
                        minlength="8"
                    />
                </div>

                <div class="form-group checkbox-group">
                    <label class="checkbox-label">
                        <input
                            type="checkbox"
                            bind:checked={waitingRoomEnabled}
                        />
                        <span>Enable Waiting Room</span>
                    </label>
                    <span class="form-hint"
                        >Require manual approval before viewers can join</span
                    >
                </div>

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
                    <span class="form-hint"
                        >Maximum participants in this room. Leave blank to use
                        the default (20).</span
                    >
                </div>
            </div>

            <div class="form-section">
                <h3>Watermark</h3>

                <div class="form-group">
                    <label for="watermarkMode-none">Watermark Mode</label>
                    <div class="radio-group">
                        <label class="radio-label">
                            <input
                                type="radio"
                                id="watermarkMode-none"
                                name="watermarkMode"
                                value="none"
                                bind:group={watermarkMode}
                            />
                            <span>None</span>
                        </label>
                        <label class="radio-label">
                            <input
                                type="radio"
                                name="watermarkMode"
                                value="text"
                                bind:group={watermarkMode}
                            />
                            <span>Text</span>
                        </label>
                        <label class="radio-label">
                            <input
                                type="radio"
                                name="watermarkMode"
                                value="logo"
                                bind:group={watermarkMode}
                            />
                            <span>Logo</span>
                        </label>
                        <label class="radio-label">
                            <input
                                type="radio"
                                name="watermarkMode"
                                value="both"
                                bind:group={watermarkMode}
                            />
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
                            placeholder={"{{ name }} - {{ date }}"}
                        />
                        <span class="form-hint">
                            Variables: {"{{ name }}"}, {"{{ room }}"}, {"{{ date }}"}, {"{{ time }}"}
                        </span>
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
                        <span class="form-hint">
                            Logo is configured in <a href="/admin/settings">Settings</a>
                        </span>
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
                                    roomName={name || "My Color Session"}
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
            </div>

            <div class="form-actions">
                <a href="/admin" class="btn btn-secondary">Cancel</a>
                <button
                    type="submit"
                    class="btn btn-primary"
                    disabled={isSaving}
                >
                    {#if isSaving}
                        <span class="btn-spinner" aria-hidden="true"></span>
                        Creating...
                    {:else}
                        Create Room
                    {/if}
                </button>
            </div>
        </form>
    {/if}
</div>

<style>
    .create-room {
        max-width: 600px;
        margin: 0 auto;
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

    .room-form {
        padding: var(--space-xl);
    }

    .form-section {
        margin-bottom: var(--space-xl);
        padding-bottom: var(--space-xl);
        border-bottom: 1px solid var(--color-border-subtle);
    }

    .form-section:last-of-type {
        border-bottom: none;
        margin-bottom: var(--space-lg);
    }

    .form-section h3 {
        font-size: 1rem;
        font-weight: 600;
        margin-bottom: var(--space-lg);
        color: var(--color-text);
    }

    .form-group {
        margin-bottom: var(--space-lg);
    }

    .form-group:last-child {
        margin-bottom: 0;
    }

    .form-group label {
        display: block;
        font-size: 0.875rem;
        font-weight: 500;
        margin-bottom: var(--space-sm);
        color: var(--color-text-muted);
    }

    .slug-preview {
        display: flex;
        align-items: center;
    }

    .slug-prefix {
        font-size: 0.875rem;
        color: var(--color-text-muted);
        padding-right: var(--space-xs);
    }

    .slug-input {
        flex: 1;
    }

    .checkbox-group,
    .radio-group {
        display: flex;
        flex-direction: column;
        gap: var(--space-sm);
    }

    .checkbox-label,
    .radio-label {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        cursor: pointer;
        font-weight: normal;
    }

    .checkbox-label input,
    .radio-label input {
        margin: 0;
    }

    .radio-group {
        flex-direction: row;
        gap: var(--space-lg);
    }

    .form-actions {
        display: flex;
        justify-content: flex-end;
        gap: var(--space-md);
    }

    .empty-note {
        color: var(--color-text-muted);
        font-size: 0.875rem;
    }

    .empty-note a {
        color: var(--color-primary);
    }

    .skeleton-form {
        min-height: 400px;
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
</style>
