// API client for Chromatic backend
// Uses httpOnly cookies for secure authentication (not vulnerable to XSS)

const API_BASE = '';

export class ApiError extends Error {
    constructor(
        public status: number,
        message: string
    ) {
        super(message);
        this.name = "ApiError";
    }
}

export async function apiGet<T>(path: string): Promise<T> {
    const res = await fetch(`${API_BASE}${path}`, {
        credentials: 'include' // Include httpOnly cookies
    });

    if (!res.ok) {
        throw new ApiError(res.status, await res.text());
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
        throw new ApiError(res.status, await res.text());
    }

    // Several endpoints (admit, deny, end) reply 204 No Content; res.json()
    // would throw on the empty body.
    const text = await res.text();
    return (text ? JSON.parse(text) : undefined) as T;
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
        throw new ApiError(res.status, await res.text());
    }

    return res.json();
}

export async function apiDelete(path: string): Promise<void> {
    const res = await fetch(`${API_BASE}${path}`, {
        method: 'DELETE',
        credentials: 'include' // Include httpOnly cookies
    });

    if (!res.ok) {
        throw new ApiError(res.status, await res.text());
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
    /** Minutes before scheduledAt that guests may enter the countdown lobby (0-120, default 10) */
    earlyOpenMinutes?: number;
    /** Set when an admin opened the room early or the first stream arrived */
    openedAt?: string;
    hasPassword: boolean;
    waitingRoomEnabled: boolean;
    streamKeyId?: string;
    watermarkMode: string;
    watermarkText?: string;
    watermarkLogoPosition?: string;
    watermarkOpacity?: number;
    /** Fractional center (0-1) of the watermark; absent = legacy placement */
    watermarkPosX?: number;
    watermarkPosY?: number;
    /** Size multiplier for the watermark text/logo (0.25-3, default 1) */
    watermarkScale?: number;
    /** Per-room participant limit (1-100); absent = global default (20) */
    maxParticipants?: number;
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
    earlyOpenMinutes?: number;
    openedAt?: string;
    /** Server clock at response time — anchor countdowns to this, not the local clock */
    serverTime?: string;
}

/** Countdown-lobby info returned by join when a scheduled room hasn't opened yet */
export interface LobbyInfo {
    scheduledAt: string;
    opensAt: string;
    waitingRoomEnabled: boolean;
}

export interface JoinResult {
    participantId: string;
    token?: string;
    isAdmitted: boolean;
    waitingRoom: boolean;
    color: string;
    name: string;
    role: 'admin' | 'viewer';
    serverTime?: string;
    lobby?: LobbyInfo;
}

// API functions
export const rooms = {
    list: (status?: string) => apiGet<Room[]>(`/api/rooms${status ? `?status=${status}` : ''}`),
    get: (slug: string) => apiGet<Room>(`/api/rooms/${slug}`),
    create: (data: Partial<Room>) => apiPost<Room>('/api/rooms', data),
    update: (slug: string, data: Partial<Room>) => apiPatch<Room>(`/api/rooms/${slug}`, data),
    delete: (slug: string, deleteFiles = false) =>
        apiDelete(`/api/rooms/${slug}${deleteFiles ? '?deleteFiles=true' : ''}`),
    // Bulk delete: loops the single-room DELETE endpoint and reports which
    // slugs failed so callers can surface partial failures.
    deleteMany: async (slugs: string[], deleteFiles = false): Promise<{ failed: string[] }> => {
        const failed: string[] = [];
        for (const slug of slugs) {
            try {
                await apiDelete(`/api/rooms/${slug}${deleteFiles ? '?deleteFiles=true' : ''}`);
            } catch {
                failed.push(slug);
            }
        }
        return { failed };
    },
    end: (slug: string) => apiPost(`/api/rooms/${slug}/end`),
    /** Open a scheduled room ahead of time (admin). Auto-admits the countdown lobby. */
    open: (slug: string) => apiPost<{ openedAt: string }>(`/api/rooms/${slug}/open`),
    // Bulk end: loops the single-room end endpoint, reporting failures.
    endMany: async (slugs: string[]): Promise<{ failed: string[] }> => {
        const failed: string[] = [];
        for (const slug of slugs) {
            try {
                await apiPost(`/api/rooms/${slug}/end`);
            } catch {
                failed.push(slug);
            }
        }
        return { failed };
    },
    info: (slug: string) => apiGet<RoomInfo>(`/api/rooms/${slug}/info`),
    join: (slug: string, name: string, password?: string, adminToken?: string) =>
        apiPost<JoinResult>(`/api/rooms/${slug}/join`, { name, password, adminToken }),
    listWaiting: (slug: string) => apiGet<{ id: string; name: string; joinedAt: string }[]>(`/api/rooms/${slug}/waiting`),
    admit: (slug: string, participantId: string) => apiPost(`/api/rooms/${slug}/admit/${participantId}`),
    admitAll: (slug: string) => apiPost(`/api/rooms/${slug}/admit-all`),
    deny: (slug: string, participantId: string) => apiPost(`/api/rooms/${slug}/deny/${participantId}`),
    checkStatus: async (slug: string, participantId: string): Promise<{ isAdmitted: boolean; roomStatus: string; serverTime?: string }> => {
        // Browser joins use the HttpOnly join cookie set by POST /join so the
        // signed join token does not need to live in localStorage or URLs.
        const res = await fetch(`${API_BASE}/api/rooms/${slug}/status/${participantId}`, {
            credentials: 'include',
        });
        if (!res.ok) {
            throw new Error(await res.text());
        }
        return res.json();
    }
};

export interface RoomFile {
    id: string;
    originalName: string;
    mimeType: string;
    sizeBytes: number;
    uploaderName: string;
    createdAt: number;
    url: string;
    thumbnailUrl?: string;
}

// Admin file review/moderation (room settings)
export const roomFiles = {
    list: (slug: string) => apiGet<{ files: RoomFile[] }>(`/api/rooms/${slug}/files`),
    delete: (fileId: string) => apiDelete(`/api/files/${fileId}`)
};

export const streamKeys = {
    list: () => apiGet<StreamKey[]>('/api/stream-keys'),
    create: (name: string) => apiPost<StreamKey>('/api/stream-keys', { name }),
    delete: (id: string) => apiDelete(`/api/stream-keys/${id}`)
};

// Config types
export interface AppConfig {
    defaultWatermarkText?: string;
    defaultWatermarkLogoPath?: string;
    defaultWatermarkLogoUrl?: string;
    turnExternalUrl?: string;
    turnExternalUsername?: string;
    hasTurnCredential: boolean;
    turnMode: "self-hosted" | "external" | "hybrid";
    turnCloudflareConfigured: boolean;
    publicUrl: string;
    whipFormat: string;
}

export interface UpdateConfigRequest {
    defaultWatermarkText?: string;
    turnExternalUrl?: string;
    turnExternalUsername?: string;
    turnExternalCredential?: string;
}

// TURN test types
export interface TURNTestResult {
    server: string;
    reachable: boolean;
    latency?: number;
    error?: string;
    protocol?: string;
    testType: string;
}

export interface TURNTestResponse {
    success: boolean;
    results: TURNTestResult[];
    message?: string;
}

// Config API functions
export const appConfig = {
    get: () => apiGet<AppConfig>('/api/config'),
    update: (data: UpdateConfigRequest) => apiPatch<AppConfig>('/api/config', data),
    uploadLogo: async (file: File): Promise<{ logoUrl: string; path: string }> => {
        const formData = new FormData();
        formData.append('logo', file);
        const res = await fetch(`${API_BASE}/api/config/logo`, {
            method: 'POST',
            credentials: 'include',
            body: formData
        });
        if (!res.ok) {
            throw new Error(await res.text());
        }
        return res.json();
    },
    deleteLogo: () => apiDelete('/api/config/logo'),
    testTurn: () => apiPost<TURNTestResponse>('/api/config/test-turn')
};

// Setup status types — mirrors the backend SetupStatusResponse contract.
export type SetupCheckStatus = 'ready' | 'needs-action' | 'warning' | 'optional';

export interface SetupCheck {
    id: string;
    title: string;
    status: SetupCheckStatus;
    required: boolean;
    summary: string;
    detail?: string;
    action?: string;
}

export interface SetupProgress {
    ready: number;
    required: number;
    total: number;
}

export interface SetupFacts {
    publicUrl: string;
    productionMode: boolean;
    allowedOrigins: string[];
    turnMode: 'self-hosted' | 'external' | 'hybrid' | string;
    turnCloudflareConfigured: boolean;
    turnStaticConfigured: boolean;
    hasTurnCredential: boolean;
    turnLastTestedAt?: string;
    turnLastTestSuccess: boolean;
    turnLastTestMessage?: string;
    turnLastTestValidForCurrentConfig: boolean;
    streamKeyCount: number;
    firstStreamKeyId?: string;
    roomCount: number;
    firstRoomSlug?: string;
    defaultWatermarkText?: string;
    defaultWatermarkLogoUrl?: string;
}

export interface SetupStatusResponse {
    completedAt?: string;
    dismissedAt?: string;
    readyToComplete: boolean;
    firstRun: boolean;
    requiresAttention: boolean;
    progress: SetupProgress;
    checks: SetupCheck[];
    facts: SetupFacts;
}

// Thrown by setup.complete() when the server returns 409 Conflict because a
// required check is no longer ready. Carries the fresh server status so the UI
// can rerender without another round-trip.
export class SetupIncompleteError extends ApiError {
    constructor(public statusResponse: SetupStatusResponse) {
        super(409, 'Setup is incomplete');
        this.name = 'SetupIncompleteError';
    }
}

// Setup API functions
export const setup = {
    status: () => apiGet<SetupStatusResponse>('/api/setup/status'),
    complete: async (): Promise<SetupStatusResponse> => {
        const res = await fetch('/api/setup/complete', {
            method: 'POST',
            credentials: 'include',
            headers: { 'Content-Type': 'application/json' }
        });
        const text = await res.text();
        // 409 carries the fresh status so the UI can rerender without another
        // round-trip; defend against a non-JSON body (proxy/gateway error) by
        // falling back to a plain ApiError instead of throwing SyntaxError.
        if (res.status === 409) {
            let data: SetupStatusResponse | undefined;
            try {
                data = text ? (JSON.parse(text) as SetupStatusResponse) : undefined;
            } catch {
                data = undefined;
            }
            if (data) throw new SetupIncompleteError(data);
            throw new ApiError(409, text || 'Setup is incomplete');
        }
        if (!res.ok) throw new ApiError(res.status, text);
        return (text ? JSON.parse(text) : undefined) as SetupStatusResponse;
    },
    dismiss: () => apiPost<SetupStatusResponse>('/api/setup/dismiss')
};

// File upload types
export interface UploadedFile {
    id: string;
    originalName: string;
    mimeType: string;
    sizeBytes: number;
    url: string;
    thumbnailUrl?: string;
}

// File upload function with progress callback
export async function uploadFile(
    roomSlug: string,
    file: File,
    onProgress?: (percent: number) => void,
    signal?: AbortSignal
): Promise<UploadedFile> {
    return postFile(`${API_BASE}/api/rooms/${roomSlug}/files`, file, onProgress, signal);
}

// Frame grabs go to their own endpoint. The server records origin='frame-grab'
// from the route, not from anything we send, and watermarks the frame per
// requester on download.
export async function uploadGrabbedFrame(
    roomSlug: string,
    file: File,
    onProgress?: (percent: number) => void,
    signal?: AbortSignal
): Promise<UploadedFile> {
    return postFile(`${API_BASE}/api/rooms/${roomSlug}/grab`, file, onProgress, signal);
}

function postFile(
    url: string,
    file: File,
    onProgress?: (percent: number) => void,
    signal?: AbortSignal
): Promise<UploadedFile> {
    return new Promise((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        const formData = new FormData();
        formData.append('file', file);

        const abortUpload = () => {
            xhr.abort();
        };
        const cleanup = () => {
            signal?.removeEventListener('abort', abortUpload);
        };

        if (signal?.aborted) {
            reject(new Error('Upload cancelled'));
            return;
        }
        signal?.addEventListener('abort', abortUpload, { once: true });

        xhr.upload.addEventListener('progress', (e) => {
            if (e.lengthComputable && onProgress) {
                onProgress(Math.round((e.loaded / e.total) * 100));
            }
        });

        xhr.addEventListener('load', () => {
            cleanup();
            if (xhr.status >= 200 && xhr.status < 300) {
                try {
                    resolve(JSON.parse(xhr.responseText));
                } catch {
                    reject(new Error('Invalid response'));
                }
            } else {
                reject(new Error(xhr.responseText || 'Upload failed'));
            }
        });

        xhr.addEventListener('error', () => {
            cleanup();
            reject(new Error('Network error'));
        });

        xhr.addEventListener('abort', () => {
            cleanup();
            reject(new Error('Upload cancelled'));
        });

        xhr.open('POST', url);
        xhr.withCredentials = true;
        xhr.send(formData);
    });
}
