import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

// `npm run dev` proxies API calls to a local voipd (VOIPD_HTTP_LISTEN).
const backend = process.env.VOIPD_URL ?? 'http://127.0.0.1:18080'

export default defineConfig({
  plugins: [svelte()],
  build: { outDir: 'dist', emptyOutDir: true, assetsDir: 'assets', target: 'es2022' },
  server: {
    proxy: {
      '/api': { target: backend, changeOrigin: false },
      '/auth': { target: backend, changeOrigin: false },
    },
  },
})
