<script lang="ts">
    import { onDestroy, onMount } from "svelte";
    import { goto } from "$app/navigation";
    import { fade } from "svelte/transition";
    import ConfirmDialog from "$lib/components/ConfirmDialog.svelte";
    import CopyField from "$lib/components/CopyField.svelte";
    import StatusPill from "$lib/components/StatusPill.svelte";
    import {
        appConfig,
        rooms,
        streamKeys,
        setup,
        SetupIncompleteError,
        type AppConfig,
        type Room,
        type StreamKey,
        type SetupStatusResponse,
        type SetupCheck,
        type TURNTestResponse,
    } from "$lib/api/client";
    import {
        setupSteps,
        stepForCheck,
        nextSetupStep,
        setupProgressPercent,
        setupCheckTone,
        checkById,
        requiredMissingChecks,
        type SetupStepId,
    } from "$lib/setup/wizard";

    const DEFAULT_WATERMARK_TEXT = "{{ name }} - {{ date }}";

    let currentStep = $state<SetupStepId>("preflight");
    let isLoading = $state(true);
    let loadError = $state("");
    let confirmDeleteLogoOpen = $state(false);

    // Server-owned setup state. The wizard no longer derives completion from
    // browser localStorage or self-attested checkboxes.
    let setupStatus = $state<SetupStatusResponse | null>(null);
    let setupError = $state("");
    let isCompletingSetup = $state(false);
    let isDismissingSetup = $state(false);

    let config = $state<AppConfig | null>(null);
    let baseUrl = $state("");
    let healthStatus = $state<"checking" | "ok" | "error">("checking");
    let healthMessage = $state("");

    let turnUrl = $state("");
    let turnUsername = $state("");
    let turnCredential = $state("");
    let clearTurnCredential = $state(false);
    let turnTestResults = $state<TURNTestResponse | null>(null);
    let isSavingTurn = $state(false);
    let isTestingTurn = $state(false);
    let turnError = $state("");
    let turnSuccess = $state("");

    let watermarkText = $state("");
    let isSavingBranding = $state(false);
    let isUploadingLogo = $state(false);
    let brandingError = $state("");
    let brandingSuccess = $state("");

    let keys = $state<StreamKey[]>([]);
    let selectedKeyId = $state("");
    let newKeyName = $state("");
    let isCreatingKey = $state(false);
    let streamError = $state("");

    let roomsList = $state<Room[]>([]);
    let roomName = $state("");
    let roomSlug = $state("");
    let roomWaitingRoom = $state(true);
    let isCreatingRoom = $state(false);
    let roomError = $state("");
    let createdRoom = $state<Room | null>(null);
    let createdRoomSource = $state<"new" | "existing" | null>(null);

    let destroyed = false;
    let loadDataRequestId = 0;
    let refreshStatusRequestId = 0;
    let refreshConfigRequestId = 0;
    let healthRequestId = 0;
    let healthAbortController: AbortController | null = null;
    let turnSuccessTimer: ReturnType<typeof setTimeout> | null = null;
    let brandingSuccessTimer: ReturnType<typeof setTimeout> | null = null;

    const currentStepIndex = $derived(
        setupSteps.findIndex((s) => s.id === currentStep),
    );
    const progressPercent = $derived(setupProgressPercent(setupStatus));
    const selectedKey = $derived(keys.find((k) => k.id === selectedKeyId));
    const existingRoomSlug = $derived(setupStatus?.facts.firstRoomSlug ?? null);
    const activeRoomSlug = $derived(
        createdRoom?.slug ?? existingRoomSlug ?? "",
    );
    const activeRoomSource = $derived(
        createdRoom ? createdRoomSource : existingRoomSlug ? "existing" : null,
    );
    const brandingCheck = $derived(checkById(setupStatus, "branding"));

    onDestroy(() => {
        destroyed = true;
        healthAbortController?.abort();
        clearTurnSuccess();
        clearBrandingSuccess();
    });

    onMount(async () => {
        await loadData();
    });

    function getErrorMessage(e: unknown, fallback: string) {
        return e instanceof Error ? e.message : fallback;
    }

    function clearTimer(timer: ReturnType<typeof setTimeout> | null) {
        if (timer) {
            clearTimeout(timer);
        }
    }

    function clearTurnSuccess() {
        clearTimer(turnSuccessTimer);
        turnSuccessTimer = null;
        turnSuccess = "";
    }

    function showTurnSuccess(message: string, durationMs = 3000) {
        clearTurnSuccess();
        turnSuccess = message;
        turnSuccessTimer = setTimeout(() => {
            turnSuccessTimer = null;
            if (!destroyed) {
                turnSuccess = "";
            }
        }, durationMs);
    }

    function clearBrandingSuccess() {
        clearTimer(brandingSuccessTimer);
        brandingSuccessTimer = null;
        brandingSuccess = "";
    }

    function showBrandingSuccess(message: string, durationMs = 3000) {
        clearBrandingSuccess();
        brandingSuccess = message;
        brandingSuccessTimer = setTimeout(() => {
            brandingSuccessTimer = null;
            if (!destroyed) {
                brandingSuccess = "";
            }
        }, durationMs);
    }

    // A step is complete when every required backend check mapped to it is
    // ready. Branding has no required checks, so it is always "done" for the
    // sidebar (it is optional and never blocks completion).
    function stepComplete(stepId: SetupStepId): boolean {
        if (!setupStatus) return false;
        const required = setupStatus.checks.filter(
            (c) => c.required && stepForCheck(c.id) === stepId,
        );
        if (required.length === 0) return true;
        return required.every((c) => c.status === "ready");
    }

    async function loadData() {
        const requestId = ++loadDataRequestId;

        loadError = "";

        try {
            const [statusData, configData, keysData, roomsData] =
                await Promise.all([
                    setup.status(),
                    appConfig.get(),
                    streamKeys.list(),
                    rooms.list(),
                ]);
            if (destroyed || requestId !== loadDataRequestId) return;

            setupStatus = statusData;
            config = configData;
            baseUrl =
                configData.publicUrl ||
                (typeof window !== "undefined"
                    ? window.location.origin
                    : "");

            keys = keysData ?? [];
            roomsList = roomsData ?? [];

            if (!selectedKeyId && keysData.length > 0) {
                selectedKeyId = keysData[0].id;
            }

            if (!createdRoom && roomsData.length > 0) {
                createdRoom = roomsData[0];
                createdRoomSource = "existing";
            }

            if (!watermarkText) {
                watermarkText =
                    configData.defaultWatermarkText || DEFAULT_WATERMARK_TEXT;
            }

            turnUrl = configData.turnExternalUrl || "";
            turnUsername = configData.turnExternalUsername || "";

            await checkHealth();
            if (destroyed || requestId !== loadDataRequestId) return;
            currentStep = nextSetupStep(
                setupStatus,
                healthStatus === "ok",
            );
        } catch (e) {
            if (destroyed || requestId !== loadDataRequestId) return;
            loadError = getErrorMessage(e, "Failed to load setup data");
        } finally {
            if (!destroyed && requestId === loadDataRequestId) {
                isLoading = false;
            }
        }
    }

    // Re-fetch the server setup status after any mutating action so the checks
    // reflect the new state. Preserves the current step unless it is now
    // complete and the next required action is later in the flow.
    async function refreshSetupStatus() {
        const requestId = ++refreshStatusRequestId;
        try {
            const next = await setup.status();
            if (destroyed || requestId !== refreshStatusRequestId) return;
            setupStatus = next;
            if (stepComplete(currentStep)) {
                const target = nextSetupStep(next, healthStatus === "ok");
                const targetIndex = setupSteps.findIndex(
                    (s) => s.id === target,
                );
                if (targetIndex > currentStepIndex) {
                    currentStep = target;
                }
            }
        } catch (e) {
            console.error("Failed to refresh setup status", e);
        }
    }

    async function refreshConfig() {
        const requestId = ++refreshConfigRequestId;
        try {
            const nextConfig = await appConfig.get();
            if (destroyed || requestId !== refreshConfigRequestId) return;

            config = nextConfig;
            baseUrl =
                config?.publicUrl ||
                (typeof window !== "undefined"
                    ? window.location.origin
                    : "");
        } catch (e) {
            console.error("Failed to refresh config", e);
        }
    }

    async function checkHealth() {
        const requestId = ++healthRequestId;
        healthAbortController?.abort();
        const controller = new AbortController();
        healthAbortController = controller;
        healthStatus = "checking";
        healthMessage = "";

        try {
            const res = await fetch("/health", {
                cache: "no-store",
                signal: controller.signal,
            });
            if (destroyed || requestId !== healthRequestId) return;

            if (!res.ok) {
                healthStatus = "error";
                healthMessage = `HTTP ${res.status}`;
                return;
            }
            const data = await res.json();
            if (destroyed || requestId !== healthRequestId) return;

            if (data?.status === "ok") {
                healthStatus = "ok";
            } else {
                healthStatus = "error";
                healthMessage = "Unexpected response";
            }
        } catch (e) {
            if (destroyed || requestId !== healthRequestId) return;
            if (e instanceof DOMException && e.name === "AbortError") return;

            healthStatus = "error";
            healthMessage = getErrorMessage(e, "Health check failed");
        } finally {
            if (healthAbortController === controller) {
                healthAbortController = null;
            }
        }
    }

    function healthTone(): "checking" | "good" | "bad" {
        if (healthStatus === "checking") return "checking";
        return healthStatus === "ok" ? "good" : "bad";
    }

    function healthLabel(): string {
        if (healthStatus === "checking") return "Checking";
        return healthStatus === "ok" ? "Healthy" : "Needs attention";
    }

    function goNext() {
        const next = currentStepIndex + 1;
        if (next < setupSteps.length) {
            currentStep = setupSteps[next].id;
        }
    }

    function goBack() {
        const prev = currentStepIndex - 1;
        if (prev >= 0) {
            currentStep = setupSteps[prev].id;
        }
    }

    async function skipSetup() {
        if (isDismissingSetup) return;
        isDismissingSetup = true;
        try {
            await setup.dismiss();
        } catch (e) {
            console.error("Failed to dismiss setup", e);
        } finally {
            if (!destroyed) {
                isDismissingSetup = false;
            }
        }
        goto("/admin");
    }

    function exitSetup() {
        goto("/admin");
    }

    async function finishSetup() {
        if (isCompletingSetup || !setupStatus?.readyToComplete) return;
        isCompletingSetup = true;
        setupError = "";
        try {
            await setup.complete();
            if (destroyed) return;
            goto("/admin");
        } catch (e) {
            if (destroyed) return;
            if (e instanceof SetupIncompleteError) {
                setupStatus = e.statusResponse;
                setupError =
                    "Finish the required checks before completing setup.";
            } else {
                setupError = getErrorMessage(e, "Failed to complete setup");
            }
        } finally {
            if (!destroyed) {
                isCompletingSetup = false;
            }
        }
    }

    async function handleSaveTurn(e: SubmitEvent) {
        e.preventDefault();
        isSavingTurn = true;
        turnError = "";
        clearTurnSuccess();

        try {
            const normalizedTurnURL = turnUrl
                .split(",")
                .map((entry) => entry.trim())
                .filter(Boolean)
                .join(",");
            const turnCredentialPayload = clearTurnCredential
                ? ""
                : turnCredential || undefined;

            const nextConfig = await appConfig.update({
                turnExternalUrl: normalizedTurnURL,
                turnExternalUsername: turnUsername.trim(),
                turnExternalCredential: turnCredentialPayload,
            });
            if (destroyed) return;

            config = nextConfig;
            baseUrl =
                config?.publicUrl ||
                (typeof window !== "undefined"
                    ? window.location.origin
                    : "");
            turnUrl = config?.turnExternalUrl || "";
            turnUsername = config?.turnExternalUsername || "";
            // Leave the credential field empty after save. The backend clears the
            // stale reachability-test signature, so refreshSetupStatus() will
            // show turn-connectivity as needing a new test.
            turnCredential = "";
            clearTurnCredential = false;
            showTurnSuccess("TURN settings saved");
            await refreshSetupStatus();
        } catch (e) {
            if (destroyed) return;
            turnError = getErrorMessage(e, "Failed to save TURN settings");
        } finally {
            if (!destroyed) {
                isSavingTurn = false;
            }
        }
    }

    async function handleTestTurn() {
        isTestingTurn = true;
        turnTestResults = null;
        turnError = "";
        clearTurnSuccess();

        try {
            const nextResults = await appConfig.testTurn();
            if (destroyed) return;

            turnTestResults = nextResults;
            if (nextResults.success) {
                showTurnSuccess("TURN reachability test passed", 4000);
            } else {
                turnError = nextResults.message || "TURN test failed";
            }
            await refreshSetupStatus();
        } catch (e) {
            if (destroyed) return;
            turnError = getErrorMessage(
                e,
                "Failed to test TURN reachability",
            );
        } finally {
            if (!destroyed) {
                isTestingTurn = false;
            }
        }
    }

    async function handleUseDefaultWatermark() {
        isSavingBranding = true;
        brandingError = "";
        clearBrandingSuccess();
        try {
            watermarkText = DEFAULT_WATERMARK_TEXT;
            const nextConfig = await appConfig.update({
                defaultWatermarkText: DEFAULT_WATERMARK_TEXT,
            });
            if (destroyed) return;
            config = nextConfig;
            showBrandingSuccess("Default watermark applied");
            await refreshSetupStatus();
        } catch (e) {
            if (destroyed) return;
            brandingError = getErrorMessage(
                e,
                "Failed to apply default watermark",
            );
        } finally {
            if (!destroyed) {
                isSavingBranding = false;
            }
        }
    }

    async function handleSaveWatermark(e: SubmitEvent) {
        e.preventDefault();
        isSavingBranding = true;
        brandingError = "";
        clearBrandingSuccess();

        try {
            const nextConfig = await appConfig.update({
                defaultWatermarkText: watermarkText || undefined,
            });
            if (destroyed) return;

            config = nextConfig;
            showBrandingSuccess("Watermark saved");
            await refreshSetupStatus();
        } catch (e) {
            if (destroyed) return;
            brandingError = getErrorMessage(e, "Failed to save watermark");
        } finally {
            if (!destroyed) {
                isSavingBranding = false;
            }
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
        clearBrandingSuccess();

        try {
            await appConfig.uploadLogo(file);
            if (destroyed) return;
            await refreshConfig();
            if (destroyed) return;

            showBrandingSuccess("Logo uploaded");
            await refreshSetupStatus();
        } catch (e) {
            if (destroyed) return;
            brandingError = getErrorMessage(e, "Failed to upload logo");
        } finally {
            if (!destroyed) {
                isUploadingLogo = false;
                input.value = "";
            }
        }
    }

    async function handleDeleteLogo() {
        confirmDeleteLogoOpen = false;
        brandingError = "";
        clearBrandingSuccess();

        try {
            await appConfig.deleteLogo();
            if (destroyed) return;
            await refreshConfig();
            if (destroyed) return;

            showBrandingSuccess("Logo removed");
            await refreshSetupStatus();
        } catch (e) {
            if (destroyed) return;
            brandingError = getErrorMessage(e, "Failed to delete logo");
        }
    }

    async function handleCreateKey(e: SubmitEvent) {
        e.preventDefault();
        if (!newKeyName.trim()) return;

        isCreatingKey = true;
        streamError = "";

        try {
            const key = await streamKeys.create(newKeyName.trim());
            if (destroyed) return;

            keys = [...keys, key];
            selectedKeyId = key.id;
            newKeyName = "";
            await refreshSetupStatus();
        } catch (e) {
            if (destroyed) return;
            streamError = getErrorMessage(e, "Failed to create stream key");
        } finally {
            if (!destroyed) {
                isCreatingKey = false;
            }
        }
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
                    watermarkText || DEFAULT_WATERMARK_TEXT;
            }

            const room = await rooms.create(payload);
            if (destroyed) return;

            createdRoom = room;
            createdRoomSource = "new";
            roomsList = [room, ...roomsList];
            await refreshSetupStatus();
        } catch (e) {
            if (destroyed) return;
            roomError = getErrorMessage(e, "Failed to create room");
        } finally {
            if (!destroyed) {
                isCreatingRoom = false;
            }
        }
    }

    function roomUrl() {
        if (!activeRoomSlug) return "";
        return `${baseUrl}/room/${activeRoomSlug}`;
    }

    function adminRoomUrl() {
        if (!activeRoomSlug) return "";
        return `/admin/rooms/${activeRoomSlug}`;
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
                Follow the guided flow to verify infrastructure, configure TURN,
                brand your rooms, and start your first stream.
            </p>
        </div>
        <div class="hero-actions">
            <button class="btn btn-ghost" onclick={exitSetup}>Exit</button>
            <button
                class="btn btn-secondary"
                onclick={skipSetup}
                disabled={isDismissingSetup}
            >
                {#if isDismissingSetup}
                    <span class="btn-spinner" aria-hidden="true"></span>
                    Skipping...
                {:else}
                    Skip for now
                {/if}
            </button>
        </div>
    </header>

    {#if isLoading}
        <div class="setup-skeletons" aria-busy="true" aria-label="Loading setup">
            <div class="skeleton skeleton-progress"></div>
            <div class="skeleton skeleton-card"></div>
            <div class="skeleton skeleton-card"></div>
        </div>
    {:else if loadError}
        <div class="alert alert-error">{loadError}</div>
        <div class="button-row">
            <button class="btn btn-secondary" onclick={loadData}>Retry</button>
        </div>
    {:else}
        <section class="setup-progress card">
            <div class="progress-meta">
                <span>Progress</span>
                <span>{progressPercent}% ready</span>
            </div>
            <div class="progress-bar">
                <div
                    class="progress-fill"
                    style="width: {progressPercent}%"
                ></div>
            </div>
        </section>

        <div class="setup-body">
            <aside class="setup-steps">
                <ol>
                    {#each setupSteps as step, index (step.id)}
                        <li
                            class:active={step.id === currentStep}
                            class:complete={stepComplete(step.id)}
                        >
                            <button
                                class="step-button"
                                onclick={() => (currentStep = step.id)}
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
                {#if currentStep === "preflight"}
                    <div class="step-panel">
                        <div class="panel-header">
                            <h2>Preflight checks</h2>
                            <p>
                                These are computed from your running Chromatic
                                server. No manual checkboxes &mdash; fix the
                                flagged items in your <code>.env</code> and
                                restart.
                            </p>
                        </div>

                        <div class="status-grid">
                            <div class="status-card">
                                <div class="status-header">
                                    <span class="status-label">Health</span>
                                    <StatusPill
                                        tone={healthTone()}
                                        label={healthLabel()}
                                    />
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

                            {#each ["public-url", "security"] as checkId (checkId)}
                                {@const c = checkById(setupStatus, checkId)}
                                <div class="status-card">
                                    <div class="status-header">
                                        <span class="status-label"
                                            >{c?.title ?? checkId}</span
                                        >
                                        <StatusPill
                                            tone={setupCheckTone(c)}
                                            label={c?.summary ?? "Checking"}
                                        />
                                    </div>
                                    {#if c?.detail}
                                        <p class="status-detail">{c.detail}</p>
                                    {/if}
                                    {#if c?.action}
                                        <p class="status-note status-action">
                                            {c.action}
                                        </p>
                                    {/if}
                                </div>
                            {/each}
                        </div>

                        <div class="status-card">
                            <div class="status-header">
                                <span class="status-label">WHIP</span>
                                <StatusPill tone="neutral" label="Format" />
                            </div>
                            <p class="status-detail">
                                {config?.whipFormat || "Not configured yet"}
                            </p>
                            <p class="status-note">
                                Stream keys replace
                                <code>{"{stream_key_token}"}</code>.
                            </p>
                        </div>
                    </div>
                {:else if currentStep === "turn"}
                    <div class="step-panel">
                        <div class="panel-header">
                            <h2>Connectivity and TURN</h2>
                            <p>
                                Cloudflare TURN is the recommended default for
                                non-self-hosted deployments. Add static TURN
                                fallback only when needed.
                            </p>
                        </div>

                        <div class="status-grid">
                            {#each ["turn-config", "turn-connectivity"] as checkId (checkId)}
                                {@const c = checkById(setupStatus, checkId)}
                                <div class="status-card">
                                    <div class="status-header">
                                        <span class="status-label"
                                            >{c?.title ?? checkId}</span
                                        >
                                        <StatusPill
                                            tone={setupCheckTone(c)}
                                            label={c?.summary ?? "Checking"}
                                        />
                                    </div>
                                    {#if c?.detail}
                                        <p class="status-detail">{c.detail}</p>
                                    {/if}
                                    {#if c?.action}
                                        <p class="status-note status-action">
                                            {c.action}
                                        </p>
                                    {/if}
                                </div>
                            {/each}
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
                                            <StatusPill
                                                tone={config?.turnCloudflareConfigured
                                                    ? "good"
                                                    : "warn"}
                                                label={config?.turnCloudflareConfigured
                                                    ? "Configured"
                                                    : "Not configured"}
                                            />
                                        </div>
                                        <p class="status-detail">
                                            {config?.turnCloudflareConfigured
                                                ? "Credentials are generated on-demand; no static username/password needed."
                                                : "Set TURN_CLOUDFLARE_KEY_ID and TURN_CLOUDFLARE_API_TOKEN in .env for the recommended setup."}
                                        </p>
                                    </div>
                                </div>

                                {#if turnError}
                                    <div
                                        class="alert alert-error"
                                        transition:fade={{ duration: 150 }}
                                    >
                                        {turnError}
                                    </div>
                                {/if}

                                {#if turnSuccess}
                                    <div
                                        class="alert alert-success"
                                        transition:fade={{ duration: 150 }}
                                    >
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
                                            oninput={() =>
                                                (clearTurnCredential = false)}
                                            placeholder={config?.hasTurnCredential
                                                ? clearTurnCredential
                                                    ? "Will be cleared on save"
                                                    : "********"
                                                : "TURN credential"}
                                        />
                                        {#if config?.hasTurnCredential}
                                            <p class="hint">
                                                A credential is already set.
                                                Enter a new value to rotate it
                                                or clear it explicitly.
                                            </p>
                                            <div class="button-row">
                                                <button
                                                    type="button"
                                                    class="btn btn-secondary btn-sm"
                                                    onclick={() => {
                                                        clearTurnCredential =
                                                            !clearTurnCredential;
                                                        if (
                                                            clearTurnCredential
                                                        ) {
                                                            turnCredential = "";
                                                        }
                                                    }}
                                                >
                                                    {clearTurnCredential
                                                        ? "Credential Will Be Cleared"
                                                        : "Clear Saved Credential"}
                                                </button>
                                            </div>
                                        {/if}
                                    </div>

                                    <div class="button-row">
                                        <button
                                            type="submit"
                                            class="btn btn-primary"
                                            disabled={isSavingTurn}
                                        >
                                            {#if isSavingTurn}
                                                <span class="btn-spinner" aria-hidden="true"></span>
                                                Saving...
                                            {:else}
                                                Save TURN
                                            {/if}
                                        </button>
                                        <button
                                            type="button"
                                            class="btn btn-secondary"
                                            onclick={handleTestTurn}
                                            disabled={isTestingTurn}
                                        >
                                            {#if isTestingTurn}
                                                <span class="btn-spinner" aria-hidden="true"></span>
                                                Testing...
                                            {:else}
                                                Test TURN reachability
                                            {/if}
                                        </button>
                                    </div>
                                </form>
                            </section>

                            <section class="card">
                                <h3>Server reachability results</h3>
                                <p class="section-description">
                                    Run a quick socket test to confirm TURN is
                                    reachable from this Chromatic server.
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
                                                                <StatusPill
                                                                    tone="good"
                                                                    label="Reachable"
                                                                />
                                                            {:else}
                                                                <StatusPill
                                                                    tone="bad"
                                                                    label="Failed"
                                                                />
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

                                <p class="hint reachability-caveat">
                                    This checks whether the Chromatic server can
                                    reach the TURN endpoint. A live viewer test
                                    still verifies the viewer's NAT path.
                                </p>
                            </section>
                        </div>
                    </div>
                {:else if currentStep === "branding"}
                    <div class="step-panel">
                        <div class="panel-header">
                            <h2>Branding and watermark</h2>
                            <p>
                                Optional. Set defaults for new rooms &mdash; you
                                can override these per room later.
                            </p>
                        </div>


                        <div class="status-card">
                            <div class="status-header">
                                <span class="status-label"
                                    >Default branding</span
                                >
                                <StatusPill
                                    tone={setupCheckTone(brandingCheck)}
                                    label={brandingCheck?.summary ??
                                        "Default branding can be set later."}
                                />
                            </div>
                        </div>

                        <div class="split-grid">
                            <section class="card">
                                <h3>Default watermark text</h3>
                                <p class="section-description">
                                    Apply the recommended default or customize
                                    it now.
                                </p>

                                {#if brandingError}
                                    <div
                                        class="alert alert-error"
                                        transition:fade={{ duration: 150 }}
                                    >
                                        {brandingError}
                                    </div>
                                {/if}

                                {#if brandingSuccess}
                                    <div
                                        class="alert alert-success"
                                        transition:fade={{ duration: 150 }}
                                    >
                                        {brandingSuccess}
                                    </div>
                                {/if}

                                <div class="button-row">
                                    <button
                                        type="button"
                                        class="btn btn-secondary btn-sm"
                                        onclick={handleUseDefaultWatermark}
                                        disabled={isSavingBranding}
                                    >
                                        Use default watermark
                                    </button>
                                </div>

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
                                            placeholder={DEFAULT_WATERMARK_TEXT}
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
                                        {#if isSavingBranding}
                                            <span class="btn-spinner" aria-hidden="true"></span>
                                            Saving...
                                        {:else}
                                            Save Watermark
                                        {/if}
                                    </button>
                                </form>
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
                                            onclick={() =>
                                                (confirmDeleteLogoOpen = true)}
                                        >
                                            Delete
                                        </button>
                                    </div>
                                {/if}

                                <div class="file-upload">
                                    <label
                                        class="btn btn-secondary file-btn"
                                        class:disabled={isUploadingLogo}
                                    >
                                        {#if isUploadingLogo}
                                            <span class="btn-spinner" aria-hidden="true"></span>
                                            Uploading...
                                        {:else}
                                            Choose Logo File
                                        {/if}
                                        <input
                                            type="file"
                                            class="visually-hidden-input"
                                            accept="image/png,image/jpeg,image/webp"
                                            onchange={handleLogoUpload}
                                            disabled={isUploadingLogo}
                                        />
                                    </label>
                                </div>
                                <p class="hint">
                                    Recommended: transparent PNG, max 500x500px,
                                    max 1MB.
                                </p>
                            </section>
                        </div>
                    </div>
                {:else if currentStep === "stream"}
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
                                    <div
                                        class="alert alert-error"
                                        transition:fade={{ duration: 150 }}
                                    >
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
                                        {#if isCreatingKey}
                                            <span class="btn-spinner" aria-hidden="true"></span>
                                            Creating...
                                        {:else}
                                            Create Key
                                        {/if}
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

                                {#if selectedKey}
                                    <CopyField
                                        label="WHIP URL"
                                        value={`${baseUrl}/whip/${selectedKey.keyToken}`}
                                    />
                                    <p class="hint">
                                        In OBS, paste this into <strong>Server</strong> and leave <strong>Bearer Token empty</strong>.
                                    </p>
                                {/if}
                            </section>

                            <section class="card">
                                <h3>OBS settings (critical)</h3>
                                <p class="section-description">
                                    Requires OBS 30 or newer (WHIP output). Late
                                    joiners wait for the next keyframe, so the
                                    keyframe interval matters most.
                                </p>
                                <ol class="obs-steps">
                                    <li>
                                        <span class="obs-step-title">
                                            Settings &rarr; Stream
                                        </span>
                                        <ul class="obs-settings">
                                            <li>
                                                <span>Service</span>
                                                <strong>WHIP</strong>
                                            </li>
                                            <li>
                                                <span>Server</span>
                                                <strong>WHIP URL (left)</strong>
                                            </li>
                                            <li>
                                                <span>Bearer Token</span>
                                                <strong
                                                    >empty (key is in the
                                                    URL)</strong
                                                >
                                            </li>
                                        </ul>
                                    </li>
                                    <li>
                                        <span class="obs-step-title">
                                            Settings &rarr; Output &mdash; set
                                            Output Mode to Advanced, Streaming
                                            tab
                                        </span>
                                        <ul class="obs-settings">
                                            <li>
                                                <span>Encoder</span>
                                                <strong
                                                    >x264 or hardware H.264
                                                    (NVENC / AMF / QSV)</strong
                                                >
                                            </li>
                                            <li>
                                                <span>Rate Control</span>
                                                <strong>CBR</strong>
                                            </li>
                                            <li>
                                                <span>Bitrate</span>
                                                <strong
                                                    >8000&ndash;12000 Kbps for
                                                    1080p48; 10000&ndash;15000
                                                    Kbps for 1080p60</strong
                                                >
                                            </li>
                                            <li>
                                                <span>Keyframe Interval</span>
                                                <strong
                                                    >1 s (2 s max) &mdash;
                                                    faster viewer join</strong
                                                >
                                            </li>
                                            <li>
                                                <span>Profile</span>
                                                <strong
                                                    >baseline (required; Main /
                                                    High are rejected)</strong
                                                >
                                            </li>
                                            <li>
                                                <span>Tune (x264)</span>
                                                <strong>zerolatency</strong>
                                            </li>
                                            <li>
                                                <span>B-frames (hardware)</span>
                                                <strong>0</strong>
                                            </li>
                                        </ul>
                                    </li>
                                    <li>
                                        <span class="obs-step-title">
                                            Settings &rarr; Video
                                        </span>
                                        <ul class="obs-settings">
                                            <li>
                                                <span>Output Resolution</span>
                                                <strong>1920x1080</strong>
                                            </li>
                                            <li>
                                                <span>FPS</span>
                                                <strong
                                                    >48/47.952 for clean 24p;
                                                    60/59.94 for lowest
                                                    latency</strong
                                                >
                                            </li>
                                        </ul>
                                        <div class="preset-grid">
                                            <div class="preset">
                                                <strong>Color-review cadence</strong>
                                                <span>1080p48 or 47.952, superfast, 8-12 Mbps. Each 24p frame repeats evenly.</span>
                                            </div>
                                            <div class="preset">
                                                <strong>Lowest latency</strong>
                                                <span>1080p60 or 59.94, superfast/ultrafast, 10-15 Mbps. Faster browser playout, possible 24p judder.</span>
                                            </div>
                                        </div>
                                    </li>
                                    <li>
                                        <span class="obs-step-title">
                                            Settings &rarr; Advanced &rarr;
                                            Video
                                        </span>
                                        <ul class="obs-settings">
                                            <li>
                                                <span>Color Format</span>
                                                <strong>NV12</strong>
                                            </li>
                                            <li>
                                                <span>Color Space</span>
                                                <strong>sRGB</strong>
                                            </li>
                                            <li>
                                                <span>Color Range</span>
                                                <strong>Limited</strong>
                                            </li>
                                        </ul>
                                        <p class="hint">
                                            sRGB matches how browsers render
                                            video on every platform. Rec. 709
                                            looks washed out on macOS, which
                                            displays 709-tagged video with a
                                            1.96 gamma.
                                        </p>
                                    </li>
                                </ol>
                            </section>
                        </div>
                    </div>
                {:else if currentStep === "room"}
                    <div class="step-panel">
                        <div class="panel-header">
                            <h2>Create your first room</h2>
                            <p>
                                This room is where you will invite viewers and
                                verify the stream.
                            </p>
                        </div>

                        {#if activeRoomSlug}
                            <section class="card existing-room-card">
                                <h3>
                                    {#if activeRoomSource === "existing"}
                                        Existing room detected
                                    {:else}
                                        Room created successfully
                                    {/if}
                                </h3>
                                <p class="section-description">
                                    Existing rooms count as ready &mdash; no need
                                    to create a duplicate.
                                </p>
                                <CopyField
                                    label="Viewer URL"
                                    value={roomUrl()}
                                />
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
                            </section>
                        {/if}

                        <div class="split-grid">
                            <section class="card">
                                <h3>Room details</h3>
                                {#if roomError}
                                    <div
                                        class="alert alert-error"
                                        transition:fade={{ duration: 150 }}
                                    >
                                        {roomError}
                                    </div>
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
                                        {#if isCreatingRoom}
                                            <span class="btn-spinner" aria-hidden="true"></span>
                                            Creating...
                                        {:else}
                                            Create Room
                                        {/if}
                                    </button>
                                </form>
                            </section>

                            <section class="card">
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
                {:else if currentStep === "finish"}
                    <div class="step-panel">
                        <div class="panel-header">
                            <h2>Finish setup</h2>
                            <p>
                                Review the required checks before completing
                                setup.
                            </p>
                        </div>

                        {#if setupError}
                            <div
                                class="alert alert-error"
                                transition:fade={{ duration: 150 }}
                            >
                                {setupError}
                            </div>
                        {/if}

                        {#if setupStatus?.readyToComplete}
                            <div class="card ready-card">
                                <h3>Chromatic is ready for the first stream</h3>
                                <p class="section-description">
                                    Every required check passed. Complete setup
                                    to dismiss this wizard.
                                </p>
                                <button
                                    class="btn btn-primary"
                                    onclick={finishSetup}
                                    disabled={isCompletingSetup}
                                >
                                    {#if isCompletingSetup}
                                        <span class="btn-spinner" aria-hidden="true"></span>
                                        Completing...
                                    {:else}
                                        Finish setup
                                    {/if}
                                </button>
                            </div>
                        {:else}
                            <div class="card">
                                <h3>Required checks still need action</h3>
                                <ul class="missing-list">
                                    {#each requiredMissingChecks(setupStatus) as c (c.id)}
                                        <li>
                                            <div class="missing-row">
                                                <StatusPill
                                                    tone="bad"
                                                    label={c.title}
                                                />
                                                <span class="missing-summary"
                                                    >{c.summary}</span
                                                >
                                            </div>
                                            {#if c.action}
                                                <p class="status-action">
                                                    {c.action}
                                                </p>
                                            {/if}
                                        </li>
                                    {/each}
                                </ul>
                                <button
                                    class="btn btn-primary"
                                    disabled
                                >
                                    Finish setup
                                </button>
                            </div>
                        {/if}
                    </div>
                {/if}
            </section>
        </div>

        <footer class="setup-footer">
            <button
                class="btn btn-secondary"
                onclick={goBack}
                disabled={currentStepIndex === 0}
            >
                Back
            </button>
            {#if currentStep !== "finish"}
                <button class="btn btn-primary" onclick={goNext}>Next</button>
            {/if}
        </footer>
    {/if}
</div>

<ConfirmDialog
    open={confirmDeleteLogoOpen}
    title="Delete the default logo?"
    body="Rooms using the default watermark logo will no longer display it."
    confirmLabel="Delete Logo"
    danger
    onConfirm={handleDeleteLogo}
    onCancel={() => (confirmDeleteLogoOpen = false)}
/>

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
            color-mix(in srgb, var(--color-primary) 12%, transparent),
            var(--color-surface)
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
            color-mix(in srgb, var(--color-primary) 22%, transparent),
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

    .setup-skeletons {
        display: flex;
        flex-direction: column;
        gap: var(--space-lg);
    }

    .skeleton-progress {
        height: 60px;
        border-radius: var(--radius-md);
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
        background: var(--color-primary);
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
        background: var(--color-success-bg);
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

    .status-action {
        color: var(--color-text-muted);
        font-size: 0.8125rem;
    }

    .section-description {
        color: var(--color-text-muted);
        font-size: 0.875rem;
        margin-bottom: var(--space-md);
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

    .reachability-caveat {
        margin-top: var(--space-md);
    }

    .button-row {
        display: flex;
        gap: var(--space-sm);
        flex-wrap: wrap;
        margin-top: var(--space-md);
    }

    .test-summary {
        padding: var(--space-sm) var(--space-md);
        border-radius: var(--radius-md);
        font-size: 0.875rem;
        margin-bottom: var(--space-md);
    }

    .test-summary.success {
        background: var(--color-success-bg);
        border: 1px solid var(--color-success);
        color: var(--color-success);
    }

    .test-summary.failure {
        background: var(--color-error-bg);
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
        vertical-align: top;
    }

    /* Let long TURN URLs wrap instead of being cut off */
    .test-results-table td:first-child code {
        display: block;
        word-break: break-all;
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

    .file-btn {
        position: relative;
        overflow: hidden;
    }

    .file-btn.disabled {
        opacity: 0.55;
        cursor: not-allowed;
    }

    .visually-hidden-input {
        position: absolute;
        width: 1px;
        height: 1px;
        padding: 0;
        margin: -1px;
        overflow: hidden;
        clip: rect(0, 0, 0, 0);
        white-space: nowrap;
        border: 0;
    }

    .obs-steps {
        list-style: decimal;
        padding-left: var(--space-lg);
        display: grid;
        gap: var(--space-md);
        margin-bottom: var(--space-md);
        font-size: 0.875rem;
    }

    .obs-step-title {
        display: block;
        font-weight: 600;
        margin-bottom: var(--space-xs);
    }

    .obs-settings {
        list-style: none;
        padding: 0;
        display: grid;
    }

    .obs-settings li {
        display: flex;
        justify-content: space-between;
        align-items: baseline;
        gap: var(--space-md);
        padding: var(--space-xs) 0;
        border-bottom: 1px solid var(--color-border-subtle);
    }

    .obs-settings li:last-child {
        border-bottom: none;
    }

    .obs-settings li span {
        color: var(--color-text-muted);
        flex-shrink: 0;
    }

    .obs-settings li strong {
        font-weight: 500;
        text-align: right;
    }

    .preset-grid {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
        gap: var(--space-sm);
        margin-top: var(--space-md);
    }

    .preset {
        display: flex;
        flex-direction: column;
        gap: 3px;
        padding: var(--space-sm);
        border: 1px solid var(--color-border-subtle);
        border-radius: var(--radius-md);
        background: var(--color-surface-elevated);
    }

    .preset strong {
        font-size: 0.8125rem;
    }

    .preset span {
        font-size: 0.75rem;
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

    .callout {
        margin-top: var(--space-lg);
        padding: var(--space-md);
        border-radius: var(--radius-md);
        background: color-mix(in srgb, var(--color-primary) 12%, transparent);
        border: 1px solid color-mix(in srgb, var(--color-primary) 30%, transparent);
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

    .existing-room-card {
        border-color: var(--color-primary);
    }

    .ready-card h3 {
        margin-bottom: var(--space-sm);
    }

    .missing-list {
        list-style: none;
        padding: 0;
        display: flex;
        flex-direction: column;
        gap: var(--space-md);
        margin-bottom: var(--space-md);
    }

    .missing-row {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        flex-wrap: wrap;
    }

    .missing-summary {
        color: var(--color-text);
        font-size: 0.875rem;
    }

    .setup-footer {
        display: flex;
        justify-content: space-between;
        gap: var(--space-md);
        padding-top: var(--space-md);
        border-top: 1px solid var(--color-border-subtle);
    }

    .btn-danger-text {
        color: var(--color-error);
    }

    .btn-danger-text:hover {
        background: var(--color-error-bg);
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
