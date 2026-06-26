import { defineConfig } from 'vitest/config';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import { fileURLToPath } from 'node:url';

const setupTests = fileURLToPath(new URL('./src/lib/setupTests.ts', import.meta.url));
const libAlias = fileURLToPath(new URL('./src/lib', import.meta.url));
const appAlias = fileURLToPath(new URL('./src/mocks/app', import.meta.url));

export default defineConfig({
    plugins: [svelte({ hot: !process.env.VITEST })],
    resolve: {
        alias: {
            '$lib': libAlias,
            '$app': appAlias
        }
    },
    test: {
        include: ['src/**/*.{test,spec}.{js,ts}'],
        environment: 'jsdom',
        setupFiles: [setupTests],
        globals: true
    }
});
