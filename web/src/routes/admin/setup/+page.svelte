<script lang="ts">
    import { onMount } from "svelte";
    import { goto } from "$app/navigation";
    import {
        appConfig,
        rooms,
        streamKeys,
        type AppConfig,
        type Room,
        type StreamKey,
        type TURNTestResponse,
    } from "$lib/api/client";

    const steps = [
        {
            id: "preflight",
            title: "Preflight",
            description: "Verify environment and security",
        },
        {
            id: "turn",
            title: "Connectivity",
            description: "TURN and network checks",
        },
        {
            id: "branding",
            title: "Branding",
            description: "Watermark defaults",
        },
        {
            id: "stream",
            title: "Stream setup",
            description: "Create stream key + OBS",
        },
        {
            id: "room",
            title: "First room",
            description: "Create and test a room",
        },
    ] as const;

    const setupCompleteKey = "chromatic-setup-complete";
    const setupDismissedKey = "chromatic-setup-dismissed";

    let currentStep = $state(0);
    let isLoading = $state(true);
    let loadError = $state("");

    let config = $state<AppConfig | null>(null);
    let baseUrl = $state("");
    let healthStatus = $state<"checking" | "ok" | "error">("checking");
    let healthMessage = $state("");

    let publicUrlStatus = $state<{
        tone: "good" | "warn" | "bad";
        label: string;
        detail: string;
    }>({
        tone: "warn",
        label: "Review",
        detail: "",
    });

    let preflightChecks = $state({
        dns: false,
        ports: false,
        ssl: false,
        env: false,
        origins: false,
        localDev: false,
    });

    let turnUrl = $state("");
    let turnUsername = $state("");
    let turnCredential = $state("");
    let turnTestResults = $state<TURNTestResponse | null>(null);
    let isSavingTurn = $state(false);
    let isTestingTurn = $state(false);
    let turnError = $state("");
    let turnSuccess = $state("");
    let turnConfirmed = $state(false);

    let watermarkText = $state("");
    let isSavingBranding = $state(false);
    let isUploadingLogo = $state(false);
    let brandingError = $state("");
    let brandingSuccess = $state("");
    let brandingConfirmed = $state(false);

    let keys = $state<StreamKey[]>([]);
    let selectedKeyId = $state("");
    let newKeyName = $state("");
    let isCreatingKey = $state(false);
    let streamError = $state("");
    let copiedKeyId = $state<string | null>(null);
    let obsConfirmed = $state(false);

    let roomsList = $state<Room[]>([]);
    let roomName = $state("");
    let roomSlug = $state("");
    let roomWaitingRoom = $state(true);
    let isCreatingRoom = $state(false);
    let roomError = $state("");
    let createdRoom = $state<Room | null>(null);
    let createdRoomSource = $state<"new" | "existing" | null>(null);
    let copiedRoom = $state(false);

    onMount(async () => {
        await loadData();
    });

    async function loadData() {
        isLoading = true;
        loadError = "";

        try {
            const [configData, keysData, roomsData] = await Promise.all([
                appConfig.get(),
                streamKeys.list(),
                rooms.list(),
            ]);

            config = configData;
            baseUrl =
                configData.publicUrl ||
                (typeof window !== "undefined" ? window.location.origin : "");

            updatePublicUrlStatus(configData.publicUrl);

            keys = keysData;
            roomsList = roomsData;

            if (!selectedKeyId && keysData.length > 0) {
                selectedKeyId = keysData[0].id;
            }

            if (!createdRoom && roomsData.length > 0) {
                createdRoom = roomsData[0];
                createdRoomSource = "existing";
            }

            if (!watermarkText) {
                watermarkText =
                    configData.defaultWatermarkText ||
                    "{{ name }} - {{ date }}";
            }

            turnUrl = configData.turnExternalUrl || "";
            turnUsername = configData.turnExternalUsername || "";

            await checkHealth();
        } catch (e: any) {
            loadError = e.message || "Failed to load setup data";
        } finally {
            isLoading = false;
        }
    }

    async function refreshConfig() {
        try {
            config = await appConfig.get();
            baseUrl =
                config?.publicUrl ||
                (typeof window !== "undefined" ? window.location.origin : "");
            updatePublicUrlStatus(config?.publicUrl);
        } catch (e) {
            console.error("Failed to refresh config", e);
        }
    }

    async function checkHealth() {
        healthStatus = "checking";
        healthMessage = "";

        try {
            const res = await fetch("/health", { cache: "no-store" });
            if (!res.ok) {
                healthStatus = "error";
                healthMessage = `HTTP ${res.status}`;
                return;
            }
            const data = await res.json();
            if (data?.status === "ok") {
                healthStatus = "ok";
            } else {
                healthStatus = "error";
                healthMessage = "Unexpected response";
            }
        } catch (e: any) {
            healthStatus = "error";
            healthMessage = e.message || "Health check failed";
        }
    }

    function updatePublicUrlStatus(url?: string) {
        if (!url) {
            publicUrlStatus = {
                tone: "bad",
                label: "Missing",
                detail: "Set PUBLIC_URL in .env",
            };
            return;
        }

        if (url.startsWith("https://") && !isLocalUrl(url)) {
            publicUrlStatus = {
                tone: "good",
                label: "Ready",
                detail: url,
            };
            return;
        }

        if (url.startsWith("http://")) {
            publicUrlStatus = {
                tone: "warn",
                label: "No TLS",
                detail: url,
            };
            return;
        }

        publicUrlStatus = {
            tone: "warn",
            label: "Review",
            detail: url,
        };
    }

    function isLocalUrl(url: string) {
        return url.includes("localhost") || url.includes("127.0.0.1");
    }

    function isPreflightComplete() {
        const { dns, ports, ssl, env, origins, localDev } = preflightChecks;
        return localDev || (dns && ports && ssl && env && origins);
    }

    function isTurnComplete() {
        return turnConfirmed || turnTestResults?.success;
    }

    function isBrandingComplete() {
        return brandingConfirmed || Boolean(config?.defaultWatermarkLogoUrl);
    }

    function isStreamComplete() {
        return obsConfirmed && Boolean(selectedKeyId);
    }

    function isRoomComplete() {
        return Boolean(createdRoom) || roomsList.length > 0;
    }

    function isStepComplete(stepId: string) {
        switch (stepId) {
            case "preflight":
                return isPreflightComplete();
            case "turn":
                return isTurnComplete();
            case "branding":
                return isBrandingComplete();
            case "stream":
                return isStreamComplete();
            case "room":
                return isRoomComplete();
            default:
                return false;
        }
    }

    function completedStepsCount() {
        return steps.filter((step) => isStepComplete(step.id)).length;
    }

    function goNext() {
        if (currentStep < steps.length - 1) {
            currentStep += 1;
        }
    }

    function goBack() {
        if (currentStep > 0) {
            currentStep -= 1;
        }
    }

    function skipSetup() {
        if (typeof localStorage !== "undefined") {
            localStorage.setItem(setupDismissedKey, "true");
        }
        goto("/admin");
    }

    function finishSetup() {
        if (typeof localStorage !== "undefined") {
            localStorage.setItem(setupCompleteKey, "true");
            localStorage.removeItem(setupDismissedKey);
        }
        goto("/admin");
    }

    function exitSetup() {
        goto("/admin");
    }

    async function handleSaveTurn(e: SubmitEvent) {
        e.preventDefault();
        isSavingTurn = true;
        turnError = "";
        turnSuccess = "";

        try {
            const normalizedTurnURL = turnUrl
                .split(",")
                .map((entry) => entry.trim())
                .filter(Boolean)
                .join(",");

            config = await appConfig.update({
                turnExternalUrl: normalizedTurnURL,
                turnExternalUsername: turnUsername.trim(),
                turnExternalCredential: turnCredential || undefined,
            });
            baseUrl =
                config?.publicUrl ||
                (typeof window !== "undefined" ? window.location.origin : "");
            updatePublicUrlStatus(config?.publicUrl);
            turnUrl = config?.turnExternalUrl || "";
            turnUsername = config?.turnExternalUsername || "";
            turnCredential = "";
            turnSuccess = "TURN settings saved";
            turnConfirmed = true;
            setTimeout(() => (turnSuccess = ""), 3000);
        } catch (e: any) {
            turnError = e.message || "Failed to save TURN settings";
        } finally {
            isSavingTurn = false;
        }
    }

    async function handleTestTurn() {
        isTestingTurn = true;
        turnTestResults = null;
        turnError = "";
        turnSuccess = "";

        try {
            turnTestResults = await appConfig.testTurn();
            if (turnTestResults.success) {
                turnSuccess = "TURN connectivity test passed";
                setTimeout(() => (turnSuccess = ""), 4000);
            } else {
                turnError = turnTestResults.message || "TURN test failed";
            }
        } catch (e: any) {
            turnError = e.message || "Failed to test TURN connectivity";
        } finally {
            isTestingTurn = false;
        }
    }

    async function handleSaveWatermark(e: SubmitEvent) {
        e.preventDefault();
        isSavingBranding = true;
        brandingError = "";
        brandingSuccess = "";

        try {
            config = await appConfig.update({
                defaultWatermarkText: watermarkText || undefined,
            });
            brandingSuccess = "Watermark saved";
            brandingConfirmed = true;
            setTimeout(() => (brandingSuccess = ""), 3000);
        } catch (e: any) {
            brandingError = e.message || "Failed to save watermark";
        } finally {
            isSavingBranding = false;
        }
    }

    async function handleLogoUpload(e: Event) {
        const input = e.target as HTMLInputElement;
        const file = input.files?.[0];
        if (!file) return;

        const allowedTypes = ["image/png", "image/jpeg", "image/webp"];
        if (!allowedTypes.includes(file.type)) {
            brandingError = "Invalid file type. Use PNG, JPEG, or WebP.";
            return;
        }

        if (file.size > 1024 * 1024) {
            brandingError = "File too large. Maximum size is 1MB.";
            return;
        }

        isUploadingLogo = true;
        brandingError = "";
        brandingSuccess = "";

        try {
            await appConfig.uploadLogo(file);
            await refreshConfig();
            brandingSuccess = "Logo uploaded";
            brandingConfirmed = true;
            setTimeout(() => (brandingSuccess = ""), 3000);
        } catch (e: any) {
            brandingError = e.message || "Failed to upload logo";
        } finally {
            isUploadingLogo = false;
            input.value = "";
        }
    }

    async function handleDeleteLogo() {
        if (!confirm("Delete the default logo?")) {
            return;
        }

        brandingError = "";
        brandingSuccess = "";

        try {
            await appConfig.deleteLogo();
            await refreshConfig();
            brandingSuccess = "Logo removed";
            setTimeout(() => (brandingSuccess = ""), 3000);
        } catch (e: any) {
            brandingError = e.message || "Failed to delete logo";
        }
    }

    async function handleCreateKey(e: SubmitEvent) {
        e.preventDefault();
        if (!newKeyName.trim()) return;

        isCreatingKey = true;
        streamError = "";

        try {
            const key = await streamKeys.create(newKeyName.trim());
            keys = [...keys, key];
            selectedKeyId = key.id;
            newKeyName = "";
        } catch (e: any) {
            streamError = e.message || "Failed to create stream key";
        } finally {
            isCreatingKey = false;
        }
    }

    function copyWhipUrl() {
        const key = keys.find((k) => k.id === selectedKeyId);
        if (!key) return;
        const url = `${baseUrl}/whip/${key.keyToken}`;
        navigator.clipboard.writeText(url);
        copiedKeyId = key.id;
        setTimeout(() => (copiedKeyId = null), 2000);
    }

    function handleRoomNameChange(e: Event) {
        const target = e.target as HTMLInputElement;
        roomName = target.value;
        if (!roomSlug || roomSlug === generateSlug(roomName.slice(0, -1))) {
            roomSlug = generateSlug(roomName);
        }
    }

    function generateSlug(text: string): string {
        return text
            .toLowerCase()
            .replace(/[^a-z0-9\s-]/g, "")
            .replace(/\s+/g, "-")
            .replace(/-+/g, "-")
            .slice(0, 64);
    }

    function inferWatermarkMode() {
        const hasText = Boolean(watermarkText?.trim());
        const hasLogo = Boolean(config?.defaultWatermarkLogoUrl);
        if (hasText && hasLogo) return "both";
        if (hasLogo) return "logo";
        if (hasText) return "text";
        return "none";
    }

    async function handleCreateRoom(e: SubmitEvent) {
        e.preventDefault();
        if (!roomName.trim() || !roomSlug.trim()) return;

        isCreatingRoom = true;
        roomError = "";

        try {
            const watermarkMode = inferWatermarkMode();
            const payload: Record<string, unknown> = {
                name: roomName.trim(),
                slug: roomSlug.trim(),
                waitingRoomEnabled: roomWaitingRoom,
                watermarkMode,
            };

            if (selectedKeyId) {
                payload.streamKeyId = selectedKeyId;
            }

            if (watermarkMode === "text" || watermarkMode === "both") {
                payload.watermarkText =
                    watermarkText || "{{ name }} - {{ date }}";
            }

            const room = await rooms.create(payload);
            createdRoom = room;
            createdRoomSource = "new";
            roomsList = [room, ...roomsList];
        } catch (e: any) {
            roomError = e.message || "Failed to create room";
        } finally {
            isCreatingRoom = false;
        }
    }

    function roomUrl() {
        if (!createdRoom) return "";
        return `${baseUrl}/room/${createdRoom.slug}`;
    }

    function adminRoomUrl() {
        if (!createdRoom) return "";
        return `/admin/rooms/${createdRoom.slug}`;
    }

    function copyRoomUrl() {
        const url = roomUrl();
        if (!url) return;
        navigator.clipboard.writeText(url);
        copiedRoom = true;
        setTimeout(() => (copiedRoom = false), 2000);
    }

    function turnModeLabel(mode?: string) {
        switch (mode) {
            case "external":
                return "External TURN only";
            case "hybrid":
                return "Hybrid (self-hosted + external)";
            case "self-hosted":
                return "Self-hosted TURN only";
            default:
                return "Not configured";
        }
    }
