import adapter from '@sveltejs/adapter-static';

/** @type {import('@sveltejs/kit').Config} */
const config = {
	kit: {
		// Output to ../backend/dist so the Go binary can embed it.
		adapter: adapter({
			pages:    '../backend/dist',
			assets:   '../backend/dist',
			fallback: 'index.html',
			strict:   false,
		}),
		// No SSR — pure SPA served by the Go binary.
		prerender: { entries: [] },
	},
};

export default config;
