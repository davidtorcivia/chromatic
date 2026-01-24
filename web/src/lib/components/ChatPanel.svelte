<script lang="ts">
    import { onMount } from "svelte";
    import { session } from "$lib/stores/session.svelte";
    import { chatStore } from "$lib/stores/chat.svelte";

    interface Props {
        isOpen: boolean;
        onClose: () => void;
    }

    let { isOpen, onClose }: Props = $props();

    let messageInput = $state("");
    let messagesContainer: HTMLDivElement;

    onMount(() => {
        // Subscribe to chat messages
        session.onMessage("chat:message", (payload) => {
            const msg = payload as {
                participantId: string;
                participantName: string;
                type: "text" | "file";
                content: string;
                file?: {
                    id: string;
                    name: string;
                    mimeType: string;
                    url: string;
                    thumbnailUrl?: string;
                };
            };
            chatStore.addMessage(msg);
            scrollToBottom();
        });
    });

    function scrollToBottom() {
        if (messagesContainer) {
            requestAnimationFrame(() => {
                messagesContainer.scrollTop = messagesContainer.scrollHeight;
            });
        }
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
</script>

{#if isOpen}
    <div class="chat-panel">
        <div class="chat-header">
            <h3>Chat</h3>
            <button class="btn btn-icon btn-ghost" onclick={onClose}>
                ✕
            </button>
        </div>

        <div class="chat-messages" bind:this={messagesContainer}>
            {#each messages as msg (msg.id)}
                <div class="chat-message">
                    <div class="chat-message-meta">
                        <span class="chat-message-author"
                            >{msg.participantName}</span
                        >
                        <span class="chat-message-time"
                            >{formatTime(msg.timestamp)}</span
                        >
                    </div>
                    {#if msg.type === "text"}
                        <div class="chat-message-content">{msg.content}</div>
                    {:else if msg.file}
                        <div class="chat-message-file">
                            {#if msg.file.mimeType.startsWith("image/")}
                                <img
                                    src={msg.file.thumbnailUrl || msg.file.url}
                                    alt={msg.file.name}
                                />
                            {:else}
                                <a href={msg.file.url} target="_blank"
                                    >{msg.file.name}</a
                                >
                            {/if}
                        </div>
                    {/if}
                </div>
            {/each}

            {#if messages.length === 0}
                <div class="chat-empty">No messages yet</div>
            {/if}
        </div>

        <form class="chat-input-container" onsubmit={handleSubmit}>
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
        font-size: 0.625rem;
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

    .chat-empty {
        text-align: center;
        color: var(--color-text-subtle);
        padding: var(--space-xl);
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
