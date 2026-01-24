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
    const messageHandlers = new Map<string, (payload: unknown) => void>();

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
        // Store params for reconnection
        connectionParams = { roomSlug, token, name };

        if (ws) {
            ws.close();
        }

        const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        const url = `${protocol}//${window.location.host}/ws/room/${roomSlug}?token=${token}&name=${encodeURIComponent(name)}`;

        ws = new WebSocket(url);

        ws.onopen = () => {
            state.connected = true;
            state.error = null;
            state.reconnecting = false;
            state.reconnectAttempt = 0;
            console.log('WebSocket connected');
        };

        ws.onclose = (e) => {
            state.connected = false;
            console.log('WebSocket closed', e.code, e.reason);

            // Attempt reconnection if not intentional close
            if (e.code !== 1000 && connectionParams) {
                if (state.reconnectAttempt < RECONNECT_MAX_ATTEMPTS) {
                    state.reconnecting = true;
                    const delay = getReconnectDelay(state.reconnectAttempt);
                    state.reconnectAttempt++;

                    console.log(`Reconnecting in ${delay}ms (attempt ${state.reconnectAttempt}/${RECONNECT_MAX_ATTEMPTS})`);

                    reconnectTimer = setTimeout(() => {
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
        console.log('WS message:', msg.type, msg.payload);

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
                messageHandlers.get('iceServers')?.(roomState.iceServers);
                break;

            case 'room:live':
                if (state.room) {
                    state.room.isLive = true;
                }
                break;

            case 'room:ended':
                if (state.room) {
                    state.room.isLive = false;
                }
                messageHandlers.get('room:ended')?.({});
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
                break;

            case 'participant:updated':
                const updatedData = msg.payload as { participant: Partial<Participant> & { id: string } };
                if (state.room) {
                    state.room.participants = state.room.participants.map(p =>
                        p.id === updatedData.participant.id ? { ...p, ...updatedData.participant } : p
                    );
                }
                break;

            default:
                // Forward to registered handlers
                messageHandlers.get(msg.type)?.(msg.payload);
        }
    }

    function send(type: string, payload: unknown) {
        if (ws && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type, payload }));
        }
    }

    function onMessage(type: string, handler: (payload: unknown) => void) {
        messageHandlers.set(type, handler);
    }

    function disconnect() {
        if (reconnectTimer) {
            clearTimeout(reconnectTimer);
            reconnectTimer = null;
        }
        connectionParams = null;
        if (ws) {
            ws.close(1000);
            ws = null;
        }
        state = {
            connected: false,
            room: null,
            participantId: null,
            isAdmin: false,
            error: null,
            reconnecting: false,
            reconnectAttempt: 0
        };
    }

    return {
        get state() { return state; },
        connect,
        disconnect,
        send,
        onMessage
    };
}

// Singleton instance
export const session = createSessionStore();
