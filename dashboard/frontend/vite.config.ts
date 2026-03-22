import { defineConfig } from 'vite';
import { sveltekit } from '@sveltejs/kit/vite';

export default defineConfig({
  plugins: [sveltekit()],

  server: {
    host: '127.0.0.1',
    port: 5173,
    // In development: proxy all /api requests to the Go backend.
    // In production: Go serves both static files and /api from the same origin.
    proxy: {
      '/api': {
        target:       'http://127.0.0.1:7374',
        changeOrigin: false,
      },
    },
  },
});
