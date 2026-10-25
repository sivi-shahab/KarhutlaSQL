import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Hasil build di-embed ke binary (internal/webui/dist, go:embed).
export default defineConfig({
  plugins: [vue()],
  base: './',
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 1500,
  },
  server: {
    // npm run dev: API diteruskan ke server lokal (--http-port 8444).
    proxy: { '/api': 'http://127.0.0.1:8444' },
  },
})
