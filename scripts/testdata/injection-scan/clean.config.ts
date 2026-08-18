// FIXTURE - an ordinary build config; must stay clean.
import { defineConfig } from 'vite';

export default defineConfig({
  build: { outDir: 'dist', sourcemap: false },
  server: { port: 3000 },
});
