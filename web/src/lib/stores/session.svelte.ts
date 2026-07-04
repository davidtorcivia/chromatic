// Session store - manages WebSocket and room state

import type { Participant } from '$lib/api/client';

// Participant state as delivered over the session WebSocket. Extends the
// REST Participant shape with the persistent screen share approval flag
// (BUG 3) so admin UIs can render per-row allow/revoke state.
export type SessionParticipant = Participant & { canScreenshare?: boolean };

export interface RoomState {
    slug: string;
    name: string;
    isLive: boolean;
    participants: SessionParticipant[];
    // Scheduling/open state (admin "open room now" banner)
    scheduledAt?: string | null;
    openedAt?: string | null;
    earlyOpenMinutes?: number;
    waitingRoomEnabled?: boolean;
    // Watermark configuration
    watermarkMode: 'none' | 'text' | 'logo' | 'both';
    watermarkText?: string;
    watermarkLogoUrl?: string;
    watermarkLogoPosition?: 'top-left' | 'top-right' | 'bottom-left' | 'bottom-right';
    watermarkOpacity: number;
    watermarkPosX?: number | null;
    watermarkPosY?: number | null;
    watermarkScale?: number;
}

export interface SessionState {
    connected: boolean;
    room: RoomState | null;
    participantId: string | null;
    isAdmin: boolean;
    error: string | null;
    reconnecting: boolean;
    reconnectAttempt: number;
    networkOffline: boolean;
}

// Reconnection configuration
// Fast first retry keeps live review hiccups short; exponential backoff still
// ramps quickly if the network or server is genuinely unavailable.
const RECONNECT_BASE_DELAY = 250; // 200-300ms after jitter on the first retry
const RECONNECT_MAX_DELAY = 30000; // 30 seconds
const RECONNECT_MAX_ATTEMPTS = 10;

// Exponential backoff with ±20% jitter. Jitter desynchronizes clients so a
// server restart doesn't trigger a synchronized reconnect stampede. Module-
// scoped and exported so the backoff curve can be unit-tested directly (it
// captures no per-store state — only the constants above).
export function getReconnectDelay(attempt: number): number {
    const exponentialDelay = RECONNECT_BASE_DELAY * Math.pow(2, attempt);
    const cappedDelay = Math.min(exponentialDelay, RECONNECT_MAX_DELAY);
    const jitter = cappedDelay * (0.8 + Math.random() * 0.4);
    return Math.round(jitter);
}

