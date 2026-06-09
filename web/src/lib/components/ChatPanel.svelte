<script lang="ts">
    import { fly } from "svelte/transition";
    import { session } from "$lib/stores/session.svelte";
    import { chatStore } from "$lib/stores/chat.svelte";
    import { uploadFile, type UploadedFile } from "$lib/api/client";

    interface Props {
        isOpen: boolean;
        onClose: () => void;
        roomSlug: string;
        joinToken: string;
        /** Map of participant id → display color (tints author names). */
        participantColors?: Record<string, string>;
        /** Own participant id, used to distinguish own messages. */
        selfId?: string;
    }

    let {
        isOpen,
        onClose,
        roomSlug,
        joinToken,
        participantColors = {},
        selfId = "",
    }: Props = $props();

    // Allowed file types (must match backend)
    const ALLOWED_TYPES = [
        "image/jpeg",
        "image/png",
        "image/gif",
        "image/webp",
        "audio/mpeg",
        "audio/wav",
        "audio/ogg",
        "application/pdf",
    ];
    const MAX_FILE_SIZE = 5 * 1024 * 1024; // 5MB

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
        if (!url || !joinToken) return url;
        try {
            const parsed = new URL(url, window.location.origin);
            parsed.searchParams.set("token", joinToken);
            return parsed.toString();
        } catch {
            return url;
        }
    }

    let messageInput = $state("");
    let messagesContainer = $state<HTMLDivElement | null>(null);
    let fileInput = $state<HTMLInputElement | null>(null);
    let uploadProgress = $state<number | null>(null);
    let uploadError = $state<string | null>(null);
    let isDragOver = $state(false);

    // Chat message handlers are registered by the session page (always
    // mounted) and feed chatStore; this panel just renders the store.

    function scrollToBottom() {
        if (!messagesContainer) return;
        const container = messagesContainer;
        requestAnimationFrame(() => {
            container.scrollTop = container.scrollHeight;
        });
    }

    function handleSubmit(e: SubmitEvent) {
        e.preventDefault();
        if (!messageInput.trim()) return;

        session.send("chat:send", { content: messageInput.trim() });
        messageInput = "";
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
        uploadError = null;

        // Validate file type
        if (!ALLOWED_TYPES.includes(file.type)) {
            uploadError = "File type not allowed. Use images, audio, or PDF.";
            return;
        }

        // Validate file size
        if (file.size > MAX_FILE_SIZE) {
            uploadError = `File too large. Maximum size is ${formatFileSize(MAX_FILE_SIZE)}.`;
            return;
        }

        uploadProgress = 0;

        try {
            const uploadedFile = await uploadFile(
                roomSlug,
                file,
                joinToken,
                (progress) => {
                    uploadProgress = progress;
                }
            );

            // Send chat:file message via WebSocket
            session.send("chat:file", {
                fileId: uploadedFile.id,
                name: uploadedFile.originalName,
                mimeType: uploadedFile.mimeType,
                url: uploadedFile.url,
                thumbnailUrl: uploadedFile.thumbnailUrl,
            });

            uploadProgress = null;
        } catch (err) {
            uploadError = err instanceof Error ? err.message : "Upload failed";
            uploadProgress = null;
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
            scrollToBottom();
        } else {
            chatStore.setVisible(false);
        }
    });

    let messages = $derived(chatStore.messages);
    let unreadCount = $derived(chatStore.unreadCount);

    // Keep the view scrolled to the newest message
    $effect(() => {
        if (messages.length > 0) {
            scrollToBottom();
        }
    });
</script>

