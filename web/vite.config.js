import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// The Go server serves the build from web/dist; `npm run dev` proxies the
// API to a server running on :8080.
export default defineConfig({
  plugins: [svelte()],
  build: { emptyOutDir: false },
  server: { proxy: { '/api': 'http://localhost:8080', '/mcp': 'http://localhost:8080' } },
})