// Browser-side WebSocket backpressure guardrails. `bufferedAmount` is bytes
// queued in the browser but not yet written to the network; once it grows,
// sending more real-time cursor samples only increases end-to-end latency.
const DISPOSABLE_MESSAGE_TYPES = new Set(['cursor', 'client:debug']);
const DISPOSABLE_BUFFERED_DROP_BYTES = 16 * 1024;
const CRITICAL_BUFFERED_RECONNECT_BYTES = 1024 * 1024;
const CLIENT_RECONNECT_CLOSE_CODE = 4001;
const CLIENT_OFFLINE_CLOSE_CODE = 4002;

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
        reconnectAttempt: 0,
        networkOffline: false
    });

    let ws: WebSocket | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let networkListenersAttached = false;
    const messageHandlers = new Map<string, Array<(payload: unknown) => void>>();
    // Callbacks invoked after a successful re-open (NOT the first open)
    const reconnectCallbacks = new Set<() => void>();
    // Tracks whether we've had at least one successful open for this session
    let hasConnectedOnce = false;

    // Store connection params for reconnection
    let connectionParams: { roomSlug: string; name: string; participantId: string } | null = null;


    function browserOffline(): boolean {
        return typeof navigator !== 'undefined' && navigator.onLine === false;
    }

    function clearReconnectTimer() {
        if (reconnectTimer) {
            clearTimeout(reconnectTimer);
            reconnectTimer = null;
        }
    }

    function attachNetworkListeners() {
        if (networkListenersAttached || typeof window === 'undefined') return;
        window.addEventListener('offline', handleOffline);
        window.addEventListener('online', handleOnline);
        networkListenersAttached = true;
    }

    function detachNetworkListeners() {
        if (!networkListenersAttached || typeof window === 'undefined') return;
        window.removeEventListener('offline', handleOffline);
        window.removeEventListener('online', handleOnline);
        networkListenersAttached = false;
    }

    function pauseReconnectWhileOffline() {
        clearReconnectTimer();
        state.connected = false;
        state.reconnecting = connectionParams !== null;
        state.networkOffline = true;
        state.error = null;
    }

    function scheduleReconnect(resetAttempt = false) {
        if (!connectionParams) return;

        if (browserOffline()) {
            pauseReconnectWhileOffline();
            return;
        }

        state.networkOffline = false;
        state.error = null;
        state.reconnecting = true;
        if (resetAttempt) {
            state.reconnectAttempt = 0;
        }

        if (state.reconnectAttempt < RECONNECT_MAX_ATTEMPTS) {
            const delay = getReconnectDelay(state.reconnectAttempt);
            state.reconnectAttempt++;

            if (DEBUG) console.log(`Reconnecting in ${delay}ms (attempt ${state.reconnectAttempt}/${RECONNECT_MAX_ATTEMPTS})`);

            clearReconnectTimer();
            reconnectTimer = setTimeout(() => {
                reconnectTimer = null;
                if (connectionParams) {
                    connect(connectionParams.roomSlug, connectionParams.name, connectionParams.participantId);
                }
            }, delay);
        } else {
            state.reconnecting = false;
            state.error = 'Connection lost. Please refresh the page.';
            console.error('Max reconnection attempts reached');
        }
    }

    function handleOffline() {
        pauseReconnectWhileOffline();
        if (ws && ws.readyState !== WebSocket.CLOSED && ws.readyState !== WebSocket.CLOSING) {
            ws.close(CLIENT_OFFLINE_CLOSE_CODE, 'browser offline');
        }
    }

    function handleOnline() {
        state.networkOffline = false;
        if (!connectionParams || state.connected) return;

        clearReconnectTimer();
        state.error = null;
        state.reconnecting = true;
        state.reconnectAttempt = 0;
        connect(connectionParams.roomSlug, connectionParams.name, connectionParams.participantId);
    }

    function connect(roomSlug: string, name: string, participantId = '') {
        attachNetworkListeners();
        // Clear any pending reconnect timer so a manual connect plus a
        // pending timer can't create parallel sockets.
        clearReconnectTimer();

        // Store params for reconnection
        connectionParams = { roomSlug, name, participantId };
        // Our own participant id — used to identify the local cursor (laser),
        // self in rosters, etc. Set here (and re-applied on every reconnect via
        // connectionParams) so it survives socket drops. Empty string from
        // legacy/test callers leaves it null.
        state.participantId = participantId || null;

        if (ws) {
            // Detach the old socket's onclose to prevent spurious reconnection
            ws.onclose = null;
            ws.close();
        }

        if (browserOffline()) {
            pauseReconnectWhileOffline();
            return;
        }

        const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        const url = `${protocol}//${window.location.host}/ws/room/${roomSlug}?name=${encodeURIComponent(name)}`;

        ws = new WebSocket(url);

        ws.onopen = () => {
            const isReconnect = hasConnectedOnce;
            hasConnectedOnce = true;
            state.connected = true;
            state.error = null;
            state.reconnecting = false;
            state.reconnectAttempt = 0;
            state.networkOffline = false;
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
                scheduleReconnect(e.code === CLIENT_OFFLINE_CLOSE_CODE);
            }
        };

        ws.onerror = (e) => {
            console.error('WebSocket error', e);
            if (!browserOffline()) {
                state.error = 'Connection error';
            }
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
                        scheduledAt?: string;
                        openedAt?: string;
                        earlyOpenMinutes?: number;
                        waitingRoomEnabled?: boolean;
                        watermarkMode?: 'none' | 'text' | 'logo' | 'both';
                        watermarkText?: string;
                        watermarkLogoUrl?: string;
                        watermarkLogoPosition?: 'top-left' | 'top-right' | 'bottom-left' | 'bottom-right';
                        watermarkOpacity?: number;
                        watermarkPosX?: number | null;
                        watermarkPosY?: number | null;
                        watermarkScale?: number;
                    };
                    participants: SessionParticipant[];
                    isLive: boolean;
                    iceServers: RTCIceServer[];
                    screenShare?: { participantId: string; name: string } | null;
                    hiddenWebcams?: string[] | null;
                };
                state.room = {
                    slug: roomState.room.slug,
                    name: roomState.room.name,
                    isLive: roomState.isLive,
                    participants: roomState.participants,
                    scheduledAt: roomState.room.scheduledAt ?? null,
                    openedAt: roomState.room.openedAt ?? null,
                    earlyOpenMinutes: roomState.room.earlyOpenMinutes ?? 10,
                    waitingRoomEnabled: roomState.room.waitingRoomEnabled ?? false,
                    // Map watermark configuration
                    watermarkMode: roomState.room.watermarkMode || 'none',
                    watermarkText: roomState.room.watermarkText,
                    watermarkLogoUrl: roomState.room.watermarkLogoUrl,
                    watermarkLogoPosition: roomState.room.watermarkLogoPosition || 'bottom-right',
                    watermarkOpacity: roomState.room.watermarkOpacity ?? 0.3,
                    watermarkPosX: roomState.room.watermarkPosX ?? null,
                    watermarkPosY: roomState.room.watermarkPosY ?? null,
                    watermarkScale: roomState.room.watermarkScale ?? 1
                };
                // Emit ICE servers for WebRTC connection
                messageHandlers.get('iceServers')?.forEach(h => h(roomState.iceServers));
                // Emit screen share state for late joiners
                if (roomState.screenShare) {
                    messageHandlers.get('screenshare:started')?.forEach(h => h(roomState.screenShare));
                }
                // Emit hidden webcam state for late joiners/reconnects. Live
                // changes use the same websocket message directly.
                for (const participantId of roomState.hiddenWebcams ?? []) {
                    messageHandlers.get('webcam:visibility')?.forEach(h => h({ participantId, visible: false }));
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
                const joinedData = msg.payload as { participant: SessionParticipant };
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
                const updatedData = msg.payload as { participant: Partial<SessionParticipant> & { id: string } };
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

    function send(type: string, payload: unknown): boolean {
        if (!ws || ws.readyState !== WebSocket.OPEN) {
            return false;
        }

        const queuedBytes = ws.bufferedAmount;
        if (DISPOSABLE_MESSAGE_TYPES.has(type) && queuedBytes > DISPOSABLE_BUFFERED_DROP_BYTES) {
            if (DEBUG) console.debug('Dropping disposable WS message under backpressure', { type, queuedBytes });
            return false;
        }

        if (queuedBytes > CRITICAL_BUFFERED_RECONNECT_BYTES) {
            console.warn('WebSocket send buffer is congested; reconnecting before sending critical message', {
                type,
                queuedBytes
            });
            ws.close(CLIENT_RECONNECT_CLOSE_CODE, 'send buffer congested');
            return false;
        }

        try {
            ws.send(JSON.stringify({ type, payload }));
            return true;
        } catch (err) {
            console.error('WebSocket send failed', err);
            ws.close(CLIENT_RECONNECT_CLOSE_CODE, 'send failed');
            return false;
        }
    }

    // Registers a handler for a message type. Returns an unsubscribe function.
    // Components should call the returned dispose in onDestroy — otherwise
    // handlers accumulate across navigations and fire with stale closures.
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
        clearReconnectTimer();
        detachNetworkListeners();
        connectionParams = null;
        hasConnectedOnce = false;
        if (ws) {
            ws.onclose = null;
            ws.close(1000);
            ws = null;
        }
        // Purge any handlers captured from the previous lifecycle so a fresh
        // connect() starts with a clean slate.
        messageHandlers.clear();
        reconnectCallbacks.clear();
        state.connected = false;
        state.room = null;
        state.participantId = null;
        state.isAdmin = false;
        state.error = null;
        state.reconnecting = false;
        state.reconnectAttempt = 0;
        state.networkOffline = false;
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
