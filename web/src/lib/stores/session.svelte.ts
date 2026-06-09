// Session store - manages WebSocket and room state

import type { Participant } from '$lib/api/client';

export interface RoomState {
    slug: string;
    name: string;
    isLive: boolean;
    participants: Participant[];
    // Watermark configuration
    watermarkMode: 'none' | 'text' | 'logo' | 'both';
    watermarkText?: string;
    watermarkLogoUrl?: string;
    watermarkLogoPosition?: 'top-left' | 'top-right' | 'bottom-left' | 'bottom-right';
    watermarkOpacity: number;
}

export interface SessionState {
    connected: boolean;
    room: RoomState | null;
    participantId: string | null;
    isAdmin: boolean;
    error: string | null;
    reconnecting: boolean;
    reconnectAttempt: number;
}

// Reconnection configuration
const RECONNECT_BASE_DELAY = 1000; // 1 second
const RECONNECT_MAX_DELAY = 30000; // 30 seconds
const RECONNECT_MAX_ATTEMPTS = 10;

// Routine WS logging is dev-only; errors are always logged.
const DEBUG = import.meta.env.DEV;

// Create reactive state using Svelte 5 runes
export function createSessionStore() {
    let state = $state<SessionState>({
        connected: false,
        room: null,
        participantId: null,
        isAdmin: false,
        error: null,
        reconnecting: false,
        reconnectAttempt: 0
    });

    let ws: WebSocket | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    const messageHandlers = new Map<string, Array<(payload: unknown) => void>>();
    // Callbacks invoked after a successful re-open (NOT the first open)
    const reconnectCallbacks = new Set<() => void>();
    // Tracks whether we've had at least one successful open for this session
    let hasConnectedOnce = false;

    // Store connection params for reconnection
    let connectionParams: { roomSlug: string; token: string; name: string } | null = null;

    // Calculate exponential backoff delay with jitter
    function getReconnectDelay(attempt: number): number {
        // Exponential backoff: base * 2^attempt
        const exponentialDelay = RECONNECT_BASE_DELAY * Math.pow(2, attempt);
        // Cap at max delay
        const cappedDelay = Math.min(exponentialDelay, RECONNECT_MAX_DELAY);
        // Add jitter (±20%)
        const jitter = cappedDelay * (0.8 + Math.random() * 0.4);
        return Math.round(jitter);
    }

    function connect(roomSlug: string, token: string, name: string) {
        // Clear any pending reconnect timer so a manual connect plus a
        // pending timer can't create parallel sockets.
        if (reconnectTimer) {
            clearTimeout(reconnectTimer);
            reconnectTimer = null;
        }

        // Store params for reconnection
        connectionParams = { roomSlug, token, name };

        if (ws) {
            // Detach the old socket's onclose to prevent spurious reconnection
            ws.onclose = null;
            ws.close();
        }

        const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        const url = `${protocol}//${window.location.host}/ws/room/${roomSlug}?token=${token}&name=${encodeURIComponent(name)}`;

        ws = new WebSocket(url);

        ws.onopen = () => {
            const isReconnect = hasConnectedOnce;
            hasConnectedOnce = true;
            state.connected = true;
            state.error = null;
            state.reconnecting = false;
            state.reconnectAttempt = 0;
            if (DEBUG) console.log('WebSocket connected');

            // Notify listeners after a successful re-open (not the first open)
            // so they can tear down per-connection state (WebRTC, etc.).
            if (isReconnect) {
                for (const cb of reconnectCallbacks) {
                    try {
                        cb();
                    } catch (err) {
                        console.error('onReconnect callback failed', err);
                    }
                }
            }
        };

        ws.onclose = (e) => {
            state.connected = false;
            if (DEBUG) console.log('WebSocket closed', e.code, e.reason);

            // Attempt reconnection if not intentional close
            if (e.code !== 1000 && connectionParams) {
                if (state.reconnectAttempt < RECONNECT_MAX_ATTEMPTS) {
                    state.reconnecting = true;
                    const delay = getReconnectDelay(state.reconnectAttempt);
                    state.reconnectAttempt++;

                    if (DEBUG) console.log(`Reconnecting in ${delay}ms (attempt ${state.reconnectAttempt}/${RECONNECT_MAX_ATTEMPTS})`);

                    reconnectTimer = setTimeout(() => {
                        reconnectTimer = null;
                        if (connectionParams) {
                            connect(connectionParams.roomSlug, connectionParams.token, connectionParams.name);
                        }
                    }, delay);
                } else {
                    state.reconnecting = false;
                    state.error = 'Connection lost. Please refresh the page.';
                    console.error('Max reconnection attempts reached');
                }
            }
        };

        ws.onerror = (e) => {
            console.error('WebSocket error', e);
            state.error = 'Connection error';
        };

        ws.onmessage = (e) => {
            try {
                const msg = JSON.parse(e.data);
                handleMessage(msg);
            } catch (err) {
                console.error('Failed to parse message', err);
            }
        };
    }

    function handleMessage(msg: { type: string; payload: unknown }) {
        if (DEBUG) console.log('WS message:', msg.type, msg.payload);

        switch (msg.type) {
            case 'room:state':
                const roomState = msg.payload as {
                    room: {
                        slug: string;
                        name: string;
                        watermarkMode?: 'none' | 'text' | 'logo' | 'both';
                        watermarkText?: string;
                        watermarkLogoUrl?: string;
                        watermarkLogoPosition?: 'top-left' | 'top-right' | 'bottom-left' | 'bottom-right';
                        watermarkOpacity?: number;
                    };
                    participants: Participant[];
                    isLive: boolean;
                    iceServers: RTCIceServer[];
                    screenShare?: { participantId: string; name: string } | null;
                };
                state.room = {
                    slug: roomState.room.slug,
                    name: roomState.room.name,
                    isLive: roomState.isLive,
                    participants: roomState.participants,
                    // Map watermark configuration
                    watermarkMode: roomState.room.watermarkMode || 'none',
                    watermarkText: roomState.room.watermarkText,
                    watermarkLogoUrl: roomState.room.watermarkLogoUrl,
                    watermarkLogoPosition: roomState.room.watermarkLogoPosition || 'bottom-right',
                    watermarkOpacity: roomState.room.watermarkOpacity ?? 0.3
                };
                // Emit ICE servers for WebRTC connection
                messageHandlers.get('iceServers')?.forEach(h => h(roomState.iceServers));
                // Emit screen share state for late joiners
                if (roomState.screenShare) {
                    messageHandlers.get('screenshare:started')?.forEach(h => h(roomState.screenShare));
                }
                break;

            case 'room:live':
                if (state.room) {
                    state.room.isLive = true;
                }
                messageHandlers.get('room:live')?.forEach(h => h(msg.payload));
                break;

            case 'room:ended':
                if (state.room) {
                    state.room.isLive = false;
                }
                messageHandlers.get('room:ended')?.forEach(h => h({}));
                break;

            case 'participant:joined':
                const joinedData = msg.payload as { participant: Participant };
                if (state.room) {
                    state.room.participants = [...state.room.participants, joinedData.participant];
                }
                break;

            case 'participant:left':
                const leftData = msg.payload as { participantId: string };
                if (state.room) {
                    state.room.participants = state.room.participants.filter(
                        p => p.id !== leftData.participantId
                    );
                }
                // Forward to registered handlers so components can clean up
                // resources tied to the departed participant (voice analysers, etc.)
                messageHandlers.get('participant:left')?.forEach(h => h(msg.payload));
                break;

            case 'participant:updated':
                const updatedData = msg.payload as { participant: Partial<Participant> & { id: string } };
                if (state.room) {
                    state.room.participants = state.room.participants.map(p =>
                        p.id === updatedData.participant.id ? { ...p, ...updatedData.participant } : p
                    );
                }
                break;

            case 'chat:history':
                // Forward to registered handlers (ChatPanel will handle)
                messageHandlers.get('chat:history')?.forEach(h => h(msg.payload));
                break;

            default:
                // Forward to registered handlers
                messageHandlers.get(msg.type)?.forEach(h => h(msg.payload));
        }
    }

    function send(type: string, payload: unknown) {
        if (ws && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type, payload }));
        }
    }

    // Registers a handler for a message type. Returns an unsubscribe function.
    function onMessage(type: string, handler: (payload: unknown) => void): () => void {
        const handlers = messageHandlers.get(type);
        if (handlers) {
            handlers.push(handler);
        } else {
            messageHandlers.set(type, [handler]);
        }
        return () => offMessage(type, handler);
    }

    function offMessage(type: string, handler: (payload: unknown) => void) {
        const handlers = messageHandlers.get(type);
        if (!handlers) return;
        const idx = handlers.indexOf(handler);
        if (idx !== -1) {
            handlers.splice(idx, 1);
        }
        if (handlers.length === 0) {
            messageHandlers.delete(type);
        }
    }

    // Registers a callback invoked after a successful re-open (not the first
    // open). Returns an unsubscribe function.
    function onReconnect(callback: () => void): () => void {
        reconnectCallbacks.add(callback);
        return () => reconnectCallbacks.delete(callback);
    }

    function clearHandlers() {
        messageHandlers.clear();
        reconnectCallbacks.clear();
    }

    function disconnect() {
        if (reconnectTimer) {
            clearTimeout(reconnectTimer);
            reconnectTimer = null;
        }
        connectionParams = null;
        hasConnectedOnce = false;
        if (ws) {
            ws.onclose = null;
            ws.close(1000);
            ws = null;
        }
        messageHandlers.clear();
        reconnectCallbacks.clear();
        state.connected = false;
        state.room = null;
        state.participantId = null;
        state.isAdmin = false;
        state.error = null;
        state.reconnecting = false;
        state.reconnectAttempt = 0;
    }

    return {
        get state() { return state; },
        connect,
        disconnect,
        send,
        onMessage,
        offMessage,
        onReconnect,
        clearHandlers
    };
}

// Singleton instance
export const session = createSessionStore();
