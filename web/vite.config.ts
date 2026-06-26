import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

const generatedMarker = '/.svelte-kit/generated/';
const kitPackageMarker = 'node_modules/@sveltejs/kit/';

export default defineConfig({
	plugins: [
		{
			name: 'chromatic-sveltekit-windows-generated-kit-resolve',
			enforce: 'pre',
			resolveId(source, importer) {
				const normalizedImporter = importer?.replaceAll('\\', '/');
				const normalizedSource = source.replaceAll('\\', '/');
				const generatedAt = normalizedImporter?.indexOf(generatedMarker) ?? -1;
				const kitAt = normalizedSource.indexOf(kitPackageMarker);
				if (
					normalizedImporter &&
					generatedAt >= 0 &&
					kitAt >= 0 &&
					normalizedSource.includes('../')
				) {
					const resolved = `${normalizedImporter.slice(0, generatedAt)}/${normalizedSource.slice(kitAt)}`;
					const leaf = resolved.slice(resolved.lastIndexOf('/') + 1);
					return leaf.includes('.') ? resolved : `${resolved}.js`;
				}
			}
		},
		sveltekit()
	]
});
