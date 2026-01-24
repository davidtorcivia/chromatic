import { writable, readable } from 'svelte/store';

// Mock for $app/stores
export const page = readable({
    url: new URL('http://localhost'),
    params: {},
    route: { id: '' },
    status: 200,
    error: null,
    data: {},
    form: null
});

export const navigating = readable(null);
export const updated = readable(false);
