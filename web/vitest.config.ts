import { defineConfig } from 'vitest/config';
import { svelte } from '@sveltejs/vite-plugin-svelte';

export default defineConfig({
    root: '.',
    plugins: [svelte({ hot: !process.env.VITEST })],
    resolve: {
        alias: {
            '$lib': '/src/lib',
            '$app': '/src/mocks/app'
        }
    },
    test: {
        include: ['src/**/*.{test,spec}.{js,ts}'],
        environment: 'jsdom',
        setupFiles: ['./src/lib/setupTests.ts'],
        globals: true
    }
});