</script>

<svelte:head>
    <title>Setup Wizard | Chromatic</title>
</svelte:head>

<div class="setup-page">
    <header class="setup-hero">
        <div class="hero-text">
            <span class="eyebrow">First-run setup</span>
            <h1>Launch a perfect Chromatic install</h1>
            <p class="hero-subtitle">
                Follow the checklist to verify infrastructure, configure TURN,
                brand your rooms, and start your first stream.
            </p>
        </div>
        <div class="hero-actions">
            <button class="btn btn-ghost" onclick={exitSetup}>Exit</button>
            <button class="btn btn-secondary" onclick={skipSetup}>
                Skip for now
            </button>
        </div>
    </header>

    {#if isLoading}
        <div class="loading">
            <div class="waiting-spinner"></div>
        </div>
    {:else if loadError}
        <div class="error-message">{loadError}</div>
    {:else}
        <section class="setup-progress card">
            <div class="progress-meta">
                <span>Progress</span>
                <span>{completedStepsCount()}/{steps.length} completed</span>
            </div>
            <div class="progress-bar">
                <div
                    class="progress-fill"
                    style="width: {Math.round(
                        (completedStepsCount() / steps.length) * 100,
                    )}%"
                ></div>
            </div>
        </section>

        <div class="setup-body">
            <aside class="setup-steps">
                <ol>
                    {#each steps as step, index}
                        <li
                            class:active={index === currentStep}
                            class:complete={isStepComplete(step.id)}
                        >
                            <button
                                class="step-button"
                                onclick={() => (currentStep = index)}
                            >
                                <span class="step-index">{index + 1}</span>
                                <span class="step-text">
                                    <span class="step-title">{step.title}</span>
                                    <span class="step-desc"
                                        >{step.description}</span
                                    >
                                </span>
                            </button>
                        </li>
                    {/each}
                </ol>
            </aside>

            <section class="setup-content">
                {#if currentStep === 0}
                    <div class="step-panel">
                        <div class="panel-header">
                            <h2>Preflight checks</h2>
                            <p>
                                Confirm the server is healthy and the deployment
                                basics are in place.
                            </p>
                        </div>

                        <div class="status-grid">
                            <div class="status-card">
                                <div class="status-header">
                                    <span class="status-label">Health</span>
                                    <span
                                        class="status-pill {healthStatus}"
                                        >{healthStatus === "checking"
                                            ? "Checking"
                                            : healthStatus === "ok"
                                              ? "Healthy"
                                              : "Needs attention"}</span
                                    >
                                </div>
                                <p class="status-detail">
                                    Endpoint: <code>/health</code>
                                </p>
                                {#if healthMessage}
                                    <p class="status-note">{healthMessage}</p>
                                {/if}
                                <button
                                    class="btn btn-secondary btn-sm"
                                    onclick={checkHealth}
                                >
                                    Recheck
                                </button>
                            </div>

                            <div class="status-card">
                                <div class="status-header">
                                    <span class="status-label">Public URL</span>
                                    <span
                                        class="status-pill {publicUrlStatus.tone}"
                                        >{publicUrlStatus.label}</span
                                    >
                                </div>
                                <p class="status-detail">
                                    {publicUrlStatus.detail ||
                                        "Set PUBLIC_URL in .env"}
                                </p>
                                <p class="status-note">
                                    Use HTTPS for production streaming.
                                </p>
                            </div>

                            <div class="status-card">
                                <div class="status-header">
                                    <span class="status-label">WHIP</span>
                                    <span class="status-pill neutral">Format</span>
                                </div>
                                <p class="status-detail">
                                    {config?.whipFormat ||
                                        "Not configured yet"}
                                </p>
                                <p class="status-note">
                                    Stream keys replace
                                    <code>{"{stream_key_token}"}</code>.
                                </p>
                            </div>
                        </div>

                        <div class="card checklist-card">
                            <h3>Production checklist</h3>
                            <p class="section-description">
                                If you are running locally, check the box to
                                skip the production items.
                            </p>
                            <div class="checklist-grid">
                                <label class="check-item">
                                    <input
                                        type="checkbox"
                                        bind:checked={preflightChecks.env}
                                    />
                                    <span>
                                        Environment vars set (PUBLIC_URL,
                                        ADMIN_TOKEN, TURN_MODE + TURN provider
                                        credentials)
                                    </span>
                                </label>
                                <label class="check-item">
                                    <input
                                        type="checkbox"
                                        bind:checked={preflightChecks.dns}
                                    />
                                    <span>DNS points to server IP</span>
                                </label>
                                <label class="check-item">
                                    <input
                                        type="checkbox"
                                        bind:checked={preflightChecks.ssl}
                                    />
                                    <span>SSL certificate is valid</span>
                                </label>
                                <label class="check-item">
                                    <input
                                        type="checkbox"
                                        bind:checked={preflightChecks.ports}
                                    />
                                    <span>
                                        Firewall open: 80, 443 (plus TURN ports
                                        only when self-hosting TURN)
                                    </span>
                                </label>
                                <label class="check-item">
                                    <input
                                        type="checkbox"
                                        bind:checked={preflightChecks.origins}
                                    />
                                    <span>
                                        PRODUCTION_MODE + ALLOWED_ORIGINS set
                                    </span>
                                </label>
                            </div>
                            <label class="check-item check-item-alt">
                                <input
                                    type="checkbox"
                                    bind:checked={preflightChecks.localDev}
                                />
                                <span>Running locally (skip production)</span>
                            </label>
                        </div>
                    </div>
                {:else if currentStep === 1}
                    <div class="step-panel">
                        <div class="panel-header">
                            <h2>Connectivity and TURN</h2>
                            <p>
                                Cloudflare TURN is the recommended default for
                                non-self-hosted deployments. Add static TURN
                                fallback only when needed.
                            </p>
                        </div>

                        <div class="split-grid">
                            <section class="card">
                                <h3>TURN provider</h3>
                                <p class="section-description">
                                    Current runtime mode:
                                    <strong>{turnModeLabel(config?.turnMode)}</strong
                                    >.
                                </p>

                                <div class="status-grid">
                                    <div class="status-card">
                                        <div class="status-header">
                                            <span class="status-label"
                                                >Cloudflare TURN</span
                                            >
                                            <span
                                                class="status-pill {config?.turnCloudflareConfigured
                                                    ? 'good'
                                                    : 'warn'}"
                                                >{config?.turnCloudflareConfigured
                                                    ? "Configured"
                                                    : "Not configured"}</span
                                            >
                                        </div>
                                        <p class="status-detail">
                                            {config?.turnCloudflareConfigured
                                                ? "Credentials are generated on-demand; no static username/password needed."
                                                : "Set TURN_CLOUDFLARE_KEY_ID and TURN_CLOUDFLARE_API_TOKEN in .env for the recommended setup."}
                                        </p>
                                    </div>
                                </div>

                                {#if turnError}
                                    <div class="error-message">{turnError}</div>
                                {/if}

                                {#if turnSuccess}
                                    <div class="success-message">
                                        {turnSuccess}
                                    </div>
                                {/if}

                                <form onsubmit={handleSaveTurn}>
                                    <div class="form-group">
                                        <label for="turnUrl"
                                            >Static TURN URL(s)</label
                                        >
                                        <input
                                            type="text"
                                            id="turnUrl"
                                            class="input"
                                            bind:value={turnUrl}
                                            placeholder="turn:turn.cloudflare.com:3478?transport=udp,turns:turn.cloudflare.com:443?transport=tcp"
                                        />
                                        <p class="hint">
                                            Optional. Comma-separated URLs are
                                            supported.
                                        </p>
                                    </div>

                                    <div class="form-group">
                                        <label for="turnUsername">Username</label>
                                        <input
                                            type="text"
                                            id="turnUsername"
                                            class="input"
                                            bind:value={turnUsername}
                                            placeholder="TURN username"
                                        />
                                    </div>

                                    <div class="form-group">
                                        <label for="turnCredential"
                                            >Credential</label
                                        >
                                        <input
                                            type="password"
                                            id="turnCredential"
                                            class="input"
                                            bind:value={turnCredential}
                                            placeholder={config?.hasTurnCredential
                                                ? "********"
                                                : "TURN credential"}
                                        />
                                        {#if config?.hasTurnCredential}
                                            <p class="hint">
                                                A credential is already set.
                                                Enter a new value to change it.
                                            </p>
                                        {/if}
                                    </div>

                                    <div class="button-row">
                                        <button
                                            type="submit"
                                            class="btn btn-primary"
                                            disabled={isSavingTurn}
                                        >
                                            {isSavingTurn
                                                ? "Saving..."
                                                : "Save TURN"}
                                        </button>
                                        <button
                                            type="button"
                                            class="btn btn-secondary"
                                            onclick={handleTestTurn}
                                            disabled={isTestingTurn}
                                        >
                                            {isTestingTurn
                                                ? "Testing..."
                                                : "Test Connectivity"}
                                        </button>
                                    </div>
                                </form>

                                <label class="check-item check-item-alt">
                                    <input
                                        type="checkbox"
                                        bind:checked={turnConfirmed}
                                    />
                                    <span>
                                        TURN configuration validated for this
                                        deployment
                                    </span>
                                </label>
                            </section>

                            <section class="card">
                                <h3>Connectivity results</h3>
                                <p class="section-description">
                                    Run a quick socket test to confirm TURN is
                                    reachable.
                                </p>

                                {#if turnTestResults}
                                    <div
                                        class="test-summary"
                                        class:success={turnTestResults.success}
                                        class:failure={!turnTestResults.success}
                                    >
                                        {turnTestResults.message}
                                    </div>
                                    {#if turnTestResults.results.length > 0}
                                        <table class="test-results-table">
                                            <thead>
                                                <tr>
                                                    <th>Server</th>
                                                    <th>Type</th>
                                                    <th>Protocol</th>
                                                    <th>Status</th>
                                                    <th>Latency</th>
                                                </tr>
                                            </thead>
                                            <tbody>
                                                {#each turnTestResults.results as result}
                                                    <tr>
                                                        <td>
                                                            <code
                                                                >{result.server}</code
                                                            >
                                                        </td>
                                                        <td>{result.testType}</td>
                                                        <td>
                                                            {result.protocol ||
                                                                "n/a"}
                                                        </td>
                                                        <td>
                                                            {#if result.reachable}
                                                                <span
                                                                    class="status-pill good"
                                                                    >Reachable</span
                                                                >
                                                            {:else}
                                                                <span
                                                                    class="status-pill bad"
                                                                    >Failed</span
                                                                >
                                                            {/if}
                                                        </td>
                                                        <td>
                                                            {#if result.latency}
                                                                {result.latency}ms
                                                            {:else if result.error}
                                                                <span
                                                                    class="error-text"
                                                                    title={result.error}
                                                                    >Error</span
                                                                >
                                                            {:else}
                                                                -
                                                            {/if}
                                                        </td>
                                                    </tr>
                                                {/each}
                                            </tbody>
                                        </table>
                                    {/if}
                                {:else}
                                    <div class="empty-note">
                                        No tests run yet.
                                    </div>
                                {/if}
                            </section>
                        </div>
                    </div>
                {:else if currentStep === 2}
                    <div class="step-panel">
                        <div class="panel-header">
                            <h2>Branding and watermark</h2>
                            <p>
                                Set defaults for new rooms. You can override
                                these per room later.
                            </p>
                        </div>

                        <div class="split-grid">
                            <section class="card">
                                <h3>Default watermark text</h3>
                                <p class="section-description">
                                    Keep the default template or customize it
                                    now.
                                </p>

                                {#if brandingError}
                                    <div class="error-message">
                                        {brandingError}
                                    </div>
                                {/if}

                                {#if brandingSuccess}
                                    <div class="success-message">
                                        {brandingSuccess}
                                    </div>
                                {/if}

                                <form onsubmit={handleSaveWatermark}>
                                    <div class="form-group">
                                        <label for="watermarkText"
                                            >Watermark text</label
                                        >
                                        <input
                                            type="text"
                                            id="watermarkText"
                                            class="input"
                                            bind:value={watermarkText}
                                            placeholder={"{{name}} - {{date}}"}
                                        />
                                        <p class="hint">
                                            Variables:
                                            <code>{"{{name}}"}</code>,
                                            <code>{"{{room}}"}</code>,
                                            <code>{"{{date}}"}</code>,
                                            <code>{"{{time}}"}</code>
                                        </p>
                                    </div>

                                    <button
                                        type="submit"
                                        class="btn btn-primary"
                                        disabled={isSavingBranding}
                                    >
                                        {isSavingBranding
                                            ? "Saving..."
                                            : "Save Watermark"}
                                    </button>
                                </form>

                                <label class="check-item check-item-alt">
                                    <input
                                        type="checkbox"
                                        bind:checked={brandingConfirmed}
                                    />
                                    <span>Keep current branding as-is</span>
                                </label>
                            </section>

                            <section class="card">
                                <h3>Default logo</h3>
                                <p class="section-description">
                                    Optional logo for brand attribution and
                                    deterrence.
                                </p>

                                {#if config?.defaultWatermarkLogoUrl}
                                    <div class="logo-preview">
                                        <img
                                            src={config.defaultWatermarkLogoUrl}
                                            alt="Current logo"
                                        />
                                        <button
                                            class="btn btn-secondary btn-sm btn-danger-text"
                                            onclick={handleDeleteLogo}
                                        >
                                            Delete
                                        </button>
                                    </div>
                                {/if}

                                <div class="file-upload">
                                    <input
                                        type="file"
                                        accept="image/png,image/jpeg,image/webp"
                                        onchange={handleLogoUpload}
                                        disabled={isUploadingLogo}
                                    />
                                    {#if isUploadingLogo}
                                        <span class="upload-status"
                                            >Uploading...</span
                                        >
                                    {/if}
                                </div>
                                <p class="hint">
                                    Recommended: transparent PNG, max 500x500px,
                                    max 1MB.
                                </p>
                            </section>
                        </div>
                    </div>
                {:else if currentStep === 3}
                    <div class="step-panel">
                        <div class="panel-header">
                            <h2>Stream setup</h2>
                            <p>
                                Create a stream key and configure OBS with the
                                WHIP URL.
                            </p>
                        </div>

                        <div class="split-grid">
                            <section class="card">
                                <h3>Stream keys</h3>
                                <p class="section-description">
                                    Each key maps to one inbound stream.
                                </p>

                                {#if streamError}
                                    <div class="error-message">
                                        {streamError}
                                    </div>
                                {/if}

                                <form onsubmit={handleCreateKey}>
                                    <div class="form-group">
                                        <label for="newKey">Key name</label>
                                        <input
                                            type="text"
                                            id="newKey"
                                            class="input"
                                            bind:value={newKeyName}
                                            placeholder="Main Studio"
                                        />
                                    </div>
                                    <button
                                        type="submit"
                                        class="btn btn-primary"
                                        disabled={isCreatingKey}
                                    >
                                        {isCreatingKey
                                            ? "Creating..."
                                            : "Create Key"}
                                    </button>
                                </form>

                                {#if keys.length > 0}
                                    <div class="form-group">
                                        <label for="keySelect">Active key</label>
                                        <select
                                            id="keySelect"
                                            class="input"
                                            bind:value={selectedKeyId}
                                        >
                                            {#each keys as key (key.id)}
                                                <option value={key.id}>
                                                    {key.name}
                                                </option>
                                            {/each}
                                        </select>
                                    </div>
                                {/if}

                                {#if selectedKeyId}
                                    <div class="whip-block">
                                        <span class="field-label">WHIP URL</span>
                                        <div class="copy-row">
                                            <code>
                                                {baseUrl}/whip/{keys.find((k) => k.id === selectedKeyId)?.keyToken}
                                            </code>
                                            <button
                                                class="btn btn-secondary btn-sm"
                                                onclick={copyWhipUrl}
                                            >
                                                {copiedKeyId === selectedKeyId
                                                    ? "Copied"
                                                    : "Copy"}
                                            </button>
                                        </div>
                                    </div>
                                {/if}
                            </section>

                            <section class="card">
                                <h3>OBS settings (critical)</h3>
                                <p class="section-description">
                                    These settings keep latency under control.
                                </p>
                                <ul class="obs-list">
                                    <li>
                                        <span>Service</span>
                                        <strong>WHIP</strong>
                                    </li>
                                    <li>
                                        <span>Profile</span>
                                        <strong>Baseline</strong>
                                    </li>
                                    <li>
                                        <span>B-frames</span>
                                        <strong>0</strong>
                                    </li>
                                    <li>
                                        <span>Keyframe interval</span>
                                        <strong>2 seconds</strong>
                                    </li>
                                    <li>
                                        <span>Tune</span>
                                        <strong>zerolatency</strong>
                                    </li>
                                    <li>
                                        <span>Bitrate</span>
                                        <strong>6000-10000 Kbps</strong>
                                    </li>
                                </ul>
                                <label class="check-item check-item-alt">
                                    <input
                                        type="checkbox"
                                        bind:checked={obsConfirmed}
                                    />
                                    <span>
                                        OBS configured with WHIP URL and
                                        B-frames set to 0
                                    </span>
                                </label>
                            </section>
                        </div>
                    </div>
                {:else if currentStep === 4}
                    <div class="step-panel">
                        <div class="panel-header">
                            <h2>Create your first room</h2>
                            <p>
                                This room is where you will invite viewers and
                                verify the stream.
                            </p>
                        </div>

                        <div class="split-grid">
                            <section class="card">
                                <h3>Room details</h3>
                                {#if roomError}
                                    <div class="error-message">{roomError}</div>
                                {/if}
                                <form onsubmit={handleCreateRoom}>
                                    <div class="form-group">
                                        <label for="roomName">Room name</label>
                                        <input
                                            type="text"
                                            id="roomName"
                                            class="input"
                                            value={roomName}
                                            oninput={handleRoomNameChange}
                                            placeholder="Color review"
                                            maxlength="100"
                                            required
                                        />
                                    </div>

                                    <div class="form-group">
                                        <label for="roomSlug">URL slug</label>
                                        <div class="slug-preview">
                                            <span class="slug-prefix">/room/</span>
                                            <input
                                                type="text"
                                                id="roomSlug"
                                                class="input slug-input"
                                                bind:value={roomSlug}
                                                placeholder="color-review"
                                                pattern={"[a-z0-9-]{3,64}"}
                                                minlength="3"
                                                maxlength="64"
                                                required
                                            />
                                        </div>
                                        <span class="form-hint"
                                            >3-64 characters: lowercase letters,
                                            numbers, hyphens</span
                                        >
                                    </div>

                                    <div class="form-group">
                                        <label for="roomKey">Stream key</label>
                                        <select
                                            id="roomKey"
                                            class="input"
                                            bind:value={selectedKeyId}
                                        >
                                            <option value="">None</option>
                                            {#each keys as key (key.id)}
                                                <option value={key.id}>
                                                    {key.name}
                                                </option>
                                            {/each}
                                        </select>
                                    </div>

                                    <label class="check-item check-item-alt">
                                        <input
                                            type="checkbox"
                                            bind:checked={roomWaitingRoom}
                                        />
                                        <span>Enable waiting room</span>
                                    </label>

                                    <button
                                        type="submit"
                                        class="btn btn-primary"
                                        disabled={isCreatingRoom}
                                    >
                                        {isCreatingRoom
                                            ? "Creating..."
                                            : "Create Room"}
                                    </button>
                                </form>
                            </section>

                            <section class="card">
                                <h3>Room launch</h3>
                                {#if createdRoom}
                                    <p class="section-description">
                                        {createdRoomSource === "existing"
                                            ? "Existing room detected."
                                            : "Room created successfully."}
                                    </p>
                                    <div class="whip-block">
                                        <span class="field-label">Viewer URL</span>
                                        <div class="copy-row">
                                            <code>{roomUrl()}</code>
                                            <button
                                                class="btn btn-secondary btn-sm"
                                                onclick={copyRoomUrl}
                                            >
                                                {copiedRoom ? "Copied" : "Copy"}
                                            </button>
                                        </div>
                                    </div>
                                    <div class="button-row">
                                        <a
                                            class="btn btn-secondary"
                                            href={adminRoomUrl()}
                                        >
                                            Open Admin Room
                                        </a>
                                        <a
                                            class="btn btn-primary"
                                            href={roomUrl()}
                                            target="_blank"
                                            rel="noopener noreferrer"
                                        >
                                            Open Viewer
                                        </a>
                                    </div>
                                {:else}
                                    <p class="section-description">
                                        Create a room to get the viewer link and
                                        test the stream.
                                    </p>
                                {/if}

                                <div class="callout">
                                    <h4>Quick test</h4>
                                    <ol>
                                        <li>Start streaming from OBS.</li>
                                        <li>Open the viewer link in a private window.</li>
                                        <li>Confirm sub-second latency.</li>
                                    </ol>
                                </div>
                            </section>
                        </div>
                    </div>
                {/if}
            </section>
        </div>

        <footer class="setup-footer">
            <button
                class="btn btn-secondary"
                onclick={goBack}
                disabled={currentStep === 0}
            >
                Back
            </button>
            {#if currentStep < steps.length - 1}
                <button class="btn btn-primary" onclick={goNext}>Next</button>
            {:else}
                <button class="btn btn-primary" onclick={finishSetup}>
                    Finish Setup
                </button>
            {/if}
        </footer>
    {/if}
</div>

<style>
    .setup-page {
        max-width: 1200px;
        margin: 0 auto;
        display: flex;
        flex-direction: column;
        gap: var(--space-xl);
    }

    .setup-hero {
        position: relative;
        overflow: hidden;
        padding: var(--space-xl);
        border-radius: var(--radius-lg);
        border: 1px solid var(--color-border);
        background: linear-gradient(
            135deg,
            rgba(72, 182, 166, 0.18),
            rgba(230, 162, 60, 0.12)
        );
        display: flex;
        justify-content: space-between;
        align-items: center;
        gap: var(--space-lg);
    }

    .setup-hero::after {
        content: "";
        position: absolute;
        width: 260px;
        height: 260px;
        right: -120px;
        top: -140px;
        background: radial-gradient(
            circle,
            rgba(72, 182, 166, 0.35),
            transparent 70%
        );
        opacity: 0.8;
    }

    .hero-text {
        max-width: 560px;
    }

    .eyebrow {
        text-transform: uppercase;
        font-size: 0.75rem;
        letter-spacing: 0.12em;
        color: var(--color-text-subtle);
    }

    .hero-subtitle {
        margin-top: var(--space-sm);
        color: var(--color-text-muted);
    }

    .hero-actions {
        display: flex;
        gap: var(--space-sm);
    }

    .setup-progress {
        padding: var(--space-lg);
    }

    .progress-meta {
        display: flex;
        justify-content: space-between;
        font-size: 0.875rem;
        color: var(--color-text-muted);
        margin-bottom: var(--space-sm);
    }

    .progress-bar {
        height: 6px;
        background: var(--color-surface-elevated);
        border-radius: var(--radius-full);
        overflow: hidden;
    }

    .progress-fill {
        height: 100%;
        background: linear-gradient(90deg, var(--color-primary), #e6a23c);
        transition: width var(--transition-normal);
    }

    .setup-body {
        display: grid;
        grid-template-columns: minmax(220px, 260px) 1fr;
        gap: var(--space-xl);
        align-items: start;
    }

    .setup-steps {
        position: sticky;
        top: var(--space-xl);
    }

    .setup-steps ol {
        list-style: none;
        display: flex;
        flex-direction: column;
        gap: var(--space-sm);
    }

    .setup-steps li {
        border-radius: var(--radius-md);
        background: var(--color-surface);
        border: 1px solid var(--color-border-subtle);
        transition: border-color var(--transition-fast);
    }

    .setup-steps li.active {
        border-color: var(--color-primary);
        box-shadow: var(--shadow-sm);
    }

    .setup-steps li.complete .step-index {
        background: rgba(47, 191, 113, 0.2);
        color: var(--color-success);
        border-color: rgba(47, 191, 113, 0.4);
    }

    .step-button {
        width: 100%;
        text-align: left;
        padding: var(--space-md);
        background: transparent;
        border: none;
        color: inherit;
        display: flex;
        gap: var(--space-md);
        cursor: pointer;
    }

    .step-index {
        width: 32px;
        height: 32px;
        border-radius: 50%;
        border: 1px solid var(--color-border);
        display: flex;
        align-items: center;
        justify-content: center;
        font-size: 0.875rem;
        color: var(--color-text-muted);
        flex-shrink: 0;
    }

    .step-text {
        display: flex;
        flex-direction: column;
        gap: 0.25rem;
    }

    .step-title {
        font-weight: 600;
        font-size: 0.95rem;
    }

    .step-desc {
        font-size: 0.75rem;
        color: var(--color-text-subtle);
    }

    .setup-content {
        display: flex;
        flex-direction: column;
        gap: var(--space-lg);
    }

    .step-panel {
        display: flex;
        flex-direction: column;
        gap: var(--space-lg);
    }

    .panel-header p {
        color: var(--color-text-muted);
        margin-top: var(--space-xs);
    }

    .status-grid {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
        gap: var(--space-md);
    }

    .status-card {
        padding: var(--space-lg);
        border-radius: var(--radius-lg);
        border: 1px solid var(--color-border);
        background: var(--color-surface);
        display: flex;
        flex-direction: column;
        gap: var(--space-sm);
    }

    .status-header {
        display: flex;
        justify-content: space-between;
        align-items: center;
    }

    .status-label {
        font-size: 0.75rem;
        text-transform: uppercase;
        letter-spacing: 0.08em;
        color: var(--color-text-subtle);
    }

    .status-detail {
        font-size: 0.875rem;
        color: var(--color-text);
        word-break: break-word;
    }

    .status-note {
        font-size: 0.75rem;
        color: var(--color-text-subtle);
    }

    .status-pill {
        padding: 0.2rem 0.5rem;
        border-radius: var(--radius-full);
        font-size: 0.75rem;
        border: 1px solid transparent;
        background: rgba(148, 163, 184, 0.15);
        color: var(--color-text-muted);
    }

    .status-pill.ok,
    .status-pill.good {
        background: rgba(47, 191, 113, 0.18);
        border-color: rgba(47, 191, 113, 0.35);
        color: var(--color-success);
    }

    .status-pill.warn {
        background: rgba(230, 162, 60, 0.18);
        border-color: rgba(230, 162, 60, 0.35);
        color: var(--color-warning);
    }

    .status-pill.bad,
    .status-pill.error {
        background: rgba(239, 90, 90, 0.18);
        border-color: rgba(239, 90, 90, 0.35);
        color: var(--color-error);
    }

    .status-pill.checking {
        background: rgba(59, 130, 246, 0.15);
        border-color: rgba(59, 130, 246, 0.35);
        color: #93c5fd;
    }

    .status-pill.neutral {
        background: rgba(148, 163, 184, 0.2);
        border-color: rgba(148, 163, 184, 0.3);
        color: var(--color-text-muted);
    }

    .checklist-card h3 {
        margin-bottom: var(--space-sm);
    }

    .section-description {
        color: var(--color-text-muted);
        font-size: 0.875rem;
        margin-bottom: var(--space-md);
    }

    .checklist-grid {
        display: grid;
        gap: var(--space-sm);
    }

    .check-item {
        display: flex;
        align-items: flex-start;
        gap: var(--space-sm);
        font-size: 0.875rem;
        color: var(--color-text);
    }

    .check-item input {
        margin-top: 0.2rem;
    }

    .check-item-alt {
        margin-top: var(--space-md);
        padding-top: var(--space-md);
        border-top: 1px solid var(--color-border-subtle);
    }

    .split-grid {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
        gap: var(--space-lg);
    }

    .form-group {
        margin-bottom: var(--space-md);
    }

    .hint {
        font-size: 0.75rem;
        color: var(--color-text-subtle);
        margin-top: var(--space-xs);
    }

    .button-row {
        display: flex;
        gap: var(--space-sm);
        flex-wrap: wrap;
        margin-top: var(--space-md);
    }

    .error-message,
    .success-message {
        padding: var(--space-sm) var(--space-md);
        border-radius: var(--radius-md);
        font-size: 0.875rem;
        margin-bottom: var(--space-md);
    }

    .error-message {
        background: rgba(239, 90, 90, 0.12);
        border: 1px solid var(--color-error);
        color: var(--color-error);
    }

    .success-message {
        background: rgba(47, 191, 113, 0.12);
        border: 1px solid var(--color-success);
        color: var(--color-success);
    }

    .test-summary {
        padding: var(--space-sm) var(--space-md);
        border-radius: var(--radius-md);
        font-size: 0.875rem;
        margin-bottom: var(--space-md);
    }

    .test-summary.success {
        background: rgba(47, 191, 113, 0.12);
        border: 1px solid var(--color-success);
        color: var(--color-success);
    }

    .test-summary.failure {
        background: rgba(239, 90, 90, 0.12);
        border: 1px solid var(--color-error);
        color: var(--color-error);
    }

    .test-results-table {
        width: 100%;
        border-collapse: collapse;
        font-size: 0.875rem;
    }

    .test-results-table th,
    .test-results-table td {
        padding: var(--space-sm);
        text-align: left;
        border-bottom: 1px solid var(--color-border);
    }

    .test-results-table th {
        font-size: 0.75rem;
        text-transform: uppercase;
        letter-spacing: 0.08em;
        color: var(--color-text-muted);
    }

    .error-text {
        color: var(--color-error);
        text-decoration: underline dotted;
        cursor: help;
    }

    .empty-note {
        padding: var(--space-md);
        background: var(--color-surface-elevated);
        border-radius: var(--radius-md);
        color: var(--color-text-muted);
    }

    .logo-preview {
        display: flex;
        align-items: center;
        gap: var(--space-md);
        margin-bottom: var(--space-md);
        padding: var(--space-md);
        background: var(--color-surface-elevated);
        border-radius: var(--radius-md);
    }

    .logo-preview img {
        max-width: 100px;
        max-height: 100px;
        object-fit: contain;
    }

    .file-upload {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
    }

    .upload-status {
        font-size: 0.875rem;
        color: var(--color-text-muted);
    }

    .whip-block .field-label {
        display: block;
        font-size: 0.75rem;
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
        border-radius: var(--radius-md);
        font-size: 0.75rem;
        overflow-x: auto;
        white-space: nowrap;
    }

    .obs-list {
        list-style: none;
        display: grid;
        gap: var(--space-sm);
        margin-bottom: var(--space-md);
    }

    .obs-list li {
        display: flex;
        justify-content: space-between;
        align-items: center;
        padding: var(--space-xs) 0;
        border-bottom: 1px solid var(--color-border-subtle);
        font-size: 0.875rem;
    }

    .obs-list li:last-child {
        border-bottom: none;
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

    .form-hint {
        display: block;
        font-size: 0.75rem;
        color: var(--color-text-subtle);
        margin-top: var(--space-xs);
    }

    .callout {
        margin-top: var(--space-lg);
        padding: var(--space-md);
        border-radius: var(--radius-md);
        background: rgba(72, 182, 166, 0.12);
        border: 1px solid rgba(72, 182, 166, 0.3);
    }

    .callout h4 {
        margin-bottom: var(--space-sm);
    }

    .callout ol {
        padding-left: var(--space-lg);
        color: var(--color-text-muted);
        font-size: 0.875rem;
        display: grid;
        gap: var(--space-xs);
    }

    .setup-footer {
        display: flex;
        justify-content: space-between;
        gap: var(--space-md);
        padding-top: var(--space-md);
        border-top: 1px solid var(--color-border-subtle);
    }

    .btn-sm {
        padding: var(--space-xs) var(--space-sm);
        font-size: 0.75rem;
    }

    .btn-danger-text {
        color: var(--color-error);
    }

    .btn-danger-text:hover {
        background: rgba(239, 90, 90, 0.15);
    }

    .loading {
        display: flex;
        justify-content: center;
        padding: var(--space-2xl);
    }

    @media (max-width: 1024px) {
        .setup-body {
            grid-template-columns: 1fr;
        }

        .setup-steps {
            position: static;
        }
    }

    @media (max-width: 768px) {
        .setup-hero {
            flex-direction: column;
            align-items: flex-start;
        }

        .hero-actions {
            width: 100%;
            justify-content: flex-start;
        }

        .setup-footer {
            flex-direction: column;
        }
    }
</style>
