import { defineConfig } from 'vitest/config';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import path from 'node:path';

const root = process.cwd();
const setupTests = path.resolve(root, 'src/lib/setupTests.ts');
const libAlias = path.resolve(root, 'src/lib');
const appAlias = path.resolve(root, 'src/mocks/app');

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
