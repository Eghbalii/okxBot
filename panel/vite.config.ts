import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8090',
        changeOrigin: true,
        ws: true, // GET /api/ws (CLAUDE.md §11.4/§12) needs the proxy to upgrade, not treat it as plain HTTP
      },
    },
  },
})
