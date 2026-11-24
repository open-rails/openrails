import path from "node:path"
import { defineConfig } from "vite"

export default defineConfig({
  build: {
    target: "esnext",
    outDir: "test-results/critical-app",
    emptyOutDir: true,
    lib: {
      entry: path.resolve(import.meta.dirname, "app.ts"),
      formats: ["es"],
      fileName: () => "app.js",
    },
  },
})
