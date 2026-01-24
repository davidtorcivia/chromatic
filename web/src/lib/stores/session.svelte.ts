// Session store - manages WebSocket and room state

import type { Participant } from '$lib/api/client';

export interface RoomState {
    slug: string;
    name: string;
    isLive: boolean;
    participants: Participant[];
}

export interface SessionState {
    connected: boolean;
    room: RoomState | null;
    participantId: string | null;
    isAdmin: boolean;
    error: string | null;
}

// Create reactive state using Svelte 5 runes
export function createSessionStore() {
    let state = $state<SessionState>({
        connected: false,
        room: null,
        participantId: null,
        isAdmin: false,
        error: null
    });

    let ws: WebSocket | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    const messageHandlers = new Map<string, (payload: unknown) => void>();

    function connect(roomSlug: string, token: string, name: string) {
        if (ws) {
            ws.close();
        }

        const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        const url = `${protocol}//${window.location.host}/ws/room/${roomSlug}?token=${token}&name=${encodeURIComponent(name)}`;

        ws = new WebSocket(url);

        ws.onopen = () => {
            state.connected = true;
            state.error = null;
            console.log('WebSocket connected');
        };

        ws.onclose = (e) => {
            state.connected = false;
            console.log('WebSocket closed', e.code, e.reason);

            // Attempt reconnection if not intentional
            if (e.code !== 1000) {
                reconnectTimer = setTimeout(() => {
                    connect(roomSlug, token, name);
                }, 3000);
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
                    room: { slug: string; name: string };
                    participants: Participant[];
                    isLive: boolean;
                    iceServers: RTCIceServer[];
                };
                state.room = {
                    slug: roomState.room.slug,
                    name: roomState.room.name,
                    isLive: roomState.isLive,
                    participants: roomState.participants
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
        }
        if (ws) {
            ws.close(1000);
            ws = null;
        }
        state = {
            connected: false,
            room: null,
            participantId: null,
            isAdmin: false,
            error: null
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
