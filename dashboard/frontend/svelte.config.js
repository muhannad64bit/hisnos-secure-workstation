import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
const config = {
  preprocess: vitePreprocess(),
  kit: {
    adapter: adapter({
      // Go backend serves the built output from dashboard/frontend/dist/
      pages:    'dist',
      assets:   'dist',
      // SPA fallback: Go serves index.html for all non-API routes
      fallback: 'index.html',
      strict:   false,
    }),
  },
};

export default config;
