import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

const generatedServerInternal = '/.svelte-kit/generated/server/internal.js';
const sharedServerRuntime = '/node_modules/@sveltejs/kit/src/runtime/shared-server.js';

export default defineConfig({
	plugins: [
		{
			name: 'chromatic-sveltekit-windows-shared-server-resolve',
			enforce: 'pre',
			resolveId(source, importer) {
				const normalizedImporter = importer?.replaceAll('\\', '/');
				if (
					normalizedImporter?.endsWith(generatedServerInternal) &&
					source.includes('node_modules/@sveltejs/kit/src/runtime/shared-server.js')
				) {
					return `${normalizedImporter.slice(0, -generatedServerInternal.length)}${sharedServerRuntime}`;
				}
			}
		},
		sveltekit()
	]
});
