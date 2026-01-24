// API client for Chromatic backend
// Uses httpOnly cookies for secure authentication (not vulnerable to XSS)

const API_BASE = '';

export async function apiGet<T>(path: string): Promise<T> {
    const res = await fetch(`${API_BASE}${path}`, {
        credentials: 'include' // Include httpOnly cookies
    });

    if (!res.ok) {
        throw new Error(await res.text());
    }

    return res.json();
}

export async function apiPost<T>(path: string, body?: unknown): Promise<T> {
    const res = await fetch(`${API_BASE}${path}`, {
        method: 'POST',
        credentials: 'include', // Include httpOnly cookies
        headers: {
            'Content-Type': 'application/json'
        },
        body: body ? JSON.stringify(body) : undefined
    });

    if (!res.ok) {
        throw new Error(await res.text());
    }

    return res.json();
}

export async function apiPatch<T>(path: string, body: unknown): Promise<T> {
    const res = await fetch(`${API_BASE}${path}`, {
        method: 'PATCH',
        credentials: 'include', // Include httpOnly cookies
        headers: {
            'Content-Type': 'application/json'
        },
        body: JSON.stringify(body)
    });

    if (!res.ok) {
        throw new Error(await res.text());
    }

    return res.json();
}

export async function apiDelete(path: string): Promise<void> {
    const res = await fetch(`${API_BASE}${path}`, {
        method: 'DELETE',
        credentials: 'include' // Include httpOnly cookies
    });

    if (!res.ok) {
        throw new Error(await res.text());
    }
}

// Authentication functions
export const auth = {
    login: async (token: string): Promise<{ success: boolean; message: string }> => {
        return apiPost('/api/auth/login', { token });
    },
    logout: async (): Promise<{ success: boolean; message: string }> => {
        return apiPost('/api/auth/logout');
    }
};

// Types
export interface Room {
    id: string;
    slug: string;
    name: string;
    scheduledAt?: string;
    durationMinutes?: number;
    hasPassword: boolean;
    waitingRoomEnabled: boolean;
    streamKeyId?: string;
    watermarkMode: string;
    watermarkText?: string;
    status: 'pending' | 'live' | 'ended';
    createdAt: string;
    startedAt?: string;
    endedAt?: string;
}

export interface StreamKey {
    id: string;
    name: string;
    keyToken: string;
    createdAt: string;
}

export interface Participant {
    id: string;
    name: string;
    role: 'admin' | 'viewer';
    color: string;
    audioEnabled: boolean;
    videoEnabled: boolean;
}

export interface RoomInfo {
    name: string;
    hasPassword: boolean;
    waitingRoomEnabled: boolean;
    status: string;
    scheduledAt?: string;
}

// API functions
export const rooms = {
    list: (status?: string) => apiGet<Room[]>(`/api/rooms${status ? `?status=${status}` : ''}`),
    get: (slug: string) => apiGet<Room>(`/api/rooms/${slug}`),
    create: (data: Partial<Room>) => apiPost<Room>('/api/rooms', data),
    update: (slug: string, data: Partial<Room>) => apiPatch<Room>(`/api/rooms/${slug}`, data),
    delete: (slug: string) => apiDelete(`/api/rooms/${slug}`),
    end: (slug: string) => apiPost(`/api/rooms/${slug}/end`),
    info: (slug: string) => apiGet<RoomInfo>(`/api/rooms/${slug}/info`),
    join: (slug: string, name: string, password?: string) =>
        apiPost<{ participantId: string; token: string; isAdmitted: boolean; waitingRoom: boolean; color: string }>(
            `/api/rooms/${slug}/join`,
            { name, password }
        ),
    listWaiting: (slug: string) => apiGet<{ id: string; name: string; joinedAt: string }[]>(`/api/rooms/${slug}/waiting`),
    admit: (slug: string, participantId: string) => apiPost(`/api/rooms/${slug}/admit/${participantId}`),
    admitAll: (slug: string) => apiPost(`/api/rooms/${slug}/admit-all`),
    checkStatus: (slug: string, participantId: string) =>
        apiGet<{ isAdmitted: boolean; roomStatus: string }>(`/api/rooms/${slug}/status/${participantId}`)
};

export const streamKeys = {
    list: () => apiGet<StreamKey[]>('/api/stream-keys'),
    create: (name: string) => apiPost<StreamKey>('/api/stream-keys', { name }),
    delete: (id: string) => apiDelete(`/api/stream-keys/${id}`)
};
