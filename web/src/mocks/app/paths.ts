// Test mock for $app/paths (SvelteKit). The app is served at the root, so base
// is empty in tests; real builds use SvelteKit's generated paths module.
export const base = '';
export const assets = '';
export function resolveRoute(id: string): string {
    return id;
}
