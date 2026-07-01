<script lang="ts">
    import { onDestroy } from "svelte";
    import { fade, fly, scale } from "svelte/transition";
    import { quintOut } from "svelte/easing";
    import { session } from "$lib/stores/session.svelte";
    import { chatStore } from "$lib/stores/chat.svelte";
    import { uploadFile, type UploadedFile } from "$lib/api/client";
    import { playChatSentChime } from "$lib/audio/chimes";
    import AudioMessage from "$lib/components/AudioMessage.svelte";

    interface Props {
        isOpen: boolean;
        onClose: () => void;
        roomSlug: string;
        /** Map of participant id → display color (tints author names). */
        participantColors?: Record<string, string>;
        /** Own participant id, used to distinguish own messages. */
        selfId?: string;
        /** Admins can delete messages (moderation). */
        canModerate?: boolean;
        /** Participants currently typing (maintained by the page). */
        typing?: { id: string; name: string }[];
    }

    let {
        isOpen,
        onClose,
        roomSlug,
        participantColors = {},
        selfId = "",
        canModerate = false,
        typing = [],
    }: Props = $props();

    // iOS: the on-screen keyboard overlays a fixed bottom sheet, hiding the
    // input. Track visualViewport so the mobile sheet rides above the keyboard.
    let keyboardInset = $state(0);
    $effect(() => {
        const vv = typeof window !== "undefined" ? window.visualViewport : null;
        if (!vv) return;
        const update = () => {
            keyboardInset = Math.max(0, window.innerHeight - vv.height - vv.offsetTop);
        };
        update();
        vv.addEventListener("resize", update);
        vv.addEventListener("scroll", update);
        return () => {
            vv.removeEventListener("resize", update);
            vv.removeEventListener("scroll", update);
        };
    });

    // 1: "A is typing" · 2: "A and B are typing" · 3: all three names ·
    // 4+: a count. The row itself is one fixed-height line that
    // ellipsizes, so any number of typers never shifts the transcript.
    let typingLabel = $derived.by(() => {
        const n = typing.length;
        if (n === 1) return `${typing[0].name} is typing`;
        if (n === 2) return `${typing[0].name} and ${typing[1].name} are typing`;
        if (n === 3) return `${typing[0].name}, ${typing[1].name} and ${typing[2].name} are typing`;
        return `${n} people are typing`;
    });

    function deleteMessage(messageId: string) {
        session.send("admin:delete-message", { messageId });
    }

    // Allowed file types (must match backend)
    const ALLOWED_TYPES = [
        "image/jpeg",
        "image/png",
        "image/gif",
        "image/webp",
        "audio/mpeg",
        "audio/wav",
        "audio/wave",
        "audio/x-wav",
        "audio/ogg",
        "application/ogg",
        "application/pdf",
    ];
    const MAX_FILE_SIZE = 5 * 1024 * 1024; // 5MB

    // Consecutive messages from the same author within this window render
    // as one visual group (header and avatar only on the first).
    const GROUP_WINDOW_MS = 3 * 60 * 1000;

    // Validate URLs to prevent javascript: and data: URL attacks
    function isSafeUrl(url: string | undefined): boolean {
        if (!url) return false;
        try {
            const parsed = new URL(url, window.location.origin);
            return parsed.protocol === "http:" || parsed.protocol === "https:";
        } catch {
            return false;
        }
    }

    function withJoinToken(url: string | undefined): string | undefined {
        return url;
    }

    let messageInput = $state("");
    let messagesContainer = $state<HTMLDivElement | null>(null);
    let fileInput = $state<HTMLInputElement | null>(null);
    let uploadProgress = $state<number | null>(null);
    let uploadName = $state("");
    let uploadError = $state<string | null>(null);
    let isDragOver = $state(false);
    let lightbox = $state<{
        url: string;
        name: string;
        kind: "image" | "pdf";
        /** Message id for images: the index into imageMessages is derived
         *  live, so deletions while the lightbox is open stay in sync. */
        id?: string;
    } | null>(null);

    // Lightbox owns Escape while open (capture phase beats the session
    // page's window handler, which would otherwise close the chat too).
    $effect(() => {
        if (!lightbox) return;
        const onKey = (e: KeyboardEvent) => {
            if (e.key === "Escape") {
                e.preventDefault();
                e.stopImmediatePropagation();
                lightbox = null;
            } else if (e.key === "ArrowLeft") {
                e.preventDefault();
                e.stopImmediatePropagation();
                navLightbox(-1);
            } else if (e.key === "ArrowRight") {
                e.preventDefault();
                e.stopImmediatePropagation();
                navLightbox(1);
            }
        };
        window.addEventListener("keydown", onKey, true);
        return () => window.removeEventListener("keydown", onKey, true);
    });

    // Chat message handlers are registered by the session page (always
    // mounted) and feed chatStore; this panel just renders the store.

    // "Pinned" is decided by where the user last SCROLLED to, not by
    // measuring after a message is appended (a tall message inflates
    // scrollHeight before the check and silently breaks follow mode).
    let pinnedToBottom = true;
    let autoScrolling = false;
    let autoScrollTimer: ReturnType<typeof setTimeout> | null = null;
    let autoScrollFrame: ReturnType<typeof requestAnimationFrame> | null = null;
    let uploadAbortController: AbortController | null = null;
    let destroyed = false;

    onDestroy(() => {
        destroyed = true;
        clearAutoScrollWork();
        uploadAbortController?.abort();
        uploadAbortController = null;
    });

    function clearAutoScrollWork() {
        if (autoScrollTimer) {
            clearTimeout(autoScrollTimer);
            autoScrollTimer = null;
        }
        if (autoScrollFrame !== null) {
            cancelAnimationFrame(autoScrollFrame);
            autoScrollFrame = null;
        }
    }

    function clearUploadState() {
        uploadProgress = null;
        uploadName = "";
    }

    function scrollToBottom(smooth = false) {
        if (!messagesContainer || destroyed) return;
        const container = messagesContainer;
        autoScrolling = true;
        // Safety: an interrupted smooth scroll never reaches the bottom,
        // which would leave the latch (and the pin) stuck true forever.
        if (autoScrollTimer) clearTimeout(autoScrollTimer);
        autoScrollTimer = setTimeout(() => {
            if (destroyed) return;
            autoScrolling = false;
            autoScrollTimer = null;
        }, 800);
        if (autoScrollFrame !== null) cancelAnimationFrame(autoScrollFrame);
        autoScrollFrame = requestAnimationFrame(() => {
            autoScrollFrame = null;
            if (destroyed || messagesContainer !== container) return;
            container.scrollTo({
                top: container.scrollHeight,
                behavior: smooth ? "smooth" : "auto",
            });
        });
    }

    // Explicit user scroll intent always overrides an in-flight auto-scroll
    function handleUserScrollIntent() {
        autoScrolling = false;
    }

    function handleMessagesScroll() {
        if (!messagesContainer) return;
        const c = messagesContainer;
        const near = c.scrollHeight - c.scrollTop - c.clientHeight < 48;
        if (near) autoScrolling = false;
        pinnedToBottom = near || autoScrolling;
    }

    /** Drawer reveal: the panel's width animates so the video reflows
     *  smoothly instead of jumping by 320px; content stays right-anchored
     *  at full width inside. On mobile the sheet slides up instead. */
    function drawer(node: HTMLElement) {
        const mobile =
            window.matchMedia("(max-width: 768px)").matches ||
            window.matchMedia(
                "(orientation: landscape) and (max-height: 480px) and (pointer: coarse)",
            ).matches;
        return {
            duration: 320,
            easing: quintOut,
            css: (t: number) =>
                mobile
                    ? `transform: translateY(${(1 - t) * 100}%)`
                    : `width: ${t * 320}px`,
        };
    }

    function handleSubmit(e: SubmitEvent) {
        e.preventDefault();
        if (!messageInput.trim()) return;

        session.send("chat:send", { content: messageInput.trim() });
        messageInput = "";
        playChatSentChime();
    }

    // Typing signal: at most one ping per 1.5s while there's input;
    // receivers expire it on their own.
    let lastTypingSentAt = 0;
    function handleTyping() {
        if (!messageInput.trim()) return;
        const now = Date.now();
        if (now - lastTypingSentAt < 1500) return;
        lastTypingSentAt = now;
        session.send("chat:typing", {});
    }

    function formatTime(timestamp: number): string {
        return new Date(timestamp).toLocaleTimeString([], {
            hour: "2-digit",
            minute: "2-digit",
        });
    }

    function formatFileSize(bytes: number): string {
        if (bytes < 1024) return `${bytes} B`;
        if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
        return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
    }

    function handleFileSelect() {
        fileInput?.click();
    }

    function handleFileInputChange(e: Event) {
        const input = e.target as HTMLInputElement;
        const file = input.files?.[0];
        if (file) {
            handleFileUpload(file);
        }
        // Reset input so same file can be selected again
        input.value = "";
    }

    async function handleFileUpload(file: File) {
        uploadAbortController?.abort();
        const controller = new AbortController();
        uploadAbortController = controller;
        uploadError = null;
        clearUploadState();

        // Validate file type
        if (!ALLOWED_TYPES.includes(file.type)) {
            uploadAbortController = null;
            uploadError = "File type not allowed. Use images, audio, or PDF.";
            return;
        }

        // Validate file size
        if (file.size > MAX_FILE_SIZE) {
            uploadAbortController = null;
            uploadError = `File too large. Maximum size is ${formatFileSize(MAX_FILE_SIZE)}.`;
            return;
        }

        uploadProgress = 0;
        uploadName = file.name;

        try {
            const uploadedFile = await uploadFile(
                roomSlug,
                file,
                (progress) => {
                    if (uploadAbortController !== controller) return;
                    uploadProgress = progress;
                },
                controller.signal
            );

            if (destroyed || uploadAbortController !== controller) return;

            // Send chat:file message via WebSocket
            session.send("chat:file", {
                fileId: uploadedFile.id,
                name: uploadedFile.originalName,
                mimeType: uploadedFile.mimeType,
                url: uploadedFile.url,
                thumbnailUrl: uploadedFile.thumbnailUrl,
            });
            playChatSentChime();

            uploadProgress = null;
        } catch (err) {
            if (destroyed || uploadAbortController !== controller) return;
            uploadError = err instanceof Error ? err.message : "Upload failed";
            uploadProgress = null;
        } finally {
            if (uploadAbortController === controller) {
                uploadAbortController = null;
            }
            if (!destroyed && uploadAbortController === null && uploadProgress === null) {
                uploadName = "";
            }
        }
    }

    function handleDragOver(e: DragEvent) {
        e.preventDefault();
        isDragOver = true;
    }

    function handleDragLeave(e: DragEvent) {
        e.preventDefault();
        isDragOver = false;
    }

    function handleDrop(e: DragEvent) {
        e.preventDefault();
        isDragOver = false;

        const file = e.dataTransfer?.files?.[0];
        if (file) {
            handleFileUpload(file);
        }
    }

    $effect(() => {
        if (isOpen) {
            chatStore.setVisible(true);
            pinnedToBottom = true;
            scrollToBottom();
        } else {
            chatStore.setVisible(false);
        }
    });

    let messages = $derived(chatStore.messages);
    let unreadCount = $derived(chatStore.unreadCount);
    // Every image in the transcript, in order: the lightbox pages
    // through these with the arrows / arrow keys.
    let imageMessages = $derived(
        messages.filter(
            (m) =>
                m.type === "file" &&
                m.file &&
                m.file.mimeType.startsWith("image/") &&
                isSafeUrl(m.file.url),
        ),
    );

    let lightboxIndex = $derived(
        lightbox?.kind === "image" && lightbox.id
            ? imageMessages.findIndex((m) => m.id === lightbox!.id)
            : -1,
    );
    // The viewed image was deleted out from under the lightbox: close it.
    $effect(() => {
        if (lightbox?.kind === "image" && lightbox.id && lightboxIndex === -1) {
            lightbox = null;
        }
    });

    function navLightbox(delta: number) {
        if (!lightbox || lightbox.kind !== "image" || lightboxIndex < 0) return;
        const target = imageMessages[lightboxIndex + delta];
        if (!target?.file) return;
        lightbox = {
            url: withJoinToken(target.file.url) ?? "",
            name: target.file.name,
            kind: "image",
            id: target.id,
        };
    }

    /** True when msg continues the previous author's run of messages. */
    function isGrouped(i: number): boolean {
        if (i === 0) return false;
        const prev = messages[i - 1];
        const msg = messages[i];
        return (
            prev.participantId === msg.participantId &&
            msg.timestamp - prev.timestamp < GROUP_WINDOW_MS
        );
    }

    // Follow the newest message, but never yank the view away from
    // someone reading history; own messages always snap down.
    let lastMessageCount = 0;
    $effect(() => {
        const count = messages.length;
        if (count > lastMessageCount) {
            const newest = messages[count - 1];
            if (pinnedToBottom || newest?.participantId === selfId) {
                scrollToBottom(lastMessageCount > 0);
            }
        }
        lastMessageCount = count;
    });
