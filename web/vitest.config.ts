import { defineConfig } from 'vitest/config';
import { svelte } from '@sveltejs/vite-plugin-svelte';

export default defineConfig({
    plugins: [svelte({ hot: !process.env.VITEST })],
    test: {
        include: ['src/**/*.{test,spec}.{js,ts}'],
        environment: 'jsdom',
        setupFiles: ['src/lib/setupTests.ts'],
        globals: true,
        alias: {
            '$lib': '/src/lib',
            '$app': '/src/mocks/app'
        }
    }
});
