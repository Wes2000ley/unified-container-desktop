import { fileURLToPath, URL } from 'node:url';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: process.env.UCD_TEST_BRIDGE === '1' ? '.test-dist' : 'dist',
  },
  resolve: {
    alias:
      process.env.UCD_TEST_BRIDGE === '1'
        ? [
            {
              find: '@wailsio/runtime',
              replacement: fileURLToPath(
                new URL('./tests/fixtures/fake-runtime.ts', import.meta.url),
              ),
            },
          ]
        : [],
  },
  server: { strictPort: true },
});