</script>

{#if isOpen}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div
        class="chat-panel"
        class:drag-over={isDragOver}
        style="--keyboard-inset: {keyboardInset}px"
        ondragover={handleDragOver}
        ondragleave={handleDragLeave}
        ondrop={handleDrop}
        transition:drawer
    >
        <div class="chat-inner">
        <div class="chat-header">
            <h3>Chat</h3>
            <button class="chat-close" onclick={onClose} aria-label="Close chat">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="16" height="16" aria-hidden="true"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>
            </button>
        </div>

        <div
            class="chat-messages"
            bind:this={messagesContainer}
            onscroll={handleMessagesScroll}
            onwheel={handleUserScrollIntent}
            ontouchmove={handleUserScrollIntent}
        >
            {#each messages as msg, i (msg.id)}
                {@const grouped = isGrouped(i)}
                {@const own = msg.participantId === selfId}
                <div
                    class="chat-message"
                    class:own
                    class:grouped
                    in:fly={{ y: 10, duration: 240, easing: quintOut }}
                >
                    {#if !own && !grouped}
                        <div class="chat-message-meta">
                            <span
                                class="chat-avatar"
                                style="background-color: {participantColors[msg.participantId] ?? 'var(--color-surface-hover)'}"
                                aria-hidden="true">{msg.participantName.charAt(0).toUpperCase()}</span
                            >
                            <span class="chat-message-author">{msg.participantName}</span>
                            <span class="chat-message-time">{formatTime(msg.timestamp)}</span>
                        </div>
                    {/if}
                    {#if grouped || own}
                        <span class="chat-hover-time" aria-hidden="true">{formatTime(msg.timestamp)}</span>
                    {/if}
                    {#if canModerate}
                        <button
                            class="chat-message-delete"
                            onclick={() => deleteMessage(msg.id)}
                            title="Delete message"
                            aria-label="Delete message from {msg.participantName}"
                        >
                            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="12" height="12" aria-hidden="true"><path d="M3 6h18"/><path d="M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6"/><path d="M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2"/></svg>
                        </button>
                    {/if}
                    <div class="chat-message-body">
                        {#if msg.type === "text"}
                            <div class="chat-message-content">{msg.content}</div>
                        {:else if msg.file && isSafeUrl(msg.file.url)}
                            <div class="chat-message-file">
                                {#if msg.file.mimeType.startsWith("image/") && isSafeUrl(msg.file.thumbnailUrl || msg.file.url)}
                                    <a
                                        href={withJoinToken(msg.file.url)}
                                        target="_blank"
                                        rel="noopener noreferrer"
                                        onclick={(e) => {
                                            e.preventDefault();
                                            lightbox = {
                                                url: withJoinToken(msg.file!.url) ?? "",
                                                name: msg.file!.name,
                                                kind: "image",
                                                id: msg.id,
                                            };
                                        }}
                                    >
                                        <img
                                            src={withJoinToken(msg.file.thumbnailUrl || msg.file.url)}
                                            alt={msg.file.name}
                                            onload={() => {
                                                if (pinnedToBottom) scrollToBottom();
                                            }}
                                        />
                                    </a>
                                {:else if msg.file.mimeType.startsWith("audio/")}
                                    <AudioMessage
                                        src={withJoinToken(msg.file.url) ?? ""}
                                        name={msg.file.name}
                                    />
                                {:else}
                                    <a
                                        href={withJoinToken(msg.file.url)}
                                        target="_blank"
                                        rel="noopener noreferrer"
                                        class="file-link"
                                        onclick={(e) => {
                                            if (msg.file!.mimeType === "application/pdf") {
                                                e.preventDefault();
                                                lightbox = {
                                                    url: withJoinToken(msg.file!.url) ?? "",
                                                    name: msg.file!.name,
                                                    kind: "pdf",
                                                };
                                            }
                                        }}
                                    >
                                        <span class="file-icon" aria-hidden="true">
                                            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="18" height="18"><path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/></svg>
                                        </span>
                                        <span class="file-name">{msg.file.name}</span>
                                    </a>
                                {/if}
                            </div>
                        {/if}
                    </div>
                </div>
            {/each}

            {#if messages.length === 0}
                <div class="chat-empty">
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" width="28" height="28" aria-hidden="true"><path d="M7.9 20A9 9 0 1 0 4 16.1L2 22Z"/></svg>
                    <span>Notes, links and stills shared during the session appear here.</span>
                </div>
            {/if}
        </div>

        {#if uploadProgress !== null}
            <div class="upload-chip" in:fly={{ y: 6, duration: 180, easing: quintOut }}>
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="13" height="13" aria-hidden="true"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="17 8 12 3 7 8"/><line x1="12" x2="12" y1="3" y2="15"/></svg>
                <span class="upload-chip-name">{uploadName}</span>
                <div class="upload-chip-bar" role="progressbar" aria-valuenow={uploadProgress} aria-valuemin="0" aria-valuemax="100">
                    <div class="upload-chip-fill" style="width: {uploadProgress}%"></div>
                </div>
                <span class="upload-chip-pct">{uploadProgress}%</span>
            </div>
        {/if}

        {#if uploadError}
            <div class="upload-chip error" in:fly={{ y: 6, duration: 180, easing: quintOut }}>
                <span class="upload-chip-name">{uploadError}</span>
                <button class="chip-dismiss" onclick={() => uploadError = null} aria-label="Dismiss error">
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="12" height="12" aria-hidden="true"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>
                </button>
            </div>
        {/if}

        {#if isDragOver}
            <div class="drop-overlay">
                <div class="drop-overlay-content">
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" width="30" height="30" aria-hidden="true"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="17 8 12 3 7 8"/><line x1="12" x2="12" y1="3" y2="15"/></svg>
                    <span>Drop to share</span>
                </div>
            </div>
        {/if}

        {#if typing.length > 0}
            <div class="typing-row" transition:fade={{ duration: 140 }}>
                <span class="typing-avatars" aria-hidden="true">
                    {#each typing.slice(0, 3) as t (t.id)}
                        <span
                            class="typing-avatar"
                            style="background-color: {participantColors[t.id] ?? 'var(--color-surface-hover)'}"
                        >{t.name.charAt(0).toUpperCase()}</span>
                    {/each}
                    {#if typing.length > 3}
                        <span class="typing-avatar overflow">+{typing.length - 3}</span>
                    {/if}
                </span>
                <span class="typing-dots" aria-hidden="true"><i></i><i></i><i></i></span>
                <span class="typing-text">{typingLabel}</span>
            </div>
        {/if}

        <form class="chat-input-container" onsubmit={handleSubmit}>
            <input
                type="file"
                bind:this={fileInput}
                onchange={handleFileInputChange}
                accept={ALLOWED_TYPES.join(",")}
                class="file-input-hidden"
            />
            <div class="chat-input-pill">
                <button
                    type="button"
                    class="chat-attach"
                    onclick={handleFileSelect}
                    disabled={uploadProgress !== null}
                    title="Attach an image, audio file or PDF (max 5MB)"
                    aria-label="Attach file"
                >
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="16" height="16" aria-hidden="true"><path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l8.57-8.57A4 4 0 1 1 18 8.84l-8.59 8.57a2 2 0 0 1-2.83-2.83l8.49-8.48"/></svg>
                </button>
                <input
                    type="text"
                    class="chat-text-input"
                    bind:value={messageInput}
                    oninput={handleTyping}
                    placeholder="Message"
                    maxlength="2000"
                />
                <button
                    type="submit"
                    class="chat-send"
                    disabled={!messageInput.trim()}
                    aria-label="Send message"
                >
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="15" height="15" aria-hidden="true"><path d="m22 2-7 20-4-9-9-4Z"/><path d="M22 2 11 13"/></svg>
                </button>
            </div>
        </form>
        </div>
    </div>
{/if}

{#if lightbox}
    <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
    <div
        class="chat-lightbox"
        role="dialog"
        aria-modal="true"
        aria-label={lightbox.name}
        tabindex="-1"
        transition:fade={{ duration: 160 }}
        onclick={(e) => {
            if (e.target === e.currentTarget) lightbox = null;
        }}
    >
        {#if lightbox.kind === "pdf"}
            <iframe
                src={lightbox.url}
                title={lightbox.name}
                class="chat-lightbox-pdf"
                transition:scale={{ start: 0.96, duration: 260, easing: quintOut }}
            ></iframe>
        {:else}
            <img
                src={lightbox.url}
                alt={lightbox.name}
                transition:scale={{ start: 0.94, duration: 260, easing: quintOut }}
            />
        {/if}
        {#if lightbox.kind === "image" && lightboxIndex >= 0 && imageMessages.length > 1}
            <button
                class="chat-lightbox-nav prev"
                onclick={() => navLightbox(-1)}
                disabled={lightboxIndex <= 0}
                aria-label="Previous image"
            >
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="20" height="20" aria-hidden="true"><path d="m15 18-6-6 6-6"/></svg>
            </button>
            <button
                class="chat-lightbox-nav next"
                onclick={() => navLightbox(1)}
                disabled={lightboxIndex >= imageMessages.length - 1}
                aria-label="Next image"
            >
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="20" height="20" aria-hidden="true"><path d="m9 18 6-6-6-6"/></svg>
            </button>
        {/if}
        <div class="chat-lightbox-bar">
            {#if lightbox.kind === "image" && lightboxIndex >= 0 && imageMessages.length > 1}
                <span class="chat-lightbox-count">{lightboxIndex + 1} of {imageMessages.length}</span>
            {/if}
            <span class="chat-lightbox-name">{lightbox.name}</span>
            <a
                class="chat-lightbox-open"
                href={lightbox.url}
                target="_blank"
                rel="noopener noreferrer"
            >Open original</a>
            <button class="chat-lightbox-close" onclick={() => (lightbox = null)} aria-label="Close">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" width="14" height="14" aria-hidden="true"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>
            </button>
        </div>
    </div>
{/if}

<style>
    .chat-panel {
        width: var(--chat-panel-width, 320px);
        height: 100%;
        display: flex;
        background: var(--color-surface);
        border-left: 1px solid var(--color-border);
        overflow: hidden;
        /* Own stacking context ABOVE the viewport-fixed cam strip (z-index 8),
           so the cam circles can never paint over chat content — not even during
           the open/close width animation, when the strip's offset transition
           and the drawer width briefly disagree. */
        position: relative;
        z-index: 9;
    }

    /* Fixed-width inner column, right-anchored: during the drawer reveal
       the panel's width animates while the content never reflows. */
    .chat-inner {
        width: var(--chat-panel-width, 320px);
        flex-shrink: 0;
        margin-left: auto;
        height: 100%;
        display: flex;
        flex-direction: column;
        position: relative;
    }

    .chat-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        padding: var(--space-sm) var(--space-md);
        border-bottom: 1px solid var(--color-border-subtle);
    }

    .chat-header h3 {
        margin: 0;
        font-size: 0.8125rem;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.08em;
        color: var(--color-text-muted);
    }

    .chat-close {
        display: flex;
        align-items: center;
        justify-content: center;
        width: 28px;
        height: 28px;
        background: transparent;
        border: none;
        border-radius: var(--radius-full);
        color: var(--color-text-subtle);
        cursor: pointer;
        transition: background 0.12s ease, color 0.12s ease;
    }
    .chat-close:hover {
        background: rgba(255, 255, 255, 0.08);
        color: var(--color-text);
    }

    .chat-messages {
        flex: 1;
        overflow-y: auto;
        padding: var(--space-md) var(--space-md) var(--space-sm);
        scrollbar-width: thin;
        scrollbar-color: var(--color-border) transparent;
    }

    .chat-message {
        position: relative;
        margin-bottom: var(--space-md);
    }
    .chat-message.grouped {
        margin-top: calc(-1 * var(--space-md) + 3px);
    }

    /* Others' grouped messages align under the name, past the avatar */
    .chat-message:not(.own) .chat-message-body {
        padding-left: calc(22px + var(--space-sm));
    }

    /* Own messages: quiet elevated bubble on the right */
    .chat-message.own .chat-message-body {
        margin-left: var(--space-xl);
        background: var(--color-surface-elevated);
        border: 1px solid var(--color-border-subtle);
        border-radius: 14px 14px 4px 14px;
        padding: var(--space-sm) 12px;
        width: fit-content;
        margin-left: auto;
        max-width: calc(100% - var(--space-xl));
    }
    .chat-message.own.grouped .chat-message-body {
        border-radius: 14px 4px 4px 14px;
    }

    .chat-message-meta {
        display: flex;
        gap: var(--space-sm);
        align-items: center;
        margin-bottom: 3px;
    }

    .chat-avatar {
        width: 22px;
        height: 22px;
        border-radius: 50%;
        display: flex;
        align-items: center;
        justify-content: center;
        font-size: 0.625rem;
        font-weight: 600;
        color: #fff;
        flex-shrink: 0;
    }

    .chat-message-author {
        font-size: 0.75rem;
        font-weight: 600;
        color: var(--color-text);
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
    }

    .chat-message-time {
        font-size: 0.6875rem;
        color: var(--color-text-subtle);
        flex-shrink: 0;
    }

    /* Hover timestamp on grouped messages (head rows show it inline) */
    .chat-hover-time {
        position: absolute;
        top: 2px;
        right: 0;
        font-size: 0.625rem;
        color: var(--color-text-subtle);
        opacity: 0;
        transition: opacity 0.12s ease;
        pointer-events: none;
        font-variant-numeric: tabular-nums;
    }
    .chat-message.own .chat-hover-time {
        right: auto;
        left: 0;
        top: 50%;
        transform: translateY(-50%);
    }
    .chat-message:hover .chat-hover-time {
        opacity: 1;
    }
    .chat-message.grouped .chat-message-delete {
        right: 42px;
    }

    /* Moderation: revealed on message hover/focus to keep the transcript clean */
    .chat-message-delete {
        position: absolute;
        top: 0;
        right: 0;
        display: flex;
        align-items: center;
        background: none;
        border: none;
        padding: 3px;
        border-radius: var(--radius-sm);
        color: var(--color-text-subtle);
        cursor: pointer;
        opacity: 0;
        transition: opacity 0.12s ease, color 0.12s ease;
        z-index: 1;
    }
    .chat-message:hover .chat-message-delete,
    .chat-message:focus-within .chat-message-delete {
        opacity: 1;
    }
    .chat-message-delete:hover {
        color: var(--color-error);
    }

    .chat-message-content {
        font-size: 0.875rem;
        line-height: 1.45;
        color: var(--color-text-muted);
        word-break: break-word;
    }
    .chat-message.own .chat-message-content {
        color: var(--color-text);
    }

    .chat-message-file img {
        max-width: 100%;
        max-height: 220px;
        border-radius: 10px;
        cursor: pointer;
        display: block;
        transition: filter 0.15s ease;
    }
    .chat-message-file img:hover {
        filter: brightness(1.08);
    }

    .chat-message-file .file-link {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        padding: var(--space-sm) 10px;
        background: var(--color-bg);
        border: 1px solid var(--color-border-subtle);
        border-radius: 10px;
        text-decoration: none;
        color: var(--color-text);
        transition: background 0.12s ease, border-color 0.12s ease;
    }

    .chat-message-file .file-link:hover {
        background: var(--color-surface-hover);
        border-color: var(--color-border);
    }

    .chat-message-file .file-icon {
        display: flex;
        align-items: center;
        color: var(--color-text-muted);
        flex-shrink: 0;
    }

    .chat-message-file .file-name {
        font-size: 0.75rem;
        word-break: break-word;
    }

    .chat-empty {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: var(--space-sm);
        text-align: center;
        color: var(--color-text-subtle);
        padding: var(--space-2xl) var(--space-lg);
        font-size: var(--text-body, 0.875rem);
        line-height: 1.5;
    }
    .chat-empty svg {
        opacity: 0.5;
    }

    /* Upload + error chips float above the input */
    .upload-chip {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        margin: 0 var(--space-md) var(--space-xs);
        padding: 6px 10px;
        background: var(--color-surface-elevated);
        border: 1px solid var(--color-border-subtle);
        border-radius: 10px;
        font-size: 0.75rem;
        color: var(--color-text-muted);
    }
    .upload-chip.error {
        border-color: rgba(239, 90, 90, 0.45);
        color: var(--color-error);
        justify-content: space-between;
    }
    .upload-chip-name {
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
        flex-shrink: 1;
        min-width: 0;
    }
    .upload-chip-bar {
        flex: 1;
        min-width: 40px;
        height: 3px;
        border-radius: 2px;
        background: rgba(255, 255, 255, 0.08);
        overflow: hidden;
    }
    .upload-chip-fill {
        height: 100%;
        border-radius: 2px;
        background: var(--color-primary);
        transition: width 0.15s ease;
    }
    .upload-chip-pct {
        font-variant-numeric: tabular-nums;
        flex-shrink: 0;
    }
    .chip-dismiss {
        display: flex;
        align-items: center;
        background: none;
        border: none;
        color: inherit;
        cursor: pointer;
        padding: 2px;
        border-radius: var(--radius-sm);
        flex-shrink: 0;
    }

    .drop-overlay {
        position: absolute;
        inset: 0;
        background: rgba(12, 12, 15, 0.9);
        display: flex;
        align-items: center;
        justify-content: center;
        z-index: 10;
        pointer-events: none;
        padding: var(--space-md);
    }

    .drop-overlay-content {
        display: flex;
        flex-direction: column;
        align-items: center;
        justify-content: center;
        gap: var(--space-sm);
        color: var(--color-text);
        font-size: 0.875rem;
        font-weight: 500;
        width: 100%;
        height: 100%;
        border: 1.5px dashed rgba(255, 255, 255, 0.25);
        border-radius: var(--radius-lg);
    }

    .chat-panel.drag-over {
        position: relative;
    }

    .file-input-hidden {
        position: absolute;
        width: 1px;
        height: 1px;
        padding: 0;
        margin: -1px;
        overflow: hidden;
        clip: rect(0, 0, 0, 0);
        border: 0;
    }

    /* Typing indicator: a quiet line that never shifts the transcript */
    .typing-row {
        display: flex;
        align-items: center;
        gap: 6px;
        padding: 0 var(--space-md) 6px;
        font-size: var(--text-min);
        color: var(--color-text-subtle);
    }
    .typing-avatars {
        display: inline-flex;
        flex-shrink: 0;
    }
    .typing-avatar {
        width: 16px;
        height: 16px;
        border-radius: 50%;
        display: flex;
        align-items: center;
        justify-content: center;
        font-size: 0.5rem;
        font-weight: 600;
        color: #fff;
        border: 1.5px solid var(--color-surface);
        margin-left: -5px;
    }
    .typing-avatar:first-child {
        margin-left: 0;
    }
    .typing-avatar.overflow {
        background: var(--color-surface-hover);
        color: var(--color-text-muted);
    }
    .typing-dots {
        display: inline-flex;
        gap: 3px;
        flex-shrink: 0;
    }
    .typing-dots i {
        width: 4px;
        height: 4px;
        border-radius: 50%;
        background: currentColor;
        animation: typing-bounce 1.2s ease-in-out infinite;
    }
    .typing-dots i:nth-child(2) { animation-delay: 0.15s; }
    .typing-dots i:nth-child(3) { animation-delay: 0.3s; }
    @keyframes typing-bounce {
        0%, 60%, 100% { opacity: 0.35; transform: translateY(0); }
        30% { opacity: 1; transform: translateY(-2px); }
    }
    .typing-text {
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
    }

    .chat-input-container {
        padding: var(--space-sm) var(--space-md) var(--space-md);
        border-top: 1px solid var(--color-border-subtle);
    }

    .chat-input-pill {
        display: flex;
        align-items: center;
        gap: 4px;
        background: rgba(255, 255, 255, 0.05);
        border: 1px solid var(--color-border);
        border-radius: var(--radius-full);
        padding: 4px;
        transition: border-color 0.15s ease, background 0.15s ease;
    }
    .chat-input-pill:focus-within {
        border-color: rgba(255, 255, 255, 0.3);
        background: rgba(255, 255, 255, 0.07);
    }

    .chat-attach {
        display: flex;
        align-items: center;
        justify-content: center;
        width: 30px;
        height: 30px;
        flex-shrink: 0;
        background: transparent;
        border: none;
        border-radius: var(--radius-full);
        color: var(--color-text-subtle);
        cursor: pointer;
        transition: background 0.12s ease, color 0.12s ease;
    }
    .chat-attach:hover:not(:disabled) {
        background: rgba(255, 255, 255, 0.08);
        color: var(--color-text);
    }
    .chat-attach:disabled {
        opacity: 0.4;
        cursor: wait;
    }

    .chat-text-input {
        flex: 1;
        min-width: 0;
        background: none;
        border: none;
        outline: none;
        color: var(--color-text);
        font-size: 0.875rem;
        font-family: inherit;
        padding: 0 4px;
    }
    /* Touch only: >=16px so iOS Safari doesn't zoom the page on focus. */
    @media (pointer: coarse) {
        .chat-text-input {
            font-size: 16px;
        }
    }
    .chat-text-input::placeholder {
        color: var(--color-text-subtle);
    }

    .chat-send {
        display: flex;
        align-items: center;
        justify-content: center;
        width: 30px;
        height: 30px;
        flex-shrink: 0;
        background: var(--color-primary);
        border: none;
        border-radius: var(--radius-full);
        color: #06211d;
        cursor: pointer;
        transition: background 0.15s ease, color 0.15s ease, transform 0.15s var(--ease-spring);
    }
    .chat-send:hover:not(:disabled) {
        background: var(--color-primary-hover);
    }
    .chat-send:active:not(:disabled) {
        transform: scale(0.9);
    }
    .chat-send:disabled {
        background: rgba(255, 255, 255, 0.07);
        color: var(--color-text-subtle);
        cursor: default;
    }

    /* In-app lightbox for shared stills: never leave the room to look
       at a frame. Glass scrim, image floats, caption bar below. */
    .chat-lightbox {
        position: fixed;
        inset: 0;
        z-index: 300;
        display: flex;
        flex-direction: column;
        align-items: center;
        justify-content: center;
        gap: var(--space-md);
        padding: var(--space-xl);
        background: rgba(8, 8, 10, 0.9);
        cursor: zoom-out;
    }
    .chat-lightbox img {
        max-width: min(92vw, 1600px);
        max-height: 80dvh;
        border-radius: 12px;
        box-shadow: 0 24px 64px rgba(0, 0, 0, 0.5);
        cursor: default;
    }
    .chat-lightbox-pdf {
        width: min(92vw, 1100px);
        height: 80dvh;
        border: none;
        border-radius: 12px;
        background: #fff;
        box-shadow: 0 24px 64px rgba(0, 0, 0, 0.5);
        cursor: default;
    }
    .chat-lightbox-bar {
        display: flex;
        align-items: center;
        gap: var(--space-md);
        padding: 6px 8px 6px 16px;
        background: rgba(20, 20, 24, 0.85);
        border: 1px solid rgba(255, 255, 255, 0.08);
        border-radius: var(--radius-full);
        box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.07);
        cursor: default;
    }
    .chat-lightbox-nav {
        position: fixed;
        top: 50%;
        transform: translateY(-50%);
        z-index: 301;
        display: flex;
        align-items: center;
        justify-content: center;
        width: 44px;
        height: 44px;
        background: rgba(20, 20, 24, 0.9);
        border: 1px solid rgba(255, 255, 255, 0.1);
        border-radius: var(--radius-full);
        color: var(--color-text);
        cursor: pointer;
        transition: background 0.12s ease, opacity 0.12s ease, transform 0.15s var(--ease-spring);
        box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.08);
    }
    .chat-lightbox-nav.prev { left: 24px; }
    .chat-lightbox-nav.next { right: 24px; }
    .chat-lightbox-nav:hover:not(:disabled) {
        background: rgba(40, 40, 46, 0.85);
    }
    .chat-lightbox-nav:active:not(:disabled) {
        transform: translateY(-50%) scale(0.92);
    }
    .chat-lightbox-nav:disabled {
        opacity: 0.25;
        cursor: default;
    }

    .chat-lightbox-count {
        font-size: var(--text-meta);
        color: var(--color-text-subtle);
        font-variant-numeric: tabular-nums;
        white-space: nowrap;
    }

    .chat-lightbox-name {
        font-size: var(--text-meta);
        color: var(--color-text-muted);
        max-width: 40vw;
        white-space: nowrap;
        overflow: hidden;
        text-overflow: ellipsis;
    }
    .chat-lightbox-open {
        font-size: var(--text-meta);
        font-weight: 500;
        color: var(--color-text);
        white-space: nowrap;
    }
    .chat-lightbox-open:hover {
        color: #fff;
    }
    .chat-lightbox-close {
        display: flex;
        align-items: center;
        justify-content: center;
        width: 28px;
        height: 28px;
        background: rgba(255, 255, 255, 0.08);
        border: none;
        border-radius: var(--radius-full);
        color: var(--color-text);
        cursor: pointer;
        transition: background 0.12s ease;
    }
    .chat-lightbox-close:hover {
        background: rgba(255, 255, 255, 0.16);
    }

    @media (max-width: 768px), (orientation: landscape) and (max-height: 480px) and (pointer: coarse) {
        .chat-panel {
            position: fixed;
            /* Ride above the iOS keyboard (keyboard-inset comes from
               visualViewport); when closed this is 0. */
            bottom: var(--keyboard-inset, 0px);
            left: 0;
            right: 0;
            width: 100dvw;
            max-width: 100dvw;
            box-sizing: border-box;
            /* Taller sheet (a 50% sheet is a slit in landscape); dvh so Safari's
               toolbar collapse doesn't change it. */
            height: min(70dvh, 560px);
            border-left: none;
            border-top: 1px solid var(--color-border);
            border-radius: var(--radius-lg) var(--radius-lg) 0 0;
            z-index: 100;
            transition: bottom 0.2s ease;
        }
        /* Keep the input clear of the home indicator when the keyboard is down. */
        .chat-input-container {
            padding-bottom: calc(var(--space-sm) + env(safe-area-inset-bottom, 0px));
        }
        .chat-inner {
            width: 100%;
            min-width: 0;
            flex: 1 1 auto;
            margin-left: 0;
        }
        .chat-header,
        .chat-messages,
        .chat-input-container,
        .typing-row,
        .upload-chip {
            width: 100%;
            box-sizing: border-box;
        }
        .chat-messages {
            padding-left: var(--space-sm);
            padding-right: var(--space-sm);
        }
        .chat-message,
        .chat-message-body,
        .chat-message-content,
        .chat-message-file,
        .chat-message-file a,
        .chat-message-file .file-link {
            width: 100%;
            max-width: 100%;
            box-sizing: border-box;
        }
        .chat-message:not(.own) .chat-message-body {
            padding-left: 0;
        }
        .chat-message.own .chat-message-body {
            margin-left: 0;
            margin-right: 0;
            width: 100%;
            max-width: 100%;
        }
        .chat-message-file img {
            width: 100%;
            max-width: 100%;
            height: auto;
            max-height: 42dvh;
            object-fit: contain;
        }
        .chat-message-file :global(.audio-msg) {
            width: 100%;
            box-sizing: border-box;
        }
        .chat-input-pill {
            width: 100%;
            box-sizing: border-box;
        }
    }
</style>
