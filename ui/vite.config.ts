import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'
import { breakpointSpecificity } from '@go-tangra/ui/vite'
import { federation } from '@module-federation/vite'
import { remoteConfig } from './module-federation.config'

// The SMS gateway UI is a federated remote served by the module under /ui/ and
// relayed by the gateway at /m/sms-gw/; `vite` alone runs a standalone dev
// shell (src/main.ts) against the gateway.
export default defineConfig({
  base: '/m/sms-gw/',
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  plugins: [vue(), tailwindcss(), breakpointSpecificity(), federation(remoteConfig)],
  server: { proxy: { '/api': { target: 'https://localhost:8443', secure: false, changeOrigin: false } } },
  build: { outDir: 'dist', emptyOutDir: true, sourcemap: false, target: 'esnext' },
  test: {
    environment: 'jsdom',
    environmentOptions: { jsdom: { url: 'https://localhost/sms-gw' } },
    include: ['src/tests/**/*.spec.ts'],
    setupFiles: ['src/tests/setup.ts'],
    server: { deps: { inline: ['@go-tangra/ui'] } },
  },
})
