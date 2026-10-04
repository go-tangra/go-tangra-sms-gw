import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'
import { breakpointSpecificity } from '@go-tangra/ui/vite'
import { federation } from '@module-federation/vite'
import { remoteConfig } from './module-federation.config'

export default defineConfig({
  base: '/m/sms-gw/',
  plugins: [vue(), tailwindcss(), breakpointSpecificity(), federation(remoteConfig)],
  build: { outDir: 'dist', emptyOutDir: true, target: 'esnext', sourcemap: false },
  test: { environment: 'node', include: ['src/tests/**/*.spec.ts'] },
})
