import path from "node:path"
import { defineConfig } from "vite"

export default defineConfig({
  define: { "process.env.NODE_ENV": JSON.stringify("production") },
  build: {
    target: "esnext",
    outDir: "test-results/admin-app",
    emptyOutDir: true,
    lib: {
      entry: path.resolve(import.meta.dirname, "app.tsx"),
      formats: ["es"],
      fileName: () => "app.js",
    },
  },
})
