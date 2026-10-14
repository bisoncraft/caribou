import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tsconfigPaths from 'vite-tsconfig-paths'

export default defineConfig({
  plugins: [react(), tsconfigPaths()],
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./vitest.setup.ts'],
    include: ['shared/src/**/*.test.ts', 'desktop/src/**/*.test.{ts,tsx}', 'mobile/src/**/*.test.{ts,tsx}']
  }
})