{#if isOpen}
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <div
        class="chat-panel"
        class:drag-over={isDragOver}
        ondragover={handleDragOver}
        ondragleave={handleDragLeave}
        ondrop={handleDrop}
        transition:fly={{ x: 320, duration: 200 }}
    >
        <div class="chat-header">
            <h3>Chat</h3>
            <button class="btn btn-icon btn-ghost" onclick={onClose} aria-label="Close chat">
                <svg viewBox="0 0 24 24" fill="currentColor" width="18" height="18" aria-hidden="true"><path d="M19 6.41 17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z"/></svg>
            </button>
        </div>

        <div class="chat-messages" bind:this={messagesContainer}>
            {#each messages as msg (msg.id)}
                <div class="chat-message" class:own={msg.participantId === selfId}>
                    <div class="chat-message-meta">
                        <span
                            class="chat-message-author"
                            style="color: {participantColors[msg.participantId] ?? 'var(--color-text)'}"
                            >{msg.participantName}</span
                        >
                        <span class="chat-message-time"
                            >{formatTime(msg.timestamp)}</span
                        >
                    </div>
                    {#if msg.type === "text"}
                        <div class="chat-message-content">{msg.content}</div>
                    {:else if msg.file && isSafeUrl(msg.file.url)}
                        <div class="chat-message-file">
                            {#if msg.file.mimeType.startsWith("image/") && isSafeUrl(msg.file.thumbnailUrl || msg.file.url)}
                                <a href={withJoinToken(msg.file.url)} target="_blank" rel="noopener noreferrer">
                                    <img
                                        src={withJoinToken(msg.file.thumbnailUrl || msg.file.url)}
                                        alt={msg.file.name}
                                    />
                                </a>
                            {:else if msg.file.mimeType.startsWith("audio/")}
                                <div class="audio-file">
                                    <span class="file-name">{msg.file.name}</span>
                                    <audio controls src={withJoinToken(msg.file.url)} preload="metadata">
                                        <track kind="captions" />
                                    </audio>
                                </div>
                            {:else}
                                <a href={withJoinToken(msg.file.url)} target="_blank" rel="noopener noreferrer" class="file-link">
                                    <span class="file-icon" aria-hidden="true">
                                        <svg viewBox="0 0 24 24" fill="currentColor" width="20" height="20"><path d="M14 2H6c-1.1 0-1.99.9-1.99 2L4 20c0 1.1.89 2 1.99 2H18c1.1 0 2-.9 2-2V8l-6-6zm2 16H8v-2h8v2zm0-4H8v-2h8v2zm-3-5V3.5L18.5 9H13z"/></svg>
                                    </span>
                                    <span class="file-name">{msg.file.name}</span>
                                </a>
                            {/if}
                        </div>
                    {/if}
                </div>
            {/each}

            {#if messages.length === 0}
                <div class="chat-empty">Notes, links and stills shared during the session appear here.</div>
            {/if}
        </div>

        {#if uploadProgress !== null}
            <div class="upload-progress">
                <div class="upload-progress-bar" style="width: {uploadProgress}%"></div>
                <span class="upload-progress-text">Uploading... {uploadProgress}%</span>
            </div>
        {/if}

        {#if uploadError}
            <div class="upload-error">
                <span>{uploadError}</span>
                <button class="btn-dismiss" onclick={() => uploadError = null} aria-label="Dismiss error">
                    <svg viewBox="0 0 24 24" fill="currentColor" width="14" height="14" aria-hidden="true"><path d="M19 6.41 17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z"/></svg>
                </button>
            </div>
        {/if}

        {#if isDragOver}
            <div class="drop-overlay">
                <div class="drop-overlay-content">
                    <span class="drop-icon" aria-hidden="true">
                        <svg viewBox="0 0 24 24" fill="currentColor" width="32" height="32"><path d="M16.5 6v11.5c0 2.21-1.79 4-4 4s-4-1.79-4-4V5c0-1.38 1.12-2.5 2.5-2.5s2.5 1.12 2.5 2.5v10.5c0 .55-.45 1-1 1s-1-.45-1-1V6H10v9.5c0 1.38 1.12 2.5 2.5 2.5s2.5-1.12 2.5-2.5V5c0-2.21-1.79-4-4-4S7 2.79 7 5v12.5c0 3.04 2.46 5.5 5.5 5.5s5.5-2.46 5.5-5.5V6h-1.5z"/></svg>
                    </span>
                    <span>Drop file to upload</span>
                </div>
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
            <button
                type="button"
                class="btn btn-icon btn-ghost"
                onclick={handleFileSelect}
                disabled={uploadProgress !== null}
                title="Attach file (images, audio, PDF - max 5MB)"
                aria-label="Attach file"
            >
                <svg viewBox="0 0 24 24" fill="currentColor" width="18" height="18" aria-hidden="true"><path d="M16.5 6v11.5c0 2.21-1.79 4-4 4s-4-1.79-4-4V5c0-1.38 1.12-2.5 2.5-2.5s2.5 1.12 2.5 2.5v10.5c0 .55-.45 1-1 1s-1-.45-1-1V6H10v9.5c0 1.38 1.12 2.5 2.5 2.5s2.5-1.12 2.5-2.5V5c0-2.21-1.79-4-4-4S7 2.79 7 5v12.5c0 3.04 2.46 5.5 5.5 5.5s5.5-2.46 5.5-5.5V6h-1.5z"/></svg>
            </button>
            <input
                type="text"
                class="input"
                bind:value={messageInput}
                placeholder="Type a message..."
                maxlength="2000"
            />
            <button
                type="submit"
                class="btn btn-primary"
                disabled={!messageInput.trim()}
            >
                Send
            </button>
        </form>
    </div>
{/if}

<style>
    .chat-panel {
        width: 320px;
        height: 100%;
        display: flex;
        flex-direction: column;
        background: var(--color-surface);
        border-left: 1px solid var(--color-border);
    }

    .chat-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        padding: var(--space-md);
        border-bottom: 1px solid var(--color-border);
    }

    .chat-header h3 {
        margin: 0;
        font-size: 1rem;
    }

    .chat-messages {
        flex: 1;
        overflow-y: auto;
        padding: var(--space-md);
    }

    .chat-message {
        margin-bottom: var(--space-md);
    }

    /* Subtle elevated bubble for the viewer's own messages */
    .chat-message.own {
        background: var(--color-surface-elevated);
        border: 1px solid var(--color-border);
        border-radius: var(--radius-md);
        padding: var(--space-sm);
        margin-left: var(--space-xl);
    }

    .chat-message-meta {
        display: flex;
        gap: var(--space-sm);
        align-items: baseline;
        margin-bottom: var(--space-xs);
    }

    .chat-message-author {
        font-size: 0.75rem;
        font-weight: 600;
        color: var(--color-text);
    }

    .chat-message-time {
        font-size: 0.6875rem;
        color: var(--color-text-subtle);
    }

    .chat-message-content {
        font-size: 0.875rem;
        color: var(--color-text-muted);
        word-break: break-word;
    }

    .chat-message-file img {
        max-width: 100%;
        border-radius: var(--radius-md);
        cursor: pointer;
    }

    .chat-message-file .audio-file {
        display: flex;
        flex-direction: column;
        gap: var(--space-xs);
    }

    .chat-message-file audio {
        width: 100%;
        height: 32px;
    }

    .chat-message-file .file-link {
        display: flex;
        align-items: center;
        gap: var(--space-sm);
        padding: var(--space-sm);
        background: var(--color-bg);
        border-radius: var(--radius-md);
        text-decoration: none;
        color: var(--color-text);
    }

    .chat-message-file .file-link:hover {
        background: var(--color-border);
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
        text-align: center;
        color: var(--color-text-subtle);
        padding: var(--space-xl);
        font-size: var(--text-body, 0.875rem);
        line-height: 1.5;
    }

    .upload-progress {
        position: relative;
        height: 24px;
        background: var(--color-bg);
        border-top: 1px solid var(--color-border);
    }

    .upload-progress-bar {
        position: absolute;
        top: 0;
        left: 0;
        height: 100%;
        background: var(--color-primary);
        opacity: 0.3;
        transition: width 0.2s ease;
    }

    .upload-progress-text {
        position: absolute;
        top: 50%;
        left: 50%;
        transform: translate(-50%, -50%);
        font-size: 0.75rem;
        color: var(--color-text-muted);
    }

    .upload-error {
        display: flex;
        align-items: center;
        justify-content: space-between;
        padding: var(--space-sm) var(--space-md);
        background: var(--color-error);
        color: white;
        font-size: 0.75rem;
    }

    .upload-error .btn-dismiss {
        background: none;
        border: none;
        color: white;
        cursor: pointer;
        padding: var(--space-xs);
        display: flex;
        align-items: center;
    }

    .drop-overlay {
        position: absolute;
        inset: 0;
        background: rgba(0, 0, 0, 0.7);
        display: flex;
        align-items: center;
        justify-content: center;
        z-index: 10;
        pointer-events: none;
    }

    .drop-overlay-content {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: var(--space-sm);
        color: white;
        font-size: 1rem;
    }

    .drop-icon {
        display: flex;
        align-items: center;
        justify-content: center;
    }

    .chat-panel.drag-over {
        border: 2px dashed var(--color-primary);
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

    .chat-input-container {
        display: flex;
        gap: var(--space-sm);
        padding: var(--space-md);
        border-top: 1px solid var(--color-border);
    }

    .chat-input-container .input {
        flex: 1;
    }

    @media (max-width: 768px) {
        .chat-panel {
            position: fixed;
            bottom: 0;
            left: 0;
            right: 0;
            width: 100%;
            height: 50%;
            border-left: none;
            border-top: 1px solid var(--color-border);
            border-radius: var(--radius-lg) var(--radius-lg) 0 0;
            z-index: 100;
        }
    }
</style>
