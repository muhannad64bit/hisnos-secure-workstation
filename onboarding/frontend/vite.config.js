import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

export default defineConfig({
	plugins: [sveltekit()],
	server: {
		// Dev-mode proxy: forward /api/* to Go backend on 9444.
		proxy: {
			'/api': 'http://localhost:9444',
		},
	},
	build: {
		// Keep source maps for easier debugging of the wizard.
		sourcemap: false,
	},
});
